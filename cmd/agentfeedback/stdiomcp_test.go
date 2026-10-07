package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
)

// seedLocal is seed without the configured url, so install resolves local.
func (e installEnv) seedLocal(t *testing.T) {
	t.Helper()
	e.seed(t)
	if err := os.Remove(filepath.Join(e.home, ".config", "agentfeedback", "config.toml")); err != nil {
		t.Fatal(err)
	}
}

// stdioMember is where each JSON adapter writes its stdio entry and the
// entry, in compact form, for the binary exe.
func stdioMember(exe string) map[string]struct {
	file string
	path []string
	want string
} {
	bin := string(mustJSONString(exe))
	typed := `{"type":"stdio","command":` + bin + `,"args":["mcp"]}`
	plain := `{"command":` + bin + `,"args":["mcp"]}`
	type m = struct {
		file string
		path []string
		want string
	}

	return map[string]m{
		"cursor":      {".cursor/mcp.json", []string{"mcpServers"}, typed},
		"opencode":    {".config/opencode/opencode.jsonc", []string{"mcp"}, `{"type":"local","command":[` + bin + `,"mcp"],"enabled":true}`},
		"omp":         {".omp/agent/mcp.json", []string{"mcpServers"}, typed},
		"pi":          {".pi/agent/mcp.json", []string{"mcpServers"}, plain},
		"copilot":     {".copilot/mcp-config.json", []string{"mcpServers"}, `{"type":"local","command":` + bin + `,"args":["mcp"],"tools":["*"]}`},
		"antigravity": {".gemini/config/mcp_config.json", []string{"mcpServers"}, plain},
		"devin":       {".config/devin/mcp_config.json", []string{"mcpServers"}, plain},
		"kiro":        {".kiro/settings/mcp.json", []string{"mcpServers"}, plain},
		"cline":       {".cline/data/settings/cline_mcp_settings.json", []string{"mcpServers"}, plain},
		"amp":         {".config/amp/settings.json", []string{"amp.mcpServers"}, plain},
		"vscode":      {".config/Code/User/mcp.json", []string{"servers"}, typed},
	}
}

func mustJSONString(s string) []byte {
	data, _ := json.Marshal(s)

	return data
}

// listed is the harness's entry in install --list --json.
func listed(t *testing.T, name string) harness.HarnessStatus {
	t.Helper()
	r := runCLI(t, "", "install", "--list", "--json")
	var v struct{ Harnesses []harness.HarnessStatus }
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("%+v", r)
	}
	for _, h := range v.Harnesses {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("%s not listed", name)

	return harness.HarnessStatus{}
}

// TestInstall_StdioMCPEveryAdapter: with the local target every adapter
// with an MCP entry writes a stdio entry running this binary's mcp command,
// a second run changes nothing, the list reads stdio with the binary, and
// uninstall restores every file.
func TestInstall_StdioMCPEveryAdapter(t *testing.T) {
	for _, name := range harness.Names() {
		if ok, _ := harness.Supports(name, harness.ModeMCP); !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.fakeClaude(t)
			e.seedLocal(t)
			before := snapshot(t, e.home)
			args := []string{"install", name, "--mcp", "--server", "local"}
			r, out := installRun(t, args...)
			if r.code != 0 || out["status"] != "installed" || harnessField(t, out, name, "mcp") != "stdio" ||
				harnessField(t, out, name, "binary") != e.exe || strings.Contains(r.stderr, envAPIKey) {
				t.Fatalf("install: %+v", r)
			}
			notes := notesOf(out)
			if strings.Contains(notes, "environment-variable syntax") {
				t.Errorf("URL note on a stdio entry: %q", notes)
			}
			if name == "pi" && !strings.Contains(notes, "the MCP entry needs pi 0.99.0 or later") {
				t.Errorf("pi note missing: %q", notes)
			}
			switch name {
			case "claude-code":
				want := `mcp add-json agentfeedback {"type":"stdio","command":` + string(mustJSONString(e.exe)) + `,"args":["mcp"]} --scope user`
				if got := strings.TrimSpace(readLogFile(e.claudeLog)); got != want {
					t.Fatalf("claude log %q\nwant %q", got, want)
				}
			case "codex":
				data, _ := os.ReadFile(filepath.Join(e.home, ".codex", "config.toml"))
				want := "# agentfeedback:begin (written by agentfeedback install; agentfeedback uninstall removes it)\n" +
					"[mcp_servers.agentfeedback]\ncommand = " + string(mustJSONString(e.exe)) + "\nargs = [\"mcp\"]\n" +
					`env_vars = ["XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "AGENT_FEEDBACK_URL"]` + "\n# agentfeedback:end\n"
				if !strings.Contains(string(data), want) || strings.Contains(string(data), "bearer_token_env_var") {
					t.Fatalf("config.toml:\n%s", data)
				}
			default:
				m := stdioMember(e.exe)[name]
				data, err := os.ReadFile(filepath.Join(e.home, filepath.FromSlash(m.file)))
				if err != nil {
					t.Fatal(err)
				}
				v, err := harness.GetMember(data, m.path, "agentfeedback")
				if err != nil || v == nil {
					t.Fatalf("%s: no entry (%v):\n%s", m.file, err, data)
				}
				if got := compactJSON(t, v); got != m.want {
					t.Errorf("%s:\n got %s\nwant %s", m.file, got, m.want)
				}
			}
			installed := snapshot(t, e.home)

			r, out = installRun(t, args...)
			if r.code != 0 || out["status"] != "unchanged" || out["changed"] != nil {
				t.Fatalf("second install: %+v", r)
			}
			sameTree(t, "second install", snapshot(t, e.home), installed)
			if h := listed(t, name); h.Mode != harness.ModeMCP || h.MCP != "stdio" || h.Binary != e.exe {
				t.Errorf("listed %+v", h)
			}

			r, out = installRun(t, "uninstall", name)
			if r.code != 0 || out["status"] != "uninstalled" {
				t.Fatalf("uninstall: %+v", r)
			}
			sameTree(t, "after uninstall", snapshot(t, e.home), before)
			if name == "claude-code" && !strings.HasSuffix(readLogFile(e.claudeLog), "mcp remove agentfeedback --scope user\n") {
				t.Fatalf("claude log %q", readLogFile(e.claudeLog))
			}
		})
	}
}

// TestInstall_MCPTargetSwitch: re-installing with another target replaces
// a URL entry with a stdio entry and back, as a change of URL does; the
// list follows the entry's kind and reports the binary for stdio only.
func TestInstall_MCPTargetSwitch(t *testing.T) {
	const srv = "https://feedback.example.test"
	for _, name := range []string{"claude-code", "codex", "omp"} {
		t.Run(name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.fakeClaude(t)
			e.seedLocal(t)
			before := snapshot(t, e.home)
			for _, step := range []struct{ server, kind, binary string }{
				{srv, "url", ""}, {"local", "stdio", e.exe}, {srv, "url", ""}, {"local", "stdio", e.exe},
			} {
				r, out := installRun(t, "install", name, "--mcp", "--server", step.server)
				if r.code != 0 || out["status"] != "installed" || harnessField(t, out, name, "mcp") != step.kind {
					t.Fatalf("%s: %+v", step.server, r)
				}
				if h := listed(t, name); h.MCP != step.kind || h.Binary != step.binary {
					t.Fatalf("%s: listed %+v", step.server, h)
				}
			}
			if name == "claude-code" {
				lines := strings.Split(strings.TrimSpace(readLogFile(e.claudeLog)), "\n")
				if len(lines) != 7 || !strings.Contains(lines[0], srv+"/mcp") || lines[1] != "mcp remove agentfeedback --scope user" ||
					!strings.Contains(lines[2], `"type":"stdio"`) || lines[3] != "mcp remove agentfeedback --scope user" {
					t.Fatalf("claude calls:\n%s", strings.Join(lines, "\n"))
				}
			}
			if name == "omp" {
				data, _ := os.ReadFile(filepath.Join(e.home, ".omp", "agent", "mcp.json"))
				if strings.Contains(string(data), `"url"`) || strings.Count(string(data), `"agentfeedback":`) != 1 {
					t.Errorf("mcp.json:\n%s", data)
				}
			}
			if r, _ := installRun(t, "uninstall", name); r.code != 0 {
				t.Fatalf("%+v", r)
			}
			sameTree(t, "after uninstall", snapshot(t, e.home), before)
		})
	}
}

// TestInstall_ClaudeStdioState: the .claude.json checks identify a stdio
// entry by its command and arguments.
func TestInstall_ClaudeStdioState(t *testing.T) {
	setup := func(t *testing.T) installEnv {
		e := newInstallEnv(t)
		e.fakeClaude(t)
		if r, _ := installRun(t, "install", "claude-code", "--mcp", "--server", "local"); r.code != 0 {
			t.Fatalf("%+v", r)
		}
		if err := os.Remove(e.claudeLog); err != nil {
			t.Fatal(err)
		}

		return e
	}
	write := func(t *testing.T, e installEnv, entry string) {
		putFile(t, filepath.Join(e.home, ".claude.json"), `{"mcpServers": {"agentfeedback": `+entry+`}}`, 0o644)
	}
	ours := func(e installEnv) string {
		return `{"type": "stdio", "command": ` + string(mustJSONString(e.exe)) + `, "args": ["mcp"]}`
	}

	t.Run("present is unchanged", func(t *testing.T) {
		e := setup(t)
		write(t, e, ours(e))
		if r, out := installRun(t, "install", "claude-code", "--mcp", "--server", "local"); r.code != 0 || out["status"] != "unchanged" || readLogFile(e.claudeLog) != "" {
			t.Fatalf("%+v %q", r, readLogFile(e.claudeLog))
		}
		if h := listed(t, "claude-code"); h.MCP != "stdio" {
			t.Errorf("listed %+v", h)
		}
	})
	for _, c := range []struct{ name, entry, says string }{
		{"other command", `{"type": "stdio", "command": "/opt/other", "args": ["mcp"]}`, "runs /opt/other mcp, not the entry agentfeedback install added"},
		{"a url", `{"type": "http", "url": "https://elsewhere.example.test/mcp"}`, "points at https://elsewhere.example.test/mcp, not the entry agentfeedback install added"},
	} {
		t.Run(c.name+" is refused", func(t *testing.T) {
			e := setup(t)
			write(t, e, c.entry)
			r, out := installRun(t, "install", "claude-code", "--mcp", "--server", "local")
			if r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), c.says) || readLogFile(e.claudeLog) != "" {
				t.Fatalf("%+v %q", r, readLogFile(e.claudeLog))
			}
			if h := listed(t, "claude-code"); h.MCP != "missing" {
				t.Errorf("listed %+v", h)
			}
			r, out = installRun(t, "uninstall", "claude-code")
			if r.code != 0 || !strings.Contains(notesOf(out), "left in place") || readLogFile(e.claudeLog) != "" {
				t.Fatalf("%+v %q", r, readLogFile(e.claudeLog))
			}
		})
	}
	t.Run("url recorded, stdio present", func(t *testing.T) {
		e := newInstallEnv(t)
		e.fakeClaude(t)
		if r, _ := installRun(t, "install", "claude-code", "--mcp", "--server", "https://feedback.example.test"); r.code != 0 {
			t.Fatalf("%+v", r)
		}
		write(t, e, ours(e))
		r, out := installRun(t, "install", "claude-code", "--mcp", "--server", "https://feedback.example.test")
		if r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), "runs "+e.exe+" mcp, not at the URL agentfeedback install added") {
			t.Fatalf("%+v", r)
		}
	})
}

// TestWindows_ManualStdioEntries: the manual steps for local carry the
// stdio entry of each adapter.
func TestWindows_ManualStdioEntries(t *testing.T) {
	e := newInstallEnv(t)
	stubWindows(t)
	_, out := windowsInstall(t, "install", "codex", "cursor", "opencode")
	if len(out.Manual) != 3 {
		t.Fatalf("%+v", out)
	}
	codex, cursor, opencode := out.Manual[0].MCP, out.Manual[1].MCP, out.Manual[2].MCP
	if codex == nil || !strings.Contains(codex.Text, "command = "+string(mustJSONString(e.exe))+"\nargs = [\"mcp\"]\n"+
		`env_vars = ["XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "AGENT_FEEDBACK_URL"]`+"\n") {
		t.Fatalf("codex %+v", codex)
	}
	want := stdioMember(e.exe)
	if cursor == nil || compactJSON(t, cursor.Value) != want["cursor"].want {
		t.Fatalf("cursor %+v", cursor)
	}
	if opencode == nil || compactJSON(t, opencode.Value) != want["opencode"].want {
		t.Fatalf("opencode %+v", opencode)
	}
}

// TestWindows_ManualStdioPathWarnings: manual steps carrying a stdio entry
// flag a binary that is not the agentfeedback on PATH; URL entries do not.
func TestWindows_ManualStdioPathWarnings(t *testing.T) {
	e := newInstallEnv(t)
	stubWindows(t)
	r, out := windowsInstall(t, "install", "cursor")
	want := "no agentfeedback on PATH; the skill runs agentfeedback from PATH, so add " + filepath.Dir(e.exe) + " to PATH"
	if len(out.Warnings) != 1 || out.Warnings[0] != want || !strings.Contains(r.stderr, "agentfeedback install: warning: "+want) {
		t.Fatalf("%+v %q", out, r.stderr)
	}
	r, out = windowsInstall(t, "install", "cursor", "--server", "https://feedback.example.test")
	if len(out.Warnings) != 0 || strings.Contains(r.stderr, "warning:") {
		t.Fatalf("%+v %q", out, r.stderr)
	}
}
