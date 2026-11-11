// Package report reads the checkup report an agent writes after judging a
// sample of sessions: it validates the report against
// schemas/checkup-report.v1.json, computes the grade from the labels (the
// agent never does the arithmetic) and renders one self-contained HTML page.
// Nothing here opens a network connection or stores the report.
package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/agentfeedback/agentfeedback/v4/schemas"
)

// SchemaFile is the report schema's name under schemas/.
const SchemaFile = "checkup-report.v1.json"

// Report is a checkup report as schemas/checkup-report.v1.json defines it.
type Report struct {
	Version     int          `json:"version"`
	GeneratedAt string       `json:"generated_at"`
	Scope       Scope        `json:"scope"`
	Skills      Skills       `json:"skills"`
	Sessions    []Session    `json:"sessions"`
	TopFindings []Finding    `json:"top_findings"`
	Suggestions []Suggestion `json:"suggestions"`
}

// Scope is the window the sessions were drawn from.
type Scope struct {
	Projects        []string `json:"projects"`
	Harnesses       []string `json:"harnesses"`
	Since           string   `json:"since"`
	SessionsScanned Count    `json:"sessions_scanned"`
	SessionsSampled Count    `json:"sessions_sampled"`
}

// Skills is the skill inventory and its use.
type Skills struct {
	Found []FoundSkill `json:"found"`
	Used  []UsedSkill  `json:"used"`
}

// FoundSkill is one installed skill.
type FoundSkill struct {
	Name    string `json:"name"`
	Harness string `json:"harness"`
	Scope   string `json:"scope"`
}

// UsedSkill is one skill and the number of sampled sessions that used it.
type UsedSkill struct {
	Name     string `json:"name"`
	Sessions Count  `json:"sessions"`
}

// Count is a non-negative integer of the report. JSON Schema takes any
// integral number as an integer, so 3.0 and 1e1 are valid spellings and
// decode here; the schema has already refused a fraction.
type Count int

// UnmarshalJSON decodes any integral JSON number.
func (c *Count) UnmarshalJSON(b []byte) error {
	f, _, err := big.ParseFloat(string(b), 10, 256, big.ToNearestEven)
	if err != nil {
		return fmt.Errorf("count %s: %w", b, err)
	}
	if !f.IsInt() {
		return fmt.Errorf("count %s is not an integer", b)
	}
	i, acc := f.Int64()
	if acc != big.Exact || i < 0 || i > math.MaxInt32 {
		return fmt.Errorf("count %s is out of range", b)
	}
	*c = Count(i)

	return nil
}

// Session is one judged session.
type Session struct {
	Ref         string   `json:"ref"`
	RulesSource string   `json:"rules_source"`
	SkillsUsed  []string `json:"skills_used"`
	Labels      Labels   `json:"labels"`
}

// Labels holds one label per rubric.
type Labels struct {
	Efficiency          Label `json:"efficiency"`
	CodeQuality         Label `json:"code_quality"`
	ProcedureCompliance Label `json:"procedure_compliance"`
	Verbosity           Label `json:"verbosity"`
}

// Label is a rubric's label for one session and why.
type Label struct {
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

// Finding is one of the top findings.
type Finding struct {
	Text string   `json:"text"`
	Refs []string `json:"refs"`
}

// Suggestion is one proposed change.
type Suggestion struct {
	Surface  string   `json:"surface"`
	Change   string   `json:"change"`
	Evidence []string `json:"evidence"`
	FiledUID string   `json:"filed_uid,omitempty"`
	Diff     string   `json:"diff,omitempty"`
}

// Problem is one reason a report is invalid: the JSON pointer of the value
// (the empty string is the whole document) and what is wrong with it.
type Problem struct {
	Pointer string `json:"pointer"`
	Message string `json:"message"`
}

var compiled = sync.OnceValues(func() (*jsonschema.Schema, error) {
	raw, err := schemas.FS.ReadFile(SchemaFile)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", SchemaFile, err)
	}
	id, _ := doc.(map[string]any)["$id"].(string)
	c := jsonschema.NewCompiler()
	// generated_at is a date-time; 2020-12 treats format as an annotation
	// unless asked to assert it.
	c.AssertFormat()
	if err := c.AddResource(id, doc); err != nil {
		return nil, err
	}

	return c.Compile(id)
})

// Check validates raw against the report schema. A report that is not JSON
// or does not match the schema comes back with its problems, sorted by
// pointer; the error is for a schema that cannot be compiled.
func Check(raw []byte) (Report, []Problem, error) {
	s, err := compiled()
	if err != nil {
		return Report{}, nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return Report{}, []Problem{{Pointer: "", Message: "not JSON: " + err.Error()}}, nil
	}
	if err := s.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return Report{}, nil, err
		}

		return Report{}, problems(ve), nil
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return Report{}, []Problem{{Pointer: "", Message: err.Error()}}, nil
	}
	if probs := consistency(r); len(probs) > 0 {
		return Report{}, probs, nil
	}

	return r, nil, nil
}

// consistency checks the counts the agent wrote against the sessions it
// listed: the schema cannot relate members, and the page shows these counts.
func consistency(r Report) []Problem {
	var out []Problem
	if int(r.Scope.SessionsSampled) != len(r.Sessions) {
		out = append(out, Problem{"/scope/sessions_sampled",
			fmt.Sprintf("is %d, but the report lists %d session(s)", r.Scope.SessionsSampled, len(r.Sessions))})
	}
	uses := skillUses(r.Sessions)
	for i, u := range r.Skills.Used {
		n := uses[u.Name]
		switch {
		case n == 0:
			out = append(out, Problem{fmt.Sprintf("/skills/used/%d/name", i),
				fmt.Sprintf("no session's skills_used names %q", u.Name)})
		case int(u.Sessions) != n:
			out = append(out, Problem{fmt.Sprintf("/skills/used/%d/sessions", i),
				fmt.Sprintf("is %d, but %d session(s) name %q in skills_used", u.Sessions, n, u.Name)})
		}
	}
	sortProblems(out)

	return out
}

// skillUses counts, per skill name, the sessions whose skills_used names it.
func skillUses(sessions []Session) map[string]int {
	uses := map[string]int{}
	for _, s := range sessions {
		seen := map[string]bool{}
		for _, n := range s.SkillsUsed {
			if !seen[n] {
				seen[n] = true
				uses[n]++
			}
		}
	}

	return uses
}

// problems flattens the validation tree to its leaves. A missing or an
// unexpected property is reported at the property's own pointer, so each
// problem names the member to add or remove.
func problems(ve *jsonschema.ValidationError) []Problem {
	var out []Problem
	var walk func(u jsonschema.OutputUnit)
	walk = func(u jsonschema.OutputUnit) {
		for _, c := range u.Errors {
			walk(c)
		}
		if len(u.Errors) > 0 || u.Error == nil {
			return
		}
		switch k := u.Error.Kind.(type) {
		case *kind.Required:
			for _, m := range k.Missing {
				out = append(out, Problem{Pointer: u.InstanceLocation + "/" + escape(m), Message: "required member is missing"})
			}
		case *kind.AdditionalProperties:
			for _, p := range k.Properties {
				out = append(out, Problem{Pointer: u.InstanceLocation + "/" + escape(p), Message: "member is not allowed"})
			}
		default:
			out = append(out, Problem{Pointer: u.InstanceLocation, Message: u.Error.String()})
		}
	}
	walk(*ve.DetailedOutput())
	sortProblems(out)

	return out
}

func sortProblems(p []Problem) {
	sort.Slice(p, func(i, j int) bool {
		if p[i].Pointer != p[j].Pointer {
			return p[i].Pointer < p[j].Pointer
		}

		return p[i].Message < p[j].Message
	})
}

// escape is RFC 6901 reference-token escaping.
func escape(token string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(token)
}
