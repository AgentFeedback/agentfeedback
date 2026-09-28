package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sqlite3 "modernc.org/sqlite/lib"
)

// ErrNotFound is returned by the lookups when no row matches.
var ErrNotFound = errors.New("submission not found")

// Families.
const (
	FamilyReview   = "review"
	FamilyFriction = "friction"
	FamilyEvent    = "event"
)

// Submission is one stored record. Payload stays raw: it is written and
// returned verbatim, never decoded into a map (which would reorder keys and
// reformat numbers).
type Submission struct {
	ID               int64           `json:"id"`
	Family           string          `json:"family"`
	SubmissionType   string          `json:"submission_type"`
	MachineName      string          `json:"machine_name"`
	CoordinatorModel string          `json:"coordinator_model"`
	RunID            *string         `json:"run_id"`
	Payload          json.RawMessage `json:"payload"`
	PayloadHash      string          `json:"payload_hash"`
	CreatedAt        int64           `json:"created_at"`
	ProcessedAt      *int64          `json:"processed_at"`
	Resolution       *string         `json:"resolution"`
}

// Summary is a Submission without the payload, plus the friction fields lifted
// out of it so a list is scannable.
type Summary struct {
	ID               int64
	Family           string
	SubmissionType   string
	MachineName      string
	CoordinatorModel string
	RunID            *string
	PayloadHash      string
	CreatedAt        int64
	ProcessedAt      *int64
	Resolution       *string
	Category         string
	Summary          string
	Project          string
	Harness          string
	Payload          json.RawMessage // only populated when the caller asked for it
}

const submissionColumns = `id, family, submission_type, machine_name, coordinator_model,
	run_id, payload, payload_hash, created_at, processed_at, resolution`

func scanSubmission(row interface{ Scan(...any) error }) (Submission, error) {
	var s Submission
	var payload []byte
	if err := row.Scan(&s.ID, &s.Family, &s.SubmissionType, &s.MachineName, &s.CoordinatorModel,
		&s.RunID, &payload, &s.PayloadHash, &s.CreatedAt, &s.ProcessedAt, &s.Resolution); err != nil {
		return Submission{}, err
	}
	s.Payload = json.RawMessage(payload)

	return s, nil
}

// InsertSubmission writes a new record and returns it with its assigned id.
func InsertSubmission(ctx context.Context, q Querier, s Submission) (Submission, error) {
	res, err := q.ExecContext(ctx, `
		INSERT INTO submissions (family, submission_type, machine_name, coordinator_model,
			run_id, payload, payload_hash, created_at, processed_at, resolution)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.Family, s.SubmissionType, s.MachineName, s.CoordinatorModel,
		s.RunID, string(s.Payload), s.PayloadHash, s.CreatedAt, s.ProcessedAt, s.Resolution)
	if err != nil {
		return Submission{}, fmt.Errorf("insert submission: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Submission{}, fmt.Errorf("insert submission: read id: %w", err)
	}
	s.ID = id

	return s, nil
}

// InsertSubmissionWithID writes a record preserving its id — the importer's
// path, never the API's.
func InsertSubmissionWithID(ctx context.Context, q Querier, s Submission) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO submissions (id, family, submission_type, machine_name, coordinator_model,
			run_id, payload, payload_hash, created_at, processed_at, resolution)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Family, s.SubmissionType, s.MachineName, s.CoordinatorModel,
		s.RunID, string(s.Payload), s.PayloadHash, s.CreatedAt, s.ProcessedAt, s.Resolution)
	if err != nil {
		return fmt.Errorf("insert submission %d: %w", s.ID, err)
	}

	return nil
}

// GetSubmissionByID returns one record, ErrNotFound when the id is unknown.
func GetSubmissionByID(ctx context.Context, q Querier, id int64) (Submission, error) {
	s, err := scanSubmission(q.QueryRowContext(ctx, `SELECT `+submissionColumns+` FROM submissions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Submission{}, ErrNotFound
	}
	if err != nil {
		return Submission{}, fmt.Errorf("get submission %d: %w", id, err)
	}

	return s, nil
}

// GetSubmissionByKey returns the record stored under an idempotency key.
func GetSubmissionByKey(ctx context.Context, q Querier, family, submissionType, runID string) (Submission, error) {
	s, err := scanSubmission(q.QueryRowContext(ctx, `SELECT `+submissionColumns+`
		FROM submissions WHERE family = ? AND submission_type = ? AND run_id = ?`,
		family, submissionType, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return Submission{}, ErrNotFound
	}
	if err != nil {
		return Submission{}, fmt.Errorf("get submission by key: %w", err)
	}

	return s, nil
}

// GetRecentFrictionByHash returns the newest friction with the given content
// hash created at or after since.
func GetRecentFrictionByHash(ctx context.Context, q Querier, hash string, since int64) (Submission, error) {
	s, err := scanSubmission(q.QueryRowContext(ctx, `SELECT `+submissionColumns+`
		FROM submissions
		WHERE family = 'friction' AND payload_hash = ? AND created_at >= ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, hash, since))
	if errors.Is(err, sql.ErrNoRows) {
		return Submission{}, ErrNotFound
	}
	if err != nil {
		return Submission{}, fmt.Errorf("get recent friction by hash: %w", err)
	}

	return s, nil
}

// ListFilter selects the rows a list or export returns. Zero values mean "no
// filter".
type ListFilter struct {
	Family    string
	Type      string
	Machine   string
	Model     string
	Since     *int64
	Until     *int64
	Processed *bool
	BeforeID  *int64
}

func (f ListFilter) where() (string, []any) {
	var clauses []string
	var args []any

	add := func(clause string, arg any) {
		clauses = append(clauses, clause)
		args = append(args, arg)
	}
	if f.Family != "" {
		add("family = ?", f.Family)
	}
	if f.Type != "" {
		add("submission_type = ?", f.Type)
	}
	if f.Machine != "" {
		add("machine_name = ?", f.Machine)
	}
	if f.Model != "" {
		add("coordinator_model = ?", f.Model)
	}
	if f.Since != nil {
		add("created_at >= ?", *f.Since)
	}
	if f.Until != nil {
		add("created_at <= ?", *f.Until)
	}
	if f.Processed != nil {
		if *f.Processed {
			clauses = append(clauses, "processed_at IS NOT NULL")
		} else {
			clauses = append(clauses, "processed_at IS NULL")
		}
	}
	if f.BeforeID != nil {
		add("id < ?", *f.BeforeID)
	}
	if len(clauses) == 0 {
		return "", nil
	}

	return " WHERE " + strings.Join(clauses, " AND "), args
}

// CountSubmissions counts every row matching the filter. BeforeID is ignored:
// the total describes the whole filtered set, not the current page.
func CountSubmissions(ctx context.Context, q Querier, f ListFilter) (int64, error) {
	f.BeforeID = nil
	where, args := f.where()

	var n int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM submissions`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count submissions: %w", err)
	}

	return n, nil
}

// ListSubmissions returns up to limit rows, newest id first, skipping offset
// rows. withPayload adds the full payload to each row.
func ListSubmissions(ctx context.Context, q Querier, f ListFilter, limit, offset int, withPayload bool) ([]Summary, error) {
	where, args := f.where()

	payloadCol := "NULL"
	if withPayload {
		payloadCol = "payload"
	}
	query := `SELECT id, family, submission_type, machine_name, coordinator_model, run_id,
		payload_hash, created_at, processed_at, resolution,
		CASE WHEN family = 'friction' THEN json_extract(payload, '$.category') END,
		CASE WHEN family = 'friction' THEN json_extract(payload, '$.summary') END,
		CASE WHEN family = 'friction' THEN json_extract(payload, '$.project') END,
		CASE WHEN family = 'friction' THEN json_extract(payload, '$.harness') END,
		` + payloadCol + `
		FROM submissions` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`

	rows, err := q.QueryContext(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, fmt.Errorf("list submissions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Summary{}
	for rows.Next() {
		var s Summary
		var category, summary, project, harness sql.NullString
		var payload []byte
		if err := rows.Scan(&s.ID, &s.Family, &s.SubmissionType, &s.MachineName, &s.CoordinatorModel,
			&s.RunID, &s.PayloadHash, &s.CreatedAt, &s.ProcessedAt, &s.Resolution,
			&category, &summary, &project, &harness, &payload); err != nil {
			return nil, fmt.Errorf("scan submission row: %w", err)
		}
		s.Category, s.Summary, s.Project, s.Harness = category.String, summary.String, project.String, harness.String
		if withPayload {
			s.Payload = json.RawMessage(payload)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list submissions: %w", err)
	}

	return out, nil
}

// IterateSubmissions streams every matching row in ascending id order.
func IterateSubmissions(ctx context.Context, q Querier, f ListFilter, fn func(Submission) error) error {
	where, args := f.where()

	rows, err := q.QueryContext(ctx, `SELECT `+submissionColumns+` FROM submissions`+where+` ORDER BY id ASC`, args...)
	if err != nil {
		return fmt.Errorf("iterate submissions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		s, err := scanSubmission(rows)
		if err != nil {
			return fmt.Errorf("scan submission row: %w", err)
		}
		if err := fn(s); err != nil {
			return err
		}
	}

	return rows.Err()
}

// ProcessingState is the mutable part of a row, read before a processed update
// decides what changed.
type ProcessingState struct {
	ID          int64
	ProcessedAt *int64
	Resolution  *string
}

// GetProcessingStates returns the current state of every requested id that exists.
func GetProcessingStates(ctx context.Context, q Querier, ids []int64) (map[int64]ProcessingState, error) {
	if len(ids) == 0 {
		return map[int64]ProcessingState{}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := q.QueryContext(ctx,
		`SELECT id, processed_at, resolution FROM submissions WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read processing states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64]ProcessingState, len(ids))
	for rows.Next() {
		var st ProcessingState
		if err := rows.Scan(&st.ID, &st.ProcessedAt, &st.Resolution); err != nil {
			return nil, fmt.Errorf("scan processing state: %w", err)
		}
		out[st.ID] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read processing states: %w", err)
	}

	return out, nil
}

// MarkProcessed stamps processed_at (only when not already set) and stores the
// resolution. A nil resolution leaves the stored one untouched.
func MarkProcessed(ctx context.Context, q Querier, ids []int64, now int64, resolution *string) error {
	for _, id := range ids {
		var err error
		if resolution == nil {
			_, err = q.ExecContext(ctx,
				`UPDATE submissions SET processed_at = COALESCE(processed_at, ?) WHERE id = ?`, now, id)
		} else {
			_, err = q.ExecContext(ctx,
				`UPDATE submissions SET processed_at = COALESCE(processed_at, ?), resolution = ? WHERE id = ?`,
				now, *resolution, id)
		}
		if err != nil {
			return fmt.Errorf("mark submission %d processed: %w", id, err)
		}
	}

	return nil
}

// UnmarkProcessed clears both the timestamp and the resolution.
func UnmarkProcessed(ctx context.Context, q Querier, ids []int64) error {
	for _, id := range ids {
		if _, err := q.ExecContext(ctx,
			`UPDATE submissions SET processed_at = NULL, resolution = NULL WHERE id = ?`, id); err != nil {
			return fmt.Errorf("unmark submission %d: %w", id, err)
		}
	}

	return nil
}

// CountAll reports how many rows the table holds (the importer's emptiness check).
func CountAll(ctx context.Context, q Querier) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM submissions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count submissions: %w", err)
	}

	return n, nil
}

// SetSequence pushes sqlite_sequence for submissions to at least value, so ids
// imported from another database are never handed out again.
func SetSequence(ctx context.Context, q Querier, value int64) error {
	res, err := q.ExecContext(ctx,
		`UPDATE sqlite_sequence SET seq = ? WHERE name = 'submissions' AND seq < ?`, value, value)
	if err != nil {
		return fmt.Errorf("advance id sequence: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("advance id sequence: %w", err)
	}
	if n == 0 {
		var existing sql.NullInt64
		if err := q.QueryRowContext(ctx,
			`SELECT seq FROM sqlite_sequence WHERE name = 'submissions'`).Scan(&existing); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("advance id sequence: %w", err)
		}
		if !existing.Valid {
			if _, err := q.ExecContext(ctx,
				`INSERT INTO sqlite_sequence (name, seq) VALUES ('submissions', ?)`, value); err != nil {
				return fmt.Errorf("advance id sequence: %w", err)
			}
		}
	}

	return nil
}

// IsUniqueViolation reports whether err is SQLite's unique-constraint failure.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if primary, full, ok := sqliteCode(err); ok {
		return full == sqlite3.SQLITE_CONSTRAINT_UNIQUE || full == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY ||
			(primary == sqlite3.SQLITE_CONSTRAINT && strings.Contains(err.Error(), "UNIQUE constraint failed"))
	}

	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// VacuumInto writes a consistent copy of the database to dest.
func (db *DB) VacuumInto(ctx context.Context, dest string) error {
	if _, err := db.writer.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("vacuum into %s: %w", dest, err)
	}

	return nil
}
