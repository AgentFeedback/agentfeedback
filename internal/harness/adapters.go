// Package harness wires the agentfeedback binary into the coding-agent
// harnesses it knows: a skill and a Stop hook (or an MCP entry instead) per
// harness, recorded in a manifest so uninstall removes exactly what install
// added. User files are edited byte for byte and backed up before the first
// change.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentfeedback/agentfeedback/internal/skillgen"
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

	// codexAt and claudeAt are the locations a manifest recorded, used
	// instead of the environment's for a harness it records.
	codexAt, claudeAt string
}

// names is the registry in display order.
var names = []string{"claude-code", "codex", "cursor", "opencode", "omp", "pi"}

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
	if d := e.Getenv("CLAUDE_CONFIG_DIR"); d != "" && filepath.IsAbs(d) {
		return d
	}

	return filepath.Join(e.Home, ".claude")
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
	if m.Harnesses["claude-code"] != nil && m.ClaudeConfigDir != "" && m.ClaudeConfigDir != e.claudeDir() {
		e.claudeAt = m.ClaudeConfigDir
		notes["claude-code"] = "claude-code was installed with CLAUDE_CONFIG_DIR=" + m.ClaudeConfigDir + "; using that location"
	}

	return e, notes
}

// recordLocations stores the locations the run used for the harnesses the
// manifest keeps.
func (e Env) recordLocations(m *Manifest) {
	m.CodexHome, m.ClaudeConfigDir = "", ""
	if m.Harnesses["codex"] != nil {
		m.CodexHome = e.codexHome()
	}
	if m.Harnesses["claude-code"] != nil {
		m.ClaudeConfigDir = e.claudeDir()
	}
}

func (e Env) opencodeDir() string { return filepath.Join(e.xdgConfig(), "opencode") }

// ManifestPath is ${XDG_CONFIG_HOME:-~/.config}/agentfeedback/install.json.
func (e Env) ManifestPath() string {
	return filepath.Join(e.xdgConfig(), "agentfeedback", "install.json")
}

// configDir is the directory whose presence marks the harness as installed.
func (e Env) configDir(name string) string {
	switch name {
	case "claude-code":
		return e.claudeDir()
	case "codex":
		return e.codexHome()
	case "cursor":
		return filepath.Join(e.Home, ".cursor")
	case "opencode":
		return e.opencodeDir()
	case "omp":
		return filepath.Join(e.Home, ".omp")
	case "pi":
		return filepath.Join(e.Home, ".pi")
	}

	return ""
}

func binaries(name string) []string {
	switch name {
	case "claude-code":
		return []string{"claude"}
	case "codex":
		return []string{"codex"}
	case "cursor":
		return []string{"cursor-agent", "agent"}
	}

	return []string{name}
}

// Detect reports how the harness was found: "binary", "dir" or "no".
func (e Env) Detect(name string) string {
	for _, b := range binaries(name) {
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
	if info, err := os.Stat(e.configDir(name)); err == nil && info.IsDir() {
		return "dir"
	}

	return "no"
}

// SkillPath is the SKILL.md install writes for the harness.
func (e Env) SkillPath(name string) string {
	var dir string
	switch name {
	case "claude-code":
		dir = filepath.Join(e.claudeDir(), "skills")
	case "codex":
		dir = filepath.Join(e.Home, ".agents", "skills")
	case "cursor":
		dir = filepath.Join(e.Home, ".cursor", "skills")
	case "opencode":
		dir = filepath.Join(e.opencodeDir(), "skills")
	case "omp":
		dir = filepath.Join(e.Home, ".omp", "agent", "skills")
	case "pi":
		dir = filepath.Join(e.Home, ".pi", "agent", "skills")
	}

	return filepath.Join(dir, "agentfeedback", "SKILL.md")
}

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
	Server   string
	Binary   string
}

// desired computes the items for name; files are read through p so the
// choice of an OpenCode config file sees the planned state.
func (p *plan) desired(name string, o Options) (desire, error) {
	e := p.env
	var d desire
	flush := o.Binary + " flush --hook"
	remind := o.Binary + " skill reminder"
	mcpURL := o.Server + "/mcp"
	if o.Mode == ModeMCP {
		if o.Reminder {
			d.notes = append(d.notes, "--with-reminder does not apply with --mcp: the server's MCP instructions replace the skill")
		}
		switch name {
		case "claude-code":
			cfg := obj("type", str("http"), "url", str(mcpURL), "headers", string(obj("Authorization", str("Bearer ${AGENT_FEEDBACK_API_KEY}"))))
			d.items = append(d.items, Item{Kind: KindClaudeMCP, Role: RoleMCP, Args: []string{"mcp", "add-json", "agentfeedback", string(cfg), "--scope", "user"}, URL: mcpURL})
		case "codex":
			d.items = append(d.items, Item{Kind: KindTOMLBlock, Role: RoleMCP, File: filepath.Join(e.codexHome(), "config.toml"), Text: codexBlock(o.Server)})
		case "cursor":
			v := obj("url", str(mcpURL), "headers", string(obj("Authorization", str("Bearer ${env:AGENT_FEEDBACK_API_KEY}"))))
			d.items = append(d.items, member(filepath.Join(e.Home, ".cursor", "mcp.json"), []string{"mcpServers"}, v))
		case "opencode":
			v := obj("type", str("remote"), "url", str(mcpURL), "enabled", "true", "oauth", "false",
				"headers", string(obj("Authorization", str("Bearer {env:AGENT_FEEDBACK_API_KEY}"))))
			file, err := p.opencodeConfig()
			if err != nil {
				return d, err
			}
			d.items = append(d.items, member(file, []string{"mcp"}, v))
		case "omp":
			v := obj("type", str("http"), "url", str(mcpURL), "headers", string(obj("Authorization", str("Bearer ${AGENT_FEEDBACK_API_KEY}"))))
			d.items = append(d.items, member(filepath.Join(e.Home, ".omp", "agent", "mcp.json"), []string{"mcpServers"}, v))
		case "pi":
			v := obj("url", str(mcpURL), "headers", string(obj("Authorization", str("Bearer ${AGENT_FEEDBACK_API_KEY}"))))
			d.items = append(d.items, member(filepath.Join(e.Home, ".pi", "agent", "mcp.json"), []string{"mcpServers"}, v))
			d.notes = append(d.notes, "the MCP entry needs pi 0.99.0 or later")
		}

		return d, nil
	}

	skill, err := skillgen.Render(skillgen.FormSkillMD, "")
	if err != nil {
		return d, err
	}
	d.items = append(d.items, fileItem(KindSkillFile, RoleSkill, e.SkillPath(name), skill))
	switch name {
	case "claude-code":
		f := filepath.Join(e.claudeDir(), "settings.json")
		d.items = append(d.items, element(f, []string{"hooks", "Stop"}, commandHook(flush), RoleHook))
		if o.Reminder {
			d.items = append(d.items, element(f, []string{"hooks", "SessionStart"}, commandHook(remind), RoleReminder))
		}
	case "codex":
		f := filepath.Join(e.codexHome(), "hooks.json")
		d.items = append(d.items, element(f, []string{"hooks", "Stop"}, commandHook(flush), RoleHook))
		if o.Reminder {
			d.items = append(d.items, element(f, []string{"hooks", "SessionStart"}, commandHook(remind), RoleReminder))
		}
		d.notes = append(d.notes, "Codex runs a new hook only after you trust it in /hooks")
	case "cursor":
		f := filepath.Join(e.Home, ".cursor", "hooks.json")
		d.items = append(d.items, element(f, []string{"hooks", "stop"}, obj("command", str(flush), "timeout", "5"), RoleHook))
		if o.Reminder {
			d.items = append(d.items, element(f, []string{"hooks", "sessionStart"}, obj("command", str(remind), "timeout", "5"), RoleReminder))
		}
	case "opencode":
		d.items = append(d.items, fileItem(KindPluginFile, RoleHook, filepath.Join(e.opencodeDir(), "plugins", "agentfeedback.js"), opencodePlugin(o.Binary)))
	case "omp":
		d.items = append(d.items, fileItem(KindPluginFile, RoleHook, filepath.Join(e.Home, ".omp", "agent", "extensions", "agentfeedback.ts"), ompExtension(o.Binary)))
	case "pi":
		d.items = append(d.items, fileItem(KindPluginFile, RoleHook, filepath.Join(e.Home, ".pi", "agent", "extensions", "agentfeedback.ts"), piExtension(o.Binary)))
	}
	switch name {
	case "opencode", "omp", "pi":
		if o.Reminder {
			d.notes = append(d.notes, "the session-start reminder is not supported for "+name+"; nothing was added for it")
		}
	}

	return d, nil
}

func member(file string, path []string, value []byte) Item {
	return Item{Kind: KindJSONMember, Role: RoleMCP, File: file, Path: path, Key: "agentfeedback", Value: value}
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

const pluginHeader = `// Written by agentfeedback install; agentfeedback uninstall removes it.
// Sends the spooled AgentFeedback submissions when a session goes idle.
import { spawn } from "node:child_process";

const BIN = `

func opencodePlugin(bin string) []byte {
	return []byte(pluginHeader + string(jsonString(bin)) + `;

export const AgentFeedbackFlush = async () => ({
  event: async ({ event }) => {
    if (event?.type !== "session.idle") return;
    try {
      const child = spawn(BIN, ["flush", "--hook"], { stdio: "ignore", detached: true });
      child.on("error", () => {});
      child.unref();
    } catch {}
  },
});
`)
}

func ompExtension(bin string) []byte {
	return []byte(pluginHeader + string(jsonString(bin)) + `;

export default function (pi: any) {
  pi.on("agent_end", (_event: unknown, ctx: any) => {
    if (ctx?.agent?.kind === "sub") return;
    try {
      const child = spawn(BIN, ["flush", "--hook"], { stdio: "ignore", detached: true });
      child.on("error", () => {});
      child.unref();
    } catch {}
  });
}
`)
}

func piExtension(bin string) []byte {
	return []byte(pluginHeader + string(jsonString(bin)) + `;

export default function (pi: any) {
  pi.on("agent_settled", () => {
    try {
      const child = spawn(BIN, ["flush", "--hook"], { stdio: "ignore", detached: true });
      child.on("error", () => {});
      child.unref();
    } catch {}
  });
}
`)
}

// candidateFiles lists every file install can write for any harness under
// this environment.
func (e Env) candidateFiles() []string {
	oc := e.opencodeDir()
	out := []string{
		filepath.Join(e.claudeDir(), "settings.json"),
		filepath.Join(e.codexHome(), "hooks.json"),
		filepath.Join(e.codexHome(), "config.toml"),
		filepath.Join(e.Home, ".cursor", "mcp.json"),
		filepath.Join(e.Home, ".cursor", "hooks.json"),
		filepath.Join(oc, "opencode.jsonc"),
		filepath.Join(oc, "opencode.json"),
		filepath.Join(oc, "plugins", "agentfeedback.js"),
		filepath.Join(e.Home, ".omp", "agent", "mcp.json"),
		filepath.Join(e.Home, ".omp", "agent", "extensions", "agentfeedback.ts"),
		filepath.Join(e.Home, ".pi", "agent", "mcp.json"),
		filepath.Join(e.Home, ".pi", "agent", "extensions", "agentfeedback.ts"),
	}
	for _, n := range names {
		out = append(out, e.SkillPath(n))
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
	bad := func(x string) error {
		return &Refusal{
			Problem: "the install manifest " + mpath + " names " + x + ", which agentfeedback install does not write under this environment",
			Next:    "run with the HOME, XDG_CONFIG_HOME, CODEX_HOME and CLAUDE_CONFIG_DIR of the install, or fix the manifest",
		}
	}
	for _, n := range sortedKeys(m.Harnesses) {
		for _, it := range m.Harnesses[n].Items {
			if it.File != "" && !files[it.File] {
				return bad(it.File)
			}
		}
	}
	for _, path := range sortedKeys(m.Files) {
		if !files[path] {
			return bad(path)
		}
		if b := m.Files[path].Backup; b != "" && !backups[b] {
			return bad(b)
		}
	}
	for _, d := range m.DirsCreated {
		if !dirs[d] {
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
