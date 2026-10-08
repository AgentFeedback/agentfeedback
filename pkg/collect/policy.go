package collect

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Policy is the user config's [collect] table.
type Policy struct {
	// DenyPaths: no collection in these directories or beneath them.
	DenyPaths []string `toml:"deny_paths"`
	// OptInOnly: collect only beneath one of OptInPaths.
	OptInOnly  bool     `toml:"opt_in_only"`
	OptInPaths []string `toml:"opt_in_paths"`
	// Disabled switches collection off everywhere.
	Disabled bool `toml:"disabled"`
}

// ContextConfig is the user config's [context] table.
type ContextConfig struct {
	// Cwd adds the full working directory as context.cwd.
	Cwd bool `toml:"cwd"`
	// Drop lists context keys never sent.
	Drop []string `toml:"drop"`
	// The non-code keys, sent as given.
	App       string `toml:"app"`
	Workspace string `toml:"workspace"`
	URL       string `toml:"url"`
	Channel   string `toml:"channel"`
	TaskID    string `toml:"task_id"`
	Workflow  string `toml:"workflow"`
}

// Decision is the outcome of Check.
type Decision struct {
	// Disabled means nothing is collected or sent from this directory.
	Disabled bool
	// Reason names the rule that disabled it: disabled, deny_paths,
	// opt_in_only, repo_disabled or repo_deny_paths.
	Reason string
	// Warnings are problems in the settings that were skipped over.
	Warnings []string
	// Drop is the repository file's context.drop; the caller unions it with
	// the user's.
	Drop []string
	// NudgeOff is the repository file's detect.nudge = false: the hook
	// still counts failures but gives no note.
	NudgeOff bool
}

// Decision reasons.
const (
	ReasonDisabled      = "disabled"
	ReasonDenyPaths     = "deny_paths"
	ReasonOptInOnly     = "opt_in_only"
	ReasonRepoDisabled  = "repo_disabled"
	ReasonRepoDenyPaths = "repo_deny_paths"
)

// RepoFile is the repository file Check reads from the repository root.
const RepoFile = ".agentfeedback.toml"

// repoFileKeys are the only keys a repository file may set: each narrows.
var repoFileKeys = []string{"collect.disabled", "collect.deny_paths", "context.drop", "detect.nudge"}

// Check decides whether dir may be collected from under the user policy p and
// the repository file of the repository dir is in. The rules apply in order:
// p.Disabled, p.DenyPaths, p.OptInOnly with p.OptInPaths, then the
// repository file (collect.disabled, collect.deny_paths, context.drop,
// detect.nudge; any other key is ignored with a warning).
//
// An empty home means os.UserHomeDir(); "~" entries cannot be expanded
// without one.
//
// submit and flush (the hook included) call it before anything is sent, and
// the hook command before it gives a note;
// doctor reports the same setting problems through Lint.
func Check(dir, home string, p Policy) Decision {
	d := CheckUser(dir, home, p)
	if d.Disabled {
		return d
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	disable := func(reason string) Decision {
		d.Disabled, d.Reason = true, reason

		return d
	}

	r, ok := findRepo(dir)
	if !ok {
		return d
	}
	rf := readRepoFile(filepath.Join(r.root, RepoFile), r.root)
	d.Warnings = append(d.Warnings, rf.warnings...)
	d.Drop = rf.drop
	d.NudgeOff = rf.nudgeOff
	if rf.disabled {
		return disable(ReasonRepoDisabled)
	}
	if matchesAny(dir, rf.denyPaths) {
		return disable(ReasonRepoDenyPaths)
	}

	return d
}

// CheckUser applies only the user rules of Check (p.Disabled, p.DenyPaths,
// p.OptInOnly with p.OptInPaths) and reads no repository file: the decision
// for a directory that no longer exists, whose repository file cannot be
// read. An empty home means os.UserHomeDir().
func CheckUser(dir, home string, p Policy) Decision {
	var d Decision
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	disable := func(reason string) Decision {
		d.Disabled, d.Reason = true, reason

		return d
	}
	if p.Disabled {
		return disable(ReasonDisabled)
	}
	userPaths := func(key string, entries []string) []string {
		out, warnings := userPathEntries(key, entries, home)
		d.Warnings = append(d.Warnings, warnings...)

		return out
	}
	if matchesAny(dir, userPaths("deny_paths", p.DenyPaths)) {
		return disable(ReasonDenyPaths)
	}
	if p.OptInOnly && !slices.ContainsFunc(userPaths("opt_in_paths", p.OptInPaths), func(e string) bool { return resolvedWithin(dir, e) }) {
		return disable(ReasonOptInOnly)
	}

	return d
}

// UserPaths returns the user policy's deny_paths and opt_in_paths entries
// expanded the way Check expands them: ~/ against home, entries that are
// neither absolute nor ~/ skipped. An empty home means os.UserHomeDir().
func UserPaths(p Policy, home string) (deny, optIn []string) {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	deny, _ = userPathEntries("deny_paths", p.DenyPaths, home)
	optIn, _ = userPathEntries("opt_in_paths", p.OptInPaths, home)

	return deny, optIn
}

// Lint returns every problem in the settings Check would skip over for dir,
// whatever Check decides: the deny_paths and opt_in_paths entries that are
// neither absolute nor ~/, and the problems of the repository file of the
// repository dir is in. An empty home means os.UserHomeDir().
func Lint(dir, home string, p Policy) []string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	_, out := userPathEntries("deny_paths", p.DenyPaths, home)
	_, w := userPathEntries("opt_in_paths", p.OptInPaths, home)
	out = append(out, w...)
	if r, ok := findRepo(dir); ok {
		out = append(out, readRepoFile(filepath.Join(r.root, RepoFile), r.root).warnings...)
	}

	return out
}

// userPathEntries expands the user config's entries under collect.<key>:
// each must be absolute or ~/; any other is skipped with a warning.
func userPathEntries(key string, entries []string, home string) (paths, warnings []string) {
	for _, e := range entries {
		x, ok := expandHome(e, home)
		if !ok || !filepath.IsAbs(x) {
			warnings = append(warnings, fmt.Sprintf("collect.%s: ignoring %q: not an absolute path or ~/ path", key, e))
			continue
		}
		paths = append(paths, x)
	}

	return paths, warnings
}

// matchesAny reports whether dir is within any of the entries, as written or
// with symlinks resolved on either side: the deny test.
func matchesAny(dir string, entries []string) bool {
	return slices.ContainsFunc(entries, func(e string) bool { return withinAny(dir, e) })
}

type repoFile struct {
	disabled  bool
	denyPaths []string
	drop      []string
	nudgeOff  bool
	warnings  []string
}

// readRepoFile reads a repository's .agentfeedback.toml. A missing file is
// no narrowing; an unreadable, unparseable or symlinked one is a warning and
// no narrowing.
func readRepoFile(path, root string) repoFile {
	var rf repoFile
	data, err := readMeta(path, maxMetaBytes, true)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			rf.warnings = append(rf.warnings, fmt.Sprintf("%s: ignoring the file: %s", RepoFile, repoFileProblem(path, err)))
		}

		return rf
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		rf.warnings = append(rf.warnings, fmt.Sprintf("%s: ignoring the file: %v", RepoFile, oneLine(err.Error())))

		return rf
	}
	wrongType := func(key, want string) {
		rf.warnings = append(rf.warnings, fmt.Sprintf("%s: ignoring %q: want %s", RepoFile, key, want))
	}

	for _, l := range leaves(nil, doc) {
		key, v := l.name(), l.value
		switch {
		case slices.Equal(l.path, []string{"collect", "disabled"}):
			b, ok := v.(bool)
			if !ok {
				wrongType(key, "a boolean")
				continue
			}
			rf.disabled = b
		case slices.Equal(l.path, []string{"collect", "deny_paths"}):
			entries, ok := stringList(v)
			if !ok {
				wrongType(key, "an array of strings")
				continue
			}
			for _, e := range entries {
				if strings.HasPrefix(e, "~") {
					rf.warnings = append(rf.warnings, fmt.Sprintf("%s: ignoring %q in collect.deny_paths: ~ is not expanded in a repository file", RepoFile, e))
					continue
				}
				if !filepath.IsAbs(e) {
					e = filepath.Join(root, e)
				}
				rf.denyPaths = append(rf.denyPaths, filepath.Clean(e))
			}
		case slices.Equal(l.path, []string{"context", "drop"}):
			entries, ok := stringList(v)
			if !ok {
				wrongType(key, "an array of strings")
				continue
			}
			rf.drop = entries
		case slices.Equal(l.path, []string{"detect", "nudge"}):
			b, ok := v.(bool)
			if !ok {
				wrongType(key, "a boolean")
				continue
			}
			rf.nudgeOff = !b
		default:
			rf.warnings = append(rf.warnings, fmt.Sprintf("%s: ignoring %q: a repository file may only narrow (%s)",
				RepoFile, key, strings.Join(repoFileKeys, ", ")))
		}
	}

	return rf
}

// repoFileProblem words a read failure without repeating the path.
func repoFileProblem(path string, err error) string {
	if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "it is a symlink"
	}
	if errors.Is(err, errNotRegular) {
		return "not a regular file"
	}

	return oneLine(err.Error())
}

// leaf is one non-table value and the key segments leading to it.
type leaf struct {
	path  []string
	value any
}

// name renders the key in TOML dotted form, quoting a segment that is not a
// bare key.
func (l leaf) name() string {
	parts := make([]string, len(l.path))
	for i, p := range l.path {
		parts[i] = p
		if !bareKey(p) {
			parts[i] = strconv.Quote(p)
		}
	}

	return strings.Join(parts, ".")
}

func bareKey(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}

	return true
}

// leaves lists every non-table value with its key segments, sorted by
// rendered name. An empty table has no leaves; an array of tables is one
// leaf.
func leaves(prefix []string, m map[string]any) []leaf {
	var out []leaf
	for k, v := range m {
		path := append(slices.Clone(prefix), k)
		if sub, ok := v.(map[string]any); ok {
			out = append(out, leaves(path, sub)...)
			continue
		}
		out = append(out, leaf{path, v})
	}
	slices.SortFunc(out, func(a, b leaf) int { return strings.Compare(a.name(), b.name()) })

	return out
}

// stringList accepts an array whose every element is a string.
func stringList(v any) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}

	return out, true
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
