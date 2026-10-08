// Package detect turns the payloads coding-agent harnesses give their hooks
// into events, counts tool failures per session and decides when a short,
// non-imperative note suggesting a friction report is due. It reads only
// the payload it is given: a transcript or other file the payload names is
// never opened. It keeps counters, never command text, and calls no model.
package detect

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Event classes.
const (
	ClassToolFailed = "tool_failed"
	ClassTurnEnd    = "turn_end"
	ClassDeliver    = "deliver"
)

// Ignore reasons: a failure that is not counted.
const (
	IgnoreInterrupt  = "interrupt"
	IgnorePermission = "permission"
	IgnoreSelf       = "self"
)

// Event is what a hook payload says, reduced to what the rule needs.
type Event struct {
	// Class is tool_failed, turn_end or deliver; "" is an event the caller
	// ignores (a tool call that succeeded).
	Class     string
	SessionID string
	Cwd       string
	Tool      string
	// Command is the shell command of a failed shell tool call, or "".
	Command string
	// ArgsDigest is the first 12 hex digits of the SHA-256 of the tool
	// input as received.
	ArgsDigest string
	// ExitCode is the command's exit status, -1 when unknown.
	ExitCode   int
	ErrorClass string
	// Ignore is "", interrupt, permission or self.
	Ignore string
	// EndsTurn is set on a turn_end event and on a deliver event that also
	// ends the turn (OpenCode's session.idle).
	EndsTurn bool
}

// object is a decoded payload: member name to raw value.
type object map[string]json.RawMessage

func decode(payload []byte) (object, bool) {
	var o object
	if json.Unmarshal(payload, &o) != nil || o == nil {
		return nil, false
	}

	return o, true
}

// str is the string member name, or "" when absent or not a string.
func (o object) str(names ...string) string {
	for _, n := range names {
		var s string
		if raw, ok := o[n]; ok && json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}

	return ""
}

func (o object) boolean(name string) bool {
	var b bool
	if raw, ok := o[name]; ok && json.Unmarshal(raw, &b) == nil {
		return b
	}

	return false
}

// firstString is the first element of the string array member name.
func (o object) firstString(name string) string {
	var arr []string
	if raw, ok := o[name]; ok && json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		return arr[0]
	}

	return ""
}

// raw is the raw value of the first present, non-null member.
func (o object) raw(names ...string) json.RawMessage {
	for _, n := range names {
		if raw, ok := o[n]; ok && string(raw) != "null" {
			return raw
		}
	}

	return nil
}

// sub decodes the member name as an object; a JSON string holding an object
// is accepted too.
func (o object) sub(names ...string) object {
	raw := o.raw(names...)
	if raw == nil {
		return nil
	}
	if s, ok := asString(raw); ok {
		raw = json.RawMessage(s)
	}
	out, _ := decode(raw)

	return out
}

func asString(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}

	return s, true
}

// digest is the first 12 hex digits of the SHA-256 of b.
func digest(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])[:12]
}

// CommandDigest is the digest a command is counted under: whitespace runs
// collapsed, then the first 12 hex digits of its SHA-256.
func CommandDigest(command string) string {
	return digest([]byte(strings.Join(strings.Fields(command), " ")))
}

var (
	exitLine   = regexp.MustCompile(`^Exit code (-?\d+)\s*$`)
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

// IsSelf reports whether a failed shell command is agentfeedback's own: the
// first word of the command, or of a part of it after a newline, &&, ||, ;
// or |, has the basename agentfeedback once leading ( and {, VAR=value
// assignments, one env, exec, sudo, command or time prefix and the
// assignments after it are skipped.
func IsSelf(command string) bool {
	for _, part := range strings.FieldsFunc(command, func(r rune) bool { return r == '&' || r == '|' || r == ';' || r == '\n' }) {
		words := strings.Fields(strings.TrimLeft(part, "({ \t\r"))
		for len(words) > 0 && assignment.MatchString(words[0]) {
			words = words[1:]
		}
		if len(words) > 0 {
			switch words[0] {
			case "env", "exec", "sudo", "command", "time":
				words = words[1:]
			}
		}
		for len(words) > 0 && assignment.MatchString(words[0]) {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		w := words[0]
		if i := strings.LastIndexAny(w, `/\`); i >= 0 {
			w = w[i+1:]
		}
		if w == "agentfeedback" {
			return true
		}
	}

	return false
}

// maxToolName is the most of a tool name kept.
const maxToolName = 64

// SafeTool is a tool name fit to store and to show: every character
// outside [A-Za-z0-9._:/-] replaced by an underscore, at most 64 bytes.
func SafeTool(tool string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == ':', r == '/', r == '-':
			return r
		}

		return '_'
	}, tool)
	if len(out) > maxToolName {
		out = out[:maxToolName]
	}

	return out
}

// errorClasses are the error classes kept; any other is "error".
var errorClasses = map[string]bool{"error": true, "exit": true, "timeout": true, "permission_denied": true, "interrupt": true}

func errorClass(c string) string {
	if errorClasses[c] {
		return c
	}

	return "error"
}

// exitFromError reads a first line "Exit code N".
func exitFromError(msg string) int {
	first, _, _ := strings.Cut(msg, "\n")
	if m := exitLine.FindStringSubmatch(strings.TrimSpace(first)); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}

	return -1
}

// failure fills the common fields of a tool failure.
func failure(sessionID, cwd, tool string, input json.RawMessage, command string) Event {
	ev := Event{Class: ClassToolFailed, SessionID: sessionID, Cwd: cwd, Tool: tool, Command: command, ExitCode: -1, ErrorClass: "error"}
	if len(input) > 0 {
		ev.ArgsDigest = digest(input)
	}
	if IsSelf(command) || strings.Contains(strings.ToLower(tool), "agentfeedback") {
		ev.Ignore = IgnoreSelf
	}

	return ev
}

// commandOf is the shell command in a tool input: the first string among
// names.
func commandOf(input object, names ...string) string {
	if input == nil {
		return ""
	}

	return input.str(names...)
}

func turnEnd(sessionID, cwd string) Event {
	return Event{Class: ClassTurnEnd, SessionID: sessionID, Cwd: cwd, ExitCode: -1, EndsTurn: true}
}

// Parse reads the payload of the harness's event. An unknown harness or
// event, or a payload that is not a JSON object, is not ok. A file the
// payload names (transcript_path and the like) is never read. A failure's
// tool name is reduced by SafeTool and its error class to a small fixed
// set.
func Parse(harness, event string, payload []byte) (Event, bool) {
	ev, ok := parse(harness, event, payload)
	if ok && ev.Class == ClassToolFailed {
		ev.Tool = SafeTool(ev.Tool)
		ev.ErrorClass = errorClass(ev.ErrorClass)
	}

	return ev, ok
}

// KnownEvent reports whether Parse reads the harness's event.
func KnownEvent(harness, event string) bool {
	_, ok := parse(harness, event, []byte("{}"))

	return ok
}

func parse(harness, event string, payload []byte) (Event, bool) {
	o, ok := decode(bytes.TrimSpace(payload))
	if !ok {
		return Event{}, false
	}
	switch harness {
	case "claude-code":
		switch event {
		case "PostToolUseFailure":
			input := o.raw("tool_input")
			ev := failure(o.str("session_id"), o.str("cwd"), o.str("tool_name"), input, commandOf(o.sub("tool_input"), "command"))
			msg := o.str("error")
			if ev.ExitCode = exitFromError(msg); ev.ExitCode >= 0 {
				ev.ErrorClass = "exit"
			}
			if o.boolean("is_interrupt") {
				ev.Ignore = IgnoreInterrupt
			}

			return ev, true
		case "Stop":
			return turnEnd(o.str("session_id"), o.str("cwd")), true
		}
	case "codex", "devin":
		if event == "Stop" {
			return turnEnd(o.str("session_id"), o.str("cwd")), true
		}
	case "cursor":
		sid := o.str("conversation_id", "session_id")
		cwd := o.str("cwd")
		if cwd == "" {
			cwd = o.firstString("workspace_roots")
		}
		switch event {
		case "postToolUseFailure":
			ev := failure(sid, cwd, o.str("tool_name"), o.raw("tool_input"), commandOf(o.sub("tool_input"), "command"))
			if ft := o.str("failure_type"); ft != "" {
				ev.ErrorClass = ft
			}
			switch {
			case o.boolean("is_interrupt") || strings.Contains(ev.ErrorClass, "interrupt") || ev.ErrorClass == "aborted" || ev.ErrorClass == "cancelled":
				ev.Ignore = IgnoreInterrupt
			case ev.ErrorClass == "permission_denied":
				ev.Ignore = IgnorePermission
			}

			return ev, true
		case "stop":
			return turnEnd(sid, cwd), true
		}
	case "copilot":
		sid := o.str("sessionId", "session_id")
		cwd := o.str("cwd")
		switch event {
		case "postToolUseFailure", "PostToolUseFailure":
			input := o.raw("toolArgs", "tool_input")
			args := o.sub("toolArgs", "tool_input")
			if s, isStr := asString(input); isStr {
				input = json.RawMessage(s)
			}
			ev := failure(sid, cwd, o.str("toolName", "tool_name"), input, commandOf(args, "command"))
			if ev.ExitCode = exitFromError(o.str("error")); ev.ExitCode >= 0 {
				ev.ErrorClass = "exit"
			}

			return ev, true
		case "agentStop", "Stop":
			return turnEnd(sid, cwd), true
		}
	case "antigravity":
		sid := o.str("conversationId")
		cwd := o.firstString("workspacePaths")
		switch event {
		case "PostToolUse":
			if o.str("error") == "" {
				return Event{SessionID: sid, Cwd: cwd, ExitCode: -1}, true
			}
			call := o.sub("toolCall")
			var input json.RawMessage
			tool := ""
			var args object
			if call != nil {
				input = call.raw("args")
				tool = call.str("name")
				args = call.sub("args")
			}

			return failure(sid, cwd, tool, input, commandOf(args, "CommandLine", "command", "Command")), true
		case "PreInvocation":
			return Event{Class: ClassDeliver, SessionID: sid, Cwd: cwd, ExitCode: -1}, true
		case "Stop":
			return turnEnd(sid, cwd), true
		}
	case "opencode":
		sid := o.str("sessionID")
		cwd := o.str("cwd")
		switch event {
		case "tool.execute.after":
			ev := failure(sid, cwd, o.str("tool"), o.raw("args"), commandOf(o.sub("args"), "command"))
			var exit int
			if raw, ok := o["exit"]; ok && json.Unmarshal(raw, &exit) == nil {
				if exit == 0 {
					return Event{SessionID: sid, Cwd: cwd, ExitCode: 0}, true
				}
				ev.ExitCode, ev.ErrorClass = exit, "exit"
			}

			return ev, true
		case "session.idle":
			return Event{Class: ClassDeliver, SessionID: sid, Cwd: cwd, ExitCode: -1, EndsTurn: true}, true
		}
	case "omp", "pi":
		sid := o.str("session_id")
		cwd := o.str("cwd")
		end := "agent_end"
		if harness == "pi" {
			end = "agent_settled"
		}
		switch event {
		case "tool_result":
			if raw, ok := o["is_error"]; ok && string(raw) == "false" {
				return Event{SessionID: sid, Cwd: cwd, ExitCode: -1}, true
			}

			return failure(sid, cwd, o.str("tool"), o.raw("input"), commandOf(o.sub("input"), "command")), true
		case end:
			return turnEnd(sid, cwd), true
		}
	}

	return Event{}, false
}

// CanDeliver reports whether the harness reads a note from the hook's
// output at this event.
func CanDeliver(harness, event string) bool {
	switch harness + " " + event {
	case "claude-code PostToolUseFailure", "cursor postToolUseFailure", "copilot postToolUseFailure", "copilot PostToolUseFailure",
		"omp tool_result", "pi tool_result", "antigravity PreInvocation", "opencode session.idle":
		return true
	}

	return false
}
