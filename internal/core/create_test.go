package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

func intPtr(v int) *int { return &v }

// newTestService opens a fresh file-backed database in the test's temp
// directory. File-backed, not in-memory: the concurrency tests need the same
// locking behaviour production has.
func newTestService(t *testing.T) *Service {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "agentfeedback.db"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return New(db)
}

func TestCreateReview_Validation(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	validReviewer := ReviewerInput{Slot: "gpt56", Model: "openai-codex/gpt-5.6-sol", Status: "completed"}
	base := func() CreateReviewInput {
		return CreateReviewInput{
			Skill: "review-panel", MachineName: "workstation-a", CoordinatorModel: "claude-fable-5",
			RunID: "run-1", Reviewers: []ReviewerInput{validReviewer},
		}
	}

	tests := []struct {
		name   string
		mutate func(*CreateReviewInput)
	}{
		{"missing skill", func(in *CreateReviewInput) { in.Skill = "" }},
		{"reserved skill friction", func(in *CreateReviewInput) { in.Skill = "friction" }},
		{"missing machine_name", func(in *CreateReviewInput) { in.MachineName = "" }},
		{"missing coordinator_model", func(in *CreateReviewInput) { in.CoordinatorModel = "" }},
		{"missing run_id", func(in *CreateReviewInput) { in.RunID = "" }},
		{"empty reviewers", func(in *CreateReviewInput) { in.Reviewers = nil }},
		{"too many reviewers", func(in *CreateReviewInput) {
			in.Reviewers = make([]ReviewerInput, maxReviewers+1)
			for i := range in.Reviewers {
				in.Reviewers[i] = validReviewer
			}
		}},
		{"reviewer missing slot", func(in *CreateReviewInput) { in.Reviewers[0].Slot = "" }},
		{"reviewer missing model", func(in *CreateReviewInput) { in.Reviewers[0].Model = "" }},
		{"reviewer missing status", func(in *CreateReviewInput) { in.Reviewers[0].Status = "" }},
		{"negative duration", func(in *CreateReviewInput) { in.Reviewers[0].DurationS = intPtr(-1) }},
		{"negative bytes", func(in *CreateReviewInput) { in.Reviewers[0].Bytes = intPtr(-1) }},
		{"score below range", func(in *CreateReviewInput) { in.Reviewers[0].Score = intPtr(0) }},
		{"score above range", func(in *CreateReviewInput) { in.Reviewers[0].Score = intPtr(6) }},
		{"negative valid", func(in *CreateReviewInput) { in.Reviewers[0].Valid = intPtr(-1) }},
		{"negative invalid", func(in *CreateReviewInput) { in.Reviewers[0].Invalid = intPtr(-1) }},
		{"skill too long", func(in *CreateReviewInput) { in.Skill = strings.Repeat("x", maxIdentifierLen+1) }},
		{"run_id too long", func(in *CreateReviewInput) { in.RunID = strings.Repeat("x", maxIdentifierLen+1) }},
		{"machine_name too long", func(in *CreateReviewInput) { in.MachineName = strings.Repeat("x", maxIdentifierLen+1) }},
		{"reviewer note too long", func(in *CreateReviewInput) { in.Reviewers[0].Note = strings.Repeat("x", maxNoteLen+1) }},
		{"reviewer slot too long", func(in *CreateReviewInput) { in.Reviewers[0].Slot = strings.Repeat("x", maxIdentifierLen+1) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mutate(&in)

			if _, _, err := svc.CreateReview(context.Background(), in); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestCreateFriction_Validation(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	base := func() CreateFrictionInput {
		return CreateFrictionInput{
			MachineName: "workstation-a", CoordinatorModel: "claude-fable-5",
			Category: "documentation", Summary: "docs drifted",
		}
	}

	tests := []struct {
		name   string
		mutate func(*CreateFrictionInput)
	}{
		{"missing machine_name", func(in *CreateFrictionInput) { in.MachineName = "" }},
		{"missing coordinator_model", func(in *CreateFrictionInput) { in.CoordinatorModel = "" }},
		{"missing category", func(in *CreateFrictionInput) { in.Category = "" }},
		{"missing summary", func(in *CreateFrictionInput) { in.Summary = "" }},
		{"whitespace summary", func(in *CreateFrictionInput) { in.Summary = "   " }},
		{"category too long", func(in *CreateFrictionInput) { in.Category = strings.Repeat("x", maxIdentifierLen+1) }},
		{"summary too long", func(in *CreateFrictionInput) { in.Summary = strings.Repeat("x", maxSummaryLen+1) }},
		{"project too long", func(in *CreateFrictionInput) { in.Project = strings.Repeat("x", maxIdentifierLen+1) }},
		{"harness too long", func(in *CreateFrictionInput) { in.Harness = strings.Repeat("x", maxIdentifierLen+1) }},
		{"too many context entries", func(in *CreateFrictionInput) {
			in.Context = map[string]string{}
			for i := 0; i <= maxContextEntries; i++ {
				in.Context[fmt.Sprintf("k%d", i)] = "v"
			}
		}},
		{"context key too long", func(in *CreateFrictionInput) {
			in.Context = map[string]string{strings.Repeat("k", maxContextKeyLen+1): "v"}
		}},
		{"context value too long", func(in *CreateFrictionInput) {
			in.Context = map[string]string{"cwd": strings.Repeat("v", maxContextValueLen+1)}
		}},
		{"empty context key", func(in *CreateFrictionInput) { in.Context = map[string]string{"": "v"} }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mutate(&in)

			if _, _, err := svc.CreateFriction(context.Background(), in); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestCreateEvent_Validation(t *testing.T) {
	t.Parallel()

	svc := newTestService(t)
	base := func() CreateEventInput {
		return CreateEventInput{
			Kind: "deploy", Key: "k1", MachineName: "workstation-a",
			CoordinatorModel: "claude-fable-5", Payload: []byte(`{"ok":true}`),
		}
	}

	tests := []struct {
		name   string
		mutate func(*CreateEventInput)
	}{
		{"missing kind", func(in *CreateEventInput) { in.Kind = "" }},
		{"reserved kind friction", func(in *CreateEventInput) { in.Kind = "friction" }},
		{"missing key", func(in *CreateEventInput) { in.Key = "" }},
		{"missing machine_name", func(in *CreateEventInput) { in.MachineName = "" }},
		{"missing coordinator_model", func(in *CreateEventInput) { in.CoordinatorModel = "" }},
		{"kind too long", func(in *CreateEventInput) { in.Kind = strings.Repeat("x", maxIdentifierLen+1) }},
		{"key too long", func(in *CreateEventInput) { in.Key = strings.Repeat("x", maxIdentifierLen+1) }},
		{"missing payload", func(in *CreateEventInput) { in.Payload = nil }},
		{"payload not an object", func(in *CreateEventInput) { in.Payload = []byte(`[1,2]`) }},
		{"payload not valid json", func(in *CreateEventInput) { in.Payload = []byte(`{"a":`) }},
		{"payload duplicate key", func(in *CreateEventInput) { in.Payload = []byte(`{"a":1,"a":2}`) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mutate(&in)

			if _, _, err := svc.CreateEvent(context.Background(), in); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

// TestFrictionHash_MatchesAPI10 pins the friction content hash to the digest
// the 1.0 (PostgreSQL) service produced for the same input. The constant was
// obtained by running the 1.0 core package's canonicalHash(frictionCanonical{…})
// over exactly this input before that code was removed. If it ever changes,
// every friction imported from 1.0 stops deduping against new submissions.
func TestFrictionHash_MatchesAPI10(t *testing.T) {
	t.Parallel()

	const want = "982269b79750f63830564ee9881c4c7e8f08eb90591ceb7c8386da8c8924175b"

	got, err := canonicalHash(frictionCanonical{
		MachineName:      "workstation-a",
		CoordinatorModel: "claude-fable-5-1",
		Payload: frictionPayload{
			Category:     "documentation",
			Summary:      "README install step references a flag that no longer exists",
			Details:      "expected --compat, got --legacy",
			SuggestedFix: "replace --legacy with --compat in README step 3",
			Project:      "example",
			Harness:      "claude-code",
		},
	})
	if err != nil {
		t.Fatalf("canonicalHash: %v", err)
	}
	if got != want {
		t.Fatalf("friction hash drifted from API 1.0: want %s, got %s", want, got)
	}

	// The same digest must come out of the payload-only path the importer uses.
	payload := `{"category":"documentation","summary":"README install step references a flag that no longer exists",` +
		`"details":"expected --compat, got --legacy","suggested_fix":"replace --legacy with --compat in README step 3",` +
		`"project":"example","harness":"claude-code","context":{"cwd":"/tmp"}}`
	recomputed, err := HashForFamily(store.FamilyFriction, "workstation-a", "claude-fable-5-1", []byte(payload))
	if err != nil {
		t.Fatalf("HashForFamily: %v", err)
	}
	if recomputed != want {
		t.Fatalf("recomputed friction hash: want %s, got %s", want, recomputed)
	}
}

// TestReviewHash_MatchesAPI10 pins the review content hash the same way.
func TestReviewHash_MatchesAPI10(t *testing.T) {
	t.Parallel()

	const want = "9afb24f3cd433330ec462ff02a6338d40c2c58f18271a8a851e741bd0371c1e9"

	got, err := canonicalHash(reviewCanonical{
		MachineName:      "workstation-a",
		CoordinatorModel: "claude-fable-5-1",
		Payload: reviewPayload{
			Prompt:    "review this",
			Reviewers: []ReviewerInput{{Slot: "gpt56", Model: "openai-codex/gpt-5.6-sol", Status: "completed"}},
		},
	})
	if err != nil {
		t.Fatalf("canonicalHash: %v", err)
	}
	if got != want {
		t.Fatalf("review hash drifted from API 1.0: want %s, got %s", want, got)
	}
}

func TestEventHash_IgnoresKeyOrderNotNumberForm(t *testing.T) {
	t.Parallel()

	a, err := eventHash("m", "c", []byte(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatalf("eventHash: %v", err)
	}
	b, err := eventHash("m", "c", []byte(`{"a":1,  "b":2}`))
	if err != nil {
		t.Fatalf("eventHash: %v", err)
	}
	if a != b {
		t.Fatal("key order and whitespace must not change the event hash")
	}

	c, err := eventHash("m", "c", []byte(`{"a":1.0,"b":2}`))
	if err != nil {
		t.Fatalf("eventHash: %v", err)
	}
	if a == c {
		t.Fatal("1 and 1.0 are different content and must hash differently")
	}
}
