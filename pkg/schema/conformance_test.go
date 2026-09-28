package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// The fixtures under conformance/ are the executable contract. The decoder
// runs every one of them end to end in its own package; here they drive the
// parts this package owns: the normalisation of envelope strings, the
// occurred_at parser and the kind-schema guide. A body that only the
// contract's own token reader can parse faithfully (invalid UTF-8, duplicate
// members) is skipped by name, with the reason.

const conformanceDir = "../../conformance"

type fixture struct {
	name     string
	body     map[string]any
	expected map[string]any
	stored   map[string]any // stored.json for decode fixtures, canonical.json for hash fixtures
}

// parseBody parses a fixture body with encoding/json, keeping number
// spellings, and reports why the body needs the contract's own reader when it
// does.
func parseBody(raw []byte) (map[string]any, string) {
	if !utf8.Valid(raw) {
		return nil, "invalid UTF-8 (decoder territory)"
	}
	if dup := duplicateMember(raw); dup != "" {
		return nil, "duplicate member " + dup + " (decoder territory)"
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, "not JSON: " + err.Error()
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, "not an object"
	}
	return obj, ""
}

// duplicateMember walks the token stream and returns the first member name
// that repeats within one object, or "".
func duplicateMember(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		object bool
		seen   map[string]bool
		key    bool // an object frame expecting a member name next
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) || err != nil {
			return ""
		}
		top := (*frame)(nil)
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &frame{object: true, seen: map[string]bool{}, key: true})
				continue
			case '[':
				stack = append(stack, &frame{})
				continue
			default:
				stack = stack[:len(stack)-1]
			}
		case string:
			if top != nil && top.object && top.key {
				if top.seen[t] {
					return t
				}
				top.seen[t] = true
				top.key = false
				continue
			}
		}
		if top != nil && top.object {
			top.key = true
		}
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

// loadFixtures returns every fixture directory under sub that has a raw body
// and the named result file, parsed; skipped bodies are logged.
func loadFixtures(t *testing.T, sub, result string) []fixture {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(conformanceDir, sub, "*"))
	if err != nil {
		t.Fatal(err)
	}
	var out []fixture
	for _, dir := range dirs {
		name := filepath.Join(sub, filepath.Base(dir))
		raw, err := os.ReadFile(filepath.Join(dir, "body.json"))
		if err != nil {
			continue // body.gen.json fixtures are size cases for the decoder
		}
		if _, err := os.Stat(filepath.Join(dir, result)); err != nil {
			continue // rejected bodies have no stored form
		}
		body, why := parseBody(raw)
		if why != "" {
			t.Logf("skip %s: %s", name, why)
			continue
		}
		out = append(out, fixture{
			name:     name,
			body:     body,
			expected: readJSON(t, filepath.Join(dir, "expected.json")),
			stored:   readJSON(t, filepath.Join(dir, result)),
		})
	}
	if len(out) == 0 {
		t.Fatalf("no fixtures under %s", sub)
	}
	return out
}

func allDecodeFixtures(t *testing.T) []fixture {
	t.Helper()
	return append(loadFixtures(t, "decode/rows", "stored.json"), loadFixtures(t, "decode/interactions", "stored.json")...)
}

// normaliseMember applies the contract's fixed order to one envelope string:
// trim, newlines to spaces in summary, token normalisation, truncation.
func normaliseMember(name, s string) string {
	rule, _ := Envelope().Property(name)
	s = Trim(s)
	if name == "summary" {
		s = NewlinesToSpaces(s)
	}
	if rule.Normalize() == "token" {
		s = Token(s)
	}
	if rule.MaxBytes() > 0 && rule.OnViolation() == "truncate" {
		s, _ = TruncateBytes(s, rule.MaxBytes())
	}
	return s
}

func TestConformanceNormalisationVectors(t *testing.T) {
	t.Parallel()
	fixtures := append(loadFixtures(t, "hash", "canonical.json"), allDecodeFixtures(t)...)
	members := []string{"kind", "key", "summary", "machine", "model", "harness", "project"}
	checked := 0
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			for _, name := range members {
				sent, ok := f.body[name].(string)
				if !ok {
					continue // absent, null or coerced: the decoder's rows
				}
				if name == "key" && strings.HasPrefix(f.name, "hash/") {
					continue // key is outside identity
				}
				got := normaliseMember(name, sent)
				want, present := f.stored[name].(string)
				if name == "kind" && got == "" {
					want, present = "", false // stored as unknown: the decoder's row
				}
				if got == "" && present && name == "kind" && want == "unknown" {
					present = false
				}
				if !present {
					if got != "" {
						t.Errorf("%s: normalised %q but the stored form omits it", name, got)
					}
					continue
				}
				if got != want {
					t.Errorf("%s: normalised %q, stored %q", name, got, want)
				}
				checked++
			}
		})
	}
	if checked == 0 {
		t.Fatal("no vectors checked")
	}
}

func TestConformanceOccurredAt(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, f := range allDecodeFixtures(t) {
		sent, ok := f.body["occurred_at"].(string)
		if !ok {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			got, parsed := NormalizeDateTime(sent)
			stored, present := f.stored["occurred_at"].(string)
			// The raw text lands in context, or in payload.context_overflow
			// when the context was already full.
			context, _ := f.stored["context"].(map[string]any)
			raw, kept := context["occurred_at_raw"].(string)
			if !kept {
				storedPayload, _ := f.stored["payload"].(map[string]any)
				overflow, _ := storedPayload["context_overflow"].(map[string]any)
				raw, kept = overflow["occurred_at_raw"].(string)
			}
			switch {
			case parsed && (!present || stored != got):
				t.Fatalf("parsed %q as %q; stored form has %q (%v)", sent, got, stored, present)
			case !parsed && present:
				t.Fatalf("did not parse %q; stored form has %q", sent, stored)
			case !parsed && (!kept || raw != sent):
				t.Fatalf("did not parse %q; stored context.occurred_at_raw is %q (%v)", sent, raw, kept)
			}
			checked++
		})
	}
	if checked == 0 {
		t.Fatal("no occurred_at vectors checked")
	}
}

var envelopeMembers = map[string]bool{
	"kind": true, "schema_version": true, "key": true, "summary": true, "machine": true, "model": true,
	"harness": true, "project": true, "occurred_at": true, "context": true, "payload": true,
}

var guideCodes = map[string]bool{
	"unknown_field": true, "type_mismatch": true, "out_of_range": true, "too_long": true,
	"invalid_format": true, "no_schema": true, "unknown_schema_version": true, "missing_recommended": true,
}

var versionRe = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)

// TestConformanceKindSchemaGuides runs Validate on every fixture whose body
// is envelope-shaped enough that the payload reaches the guide unchanged by
// inference: only envelope members at the top level, an explicit kind, an
// object payload. The guide's share of the expected warnings must match as a
// multiset of (code, pointer) and the transformed payload must equal the
// stored one.
func TestConformanceKindSchemaGuides(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, f := range allDecodeFixtures(t) {
		f := f
		kind, _ := f.body["kind"].(string)
		payload, isObject := f.body["payload"].(map[string]any)
		version := uint64(1)
		if sv, present := f.body["schema_version"]; present {
			n, isNum := sv.(json.Number)
			if !isNum || !versionRe.MatchString(string(n)) {
				continue // defaulted with a warning: the decoder's row
			}
			if err := json.Unmarshal([]byte(n), &version); err != nil || version > 1<<53-1 {
				continue
			}
		}
		if Token(kind) == "" || !isObject {
			continue
		}
		if ctx, present := f.body["context"]; present {
			// A non-object context, or one that overflows once
			// occurred_at_raw is added, places members in the payload.
			if m, ok := ctx.(map[string]any); !ok || len(m) >= 32 {
				continue
			}
		}
		shaped := true
		for name := range f.body {
			if !envelopeMembers[name] {
				shaped = false
			}
		}
		if !shaped {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			got := keys(Validate(Token(kind), version, payload, nil))
			var want []string
			for _, w := range f.expected["warnings"].([]any) {
				m := w.(map[string]any)
				code, pointer := m["code"].(string), m["pointer"].(string)
				// The guide's share: payload pointers, plus the two lookup codes.
				inPayload := strings.HasPrefix(pointer, "/payload/")
				lookup := code == "no_schema" || code == "unknown_schema_version"
				if (guideCodes[code] && inPayload) || lookup || (strings.HasPrefix(code, "duplicate_") && code != "duplicate_key") {
					want = append(want, code+" "+pointer)
				}
			}
			sort.Strings(want)
			if len(got) == 0 && len(want) == 0 {
				got, want = nil, nil
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("guide details = %v, want %v", got, want)
			}
			if after, stored := sortedJSON(t, payload), sortedJSON(t, f.stored["payload"]); after != stored {
				t.Fatalf("payload after guide = %s, stored %s", after, stored)
			}
			checked++
		})
	}
	if checked < 3 {
		t.Fatalf("only %d guide fixtures checked; expected the kind-schema fixtures at least", checked)
	}
}
