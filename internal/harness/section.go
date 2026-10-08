package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

// Markers of the section install writes into an instruction file. The begin
// marker carries attributes (v=, hash=); any line starting with sectionBegin
// followed by a space or by --> and ending with --> begins a section.
const (
	sectionBegin = "<!-- agentfeedback:begin"
	sectionEnd   = "<!-- agentfeedback:end -->"
)

// Section states install --check reports.
const (
	RuleCurrent = "current"
	RuleStale   = "stale"
	RuleMissing = "missing"
)

// Section is the marked section holding body: the begin marker with the
// skill version and the first 16 hex digits of the SHA-256 of body, body,
// then the end marker, each on its own line.
func Section(version, body string) string {
	s := sha256.Sum256([]byte(body))

	return sectionBegin + " v=" + version + " hash=" + hex.EncodeToString(s[:])[:16] + " -->\n" + body + "\n" + sectionEnd + "\n"
}

// RuleSection is the section install writes into a harness's global
// instruction file: the rule of source/skill.json under ## AgentFeedback.
func RuleSection() (string, error) {
	m, _, err := skillgen.Source()
	if err != nil {
		return "", err
	}

	return Section(m.Version, "## "+m.Title+"\n\n"+m.Rule), nil
}

// ProjectSection is the section install --project writes into a
// repository's instruction file: the pointer of source/skill.json.
func ProjectSection() (string, error) {
	m, _, err := skillgen.Source()
	if err != nil {
		return "", err
	}

	return Section(m.Version, "## "+m.Title+"\n\n"+m.ProjectRule), nil
}

// SectionError is a file whose sections cannot be told apart: more than one
// begin marker, or a begin marker without its end marker.
type SectionError struct{ Path, Problem string }

func (e *SectionError) Error() string { return e.Path + " " + e.Problem }

// findSection locates the one marked section of doc: start is the offset of
// its begin line, end the offset just past its end line. found is false
// without a section.
func findSection(path string, doc []byte) (start, end int, found bool, err error) {
	unterminated := &SectionError{Path: path, Problem: "has an agentfeedback begin marker without its end marker"}
	open := -1
	for off := 0; off < len(doc); {
		lineEnd := len(doc)
		if i := bytes.IndexByte(doc[off:], '\n'); i >= 0 {
			lineEnd = off + i + 1
		}
		l := strings.TrimRight(string(doc[off:lineEnd]), "\r\n")
		t := strings.TrimRight(l, " \t")
		switch {
		case isBeginMarker(l) && strings.HasSuffix(t, "-->"):
			if open >= 0 {
				return 0, 0, false, unterminated
			}
			if found {
				return 0, 0, false, &SectionError{Path: path, Problem: "holds more than one agentfeedback section"}
			}
			open = off
		case t == sectionEnd && open >= 0:
			start, end, found, open = open, lineEnd, true, -1
		}
		off = lineEnd
	}
	if open >= 0 {
		return 0, 0, false, unterminated
	}

	return start, end, found, nil
}

// isBeginMarker is whether line starts with sectionBegin followed by a
// space or by -->, so a longer word such as agentfeedback:beginner is not
// a marker.
func isBeginMarker(line string) bool {
	rest, ok := strings.CutPrefix(line, sectionBegin)

	return ok && (strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, "-->"))
}

// SetSection returns doc with its section replaced in place by text, or
// with text appended after one blank line when doc has none. inserted is
// exactly what was appended (the separator included), or text when it
// replaced a section.
func SetSection(path string, doc []byte, text string) (out []byte, inserted string, err error) {
	start, end, found, err := findSection(path, doc)
	if err != nil {
		return nil, "", err
	}
	if found {
		out = append(append(bytes.Clone(doc[:start]), text...), doc[end:]...)

		return out, text, nil
	}
	sep := ""
	switch {
	case len(doc) == 0:
	case doc[len(doc)-1] != '\n':
		sep = "\n\n"
	default:
		sep = "\n"
	}
	inserted = sep + text

	return append(bytes.Clone(doc), inserted...), inserted, nil
}

// RemoveSection returns doc without its section, edited or not. inserted is
// what install recorded: when doc still holds it verbatim it is removed
// whole; otherwise the section goes by its markers, with the blank line
// install put before it when that is still there. found is false when doc
// has no section.
func RemoveSection(path string, doc []byte, inserted string) (out []byte, found bool, err error) {
	start, end, found, err := findSection(path, doc)
	if err != nil {
		return doc, false, err
	}
	if inserted != "" && strings.TrimLeft(inserted, "\n") != inserted {
		if i := bytes.LastIndex(doc, []byte(inserted)); i >= 0 {
			return append(bytes.Clone(doc[:i]), doc[i+len(inserted):]...), true, nil
		}
	}
	if !found {
		return doc, false, nil
	}
	pre := doc[:start]
	switch sep := inserted[:len(inserted)-len(strings.TrimLeft(inserted, "\n"))]; sep {
	case "\n":
		if bytes.HasSuffix(pre, []byte("\n\n")) {
			pre = pre[:len(pre)-1]
		}
	case "\n\n":
		if bytes.HasSuffix(pre, []byte("\n\n")) && !bytes.HasSuffix(pre, []byte("\n\n\n")) {
			pre = pre[:len(pre)-2]
		}
	}

	return append(bytes.Clone(pre), doc[end:]...), true, nil
}

// SectionState is how doc's section compares with text: RuleMissing without
// one, RuleCurrent when it is text byte for byte, else RuleStale (also for
// a file whose sections cannot be told apart).
func SectionState(path string, doc []byte, text string) string {
	start, end, found, err := findSection(path, doc)
	switch {
	case err != nil:
		return RuleStale
	case !found:
		return RuleMissing
	case string(doc[start:end]) == text:
		return RuleCurrent
	}

	return RuleStale
}

// RuleCheck is one harness's global instruction section as install --check
// reports it: Rule is RuleCurrent, RuleStale, RuleMissing, or "-" for a
// harness install writes no rule for.
type RuleCheck struct {
	Name string `json:"name"`
	File string `json:"file"`
	Rule string `json:"rule"`
}

// ruleFile is the global instruction file install writes the rule into
// for the adapter, or "" when it writes none: no global file, or no CLI
// mode.
func (a *Adapter) ruleFile(e Env) string {
	if a.Instructions.Global == nil || a.noCLI != nil {
		return ""
	}
	g := a.Instructions.Global(e)
	if len(g) == 0 {
		return ""
	}

	return g[0]
}

// Check reads the global instruction file of each named harness, at the
// location the manifest records when it records one, and compares its
// section with the one this binary writes; a harness the manifest records
// in MCP mode has none. It writes nothing.
func (e Env) Check(harnesses []string) ([]RuleCheck, error) {
	man, _, err := LoadManifest(e.ManifestPath())
	if err != nil {
		return nil, err
	}
	located, _ := e.withRecorded(man)
	want, err := ruleSection()
	if err != nil {
		return nil, err
	}
	out := make([]RuleCheck, 0, len(harnesses))
	for _, name := range harnesses {
		a := adapterOf(name)
		if a == nil {
			continue
		}
		c := RuleCheck{Name: name, Rule: "-"}
		if rec := man.Harnesses[name]; rec != nil && rec.Mode == ModeMCP {
			// Install writes no rule in MCP mode.
			out = append(out, c)

			continue
		}
		if c.File = a.ruleFile(located); c.File != "" {
			data, err := os.ReadFile(c.File)
			switch {
			case err == nil:
				c.Rule = SectionState(c.File, data, want)
			case os.IsNotExist(err):
				c.Rule = RuleMissing
			default:
				return nil, &FileError{Path: c.File, Err: err}
			}
		}
		out = append(out, c)
	}

	return out, nil
}
