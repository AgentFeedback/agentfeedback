package capture

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows byte-range locks are not inherited. Instead, ownership is the open
// file object's write-sharing denial, which remains effective until both the
// parent and inherited child handles close. Never delete or replace this file.
func openSweepLock(path string) (*os.File, bool, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, false, nil
		}
		return nil, false, err
	}
	f := os.NewFile(uintptr(handle), path)
	if err := validateSweepFile(f, path); err != nil {
		_ = f.Close()
		return nil, false, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, false, err
	}
	return f, true, nil
}

func detachSweep(cmd *exec.Cmd, lock *os.File) (string, error) {
	handle := windows.Handle(lock.Fd())
	if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return "", err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:                 true,
		CreationFlags:              windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(handle)},
	}
	return strconv.FormatUint(uint64(handle), 10), nil
}

func acceptSweepLock(lock *os.File) error {
	// A readable handle to this inode is not ownership: FILE_SHARE_READ lets
	// ordinary readers inspect metadata while the writer owns the lock. An
	// unchanged-size truncate verifies write access, which no second open can
	// obtain while the inherited write-sharing denial is alive.
	info, err := lock.Stat()
	if err != nil {
		return err
	}
	if err := lock.Truncate(info.Size()); err != nil {
		return err
	}
	return windows.SetHandleInformation(windows.Handle(lock.Fd()), windows.HANDLE_FLAG_INHERIT, 0)
}
