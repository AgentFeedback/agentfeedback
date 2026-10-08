package sessions

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// HarnessClaudeCode is the registry name of Claude Code.
const HarnessClaudeCode = "claude-code"

// claudeReader reads Claude Code's JSONL session logs.
type claudeReader struct{}

func init() { register(claudeReader{}) }

func (claudeReader) harness() string { return HarnessClaudeCode }
func (claudeReader) name() string    { return "claude-code-jsonl" }

func (claudeReader) location(env Env) string {
	return filepath.Join(env.ClaudeConfigDir, "projects")
}

// candidates lists <projects>/<dir>/<id>.jsonl from directory entries alone;
// no session file is opened. A project directory that cannot be listed, or
// a session file whose entry cannot be read, is reported in problems and
// its sessions are not listed.
func (r claudeReader) candidates(env Env) ([]candidate, []problem, error) {
	root := r.location(env)
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	var out []candidate
	var problems []problem
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			problems = append(problems, problem{path: dir, dir: true, reason: ioReason(err)})

			continue
		}
		for _, f := range files {
			if !f.Type().IsRegular() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			p := filepath.Join(dir, f.Name())
			info, err := f.Info()
			if err != nil {
				problems = append(problems, problem{path: p, reason: ioReason(err)})

				continue
			}
			out = append(out, candidate{
				sessionID: strings.TrimSuffix(f.Name(), ".jsonl"),
				path:      p,
				project:   d.Name(),
				mtime:     info.ModTime(),
			})
		}
	}

	return out, problems, nil
}

// encodeProject is Claude Code's project directory name for a path: every
// byte outside [A-Za-z0-9] becomes '-'.
func encodeProject(p string) string {
	b := []byte(p)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}

	return string(b)
}

// encodedWithin reports whether the encoded directory name dir is the
// encoded form of base or lies beneath it; fold compares without case. A
// filesystem root contains every directory.
func encodedWithin(dir, base string, fold bool) bool {
	c := filepath.Clean(base)
	if filepath.Dir(c) == c {
		return true
	}
	e := encodeProject(c)
	if fold {
		dir, e = strings.ToLower(dir), strings.ToLower(e)
	}

	return dir == e || strings.HasPrefix(dir, e+"-")
}

// withinAny reports whether the encoded directory name dir lies within one
// of paths, each taken as written and, when it differs, with its symbolic
// links resolved: Claude Code records the cwd as its process saw it. fold
// compares without case.
func withinAny(dir string, paths []string, fold bool) bool {
	for _, p := range paths {
		if encodedWithin(dir, p, fold) {
			return true
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != filepath.Clean(p) && encodedWithin(dir, resolved, fold) {
			return true
		}
	}

	return false
}

// gate applies the user policy to the project directory name.
func (claudeReader) gate(env Env, c candidate) (state, reason string) {
	if env.Policy.Disabled {
		return StateDisabled, collect.ReasonDisabled
	}
	deny, optIn := collect.UserPaths(env.Policy, env.Home)
	fold := collect.FoldCase()
	if withinAny(c.project, deny, fold) {
		return StateDenied, collect.ReasonDenyPaths
	}
	if env.Policy.OptInOnly && !withinAny(c.project, optIn, fold) {
		return StateDenied, collect.ReasonOptInOnly
	}

	return "", ""
}

type ccLine struct {
	Type          json.RawMessage `json:"type"`
	UUID          string          `json:"uuid"`
	Timestamp     string          `json:"timestamp"`
	SessionID     string          `json:"sessionId"`
	Cwd           string          `json:"cwd"`
	IsMeta        bool            `json:"isMeta"`
	Message       *ccMessage      `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

type ccMessage struct {
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type ccItem struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

const (
	interruptText = "[Request interrupted by user"
	denyText1     = "The user doesn't want to proceed with this tool use"
	denyText2     = "has been denied"
)

func (r claudeReader) load(env Env, c candidate) (*transcript, error) {
	return loadFile(env, c, r.parse)
}

// parse reads the complete lines of r into a transcript.
func (claudeReader) parse(r io.Reader) (*transcript, error) {
	t := &transcript{}
	calls := map[string]*call{}
	if err := scanJSONL(r, t, func(offset int64, body []byte) bool { return t.addLine(offset, body, calls) }); err != nil {
		return nil, err
	}
	t.summarise(true)

	return t, nil
}

// addLine adds the entry of one line and reports whether it parsed.
func (t *transcript) addLine(offset int64, body []byte, calls map[string]*call) bool {
	var l ccLine
	if err := json.Unmarshal(body, &l); err != nil {
		// A member of an unexpected type: keep the line if its type is
		// still a string.
		var minimal struct {
			Type json.RawMessage `json:"type"`
		}
		if json.Unmarshal(body, &minimal) != nil {
			return false
		}
		l = ccLine{Type: minimal.Type}
	}
	var typ string
	if len(l.Type) == 0 || json.Unmarshal(l.Type, &typ) != nil {
		return false
	}
	e := entry{offset: offset, span: l.UUID, cwd: t.addCwd(l.Cwd), kind: kindOther}
	if ts, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
		e.at = t.seen(ts)
	}
	if l.SessionID != "" && t.sessionID == "" {
		t.sessionID = l.SessionID
	}

	switch typ {
	case "assistant":
		if l.Message == nil {
			break
		}
		e.model = l.Message.Model
		var items []ccItem
		if json.Unmarshal(l.Message.Content, &items) != nil {
			var s string
			if json.Unmarshal(l.Message.Content, &s) == nil && strings.TrimSpace(s) != "" {
				e.kind, e.text = kindAssistantText, s
			}

			break
		}
		var texts []string
		for _, it := range items {
			switch it.Type {
			case "text":
				if strings.TrimSpace(it.Text) != "" {
					texts = append(texts, it.Text)
				}
			case "tool_use":
				c := &call{id: it.ID, tool: it.Name, input: it.Input}
				var in struct {
					Command *string `json:"command"`
				}
				if json.Unmarshal(it.Input, &in) == nil && in.Command != nil {
					c.command, c.hasCommand = *in.Command, true
				}
				e.calls = append(e.calls, c)
				if it.ID != "" {
					calls[it.ID] = c
				}
			}
		}
		switch {
		case len(e.calls) > 0:
			e.kind = kindToolCall
			e.text = strings.Join(texts, "\n")
		case len(texts) > 0:
			e.kind, e.text = kindAssistantText, strings.Join(texts, "\n")
		}
	case "user":
		if l.Message == nil {
			break
		}
		var s string
		if json.Unmarshal(l.Message.Content, &s) == nil {
			if !l.IsMeta {
				e.kind, e.text = userTextKind(s), s
			}

			break
		}
		var items []ccItem
		if json.Unmarshal(l.Message.Content, &items) != nil {
			break
		}
		interrupted := toolInterrupted(l.ToolUseResult)
		var texts []string
		results := false
		for _, it := range items {
			switch it.Type {
			case "tool_result":
				results = true
				c := calls[it.ToolUseID]
				if c == nil {
					continue
				}
				c.result, c.resultOffset = true, offset
				c.content = resultText(it.Content)
				c.status = resultStatus(c.content, it.IsError, interrupted)
			case "text":
				texts = append(texts, it.Text)
			}
		}
		if !results && !l.IsMeta && len(texts) > 0 {
			s := strings.Join(texts, "\n")
			e.kind, e.text = userTextKind(s), s
		}
	}
	if e.kind == kindPrompt && strings.TrimSpace(e.text) == "" {
		e.kind = kindOther
	}
	t.entries = append(t.entries, e)

	return true
}

func userTextKind(s string) string {
	if strings.HasPrefix(strings.TrimSpace(s), interruptText) {
		return kindInterrupt
	}

	return kindPrompt
}

// toolInterrupted reads toolUseResult.interrupted; the member may also be a
// string or null.
func toolInterrupted(raw json.RawMessage) bool {
	var r struct {
		Interrupted bool `json:"interrupted"`
	}

	return json.Unmarshal(raw, &r) == nil && r.Interrupted
}

// resultText is a tool_result's content: a string, or the text items of an
// array joined by newlines.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []ccItem
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var texts []string
	for _, it := range items {
		if it.Type == "text" {
			texts = append(texts, it.Text)
		}
	}

	return strings.Join(texts, "\n")
}

func resultStatus(content string, isError, interrupted bool) string {
	switch {
	case interrupted:
		return statusInterrupted
	case !isError:
		return statusOK
	case strings.Contains(content, denyText1) || strings.Contains(content, denyText2):
		return statusDenied
	case strings.Contains(content, interruptText):
		return statusInterrupted
	default:
		return statusError
	}
}
