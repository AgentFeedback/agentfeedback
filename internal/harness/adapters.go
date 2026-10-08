// Package harness wires the agentfeedback binary into the coding-agent
// harnesses it knows: a skill and the hooks running agentfeedback hook (or an
// MCP entry instead) per harness, recorded in a manifest so uninstall removes exactly what install
// added. User files are edited byte for byte and backed up before the first
// change.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

// Modes of a wired harness.
const (
	ModeCLI = "cli"
	ModeMCP = "mcp"
)

// Item kinds recorded in the manifest.
const (
	KindSkillFile  = "skill_file"
	KindJSONMember = "json_member"
	KindJSONElem   = "json_element"
	KindTOMLBlock  = "toml_block"
	KindPluginFile = "plugin_file"
	KindClaudeMCP  = "claude_mcp"
)

// Roles group items into the columns a status shows.
const (
	RoleSkill    = "skill"
	RoleMCP      = "mcp"
	RoleHook     = "hook"
	RoleReminder = "reminder"
	RoleDocs     = "docs"
)

// Env is everything the package reads from the machine; tests replace each
// part.
type Env struct {
	Home     string
	Getenv   func(string) string
	LookPath func(string) (string, error)
	// Exec runs a command and returns its combined output. setenv overrides
	// variables of the environment; an empty value unsets one.
	Exec func(ctx context.Context, setenv map[string]string, name string, args ...string) ([]byte, error)
	// Version, when set, reports the version of an agentfeedback binary;
	// Status fills HarnessStatus.BinaryVersion with it.
	Version func(path string) string

	// codexAt and claudeAt are the locations a manifest recorded, used
	// instead of the environment's for a harness it records; claudeAtSet
	// is whether CLAUDE_CONFIG_DIR was set at that install.
	codexAt, claudeAt string
	claudeAtSet       bool
}

// Names lists every harness the package knows.
func Names() []string { return append([]string(nil), names...) }

// Known reports whether name is a harness the package knows.
func Known(name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}

	return false
}

func (e Env) xdgConfig() string {
	if d := e.Getenv("XDG_CONFIG_HOME"); d != "" && filepath.IsAbs(d) {
		return d
	}

	return filepath.Join(e.Home, ".config")
}

// xdgCache is ${XDG_CACHE_HOME:-~/.cache}.
func (e Env) xdgCache() string {
	if d := e.Getenv("XDG_CACHE_HOME"); d != "" && filepath.IsAbs(d) {
		return d
	}

	return filepath.Join(e.Home, ".cache")
}

// SpawnErrorPath is the file a plugin writes when it cannot start the hook
// command for the harness.
func (e Env) SpawnErrorPath(name string) string {
	return filepath.Join(e.xdgCache(), "agentfeedback", "hooks", name+".spawn-error.json")
}

func (e Env) codexHome() string {
	if e.codexAt != "" {
		return e.codexAt
	}
	if d := e.Getenv("CODEX_HOME"); d != "" && filepath.IsAbs(d) {
		return d
	}

	return filepath.Join(e.Home, ".codex")
}

// claudeDir is ${CLAUDE_CONFIG_DIR:-~/.claude}; Claude Code keeps its
// settings and skills there.
func (e Env) claudeDir() string {
	if e.claudeAt != "" {
		return e.claudeAt
	}
	if e.claudeSet() {
		return e.Getenv("CLAUDE_CONFIG_DIR")
	}

	return filepath.Join(e.Home, ".claude")
}

// claudeSet reports whether CLAUDE_CONFIG_DIR is in force, even when it
// names ~/.claude: Claude Code then keeps .claude.json inside it.
func (e Env) claudeSet() bool {
	if e.claudeAt != "" {
		return e.claudeAtSet
	}
	d := e.Getenv("CLAUDE_CONFIG_DIR")

	return d != "" && filepath.IsAbs(d)
}

// claudeJSON is the file holding Claude Code's user-scope MCP servers:
// $CLAUDE_CONFIG_DIR/.claude.json when the variable is set, otherwise
// ~/.claude.json.
func (e Env) claudeJSON() string {
	if e.claudeSet() {
		return filepath.Join(e.claudeDir(), ".claude.json")
	}

	return filepath.Join(e.Home, ".claude.json")
}

// withRecorded returns e using the Codex and Claude Code locations the
// manifest recorded for the harnesses it records, with a note per harness
// whose location differs from the environment's.
func (e Env) withRecorded(m *Manifest) (Env, map[string]string) {
	notes := map[string]string{}
	if m.Harnesses["codex"] != nil && m.CodexHome != "" && m.CodexHome != e.codexHome() {
		e.codexAt = m.CodexHome
		notes["codex"] = "codex was installed with CODEX_HOME=" + m.CodexHome + "; using that location"
	}
	// A manifest without claude_config_dir_set names a directory other
	// than ~/.claude only when CLAUDE_CONFIG_DIR was set.
	set := m.ClaudeConfigDirSet || m.ClaudeConfigDir != filepath.Join(e.Home, ".claude")
	if m.Harnesses["claude-code"] != nil && m.ClaudeConfigDir != "" &&
		(m.ClaudeConfigDir != e.claudeDir() || set != e.claudeSet()) {
		e.claudeAt, e.claudeAtSet = m.ClaudeConfigDir, set
		if set {
			notes["claude-code"] = "claude-code was installed with CLAUDE_CONFIG_DIR=" + m.ClaudeConfigDir + "; using that location"
		} else {
			notes["claude-code"] = "claude-code was installed without CLAUDE_CONFIG_DIR; using " + m.ClaudeConfigDir + " and ~/.claude.json"
		}
	}

	return e, notes
}

// recordLocations stores the locations the run used for the harnesses the
// manifest keeps.
func (e Env) recordLocations(m *Manifest) {
	m.CodexHome, m.ClaudeConfigDir, m.ClaudeConfigDirSet = "", "", false
	if m.Harnesses["codex"] != nil {
		m.CodexHome = e.codexHome()
	}
	if m.Harnesses["claude-code"] != nil {
		m.ClaudeConfigDir, m.ClaudeConfigDirSet = e.claudeDir(), e.claudeSet()
	}
}

func (e Env) opencodeDir() string { return filepath.Join(e.xdgConfig(), "opencode") }

// ManifestPath is ${XDG_CONFIG_HOME:-~/.config}/agentfeedback/install.json.
func (e Env) ManifestPath() string {
	return filepath.Join(e.xdgConfig(), "agentfeedback", "install.json")
}

// Detect reports how the harness was found: "binary", "dir" or "no".
func (e Env) Detect(name string) string {
	a := adapterOf(name)
	if a == nil {
		return "no"
	}
	for _, b := range a.Binaries {
		p, err := e.LookPath(b)
		if err != nil {
			continue
		}
		// "agent" is a common name; it counts only when it resolves into a
		// Cursor installation.
		if name == "cursor" && b == "agent" {
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil || !strings.Contains(strings.ToLower(resolved), "cursor") {
				continue
			}
		}

		return "binary"
	}
	for _, d := range a.ConfigDirs(e) {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return "dir"
		}
	}

	return "no"
}

// SkillPath is the SKILL.md install writes for the harness, in its first
// skills directory; "" for a harness without one.
func (e Env) SkillPath(name string) string {
	d := e.skillDirs(name)
	if len(d) == 0 {
		return ""
	}

	return filepath.Join(d[0], "agentfeedback", "SKILL.md")
}

// SkillPaths is every SKILL.md install writes for the harness, one per
// skills directory.
func (e Env) SkillPaths(name string) []string {
	var out []string
	for _, d := range e.skillDirs(name) {
		out = append(out, filepath.Join(d, "agentfeedback", "SKILL.md"))
	}

	return out
}

// DocsDir is the agentfeedback-docs skill directory install --docs writes
// for the harness, in its first skills directory; "" for a harness without
// one.
func (e Env) DocsDir(name string) string {
	d := e.skillDirs(name)
	if len(d) == 0 {
		return ""
	}

	return filepath.Join(d[0], "agentfeedback-docs")
}

// docsFiles is the docs skill tree; a variable so tests can stand in for
// another binary's version of it.
var docsFiles = skillgen.Docs

func jsonString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)

	return bytes.TrimSpace(buf.Bytes())
}

// obj builds a compact JSON object from ordered key/value pairs; a value is
// raw JSON.
func obj(kv ...string) []byte {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jsonString(kv[i]))
		b.WriteByte(':')
		b.WriteString(kv[i+1])
	}
	b.WriteByte('}')

	return []byte(b.String())
}

func str(s string) string { return string(jsonString(s)) }

// commandHook is the Claude Code and Codex hook group running command.
func commandHook(command string) []byte {
	return obj("hooks", "["+string(obj("type", str("command"), "command", str(command), "timeout", "5"))+"]")
}

// desire is what install wants for one harness, plus the notes it reports.
type desire struct {
	items []Item
	notes []string
}

// Options is the requested wiring.
type Options struct {
	Mode     string
	Reminder bool
	// Docs adds the agentfeedback-docs skill, in either mode.
	Docs   bool
	Server string
	Binary string
}

// ServerLocal is the Server of the local target: no URL, the data-directory
// database. Its MCP entry is a stdio entry running Binary's mcp command.
const ServerLocal = "local"

// stdio reports whether the MCP entry for o is a stdio entry.
func (o Options) stdio() bool { return o.Server == ServerLocal }

// desired computes the items for name; files are read through p so the
// choice of an OpenCode config file sees the planned state.
func (p *plan) desired(name string, o Options) (desire, error) {
	d, err := p.wiring(name, o)
	if err != nil || !o.Docs {
		return d, err
	}
	files, err := docsFiles()
	if err != nil {
		return d, err
	}
	dirs := p.env.skillDirs(name)
	if len(dirs) == 0 {
		d.notes = append(d.notes, name+" has no skill directory; --docs added nothing for it")
	}
	for _, dir := range dirs {
		for _, f := range files {
			d.items = append(d.items, fileItem(KindSkillFile, RoleDocs, filepath.Join(dir, "agentfeedback-docs", filepath.FromSlash(f.Path)), f.Data))
		}
	}

	return d, nil
}

// wiring is the skill and hook, or the MCP entry, of the mode.
func (p *plan) wiring(name string, o Options) (desire, error) {
	var d desire
	a := adapterOf(name)
	if a == nil {
		return d, fmt.Errorf("unknown harness %q", name)
	}
	if r := a.refusal(o.Mode); r != nil {
		return d, r
	}
	if o.Mode == ModeMCP {
		if o.Reminder {
			d.notes = append(d.notes, "--with-reminder does not apply with --mcp: the server's MCP instructions replace the skill")
		}
		items, notes, err := a.mcp(p, o)
		if err != nil {
			return d, err
		}
		d.items = append(d.items, items...)
		d.notes = append(d.notes, notes...)

		return d, nil
	}

	skill, err := skillgen.Render(skillgen.FormSkillMD, "")
	if err != nil {
		return d, err
	}
	for _, dir := range a.SkillDirs(p.env) {
		d.items = append(d.items, fileItem(KindSkillFile, RoleSkill, filepath.Join(dir, "agentfeedback", "SKILL.md"), skill))
	}
	if a.hook != nil {
		items, notes := a.hook(p, o)
		d.items = append(d.items, items...)
		d.notes = append(d.notes, notes...)
	}
	if o.Reminder && !a.Hook.Reminder {
		d.notes = append(d.notes, "the session-start reminder is not supported for "+name+"; nothing was added for it")
	}

	return d, nil
}

// ManualMCP is the MCP entry install --mcp would add for the harness against
// server (a stdio entry running binary for ServerLocal), for wiring it by
// hand; it writes nothing. The OpenCode config file is chosen from what is
// on disk.
func (e Env) ManualMCP(name, server, binary string) (Item, error) {
	p := newPlan(e, newManifest())
	d, err := p.wiring(name, Options{Mode: ModeMCP, Server: server, Binary: binary})
	if err != nil {
		return Item{}, err
	}
	for _, it := range d.items {
		if it.Role == RoleMCP {
			return it, nil
		}
	}

	return Item{}, fmt.Errorf("no MCP entry for harness %q", name)
}

func member(file string, path []string, value []byte, role string) Item {
	return Item{Kind: KindJSONMember, Role: role, File: file, Path: path, Key: "agentfeedback", Value: value}
}

func element(file string, path []string, value []byte, role string) Item {
	return Item{Kind: KindJSONElem, Role: role, File: file, Path: path, Value: value}
}

func fileItem(kind, role, path string, content []byte) Item {
	return Item{Kind: kind, Role: role, File: path, SHA256: sha(content), content: content}
}

// opencodeConfig picks the OpenCode file for the MCP entry: the first of
// opencode.jsonc and opencode.json with a top-level mcp member, else an
// existing opencode.jsonc, else opencode.json.
func (p *plan) opencodeConfig() (string, error) {
	dir := p.env.opencodeDir()
	jsonc, plain := filepath.Join(dir, "opencode.jsonc"), filepath.Join(dir, "opencode.json")
	for _, f := range []string{jsonc, plain} {
		fs, err := p.load(f)
		if err != nil {
			return "", err
		}
		if !fs.present {
			continue
		}
		v, err := GetMember(fs.cur, nil, "mcp")
		if err != nil {
			return "", editErr(f, err)
		}
		if v != nil {
			return f, nil
		}
	}
	fs, err := p.load(jsonc)
	if err != nil {
		return "", err
	}
	if fs.present {
		return jsonc, nil
	}

	return plain, nil
}

// pluginHelper is the code every plugin and extension shares: run spawns
// the hook with the payload on stdin, collects its stdout and resolves with
// it, or with "" after 5 s or on any failure; a failure to start the hook
// is recorded in SPAWN_ERR for doctor. It never throws. typed adds the
// TypeScript annotations.
func pluginHelper(bin, spawnErr, harness string, typed bool) string {
	t := func(ts string) string {
		if typed {
			return ts
		}

		return ""
	}

	return `// Written by agentfeedback install; agentfeedback uninstall removes it.
// Runs agentfeedback hook on failed tool calls and at the end of a turn, and
// passes on the short note it prints.
import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";

const BIN = ` + string(jsonString(bin)) + `;
const SPAWN_ERR = ` + string(jsonString(spawnErr)) + `;
const HARNESS = ` + string(jsonString(harness)) + `;
const MAX_OUT = 65536;

function spawnError(err` + t(": unknown") + `)` + t(": void") + ` {
  try {
    mkdirSync(dirname(SPAWN_ERR), { recursive: true, mode: 0o700 });
    writeFileSync(SPAWN_ERR, JSON.stringify({ ts: new Date().toISOString(), error: String(err) }) + "\n", { mode: 0o600 });
  } catch {}
}

function run(event` + t(": string") + `, payload` + t(": unknown") + `)` + t(": Promise<string>") + ` {
  return new Promise((resolve) => {
    let out = "";
    let done = false;
    let child` + t(": any") + `;
    let timer` + t(": any") + `;
    const finish = (text` + t(": string") + `) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      resolve(text);
    };
    timer = setTimeout(() => {
      try {
        child?.kill();
      } catch {}
      finish("");
    }, 5000);
    try {
      child = spawn(BIN, ["hook", HARNESS, event], { stdio: ["pipe", "pipe", "ignore"] });
      child.on("error", (err` + t(": unknown") + `) => {
        spawnError(err);
        finish("");
      });
      child.stdout.on("data", (d` + t(": any") + `) => {
        if (out.length < MAX_OUT) out += String(d);
      });
      child.on("close", () => finish(out.slice(0, MAX_OUT)));
      child.stdin.on("error", () => {});
      child.stdin.end(JSON.stringify(payload ?? {}));
    } catch (err) {
      spawnError(err);
      finish("");
    }
  });
}

// note is the additionalContext of the hook's output, or "".
function note(out` + t(": string") + `)` + t(": string") + ` {
  try {
    const v = JSON.parse(out);
    return typeof v?.additionalContext === "string" ? v.additionalContext : "";
  } catch {
    return "";
  }
}
`
}

func opencodePlugin(bin, spawnErr string) []byte {
	return []byte(pluginHelper(bin, spawnErr, "opencode", false) + `
export const AgentFeedback = async ({ client, directory }) => ({
  "tool.execute.after": async (input, output) => {
    if (input?.tool !== "bash") return;
    const exit = output?.metadata?.exit;
    if (typeof exit !== "number" || exit === 0) return;
    await run("tool.execute.after", { sessionID: input.sessionID, tool: input.tool, args: input.args, exit, cwd: directory });
  },
  event: async ({ event }) => {
    if (event?.type !== "session.idle") return;
    const sessionID = event.properties?.sessionID;
    const text = note(await run("session.idle", { sessionID, cwd: directory }));
    if (!text || !sessionID) return;
    try {
      await client.session.prompt({ path: { id: sessionID }, body: { noReply: true, parts: [{ type: "text", text }] } });
    } catch {}
  },
});
`)
}

func ompExtension(bin, spawnErr string) []byte {
	return []byte(pluginHelper(bin, spawnErr, "omp", true) + `
export default function (pi: any) {
  pi.on("tool_result", async (event: any, ctx: any) => {
    if (!event?.isError) return;
    const text = note(
      await run("tool_result", {
        session_id: ctx?.sessionManager?.getSessionId?.(),
        tool: event.toolName,
        input: event.input,
        cwd: ctx?.cwd ?? process.cwd(),
        is_error: true,
      }),
    );
    if (text) return { additionalContext: text };
  });
  pi.on("agent_end", (_event: unknown, ctx: any) => {
    if (ctx?.agent?.kind === "sub") return;
    void run("agent_end", { session_id: ctx?.sessionManager?.getSessionId?.(), cwd: ctx?.cwd ?? process.cwd() });
  });
}
`)
}

func piExtension(bin, spawnErr string) []byte {
	return []byte(pluginHelper(bin, spawnErr, "pi", true) + `
export default function (pi: any) {
  pi.on("tool_result", async (event: any, ctx: any) => {
    if (!event?.isError) return;
    const text = note(
      await run("tool_result", {
        session_id: ctx?.sessionManager?.getSessionId?.(),
        tool: event.toolName,
        input: event.input,
        cwd: ctx?.cwd ?? process.cwd(),
        is_error: true,
      }),
    );
    if (!text) return;
    try {
      pi.sendMessage({ customType: "agentfeedback", content: text, display: false }, { deliverAs: "nextTurn" });
    } catch {}
  });
  pi.on("agent_settled", (_event: unknown, ctx: any) => {
    void run("agent_settled", { session_id: ctx?.sessionManager?.getSessionId?.(), cwd: ctx?.cwd ?? process.cwd() });
  });
}
`)
}

// candidateFiles lists every file install can write for any harness under
// this environment.
func (e Env) candidateFiles() []string {
	var out []string
	for _, a := range registry {
		config, owned := a.files(e)
		out = append(out, config...)
		out = append(out, owned...)
		for _, d := range a.SkillDirs(e) {
			out = append(out, filepath.Join(d, "agentfeedback", "SKILL.md"))
		}
	}

	return out
}

// ancestors lists the directories above path up to, not including, home or
// the filesystem root.
func ancestors(path, home string) []string {
	var out []string
	for d := filepath.Dir(path); d != home && filepath.Dir(d) != d; d = filepath.Dir(d) {
		out = append(out, d)
	}

	return out
}

// validateManifest refuses a manifest that names a path install never
// writes, so uninstall cannot be pointed at an arbitrary file.
func (e Env) validateManifest(m *Manifest, mpath string) error {
	files := map[string]bool{}
	backups := map[string]bool{}
	dirs := map[string]bool{}
	for _, f := range e.candidateFiles() {
		files[f] = true
		backups[f+BackupSuffix] = true
		for _, d := range ancestors(f, e.Home) {
			dirs[d] = true
		}
	}
	for _, d := range ancestors(mpath, e.Home) {
		dirs[d] = true
	}
	// The docs skill's files follow the binary's version, so any clean path
	// beneath a docs directory is accepted, and so are its directories.
	var docsDirs []string
	for _, a := range registry {
		for _, sd := range a.SkillDirs(e) {
			dd := filepath.Join(sd, "agentfeedback-docs")
			docsDirs = append(docsDirs, dd)
			dirs[dd] = true
			for _, d := range ancestors(dd, e.Home) {
				dirs[d] = true
			}
		}
	}
	underDocs := func(x string) bool {
		if !filepath.IsAbs(x) || filepath.Clean(x) != x {
			return false
		}
		for _, dd := range docsDirs {
			if strings.HasPrefix(x, dd+string(filepath.Separator)) {
				return true
			}
		}

		return false
	}
	bad := func(x string) error {
		return &Refusal{
			Problem: "the install manifest " + mpath + " names " + x + ", which agentfeedback install does not write under this environment",
			Next:    "run with the HOME, XDG_CONFIG_HOME, CODEX_HOME and CLAUDE_CONFIG_DIR of the install, or fix the manifest",
		}
	}
	for _, n := range sortedKeys(m.Harnesses) {
		for _, it := range m.Harnesses[n].Items {
			if it.File == "" || files[it.File] {
				continue
			}
			// Only the docs skill's own files lie beneath a docs directory.
			if !underDocs(it.File) || it.Kind != KindSkillFile || it.Role != RoleDocs {
				return bad(it.File)
			}
		}
	}
	for _, path := range sortedKeys(m.Files) {
		if !files[path] && !underDocs(path) {
			return bad(path)
		}
		if b := m.Files[path].Backup; b != "" && !backups[b] && (b != path+BackupSuffix || !underDocs(path)) {
			return bad(b)
		}
	}
	for _, d := range m.DirsCreated {
		if !dirs[d] && !underDocs(d) {
			return bad(d)
		}
	}

	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)

	return out
}
