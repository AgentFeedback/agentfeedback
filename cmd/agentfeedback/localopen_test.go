package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// oddName is a directory name the SQLite URI form would misparse unescaped.
const oddName = "a?b#c%25d"

// dirEntries lists the names in dir.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}

	return names
}

// onlyDatabase fails unless dir holds the database file named base and at
// most its -wal and -shm files, plus the names in also.
func onlyDatabase(t *testing.T, dir, base string, also ...string) {
	t.Helper()
	names := dirEntries(t, dir)
	if !slices.Contains(names, base) {
		t.Fatalf("%s holds %v, no %s", dir, names, base)
	}
	for _, n := range names {
		if n != base && n != base+"-wal" && n != base+"-shm" && !slices.Contains(also, n) {
			t.Fatalf("%s holds %v", dir, names)
		}
	}
}

// TestLocalMode_DataDirNeedingEscapes: submit, list and backup on a data
// directory whose name holds ?, # and % use exactly the database in it and
// create nothing else beside it.
func TestLocalMode_DataDirNeedingEscapes(t *testing.T) {
	if goos == "windows" {
		t.Skip("? is not a valid file name character on Windows")
	}
	isolate(t)
	t.Setenv("DATABASE_PATH", "")
	parent := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(parent, oddName))
	if r := runCLI(t, "", "submit", "friction", "--summary", "odd path"); r.code != 0 || outcomeOf(t, r)["outcome"] != "submitted" {
		t.Fatalf("submit: %+v", r)
	}
	if r := runCLI(t, "", "list", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"total":1`) {
		t.Fatalf("list: %+v", r)
	}
	destDir := filepath.Join(t.TempDir(), oddName)
	if err := os.Mkdir(destDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(destDir, "copy.db")
	if r := runCLI(t, "", "backup", dest); r.code != 0 {
		t.Fatalf("backup: %+v", r)
	}
	if names := dirEntries(t, parent); !slices.Equal(names, []string{oddName}) {
		t.Fatalf("parent holds %v", names)
	}
	onlyDatabase(t, dataRoot(t), "agentfeedback.db")
	if names := dirEntries(t, filepath.Dir(dataRoot(t))); !slices.Equal(names, []string{"agentfeedback"}) {
		t.Fatalf("data home holds %v", names)
	}
	onlyDatabase(t, destDir, "copy.db")
	db, err := store.Open(context.Background(), dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.Read(context.Background(), func(q store.Querier) error {
		return q.QueryRowContext(context.Background(), "SELECT count(*) FROM submissions").Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("backup holds %d rows, %v", n, err)
	}
}

// TestServe_DatabasePathNeedingEscapes: serve with a DATABASE_PATH whose
// directory name holds ?, # and % opens exactly that file.
func TestServe_DatabasePathNeedingEscapes(t *testing.T) {
	if goos == "windows" {
		t.Skip("? is not a valid file name character on Windows")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, oddName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cmd := binaryCommand(t, binaryEnv(t, "DATABASE_PATH="+filepath.Join(dir, "agentfeedback.db"),
		"API_KEY=test-key-0123456789abcdef0123456789abcdef", "API_KEY_FILE=", "HTTP_LISTEN_ADDR="+addr), "serve")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := http.Get("http://" + addr + "/health")
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not come up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if names := dirEntries(t, parent); !slices.Equal(names, []string{oddName}) {
		t.Fatalf("parent holds %v", names)
	}
	onlyDatabase(t, dir, "agentfeedback.db")
}

// holdExclusive opens the database at path in exclusive locking mode and
// takes its write lock, so no other connection can read or write it until
// the returned release runs.
func holdExclusive(t *testing.T, path string) (release func()) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", store.FileURI(path, url.Values{"_pragma": {"locking_mode(EXCLUSIVE)"}}))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE schema_version SET version = version"); err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		_ = conn.Close()
		_ = db.Close()
	}
	t.Cleanup(release)

	return release
}

// spoolDestination reads the destination of the one spool entry.
func spoolDestination(t *testing.T) string {
	t.Helper()
	dir := client.SpoolDir(dataRoot(t))
	files := spoolFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("spool holds %v", files)
	}
	raw, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Destination string `json:"destination"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("entry %s: %v", raw, err)
	}

	return e.Destination
}

// TestSubmit_BusyDatabaseSpools: a local database locked past the busy
// timeout spools the submission for the local database (outcome spooled,
// exit 0, a stderr line naming the cause, nothing echoed); list keeps
// reporting the open error; once the lock is gone the next command delivers
// the entry.
func TestSubmit_BusyDatabaseSpools(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the busy timeout")
	}
	isolate(t)
	if r := runCLI(t, "", "list"); r.code != 0 {
		t.Fatalf("create the local database: %+v", r)
	}
	path, err := localDBPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	release := holdExclusive(t, path)
	r := runCLI(t, "", "submit", "friction", "--summary", "while busy")
	o := outcomeOf(t, r)
	if r.code != 0 || o["outcome"] != "spooled" || o["reason"] != reasonLocalBusy ||
		!strings.Contains(r.stderr, "is busy") || strings.Contains(r.stderr, "NOT persisted") {
		t.Fatalf("submit while busy: %+v", r)
	}
	if d := spoolDestination(t); d != client.LocalDestination {
		t.Fatalf("destination %q", d)
	}
	if r := runCLI(t, "", "list"); r.code != 1 || !strings.Contains(r.stderr, "cannot be opened") {
		t.Fatalf("list while busy: %+v", r)
	}
	release()
	r = runCLI(t, "", "list", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"total":1`) || !strings.Contains(r.stdout, "while busy") {
		t.Fatalf("list after the lock: %+v", r)
	}
	if left := spoolFiles(t, client.SpoolDir(dataRoot(t))); len(left) != 0 {
		t.Fatalf("spool left %v", left)
	}
}

// TestSubmit_ForeignDatabaseEchoes: a local database without the
// AgentFeedback stamp is not a spool case: submit reports the error and
// echoes the body.
func TestSubmit_ForeignDatabaseEchoes(t *testing.T) {
	isolate(t)
	// The path quotes the busy messages: classification must not read text.
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "sqlite_busy database is locked"))
	path, err := localDBPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.FileURI(path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE other (x)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	r := runCLI(t, "", "submit", "friction", "--summary", "foreign")
	o := outcomeOf(t, r)
	if r.code == 0 || o["outcome"] != "error" || o["reason"] != "client_setup" ||
		!strings.Contains(r.stderr, "NOT persisted") || !strings.Contains(r.stderr, "foreign") {
		t.Fatalf("%+v", r)
	}
	if files := spoolFiles(t, client.SpoolDir(dataRoot(t))); len(files) != 0 {
		t.Fatalf("spool holds %v", files)
	}
}

// TestSubmit_NewerDatabaseSpoolUnwritable: when the spool cannot be written
// either, submit reports error spool_unwritable, echoes the body and never
// claims the report was kept.
func TestSubmit_NewerDatabaseSpoolUnwritable(t *testing.T) {
	isolate(t)
	newerSchemaDB(t)
	if err := os.WriteFile(client.SpoolDir(dataRoot(t)), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, "", "submit", "friction", "--summary", "nowhere to keep")
	o := outcomeOf(t, r)
	if r.code == 0 || o["outcome"] != "error" || o["reason"] != "spool_unwritable" ||
		!strings.Contains(r.stderr, "NOT persisted") || !strings.Contains(r.stderr, "nowhere to keep") ||
		strings.Contains(r.stderr, "kept in the spool") {
		t.Fatalf("%+v", r)
	}
}
