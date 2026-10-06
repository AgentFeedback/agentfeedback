package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/localmode"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// requestIDRe matches a request id value and exportedAtRe the export
// header's generation time; both differ per request by design.
var (
	requestIDRe  = regexp.MustCompile(`"request_id":"[^"]*"`)
	exportedAtRe = regexp.MustCompile(`"exported_at":"[^"]*"`)
)

func normaliseRequestIDs(s string) string {
	s = requestIDRe.ReplaceAllString(s, `"request_id":"X"`)

	return exportedAtRe.ReplaceAllString(s, `"exported_at":"X"`)
}

// localFixture submits two frictions and marks the first done in local mode,
// and returns the data-directory database path.
func localFixture(t *testing.T) string {
	t.Helper()
	for _, summary := range []string{"eq one", "eq two"} {
		if r := runCLI(t, "", "submit", "friction", "--summary", summary, "--category", "test"); r.code != 0 {
			t.Fatalf("submit %q: %+v", summary, r)
		}
	}
	if r := runCLI(t, "", "done", "1", "--verdict", "v", "--resolution", "r"); r.code != 0 {
		t.Fatalf("done: %+v", r)
	}
	path, err := localDBPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// TestLocalMode_EquivalentToServer: every read command prints the same bytes
// in local mode as against a server over the same database file, request
// ids aside.
func TestLocalMode_EquivalentToServer(t *testing.T) {
	isolate(t)
	path := localFixture(t)

	db := openDB(t, path)
	svc := core.New(db, core.Config{Version: clientVersion().Version, Features: api.Features})
	srv := httptest.NewServer(api.New(api.Config{Service: svc, DB: db, APIKey: testKey}).Handler())
	t.Cleanup(srv.Close)

	for _, args := range [][]string{
		{"list", "--json"},
		{"list", "--json", "--processed"},
		{"get", "1", "--json"},
		{"get", "2", "--json"},
		{"stats", "--json"},
		{"export"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv(envURL, "")
			t.Setenv(envAPIKey, "")
			loc := runCLI(t, "", args...)
			t.Setenv(envURL, srv.URL)
			t.Setenv(envAPIKey, testKey)
			rem := runCLI(t, "", args...)
			if loc.code != 0 || rem.code != 0 {
				t.Fatalf("local %+v\nremote %+v", loc, rem)
			}
			if a, b := normaliseRequestIDs(loc.stdout), normaliseRequestIDs(rem.stdout); a != b {
				t.Fatalf("output differs\nlocal:  %s\nremote: %s", a, b)
			}
		})
	}
}

// TestLocalMode_NoOperatorEndpoints: the CLI's local client serves no
// /health, /metrics or /mcp.
func TestLocalMode_NoOperatorEndpoints(t *testing.T) {
	isolate(t)
	m, err := resolveMode(modeFlags{}, os.Getenv)
	if err != nil || m.Mode != modeLocal {
		t.Fatalf("mode %+v %v", m, err)
	}
	c, err := openLocalClient(m, os.Getenv, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeLocal)
	for _, p := range []string{"/health", "/metrics", "/mcp"} {
		_, err := c.Do(context.Background(), http.MethodGet, p, nil, nil)
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

// TestLocalMode_OperatorCommands: backup, export, import --dry-run and
// doctor --e2e work without a URL and without DATABASE_PATH.
func TestLocalMode_OperatorCommands(t *testing.T) {
	isolate(t)
	isolateServerEnv(t)
	path := localFixture(t)

	dest := filepath.Join(t.TempDir(), "copy.db")
	if r := runCLI(t, "", "backup", dest); r.code != 0 {
		t.Fatalf("backup: %+v", r)
	}
	openDB(t, dest) // the copy opens as a stamped database
	if info, err := os.Stat(dest); err != nil || info.Size() == 0 {
		t.Fatalf("backup copy: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	exp := runCLI(t, "", "export")
	if exp.code != 0 || exp.stdout == "" {
		t.Fatalf("export: %+v", exp)
	}
	file := filepath.Join(t.TempDir(), "export.ndjson")
	writeFile(t, file, exp.stdout, 0o600)
	imp := runCLI(t, "", "import", "--dry-run", file)
	// The dry run reads the data-directory database: both records are
	// already there, so both count as skipped.
	if imp.code != 0 || !strings.Contains(imp.stdout, `"imported":0,"skipped":2,`) || !strings.Contains(imp.stdout, `"dry_run":true`) {
		t.Fatalf("import --dry-run: %+v", imp)
	}

	if r := runCLI(t, "", "doctor", "--e2e"); r.code != 0 {
		t.Fatalf("doctor --e2e: %+v", r)
	}
}

// TestLocalMode_WriteFailureSpools: a local write the database cannot take
// (here a read-only file; a lock held past the busy timeout spools the same
// way) is spooled with destination local, outcome spooled and exit 0, with
// nothing echoed; the next command's start-up pass delivers it before its
// own work, so list shows the row.
func TestLocalMode_WriteFailureSpools(t *testing.T) {
	if goos == "windows" {
		t.Skip("a read-only database file is not reproducible on Windows")
	}
	isolate(t)
	path := localFixture(t)
	chmodAll(t, path, 0o400)
	r := runCLI(t, "", "submit", "friction", "--summary", "while read-only")
	o := outcomeOf(t, r)
	if r.code != 0 || o["outcome"] != "spooled" || strings.Contains(r.stderr, "NOT persisted") {
		t.Fatalf("read-only write: %+v", r)
	}
	data := dataRoot(t)
	files := spoolFiles(t, client.SpoolDir(data))
	if len(files) != 1 {
		t.Fatalf("spool holds %v", files)
	}
	raw, err := os.ReadFile(filepath.Join(client.SpoolDir(data), files[0]))
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Destination string `json:"destination"`
	}
	if json.Unmarshal(raw, &entry) != nil || entry.Destination != client.LocalDestination {
		t.Fatalf("entry %s", raw)
	}
	if r := runCLI(t, "", "list", "--json"); !strings.Contains(r.stdout, `"total":2`) {
		t.Fatalf("list while read-only delivered something: %+v", r)
	}

	chmodAll(t, path, 0o600)
	r = runCLI(t, "", "list", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"total":3`) || !strings.Contains(r.stdout, "while read-only") {
		t.Fatalf("list after the pass: %+v", r)
	}
	if left := spoolFiles(t, client.SpoolDir(data)); len(left) != 0 {
		t.Fatalf("spool left %v", left)
	}
	if r := runCLI(t, "", "doctor", "--json"); !strings.Contains(r.stdout, `"outcome":"flushed"`) {
		t.Fatalf("no flushed line in the client log: %+v", r)
	}
}

// TestLocalMode_UnwritableSpoolEchoes: when the write fails and the spool
// cannot be written either, the body is echoed on stderr for recovery and
// the command exits 1 (outcome error).
func TestLocalMode_UnwritableSpoolEchoes(t *testing.T) {
	if goos == "windows" {
		t.Skip("a read-only database file is not reproducible on Windows")
	}
	isolate(t)
	chmodAll(t, localFixture(t), 0o400)
	writeFile(t, client.SpoolDir(dataRoot(t)), "not a directory", 0o600)
	r := runCLI(t, "", "submit", "friction", "--summary", "nowhere to go")
	o := outcomeOf(t, r)
	if r.code != 1 || o["outcome"] != "error" || o["reason"] != "spool_unwritable" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.stderr, "NOT persisted") || !strings.Contains(r.stderr, `"summary":"nowhere to go"`) {
		t.Fatalf("body not echoed: %s", r.stderr)
	}
}

// chmodAll sets mode on the database file and the -wal and -shm files that
// exist: SQLite gives new sidecars the database file's mode, so a read-only
// database left alone would keep read-only sidecars after the file is
// restored.
func chmodAll(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	for _, p := range localmode.Sidecars(path) {
		if err := os.Chmod(p, mode); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}
