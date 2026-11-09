package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

type importFixtureStep struct {
	Operation string `json:"operation"`
	Input     string `json:"input"`
	Generator string `json:"generator"`
	DryRun    bool   `json:"dry_run"`
}

type importFixtureError struct {
	Status  int    `json:"status"`
	Error   string `json:"error"`
	Line    *int   `json:"line"`
	Code    string `json:"code"`
	Pointer string `json:"pointer"`
}

type importFixtureExpected struct {
	Result    map[string]any      `json:"result"`
	Error     *importFixtureError `json:"error"`
	CreatedID int64               `json:"created_id"`
	Records   []map[string]any    `json:"records"`
}

// TestImportFixtures consumes authored streams directly: no export of a source
// service and no reference-produced expected responses or stored records.
func TestImportFixtures(t *testing.T) {
	for _, dir := range fixtureDirs(t, "import") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var scenario struct {
				Mode  string              `json:"mode"`
				Steps []importFixtureStep `json:"steps"`
			}
			importFixtureReadJSON(t, filepath.Join(dir, "scenario.json"), &scenario)
			if scenario.Mode == "client-migration" {
				t.Skip("fixture consumed by cmd/agentfeedback migration tests")
			}
			var expected struct {
				Steps []importFixtureExpected `json:"steps"`
			}
			importFixtureReadJSON(t, filepath.Join(dir, "expected.json"), &expected)
			if len(scenario.Steps) == 0 || len(scenario.Steps) != len(expected.Steps) {
				t.Fatal("scenario and expected steps must have the same nonzero length")
			}
			s, _, _ := newTestService(t)
			uidCall := 99
			s.newUID = func() (string, error) {
				value := fmt.Sprintf("01925f00-0000-7000-8000-%012d", uidCall)
				uidCall++
				return value, nil
			}
			for index, step := range scenario.Steps {
				want := expected.Steps[index]
				t.Run(fmt.Sprintf("step-%02d-%s", index+1, step.Operation), func(t *testing.T) {
					var body []byte
					if step.Generator != "" {
						if step.Generator != "body.gen.json" {
							t.Fatal("generator must be body.gen.json")
						}
						body = fixtureBody(t, dir)
					} else {
						var err error
						body, err = os.ReadFile(filepath.Join(dir, step.Input))
						if err != nil {
							t.Fatal(err)
						}
					}
					if step.Operation == "create" {
						created, err := s.Create(context.Background(), body)
						if err != nil {
							t.Fatal(err)
						}
						if created.Record.ID != want.CreatedID {
							t.Fatalf("created ID %d, want %d", created.Record.ID, want.CreatedID)
						}
					} else {
						var result ImportResult
						var err error
						switch step.Operation {
						case "import":
							result, err = s.Import(context.Background(), body)
						case "restore":
							result, err = s.Restore(context.Background(), body, step.DryRun)
						default:
							t.Fatalf("unknown operation %q", step.Operation)
						}
						if want.Error != nil {
							p := asProblem(t, err)
							if p.Status != want.Error.Status || p.Code != want.Error.Error {
								t.Fatalf("problem %d %s, want %+v", p.Status, p.Code, want.Error)
							}
							if p.Status == 400 {
								if len(p.Details) != 1 || p.Details[0].Code != want.Error.Code || p.Details[0].Pointer != want.Error.Pointer {
									t.Fatalf("details %+v, want %+v", p.Details, want.Error)
								}
								if want.Error.Line != nil && !regexp.MustCompile(fmt.Sprintf(`\bline\s+%d\b`, *want.Error.Line)).MatchString(p.Message) {
									t.Fatalf("wrong error line: %+v", p)
								}
							}
						} else {
							if err != nil {
								t.Fatal(err)
							}
							raw, err := Marshal(result)
							if err != nil {
								t.Fatal(err)
							}
							var got map[string]any
							if err := json.Unmarshal(raw, &got); err != nil {
								t.Fatal(err)
							}
							for _, value := range []map[string]any{got, want.Result} {
								warnings, ok := value["warnings"].([]any)
								if !ok {
									t.Fatalf("warnings must be an array: %+v", value)
								}
								for _, warning := range warnings {
									delete(warning.(map[string]any), "message")
								}
								sort.Slice(warnings, func(i, j int) bool {
									a, b := warnings[i].(map[string]any), warnings[j].(map[string]any)
									return fmt.Sprintf("%v\x00%v\x00%v", a["line"], a["code"], a["pointer"]) < fmt.Sprintf("%v\x00%v\x00%v", b["line"], b["code"], b["pointer"])
								})
							}
							if !reflect.DeepEqual(got, want.Result) {
								t.Fatalf("result\n got %s\nwant %+v", raw, want.Result)
							}
						}
					}
					// Full snapshots pin retained fields, skipped-UID immutability,
					// validation atomicity and dry-run read-only behavior alike.
					_, lines, _, _ := exportLines(t, s, ExportParams{})
					gotRecords := make([]map[string]any, 0, len(lines))
					for _, line := range lines {
						var record map[string]any
						if err := json.Unmarshal([]byte(line), &record); err != nil {
							t.Fatal(err)
						}
						gotRecords = append(gotRecords, record)
					}
					if !reflect.DeepEqual(gotRecords, want.Records) {
						t.Fatalf("stored records\n got %+v\nwant %+v", gotRecords, want.Records)
					}
				})
			}
		})
	}
}

func importFixtureReadJSON(t *testing.T, path string, out any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatal(err)
	}
}
