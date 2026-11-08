package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenCodeIdentityMigrationCollisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(q Querier) error {
		_, err := q.ExecContext(ctx, `
INSERT INTO sessions_seen
  (harness, session_id, path, mtime, size, head, digested_at,
   processed_at, processed_size, processed_mtime, processed_head, outcome, refs)
VALUES
  ('opencode', 'legacy', 'old', 10, 100, 'h', 10, 10, 100, 10, 'h', 'filed', '["uid"]'),
  ('opencode', 'collision', 'old', 10, 200, 'h', 10, 10, 200, 10, 'h', 'filed', '["uid"]'),
  ('opencode-v1', 'collision', 'new', 20, 300, 'h', 20, 20, 100, 20, 'h', 'nothing', '[]'),
  ('opencode', 'canonical-progress', 'old', 10, 100, 'h', 10, 30, 100, 10, 'h', 'nothing', '[]'),
  ('opencode-v1', 'canonical-progress', 'new', 20, 300, 'h', 20, 20, 300, 20, 'h', 'filed', '["canonical"]'),
  ('opencode', 'rotated', 'old', 30, 50, 'rotated', 30, 30, 50, 30, 'rotated', 'nothing', '[]'),
  ('opencode-v1', 'rotated', 'new', 20, 300, 'h', 20, 20, 300, 20, 'h', 'filed', '["canonical"]');
UPDATE schema_version SET version = 4`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Read(ctx, func(q Querier) error {
		for _, tc := range []struct {
			id                  string
			size, processed     int64
			head, outcome, refs string
		}{
			{"legacy", 100, 100, "h", "filed", `["uid"]`},
			{"collision", 300, 200, "h", "filed", `["uid"]`},
			{"canonical-progress", 300, 300, "h", "filed", `["canonical"]`},
			{"rotated", 50, 50, "rotated", "nothing", `[]`},
		} {
			row, ok, err := GetSession(ctx, q, "opencode-v1", tc.id)
			if err != nil {
				return err
			}
			if !ok || row.Size != tc.size || row.ProcessedSize == nil || *row.ProcessedSize != tc.processed || row.ProcessedHead != tc.head || row.Outcome != tc.outcome || string(row.Refs) != tc.refs {
				t.Fatalf("%s: %+v", tc.id, row)
			}
			if _, ok, err := GetSession(ctx, q, "opencode", tc.id); err != nil || ok {
				t.Fatalf("legacy row remains: %s, %v", tc.id, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
