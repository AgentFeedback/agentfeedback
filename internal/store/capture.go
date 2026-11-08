package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// CapturePolicy is one server/organisation policy observation. Enabled is the
// last successfully observed consent, not permission to upload: State must also
// be enabled. Nullable timestamps are unix microseconds UTC.
type CapturePolicy struct {
	Server                string
	OrgID                 string
	OrgName               string
	Enabled               bool
	NoticeVersion         string
	LastNoticeVersion     string
	Retention             json.RawMessage
	Bounds                json.RawMessage
	Since                 *int64
	CheckedAt             int64
	RetryAt               *int64
	State                 string
	CredentialFingerprint string
	Generation            int64
}

const capturePolicyColumns = `server, org_id, org_name, enabled, notice_version,
	last_notice_version, retention_json, bounds_json, since, checked_at, retry_at, state,
	credential_fingerprint, generation`

func scanCapturePolicy(row scanner) (CapturePolicy, bool, error) {
	var p CapturePolicy
	var retention, bounds string
	err := row.Scan(&p.Server, &p.OrgID, &p.OrgName, &p.Enabled, &p.NoticeVersion,
		&p.LastNoticeVersion, &retention, &bounds, &p.Since, &p.CheckedAt, &p.RetryAt, &p.State,
		&p.CredentialFingerprint, &p.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return CapturePolicy{}, false, nil
	}
	if err != nil {
		return CapturePolicy{}, false, fmt.Errorf("read capture policy: %w", err)
	}
	p.Retention, p.Bounds = json.RawMessage(retention), json.RawMessage(bounds)

	return p, true, nil
}

// GetCapturePolicy reads the independent policy history of server and orgID.
func GetCapturePolicy(ctx context.Context, q Querier, server, orgID string) (CapturePolicy, bool, error) {
	return scanCapturePolicy(q.QueryRowContext(ctx, `SELECT `+capturePolicyColumns+`
		FROM capture_policy WHERE server = ? AND org_id = ?`, server, orgID))
}

// CurrentCapturePolicy reads the most recently selected organisation of server.
func CurrentCapturePolicy(ctx context.Context, q Querier, server string) (CapturePolicy, bool, error) {
	return scanCapturePolicy(q.QueryRowContext(ctx, `SELECT `+capturePolicyColumns+`
		FROM capture_policy WHERE server = ? AND is_current = 1`, server))
}

// NextCaptureGeneration allocates a server's next request before discovery.
// Call within DB.Write; do not keep that transaction open over the network.
func NextCaptureGeneration(ctx context.Context, q Querier, server string) (int64, error) {
	var generation int64
	err := q.QueryRowContext(ctx, `INSERT INTO capture_refresh (server, generation) VALUES (?, 1)
		ON CONFLICT (server) DO UPDATE SET generation = generation + 1
		RETURNING generation`, server).Scan(&generation)
	if err != nil {
		return 0, fmt.Errorf("allocate capture request generation: %w", err)
	}
	return generation, nil
}

// CaptureGenerationCurrent checks ordering within the same transaction used
// for cache selection or committing a discovered observation.
func CaptureGenerationCurrent(ctx context.Context, q Querier, server string, generation int64) (bool, error) {
	var current int64
	err := q.QueryRowContext(ctx, `SELECT generation FROM capture_refresh WHERE server = ?`, server).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read capture request generation: %w", err)
	}
	return generation == current, nil
}

// SetCapturePolicy selects and stores p. Call within DB.Write so clearing the
// previous selection and writing its replacement are atomic. Consent epochs are
// supplied by the caller; no other organisation's history is changed.
func SetCapturePolicy(ctx context.Context, q Querier, p CapturePolicy) error {
	if _, err := q.ExecContext(ctx, `UPDATE capture_policy SET is_current = 0 WHERE server = ? AND is_current = 1`, p.Server); err != nil {
		return fmt.Errorf("clear current capture policy: %w", err)
	}
	retention, bounds := p.Retention, p.Bounds
	if retention == nil {
		retention = json.RawMessage(`null`)
	}
	if bounds == nil {
		bounds = json.RawMessage(`{}`)
	}
	_, err := q.ExecContext(ctx, `INSERT INTO capture_policy (`+capturePolicyColumns+`, is_current)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT (server, org_id) DO UPDATE SET
		  org_name = excluded.org_name, enabled = excluded.enabled,
		  notice_version = excluded.notice_version, last_notice_version = excluded.last_notice_version,
		  retention_json = excluded.retention_json, bounds_json = excluded.bounds_json,
		  since = excluded.since, checked_at = excluded.checked_at, retry_at = excluded.retry_at,
		  state = excluded.state, credential_fingerprint = excluded.credential_fingerprint,
		  generation = excluded.generation, is_current = 1`,
		p.Server, p.OrgID, p.OrgName, p.Enabled, p.NoticeVersion, p.LastNoticeVersion,
		string(retention), string(bounds), p.Since, p.CheckedAt, p.RetryAt, p.State,
		p.CredentialFingerprint, p.Generation)
	if err != nil {
		return fmt.Errorf("store capture policy: %w", err)
	}

	return nil
}

// MarkCaptureNotice records only the version that was actually printed. A
// concurrent refresh to a different notice cannot be marked by a stale caller.
func MarkCaptureNotice(ctx context.Context, q Querier, server, orgID, version string) error {
	_, err := q.ExecContext(ctx, `UPDATE capture_policy SET last_notice_version = ?
		WHERE server = ? AND org_id = ? AND notice_version = ?`, version, server, orgID, version)
	if err != nil {
		return fmt.Errorf("mark capture notice: %w", err)
	}

	return nil
}
