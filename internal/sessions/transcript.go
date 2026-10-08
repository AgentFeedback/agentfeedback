package sessions

import (
	"encoding/json"
	"path/filepath"
	"slices"
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
	entries   []entry
}

// summarise fills total and lastModel and drops the entries and the
// unparsed offsets: List and Status keep only the summary of a session.
func (t *transcript) summarise(keep bool) {
	t.total, t.lastModel = t.counts(0), t.model()
	if !keep {
		t.entries, t.badLines = nil, nil
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
