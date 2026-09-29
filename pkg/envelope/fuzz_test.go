package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/pkg/canonjson"
)

// FuzzDecode checks the decoder's promises on arbitrary bytes: it never
// panics; it accepts exactly the bodies the contract accepts, with
// encoding/json's grammar check as the oracle for "is JSON" (it keeps number
// spellings out of the question, which is what makes it usable: 1e400 is
// valid JSON) and a token scan for "is an object" and the nesting depth; and
// what it stores obeys the contract's shape. The seed corpus is every
// fixture body.json (the generated size cases are the conformance tests'
// business) plus the hand-written cases under testdata/fuzz/FuzzDecode.
func FuzzDecode(f *testing.F) {
	for _, sub := range []string{"decode/rows", "decode/interactions", "hash"} {
		dirs, err := filepath.Glob(filepath.Join(conformanceDir, sub, "*", "body.json"))
		if err != nil {
			f.Fatal(err)
		}
		for _, path := range dirs {
			raw, err := os.ReadFile(path)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(raw)
		}
	}
	f.Add([]byte(`{"kind":"friction","schema_version":1e0,"payload":[1,{"a":"\ud800"}],"context":{"k":[1]},"Kind":"x","occurred_at":"2026-01-01t00:00:00z"}`))
	f.Add([]byte(strings.Repeat(`{"a":`, 600) + "1" + strings.Repeat("}", 600)))
	f.Fuzz(func(t *testing.T, body []byte) {
		env, warnings, err := Decode(body)
		accepted := len(body) <= BodyLimit && json.Valid(body) && isObjectWithin(body, MaxDepth)
		if (err == nil) != accepted {
			t.Fatalf("accepted=%v, err=%v for %q", accepted, err, body)
		}
		if err != nil {
			var r *Rejection
			if !errors.As(err, &r) || (r.Status != 400 && r.Status != 413) || env != nil || warnings != nil {
				t.Fatalf("bad rejection %v", err)
			}
			return
		}
		if env.Kind == "" || !utf8.ValidString(env.Kind) || env.SchemaVersion < 1 || env.SchemaVersion > schemaVersionMax || env.Payload == nil {
			t.Fatalf("envelope %+v", env)
		}
		for name, value := range map[string]string{"summary": env.Summary, "machine": env.Machine, "model": env.Model, "harness": env.Harness, "project": env.Project, "key": env.Key, "occurred_at": env.OccurredAt} {
			if !utf8.ValidString(value) {
				t.Fatalf("%s is not valid UTF-8", name)
			}
			if limit := rules[name].limit; limit > 0 && len(value) > limit {
				t.Fatalf("%s is %d bytes, limit %d", name, len(value), limit)
			}
		}
		if len(env.Context) > contextEntries {
			t.Fatalf("context has %d entries", len(env.Context))
		}
		for k, v := range env.Context {
			if !utf8.ValidString(k) || !utf8.ValidString(v) || len(v) > contextValueBytes {
				t.Fatalf("context entry %q=%q", k, v)
			}
		}
		for _, d := range warnings {
			if d.Code == "" || (d.Pointer != "" && d.Pointer[0] != '/') || !utf8.ValidString(d.Pointer) {
				t.Fatalf("warning %+v", d)
			}
		}
		stored := canonjson.Marshal(env.Tree())
		if !json.Valid(stored) {
			t.Fatalf("stored envelope is not JSON: %s", stored)
		}
		// The stored envelope is a fixed point of the write path: decoded
		// again as the import path does (kind left out when inferred, the
		// depth headroom inference can add; the byte cap is import's
		// business, so none here), it stores the same bytes.
		tree := env.Tree()
		if env.Kind == "unknown" {
			delete(tree, "kind")
		}
		again, _, err := DecodeWithLimits(canonjson.Marshal(tree), len(stored)+len(`,"kind":"unknown"`), MaxDepth+StoredDepthHeadroom)
		if err != nil {
			t.Fatalf("stored envelope does not decode: %v\n %s", err, stored)
		}
		if got := canonjson.Marshal(again.Tree()); !bytes.Equal(got, stored) {
			t.Fatalf("stored envelope is not a fixed point of Decode:\n %s\n %s", stored, got)
		}
	})
}

// isObjectWithin reports whether body (valid JSON) is an object whose
// containers nest at most maxDepth levels, the body object being level 1.
func isObjectWithin(body []byte, maxDepth int) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	first := true
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return !first
		}
		if err != nil {
			return false
		}
		if first {
			if d, ok := tok.(json.Delim); !ok || d != '{' {
				return false
			}
			first = false
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
				if depth > maxDepth {
					return false
				}
			default:
				depth--
			}
		}
	}
}
