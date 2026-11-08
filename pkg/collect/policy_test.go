package collect

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// minimalRepo makes dir a repository root: .git with HEAD is all the walk
// needs.
func minimalRepo(t *testing.T, dir string) string {
	t.Helper()
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")

	return dir
}

func TestCheckUserPolicy(t *testing.T) {
	base := t.TempDir()
	home := mkdir(t, filepath.Join(base, "home"))
	work := mkdir(t, filepath.Join(base, "a", "work"))
	workshop := mkdir(t, filepath.Join(base, "a", "workshop"))
	deep := mkdir(t, filepath.Join(work, "x", "y"))
	inHome := mkdir(t, filepath.Join(home, "secret", "p"))
	real := mkdir(t, filepath.Join(base, "real", "proj"))
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(base, "real"), link); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		dir    string
		p      Policy
		reason string
		warn   string
	}{
		{name: "nothing set", dir: work},
		{name: "disabled", dir: work, p: Policy{Disabled: true}, reason: ReasonDisabled},
		{name: "deny equal path", dir: work, p: Policy{DenyPaths: []string{work}}, reason: ReasonDenyPaths},
		{name: "deny beneath", dir: deep, p: Policy{DenyPaths: []string{work}}, reason: ReasonDenyPaths},
		{name: "deny is per component", dir: workshop, p: Policy{DenyPaths: []string{work}}},
		{name: "deny trailing slash", dir: deep, p: Policy{DenyPaths: []string{work + "/"}}, reason: ReasonDenyPaths},
		{name: "deny tilde", dir: inHome, p: Policy{DenyPaths: []string{"~/secret"}}, reason: ReasonDenyPaths},
		{name: "deny tilde miss", dir: work, p: Policy{DenyPaths: []string{"~/secret"}}},
		{name: "deny home itself", dir: inHome, p: Policy{DenyPaths: []string{"~"}}, reason: ReasonDenyPaths},
		{name: "deny relative ignored", dir: work, p: Policy{DenyPaths: []string{"a/work"}}, warn: `"a/work"`},
		{name: "deny ~user ignored", dir: work, p: Policy{DenyPaths: []string{"~other/x"}}, warn: `"~other/x"`},
		{name: "deny via symlinked dir", dir: filepath.Join(link, "proj"), p: Policy{DenyPaths: []string{filepath.Join(base, "real")}}, reason: ReasonDenyPaths},
		{name: "deny via symlinked entry", dir: real, p: Policy{DenyPaths: []string{link}}, reason: ReasonDenyPaths},
		{name: "opt-in without paths", dir: work, p: Policy{OptInOnly: true}, reason: ReasonOptInOnly},
		{name: "opt-in match", dir: deep, p: Policy{OptInOnly: true, OptInPaths: []string{work}}},
		{name: "opt-in per component", dir: workshop, p: Policy{OptInOnly: true, OptInPaths: []string{work}}, reason: ReasonOptInOnly},
		{name: "opt-in tilde", dir: inHome, p: Policy{OptInOnly: true, OptInPaths: []string{"~/secret"}}},
		{name: "opt-in symlink", dir: filepath.Join(link, "proj"), p: Policy{OptInOnly: true, OptInPaths: []string{real}}},
		{name: "opt-in paths without opt_in_only", dir: workshop, p: Policy{OptInPaths: []string{work}}},
		{name: "deny beats opt-in", dir: deep, p: Policy{DenyPaths: []string{deep}, OptInOnly: true, OptInPaths: []string{work}}, reason: ReasonDenyPaths},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Check(tt.dir, home, tt.p)
			if d.Disabled != (tt.reason != "") || d.Reason != tt.reason {
				t.Fatalf("got %+v, want reason %q", d, tt.reason)
			}
			if tt.warn == "" && len(d.Warnings) != 0 || tt.warn != "" && !slices.ContainsFunc(d.Warnings, func(w string) bool { return strings.Contains(w, tt.warn) }) {
				t.Fatalf("warnings %q, want one naming %s", d.Warnings, tt.warn)
			}
		})
	}
}

func TestCheckRepoFile(t *testing.T) {
	base := t.TempDir()
	repo := minimalRepo(t, filepath.Join(base, "repo"))
	sub := mkdir(t, filepath.Join(repo, "vendor", "x"))
	other := mkdir(t, filepath.Join(repo, "src"))
	outside := mkdir(t, filepath.Join(base, "plain"))
	rf := filepath.Join(repo, RepoFile)

	tests := []struct {
		name   string
		body   string
		dir    string
		reason string
		drop   []string
		warns  []string
	}{
		{name: "no file", dir: repo},
		{name: "disabled", body: "[collect]\ndisabled = true\n", dir: sub, reason: ReasonRepoDisabled},
		{name: "disabled false is a no-op", body: "[collect]\ndisabled = false\n", dir: sub},
		{name: "relative deny hit", body: "[collect]\ndeny_paths = [\"vendor\"]\n", dir: sub, reason: ReasonRepoDenyPaths},
		{name: "relative deny miss", body: "[collect]\ndeny_paths = [\"vendor\"]\n", dir: other},
		{name: "absolute deny", body: "[collect]\ndeny_paths = [\"" + filepath.ToSlash(other) + "\"]\n", dir: other, reason: ReasonRepoDenyPaths},
		{name: "tilde not expanded", body: "[collect]\ndeny_paths = [\"~/x\", \"vendor\"]\n", dir: sub, reason: ReasonRepoDenyPaths, warns: []string{`"~/x"`}},
		{name: "drop", body: "[context]\ndrop = [\"git_remote\", \"repo_root\"]\n", dir: repo, drop: []string{"git_remote", "repo_root"}},
		{
			name: "widening keys ignored, narrowing kept",
			body: "url = \"https://evil\"\napi_key = \"k\"\nkey = \"k\"\nmachine = \"m\"\n" +
				"[collect]\nopt_in_paths = [\"/\"]\nopt_in_only = false\ndeny_paths = [\"vendor\"]\n" +
				"[context]\ncwd = true\napp = \"a\"\ndrop = [\"git_tag\"]\n[future.nested]\nx = 1\n",
			dir: sub, reason: ReasonRepoDenyPaths, drop: []string{"git_tag"},
			warns: []string{
				`"url"`, `"api_key"`, `"key"`, `"machine"`, `"collect.opt_in_paths"`, `"collect.opt_in_only"`,
				`"context.cwd"`, `"context.app"`, `"future.nested.x"`,
			},
		},
		{
			name: "wrong type", body: "[collect]\ndisabled = \"yes\"\ndeny_paths = 5\n[context]\ndrop = [1]\n", dir: sub,
			warns: []string{`"collect.disabled"`, `"collect.deny_paths"`, `"context.drop"`},
		},
		{name: "unparseable", body: "[collect\ndisabled = true\n", dir: sub, warns: []string{RepoFile + ": ignoring the file"}},
		{name: "outside a repository", body: "[collect]\ndisabled = true\n", dir: outside},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Remove(rf)
			if tt.body != "" {
				write(t, rf, tt.body)
			}
			d := Check(tt.dir, base, Policy{})
			if d.Disabled != (tt.reason != "") || d.Reason != tt.reason || !slices.Equal(d.Drop, tt.drop) {
				t.Fatalf("got %+v, want reason %q drop %v", d, tt.reason, tt.drop)
			}
			if len(d.Warnings) != len(tt.warns) {
				t.Fatalf("warnings %q, want %d", d.Warnings, len(tt.warns))
			}
			for _, w := range tt.warns {
				if !slices.ContainsFunc(d.Warnings, func(got string) bool { return strings.Contains(got, w) }) {
					t.Errorf("no warning naming %s in %q", w, d.Warnings)
				}
			}
		})
	}

	write(t, rf, "url = \"x\"\n")
	d := Check(repo, base, Policy{})
	want := `.agentfeedback.toml: ignoring "url": a repository file may only narrow (collect.disabled, collect.deny_paths, context.drop, detect.nudge, sessions.push)`
	if len(d.Warnings) != 1 || d.Warnings[0] != want {
		t.Fatalf("warning %q, want %q", d.Warnings, want)
	}
}

// TestCheckUserBeforeRepo: the user rules decide first; the repository file
// cannot re-enable what they switched off.
func TestCheckUserBeforeRepo(t *testing.T) {
	base := t.TempDir()
	repo := minimalRepo(t, filepath.Join(base, "repo"))
	write(t, filepath.Join(repo, RepoFile), "[collect]\ndisabled = false\n")
	if d := Check(repo, base, Policy{DenyPaths: []string{repo}}); d.Reason != ReasonDenyPaths {
		t.Fatalf("%+v", d)
	}
}

func TestDisabledDecisionOutcome(t *testing.T) {
	base := t.TempDir()
	d := Check(base, base, Policy{Disabled: true})
	o := client.Disabled(d.Reason)
	var b strings.Builder
	if err := o.Write(&b); err != nil {
		t.Fatal(err)
	}
	if o.ExitCode() != 0 || b.String() != `{"outcome":"disabled","reason":"disabled"}`+"\n" {
		t.Fatalf("exit %d, line %q", o.ExitCode(), b.String())
	}
}

// TestLint: every setting problem is listed whatever Check decides: the
// user's relative path entries (even with collection disabled or opt-in off)
// and the repository file's warnings.
func TestLint(t *testing.T) {
	base := t.TempDir()
	home := mkdir(t, filepath.Join(base, "home"))
	repo := minimalRepo(t, mkdir(t, filepath.Join(base, "repo")))
	write(t, filepath.Join(repo, RepoFile), "[collect]\nopt_in_only = true\n")
	plain := mkdir(t, filepath.Join(base, "plain"))

	got := Lint(plain, home, Policy{Disabled: true, DenyPaths: []string{"rel/deny", "~/ok"}, OptInPaths: []string{"rel/in"}})
	want := []string{
		`collect.deny_paths: ignoring "rel/deny": not an absolute path or ~/ path`,
		`collect.opt_in_paths: ignoring "rel/in": not an absolute path or ~/ path`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("user entries: %q, want %q", got, want)
	}
	if got := Lint(plain, home, Policy{DenyPaths: []string{"~/ok", "/abs"}}); len(got) != 0 {
		t.Fatalf("clean policy: %q", got)
	}
	got = Lint(repo, home, Policy{})
	if len(got) != 1 || !strings.Contains(got[0], `ignoring "collect.opt_in_only": a repository file may only narrow`) {
		t.Fatalf("repository file: %q", got)
	}
	if d := Check(plain, home, Policy{DenyPaths: []string{"rel/deny"}}); !slices.Equal(d.Warnings, want[:1]) {
		t.Fatalf("Check warning %q differs from Lint's", d.Warnings)
	}
}

// TestCheckDetectNudge: a repository file's detect.nudge = false switches
// the hook's note off without disabling collection; true is accepted and
// changes nothing; another type is a warning.
func TestCheckDetectNudge(t *testing.T) {
	base := t.TempDir()
	repo := minimalRepo(t, filepath.Join(base, "repo"))
	rf := filepath.Join(repo, RepoFile)
	for _, tt := range []struct {
		body     string
		off      bool
		warnings int
	}{
		{"[detect]\nnudge = false\n", true, 0},
		{"[detect]\nnudge = true\n", false, 0},
		{"[detect]\nnudge = \"no\"\n", false, 1},
		{"[detect]\nsame_tool = 1\n", false, 1},
	} {
		write(t, rf, tt.body)
		d := Check(repo, base, Policy{})
		if d.Disabled || d.NudgeOff != tt.off || len(d.Warnings) != tt.warnings {
			t.Errorf("%q: %+v", tt.body, d)
		}
	}
}

func TestSessionPush(t *testing.T) {
	for _, tt := range []struct {
		name    string
		body    string
		allowed bool
		warning string
	}{
		{"off", "[sessions]\npush = false\n", false, ""},
		{"on", "[sessions]\npush = true\n", true, ""},
		{"wrong type", "[sessions]\npush = \"false\"\n", false, "want a boolean"},
		{"typo", "[sessions]\npuhs = false\n", true, "sessions.puhs"},
		{"global setting", "[sessions]\nquiet = \"1m\"\n", true, "sessions.quiet"},
		{"literal dotted key", "\"sessions.push\" = false\n", true, `\"sessions.push\"`},
		{"malformed", "[sessions", false, "ignoring the file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := minimalRepo(t, t.TempDir())
			write(t, filepath.Join(root, RepoFile), tt.body)
			allowed, warnings := SessionPush(root)
			if allowed != tt.allowed {
				t.Fatalf("allowed = %v, want %v", allowed, tt.allowed)
			}
			if tt.warning == "" {
				if len(warnings) != 0 {
					t.Fatalf("warnings = %q", warnings)
				}
			} else if len(warnings) != 1 || !strings.Contains(warnings[0], tt.warning) {
				t.Fatalf("warnings = %q, want %q", warnings, tt.warning)
			}
			// Repository push=true never overrides an existing collection veto.
			if d := Check(root, "", Policy{Disabled: true}); !d.Disabled {
				t.Fatal("repository expanded the user policy")
			}
		})
	}
}

func TestSessionPushReadProtection(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		allowed, warnings := SessionPush(t.TempDir())
		if !allowed || len(warnings) != 0 {
			t.Fatalf("allowed=%v warnings=%q", allowed, warnings)
		}
	})
	t.Run("not regular", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, RepoFile), 0o700); err != nil {
			t.Fatal(err)
		}
		allowed, warnings := SessionPush(root)
		if allowed || len(warnings) != 1 || !strings.Contains(warnings[0], "not a regular file") {
			t.Fatalf("allowed=%v warnings=%q", allowed, warnings)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(t.TempDir(), "policy.toml")
		write(t, target, "[sessions]\npush = false\n")
		if err := os.Symlink(target, filepath.Join(root, RepoFile)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		allowed, warnings := SessionPush(root)
		if allowed || len(warnings) != 1 || !strings.Contains(warnings[0], "symlink") {
			t.Fatalf("allowed=%v warnings=%q", allowed, warnings)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, RepoFile)
		write(t, path, "[sessions]\npush = false\n")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if _, err := os.ReadFile(path); err == nil {
			t.Skip("process can read mode-000 files")
		}
		allowed, warnings := SessionPush(root)
		if allowed || len(warnings) != 1 || !strings.Contains(warnings[0], "ignoring the file") {
			t.Fatalf("allowed=%v warnings=%q", allowed, warnings)
		}
	})
}

func TestSessionPushFromNestedAndSymlinkCwd(t *testing.T) {
	root := minimalRepo(t, t.TempDir())
	cwd := mkdir(t, filepath.Join(root, "nested", "project"))
	write(t, filepath.Join(root, RepoFile), "[sessions]\npush = false\n")
	if allowed, warnings := SessionPush(cwd); allowed || len(warnings) != 0 {
		t.Fatalf("nested cwd bypassed veto: allowed=%v warnings=%q", allowed, warnings)
	}
	link := filepath.Join(t.TempDir(), "linked-project")
	if err := os.Symlink(cwd, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if allowed, warnings := SessionPush(link); allowed || len(warnings) != 0 {
		t.Fatalf("symlink cwd bypassed veto: allowed=%v warnings=%q", allowed, warnings)
	}
}
