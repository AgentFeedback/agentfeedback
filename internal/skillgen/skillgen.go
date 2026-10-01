// Package skillgen renders the submission guidance from one source into every
// form a harness reads. The source is source/skill.json plus one Markdown
// fragment per teaching point; a fragment's text outside channel blocks is
// shared by every form, and a block
//
//	<!-- only: cli http -->
//	...
//	<!-- end -->
//
// is kept only in the forms whose channel it names: cli (the forms that run
// the binary), http (the prompt form, for agents with nothing but HTTP) and
// mcp (the server's MCP instructions). {{server}} is the server's base URL.
package skillgen

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
)

//go:embed source
var sourceFS embed.FS

// The rendered forms.
const (
	FormSkillMD  = "skill-md"  // Agent Skills SKILL.md
	FormAgentsMD = "agents-md" // snippet to paste into an AGENTS.md
	FormCursor   = "cursor"    // Cursor .mdc rule
	FormPrompt   = "prompt"    // plain prompt block with the HTTP call inline
	FormMCP      = "mcp"       // the MCP server's instructions
)

// The channels a fragment block can name.
const (
	ChannelCLI  = "cli"
	ChannelHTTP = "http"
	ChannelMCP  = "mcp"
)

var formChannel = map[string]string{
	FormSkillMD:  ChannelCLI,
	FormAgentsMD: ChannelCLI,
	FormCursor:   ChannelCLI,
	FormPrompt:   ChannelHTTP,
	FormMCP:      ChannelMCP,
}

// Forms lists the forms Render accepts, in help order.
func Forms() []string {
	return []string{FormSkillMD, FormAgentsMD, FormCursor, FormPrompt, FormMCP}
}

// Channel returns the channel a form teaches, or "" for an unknown form.
func Channel(form string) string { return formChannel[form] }

// CarriesServer reports whether a form names the server's URL. The forms that
// run the binary never do: the binary's configuration carries it.
func CarriesServer(form string) bool { return form == FormPrompt || form == FormMCP }

// serverPlaceholder stands for the URL in a form rendered without a server.
const serverPlaceholder = "<server URL>"

// Meta is source/skill.json.
type Meta struct {
	Name          string `json:"name"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	Version       string `json:"version"`
	License       string `json:"license"`
	Compatibility string `json:"compatibility"`
	// Reminder is the one line skill reminder prints at a session start.
	Reminder  string   `json:"reminder"`
	Fragments []string `json:"fragments"`
}

// Reminder is the one-line session-start reminder from source/skill.json.
func Reminder() (string, error) {
	m, _, err := Source()

	return m.Reminder, err
}

// Fragment is one teaching point of the source.
type Fragment struct {
	ID string
	// Shared is the text outside every channel block, with {{server}} left in.
	Shared string
	lines  []line
}

// line is one source line and the channels it belongs to (nil: every one).
type line struct {
	text     string
	channels []string
}

// ErrUnknownForm is returned by Render for a form not in Forms.
var ErrUnknownForm = errors.New("unknown form")

// ServerError explains why a server URL was refused.
type ServerError struct{ Raw, Reason string }

func (e *ServerError) Error() string { return fmt.Sprintf("server URL %q: %s", e.Raw, e.Reason) }

var (
	loadOnce  sync.Once
	meta      Meta
	fragments []Fragment
	loadErr   error
)

// Source returns the parsed source. The source is embedded, so an error means
// the binary was built from a broken source; the package's tests catch it.
func Source() (Meta, []Fragment, error) {
	loadOnce.Do(func() { meta, fragments, loadErr = load(sourceFS) })

	return meta, fragments, loadErr
}

func load(fsys fs.FS) (Meta, []Fragment, error) {
	var m Meta
	data, err := fs.ReadFile(fsys, "source/skill.json")
	if err != nil {
		return m, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, nil, fmt.Errorf("source/skill.json: %w", err)
	}
	if m.Name == "" || m.Title == "" || m.Description == "" || m.Version == "" || len(m.Fragments) == 0 {
		return m, nil, errors.New("source/skill.json: name, title, description, version and fragments are required")
	}
	if strings.ContainsAny(m.Reminder, "\r\n") {
		return m, nil, errors.New("source/skill.json: reminder must be one line")
	}
	frags := make([]Fragment, 0, len(m.Fragments))
	for i, id := range m.Fragments {
		if slices.Contains(m.Fragments[:i], id) {
			return m, nil, fmt.Errorf("source/skill.json: fragment %q listed twice", id)
		}
		data, err := fs.ReadFile(fsys, "source/"+id+".md")
		if err != nil {
			return m, nil, err
		}
		f, err := parseFragment(id, string(data))
		if err != nil {
			return m, nil, err
		}
		frags = append(frags, f)
	}
	entries, err := fs.ReadDir(fsys, "source")
	if err != nil {
		return m, nil, err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".md")
		if ok && !slices.Contains(m.Fragments, id) {
			return m, nil, fmt.Errorf("source/%s is not listed in skill.json", e.Name())
		}
	}

	return m, frags, nil
}

var (
	blockStart = regexp.MustCompile(`^<!-- only: ([a-z ]+) -->$`)
	blockEnd   = "<!-- end -->"
)

func parseFragment(id, text string) (Fragment, error) {
	f := Fragment{ID: id}
	var open []string
	var shared []string
	for i, raw := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		where := fmt.Sprintf("source/%s.md:%d", id, i+1)
		if strings.Contains(raw, "\r") {
			return f, fmt.Errorf("%s: carriage return", where)
		}
		switch m := blockStart.FindStringSubmatch(raw); {
		case m != nil:
			if open != nil {
				return f, fmt.Errorf("%s: block opened inside a block", where)
			}
			open = strings.Fields(m[1])
			if len(open) == 0 {
				return f, fmt.Errorf("%s: block names no channel", where)
			}
			for _, c := range open {
				if c != ChannelCLI && c != ChannelHTTP && c != ChannelMCP {
					return f, fmt.Errorf("%s: unknown channel %q", where, c)
				}
			}

			continue
		case raw == blockEnd:
			if open == nil {
				return f, fmt.Errorf("%s: end without a block", where)
			}
			open = nil

			continue
		case strings.HasPrefix(strings.TrimSpace(raw), "<!--"):
			return f, fmt.Errorf("%s: unrecognised directive %q (directives start in column 1)", where, raw)
		}
		f.lines = append(f.lines, line{text: raw, channels: open})
		if open == nil {
			shared = append(shared, raw)
		}
	}
	if open != nil {
		return f, fmt.Errorf("source/%s.md: block not closed", id)
	}
	if len(f.lines) == 0 || f.lines[0].channels != nil || !strings.HasPrefix(f.lines[0].text, "## ") {
		return f, fmt.Errorf("source/%s.md: must start with a level-2 heading outside any block", id)
	}
	f.Shared = tidy(strings.Join(shared, "\n"))

	return f, nil
}

// text returns the fragment as a channel sees it.
func (f Fragment) text(channel string) string {
	var out []string
	for _, l := range f.lines {
		if l.channels == nil || slices.Contains(l.channels, channel) {
			out = append(out, l.text)
		}
	}

	return tidy(strings.Join(out, "\n"))
}

// tidy drops trailing spaces, collapses the blank lines a removed block leaves
// behind and trims blank lines at both ends. Fenced code is left as written.
func tidy(s string) string {
	var out []string
	var fence fenceState
	for _, l := range strings.Split(s, "\n") {
		if fence.inside() || fence.toggles(l) {
			fence.step(l)
			out = append(out, l)

			continue
		}
		l = strings.TrimRight(l, " \t")
		if l == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, l)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}

	return strings.Join(out, "\n")
}

// fenceState follows CommonMark code fences: up to three spaces, then three or
// more backticks or tildes; the closing fence uses the same character and is at
// least as long.
type fenceState struct {
	char byte
	n    int
}

func (f *fenceState) inside() bool { return f.n > 0 }

// fenceRun returns the fence character and run length l opens or closes with.
func fenceRun(l string) (byte, int) {
	t := strings.TrimLeft(l, " ")
	if len(l)-len(t) > 3 || len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return 0, 0
	}
	n := len(t) - len(strings.TrimLeft(t, t[:1]))
	if n < 3 {
		return 0, 0
	}

	return t[0], n
}

// toggles reports whether l opens a fence (when outside one).
func (f *fenceState) toggles(l string) bool { _, n := fenceRun(l); return n > 0 }

// step advances the state over one line.
func (f *fenceState) step(l string) {
	c, n := fenceRun(l)
	switch {
	case f.n == 0 && n > 0:
		f.char, f.n = c, n
	case f.n > 0 && c == f.char && n >= f.n && strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), string(c))) == "":
		f.char, f.n = 0, 0
	}
}

// urlSafe is what a normalised server URL may contain: it is pasted into
// Markdown and into a double-quoted shell argument, so nothing a shell or
// Markdown interprets.
var urlSafe = regexp.MustCompile(`^https?://[A-Za-z0-9.\-\[\]:]+(/[A-Za-z0-9._~%\-/]*)?$`)

// NormalizeServer checks a base URL and returns it without a trailing slash.
func NormalizeServer(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", &ServerError{raw, "it does not parse"}
	case u.Scheme != "http" && u.Scheme != "https":
		return "", &ServerError{raw, "the scheme is not http or https"}
	case u.Host == "" || u.Hostname() == "":
		return "", &ServerError{raw, "it has no host"}
	case u.User != nil:
		return "", &ServerError{raw, "it carries credentials"}
	case u.RawQuery != "" || u.ForceQuery:
		return "", &ServerError{raw, "it has a query"}
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return "", &ServerError{raw, "it has a fragment"}
	}
	out := u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/")
	if !urlSafe.MatchString(out) {
		return "", &ServerError{raw, "it contains characters that are not safe in a shell command"}
	}

	return out, nil
}

// Render returns a form. server is the base URL the prompt and mcp forms name;
// empty renders them with a placeholder, and the other forms ignore it.
func Render(form, server string) ([]byte, error) {
	channel := Channel(form)
	if channel == "" {
		return nil, fmt.Errorf("%w %q", ErrUnknownForm, form)
	}
	m, frags, err := Source()
	if err != nil {
		return nil, err
	}
	shown := serverPlaceholder
	if !CarriesServer(form) {
		server = ""
	}
	if server != "" {
		if shown, err = NormalizeServer(server); err != nil {
			return nil, err
		}
	}

	parts := make([]string, 0, len(frags))
	for _, f := range frags {
		parts = append(parts, strings.ReplaceAll(f.text(channel), "{{server}}", shown))
	}
	body := strings.Join(parts, "\n\n")

	var b strings.Builder
	switch form {
	case FormSkillMD:
		fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\n", m.Name, yamlString(m.Description))
		if m.License != "" {
			fmt.Fprintf(&b, "license: %s\n", yamlString(m.License))
		}
		if m.Compatibility != "" {
			fmt.Fprintf(&b, "compatibility: %s\n", yamlString(m.Compatibility))
		}
		fmt.Fprintf(&b, "metadata:\n  author: AgentFeedback\n  version: %s\n---\n\n", yamlQuoted(m.Version))
		fmt.Fprintf(&b, "<!-- Generated by `agentfeedback skill render skill-md`; do not edit. -->\n\n")
		fmt.Fprintf(&b, "# %s\n\n%s\n", m.Title, body)
	case FormAgentsMD:
		fmt.Fprintf(&b, "<!-- %s:begin %s -->\n## %s\n\n%s\n\n%s\n<!-- %s:end -->\n",
			m.Name, m.Version, m.Title, m.Description, demote(body), m.Name)
	case FormCursor:
		fmt.Fprintf(&b, "---\ndescription: %s\nalwaysApply: false\n---\n\n# %s\n\n%s\n",
			yamlString(m.Description), m.Title, body)
	case FormPrompt:
		fmt.Fprintf(&b, "# %s\n\nYou can report friction to the AgentFeedback server at %s over plain HTTP. "+
			"The API key comes from the user or your configuration; never print it or put it in a report.\n\n%s\n",
			m.Title, shown, body)
	case FormMCP:
		fmt.Fprintf(&b, "# %s\n\nThis server collects friction reports from AI agents through its tools.", m.Title)
		if server != "" {
			fmt.Fprintf(&b, " Its HTTP API is at %s/api/v1.", shown)
		}
		fmt.Fprintf(&b, "\n\n%s\n", body)
	}

	return []byte(b.String()), nil
}

var atxHeading = regexp.MustCompile(`^#{1,5}( |$)`)

// demote moves every Markdown heading outside a code fence one level down, so
// the snippet nests under the ## heading it is pasted as.
func demote(s string) string {
	ls := strings.Split(s, "\n")
	var fence fenceState
	for i, l := range ls {
		if fence.inside() || fence.toggles(l) {
			fence.step(l)

			continue
		}
		if atxHeading.MatchString(l) {
			ls[i] = "#" + l
		}
	}

	return strings.Join(ls, "\n")
}

var yamlPlain = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 .,;()/+_-]*[A-Za-z0-9.)]$`)

// yamlString writes s as a plain YAML scalar when that is unambiguous, and as
// a double-quoted one otherwise.
func yamlString(s string) string {
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "y", "n":
		return yamlQuoted(s)
	}
	if yamlPlain.MatchString(s) {
		return s
	}

	return yamlQuoted(s)
}

// yamlQuoted writes s as a double-quoted YAML scalar, in JSON string syntax,
// which YAML accepts.
func yamlQuoted(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)

	return strings.TrimSuffix(b.String(), "\n")
}
