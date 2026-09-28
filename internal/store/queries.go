package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

// ErrNotFound is returned by the lookups when no row matches.
var ErrNotFound = errors.New("submission not found")

// KindUnknown is the kind the decoder assigns to an un-kinded body; the list
// filter kind=unknown selects those rows like any other kind.
const KindUnknown = "unknown"

// Tombstone is the payload of a redacted row. Everything else the redaction
// removes is set to NULL; the hash and the identity fields stay.
const Tombstone = `{"redacted":true}`

// Submission is one stored record, every column of the table. Optional text
// columns are "" when NULL; nullable timestamps are nil when NULL. Payload
// and Context are the stored bytes, never decoded on the way out.
type Submission struct {
	ID            int64
	UID           string
	Kind          string
	SchemaVersion int64
	Key           string
	Summary       string
	Machine       string
	Model         string
	Harness       string
	Project       string
	OccurredAt    *int64
	Context       json.RawMessage // nil when absent
	Payload       json.RawMessage // nil only on list rows fetched without the payload
	ContentHash   string
	CreatedAt     int64
	ProcessedAt   *int64
	Verdict       string
	Resolution    string
	Ref           string
	ProcessedBy   string
	RedactedAt    *int64
}

// columns lists the table's stored columns in scan order. payloadColumn is
// the placeholder a list without payloads substitutes with NULL.
const (
	columnsBefore = `id, uid, kind, schema_version, key, summary, machine, model, harness, project,
	occurred_at, context, `
	columnsAfter = `, content_hash, created_at, processed_at, verdict, resolution, ref,
	processed_by, redacted_at`
	columns = columnsBefore + "payload" + columnsAfter
)

func selectColumns(withPayload bool) string {
	if withPayload {
		return columns
	}

	return columnsBefore + "NULL" + columnsAfter
}

type scanner interface{ Scan(dest ...any) error }

func scanSubmission(row scanner) (Submission, error) {
	var s Submission
	var key, summary, machine, model, harness, project, verdict, resolution, ref, processedBy sql.NullString
	var contextBytes, payload []byte
	if err := row.Scan(&s.ID, &s.UID, &s.Kind, &s.SchemaVersion, &key, &summary, &machine, &model, &harness,
		&project, &s.OccurredAt, &contextBytes, &payload, &s.ContentHash, &s.CreatedAt, &s.ProcessedAt,
		&verdict, &resolution, &ref, &processedBy, &s.RedactedAt); err != nil {
		return Submission{}, err
	}
	s.Key, s.Summary, s.Machine, s.Model = key.String, summary.String, machine.String, model.String
	s.Harness, s.Project = harness.String, project.String
	s.Verdict, s.Resolution, s.Ref, s.ProcessedBy = verdict.String, resolution.String, ref.String, processedBy.String
	if contextBytes != nil {
		s.Context = json.RawMessage(contextBytes)
	}
	if payload != nil {
		s.Payload = json.RawMessage(payload)
	}

	return s, nil
}

// nullText maps the package convention ("" is absent) onto SQL NULL.
func nullText(s string) any {
	if s == "" {
		return nil
	}

	return s
}

// jsonText binds JSON bytes as TEXT. A []byte argument would be stored as a
// BLOB, which the JSON functions read as JSONB and reject.
func jsonText(b []byte) any {
	if b == nil {
		return nil
	}

	return string(b)
}

func insertArgs(s Submission) []any {
	return []any{
		s.UID, s.Kind, s.SchemaVersion, nullText(s.Key), nullText(s.Summary), nullText(s.Machine),
		nullText(s.Model), nullText(s.Harness), nullText(s.Project), s.OccurredAt, jsonText(s.Context),
		string(s.Payload), s.ContentHash, s.CreatedAt, s.ProcessedAt, nullText(s.Verdict),
		nullText(s.Resolution), nullText(s.Ref), nullText(s.ProcessedBy), s.RedactedAt,
	}
}

const insertColumns = `uid, kind, schema_version, key, summary, machine, model, harness, project,
	occurred_at, context, payload, content_hash, created_at, processed_at, verdict, resolution, ref,
	processed_by, redacted_at`

// Insert writes a new record and returns its assigned id. The caller supplies
// uid, content_hash and created_at; a duplicate (kind, key) or uid fails with
// an error IsUniqueViolation recognises.
func Insert(ctx context.Context, q Querier, s Submission) (int64, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO submissions (`+insertColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, insertArgs(s)...)
	if err != nil {
		return 0, fmt.Errorf("insert submission: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert submission: read id: %w", err)
	}

	return id, nil
}

// InsertWithID writes a record keeping its id: the server-host restore path,
// never the API's import route, which assigns new ids. Pair it with
// SetSequence so restored ids are never handed out again.
func InsertWithID(ctx context.Context, q Querier, s Submission) error {
	args := append([]any{s.ID}, insertArgs(s)...)
	if _, err := q.ExecContext(ctx, `INSERT INTO submissions (id, `+insertColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...); err != nil {
		return fmt.Errorf("insert submission %d: %w", s.ID, err)
	}

	return nil
}

func getOne(ctx context.Context, q Querier, what, where string, args ...any) (Submission, error) {
	s, err := scanSubmission(q.QueryRowContext(ctx, `SELECT `+columns+` FROM submissions WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Submission{}, ErrNotFound
	}
	if err != nil {
		return Submission{}, fmt.Errorf("get submission by %s: %w", what, err)
	}

	return s, nil
}

// Lookup predicates, shared with the query-plan test.
const (
	whereByID         = `id = ?`
	whereByUID        = `uid = ?`
	whereByKey        = `kind = ? AND key = ?`
	whereRecentByHash = `content_hash = ? AND created_at >= ? AND processed_at IS NULL
		ORDER BY created_at DESC, id DESC LIMIT 1`
)

// GetByID returns one record, ErrNotFound when the id is unknown.
func GetByID(ctx context.Context, q Querier, id int64) (Submission, error) {
	return getOne(ctx, q, "id", whereByID, id)
}

// GetByUID returns the record carrying uid, ErrNotFound when absent.
func GetByUID(ctx context.Context, q Querier, uid string) (Submission, error) {
	return getOne(ctx, q, "uid", whereByUID, uid)
}

// GetByKey returns the record stored under (kind, key), ErrNotFound when absent.
func GetByKey(ctx context.Context, q Querier, kind, key string) (Submission, error) {
	return getOne(ctx, q, "key", whereByKey, kind, key)
}

// GetRecentByHash is the keyless dedupe lookup: the newest row with the given
// content hash, created at or after since, that has not been processed.
// Processed rows are skipped so a recurring problem files again after triage.
func GetRecentByHash(ctx context.Context, q Querier, hash string, since int64) (Submission, error) {
	return getOne(ctx, q, "hash", whereRecentByHash, hash, since)
}

// ListFilter selects rows for a list, a count, the stats and their groups.
// Zero values mean "no filter". Timestamps are unix microseconds and match
// inclusively on the column On names.
type ListFilter struct {
	Kind          string
	SchemaVersion *int64
	Key           string
	Machine       string
	Model         string
	Project       string
	Harness       string
	Category      string
	FixStatus     string
	ExcludeKinds  []string
	Verdict       string
	Processed     *bool
	Redacted      *bool
	ContentHash   string
	Since         *int64
	Until         *int64
	On            string // OnCreatedAt (default when "") or OnOccurredAt
	Q             string // case-insensitive substring; see qClause
}

// Values of ListFilter.On.
const (
	OnCreatedAt  = "created_at"
	OnOccurredAt = "occurred_at"
)

// qClause matches the summary and every string value in the payload, at any
// depth, after the simple lower-case mapping on both sides. Keys, numbers and
// booleans are not searched, and escaped text is matched in its decoded form.
// It has no index by design; another filter in the same request narrows the
// rows it scans.
const qClause = `(instr(` + lowerFunc + `(summary), ?) > 0 OR EXISTS (
		SELECT 1 FROM json_tree(payload) WHERE type = 'text' AND instr(` + lowerFunc + `(value), ?) > 0))`

type whereBuilder struct {
	clauses []string
	args    []any
}

func (w *whereBuilder) add(clause string, args ...any) {
	w.clauses = append(w.clauses, clause)
	w.args = append(w.args, args...)
}

func (w *whereBuilder) sql() string {
	if len(w.clauses) == 0 {
		return ""
	}

	return " WHERE " + strings.Join(w.clauses, " AND ")
}

func (f ListFilter) onColumn() (string, error) {
	switch f.On {
	case "", OnCreatedAt:
		return OnCreatedAt, nil
	case OnOccurredAt:
		return OnOccurredAt, nil
	default:
		return "", fmt.Errorf("list filter: on must be %s or %s, got %q", OnCreatedAt, OnOccurredAt, f.On)
	}
}

func (f ListFilter) where() (*whereBuilder, error) {
	on, err := f.onColumn()
	if err != nil {
		return nil, err
	}
	w := &whereBuilder{}
	for _, c := range []struct {
		column, value string
	}{
		{"kind", f.Kind}, {"key", f.Key}, {"machine", f.Machine}, {"model", f.Model},
		{"project", f.Project}, {"harness", f.Harness}, {"category", f.Category},
		{"fix_status", f.FixStatus}, {"verdict", f.Verdict}, {"content_hash", f.ContentHash},
	} {
		if c.value != "" {
			w.add(c.column+" = ?", c.value)
		}
	}
	if f.SchemaVersion != nil {
		w.add("schema_version = ?", *f.SchemaVersion)
	}
	if len(f.ExcludeKinds) > 0 {
		args := make([]any, len(f.ExcludeKinds))
		for i, k := range f.ExcludeKinds {
			args[i] = k
		}
		w.add("kind NOT IN ("+placeholders(len(args))+")", args...)
	}
	if f.Processed != nil {
		w.add("processed_at IS " + isNull(!*f.Processed))
	}
	if f.Redacted != nil {
		w.add("redacted_at IS " + isNull(!*f.Redacted))
	}
	if f.Since != nil {
		w.add(on+" >= ?", *f.Since)
	}
	if f.Until != nil {
		w.add(on+" <= ?", *f.Until)
	}
	if f.Q != "" {
		needle := schema.LowerSimple(f.Q)
		w.add(qClause, needle, needle)
	}

	return w, nil
}

func isNull(null bool) string {
	if null {
		return "NULL"
	}

	return "NOT NULL"
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Page selects one page of a list. Exactly one of BeforeID and AfterID may be
// set: BeforeID pages newest first over id < BeforeID, AfterID oldest first
// over id > AfterID (0 is a valid start). Neither set means newest first from
// the top. Limit must be positive; the caller asks for one row more than it
// returns to learn whether another page exists.
type Page struct {
	BeforeID *int64
	AfterID  *int64
	Limit    int
}

func listSQL(f ListFilter, page Page, withPayload bool) (string, []any, error) {
	if page.Limit <= 0 {
		return "", nil, fmt.Errorf("list: limit must be positive, got %d", page.Limit)
	}
	if page.BeforeID != nil && page.AfterID != nil {
		return "", nil, errors.New("list: before_id and after_id are exclusive")
	}
	w, err := f.where()
	if err != nil {
		return "", nil, err
	}
	order := " ORDER BY id DESC"
	switch {
	case page.BeforeID != nil:
		w.add("id < ?", *page.BeforeID)
	case page.AfterID != nil:
		w.add("id > ?", *page.AfterID)
		order = " ORDER BY id ASC"
	}
	args := append(w.args, page.Limit)

	return `SELECT ` + selectColumns(withPayload) + ` FROM submissions` + w.sql() + order + ` LIMIT ?`, args, nil
}

// List returns one page of rows matching f. Rows carry the payload only when
// withPayload is set; the other columns are always present.
func List(ctx context.Context, q Querier, f ListFilter, page Page, withPayload bool) ([]Submission, error) {
	query, args, err := listSQL(f, page, withPayload)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list submissions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Submission{}
	for rows.Next() {
		s, err := scanSubmission(rows)
		if err != nil {
			return nil, fmt.Errorf("scan submission row: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list submissions: %w", err)
	}

	return out, nil
}

func countSQL(f ListFilter) (string, []any, error) {
	w, err := f.where()
	if err != nil {
		return "", nil, err
	}

	return `SELECT COUNT(*) FROM submissions` + w.sql(), w.args, nil
}

// Count counts every row matching f, ignoring any page: the total describes
// the whole filtered set. Run it in the same Read as the List it describes.
func Count(ctx context.Context, q Querier, f ListFilter) (int64, error) {
	query, args, err := countSQL(f)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := q.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count submissions: %w", err)
	}

	return n, nil
}

// Totals are the top-level counters of the stats.
type Totals struct {
	Total     int64
	Open      int64
	Processed int64
	Redacted  int64
}

func totalsSQL(f ListFilter) (string, []any, error) {
	w, err := f.where()
	if err != nil {
		return "", nil, err
	}

	return `SELECT COUNT(*), COALESCE(SUM(processed_at IS NULL), 0), COALESCE(SUM(processed_at IS NOT NULL), 0),
		COALESCE(SUM(redacted_at IS NOT NULL), 0) FROM submissions` + w.sql(), w.args, nil
}

// StatsTotals counts the rows matching f, split by processing and redaction.
func StatsTotals(ctx context.Context, q Querier, f ListFilter) (Totals, error) {
	query, args, err := totalsSQL(f)
	if err != nil {
		return Totals{}, err
	}
	var t Totals
	if err := q.QueryRowContext(ctx, query, args...).Scan(&t.Total, &t.Open, &t.Processed, &t.Redacted); err != nil {
		return Totals{}, fmt.Errorf("stats totals: %w", err)
	}

	return t, nil
}

// GroupKeys are the columns a stats request may group by, in the contract's
// order. Every name is a real column, so the store never interpolates input.
var GroupKeys = []string{"kind", "project", "category", "fix_status", "machine", "model", "harness", "verdict", "schema_version"}

// MaxGroupKeys and MaxGroups bound one stats response.
const (
	MaxGroupKeys = 3
	MaxGroups    = 100
)

// Group is one row of the stats groups: its key values and its counters.
// Keys holds a string per text column and an int64 for schema_version.
type Group struct {
	Keys      map[string]any
	Total     int64
	Open      int64
	Processed int64
}

func validGroupKeys(by []string) error {
	if len(by) == 0 || len(by) > MaxGroupKeys {
		return fmt.Errorf("stats groups: between 1 and %d keys, got %d", MaxGroupKeys, len(by))
	}
	seen := map[string]bool{}
	for _, k := range by {
		known := false
		for _, g := range GroupKeys {
			if g == k {
				known = true
			}
		}
		if !known {
			return fmt.Errorf("stats groups: unknown key %q", k)
		}
		if seen[k] {
			return fmt.Errorf("stats groups: key %q repeated", k)
		}
		seen[k] = true
	}

	return nil
}

func groupsSQL(f ListFilter, by []string) (string, []any, error) {
	if err := validGroupKeys(by); err != nil {
		return "", nil, err
	}
	w, err := f.where()
	if err != nil {
		return "", nil, err
	}
	// A row without a value for a grouping key is in no group: the response
	// cannot carry a null key, and a partial index on category or fix_status
	// is only usable under that predicate.
	for _, k := range by {
		w.add(k + " IS NOT NULL")
	}
	keys := strings.Join(by, ", ")

	return `SELECT ` + keys + `, COUNT(*), SUM(processed_at IS NULL), SUM(processed_at IS NOT NULL)
		FROM submissions` + w.sql() + ` GROUP BY ` + keys + ` ORDER BY COUNT(*) DESC, ` + keys +
		` LIMIT ?`, append(w.args, MaxGroups), nil
}

// StatsGroups groups the rows matching f by up to three keys from GroupKeys,
// largest group first, ties in key order, at most MaxGroups groups.
func StatsGroups(ctx context.Context, q Querier, f ListFilter, by []string) ([]Group, error) {
	query, args, err := groupsSQL(f, by)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("stats groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Group{}
	for rows.Next() {
		g := Group{Keys: make(map[string]any, len(by))}
		dest := make([]any, 0, len(by)+3)
		texts := make([]sql.NullString, len(by))
		ints := make([]sql.NullInt64, len(by))
		for i, k := range by {
			if k == "schema_version" {
				dest = append(dest, &ints[i])
			} else {
				dest = append(dest, &texts[i])
			}
		}
		dest = append(dest, &g.Total, &g.Open, &g.Processed)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan stats group: %w", err)
		}
		for i, k := range by {
			if k == "schema_version" {
				g.Keys[k] = ints[i].Int64
			} else {
				g.Keys[k] = texts[i].String
			}
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats groups: %w", err)
	}

	return out, nil
}

// Recurrence is one recurring content hash among the rows matching a filter,
// described by its newest row.
type Recurrence struct {
	ContentHash string
	Count       int64
	FirstID     int64
	LastID      int64
	Summary     string
	Kind        string
	Project     string
}

func recurringSQL(f ListFilter, top int) (string, []any, error) {
	if top < 0 {
		return "", nil, fmt.Errorf("stats recurring: top must not be negative, got %d", top)
	}
	w, err := f.where()
	if err != nil {
		return "", nil, err
	}

	return `SELECT r.content_hash, r.n, r.first_id, r.last_id, s.summary, s.kind, s.project
		FROM (SELECT content_hash, COUNT(*) AS n, MIN(id) AS first_id, MAX(id) AS last_id
			FROM submissions` + w.sql() + ` GROUP BY content_hash HAVING COUNT(*) > 1
			ORDER BY n DESC, last_id DESC LIMIT ?) AS r
		JOIN submissions AS s ON s.id = r.last_id
		ORDER BY r.n DESC, r.last_id DESC`, append(w.args, top), nil
}

// StatsRecurring returns the top content hashes that occur more than once
// among the rows matching f, most frequent first, newest first among equals.
func StatsRecurring(ctx context.Context, q Querier, f ListFilter, top int) ([]Recurrence, error) {
	query, args, err := recurringSQL(f, top)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("stats recurring: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Recurrence{}
	for rows.Next() {
		var r Recurrence
		var summary, project sql.NullString
		if err := rows.Scan(&r.ContentHash, &r.Count, &r.FirstID, &r.LastID, &summary, &r.Kind, &project); err != nil {
			return nil, fmt.Errorf("scan stats recurrence: %w", err)
		}
		r.Summary, r.Project = summary.String, project.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats recurring: %w", err)
	}

	return out, nil
}

// Values of the stats bucket.
const (
	BucketDay  = "day"
	BucketWeek = "week"
)

// Bucket is one point of the stats time series: the UTC day, or the Monday
// of the ISO week, as YYYY-MM-DD.
type Bucket struct {
	Bucket string
	Total  int64
	Open   int64
}

func seriesSQL(f ListFilter, bucket string) (string, []any, error) {
	on, err := f.onColumn()
	if err != nil {
		return "", nil, err
	}
	// Microseconds become fractional unixepoch seconds; date() floors them
	// to the UTC day. The week is the Monday on or before that day: step
	// back six days, then forward to the next Monday (or stay on one).
	var expr string
	switch bucket {
	case BucketDay:
		expr = `date(` + on + ` / 1000000.0, 'unixepoch')`
	case BucketWeek:
		expr = `date(` + on + ` / 1000000.0, 'unixepoch', '-6 days', 'weekday 1')`
	default:
		return "", nil, fmt.Errorf("stats series: bucket must be %s or %s, got %q", BucketDay, BucketWeek, bucket)
	}
	w, err := f.where()
	if err != nil {
		return "", nil, err
	}
	w.add(on + " IS NOT NULL")

	return `SELECT ` + expr + ` AS bucket, COUNT(*), SUM(processed_at IS NULL) FROM submissions` + w.sql() +
		` GROUP BY bucket ORDER BY bucket`, w.args, nil
}

// StatsSeries buckets the rows matching f on the timestamp f.On names,
// ascending; rows without that timestamp are not counted.
func StatsSeries(ctx context.Context, q Querier, f ListFilter, bucket string) ([]Bucket, error) {
	query, args, err := seriesSQL(f, bucket)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("stats series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Bucket, &b.Total, &b.Open); err != nil {
			return nil, fmt.Errorf("scan stats bucket: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats series: %w", err)
	}

	return out, nil
}

// ProcessingState is the mutable part of a row, read before a mark decides
// what changed.
type ProcessingState struct {
	ID          int64
	ProcessedAt *int64
	Verdict     string
	Resolution  string
	Ref         string
	ProcessedBy string
	RedactedAt  *int64
}

// ProcessingStates returns the current state of every requested id that
// exists; ids missing from the map are unknown.
func ProcessingStates(ctx context.Context, q Querier, ids []int64) (map[int64]ProcessingState, error) {
	out := make(map[int64]ProcessingState, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT id, processed_at, verdict, resolution, ref, processed_by, redacted_at
		FROM submissions WHERE id IN (`+placeholders(len(ids))+`)`, idArgs(ids)...)
	if err != nil {
		return nil, fmt.Errorf("read processing states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var st ProcessingState
		var verdict, resolution, ref, processedBy sql.NullString
		if err := rows.Scan(&st.ID, &st.ProcessedAt, &verdict, &resolution, &ref, &processedBy, &st.RedactedAt); err != nil {
			return nil, fmt.Errorf("scan processing state: %w", err)
		}
		st.Verdict, st.Resolution, st.Ref, st.ProcessedBy = verdict.String, resolution.String, ref.String, processedBy.String
		out[st.ID] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read processing states: %w", err)
	}

	return out, nil
}

func idArgs(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	return args
}

// Mark is a processing mark. A nil field keeps the stored value; a non-nil
// field replaces it, and an empty string clears it.
type Mark struct {
	Verdict     *string
	Resolution  *string
	Ref         *string
	ProcessedBy *string
}

// MarkProcessed marks every id in one statement: processed_at is set only
// when unset, the mark's fields apply as Mark documents. It returns how many
// rows the statement touched; a re-mark with identical values still counts,
// so classification is the caller's job on top of ProcessingStates.
func MarkProcessed(ctx context.Context, q Querier, ids []int64, now int64, m Mark) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := []any{now}
	set := []string{"processed_at = COALESCE(processed_at, ?)"}
	for _, c := range []struct {
		column string
		value  *string
	}{{"verdict", m.Verdict}, {"resolution", m.Resolution}, {"ref", m.Ref}, {"processed_by", m.ProcessedBy}} {
		if c.value == nil {
			continue
		}
		set = append(set, c.column+" = ?")
		args = append(args, nullText(*c.value))
	}
	res, err := q.ExecContext(ctx, `UPDATE submissions SET `+strings.Join(set, ", ")+
		` WHERE id IN (`+placeholders(len(ids))+`)`, append(args, idArgs(ids)...)...)
	if err != nil {
		return 0, fmt.Errorf("mark submissions processed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mark submissions processed: %w", err)
	}

	return n, nil
}

// UnmarkProcessed clears the timestamp and every processing field of the
// given ids in one statement and returns how many rows it touched.
func UnmarkProcessed(ctx context.Context, q Querier, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := q.ExecContext(ctx, `UPDATE submissions SET processed_at = NULL, verdict = NULL, resolution = NULL,
		ref = NULL, processed_by = NULL WHERE id IN (`+placeholders(len(ids))+`)`, idArgs(ids)...)
	if err != nil {
		return 0, fmt.Errorf("unmark submissions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("unmark submissions: %w", err)
	}

	return n, nil
}

// Redact turns the row into its tombstone: payload becomes Tombstone, summary
// and context go, redacted_at is set. Everything else, the hash included,
// stays. It reports whether the row changed: false when it was already a
// tombstone or does not exist; the caller tells those apart with GetByID.
func Redact(ctx context.Context, q Querier, id int64, now int64) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE submissions SET payload = ?, summary = NULL, context = NULL, redacted_at = ?
		WHERE id = ? AND redacted_at IS NULL`, Tombstone, now, id)
	if err != nil {
		return false, fmt.Errorf("redact submission %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("redact submission %d: %w", id, err)
	}

	return n > 0, nil
}

// ExportFilter selects the rows of an export: an optional kind, an optional
// lower bound on created_at, an optional cursor (rows with id > AfterID) and
// an optional cap (Limit <= 0 means every row).
type ExportFilter struct {
	Kind    string
	Since   *int64
	AfterID *int64
	Limit   int
}

func iterateSQL(f ExportFilter) (string, []any) {
	w := &whereBuilder{}
	if f.Kind != "" {
		w.add("kind = ?", f.Kind)
	}
	if f.Since != nil {
		w.add("created_at >= ?", *f.Since)
	}
	if f.AfterID != nil {
		w.add("id > ?", *f.AfterID)
	}
	limit := int64(-1) // SQLite: no limit
	if f.Limit > 0 {
		limit = int64(f.Limit)
	}

	return `SELECT ` + columns + ` FROM submissions` + w.sql() + ` ORDER BY id ASC LIMIT ?`, append(w.args, limit)
}

// Iterate streams every row matching f in ascending id order, full records
// including tombstones, and stops at the first error fn returns.
func Iterate(ctx context.Context, q Querier, f ExportFilter, fn func(Submission) error) error {
	query, args := iterateSQL(f)
	rows, err := q.QueryContext(ctx, query, args...)
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

// CountAll reports how many rows the table holds (the restore's emptiness check).
func CountAll(ctx context.Context, q Querier) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM submissions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count submissions: %w", err)
	}

	return n, nil
}

// SetSequence pushes sqlite_sequence for submissions to at least value, so ids
// restored from another database are never handed out again.
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
