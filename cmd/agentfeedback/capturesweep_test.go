package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/capture"
)

func TestCaptureSweepHookAndStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell child fixture")
	}
	captureNoticeFixture(t, true)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	t.Setenv("SWEEP_TEST_CALLS", calls)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "agentfeedback"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SWEEP_TEST_CALLS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldSweep := captureSweep
	captureSweep = func(cache, data string, opts capture.SweepOptions) error {
		opts.Executable = filepath.Join(dir, "agentfeedback")
		return capture.Sweep(cache, data, opts)
	}
	t.Cleanup(func() { captureSweep = oldSweep })
	payload := `{"session_id":"sweep-fixture","cwd":"/tmp","hook_event_name":"Stop","stop_hook_active":false}`
	start := time.Now()
	r := runCLI(t, payload, "hook", "claude-code", "Stop")
	if r.code != 0 || r.stdout != "" || r.stderr != "" || time.Since(start) > 4500*time.Millisecond {
		t.Fatalf("hook blocked or printed: %+v", r)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := os.ReadFile(calls)
		if err == nil && string(b) == "sessions push --due --json\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child was not launched: %q, %v", b, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	r = runCLI(t, payload, "hook", "claude-code", "Stop")
	if r.code != 0 {
		t.Fatalf("repeated hook: %+v", r)
	}
	// A normal CLI startup in the same quiet interval must not launch again.
	r = runCLI(t, "", "list", "--json")
	if r.code != 0 {
		t.Fatalf("sessions list: %+v", r)
	}
	b, err := os.ReadFile(calls)
	if err != nil || string(b) != "sessions push --due --json\n" {
		t.Fatalf("quiet interval launched another child: %q, %v", b, err)
	}
}

func TestCaptureLastRunVisible(t *testing.T) {
	captureNoticeFixture(t, false)
	cache, err := cacheDir(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	last := capture.LastRun{Started: started, Finished: started.Add(time.Minute), Pushed: 2, Replayed: 1, Skipped: 3, Deferred: 4, Errors: 1}
	b, err := json.Marshal(last)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "push.last-run.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"doctor", "--json"}, {"sessions", "status", "--json"}} {
		r := runCLI(t, "", args...)
		var result struct {
			LastPush capture.LastRun `json:"last_push"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &result); err != nil || result.LastPush != last {
			t.Fatalf("%v lost last run: %+v, %v", args, r, err)
		}
	}
	r := runCLI(t, "", "doctor")
	if !strings.Contains(r.stdout, "last push:") || !strings.Contains(r.stdout, "2 pushed, 1 replayed, 3 skipped, 4 deferred, 1 errors") {
		t.Fatalf("doctor lost last run: %+v", r)
	}
}

func TestCaptureSweepLaunchDeadline(t *testing.T) {
	captureNoticeFixture(t, true)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	oldSweep := captureSweep
	captureSweep = func(string, string, capture.SweepOptions) error {
		close(entered)
		<-release
		close(finished)
		return errors.New("fixture launch failure")
	}
	t.Cleanup(func() { captureSweep = oldSweep })
	start := time.Now()
	triggerCaptureSweep(os.Getenv, modeFlags{}, start.Add(100*time.Millisecond))
	select {
	case <-entered:
	default:
		close(release)
		t.Fatal("launcher was not reached")
	}
	close(release)
	<-finished
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("trigger waited on blocked launch: %s", elapsed)
	}
	cache, _ := cacheDir(os.Getenv)
	b, err := os.ReadFile(filepath.Join(cache, "log", "client.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "trigger failed") {
		t.Fatal("deadline produced a spurious failure log")
	}
}

func TestCaptureSweepHandoffFailureLogged(t *testing.T) {
	isolateCLI(t)
	t.Setenv("AGENTFEEDBACK_CAPTURE_SWEEP", "private-invalid-control")
	r := runCLI(t, "", "sessions", "push", "--due", "--json")
	if r.code == 0 {
		t.Fatal("invalid handoff accepted")
	}
	cache, _ := cacheDir(os.Getenv)
	b, err := os.ReadFile(filepath.Join(cache, "log", "client.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "handoff rejected") || strings.Contains(string(b), "private-invalid-control") {
		t.Fatalf("handoff failure log: %s", b)
	}
}
