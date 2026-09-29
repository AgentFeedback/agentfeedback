package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/agentfeedback/agentfeedback/internal/skillgen"
)

const skillSynopsis = "skill render <form> [--server URL]"

// runSkill prints a rendered form of the submission guidance on stdout.
func runSkill(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		fmt.Fprintf(stderr, "usage: agentfeedback %s\nforms: %s\n", skillSynopsis, strings.Join(skillgen.Forms(), ", "))

		return flag.ErrHelp
	}
	if len(args) == 0 {
		return errArgs("skill", skillSynopsis)
	}
	if args[0] != "render" {
		return errSkillVerb(args[0])
	}

	fs := newFlagSet("skill render")
	server := fs.String("server", "", "base URL named by the forms that carry one (prompt, mcp)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\nforms: %s\n", skillSynopsis, strings.Join(skillgen.Forms(), ", "))
		fs.PrintDefaults()
	}
	positional, err := parseInterspersed(fs, args[1:], stderr)
	if err != nil {
		return errFlags("skill", err)
	}
	if len(positional) != 1 {
		return errArgs("skill", skillSynopsis)
	}

	out, err := skillgen.Render(positional[0], *server)
	var se *skillgen.ServerError
	switch {
	case errors.Is(err, skillgen.ErrUnknownForm):
		return errSkillForm(positional[0], skillgen.Forms())
	case errors.As(err, &se):
		return errSkillServer(se.Raw, se.Reason)
	case err != nil:
		return err
	}
	_, err = stdout.Write(out)

	return err
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
