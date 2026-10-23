package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
)

// binaryEnv is an environment for the test binary run as agentfeedback: a
// fresh HOME and XDG directories and no AGENT_FEEDBACK_* variable, plus
// extra.
func binaryEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "HOME" || strings.HasPrefix(name, "XDG_") || strings.HasPrefix(name, "AGENT_FEEDBACK_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, runMainEnv+"=1", "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir(),
		"XDG_CACHE_HOME="+t.TempDir(), "XDG_DATA_HOME="+t.TempDir())

	return append(env, extra...)
}

// binaryCommand runs the test binary as agentfeedback with args in env.
func binaryCommand(t *testing.T, env []string, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = env

	return cmd
}

func toolText(t *testing.T, cs *sdk.ClientSession, name string, args any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: %d content blocks", name, len(res.Content))
	}
	text, ok := res.Content[0].(*sdk.TextContent)
	if !ok || res.IsError {
		t.Fatalf("%s: error %v, content %+v", name, res.IsError, res.Content[0])
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(text.Text), &v); err != nil {
		t.Fatalf("%s: %v: %s", name, err, text.Text)
	}

	return v
}

// TestMcp_StdioOnLocalDatabase runs agentfeedback mcp as a process and drives
// it over its real stdin and stdout: initialize, tools/list, submit, list and
// mark; the row is then in the local database the CLI reads.
func TestMcp_StdioOnLocalDatabase(t *testing.T) {
	env := binaryEnv(t)
	var stderr bytes.Buffer
	cmd := binaryCommand(t, env, "mcp")
	cmd.Stderr = &stderr
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v; stderr %q", err, stderr.String())
	}
	if init := cs.InitializeResult(); init.ServerInfo.Name != "agentfeedback" || init.ServerInfo.Version != clientVersion().Version {
		t.Errorf("server info %+v", init.ServerInfo)
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 9 {
		t.Fatalf("tools/list: %v, %+v", err, tools)
	}
	if st := toolText(t, cs, "sessions_list", map[string]any{}); st["stores"] == nil {
		t.Errorf("sessions_list: %+v", st)
	}
	sub := toolText(t, cs, "submit_feedback", map[string]any{"kind": "friction", "summary": "stdio mcp row", "project": "p1",
		"payload": map[string]any{"category": "tooling", "details": "d", "suggested_fix": "f"}})
	id := sub["submission"].(map[string]any)["id"].(float64)
	if list := toolText(t, cs, "list_submissions", map[string]any{"limit": 10}); list["total"].(float64) != 1 {
		t.Errorf("list: %+v", list)
	}
	toolText(t, cs, "mark_processed", map[string]any{"ids": []any{id}, "processed": true, "verdict": "fixed"})
	if err := cs.Close(); err != nil {
		t.Fatalf("close: %v; stderr %q", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr %q", stderr.String())
	}

	out, err := binaryCommand(t, env, "list", "--json", "--processed").Output()
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var page struct {
		Submissions []struct {
			ID      float64 `json:"id"`
			Summary string  `json:"summary"`
		} `json:"submissions"`
	}
	if err := json.Unmarshal(out, &page); err != nil || len(page.Submissions) != 1 || page.Submissions[0].ID != id || page.Submissions[0].Summary != "stdio mcp row" {
		t.Fatalf("list --json %v: %s", err, out)
	}
}

// mcpProcess starts agentfeedback mcp in env and connects to it over its
// stdin and stdout; stderr collects its stderr.
func mcpProcess(t *testing.T, env []string) (cs *sdk.ClientSession, stderr *bytes.Buffer) {
	t.Helper()
	stderr = &bytes.Buffer{}
	cmd := binaryCommand(t, env, "mcp")
	cmd.Stderr = stderr
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v; stderr %q", err, stderr.String())
	}
	t.Cleanup(func() { _ = cs.Close() })

	return cs, stderr
}

// toolList is the tools/list entries by name, each re-encoded with sorted
// keys.
func toolList(t *testing.T, cs *sdk.ClientSession) map[string]string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, tool := range res.Tools {
		b, _ := json.Marshal(tool)
		var v any
		_ = json.Unmarshal(b, &v)
		b, _ = json.Marshal(v)
		out[tool.Name] = string(b)
	}

	return out
}

// relayServer serves the real v1 handler with a throwaway key over a fresh
// database and returns its URL and service.
func relayServer(t *testing.T) (string, *core.Service) {
	t.Helper()
	db := openDB(t, filepath.Join(t.TempDir(), "relay.db"))
	svc := newService(db)
	srv := httptest.NewServer(api.New(api.Config{Service: svc, DB: db, APIKey: testKey}).Handler())
	t.Cleanup(srv.Close)

	return srv.URL, svc
}

// noKeyIn fails when the key appears in out or in any file under dirs.
func noKeyIn(t *testing.T, out string, dirs ...string) {
	t.Helper()
	if strings.Contains(out, testKey) {
		t.Errorf("the key is in the output: %q", out)
	}
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(testKey)) {
				t.Errorf("the key is in %s", p)
			}

			return nil
		})
	}
}

// envValue is the value of name in env.
func envValue(env []string, name string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == name {
			v = val
		}
	}

	return v
}

// TestMcp_RelayToServer: with a server configured, mcp relays to its /mcp:
// the tools/list is the local stdio server's, schemas included; a call is
// forwarded (the row is on the server), a server tool error comes back as
// it is, the session tools answer locally, and the key appears in no
// output and no file.
func TestMcp_RelayToServer(t *testing.T) {
	url, svc := relayServer(t)
	local, _ := mcpProcess(t, binaryEnv(t))
	env := binaryEnv(t, envURL+"="+url, envAPIKey+"="+testKey)
	cs, stderr := mcpProcess(t, env)

	relayTools, localTools := toolList(t, cs), toolList(t, local)
	if !maps.Equal(relayTools, localTools) {
		t.Fatalf("relay tools/list differs from local mode:\nrelay: %v\nlocal: %v", relayTools, localTools)
	}
	t.Logf("relay tools/list: %v", slices.Sorted(maps.Keys(relayTools)))
	if init := cs.InitializeResult(); init.ServerInfo.Version != clientVersion().Version || !strings.Contains(init.Instructions, url) {
		t.Errorf("initialize: %+v", init.ServerInfo)
	}

	sub := toolText(t, cs, "submit_feedback", map[string]any{"kind": "friction", "summary": "relayed row", "project": "p1",
		"payload": map[string]any{"category": "tooling", "details": "d", "suggested_fix": "f"}})
	id := int64(sub["submission"].(map[string]any)["id"].(float64))
	if rec, err := svc.Get(context.Background(), id); err != nil || rec.Summary != "relayed row" {
		t.Fatalf("row on the server: %+v %v", rec, err)
	}
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "get_submission", Arguments: map[string]any{"id": 999}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*sdk.TextContent).Text, `"not_found"`) {
		t.Fatalf("server tool error: %v %+v", err, res)
	}
	if st := toolText(t, cs, "sessions_list", map[string]any{}); st["stores"] == nil {
		t.Errorf("sessions_list: %+v", st)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("close: %v; stderr %q", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr %q", stderr.String())
	}
	noKeyIn(t, stderr.String(), envValue(env, "HOME"), envValue(env, "XDG_CONFIG_HOME"), envValue(env, "XDG_CACHE_HOME"), envValue(env, "XDG_DATA_HOME"))
}

// TestMcp_RelayStartupFailure: a wrong key, a server that is not there, a
// URL the client refuses and no key at all fail before serving, exit 1 on
// stderr only, and show neither the key nor credentials in the URL.
func TestMcp_RelayStartupFailure(t *testing.T) {
	url, _ := relayServer(t)
	for _, tt := range []struct {
		name string
		env  []string
		want string
	}{
		{"wrong key", []string{envURL + "=" + url, envAPIKey + "=wrong-" + testKey}, "the MCP endpoint " + url + "/mcp cannot be reached"},
		{"not there", []string{envURL + "=http://127.0.0.1:1", envAPIKey + "=" + testKey}, "the MCP endpoint http://127.0.0.1:1/mcp cannot be reached"},
		{"credentials in the URL", []string{envURL + "=https://user:secret@feedback.example.com/", envAPIKey + "=" + testKey}, "credentials"},
		{"no key", []string{envURL + "=" + url}, "no API key is set"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := binaryCommand(t, binaryEnv(t, tt.env...), "mcp")
			var stdout, stderr bytes.Buffer
			cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(""), &stdout, &stderr
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("exit %v; stderr %q", err, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout %q", stdout.String())
			}
			t.Logf("stderr: %s", stderr.String())
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr %q lacks %q", stderr.String(), tt.want)
			}
			if strings.Contains(stderr.String(), "secret") {
				t.Errorf("stderr shows the URL's credentials: %q", stderr.String())
			}
			noKeyIn(t, stderr.String())
		})
	}
}

// TestMcp_ServerEntryRelaysWithoutConfig: install --mcp against a server
// with no config.toml writes an entry running mcp --server <URL>, and that
// command, run with the key in the environment and no URL anywhere else,
// talks to the server, not the local database.
func TestMcp_ServerEntryRelaysWithoutConfig(t *testing.T) {
	url, svc := relayServer(t)
	e := newInstallEnv(t)
	if r, _ := installRun(t, "install", "cursor", "--mcp", "--server", url); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	data, _ := os.ReadFile(filepath.Join(e.home, ".cursor", "mcp.json"))
	v, err := harness.GetMember(data, []string{"mcpServers"}, "agentfeedback")
	if err != nil || v == nil {
		t.Fatalf("no entry: %v\n%s", err, data)
	}
	var entry struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal(v, &entry); err != nil || entry.Command != e.exe || !slices.Equal(entry.Args, []string{"mcp", "--server", url}) {
		t.Fatalf("entry %s", v)
	}
	if strings.Contains(string(data), testKey) {
		t.Fatalf("the key is in mcp.json")
	}
	t.Logf("cursor entry: %s", v)

	env := binaryEnv(t, envAPIKey+"="+testKey)
	var stderr bytes.Buffer
	cmd := binaryCommand(t, env, entry.Args...)
	cmd.Stderr = &stderr
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v; stderr %q", err, stderr.String())
	}
	sub := toolText(t, cs, "submit_feedback", map[string]any{"kind": "friction", "summary": "via the entry", "payload": map[string]any{"category": "tooling"}})
	id := int64(sub["submission"].(map[string]any)["id"].(float64))
	if rec, err := svc.Get(context.Background(), id); err != nil || rec.Summary != "via the entry" {
		t.Fatalf("row on the server: %+v %v", rec, err)
	}
	_ = cs.Close()
	if _, err := os.Stat(filepath.Join(envValue(env, "XDG_DATA_HOME"), "agentfeedback", "agentfeedback.db")); err == nil {
		// The session tools open the data-directory database; the row must
		// not be in it.
		out, _ := binaryCommand(t, append(env, "AGENT_FEEDBACK_URL="), "list", "--json", "--local").Output()
		if strings.Contains(string(out), "via the entry") {
			t.Fatalf("the row went to the local database: %s", out)
		}
	}
	noKeyIn(t, stderr.String())
}

// TestMcp_RelayBadRemoteTool: a server advertising a tool with a
// non-object input schema and an invalid name is served without it, with
// one warning on stderr naming it, and the key appears in no output.
func TestMcp_RelayBadRemoteTool(t *testing.T) {
	s := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "1"}, nil)
	s.AddTool(&sdk.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "{}"}}}, nil
	})
	s.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			res, err := next(ctx, method, req)
			if lt, ok := res.(*sdk.ListToolsResult); ok && err == nil {
				lt.Tools = append(lt.Tools, &sdk.Tool{Name: "bad schema " + testKey, InputSchema: map[string]any{"type": "string"}},
					&sdk.Tool{Name: "bad name " + testKey, InputSchema: map[string]any{"type": "object"}})
			}
			return res, err
		}
	})
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("X-Api-Key") != testKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cs, stderr := mcpProcess(t, binaryEnv(t, envURL+"="+srv.URL, envAPIKey+"="+testKey))
	tools := toolList(t, cs)
	if _, ok := tools["echo"]; !ok {
		t.Fatalf("tools %v", slices.Sorted(maps.Keys(tools)))
	}
	for name := range tools {
		if strings.HasPrefix(name, "bad schema") {
			t.Errorf("served %q", name)
		}
	}
	_ = cs.Close()
	msg := stderr.String()
	t.Logf("stderr: %s", msg)
	if strings.Count(msg, "warning:") != 1 || !strings.Contains(msg, `"bad schema [REDACTED]"`) {
		t.Errorf("stderr %q", msg)
	}
	noKeyIn(t, msg)
}
