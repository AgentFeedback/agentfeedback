package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/internal/core"
	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// v1Records are in the shape a legacy export-format-1 producer emits: family
// derived from submission_type, resolution always null, and — for rows written
// before hashing existed — payload_hash null.
var v1Records = []string{
	`{"id":1,"family":"friction","submission_type":"friction","machine_name":"workstation-a",` +
		`"coordinator_model":"claude-fable-5-1","run_id":null,` +
		`"payload":{"category":"documentation","summary":"docs drifted","context":{"cwd":"/tmp"}},` +
		`"payload_hash":null,"created_at":"2026-07-30T10:00:00.000000Z","processed_at":null,"resolution":null}`,
	`{"id":2,"family":"review","submission_type":"review-panel","machine_name":"workstation-a",` +
		`"coordinator_model":"claude-fable-5-1","run_id":"run-1",` +
		`"payload":{"prompt":"review this","reviewers":[{"slot":"a","model":"m","status":"completed"}]},` +
		`"payload_hash":"deadbeef","created_at":"2026-07-30T11:00:00.123456Z",` +
		`"processed_at":"2026-07-31T09:30:00Z","resolution":null}`,
	`{"id":7,"family":"friction","submission_type":"friction","machine_name":"workstation-b",` +
		`"coordinator_model":"claude-fable-5-1",` +
		`"payload":{"category":"tooling","summary":"tool hung"},` +
		`"created_at":"2026-08-01T12:00:00+02:00"}`,
}

func writeExportFile(t *testing.T, dir string, header string, records []string, withTerminator bool, digest string) string {
	t.Helper()

	var buf bytes.Buffer
	buf.WriteString(header + "\n")
	var body bytes.Buffer
	for _, r := range records {
		body.WriteString(r + "\n")
	}
	buf.Write(body.Bytes())
	if withTerminator {
		if digest == "" {
			sum := sha256.Sum256(body.Bytes())
			digest = hex.EncodeToString(sum[:])
		}
		fmt.Fprintf(&buf, `{"export_complete":true,"count":%d,"sha256":%q}`+"\n", len(records), digest)
	}

	path := filepath.Join(dir, "export.jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write export file: %v", err)
	}

	return path
}

const v1Header = `{"export_format":1,"family":null,"since":null,"exported_at":"2026-08-02T00:00:00.000000Z","source":"legacy-export"}`

func openDB(t *testing.T, path string) *store.DB {
	t.Helper()

	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func TestImport_V1Export(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentfeedback.db")
	t.Setenv("DATABASE_PATH", dbPath)

	// The fixture's review row carries a placeholder hash, as a hand-built v1
	// dump may: --trust-hashes keeps it instead of failing the comparison.
	path := writeExportFile(t, dir, v1Header, v1Records, true, "")
	if err := runImport([]string{"--trust-hashes", path}); err != nil {
		t.Fatalf("import: %v", err)
	}

	ctx := context.Background()
	db := openDB(t, dbPath)
	svc := core.New(db)

	for _, want := range []struct {
		id     int64
		family string
	}{{1, "friction"}, {2, "review"}, {7, "friction"}} {
		got, err := svc.GetSubmission(ctx, want.id)
		if err != nil {
			t.Fatalf("get %d: %v", want.id, err)
		}
		if got.Family != want.family {
			t.Fatalf("submission %d: family %q, want %q", want.id, got.Family, want.family)
		}
		if got.PayloadHash == "" {
			t.Fatalf("submission %d: payload_hash must never be empty", want.id)
		}
	}

	// The row exported without a hash gets the one its family's rule produces,
	// so it still dedupes against a new submission of the same friction.
	row1, err := svc.GetSubmission(ctx, 1)
	if err != nil {
		t.Fatalf("get 1: %v", err)
	}
	wantHash, err := core.HashForFamily(store.FamilyFriction, "workstation-a", "claude-fable-5-1", row1.Payload)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if row1.PayloadHash != wantHash {
		t.Fatalf("recomputed hash %s, want %s", row1.PayloadHash, wantHash)
	}
	// The recomputed hash is the one a fresh submission of the same friction
	// produces, so the imported row still participates in duplicate detection
	// (this fixture predates the 24 h window, so the new row is a recurrence).
	resubmitted, _, err := svc.CreateFriction(ctx, core.CreateFrictionInput{
		MachineName: "workstation-a", CoordinatorModel: "claude-fable-5-1",
		Category: "documentation", Summary: "docs drifted",
		Context: map[string]string{"cwd": "/elsewhere"},
	})
	if err != nil {
		t.Fatalf("create friction: %v", err)
	}
	if resubmitted.PayloadHash != row1.PayloadHash {
		t.Fatalf("re-submitted friction hashed to %s, imported row has %s", resubmitted.PayloadHash, row1.PayloadHash)
	}

	// An explicit hash is preserved verbatim.
	row2, err := svc.GetSubmission(ctx, 2)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if row2.PayloadHash != "deadbeef" {
		t.Fatalf("stored hash %q, want deadbeef", row2.PayloadHash)
	}
	if row2.ProcessedAt == nil || core.FormatMicros(*row2.ProcessedAt) != "2026-07-31T09:30:00.000000Z" {
		t.Fatalf("processed_at was not preserved: %+v", row2.ProcessedAt)
	}
	if row2.Resolution != nil {
		t.Fatalf("null resolution must stay unset, got %q", *row2.Resolution)
	}

	// A non-UTC offset is normalized.
	row7, err := svc.GetSubmission(ctx, 7)
	if err != nil {
		t.Fatalf("get 7: %v", err)
	}
	if core.FormatMicros(row7.CreatedAt) != "2026-08-01T10:00:00.000000Z" {
		t.Fatalf("created_at not normalized to UTC: %s", core.FormatMicros(row7.CreatedAt))
	}
	if row7.RunID != nil {
		t.Fatal("an absent run_id must stay unset")
	}

	// ids are never reused: the sequence continues past the highest import.
	fresh, _, err := svc.CreateFriction(ctx, core.CreateFrictionInput{
		MachineName: "workstation-a", CoordinatorModel: "m", Category: "tooling", Summary: "brand new",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if fresh.ID <= 7 {
		t.Fatalf("next id %d must be greater than the highest imported id", fresh.ID)
	}
}

func TestImport_ReservesIDsAndRefusesNonEmpty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentfeedback.db")
	t.Setenv("DATABASE_PATH", dbPath)

	path := writeExportFile(t, dir, v1Header, v1Records, true, "")
	if err := runImport([]string{"--trust-hashes", "--reserve-ids-through", "500", path}); err != nil {
		t.Fatalf("import: %v", err)
	}

	svc := core.New(openDB(t, dbPath))
	fresh, _, err := svc.CreateFriction(context.Background(), core.CreateFrictionInput{
		MachineName: "m", CoordinatorModel: "c", Category: "tooling", Summary: "new",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if fresh.ID != 501 {
		t.Fatalf("reserved range ignored: next id is %d, want 501", fresh.ID)
	}

	// A second import into the now non-empty database is refused.
	err = runImport([]string{"--trust-hashes", path})
	if err == nil || !strings.Contains(err.Error(), "allow-nonempty") {
		t.Fatalf("expected a refusal naming --allow-nonempty, got %v", err)
	}
}

func TestImport_RejectsBrokenStreams(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		records    []string
		terminator bool
		digest     string
		wantErr    string
	}{
		{
			name: "truncated stream", header: v1Header, records: v1Records,
			terminator: false, wantErr: "truncated",
		},
		{
			name: "wrong digest", header: v1Header, records: v1Records,
			terminator: true, digest: strings.Repeat("0", 64), wantErr: "digest mismatch",
		},
		{
			name:    "unsupported format",
			header:  `{"export_format":2}`,
			records: v1Records, terminator: true, wantErr: "unsupported export_format",
		},
		{
			name:    "filtered export",
			header:  `{"export_format":1,"family":"friction","since":null,"exported_at":"2026-08-02T00:00:00.000000Z"}`,
			records: v1Records[:1], terminator: true, wantErr: "filtered export",
		},
		{
			name: "unknown family", header: v1Header,
			records: []string{`{"id":1,"family":"nope","submission_type":"x","machine_name":"m",` +
				`"coordinator_model":"c","payload":{},"created_at":"2026-08-01T12:00:00Z"}`},
			terminator: true, wantErr: "family must be",
		},
		{
			name: "payload not an object", header: v1Header,
			records: []string{`{"id":1,"family":"event","submission_type":"deploy","machine_name":"m",` +
				`"coordinator_model":"c","run_id":"k1","payload":[1],"created_at":"2026-08-01T12:00:00Z"}`},
			terminator: true, wantErr: "payload must be a JSON object",
		},
		{
			name: "missing machine_name", header: v1Header,
			records: []string{`{"id":1,"family":"event","submission_type":"deploy","machine_name":"",` +
				`"coordinator_model":"c","payload":{},"created_at":"2026-08-01T12:00:00Z"}`},
			terminator: true, wantErr: "machine_name is required",
		},
		{
			name: "bad timestamp", header: v1Header,
			records: []string{`{"id":1,"family":"event","submission_type":"deploy","machine_name":"m",` +
				`"coordinator_model":"c","run_id":"k1","payload":{},"created_at":"yesterday"}`},
			terminator: true, wantErr: "created_at",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "agentfeedback.db")
			t.Setenv("DATABASE_PATH", dbPath)

			path := writeExportFile(t, dir, tc.header, tc.records, tc.terminator, tc.digest)
			err := runImport([]string{"--trust-hashes", path})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected an error containing %q, got %v", tc.wantErr, err)
			}

			// Nothing may be committed when the stream is rejected.
			if _, statErr := os.Stat(dbPath); statErr == nil {
				db := openDB(t, dbPath)
				var n int64
				if err := db.Read(context.Background(), func(q store.Querier) error {
					var err error
					n, err = store.CountAll(context.Background(), q)

					return err
				}); err != nil {
					t.Fatalf("count: %v", err)
				}
				if n != 0 {
					t.Fatalf("a rejected import must leave the database empty, found %d row(s)", n)
				}
			}
		})
	}
}

// TestImport_RoundTripFromExport is the migration path end to end: export a
// populated database, import it into an empty one, compare every record.
func TestImport_RoundTripFromExport(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")

	ctx := context.Background()
	source := core.New(openDB(t, sourcePath))

	friction, _, err := source.CreateFriction(ctx, core.CreateFrictionInput{
		MachineName: "workstation-a", CoordinatorModel: "claude-fable-5",
		Category: "documentation", Summary: "docs drifted",
		Context: map[string]string{"cwd": "/tmp"},
	})
	if err != nil {
		t.Fatalf("create friction: %v", err)
	}
	if _, _, err := source.CreateReview(ctx, core.CreateReviewInput{
		Skill: "review-panel", MachineName: "workstation-a", CoordinatorModel: "claude-fable-5",
		RunID: "run-1", Prompt: "review this",
		Reviewers: []core.ReviewerInput{{Slot: "a", Model: "m", Status: "completed"}},
	}); err != nil {
		t.Fatalf("create review: %v", err)
	}
	if _, _, err := source.CreateEvent(ctx, core.CreateEventInput{
		Kind: "deploy", Key: "k1", MachineName: "workstation-a", CoordinatorModel: "claude-fable-5",
		Payload: []byte(`{"b":2,"a":1,"n":1.50}`),
	}); err != nil {
		t.Fatalf("create event: %v", err)
	}
	if _, err := source.SetProcessed(ctx, core.SetProcessedInput{
		IDs: []int64{friction.ID}, Processed: true, Resolution: strPtr("fixed in example@1a2b3c4"),
	}); err != nil {
		t.Fatalf("mark processed: %v", err)
	}

	var exported bytes.Buffer
	if err := source.Export(ctx, core.ExportInput{}, &exported, nil); err != nil {
		t.Fatalf("export: %v", err)
	}
	exportPath := filepath.Join(dir, "export.jsonl")
	if err := os.WriteFile(exportPath, exported.Bytes(), 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}

	targetPath := filepath.Join(dir, "target.db")
	t.Setenv("DATABASE_PATH", targetPath)
	if err := runImport([]string{exportPath}); err != nil {
		t.Fatalf("import: %v", err)
	}

	target := core.New(openDB(t, targetPath))
	var reExported bytes.Buffer
	if err := target.Export(ctx, core.ExportInput{}, &reExported, nil); err != nil {
		t.Fatalf("re-export: %v", err)
	}

	wantRecords := recordLines(t, exported.String())
	gotRecords := recordLines(t, reExported.String())
	if len(wantRecords) != 3 || len(gotRecords) != len(wantRecords) {
		t.Fatalf("record counts differ: %d vs %d", len(wantRecords), len(gotRecords))
	}
	for i := range wantRecords {
		if wantRecords[i] != gotRecords[i] {
			t.Fatalf("record %d changed across the round trip:\n before: %s\n after:  %s", i, wantRecords[i], gotRecords[i])
		}
	}

	// The importer advanced the sequence, so a new row cannot collide.
	fresh, _, err := target.CreateFriction(ctx, core.CreateFrictionInput{
		MachineName: "m", CoordinatorModel: "c", Category: "tooling", Summary: "new",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if fresh.ID != 4 {
		t.Fatalf("next id %d, want 4", fresh.ID)
	}
}

func recordLines(t *testing.T, stream string) []string {
	t.Helper()

	lines := strings.Split(strings.TrimRight(stream, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("stream is too short: %q", stream)
	}
	records := lines[1 : len(lines)-1]
	for _, line := range records {
		var rec core.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode record: %v", err)
		}
	}

	return records
}

func strPtr(s string) *string { return &s }

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what it
// printed — the import summary is part of the command's contract.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	runErr := fn()

	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}

	return string(out), runErr
}

type importSummary struct {
	Imported int64            `json:"imported"`
	Skipped  int64            `json:"skipped"`
	MaxID    int64            `json:"max_id"`
	Sequence int64            `json:"sequence"`
	ByFamily map[string]int64 `json:"by_family"`
}

func countRows(t *testing.T, db *store.DB) int64 {
	t.Helper()

	var n int64
	if err := db.Read(context.Background(), func(q store.Querier) error {
		var err error
		n, err = store.CountAll(context.Background(), q)

		return err
	}); err != nil {
		t.Fatalf("count: %v", err)
	}

	return n
}

// eventRecord builds an event line whose payload is whatever raw JSON object
// the caller passes.
func eventRecord(id int64, key, payload string) string {
	return fmt.Sprintf(`{"id":%d,"family":"event","submission_type":"deploy","machine_name":"m",`+
		`"coordinator_model":"c","run_id":%q,"payload":%s,"created_at":"2026-08-01T12:00:00Z"}`,
		id, key, payload)
}

func frictionRecord(id int64, summary string) string {
	return fmt.Sprintf(`{"id":%d,"family":"friction","submission_type":"friction","machine_name":"m",`+
		`"coordinator_model":"c","payload":{"category":"tooling","summary":%q},`+
		`"created_at":"2026-08-01T12:00:00Z"}`, id, summary)
}

// A record is recognized by its top-level keys, never by a substring search:
// a payload that merely contains export_complete / export_format is data.
func TestImport_RecordWhosePayloadLooksLikeAHeaderOrTerminator(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentfeedback.db")
	t.Setenv("DATABASE_PATH", dbPath)

	records := []string{eventRecord(1, "k1", `{"export_complete":true,"export_format":1}`)}
	path := writeExportFile(t, dir, v1Header, records, true, "")
	if err := runImport([]string{path}); err != nil {
		t.Fatalf("import: %v", err)
	}

	db := openDB(t, dbPath)
	if n := countRows(t, db); n != 1 {
		t.Fatalf("imported %d row(s), want 1", n)
	}
	got, err := core.New(db).GetSubmission(context.Background(), 1)
	if err != nil {
		t.Fatalf("get 1: %v", err)
	}
	if string(got.Payload) != `{"export_complete":true,"export_format":1}` {
		t.Fatalf("payload changed: %s", got.Payload)
	}
}

func TestImport_TerminatorWithoutSHA256IsRejected(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentfeedback.db")
	t.Setenv("DATABASE_PATH", dbPath)

	for _, terminator := range []string{
		`{"export_complete":true,"count":1}`,
		`{"export_complete":true,"count":1,"sha256":""}`,
		`{"export_complete":true,"count":1,"sha256":null}`,
	} {
		body := frictionRecord(1, "no digest") + "\n"
		path := filepath.Join(dir, "export.jsonl")
		if err := os.WriteFile(path, []byte(v1Header+"\n"+body+terminator+"\n"), 0o600); err != nil {
			t.Fatalf("write export: %v", err)
		}
		err := runImport([]string{path})
		if err == nil || !strings.Contains(err.Error(), "no sha256") {
			t.Fatalf("terminator %s: expected a refusal naming sha256, got %v", terminator, err)
		}
	}

	// A terminator without a count is equally unverifiable.
	body := frictionRecord(1, "no count") + "\n"
	path := filepath.Join(dir, "export.jsonl")
	if err := os.WriteFile(path, []byte(v1Header+"\n"+body+`{"export_complete":true,"sha256":"x"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}
	if err := runImport([]string{path}); err == nil || !strings.Contains(err.Error(), "no count") {
		t.Fatalf("expected a refusal naming count, got %v", err)
	}

	if _, err := os.Stat(dbPath); err == nil {
		if n := countRows(t, openDB(t, dbPath)); n != 0 {
			t.Fatalf("a rejected stream must leave the database empty, found %d row(s)", n)
		}
	}
}

func TestImport_DeclaredHashMismatch(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentfeedback.db")
	t.Setenv("DATABASE_PATH", dbPath)

	records := []string{
		`{"id":3,"family":"friction","submission_type":"friction","machine_name":"m",` +
			`"coordinator_model":"c","payload":{"category":"tooling","summary":"tampered"},` +
			`"payload_hash":"` + strings.Repeat("a", 64) + `","created_at":"2026-08-01T12:00:00Z"}`,
	}
	path := writeExportFile(t, dir, v1Header, records, true, "")

	err := runImport([]string{path})
	if err == nil || !strings.Contains(err.Error(), "does not match the hash recomputed") ||
		!strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), strings.Repeat("a", 64)) {
		t.Fatalf("expected a mismatch error naming the line and both hashes, got %v", err)
	}
	if _, statErr := os.Stat(dbPath); statErr == nil {
		if n := countRows(t, openDB(t, dbPath)); n != 0 {
			t.Fatalf("a rejected stream must leave the database empty, found %d row(s)", n)
		}
	}

	// --trust-hashes keeps the declared hash instead.
	if err := runImport([]string{"--trust-hashes", path}); err != nil {
		t.Fatalf("import with --trust-hashes: %v", err)
	}
	got, err := core.New(openDB(t, dbPath)).GetSubmission(context.Background(), 3)
	if err != nil {
		t.Fatalf("get 3: %v", err)
	}
	if got.PayloadHash != strings.Repeat("a", 64) {
		t.Fatalf("stored hash %q, want the declared one", got.PayloadHash)
	}
}

func TestImport_KeyedRecordWithoutRunIDIsRejected(t *testing.T) {
	for _, family := range []string{"review", "event"} {
		t.Run(family, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "agentfeedback.db")
			t.Setenv("DATABASE_PATH", dbPath)

			records := []string{fmt.Sprintf(
				`{"id":1,"family":%q,"submission_type":"t","machine_name":"m","coordinator_model":"c",`+
					`"run_id":null,"payload":{"prompt":"p","reviewers":[]},"created_at":"2026-08-01T12:00:00Z"}`,
				family)}
			path := writeExportFile(t, dir, v1Header, records, true, "")

			err := runImport([]string{path})
			if err == nil || !strings.Contains(err.Error(), "run_id is required") ||
				!strings.Contains(err.Error(), "line 2") {
				t.Fatalf("expected a refusal naming the line and run_id, got %v", err)
			}
		})
	}
}

func TestImport_FamilyFilter(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentfeedback.db")
	t.Setenv("DATABASE_PATH", dbPath)

	records := []string{
		frictionRecord(1, "first friction"),
		`{"id":2,"family":"review","submission_type":"review-panel","machine_name":"m",` +
			`"coordinator_model":"c","run_id":"run-1","payload":{"prompt":"p","reviewers":[]},` +
			`"created_at":"2026-08-01T12:00:00Z"}`,
		eventRecord(3, "k1", `{"a":1}`),
		frictionRecord(4, "second friction"),
		// The highest id in the stream belongs to a row the filter skips.
		`{"id":9,"family":"review","submission_type":"review-panel","machine_name":"m",` +
			`"coordinator_model":"c","run_id":"run-2","payload":{"prompt":"p","reviewers":[]},` +
			`"created_at":"2026-08-01T12:00:00Z"}`,
	}
	path := writeExportFile(t, dir, v1Header, records, true, "")

	out, err := captureStdout(t, func() error { return runImport([]string{"--family", "friction", path}) })
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	summary := decodeSummary(t, out)
	if summary.Imported != 2 || summary.Skipped != 3 {
		t.Fatalf("summary %+v, want imported 2 / skipped 3", summary)
	}
	// max_id is the highest imported id; the sequence follows the highest id in
	// the whole stream, so a skipped row's id is never handed out again.
	if summary.MaxID != 4 || summary.Sequence != 9 {
		t.Fatalf("summary %+v, want max_id 4 and sequence 9", summary)
	}
	if summary.ByFamily["friction"] != 2 || len(summary.ByFamily) != 1 {
		t.Fatalf("by_family %+v, want only frictions", summary.ByFamily)
	}

	db := openDB(t, dbPath)
	if n := countRows(t, db); n != 2 {
		t.Fatalf("database holds %d row(s), want 2", n)
	}
	svc := core.New(db)
	for _, id := range []int64{1, 4} {
		if _, err := svc.GetSubmission(context.Background(), id); err != nil {
			t.Fatalf("friction %d must be imported: %v", id, err)
		}
	}
	for _, id := range []int64{2, 3, 9} {
		if _, err := svc.GetSubmission(context.Background(), id); err == nil {
			t.Fatalf("record %d must have been skipped", id)
		}
	}

	// A new row cannot collide with a skipped row's id.
	fresh, _, err := svc.CreateFriction(context.Background(), core.CreateFrictionInput{
		MachineName: "m", CoordinatorModel: "c", Category: "tooling", Summary: "brand new",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if fresh.ID != 10 {
		t.Fatalf("next id %d, want 10", fresh.ID)
	}

	// The digest still covers the whole stream, skipped records included: a
	// filtered import of a tampered stream is refused.
	dir2 := t.TempDir()
	t.Setenv("DATABASE_PATH", filepath.Join(dir2, "agentfeedback.db"))
	tampered := writeExportFile(t, dir2, v1Header, records, true, strings.Repeat("0", 64))
	if err := runImport([]string{"--family", "friction", tampered}); err == nil ||
		!strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected a digest mismatch, got %v", err)
	}
}

func decodeSummary(t *testing.T, out string) importSummary {
	t.Helper()

	line := strings.TrimSpace(out)
	var summary importSummary
	if err := json.Unmarshal([]byte(line), &summary); err != nil {
		t.Fatalf("decode summary %q: %v", line, err)
	}

	return summary
}

// A stream that goes bad after thousands of good records must still leave the
// database empty: the importer inserts as it reads, inside one transaction.
func TestImport_RollbackOnCorruptLineLateInStream(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentfeedback.db")
	t.Setenv("DATABASE_PATH", dbPath)

	records := make([]string, 0, 5000)
	for i := int64(1); i <= 5000; i++ {
		records = append(records, frictionRecord(i, fmt.Sprintf("friction %d", i)))
	}
	records = append(records, `{"id":5001,"family":"friction","submission_type":"friction",`+
		`"machine_name":"m","coordinator_model":"c","payload":{"category":"tooling","summary":"bad"},`+
		`"created_at":"not a timestamp"}`)
	path := writeExportFile(t, dir, v1Header, records, true, "")

	err := runImport([]string{path})
	if err == nil || !strings.Contains(err.Error(), "created_at") || !strings.Contains(err.Error(), "line 5002") {
		t.Fatalf("expected the failure to name line 5002, got %v", err)
	}
	if n := countRows(t, openDB(t, dbPath)); n != 0 {
		t.Fatalf("a failed import must roll back completely, found %d row(s)", n)
	}
}

// A 1.x export can carry whitespace-padded identifiers. They must be trimmed
// before validation, storage and hash recomputation, exactly as the create path
// trims them — otherwise the imported row sits under a key no later submission
// can match, and replaying the same review would insert a second row.
func TestImport_TrimsIdentifiers(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentfeedback.db")
	t.Setenv("DATABASE_PATH", dbPath)

	records := []string{
		`{"id":11,"family":"review","submission_type":"  review-panel  ",` +
			`"machine_name":" workstation-a ","coordinator_model":" claude-fable-5-1 ",` +
			`"run_id":"  run-9  ",` +
			`"payload":{"prompt":"p","reviewers":[{"slot":"a","model":"m","status":"completed"}]},` +
			`"created_at":"2026-07-30T10:00:00.000000Z"}`,
	}
	path := writeExportFile(t, dir, v1Header, records, true, "")
	if err := runImport([]string{path}); err != nil {
		t.Fatalf("import: %v", err)
	}

	ctx := context.Background()
	db := openDB(t, dbPath)
	svc := core.New(db)

	row, err := svc.GetSubmission(ctx, 11)
	if err != nil {
		t.Fatalf("get 11: %v", err)
	}
	if row.SubmissionType != "review-panel" {
		t.Fatalf("submission_type %q, want %q", row.SubmissionType, "review-panel")
	}
	if row.MachineName != "workstation-a" {
		t.Fatalf("machine_name %q, want %q", row.MachineName, "workstation-a")
	}
	if row.CoordinatorModel != "claude-fable-5-1" {
		t.Fatalf("coordinator_model %q, want %q", row.CoordinatorModel, "claude-fable-5-1")
	}
	if row.RunID == nil || *row.RunID != "run-9" {
		t.Fatalf("run_id %v, want %q", row.RunID, "run-9")
	}

	// The hash is the one the trimmed values produce, so the imported row is
	// the row a replay of the same review finds.
	wantHash, err := core.HashForFamily(store.FamilyReview, "workstation-a", "claude-fable-5-1", row.Payload)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if row.PayloadHash != wantHash {
		t.Fatalf("payload_hash %s, want %s", row.PayloadHash, wantHash)
	}

	// The same review submitted through the API replays onto the imported row
	// instead of inserting a second one.
	got, replayed, err := svc.CreateReview(ctx, core.CreateReviewInput{
		Skill:            "review-panel",
		MachineName:      "workstation-a",
		CoordinatorModel: "claude-fable-5-1",
		RunID:            "run-9",
		Prompt:           "p",
		Reviewers:        []core.ReviewerInput{{Slot: "a", Model: "m", Status: "completed"}},
	})
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	if !replayed {
		t.Fatal("an identical review must replay onto the imported row, not insert a new one")
	}
	if got.ID != 11 {
		t.Fatalf("replay returned id %d, want the imported row 11", got.ID)
	}
}
