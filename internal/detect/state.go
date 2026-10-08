package detect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Retention of the session files, and how often they are pruned.
const (
	retention  = 7 * 24 * time.Hour
	pruneEvery = 24 * time.Hour
	maxIDLen   = 128
)

// SessionsDir is the directory of the session files under the client's
// cache directory.
func SessionsDir(cache string) string { return filepath.Join(cache, "sessions") }

// SafeID is a session id fit for a file name: every character outside
// [A-Za-z0-9._-] replaced by an underscore, at most 128 bytes. An empty id
// stays empty.
func SafeID(id string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}

		return '_'
	}, id)
	if len(out) > maxIDLen {
		out = out[:maxIDLen]
	}

	return out
}

// fileIDMax is the most of the sanitised session id a file name keeps.
const fileIDMax = 40

// lockPoll is how often Update tries the lock again.
const lockPoll = 10 * time.Millisecond

// ErrLocked is Update's error when another process held the session's lock
// until the deadline.
var ErrLocked = errors.New("the session file stayed locked until the deadline")

// StatePath is the session file of a harness's session; "" for an empty
// session id. The name is the sanitised id cut to 40 bytes and the first
// 16 hex digits of the SHA-256 of the whole id, so two ids never share a
// file.
func StatePath(cache, harness, sessionID string) string {
	id := SafeID(sessionID)
	if id == "" {
		return ""
	}
	if len(id) > fileIDMax {
		id = id[:fileIDMax]
	}
	sum := sha256.Sum256([]byte(sessionID))

	return filepath.Join(SessionsDir(cache), SafeID(harness), id+"-"+hex.EncodeToString(sum[:])[:16]+".json")
}

// lockSuffix names the lock file beside a session file.
const lockSuffix = ".lock"

// Update reads the session's state under an exclusive lock, lets fn change
// it and writes it back by rename (owner-only, directories 0700). The lock
// is on a sibling <file>.lock and is tried without waiting until deadline;
// past it Update gives up with ErrLocked and changes nothing. The deadline
// also bounds the write: once it has passed, after the lock is taken or
// just before the file is written, Update releases the lock and returns
// ErrLocked without writing. A missing or
// unreadable file starts from a fresh state. An empty session id keeps no
// state: fn is not called.
func Update(cache, harness, sessionID string, deadline time.Time, fn func(*State)) error {
	path := StatePath(cache, harness, sessionID)
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+lockSuffix, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	for !tryLock(lock) {
		if !time.Now().Add(lockPoll).Before(deadline) {
			return ErrLocked
		}
		time.Sleep(lockPoll)
	}
	if !time.Now().Before(deadline) {
		return ErrLocked
	}
	var st State
	if f, err := os.Open(path); err == nil {
		data, rerr := io.ReadAll(io.LimitReader(f, 1<<20))
		_ = f.Close()
		if rerr != nil {
			return rerr
		}
		if len(data) > 0 && json.Unmarshal(data, &st) != nil {
			st = State{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fn(&st)
	out, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if !time.Now().Before(deadline) {
		return ErrLocked
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(out, '\n'))
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Sync(), tmp.Close()); err != nil {
		_ = os.Remove(tmp.Name())

		return err
	}
	if !time.Now().Before(deadline) {
		_ = os.Remove(tmp.Name())

		return ErrLocked
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())

		return err
	}

	return nil
}

// Prune deletes the session files not changed for 7 days with their lock
// files, the temporary files and the lock files without a session file not
// changed for 7 days, and the files directly in each of extra not changed for 7 days, at
// most once a day: the marker file sessions/.pruned records the last pass.
func Prune(cache string, now time.Time, extra ...string) error {
	dir := SessionsDir(cache)
	marker := filepath.Join(dir, ".pruned")
	if info, err := os.Stat(marker); err == nil && now.Sub(info.ModTime()) < pruneEvery {
		return nil
	}
	harnesses, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var errs []error
	for _, h := range harnesses {
		if !h.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, h.Name()))
		if err != nil {
			errs = append(errs, err)

			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			isState := strings.HasSuffix(name, ".json")
			isTemp := strings.HasPrefix(name, ".state-") && strings.HasSuffix(name, ".tmp")
			isLock := strings.HasSuffix(name, ".json"+lockSuffix)
			if !isState && !isTemp && !isLock {
				continue
			}
			info, err := f.Info()
			if err != nil || now.Sub(info.ModTime()) < retention {
				continue
			}
			path := filepath.Join(dir, h.Name(), name)
			if isLock {
				if _, err := os.Lstat(strings.TrimSuffix(path, lockSuffix)); err == nil || !errors.Is(err, os.ErrNotExist) {
					continue
				}
			}
			targets := []string{path}
			if isState {
				targets = append(targets, path+lockSuffix)
			}
			for _, p := range targets {
				if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
				}
			}
		}
	}
	for _, d := range extra {
		files, err := os.ReadDir(d)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}

			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			info, err := f.Info()
			if err != nil || now.Sub(info.ModTime()) < retention {
				continue
			}
			if err := os.Remove(filepath.Join(d, f.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		errs = append(errs, err)
	} else if err := os.WriteFile(marker, nil, 0o600); err != nil {
		errs = append(errs, err)
	} else if err := os.Chtimes(marker, now, now); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}
