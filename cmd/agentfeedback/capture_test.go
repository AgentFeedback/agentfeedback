package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/capture"
)

func TestCaptureCommandsSharePolicyCache(t *testing.T) {
	newInstallEnv(t)
	// This exercises foreground policy caching. Automatic sweeps refresh
	// consent independently and are covered by the sweep tests.
	oldSweep := captureSweep
	captureSweep = func(string, string, capture.SweepOptions) error { return nil }
	t.Cleanup(func() { captureSweep = oldSweep })
	live := newLive(t, testKey)
	var requests atomic.Int32
	srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/openapi.json" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"paths":{"/api/v1/sessions/policy":{"get":{}}}}`)
			return
		}
		if r.URL.Path != "/api/v1/sessions/policy" {
			live.srv.Config.Handler.ServeHTTP(w, r)
			return
		}
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			t.Error("policy request lacks installation authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"org_id":"11111111-1111-4111-8111-111111111111","org_name":"Example","enabled":true,"notice_version":"1","max_part_bytes":10485760,"max_session_bytes":25165824,"incomplete_upload_ttl_seconds":86400,"retention":{"mode":"retain","days":30}}`)
	}))
	r, init := initRun(t, testKey+"\n", "--server", srv.URL, "--key-from-stdin", "--yes", "--harnesses", "none")
	if r.code != 0 || init.Capture.State != "enabled" || strings.Contains(r.stderr, "sessions: this org") {
		t.Fatalf("init: %+v", r)
	}
	path, err := configPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	file, _, err := loadFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := capturePolicy(os.Getenv, modeFlags{}, file)
	if err != nil || policy.State != capture.Enabled || policy.LastNoticeVersion != "" {
		t.Fatalf("JSON init marked notice: %+v, %v", policy, err)
	}
	for _, args := range [][]string{{"doctor", "--json"}, {"sessions", "status", "--json"}} {
		r := runCLI(t, "", args...)
		var out struct {
			Capture struct {
				State string `json:"state"`
			} `json:"capture"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Capture.State != "enabled" {
			t.Fatalf("%v: %+v", args, r)
		}
		if strings.Contains(r.stdout+r.stderr, testKey) {
			t.Fatal("installation key leaked")
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("policy requests = %d, want cached single request", requests.Load())
	}
}

func TestCaptureDoctorDenialAndBackoff(t *testing.T) {
	for _, tc := range []struct {
		code           int
		state, problem string
	}{{401, "unauthorized", "configured server"}, {403, "unauthorized", "configured server"}, {429, "suspended", "Retry-After"}, {404, "unsupported", ""}} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			isolateCLI(t)
			live := newLive(t, testKey)
			var requests atomic.Int32
			srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/sessions/policy" {
					live.srv.Config.Handler.ServeHTTP(w, r)
					return
				}
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(tc.code)
				fmt.Fprint(w, `{"error":"policy_unavailable"}`)
			}))
			t.Setenv(envURL, srv.URL)
			t.Setenv(envAPIKey, testKey)
			for range 2 {
				rep, r := doctorJSON(t)
				if rep.Capture.State != tc.state {
					t.Fatalf("state %q: %+v", rep.Capture.State, r)
				}
				if tc.problem != "" && (r.code != 1 || !hasProblem(rep, tc.problem)) {
					t.Fatalf("missing actionable problem: %+v", r)
				}
				if tc.problem == "" && r.code != 0 {
					t.Fatalf("unsupported server should be healthy: %+v", r)
				}
			}
			if requests.Load() != 1 {
				t.Fatalf("backoff sent %d requests", requests.Load())
			}
		})
	}
}

// A short write without an error still means the notice was not delivered.
type captureShortWriter struct{}

func (captureShortWriter) Write(p []byte) (int, error) {
	return len(p) - 1, nil
}

type captureFailedWriter struct{}

func (captureFailedWriter) Write([]byte) (int, error) {
	return 0, errors.New("notice output failed")
}

func captureNoticeFixture(t *testing.T, enabled bool) capture.Policy {
	t.Helper()
	isolateCLI(t)
	live := newLive(t, testKey)
	srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/openapi.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"paths":{"/api/v1/sessions/policy":{"get":{}}}}`)
		case "/api/v1/sessions/policy":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"org_id":"11111111-1111-4111-8111-111111111111","org_name":"Example","enabled":%t,"notice_version":"notice-1","max_part_bytes":10485760,"max_session_bytes":25165824,"incomplete_upload_ttl_seconds":86400,"retention":{"mode":"retain","days":30}}`, enabled)
		default:
			live.srv.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Setenv(envURL, srv.URL)
	t.Setenv(envAPIKey, testKey)
	p, err := capturePolicy(os.Getenv, modeFlags{}, fileConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCaptureNoticePersistence(t *testing.T) {
	for _, command := range []string{"doctor", "sessions status", "init", "init failure"} {
		for _, output := range []string{"json", "disabled", "failed", "short", "text"} {
			t.Run(command+"/"+output, func(t *testing.T) {
				p := captureNoticeFixture(t, output != "disabled")
				var out, stderr bytes.Buffer
				var w io.Writer = &out
				switch output {
				case "failed":
					w = captureFailedWriter{}
				case "short":
					w = captureShortWriter{}
				}
				asJSON := output == "json"
				var err error
				switch command {
				case "doctor":
					var args []string
					if asJSON {
						args = []string{"--json"}
					}
					err = runDoctor(args, strings.NewReader(""), w, &stderr)
				case "sessions status":
					var args []string
					if asJSON {
						args = []string{"--json"}
					}
					err = runSessionsStatus(args, w, &stderr)
				default:
					var initErr error
					if command == "init failure" {
						initErr = errors.New("init check failed")
					}
					err = finishInit(w, initResult{Mode: modeRemote, Capture: p}, asJSON, initErr)
				}
				if output == "failed" || output == "short" {
					if err == nil {
						t.Fatal("output failure was ignored")
					}
					if output == "short" && !errors.Is(err, io.ErrShortWrite) {
						t.Fatalf("short write: %v", err)
					}
				} else if command != "init failure" && err != nil {
					t.Fatalf("%v\nstdout: %s\nstderr: %s", err, out.String(), stderr.String())
				}
				current, loadErr := capturePolicy(os.Getenv, modeFlags{}, fileConfig{})
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				want := ""
				if output == "text" {
					want = p.NoticeVersion
				}
				if current.LastNoticeVersion != want {
					t.Fatalf("persisted notice = %q, want %q", current.LastNoticeVersion, want)
				}
			})
		}
	}
}

func TestCaptureSessionsStatusRemoteFailureReportedOnce(t *testing.T) {
	for _, args := range [][]string{{"sessions", "status"}, {"sessions", "status", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateCLI(t)
			srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error":"unavailable"}`)
			}))
			t.Setenv(envURL, srv.URL)
			t.Setenv(envAPIKey, testKey)
			r := runCLI(t, "", args...)
			if r.code == 0 || strings.Count(r.stderr, "policy request failed (HTTP 500)") != 1 {
				t.Fatalf("remote failure should be nonzero and reported once: %+v", r)
			}
		})
	}
}

func TestCaptureMissingKeyIsNotReportedAsRejected(t *testing.T) {
	newInstallEnv(t)
	srv := startServer(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("missing key must not request the remote policy")
	}))
	t.Setenv(envURL, srv.URL)
	t.Setenv(envAPIKey, "")
	p, err := capturePolicy(os.Getenv, modeFlags{}, fileConfig{})
	if err != nil || p.State != capture.Unauthorized {
		t.Fatalf("missing-key policy: %+v, %v", p, err)
	}
	if status := capture.Status(p); !strings.Contains(status, "installation key is required") || strings.Contains(status, "the key cannot") {
		t.Fatalf("misleading missing-key status: %q", status)
	}
	if problem := captureProblem(p); problem == nil || !strings.Contains(problem.Error(), "installation key is required") {
		t.Fatalf("misleading missing-key problem: %v", problem)
	}
	r, res := initRun(t, "", "--yes", "--harnesses", "none")
	if r.code != 0 || res.Capture.State != capture.Unauthorized {
		t.Fatalf("missing-key init: %+v", r)
	}
	if strings.Contains(r.stdout+r.stderr, "the key cannot") || !strings.Contains(r.stdout, "installation key is required") {
		t.Fatalf("misleading missing-key init: %+v", r)
	}
}

func TestCaptureDoctorLocalURLCredentials(t *testing.T) {
	isolateCLI(t)
	t.Setenv(envURL, "https://user:private-password@example.invalid/?token=private-query#private-fragment")
	r := runCLI(t, "", "doctor", "--local", "--json")
	var report doctorReport
	if err := json.Unmarshal([]byte(r.stdout), &report); err != nil {
		t.Fatalf("invalid doctor output: %v", err)
	}
	if report.Capture.State != capture.NotApplicable || report.Capture.Server != "" {
		t.Fatalf("local capture disclosed a destination: %+v", report.Capture)
	}
	for _, secret := range []string{"private-password", "private-query", "private-fragment"} {
		if strings.Contains(r.stdout+r.stderr, secret) {
			t.Fatal("doctor leaked URL credentials")
		}
	}
}
