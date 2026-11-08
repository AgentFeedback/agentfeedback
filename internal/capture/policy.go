// Package capture discovers the configured organisation's session policy. It
// neither reads transcripts nor uploads them.
package capture

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

const (
	Enabled       = "enabled"
	Disabled      = "disabled"
	Unsupported   = "unsupported"
	Unauthorized  = "unauthorized"
	Suspended     = "suspended"
	NotApplicable = "not_applicable"
	Error         = "error"

	policyPath     = "/api/v1/sessions/policy"
	discoveryPath  = "/api/v1/openapi.json"
	defaultQuiet   = 5 * time.Minute
	defaultTTL     = 5 * time.Minute
	unsupportedTTL = 24 * time.Hour
)

// Config contains the parsed client settings shared with the CLI. The config
// reader supplies defaults and validates these values.
type Config struct {
	Quiet      time.Duration
	PolicyTTL  time.Duration
	PushBatch  int
	PushBudget time.Duration
}

// Options selects a destination and the policy cache lifetimes. Local bypasses
// the database and client entirely. Clock defaults to time.Now, or a fixed Now
// when supplied. Clock is sampled again when each HTTP response is received.
type Options struct {
	Server    string
	Local     bool
	Quiet     time.Duration
	PolicyTTL time.Duration
	Now       time.Time
	Clock     func() time.Time
}

// Bounds are the supported hosted policy's upload limits, not client overrides.
type Bounds struct {
	MaxPartBytes               int `json:"max_part_bytes"`
	MaxSessionBytes            int `json:"max_session_bytes"`
	IncompleteUploadTTLSeconds int `json:"incomplete_upload_ttl_seconds"`
}

// Retention is optional until the server exposes the organisation's setting.
type Retention struct {
	Mode string `json:"mode"`
	Days int    `json:"days,omitempty"`
}

// Policy is the current effective permission. Enabled is true only for an
// enabled state; transient failures never turn an earlier consent into current
// permission. Since identifies the independent server/organisation consent epoch.
type Policy struct {
	State             string     `json:"state"`
	Server            string     `json:"server,omitempty"`
	OrgID             string     `json:"org_id"`
	OrgName           string     `json:"org_name,omitempty"`
	Enabled           bool       `json:"enabled"`
	NoticeVersion     string     `json:"notice_version"`
	Retention         *Retention `json:"retention"`
	Since             time.Time  `json:"since"`
	Quiet             string     `json:"quiet"`
	CheckedAt         time.Time  `json:"checked_at"`
	RetryAt           time.Time  `json:"retry_at,omitempty"`
	LastNoticeVersion string     `json:"-"`
	Bounds
}

// Refresh returns a cached current policy or discovers it using the configured
// authenticated client. Unsupported servers are rechecked no more than daily;
// unauthorized answers expire at PolicyTTL and 429 answers at RetryAt. Network,
// unexpected HTTP, and malformed JSON/schema failures return a fail-closed error
// policy and a credential-free error, never the earlier enabled observation.
func Refresh(ctx context.Context, db *store.DB, c *client.Client, opts Options) (Policy, error) {
	if opts.Clock == nil {
		opts.Clock = time.Now
		if !opts.Now.IsZero() {
			fixed := opts.Now
			opts.Clock = func() time.Time { return fixed }
		}
	}
	if opts.Now.IsZero() {
		opts.Now = opts.Clock()
	}
	opts.Now = opts.Now.UTC()
	if opts.Quiet <= 0 {
		opts.Quiet = defaultQuiet
	}
	if opts.PolicyTTL <= 0 {
		opts.PolicyTTL = defaultTTL
	}
	p := Policy{
		State: NotApplicable, Quiet: quietString(opts.Quiet), CheckedAt: opts.Now,
	}
	if opts.Local {
		return p, nil
	}
	p.State = Error
	p.Server = strings.TrimRight(strings.TrimSpace(opts.Server), "/")
	u, err := url.Parse(p.Server)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		p.Server = "[REDACTED]"

		return p, errors.New("sessions: invalid server URL; use a base URL without credentials, query or fragment")
	}
	u.Host = strings.ToLower(u.Host)
	p.Server = u.String()
	if db == nil || c == nil {
		return p, errors.New("sessions: remote policy requires a database and configured client")
	}
	fingerprint := c.CredentialFingerprint()
	var previous store.CapturePolicy
	var cached bool
	var generation int64
	if err := db.Write(ctx, func(q store.Querier) error {
		var found bool
		var err error
		previous, found, err = store.CurrentCapturePolicy(ctx, q, p.Server)
		if err != nil {
			return err
		}
		if found && previous.CredentialFingerprint == fingerprint && cacheCurrent(previous, opts) {
			cached, err = store.CaptureGenerationCurrent(ctx, q, p.Server, previous.Generation)
			if err != nil || cached {
				return err
			}
		}
		generation, err = store.NextCaptureGeneration(ctx, q, p.Server)
		return err
	}); err != nil {
		return p, err
	}
	if cached {
		return fromStored(previous, p.Quiet)
	}

	observed, refreshErr := discover(ctx, c, p, opts.Clock)
	if err := db.Write(ctx, func(q store.Querier) error {
		current, err := store.CaptureGenerationCurrent(ctx, q, p.Server, generation)
		if err != nil {
			return err
		}
		if !current {
			return errors.New("sessions: policy refresh superseded by a newer request; retry")
		}
		var row store.CapturePolicy
		if observed.State == Enabled || observed.State == Disabled {
			old, ok, err := store.GetCapturePolicy(ctx, q, observed.Server, observed.OrgID)
			if err != nil {
				return err
			}
			row = old
			row.Server, row.OrgID, row.OrgName = observed.Server, observed.OrgID, observed.OrgName
			if observed.Enabled && (!ok || !old.Enabled || old.Since == nil) {
				since := opts.Now.UnixMicro()
				row.Since = &since
			}
			row.Enabled, row.NoticeVersion = observed.Enabled, observed.NoticeVersion
			row.Retention, err = json.Marshal(observed.Retention)
			if err != nil {
				return err
			}
			row.Bounds, err = json.Marshal(observed.Bounds)
			if err != nil {
				return err
			}
		} else {
			// No authenticated identity was learned. Keep the latest org's
			// consent epoch, even if another caller refreshed it meanwhile.
			var err error
			row, _, err = store.CurrentCapturePolicy(ctx, q, observed.Server)
			if err != nil {
				return err
			}
			row.Server = observed.Server
		}
		row.CheckedAt, row.State, row.RetryAt = opts.Now.UnixMicro(), observed.State, nil
		row.CredentialFingerprint, row.Generation = fingerprint, generation
		if !observed.RetryAt.IsZero() {
			retry := observed.RetryAt.UnixMicro()
			row.RetryAt = &retry
		}
		if err := store.SetCapturePolicy(ctx, q, row); err != nil {
			return err
		}
		observed, err = fromStored(row, p.Quiet)

		return err
	}); err != nil {
		p.State, p.Enabled = Error, false

		return p, errors.Join(refreshErr, err)
	}

	return observed, refreshErr
}

func cacheCurrent(p store.CapturePolicy, opts Options) bool {
	checked := time.UnixMicro(p.CheckedAt)
	if opts.Now.Before(checked) {
		return false
	}
	if p.State == Suspended {
		return p.RetryAt != nil && opts.Now.Before(time.UnixMicro(*p.RetryAt))
	}
	ttl := opts.PolicyTTL
	switch p.State {
	case Unsupported:
		if ttl < unsupportedTTL {
			ttl = unsupportedTTL
		}
	case Enabled, Disabled, Unauthorized:
	default:
		return false
	}

	return opts.Now.Before(checked.Add(ttl))
}

func fromStored(row store.CapturePolicy, quiet string) (Policy, error) {
	p := Policy{
		State: row.State, Server: row.Server, OrgID: row.OrgID, OrgName: row.OrgName,
		Enabled: row.Enabled && row.State == Enabled, NoticeVersion: row.NoticeVersion,
		LastNoticeVersion: row.LastNoticeVersion, Quiet: quiet, CheckedAt: time.UnixMicro(row.CheckedAt).UTC(),
	}
	if row.Since != nil {
		p.Since = time.UnixMicro(*row.Since).UTC()
	}
	if row.RetryAt != nil {
		p.RetryAt = time.UnixMicro(*row.RetryAt).UTC()
	}
	if len(row.Retention) != 0 {
		if err := json.Unmarshal(row.Retention, &p.Retention); err != nil {
			p.State, p.Enabled = Error, false

			return p, errors.New("sessions: invalid cached retention policy")
		}
	}
	if len(row.Bounds) != 0 {
		if err := json.Unmarshal(row.Bounds, &p.Bounds); err != nil {
			p.State, p.Enabled = Error, false

			return p, errors.New("sessions: invalid cached policy bounds")
		}
	}
	if p.State == Enabled && (!p.Enabled || p.Since.IsZero() || !validPolicy(p)) {
		p.State, p.Enabled = Error, false

		return p, errors.New("sessions: invalid cached enabled policy")
	}

	return p, nil
}

func validPolicy(p Policy) bool {
	id, err := uuid.Parse(p.OrgID)
	return err == nil && len(p.OrgID) == 36 && strings.EqualFold(id.String(), p.OrgID) &&
		strings.TrimSpace(p.NoticeVersion) != "" && p.MaxPartBytes == 10485760 &&
		p.MaxSessionBytes == 25165824 && p.IncompleteUploadTTLSeconds == 86400
}

// MarkNotice persists the notice version only after its status was printed.
func MarkNotice(ctx context.Context, db *store.DB, p Policy) error {
	if p.OrgID == "" || p.NoticeVersion == "" || p.State == NotApplicable {
		return nil
	}
	if db == nil {
		return errors.New("sessions: marking a policy notice requires a database")
	}

	return db.Write(ctx, func(q store.Querier) error {
		return store.MarkCaptureNotice(ctx, q, p.Server, p.OrgID, p.NoticeVersion)
	})
}
