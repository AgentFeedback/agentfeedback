package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

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
	if got != known {
		t.Fatalf("schema version %d, want %d", got, known)
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

func TestSetSequenceNeverLowersTheCounter(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "agentfeedback.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	err = db.Write(ctx, func(q Querier) error {
		if err := InsertSubmissionWithID(ctx, q, Submission{
			ID: 500, Family: FamilyFriction, SubmissionType: FamilyFriction,
			MachineName: "m", CoordinatorModel: "c", Payload: []byte(`{"a":1}`),
			PayloadHash: "h", CreatedAt: 1,
		}); err != nil {
			return err
		}
		if err := SetSequence(ctx, q, 1000); err != nil {
			return err
		}
		// A lower value must not rewind the counter.
		if err := SetSequence(ctx, q, 10); err != nil {
			return err
		}
		created, err := InsertSubmission(ctx, q, Submission{
			Family: FamilyFriction, SubmissionType: FamilyFriction,
			MachineName: "m", CoordinatorModel: "c", Payload: []byte(`{"a":2}`),
			PayloadHash: "h2", CreatedAt: 2,
		})
		if err != nil {
			return err
		}
		if created.ID != 1001 {
			t.Fatalf("next id %d, want 1001", created.ID)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
}
