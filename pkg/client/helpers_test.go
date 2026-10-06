package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "sekrit-key-123"

var t0 = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// testEnv is a client over a temp cache with a settable clock.
type testEnv struct {
	c      *Client
	data   string
	cache  string
	stderr *bytes.Buffer
	now    time.Time
}

func newTestEnv(t *testing.T, url string, hc *http.Client) *testEnv {
	t.Helper()
	e := &testEnv{data: t.TempDir(), cache: t.TempDir(), stderr: &bytes.Buffer{}, now: t0}
	c, err := New(Config{URL: url, APIKey: testKey, DataDir: e.data, CacheDir: e.cache, HTTP: hc, Now: func() time.Time { return e.now }, Stderr: e.stderr, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.c = c

	return e
}

// af1Files lists the af1- names in dir.
func af1Files(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		if strings.HasPrefix(d.Name(), entryPrefix) {
			out = append(out, d.Name())
		}
	}

	return out
}

func readEntry(t *testing.T, path string) entry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e entry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("%s: %v", path, err)
	}

	return e
}

// sentHead reads kind and key from a request body.
func sentHead(r *http.Request) (kind, key string) {
	data, _ := io.ReadAll(r.Body)
	var h struct {
		Kind string `json:"kind"`
		Key  string `json:"key"`
	}
	_ = json.Unmarshal(data, &h)

	return h.Kind, h.Key
}

func created(w http.ResponseWriter, status int, id int64, kind, key string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"submission":{"id":%d,"kind":%q,"key":%q},"warnings":[]}`, id, kind, key)
}

func problemBody(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":"x","message":%q,"request_id":"body-rid","details":[{"code":"c1","pointer":"/a","message":"m1"}]}`, msg)
}

// acceptingServer answers 201 naming what was sent and counts requests.
func acceptingServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		kind, key := sentHead(r)
		created(w, http.StatusCreated, 7, kind, key)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// spoolEntry writes a ready entry bound to the client's destination into
// spool/ under name.
func (e *testEnv) spoolEntry(t *testing.T, name, kind, key string, created, notBefore time.Time) {
	t.Helper()
	en := e.c.newEntry(kind, key, []byte(fmt.Sprintf(`{"kind":%q,"key":%q,"summary":"s"}`, kind, key)), created)
	en.NotBefore = notBefore
	en.Attempts = 1
	if err := e.c.writeEntry(SpoolDir(e.data), name, en); err != nil {
		t.Fatal(err)
	}
}

func entryName(i int) string { return fmt.Sprintf("af1-20260101T000000.%09dZ-00000000.json", i) }

func logLines(t *testing.T, cache string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(LogPath(cache))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}

	return out
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }
