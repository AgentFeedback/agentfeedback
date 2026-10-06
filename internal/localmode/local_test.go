package localmode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

func openTarget(t *testing.T) (*Target, *client.Client) {
	t.Helper()
	dir := t.TempDir()
	tg, err := Open(context.Background(), filepath.Join(dir, "data", "agentfeedback.db"), "0.0.0-test")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = tg.Close() })
	info, err := os.Stat(filepath.Join(dir, "data"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("data directory: %v, mode %v", err, info.Mode())
	}
	c, err := client.New(client.Config{Transport: tg.Transport(), APIKey: Key, CacheDir: t.TempDir(), NoSpool: true, Version: "0.0.0-test"})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	return tg, c
}

func TestRoundTrip(t *testing.T) {
	_, c := openTarget(t)
	ctx := context.Background()
	body := []byte(`{"kind":"friction","summary":"local round trip","payload":{"category":"test"}}`)
	o := c.Submit(ctx, body)
	if o.Outcome != client.OutcomeSubmitted || o.ID < 1 {
		t.Fatalf("submit: %+v", o)
	}
	res, err := c.Do(ctx, http.MethodGet, "/api/v1/submissions", nil, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed struct {
		Submissions []struct {
			ID      int64  `json:"id"`
			Summary string `json:"summary"`
		} `json:"submissions"`
	}
	if err := json.Unmarshal(res.Body, &listed); err != nil || len(listed.Submissions) != 1 || listed.Submissions[0].ID != o.ID {
		t.Fatalf("list body %s (%v)", res.Body, err)
	}
	if res.RequestID == "" {
		t.Error("no request id")
	}
	if ct := res.Status; ct != 200 {
		t.Errorf("status %d", ct)
	}

	stream, _, err := c.Stream(ctx, "/api/v1/export", nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	data, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], `"export_complete":true`) {
		t.Fatalf("export lines: %q", lines)
	}

	_, err = c.Do(ctx, http.MethodGet, "/api/v1/submissions/999", nil, nil)
	var ae *client.APIError
	if !asAPIError(err, &ae) || ae.Status != 404 || ae.Code != "not_found" {
		t.Fatalf("missing id: %v", err)
	}
}

func TestNotMounted(t *testing.T) {
	_, c := openTarget(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/health"}, {http.MethodGet, "/ready"}, {http.MethodGet, "/metrics"},
		{http.MethodPost, "/mcp"}, {http.MethodPost, "/mcp/proj"},
	} {
		_, err := c.Do(context.Background(), tc.method, tc.path, nil, []byte(`{}`))
		var ae *client.APIError
		if !asAPIError(err, &ae) || ae.Status != 404 {
			t.Errorf("%s %s: %v, want 404", tc.method, tc.path, err)
		}
	}
	res, err := c.Do(context.Background(), http.MethodGet, "/api/v1/meta", nil, nil)
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	var meta struct {
		Features []string `json:"features"`
	}
	_ = json.Unmarshal(res.Body, &meta)
	for _, f := range meta.Features {
		if f == "mcp" {
			t.Errorf("meta lists mcp: %v", meta.Features)
		}
	}
}

func TestStat(t *testing.T) {
	dir := t.TempDir()
	if err := Stat(filepath.Join(dir, "none.db")); err != ErrNoDatabase {
		t.Errorf("missing: %v", err)
	}
	if err := Stat(dir); err == nil || err == ErrNoDatabase {
		t.Errorf("directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Stat(filepath.Join(dir, "x.db")); err != nil {
		t.Errorf("file: %v", err)
	}
}

func asAPIError(err error, target **client.APIError) bool {
	ae, ok := err.(*client.APIError)
	if ok {
		*target = ae
	}

	return ok
}

// handlerTransport is a transport over any handler, for the failure paths.
func handlerTransport(h http.Handler) http.RoundTripper {
	t := &Target{handler: h}

	return t.Transport()
}

func TestTransportPanics(t *testing.T) {
	before := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("early") })
	req, _ := http.NewRequest(http.MethodGet, "http://local/x", nil)
	if _, err := handlerTransport(before).RoundTrip(req); err == nil || !strings.Contains(err.Error(), "handler panic: early") {
		t.Fatalf("panic before headers: %v", err)
	}

	after := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("partial"))
		panic("late")
	})
	resp, err := handlerTransport(after).RoundTrip(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("panic after headers: %v %+v", err, resp)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(data) != "partial" || err == nil || !strings.Contains(err.Error(), "handler panic: late") {
		t.Fatalf("body %q err %v", data, err)
	}
}

func TestTransportCancellation(t *testing.T) {
	unblocked := make(chan struct{})
	stall := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("head"))
		// The second write blocks until the reader goes away.
		_, err := w.Write([]byte("tail"))
		if err == nil {
			t.Error("the write after cancellation succeeded")
		}
		close(unblocked)
	})
	target := &Target{handler: stall}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://local/x", nil)
	resp, err := target.Transport().RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "head" {
		t.Fatalf("first read: %q %v", buf, err)
	}
	cancel()
	<-unblocked
	if _, err := io.ReadAll(resp.Body); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("read after cancel: %v", err)
	}
	// Close returns once the handler is gone; with no database it has nothing
	// else to close.
	target.wg.Wait()

	closed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("head"))
		if _, err := w.Write([]byte("tail")); err == nil {
			t.Error("the write after Close succeeded")
		}
	})
	req, _ = http.NewRequest(http.MethodGet, "http://local/x", nil)
	resp, err = handlerTransport(closed).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}
