package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
)

// initRun runs init with --json and decodes its one line.
func initRun(t *testing.T, stdin string, args ...string) (result, initResult) {
	t.Helper()
	r := runCLI(t, stdin, append([]string{"init", "--json"}, args...)...)
	var out initResult
	if err := json.Unmarshal([]byte(lastLine(r.stdout)), &out); err != nil {
		t.Fatalf("init %v: last stdout line is not JSON: %+v", args, r)
	}

	return r, out
}

// detectHarnesses makes the named harnesses detected by their config
// directories.
func (e installEnv) detectHarnesses(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(e.home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func harnessByName(st []harness.HarnessStatus, name string) *harness.HarnessStatus {
	i := slices.IndexFunc(st, func(h harness.HarnessStatus) bool { return h.Name == name })
	if i < 0 {
		return nil
	}

	return &st[i]
}

func checkE2E(t *testing.T, steps []e2eStep) {
	t.Helper()
	if len(steps) != 3 {
		t.Fatalf("e2e steps %+v", steps)
	}
	for i, want := range []string{"submit", "list", "mark"} {
		if steps[i].Step != want || steps[i].Outcome != "ok" || steps[i].ID != steps[0].ID || steps[0].ID == 0 {
			t.Fatalf("e2e step %d: %+v", i, steps[i])
		}
	}
}

func nextCommands(n []initNext) []string {
	var out []string
	for _, s := range n {
		out = append(out, s.Command)
	}

	return out
}

func TestInit_YesLocalWiresDetected(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude", ".codex")
	cfg, err := configPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}

	r, out := initRun(t, "", "--yes")
	if r.code != 0 || out.Status != "ok" || out.Mode != "local" || out.Server != "" {
		t.Fatalf("init: %+v", r)
	}
	for _, name := range []string{"claude-code", "codex"} {
		h := harnessByName(out.Harnesses, name)
		if h == nil || h.Mode != "cli" || h.Skill != "wired" || h.Hook != "wired" || h.Rule != "wired" {
			t.Fatalf("%s: %+v", name, h)
		}
	}
	if len(out.Harnesses) != 2 {
		t.Fatalf("harnesses %+v", out.Harnesses)
	}
	checkE2E(t, out.E2E)
	if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) || out.Config != "" {
		t.Fatalf("init wrote a config file in local mode: %v %q", err, out.Config)
	}
	if out.DataDir != dataRoot(t) || out.Database != filepath.Join(dataRoot(t), "agentfeedback.db") {
		t.Fatalf("data dir %q, database %q", out.DataDir, out.Database)
	}
	if _, err := os.Stat(out.Database); err != nil {
		t.Fatalf("the local database: %v", err)
	}
	next := nextCommands(out.Next)
	for _, want := range []string{"agentfeedback ui", "agentfeedback list --open", "agentfeedback uninstall claude-code codex"} {
		if !slices.Contains(next, want) {
			t.Errorf("next %q lacks %q", next, want)
		}
	}
	if !slices.ContainsFunc(next, func(c string) bool { return strings.Contains(c, "agentfeedback-triage") }) {
		t.Errorf("next %q names no triage step", next)
	}
	// Local mode: the hooks nudge and send nothing.
	if !slices.ContainsFunc(out.Hooks, func(h string) bool {
		return strings.HasPrefix(h, "claude-code hook: counts the session's failed tool calls") && !strings.Contains(h, "spooled")
	}) || !slices.Contains(out.Hooks, "claude-code session start: runs agentfeedback prime, which prints the reporting guidance into the session") {
		t.Errorf("hooks %q", out.Hooks)
	}

	// The summary form names the same things.
	r = runCLI(t, "", "init", "--yes")
	for _, want := range []string{"mode:     local", "data:     " + dataRoot(t), "harness:  claude-code (skill wired", "check:    submit:", "next:     agentfeedback ui"} {
		if r.code != 0 || !strings.Contains(r.stdout, want) {
			t.Fatalf("summary lacks %q: %+v", want, r)
		}
	}
}

func TestInit_ServerWritesConfig(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude")
	cfg, err := configPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}

	// No key: the URL alone, and the check waits for the key.
	r, out := initRun(t, "", "--server", "https://feedback.example.test", "--yes")
	if r.code != 0 || out.Status != "ok" || out.Mode != "remote" || out.Server != "https://feedback.example.test" || out.Config != cfg {
		t.Fatalf("init: %+v", r)
	}
	data, err := os.ReadFile(cfg)
	if err != nil || string(data) != "url = 'https://feedback.example.test'\n" {
		t.Fatalf("config %q (%v)", data, err)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", fi.Mode())
	}
	if len(out.E2E) != 1 || out.E2E[0].Outcome != "skipped" {
		t.Fatalf("e2e %+v", out.E2E)
	}
	if len(out.Next) == 0 || !strings.Contains(out.Next[0].Command, "doctor --init --url https://feedback.example.test --key-from-stdin --force") {
		t.Fatalf("next %+v", out.Next)
	}
	if h := harnessByName(out.Harnesses, "claude-code"); h == nil || h.Hook != "wired" {
		t.Fatalf("claude-code %+v", h)
	}
	if !slices.ContainsFunc(out.Hooks, func(h string) bool { return strings.Contains(h, "spooled") }) {
		t.Errorf("hooks %q", out.Hooks)
	}

	// Run again: the configured server is kept and nothing is rewritten.
	r, out = initRun(t, "", "--yes")
	if r.code != 0 || out.Server != "https://feedback.example.test" || out.Config != "" {
		t.Fatalf("second init: %+v", r)
	}
}

func TestInit_ServerKeyFromStdinRunsTheCheck(t *testing.T) {
	newInstallEnv(t)
	l := newLive(t, testKey)
	cfg, _ := configPath(os.Getenv)

	r, out := initRun(t, testKey+"\n", "--server", l.srv.URL, "--key-from-stdin", "--harnesses", "none")
	if r.code != 0 || out.Status != "ok" || len(out.Harnesses) != 0 {
		t.Fatalf("init: %+v", r)
	}
	checkE2E(t, out.E2E)
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), "api_key = '"+testKey+"'") {
		t.Fatalf("config %q", data)
	}
	if strings.Contains(r.stdout+r.stderr, testKey) {
		t.Fatal("the key reached the output")
	}
	if rec := l.record(t, out.E2E[0].ID); rec["kind"] != "install-check" {
		t.Fatalf("record %v", rec)
	}
}

func TestInit_Interactive(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude", ".codex")
	isTerminal = func() bool { return true }

	// Enter for local, then one of the two detected harnesses.
	r, out := initRun(t, "\ncodex\n")
	if r.code != 0 || out.Mode != "local" || len(out.Harnesses) != 1 || out.Harnesses[0].Name != "codex" {
		t.Fatalf("init: %+v", r)
	}
	if !strings.Contains(r.stderr, "Where should reports go?") || !strings.Contains(r.stderr, "Detected: claude-code, codex.") {
		t.Fatalf("prompts: %q", r.stderr)
	}
	checkE2E(t, out.E2E)

	// A server, its key typed at the hidden prompt, and Enter for all.
	l := newLive(t, testKey)
	origSecret := readSecret
	readSecret = func() (string, error) { return testKey, nil }
	t.Cleanup(func() { readSecret = origSecret })
	r, out = initRun(t, l.srv.URL+"\n\n")
	if r.code != 0 || out.Mode != "remote" || len(out.Harnesses) != 2 {
		t.Fatalf("init: %+v", r)
	}
	if !strings.Contains(r.stderr, "API key for "+l.srv.URL+" (not shown") || strings.Contains(r.stdout+r.stderr, testKey) {
		t.Fatalf("key prompt: %q", r.stderr)
	}
	checkE2E(t, out.E2E)
}

func TestInit_NoHooksAndMCP(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude")
	e.fakeClaude(t)

	r, out := initRun(t, "", "--yes", "--no-hooks")
	h := harnessByName(out.Harnesses, "claude-code")
	if r.code != 0 || h == nil || h.Skill != "wired" || h.Rule != "wired" || h.Hook != "-" || h.Reminder != "-" || len(out.Hooks) != 0 {
		t.Fatalf("init --no-hooks: %+v", r)
	}
	settings, _ := os.ReadFile(filepath.Join(e.home, ".claude", "settings.json"))
	if strings.Contains(string(settings), "agentfeedback") {
		t.Fatalf("settings.json carries a hook: %s", settings)
	}

	r, out = initRun(t, "", "--yes", "--mcp")
	h = harnessByName(out.Harnesses, "claude-code")
	if r.code != 0 || h == nil || h.Mode != "mcp" || h.MCP != "stdio" || h.Skill != "-" || h.Rule != "-" {
		t.Fatalf("init --mcp: %+v", r)
	}
}

func TestInit_NothingDetected(t *testing.T) {
	newInstallEnv(t)

	r, out := initRun(t, "", "--yes")
	if r.code != 0 || out.Status != "ok" || len(out.Harnesses) != 0 {
		t.Fatalf("init: %+v", r)
	}
	if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.Contains(w, "no harness was detected") }) {
		t.Fatalf("warnings %q", out.Warnings)
	}
	checkE2E(t, out.E2E)
	if slices.ContainsFunc(nextCommands(out.Next), func(c string) bool { return strings.HasPrefix(c, "agentfeedback uninstall") }) {
		t.Fatalf("next %+v", out.Next)
	}

	// A named harness is wired even when it was not detected.
	r, out = initRun(t, "", "--harnesses", "claude-code")
	if h := harnessByName(out.Harnesses, "claude-code"); r.code != 0 || h == nil || h.Skill != "wired" {
		t.Fatalf("init --harnesses claude-code: %+v", r)
	}
}

func TestInit_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, e installEnv)
		args    []string
		code    int
		message string
	}{
		{"both targets", nil, []string{"--local", "--server", "https://x.example.test"}, 2, "--local and --server are both set"},
		{"key without server", nil, []string{"--key-from-stdin"}, 2, "--key-from-stdin applies only with --server"},
		{"unknown harness", nil, []string{"--harnesses", "claude-code,nope"}, 2, `"nope"`},
		{"none beside names", nil, []string{"--harnesses", "none,codex"}, 2, "none beside others"},
		{"argument", nil, []string{"claude-code"}, 2, "usage"},
		{"bad URL", nil, []string{"--server", "ftp://x"}, 2, "ftp://x"},
		{"local over a configured server", func(t *testing.T, e installEnv) { e.seed(t) }, []string{"--local"}, 1, "never switches a configured server to local"},
		{"another server than the configured one", func(t *testing.T, e installEnv) { e.seed(t) }, []string{"--server", "https://other.example.test"}, 1, "differs from"},
		{"a config without url", func(t *testing.T, e installEnv) {
			putFile(t, filepath.Join(e.home, ".config", "agentfeedback", "config.toml"), "model = \"m\"\n", 0o600)
		}, []string{"--server", "https://x.example.test"}, 1, "names no server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInstallEnv(t)
			e.detectHarnesses(t, ".claude")
			if tc.setup != nil {
				tc.setup(t, e)
			}
			before := snapshot(t, e.home)
			r, out := initRun(t, "", append([]string{"--yes"}, tc.args...)...)
			if r.code != tc.code || out.Status != "error" || !strings.Contains(out.Message, tc.message) {
				t.Fatalf("init %v: %+v", tc.args, r)
			}
			sameTree(t, "after a refusal", snapshot(t, e.home), before)
		})
	}
}

func TestInit_FailedCheckIsShown(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude")
	l := newLive(t, testKey)
	putFile(t, filepath.Join(e.home, ".config", "agentfeedback", "config.toml"), "url = '"+l.srv.URL+"'\napi_key = 'wrong-key'\n", 0o600)

	// The summary names the failed step and what was wired before it.
	r := runCLI(t, "", "init", "--yes")
	if r.code != 1 || !strings.Contains(r.stdout, "check:    submit:   error:") || !strings.Contains(r.stdout, "harness:  claude-code (skill wired") {
		t.Fatalf("init: %+v", r)
	}
	r, out := initRun(t, "", "--yes")
	if r.code != 1 || out.Status != "error" || len(out.E2E) != 1 || out.E2E[0].Outcome != "error" || out.Message != out.E2E[0].Message {
		t.Fatalf("init --json: %+v", r)
	}
	if strings.Contains(r.stdout+r.stderr, "wrong-key") {
		t.Fatal("the key reached the output")
	}
}

func TestInit_ConfiguredServerKeepsItsConfig(t *testing.T) {
	e := newInstallEnv(t)
	e.seed(t)
	cfg := filepath.Join(e.home, ".config", "agentfeedback", "config.toml")
	before, _ := os.ReadFile(cfg)

	r, out := initRun(t, "secret\n", "--server", "https://feedback.example.test", "--key-from-stdin", "--harnesses", "none")
	if r.code != 1 || !strings.Contains(out.Message, "never edits the configuration") {
		t.Fatalf("init --key-from-stdin over a configured server: %+v", r)
	}
	if after, _ := os.ReadFile(cfg); string(after) != string(before) {
		t.Fatalf("config changed: %s", after)
	}
	// The same server in the environment and no file: still refused.
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envURL, "https://feedback.example.test")
	r, out = initRun(t, "secret\n", "--server", "https://feedback.example.test", "--key-from-stdin", "--harnesses", "none")
	if r.code != 1 || !strings.Contains(out.Message, envURL) {
		t.Fatalf("init --key-from-stdin over %s: %+v", envURL, r)
	}
	if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("init wrote %s: %v", cfg, err)
	}
	// Without a key anywhere the check waits, and the next step does not
	// replace a file it did not write.
	r, out = initRun(t, "", "--yes", "--harnesses", "none")
	if r.code != 0 || out.E2E[0].Outcome != "skipped" || strings.Contains(out.Next[0].Command, "--force") {
		t.Fatalf("init: %+v", r)
	}
}

func TestInit_RecordedServer(t *testing.T) {
	e := newInstallEnv(t)
	e.detectHarnesses(t, ".claude")
	if r, _ := installRun(t, "install", "claude-code", "--server", "https://feedback.example.test"); r.code != 0 {
		t.Fatalf("install: %+v", r)
	}
	cfg, _ := configPath(os.Getenv)

	// Without a terminal, local would rewire away from the recorded server.
	r, out := initRun(t, "", "--yes")
	if r.code != 1 || !strings.Contains(out.Message, "wired to https://feedback.example.test") {
		t.Fatalf("init: %+v", r)
	}
	if r, out = initRun(t, "", "--yes", "--local"); r.code != 0 || out.Mode != "local" {
		t.Fatalf("init --local: %+v", r)
	}
	if h := harnessByName(out.Harnesses, "claude-code"); h == nil || h.Hook != "wired" {
		t.Fatalf("claude-code %+v", h)
	}

	// On a terminal Enter keeps the recorded server.
	if r, _ := installRun(t, "install", "claude-code", "--server", "https://feedback.example.test"); r.code != 0 {
		t.Fatalf("install: %+v", r)
	}
	isTerminal = func() bool { return true }
	origSecret := readSecret
	readSecret = func() (string, error) { return "", nil }
	t.Cleanup(func() { readSecret = origSecret })
	r, out = initRun(t, "\n\n")
	if r.code != 0 || out.Server != "https://feedback.example.test" || out.Config != cfg || !strings.Contains(r.stderr, "the server the harnesses are wired to") {
		t.Fatalf("init on a terminal: %+v", r)
	}
}

func TestInstall_NoHooks(t *testing.T) {
	e := newInstallEnv(t)
	settings := filepath.Join(e.home, ".claude", "settings.json")

	if r, out := installRun(t, "install", "claude-code", "--server", "local"); r.code != 0 || out["status"] != "installed" {
		t.Fatalf("install: %+v", r)
	}
	if data, _ := os.ReadFile(settings); !strings.Contains(string(data), "hook claude-code") {
		t.Fatalf("settings.json without the hook: %s", data)
	}
	// --no-hooks takes the hooks and the reminder out and keeps the rest.
	r, _ := installRun(t, "install", "claude-code", "--no-hooks")
	st := listJSON(t)["claude-code"]
	if r.code != 0 || st.Skill != "wired" || st.Rule != "wired" || st.Hook != "-" || st.Reminder != "-" {
		t.Fatalf("install --no-hooks: %+v, status %+v", r, st)
	}
	if data, _ := os.ReadFile(settings); strings.Contains(string(data), "agentfeedback") {
		t.Fatalf("settings.json still carries a hook: %s", data)
	}
	r, _ = installRun(t, "install", "claude-code", "--no-hooks", "--with-reminder")
	if r.code != 0 || !strings.Contains(r.stderr, "--with-reminder does not apply with --no-hooks") {
		t.Fatalf("install --no-hooks --with-reminder: %+v", r)
	}
}
