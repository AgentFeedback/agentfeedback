package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSessionsSeenRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)

	read := func(harness, sid string) (SessionSeen, bool) {
		t.Helper()
		var s SessionSeen
		var ok bool
		if err := db.Read(ctx, func(q Querier) error {
			var err error
			s, ok, err = GetSession(ctx, q, harness, sid)

			return err
		}); err != nil {
			t.Fatal(err)
		}

		return s, ok
	}
	write := func(fn func(Querier) error) error { return db.Write(ctx, fn) }

	if _, ok := read("claude-code", "a"); ok {
		t.Fatal("no row expected before a digest")
	}
	// Marking before any digest is refused.
	if err := write(func(q Querier) error { return MarkSession(ctx, q, "claude-code", "a", 1, "nothing", nil) }); !errors.Is(err, ErrNotDigested) {
		t.Fatalf("mark before digest: want ErrNotDigested, got %v", err)
	}

	d := SessionDigest{Harness: "claude-code", SessionID: "a", Path: "/p/a.jsonl", Mtime: 10, Size: 100, Head: "h1", DigestedAt: 5}
	if err := write(func(q Querier) error { return UpsertSessionDigest(ctx, q, d) }); err != nil {
		t.Fatal(err)
	}
	s, ok := read("claude-code", "a")
	if !ok || s.Path != d.Path || s.Mtime != 10 || s.Size != 100 || s.Head != "h1" || s.DigestedAt == nil || *s.DigestedAt != 5 ||
		s.ProcessedAt != nil || s.ProcessedSize != nil || s.ProcessedHead != "" || s.Outcome != "" || s.Refs != nil {
		t.Fatalf("after digest: %+v", s)
	}

	if err := write(func(q Querier) error {
		return MarkSession(ctx, q, "claude-code", "a", 7, "filed", json.RawMessage(`["u1","u2"]`))
	}); err != nil {
		t.Fatal(err)
	}
	s, _ = read("claude-code", "a")
	if s.ProcessedAt == nil || *s.ProcessedAt != 7 || s.ProcessedSize == nil || *s.ProcessedSize != 100 ||
		s.ProcessedMtime == nil || *s.ProcessedMtime != 10 || s.ProcessedHead != "h1" || s.Outcome != "filed" || string(s.Refs) != `["u1","u2"]` {
		t.Fatalf("after mark: %+v", s)
	}

	// A later digest moves the watermark and leaves the processed columns.
	d.Size, d.Mtime, d.Head, d.DigestedAt = 150, 20, "h2", 9
	if err := write(func(q Querier) error { return UpsertSessionDigest(ctx, q, d) }); err != nil {
		t.Fatal(err)
	}
	s, _ = read("claude-code", "a")
	if s.Size != 150 || s.Head != "h2" || *s.DigestedAt != 9 || *s.ProcessedSize != 100 || s.ProcessedHead != "h1" || s.Outcome != "filed" {
		t.Fatalf("after second digest: %+v", s)
	}

	// Empty refs store an empty array.
	if err := write(func(q Querier) error { return MarkSession(ctx, q, "claude-code", "a", 11, "nothing", nil) }); err != nil {
		t.Fatal(err)
	}
	s, _ = read("claude-code", "a")
	if string(s.Refs) != `[]` || *s.ProcessedSize != 150 {
		t.Fatalf("after second mark: %+v", s)
	}

	// Listing is per harness.
	for _, x := range []SessionDigest{
		{Harness: "claude-code", SessionID: "b", Path: "/p/b", Head: "x"},
		{Harness: "codex", SessionID: "c", Path: "/p/c", Head: "y"},
	} {
		if err := write(func(q Querier) error { return UpsertSessionDigest(ctx, q, x) }); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	if err := db.Read(ctx, func(q Querier) error {
		rows, err := ListSessions(ctx, q, "claude-code")
		for _, r := range rows {
			ids = append(ids, r.SessionID)
		}

		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("list: %v", ids)
	}
}

func TestTriageState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	get := func() (string, bool) {
		t.Helper()
		var v string
		var ok bool
		if err := db.Read(ctx, func(q Querier) error {
			var err error
			v, ok, err = TriageState(ctx, q, "selection")

			return err
		}); err != nil {
			t.Fatal(err)
		}

		return v, ok
	}
	if _, ok := get(); ok {
		t.Fatal("no value expected")
	}
	for _, v := range []string{`{"limit":1}`, `{"limit":2}`} {
		if err := db.Write(ctx, func(q Querier) error { return SetTriageState(ctx, q, "selection", v) }); err != nil {
			t.Fatal(err)
		}
		if got, ok := get(); !ok || got != v {
			t.Fatalf("got %q %v, want %q", got, ok, v)
		}
	}
}
