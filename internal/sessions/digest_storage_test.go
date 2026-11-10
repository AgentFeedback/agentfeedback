package sessions

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestDigestUnprocessedSkipsStorageBeforeLimit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := cursorEnv(w)
	path := putCursor(t, w, "older-storage", defaultCwd, "opaque stored transcript\n")
	older := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, older, older); err != nil {
		t.Fatal(err)
	}
	w.put(defaultCwd, "newer-claude", fixture(t, "basic.jsonl", "newer-claude", defaultCwd))

	out, err := Digest(w.ctx, w.db, e, DigestRequest{Unprocessed: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].Ref != "claude-code:newer-claude" || len(out.Sessions[0].Events) == 0 || len(out.Errors) != 0 {
		t.Fatalf("storage-only session consumed the digest limit: %+v", out)
	}
	if w.opens[path] != 0 {
		t.Fatalf("automatic digest opened storage-only transcript %d times", w.opens[path])
	}
}

func TestDigestUnprocessedCursorRefusedWithoutOpening(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := cursorEnv(w)
	path := putCursor(t, w, "storage-only", defaultCwd, "opaque stored transcript\n")

	out, err := Digest(w.ctx, w.db, e, DigestRequest{Unprocessed: true, Harness: HarnessCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 0 || len(out.Errors) != 1 || out.Errors[0].Ref != "cursor:storage-only" || out.Errors[0].State != StateUnsupportedFormat || !strings.Contains(out.Errors[0].Message, "storage capture only") {
		t.Fatalf("explicit storage-only harness was not refused: %+v", out)
	}
	if w.opens[path] != 0 {
		t.Fatalf("unsupported harness opened transcript %d times", w.opens[path])
	}
}
