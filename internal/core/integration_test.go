package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

func reviewInput(runID string) CreateReviewInput {
	return CreateReviewInput{
		Skill: "review-panel", MachineName: "workstation-a", CoordinatorModel: "claude-fable-5",
		RunID: runID, Prompt: "review this",
		Reviewers: []ReviewerInput{{Slot: "gpt56", Model: "openai-codex/gpt-5.6-sol", Status: "completed"}},
	}
}

func frictionInput(summary string) CreateFrictionInput {
	return CreateFrictionInput{
		MachineName: "workstation-a", CoordinatorModel: "claude-fable-5",
		Category: "documentation", Summary: summary,
	}
}

func eventInput(key string, payload string) CreateEventInput {
	return CreateEventInput{
		Kind: "deploy", Key: key, MachineName: "workstation-a",
		CoordinatorModel: "claude-fable-5", Payload: []byte(payload),
	}
}

func TestReview_ReplayAndMismatch(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	ctx := context.Background()

	created, replayed, err := svc.CreateReview(ctx, reviewInput("run-1"))
	if err != nil || replayed {
		t.Fatalf("first create: replayed=%v err=%v", replayed, err)
	}
	if created.Family != store.FamilyReview || created.RunID == nil || *created.RunID != "run-1" {
		t.Fatalf("unexpected stored row: %+v", created)
	}

	again, replayed, err := svc.CreateReview(ctx, reviewInput("run-1"))
	if err != nil || !replayed {
		t.Fatalf("replay: replayed=%v err=%v", replayed, err)
	}
	if again.ID != created.ID {
		t.Fatalf("replay returned id %d, want %d", again.ID, created.ID)
	}

	changed := reviewInput("run-1")
	changed.Prompt = "different"
	if _, _, err := svc.CreateReview(ctx, changed); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("expected ErrReplayMismatch, got %v", err)
	}
	if _, _, err := svc.CreateReview(ctx, changed); !strings.Contains(fmt.Sprint(err), fmt.Sprint(created.ID)) {
		t.Fatalf("the mismatch error must name the stored submission: %v", err)
	}
}

func TestEvent_ReplayAndMismatch(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	ctx := context.Background()

	created, replayed, err := svc.CreateEvent(ctx, eventInput("k1", `{"b":2,"a":1}`))
	if err != nil || replayed {
		t.Fatalf("first create: replayed=%v err=%v", replayed, err)
	}
	// Stored verbatim: key order and number text survive the round trip.
	if string(created.Payload) != `{"b":2,"a":1}` {
		t.Fatalf("payload was rewritten: %s", created.Payload)
	}

	// Same content, different key order and spacing: an identical replay.
	again, replayed, err := svc.CreateEvent(ctx, eventInput("k1", `{"a":1, "b":2}`))
	if err != nil || !replayed {
		t.Fatalf("replay: replayed=%v err=%v", replayed, err)
	}
	if again.ID != created.ID {
		t.Fatalf("replay returned id %d, want %d", again.ID, created.ID)
	}

	if _, _, err := svc.CreateEvent(ctx, eventInput("k1", `{"a":1,"b":3}`)); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("expected ErrReplayMismatch, got %v", err)
	}
}

func TestFriction_DedupeWindow(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	first, duplicate, err := svc.CreateFriction(ctx, frictionInput("docs drifted"))
	if err != nil || duplicate {
		t.Fatalf("first create: duplicate=%v err=%v", duplicate, err)
	}

	// Inside the window, with different auto-collected context: absorbed.
	withContext := frictionInput("docs drifted")
	withContext.Context = map[string]string{"cwd": "/elsewhere", "occurred_at": "2026-01-01T00:00:00Z"}
	dup, duplicate, err := svc.CreateFriction(ctx, withContext)
	if err != nil || !duplicate {
		t.Fatalf("duplicate inside window: duplicate=%v err=%v", duplicate, err)
	}
	if dup.ID != first.ID {
		t.Fatalf("duplicate returned id %d, want %d", dup.ID, first.ID)
	}

	// Different content is a different friction, never absorbed.
	other, duplicate, err := svc.CreateFriction(ctx, frictionInput("something else"))
	if err != nil || duplicate {
		t.Fatalf("different content: duplicate=%v err=%v", duplicate, err)
	}
	if other.ID == first.ID {
		t.Fatal("different content must produce a new row")
	}

	// Outside the window a recurrence is a new row on purpose.
	original := frictionDedupeWindow
	frictionDedupeWindow = time.Nanosecond
	t.Cleanup(func() { frictionDedupeWindow = original })

	recurrence, duplicate, err := svc.CreateFriction(ctx, frictionInput("docs drifted"))
	if err != nil || duplicate {
		t.Fatalf("outside window: duplicate=%v err=%v", duplicate, err)
	}
	if recurrence.ID == first.ID {
		t.Fatal("a friction outside the dedupe window must create a new row")
	}
}

func TestConcurrentIdenticalWritesProduceOneRow(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	ctx := context.Background()

	const goroutines = 20

	run := func(t *testing.T, fn func() (store.Submission, bool, error)) []int64 {
		t.Helper()

		var (
			wg  sync.WaitGroup
			mu  sync.Mutex
			ids []int64
		)
		wg.Add(goroutines)
		for range goroutines {
			go func() {
				defer wg.Done()

				sub, _, err := fn()
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					t.Errorf("concurrent create: %v", err)

					return
				}
				ids = append(ids, sub.ID)
			}()
		}
		wg.Wait()

		return ids
	}

	frictionIDs := run(t, func() (store.Submission, bool, error) {
		return svc.CreateFriction(ctx, frictionInput("racing friction"))
	})
	assertAllEqual(t, "friction", frictionIDs, goroutines)

	reviewIDs := run(t, func() (store.Submission, bool, error) {
		return svc.CreateReview(ctx, reviewInput("racing-run"))
	})
	assertAllEqual(t, "review", reviewIDs, goroutines)

	eventIDs := run(t, func() (store.Submission, bool, error) {
		return svc.CreateEvent(ctx, eventInput("racing-key", `{"ok":true}`))
	})
	assertAllEqual(t, "event", eventIDs, goroutines)

	res, err := svc.ListSubmissions(ctx, ListSubmissionsInput{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Total != 3 {
		t.Fatalf("expected exactly 3 rows after the races, got %d", res.Total)
	}
}

func assertAllEqual(t *testing.T, family string, ids []int64, want int) {
	t.Helper()

	if len(ids) != want {
		t.Fatalf("%s: got %d results, want %d", family, len(ids), want)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("%s: concurrent identical writes produced different ids: %v", family, ids)
		}
	}
}

func TestSetProcessed_Transitions(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	ctx := context.Background()

	a, _, err := svc.CreateFriction(ctx, frictionInput("a"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, _, err := svc.CreateFriction(ctx, frictionInput("b"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	resolution := "fixed in example@1a2b3c4"
	res, err := svc.SetProcessed(ctx, SetProcessedInput{
		IDs: []int64{a.ID, a.ID, b.ID, 999999}, Processed: true, Resolution: &resolution,
	})
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if len(res.Updated) != 2 || len(res.NotFound) != 1 || len(res.Unchanged) != 0 {
		t.Fatalf("unexpected classification: %+v", res)
	}

	marked, err := svc.GetSubmission(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if marked.ProcessedAt == nil || marked.Resolution == nil || *marked.Resolution != resolution {
		t.Fatalf("mark did not store the resolution: %+v", marked)
	}
	firstStamp := *marked.ProcessedAt

	// Same resolution again: idempotent.
	res, err = svc.SetProcessed(ctx, SetProcessedInput{IDs: []int64{a.ID}, Processed: true, Resolution: &resolution})
	if err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if len(res.Unchanged) != 1 || len(res.Updated) != 0 {
		t.Fatalf("expected unchanged, got %+v", res)
	}

	// No resolution given: still idempotent, stored resolution untouched.
	res, err = svc.SetProcessed(ctx, SetProcessedInput{IDs: []int64{a.ID}, Processed: true})
	if err != nil {
		t.Fatalf("re-mark without resolution: %v", err)
	}
	if len(res.Unchanged) != 1 {
		t.Fatalf("expected unchanged, got %+v", res)
	}

	// A different resolution replaces it and keeps the original timestamp.
	replacement := "duplicate of 41"
	res, err = svc.SetProcessed(ctx, SetProcessedInput{IDs: []int64{a.ID}, Processed: true, Resolution: &replacement})
	if err != nil {
		t.Fatalf("replace resolution: %v", err)
	}
	if len(res.Updated) != 1 {
		t.Fatalf("expected updated, got %+v", res)
	}
	reMarked, err := svc.GetSubmission(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if reMarked.ProcessedAt == nil || *reMarked.ProcessedAt != firstStamp {
		t.Fatal("replacing the resolution must keep the original processed_at")
	}
	if reMarked.Resolution == nil || *reMarked.Resolution != replacement {
		t.Fatalf("resolution was not replaced: %+v", reMarked.Resolution)
	}

	// Unmarking clears both fields.
	res, err = svc.SetProcessed(ctx, SetProcessedInput{IDs: []int64{a.ID}, Processed: false})
	if err != nil {
		t.Fatalf("unmark: %v", err)
	}
	if len(res.Updated) != 1 {
		t.Fatalf("expected updated, got %+v", res)
	}
	cleared, err := svc.GetSubmission(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cleared.ProcessedAt != nil || cleared.Resolution != nil {
		t.Fatalf("unmark must clear processed_at and resolution: %+v", cleared)
	}

	// Unmarking again changes nothing.
	res, err = svc.SetProcessed(ctx, SetProcessedInput{IDs: []int64{a.ID}, Processed: false})
	if err != nil {
		t.Fatalf("unmark again: %v", err)
	}
	if len(res.Unchanged) != 1 {
		t.Fatalf("expected unchanged, got %+v", res)
	}
}

func TestSetProcessed_Rejects(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	ctx := context.Background()
	blank := "   "
	resolution := "done"

	cases := map[string]SetProcessedInput{
		"empty ids":                {IDs: nil, Processed: true},
		"non-positive id":          {IDs: []int64{0}, Processed: true},
		"too many ids":             {IDs: make([]int64, maxProcessedIDs+1), Processed: true},
		"blank resolution":         {IDs: []int64{1}, Processed: true, Resolution: &blank},
		"resolution when clearing": {IDs: []int64{1}, Processed: false, Resolution: &resolution},
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.SetProcessed(ctx, in); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestList_FiltersPagingAndTotals(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	ctx := context.Background()

	for i := range 5 {
		if _, _, err := svc.CreateFriction(ctx, frictionInput(fmt.Sprintf("friction %d", i))); err != nil {
			t.Fatalf("create friction: %v", err)
		}
	}
	if _, _, err := svc.CreateReview(ctx, reviewInput("run-1")); err != nil {
		t.Fatalf("create review: %v", err)
	}
	if _, _, err := svc.CreateEvent(ctx, eventInput("k1", `{"ok":true}`)); err != nil {
		t.Fatalf("create event: %v", err)
	}

	all, err := svc.ListSubmissions(ctx, ListSubmissionsInput{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if all.Total != 7 || len(all.Rows) != 7 || all.HasMore || all.NextBeforeID != nil {
		t.Fatalf("unexpected full list: total=%d rows=%d hasMore=%v", all.Total, len(all.Rows), all.HasMore)
	}
	// Newest first.
	if all.Rows[0].ID < all.Rows[len(all.Rows)-1].ID {
		t.Fatal("rows must be ordered by descending id")
	}

	byFamily, err := svc.ListSubmissions(ctx, ListSubmissionsInput{Family: store.FamilyFriction})
	if err != nil {
		t.Fatalf("list by family: %v", err)
	}
	if byFamily.Total != 5 {
		t.Fatalf("family filter: total=%d, want 5", byFamily.Total)
	}
	for _, row := range byFamily.Rows {
		if row.Category != "documentation" || row.Summary == "" {
			t.Fatalf("friction fields must be lifted into the summary: %+v", row)
		}
		if len(row.Payload) != 0 {
			t.Fatal("summaries must not carry the payload")
		}
	}

	// Review rows have no lifted friction fields.
	reviews, err := svc.ListSubmissions(ctx, ListSubmissionsInput{Family: store.FamilyReview})
	if err != nil {
		t.Fatalf("list reviews: %v", err)
	}
	if len(reviews.Rows) != 1 || reviews.Rows[0].Category != "" || reviews.Rows[0].Summary != "" {
		t.Fatalf("unexpected review summary: %+v", reviews.Rows)
	}

	// Keyset paging walks every row exactly once.
	seen := map[int64]bool{}
	var before *int64
	pages := 0
	for {
		page, err := svc.ListSubmissions(ctx, ListSubmissionsInput{Limit: 2, BeforeID: before})
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		if page.Total != 7 {
			t.Fatalf("total must ignore before_id: got %d", page.Total)
		}
		for _, row := range page.Rows {
			if seen[row.ID] {
				t.Fatalf("row %d returned twice", row.ID)
			}
			seen[row.ID] = true
		}
		pages++
		if !page.HasMore {
			if page.NextBeforeID != nil {
				t.Fatal("next_before_id must be null on the last page")
			}

			break
		}
		if page.NextBeforeID == nil {
			t.Fatal("has_more without next_before_id")
		}
		before = page.NextBeforeID
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != 7 {
		t.Fatalf("paging saw %d rows, want 7", len(seen))
	}

	// Offset paging (API 1.0) still works.
	offsetPage, err := svc.ListSubmissions(ctx, ListSubmissionsInput{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("offset page: %v", err)
	}
	if offsetPage.Offset != 2 || len(offsetPage.Rows) != 2 || offsetPage.Rows[0].ID != all.Rows[2].ID {
		t.Fatalf("unexpected offset page: %+v", offsetPage)
	}

	withPayload, err := svc.ListSubmissions(ctx, ListSubmissionsInput{IncludePayload: true, Limit: 1000})
	if err != nil {
		t.Fatalf("list include=payload: %v", err)
	}
	if withPayload.Limit != maxListLimitWithPayload {
		t.Fatalf("include=payload must cap the limit at %d, got %d", maxListLimitWithPayload, withPayload.Limit)
	}
	for _, row := range withPayload.Rows {
		if len(row.Payload) == 0 {
			t.Fatal("include=payload must return the payload")
		}
	}

	processed := false
	open, err := svc.ListSubmissions(ctx, ListSubmissionsInput{Processed: &processed})
	if err != nil {
		t.Fatalf("list unprocessed: %v", err)
	}
	if open.Total != 7 {
		t.Fatalf("unprocessed total=%d, want 7", open.Total)
	}
	if _, err := svc.SetProcessed(ctx, SetProcessedInput{IDs: []int64{all.Rows[0].ID}, Processed: true}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	open, err = svc.ListSubmissions(ctx, ListSubmissionsInput{Processed: &processed})
	if err != nil {
		t.Fatalf("list unprocessed: %v", err)
	}
	if open.Total != 6 {
		t.Fatalf("unprocessed total after marking=%d, want 6", open.Total)
	}

	if _, err := svc.ListSubmissions(ctx, ListSubmissionsInput{Family: "nope"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("an unknown family must be rejected")
	}
}

func TestExport_TerminatorAndDigest(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	ctx := context.Background()

	for i := range 3 {
		if _, _, err := svc.CreateFriction(ctx, frictionInput(fmt.Sprintf("friction %d", i))); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if _, _, err := svc.CreateReview(ctx, reviewInput("run-1")); err != nil {
		t.Fatalf("create review: %v", err)
	}

	var buf bytes.Buffer
	if err := svc.Export(ctx, ExportInput{}, &buf, nil); err != nil {
		t.Fatalf("export: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected header + 4 records + terminator, got %d lines", len(lines))
	}

	var header ExportHeader
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.ExportFormat != 1 || header.Family != nil || header.Since != nil {
		t.Fatalf("unexpected header: %+v", header)
	}

	var lastID int64
	for _, line := range lines[1 : len(lines)-1] {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode record: %v", err)
		}
		if rec.ID <= lastID {
			t.Fatal("export must be ordered by ascending id")
		}
		lastID = rec.ID
	}

	var term ExportTerminator
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &term); err != nil {
		t.Fatalf("decode terminator: %v", err)
	}
	if !term.ExportComplete || term.Count != 4 || term.SHA256 == "" {
		t.Fatalf("unexpected terminator: %+v", term)
	}

	// The digest covers the record lines only.
	records := strings.Join(lines[1:len(lines)-1], "\n") + "\n"
	if got := sha256Hex(records); got != term.SHA256 {
		t.Fatalf("digest mismatch: terminator %s, computed %s", term.SHA256, got)
	}

	// A filtered export marks itself as partial.
	buf.Reset()
	if err := svc.Export(ctx, ExportInput{Family: store.FamilyReview}, &buf, nil); err != nil {
		t.Fatalf("filtered export: %v", err)
	}
	if err := json.Unmarshal([]byte(strings.SplitN(buf.String(), "\n", 2)[0]), &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.Family == nil || *header.Family != store.FamilyReview {
		t.Fatalf("filtered export must name the filter: %+v", header)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}
