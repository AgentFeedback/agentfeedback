package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// HarnessCodex is the registry name of Codex.
const HarnessCodex = "codex"

// codexReader reads Codex's rollout files, in the format of openai/codex
// rust-v0.161.0: one JSON object per line, {"timestamp", "type",
// "payload"}. Sessions live at <CodexHome>/sessions/YYYY/MM/DD/ and, once
// archived, flat in <CodexHome>/archived_sessions/, named
// rollout-<local time>-<thread id>.jsonl, or
// rollout-<local time>-<thread id>_<rollout id>.jsonl after a revert; the
// session id is the name after the time, without the extension. A
// compressed rollout (.jsonl.zst) is listed unsupported-format and never
// decompressed. A paginated rollout's history_base, a prefix kept in
// another file, is not read.
//
// Prompts come from event_msg user_message and from item_completed
// UserMessage items, never from response_item user messages, which also
// carry injected context; assistant text comes from response_item
// assistant messages only. A tool call's result is its first output line;
// a later item_completed item, patch_apply_end or mcp_tool_call_end with
// the same call id refines its status and exit code, moving the result to
// its line when the status changes. A turn_aborted with reason "interrupted" interrupts every call
// still without a result. Entries without an id of their own take the span
// "L" and their line offset in decimal.
type codexReader struct{}

func init() { register(codexReader{}) }

func (codexReader) harness() string { return HarnessCodex }
func (codexReader) name() string    { return "codex-rollout" }

func (codexReader) location(env Env) string {
	return filepath.Join(env.CodexHome, "sessions")
}

const (
	codexPrefix     = "rollout-"
	codexTimeLayout = "2006-01-02T15-04-05"
	codexZstSuffix  = ".jsonl.zst"
	// codexFirstLineCap bounds the first line gate reads: the session
	// metadata may carry long base instructions.
	codexFirstLineCap = 1 << 20
)

// codexSessionID is the session id of a rollout file name, ok false when
// the name is not a rollout's.
func codexSessionID(name string) (id string, compressed, ok bool) {
	stem, found := strings.CutSuffix(name, codexZstSuffix)
	compressed = found
	if !found {
		if stem, found = strings.CutSuffix(name, ".jsonl"); !found {
			return "", false, false
		}
	}
	stem, found = strings.CutPrefix(stem, codexPrefix)
	n := len(codexTimeLayout)
	if !found || len(stem) < n+2 || stem[n] != '-' {
		return "", false, false
	}
	if _, err := time.Parse(codexTimeLayout, stem[:n]); err != nil {
		return "", false, false
	}

	return stem[n+1:], compressed, true
}

// candidates lists the rollout files under sessions (any depth) and
// archived_sessions (flat) from directory entries alone; no rollout is
// opened. The store is absent when neither directory exists. A directory
// that cannot be listed, or a rollout whose entry cannot be read, is
// reported in problems and its sessions are not listed.
func (r codexReader) candidates(env Env) ([]candidate, []problem, error) {
	root := r.location(env)
	archived := filepath.Join(env.CodexHome, "archived_sessions")
	var out []candidate
	var problems []problem
	add := func(dir string, f fs.DirEntry) {
		if !f.Type().IsRegular() {
			return
		}
		id, _, ok := codexSessionID(f.Name())
		if !ok {
			return
		}
		p := filepath.Join(dir, f.Name())
		info, err := f.Info()
		if err != nil {
			problems = append(problems, problem{path: p, reason: ioReason(err)})

			return
		}
		out = append(out, candidate{sessionID: id, path: p, mtime: info.ModTime(), gated: &gatedMeta{}})
	}

	_, rootErr := os.Stat(root)
	_, archErr := os.Stat(archived)
	if errors.Is(rootErr, fs.ErrNotExist) && errors.Is(archErr, fs.ErrNotExist) {
		return nil, nil, rootErr
	}
	if rootErr == nil {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == root {
					return err
				}
				problems = append(problems, problem{path: p, dir: true, reason: ioReason(err)})

				return fs.SkipDir
			}
			if !d.IsDir() {
				add(filepath.Dir(p), d)
			}

			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	} else if !errors.Is(rootErr, fs.ErrNotExist) {
		return nil, nil, rootErr
	}
	if archErr == nil {
		files, err := os.ReadDir(archived)
		if err != nil {
			problems = append(problems, problem{path: archived, dir: true, reason: ioReason(err)})
		}
		for _, f := range files {
			add(archived, f)
		}
	} else if !errors.Is(archErr, fs.ErrNotExist) {
		problems = append(problems, problem{path: archived, dir: true, reason: ioReason(archErr)})
	}
	// A session in both directories is listed from sessions.
	live := map[string]string{}
	for _, c := range out {
		if filepath.Dir(c.path) != archived {
			if _, ok := live[c.sessionID]; !ok {
				live[c.sessionID] = c.path
			}
		}
	}
	kept := out[:0]
	for _, c := range out {
		if prev, ok := live[c.sessionID]; ok && filepath.Dir(c.path) == archived {
			problems = append(problems, problem{path: c.path, reason: "session " + c.sessionID + " is also in " + prev + "; listed from there"})

			continue
		}
		kept = append(kept, c)
	}

	return kept, problems, nil
}

// gatedMeta is the session metadata the gate read: its cwd, and whether
// the first line could not be read as session metadata.
type gatedMeta struct {
	cwd    string
	failed bool
}

// codexMetaUnread is the reason a session is refused when the policy
// restricts paths and its metadata could not be read.
const codexMetaUnread = "the session metadata could not be read, so the policy could not be checked"

// errCodexChanged: the rollout's metadata changed between the gate and the
// read.
var errCodexChanged = errors.New("the session changed since its policy was checked")

// gate applies the user policy before the rollout is read: unless
// collection is disabled, the first line (the session metadata) is read
// before the policy is applied, and its cwd is checked like a recorded
// one. A first line that is not session metadata, does not decode or is
// longer than codexFirstLineCap refuses the session as denied when the
// policy has deny_paths or opt_in_only, else leaves only the user-level
// switch.
func (codexReader) gate(env Env, c candidate) (state, reason string) {
	if env.Policy.Disabled || strings.HasSuffix(c.path, codexZstSuffix) {
		return gateCwd(env, c)
	}
	var m gatedMeta
	if f, err := env.open(c.path); err == nil {
		_, m.cwd, m.failed, _ = codexFirstLine(f)
		_ = f.Close()
	}
	if c.gated != nil {
		*c.gated = m
	}
	if m.failed && (len(env.Policy.DenyPaths) > 0 || env.Policy.OptInOnly) {
		return StateDenied, codexMetaUnread
	}
	c.cwd = m.cwd

	return gateCwd(env, c)
}

// codexFirstLine reads the first line of r one byte at a time, so nothing
// after its '\n' is consumed, and at most codexFirstLineCap+1 bytes. It
// returns the bytes read, the absolute cwd of the session metadata ("" when
// it records none), and failed when the line is incomplete, longer than
// the cap, not session metadata or does not decode.
func codexFirstLine(r io.Reader) (line []byte, cwd string, failed bool, err error) {
	var b [1]byte
	complete := false
	for len(line) <= codexFirstLineCap {
		n, rerr := r.Read(b[:])
		if n == 1 {
			line = append(line, b[0])
			if b[0] == '\n' {
				complete = true

				break
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return line, "", true, rerr
		}
	}
	if !complete {
		return line, "", true, nil
	}
	var l struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type != "session_meta" {
		return line, "", true, nil
	}
	if filepath.IsAbs(l.Payload.Cwd) {
		cwd = l.Payload.Cwd
	}

	return line, cwd, false, nil
}

// errCompressed is a compressed rollout: its state's reason is the error's
// text alone.
type errCompressed struct{}

func (errCompressed) Error() string        { return "compressed rollout" }
func (errCompressed) Is(target error) bool { return target == errUnsupported }

// load reads the first line, refuses the session when its metadata cwd is
// not the one the gate checked, and then parses the whole rollout.
func (r codexReader) load(env Env, c candidate) (*transcript, error) {
	if strings.HasSuffix(c.path, codexZstSuffix) {
		return nil, errCompressed{}
	}

	return loadFile(env, c, func(f io.Reader) (*transcript, error) {
		first, cwd, _, err := codexFirstLine(f)
		if err != nil {
			return nil, err
		}
		var gated gatedMeta
		if c.gated != nil {
			gated = *c.gated
		}
		if cwd != gated.cwd {
			return nil, errCodexChanged
		}

		return r.parse(io.MultiReader(bytes.NewReader(first), f))
	})
}

type cxLine struct {
	Timestamp string          `json:"timestamp"`
	Type      json.RawMessage `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type cxPayload struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Cwd       string          `json:"cwd"`
	Model     string          `json:"model"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Message   json.RawMessage `json:"message"`
	Reason    string          `json:"reason"`
	Item      *cxItem         `json:"item"`
	Status    string          `json:"status"`
	Success   *bool           `json:"success"`
	Stdout    string          `json:"stdout"`
	Stderr    string          `json:"stderr"`
	Result    json.RawMessage `json:"result"`
}

type cxItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Content          json.RawMessage `json:"content"`
	Status           string          `json:"status"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Stdout           string          `json:"stdout"`
	Stderr           string          `json:"stderr"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type cxText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// codexParse is the state of one rollout's read.
type codexParse struct {
	t     *transcript
	calls map[string]*call
	open  []*call // calls without a result, in order
}

// parse reads the complete lines of r into a transcript.
func (codexReader) parse(r io.Reader) (*transcript, error) {
	p := &codexParse{t: &transcript{}, calls: map[string]*call{}}
	if err := scanJSONL(r, p.t, p.line); err != nil {
		return nil, err
	}
	p.t.summarise(true)

	return p.t, nil
}

// line adds the entry of one line and reports whether it parsed: an object
// whose type is a string.
func (p *codexParse) line(offset int64, body []byte) bool {
	var l cxLine
	if err := json.Unmarshal(body, &l); err != nil {
		var minimal struct {
			Type json.RawMessage `json:"type"`
		}
		if json.Unmarshal(body, &minimal) != nil {
			return false
		}
		l = cxLine{Type: minimal.Type}
	}
	var typ string
	if len(l.Type) == 0 || json.Unmarshal(l.Type, &typ) != nil {
		return false
	}
	t := p.t
	e := entry{offset: offset, span: "L" + strconv.FormatInt(offset, 10), kind: kindOther}
	if ts, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
		e.at = t.seen(ts)
	}
	var pl cxPayload
	if json.Unmarshal(l.Payload, &pl) != nil {
		pl = cxPayload{}
	}
	switch typ {
	case "session_meta":
		if pl.ID != "" && t.sessionID == "" {
			t.sessionID = pl.ID
		}
		t.addCwd(pl.Cwd)
	case "turn_context":
		t.addCwd(pl.Cwd)
		e.model = pl.Model
	case "response_item":
		p.responseItem(&e, offset, pl)
	case "event_msg":
		p.eventMsg(&e, offset, pl)
	}
	e.cwd = t.lastCwd
	if e.kind == kindPrompt && strings.TrimSpace(e.text) == "" {
		e.kind = kindOther
	}
	t.entries = append(t.entries, e)

	return true
}

func (p *codexParse) responseItem(e *entry, offset int64, pl cxPayload) {
	switch pl.Type {
	case "message":
		if pl.Role != "assistant" {
			return
		}
		if s := joinTexts(pl.Content, "output_text"); s != "" {
			e.kind, e.text = kindAssistantText, s
		}
	case "function_call":
		c := &call{id: pl.CallID, tool: pl.Name, input: codexArguments(pl.Arguments)}
		switch pl.Name {
		case "exec_command":
			var in struct {
				Cmd *string `json:"cmd"`
			}
			if json.Unmarshal(c.input, &in) == nil && in.Cmd != nil {
				c.command, c.hasCommand = *in.Cmd, true
			}
		case "shell":
			var in struct {
				Command json.RawMessage `json:"command"`
			}
			if json.Unmarshal(c.input, &in) == nil {
				c.command, c.hasCommand = argvCommand(in.Command)
			}
		}
		p.addCall(e, c)
	case "custom_tool_call":
		in := pl.Input
		if len(in) == 0 {
			in = json.RawMessage("null")
		}
		p.addCall(e, &call{id: pl.CallID, tool: pl.Name, input: in})
	case "local_shell_call":
		c := &call{id: pl.CallID, tool: "local_shell", input: pl.Action}
		if len(c.input) == 0 {
			c.input = json.RawMessage("null")
		}
		var a struct {
			Command json.RawMessage `json:"command"`
		}
		if json.Unmarshal(pl.Action, &a) == nil {
			c.command, c.hasCommand = argvCommand(a.Command)
		}
		p.addCall(e, c)
	case "function_call_output", "custom_tool_call_output":
		c := p.calls[pl.CallID]
		if c == nil || c.result {
			return
		}
		content, status, exit := codexOutput(pl.Output)
		c.result, c.resultOffset = true, offset
		c.content, c.status, c.exitCode = content, status, exit
	}
}

func (p *codexParse) eventMsg(e *entry, offset int64, pl cxPayload) {
	switch pl.Type {
	case "user_message":
		var s string
		if json.Unmarshal(pl.Message, &s) == nil {
			e.kind, e.text = kindPrompt, s
		}
	case "item_completed":
		if pl.Item == nil {
			return
		}
		it := pl.Item
		switch it.Type {
		case "UserMessage":
			e.kind, e.text = kindPrompt, joinTexts(it.Content, "text")
			if it.ID != "" {
				e.span = it.ID
			}
		case "CommandExecution":
			var exit *int
			status := ""
			switch it.Status {
			case "completed":
				status = statusOK
			case "failed":
				status = statusError
				if it.ExitCode != nil && *it.ExitCode >= 0 {
					n := *it.ExitCode
					exit = &n
				}
			case "declined":
				status = statusDenied
			}
			p.refine(it.ID, offset, status, exit, it.AggregatedOutput)
		case "FileChange":
			p.refine(it.ID, offset, codexOutcome(it.Status), nil, joinNonEmpty(it.Stdout, it.Stderr))
		case "McpToolCall":
			msg := ""
			if it.Error != nil {
				msg = it.Error.Message
			}
			p.refine(it.ID, offset, codexOutcome(it.Status), nil, msg)
		}
	case "patch_apply_end":
		status := codexOutcome(pl.Status)
		if status == "" && pl.Success != nil {
			status = statusError
			if *pl.Success {
				status = statusOK
			}
		}
		p.refine(pl.CallID, offset, status, nil, joinNonEmpty(pl.Stdout, pl.Stderr))
	case "mcp_tool_call_end":
		var res struct {
			Ok *struct {
				Content json.RawMessage `json:"content"`
				IsError bool            `json:"isError"`
			} `json:"Ok"`
			Err *string `json:"Err"`
		}
		if json.Unmarshal(pl.Result, &res) != nil {
			return
		}
		switch {
		case res.Err != nil:
			p.refine(pl.CallID, offset, statusError, nil, *res.Err)
		case res.Ok != nil && res.Ok.IsError:
			p.refine(pl.CallID, offset, statusError, nil, joinTexts(res.Ok.Content, "text"))
		case res.Ok != nil:
			p.refine(pl.CallID, offset, statusOK, nil, "")
		}
	case "turn_aborted":
		if pl.Reason != "interrupted" {
			return
		}
		e.kind = kindInterrupt
		for _, c := range p.open {
			if !c.result {
				c.result, c.resultOffset, c.status = true, offset, statusInterrupted
			}
		}
		p.open = nil
	}
}

// addCall makes e the tool call entry of c, spanned by its call id.
func (p *codexParse) addCall(e *entry, c *call) {
	e.kind = kindToolCall
	e.calls = append(e.calls, c)
	if c.id != "" {
		e.span = c.id
		p.calls[c.id] = c
	}
	p.open = append(p.open, c)
}

// refine sets the status of the call id names from a completion record;
// it sets the result offset when no output line came first or the status
// changes, the exit code when the record carries one, and the content only
// when there is none yet. An empty status changes nothing.
func (p *codexParse) refine(id string, offset int64, status string, exit *int, content string) {
	c := p.calls[id]
	if c == nil || status == "" {
		return
	}
	if !c.result || c.status != status {
		c.result, c.resultOffset = true, offset
	}
	c.status = status
	if exit != nil {
		c.exitCode = exit
	}
	if c.content == "" {
		c.content = content
	}
}

// codexOutcome maps a completion status to a call status, "" when the
// call has not finished.
func codexOutcome(s string) string {
	switch s {
	case "completed":
		return statusOK
	case "failed":
		return statusError
	case "declined":
		return statusDenied
	}

	return ""
}

// codexArguments is a function call's input: the decoded object when the
// arguments are a string holding JSON, else the arguments as given.
func codexArguments(raw json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
		b, _ := json.Marshal(s)

		return b
	}
	if len(raw) == 0 {
		return json.RawMessage("null")
	}

	return raw
}

// codexShells and codexShellFlags are the shell wrappers argvCommand
// unwraps.
var (
	codexShells     = []string{"bash", "sh", "zsh", "dash"}
	codexShellFlags = []string{"-c", "-lc", "-cl"}
)

// argvCommand is the command of an argv array: the script of exactly
// [<shell>, <flag>, <script>] with a known shell (by base name) and flag,
// else the elements joined with spaces; a string is taken as is.
func argvCommand(raw json.RawMessage) (string, bool) {
	var argv []string
	if json.Unmarshal(raw, &argv) == nil && argv != nil {
		if len(argv) == 3 && slices.Contains(codexShells, path.Base(argv[0])) && slices.Contains(codexShellFlags, argv[1]) {
			return argv[2], true
		}

		return strings.Join(argv, " "), true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}

	return "", false
}

// joinTexts joins the non-blank text of the content items of type typ.
func joinTexts(raw json.RawMessage, typ string) string {
	var items []cxText
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var texts []string
	for _, it := range items {
		if it.Type == typ && strings.TrimSpace(it.Text) != "" {
			texts = append(texts, it.Text)
		}
	}

	return strings.Join(texts, "\n")
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, s := range parts {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}

	return strings.Join(out, "\n")
}

var (
	codexExitHeader = regexp.MustCompile(`^(?:Process exited with code|Exit code:) (-?\d+)\s*$`)
	codexAborted    = regexp.MustCompile(`(?m)^aborted by user`)
	codexDenials    = []string{
		"exec command rejected by user",
		"patch rejected by user",
		"rejected by configuration",
		"automatic approval review denied the action",
	}
)

// codexOutput reads a tool output: a string, or content items whose text
// is joined. The oldest form is a JSON string {"output", "metadata":
// {"exit_code"}}, classified by its exit code alone. Otherwise only the
// harness's text is classified: the header lines before the first line
// that is exactly "Output:". An exit code of 0 there is ok; a denial or an
// "aborted by user" line in the header decides the status, and without an
// "Output:" line a denial only when the text starts with it; else a
// non-zero exit code makes it an error with that exit code, and anything
// else is ok. The content is the text after the header (codexOutputBody).
func codexOutput(raw json.RawMessage) (content, status string, exit *int) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var items []cxText
		if json.Unmarshal(raw, &items) == nil {
			var texts []string
			for _, it := range items {
				if it.Text != "" {
					texts = append(texts, it.Text)
				}
			}
			s = strings.Join(texts, "\n")
		}
	}
	content = s
	var legacy struct {
		Output   *string `json:"output"`
		Metadata *struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if strings.HasPrefix(strings.TrimSpace(s), "{") && json.Unmarshal([]byte(s), &legacy) == nil && legacy.Output != nil && legacy.Metadata != nil {
		content, exit = *legacy.Output, legacy.Metadata.ExitCode
		status = statusOK
		if exit != nil && *exit != 0 {
			status = statusError
		}

		return codexOutputBody(content), status, exit
	}
	var header []string
	body := false
	for line := range strings.SplitSeq(s, "\n") {
		if line == "Output:" {
			body = true

			break
		}
		header = append(header, line)
		if m := codexExitHeader.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				exit = &n
			}
		}
	}
	head := strings.Join(header, "\n")
	denied := codexDenied(head)
	if !body {
		trimmed := strings.TrimSpace(s)
		denied = slices.ContainsFunc(codexDenials, func(d string) bool { return strings.HasPrefix(trimmed, d) })
	}
	status = statusOK
	switch {
	case exit != nil && *exit == 0:
	case denied:
		status, exit = statusDenied, nil
	case codexAborted.MatchString(head):
		status, exit = statusInterrupted, nil
	case exit != nil:
		status = statusError
	}

	return codexOutputBody(content), status, exit
}

func codexDenied(s string) bool {
	for _, d := range codexDenials {
		if strings.Contains(s, d) {
			return true
		}
	}

	return false
}

// codexOutputBody is the text after the first line that is exactly
// "Output:", the whole text when there is no such line: the header lines
// before it are the tool's, not the command's.
func codexOutputBody(s string) string {
	if strings.HasPrefix(s, "Output:\n") {
		return s[len("Output:\n"):]
	}
	if _, after, ok := strings.Cut(s, "\nOutput:\n"); ok {
		return after
	}
	if s == "Output:" || strings.HasSuffix(s, "\nOutput:") {
		return ""
	}

	return s
}
