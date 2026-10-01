//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package harness

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestLock(t *testing.T) {
	e := testEnv(t)
	unlock, err := Lock(e, nil, "install")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Lock(e, nil, "install")
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(err.Error(), "another agentfeedback install or uninstall is running") {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	unlock, err = Lock(e, nil, "install")
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	unlock()
	if entries, _ := os.ReadDir(e.Home); len(entries) != 0 {
		t.Errorf("the lock left files: %v", entries)
	}
}

func TestLock_Unsupported(t *testing.T) {
	e := testEnv(t)
	orig := flock
	flock = func(int, int) error { return syscall.ENOLCK }
	t.Cleanup(func() { flock = orig })
	var warn bytes.Buffer
	unlock, err := Lock(e, &warn, "uninstall")
	if err != nil {
		t.Fatalf("a file system without locks stopped the run: %v", err)
	}
	unlock()
	if !strings.HasPrefix(warn.String(), "agentfeedback uninstall: warning: cannot lock "+e.Home+" (") ||
		!strings.Contains(warn.String(), "make sure no other install runs at the same time") {
		t.Errorf("warning %q", warn.String())
	}
}
