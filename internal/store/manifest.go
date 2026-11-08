package store

import (
	"context"
	"database/sql"
	"errors"
)

// SessionManifest retains only the initial scrubbed manifest, never parts.
type SessionManifest struct {
	PushScope
	Harness, SessionID, Revision string
	Manifest                     []byte
	PartBytes                    int
}

// GetSessionManifest returns the retry metadata for exactly one upload identity.
func GetSessionManifest(ctx context.Context, q Querier, r SessionManifest) (SessionManifest, bool, error) {
	err := q.QueryRowContext(ctx, `SELECT manifest, part_bytes FROM session_manifests
		WHERE server = ? AND org_id = ? AND credential_fingerprint = ? AND consent_since = ?
		AND harness = ? AND session_id = ? AND revision = ?`,
		r.Server, r.OrgID, r.CredentialFingerprint, r.ConsentSince, r.Harness, r.SessionID, r.Revision).Scan(&r.Manifest, &r.PartBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// KeepSessionManifest makes concurrent attempts use the first manifest.
func KeepSessionManifest(ctx context.Context, q Querier, r SessionManifest) (SessionManifest, error) {
	if _, err := q.ExecContext(ctx, `DELETE FROM session_manifests
		WHERE server = ? AND org_id = ? AND credential_fingerprint = ? AND consent_since = ?
		AND harness = ? AND session_id = ? AND revision != ?`,
		r.Server, r.OrgID, r.CredentialFingerprint, r.ConsentSince, r.Harness, r.SessionID, r.Revision); err != nil {
		return r, err
	}
	_, err := q.ExecContext(ctx, `INSERT INTO session_manifests
		(server, org_id, credential_fingerprint, consent_since, harness, session_id, revision, manifest, part_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		r.Server, r.OrgID, r.CredentialFingerprint, r.ConsentSince, r.Harness, r.SessionID, r.Revision, r.Manifest, r.PartBytes)
	if err != nil {
		return r, err
	}
	r, _, err = GetSessionManifest(ctx, q, r)
	return r, err
}

// DeleteSessionManifest clears metadata after completion or terminal rejection.
func DeleteSessionManifest(ctx context.Context, q Querier, r SessionManifest) error {
	_, err := q.ExecContext(ctx, `DELETE FROM session_manifests
		WHERE server = ? AND org_id = ? AND credential_fingerprint = ? AND consent_since = ?
		AND harness = ? AND session_id = ? AND revision = ?`,
		r.Server, r.OrgID, r.CredentialFingerprint, r.ConsentSince, r.Harness, r.SessionID, r.Revision)
	return err
}

// PruneSessionManifests invalidates metadata when destination consent changes.
// An empty scope removes all pending metadata for that server.
func PruneSessionManifests(ctx context.Context, q Querier, scope PushScope) error {
	_, err := q.ExecContext(ctx, `DELETE FROM session_manifests WHERE server = ?
		AND (org_id != ? OR credential_fingerprint != ? OR consent_since != ?)`,
		scope.Server, scope.OrgID, scope.CredentialFingerprint, scope.ConsentSince)
	return err
}
