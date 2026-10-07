package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Verification levels of an adapter record.
const (
	LevelDocumented    = "documented"
	LevelFixtureTested = "fixture-tested"
	LevelLiveChecked   = "live-checked"
)

// Hook events an adapter declares; the value is the harness's own event
// name, or "none".
const (
	EventToolFailed   = "tool_failed"
	EventTurnEnd      = "turn_end"
	EventSessionStart = "session_start"
)

// Instructions are the instruction files a harness reads; declared only,
// install writes none of them.
type Instructions struct {
	Global func(Env) []string
	// NoGlobal is why there is no global instruction file.
	NoGlobal string
	Project  []string
}

// Hook is how the harness runs a command at the end of a turn.
type Hook struct {
	File func(Env) string
	// Format is how install writes the hook, or "none".
	Format string
	// NoHook is why no hook is wired when Format is "none".
	NoHook string
	Events map[string]string
	// Reminder is whether the session-start reminder can be wired.
	Reminder bool
}

// MCP is the MCP entry install --mcp writes.
type MCP struct {
	File       func(Env) string
	Path       []string
	URLShape   string
	StdioShape string
	EnvRef     string
	// NoMCP is why there is no MCP entry.
	NoMCP string
}

// Sessions is where the harness keeps its session logs and the reader that
// understands them.
type Sessions struct {
	Locations []string
	Reader    string
	// NoReader is why Reader is "none".
	NoReader string
}

// Verified is how far an adapter record was checked against the harness.
type Verified struct {
	Level string `json:"level"`
	Date  string `json:"date"`
	Note  string `json:"note"`
}

// Adapter is everything the package knows about one harness.
type Adapter struct {
	Name       string
	Binaries   []string
	ConfigDirs func(Env) []string
	SkillDirs  func(Env) []string
	// NoSkill is why SkillDirs is empty.
	NoSkill      string
	Instructions Instructions
	Hook         Hook
	MCP          MCP
	Sessions     Sessions
	Caveats      []string
	Verified     Verified

	// hook is the CLI-mode items beside the skill, with notes.
	hook func(p *plan, o Options) ([]Item, []string)
	// mcp is the MCP-mode items, with notes; nil when there is no entry.
	mcp func(p *plan, o Options) ([]Item, []string, error)
	// mcpValue is the JSON (or TOML) of the MCP entry for a URL, and
	// mcpStdio that of the stdio entry running bin.
	mcpValue func(url string) string
	mcpStdio func(bin string) string
	// files lists the configuration files (JSON, JSONC, TOML) and the whole
	// files install owns, besides skills.
	files        func(Env) (config, owned []string)
	noCLI, noMCP *Refusal
	row          docsRow
}

// docsRow is the adapter's row of the harness table in docs/operate.md.
type docsRow struct{ detected, skill, hook, reminder, mcp string }

// goos is the operating system the VS Code paths follow; tests set it.
var goos = runtime.GOOS

const verifiedDate = "2026-10-07"

func documented() Verified { return Verified{Level: LevelDocumented, Date: verifiedDate} }

func dirs(f func(Env) string) func(Env) []string {
	return func(e Env) []string { return []string{f(e)} }
}

func homePath(parts ...string) func(Env) string {
	return func(e Env) string { return filepath.Join(append([]string{e.Home}, parts...)...) }
}

func events(toolFailed, turnEnd, sessionStart string) map[string]string {
	return map[string]string{EventToolFailed: toolFailed, EventTurnEnd: turnEnd, EventSessionStart: sessionStart}
}

func bearer(ref string) string {
	return string(obj("Authorization", str("Bearer "+ref)))
}

// mcpEntry is the MCP entry for o: the stdio entry for the local target,
// else the URL entry, with urlNotes for a URL entry only.
func (a *Adapter) mcpEntry(o Options, urlNotes []string) (string, []string) {
	if o.stdio() {
		return a.mcpStdio(o.Binary), nil
	}

	return a.mcpValue(o.Server + "/mcp"), urlNotes
}

// jsonMCP is an MCP entry written as the member agentfeedback at path of
// the file file picks; notes apply to both entries, urlNotes to a URL
// entry only.
func jsonMCP(a *Adapter, file func(p *plan) (string, error), notes, urlNotes []string) func(p *plan, o Options) ([]Item, []string, error) {
	return func(p *plan, o Options) ([]Item, []string, error) {
		f, err := file(p)
		if err != nil {
			return nil, nil, err
		}
		v, extra := a.mcpEntry(o, urlNotes)

		return []Item{member(f, a.MCP.Path, []byte(v), RoleMCP)}, append(slices.Clone(notes), extra...), nil
	}
}

// stdioArgs is the arguments of the stdio entry's command.
const stdioArgs = `["mcp"]`

// typedStdio is the stdio entry with a type member typ.
func typedStdio(typ string) func(bin string) string {
	return func(bin string) string {
		return string(obj("type", str(typ), "command", str(bin), "args", stdioArgs))
	}
}

// plainStdio is the stdio entry without a type member.
func plainStdio(bin string) string { return string(obj("command", str(bin), "args", stdioArgs)) }

func fixed(f func(Env) string) func(p *plan) (string, error) {
	return func(p *plan) (string, error) { return f(p.env), nil }
}

func flushCmd(o Options) string  { return o.Binary + " flush --hook" }
func remindCmd(o Options) string { return o.Binary + " skill reminder" }

func undocumentedEnvNote(name string) string {
	return name + " documents no environment-variable syntax for MCP headers; check that it connects (a 401 means the key reference was not expanded)"
}

func (e Env) copilotDir() string { return filepath.Join(e.Home, ".copilot") }
func (e Env) geminiDir() string  { return filepath.Join(e.Home, ".gemini") }
func (e Env) devinDir() string   { return filepath.Join(e.Home, ".config", "devin") }
func (e Env) ampDir() string     { return filepath.Join(e.xdgConfig(), "amp") }
func (e Env) clineMCPFile() string {
	return filepath.Join(e.Home, ".cline", "data", "settings", "cline_mcp_settings.json")
}
func (e Env) copilotHookFile() string {
	return filepath.Join(e.copilotDir(), "hooks", "agentfeedback.json")
}

// vscodeUserDir is VS Code's user directory.
func (e Env) vscodeUserDir() string {
	switch goos {
	case "darwin":
		return filepath.Join(e.Home, "Library", "Application Support", "Code", "User")
	case "windows":
		if d := e.Getenv("APPDATA"); d != "" && filepath.IsAbs(d) {
			return filepath.Join(d, "Code", "User")
		}

		return filepath.Join(e.Home, "AppData", "Roaming", "Code", "User")
	}

	return filepath.Join(e.xdgConfig(), "Code", "User")
}

// copilotHook is the hook file install writes for Copilot.
func copilotHook(bin string) []byte {
	compact := obj("version", "1", "hooks", string(obj("agentStop",
		"["+string(obj("type", str("command"), "bash", str(bin+" flush --hook"), "timeoutSec", "5"))+"]")))
	var buf bytes.Buffer
	_ = json.Indent(&buf, compact, "", "  ")
	buf.WriteByte('\n')

	return buf.Bytes()
}

// ampConfig picks Amp's settings file: settings.jsonc when it exists in
// the planned state, else settings.json.
func (p *plan) ampConfig() (string, error) {
	jsonc := filepath.Join(p.env.ampDir(), "settings.jsonc")
	fs, err := p.load(jsonc)
	if err != nil {
		return "", err
	}
	if fs.present {
		return jsonc, nil
	}

	return filepath.Join(p.env.ampDir(), "settings.json"), nil
}

// registry is every harness the package knows, in display order.
var registry = []Adapter{
	{
		Name:       "claude-code",
		Binaries:   []string{"claude"},
		ConfigDirs: dirs(Env.claudeDir),
		SkillDirs:  func(e Env) []string { return []string{filepath.Join(e.claudeDir(), "skills")} },
		Instructions: Instructions{
			Global:  func(e Env) []string { return []string{filepath.Join(e.claudeDir(), "CLAUDE.md")} },
			Project: []string{"CLAUDE.md"},
		},
		Hook: Hook{
			File:     func(e Env) string { return filepath.Join(e.claudeDir(), "settings.json") },
			Format:   "json-element",
			Events:   events("PostToolUseFailure", "Stop", "SessionStart"),
			Reminder: true,
		},
		MCP:      MCP{File: Env.claudeJSON, Path: []string{"mcpServers"}, EnvRef: "${AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Locations: []string{"<claude config>/projects/<cwd>/<uuid>.jsonl"}, Reader: "claude-code-jsonl"},
		Verified: Verified{Level: LevelLiveChecked, Date: verifiedDate, Note: "the just live-harness gate"},
		hook: func(p *plan, o Options) ([]Item, []string) {
			f := filepath.Join(p.env.claudeDir(), "settings.json")
			items := []Item{element(f, []string{"hooks", "Stop"}, commandHook(flushCmd(o)), RoleHook)}
			if o.Reminder {
				items = append(items, element(f, []string{"hooks", "SessionStart"}, commandHook(remindCmd(o)), RoleReminder))
			}

			return items, nil
		},
		mcpValue: func(u string) string {
			return string(obj("type", str("http"), "url", str(u), "headers", bearer("${AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: typedStdio("stdio"),
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.claudeDir(), "settings.json")}, nil
		},
		row: docsRow{
			detected: "`claude`, `$CLAUDE_CONFIG_DIR` (default `~/.claude`)",
			skill:    "`<claude config>/skills/agentfeedback/SKILL.md`",
			hook:     "`<claude config>/settings.json` `hooks.Stop`",
			reminder: "`hooks.SessionStart`",
			mcp:      "`claude mcp add-json agentfeedback ... --scope user`",
		},
	},
	{
		Name:       "codex",
		Binaries:   []string{"codex"},
		ConfigDirs: dirs(Env.codexHome),
		SkillDirs:  func(e Env) []string { return []string{filepath.Join(e.Home, ".agents", "skills")} },
		Instructions: Instructions{
			Global:  func(e Env) []string { return []string{filepath.Join(e.codexHome(), "AGENTS.md")} },
			Project: []string{"AGENTS.md"},
		},
		Hook: Hook{
			File:     func(e Env) string { return filepath.Join(e.codexHome(), "hooks.json") },
			Format:   "json-element",
			Events:   events("PostToolUse", "Stop", "SessionStart"),
			Reminder: true,
		},
		MCP:      MCP{File: func(e Env) string { return filepath.Join(e.codexHome(), "config.toml") }, Path: []string{"mcp_servers"}, EnvRef: "bearer_token_env_var"},
		Sessions: Sessions{Locations: []string{"<codex home>/sessions"}, Reader: "codex-rollout"},
		Verified: documented(),
		hook: func(p *plan, o Options) ([]Item, []string) {
			f := filepath.Join(p.env.codexHome(), "hooks.json")
			items := []Item{element(f, []string{"hooks", "Stop"}, commandHook(flushCmd(o)), RoleHook)}
			if o.Reminder {
				items = append(items, element(f, []string{"hooks", "SessionStart"}, commandHook(remindCmd(o)), RoleReminder))
			}

			return items, []string{"Codex runs a new hook only after you trust it in /hooks"}
		},
		mcp: func(p *plan, o Options) ([]Item, []string, error) {
			text := codexBlock(o.Server)
			if o.stdio() {
				text = codexStdioBlock(o.Binary)
			}

			return []Item{{Kind: KindTOMLBlock, Role: RoleMCP, File: filepath.Join(p.env.codexHome(), "config.toml"), Text: text}}, nil, nil
		},
		mcpValue: func(u string) string { return codexBlock(strings.TrimSuffix(u, "/mcp")) },
		mcpStdio: codexStdioBlock,
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.codexHome(), "hooks.json"), filepath.Join(e.codexHome(), "config.toml")}, nil
		},
		row: docsRow{
			detected: "`codex`, `$CODEX_HOME` (default `~/.codex`)",
			skill:    "`~/.agents/skills/agentfeedback/SKILL.md`",
			hook:     "`<codex home>/hooks.json` `hooks.Stop`",
			reminder: "`hooks.SessionStart`",
			mcp:      "marked `[mcp_servers.agentfeedback]` block in `<codex home>/config.toml`",
		},
	},
	{
		Name:       "cursor",
		Binaries:   []string{"cursor-agent", "agent"},
		ConfigDirs: dirs(homePath(".cursor")),
		SkillDirs:  dirs(homePath(".cursor", "skills")),
		Instructions: Instructions{
			NoGlobal: "Cursor's User Rules live in its settings UI, not in a file",
			Project:  []string{"AGENTS.md", ".cursor/rules/"},
		},
		Hook: Hook{
			File:     homePath(".cursor", "hooks.json"),
			Format:   "json-element",
			Events:   events("postToolUseFailure", "stop", "sessionStart"),
			Reminder: true,
		},
		MCP:      MCP{File: homePath(".cursor", "mcp.json"), Path: []string{"mcpServers"}, EnvRef: "${env:AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Locations: []string{"~/.cursor/projects/*/agent-transcripts/"}, Reader: "none", NoReader: "the transcript format is not settled yet"},
		Verified: documented(),
		hook: func(p *plan, o Options) ([]Item, []string) {
			f := filepath.Join(p.env.Home, ".cursor", "hooks.json")
			items := []Item{element(f, []string{"hooks", "stop"}, obj("command", str(flushCmd(o)), "timeout", "5"), RoleHook)}
			if o.Reminder {
				items = append(items, element(f, []string{"hooks", "sessionStart"}, obj("command", str(remindCmd(o)), "timeout", "5"), RoleReminder))
			}

			return items, nil
		},
		mcpValue: func(u string) string {
			return string(obj("url", str(u), "headers", bearer("${env:AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: typedStdio("stdio"),
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.Home, ".cursor", "mcp.json"), filepath.Join(e.Home, ".cursor", "hooks.json")}, nil
		},
		row: docsRow{
			detected: "`cursor-agent`, or an `agent` that resolves into a Cursor install; `~/.cursor`",
			skill:    "`~/.cursor/skills/agentfeedback/SKILL.md`",
			hook:     "`~/.cursor/hooks.json` `hooks.stop`",
			reminder: "`hooks.sessionStart`",
			mcp:      "`~/.cursor/mcp.json` `mcpServers.agentfeedback`",
		},
	},
	{
		Name:       "opencode",
		Binaries:   []string{"opencode"},
		ConfigDirs: dirs(Env.opencodeDir),
		SkillDirs:  func(e Env) []string { return []string{filepath.Join(e.opencodeDir(), "skills")} },
		Instructions: Instructions{
			Global:  func(e Env) []string { return []string{filepath.Join(e.opencodeDir(), "AGENTS.md")} },
			Project: []string{"AGENTS.md"},
		},
		Hook: Hook{
			File:   func(e Env) string { return filepath.Join(e.opencodeDir(), "plugins", "agentfeedback.js") },
			Format: "plugin",
			Events: events("tool.execute.after", "session.idle", "none"),
		},
		MCP:      MCP{File: func(e Env) string { return filepath.Join(e.opencodeDir(), "opencode.jsonc") }, Path: []string{"mcp"}, EnvRef: "{env:AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Locations: []string{"~/.local/share/opencode/opencode.db"}, Reader: "opencode-sqlite"},
		Verified: Verified{Level: LevelLiveChecked, Date: verifiedDate, Note: "OpenCode 1.18.34: opencode debug skill lists the skill and opencode mcp list connects the MCP entry; the plugin was not run in a session"},
		hook: func(p *plan, o Options) ([]Item, []string) {
			return []Item{fileItem(KindPluginFile, RoleHook, filepath.Join(p.env.opencodeDir(), "plugins", "agentfeedback.js"), opencodePlugin(o.Binary))}, nil
		},
		mcpValue: func(u string) string {
			return string(obj("type", str("remote"), "url", str(u), "enabled", "true", "oauth", "false",
				"headers", bearer("{env:AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: func(bin string) string {
			return string(obj("type", str("local"), "command", "["+str(bin)+","+str("mcp")+"]", "enabled", "true"))
		},
		files: func(e Env) ([]string, []string) {
			oc := e.opencodeDir()

			return []string{filepath.Join(oc, "opencode.jsonc"), filepath.Join(oc, "opencode.json")}, []string{filepath.Join(oc, "plugins", "agentfeedback.js")}
		},
		row: docsRow{
			detected: "`opencode`, `${XDG_CONFIG_HOME:-~/.config}/opencode`",
			skill:    "`<opencode>/skills/agentfeedback/SKILL.md`",
			hook:     "plugin `<opencode>/plugins/agentfeedback.js` (on `session.idle`)",
			reminder: "not supported",
			mcp:      "`mcp.agentfeedback` in the first of `opencode.jsonc`, `opencode.json` that has an `mcp` member, else an existing `opencode.jsonc`, else `opencode.json`",
		},
	},
	{
		Name:       "omp",
		Binaries:   []string{"omp"},
		ConfigDirs: dirs(homePath(".omp")),
		SkillDirs:  dirs(homePath(".omp", "agent", "skills")),
		Instructions: Instructions{
			Global:  dirs(homePath(".omp", "agent", "AGENTS.md")),
			Project: []string{"AGENTS.md"},
		},
		Hook: Hook{
			File:   homePath(".omp", "agent", "extensions", "agentfeedback.ts"),
			Format: "extension",
			Events: events("none", "agent_end", "none"),
		},
		MCP:      MCP{File: homePath(".omp", "agent", "mcp.json"), Path: []string{"mcpServers"}, EnvRef: "${AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Locations: []string{"~/.omp/agent/sessions"}, Reader: "none", NoReader: "no reader yet"},
		Verified: Verified{Level: LevelDocumented, Date: verifiedDate, Note: "omp 18.8.0 is installed on the delivery machine but lists neither MCP servers nor skills outside a model session"},
		hook: func(p *plan, o Options) ([]Item, []string) {
			return []Item{fileItem(KindPluginFile, RoleHook, filepath.Join(p.env.Home, ".omp", "agent", "extensions", "agentfeedback.ts"), ompExtension(o.Binary))}, nil
		},
		mcpValue: func(u string) string {
			return string(obj("type", str("http"), "url", str(u), "headers", bearer("${AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: typedStdio("stdio"),
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.Home, ".omp", "agent", "mcp.json")}, []string{filepath.Join(e.Home, ".omp", "agent", "extensions", "agentfeedback.ts")}
		},
		row: docsRow{
			detected: "`omp`, `~/.omp`",
			skill:    "`~/.omp/agent/skills/agentfeedback/SKILL.md`",
			hook:     "extension `~/.omp/agent/extensions/agentfeedback.ts` (on `agent_end`, not for subagents)",
			reminder: "not supported",
			mcp:      "`~/.omp/agent/mcp.json` `mcpServers.agentfeedback`",
		},
	},
	{
		Name:       "pi",
		Binaries:   []string{"pi"},
		ConfigDirs: dirs(homePath(".pi")),
		SkillDirs:  dirs(homePath(".pi", "agent", "skills")),
		Instructions: Instructions{
			Global:  dirs(homePath(".pi", "agent", "AGENTS.md")),
			Project: []string{"AGENTS.md"},
		},
		Hook: Hook{
			File:   homePath(".pi", "agent", "extensions", "agentfeedback.ts"),
			Format: "extension",
			Events: events("none", "agent_settled", "none"),
		},
		MCP:      MCP{File: homePath(".pi", "agent", "mcp.json"), Path: []string{"mcpServers"}, EnvRef: "${AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Locations: []string{"~/.pi/agent/sessions"}, Reader: "none", NoReader: "no reader yet"},
		Caveats:  []string{"the MCP entry needs pi 0.99.0 or later"},
		Verified: documented(),
		hook: func(p *plan, o Options) ([]Item, []string) {
			return []Item{fileItem(KindPluginFile, RoleHook, filepath.Join(p.env.Home, ".pi", "agent", "extensions", "agentfeedback.ts"), piExtension(o.Binary))}, nil
		},
		mcpValue: func(u string) string {
			return string(obj("url", str(u), "headers", bearer("${AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: plainStdio,
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.Home, ".pi", "agent", "mcp.json")}, []string{filepath.Join(e.Home, ".pi", "agent", "extensions", "agentfeedback.ts")}
		},
		row: docsRow{
			detected: "`pi`, `~/.pi`",
			skill:    "`~/.pi/agent/skills/agentfeedback/SKILL.md`",
			hook:     "extension `~/.pi/agent/extensions/agentfeedback.ts` (on `agent_settled`)",
			reminder: "not supported",
			mcp:      "`~/.pi/agent/mcp.json` `mcpServers.agentfeedback`; needs pi 0.99.0 or later",
		},
	},
	{
		Name:       "copilot",
		Binaries:   []string{"copilot"},
		ConfigDirs: dirs(Env.copilotDir),
		SkillDirs:  func(e Env) []string { return []string{filepath.Join(e.copilotDir(), "skills")} },
		Instructions: Instructions{
			Global:  func(e Env) []string { return []string{filepath.Join(e.copilotDir(), "copilot-instructions.md")} },
			Project: []string{".github/copilot-instructions.md", "AGENTS.md"},
		},
		Hook: Hook{
			File:   Env.copilotHookFile,
			Format: "whole-file",
			Events: events("postToolUseFailure", "agentStop", "sessionStart"),
		},
		MCP:      MCP{File: func(e Env) string { return filepath.Join(e.copilotDir(), "mcp-config.json") }, Path: []string{"mcpServers"}, EnvRef: "${AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Locations: []string{"~/.copilot/session-state/<id>/events.jsonl"}, Reader: "copilot-events"},
		Caveats:  []string{"COPILOT_HOME is not followed"},
		Verified: documented(),
		hook: func(p *plan, o Options) ([]Item, []string) {
			return []Item{fileItem(KindPluginFile, RoleHook, p.env.copilotHookFile(), copilotHook(o.Binary))}, nil
		},
		mcpValue: func(u string) string {
			return string(obj("type", str("http"), "url", str(u), "headers", bearer("${AGENT_FEEDBACK_API_KEY}"), "tools", `["*"]`))
		},
		mcpStdio: func(bin string) string {
			return string(obj("type", str("local"), "command", str(bin), "args", stdioArgs, "tools", `["*"]`))
		},
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.copilotDir(), "mcp-config.json")}, []string{e.copilotHookFile()}
		},
		row: docsRow{
			detected: "`copilot`, `~/.copilot`",
			skill:    "`~/.copilot/skills/agentfeedback/SKILL.md`",
			hook:     "file `~/.copilot/hooks/agentfeedback.json` (`agentStop`)",
			reminder: "not supported",
			mcp:      "`~/.copilot/mcp-config.json` `mcpServers.agentfeedback`",
		},
	},
	{
		Name:     "antigravity",
		Binaries: []string{"agy"},
		ConfigDirs: func(e Env) []string {
			return []string{filepath.Join(e.geminiDir(), "antigravity-cli"), filepath.Join(e.geminiDir(), "antigravity")}
		},
		SkillDirs: func(e Env) []string {
			return []string{filepath.Join(e.geminiDir(), "config", "skills"), filepath.Join(e.geminiDir(), "antigravity-cli", "skills")}
		},
		Instructions: Instructions{
			Global:  func(e Env) []string { return []string{filepath.Join(e.geminiDir(), "GEMINI.md")} },
			Project: []string{"GEMINI.md", "AGENTS.md"},
		},
		Hook: Hook{
			File:   func(e Env) string { return filepath.Join(e.geminiDir(), "config", "hooks.json") },
			Format: "json-member",
			Events: events("PostToolUse", "Stop", "none"),
		},
		MCP:      MCP{File: func(e Env) string { return filepath.Join(e.geminiDir(), "config", "mcp_config.json") }, Path: []string{"mcpServers"}, EnvRef: "${AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Locations: []string{"~/.gemini/antigravity*/brain/<id>/.system_generated/logs/transcript.jsonl"}, Reader: "antigravity-transcript"},
		Caveats:  []string{"the legacy Cascade configuration is not wired", "~/.gemini/GEMINI.md is shared with gemini-cli"},
		Verified: documented(),
		hook: func(p *plan, o Options) ([]Item, []string) {
			v := obj("enabled", "true", "Stop", "["+string(commandHook(flushCmd(o)))+"]")

			return []Item{member(filepath.Join(p.env.geminiDir(), "config", "hooks.json"), nil, v, RoleHook)}, nil
		},
		mcpValue: func(u string) string {
			return string(obj("serverUrl", str(u), "headers", bearer("${AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: plainStdio,
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.geminiDir(), "config", "hooks.json"), filepath.Join(e.geminiDir(), "config", "mcp_config.json")}, nil
		},
		row: docsRow{
			detected: "`agy`, `~/.gemini/antigravity-cli` or `~/.gemini/antigravity`",
			skill:    "`~/.gemini/config/skills/agentfeedback/SKILL.md` and `~/.gemini/antigravity-cli/skills/agentfeedback/SKILL.md`",
			hook:     "`~/.gemini/config/hooks.json` `agentfeedback` (on `Stop`)",
			reminder: "not supported",
			mcp:      "`~/.gemini/config/mcp_config.json` `mcpServers.agentfeedback`; the key reference in the header may not be expanded",
		},
	},
	{
		Name:       "devin",
		Binaries:   []string{"devin"},
		ConfigDirs: dirs(Env.devinDir),
		SkillDirs:  func(e Env) []string { return []string{filepath.Join(e.devinDir(), "skills")} },
		Instructions: Instructions{
			Global:  func(e Env) []string { return []string{filepath.Join(e.devinDir(), "AGENTS.md")} },
			Project: []string{"AGENTS.md"},
		},
		Hook: Hook{
			File:   func(e Env) string { return filepath.Join(e.devinDir(), "config.json") },
			Format: "json-element",
			Events: events("PostToolUse", "Stop", "SessionStart"),
		},
		MCP:      MCP{File: func(e Env) string { return filepath.Join(e.devinDir(), "mcp_config.json") }, Path: []string{"mcpServers"}, EnvRef: "${env:AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Reader: "none", NoReader: "Devin documents no local session location"},
		Caveats:  []string{"Devin also runs Claude Code's hooks from ~/.claude/settings.json, so a double flush is possible and harmless"},
		Verified: documented(),
		hook: func(p *plan, o Options) ([]Item, []string) {
			return []Item{element(filepath.Join(p.env.devinDir(), "config.json"), []string{"hooks", "Stop"}, commandHook(flushCmd(o)), RoleHook)}, nil
		},
		mcpValue: func(u string) string {
			return string(obj("url", str(u), "transport", str("http"), "headers", bearer("${env:AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: plainStdio,
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.devinDir(), "config.json"), filepath.Join(e.devinDir(), "mcp_config.json")}, nil
		},
		row: docsRow{
			detected: "`devin`, `~/.config/devin`",
			skill:    "`~/.config/devin/skills/agentfeedback/SKILL.md`",
			hook:     "`~/.config/devin/config.json` `hooks.Stop`",
			reminder: "not supported",
			mcp:      "`~/.config/devin/mcp_config.json` `mcpServers.agentfeedback`",
		},
	},
	{
		Name:       "kiro",
		Binaries:   []string{"kiro-cli"},
		ConfigDirs: dirs(homePath(".kiro")),
		SkillDirs:  dirs(homePath(".kiro", "skills")),
		Instructions: Instructions{
			Global:  dirs(homePath(".kiro", "steering", "AGENTS.md")),
			Project: []string{".kiro/steering/", "AGENTS.md"},
		},
		Hook:     Hook{Format: "none", NoHook: "the skill, the MCP entry and the instruction file carry it", Events: events("none", "none", "none")},
		MCP:      MCP{File: homePath(".kiro", "settings", "mcp.json"), Path: []string{"mcpServers"}, EnvRef: "${AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Locations: []string{"~/.kiro/sessions/"}, Reader: "none", NoReader: "no reader yet"},
		Caveats:  []string{"KIRO_HOME is not followed"},
		Verified: documented(),
		mcpValue: func(u string) string {
			return string(obj("url", str(u), "headers", bearer("${AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: plainStdio,
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.Home, ".kiro", "settings", "mcp.json")}, nil
		},
		row: docsRow{
			detected: "`kiro-cli`, `~/.kiro`",
			skill:    "`~/.kiro/skills/agentfeedback/SKILL.md`",
			hook:     "none",
			reminder: "not supported",
			mcp:      "`~/.kiro/settings/mcp.json` `mcpServers.agentfeedback`",
		},
	},
	{
		Name:       "cline",
		Binaries:   []string{"cline"},
		ConfigDirs: dirs(homePath(".cline")),
		SkillDirs:  dirs(homePath(".cline", "skills")),
		Instructions: Instructions{
			Global:  dirs(homePath(".cline", "rules", "agentfeedback.md")),
			Project: []string{".clinerules/"},
		},
		Hook:     Hook{Format: "none", NoHook: "the skill, the MCP entry and the instruction file carry it", Events: events("none", "none", "none")},
		MCP:      MCP{File: Env.clineMCPFile, Path: []string{"mcpServers"}, EnvRef: "${AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Locations: []string{"~/.cline/data/sessions/"}, Reader: "none", NoReader: "no reader yet; the documented locations disagree"},
		Verified: documented(),
		mcpValue: func(u string) string {
			return string(obj("type", str("streamableHttp"), "url", str(u), "headers", bearer("${AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: plainStdio,
		files:    func(e Env) ([]string, []string) { return []string{e.clineMCPFile()}, nil },
		row: docsRow{
			detected: "`cline`, `~/.cline`",
			skill:    "`~/.cline/skills/agentfeedback/SKILL.md`",
			hook:     "none",
			reminder: "not supported",
			mcp:      "`~/.cline/data/settings/cline_mcp_settings.json` `mcpServers.agentfeedback`; the key reference in the header may not be expanded",
		},
	},
	{
		Name:       "amp",
		Binaries:   []string{"amp"},
		ConfigDirs: dirs(Env.ampDir),
		SkillDirs:  func(e Env) []string { return []string{filepath.Join(e.ampDir(), "skills")} },
		Instructions: Instructions{
			Global:  func(e Env) []string { return []string{filepath.Join(e.ampDir(), "AGENTS.md")} },
			Project: []string{"AGENTS.md"},
		},
		Hook:     Hook{Format: "none", NoHook: "Amp hooks are TypeScript plugins, not wired yet", Events: events("none", "none", "none")},
		MCP:      MCP{File: func(e Env) string { return filepath.Join(e.ampDir(), "settings.json") }, Path: []string{"amp.mcpServers"}, EnvRef: "${AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Reader: "none", NoReader: "Amp keeps its threads on its server"},
		Verified: documented(),
		mcpValue: func(u string) string {
			return string(obj("url", str(u), "headers", bearer("${AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: plainStdio,
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.ampDir(), "settings.jsonc"), filepath.Join(e.ampDir(), "settings.json")}, nil
		},
		row: docsRow{
			detected: "`amp`, `<xdg>/amp` (`<xdg>` is `${XDG_CONFIG_HOME:-~/.config}`)",
			skill:    "`<xdg>/amp/skills/agentfeedback/SKILL.md`",
			hook:     "none",
			reminder: "not supported",
			mcp:      "`amp.mcpServers` member `agentfeedback` in `<xdg>/amp/settings.jsonc` when it exists, else `settings.json`",
		},
	},
	{
		Name:       "vscode",
		Binaries:   []string{"code"},
		ConfigDirs: dirs(Env.vscodeUserDir),
		SkillDirs:  func(Env) []string { return nil },
		NoSkill:    "VS Code reads the skill that the copilot or claude-code adapter installs",
		Instructions: Instructions{
			Global:  func(e Env) []string { return []string{filepath.Join(e.copilotDir(), "copilot-instructions.md")} },
			Project: []string{".github/copilot-instructions.md", "AGENTS.md"},
		},
		Hook:     Hook{Format: "none", NoHook: "VS Code has no skill of its own to pair a hook with", Events: events("none", "none", "none")},
		MCP:      MCP{File: func(e Env) string { return filepath.Join(e.vscodeUserDir(), "mcp.json") }, Path: []string{"servers"}, EnvRef: "${env:AGENT_FEEDBACK_API_KEY}"},
		Sessions: Sessions{Reader: "none", NoReader: "VS Code keeps chat sessions in SQLite at an undocumented path"},
		Verified: documented(),
		mcpValue: func(u string) string {
			return string(obj("type", str("http"), "url", str(u), "headers", bearer("${env:AGENT_FEEDBACK_API_KEY}")))
		},
		mcpStdio: typedStdio("stdio"),
		files: func(e Env) ([]string, []string) {
			return []string{filepath.Join(e.vscodeUserDir(), "mcp.json")}, nil
		},
		noCLI: &Refusal{
			Problem: "vscode has no skill directory of its own; VS Code reads the skill that the copilot or claude-code adapter installs",
			Next:    "install copilot or claude-code for the skill, or run agentfeedback install vscode --mcp",
		},
		row: docsRow{
			detected: "`code`, `<Code/User>`: `<xdg>/Code/User` on Linux, `~/Library/Application Support/Code/User` on macOS",
			skill:    "none: VS Code reads the skill the `copilot` or `claude-code` adapter installs",
			hook:     "none",
			reminder: "not supported",
			mcp:      "`<Code/User>/mcp.json` `servers.agentfeedback`",
		},
	},
	{
		Name:       "gemini-cli",
		Binaries:   []string{"gemini"},
		ConfigDirs: func(Env) []string { return nil },
		SkillDirs:  func(e Env) []string { return []string{filepath.Join(e.geminiDir(), "skills")} },
		Instructions: Instructions{
			Global:  func(e Env) []string { return []string{filepath.Join(e.geminiDir(), "GEMINI.md")} },
			Project: []string{"GEMINI.md"},
		},
		Hook:     Hook{Format: "none", NoHook: "not wired yet", Events: events("AfterTool", "AfterAgent", "SessionStart")},
		MCP:      MCP{NoMCP: "not wired yet"},
		Sessions: Sessions{Locations: []string{"~/.gemini/tmp/<project_hash>/chats/"}, Reader: "gemini-cli-jsonl"},
		Caveats:  []string{"detected by its binary only: ~/.gemini is shared with antigravity"},
		Verified: documented(),
		files:    func(Env) ([]string, []string) { return nil, nil },
		noMCP: &Refusal{
			Problem: "gemini-cli has no MCP entry in agentfeedback install",
			Next:    "run agentfeedback install gemini-cli without --mcp",
		},
		row: docsRow{
			detected: "`gemini` (`~/.gemini` is shared with antigravity)",
			skill:    "`~/.gemini/skills/agentfeedback/SKILL.md`",
			hook:     "none",
			reminder: "not supported",
			mcp:      "none",
		},
	},
}

func init() {
	for i := range registry {
		a := &registry[i]
		if a.mcpValue != nil {
			a.MCP.URLShape = a.mcpValue("<url>")
		}
		if a.mcpStdio != nil {
			a.MCP.StdioShape = a.mcpStdio("<binary>")
		}
		if a.mcp != nil || a.mcpValue == nil {
			continue
		}
		switch a.Name {
		case "claude-code":
			a.mcp = func(_ *plan, o Options) ([]Item, []string, error) {
				v, _ := a.mcpEntry(o, nil)
				it := Item{Kind: KindClaudeMCP, Role: RoleMCP, Args: []string{"mcp", "add-json", "agentfeedback", v, "--scope", "user"}}
				if o.stdio() {
					it.Stdio = []string{o.Binary, "mcp"}
				} else {
					it.URL = o.Server + "/mcp"
				}

				return []Item{it}, nil, nil
			}
		case "opencode":
			a.mcp = jsonMCP(a, (*plan).opencodeConfig, nil, nil)
		case "amp":
			a.mcp = jsonMCP(a, (*plan).ampConfig, nil, nil)
		case "pi":
			a.mcp = jsonMCP(a, fixed(a.MCP.File), []string{"the MCP entry needs pi 0.99.0 or later"}, nil)
		case "antigravity", "cline":
			a.mcp = jsonMCP(a, fixed(a.MCP.File), nil, []string{undocumentedEnvNote(a.Name)})
		default:
			a.mcp = jsonMCP(a, fixed(a.MCP.File), nil, nil)
		}
	}
}

// names is the registry's harness names in display order.
var names = func() []string {
	out := make([]string, len(registry))
	for i, a := range registry {
		out[i] = a.Name
	}

	return out
}()

// adapterOf is the registry entry of name, or nil.
func adapterOf(name string) *Adapter {
	for i := range registry {
		if registry[i].Name == name {
			return &registry[i]
		}
	}

	return nil
}

// Adapters returns a copy of the registry in display order.
func Adapters() []Adapter {
	out := make([]Adapter, len(registry))
	for i, a := range registry {
		a.Binaries = slices.Clone(a.Binaries)
		a.Instructions.Project = slices.Clone(a.Instructions.Project)
		a.Hook.Events = maps.Clone(a.Hook.Events)
		a.MCP.Path = slices.Clone(a.MCP.Path)
		a.Sessions.Locations = slices.Clone(a.Sessions.Locations)
		a.Caveats = slices.Clone(a.Caveats)
		out[i] = a
	}

	return out
}

// Verification is the verification record of the harness name.
func Verification(name string) (Verified, bool) {
	a := adapterOf(name)
	if a == nil {
		return Verified{}, false
	}

	return a.Verified, true
}

// Supports reports whether install can wire name in mode; reason says why
// not.
func Supports(name, mode string) (ok bool, reason string) {
	a := adapterOf(name)
	if a == nil {
		return false, "unknown harness " + name
	}
	if r := a.refusal(mode); r != nil {
		return false, r.Problem
	}

	return true, ""
}

// refusal is the refusal for wiring the adapter in mode, or nil.
func (a *Adapter) refusal(mode string) *Refusal {
	if mode == ModeMCP {
		return a.noMCP
	}

	return a.noCLI
}

// skillDirs is the directories the harness reads skills from.
func (e Env) skillDirs(name string) []string {
	if a := adapterOf(name); a != nil {
		return a.SkillDirs(e)
	}

	return nil
}

// configFiles is every JSON, JSONC or TOML configuration file install can
// edit under this environment; a symbolic link to one is followed.
func (e Env) configFiles() map[string]bool {
	out := map[string]bool{}
	for _, a := range registry {
		config, _ := a.files(e)
		for _, f := range config {
			out[f] = true
		}
	}

	return out
}

// DocsTable is the harness table of docs/operate.md, rendered from the
// registry.
func DocsTable() string {
	var b strings.Builder
	b.WriteString("| Harness | Detected by | Skill | Hook (default) | Reminder | MCP entry (`--mcp`) | Verified |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, a := range registry {
		r := a.row
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | %s %s |\n", a.Name, r.detected, r.skill, r.hook, r.reminder, r.mcp, a.Verified.Level, a.Verified.Date)
	}

	return b.String()
}
