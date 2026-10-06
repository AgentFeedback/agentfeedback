package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

// runSchema lists the schemas this binary ships or prints one verbatim: the
// highest version of a kind, or the version named.
func runSchema(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("schema")
	asJSON := fs.Bool("json", false, "print the list as JSON (a schema is always JSON)")
	if err := parseFlags(fs, args, stderr); err != nil {
		return errFlags("schema", err)
	}
	if fs.NArg() > 2 {
		return errArgs("schema", "schema [<kind> [<version>]] [--json]")
	}

	entries := schema.List()
	if fs.NArg() == 0 {
		if *asJSON {
			return writeJSON(stdout, entries)
		}
		for _, e := range entries {
			if _, err := fmt.Fprintf(stdout, "%-10s %s\n", e.Kind, joinVersions(e.Versions)); err != nil {
				return err
			}
		}

		return nil
	}

	kind := schema.Token(fs.Arg(0))
	var versions []uint64
	kinds := make([]string, 0, len(entries))
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
		if e.Kind == kind {
			versions = e.Versions
		}
	}
	if versions == nil {
		return errSchemaKind(fs.Arg(0), kinds)
	}

	version := versions[len(versions)-1]
	if fs.NArg() == 2 {
		v, err := strconv.ParseUint(fs.Arg(1), 10, 64)
		if err != nil || v == 0 {
			return errSchemaVersion(kind, fs.Arg(1), versionStrings(versions))
		}
		version = v
	}
	doc, ok := schema.Document(kind, version)
	if !ok {
		return errSchemaVersion(kind, strconv.FormatUint(version, 10), versionStrings(versions))
	}
	if _, err := stdout.Write(doc); err != nil {
		return err
	}
	if len(doc) > 0 && doc[len(doc)-1] != '\n' {
		_, err := io.WriteString(stdout, "\n")

		return err
	}

	return nil
}

func versionStrings(versions []uint64) []string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = strconv.FormatUint(v, 10)
	}

	return out
}

func joinVersions(versions []uint64) string { return strings.Join(versionStrings(versions), ",") }
