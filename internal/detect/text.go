package detect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

// MaxText is the size every note stays under, in bytes.
const MaxText = 600

const (
	maxExcerpt   = 80
	maxTool      = 64
	maxSessionID = 128
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// excerpt is a command fit for the note: known secret formats redacted,
// control characters dropped, whitespace runs collapsed, at most 80 bytes
// with an ellipsis when cut.
func excerpt(command string) string {
	s, _ := scrub.String(command)
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")

	return truncate(s, maxExcerpt)
}

// truncate cuts s to at most n bytes on a rune boundary, ending in an
// ellipsis when it cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ell = "…"
	cut := n - len(ell)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + ell
}

func times(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	}

	return fmt.Sprintf("%d times", n)
}

// facts words f, with the command excerpt when withCommand.
func (f Facts) words(withCommand bool) string {
	span := " in this session"
	if f.SinceLast {
		span = " since the last note"
	}
	var head string
	switch f.Reason {
	case ReasonSameTool:
		head = truncate(oneLine(f.Tool), maxTool) + " failed " + times(f.Count) + span
	case ReasonSameCommand:
		head = "the same command failed " + times(f.Count)
	default:
		head = fmt.Sprintf("%d tool calls failed", f.Count) + span
	}
	var last []string
	if f.Exit >= 0 {
		last = append(last, fmt.Sprintf("exit %d", f.Exit))
	}
	if withCommand {
		if c := excerpt(f.Command); c != "" {
			last = append(last, `"`+c+`"`)
		}
	}
	if len(last) > 0 {
		head += " (last: " + strings.Join(last, ", ") + ")"
	}

	return head
}

func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
}

// shellWord is s as one shell word: as is when it holds only
// [A-Za-z0-9._-], else single-quoted with each ' written as '\”.
func shellWord(s string) string {
	if safeID.MatchString(s) {
		return s
	}

	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CleanSessionID is a session id as the note shows it: control characters
// and Unicode format characters (category Cf) dropped.
func CleanSessionID(id string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}

		return r
	}, id)
}

// Text is the note for a session of the harness. The session_id context
// carries every non-empty id of at most 128 bytes, cleaned by
// CleanSessionID and shell-quoted when needed. When the note would reach
// MaxText bytes the command excerpt is left out first, then the session_id
// context.
func Text(harness, sessionID string, f Facts) string {
	ctx := " --context origin=hook-nudge"
	sid := ""
	if len(sessionID) <= maxSessionID {
		if id := CleanSessionID(sessionID); id != "" {
			sid = " --context session_id=" + shellWord(id)
		}
	}
	hctx := ""
	if len(harness) <= 32 && safeID.MatchString(harness) {
		hctx = " --context session_harness=" + harness
	}
	build := func(withCommand, withSession bool) string {
		c := ctx
		if withSession {
			c += sid
		}

		return "AgentFeedback: " + f.words(withCommand) +
			". If this was friction (a missing or wrong doc, a misbehaving tool, stale config), you can file it with agentfeedback submit friction --summary '<one line>'" +
			c + hctx + "; otherwise ignore this note."
	}
	t := build(true, true)
	if len(t) >= MaxText {
		t = build(false, true)
	}
	if len(t) >= MaxText {
		t = build(false, false)
	}

	return t
}

// Render is the hook's output for a note at the harness's event: what goes
// on stdout and the exit status. An empty text is no note: nothing on
// stdout and exit 0.
func Render(harness, event, text string) ([]byte, int) {
	if text == "" {
		return nil, 0
	}
	enc := func(v any) []byte {
		var buf bytes.Buffer
		e := json.NewEncoder(&buf)
		e.SetEscapeHTML(false)
		_ = e.Encode(v)

		return buf.Bytes()
	}
	switch harness + " " + event {
	case "claude-code PostToolUseFailure":
		return enc(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "PostToolUseFailure", "additionalContext": text}}), 0
	case "cursor postToolUseFailure":
		return enc(map[string]string{"additional_context": text}), 0
	case "copilot postToolUseFailure", "copilot PostToolUseFailure":
		return []byte(text + "\n"), 2
	case "antigravity PreInvocation":
		return enc(map[string]any{"injectSteps": []map[string]string{{"ephemeralMessage": text}}}), 0
	case "opencode session.idle", "omp tool_result", "pi tool_result":
		return enc(map[string]string{"additionalContext": text}), 0
	}

	return nil, 0
}
