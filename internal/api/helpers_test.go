package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

const testKey = "test-key"

// testEnv is a server over a fresh temporary database.
type testEnv struct {
	srv *httptest.Server
	db  *store.DB
	svc *core.Service
	// down is the server's ShuttingDown flag.
	down *atomic.Bool
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnvWith(t, Config{})
}

// newEnvWith is newEnv with cfg's PublicURL and MCPInstructions.
func newEnvWith(t *testing.T, cfg Config) *testEnv {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := core.New(db, core.Config{Version: "4.0.0", Features: Features})
	down := &atomic.Bool{}
	srv := httptest.NewServer(New(Config{Service: svc, DB: db, APIKey: testKey, ShuttingDown: down, Registry: prometheus.NewRegistry(),
		PublicURL: cfg.PublicURL, MCPInstructions: cfg.MCPInstructions}).Handler())
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, db: db, svc: svc, down: down}
}

// response is a finished response with its body read.
type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, r.body)
	}
	return v
}

// request builds a request against e; auth adds the test key as a bearer
// token.
func (e *testEnv) request(t *testing.T, method, path string, body []byte, auth bool) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+testKey)
	}
	return req
}

func send(t *testing.T, req *http.Request) response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

func (e *testEnv) do(t *testing.T, method, path, body string, auth bool) response {
	t.Helper()
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	return send(t, e.request(t, method, path, b, auth))
}

// assertError checks the Error body shape, the code, request_id against
// X-Request-Id and Cache-Control: no-store.
func assertError(t *testing.T, r response, status int, code string) map[string]any {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d: %s", r.status, status, r.body)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := r.header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	v := r.json(t)
	if v["error"] != code {
		t.Errorf("error = %v, want %s: %s", v["error"], code, r.body)
	}
	if m, _ := v["message"].(string); m == "" {
		t.Errorf("message empty: %s", r.body)
	}
	if id := r.header.Get("X-Request-Id"); id == "" || v["request_id"] != id {
		t.Errorf("request_id = %v, X-Request-Id = %q", v["request_id"], id)
	}
	if d, ok := v["details"]; ok {
		if arr, _ := d.([]any); len(arr) == 0 {
			t.Errorf("details present but empty: %s", r.body)
		}
	}
	return v
}

// assertDetail checks a 400 validation_error with one detail at pointer.
func assertDetail(t *testing.T, r response, code, pointer string) {
	t.Helper()
	v := assertError(t, r, http.StatusBadRequest, core.CodeValidation)
	details, _ := v["details"].([]any)
	if len(details) != 1 {
		t.Fatalf("details = %v, want one", v["details"])
	}
	d := details[0].(map[string]any)
	if d["pointer"] != pointer || d["code"] != code {
		t.Errorf("detail = %v, want code %s pointer %s", d, code, pointer)
	}
}

const frictionBody = `{"kind":"friction","summary":"the linter ignores its config file","machine":"workstation-a","model":"claude-fable-5-1","project":"example","payload":{"category":"tooling","details":"expected .lintrc to apply; it did not","fix_status":"none"}}`
