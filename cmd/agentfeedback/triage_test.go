package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

// triageTree is the triage skill as install writes it, relative to its
// directory, with the directories it holds.
func triageTree(t *testing.T) map[string]string {
	t.Helper()
	files, err := skillgen.Triage()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(files, func(f skillgen.File) bool { return f.Path == "SKILL.md" }) ||
		!slices.ContainsFunc(files, func(f skillgen.File) bool { return f.Path == "scripts/cluster.py" }) {
		t.Fatalf("the embedded triage skill lacks SKILL.md or scripts/cluster.py: %d files", len(files))
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

// contentTree is the snapshot of dir without the root entry and the modes.
func contentTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := snapshot(t, dir)
	delete(got, "./")
	maps.DeleteFunc(got, func(k, _ string) bool { return strings.HasSuffix(k, " (mode)") })

	return got
}

func TestInit_InstallsTriage(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude")
	dir := filepath.Join(e.home, ".claude", "skills", harness.TriageDir)

	r, out := initRun(t, "", "--yes")
	if r.code != 0 || out.Status != "ok" {
		t.Fatalf("init: %+v", r)
	}
	if h := harnessByName(out.Harnesses, "claude-code"); h == nil || h.Triage != "wired" {
		t.Fatalf("claude-code: %+v", h)
	}
	sameTree(t, "triage skill", contentTree(t, dir), triageTree(t))
	if m := readManifest(t, e); !m.Harnesses["claude-code"].Triage {
		t.Error("the manifest does not record triage")
	}
	if next := nextCommands(out.Next); !slices.Contains(next, "/agentfeedback-triage") {
		t.Errorf("next %q", next)
	}
	if r := runCLI(t, "", "init", "--yes"); r.code != 0 || !strings.Contains(r.stdout, "triage wired") {
		t.Fatalf("summary: %+v", r)
	}
}

func TestInit_NoTriage(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude")
	dir := filepath.Join(e.home, ".claude", "skills", harness.TriageDir)

	r, out := initRun(t, "", "--yes", "--no-triage")
	if r.code != 0 || out.Status != "ok" {
		t.Fatalf("init: %+v", r)
	}
	if h := harnessByName(out.Harnesses, "claude-code"); h == nil || h.Triage != "-" || h.Skill != "wired" {
		t.Fatalf("claude-code: %+v", h)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("triage skill installed: %v", err)
	}
	if next := nextCommands(out.Next); slices.ContainsFunc(next, func(c string) bool { return strings.Contains(c, "triage") }) {
		t.Errorf("next %q", next)
	}
}

func TestInit_AsksForTriage(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude")
	isTerminal = func() bool { return true }
	dir := filepath.Join(e.home, ".claude", "skills", harness.TriageDir)

	// Enter for local, Enter for all detected, n for the triage skill.
	r, out := initRun(t, "\n\nn\n")
	if r.code != 0 || out.Status != "ok" || !strings.Contains(r.stderr, "Install the agentfeedback-triage skill (process the reports with /agentfeedback-triage)? [Y/n]") {
		t.Fatalf("init: %+v", r)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("triage skill installed after no: %v", err)
	}
}

func TestInstall_TriageRoundTrip(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	before := snapshot(t, e.home)
	dir := filepath.Join(e.home, ".claude", "skills", harness.TriageDir)

	r, out := installRun(t, "install", "claude-code")
	if r.code != 0 || out["status"] != "installed" || harnessField(t, out, "claude-code", "triage") != "wired" {
		t.Fatalf("install: %+v", r)
	}
	sameTree(t, "triage skill", contentTree(t, dir), triageTree(t))
	r = runCLI(t, "", "install", "--list", "--json")
	var list map[string]any
	if err := json.Unmarshal([]byte(lastLine(r.stdout)), &list); err != nil || harnessField(t, list, "claude-code", "triage") != "wired" {
		t.Fatalf("list --json: %+v", r)
	}

	// --with-triage=false removes it, directories and all.
	r, out = installRun(t, "install", "claude-code", "--with-triage=false")
	if r.code != 0 || out["status"] != "installed" || harnessField(t, out, "claude-code", "triage") != "-" {
		t.Fatalf("install --with-triage=false: %+v", r)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("triage skill left: %v", err)
	}

	if r, _ := installRun(t, "install", "claude-code"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if r, out := installRun(t, "uninstall", "claude-code"); r.code != 0 || out["status"] != "uninstalled" {
		t.Fatalf("uninstall: %+v", r)
	}
	sameTree(t, "after uninstall", snapshot(t, e.home), before)
}

func TestInstall_TriageExternal(t *testing.T) {
	for _, link := range []bool{false, true} {
		t.Run(fmt.Sprintf("link=%v", link), func(t *testing.T) {
			e := newInstallEnv(t)
			e.seed(t)
			dir := filepath.Join(e.home, ".claude", "skills", harness.TriageDir)
			if link {
				target := filepath.Join(e.home, "checkout", "agentfeedback-triage")
				putFile(t, filepath.Join(target, "SKILL.md"), "a linked copy\n", 0o644)
				if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, dir); err != nil {
					t.Fatal(err)
				}
			} else {
				putFile(t, filepath.Join(dir, "SKILL.md"), "a copy from a skills CLI\n", 0o644)
			}
			before := snapshot(t, e.home)
			external := func() map[string]string {
				out := map[string]string{}
				for k, v := range snapshot(t, e.home) {
					if strings.HasPrefix(k, filepath.Join(".claude", "skills", harness.TriageDir)) || strings.HasPrefix(k, "checkout") {
						out[k] = v
					}
				}

				return out
			}
			want := external()

			r, out := installRun(t, "install", "claude-code")
			if r.code != 0 || out["status"] != "installed" || harnessField(t, out, "claude-code", "triage") != "external" || harnessField(t, out, "claude-code", "skill") != "wired" {
				t.Fatalf("install: %+v", r)
			}
			note := dir + " already exists and agentfeedback install did not write it; left as is (remove it and run agentfeedback install again to get this binary's agentfeedback-triage skill)"
			if !strings.Contains(notesOf(out), note) {
				t.Errorf("notes %q", notesOf(out))
			}
			sameTree(t, "external triage after install", external(), want)
			r = runCLI(t, "", "install", "--list", "--json")
			var list map[string]any
			if err := json.Unmarshal([]byte(lastLine(r.stdout)), &list); err != nil || harnessField(t, list, "claude-code", "triage") != "external" {
				t.Fatalf("list --json: %+v", r)
			}

			if r, out := installRun(t, "uninstall", "claude-code"); r.code != 0 || out["status"] != "uninstalled" {
				t.Fatalf("uninstall: %+v", r)
			}
			sameTree(t, "after uninstall", snapshot(t, e.home), before)
		})
	}
}

// A manifest written by another version names triage files this binary does
// not have; it is still valid. A non-triage item beneath a triage directory
// is refused.
func TestInstall_ManifestValidationTriage(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	if r, _ := installRun(t, "install", "pi"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	mpath := filepath.Join(e.home, ".config", "agentfeedback", "install.json")
	dir := filepath.Join(e.home, ".pi", "agent", "skills", harness.TriageDir)
	data, _ := os.ReadFile(mpath)
	gone := filepath.Join(dir, "playbooks", "gone.md")
	renamed := strings.ReplaceAll(string(data), filepath.Join(dir, "playbooks", "weekly-digest.md"), gone)
	if renamed == string(data) {
		t.Fatal("the manifest does not name weekly-digest.md")
	}
	putFile(t, mpath, renamed, 0o600)
	if err := os.Rename(filepath.Join(dir, "playbooks", "weekly-digest.md"), gone); err != nil {
		t.Fatal(err)
	}
	if r, _ := installRun(t, "install", "pi"); r.code != 0 {
		t.Fatalf("a recorded triage file of another version was refused: %+v", r)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("the old triage file is left: %v", err)
	}

	var m map[string]any
	data, _ = os.ReadFile(mpath)
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	items := m["harnesses"].(map[string]any)["pi"].(map[string]any)["items"].([]any)
	changed := false
	for _, it := range items {
		if it := it.(map[string]any); it["role"] == harness.RoleTriage {
			it["role"] = harness.RoleDocs
			changed = true

			break
		}
	}
	if !changed {
		t.Fatal("the manifest records no triage item")
	}
	bad, _ := json.Marshal(m)
	putFile(t, mpath, string(bad), 0o600)
	before := snapshot(t, e.home)
	r, out := installRun(t, "uninstall", "pi")
	if r.code != 1 || !strings.Contains(fmt.Sprint(out["message"]), ", which agentfeedback install does not write") {
		t.Fatalf("%+v", r)
	}
	sameTree(t, "after a refused manifest", snapshot(t, e.home), before)
}

// A manifest as the previous release wrote it, with no triage member: install
// adds the triage skill, and uninstall still restores the home exactly.
func TestInstall_TriageOverPreviousManifest(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	before := snapshot(t, e.home)
	if r, _ := installRun(t, "install", "claude-code", "--with-triage=false"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	mpath := filepath.Join(e.home, ".config", "agentfeedback", "install.json")
	data, _ := os.ReadFile(mpath)
	old := regexp.MustCompile(`\s*"triage":\s*false,`).ReplaceAllString(string(data), "")
	if old == string(data) || strings.Contains(old, `"triage"`) {
		t.Fatalf("the manifest's triage member was not removed: %s", data)
	}
	putFile(t, mpath, old, 0o600)

	r, out := installRun(t, "install", "claude-code")
	if r.code != 0 || out["status"] != "installed" || harnessField(t, out, "claude-code", "triage") != "wired" {
		t.Fatalf("install: %+v", r)
	}
	sameTree(t, "triage skill", contentTree(t, filepath.Join(e.home, ".claude", "skills", harness.TriageDir)), triageTree(t))
	if r, out := installRun(t, "uninstall", "claude-code"); r.code != 0 || out["status"] != "uninstalled" {
		t.Fatalf("uninstall: %+v", r)
	}
	sameTree(t, "after uninstall", snapshot(t, e.home), before)
}

func TestInstall_TriageDryRun(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	for _, step := range [][]string{
		{"install", "claude-code", "--dry-run"},
		{"install", "claude-code", "--with-triage=false"},
		{"install", "claude-code", "--dry-run"},
	} {
		before := snapshot(t, e.home)
		r, out := installRun(t, step...)
		if r.code != 0 {
			t.Fatalf("%v: %+v", step, r)
		}
		if slices.Contains(step, "--dry-run") {
			if out["status"] != "dry_run" || harnessField(t, out, "claude-code", "triage") != "wired" {
				t.Fatalf("%v: %+v", step, r)
			}
			sameTree(t, "after a dry run", snapshot(t, e.home), before)
		}
	}
}
