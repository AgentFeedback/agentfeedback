package sessions

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// Skill scopes.
const (
	ScopeProject = "project"
	ScopeGlobal  = "global"
)

// skillFrontMatterMax is the most of a SKILL.md read for its front matter.
const skillFrontMatterMax = 64 << 10

// Skill is one SKILL.md and every harness that reads the directory it lies
// in. Name, Path and Description are scrubbed.
type Skill struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Path        string   `json:"path"`
	Scope       string   `json:"scope"`
	Harnesses   []string `json:"harnesses"`
}

// SkillProblem is a SKILL.md, or a skill directory, that could not be
// read; Path and Reason are scrubbed.
type SkillProblem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Inventory is Skills' result.
type Inventory struct {
	Skills   []Skill        `json:"skills"`
	Problems []SkillProblem `json:"problems"`
}

// SkillsRequest selects the skills to list: those of the projects at
// Projects (absolute paths) and, with Global, the user-level ones.
type SkillsRequest struct {
	Projects []string
	Global   bool
}

// ErrNoSkillScope: neither a project nor Global was asked for.
var ErrNoSkillScope = errors.New("skills: give a project or global")

// SessionSkill is a skill a session used: the skill's front-matter name
// and the span of its first use.
type SessionSkill struct {
	Name string `json:"name"`
	Span string `json:"span"`
}

// foundSkill is one parsed SKILL.md: path is where it was found, not
// resolved.
type foundSkill struct {
	name, description, path string
}

// skillScan is the result of reading one skill directory.
type skillScan struct {
	skills   []foundSkill
	problems []SkillProblem
}

// skillIndex reads skill directories once per call: a List or Digest reads
// each directory and each project walk at most once however many sessions
// need it.
type skillIndex struct {
	env    Env
	scans  map[string]skillScan
	walks  map[string][]string
	global []foundSkill
	ready  bool
}

func newSkillIndex(env Env) *skillIndex {
	return &skillIndex{env: env, scans: map[string]skillScan{}, walks: map[string][]string{}}
}

// Skills lists the skills req selects. A project the [collect] policy
// disables or denies is not read and is named in Problems. A SKILL.md that
// is not a regular file, has no front matter block, front matter that is
// not YAML or no name is named in Problems, not listed; a skill directory
// that is absent is skipped.
func Skills(env Env, req SkillsRequest) (Inventory, error) {
	if len(req.Projects) == 0 && !req.Global {
		return Inventory{}, ErrNoSkillScope
	}
	idx := newSkillIndex(env)
	type key struct{ scope, path string }
	var order []key
	byKey := map[key]*Skill{}
	problems := map[string]SkillProblem{}
	add := func(scope, harness, root string) {
		sc := idx.scan(root)
		for _, p := range sc.problems {
			if _, ok := problems[p.Path]; !ok {
				problems[p.Path] = p
			}
		}
		for _, f := range sc.skills {
			k := key{scope, f.path}
			s, ok := byKey[k]
			if !ok {
				s = &Skill{Name: f.name, Description: f.description, Path: f.path, Scope: scope}
				byKey[k] = s
				order = append(order, k)
			}
			if !slices.Contains(s.Harnesses, harness) {
				s.Harnesses = append(s.Harnesses, harness)
			}
		}
	}
	if req.Global {
		for _, d := range env.SkillDirs {
			for _, root := range d.Home {
				add(ScopeGlobal, d.Harness, root)
			}
		}
	}
	for _, p := range req.Projects {
		if state, reason := cwdPolicy(env, []string{p}); state != "" {
			problems[p] = SkillProblem{Path: p, Reason: "project not read: collection is " + state + " (" + reason + ")"}

			continue
		}
		for _, dir := range idx.walk(p) {
			for _, d := range env.SkillDirs {
				for _, rel := range d.Project {
					add(ScopeProject, d.Harness, filepath.Join(dir, rel))
				}
			}
		}
	}

	out := Inventory{Skills: []Skill{}, Problems: []SkillProblem{}}
	for _, k := range order {
		s := *byKey[k]
		s.Name, s.Path, s.Description = scrubString(s.Name), scrubString(s.Path), scrubString(s.Description)
		out.Skills = append(out.Skills, s)
	}
	slices.SortStableFunc(out.Skills, func(a, b Skill) int {
		if c := strings.Compare(a.Scope, b.Scope); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}

		return strings.Compare(a.Path, b.Path)
	})
	for _, p := range problems {
		out.Problems = append(out.Problems, SkillProblem{Path: scrubString(p.Path), Reason: scrubString(p.Reason)})
	}
	slices.SortFunc(out.Problems, func(a, b SkillProblem) int { return strings.Compare(a.Path, b.Path) })

	return out, nil
}

// walk is the directories a harness looks in for the project at p: p and
// its ancestors up to and including the nearest one that holds a .git
// entry, or p alone when none does. The walk stops below the home
// directory: neither home nor a directory above it is a project directory,
// so a .git in home does not make the user-level skill directories project
// ones, and p at or above home has none.
func (idx *skillIndex) walk(p string) []string {
	p = filepath.Clean(p)
	if w, ok := idx.walks[p]; ok {
		return w
	}
	aboveHome := func(d string) bool { return idx.env.Home != "" && collect.Within(idx.env.Home, d) }
	var w []string
	for d := p; !aboveHome(d); d = filepath.Dir(d) {
		w = append(w, d)
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			break
		}
		if up := filepath.Dir(d); up == d || aboveHome(up) {
			w = []string{p}

			break
		}
	}
	idx.walks[p] = w

	return w
}

// scan reads the skills of one skill directory: every <root>/<name>/SKILL.md,
// one level deep, a symbolically linked skill directory followed. An
// absent root has none; one that cannot be listed is a problem.
func (idx *skillIndex) scan(root string) skillScan {
	if sc, ok := idx.scans[root]; ok {
		return sc
	}
	var sc skillScan
	var ents []os.DirEntry
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		sc.problems = append(sc.problems, SkillProblem{Path: root, Reason: ioReason(err)})
	case info.IsDir():
		if ents, err = os.ReadDir(root); err != nil {
			sc.problems = append(sc.problems, SkillProblem{Path: root, Reason: ioReason(err)})
		}
	}
	for _, e := range ents {
		dir := filepath.Join(root, e.Name())
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		p := filepath.Join(dir, "SKILL.md")
		name, description, reason := readSkillFrontMatter(p)
		switch {
		case reason == "":
			sc.skills = append(sc.skills, foundSkill{name: name, description: description, path: p})
		case reason != reasonAbsent:
			sc.problems = append(sc.problems, SkillProblem{Path: p, Reason: reason})
		}
	}
	idx.scans[root] = sc

	return sc
}

// reasonAbsent is readSkillFrontMatter's reason for a directory without a
// SKILL.md: not a skill, and not a problem.
const reasonAbsent = "absent"

// reasonNotRegular is the problem of a SKILL.md that is not a regular file.
const reasonNotRegular = "not a regular file"

// readSkillFrontMatter reads the name and description of the front matter
// of the SKILL.md at p: the block between a first line "---" and the next
// line "---", within the first skillFrontMatterMax bytes. reason is ""
// when they were read.
func readSkillFrontMatter(p string) (name, description, reason string) {
	// Only a regular file is opened: opening a FIFO or a device would block
	// or read without end.
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", reasonAbsent
	}
	if err != nil {
		return "", "", ioReason(err)
	}
	if !info.Mode().IsRegular() {
		return "", "", reasonNotRegular
	}
	f, err := os.Open(p)
	if err != nil {
		return "", "", ioReason(err)
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", "", reasonNotRegular
	}
	b, err := io.ReadAll(io.LimitReader(f, skillFrontMatterMax))
	if err != nil {
		return "", "", ioReason(err)
	}
	block, ok := frontMatter(b)
	if !ok {
		return "", "", "no front matter block (--- ... ---) in the first 64 KiB"
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(block, &fm); err != nil {
		return "", "", "front matter is not valid YAML: " + strings.Join(strings.Fields(err.Error()), " ")
	}
	if strings.TrimSpace(fm.Name) == "" {
		return "", "", "front matter has no name"
	}

	return strings.TrimSpace(fm.Name), strings.TrimSpace(fm.Description), ""
}

// frontMatter is the YAML between a first line "---" and the next line
// "---" of b, a UTF-8 byte order mark and CRLF line ends allowed.
func frontMatter(b []byte) ([]byte, bool) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	first, rest, ok := bytes.Cut(b, []byte("\n"))
	if !ok || string(bytes.TrimRight(first, " \t\r")) != "---" {
		return nil, false
	}
	at := 0
	for at <= len(rest) {
		line, _, more := bytes.Cut(rest[at:], []byte("\n"))
		if string(bytes.TrimRight(line, " \t\r")) == "---" {
			return rest[:at], true
		}
		if !more {
			break
		}
		at += len(line) + 1
	}

	return nil, false
}

// known is every skill a session that ran in cwds could use: the
// user-level skills of every harness and the project skills of each cwd's
// walk, by every harness's project directories.
func (idx *skillIndex) known(cwds []string) []foundSkill {
	if !idx.ready {
		for _, d := range idx.env.SkillDirs {
			for _, root := range d.Home {
				idx.global = append(idx.global, idx.scan(root).skills...)
			}
		}
		idx.ready = true
	}
	out := slices.Clone(idx.global)
	for _, cwd := range cwds {
		if state, _ := cwdPolicy(idx.env, []string{cwd}); state != "" {
			continue
		}
		for _, dir := range idx.walk(cwd) {
			for _, d := range idx.env.SkillDirs {
				for _, rel := range d.Project {
					out = append(out, idx.scan(filepath.Join(dir, rel)).skills...)
				}
			}
		}
	}

	return out
}

// usedSkill is a skill's first use at or after a digest's start, with the
// offset of the line that records it.
type usedSkill struct {
	SessionSkill
	offset int64
}

// matched matches the transcript's skill hits at or after from against
// the skills its session could use and returns the first use of each, in
// log order, scrubbed. A hit by name matches a skill by its front-matter
// name; a hit by a SKILL.md path, by the skill directory's base name. Hits
// no known skill matches are left out.
func (idx *skillIndex) matched(tr *transcript, from int64) []usedSkill {
	if tr == nil || len(tr.skillHits) == 0 {
		return nil
	}
	known := idx.known(tr.cwds)
	var out []usedSkill
	seen := map[string]bool{}
	for _, h := range tr.skillHits {
		if h.offset < from {
			continue
		}
		name, ok := matchSkill(h, known)
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, usedSkill{SessionSkill{Name: scrubString(name), Span: scrubString(h.span)}, h.offset})
	}

	return out
}

// used is matched's skills.
func (idx *skillIndex) used(tr *transcript, from int64) []SessionSkill {
	return skillsBefore(idx.matched(tr, from), -1)
}

// skillsBefore is the skills of used first used before the offset to (all
// of them when to is negative), never nil when used is not.
func skillsBefore(used []usedSkill, to int64) []SessionSkill {
	if used == nil {
		return nil
	}
	out := []SessionSkill{}
	for _, u := range used {
		if to < 0 || u.offset < to {
			out = append(out, u.SessionSkill)
		}
	}

	return out
}

// matchSkill is the front-matter name of the first known skill h matches.
func matchSkill(h skillHit, known []foundSkill) (string, bool) {
	for _, s := range known {
		if (h.dir != "" && filepath.Base(filepath.Dir(s.path)) == h.dir) || (h.dir == "" && s.name == h.name) {
			return s.name, true
		}
	}

	return "", false
}
