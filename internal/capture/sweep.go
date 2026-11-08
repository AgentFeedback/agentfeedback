package capture

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sweepControlEnv = "AGENTFEEDBACK_CAPTURE_SWEEP"

// SweepOptions contains permission already read from the matching destination's
// cache. Sweep never discovers policy or opens transcripts. Executable defaults
// to the running executable; Args and Env default to sessions push --due --json
// and the current environment, respectively.
// The child must resolve the same cache and data directories as its parent.
type SweepOptions struct {
	Policy     Policy
	Config     Config
	Executable string
	Args       []string
	Env        []string
	Clock      func() time.Time
}

// LastRun is the credential- and transcript-free summary of one automatic run.
type LastRun struct {
	Started        time.Time  `json:"started"`
	Finished       time.Time  `json:"finished"`
	Pushed         int        `json:"pushed"`
	Replayed       int        `json:"replayed"`
	Skipped        int        `json:"skipped"`
	Deferred       int        `json:"deferred"`
	Errors         int        `json:"errors"`
	SuspendedUntil *time.Time `json:"suspended_until"`
}

type sweepControl struct {
	Handle  string    `json:"handle"`
	Token   string    `json:"token"`
	Cache   string    `json:"cache"`
	DataDir string    `json:"data_dir"`
	Started time.Time `json:"started"`
}

type sweepStart struct {
	Started time.Time `json:"started"`
}

// Sweep tries once to launch a bounded, detached push. Disabled or incomplete
// permission, a busy lock, and a quiet-period hit are successful no-ops. Only the
// child runs Push, which refreshes consent and enforces the configured budget
// and batch. No network work happens here.
func Sweep(cache, dataDir string, opts SweepOptions) error {
	if !usablePolicy(opts.Policy) {
		return nil
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	now := clock().UTC()
	quiet := opts.Config.Quiet
	if quiet <= 0 {
		quiet = defaultQuiet
	}
	budget := opts.Config.PushBudget
	if budget <= 0 {
		budget = 90 * time.Second
	}
	cache, dataDir, err := sweepDirectories(cache, dataDir)
	if err != nil {
		return err
	}
	if err := ensureSweepDir(cache); err != nil {
		return err
	}
	lock, locked, err := openSweepLock(filepath.Join(cache, "push.lock"))
	if err != nil || !locked {
		return err
	}
	// Close, never LOCK_UN: the child inherits this same lock ownership. An
	// explicit unlock here would also unlock the child's open file description.
	defer func() { _ = lock.Close() }()
	startedPath := filepath.Join(cache, "push.last-started")
	previous, err := readSweepFile(startedPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(previous) > 0 {
		var last sweepStart
		// Invalid or future starts are abandoned once kernel ownership is ours.
		// The marker never authorizes bypassing a live uploading child's lock.
		if err := json.Unmarshal(previous, &last); err == nil && !last.Started.IsZero() && !last.Started.After(now) {
			age := now.Sub(last.Started)
			if age < quiet {
				if age < budget+time.Minute {
					return nil
				}
				completed, err := ReadLastRun(cache)
				if err != nil {
					return err
				}
				if completed.Started.Equal(last.Started) && !completed.Finished.IsZero() {
					return nil
				}
				// An abandoned start bypasses a longer quiet period.
			}
		}
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	control := sweepControl{Token: hex.EncodeToString(token[:]), Cache: cache, DataDir: dataDir, Started: now}
	metadata, err := json.Marshal(control)
	if err != nil {
		return err
	}
	if err := lock.Truncate(0); err != nil {
		return err
	}
	if _, err := lock.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := lock.Write(append(metadata, '\n')); err != nil {
		return err
	}
	if err := lock.Sync(); err != nil {
		return err
	}
	executable := opts.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return err
		}
	}
	args := opts.Args
	if args == nil {
		args = []string{"sessions", "push", "--due", "--json"}
	}
	cmd := exec.Command(executable, args...)
	control.Handle, err = detachSweep(cmd, lock)
	if err != nil {
		return err
	}
	controlJSON, err := json.Marshal(control)
	if err != nil {
		return err
	}
	env := opts.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(strings.ToUpper(entry), sweepControlEnv+"=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, sweepControlEnv+"="+string(controlJSON))
	// Nil standard streams are connected to the null device by os/exec.
	// Persist before Start so a fast child cannot finish before its start marker.
	if err := writeSweepJSON(startedPath, sweepStart{Started: now}); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		var restoreErr error
		if len(previous) == 0 {
			restoreErr = os.Remove(startedPath)
		} else {
			restoreErr = atomicSweepWrite(startedPath, previous)
		}
		return errors.Join(fmt.Errorf("sessions: cannot start sweep: %w", err), restoreErr)
	}
	// Reap while the parent lives; the detached child does not depend on this
	// goroutine or on the parent's lifetime. Never wait on its work in a hook.
	go func() { _ = cmd.Wait() }()
	return nil
}

// SweepRun owns the inherited lock until Finish or Close. It must remain open
// for the complete Push call, including policy refresh and outcome recording.
type SweepRun struct {
	lock    *os.File
	cache   string
	started time.Time
	mu      sync.Mutex
}

// AcceptSweep returns nil for an ordinary manual push. A control marker is not
// sufficient: the inherited handle must name the stable lock inode, carry its
// random ownership token, and still hold the platform lock. Invalid handoffs
// fail closed rather than running an unprotected upload.
func AcceptSweep(cache, dataDir string) (*SweepRun, error) {
	raw := os.Getenv(sweepControlEnv)
	if raw == "" {
		return nil, nil
	}
	var control sweepControl
	if err := json.Unmarshal([]byte(raw), &control); err != nil || control.Token == "" || control.Started.IsZero() {
		return nil, errors.New("sessions: invalid sweep handoff")
	}
	cache, dataDir, err := sweepDirectories(cache, dataDir)
	if err != nil {
		return nil, err
	}
	if cache != control.Cache || dataDir != control.DataDir {
		return nil, errors.New("sessions: sweep directory mismatch")
	}
	handle, err := strconv.ParseUint(control.Handle, 10, 64)
	if err != nil || handle <= 2 || uint64(uintptr(handle)) != handle {
		return nil, errors.New("sessions: invalid sweep handle")
	}
	lock := os.NewFile(uintptr(handle), "capture sweep lock")
	if lock == nil {
		return nil, errors.New("sessions: missing sweep handle")
	}
	fail := func(err error) (*SweepRun, error) {
		_ = lock.Close()
		return nil, err
	}
	if err := validateSweepFile(lock, filepath.Join(cache, "push.lock")); err != nil {
		return fail(err)
	}
	if _, err := lock.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	metadata, err := io.ReadAll(io.LimitReader(lock, 4097))
	if err != nil || len(metadata) > 4096 {
		return fail(errors.New("sessions: invalid sweep lock metadata"))
	}
	var owner sweepControl
	if err := json.Unmarshal(metadata, &owner); err != nil || owner.Token != control.Token || owner.Cache != cache || owner.DataDir != dataDir || !owner.Started.Equal(control.Started) {
		return fail(errors.New("sessions: sweep ownership mismatch"))
	}
	if err := acceptSweepLock(lock); err != nil {
		return fail(err)
	}
	// Do not accidentally hand the control marker to any subprocess of Push.
	if err := os.Unsetenv(sweepControlEnv); err != nil {
		return fail(err)
	}
	return &SweepRun{lock: lock, cache: cache, started: control.Started}, nil
}

func (r *SweepRun) Started() time.Time { return r.started }

// Finish persists a complete summary by owner-only atomic rename before
// releasing ownership. A failed write still releases the lock and is returned
// for the CLI's normal client log. The start cannot be supplied by the caller.
func (r *SweepRun) Finish(last LastRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock == nil {
		return errors.New("sessions: sweep already released")
	}
	last.Started = r.started
	if last.Finished.IsZero() {
		last.Finished = time.Now().UTC()
	}
	err := writeSweepJSON(filepath.Join(r.cache, "push.last-run.json"), last)
	err = errors.Join(err, r.lock.Close())
	r.lock = nil
	return err
}

// Close releases a handoff without recording a completed run. It is idempotent.
func (r *SweepRun) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock == nil {
		return nil
	}
	err := r.lock.Close()
	r.lock = nil
	return err
}

// ReadLastRun reads the last complete automatic-run summary, never partial
// progress. A missing record is the zero value, not an error.
func ReadLastRun(cache string) (LastRun, error) {
	var last LastRun
	data, err := readSweepFile(filepath.Join(cache, "push.last-run.json"))
	if errors.Is(err, os.ErrNotExist) {
		return last, nil
	}
	if err != nil {
		return last, err
	}
	if err := json.Unmarshal(data, &last); err != nil {
		return LastRun{}, errors.New("sessions: invalid sweep run record")
	}
	return last, nil
}

func sweepDirectories(cache, dataDir string) (string, string, error) {
	if cache == "" || dataDir == "" {
		return "", "", errors.New("sessions: missing sweep directory")
	}
	cache, err := filepath.Abs(cache)
	if err != nil {
		return "", "", err
	}
	dataDir, err = filepath.Abs(dataDir)
	return cache, dataDir, err
}

func ensureSweepDir(cache string) error {
	dir := cache
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("sessions: unsafe sweep directory")
	}
	return os.Chmod(dir, 0o700)
}

func validateSweepFile(f *os.File, path string) error {
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return errors.New("sessions: unsafe sweep file")
	}
	return nil
}

func readSweepFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if err := validateSweepFile(f, path); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if len(data) > 4096 {
		return nil, errors.New("sessions: sweep record is too large")
	}
	return data, err
}

func writeSweepJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicSweepWrite(path, append(data, '\n'))
}

func atomicSweepWrite(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("sessions: unsafe sweep record")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".push-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Chmod(0o600), f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
