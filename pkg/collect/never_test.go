package collect

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNeverCollected plants canaries everywhere the collector must not look
// and checks none reaches the result.
func TestNeverCollected(t *testing.T) {
	hermeticGit(t)
	const (
		user     = "canaryuser7f3a"
		fileText = "canary-filetext-51b2"
		untrack  = "canary-untracked-a9d0"
		argv     = "canary-argv-e3c4"
	)
	home := filepath.Join(t.TempDir(), user)
	repo := filepath.Join(home, "src", "repo")
	copyFixture(t, repo)
	write(t, filepath.Join(repo, "README.md"), fileText+"\n")
	write(t, filepath.Join(repo, "notes.txt"), untrack+"\n")

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not on PATH")
	}
	child := exec.Command(sh, "-c", "sleep 60", argv)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })

	env := hostileEnv()
	env["USER"], env["LOGNAME"], env["HOME"] = user, user, home
	canaries := []string{user, fileText, untrack, argv, "user:tok", "tok@", "x=1", "frag"}
	for name, v := range env {
		if !allowedEnv[name] && v != user && v != home {
			canaries = append(canaries, v)
		}
	}

	for _, dir := range []string{repo, filepath.Join(repo, "sub")} {
		for _, lp := range []func(string) (string, error){nil, noGit} {
			r := Collect(Options{Dir: dir, Home: home, Getenv: envMap(env), LookPath: lp, Hostname: fixedHost})
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range canaries {
				if strings.Contains(string(b), c) {
					t.Errorf("%q collected: %s", c, b)
				}
			}
			if r.Context["repo_root"] != filepath.Join("~", "src", "repo") {
				t.Errorf("repo_root = %q", r.Context["repo_root"])
			}
		}
	}
}

// TestContextKeysDocumented: every key Collect can produce is a documented
// context property of schemas/envelope.v1.json.
func TestContextKeysDocumented(t *testing.T) {
	hermeticGit(t)
	b, err := os.ReadFile(filepath.Join("..", "..", "schemas", "envelope.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Context struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"context"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	documented := schema.Properties.Context.Properties
	if len(documented) == 0 {
		t.Fatal("no documented context keys")
	}

	repo := filepath.Join(t.TempDir(), "repo")
	copyFixture(t, repo)
	nonCode := map[string]string{"undocumented": "x"}
	for _, k := range NonCodeKeys {
		nonCode[k] = "v-" + k
	}
	seen := map[string]bool{}
	for _, env := range []map[string]string{
		{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "s", "CLAUDE_EFFORT": "e", "AI_AGENT": "a"},
		{"OMP_PROFILE": "p"},
		{"PI_CODING_AGENT": "1", "PI_PROFILE": "p"},
	} {
		for _, lp := range []func(string) (string, error){nil, noGit} {
			r := Collect(Options{
				Dir: repo, Getenv: envMap(env), LookPath: lp, Hostname: fixedHost,
				ClientVersion: "4.0.0", WithCwd: true, Context: nonCode,
			})
			for k := range r.Context {
				seen[k] = true
				if _, ok := documented[k]; !ok {
					t.Errorf("context key %q is not documented in the schema", k)
				}
			}
		}
	}
	for _, k := range []string{"folder", "repo_root", "git_remote", "git_branch", "git_commit", "git_upstream", "git_tag",
		"git_default_branch", "cwd", "os", "arch", "session_id", "agent", "effort", "profile", "client",
		"app", "workspace", "url", "channel", "task_id", "workflow"} {
		if !seen[k] {
			t.Errorf("key %q never produced", k)
		}
	}
}

func TestDropAndCwd(t *testing.T) {
	dir := t.TempDir()
	r := Collect(Options{Dir: dir, LookPath: noGit, Hostname: fixedHost, WithCwd: true,
		Context: map[string]string{"app": "a", "url": ""}, Drop: []string{"os", "app"}})
	if r.Context["cwd"] != dir || r.Context["os"] != "" || r.Context["app"] != "" || r.Machine != "testhost" {
		t.Fatalf("%+v", r)
	}
	if _, ok := r.Context["url"]; ok {
		t.Fatal("empty value sent")
	}
	r = Collect(Options{Dir: dir, LookPath: noGit, Hostname: func() (string, error) { return "", errNoHost }})
	if _, ok := r.Context["cwd"]; ok || r.Machine != "" || r.Project != filepath.Base(dir) {
		t.Fatalf("%+v", r)
	}
}

// TestSourceScan keeps the package free of network, user-database, process
// and environment-listing access.
func TestSourceScan(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "net" || strings.HasPrefix(path, "net/") || path == "os/user" {
				t.Errorf("%s imports %s", f, path)
			}
		}
		for _, bad := range []string{`"/proc`, `"/sys`, "os.Environ"} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s contains %s", f, bad)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no source files scanned")
	}
}

func TestScrubRemote(t *testing.T) {
	tests := map[string]string{
		"https://user:tok@example.com/org/r.git?x=1#frag": "https://example.com/org/r.git",
		"https://tok@example.com/org/r":                   "https://example.com/org/r",
		"ssh://git@example.com:22/org/r.git":              "ssh://example.com:22/org/r.git",
		"https://example.com":                             "https://example.com",
		"git@github.com:org/r.git":                        "github.com:org/r.git",
		"github.com:org/r.git":                            "github.com:org/r.git",
		"/srv/git/r.git":                                  "/srv/git/r.git",
		"file:///srv/git/r.git":                           "file:///srv/git/r.git",
		"a/b@c:d":                                         "a/b@c:d",
	}
	for in, want := range tests {
		if got := ScrubRemote(in); got != want {
			t.Errorf("ScrubRemote(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"https://example.com/org/fixture.git": "fixture", "git@h:org/r": "r", "h:r.git": "r", "": "",
	} {
		if got := repoName(in); got != want {
			t.Errorf("repoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseConfigValue(t *testing.T) {
	for in, want := range map[string]string{
		` origin `:                   "origin",
		`"a#b" # comment`:            "a#b",
		`a ; comment`:                "a",
		`"say \"hi\"\t"`:             "say \"hi\"\t",
		`inner  space kept`:          "inner  space kept",
		`"https://u:t@h/p?x=1#frag"`: "https://u:t@h/p?x=1#frag",
	} {
		if got := parseValue(in); got != want {
			t.Errorf("parseValue(%q) = %q, want %q", in, got, want)
		}
	}
	if got := configKey("Remote.Origin.URL"); got != "remote.Origin.url" {
		t.Errorf("configKey = %q", got)
	}
}
