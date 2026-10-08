package detect

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUpdate_FileModesAndCounting(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "agentfeedback")
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Update(cache, "claude-code", "s/1", time.Now().Add(10*time.Second), func(st *State) { st.Total++ }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	path := StatePath(cache, "claude-code", "s/1")
	if !strings.HasPrefix(filepath.Base(path), "s_1-") || len(filepath.Base(path)) != len("s_1-")+16+len(".json") {
		t.Errorf("path %s", path)
	}
	var total int
	if err := Update(cache, "claude-code", "s/1", time.Now().Add(10*time.Second), func(st *State) { total = st.Total }); err != nil || total != 20 {
		t.Errorf("total %d %v", total, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file %v %v", info, err)
	}
	for _, d := range []string{SessionsDir(cache), filepath.Dir(path)} {
		if info, err := os.Stat(d); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("dir %s %v %v", d, info, err)
		}
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "npm") {
		t.Error("command text stored")
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Errorf("lock file: %v", err)
	}
	if files, _ := os.ReadDir(filepath.Dir(path)); len(files) != 2 {
		t.Errorf("files left beside the state: %v", files)
	}
}

// TestStatePath_Distinct: ids that sanitise alike, or share a long prefix,
// never share a file.
func TestStatePath_Distinct(t *testing.T) {
	long := strings.Repeat("p", 128)
	for _, pair := range [][2]string{{"s/1", "s_1"}, {long + "a", long + "b"}} {
		a, b := StatePath("c", "pi", pair[0]), StatePath("c", "pi", pair[1])
		if a == b {
			t.Errorf("%q and %q share %s", pair[0], pair[1], a)
		}
		if n := len(filepath.Base(a)); n > fileIDMax+1+16+len(".json") {
			t.Errorf("name %d bytes", n)
		}
	}
	cache := t.TempDir()
	deadline := time.Now().Add(10 * time.Second)
	if err := Update(cache, "pi", "s/1", deadline, func(st *State) { st.Total = 1 }); err != nil {
		t.Fatal(err)
	}
	var total int
	if err := Update(cache, "pi", "s_1", deadline, func(st *State) { total = st.Total }); err != nil || total != 0 {
		t.Errorf("s_1 read s/1's state: %d %v", total, err)
	}
}

// TestUpdate_DeadlineBoundsWrite: once the deadline has passed, before or
// after fn, Update writes nothing and returns ErrLocked.
func TestUpdate_DeadlineBoundsWrite(t *testing.T) {
	cache := t.TempDir()
	path := StatePath(cache, "pi", "s")
	if err := Update(cache, "pi", "s", time.Now().Add(-time.Second), func(*State) { t.Error("fn called past the deadline") }); !errors.Is(err, ErrLocked) {
		t.Fatalf("past deadline: %v", err)
	}
	err := Update(cache, "pi", "s", time.Now().Add(50*time.Millisecond), func(st *State) {
		st.Total++
		time.Sleep(100 * time.Millisecond)
	})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("deadline during fn: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state written: %v", err)
	}
	if tmps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".state-*.tmp")); len(tmps) != 0 {
		t.Errorf("temporary files left: %v", tmps)
	}
}

func TestUpdate_EmptySessionKeepsNothing(t *testing.T) {
	cache := t.TempDir()
	called := false
	if err := Update(cache, "claude-code", "", time.Now().Add(time.Second), func(*State) { called = true }); err != nil || called {
		t.Fatalf("%v %v", called, err)
	}
	if _, err := os.Stat(SessionsDir(cache)); !os.IsNotExist(err) {
		t.Errorf("sessions dir: %v", err)
	}
}

func TestSafeID(t *testing.T) {
	if got := SafeID("../a b/" + strings.Repeat("x", 200)); strings.ContainsAny(got, "/ ") || len(got) != 128 {
		t.Errorf("%q", got)
	}
}

func TestPrune(t *testing.T) {
	cache := t.TempDir()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	old := StatePath(cache, "pi", "old")
	fresh := StatePath(cache, "pi", "fresh")
	for _, id := range []string{"old", "fresh"} {
		if err := Update(cache, "pi", id, time.Now().Add(10*time.Second), func(*State) {}); err != nil {
			t.Fatal(err)
		}
	}
	filed := filepath.Join(cache, "filed")
	if err := os.MkdirAll(filed, 0o700); err != nil {
		t.Fatal(err)
	}
	oldMark, freshMark := filepath.Join(filed, "old"), filepath.Join(filed, "fresh")
	for _, p := range []string{oldMark, freshMark} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(oldMark, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(freshMark, now.Add(-6*24*time.Hour), now.Add(-6*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fresh, now.Add(-6*24*time.Hour), now.Add(-6*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(old)
	oldTmp, freshTmp := filepath.Join(dir, ".state-1.tmp"), filepath.Join(dir, ".state-2.tmp")
	orphanLock, freshOrphanLock := filepath.Join(dir, "gone-0.json.lock"), filepath.Join(dir, "new-0.json.lock")
	for _, p := range []string{oldTmp, freshTmp, orphanLock, freshOrphanLock} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{oldTmp, orphanLock, fresh + ".lock"} {
		if err := os.Chtimes(p, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Prune(cache, now, filed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old kept: %v", err)
	}
	if _, err := os.Stat(old + ".lock"); !os.IsNotExist(err) {
		t.Errorf("old lock kept: %v", err)
	}
	if _, err := os.Stat(fresh + ".lock"); err != nil {
		t.Errorf("old lock of a fresh session removed: %v", err)
	}
	for _, p := range []string{oldTmp, orphanLock} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s kept: %v", p, err)
		}
	}
	for _, p := range []string{freshTmp, freshOrphanLock} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
	if _, err := os.Stat(oldMark); !os.IsNotExist(err) {
		t.Errorf("old marker kept: %v", err)
	}
	if _, err := os.Stat(freshMark); err != nil {
		t.Errorf("fresh marker removed: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh removed: %v", err)
	}
	// Within a day of the last pass nothing is pruned.
	if err := os.Chtimes(fresh, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := Prune(cache, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("pruned within a day: %v", err)
	}
	if err := Prune(cache, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Errorf("not pruned after a day: %v", err)
	}
}
