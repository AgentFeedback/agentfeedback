package capture

import (
	"context"
	"errors"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

// PushStatus describes currently eligible source revisions, not digest state.
// Skipped counts only matching too_large suppression, never policy refusals.
type PushStatus struct {
	Pushed   int        `json:"pushed"`
	Pending  int        `json:"pending"`
	Skipped  int        `json:"skipped"`
	LastPush *time.Time `json:"last_push"`
}

func scopedPushRows(ctx context.Context, db *store.DB, p Policy) ([]store.SessionPush, bool, error) {
	if db == nil || !usablePolicy(p) {
		return nil, false, nil
	}
	var rows []store.SessionPush
	matched := false
	err := db.Read(ctx, func(q store.Querier) error {
		// Even display cannot cross a credential/org switch that happened
		// after its caller selected policy.
		current, found, err := store.CurrentCapturePolicy(ctx, q, p.Server)
		if err != nil || !found {
			return err
		}
		if !current.Enabled || current.State != Enabled || current.Generation != p.generation || current.OrgID != p.OrgID || current.CredentialFingerprint != p.credentialFingerprint || current.Since == nil || *current.Since != p.Since.UnixMicro() {
			return nil
		}
		matched, err = store.CaptureGenerationCurrent(ctx, q, p.Server, p.generation)
		if err != nil || !matched {
			return err
		}
		rows, err = store.ListSessionPushes(ctx, q, policyScope(p))
		return err
	})
	return rows, matched, err
}

// PushedRevisions returns each session's latest successful source revision in
// exactly p's destination/installation/consent scope. It never opens a source.
func PushedRevisions(ctx context.Context, db *store.DB, p Policy) (map[string]string, error) {
	out := make(map[string]string)
	rows, _, err := scopedPushRows(ctx, db, p)
	if err != nil {
		return nil, errors.New("sessions: push state unavailable")
	}
	for _, r := range rows {
		if r.Status == "pushed" || r.Status == "replayed" {
			out[sessions.Ref(r.Harness, r.SessionID)] = r.Revision
		}
	}
	return out, nil
}

// PushSummary evaluates current source metadata under the same digest gates;
// sources refused by filesystem policy are never read. Non-enabled policies
// have no current upload eligibility and return zero counts.
func PushSummary(ctx context.Context, db *store.DB, env sessions.Env, p Policy) (PushStatus, error) {
	var out PushStatus
	if !usablePolicy(p) {
		return out, nil
	}
	rows, matched, err := scopedPushRows(ctx, db, p)
	if err != nil {
		return out, errors.New("sessions: push state unavailable")
	}
	if !matched {
		return out, nil
	}
	for _, r := range rows {
		if (r.Status == "pushed" || r.Status == "replayed") && r.PushedAt != nil {
			at := time.UnixMicro(*r.PushedAt).UTC()
			if out.LastPush == nil || at.After(*out.LastPush) {
				out.LastPush = &at
			}
		}
	}
	candidates, err := sessions.CaptureCandidates(ctx, db, env, nil)
	if err != nil {
		return out, errors.New("sessions: capture source enumeration failed")
	}
	now := time.Now()
	if env.Now != nil {
		now = env.Now()
	}
	quiet, err := time.ParseDuration(p.Quiet)
	if err != nil || quiet <= 0 {
		quiet = defaultQuiet
	}
	for _, c := range candidates {
		if refused(c) {
			continue
		}
		status, reason := suppressed(c, rows, p.MaxSessionBytes)
		switch {
		case status == "replayed":
			out.Pushed++
		case reason == "too_large":
			out.Skipped++
		case reason == "replay_mismatch":
		case !c.End.Before(p.Since) && now.Sub(c.Mtime) >= quiet:
			out.Pending++
		}
	}
	return out, nil
}
