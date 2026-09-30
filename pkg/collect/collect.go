package collect

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Options configures one collection. Only Dir is required.
type Options struct {
	// Dir is the working directory the submission is made from.
	Dir string
	// Home is the home directory; a repository beneath it is reported as
	// "~/…". Empty means os.UserHomeDir().
	Home string
	// Getenv looks up one environment variable by name. The package never
	// lists the environment; nil reads nothing.
	Getenv func(string) string
	// LookPath finds git; nil means exec.LookPath. A failing LookPath makes
	// every repository key come from the files.
	LookPath func(string) (string, error)
	// Hostname reports the machine name; nil means os.Hostname.
	Hostname func() (string, error)
	// ClientVersion fills context.client as agentfeedback/<version>.
	ClientVersion string
	// WithCwd adds the full working directory as context.cwd.
	WithCwd bool
	// Context carries the non-code keys given by flags or config; keys other
	// than NonCodeKeys are ignored.
	Context map[string]string
	// Drop lists context keys removed after everything else.
	Drop []string
	// GitTimeout bounds each git command; zero means DefaultGitTimeout.
	GitTimeout time.Duration
}

// Result is what Collect found: the envelope defaults and the context map.
// Empty values are never present in Context.
type Result struct {
	Project string            `json:"project,omitempty"`
	Machine string            `json:"machine,omitempty"`
	Harness string            `json:"harness,omitempty"`
	Model   string            `json:"model,omitempty"`
	Context map[string]string `json:"context"`
}

// NonCodeKeys are the context keys a caller may supply for work outside a
// repository.
var NonCodeKeys = []string{"app", "workspace", "url", "channel", "task_id", "workflow"}

// envVar maps one allow-listed variable to the context key it fills.
type envVar struct {
	name, key string
}

// harness is one entry of the detection table.
type harness struct {
	name string
	// markers: the harness is detected when any of them is non-empty.
	markers []string
	// vars are read only when this harness is the detected one.
	vars []envVar
	// model, when set, names the variable holding the model id.
	model string
}

// harnesses is the allow-list, in detection order: the first harness with a
// non-empty marker wins. Nested launches carry the outer harness's markers
// too, so inner harnesses are listed before claude-code. PI_MODEL and
// OPENCODE_MODEL are legacy wrapper variables, checked last. No prefix is ever
// sniffed: shell profiles export keys such as OPENCODE_API_KEY everywhere.
var harnesses = []harness{
	{name: "omp", markers: []string{"PI_CODING_AGENT_DIR", "OMP_PROFILE"}, vars: []envVar{{"OMP_PROFILE", "profile"}}},
	{name: "pi", markers: []string{"PI_CODING_AGENT"}, vars: []envVar{{"PI_PROFILE", "profile"}}, model: "PI_MODEL"},
	{name: "opencode", markers: []string{"OPENCODE"}, model: "OPENCODE_MODEL"},
	{name: "codex", markers: []string{"CODEX_SANDBOX"}},
	{name: "claude-code", markers: []string{"CLAUDECODE"}, vars: []envVar{{"CLAUDE_CODE_SESSION_ID", "session_id"}, {"CLAUDE_EFFORT", "effort"}}},
	{name: "pi", markers: []string{"PI_MODEL"}, vars: []envVar{{"PI_PROFILE", "profile"}}, model: "PI_MODEL"},
	{name: "opencode", markers: []string{"OPENCODE_MODEL"}, model: "OPENCODE_MODEL"},
}

// commonVars are read whatever the harness; they are applied after the
// harness's own, so AGENT_FEEDBACK_SESSION_ID overrides a harness session id.
var commonVars = []envVar{{"AI_AGENT", "agent"}, {"AGENT_FEEDBACK_SESSION_ID", "session_id"}}

// Collect gathers the project, machine and harness groups for opts.Dir. It
// never fails: a source that cannot be read only omits its keys.
func Collect(opts Options) Result {
	res := Result{Context: map[string]string{}}
	set := func(key, value string) {
		if value != "" {
			res.Context[key] = value
		}
	}
	dir := opts.Dir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if opts.Home == "" {
		opts.Home, _ = os.UserHomeDir()
	}

	// The home directory's basename is the username.
	folder := filepath.Base(dir)
	if opts.Home != "" && samePath(dir, opts.Home) {
		folder = "~"
	}
	set("folder", folder)
	res.Project = folder
	if r, ok := findRepo(dir); ok {
		if name := collectRepo(r, dir, opts, set); name != "" {
			res.Project = name
		}
	}

	set("os", runtime.GOOS)
	set("arch", runtime.GOARCH)
	if opts.ClientVersion != "" {
		set("client", "agentfeedback/"+opts.ClientVersion)
	}
	hostname := opts.Hostname
	if hostname == nil {
		hostname = os.Hostname
	}
	if h, err := hostname(); err == nil {
		res.Machine, _, _ = strings.Cut(h, ".")
	}

	res.Harness, res.Model = collectHarness(opts.Getenv, set)

	if opts.WithCwd {
		set("cwd", dir)
	}
	for _, k := range NonCodeKeys {
		set(k, opts.Context[k])
	}
	for _, k := range opts.Drop {
		delete(res.Context, k)
	}

	return res
}

// collectRepo fills the project group and returns the repository name taken
// from the remote, "" when there is none.
func collectRepo(r repo, dir string, opts Options, set func(string, string)) string {
	set("repo_root", tildePath(r.root, opts.Home))

	files := newFileSource(r)
	var src source = files
	var git *gitSource
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if path, err := lookPath("git"); err == nil {
		timeout := opts.GitTimeout
		if timeout <= 0 {
			timeout = DefaultGitTimeout
		}
		g := &gitSource{git: path, dir: dir, timeout: timeout}
		if g.probe(r) {
			git, src = g, fallbackSource{g: g, f: files}
		}
	}

	branch, commit := src.head()
	remote := ""
	if src.get("remote.origin.url") != "" {
		remote = "origin"
	} else if branch != "" {
		if br := src.get("branch." + branch + ".remote"); br != "" && br != "." {
			remote = br
		}
	}
	url := ""
	if remote != "" {
		url = ScrubRemote(src.get("remote." + remote + ".url"))
		set("git_remote", localRemote(url, opts.Home))
		set("git_default_branch", src.defaultBranch(remote))
	}
	set("git_branch", branch)
	set("git_commit", commit)
	if branch != "" {
		set("git_upstream", src.upstream(branch))
	}
	if tags := src.tags(commit); commit != "" && len(tags) > 0 {
		set("git_tag", tags[0])
	}
	// git status only when the repository config cannot make it run a
	// program (see gitSource.dirty), and only while git answers in time.
	if git != nil && !git.dead && files.statusSafe() {
		set("git_dirty", git.dirty())
	}

	return repoName(url)
}

// collectHarness detects the harness from the allow-list and fills its keys
// and the common ones. It looks up no variable outside the table.
func collectHarness(getenv func(string) string, set func(string, string)) (name, model string) {
	if getenv == nil {
		return "", ""
	}
	for _, h := range harnesses {
		detected := false
		for _, m := range h.markers {
			if getenv(m) != "" {
				detected = true
				break
			}
		}
		if !detected {
			continue
		}
		for _, v := range h.vars {
			set(v.key, getenv(v.name))
		}
		if h.model != "" {
			model = getenv(h.model)
		}
		name = h.name

		break
	}
	for _, v := range commonVars {
		set(v.key, getenv(v.name))
	}

	return name, model
}

// ScrubRemote strips what is not repository identity from a remote URL.
// For a scheme:// URL the userinfo is cut first, through the last "@" of the
// authority (the text up to the first "/" after "://"), so a "#" or "?" in a
// password cannot hide it; then the ?query and #fragment go. An SCP-style
// user@host:path loses its user@.
func ScrubRemote(remote string) string {
	if scheme, rest, ok := strings.Cut(remote, "://"); ok {
		authority, tail, hasPath := strings.Cut(rest, "/")
		if i := strings.LastIndex(authority, "@"); i >= 0 {
			authority = authority[i+1:]
		}
		if hasPath {
			tail = "/" + tail
		}
		remote = scheme + "://" + authority + tail
		remote, _, _ = strings.Cut(remote, "#")
		remote, _, _ = strings.Cut(remote, "?")

		return remote
	}
	remote, _, _ = strings.Cut(remote, "#")
	remote, _, _ = strings.Cut(remote, "?")
	// SCP style: user@host:path, where the user has no / @ : and the host no / :.
	if at := strings.Index(remote, "@"); at > 0 {
		user, rest := remote[:at], remote[at+1:]
		host, _, hasColon := strings.Cut(rest, ":")
		if hasColon && host != "" && !strings.ContainsAny(user, "/@:") && !strings.ContainsAny(host, "/") {
			return rest
		}
	}

	return remote
}

// localRemote reports a local remote (an absolute path or a file:// URL)
// relative to home exactly as repo_root is; other remotes are unchanged.
func localRemote(url, home string) string {
	if p, ok := strings.CutPrefix(url, "file://"); ok && filepath.IsAbs(p) {
		return "file://" + tildePath(p, home)
	}
	if filepath.IsAbs(url) {
		return tildePath(url, home)
	}

	return url
}

// repoName is the last path segment of a remote without ".git".
func repoName(url string) string {
	url = strings.TrimRight(url, "/")
	if i := strings.LastIndexAny(url, "/:"); i >= 0 {
		url = url[i+1:]
	}

	return strings.TrimSuffix(url, ".git")
}
