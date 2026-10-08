package harness

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func wantRule(t *testing.T) string {
	t.Helper()
	text, err := RuleSection()
	if err != nil {
		t.Fatal(err)
	}

	return text
}

func uninstallRequest(names ...string) Request {
	return Request{Harnesses: names, Uninstall: true}
}

// withRule makes install write text as the global instruction section.
func withRule(t *testing.T, text string) {
	t.Helper()
	old := ruleSection
	ruleSection = func() (string, error) { return text, nil }
	t.Cleanup(func() { ruleSection = old })
}

func TestSection_Format(t *testing.T) {
	got := Section("5.0", "## AgentFeedback\n\nrule")
	if !strings.HasPrefix(got, "<!-- agentfeedback:begin v=5.0 hash=") || !strings.HasSuffix(got, " -->\n## AgentFeedback\n\nrule\n<!-- agentfeedback:end -->\n") {
		t.Fatalf("%q", got)
	}
	hash := strings.TrimSuffix(strings.SplitN(strings.TrimPrefix(got, "<!-- agentfeedback:begin v=5.0 hash="), " ", 2)[0], " -->")
	if len(hash) != 16 {
		t.Errorf("hash %q", hash)
	}
	text := wantRule(t)
	if !strings.Contains(text, "agentfeedback prime") {
		t.Errorf("the rule does not name agentfeedback prime:\n%s", text)
	}
}

// Every adapter with a global instruction file gets the section in CLI mode:
// written once, unchanged on a rerun, the user's text kept byte for byte,
// and the file restored from its backup (or deleted when install created
// it) by uninstall.
func TestRule_EveryGlobalFile(t *testing.T) {
	want := wantRule(t)
	for _, a := range registry {
		e := testEnv(t)
		file := a.ruleFile(e)
		if (file == "") != (a.Name == "cursor" || a.Name == "vscode") {
			t.Errorf("%s: rule file %q", a.Name, file)
		}
		if file == "" {
			continue
		}
		for _, orig := range []string{"", "# Mine\n\nkeep this\n", "no trailing newline"} {
			e := testEnv(t)
			file := a.ruleFile(e)
			if orig != "" {
				put(t, file, orig)
			}
			res, err := e.Run(cliRequest(a.Name))
			if err != nil {
				t.Fatalf("%s: %v", a.Name, err)
			}
			got := readFile(t, file)
			sep := ""
			switch {
			case orig == "":
			case strings.HasSuffix(orig, "\n"):
				sep = "\n"
			default:
				sep = "\n\n"
			}
			if got != orig+sep+want {
				t.Errorf("%s %q: file is\n%q", a.Name, orig, got)
			}
			if res.Harnesses[0].Rule != "wired" {
				t.Errorf("%s: rule status %q", a.Name, res.Harnesses[0].Rule)
			}
			if res, err := e.Run(cliRequest(a.Name)); err != nil || res.Status != "unchanged" {
				t.Errorf("%s: rerun %s %v", a.Name, res.Status, err)
			}
			if _, err := e.Run(uninstallRequest(a.Name)); err != nil {
				t.Fatalf("%s: uninstall: %v", a.Name, err)
			}
			data, err := os.ReadFile(file)
			switch {
			case orig == "" && !errors.Is(err, os.ErrNotExist):
				t.Errorf("%s: the created file is still there: %q %v", a.Name, data, err)
			case orig != "" && string(data) != orig:
				t.Errorf("%s: not restored: %q", a.Name, data)
			}
			if _, err := os.Lstat(file + BackupSuffix); err == nil {
				t.Errorf("%s: the backup is still there", a.Name)
			}
		}
	}
}

func TestRule_NotInMCPMode(t *testing.T) {
	e := testEnv(t)
	req := cliRequest("codex")
	req.Options.Mode = ModeMCP
	if _, err := e.Run(req); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.Home, ".codex", "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("MCP mode wrote the rule: %v", err)
	}
}

// An edited section is the install's: a reinstall replaces it where it
// stands, and uninstall removes it.
func TestRule_EditedSectionReplacedInPlace(t *testing.T) {
	e := testEnv(t)
	file := filepath.Join(e.Home, ".claude", "CLAUDE.md")
	put(t, file, "top\n")
	if _, err := e.Run(cliRequest("claude-code")); err != nil {
		t.Fatal(err)
	}
	want := wantRule(t)
	edited := strings.Replace(readFile(t, file), "## AgentFeedback", "## AgentFeedback, edited", 1) + "bottom\n"
	put(t, file, edited)
	res, err := e.Run(cliRequest("claude-code"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "installed" {
		t.Errorf("status %s", res.Status)
	}
	if got := readFile(t, file); got != "top\n\n"+want+"bottom\n" {
		t.Errorf("file is\n%q", got)
	}
	put(t, file, strings.Replace(readFile(t, file), "## AgentFeedback", "## Edited again", 1))
	if _, err := e.Run(uninstallRequest("claude-code")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != "top\nbottom\n" {
		t.Errorf("after uninstall the file is\n%q", got)
	}
}

func TestRule_UninstallRemovesEditedSection(t *testing.T) {
	e := testEnv(t)
	file := filepath.Join(e.Home, ".codex", "AGENTS.md")
	put(t, file, "top\n")
	if _, err := e.Run(cliRequest("codex")); err != nil {
		t.Fatal(err)
	}
	put(t, file, strings.Replace(readFile(t, file), "Always surface friction", "Sometimes surface friction", 1))
	if _, err := e.Run(uninstallRequest("codex")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != "top\n" {
		t.Errorf("file is\n%q", got)
	}
	if _, err := os.Lstat(file + BackupSuffix); err == nil {
		t.Error("the backup equal to the file was kept")
	}
}

// A section the manifest does not record is adopted: replaced in place,
// not refused.
func TestRule_UnrecordedSectionAdopted(t *testing.T) {
	e := testEnv(t)
	file := filepath.Join(e.Home, ".config", "opencode", "AGENTS.md")
	orig := "a\n" + Section("0.1", "## AgentFeedback\n\nold") + "b\n"
	put(t, file, orig)
	if _, err := e.Run(cliRequest("opencode")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != "a\n"+wantRule(t)+"b\n" {
		t.Errorf("file is\n%q", got)
	}
	if _, err := e.Run(uninstallRequest("opencode")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != orig {
		t.Errorf("not restored from the backup:\n%q", got)
	}
}

// A new rule (another skill version) replaces the recorded section in place.
func TestRule_UpgradeReplacesInPlace(t *testing.T) {
	e := testEnv(t)
	file := filepath.Join(e.Home, ".pi", "agent", "AGENTS.md")
	put(t, file, "top\n")
	old := Section("4.0", "## AgentFeedback\n\nold rule")
	withRule(t, old)
	if _, err := e.Run(cliRequest("pi")); err != nil {
		t.Fatal(err)
	}
	put(t, file, readFile(t, file)+"user text below\n")
	if got := ruleState(t, e, "pi"); got != RuleCurrent {
		t.Errorf("check before the upgrade: %s", got)
	}
	next := Section("5.0", "## AgentFeedback\n\nnew rule")
	withRule(t, next)
	if got := ruleState(t, e, "pi"); got != RuleStale {
		t.Errorf("check after the upgrade: %s", got)
	}
	res, err := e.Run(cliRequest("pi"))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != "top\n\n"+next+"user text below\n" {
		t.Errorf("file is\n%q", got)
	}
	if res.Harnesses[0].Rule != "wired" {
		t.Errorf("rule status %q", res.Harnesses[0].Rule)
	}
	if got := ruleState(t, e, "pi"); got != RuleCurrent {
		t.Errorf("check after the reinstall: %s", got)
	}
	if res, err := e.Run(cliRequest("pi")); err != nil || res.Status != "unchanged" {
		t.Errorf("rerun %s %v", res.Status, err)
	}
	if _, err := e.Run(uninstallRequest("pi")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != "top\nuser text below\n" {
		t.Errorf("after uninstall the file is\n%q", got)
	}
}

// ruleState is the rule state env.Check reports for name.
func ruleState(t *testing.T, e Env, name string) string {
	t.Helper()
	got, err := e.Check([]string{name})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}

	return got[0].Rule
}

func TestCheck_States(t *testing.T) {
	e := testEnv(t)
	got, err := e.Check([]string{"claude-code", "cursor", "vscode", "codex"})
	if err != nil {
		t.Fatal(err)
	}
	want := []RuleCheck{
		{Name: "claude-code", File: filepath.Join(e.Home, ".claude", "CLAUDE.md"), Rule: RuleMissing},
		{Name: "cursor", Rule: "-"},
		{Name: "vscode", Rule: "-"},
		{Name: "codex", File: filepath.Join(e.Home, ".codex", "AGENTS.md"), Rule: RuleMissing},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, got[i], want[i])
		}
	}
	put(t, filepath.Join(e.Home, ".codex", "AGENTS.md"), "no section\n")
	if s := ruleState(t, e, "codex"); s != RuleMissing {
		t.Errorf("no section: %s", s)
	}
	put(t, filepath.Join(e.Home, ".codex", "AGENTS.md"), "x\n"+wantRule(t))
	if s := ruleState(t, e, "codex"); s != RuleCurrent {
		t.Errorf("the section: %s", s)
	}
	put(t, filepath.Join(e.Home, ".codex", "AGENTS.md"), "x\n"+strings.Replace(wantRule(t), "Always", "Often", 1))
	if s := ruleState(t, e, "codex"); s != RuleStale {
		t.Errorf("an edited section: %s", s)
	}
}

// antigravity and gemini-cli share ~/.gemini/GEMINI.md: one section, kept
// until the last of the two is uninstalled.
func TestRule_SharedGeminiFile(t *testing.T) {
	for _, together := range []bool{false, true} {
		e := testEnv(t)
		file := filepath.Join(e.Home, ".gemini", "GEMINI.md")
		if together {
			if _, err := e.Run(cliRequest("antigravity", "gemini-cli")); err != nil {
				t.Fatal(err)
			}
		} else {
			for _, n := range []string{"antigravity", "gemini-cli"} {
				if _, err := e.Run(cliRequest(n)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := readFile(t, file); got != wantRule(t) {
			t.Fatalf("together=%v: file is\n%q", together, got)
		}
		st, err := e.Status()
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range st {
			if (h.Name == "antigravity" || h.Name == "gemini-cli") && h.Rule != "wired" {
				t.Errorf("%s: rule %q", h.Name, h.Rule)
			}
		}
		if res, err := e.Run(cliRequest("antigravity", "gemini-cli")); err != nil || res.Status != "unchanged" {
			t.Errorf("rerun %s %v", res.Status, err)
		}
		if _, err := e.Run(uninstallRequest("antigravity")); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, file); got != wantRule(t) {
			t.Errorf("together=%v: after one uninstall the file is\n%q", together, got)
		}
		if _, err := e.Run(uninstallRequest("gemini-cli")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("together=%v: the created file is still there: %v", together, err)
		}
	}
	e := testEnv(t)
	file := filepath.Join(e.Home, ".gemini", "GEMINI.md")
	put(t, file, "mine\n")
	if _, err := e.Run(cliRequest("antigravity", "gemini-cli")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(uninstallRequest("gemini-cli", "antigravity")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != "mine\n" {
		t.Errorf("after uninstalling both the file is\n%q", got)
	}
}

func TestRule_MultipleSections(t *testing.T) {
	e := testEnv(t)
	file := filepath.Join(e.Home, ".kiro", "steering", "AGENTS.md")
	two := Section("1", "a") + "between\n" + Section("1", "b")
	put(t, file, two)
	_, err := e.Run(cliRequest("kiro"))
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Problem, file+" holds more than one agentfeedback section") {
		t.Fatalf("%v", err)
	}
	if got := readFile(t, file); got != two {
		t.Error("the file changed")
	}
	unterminated := "x\n<!-- agentfeedback:begin v=1 -->\nbody\n"
	put(t, file, unterminated)
	if _, err := e.Run(cliRequest("kiro")); !errors.As(err, &r) || !strings.Contains(r.Problem, "without its end marker") {
		t.Fatalf("%v", err)
	}

	// Uninstall leaves such a file as it is, with a note.
	put(t, file, "mine\n")
	if _, err := e.Run(cliRequest("kiro")); err != nil {
		t.Fatal(err)
	}
	// The recorded section, edited, and a second one: neither the exact text
	// nor a single section is there.
	put(t, file, strings.Replace(readFile(t, file), "Always", "Often", 1)+Section("1", "b"))
	before := readFile(t, file)
	res, err := e.Run(uninstallRequest("kiro"))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != before {
		t.Errorf("the file changed:\n%q", got)
	}
	if notes := strings.Join(res.Harnesses[0].Notes, "\n"); !strings.Contains(notes, "more than one agentfeedback section; it is left as it is") {
		t.Errorf("notes %q", notes)
	}
}

// A linked global instruction file is followed, like a configuration file.
func TestRule_LinkedFileFollowed(t *testing.T) {
	e := testEnv(t)
	target := filepath.Join(e.Home, "dotfiles", "CLAUDE.md")
	put(t, target, "mine\n")
	file := filepath.Join(e.Home, ".claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(cliRequest("claude-code")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != "mine\n\n"+wantRule(t) {
		t.Errorf("target is\n%q", got)
	}
	if !isLink(file) {
		t.Error("the link was replaced")
	}
	if _, err := e.Run(uninstallRequest("claude-code")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != "mine\n" {
		t.Errorf("target is\n%q", got)
	}
}

func TestValidateManifest_RuleFiles(t *testing.T) {
	e := testEnv(t)
	if _, err := e.Run(cliRequest("claude-code")); err != nil {
		t.Fatal(err)
	}
	man, _, err := LoadManifest(e.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.validateManifest(man, e.ManifestPath()); err != nil {
		t.Fatal(err)
	}
	for _, it := range man.Harnesses["claude-code"].Items {
		if it.Kind == KindManagedSection {
			man.Harnesses["claude-code"].Items = append(man.Harnesses["claude-code"].Items, Item{Kind: KindManagedSection, Role: RoleRule, File: filepath.Join(e.Home, "AGENTS.md"), Text: it.Text})
		}
	}
	var r *Refusal
	if err := e.validateManifest(man, e.ManifestPath()); !errors.As(err, &r) {
		t.Fatalf("a section in another file was accepted: %v", err)
	}
}

// Uninstalling one of two harnesses sharing ~/.gemini/GEMINI.md leaves the
// file as it is; the last uninstall then still sees a user's edit outside
// the section and removes only the section.
func TestRule_SharedGeminiFileKeepsLaterEdit(t *testing.T) {
	e := testEnv(t)
	file := filepath.Join(e.Home, ".gemini", "GEMINI.md")
	put(t, file, "mine\n")
	if _, err := e.Run(cliRequest("antigravity", "gemini-cli")); err != nil {
		t.Fatal(err)
	}
	put(t, file, "mine, edited\n"+strings.TrimPrefix(readFile(t, file), "mine\n"))
	if _, err := e.Run(uninstallRequest("antigravity")); err != nil {
		t.Fatal(err)
	}
	res, err := e.Run(uninstallRequest("gemini-cli"))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != "mine, edited\n" {
		t.Errorf("after the last uninstall the file is\n%q", got)
	}
	if _, err := os.Lstat(file + BackupSuffix); err != nil {
		t.Errorf("the backup is gone: %v", err)
	}
	if !slices.Contains(res.Backups, file+BackupSuffix) {
		t.Errorf("the kept backup is not listed: %q", res.Backups)
	}
}

// Two identical sections are not the install's section: install refuses,
// uninstall leaves the file with a note.
func TestRule_TwoIdenticalSections(t *testing.T) {
	e := testEnv(t)
	file := filepath.Join(e.Home, ".kiro", "steering", "AGENTS.md")
	if _, err := e.Run(cliRequest("kiro")); err != nil {
		t.Fatal(err)
	}
	two := readFile(t, file) + "\n" + wantRule(t)
	put(t, file, two)
	_, err := e.Run(cliRequest("kiro"))
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Problem, "more than one agentfeedback section") {
		t.Fatalf("install: %v", err)
	}
	res, err := e.Run(uninstallRequest("kiro"))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != two {
		t.Errorf("the file changed:\n%q", got)
	}
	if notes := strings.Join(res.Harnesses[0].Notes, "\n"); !strings.Contains(notes, "more than one agentfeedback section; it is left as it is") {
		t.Errorf("notes %q", notes)
	}
}

func TestFindSection_BeginMarkerWord(t *testing.T) {
	doc := []byte("<!-- agentfeedback:beginner -->\nx\n")
	if _, _, found, err := findSection("f", doc); found || err != nil {
		t.Errorf("beginner read as a marker: found=%v err=%v", found, err)
	}
	doc = []byte("<!-- agentfeedback:begin-->\nx\n" + sectionEnd + "\n")
	if _, _, found, err := findSection("f", doc); !found || err != nil {
		t.Errorf("bare marker: found=%v err=%v", found, err)
	}
}

// A harness the manifest records in MCP mode has no rule to check.
func TestCheck_RecordedMCPMode(t *testing.T) {
	e := testEnv(t)
	req := cliRequest("codex")
	req.Options.Mode = ModeMCP
	if _, err := e.Run(req); err != nil {
		t.Fatal(err)
	}
	got, err := e.Check([]string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (RuleCheck{Name: "codex", Rule: "-"}); len(got) != 1 || got[0] != want {
		t.Errorf("%+v, want %+v", got, want)
	}
}
