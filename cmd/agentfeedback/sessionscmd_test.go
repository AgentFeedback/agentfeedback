package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
)

// fixtureUID is a submission uid in the form the API issues.
const fixtureUID = "01928c4e-7d2a-7b3c-8d4e-5f6a7b8c9d0e"

// ccFixture is a hand-written Claude Code session: a prompt, a failing
// Bash call and its result. {{SID}} and {{CWD}} are filled in.
const ccFixture = `{"type":"user","uuid":"u-1","timestamp":"2026-10-08T07:00:00.000Z","sessionId":"{{SID}}","cwd":"{{CWD}}","message":{"role":"user","content":"The deploy is still failing"}}
{"type":"assistant","uuid":"a-2","timestamp":"2026-10-08T07:00:05.000Z","sessionId":"{{SID}}","cwd":"{{CWD}}","message":{"model":"claude-fixture-9","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make deploy"}}]}}
{"type":"user","uuid":"u-3","timestamp":"2026-10-08T07:00:09.250Z","sessionId":"{{SID}}","cwd":"{{CWD}}","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"Exit code 2\nboom"}]}}
`

// sessionsWorld isolates the CLI with its own Claude Code configuration
// directory and returns it.
func sessionsWorld(t *testing.T) string {
	t.Helper()
	isolateCLI(t)
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	return cfg
}

// putSession writes the fixture as session sid launched in cwd.
func putSession(t *testing.T, cfg, sid, cwd string) {
	t.Helper()
	enc := []byte(cwd)
	for i, c := range enc {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			enc[i] = '-'
		}
	}
	dir := filepath.Join(cfg, "projects", string(enc))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReplacer("{{SID}}", sid, "{{CWD}}", cwd).Replace(ccFixture)
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeClientConfig(t *testing.T, body string) {
	t.Helper()
	p, err := configPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("%v: %q", err, s)
	}
}

func TestSessions_ListDigestMarkStatus(t *testing.T) {
	cfg := sessionsWorld(t)
	cwd := filepath.Join(t.TempDir(), "proj")
	putSession(t, cfg, "s1", cwd)

	r := runCLI(t, "", "sessions", "list", "--json")
	var l sessions.Listing
	decodeJSON(t, r.stdout, &l)
	if r.code != 0 || len(l.Sessions) != 1 || l.Sessions[0].Ref != "claude-code:s1" || l.Sessions[0].State != sessions.StateNew ||
		len(l.Stores) != 1 || l.Stores[0].Location != filepath.Join(cfg, "projects") {
		t.Fatalf("list --json: %+v", r)
	}
	if r := runCLI(t, "", "sessions", "list"); r.code != 0 || !strings.Contains(r.stdout, "claude-code:s1") || !strings.Contains(r.stdout, "new") {
		t.Fatalf("list: %+v", r)
	}
	if r := runCLI(t, "", "sessions", "mark", "claude-code:s1", "--outcome", "nothing"); r.code != 1 || !strings.Contains(r.stderr, "sessions digest") {
		t.Fatalf("mark before digest: %+v", r)
	}

	r = runCLI(t, "", "sessions", "digest", "claude-code:s1", "--json")
	var d sessions.DigestOutput
	decodeJSON(t, r.stdout, &d)
	if r.code != 0 || len(d.Sessions) != 1 || d.Notice != sessions.Notice || d.DetectorVersion != sessions.DetectorVersion {
		t.Fatalf("digest --json: %+v", r)
	}
	if r := runCLI(t, "", "sessions", "digest", "claude-code:s1"); r.code != 0 || !strings.HasPrefix(r.stdout, sessions.Notice) || !strings.Contains(r.stdout, "#a-2") {
		t.Fatalf("digest: %+v", r)
	}

	r = runCLI(t, "", "sessions", "mark", "claude-code:s1", "--outcome", "filed", "--ref", fixtureUID, "--json")
	var m markResult
	decodeJSON(t, r.stdout, &m)
	if r.code != 0 || m.Outcome != "filed" || len(m.UIDs) != 1 || m.UIDs[0] != fixtureUID {
		t.Fatalf("mark: %+v", r)
	}
	r = runCLI(t, "", "sessions", "list", "--unprocessed", "--json")
	decodeJSON(t, r.stdout, &l)
	if r.code != 0 || len(l.Sessions) != 0 {
		t.Fatalf("list --unprocessed after mark: %+v", r)
	}
	r = runCLI(t, "", "sessions", "digest", "--unprocessed", "--json")
	decodeJSON(t, r.stdout, &d)
	if r.code != 0 || len(d.Sessions) != 0 || len(d.Errors) != 0 {
		t.Fatalf("digest --unprocessed with nothing to do: %+v", r)
	}

	if r := runCLI(t, "", "sessions", "status", "--set-selection", "--harness", "claude-code", "--since", "7d", "--limit", "5", "--json"); r.code != 0 {
		t.Fatalf("status --set-selection: %+v", r)
	}
	r = runCLI(t, "", "sessions", "status", "--json")
	var st sessions.StatusOutput
	decodeJSON(t, r.stdout, &st)
	if r.code != 0 || st.Selection == nil || st.Selection.Since != "7d" || st.Selection.Limit != 5 || st.Harnesses[0].States[sessions.StateProcessed] != 1 {
		t.Fatalf("status: %+v", r)
	}

	for _, args := range [][]string{
		{"sessions"}, {"sessions", "nope"}, {"sessions", "digest"}, {"sessions", "digest", "claude-code:s1", "--unprocessed"},
		{"sessions", "digest", "claude-code:s1", "--limit", "2"}, {"sessions", "digest", "codex:s1"},
		{"sessions", "mark", "claude-code:s1", "--outcome", "maybe"}, {"sessions", "list", "--harness", "codex"},
		{"sessions", "mark", "claude-code:s1", "--outcome", "filed", "--ref", "uid-1"},
		{"sessions", "list", "--since", "yesterday"}, {"sessions", "status", "--limit", "3"},
	} {
		if r := runCLI(t, "", args...); r.code != 2 {
			t.Errorf("%v: %+v", args, r)
		}
	}
}

// TestSessions_DeniedAndRemote: a denied session is listed unread and its
// digest fails; with a server URL configured the commands still work on the
// data-directory database and never contact the server.
func TestSessions_DeniedAndRemote(t *testing.T) {
	cfg := sessionsWorld(t)
	cwd := filepath.Join(t.TempDir(), "secret")
	putSession(t, cfg, "s1", cwd)
	writeClientConfig(t, "url = \"http://127.0.0.1:1\"\napi_key = \"k\"\n[collect]\ndeny_paths = ["+string(mustJSONString(cwd))+"]\n")

	r := runCLI(t, "", "sessions", "list", "--json")
	var l sessions.Listing
	decodeJSON(t, r.stdout, &l)
	if r.code != 0 || len(l.Sessions) != 1 || l.Sessions[0].State != sessions.StateDenied || l.Sessions[0].Counts != nil || r.stderr != "" {
		t.Fatalf("list: %+v", r)
	}
	r = runCLI(t, "", "sessions", "digest", "claude-code:s1", "--json")
	var d sessions.DigestOutput
	decodeJSON(t, r.stdout, &d)
	if r.code != 1 || len(d.Errors) != 1 || d.Errors[0].State != sessions.StateDenied {
		t.Fatalf("digest of a denied session: %+v", r)
	}
	db, err := localDBPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("the session state is not in the data-directory database: %v", err)
	}
}

// processRepo makes a directory that looks like a git repository with a
// distinctive name and remote, and makes it the working directory.
func processRepo(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "process-marker-repo")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"HEAD":   "ref: refs/heads/process-marker-branch\n",
		"config": "[remote \"origin\"]\n\turl = https://example.invalid/process-marker.git\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, ".git", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

func TestSubmit_ContextFrom(t *testing.T) {
	cfg := sessionsWorld(t)
	cwd := filepath.Join(t.TempDir(), "session-proj")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	putSession(t, cfg, "s1", cwd)
	gone := filepath.Join(t.TempDir(), "gone-proj")
	putSession(t, cfg, "s2", gone)
	processRepo(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "process-session-marker")

	stdin := `{"summary":"deploy failed","key":"mine","project":"stdin-project","occurred_at":"2020-01-01T00:00:00Z","payload":{"category":"tooling","details":"d","suggested_fix":"f"}}`
	r := runCLI(t, stdin, "submit", "friction", "--stdin", "--context-from", "claude-code:s1#u-3", "--dry-run")
	if r.code != 0 {
		t.Fatalf("dry run: %+v", r)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	var body struct {
		Key        string            `json:"key"`
		Project    string            `json:"project"`
		Harness    string            `json:"harness"`
		Model      string            `json:"model"`
		OccurredAt string            `json:"occurred_at"`
		Context    map[string]string `json:"context"`
	}
	decodeJSON(t, lines[0], &body)
	wantKey := sessions.Key("claude-code", "s1", "u-3", 1, sessions.DetectorVersion)
	if body.Key != wantKey || body.Project != "session-proj" || body.Harness != "claude-code" || body.Model != "claude-fixture-9" ||
		body.OccurredAt != "2026-10-08T07:00:09.250000Z" {
		t.Fatalf("body %s", lines[0])
	}
	for k, v := range map[string]string{"origin": "session-scan", "detector": sessions.Detector, "session_id": "s1", "session_harness": "claude-code", "folder": "session-proj"} {
		if body.Context[k] != v {
			t.Errorf("context.%s = %q, want %q (%s)", k, body.Context[k], v, lines[0])
		}
	}
	if strings.Contains(r.stdout, "process-marker") || strings.Contains(r.stderr, "process-marker") {
		t.Fatalf("the process's directory or environment leaked: %+v", r)
	}
	r = runCLI(t, stdin, "submit", "friction", "--stdin", "--context-from", "claude-code:s1#u-3", "--ordinal", "2", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stdout, sessions.Key("claude-code", "s1", "u-3", 2, sessions.DetectorVersion)) {
		t.Fatalf("ordinal 2: %+v", r)
	}

	// A working directory that is gone: its base name, nothing collected.
	r = runCLI(t, stdin, "submit", "friction", "--stdin", "--context-from", "claude-code:s2#u-1", "--dry-run")
	body.Context = nil
	decodeJSON(t, strings.Split(r.stdout, "\n")[0], &body)
	if r.code != 0 || body.Project != "gone-proj" || body.Context["folder"] != "" || body.Context["origin"] != "session-scan" {
		t.Fatalf("gone cwd: %+v", r)
	}

	// Filed twice: the replay is a duplicate; other prose under the same
	// key is the 409.
	var o struct{ Outcome, Key, Reason, Message string }
	r = runCLI(t, stdin, "submit", "friction", "--stdin", "--context-from", "claude-code:s1#u-3")
	decodeJSON(t, r.stdout, &o)
	if r.code != 0 || o.Outcome != "submitted" || o.Key != wantKey {
		t.Fatalf("first: %+v", r)
	}
	r = runCLI(t, stdin, "submit", "friction", "--stdin", "--context-from", "claude-code:s1#u-3")
	decodeJSON(t, r.stdout, &o)
	if r.code != 0 || o.Outcome != "duplicate" {
		t.Fatalf("replay: %+v", r)
	}
	other := strings.Replace(stdin, "deploy failed", "deploy failed differently", 1)
	r = runCLI(t, other, "submit", "friction", "--stdin", "--context-from", "claude-code:s1#u-3")
	decodeJSON(t, r.stdout, &o)
	if r.code == 0 || o.Outcome != "mismatch" || o.Reason != "mismatch" || !strings.Contains(o.Message, wantKey) {
		t.Fatalf("mismatch: %+v", r)
	}

	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--context-from", "claude-code:s1#u-3", "--key", "k"}, 2, "--key"},
		{[]string{"--context-from", "claude-code:s1#u-3", "--project", "p"}, 2, "--project"},
		{[]string{"--context-from", "claude-code:s1"}, 2, "names no span"},
		{[]string{"--context-from", "codex:s1#u-3"}, 2, "no session reader"},
		{[]string{"--context-from", "claude-code:s1#u-3", "--ordinal", "0"}, 2, "--ordinal"},
		{[]string{"--ordinal", "2"}, 2, "--context-from"},
		{[]string{"--context-from", "claude-code:nope#u-3"}, 1, "session not found"},
		{[]string{"--context-from", "claude-code:s1#nope"}, 1, "span not found"},
	} {
		r := runCLI(t, stdin, append([]string{"submit", "friction", "--stdin", "--dry-run"}, tt.args...)...)
		if r.code != tt.code || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("%v: %+v", tt.args, r)
		}
	}
}

func TestSubmit_ContextFromRefused(t *testing.T) {
	cfg := sessionsWorld(t)
	cwd := filepath.Join(t.TempDir(), "secret")
	putSession(t, cfg, "s1", cwd)
	// A session that records no cwd.
	dir := filepath.Join(cfg, "projects", "-nocwd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	nocwd := `{"type":"user","uuid":"u-1","timestamp":"2026-10-08T07:00:00.000Z","sessionId":"s2","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s2.jsonl"), []byte(nocwd), 0o644); err != nil {
		t.Fatal(err)
	}
	writeClientConfig(t, "[collect]\ndeny_paths = ["+string(mustJSONString(cwd))+"]\n")
	stdin := `{"summary":"x","payload":{"category":"tooling","details":"d","suggested_fix":"f"}}`

	r := runCLI(t, stdin, "submit", "friction", "--stdin", "--dry-run", "--context-from", "claude-code:s1#u-3")
	if r.code != 1 || !strings.Contains(r.stderr, "denied") || r.stdout != "" {
		t.Fatalf("denied: %+v", r)
	}
	r = runCLI(t, stdin, "submit", "friction", "--stdin", "--dry-run", "--context-from", "claude-code:s2#u-1")
	if r.code != 1 || !strings.Contains(r.stderr, "--allow-unknown-project") {
		t.Fatalf("unknown project: %+v", r)
	}
	r = runCLI(t, stdin, "submit", "friction", "--stdin", "--dry-run", "--context-from", "claude-code:s2#u-1", "--allow-unknown-project")
	if r.code != 0 || !strings.Contains(r.stdout, `"session_id":"s2"`) {
		t.Fatalf("unknown project allowed: %+v", r)
	}
}

// TestSessions_MarkToolRejectsUID: the sessions_mark tool refuses a uid
// that is not a submission uid with a validation problem on uids.
func TestSessions_MarkToolRejectsUID(t *testing.T) {
	sessionsWorld(t)
	svc, err := newSessionsService(os.Getenv, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.db.Close() })
	_, err = svc.Mark(context.Background(), mcp.SessionsMarkArgs{Refs: []string{"claude-code:s1"}, Outcome: "filed", UIDs: []string{"uid-1"}})
	var p *core.Problem
	if !errors.As(err, &p) || p.Code != core.CodeValidation || len(p.Details) != 1 || p.Details[0].Pointer != "?uids" {
		t.Fatalf("got %v", err)
	}
}

// putRaw writes body as session sid in a project directory named dir.
func putRaw(t *testing.T, cfg, dir, sid, body string) {
	t.Helper()
	p := filepath.Join(cfg, "projects", dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, sid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSubmit_ContextFromOwnsModelTimeAndScrub: the model comes from the
// session alone, a span without a time is refused, and the body is always
// scrubbed, whatever origin --context sets.
func TestSubmit_ContextFromOwnsModelTimeAndScrub(t *testing.T) {
	cfg := sessionsWorld(t)
	cwd := filepath.Join(t.TempDir(), "proj")
	putSession(t, cfg, "s1", cwd)
	t.Setenv("AGENT_FEEDBACK_MODEL", "env-model-marker")
	stdin := `{"summary":"x","model":"stdin-model-marker","payload":{"category":"tooling","details":"token ghp_abcdefghijklmnopqrstuvwxyz0123456789","suggested_fix":"f"}}`

	// u-1 comes before any assistant entry: the session names no model.
	r := runCLI(t, stdin, "submit", "friction", "--stdin", "--dry-run", "--context-from", "claude-code:s1#u-1")
	if r.code != 0 {
		t.Fatalf("dry run: %+v", r)
	}
	body := strings.Split(r.stdout, "\n")[0]
	var obj map[string]any
	decodeJSON(t, body, &obj)
	if _, ok := obj["model"]; ok {
		t.Fatalf("model set without one in the session: %s", body)
	}

	r = runCLI(t, stdin, "submit", "friction", "--stdin", "--dry-run", "--context-from", "claude-code:s1#u-3", "--context", "origin=manual")
	body = strings.Split(r.stdout, "\n")[0]
	if r.code != 0 || strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") || !strings.Contains(body, "[REDACTED:github_token]") ||
		!strings.Contains(body, `"origin":"manual"`) {
		t.Fatalf("scrub under another origin: %+v", r)
	}

	untimed := `{"type":"user","uuid":"u-1","sessionId":"s2","cwd":` + string(mustJSONString(cwd)) + `,"message":{"role":"user","content":"hi"}}` + "\n"
	putRaw(t, cfg, "-untimed", "s2", untimed)
	r = runCLI(t, stdin, "submit", "friction", "--stdin", "--dry-run", "--context-from", "claude-code:s2#u-1")
	if r.code != 1 || !strings.Contains(r.stderr, "records no time") || r.stdout != "" {
		t.Fatalf("span without a time: %+v", r)
	}
}
