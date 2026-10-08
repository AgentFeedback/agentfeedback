package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, harness, event string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "payloads", harness+"."+event+".json"))
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func TestParse_Fixtures(t *testing.T) {
	for _, tt := range []struct {
		harness, event string
		want           Event
	}{
		{"claude-code", "PostToolUseFailure", Event{Class: ClassToolFailed, SessionID: "abc123", Cwd: "/Users/x/project", Tool: "Bash", Command: "npm test", ExitCode: 1, ErrorClass: "exit"}},
		{"claude-code", "Stop", Event{Class: ClassTurnEnd, SessionID: "abc123", Cwd: "/Users/x/project", ExitCode: -1, EndsTurn: true}},
		{"codex", "Stop", Event{Class: ClassTurnEnd, SessionID: "019a-thread", Cwd: "/work/repo", ExitCode: -1, EndsTurn: true}},
		{"devin", "Stop", Event{Class: ClassTurnEnd, SessionID: "devin-1", Cwd: "/work/repo", ExitCode: -1, EndsTurn: true}},
		{"cursor", "postToolUseFailure", Event{Class: ClassToolFailed, SessionID: "conv-1", Cwd: "/project", Tool: "Shell", Command: "npm test", ExitCode: -1, ErrorClass: "timeout"}},
		{"cursor", "stop", Event{Class: ClassTurnEnd, SessionID: "conv-1", Cwd: "/project", ExitCode: -1, EndsTurn: true}},
		{"copilot", "postToolUseFailure", Event{Class: ClassToolFailed, SessionID: "cp-1", Cwd: "/repo", Tool: "bash", Command: "make test", ExitCode: 2, ErrorClass: "exit"}},
		{"copilot", "agentStop", Event{Class: ClassTurnEnd, SessionID: "cp-1", Cwd: "/repo", ExitCode: -1, EndsTurn: true}},
		{"antigravity", "PostToolUse", Event{Class: ClassToolFailed, SessionID: "ec33ebf9-0cba-4100-8142-c61503f6c587", Cwd: "/workspace/project", Tool: "run_command", Command: "npm test", ExitCode: -1, ErrorClass: "error"}},
		{"antigravity", "PreInvocation", Event{Class: ClassDeliver, SessionID: "ec33ebf9-0cba-4100-8142-c61503f6c587", Cwd: "/workspace/project", ExitCode: -1}},
		{"antigravity", "Stop", Event{Class: ClassTurnEnd, SessionID: "ec33ebf9-0cba-4100-8142-c61503f6c587", Cwd: "/workspace/project", ExitCode: -1, EndsTurn: true}},
		{"opencode", "tool.execute.after", Event{Class: ClassToolFailed, SessionID: "ses_1", Cwd: "/repo", Tool: "bash", Command: "go test ./...", ExitCode: 1, ErrorClass: "exit"}},
		{"opencode", "session.idle", Event{Class: ClassDeliver, SessionID: "ses_1", Cwd: "/repo", ExitCode: -1, EndsTurn: true}},
		{"omp", "tool_result", Event{Class: ClassToolFailed, SessionID: "omp-1", Cwd: "/repo", Tool: "bash", Command: "cargo build", ExitCode: -1, ErrorClass: "error"}},
		{"omp", "agent_end", Event{Class: ClassTurnEnd, SessionID: "omp-1", Cwd: "/repo", ExitCode: -1, EndsTurn: true}},
		{"pi", "tool_result", Event{Class: ClassToolFailed, SessionID: "pi-1", Cwd: "/repo", Tool: "bash", Command: "pytest -x", ExitCode: -1, ErrorClass: "error"}},
		{"pi", "agent_settled", Event{Class: ClassTurnEnd, SessionID: "pi-1", Cwd: "/repo", ExitCode: -1, EndsTurn: true}},
	} {
		got, ok := Parse(tt.harness, tt.event, fixture(t, tt.harness, tt.event))
		if !ok {
			t.Errorf("%s %s: not ok", tt.harness, tt.event)

			continue
		}
		if tt.want.Class == ClassToolFailed && len(got.ArgsDigest) != 12 {
			t.Errorf("%s %s: args digest %q", tt.harness, tt.event, got.ArgsDigest)
		}
		got.ArgsDigest = ""
		if got != tt.want {
			t.Errorf("%s %s:\n got %+v\nwant %+v", tt.harness, tt.event, got, tt.want)
		}
	}
}

func TestParse_NotOK(t *testing.T) {
	for _, tt := range []struct{ harness, event, payload string }{
		{"claude-code", "PreToolUse", `{"session_id":"a"}`},
		{"claude-code", "PostToolUseFailure", `not json`},
		{"claude-code", "PostToolUseFailure", `["a"]`},
		{"codex", "PostToolUse", `{"session_id":"a"}`},
		{"kiro", "Stop", `{"session_id":"a"}`},
		{"nobody", "Stop", `{}`},
		{"pi", "agent_end", `{}`},
		{"omp", "agent_settled", `{}`},
	} {
		if _, ok := Parse(tt.harness, tt.event, []byte(tt.payload)); ok {
			t.Errorf("%s %s %s: ok", tt.harness, tt.event, tt.payload)
		}
	}
}

func TestParse_IgnoreAndSuccess(t *testing.T) {
	for _, tt := range []struct {
		name, harness, event, payload string
		class, ignore                 string
	}{
		{"claude interrupt", "claude-code", "PostToolUseFailure", `{"session_id":"s","tool_name":"Bash","tool_input":{"command":"sleep 9"},"error":"aborted","is_interrupt":true}`, ClassToolFailed, IgnoreInterrupt},
		{"claude self command", "claude-code", "PostToolUseFailure", `{"session_id":"s","tool_name":"Bash","tool_input":{"command":"agentfeedback submit friction --summary x"},"error":"Exit code 1"}`, ClassToolFailed, IgnoreSelf},
		{"claude self path", "claude-code", "PostToolUseFailure", `{"session_id":"s","tool_name":"Bash","tool_input":{"command":"/usr/bin/agentfeedback list"},"error":"Exit code 1"}`, ClassToolFailed, IgnoreSelf},
		{"claude self mcp", "claude-code", "PostToolUseFailure", `{"session_id":"s","tool_name":"mcp__agentfeedback__submit","tool_input":{},"error":"bad"}`, ClassToolFailed, IgnoreSelf},
		{"not self", "claude-code", "PostToolUseFailure", `{"session_id":"s","tool_name":"Bash","tool_input":{"command":"ls agentfeedback-docs"},"error":"Exit code 2"}`, ClassToolFailed, ""},
		{"cursor permission", "cursor", "postToolUseFailure", `{"conversation_id":"c","tool_name":"Shell","tool_input":{"command":"rm -rf /"},"error_message":"denied","failure_type":"permission_denied"}`, ClassToolFailed, IgnorePermission},
		{"cursor interrupt", "cursor", "postToolUseFailure", `{"conversation_id":"c","tool_name":"Shell","tool_input":{"command":"x"},"error_message":"stopped","failure_type":"error","is_interrupt":true}`, ClassToolFailed, IgnoreInterrupt},
		{"antigravity success", "antigravity", "PostToolUse", `{"conversationId":"c","toolCall":{"name":"run_command","args":{"CommandLine":"ls"}},"error":""}`, "", ""},
		{"opencode success", "opencode", "tool.execute.after", `{"sessionID":"s","tool":"bash","args":{"command":"ls"},"exit":0}`, "", ""},
		{"omp success", "omp", "tool_result", `{"session_id":"s","tool":"bash","input":{"command":"ls"},"is_error":false}`, "", ""},
		{"copilot snake keys", "copilot", "PostToolUseFailure", `{"session_id":"s","tool_name":"bash","tool_input":{"command":"x"},"error":"boom"}`, ClassToolFailed, ""},
	} {
		got, ok := Parse(tt.harness, tt.event, []byte(tt.payload))
		if !ok || got.Class != tt.class || got.Ignore != tt.ignore {
			t.Errorf("%s: %+v %v", tt.name, got, ok)
		}
	}
}

// TestParse_NeverOpensTranscript: a transcript_path naming a FIFO is never
// opened, so Parse returns at once instead of blocking on it.
func TestParse_NeverOpensTranscript(t *testing.T) {
	payload := []byte(`{"session_id":"s","transcript_path":"/dev/stdin","transcriptPath":"/dev/stdin","tool_name":"Bash","tool_input":{"command":"x"},"error":"Exit code 1"}`)
	if _, ok := Parse("claude-code", "PostToolUseFailure", payload); !ok {
		t.Fatal("not ok")
	}
}

func TestCommandDigest_CollapsesWhitespace(t *testing.T) {
	if CommandDigest("npm  test\n") != CommandDigest("npm test") || CommandDigest("npm test") == CommandDigest("npm tests") {
		t.Error("digest")
	}
}

func TestIsSelf(t *testing.T) {
	for _, tt := range []struct {
		command string
		want    bool
	}{
		{"agentfeedback submit friction --summary x", true},
		{"/usr/local/bin/agentfeedback list", true},
		{"AGENTFEEDBACK_URL=x FOO=1 agentfeedback doctor", true},
		{"env agentfeedback doctor", true},
		{"exec ~/bin/agentfeedback flush", true},
		{"sudo agentfeedback install claude-code", true},
		{"command agentfeedback version", true},
		{"cd /tmp && agentfeedback doctor", true},
		{"false || agentfeedback doctor", true},
		{"true; agentfeedback doctor", true},
		{"echo '{}' | agentfeedback submit friction --stdin", true},
		{"cd /tmp\nagentfeedback doctor", true},
		{"env FOO=1 agentfeedback doctor", true},
		{"(agentfeedback doctor)", true},
		{"{ agentfeedback doctor; }", true},
		{"time agentfeedback doctor", true},
		{"go test ./cmd/agentfeedback/...", false},
		{"cd ~/x/agentfeedback.dev && make", false},
		{"grep agentfeedback f", false},
		{"ls agentfeedback-docs", false},
		{"agentfeedback-docs build", false},
		{"FOO=agentfeedback make", false},
		{"", false},
	} {
		if got := IsSelf(tt.command); got != tt.want {
			t.Errorf("%q: %v, want %v", tt.command, got, tt.want)
		}
	}
}

func TestParse_UntrustedToolAndClass(t *testing.T) {
	long := strings.Repeat("t", 100)
	for _, tt := range []struct {
		name, harness, event, payload string
		tool, class                   string
	}{
		{"control", "claude-code", "PostToolUseFailure", `{"session_id":"s","tool_name":"Ba\u001b[31msh x\ny","error":"boom"}`, "Ba__31msh_x_y", "error"},
		{"kept", "claude-code", "PostToolUseFailure", `{"session_id":"s","tool_name":"mcp__srv__a.b:c/d-e","error":"Exit code 1"}`, "mcp__srv__a.b:c/d-e", "exit"},
		{"long", "claude-code", "PostToolUseFailure", `{"session_id":"s","tool_name":"` + long + `","error":"boom"}`, long[:64], "error"},
		{"cursor class", "cursor", "postToolUseFailure", `{"conversation_id":"c","tool_name":"Shell","failure_type":"made-up <b>"}`, "Shell", "error"},
		{"cursor timeout", "cursor", "postToolUseFailure", `{"conversation_id":"c","tool_name":"Shell","failure_type":"timeout"}`, "Shell", "timeout"},
	} {
		got, ok := Parse(tt.harness, tt.event, []byte(tt.payload))
		if !ok || got.Tool != tt.tool || got.ErrorClass != tt.class {
			t.Errorf("%s: %+v %v", tt.name, got, ok)
		}
	}
}

func TestKnownEvent(t *testing.T) {
	for _, tt := range []struct {
		harness, event string
		want           bool
	}{
		{"claude-code", "Stop", true}, {"antigravity", "PreInvocation", true}, {"opencode", "session.idle", true},
		{"claude-code", "PreToolUse", false}, {"nobody", "Stop", false}, {"pi", "agent_end", false},
	} {
		if got := KnownEvent(tt.harness, tt.event); got != tt.want {
			t.Errorf("%s %s: %v", tt.harness, tt.event, got)
		}
	}
}
