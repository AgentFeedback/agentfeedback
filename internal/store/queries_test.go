package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	second = int64(1_000_000)
	hour   = 3600 * second
	day    = 24 * hour
	// base is 2026-09-28T12:00:00Z, a Monday.
	base = int64(1_790_596_800) * second
)

func ptr[T any](v T) *T { return &v }

// newSub is a minimal valid record; tests override what they care about.
func newSub(uid string) Submission {
	return Submission{
		UID: uid, Kind: "friction", SchemaVersion: 1, Payload: json.RawMessage(`{}`),
		ContentHash: "h-" + uid, CreatedAt: base,
	}
}

func mustInsert(t *testing.T, db *DB, s Submission) int64 {
	t.Helper()
	var id int64
	err := db.Write(context.Background(), func(q Querier) error {
		var err error
		id, err = Insert(context.Background(), q, s)

		return err
	})
	if err != nil {
		t.Fatalf("insert %s: %v", s.UID, err)
	}

	return id
}

func mustGet(t *testing.T, db *DB, id int64) Submission {
	t.Helper()
	var s Submission
	if err := db.Read(context.Background(), func(q Querier) error {
		var err error
		s, err = GetByID(context.Background(), q, id)

		return err
	}); err != nil {
		t.Fatalf("get %d: %v", id, err)
	}

	return s
}

func ids(rows []Submission) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}

	return out
}

func listIDs(t *testing.T, db *DB, f ListFilter, p Page) []int64 {
	t.Helper()
	var rows []Submission
	if err := db.Read(context.Background(), func(q Querier) error {
		var err error
		rows, err = List(context.Background(), q, f, p, false)

		return err
	}); err != nil {
		t.Fatalf("list: %v", err)
	}

	return ids(rows)
}

func TestInsertAndGetReturnTheStoredBytesExactly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)

	// Spacing, member order, a large integer, an exponent spelling, raw
	// UTF-8, a replacement character and escapes must all come back as
	// written: nothing on the read path may decode the payload.
	payload := json.RawMessage("{\"z\": 1,  \"a\": 18446744073709551615, \"e\": 1e2, \"s\": \"café � \\u0000 \\/\", \"n\": {\"deep\": [true, null]}}")
	contextBytes := json.RawMessage(`{"git_commit": "a1b2c3d",  "client": "agentfeedback/4.0.0"}`)
	s := newSub("u-bytes")
	s.Key, s.Summary, s.Machine, s.Model, s.Harness, s.Project = "k1", "a summary", "m1", "model-x", "claude-code", "example"
	s.OccurredAt = ptr(base - hour)
	s.Payload, s.Context = payload, contextBytes
	id := mustInsert(t, db, s)

	got := mustGet(t, db, id)
	if !bytes.Equal(got.Payload, payload) {
		t.Fatalf("payload\n got %s\nwant %s", got.Payload, payload)
	}
	if !bytes.Equal(got.Context, contextBytes) {
		t.Fatalf("context\n got %s\nwant %s", got.Context, contextBytes)
	}
	s.ID = id
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("record\n got %+v\nwant %+v", got, s)
	}

	// The same bytes through the other read paths.
	if err := db.Read(ctx, func(q Querier) error {
		byUID, err := GetByUID(ctx, q, "u-bytes")
		if err != nil {
			return err
		}
		if !bytes.Equal(byUID.Payload, payload) {
			t.Fatalf("GetByUID payload %s", byUID.Payload)
		}
		byKey, err := GetByKey(ctx, q, "friction", "k1")
		if err != nil {
			return err
		}
		if byKey.ID != id {
			t.Fatalf("GetByKey id %d, want %d", byKey.ID, id)
		}
		withPayload, err := List(ctx, q, ListFilter{}, Page{Limit: 10}, true)
		if err != nil {
			return err
		}
		if len(withPayload) != 1 || !bytes.Equal(withPayload[0].Payload, payload) {
			t.Fatalf("List with payload: %+v", withPayload)
		}
		without, err := List(ctx, q, ListFilter{}, Page{Limit: 10}, false)
		if err != nil {
			return err
		}
		if len(without) != 1 || without[0].Payload != nil || !bytes.Equal(without[0].Context, contextBytes) {
			t.Fatalf("List without payload must drop only the payload: %+v", without)
		}
		var seen int
		if err := Iterate(ctx, q, ExportFilter{}, func(r Submission) error {
			seen++
			if !bytes.Equal(r.Payload, payload) {
				t.Fatalf("Iterate payload %s", r.Payload)
			}

			return nil
		}); err != nil {
			return err
		}
		if seen != 1 {
			t.Fatalf("iterated %d rows", seen)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyStringsAreStoredAsNull(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	id := mustInsert(t, db, newSub("u-empty"))

	var nulls int
	if err := db.reader.QueryRowContext(ctx, `SELECT (key IS NULL) + (summary IS NULL) + (machine IS NULL) +
		(model IS NULL) + (harness IS NULL) + (project IS NULL) + (context IS NULL) + (occurred_at IS NULL) +
		(verdict IS NULL) + (resolution IS NULL) + (ref IS NULL) + (processed_by IS NULL)
		FROM submissions WHERE id = ?`, id).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 12 {
		t.Fatalf("%d of 12 optional columns are NULL", nulls)
	}
	got := mustGet(t, db, id)
	if got.Key != "" || got.Context != nil || got.OccurredAt != nil {
		t.Fatalf("absent members must read back as zero values: %+v", got)
	}
	// An invalid payload is refused by the table itself (the generated
	// column's json_extract or the CHECK, whichever runs first).
	bad := newSub("u-bad")
	bad.Payload = json.RawMessage(`{not json`)
	if err := db.Write(ctx, func(q Querier) error {
		_, err := Insert(ctx, q, bad)

		return err
	}); err == nil {
		t.Fatal("invalid payload must be refused")
	}
	if n := listIDs(t, db, ListFilter{}, Page{Limit: 10}); len(n) != 1 {
		t.Fatalf("the refused row must not exist: %v", n)
	}
}

func TestKeyedUniqueness(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)

	keyed := newSub("u-k1")
	keyed.Key = "same-key"
	mustInsert(t, db, keyed)

	dup := newSub("u-k2")
	dup.Key = "same-key"
	err := db.Write(ctx, func(q Querier) error {
		_, err := Insert(ctx, q, dup)

		return err
	})
	if !IsUniqueViolation(err) {
		t.Fatalf("second row under (friction, same-key) must violate ux_submissions_key, got %v", err)
	}

	// The same key under another kind is another row; keyless rows never collide.
	otherKind := newSub("u-k3")
	otherKind.Kind, otherKind.Key = "review", "same-key"
	mustInsert(t, db, otherKind)
	mustInsert(t, db, newSub("u-k4"))
	mustInsert(t, db, newSub("u-k5"))

	// uid is unique on its own.
	sameUID := newSub("u-k1")
	err = db.Write(ctx, func(q Querier) error {
		_, err := Insert(ctx, q, sameUID)

		return err
	})
	if !IsUniqueViolation(err) {
		t.Fatalf("duplicate uid must be a unique violation, got %v", err)
	}
	if IsUniqueViolation(nil) || IsUniqueViolation(errors.New("other")) {
		t.Fatal("IsUniqueViolation must be false for nil and unrelated errors")
	}
}

func TestRecentByHashWindow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	since := base - day

	sub := func(uid string, created int64) Submission {
		s := newSub(uid)
		s.ContentHash, s.CreatedAt = "same-hash", created

		return s
	}
	mustInsert(t, db, sub("u-old", base-25*hour)) // outside the window
	processed := mustInsert(t, db, sub("u-processed", base-hour))
	if err := db.Write(ctx, func(q Querier) error {
		_, err := MarkProcessed(ctx, q, []int64{processed}, base, Mark{})

		return err
	}); err != nil {
		t.Fatal(err)
	}
	older := mustInsert(t, db, sub("u-open-older", base-2*hour))

	lookup := func(hash string) (Submission, error) {
		var s Submission
		err := db.Read(ctx, func(q Querier) error {
			var err error
			s, err = GetRecentByHash(ctx, q, hash, since)

			return err
		})

		return s, err
	}
	got, err := lookup("same-hash")
	if err != nil || got.ID != older {
		t.Fatalf("want the open row inside the window (%d), got %d, %v", older, got.ID, err)
	}

	// A newer open row wins over the older one.
	newer := mustInsert(t, db, sub("u-open-newer", base-30*60*second))
	if got, err = lookup("same-hash"); err != nil || got.ID != newer {
		t.Fatalf("want the newest open row (%d), got %d, %v", newer, got.ID, err)
	}

	// Only the old row: nothing inside the window.
	if _, err := lookup("other-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hash: want ErrNotFound, got %v", err)
	}
	if err := db.Write(ctx, func(q Querier) error {
		_, err := MarkProcessed(ctx, q, []int64{older, newer}, base, Mark{})

		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := lookup("same-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("every row in the window processed: want ErrNotFound, got %v", err)
	}
}

// seedListSet inserts a small dataset with every filterable dimension varied
// and returns the ids in insertion order (1..n).
func seedListSet(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	type row struct {
		kind, key, summary, machine, model, harness, project, hash, payload string
		version                                                             int64
		created                                                             int64
		occurred                                                            *int64
	}
	rows := []row{
		{"friction", "k-1", "README step 3 flag gone", "ws-a", "claude-fable-5-1", "claude-code", "example", "hash-a", `{"category":"documentation","details":"the --flag is gone","fix_status":"applied"}`, 1, base - 6*day, ptr(base - 6*day - hour)},
		{"friction", "", "Docs drift in operate", "ws-b", "gpt-6", "codex", "example", "hash-a", `{"category":"documentation","details":"stale section","fix_status":"proposed"}`, 1, base - 5*day, nil},
		{"review", "k-3", "Panel run on PR 12", "ws-a", "claude-fable-5-1", "claude-code", "other", "hash-b", `{"category":"never-matches-review","findings":[{"text":"Café escapes é"}]}`, 1, base - 4*day, ptr(base - 4*day)},
		{"unknown", "", "", "ws-c", "", "", "", "hash-c", `{"value":"loose text HERE"}`, 1, base - 3*day, nil},
		{"friction", "k-5", "Tooling: gh pr checks hangs", "ws-b", "gpt-6", "opencode", "example", "hash-d", `{"category":"tooling","details":"hangs on a closed PR"}`, 2, base - 2*day, ptr(base - 2*day)},
		{"install-check", "k-6", "install ok", "ws-c", "claude-fable-5-1", "claude-code", "example", "hash-e", `{"ok":true}`, 1, base - day, ptr(base - day)},
		{"friction", "k-7", "Config points at stale host", "ws-a", "claude-fable-5-1", "claude-code", "fleet", "hash-a", `{"category":"config","details":"host renamed","fix_status":"applied"}`, 1, base, ptr(base)},
	}
	for i, r := range rows {
		s := Submission{
			UID: fmt.Sprintf("u-%d", i+1), Kind: r.kind, SchemaVersion: r.version, Key: r.key, Summary: r.summary,
			Machine: r.machine, Model: r.model, Harness: r.harness, Project: r.project, OccurredAt: r.occurred,
			Payload: json.RawMessage(r.payload), ContentHash: r.hash, CreatedAt: r.created,
		}
		if id := mustInsert(t, db, s); id != int64(i+1) {
			t.Fatalf("seed id %d, want %d", id, i+1)
		}
	}
	// Rows 1 and 3 processed (1 fixed, 3 invalid); row 6 processed with no
	// verdict; row 2 redacted.
	if err := db.Write(ctx, func(q Querier) error {
		if _, err := MarkProcessed(ctx, q, []int64{1}, base, Mark{Verdict: ptr("fixed")}); err != nil {
			return err
		}
		if _, err := MarkProcessed(ctx, q, []int64{3}, base, Mark{Verdict: ptr("invalid")}); err != nil {
			return err
		}
		if _, err := MarkProcessed(ctx, q, []int64{6}, base, Mark{}); err != nil {
			return err
		}
		_, err := Redact(ctx, q, 2, base)

		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestListFilters(t *testing.T) {
	t.Parallel()

	db := openTest(t)
	seedListSet(t, db)
	all := Page{Limit: 100}

	cases := []struct {
		name string
		f    ListFilter
		want []int64 // newest first
	}{
		{"none", ListFilter{}, []int64{7, 6, 5, 4, 3, 2, 1}},
		{"kind", ListFilter{Kind: "friction"}, []int64{7, 5, 2, 1}},
		{"kind unknown", ListFilter{Kind: KindUnknown}, []int64{4}},
		{"schema_version", ListFilter{SchemaVersion: ptr(int64(2))}, []int64{5}},
		{"key", ListFilter{Key: "k-5"}, []int64{5}},
		{"machine", ListFilter{Machine: "ws-a"}, []int64{7, 3, 1}},
		{"model", ListFilter{Model: "gpt-6"}, []int64{5, 2}},
		{"project", ListFilter{Project: "example"}, []int64{6, 5, 2, 1}},
		{"harness", ListFilter{Harness: "codex"}, []int64{2}},
		{"category (friction only)", ListFilter{Category: "documentation"}, []int64{1}}, // row 2 redacted, row 3 is a review
		{"category never matches a review", ListFilter{Category: "never-matches-review"}, []int64{}},
		{"fix_status", ListFilter{FixStatus: "applied"}, []int64{7, 1}},
		{"exclude one kind", ListFilter{ExcludeKinds: []string{"install-check"}}, []int64{7, 5, 4, 3, 2, 1}},
		{"exclude two kinds", ListFilter{ExcludeKinds: []string{"install-check", "review"}}, []int64{7, 5, 4, 2, 1}},
		{"verdict", ListFilter{Verdict: "fixed"}, []int64{1}},
		{"processed true", ListFilter{Processed: ptr(true)}, []int64{6, 3, 1}},
		{"processed false (the queue)", ListFilter{Processed: ptr(false)}, []int64{7, 5, 4, 2}},
		{"redacted true", ListFilter{Redacted: ptr(true)}, []int64{2}},
		{"redacted false", ListFilter{Redacted: ptr(false)}, []int64{7, 6, 5, 4, 3, 1}},
		{"content_hash", ListFilter{ContentHash: "hash-a"}, []int64{7, 2, 1}},
		{"since inclusive", ListFilter{Since: ptr(base - 2*day)}, []int64{7, 6, 5}},
		{"until inclusive", ListFilter{Until: ptr(base - 5*day)}, []int64{2, 1}},
		{"since and until", ListFilter{Since: ptr(base - 4*day), Until: ptr(base - 2*day)}, []int64{5, 4, 3}},
		{"since on occurred_at skips rows without it", ListFilter{Since: ptr(base - 6*day - hour), On: OnOccurredAt}, []int64{7, 6, 5, 3, 1}},
		{"until on occurred_at", ListFilter{Until: ptr(base - 4*day), On: OnOccurredAt}, []int64{3, 1}},
		{"q summary", ListFilter{Q: "flag gone"}, []int64{1}},
		{"q summary case-insensitive", ListFilter{Q: "PR CHECKS"}, []int64{5}},
		{"q payload string value", ListFilter{Q: "closed pr"}, []int64{5}},
		{"q nested payload value with an escape", ListFilter{Q: "CAFÉ ESCAPES É"}, []int64{3}},
		{"q payload key names are not text", ListFilter{Q: "category"}, []int64{}},
		{"q over a redacted row finds nothing", ListFilter{Q: "stale section"}, []int64{}},
		{"q with another filter", ListFilter{Q: "here", Kind: KindUnknown}, []int64{4}},
		{"q with a filter that excludes the match", ListFilter{Q: "here", Kind: "friction"}, []int64{}},
		{"combined", ListFilter{Kind: "friction", Project: "example", Processed: ptr(false), Redacted: ptr(false)}, []int64{5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := listIDs(t, db, c.f, all); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			var n int64
			if err := db.Read(context.Background(), func(q Querier) error {
				var err error
				n, err = Count(context.Background(), q, c.f)

				return err
			}); err != nil {
				t.Fatal(err)
			}
			if n != int64(len(c.want)) {
				t.Fatalf("count %d, want %d", n, len(c.want))
			}
		})
	}
}

func TestListCursors(t *testing.T) {
	t.Parallel()

	db := openTest(t)
	seedListSet(t, db)
	f := ListFilter{}

	cases := []struct {
		name string
		page Page
		want []int64
	}{
		{"first page newest first", Page{Limit: 3}, []int64{7, 6, 5}},
		{"before_id continues", Page{Limit: 3, BeforeID: ptr(int64(5))}, []int64{4, 3, 2}},
		{"before_id last page", Page{Limit: 3, BeforeID: ptr(int64(2))}, []int64{1}},
		{"before_id exhausted", Page{Limit: 3, BeforeID: ptr(int64(1))}, []int64{}},
		{"after_id zero is the oldest page", Page{Limit: 3, AfterID: ptr(int64(0))}, []int64{1, 2, 3}},
		{"after_id continues", Page{Limit: 3, AfterID: ptr(int64(3))}, []int64{4, 5, 6}},
		{"after_id last page", Page{Limit: 3, AfterID: ptr(int64(6))}, []int64{7}},
		{"limit plus one reveals another page", Page{Limit: 8}, []int64{7, 6, 5, 4, 3, 2, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := listIDs(t, db, f, c.page); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}

	// Cursors combine with filters: the queue, oldest first, after row 2.
	got := listIDs(t, db, ListFilter{Processed: ptr(false)}, Page{Limit: 10, AfterID: ptr(int64(2))})
	if !reflect.DeepEqual(got, []int64{4, 5, 7}) {
		t.Fatalf("queue after 2: %v", got)
	}

	for name, page := range map[string]Page{
		"both cursors": {Limit: 1, BeforeID: ptr(int64(1)), AfterID: ptr(int64(0))},
		"zero limit":   {Limit: 0},
	} {
		if _, _, err := listSQL(f, page, false); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	if _, _, err := listSQL(ListFilter{On: "processed_at"}, Page{Limit: 1}, false); err == nil {
		t.Fatal("an unknown On column must be rejected")
	}
}

func TestCountAndListShareOneSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	seedListSet(t, db)

	err := db.Read(ctx, func(q Querier) error {
		total, err := Count(ctx, q, ListFilter{})
		if err != nil {
			return err
		}
		// A row lands between the count and the page, on the writer.
		mustInsert(t, db, newSub("u-late"))
		rows, err := List(ctx, q, ListFilter{}, Page{Limit: 100}, false)
		if err != nil {
			return err
		}
		if total != 7 || len(rows) != 7 {
			t.Fatalf("total %d, rows %d: the read transaction must not see the late row", total, len(rows))
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := listIDs(t, db, ListFilter{}, Page{Limit: 1}); !reflect.DeepEqual(got, []int64{8}) {
		t.Fatalf("a new read sees the late row: %v", got)
	}
}

func TestStats(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	seedListSet(t, db)

	run := func(fn func(q Querier) error) {
		t.Helper()
		if err := db.Read(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("totals", func(t *testing.T) {
		run(func(q Querier) error {
			got, err := StatsTotals(ctx, q, ListFilter{})
			if err != nil {
				return err
			}
			if want := (Totals{Total: 7, Open: 4, Processed: 3, Redacted: 1}); got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			got, err = StatsTotals(ctx, q, ListFilter{Kind: "nothing"})
			if err != nil {
				return err
			}
			if got != (Totals{}) {
				t.Fatalf("empty set must be all zeros, got %+v", got)
			}

			return nil
		})
	})

	t.Run("groups", func(t *testing.T) {
		type g struct {
			keys                   string
			total, open, processed int64
		}
		render := func(groups []Group) []g {
			out := make([]g, len(groups))
			for i, grp := range groups {
				names := make([]string, 0, len(grp.Keys))
				for k := range grp.Keys {
					names = append(names, k)
				}
				sort.Strings(names)
				parts := make([]string, len(names))
				for j, k := range names {
					parts[j] = fmt.Sprintf("%s=%v", k, grp.Keys[k])
				}
				out[i] = g{strings.Join(parts, ","), grp.Total, grp.Open, grp.Processed}
			}

			return out
		}
		cases := []struct {
			name string
			f    ListFilter
			by   []string
			want []g
		}{
			{"kind", ListFilter{}, []string{"kind"}, []g{{"kind=friction", 4, 3, 1}, {"kind=install-check", 1, 0, 1}, {"kind=review", 1, 0, 1}, {"kind=unknown", 1, 1, 0}}},
			{"project skips rows without one", ListFilter{}, []string{"project"}, []g{{"project=example", 4, 2, 2}, {"project=fleet", 1, 1, 0}, {"project=other", 1, 0, 1}}},
			{"category is friction only and drops the tombstone", ListFilter{}, []string{"category"}, []g{{"category=config", 1, 1, 0}, {"category=documentation", 1, 0, 1}, {"category=tooling", 1, 1, 0}}},
			{"fix_status", ListFilter{}, []string{"fix_status"}, []g{{"fix_status=applied", 2, 1, 1}}},
			{"verdict only over processed rows with one", ListFilter{}, []string{"verdict"}, []g{{"verdict=fixed", 1, 0, 1}, {"verdict=invalid", 1, 0, 1}}},
			{"schema_version renders as an integer", ListFilter{}, []string{"schema_version"}, []g{{"schema_version=1", 6, 3, 3}, {"schema_version=2", 1, 1, 0}}},
			{"machine with a filter", ListFilter{Kind: "friction"}, []string{"machine"}, []g{{"machine=ws-a", 2, 1, 1}, {"machine=ws-b", 2, 2, 0}}},
			{"two keys, any NULL key excludes the row", ListFilter{}, []string{"project", "category"}, []g{{"category=documentation,project=example", 1, 0, 1}, {"category=tooling,project=example", 1, 1, 0}, {"category=config,project=fleet", 1, 1, 0}}},
			{"three keys", ListFilter{Project: "example"}, []string{"kind", "harness", "model"}, []g{{"harness=claude-code,kind=friction,model=claude-fable-5-1", 1, 0, 1}, {"harness=codex,kind=friction,model=gpt-6", 1, 1, 0}, {"harness=opencode,kind=friction,model=gpt-6", 1, 1, 0}, {"harness=claude-code,kind=install-check,model=claude-fable-5-1", 1, 0, 1}}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				run(func(q Querier) error {
					groups, err := StatsGroups(ctx, q, c.f, c.by)
					if err != nil {
						return err
					}
					if got := render(groups); !reflect.DeepEqual(got, c.want) {
						t.Fatalf("\n got %v\nwant %v", got, c.want)
					}

					return nil
				})
			})
		}
		run(func(q Querier) error {
			groups, err := StatsGroups(ctx, q, ListFilter{}, []string{"schema_version"})
			if err != nil {
				return err
			}
			if _, ok := groups[0].Keys["schema_version"].(int64); !ok {
				t.Fatalf("schema_version key must be an int64, got %T", groups[0].Keys["schema_version"])
			}

			return nil
		})
		for name, by := range map[string][]string{
			"no keys": {}, "four keys": {"kind", "project", "machine", "model"},
			"unknown key": {"payload"}, "repeated key": {"kind", "kind"},
		} {
			if _, _, err := groupsSQL(ListFilter{}, by); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		}
	})

	t.Run("a numeric category is grouped as text", func(t *testing.T) {
		numeric := newSub("u-numeric")
		numeric.Project, numeric.CreatedAt = "numeric", base+day // after the seeded window the series below covers
		numeric.Payload = json.RawMessage(`{"category": 5}`)
		mustInsert(t, db, numeric)
		run(func(q Querier) error {
			groups, err := StatsGroups(ctx, q, ListFilter{Project: "numeric"}, []string{"category"})
			if err != nil {
				return err
			}
			if len(groups) != 1 || groups[0].Keys["category"] != "5" {
				t.Fatalf("got %+v, want one group with category \"5\"", groups)
			}

			return nil
		})
	})

	t.Run("recurring", func(t *testing.T) {
		run(func(q Querier) error {
			got, err := StatsRecurring(ctx, q, ListFilter{}, 10)
			if err != nil {
				return err
			}
			// hash-a occurs three times (rows 1, 2, 7); the newest row
			// describes it. Single hashes are not recurring.
			want := []Recurrence{{ContentHash: "hash-a", Count: 3, FirstID: 1, LastID: 7, Summary: "Config points at stale host", Kind: "friction", Project: "fleet"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("\n got %+v\nwant %+v", got, want)
			}
			// The filter applies before counting.
			got, err = StatsRecurring(ctx, q, ListFilter{Project: "example"}, 10)
			if err != nil {
				return err
			}
			want = []Recurrence{{ContentHash: "hash-a", Count: 2, FirstID: 1, LastID: 2, Summary: "", Kind: "friction", Project: "example"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("filtered\n got %+v\nwant %+v", got, want)
			}
			if got, err = StatsRecurring(ctx, q, ListFilter{}, 0); err != nil || len(got) != 0 {
				t.Fatalf("top=0 must return nothing, got %v, %v", got, err)
			}

			return nil
		})
		if _, _, err := recurringSQL(ListFilter{}, -1); err == nil {
			t.Fatal("negative top must be rejected")
		}
	})

	t.Run("series", func(t *testing.T) {
		run(func(q Querier) error {
			days, err := StatsSeries(ctx, q, ListFilter{Kind: "friction", Until: ptr(base)}, BucketDay)
			if err != nil {
				return err
			}
			wantDays := []Bucket{{"2026-09-22", 1, 0}, {"2026-09-23", 1, 1}, {"2026-09-26", 1, 1}, {"2026-09-28", 1, 1}}
			if !reflect.DeepEqual(days, wantDays) {
				t.Fatalf("days\n got %v\nwant %v", days, wantDays)
			}
			weeks, err := StatsSeries(ctx, q, ListFilter{Until: ptr(base)}, BucketWeek)
			if err != nil {
				return err
			}
			// Rows 1 to 6 fall in the ISO week of Monday 2026-09-21
			// (Tuesday to Sunday), row 7 on Monday 2026-09-28.
			wantWeeks := []Bucket{{"2026-09-21", 6, 3}, {"2026-09-28", 1, 1}}
			if !reflect.DeepEqual(weeks, wantWeeks) {
				t.Fatalf("weeks\n got %v\nwant %v", weeks, wantWeeks)
			}
			// On occurred_at, rows without one are not counted.
			occurred, err := StatsSeries(ctx, q, ListFilter{On: OnOccurredAt, Until: ptr(base)}, BucketWeek)
			if err != nil {
				return err
			}
			wantOccurred := []Bucket{{"2026-09-21", 4, 1}, {"2026-09-28", 1, 1}}
			if !reflect.DeepEqual(occurred, wantOccurred) {
				t.Fatalf("occurred weeks\n got %v\nwant %v", occurred, wantOccurred)
			}

			return nil
		})
		if _, _, err := seriesSQL(ListFilter{}, "month"); err == nil {
			t.Fatal("an unknown bucket must be rejected")
		}
	})

	t.Run("buckets at the boundaries", func(t *testing.T) {
		// The last microsecond of a Sunday is still that Sunday and the week
		// before Monday 00:00:00; the turn of the year keeps the ISO Monday;
		// the ends of the contract's date range stay in range.
		for _, c := range []struct {
			at        string
			day, week string
		}{
			{"2026-09-27T23:59:59.999999Z", "2026-09-27", "2026-09-21"},
			{"2026-09-28T00:00:00Z", "2026-09-28", "2026-09-28"},
			{"2026-10-04T12:00:00Z", "2026-10-04", "2026-09-28"},
			{"2026-01-01T00:00:00Z", "2026-01-01", "2025-12-29"},
			{"2025-01-05T00:00:00Z", "2025-01-05", "2024-12-30"},
			{"2024-12-30T00:00:00Z", "2024-12-30", "2024-12-30"},
			{"1969-12-31T23:59:59.999999Z", "1969-12-31", "1969-12-29"},
			{"0001-01-01T00:00:00Z", "0001-01-01", "0001-01-01"},
			{"9999-12-31T23:59:59.999999Z", "9999-12-31", "9999-12-27"},
		} {
			ts, err := time.Parse(time.RFC3339Nano, c.at)
			if err != nil {
				t.Fatal(err)
			}
			s := newSub("u-bucket-" + c.at)
			s.Project, s.CreatedAt = "buckets", ts.UnixMicro()
			mustInsert(t, db, s)
			run(func(q Querier) error {
				only := ListFilter{Project: "buckets", Since: &s.CreatedAt, Until: &s.CreatedAt}
				days, err := StatsSeries(ctx, q, only, BucketDay)
				if err != nil {
					return err
				}
				if len(days) != 1 || days[0].Bucket != c.day {
					t.Fatalf("%s: got %v, want day %s", c.at, days, c.day)
				}
				weeks, err := StatsSeries(ctx, q, only, BucketWeek)
				if err != nil {
					return err
				}
				if len(weeks) != 1 || weeks[0].Bucket != c.week {
					t.Fatalf("%s: got %v, want week %s", c.at, weeks, c.week)
				}

				return nil
			})
		}
	})

	t.Run("all four parts see one snapshot", func(t *testing.T) {
		run(func(q Querier) error {
			before, err := StatsTotals(ctx, q, ListFilter{Project: "snapshot"})
			if err != nil {
				return err
			}
			late := newSub("u-snapshot")
			late.Project = "snapshot"
			mustInsert(t, db, late)
			groups, err := StatsGroups(ctx, q, ListFilter{Project: "snapshot"}, []string{"kind"})
			if err != nil {
				return err
			}
			series, err := StatsSeries(ctx, q, ListFilter{Project: "snapshot"}, BucketDay)
			if err != nil {
				return err
			}
			if before.Total != 0 || len(groups) != 0 || len(series) != 0 {
				t.Fatalf("the late row leaked into the snapshot: %+v %v %v", before, groups, series)
			}

			return nil
		})
	})
}

func TestMarkUnmarkAndProcessingStates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	a := mustInsert(t, db, newSub("u-a"))
	b := mustInsert(t, db, newSub("u-b"))
	missing := int64(999)

	write := func(fn func(q Querier) error) {
		t.Helper()
		if err := db.Write(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	states := func(ids ...int64) map[int64]ProcessingState {
		t.Helper()
		var out map[int64]ProcessingState
		if err := db.Read(ctx, func(q Querier) error {
			var err error
			out, err = ProcessingStates(ctx, q, ids)

			return err
		}); err != nil {
			t.Fatal(err)
		}

		return out
	}

	// First mark: timestamp and every given field; the unknown id is not counted.
	write(func(q Querier) error {
		n, err := MarkProcessed(ctx, q, []int64{a, b, missing}, base, Mark{
			Verdict: ptr("fixed"), Resolution: ptr("done"), Ref: ptr("example@1a2b3c4"), ProcessedBy: ptr("ws-b/session-7"),
		})
		if n != 2 {
			t.Fatalf("marked %d rows, want 2", n)
		}

		return err
	})
	st := states(a, b, missing)
	if len(st) != 2 {
		t.Fatalf("states for %v, want only the existing rows", st)
	}
	want := ProcessingState{ID: a, ProcessedAt: ptr(base), Verdict: "fixed", Resolution: "done", Ref: "example@1a2b3c4", ProcessedBy: "ws-b/session-7"}
	if !reflect.DeepEqual(st[a], want) {
		t.Fatalf("state\n got %+v\nwant %+v", st[a], want)
	}

	// Re-mark: processed_at is kept, a given field replaces, a nil field
	// keeps, an empty field clears.
	write(func(q Querier) error {
		_, err := MarkProcessed(ctx, q, []int64{a}, base+hour, Mark{Verdict: ptr("duplicate"), Ref: ptr("")})

		return err
	})
	got := mustGet(t, db, a)
	if *got.ProcessedAt != base || got.Verdict != "duplicate" || got.Resolution != "done" || got.Ref != "" || got.ProcessedBy != "ws-b/session-7" {
		t.Fatalf("re-mark: %+v", got)
	}

	// Unmark clears all five, and only for the given ids.
	write(func(q Querier) error {
		n, err := UnmarkProcessed(ctx, q, []int64{a})
		if n != 1 {
			t.Fatalf("unmarked %d rows, want 1", n)
		}

		return err
	})
	got = mustGet(t, db, a)
	if got.ProcessedAt != nil || got.Verdict != "" || got.Resolution != "" || got.Ref != "" || got.ProcessedBy != "" {
		t.Fatalf("unmark left %+v", got)
	}
	if other := mustGet(t, db, b); other.ProcessedAt == nil || other.Verdict != "fixed" {
		t.Fatalf("unmark touched another row: %+v", other)
	}

	// Empty id lists are no-ops.
	write(func(q Querier) error {
		if n, err := MarkProcessed(ctx, q, nil, base, Mark{}); n != 0 || err != nil {
			t.Fatalf("mark nothing: %d, %v", n, err)
		}
		n, err := UnmarkProcessed(ctx, q, nil)
		if n != 0 {
			t.Fatalf("unmark nothing: %d", n)
		}

		return err
	})
}

func TestRedactLeavesATombstone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	s := newSub("u-redact")
	s.Key, s.Summary, s.Machine, s.Project = "k", "secret summary", "ws-a", "example"
	s.Context = json.RawMessage(`{"session_id":"s-1"}`)
	s.Payload = json.RawMessage(`{"category":"config","details":"secret"}`)
	s.OccurredAt = ptr(base - hour)
	id := mustInsert(t, db, s)
	if err := db.Write(ctx, func(q Querier) error {
		_, err := MarkProcessed(ctx, q, []int64{id}, base, Mark{Verdict: ptr("fixed")})

		return err
	}); err != nil {
		t.Fatal(err)
	}

	changed := func(now int64) bool {
		t.Helper()
		var ok bool
		if err := db.Write(ctx, func(q Querier) error {
			var err error
			ok, err = Redact(ctx, q, id, now)

			return err
		}); err != nil {
			t.Fatal(err)
		}

		return ok
	}
	if !changed(base + hour) {
		t.Fatal("first redaction must report a change")
	}
	got := mustGet(t, db, id)
	if string(got.Payload) != Tombstone || got.Summary != "" || got.Context != nil || got.RedactedAt == nil || *got.RedactedAt != base+hour {
		t.Fatalf("tombstone: %+v", got)
	}
	// Identity, envelope and processing fields survive.
	if got.UID != "u-redact" || got.Key != "k" || got.Machine != "ws-a" || got.Project != "example" ||
		got.ContentHash != "h-u-redact" || got.OccurredAt == nil || got.ProcessedAt == nil || got.Verdict != "fixed" {
		t.Fatalf("redaction removed more than it should: %+v", got)
	}
	// Repeating it changes nothing, and neither does an unknown id.
	if changed(base+2*hour) || *mustGet(t, db, id).RedactedAt != base+hour {
		t.Fatal("a second redaction must be a no-op")
	}
	if err := db.Write(ctx, func(q Querier) error {
		ok, err := Redact(ctx, q, 999, base)
		if ok {
			t.Fatal("redacting an unknown id must report no change")
		}

		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The generated column follows the payload: the tombstone has no category.
	if got := listIDs(t, db, ListFilter{Category: "config"}, Page{Limit: 10}); len(got) != 0 {
		t.Fatalf("a tombstone must not match its old category: %v", got)
	}
	// A keyed lookup still finds it, hash intact, and it can still be marked.
	if err := db.Write(ctx, func(q Querier) error {
		byKey, err := GetByKey(ctx, q, "friction", "k")
		if err != nil {
			return err
		}
		if byKey.ContentHash != "h-u-redact" {
			t.Fatalf("hash after redaction %q", byKey.ContentHash)
		}
		n, err := MarkProcessed(ctx, q, []int64{id}, base, Mark{Verdict: ptr("invalid")})
		if n != 1 {
			t.Fatalf("marking a tombstone touched %d rows", n)
		}

		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIterateExportOrderAndFilters(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	seedListSet(t, db)

	collect := func(f ExportFilter) []int64 {
		t.Helper()
		var out []int64
		if err := db.Read(ctx, func(q Querier) error {
			return Iterate(ctx, q, f, func(s Submission) error {
				out = append(out, s.ID)
				if s.ID == 2 && string(s.Payload) != Tombstone {
					t.Fatalf("tombstones export as tombstones, got %s", s.Payload)
				}

				return nil
			})
		}); err != nil {
			t.Fatal(err)
		}

		return out
	}
	cases := []struct {
		name string
		f    ExportFilter
		want []int64
	}{
		{"everything ascending", ExportFilter{}, []int64{1, 2, 3, 4, 5, 6, 7}},
		{"kind", ExportFilter{Kind: "friction"}, []int64{1, 2, 5, 7}},
		{"since inclusive", ExportFilter{Since: ptr(base - 2*day)}, []int64{5, 6, 7}},
		{"after_id zero", ExportFilter{AfterID: ptr(int64(0))}, []int64{1, 2, 3, 4, 5, 6, 7}},
		{"after_id", ExportFilter{AfterID: ptr(int64(5))}, []int64{6, 7}},
		{"limit", ExportFilter{Limit: 2}, []int64{1, 2}},
		{"after_id with limit pages", ExportFilter{AfterID: ptr(int64(2)), Limit: 2}, []int64{3, 4}},
		{"all together", ExportFilter{Kind: "friction", Since: ptr(base - 5*day), AfterID: ptr(int64(2)), Limit: 1}, []int64{5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := collect(c.f); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
	// The callback's error stops the iteration and is returned as is.
	stop := errors.New("stop")
	var seen int
	err := db.Read(ctx, func(q Querier) error {
		return Iterate(ctx, q, ExportFilter{}, func(Submission) error {
			seen++

			return stop
		})
	})
	if !errors.Is(err, stop) || seen != 1 {
		t.Fatalf("iterate must stop on the callback's error: %v after %d rows", err, seen)
	}
	var n int64
	if err := db.Read(ctx, func(q Querier) error {
		var err error
		n, err = CountAll(ctx, q)

		return err
	}); err != nil || n != 7 {
		t.Fatalf("CountAll %d, %v", n, err)
	}
}

func TestLookupsReportNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	if err := db.Read(ctx, func(q Querier) error {
		if _, err := GetByID(ctx, q, 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetByID: %v", err)
		}
		if _, err := GetByUID(ctx, q, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetByUID: %v", err)
		}
		if _, err := GetByKey(ctx, q, "friction", "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetByKey: %v", err)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQSearchesFromEveryReaderConnection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTest(t)
	s := newSub("u-q")
	s.Summary = "Straße ÉCOLE"
	s.Payload = json.RawMessage(`{"details":{"nested":["Kelvin K sign"]}}`)
	mustInsert(t, db, s)

	// More goroutines than reader connections, so the registered function
	// is exercised on every physical connection the pool opens.
	var wg sync.WaitGroup
	errs := make(chan error, 4*readerMaxConns)
	for i := 0; i < 4*readerMaxConns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// "ß" lower-cases to itself, never to "ss"; the Kelvin sign
			// (U+212A) lower-cases to the ASCII k; a non-substring misses.
			needle := []string{"straße école", "STRASSE", "KELVIN \u212a", "kelvin sign"}[i%4]
			wantHit := i%4 == 0 || i%4 == 2
			err := db.Read(ctx, func(q Querier) error {
				rows, err := List(ctx, q, ListFilter{Q: needle}, Page{Limit: 1}, false)
				if err != nil {
					return err
				}
				if (len(rows) == 1) != wantHit {
					return fmt.Errorf("q=%q: %d rows, want hit=%v", needle, len(rows), wantHit)
				}

				return nil
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestQStopsAtAnEmbeddedNul(t *testing.T) {
	t.Parallel()

	// Pins the documented limit of qClause: text before a NUL is searched,
	// text after it is not. If this starts failing on the "after" cases the
	// driver has changed how it passes TEXT arguments and the comment on
	// qClause can go.
	db := openTest(t)
	s := newSub("u-nul")
	s.Summary = "head\x00Tail of the summary"
	s.Payload = json.RawMessage(`{"details":"before\u0000Needle after"}`)
	mustInsert(t, db, s)

	for _, needle := range []string{"head", "before"} {
		if got := listIDs(t, db, ListFilter{Q: needle}, Page{Limit: 10}); len(got) != 1 {
			t.Fatalf("q=%q: %v, want the row", needle, got)
		}
	}
	for _, needle := range []string{"needle after", "tail of the summary"} {
		if got := listIDs(t, db, ListFilter{Q: needle}, Page{Limit: 10}); len(got) != 0 {
			t.Fatalf("q=%q: %v, the text after a NUL is documented as unsearchable", needle, got)
		}
	}
}

func TestOriginFilterAndGroups(t *testing.T) {
	t.Parallel()

	db := openTest(t)
	ctx := context.Background()
	for i, c := range []string{`{"origin":"agent"}`, `{"origin":"session-scan"}`, `{"origin":"agent","client":"x"}`, `{"client":"x"}`, ""} {
		s := newSub(fmt.Sprintf("o-%d", i+1))
		if c != "" {
			s.Context = json.RawMessage(c)
		}
		mustInsert(t, db, s)
	}
	if got := listIDs(t, db, ListFilter{Origin: "agent"}, Page{Limit: 10}); !reflect.DeepEqual(got, []int64{3, 1}) {
		t.Fatalf("origin agent: got %v, want [3 1]", got)
	}
	if got := listIDs(t, db, ListFilter{Origin: "Agent"}, Page{Limit: 10}); len(got) != 0 {
		t.Fatalf("origin matches exactly, got %v", got)
	}
	if err := db.Read(ctx, func(q Querier) error {
		groups, err := StatsGroups(ctx, q, ListFilter{}, []string{"origin"})
		if err != nil {
			return err
		}
		if len(groups) != 2 || groups[0].Keys["origin"] != "agent" || groups[0].Total != 2 ||
			groups[1].Keys["origin"] != "session-scan" || groups[1].Total != 1 {
			t.Fatalf("group by origin: got %+v", groups)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
