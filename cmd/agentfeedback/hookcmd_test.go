package main

import (
	"encoding/json"
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

// cursorHookFixture loads a synthetic payload based on the official hook
// contract, substituting this test's home and workspace (not a live capture).
func cursorHookFixture(t *testing.T, event, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "detect", "testdata", "payloads", "cursor."+event+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	payload["transcript_path"] = strings.ReplaceAll(payload["transcript_path"].(string), "/home/synthetic", home)
	payload["workspace_roots"] = []string{filepath.Join(home, "workspace"), filepath.Join(home, "second")}
	if event == "postToolUseFailure" {
		payload["cwd"] = filepath.Join(home, "workspace", "sub")
	}

	return payload
}

func cursorHookState(t *testing.T, cache, session string) detect.State {
	t.Helper()
	raw, err := os.ReadFile(detect.StatePath(cache, "cursor", session))
	if err != nil {
		t.Fatal(err)
	}
	var st detect.State
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}

	return st
}

func TestHook_CursorFixtureIndexesTranscript(t *testing.T) {
	for _, event := range []string{"stop", "sessionEnd", "postToolUseFailure"} {
		t.Run(event, func(t *testing.T) {
			_, cache := isolate(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			payload := cursorHookFixture(t, event, home)
			const session = "original-conv:id"
			payload["conversation_id"] = session
			payload["session_id"] = "different-session-id"
			raw, _ := json.Marshal(payload)
			if r := runCLI(t, string(raw), "hook", "cursor", event); r.code != 0 || r.stdout != "" || r.stderr != "" {
				t.Fatalf("%+v", r)
			}
			st := cursorHookState(t, cache, session)
			want := detect.CursorSession{
				ConversationID: session, TranscriptPath: payload["transcript_path"].(string),
				WorkspaceRoots: []string{filepath.Join(home, "workspace"), filepath.Join(home, "second")},
			}
			if event == "postToolUseFailure" {
				want.Cwd = filepath.Join(home, "workspace", "sub")
			}
			if st.Cursor == nil || !reflect.DeepEqual(*st.Cursor, want) {
				t.Fatalf("indexed metadata: %+v, want %+v", st.Cursor, want)
			}
			wantTotal := 0
			if event == "postToolUseFailure" {
				wantTotal = 1
			}
			if st.Total != wantTotal || st.Created.IsZero() || st.Updated.IsZero() {
				t.Fatalf("detector counters changed: %+v", st)
			}
			path := detect.StatePath(cache, "cursor", session)
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("state permissions: %v, %v", info, err)
			}
			if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("index directory permissions: %v, %v", info, err)
			}
			if _, err := os.Stat(want.TranscriptPath); !os.IsNotExist(err) {
				t.Fatalf("fixture unexpectedly created a transcript: %v", err)
			}
		})
	}
}

func TestHook_CursorInvalidTranscriptClearsIndex(t *testing.T) {
	for _, event := range []string{"stop", "sessionEnd", "postToolUseFailure"} {
		for _, name := range []string{"null", "missing", "empty", "relative", "outside", "sibling-prefix", "home", "traversal", "wrong-type", "nul"} {
			t.Run(event+"/"+name, func(t *testing.T) {
				cfg, cache := isolate(t)
				putFile(t, cfg, "[detect]\nnudge = false\n", 0o600)
				home := t.TempDir()
				t.Setenv("HOME", home)
				payload := cursorHookFixture(t, event, home)
				raw, _ := json.Marshal(payload)
				if r := runCLI(t, string(raw), "hook", "cursor", event); r.code != 0 || r.stdout != "" || r.stderr != "" {
					t.Fatalf("seed: %+v", r)
				}
				if cursorHookState(t, cache, "conv-1").Cursor == nil {
					t.Fatal("seed mapping missing")
				}
				switch name {
				case "null":
					payload["transcript_path"] = nil
				case "missing":
					delete(payload, "transcript_path")
				case "empty":
					payload["transcript_path"] = ""
				case "relative":
					payload["transcript_path"] = ".cursor/transcript.txt"
				case "outside":
					payload["transcript_path"] = filepath.Join(t.TempDir(), "transcript.txt")
				case "sibling-prefix":
					payload["transcript_path"] = home + "-other/transcript.txt"
				case "home":
					payload["transcript_path"] = home
				case "traversal":
					payload["transcript_path"] = home + "/../outside/transcript.txt"
				case "wrong-type":
					payload["transcript_path"] = map[string]string{"path": home + "/transcript.txt"}
				case "nul":
					payload["transcript_path"] = home + "/transcript\x00.txt"
				}
				raw, _ = json.Marshal(payload)
				if r := runCLI(t, string(raw), "hook", "cursor", event); r.code != 0 || r.stdout != "" || r.stderr != "" {
					t.Fatalf("invalid metadata: %+v", r)
				}
				st := cursorHookState(t, cache, "conv-1")
				if st.Cursor != nil {
					t.Fatalf("unsafe or stale mapping retained: %+v", st.Cursor)
				}
				wantTotal := 0
				if event == "postToolUseFailure" {
					wantTotal = 2
				}
				if st.Total != wantTotal {
					t.Fatalf("metadata validation changed counting: %+v", st)
				}
			})
		}
	}
}

func TestHook_CursorWorkspaceDoesNotReuseStaleRootOrToolCwd(t *testing.T) {
	for _, tt := range []struct {
		name  string
		roots any
	}{
		{"null", nil},
		{"missing", nil},
		{"empty", []string{}},
		{"relative", []string{"relative"}},
		{"relative-secondary", []string{"/workspace", "relative"}},
		{"empty-secondary", []string{"/workspace", ""}},
		{"nul-secondary", []string{"/workspace", "/private\x00"}},
		{"wrong-type", "not-an-array"},
		{"mixed-type", []any{"/workspace", 7}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, cache := isolate(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			payload := cursorHookFixture(t, "stop", home)
			raw, _ := json.Marshal(payload)
			if r := runCLI(t, string(raw), "hook", "cursor", "stop"); r.code != 0 || r.stdout != "" || r.stderr != "" {
				t.Fatalf("seed: %+v", r)
			}
			payload["cwd"] = filepath.Join(home, "old-workspace", "sub")
			payload["workspace_roots"] = tt.roots
			if tt.name == "missing" {
				delete(payload, "workspace_roots")
			}
			raw, _ = json.Marshal(payload)
			if r := runCLI(t, string(raw), "hook", "cursor", "stop"); r.code != 0 || r.stdout != "" || r.stderr != "" {
				t.Fatalf("%+v", r)
			}
			st := cursorHookState(t, cache, "conv-1")
			if st.Cursor == nil || len(st.Cursor.WorkspaceRoots) != 0 {
				t.Fatalf("missing or invalid roots did not replace stale workspace metadata: %+v", st.Cursor)
			}
		})
	}
}

func TestHook_CursorInvalidCwdClearsIndex(t *testing.T) {
	for _, event := range []string{"stop", "sessionEnd", "postToolUseFailure"} {
		for _, tt := range []struct {
			name string
			cwd  any
		}{
			{"empty", ""}, {"null", nil}, {"wrong-type", 7}, {"relative", "relative"}, {"nul", "/workspace\x00"},
		} {
			t.Run(event+"/"+tt.name, func(t *testing.T) {
				_, cache := isolate(t)
				home := t.TempDir()
				t.Setenv("HOME", home)
				payload := cursorHookFixture(t, event, home)
				raw, _ := json.Marshal(payload)
				if r := runCLI(t, string(raw), "hook", "cursor", event); r.code != 0 || r.stdout != "" || r.stderr != "" {
					t.Fatalf("seed: %+v", r)
				}
				if cursorHookState(t, cache, "conv-1").Cursor == nil {
					t.Fatal("seed mapping missing")
				}
				payload["cwd"] = tt.cwd
				raw, _ = json.Marshal(payload)
				if r := runCLI(t, string(raw), "hook", "cursor", event); r.code != 0 || r.stderr != "" {
					t.Fatalf("invalid cwd: %+v", r)
				}
				if st := cursorHookState(t, cache, "conv-1"); st.Cursor != nil {
					t.Fatalf("invalid cwd retained a mapping: %+v", st.Cursor)
				}
			})
		}
	}
}

func TestHook_CursorInvalidConversationIDsNotIndexed(t *testing.T) {
	for _, event := range []string{"stop", "sessionEnd", "postToolUseFailure"} {
		for _, tt := range []struct {
			name, id string
		}{
			{"missing", ""},
			{"empty", ""},
			{"slash", "invalid/id"},
			{"backslash", `invalid\id`},
			{"hash", "invalid#id"},
			{"dot", "."},
			{"dotdot", ".."},
		} {
			t.Run(event+"/"+tt.name, func(t *testing.T) {
				_, cache := isolate(t)
				home := t.TempDir()
				t.Setenv("HOME", home)
				payload := cursorHookFixture(t, event, home)
				payload["conversation_id"] = tt.id
				if tt.name == "missing" {
					delete(payload, "conversation_id")
				}
				payload["session_id"] = "detector-fallback"
				raw, _ := json.Marshal(payload)
				if r := runCLI(t, string(raw), "hook", "cursor", event); r.code != 0 || r.stdout != "" || r.stderr != "" {
					t.Fatalf("invalid conversation id: %+v", r)
				}
				session := tt.id
				if session == "" {
					session = "detector-fallback"
				}
				if st := cursorHookState(t, cache, session); st.Cursor != nil {
					t.Fatalf("unrepresentable conversation indexed: %+v", st.Cursor)
				}
			})
		}
	}
}

func TestHook_CursorSessionEndDropsPendingWithoutNudge(t *testing.T) {
	_, cache := isolate(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := detect.Update(cache, "cursor", "conv-1", time.Now().Add(time.Second), func(st *detect.State) {
		st.Total, st.Failures = 5, 5
		st.Tools = map[string]int{"Shell": 5}
		st.LastTool = "Shell"
		st.Pending = true
		st.PendingFacts = &detect.Facts{Reason: detect.ReasonSameTool, Tool: "Shell", Count: 5}
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cursorHookFixture(t, "sessionEnd", home))
	if r := runCLI(t, string(raw), "hook", "cursor", "sessionEnd"); r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	st := cursorHookState(t, cache, "conv-1")
	if st.Pending || st.PendingFacts != nil || st.Nudges != 0 || st.Total != 5 || st.Cursor == nil {
		t.Fatalf("sessionEnd failed fire-and-forget semantics: %+v", st)
	}
}

func TestHook_CursorSessionEndFlushes(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
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
	raw, _ := json.Marshal(cursorHookFixture(t, "sessionEnd", home))
	r := runCLI(t, string(raw), "hook", "cursor", "sessionEnd")
	if r.code != 0 || r.stdout != "" || r.stderr != "" || hits.Load() == 0 {
		t.Fatalf("sessionEnd did not flush silently: %+v hits=%d", r, hits.Load())
	}
}

func TestHook_CursorMetadataDoesNotChangeNudges(t *testing.T) {
	_, cache := isolate(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	payload := cursorHookFixture(t, "postToolUseFailure", home)
	raw, _ := json.Marshal(payload)
	if r := runCLI(t, string(raw), "hook", "cursor", "postToolUseFailure"); r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("first failure: %+v", r)
	}
	payload["transcript_path"] = nil
	raw, _ = json.Marshal(payload)
	r := runCLI(t, string(raw), "hook", "cursor", "postToolUseFailure")
	var note struct {
		AdditionalContext string `json:"additional_context"`
	}
	if r.code != 0 || r.stderr != "" || json.Unmarshal([]byte(r.stdout), &note) != nil ||
		!strings.Contains(note.AdditionalContext, "agentfeedback submit friction") ||
		!strings.Contains(note.AdditionalContext, "--context session_id=conv-1 --context session_harness=cursor") {
		t.Fatalf("second failure did not give the existing Cursor note: %+v", r)
	}
	st := cursorHookState(t, cache, "conv-1")
	if st.Total != 2 || st.Nudges != 1 || st.Cursor != nil {
		t.Fatalf("metadata changed detector semantics: %+v", st)
	}
	data, err := os.ReadFile(detect.StatePath(cache, "cursor", "conv-1"))
	if err != nil || strings.Contains(string(data), "npm test") || strings.Contains(string(data), "Command timed out") {
		t.Fatalf("state retained hook content: %s, %v", data, err)
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
		{"hook"},
		{"hook", "claude-code"},
		{"hook", "claude-code", "Stop", "x"},
		{"hook", "nobody", "Stop"},
		{"hook", "claude-code", "PreToolUse"},
		{"hook", "-h"},
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

func TestHook_ClineSlowFlushReleasesClaim(t *testing.T) {
	isolate(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	t.Setenv(envURL, srv.URL)
	t.Setenv(envAPIKey, "throwaway")
	data := dataRoot(t)
	spoolOne(t, data, srv.URL)
	start := time.Now()
	r := runCLI(t, `{"session_id":"cline-slow"}`, "hook", "cline", "afterRun")
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("hook exceeded plugin subprocess budget: %v", elapsed)
	}
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	entries, err := os.ReadDir(client.SpoolDir(data))
	if err != nil || len(entries) != 1 || strings.Contains(entries[0].Name(), "inflight") {
		t.Fatalf("spool claim not released: %v, %v", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(client.SpoolDir(data), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Attempts  int       `json:"attempts"`
		NotBefore time.Time `json:"not_before"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Attempts != 1 || !entry.NotBefore.After(start) {
		t.Fatalf("cancelled send not rescheduled: %s, %v", raw, err)
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
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := writeLastRun(cache, "opencode", "session.idle", at); err != nil {
		t.Fatal(err)
	}
	putFile(t, hookSpawnErrorPath(cache, "opencode"), `{"ts":"2026-10-01T11:00:00.000Z","error":"Error: spawn /gone ENOENT"}`, 0o600)
	rep, err := diagnose(os.Getenv, modeFlags{}, "4.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if hasProblem(rep, "could not start") {
		t.Fatalf("an older spawn error is a problem: %q", rep.Problems)
	}
	var got string
	for _, h := range rep.Harnesses {
		if h.Name == "opencode-v1" {
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
	if !hasProblem(rep, "could not start") {
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
