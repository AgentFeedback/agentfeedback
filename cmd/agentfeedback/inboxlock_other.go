//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package main

import (
	"errors"
	"os"
)

func tryLockInbox(*os.File) (bool, error) {
	return false, errors.New("inbox locking is unsupported on this platform")
}
