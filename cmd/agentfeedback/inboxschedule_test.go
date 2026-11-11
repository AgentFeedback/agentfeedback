package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

func TestInboxScheduleFairRetries(t *testing.T) {
	isolate(t)
	db := openDB(t, filepath.Join(t.TempDir(), "srv.db"))
	svc := core.New(db, core.Config{Version: clientVersion().Version, Features: api.Features})
	handler := api.New(api.Config{Service: svc, DB: db, APIKey: testKey}).Handler()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var item struct {
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(body, &item); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		seen = append(seen, item.Summary)
		mu.Unlock()
		if item.Summary == "a" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"busy","message":"retry"}`))
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(envURL, srv.URL)
	t.Setenv(envAPIKey, testKey)
	dir := inboxDir(t)
	for _, n := range []string{"c", "a", "b"} {
		writeFile(t, filepath.Join(dir, n+".json"), `{"kind":"friction","summary":"`+n+`","payload":{"category":"documentation"}}`, 0o600)
	}
	for i, want := range []inboxReport{{Pending: 3}, {Ingested: 2, Pending: 1}, {Pending: 1}} {
		rep, r := ingestReport(t)
		if r.code != 0 || rep != want {
			t.Fatalf("pass %d: %+v %+v, want %+v", i, rep, r, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(seen, []string{"a", "b", "c", "a", "a"}) {
		t.Fatalf("requests %v", seen)
	}
	mustExist(t, filepath.Join(dir, "a.json"))
	mustBeGone(t, filepath.Join(dir, "b.json"))
	mustBeGone(t, filepath.Join(dir, "c.json"))
}

func TestInboxScheduleRemovedTargetAndWrap(t *testing.T) {
	for _, tc := range []struct {
		cursor string
		want   []string
	}{
		{"b.json", []string{"c.json", "a.json"}},
		{"z.json", []string{"a.json", "c.json"}},
		{"", []string{"a.json", "c.json"}},
	} {
		names := []string{"c.json", "a.json"}
		orderInbox(names, tc.cursor)
		if !reflect.DeepEqual(names, tc.want) {
			t.Fatalf("cursor %q: %v", tc.cursor, names)
		}
	}
}

func TestInboxScheduleRejectsUnsafeState(t *testing.T) {
	for _, state := range []string{"malformed", "traversal", "symlink", "lock symlink", "directory"} {
		t.Run(state, func(t *testing.T) {
			isolate(t)
			dir := inboxDir(t)
			writeFile(t, filepath.Join(dir, "a.json"), inboxEnvelope, 0o600)
			outside := filepath.Join(t.TempDir(), "outside")
			writeFile(t, outside, `"a.json"`, 0o600)
			switch state {
			case "malformed":
				writeFile(t, filepath.Join(dir, inboxCursor), "{", 0o600)
			case "traversal":
				writeFile(t, filepath.Join(dir, inboxCursor), `"../a.json"`, 0o600)
			case "symlink", "lock symlink":
				name := inboxCursor
				if state == "lock symlink" {
					name = inboxLock
				}
				if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
					t.Skip(err)
				}
			case "directory":
				if err := os.Mkdir(filepath.Join(dir, inboxCursor), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			_, r := ingestReport(t)
			if r.code == 0 || !strings.Contains(r.stderr, "scheduler") {
				t.Fatalf("unsafe state accepted: %+v", r)
			}
			mustExist(t, filepath.Join(dir, "a.json"))
			got, err := os.ReadFile(outside)
			if err != nil || string(got) != `"a.json"` {
				t.Fatalf("outside file changed: %q %v", got, err)
			}
		})
	}
}

func TestInboxScheduleConcurrentPasses(t *testing.T) {
	isolate(t)
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var item struct {
			Summary string `json:"summary"`
		}
		if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
			t.Error(err)
		}
		mu.Lock()
		seen = append(seen, item.Summary)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"busy","message":"retry"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(envURL, srv.URL)
	t.Setenv(envAPIKey, testKey)
	dir := inboxDir(t)
	for _, n := range []string{"a", "b"} {
		writeFile(t, filepath.Join(dir, n+".json"), `{"kind":"friction","summary":"`+n+`"}`, 0o600)
	}
	skipStartupPass = true
	c, err := apiClient(os.Getenv, modeFlags{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			rep, err := ingestInbox(context.Background(), c, dataRoot(t), func(string) {})
			if err != nil || rep.Pending != 2 {
				t.Errorf("pass %+v %v", rep, err)
			}
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(seen, []string{"a", "b"}) {
		t.Fatalf("concurrent requests %v", seen)
	}
}

type inboxRoundTrip func(*http.Request) (*http.Response, error)

func (f inboxRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInboxScheduleDeadlineAndReplay(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	for _, name := range []string{"a.json", "b.json"} {
		writeFile(t, filepath.Join(dir, name), inboxEnvelope, 0o600)
	}
	db := openDB(t, filepath.Join(t.TempDir(), "srv.db"))
	svc := core.New(db, core.Config{Version: clientVersion().Version, Features: api.Features})
	handler := api.New(api.Config{Service: svc, DB: db, APIKey: testKey}).Handler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A failed move has the same durable state as a crash after acceptance:
	// the original keyed file remains for replay.
	writeFile(t, filepath.Join(dir, inboxDone), "blocked", 0o600)
	transport := inboxRoundTrip(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		cancel()
		resp := rec.Result()
		resp.Request = r
		return resp, nil
	})
	c, err := client.New(client.Config{APIKey: testKey, DataDir: dataRoot(t), CacheDir: t.TempDir(), Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := ingestInbox(ctx, c, dataRoot(t), func(string) {})
	if err != nil || rep.Ingested != 1 || rep.Pending != 1 {
		t.Fatalf("short pass: %+v %v", rep, err)
	}
	if err := os.Remove(filepath.Join(dir, inboxDone)); err != nil {
		t.Fatal(err)
	}
	// A new client reloads the durable cursor and reaches b before replaying a.
	c, err = client.New(client.Config{APIKey: testKey, DataDir: dataRoot(t), CacheDir: t.TempDir(), Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	rep, err = ingestInbox(context.Background(), c, dataRoot(t), func(string) {})
	if err != nil || rep != (inboxReport{Ingested: 1, Duplicates: 1}) {
		t.Fatalf("replay: %+v %v", rep, err)
	}
	for _, name := range []string{"a.json", "b.json"} {
		mustBeGone(t, filepath.Join(dir, name))
	}
}

func TestInboxSchedulePublicationFailure(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	writeFile(t, filepath.Join(dir, "a.json"), inboxEnvelope, 0o600)
	transport := inboxRoundTrip(func(r *http.Request) (*http.Response, error) {
		if err := os.Mkdir(filepath.Join(dir, inboxCursor), 0o700); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(http.StatusServiceUnavailable)
		_, _ = rec.Write([]byte(`{"error":"busy","message":"retry"}`))
		resp := rec.Result()
		resp.Request = r
		return resp, nil
	})
	c, err := client.New(client.Config{APIKey: testKey, DataDir: dataRoot(t), CacheDir: t.TempDir(), Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := ingestInbox(context.Background(), c, dataRoot(t), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "cannot save progress") || rep.Pending != 1 {
		t.Fatalf("publication: %+v %v", rep, err)
	}
	mustExist(t, filepath.Join(dir, "a.json"))
}

func TestInboxScheduleCanceledLockWait(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	writeFile(t, filepath.Join(dir, "a.json"), inboxEnvelope, 0o600)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := lockInbox(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		rep, err := ingestInbox(ctx, nil, dataRoot(t), func(string) {})
		if rep != (inboxReport{Pending: 1}) {
			t.Errorf("canceled pass: %+v", rep)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("canceled pass: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled pass waited for the held lock")
	}
	mustExist(t, filepath.Join(dir, "a.json"))
	mustBeGone(t, filepath.Join(dir, inboxCursor))
}

func TestInboxScheduleEmptyMetadataNoWrite(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeInboxCursor(root, "gone.json"); err != nil {
		t.Fatal(err)
	}
	lock, err := lockInbox(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"flush", "ingest"} {
		if r := runCLI(t, "", command); r.code != 0 {
			t.Fatalf("%s: %+v", command, r)
		}
		mustBeGone(t, filepath.Join(dataRoot(t), "agentfeedback.db"))
	}
	writeFile(t, filepath.Join(dir, inboxCursor), "{", 0o600)
	if r := runCLI(t, "", "ingest"); r.code == 0 || !strings.Contains(r.stderr, "scheduler") {
		t.Fatalf("empty malformed scheduler: %+v", r)
	}
}

func TestInboxScheduleHoldDoesNotAdvance(t *testing.T) {
	isolate(t)
	dir := inboxDir(t)
	for _, name := range []string{"a.json", "b.json"} {
		writeFile(t, filepath.Join(dir, name), `{"kind":"friction","summary":"`+name+`"}`, 0o600)
	}
	var seen []string
	transport := inboxRoundTrip(func(r *http.Request) (*http.Response, error) {
		var body struct{ Summary string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, body.Summary)
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(http.StatusUnauthorized)
		_, _ = rec.Write([]byte(`{"error":"unauthorized","message":"wrong key"}`))
		resp := rec.Result()
		resp.Request = r
		return resp, nil
	})
	c, err := client.New(client.Config{APIKey: testKey, DataDir: dataRoot(t), CacheDir: t.TempDir(), Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		rep, err := ingestInbox(context.Background(), c, dataRoot(t), func(string) {})
		if err != nil || rep != (inboxReport{Pending: 2}) {
			t.Fatalf("held pass: %+v %v", rep, err)
		}
	}
	if !reflect.DeepEqual(seen, []string{"a.json", "a.json"}) {
		t.Fatalf("Hold advanced: %v", seen)
	}
	mustBeGone(t, filepath.Join(dir, inboxCursor))
}
