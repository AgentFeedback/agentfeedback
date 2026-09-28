// Package core holds the service's business logic: validation, content hashing
// and duplicate handling, and the read paths. It owns no HTTP and no SQL text.
package core

import (
	"errors"
	"time"

	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// Sentinel errors. The API layer maps these to status codes.
var (
	ErrSubmissionNotFound = errors.New("submission not found")
	ErrInvalidInput       = errors.New("invalid input")
	// ErrReplayMismatch: a review or event replay carried different content
	// than the stored row under the same idempotency key. Mapped to 409 —
	// corrections are new submissions, never silent discards.
	ErrReplayMismatch = errors.New("replay mismatch")
)

// Service executes the business logic against the store.
type Service struct {
	db *store.DB
}

// New wires a Service to an opened database.
func New(db *store.DB) *Service {
	return &Service{db: db}
}

// DB exposes the underlying database for readiness probes and maintenance
// commands.
func (s *Service) DB() *store.DB { return s.db }

// nowMicros is the clock used for created_at and processed_at. Package
// variable so tests can pin it.
var nowMicros = func() int64 { return time.Now().UTC().UnixMicro() }
