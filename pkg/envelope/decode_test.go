package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/pkg/canonjson"
)

func decodeString(t *testing.T, body string) (*Envelope, []string) {
	t.Helper()
	env, warnings, err := Decode([]byte(body))
	if err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return env, keys(warnings)
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		raw  any
		want uint64
		ok   bool
	}{
		{json.Number("1"), 1, true},
		{json.Number("9007199254740991"), 9007199254740991, true},
		{json.Number("9007199254740992"), 0, false},
		{json.Number("18446744073709551616"), 0, false}, // beyond uint64: defaulted, never rejected
		{json.Number(strings.Repeat("9", 5000)), 0, false},
		{json.Number("0"), 0, false},
		{json.Number("-1"), 0, false},
		{json.Number("2.0"), 0, false},
		{json.Number("1e0"), 0, false},
		{json.Number("01"), 0, false},
		{"2", 0, false},
		{true, 0, false},
		{nil, 0, false},
		{[]any{json.Number("2")}, 0, false},
	} {
		got, ok := parseVersion(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("%v: got %d %v, want %d %v", c.raw, got, ok, c.want, c.ok)
		}
	}
}

func TestDecodeMinimal(t *testing.T) {
	t.Parallel()
	env, warnings := decodeString(t, `{}`)
	want := &Envelope{Kind: "unknown", SchemaVersion: 1, Payload: map[string]any{}}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("envelope %#v, want %#v", env, want)
	}
	if !reflect.DeepEqual(warnings, []string{"missing_kind /kind", "payload_inferred /payload", "missing_recommended /summary", "missing_recommended /machine", "missing_recommended /model"}) {
		t.Errorf("warnings %v", warnings)
	}
	if got := string(canonjson.Marshal(env.Tree())); got != `{"kind":"unknown","payload":{},"schema_version":1}` {
		t.Errorf("tree %s", got)
	}
}

func TestDecodeKeepsSpellingsAndUnknownFields(t *testing.T) {
	t.Parallel()
	body := `{"kind":"deploy-note","schema_version":3,"payload":{"n":[1.0,1e2,-0,0.10,123456789012345678901234567890],"extra":{"deep":[null,true,"x"]},"z":"\u2028/"},"key":" k ","occurred_at":"2026-09-27T09:58:12.5+02:00"}`
	env, warnings := decodeString(t, body)
	if env.SchemaVersion != 3 || env.Key != "k" || env.OccurredAt != "2026-09-27T07:58:12.500000Z" {
		t.Errorf("envelope %+v", env)
	}
	want := `{"key":"k","kind":"deploy-note","occurred_at":"2026-09-27T07:58:12.500000Z","payload":{"extra":{"deep":[null,true,"x"]},"n":[1.0,1e2,-0,0.10,123456789012345678901234567890],"z":"` + "\u2028" + `/"},"schema_version":3}`
	if got := string(canonjson.Marshal(env.Tree())); got != want {
		t.Errorf("tree\n got %s\nwant %s", got, want)
	}
	if !reflect.DeepEqual(warnings, []string{"no_schema /kind", "missing_recommended /summary", "missing_recommended /machine", "missing_recommended /model"}) {
		t.Errorf("warnings %v", warnings)
	}
}

func TestDecodeMovedOrderIsCodePointOrder(t *testing.T) {
	t.Parallel()
	// Whatever the body order, "moved" is placed first: it sorts before "x".
	for _, body := range []string{
		`{"kind":"k","x":2,"moved":1,"payload":{"x":0}}`,
		`{"kind":"k","moved":1,"x":2,"payload":{"x":0}}`,
	} {
		env, warnings := decodeString(t, body)
		if got := string(canonjson.Marshal(env.Payload)); got != `{"moved":{"value":1,"x":2},"x":0}` {
			t.Errorf("%s: payload %s", body, got)
		}
		if !reflect.DeepEqual(warnings, []string{"payload_wrapped /payload/moved", "moved_to_payload /payload/moved/value", "moved_to_payload /payload/moved/x", "no_schema /kind", "missing_recommended /summary", "missing_recommended /machine", "missing_recommended /model"}) {
			t.Errorf("%s: warnings %v", body, warnings)
		}
	}
}

func TestDecodeCollapseKeepsEveryWarning(t *testing.T) {
	t.Parallel()
	// Two invalid strings inside a coerced member are two warnings at the
	// member: the multiset counts.
	_, warnings := decodeString(t, `{"kind":"k","summary":{"a":"\udc00","b":["\udc00"]},"machine":"m","model":"x","payload":{}}`)
	if !reflect.DeepEqual(warnings, []string{"coerced /summary", "invalid_utf8 /summary", "invalid_utf8 /summary", "no_schema /kind"}) {
		t.Errorf("warnings %v", warnings)
	}
}

func TestDecodeContextValuesAndKeys(t *testing.T) {
	t.Parallel()
	env, warnings := decodeString(t, `{"kind":"k","summary":"s","machine":"m","model":"x","payload":{},"context":{"":"empty key kept","n":1,"o":{"a":"\udc00"},"long":"`+strings.Repeat("é", 1001)+`","k/ey":true}}`)
	if env.Context[""] != "empty key kept" || env.Context["n"] != "1" || env.Context["o"] != `{"a":"�"}` || env.Context["k/ey"] != "true" {
		t.Errorf("context %v", env.Context)
	}
	if got := len(env.Context["long"]); got != 2000 {
		t.Errorf("long value is %d bytes", got)
	}
	if !reflect.DeepEqual(warnings, []string{"coerced /context/k~1ey", "truncated /context/long", "coerced /context/n", "coerced /context/o", "invalid_utf8 /context/o", "no_schema /kind"}) {
		t.Errorf("warnings %v", warnings)
	}
}

func TestDecodeEmptyAfterNormalisation(t *testing.T) {
	t.Parallel()
	env, warnings := decodeString(t, "{\"kind\":\" \\u0085 \",\"key\":\"\\u00a0\",\"summary\":\"\\r\\n\",\"harness\":\"\\t\",\"machine\":\"m\",\"model\":\"x\",\"payload\":{}}")
	if env.Kind != "unknown" || env.Key != "" || env.Summary != "" || env.Harness != "" {
		t.Errorf("envelope %+v", env)
	}
	if !reflect.DeepEqual(warnings, []string{"missing_kind /kind", "missing_recommended /summary"}) {
		t.Errorf("warnings %v", warnings)
	}
}

func TestDecodeTokenAndTrimOrder(t *testing.T) {
	t.Parallel()
	env, _ := decodeString(t, `{"kind":"  Friction  Report ","harness":"Claude\tCode\u3000X","summary":"  a\r\nb  ","machine":"m","model":"x","payload":{}}`)
	if env.Kind != "friction-report" || env.Harness != "claude-code-x" || env.Summary != "a b" {
		t.Errorf("envelope %+v", env)
	}
}

// A body with a warning per member must decode in time linear in its size:
// the trie keeps duplicate drops, remaps and collapses local.
func TestDecodeLinearOnWarningHeavyBodies(t *testing.T) {
	t.Parallel()
	const members = 200000
	var flat bytes.Buffer
	flat.WriteString(`{"kind":"friction"`)
	for i := 0; i < members; i++ {
		flat.WriteString(`,"m` + strconv.Itoa(i) + `":"\udc00"`)
	}
	flat.WriteString("}")
	var dups bytes.Buffer
	dups.WriteString(`{"kind":"friction","payload":{"a":1`)
	for i := 0; i < members; i++ {
		k := `"d` + strconv.Itoa(i) + `":`
		dups.WriteString("," + k + `"\udc00",` + k + "1")
	}
	dups.WriteString("}}")
	for name, body := range map[string][]byte{"flat": flat.Bytes(), "duplicates": dups.Bytes()} {
		start := time.Now()
		_, warnings, err := Decode(body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(warnings) < members {
			t.Errorf("%s: %d warnings", name, len(warnings))
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("%s: %d members took %s", name, members, elapsed)
		}
	}
}

func TestTreeOmitsAbsentMembers(t *testing.T) {
	t.Parallel()
	env := &Envelope{Kind: "k", SchemaVersion: 7, Summary: "s", Context: map[string]string{"a": "b"}, Payload: map[string]any{"p": json.Number("1")}}
	if got := string(canonjson.Marshal(env.Tree())); got != `{"context":{"a":"b"},"kind":"k","payload":{"p":1},"schema_version":7,"summary":"s"}` {
		t.Errorf("tree %s", got)
	}
	if tree := env.Tree(); reflect.ValueOf(tree["payload"]).Pointer() != reflect.ValueOf(env.Payload).Pointer() {
		t.Error("Tree copies the payload")
	}
}

func TestDecodeWithLimits(t *testing.T) {
	t.Parallel()
	deep := []byte(`{"a":[[0]]}`) // depth 3
	if _, _, err := DecodeWithLimits(deep, BodyLimit, 3); err != nil {
		t.Fatalf("at the depth limit: %v", err)
	}
	_, _, err := DecodeWithLimits(deep, BodyLimit, 2)
	var d *Rejection
	if !errors.As(err, &d) || d.Status != 400 || !strings.Contains(d.Message, "deeper than 2 levels") {
		t.Fatalf("over the depth limit: %v", err)
	}
	body := []byte(`{"kind":"friction"}`)
	if _, _, err := DecodeWithLimits(body, len(body), MaxDepth); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	_, _, err = DecodeWithLimits(body, len(body)-1, MaxDepth)
	var r *Rejection
	if !errors.As(err, &r) || r.Status != 413 || r.Code != "request_too_large" {
		t.Fatalf("over the limit: %v", err)
	}
	if want := fmt.Sprintf("body over %d bytes", len(body)-1); r.Message != want {
		t.Errorf("message %q, want %q", r.Message, want)
	}
}
