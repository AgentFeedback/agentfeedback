//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package capture

import (
	"errors"
	"os"
	"os/exec"
)

func openSweepLock(string) (*os.File, bool, error) {
	// Automatic sweeps are a successful no-op without platform locking.
	return nil, false, nil
}

func detachSweep(*exec.Cmd, *os.File) (string, error) {
	return "", errors.New("sessions: detached sweep is unsupported on this platform")
}

func acceptSweepLock(*os.File) error {
	return errors.New("sessions: inherited sweep locking is unsupported on this platform")
}
