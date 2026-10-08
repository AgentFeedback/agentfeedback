package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/detect"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// ccFailure is a Claude Code PostToolUseFailure payload of a Bash command.
func ccFailure(session, cwd, command string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": session, "transcript_path": "/nonexistent/t.jsonl", "cwd": cwd, "hook_event_name": "PostToolUseFailure",
		"tool_name": "Bash", "tool_input": map[string]string{"command": command}, "error": "Exit code 1\nboom", "is_interrupt": false,
	})

	return string(b)
}

func ccStop(session, cwd string) string {
	b, _ := json.Marshal(map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop", "stop_hook_active": false})

	return string(b)
}

// noteOf is the additionalContext of a Claude Code hook output, or "".
func noteOf(t *testing.T, stdout string) string {
	t.Helper()
	if stdout == "" {
		return ""
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.HookSpecificOutput.HookEventName != "PostToolUseFailure" {
		t.Fatalf("output %q: %v", stdout, err)
	}

	return out.HookSpecificOutput.AdditionalContext
}

func TestHook_ClaudeCode(t *testing.T) {
	_, cache := isolate(t)
	cwd := t.TempDir()
	r := runCLI(t, ccFailure("sess-1", cwd, "npm test"), "hook", "claude-code", "PostToolUseFailure")
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("first failure %+v", r)
	}
	r = runCLI(t, ccFailure("sess-1", cwd, "npm  test"), "hook", "claude-code", "PostToolUseFailure")
	note := noteOf(t, r.stdout)
	if r.code != 0 || r.stderr != "" || !strings.Contains(note, "agentfeedback submit friction") ||
		!strings.Contains(note, "--context session_id=sess-1 --context session_harness=claude-code") || len(note) >= detect.MaxText {
		t.Fatalf("second failure %+v", r)
	}
	r = runCLI(t, ccStop("sess-1", cwd), "hook", "claude-code", "Stop")
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("stop %+v", r)
	}
	var last lastRun
	data, err := os.ReadFile(hookLastRunPath(cache, "claude-code"))
	if err != nil || json.Unmarshal(data, &last) != nil || last.Event != "Stop" {
		t.Fatalf("last run %s %v", data, err)
	}
	if _, err := time.Parse(time.RFC3339, last.TS); err != nil {
		t.Errorf("ts %q", last.TS)
	}
	if info, err := os.Stat(hookLastRunPath(cache, "claude-code")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("last run mode %v %v", info, err)
	}
	// The session file holds counters, never the command.
	state, _ := os.ReadFile(detect.StatePath(cache, "claude-code", "sess-1"))
	if len(state) == 0 || strings.Contains(string(state), "npm") {
		t.Errorf("state %s", state)
	}
}

func TestHook_ConfigAndRepoSuppress(t *testing.T) {
	t.Run("config thresholds", func(t *testing.T) {
		cfgPath, _ := isolate(t)
		putFile(t, cfgPath, "[detect]\nsame_command = 3\nsame_tool = 9\n", 0o600)
		cwd := t.TempDir()
		for i, want := range []bool{false, false, true} {
			r := runCLI(t, ccFailure("s", cwd, "make"), "hook", "claude-code", "PostToolUseFailure")
			if (noteOf(t, r.stdout) != "") != want {
				t.Fatalf("run %d: %+v", i, r)
			}
		}
	})
	t.Run("config nudge off", func(t *testing.T) {
		cfgPath, cache := isolate(t)
		putFile(t, cfgPath, "[detect]\nnudge = false\n", 0o600)
		cwd := t.TempDir()
		for range 3 {
			if r := runCLI(t, ccFailure("s", cwd, "make"), "hook", "claude-code", "PostToolUseFailure"); r.stdout != "" {
				t.Fatalf("%+v", r)
			}
		}
		// Counting goes on.
		var total int
		if err := detect.Update(cache, "claude-code", "s", time.Now().Add(10*time.Second), func(st *detect.State) { total = st.Total }); err != nil || total != 3 {
			t.Fatalf("total %d %v", total, err)
		}
	})
	t.Run("repository file", func(t *testing.T) {
		isolate(t)
		repo := t.TempDir()
		putFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
		putFile(t, filepath.Join(repo, ".agentfeedback.toml"), "[detect]\nnudge = false\n", 0o644)
		cwd := filepath.Join(repo, "sub")
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if r := runCLI(t, ccFailure("s", cwd, "make"), "hook", "claude-code", "PostToolUseFailure"); r.code != 0 || r.stdout != "" || r.stderr != "" {
				t.Fatalf("%+v", r)
			}
		}
	})
	t.Run("filed in the session", func(t *testing.T) {
		_, cache := isolate(t)
		client.LogTo(cache, client.Outcome{Outcome: client.OutcomeSubmitted, Kind: "friction", Key: "k", SessionID: "s"}, time.Now(), nil)
		cwd := t.TempDir()
		for range 3 {
			if r := runCLI(t, ccFailure("s", cwd, "make"), "hook", "claude-code", "PostToolUseFailure"); r.stdout != "" {
				t.Fatalf("%+v", r)
			}
		}
		// A marker under the id the note shows suppresses too.
		client.LogTo(cache, client.Outcome{Outcome: client.OutcomeSubmitted, Kind: "friction", Key: "k2", SessionID: "uv"}, time.Now(), nil)
		for range 3 {
			if r := runCLI(t, ccFailure("u\u202ev", cwd, "make"), "hook", "claude-code", "PostToolUseFailure"); r.stdout != "" {
				t.Fatalf("cleaned id: %+v", r)
			}
		}
		// Another session is not suppressed.
		runCLI(t, ccFailure("t", cwd, "make"), "hook", "claude-code", "PostToolUseFailure")
		if r := runCLI(t, ccFailure("t", cwd, "make"), "hook", "claude-code", "PostToolUseFailure"); noteOf(t, r.stdout) == "" {
			t.Fatalf("%+v", r)
		}
	})
}

func TestHook_Silent(t *testing.T) {
	_, cache := isolate(t)
	for _, args := range [][]string{
		{"hook"}, {"hook", "claude-code"}, {"hook", "claude-code", "Stop", "x"},
		{"hook", "nobody", "Stop"}, {"hook", "claude-code", "PreToolUse"}, {"hook", "-h"},
	} {
		r := runCLI(t, ccStop("s", "/tmp"), args...)
		if r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Errorf("%v: %+v", args, r)
		}
	}
	if r := runCLI(t, "not json", "hook", "claude-code", "Stop"); r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Errorf("bad payload %+v", r)
	}
	if _, err := os.Stat(filepath.Join(cache, "hooks", "nobody.json")); err == nil {
		t.Error("an unknown harness wrote a last-run file")
	}
	if _, err := os.Stat(hookLastRunPath(cache, "claude-code")); err != nil {
		t.Errorf("a payload that does not parse wrote no last-run file: %v", err)
	}
}

// TestHook_StdinNeverEnds: stdin that stays open counts as no payload; the
// hook returns within its deadline and logs one error line.
func TestHook_StdinNeverEnds(t *testing.T) {
	_, cache := isolate(t)
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	done := make(chan result, 1)
	go func() {
		var out, errOut strings.Builder
		code := run([]string{"hook", "claude-code", "PostToolUseFailure"}, pr, &out, &errOut)
		done <- result{code, out.String(), errOut.String()}
	}()
	select {
	case r := <-done:
		if r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hook waited on stdin")
	}
	n := 0
	for _, l := range hookLogLines(t, cache) {
		if l["outcome"] == client.OutcomeError && strings.Contains(l["reason"].(string), "hook claude-code PostToolUseFailure: stdin") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("log %v", hookLogLines(t, cache))
	}
}

// TestHook_NudgeFailsClosed: a user config that cannot be read, or a
// collection policy that is off or opt-in only with no usable cwd, gives
// no note; counting goes on.
func TestHook_NudgeFailsClosed(t *testing.T) {
	for _, tt := range []struct{ name, config, cwd string }{
		{"bad config", "[detect\n", ""},
		{"disabled, no cwd", "[collect]\ndisabled = true\n", ""},
		{"opt-in only, relative cwd", "[collect]\nopt_in_only = true\n", "rel/dir"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath, cache := isolate(t)
			putFile(t, cfgPath, tt.config, 0o600)
			for range 3 {
				if r := runCLI(t, ccFailure("s", tt.cwd, "make"), "hook", "claude-code", "PostToolUseFailure"); r.code != 0 || r.stdout != "" || r.stderr != "" {
					t.Fatalf("%+v", r)
				}
			}
			var total int
			if err := detect.Update(cache, "claude-code", "s", time.Now().Add(10*time.Second), func(st *detect.State) { total = st.Total }); err != nil || total != 3 {
				t.Fatalf("total %d %v", total, err)
			}
		})
	}
	// With no cwd and collection on, the note is given.
	isolate(t)
	runCLI(t, ccFailure("s", "", "make"), "hook", "claude-code", "PostToolUseFailure")
	if r := runCLI(t, ccFailure("s", "", "make"), "hook", "claude-code", "PostToolUseFailure"); noteOf(t, r.stdout) == "" {
		t.Fatalf("%+v", r)
	}
}

func TestHook_CopilotExit2(t *testing.T) {
	isolate(t)
	payload := `{"sessionId":"c","cwd":"/tmp","toolName":"bash","toolArgs":{"command":"make"},"error":"Exit code 2"}`
	if r := runCLI(t, payload, "hook", "copilot", "postToolUseFailure"); r.code != 0 || r.stdout != "" {
		t.Fatalf("first %+v", r)
	}
	r := runCLI(t, payload, "hook", "copilot", "postToolUseFailure")
	if r.code != 2 || !strings.HasPrefix(r.stdout, "AgentFeedback: ") || !strings.HasSuffix(r.stdout, "\n") || r.stderr != "" {
		t.Fatalf("second %+v", r)
	}
}

func TestHook_AntigravityPending(t *testing.T) {
	isolate(t)
	failure := `{"conversationId":"ag","workspacePaths":["/w"],"toolCall":{"name":"run_command","args":{"CommandLine":"npm test"}},"error":"exit status 1"}`
	for range 2 {
		if r := runCLI(t, failure, "hook", "antigravity", "PostToolUse"); r.code != 0 || r.stdout != "" {
			t.Fatalf("%+v", r)
		}
	}
	r := runCLI(t, `{"conversationId":"ag","workspacePaths":["/w"],"invocationNum":1}`, "hook", "antigravity", "PreInvocation")
	var out struct {
		InjectSteps []struct {
			EphemeralMessage string `json:"ephemeralMessage"`
		} `json:"injectSteps"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || len(out.InjectSteps) != 1 || !strings.Contains(out.InjectSteps[0].EphemeralMessage, "submit friction") {
		t.Fatalf("%+v", r)
	}
}

// TestHook_LocalModeStopOpensNothing: in local mode the end of a turn
// opens no database and leaves the spool alone.
func TestHook_LocalModeStopOpensNothing(t *testing.T) {
	isolate(t)
	data := dataRoot(t)
	spoolOne(t, data, "local")
	r := runCLI(t, ccStop("s", t.TempDir()), "hook", "claude-code", "Stop")
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(data, "agentfeedback.db")); err == nil {
		t.Error("the local database was created")
	}
	if n := len(spoolFiles(t, client.SpoolDir(data))); n != 1 {
		t.Errorf("spool %d", n)
	}
}

// TestHook_RemoteStopFlushes: in remote mode the end of a turn sends the
// spool's due entries.
func TestHook_RemoteStopFlushes(t *testing.T) {
	isolate(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"kind":"friction","key":"k-hook","duplicate":false}`))
	}))
	defer srv.Close()
	t.Setenv(envURL, srv.URL)
	t.Setenv(envAPIKey, "throwaway")
	spoolOne(t, dataRoot(t), srv.URL)
	r := runCLI(t, ccStop("s", t.TempDir()), "hook", "claude-code", "Stop")
	if r.code != 0 || r.stdout != "" || r.stderr != "" || hits.Load() == 0 {
		t.Fatalf("%+v hits %d", r, hits.Load())
	}
}

func TestHook_PrunesOldSessions(t *testing.T) {
	_, cache := isolate(t)
	if err := detect.Update(cache, "claude-code", "old", time.Now().Add(10*time.Second), func(*detect.State) {}); err != nil {
		t.Fatal(err)
	}
	old := detect.StatePath(cache, "claude-code", "old")
	when := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, when, when); err != nil {
		t.Fatal(err)
	}
	if r := runCLI(t, ccStop("s", t.TempDir()), "hook", "claude-code", "Stop"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old session kept: %v", err)
	}
}

func TestSubmit_ContextFlag(t *testing.T) {
	_, cache := isolate(t)
	r := runCLI(t, `{"summary":"x","context":{"origin":"agent","keep":"y"}}`, "submit", "friction", "--stdin", "--dry-run",
		"--context", "origin=hook-nudge", "--context", "session_id=abc", "--context", "empty=")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	var body struct {
		Context map[string]any `json:"context"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(r.stdout, "\n", 2)[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body.Context["origin"] != "hook-nudge" || body.Context["session_id"] != "abc" || body.Context["keep"] != "y" || body.Context["empty"] != "" {
		t.Fatalf("context %v", body.Context)
	}
	for _, bad := range []string{"noequals", "=v"} {
		if r := runCLI(t, "", "submit", "friction", "--summary", "x", "--context", bad); r.code != 2 {
			t.Errorf("%s: %+v", bad, r)
		}
	}
	if r := runCLI(t, `{"summary":"x","context":"flat"}`, "submit", "friction", "--stdin", "--context", "a=b"); r.code != 2 {
		t.Errorf("non-object context: %+v", r)
	}
	// The client log keeps the session id; the outcome line does not show it.
	r = runCLI(t, "", "submit", "friction", "--summary", "x", "--context", "session_id=sess-9")
	if r.code != 0 || strings.Contains(r.stdout, "session_id") {
		t.Fatalf("%+v", r)
	}
	found := false
	for _, l := range hookLogLines(t, cache) {
		found = found || (l["outcome"] == client.OutcomeSubmitted && l["session_id"] == "sess-9" && l["kind"] == "friction")
	}
	if !found || !filedInSession(cache, "sess-9") || filedInSession(cache, "other") {
		t.Fatalf("log %v", hookLogLines(t, cache))
	}
}

// TestDoctor_Hooks: doctor names when each CLI-mode harness's hook last
// ran, a plugin's failure to start the hook newer than that, and a recorded
// hook that still runs flush --hook.
func TestDoctor_Hooks(t *testing.T) {
	e := newInstallEnv(t)
	stubBinaries(t, map[string]string{e.exe: "4.1.0"}, e.exe, true)
	if r, _ := installRun(t, "install", "claude-code", "opencode", "--server", "local"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	cache, err := cacheDir(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := diagnose(os.Getenv, modeFlags{}, "4.1.0")
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	printReport(&buf, rep)
	if !strings.Contains(buf.String(), "hook:     claude-code last ran at never") || !strings.Contains(buf.String(), "hook:     opencode last ran at never") {
		t.Fatalf("report:\n%s", buf.String())
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := writeLastRun(cache, "opencode", "session.idle", at); err != nil {
		t.Fatal(err)
	}
	putFile(t, hookSpawnErrorPath(cache, "opencode"), `{"ts":"2026-10-01T11:00:00.000Z","error":"Error: spawn /gone ENOENT"}`, 0o600)
	rep, _ = diagnose(os.Getenv, modeFlags{}, "4.1.0")
	if hasProblem(rep, "could not start") {
		t.Fatalf("an older spawn error is a problem: %q", rep.Problems)
	}
	var got string
	for _, h := range rep.Harnesses {
		if h.Name == "opencode" {
			got = h.HookLastRun
		}
	}
	if got != "2026-10-01T12:00:00Z" {
		t.Fatalf("last run %q", got)
	}
	data, _ := json.Marshal(rep)
	if !strings.Contains(string(data), `"hook_last_run":"2026-10-01T12:00:00Z"`) {
		t.Fatalf("json %s", data)
	}
	putFile(t, hookSpawnErrorPath(cache, "opencode"), `{"ts":"2026-10-01T13:00:00.000Z","error":"Error: spawn /gone ENOENT"}`, 0o600)
	rep, _ = diagnose(os.Getenv, modeFlags{}, "4.1.0")
	if !hasProblem(rep, "the opencode plugin could not start agentfeedback hook (Error: spawn /gone ENOENT)") {
		t.Fatalf("spawn error: %q", rep.Problems)
	}
	if hasProblem(rep, "flush --hook") {
		t.Fatalf("legacy: %q", rep.Problems)
	}

	mpath := filepath.Join(e.home, ".config", "agentfeedback", "install.json")
	manifest, _ := os.ReadFile(mpath)
	putFile(t, mpath, strings.ReplaceAll(string(manifest), " hook claude-code Stop", " flush --hook"), 0o600)
	rep, _ = diagnose(os.Getenv, modeFlags{}, "4.1.0")
	if !hasProblem(rep, "claude-code still runs the older flush --hook entry; run agentfeedback install claude-code to wire agentfeedback hook") {
		t.Fatalf("legacy: %q", rep.Problems)
	}
}
