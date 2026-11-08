package store

import (
	"context"
	"fmt"
)

// PushScope binds watermarks to a destination, authenticated installation and
// consent epoch. A successful upload in one scope never authorizes another.
type PushScope struct {
	Server                string
	OrgID                 string
	CredentialFingerprint string
	ConsentSince          int64
}

// SessionPush stores only identities, hashes, byte counts and outcomes.
type SessionPush struct {
	PushScope
	Harness         string
	SessionID       string
	Revision        string
	Head            string
	SourceSize      int64
	ContentSHA256   string
	EnvelopeBytes   int64
	MaxSessionBytes int64
	PushedAt        *int64
	ObservedAt      int64
	SubmissionUID   string
	Status          string
	Reason          string
}

// ListSessionPushes returns outcome history within exactly one scope.
func ListSessionPushes(ctx context.Context, q Querier, scope PushScope) ([]SessionPush, error) {
	rows, err := q.QueryContext(ctx, `SELECT harness, session_id, revision, head, source_size,
		content_sha256, envelope_bytes, max_session_bytes, pushed_at, observed_at, submission_uid, status, reason
		FROM sessions_pushed WHERE server = ? AND org_id = ? AND credential_fingerprint = ? AND consent_since = ?
		ORDER BY observed_at, rowid`, scope.Server, scope.OrgID, scope.CredentialFingerprint, scope.ConsentSince)
	if err != nil {
		return nil, fmt.Errorf("read session push state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionPush
	for rows.Next() {
		r := SessionPush{PushScope: scope}
		if err := rows.Scan(&r.Harness, &r.SessionID, &r.Revision, &r.Head, &r.SourceSize,
			&r.ContentSHA256, &r.EnvelopeBytes, &r.MaxSessionBytes, &r.PushedAt, &r.ObservedAt,
			&r.SubmissionUID, &r.Status, &r.Reason); err != nil {
			return nil, fmt.Errorf("read session push row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetSessionPush records an upload outcome; callers never supply content.
func SetSessionPush(ctx context.Context, q Querier, r SessionPush) error {
	_, err := q.ExecContext(ctx, `INSERT INTO sessions_pushed
		(server, org_id, credential_fingerprint, consent_since, harness, session_id, revision, head, source_size,
		 content_sha256, envelope_bytes, max_session_bytes, pushed_at, observed_at, submission_uid, status, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (server, org_id, credential_fingerprint, consent_since, harness, session_id, revision)
		DO UPDATE SET head = excluded.head, source_size = excluded.source_size,
		content_sha256 = excluded.content_sha256, envelope_bytes = excluded.envelope_bytes,
		max_session_bytes = excluded.max_session_bytes, pushed_at = excluded.pushed_at,
		observed_at = excluded.observed_at, submission_uid = excluded.submission_uid,
		status = excluded.status, reason = excluded.reason`,
		r.Server, r.OrgID, r.CredentialFingerprint, r.ConsentSince, r.Harness, r.SessionID, r.Revision,
		r.Head, r.SourceSize, r.ContentSHA256, r.EnvelopeBytes, r.MaxSessionBytes, r.PushedAt,
		r.ObservedAt, r.SubmissionUID, r.Status, r.Reason)
	if err != nil {
		return fmt.Errorf("record session push state: %w", err)
	}
	return nil
}

// SuspendCapturePolicy is a compare-and-set against the exact consent used by
// the upload. A stale response cannot suspend a newer request or credential.
func SuspendCapturePolicy(ctx context.Context, q Querier, scope PushScope, generation, checkedAt, retryAt int64) error {
	_, err := q.ExecContext(ctx, `UPDATE capture_policy SET state = 'suspended', retry_at = ?, checked_at = ?
		WHERE server = ? AND org_id = ? AND credential_fingerprint = ? AND since = ?
		AND generation = ? AND is_current = 1
		AND EXISTS (SELECT 1 FROM capture_refresh WHERE server = ? AND generation = ?)`,
		retryAt, checkedAt, scope.Server, scope.OrgID, scope.CredentialFingerprint, scope.ConsentSince,
		generation, scope.Server, generation)
	if err != nil {
		return fmt.Errorf("suspend session capture: %w", err)
	}
	return nil
}
