package detect

import (
	"strings"
	"testing"
)

// The payloads the generated opencode-v2 plugin sends.
func TestOpenCodeV2Parse(t *testing.T) {
	ev, ok := Parse("opencode-v2", "tool.execute.after", []byte(`{"sessionID":"ses_1","tool":"shell","args":{"command":"make test","description":"run"},"exit":2,"cwd":"/w"}`))
	if !ok || ev.Class != ClassToolFailed || ev.SessionID != "ses_1" || ev.Cwd != "/w" || ev.Tool != "shell" ||
		ev.Command != "make test" || ev.ExitCode != 2 || ev.ErrorClass != "exit" {
		t.Fatalf("failure: %+v %v", ev, ok)
	}
	ev, ok = Parse("opencode-v2", "tool.execute.after", []byte(`{"sessionID":"ses_1","tool":"shell","args":{"command":"true"},"exit":0,"cwd":"/w"}`))
	if !ok || ev.Class != "" {
		t.Fatalf("success: %+v %v", ev, ok)
	}
	ev, ok = Parse("opencode-v2", "session.idle", []byte(`{"sessionID":"ses_1","cwd":"/w"}`))
	if !ok || ev.Class != ClassDeliver || !ev.EndsTurn || ev.SessionID != "ses_1" {
		t.Fatalf("idle: %+v %v", ev, ok)
	}
	if !CanDeliver("opencode-v2", "session.idle") || CanDeliver("opencode-v2", "tool.execute.after") {
		t.Fatal("CanDeliver")
	}
	out, code := Render("opencode-v2", "session.idle", "note")
	if code != 0 || strings.TrimSpace(string(out)) != `{"additionalContext":"note"}` {
		t.Fatalf("render %q %d", out, code)
	}
	if got := StatePath("/c", "opencode-v2", "ses_1"); !strings.Contains(got, "/opencode-v2/") {
		t.Fatalf("state path %s", got)
	}
}
