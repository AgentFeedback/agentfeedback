package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

func TestPrime(t *testing.T) {
	isolateCLI(t)
	want, err := skillgen.Prime()
	if err != nil {
		t.Fatal(err)
	}
	r := runCLI(t, "", "prime")
	if r.code != 0 || r.stdout != string(want) {
		t.Fatalf("prime: %+v", r)
	}
	if !strings.Contains(r.stdout, "agentfeedback submit friction") {
		t.Errorf("prime does not teach submit friction:\n%s", r.stdout)
	}
	r = runCLI(t, "", "prime", "--format", "cursor")
	var v map[string]string
	if r.code != 0 || !strings.HasSuffix(r.stdout, "}\n") || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("prime --format cursor: %+v", r)
	}
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || len(v) != 1 || v["additional_context"] != string(want) {
		t.Fatalf("prime --format cursor is not the object Cursor reads: %v %q", err, r.stdout)
	}
	if strings.Contains(r.stdout, `\u003c`) {
		t.Error("prime --format cursor escapes HTML")
	}
	if r := runCLI(t, "", "prime", "--format", "html"); r.code != 2 || !strings.Contains(r.stderr, `unknown format "html"`) {
		t.Errorf("unknown format: %+v", r)
	}
	if r := runCLI(t, "", "prime", "extra"); r.code != 2 {
		t.Errorf("an argument: %+v", r)
	}
}

// The session-start entries run prime, by default; --with-reminder=false
// leaves them out, and the notes about a reminder that does not apply come
// only with the flag given.
func TestInstall_ReminderRunsPrime(t *testing.T) {
	e := newInstallEnv(t)
	r, out := installRun(t, "install", "claude-code", "codex", "cursor", "opencode", "--server", "https://feedback.example.test")
	if r.code != 0 || out["status"] != "installed" {
		t.Fatalf("%+v", r)
	}
	for file, want := range map[string]string{
		filepath.Join(".claude", "settings.json"): `"command": "` + e.exe + ` prime"`,
		filepath.Join(".codex", "hooks.json"):     `"command": "` + e.exe + ` prime"`,
		filepath.Join(".cursor", "hooks.json"):    `"command": "` + e.exe + ` prime --format cursor"`,
	} {
		data, err := os.ReadFile(filepath.Join(e.home, file))
		if err != nil || !strings.Contains(string(data), want) {
			t.Errorf("%s lacks %s:\n%s", file, want, data)
		}
		if strings.Contains(string(data), "skill reminder") {
			t.Errorf("%s still runs skill reminder", file)
		}
	}
	for _, n := range []string{"claude-code", "codex", "cursor"} {
		if got := harnessField(t, out, n, "reminder"); got != "wired" {
			t.Errorf("%s: reminder %v", n, got)
		}
	}
	if notes := notesOf(out); strings.Contains(notes, "reminder is not supported") {
		t.Errorf("a note for a reminder nobody asked for: %q", notes)
	}

	r, out = installRun(t, "install", "claude-code", "opencode", "--with-reminder=false")
	if r.code != 0 || harnessField(t, out, "claude-code", "reminder") != "-" {
		t.Fatalf("--with-reminder=false: %+v", r)
	}
	if data, _ := os.ReadFile(filepath.Join(e.home, ".claude", "settings.json")); strings.Contains(string(data), "SessionStart") {
		t.Errorf("--with-reminder=false kept the SessionStart entry:\n%s", data)
	}
	_, out = installRun(t, "install", "opencode", "--with-reminder")
	if notes := notesOf(out); !strings.Contains(notes, "the session-start reminder is not supported for opencode") {
		t.Errorf("notes %q", notes)
	}
}

func TestInstallCheck(t *testing.T) {
	e := newInstallEnv(t)
	check := func(args ...string) (result, ruleCheckOutcome) {
		t.Helper()
		r := runCLI(t, "", append([]string{"install", "--check", "--json"}, args...)...)
		var v ruleCheckOutcome
		if err := json.Unmarshal([]byte(lastLine(r.stdout)), &v); err != nil {
			t.Fatalf("not JSON: %+v", r)
		}

		return r, v
	}
	before := snapshot(t, e.home)
	r, v := check("claude-code", "codex", "cursor")
	if r.code != 3 || v.Status != "not_current" || len(v.Harnesses) != 3 || v.Harnesses[0].Rule != harness.RuleMissing || v.Harnesses[2].Rule != "-" {
		t.Fatalf("before install: %+v", r)
	}
	sameTree(t, "after --check", snapshot(t, e.home), before)
	if r, _ := check("cursor"); r.code != 0 {
		t.Errorf("a harness without a rule file: %+v", r)
	}
	if r, _ := installRun(t, "install", "claude-code", "codex", "--server", "https://feedback.example.test"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if r, v := check("claude-code", "codex", "cursor"); r.code != 0 || v.Status != "current" || v.Harnesses[0].File != filepath.Join(e.home, ".claude", "CLAUDE.md") {
		t.Fatalf("after install: %+v", r)
	}
	// No names: the detected and recorded harnesses.
	if r, v := check(); r.code != 0 || len(v.Harnesses) != 2 {
		t.Errorf("recorded: %+v", r)
	}
	file := filepath.Join(e.home, ".codex", "AGENTS.md")
	data, _ := os.ReadFile(file)
	putFile(t, file, strings.Replace(string(data), "Always", "Never", 1), 0o644)
	if r, v := check(); r.code != 3 || v.Harnesses[1].Rule != harness.RuleStale {
		t.Errorf("an edited section: %+v", r)
	}
	r = runCLI(t, "", "install", "--check", "codex")
	if r.code != 3 || !strings.Contains(r.stdout, "HARNESS") || !strings.Contains(r.stdout, file) || !strings.Contains(r.stdout, "stale") {
		t.Errorf("table: %+v", r)
	}
	if r := runCLI(t, "", "install", "--check", "nope"); r.code != 2 {
		t.Errorf("an unknown harness: %+v", r)
	}
}

// projectRepo is a fixture repository the working directory moves into;
// with git on PATH it is a real one with a first commit, and head reads its
// HEAD.
func projectRepo(t *testing.T) (dir string, head func() string) {
	t.Helper()
	git, gitErr := exec.LookPath("git")
	e := newInstallEnv(t)
	dir = filepath.Join(filepath.Dir(e.home), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	head = func() string { return "" }
	if gitErr != nil {
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		gitRun := func(args ...string) string {
			t.Helper()
			cmd := exec.Command(git, append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"}, args...)...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}

			return strings.TrimSpace(string(out))
		}
		gitRun("init", "-q")
		gitRun("commit", "-q", "--allow-empty", "-m", "first")
		head = func() string { return gitRun("rev-parse", "HEAD") + " " + gitRun("rev-list", "--count", "HEAD") }
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	return dir, head
}

func TestInstallProject(t *testing.T) {
	dir, head := projectRepo(t)
	headBefore := head()
	manifest := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agentfeedback", "install.json")
	agents := filepath.Join(dir, "AGENTS.md")
	want, err := harness.ProjectSection()
	if err != nil {
		t.Fatal(err)
	}

	r, out := installRun(t, "install", "--project")
	if r.code != 1 || out["status"] != "error" || !strings.Contains(r.stdout, "--yes") {
		t.Fatalf("off a terminal without --yes: %+v", r)
	}
	if _, err := os.Stat(agents); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the refusal wrote the file")
	}
	if r, out := installRun(t, "install", "--project", "--dry-run"); r.code != 0 || out["status"] != "dry_run" {
		t.Fatalf("dry run: %+v", r)
	}
	if _, err := os.Stat(agents); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the dry run wrote the file")
	}
	if r := runCLI(t, "", "install", "--check", "--project", "--json"); r.code != 3 || !strings.Contains(r.stdout, `"project":{"repo":"`+dir+`","file":"`+agents+`","rule":"missing"}`) {
		t.Errorf("check before: %+v", r)
	}

	r, out = installRun(t, "install", "--project", "--yes")
	if r.code != 0 || out["status"] != "installed" || !strings.Contains(r.stdout, agents) {
		t.Fatalf("--yes: %+v", r)
	}
	if got, _ := os.ReadFile(agents); string(got) != want {
		t.Errorf("AGENTS.md is\n%q", got)
	}
	if !strings.Contains(want, "agentfeedback prime") || strings.Contains(want, "Always surface friction") {
		t.Errorf("the project section is not the pointer:\n%s", want)
	}
	if r := runCLI(t, "", "install", "--check", "--project"); r.code != 0 || !strings.Contains(r.stdout, "current") {
		t.Errorf("check after: %+v", r)
	}
	if r, out := installRun(t, "install", "--project", "--yes"); r.code != 0 || out["status"] != "unchanged" {
		t.Errorf("rerun: %+v", r)
	}
	if _, err := os.Stat(manifest); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("install --project wrote the manifest: %v", err)
	}
	if got := head(); got != headBefore {
		t.Errorf("HEAD moved: %s, was %s", got, headBefore)
	}

	// A terminal asks; anything but y refuses.
	isTerminal = func() bool { return true }
	if r := runCLI(t, "n\n", "install", "--project", "--uninstall"); r.code != 1 || !strings.Contains(r.stderr, "Remove the AgentFeedback pointer section from AGENTS.md in "+dir+"? [y/N] ") {
		t.Fatalf("answered n: %+v", r)
	}
	if got, _ := os.ReadFile(agents); string(got) != want {
		t.Error("the refusal changed the file")
	}
	// No answer is not a yes.
	if r, out := installRun(t, "install", "--project", "--uninstall"); r.code != 1 || out["status"] != "error" {
		t.Fatalf("answered nothing: %+v", r)
	}
	if _, err := os.Stat(agents); err != nil {
		t.Fatal("an empty answer removed the file")
	}
	if r := runCLI(t, "y\n", "install", "--project", "--uninstall"); r.code != 0 {
		t.Fatalf("answered y: %+v", r)
	}
	if _, err := os.Stat(agents); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the file holding only the section is still there: %v", err)
	}
	if r := runCLI(t, "y\n", "install", "--project", "--uninstall"); r.code != 0 || !strings.Contains(r.stdout, `"status":"unchanged"`) {
		t.Errorf("uninstall without a section: %+v", r)
	}
	if got := head(); got != headBefore {
		t.Errorf("HEAD moved: %s, was %s", got, headBefore)
	}
}

func TestInstallProject_ClaudeMDOnly(t *testing.T) {
	dir, _ := projectRepo(t)
	claude := filepath.Join(dir, "CLAUDE.md")
	putFile(t, claude, "# Mine\n\nkeep\n", 0o600)
	if r, _ := installRun(t, "install", "--project", "--yes"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	want, _ := harness.ProjectSection()
	if got, _ := os.ReadFile(claude); string(got) != "# Mine\n\nkeep\n\n"+want {
		t.Errorf("CLAUDE.md is\n%q", got)
	}
	if info, _ := os.Stat(claude); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("AGENTS.md was created beside CLAUDE.md")
	}
	// An edited section is replaced in place.
	data, _ := os.ReadFile(claude)
	putFile(t, claude, strings.Replace(string(data), "## AgentFeedback", "## Edited", 1)+"after\n", 0o600)
	if r, out := installRun(t, "install", "--project", "--yes"); r.code != 0 || out["status"] != "installed" {
		t.Fatalf("%+v", r)
	}
	if got, _ := os.ReadFile(claude); string(got) != "# Mine\n\nkeep\n\n"+want+"after\n" {
		t.Errorf("CLAUDE.md is\n%q", got)
	}
	if r, _ := installRun(t, "install", "--project", "--uninstall", "--yes"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if got, _ := os.ReadFile(claude); string(got) != "# Mine\n\nkeep\nafter\n" {
		t.Errorf("CLAUDE.md is\n%q", got)
	}
}

func TestInstallProject_Refusals(t *testing.T) {
	projectRepo(t)
	for _, args := range [][]string{{"install", "--project", "claude-code"}, {"install", "--project", "--mcp"}, {"install", "--yes"}, {"install", "claude-code", "--uninstall"}} {
		if r := runCLI(t, "", args...); r.code != 2 {
			t.Errorf("%v: %+v", args, r)
		}
	}
	t.Chdir(t.TempDir())
	if r := runCLI(t, "", "install", "--project", "--yes"); r.code != 1 || !strings.Contains(r.stdout, "no git repository contains") {
		t.Errorf("outside a repository: %+v", r)
	}
}

// A linked or non-regular instruction file is refused by install and
// uninstall; install --check --project still reads it.
func TestInstallProject_LinkRefused(t *testing.T) {
	dir, _ := projectRepo(t)
	target := filepath.Join(dir, "elsewhere.md")
	putFile(t, target, "mine\n", 0o644)
	agents := filepath.Join(dir, "AGENTS.md")
	if err := os.Symlink(target, agents); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"install", "--project", "--yes"}, {"install", "--project", "--uninstall", "--yes"}} {
		if r := runCLI(t, "", args...); r.code != 1 || !strings.Contains(r.stdout, agents+" is a symbolic link") {
			t.Errorf("%v: %+v", args, r)
		}
	}
	if got, _ := os.ReadFile(target); string(got) != "mine\n" {
		t.Errorf("the target changed: %q", got)
	}
	if info, err := os.Lstat(agents); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
	if r := runCLI(t, "", "install", "--check", "--project"); r.code != 3 || !strings.Contains(r.stdout, "missing") {
		t.Errorf("check: %+v", r)
	}
	if err := os.Remove(agents); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := runCLI(t, "", "install", "--project", "--yes"); r.code != 1 || !strings.Contains(r.stdout, agents+" is not a regular file") {
		t.Errorf("directory: %+v", r)
	}
}

// changingReader edits the file when the prompt is answered.
type changingReader struct {
	t    *testing.T
	path string
	done bool
}

func (c *changingReader) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	c.done = true
	putFile(c.t, c.path, "edited meanwhile\n", 0o644)

	return copy(p, "y\n"), nil
}

func TestInstallProject_ChangedWhileRunning(t *testing.T) {
	dir, _ := projectRepo(t)
	agents := filepath.Join(dir, "AGENTS.md")
	putFile(t, agents, "mine\n", 0o644)
	isTerminal = func() bool { return true }
	var stdout, stderr strings.Builder
	err := runInstallProject(false, false, false, &changingReader{t: t, path: agents}, &stdout, &stderr)
	if err == nil || !strings.Contains(stdout.String(), agents+" changed while install --project ran") {
		t.Errorf("%v: %s", err, stdout.String())
	}
	if got, _ := os.ReadFile(agents); string(got) != "edited meanwhile\n" {
		t.Errorf("AGENTS.md is %q", got)
	}
}

func TestInstallProject_SyncsDirectory(t *testing.T) {
	dir, _ := projectRepo(t)
	var synced []string
	orig := syncProjectDir
	syncProjectDir = func(d string) error { synced = append(synced, d); return nil }
	t.Cleanup(func() { syncProjectDir = orig })
	if r, _ := installRun(t, "install", "--project", "--yes"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if r, _ := installRun(t, "install", "--project", "--uninstall", "--yes"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if len(synced) != 2 || synced[0] != dir || synced[1] != dir {
		t.Errorf("synced %q", synced)
	}
}

// Uninstall deletes the file only when nothing at all remains.
func TestInstallProject_WhitespaceKept(t *testing.T) {
	dir, _ := projectRepo(t)
	agents := filepath.Join(dir, "AGENTS.md")
	want, _ := harness.ProjectSection()
	putFile(t, agents, "\n\n"+want, 0o644)
	if r, _ := installRun(t, "install", "--project", "--uninstall", "--yes"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	got, err := os.ReadFile(agents)
	if err != nil || strings.TrimSpace(string(got)) != "" || len(got) == 0 {
		t.Errorf("AGENTS.md: %q %v", got, err)
	}
}

func TestInstallCheck_NamesWithProject(t *testing.T) {
	projectRepo(t)
	if r := runCLI(t, "", "install", "--check", "codex", "--project", "--json"); r.code != 3 || !strings.Contains(r.stdout, `"name":"codex"`) || !strings.Contains(r.stdout, `"project":`) {
		t.Errorf("%+v", r)
	}
}
