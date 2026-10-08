package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
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

// TestMcp_RemoteModeRefused: with a server URL configured, mcp refuses on
// stderr, names the server's /mcp without userinfo, query or fragment and
// writes nothing to stdout.
func TestMcp_RemoteModeRefused(t *testing.T) {
	for _, configured := range []string{"https://user:secret@feedback.example.com/", "https://user:secret@feedback.example.com/?token=secret#frag"} {
		cmd := binaryCommand(t, binaryEnv(t, envURL+"="+configured), "mcp")
		var stdout, stderr bytes.Buffer
		cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(""), &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("%s: exit %v; stderr %q", configured, err, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: stdout %q", configured, stdout.String())
		}
		msg := stderr.String()
		for _, want := range []string{"serves the local database", "(" + envURL + ")", "https://REDACTED@feedback.example.com/mcp ", "--local"} {
			if !strings.Contains(msg, want) {
				t.Errorf("stderr %q lacks %q", msg, want)
			}
		}
		for _, leak := range []string{"secret", "token", "frag", "?", "#"} {
			if strings.Contains(msg, leak) {
				t.Errorf("stderr shows %q: %q", leak, msg)
			}
		}
	}
}
