package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type exportFixtureScenario struct {
	ExportedAt string `json:"exported_at"`
	Seed       []struct {
		At   string `json:"at"`
		UID  string `json:"uid"`
		Body string `json:"body"`
	} `json:"seed"`
	Mutations []struct {
		Op   string `json:"op"`
		ID   int64  `json:"id"`
		At   string `json:"at"`
		Body string `json:"body"`
	} `json:"mutations"`
	Params struct {
		Kind    string  `json:"kind"`
		Since   *string `json:"since"`
		AfterID *int64  `json:"after_id"`
		Limit   *int    `json:"limit"`
	} `json:"params"`
}

type exportFixtureExpected struct {
	Status  int    `json:"status"`
	Error   string `json:"error"`
	Details []struct {
		Code    string `json:"code"`
		Pointer string `json:"pointer"`
	} `json:"details"`
	IDs    []int64 `json:"ids"`
	SHA256 string  `json:"sha256"`
}

func exportFixtureTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("fixture time %q: %v", value, err)
	}
	return parsed
}

func exportFixtureJSON(t *testing.T, path string, dst any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// TestExportFixtures consumes every public export scenario through real create,
// processing, redaction and export operations; no store rows are fabricated.
func TestExportFixtures(t *testing.T) {
	for _, dir := range fixtureDirs(t, "export") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var scenario exportFixtureScenario
			var expected exportFixtureExpected
			exportFixtureJSON(t, filepath.Join(dir, "scenario.json"), &scenario)
			exportFixtureJSON(t, filepath.Join(dir, "expected.json"), &expected)
			want, err := os.ReadFile(filepath.Join(dir, "expected.ndjson"))
			if err != nil {
				t.Fatal(err)
			}
			_, db, _ := newTestService(t)
			now := exportFixtureTime(t, scenario.ExportedAt)
			uidIndex := 0
			s := New(db, Config{}, WithClock(func() time.Time { return now }), WithUIDGenerator(func() (string, error) {
				if uidIndex >= len(scenario.Seed) {
					return "", fmt.Errorf("fixture UID generator exhausted")
				}
				uid := scenario.Seed[uidIndex].UID
				uidIndex++
				return uid, nil
			}))
			ctx := context.Background()
			for i, seed := range scenario.Seed {
				now = exportFixtureTime(t, seed.At)
				result, err := s.Create(ctx, []byte(seed.Body))
				if err != nil {
					t.Fatalf("seed %d: %v", i+1, err)
				}
				if !result.Created || result.Record.ID != int64(i+1) || result.Record.UID != seed.UID {
					t.Fatalf("seed %d did not create its deterministic ID/UID: %+v", i+1, result)
				}
			}
			if uidIndex != len(scenario.Seed) {
				t.Fatalf("consumed %d UIDs for %d seeds", uidIndex, len(scenario.Seed))
			}
			for _, mutation := range scenario.Mutations {
				now = exportFixtureTime(t, mutation.At)
				switch mutation.Op {
				case "mark":
					_, err = s.Mark(ctx, mutation.ID, []byte(mutation.Body))
				case "redact":
					_, err = s.Redact(ctx, mutation.ID)
				default:
					t.Fatalf("unknown mutation %q", mutation.Op)
				}
				if err != nil {
					t.Fatalf("%s %d: %v", mutation.Op, mutation.ID, err)
				}
			}
			p := ExportParams{Kind: scenario.Params.Kind, AfterID: scenario.Params.AfterID, Limit: scenario.Params.Limit}
			if scenario.Params.Since != nil {
				since := exportFixtureTime(t, *scenario.Params.Since)
				p.Since = &since
			}
			now = exportFixtureTime(t, scenario.ExportedAt)
			var got bytes.Buffer
			err = s.Export(ctx, p, &got)
			if expected.Status != 200 {
				if err == nil {
					t.Fatal("invalid export succeeded")
				}
				problem := asProblem(t, err)
				if problem.Status != expected.Status || problem.Code != expected.Error || len(problem.Details) != len(expected.Details) {
					t.Fatalf("problem = %+v, expected %+v", problem, expected)
				}
				for i, detail := range expected.Details {
					if problem.Details[i].Code != detail.Code || problem.Details[i].Pointer != detail.Pointer {
						t.Errorf("detail %d = %+v, expected %+v", i, problem.Details[i], detail)
					}
				}
				if got.Len() != 0 || len(want) != 0 {
					t.Fatal("invalid export must write no bytes")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("export differs from expected.ndjson\n got: %s\nwant: %s", got.Bytes(), want)
			}
			if !bytes.HasSuffix(got.Bytes(), []byte{'\n'}) {
				t.Fatal("export has no final LF")
			}
			lines := bytes.Split(got.Bytes()[:got.Len()-1], []byte{'\n'})
			if len(lines) < 2 {
				t.Fatal("export lacks header/trailer")
			}
			ids := make([]int64, 0, len(lines)-2)
			digest := sha256.New()
			for _, line := range lines[1 : len(lines)-1] {
				var record struct {
					ID int64 `json:"id"`
				}
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, record.ID)
				digest.Write(line)
				digest.Write([]byte{'\n'})
			}
			if !reflect.DeepEqual(ids, expected.IDs) {
				t.Errorf("record IDs = %v, expected %v", ids, expected.IDs)
			}
			if sum := hex.EncodeToString(digest.Sum(nil)); sum != expected.SHA256 {
				t.Errorf("record-line digest = %s, expected %s", sum, expected.SHA256)
			}
		})
	}
}
