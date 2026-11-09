package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

type identityFixtureAction struct {
	Op   string          `json:"op"`
	At   string          `json:"at"`
	ID   int64           `json:"id"`
	Body json.RawMessage `json:"body"`
}

type identityFixtureDetail struct {
	Code       string `json:"code"`
	Pointer    string `json:"pointer"`
	ExistingID int64  `json:"existing_id"`
}

type identityFixtureResult struct {
	Status    int                     `json:"status"`
	ID        int64                   `json:"id"`
	Stored    string                  `json:"stored"`
	Canonical string                  `json:"canonical"`
	SHA256    string                  `json:"sha256"`
	Record    string                  `json:"record"`
	Error     string                  `json:"error"`
	Warnings  []identityFixtureDetail `json:"warnings"`
	Details   []identityFixtureDetail `json:"details"`
}

func identityFixtureRead(t *testing.T, dir, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func identityFixtureJSON(t *testing.T, dir, name string, dst any) {
	t.Helper()
	if err := json.Unmarshal(identityFixtureRead(t, dir, name), dst); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func identityFixtureRecord(t *testing.T, dir, name string, record Record) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(record.AppendJSON(nil)))
	dec.UseNumber()
	var tree map[string]any
	if err := dec.Decode(&tree); err != nil {
		t.Fatal(err)
	}
	got := canonjson.Marshal(tree)
	want := identityFixtureRead(t, dir, name)
	if !bytes.Equal(got, want) {
		t.Errorf("record %s\n got %s\nwant %s", name, got, want)
	}
}

func identityFixtureWarnings(t *testing.T, got []schema.Detail, want []identityFixtureDetail) {
	t.Helper()
	actual, expected := make([]string, len(got)), make([]string, len(want))
	for i, d := range got {
		actual[i] = d.Code + "\x00" + d.Pointer
	}
	for i, d := range want {
		expected[i] = d.Code + "\x00" + d.Pointer
	}
	sort.Strings(actual)
	sort.Strings(expected)
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("warning code/pointer multiset %q, want %q", actual, expected)
	}
}

// TestIdentityFixtures consumes every scenario through the real transactional
// service. Full record snapshots make replay mutation and hash recomputation on
// tombstones observable, including fields excluded from the identity projection.
func TestIdentityFixtures(t *testing.T) {
	t.Parallel()
	for _, dir := range fixtureDirs(t, "identity") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			t.Parallel()
			var scenario struct {
				UIDs    []string                `json:"uids"`
				Actions []identityFixtureAction `json:"actions"`
			}
			var expected struct {
				Results []identityFixtureResult `json:"results"`
				Records []string                `json:"records"`
				Rows    int64                   `json:"rows"`
			}
			identityFixtureJSON(t, dir, "scenario.json", &scenario)
			identityFixtureJSON(t, dir, "expected.json", &expected)
			if len(scenario.Actions) == 0 || len(scenario.Actions) != len(expected.Results) {
				t.Fatal("scenario must have one expected result per action")
			}
			_, db, clock := newTestService(t)
			uidIndex := 0
			s := New(db, Config{Version: "4.0.0", Features: Features}, WithClock(clock.now),
				WithUIDGenerator(func() (string, error) {
					if uidIndex >= len(scenario.UIDs) {
						return "", fmt.Errorf("identity fixture exhausted its UID sequence")
					}
					uid := scenario.UIDs[uidIndex]
					uidIndex++
					return uid, nil
				}))
			ctx := context.Background()
			for i, action := range scenario.Actions {
				want := expected.Results[i]
				at, err := time.Parse(time.RFC3339Nano, action.At)
				if err != nil {
					t.Fatalf("action %d clock: %v", i+1, err)
				}
				clock.advance(at.Sub(clock.now()))
				var record Record
				status := 200
				switch action.Op {
				case "create":
					var filename string
					if err := json.Unmarshal(action.Body, &filename); err != nil {
						t.Fatal(err)
					}
					body := identityFixtureRead(t, dir, filename)
					env, warnings, err := envelope.Decode(body)
					if err != nil {
						t.Fatalf("action %d decode: %v", i+1, err)
					}
					if got := canonjson.Marshal(env.Tree()); !bytes.Equal(got, identityFixtureRead(t, dir, want.Stored)) {
						t.Errorf("action %d normalized envelope differs from %s", i+1, want.Stored)
					}
					if got := canonjson.Marshal(env.IdentityTree()); !bytes.Equal(got, identityFixtureRead(t, dir, want.Canonical)) {
						t.Errorf("action %d canonical identity differs from %s", i+1, want.Canonical)
					}
					if got := env.ContentHash(); got != want.SHA256 {
						t.Errorf("action %d identity hash %s, want %s", i+1, got, want.SHA256)
					}
					identityFixtureWarnings(t, warnings, want.Warnings)
					result, err := s.Create(ctx, body)
					if want.Error != "" {
						p := asProblem(t, err)
						if p.Status != want.Status || p.Code != want.Error || len(p.Details) != len(want.Details) {
							t.Fatalf("action %d problem %+v, want %+v", i+1, p, want)
						}
						for j, detail := range p.Details {
							got := identityFixtureDetail{Code: detail.Code, Pointer: detail.Pointer, ExistingID: detail.ExistingID}
							if got != want.Details[j] || detail.ExistingID != want.ID || detail.Message == "" {
								t.Errorf("action %d conflict detail %+v, want %+v", i+1, detail, want.Details[j])
							}
						}
						continue
					}
					if err != nil {
						t.Fatalf("action %d create: %v", i+1, err)
					}
					if result.Created {
						status = 201
					}
					identityFixtureWarnings(t, result.Warnings, want.Warnings)
					record = result.Record
				case "mark":
					record, err = s.Mark(ctx, action.ID, action.Body)
				case "redact":
					record, err = s.Redact(ctx, action.ID)
				default:
					t.Fatalf("action %d unknown op %q", i+1, action.Op)
				}
				if err != nil {
					t.Fatalf("action %d %s: %v", i+1, action.Op, err)
				}
				if status != want.Status || record.ID != want.ID {
					t.Errorf("action %d status/id %d/%d, want %d/%d", i+1, status, record.ID, want.Status, want.ID)
				}
				identityFixtureRecord(t, dir, want.Record, record)
				stored, err := s.Get(ctx, want.ID)
				if err != nil {
					t.Fatal(err)
				}
				identityFixtureRecord(t, dir, want.Record, stored)
			}
			if uidIndex != len(scenario.UIDs) {
				t.Errorf("used %d UIDs, want %d", uidIndex, len(scenario.UIDs))
			}
			if got := rowCount(t, db); got != expected.Rows {
				t.Errorf("stored %d rows, want %d", got, expected.Rows)
			}
			if int64(len(expected.Records)) != expected.Rows {
				t.Fatal("final snapshots must cover every row")
			}
			seen := make(map[int64]bool)
			for _, filename := range expected.Records {
				var snapshot struct {
					ID int64 `json:"id"`
				}
				identityFixtureJSON(t, dir, filename, &snapshot)
				if seen[snapshot.ID] {
					t.Fatalf("duplicate final snapshot ID %d", snapshot.ID)
				}
				seen[snapshot.ID] = true
				record, err := s.Get(ctx, snapshot.ID)
				if err != nil {
					t.Fatal(err)
				}
				identityFixtureRecord(t, dir, filename, record)
			}
		})
	}
}
