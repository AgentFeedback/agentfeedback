//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package detect

import (
	"errors"
	"os"
	"syscall"
)

// tryLock tries once to take an exclusive advisory lock on f without
// waiting. It reports false only when another process holds the lock; a
// file system that cannot lock goes on without one.
func tryLock(f *os.File) bool {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)

	return !errors.Is(err, syscall.EWOULDBLOCK)
}
