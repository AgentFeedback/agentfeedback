package schema

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/schemas"
)

func mustCompile(t *testing.T, src string, m mode) *Schema {
	t.Helper()
	s, err := compile([]byte(src), m)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return s
}

func TestListAndDocuments(t *testing.T) {
	t.Parallel()
	want := []Entry{{Kind: "envelope", Versions: []uint64{1}}, {Kind: "friction", Versions: []uint64{1}}, {Kind: "review", Versions: []uint64{1}}}
	if got := List(); !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %+v, want %+v", got, want)
	}
	for _, e := range List() {
		for _, v := range e.Versions {
			doc, ok := Document(e.Kind, v)
			if !ok {
				t.Fatalf("Document(%s, %d) missing", e.Kind, v)
			}
			path := "envelope.v1.json"
			if e.Kind != EnvelopeKind {
				path = "kinds/" + e.Kind + ".v1.json"
			}
			raw, err := schemas.FS.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(doc) != string(raw) {
				t.Fatalf("Document(%s, %d) differs from the embedded file", e.Kind, v)
			}
			disk, err := os.ReadFile("../../schemas/" + path)
			if err != nil {
				t.Fatal(err)
			}
			if string(disk) != string(raw) {
				t.Fatalf("embedded %s differs from the file on disk", path)
			}
		}
	}
	doc, _ := Document("friction", 1)
	doc[0] = 'x'
	again, _ := Document("friction", 1)
	if again[0] == 'x' {
		t.Fatal("Document returned shared bytes")
	}
	for _, miss := range []struct {
		kind string
		v    uint64
	}{{"envelope", 2}, {"friction", 2}, {"deploy-note", 1}, {"", 1}} {
		if _, ok := Document(miss.kind, miss.v); ok {
			t.Errorf("Document(%q, %d) should be missing", miss.kind, miss.v)
		}
		if _, ok := Kind(miss.kind, miss.v); ok {
			t.Errorf("Kind(%q, %d) should be missing", miss.kind, miss.v)
		}
	}
}

func TestEnvelopeRules(t *testing.T) {
	t.Parallel()
	env := Envelope()
	if got := env.Recommended(); !reflect.DeepEqual(got, []string{"kind", "summary", "machine", "model"}) {
		t.Fatalf("envelope recommended = %v", got)
	}
	if got := env.AdditionalProperties(); got == nil || got.OnViolation() != "move" {
		t.Fatalf("envelope additionalProperties = %+v", got)
	}
	tests := []struct {
		name      string
		maxBytes  int
		trim      bool
		normalize string
		violation string
	}{
		{"kind", 0, false, "token", "warn"},
		{"key", 0, true, "", "warn"},
		{"summary", 2000, true, "", "truncate"},
		{"machine", 200, true, "", "truncate"},
		{"model", 200, true, "", "truncate"},
		{"harness", 200, false, "token", "truncate"},
		{"project", 200, true, "", "truncate"},
	}
	for _, tc := range tests {
		p, ok := env.Property(tc.name)
		if !ok {
			t.Fatalf("envelope has no %s", tc.name)
		}
		if p.MaxBytes() != tc.maxBytes || p.Trim() != tc.trim || p.Normalize() != tc.normalize || p.OnViolation() != tc.violation {
			t.Errorf("%s: maxBytes=%d trim=%v normalize=%q onViolation=%q", tc.name, p.MaxBytes(), p.Trim(), p.Normalize(), p.OnViolation())
		}
	}
	ctx, _ := env.Property("context")
	if n, ok := ctx.MaxProperties(); !ok || n != 32 {
		t.Fatalf("context maxProperties = %d, %v", n, ok)
	}
	if ap := ctx.AdditionalProperties(); ap == nil || ap.MaxBytes() != 2000 || ap.OnViolation() != "truncate" {
		t.Fatalf("context additionalProperties = %+v", ap)
	}
	occ, _ := env.Property("occurred_at")
	if occ.Format() != "date-time" {
		t.Fatalf("occurred_at format = %q", occ.Format())
	}
	names := env.PropertyNames()
	if names[0] != "context" || len(names) != 11 {
		t.Fatalf("envelope property names = %v", names)
	}
	rec := env.Recommended()
	rec[0] = "x"
	if env.Recommended()[0] != "kind" {
		t.Fatal("Recommended returned the internal slice")
	}
}

func TestKindSchemaAccessors(t *testing.T) {
	t.Parallel()
	review, ok := Kind("review", 1)
	if !ok {
		t.Fatal("review schema missing")
	}
	reviewers, _ := review.Property("reviewers")
	if reviewers.UniqueBy() != "slot" {
		t.Fatalf("reviewers x-unique-by = %q", reviewers.UniqueBy())
	}
	if n, _ := reviewers.MinItems(); n != 1 {
		t.Fatalf("reviewers minItems = %d", n)
	}
	if n, _ := reviewers.MaxItems(); n != 100 {
		t.Fatalf("reviewers maxItems = %d", n)
	}
	if got := reviewers.Types(); !reflect.DeepEqual(got, []string{"array"}) {
		t.Fatalf("reviewers types = %v", got)
	}
	item := reviewers.Items()
	if item == nil || !reflect.DeepEqual(item.Recommended(), []string{"slot", "model", "status"}) {
		t.Fatalf("reviewers items = %+v", item)
	}
	friction, _ := Kind("friction", 1)
	category, _ := friction.Property("category")
	if n, ok := category.MinLength(); !ok || n != 1 {
		t.Fatalf("category minLength = %d, %v", n, ok)
	}
	if category.Normalize() != "token" || category.MaxBytes() != 200 || category.OnViolation() != "warn" {
		t.Fatalf("category rules: %q %d %q", category.Normalize(), category.MaxBytes(), category.OnViolation())
	}
}

func TestCompileRejectsWhatTheContractForbids(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		m    mode
		want string
	}{
		{"unsupported keyword", `{"type":"string","pattern":"x"}`, modeKind, `unsupported keyword "pattern"`},
		{"unsupported nested keyword", `{"properties":{"a":{"const":1}}}`, modeKind, `/properties/a: unsupported keyword "const"`},
		{"unknown x keyword", `{"x-foo":1}`, modeKind, `unsupported keyword "x-foo"`},
		{"unknown type", `{"type":"str"}`, modeKind, `unknown type "str"`},
		{"empty type list", `{"type":[]}`, modeKind, "at least one type"},
		{"format", `{"type":"string","format":"email"}`, modeKind, "unsupported format"},
		{"x-max-bytes zero", `{"type":"string","x-max-bytes":0}`, modeKind, "x-max-bytes must be a positive integer"},
		{"x-max-bytes float", `{"type":"string","x-max-bytes":1.5}`, modeKind, "x-max-bytes must be a positive integer"},
		{"x-trim false", `{"type":"string","x-trim":false}`, modeKind, "x-trim must be true"},
		{"x-normalize upper", `{"type":"string","x-normalize":"upper"}`, modeKind, `x-normalize must be "token"`},
		{"x-normalize with x-trim", `{"type":"string","x-normalize":"token","x-trim":true}`, modeKind, "declare one"},
		{"x-recommended undeclared", `{"type":"object","x-recommended":["a"]}`, modeKind, `x-recommended names "a"`},
		{"x-recommended not strings", `{"type":"object","x-recommended":[1]}`, modeKind, "array of member names"},
		{"x-unique-by key", `{"type":"array","x-unique-by":"key"}`, modeKind, "other than key"},
		{"x-unique-by non-array", `{"type":"object","x-unique-by":"slot"}`, modeKind, "non-array"},
		{"x-on-violation unknown", `{"x-on-violation":"drop"}`, modeKind, "warn, truncate or move"},
		{"move in a kind schema", `{"additionalProperties":{"x-on-violation":"move"}}`, modeKind, "only for the envelope"},
		{"move elsewhere in the envelope", `{"properties":{"a":{"x-on-violation":"move"}}}`, modeEnvelope, "only for the envelope"},
		{"enum empty", `{"enum":[]}`, modeKind, "non-empty array"},
		{"minimum string", `{"minimum":"1"}`, modeKind, "minimum must be a number"},
		{"minLength negative", `{"minLength":-1}`, modeKind, "non-negative integer"},
		{"items array form", `{"type":"array","items":[{"type":"string"}]}`, modeKind, "items must be a schema object"},
		{"additionalProperties number", `{"additionalProperties":1}`, modeKind, "boolean or a schema object"},
		{"properties not object", `{"properties":[]}`, modeKind, "properties must be an object"},
		{"not an object", `[]`, modeKind, "not an object"},
		{"duplicate keyword", `{"minLength":1,"minLength":5}`, modeKind, `member "minLength" appears more than once`},
		{"duplicate nested keyword", `{"properties":{"a":{"type":"string"},"a":{}}}`, modeKind, `member "a" appears more than once`},
		{"trailing data", `{} {}`, modeKind, "trailing data"},
		{"not json", `{`, modeKind, "unexpected EOF"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := compile([]byte(tc.src), tc.m)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("compile(%s) error = %v, want one containing %q", tc.src, err, tc.want)
			}
		})
	}
}

func TestCompileAcceptsTheContractShapes(t *testing.T) {
	t.Parallel()
	s := mustCompile(t, `{"$schema":"x","$id":"y","title":"t","description":"d","examples":[{}],"default":"none",
		"type":["object","null"],"maxProperties":3,"additionalProperties":true,
		"properties":{"a":{"type":"number","minimum":-1.5,"maximum":1e2,"enum":[1,2.0]},
		              "b":{"type":"array","minItems":1,"maxItems":2,"x-unique-by":"id","items":{"type":"object"}},
		              "c":{"type":"string","minLength":2,"x-max-bytes":10,"x-on-violation":"truncate","format":"date-time"}},
		"x-recommended":["a"]}`, modeKind)
	if !reflect.DeepEqual(s.Types(), []string{"object", "null"}) || s.AdditionalProperties() != nil {
		t.Fatalf("root = %+v", s)
	}
	a, _ := s.Property("a")
	if a.minimum.spelling != "-1.5" || a.maximum.spelling != "1e2" || len(a.enum) != 2 {
		t.Fatalf("a = %+v", a)
	}
	c, _ := s.Property("c")
	if c.MaxBytes() != 10 || c.OnViolation() != "truncate" || c.Format() != "date-time" {
		t.Fatalf("c = %+v", c)
	}
}

func TestParseKindFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		kind    string
		version uint64
		ok      bool
	}{
		{"friction.v1.json", "friction", 1, true},
		{"deploy-note.v12.json", "deploy-note", 12, true},
		{"friction.v0.json", "", 0, false},
		{"friction.v01.json", "", 0, false},
		{"friction.json", "", 0, false},
		{"Friction.v1.json", "", 0, false},
		{"envelope.v1.json", "", 0, false},
		{".v1.json", "", 0, false},
		{"friction.v1.txt", "", 0, false},
		{"friction.v9007199254740992.json", "", 0, false},
		{"friction.v9007199254740991.json", "friction", 9007199254740991, true},
	}
	for _, tc := range tests {
		kind, version, ok := parseKindFile(tc.name)
		if kind != tc.kind || version != tc.version || ok != tc.ok {
			t.Errorf("parseKindFile(%q) = %q, %d, %v; want %q, %d, %v", tc.name, kind, version, ok, tc.kind, tc.version, tc.ok)
		}
	}
}

func TestKeywordCodesMatchWarningsJSON(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../conformance/warnings.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Codes []struct {
			Code string `json:"code"`
		} `json:"codes"`
		KeywordCodes map[string]string `json:"keyword_codes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.KeywordCodes, keywordCodes) {
		t.Fatalf("keyword_codes differ:\n contract %v\n go       %v", doc.KeywordCodes, keywordCodes)
	}
	listed := map[string]bool{}
	for _, c := range doc.Codes {
		listed[c.Code] = true
	}
	for _, c := range []string{codeNoSchema, codeUnknownSchemaVersion} {
		if !listed[c] {
			t.Errorf("code %q is not in warnings.json", c)
		}
	}
}

func TestFirstDuplicateKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		src  string
		want string
		dup  bool
	}{
		{`{"o":{"x":1},"o":2}`, "o", true},
		{`{"payload":{},"kind":"review","summary":"review"}`, "", false},
		{`{"a":{},"b":"x","c":"x"}`, "", false},
		{`{"a":[{"k":1},{"k":2}],"b":[[],{}]}`, "", false},
		{`{"a":{"k":1,"k":2}}`, "k", true},
		{`{"a":[1,2],"a":3}`, "a", true},
		{`{"kind":"x","payload":{"category":"a","details":"d"},"a/b":1,"a/b":2}`, "a/b", true},
		{`{}`, "", false},
		{`[1,1,{"z":null,"z":true}]`, "z", true},
		{`{"a":"a","b":"a"}`, "", false},
		{`{"x":1`, "", false},
	}
	for _, tc := range tests {
		name, dup := firstDuplicateKey([]byte(tc.src))
		if name != tc.want || dup != tc.dup {
			t.Errorf("firstDuplicateKey(%s) = %q, %v; want %q, %v", tc.src, name, dup, tc.want, tc.dup)
		}
	}
}
