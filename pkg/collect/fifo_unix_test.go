//go:build unix

package collect

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFIFOConfigDoesNotBlock(t *testing.T) {
	base := t.TempDir()
	repo := fixtureAt(t, base)
	cfg := filepath.Join(repo, ".git", "config")
	if err := syscall.Unlink(cfg); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(cfg, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan Result, 1)
	go func() { done <- collectFor(repo, base, false) }()
	select {
	case r := <-done:
		if r.Context["git_commit"] != headSHA {
			t.Fatalf("%v", r.Context)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO config blocked")
	}
}
