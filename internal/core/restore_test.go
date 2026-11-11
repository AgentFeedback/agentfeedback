package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

// reframe rebuilds an export body around the given record lines.
func reframe(header string, records ...string) []byte {
	d := sha256.New()
	for _, l := range records {
		d.Write([]byte(l + "\n"))
	}
	lines := append([]string{header}, records...)
	lines = append(lines, fmt.Sprintf(`{"export_complete":true,"count":%d,"first_id":null,"last_id":null,"sha256":"%s"}`,
		len(records), hex.EncodeToString(d.Sum(nil))))
	return []byte(strings.Join(lines, "\n") + "\n")
}

var idRe = regexp.MustCompile(`^\{"id":[0-9]+,`)

// withID replaces a record line's id.
func withID(line string, id int64) string {
	return idRe.ReplaceAllString(line, fmt.Sprintf(`{"id":%d,`, id))
}

func TestRestoreKeepsIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, _, _ := newTestService(t)
	seedTransfer(t, src)
	header, lines, _, _ := exportLines(t, src, ExportParams{})
	// Gaps in the ids must survive the restore.
	for i := range lines {
		lines[i] = withID(lines[i], int64(10*(i+1)))
	}
	body := reframe(header, lines...)

	dst, db, _ := newTestService(t)
	dry, err := dst.Restore(ctx, body, true)
	if err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, db); n != 0 {
		t.Fatalf("dry run wrote %d rows", n)
	}
	res, err := dst.Restore(ctx, body, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 5 || res.Skipped != 0 || len(res.Conflicts) != 0 || *res.FirstID != 10 || *res.LastID != 50 {
		t.Fatalf("restore %+v", res)
	}
	dj, _ := Marshal(dry)
	rj, _ := Marshal(res)
	if string(dj) != string(rj) {
		t.Errorf("dry run %s, real run %s", dj, rj)
	}
	for _, w := range res.Warnings {
		if w.Line == 5 {
			t.Errorf("tombstone (line 5) reported warning %+v", w)
		}
	}
	_, got, _, _ := exportLines(t, dst, ExportParams{})
	if !slices.Equal(got, lines) {
		t.Errorf("record lines differ\n got %v\nwant %v", got, lines)
	}
	if !strings.Contains(got[0], `"verdict":"fixed","resolution":"r","ref":"x@1","processed_by":"me"`) ||
		!strings.Contains(got[3], `"redacted_at"`) {
		t.Errorf("processing fields or tombstone lost: %v", got)
	}

	again, err := dst.Restore(ctx, body, false)
	if err != nil || again.Imported != 0 || again.Skipped != 5 || len(again.Conflicts) != 0 || again.FirstID != nil {
		t.Fatalf("re-run %+v %v", again, err)
	}
	if n := rowCount(t, db); n != 5 {
		t.Errorf("%d rows after re-run", n)
	}
	if c := mustCreate(t, dst, `{"summary":"after"}`); c.Record.ID <= 50 {
		t.Errorf("create after restore got id %d", c.Record.ID)
	}
}

func TestRestoreConflictsAndSequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, _, _ := newTestService(t)
	mustCreate(t, src, `{"kind":"friction","key":"a","summary":"one"}`)
	mustCreate(t, src, `{"summary":"two"}`)
	header, lines, _, _ := exportLines(t, src, ExportParams{})

	// ids {5,9} into an empty database: the next create is past 9.
	fresh, _, _ := newTestService(t)
	if _, err := fresh.Restore(ctx, reframe(header, withID(lines[0], 5), withID(lines[1], 9)), false); err != nil {
		t.Fatal(err)
	}
	if c := mustCreate(t, fresh, `{"summary":"next"}`); c.Record.ID < 10 {
		t.Errorf("create after restoring {5,9} got id %d", c.Record.ID)
	}

	// id 1 is held by another uid, key a under another uid with another hash.
	dst, db, _ := newTestService(t)
	mustCreate(t, dst, `{"kind":"friction","key":"a","summary":"different"}`)
	body := reframe(header, withID(lines[0], 7), withID(lines[1], 1))
	dry, err := dst.Restore(ctx, body, true)
	if err != nil {
		t.Fatal(err)
	}
	res, err := dst.Restore(ctx, body, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 0 || res.Skipped != 2 || len(res.Conflicts) != 2 || res.FirstID != nil {
		t.Fatalf("restore %+v", res)
	}
	if c := res.Conflicts[0]; c.Line != 2 || c.ExistingID != 1 || c.Reason != "key_mismatch" || !strings.Contains(lines[0], c.UID) {
		t.Errorf("conflict %+v", c)
	}
	if c := res.Conflicts[1]; c.Line != 3 || c.ExistingID != 1 || c.Reason != "id_taken" || !strings.Contains(lines[1], c.UID) {
		t.Errorf("conflict %+v", c)
	}
	dj, _ := Marshal(dry)
	rj, _ := Marshal(res)
	if string(dj) != string(rj) {
		t.Errorf("dry run %s, real run %s", dj, rj)
	}
	if n := rowCount(t, db); n != 1 {
		t.Errorf("%d rows", n)
	}
	// The highest id (7) was skipped, yet the sequence passed it.
	if c := mustCreate(t, dst, `{"summary":"next"}`); c.Record.ID < 8 {
		t.Errorf("create after a skipped id 7 got id %d", c.Record.ID)
	}
}

func TestRestoreRejectsWithoutWriting(t *testing.T) {
	t.Parallel()
	src, _, _ := newTestService(t)
	seedTransfer(t, src)
	_, _, _, body := exportLines(t, src, ExportParams{})
	bad := []byte(strings.Replace(string(body), `"sha256":"`, `"sha256":"0`, 1))
	dst, db, _ := newTestService(t)
	_, err := dst.Restore(context.Background(), bad, false)
	if p := asProblem(t, err); p.Code != CodeValidation || !strings.Contains(p.Message, "line 7: trailer sha256") {
		t.Errorf("got %s %q", p.Code, p.Message)
	}
	if n := rowCount(t, db); n != 0 {
		t.Errorf("%d rows written", n)
	}
}

func TestRestoreRejectsMaxIDBeforeWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, _, _ := newTestService(t)
	mustCreate(t, src, `{"kind":"friction","key":"limit","summary":"source"}`)
	mustCreate(t, src, `{"summary":"valid earlier row"}`)
	header, lines, _, _ := exportLines(t, src, ExportParams{})
	body := reframe(header, withID(lines[1], 10), withID(lines[0], math.MaxInt64))
	for _, existing := range []string{"none", "uid", "key", "id"} {
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dry=%t", existing, dry), func(t *testing.T) {
				dst, db, _ := newTestService(t)
				switch existing {
				case "uid":
					if _, err := dst.Import(ctx, reframe(header, lines[0])); err != nil {
						t.Fatal(err)
					}
				case "key":
					mustCreate(t, dst, `{"kind":"friction","key":"limit","summary":"different"}`)
				case "id":
					mustCreate(t, dst, `{"summary":"occupied"}`)
					if err := db.Write(ctx, func(q store.Querier) error {
						_, err := q.ExecContext(ctx, "UPDATE submissions SET id = ? WHERE id = 1", int64(math.MaxInt64))
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				sequence := func() int64 {
					var n int64
					if err := db.Read(ctx, func(q store.Querier) error {
						return q.QueryRowContext(ctx, "SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'submissions'), 0)").Scan(&n)
					}); err != nil {
						t.Fatal(err)
					}
					return n
				}
				beforeSeq := sequence()
				_, before, _, _ := exportLines(t, dst, ExportParams{})
				_, err := dst.Restore(ctx, body, dry)
				p := asProblem(t, err)
				if p.Code != CodeValidation || !strings.Contains(p.Message, "line 3:") ||
					len(p.Details) != 1 || p.Details[0].Pointer != "/id" || p.Details[0].Code != "invalid_value" {
					t.Fatalf("wrong rejection: %+v", p)
				}
				_, after, _, _ := exportLines(t, dst, ExportParams{})
				if !slices.Equal(before, after) || sequence() != beforeSeq {
					t.Fatal("rejected restore changed rows or sequence")
				}
			})
		}
	}
}

func TestRestoreNearMaxIDAndImportMaxID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, _, _ := newTestService(t)
	mustCreate(t, src, `{"summary":"source"}`)
	header, lines, _, _ := exportLines(t, src, ExportParams{})
	dst, _, _ := newTestService(t)
	if _, err := dst.Restore(ctx, reframe(header, withID(lines[0], math.MaxInt64-1)), false); err != nil {
		t.Fatal(err)
	}
	if got := mustCreate(t, dst, `{"summary":"last available"}`).Record.ID; got != math.MaxInt64 {
		t.Fatalf("last ID = %d", got)
	}
	if _, err := dst.Create(ctx, []byte(`{"summary":"exhausted"}`)); err == nil {
		t.Fatal("exhausted sequence accepted another distinct create")
	}
	fresh, _, _ := newTestService(t)
	res, err := fresh.Import(ctx, reframe(header, withID(lines[0], math.MaxInt64)))
	if err != nil || res.Imported != 1 || res.FirstID == nil || *res.FirstID != 1 {
		t.Fatalf("HTTP import policy: %+v, %v", res, err)
	}
}
