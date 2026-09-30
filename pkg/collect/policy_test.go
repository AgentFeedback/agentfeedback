package collect

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/pkg/client"
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
			warns: []string{`"url"`, `"api_key"`, `"key"`, `"machine"`, `"collect.opt_in_paths"`, `"collect.opt_in_only"`,
				`"context.cwd"`, `"context.app"`, `"future.nested.x"`},
		},
		{name: "wrong type", body: "[collect]\ndisabled = \"yes\"\ndeny_paths = 5\n[context]\ndrop = [1]\n", dir: sub,
			warns: []string{`"collect.disabled"`, `"collect.deny_paths"`, `"context.drop"`}},
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
	want := `.agentfeedback.toml: ignoring "url": a repository file may only narrow (collect.disabled, collect.deny_paths, context.drop)`
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
