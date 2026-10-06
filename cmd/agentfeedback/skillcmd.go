package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

const skillSynopsis = "skill render <form> [--server URL] | skill render docs|marketplace --out DIR | skill render agent-plugin --out DIR [--server URL] | skill reminder"

// runSkill prints a rendered form of the submission guidance, or the
// one-line session-start reminder, on stdout; skill render docs, agent-plugin
// and marketplace write a directory into a new or empty --out instead.
func runSkill(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		fmt.Fprintf(stderr, "usage: agentfeedback %s\n%s", skillSynopsis, skillForms())

		return flag.ErrHelp
	}
	if len(args) == 0 {
		return errArgs("skill", skillSynopsis)
	}
	if args[0] == "reminder" {
		if len(args) != 1 {
			return errArgs("skill reminder", "skill reminder")
		}
		line, err := skillgen.Reminder()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, line)

		return err
	}
	if args[0] != "render" {
		return errSkillVerb(args[0])
	}

	fs := newFlagSet("skill render")
	server := fs.String("server", "", "base URL named by the forms that carry one (prompt, mcp, agent-plugin)")
	out := fs.String("out", "", "docs, agent-plugin and marketplace only: write into this new or empty directory")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\n%s", skillSynopsis, skillForms())
		fs.PrintDefaults()
	}
	positional, err := parseInterspersed(fs, args[1:], stderr)
	if err != nil {
		return errFlags("skill", err)
	}
	if len(positional) != 1 {
		return errArgs("skill", skillSynopsis)
	}
	switch form := positional[0]; form {
	case skillgen.FormDocs, skillgen.FormAgentPlugin, skillgen.FormMarketplace:
		switch {
		case *server != "" && form != skillgen.FormAgentPlugin:
			return errSkillServerNotApplicable(form)
		case *out == "":
			return errSkillNeedsOut(form)
		}
		var files []skillgen.File
		switch form {
		case skillgen.FormDocs:
			files, err = skillgen.Docs()
		case skillgen.FormAgentPlugin:
			files, err = skillgen.AgentPlugin(*server)
		default:
			files, err = skillgen.Marketplace()
		}
		var se *skillgen.ServerError
		switch {
		case errors.As(err, &se):
			return errSkillServer(se.Raw, se.Reason)
		case err != nil:
			return err
		}

		return renderDir(*out, files)
	}
	if *out != "" {
		return errSkillOutOnlyDirs()
	}

	rendered, err := skillgen.Render(positional[0], *server)
	var se *skillgen.ServerError
	switch {
	case errors.Is(err, skillgen.ErrUnknownForm):
		return errSkillForm(positional[0], append(skillgen.Forms(), skillgen.FormDocs, skillgen.FormAgentPlugin, skillgen.FormMarketplace))
	case errors.As(err, &se):
		return errSkillServer(se.Raw, se.Reason)
	case err != nil:
		return err
	}
	_, err = stdout.Write(rendered)

	return err
}

func skillForms() string {
	return fmt.Sprintf("forms: %s (printed on stdout), %s, %s, %s (written with --out DIR)\n",
		strings.Join(skillgen.Forms(), ", "), skillgen.FormDocs, skillgen.FormAgentPlugin, skillgen.FormMarketplace)
}

// renderDir writes a rendered directory into dir, which must not exist or be
// an empty directory, so a render never mixes with other files.
func renderDir(dir string, files []skillgen.File) error {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return errSkillOutDir(dir, err)
	case !info.IsDir():
		return errSkillOutNotEmpty(dir)
	default:
		entries, err := os.ReadDir(dir)
		if err != nil {
			return errSkillOutDir(dir, err)
		}
		if len(entries) > 0 {
			return errSkillOutNotEmpty(dir)
		}
	}
	// The files are rendered beside dir and renamed into place, so a failed
	// render leaves nothing behind.
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return errSkillOutDir(parent, err)
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".tmp-*")
	if err != nil {
		return errSkillOutDir(parent, err)
	}
	if err := renderInto(tmp, dir, files); err != nil {
		_ = os.RemoveAll(tmp)

		return err
	}

	return nil
}

// renderInto writes files into tmp and renames it to dir, replacing dir
// when it is an empty directory.
func renderInto(tmp, dir string, files []skillgen.File) error {
	if err := os.Chmod(tmp, 0o755); err != nil {
		return errSkillOutDir(tmp, err)
	}
	for _, f := range files {
		path := filepath.Join(tmp, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return errSkillOutDir(filepath.Dir(path), err)
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(path, f.Data, mode); err != nil {
			return errSkillOutWrite(path, err)
		}
		// WriteFile's mode passes through the umask; the render's does not.
		if err := os.Chmod(path, mode); err != nil {
			return errSkillOutWrite(path, err)
		}
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errSkillOutDir(dir, err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return errSkillOutDir(dir, err)
	}

	return nil
}

// parseInterspersed parses flags wherever they appear among the positional
// arguments, so `skill render prompt --server URL` reads as documented.
func parseInterspersed(fs *flag.FlagSet, args []string, stderr io.Writer) ([]string, error) {
	var positional []string
	for {
		if err := parseFlags(fs, args, stderr); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}
