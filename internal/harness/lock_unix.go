//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package harness

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// flock is syscall.Flock; a variable so tests can make it fail.
var flock = syscall.Flock

// Lock takes an exclusive advisory lock on the home directory itself, so
// two install or uninstall runs never interleave and no lock file is left
// behind; the returned function releases it. Another run holding the lock
// is a refusal. A file system that cannot lock (NFS, for one) is a warning
// on warn, and the run goes on without the lock.
func Lock(e Env, warn io.Writer, command string) (func(), error) {
	f, err := os.Open(e.Home)
	if err != nil {
		return nil, &FileError{Path: e.Home, Err: err}
	}
	if err := flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &Refusal{
				Problem: "another agentfeedback install or uninstall is running",
				Next:    "run it again when it finishes",
			}
		}
		fmt.Fprintf(warn, "agentfeedback %s: warning: cannot lock %s (%v); make sure no other install runs at the same time\n", command, e.Home, err)

		return func() {}, nil
	}

	return func() { _ = f.Close() }, nil
}
