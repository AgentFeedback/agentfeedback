//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package main

import (
	"errors"
	"os"
)

func lockConfigHandle(*os.File) error {
	return errors.New("configuration locking is unsupported on this platform")
}
