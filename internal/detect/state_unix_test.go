//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package detect

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestUpdate_LockedGivesUp: a lock another holder keeps makes Update give
// up at the deadline without counting.
func TestUpdate_LockedGivesUp(t *testing.T) {
	cache := t.TempDir()
	path := StatePath(cache, "pi", "s")
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
	called := false
	err = Update(cache, "pi", "s", start.Add(200*time.Millisecond), func(*State) { called = true })
	if !errors.Is(err, ErrLocked) || called {
		t.Fatalf("%v %v", err, called)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state written: %v", err)
	}
}
