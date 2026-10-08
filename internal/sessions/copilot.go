package sessions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// HarnessCopilot is the registry name of the GitHub Copilot CLI.
const HarnessCopilot = "copilot"

// copilotReader reads the GitHub Copilot CLI's session event logs, in the
// format of the session-events schema bundled with @github/copilot 1.0.92:
// <CopilotHome>/session-state/<session id>/events.jsonl, with the session's
// workspace.yaml beside it. The session-store.db index and the legacy flat
// transcripts are not read.
//
// Events that carry an agentId come from sub-agents: they are entries of
// kind other, never read as prompts, calls or results.
type copilotReader struct{}

func init() { register(copilotReader{}) }

func (copilotReader) harness() string { return HarnessCopilot }
func (copilotReader) name() string    { return "copilot-events" }

func (copilotReader) location(env Env) string {
	return filepath.Join(env.CopilotHome, "session-state")
}

const (
	copilotEvents    = "events.jsonl"
	copilotWorkspace = "workspace.yaml"
)

// candidates lists <session-state>/<id>/events.jsonl from directory entries
// and reads each session's workspace.yaml for its cwd: store metadata, not
// session content. No events file is opened. A session directory that
// cannot be listed, or an events file whose entry cannot be read, is
// reported in problems and its session is not listed.
func (r copilotReader) candidates(env Env) ([]candidate, []problem, error) {
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
			if f.Name() != copilotEvents || !f.Type().IsRegular() {
				continue
			}
			p := filepath.Join(dir, f.Name())
			info, err := f.Info()
			if err != nil {
				problems = append(problems, problem{path: p, reason: ioReason(err)})

				continue
			}
			out = append(out, candidate{
				sessionID: d.Name(),
				path:      p,
				cwd:       workspaceCwd(filepath.Join(dir, copilotWorkspace)),
				mtime:     info.ModTime(),
			})
		}
	}

	return out, problems, nil
}

// workspaceMax is the largest workspace.yaml read.
const workspaceMax = 64 << 10

// workspaceCwd is the absolute cwd a workspace.yaml records, "" when the
// file cannot be read, is not a regular file (after symlinks), is larger
// than workspaceMax or records none. Only a top-level `cwd:` scalar
// (plain, single- or double-quoted) is read, or, when the file starts with
// '{', the JSON member cwd: no other YAML is understood.
func workspaceCwd(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > workspaceMax {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, workspaceMax+1))
	if err != nil || len(b) > workspaceMax {
		return ""
	}
	var cwd string
	if trimmed := bytes.TrimSpace(b); len(trimmed) > 0 && trimmed[0] == '{' {
		var w struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(trimmed, &w) == nil {
			cwd = w.Cwd
		}
	} else {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			if v, ok := strings.CutPrefix(strings.TrimRight(sc.Text(), "\r"), "cwd:"); ok {
				cwd = yamlScalar(v)

				break
			}
		}
	}
	cwd = strings.TrimSpace(cwd)
	if !filepath.IsAbs(cwd) {
		return ""
	}

	return cwd
}

// yamlScalar is the value of a one-line YAML scalar: double-quoted with Go
// escapes, single-quoted with ” for a quote, or plain up to a " #"
// comment. A quoted scalar may be followed by a comment.
func yamlScalar(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || (v[0] != '"' && v[0] != '\'') {
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}

		return strings.TrimSpace(v)
	}
	q := v[0]
	end := -1
	for i := 1; i < len(v); i++ {
		switch {
		case q == '"' && v[i] == '\\':
			i++
		case v[i] == q && q == '\'' && i+1 < len(v) && v[i+1] == '\'':
			i++
		case v[i] == q:
			end = i
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return ""
	}
	if rest := v[end+1:]; strings.TrimSpace(rest) != "" {
		if rest[0] != ' ' && rest[0] != '\t' || !strings.HasPrefix(strings.TrimLeft(rest, " \t"), "#") {
			return ""
		}
	}
	if q == '\'' {
		return strings.ReplaceAll(v[1:end], "''", "'")
	}
	s, err := strconv.Unquote(v[:end+1])
	if err != nil {
		return ""
	}

	return s
}

// gate applies the user policy to the workspace.yaml cwd.
func (copilotReader) gate(env Env, c candidate) (state, reason string) {
	return gateCwd(env, c)
}

type cpLine struct {
	Type      json.RawMessage `json:"type"`
	Data      json.RawMessage `json:"data"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	AgentID   string          `json:"agentId"`
}

type cpContext struct {
	Cwd string `json:"cwd"`
}

// cpData holds the members of every event type the reader uses.
type cpData struct {
	SessionID               string          `json:"sessionId"`
	SelectedModel           string          `json:"selectedModel"`
	Context                 *cpContext      `json:"context"`
	Cwd                     string          `json:"cwd"`
	Model                   string          `json:"model"`
	NewModel                string          `json:"newModel"`
	Content                 string          `json:"content"`
	Source                  string          `json:"source"`
	IsAutopilotContinuation bool            `json:"isAutopilotContinuation"`
	ToolCallID              string          `json:"toolCallId"`
	ToolName                string          `json:"toolName"`
	Arguments               json.RawMessage `json:"arguments"`
	ShellToolInfo           json.RawMessage `json:"shellToolInfo"`
	Success                 bool            `json:"success"`
	Result                  *struct {
		Content string `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
	ShellExecution *struct {
		ExitCode *int `json:"exitCode"`
	} `json:"shellExecution"`
}

// cpPermission is permission.completed's data: result is an object here.
type cpPermission struct {
	ToolCallID string `json:"toolCallId"`
	Result     struct {
		Kind string `json:"kind"`
	} `json:"result"`
}

// shellExitLine is the line the shell tool ends its result with.
var shellExitLine = regexp.MustCompile(`^<(?:detached command with )?shellId: [^\r\n]+ completed with exit code (-?\d+)>[ \t]*$`)

// shellNotes start the lines the shell tool may add after its exit line.
var shellNotes = []string{"<sandbox is active ", "<this failure may be caused by t"}

// shellMarker reads the exit code from the last non-empty line of a shell
// result, not counting the note lines after it, and returns the content
// without that line and the notes; content without the line comes back as
// is, with a nil exit code.
func shellMarker(content string) (string, *int) {
	lines := strings.Split(content, "\n")
	i := len(lines) - 1
	for ; i >= 0; i-- {
		l := strings.TrimRight(lines[i], "\r")
		if strings.TrimSpace(l) == "" || slices.ContainsFunc(shellNotes, func(p string) bool { return strings.HasPrefix(l, p) }) {
			continue
		}

		break
	}
	if i < 0 {
		return content, nil
	}
	m := shellExitLine.FindStringSubmatch(strings.TrimRight(lines[i], "\r"))
	if m == nil {
		return content, nil
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return content, nil
	}

	return strings.TrimRight(strings.Join(lines[:i], "\n"), "\r\n"), &n
}

// cpModel is the first string member among newModel, model and
// selectedModel of session.model_change's data.
func cpModel(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"newModel", "model", "selectedModel"} {
		var s string
		if json.Unmarshal(m[k], &s) == nil {
			return s
		}
	}

	return ""
}

// abortedText is the result upstream gives every call pending at an abort.
const abortedText = "Operation aborted by user"

// cpCalls tracks the calls of one session: the call of each toolCallId,
// whether its first execution_complete has been read, and whether it is a
// shell tool's.
type cpCalls struct {
	byID     map[string]*call
	complete map[string]bool
	shell    map[*call]bool
}

// cpShellTools are the shell tools' names.
var cpShellTools = []string{"bash", "powershell"}

func (r copilotReader) load(env Env, c candidate) (*transcript, error) {
	return loadFile(env, c, func(f io.Reader) (*transcript, error) { return r.parse(f, c.cwd) })
}

// parse reads the complete lines of r into a transcript; cwd is the
// workspace.yaml cwd, recorded first.
func (copilotReader) parse(r io.Reader, cwd string) (*transcript, error) {
	t := &transcript{}
	t.addCwd(cwd)
	calls := &cpCalls{byID: map[string]*call{}, complete: map[string]bool{}, shell: map[*call]bool{}}
	if err := scanJSONL(r, t, func(offset int64, body []byte) bool { return t.addCopilotLine(offset, body, calls) }); err != nil {
		return nil, err
	}
	t.summarise(true)

	return t, nil
}

// addCopilotLine adds the entry of one event line and reports whether it
// parsed. Each entry carries the cwd current after its line.
//
// tool.execution_start is a call; the first tool.execution_complete with
// its toolCallId is its result: a failure with error.code "rejected" or
// "denied" is denied; success with no exit code or exit code 0 is ok;
// anything else, success with a non-zero exit code included, an error. The
// exit code is shellExecution.exitCode, else, for a shell tool's call (a
// bash or powershell call, one whose start carries shellToolInfo, or one
// whose complete carries shellExecution), the one in the shell tool's
// closing exit line, which is dropped from the content with the notes
// after it. permission.completed with a "denied-" kind makes a call without
// a result denied and "cancelled" interrupted; abort
// makes every call without a result interrupted. A call so settled before
// its execution_complete keeps its status and result offset; the late
// complete only fills a missing content and exit code, and not even those
// after an abort.
func (t *transcript) addCopilotLine(offset int64, body []byte, calls *cpCalls) bool {
	var l cpLine
	if err := json.Unmarshal(body, &l); err != nil {
		// A member of an unexpected type: keep the line if its type is
		// still a string.
		var minimal struct {
			Type json.RawMessage `json:"type"`
		}
		if json.Unmarshal(body, &minimal) != nil {
			return false
		}
		l = cpLine{Type: minimal.Type}
	}
	var typ string
	if len(l.Type) == 0 || json.Unmarshal(l.Type, &typ) != nil {
		return false
	}
	e := entry{offset: offset, span: l.ID, kind: kindOther}
	if ts, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
		e.at = t.seen(ts)
	}
	if l.AgentID != "" {
		e.cwd = t.lastCwd
		t.entries = append(t.entries, e)

		return true
	}
	var d cpData
	dataOK := json.Unmarshal(l.Data, &d) == nil

	switch {
	case !dataOK:
	case typ == "session.start" || typ == "session.resume":
		if typ == "session.start" && d.SessionID != "" && t.sessionID == "" {
			t.sessionID = d.SessionID
		}
		if d.Context != nil {
			t.addCwd(d.Context.Cwd)
		}
		e.model = d.SelectedModel
	case typ == "session.context_changed":
		t.addCwd(d.Cwd)
	case typ == "session.model_change":
		e.model = cpModel(l.Data)
	case typ == "user.message":
		if (d.Source == "" || d.Source == "user") && !d.IsAutopilotContinuation && strings.TrimSpace(d.Content) != "" {
			e.kind, e.text = kindPrompt, d.Content
		}
	case typ == "assistant.message":
		e.model = d.Model
		if strings.TrimSpace(d.Content) != "" {
			e.kind, e.text = kindAssistantText, d.Content
		}
	case typ == "tool.execution_start":
		e.model = d.Model
		c := &call{id: d.ToolCallID, tool: d.ToolName, input: d.Arguments}
		var in struct {
			Command *string `json:"command"`
		}
		if json.Unmarshal(d.Arguments, &in) == nil && in.Command != nil {
			c.command, c.hasCommand = *in.Command, true
		}
		e.kind, e.calls = kindToolCall, []*call{c}
		if slices.Contains(cpShellTools, d.ToolName) || len(d.ShellToolInfo) > 0 && string(d.ShellToolInfo) != "null" {
			calls.shell[c] = true
		}
		if d.ToolCallID != "" {
			if _, ok := calls.byID[d.ToolCallID]; !ok {
				calls.byID[d.ToolCallID] = c
			}
		}
	case typ == "tool.execution_complete":
		e.model = d.Model
		c := calls.byID[d.ToolCallID]
		if c == nil || calls.complete[d.ToolCallID] {
			break
		}
		calls.complete[d.ToolCallID] = true
		content := ""
		switch {
		case d.Result != nil && d.Result.Content != "":
			content = d.Result.Content
		case d.Error != nil:
			content = d.Error.Message
		}
		var exit *int
		if calls.shell[c] || d.ShellExecution != nil {
			content, exit = shellMarker(content)
		}
		if d.ShellExecution != nil && d.ShellExecution.ExitCode != nil {
			n := *d.ShellExecution.ExitCode
			exit = &n
		}
		if c.result {
			if c.content == "" {
				c.content = content
			}
			if c.exitCode == nil {
				c.exitCode = exit
			}

			break
		}
		c.result, c.resultOffset, c.content, c.exitCode = true, offset, content, exit
		switch {
		case !d.Success && d.Error != nil && (d.Error.Code == "rejected" || d.Error.Code == "denied"):
			c.status = statusDenied
		case d.Success && (exit == nil || *exit == 0):
			c.status = statusOK
		default:
			c.status = statusError
		}
	case typ == "permission.completed":
		var p cpPermission
		if json.Unmarshal(l.Data, &p) != nil {
			break
		}
		c := calls.byID[p.ToolCallID]
		if c == nil || c.result {
			break
		}
		var status string
		switch {
		case strings.HasPrefix(p.Result.Kind, "denied-"):
			status = statusDenied
		case p.Result.Kind == "cancelled":
			status = statusInterrupted
		}
		if status == "" {
			break
		}
		c.status, c.result, c.resultOffset = status, true, offset
	case typ == "abort":
		e.kind = kindInterrupt
		for _, c := range calls.byID {
			if c.result {
				continue
			}
			c.result, c.resultOffset, c.status, c.content = true, offset, statusInterrupted, abortedText
			// An abort settles the call for good.
			calls.complete[c.id] = true
		}
	}
	e.cwd = t.lastCwd
	t.entries = append(t.entries, e)

	return true
}
