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

	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

func testEnv(t *testing.T) Env {
	t.Helper()
	home := t.TempDir()

	return Env{
		Home:     home,
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Exec: func(context.Context, map[string]string, string, ...string) ([]byte, error) {
			return nil, errors.New("no commands in this test")
		},
	}
}

func put(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cliRequest(names ...string) Request {
	return Request{Harnesses: names, Options: Options{Mode: ModeCLI, Server: "https://feedback.example.test", Binary: "/usr/local/bin/agentfeedback"}}
}

func TestDuplicateKey_Parse(t *testing.T) {
	for _, doc := range []string{`{"a": 1, "a": 2}`, `{"x": {"b": 1, /* c */ "b": 2}}`, `{"x": [{"k": 1, "k": 1}]}`} {
		_, err := parse([]byte(doc))
		var dup *DuplicateKeyError
		if !errors.As(err, &dup) {
			t.Errorf("%s: %v", doc, err)
		}
	}
	if _, err := parse([]byte(`{"a": {"k": 1}, "b": {"k": 1}}`)); err != nil {
		t.Errorf("the same key in two objects: %v", err)
	}
}

func TestDuplicateKey_InstallRefuses(t *testing.T) {
	e := testEnv(t)
	settings := filepath.Join(e.Home, ".claude", "settings.json")
	doc := "{\n  \"model\": \"a\",\n  \"model\": \"b\"\n}\n"
	put(t, settings, doc)
	_, err := e.Run(cliRequest("claude-code"))
	var r *Refusal
	if !errors.As(err, &r) || err.Error() != settings+" has the key model twice in one object; fix the file, then run install again." {
		t.Fatalf("%v", err)
	}
	if got, _ := os.ReadFile(settings); string(got) != doc {
		t.Error("the file changed")
	}
	if _, err := os.Stat(e.ManifestPath()); err == nil {
		t.Error("a manifest was written")
	}
}

func TestDuplicateKey_UninstallLeavesFile(t *testing.T) {
	e := testEnv(t)
	settings := filepath.Join(e.Home, ".claude", "settings.json")
	put(t, settings, "{\n  \"model\": \"a\"\n}\n")
	req := cliRequest("claude-code")
	req.Options.Reminder = true
	if _, err := e.Run(req); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(settings)
	edited := strings.Replace(string(data), `"model": "a"`, `"model": "a", "model": "b"`, 1)
	put(t, settings, edited)
	res, err := e.Run(Request{Harnesses: []string{"claude-code"}, Uninstall: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(settings); string(got) != edited {
		t.Errorf("the file changed:\n%s", got)
	}
	if len(res.Backups) != 1 || res.Backups[0] != settings+BackupSuffix {
		t.Errorf("backups %v", res.Backups)
	}
	if _, err := os.Stat(settings + BackupSuffix); err != nil {
		t.Error("the backup was not kept")
	}
	// The hook and the reminder both meet the duplicate; the note is given
	// once.
	if notes := strings.Join(res.Harnesses[0].Notes, "\n"); strings.Count(notes, "the key model twice") != 1 {
		t.Errorf("notes %q", notes)
	}
}

func TestPluginSources_IgnoreSpawnErrors(t *testing.T) {
	for name, src := range map[string][]byte{
		"opencode": opencodePlugin("/bin/af", "/c/err.json"), "omp": ompExtension("/bin/af", "/c/err.json"), "pi": piExtension("/bin/af", "/c/err.json"),
	} {
		for _, want := range []string{
			`const BIN = "/bin/af";`, `const SPAWN_ERR = "/c/err.json";`, `const HARNESS = "` + name + `";`,
			`spawn(BIN, ["hook", HARNESS, event]`, `child.on("error", (err`, "spawnError(err);", "}, 5000);",
		} {
			if !strings.Contains(string(src), want) {
				t.Errorf("%s lacks %s:\n%s", name, want, src)
			}
		}
		if strings.Contains(string(src), "flush") {
			t.Errorf("%s still names flush:\n%s", name, src)
		}
	}
}

func TestChangedSince_NextFitsBothCommands(t *testing.T) {
	e := testEnv(t)
	path := filepath.Join(e.Home, "f.json")
	put(t, path, "{}\n")
	err := changedSince(&fileState{path: path, exists: true, orig: []byte("{\"a\": 1}\n")})
	var r *Refusal
	if !errors.As(err, &r) || r.Next != "run agentfeedback install or uninstall again" {
		t.Fatalf("%v", err)
	}
}

func TestApply_ChangeUnderTheRunDropsBackup(t *testing.T) {
	e := testEnv(t)
	settings := filepath.Join(e.Home, ".claude", "settings.json")
	put(t, settings, "{\n  \"model\": \"a\"\n}\n")
	beforeRename = func(path string) {
		if path == settings {
			put(t, settings, "{\n  \"model\": \"b\"\n}\n")
		}
	}
	t.Cleanup(func() { beforeRename = nil })
	res, err := e.Run(cliRequest("claude-code"))
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(settings + BackupSuffix); err == nil {
		t.Fatal("the backup of a file the run could not write was left")
	}
	for _, b := range res.Backups {
		if b == settings+BackupSuffix {
			t.Errorf("backups lists a deleted backup: %v", res.Backups)
		}
	}
	beforeRename = nil
	if _, err := e.Run(cliRequest("claude-code")); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestInstall_EmptyRecordedSkillDirIsOurs(t *testing.T) {
	e := testEnv(t)
	skillDir := filepath.Dir(e.SkillPath("pi"))
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// An interrupted run created the directory, recorded it, and stopped.
	dirs := []string{filepath.Join(e.Home, ".pi"), filepath.Join(e.Home, ".pi", "agent"), filepath.Join(e.Home, ".pi", "agent", "skills"), skillDir}
	m := newManifest()
	m.DirsCreated = dirs
	put(t, e.ManifestPath(), string(m.encode()))
	if _, err := e.Run(cliRequest("pi")); err != nil {
		t.Fatalf("the recorded empty skill directory was refused: %v", err)
	}
	// An empty one the manifest does not record is still foreign.
	e2 := testEnv(t)
	if err := os.MkdirAll(filepath.Dir(e2.SkillPath("pi")), 0o755); err != nil {
		t.Fatal(err)
	}
	var r *Refusal
	if _, err := e2.Run(cliRequest("pi")); !errors.As(err, &r) {
		t.Fatalf("unrecorded skill directory: %v", err)
	}
}

func TestWriteAtomic_SyncsDirectory(t *testing.T) {
	e := testEnv(t)
	path := filepath.Join(e.Home, "f.json")
	var synced []string
	orig := syncDir
	syncDir = func(dir string) error { synced = append(synced, dir); return nil }
	t.Cleanup(func() { syncDir = orig })
	if err := writeAtomic(&fileState{path: path, mode: 0o644, present: true, cur: []byte("{}\n")}); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != e.Home {
		t.Fatalf("synced %v", synced)
	}
}

// withDocs stands in for another binary's docs skill tree.
func withDocs(t *testing.T, files map[string]string) {
	t.Helper()
	orig := docsFiles
	docsFiles = func() ([]skillgen.File, error) {
		var out []skillgen.File
		for _, p := range slices.Sorted(maps.Keys(files)) {
			out = append(out, skillgen.File{Path: p, Data: []byte(files[p])})
		}

		return out, nil
	}
	t.Cleanup(func() { docsFiles = orig })
}

func docsRequest(names ...string) Request {
	req := cliRequest(names...)
	req.Options.Docs = true

	return req
}

func TestInstall_DocsUpgradeReplacesTheTree(t *testing.T) {
	e := testEnv(t)
	dir := e.DocsDir("codex")
	withDocs(t, map[string]string{"SKILL.md": "v1\n", "references/a.md": "# A\n", "references/old/b.md": "# B\n"})
	if _, err := e.Run(docsRequest("codex")); err != nil {
		t.Fatal(err)
	}
	// The next binary drops b, adds c and changes SKILL.md; a is the same.
	withDocs(t, map[string]string{"SKILL.md": "v2\n", "references/a.md": "# A\n", "references/new/c.md": "# C\n"})
	res, err := e.Run(docsRequest("codex"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "installed" {
		t.Fatalf("status %s", res.Status)
	}
	for path, want := range map[string]string{"SKILL.md": "v2\n", "references/a.md": "# A\n", "references/new/c.md": "# C\n"} {
		if got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path))); err != nil || string(got) != want {
			t.Errorf("%s: %q %v", path, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "references", "old")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dropped file's directory is left: %v", err)
	}
	if !slices.Contains(res.Changed, filepath.Join(dir, "references", "old", "b.md")) {
		t.Errorf("changed %v", res.Changed)
	}
	// A user edit of a docs file is refused on install like SKILL.md.
	put(t, filepath.Join(dir, "references", "a.md"), "mine\n")
	withDocs(t, map[string]string{"SKILL.md": "v2\n", "references/a.md": "# A2\n", "references/new/c.md": "# C\n"})
	var r *Refusal
	if _, err := e.Run(docsRequest("codex")); !errors.As(err, &r) || !strings.Contains(r.Problem, "a.md was modified since install") {
		t.Fatalf("%v", err)
	}
}

func TestInstall_DocsRemovedWithItsDirectories(t *testing.T) {
	e := testEnv(t)
	withDocs(t, map[string]string{"SKILL.md": "v1\n", "references/docs/a.md": "# A\n", "references/schemas/kinds/f.json": "{}\n"})
	if _, err := e.Run(docsRequest("pi")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(cliRequest("pi")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.DocsDir("pi")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("docs skill left after a run without docs: %v", err)
	}
	if _, err := os.Stat(e.SkillPath("pi")); err != nil {
		t.Fatalf("skill removed: %v", err)
	}
}

func TestInstall_DocsSkillRootChecks(t *testing.T) {
	withDocs(t, map[string]string{"SKILL.md": "v1\n", "references/a.md": "# A\n"})
	e := testEnv(t)
	put(t, filepath.Join(e.DocsDir("pi"), "notes.md"), "mine\n")
	var r *Refusal
	if _, err := e.Run(docsRequest("pi")); !errors.As(err, &r) || !strings.Contains(r.Problem, "an agentfeedback-docs skill directory") {
		t.Fatalf("foreign docs directory: %v", err)
	}
	if _, err := os.Stat(e.ManifestPath()); err == nil {
		t.Error("a manifest was written")
	}

	e2 := testEnv(t)
	src := filepath.Join(e2.Home, "elsewhere")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e2.DocsDir("pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := newManifest()
	// A recorded docs file, so only the link below it is in the way.
	m.Harnesses["pi"] = &HarnessRecord{Mode: ModeCLI, Docs: true, Items: []Item{{Kind: KindSkillFile, Role: RoleDocs, File: filepath.Join(e2.DocsDir("pi"), "SKILL.md"), SHA256: sha([]byte("v0\n"))}}}
	put(t, filepath.Join(e2.DocsDir("pi"), "SKILL.md"), "v0\n")
	put(t, e2.ManifestPath(), string(m.encode()))
	if err := os.Symlink(src, filepath.Join(e2.DocsDir("pi"), "references")); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Run(docsRequest("pi")); !errors.As(err, &r) || !strings.Contains(r.Problem, "references is a symbolic link") {
		t.Fatalf("linked references directory: %v", err)
	}
}

func TestValidateManifest_DocsPaths(t *testing.T) {
	e := testEnv(t)
	dir := e.DocsDir("claude-code")
	for _, tt := range []struct {
		path string
		ok   bool
	}{
		{filepath.Join(dir, "references", "docs", "gone.md"), true},
		{dir + "/references/../../agentfeedback/x.md", false},
		{dir + "/../../../.bashrc", false},
		{"/etc/passwd", false},
		{dir, false},
		{dir + "-other/x.md", false},
	} {
		m := newManifest()
		m.Harnesses["claude-code"] = &HarnessRecord{Mode: ModeCLI, Docs: true, Items: []Item{{Kind: KindSkillFile, Role: RoleDocs, File: tt.path}}}
		m.Files[tt.path] = &FileRecord{Created: true}
		m.DirsCreated = []string{dir, filepath.Join(dir, "references")}
		err := e.validateManifest(m, e.ManifestPath())
		if (err == nil) != tt.ok {
			t.Errorf("%s: %v", tt.path, err)
		}
	}
	m := newManifest()
	member := filepath.Join(dir, "references", "settings.json")
	m.Harnesses["claude-code"] = &HarnessRecord{Mode: ModeCLI, Items: []Item{{Kind: KindJSONMember, File: member, Key: "agentfeedback"}}}
	if err := e.validateManifest(m, e.ManifestPath()); err == nil {
		t.Error("a JSON member beneath the docs directory was accepted")
	}
	m = newManifest()
	m.DirsCreated = []string{filepath.Join(dir, "references", "..", "..", "..", "x")}
	if err := e.validateManifest(m, e.ManifestPath()); err == nil {
		t.Error("an unclean directory was accepted")
	}
	m = newManifest()
	f := filepath.Join(dir, "a.md")
	m.Files[f] = &FileRecord{Backup: filepath.Join(dir, "b.md")}
	if err := e.validateManifest(m, e.ManifestPath()); err == nil {
		t.Error("a backup that is another docs file was accepted")
	}
	m.Files[f].Backup = f + BackupSuffix
	if err := e.validateManifest(m, e.ManifestPath()); err != nil {
		t.Errorf("a docs file's own backup: %v", err)
	}
}

func TestInstall_DocsLinkedReferencesLeftAlone(t *testing.T) {
	e := testEnv(t)
	withDocs(t, map[string]string{"SKILL.md": "v1\n", "references/docs/a.md": "# A\n"})
	if _, err := e.Run(docsRequest("pi")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.DocsDir("pi"), "references", "docs")
	moved := filepath.Join(e.Home, "moved")
	if err := os.Rename(link, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, link); err != nil {
		t.Fatal(err)
	}
	refused := func(req Request) {
		t.Helper()
		var r *Refusal
		if _, err := e.Run(req); !errors.As(err, &r) || !strings.Contains(r.Problem, link+" is a symbolic link") || !strings.Contains(r.Next, "remove it") {
			t.Fatalf("%v", err)
		}
		if _, err := os.Stat(filepath.Join(moved, "a.md")); err != nil {
			t.Fatalf("the linked file was removed: %v", err)
		}
	}
	// Without docs the file would be removed; with docs it is unchanged
	// but not in place.
	refused(cliRequest("pi"))
	refused(docsRequest("pi"))
	res, err := e.Run(Request{Harnesses: []string{"pi"}, Uninstall: true})
	if err != nil {
		t.Fatal(err)
	}
	note := link + " is a symbolic link; " + filepath.Join(link, "a.md") + " is left in place"
	if len(res.Harnesses) != 1 || !slices.Contains(res.Harnesses[0].Notes, note) {
		t.Errorf("notes %+v", res.Harnesses)
	}
	if _, err := os.Stat(filepath.Join(moved, "a.md")); err != nil {
		t.Errorf("the linked file was removed: %v", err)
	}
}

func TestLoadManifest_LegacyRecordKeepsItsBinary(t *testing.T) {
	e := testEnv(t)
	old := cliRequest("pi")
	old.Options.Binary = "/old/af"
	if _, err := e.Run(old); err != nil {
		t.Fatal(err)
	}
	// Rewrite the record as one written before records kept their binary.
	m, _, err := LoadManifest(e.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	m.Harnesses["pi"].Binary = ""
	put(t, e.ManifestPath(), string(m.encode()))
	req := cliRequest("claude-code")
	req.Options.Binary = "/new/af"
	if _, err := e.Run(req); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range st {
		got[s.Name] = s.Binary
	}
	if got["pi"] != "/old/af" || got["claude-code"] != "/new/af" {
		t.Fatalf("binaries: %v", got)
	}
}

func TestApply_RetargetedLinkUnderTheRun(t *testing.T) {
	e := testEnv(t)
	settings := filepath.Join(e.Home, ".claude", "settings.json")
	real := filepath.Join(e.Home, "a.json")
	other := filepath.Join(e.Home, "b.json")
	put(t, real, "{\n  \"model\": \"a\"\n}\n")
	put(t, other, "{\n  \"model\": \"b\"\n}\n")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, settings); err != nil {
		t.Fatal(err)
	}
	beforeRename = func(path string) {
		if path == settings {
			_ = os.Remove(settings)
			_ = os.Symlink(other, settings)
		}
	}
	t.Cleanup(func() { beforeRename = nil })
	res, err := e.Run(cliRequest("claude-code"))
	var r *Refusal
	if !errors.As(err, &r) || r.Problem != settings+" changed while installing" {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(settings + BackupSuffix); err == nil {
		t.Fatal("the backup of a file the run could not write was left")
	}
	if slices.Contains(res.Backups, settings+BackupSuffix) {
		t.Errorf("backups lists a deleted backup: %v", res.Backups)
	}
	if got, _ := os.ReadFile(real); string(got) != "{\n  \"model\": \"a\"\n}\n" {
		t.Errorf("the old target changed:\n%s", got)
	}
}

func TestApply_FileReplacedByLinkUnderTheRun(t *testing.T) {
	e := testEnv(t)
	settings := filepath.Join(e.Home, ".claude", "settings.json")
	doc := "{\n  \"model\": \"a\"\n}\n"
	put(t, settings, doc)
	copyOf := filepath.Join(e.Home, "copy.json")
	put(t, copyOf, doc)
	beforeRename = func(path string) {
		if path == settings {
			_ = os.Remove(settings)
			_ = os.Symlink(copyOf, settings)
		}
	}
	t.Cleanup(func() { beforeRename = nil })
	var r *Refusal
	if _, err := e.Run(cliRequest("claude-code")); !errors.As(err, &r) || r.Problem != settings+" changed while installing" {
		t.Fatalf("%v", err)
	}
	if got, _ := os.ReadFile(copyOf); string(got) != doc {
		t.Errorf("the link's target changed:\n%s", got)
	}
}

// TestLoadManifest_LegacyURLEntry: a manifest written before stdio entries
// (a URL entry, no binary on the record) still reads as a URL entry.
func TestLoadManifest_LegacyURLEntry(t *testing.T) {
	e := testEnv(t)
	put(t, e.ManifestPath(), `{
  "version": 1,
  "server": "https://feedback.example.test",
  "binary": "/usr/local/bin/agentfeedback",
  "harnesses": {
    "omp": {
      "mode": "mcp",
      "reminder": false,
      "docs": false,
      "items": [
        {
          "kind": "json_member",
          "role": "mcp",
          "file": "`+filepath.Join(e.Home, ".omp", "agent", "mcp.json")+`",
          "path": ["mcpServers"],
          "key": "agentfeedback",
          "value": {"type":"http","url":"https://feedback.example.test/mcp","headers":{"Authorization":"Bearer ${AGENT_FEEDBACK_API_KEY}"}},
          "created_from": 0
        }
      ]
    }
  },
  "files": {},
  "dirs_created": []
}
`)
	put(t, filepath.Join(e.Home, ".omp", "agent", "mcp.json"), `{"mcpServers": {"agentfeedback": {"type":"http","url":"https://feedback.example.test/mcp","headers":{"Authorization":"Bearer ${AGENT_FEEDBACK_API_KEY}"}}}}`)
	st, err := e.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if s.Name == "omp" && (s.MCP != "url" || s.Binary != "") {
			t.Fatalf("omp %+v", s)
		}
	}
}

// TestStatus_TransportFromItem: the MCP column and the binary follow the
// recorded MCP item, not the record's Binary, which lags behind the items
// while a switch between a URL and a stdio entry is partly applied.
func TestStatus_TransportFromItem(t *testing.T) {
	const bin = "/opt/af/agentfeedback"
	stdioValue := plainStdio(bin)
	urlValue := `{"type":"http","url":"https://feedback.example.test/mcp"}`
	stdioText := codexStdioBlock(bin)
	urlText := codexBlock("https://feedback.example.test")
	for _, tc := range []struct {
		name, recBinary, value, text, wantMCP, wantBinary string
	}{
		{"stdio item, empty binary", "", stdioValue, stdioText, "stdio", bin},
		{"url item, stale binary", "/old/agentfeedback", urlValue, urlText, "url", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := testEnv(t)
			mcpJSON := filepath.Join(e.Home, ".omp", "agent", "mcp.json")
			config := filepath.Join(e.Home, ".codex", "config.toml")
			put(t, mcpJSON, `{"mcpServers": {"agentfeedback": `+tc.value+`}}`)
			put(t, config, tc.text)
			man := newManifest()
			man.Harnesses["omp"] = &HarnessRecord{Mode: ModeMCP, Binary: tc.recBinary, Items: []Item{
				member(mcpJSON, []string{"mcpServers"}, []byte(tc.value), RoleMCP),
			}}
			man.Harnesses["codex"] = &HarnessRecord{Mode: ModeMCP, Binary: tc.recBinary, Items: []Item{
				{Kind: KindTOMLBlock, Role: RoleMCP, File: config, Text: tc.text},
			}}
			p := newPlan(e, man)
			for _, name := range []string{"omp", "codex"} {
				if s := p.status(name); s.MCP != tc.wantMCP || s.Binary != tc.wantBinary {
					t.Errorf("%s %+v", name, s)
				}
			}
		})
	}
}

// TestInstall_ReplacesOlderFlushHook: a manifest recording the older
// flush --hook entries is reported by LegacyHooks, and a reinstall removes
// those entries and adds the agentfeedback hook ones.
func TestInstall_ReplacesOlderFlushHook(t *testing.T) {
	e := testEnv(t)
	settings := filepath.Join(e.Home, ".claude", "settings.json")
	put(t, settings, "{\n  \"model\": \"a\"\n}\n")
	claude, opencode := adapterOf("claude-code"), adapterOf("opencode")
	origClaude, origOpencode := claude.hook, opencode.hook
	t.Cleanup(func() { claude.hook, opencode.hook = origClaude, origOpencode })
	claude.hook = func(p *plan, o Options) ([]Item, []string) {
		return []Item{element(settings, []string{"hooks", "Stop"}, commandHook(o.Binary+" flush --hook"), RoleHook)}, nil
	}
	opencode.hook = func(p *plan, o Options) ([]Item, []string) {
		old := []byte("const BIN = \"" + o.Binary + "\";\nspawn(BIN, [\"flush\", \"--hook\"], { stdio: \"ignore\" });\n")

		return []Item{fileItem(KindPluginFile, RoleHook, filepath.Join(p.env.opencodeDir(), "plugins", "agentfeedback.js"), old)}, nil
	}
	if _, err := e.Run(cliRequest("claude-code", "opencode")); err != nil {
		t.Fatal(err)
	}
	if got, err := e.LegacyHooks(); err != nil || !slices.Equal(got, []string{"claude-code", "opencode"}) {
		t.Fatalf("legacy %v %v", got, err)
	}
	claude.hook, opencode.hook = origClaude, origOpencode
	if _, err := e.Run(cliRequest("claude-code", "opencode")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(settings)
	if strings.Contains(string(data), "flush --hook") || !strings.Contains(string(data), "agentfeedback hook claude-code Stop") ||
		!strings.Contains(string(data), "agentfeedback hook claude-code PostToolUseFailure") || !strings.Contains(string(data), `"model": "a"`) {
		t.Fatalf("settings.json:\n%s", data)
	}
	plugin, _ := os.ReadFile(filepath.Join(e.opencodeDir(), "plugins", "agentfeedback.js"))
	if strings.Contains(string(plugin), "flush") || !strings.Contains(string(plugin), `run("session.idle"`) {
		t.Fatalf("plugin:\n%s", plugin)
	}
	if got, err := e.LegacyHooks(); err != nil || len(got) != 0 {
		t.Fatalf("legacy after reinstall %v %v", got, err)
	}
	if _, err := e.Run(Request{Harnesses: []string{"claude-code", "opencode"}, Uninstall: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(settings); string(got) != "{\n  \"model\": \"a\"\n}\n" {
		t.Errorf("after uninstall:\n%s", got)
	}
}

// TestInstall_HookEntries: install writes the agentfeedback hook entries,
// with the spawn-error path under the cache directory in the plugins.
func TestInstall_HookEntries(t *testing.T) {
	e := testEnv(t)
	cache := filepath.Join(e.Home, "xdg-cache")
	e.Getenv = func(k string) string {
		if k == "XDG_CACHE_HOME" {
			return cache
		}

		return ""
	}
	names := []string{"claude-code", "codex", "cursor", "opencode", "omp", "pi", "copilot", "antigravity", "devin"}
	if _, err := e.Run(cliRequest(names...)); err != nil {
		t.Fatal(err)
	}
	bin := "/usr/local/bin/agentfeedback"
	for _, tt := range []struct {
		file string
		want []string
	}{
		{".claude/settings.json", []string{bin + " hook claude-code PostToolUseFailure", bin + " hook claude-code Stop"}},
		{".codex/hooks.json", []string{bin + " hook codex Stop"}},
		{".cursor/hooks.json", []string{bin + " hook cursor postToolUseFailure", bin + " hook cursor stop", `"timeout": 5`}},
		{".config/opencode/plugins/agentfeedback.js", []string{`await run("tool.execute.after"`, `run("session.idle"`, filepath.Join(cache, "agentfeedback", "hooks", "opencode.spawn-error.json")}},
		{".omp/agent/extensions/agentfeedback.ts", []string{`run("tool_result"`, `run("agent_end"`, "additionalContext: text", filepath.Join(cache, "agentfeedback", "hooks", "omp.spawn-error.json")}},
		{".pi/agent/extensions/agentfeedback.ts", []string{`run("tool_result"`, `run("agent_settled"`, `deliverAs: "nextTurn"`, filepath.Join(cache, "agentfeedback", "hooks", "pi.spawn-error.json")}},
		{".copilot/hooks/agentfeedback.json", []string{bin + " hook copilot postToolUseFailure", bin + " hook copilot agentStop", `"timeoutSec": 5`}},
		{".gemini/config/hooks.json", []string{bin + " hook antigravity PostToolUse", bin + " hook antigravity PreInvocation", bin + " hook antigravity Stop", `"matcher": "*"`}},
		{".config/devin/config.json", []string{bin + " hook devin Stop"}},
	} {
		data, err := os.ReadFile(filepath.Join(e.Home, filepath.FromSlash(tt.file)))
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range tt.want {
			if !strings.Contains(string(data), w) {
				t.Errorf("%s lacks %s:\n%s", tt.file, w, data)
			}
		}
		if strings.Contains(string(data), "flush") {
			t.Errorf("%s names flush:\n%s", tt.file, data)
		}
	}
}

// TestInstall_QuotedBinary: a binary path with a character a shell treats
// specially is single-quoted in every command-string hook entry.
func TestInstall_QuotedBinary(t *testing.T) {
	e := testEnv(t)
	req := cliRequest("claude-code", "codex", "cursor", "copilot", "antigravity", "devin")
	req.Options.Binary = "/opt/my tools/it's/agentfeedback"
	req.Options.Reminder = true
	if _, err := e.Run(req); err != nil {
		t.Fatal(err)
	}
	q := `'/opt/my tools/it'\''s/agentfeedback'`
	for _, tt := range []struct {
		file string
		want []string
	}{
		{".claude/settings.json", []string{q + " hook claude-code PostToolUseFailure", q + " hook claude-code Stop", q + " skill reminder"}},
		{".codex/hooks.json", []string{q + " hook codex Stop", q + " skill reminder"}},
		{".cursor/hooks.json", []string{q + " hook cursor postToolUseFailure", q + " hook cursor stop", q + " skill reminder"}},
		{".copilot/hooks/agentfeedback.json", []string{q + " hook copilot postToolUseFailure", q + " hook copilot agentStop"}},
		{".gemini/config/hooks.json", []string{q + " hook antigravity PostToolUse", q + " hook antigravity PreInvocation", q + " hook antigravity Stop"}},
		{".config/devin/config.json", []string{q + " hook devin Stop"}},
	} {
		data, err := os.ReadFile(filepath.Join(e.Home, filepath.FromSlash(tt.file)))
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: %v", tt.file, err)
		}
		flat, _ := json.Marshal(doc)
		for _, w := range tt.want {
			ws, _ := json.Marshal(w)
			if !strings.Contains(string(flat), strings.Trim(string(ws), `"`)) {
				t.Errorf("%s lacks %s:\n%s", tt.file, w, data)
			}
		}
	}
	if got := shellBinary("/usr/local/bin/agent+feedback-1.0_x"); got != "/usr/local/bin/agent+feedback-1.0_x" {
		t.Errorf("plain path quoted: %s", got)
	}
}
