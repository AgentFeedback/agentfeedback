package mcp_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/localmode"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
)

// connectStdio opens an SDK client session on the stdio construction (the
// server localmode builds for agentfeedback mcp) over in-memory transports,
// on a fresh local database.
func connectStdio(t *testing.T) *sdk.ClientSession {
	t.Helper()
	target, err := localmode.Open(context.Background(), filepath.Join(t.TempDir(), "local.db"), "4.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	srv, err := target.MCPServer()
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

// toolsJSON is tools/list as JSON: names, descriptions, input schemas and
// annotations, in order.
func toolsJSON(t *testing.T, cs *sdk.ClientSession) string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(res.Tools)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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
	if stdio, httpTools := toolsJSON(t, cs), toolsJSON(t, e.connect(t, "/mcp", "")); stdio != httpTools {
		t.Fatalf("tools/list over stdio differs from /mcp:\nstdio: %s\nhttp:  %s", stdio, httpTools)
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
