package skillgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4"
)

// The plugin forms are directories, not stdout forms, so they are not in
// Forms. FormAgentPlugin is an Agent Plugins 1.0.0 bundle of the agentfeedback
// skill, with an mcp.json only when a server is named; FormMarketplace is a
// marketplace root that lists that bundle, without mcp.json, for Claude Code
// and Codex.
const (
	FormAgentPlugin = "agent-plugin"
	FormMarketplace = "marketplace"
)

// The Agent Plugins schemas the rendered manifests name.
const (
	pluginSchema = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	mcpSchema    = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
)

// pluginDir is where a marketplace root keeps the bundle.
const pluginDir = "plugins/agentfeedback"

// PluginMeta is source/plugin.json.
type PluginMeta struct {
	DisplayName            string       `json:"display_name"`
	Description            string       `json:"description"`
	MarketplaceDescription string       `json:"marketplace_description"`
	Keywords               []string     `json:"keywords"`
	Author                 PluginAuthor `json:"author"`
	Homepage               string       `json:"homepage"`
	Repository             string       `json:"repository"`
	CodexCategory          string       `json:"codex_category"`
}

// PluginAuthor is the author of the plugin and the owner of the marketplace.
type PluginAuthor struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// pluginManifest is plugin.json; Schema is empty, and left out, in the
// Claude Code manifest.
type pluginManifest struct {
	Schema      string       `json:"$schema,omitempty"`
	Name        string       `json:"name"`
	Version     string       `json:"version"`
	Description string       `json:"description"`
	Author      PluginAuthor `json:"author"`
	Homepage    string       `json:"homepage"`
	Repository  string       `json:"repository"`
	License     string       `json:"license"`
	Keywords    []string     `json:"keywords"`
}

type mcpManifest struct {
	Schema     string               `json:"$schema"`
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

type mcpServer struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type claudeMarketplace struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Owner       PluginAuthor  `json:"owner"`
	Plugins     []claudeEntry `json:"plugins"`
}

type claudeEntry struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Description string `json:"description"`
}

type codexMarketplace struct {
	Name      string `json:"name"`
	Interface struct {
		DisplayName string `json:"displayName"`
	} `json:"interface"`
	Plugins []codexEntry `json:"plugins"`
}

type codexEntry struct {
	Name   string `json:"name"`
	Source struct {
		Source string `json:"source"`
		Path   string `json:"path"`
	} `json:"source"`
	Policy struct {
		Installation   string `json:"installation"`
		Authentication string `json:"authentication"`
	} `json:"policy"`
	Category string `json:"category"`
}

// AgentPlugin renders the plugin bundle: plugin.json, the Claude Code
// manifest, the agentfeedback skill and its install script, then, only when
// server is not empty, an mcp.json naming that server's MCP endpoint.
func AgentPlugin(server string) ([]File, error) {
	m, _, err := Source()
	if err != nil {
		return nil, err
	}
	p, err := pluginSource(sourceFS)
	if err != nil {
		return nil, err
	}
	version, err := semver(m.Version)
	if err != nil {
		return nil, fmt.Errorf("source/skill.json: %w", err)
	}
	manifest := pluginManifest{
		Schema: pluginSchema, Name: m.Name, Version: version, Description: p.Description,
		Author: p.Author, Homepage: p.Homepage, Repository: p.Repository, License: m.License, Keywords: p.Keywords,
	}
	skill, err := Render(FormSkillMD, "")
	if err != nil {
		return nil, err
	}

	files := []File{{Path: "plugin.json", Data: encodeJSON(manifest)}}
	manifest.Schema = ""
	files = append(files,
		File{Path: ".claude-plugin/plugin.json", Data: encodeJSON(manifest)},
		File{Path: "skills/" + m.Name + "/SKILL.md", Data: skill},
		File{Path: "skills/" + m.Name + "/scripts/install.sh", Data: agentfeedback.InstallScript, Mode: 0o755},
	)
	if server == "" {
		return files, nil
	}
	base, err := mcpServerURL(server)
	if err != nil {
		return nil, err
	}

	return append(files, File{Path: "mcp.json", Data: encodeJSON(mcpManifest{
		Schema:     mcpSchema,
		MCPServers: map[string]mcpServer{m.Name: {Type: "streamable-http", URL: base + "/mcp"}},
	})}), nil
}

// Marketplace renders a marketplace root: the Claude Code and Codex
// marketplace manifests, then the bundle without mcp.json under
// plugins/agentfeedback/.
func Marketplace() ([]File, error) {
	m, _, err := Source()
	if err != nil {
		return nil, err
	}
	p, err := pluginSource(sourceFS)
	if err != nil {
		return nil, err
	}
	bundle, err := AgentPlugin("")
	if err != nil {
		return nil, err
	}

	claude := claudeMarketplace{
		Name: m.Name, Description: p.MarketplaceDescription, Owner: p.Author,
		Plugins: []claudeEntry{{Name: m.Name, Source: "./" + pluginDir, Description: p.Description}},
	}
	codex := codexMarketplace{Name: m.Name, Plugins: []codexEntry{{Name: m.Name, Category: p.CodexCategory}}}
	codex.Interface.DisplayName = p.DisplayName
	codex.Plugins[0].Source.Source = "local"
	codex.Plugins[0].Source.Path = "./" + pluginDir
	codex.Plugins[0].Policy.Installation = "AVAILABLE"
	codex.Plugins[0].Policy.Authentication = "ON_INSTALL"

	files := []File{
		{Path: ".claude-plugin/marketplace.json", Data: encodeJSON(claude)},
		{Path: ".agents/plugins/marketplace.json", Data: encodeJSON(codex)},
	}
	for _, f := range bundle {
		f.Path = pluginDir + "/" + f.Path
		files = append(files, f)
	}

	return files, nil
}

func pluginSource(source fs.FS) (PluginMeta, error) {
	var p PluginMeta
	data, err := fs.ReadFile(source, "source/plugin.json")
	if err != nil {
		return p, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("source/plugin.json: %w", err)
	}
	if p.DisplayName == "" || p.Description == "" || p.MarketplaceDescription == "" || len(p.Keywords) == 0 ||
		p.Author.Name == "" || p.Author.URL == "" || p.Homepage == "" || p.Repository == "" || p.CodexCategory == "" {
		return p, errors.New("source/plugin.json: every field is required")
	}

	return p, nil
}

// semver renders the skill's version as the semantic version a plugin
// manifest needs: "5.0" becomes "5.0.0", "5.0.1" stays.
func semver(v string) (string, error) {
	parts := strings.Split(v, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return "", fmt.Errorf("version %q is not MAJOR.MINOR or MAJOR.MINOR.PATCH", v)
	}
	for _, p := range parts {
		if _, err := strconv.ParseUint(p, 10, 64); err != nil || (len(p) > 1 && p[0] == '0') {
			return "", fmt.Errorf("version %q is not MAJOR.MINOR or MAJOR.MINOR.PATCH", v)
		}
	}
	if len(parts) == 2 {
		v += ".0"
	}

	return v, nil
}

// mcpServerURL checks a server for an Agent Plugins mcp.json, which needs
// https except on the local machine.
func mcpServerURL(server string) (string, error) {
	base, err := NormalizeServer(server)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", &ServerError{server, "it does not parse"}
	}
	if u.Scheme == "http" {
		// Host names are case-insensitive; the spec's "exactly localhost" admits no trailing dot.
		h := u.Hostname()
		if ip := net.ParseIP(h); !strings.EqualFold(h, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return "", &ServerError{server, "an Agent Plugins mcp.json needs https unless the host is localhost or a loopback address"}
		}
	}

	return base, nil
}

// encodeJSON is v as two-space indented JSON ending in one newline; the
// values are the package's own structs, so encoding cannot fail.
func encodeJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}

	return b.Bytes()
}
