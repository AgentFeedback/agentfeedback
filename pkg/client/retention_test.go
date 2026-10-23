package client

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestLocalFrictionKeeps30Days: a friction bound to LocalDestination keeps
// the 30-day retention, so one 2 days old is delivered, not expired.
func TestLocalFrictionKeeps30Days(t *testing.T) {
	data, cache, stderr := t.TempDir(), t.TempDir(), &bytes.Buffer{}
	var hits atomic.Int64
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		kind, key := sentHead(r)
		rec := httptest.NewRecorder()
		created(rec, http.StatusCreated, 5, kind, key)
		resp := rec.Result()
		resp.Request = r

		return resp, nil
	})
	c, err := New(Config{APIKey: testKey, DataDir: data, CacheDir: cache, Transport: rt, Stderr: stderr, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	en := c.newEntry("friction", "local-old", []byte(`{"kind":"friction","key":"local-old","summary":"s"}`), t0.Add(-48*time.Hour))
	if en.Destination != LocalDestination {
		t.Fatalf("destination %q, want local", en.Destination)
	}
	if err := c.writeEntry(SpoolDir(data), entryName(1), en); err != nil {
		t.Fatal(err)
	}
	if !HasDue(data, t0) {
		t.Fatal("a due local friction not reported due")
	}
	if rep := c.Flush(context.Background()); rep != (FlushReport{Flushed: 1}) || hits.Load() != 1 {
		t.Fatalf("report %+v hits %d; stderr %s", rep, hits.Load(), stderr)
	}
	if left := af1Files(t, SpoolDir(data)); len(left) != 0 {
		t.Errorf("spool left %v", left)
	}
}

// TestExpiredEntryIsDue: a server-bound friction past its 20 h retention,
// even one whose not_before is later, is due for a flush bound to its
// destination, which deletes and logs it; HasDue, which knows no
// destination, does not count it, so an entry bound elsewhere never stays
// due forever.
func TestExpiredEntryIsDue(t *testing.T) {
	var hits atomic.Int64
	srv := acceptingServer(t, &hits)
	e := newTestEnv(t, srv.URL, nil)
	e.spoolEntry(t, entryName(1), "friction", "old", t0.Add(-21*time.Hour), t0.Add(time.Hour))
	if HasDue(e.data, t0) {
		t.Fatal("HasDue counts an entry past retention")
	}
	if HasDueFor(e.data, "https://elsewhere.example.test", t0) {
		t.Fatal("an entry past retention is due for another destination")
	}
	if !HasDueFor(e.data, e.c.destination, t0) {
		t.Fatal("an entry past retention not due for its destination")
	}
	if rep := e.c.Flush(context.Background()); rep != (FlushReport{Expired: 1}) || hits.Load() != 0 {
		t.Fatalf("report %+v hits %d; stderr %s", rep, hits.Load(), e.stderr)
	}
	if left := af1Files(t, SpoolDir(e.data)); len(left) != 0 {
		t.Errorf("spool left %v", left)
	}
	var logged bool
	for _, l := range logLines(t, e.cache) {
		logged = logged || (l["reason"] == "expired" && l["key"] == "old")
	}
	if !logged {
		t.Error("the expiry left no client log line")
	}
}
