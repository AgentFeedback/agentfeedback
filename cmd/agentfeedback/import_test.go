package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/internal/api"
	"github.com/agentfeedback/agentfeedback/internal/core"
	"github.com/agentfeedback/agentfeedback/internal/store"
)

func openDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func newService(db *store.DB) *core.Service {
	return core.New(db, core.Config{Version: "4.0.0", Features: api.Features})
}

func countRows(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Read(context.Background(), func(q store.Querier) error {
		var err error
		n, err = store.CountAll(context.Background(), q)

		return err
	}); err != nil {
		t.Fatalf("count rows: %v", err)
	}

	return n
}

// exportBody exports the whole database and returns the body without its
// header line, whose exported_at differs between runs.
func exportBody(t *testing.T, svc *core.Service) (full []byte, rest string) {
	t.Helper()
	var buf bytes.Buffer
	if err := svc.Export(context.Background(), core.ExportParams{}, &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	_, rest, _ = strings.Cut(buf.String(), "\n")

	return buf.Bytes(), rest
}

// seedExport fills a database with a keyed friction, an unknown kind, a
// processed row and a redacted row, and writes its export to a file.
func seedExport(t *testing.T, dir string) (path, records string) {
	t.Helper()
	ctx := context.Background()
	svc := newService(openDB(t, filepath.Join(dir, "a.db")))
	for _, body := range []string{
		`{"kind":"friction","key":"k1","summary":"docs drifted","payload":{"category":"documentation","n":1.50}}`,
		`{"kind":"mystery","summary":"unknown kind","payload":{"x":[1,2]}}`,
		`{"kind":"friction","summary":"processed","payload":{"category":"tooling"}}`,
		`{"kind":"friction","summary":"to redact","context":{"cwd":"/x"},"payload":{"details":"secret"}}`,
	} {
		if _, err := svc.Create(ctx, []byte(body)); err != nil {
			t.Fatalf("create %s: %v", body, err)
		}
	}
	if _, err := svc.Mark(ctx, 3, []byte(`{"verdict":"fixed","resolution":"r","ref":"abc@1","processed_by":"me"}`)); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if _, err := svc.Redact(ctx, 4); err != nil {
		t.Fatalf("redact: %v", err)
	}
	full, records := exportBody(t, svc)
	path = filepath.Join(dir, "export.ndjson")
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}

	return path, records
}

type importLine struct {
	Imported  int               `json:"imported"`
	Skipped   int               `json:"skipped"`
	Conflicts []json.RawMessage `json:"conflicts"`
	FirstID   *int64            `json:"first_id"`
	LastID    *int64            `json:"last_id"`
	DryRun    bool              `json:"dry_run"`
}

func parseImport(t *testing.T, r result) importLine {
	t.Helper()
	var out importLine
	if r.code != 0 || strings.Count(r.stdout, "\n") != 1 || json.Unmarshal([]byte(r.stdout), &out) != nil {
		t.Fatalf("import: %+v", r)
	}

	return out
}

func TestImport_RestoresAnExportByteExact(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	path, want := seedExport(t, dir)
	dbB := filepath.Join(dir, "b.db")
	t.Setenv("DATABASE_PATH", dbB)

	out := parseImport(t, runCLI(t, "", "import", path))
	if out.Imported != 4 || out.Skipped != 0 || len(out.Conflicts) != 0 || out.DryRun ||
		out.FirstID == nil || *out.FirstID != 1 || *out.LastID != 4 {
		t.Fatalf("import %+v", out)
	}
	_, got := exportBody(t, newService(openDB(t, dbB)))
	if got != want {
		t.Fatalf("restored export differs\n got %s\nwant %s", got, want)
	}

	again := parseImport(t, runCLI(t, "", "import", path))
	if again.Imported != 0 || again.Skipped != 4 || len(again.Conflicts) != 0 || again.FirstID != nil {
		t.Fatalf("re-run %+v", again)
	}
	if _, after := exportBody(t, newService(openDB(t, dbB))); after != want {
		t.Fatalf("re-run changed the database\n got %s\nwant %s", after, want)
	}
}

func TestImport_DryRunWritesNothing(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	path, _ := seedExport(t, dir)
	dbB := filepath.Join(dir, "b.db")
	t.Setenv("DATABASE_PATH", dbB)

	out := parseImport(t, runCLI(t, "", "import", "--dry-run", path))
	if out.Imported != 4 || out.Skipped != 0 || !out.DryRun || *out.FirstID != 1 || *out.LastID != 4 {
		t.Fatalf("dry run %+v", out)
	}
	if _, err := os.Stat(dbB); !os.IsNotExist(err) {
		t.Fatalf("dry run created DATABASE_PATH: %v", err)
	}

	// Against an existing database the dry run counts there and writes nothing.
	if n := countRows(t, openDB(t, dbB)); n != 0 {
		t.Fatalf("fresh database holds %d row(s)", n)
	}
	out = parseImport(t, runCLI(t, "", "import", "--dry-run", path))
	if out.Imported != 4 || !out.DryRun {
		t.Fatalf("dry run on existing %+v", out)
	}
	if n := countRows(t, openDB(t, dbB)); n != 0 {
		t.Fatalf("dry run wrote %d row(s)", n)
	}
}

func TestImport_CorruptFileWritesNothing(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	path, _ := seedExport(t, dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(raw, []byte(`"sha256":"`), []byte(`"sha256":"0`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	dbB := filepath.Join(dir, "b.db")
	t.Setenv("DATABASE_PATH", dbB)

	r := runCLI(t, "", "import", path)
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "line 6: trailer sha256") {
		t.Fatalf("corrupt import: %+v", r)
	}
	if _, err := os.Stat(dbB); !os.IsNotExist(err) {
		t.Fatalf("corrupt import created DATABASE_PATH: %v", err)
	}

	if r := runCLI(t, "", "import", filepath.Join(dir, "missing.ndjson")); r.code != 1 {
		t.Fatalf("missing file: %+v", r)
	}
}

func TestRestoreErr_OnlyVerificationIsRejected(t *testing.T) {
	for _, status := range []int{400, 413} {
		err := restoreErr("e.ndjson", &core.Problem{Status: status, Message: "bad"})
		if !strings.Contains(err.Error(), "was not imported") {
			t.Fatalf("%d: %v", status, err)
		}
	}
	err := restoreErr("e.ndjson", &core.Problem{Status: 503, Message: "busy"})
	if msg := err.Error(); strings.Contains(msg, "was not imported") || !strings.Contains(msg, "failed, nothing was written: busy") {
		t.Fatalf("503: %v", err)
	}
}
