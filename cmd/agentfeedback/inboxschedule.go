package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	inboxCursor = ".cursor"
	inboxLock   = ".cursor.lock"
)

// Keep the lock inode stable while cursor publication replaces its own inode.
// The lock covers the pass, so an older invocation cannot overwrite newer progress.
func lockInbox(ctx context.Context, root *os.Root) (*os.File, error) {
	f, err := root.OpenFile(inboxLock, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := root.Lstat(inboxLock)
		if statErr != nil {
			return nil, statErr
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("inbox scheduler lock is not a regular file")
		}
		f, err = root.OpenFile(inboxLock, os.O_RDWR|inboxOpenFlags, 0)
		if err == nil {
			opened, statErr := f.Stat()
			current, currentErr := root.Lstat(inboxLock)
			if statErr != nil || currentErr != nil || !os.SameFile(info, opened) || !os.SameFile(info, current) || !current.Mode().IsRegular() {
				_ = f.Close()
				return nil, fmt.Errorf("inbox scheduler lock changed while opening")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		locked, err := tryLockInbox(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if locked {
			break
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return f, nil
}

func readInboxCursor(root *os.Root) (string, error) {
	info, statErr := root.Lstat(inboxCursor)
	if statErr == nil && info.Mode().IsRegular() && info.Size() > 6*nameMax+2 {
		return "", fmt.Errorf("inbox scheduler: cursor is too large")
	}
	body, _, why, err := readInboxFile(root, inboxCursor)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inbox scheduler: %w", err)
	}
	if why != "" {
		return "", fmt.Errorf("inbox scheduler: %s", why)
	}
	var rawName []byte
	if err := json.Unmarshal(body, &rawName); err != nil {
		return "", fmt.Errorf("inbox scheduler: invalid cursor: %w", err)
	}
	name := string(rawName)
	if name == "" || len(name) > nameMax || filepath.Base(name) != name || strings.ContainsRune(name, '\x00') || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, inboxSuffix) {
		return "", fmt.Errorf("inbox scheduler: invalid cursor")
	}
	return name, nil
}

func orderInbox(names []string, cursor string) {
	slices.Sort(names)
	start, found := slices.BinarySearch(names, cursor)
	if found {
		start++
	}
	// Rotate in place, including when the previous candidate was removed.
	slices.Reverse(names[:start])
	slices.Reverse(names[start:])
	slices.Reverse(names)
}

func writeInboxCursor(root *os.Root, name string) error {
	// File names need not be UTF-8 on Unix; JSON byte encoding preserves them.
	body, err := json.Marshal([]byte(name))
	if err != nil {
		return err
	}
	tmp := ".cursor-" + uuid.NewString()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()
	if n, writeErr := f.Write(body); writeErr != nil {
		err = writeErr
	} else if n != len(body) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = root.Rename(tmp, inboxCursor); err != nil {
		return err
	}
	// Windows cannot flush a read-only directory handle. The file itself was
	// synced before rename; a power loss may replay an older cursor there.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
