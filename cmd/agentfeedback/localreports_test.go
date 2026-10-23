package main

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedLocalReports submits two friction reports and one install-check row
// to the local database.
func seedLocalReports(t *testing.T) {
	t.Helper()
	for _, c := range []struct {
		stdin string
		args  []string
	}{
		{"", []string{"submit", "friction", "--summary", "local one", "--category", "test"}},
		{"", []string{"submit", "friction", "--summary", "local two", "--category", "test"}},
		{`{"summary":"install ok"}`, []string{"submit", "install-check", "--stdin"}},
	} {
		if r := runCLI(t, c.stdin, c.args...); r.code != 0 {
			t.Fatalf("%v: %+v", c.args, r)
		}
	}
}

// localRows counts every row of the local database, read-only.
func localRows(t *testing.T) int {
	t.Helper()
	path, err := localDBPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM submissions`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

// frictionRows counts the friction rows on a server.
func frictionRows(t *testing.T, l *live) int {
	t.Helper()
	_, recs := exported(t, l)
	n := 0
	for _, rec := range recs {
		if string(rec["kind"]) == `"friction"` {
			n++
		}
	}

	return n
}

func TestInit_SwitchNamesLocalReports(t *testing.T) {
	newInstallEnv(t)
	seedLocalReports(t)
	before := localRows(t)
	l := newLive(t, testKey)

	r, out := initRun(t, testKey+"\n", "--yes", "--server", l.srv.URL, "--key-from-stdin", "--harnesses", "none")
	want := migrateLocalCommand(l.srv.URL)
	if r.code != 0 || out.Status != "ok" || out.LocalReports == nil || out.LocalReports.Count != 2 ||
		out.LocalReports.Command != want || out.LocalReports.Migrated != nil {
		t.Fatalf("init: %+v", r)
	}
	if next := nextCommands(out.Next); len(next) == 0 || next[0] != want {
		t.Fatalf("next %q", next)
	}
	if after := localRows(t); after != before {
		t.Fatalf("local rows %d, were %d", after, before)
	}
	if n := frictionRows(t, l); n != 0 {
		t.Fatalf("server holds %d friction rows", n)
	}
	if strings.Contains(r.stdout+r.stderr, testKey) {
		t.Fatal("the key reached the output")
	}

	// Run again against the configured server: no switch, nothing named.
	_, out = initRun(t, "", "--yes", "--harnesses", "none")
	if out.Status != "ok" || out.LocalReports != nil {
		t.Fatalf("second init: %+v", out)
	}
}

func TestInit_SwitchCopiesOnYes(t *testing.T) {
	e := newInstallEnv(t)
	seedLocalReports(t)
	before := localRows(t)
	l := newLive(t, testKey)
	isTerminal = func() bool { return true }
	origSecret := readSecret
	readSecret = func() (string, error) { return testKey, nil }
	t.Cleanup(func() { readSecret = origSecret })

	// The server, none for harnesses, then yes to the copy.
	r := runCLI(t, l.srv.URL+"\nnone\ny\n", "init")
	if r.code != 0 || !strings.Contains(r.stderr, "This machine's local database holds 2 report(s). Copy them to "+l.srv.URL+" now? [y/N] ") {
		t.Fatalf("init: %+v", r)
	}
	if !strings.Contains(r.stdout, "local:    2 report(s) copied from the local database: sent 2, imported 2, skipped 0, conflicts 0") ||
		strings.Contains(r.stdout, "next:     agentfeedback migrate") {
		t.Fatalf("summary: %s", r.stdout)
	}
	if strings.Contains(r.stdout+r.stderr, testKey) {
		t.Fatal("the key reached the output")
	}
	if n := frictionRows(t, l); n != 2 {
		t.Fatalf("server holds %d friction rows", n)
	}
	if after := localRows(t); after != before {
		t.Fatalf("local rows %d, were %d", after, before)
	}

	// A second switch (a fresh config) copies nothing new.
	if err := os.Remove(e.home + "/.config/agentfeedback/config.toml"); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, l.srv.URL+"\nnone\nyes\n", "init")
	if r.code != 0 || !strings.Contains(r.stdout, "sent 2, imported 0, skipped 2") {
		t.Fatalf("second init: %+v", r)
	}
	if n := frictionRows(t, l); n != 2 {
		t.Fatalf("server holds %d friction rows", n)
	}

	// No: nothing copied, the command named.
	if err := os.Remove(e.home + "/.config/agentfeedback/config.toml"); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, l.srv.URL+"\nnone\n\n", "init")
	if r.code != 0 || !strings.Contains(r.stdout, "local:    2 report(s) in the local database stay there; copy them with "+migrateLocalCommand(l.srv.URL)) {
		t.Fatalf("init answered no: %+v", r)
	}
}

func TestInit_SwitchCopyFailureWarns(t *testing.T) {
	newInstallEnv(t)
	seedLocalReports(t)
	l := newLive(t, testKey)
	isTerminal = func() bool { return true }
	origSecret := readSecret
	readSecret = func() (string, error) { return testKey, nil }
	t.Cleanup(func() { readSecret = origSecret })
	orig := migrateTransport
	migrateTransport = func(http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("target down") })
	}
	t.Cleanup(func() { migrateTransport = orig })

	r := runCLI(t, l.srv.URL+"\nnone\ny\n", "init")
	if r.code != 0 || !strings.Contains(r.stderr, "agentfeedback init: warning: copying the local reports to "+l.srv.URL+" failed") ||
		!strings.Contains(r.stdout, "local:    2 report(s) in the local database stay there") ||
		!strings.Contains(r.stdout, "next:     "+migrateLocalCommand(l.srv.URL)) {
		t.Fatalf("init: %+v", r)
	}
}

func TestInit_SwitchWithoutLocalDatabase(t *testing.T) {
	newInstallEnv(t)
	l := newLive(t, testKey)
	path, _ := localDBPath(os.Getenv)

	r, out := initRun(t, testKey+"\n", "--yes", "--server", l.srv.URL, "--key-from-stdin", "--harnesses", "none")
	if r.code != 0 || out.Status != "ok" || out.LocalReports != nil || strings.Contains(r.stdout, "local_reports") {
		t.Fatalf("init: %+v", r)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the local database exists: %v", err)
	}
}

func TestDoctorInit_NamesLocalReports(t *testing.T) {
	newInstallEnv(t)
	seedLocalReports(t)
	before := localRows(t)

	r := runCLI(t, "throwaway\n", "doctor", "--init", "--url", "https://feedback.example.test", "--key-from-stdin")
	want := migrateLocalCommand("https://feedback.example.test")
	if r.code != 0 || !strings.Contains(r.stdout, `"status":"written"`) ||
		!strings.Contains(r.stdout, `"local_reports":{"count":2,"command":"`+want+`"}`) ||
		!strings.Contains(r.stderr, "local database holds 2 report(s)") || !strings.Contains(r.stderr, want) {
		t.Fatalf("doctor --init: %+v", r)
	}
	if after := localRows(t); after != before {
		t.Fatalf("local rows %d, were %d", after, before)
	}
}

func TestInit_SwitchNamesLocalReportsWhenTheCheckFails(t *testing.T) {
	newInstallEnv(t)
	seedLocalReports(t)
	l := newLive(t, testKey)

	r, out := initRun(t, "wrong-key\n", "--yes", "--server", l.srv.URL, "--key-from-stdin", "--harnesses", "none")
	if r.code != 1 || out.Status != "error" || out.LocalReports == nil || out.LocalReports.Count != 2 ||
		out.LocalReports.Command != migrateLocalCommand(l.srv.URL) {
		t.Fatalf("init: %+v", r)
	}
	if strings.Contains(r.stdout+r.stderr, "wrong-key") {
		t.Fatal("the key reached the output")
	}
	if err := os.Remove(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agentfeedback", "config.toml")); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, "wrong-key\n", "init", "--yes", "--server", l.srv.URL, "--key-from-stdin", "--harnesses", "none")
	if r.code != 1 || !strings.Contains(r.stdout, "local:    2 report(s) in the local database stay there; copy them with "+migrateLocalCommand(l.srv.URL)) {
		t.Fatalf("summary: %+v", r)
	}
}

func TestInit_CopyUsesTheResolvedKey(t *testing.T) {
	newInstallEnv(t)
	seedLocalReports(t)
	l := newLive(t, testKey)
	isTerminal = func() bool { return true }
	t.Setenv(envAPIKey, testKey)
	// Typed at a prompt that does not run: the environment's key wins.
	origSecret := readSecret
	readSecret = func() (string, error) { return "typed-other-key", nil }
	t.Cleanup(func() { readSecret = origSecret })

	r := runCLI(t, l.srv.URL+"\nnone\ny\n", "init")
	if r.code != 0 || !strings.Contains(r.stdout, "sent 2, imported 2") {
		t.Fatalf("init: %+v", r)
	}
}

func TestMigrateLocalCommand_Quotes(t *testing.T) {
	for in, want := range map[string]string{
		"https://feedback.example.test":     "https://feedback.example.test",
		"https://feedback.example.test/a;b": "'https://feedback.example.test/a;b'",
		"https://x.test/it's":               `'https://x.test/it'\''s'`,
	} {
		if got := migrateLocalCommand(in); got != "agentfeedback migrate --local --to "+want+" --to-key-from-stdin" {
			t.Errorf("%q: %s", in, got)
		}
	}
}

func TestCountLocalReports_OddDataDir(t *testing.T) {
	isolate(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "a?b#c"))
	// internal/store builds its DSN without escaping, so the fixture is
	// written here through the escaped URI.
	path, err := localDBPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", strings.Replace(readOnlyDSN(path), "mode=ro", "mode=rwc", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE submissions (kind TEXT NOT NULL); INSERT INTO submissions VALUES ('friction'), ('friction'), ('install-check')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	n, err := countLocalReports(os.Getenv)
	if err != nil || n != 2 {
		t.Fatalf("count %d, %v", n, err)
	}
}
