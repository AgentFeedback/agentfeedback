package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// answer503 answers every request with a 503 problem body.
func answer503(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 503,
		Header:     http.Header{"Content-Type": []string{"application/problem+json"}},
		Body:       io.NopCloser(strings.NewReader(`{"title":"down","status":503,"message":"later"}`)),
		Request:    r,
	}, nil
}

func TestNewTransport(t *testing.T) {
	var got *http.Request
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r

		return answer503(r)
	})
	c, err := New(Config{APIKey: testKey, DataDir: t.TempDir(), CacheDir: t.TempDir(), Transport: rt})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Submit(context.Background(), []byte(`{"kind":"friction"}`))
	if got == nil {
		t.Fatal("the RoundTripper saw no request")
	}
	if u := got.URL.String(); u != LocalURL+submissionsPath {
		t.Errorf("URL = %q, want %q", u, LocalURL+submissionsPath)
	}
	if got.Header.Get("Authorization") != "Bearer "+testKey || got.Header.Get("X-API-Key") != testKey {
		t.Errorf("auth headers = %q, %q", got.Header.Get("Authorization"), got.Header.Get("X-API-Key"))
	}

	if _, err := New(Config{APIKey: testKey, DataDir: t.TempDir(), CacheDir: t.TempDir(), Transport: rt, HTTP: &http.Client{}}); err == nil {
		t.Error("Transport plus HTTP was accepted")
	}
	if _, err := New(Config{URL: "ftp://host", APIKey: testKey, DataDir: t.TempDir(), CacheDir: t.TempDir(), Transport: rt}); err == nil {
		t.Error("Transport plus an invalid URL was accepted")
	}
}

// TestSubmitLocalSpools: a submission the local database cannot take now
// (503 through the Transport) is spooled with destination local and no
// backoff, outcome spooled, and the next Flush over the same transport
// delivers it, while a Flush bound to a server leaves it in place.
func TestSubmitLocalSpools(t *testing.T) {
	data, cache, stderr := t.TempDir(), t.TempDir(), &bytes.Buffer{}
	var up atomic.Bool
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !up.Load() {
			return answer503(r)
		}
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
	o := c.Submit(context.Background(), []byte(`{"kind":"friction","summary":"s"}`))
	files := af1Files(t, SpoolDir(data))
	if o.Outcome != OutcomeSpooled || o.ExitCode() != 0 || len(files) != 1 {
		t.Fatalf("outcome %+v, spool files %v", o, files)
	}
	en := readEntry(t, filepath.Join(SpoolDir(data), files[0]))
	if en.Destination != LocalDestination || !en.NotBefore.Equal(t0) || en.Attempts != 1 {
		t.Fatalf("entry destination %q not_before %v attempts %d; want local, no backoff", en.Destination, en.NotBefore, en.Attempts)
	}
	if strings.Contains(stderr.String(), `"summary":"s"`) {
		t.Error("the body was echoed although it was spooled")
	}

	// A flush bound to a server never takes a local entry.
	remote, err := New(Config{URL: "http://h.example", APIKey: testKey, DataDir: data, CacheDir: cache, HTTP: &http.Client{Transport: roundTripFunc(answer503)}, Stderr: stderr, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	if rep := remote.Flush(context.Background()); rep != (FlushReport{OtherDestination: 1}) {
		t.Fatalf("remote flush report %+v", rep)
	}
	if !strings.Contains(stderr.String(), "is for local, not http://h.example; left in place; run agentfeedback flush --local") {
		t.Errorf("mismatch not named: %s", stderr)
	}

	// Still down: the local pass stops after the first entry, which keeps
	// its place without a backoff.
	c.Submit(context.Background(), []byte(`{"kind":"friction","summary":"t"}`))
	if rep := c.Flush(context.Background()); rep.Pending != 1 || rep.Stopped != "server_503" || rep.Flushed != 0 {
		t.Fatalf("flush while down %+v", rep)
	}
	for _, name := range af1Files(t, SpoolDir(data)) {
		if en := readEntry(t, filepath.Join(SpoolDir(data), name)); !en.NotBefore.Equal(t0) {
			t.Errorf("%s rescheduled to %v, want no backoff", name, en.NotBefore)
		}
	}

	up.Store(true)
	if rep := c.Flush(context.Background()); rep != (FlushReport{Flushed: 2}) {
		t.Fatalf("flush once up %+v", rep)
	}
	if left := af1Files(t, SpoolDir(data)); len(left) != 0 {
		t.Errorf("spool left %v", left)
	}
}

// TestFlushDestinationBinding: a spool holding entries for two servers is
// flushed by a client of one of them: its entry goes, the other stays and is
// named, even past its retention (only its own destination's flush expires
// it); an entry without a destination is quarantined, never sent, with an
// error line in the client log.
func TestFlushDestinationBinding(t *testing.T) {
	var hits atomic.Int64
	srv := acceptingServer(t, &hits)
	e := newTestEnv(t, srv.URL, nil)
	e.spoolEntry(t, entryName(1), "friction", "mine", t0, t0.Add(-time.Minute))
	other := e.c.newEntry("friction", "theirs", []byte(`{"kind":"friction","key":"theirs"}`), t0.Add(-31*24*time.Hour))
	other.Destination = "https://other.example"
	other.NotBefore = t0.Add(-time.Minute)
	if err := e.c.writeEntry(SpoolDir(e.data), entryName(2), other); err != nil {
		t.Fatal(err)
	}
	legacy := e.c.newEntry("friction", "legacy", []byte(`{"kind":"friction","key":"legacy"}`), t0)
	legacy.Destination = ""
	if err := e.c.writeEntry(SpoolDir(e.data), entryName(3), legacy); err != nil {
		t.Fatal(err)
	}

	rep := e.c.Flush(context.Background())
	if want := (FlushReport{Flushed: 1, OtherDestination: 1}); rep != want || hits.Load() != 1 {
		t.Fatalf("report %+v, want %+v; hits %d; stderr %s", rep, want, hits.Load(), e.stderr)
	}
	if left := af1Files(t, SpoolDir(e.data)); len(left) != 1 || left[0] != entryName(2) {
		t.Errorf("spool left %v, want only the other server's entry", left)
	}
	if !strings.Contains(e.stderr.String(), entryName(2)+" is for https://other.example, not "+srv.URL+"; left in place; run agentfeedback flush --server https://other.example") {
		t.Errorf("other entry not named: %s", e.stderr)
	}
	if rej := af1Files(t, RejectedDir(e.data)); len(rej) != 1 || rej[0] != entryName(3) {
		t.Errorf("rejected/ %v, want the destination-less entry quarantined", rej)
	}
	if !strings.Contains(e.stderr.String(), "names no destination") {
		t.Errorf("quarantine not explained: %s", e.stderr)
	}
	var logged bool
	for _, l := range logLines(t, e.cache) {
		logged = logged || (l["reason"] == "quarantined" && l["key"] == entryName(3))
	}
	if !logged {
		t.Error("the quarantine left no client log line")
	}

	// A URL change never delivers the other server's entry.
	hits.Store(0)
	e2 := newTestEnv(t, srv.URL+"/", nil)
	e2.data = e.data
	e2.c.dataDir = e.data
	if rep := e2.c.Flush(context.Background()); rep != (FlushReport{OtherDestination: 1}) || hits.Load() != 0 {
		t.Fatalf("after a URL change: %+v, hits %d", rep, hits.Load())
	}
}
