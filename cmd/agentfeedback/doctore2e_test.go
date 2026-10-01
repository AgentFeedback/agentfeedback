package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// e2eLines parses every stdout line of doctor --e2e --json.
func e2eLines(t *testing.T, stdout string) []e2eStep {
	t.Helper()
	var steps []e2eStep
	for line := range strings.SplitSeq(strings.TrimRight(stdout, "\n"), "\n") {
		var s e2eStep
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		steps = append(steps, s)
	}

	return steps
}

func TestDoctorE2E_AgainstServer(t *testing.T) {
	_, cache := isolate(t)
	l := liveServer(t)

	r := runCLI(t, "", "doctor", "--e2e", "--json")
	if r.code != 0 {
		t.Fatalf("doctor --e2e: %+v", r)
	}
	steps := e2eLines(t, r.stdout)
	if len(steps) != 3 {
		t.Fatalf("want 3 lines, got %q", r.stdout)
	}
	for i, want := range []string{"submit", "list", "mark"} {
		if steps[i].Step != want || steps[i].Outcome != "ok" || steps[i].ID != steps[0].ID {
			t.Fatalf("line %d: %+v", i, steps[i])
		}
	}
	if steps[0].Key == "" || steps[2].Verdict != "install-check" {
		t.Fatalf("steps %+v", steps)
	}

	rec := l.record(t, steps[0].ID)
	if rec["kind"] != "install-check" || rec["key"] != steps[0].Key || rec["verdict"] != "install-check" || rec["processed_at"] == nil {
		t.Fatalf("record %v", rec)
	}
	ctx, _ := rec["context"].(map[string]any)
	for k := range ctx {
		if k != "os" && k != "arch" && k != "client" {
			t.Fatalf("context carries %q: %v", k, ctx)
		}
	}
	if _, err := os.Stat(filepath.Join(cache, "spool")); !os.IsNotExist(err) {
		t.Fatalf("the check spooled: %v", err)
	}

	// The row is out of the default views.
	stats := func(args ...string) float64 {
		t.Helper()
		r := runCLI(t, "", append([]string{"stats", "--json"}, args...)...)
		var s struct {
			Total float64 `json:"total"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &s) != nil {
			t.Fatalf("stats %v: %+v", args, r)
		}

		return s.Total
	}
	if n := stats(); n != 0 {
		t.Fatalf("default stats count %v install-check rows", n)
	}
	if n := stats("--include-kind", "install-check"); n != 1 {
		t.Fatalf("stats --include-kind install-check: total %v", n)
	}
	// digest pulls only open rows; the check marks its row processed, so it
	// is absent with and without --include-kind.
	for _, args := range [][]string{nil, {"--include-kind", "install-check"}} {
		out := filepath.Join(t.TempDir(), "d")
		r := runCLI(t, "", append([]string{"digest", "--out", out}, args...)...)
		if r.code != 0 || !strings.Contains(r.stderr, "pulled 0 row(s) (server total 0)") {
			t.Fatalf("digest %v: %+v", args, r)
		}
	}
}

func TestDoctorE2E_Human(t *testing.T) {
	isolate(t)
	liveServer(t)
	r := runCLI(t, "", "doctor", "--e2e")
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if r.code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "submit:   ok (id 1, key ") ||
		lines[1] != "list:     ok (id 1)" || lines[2] != "mark:     ok (id 1, verdict install-check)" {
		t.Fatalf("doctor --e2e: %+v", r)
	}
}

func TestDoctorE2E_StopsAtTheFailingStep(t *testing.T) {
	for _, tc := range []struct {
		name, failOn string
		status       int
		want         int
	}{
		{"list 500", "GET", http.StatusInternalServerError, 2},
		{"mark 401", "POST /api/v1/submissions/processed", http.StatusUnauthorized, 3},
		{"submit 401", "POST /api/v1/submissions", http.StatusUnauthorized, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cache := isolate(t)
			stub(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method+" "+r.URL.Path == tc.failOn || r.Method == tc.failOn {
					w.Header().Set("X-Request-Id", "req-1")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"code":"x","message":"refused"}`))

					return
				}
				switch r.Method + " " + r.URL.Path {
				case "POST /api/v1/submissions":
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"submission":{"id":9},"warnings":[]}`))
				case "GET /api/v1/submissions":
					_, _ = w.Write([]byte(`{"submissions":[{"id":9}],"limit":50,"total":1,"has_more":false,"next_before_id":null,"next_after_id":null}`))
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
			})
			r := runCLI(t, "", "doctor", "--e2e", "--json")
			steps := e2eLines(t, r.stdout)
			if r.code != 1 || len(steps) != tc.want || r.stderr != "" {
				t.Fatalf("%+v", r)
			}
			last := steps[len(steps)-1]
			if last.Outcome != "error" || last.Message == "" || last.RequestID != "req-1" {
				t.Fatalf("last line %+v", last)
			}
			for _, s := range steps[:len(steps)-1] {
				if s.Outcome != "ok" {
					t.Fatalf("earlier line %+v", s)
				}
			}
			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Fatalf("the check wrote to the cache: %v", err)
			}
		})
	}
}

func TestDoctorE2E_ListMismatch(t *testing.T) {
	isolate(t)
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"submission":{"id":9},"warnings":[]}`))

			return
		}
		_, _ = w.Write([]byte(`{"submissions":[],"limit":50,"total":0,"has_more":false,"next_before_id":null,"next_after_id":null}`))
	})
	r := runCLI(t, "", "doctor", "--e2e", "--json")
	steps := e2eLines(t, r.stdout)
	if r.code != 1 || len(steps) != 2 || steps[1].Step != "list" || !strings.Contains(steps[1].Message, "0 row(s)") {
		t.Fatalf("%+v", r)
	}
}

func TestDoctorE2E_NoConfig(t *testing.T) {
	_, cache := isolate(t)
	r := runCLI(t, "", "doctor", "--e2e", "--json")
	steps := e2eLines(t, r.stdout)
	if r.code != 1 || len(steps) != 1 || steps[0].Step != "submit" || steps[0].Outcome != "error" ||
		!strings.Contains(steps[0].Message, "no server URL is set") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("the check wrote to the cache: %v", err)
	}
}

func TestDoctorE2E_FlagConflicts(t *testing.T) {
	isolate(t)
	for _, extra := range []string{"--force", "--key-from-stdin"} {
		r := runCLI(t, "", "doctor", "--e2e", extra)
		if r.code != 2 || !strings.Contains(r.stderr, "--e2e cannot be combined") {
			t.Fatalf("%s: %+v", extra, r)
		}
	}
}

func TestDoctorE2E_URLFlag(t *testing.T) {
	isolate(t)
	l := newLive(t, testKey)
	t.Setenv(envAPIKey, testKey)
	t.Setenv(envURL, "http://127.0.0.1:1")
	r := runCLI(t, "", "doctor", "--e2e", "--json", "--url", l.srv.URL)
	if r.code != 0 || len(e2eLines(t, r.stdout)) != 3 || len(l.requests()) != 3 {
		t.Fatalf("%+v %v", r, l.requests())
	}
}

func TestDoctorE2E_InitConflict(t *testing.T) {
	isolate(t)
	r := runCLI(t, "", "doctor", "--e2e", "--init")
	if r.code != 2 || !strings.Contains(r.stderr, "--e2e cannot be combined") {
		t.Fatalf("%+v", r)
	}
}
