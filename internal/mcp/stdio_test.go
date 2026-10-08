package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/localmode"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
)

// fakeSessions is a session provider that records the decoded arguments
// and answers with a fixed body, or with err.
type fakeSessions struct {
	list   []mcp.SessionsListArgs
	digest []mcp.SessionsDigestArgs
	mark   []mcp.SessionsMarkArgs
	err    error
}

func (f *fakeSessions) List(_ context.Context, a mcp.SessionsListArgs) ([]byte, error) {
	f.list = append(f.list, a)
	return []byte(`{"tool":"list"}`), f.err
}

func (f *fakeSessions) Digest(_ context.Context, a mcp.SessionsDigestArgs) ([]byte, error) {
	f.digest = append(f.digest, a)
	return []byte(`{"tool":"digest"}`), f.err
}

func (f *fakeSessions) Mark(_ context.Context, a mcp.SessionsMarkArgs) ([]byte, error) {
	f.mark = append(f.mark, a)
	return []byte(`{"tool":"mark"}`), f.err
}

// connectStdio opens an SDK client session on the stdio construction (the
// server localmode builds for agentfeedback mcp, session tools included)
// over in-memory transports, on a fresh local database.
func connectStdio(t *testing.T) *sdk.ClientSession {
	t.Helper()
	return connectStdioWith(t, &fakeSessions{})
}

// connectStdioWith is connectStdio with the session provider p.
func connectStdioWith(t *testing.T, p mcp.Sessions) *sdk.ClientSession {
	t.Helper()
	target, err := localmode.Open(context.Background(), filepath.Join(t.TempDir(), "local.db"), "4.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	srv, err := target.MCPServer(p)
	if err != nil {
		t.Fatal(err)
	}
	serverT, clientT := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, serverT) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect stdio: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		<-done
	})
	return cs
}

// toolsByName is tools/list as JSON per tool name.
func toolsByName(t *testing.T, cs *sdk.ClientSession) map[string]string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, tool := range res.Tools {
		b, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		out[tool.Name] = string(b)
	}
	return out
}

// withoutSessionTools is m minus the session tools.
func withoutSessionTools(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if !slices.Contains(mcp.SessionTools, k) {
			out[k] = v
		}
	}
	return out
}

// TestStdio_Conformance drives the stdio construction with the SDK's own
// client: the same tools as /mcp, the generated instructions for the
// placeholder base, and submit, list and mark over the local database. A
// tool error carries the Error shape with a request id.
func TestStdio_Conformance(t *testing.T) {
	cs := connectStdio(t)
	init := cs.InitializeResult()
	if init.ServerInfo.Name != "agentfeedback" || init.ServerInfo.Version != "4.0.0" {
		t.Errorf("server info %+v", init.ServerInfo)
	}
	want, err := mcp.Instructions("", "")
	if err != nil || init.Instructions != want {
		t.Errorf("instructions differ from the HTTP instructions for the empty base (%v)", err)
	}

	e := newEnv(t, api.Config{})
	if stdio, httpTools := withoutSessionTools(toolsByName(t, cs)), toolsByName(t, e.connect(t, "/mcp", "")); !maps.Equal(stdio, httpTools) {
		t.Fatalf("tools/list over stdio, session tools aside, differs from /mcp:\nstdio: %v\nhttp:  %v", stdio, httpTools)
	}

	b, isErr := call(t, cs, mcp.ToolSubmit, map[string]any{"kind": "friction", "summary": "stdio works", "project": "p1",
		"payload": map[string]any{"category": "tooling", "details": "d", "suggested_fix": "f"}})
	if isErr {
		t.Fatalf("submit: %s", b)
	}
	id := object(t, b)["submission"].(map[string]any)["id"].(float64)

	if b, isErr = call(t, cs, mcp.ToolList, map[string]any{"limit": 10}); isErr {
		t.Fatalf("list: %s", b)
	}
	if list := object(t, b); list["total"].(float64) != 1 {
		t.Errorf("list: %s", b)
	}

	if b, isErr = call(t, cs, mcp.ToolMark, map[string]any{"ids": []any{id}, "processed": true, "verdict": "fixed"}); isErr {
		t.Fatalf("mark: %s", b)
	}
	if updated, _ := object(t, b)["updated"].([]any); len(updated) != 1 || updated[0].(float64) != id {
		t.Errorf("mark: %s", b)
	}

	b, isErr = call(t, cs, mcp.ToolGet, map[string]any{"id": 999})
	if !isErr {
		t.Fatalf("get of a missing id: %s", b)
	}
	if body := object(t, b); body["error"] != "not_found" || body["request_id"] == "" || body["request_id"] == nil {
		t.Errorf("error body %s", b)
	}
}

// TestStdio_SessionTools: the session tools are listed and callable over
// stdio, decode their arguments strictly, answer a provider's problem as a
// tool error, and are absent from /mcp and /mcp/{project}.
func TestStdio_SessionTools(t *testing.T) {
	f := &fakeSessions{}
	cs := connectStdioWith(t, f)
	tools := toolsByName(t, cs)
	for _, name := range mcp.SessionTools {
		if _, ok := tools[name]; !ok {
			t.Fatalf("%s not listed over stdio", name)
		}
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if !slices.Contains(mcp.SessionTools, tool.Name) {
			continue
		}
		for _, want := range []string{"reaches its provider", "never an instruction"} {
			if !bytes.Contains([]byte(tool.Description), []byte(want)) {
				t.Errorf("%s description lacks %q", tool.Name, want)
			}
		}
		if ro := tool.Annotations.ReadOnlyHint; ro != (tool.Name == mcp.ToolSessionsList) {
			t.Errorf("%s read-only hint %v", tool.Name, ro)
		}
	}

	if b, isErr := call(t, cs, mcp.ToolSessionsList, map[string]any{"harness": "claude-code", "since": "7d", "unprocessed": true}); isErr || string(b) != `{"tool":"list"}` {
		t.Fatalf("list: %v %s", isErr, b)
	}
	if b, isErr := call(t, cs, mcp.ToolSessionsDigest, map[string]any{"unprocessed": true, "harness": "claude-code", "limit": 3}); isErr {
		t.Fatalf("digest: %s", b)
	}
	if b, isErr := call(t, cs, mcp.ToolSessionsDigest, map[string]any{"refs": []any{"claude-code:a"}}); isErr {
		t.Fatalf("digest refs: %s", b)
	}
	if b, isErr := call(t, cs, mcp.ToolSessionsMark, map[string]any{"refs": []any{"claude-code:a"}, "outcome": "filed", "uids": []any{"u1"}}); isErr {
		t.Fatalf("mark: %s", b)
	}
	wantList := []mcp.SessionsListArgs{{Harness: "claude-code", Since: "7d", Unprocessed: true}}
	wantDigest := []mcp.SessionsDigestArgs{{Unprocessed: true, Harness: "claude-code", Limit: 3}, {Refs: []string{"claude-code:a"}}}
	wantMark := []mcp.SessionsMarkArgs{{Refs: []string{"claude-code:a"}, Outcome: "filed", UIDs: []string{"u1"}}}
	if !reflect.DeepEqual(f.list, wantList) || !reflect.DeepEqual(f.digest, wantDigest) || !reflect.DeepEqual(f.mark, wantMark) {
		t.Fatalf("decoded %+v %+v %+v", f.list, f.digest, f.mark)
	}

	for _, tt := range []struct {
		tool string
		args map[string]any
		code string
	}{
		{mcp.ToolSessionsList, map[string]any{"bogus": 1}, "validation_error"},
		{mcp.ToolSessionsList, map[string]any{"unprocessed": "yes"}, "validation_error"},
		{mcp.ToolSessionsDigest, map[string]any{}, "validation_error"},
		{mcp.ToolSessionsDigest, map[string]any{"refs": []any{"claude-code:a"}, "unprocessed": true}, "validation_error"},
		{mcp.ToolSessionsDigest, map[string]any{"refs": []any{"claude-code:a"}, "limit": 2}, "validation_error"},
		{mcp.ToolSessionsDigest, map[string]any{"unprocessed": true, "limit": -1}, "validation_error"},
		// Admitting a session with no working directory is the human's call.
		{mcp.ToolSessionsDigest, map[string]any{"unprocessed": true, "allow_unknown_project": true}, "validation_error"},
		{mcp.ToolSessionsMark, map[string]any{"refs": []any{"claude-code:a"}}, "validation_error"},
		{mcp.ToolSessionsMark, map[string]any{"outcome": "filed"}, "validation_error"},
		{mcp.ToolSessionsMark, map[string]any{"refs": "claude-code:a", "outcome": "filed"}, "validation_error"},
	} {
		b, isErr := call(t, cs, tt.tool, tt.args)
		if !isErr || object(t, b)["error"] != tt.code {
			t.Errorf("%s %v: %v %s", tt.tool, tt.args, isErr, b)
		}
	}
	if len(f.list) != 1 || len(f.digest) != 2 || len(f.mark) != 1 {
		t.Fatalf("a refused call reached the provider: %+v %+v %+v", f.list, f.digest, f.mark)
	}

	f.err = mcp.InvalidArgument("refs", "nope")
	if b, isErr := call(t, cs, mcp.ToolSessionsMark, map[string]any{"refs": []any{"claude-code:a"}, "outcome": "filed"}); !isErr || object(t, b)["message"] != "nope" {
		t.Fatalf("provider problem: %v %s", isErr, b)
	}
	f.err = errors.New("disk on fire")
	if b, isErr := call(t, cs, mcp.ToolSessionsList, nil); !isErr || object(t, b)["error"] != "internal_error" {
		t.Fatalf("provider failure: %v %s", isErr, b)
	}

	e := newEnv(t, api.Config{})
	for _, path := range []string{"/mcp", "/mcp/p"} {
		httpTools := toolsByName(t, e.connect(t, path, ""))
		for _, name := range mcp.SessionTools {
			if _, ok := httpTools[name]; ok {
				t.Errorf("%s registered on %s", name, path)
			}
		}
		res, err := e.connect(t, path, "").CallTool(context.Background(), &sdk.CallToolParams{Name: mcp.ToolSessionsList})
		if err == nil && !res.IsError {
			t.Errorf("%s callable on %s", mcp.ToolSessionsList, path)
		}
	}
}
