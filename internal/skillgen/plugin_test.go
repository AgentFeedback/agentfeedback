package skillgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback"
)

func paths(files []File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}

	return out
}

func file(t *testing.T, files []File, p string) File {
	t.Helper()
	for _, f := range files {
		if f.Path == p {
			return f
		}
	}
	t.Fatalf("no %s in %v", p, paths(files))

	return File{}
}

func TestAgentPlugin(t *testing.T) {
	files, err := AgentPlugin("")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plugin.json", ".claude-plugin/plugin.json", "skills/agentfeedback/SKILL.md", "skills/agentfeedback/scripts/install.sh"}
	if got := paths(files); !slices.Equal(got, want) {
		t.Fatalf("paths %v, want %v", got, want)
	}
	if got := file(t, files, "skills/agentfeedback/SKILL.md").Data; string(got) != render(t, FormSkillMD, "") {
		t.Error("SKILL.md differs from the skill-md form")
	}
	script := file(t, files, "skills/agentfeedback/scripts/install.sh")
	if !bytes.Equal(script.Data, agentfeedback.InstallScript) || script.Mode != 0o755 {
		t.Errorf("install.sh: mode %v, equal %v", script.Mode, bytes.Equal(script.Data, agentfeedback.InstallScript))
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".json") && !json.Valid(f.Data) {
			t.Errorf("%s does not parse", f.Path)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(file(t, files, "plugin.json").Data, &m); err != nil {
		t.Fatal(err)
	}
	if m["$schema"] != pluginSchema || m["name"] != "agentfeedback" || m["version"] != "5.0.0" {
		t.Errorf("plugin.json: %v", m)
	}
	var c map[string]any
	if err := json.Unmarshal(file(t, files, ".claude-plugin/plugin.json").Data, &c); err != nil {
		t.Fatal(err)
	}
	if _, ok := c["$schema"]; ok || c["name"] != "agentfeedback" {
		t.Errorf(".claude-plugin/plugin.json: %v", c)
	}
}

func TestAgentPluginServer(t *testing.T) {
	for server, want := range map[string]string{
		"https://feedback.example.com/": "https://feedback.example.com/mcp",
		"http://localhost:8080":         "http://localhost:8080/mcp",
		"http://127.0.0.1:8080":         "http://127.0.0.1:8080/mcp",
		"http://[::1]:8080":             "http://[::1]:8080/mcp",
		"http://LocalHost:8080":         "http://LocalHost:8080/mcp",
	} {
		files, err := AgentPlugin(server)
		if err != nil {
			t.Fatalf("%s: %v", server, err)
		}
		if files[len(files)-1].Path != "mcp.json" {
			t.Fatalf("%s: paths %v", server, paths(files))
		}
		var m struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(files[len(files)-1].Data, &m); err != nil {
			t.Fatal(err)
		}
		if s := m.MCPServers["agentfeedback"]; s["url"] != want || s["type"] != "streamable-http" || len(s) != 2 {
			t.Errorf("%s: %v", server, m)
		}
	}
	// ServerError carries the raw URL; the CLI redacts credentials from it.
	for _, server := range []string{"http://feedback.example.com", "http://localhost.:8080", "http://localhost.example.com", "https://user:pw@x", "ftp://x"} {
		_, err := AgentPlugin(server)
		var se *ServerError
		if !errors.As(err, &se) {
			t.Errorf("%s: %v, want a *ServerError", server, err)
		}
	}
	if _, err := AgentPlugin("http://feedback.example.com"); err == nil || !strings.Contains(err.Error(), "needs https") {
		t.Errorf("http refusal: %v", err)
	}
}

// The golden mcp.json is what scripts/contract-check.py validates against the
// vendored mcp schema.
func TestAgentPluginMCPGolden(t *testing.T) {
	files, err := AgentPlugin(testServer)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/agent-plugin-mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := file(t, files, "mcp.json").Data; !bytes.Equal(got, golden) {
		t.Errorf("mcp.json differs from the golden:\n%s", got)
	}
}

func TestMarketplace(t *testing.T) {
	files, err := Marketplace()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".claude-plugin/marketplace.json", ".agents/plugins/marketplace.json",
		"plugins/agentfeedback/plugin.json", "plugins/agentfeedback/.claude-plugin/plugin.json",
		"plugins/agentfeedback/skills/agentfeedback/SKILL.md", "plugins/agentfeedback/skills/agentfeedback/scripts/install.sh",
	}
	if got := paths(files); !slices.Equal(got, want) {
		t.Fatalf("paths %v, want %v", got, want)
	}
	var claude struct {
		Name    string
		Plugins []struct{ Name, Source string }
	}
	var codex struct {
		Name    string
		Plugins []struct {
			Name   string
			Source struct{ Source, Path string }
		}
	}
	if err := json.Unmarshal(file(t, files, ".claude-plugin/marketplace.json").Data, &claude); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(file(t, files, ".agents/plugins/marketplace.json").Data, &codex); err != nil {
		t.Fatal(err)
	}
	if len(claude.Plugins) != 1 || len(codex.Plugins) != 1 {
		t.Fatalf("plugins: %+v %+v", claude, codex)
	}
	if claude.Name != codex.Name || claude.Plugins[0].Name != codex.Plugins[0].Name || codex.Plugins[0].Source.Source != "local" {
		t.Errorf("names differ: %+v %+v", claude, codex)
	}
	for _, src := range []string{claude.Plugins[0].Source, codex.Plugins[0].Source.Path} {
		file(t, files, strings.TrimPrefix(src, "./")+"/plugin.json")
		file(t, files, strings.TrimPrefix(src, "./")+"/.claude-plugin/plugin.json")
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, "mcp.json") {
			t.Errorf("a marketplace carries %s", f.Path)
		}
	}
}

func TestSemver(t *testing.T) {
	for in, want := range map[string]string{"5.0": "5.0.0", "5.0.1": "5.0.1", "5": "", "5.x": "", "5.0.0.1": "", "": "", "5..0": "", "05.0": "", "5.00": "", "10.0": "10.0.0"} {
		got, err := semver(in)
		if got != want || (err != nil) != (want == "") {
			t.Errorf("semver(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
