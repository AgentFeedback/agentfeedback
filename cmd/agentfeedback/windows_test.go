package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubWindows makes the binary behave as on Windows for the test.
func stubWindows(t *testing.T) {
	t.Helper()
	orig := goos
	goos = "windows"
	t.Cleanup(func() { goos = orig })
}

// TestWindows_XDGLayout: the data, config and cache directories are the XDG
// layout under the home directory on Windows too.
func TestWindows_XDGLayout(t *testing.T) {
	stubWindows(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, "")
	}
	for _, c := range []struct {
		name string
		fn   func(func(string) string) (string, error)
		want string
	}{
		{"database", localDBPath, filepath.Join(home, ".local", "share", "agentfeedback", "agentfeedback.db")},
		{"config", configPath, filepath.Join(home, ".config", "agentfeedback", "config.toml")},
		{"cache", cacheDir, filepath.Join(home, ".cache", "agentfeedback")},
	} {
		if got, err := c.fn(os.Getenv); err != nil || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

// windowsOutcome is the refusal install ends with on Windows.
type windowsOutcome struct {
	Status string       `json:"status"`
	Manual []manualStep `json:"manual"`
}

func windowsInstall(t *testing.T, args ...string) (result, windowsOutcome) {
	t.Helper()
	r := runCLI(t, "", args...)
	var out windowsOutcome
	if err := json.Unmarshal([]byte(lastLine(r.stdout)), &out); err != nil {
		t.Fatalf("%v: %+v", args, r)
	}

	return r, out
}

// TestWindows_InstallManualSteps: install is refused on Windows with the
// wiring by hand of each harness, the MCP entry only with a server URL.
func TestWindows_InstallManualSteps(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		e := newInstallEnv(t)
		stubWindows(t)
		r, out := windowsInstall(t, "install", "claude-code")
		if r.code != 2 || out.Status != "error" || len(out.Manual) != 1 || !strings.Contains(r.stdout, "use the steps printed above to wire the harness by hand") {
			t.Fatalf("%+v", r)
		}
		s := out.Manual[0]
		skill := filepath.Join(e.home, ".claude", "skills", "agentfeedback", "SKILL.md")
		if s.Harness != "claude-code" || s.SkillPath != skill || s.SkillCommand != "New-Item -ItemType Directory -Force -Path '"+filepath.Dir(skill)+"' | Out-Null; [IO.File]::WriteAllText('"+skill+"', (agentfeedback skill render skill-md | Out-String))" ||
			s.RuleCommand != "agentfeedback skill render agents-md" || s.MCP != nil ||
			!strings.Contains(strings.Join(s.Notes, "\n"), "the MCP entry needs a server URL; local mode has none yet") {
			t.Fatalf("%+v", s)
		}
		if !strings.Contains(r.stderr, "claude-code: wire by hand") || !strings.Contains(r.stderr, s.SkillCommand) {
			t.Fatalf("stderr %q", r.stderr)
		}
	})
	t.Run("server", func(t *testing.T) {
		e := newInstallEnv(t)
		e.seed(t)
		stubWindows(t)
		r, out := windowsInstall(t, "install", "claude-code", "cursor")
		if r.code != 2 || out.Status != "error" || len(out.Manual) != 2 {
			t.Fatalf("%+v", r)
		}
		claude, cursor := out.Manual[0].MCP, out.Manual[1].MCP
		if claude == nil || !strings.HasPrefix(claude.Command, "claude mcp add-json agentfeedback ") ||
			!strings.Contains(claude.Command, "https://feedback.example.test/mcp") {
			t.Fatalf("claude-code %+v", claude)
		}
		if cursor == nil || cursor.File != filepath.Join(e.home, ".cursor", "mcp.json") || cursor.Key != "agentfeedback" ||
			!strings.Contains(string(cursor.Value), "https://feedback.example.test/mcp") {
			t.Fatalf("cursor %+v", cursor)
		}
	})
	t.Run("quote in path", func(t *testing.T) {
		e := newInstallEnv(t)
		home := filepath.Join(e.home, "o'brien")
		if err := os.MkdirAll(home, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		stubWindows(t)
		_, out := windowsInstall(t, "install", "claude-code")
		if len(out.Manual) != 1 {
			t.Fatalf("%+v", out)
		}
		dir := strings.ReplaceAll(filepath.Join(home, ".claude", "skills", "agentfeedback"), "'", "''")
		want := "New-Item -ItemType Directory -Force -Path '" + dir + "' | Out-Null; [IO.File]::WriteAllText('" + dir +
			string(filepath.Separator) + "SKILL.md', (agentfeedback skill render skill-md | Out-String))"
		if got := out.Manual[0].SkillCommand; got != want {
			t.Fatalf("got  %s\nwant %s", got, want)
		}
	})
	t.Run("server not usable", func(t *testing.T) {
		newInstallEnv(t)
		stubWindows(t)
		_, out := windowsInstall(t, "install", "claude-code", "--server", "ftp://x")
		if len(out.Manual) != 1 || out.Manual[0].MCP != nil ||
			!strings.HasPrefix(out.Manual[0].Notes[0], "the MCP entry cannot be computed: ") {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("list", func(t *testing.T) {
		e := newInstallEnv(t)
		putFile(t, filepath.Join(e.home, ".claude", "settings.json"), "{}\n", 0o600)
		stubWindows(t)
		r, out := windowsInstall(t, "install", "--list")
		if r.code != 2 || out.Status != "error" || len(out.Manual) != 1 || out.Manual[0].Harness != "claude-code" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("unknown name", func(t *testing.T) {
		newInstallEnv(t)
		stubWindows(t)
		r, out := windowsInstall(t, "install", "nope")
		if r.code != 2 || out.Manual != nil || !strings.Contains(r.stdout, `unknown harness \"nope\"`) {
			t.Fatalf("%+v", r)
		}
	})
}
