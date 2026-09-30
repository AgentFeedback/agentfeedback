package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/pkg/client"
)

const fixtureRun = "20260730-101010-4242"

// reviewTestdata is absolute: the submit tests change the working directory.
var reviewTestdata, _ = filepath.Abs(filepath.Join("testdata", "review"))

// isolateSubmit is isolate plus the review and harness variables submit
// reads, and a working directory outside any repository.
func isolateSubmit(t *testing.T) (cfgPath, cache string) {
	t.Helper()
	cfgPath, cache = isolate(t)
	for _, name := range []string{envReviewLogDir, envReviewDirs, "AGENT_FEEDBACK_SESSION_ID", "AI_AGENT",
		"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_EFFORT", "OPENCODE", "OPENCODE_MODEL", "PI_CODING_AGENT",
		"PI_CODING_AGENT_DIR", "OMP_PROFILE", "PI_MODEL", "PI_PROFILE", "CODEX_SANDBOX"} {
		t.Setenv(name, "")
	}
	t.Chdir(t.TempDir())
	old := reviewLocation
	reviewLocation = time.UTC
	t.Cleanup(func() { reviewLocation = old })

	return cfgPath, cache
}

// noRequests points the client at a server that fails the test on any
// request.
func noRequests(t *testing.T) {
	t.Helper()
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
}

// deadURL points the client at a closed port, so every send is spooled.
func deadURL(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	t.Setenv(envURL, u)
	t.Setenv(envAPIKey, testKey)
}

// copyFixture copies the review fixture base into a temporary directory,
// keeping the run dir's basename (part of the key), and returns the run dir.
func copyFixture(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "base")
	if err := os.CopyFS(dst, os.DirFS(filepath.Join(reviewTestdata, "base"))); err != nil {
		t.Fatal(err)
	}

	return filepath.Join(dst, fixtureRun)
}

// decodeLine decodes one JSON line keeping number spellings.
func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("not a JSON object: %q: %v", line, err)
	}

	return m
}

func outcomeOf(t *testing.T, r result) map[string]any {
	t.Helper()

	return decodeLine(t, lastLine(r.stdout))
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "af1-") {
			out = append(out, e.Name())
		}
	}

	return out
}

func TestAC1_SubmitFlagsParse(t *testing.T) {
	isolateSubmit(t)
	noRequests(t)

	r := runCLI(t, "", "submit", "friction", "--summary", "s", "--category", "tooling", "--details", "d",
		"--suggested-fix", "f", "--fix-status", "proposed", "--fix-ref", "repo@abc", "--severity", "high",
		"--project", "p", "--harness", "h", "--model", "m", "--machine", "mc", "--key", "k1", "--schema-version", "1", "--dry-run")
	if r.code != 0 || outcomeOf(t, r)["outcome"] != "valid" {
		t.Fatalf("friction %+v", r)
	}
	body := decodeLine(t, strings.Split(r.stdout, "\n")[0])
	payload, _ := body["payload"].(map[string]any)
	want := map[string]any{"kind": "friction", "summary": "s", "project": "p", "harness": "h", "model": "m", "machine": "mc",
		"key": "k1", "schema_version": json.Number("1")}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
	wantPayload := map[string]any{"category": "tooling", "details": "d", "suggested_fix": "f", "fix_status": "proposed", "fix_ref": "repo@abc", "severity": "high"}
	if !reflect.DeepEqual(payload, wantPayload) {
		t.Errorf("payload %v", payload)
	}
	if _, ok := body["occurred_at"].(string); !ok {
		t.Errorf("no occurred_at in %v", body)
	}

	// Values outside the schema's enums pass through: the server warns.
	r = runCLI(t, "", "submit", "friction", "--summary", "s", "--fix-status", "later", "--severity", "urgent", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stdout, `"fix_status":"later"`) || !strings.Contains(r.stdout, `"severity":"urgent"`) {
		t.Fatalf("enum pass-through %+v", r)
	}

	r = runCLI(t, `{"a":1}`, "submit", "note", "--stdin", "--summary", "s", "--schema-version", "2", "--key", "k2", "--dry-run")
	body = decodeLine(t, strings.Split(r.stdout, "\n")[0])
	if r.code != 0 || body["kind"] != "note" || body["summary"] != "s" || body["schema_version"] != json.Number("2") || body["key"] != "k2" || body["a"] != json.Number("1") {
		t.Fatalf("generic %+v", r)
	}

	dir := copyFixture(t)
	if r := runCLI(t, "", "submit", "review", dir, "--include-outputs", "--dry-run"); r.code != 0 || outcomeOf(t, r)["outcome"] != "valid" {
		t.Fatalf("review %+v", r)
	}
	if r := runCLI(t, "", "submit", "review", "--sweep", "--dry-run", filepath.Dir(dir)); r.code != 0 {
		t.Fatalf("sweep %+v", r)
	}

	for _, args := range [][]string{
		{"submit"}, {"submit", "--summary", "x"}, {"submit", "friction"}, {"submit", "friction", "--summary", "s", "extra"},
		{"submit", "friction", "--summary", "s", "--schema-version", "0"}, {"submit", "friction", "--summary", "s", "--schema-version", "x"},
		{"submit", "friction", "--summary", "s", "--bogus"}, {"submit", "  ", "--stdin"}, {"submit", "review"},
		{"submit", "review", "a", "b"}, {"submit", "review", "--sweep", "--include-outputs"}, {"flush", "x"},
	} {
		if r := runCLI(t, "", args...); r.code != 2 {
			t.Errorf("%v: %+v", args, r)
		}
	}
	for _, in := range []string{"", "[]", `{"a":1} {"b":2}`, `{"a":`, `"s"`} {
		if r := runCLI(t, in, "submit", "note", "--stdin", "--dry-run"); r.code != 2 || !strings.Contains(r.stderr, "exactly one JSON object") {
			t.Errorf("stdin %q: %+v", in, r)
		}
	}
	if r := runCLI(t, `{"payload":[1]}`, "submit", "friction", "--stdin", "--summary", "s", "--details", "d", "--dry-run"); r.code != 2 {
		t.Errorf("non-object payload %+v", r)
	}
}

func TestAC2_StdinMergeFlagsWinBytesExact(t *testing.T) {
	isolateSubmit(t)
	noRequests(t)
	t.Setenv(envMachine, "env-machine")
	in := `{"summary":"from stdin","kind":"note","big":12345678901234567890,"f":1.0,"x":{"n":1E+2},"details":"flat",` +
		`"machine":"stdin-machine","context":{"cwd":"/stdin","folder":"mine"},` +
		`"payload":{"keep":12345678901234567890,"g":1.0,"details":"old"}}`
	r := runCLI(t, in, "submit", "friction", "--stdin", "--summary", "from flag", "--details", "new", "--dry-run")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	line := strings.Split(r.stdout, "\n")[0]
	for _, want := range []string{`"big":12345678901234567890`, `"f":1.0`, `"x":{"n":1E+2}`, `"summary":"from flag"`, `"kind":"friction"`,
		`"machine":"stdin-machine"`, `"payload":{"keep":12345678901234567890,"g":1.0,"details":"new"}`} {
		if !strings.Contains(line, want) {
			t.Errorf("body lacks %s: %s", want, line)
		}
	}
	if strings.Contains(line, `"details":"flat"`) || strings.Contains(line, "from stdin") {
		t.Errorf("stdin value survived a flag: %s", line)
	}
	ctx, _ := decodeLine(t, line)["context"].(map[string]any)
	if ctx["cwd"] != "/stdin" || ctx["folder"] != "mine" || ctx["os"] == nil {
		t.Errorf("context %v", ctx)
	}

	// Env fills a member stdin lacks; a flag beats both.
	r = runCLI(t, `{"summary":"s"}`, "submit", "friction", "--stdin", "--dry-run")
	if body := decodeLine(t, strings.Split(r.stdout, "\n")[0]); body["machine"] != "env-machine" {
		t.Errorf("env machine: %v", body["machine"])
	}
	r = runCLI(t, `{"summary":"s","machine":"m"}`, "submit", "friction", "--stdin", "--machine", "flag", "--dry-run")
	if body := decodeLine(t, strings.Split(r.stdout, "\n")[0]); body["machine"] != "flag" {
		t.Errorf("flag machine: %v", body["machine"])
	}

	// The server stores the large integer unchanged.
	l := liveServer(t)
	r = runCLI(t, in, "submit", "friction", "--stdin", "--details", "new")
	o := outcomeOf(t, r)
	if r.code != 0 || o["outcome"] != "submitted" {
		t.Fatalf("live %+v", r)
	}
	_, raw := l.call(t, http.MethodGet, "/api/v1/submissions/1", "")
	if !bytes.Contains(raw, []byte("12345678901234567890")) {
		t.Errorf("stored %s", raw)
	}
}

func TestAC3_DryRunSendsNothing(t *testing.T) {
	_, cache := isolateSubmit(t)
	r := runCLI(t, "", "submit", "friction", "--summary", "dry", "--dry-run")
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	o := outcomeOf(t, r)
	if r.code != 0 || len(lines) != 2 || o["outcome"] != "valid" || o["kind"] != "friction" || o["key"] == "" {
		t.Fatalf("unset URL and key %+v", r)
	}
	if body := decodeLine(t, lines[0]); body["key"] != o["key"] || body["summary"] != "dry" {
		t.Fatalf("body %s", lines[0])
	}
	// Local envelope warnings go to stderr.
	r = runCLI(t, `{"payload":{"fix_status":"later"}}`, "submit", "friction", "--stdin", "--summary", "s", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stderr, "warning:") || !strings.Contains(r.stderr, "/payload/fix_status") {
		t.Fatalf("warnings %+v", r)
	}

	noRequests(t)
	if r := runCLI(t, "", "submit", "friction", "--summary", "dry", "--dry-run"); r.code != 0 || outcomeOf(t, r)["outcome"] != "valid" {
		t.Fatalf("with server %+v", r)
	}
	if _, err := os.Stat(client.LogPath(cache)); !os.IsNotExist(err) {
		t.Errorf("dry run logged: %v", err)
	}
}

func TestAC4_ReviewGolden(t *testing.T) {
	isolateSubmit(t)
	noRequests(t)
	for _, tc := range []struct {
		golden, v3 string
		args       []string
	}{
		{"20260730-101010-4242.v1.json", "20260730-101010-4242.v3.json", nil},
		{"20260730-101010-4242.outputs.v1.json", "20260730-101010-4242.outputs.v3.json", []string{"--include-outputs"}},
	} {
		dir := copyFixture(t)
		r := runCLI(t, "", append([]string{"submit", "review", dir, "--dry-run"}, tc.args...)...)
		if r.code != 0 || outcomeOf(t, r)["outcome"] != "valid" || !strings.Contains(r.stderr, `non-numeric bytes "12x" for slot glm`) {
			t.Fatalf("%s: %+v", tc.golden, r)
		}
		got := decodeLine(t, strings.Split(r.stdout, "\n")[0])
		ctx, _ := got["context"].(map[string]any)
		if len(ctx) != 3 || ctx["os"] == nil || ctx["arch"] == nil || ctx["client"] == nil {
			t.Errorf("context %v", ctx)
		}
		delete(got, "context")
		raw, err := os.ReadFile(filepath.Join(reviewTestdata, tc.golden))
		if err != nil {
			t.Fatal(err)
		}
		if want := decodeLine(t, string(raw)); !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.Marshal(got)
			t.Errorf("%s differs:\n got %s\nwant %s", tc.golden, gotJSON, bytes.TrimSpace(raw))
		}

		raw, err = os.ReadFile(filepath.Join(reviewTestdata, tc.v3))
		if err != nil {
			t.Fatal(err)
		}
		v3 := decodeLine(t, string(raw))
		payload, _ := got["payload"].(map[string]any)
		grader, _ := payload["grader"].(map[string]any)
		checks := []struct {
			name      string
			v3, v1    any
			v1Present bool
		}{
			{"reviewers", v3["reviewers"], payload["reviewers"], true},
			{"prompt", v3["prompt"], payload["prompt"], true},
			{"skill", v3["skill"], payload["skill"], true},
			{"machine", v3["machine_name"], got["machine"], true},
			{"model", v3["coordinator_model"], got["model"], true},
			{"grader.model", v3["coordinator_model"], grader["model"], true},
			{"key", fmt.Sprint(v3["skill"], "-", v3["run_id"]), got["key"], true},
		}
		for _, c := range checks {
			if !reflect.DeepEqual(c.v3, c.v1) {
				t.Errorf("%s: v3 %v, v1 %v", c.name, c.v3, c.v1)
			}
		}
		if got["occurred_at"] != "2026-07-30T10:10:10.000000Z" || got["summary"] != "review-panel: 3 reviewers, 2 completed, mean 4.0/5" {
			t.Errorf("occurred_at %v summary %v", got["occurred_at"], got["summary"])
		}
	}
}

func TestAC4_ReviewMarkerAfterOutcome(t *testing.T) {
	isolateSubmit(t)
	dir := copyFixture(t)
	marker := filepath.Join(dir, submittedMarker)
	markerID := func() string { b, _ := os.ReadFile(marker); return strings.TrimSpace(string(b)) }

	deadURL(t)
	if r := runCLI(t, "", "submit", "review", dir); r.code != 0 || outcomeOf(t, r)["outcome"] != "spooled" || markerID() != "" {
		t.Fatalf("spooled %+v marker %q", r, markerID())
	}

	l := liveServer(t)
	r := runCLI(t, "", "submit", "review", dir)
	o := outcomeOf(t, r)
	if r.code != 0 || o["outcome"] != "submitted" || o["key"] != "review-panel-testmach-"+fixtureRun || markerID() != "1" {
		t.Fatalf("submitted %+v marker %q", r, markerID())
	}
	rec := l.record(t, 1)
	if rec["kind"] != "review" || rec["machine"] != "testmach" || rec["model"] != "claude-fable-5" || rec["harness"] != nil || rec["project"] != nil {
		t.Fatalf("record %v", rec)
	}
	_ = os.Remove(marker)
	if r := runCLI(t, "", "submit", "review", dir); r.code != 0 || outcomeOf(t, r)["outcome"] != "duplicate" || markerID() != "1" {
		t.Fatalf("duplicate %+v", r)
	}

	_ = os.Remove(marker)
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "scorecards.tsv"),
		[]byte("20260730-101010\tx\tGPT-5.6-Sol\t4\t2\t0\tchanged\n20260730-101010\tx\tGLM\t3\t\t\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A mismatch marks the run too: the server holds a submission under
	// the key.
	if r := runCLI(t, "", "submit", "review", dir); r.code != 1 || outcomeOf(t, r)["outcome"] != "mismatch" || markerID() != "mismatch" {
		t.Fatalf("mismatch %+v marker %q", r, markerID())
	}
}

func TestReview_MarkerReplacesSymlink(t *testing.T) {
	isolateSubmit(t)
	dir := copyFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, outside, "keep\n", 0o600)
	marker := filepath.Join(dir, submittedMarker)
	if err := os.Symlink(outside, marker); err != nil {
		t.Fatal(err)
	}
	liveServer(t)
	if r := runCLI(t, "", "submit", "review", dir); r.code != 0 || outcomeOf(t, r)["outcome"] != "submitted" {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep\n" {
		t.Errorf("outside file changed: %q", b)
	}
	if st, err := os.Lstat(marker); err != nil || !st.Mode().IsRegular() {
		t.Errorf("marker %v %v", st, err)
	}
	if b, _ := os.ReadFile(marker); string(b) != "1\n" {
		t.Errorf("marker %q", b)
	}
}

func TestReview_RelativeDirSameKey(t *testing.T) {
	isolateSubmit(t)
	noRequests(t)
	dir := copyFixture(t)
	abs := runCLI(t, "", "submit", "review", dir, "--dry-run")
	t.Chdir(dir)
	rel := runCLI(t, "", "submit", "review", ".", "--dry-run")
	if abs.code != 0 || rel.code != 0 || outcomeOf(t, rel)["key"] != outcomeOf(t, abs)["key"] ||
		outcomeOf(t, rel)["key"] != "review-panel-testmach-"+fixtureRun {
		t.Fatalf("abs %+v rel %+v", abs, rel)
	}
	if strings.Split(abs.stdout, "\n")[0] != strings.Split(rel.stdout, "\n")[0] {
		t.Errorf("bodies differ:\n%s\n%s", abs.stdout, rel.stdout)
	}
}

func TestReview_CRLFFiles(t *testing.T) {
	isolateSubmit(t)
	noRequests(t)
	dir := copyFixture(t)
	want := runCLI(t, "", "submit", "review", dir, "--dry-run")
	for _, path := range []string{filepath.Join(dir, "summary.tsv"), filepath.Join(filepath.Dir(dir), "scorecards.tsv")} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, strings.ReplaceAll(string(b), "\n", "\r\n"), 0o600)
	}
	got := runCLI(t, "", "submit", "review", dir, "--dry-run")
	if want.code != 0 || got.code != 0 || strings.Split(got.stdout, "\n")[0] != strings.Split(want.stdout, "\n")[0] {
		t.Fatalf("CRLF\n got %+v\nwant %+v", got, want)
	}
}

func TestReview_OutputReads(t *testing.T) {
	isolateSubmit(t)
	noRequests(t)
	dir := copyFixture(t)
	// A missing prompt is absent.
	if err := os.Remove(filepath.Join(dir, "prompt.md")); err != nil {
		t.Fatal(err)
	}
	if r := runCLI(t, "", "submit", "review", dir, "--include-outputs", "--dry-run"); r.code != 0 || strings.Contains(strings.Split(r.stdout, "\n")[0], `"prompt"`) {
		t.Fatalf("missing prompt %+v", r)
	}
	// Unreadable: a directory where the prompt should be.
	if err := os.Mkdir(filepath.Join(dir, "prompt.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, "", "submit", "review", dir, "--include-outputs", "--dry-run")
	if o := outcomeOf(t, r); r.code != 1 || o["reason"] != "unreadable_output" || !strings.Contains(o["message"].(string), "prompt.md") {
		t.Fatalf("unreadable %+v", r)
	}
	if err := os.Remove(filepath.Join(dir, "prompt.md")); err != nil {
		t.Fatal(err)
	}
	// Over the limit: a sparse file is stat-ed, never read.
	f, err := os.Create(filepath.Join(dir, "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(11 << 20); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	r = runCLI(t, "", "submit", "review", dir, "--include-outputs", "--dry-run")
	if o := outcomeOf(t, r); r.code != 1 || o["reason"] != "body_too_large" || !strings.Contains(o["message"].(string), "--include-outputs") {
		t.Fatalf("too large %+v", r)
	}
}

func TestReview_ContextDrop(t *testing.T) {
	cfgPath, _ := isolateSubmit(t)
	noRequests(t)
	writeFile(t, cfgPath, "[context]\ndrop = [\"client\"]\n", 0o600)
	dir := copyFixture(t)
	r := runCLI(t, "", "submit", "review", dir, "--dry-run")
	ctx, _ := decodeLine(t, strings.Split(r.stdout, "\n")[0])["context"].(map[string]any)
	if r.code != 0 || ctx["client"] != nil || ctx["os"] == nil || ctx["arch"] == nil {
		t.Fatalf("context %v %+v", ctx, r)
	}
}

func TestReview_SweepSkipsMismatchAndWarnsFlush(t *testing.T) {
	isolateSubmit(t)
	fixClock(t, time.Date(2020, 1, 1, 3, 0, 0, 0, time.UTC))
	base := t.TempDir()
	run := writeRun(t, base, "20200101-000000-run", "20200101-000000", "One", "5")
	liveServer(t)
	if r := runCLI(t, "", "submit", "review", "--sweep", base); outcomeOf(t, r)["outcome"] != "submitted" {
		t.Fatalf("first %+v", r)
	}
	_ = os.Remove(filepath.Join(run, submittedMarker))
	writeFile(t, filepath.Join(base, "scorecards.tsv"), "20200101-000000\tx\tOne\t3\t1\t0\tchanged\n", 0o600)
	if r := runCLI(t, "", "submit", "review", "--sweep", base); outcomeOf(t, r)["outcome"] != "mismatch" {
		t.Fatalf("mismatch %+v", r)
	}
	if r := runCLI(t, "", "submit", "review", "--sweep", base); r.code != 0 || r.stdout != "" {
		t.Fatalf("re-sent after mismatch %+v", r)
	}

	// A flush that rejects warns once with the counts.
	deadURL(t)
	if r := runCLI(t, "", "submit", "note", "--key", "c", "--summary", "c"); outcomeOf(t, r)["outcome"] != "spooled" {
		t.Fatalf("spool %+v", r)
	}
	fixClock(t, time.Date(2020, 1, 1, 5, 0, 0, 0, time.UTC))
	var hits atomic.Int64
	flushServer(t, &hits)
	r := runCLI(t, "", "submit", "review", "--sweep", t.TempDir())
	if r.code != 0 || strings.Count(r.stderr, "spool flush:") != 1 ||
		!strings.Contains(r.stderr, "spool flush: flushed 0, pending 0, rejected 1, mismatched 0") {
		t.Fatalf("flush warning %+v", r)
	}
}

// writeRun writes one run dir with a single completed reviewer; score ""
// leaves the scorecard out.
func writeRun(t *testing.T, base, name, runTS, label, score string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	writeFile(t, filepath.Join(dir, "meta.json"), fmt.Sprintf(`{"machine":"testmach","skill":"review-panel","run_ts":%q,"caller":"caller","slots":{"one":{"label":%q}}}`, runTS, label), 0o600)
	writeFile(t, filepath.Join(dir, "summary.tsv"), "slot\tmodel\tstatus\tduration_s\tbytes\none\tmodel/one\tcompleted\t1\t2\n", 0o600)
	if score != "" {
		f, err := os.OpenFile(filepath.Join(base, "scorecards.tsv"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(f, "%s\tx\t%s\t%s\t1\t0\tok\n", runTS, label, score)
		_ = f.Close()
	}

	return dir
}

func TestAC4_ReviewDirectSkips(t *testing.T) {
	_, cache := isolateSubmit(t)
	noRequests(t)
	base := t.TempDir()
	pending := writeRun(t, base, "20200101-000000-pending", "20200101-000000", "Pending", "PENDING")
	ambigA := writeRun(t, base, "20200101-000002-a", "20200101-000002", "Shared", "5")
	writeRun(t, base, "20200101-000002-b", "20200101-000002", "Shared2", "")
	noMeta := filepath.Join(base, "20200101-000009-nometa")
	writeFile(t, filepath.Join(noMeta, "summary.tsv"), "slot\n", 0o600)
	missing := writeRun(t, base, "20200101-000004-missing", "20200101-000004", "Missing", "")

	for dir, reason := range map[string]string{pending: "incomplete_scorecard", ambigA: "ambiguous_timestamp", noMeta: "no_meta", missing: "incomplete_scorecard"} {
		r := runCLI(t, "", "submit", "review", dir)
		o := outcomeOf(t, r)
		if r.code != 1 || o["outcome"] != "rejected" || o["reason"] != reason {
			t.Errorf("%s: %+v", filepath.Base(dir), r)
		}
		if _, err := os.Stat(filepath.Join(dir, submittedMarker)); err == nil {
			t.Errorf("%s: marker written", dir)
		}
	}
	if data, _ := os.ReadFile(client.LogPath(cache)); strings.Count(string(data), `"outcome":"rejected"`) != 4 {
		t.Errorf("log %s", data)
	}
}

func TestAC4_ReviewSweep(t *testing.T) {
	_, cache := isolateSubmit(t)
	fixClock(t, time.Date(2020, 1, 1, 3, 0, 0, 0, time.UTC))
	base := t.TempDir()
	old := writeRun(t, base, "20200101-000000-old", "20200101-000000", "Old", "5")
	young := writeRun(t, base, "20200101-013000-young", "20200101-013000", "Young", "5")
	marked := writeRun(t, base, "20200101-000100-marked", "20200101-000100", "Marked", "5")
	writeFile(t, filepath.Join(marked, submittedMarker), "9\n", 0o600)
	pending := writeRun(t, base, "20200101-000200-pending", "20200101-000200", "Pending", "PENDING")
	if err := os.MkdirAll(filepath.Join(base, "no-meta"), 0o700); err != nil {
		t.Fatal(err)
	}

	// No bases: a warning, and the spool is still flushed first.
	deadURL(t)
	fixClock(t, time.Date(2020, 1, 1, 1, 0, 0, 0, time.UTC))
	if r := runCLI(t, "", "submit", "friction", "--summary", "spooled first"); outcomeOf(t, r)["outcome"] != "spooled" {
		t.Fatalf("spool %+v", r)
	}
	fixClock(t, time.Date(2020, 1, 1, 3, 0, 0, 0, time.UTC))
	l := liveServer(t)
	r := runCLI(t, "", "submit", "review", "--sweep")
	if r.code != 0 || !strings.Contains(r.stderr, "no review run directories configured") || r.stdout != "" {
		t.Fatalf("no bases %+v", r)
	}
	if rec := l.record(t, 1); rec["summary"] != "spooled first" || len(spoolFiles(t, client.SpoolDir(cache))) != 0 {
		t.Fatalf("not flushed first: %v", rec)
	}

	// Configured bases from both variables.
	t.Setenv(envReviewLogDir, filepath.Join(t.TempDir(), "absent"))
	t.Setenv(envReviewDirs, ":"+base)
	r = runCLI(t, "", "submit", "review", "--sweep")
	if r.code != 0 || strings.Count(r.stdout, "\n") != 1 || outcomeOf(t, r)["outcome"] != "submitted" ||
		outcomeOf(t, r)["key"] != "review-panel-testmach-20200101-000000-old" || !strings.Contains(r.stderr, "incomplete scorecard") {
		t.Fatalf("sweep %+v", r)
	}
	for dir, want := range map[string]bool{old: true, young: false, pending: false} {
		if _, err := os.Stat(filepath.Join(dir, submittedMarker)); (err == nil) != want {
			t.Errorf("%s marker %v", filepath.Base(dir), err == nil)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(marked, submittedMarker)); string(b) != "9\n" {
		t.Errorf("marked dir touched: %q", b)
	}

	// Positional bases replace the configured ones; the clock moves on.
	fixClock(t, time.Date(2020, 1, 1, 4, 0, 0, 0, time.UTC))
	t.Setenv(envReviewDirs, filepath.Join(t.TempDir(), "other"))
	r = runCLI(t, "", "submit", "review", "--sweep", base)
	if r.code != 0 || outcomeOf(t, r)["key"] != "review-panel-testmach-20200101-013000-young" {
		t.Fatalf("positional %+v", r)
	}
	if r := runCLI(t, "", "submit", "review", "--sweep", "--include-outputs"); r.code != 2 {
		t.Fatalf("include-outputs %+v", r)
	}
}

// flushServer answers by key: a 201, b 200, c 422.
func flushServer(t *testing.T, hits *atomic.Int64) {
	t.Helper()
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		data, _ := io.ReadAll(r.Body)
		var h struct{ Kind, Key string }
		_ = json.Unmarshal(data, &h)
		w.Header().Set("Content-Type", "application/json")
		switch h.Key {
		case "a", "b":
			status := map[string]int{"a": http.StatusCreated, "b": http.StatusOK}[h.Key]
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"submission":{"id":%d,"kind":%q,"key":%q},"warnings":[]}`, status, h.Kind, h.Key)
		default:
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"error":"x","message":"no","request_id":"r"}`)
		}
	})
}

func TestAC5_Flush(t *testing.T) {
	_, cache := isolateSubmit(t)
	noRequests(t)
	r := runCLI(t, "", "flush")
	if r.code != 0 || lastLine(r.stdout) != `{"flushed":0,"duplicates":0,"pending":0,"rejected":0,"mismatched":0,"expired":0,"deferred":0}` {
		t.Fatalf("empty %+v", r)
	}

	deadURL(t)
	for _, k := range []string{"a", "b", "c"} {
		if r := runCLI(t, "", "submit", "note", "--key", k, "--summary", k); outcomeOf(t, r)["outcome"] != "spooled" {
			t.Fatalf("spool %s %+v", k, r)
		}
	}
	spool := client.SpoolDir(cache)
	if n := len(spoolFiles(t, spool)); n != 3 {
		t.Fatalf("spool holds %d", n)
	}
	// Spooled entries wait out their backoff.
	fixClock(t, time.Now().Add(time.Hour))
	var hits atomic.Int64
	flushServer(t, &hits)
	r = runCLI(t, "", "flush")
	if r.code != 0 || hits.Load() != 3 {
		t.Fatalf("flush %+v hits %d", r, hits.Load())
	}
	rep := outcomeOf(t, r)
	if rep["flushed"] != json.Number("2") || rep["duplicates"] != json.Number("1") || rep["rejected"] != json.Number("1") {
		t.Fatalf("report %v", rep)
	}
	if n := len(spoolFiles(t, spool)); n != 0 {
		t.Errorf("spool left %d", n)
	}
	if n := len(spoolFiles(t, client.RejectedDir(cache))); n != 1 {
		t.Errorf("rejected holds %d", n)
	}

	isolate(t)
	if r := runCLI(t, "", "flush"); r.code != 1 {
		t.Errorf("unconfigured flush %+v", r)
	}
}

func TestNarrowing_DisabledSubmitAndFlush(t *testing.T) {
	cfgPath, cache := isolateSubmit(t)
	noRequests(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfgPath, fmt.Sprintf("[collect]\ndeny_paths = [%q]\n", dir), 0o600)
	for _, tc := range []struct {
		stdin string
		args  []string
	}{
		{"", []string{"submit", "friction", "--summary", "x"}}, {"", []string{"flush"}}, {"", []string{"submit", "review", "--sweep"}},
		// The gate comes before stdin and the summary check.
		{`{"a":`, []string{"submit", "note", "--stdin"}}, {"", []string{"submit", "friction"}},
	} {
		args := tc.args
		r := runCLI(t, tc.stdin, args...)
		o := outcomeOf(t, r)
		if r.code != 0 || o["outcome"] != "disabled" || o["reason"] != "deny_paths" {
			t.Errorf("%v: %+v", args, r)
		}
	}
	data, err := os.ReadFile(client.LogPath(cache))
	if err != nil || strings.Count(string(data), `"outcome":"disabled"`) != 5 {
		t.Errorf("log %s %v", data, err)
	}
}

func TestBodyTooLarge(t *testing.T) {
	isolateSubmit(t)
	noRequests(t)
	big := strings.Repeat("x", 10<<20)
	r := runCLI(t, "", "submit", "friction", "--summary", "s", "--details", big)
	if o := outcomeOf(t, r); r.code != 1 || o["outcome"] != "rejected" || o["reason"] != "body_too_large" {
		t.Fatalf("flags %+v", o)
	}
	r = runCLI(t, `{"summary":"`+big+`"}`, "submit", "note", "--stdin")
	if o := outcomeOf(t, r); r.code != 1 || o["outcome"] != "rejected" || o["reason"] != "body_too_large" {
		t.Fatalf("stdin %+v", o)
	}
}

func TestSubmit_WorkdirGone(t *testing.T) {
	isolateSubmit(t)
	noRequests(t)
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Getwd(); err == nil {
		t.Skip("os.Getwd succeeds in a removed directory here")
	}
	r := runCLI(t, "", "submit", "friction", "--summary", "s", "--dry-run")
	if r.code != 1 || !strings.Contains(r.stderr, "cannot resolve the working directory") || r.stdout != "" {
		t.Fatalf("%+v", r)
	}
}
