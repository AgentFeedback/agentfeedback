package capture

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

const captureTimeLayout = time.RFC3339Nano

// PushOptions controls one manual batch. No transcript is persisted or spooled.
type PushOptions struct {
	Policy        Options
	Config        Config
	Refs          []string
	Since         time.Time
	Limit         int
	DryRun        bool
	ClientVersion string
	WithCwd       bool
	Drop          []string
	Log           func(string)
}

// PushResult exposes only scrubbed metadata and fixed outcome reasons. Preview
// is an in-memory, scrubbed UTF-8 prefix returned only for successful dry runs.
type PushResult struct {
	Ref           string `json:"ref"`
	Revision      string `json:"revision,omitempty"`
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	Bytes         int64  `json:"bytes,omitempty"`
	Parts         int    `json:"part_count,omitempty"`
	Preview       string `json:"preview,omitempty"`
	SubmissionUID string `json:"submission_uid,omitempty"`
}

func formatRevision(size int64) string { return strconv.FormatInt(size, 10) }
func policyScope(p Policy) store.PushScope {
	return store.PushScope{Server: p.Server, OrgID: p.OrgID, CredentialFingerprint: p.credentialFingerprint, ConsentSince: p.Since.UnixMicro()}
}

func usablePolicy(p Policy) bool {
	return p.State == Enabled && p.Enabled && p.Server != "" && p.OrgID != "" && p.credentialFingerprint != "" && !p.Since.IsZero()
}

func refused(c sessions.CaptureCandidate) bool {
	switch c.State {
	case sessions.StateNew, sessions.StateChanged, sessions.StateProcessed:
		return false
	default:
		return true
	}
}

func outcome(c sessions.CaptureCandidate, status, reason string) PushResult {
	return PushResult{Ref: terminalText(c.Ref), Revision: formatRevision(c.Size), Status: status, Reason: reason}
}

func suppressed(c sessions.CaptureCandidate, rows []store.SessionPush, cap int) (string, string) {
	// Replay mismatch suppression follows source size even for same-size
	// rewrites. Rotation/head changes only reopen too_large/success outcomes.
	for _, r := range rows {
		if r.Harness == c.Harness && r.SessionID == c.SessionID && r.Status == "replay_mismatch" && r.Revision == formatRevision(c.Size) {
			return "error", "replay_mismatch"
		}
	}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Harness != c.Harness || r.SessionID != c.SessionID {
			continue
		}
		if r.Status == "too_large" && c.Head == r.Head && c.Size >= r.SourceSize && int64(cap) <= r.MaxSessionBytes {
			return "skipped", "too_large"
		}
		if (r.Status == "pushed" || r.Status == "replayed") && r.Revision == formatRevision(c.Size) && r.Head == c.Head {
			return "replayed", "already_pushed"
		}
		// Only the latest outcome of a session controls size/rotation suppression.
		return "", ""
	}
	return "", ""
}

type pushRun struct {
	ctx         context.Context
	db          *store.DB
	client      *client.Client
	policy      Policy
	opts        PushOptions
	clock       func() time.Time
	deadline    time.Time
	checkSource func() error
}

func (r *pushRun) stopped() bool { return r.ctx.Err() != nil || !r.clock().Before(r.deadline) }

// authorized checks the same generation immediately before every network stage.
// It never upgrades a cached observation to new consent.
func (r *pushRun) authorized() bool {
	ok := false
	err := r.db.Read(r.ctx, func(q store.Querier) error {
		p, found, err := store.CurrentCapturePolicy(r.ctx, q, r.policy.Server)
		if err != nil || !found {
			return err
		}
		if !p.Enabled || p.State != Enabled || p.OrgID != r.policy.OrgID || p.Since == nil || *p.Since != r.policy.Since.UnixMicro() || p.CredentialFingerprint != r.policy.credentialFingerprint || p.Generation != r.policy.generation {
			return nil
		}
		ok, err = store.CaptureGenerationCurrent(r.ctx, q, p.Server, p.Generation)
		return err
	})
	return err == nil && ok
}

func (r *pushRun) record(c sessions.CaptureCandidate, result PushResult, hash string) error {
	row := store.SessionPush{
		PushScope: policyScope(r.policy), Harness: c.Harness, SessionID: c.SessionID,
		Revision: formatRevision(c.Size), Head: c.Head, SourceSize: c.Size, ContentSHA256: hash,
		EnvelopeBytes: result.Bytes, MaxSessionBytes: int64(r.policy.MaxSessionBytes), ObservedAt: r.clock().UnixMicro(),
		SubmissionUID: result.SubmissionUID, Status: result.Status, Reason: result.Reason,
	}
	if result.Reason == "too_large" || result.Reason == "replay_mismatch" {
		row.Status = result.Reason
	}
	if result.Status == "pushed" || result.Status == "replayed" {
		at := row.ObservedAt
		row.PushedAt = &at
	}
	return r.db.Write(r.ctx, func(q store.Querier) error {
		if err := store.SetSessionPush(r.ctx, q, row); err != nil {
			return err
		}
		return store.DeleteSessionManifest(r.ctx, q, r.manifestKey(c))
	})
}

// Push refreshes effective consent before enumerating or opening any session.
// Named refs bypass quiet time, never filesystem policy or consent. Since is an
// explicit replacement for the consent cutoff, not an automatic widening.
func Push(ctx context.Context, db *store.DB, c *client.Client, env sessions.Env, opts PushOptions) ([]PushResult, error) {
	clock := opts.Policy.Clock
	if clock == nil {
		clock = time.Now
		if !opts.Policy.Now.IsZero() {
			fixed := opts.Policy.Now
			clock = func() time.Time { return fixed }
		}
	}
	budget := opts.Config.PushBudget
	if budget <= 0 {
		budget = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	deadline := clock().Add(budget)
	if opts.Policy.Quiet <= 0 {
		opts.Policy.Quiet = opts.Config.Quiet
	}
	if opts.Policy.PolicyTTL <= 0 {
		opts.Policy.PolicyTTL = opts.Config.PolicyTTL
	}
	opts.Policy.forceRefresh = true
	p, err := Refresh(ctx, db, c, opts.Policy)
	if err != nil {
		return nil, errors.New("sessions: policy refresh failed")
	}
	out := make([]PushResult, 0)
	if !opts.DryRun {
		scope := policyScope(p)
		if p.State == Disabled || p.State == Unauthorized || p.State == Unsupported {
			scope = store.PushScope{Server: opts.Policy.Server}
		}
		if err := db.Write(ctx, func(q store.Querier) error { return store.PruneSessionManifests(ctx, q, scope) }); err != nil {
			return nil, errors.New("sessions: could not invalidate pending manifests")
		}
	}
	if !usablePolicy(p) {
		for _, ref := range opts.Refs {
			status := "skipped"
			if p.State == Suspended {
				status = "suspended"
			}
			out = append(out, PushResult{Ref: terminalText(ref), Status: status, Reason: "policy_" + p.State})
		}
		if len(out) == 0 {
			out = append(out, PushResult{Status: "skipped", Reason: "policy_" + p.State})
			if p.State == Suspended {
				out[0].Status = "suspended"
			}
		}
		return out, nil
	}
	run := pushRun{ctx: ctx, db: db, client: c, policy: p, opts: opts, clock: clock, deadline: deadline}
	if run.stopped() {
		for _, ref := range opts.Refs {
			out = append(out, PushResult{Ref: terminalText(ref), Status: "deferred", Reason: "push_budget"})
		}
		return out, nil
	}
	candidates, err := sessions.CaptureCandidates(ctx, db, env, opts.Refs)
	if err != nil {
		if run.stopped() {
			if len(opts.Refs) > 0 {
				for _, ref := range opts.Refs {
					out = append(out, PushResult{Ref: terminalText(ref), Status: "deferred", Reason: "push_budget"})
				}
			} else {
				for _, candidate := range candidates {
					out = append(out, outcome(candidate, "deferred", "push_budget"))
				}
				if len(out) == 0 {
					out = append(out, PushResult{Status: "deferred", Reason: "push_budget"})
				}
			}
			return out, nil
		}
		return nil, errors.New("sessions: capture source enumeration failed")
	}
	var rows []store.SessionPush
	if err := db.Read(ctx, func(q store.Querier) error {
		var err error
		rows, err = store.ListSessionPushes(ctx, q, policyScope(p))
		return err
	}); err != nil {
		return nil, errors.New("sessions: push state unavailable")
	}
	if len(opts.Refs) == 0 {
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].End.Equal(candidates[j].End) {
				return candidates[i].Ref < candidates[j].Ref
			}
			return candidates[i].End.Before(candidates[j].End)
		})
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = opts.Config.PushBatch
	}
	if limit <= 0 {
		limit = 5
	}
	cutoff := p.Since
	if !opts.Since.IsZero() {
		cutoff = opts.Since
	}
	quiet := opts.Policy.Quiet
	if quiet <= 0 {
		quiet = defaultQuiet
	}
	selected := 0
	for index, candidate := range candidates {
		if run.stopped() {
			out = append(out, outcome(candidate, "deferred", "push_budget"))
			if len(opts.Refs) > 0 {
				for _, remaining := range candidates[index+1:] {
					out = append(out, outcome(remaining, "deferred", "push_budget"))
				}
			}
			break
		}
		if refused(candidate) {
			out = append(out, outcome(candidate, "skipped", "source_"+safeSourceState(candidate.State)))
			continue
		}
		if candidate.End.Before(cutoff) {
			out = append(out, outcome(candidate, "skipped", "before_since"))
			continue
		}
		if len(opts.Refs) == 0 && clock().Sub(candidate.Mtime) < quiet {
			out = append(out, outcome(candidate, "deferred", "not_quiet"))
			continue
		}
		if status, reason := suppressed(candidate, rows, p.MaxSessionBytes); status != "" {
			out = append(out, outcome(candidate, status, reason))
			continue
		}
		if len(opts.Refs) == 0 && selected >= limit {
			break
		}
		selected++
		result, stop, err := run.pushOne(env, candidate)
		out = append(out, result)
		if err != nil {
			return out, err
		}
		if stop {
			if len(opts.Refs) > 0 {
				for _, remaining := range candidates[index+1:] {
					out = append(out, outcome(remaining, "deferred", "batch_stopped"))
				}
			}
			break
		}
	}
	return out, nil
}

func safeSourceState(state string) string {
	switch state {
	case sessions.StateDisabled, sessions.StateDenied, sessions.StateUnknownProject, sessions.StateUnreadable, sessions.StateUnsupportedFormat, sessions.StateAbsent:
		return state
	default:
		return "unavailable"
	}
}

func (r *pushRun) pushOne(env sessions.Env, c sessions.CaptureCandidate) (PushResult, bool, error) {
	result := outcome(c, "error", "source_read_failed")
	if !r.authorized() {
		result.Status, result.Reason = "suspended", "policy_changed"
		return result, true, nil
	}
	r.checkSource = func() error { return sessions.CheckCapture(r.ctx, env, c) }
	defer func() { r.checkSource = nil }()
	raw, meta, err := sessions.OpenCapture(r.ctx, env, c)
	if err != nil {
		return captureReadFailure(result, err), false, nil
	}
	text, counts, readErr := scrubTranscript(raw, maxEnvelope)
	closeErr := raw.Close()
	if closeErr != nil {
		return captureReadFailure(result, closeErr), false, nil
	}
	if errors.Is(readErr, errTranscriptLarge) {
		result.Status, result.Reason, result.Bytes = "skipped", "too_large", maxEnvelope+1
		if r.opts.DryRun {
			return result, false, nil
		}
		if err := r.record(c, result, ""); err != nil {
			return result, true, errors.New("sessions: could not record push outcome")
		}
		return result, false, nil
	}
	if errors.Is(readErr, errTranscriptUTF8) {
		result.Reason = "invalid_utf8"
		return result, false, nil
	}
	if readErr != nil {
		return captureReadFailure(result, readErr), false, nil
	}
	if r.stopped() {
		result.Status, result.Reason = "deferred", "push_budget"
		return result, true, nil
	}
	if meta.Start.IsZero() {
		meta.Start = c.Mtime
	}
	if meta.End.IsZero() {
		meta.End = c.Mtime
	}
	if meta.End.Before(meta.Start) {
		result.Reason = "invalid_capture_range"
		return result, false, nil
	}
	envelope, metadataCounts, allowed := captureEnvelope(env, c, meta, result.Revision, r.opts)
	if !allowed {
		result.Status, result.Reason = "skipped", "source_refused"
		return result, false, nil
	}
	counts.Add(metadataCounts)
	hash := shaText(text)
	partBytes := min(r.policy.MaxPartBytes, maxRawPart)
	installation := ""
	pending, savedPartBytes, found, err := r.pending(c)
	if err != nil {
		return result, true, errors.New("sessions: pending manifest unavailable")
	}
	if found {
		if pending.ContentSHA256 != hash || pending.ContentBytes != int64(len(text)) {
			if r.opts.DryRun {
				result.Status, result.Reason = "error", "replay_mismatch"
				return result, false, nil
			}
			return r.uploadFailure(c, result, errManifestChanged, hash)
		}
		// A retry must not restore context removed by current collection rules.
		oldContext, _ := pending.Envelope["context"].(map[string]any)
		newContext, _ := envelope["context"].(map[string]any)
		for key := range oldContext {
			if _, allowed := newContext[key]; !allowed {
				result.Status, result.Reason = "skipped", "source_refused"
				return result, false, nil
			}
		}
		envelope, installation, partBytes = pending.Envelope, pending.Installation, savedPartBytes
		meta.Start, err = time.Parse(captureTimeLayout, pending.CaptureStart)
		if err == nil {
			meta.End, err = time.Parse(captureTimeLayout, pending.CaptureEnd)
		}
		if err != nil {
			return result, true, errors.New("sessions: invalid pending manifest range")
		}
		if partBytes > r.policy.MaxPartBytes {
			result.Status, result.Reason = "deferred", "part_limit_changed"
			return result, false, nil
		}
	}
	if counts.Total() > 0 && r.opts.Log != nil {
		r.opts.Log("sessions: scrubbed " + counts.String())
	}
	result.Bytes = assembledBytes(envelope, text)
	if result.Bytes > int64(min(r.policy.MaxSessionBytes, maxEnvelope)) || len(text) > maxEnvelope {
		result.Status, result.Reason = "skipped", "too_large"
		if r.opts.DryRun {
			return result, false, nil
		}
		err := r.record(c, result, "")
		if err == nil && r.opts.Log != nil {
			r.opts.Log("sessions: skipped 1 too_large")
		}
		if err != nil {
			return result, true, errors.New("sessions: could not record push outcome")
		}
		return result, false, nil
	}
	parts := splitTranscript(text, partBytes)
	if len(parts) == 0 {
		result.Status, result.Reason = "skipped", "empty_or_unpartitionable"
		return result, false, nil
	}
	result.Parts = len(parts)
	if r.stopped() {
		result.Status, result.Reason = "deferred", "push_budget"
		return result, true, nil
	}
	if r.opts.DryRun {
		result.Status, result.Reason, result.Preview = "dry-run", "", preview(text)
		return result, false, nil
	}
	if installation == "" {
		installation, err = r.installation()
		if err != nil {
			return r.uploadFailure(c, result, err, "")
		}
	}
	result, err = r.upload(c, meta, envelope, parts, installation, hash, partBytes, result)
	if err != nil {
		return r.uploadFailure(c, result, err, hash)
	}
	if err := r.record(c, result, hash); err != nil {
		return result, true, errors.New("sessions: could not record push outcome")
	}
	return result, false, nil
}

func captureReadFailure(result PushResult, err error) PushResult {
	var denied *sessions.RefusedError
	if errors.Is(err, sessions.ErrCaptureChanged) {
		result.Status, result.Reason = "deferred", "source_changed"
	} else if errors.As(err, &denied) {
		result.Status, result.Reason = "skipped", "source_refused"
	}
	return result
}

// These errors are private fixed sentinels; no HTTP body or reader bytes escape.
var (
	errBudget  = errors.New("push budget exhausted")
	errConsent = errors.New("capture consent changed")
	errReceipt = errors.New("invalid upload receipt")
)

func (r *pushRun) uploadFailure(c sessions.CaptureCandidate, result PushResult, err error, hash string) (PushResult, bool, error) {
	result.Status, result.Reason = "error", "invalid_receipt"
	stop := false
	var api *client.APIError
	switch {
	case errors.Is(err, errManifestChanged):
		result.Reason = "replay_mismatch"
	case errors.Is(err, sessions.ErrCaptureChanged):
		result.Status, result.Reason = "deferred", "source_changed"
	case errors.As(err, &api):
		switch api.Status {
		case 429:
			result.Status, result.Reason, stop = "suspended", "rate_limited", true
			now := r.clock()
			retry := retryTime(api.RetryAfter, now)
			if !retry.After(now) {
				retry = now.Add(time.Minute)
			}
			persist, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 5*time.Second)
			defer cancel()
			if err := r.db.Write(persist, func(q store.Querier) error {
				return store.SuspendCapturePolicy(persist, q, policyScope(r.policy), r.policy.generation, now.UnixMicro(), retry.UnixMicro())
			}); err != nil {
				return result, true, errors.New("sessions: could not suspend capture policy")
			}
		case 409:
			result.Reason = "replay_mismatch"
		case 413:
			result.Status, result.Reason = "skipped", "too_large"
		case 401, 403:
			result.Reason, stop = "unauthorized", true
		default:
			if api.Status >= 500 {
				result.Status, result.Reason = "deferred", "server_unavailable"
			} else {
				result.Reason = fmt.Sprintf("http_%d", api.Status)
			}
		}
	case errors.Is(err, errBudget), r.stopped():
		result.Status, result.Reason, stop = "deferred", "push_budget", true
	case errors.Is(err, errConsent):
		result.Status, result.Reason, stop = "suspended", "policy_changed", true
	case errors.As(err, new(*sessions.RefusedError)):
		result.Status, result.Reason = "skipped", "source_refused"
	default:
		if !errors.Is(err, errReceipt) {
			result.Status, result.Reason = "deferred", "network_error"
		}
	}
	if result.Reason == "too_large" || result.Reason == "replay_mismatch" {
		if err := r.record(c, result, hash); err != nil {
			return result, true, errors.New("sessions: could not record push outcome")
		}
	}
	return result, stop, nil
}
