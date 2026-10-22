package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

// listStatus is one harness as install --list --json reports it.
func listStatus(t *testing.T, name string) map[string]any {
	t.Helper()
	r := runCLI(t, "", "install", "--list", "--json")
	var list map[string]any
	if err := json.Unmarshal([]byte(lastLine(r.stdout)), &list); err != nil {
		t.Fatalf("list --json: %+v", r)
	}
	for _, h := range list["harnesses"].([]any) {
		if h := h.(map[string]any); h["name"] == name {
			return h
		}
	}
	t.Fatalf("list --json lacks %s: %+v", name, r)

	return nil
}

// forget removes every record of file from the install manifest, as an
// install interrupted between writing the file and recording it leaves it.
func forget(t *testing.T, e installEnv, file string) {
	t.Helper()
	mpath := filepath.Join(e.home, ".config", "agentfeedback", "install.json")
	m := readManifest(t, e)
	found := false
	for _, rec := range m.Harnesses {
		rec.Items = slices.DeleteFunc(rec.Items, func(it harness.Item) bool {
			found = found || it.File == file

			return it.File == file
		})
	}
	if !found {
		t.Fatalf("the manifest does not record %s", file)
	}
	delete(m.Files, file)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, mpath, string(data)+"\n", 0o600)
}

func recordsFile(m harness.Manifest, file string) bool {
	for _, rec := range m.Harnesses {
		if slices.ContainsFunc(rec.Items, func(it harness.Item) bool { return it.File == file }) {
			return m.Files[file] != nil
		}
	}

	return false
}

// A skill a skills CLI installed (SKILL.md and scripts/install.sh, no
// manifest record) is left byte for byte; init wires the rest and reports
// the skill external, and uninstall leaves it.
func TestInit_SkillFromSkillsCLIIsExternal(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude")
	dir := filepath.Join(e.home, ".claude", "skills", "agentfeedback")
	skill, err := os.ReadFile(filepath.Join("..", "..", "skills", "agentfeedback", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(dir, "SKILL.md"), string(skill), 0o644)
	putFile(t, filepath.Join(dir, "scripts", "install.sh"), "#!/bin/sh\necho installer\n", 0o755)
	want := snapshot(t, dir)

	r, out := initRun(t, "", "--yes", "--harnesses", "claude-code")
	h := harnessByName(out.Harnesses, "claude-code")
	if r.code != 0 || out.Status != "ok" || h == nil || h.Skill != "external" || h.Hook != "wired" || h.Rule != "wired" || h.Triage != "wired" {
		t.Fatalf("init: %+v", r)
	}
	note := dir + " already exists and agentfeedback install did not write it; left as is (remove it and run agentfeedback install again to get this binary's agentfeedback skill)"
	if !slices.Contains(h.Notes, note) {
		t.Errorf("notes %q", h.Notes)
	}
	sameTree(t, "skill after init", snapshot(t, dir), want)
	if s := listStatus(t, "claude-code"); s["skill"] != "external" || s["hook"] != "wired" {
		t.Fatalf("list: %+v", s)
	}
	if r := runCLI(t, "", "init", "--yes", "--harnesses", "claude-code"); r.code != 0 || !strings.Contains(r.stdout, "skill external") {
		t.Fatalf("summary: %+v", r)
	}

	if r, out := installRun(t, "uninstall", "claude-code"); r.code != 0 || out["status"] != "uninstalled" {
		t.Fatalf("uninstall: %+v", r)
	}
	sameTree(t, "skill after uninstall", snapshot(t, dir), want)
	if s := listStatus(t, "claude-code"); s["skill"] != "external" || s["hook"] != "-" || s["rule"] != "-" || s["mode"] != "-" {
		t.Fatalf("list after uninstall: %+v", s)
	}
	if data, _ := os.ReadFile(filepath.Join(e.home, ".claude", "CLAUDE.md")); strings.Contains(string(data), "agentfeedback") {
		t.Errorf("the rule is left:\n%s", data)
	}
}

// A Claude Code plugin install provides the skill: install writes none,
// removes the copy an earlier install wrote, and never touches the plugin.
func TestInstall_ClaudePluginSkill(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	skills := filepath.Join(e.home, ".claude", "skills", "agentfeedback")
	if r, out := installRun(t, "install", "claude-code"); r.code != 0 || harnessField(t, out, "claude-code", "skill") != "wired" {
		t.Fatalf("install: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(skills, "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	plugins := filepath.Join(e.home, ".claude", "plugins")
	putFile(t, filepath.Join(plugins, "installed_plugins.json"), `{"version":2,"plugins":{"agentfeedback@agentfeedback":[{"scope":"user","installPath":"`+
		filepath.Join(plugins, "cache", "agentfeedback", "agentfeedback", "5.0.0")+`","version":"5.0.0"}]}}`, 0o644)
	putFile(t, filepath.Join(plugins, "cache", "agentfeedback", "agentfeedback", "5.0.0", "skills", "agentfeedback", "SKILL.md"), "plugin copy\n", 0o644)
	want := snapshot(t, plugins)

	r, out := installRun(t, "install", "claude-code")
	if r.code != 0 || harnessField(t, out, "claude-code", "skill") != "external" || harnessField(t, out, "claude-code", "hook") != "wired" ||
		!strings.Contains(notesOf(out), "the agentfeedback plugin provides the skill; install wires the rule and hooks only") {
		t.Fatalf("install with the plugin: %+v", r)
	}
	if _, err := os.Lstat(skills); !os.IsNotExist(err) {
		t.Fatalf("the earlier skill copy is left: %v", err)
	}
	if s := listStatus(t, "claude-code"); s["skill"] != "external" {
		t.Fatalf("list: %+v", s)
	}
	if r, _ := installRun(t, "uninstall", "claude-code"); r.code != 0 {
		t.Fatalf("uninstall: %+v", r)
	}
	sameTree(t, "plugin after uninstall", snapshot(t, plugins), want)

}

// init on a machine whose skill comes from the Claude Code plugin writes
// none and reports it external; uninstall leaves the plugin.
func TestInit_ClaudePluginSkill(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude")
	plugins := filepath.Join(e.home, ".claude", "plugins")
	putFile(t, filepath.Join(plugins, "installed_plugins.json"), `{"version":2,"plugins":{"agentfeedback@agentfeedback":[{"scope":"user","version":"5.0.0"}]}}`, 0o644)
	putFile(t, filepath.Join(plugins, "cache", "agentfeedback", "agentfeedback", "5.0.0", "skills", "agentfeedback", "SKILL.md"), "plugin copy\n", 0o644)
	want := snapshot(t, plugins)
	r, out := initRun(t, "", "--yes", "--harnesses", "claude-code")
	if h := harnessByName(out.Harnesses, "claude-code"); r.code != 0 || out.Status != "ok" || h == nil || h.Skill != "external" || h.Hook != "wired" {
		t.Fatalf("init: %+v", r)
	}
	if _, err := os.Lstat(filepath.Join(e.home, ".claude", "skills", "agentfeedback")); !os.IsNotExist(err) {
		t.Fatalf("init wrote the skill: %v", err)
	}
	if r, _ := installRun(t, "uninstall", "claude-code"); r.code != 0 {
		t.Fatalf("uninstall: %+v", r)
	}
	sameTree(t, "plugin after uninstall", snapshot(t, plugins), want)
}

// A Codex plugin enabled in config.toml provides the skill; a disabled one
// does not.
func TestInstall_CodexPluginSkill(t *testing.T) {
	for _, tt := range []struct {
		name, table, want string
	}{
		{"enabled", "enabled = true\n", "external"},
		{"no enabled key", "", "external"},
		{"disabled", "enabled = false\n", "wired"},
		{"enabled a string", "enabled = \"true\"\n", "wired"},
		{"enabled a number", "enabled = 1\n", "wired"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.seed(t)
			cfg := filepath.Join(e.home, ".codex", "config.toml")
			old, _ := os.ReadFile(cfg)
			putFile(t, cfg, string(old)+"\n[plugins.\"agentfeedback@agentfeedback\"]\n"+tt.table, 0o644)
			cache := filepath.Join(e.home, ".codex", "plugins", "cache", "agentfeedback", "agentfeedback", "5.0.0", "skills", "agentfeedback", "SKILL.md")
			putFile(t, cache, "plugin copy\n", 0o644)
			skill := filepath.Join(e.home, ".agents", "skills", "agentfeedback", "SKILL.md")

			r, out := installRun(t, "install", "codex")
			if r.code != 0 || harnessField(t, out, "codex", "skill") != tt.want || harnessField(t, out, "codex", "hook") != "wired" {
				t.Fatalf("install: %+v", r)
			}
			if _, err := os.Stat(skill); (err == nil) != (tt.want == "wired") {
				t.Fatalf("skill %s: %v", skill, err)
			}
			if data, err := os.ReadFile(cache); err != nil || string(data) != "plugin copy\n" {
				t.Fatalf("plugin copy: %q %v", data, err)
			}
			if r, _ := installRun(t, "uninstall", "codex"); r.code != 0 {
				t.Fatalf("uninstall: %+v", r)
			}
			if data, err := os.ReadFile(cache); err != nil || string(data) != "plugin copy\n" {
				t.Fatalf("plugin copy after uninstall: %q %v", data, err)
			}
		})
	}
}

// A plugin registry that does not parse, or lists no usable entry, lists
// no plugin: install proceeds and writes the skill.
func TestInstall_UnreadablePluginRegistry(t *testing.T) {
	for _, tt := range []struct{ name, claude, codex string }{
		{"unparsable", `{"plugins": {"agentfeedback@x": [`, "[plugins.\"agentfeedback@x\"\nenabled = \n"},
		{"no usable entry", `{"plugins": {"agentfeedback@x": [], "other@x": [{}]}}`, "[plugins]\n\"agentfeedback@x\" = \"not a table\"\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.seed(t)
			putFile(t, filepath.Join(e.home, ".claude", "plugins", "installed_plugins.json"), tt.claude, 0o644)
			cfg := filepath.Join(e.home, ".codex", "config.toml")
			old, _ := os.ReadFile(cfg)
			putFile(t, cfg, string(old)+"\n"+tt.codex, 0o644)
			r, out := installRun(t, "install", "claude-code", "codex")
			if r.code != 0 || harnessField(t, out, "claude-code", "skill") != "wired" || harnessField(t, out, "codex", "skill") != "wired" {
				t.Fatalf("install: %+v", r)
			}
			for _, f := range []string{filepath.Join(e.home, ".claude", "skills", "agentfeedback", "SKILL.md"), filepath.Join(e.home, ".agents", "skills", "agentfeedback", "SKILL.md")} {
				if _, err := os.Stat(f); err != nil {
					t.Error(err)
				}
			}
		})
	}
}

// AFDEV-85: a file install would write, found with exactly its bytes and
// no manifest record (an install interrupted before its manifest write),
// is recorded as install's own and removed by uninstall; one with other
// bytes beneath a recorded skill directory is still refused.
func TestInstall_AdoptsIdenticalFiles(t *testing.T) {
	t.Run("unrecorded skill directory", func(t *testing.T) {
		e := newInstallEnv(t)
		e.seed(t)
		skill, err := skillgen.Render(skillgen.FormSkillMD, "")
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(e.home, ".claude", "skills", "agentfeedback")
		path := filepath.Join(dir, "SKILL.md")
		putFile(t, path, string(skill), 0o644)
		r, out := installRun(t, "install", "claude-code")
		if r.code != 0 || harnessField(t, out, "claude-code", "skill") != "wired" {
			t.Fatalf("install: %+v", r)
		}
		m := readManifest(t, e)
		if !recordsFile(m, path) || !m.Files[path].Created || m.Files[path].Backup != "" {
			t.Fatalf("the adopted file is not recorded as install's: %+v", m.Files[path])
		}
		if _, err := os.Lstat(path + harness.BackupSuffix); !os.IsNotExist(err) {
			t.Fatalf("a backup was taken: %v", err)
		}
		if r, _ := installRun(t, "uninstall", "claude-code"); r.code != 0 {
			t.Fatalf("uninstall: %+v", r)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("the adopted skill directory is left: %v", err)
		}
	})

	for _, tt := range []struct {
		name, harness string
		args          []string
		file          func(e installEnv) string
	}{
		{"skill", "pi", nil, func(e installEnv) string {
			return filepath.Join(e.home, ".pi", "agent", "skills", "agentfeedback", "SKILL.md")
		}},
		{"docs", "pi", []string{"--docs"}, func(e installEnv) string {
			return filepath.Join(e.home, ".pi", "agent", "skills", "agentfeedback-docs", "SKILL.md")
		}},
		{"triage", "pi", nil, func(e installEnv) string {
			return filepath.Join(e.home, ".pi", "agent", "skills", harness.TriageDir, "playbooks", "weekly-digest.md")
		}},
		{"plugin file", "opencode", nil, func(e installEnv) string {
			return filepath.Join(e.home, ".config", "opencode", "plugins", "agentfeedback.js")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.seed(t)
			args := append([]string{"install", tt.harness}, tt.args...)
			if r, _ := installRun(t, args...); r.code != 0 {
				t.Fatalf("install: %+v", r)
			}
			path := tt.file(e)
			orig, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			forget(t, e, path)
			if r, out := installRun(t, args...); r.code != 0 || out["status"] != "installed" {
				t.Fatalf("install over the unrecorded file: %+v", r)
			}
			if !recordsFile(readManifest(t, e), path) {
				t.Fatalf("%s is not recorded", path)
			}
			if data, _ := os.ReadFile(path); string(data) != string(orig) {
				t.Fatal("the adopted file changed")
			}

			// Other bytes, unrecorded, are still refused.
			forget(t, e, path)
			putFile(t, path, "edited\n", 0o644)
			r, out := installRun(t, args...)
			// The skill directory holds that one file, so with it
			// unrecorded the directory itself is the one refused.
			msg := fmt.Sprint(out["message"])
			if r.code != 1 || !(strings.HasPrefix(msg, path+" already holds") || strings.HasPrefix(msg, filepath.Dir(path)+" already holds")) {
				t.Fatalf("a different file was not refused: %+v", r)
			}
			if data, _ := os.ReadFile(path); string(data) != "edited\n" {
				t.Fatal("the refused file changed")
			}
			putFile(t, path, string(orig), 0o644)
			if r, _ := installRun(t, args...); r.code != 0 {
				t.Fatalf("install: %+v", r)
			}
			if r, _ := installRun(t, "uninstall", tt.harness); r.code != 0 {
				t.Fatalf("uninstall: %+v", r)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("uninstall left the adopted file: %v", err)
			}
		})
	}
}

// A Claude Code plugin disabled in settings.json, or installed for a
// project only, does not provide the skill: install writes it.
func TestInstall_ClaudePluginNotCounted(t *testing.T) {
	for _, tt := range []struct{ name, scope, settings string }{
		{"disabled", "user", `{"enabledPlugins": {"agentfeedback@agentfeedback": false}}`},
		{"project scope", "project", `{"enabledPlugins": {"agentfeedback@agentfeedback": true}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.seed(t)
			putFile(t, filepath.Join(e.home, ".claude", "settings.json"), tt.settings, 0o644)
			putFile(t, filepath.Join(e.home, ".claude", "plugins", "installed_plugins.json"),
				`{"version":2,"plugins":{"agentfeedback@agentfeedback":[{"scope":"`+tt.scope+`","projectPath":"/work/repo","version":"5.0.0"}]}}`, 0o644)
			r, out := installRun(t, "install", "claude-code")
			if r.code != 0 || harnessField(t, out, "claude-code", "skill") != "wired" {
				t.Fatalf("install: %+v", r)
			}
			if _, err := os.Stat(filepath.Join(e.home, ".claude", "skills", "agentfeedback", "SKILL.md")); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Enabled in settings.json, or absent from it, the user-scope plugin counts.
	e := newInstallEnv(t)
	e.seed(t)
	putFile(t, filepath.Join(e.home, ".claude", "plugins", "installed_plugins.json"),
		`{"version":2,"plugins":{"agentfeedback@agentfeedback":[{"scope":"project"},{"scope":"user"}]}}`, 0o644)
	if r, out := installRun(t, "install", "claude-code"); r.code != 0 || harnessField(t, out, "claude-code", "skill") != "external" {
		t.Fatalf("install: %+v", r)
	}
}

// A skill directory adopted whole, into which the same run writes files in
// new subdirectories, leaves no directory behind after uninstall, whatever
// order its directories were recorded in.
func TestUninstall_AdoptedDirWithNestedNewDirs(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	files, err := skillgen.Triage()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.home, ".claude", "skills", harness.TriageDir)
	i := slices.IndexFunc(files, func(f skillgen.File) bool { return f.Path == "reference/clustering.md" })
	if i < 0 {
		t.Fatal("no reference/clustering.md in the triage skill")
	}
	putFile(t, filepath.Join(dir, "reference", "clustering.md"), string(files[i].Data), 0o644)
	if r, out := installRun(t, "install", "claude-code"); r.code != 0 || harnessField(t, out, "claude-code", "triage") != "wired" {
		t.Fatalf("install: %+v", r)
	}
	if r, _ := installRun(t, "uninstall", "claude-code"); r.code != 0 {
		t.Fatalf("uninstall: %+v", r)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("uninstall left %s: %v", dir, err)
	}
}
