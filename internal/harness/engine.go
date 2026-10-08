package harness

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// BackupSuffix is appended to a user file's name for the copy taken before
// install first changes it.
const BackupSuffix = ".agentfeedback-backup"

// claudeTimeout bounds every claude mcp command.
const claudeTimeout = 30 * time.Second

// Item is one thing install added, as the manifest records it.
type Item struct {
	Kind string `json:"kind"`
	Role string `json:"role"`
	File string `json:"file,omitempty"`
	// Path and Key locate a JSON member; Path alone an array for an element.
	Path  []string        `json:"path,omitempty"`
	Key   string          `json:"key,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
	// Text is the exact text a TOML block inserted.
	Text string `json:"text,omitempty"`
	// SHA256 is the content written for a whole file.
	SHA256 string `json:"sha256,omitempty"`
	// CreatedFrom is how many Path segments existed before install.
	CreatedFrom int `json:"created_from,omitempty"`
	// Args is the claude command that added an MCP entry, and URL the url
	// that entry points at, or Stdio the command and arguments it runs.
	Args  []string `json:"args,omitempty"`
	URL   string   `json:"url,omitempty"`
	Stdio []string `json:"stdio,omitempty"`

	content []byte
}

// same reports whether two items add the same thing.
func (a Item) same(b Item) bool {
	return a.Kind == b.Kind && a.Role == b.Role && a.File == b.File && slices.Equal(a.Path, b.Path) &&
		a.Key == b.Key && bytes.Equal(a.Value, b.Value) && strings.TrimPrefix(a.Text, "\n") == strings.TrimPrefix(b.Text, "\n") &&
		a.SHA256 == b.SHA256 && slices.Equal(a.Args, b.Args) && a.URL == b.URL && slices.Equal(a.Stdio, b.Stdio)
}

// HarnessRecord is one wired harness in the manifest.
type HarnessRecord struct {
	Mode     string `json:"mode"`
	Reminder bool   `json:"reminder"`
	Docs     bool   `json:"docs"`
	// Binary is the agentfeedback binary the CLI-mode hooks or the stdio MCP
	// entry of this harness run; empty for a URL MCP entry and for records
	// written before it was kept.
	Binary string `json:"binary,omitempty"`
	Items  []Item `json:"items"`
}

// FileRecord is one file install changed or created. Diverged marks a file
// that someone else changed between two installs; uninstall then never
// restores its backup or deletes it.
type FileRecord struct {
	Created      bool   `json:"created"`
	Backup       string `json:"backup"`
	BackupSHA256 string `json:"backup_sha256,omitempty"`
	SHA256After  string `json:"sha256_after"`
	Diverged     bool   `json:"diverged,omitempty"`
}

// Manifest is install.json.
type Manifest struct {
	Version int    `json:"version"`
	Server  string `json:"server"`
	Binary  string `json:"binary"`
	// CodexHome and ClaudeConfigDir are the locations the run resolved,
	// so a later run under another environment still finds what it wrote.
	// ClaudeConfigDirSet records whether CLAUDE_CONFIG_DIR was set, which
	// decides where .claude.json lives even when the directory is ~/.claude.
	CodexHome          string                    `json:"codex_home,omitempty"`
	ClaudeConfigDir    string                    `json:"claude_config_dir,omitempty"`
	ClaudeConfigDirSet bool                      `json:"claude_config_dir_set,omitempty"`
	Harnesses          map[string]*HarnessRecord `json:"harnesses"`
	Files              map[string]*FileRecord    `json:"files"`
	DirsCreated        []string                  `json:"dirs_created"`
}

func newManifest() *Manifest {
	return &Manifest{Version: 1, Harnesses: map[string]*HarnessRecord{}, Files: map[string]*FileRecord{}}
}

func (m *Manifest) clone() *Manifest {
	data, _ := json.Marshal(m)
	c := newManifest()
	_ = json.Unmarshal(data, c)
	if c.Harnesses == nil {
		c.Harnesses = map[string]*HarnessRecord{}
	}
	if c.Files == nil {
		c.Files = map[string]*FileRecord{}
	}

	return c
}

func (m *Manifest) encode() []byte {
	data, _ := json.MarshalIndent(m, "", "  ")

	return append(data, '\n')
}

// LoadManifest reads the manifest; a missing one is an empty manifest.
func LoadManifest(path string) (m *Manifest, raw []byte, err error) {
	raw, err = os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newManifest(), nil, nil
	}
	if err != nil {
		return nil, nil, &FileError{Path: path, Err: err}
	}
	m = newManifest()
	if err := json.Unmarshal(raw, m); err != nil || m.Version != 1 {
		if err == nil {
			err = fmt.Errorf("version %d is not 1", m.Version)
		}

		return nil, nil, &Refusal{
			Problem: fmt.Sprintf("the install manifest %s is not readable: %v", path, err),
			Next:    "restore it from a backup, or remove it and undo the wiring by hand",
		}
	}
	if m.Harnesses == nil {
		m.Harnesses = map[string]*HarnessRecord{}
	}
	if m.Files == nil {
		m.Files = map[string]*FileRecord{}
	}
	// A record written before records carried their own binary runs the
	// manifest-wide one as loaded, before a later run overwrites it.
	for _, rec := range m.Harnesses {
		if rec != nil && rec.Mode == ModeCLI && rec.Binary == "" {
			rec.Binary = m.Binary
		}
	}

	return m, raw, nil
}

// Refusal is a run stopped with the next step. Most refusals come from
// planning, before anything is written. Two come while applying: claude mcp
// add-json reporting the name as taken (claude steps run first, so only the
// run's first manifest write and earlier claude commands precede it, such
// as the remove of a server change, which runs [remove, add-json]), and a
// file that changed under the run, which can follow writes of earlier
// steps or files of the same run. The outcome's changed then lists what
// was written.
type Refusal struct{ Problem, Next string }

func (r *Refusal) Error() string { return r.Problem + "; " + r.Next + "." }

// FileError is a file install could not read, parse or write.
type FileError struct {
	Path string
	Err  error
}

func (e *FileError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e *FileError) Unwrap() error { return e.Err }

// editErr is err from reading or editing path: a refusal stays one, a
// duplicate key becomes one, anything else is a FileError.
func editErr(path string, err error) error {
	var dup *DuplicateKeyError
	var r *Refusal
	if errors.As(err, &r) {
		return err
	}
	if errors.As(err, &dup) {
		return &Refusal{
			Problem: fmt.Sprintf("%s has the key %s twice in one object", path, dup.Key),
			Next:    "fix the file, then run install again",
		}
	}

	return &FileError{Path: path, Err: err}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)

	return hex.EncodeToString(s[:])
}

// fileState is one file as read at planning time and as the plan leaves it.
type fileState struct {
	path    string
	exists  bool
	orig    []byte
	mode    fs.FileMode
	cur     []byte
	present bool
	// target is the file a symbolic link at path resolves to; reads and
	// writes go to it, and path stays the file's identity.
	target string
	// planned marks a file read by plan.load; only those are checked for a
	// link that appeared during the run (the manifest is not).
	planned bool
	// planned on finalize
	backupNew    bool
	backupDelete string
	owner        string
}

// dest is the file written: the link's target, or path itself.
func (f *fileState) dest() string {
	if f.target != "" {
		return f.target
	}

	return f.path
}

func (f *fileState) changed() bool {
	return f.exists != f.present || !bytes.Equal(f.orig, f.cur)
}

type step struct {
	name   string
	first  [][]string // claude commands, run before the files are written
	files  []string
	record *HarnessRecord // nil: the harness is removed
}

type plan struct {
	env   Env
	man   *Manifest // the manifest as the plan leaves it
	files map[string]*fileState
	owned map[string]bool // files recorded in the manifest before the run
	steps []*step
	notes map[string][]string
	kept  []string // backups left in place because a file changed since
}

func (p *plan) load(path string) (*fileState, error) {
	if f, ok := p.files[path]; ok {
		return f, nil
	}
	f := &fileState{path: path, mode: 0o644}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, &FileError{Path: path, Err: err}
	case info.Mode()&fs.ModeSymlink != 0:
		if !p.env.configFiles()[path] {
			return nil, p.symlinkRefusal(path)
		}
		if err := p.loadLinked(f); err != nil {
			return nil, err
		}
	case !info.Mode().IsRegular():
		return nil, &Refusal{Problem: path + " is not a regular file", Next: "replace it with a regular file or remove it"}
	default:
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, &FileError{Path: path, Err: err}
		}
		f.exists, f.orig, f.mode = true, data, info.Mode().Perm()
	}
	f.cur, f.present, f.planned = bytes.Clone(f.orig), f.exists, true
	dest := filepath.Clean(f.dest())
	for _, other := range sortedKeys(p.files) {
		o := p.files[other]
		if filepath.Clean(o.dest()) != dest {
			continue
		}
		problem := path + " and " + other + " are the same file " + dest
		if f.target != "" && o.target != "" {
			problem = path + " and " + other + " both link to " + dest
		}

		return nil, &Refusal{Problem: problem, Next: "wire one of the two harnesses by hand, or give each its own file"}
	}
	p.files[path] = f

	return f, nil
}

// loadLinked reads the configuration file a symbolic link at f.path points
// to; a dangling link and a target that is not a regular file are refused.
func (p *plan) loadLinked(f *fileState) error {
	target, err := filepath.EvalSymlinks(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		dest, _ := os.Readlink(f.path)

		return &Refusal{Problem: f.path + " is a symbolic link to " + dest + ", which does not exist", Next: "create the target or remove the link"}
	}
	if err != nil {
		return &FileError{Path: f.path, Err: err}
	}
	info, err := os.Stat(target)
	if err != nil {
		return &FileError{Path: f.path, Err: err}
	}
	if !info.Mode().IsRegular() {
		return &Refusal{Problem: f.path + " is a symbolic link to " + target + ", which is not a regular file", Next: "point the link at a regular file or remove it"}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return &FileError{Path: f.path, Err: err}
	}
	f.exists, f.orig, f.mode, f.target = true, data, info.Mode().Perm(), target

	return nil
}

// isLink reports whether path is a symbolic link.
func isLink(path string) bool {
	info, err := os.Lstat(path)

	return err == nil && info.Mode()&fs.ModeSymlink != 0
}

// symlinkRefusal refuses a symbolic link: a linked skill is removed, a
// linked configuration file is wired by hand or replaced by its target.
func (p *plan) symlinkRefusal(path string) error {
	next := "wire this harness by hand from the table in docs/operate.md, or replace the link with the file it points to"
	switch filepath.Base(path) {
	case "SKILL.md", "agentfeedback", "agentfeedback-docs", "agentfeedback.js", "agentfeedback.ts", "agentfeedback.json":
		next = "remove it, then run agentfeedback install again"
	}
	if p.env.skillRootOf(path) != "" {
		next = "remove it, then run agentfeedback install again"
	}

	return &Refusal{Problem: path + " is a symbolic link; agentfeedback install edits only regular files", Next: next}
}

func (p *plan) touch(s *step, f *fileState) {
	if f.owner == "" {
		f.owner = s.name
		s.files = append(s.files, f.path)
	}
}

func foreign(path, what string) error {
	return &Refusal{
		Problem: fmt.Sprintf("%s already holds %s, which agentfeedback install did not write", path, what),
		Next:    "remove or rename it, then run agentfeedback install again",
	}
}

// present reports whether item is in place in the planned state.
func (p *plan) present(it Item) (bool, error) {
	if it.Kind == KindClaudeMCP {
		known, entry, exists := p.claudeEntry()

		return !known || (exists && entry.is(it)), nil
	}
	if it.Kind == KindSkillFile && p.env.symlinkedAncestor(it.File) != "" {
		return false, nil
	}
	f, err := p.load(it.File)
	if err != nil || !f.present {
		return false, err
	}
	switch it.Kind {
	case KindSkillFile, KindPluginFile:
		return true, nil
	case KindJSONMember:
		v, err := GetMember(f.cur, it.Path, it.Key)
		if err != nil {
			return false, editErr(it.File, err)
		}

		return v != nil, nil
	case KindJSONElem:
		ok, err := HasElement(f.cur, it.Path, it.Value)
		if err != nil {
			return false, editErr(it.File, err)
		}

		return ok, nil
	case KindTOMLBlock:
		return bytes.Contains(f.cur, []byte(strings.TrimPrefix(it.Text, "\n"))), nil
	}

	return false, nil
}

// claudeRemoveArgs removes the Claude Code user-scope entry install added.
var claudeRemoveArgs = []string{"mcp", "remove", "agentfeedback", "--scope", "user"}

// claudeMCP is what identifies a Claude Code MCP entry: its url, or the
// command and arguments of a stdio entry.
type claudeMCP struct {
	URL     string   `json:"url"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// is reports whether the entry is the one item added; an item recorded
// without a url or a command (an older manifest) matches any entry.
func (c claudeMCP) is(it Item) bool {
	switch {
	case it.URL != "":
		return c.URL == it.URL
	case len(it.Stdio) > 0:
		return c.URL == "" && slices.Equal(append([]string{c.Command}, c.Args...), it.Stdio)
	}

	return true
}

// describe is how the entry is named in a message.
func (c claudeMCP) describe() string {
	switch {
	case c.URL != "":
		return "points at " + c.URL
	case c.Command != "":
		return "runs " + strings.Join(append([]string{c.Command}, c.Args...), " ")
	}

	return "has neither a url nor a command"
}

// claudeEntry reads the user-scope agentfeedback entry from Claude Code's
// .claude.json without changing it. known is false when the file cannot be
// read or parsed; then the entry is assumed to be as the manifest records
// it.
func (p *plan) claudeEntry() (known bool, entry claudeMCP, exists bool) {
	data, err := os.ReadFile(p.env.claudeJSON())
	if err != nil {
		return false, entry, false
	}
	v, err := GetMember(data, []string{"mcpServers"}, "agentfeedback")
	if err != nil {
		return false, entry, false
	}
	if v == nil {
		return true, entry, false
	}
	_ = json.Unmarshal(Standard(v), &entry)

	return true, entry, true
}

// claudeManual is the note for a Claude Code entry uninstall could not
// remove.
const claudeManual = "remove the Claude Code MCP entry by hand with claude mcp remove agentfeedback --scope user"

// claudeForeign refuses before anything is written when the user-scope MCP
// servers in Claude Code's .claude.json already name agentfeedback. The file is only
// read: Claude Code owns it. claude mcp add-json reporting the name as taken
// stays the fallback for a file this check cannot read.
func (p *plan) claudeForeign() error {
	if known, _, _ := p.claudeEntry(); !known {
		return nil
	}
	path := p.env.claudeJSON()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if v, err := GetMember(data, []string{"mcpServers"}, "agentfeedback"); err == nil && v != nil {
		return &Refusal{
			Problem: "Claude Code already has an MCP server named agentfeedback in " + path + ", which agentfeedback install did not add",
			Next:    "remove it with claude mcp remove agentfeedback --scope user, then run agentfeedback install again",
		}
	}

	return nil
}

// remove takes item out of the planned state. During install a whole file
// changed since it was written is refused; uninstall leaves it in place.
func (p *plan) remove(s *step, it Item, uninstall bool) error {
	if it.Kind == KindClaudeMCP {
		known, entry, exists := p.claudeEntry()
		switch {
		case known && !exists:
			if uninstall {
				p.notes[s.name] = append(p.notes[s.name], "the Claude Code MCP entry agentfeedback was already removed")
			}

			return nil
		case known && !entry.is(it):
			added := "at the URL"
			if it.URL == "" {
				added = "the entry"
			}
			if !uninstall {
				return &Refusal{
					Problem: "Claude Code's MCP server agentfeedback " + entry.describe() + ", not " + added + " agentfeedback install added",
					Next:    "remove it with claude mcp remove agentfeedback --scope user, then run agentfeedback install again",
				}
			}
			p.notes[s.name] = append(p.notes[s.name], "Claude Code's MCP server agentfeedback "+entry.describe()+", not "+added+" install added; it is left in place")

			return nil
		}
		if uninstall {
			if _, err := p.env.LookPath("claude"); err != nil {
				p.notes[s.name] = append(p.notes[s.name], "claude is not on PATH; "+claudeManual)

				return nil
			}
		}
		// Removals are planned before additions, so a replaced entry is
		// removed before its successor is added.
		s.first = append(s.first, claudeRemoveArgs)

		return nil
	}
	if it.Kind == KindPluginFile && uninstall && isLink(it.File) {
		p.notes[s.name] = append(p.notes[s.name], it.File+" is a symbolic link; it is left in place")

		return nil
	}
	if it.Kind == KindSkillFile {
		if link := p.env.symlinkedAncestor(it.File); link != "" {
			if !uninstall {
				return p.symlinkRefusal(link)
			}
			p.notes[s.name] = append(p.notes[s.name], link+" is a symbolic link; "+it.File+" is left in place")

			return nil
		}
	}
	f, err := p.load(it.File)
	if err != nil {
		return err
	}
	if !f.present {
		return nil
	}
	if it.Kind == KindJSONMember {
		changed, err := memberChanged(f.cur, it)
		var dup *DuplicateKeyError
		switch {
		case uninstall && errors.As(err, &dup):
			p.notes[s.name] = append(p.notes[s.name], fmt.Sprintf("%s has the key %s twice in one object; it is left as it is", it.File, dup.Key))
			p.touch(s, f)

			return nil
		case err != nil:
			return editErr(it.File, err)
		case changed && !uninstall:
			return &Refusal{
				Problem: fmt.Sprintf("%s: %s.%s was changed since install", it.File, strings.Join(it.Path, "."), it.Key),
				Next:    "move your changes elsewhere and remove the entry, then run agentfeedback install again",
			}
		case changed:
			p.notes[s.name] = append(p.notes[s.name], fmt.Sprintf("%s: %s.%s was changed since install and is left in place", it.File, strings.Join(it.Path, "."), it.Key))
			p.touch(s, f)

			return nil
		}
	}
	var out []byte
	switch it.Kind {
	case KindSkillFile, KindPluginFile:
		if sha(f.cur) != it.SHA256 {
			if !uninstall {
				return &Refusal{
					Problem: fmt.Sprintf("%s was modified since install", it.File),
					Next:    "move your changes elsewhere and remove the file, then run agentfeedback install again",
				}
			}
			p.notes[s.name] = append(p.notes[s.name], it.File+" was modified since install and is left in place")

			return nil
		}
		p.touch(s, f)
		f.cur, f.present = nil, false

		return nil
	case KindJSONMember:
		out, err = RemoveMember(f.cur, it.Path, it.Key, it.CreatedFrom)
	case KindJSONElem:
		out, err = RemoveElement(f.cur, it.Path, it.Value, it.CreatedFrom)
	case KindTOMLBlock:
		out, err = tomlRemove(it.File, f.cur, it.Text)
	}
	var dup *DuplicateKeyError
	if uninstall && errors.As(err, &dup) {
		p.notes[s.name] = append(p.notes[s.name], fmt.Sprintf("%s has the key %s twice in one object; it is left as it is", it.File, dup.Key))
		p.touch(s, f)

		return nil
	}
	if err != nil {
		return editErr(it.File, err)
	}
	if !bytes.Equal(out, f.cur) {
		p.touch(s, f)
		f.cur = out
	}

	return nil
}

// memberChanged reports whether the member it recorded holds a value other
// than the one install wrote; a missing member is not changed.
func memberChanged(doc []byte, it Item) (bool, error) {
	v, err := GetMember(doc, it.Path, it.Key)
	if err != nil || v == nil {
		return false, err
	}
	got, err := compact(v)
	if err != nil {
		return true, nil
	}
	want, err := compact(it.Value)
	if err != nil {
		return true, nil
	}

	return !bytes.Equal(got, want), nil
}

// add puts item into the planned state, refusing one that is already there
// without a manifest record.
func (p *plan) add(s *step, it *Item) error {
	if it.Kind == KindClaudeMCP {
		if _, err := p.env.LookPath("claude"); err != nil {
			return &Refusal{Problem: "claude is not on PATH, so the Claude Code MCP entry cannot be added", Next: "install Claude Code or add it to PATH, then run agentfeedback install again"}
		}
		if !slices.ContainsFunc(s.first, func(c []string) bool { return slices.Equal(c, claudeRemoveArgs) }) {
			if err := p.claudeForeign(); err != nil {
				return err
			}
		}
		s.first = append(s.first, it.Args)

		return nil
	}
	f, err := p.load(it.File)
	if err != nil {
		return err
	}
	switch it.Kind {
	case KindSkillFile, KindPluginFile:
		dir := it.File
		what := "a file named " + filepath.Base(it.File)
		ours := p.owned[it.File]
		if it.Kind == KindSkillFile {
			dir = p.skillRoot(s.name, it.File)
			what = "an " + filepath.Base(dir) + " skill directory"
			if link := p.env.symlinkedAncestor(it.File); link != "" {
				return p.symlinkRefusal(link)
			}
			ours = p.ownsBelow(dir)
		}
		if _, err := os.Lstat(dir); err == nil && !ours && !p.ourEmptyDir(dir) {
			return foreign(dir, what)
		}
		if f.present && !p.owned[it.File] {
			return foreign(it.File, what)
		}
		p.touch(s, f)
		f.cur, f.present = bytes.Clone(it.content), true

		return nil
	case KindTOMLBlock:
		if f.present {
			bad, err := tomlForeign(it.File, f.cur)
			if err != nil {
				return editErr(it.File, err)
			}
			if bad {
				return foreign(it.File, "an [mcp_servers.agentfeedback] table")
			}
		}
		out, inserted, err := tomlAppend(f.cur, it.Text)
		if err != nil {
			return editErr(it.File, err)
		}
		it.Text = inserted
		p.touch(s, f)
		f.cur, f.present = out, true

		return nil
	}
	doc := f.cur
	if !f.present {
		doc = []byte("{}\n")
		if strings.HasSuffix(it.File, filepath.Join(".cursor", "hooks.json")) {
			doc = []byte("{\n  \"version\": 1\n}\n")
		}
	}
	var out []byte
	var from int
	switch it.Kind {
	case KindJSONMember:
		v, err := GetMember(doc, it.Path, it.Key)
		if err != nil {
			return editErr(it.File, err)
		}
		if v != nil {
			return foreign(it.File, "an "+strings.Join(append(slices.Clone(it.Path), it.Key), ".")+" entry")
		}
		out, from, err = InsertMember(doc, it.Path, it.Key, it.Value)
		if err != nil {
			return editErr(it.File, err)
		}
	case KindJSONElem:
		ok, err := HasElement(doc, it.Path, it.Value)
		if err != nil {
			return editErr(it.File, err)
		}
		if ok {
			return foreign(it.File, "the hook "+string(it.Value)+" under "+strings.Join(it.Path, "."))
		}
		out, from, err = AppendElement(doc, it.Path, it.Value)
		if err != nil {
			return editErr(it.File, err)
		}
	}
	it.CreatedFrom = from
	p.touch(s, f)
	f.cur, f.present = out, true

	return nil
}

// skillRoot is the skill directory holding file: the entry of the harness's
// skills directory that file lies beneath (agentfeedback for SKILL.md,
// agentfeedback-docs for the docs skill's files).
func (p *plan) skillRoot(harness, file string) string {
	for _, skills := range p.env.skillDirs(harness) {
		rel, err := filepath.Rel(skills, file)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		first, _, _ := strings.Cut(rel, string(filepath.Separator))

		return filepath.Join(skills, first)
	}

	return filepath.Dir(file)
}

// skillRootOf is the skill directory path is or lies beneath: the entry of
// any harness's skills directory holding it, or "" outside every one.
func (e Env) skillRootOf(path string) string {
	sep := string(filepath.Separator)
	for _, a := range registry {
		for _, skills := range a.SkillDirs(e) {
			if rel, ok := strings.CutPrefix(path, skills+sep); ok && rel != "" {
				first, _, _ := strings.Cut(rel, sep)

				return filepath.Join(skills, first)
			}
		}
	}

	return ""
}

// symlinkedAncestor is the first symbolic link among file's directories,
// from its parent up to and including its skill root, or "".
func (e Env) symlinkedAncestor(file string) string {
	root := e.skillRootOf(file)
	if root == "" {
		root = filepath.Dir(file)
	}
	for d := filepath.Dir(file); ; d = filepath.Dir(d) {
		if info, err := os.Lstat(d); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return d
		}
		if d == root || filepath.Dir(d) == d {
			return ""
		}
	}
}

// ownsBelow reports whether the manifest records a file beneath dir.
func (p *plan) ownsBelow(dir string) bool {
	for f := range p.owned {
		if strings.HasPrefix(f, dir+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// ourEmptyDir reports whether dir is an empty directory the manifest
// records as created by install, such as one left by an interrupted run.
func (p *plan) ourEmptyDir(dir string) bool {
	if !slices.Contains(p.man.DirsCreated, dir) {
		return false
	}
	entries, err := os.ReadDir(dir)

	return err == nil && len(entries) == 0
}

func newPlan(env Env, man *Manifest) *plan {
	p := &plan{env: env, man: man.clone(), files: map[string]*fileState{}, owned: map[string]bool{}, notes: map[string][]string{}}
	for _, h := range man.Harnesses {
		for _, it := range h.Items {
			if it.File != "" {
				p.owned[it.File] = true
			}
		}
	}

	return p
}

// install reconciles one harness: recorded items that are still wanted and
// in place stay, the others are removed first, the missing ones are added.
func (p *plan) install(name string, o Options) error {
	s := &step{name: name}
	p.steps = append(p.steps, s)
	d, err := p.desired(name, o)
	if err != nil {
		return err
	}
	p.notes[name] = append(p.notes[name], d.notes...)
	old := p.man.Harnesses[name]
	final := make([]Item, len(d.items))
	kept := make([]bool, len(d.items))
	if old != nil {
		for _, it := range old.Items {
			i := slices.IndexFunc(d.items, func(w Item) bool { return w.same(it) })
			ok := false
			if i >= 0 && !kept[i] {
				if ok, err = p.present(it); err != nil {
					return err
				}
			}
			// A recorded whole file changed since install is refused even
			// when it is still wanted.
			if ok && (it.Kind == KindSkillFile || it.Kind == KindPluginFile) {
				if f, _ := p.load(it.File); sha(f.cur) != it.SHA256 {
					return p.remove(s, it, false)
				}
			}
			if ok && it.Kind == KindJSONMember {
				f, _ := p.load(it.File)
				if changed, err := memberChanged(f.cur, it); err != nil || changed {
					if err != nil {
						return editErr(it.File, err)
					}

					return p.remove(s, it, false)
				}
			}
			if ok {
				kept[i], final[i] = true, it

				continue
			}
			if err := p.remove(s, it, false); err != nil {
				return err
			}
		}
	}
	for i := range d.items {
		if kept[i] {
			continue
		}
		it := d.items[i]
		if err := p.add(s, &it); err != nil {
			return err
		}
		final[i] = it
	}
	s.record = &HarnessRecord{Mode: o.Mode, Reminder: o.Reminder && o.Mode == ModeCLI, Docs: o.Docs, Items: final}
	if o.Mode == ModeCLI || o.stdio() {
		s.record.Binary = o.Binary
	}
	p.man.Harnesses[name] = s.record

	return nil
}

func (p *plan) uninstall(name string) error {
	old := p.man.Harnesses[name]
	if old == nil {
		p.notes[name] = append(p.notes[name], name+" is not in the install manifest; nothing to remove")

		return nil
	}
	s := &step{name: name}
	p.steps = append(p.steps, s)
	for _, it := range old.Items {
		if err := p.remove(s, it, true); err != nil {
			return err
		}
	}
	// Every file the harness recorded is settled, even one whose entries
	// are all gone already, so no file record or backup outlives them.
	for _, it := range old.Items {
		if it.File == "" || p.man.Files[it.File] == nil {
			continue
		}
		// A file reached through a link is left in place and forgotten.
		if (it.Kind == KindSkillFile && p.env.symlinkedAncestor(it.File) != "") || (it.Kind == KindPluginFile && isLink(it.File)) {
			delete(p.man.Files, it.File)

			continue
		}
		f, err := p.load(it.File)
		if err != nil {
			return err
		}
		p.touch(s, f)
	}
	delete(p.man.Harnesses, name)

	return nil
}

// finalize settles every touched file: a file left without items goes back
// to its backup (or away, when install created it) if nobody changed it
// since; otherwise it keeps the surgical result and its backup. A file
// that keeps items gets its record, and a backup before its first change.
func (p *plan) finalize() error {
	paths := make([]string, 0, len(p.files))
	for path, f := range p.files {
		if f.owner != "" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		f := p.files[path]
		remaining := false
		for _, h := range p.man.Harnesses {
			for _, it := range h.Items {
				remaining = remaining || it.File == path
			}
		}
		rec := p.man.Files[path]
		if !remaining {
			if rec == nil {
				continue
			}
			if err := p.settle(f, rec); err != nil {
				return err
			}
			delete(p.man.Files, path)

			continue
		}
		if rec == nil {
			rec = &FileRecord{Created: !f.exists}
			if f.exists {
				rec.Backup = path + BackupSuffix
				rec.BackupSHA256 = sha(f.orig)
				f.backupNew = true
				if _, err := os.Lstat(rec.Backup); err == nil {
					// A backup equal to the file is the leftover of an
					// interrupted run: it is taken over as the backup.
					data, rerr := os.ReadFile(rec.Backup)
					if rerr != nil || !bytes.Equal(data, f.orig) {
						return &Refusal{
							Problem: rec.Backup + " already exists and the install manifest does not record it",
							Next:    "move it away, then run agentfeedback install again",
						}
					}
					f.backupNew = false
				}
			}
			p.man.Files[path] = rec
		} else if f.exists && sha(f.orig) != rec.SHA256After && f.changed() {
			rec.Diverged = true
		}
		rec.SHA256After = sha(f.cur)
	}

	return nil
}

// settle plans a file that keeps no item: back to its backup, or away when
// install created it, if nobody changed it since install. A diverged or
// changed file, or one whose backup is missing or not the one install took,
// keeps the surgical result; a backup left in place is listed.
func (p *plan) settle(f *fileState, rec *FileRecord) error {
	owner := f.owner
	if !f.exists || sha(f.orig) != rec.SHA256After || rec.Diverged {
		p.keepBackup(f, rec)

		return nil
	}
	if rec.Created && f.target != "" {
		p.notes[owner] = append(p.notes[owner], f.path+" is now a symbolic link; only the agentfeedback entries were removed")

		return nil
	}
	if rec.Created {
		f.cur, f.present = nil, false

		return nil
	}
	data, err := os.ReadFile(rec.Backup)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		p.notes[owner] = append(p.notes[owner], fmt.Sprintf("the backup %s is missing, so %s only had the agentfeedback entries removed", rec.Backup, f.path))
	case err != nil:
		return &FileError{Path: rec.Backup, Err: err}
	case rec.BackupSHA256 != "" && sha(data) != rec.BackupSHA256:
		p.notes[owner] = append(p.notes[owner], fmt.Sprintf("the backup %s is not the copy install took, so %s only had the agentfeedback entries removed and the backup is kept", rec.Backup, f.path))
		p.keepBackup(f, rec)
	default:
		f.cur, f.present = data, true
		f.backupDelete = rec.Backup
	}

	return nil
}

// keepBackup leaves a file's backup in place and lists it, unless the file
// as planned is byte for byte the backup: then the backup is deleted.
func (p *plan) keepBackup(f *fileState, rec *FileRecord) {
	if rec.Backup == "" {
		return
	}
	data, err := os.ReadFile(rec.Backup)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err == nil && f.present && bytes.Equal(data, f.cur):
		f.backupDelete = rec.Backup
	default:
		p.kept = append(p.kept, rec.Backup)
	}
}

// Request is one install or uninstall run.
type Request struct {
	Harnesses []string
	Options   Options
	Uninstall bool
	DryRun    bool
}

// HarnessStatus is one harness as a run leaves it.
type HarnessStatus struct {
	Name     string `json:"name"`
	Detected string `json:"detected,omitempty"`
	Mode     string `json:"mode"`
	Skill    string `json:"skill"`
	MCP      string `json:"mcp"`
	Hook     string `json:"hook"`
	Reminder string `json:"reminder"`
	Docs     string `json:"docs"`
	// Binary is the agentfeedback binary a CLI-mode harness's hooks or its
	// stdio MCP entry run, and BinaryVersion its version when Env.Version is
	// set.
	Binary        string `json:"binary,omitempty"`
	BinaryVersion string `json:"binary_version,omitempty"`
	// Verification is the adapter's verification record; set by Status.
	Verification *Verified `json:"verification,omitempty"`
	Notes        []string  `json:"notes,omitempty"`
}

// Result is what a run did, or with DryRun would do.
type Result struct {
	Status    string
	Harnesses []HarnessStatus
	Changed   []string
	Backups   []string
	Commands  []string
}

// Run plans every harness first, so a refusal writes nothing, then applies
// harness by harness. The caller holds Lock for the whole run.
func (e Env) Run(req Request) (Result, error) {
	mpath := e.ManifestPath()
	man, raw, err := LoadManifest(mpath)
	if err != nil {
		return Result{}, err
	}
	e, located := e.withRecorded(man)
	if err := e.validateManifest(man, mpath); err != nil {
		return Result{}, err
	}
	p := newPlan(e, man)
	for name, n := range located {
		if slices.Contains(req.Harnesses, name) {
			p.notes[name] = append(p.notes[name], n)
		}
	}
	for _, name := range req.Harnesses {
		if req.Uninstall {
			err = p.uninstall(name)
		} else {
			err = p.install(name, req.Options)
		}
		if err != nil {
			return Result{}, err
		}
	}
	if !req.Uninstall {
		p.man.Server, p.man.Binary = req.Options.Server, req.Options.Binary
	}
	if err := p.finalize(); err != nil {
		return Result{}, err
	}
	e.recordLocations(p.man)
	p.order()

	res := Result{Backups: slices.Clone(p.kept)}
	newRaw := p.man.encode()
	manifestGone := len(p.man.Harnesses) == 0
	manifestChanged := (manifestGone && raw != nil) || (!manifestGone && !bytes.Equal(newRaw, raw))
	for _, s := range p.steps {
		for _, c := range s.first {
			res.Commands = append(res.Commands, ClaudeCommand(c))
		}
		for _, path := range s.files {
			f := p.files[path]
			if f.changed() {
				res.Changed = append(res.Changed, path)
			}
			if f.backupNew {
				res.Backups = append(res.Backups, path+BackupSuffix)
			}
		}
	}
	if manifestChanged {
		res.Changed = append(res.Changed, mpath)
	}
	nothing := len(res.Changed) == 0 && len(res.Commands) == 0
	switch {
	case req.DryRun:
		res.Status = "dry_run"
	case nothing:
		res.Status = "unchanged"
	case req.Uninstall:
		res.Status = "uninstalled"
	default:
		res.Status = "installed"
	}
	if req.DryRun || nothing {
		res.Harnesses = p.statuses(req.Harnesses)

		return res, nil
	}

	w := &written{}
	if err := e.apply(p, man.clone(), mpath, req, w); err != nil {
		// A run that had no manifest and stopped before changing anything
		// else (a claude refusal) leaves none behind.
		if raw == nil && w.manifest && len(w.changed) == 0 && len(w.backups) == 0 {
			if os.Remove(mpath) == nil {
				w.manifest = false
				for i := len(w.firstDirs) - 1; i >= 0; i-- {
					_ = os.Remove(w.firstDirs[i])
				}
			}
		}
		res.Status = "error"
		res.Changed, res.Backups = w.changed, w.backups
		if w.manifest {
			res.Changed = append(res.Changed, mpath)
		}

		return res, err
	}
	res.Harnesses = p.statuses(req.Harnesses)

	return res, nil
}

// order moves the steps that run claude commands first, so their refusal
// comes before any file of the run is written.
func (p *plan) order() {
	sort.SliceStable(p.steps, func(i, j int) bool {
		return len(p.steps[i].first) > 0 && len(p.steps[j].first) == 0
	})
}

// written is what apply actually changed, for the outcome of a failed run.
type written struct {
	changed   []string
	backups   []string
	manifest  bool
	firstDirs []string // directories the run's first manifest write created
}

// ClaudeCommand is the claude command line with args, quoted for a POSIX
// shell where an argument needs it.
func ClaudeCommand(args []string) string {
	return "claude " + strings.Join(quoteArgs(args), " ")
}

func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \"'{}$") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}

	return out
}

// runClaude runs claude against the configuration install uses:
// CLAUDE_CONFIG_DIR is set to its directory when it was set, even to
// ~/.claude, and unset otherwise.
func (e Env) runClaude(args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), claudeTimeout)
	defer cancel()
	dir := ""
	if e.claudeSet() {
		dir = e.claudeDir()
	}

	return e.Exec(ctx, map[string]string{"CLAUDE_CONFIG_DIR": dir}, "claude", args...)
}

// apply carries out the plan step by step and keeps the manifest equal to
// what is on disk: each claude command and each file is followed at once by
// a manifest write recording exactly that change. A failed write therefore
// leaves a manifest that matches the disk, and the next run finishes or
// replaces what this one started. The harness record's final mode and
// reminder (or its removal) are written after the step's last file.
func (e Env) apply(p *plan, applied *Manifest, mpath string, req Request, w *written) error {
	commit := func() error {
		p.env.recordLocations(applied)
		if len(applied.Harnesses) == 0 {
			if err := os.Remove(mpath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return &FileError{Path: mpath, Err: err}
			}
			w.manifest = true

			return nil
		}
		created, err := writeManifest(mpath, applied)
		applied.DirsCreated = append(applied.DirsCreated, created...)
		if err == nil && len(created) > 0 {
			_, err = writeManifest(mpath, applied)
		}
		if err == nil {
			w.manifest = true
		}

		return err
	}
	// The first write of the run records its server and binary with every
	// existing record unchanged; when it fails, nothing is written. Every
	// write records the locations of the harnesses it still holds, so an
	// interrupted uninstall keeps the location its retry needs.
	if !req.Uninstall {
		applied.Server, applied.Binary = p.man.Server, p.man.Binary
	}
	before := len(applied.DirsCreated)
	p.env.recordLocations(applied)
	err := writeFirst(mpath, applied)
	w.firstDirs = slices.Clone(applied.DirsCreated[before:])
	if err != nil {
		return err
	}
	w.manifest = true
	for _, s := range p.steps {
		var oldItems, finalItems []Item
		rec := applied.Harnesses[s.name]
		if rec != nil {
			oldItems = slices.Clone(rec.Items)
			rec = &HarnessRecord{Mode: rec.Mode, Reminder: rec.Reminder, Docs: rec.Docs, Binary: rec.Binary, Items: slices.Clone(rec.Items)}
		} else if s.record != nil {
			rec = &HarnessRecord{Mode: s.record.Mode, Reminder: s.record.Reminder, Docs: s.record.Docs, Binary: s.record.Binary}
		} else {
			rec = &HarnessRecord{}
		}
		if s.record != nil {
			finalItems = s.record.Items
		}
		has := func(list []Item, it Item) bool { return slices.ContainsFunc(list, it.same) }
		var removed, added []Item
		for _, it := range oldItems {
			if !has(finalItems, it) {
				removed = append(removed, it)
			}
		}
		for _, it := range finalItems {
			if !has(oldItems, it) {
				added = append(added, it)
			}
		}
		// update drops the removed items drop matches and adds the added
		// ones add matches, then writes the manifest.
		update := func(drop, add func(Item) bool) error {
			for _, it := range removed {
				if drop(it) {
					if i := slices.IndexFunc(rec.Items, it.same); i >= 0 {
						rec.Items = slices.Delete(rec.Items, i, i+1)
					}
				}
			}
			for _, it := range added {
				if add(it) && !has(rec.Items, it) {
					rec.Items = append(rec.Items, it)
				}
			}
			applied.Harnesses[s.name] = rec

			return commit()
		}
		for _, c := range s.first {
			out, err := p.env.runClaude(c)
			if err != nil {
				if !req.Uninstall || !slices.Equal(c, claudeRemoveArgs) {
					if bytes.Contains(bytes.ToLower(out), []byte("already exists")) {
						return &Refusal{
							Problem: "Claude Code already has an MCP server named agentfeedback, which agentfeedback install did not add",
							Next:    "remove it with claude mcp remove agentfeedback --scope user, then run agentfeedback install again",
						}
					}

					return fmt.Errorf("claude %s failed: %v: %s", strings.Join(c[:2], " "), err, strings.TrimSpace(string(out)))
				}
				p.notes[s.name] = append(p.notes[s.name], "claude mcp remove failed ("+strings.TrimSpace(string(out))+"); "+claudeManual)
			}
			isRemove := slices.Equal(c, claudeRemoveArgs)
			if err := update(
				func(it Item) bool { return isRemove && it.Kind == KindClaudeMCP },
				func(it Item) bool { return !isRemove && it.Kind == KindClaudeMCP && slices.Equal(it.Args, c) },
			); err != nil {
				return err
			}
		}
		for _, path := range s.files {
			f := p.files[path]
			if f.changed() || f.backupNew {
				if err := changedSince(f); err != nil {
					return err
				}
			}
			backup := ""
			if f.backupNew {
				if err := writeExclusive(path+BackupSuffix, f.orig, f.mode); err != nil {
					return &FileError{Path: path + BackupSuffix, Err: err}
				}
				backup = path + BackupSuffix
				w.backups = append(w.backups, backup)
			}
			// fail drops the backup this run wrote for a file it then could
			// not write.
			fail := func(err error) error {
				if backup != "" && os.Remove(backup) == nil {
					w.backups = slices.DeleteFunc(w.backups, func(b string) bool { return b == backup })
				}
				var fe *FileError
				var r *Refusal
				if !errors.As(err, &fe) && !errors.As(err, &r) {
					err = &FileError{Path: path, Err: err}
				}

				return err
			}
			if f.changed() {
				if f.present {
					created, err := mkdirs(filepath.Dir(path), 0o755)
					applied.DirsCreated = append(applied.DirsCreated, created...)
					if err == nil {
						err = writeAtomic(f)
					}
					if err != nil {
						if len(created) > 0 {
							_, _ = writeManifest(mpath, applied)
						}

						return fail(err)
					}
				} else if err := removeChecked(f); err != nil {
					return fail(err)
				}
				w.changed = append(w.changed, path)
			}
			if f.backupDelete != "" {
				if err := os.Remove(f.backupDelete); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return &FileError{Path: f.backupDelete, Err: err}
				}
			}
			if fr := p.man.Files[path]; fr != nil {
				c := *fr
				applied.Files[path] = &c
			} else {
				delete(applied.Files, path)
			}
			inFile := func(it Item) bool { return it.File == path }
			if err := update(inFile, inFile); err != nil {
				return err
			}
		}
		if s.record != nil {
			applied.Harnesses[s.name] = s.record
		} else {
			delete(applied.Harnesses, s.name)
		}
		if !req.Uninstall {
			applied.Server, applied.Binary = p.man.Server, p.man.Binary
		}
		if err := commit(); err != nil {
			return err
		}
	}
	// Directories install created and that are now empty go, deepest first.
	var left []string
	for i := len(applied.DirsCreated) - 1; i >= 0; i-- {
		d := applied.DirsCreated[i]
		if entries, err := os.ReadDir(d); err == nil && len(entries) == 0 {
			if os.Remove(d) == nil {
				continue
			}
		} else if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		left = append([]string{d}, left...)
	}
	if !slices.Equal(left, applied.DirsCreated) && len(applied.Harnesses) > 0 {
		applied.DirsCreated = left
		_, err := writeManifest(mpath, applied)

		return err
	}

	return nil
}

// writeFirst writes the manifest before anything else of the run, creating
// its directory and recording the directories it created.
func writeFirst(path string, m *Manifest) error {
	created, err := writeManifest(path, m)
	m.DirsCreated = append(m.DirsCreated, created...)
	if err == nil && len(created) > 0 {
		_, err = writeManifest(path, m)
	}

	return err
}

// writeManifest writes m to path and returns the directories it created
// for it; m itself is not changed.
func writeManifest(path string, m *Manifest) ([]string, error) {
	created, err := mkdirs(filepath.Dir(path), 0o700)
	if err != nil {
		return created, &FileError{Path: path, Err: err}
	}
	f := &fileState{path: path, mode: 0o600, present: true, cur: m.encode()}
	if data, err := os.ReadFile(path); err == nil {
		f.exists, f.orig = true, data
	}

	if err := writeAtomic(f); err != nil {
		return created, err
	}

	return created, syncDir(filepath.Dir(path))
}

// syncDir makes a rename in dir durable; a variable so tests can see it
// called.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return &FileError{Path: dir, Err: err}
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return &FileError{Path: dir, Err: err}
	}

	return nil
}

// mkdirs creates dir and its missing parents and returns the ones it
// created, shallowest first.
func mkdirs(dir string, perm fs.FileMode) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		}
		missing = append([]string{d}, missing...)
		if filepath.Dir(d) == d {
			break
		}
	}
	var created []string
	for _, d := range missing {
		if err := os.Mkdir(d, perm); err != nil && !errors.Is(err, fs.ErrExist) {
			return created, err
		}
		created = append(created, d)
	}

	return created, nil
}

func changedSince(f *fileState) error {
	changed := &Refusal{Problem: f.path + " changed while installing", Next: "run agentfeedback install or uninstall again"}
	// A link retargeted, or a file replaced by a link, is a change too.
	if f.target != "" {
		if t, err := filepath.EvalSymlinks(f.path); err != nil || t != f.target {
			return changed
		}
	} else if info, err := os.Lstat(f.path); f.planned && err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return changed
	}
	data, err := os.ReadFile(f.dest())
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &FileError{Path: f.path, Err: err}
	}
	if exists != f.exists || !bytes.Equal(data, f.orig) {
		return changed
	}

	return nil
}

// writeAtomic writes f.cur through a temporary file in the same directory,
// keeping f.mode, and refuses when the file changed since it was read. A
// linked file is written in its target's directory and renamed onto the
// target, so the link stays.
func writeAtomic(f *fileState) error {
	dest := f.dest()
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+"-*")
	if err != nil {
		return &FileError{Path: f.path, Err: err}
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	_, err = tmp.Write(f.cur)
	if err == nil {
		err = tmp.Chmod(f.mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return &FileError{Path: f.path, Err: err}
	}
	if beforeRename != nil {
		beforeRename(f.path)
	}
	if err := changedSince(f); err != nil {
		return err
	}
	if err := os.Rename(name, dest); err != nil {
		return &FileError{Path: f.path, Err: err}
	}

	return syncDir(filepath.Dir(dest))
}

// beforeRename, when set, runs just before writeAtomic's final check; tests
// use it to change a file under the run.
var beforeRename func(path string)

func removeChecked(f *fileState) error {
	if err := changedSince(f); err != nil {
		return err
	}
	if err := os.Remove(f.dest()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &FileError{Path: f.path, Err: err}
	}

	return nil
}

// writeExclusive creates path with data and mode and syncs it; an existing
// name is an error.
func writeExclusive(path string, data []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(path, mode)
	}

	return err
}

func (p *plan) statuses(namesWanted []string) []HarnessStatus {
	out := make([]HarnessStatus, 0, len(namesWanted))
	for _, name := range namesWanted {
		hs := p.status(name)
		for _, n := range p.notes[name] {
			if !slices.Contains(hs.Notes, n) {
				hs.Notes = append(hs.Notes, n)
			}
		}
		out = append(out, hs)
	}

	return out
}

func (p *plan) status(name string) HarnessStatus {
	hs := HarnessStatus{Name: name, Mode: "-", Skill: "-", MCP: "-", Hook: "-", Reminder: "-", Docs: "-"}
	rec := p.man.Harnesses[name]
	if rec == nil {
		return hs
	}
	hs.Mode = rec.Mode
	// A wired MCP entry reads as its kind, taken from the recorded item: the
	// record's Binary lags behind the items while a switch between a URL and
	// a stdio entry is partly applied. Without CLI-mode hooks or a stdio
	// entry no binary runs.
	if rec.Mode == ModeCLI {
		hs.Binary = rec.Binary
	}
	wired := map[string]string{RoleMCP: "url"}
	for _, it := range rec.Items {
		if it.Role != RoleMCP {
			continue
		}
		if bin, ok := stdioCommand(it); ok {
			wired[RoleMCP] = "stdio"
			hs.Binary = cmp.Or(bin, rec.Binary)
		}
	}
	col := map[string]*string{RoleSkill: &hs.Skill, RoleMCP: &hs.MCP, RoleHook: &hs.Hook, RoleReminder: &hs.Reminder, RoleDocs: &hs.Docs}
	for _, it := range rec.Items {
		c := col[it.Role]
		ok, err := p.present(it)
		switch {
		case err != nil || !ok:
			*c = "missing"
		case *c == "-":
			*c = "wired"
			if w := wired[it.Role]; w != "" {
				*c = w
			}
		}
	}

	return hs
}

// stdioCommand reports whether the MCP item records a stdio entry, and
// the command that entry runs when the item says. A Claude item carries
// Stdio; a JSON member or TOML block of a stdio entry has a command, which
// no URL entry has.
func stdioCommand(it Item) (string, bool) {
	switch it.Kind {
	case KindClaudeMCP:
		if len(it.Stdio) > 0 {
			return it.Stdio[0], true
		}
	case KindJSONMember:
		var v struct {
			Command json.RawMessage `json:"command"`
		}
		if json.Unmarshal(it.Value, &v) != nil || len(v.Command) == 0 {
			return "", false
		}
		var bin string
		if json.Unmarshal(v.Command, &bin) == nil {
			return bin, true
		}
		var argv []string
		if json.Unmarshal(v.Command, &argv) == nil && len(argv) > 0 {
			return argv[0], true
		}

		return "", true
	case KindTOMLBlock:
		var m struct {
			MCPServers map[string]struct {
				Command *string `toml:"command"`
			} `toml:"mcp_servers"`
		}
		if toml.Unmarshal([]byte(it.Text), &m) != nil {
			return "", false
		}
		if c := m.MCPServers["agentfeedback"].Command; c != nil {
			return *c, true
		}
	}

	return "", false
}

// Status reports every known harness from the manifest and the files on
// disk; it writes nothing.
func (e Env) Status() ([]HarnessStatus, error) {
	man, _, err := LoadManifest(e.ManifestPath())
	if err != nil {
		return nil, err
	}
	located, _ := e.withRecorded(man)
	p := newPlan(located, man)
	out := p.statuses(names)
	versions := map[string]string{}
	for i := range out {
		out[i].Detected = e.Detect(out[i].Name)
		if v, ok := Verification(out[i].Name); ok {
			out[i].Verification = &v
		}
		if e.Version == nil || out[i].Binary == "" {
			continue
		}
		v, ok := versions[out[i].Binary]
		if !ok {
			v = e.Version(out[i].Binary)
			versions[out[i].Binary] = v
		}
		out[i].BinaryVersion = v
	}

	return out, nil
}

// Recorded lists the harnesses the manifest records, in registry order.
func (e Env) Recorded() ([]string, error) {
	man, _, err := LoadManifest(e.ManifestPath())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if man.Harnesses[n] != nil {
			out = append(out, n)
		}
	}

	return out, nil
}

// Server is the server the manifest records, or "".
func (e Env) Server() (string, error) {
	man, _, err := LoadManifest(e.ManifestPath())
	if err != nil {
		return "", err
	}

	return man.Server, nil
}

// LegacyHooks lists, in registry order, the harnesses whose recorded hook
// still runs flush --hook, the entry installs wired before agentfeedback
// hook; a reinstall replaces it.
func (e Env) LegacyHooks() ([]string, error) {
	man, _, err := LoadManifest(e.ManifestPath())
	if err != nil {
		return nil, err
	}
	legacy := func(b []byte) bool {
		return bytes.Contains(b, []byte("flush --hook")) || bytes.Contains(b, []byte(`"flush", "--hook"`))
	}
	var out []string
	for _, n := range names {
		rec := man.Harnesses[n]
		if rec == nil || rec.Mode != ModeCLI {
			continue
		}
		for _, it := range rec.Items {
			if it.Role != RoleHook {
				continue
			}
			old := legacy(it.Value) || legacy([]byte(it.Text))
			if !old && it.Kind == KindPluginFile && it.File != "" {
				if data, err := os.ReadFile(it.File); err == nil {
					old = legacy(data)
				}
			}
			if old {
				out = append(out, n)

				break
			}
		}
	}

	return out, nil
}
