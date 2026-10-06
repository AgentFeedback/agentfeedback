package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

var toolNames = []string{mcp.ToolGetSchema, mcp.ToolGet, mcp.ToolList, mcp.ToolMark, mcp.ToolStats, mcp.ToolSubmit}

// readOnly is each tool's readOnlyHint; none is destructive or open-world.
var readOnly = map[string]bool{
	mcp.ToolSubmit: false, mcp.ToolList: true, mcp.ToolGet: true,
	mcp.ToolStats: true, mcp.ToolMark: false, mcp.ToolGetSchema: true,
}

func properties(t *testing.T, tool *sdk.Tool) map[string]any {
	t.Helper()
	b, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil || s.Type != "object" {
		t.Fatalf("%s input schema %s: %v", tool.Name, b, err)
	}
	return s.Properties
}

func object(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("not an object: %v: %s", err, b)
	}
	return v
}

// TestClient_Conformance drives every tool with the SDK's own client over
// the latest (sessionless) protocol and a legacy one with the initialize
// handshake, on /mcp and on /mcp/{project}.
func TestClient_Conformance(t *testing.T) {
	for _, version := range []string{"", "2025-06-18"} {
		for _, tc := range []struct{ name, path, preset string }{{"plain", "/mcp", ""}, {"preset", "/mcp/%20Web%20App%20", "Web App"}} {
			t.Run(fmt.Sprintf("v%s-%s", version, tc.name), func(t *testing.T) {
				e := newEnv(t, api.Config{})
				cs := e.connect(t, tc.path, version)
				init := cs.InitializeResult()
				want := "2026-07-28"
				if version != "" {
					want = version
				}
				if init.ProtocolVersion != want {
					t.Errorf("protocol %s, want %s", init.ProtocolVersion, want)
				}
				if init.ServerInfo.Name != "agentfeedback" || init.ServerInfo.Version != "4.0.0" {
					t.Errorf("server info %+v", init.ServerInfo)
				}
				instr, err := skillgen.Render(skillgen.FormMCP, e.srv.URL)
				if err != nil || init.Instructions != string(instr) {
					t.Errorf("instructions differ from the mcp render for %s (%v)", e.srv.URL, err)
				}

				tools, err := cs.ListTools(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				for _, tool := range tools.Tools {
					names = append(names, tool.Name)
					a := tool.Annotations
					if a == nil || a.DestructiveHint == nil || a.OpenWorldHint == nil {
						t.Fatalf("%s: annotations %+v", tool.Name, a)
					}
					if a.ReadOnlyHint != readOnly[tool.Name] || *a.DestructiveHint || *a.OpenWorldHint {
						t.Errorf("%s: annotations %+v", tool.Name, a)
					}
					if tool.Description == "" {
						t.Errorf("%s: no description", tool.Name)
					}
					_, hasProject := properties(t, tool)["project"]
					scoped := slices.Contains([]string{mcp.ToolSubmit, mcp.ToolList, mcp.ToolStats}, tool.Name)
					if scoped && hasProject == (tc.preset != "") {
						t.Errorf("%s: project property present %v with preset %q", tool.Name, hasProject, tc.preset)
					}
				}
				if !slices.Equal(names, toolNames) {
					t.Fatalf("tools %v, want %v", names, toolNames)
				}

				sub := map[string]any{"kind": "friction", "summary": "the build cache is stale", "machine": "m1",
					"payload": map[string]any{"category": "tooling", "details": "d", "suggested_fix": "f"}}
				if tc.preset == "" {
					sub["project"] = "p1"
				}
				b, isErr := call(t, cs, mcp.ToolSubmit, sub)
				if isErr {
					t.Fatalf("submit: %s", b)
				}
				rec := object(t, b)["submission"].(map[string]any)
				id := rec["id"].(float64)
				wantProject := "p1"
				if tc.preset != "" {
					wantProject = tc.preset
				}
				if rec["project"] != wantProject {
					t.Errorf("stored project %v, want %q", rec["project"], wantProject)
				}
				if b, isErr = call(t, cs, mcp.ToolSubmit, map[string]any{"summary": "other project", "project": "elsewhere"}); isErr != (tc.preset != "") {
					t.Errorf("submit with project under preset %q: error %v: %s", tc.preset, isErr, b)
				}
				if tc.preset != "" {
					// Rows of another project exist; the preset filters them out.
					if _, err := e.svc.Create(context.Background(), []byte(`{"summary":"elsewhere","project":"elsewhere"}`)); err != nil {
						t.Fatal(err)
					}
				}

				b, isErr = call(t, cs, mcp.ToolList, map[string]any{"limit": 10, "include": "payload"})
				if isErr {
					t.Fatalf("list: %s", b)
				}
				list := object(t, b)
				if tc.preset != "" {
					if list["total"].(float64) != 1 {
						t.Errorf("list under preset: total %v: %s", list["total"], b)
					}
					if _, isErr := call(t, cs, mcp.ToolList, map[string]any{"project": "x"}); !isErr {
						t.Errorf("list accepted project under a preset")
					}
					if _, isErr := call(t, cs, mcp.ToolStats, map[string]any{"project": "x"}); !isErr {
						t.Errorf("stats accepted project under a preset")
					}
				}
				if b, isErr = call(t, cs, mcp.ToolGet, map[string]any{"id": id}); isErr {
					t.Fatalf("get: %s", b)
				}
				b, isErr = call(t, cs, mcp.ToolStats, map[string]any{"by": "kind,project", "top": 5})
				if isErr {
					t.Fatalf("stats: %s", b)
				}
				if tc.preset != "" && object(t, b)["total"].(float64) != 1 {
					t.Errorf("stats under preset: %s", b)
				}
				if b, isErr = call(t, cs, mcp.ToolMark, map[string]any{"ids": []any{id}, "processed": true, "verdict": "fixed"}); isErr {
					t.Fatalf("mark: %s", b)
				}
				if b, isErr = call(t, cs, mcp.ToolGetSchema, nil); isErr {
					t.Fatalf("schema list: %s", b)
				}
				if b, isErr = call(t, cs, mcp.ToolGetSchema, map[string]any{"kind": "friction", "version": 1}); isErr {
					t.Fatalf("schema: %s", b)
				}
			})
		}
	}
}

func TestClient_InstructionsAppendOperatorText(t *testing.T) {
	e := newEnv(t, api.Config{PublicURL: "https://feedback.example.com", MCPInstructions: "  File one report per task.\n"})
	cs := e.connect(t, "/mcp", "")
	instr, err := skillgen.Render(skillgen.FormMCP, "https://feedback.example.com")
	if err != nil {
		t.Fatal(err)
	}
	want, err := mcp.Instructions("https://feedback.example.com", "File one report per task.")
	if err != nil {
		t.Fatal(err)
	}
	if got := cs.InitializeResult().Instructions; got != want || got == string(instr) {
		t.Errorf("instructions:\n%s\nwant:\n%s", got, want)
	}
	if want != string(instr[:len(instr)-countTrailingNewlines(instr)])+"\n\nFile one report per task." {
		t.Errorf("operator text not appended after a blank line:\n%s", want)
	}
}

func countTrailingNewlines(b []byte) int {
	n := 0
	for n < len(b) && b[len(b)-1-n] == '\n' {
		n++
	}
	return n
}

// TestHTTP_LongHostRendersPlaceholder: a request-derived base URL over the
// limit is not embedded in the instructions (the mcp form without a server
// URL names none).
func TestHTTP_LongHostRendersPlaceholder(t *testing.T) {
	e := newEnv(t, api.Config{})
	req, err := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = strings.Repeat("a", 250) + ".example"
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"instructions":"# AgentFeedback`) || strings.Contains(string(b), "aaaa") {
		t.Errorf("initialize with a long Host: %d %s", resp.StatusCode, b)
	}
}
