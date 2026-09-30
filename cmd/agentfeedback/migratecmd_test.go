package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	sourceKey = "src-key-7d1e0b9a55"
	targetKey = "dst-key-c42f8e6130"
)

// migratePair starts the source, which the client is pointed at, and the
// target, each with its own distinctive key; call isolate first.
func migratePair(t *testing.T) (src, dst *live) {
	t.Helper()
	src = newLive(t, sourceKey)
	dst = newLive(t, targetKey)
	t.Setenv(envURL, src.srv.URL)
	t.Setenv(envAPIKey, sourceKey)

	return src, dst
}

// migrate runs migrate against dst with the target key on stdin.
func migrate(t *testing.T, dst *live, args ...string) result {
	t.Helper()

	return runCLI(t, targetKey+"\n", append([]string{"migrate", "--to", dst.srv.URL, "--to-key-from-stdin"}, args...)...)
}

// exported is a server's export: its record lines in order and each record's
// members by uid.
func exported(t *testing.T, l *live) (lines []string, byUID map[string]map[string]json.RawMessage) {
	t.Helper()
	status, raw := l.call(t, http.MethodGet, "/api/v1/export", "")
	all := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if status != http.StatusOK || len(all) < 2 {
		t.Fatalf("export %d %s", status, raw)
	}
	lines = all[1 : len(all)-1]
	byUID = map[string]map[string]json.RawMessage{}
	for _, line := range lines {
		var rec map[string]json.RawMessage
		var uid string
		if json.Unmarshal([]byte(line), &rec) != nil || json.Unmarshal(rec["uid"], &uid) != nil {
			t.Fatalf("record %s", line)
		}
		byUID[uid] = rec
	}

	return lines, byUID
}

// sameRecords fails unless every source record is on the target under its
// uid with the same content_hash, payload bytes and stored fields.
func sameRecords(t *testing.T, src, dst *live, want int) {
	t.Helper()
	_, from := exported(t, src)
	_, to := exported(t, dst)
	if len(from) != want || len(to) != want {
		t.Fatalf("source %d records, target %d, want %d", len(from), len(to), want)
	}
	for uid, rec := range from {
		got, ok := to[uid]
		if !ok {
			t.Fatalf("uid %s missing on the target", uid)
		}
		for _, member := range []string{"kind", "key", "content_hash", "payload", "context", "created_at", "processed_at", "verdict", "ref", "redacted_at"} {
			if string(got[member]) != string(rec[member]) {
				t.Fatalf("uid %s %s: got %s, want %s", uid, member, got[member], rec[member])
			}
		}
	}
}

// outcome decodes the last stdout line.
func outcome(t *testing.T, r result) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(lastLine(r.stdout)), &out); err != nil {
		t.Fatalf("last line is not JSON: %+v", r)
	}

	return out
}

func seedMigration(t *testing.T, src *live) {
	t.Helper()
	src.seed(t, `{"kind":"friction","key":"k1","summary":"docs drifted","payload":{"category":"documentation","n":1.50}}`)
	src.seed(t, `{"kind":"mystery","summary":"unknown kind","payload":{"x":[1,2],"big":12345678901234567890}}`)
	src.seed(t, `{"kind":"friction","summary":"processed","payload":{"category":"tooling"}}`)
	src.seed(t, `{"kind":"friction","summary":"to redact","context":{"cwd":"/x"},"payload":{"details":"secret"}}`)
	src.call(t, http.MethodPost, "/api/v1/submissions/processed", `{"ids":[3],"verdict":"fixed","ref":"abc@1"}`)
	src.call(t, http.MethodDelete, "/api/v1/submissions/4", "")
}

func TestMigrate_RoundTrip(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	seedMigration(t, src)

	r := migrate(t, dst)
	if r.code != 0 || lastLine(r.stdout) != `{"outcome":"migrated","sent":4,"imported":4,"skipped":0,"conflicts":0,"excluded":0}` {
		t.Fatalf("first run %+v", r)
	}
	sameRecords(t, src, dst, 4)
	imports := 0
	for _, req := range dst.requests() {
		if req == "POST /api/v1/import" {
			imports++
		}
	}
	if imports != 1 {
		t.Fatalf("target requests %v", dst.requests())
	}

	r = migrate(t, dst)
	if r.code != 0 || lastLine(r.stdout) != `{"outcome":"migrated","sent":4,"imported":0,"skipped":4,"conflicts":0,"excluded":0}` {
		t.Fatalf("second run %+v", r)
	}
	sameRecords(t, src, dst, 4)
}

func TestMigrate_ChunksUnderTheCap(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	seedMigration(t, src)
	lines, _ := exported(t, src)
	longest := 0
	for _, l := range lines {
		longest = max(longest, len(l)+1)
	}
	old := migrateChunkMax
	t.Cleanup(func() { migrateChunkMax = old })
	migrateChunkMax = len(importHeader) + longest + trailerLen(1) // one record per chunk

	r := migrate(t, dst)
	if r.code != 0 || lastLine(r.stdout) != `{"outcome":"migrated","sent":4,"imported":4,"skipped":0,"conflicts":0,"excluded":0}` {
		t.Fatalf("%+v", r)
	}
	imports := 0
	for _, req := range dst.requests() {
		if req == "POST /api/v1/import" {
			imports++
		}
	}
	if imports != 4 {
		t.Fatalf("%d imports, want 4: %v", imports, dst.requests())
	}
	sameRecords(t, src, dst, 4)

	// A record that alone exceeds the cap stops the run and names its uid.
	var first struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	migrateChunkMax = len(importHeader) + trailerLen(1) + 10
	r = migrate(t, dst)
	if r.code != 1 || !strings.Contains(r.stderr, "record uid="+first.UID+" alone exceeds") {
		t.Fatalf("too large %+v", r)
	}
	if out := outcome(t, r); out["outcome"] != "error" || !strings.Contains(out["message"].(string), "alone exceeds") {
		t.Fatalf("outcome %v", out)
	}
}

func TestMigrate_LimitIsATotalAndPagesAfterTheLastID(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	src.seed(t, `{"kind":"install-check","summary":"install ok"}`)
	_, want := src.seed(t, `{"kind":"friction","summary":"one"}`)
	src.seed(t, `{"kind":"friction","summary":"two"}`)

	r := migrate(t, dst, "--limit", "1")
	if r.code != 0 || lastLine(r.stdout) != `{"outcome":"migrated","sent":1,"imported":1,"skipped":0,"conflicts":0,"excluded":1}` {
		t.Fatalf("%+v", r)
	}
	exports := 0
	for _, req := range src.requests() {
		if req == "GET /api/v1/export" {
			exports++
		}
	}
	_, got := exported(t, dst)
	if _, ok := got[want]; !ok || len(got) != 1 || exports != 2 {
		t.Fatalf("target %v, %d export pages", got, exports)
	}
}

// recorder is a RoundTripper for the target that logs every request and
// passes it on, unless answer is set and returns a response.
type recorder struct {
	mu     sync.Mutex
	reqs   []*http.Request
	next   http.RoundTripper
	answer func(*http.Request) *http.Response
}

func (rec *recorder) RoundTrip(r *http.Request) (*http.Response, error) {
	rec.mu.Lock()
	rec.reqs = append(rec.reqs, r)
	rec.mu.Unlock()
	if rec.answer != nil {
		if resp := rec.answer(r); resp != nil {
			return resp, nil
		}
	}

	return rec.next.RoundTrip(r)
}

// recordTarget installs a recorder around the target's transport.
func recordTarget(t *testing.T, answer func(*http.Request) *http.Response) *recorder {
	t.Helper()
	rec := &recorder{answer: answer}
	old := migrateTransport
	migrateTransport = func(rt http.RoundTripper) http.RoundTripper {
		rec.next = rt

		return rec
	}
	t.Cleanup(func() { migrateTransport = old })

	return rec
}

func TestMigrate_DryRunOnlyReads(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	id, uid := src.seed(t, `{"kind":"friction","summary":"to redact later","payload":{"details":"secret"}}`)
	if r := migrate(t, dst); r.code != 0 {
		t.Fatalf("setup %+v", r)
	}
	_, onTarget := exported(t, dst)
	var targetID int64
	if json.Unmarshal(onTarget[uid]["id"], &targetID) != nil {
		t.Fatalf("target %v", onTarget)
	}
	src.call(t, http.MethodDelete, "/api/v1/submissions/"+strconv.FormatInt(id, 10), "")
	src.seed(t, `{"kind":"friction","summary":"second"}`)
	src.seed(t, `{"kind":"mystery","payload":{"x":1}}`)
	src.seed(t, `{"kind":"install-check","summary":"install ok"}`)
	lines, _ := exported(t, src)

	rec := recordTarget(t, nil)
	r := migrate(t, dst, "--dry-run")
	want := strings.Join([]string{
		"kind friction: 2 records",
		"first friction: " + lines[0],
		"kind mystery: 1 records",
		"first mystery: " + lines[2],
		"excluded install-check: 1",
		"redact on target: uid=" + uid + " id=" + strconv.FormatInt(targetID, 10),
		`{"outcome":"dry_run","sent":0,"would_send":3,"excluded":1,"unredacted_tombstones":1}`,
	}, "\n") + "\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("dry run %+v\nwant %s", r, want)
	}
	if len(rec.reqs) != 2 {
		t.Fatalf("target requests %d", len(rec.reqs))
	}
	for _, req := range rec.reqs {
		if req.Method != http.MethodGet {
			t.Fatalf("dry run sent %s %s", req.Method, req.URL)
		}
	}
	if q := rec.reqs[1].URL.Query(); rec.reqs[1].URL.Path != "/api/v1/submissions" || q.Get("redacted") != "false" ||
		q.Get("content_hash") == "" || q.Get("limit") != "100" {
		t.Fatalf("tombstone check %s", rec.reqs[1].URL)
	}
	if _, after := exported(t, dst); len(after) != 1 {
		t.Fatalf("dry run changed the target: %v", after)
	}
}

func TestMigrate_InstallCheckAndConflicts(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	_, checkUID := src.seed(t, `{"kind":"install-check","summary":"install ok"}`)
	_, conflictUID := src.seed(t, `{"kind":"friction","key":"k1","summary":"source wording"}`)
	src.seed(t, `{"kind":"friction","summary":"plain"}`)
	existingID, _ := dst.seed(t, `{"kind":"friction","key":"k1","summary":"target wording"}`)

	r := migrate(t, dst)
	wantConflict := "conflict uid=" + conflictUID + " existing_id=" + strconv.FormatInt(existingID, 10) + " reason=key_mismatch\n"
	if r.code != 0 || !strings.HasPrefix(r.stdout, wantConflict) ||
		lastLine(r.stdout) != `{"outcome":"migrated","sent":2,"imported":1,"skipped":1,"conflicts":1,"excluded":1}` {
		t.Fatalf("default %+v", r)
	}
	if _, got := exported(t, dst); got[checkUID] != nil || got[conflictUID] != nil || len(got) != 2 {
		t.Fatalf("target %v", got)
	}

	r = migrate(t, dst, "--include-kind", "install-check")
	if r.code != 0 || !strings.HasPrefix(r.stdout, wantConflict) ||
		lastLine(r.stdout) != `{"outcome":"migrated","sent":3,"imported":1,"skipped":2,"conflicts":1,"excluded":0}` {
		t.Fatalf("included %+v", r)
	}
	if _, got := exported(t, dst); got[checkUID] == nil {
		t.Fatalf("install-check row not migrated: %v", got)
	}
}

func TestMigrate_KeysNeverShown(t *testing.T) {
	_, cache := isolate(t)
	src, dst := migratePair(t)
	seedMigration(t, src)
	var runs []result
	runs = append(runs, migrate(t, dst), migrate(t, dst, "--dry-run"))
	wrong := runCLI(t, "wrong-key-99e1\n", "migrate", "--to", dst.srv.URL, "--to-key-from-stdin")
	if wrong.code != 1 || !strings.Contains(wrong.stderr, "the key read from stdin") || strings.Contains(wrong.stderr, envAPIKey) ||
		strings.Contains(wrong.stdout, envAPIKey) {
		t.Fatalf("wrong key %+v", wrong)
	}
	runs = append(runs, wrong)
	for _, r := range runs {
		for _, key := range []string{sourceKey, targetKey} {
			if strings.Contains(r.stdout, key) || strings.Contains(r.stderr, key) {
				t.Fatalf("key in output %+v", r)
			}
		}
	}
	err := filepath.WalkDir(cache, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), sourceKey) || strings.Contains(string(raw), targetKey) {
			t.Fatalf("key in %s", path)
		}
		if filepath.Base(path) == "client.jsonl" {
			t.Fatalf("migrate wrote %s", path)
		}

		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestMigrate_ToCloud(t *testing.T) {
	isolate(t)
	src, _ := migratePair(t)
	src.seed(t, `{"kind":"friction","summary":"one"}`)
	rec := recordTarget(t, func(r *http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}
	})

	r := runCLI(t, targetKey, "migrate", "--to", "cloud", "--to-key-from-stdin")
	if r.code != 1 || !strings.Contains(r.stderr, "the target "+cloudURL+" is too old") || outcome(t, r)["outcome"] != "error" {
		t.Fatalf("%+v", r)
	}
	if len(rec.reqs) != 1 || rec.reqs[0].URL.String() != cloudURL+"/api/v1/meta" {
		t.Fatalf("requests %v", rec.reqs)
	}

	count := 0
	for _, root := range []string{".", filepath.Join("..", "..", "pkg")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			raw, err := os.ReadFile(path)
			count += strings.Count(string(raw), cloudURL)

			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("the cloud URL appears %d times in non-test Go code under cmd/ and pkg/", count)
	}
}

func TestMigrate_TargetWithoutImport(t *testing.T) {
	isolate(t)
	migratePair(t)
	for _, features := range []string{`["q","stats"]`, `[]`} {
		recordTarget(t, func(r *http.Request) *http.Response {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(`{"features":` + features + `}`)), Request: r}
		})
		r := runCLI(t, targetKey, "migrate", "--to", "https://old.example", "--to-key-from-stdin")
		if r.code != 1 || !strings.Contains(r.stderr, "is too old: it has no POST /api/v1/import") {
			t.Fatalf("%s: %+v", features, r)
		}
	}
}

func TestMigrate_UsageErrors(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no key flag", []string{"--to", dst.srv.URL}, "pass --to-key-from-stdin"},
		{"no target", []string{"--to-key-from-stdin"}, "pass --to cloud"},
		{"bad target", []string{"--to", "ftp://x.example", "--to-key-from-stdin"}, "not usable"},
		{"credentials", []string{"--to", "https://u:hunter2@x.example", "--to-key-from-stdin"}, "carries credentials"},
		{"key as user name", []string{"--to", "https://sk-secret-123@host", "--to-key-from-stdin"}, "REDACTED@host"},
		{"same server", []string{"--to", src.srv.URL + "/", "--to-key-from-stdin"}, "is the configured source server"},
		{"limit", []string{"--to", dst.srv.URL, "--to-key-from-stdin", "--limit", "0"}, "pass --limit 1 or more"},
		{"args", []string{"--to", dst.srv.URL, "--to-key-from-stdin", "extra"}, "wrong number of arguments"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := runCLI(t, targetKey, append([]string{"migrate"}, tt.args...)...)
			if r.code != 2 || !strings.Contains(r.stderr, tt.want) || strings.Contains(r.stderr, "hunter2") ||
				strings.Contains(r.stdout+r.stderr, "sk-secret-123") {
				t.Fatalf("%+v", r)
			}
		})
	}
	if len(dst.requests()) != 0 {
		t.Fatalf("a usage error reached the target: %v", dst.requests())
	}
}

func TestMigrate_TargetFailureStopsWithCounts(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	seedMigration(t, src)
	old := migrateChunkMax
	t.Cleanup(func() { migrateChunkMax = old })
	lines, _ := exported(t, src)
	longest := 0
	for _, l := range lines {
		longest = max(longest, len(l)+1)
	}
	migrateChunkMax = len(importHeader) + longest + trailerLen(1)
	imports := 0
	recordTarget(t, func(r *http.Request) *http.Response {
		if r.Method == http.MethodPost {
			imports++
			if imports == 2 {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader(`{"error":"unavailable","message":"busy"}`)), Request: r}
			}
		}

		return nil
	})

	r := migrate(t, dst)
	if r.code != 1 || !strings.Contains(r.stderr, "migration stopped after 1 records sent (1 imported, 0 skipped)") ||
		!strings.Contains(r.stderr, "which is safe because the target skips the uids it already holds") ||
		lastLine(r.stdout) != `{"outcome":"error","message":`+string(mustJSON(t, strings.TrimPrefix(lastLine(r.stderr), "agentfeedback migrate: ")))+`,"sent":1,"imported":1,"skipped":0,"conflicts":0,"excluded":0}` {
		t.Fatalf("%+v", r)
	}
	if imports != 2 {
		t.Fatalf("%d imports; a failure must stop the run", imports)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

// fakeSource serves body as the source's export and points the client at it.
func fakeSource(t *testing.T, body string) {
	t.Helper()
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/export" {
			http.NotFound(w, r)

			return
		}
		_, _ = io.WriteString(w, body)
	})
}

// exportPage builds an export body from record lines; sum overrides the
// trailer's sha256 when not empty.
func exportPage(lines []string, sum string) string {
	var records strings.Builder
	for _, l := range lines {
		records.WriteString(l + "\n")
	}
	if sum == "" {
		h := sha256.Sum256([]byte(records.String()))
		sum = hex.EncodeToString(h[:])
	}

	return `{"export_format":2}` + "\n" + records.String() +
		`{"export_complete":true,"count":` + strconv.Itoa(len(lines)) + `,"sha256":"` + sum + `"}` + "\n"
}

// imports counts the target's import requests.
func imports(l *live) int {
	n := 0
	for _, req := range l.requests() {
		if req == "POST /api/v1/import" {
			n++
		}
	}

	return n
}

func TestMigrate_PageVerifiedBeforeAnythingIsSent(t *testing.T) {
	_, cache := isolate(t)
	src, dst := migratePair(t)
	seedMigration(t, src)
	lines, _ := exported(t, src)
	longest := 0
	for _, l := range lines {
		longest = max(longest, len(l)+1)
	}
	old := migrateChunkMax
	t.Cleanup(func() { migrateChunkMax = old })
	migrateChunkMax = len(importHeader) + longest + trailerLen(1) // one record per chunk
	fakeSource(t, exportPage(lines, strings.Repeat("0", 64)))

	r := migrate(t, dst)
	if r.code != 1 || !strings.Contains(r.stderr, "the trailer sha256 does not match") || imports(dst) != 0 {
		t.Fatalf("%+v, %d imports", r, imports(dst))
	}
	if files, _ := filepath.Glob(filepath.Join(cache, "migrate-*")); len(files) != 0 {
		t.Fatalf("page files left: %v", files)
	}
}

func TestMigrate_IDsMustAdvance(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	seedMigration(t, src)
	lines, _ := exported(t, src)
	fakeSource(t, exportPage([]string{lines[1], lines[0]}, ""))

	r := migrate(t, dst)
	if r.code != 1 || !strings.Contains(r.stderr, "the source export's ids did not advance (from 2 to 1)") || imports(dst) != 0 {
		t.Fatalf("%+v", r)
	}
}

// roundTripFunc is a RoundTripper from a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMigrate_MetaLimitCapsChunks(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	seedMigration(t, src)
	lines, _ := exported(t, src)
	longest := 0
	for _, l := range lines {
		longest = max(longest, len(l)+1)
	}
	limit := len(importHeader) + longest + trailerLen(1)
	old := migrateTransport
	t.Cleanup(func() { migrateTransport = old })
	migrateTransport = func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := rt.RoundTrip(r)
			if err != nil || r.URL.Path != "/api/v1/meta" {
				return resp, err
			}
			var meta map[string]any
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err := json.Unmarshal(raw, &meta); err != nil {
				return nil, err
			}
			meta["limits"].(map[string]any)["import_bytes"] = limit
			raw, _ = json.Marshal(meta)
			resp.Body = io.NopCloser(strings.NewReader(string(raw)))
			resp.ContentLength = int64(len(raw))
			resp.Header.Del("Content-Length")

			return resp, nil
		})
	}

	r := migrate(t, dst)
	if r.code != 0 || imports(dst) != 4 {
		t.Fatalf("%+v, %d imports", r, imports(dst))
	}
	sameRecords(t, src, dst, 4)
}

func TestMigrate_WarningNamesItsRecord(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	src.seed(t, `{"kind":"friction","summary":"one"}`)
	_, second := src.seed(t, `{"kind":"friction","summary":"two"}`)
	recordTarget(t, func(r *http.Request) *http.Response {
		if r.Method != http.MethodPost {
			return nil
		}

		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(
			`{"imported":2,"skipped":0,"conflicts":[],"warnings":[{"line":3,"code":"x_code","pointer":"/p","message":"m"}]}`))}
	})

	r := migrate(t, dst)
	if r.code != 0 || !strings.Contains(r.stderr, "warning uid="+second+" x_code /p: m\n") {
		t.Fatalf("%+v", r)
	}
}

func TestMigrate_ImportCountsMustAddUp(t *testing.T) {
	isolate(t)
	src, dst := migratePair(t)
	src.seed(t, `{"kind":"friction","summary":"one"}`)
	recordTarget(t, func(r *http.Request) *http.Response {
		if r.Method != http.MethodPost {
			return nil
		}

		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r,
			Body: io.NopCloser(strings.NewReader(`{"imported":5,"skipped":0,"conflicts":[],"warnings":[]}`))}
	})

	r := migrate(t, dst)
	if r.code != 1 || !strings.Contains(r.stderr, "counts 5 imported and 0 skipped for 1 records") ||
		!strings.Contains(lastLine(r.stdout), `"sent":1,"imported":0`) {
		t.Fatalf("%+v", r)
	}
}
