package collect

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// DefaultGitTimeout bounds each git command when Options.GitTimeout is zero.
const DefaultGitTimeout = 2 * time.Second

// repo is a repository found by walking up from a directory.
type repo struct {
	root      string // work tree top level: the directory holding .git
	gitDir    string // per-worktree git directory: HEAD
	commonDir string // refs, packed-refs, config and objects
}

// findRepo walks up from dir to the first .git: a directory, or a file
// "gitdir: <path>" as linked worktrees and submodules write. A candidate
// without HEAD is not a git directory and the walk goes on.
func findRepo(dir string) (repo, bool) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return repo{}, false
	}
	for d := dir; ; {
		if gitDir, ok := gitDirAt(filepath.Join(d, ".git")); ok {
			r := repo{root: d, gitDir: gitDir, commonDir: gitDir}
			if b, err := readMeta(filepath.Join(gitDir, "commondir"), maxMetaBytes, false); err == nil {
				c := strings.TrimSpace(string(b))
				if !filepath.IsAbs(c) {
					c = filepath.Join(gitDir, c)
				}
				r.commonDir = filepath.Clean(c)
			}

			return r, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return repo{}, false
		}
		d = parent
	}
}

// gitDirAt resolves a .git entry to its git directory.
func gitDirAt(p string) (string, bool) {
	info, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	gitDir := p
	if info.Mode().IsRegular() {
		b, err := readMeta(p, maxMetaBytes, false)
		if err != nil {
			return "", false
		}
		line, _, _ := strings.Cut(string(b), "\n")
		target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
		if !ok {
			return "", false
		}
		gitDir = strings.TrimSpace(target)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(filepath.Dir(p), gitDir)
		}
	} else if !info.IsDir() {
		return "", false
	}
	gitDir = filepath.Clean(gitDir)
	if info, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil || !info.Mode().IsRegular() {
		return "", false
	}

	return gitDir, true
}

// source answers the repository questions the project group asks. gitSource
// asks git; fileSource parses the files. Both must give the same answers.
type source interface {
	// head returns the current branch ("" when detached) and HEAD's commit
	// ("" before the first commit).
	head() (branch, commit string)
	// get returns the repository config value of a key such as
	// remote.origin.url, "" when unset.
	get(key string) string
	// upstream returns the branch's upstream as <remote>/<branch>.
	upstream(branch string) string
	// tags returns the short names of the tags pointing at commit, lightweight
	// and annotated (peeled), sorted.
	tags(commit string) []string
	// defaultBranch returns the branch refs/remotes/<remote>/HEAD points at,
	// without the "<remote>/" prefix.
	defaultBranch(remote string) string
}

// gitSource runs read-only git commands in dir.
type gitSource struct {
	git     string
	dir     string
	timeout time.Duration
	// dead is set when a command hits its timeout: git is not asked again
	// during this collection.
	dead bool
}

// errGitDead is returned once a command has timed out.
var errGitDead = errors.New("git timed out earlier in this collection")

// gitWaitDelay bounds the wait for git's output pipes after a timeout kill,
// in case a child of git still holds them.
const gitWaitDelay = 250 * time.Millisecond

// run executes git -c core.fsmonitor=false <args> and returns its stdout
// without the trailing newline. Every GIT_* variable of the process is
// dropped, so none can point git at another repository, config or trace
// file; the fixed variables keep git from taking locks, prompting, or
// translating its output.
func (g *gitSource) run(args ...string) (string, error) {
	if g.dead {
		return "", errGitDead
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.git, append([]string{"-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = g.dir
	cmd.WaitDelay = gitWaitDelay
	env := slices.DeleteFunc(cmd.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })
	cmd.Env = append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdin = nil
	cmd.Stderr = nil
	out, err := cmd.Output()
	if ctx.Err() != nil {
		g.dead = true

		return "", errGitDead
	}
	if err != nil {
		return "", err
	}

	return strings.TrimRight(string(out), "\n"), nil
}

// probe reports whether git accepts the repository at all and sees the same
// one the walk found: a safe.directory refusal or any other disagreement
// sends every key to fileSource.
func (g *gitSource) probe(r repo) bool {
	out, err := g.run("rev-parse", "--absolute-git-dir", "--show-toplevel")
	if err != nil {
		return false
	}
	gitDir, top, ok := strings.Cut(out, "\n")

	return ok && samePath(gitDir, r.gitDir) && samePath(top, r.root)
}

// samePath compares two paths as written and with symlinks resolved.
func samePath(a, b string) bool {
	for _, af := range forms(a) {
		for _, bf := range forms(b) {
			if within(af, bf, foldCase) && within(bf, af, foldCase) {
				return true
			}
		}
	}

	return false
}

// Full ref names are asked for and the prefixes stripped here: --short
// would turn a branch that shares its name with a tag into "heads/<name>".

func (g *gitSource) head() (string, string) {
	branch := ""
	if full, err := g.run("symbolic-ref", "-q", "HEAD"); err == nil {
		branch, _ = strings.CutPrefix(full, "refs/heads/")
		if branch == full {
			branch = ""
		}
	}
	commit, _ := g.run("rev-parse", "-q", "--verify", "HEAD^{commit}")

	return branch, commit
}

func (g *gitSource) get(key string) string {
	v, _ := g.run("config", "--local", "--no-includes", "--get", key)

	return v
}

func (g *gitSource) upstream(string) string {
	full, err := g.run("rev-parse", "--symbolic-full-name", "@{upstream}")
	if err != nil {
		return ""
	}
	for _, prefix := range []string{"refs/remotes/", "refs/heads/"} {
		if short, ok := strings.CutPrefix(full, prefix); ok {
			return short
		}
	}

	return ""
}

func (g *gitSource) tags(string) []string {
	out, err := g.run("for-each-ref", "--points-at", "HEAD", "--format=%(refname)", "refs/tags")
	if err != nil || out == "" {
		return nil
	}
	var tags []string
	for full := range strings.SplitSeq(out, "\n") {
		if short, ok := strings.CutPrefix(full, "refs/tags/"); ok && short != "" {
			tags = append(tags, short)
		}
	}
	slices.Sort(tags)

	return tags
}

func (g *gitSource) defaultBranch(remote string) string {
	prefix := "refs/remotes/" + remote + "/"
	if !validRefName(prefix + "HEAD") {
		return ""
	}
	full, err := g.run("symbolic-ref", "-q", prefix+"HEAD")
	if err != nil {
		return ""
	}
	branch, ok := strings.CutPrefix(full, prefix)
	if !ok {
		return ""
	}

	return branch
}

// dirty reports "true" or "false" from git status, untracked files included;
// "" when git fails.
//
// git status can run programs named by repository config: core.fsmonitor
// (switched off with -c) and the clean/process filters .gitattributes
// assigns (filter.<driver>.*). A repository unpacked from an archive brings
// its own .git/config, so the caller asks only when the parsed local config
// has no filter.* key and no include/includeIf that could add one.
func (g *gitSource) dirty() string {
	out, err := g.run("status", "--porcelain", "--untracked-files=normal")
	switch {
	case err != nil:
		return ""
	case out != "":
		return "true"
	}

	return "false"
}

// fallbackSource asks git while it answers in time and fileSource once a git
// command has timed out.
type fallbackSource struct {
	g *gitSource
	f *fileSource
}

func (s fallbackSource) head() (string, string) {
	if b, c := s.g.head(); !s.g.dead {
		return b, c
	}

	return s.f.head()
}

func (s fallbackSource) get(key string) string {
	if v := s.g.get(key); !s.g.dead {
		return v
	}

	return s.f.get(key)
}

func (s fallbackSource) upstream(branch string) string {
	if v := s.g.upstream(branch); !s.g.dead {
		return v
	}

	return s.f.upstream(branch)
}

func (s fallbackSource) tags(commit string) []string {
	if v := s.g.tags(commit); !s.g.dead {
		return v
	}

	return s.f.tags(commit)
}

func (s fallbackSource) defaultBranch(remote string) string {
	if v := s.g.defaultBranch(remote); !s.g.dead {
		return v
	}

	return s.f.defaultBranch(remote)
}

// fileSource reads the repository's files directly.
type fileSource struct {
	r      repo
	config map[string]string
	// configOK is false when a config file exists but could not be read
	// whole; nothing is then known about the filters it may define.
	configOK bool
	packed   map[string]packedRef
}

type packedRef struct {
	sha, peeled string
}

// maxRefDepth bounds symbolic-ref and tag-peeling chains.
const maxRefDepth = 5

func newFileSource(r repo) *fileSource {
	f := &fileSource{r: r, configOK: true, packed: parsePackedRefs(filepath.Join(r.commonDir, "packed-refs"))}
	f.config, f.configOK = parseConfig(filepath.Join(r.commonDir, "config"))
	if wt, ok := parseConfig(filepath.Join(r.gitDir, "config.worktree")); ok {
		for k, v := range wt {
			f.config[k] = v
		}
	} else {
		f.configOK = false
	}

	return f
}

// statusSafe reports whether the repository config is known to name no
// filter driver and to include no other file, so git status runs no
// program the repository chose.
func (f *fileSource) statusSafe() bool {
	if !f.configOK {
		return false
	}
	for k := range f.config {
		if strings.HasPrefix(k, "filter.") || strings.HasPrefix(k, "include.") || strings.HasPrefix(k, "includeif.") {
			return false
		}
	}

	return true
}

// validRefName reports whether a ref name is safe to use as a path under
// the git directory: HEAD, or a relative slash-separated name under refs/
// with no empty, "." or ".." component, no backslash and no NUL.
func validRefName(name string) bool {
	if name == "HEAD" {
		return true
	}
	if !strings.HasPrefix(name, "refs/") || strings.ContainsAny(name, "\\\x00") {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}

	return true
}

// readRef returns the raw value of a ref: "ref: <target>" or a SHA. HEAD is
// per worktree; every other ref lives in the common directory, loose files
// overriding packed-refs. An invalid name is absent.
func (f *fileSource) readRef(name string) (string, bool) {
	if !validRefName(name) {
		return "", false
	}
	dir := f.r.commonDir
	if name == "HEAD" {
		dir = f.r.gitDir
	}
	if b, err := readMeta(filepath.Join(dir, filepath.FromSlash(name)), maxMetaBytes, false); err == nil {
		return strings.TrimSpace(string(b)), true
	}
	if p, ok := f.packed[name]; ok {
		return p.sha, true
	}

	return "", false
}

// resolve follows symbolic refs to a SHA.
func (f *fileSource) resolve(name string) string {
	for range maxRefDepth {
		v, ok := f.readRef(name)
		if !ok {
			return ""
		}
		target, sym := strings.CutPrefix(v, "ref:")
		if !sym {
			if isSHA(v) {
				return v
			}

			return ""
		}
		name = strings.TrimSpace(target)
	}

	return ""
}

func (f *fileSource) head() (string, string) {
	v, ok := f.readRef("HEAD")
	if !ok {
		return "", ""
	}
	branch := ""
	if target, sym := strings.CutPrefix(v, "ref:"); sym {
		target = strings.TrimSpace(target)
		if validRefName(target) {
			branch, _ = strings.CutPrefix(target, "refs/heads/")
			if branch == target {
				branch = ""
			}
		}
	}

	return branch, f.resolve("HEAD")
}

func (f *fileSource) get(key string) string {
	return f.config[configKey(key)]
}

// upstream mirrors @{upstream}: the configured upstream counts only when its
// tracking ref exists.
func (f *fileSource) upstream(branch string) string {
	remote := f.get("branch." + branch + ".remote")
	merged, ok := strings.CutPrefix(f.get("branch."+branch+".merge"), "refs/heads/")
	if remote == "" || !ok || merged == "" {
		return ""
	}
	short, tracking := remote+"/"+merged, "refs/remotes/"+remote+"/"+merged
	if remote == "." {
		short, tracking = merged, "refs/heads/"+merged
	}
	if !validRefName(tracking) || f.resolve(tracking) == "" {
		return ""
	}

	return short
}

// tags lists the tags whose ref, peeled, is commit. Known limitation: an
// annotated tag whose ref is loose (so there is no peeled line) and whose
// tag object is only in a pack is not seen; git sees it.
func (f *fileSource) tags(commit string) []string {
	if commit == "" {
		return nil
	}
	values := map[string]string{} // short name -> SHA the ref holds
	peeled := map[string]string{}
	for name, p := range f.packed {
		if short, ok := strings.CutPrefix(name, "refs/tags/"); ok {
			values[short] = p.sha
			if p.peeled != "" {
				peeled[short] = p.peeled
			}
		}
	}
	tagsDir := filepath.Join(f.r.commonDir, "refs", "tags")
	_ = filepath.WalkDir(tagsDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := readMeta(p, maxMetaBytes, false)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(tagsDir, p)
		if err != nil {
			return nil
		}
		short := filepath.ToSlash(rel)
		values[short] = strings.TrimSpace(string(b))
		delete(peeled, short) // the loose ref overrides the packed one
		return nil
	})

	var out []string
	for short, sha := range values {
		target, ok := peeled[short]
		if !ok {
			target = f.peel(sha)
		}
		if target == commit {
			out = append(out, short)
		}
	}
	slices.Sort(out)

	return out
}

// peel follows tag objects to the object they tag. A SHA that is not a
// loose tag object is returned as is (a commit, or an object in a pack,
// which is then compared as is); a tag object that cannot be read peels to
// "".
func (f *fileSource) peel(sha string) string {
	for range maxRefDepth {
		if !isSHA(sha) {
			return ""
		}
		kind, body, ok := f.readLooseObject(sha)
		if !ok || kind != "tag" {
			return sha
		}
		object, typ := tagTarget(body)
		if object == "" {
			return ""
		}
		if typ != "tag" {
			return object
		}
		sha = object
	}

	return ""
}

// readLooseObject inflates objects/xx/yyyy and splits the "<type> <size>\x00"
// header from the body. It inflates at most a bounded prefix: only tag
// headers are ever parsed.
func (f *fileSource) readLooseObject(sha string) (kind string, body []byte, ok bool) {
	raw, err := readMeta(filepath.Join(f.r.commonDir, "objects", sha[:2], sha[2:]), maxMetaBytes, false)
	if err != nil {
		return "", nil, false
	}
	z, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", nil, false
	}
	defer func() { _ = z.Close() }()
	data, err := io.ReadAll(io.LimitReader(z, 64<<10))
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", nil, false
	}
	header, body, found := bytes.Cut(data, []byte{0})
	if !found {
		return "", nil, false
	}
	kind, _, _ = strings.Cut(string(header), " ")

	return kind, body, true
}

// tagTarget reads the object and type lines of a tag object's body.
func tagTarget(body []byte) (object, typ string) {
	for line := range strings.SplitSeq(string(body), "\n") {
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "object "); ok {
			object = v
		} else if v, ok := strings.CutPrefix(line, "type "); ok {
			typ = v
		}
	}
	if !isSHA(object) {
		return "", ""
	}

	return object, typ
}

func (f *fileSource) defaultBranch(remote string) string {
	prefix := "refs/remotes/" + remote + "/"
	v, ok := f.readRef(prefix + "HEAD")
	if !ok {
		return ""
	}
	target, ok := strings.CutPrefix(v, "ref:")
	if !ok {
		return ""
	}
	branch, ok := strings.CutPrefix(strings.TrimSpace(target), prefix)
	if !ok || !validRefName(prefix+branch) {
		return ""
	}

	return branch
}

// isSHA reports whether s is a full lowercase hex object id (SHA-1 or
// SHA-256).
func isSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// parsePackedRefs reads packed-refs: "<sha> <name>" lines, each optionally
// followed by a "^<sha>" line with the peeled object of an annotated tag.
func parsePackedRefs(path string) map[string]packedRef {
	refs := map[string]packedRef{}
	b, err := readMeta(path, maxPackedBytes, false)
	if err != nil {
		return refs
	}
	last := ""
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "^"):
			if p, ok := refs[last]; ok && isSHA(line[1:]) {
				p.peeled = line[1:]
				refs[last] = p
			}
		default:
			sha, name, ok := strings.Cut(line, " ")
			if ok && isSHA(sha) && validRefName(name) {
				refs[name] = packedRef{sha: sha}
				last = name
			}
		}
	}

	return refs
}

// configKey normalises a dotted key the way git compares them: section and
// variable name without case, the subsection exactly.
func configKey(key string) string {
	first := strings.Index(key, ".")
	last := strings.LastIndex(key, ".")
	if first < 0 || first == last {
		return strings.ToLower(key)
	}

	return strings.ToLower(key[:first]) + "." + key[first+1:last] + "." + strings.ToLower(key[last+1:])
}

// parseConfig reads a git config file into normalised key -> last value.
// It covers what git writes (sections, quoted subsections, a key on the
// header line, quoted values with escapes, comments, backslash-newline
// continuations); include directives are not followed. ok is false when the
// file exists but cannot be read or scanned whole; the map is then empty. A
// missing file is ok and empty.
func parseConfig(path string) (cfg map[string]string, ok bool) {
	cfg = map[string]string{}
	data, err := readMeta(path, maxMetaBytes, false)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, true
	}
	if err != nil {
		return cfg, false
	}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), maxMetaBytes)
	pending := ""
	for sc.Scan() {
		raw := pending + sc.Text()
		pending = ""
		if strings.HasSuffix(raw, `\`) && !strings.HasSuffix(raw, `\\`) {
			pending = strings.TrimSuffix(raw, `\`)
			continue
		}
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := headerEnd(line)
			if end < 0 {
				section = ""
				continue
			}
			section = parseSection(line[1:end])
			line = strings.TrimSpace(line[end+1:])
			if line == "" || line[0] == '#' || line[0] == ';' {
				continue
			}
		}
		if section == "" {
			continue
		}
		name, value, hasValue := strings.Cut(line, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if !hasValue {
			cfg[section+"."+name] = "true"
			continue
		}
		cfg[section+"."+name] = parseValue(value)
	}
	if sc.Err() != nil {
		return map[string]string{}, false
	}

	return cfg, true
}

// headerEnd returns the index of the "]" closing a section header, skipping
// a quoted subsection; -1 when there is none.
func headerEnd(line string) int {
	inQuote := false
	for i := 1; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && inQuote:
			i++
		case c == '"':
			inQuote = !inQuote
		case c == ']' && !inQuote:
			return i
		}
	}

	return -1
}

// parseSection turns `remote "origin"` into remote.origin and the legacy
// `remote.origin` into remote.origin.
func parseSection(s string) string {
	s = strings.TrimSpace(s)
	name, sub, quoted := strings.Cut(s, " ")
	if !quoted {
		if n, legacy, ok := strings.Cut(s, "."); ok {
			return strings.ToLower(n) + "." + strings.ToLower(legacy)
		}

		return strings.ToLower(s)
	}
	sub = strings.TrimSpace(sub)
	sub = strings.TrimSuffix(strings.TrimPrefix(sub, `"`), `"`)
	sub = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(sub)

	return strings.ToLower(name) + "." + sub
}

// parseValue unquotes a config value and strips a trailing comment.
func parseValue(v string) string {
	var b strings.Builder
	inQuote := false
	pendingSpace := ""
	v = strings.TrimSpace(v)
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '\\' && i+1 < len(v):
			i++
			b.WriteString(pendingSpace)
			pendingSpace = ""
			switch v[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			default:
				b.WriteByte(v[i])
			}
		case c == '"':
			inQuote = !inQuote
		case !inQuote && (c == '#' || c == ';'):
			return b.String()
		case !inQuote && (c == ' ' || c == '\t'):
			// Unquoted inner whitespace is kept; trailing whitespace is not.
			pendingSpace += string(c)
		default:
			b.WriteString(pendingSpace)
			pendingSpace = ""
			b.WriteByte(c)
		}
	}

	return b.String()
}
