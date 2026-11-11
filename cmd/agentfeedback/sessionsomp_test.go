package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
)

// Synthetic journals follow omp's version-3 SessionHeader/SessionMessageEntry
// in packages/coding-agent/src/session/session-entries.ts and message types in
// packages/ai/src/types.ts, upstream f2ee6ba9b3e00b379c7c12b9d99ef6369fd08f3f.
// No source session data is included.
func init() {
	registerGoldenStore("omp", func(t *testing.T, home string) string {
		cwd := filepath.Join(t.TempDir(), "omp-proj")
		if err := os.Mkdir(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		putOMPStore(t, filepath.Join(home, ".omp", "agent", "sessions"), "c0ffee01", cwd)
		return cwd
	})
	registerGoldenSkillSessions("omp", func(t *testing.T, home, cwd string) {
		copyStore(t, filepath.Join("..", "..", "internal", "sessions", "testdata", "omp", "skills"), filepath.Join(home, ".omp", "agent", "sessions"), map[string]string{"{{SID}}": "c0ffee01", "{{CWD}}": cwd})
	})
}

func putOMPStore(t *testing.T, root, sid, cwd string) {
	t.Helper()
	copyStore(t, filepath.Join("..", "..", "internal", "sessions", "testdata", "omp", "project"), filepath.Join(root, "project"), map[string]string{"{{SID}}": sid, "{{CWD}}": cwd})
}

func ompWorld(t *testing.T) string {
	t.Helper()
	sessionsWorld(t)
	root := filepath.Join(t.TempDir(), "sessions")
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Dir(root))
	cwd := filepath.Join(t.TempDir(), "omp-proj")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	putOMPStore(t, root, "omp-fixture", cwd)
	return cwd
}

func TestOMPSessionsLifecycle(t *testing.T) {
	ompWorld(t)
	r := runCLI(t, "", "sessions", "list", "--harness", "omp", "--json")
	var listing sessions.Listing
	decodeJSON(t, r.stdout, &listing)
	if r.code != 0 || len(listing.Sessions) != 1 || listing.Sessions[0].Ref != "omp:omp-fixture" || listing.Sessions[0].State != sessions.StateNew {
		t.Fatalf("list: %+v", r)
	}
	r = runCLI(t, "", "sessions", "digest", "omp:omp-fixture", "--json")
	var digest sessions.DigestOutput
	decodeJSON(t, r.stdout, &digest)
	if r.code != 0 || len(digest.Sessions) != 1 || digest.Sessions[0].Counts.Prompts != 1 || digest.Sessions[0].Counts.ToolErrors != 1 || digest.Sessions[0].Counts.Interrupts != 1 {
		t.Fatalf("digest: %+v", r)
	}
	for _, excluded := range []string{"INJECTED_SENTINEL", "DEVELOPER_SENTINEL", "AMBIGUOUS_SENTINEL"} {
		if strings.Contains(r.stdout, excluded) {
			t.Fatalf("injected input in digest: %+v", r)
		}
	}
	r = runCLI(t, "", "sessions", "mark", "omp:omp-fixture", "--outcome", "nothing", "--json")
	if r.code != 0 {
		t.Fatalf("mark: %+v", r)
	}
	r = runCLI(t, "", "sessions", "list", "--harness", "omp", "--unprocessed", "--json")
	decodeJSON(t, r.stdout, &listing)
	if r.code != 0 || len(listing.Sessions) != 0 {
		t.Fatalf("watermark did not process journal: %+v", r)
	}
	r = runCLI(t, "", "sessions", "status", "--set-selection", "--harness", "omp", "--since", "7d", "--limit", "5", "--json")
	var status sessions.StatusOutput
	decodeJSON(t, r.stdout, &status)
	if r.code != 0 || status.Selection == nil || status.Selection.Limit != 5 {
		t.Fatalf("status: %+v", r)
	}
	found := false
	for _, h := range status.Harnesses {
		if h.Harness == "omp" {
			found = h.States[sessions.StateProcessed] == 1
		}
	}
	if !found {
		t.Fatalf("status omitted processed omp watermark: %+v", status)
	}
}

func TestOMPSubmitContextFrom(t *testing.T) {
	ompWorld(t)
	processRepo(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "ambient-session")
	stdin := `{"summary":"deploy failed","payload":{"category":"tooling","details":"d","suggested_fix":"f"}}`
	wantKey := sessions.Key("omp", "omp-fixture", "r-3", 1, sessions.DetectorVersion)
	for range 2 {
		r := runCLI(t, stdin, "submit", "friction", "--stdin", "--context-from", "omp:omp-fixture#r-3", "--dry-run")
		if r.code != 0 {
			t.Fatalf("submit: %+v", r)
		}
		var body struct {
			Key        string            `json:"key"`
			Project    string            `json:"project"`
			Harness    string            `json:"harness"`
			Model      string            `json:"model"`
			OccurredAt string            `json:"occurred_at"`
			Context    map[string]string `json:"context"`
		}
		decodeJSON(t, strings.Split(r.stdout, "\n")[0], &body)
		if body.Key != wantKey || body.Project != "omp-proj" || body.Harness != "omp" || body.Model != "gpt-5.3-codex" || body.OccurredAt != "2026-10-08T07:00:09.250000Z" || body.Context["folder"] != "omp-proj" || body.Context["detector"] != "omp-jsonl/"+sessions.DetectorVersion || strings.Contains(r.stdout, "ambient-session") {
			t.Fatalf("session context: %s", r.stdout)
		}
	}
}

func TestOMPPolicyRefusal(t *testing.T) {
	cwd := ompWorld(t)
	writeClientConfig(t, "[collect]\ndeny_paths = ["+string(mustJSONString(cwd))+"]\n")
	r := runCLI(t, "", "sessions", "digest", "omp:omp-fixture", "--json")
	var digest sessions.DigestOutput
	decodeJSON(t, r.stdout, &digest)
	if r.code != 1 || len(digest.Errors) != 1 || digest.Errors[0].State != sessions.StateDenied {
		t.Fatalf("denied digest: %+v", r)
	}
	r = runCLI(t, `{"summary":"x","payload":{"category":"tooling","details":"d","suggested_fix":"f"}}`, "submit", "friction", "--stdin", "--context-from", "omp:omp-fixture#r-3", "--dry-run")
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "denied") {
		t.Fatalf("denied submit: %+v", r)
	}
}

func TestOMPStoreXDGAndExplicitDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("omp uses XDG data directories on Linux only")
	}
	sessionsWorld(t)
	dataHome := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()
	putOMPStore(t, filepath.Join(dataHome, "omp", "sessions"), "xdg-fixture", cwd)
	r := runCLI(t, "", "sessions", "list", "--harness", "omp", "--json")
	var listing sessions.Listing
	decodeJSON(t, r.stdout, &listing)
	if r.code != 0 || len(listing.Sessions) != 1 || listing.Sessions[0].Ref != "omp:xdg-fixture" {
		t.Fatalf("XDG session discovery: %+v", r)
	}
	explicit := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", explicit)
	putOMPStore(t, filepath.Join(explicit, "sessions"), "explicit-fixture", cwd)
	r = runCLI(t, "", "sessions", "list", "--harness", "omp", "--json")
	decodeJSON(t, r.stdout, &listing)
	if r.code != 0 || len(listing.Sessions) != 1 || listing.Sessions[0].Ref != "omp:explicit-fixture" {
		t.Fatalf("explicit directory did not override XDG: %+v", r)
	}
}
