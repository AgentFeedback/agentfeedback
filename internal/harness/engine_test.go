package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"opencode": opencodePlugin("/bin/af"), "omp": ompExtension("/bin/af"), "pi": piExtension("/bin/af"),
	} {
		if !strings.Contains(string(src), `child.on("error", () => {});`) || !strings.Contains(string(src), "child.unref();") {
			t.Errorf("%s:\n%s", name, src)
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
