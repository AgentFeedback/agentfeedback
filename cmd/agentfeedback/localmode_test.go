package main

import (
	"context"
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
