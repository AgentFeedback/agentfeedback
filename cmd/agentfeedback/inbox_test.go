package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
)

const inboxEnvelope = `{"kind":"friction","summary":"from inbox","payload":{"category":"documentation"}}`

// inboxDir creates the inbox under the isolated data directory, 0700.
func inboxDir(t *testing.T) string {
	t.Helper()
	dir := client.InboxDir(dataRoot(t))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	return dir
}

// listRows runs list --json and returns its submissions.
func listRows(t *testing.T) []map[string]any {
	t.Helper()
	r := runCLI(t, "", "list", "--json")
	if r.code != 0 {
		t.Fatalf("list: %+v", r)
	}
	var out struct {
		Submissions []map[string]any `json:"submissions"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("list output %q: %v", r.stdout, err)
	}

	return out.Submissions
}

// getRow runs get <id> --json and returns the submission object.
func getRow(t *testing.T, id int64) map[string]any {
	t.Helper()
	r := runCLI(t, "", "get", strconv.FormatInt(id, 10), "--json")
	if r.code != 0 {
		t.Fatalf("get: %+v", r)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &m); err != nil {
		t.Fatalf("get output %q: %v", r.stdout, err)
	}
	if s, ok := m["submission"].(map[string]any); ok {
		return s
	}

	return m
}

func rowID(t *testing.T, row map[string]any) int64 {
	t.Helper()
	f, ok := row["id"].(float64)
	if !ok {
		t.Fatalf("row without id: %v", row)
	}

	return int64(f)
}

// ingestReport runs ingest --json (plus extra args) and decodes the report.
func ingestReport(t *testing.T, args ...string) (inboxReport, result) {
	t.Helper()
	r := runCLI(t, "", append([]string{"ingest", "--json"}, args...)...)
	var rep inboxReport
	if r.code == 0 {
		if err := json.Unmarshal([]byte(lastLine(r.stdout)), &rep); err != nil {
			t.Fatalf("ingest output %q: %v", r.stdout, err)
		}
	}

	return rep, r
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func mustBeGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still exists (%v)", path, err)
	}
}

// writeOld writes path with an mtime two minutes back, past inboxSettle.
func writeOld(t *testing.T, path, body string) {
	t.Helper()
	writeFile(t, path, body, 0o600)
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// reasonOutcome decodes the first line of a .reason sidecar.
func reasonOutcome(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := bufio.NewReader(bytes.NewReader(raw)).ReadLine()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("reason first line %q: %v", line, err)
	}

	return m
}

// TestInbox_IngestedAtStartOfCommand proves any local-mode command ingests
// the inbox first: list shows the row, which carries origin inbox and a
// derived key, and the file moves to done/<id>-<name>.
func TestInbox_IngestedAtStartOfCommand(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	writeFile(t, filepath.Join(dir, "a.json"), inboxEnvelope, 0o600)

	rows := listRows(t)
	if len(rows) != 1 || rows[0]["summary"] != "from inbox" {
		t.Fatalf("rows %v", rows)
	}
	id := rowID(t, rows[0])
	row := getRow(t, id)
	ctx, _ := row["context"].(map[string]any)
	if ctx["origin"] != originInbox {
		t.Fatalf("context %v", row["context"])
	}
	if key, _ := row["key"].(string); !strings.HasPrefix(key, "inbox-") {
		t.Fatalf("key %v", row["key"])
	}
	mustBeGone(t, filepath.Join(dir, "a.json"))
	mustExist(t, filepath.Join(dir, inboxDone, strconv.FormatInt(id, 10)+"-a.json"))
}

// TestInbox_WriterKeyAndContextKept proves a key the writer gave is the row
// key, and a context object keeps its members with origin forced to inbox.
func TestInbox_WriterKeyAndContextKept(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	writeFile(t, filepath.Join(dir, "b.json"),
		`{"kind":"friction","key":"my-key-1","summary":"keyed","context":{"origin":"agent","note":"kept"}}`, 0o600)

	rep, r := ingestReport(t)
	if r.code != 0 || rep.Ingested != 1 {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	rows := listRows(t)
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	row := getRow(t, rowID(t, rows[0]))
	if row["key"] != "my-key-1" {
		t.Fatalf("key %v", row["key"])
	}
	ctx, _ := row["context"].(map[string]any)
	if ctx["origin"] != originInbox || ctx["note"] != "kept" {
		t.Fatalf("context %v", row["context"])
	}
}

// TestInbox_InvalidFilesRejected proves bodies refused client-side and by
// the destination are moved to rejected/ with a reason sidecar whose first
// line is the rejected outcome.
func TestInbox_InvalidFilesRejected(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
	}{
		{"not-json.json", `not json`, "invalid_body"},
		{"array.json", `[1]`, "invalid_body"},
		{"numkey.json", `{"kind":"friction","key":5,"summary":"x"}`, "invalid_body"},
		// Nested past envelope.MaxDepth: valid JSON the client passes and
		// the create path refuses (400 bad_request).
		{"server.json", `{"kind":"friction","summary":"deep","payload":{"d":` +
			strings.Repeat("[", envelope.MaxDepth+1) + strings.Repeat("]", envelope.MaxDepth+1) + `}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			dir := inboxDir(t)
			writeOld(t, filepath.Join(dir, tc.name), tc.body)
			rep, r := ingestReport(t)
			if r.code != 0 || rep.Rejected != 1 || rep.Ingested != 0 {
				t.Fatalf("ingest %+v %+v", rep, r)
			}
			mustBeGone(t, filepath.Join(dir, tc.name))
			dest := filepath.Join(dir, inboxRejected, tc.name)
			mustExist(t, dest)
			o := reasonOutcome(t, dest+reasonSuffix)
			if o["outcome"] != "rejected" {
				t.Fatalf("reason %v", o)
			}
			if tc.reason != "" && o["reason"] != tc.reason {
				t.Fatalf("reason %v, want %s", o, tc.reason)
			}
			if tc.reason == "" && o["reason"] == "invalid_body" {
				t.Fatalf("refused client-side, want a destination refusal: %v", o)
			}
			if rows := listRows(t); len(rows) != 0 {
				t.Fatalf("rows %v", rows)
			}
		})
	}
}

// TestInbox_SymlinkSkipped proves a symlink in the inbox is named, counted
// as skipped, left in place and never read.
func TestInbox_SymlinkSkipped(t *testing.T) {
	if goos == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	isolate(t)
	dir := inboxDir(t)
	target := filepath.Join(t.TempDir(), "outside.json")
	writeFile(t, target, inboxEnvelope, 0o600)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	rep, r := ingestReport(t)
	if r.code != 0 || rep.Skipped != 1 || rep.Ingested != 0 {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	if !strings.Contains(r.stderr, "skipped") || !strings.Contains(r.stderr, link) {
		t.Fatalf("stderr %q", r.stderr)
	}
	mustExist(t, link)
	if raw, err := os.ReadFile(target); err != nil || string(raw) != inboxEnvelope {
		t.Fatalf("target changed: %q %v", raw, err)
	}
	if rows := listRows(t); len(rows) != 0 {
		t.Fatalf("rows %v", rows)
	}
}

// TestInbox_OverCapRejected proves a file over the create cap is rejected
// as body_too_large.
func TestInbox_OverCapRejected(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	path := filepath.Join(dir, "big.json")
	writeFile(t, path, "", 0o600)
	if err := os.Truncate(path, envelope.BodyLimit+1); err != nil {
		t.Fatal(err)
	}
	rep, r := ingestReport(t)
	if r.code != 0 || rep.Rejected != 1 {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	o := reasonOutcome(t, filepath.Join(dir, inboxRejected, "big.json"+reasonSuffix))
	if o["outcome"] != "rejected" || o["reason"] != "body_too_large" {
		t.Fatalf("reason %v", o)
	}
}

// TestInbox_NonCandidatesUntouched proves dotfiles, non-.json names and the
// done/ and rejected/ subdirectories are not read.
func TestInbox_NonCandidatesUntouched(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	paths := []string{
		filepath.Join(dir, ".x.json"),
		filepath.Join(dir, "x.tmp"),
		filepath.Join(dir, inboxDone, "d.json"),
		filepath.Join(dir, inboxRejected, "r.json"),
	}
	for _, sub := range []string{inboxDone, inboxRejected} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range paths {
		writeFile(t, p, inboxEnvelope, 0o600)
	}
	rep, r := ingestReport(t)
	if r.code != 0 || rep != (inboxReport{}) {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	for _, p := range paths {
		if raw, err := os.ReadFile(p); err != nil || string(raw) != inboxEnvelope {
			t.Fatalf("%s changed: %q %v", p, raw, err)
		}
	}
	if rows := listRows(t); len(rows) != 0 {
		t.Fatalf("rows %v", rows)
	}
}

// TestInbox_InboxSymlinkRefused proves an inbox that is a link to a
// directory fails ingest, names the path, and ingests nothing.
func TestInbox_InboxSymlinkRefused(t *testing.T) {
	if goos == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	isolate(t)
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "a.json"), inboxEnvelope, 0o600)
	dir := client.InboxDir(dataRoot(t))
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, dir); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, "", "ingest")
	if r.code == 0 || !strings.Contains(r.stderr, dir) {
		t.Fatalf("ingest %+v", r)
	}
	mustExist(t, filepath.Join(real, "a.json"))
	if rows := listRows(t); len(rows) != 0 {
		t.Fatalf("rows %v", rows)
	}
}

// TestInbox_ReplayIsDuplicate proves the same name and bytes ingested twice
// is one row: the second pass is a duplicate and moves the file to done.
func TestInbox_ReplayIsDuplicate(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	path := filepath.Join(dir, "a.json")
	writeFile(t, path, inboxEnvelope, 0o600)
	if rep, r := ingestReport(t); r.code != 0 || rep.Ingested != 1 {
		t.Fatalf("first ingest %+v %+v", rep, r)
	}
	writeFile(t, path, inboxEnvelope, 0o600)
	rep, r := ingestReport(t)
	if r.code != 0 || rep.Duplicates != 1 || rep.Ingested != 0 {
		t.Fatalf("second ingest %+v %+v", rep, r)
	}
	mustBeGone(t, path)
	rows := listRows(t)
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	mustExist(t, filepath.Join(dir, inboxDone, strconv.FormatInt(rowID(t, rows[0]), 10)+"-a.json"))
}

// TestIngest_EmptyInboxCreatesNoDatabase proves ingest over a missing or
// empty inbox in local mode prints zero counts and creates no database.
func TestIngest_EmptyInboxCreatesNoDatabase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create bool
	}{{"missing", false}, {"empty", true}} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			if tc.create {
				inboxDir(t)
			}
			db := filepath.Join(dataRoot(t), "agentfeedback.db")
			r := runCLI(t, "", "ingest")
			if r.code != 0 || r.stdout != "ingested 0, duplicates 0, rejected 0, pending 0, skipped 0\n" {
				t.Fatalf("ingest %+v", r)
			}
			r = runCLI(t, "", "ingest", "--json")
			if r.code != 0 {
				t.Fatalf("ingest --json %+v", r)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(r.stdout), &m); err != nil {
				t.Fatalf("ingest --json %q: %v", r.stdout, err)
			}
			for _, k := range []string{"ingested", "duplicates", "rejected", "pending", "skipped"} {
				if m[k] != float64(0) {
					t.Fatalf("%s = %v in %v", k, m[k], m)
				}
			}
			if len(m) != 5 {
				t.Fatalf("keys %v", m)
			}
			mustBeGone(t, db)
		})
	}
}

// TestIngest_RemoteMode proves ingest delivers through a configured server,
// leaves a file pending on 503, and stops the pass on 401 with every
// candidate pending and in place.
func TestIngest_RemoteMode(t *testing.T) {
	t.Run("delivered", func(t *testing.T) {
		isolate(t)
		db := openDB(t, filepath.Join(t.TempDir(), "srv.db"))
		svc := core.New(db, core.Config{Version: clientVersion().Version, Features: api.Features})
		srv := httptest.NewServer(api.New(api.Config{Service: svc, DB: db, APIKey: testKey}).Handler())
		t.Cleanup(srv.Close)
		t.Setenv(envURL, srv.URL)
		t.Setenv(envAPIKey, testKey)
		dir := inboxDir(t)
		writeFile(t, filepath.Join(dir, "a.json"), inboxEnvelope, 0o600)
		rep, r := ingestReport(t)
		if r.code != 0 || rep.Ingested != 1 {
			t.Fatalf("ingest %+v %+v", rep, r)
		}
		mustBeGone(t, filepath.Join(dir, "a.json"))
		rows := listRows(t)
		if len(rows) != 1 || rows[0]["summary"] != "from inbox" {
			t.Fatalf("server rows %v", rows)
		}
	})
	for _, tc := range []struct {
		name   string
		status int
		files  []string
	}{
		{"busy", http.StatusServiceUnavailable, []string{"a.json"}},
		{"wrong key", http.StatusUnauthorized, []string{"a.json", "b.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"x","message":"no"}`))
			}))
			t.Cleanup(srv.Close)
			t.Setenv(envURL, srv.URL)
			t.Setenv(envAPIKey, testKey)
			dir := inboxDir(t)
			for _, f := range tc.files {
				writeFile(t, filepath.Join(dir, f), inboxEnvelope, 0o600)
			}
			rep, r := ingestReport(t)
			if r.code != 0 || rep != (inboxReport{Pending: len(tc.files)}) {
				t.Fatalf("ingest %+v %+v", rep, r)
			}
			for _, f := range tc.files {
				mustExist(t, filepath.Join(dir, f))
			}
		})
	}
}

// TestInbox_Modes proves done/ and rejected/ are created 0700 and the
// reason sidecar is 0600.
func TestInbox_Modes(t *testing.T) {
	if goos == "windows" {
		t.Skip("POSIX modes")
	}
	isolate(t)
	dir := inboxDir(t)
	writeFile(t, filepath.Join(dir, "a.json"), inboxEnvelope, 0o600)
	writeOld(t, filepath.Join(dir, "bad.json"), `not json`)
	if rep, r := ingestReport(t); r.code != 0 || rep.Ingested != 1 || rep.Rejected != 1 {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, inboxDone):                              0o700,
		filepath.Join(dir, inboxRejected):                          0o700,
		filepath.Join(dir, inboxRejected, "bad.json"+reasonSuffix): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode %o, want %o", path, got, want)
		}
	}
}

// TestInboxKey proves the derived key is deterministic, depends on name and
// bytes, and has the inbox- prefix and fixed length.
func TestInboxKey(t *testing.T) {
	a := inboxKey("a.json", []byte("x"))
	if a != inboxKey("a.json", []byte("x")) {
		t.Fatal("not deterministic")
	}
	if a == inboxKey("b.json", []byte("x")) || a == inboxKey("a.json", []byte("y")) {
		t.Fatal("does not depend on name and bytes")
	}
	if !strings.HasPrefix(a, "inbox-") || len(a) != 38 {
		t.Fatalf("key %q", a)
	}
}

// TestInboxBody_DropsByteOrderMark proves a file a Windows writer saved with
// a UTF-8 byte order mark is read as the object it holds.
func TestInboxBody_DropsByteOrderMark(t *testing.T) {
	got, err := inboxBody("bom.json", []byte("\xef\xbb\xbf{\"kind\":\"friction\",\"key\":\"k\",\"summary\":\"s\"}"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"friction","key":"k","summary":"s","context":{"origin":"inbox"}}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// TestInbox_FreshInvalidLeftPending proves a file that is not one JSON
// object and was written in the last minute is left for the next pass.
func TestInbox_FreshInvalidLeftPending(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	writeFile(t, filepath.Join(dir, "fresh.json"), `{"kind":`, 0o600)
	rep, r := ingestReport(t)
	if r.code != 0 || rep != (inboxReport{Pending: 1}) {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	mustExist(t, filepath.Join(dir, "fresh.json"))
	mustBeGone(t, filepath.Join(dir, inboxRejected, "fresh.json"))
	mustBeGone(t, filepath.Join(dir, inboxRejected, "fresh.json"+reasonSuffix))
}

// TestInbox_ReasonSidecarSymlinkNotFollowed proves a pre-existing link with
// the sidecar's name is never written through: the file lands in rejected/
// under another name with its own sidecar.
func TestInbox_ReasonSidecarSymlinkNotFollowed(t *testing.T) {
	if goos == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	isolate(t)
	dir := inboxDir(t)
	if err := os.Mkdir(filepath.Join(dir, inboxRejected), 0o700); err != nil {
		t.Fatal(err)
	}
	const outsideBody = "outside, untouched"
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, outsideBody, 0o600)
	link := filepath.Join(dir, inboxRejected, "bad.json"+reasonSuffix)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	writeOld(t, filepath.Join(dir, "bad.json"), `not json`)

	rep, r := ingestReport(t)
	if r.code != 0 || rep.Rejected != 1 {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != outsideBody {
		t.Fatalf("outside file changed: %q %v", raw, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link %v %v", info, err)
	}
	mustBeGone(t, filepath.Join(dir, "bad.json"))
	mustBeGone(t, filepath.Join(dir, inboxRejected, "bad.json"))
	matches, err := filepath.Glob(filepath.Join(dir, inboxRejected, "bad-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("rejected entries %v %v", matches, err)
	}
	if o := reasonOutcome(t, matches[0]+reasonSuffix); o["reason"] != "invalid_body" {
		t.Fatalf("reason %v", o)
	}
}

// TestMoveInboxFile_ReplacedNotMoved proves a name that now holds another
// file is not moved, and the same file is.
func TestMoveInboxFile_ReplacedNotMoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	writeFile(t, path, "old", 0o600)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat("a.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// A different size: on ext4 the new file can get the old inode back, and
	// within one clock tick the same modification time.
	writeFile(t, path, "new content", 0o600)
	if err := moveInboxFile(root, info, "a.json", "done/x-a.json"); !errors.Is(err, errInboxChanged) {
		t.Fatalf("err %v, want errInboxChanged", err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "new content" {
		t.Fatalf("a.json %q %v", raw, err)
	}
	mustBeGone(t, filepath.Join(dir, "done", "x-a.json"))

	info, err = root.Lstat("a.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := moveInboxFile(root, info, "a.json", "done/x-a.json"); err != nil {
		t.Fatal(err)
	}
	mustBeGone(t, path)
	if raw, err := os.ReadFile(filepath.Join(dir, "done", "x-a.json")); err != nil || string(raw) != "new content" {
		t.Fatalf("moved %q %v", raw, err)
	}
	if err := moveInboxFile(root, info, "a.json", "done/y-a.json"); !errors.Is(err, errInboxGone) {
		t.Fatalf("a moved file: err %v, want errInboxGone", err)
	}
}

// TestMoveInboxFile_RewrittenInPlaceNotMoved proves a file rewritten through
// the same inode (O_TRUNC) after the read is not moved.
func TestMoveInboxFile_RewrittenInPlaceNotMoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	writeFile(t, path, "old", 0o600)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat("a.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("rewritten"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveInboxFile(root, info, "a.json", "done/x-a.json"); !errors.Is(err, errInboxChanged) {
		t.Fatalf("err %v, want errInboxChanged", err)
	}
	mustBeGone(t, filepath.Join(dir, "done", "x-a.json"))
}

// TestInbox_LongNameRejected proves a refused file whose name is near the
// file-name limit still lands in rejected/ with its reason.
func TestInbox_LongNameRejected(t *testing.T) {
	isolate(t)
	name := strings.Repeat("n", 250) + ".json"
	writeOld(t, filepath.Join(inboxDir(t), name), "not json")
	rep, r := ingestReport(t)
	if rep.Rejected != 1 || rep.Pending != 0 {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	entries, err := os.ReadDir(filepath.Join(inboxDir(t), inboxRejected))
	if err != nil || len(entries) != 2 {
		t.Fatalf("rejected/: %v %v", entries, err)
	}
	for _, e := range entries {
		if len(e.Name()) > nameMax {
			t.Fatalf("%d-byte name %s", len(e.Name()), e.Name())
		}
	}
}

// TestFitName proves fitName keeps the ending and never splits a character.
func TestFitName(t *testing.T) {
	if got := fitName("short.json", 20); got != "short.json" {
		t.Fatal(got)
	}
	if got := fitName("abcdef.json", 8); got != "def.json" {
		t.Fatal(got)
	}
	// "é" is two bytes; a cut through one moves to the next character.
	if got := fitName("éé.json", 7); got != "é.json" {
		t.Fatalf("%q", got)
	}
	if got := fitName("xé.json", 6); got != ".json" || !utf8.ValidString(got) {
		t.Fatalf("%q", got)
	}
}

// TestIngestInbox_ExpiredContext proves a pass whose deadline has passed
// reads nothing and leaves every file pending.
func TestIngestInbox_ExpiredContext(t *testing.T) {
	isolate(t)
	writeFile(t, filepath.Join(inboxDir(t), "a.json"), `{"kind":"friction","summary":"a"}`, 0o600)
	writeFile(t, filepath.Join(inboxDir(t), "b.json"), `{"kind":"friction","summary":"b"}`, 0o600)
	skipStartupPass = true // the pass under test is the only one
	c, err := apiClient(os.Getenv, modeFlags{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLocal()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := ingestInbox(ctx, c, dataRoot(t), func(string) {})
	if err != nil || rep.Pending != 2 || rep.Ingested != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	for _, n := range []string{"a.json", "b.json"} {
		if _, err := os.Stat(filepath.Join(inboxDir(t), n)); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
}

// TestInbox_NonObjectContext proves a null context becomes {"origin":"inbox"}
// and a scalar one is kept as context_raw beside it.
func TestInbox_NonObjectContext(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	writeFile(t, filepath.Join(dir, "null.json"), `{"kind":"friction","key":"k-null","summary":"null ctx","context":null}`, 0o600)
	writeFile(t, filepath.Join(dir, "scalar.json"), `{"kind":"friction","key":"k-scalar","summary":"scalar ctx","context":"scalar"}`, 0o600)
	rep, r := ingestReport(t)
	if r.code != 0 || rep.Ingested != 2 {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	rows := listRows(t)
	if len(rows) != 2 {
		t.Fatalf("rows %v", rows)
	}
	for _, lr := range rows {
		row := getRow(t, rowID(t, lr))
		ctx, _ := row["context"].(map[string]any)
		if ctx["origin"] != originInbox {
			t.Fatalf("context %v in %v", row["context"], row)
		}
		payload, _ := row["payload"].(map[string]any)
		switch row["key"] {
		case "k-scalar":
			if payload["context_raw"] != "scalar" {
				t.Fatalf("scalar row %v", row)
			}
		case "k-null":
			if _, ok := payload["context_raw"]; ok {
				t.Fatalf("null row %v", row)
			}
		default:
			t.Fatalf("row %v", row)
		}
	}
}

// TestInbox_KeyMismatchRejected proves an inbox file reusing a stored key
// with other content is moved to rejected/ with a mismatch reason.
func TestInbox_KeyMismatchRejected(t *testing.T) {
	isolate(t)
	if r := runCLI(t, "", "submit", "friction", "--summary", "a", "--key", "k1"); r.code != 0 {
		t.Fatalf("submit %+v", r)
	}
	dir := inboxDir(t)
	writeFile(t, filepath.Join(dir, "m.json"), `{"kind":"friction","key":"k1","summary":"different"}`, 0o600)
	rep, r := ingestReport(t)
	if r.code != 0 || rep.Rejected != 1 {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	mustBeGone(t, filepath.Join(dir, "m.json"))
	mustExist(t, filepath.Join(dir, inboxRejected, "m.json"))
	if o := reasonOutcome(t, filepath.Join(dir, inboxRejected, "m.json"+reasonSuffix)); o["outcome"] != "mismatch" {
		t.Fatalf("reason %v", o)
	}
	if rows := listRows(t); len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
}

// TestInbox_OverCapAfterSplice proves a file exactly at the create cap that
// goes over it once the key and origin are added is refused client-side.
func TestInbox_OverCapAfterSplice(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	head, tail := `{"kind":"friction","summary":"`, `"}`
	body := head + strings.Repeat("a", envelope.BodyLimit-len(head)-len(tail)) + tail
	if len(body) != envelope.BodyLimit {
		t.Fatalf("body %d bytes", len(body))
	}
	writeOld(t, filepath.Join(dir, "edge.json"), body)
	rep, r := ingestReport(t)
	if r.code != 0 || rep.Rejected != 1 {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	o := reasonOutcome(t, filepath.Join(dir, inboxRejected, "edge.json"+reasonSuffix))
	if o["reason"] != "body_too_large" {
		t.Fatalf("reason %v", o)
	}
	if msg, _ := o["message"].(string); !strings.Contains(msg, "with the key and origin added") {
		t.Fatalf("message %v", o)
	}
}

// TestInbox_RetryStopsPass proves a busy destination stops the pass after
// one request, with every file pending and in place.
func TestInbox_RetryStopsPass(t *testing.T) {
	isolate(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"x","message":"busy"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(envURL, srv.URL)
	t.Setenv(envAPIKey, testKey)
	dir := inboxDir(t)
	for _, f := range []string{"a.json", "b.json"} {
		writeFile(t, filepath.Join(dir, f), inboxEnvelope, 0o600)
	}
	rep, r := ingestReport(t)
	if r.code != 0 || rep != (inboxReport{Pending: 2}) {
		t.Fatalf("ingest %+v %+v", rep, r)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("requests %d, want 1", n)
	}
	for _, f := range []string{"a.json", "b.json"} {
		mustExist(t, filepath.Join(dir, f))
	}
}

// TestFlush_IngestsInboxLocal proves flush in local mode ingests the inbox.
func TestFlush_IngestsInboxLocal(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	writeFile(t, filepath.Join(dir, "a.json"), inboxEnvelope, 0o600)
	if r := runCLI(t, "", "flush"); r.code != 0 {
		t.Fatalf("flush %+v", r)
	}
	mustBeGone(t, filepath.Join(dir, "a.json"))
	rows := listRows(t)
	if len(rows) != 1 || rows[0]["summary"] != "from inbox" {
		t.Fatalf("rows %v", rows)
	}
	mustExist(t, filepath.Join(dir, inboxDone, strconv.FormatInt(rowID(t, rows[0]), 10)+"-a.json"))
}

// TestFlushHookBody_IngestsInbox proves the hook body with an empty spool
// still ingests an inbox file in local mode.
func TestFlushHookBody_IngestsInbox(t *testing.T) {
	isolate(t)
	data := dataRoot(t)
	dir := inboxDir(t)
	writeFile(t, filepath.Join(dir, "a.json"), inboxEnvelope, 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := flushHookBody(ctx, data, modeFlags{})
	closeLocal()
	if err != nil {
		t.Fatal(err)
	}
	mustBeGone(t, filepath.Join(dir, "a.json"))
	rows := listRows(t)
	if len(rows) != 1 || rows[0]["summary"] != "from inbox" {
		t.Fatalf("rows %v", rows)
	}
}
