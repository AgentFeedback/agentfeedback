package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/capture"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

const configSynopsis = "config [show [--json] [--local | --server URL] | get <key> [--local | --server URL] | set <key> <value>]"

type configView struct {
	Path       string                     `json:"path"`
	Settings   []configSetting            `json:"settings"`
	Capture    capture.Policy             `json:"capture"`
	Repository collect.RepositorySettings `json:"repository"`
	Warnings   []string                   `json:"warnings,omitempty"`
}

func runConfig(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		if !isTerminal() {
			return usageErr("config needs a terminal for its menu", "use agentfeedback config show, config get <key> or config set <key> <value>")
		}
		return runConfigMenu(stdin, stdout, stderr)
	}
	command := args[0]
	if command != "show" && command != "get" && command != "set" {
		if command == "--help" || command == "-h" {
			_, err := fmt.Fprintf(stdout, "usage: agentfeedback %s\n", configSynopsis)
			return err
		}
		return errArgs("config", configSynopsis)
	}
	fs := newFlagSet("config " + command)
	synopsis := "config show [--json] [--local | --server URL]"
	switch command {
	case "get":
		synopsis = "config get <key> [--local | --server URL]"
	case "set":
		synopsis = "config set <key> <value>"
	}
	fs.Usage = func() { fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\n", synopsis); fs.PrintDefaults() }
	mf := &modeFlags{}
	if command != "set" {
		mf = addModeFlags(fs)
	}
	asJSON := false
	if command == "show" {
		fs.BoolVar(&asJSON, "json", false, "print effective settings and their sources as JSON")
	}
	pos, err := parseInterleaved(fs, args[1:], stderr)
	if err != nil {
		return errFlags("config "+command, err)
	}
	switch command {
	case "set":
		if len(pos) != 2 {
			return errArgs("config set", "config set <key> <value>")
		}
		return setConfigSetting(os.Getenv, pos[0], pos[1])
	case "get":
		if len(pos) != 1 {
			return errArgs("config get", "config get <key> [--local | --server URL]")
		}
		_, _, settings, err := effectiveConfigSettings(*mf, os.Getenv)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(settings))
		for _, setting := range settings {
			if setting.Key == pos[0] {
				if !setting.Set {
					return exitStatus(3)
				}
				_, err := fmt.Fprintln(stdout, configValueText(setting.Value))
				return err
			}
			keys = append(keys, setting.Key)
		}
		return usageErr(fmt.Sprintf("unknown configuration key %q", safe(pos[0])), "use one of: "+strings.Join(keys, ", "))
	default:
		if len(pos) != 0 {
			return errArgs("config show", "config show [--json] [--local | --server URL]")
		}
		view, err := loadConfigView(*mf, os.Getenv)
		if err != nil {
			return err
		}
		if asJSON {
			return writeJSON(stdout, view)
		}
		return printConfigView(stdout, view)
	}
}

func loadConfigView(mf modeFlags, getenv func(string) string) (configView, error) {
	path, file, settings, err := effectiveConfigSettings(mf, getenv)
	if err != nil {
		return configView{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return configView{}, err
	}
	view := configView{Path: path, Settings: settings, Repository: collect.ReadRepositorySettings(cwd)}
	view.Warnings = append(view.Warnings, collect.Lint(cwd, getenv("HOME"), file.Collect)...)
	if !view.Repository.InRepository {
		view.Warnings = append(view.Warnings, view.Repository.Warnings...)
	}
	checkConfigKeys(path, func(err error) { view.Warnings = append(view.Warnings, err.Error()) })
	view.Capture, err = capturePolicy(getenv, mf, file)
	if err != nil {
		view.Capture.State = capture.Error
		view.Warnings = append(view.Warnings, "session policy unavailable: "+err.Error())
	}
	return view, nil
}

func printConfigView(stdout io.Writer, view configView) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Machine configuration: %s\n", safe(view.Path))
	fmt.Fprintln(&b, "Destination (read-only): change with agentfeedback init or doctor --init")
	for _, setting := range view.Settings {
		value := configValueText(setting.Value)
		if !setting.Set {
			value = "(unset)"
		}
		fmt.Fprintf(&b, "  %s = %s [%s]\n", setting.Key, safe(oneLine(value)), setting.Source)
	}
	fmt.Fprintln(&b, "Organisation session policy (read-only):")
	fmt.Fprintln(&b, capture.Status(view.Capture))
	r := view.Repository
	if !r.Present {
		fmt.Fprintln(&b, "Repository narrowing (read-only): no file in effect")
	} else {
		fmt.Fprintf(&b, "Repository narrowing (read-only): %s\n", safe(r.Path))
		if r.InRepository {
			fmt.Fprintf(&b, "  collect.disabled = %t\n  collect.deny_paths = %s\n  context.drop = %s\n  detect.nudge = %t\n", r.Disabled, safe(configValueText(r.DenyPaths)), safe(configValueText(r.Drop)), !r.NudgeOff)
		} else {
			fmt.Fprintln(&b, "  outside Git: only the session-push restriction applies")
		}
		fmt.Fprintf(&b, "  sessions.push = %t (never grants capture permission)\n", r.PushAllowed)
	}
	for _, warning := range view.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", safe(oneLine(warning)))
	}
	return writeCaptureOutput(os.Getenv, stdout, b.String(), view.Capture)
}
