//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package capture

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func openSweepLock(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	fail := func(err error) (*os.File, bool, error) {
		_ = f.Close()
		return nil, false, err
	}
	if err := validateSweepFile(f, path); err != nil {
		return fail(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fail(nil)
		}
		// Unsupported locking must fail closed, unlike advisory UI locks.
		return fail(err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	return f, true, nil
}

func detachSweep(cmd *exec.Cmd, lock *os.File) (string, error) {
	cmd.ExtraFiles = []*os.File{lock}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return "3", nil
}

func acceptSweepLock(lock *os.File) error {
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	syscall.CloseOnExec(int(lock.Fd()))
	return nil
}
