package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/internal/store"
	"github.com/agentfeedback/agentfeedback/pkg/canonjson"
)

const conformanceDir = "../../conformance"

// testClock is an adjustable clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestService(t *testing.T) (*Service, *store.DB, *testClock) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := &testClock{t: time.Date(2026, 9, 27, 10, 0, 1, 123456789, time.UTC)}
	return New(db, Config{Version: "4.0.0", Features: Features}, WithClock(clock.now)), db, clock
}

func mustCreate(t *testing.T, s *Service, body string) CreateResult {
	t.Helper()
	res, err := s.Create(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("create %s: %v", body, err)
	}
	return res
}

func asProblem(t *testing.T, err error) *Problem {
	t.Helper()
	var p *Problem
	if !errors.As(err, &p) {
		t.Fatalf("err = %v, want a *Problem", err)
	}
	return p
}

// fixtureBody returns a fixture's raw body: body.json, or the body
// body.gen.json describes.
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

// fixtureDirs returns the fixture directories under conformance/sub, sorted.
func fixtureDirs(t *testing.T, sub string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(conformanceDir, sub))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(conformanceDir, sub, e.Name()))
		}
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		t.Fatalf("no fixtures under %s", sub)
	}
	return dirs
}

// decodeFixtures lists every decode fixture, rows and interactions.
func decodeFixtures(t *testing.T) []string {
	return append(fixtureDirs(t, "decode/rows"), fixtureDirs(t, "decode/interactions")...)
}

var storedEnvelopeMembers = []string{"kind", "schema_version", "key", "summary", "machine", "model",
	"harness", "project", "occurred_at", "context", "payload"}

// storedEnvelope returns the record's envelope members as canonical JSON,
// parsed from the record's JSON form with number spellings kept.
func storedEnvelope(t *testing.T, r Record) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(r.AppendJSON(nil)))
	dec.UseNumber()
	var all map[string]any
	if err := dec.Decode(&all); err != nil {
		t.Fatal(err)
	}
	env := map[string]any{}
	for _, name := range storedEnvelopeMembers {
		if v, ok := all[name]; ok {
			env[name] = v
		}
	}
	return canonjson.Marshal(env)
}

// exportLines runs an export and splits it into header, record lines and trailer.
func exportLines(t *testing.T, s *Service, p ExportParams) (string, []string, string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := s.Export(context.Background(), p, &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("export has %d lines", len(lines))
	}
	return lines[0], lines[1 : len(lines)-1], lines[len(lines)-1], buf.Bytes()
}

func rowCount(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var n int64
	err := db.Read(context.Background(), func(q store.Querier) error {
		var err error
		n, err = store.CountAll(context.Background(), q)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func ptr[T any](v T) *T { return &v }
