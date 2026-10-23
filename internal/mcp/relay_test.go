package mcp_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
)

// connectRelay builds the relay over remote and opens an SDK client session
// on it over in-memory transports.
func connectRelay(t *testing.T, remote *sdk.ClientSession, f mcp.ContextFrom) *sdk.ClientSession {
	t.Helper()
	srv, err := mcp.NewRelay(context.Background(), mcp.RelayConfig{
		Remote: remote, Sessions: &fakeSessions{}, ContextFrom: f,
		ErrorBody: api.ToolErrorBody, CallContext: api.WithNewRequestID,
		Instructions: "local instructions", Version: "4.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	serverT, clientT := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, serverT) }()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), clientT, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect relay: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		<-done
	})
	return cs
}

// TestRelay_ServesWhatStdioServes: the relay over a server's /mcp lists
// exactly the tools the local stdio server lists, schemas included, serves
// the server's instructions, forwards calls and the server's tool errors
// unchanged, resolves context_from locally and forwards the body, and
// answers a call the server never gets as an unavailable tool error.
func TestRelay_ServesWhatStdioServes(t *testing.T) {
	e := newEnv(t, api.Config{})
	remote := e.connect(t, "/mcp", "")
	f := &fakeContextFrom{}
	cs := connectRelay(t, remote, f)

	relayTools, stdioTools := toolsByName(t, cs), toolsByName(t, connectStdio(t))
	normal := func(m map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = normalTool(v, nil)
		}
		return out
	}
	if !maps.Equal(normal(relayTools), normal(stdioTools)) {
		t.Fatalf("relay tools/list differs from stdio:\nrelay: %v\nstdio: %v", relayTools, stdioTools)
	}
	if got, want := cs.InitializeResult().Instructions, strings.TrimRight(remote.InitializeResult().Instructions, "\n")+"\n\n"+mcp.LocalGuidance; got != want {
		t.Errorf("relay instructions are not the server's followed by the local guidance:\n%s", got)
	}

	b, isErr := call(t, cs, mcp.ToolSubmit, map[string]any{"kind": "friction", "summary": "relayed", "payload": map[string]any{"category": "tooling"}})
	if isErr {
		t.Fatalf("submit: %s", b)
	}
	if b, isErr = call(t, cs, mcp.ToolSubmit, map[string]any{"kind": "friction", "summary": "from a span", "context_from": "codex:s#1"}); isErr {
		t.Fatalf("submit with context_from: %s", b)
	}
	if sub := object(t, b)["submission"].(map[string]any); sub["key"] != "from-the-session" || len(f.args) != 1 {
		t.Fatalf("context_from: %s, provider calls %d", b, len(f.args))
	}
	if b, isErr = call(t, cs, mcp.ToolList, map[string]any{"limit": 10}); isErr || object(t, b)["total"].(float64) != 2 {
		t.Fatalf("list on the server: %v %s", isErr, b)
	}
	if b, isErr = call(t, cs, mcp.ToolGet, map[string]any{"id": 999}); !isErr || object(t, b)["error"] != "not_found" {
		t.Fatalf("server tool error: %v %s", isErr, b)
	}
	if b, isErr = call(t, cs, mcp.ToolSessionsList, nil); isErr || string(b) != `{"tool":"list"}` {
		t.Fatalf("local session tool: %v %s", isErr, b)
	}

	_ = remote.Close()
	b, isErr = call(t, cs, mcp.ToolStats, nil)
	if body := object(t, b); !isErr || body["error"] != "unavailable" || body["message"] == "" {
		t.Fatalf("closed remote: %v %s", isErr, b)
	}
}

// fakeRemote is a stateful MCP server over HTTP: tool echo answers its
// arguments, tool boom answers a JSON-RPC error, tools/list also advertises
// tools with no or a non-object input schema, and drop makes it answer 404
// to the first session from then on, as a server that ended that session.
type fakeRemote struct {
	srv     *httptest.Server
	drop    atomic.Bool
	mu      sync.Mutex
	first   string
	calls   atomic.Int32
	connect atomic.Int32
}

func newFakeRemote(t *testing.T) *fakeRemote {
	t.Helper()
	f := &fakeRemote{}
	s := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "1"}, &sdk.ServerOptions{Instructions: "fake instructions"})
	s.AddTool(&sdk.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		f.calls.Add(1)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(req.Params.Arguments)}},
			StructuredContent: req.Params.Arguments}, nil
	})
	s.AddTool(&sdk.Tool{Name: "boom", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return nil, errors.New("unreachable")
	})
	s.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "tools/call" && req.(*sdk.CallToolRequest).Params.Name == "boom" {
				return nil, &jsonrpc.Error{Code: -32602, Message: "boom refused " + testKey}
			}
			res, err := next(ctx, method, req)
			if lt, ok := res.(*sdk.ListToolsResult); ok && err == nil {
				lt.Tools = append(lt.Tools, &sdk.Tool{Name: "no_schema"}, &sdk.Tool{Name: "string_schema", InputSchema: map[string]any{"type": "string"}})
			}
			return res, err
		}
	})
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, nil)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("Mcp-Session-Id")
		f.mu.Lock()
		first := f.first
		f.mu.Unlock()
		if id != "" && id == first && f.drop.Load() {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
		if issued := w.Header().Get("Mcp-Session-Id"); issued != "" && id == "" {
			f.connect.Add(1)
			f.mu.Lock()
			if f.first == "" {
				f.first = issued
			}
			f.mu.Unlock()
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRemote) dial(ctx context.Context) (*sdk.ClientSession, error) {
	return sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ClientOptions{Logger: mcp.DiscardLogger}).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: f.srv.URL, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
}

// relayOver builds the relay over a session with f and connects to it.
func relayOver(t *testing.T, f *fakeRemote, reconnect bool, warn func(string)) *sdk.ClientSession {
	t.Helper()
	remote, err := f.dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	cfg := mcp.RelayConfig{
		Remote: remote, ErrorBody: api.ToolErrorBody, Version: "4.0.0", Warn: warn,
		Redact: func(msg string) string { return strings.ReplaceAll(msg, testKey, "[REDACTED]") },
	}
	if reconnect {
		cfg.Reconnect = f.dial
	}
	srv, err := mcp.NewRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	serverT, clientT := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, serverT) }()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), clientT, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		<-done
	})
	return cs
}

// TestRelay_SkipsUnusableTools: a remote tool with no input schema or a
// non-object one is left out with one warning naming it, never a panic;
// the others are served.
func TestRelay_SkipsUnusableTools(t *testing.T) {
	var warnings []string
	cs := relayOver(t, newFakeRemote(t), false, func(m string) { warnings = append(warnings, m) })
	tools := toolsByName(t, cs)
	if _, ok := tools["echo"]; !ok || len(tools) != 2 {
		t.Fatalf("tools %v", tools)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], `"no_schema"`) || !strings.Contains(warnings[1], `"string_schema"`) {
		t.Fatalf("warnings %q", warnings)
	}
}

// TestRelay_ProtocolError: a JSON-RPC error the remote answers is a
// bad_request tool error carrying its code and message, redacted.
func TestRelay_ProtocolError(t *testing.T) {
	cs := relayOver(t, newFakeRemote(t), false, nil)
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "boom", Arguments: map[string]any{}})
	if err != nil || !res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	text := res.Content[0].(*sdk.TextContent).Text
	body := object(t, []byte(text))
	if body["error"] != "bad_request" || !strings.Contains(text, "JSON-RPC error -32602") || !strings.Contains(text, "boom refused") || strings.Contains(text, testKey) {
		t.Fatalf("body %s", text)
	}
}

// TestRelay_ReconnectsOnce: a call whose remote session the server ended is
// retried once on a new session; without a way to reconnect it is an
// unavailable tool error.
func TestRelay_ReconnectsOnce(t *testing.T) {
	f := newFakeRemote(t)
	cs := relayOver(t, f, true, nil)
	if b, isErr := call(t, cs, "echo", map[string]any{"n": 1}); isErr {
		t.Fatalf("before the drop: %s", b)
	}
	f.drop.Store(true)
	before := f.connect.Load()
	if b, isErr := call(t, cs, "echo", map[string]any{"n": 2}); isErr || string(b) != `{"n":2}` {
		t.Fatalf("after the drop: %v %s", isErr, b)
	}
	if f.connect.Load() != before+1 || f.calls.Load() != 2 {
		t.Fatalf("connects %d -> %d, calls %d", before, f.connect.Load(), f.calls.Load())
	}

	g := newFakeRemote(t)
	cs = relayOver(t, g, false, nil)
	g.drop.Store(true)
	b, isErr := call(t, cs, "echo", map[string]any{})
	if body := object(t, b); !isErr || body["error"] != "unavailable" {
		t.Fatalf("no reconnect: %v %s", isErr, b)
	}
}
