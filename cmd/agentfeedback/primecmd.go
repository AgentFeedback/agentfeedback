package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/agentfeedback/agentfeedback/v4/internal/skillgen"
)

const primeSynopsis = "prime [--format text|cursor]"

// runPrime prints the submission guidance a session starts with: as text,
// or with --format cursor as the JSON Cursor's sessionStart hook reads.
func runPrime(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("prime")
	format := fs.String("format", "text", "text, or cursor for the JSON object Cursor's sessionStart hook reads")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: agentfeedback %s\n", primeSynopsis)
		fs.PrintDefaults()
	}
	pos, err := parseInterleaved(fs, args, stderr)
	if err != nil {
		return errFlags("prime", err)
	}
	if len(pos) > 0 {
		return errArgs("prime", primeSynopsis)
	}
	text, err := skillgen.Prime()
	if err != nil {
		return err
	}
	switch *format {
	case "text":
		_, err = stdout.Write(text)

		return err
	case "cursor":
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(map[string]string{"additional_context": string(text)}); err != nil {
			return err
		}
		_, err = stdout.Write(buf.Bytes())

		return err
	}

	return errPrimeFormat(*format)
}

func errPrimeFormat(format string) error {
	return usageErr(fmt.Sprintf("unknown format %q", format), "use agentfeedback prime --format text or --format cursor")
}
