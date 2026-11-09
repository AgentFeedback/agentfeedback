package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
)

// A batch keeps interpreter startup out of each case. Base64 (encoding/json's
// []byte representation) preserves malformed UTF-8 and duplicate object keys.
const differentialReference = `
import base64, json, sys
from conformance.reference.decode import decode
from conformance.reference.canon import canonical_bytes
for raw in json.load(sys.stdin):
    result = decode(base64.b64decode(raw))
    print(json.dumps({"status": result.status, "error": result.error or "",
        "warnings": sorted([w.code, w.pointer] for w in result.warnings),
        "stored": base64.b64encode(canonical_bytes(result.envelope)).decode() if result.status == 201 else ""}))
`

func TestDecodeDifferential(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable; see docs/develop.md")
	}
	const seed = uint64(66)
	rng := rand.New(rand.NewPCG(seed, seed))
	bodies := [][]byte{
		[]byte(`{"payload":{"x":1,"x":2,"x":3},"payload":{"x":4},"payload":null}`),
		[]byte(`{"summary":{"a":1},"payload":[],"context":[1],"extra":true}`),
		[]byte("{\"summary\":\"\xff\xfe\",\"payload\":{\"\xff\":1}}"),
		[]byte(`{"kind":"friction","schema_version":1,"payload":{"category":false,"summary":[],"suggested_fix":42}}`),
	}
	for _, malformed := range []string{"\xe2\x82", "\xed\xa0\x80", "\xc0\xaf", "\xf4\x90\x80\x80"} {
		bodies = append(bodies, []byte(`{"summary":"`+malformed+`"}`))
	}
	keys := []string{"kind", "schema_version", "summary", "payload", "context", "machine", "project", "model", "harness", "occurred_at", "key", "extra", "a/b", "~"}
	values := []string{`null`, `true`, `false`, `0`, `1`, `1e0`, `-1`, `1e400`, `""`, `"friction"`, `"unknown"`, `" a "`, `"\ud800"`, `"\u0000"`, `"2026-01-01T00:00:00Z"`, `[]`, `[1,null,"x"]`, `{}`, `{"a":1,"a":2}`, `{"category":"tooling","summary":"x"}`, `{"a/b":null,"~":[true]}`, `"` + string([]byte{0xff, 0xfe}) + `"`}
	for range 1000 {
		var body strings.Builder
		body.WriteByte('{')
		for j, n := 0, 1+rng.IntN(15); j < n; j++ {
			if j > 0 {
				body.WriteByte(',')
			}
			key, _ := json.Marshal(keys[rng.IntN(len(keys))])
			body.Write(key)
			body.WriteByte(':')
			body.WriteString(values[rng.IntN(len(values))])
		}
		body.WriteByte('}')
		bodies = append(bodies, []byte(body.String()))
	}
	bodies = append(bodies, []byte(`null`), []byte(`[]`), []byte(`{"x":`), []byte(`{"x":1} trailing`))
	input, err := json.Marshal(bodies)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", differentialReference)
	cmd.Dir = "../.."
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("Python reference: %v\n%s", err, stderr.Bytes())
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for i, body := range bodies {
		var want struct {
			Status   int         `json:"status"`
			Error    string      `json:"error"`
			Warnings [][2]string `json:"warnings"`
			Stored   []byte      `json:"stored"`
		}
		if err := decoder.Decode(&want); err != nil {
			t.Fatalf("seed_%d/case_%d body=%q: reference output: %v", seed, i, body, err)
		}
		t.Run(fmt.Sprintf("seed_%d/case_%d", seed, i), func(t *testing.T) {
			env, warnings, err := Decode(body)
			status, code := 201, ""
			if err != nil {
				var rejection *Rejection
				if !errors.As(err, &rejection) {
					t.Fatal(err)
				}
				status, code = rejection.Status, rejection.Code
			}
			if status != want.Status || code != want.Error {
				t.Fatalf("body=%q\nstatus/code: Go %d/%s, Python %d/%s", body, status, code, want.Status, want.Error)
			}
			gotWarnings := make([][2]string, 0, len(warnings))
			for _, w := range warnings {
				gotWarnings = append(gotWarnings, [2]string{w.Code, w.Pointer})
			}
			slices.SortFunc(gotWarnings, func(a, b [2]string) int {
				if c := strings.Compare(a[0], b[0]); c != 0 {
					return c
				}
				return strings.Compare(a[1], b[1])
			})
			if !slices.Equal(gotWarnings, want.Warnings) {
				t.Fatalf("body=%q\nwarnings: Go %v, Python %v", body, gotWarnings, want.Warnings)
			}
			if status == 201 {
				got := canonjson.Marshal(env.Tree())
				if !bytes.Equal(got, want.Stored) {
					t.Fatalf("body=%q\nstored: Go %s\nPython %s", body, got, want.Stored)
				}
			}
		})
	}
}
