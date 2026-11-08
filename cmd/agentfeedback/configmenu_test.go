package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func configMenuEnv(t *testing.T) installEnv {
	t.Helper()
	e := newInstallEnv(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(e.home, ".local", "share"))
	// Repository discovery must not read the checkout's own policy.
	t.Chdir(e.home)

	return e
}

func configMenuRun(t *testing.T, input string) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runConfigMenu(strings.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	return stdout.String(), stderr.String()
}

func configMenuChoice(t *testing.T, key string) int {
	t.Helper()
	i := slices.Index(configEditableKeys(), key)
	if i < 0 {
		t.Fatalf("%s has no menu entry", key)
	}

	return i + 1
}

func TestConfigMenuGolden(t *testing.T) {
	// Read fixtures before configMenuEnv changes the working directory.
	want, err := os.ReadFile(filepath.Join("testdata", "config", "menu.golden"))
	if err != nil {
		t.Fatal(err)
	}
	e := configMenuEnv(t)
	before := snapshot(t, e.home)
	stdout, stderr := configMenuRun(t, "0\n")
	_, menu, ok := strings.Cut(stdout, "\nMachine configuration menu")
	if !ok {
		t.Fatalf("missing numbered menu: %s", stdout)
	}
	if got := "\nMachine configuration menu" + menu; got != string(want) {
		t.Errorf("menu.golden mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if stderr != "Choice: " {
		t.Errorf("unexpected prompt: %q", stderr)
	}
	sameTree(t, "quit", snapshot(t, e.home), before)
}

func TestConfigMenuPromptGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "config", "menu-prompts.golden"))
	if err != nil {
		t.Fatal(err)
	}
	configMenuEnv(t)
	_, stderr := configMenuRun(t, "not-a-number\n99\n0\n")
	if stderr != string(want) {
		t.Errorf("menu-prompts.golden mismatch:\ngot %q\nwant %q", stderr, want)
	}
}

func TestConfigMenuEveryEditableSetting(t *testing.T) {
	cases := []struct {
		key, raw string
		want     any
	}{
		{"collect.deny_paths", "~/private, /private/archive", []string{"~/private", "/private/archive"}},
		{"collect.opt_in_paths", `["~/work","/work/shared"]`, []string{"~/work", "/work/shared"}},
		{"collect.opt_in_only", "true", true},
		{"collect.disabled", "true", true},
		{"sessions.quiet", "17m0s", "17m0s"},
		{"context.cwd", "true", true},
		{"detect.nudge", "false", false},
		{"detect.same_tool", "11", 11},
		{"detect.same_command", "12", 12},
		{"detect.session_failures", "13", 13},
		{"detect.interval_minutes", "14", 14},
		{"detect.max_per_session", "15", 15},
	}
	var covered []string
	for _, tc := range cases {
		covered = append(covered, tc.key)
		t.Run(tc.key, func(t *testing.T) {
			e := configMenuEnv(t)
			input := fmt.Sprintf("%d\n%s\n0\n", configMenuChoice(t, tc.key), tc.raw)
			stdout, stderr := configMenuRun(t, input)
			if strings.Contains(stderr, "Cannot update") || !strings.Contains(stdout, "Updated "+tc.key+".") {
				t.Fatalf("edit did not succeed:\n%s\n%s", stdout, stderr)
			}
			path, _, settings, err := effectiveConfigSettings(modeFlags{}, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			if path != filepath.Join(e.home, ".config", "agentfeedback", "config.toml") {
				t.Fatalf("write escaped test HOME: %s", path)
			}
			if _, exists, err := loadFileConfig(path); err != nil || !exists {
				t.Fatalf("persisted config: exists=%v err=%v", exists, err)
			}
			for _, setting := range settings {
				if setting.Key == tc.key {
					if !reflect.DeepEqual(setting.Value, tc.want) || setting.Source != "file" || !setting.Set {
						t.Fatalf("persisted setting: %+v; want %#v from file", setting, tc.want)
					}
					return
				}
			}
			t.Fatalf("missing effective setting %s", tc.key)
		})
	}
	if !slices.Equal(covered, configEditableKeys()) {
		t.Fatalf("menu test coverage %v differs from editable keys %v", covered, configEditableKeys())
	}
}

func TestConfigMenuReadOnlyPolicyAndRepository(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "config", "menu-readonly.golden"))
	if err != nil {
		t.Fatal(err)
	}
	e := configMenuEnv(t)
	repo := filepath.Join(e.home, "repository")
	putFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n", 0o600)
	repoPath := filepath.Join(repo, ".agentfeedback.toml")
	const repoPolicy = "[collect]\ndisabled = true\ndeny_paths = [\"private\"]\n[context]\ndrop = [\"machine\"]\n[detect]\nnudge = false\n[sessions]\npush = false\n"
	putFile(t, repoPath, repoPolicy, 0o600)
	t.Chdir(repo)
	stdout, _ := configMenuRun(t, fmt.Sprintf("%d\ntrue\n0\n", configMenuChoice(t, "context.cwd")))
	const policyHeader = "Organisation session policy (read-only):\n"
	_, readonly, ok := strings.Cut(stdout, policyHeader)
	if !ok {
		t.Fatalf("missing read-only policy: %s", stdout)
	}
	readonly, _, ok = strings.Cut(readonly, "\nMachine configuration menu")
	if !ok {
		t.Fatalf("missing menu after read-only display: %s", stdout)
	}
	normalized := strings.ReplaceAll(policyHeader+readonly, e.home, "$HOME")
	if normalized != string(want) {
		t.Errorf("menu-readonly.golden mismatch:\ngot:\n%s\nwant:\n%s", normalized, want)
	}
	got, err := os.ReadFile(repoPath)
	if err != nil || string(got) != repoPolicy {
		t.Fatalf("machine edit changed repository policy: %q, %v", got, err)
	}
	menuStart := strings.Index(stdout, "\nMachine configuration menu")
	if menuStart < 0 {
		t.Fatalf("missing numbered menu: %s", stdout)
	}
	end := strings.Index(stdout[menuStart:], "  0. Quit\n")
	if end < 0 {
		t.Fatalf("missing quit entry: %s", stdout)
	}
	menuEnd := menuStart + end + len("  0. Quit\n")
	for _, forbidden := range []string{"Edit url", "Edit api_key", "Edit sessions.push", "Edit context.drop"} {
		if strings.Contains(stdout[menuStart:menuEnd], forbidden) {
			t.Errorf("read-only setting offered for editing: %s", forbidden)
		}
	}
}

func TestConfigMenuInvalidEditCanRetry(t *testing.T) {
	configMenuEnv(t)
	choice := configMenuChoice(t, "collect.disabled")
	const refused = "not-a-boolean-sensitive-value"
	stdout, stderr := configMenuRun(t, fmt.Sprintf("%d\n%s\n%d\ntrue\n0\n", choice, refused, choice))
	if !strings.Contains(stderr, "Cannot update collect.disabled:") || !strings.Contains(stdout, "Updated collect.disabled.") {
		t.Fatalf("failed edit did not allow retry:\n%s\n%s", stdout, stderr)
	}
	if strings.Contains(stdout+stderr, refused) {
		t.Fatal("refused value leaked into output")
	}
	_, file, _, err := effectiveConfigSettings(modeFlags{}, os.Getenv)
	if err != nil || !file.Collect.Disabled {
		t.Fatalf("retry was not persisted: disabled=%v err=%v", file.Collect.Disabled, err)
	}
}

func TestConfigMenuEOF(t *testing.T) {
	for _, input := range []string{"", "0", "1\n", "14\n", "15\n"} {
		t.Run(fmt.Sprintf("input-%q", input), func(t *testing.T) {
			e := configMenuEnv(t)
			before := snapshot(t, e.home)
			configMenuRun(t, input)
			sameTree(t, "EOF", snapshot(t, e.home), before)
		})
	}
	t.Run("unterminated value", func(t *testing.T) {
		configMenuEnv(t)
		configMenuRun(t, fmt.Sprintf("%d\ntrue", configMenuChoice(t, "context.cwd")))
		_, file, _, err := effectiveConfigSettings(modeFlags{}, os.Getenv)
		if err != nil || !file.Context.Cwd {
			t.Fatalf("final answer was discarded: cwd=%v err=%v", file.Context.Cwd, err)
		}
	})
	t.Run("empty list answer clears", func(t *testing.T) {
		e := configMenuEnv(t)
		putFile(t, filepath.Join(e.home, ".config", "agentfeedback", "config.toml"), "[collect]\ndeny_paths = [\"~/private\"]\n", 0o600)
		configMenuRun(t, "1\n\n0\n")
		_, file, _, err := effectiveConfigSettings(modeFlags{}, os.Getenv)
		if err != nil || len(file.Collect.DenyPaths) != 0 {
			t.Fatalf("empty answer did not clear list: %v, %v", file.Collect.DenyPaths, err)
		}
	})
}

func TestConfigMenuHarnessListIsReadOnly(t *testing.T) {
	e := configMenuEnv(t)
	before := snapshot(t, e.home)
	stdout, stderr := configMenuRun(t, fmt.Sprintf("%d\n0\n", len(configEditableKeys())+1))
	if !strings.Contains(stdout, "HARNESS") || !strings.Contains(stdout, "codex") || strings.Contains(stderr, "Cannot list") {
		t.Fatalf("missing existing harness listing:\n%s\n%s", stdout, stderr)
	}
	sameTree(t, "harness listing", snapshot(t, e.home), before)
}

func TestConfigMenuHarnessRejectsNonSingleTarget(t *testing.T) {
	for _, action := range []int{2, 3} {
		for _, target := range []string{"all", "codex omp", "--server", "not-a-harness", ""} {
			t.Run(fmt.Sprintf("%d-%s", action, target), func(t *testing.T) {
				e := configMenuEnv(t)
				before := snapshot(t, e.home)
				stdout, stderr := configMenuRun(t, fmt.Sprintf("%d\n%s\n0\n", len(configEditableKeys())+action, target))
				if !strings.Contains(stdout, "HARNESS") || !strings.Contains(stderr, "One harness to ") {
					t.Fatalf("target requested without listing:\n%s\n%s", stdout, stderr)
				}
				if target != "" && !strings.Contains(stderr, "Choose exactly one known harness name") {
					t.Fatalf("target was not refused: %s", stderr)
				}
				sameTree(t, "refused harness target", snapshot(t, e.home), before)
			})
		}
	}
}

func TestConfigMenuSingleHarnessAndBufferedInstallPrompt(t *testing.T) {
	e := configMenuEnv(t)
	isTerminal = func() bool { return true }
	putFile(t, filepath.Join(e.home, ".codex", "config.toml"), "model = \"mine\"\n", 0o600)
	other := filepath.Join(e.home, ".cursor")
	putFile(t, filepath.Join(other, "mcp.json"), "{\"mcpServers\":{}}\n", 0o600)
	before := snapshot(t, other)
	// The initial menu read buffers the target, install's destination answer,
	// and the following edit. Forwarding the original stdin would lose them.
	input := fmt.Sprintf("%d\ncodex\nlocal\n%d\ntrue\n0\n", len(configEditableKeys())+2, configMenuChoice(t, "context.cwd"))
	stdout, stderr := configMenuRun(t, input)
	if !strings.Contains(stderr, `AgentFeedback server ("local", "cloud" or a URL): `) {
		t.Fatalf("install did not receive the shared reader: %s", stderr)
	}
	assertConfigMenuHarnessOutcome(t, stdout, "installed", "codex")
	env, err := harnessEnv()
	if err != nil {
		t.Fatal(err)
	}
	if names, err := env.Recorded(); err != nil || !slices.Equal(names, []string{"codex"}) {
		t.Fatalf("install was not exactly one harness: %v, %v", names, err)
	}
	_, file, _, err := effectiveConfigSettings(modeFlags{}, os.Getenv)
	if err != nil || !file.Context.Cwd {
		t.Fatalf("buffered post-install edit was lost: cwd=%v err=%v", file.Context.Cwd, err)
	}
	sameTree(t, "other harness after install", snapshot(t, other), before)
	// An independently installed harness must survive a single-target removal.
	if err := runInstall([]string{"cursor", "--server", "local"}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	before = snapshot(t, other)
	stdout, _ = configMenuRun(t, fmt.Sprintf("%d\ncodex\n0\n", len(configEditableKeys())+3))
	assertConfigMenuHarnessOutcome(t, stdout, "uninstalled", "codex")
	if names, err := env.Recorded(); err != nil || !slices.Equal(names, []string{"cursor"}) {
		t.Fatalf("uninstall affected another harness: %v, %v", names, err)
	}
	sameTree(t, "other harness after uninstall", snapshot(t, other), before)
}

func assertConfigMenuHarnessOutcome(t *testing.T, stdout, status, name string) {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		var out installOutcome
		if json.Unmarshal([]byte(line), &out) == nil && out.Status == status {
			if len(out.Harnesses) != 1 || out.Harnesses[0].Name != name {
				t.Fatalf("%s did not isolate %s: %+v", status, name, out)
			}
			return
		}
	}
	t.Fatalf("missing %s outcome:\n%s", status, stdout)
}

type configMenuErrorReader struct{ err error }

func (r configMenuErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestConfigMenuReadError(t *testing.T) {
	configMenuEnv(t)
	want := errors.New("terminal read failed")
	if err := runConfigMenu(configMenuErrorReader{want}, io.Discard, io.Discard); !errors.Is(err, want) {
		t.Fatalf("read error = %v, want %v", err, want)
	}
}
