package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotDigested is returned by MarkSession when the session has no row or
// its row was never digested: only a digested session can be marked, since
// marking copies the digest watermark.
var ErrNotDigested = errors.New("session not digested")

// SessionSeen is one sessions_seen row. Nullable columns are nil (integers)
// or "" (text) when NULL; Refs is the stored JSON array, nil when NULL.
type SessionSeen struct {
	Harness        string
	SessionID      string
	Path           string
	Mtime          int64 // unix nanoseconds of the file at the last digest
	Size           int64 // bytes of complete lines the last digest covered
	Head           string
	DigestedAt     *int64
	ProcessedAt    *int64
	ProcessedSize  *int64
	ProcessedMtime *int64
	ProcessedHead  string
	Outcome        string
	Refs           json.RawMessage
}

// SessionDigest is the watermark one digest records.
type SessionDigest struct {
	Harness    string
	SessionID  string
	Path       string
	Mtime      int64
	Size       int64
	Head       string
	DigestedAt int64
}

const sessionColumns = `harness, session_id, path, mtime, size, head, digested_at, processed_at,
	processed_size, processed_mtime, processed_head, outcome, refs`

// The statements below are package-level so the plan test explains them.
const (
	sqlGetSession   = `SELECT ` + sessionColumns + ` FROM sessions_seen WHERE harness = ? AND session_id = ?`
	sqlListSessions = `SELECT ` + sessionColumns + ` FROM sessions_seen WHERE harness = ? ORDER BY session_id`
	sqlUpsertDigest = `INSERT INTO sessions_seen (harness, session_id, path, mtime, size, head, digested_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (harness, session_id) DO UPDATE SET
	  path = excluded.path, mtime = excluded.mtime, size = excluded.size, head = excluded.head,
	  digested_at = excluded.digested_at`
	sqlMarkSession = `UPDATE sessions_seen SET
	  processed_size = size, processed_mtime = mtime, processed_head = head,
	  processed_at = ?, outcome = ?, refs = ?
	WHERE harness = ? AND session_id = ? AND digested_at IS NOT NULL`
	sqlGetTriageState = `SELECT value FROM triage_state WHERE key = ?`
	sqlSetTriageState = `INSERT INTO triage_state (key, value) VALUES (?, ?)
	ON CONFLICT (key) DO UPDATE SET value = excluded.value`
)

func scanSessionSeen(row scanner) (SessionSeen, error) {
	var s SessionSeen
	var processedHead, outcome, refs sql.NullString
	if err := row.Scan(&s.Harness, &s.SessionID, &s.Path, &s.Mtime, &s.Size, &s.Head, &s.DigestedAt,
		&s.ProcessedAt, &s.ProcessedSize, &s.ProcessedMtime, &processedHead, &outcome, &refs); err != nil {
		return SessionSeen{}, err
	}
	s.ProcessedHead, s.Outcome = processedHead.String, outcome.String
	if refs.Valid {
		s.Refs = json.RawMessage(refs.String)
	}

	return s, nil
}

// GetSession returns the row of (harness, sessionID); ok is false when there
// is none.
func GetSession(ctx context.Context, q Querier, harness, sessionID string) (s SessionSeen, ok bool, err error) {
	s, err = scanSessionSeen(q.QueryRowContext(ctx, sqlGetSession, harness, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return SessionSeen{}, false, nil
	}
	if err != nil {
		return SessionSeen{}, false, fmt.Errorf("get session %s:%s: %w", harness, sessionID, err)
	}

	return s, true, nil
}

// ListSessions returns every row of harness, by session id.
func ListSessions(ctx context.Context, q Querier, harness string) ([]SessionSeen, error) {
	rows, err := q.QueryContext(ctx, sqlListSessions, harness)
	if err != nil {
		return nil, fmt.Errorf("list sessions of %s: %w", harness, err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionSeen
	for rows.Next() {
		s, err := scanSessionSeen(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions of %s: %w", harness, err)
	}

	return out, nil
}

// UpsertSessionDigest records a digest watermark: path, mtime, size, head and
// digested_at. The processed_* columns, outcome and refs are left alone.
func UpsertSessionDigest(ctx context.Context, q Querier, d SessionDigest) error {
	if _, err := q.ExecContext(ctx, sqlUpsertDigest, d.Harness, d.SessionID, d.Path, d.Mtime, d.Size, d.Head, d.DigestedAt); err != nil {
		return fmt.Errorf("record digest of %s:%s: %w", d.Harness, d.SessionID, err)
	}

	return nil
}

// MarkSession copies the digest watermark into the processed_* columns and
// sets processed_at, outcome and refs (a JSON array; nil stores []). It
// returns ErrNotDigested when the session has no row or was never digested.
func MarkSession(ctx context.Context, q Querier, harness, sessionID string, processedAt int64, outcome string, refs json.RawMessage) error {
	if refs == nil {
		refs = json.RawMessage(`[]`)
	}
	res, err := q.ExecContext(ctx, sqlMarkSession, processedAt, outcome, jsonText(refs), harness, sessionID)
	if err != nil {
		return fmt.Errorf("mark session %s:%s: %w", harness, sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark session %s:%s: %w", harness, sessionID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s:%s", ErrNotDigested, harness, sessionID)
	}

	return nil
}

// TriageState returns the value under key; ok is false when there is none.
func TriageState(ctx context.Context, q Querier, key string) (value string, ok bool, err error) {
	err = q.QueryRowContext(ctx, sqlGetTriageState, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get triage state %s: %w", key, err)
	}

	return value, true, nil
}

// SetTriageState writes value under key, replacing any previous value.
func SetTriageState(ctx context.Context, q Querier, key, value string) error {
	if _, err := q.ExecContext(ctx, sqlSetTriageState, key, value); err != nil {
		return fmt.Errorf("set triage state %s: %w", key, err)
	}

	return nil
}
