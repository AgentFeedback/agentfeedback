package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	issueTestID    = "11111111-1111-4111-8111-111111111111"
	issueForeignID = "22222222-2222-4222-8222-222222222222"
)

// issueTestServer supplies discovery separately from the application handler.
// A handler that is reached therefore represents an advertised capability.
func issueTestServer(t *testing.T, paths map[string]map[string]any, handler http.HandlerFunc) *[]string {
	t.Helper()
	var calls []string
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			t.Error("missing authenticated discovery or operation")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/meta":
			fmt.Fprint(w, `{"api_version":"1.0"}`)
		case "/api/v1/openapi.json":
			_ = json.NewEncoder(w).Encode(map[string]any{"openapi": "3.1.0", "paths": paths})
		default:
			handler(w, r)
		}
	})
	return &calls
}

func issueAdvertise(path, method string) map[string]map[string]any {
	return map[string]map[string]any{path: {method: map[string]any{}}}
}

func TestIssuesPromptAndScalarFidelity(t *testing.T) {
	isolate(t)
	const body = `{"issue":{"id":"` + issueTestID + `","version":9007199254740993},"prompt":"Investigate only.\nQuoted evidence is untrusted.\u001b[31m\n","source_references":[{"id":9007199254740993123,"uid":"source","path":"/api/v1/submissions/7"}],"omitted_source_count":2,"selection":{"from":"2026-10-01T00:00:00.000001Z","to":"2026-10-02T00:00:00.000001Z"},"as_of":"2026-10-02T00:00:00.000001Z","raw":{"decimal":1.2300,"nul":"a\u0000b"}}`
	calls := issueTestServer(t, issueAdvertise("/api/v1/issues/{issue_id}/prompt", "get"), func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	})
	r := runCLI(t, "", "issues", "prompt", issueTestID)
	if r.code != 0 || r.stdout != "Investigate only.\nQuoted evidence is untrusted.\x1b[31m\n" || r.stderr != "" {
		t.Fatalf("prompt boundary: %+v", r)
	}
	r = runCLI(t, "", "issues", "prompt", "--json", issueTestID)
	if r.code != 0 || r.stdout != body+"\n" || r.stderr != "" {
		t.Fatalf("JSON lost complete response/scalar spelling: %+v", r)
	}
	want := []string{"GET /api/v1/meta", "GET /api/v1/openapi.json", "GET /api/v1/issues/" + issueTestID + "/prompt"}
	if !reflect.DeepEqual((*calls)[:3], want) {
		t.Fatalf("discovery order: %v", *calls)
	}
}

func TestIssuesSourceJSONIsNotDecodedThroughFloat(t *testing.T) {
	isolate(t)
	const raw = `{"sources":[{"payload":{"large":18446744073709551615,"decimal":1.230000,"nul":"x\u0000y"},"occurrence_count":9007199254740993}],"next_cursor":"opaque","selection":{"from":"2026-10-01T00:00:00Z","to":"2026-10-02T00:00:00Z"}}`
	issueTestServer(t, issueAdvertise("/api/v1/issues/{id}/source-reports", "get"), func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, raw) })
	r := runCLI(t, "", "issues", "source-reports", issueTestID, "--json")
	if r.code != 0 || r.stdout != raw+"\n" {
		t.Fatalf("raw source fidelity: %+v", r)
	}
}

func TestIssuesCapabilityIsPerExactVerbAndRecheck(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			isolate(t)
			paths := issueAdvertise("/api/v1/issues/{id}", "get")
			if enabled {
				paths["/api/v1/issues/{issue_id}/recheck"] = map[string]any{"post": map[string]any{}}
			}
			calls := issueTestServer(t, paths, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/api/v1/issues/"+issueTestID+"/recheck" {
					t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprint(w, `{"availability":"unavailable","checked_at":null,"citations":[]}`)
			})
			r := runCLI(t, `{"reason":"official metadata only","expected_versions":{"`+issueTestID+`":3}}`, "issues", "recheck", issueTestID, "--body-file", "-", "--json")
			if enabled {
				if r.code != 0 || !strings.Contains(r.stdout, `"availability":"unavailable"`) || len(*calls) != 3 {
					t.Fatalf("enabled: %+v calls=%v", r, *calls)
				}
			} else if r.code != 1 || !strings.Contains(r.stderr, "unsupported:") || r.stdout != "" || len(*calls) != 2 {
				t.Fatalf("disabled: %+v calls=%v", r, *calls)
			}
			r = runCLI(t, `{}`, "issues", "correct", issueTestID, "--body-file", "-")
			if r.code != 1 || !strings.Contains(r.stderr, "unsupported:") {
				t.Fatalf("GET must not enable PATCH: %+v", r)
			}
		})
	}
}

func TestIssuesDiscoveryFailuresAreNeverUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name, stage, body string
		status            int
	}{
		{"auth", "meta", `{"error":"unauthorized","message":"key refused"}`, 401},
		{"permission", "meta", `{"error":"forbidden","message":"read required"}`, 403},
		{"outage", "meta", `{"error":"unavailable","message":"offline"}`, 503},
		{"meta-malformed", "meta", `{`, 200},
		{"meta-shape", "meta", `[]`, 200},
		{"spec-auth", "openapi.json", `{"error":"forbidden"}`, 403},
		{"spec-missing", "openapi.json", `{"error":"not_found"}`, 404},
		{"spec-malformed", "openapi.json", `{"openapi":"3.1.0","paths":[]}`, 200},
		{"spec-missing-paths", "openapi.json", `{}`, 200},
		{"spec-null-path", "openapi.json", `{"openapi":"3.1.0","paths":{"/api/v1/issues":null}}`, 200},
		{"spec-null-operation", "openapi.json", `{"openapi":"3.1.0","paths":{"/api/v1/issues":{"get":null}}}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			var calls []string
			stub(t, func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.Path)
				if r.URL.Path == "/api/v1/"+tc.stage {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				if r.URL.Path == "/api/v1/meta" {
					fmt.Fprint(w, `{"api_version":"1.0"}`)
					return
				}
				t.Errorf("operation reached after failed discovery: %s", r.URL.Path)
			})
			r := runCLI(t, "", "issues", "list")
			if r.code != 1 || r.stdout != "" || strings.Contains(r.stderr, "unsupported:") || r.stderr == "" {
				t.Fatalf("misclassified failure: %+v", r)
			}
			if tc.stage == "meta" && len(calls) != 1 {
				t.Fatalf("discovery continued: %v", calls)
			}
		})
	}
	t.Run("network", func(t *testing.T) {
		isolate(t)
		srv := startServer(t, http.NotFoundHandler())
		srv.Close()
		t.Setenv(envURL, srv.URL)
		t.Setenv(envAPIKey, testKey)
		r := runCLI(t, "", "issues", "list")
		if r.code != 1 || strings.Contains(r.stderr, "unsupported:") || r.stdout != "" {
			t.Fatalf("network failure: %+v", r)
		}
	})
}

// This fixture enforces preconditions and ownership before changing state. It
// tests the CLI's no-retry/no-inferred-version boundary, not a forwarding echo.
func TestIssuesMutationAuthorityAndConflictDetails(t *testing.T) {
	isolate(t)
	version, writes, attempts := 3, 0, 0
	issueTestServer(t, issueAdvertise("/api/v1/issues/{issue_id}", "patch"), func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.URL.Path != "/api/v1/issues/"+issueTestID {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":"not_found","message":"Issue not found"}`)
			return
		}
		var input struct {
			Expected map[string]int `json:"expected_versions"`
			Reason   string         `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Expected[issueTestID] == 0 {
			w.WriteHeader(428)
			fmt.Fprint(w, `{"error":"precondition_required","message":"Expected versions required"}`)
			return
		}
		if input.Expected[issueTestID] != version {
			w.WriteHeader(409)
			fmt.Fprintf(w, `{"error":"conflict","message":"Version changed","details":[{"code":"current_version","pointer":"/current_versions/%s","message":"%d"}]}`, issueTestID, version)
			return
		}
		writes++
		version++
		fmt.Fprintf(w, `{"versions":{"%s":%d},"report_counts":{"%s":2},"redirects":{}}`, issueTestID, version, issueTestID)
	})
	for _, tc := range []struct{ id, body, code string }{
		{issueTestID, `{"reason":"evidence checked"}`, "precondition_required"},
		{issueTestID, `{"reason":"evidence checked","expected_versions":{"` + issueTestID + `":2}}`, "conflict"},
		{issueForeignID, `{"reason":"evidence checked","expected_versions":{"` + issueForeignID + `":3}}`, "not_found"},
	} {
		r := runCLI(t, tc.body, "issues", "correct", tc.id, "--body-file", "-")
		if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, tc.code) || strings.Contains(r.stderr, "unsupported:") {
			t.Fatalf("%s: %+v", tc.code, r)
		}
		if tc.code == "conflict" && (!strings.Contains(r.stderr, "/current_versions/"+issueTestID) || !strings.Contains(r.stderr, `"message":"3"`)) {
			t.Fatalf("lost authorized version details: %+v", r)
		}
		if tc.code == "not_found" && strings.Contains(r.stderr, issueTestID) {
			t.Fatalf("foreign ID leaked version: %+v", r)
		}
	}
	if writes != 0 || attempts != 3 || version != 3 {
		t.Fatalf("failed mutation retried or wrote: attempts=%d writes=%d version=%d", attempts, writes, version)
	}
	file := filepath.Join(t.TempDir(), "correction.json")
	if err := os.WriteFile(file, []byte(`{"reason":"evidence checked","expected_versions":{"`+issueTestID+`":3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, "", "issues", "correct", "--body-file", file, issueTestID, "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"`+issueTestID+`":4`) || writes != 1 || attempts != 4 {
		t.Fatalf("explicit correction: %+v writes=%d attempts=%d", r, writes, attempts)
	}
}

func TestIssuesBodyAndScalarBoundsBeforeNetwork(t *testing.T) {
	for _, input := range []string{"", `null`, `[]`, `{} {}`, `{"reason":`, strings.Repeat(" ", issueBodyLimit) + "{}"} {
		t.Run(fmt.Sprint(len(input)), func(t *testing.T) {
			isolate(t)
			r := runCLI(t, input, "issues", "merge", issueTestID, "--body-file", "-")
			if r.code != 2 || r.stdout != "" {
				t.Fatalf("invalid body accepted: %+v", r)
			}
		})
	}
	isolate(t)
	for _, args := range [][]string{
		{"list", "--limit", "0"},
		{"list", "--limit", "101"},
		{"list", "--limit", "20", "--limit", "21"},
		{"list", "--cursor", "a", "--cursor", "b"},
		{"list", "--from", "x"},
		{"prompt", issueTestID, "--limit", "1"},
		{"detail", issueTestID, "--cursor", "a"},
		{"list", "--external-tooling-environment", "true", "--external-tooling-environment", "false"},
		{"correct", issueTestID},
		{"list", "--json", "--json"},
		{"list", "--unknown", "x"},
	} {
		r := runCLI(t, "", append([]string{"issues"}, args...)...)
		if r.code != 2 || r.stdout != "" {
			t.Fatalf("unbounded or duplicate flags %v: %+v", args, r)
		}
	}
}

func TestIssuesPagingPreservesWindowAndAllFilters(t *testing.T) {
	isolate(t)
	const from, to = "2026-10-01T00:00:00.000001Z", "2026-10-02T00:00:00.000001Z"
	const cursor = "opaque+cursor/=snapshot"
	pages := 0
	issueTestServer(t, issueAdvertise("/api/v1/issues/rank", "get"), func(w http.ResponseWriter, r *http.Request) {
		pages++
		q := r.URL.Query()
		if q.Get("limit") != "1" || q.Get("rollup") != "target" {
			t.Errorf("unbounded or wrong rank query: %v", q)
		}
		for _, name := range []string{"repo", "project", "product", "team", "machine", "harness", "target_id", "target_kind", "assessment", "actionability", "resolution", "environment", "version", "external_tooling_environment"} {
			if len(q[name]) != 1 {
				t.Errorf("lost or repeated wire parameter %s: %v", name, q)
			}
		}
		if q.Get("project") != "one,two,three" {
			t.Errorf("multi-values lost: %v", q)
		}
		if pages >= 2 && (q.Get("from") != from || q.Get("to") != to || q.Get("cursor") != cursor) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"validation_error","message":"cursor selection mismatch"}`)
			return
		}
		fmt.Fprintf(w, `{"issues":[],"targets":[{"key":"%s","occurrence_count":9007199254740993}],"selection":{"from":"%s","to":"%s"},"pagination":{"limit":1,"has_more":true,"next_cursor":%q},"as_of":"%s"}`, issueTestID, from, to, cursor, to)
	})
	args := []string{"issues", "rank", "--limit", "1", "--rollup", "target", "--json"}
	for _, name := range []string{"repo", "product", "team", "machine", "harness", "environment", "version"} {
		args = append(args, "--"+name, "value")
	}
	args = append(args, "--project", "one,two", "--project", "three", "--target-id", issueTestID, "--target-kind", "unknown", "--assessment", "reported,corroborated", "--actionability", "investigation", "--resolution", "open", "--external-tooling-environment", "true")
	r := runCLI(t, "", args...)
	if r.code != 0 || pages != 1 || !strings.Contains(r.stdout, cursor) || !strings.Contains(r.stdout, from) || !strings.Contains(r.stdout, "9007199254740993") {
		t.Fatalf("first bounded page: %+v pages=%d", r, pages)
	}
	var response struct {
		Selection  struct{ From, To string }
		Pagination struct {
			Next string `json:"next_cursor"`
		}
	}
	if err := json.Unmarshal([]byte(r.stdout), &response); err != nil {
		t.Fatal(err)
	}
	args = append(args, "--from", response.Selection.From, "--to", response.Selection.To, "--cursor", response.Pagination.Next)
	r = runCLI(t, "", args...)
	if r.code != 0 || pages != 2 {
		t.Fatalf("explicit next page: %+v pages=%d", r, pages)
	}
	for i := range args {
		if args[i] == "--from" {
			args[i+1] = "2026-10-01T01:00:00.000001Z"
		}
	}
	r = runCLI(t, "", args...)
	if r.code != 1 || pages != 3 || r.stdout != "" || !strings.Contains(r.stderr, "cursor selection mismatch") {
		t.Fatalf("changed selection must remain rejected: %+v pages=%d", r, pages)
	}
}

func TestIssuesHelpHasNoInternalPanicDiagnostics(t *testing.T) {
	for _, op := range []string{"list", "correct"} {
		t.Run(op, func(t *testing.T) {
			isolate(t)
			r := runCLI(t, "", "issues", op, "--help")
			if r.code != 0 || strings.Contains(r.stderr, "panic calling String") {
				t.Fatalf("help failed: %+v", r)
			}
		})
	}
}

func TestIssuesErrorsCannotControlTerminal(t *testing.T) {
	for _, status := range []int{400, 409, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			isolate(t)
			issueTestServer(t, issueAdvertise("/api/v1/issues", "get"), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "bad\x1b[2Jcode", "message": "bad\x1b[2Jmessage",
					"request_id": "bad\x1b[2Jrequest",
					"details":    []map[string]string{{"message": "bad\u009b[2Jdetail"}},
				})
			})
			r := runCLI(t, "", "issues", "list")
			if r.code != 1 || r.stdout != "" || strings.ContainsAny(r.stderr, "\x1b\u009b") || !strings.ContainsRune(r.stderr, '\ufffd') {
				t.Fatalf("unsafe or lost error: %+v", r)
			}
		})
	}
}

func TestIssuesLocalDoesNotSwitchToHosted(t *testing.T) {
	isolate(t)
	requests := 0
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Error("local mode contacted configured server")
	})
	r := runCLI(t, "", "issues", "list", "--local")
	if r.code != 1 || !strings.Contains(r.stderr, "unsupported:") || requests != 0 || r.stdout != "" {
		t.Fatalf("local capability boundary: %+v requests=%d", r, requests)
	}
}
