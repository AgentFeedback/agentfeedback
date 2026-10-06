package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
)

type decodeExpectation struct {
	Status   int    `json:"status"`
	Error    string `json:"error"`
	Warnings []struct {
		Code    string `json:"code"`
		Pointer string `json:"pointer"`
	} `json:"warnings"`
}

// TestCreateDecodeFixtures runs every decode fixture through Create on a
// fresh database: a rejection is a Problem with the fixture's status and
// code; an accepted body is 201 with the fixture's warning multiset and the
// stored envelope equal to stored.json.
func TestCreateDecodeFixtures(t *testing.T) {
	t.Parallel()
	for _, dir := range decodeFixtures(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join(dir, "expected.json"))
			if err != nil {
				t.Fatal(err)
			}
			var want decodeExpectation
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			s, _, _ := newTestService(t)
			res, err := s.Create(context.Background(), fixtureBody(t, dir))
			if want.Status != 201 {
				p := asProblem(t, err)
				if p.Status != want.Status || p.Code != want.Error {
					t.Fatalf("got %d %s, want %d %s", p.Status, p.Code, want.Status, want.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("want 201, got %v", err)
			}
			if !res.Created {
				t.Error("a fresh database returned an existing row")
			}
			var got, exp []string
			for _, d := range res.Warnings {
				got = append(got, d.Code+" "+d.Pointer)
			}
			for _, w := range want.Warnings {
				exp = append(exp, w.Code+" "+w.Pointer)
			}
			sort.Strings(got)
			sort.Strings(exp)
			if !slices.Equal(got, exp) {
				t.Errorf("warnings\n got %v\nwant %v", got, exp)
			}
			stored, err := os.ReadFile(filepath.Join(dir, "stored.json"))
			if err != nil {
				t.Fatal(err)
			}
			rec, err := s.Get(context.Background(), res.Record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mine := storedEnvelope(t, rec); !bytes.Equal(mine, stored) {
				t.Errorf("stored envelope\n got %.300s\nwant %.300s", mine, stored)
			}
			if !bytes.Equal(rec.AppendJSON(nil), res.Record.AppendJSON(nil)) {
				t.Error("the created record differs from the record read back")
			}
		})
	}
}

// TestCreateHashVectors stores every hash vector through Create and checks
// content_hash against the vector's sha256.
func TestCreateHashVectors(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestService(t)
	for _, dir := range fixtureDirs(t, "hash") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, "expected.json"))
			if err != nil {
				t.Fatal(err)
			}
			var want struct {
				SHA256 string `json:"sha256"`
			}
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			res, err := s.Create(context.Background(), fixtureBody(t, dir))
			if err != nil {
				t.Fatal(err)
			}
			if res.Record.ContentHash != want.SHA256 {
				t.Errorf("content_hash %s, want %s", res.Record.ContentHash, want.SHA256)
			}
		})
	}
}

// TestFixturesRoundTrip stores every accepted fixture and hash vector,
// exports, imports into an empty database and exports again: the record
// lines are byte-equal, so the decoder is idempotent on its own output and
// number spellings, hashes and the inferred kind survive.
func TestFixturesRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, _, _ := newTestService(t)
	dirs := append(decodeFixtures(t), fixtureDirs(t, "hash")...)
	unknownKind := 0
	for _, dir := range dirs {
		res, err := src.Create(ctx, fixtureBody(t, dir))
		if err != nil {
			if p := asProblem(t, err); p.Status == 409 {
				continue // a key reused by another fixture
			}
			continue // a rejected fixture
		}
		if res.Record.Kind == store.KindUnknown {
			unknownKind++
		}
	}
	if unknownKind == 0 {
		t.Fatal("no fixture stored kind unknown; the inferred-kind round trip is untested")
	}
	_, want, _, body := exportLines(t, src, ExportParams{})
	t.Logf("%d records, %d export bytes", len(want), len(body))

	dst, db, _ := newTestService(t)
	res, err := dst.Import(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != len(want) || res.Skipped != 0 || len(res.Conflicts) != 0 {
		t.Fatalf("imported %d skipped %d conflicts %v, want %d 0 none", res.Imported, res.Skipped, res.Conflicts, len(want))
	}
	if n := rowCount(t, db); n != int64(len(want)) {
		t.Fatalf("%d rows after import, want %d", n, len(want))
	}
	_, got, _, _ := exportLines(t, dst, ExportParams{})
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record line %d differs after the round trip\n got %.400s\nwant %.400s", i+2, got[i], want[i])
		}
	}
}

// TestRoundTripOverBodyLimit stores an envelope larger than BodyLimit
// (invalid UTF-8 expands to U+FFFD) and imports its export.
func TestRoundTripOverBodyLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	prefix, suffix := `{"kind":"blob","summary":"large","payload":{"d":"`, `"}}`
	body := []byte(prefix)
	body = append(body, bytes.Repeat([]byte{0xff}, envelope.BodyLimit-len(prefix)-len(suffix))...)
	body = append(body, suffix...)
	src, _, _ := newTestService(t)
	res, err := src.Create(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Record.Payload); n <= envelope.BodyLimit {
		t.Fatalf("stored payload is %d bytes, not over BodyLimit", n)
	}
	_, want, _, export := exportLines(t, src, ExportParams{})
	dst, _, _ := newTestService(t)
	if _, err := dst.Import(ctx, export); err != nil {
		t.Fatal(err)
	}
	_, got, _, _ := exportLines(t, dst, ExportParams{})
	if !slices.Equal(got, want) {
		t.Error("record line differs after the round trip")
	}
	if !strings.Contains(want[0], "�") {
		t.Error("no U+FFFD in the stored record")
	}
}
