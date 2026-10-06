package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// failingServer answers every request with 500 and counts them.
func failingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	return srv, &n
}

func TestMode_NothingConfiguredIsLocal(t *testing.T) {
	isolate(t)
	r := runCLI(t, "", "list", "--json")
	var out struct {
		Submissions []json.RawMessage `json:"submissions"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || len(out.Submissions) != 0 {
		t.Fatalf("%+v", r)
	}
	db := filepath.Join(os.Getenv("XDG_DATA_HOME"), "agentfeedback", "agentfeedback.db")
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("database: %v", err)
	}
}

func TestMode_LocalRoundTrip(t *testing.T) {
	isolate(t)
	r := runCLI(t, "", "submit", "friction", "--summary", "x")
	if r.code != 0 {
		t.Fatalf("submit %+v", r)
	}
	idf, ok := outcome(t, r)["id"].(float64)
	if !ok {
		t.Fatalf("submit outcome %+v", r)
	}
	id := strconv.FormatInt(int64(idf), 10)

	r = runCLI(t, "", "list", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"id":`+id) {
		t.Fatalf("list %+v", r)
	}
	r = runCLI(t, "", "get", id, "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"id":`+id) || !strings.Contains(r.stdout, `"summary":"x"`) {
		t.Fatalf("get %+v", r)
	}
	if r = runCLI(t, "", "done", id, "--verdict", "fixed"); r.code != 0 || !strings.Contains(r.stdout, id) {
		t.Fatalf("done %+v", r)
	}
	if r = runCLI(t, "", "undo", id); r.code != 0 || !strings.Contains(r.stdout, id) {
		t.Fatalf("undo %+v", r)
	}
	if r = runCLI(t, "", "stats", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"total":1`) {
		t.Fatalf("stats %+v", r)
	}
	if r = runCLI(t, "", "export"); r.code != 0 || !strings.Contains(r.stdout, `"id":`+id) {
		t.Fatalf("export %+v", r)
	}
}

func TestMode_LocalFlagOverridesConfiguredURL(t *testing.T) {
	cfg, _ := isolate(t)
	srv, n := failingServer(t)
	writeFile(t, cfg, "url = \""+srv.URL+"\"\napi_key = \"k\"\n", 0o600)
	if r := runCLI(t, "", "list", "--json", "--local"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if n.Load() != 0 {
		t.Fatalf("--local reached the server %d time(s)", n.Load())
	}
}

func TestMode_ServerFlagGoesRemote(t *testing.T) {
	isolate(t)
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	r := runCLI(t, "", "list", "--json", "--server", srv.URL)
	if r.code != 1 || !strings.Contains(r.stderr, "no API key is set") {
		t.Fatalf("no key %+v", r)
	}
	t.Setenv(envAPIKey, "k")
	if r = runCLI(t, "", "list", "--json", "--server", srv.URL); r.code != 1 || n.Load() == 0 {
		t.Fatalf("with key %+v, requests %d", r, n.Load())
	}
	db := filepath.Join(os.Getenv("XDG_DATA_HOME"), "agentfeedback", "agentfeedback.db")
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("remote mode created the local database: %v", err)
	}
}

func TestMode_LocalAndServerIsUsage(t *testing.T) {
	isolate(t)
	if r := runCLI(t, "", "list", "--local", "--server", "http://x"); r.code != 2 || !strings.Contains(r.stderr, "--local and --server") {
		t.Fatalf("%+v", r)
	}
}

func TestMode_ConfiguredURLIsUsed(t *testing.T) {
	cfg, _ := isolate(t)
	srv, n := failingServer(t)
	writeFile(t, cfg, "url = \""+srv.URL+"\"\napi_key = \"k\"\n", 0o600)
	if r := runCLI(t, "", "list", "--json"); r.code != 1 || n.Load() == 0 {
		t.Fatalf("%+v, requests %d", r, n.Load())
	}
}

func TestMode_FlushLocal(t *testing.T) {
	_, cache := isolate(t)
	r := runCLI(t, "", "flush")
	if r.code != 0 || strings.TrimSpace(r.stdout) != `{"flushed":0,"duplicates":0,"pending":0,"rejected":0,"mismatched":0,"expired":0,"deferred":0}` {
		t.Fatalf("empty %+v", r)
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	t.Setenv(envAPIKey, "k")
	if r = runCLI(t, "", "submit", "friction", "--summary", "x", "--server", srv.URL); !strings.Contains(r.stdout, "spool") {
		t.Fatalf("spool setup %+v", r)
	}
	t.Setenv(envAPIKey, "")
	n, err := pendingSpool(cache)
	if err != nil || n != 1 {
		t.Fatalf("pending %d %v", n, err)
	}
	r = runCLI(t, "", "flush")
	if r.code != 1 || !strings.Contains(r.stderr, "the spool holds 1 submission(s) for a server and this invocation is in local mode; nothing was sent") ||
		!strings.Contains(r.stderr, "run agentfeedback flush --server URL") {
		t.Fatalf("pending %+v", r)
	}
	if n, _ = pendingSpool(cache); n != 1 {
		t.Fatalf("flush consumed the spool: %d", n)
	}
	r = runCLI(t, "", "flush", "--hook")
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("hook %+v", r)
	}
	if n, _ = pendingSpool(cache); n != 1 {
		t.Fatalf("hook consumed the spool: %d", n)
	}
}

func TestMode_MigrateLocalSourceDryRun(t *testing.T) {
	isolate(t)
	dst := newLive(t, targetKey)
	if r := runCLI(t, "", "submit", "friction", "--summary", "local one"); r.code != 0 {
		t.Fatalf("submit %+v", r)
	}
	r := migrate(t, dst, "--dry-run")
	if r.code != 0 || !strings.Contains(lastLine(r.stdout), `"outcome":"dry_run"`) || !strings.Contains(lastLine(r.stdout), `"would_send":1`) {
		t.Fatalf("%+v", r)
	}
}

// TestMode_InstallRefusesLocalBesideEnvURL: install --server local is refused
// when AGENT_FEEDBACK_URL names a server, since every command would still
// resolve that server; install never records a server the commands ignore.
func TestMode_InstallRefusesLocalBesideEnvURL(t *testing.T) {
	isolate(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envURL, "https://env.example.test")
	r := runCLI(t, "", "install", "pi", "--server", "local")
	if r.code != 1 || !strings.Contains(r.stdout, "differs from https://env.example.test (AGENT_FEEDBACK_URL)") {
		t.Fatalf("%+v", r)
	}
}

// TestMode_SubmitWhenLocalDatabaseCannotOpen: a local database that cannot be
// opened ends in the error outcome with the body echoed, never a bare error.
func TestMode_SubmitWhenLocalDatabaseCannotOpen(t *testing.T) {
	isolate(t)
	data := os.Getenv("XDG_DATA_HOME")
	// The data directory's parent is a file, so nothing under it can be created.
	writeFile(t, filepath.Join(data, "agentfeedback"), "not a directory", 0o600)
	r := runCLI(t, "", "submit", "friction", "--summary", "lost?")
	o := outcome(t, r)
	if r.code != 1 || o["outcome"] != "error" || o["reason"] != "client_setup" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.stderr, "NOT persisted") || !strings.Contains(r.stderr, `"summary":"lost?"`) {
		t.Fatalf("body not echoed: %+v", r)
	}
}

// TestMode_DoctorFlagConflicts: --init takes --url only; --url with --local
// names the right flag.
func TestMode_DoctorFlagConflicts(t *testing.T) {
	isolate(t)
	r := runCLI(t, "k", "doctor", "--init", "--url", "http://a", "--key-from-stdin", "--local")
	if r.code != 2 || !strings.Contains(r.stdout, "--init takes --url, not --local or --server") {
		t.Fatalf("init: %+v", r)
	}
	r = runCLI(t, "", "doctor", "--url", "http://a", "--local")
	if r.code != 2 || !strings.Contains(r.stderr, "--local and --url are both set") {
		t.Fatalf("url+local: %+v", r)
	}
}
