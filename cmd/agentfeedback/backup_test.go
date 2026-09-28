package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/internal/v3/core"
	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// A backup must be a usable database, not just a file: the copy is opened and
// its rows counted.
func TestBackup_CopyIsAReadableDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentfeedback.db")
	t.Setenv("DATABASE_PATH", dbPath)

	ctx := context.Background()
	svc := core.New(openDB(t, dbPath))
	for _, summary := range []string{"one", "two", "three"} {
		if _, _, err := svc.CreateFriction(ctx, core.CreateFrictionInput{
			MachineName: "m", CoordinatorModel: "c", Category: "tooling", Summary: summary,
		}); err != nil {
			t.Fatalf("create friction %q: %v", summary, err)
		}
	}

	dest := filepath.Join(dir, "backup.db")
	if err := runBackup([]string{dest}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	copied := openDB(t, dest)
	if n := countRows(t, copied); n != 3 {
		t.Fatalf("backup holds %d row(s), want 3", n)
	}

	// A backup command that clobbers an existing backup is a data-loss command.
	err := runBackup([]string{dest})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected a refusal to overwrite, got %v", err)
	}
}

// VacuumInto is the mechanism behind the command; it must produce a consistent
// copy at an arbitrary path.
func TestVacuumIntoTempPath(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	db := openDB(t, filepath.Join(dir, "source.db"))
	svc := core.New(db)
	if _, _, err := svc.CreateEvent(ctx, core.CreateEventInput{
		Kind: "deploy", Key: "k1", MachineName: "m", CoordinatorModel: "c",
		Payload: []byte(`{"a":1}`),
	}); err != nil {
		t.Fatalf("create event: %v", err)
	}

	dest := filepath.Join(dir, "copy.db")
	if err := db.VacuumInto(ctx, dest); err != nil {
		t.Fatalf("vacuum into: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("stat copy: %v", err)
	}

	copied := openDB(t, dest)
	if n := countRows(t, copied); n != 1 {
		t.Fatalf("copy holds %d row(s), want 1", n)
	}
	var family string
	if err := copied.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, `SELECT family FROM submissions WHERE id = 1`).Scan(&family)
	}); err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if family != store.FamilyEvent {
		t.Fatalf("family %q, want event", family)
	}
}
