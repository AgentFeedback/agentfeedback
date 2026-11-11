package sessions

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Entry kinds a reader assigns.
const (
	kindOther         = "other"
	kindPrompt        = "prompt"
	kindInterrupt     = "interrupt"
	kindToolCall      = "tool_call"
	kindAssistantText = "assistant_text"
)

// Tool result statuses.
const (
	statusOK          = "ok"
	statusError       = "error"
	statusInterrupted = "interrupted"
	statusDenied      = "denied"
)

// transcript is a read session in the readers' common form.
type transcript struct {
	size      int64   // bytes of complete lines
	head      string  // SHA-256 hex of the first complete line, "" when none
	parsed    int     // lines that parsed as an entry
	badLines  []int64 // offsets of the lines that did not parse
	sessionID string
	cwds      []string // distinct, in order of first appearance
	lastCwd   string   // the cwd of the last entry that records one
	start     time.Time
	end       time.Time
	total     Counts // counts(0), kept when entries are dropped
	lastModel string // model(), kept when entries are dropped
	// captureModel is Codex's last assistant activity model; a later
	// turn_context without an assistant must not change upload metadata.
	captureModel string
	entries      []entry
	// skillHits are the skill uses the reader saw, in log order, before
	// they are matched against the skill inventory.
	skillHits []skillHit
	// instructions and skillsAvailable are the standing rules and the
	// skill listing the session's log records as loaded, in first-seen
	// order; capture keeps none, like text.
	instructions    []recordedInstruction
	skillsAvailable []string
	captureOnly     bool // discard record content after classification
	storageOnly     bool // opaque complete lines, with no semantic entries
}

// summarise fills total and lastModel and, unless keep, drops the
// entries, the unparsed offsets and the recorded rules: List and Status keep only the summary of a session.
func (t *transcript) summarise(keep bool) {
	if t.storageOnly {
		t.total = Counts{Entries: t.parsed}
		return
	}
	t.total, t.lastModel = t.counts(0), t.model()
	if !keep {
		t.entries, t.badLines = nil, nil
		t.instructions, t.skillsAvailable = nil, nil
	}
}

type entry struct {
	offset int64
	span   string
	at     time.Time
	cwd    string
	model  string
	kind   string
	text   string
	calls  []*call
}

type call struct {
	id         string
	tool       string
	input      json.RawMessage
	command    string
	hasCommand bool
	result     bool
	// resultOffset is the offset of the line carrying the tool_result.
	resultOffset int64
	status       string
	content      string
	// exitCode is the exit code the store records natively, nil when it
	// records none.
	exitCode *int
}

// addEntry retains the same count and status metadata in capture mode without
// keeping transcript text or tool bodies reachable through entries or call maps.
func (t *transcript) addEntry(e entry) {
	if t.captureOnly {
		e.text = ""
		for _, c := range e.calls {
			c.input, c.command, c.content = nil, "", ""
		}
	}
	t.entries = append(t.entries, e)
}

// skillHit is one use of a skill as the log records it: by name, or by
// dir, the base name of the skill directory whose SKILL.md it names.
type skillHit struct {
	name   string
	dir    string
	span   string
	offset int64
}

// addSkillHit records a use of the skill name; capture keeps none, like
// text.
func (t *transcript) addSkillHit(name, span string, offset int64) {
	if name = strings.TrimSpace(name); name != "" && !t.captureOnly {
		t.skillHits = append(t.skillHits, skillHit{name: name, span: span, offset: offset})
	}
}

// addSkillDir records a use of the skill in the skill directory dir.
func (t *transcript) addSkillDir(dir, span string, offset int64) {
	if dir != "" && !t.captureOnly {
		t.skillHits = append(t.skillHits, skillHit{dir: dir, span: span, offset: offset})
	}
}

// recordedInstruction is one instruction file the log records as loaded:
// source is the harness's type for it ("" when it records none), path
// the file or directory it names ("" when it names none).
type recordedInstruction struct {
	source, path, text string
}

// addInstruction records an instruction file once: by path, or by text
// when the log names no path.
func (t *transcript) addInstruction(source, path, text string) {
	if t.captureOnly || strings.TrimSpace(text) == "" {
		return
	}
	for _, in := range t.instructions {
		if (path != "" && in.path == path) || (path == "" && in.path == "" && in.text == text) {
			return
		}
	}
	t.instructions = append(t.instructions, recordedInstruction{source: source, path: path, text: text})
}

// addSkillAvailable records a skill the log lists as available, once.
func (t *transcript) addSkillAvailable(name string) {
	if name = strings.TrimSpace(name); name != "" && !t.captureOnly && !slices.Contains(t.skillsAvailable, name) {
		t.skillsAvailable = append(t.skillsAvailable, name)
	}
}

// seen widens the transcript's start and end to at and returns at in UTC.
func (t *transcript) seen(at time.Time) time.Time {
	at = at.UTC()
	if t.start.IsZero() || at.Before(t.start) {
		t.start = at
	}
	if at.After(t.end) {
		t.end = at
	}

	return at
}

// addCwd records cwd as the last working directory and, the first time,
// among cwds, and returns it. An empty or relative cwd is ignored and ""
// returned: a relative one would resolve against the reading process.
func (t *transcript) addCwd(cwd string) string {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	t.lastCwd = cwd
	if !slices.Contains(t.cwds, cwd) {
		t.cwds = append(t.cwds, cwd)
	}

	return cwd
}

// counts tallies the entries at or after from.
func (t *transcript) counts(from int64) Counts {
	var c Counts
	for _, o := range t.badLines {
		if o >= from {
			c.Unparsed++
		}
	}
	for _, e := range t.entries {
		if e.offset < from {
			continue
		}
		c.Entries++
		switch e.kind {
		case kindPrompt:
			c.Prompts++
		case kindInterrupt:
			c.Interrupts++
		}
		for _, k := range e.calls {
			c.ToolCalls++
			switch k.status {
			case statusError:
				c.ToolErrors++
			case statusInterrupted:
				c.Interrupts++
			case statusDenied:
				c.Denials++
			}
		}
	}

	return c
}

// model is the model of the last assistant entry that names one.
func (t *transcript) model() string {
	for i := len(t.entries) - 1; i >= 0; i-- {
		if t.entries[i].model != "" {
			return t.entries[i].model
		}
	}

	return ""
}
