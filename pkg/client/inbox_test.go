package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeliver proves Deliver maps each answer to its outcome and flags and
// keeps nothing: no spool, no rejected/ file, no client log, no stderr.
func TestDeliver(t *testing.T) {
	const problem = `{"error":"x","message":"no"}`
	for _, tc := range []struct {
		name        string
		status      int
		outcome     string
		retry, hold bool
		problem     bool
	}{
		{"created", http.StatusCreated, OutcomeSubmitted, false, false, false},
		{"duplicate", http.StatusOK, OutcomeDuplicate, false, false, false},
		{"mismatch", http.StatusConflict, OutcomeMismatch, false, false, true},
		{"rejected", http.StatusBadRequest, OutcomeRejected, false, false, true},
		{"busy", http.StatusServiceUnavailable, OutcomeError, true, false, false},
		{"wrong key", http.StatusUnauthorized, OutcomeError, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == http.StatusCreated || tc.status == http.StatusOK {
					kind, key := sentHead(r)
					created(w, tc.status, 7, kind, key)

					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(problem))
			}))
			t.Cleanup(srv.Close)
			e := newTestEnv(t, srv.URL, srv.Client())

			d := e.c.Deliver(context.Background(), []byte(`{"kind":"friction","key":"k1","summary":"s"}`))
			if d.Outcome.Outcome != tc.outcome || d.Retry != tc.retry || d.Hold != tc.hold {
				t.Fatalf("delivery %+v", d)
			}
			if tc.outcome == OutcomeSubmitted && d.Outcome.ID != 7 {
				t.Fatalf("id %d", d.Outcome.ID)
			}
			if tc.problem && string(d.Problem) != problem {
				t.Fatalf("problem %q", d.Problem)
			}
			for _, p := range []string{SpoolDir(e.data), filepath.Join(e.data, "rejected"), LogPath(e.cache)} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Fatalf("%s written (%v)", p, err)
				}
			}
			if e.stderr.Len() != 0 {
				t.Fatalf("stderr %q", e.stderr)
			}
		})
	}
}

// TestDeliver_ProblemCut proves a refusal body over 64 KiB is cut to
// exactly 64 KiB.
func TestDeliver_ProblemCut(t *testing.T) {
	big := `{"error":"x","message":"` + strings.Repeat("a", 70<<10) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(big))
	}))
	t.Cleanup(srv.Close)
	e := newTestEnv(t, srv.URL, srv.Client())
	d := e.c.Deliver(context.Background(), []byte(`{"kind":"friction","key":"k1","summary":"s"}`))
	if d.Outcome.Outcome != OutcomeRejected {
		t.Fatalf("delivery %+v", d.Outcome)
	}
	if len(d.Problem) != 65536 || string(d.Problem) != big[:65536] {
		t.Fatalf("problem %d bytes", len(d.Problem))
	}
}
