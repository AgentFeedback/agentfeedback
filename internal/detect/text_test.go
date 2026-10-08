package detect

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/nudge.golden")

// TestText_Golden pins the note for each reason.
func TestText_Golden(t *testing.T) {
	var b strings.Builder
	for _, tt := range []struct {
		harness, session string
		f                Facts
	}{
		{"claude-code", "abc123", Facts{Reason: ReasonSameTool, Tool: "Bash", Count: 3, Exit: 1, Command: "npm test"}},
		{"cursor", "conv-1", Facts{Reason: ReasonSameCommand, Tool: "Shell", Count: 2, Exit: -1, Command: "npm test"}},
		{"antigravity", "ec33ebf9-0cba-4100-8142-c61503f6c587", Facts{Reason: ReasonSession, Tool: "run_command", Count: 5, Exit: -1}},
		{"copilot", "has space", Facts{Reason: ReasonSameTool, Tool: "bash", Count: 3, Exit: 2, Command: "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY make\ttest"}},
		{"pi", "it's\x1b[0m;x", Facts{Reason: ReasonSameTool, Tool: "bash", Count: 3, Exit: 1, SinceLast: true}},
		{"omp", "s-2", Facts{Reason: ReasonSession, Count: 5, Exit: -1, SinceLast: true}},
	} {
		text := Text(tt.harness, tt.session, tt.f)
		if len(text) >= MaxText {
			t.Errorf("%d bytes: %s", len(text), text)
		}
		b.WriteString(text + "\n")
	}
	path := filepath.Join("testdata", "nudge.golden")
	if *update {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// TestText_WorstCase: the longest inputs stay under MaxText.
func TestText_WorstCase(t *testing.T) {
	long := strings.Repeat("é", 400) + "\x00\x1b[31m" + strings.Repeat("x", 4000)
	for _, f := range []Facts{
		{Reason: ReasonSameTool, Tool: long, Count: 1 << 30, Exit: 1 << 30, Command: long},
		{Reason: ReasonSameCommand, Tool: long, Count: -1 << 30, Exit: -1, Command: long},
		{Reason: ReasonSession, Tool: long, Count: 1 << 30, Exit: 255, Command: long},
	} {
		for _, id := range []string{"", strings.Repeat("a", 128), strings.Repeat("a", 129), strings.Repeat("'", 128), strings.Repeat("\x1b", 128), long} {
			text := Text("antigravity", id, f)
			if len(text) >= MaxText {
				t.Errorf("%d bytes: %s", len(text), text)
			}
			if strings.ContainsAny(text, "\x00\x1b\n") {
				t.Errorf("control characters: %q", text)
			}
			if strings.Contains(text, "session_id="+strings.Repeat("a", 129)) {
				t.Error("a 129-byte session id was kept")
			}
			if strings.Contains(text, "session_id=''") || strings.HasSuffix(text, "session_id=") {
				t.Errorf("empty session id: %s", text)
			}
		}
	}
}

func TestRender(t *testing.T) {
	const text = "AgentFeedback: note <&>"
	for _, tt := range []struct {
		harness, event, want string
		exit                 int
	}{
		{"claude-code", "PostToolUseFailure", `{"hookSpecificOutput":{"additionalContext":"AgentFeedback: note <&>","hookEventName":"PostToolUseFailure"}}`, 0},
		{"cursor", "postToolUseFailure", `{"additional_context":"AgentFeedback: note <&>"}`, 0},
		{"antigravity", "PreInvocation", `{"injectSteps":[{"ephemeralMessage":"AgentFeedback: note <&>"}]}`, 0},
		{"opencode", "session.idle", `{"additionalContext":"AgentFeedback: note <&>"}`, 0},
		{"omp", "tool_result", `{"additionalContext":"AgentFeedback: note <&>"}`, 0},
		{"pi", "tool_result", `{"additionalContext":"AgentFeedback: note <&>"}`, 0},
	} {
		out, exit := Render(tt.harness, tt.event, text)
		var got, want any
		if json.Unmarshal(out, &got) != nil || json.Unmarshal([]byte(tt.want), &want) != nil || exit != tt.exit {
			t.Errorf("%s %s: %s %d", tt.harness, tt.event, out, exit)

			continue
		}
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		if !bytes.Equal(g, w) || !bytes.HasSuffix(out, []byte("\n")) {
			t.Errorf("%s %s: %s", tt.harness, tt.event, out)
		}
	}
	if out, exit := Render("copilot", "postToolUseFailure", text); string(out) != text+"\n" || exit != 2 {
		t.Errorf("copilot: %q %d", out, exit)
	}
	for _, tt := range []struct{ harness, event, text string }{
		{"claude-code", "PostToolUseFailure", ""},
		{"copilot", "postToolUseFailure", ""},
		{"claude-code", "Stop", text},
		{"antigravity", "PostToolUse", text},
		{"codex", "Stop", text},
	} {
		if out, exit := Render(tt.harness, tt.event, tt.text); len(out) != 0 || exit != 0 {
			t.Errorf("%s %s %q: %q %d", tt.harness, tt.event, tt.text, out, exit)
		}
	}
}

// TestText_SessionID: every non-empty id of at most 128 bytes is in the
// note, shell-quoted when needed; the command excerpt goes before it.
func TestText_SessionID(t *testing.T) {
	f := Facts{Reason: ReasonSameTool, Tool: "Bash", Count: 3, Exit: 1, Command: strings.Repeat("c", 200)}
	for _, tt := range []struct{ id, want string }{
		{"abc-1.2_3", " --context session_id=abc-1.2_3 "},
		{"a b", " --context session_id='a b' "},
		{"it's", ` --context session_id='it'\''s' `},
		{"x\x00y\x1bz", " --context session_id=xyz "},
		{"a\u202eb\u200bc", " --context session_id=abc "},
		{strings.Repeat("a", 128), " --context session_id=" + strings.Repeat("a", 128) + " "},
	} {
		text := Text("claude-code", tt.id, f)
		if !strings.Contains(text, tt.want) || len(text) >= MaxText {
			t.Errorf("%q: %s", tt.id, text)
		}
	}
	// A long quoted id leaves no room for the excerpt but is kept.
	id := strings.Repeat("'", 60)
	text := Text("claude-code", id, f)
	if !strings.Contains(text, "session_id='"+strings.Repeat(`'\''`, 60)+"' ") || strings.Contains(text, "ccc") || len(text) >= MaxText {
		t.Errorf("%d bytes: %s", len(text), text)
	}
	if got := CleanSessionID("s\u202e\x00\u200b1"); got != "s1" {
		t.Errorf("CleanSessionID %q", got)
	}
	if text := Text("claude-code", "\x00", f); strings.Contains(text, "session_id") {
		t.Errorf("control-only id: %s", text)
	}
}
