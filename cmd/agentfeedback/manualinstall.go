package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
)

// manualStep is how to wire one harness by hand where install cannot run:
// the skill file and the command that writes it, the MCP entry when a server
// URL is configured, and the command whose output goes into the harness's
// instruction file.
type manualStep struct {
	Harness   string `json:"harness"`
	SkillPath string `json:"skill_path"`
	// SkillCommand is a PowerShell command that creates the skill directory
	// and writes the skill file as UTF-8 without a byte order mark.
	SkillCommand string     `json:"skill_command"`
	MCP          *manualMCP `json:"mcp,omitempty"`
	RuleCommand  string     `json:"rule_command"`
	Notes        []string   `json:"notes,omitempty"`
}

// manualMCP is the MCP entry install --mcp would add: a JSON member (file,
// path, key, value), a TOML block (file, text), or for Claude Code the
// claude command that adds it.
type manualMCP struct {
	File    string          `json:"file,omitempty"`
	Path    []string        `json:"path,omitempty"`
	Key     string          `json:"key,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Text    string          `json:"text,omitempty"`
	Command string          `json:"command,omitempty"`
}

// ruleNote tells where the rule command's output goes.
const ruleNote = "paste the output of the rule command into the harness's instruction file"

// manualSteps computes the wiring by hand of the named harnesses, or of the
// detected ones when none (or all) is named, and prints it on stderr. The
// server is resolved without a prompt; nothing is written.
func manualSteps(pos []string, serverFlag string, stderr io.Writer) []manualStep {
	env, err := harnessEnv()
	if err != nil {
		return nil
	}
	names := pos
	if len(pos) == 0 || slices.Contains(pos, "all") {
		names = nil
		for _, n := range harness.Names() {
			if env.Detect(n) != "no" || slices.Contains(pos, n) {
				names = append(names, n)
			}
		}
	}
	names = dedupe(names)
	srv, _, srvErr := installServerNoPrompt(os.Getenv, env, serverFlag)
	var out []manualStep
	for _, n := range names {
		s := manualStep{
			Harness:     n,
			SkillPath:   env.SkillPath(n),
			RuleCommand: "agentfeedback skill render agents-md",
		}
		s.SkillCommand = "New-Item -ItemType Directory -Force -Path " + psQuote(filepath.Dir(s.SkillPath)) + " | Out-Null; " +
			"[IO.File]::WriteAllText(" + psQuote(s.SkillPath) + ", (agentfeedback skill render skill-md | Out-String))"
		switch {
		case srvErr != nil:
			s.Notes = append(s.Notes, "the MCP entry cannot be computed: "+oneLine(srvErr.Error()))
		case srv == "" || srv == serverLocal:
			s.Notes = append(s.Notes, "the MCP entry needs a server URL; local mode has none yet")
		default:
			it, err := env.ManualMCP(n, srv)
			if err != nil {
				s.Notes = append(s.Notes, "the MCP entry cannot be computed: "+oneLine(err.Error()))

				break
			}
			m := &manualMCP{File: it.File, Path: it.Path, Key: it.Key, Value: it.Value, Text: it.Text}
			if len(it.Args) > 0 {
				m.Command = harness.ClaudeCommand(it.Args)
			}
			s.MCP = m
		}
		s.Notes = append(s.Notes, ruleNote)
		out = append(out, s)
	}
	printManualSteps(stderr, out)

	return out
}

// psQuote is s as a PowerShell literal string: single-quoted, with each
// single quote doubled.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// printManualSteps prints the steps for a person on stderr.
func printManualSteps(w io.Writer, steps []manualStep) {
	const prefix = "agentfeedback install: "
	if len(steps) == 0 {
		fmt.Fprintln(w, prefix+"no harness detected; name one to see how to wire it by hand")
	}
	for _, s := range steps {
		fmt.Fprintf(w, "%s%s: wire by hand:\n", prefix, s.Harness)
		fmt.Fprintf(w, "  skill: %s\n", s.SkillCommand)
		if m := s.MCP; m != nil {
			switch {
			case m.Command != "":
				fmt.Fprintf(w, "  mcp:   %s\n", m.Command)
			case m.Text != "":
				fmt.Fprintf(w, "  mcp:   add to %s:\n%s\n", m.File, strings.TrimRight(m.Text, "\n"))
			default:
				fmt.Fprintf(w, "  mcp:   in %s, set %s to %s\n", m.File, strings.Join(append(slices.Clone(m.Path), m.Key), "."), m.Value)
			}
		}
		fmt.Fprintf(w, "  rule:  %s\n", s.RuleCommand)
		for _, n := range s.Notes {
			fmt.Fprintf(w, "  note:  %s\n", n)
		}
	}
}
