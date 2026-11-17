package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/capture"
)

func TestFinalHookDeliversAfterExit(t *testing.T) {
	for _, tc := range []struct{ harness, event string }{{"omp", "agent_end"}, {"pi", "agent_settled"}, {"codex", "Stop"}, {"opencode-v1", "session.idle"}, {"opencode-v2", "session.idle"}} {
		t.Run(tc.harness, func(t *testing.T) {
			home := goldenWorld(t)
			cwd := goldenStores[tc.harness](t, home)
			writeClientConfig(t, "[sessions]\nquiet = '300ms'\npush_batch = 1\npush_budget = '2s'\n")
			var completed atomic.Int32
			var policies atomic.Int32
			srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/v1/openapi.json":
					fmt.Fprint(w, `{"paths":{"/api/v1/sessions/policy":{"get":{}},"/api/v1/sessions/uploads":{"post":{}},"/api/v1/sessions/uploads/{id}/parts/{part}":{"put":{}},"/api/v1/sessions/uploads/{id}/complete":{"post":{}}}}`)
				case r.URL.Path == "/api/v1/sessions/policy":
					policies.Add(1)
					fmt.Fprint(w, `{"org_id":"11111111-1111-4111-8111-111111111111","enabled":true,"notice_version":"1","max_part_bytes":10485760,"max_session_bytes":25165824,"incomplete_upload_ttl_seconds":86400}`)
				case r.URL.Path == "/api/v1/meta":
					fmt.Fprint(w, `{}`)
				case r.URL.Path == "/api/v1/sessions/uploads":
					fmt.Fprintf(w, `{"id":"4f877165-f34c-4e5c-a27d-031427c07c69","expires_at":%q,"received_parts":[],"submission_id":null}`, time.Now().Add(time.Hour).Format(time.RFC3339Nano))
				case strings.Contains(r.URL.Path, "/parts/"):
					fmt.Fprint(w, `{"id":"4f877165-f34c-4e5c-a27d-031427c07c69","part":1}`)
				case strings.HasSuffix(r.URL.Path, "/complete"):
					completed.Add(1)
					fmt.Fprint(w, `{"submission":{"id":"9007199254740993","uid":"82c6199d-8a9d-476d-b173-af04d731c198"},"warnings":[]}`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Setenv(envURL, srv.URL)
			t.Setenv(envAPIKey, testKey)
			nowFunc = func() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) }
			if _, err := capturePolicy(os.Getenv, modeFlags{}, fileConfig{}); err != nil {
				t.Fatal(err)
			}
			nowFunc = time.Now
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(map[string]any{"session_id": "final-event", "sessionID": "final-event", "cwd": cwd})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "hook", tc.harness, tc.event)
			cmd.Env = append(os.Environ(), runMainEnv+"=1")
			cmd.Stdin = strings.NewReader(string(payload))
			start := time.Now()
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("hook: %v %s", err, output)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("hook waited for delivery")
			}
			cache, err := cacheDir(os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(15 * time.Second)
			for {
				last, err := capture.ReadLastRun(cache)
				if err != nil {
					t.Fatal(err)
				}
				if !last.Finished.IsZero() {
					if completed.Load() == 0 || last.Errors != 0 || last.Deferred != 0 || policies.Load() < 2 {
						log, _ := os.ReadFile(filepath.Join(cache, "client.log"))
						t.Fatalf("delivery: completed=%d policy=%d last=%+v log=%s", completed.Load(), policies.Load(), last, log)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("detached delivery did not finish")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
