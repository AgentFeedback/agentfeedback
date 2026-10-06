package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// installEnv is a hermetic machine: HOME holds every file install may
// touch (the XDG directories included), PATH holds only bin.
type installEnv struct {
	home, bin, exe, claudeLog string
}

func newInstallEnv(t *testing.T) installEnv {
	t.Helper()
	isolateCLI(t)
	root := t.TempDir()
	e := installEnv{home: filepath.Join(root, "home"), bin: filepath.Join(root, "bin")}
	for _, d := range []string{e.home, e.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", e.home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(e.home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(e.home, ".cache"))
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", e.bin)
	e.exe = filepath.Join(e.bin, "agentfeedback")
	putFile(t, e.exe, "#!/bin/sh\n", 0o755)
	orig := executable
	executable = func() (string, error) { return e.exe, nil }
	t.Cleanup(func() { executable = orig })
	origTerm := isTerminal
	isTerminal = func() bool { return false }
	t.Cleanup(func() { isTerminal = origTerm })
	e.claudeLog = filepath.Join(root, "claude.log")

	return e
}

// fakeClaude puts a claude on PATH that logs its arguments; add-json fails
// with "already exists" when the marker file exists.
func (e installEnv) fakeClaude(t *testing.T) string {
	t.Helper()
	marker := filepath.Join(filepath.Dir(e.bin), "claude-has-agentfeedback")
	putFile(t, filepath.Join(e.bin, "claude"), fmt.Sprintf(`#!/bin/sh
echo "$*" >> %s
if [ "$2" = add-json ] && [ -e %s ]; then
  echo "MCP server agentfeedback already exists in user config" >&2
  exit 1
fi
exit 0
`, e.claudeLog, marker), 0o755)

	return marker
}

func putFile(t *testing.T, path, data string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

// snapshot is every directory and file under root with its content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			out[rel+"/"] = ""

			return nil
		}
		data, err := os.ReadFile(path)
		out[rel] = string(data)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func sameTree(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if g, ok := got[k]; !ok {
			t.Errorf("%s: %s is missing", what, k)
		} else if g != v {
			t.Errorf("%s: %s differs:\n%s\nwant\n%s", what, k, g, v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: %s is extra", what, k)
		}
	}
}

// seed writes realistic existing configuration for every harness.
func (e installEnv) seed(t *testing.T) {
	t.Helper()
	h := e.home
	putFile(t, filepath.Join(h, ".config", "agentfeedback", "config.toml"), "url = \"https://feedback.example.test\"\napi_key = \"throwaway\"\n", 0o600)
	putFile(t, filepath.Join(h, ".claude", "settings.json"), `{
  "model": "opus",
  "hooks": {
    "Stop": [
      {
        "hooks": [{"type": "command", "command": "notify-send done"}]
      }
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "guard"}]}
    ]
  }
}
`, 0o600)
	putFile(t, filepath.Join(h, ".codex", "config.toml"), `# Codex settings
model = "gpt-5"

[mcp_servers.other]
url = "https://other.example.test/mcp" # another server
`, 0o644)
	putFile(t, filepath.Join(h, ".cursor", "mcp.json"), `{
  // servers used in every project
  "mcpServers": {
    "other": { "url": "https://other.example.test/mcp" },
  },
}
`, 0o644)
	putFile(t, filepath.Join(h, ".cursor", "hooks.json"), "{\n\t\"version\": 1,\n\t\"hooks\": {\n\t\t\"stop\": [\n\t\t\t{ \"command\": \"echo bye\" }\n\t\t]\n\t}\n}\n", 0o644)
	putFile(t, filepath.Join(h, ".config", "opencode", "opencode.jsonc"), `{
  "$schema": "https://opencode.ai/config.json",
  /* local servers */
  "mcp": {
    "other": { "type": "local", "command": ["other"] }, // keep
  },
}
`, 0o644)
	putFile(t, filepath.Join(h, ".omp", "agent", "mcp.json"), "{\r\n  \"mcpServers\": {}\r\n}\r\n", 0o644)
	putFile(t, filepath.Join(h, ".pi", "agent", "settings.json"), "{}\n", 0o644)
}

func installRun(t *testing.T, args ...string) (result, map[string]any) {
	t.Helper()
	r := runCLI(t, "", args...)
	var out map[string]any
	if err := json.Unmarshal([]byte(lastLine(r.stdout)), &out); err != nil {
		t.Fatalf("%v: last stdout line is not JSON: %+v", args, r)
	}

	return r, out
}

func TestInstall_EveryAdapterRoundTrip(t *testing.T) {
	for _, name := range harness.Names() {
		for _, mode := range []string{"cli", "mcp"} {
			for _, seeded := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/seeded=%v", name, mode, seeded), func(t *testing.T) {
					e := newInstallEnv(t)
					e.fakeClaude(t)
					args := []string{"install", name}
					if seeded {
						e.seed(t)
					} else {
						args = append(args, "--server", "https://feedback.example.test")
					}
					if mode == "mcp" {
						args = append(args, "--mcp")
					} else {
						args = append(args, "--with-reminder")
					}
					before := snapshot(t, e.home)

					r, out := installRun(t, args...)
					if r.code != 0 || out["status"] != "installed" {
						t.Fatalf("install: %+v", r)
					}
					if name == "claude-code" && mode == "mcp" {
						log, _ := os.ReadFile(e.claudeLog)
						want := `mcp add-json agentfeedback {"type":"http","url":"https://feedback.example.test/mcp","headers":{"Authorization":"Bearer ${AGENT_FEEDBACK_API_KEY}"}} --scope user`
						if strings.TrimSpace(string(log)) != want {
							t.Fatalf("claude log %q", log)
						}
					}
					backups, _ := out["backups"].([]any)
					for _, b := range backups {
						path := b.(string)
						orig := strings.TrimSuffix(path, harness.BackupSuffix)
						rel, _ := filepath.Rel(e.home, orig)
						data, err := os.ReadFile(path)
						if err != nil || string(data) != before[rel] {
							t.Errorf("backup %s does not equal the original (%v)", path, err)
						}
					}
					// The seeded files each combination edits in place.
					edits := map[string]bool{"claude-code/cli": true, "codex/mcp": true, "cursor/cli": true, "cursor/mcp": true, "opencode/mcp": true, "omp/mcp": true}
					if want := seeded && edits[name+"/"+mode]; want != (len(backups) == 1) {
						t.Errorf("backups %v, want one: %v", backups, want)
					}
					installed := snapshot(t, e.home)

					r, out = installRun(t, args...)
					if r.code != 0 || out["status"] != "unchanged" || out["changed"] != nil {
						t.Fatalf("second install: %+v", r)
					}
					sameTree(t, "second install", snapshot(t, e.home), installed)

					r, out = installRun(t, "uninstall", name)
					if r.code != 0 || out["status"] != "uninstalled" {
						t.Fatalf("uninstall: %+v", r)
					}
					sameTree(t, "after uninstall", snapshot(t, e.home), before)
					if name == "claude-code" && mode == "mcp" {
						log, _ := os.ReadFile(e.claudeLog)
						if !strings.HasSuffix(string(log), "mcp remove agentfeedback --scope user\n") {
							t.Fatalf("claude log %q", log)
						}
					}
				})
			}
		}
	}
}

func TestInstall_WiredContent(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	if r, _ := installRun(t, "install", "claude-code", "opencode", "omp", "cursor", "--with-reminder"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	settings, _ := os.ReadFile(filepath.Join(e.home, ".claude", "settings.json"))
	for _, want := range []string{`"command": "` + e.exe + ` flush --hook"`, `"command": "` + e.exe + ` skill reminder"`, `"command": "notify-send done"`} {
		if !strings.Contains(string(settings), want) {
			t.Errorf("settings.json lacks %s:\n%s", want, settings)
		}
	}
	skill, _ := os.ReadFile(filepath.Join(e.home, ".claude", "skills", "agentfeedback", "SKILL.md"))
	want, _ := skillgen.Render(skillgen.FormSkillMD, "")
	if string(skill) != string(want) {
		t.Error("SKILL.md is not the skill-md render")
	}
	plugin, _ := os.ReadFile(filepath.Join(e.home, ".config", "opencode", "plugins", "agentfeedback.js"))
	if !strings.Contains(string(plugin), `const BIN = "`+e.exe+`";`) || !strings.Contains(string(plugin), `"session.idle"`) {
		t.Errorf("opencode plugin:\n%s", plugin)
	}
	ext, _ := os.ReadFile(filepath.Join(e.home, ".omp", "agent", "extensions", "agentfeedback.ts"))
	if !strings.Contains(string(ext), `pi.on("agent_end"`) || !strings.Contains(string(ext), `ctx?.agent?.kind === "sub"`) {
		t.Errorf("omp extension:\n%s", ext)
	}
	hooks, _ := os.ReadFile(filepath.Join(e.home, ".cursor", "hooks.json"))
	if !strings.Contains(string(hooks), "\"sessionStart\"") || !strings.Contains(string(hooks), "\t\t\t{ \"command\": \"echo bye\" },\n\t\t\t{\n") {
		t.Errorf("cursor hooks.json:\n%s", hooks)
	}
	mpath := filepath.Join(e.home, ".config", "agentfeedback", "install.json")
	info, err := os.Stat(mpath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest %v %v", info, err)
	}
	var m harness.Manifest
	data, _ := os.ReadFile(mpath)
	if err := json.Unmarshal(data, &m); err != nil || m.Version != 1 || m.Server != "https://feedback.example.test" || m.Binary != e.exe || len(m.Harnesses) != 4 {
		t.Fatalf("manifest %s", data)
	}
	if st, _ := os.Stat(filepath.Join(e.home, ".claude", "settings.json")); st.Mode().Perm() != 0o600 {
		t.Errorf("settings.json mode %v, want 0600 kept", st.Mode().Perm())
	}

	// --mcp on opencode puts the member into the jsonc file that has mcp.
	if r, _ := installRun(t, "install", "opencode", "--mcp"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	oc, _ := os.ReadFile(filepath.Join(e.home, ".config", "opencode", "opencode.jsonc"))
	if !strings.Contains(string(oc), `"Authorization": "Bearer {env:AGENT_FEEDBACK_API_KEY}"`) || !strings.Contains(string(oc), "// keep") {
		t.Errorf("opencode.jsonc:\n%s", oc)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".config", "opencode", "plugins")); err == nil {
		t.Error("switching to --mcp left the plugins directory install created")
	}
}

func TestInstall_ModeSwitchRoundTrip(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	before := snapshot(t, e.home)
	for _, args := range [][]string{{"install", "omp", "pi"}, {"install", "omp", "pi", "--mcp"}, {"install", "omp"}} {
		if r, _ := installRun(t, args...); r.code != 0 {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	if _, err := os.Stat(filepath.Join(e.home, ".omp", "agent", "skills", "agentfeedback", "SKILL.md")); err != nil {
		t.Fatal("omp skill missing after switching back")
	}
	if r, _ := installRun(t, "uninstall", "all"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after uninstall all", snapshot(t, e.home), before)
}

func TestInstall_ForeignRefusals(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		args    []string
		setup   func(e installEnv, t *testing.T) string
	}{
		{"skill directory", "claude-code", nil, func(e installEnv, t *testing.T) string {
			p := filepath.Join(e.home, ".claude", "skills", "agentfeedback")
			putFile(t, filepath.Join(p, "SKILL.md"), "old copy\n", 0o644)

			return p
		}},
		{"mcp member", "omp", []string{"--mcp"}, func(e installEnv, t *testing.T) string {
			p := filepath.Join(e.home, ".omp", "agent", "mcp.json")
			putFile(t, p, `{"mcpServers": {"agentfeedback": {"url": "x"}}}`, 0o644)

			return p
		}},
		{"toml table", "codex", []string{"--mcp"}, func(e installEnv, t *testing.T) string {
			p := filepath.Join(e.home, ".codex", "config.toml")
			putFile(t, p, "[mcp_servers.agentfeedback]\nurl = \"x\"\n", 0o644)

			return p
		}},
		{"plugin file", "opencode", nil, func(e installEnv, t *testing.T) string {
			p := filepath.Join(e.home, ".config", "opencode", "plugins", "agentfeedback.js")
			putFile(t, p, "// mine\n", 0o644)

			return p
		}},
		{"identical hook", "claude-code", nil, func(e installEnv, t *testing.T) string {
			p := filepath.Join(e.home, ".claude", "settings.json")
			putFile(t, p, `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "`+e.exe+` flush --hook", "timeout": 5}]}]}}`, 0o644)

			return p
		}},
		{"claude.json member", "claude-code", []string{"--mcp"}, func(e installEnv, t *testing.T) string {
			e.fakeClaude(t)
			p := filepath.Join(e.home, ".claude.json")
			putFile(t, p, `{"numStartups": 3, "mcpServers": {"agentfeedback": {"type": "http", "url": "x"}}}`, 0o644)

			return p
		}},
		{"claude mcp", "claude-code", []string{"--mcp"}, func(e installEnv, t *testing.T) string {
			putFile(t, e.fakeClaude(t), "", 0o644)

			return "claude mcp remove agentfeedback"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.seed(t)
			// Another harness planned first must not be written either.
			mustName := tt.setup(e, t)
			before := snapshot(t, e.home)
			args := append([]string{"install", "pi", tt.harness}, tt.args...)
			r, out := installRun(t, args...)
			if r.code != 1 || out["status"] != "error" || !strings.Contains(out["message"].(string), mustName) {
				t.Fatalf("%+v", r)
			}
			// Steps with claude commands run first, so even claude's own
			// refusal comes before pi's files are written, and the run's
			// first manifest write is taken back.
			sameTree(t, "after a refusal", snapshot(t, e.home), before)
		})
	}
}

func TestInstall_ClaudeMCPServerChangeRemovesFirst(t *testing.T) {
	e := newInstallEnv(t)
	e.fakeClaude(t)
	for _, srv := range []string{"http://a.example", "http://b.example"} {
		if r, _ := installRun(t, "install", "claude-code", "--mcp", "--server", srv); r.code != 0 {
			t.Fatalf("%s: %+v", srv, r)
		}
	}
	data, err := os.ReadFile(e.claudeLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "mcp add-json agentfeedback") ||
		lines[1] != "mcp remove agentfeedback --scope user" ||
		!strings.HasPrefix(lines[2], "mcp add-json agentfeedback") || !strings.Contains(lines[2], "http://b.example/mcp") {
		t.Fatalf("claude calls:\n%s", data)
	}
}

func TestInstall_ModifiedSkillRefused(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	if r, _ := installRun(t, "install", "claude-code"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	skill := filepath.Join(e.home, ".claude", "skills", "agentfeedback", "SKILL.md")
	putFile(t, skill, "edited\n", 0o644)
	before := snapshot(t, e.home)
	r, out := installRun(t, "install", "claude-code")
	if r.code != 1 || !strings.Contains(out["message"].(string), skill+" was modified since install") {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after refusal", snapshot(t, e.home), before)
	// Uninstall leaves the edited file and says so.
	r, _ = installRun(t, "uninstall", "claude-code")
	if r.code != 0 || !strings.Contains(r.stdout, "modified since install and is left in place") {
		t.Fatalf("%+v", r)
	}
	if data, _ := os.ReadFile(skill); string(data) != "edited\n" {
		t.Fatal("uninstall removed an edited skill")
	}
}

func TestInstall_SurgicalUninstallKeepsBackup(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	if r, _ := installRun(t, "install", "claude-code"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	p := filepath.Join(e.home, ".claude", "settings.json")
	data, _ := os.ReadFile(p)
	edited := strings.Replace(string(data), `"model": "opus"`, `"model": "sonnet"`, 1)
	putFile(t, p, edited, 0o600)
	r, out := installRun(t, "uninstall", "claude-code")
	if r.code != 0 || out["backups"] == nil {
		t.Fatalf("%+v", r)
	}
	got, _ := os.ReadFile(p)
	if strings.Contains(string(got), "flush --hook") || !strings.Contains(string(got), `"model": "sonnet"`) {
		t.Fatalf("surgical removal:\n%s", got)
	}
	if _, err := os.Stat(p + harness.BackupSuffix); err != nil {
		t.Fatal("backup not kept")
	}
}

func TestInstall_ServerResolution(t *testing.T) {
	manifestServer := func(t *testing.T, e installEnv) string {
		t.Helper()
		data, _ := os.ReadFile(filepath.Join(e.home, ".config", "agentfeedback", "install.json"))
		var m harness.Manifest
		_ = json.Unmarshal(data, &m)

		return m.Server
	}

	t.Run("none configured, not a terminal", func(t *testing.T) {
		e := newInstallEnv(t)
		before := snapshot(t, e.home)
		r, out := installRun(t, "install", "claude-code")
		if r.code == 0 || !strings.Contains(out["message"].(string), "no server configured; pass --server cloud|URL or run agentfeedback doctor --init first") {
			t.Fatalf("%+v", r)
		}
		sameTree(t, "no server", snapshot(t, e.home), before)
	})
	t.Run("config url", func(t *testing.T) {
		e := newInstallEnv(t)
		e.seed(t)
		r, out := installRun(t, "install", "pi")
		if r.code != 0 || out["next"] != nil || manifestServer(t, e) != "https://feedback.example.test" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("differing --server", func(t *testing.T) {
		e := newInstallEnv(t)
		e.seed(t)
		before := snapshot(t, e.home)
		r, out := installRun(t, "install", "pi", "--server", "cloud")
		if r.code != 1 || !strings.Contains(out["message"].(string), "doctor --init --force") {
			t.Fatalf("%+v", r)
		}
		sameTree(t, "differing server", snapshot(t, e.home), before)
		if r, _ := installRun(t, "install", "pi", "--server", "https://feedback.example.test/"); r.code != 0 {
			t.Fatalf("same server with a slash: %+v", r)
		}
	})
	t.Run("manifest server reused", func(t *testing.T) {
		e := newInstallEnv(t)
		if r, out := installRun(t, "install", "pi", "--server", "cloud"); r.code != 0 || !strings.Contains(fmt.Sprint(out["next"]), "doctor --init --url "+cloudURL) {
			t.Fatalf("%+v", r)
		}
		if r, _ := installRun(t, "install", "omp"); r.code != 0 || manifestServer(t, e) != cloudURL {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("prompt", func(t *testing.T) {
		e := newInstallEnv(t)
		isTerminal = func() bool { return true }
		r := runCLI(t, "https://prompted.example.test\n", "install", "pi")
		if r.code != 0 || !strings.Contains(r.stderr, `AgentFeedback server ("cloud" or a URL): `) || manifestServer(t, e) != "https://prompted.example.test" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("bad --server", func(t *testing.T) {
		newInstallEnv(t)
		if r, _ := installRun(t, "install", "pi", "--server", "ftp://x"); r.code != 2 {
			t.Fatalf("%+v", r)
		}
	})
}

func TestInstall_ChangesNothing(t *testing.T) {
	for _, args := range [][]string{
		{"install"},
		{"install", "--list"},
		{"install", "--list", "--json"},
		{"install", "claude-code", "codex", "--dry-run"},
		{"install", "all", "--dry-run", "--mcp"},
		{"uninstall", "all", "--dry-run"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			e := newInstallEnv(t)
			e.seed(t)
			e.fakeClaude(t)
			before := snapshot(t, e.home)
			r := runCLI(t, "", args...)
			if r.code != 0 {
				t.Fatalf("%+v", r)
			}
			sameTree(t, "after "+strings.Join(args, " "), snapshot(t, e.home), before)
			if _, err := os.Stat(e.claudeLog); err == nil {
				t.Fatal("claude was run")
			}
			switch {
			case args[len(args)-1] == "install" || args[len(args)-1] == "--list":
				if !strings.HasPrefix(r.stdout, "HARNESS") || !strings.Contains(r.stdout, "claude-code  binary") {
					t.Fatalf("table: %s", r.stdout)
				}
			case args[len(args)-1] == "--json":
				var v struct{ Harnesses []harness.HarnessStatus }
				if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || len(v.Harnesses) != 6 {
					t.Fatalf("json: %s", r.stdout)
				}
			case args[0] == "install":
				var v installOutcome
				if err := json.Unmarshal([]byte(lastLine(r.stdout)), &v); err != nil || v.Status != "dry_run" || len(v.Changed) == 0 {
					t.Fatalf("dry run: %s", r.stdout)
				}
				if !strings.Contains(r.stderr, "would change") {
					t.Fatalf("dry run stderr: %s", r.stderr)
				}
			}
		})
	}
}

func TestInstall_CommandLine(t *testing.T) {
	newInstallEnv(t)
	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"install", "vim"}, 2, `unknown harness "vim"`},
		{[]string{"uninstall"}, 2, "wrong number of arguments for uninstall"},
		{[]string{"uninstall", "emacs"}, 2, `unknown harness "emacs"`},
		{[]string{"install", "all", "--server", "cloud"}, 1, "no harness was detected"},
	} {
		r, out := installRun(t, tt.args...)
		if r.code != tt.code || !strings.Contains(out["message"].(string), tt.want) {
			t.Errorf("%v: %+v", tt.args, r)
		}
	}
	r, out := installRun(t, "uninstall", "pi")
	if r.code != 0 || out["status"] != "unchanged" || !strings.Contains(r.stdout, "not in the install manifest") {
		t.Errorf("uninstall of a harness not installed: %+v", r)
	}
	orig := goos
	goos = "windows"
	t.Cleanup(func() { goos = orig })
	if r, _ := installRun(t, "install", "pi"); r.code != 2 {
		t.Errorf("windows: %+v", r)
	}
}

func TestSkillReminder(t *testing.T) {
	isolateCLI(t)
	r := runCLI(t, "", "skill", "reminder")
	want, _ := skillgen.Reminder()
	if r.code != 0 || r.stdout != want+"\n" || !strings.HasPrefix(want, "AgentFeedback is set up here:") {
		t.Fatalf("%+v", r)
	}
}

// spoolOne writes one due entry the way the client spools it.
func spoolOne(t *testing.T, cache string) {
	t.Helper()
	now := time.Now().UTC()
	entry := fmt.Sprintf(`{"v":1,"kind":"friction","key":"k-hook","created_at":%q,"attempts":0,"not_before":%q,"last_error":"","body":{"kind":"friction","key":"k-hook","summary":"s"}}`,
		now.Add(-time.Minute).Format(time.RFC3339Nano), now.Add(-time.Minute).Format(time.RFC3339Nano))
	putFile(t, filepath.Join(client.SpoolDir(cache), "af1-20260101T000000.000000001Z-00000000.json"), entry, 0o600)
}

func hookLogLines(t *testing.T, cache string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(client.LogPath(cache))
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}

	return out
}

func TestFlushHook(t *testing.T) {
	t.Run("empty spool", func(t *testing.T) {
		_, cache := isolate(t)
		var hits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		defer srv.Close()
		t.Setenv(envURL, srv.URL)
		t.Setenv(envAPIKey, "throwaway")
		r := runCLI(t, "", "flush", "--hook")
		if r.code != 0 || r.stdout != "" || hits.Load() != 0 {
			t.Fatalf("%+v hits=%d", r, hits.Load())
		}
		if _, err := os.Stat(client.LogPath(cache)); err == nil {
			t.Fatal("an empty spool wrote a log line")
		}
	})
	t.Run("hanging server", func(t *testing.T) {
		_, cache := isolate(t)
		oldSend, old, oldFlush := flushHookSend, flushHookDeadline, flushHookFlush
		flushHookSend, flushHookDeadline = 300*time.Millisecond, 3*time.Second
		// The bookkeeping after the cancelled send is stretched well past
		// the send deadline, so a hook that stops waiting at the send
		// deadline returns before it, every time.
		var finished atomic.Bool
		flushHookFlush = func(ctx context.Context, c *client.Client) client.FlushReport {
			rep := c.Flush(ctx)
			time.Sleep(500 * time.Millisecond)
			finished.Store(true)

			return rep
		}
		t.Cleanup(func() { flushHookSend, flushHookDeadline, flushHookFlush = oldSend, old, oldFlush })
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		defer srv.Close()
		defer close(release)
		t.Setenv(envURL, srv.URL)
		t.Setenv(envAPIKey, "throwaway")
		spoolOne(t, cache)
		start := time.Now()
		r := runCLI(t, "", "flush", "--hook")
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("flush --hook took %v", took)
		}
		if r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
		var found bool
		for _, l := range hookLogLines(t, cache) {
			found = found || (l["outcome"] == "error" && strings.Contains(fmt.Sprint(l["reason"]), "flush --hook"))
		}
		if !found {
			t.Fatalf("no error line in client.jsonl: %v", hookLogLines(t, cache))
		}
		if !finished.Load() {
			t.Fatal("the hook returned before the flush finished its bookkeeping")
		}
		// The flush finished its bookkeeping after the cancelled send: the
		// entry is released, not left claimed, with one more attempt.
		entries, _ := os.ReadDir(client.SpoolDir(cache))
		if len(entries) != 1 || strings.Contains(entries[0].Name(), "inflight") {
			t.Fatalf("spool after the hook: %v", entries)
		}
		data, _ := os.ReadFile(filepath.Join(client.SpoolDir(cache), entries[0].Name()))
		var entry struct {
			Attempts  int       `json:"attempts"`
			NotBefore time.Time `json:"not_before"`
		}
		if err := json.Unmarshal(data, &entry); err != nil || entry.Attempts != 1 || !entry.NotBefore.After(start) {
			t.Fatalf("entry not rescheduled: %s %v", data, err)
		}
	})
	t.Run("slow body", func(t *testing.T) {
		_, cache := isolate(t)
		oldSend, old, oldHasDue := flushHookSend, flushHookDeadline, flushHookHasDue
		entered := make(chan struct{}, 1)
		flushHookSend, flushHookDeadline = 100*time.Millisecond, 300*time.Millisecond
		flushHookHasDue = func(string, time.Time) bool {
			entered <- struct{}{}
			time.Sleep(2 * time.Second)

			return true
		}
		t.Cleanup(func() { flushHookSend, flushHookDeadline, flushHookHasDue = oldSend, old, oldHasDue })
		start := time.Now()
		r := runCLI(t, "", "flush", "--hook")
		if took := time.Since(start); took > time.Second {
			t.Fatalf("flush --hook took %v with a slow body", took)
		}
		<-entered
		if r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
		lines := hookLogLines(t, cache)
		if len(lines) != 1 || !strings.Contains(fmt.Sprint(lines[0]["reason"]), "ran past its deadline") {
			t.Fatalf("log %v", lines)
		}
	})
	t.Run("missing config", func(t *testing.T) {
		_, cache := isolate(t)
		spoolOne(t, cache)
		r := runCLI(t, "", "flush", "--hook")
		if r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
		lines := hookLogLines(t, cache)
		if len(lines) != 1 || lines[0]["outcome"] != "error" || !strings.Contains(fmt.Sprint(lines[0]["reason"]), "no server URL is set") {
			t.Fatalf("log %v", lines)
		}
	})
	t.Run("undecodable entry", func(t *testing.T) {
		// An entry that cannot be decoded counts as due, so the hook goes on
		// to Flush (which quarantines it) instead of returning early.
		_, cache := isolate(t)
		putFile(t, filepath.Join(client.SpoolDir(cache), "af1-20260101T000000.000000001Z-00000000.json"), "garbage", 0o600)
		r := runCLI(t, "", "flush", "--hook")
		if r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Fatalf("%+v", r)
		}
		if lines := hookLogLines(t, cache); len(lines) == 0 {
			t.Fatal("the hook returned early on an undecodable entry")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		if flushHookSend != 3500*time.Millisecond || flushHookDeadline != 4500*time.Millisecond {
			t.Fatalf("deadlines %v %v, want 3.5s and 4.5s", flushHookSend, flushHookDeadline)
		}
	})
}

func readManifest(t *testing.T, e installEnv) harness.Manifest {
	t.Helper()
	var m harness.Manifest
	data, err := os.ReadFile(filepath.Join(e.home, ".config", "agentfeedback", "install.json"))
	if err != nil || json.Unmarshal(data, &m) != nil {
		t.Fatalf("manifest %s %v", data, err)
	}

	return m
}

func notesOf(out map[string]any) string {
	var b strings.Builder
	hs, _ := out["harnesses"].([]any)
	for _, h := range hs {
		notes, _ := h.(map[string]any)["notes"].([]any)
		for _, n := range notes {
			b.WriteString(fmt.Sprint(n) + "\n")
		}
	}

	return b.String()
}

// readOnly makes dir unwritable until the test ends or restore is called.
func readOnly(t *testing.T, dir string) (restore func()) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	restore = func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	return restore
}

func TestInstall_FailedWriteManifestMatchesDisk(t *testing.T) {
	mustRun := func(t *testing.T, code int, args ...string) map[string]any {
		t.Helper()
		r, out := installRun(t, args...)
		if r.code != code {
			t.Fatalf("%v: %+v", args, r)
		}

		return out
	}
	t.Run("uninstall retried", func(t *testing.T) {
		e := newInstallEnv(t)
		e.seed(t)
		before := snapshot(t, e.home)
		mustRun(t, 0, "install", "omp", "pi")
		restore := readOnly(t, filepath.Join(e.home, ".pi", "agent", "extensions"))
		mustRun(t, 1, "uninstall", "pi")
		restore()
		mustRun(t, 0, "uninstall", "pi")
		mustRun(t, 0, "uninstall", "omp")
		sameTree(t, "after the retried uninstall", snapshot(t, e.home), before)
	})
	t.Run("install retried after the binary moved", func(t *testing.T) {
		e := newInstallEnv(t)
		e.seed(t)
		before := snapshot(t, e.home)
		mustRun(t, 0, "install", "omp", "pi")
		moved := filepath.Join(e.bin, "agentfeedback-2")
		putFile(t, moved, "#!/bin/sh\n", 0o755)
		orig := executable
		executable = func() (string, error) { return moved, nil }
		t.Cleanup(func() { executable = orig })
		restore := readOnly(t, filepath.Join(e.home, ".pi", "agent", "extensions"))
		out := mustRun(t, 1, "install", "omp", "pi")
		if strings.Contains(fmt.Sprint(out["changed"]), filepath.Join(e.home, ".pi")) {
			t.Errorf("changed lists a pi file that was not written: %v", out["changed"])
		}
		restore()
		mustRun(t, 0, "install", "omp", "pi")
		ext, _ := os.ReadFile(filepath.Join(e.home, ".pi", "agent", "extensions", "agentfeedback.ts"))
		if !strings.Contains(string(ext), moved) {
			t.Fatalf("pi extension not rewritten:\n%s", ext)
		}
		mustRun(t, 0, "uninstall", "all")
		sameTree(t, "after uninstall", snapshot(t, e.home), before)
	})
	t.Run("install retried with another server", func(t *testing.T) {
		e := newInstallEnv(t)
		before := snapshot(t, e.home)
		mustRun(t, 0, "install", "pi", "--mcp", "--server", "https://a.example.test")
		restore := readOnly(t, filepath.Join(e.home, ".pi", "agent"))
		mustRun(t, 1, "install", "pi", "--mcp", "--server", "https://b.example.test")
		if m := readManifest(t, e); !strings.Contains(string(m.Harnesses["pi"].Items[0].Value), "a.example.test") {
			t.Fatalf("the manifest does not match the disk: %s", m.Harnesses["pi"].Items[0].Value)
		}
		restore()
		mustRun(t, 0, "install", "pi", "--mcp", "--server", "https://b.example.test")
		data, _ := os.ReadFile(filepath.Join(e.home, ".pi", "agent", "mcp.json"))
		if !strings.Contains(string(data), "b.example.test") || strings.Contains(string(data), "a.example.test") {
			t.Fatalf("mcp.json:\n%s", data)
		}
		mustRun(t, 0, "uninstall", "pi")
		sameTree(t, "after uninstall", snapshot(t, e.home), before)
	})
}

func TestUninstall_EntriesRemovedByHand(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	before := snapshot(t, e.home)
	omp := filepath.Join(e.home, ".omp", "agent", "mcp.json")
	if r, _ := installRun(t, "install", "omp", "--mcp"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	// The user takes the entry out again by hand.
	putFile(t, omp, before[".omp/agent/mcp.json"], 0o644)
	r, out := installRun(t, "uninstall", "omp")
	if r.code != 0 || out["backups"] != nil {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after uninstall", snapshot(t, e.home), before)
}

func TestInstall_OrphanBackup(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	omp := filepath.Join(e.home, ".omp", "agent", "mcp.json")
	before := snapshot(t, e.home)
	// An interrupted run left a backup equal to the file: it is taken over.
	putFile(t, omp+harness.BackupSuffix, before[".omp/agent/mcp.json"], 0o644)
	if r, _ := installRun(t, "install", "omp", "--mcp"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if m := readManifest(t, e); m.Files[omp] == nil || m.Files[omp].Backup != omp+harness.BackupSuffix {
		t.Fatalf("orphan backup not recorded: %+v", m.Files[omp])
	}
	if r, _ := installRun(t, "uninstall", "omp"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after uninstall", snapshot(t, e.home), before)
	// One that differs from the file is still refused.
	putFile(t, omp+harness.BackupSuffix, "{}\n", 0o644)
	if r, out := installRun(t, "install", "omp", "--mcp"); r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), "already exists and the install manifest does not record it") {
		t.Fatalf("%+v", r)
	}
}

func TestInstall_RecordedCodexHome(t *testing.T) {
	e := newInstallEnv(t)
	first := filepath.Join(e.home, "codex-a")
	t.Setenv("CODEX_HOME", first)
	before := snapshot(t, e.home)
	if r, _ := installRun(t, "install", "codex", "--mcp", "--server", "https://feedback.example.test"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if m := readManifest(t, e); m.CodexHome != first {
		t.Fatalf("codex_home %q", m.CodexHome)
	}
	t.Setenv("CODEX_HOME", filepath.Join(e.home, "codex-b"))
	r, out := installRun(t, "uninstall", "codex")
	if r.code != 0 || !strings.Contains(notesOf(out), "codex was installed with CODEX_HOME="+first+"; using that location") {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after uninstall", snapshot(t, e.home), before)
}

func TestInstall_ClaudeConfigDir(t *testing.T) {
	e := newInstallEnv(t)
	e.fakeClaude(t)
	dir := filepath.Join(e.home, ".claude-work")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	before := snapshot(t, e.home)
	if r, _ := installRun(t, "install", "claude-code", "--server", "https://feedback.example.test"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	for _, f := range []string{filepath.Join(dir, "skills", "agentfeedback", "SKILL.md"), filepath.Join(dir, "settings.json")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.home, ".claude")); err == nil {
		t.Error("~/.claude was written with CLAUDE_CONFIG_DIR set")
	}
	if r, _ := installRun(t, "uninstall", "claude-code"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after uninstall", snapshot(t, e.home), before)

	// With a relocated configuration, ~/.claude.json does not tell whether
	// the entry is there: uninstall always runs claude mcp remove.
	if r, _ := installRun(t, "install", "claude-code", "--mcp", "--server", "https://feedback.example.test"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	writeClaudeJSON(t, e, "")
	if r, _ := installRun(t, "uninstall", "claude-code"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if log, _ := os.ReadFile(e.claudeLog); !strings.HasSuffix(string(log), "mcp remove agentfeedback --scope user\n") {
		t.Fatalf("claude log %q", log)
	}
}

func TestInstall_SymlinkedSkillDir(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	src := filepath.Join(e.home, "checkout", "skills", "agentfeedback")
	putFile(t, filepath.Join(src, "SKILL.md"), "mine\n", 0o644)
	link := filepath.Join(e.home, ".claude", "skills", "agentfeedback")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}
	r, out := installRun(t, "install", "claude-code")
	if r.code != 1 || fmt.Sprint(out["message"]) != link+" is a symbolic link; agentfeedback install edits only regular files; remove it, then run agentfeedback install again." {
		t.Fatalf("%+v", r)
	}
}

func TestUninstall_BackupMissingOrReplaced(t *testing.T) {
	for _, tt := range []struct {
		name     string
		mangle   func(path string) error
		wantNote string
		kept     bool
	}{
		{"missing", os.Remove, "is missing", false},
		{"replaced", func(p string) error { return os.WriteFile(p, []byte("{\"x\": 1}\n"), 0o644) }, "is not the copy install took", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.seed(t)
			omp := filepath.Join(e.home, ".omp", "agent", "mcp.json")
			orig, _ := os.ReadFile(omp)
			if r, _ := installRun(t, "install", "omp", "--mcp"); r.code != 0 {
				t.Fatalf("%+v", r)
			}
			if err := tt.mangle(omp + harness.BackupSuffix); err != nil {
				t.Fatal(err)
			}
			r, out := installRun(t, "uninstall", "omp")
			if r.code != 0 || !strings.Contains(notesOf(out), tt.wantNote) {
				t.Fatalf("%+v", r)
			}
			if got, _ := os.ReadFile(omp); string(got) != string(orig) {
				t.Errorf("mcp.json is not the surgical result:\n%s", got)
			}
			_, err := os.Stat(omp + harness.BackupSuffix)
			if tt.kept != (err == nil) || tt.kept != strings.Contains(fmt.Sprint(out["backups"]), omp+harness.BackupSuffix) {
				t.Errorf("backup kept=%v, want %v: %v", err == nil, tt.kept, out["backups"])
			}
		})
	}
}

// writeClaudeJSON stands in for the state claude keeps in ~/.claude.json.
func writeClaudeJSON(t *testing.T, e installEnv, url string) {
	t.Helper()
	servers := ""
	if url != "" {
		servers = `"agentfeedback": {"type": "http", "url": "` + url + `"}`
	}
	putFile(t, filepath.Join(e.home, ".claude.json"), `{"mcpServers": {`+servers+`}}`, 0o644)
}

func TestInstall_ClaudeMCPState(t *testing.T) {
	const srv, mcpURL = "https://feedback.example.test", "https://feedback.example.test/mcp"
	setup := func(t *testing.T) installEnv {
		e := newInstallEnv(t)
		e.fakeClaude(t)
		if r, _ := installRun(t, "install", "claude-code", "--mcp", "--server", srv); r.code != 0 {
			t.Fatalf("%+v", r)
		}
		if err := os.Remove(e.claudeLog); err != nil {
			t.Fatal(err)
		}

		return e
	}
	claudeLog := func(e installEnv) string { data, _ := os.ReadFile(e.claudeLog); return string(data) }

	t.Run("present is unchanged", func(t *testing.T) {
		e := setup(t)
		writeClaudeJSON(t, e, mcpURL)
		if r, out := installRun(t, "install", "claude-code", "--mcp"); r.code != 0 || out["status"] != "unchanged" || claudeLog(e) != "" {
			t.Fatalf("%+v %q", r, claudeLog(e))
		}
	})
	t.Run("absent is added again", func(t *testing.T) {
		e := setup(t)
		writeClaudeJSON(t, e, "")
		r, out := installRun(t, "install", "claude-code", "--mcp")
		if r.code != 0 || out["status"] != "installed" || !strings.HasPrefix(claudeLog(e), "mcp add-json") || strings.Contains(claudeLog(e), "remove") {
			t.Fatalf("%+v %q", r, claudeLog(e))
		}
	})
	t.Run("other url is refused", func(t *testing.T) {
		e := setup(t)
		writeClaudeJSON(t, e, "https://elsewhere.example.test/mcp")
		mpath := filepath.Join(e.home, ".config", "agentfeedback", "install.json")
		before, _ := os.ReadFile(mpath)
		r, out := installRun(t, "install", "claude-code", "--mcp")
		if r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), "elsewhere.example.test") || claudeLog(e) != "" {
			t.Fatalf("%+v %q", r, claudeLog(e))
		}
		if after, _ := os.ReadFile(mpath); string(after) != string(before) {
			t.Errorf("manifest changed: %s", after)
		}
	})
	t.Run("uninstall absent", func(t *testing.T) {
		e := setup(t)
		writeClaudeJSON(t, e, "")
		r, out := installRun(t, "uninstall", "claude-code")
		if r.code != 0 || !strings.Contains(notesOf(out), "already removed") || claudeLog(e) != "" {
			t.Fatalf("%+v %q", r, claudeLog(e))
		}
	})
	t.Run("uninstall other url", func(t *testing.T) {
		e := setup(t)
		writeClaudeJSON(t, e, "https://elsewhere.example.test/mcp")
		r, out := installRun(t, "uninstall", "claude-code")
		if r.code != 0 || !strings.Contains(notesOf(out), "left in place") || claudeLog(e) != "" {
			t.Fatalf("%+v %q", r, claudeLog(e))
		}
		if _, err := os.Stat(filepath.Join(e.home, ".config", "agentfeedback", "install.json")); err == nil {
			t.Error("the record was not dropped")
		}
	})
	t.Run("uninstall without claude", func(t *testing.T) {
		e := setup(t)
		if err := os.Remove(filepath.Join(e.bin, "claude")); err != nil {
			t.Fatal(err)
		}
		r, out := installRun(t, "uninstall", "claude-code")
		if r.code != 0 || !strings.Contains(notesOf(out), "claude is not on PATH; remove the Claude Code MCP entry by hand with claude mcp remove agentfeedback --scope user") {
			t.Fatalf("%+v", r)
		}
		if _, err := os.Stat(filepath.Join(e.home, ".config", "agentfeedback", "install.json")); err == nil {
			t.Error("the record was not dropped")
		}
	})
	t.Run("uninstall when remove fails", func(t *testing.T) {
		e := setup(t)
		putFile(t, filepath.Join(e.bin, "claude"), "#!/bin/sh\necho boom >&2\nexit 1\n", 0o755)
		r, out := installRun(t, "uninstall", "claude-code")
		if r.code != 0 || out["status"] != "uninstalled" || !strings.Contains(notesOf(out), "claude mcp remove agentfeedback --scope user") {
			t.Fatalf("%+v", r)
		}
		if _, err := os.Stat(filepath.Join(e.home, ".config", "agentfeedback", "install.json")); err == nil {
			t.Error("the record was not dropped")
		}
	})
}

func TestInstall_ChangedJSONMember(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	if r, _ := installRun(t, "install", "cursor", "--mcp"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	path := filepath.Join(e.home, ".cursor", "mcp.json")
	data, _ := os.ReadFile(path)
	edited := strings.Replace(string(data), "https://feedback.example.test/mcp", "https://mine.example.test/mcp", 1)
	putFile(t, path, edited, 0o644)
	r, out := installRun(t, "install", "cursor", "--mcp")
	if r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), "mcpServers.agentfeedback was changed since install") {
		t.Fatalf("install over a changed member: %+v", r)
	}
	r, out = installRun(t, "uninstall", "cursor")
	if r.code != 0 || !strings.Contains(notesOf(out), path+": mcpServers.agentfeedback was changed since install and is left in place") {
		t.Fatalf("%+v", r)
	}
	if got, _ := os.ReadFile(path); string(got) != edited {
		t.Errorf("the changed member was not left in place:\n%s", got)
	}
	if !strings.Contains(fmt.Sprint(out["backups"]), path+harness.BackupSuffix) {
		t.Errorf("backup not listed: %v", out["backups"])
	}
}

func TestInstall_DivergedFile(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	if r, _ := installRun(t, "install", "claude-code"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	path := filepath.Join(e.home, ".claude", "settings.json")
	data, _ := os.ReadFile(path)
	// The harness rewrites its own file; the next install still edits it.
	putFile(t, path, strings.Replace(string(data), `"model": "opus",`, `"model": "opus", "theme": "dark",`, 1), 0o600)
	if r, _ := installRun(t, "install", "claude-code", "--with-reminder"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if m := readManifest(t, e); !m.Files[path].Diverged {
		t.Fatalf("not marked diverged: %+v", m.Files[path])
	}
	// A later install leaves the mark in place.
	if r, _ := installRun(t, "install", "claude-code"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if m := readManifest(t, e); !m.Files[path].Diverged {
		t.Fatal("the diverged mark did not stick")
	}
	r, out := installRun(t, "uninstall", "claude-code")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), `"theme": "dark"`) || strings.Contains(string(got), "flush --hook") {
		t.Errorf("settings.json was restored from the backup or kept the hook:\n%s", got)
	}
	if _, err := os.Stat(path + harness.BackupSuffix); err != nil || !strings.Contains(fmt.Sprint(out["backups"]), path+harness.BackupSuffix) {
		t.Errorf("backup not kept and listed: %v %v", err, out["backups"])
	}
}

func TestInstall_ServerSafety(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		e := newInstallEnv(t)
		before := snapshot(t, e.home)
		r, out := installRun(t, "install", "pi", "--server", "https://feedback.example.test/$(touch x)")
		if r.code != 2 || !strings.Contains(fmt.Sprint(out["message"]), "not safe in a shell command") {
			t.Fatalf("%+v", r)
		}
		sameTree(t, "after a refused --server", snapshot(t, e.home), before)
	})
	t.Run("config", func(t *testing.T) {
		e := newInstallEnv(t)
		cfg := filepath.Join(e.home, ".config", "agentfeedback", "config.toml")
		putFile(t, cfg, "url = \"https://user:pass@feedback.example.test\"\napi_key = \"throwaway\"\n", 0o600)
		before := snapshot(t, e.home)
		r, out := installRun(t, "install", "pi")
		msg := fmt.Sprint(out["message"])
		if r.code != 1 || !strings.Contains(msg, cfg) || !strings.Contains(msg, "carries credentials") || strings.Contains(msg, "user:pass") {
			t.Fatalf("%+v", r)
		}
		sameTree(t, "after a refused config url", snapshot(t, e.home), before)
	})
}

func TestInstall_ManifestValidation(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	if r, _ := installRun(t, "install", "pi"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	mpath := filepath.Join(e.home, ".config", "agentfeedback", "install.json")
	data, _ := os.ReadFile(mpath)
	victim := filepath.Join(e.home, "notes.txt")
	putFile(t, victim, "mine\n", 0o644)
	bad := strings.Replace(string(data), filepath.Join(e.home, ".pi", "agent", "skills", "agentfeedback", "SKILL.md"), victim, -1)
	putFile(t, mpath, bad, 0o600)
	before := snapshot(t, e.home)
	for _, args := range [][]string{{"install", "pi"}, {"uninstall", "pi"}} {
		r, out := installRun(t, args...)
		if r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), "names "+victim+", which agentfeedback install does not write under this environment; run with the HOME, XDG_CONFIG_HOME, CODEX_HOME and CLAUDE_CONFIG_DIR of the install, or fix the manifest") {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	sameTree(t, "after a refused manifest", snapshot(t, e.home), before)
}

func TestInstall_AllPlusNamed(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	for _, d := range []string{".codex", ".cursor", ".config/opencode", ".omp", ".pi"} {
		if err := os.RemoveAll(filepath.Join(e.home, d)); err != nil {
			t.Fatal(err)
		}
	}
	r, out := installRun(t, "install", "all", "pi")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	m := readManifest(t, e)
	if len(m.Harnesses) != 2 || m.Harnesses["claude-code"] == nil || m.Harnesses["pi"] == nil {
		t.Fatalf("harnesses %v", m.Harnesses)
	}
	if !strings.Contains(notesOf(out), "pi was not detected") {
		t.Errorf("notes %q", notesOf(out))
	}
}

func TestInstall_SymlinkedConfig(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	path := filepath.Join(e.home, ".claude", "settings.json")
	real := filepath.Join(e.home, "dotfiles-settings.json")
	if err := os.Rename(path, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, e.home)
	r, out := installRun(t, "install", "claude-code")
	msg := fmt.Sprint(out["message"])
	if r.code != 1 || !strings.Contains(msg, path+" is a symbolic link; agentfeedback install edits only regular files") ||
		!strings.Contains(msg, "replace the link with the file it points to") {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after a refused symlink", snapshot(t, e.home), before)
}

func TestInstall_ManifestValidationBackupAndDirs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mangle func(m map[string]any, e installEnv) string
	}{
		{"backup", func(m map[string]any, e installEnv) string {
			x := filepath.Join(e.home, "notes.txt")
			for _, rec := range m["files"].(map[string]any) {
				rec.(map[string]any)["backup"] = x
			}

			return x
		}},
		{"dirs_created", func(m map[string]any, e installEnv) string {
			x := filepath.Join(e.home, "projects")
			m["dirs_created"] = append(m["dirs_created"].([]any), x)

			return x
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.seed(t)
			if r, _ := installRun(t, "install", "omp", "--mcp"); r.code != 0 {
				t.Fatalf("%+v", r)
			}
			mpath := filepath.Join(e.home, ".config", "agentfeedback", "install.json")
			data, _ := os.ReadFile(mpath)
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			if m["dirs_created"] == nil {
				m["dirs_created"] = []any{}
			}
			x := tt.mangle(m, e)
			bad, _ := json.Marshal(m)
			putFile(t, mpath, string(bad), 0o600)
			before := snapshot(t, e.home)
			r, out := installRun(t, "uninstall", "omp")
			if r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), "names "+x+", which agentfeedback install does not write") {
				t.Fatalf("%+v", r)
			}
			sameTree(t, "after a refused manifest", snapshot(t, e.home), before)
		})
	}
}

func TestInstall_FirstManifestWriteFails(t *testing.T) {
	e := newInstallEnv(t)
	cfg := filepath.Join(e.home, ".config")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, e.home)
	restore := readOnly(t, cfg)
	if r, _ := installRun(t, "install", "pi", "--server", "https://feedback.example.test"); r.code != 1 {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after the failed first write", snapshot(t, e.home), before)
	restore()
	if r, _ := installRun(t, "install", "pi", "--server", "https://feedback.example.test"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestInstall_ClaudeRunsWithRecordedConfigDir(t *testing.T) {
	e := newInstallEnv(t)
	log := filepath.Join(e.home, "..", "claude-env.log")
	putFile(t, filepath.Join(e.bin, "claude"), "#!/bin/sh\necho \"dir=${CLAUDE_CONFIG_DIR-unset} $1 $2\" >> "+log+"\nexit 0\n", 0o755)
	work := filepath.Join(e.home, ".claude-work")
	t.Setenv("CLAUDE_CONFIG_DIR", work)
	if r, _ := installRun(t, "install", "claude-code", "--mcp", "--server", "https://feedback.example.test"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(e.home, ".claude-other"))
	if r, _ := installRun(t, "uninstall", "claude-code"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	// Installed with the default directory: claude runs with it unset.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if r, _ := installRun(t, "install", "claude-code", "--mcp", "--server", "https://feedback.example.test"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(e.home, ".claude-other"))
	if r, _ := installRun(t, "uninstall", "claude-code"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	data, _ := os.ReadFile(log)
	want := "dir=" + work + " mcp add-json\ndir=" + work + " mcp remove\ndir=unset mcp add-json\ndir=unset mcp remove\n"
	if string(data) != want {
		t.Fatalf("claude ran with\n%s\nwant\n%s", data, want)
	}
}

func TestInstall_SymlinkedPluginFile(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	real := filepath.Join(e.home, "mine.ts")
	putFile(t, real, "// mine\n", 0o644)
	link := filepath.Join(e.home, ".pi", "agent", "extensions", "agentfeedback.ts")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	r, out := installRun(t, "install", "pi")
	if r.code != 1 || fmt.Sprint(out["message"]) != link+" is a symbolic link; agentfeedback install edits only regular files; remove it, then run agentfeedback install again." {
		t.Fatalf("%+v", r)
	}
}

// docsTree is the docs skill as install --docs writes it, relative to its
// directory, with the directories it holds.
func docsTree(t *testing.T) map[string]string {
	t.Helper()
	files, err := skillgen.Docs()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		p := filepath.FromSlash(f.Path)
		out[p] = string(f.Data)
		for d := filepath.Dir(p); d != "."; d = filepath.Dir(d) {
			out[d+"/"] = ""
		}
	}

	return out
}

func harnessField(t *testing.T, out map[string]any, name, field string) any {
	t.Helper()
	hs, _ := out["harnesses"].([]any)
	for _, h := range hs {
		if m := h.(map[string]any); m["name"] == name {
			return m[field]
		}
	}
	t.Fatalf("no harness %s in %v", name, out)

	return nil
}

func TestInstall_Docs(t *testing.T) {
	dirs := map[string]string{"claude-code": filepath.Join(".claude", "skills", "agentfeedback-docs"), "codex": filepath.Join(".agents", "skills", "agentfeedback-docs")}
	for _, name := range []string{"claude-code", "codex"} {
		for _, mode := range []string{"cli", "mcp"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				e := newInstallEnv(t)
				e.fakeClaude(t)
				before := snapshot(t, e.home)
				dir := filepath.Join(e.home, dirs[name])
				args := []string{"install", name, "--server", "https://feedback.example.test"}
				if mode == "mcp" {
					args = append(args, "--mcp")
				}
				withDocs := append(slices.Clone(args), "--docs")

				r, out := installRun(t, withDocs...)
				if r.code != 0 || out["status"] != "installed" || harnessField(t, out, name, "docs") != "wired" {
					t.Fatalf("install --docs: %+v", r)
				}
				got := snapshot(t, dir)
				delete(got, "./")
				sameTree(t, "docs skill", got, docsTree(t))
				if m := readManifest(t, e); !m.Harnesses[name].Docs {
					t.Error("the manifest does not record docs")
				}
				r = runCLI(t, "", "install", "--list", "--json")
				var list map[string]any
				if err := json.Unmarshal([]byte(lastLine(r.stdout)), &list); err != nil || harnessField(t, list, name, "docs") != "wired" {
					t.Fatalf("list --json: %+v", r)
				}
				if r := runCLI(t, "", "install", "--list"); !strings.Contains(r.stdout, "REMINDER  DOCS") {
					t.Errorf("list table: %q", r.stdout)
				}
				installed := snapshot(t, e.home)

				r, out = installRun(t, withDocs...)
				if r.code != 0 || out["status"] != "unchanged" {
					t.Fatalf("second install: %+v", r)
				}
				sameTree(t, "second install", snapshot(t, e.home), installed)

				// Without --docs the docs skill goes, directories and all.
				r, out = installRun(t, args...)
				if r.code != 0 || out["status"] != "installed" || harnessField(t, out, name, "docs") != "-" {
					t.Fatalf("install without --docs: %+v", r)
				}
				if !strings.Contains(r.stderr, "changed "+filepath.Join(dir, "SKILL.md")) {
					t.Errorf("the removal is not reported: %q", r.stderr)
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("docs skill left: %v", err)
				}

				if r, _ := installRun(t, withDocs...); r.code != 0 {
					t.Fatalf("%+v", r)
				}
				if r, out := installRun(t, "uninstall", name); r.code != 0 || out["status"] != "uninstalled" {
					t.Fatalf("uninstall: %+v", r)
				}
				sameTree(t, "after uninstall", snapshot(t, e.home), before)
			})
		}
	}
}

func TestInstall_DocsForeignAndModified(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	dir := filepath.Join(e.home, ".claude", "skills", "agentfeedback-docs")
	putFile(t, filepath.Join(dir, "SKILL.md"), "an older copy\n", 0o644)
	before := snapshot(t, e.home)
	r, out := installRun(t, "install", "claude-code", "--docs")
	if r.code != 1 || fmt.Sprint(out["message"]) != dir+" already holds an agentfeedback-docs skill directory, which agentfeedback install did not write; remove or rename it, then run agentfeedback install again." {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after a refusal", snapshot(t, e.home), before)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if r, _ := installRun(t, "install", "claude-code", "--docs"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	edited := filepath.Join(dir, "references", "docs", "api.md")
	putFile(t, edited, "my notes\n", 0o644)
	before = snapshot(t, e.home)
	r, out = installRun(t, "install", "claude-code")
	if r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), edited+" was modified since install") {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after a refused removal", snapshot(t, e.home), before)
	r, _ = installRun(t, "uninstall", "claude-code")
	if r.code != 0 || !strings.Contains(r.stdout, edited+" was modified since install and is left in place") {
		t.Fatalf("%+v", r)
	}
	if data, _ := os.ReadFile(edited); string(data) != "my notes\n" {
		t.Fatal("uninstall removed an edited docs file")
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("uninstall left the unedited docs files: %v", err)
	}
}

// A manifest written by another version names docs files this binary does
// not have; it is still valid, and its files are removed.
func TestInstall_ManifestValidationDocs(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	if r, _ := installRun(t, "install", "pi", "--docs"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	mpath := filepath.Join(e.home, ".config", "agentfeedback", "install.json")
	dir := filepath.Join(e.home, ".pi", "agent", "skills", "agentfeedback-docs")
	data, _ := os.ReadFile(mpath)
	gone := filepath.Join(dir, "references", "docs", "gone.md")
	renamed := strings.Replace(string(data), filepath.Join(dir, "references", "docs", "api.md"), gone, -1)
	if renamed == string(data) {
		t.Fatal("the manifest does not name api.md")
	}
	putFile(t, mpath, renamed, 0o600)
	if err := os.Rename(filepath.Join(dir, "references", "docs", "api.md"), gone); err != nil {
		t.Fatal(err)
	}
	if r, _ := installRun(t, "install", "pi", "--docs"); r.code != 0 {
		t.Fatalf("a recorded docs file of another version was refused: %+v", r)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("the old docs file is left: %v", err)
	}

	data, _ = os.ReadFile(mpath)
	for _, victim := range []string{dir + "/../../../../notes.txt", "/etc/passwd"} {
		bad := strings.Replace(string(data), filepath.Join(dir, "SKILL.md"), victim, -1)
		putFile(t, mpath, bad, 0o600)
		before := snapshot(t, e.home)
		r, out := installRun(t, "uninstall", "pi")
		if r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), "names "+victim+", which agentfeedback install does not write") {
			t.Fatalf("%s: %+v", victim, r)
		}
		sameTree(t, "after a refused manifest", snapshot(t, e.home), before)
	}
}
