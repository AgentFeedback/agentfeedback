package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
)

// runMainEnv, set to 1, makes the test binary run as the agentfeedback
// binary: the tests that need a real process (stdin, stdout, exit status)
// execute it with the command line as its arguments.
const runMainEnv = "AGENTFEEDBACK_TEST_RUN_MAIN"

// TestMain keeps the package's tests off the host: no test finds an
// agentfeedback on PATH unless it stubs pathBinary itself.
func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
	}
	pathBinary = func() (string, bool) { return "", false }
	os.Exit(m.Run())
}

// stubBinaries makes binaryVersion answer from versions ("unknown" for any
// other path) and pathBinary return path, found.
func stubBinaries(t *testing.T, versions map[string]string, path string, found bool) {
	t.Helper()
	origVersion, origPath := binaryVersion, pathBinary
	binaryVersion = func(p string) string {
		if v, ok := versions[p]; ok {
			return v
		}

		return "unknown"
	}
	pathBinary = func() (string, bool) { return path, found }
	t.Cleanup(func() { binaryVersion, pathBinary = origVersion, origPath })
}

// listJSON runs install --list --json and returns the harness statuses.
func listJSON(t *testing.T) map[string]harness.HarnessStatus {
	t.Helper()
	r := runCLI(t, "", "install", "--list", "--json")
	var out struct {
		Harnesses []harness.HarnessStatus `json:"harnesses"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil {
		t.Fatalf("%+v", r)
	}
	m := map[string]harness.HarnessStatus{}
	for _, h := range out.Harnesses {
		m[h.Name] = h
	}

	return m
}

// TestInstall_RecordsBinaryPerHarness: install records the wired binary per
// CLI-mode harness, and the list shows it with its version.
func TestInstall_RecordsBinaryPerHarness(t *testing.T) {
	e := newInstallEnv(t)
	stubBinaries(t, map[string]string{e.exe: "4.1.0"}, e.exe, true)
	if r, out := installRun(t, "install", "claude-code", "--server", "local"); r.code != 0 || out["warnings"] != nil {
		t.Fatalf("%+v", r)
	}
	if got := readManifest(t, e).Harnesses["claude-code"].Binary; got != e.exe {
		t.Fatalf("recorded binary %q, want %q", got, e.exe)
	}
	h := listJSON(t)
	if h["claude-code"].Binary != e.exe || h["claude-code"].BinaryVersion != "4.1.0" {
		t.Fatalf("claude-code %+v", h["claude-code"])
	}
	if h["codex"].Binary != "" || h["codex"].BinaryVersion != "" {
		t.Fatalf("unwired codex %+v", h["codex"])
	}
	table := runCLI(t, "", "install", "--list")
	if table.code != 0 || !strings.Contains(table.stdout, "BINARY") || !strings.Contains(table.stdout, "VERSION") ||
		!strings.Contains(table.stdout, e.exe) || !strings.Contains(table.stdout, "4.1.0") {
		t.Fatalf("table %+v", table)
	}
}

// TestInstall_BinaryFallsBackToManifest: a record without its own binary
// shows the manifest's.
func TestInstall_BinaryFallsBackToManifest(t *testing.T) {
	e := newInstallEnv(t)
	stubBinaries(t, map[string]string{e.exe: "4.0.0"}, e.exe, true)
	if r, _ := installRun(t, "install", "claude-code", "--server", "local"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	m := readManifest(t, e)
	m.Harnesses["claude-code"].Binary = ""
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(e.home, ".config", "agentfeedback", "install.json"), string(data), 0o600)
	if h := listJSON(t)["claude-code"]; h.Binary != e.exe || h.BinaryVersion != "4.0.0" {
		t.Fatalf("claude-code %+v", h)
	}
}

// TestInstall_PathWarnings: install warns when PATH has no agentfeedback or
// another one than the binary being wired.
func TestInstall_PathWarnings(t *testing.T) {
	t.Run("different", func(t *testing.T) {
		e := newInstallEnv(t)
		other := filepath.Join(t.TempDir(), "agentfeedback")
		stubBinaries(t, map[string]string{other: "3.9.0"}, other, true)
		r, out := installRun(t, "install", "claude-code", "--server", "local")
		want := "the agentfeedback on PATH (" + other + ", version 3.9.0) is not the one being wired (" + e.exe
		if r.code != 0 || !strings.Contains(r.stderr, "agentfeedback install: warning: "+want) {
			t.Fatalf("%+v", r)
		}
		if w, _ := out["warnings"].([]any); len(w) != 1 || !strings.Contains(w[0].(string), want) {
			t.Fatalf("warnings %v", out["warnings"])
		}
	})
	t.Run("none", func(t *testing.T) {
		e := newInstallEnv(t)
		stubBinaries(t, nil, "", false)
		r, out := installRun(t, "install", "claude-code", "--server", "local", "--dry-run")
		want := "no agentfeedback on PATH; the skill runs agentfeedback from PATH, so add " + e.bin + " to PATH"
		if r.code != 0 || !strings.Contains(r.stderr, "agentfeedback install: warning: "+want) {
			t.Fatalf("%+v", r)
		}
		if w, _ := out["warnings"].([]any); len(w) != 1 || w[0] != want {
			t.Fatalf("warnings %v", out["warnings"])
		}
	})
}

// TestDoctor_HarnessBinaries: doctor lists the binary of every CLI-mode
// harness and the one on PATH, and reports a missing or an older one.
func TestDoctor_HarnessBinaries(t *testing.T) {
	e := newInstallEnv(t)
	stubBinaries(t, map[string]string{e.exe: "4.1.0"}, e.exe, true)
	if r, _ := installRun(t, "install", "claude-code", "--server", "local"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	rep, err := diagnose(os.Getenv, modeFlags{}, "4.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Harnesses, []harnessBinary{{Name: "claude-code", Binary: e.exe, Version: "4.1.0", cli: true}}) ||
		rep.Path != (pathCheck{Binary: e.exe, Version: "4.1.0", Found: true}) || hasProblem(rep, "hooks run") {
		t.Fatalf("%+v", rep)
	}

	stubBinaries(t, map[string]string{e.exe: "4.0.0"}, e.exe, true)
	if rep, _ = diagnose(os.Getenv, modeFlags{}, "4.1.0"); !hasProblem(rep, "the claude-code hooks run "+e.exe+" version 4.0.0, older than this client 4.1.0") ||
		!hasProblem(rep, "agentfeedback install claude-code") {
		t.Fatalf("older: %q", rep.Problems)
	}

	stubBinaries(t, map[string]string{e.exe: "missing"}, "", false)
	if rep, _ = diagnose(os.Getenv, modeFlags{}, "4.1.0"); !hasProblem(rep, "the claude-code hooks run "+e.exe+", which does not exist") ||
		rep.Path.Found {
		t.Fatalf("missing: %q %+v", rep.Problems, rep.Path)
	}

	stubBinaries(t, map[string]string{e.exe: "unknown"}, e.exe, true)
	if rep, _ = diagnose(os.Getenv, modeFlags{}, "4.1.0"); !hasProblem(rep, "the claude-code hooks run "+e.exe+", which does not answer agentfeedback version --json, so they may fail silently") ||
		!hasProblem(rep, "agentfeedback install claude-code") {
		t.Fatalf("unknown: %q", rep.Problems)
	}
	stubBinaries(t, map[string]string{e.exe: "missing"}, "", false)

	human := runCLI(t, "", "doctor")
	if !strings.Contains(human.stdout, "harness:  claude-code "+e.exe+" (missing)") || !strings.Contains(human.stdout, "path:     no agentfeedback on PATH") {
		t.Fatalf("human %+v", human)
	}
}

// TestBinaryVersion runs the real probe: a binary printing its version, and
// a path with nothing there.
func TestBinaryVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agentfeedback")
	putFile(t, bin, "#!/bin/sh\necho '{\"version\":\"4.1.0\"}'\n", 0o755)
	if got := binaryVersion(bin); got != "4.1.0" {
		t.Fatalf("version %q", got)
	}
	silent := filepath.Join(dir, "silent")
	putFile(t, silent, "#!/bin/sh\n", 0o755)
	if got := binaryVersion(silent); got != "unknown" {
		t.Fatalf("silent %q", got)
	}
	if got := binaryVersion(filepath.Join(dir, "absent")); got != "missing" {
		t.Fatalf("absent %q", got)
	}
}

// TestDoctor_ConfigProblems: doctor reports a misspelt [collect] key, a
// relative deny_paths entry and a repository file key that does not narrow.
func TestDoctor_ConfigProblems(t *testing.T) {
	cfgPath, _ := isolate(t)
	stubBinaries(t, nil, "", false)
	putFile(t, cfgPath, "future = 1\n[collect]\ndeny_path = [\"/x\"]\ndeny_paths = [\"rel\"]\n[context]\ncwdd = true\n", 0o600)
	repo := t.TempDir()
	putFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	putFile(t, filepath.Join(repo, ".agentfeedback.toml"), "[collect]\nopt_in_only = true\n", 0o644)
	t.Chdir(repo)
	rep, r := doctorJSON(t)
	if r.code != 1 || rep.Status != "error" {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{
		cfgPath + ": unknown key collect.deny_path is ignored; check its spelling (narrowing it names is not applied).",
		cfgPath + ": unknown key context.cwdd is ignored",
		`collect.deny_paths: ignoring "rel": not an absolute path or ~/ path`,
		`ignoring "collect.opt_in_only": a repository file may only narrow`,
	} {
		if !hasProblem(rep, want) {
			t.Errorf("no problem %q in %q", want, rep.Problems)
		}
	}
	if hasProblem(rep, "future") {
		t.Errorf("a top-level key was reported: %q", rep.Problems)
	}
	if len(rep.Collect.Warnings) != 2 {
		t.Errorf("collect warnings %q", rep.Collect.Warnings)
	}
	if human := runCLI(t, "", "doctor"); !strings.Contains(human.stdout, "collect:  2 warning(s)") {
		t.Errorf("human %+v", human)
	}
}

// TestBinaryVersion_NotARegularFile: a directory at the path is "unknown",
// a missing path "missing", without running anything.
func TestBinaryVersion_NotARegularFile(t *testing.T) {
	dir := t.TempDir()
	if v := binaryVersion(dir); v != "unknown" {
		t.Fatalf("directory: %q", v)
	}
	if v := binaryVersion(filepath.Join(dir, "absent")); v != "missing" {
		t.Fatalf("absent: %q", v)
	}
}

// TestUnknownConfigKeys_CaseInsensitive: keys match as go-toml v2 decodes
// them, ignoring case, so Deny_Paths is known and deny_path is not.
func TestUnknownConfigKeys_CaseInsensitive(t *testing.T) {
	got := unknownConfigKeys([]byte("[collect]\nDeny_Paths = [\"/x\"]\ndeny_path = [\"/y\"]\n"))
	if !slices.Equal(got, []string{"collect.deny_path"}) {
		t.Fatalf("%q", got)
	}
}

// TestUnknownConfigKeys_Detect: the detect table is checked too.
func TestUnknownConfigKeys_Detect(t *testing.T) {
	got := unknownConfigKeys([]byte("[detect]\nnudge = false\nsame_tool = 4\nsame_tools = 4\n"))
	if !slices.Equal(got, []string{"detect.same_tools"}) {
		t.Fatalf("%q", got)
	}
}
