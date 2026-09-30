package collect

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden")

// scenario builds a directory layout under base and returns the working
// directory and home directory to collect with.
type scenario struct {
	name  string
	build func(t *testing.T, base string) (dir, home string)
}

var scenarios = []scenario{
	{"clean", func(t *testing.T, base string) (string, string) {
		copyFixture(t, filepath.Join(base, "repo"))

		return filepath.Join(base, "repo"), mkdir(t, filepath.Join(base, "home"))
	}},
	{"dirty", func(t *testing.T, base string) (string, string) {
		repo := filepath.Join(base, "repo")
		copyFixture(t, repo)
		write(t, filepath.Join(repo, "README.md"), "changed\n")
		write(t, filepath.Join(repo, "untracked.txt"), "new\n")

		return repo, mkdir(t, filepath.Join(base, "home"))
	}},
	{"untracked-only", func(t *testing.T, base string) (string, string) {
		repo := filepath.Join(base, "repo")
		copyFixture(t, repo)
		write(t, filepath.Join(repo, "untracked.txt"), "new\n")

		return repo, mkdir(t, filepath.Join(base, "home"))
	}},
	{"detached", func(t *testing.T, base string) (string, string) {
		repo := filepath.Join(base, "repo")
		copyFixture(t, repo)
		write(t, filepath.Join(repo, ".git", "HEAD"), headSHA+"\n")

		return repo, mkdir(t, filepath.Join(base, "home"))
	}},
	{"outside", func(t *testing.T, base string) (string, string) {
		return mkdir(t, filepath.Join(base, "plain")), mkdir(t, filepath.Join(base, "home"))
	}},
	{"subdir", func(t *testing.T, base string) (string, string) {
		copyFixture(t, filepath.Join(base, "repo"))

		return filepath.Join(base, "repo", "sub"), mkdir(t, filepath.Join(base, "home"))
	}},
	{"worktree", func(t *testing.T, base string) (string, string) {
		repo := filepath.Join(base, "repo")
		copyFixture(t, repo)
		wt := filepath.Join(base, "wt")
		admin := filepath.Join(repo, ".git", "worktrees", "wt")
		copyTree(t, filepath.Join("testdata", "repo", "sub"), filepath.Join(wt, "sub"))
		b, err := os.ReadFile(filepath.Join(repo, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(wt, "README.md"), string(b))
		write(t, filepath.Join(wt, ".git"), "gitdir: ../repo/.git/worktrees/wt\n")
		write(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/feature\n")
		write(t, filepath.Join(admin, "commondir"), "../..\n")
		write(t, filepath.Join(admin, "gitdir"), filepath.Join(wt, ".git")+"\n")
		idx, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(admin, "index"), string(idx))
		write(t, filepath.Join(repo, ".git", "refs", "heads", "feature"), headSHA+"\n")

		return wt, mkdir(t, filepath.Join(base, "home"))
	}},
	{"empty", func(t *testing.T, base string) (string, string) {
		repo := filepath.Join(base, "repo")
		write(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
		write(t, filepath.Join(repo, ".git", "config"), "[core]\n\trepositoryformatversion = 0\n\tbare = false\n")
		mkdir(t, filepath.Join(repo, ".git", "objects"))
		mkdir(t, filepath.Join(repo, ".git", "refs", "heads"))

		return repo, mkdir(t, filepath.Join(base, "home"))
	}},
	{"under-home", func(t *testing.T, base string) (string, string) {
		copyFixture(t, filepath.Join(base, "repo"))

		return filepath.Join(base, "repo"), base
	}},
}

// collectFor runs Collect for a golden scenario, with or without git.
func collectFor(dir, home string, withGit bool) Result {
	opts := Options{Dir: dir, Home: home, Getenv: envMap(nil), Hostname: fixedHost}
	if !withGit {
		opts.LookPath = noGit
	}

	return Collect(opts)
}

// normalise removes what varies by machine: os and arch are checked here and
// dropped, temporary paths become <tmp>.
func normalise(t *testing.T, r Result, base string) []byte {
	t.Helper()
	if r.Context["os"] != runtime.GOOS || r.Context["arch"] != runtime.GOARCH {
		t.Fatalf("os/arch = %q/%q", r.Context["os"], r.Context["arch"])
	}
	ctx := maps.Clone(r.Context)
	delete(ctx, "os")
	delete(ctx, "arch")
	r.Context = ctx
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range forms(base) {
		b = bytes.ReplaceAll(b, []byte(p), []byte("<tmp>"))
	}

	return append(b, '\n')
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".json")
	if *update {
		write(t, path, string(got))

		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden file:\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

func TestCollectGolden(t *testing.T) {
	hermeticGit(t)
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			base := t.TempDir()
			dir, home := sc.build(t, base)

			without := collectFor(dir, home, false)
			checkGolden(t, sc.name+"-nogit", normalise(t, without, base))
			if _, ok := without.Context["git_dirty"]; ok {
				t.Fatal("git_dirty without git")
			}

			t.Run("git", func(t *testing.T) {
				requireGit(t)
				with := collectFor(dir, home, true)
				checkGolden(t, sc.name+"-git", normalise(t, with, base))

				a, b := maps.Clone(with.Context), maps.Clone(without.Context)
				delete(a, "git_dirty")
				if !maps.Equal(a, b) || with.Project != without.Project {
					t.Fatalf("with git and without disagree:\nwith    %v %v\nwithout %v %v", with.Project, a, without.Project, b)
				}
				_, inRepo := with.Context["repo_root"]
				if _, ok := with.Context["git_dirty"]; ok != inRepo {
					t.Fatalf("git_dirty present = %v in a repository = %v", ok, inRepo)
				}
			})
		})
	}
}

// TestTagsAgree compares the full tag lists, not only the first: loose,
// packed, peeled from packed-refs, peeled from a loose tag object, and a
// loose ref overriding a packed one.
func TestTagsAgree(t *testing.T) {
	hermeticGit(t)
	repo := filepath.Join(t.TempDir(), "repo")
	copyFixture(t, repo)
	r, ok := findRepo(repo)
	if !ok {
		t.Fatal("fixture not found")
	}
	want := []string{"old", "v1.0.0", "v2", "zz-light"}
	if got := newFileSource(r).tags(headSHA); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files: %v, want %v", got, want)
	}
	g := gitSource{git: requireGit(t), dir: repo, timeout: DefaultGitTimeout}
	if !g.probe(r) {
		t.Fatal("git refused the fixture")
	}
	if got := g.tags(headSHA); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("git: %v, want %v", got, want)
	}
}

// TestGitRefusedFallsBack: git on PATH that fails for the whole repository
// (a safe.directory refusal looks like this) still yields the keys the files
// provide.
func TestGitRefusedFallsBack(t *testing.T) {
	hermeticGit(t)
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	copyFixture(t, repo)
	fake := fakeGit(t, "echo 'fatal: detected dubious ownership' >&2; exit 128\n")
	got := Collect(Options{Dir: repo, Home: base, Getenv: envMap(nil), Hostname: fixedHost,
		LookPath: func(string) (string, error) { return fake, nil }})
	want := collectFor(repo, base, false)
	if !maps.Equal(got.Context, want.Context) || got.Context["git_commit"] != headSHA {
		t.Fatalf("got %v, want %v", got.Context, want.Context)
	}
}
