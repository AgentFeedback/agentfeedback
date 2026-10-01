package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "agentfeedback.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func TestOpenAppliesSchemaAndIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agentfeedback.db")
	ctx := context.Background()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	known, err := KnownSchemaVersion()
	if err != nil {
		t.Fatalf("known version: %v", err)
	}
	got, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if got != known || known != 1 {
		t.Fatalf("schema version %d, known %d, want both 1", got, known)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopening an already-migrated database is a no-op.
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestOnlyOneMigrationIsEmbedded(t *testing.T) {
	t.Parallel()

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "001_init.sql" {
		t.Fatalf("migrations/ holds %v, want exactly 001_init.sql", names)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agentfeedback.db")
	ctx := context.Background()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Write(ctx, func(q Querier) error {
		_, err := q.ExecContext(ctx, "UPDATE schema_version SET version = 999")

		return err
	}); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A database written by a newer binary must not be opened: writing rows a
	// newer schema no longer expects is worse than refusing to start.
	if _, err := Open(ctx, path); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("expected ErrSchemaTooNew, got %v", err)
	}
}

func TestOpenRefusesADatabaseItDidNotCreate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v3.db")

	// The previous major version's database: tables, schema_version = 1, no
	// stamp. Its version number alone would pass the too-new check.
	raw, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (version) VALUES (1)`,
		`CREATE TABLE submissions (id INTEGER PRIMARY KEY AUTOINCREMENT, family TEXT NOT NULL, payload TEXT NOT NULL)`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(ctx, path)
	if !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("expected ErrForeignDatabase, got %v", err)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "DATABASE_PATH") {
		t.Fatalf("error must name the file and the way out, got %q", err)
	}

	// Any schema object counts, not only tables.
	viewOnly := filepath.Join(t.TempDir(), "views.db")
	raw, err = sql.Open("sqlite", dsn(viewOnly, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE VIEW v AS SELECT 1`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, viewOnly); !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("a database holding only a view must be refused, got %v", err)
	}
}

func TestConcurrentFirstOpensAgreeOnOneSchema(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	// serve and a restore starting on the same fresh file at once: both
	// must come up, and schema_version must hold exactly one row.
	const openers = 4
	var wg sync.WaitGroup
	errs := make(chan error, openers)
	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := Open(ctx, path)
			if err != nil {
				errs <- err

				return
			}
			errs <- db.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent open: %v", err)
		}
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var rows int
	if err := db.reader.QueryRowContext(ctx, "SELECT count(*) FROM schema_version").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("schema_version holds %d rows, want 1", rows)
	}
}

func TestOpenResumesAfterAnInterruptedFirstOpen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "partial.db")

	// The stamp is written before anything else, so a crash right after it
	// leaves a stamped database without tables; the next open completes it.
	raw, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA application_id = "+strconv.Itoa(applicationID)); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open after interrupted first open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if v, err := db.SchemaVersion(ctx); err != nil || v != 1 {
		t.Fatalf("schema version %d, %v; want 1", v, err)
	}
}

func TestOpenOfACurrentDatabaseTakesNoWriteLock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()

	// A writer holding the lock (serve mid-insert) must not stall a second
	// open of a database that needs no migration (a backup, an import).
	tx, err := first.writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	openCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	second, err := Open(openCtx, path)
	if err != nil {
		t.Fatalf("open beside a held write lock: %v", err)
	}
	_ = second.Close()
}

func TestApplyMigrationRechecksTheVersionUnderTheLock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	known := migs[len(migs)-1].version

	// Another opener applied the migration after this one read version 0:
	// applying it again must be a no-op, not "table already exists".
	if err := db.applyMigration(ctx, migs[0], known); err != nil {
		t.Fatalf("re-applying an applied migration: %v", err)
	}

	// A newer binary migrated the file in that window: refuse it.
	if err := db.Write(ctx, func(q Querier) error {
		_, err := q.ExecContext(ctx, "UPDATE schema_version SET version = ?", known+1)

		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.applyMigration(ctx, migs[0], known); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("expected ErrSchemaTooNew, got %v", err)
	}
}

func TestPingWhileBusyReportsCancellation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "locked.db")
	holder, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	holder.SetMaxOpenConns(1)
	// An exclusive lock keeps every other connection busy, the WAL switch
	// of a new one included.
	if _, err := holder.ExecContext(ctx, "PRAGMA locking_mode = EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(ctx, "CREATE TABLE t (x)"); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	err = pingWhileBusy(cctx, db)
	if !errors.Is(err, context.DeadlineExceeded) || !IsBusy(err) {
		t.Fatalf("expected the deadline wrapping the busy error, got %v", err)
	}
}

func TestSchemaShape(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)

	var stamp int64
	if err := db.reader.QueryRowContext(ctx, "PRAGMA application_id").Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if stamp != applicationID {
		t.Fatalf("application_id %#x, want %#x", stamp, applicationID)
	}

	rows, err := db.reader.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'submissions' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	want := []string{
		"ix_submissions_category", "ix_submissions_fix_status", "ix_submissions_hash_created",
		"ix_submissions_kind_created", "ix_submissions_machine", "ix_submissions_occurred",
		"ix_submissions_open", "ix_submissions_project", "ix_submissions_verdict",
		"sqlite_autoindex_submissions_1", // uid UNIQUE
		"ux_submissions_key",
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("indexes\n got %v\nwant %v", got, want)
	}

	// The two friction projections are VIRTUAL generated columns (hidden = 2
	// in table_xinfo), so the payload stays the single source of truth.
	xinfo, err := db.reader.QueryContext(ctx, `SELECT name, hidden FROM pragma_table_xinfo('submissions') WHERE hidden <> 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = xinfo.Close() }()
	generated := map[string]int{}
	for xinfo.Next() {
		var name string
		var hidden int
		if err := xinfo.Scan(&name, &hidden); err != nil {
			t.Fatal(err)
		}
		generated[name] = hidden
	}
	if len(generated) != 2 || generated["category"] != 2 || generated["fix_status"] != 2 {
		t.Fatalf("generated columns %v, want category and fix_status as VIRTUAL", generated)
	}
}

func TestSetSequenceNeverLowersTheCounter(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)

	err := db.Write(ctx, func(q Querier) error {
		s := newSub("u-500")
		s.ID = 500
		if err := InsertWithID(ctx, q, s); err != nil {
			return err
		}
		if err := SetSequence(ctx, q, 1000); err != nil {
			return err
		}
		// A lower value must not rewind the counter.
		if err := SetSequence(ctx, q, 10); err != nil {
			return err
		}
		id, err := Insert(ctx, q, newSub("u-next"))
		if err != nil {
			return err
		}
		if id != 1001 {
			t.Fatalf("next id %d, want 1001", id)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
}
