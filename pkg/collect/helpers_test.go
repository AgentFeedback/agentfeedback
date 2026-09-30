package collect

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// headSHA is the fixture's HEAD commit (testdata/make-fixture.sh).
const headSHA = "eca957ddd6a6f8f79aae4152adfade992df6ebe4"

// hermeticGit keeps the caller's git configuration and repository variables
// out of every git command a test runs.
func hermeticGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_CEILING_DIRECTORIES"} {
		if old, ok := os.LookupEnv(name); ok {
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Setenv(name, old) })
		}
	}
}

// requireGit returns git's path or skips the test.
func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}

	return path
}

// noGit is a LookPath that finds nothing.
func noGit(string) (string, error) { return "", exec.ErrNotFound }

// fixedHost is a Hostname that returns a fixed FQDN.
func fixedHost() (string, error) { return "testhost.example.com", nil }

// copyFixture copies testdata/repo to dst and renames dotgit to .git.
func copyFixture(t *testing.T, dst string) {
	t.Helper()
	copyTree(t, filepath.Join("testdata", "repo"), dst)
	if err := os.Rename(filepath.Join(dst, "dotgit"), filepath.Join(dst, ".git")); err != nil {
		t.Fatal(err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}

// envMap is a Getenv backed by a map.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var errNoHost = errors.New("no hostname")

// fakeGit writes an executable shell script standing in for git and returns
// its path; the script body receives git's arguments.
func fakeGit(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for git")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not on PATH")
	}
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte("#!"+sh+"\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}
