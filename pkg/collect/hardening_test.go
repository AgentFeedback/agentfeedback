package collect

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fixtureAt copies the fixture to <base>/repo and returns its path.
func fixtureAt(t *testing.T, base string) string {
	t.Helper()
	repo := filepath.Join(base, "repo")
	copyFixture(t, repo)

	return repo
}

func appendFile(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
}

// bothPaths collects with git (when on PATH) and without, returning the
// results in that order; the git half is skipped when git is missing.
func bothPaths(t *testing.T, dir, home string) (withGit, without Result) {
	t.Helper()
	without = collectFor(dir, home, false)
	withGit = collectFor(dir, home, true)

	return withGit, without
}

func TestDirtySkippedWhenConfigCanRunPrograms(t *testing.T) {
	hermeticGit(t)
	requireGit(t)
	for name, cfg := range map[string]string{
		"filter":    "[filter \"x\"]\n\tclean = touch %s\n\tsmudge = cat\n",
		"process":   "[filter \"x\"]\n\tprocess = touch %s\n",
		"include":   "[include]\n\tpath = other\n",
		"includeIf": "[includeIf \"gitdir:/\"]\n\tpath = other\n",
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			repo := fixtureAt(t, base)
			marker := filepath.Join(base, "ran")
			appendFile(t, filepath.Join(repo, ".git", "config"), strings.ReplaceAll(cfg, "%s", marker))
			write(t, filepath.Join(repo, ".gitattributes"), "* filter=x\n")
			// A fresh mtime makes git re-read the file through the filter.
			write(t, filepath.Join(repo, "README.md"), "fixture, second revision\n")
			r := collectFor(repo, base, true)
			if _, ok := r.Context["git_dirty"]; ok {
				t.Fatalf("git_dirty collected: %v", r.Context)
			}
			if r.Context["git_commit"] != headSHA {
				t.Fatalf("other keys lost: %v", r.Context)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a filter program ran")
			}
		})
	}
}

func TestGitEnvironmentDropped(t *testing.T) {
	hermeticGit(t)
	requireGit(t)
	base := t.TempDir()
	repo := fixtureAt(t, base)
	want := collectFor(repo, base, true)
	trace := filepath.Join(base, "trace")
	t.Setenv("GIT_TRACE", trace)
	t.Setenv("GIT_COMMON_DIR", filepath.Join(base, "bogus"))
	t.Setenv("GIT_DIR", filepath.Join(base, "bogus"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(base, "bogus-index"))
	got := collectFor(repo, base, true)
	if !maps.Equal(got.Context, want.Context) || got.Context["git_dirty"] != "false" {
		t.Fatalf("got %v, want %v", got.Context, want.Context)
	}
	if _, err := os.Stat(trace); err == nil {
		t.Fatal("GIT_TRACE reached git")
	}
}

func TestRefPathContainment(t *testing.T) {
	hermeticGit(t)
	base := t.TempDir()
	repo := fixtureAt(t, base)
	write(t, filepath.Join(base, "outside"), headSHA+"\n")
	write(t, filepath.Join(repo, ".git", "HEAD"), "ref: ../../outside\n")
	without := collectFor(repo, base, false)
	if _, ok := without.Context["git_commit"]; ok {
		t.Fatalf("followed a ref outside the git directory: %v", without.Context)
	}
	for _, name := range []string{"refs/../x", "refs//x", "refs/./x", "/refs/x", `refs\x`, "refs/x\x00", "x", "refs/"} {
		if validRefName(name) {
			t.Errorf("validRefName(%q) = true", name)
		}
	}
	for _, name := range []string{"HEAD", "refs/heads/main", "refs/remotes/origin/HEAD"} {
		if !validRefName(name) {
			t.Errorf("validRefName(%q) = false", name)
		}
	}
	r, _ := findRepo(repo)
	if got := newFileSource(r).defaultBranch("../../x"); got != "" {
		t.Errorf("remote name escaped: %q", got)
	}
}

func TestSymlinkedRepoFileIgnored(t *testing.T) {
	base := t.TempDir()
	repo := minimalRepo(t, filepath.Join(base, "repo"))
	target := filepath.Join(base, "elsewhere.toml")
	write(t, target, "[collect]\ndisabled = true\n")
	if err := os.Symlink(target, filepath.Join(repo, RepoFile)); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	d := Check(repo, base, Policy{})
	if d.Disabled || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "symlink") {
		t.Fatalf("%+v", d)
	}
}

func TestLocalRemoteUnderHome(t *testing.T) {
	hermeticGit(t)
	for _, tt := range []struct{ url, want string }{
		{"%s/src/up.git", "~/src/up.git"},
		{"file://%s/src/up.git", "file://~/src/up.git"},
	} {
		base := t.TempDir()
		home := mkdir(t, filepath.Join(base, "home"))
		repo := fixtureAt(t, base)
		appendFile(t, filepath.Join(repo, ".git", "config"),
			"[remote \"origin\"]\n\turl = "+strings.ReplaceAll(tt.url, "%s", home)+"\n")
		with, without := bothPaths(t, repo, home)
		if without.Context["git_remote"] != tt.want || with.Context["git_remote"] != tt.want || without.Project != "up" {
			t.Errorf("%s: got %q / %q (project %q), want %q", tt.url, with.Context["git_remote"], without.Context["git_remote"], without.Project, tt.want)
		}
	}
}

func TestScrubRemoteUserinfoFirst(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pa#ss@host/r.git":     "https://host/r.git",
		"https://user:pa?ss@host/r.git?q=1": "https://host/r.git",
		"https://u@x:p@host:8443/r#frag":    "https://host:8443/r",
		"https://host/r.git#a@b":            "https://host/r.git",
	} {
		if got := ScrubRemote(in); got != want {
			t.Errorf("ScrubRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHomeDefault(t *testing.T) {
	hermeticGit(t)
	base := t.TempDir()
	repo := fixtureAt(t, base)
	t.Setenv("HOME", base)
	r := Collect(Options{Dir: repo, Getenv: envMap(nil), LookPath: noGit, Hostname: fixedHost})
	if r.Context["repo_root"] != "~/repo" {
		t.Fatalf("repo_root = %q", r.Context["repo_root"])
	}
	if d := Check(repo, "", Policy{DenyPaths: []string{"~/repo"}}); d.Reason != ReasonDenyPaths {
		t.Fatalf("Check with default home: %+v", d)
	}
}

func TestFolderAtHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "canaryuser")
	mkdir(t, home)
	r := Collect(Options{Dir: home, Home: home, LookPath: noGit, Hostname: fixedHost})
	if r.Context["folder"] != "~" || r.Project != "~" {
		t.Fatalf("%+v", r)
	}
}

func TestOptInDoesNotWiden(t *testing.T) {
	base := t.TempDir()
	allowed := mkdir(t, filepath.Join(base, "allowed"))
	private := mkdir(t, filepath.Join(base, "private", "x"))
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(filepath.Dir(private), link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	dir := filepath.Join(link, "x")
	if d := Check(dir, base, Policy{OptInOnly: true, OptInPaths: []string{allowed}}); d.Reason != ReasonOptInOnly {
		t.Fatalf("opt-in through a symlink: %+v", d)
	}
	if d := Check(dir, base, Policy{DenyPaths: []string{filepath.Join(base, "private")}}); d.Reason != ReasonDenyPaths {
		t.Fatalf("deny through a symlink: %+v", d)
	}
	upper := strings.ToUpper(allowed)
	if upper != allowed {
		if d := Check(mkdir(t, filepath.Join(allowed, "y")), base, Policy{OptInOnly: true, OptInPaths: []string{upper}}); d.Reason != ReasonOptInOnly {
			t.Fatalf("opt-in folded case: %+v", d)
		}
	}
}

func TestRepoFileKeySegments(t *testing.T) {
	base := t.TempDir()
	repo := minimalRepo(t, filepath.Join(base, "repo"))
	write(t, filepath.Join(repo, RepoFile), "\"collect.disabled\" = false\n[collect]\ndisabled = true\n")
	d := Check(repo, base, Policy{})
	if d.Reason != ReasonRepoDisabled || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], `\"collect.disabled\"`) {
		t.Fatalf("%+v", d)
	}
	write(t, filepath.Join(repo, RepoFile), "[collect]\n[context]\n")
	if d := Check(repo, base, Policy{}); d.Disabled || len(d.Warnings) != 0 {
		t.Fatalf("empty tables: %+v", d)
	}
}

func TestBranchSharingATagName(t *testing.T) {
	hermeticGit(t)
	base := t.TempDir()
	repo := fixtureAt(t, base)
	write(t, filepath.Join(repo, ".git", "refs", "tags", "main"), headSHA+"\n")
	with, without := bothPaths(t, repo, base)
	for _, r := range []Result{with, without} {
		if r.Context["git_branch"] != "main" || r.Context["git_upstream"] != "origin/main" || r.Context["git_default_branch"] != "main" {
			t.Fatalf("%v", r.Context)
		}
	}
	if with.Context["git_tag"] != "main" || without.Context["git_tag"] != "main" {
		t.Fatalf("tag %q / %q", with.Context["git_tag"], without.Context["git_tag"])
	}
}

func TestUnfetchedUpstream(t *testing.T) {
	hermeticGit(t)
	base := t.TempDir()
	repo := fixtureAt(t, base)
	cfg := filepath.Join(repo, ".git", "config")
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, strings.Replace(string(b), "merge = refs/heads/main", "merge = refs/heads/nope", 1))
	with, without := bothPaths(t, repo, base)
	for _, r := range []Result{with, without} {
		if v, ok := r.Context["git_upstream"]; ok {
			t.Fatalf("upstream %q without a tracking ref", v)
		}
	}
}

func TestParseConfigForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	write(t, path, "[remote \"o]x\"] url = a\n[branch \"b\"]\n\tmerge = refs/heads/\\\nlong\n\tremote = o ; c\n")
	cfg, ok := parseConfig(path)
	want := map[string]string{"remote.o]x.url": "a", "branch.b.merge": "refs/heads/long", "branch.b.remote": "o"}
	if !ok || !maps.Equal(cfg, want) {
		t.Fatalf("got %v %v, want %v", cfg, ok, want)
	}
	write(t, path, "[core]\n\tx = "+strings.Repeat("a", maxMetaBytes)+"\n")
	if cfg, ok := parseConfig(path); ok || len(cfg) != 0 {
		t.Fatalf("oversized config: %v %v", len(cfg), ok)
	}
	if cfg, ok := parseConfig(filepath.Join(t.TempDir(), "missing")); !ok || len(cfg) != 0 {
		t.Fatal("missing config")
	}
}

func TestGitTimeoutFallsBack(t *testing.T) {
	hermeticGit(t)
	base := t.TempDir()
	repo := fixtureAt(t, base)
	r, _ := findRepo(repo)
	fake := fakeGit(t, "if [ \"$3\" = rev-parse ] && [ \"$4\" = --absolute-git-dir ]; then\n"+
		"printf '%s\\n%s\\n' '"+r.gitDir+"' '"+r.root+"'; exit 0; fi\nexec sleep 30\n")
	start := time.Now()
	got := Collect(Options{Dir: repo, Home: base, Getenv: envMap(nil), Hostname: fixedHost, GitTimeout: 100 * time.Millisecond,
		LookPath: func(string) (string, error) { return fake, nil }})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %v", elapsed)
	}
	want := collectFor(repo, base, false)
	if !maps.Equal(got.Context, want.Context) {
		t.Fatalf("got %v, want %v", got.Context, want.Context)
	}
	if !slices.Contains(slices.Collect(maps.Keys(got.Context)), "git_commit") {
		t.Fatal("no fallback")
	}
}
