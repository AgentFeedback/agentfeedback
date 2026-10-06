package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

// The fixtures under conformance/ are the executable contract, and this
// package runs every one of them end to end: the raw body through Decode,
// the status and error code, the warnings as a multiset of (code, pointer),
// and the stored envelope as canonical bytes. One named test per row of the
// inference table reads its own fixture and adds the assertion that row is
// about; the set of fixture directories must equal both the manifest and the
// row table, so a fixture without a test or a test without a fixture fails.

const conformanceDir = "../../conformance"

// rows lists the inference table's fixtures in the table's order, each with
// the assertion its row is about beyond the generic comparison.
var rows = []struct {
	dir   string
	check func(t *testing.T, env *Envelope, warnings []schema.Detail, err error)
}{
	{"01-body-not-json", rejected(400, "bad_request")},
	{"02-body-not-object", rejected(400, "bad_request")},
	{"03-body-over-limit", rejected(413, "request_too_large")},
	{"04-invalid-utf8", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "invalid_utf8")
		if !strings.Contains(string(canonjson.Marshal(env.Tree())), "�") {
			t.Error("no U+FFFD in the stored envelope")
		}
	}},
	{"05-lone-surrogate", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "invalid_utf8")
	}},
	{"06-duplicate-member", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "duplicate_key")
	}},
	{"07-payload-not-object", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "payload_wrapped")
		if _, ok := env.Payload["value"]; !ok {
			t.Error("payload.value absent after wrapping")
		}
	}},
	{"08-unknown-member-moved", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "moved_to_payload")
	}},
	{"09-moved-member-collision", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		if _, ok := env.Payload["moved"].(map[string]any); !ok {
			t.Error("payload.moved is not an object")
		}
		for _, d := range w {
			if d.Code == "moved_to_payload" && !strings.HasPrefix(d.Pointer, "/payload/moved/") {
				t.Errorf("moved_to_payload at %s, want under /payload/moved/", d.Pointer)
			}
		}
	}},
	{"10-payload-inferred", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "payload_inferred")
	}},
	{"11-kind-missing", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "missing_kind")
		if env.Kind != "unknown" {
			t.Errorf("kind %q, want unknown", env.Kind)
		}
	}},
	{"12-schema-version-absent", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		hasNot(t, w, "schema_version_defaulted")
		if env.SchemaVersion != 1 {
			t.Errorf("schema_version %d, want 1", env.SchemaVersion)
		}
	}},
	{"13-schema-version-defaulted", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "schema_version_defaulted")
		if env.SchemaVersion != 1 {
			t.Errorf("schema_version %d, want 1", env.SchemaVersion)
		}
	}},
	{"14-truncated", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "truncated")
		for name, value := range map[string]string{"summary": env.Summary, "machine": env.Machine, "model": env.Model, "harness": env.Harness, "project": env.Project} {
			if limit := rules[name].limit; len(value) > limit {
				t.Errorf("%s is %d bytes, limit %d", name, len(value), limit)
			}
		}
	}},
	{"15-coerced", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "coerced")
	}},
	{"16-summary-newlines", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		if strings.ContainsAny(env.Summary, "\r\n") {
			t.Errorf("summary keeps a newline: %q", env.Summary)
		}
	}},
	{"17-occurred-at-invalid", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		has(t, w, "invalid_format")
		if env.OccurredAt != "" {
			t.Errorf("occurred_at stored as %q", env.OccurredAt)
		}
		if _, ok := env.Context["occurred_at_raw"]; !ok {
			t.Error("context.occurred_at_raw absent")
		}
	}},
	{"18-context-not-object", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		if _, ok := env.Payload["context_raw"]; !ok {
			t.Error("payload.context_raw absent")
		}
		if env.Context != nil {
			t.Errorf("context kept: %v", env.Context)
		}
	}},
	{"19-context-overflow", func(t *testing.T, env *Envelope, w []schema.Detail, _ error) {
		if len(env.Context) != contextEntries {
			t.Errorf("context has %d entries, want %d", len(env.Context), contextEntries)
		}
		if _, ok := env.Payload["context_overflow"].(map[string]any); !ok {
			t.Error("payload.context_overflow is not an object")
		}
	}},
}

func rejected(status int, code string) func(t *testing.T, env *Envelope, warnings []schema.Detail, err error) {
	return func(t *testing.T, env *Envelope, warnings []schema.Detail, err error) {
		var r *Rejection
		if !errors.As(err, &r) {
			t.Fatalf("err = %v, want a *Rejection", err)
		}
		if r.Status != status || r.Code != code {
			t.Errorf("rejection %d %s, want %d %s", r.Status, r.Code, status, code)
		}
		if env != nil || warnings != nil {
			t.Error("a rejection returned an envelope or warnings")
		}
		if r.Error() == "" {
			t.Error("empty error string")
		}
	}
}

func has(t *testing.T, warnings []schema.Detail, code string) {
	t.Helper()
	for _, d := range warnings {
		if d.Code == code {
			return
		}
	}
	t.Errorf("no %s warning", code)
}

func hasNot(t *testing.T, warnings []schema.Detail, code string) {
	t.Helper()
	for _, d := range warnings {
		if d.Code == code {
			t.Errorf("unexpected %s warning at %s", code, d.Pointer)
		}
	}
}

// fixtureBody returns the raw body of a fixture directory: body.json as is,
// or the body a body.gen.json describes (prefix, one pad byte repeated,
// suffix, total_bytes long).
func fixtureBody(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "body.json"))
	if err == nil {
		return raw
	}
	spec, err := os.ReadFile(filepath.Join(dir, "body.gen.json"))
	if err != nil {
		t.Fatalf("%s: neither body.json nor body.gen.json", dir)
	}
	var gen struct {
		Prefix     string `json:"prefix"`
		Pad        string `json:"pad"`
		Suffix     string `json:"suffix"`
		TotalBytes int    `json:"total_bytes"`
	}
	if err := json.Unmarshal(spec, &gen); err != nil {
		t.Fatal(err)
	}
	fill := gen.TotalBytes - len(gen.Prefix) - len(gen.Suffix)
	if fill < 0 || len(gen.Pad) != 1 {
		t.Fatalf("%s: bad body.gen.json", dir)
	}
	body := make([]byte, 0, gen.TotalBytes)
	body = append(body, gen.Prefix...)
	body = append(body, bytes.Repeat([]byte(gen.Pad), fill)...)
	return append(body, gen.Suffix...)
}

type expectation struct {
	Row      string `json:"row"`
	Status   int    `json:"status"`
	Error    string `json:"error"`
	Warnings []struct {
		Code    string `json:"code"`
		Pointer string `json:"pointer"`
	} `json:"warnings"`
}

// checkDecodeFixture runs one decode fixture through Decode and compares the
// status, the error code, the warning multiset and the stored bytes.
func checkDecodeFixture(t *testing.T, dir string) (*Envelope, []schema.Detail, error) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want expectation
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	env, warnings, err := Decode(fixtureBody(t, dir))
	if want.Status != 201 {
		var r *Rejection
		if !errors.As(err, &r) {
			t.Fatalf("expected %d %s, got no rejection", want.Status, want.Error)
		}
		if r.Status != want.Status || r.Code != want.Error {
			t.Fatalf("expected %d %s, got %d %s", want.Status, want.Error, r.Status, r.Code)
		}
		if _, err := os.Stat(filepath.Join(dir, "stored.json")); err == nil {
			t.Fatal("stored.json present on a rejected body")
		}
		return env, warnings, err
	}
	if err != nil {
		t.Fatalf("expected 201, got %v", err)
	}
	var got, expected []string
	for _, d := range warnings {
		got = append(got, d.Code+" "+d.Pointer)
	}
	for _, w := range want.Warnings {
		expected = append(expected, w.Code+" "+w.Pointer)
	}
	sort.Strings(got)
	sort.Strings(expected)
	if !slices.Equal(got, expected) {
		t.Errorf("warnings\n got %v\nwant %v", got, expected)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "stored.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mine := canonjson.Marshal(env.Tree()); !bytes.Equal(mine, stored) {
		t.Errorf("stored envelope\n got %s\nwant %s", mine, stored)
	}
	return env, warnings, err
}

func TestRows(t *testing.T) {
	t.Parallel()
	var listed []string
	for _, row := range rows {
		listed = append(listed, row.dir)
	}
	if onDisk := fixtureDirs(t, "decode/rows"); !slices.Equal(listed, onDisk) {
		t.Fatalf("row table %v differs from conformance/decode/rows %v", listed, onDisk)
	}
	for _, row := range rows {
		t.Run(row.dir, func(t *testing.T) {
			t.Parallel()
			env, warnings, err := checkDecodeFixture(t, filepath.Join(conformanceDir, "decode/rows", row.dir))
			row.check(t, env, warnings, err)
		})
	}
}

func TestInteractions(t *testing.T) {
	t.Parallel()
	for _, name := range fixtureDirs(t, "decode/interactions") {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkDecodeFixture(t, filepath.Join(conformanceDir, "decode/interactions", name))
		})
	}
}

// hashMembers are the identity members, the projection of the stored
// envelope that the hash fixtures' canonical.json holds. The digest itself is
// the identity package's; the projection is a free check of normalisation.
var hashMembers = []string{"kind", "schema_version", "machine", "model", "harness", "project", "summary", "payload"}

func TestHashFixturesProjection(t *testing.T) {
	t.Parallel()
	for _, name := range fixtureDirs(t, "hash") {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(conformanceDir, "hash", name)
			env, _, err := Decode(fixtureBody(t, dir))
			if err != nil {
				t.Fatal(err)
			}
			tree := env.Tree()
			input := map[string]any{}
			for _, name := range hashMembers {
				if v, ok := tree[name]; ok {
					input[name] = v
				}
			}
			want, err := os.ReadFile(filepath.Join(dir, "canonical.json"))
			if err != nil {
				t.Fatal(err)
			}
			if got := canonjson.Marshal(input); !bytes.Equal(got, want) {
				t.Errorf("identity input\n got %s\nwant %s", got, want)
			}
		})
	}
}

// fixtureDirs returns the fixture directories under sub, sorted, and checks
// them against conformance/manifest.json.
func fixtureDirs(t *testing.T, sub string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(conformanceDir, sub))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	listed := manifest(t)[sub]
	sort.Strings(listed)
	if !slices.Equal(names, listed) {
		t.Fatalf("conformance/%s on disk %v, manifest %v", sub, names, listed)
	}
	if len(names) == 0 {
		t.Fatalf("no fixtures under %s", sub)
	}
	return names
}

func manifest(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(conformanceDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		MaxDepth           int      `json:"max_depth"`
		Rows               []string `json:"decode/rows"`
		Interactions       []string `json:"decode/interactions"`
		Hash               []string `json:"hash"`
		unexportedSentinel struct{}
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.MaxDepth != MaxDepth {
		t.Fatalf("manifest max_depth %d, MaxDepth %d", m.MaxDepth, MaxDepth)
	}
	return map[string][]string{"decode/rows": m.Rows, "decode/interactions": m.Interactions, "hash": m.Hash}
}

// TestConstantsAgreeWithContract pins the constants the decoder cannot read
// from the compiled schema to the contract files.
func TestConstantsAgreeWithContract(t *testing.T) {
	t.Parallel()
	manifest(t) // checks MaxDepth
	doc, ok := schema.Document(schema.EnvelopeKind, 1)
	if !ok {
		t.Fatal("no envelope document")
	}
	var envelope struct {
		Properties struct {
			SchemaVersion struct {
				Maximum json.Number `json:"maximum"`
			} `json:"schema_version"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(doc, &envelope); err != nil {
		t.Fatal(err)
	}
	if got := envelope.Properties.SchemaVersion.Maximum; string(got) != "9007199254740991" || uint64(schemaVersionMax) != 9007199254740991 {
		t.Errorf("schema maximum %s, decoder %d", got, uint64(schemaVersionMax))
	}
	readme, err := os.ReadFile(filepath.Join(conformanceDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(readme, []byte("more than 10485760 bytes")) || BodyLimit != 10485760 {
		t.Errorf("BodyLimit %d is not the README's limit", BodyLimit)
	}
}

func TestMembersIsAClone(t *testing.T) {
	got := Members()
	if !slices.Equal(got, envelopeMembers) {
		t.Fatalf("Members() = %v", got)
	}
	got[0] = "changed"
	if envelopeMembers[0] != "kind" {
		t.Error("Members() shares its backing array")
	}
}
