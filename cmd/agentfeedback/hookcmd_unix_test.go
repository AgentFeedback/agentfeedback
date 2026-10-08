//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/detect"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// TestHook_NeverOpensTranscript: a transcript_path naming a FIFO with no
// writer would block an open; the hook returns because it never opens it.
func TestHook_NeverOpensTranscript(t *testing.T) {
	isolate(t)
	fifo := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"session_id": "s", "transcript_path": fifo, "cwd": t.TempDir(), "tool_name": "Bash",
		"tool_input": map[string]string{"command": "x"}, "error": "Exit code 1",
	})
	done := make(chan result, 1)
	go func() { done <- runCLI(t, string(payload), "hook", "claude-code", "PostToolUseFailure") }()
	select {
	case r := <-done:
		if r.code != 0 {
			t.Fatalf("%+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook blocked on the transcript FIFO")
	}
}

// TestHook_LockedSessionGivesUp: a session file another process keeps
// locked makes the hook give up within its deadline, count nothing and
// print nothing.
func TestHook_LockedSessionGivesUp(t *testing.T) {
	_, cache := isolate(t)
	path := detect.StatePath(cache, "claude-code", "s")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Skip(err)
	}
	start := time.Now()
	for range 2 {
		if r := runCLI(t, ccFailure("s", t.TempDir(), "make"), "hook", "claude-code", "PostToolUseFailure"); r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state written: %v", err)
	}
}

// TestHook_DetectTimeout drives runHook into its detect-timeout branch: the
// session file is a FIFO with no writer, so the detect part blocks reading
// it past the deadline. The hook exits 0 with no output and one client-log
// error line, a remote end of turn still flushes, and the detect part,
// released after the deadline, writes no state and logs nothing more.
func TestHook_DetectTimeout(t *testing.T) {
	_, cache := isolate(t)
	oldWait, oldHasDue := hookDetectWait, flushHookHasDue
	hookDetectWait = 300 * time.Millisecond
	var flushed atomic.Bool
	flushHookHasDue = func(string, time.Time) bool { flushed.Store(true); return false }
	t.Cleanup(func() { hookDetectWait, flushHookHasDue = oldWait, oldHasDue })
	t.Setenv(envURL, "http://127.0.0.1:1")
	t.Setenv(envAPIKey, "throwaway")
	path := detect.StatePath(cache, "claude-code", "s")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip(err)
	}
	r := runCLI(t, ccStop("s", t.TempDir()), "hook", "claude-code", "Stop")
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	if !flushed.Load() {
		t.Error("the end of the turn did not flush")
	}
	// Release the blocked reader after the deadline.
	time.Sleep(200 * time.Millisecond)
	opened := make(chan *os.File, 1)
	go func() {
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			opened <- nil

			return
		}
		opened <- w
	}()
	select {
	case w := <-opened:
		if w == nil {
			t.Fatal("open the FIFO for writing")
		}
		_ = w.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the detect part was not blocked on the session file")
	}
	time.Sleep(300 * time.Millisecond)
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("state written after the deadline: %v %v", info, err)
	}
	n := 0
	for _, l := range hookLogLines(t, cache) {
		if l["outcome"] == client.OutcomeError {
			n++
			if !strings.Contains(l["reason"].(string), "detect part ran past its deadline") {
				t.Errorf("line %v", l)
			}
		}
	}
	if n != 1 {
		t.Errorf("log %v", hookLogLines(t, cache))
	}
}
