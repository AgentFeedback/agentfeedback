package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// seedTransfer stores ordinary, keyed, marked and redacted rows.
func seedTransfer(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	mustCreate(t, s, `{"kind":"friction","key":"a","summary":"<b>&amp;</b>","machine":"m","model":"x","payload":{"n":1.50,"big":123456789012345678901234567890,"s":" "}}`)
	mustCreate(t, s, `{"summary":"flat","category":"tooling","occurred_at":"2026-01-02T03:04:05.123456789+02:00"}`)
	mustCreate(t, s, `{"kind":"review","key":"b","summary":"review","context":{"a":"1","b":2},"payload":{"reviewers":[]}}`)
	mustCreate(t, s, `{"kind":"friction","summary":"to redact","context":{"cwd":"/x"},"payload":{"details":"secret"}}`)
	mustCreate(t, s, `{"kind":"note","summary":"lookalike","payload":{"redacted":true}}`)
	if _, err := s.MarkBatch(ctx, []byte(`{"ids":[1,4],"verdict":"fixed","resolution":"r","ref":"x@1","processed_by":"me"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redact(ctx, 4); err != nil {
		t.Fatal(err)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, _, clock := newTestService(t)
	seedTransfer(t, src)
	header, want, trailer, body := exportLines(t, src, ExportParams{})
	if header != `{"export_format":2,"kind":null,"since":null,"after_id":null,"exported_at":"2026-09-27T10:00:01.123456Z"}` {
		t.Errorf("header %s", header)
	}
	digest := sha256.New()
	for _, l := range want {
		digest.Write([]byte(l + "\n"))
	}
	if wantTrailer := fmt.Sprintf(`{"export_complete":true,"count":5,"first_id":1,"last_id":5,"sha256":"%s"}`,
		hex.EncodeToString(digest.Sum(nil))); trailer != wantTrailer {
		t.Errorf("trailer %s\nwant %s", trailer, wantTrailer)
	}
	if !strings.Contains(want[0], `<b>&amp;</b>`) || !strings.Contains(want[0], " ") ||
		!strings.Contains(want[0], `"big":123456789012345678901234567890`) || !strings.Contains(want[0], `"n":1.50`) {
		t.Errorf("record line escapes or reformats: %s", want[0])
	}

	dst, db, _ := newTestService(t)
	clock.advance(time.Hour)
	res, err := dst.Import(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 5 || res.Skipped != 0 || len(res.Conflicts) != 0 || *res.FirstID != 1 || *res.LastID != 5 {
		t.Fatalf("import %+v", res)
	}
	for _, w := range res.Warnings {
		if w.Line == 5 {
			t.Errorf("tombstone (line 5) reported warning %+v", w)
		}
	}
	_, got, _, _ := exportLines(t, dst, ExportParams{})
	if !slices.Equal(got, want) {
		t.Errorf("record lines differ\n got %v\nwant %v", got, want)
	}

	again, err := dst.Import(ctx, body)
	if err != nil || again.Imported != 0 || again.Skipped != 5 || again.FirstID != nil {
		t.Fatalf("re-import %+v %v", again, err)
	}
	out, _ := Marshal(again)
	if string(out) != `{"imported":0,"skipped":5,"conflicts":[],"warnings":[]}` {
		t.Errorf("re-import JSON %s", out)
	}
	if n := rowCount(t, db); n != 5 {
		t.Errorf("%d rows after re-import", n)
	}

	// A key taken under another uid: same hash is skipped, different is a conflict.
	other, _, _ := newTestService(t)
	mustCreate(t, other, `{"kind":"friction","key":"a","summary":"different"}`)
	mustCreate(t, other, `{"kind":"review","key":"b","summary":"review","context":{"x":"y"},"payload":{"reviewers":[]}}`)
	res, err = other.Import(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 3 || res.Skipped != 2 || len(res.Conflicts) != 1 {
		t.Fatalf("conflict import %+v", res)
	}
	if c := res.Conflicts[0]; c.Line != 2 || c.ExistingID != 1 || c.Reason != "key_mismatch" || !strings.Contains(want[0], c.UID) {
		t.Errorf("conflict %+v", c)
	}
}

func TestImportFilters(t *testing.T) {
	t.Parallel()
	src, _, _ := newTestService(t)
	seedTransfer(t, src)
	header, lines, trailer, _ := exportLines(t, src, ExportParams{Kind: " Friction ", AfterID: ptr(int64(0)), Limit: ptr(1)})
	if !strings.HasPrefix(header, `{"export_format":2,"kind":"friction","since":null,"after_id":0,`) || len(lines) != 1 ||
		!strings.Contains(trailer, `"count":1,"first_id":1,"last_id":1`) {
		t.Errorf("filtered export\n%s\n%v\n%s", header, lines, trailer)
	}
	since := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	header, lines, trailer, _ = exportLines(t, src, ExportParams{Since: &since})
	if !strings.Contains(header, `"since":"2030-01-01T00:00:00.000000Z"`) || len(lines) != 0 ||
		!strings.Contains(trailer, `"count":0,"first_id":null,"last_id":null`) {
		t.Errorf("empty export\n%s\n%v\n%s", header, lines, trailer)
	}
}

func TestImportRejects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, _, _ := newTestService(t)
	seedTransfer(t, src)
	header, lines, trailer, body := exportLines(t, src, ExportParams{})
	join := func(ls ...string) []byte { return []byte(strings.Join(ls, "\n") + "\n") }
	resum := func(records ...string) string {
		d := sha256.New()
		for _, l := range records {
			d.Write([]byte(l + "\n"))
		}
		return fmt.Sprintf(`{"export_complete":true,"count":%d,"first_id":1,"last_id":1,"sha256":"%s"}`, len(records), hex.EncodeToString(d.Sum(nil)))
	}
	with := func(record string) []byte { return join(header, record, resum(record)) }
	tombstone := lines[3]

	cases := []struct {
		name, want string
		body       []byte
	}{
		{"tampered count", "line 7: trailer count", []byte(strings.Replace(string(body), `"count":5`, `"count":4`, 1))},
		{"tampered sha256", "line 7: trailer sha256", []byte(strings.Replace(string(body), `"sha256":"`, `"sha256":"0`, 1))},
		{"tampered record", "trailer sha256", []byte(strings.Replace(string(body), "flat", "flap", 1))},
		{"missing trailer", "line 6: the last line must be the trailer", join(append([]string{header}, lines...)...)},
		{"wrong format", "line 1: export_format must be 2", join(strings.Replace(header, `:2`, `:1`, 1), trailer)},
		{"blank line", "line 2: blank line", join(header, "", trailer)},
		{"crlf", "line 1: line ends in CR", []byte(strings.ReplaceAll(string(body), "\n", "\r\n"))},
		{"hash mismatch", "line 2: content_hash", with(strings.Replace(lines[1], `"summary":"flat"`, `"summary":"flap"`, 1))},
		{"unknown member", "line 2: member \"extra\"", with(strings.Replace(lines[1], `{"id"`, `{"extra":1,"id"`, 1))},
		{"duplicate member", "line 2: member \"id\" appears", with(strings.Replace(lines[1], `{"id":2`, `{"id":2,"id":2`, 1))},
		{"missing uid", "line 2: uid is required", with(regexp.MustCompile(`"uid":"[^"]*",`).ReplaceAllString(lines[1], ""))},
		{"bad uid", "line 2: uid must be a UUID", with(strings.Replace(lines[1], `"uid":"`, `"uid":"x`, 1))},
		{"bad created_at", "line 2: created_at must be an RFC 3339", with(strings.Replace(lines[1], `"created_at":"2026`, `"created_at":"x2026`, 1))},
		{"verdict without processed_at", "line 2: verdict is present", with(strings.Replace(lines[1], `"content_hash"`, `"verdict":"v","content_hash"`, 1))},
		{"tombstone with summary", "line 2: a redacted record has no summary", with(strings.Replace(tombstone, `"content_hash"`, `"summary":"s","content_hash"`, 1))},
		{"tombstone payload", "line 2: a redacted record's payload", with(strings.Replace(tombstone, `{"redacted":true}`, `{"redacted":false}`, 1))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst, db, _ := newTestService(t)
			_, err := dst.Import(ctx, c.body)
			p := asProblem(t, err)
			if p.Status != 400 || p.Code != CodeValidation || !strings.Contains(p.Message, c.want) {
				t.Errorf("got %d %s %q, want 400 validation_error containing %q", p.Status, p.Code, p.Message, c.want)
			}
			if n := rowCount(t, db); n != 0 {
				t.Errorf("%d rows written", n)
			}
		})
	}

	s, _, _ := newTestService(t)
	big := make([]byte, ImportLimit+1)
	if _, err := s.Import(ctx, big); asProblem(t, err).Status != 413 {
		t.Errorf("oversize import: %v", err)
	}
	// A tombstone keeps its exported hash; an ordinary record whose payload
	// looks like one is hashed like any other.
	if _, err := s.Import(ctx, with(tombstone)); err != nil {
		t.Errorf("tombstone import: %v", err)
	}
	if !bytes.Contains([]byte(lines[4]), []byte(`"payload":{"redacted":true}`)) || bytes.Contains([]byte(lines[4]), []byte("redacted_at")) {
		t.Fatalf("line 5 is not the lookalike: %s", lines[4])
	}
	if _, err := s.Import(ctx, with(lines[4])); err != nil {
		t.Errorf("lookalike import: %v", err)
	}
}
