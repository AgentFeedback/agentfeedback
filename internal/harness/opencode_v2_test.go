package harness

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// openCodeEnv is a test Env whose opencode binary reports version.
func openCodeEnv(t *testing.T, version string) Env {
	t.Helper()
	e := testEnv(t)
	e.LookPath = func(name string) (string, error) {
		if name == "opencode" {
			return "/stub/opencode", nil
		}
		return "", errors.New("not found")
	}
	e.Exec = func(context.Context, map[string]string, string, ...string) ([]byte, error) {
		return []byte(version), nil
	}
	return e
}

func mcpRequest(names ...string) Request {
	return Request{Harnesses: names, Options: Options{Mode: ModeMCP, Server: ServerLocal, Binary: "/usr/local/bin/agentfeedback"}}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

func TestOpenCodeV2Detection(t *testing.T) {
	for _, tt := range []struct {
		version string
		v1, v2  string
	}{
		{"opencode v2.2.6\n", "no", "binary"},
		{"1.19.5\n", "binary", "no"},
		{"not a version", "no", "no"},
	} {
		e := openCodeEnv(t, tt.version)
		put(t, filepath.Join(e.opencodeDir(), "opencode.json"), "{}")
		if got := e.Detect("opencode-v1"); got != tt.v1 {
			t.Errorf("%q: opencode-v1 detection %s", tt.version, got)
		}
		if got := e.Detect("opencode-v2"); got != tt.v2 {
			t.Errorf("%q: opencode-v2 detection %s", tt.version, got)
		}
	}
	// The config directory alone detects V1, never V2.
	e := testEnv(t)
	put(t, filepath.Join(e.opencodeDir(), "opencode.json"), "{}")
	if e.Detect("opencode-v1") != "dir" || e.Detect("opencode-v2") != "no" {
		t.Fatal("config-only detection")
	}
}

func TestOpenCodeV2InstallAndUninstall(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	if _, err := e.Run(cliRequest("opencode-v2")); err != nil {
		t.Fatal(err)
	}
	plugin, err := os.ReadFile(filepath.Join(e.opencodeDir(), "plugins", "agentfeedback.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plugin), `id: "agentfeedback"`) || !strings.Contains(string(plugin), `const HARNESS = "opencode-v2";`) ||
		!strings.Contains(string(plugin), filepath.Join(e.xdgCache(), "agentfeedback", "hooks", "opencode-v2.spawn-error.json")) {
		t.Fatalf("plugin:\n%s", plugin)
	}
	for _, f := range []string{filepath.Join(e.opencodeDir(), "skills", "agentfeedback", "SKILL.md"), filepath.Join(e.opencodeDir(), "AGENTS.md")} {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Run(Request{Harnesses: []string{"opencode-v2"}, Uninstall: true}); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(e.opencodeDir()); err == nil && len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	if _, err := os.Stat(e.ManifestPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest remains: %v", err)
	}
}

func TestOpenCodeV2MCPShapes(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	if _, err := e.Run(mcpRequest("opencode-v2")); err != nil {
		t.Fatal(err)
	}
	cfg := readJSON(t, filepath.Join(e.opencodeDir(), "opencode.json"))
	entry := cfg["mcp"].(map[string]any)["servers"].(map[string]any)["agentfeedback"].(map[string]any)
	if entry["type"] != "local" || len(entry) != 2 {
		t.Fatalf("stdio entry %v", entry)
	}
	if cmd := entry["command"].([]any); len(cmd) != 2 || cmd[0] != "/usr/local/bin/agentfeedback" || cmd[1] != "mcp" {
		t.Fatalf("command %v", cmd)
	}
	a := adapterOf("opencode-v2")
	var remote map[string]any
	if err := json.Unmarshal([]byte(a.mcpValue("https://x.test/mcp")), &remote); err != nil {
		t.Fatal(err)
	}
	headers := remote["headers"].(map[string]any)
	if remote["type"] != "remote" || remote["url"] != "https://x.test/mcp" || remote["oauth"] != false || len(remote) != 4 ||
		headers["Authorization"] != "Bearer {env:AGENT_FEEDBACK_API_KEY}" {
		t.Fatalf("remote entry %v", remote)
	}
	if _, err := e.Run(Request{Harnesses: []string{"opencode-v2"}, Uninstall: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.opencodeDir(), "opencode.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config remains: %v", err)
	}
}

func TestOpenCodeVariantsReplaceEachOther(t *testing.T) {
	for _, tt := range []struct{ from, to, version string }{
		{"opencode-v1", "opencode-v2", "opencode v2.2.6\n"},
		{"opencode-v2", "opencode-v1", "1.19.5\n"},
	} {
		t.Run(tt.from+"->"+tt.to, func(t *testing.T) {
			e := openCodeEnv(t, tt.version)
			oc := e.opencodeDir()
			put(t, filepath.Join(oc, "opencode.json"), `{"mcp":{"other":{"type":"local","command":["x"]}}}`)
			put(t, filepath.Join(oc, "AGENTS.md"), "# mine\n")
			want := "replaced the " + tt.from + " wiring (OpenCode V1 and V2 share " + oc + ")"
			switchTo := func(req func(...string) Request) {
				t.Helper()
				if _, err := e.Run(req(tt.from)); err != nil {
					t.Fatal(err)
				}
				res, err := e.Run(req(tt.to))
				if err != nil {
					t.Fatal(err)
				}
				var notes []string
				for _, h := range res.Harnesses {
					if h.Name == tt.to {
						notes = h.Notes
					}
				}
				if !slices.Contains(notes, want) {
					t.Fatalf("notes %v", notes)
				}
				m, _, err := LoadManifest(e.ManifestPath())
				if err != nil {
					t.Fatal(err)
				}
				if m.Harnesses[tt.from] != nil || m.Harnesses[tt.to] == nil {
					t.Fatalf("manifest %v", m.Harnesses)
				}
			}

			switchTo(cliRequest)
			m, _, err := LoadManifest(e.ManifestPath())
			if err != nil {
				t.Fatal(err)
			}
			skill := filepath.Join(oc, "skills", "agentfeedback", "SKILL.md")
			if !slices.ContainsFunc(m.Harnesses[tt.to].Items, func(it Item) bool { return it.File == skill }) {
				t.Fatalf("skill not recorded under %s", tt.to)
			}
			if _, err := os.Stat(skill); err != nil {
				t.Fatal(err)
			}
			rule, err := os.ReadFile(filepath.Join(oc, "AGENTS.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(rule), "# mine\n") || strings.Count(string(rule), "<!-- agentfeedback:begin") != 1 {
				t.Fatalf("rule file:\n%s", rule)
			}
			plugin, err := os.ReadFile(filepath.Join(oc, "plugins", "agentfeedback.js"))
			if err != nil || !strings.Contains(string(plugin), `const HARNESS = "`+tt.to+`";`) {
				t.Fatalf("plugin %v:\n%s", err, plugin)
			}

			switchTo(mcpRequest)
			mcp := readJSON(t, filepath.Join(oc, "opencode.json"))["mcp"].(map[string]any)
			if mcp["other"] == nil {
				t.Fatalf("user entry lost: %v", mcp)
			}
			_, v1 := mcp["agentfeedback"]
			var v2 bool
			if servers, ok := mcp["servers"].(map[string]any); ok {
				_, v2 = servers["agentfeedback"]
			}
			if v1 != (tt.to == "opencode-v1") || v2 != (tt.to == "opencode-v2") {
				t.Fatalf("mcp members %v", mcp)
			}

			if _, err := e.Run(Request{Harnesses: []string{tt.to}, Uninstall: true}); err != nil {
				t.Fatal(err)
			}
			if got := mustJSON(t, readJSON(t, filepath.Join(oc, "opencode.json"))); !strings.Contains(got, `"other"`) || strings.Contains(got, "agentfeedback") {
				t.Fatalf("after uninstall %s", got)
			}
			if _, err := os.Stat(filepath.Join(oc, "plugins", "agentfeedback.js")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("plugin remains: %v", err)
			}
			if rule, _ := os.ReadFile(filepath.Join(oc, "AGENTS.md")); string(rule) != "# mine\n" {
				t.Fatalf("rule file after uninstall:\n%s", rule)
			}
			if _, err := os.Stat(e.ManifestPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("manifest remains: %v", err)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestOpenCodeBothVariantsRefused(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	_, err := e.Run(cliRequest("opencode-v1", "opencode-v2"))
	var r *Refusal
	if !errors.As(err, &r) || r.Problem != "opencode-v1 and opencode-v2 share one configuration directory" {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(e.opencodeDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal wrote files")
	}
}

func TestOpenCodeV2StatusProblemKeysOnRecord(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	if _, err := e.Run(cliRequest("opencode-v2")); err != nil {
		t.Fatal(err)
	}
	statuses, err := e.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range statuses {
		if strings.HasPrefix(st.Name, "opencode") && len(st.Problems) != 0 {
			t.Fatalf("%s problems %v", st.Name, st.Problems)
		}
	}
}

func TestOpenCodeSwitchDryRunWritesNothing(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	if _, err := e.Run(cliRequest("opencode-v1")); err != nil {
		t.Fatal(err)
	}
	before := treeOf(t, e.Home)
	req := cliRequest("opencode-v2")
	req.DryRun = true
	res, err := e.Run(req)
	if err != nil || res.Status != "dry_run" {
		t.Fatalf("%+v %v", res, err)
	}
	plugin := filepath.Join(e.opencodeDir(), "plugins", "agentfeedback.js")
	if !slices.Contains(res.Changed, plugin) || !slices.Contains(res.Changed, e.ManifestPath()) {
		t.Fatalf("changed %v", res.Changed)
	}
	if len(res.Harnesses) != 1 || !slices.ContainsFunc(res.Harnesses[0].Notes, func(n string) bool { return strings.HasPrefix(n, "replaced the opencode-v1 wiring") }) {
		t.Fatalf("harnesses %+v", res.Harnesses)
	}
	if after := treeOf(t, e.Home); !maps.Equal(before, after) {
		t.Fatal("dry run wrote files")
	}
}

func TestOpenCodeSwitchRefusalKeepsV1(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	if _, err := e.Run(mcpRequest("opencode-v1")); err != nil {
		t.Fatal(err)
	}
	// A non-object mcp.servers: the V1 uninstall half could proceed, the V2
	// install half cannot add its member.
	cfg := filepath.Join(e.opencodeDir(), "opencode.json")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	put(t, cfg, strings.Replace(string(data), `"mcp": {`, `"mcp": {"servers": 1,`, 1))
	if !strings.Contains(readFile(t, cfg), `"servers": 1`) {
		t.Fatalf("seed failed:\n%s", readFile(t, cfg))
	}
	before := treeOf(t, e.Home)
	if _, err := e.Run(mcpRequest("opencode-v2")); err == nil {
		t.Fatal("install half did not refuse")
	}
	if after := treeOf(t, e.Home); !maps.Equal(before, after) {
		t.Fatal("refused switch wrote files")
	}
}

func TestOpenCodeSwitchKeepsModifiedV1Member(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	if _, err := e.Run(mcpRequest("opencode-v1")); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(e.opencodeDir(), "opencode.json")
	put(t, cfg, strings.Replace(readFile(t, cfg), `"enabled": true`, `"enabled": false`, 1))
	if !strings.Contains(readFile(t, cfg), `"enabled": false`) {
		t.Fatalf("seed failed:\n%s", readFile(t, cfg))
	}
	res, err := e.Run(mcpRequest("opencode-v2"))
	if err != nil {
		t.Fatal(err)
	}
	notes := res.Harnesses[0].Notes
	if res.Harnesses[0].Name != "opencode-v2" || !slices.Contains(notes, cfg+": mcp.agentfeedback was changed since install and is left in place") ||
		!slices.ContainsFunc(notes, func(n string) bool { return strings.HasPrefix(n, "replaced the opencode-v1 wiring") }) {
		t.Fatalf("notes %v", notes)
	}
	mcp := readJSON(t, cfg)["mcp"].(map[string]any)
	if mcp["agentfeedback"] == nil || mcp["servers"].(map[string]any)["agentfeedback"] == nil {
		t.Fatalf("mcp %v", mcp)
	}
}

// A note finalize adds for the replaced variant's file (its backup is
// missing) still shows on the installed variant.
func TestOpenCodeSwitchKeepsFinalizeNotes(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	cfg := filepath.Join(e.opencodeDir(), "opencode.json")
	put(t, cfg, "{\n  \"theme\": \"x\"\n}\n")
	if _, err := e.Run(mcpRequest("opencode-v1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg + BackupSuffix); err != nil {
		t.Fatal(err)
	}
	res, err := e.Run(cliRequest("opencode-v2"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Harnesses[0].Notes, func(n string) bool { return strings.Contains(n, "is missing") }) {
		t.Fatalf("notes %v", res.Harnesses[0].Notes)
	}
}

// treeOf maps every regular file under root to its content.
func treeOf(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		out[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOpenCodeV2StatusProblemOnV1(t *testing.T) {
	e := openCodeEnv(t, "1.19.5\n")
	if _, err := e.Run(cliRequest("opencode-v2")); err != nil {
		t.Fatal(err)
	}
	statuses, err := e.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range statuses {
		want := 0
		if st.Name == "opencode-v2" {
			want = 1
		}
		if strings.HasPrefix(st.Name, "opencode") && (len(st.Problems) != want || want == 1 && st.Problems[0] != "OpenCode 1.x does not run the installed opencode-v2 plugin; run agentfeedback install opencode-v1, which replaces it") {
			t.Fatalf("%s problems %v", st.Name, st.Problems)
		}
	}
}

// The switch runs in one plan: a shared file the user edited after the V1
// install stays owned across it, so the V2 install never meets it as a
// file the manifest does not record.
func TestOpenCodeSwitchEditedRuleFile(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	rule := filepath.Join(e.opencodeDir(), "AGENTS.md")
	put(t, rule, "# mine\n")
	if _, err := e.Run(cliRequest("opencode-v1")); err != nil {
		t.Fatal(err)
	}
	put(t, rule, readFile(t, rule)+"\nmy later line\n")
	if _, err := e.Run(cliRequest("opencode-v2")); err != nil {
		t.Fatal(err)
	}
	m, _, err := LoadManifest(e.ManifestPath())
	if err != nil || m.Harnesses["opencode-v1"] != nil || m.Harnesses["opencode-v2"] == nil {
		t.Fatalf("manifest %v %v", m, err)
	}
	if got := readFile(t, rule); strings.Count(got, sectionBegin) != 1 || !strings.Contains(got, "my later line") {
		t.Fatalf("rule file:\n%s", got)
	}
}

func TestOpenCodeSwitchEditedConfig(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	cfg := filepath.Join(e.opencodeDir(), "opencode.jsonc")
	put(t, cfg, "{\n  // mine\n  \"theme\": \"a\",\n  \"mcp\": {}\n}\n")
	if _, err := e.Run(mcpRequest("opencode-v1")); err != nil {
		t.Fatal(err)
	}
	put(t, cfg, strings.Replace(readFile(t, cfg), `"theme": "a"`, `"theme": "b"`, 1))
	if _, err := e.Run(mcpRequest("opencode-v2")); err != nil {
		t.Fatal(err)
	}
	m, _, err := LoadManifest(e.ManifestPath())
	if err != nil || m.Harnesses["opencode-v1"] != nil || m.Harnesses["opencode-v2"] == nil {
		t.Fatalf("manifest %v %v", m, err)
	}
	got := readFile(t, cfg)
	v1, err1 := GetMember([]byte(got), []string{"mcp"}, "agentfeedback")
	v2, err2 := GetMember([]byte(got), []string{"mcp", "servers"}, "agentfeedback")
	if !strings.Contains(got, `"theme": "b"`) || err1 != nil || err2 != nil || v1 != nil || v2 == nil {
		t.Fatalf("config:\n%s", got)
	}
}

// A Claude refusal comes before any file of the run, the V1 removal
// included.
func TestOpenCodeSwitchClaudeRefusalKeepsV1(t *testing.T) {
	e := openCodeEnv(t, "opencode v2.2.6\n")
	if _, err := e.Run(mcpRequest("opencode-v1")); err != nil {
		t.Fatal(err)
	}
	e.LookPath = func(name string) (string, error) {
		if name == "opencode" || name == "claude" {
			return "/stub/" + name, nil
		}
		return "", errors.New("not found")
	}
	e.Exec = func(_ context.Context, _ map[string]string, bin string, args ...string) ([]byte, error) {
		if strings.HasSuffix(bin, "claude") {
			return []byte("MCP server agentfeedback already exists in user config"), errors.New("exit status 1")
		}
		return []byte("opencode v2.2.6\n"), nil
	}
	before := treeOf(t, e.Home)
	_, err := e.Run(mcpRequest("claude-code", "opencode-v2"))
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Problem, "already has an MCP server named agentfeedback") {
		t.Fatalf("got %v", err)
	}
	if after := treeOf(t, e.Home); !maps.Equal(before, after) {
		t.Fatal("refused run changed files")
	}
	m, _, err := LoadManifest(e.ManifestPath())
	if err != nil || m.Harnesses["opencode-v1"] == nil || m.Harnesses["opencode-v2"] != nil {
		t.Fatalf("manifest %v %v", m, err)
	}
}
