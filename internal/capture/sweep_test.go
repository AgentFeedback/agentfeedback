//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows

package capture

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test executable is a real detached child, not a mocked Start callback.
// It keeps the inherited lock while waiting for an explicit release marker.
func TestSweepChildProcess(t *testing.T) {
	if os.Getenv("AF_SWEEP_TEST_CHILD") != "1" {
		return
	}
	root := os.Getenv("AF_SWEEP_TEST_ROOT")
	fail := func(err error) {
		_ = os.WriteFile(filepath.Join(root, "error"), []byte(err.Error()), 0o600)
		os.Exit(91)
	}
	run, err := AcceptSweep(os.Getenv("AF_SWEEP_TEST_CACHE"), os.Getenv("AF_SWEEP_TEST_DATA"))
	if err != nil {
		fail(err)
	}
	if run == nil {
		fail(errors.New("child did not accept inherited ownership"))
	}
	active := filepath.Join(root, "active")
	f, err := os.OpenFile(active, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		fail(errors.New("overlapping child"))
	}
	_ = f.Close()
	launches, err := os.OpenFile(filepath.Join(root, "launches"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fail(err)
	}
	_, err = launches.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	_ = launches.Close()
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ready"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fail(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fail(errors.New("test child release deadline"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	suspended := run.Started().Add(time.Hour)
	err = run.Finish(LastRun{
		Finished: run.Started().Add(time.Second), Pushed: 2, Replayed: 3,
		Skipped: 4, Deferred: 5, Errors: 6, SuspendedUntil: &suspended,
	})
	if err != nil {
		fail(err)
	}
	_ = os.Remove(active)
	if err := os.WriteFile(filepath.Join(root, "finished"), []byte("done"), 0o600); err != nil {
		fail(err)
	}
	os.Exit(0)
}

type sweepFixture struct {
	t     *testing.T
	cache string
	data  string
	root  string
	now   time.Time
	opts  SweepOptions
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	f := &sweepFixture{
		t: t, cache: filepath.Join(t.TempDir(), "cache"), data: t.TempDir(),
		root: t.TempDir(), now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.opts = SweepOptions{
		Policy: Policy{
			State: Enabled, Enabled: true, Server: boundaryServer,
			OrgID: testOrg, Since: f.now.Add(-time.Hour), credentialFingerprint: "test-fingerprint",
		},
		Config:     Config{Quiet: 5 * time.Minute, PushBudget: time.Second, PushBatch: 2},
		Executable: executable, Args: []string{"-test.run=^TestSweepChildProcess$"},
		Clock: func() time.Time { return f.now },
	}
	f.opts.Env = f.childEnv(f.root)
	return f
}

func (f *sweepFixture) childEnv(root string) []string {
	f.t.Helper()
	env := append(os.Environ(), "AF_SWEEP_TEST_CHILD=1", "AF_SWEEP_TEST_ROOT="+root,
		"AF_SWEEP_TEST_CACHE="+f.cache, "AF_SWEEP_TEST_DATA="+f.data)
	f.t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(root, "release"), []byte("release"), 0o600)
	})
	return env
}

func waitSweep(t *testing.T, root string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if data, err := os.ReadFile(filepath.Join(root, "error")); err == nil {
			t.Fatalf("detached child: %s", data)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for detached child")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sweepExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (f *sweepFixture) start() {
	f.t.Helper()
	if err := Sweep(f.cache, f.data, f.opts); err != nil {
		f.t.Fatal(err)
	}
	waitSweep(f.t, f.root, func() bool { return sweepExists(filepath.Join(f.root, "ready")) })
}

func (f *sweepFixture) finish() {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, "release"), []byte("release"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	waitSweep(f.t, f.root, func() bool { return sweepExists(filepath.Join(f.root, "finished")) })
}

func TestSweepNonEnabledDoesNothing(t *testing.T) {
	f := newSweepFixture(t)
	for _, state := range []string{"", Disabled, Unsupported, Unauthorized, Suspended, Error, NotApplicable} {
		f.opts.Policy.State = state
		if err := Sweep(f.cache, f.data, f.opts); err != nil {
			t.Fatal(err)
		}
	}
	f.opts.Policy.State, f.opts.Policy.Enabled = Enabled, false
	if err := Sweep(f.cache, f.data, f.opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-enabled sweep touched cache: %v", err)
	}
	if sweepExists(filepath.Join(f.root, "ready")) {
		t.Fatal("non-enabled sweep spawned a child")
	}
}

func TestSweepDefaultsToRunningExecutable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	f := newSweepFixture(t)
	f.opts.Executable = ""
	f.start()
	f.finish()
}

func TestSweepInheritedLockAndQuietThrottle(t *testing.T) {
	f := newSweepFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() { errs <- Sweep(f.cache, f.data, f.opts) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	waitSweep(t, f.root, func() bool { return sweepExists(filepath.Join(f.root, "ready")) })
	// Sweep has returned while the uploading child still owns the lock.
	if sweepExists(filepath.Join(f.root, "finished")) {
		t.Fatal("child did not remain detached and running")
	}
	f.finish()
	// A completed run stays quiet even after budget+one-minute has elapsed.
	f.now = f.now.Add(2 * time.Minute)
	if err := Sweep(f.cache, f.data, f.opts); err != nil {
		t.Fatal(err)
	}
	marker, err := readSweepFile(filepath.Join(f.cache, "push.last-started"))
	var started sweepStart
	if err != nil || json.Unmarshal(marker, &started) != nil || !started.Started.Equal(f.now.Add(-2*time.Minute)) {
		t.Fatalf("completed quiet run started a new child: %s, %v", marker, err)
	}
	data, err := os.ReadFile(filepath.Join(f.root, "launches"))
	if err != nil || len(strings.Fields(string(data))) != 1 {
		t.Fatalf("launches after contention and quiet hit = %q, %v", data, err)
	}
	// Once quiet expires a new real child can acquire the unchanged lock file.
	f.now = f.now.Add(5 * time.Minute)
	second := t.TempDir()
	f.opts.Env = f.childEnv(second)
	if err := Sweep(f.cache, f.data, f.opts); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, second, func() bool { return sweepExists(filepath.Join(second, "ready")) })
	if err := os.WriteFile(filepath.Join(second, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, second, func() bool { return sweepExists(filepath.Join(second, "finished")) })
}

func TestSweepStaleRecoveryNeverStealsLiveLock(t *testing.T) {
	f := newSweepFixture(t)
	f.opts.Config.Quiet = time.Hour
	f.start()
	before, err := os.Stat(filepath.Join(f.cache, "push.lock"))
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(f.opts.Config.PushBudget + time.Minute + time.Second)
	second := t.TempDir()
	f.opts.Env = f.childEnv(second)
	if err := Sweep(f.cache, f.data, f.opts); err != nil {
		t.Fatal(err)
	}
	// Give a wrongly spawned child time to become observable before killing
	// the original; otherwise an overlap bug could hide behind scheduling.
	until := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(until) {
		if sweepExists(filepath.Join(second, "ready")) {
			t.Fatal("stale timestamp bypassed live inherited ownership")
		}
		if data, err := os.ReadFile(filepath.Join(second, "error")); err == nil {
			t.Fatalf("unexpected second child: %s", data)
		}
		time.Sleep(5 * time.Millisecond)
	}
	after, err := os.Stat(filepath.Join(f.cache, "push.lock"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("stale lock inode was replaced: %v", err)
	}
	pidBytes, err := os.ReadFile(filepath.Join(f.root, "ready"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = process.Release()
	// The abandoned timestamp is stale even though the one-hour quiet period
	// has not elapsed. Retry only nonblocking lock attempts until death releases
	// kernel ownership; no PID guessing or lock-file deletion is involved.
	waitSweep(t, second, func() bool {
		if err := Sweep(f.cache, f.data, f.opts); err != nil {
			t.Fatal(err)
		}
		return sweepExists(filepath.Join(second, "ready"))
	})
	if err := os.WriteFile(filepath.Join(second, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitSweep(t, second, func() bool { return sweepExists(filepath.Join(second, "finished")) })
}

func TestSweepInvalidStartRecoveryNeverStealsLiveLock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record string
	}{
		{"malformed JSON", `{"started":`},
		{"malformed timestamp", `{"started":"invalid"}`},
		{"missing timestamp", `{}`},
		{"zero timestamp", `{"started":"0001-01-01T00:00:00Z"}`},
		{"future timestamp", `{"started":"9999-12-31T23:59:59Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSweepFixture(t)
			f.opts.Config.Quiet = time.Hour
			f.start()
			lockPath := filepath.Join(f.cache, "push.lock")
			before, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			startedPath := filepath.Join(f.cache, "push.last-started")
			if err := atomicSweepWrite(startedPath, []byte(tc.record)); err != nil {
				t.Fatal(err)
			}
			second := t.TempDir()
			f.opts.Env = f.childEnv(second)
			if err := Sweep(f.cache, f.data, f.opts); err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(100 * time.Millisecond)
			for time.Now().Before(until) {
				if sweepExists(filepath.Join(second, "ready")) {
					t.Fatal("invalid start bypassed live inherited ownership")
				}
				if data, err := os.ReadFile(filepath.Join(second, "error")); err == nil {
					t.Fatalf("unexpected second child: %s", data)
				}
				time.Sleep(5 * time.Millisecond)
			}
			// Finishing releases kernel ownership but leaves the invalid start.
			// A completed summary must not prevent repairing that marker.
			f.finish()
			if err := Sweep(f.cache, f.data, f.opts); err != nil {
				t.Fatal(err)
			}
			waitSweep(t, second, func() bool { return sweepExists(filepath.Join(second, "ready")) })
			after, err := os.Stat(lockPath)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("invalid start replaced lock inode: %v", err)
			}
			marker, err := readSweepFile(startedPath)
			var started sweepStart
			if err != nil || json.Unmarshal(marker, &started) != nil || !started.Started.Equal(f.now) {
				t.Fatalf("invalid start was not repaired: %s, %v", marker, err)
			}
			if err := os.WriteFile(filepath.Join(second, "release"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			waitSweep(t, second, func() bool { return sweepExists(filepath.Join(second, "finished")) })
		})
	}
}

func TestSweepPersistsOwnerOnlySummary(t *testing.T) {
	f := newSweepFixture(t)
	if last, err := ReadLastRun(f.cache); err != nil || !last.Started.IsZero() {
		t.Fatalf("missing summary = %+v, %v", last, err)
	}
	f.start()
	f.finish()
	last, err := ReadLastRun(f.cache)
	if err != nil {
		t.Fatal(err)
	}
	suspended := f.now.Add(time.Hour)
	want := LastRun{
		Started: f.now, Finished: f.now.Add(time.Second), Pushed: 2,
		Replayed: 3, Skipped: 4, Deferred: 5, Errors: 6, SuspendedUntil: &suspended,
	}
	if !reflect.DeepEqual(last, want) {
		t.Fatalf("summary = %+v, want %+v", last, want)
	}
	for _, name := range []string{"push.lock", "push.last-started", "push.last-run.json"} {
		path := filepath.Join(f.cache, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("%s permissions = %o", name, info.Mode().Perm())
		}
		if name != "push.lock" {
			data, err := os.ReadFile(path)
			if err != nil || !json.Valid(data) {
				t.Errorf("%s is not a complete JSON record: %q, %v", name, data, err)
			}
		}
	}
	info, err := os.Stat(f.cache)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
		t.Fatalf("cache permissions = %v, %v", info, err)
	}
	entries, err := os.ReadDir(f.cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".push-") {
			t.Errorf("atomic write left temporary %s", entry.Name())
		}
	}
}

func TestSweepFailedStartDoesNotThrottle(t *testing.T) {
	f := newSweepFixture(t)
	f.opts.Executable = filepath.Join(t.TempDir(), "missing-agentfeedback")
	if err := Sweep(f.cache, f.data, f.opts); err == nil {
		t.Fatal("missing executable did not fail")
	}
	if _, err := os.Stat(filepath.Join(f.cache, "push.last-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed start retained throttle marker: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Executable = executable
	f.start()
	f.finish()
}

func TestAcceptSweepRejectsUninheritedMarker(t *testing.T) {
	t.Setenv(sweepControlEnv, "")
	if run, err := AcceptSweep("", ""); err != nil || run != nil {
		t.Fatalf("manual push acceptance = %v, %v", run, err)
	}
	t.Setenv(sweepControlEnv, `{"handle":"1","token":"fake","started":"2026-08-01T00:00:00Z"}`)
	if run, err := AcceptSweep(t.TempDir(), t.TempDir()); err == nil || run != nil {
		t.Fatalf("forged handoff acceptance = %v, %v", run, err)
	}
}

func TestSweepFinishFailureStillReleasesOwnership(t *testing.T) {
	f := newSweepFixture(t)
	if err := ensureSweepDir(f.cache); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.cache, "push.lock")
	lock, locked, err := openSweepLock(path)
	if err != nil || !locked {
		t.Fatalf("take lock = %v, %v", locked, err)
	}
	run := &SweepRun{lock: lock, cache: f.cache, started: f.now}
	if err := os.Mkdir(filepath.Join(f.cache, "push.last-run.json"), 0o700); err != nil {
		_ = run.Close()
		t.Fatal(err)
	}
	if err := run.Finish(LastRun{Finished: f.now.Add(time.Second)}); err == nil {
		t.Fatal("unsafe run record destination was accepted")
	}
	next, locked, err := openSweepLock(path)
	if err != nil || !locked {
		t.Fatalf("failed summary retained ownership: %v, %v", locked, err)
	}
	_ = next.Close()
	if err := run.Close(); err != nil {
		t.Fatalf("Close after Finish is not idempotent: %v", err)
	}
}
