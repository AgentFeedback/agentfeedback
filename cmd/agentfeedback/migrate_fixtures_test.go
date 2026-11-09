package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestMigrateConformanceFixtures(t *testing.T) {
	var fixture struct {
		Seed  []json.RawMessage `json:"seed"`
		Cases []struct {
			Name                 string         `json:"name"`
			Args                 []string       `json:"args"`
			Cap                  string         `json:"one_record_cap"`
			FailImport           int            `json:"fail_import"`
			WithoutImport        bool           `json:"without_import"`
			Code                 int            `json:"code"`
			Outcome              map[string]any `json:"outcome"`
			RedactAfterMigration []int          `json:"redact_after_migration"`
			ErrorContains        string         `json:"error_contains"`
			TargetSummaries      []string       `json:"target_summaries"`
			ImportRequests       *int           `json:"import_requests"`
			SeedOrder            []int          `json:"seed_order"`
			ExportRequests       *int           `json:"export_requests"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile("../../conformance/import/client-migration/scenario.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			isolate(t)
			src, dst := migratePair(t)
			seeds := fixture.Seed
			if tc.SeedOrder != nil {
				seeds = make([]json.RawMessage, 0, len(tc.SeedOrder))
				for _, index := range tc.SeedOrder {
					seeds = append(seeds, fixture.Seed[index])
				}
			}
			uids := make([]string, 0, len(fixture.Seed))
			for _, body := range seeds {
				_, uid := src.seed(t, string(body))
				uids = append(uids, uid)
			}
			if len(tc.RedactAfterMigration) > 0 {
				if result := migrate(t, dst); result.code != 0 {
					t.Fatalf("setup migration: %+v", result)
				}
				for _, id := range tc.RedactAfterMigration {
					status, body := src.call(t, http.MethodDelete, "/api/v1/submissions/"+strconv.Itoa(id), "")
					if status != http.StatusOK {
						t.Fatalf("redaction %d: %d %s", id, status, body)
					}
				}
			}
			lines, _ := exported(t, src)
			longest := 0
			for _, line := range lines {
				longest = max(longest, len(line)+1)
			}
			capBytes := len(importHeader) + longest + trailerLen(1)
			oldCap, oldTransport := migrateChunkMax, migrateTransport
			t.Cleanup(func() { migrateChunkMax, migrateTransport = oldCap, oldTransport })
			if tc.Cap == "client" {
				migrateChunkMax = capBytes
			}
			attempts := 0
			migrateTransport = func(rt http.RoundTripper) http.RoundTripper {
				return roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Host != strings.TrimPrefix(dst.srv.URL, "http://") {
						return rt.RoundTrip(req)
					}
					if req.Method == http.MethodPost && req.URL.Path == "/api/v1/import" {
						attempts++
						if tc.FailImport == attempts {
							return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Request: req, Body: io.NopCloser(strings.NewReader(`{"error":"bad_request","message":"fixture rejects chunk"}`))}, nil
						}
					}
					resp, err := rt.RoundTrip(req)
					if err != nil || req.URL.Path != "/api/v1/meta" || (tc.Cap != "target" && !tc.WithoutImport) {
						return resp, err
					}
					var meta map[string]any
					err = json.NewDecoder(resp.Body).Decode(&meta)
					_ = resp.Body.Close()
					if err != nil {
						return nil, err
					}
					if tc.Cap == "target" {
						meta["limits"].(map[string]any)["import_bytes"] = capBytes
					}
					if tc.WithoutImport {
						meta["features"] = []string{}
					}
					body, err := json.Marshal(meta)
					if err != nil {
						return nil, err
					}
					resp.Body = io.NopCloser(strings.NewReader(string(body)))
					resp.ContentLength = int64(len(body))
					resp.Header.Del("Content-Length")
					return resp, nil
				})
			}
			initialRequests := len(dst.requests())
			initialSourceRequests := len(src.requests())
			result := migrate(t, dst, tc.Args...)
			if result.code != tc.Code {
				t.Fatalf("result %+v, want exit %d", result, tc.Code)
			}
			if tc.ErrorContains != "" && !strings.Contains(result.stderr, tc.ErrorContains) {
				t.Fatalf("stderr %q lacks %q", result.stderr, tc.ErrorContains)
			}
			if tc.Outcome != nil {
				actual := outcome(t, result)
				for key, want := range tc.Outcome {
					if !reflect.DeepEqual(actual[key], want) {
						t.Errorf("outcome %s = %v, want %v", key, actual[key], want)
					}
				}
			}
			if tc.ImportRequests != nil && attempts != *tc.ImportRequests {
				t.Errorf("import attempts %d, want %d", attempts, *tc.ImportRequests)
			}
			if tc.ExportRequests != nil {
				exports := 0
				for _, req := range src.requests()[initialSourceRequests:] {
					if req == "GET /api/v1/export" {
						exports++
					}
				}
				if exports != *tc.ExportRequests {
					t.Errorf("source export pages %d, want %d", exports, *tc.ExportRequests)
				}
			}
			records, onTarget := exported(t, dst)
			for _, id := range tc.RedactAfterMigration {
				uid := uids[id-1]
				record, ok := onTarget[uid]
				if !ok {
					t.Fatalf("target lacks tombstone UID %s", uid)
				}
				var targetID int64
				if err := json.Unmarshal(record["id"], &targetID); err != nil {
					t.Fatalf("target ID for UID %s: %v", uid, err)
				}
				want := "redact on target: uid=" + uid + " id=" + strconv.FormatInt(targetID, 10)
				if !strings.Contains(result.stdout, want) {
					t.Errorf("dry-run output %q lacks %q", result.stdout, want)
				}
			}
			summaries := make([]string, 0, len(records))
			for _, line := range records {
				var record struct {
					Summary string `json:"summary"`
				}
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				summaries = append(summaries, record.Summary)
			}
			if !reflect.DeepEqual(summaries, tc.TargetSummaries) {
				t.Errorf("target summaries %v, want %v", summaries, tc.TargetSummaries)
			}
			if len(tc.Args) > 0 && tc.Args[0] == "--dry-run" {
				for _, req := range dst.requests()[initialRequests:] {
					if !strings.HasPrefix(req, "GET ") {
						t.Errorf("dry-run wrote to target: %s", req)
					}
				}
			}
		})
	}
}
