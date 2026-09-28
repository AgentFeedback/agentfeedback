package core

import (
	"bytes"
	"context"
	"slices"
	"strconv"
	"testing"
	"time"
)

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func TestRedactAndMark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, clock := newTestService(t)
	body := `{"kind":"friction","key":"r1","summary":"secret","machine":"m","model":"x","harness":"cc","project":"p",` +
		`"occurred_at":"2026-09-27T09:58:12Z","context":{"cwd":"/home/x"},"payload":{"category":"tooling","details":"token=abc"}}`
	orig := mustCreate(t, s, body).Record
	marked, err := s.Mark(ctx, orig.ID, []byte(`{"verdict":"Fixed","resolution":" done ","ref":"r","processed_by":"me"}`))
	if err != nil {
		t.Fatal(err)
	}
	if marked.ProcessedAt == nil || marked.Verdict != "fixed" || marked.Resolution != "done" {
		t.Fatalf("mark: %+v", marked.Submission)
	}
	clock.advance(time.Hour)

	tomb, err := s.Redact(ctx, orig.ID)
	if err != nil {
		t.Fatal(err)
	}
	o, r := orig.Submission, tomb.Submission
	if r.ID != o.ID || r.UID != o.UID || r.ContentHash != o.ContentHash || r.Kind != o.Kind ||
		r.SchemaVersion != o.SchemaVersion || r.Key != o.Key || r.Machine != o.Machine || r.Model != o.Model ||
		r.Harness != o.Harness || r.Project != o.Project || *r.OccurredAt != *o.OccurredAt || r.CreatedAt != o.CreatedAt {
		t.Errorf("tombstone lost a kept field:\n%s\n%s", orig.AppendJSON(nil), tomb.AppendJSON(nil))
	}
	if *r.ProcessedAt != *marked.ProcessedAt || r.Verdict != "fixed" || r.Resolution != "done" || r.Ref != "r" || r.ProcessedBy != "me" {
		t.Errorf("tombstone lost the processing fields: %s", tomb.AppendJSON(nil))
	}
	if r.Summary != "" || r.Context != nil || string(r.Payload) != `{"redacted":true}` || r.RedactedAt == nil {
		t.Errorf("tombstone keeps content: %s", tomb.AppendJSON(nil))
	}

	clock.advance(time.Hour)
	again, err := s.Redact(ctx, orig.ID)
	if err != nil || !bytes.Equal(again.AppendJSON(nil), tomb.AppendJSON(nil)) {
		t.Errorf("repeat redact changed the tombstone: %v\n%s\n%s", err, again.AppendJSON(nil), tomb.AppendJSON(nil))
	}
	replay := mustCreate(t, s, body)
	if replay.Created || !bytes.Equal(replay.Record.AppendJSON(nil), tomb.AppendJSON(nil)) {
		t.Errorf("keyed replay of redacted content: created %v %s", replay.Created, replay.Record.AppendJSON(nil))
	}

	// Re-mark: processed_at kept, given fields replaced, absent ones kept.
	remarked, err := s.Mark(ctx, orig.ID, []byte(`{"verdict":"invalid","resolution":"not a bug"}`))
	if err != nil {
		t.Fatal(err)
	}
	if *remarked.ProcessedAt != *marked.ProcessedAt || remarked.Verdict != "invalid" || remarked.Resolution != "not a bug" ||
		remarked.Ref != "r" || remarked.ProcessedBy != "me" || remarked.RedactedAt == nil {
		t.Errorf("re-mark: %s", remarked.AppendJSON(nil))
	}
	cleared, err := s.Mark(ctx, orig.ID, []byte(`{"processed":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if c := cleared.Submission; c.ProcessedAt != nil || c.Verdict != "" || c.Resolution != "" || c.Ref != "" || c.ProcessedBy != "" {
		t.Errorf("processed=false left fields: %s", cleared.AppendJSON(nil))
	}

	if _, err := s.Redact(ctx, 999); asProblem(t, err).Status != 404 {
		t.Errorf("redact absent: %v", err)
	}
	if _, err := s.Mark(ctx, 999, []byte(`{}`)); asProblem(t, err).Status != 404 {
		t.Errorf("mark absent: %v", err)
	}
	if _, err := s.Get(ctx, 999); asProblem(t, err).Status != 404 {
		t.Errorf("get absent: %v", err)
	}
}

func TestMarkBatchClassification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, _ := newTestService(t)
	for i := range 4 {
		mustCreate(t, s, `{"kind":"k","summary":"row `+strconv.Itoa(i)+`"}`)
	}
	res, err := s.MarkBatch(ctx, []byte(`{"ids":[3,1,1,99,2],"verdict":"fixed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Updated, []int64{1, 2, 3}) || len(res.Unchanged) != 0 || !slices.Equal(res.NotFound, []int64{99}) {
		t.Fatalf("first batch %+v", res)
	}
	res, err = s.MarkBatch(ctx, []byte(`{"ids":[1,2,4],"verdict":"fixed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Updated, []int64{4}) || !slices.Equal(res.Unchanged, []int64{1, 2}) || len(res.NotFound) != 0 {
		t.Fatalf("second batch %+v", res)
	}
	res, err = s.MarkBatch(ctx, []byte(`{"ids":[1,4],"verdict":"wont fix"}`))
	if err != nil || !slices.Equal(res.Updated, []int64{1, 4}) || *res.Verdict != "wont-fix" {
		t.Fatalf("verdict change %+v %v", res, err)
	}
	res, err = s.MarkBatch(ctx, []byte(`{"ids":[1,2,3,4],"processed":false}`))
	if err != nil || !slices.Equal(res.Updated, []int64{1, 2, 3, 4}) {
		t.Fatalf("unmark %+v %v", res, err)
	}
	res, err = s.MarkBatch(ctx, []byte(`{"ids":[1],"processed":false}`))
	if err != nil || !slices.Equal(res.Unchanged, []int64{1}) || len(res.Updated) != 0 {
		t.Fatalf("unmark again %+v %v", res, err)
	}
	out, err := Marshal(res)
	if err != nil || string(out) != `{"processed":false,"updated":[],"unchanged":[1],"not_found":[]}` {
		t.Errorf("batch JSON %s %v", out, err)
	}
}
