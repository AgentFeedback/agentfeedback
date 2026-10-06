package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

const testKey = "test-key"

type env struct {
	srv *httptest.Server
	svc *core.Service
	reg *prometheus.Registry
	// lastID is the X-Request-Id of the last response the SDK client got.
	lastID atomic.Value
}

func (e *env) lastRequestID() string {
	id, _ := e.lastID.Load().(string)
	return id
}

// newEnv serves the real api.Handler over a fresh temporary database.
func newEnv(t *testing.T, cfg api.Config, opts ...core.Option) *env {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "mcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := core.New(db, core.Config{Version: "4.0.0", Features: api.Features}, opts...)
	cfg.Service, cfg.DB, cfg.APIKey, cfg.Registry = svc, db, testKey, prometheus.NewRegistry()
	srv := httptest.NewServer(api.New(cfg).Handler())
	t.Cleanup(srv.Close)
	return &env{srv: srv, svc: svc, reg: cfg.Registry}
}

// keyTransport adds the API key to every request and records each
// response's X-Request-Id.
type keyTransport struct {
	header, value string
	last          *atomic.Value
}

func (k keyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(k.header, k.value)
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && k.last != nil {
		k.last.Store(resp.Header.Get("X-Request-Id"))
	}
	return resp, err
}

// connect opens an SDK client session on path; version "" is the latest
// protocol.
func (e *env) connect(t *testing.T, path, version string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	transport := &sdk.StreamableClientTransport{
		Endpoint:   e.srv.URL + path,
		HTTPClient: &http.Client{Transport: keyTransport{"Authorization", "Bearer " + testKey, &e.lastID}},
	}
	var opts *sdk.ClientSessionOptions
	if version != "" {
		opts = &sdk.ClientSessionOptions{ProtocolVersion: version}
	}
	cs, err := client.Connect(context.Background(), transport, opts)
	if err != nil {
		t.Fatalf("connect %s (%q): %v", path, version, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call calls a tool and returns its text content, the exact JSON the
// server wrote; the structured content must hold the same value (the SDK
// client decodes it into float64 numbers, so it is compared as such).
func call(t *testing.T, cs *sdk.ClientSession, name string, args any) (json.RawMessage, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: %d content blocks", name, len(res.Content))
	}
	text, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("%s: content %T", name, res.Content[0])
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if json.Unmarshal([]byte(text.Text), &a) != nil || json.Unmarshal(structured, &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("%s: text content %s differs from structured content %s", name, text.Text, structured)
	}
	return json.RawMessage(text.Text), res.IsError
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (e *env) do(t *testing.T, method, path string, body []byte, header map[string]string) response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{resp.StatusCode, resp.Header, b}
}

var authed = map[string]string{"Authorization": "Bearer " + testKey}

// normalize decodes JSON for comparison, dropping request_id, which differs
// per request.
func normalize(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %v: %s", err, b)
	}
	if m, ok := v.(map[string]any); ok {
		delete(m, "request_id")
	}
	return v
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	x, err1 := json.Marshal(normalize(t, a))
	y, err2 := json.Marshal(normalize(t, b))
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}
