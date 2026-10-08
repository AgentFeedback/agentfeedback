package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// HarnessGeminiCLI is the registry name of Gemini CLI.
const HarnessGeminiCLI = "gemini-cli"

// geminiReader reads Gemini CLI's JSONL session logs, in the format of
// google-gemini/gemini-cli v0.63.0 (packages/core/src/services/
// chatRecordingService.ts and chatRecordingTypes.ts).
//
// Sessions live at <GeminiDir>/tmp/<slug>/chats/session-<timestamp>-<short
// id>.jsonl, where <slug> names the project (a slug of its folder name, or
// a SHA-256 hex directory in older releases) and <slug>/.project_root holds
// the project's absolute path. Sub-agent sessions under
// chats/<parent session id>/ are not read. Single-document session files
// chats/session-*.json of releases before v0.39.0 are listed and reported
// unsupported-format.
//
// A file is a metadata line, then message records appended as they change:
// the same message is written again under the same id each time it
// changes, the last write wins and the first write fixes its place. One
// entry stands for each message id, at the offset of its first line; a
// later line with that id updates the entry's text, model and tool calls
// in place, so a rewrite after a watermark adds no entry. A tool call's
// result offset is the first line that records it in a terminal status,
// and later rewrites never move it. Control records ({"$set":…},
// {"$rewindTo":…}, {"$patch":…}) and the metadata line are parsed lines,
// not entries; a $set that carries "messages" is read as those messages
// written again at its line. Messages a $rewindTo removes from Gemini
// CLI's view are kept: they happened. A $patch is not applied.
type geminiReader struct{}

func init() { register(geminiReader{}) }

func (geminiReader) harness() string { return HarnessGeminiCLI }
func (geminiReader) name() string    { return "gemini-cli-jsonl" }

func (geminiReader) location(env Env) string {
	return filepath.Join(env.GeminiDir, "tmp")
}

// candidates lists <tmp>/<slug>/chats/session-*.jsonl and session-*.json
// from directory entries, with the project root read from
// <slug>/.project_root unless collection is disabled; no session file is
// opened. A slug directory
// without chats is skipped. A chats directory that cannot be listed, or a
// .project_root that exists but cannot be read, is reported in problems
// and its sessions are not listed. An unset GeminiDir means no store.
func (r geminiReader) candidates(env Env) ([]candidate, []problem, error) {
	if env.GeminiDir == "" {
		return nil, nil, fs.ErrNotExist
	}
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
		chats := filepath.Join(root, d.Name(), "chats")
		files, err := os.ReadDir(chats)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = append(problems, problem{path: chats, dir: true, reason: ioReason(err)})

			continue
		}
		cwd := ""
		if !env.Policy.Disabled {
			if cwd, err = projectRoot(filepath.Join(root, d.Name(), ".project_root")); err != nil {
				problems = append(problems, problem{path: chats, dir: true, reason: ".project_root: " + ioReason(err)})

				continue
			}
		}
		for _, f := range files {
			name := f.Name()
			if !f.Type().IsRegular() || !strings.HasPrefix(name, "session-") {
				continue
			}
			stem, ok := strings.CutSuffix(name, ".jsonl")
			if !ok {
				if stem, ok = strings.CutSuffix(name, ".json"); !ok {
					continue
				}
			}
			p := filepath.Join(chats, name)
			info, err := f.Info()
			if err != nil {
				problems = append(problems, problem{path: p, reason: ioReason(err)})

				continue
			}
			out = append(out, candidate{sessionID: stem, path: p, project: d.Name(), cwd: cwd, mtime: info.ModTime()})
		}
	}

	return out, problems, nil
}

// projectRootMax is the largest .project_root read.
const projectRootMax = 4 << 10

// projectRoot reads a .project_root file: its trimmed contents when they
// are an absolute path, "" when the file is missing, is not a regular file
// (after symlinks), is larger than projectRootMax or holds anything else.
func projectRoot(p string) (string, error) {
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > projectRootMax {
		return "", nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, projectRootMax+1))
	if err != nil {
		return "", err
	}
	if len(b) > projectRootMax {
		return "", nil
	}
	s := strings.TrimSpace(string(b))
	if !filepath.IsAbs(s) {
		return "", nil
	}

	return s, nil
}

// gate applies the user policy to the project root.
func (geminiReader) gate(env Env, c candidate) (state, reason string) { return gateCwd(env, c) }

const (
	geminiShellTool   = "run_shell_command"
	geminiUserDenied  = "[Operation Cancelled] Reason: User denied execution."
	geminiPolicyDeny  = "Tool execution denied by policy."
	geminiSessionCtx  = "<session_context>"
	geminiFuncRespKey = "functionResponse"
)

type gmPart struct {
	FunctionResponse *struct {
		Response struct {
			Output json.RawMessage `json:"output"`
			Error  json.RawMessage `json:"error"`
		} `json:"response"`
	} `json:"functionResponse"`
}

type gmToolCall struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args"`
	Result json.RawMessage `json:"result"`
	Status string          `json:"status"`
}

func (r geminiReader) load(env Env, c candidate) (*transcript, error) {
	if strings.HasSuffix(c.path, ".json") {
		return nil, fmt.Errorf("%w: legacy single-document session file", errUnsupported)
	}

	return loadFile(env, c, func(rd io.Reader) (*transcript, error) { return r.parse(rd, c.cwd) })
}

// geminiParse is the state of one read: the entry index, the calls of each
// message id, and the directories the metadata line records.
type geminiParse struct {
	t     *transcript
	cwd   string
	index map[string]int
	calls map[string]map[string]*call
	dirs  []string
}

// parse reads the complete lines of r into a transcript; every entry gets
// cwd, the project root, since the file records none. The absolute paths
// of the metadata's directories member are recorded after the read, after
// the project root.
func (geminiReader) parse(r io.Reader, cwd string) (*transcript, error) {
	t := &transcript{}
	g := &geminiParse{t: t, cwd: t.addCwd(cwd), index: map[string]int{}, calls: map[string]map[string]*call{}}
	if err := scanJSONL(r, t, g.line); err != nil {
		return nil, err
	}
	for _, d := range g.dirs {
		t.addCwd(d)
	}
	t.summarise(true)

	return t, nil
}

// line reads one line and reports whether it is a record of a known shape.
func (g *geminiParse) line(offset int64, body []byte) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return false
	}
	if raw, ok := obj["$set"]; ok {
		var set map[string]json.RawMessage
		if json.Unmarshal(raw, &set) != nil || set == nil {
			return false
		}
		var msgs []map[string]json.RawMessage
		if json.Unmarshal(set["messages"], &msgs) == nil {
			for _, m := range msgs {
				g.message(offset, m)
			}
		}

		return true
	}
	if raw, ok := obj["$rewindTo"]; ok {
		var id string

		return json.Unmarshal(raw, &id) == nil
	}
	if raw, ok := obj["$patch"]; ok {
		var patch map[string]json.RawMessage

		return json.Unmarshal(raw, &patch) == nil && patch != nil
	}
	if _, ok := obj["type"]; ok {
		return g.message(offset, obj)
	}
	var sid string
	if json.Unmarshal(obj["sessionId"], &sid) != nil || sid == "" {
		return false
	}
	if g.t.sessionID == "" {
		g.t.sessionID = sid
	}
	var dirs []string
	if json.Unmarshal(obj["directories"], &dirs) == nil {
		g.dirs = append(g.dirs, dirs...)
	}
	var start string
	if json.Unmarshal(obj["startTime"], &start) == nil {
		if ts, err := time.Parse(time.RFC3339Nano, start); err == nil {
			g.t.seen(ts)
		}
	}

	return true
}

// message adds or updates the entry of one message record and reports
// whether the record has a string id and type.
func (g *geminiParse) message(offset int64, m map[string]json.RawMessage) bool {
	var id, typ string
	if json.Unmarshal(m["id"], &id) != nil || id == "" || json.Unmarshal(m["type"], &typ) != nil {
		return false
	}
	t := g.t
	i, ok := g.index[id]
	if !ok {
		i = len(t.entries)
		g.index[id] = i
		g.calls[id] = map[string]*call{}
		t.entries = append(t.entries, entry{offset: offset, span: id, cwd: g.cwd, kind: kindOther})
	}
	e := &t.entries[i]
	var ts string
	if json.Unmarshal(m["timestamp"], &ts) == nil {
		if at, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			at = t.seen(at)
			if e.at.IsZero() {
				e.at = at
			}
		}
	}
	text, onlyResponses := geminiContent(m["content"])
	e.kind, e.text = kindOther, ""
	switch typ {
	case "user":
		if !onlyResponses && !strings.HasPrefix(text, geminiSessionCtx) && !geminiSlashCommand(text) && strings.TrimSpace(text) != "" {
			e.kind, e.text = kindPrompt, text
		}
	case "gemini":
		var model string
		if json.Unmarshal(m["model"], &model) == nil && model != "" {
			e.model = model
		}
		var records []gmToolCall
		_ = json.Unmarshal(m["toolCalls"], &records)
		for n, rec := range records {
			g.updateCall(offset, id, n, e, rec)
		}
		switch {
		case len(e.calls) > 0:
			e.kind, e.text = kindToolCall, text
		case strings.TrimSpace(text) != "":
			e.kind, e.text = kindAssistantText, text
		}
	}

	return true
}

// updateCall adds or updates the call rec of message id; the first terminal
// status a call is written with is final.
func (g *geminiParse) updateCall(offset int64, id string, n int, e *entry, rec gmToolCall) {
	key := rec.ID
	if key == "" {
		key = fmt.Sprintf("#%d", n)
	}
	c, ok := g.calls[id][key]
	if !ok {
		c = &call{id: rec.ID}
		g.calls[id][key] = c
		e.calls = append(e.calls, c)
	}
	if rec.Name != "" {
		c.tool = rec.Name
	}
	if len(rec.Args) > 0 {
		c.input = rec.Args
		var in struct {
			Command *string `json:"command"`
		}
		if json.Unmarshal(rec.Args, &in) == nil && in.Command != nil {
			c.command, c.hasCommand = *in.Command, true
		} else {
			c.command, c.hasCommand = "", false
		}
	}
	if c.result {
		return
	}
	switch rec.Status {
	case "success", "error", "cancelled":
	default:
		// validating, scheduled, executing, awaiting_approval: no result yet.
		return
	}
	c.result, c.resultOffset = true, offset
	c.content = geminiResult(rec.Result)
	c.status = geminiStatus(rec.Status, c.content)
	if c.tool == geminiShellTool {
		c.content, c.status, c.exitCode = geminiShell(c.content, c.status)
	}
}

// geminiContent is a message's text: a string, or the text parts of a
// Part array joined by newlines, thought parts left out. onlyResponses
// reports an array of functionResponse parts alone.
func geminiContent(raw json.RawMessage) (text string, onlyResponses bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, false
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var texts []string
	responses, others := 0, 0
	for _, raw := range parts {
		if _, ok := raw[geminiFuncRespKey]; ok {
			responses++

			continue
		}
		others++
		var s string
		var thought bool
		_ = json.Unmarshal(raw["thought"], &thought)
		if json.Unmarshal(raw["text"], &s) == nil && !thought {
			texts = append(texts, s)
		}
	}

	return strings.Join(texts, "\n"), responses > 0 && others == 0
}

// geminiResult is the text of a tool call's result parts: each
// functionResponse's response.error when it is a string, else its
// response.output when it is a string, joined by newlines.
func geminiResult(raw json.RawMessage) string {
	var parts []gmPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.FunctionResponse == nil {
			continue
		}
		var s string
		if json.Unmarshal(p.FunctionResponse.Response.Error, &s) == nil {
			texts = append(texts, s)

			continue
		}
		if json.Unmarshal(p.FunctionResponse.Response.Output, &s) == nil {
			texts = append(texts, s)
		}
	}

	return strings.Join(texts, "\n")
}

// geminiSlash is the first token of a slash command.
var geminiSlash = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_-]*$`)

// geminiSlashCommand reports whether a user text is a slash command: its
// first token is "/" and a command name.
func geminiSlashCommand(text string) bool {
	f := strings.Fields(text)

	return len(f) > 0 && geminiSlash.MatchString(f[0])
}

var (
	geminiExitLine   = regexp.MustCompile(`^Exit Code: (-?\d+)\s*$`)
	geminiSignalLine = regexp.MustCompile(`^Signal: \S`)
	// geminiTrailerLines start the lines the shell tool appends after the
	// command's output.
	geminiTrailerLines = []string{"Error: ", "Exit Code: ", "Signal: ", "Background PIDs: ", "Process Group PGID: "}
)

// geminiTrailer is the trailer of a shell result: the maximal run of final
// lines that each start with one of geminiTrailerLines.
func geminiTrailer(text string) []string {
	lines := strings.Split(strings.TrimRight(text, "\r\n"), "\n")
	i := len(lines)
	for i > 0 {
		l := strings.TrimRight(lines[i-1], "\r")
		if !slices.ContainsFunc(geminiTrailerLines, func(p string) bool { return strings.HasPrefix(l, p) }) {
			break
		}
		i--
	}

	return lines[i:]
}

// geminiShell reads a run_shell_command result. The shell tool returns a
// command that exits non-zero normally, so the record's status says
// success: in the trailer the tool appends (geminiTrailer), the last
// "Exit Code: N" line with N != 0 makes an ok or error call an error with
// exit code N, and a "Signal: …" line without one an error with none. Lines
// the command printed are not read. A cancelled call keeps its status. The
// text loses its <untrusted_context> wrapper and its leading "Output: ",
// so it starts with the command's output.
func geminiShell(content, status string) (string, string, *int) {
	text := content
	if rest, ok := strings.CutPrefix(text, "<untrusted_context>\n"); ok {
		rest = strings.TrimSuffix(strings.TrimRight(rest, "\n"), "</untrusted_context>")
		text = strings.TrimRight(rest, "\n")
	}
	text = strings.TrimPrefix(text, "Output: ")
	if status != statusOK && status != statusError {
		return text, status, nil
	}
	var exit *int
	signal := false
	for _, l := range geminiTrailer(text) {
		l = strings.TrimRight(l, "\r")
		if m := geminiExitLine.FindStringSubmatch(l); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				exit = &n
			}
		}
		if geminiSignalLine.MatchString(l) {
			signal = true
		}
	}
	if exit != nil && *exit != 0 {
		return text, statusError, exit
	}
	if signal {
		return text, statusError, nil
	}

	return text, status, nil
}

// geminiStatus classifies a terminal status: a cancellation the user
// denied, or an error the policy denied, is a denial; any other
// cancellation an interruption. Gemini CLI records no exit code.
func geminiStatus(status, content string) string {
	switch status {
	case "success":
		return statusOK
	case "cancelled":
		if strings.Contains(content, geminiUserDenied) {
			return statusDenied
		}

		return statusInterrupted
	default:
		if strings.Contains(content, geminiPolicyDeny) {
			return statusDenied
		}

		return statusError
	}
}
