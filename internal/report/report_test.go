package report

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/schemas"
)

// schemaDoc is the embedded report schema, decoded.
func schemaDoc(t *testing.T) map[string]any {
	t.Helper()
	raw, err := schemas.FS.ReadFile(SchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	return doc
}

// examples returns the schema's examples, each as JSON.
func examples(t *testing.T) [][]byte {
	t.Helper()
	var out [][]byte
	for _, e := range schemaDoc(t)["examples"].([]any) {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		t.Fatal("the schema has no examples")
	}

	return out
}

// mutated returns the first example after f changed its decoded tree.
func mutated(t *testing.T, f func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(examples(t)[0], &doc); err != nil {
		t.Fatal(err)
	}
	f(doc)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func labelsOf(doc map[string]any, session int) map[string]any {
	return doc["sessions"].([]any)[session].(map[string]any)["labels"].(map[string]any)
}

func TestCheck_ExamplesValid(t *testing.T) {
	for i, ex := range examples(t) {
		if _, probs, err := Check(ex); err != nil || len(probs) > 0 {
			t.Fatalf("example %d: err %v, problems %v", i, err, probs)
		}
	}
}

func TestCheck_Rejects(t *testing.T) {
	tests := []struct {
		name   string
		raw    []byte
		want   Problem
		mutate func(doc map[string]any)
	}{
		{name: "unknown label", want: Problem{"/sessions/0/labels/efficiency/label", ""}, mutate: func(doc map[string]any) {
			labelsOf(doc, 0)["efficiency"].(map[string]any)["label"] = "speedy"
		}},
		{name: "a label of another rubric", want: Problem{"/sessions/1/labels/verbosity/label", ""}, mutate: func(doc map[string]any) {
			labelsOf(doc, 1)["verbosity"].(map[string]any)["label"] = "sound"
		}},
		{name: "missing reason", want: Problem{"/sessions/0/labels/code_quality/reason", "required member is missing"}, mutate: func(doc map[string]any) {
			delete(labelsOf(doc, 0)["code_quality"].(map[string]any), "reason")
		}},
		{name: "missing reason on insufficient_evidence", want: Problem{"/sessions/2/labels/code_quality/reason", "required member is missing"}, mutate: func(doc map[string]any) {
			delete(labelsOf(doc, 2)["code_quality"].(map[string]any), "reason")
		}},
		{name: "blank reason", want: Problem{"/sessions/0/labels/verbosity/reason", ""}, mutate: func(doc map[string]any) {
			labelsOf(doc, 0)["verbosity"].(map[string]any)["reason"] = "  "
		}},
		{name: "missing rubric", want: Problem{"/sessions/0/labels/verbosity", "required member is missing"}, mutate: func(doc map[string]any) {
			delete(labelsOf(doc, 0), "verbosity")
		}},
		{name: "fourth top finding", want: Problem{"/top_findings", ""}, mutate: func(doc map[string]any) {
			doc["top_findings"] = append(doc["top_findings"].([]any), map[string]any{"text": "A fourth.", "refs": []any{}})
		}},
		{name: "unknown rules_source", want: Problem{"/sessions/0/rules_source", ""}, mutate: func(doc map[string]any) {
			doc["sessions"].([]any)[0].(map[string]any)["rules_source"] = "memory"
		}},
		{name: "unknown member", want: Problem{"/scope/grade", "member is not allowed"}, mutate: func(doc map[string]any) {
			doc["scope"].(map[string]any)["grade"] = "A"
		}},
		{name: "not a date-time", want: Problem{"/generated_at", ""}, mutate: func(doc map[string]any) {
			doc["generated_at"] = "yesterday"
		}},
		{name: "not JSON", raw: []byte("{"), want: Problem{"", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := tt.raw
			if tt.mutate != nil {
				raw = mutated(t, tt.mutate)
			}
			_, probs, err := Check(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(probs, func(p Problem) bool {
				return p.Pointer == tt.want.Pointer && (tt.want.Message == "" || p.Message == tt.want.Message)
			}) {
				t.Fatalf("want a problem at %q, got %+v", tt.want.Pointer, probs)
			}
		})
	}
}

func TestCheck_ProblemsSorted(t *testing.T) {
	raw := mutated(t, func(doc map[string]any) {
		delete(doc, "version")
		delete(labelsOf(doc, 1)["code_quality"].(map[string]any), "reason")
		labelsOf(doc, 0)["efficiency"].(map[string]any)["label"] = "speedy"
	})
	_, probs, err := Check(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(probs) != 3 || !slices.IsSortedFunc(probs, func(a, b Problem) int {
		if a.Pointer < b.Pointer {
			return -1
		}
		if a.Pointer > b.Pointer {
			return 1
		}

		return 0
	}) {
		t.Fatalf("want three problems sorted by pointer, got %+v", probs)
	}
}

// The schema's label enums and the grade's label tables are one list.
func TestSchemaLabelsMatchRubrics(t *testing.T) {
	defs := schemaDoc(t)["$defs"].(map[string]any)
	for _, rb := range Rubrics {
		def, ok := defs[rb.Name].(map[string]any)
		if !ok {
			t.Fatalf("$defs/%s missing", rb.Name)
		}
		var got []string
		for _, v := range def["properties"].(map[string]any)["label"].(map[string]any)["enum"].([]any) {
			got = append(got, v.(string))
		}
		var want []string
		for _, ls := range rb.Labels {
			want = append(want, ls.Label)
		}
		want = append(want, InsufficientEvidence)
		if !slices.Equal(got, want) {
			t.Fatalf("%s: schema labels %v, grade labels %v", rb.Name, got, want)
		}
	}
}

// JSON Schema takes any integral number as an integer; such a spelling
// decodes, and a fraction is refused by the schema.
func TestCheck_IntegralSpellings(t *testing.T) {
	ex := string(examples(t)[0])
	for _, sub := range [][2]string{{`"sessions_sampled":3`, `"sessions_sampled":3.0`}, {`"sessions_scanned":23`, `"sessions_scanned":2.3e1`}, {`"sessions":1}`, `"sessions":1e0}`}} {
		if !strings.Contains(ex, sub[0]) {
			t.Fatalf("the example has no %s", sub[0])
		}
		ex = strings.Replace(ex, sub[0], sub[1], 1)
	}
	r, probs, err := Check([]byte(ex))
	if err != nil || len(probs) > 0 {
		t.Fatalf("err %v, problems %+v", err, probs)
	}
	if r.Scope.SessionsSampled != 3 || r.Scope.SessionsScanned != 23 || r.Skills.Used[0].Sessions != 1 {
		t.Fatalf("decoded %+v %+v", r.Scope, r.Skills.Used)
	}
	_, probs, err = Check([]byte(strings.Replace(ex, `2.3e1`, `22.5`, 1)))
	if err != nil || len(probs) != 1 || probs[0].Pointer != "/scope/sessions_scanned" {
		t.Fatalf("a fraction: err %v, problems %+v", err, probs)
	}
}

func TestCheck_Consistency(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(doc map[string]any)
	}{
		{"sampled differs from the sessions listed", "/scope/sessions_sampled", func(doc map[string]any) {
			doc["scope"].(map[string]any)["sessions_sampled"] = 4
		}},
		{"used count differs", "/skills/used/0/sessions", func(doc map[string]any) {
			doc["skills"].(map[string]any)["used"].([]any)[0].(map[string]any)["sessions"] = 2
		}},
		{"used skill no session names", "/skills/used/1/name", func(doc map[string]any) {
			used := doc["skills"].(map[string]any)["used"].([]any)
			doc["skills"].(map[string]any)["used"] = append(used, map[string]any{"name": "db-migrate", "sessions": 0})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, probs, err := Check(mutated(t, tt.mutate))
			if err != nil || len(probs) != 1 || probs[0].Pointer != tt.want {
				t.Fatalf("want one problem at %s, got %v %+v", tt.want, err, probs)
			}
		})
	}
}
