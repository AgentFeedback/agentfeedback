package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// localReports is what a switch to a server found in the local database:
// the reports there (install-check rows aside), the command that copies
// them, and the migrate outcome when init copied them.
type localReports struct {
	Count    int             `json:"count"`
	Command  string          `json:"command"`
	Migrated *migrateOutcome `json:"migrated,omitempty"`
}

// migrateLocalCommand is the command that copies the local reports to server.
func migrateLocalCommand(server string) string {
	return "agentfeedback migrate --local --to " + shellQuote(server) + " --to-key-from-stdin"
}

// shellQuote leaves s as it is when it holds only [A-Za-z0-9:/._-], else
// single-quotes it the POSIX way: an embedded single quote ends the
// quoting, is escaped with a backslash, and the quoting resumes.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(":/._-", r))
	}) {
		return s
	}

	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readOnlyDSN is the driver's file: URI for path, opened read-only, the
// path escaped so ? and # stay part of it.
func readOnlyDSN(path string) string {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")

	return (&url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: q.Encode()}).String()
}

// countLocalReports counts the reports in the data-directory database,
// install-check rows aside, without creating, migrating or writing it: a
// missing file counts 0, and the file is opened read-only. Local data never
// silently forwards to a newly configured server, so a switch names them.
func countLocalReports(getenv func(string) string) (int, error) {
	path, err := localDBPath(getenv)
	if err != nil {
		return 0, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM submissions WHERE kind <> 'install-check'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the reports in %s: %w", path, err)
	}

	return n, nil
}

// askCopyLocal asks whether to copy the local reports now; Enter is no.
func askCopyLocal(n int, server string, in *bufio.Reader, stderr io.Writer) (bool, error) {
	fmt.Fprintf(stderr, "This machine's local database holds %d report(s). Copy them to %s now? [y/N] ", n, server)
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, errInitAnswer(err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}

	return false, nil
}

// migrateLocalTo runs migrate --local --to server --to-key-from-stdin in
// process, the key fed from memory. The lines migrate prints before its
// outcome (conflicts) go to stderr; the outcome line is returned.
func migrateLocalTo(server, key string, stderr io.Writer) *migrateOutcome {
	// The copy reads the local database and changes nothing in it: no
	// spool delivery, no inbox ingest.
	prev := skipStartupPass
	skipStartupPass = true
	defer func() { skipStartupPass = prev }()
	var out bytes.Buffer
	err := runMigrate([]string{"--local", "--to", server, "--to-key-from-stdin"}, strings.NewReader(key+"\n"), &out, stderr)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	for _, l := range lines[:len(lines)-1] {
		fmt.Fprintln(stderr, l)
	}
	var o migrateOutcome
	if jerr := json.Unmarshal([]byte(lines[len(lines)-1]), &o); jerr != nil || o.Outcome == "" {
		msg := "migrate printed no outcome"
		if err != nil {
			msg = err.Error()
		}
		o = migrateOutcome{Outcome: "error", Message: msg}
	}
	if err != nil && o.Outcome != "error" {
		o.Outcome, o.Message = "error", err.Error()
	}

	return &o
}

// doctorInitLocalReports names the local reports after doctor --init wrote
// server's URL: a note on stderr and the member of the outcome line. It
// never copies them; a count that fails is a note, not a failure.
func doctorInitLocalReports(server string, stderr io.Writer) *localReports {
	n, err := countLocalReports(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "agentfeedback doctor: warning: the local reports could not be counted: %s\n", oneLine(err.Error()))

		return nil
	}
	if n == 0 {
		return nil
	}
	lr := &localReports{Count: n, Command: migrateLocalCommand(strings.TrimRight(server, "/"))}
	fmt.Fprintf(stderr, "agentfeedback doctor: this machine's local database holds %d report(s); they stay there; copy them with %s\n", n, lr.Command)

	return lr
}
