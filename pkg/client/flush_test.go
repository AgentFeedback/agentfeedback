package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestFlushRetention is AC3's retention half: expiry by kind, rejected/
// pruning, and rejected/ never sent.
func TestFlushRetention(t *testing.T) {
	var hits atomic.Int64
	srv := acceptingServer(t, &hits)
	e := newTestEnv(t, srv.URL, nil)
	e.spoolEntry(t, entryName(1), "friction", "f-young", t0.Add(-(19*time.Hour + 59*time.Minute)), t0.Add(-time.Minute))
	e.spoolEntry(t, entryName(2), "Friction", "f-old", t0.Add(-(20*time.Hour + time.Minute)), t0.Add(-time.Minute))
	e.spoolEntry(t, entryName(3), "event", "e-young", t0.Add(-29*24*time.Hour), t0.Add(-time.Minute))
	e.spoolEntry(t, entryName(4), "event", "e-old", t0.Add(-(30*24*time.Hour + time.Hour)), t0.Add(-time.Minute))

	for i, age := range map[int]time.Duration{5: 31 * 24 * time.Hour, 6: 29 * 24 * time.Hour} {
		en := e.c.newEntry("friction", "r"+strconv.Itoa(i), []byte(`{"kind":"friction"}`), t0.Add(-age))
		at := t0.Add(-age)
		en.RejectedAt = &at
		en.Status = 400
		if err := e.c.writeEntry(RejectedDir(e.data), entryName(i), en); err != nil {
			t.Fatal(err)
		}
	}
	// A bash-era file and an unreadable af1- file.
	if err := os.WriteFile(filepath.Join(SpoolDir(e.data), "friction-1.json"), []byte(`{"kind":"friction"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(SpoolDir(e.data), entryName(9)), []byte(`garbage`), 0o600); err != nil {
		t.Fatal(err)
	}

	rep := e.c.Flush(context.Background())
	want := FlushReport{Flushed: 2, Expired: 2}
	if rep != want {
		t.Fatalf("report = %+v, want %+v; stderr %s", rep, want, e.stderr)
	}
	if hits.Load() != 2 {
		t.Errorf("requests = %d, want 2 (rejected/ must never be sent)", hits.Load())
	}
	if got := af1Files(t, SpoolDir(e.data)); len(got) != 0 {
		t.Errorf("spool left %v", got)
	}
	if !exists(filepath.Join(SpoolDir(e.data), "friction-1.json")) {
		t.Error("bash-era file touched")
	}
	rej := af1Files(t, RejectedDir(e.data))
	if len(rej) != 2 || rej[0] != entryName(6) || rej[1] != entryName(9) {
		t.Errorf("rejected/ = %v, want the 29-day entry and the quarantined file", rej)
	}
	var expired int
	for _, l := range logLines(t, e.cache) {
		if l["reason"] == "expired" {
			expired++
		}
	}
	if expired != 2 {
		t.Errorf("expired log lines = %d", expired)
	}
}

// TestFlushScheduling is AC3's scheduling half: deferred, in-flight claims,
// a stop on network failure, and 409.
func TestFlushScheduling(t *testing.T) {
	t.Run("deferred", func(t *testing.T) {
		var hits atomic.Int64
		e := newTestEnv(t, acceptingServer(t, &hits).URL, nil)
		e.spoolEntry(t, entryName(1), "event", "k", t0, t0.Add(time.Minute))
		if rep := e.c.Flush(context.Background()); rep != (FlushReport{Deferred: 1}) || hits.Load() != 0 {
			t.Errorf("report %+v hits %d", rep, hits.Load())
		}
	})
	t.Run("inflight", func(t *testing.T) {
		var hits atomic.Int64
		e := newTestEnv(t, acceptingServer(t, &hits).URL, nil)
		spool := SpoolDir(e.data)
		e.spoolEntry(t, entryName(1), "event", "stale", t0, t0)
		e.spoolEntry(t, entryName(2), "event", "fresh", t0, t0)
		rename := func(i int, at time.Time) {
			base := entryName(i)
			to := base + ".inflight-" + strconv.FormatInt(at.UnixNano(), 10)
			if err := os.Rename(filepath.Join(spool, base), filepath.Join(spool, to)); err != nil {
				t.Fatal(err)
			}
		}
		rename(1, t0.Add(-2*time.Minute))
		rename(2, t0.Add(-10*time.Second))
		rep := e.c.Flush(context.Background())
		if rep != (FlushReport{Flushed: 1}) || hits.Load() != 1 {
			t.Errorf("report %+v hits %d", rep, hits.Load())
		}
		if left := af1Files(t, spool); len(left) != 1 {
			t.Errorf("spool = %v, want the fresh claim only", left)
		}
	})
	t.Run("stop on network", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		e := newTestEnv(t, srv.URL, nil)
		e.spoolEntry(t, entryName(1), "event", "a", t0, t0)
		e.spoolEntry(t, entryName(2), "event", "b", t0, t0)
		second := filepath.Join(SpoolDir(e.data), entryName(2))
		before, _ := os.ReadFile(second)
		rep := e.c.Flush(context.Background())
		if rep != (FlushReport{Pending: 1, Stopped: "network"}) {
			t.Fatalf("report %+v", rep)
		}
		after, _ := os.ReadFile(second)
		if string(before) != string(after) {
			t.Error("second entry changed")
		}
		first := readEntry(t, filepath.Join(SpoolDir(e.data), entryName(1)))
		if first.Attempts != 2 || !first.NotBefore.Equal(t0.Add(time.Minute)) || first.LastError != "network" {
			t.Errorf("first = %+v", first)
		}
		if lines := logLines(t, e.cache); len(lines) != 1 || lines[0]["outcome"] != OutcomeSpooled || lines[0]["reason"] != "network" {
			t.Errorf("log = %v", lines)
		}
	})
	t.Run("stop on unauthorized", func(t *testing.T) {
		var hits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			problemBody(w, 401, "no")
		}))
		defer srv.Close()
		e := newTestEnv(t, srv.URL, nil)
		e.spoolEntry(t, entryName(1), "event", "a", t0, t0)
		e.spoolEntry(t, entryName(2), "event", "b", t0, t0)
		rep := e.c.Flush(context.Background())
		if rep != (FlushReport{Pending: 1, Stopped: "unauthorized"}) || hits.Load() != 1 {
			t.Fatalf("report %+v hits %d", rep, hits.Load())
		}
		if first := readEntry(t, filepath.Join(SpoolDir(e.data), entryName(1))); !first.NotBefore.Equal(t0) {
			t.Errorf("not_before = %v", first.NotBefore)
		}
	})
	t.Run("409 and 400", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, key := sentHead(r)
			code := 400
			if key == "a" {
				code = 409
			}
			problemBody(w, code, fmt.Sprint(code))
		}))
		defer srv.Close()
		e := newTestEnv(t, srv.URL, nil)
		e.spoolEntry(t, entryName(1), "event", "a", t0, t0)
		e.spoolEntry(t, entryName(2), "event", "b", t0, t0)
		rep := e.c.Flush(context.Background())
		if rep != (FlushReport{Mismatched: 1, Rejected: 1}) {
			t.Fatalf("report %+v", rep)
		}
		if en := readEntry(t, filepath.Join(RejectedDir(e.data), entryName(1))); en.Status != 409 {
			t.Errorf("status = %d", en.Status)
		}
		if left := af1Files(t, SpoolDir(e.data)); len(left) != 0 {
			t.Errorf("spool = %v", left)
		}
		lines := logLines(t, e.cache)
		if len(lines) != 2 || lines[0]["outcome"] != OutcomeMismatch || lines[1]["outcome"] != OutcomeRejected {
			t.Errorf("log = %v", lines)
		}
	})
	t.Run("not due at claim", func(t *testing.T) {
		var hits atomic.Int64
		e := newTestEnv(t, acceptingServer(t, &hits).URL, nil)
		e.spoolEntry(t, entryName(1), "event", "a", t0, t0.Add(30*time.Minute))
		calls := 0
		e.c.now = func() time.Time {
			calls++
			if calls == 1 {
				return t0.Add(time.Hour) // the pass lists it as due
			}

			return t0 // by the claim it is not
		}
		rep := e.c.Flush(context.Background())
		if rep != (FlushReport{Deferred: 1}) || hits.Load() != 0 {
			t.Fatalf("report %+v hits %d", rep, hits.Load())
		}
		if left := af1Files(t, SpoolDir(e.data)); len(left) != 1 || left[0] != entryName(1) {
			t.Errorf("spool = %v", left)
		}
	})
}

// TestFlushHygiene: quarantined files age by mtime from quarantine, and
// stale .tmp- files go while the bash client's .tmp. files stay.
func TestFlushHygiene(t *testing.T) {
	var hits atomic.Int64
	e := newTestEnv(t, acceptingServer(t, &hits).URL, nil)
	spool, rejected := SpoolDir(e.data), RejectedDir(e.data)
	for _, d := range []string{spool, rejected} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, age time.Duration) {
		if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, t0.Add(-age), t0.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(spool, entryName(1)), 40*24*time.Hour)
	write(filepath.Join(rejected, entryName(2)), 31*24*time.Hour)
	write(filepath.Join(rejected, entryName(3)), 29*24*time.Hour)
	write(filepath.Join(spool, ".tmp-old"), 25*time.Hour)
	write(filepath.Join(rejected, ".tmp-old"), 25*time.Hour)
	write(filepath.Join(spool, ".tmp-new"), 23*time.Hour)
	write(filepath.Join(spool, ".tmp.bash"), 25*time.Hour)

	e.c.Flush(context.Background())
	if got := af1Files(t, rejected); len(got) != 2 || got[0] != entryName(1) || got[1] != entryName(3) {
		t.Fatalf("rejected/ = %v", got)
	}
	st, err := os.Stat(filepath.Join(rejected, entryName(1)))
	if err != nil || !st.ModTime().Equal(t0) {
		t.Errorf("quarantined mtime = %v, want %v", st.ModTime(), t0)
	}
	for path, want := range map[string]bool{
		filepath.Join(spool, ".tmp-old"):    false,
		filepath.Join(rejected, ".tmp-old"): false,
		filepath.Join(spool, ".tmp-new"):    true,
		filepath.Join(spool, ".tmp.bash"):   true,
	} {
		if exists(path) != want {
			t.Errorf("%s exists = %v, want %v", path, !want, want)
		}
	}

	// 31 days after quarantine it goes.
	e.now = t0.Add(31 * 24 * time.Hour)
	e.c.Flush(context.Background())
	if got := af1Files(t, rejected); len(got) != 0 {
		t.Errorf("rejected/ = %v", got)
	}
	if hits.Load() != 0 {
		t.Errorf("requests = %d", hits.Load())
	}
}

func TestHasDue(t *testing.T) {
	e := newTestEnv(t, "http://127.0.0.1:1", nil)
	if HasDue(e.data, t0) {
		t.Fatal("empty spool reported due")
	}
	e.spoolEntry(t, entryName(1), "event", "later", t0, t0.Add(time.Minute))
	e.spoolEntry(t, entryName(2), "friction", "old", t0.Add(-21*time.Hour), t0.Add(-time.Hour))
	if HasDue(e.data, t0) {
		t.Fatal("deferred and expired entries reported due")
	}
	e.spoolEntry(t, entryName(4), "event", "now", t0, t0)
	if !HasDue(e.data, t0) {
		t.Fatal("a due entry not reported")
	}
	if err := os.Remove(filepath.Join(SpoolDir(e.data), entryName(4))); err != nil {
		t.Fatal(err)
	}
	// An entry that cannot be read is not due: Flush skips it too.
	unreadable := filepath.Join(SpoolDir(e.data), entryName(5))
	e.spoolEntry(t, entryName(5), "event", "locked", t0, t0)
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(unreadable); err == nil {
		t.Skip("running with permissions that read a mode-0 file")
	}
	if HasDue(e.data, t0) {
		t.Fatal("an unreadable entry reported due")
	}
	if err := os.Remove(unreadable); err != nil {
		t.Fatal(err)
	}
	// An undecodable entry is due: Flush quarantines it.
	if err := os.WriteFile(filepath.Join(SpoolDir(e.data), entryName(3)), []byte(`garbage`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !HasDue(e.data, t0) {
		t.Fatal("an undecodable entry not reported due")
	}
	if got := af1Files(t, SpoolDir(e.data)); len(got) != 3 {
		t.Fatalf("HasDue changed the spool: %v", got)
	}
}
