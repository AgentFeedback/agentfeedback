package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

// A backup taken while the database is being written is a consistent,
// usable database holding a snapshot between the counts before and after.
func TestBackup_ConsistentWhileWriting(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentfeedback.db")
	t.Setenv("DATABASE_PATH", dbPath)

	ctx := context.Background()
	db := openDB(t, dbPath)
	svc := newService(db)
	create := func(n int64) error {
		res, err := svc.Create(ctx, []byte(fmt.Sprintf(`{"summary":"row %d"}`, n)))
		if err == nil && !res.Created {
			err = fmt.Errorf("row %d was not a new insert", n)
		}

		return err
	}
	var seq atomic.Int64
	for range 50 {
		if err := create(seq.Add(1)); err != nil {
			t.Fatal(err)
		}
	}

	// The writer commits at least one insert before the backup starts and
	// keeps inserting until it returns.
	var started, stop atomic.Bool
	var during atomic.Int64
	firstDone := make(chan struct{})
	var wg sync.WaitGroup
	var writeErr atomic.Value
	wg.Add(1)
	go func() {
		defer wg.Done()
		first := true
		for !stop.Load() {
			if err := create(seq.Add(1)); err != nil {
				writeErr.Store(err)
				if first {
					close(firstDone)
				}

				return
			}
			if started.Load() {
				during.Add(1)
			}
			if first {
				first = false
				close(firstDone)
			}
		}
	}()
	<-firstDone
	before := countRows(t, db)
	dest := filepath.Join(dir, "backup.db")
	started.Store(true)
	r := runCLI(t, "", "backup", dest)
	stop.Store(true)
	wg.Wait()
	after := countRows(t, db)
	if err, _ := writeErr.Load().(error); err != nil {
		t.Fatalf("concurrent create: %v", err)
	}
	if during.Load() == 0 {
		t.Fatal("no insert committed while the backup ran")
	}
	if r.code != 0 || !strings.Contains(r.stdout, "backed up "+dbPath+" to "+dest) {
		t.Fatalf("backup: %+v", r)
	}

	copied := openDB(t, dest)
	var check string
	if err := copied.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check)
	}); err != nil || check != "ok" {
		t.Fatalf("integrity_check %q %v", check, err)
	}
	if n := countRows(t, copied); n < before || n > after {
		t.Fatalf("backup holds %d row(s), want between %d and %d", n, before, after)
	}

	// A backup command that clobbers an existing backup is a data-loss command.
	if r := runCLI(t, "", "backup", dest); r.code != 1 || !strings.Contains(r.stderr, "already exists") {
		t.Fatalf("overwrite: %+v", r)
	}
}

func TestBackup_RefusesAMissingDatabase(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "absent.db")
	t.Setenv("DATABASE_PATH", dbPath)

	r := runCLI(t, "", "backup", filepath.Join(dir, "backup.db"))
	if r.code != 1 || !strings.Contains(r.stderr, "database "+dbPath+" does not exist; nothing to back up") {
		t.Fatalf("missing source: %+v", r)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("backup created the source database: %v", err)
	}
}
