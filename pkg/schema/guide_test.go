package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// payloadOf parses src with number spellings kept, as the decoder would.
func payloadOf(t *testing.T, src string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(src))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("payload %s: %v", src, err)
	}
	return v
}

// keys renders details as "code pointer", sorted, for comparison.
func keys(details []Detail) []string {
	out := make([]string, 0, len(details))
	for _, d := range details {
		out = append(out, d.Code+" "+d.Pointer)
	}
	sort.Strings(out)
	return out
}

func sortedJSON(t *testing.T, v any) string {
	t.Helper()
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func TestGuideKeywords(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		schema  string
		payload string
		want    []string // "code pointer"
		stored  string   // payload after transforms, sorted JSON; "" means unchanged
	}{
		{"type mismatch stops further checks", `{"properties":{"a":{"type":"string","minLength":5,"enum":["x"]}}}`,
			`{"a":1}`, []string{"type_mismatch /payload/a"}, ""},
		{"type list", `{"properties":{"a":{"type":["string","null"]}}}`, `{"a":null}`, nil, ""},
		{"integer accepts zero fraction", `{"properties":{"a":{"type":"integer"}}}`, `{"a":2.0}`, nil, ""},
		{"integer rejects fraction", `{"properties":{"a":{"type":"integer"}}}`, `{"a":2.5}`, []string{"type_mismatch /payload/a"}, ""},
		{"number accepts integer", `{"properties":{"a":{"type":"number"}}}`, `{"a":2}`, nil, ""},
		{"boolean is not a number", `{"properties":{"a":{"type":"number"}}}`, `{"a":true}`, []string{"type_mismatch /payload/a"}, ""},
		{"minimum", `{"properties":{"a":{"type":"number","minimum":1}}}`, `{"a":0.999}`, []string{"out_of_range /payload/a"}, ""},
		{"minimum met exactly", `{"properties":{"a":{"type":"number","minimum":1}}}`, `{"a":1.0}`, nil, ""},
		{"maximum", `{"properties":{"a":{"type":"number","maximum":5}}}`, `{"a":9}`, []string{"out_of_range /payload/a"}, ""},
		{"maximum huge exponent", `{"properties":{"a":{"type":"number","maximum":5}}}`, `{"a":1e` + strings.Repeat("9", 40) + `}`, []string{"out_of_range /payload/a"}, ""},
		{"minimum tiny exponent", `{"properties":{"a":{"type":"number","minimum":0}}}`, `{"a":-1e-` + strings.Repeat("9", 40) + `}`, []string{"out_of_range /payload/a"}, ""},
		{"negative zero is not below zero", `{"properties":{"a":{"type":"number","minimum":0}}}`, `{"a":-0}`, nil, ""},
		{"both bounds violated", `{"properties":{"a":{"type":"number","minimum":1,"maximum":1}}}`, `{"a":2}`, []string{"out_of_range /payload/a"}, ""},
		{"enum string", `{"properties":{"a":{"type":"string","enum":["x","y"]}}}`, `{"a":"z"}`, []string{"out_of_range /payload/a"}, ""},
		{"enum string after transform", `{"properties":{"a":{"type":"string","x-normalize":"token","enum":["x"]}}}`, `{"a":" X "}`, nil, `{"a":"x"}`},
		{"enum number across spellings", `{"properties":{"a":{"enum":[2]}}}`, `{"a":2.0}`, nil, ""},
		{"enum number miss", `{"properties":{"a":{"enum":[2]}}}`, `{"a":"2"}`, []string{"out_of_range /payload/a"}, ""},
		{"enum null and boolean", `{"properties":{"a":{"enum":[null]},"b":{"enum":[true]}}}`, `{"a":null,"b":false}`, []string{"out_of_range /payload/b"}, ""},
		{"minLength counts code points after transform", `{"properties":{"a":{"type":"string","x-trim":true,"minLength":2}}}`, `{"a":" é "}`, []string{"out_of_range /payload/a"}, `{"a":"é"}`},
		{"minLength met by multibyte", `{"properties":{"a":{"type":"string","minLength":2}}}`, `{"a":"éé"}`, nil, ""},
		{"minItems", `{"properties":{"a":{"type":"array","minItems":1}}}`, `{"a":[]}`, []string{"out_of_range /payload/a"}, ""},
		{"maxItems", `{"properties":{"a":{"type":"array","maxItems":1}}}`, `{"a":[1,2]}`, []string{"out_of_range /payload/a"}, ""},
		{"maxProperties", `{"properties":{"a":{"type":"object","maxProperties":1}}}`, `{"a":{"x":1,"y":2}}`, []string{"out_of_range /payload/a", "unknown_field /payload/a/x", "unknown_field /payload/a/y"}, ""},
		{"format date-time", `{"properties":{"a":{"type":"string","format":"date-time"}}}`, `{"a":"yesterday"}`, []string{"invalid_format /payload/a"}, ""},
		{"format date-time ok", `{"properties":{"a":{"type":"string","format":"date-time"}}}`, `{"a":"2026-01-01T00:00:00Z"}`, nil, ""},
		{"x-max-bytes warns without truncate", `{"properties":{"a":{"type":"string","x-max-bytes":3}}}`, `{"a":"abcd"}`, []string{"too_long /payload/a"}, ""},
		{"x-max-bytes counts bytes", `{"properties":{"a":{"type":"string","x-max-bytes":3}}}`, `{"a":"éé"}`, []string{"too_long /payload/a"}, ""},
		{"x-on-violation truncate is silent", `{"properties":{"a":{"type":"string","x-max-bytes":3,"x-on-violation":"truncate"}}}`, `{"a":"aéb"}`, nil, `{"a":"aé"}`},
		{"x-on-violation warn is the default", `{"properties":{"a":{"type":"string","x-max-bytes":1,"x-on-violation":"warn"}}}`, `{"a":"ab"}`, []string{"too_long /payload/a"}, ""},
		{"x-trim", `{"properties":{"a":{"type":"string","x-trim":true}}}`, `{"a":"\u0085 a 　"}`, nil, `{"a":"a"}`},
		{"x-normalize token", `{"properties":{"a":{"type":"string","x-normalize":"token"}}}`, `{"a":"  Docs   And Config "}`, nil, `{"a":"docs-and-config"}`},
		{"trim before truncate", `{"properties":{"a":{"type":"string","x-trim":true,"x-max-bytes":2,"x-on-violation":"truncate"}}}`, `{"a":"   ab   "}`, nil, `{"a":"ab"}`},
		{"x-recommended", `{"type":"object","properties":{"a":{},"b":{}},"x-recommended":["a","b"]}`, `{"b":1}`, []string{"missing_recommended /payload/a"}, ""},
		{"x-recommended nested pointer", `{"properties":{"o":{"type":"object","properties":{"a/b":{}},"x-recommended":["a/b"]}}}`, `{"o":{}}`, []string{"missing_recommended /payload/o/a~1b"}, ""},
		{"x-unique-by", `{"properties":{"r":{"type":"array","x-unique-by":"slot","items":{"type":"object","properties":{"slot":{}}}}}}`,
			`{"r":[{"slot":"a"},{"slot":"b"},{"slot":"a"},{"slot":"a"}]}`, []string{"duplicate_slot /payload/r/2", "duplicate_slot /payload/r/3"}, ""},
		{"x-unique-by compares strings only", `{"properties":{"r":{"type":"array","x-unique-by":"slot","items":{"type":"object","properties":{"slot":{}}}}}}`,
			`{"r":[{"slot":1},{"slot":"1"},{"slot":1},{"x":1},"s"]}`, []string{"unknown_field /payload/r/3/x", "type_mismatch /payload/r/4"}, ""},
		{"unknown_field", `{"type":"object","properties":{"a":{}}}`, `{"a":1,"b":2}`, []string{"unknown_field /payload/b"}, ""},
		{"unknown_field nested and escaped", `{"properties":{"o":{"type":"object","properties":{}}}}`, `{"o":{"a/b":1,"m~n":2}}`, []string{"unknown_field /payload/o/a~1b", "unknown_field /payload/o/m~0n"}, ""},
		{"additionalProperties schema is not applied", `{"properties":{"o":{"type":"object","additionalProperties":{"type":"string"}}}}`, `{"o":{"x":1}}`, []string{"unknown_field /payload/o/x"}, ""},
		{"unlisted members are not descended", `{"type":"object","properties":{}}`, `{"deep":{"deeper":{"x":1}}}`, []string{"unknown_field /payload/deep"}, ""},
		{"items transform in place", `{"properties":{"r":{"type":"array","items":{"type":"string","x-trim":true}}}}`, `{"r":[" a ","b "]}`, nil, `{"r":["a","b"]}`},
		{"empty schema lists no members", `{}`, `{"a":[1,{"b":null}]}`, []string{"unknown_field /payload/a"}, ""},
		{"root type mismatch is one warning", `{"type":"array"}`, `{"a":1}`, []string{"type_mismatch /payload"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := mustCompile(t, tc.schema, modeKind)
			payload := payloadOf(t, tc.payload)
			before := sortedJSON(t, payload)
			got := keys(s.Guide(payload, nil))
			want := tc.want
			sort.Strings(want)
			if len(got) == 0 && len(want) == 0 {
				got, want = nil, nil
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("details = %v, want %v", got, want)
			}
			stored := tc.stored
			if stored == "" {
				stored = before
			}
			if after := sortedJSON(t, payload); after != stored {
				t.Fatalf("payload after guide = %s, want %s", after, stored)
			}
		})
	}
}

func TestGuidePlacedMembersAtRootOnly(t *testing.T) {
	t.Parallel()
	s := mustCompile(t, `{"type":"object","properties":{"o":{"type":"object","properties":{}}}}`, modeKind)
	payload := payloadOf(t, `{"moved":{"x":1},"context_raw":"s","value":1,"other":1,"o":{"moved":1}}`)
	placed := map[string]bool{"moved": true, "context_raw": true, "value": true}
	got := keys(s.Guide(payload, placed))
	want := []string{"unknown_field /payload/o/moved", "unknown_field /payload/other"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("details = %v, want %v", got, want)
	}
}

func TestGuideDetailShape(t *testing.T) {
	t.Parallel()
	s := mustCompile(t, `{"properties":{"a":{"type":"string"}}}`, modeKind)
	details := s.Guide(payloadOf(t, `{"a":1}`), nil)
	if len(details) != 1 || details[0].Message == "" {
		t.Fatalf("details = %+v", details)
	}
	raw, err := json.Marshal(details[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"code":"type_mismatch","pointer":"/payload/a","message":"expected string, got integer"}` {
		t.Fatalf("json = %s", raw)
	}
}

func TestGuideNilPayload(t *testing.T) {
	t.Parallel()
	friction, _ := Kind("friction", 1)
	got := keys(friction.Guide(nil, nil))
	want := []string{"missing_recommended /payload/category", "missing_recommended /payload/details"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("details = %v, want %v", got, want)
	}
}

func TestValidateLookups(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind    string
		version uint64
		want    []string
	}{
		{"friction", 1, []string{"missing_recommended /payload/category", "missing_recommended /payload/details"}},
		{"friction", 2, []string{"unknown_schema_version /schema_version"}},
		{"review", 7, []string{"unknown_schema_version /schema_version"}},
		{"deploy-note", 1, []string{"no_schema /kind"}},
		{"envelope", 1, []string{"no_schema /kind"}},
		{"unknown", 1, []string{"no_schema /kind"}},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s/%d", tc.kind, tc.version), func(t *testing.T) {
			t.Parallel()
			payload := map[string]any{}
			got := keys(Validate(tc.kind, tc.version, payload, nil))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("details = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGuideDoesNotShareState(t *testing.T) {
	t.Parallel()
	friction, _ := Kind("friction", 1)
	p1 := payloadOf(t, `{"category":" A ","details":"d"}`)
	p2 := payloadOf(t, `{"category":"B","details":"d"}`)
	friction.Guide(p1, nil)
	friction.Guide(p2, nil)
	if p1["category"] != "a" || p2["category"] != "b" {
		t.Fatalf("payloads = %v %v", p1, p2)
	}
	category, _ := friction.Property("category")
	if category.Normalize() != "token" {
		t.Fatal("schema changed")
	}
}
