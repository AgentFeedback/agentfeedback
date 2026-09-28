// Package store owns the SQLite database: connection setup, forward-only schema
// migration, and every SQL statement the service runs.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	sqlitedrv "modernc.org/sqlite" // database/sql driver "sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrSchemaTooNew is returned when the database was written by a newer binary.
// Starting anyway would mean writing rows a newer schema no longer expects, so
// the service refuses instead.
var ErrSchemaTooNew = errors.New("database schema is newer than this binary supports")

// DB is the service's handle on the SQLite database. It keeps two pools: a
// single-connection writer whose transactions begin IMMEDIATE (so a write
// transaction can never fail with SQLITE_BUSY halfway through after taking a
// read snapshot), and a small reader pool whose transactions stay deferred so
// long reads such as the export never hold the write lock.
type DB struct {
	writer *sql.DB
	reader *sql.DB
	path   string
	// busyWrites counts write transactions that still failed with SQLITE_BUSY
	// after the busy_timeout elapsed — the signal that the single writer is
	// saturated, exported as a metric.
	busyWrites atomic.Int64
}

const readerMaxConns = 4

func dsn(path string, immediate bool) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	if immediate {
		q.Set("_txlock", "immediate")
	}

	return "file:" + path + "?" + q.Encode()
}

// Open opens (creating if needed) the database at path and migrates it forward
// to the schema this binary knows.
func Open(ctx context.Context, path string) (*DB, error) {
	writer, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, fmt.Errorf("open sqlite writer at %s: %w", path, err)
	}
	writer.SetMaxOpenConns(1)

	reader, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open sqlite reader at %s: %w", path, err)
	}
	reader.SetMaxOpenConns(readerMaxConns)

	db := &DB{writer: writer, reader: reader, path: path}

	if err := writer.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite database at %s: %w", path, err)
	}
	if err := db.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

// Close releases both pools.
func (db *DB) Close() error {
	rerr := db.reader.Close()
	werr := db.writer.Close()
	if werr != nil {
		return werr
	}

	return rerr
}

// Path is the database file's path.
func (db *DB) Path() string { return db.path }

// Ping verifies the database answers a trivial query.
func (db *DB) Ping(ctx context.Context) error {
	var one int
	if err := db.reader.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	return nil
}

// Querier is the subset of *sql.DB / *sql.Tx the query functions need.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Write runs fn inside one BEGIN IMMEDIATE transaction on the writer pool. The
// transaction is rolled back unless fn returns nil.
func (db *DB) Write(ctx context.Context, fn func(Querier) error) error {
	tx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		// BEGIN IMMEDIATE takes the write lock, so the saturation shows up
		// here as often as inside fn.
		db.countIfBusy(err)

		return fmt.Errorf("begin write transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		db.countIfBusy(err)

		return err
	}

	if err := tx.Commit(); err != nil {
		db.countIfBusy(err)

		return fmt.Errorf("commit write transaction: %w", err)
	}

	return nil
}

func (db *DB) countIfBusy(err error) {
	if IsBusy(err) {
		db.busyWrites.Add(1)
	}
}

// sqliteCode returns the SQLite result code carried by err, masked down to its
// primary code, and whether err carried one at all. modernc.org/sqlite wraps
// every library failure in its exported *sqlite.Error, so classification does
// not have to parse messages; extended codes (SQLITE_BUSY_SNAPSHOT and the
// like) share the low byte with their primary code.
func sqliteCode(err error) (primary, full int, ok bool) {
	var serr *sqlitedrv.Error
	if !errors.As(err, &serr) {
		return 0, 0, false
	}
	code := serr.Code()

	return code & 0xff, code, true
}

// IsBusy reports whether err is SQLite's "database is locked" failure: the
// write transaction could not take the lock before busy_timeout elapsed.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	if primary, _, ok := sqliteCode(err); ok {
		return primary == sqlite3.SQLITE_BUSY || primary == sqlite3.SQLITE_LOCKED
	}
	// Errors that reach here were produced outside the driver (or had their
	// chain flattened to a string); the message is all that is left.
	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

// BusyWrites reports how many write transactions failed because the database
// stayed locked past the busy timeout.
func (db *DB) BusyWrites() int64 { return db.busyWrites.Load() }

// FileBytes is the on-disk size of the database plus its write-ahead log.
func (db *DB) FileBytes() int64 {
	var total int64
	for _, p := range []string{db.path, db.path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}

	return total
}

// Read runs fn inside one deferred read transaction, so every statement in fn
// sees the same consistent snapshot.
func (db *DB) Read(ctx context.Context, fn func(Querier) error) error {
	tx, err := db.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	return fn(tx)
}

// SchemaVersion reports the schema version recorded in the database.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := db.reader.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}

	return v, nil
}

// KnownSchemaVersion is the highest migration this binary embeds.
func KnownSchemaVersion() (int, error) {
	migs, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	if len(migs) == 0 {
		return 0, nil
	}

	return migs[len(migs)-1].version, nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	migs := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: expected NNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		body, err := migrationsFS.ReadFile(filepath.ToSlash(filepath.Join("migrations", e.Name())))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		migs = append(migs, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	for i, m := range migs {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered consecutively from 001, got %s at position %d", m.name, i+1)
		}
	}

	return migs, nil
}

// migrate applies every migration newer than the database's recorded version,
// each in its own transaction.
func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.writer.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	var current int
	err := db.writer.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := db.writer.ExecContext(ctx, "INSERT INTO schema_version (version) VALUES (0)"); err != nil {
			return fmt.Errorf("initialize schema_version: %w", err)
		}
		current = 0
	case err != nil:
		return fmt.Errorf("read schema version: %w", err)
	}

	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	known := 0
	if len(migs) > 0 {
		known = migs[len(migs)-1].version
	}
	if current > known {
		return fmt.Errorf("%w: database at version %d, binary knows %d", ErrSchemaTooNew, current, known)
	}

	for _, m := range migs {
		if m.version <= current {
			continue
		}
		if err := db.applyMigration(ctx, m); err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE schema_version SET version = ?", m.version); err != nil {
		return fmt.Errorf("record migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.name, err)
	}

	return nil
}
