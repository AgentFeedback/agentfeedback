// Package core is the transport-neutral v1 service: create with content
// identity, get, list, processing marks, redaction, stats, export and import
// in format 2, and the service metadata. HTTP and MCP adapt it; neither owns
// a rule.
//
// Boundary: every JSON request body (create, mark, batch mark, import)
// arrives as bytes and core decodes it; query-style parameters arrive typed
// (ints, bools, times already parsed by the transport) and core validates
// their ranges and exclusivity and normalises tokens with pkg/schema.
//
// Errors a client caused are *Problem values carrying the contract's status
// and error code; any other error is internal. The package never imports
// net/http, so a transport maps Problem.Status itself.
package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/agentfeedback/agentfeedback/internal/store"
	"github.com/agentfeedback/agentfeedback/pkg/envelope"
)

// DedupeWindow is how far back a keyless create looks for an unprocessed
// row with the same content hash.
const DedupeWindow = 24 * time.Hour

// Limits core enforces itself; the body, context and member limits live in
// pkg/envelope and the compiled envelope schema.
const (
	ImportLimit = 33554432 // largest import body (32 MiB); one byte more is 413
	// ImportDepth is the nesting a re-decoded stored envelope may reach:
	// what create accepts plus what inference can add.
	ImportDepth        = envelope.MaxDepth + envelope.StoredDepthHeadroom
	ListMax            = 500 // list limit without payloads
	ListMaxWithPayload = 100 // list limit with include=payload
	ListDefault        = 50
	ProcessedIDsMax    = 500 // ids in one batch mark
	ExportMax          = 500 // export limit when one is given
	QMaxBytes          = 200
	StatsTopMax        = 50
	StatsTopDefault    = 10
	VerdictBytes       = 64
	ResolutionBytes    = 2000
	RefBytes           = 200
	ProcessedByBytes   = 200
)

// Versions the service reports.
const (
	APIVersion   = "1.0"
	ExportFormat = 2
)

// Error codes of the contract's Error body that core produces.
const (
	CodeBadRequest      = "bad_request"
	CodeValidation      = "validation_error"
	CodeNotFound        = "not_found"
	CodeReplayMismatch  = "replay_mismatch"
	CodeRequestTooLarge = "request_too_large"
	CodeUnavailable     = "unavailable"
)

// Features is the full feature list the contract names. A server passes the
// subset it actually wires in Config.Features.
var Features = []string{"q", "stats", "export.after_id", "export.limit", "import", "mcp", "redaction"}

// Config is what the server tells the core about itself: the service version
// (service_version and both client versions in Meta) and the features it
// wires.
type Config struct {
	Version  string
	Features []string
}

// Detail is one item of an error's details: a code, an RFC 6901 pointer into
// the body (a query parameter is "?name") and a message. ExistingID is set
// only on the key_reused detail of a replay mismatch.
type Detail struct {
	Code       string `json:"code"`
	Pointer    string `json:"pointer"`
	Message    string `json:"message"`
	ExistingID int64  `json:"existing_id,omitempty"`
}

// Problem is an error the client caused: the HTTP status, the contract's
// error code, a message naming the field or parameter, the value when short
// and the accepted range or shape, and optional details.
type Problem struct {
	Status  int
	Code    string
	Message string
	Details []Detail
}

func (p *Problem) Error() string { return p.Code + ": " + p.Message }

// invalid builds a 400 validation_error whose one detail repeats the message.
func invalid(code, pointer, format string, args ...any) *Problem {
	msg := fmt.Sprintf(format, args...)
	return &Problem{Status: 400, Code: CodeValidation, Message: msg,
		Details: []Detail{{Code: code, Pointer: pointer, Message: msg}}}
}

func notFound(id int64) *Problem {
	return &Problem{Status: 404, Code: CodeNotFound, Message: fmt.Sprintf("submission %d not found", id)}
}

// Service is the v1 core over one database.
type Service struct {
	db     *store.DB
	now    func() time.Time
	newUID func() (string, error)
	config Config
}

// Option configures a Service.
type Option func(*Service)

// WithClock replaces the wall clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithUIDGenerator replaces the UUIDv7 generator (tests). The function must
// return a lower-case 8-4-4-4-12 UUID; an error fails the create as an
// internal error.
func WithUIDGenerator(newUID func() (string, error)) Option {
	return func(s *Service) { s.newUID = newUID }
}

// New returns the service over db.
func New(db *store.DB, config Config, opts ...Option) *Service {
	s := &Service{db: db, now: time.Now, newUID: newUUIDv7, config: config}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func newUUIDv7() (string, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate uid: %w", err)
	}
	return strings.ToLower(u.String()), nil
}

// storeErr maps a store error for the caller: a Problem passes through, a
// saturated writer (SQLITE_BUSY after the busy timeout) is 503 unavailable,
// anything else is returned as is for the transport to report as internal.
func storeErr(err error) error {
	if err == nil {
		return nil
	}
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	if store.IsBusy(err) {
		return &Problem{Status: 503, Code: CodeUnavailable, Message: "the database is busy; retry shortly"}
	}
	return err
}

// write and read run fn in one transaction and map the error with storeErr.
func (s *Service) write(ctx context.Context, fn func(store.Querier) error) error {
	return storeErr(s.db.Write(ctx, fn))
}

func (s *Service) read(ctx context.Context, fn func(store.Querier) error) error {
	return storeErr(s.db.Read(ctx, fn))
}

// checkID rejects a non-positive id.
func checkID(id int64) error {
	if id <= 0 {
		return invalid("out_of_range", "/id", "id must be a positive integer, got %d", id)
	}
	return nil
}

// nowMicros is the clock in unix microseconds UTC.
func (s *Service) nowMicros() int64 { return s.now().UnixMicro() }
