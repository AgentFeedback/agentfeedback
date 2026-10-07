package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

// newerSchemaDB creates the local database through a client command and
// stamps it with a schema version no binary knows yet.
func newerSchemaDB(t *testing.T) {
	t.Helper()
	if r := runCLI(t, "", "list"); r.code != 0 {
		t.Fatalf("create the local database: %+v", r)
	}
	path, err := localDBPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(q store.Querier) error {
		_, err := q.ExecContext(ctx, "UPDATE schema_version SET version = 999")

		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// schemaTooNewLogged reports whether the client log holds an error line for
// a database written by a newer binary.
func schemaTooNewLogged(t *testing.T, cache string) bool {
	t.Helper()
	for _, l := range hookLogLines(t, cache) {
		if l["outcome"] == client.OutcomeError && strings.HasPrefix(fmt.Sprint(l["reason"]), "schema_too_new") {
			return true
		}
	}

	return false
}

// TestSchemaTooNew_Command: a command that meets a newer local database fails
// with the store's message and leaves a schema_too_new line in the client log.
func TestSchemaTooNew_Command(t *testing.T) {
	_, cache := isolate(t)
	newerSchemaDB(t)
	r := runCLI(t, "", "list")
	if r.code != 1 || !strings.Contains(r.stderr, "database schema is newer than this binary supports") ||
		!strings.Contains(r.stderr, "run the newest agentfeedback binary") {
		t.Fatalf("%+v", r)
	}
	if !schemaTooNewLogged(t, cache) {
		t.Fatalf("no schema_too_new line: %v", hookLogLines(t, cache))
	}
}

// TestSchemaTooNew_FlushHook: the hook stays silent and exits 0 on a newer
// local database, and logs schema_too_new instead of a generic hook line.
func TestSchemaTooNew_FlushHook(t *testing.T) {
	_, cache := isolate(t)
	newerSchemaDB(t)
	spoolOne(t, dataRoot(t), client.LocalDestination)
	r := runCLI(t, "", "flush", "--hook")
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	if !schemaTooNewLogged(t, cache) {
		t.Fatalf("no schema_too_new line: %v", hookLogLines(t, cache))
	}
	for _, l := range hookLogLines(t, cache) {
		if strings.HasPrefix(fmt.Sprint(l["reason"]), "flush --hook") {
			t.Fatalf("generic hook line logged: %v", l)
		}
	}
}

// countSchemaTooNew counts the client-log lines for a database written by a
// newer binary.
func countSchemaTooNew(t *testing.T, cache string) int {
	t.Helper()
	n := 0
	for _, l := range hookLogLines(t, cache) {
		if strings.HasPrefix(fmt.Sprint(l["reason"]), "schema_too_new") {
			n++
		}
	}

	return n
}

// TestSchemaTooNew_Submit: submit keeps its client_setup outcome on stdout
// and logs exactly one schema_too_new line instead of the generic one.
func TestSchemaTooNew_Submit(t *testing.T) {
	_, cache := isolate(t)
	newerSchemaDB(t)
	r := runCLI(t, "", "submit", "friction", "--summary", "skewed")
	if !strings.Contains(r.stdout, `"reason":"client_setup"`) {
		t.Fatalf("%+v", r)
	}
	if n := countSchemaTooNew(t, cache); n != 1 {
		t.Fatalf("%d schema_too_new lines: %v", n, hookLogLines(t, cache))
	}
	for _, l := range hookLogLines(t, cache) {
		if l["reason"] == "client_setup" {
			t.Fatalf("generic line logged: %v", l)
		}
	}
}

// TestSchemaTooNew_Doctor: doctor reports the newer database and logs one
// schema_too_new line.
func TestSchemaTooNew_Doctor(t *testing.T) {
	_, cache := isolate(t)
	newerSchemaDB(t)
	r := runCLI(t, "", "doctor")
	if r.code == 0 {
		t.Fatalf("%+v", r)
	}
	if n := countSchemaTooNew(t, cache); n != 1 {
		t.Fatalf("%d schema_too_new lines: %v", n, hookLogLines(t, cache))
	}
}
