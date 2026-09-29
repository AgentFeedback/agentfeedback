package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/agentfeedback/agentfeedback/internal/canonjson"
	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// Generous upper bounds. They keep indexed columns small and stop garbage rows
// — they do not police content. details, suggested_fix, prompt, output and
// event payloads are bounded only by the 10 MiB request cap; submitting
// feedback must stay easy.
const (
	maxIdentifierLen = 200  // machine_name, coordinator_model, run_id, skill, kind, key, category, project, harness, slot, model, status
	maxSummaryLen    = 2000 // friction summary
	maxNoteLen       = 4000 // reviewer note
	maxReviewers     = 100

	maxContextEntries  = 32
	maxContextKeyLen   = 64
	maxContextValueLen = 2000
)

// frictionDedupeWindow is how far back CreateFriction looks for an
// identical-content friction before absorbing the new one as a duplicate.
// Package variable so tests can shrink it.
var frictionDedupeWindow = 24 * time.Hour

func fieldTooLong(field, v string, maxLen int) error {
	if len(v) > maxLen {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidInput, field, maxLen)
	}

	return nil
}

// validateContext caps the auto-collected client metadata map. Keys are
// free-form (clients may add new ones without a server change); only sizes
// are enforced.
func validateContext(ctx map[string]string) error {
	if len(ctx) > maxContextEntries {
		return fmt.Errorf("%w: context has more than %d entries", ErrInvalidInput, maxContextEntries)
	}
	for k, v := range ctx {
		if k == "" {
			return fmt.Errorf("%w: context keys must be non-empty", ErrInvalidInput)
		}
		if len(k) > maxContextKeyLen {
			return fmt.Errorf("%w: context key %q exceeds %d bytes", ErrInvalidInput, k[:maxContextKeyLen], maxContextKeyLen)
		}
		if len(v) > maxContextValueLen {
			return fmt.Errorf("%w: context value for %q exceeds %d bytes", ErrInvalidInput, k, maxContextValueLen)
		}
	}

	return nil
}

// CreateReview validates and stores a review submission (review-panel,
// plan-review, ...).
//
// Idempotent on (skill, run_id): replaying identical content returns the
// existing row with replayed=true; replaying different content under the same
// key returns ErrReplayMismatch (409) instead of silently discarding the new
// payload.
func (s *Service) CreateReview(ctx context.Context, in CreateReviewInput) (store.Submission, bool, error) {
	skill := strings.TrimSpace(in.Skill)
	machineName := strings.TrimSpace(in.MachineName)
	coordinatorModel := strings.TrimSpace(in.CoordinatorModel)
	runID := strings.TrimSpace(in.RunID)

	if skill == "" {
		return store.Submission{}, false, fmt.Errorf("%w: skill is required", ErrInvalidInput)
	}
	if skill == store.FamilyFriction {
		return store.Submission{}, false, fmt.Errorf("%w: skill \"friction\" is reserved for friction submissions", ErrInvalidInput)
	}
	if machineName == "" {
		return store.Submission{}, false, fmt.Errorf("%w: machine_name is required", ErrInvalidInput)
	}
	if coordinatorModel == "" {
		return store.Submission{}, false, fmt.Errorf("%w: coordinator_model is required", ErrInvalidInput)
	}
	if runID == "" {
		return store.Submission{}, false, fmt.Errorf("%w: run_id is required", ErrInvalidInput)
	}
	for field, v := range map[string]string{
		"skill": skill, "machine_name": machineName, "coordinator_model": coordinatorModel, "run_id": runID,
	} {
		if err := fieldTooLong(field, v, maxIdentifierLen); err != nil {
			return store.Submission{}, false, err
		}
	}
	if len(in.Reviewers) == 0 {
		return store.Submission{}, false, fmt.Errorf("%w: reviewers must be non-empty", ErrInvalidInput)
	}
	if len(in.Reviewers) > maxReviewers {
		return store.Submission{}, false, fmt.Errorf("%w: at most %d reviewers per submission", ErrInvalidInput, maxReviewers)
	}
	for i, rv := range in.Reviewers {
		if strings.TrimSpace(rv.Slot) == "" {
			return store.Submission{}, false, fmt.Errorf("%w: reviewers[%d].slot is required", ErrInvalidInput, i)
		}
		if strings.TrimSpace(rv.Model) == "" {
			return store.Submission{}, false, fmt.Errorf("%w: reviewers[%d].model is required", ErrInvalidInput, i)
		}
		if strings.TrimSpace(rv.Status) == "" {
			return store.Submission{}, false, fmt.Errorf("%w: reviewers[%d].status is required", ErrInvalidInput, i)
		}
		for field, v := range map[string]string{"slot": rv.Slot, "model": rv.Model, "status": rv.Status} {
			if err := fieldTooLong(fmt.Sprintf("reviewers[%d].%s", i, field), v, maxIdentifierLen); err != nil {
				return store.Submission{}, false, err
			}
		}
		if err := fieldTooLong(fmt.Sprintf("reviewers[%d].note", i), rv.Note, maxNoteLen); err != nil {
			return store.Submission{}, false, err
		}
		if rv.DurationS != nil && *rv.DurationS < 0 {
			return store.Submission{}, false, fmt.Errorf("%w: reviewers[%d].duration_s must be >= 0", ErrInvalidInput, i)
		}
		if rv.Bytes != nil && *rv.Bytes < 0 {
			return store.Submission{}, false, fmt.Errorf("%w: reviewers[%d].bytes must be >= 0", ErrInvalidInput, i)
		}
		if rv.Score != nil && (*rv.Score < 1 || *rv.Score > 5) {
			return store.Submission{}, false, fmt.Errorf("%w: reviewers[%d].score must be between 1 and 5", ErrInvalidInput, i)
		}
		if rv.Valid != nil && *rv.Valid < 0 {
			return store.Submission{}, false, fmt.Errorf("%w: reviewers[%d].valid must be >= 0", ErrInvalidInput, i)
		}
		if rv.Invalid != nil && *rv.Invalid < 0 {
			return store.Submission{}, false, fmt.Errorf("%w: reviewers[%d].invalid must be >= 0", ErrInvalidInput, i)
		}
	}

	payloadShape := reviewPayload{Prompt: in.Prompt, Reviewers: in.Reviewers}
	hash, err := canonicalHash(reviewCanonical{
		MachineName:      machineName,
		CoordinatorModel: coordinatorModel,
		Payload:          payloadShape,
	})
	if err != nil {
		return store.Submission{}, false, fmt.Errorf("hash review payload: %w", err)
	}
	payload, err := json.Marshal(payloadShape)
	if err != nil {
		return store.Submission{}, false, fmt.Errorf("marshal review payload: %w", err)
	}

	sub, replayed, err := s.createKeyed(ctx, store.Submission{
		Family:           store.FamilyReview,
		SubmissionType:   skill,
		MachineName:      machineName,
		CoordinatorModel: coordinatorModel,
		RunID:            &runID,
		Payload:          payload,
		PayloadHash:      hash,
	})
	if err != nil {
		return store.Submission{}, false, err
	}
	if !replayed {
		slog.InfoContext(ctx, "review submission created",
			"submission_id", sub.ID, "skill", skill, "machine_name", machineName, "run_id", runID)
	}

	return sub, replayed, nil
}

// CreateEvent validates and stores a write-once event. Idempotent on
// (kind, key) with the same replay/mismatch rules as reviews.
func (s *Service) CreateEvent(ctx context.Context, in CreateEventInput) (store.Submission, bool, error) {
	kind := strings.TrimSpace(in.Kind)
	key := strings.TrimSpace(in.Key)
	machineName := strings.TrimSpace(in.MachineName)
	coordinatorModel := strings.TrimSpace(in.CoordinatorModel)

	if kind == "" {
		return store.Submission{}, false, fmt.Errorf("%w: kind is required", ErrInvalidInput)
	}
	if kind == store.FamilyFriction {
		return store.Submission{}, false, fmt.Errorf("%w: kind \"friction\" is reserved for friction submissions", ErrInvalidInput)
	}
	if key == "" {
		return store.Submission{}, false, fmt.Errorf("%w: key is required", ErrInvalidInput)
	}
	if machineName == "" {
		return store.Submission{}, false, fmt.Errorf("%w: machine_name is required", ErrInvalidInput)
	}
	if coordinatorModel == "" {
		return store.Submission{}, false, fmt.Errorf("%w: coordinator_model is required", ErrInvalidInput)
	}
	for field, v := range map[string]string{
		"kind": kind, "key": key, "machine_name": machineName, "coordinator_model": coordinatorModel,
	} {
		if err := fieldTooLong(field, v, maxIdentifierLen); err != nil {
			return store.Submission{}, false, err
		}
	}
	payload, err := compactObject(in.Payload)
	if err != nil {
		return store.Submission{}, false, err
	}

	hash, err := eventHash(machineName, coordinatorModel, payload)
	if err != nil {
		return store.Submission{}, false, fmt.Errorf("hash event payload: %w", err)
	}

	sub, replayed, err := s.createKeyed(ctx, store.Submission{
		Family:           store.FamilyEvent,
		SubmissionType:   kind,
		MachineName:      machineName,
		CoordinatorModel: coordinatorModel,
		RunID:            &key,
		Payload:          payload,
		PayloadHash:      hash,
	})
	if err != nil {
		return store.Submission{}, false, err
	}
	if !replayed {
		slog.InfoContext(ctx, "event submission created",
			"submission_id", sub.ID, "kind", kind, "machine_name", machineName, "key", key)
	}

	return sub, replayed, nil
}

// compactObject validates that raw is a JSON object with no duplicate keys and
// returns it compacted — key order and number text untouched.
func compactObject(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: payload is required and must be a JSON object", ErrInvalidInput)
	}
	if bytes.TrimSpace(raw)[0] != '{' {
		return nil, fmt.Errorf("%w: payload must be a JSON object", ErrInvalidInput)
	}
	// canonjson parses strictly: it rejects malformed JSON, trailing values and
	// duplicate keys, which would otherwise make the content hash depend on a
	// decoder detail.
	if _, err := canonjson.Marshal(raw); err != nil {
		if errors.Is(err, canonjson.ErrDuplicateKey) {
			return nil, fmt.Errorf("%w: payload contains a duplicate key", ErrInvalidInput)
		}

		return nil, fmt.Errorf("%w: payload must be valid JSON", ErrInvalidInput)
	}

	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("%w: payload must be valid JSON", ErrInvalidInput)
	}

	return buf.Bytes(), nil
}

// idempotencyKeyName is what the caller's request calls the key of a keyed
// family: reviews send run_id, events send key. The error message has to name
// the field the caller actually sent.
func idempotencyKeyName(family string) string {
	if family == store.FamilyEvent {
		return "key"
	}

	return "run_id"
}

func replayMismatchError(family string, existingID int64) error {
	name := idempotencyKeyName(family)

	return fmt.Errorf(
		"%w: %s already stored as submission %d with different content; submit the correction as a new submission (new %s)",
		ErrReplayMismatch, name, existingID, name)
}

// createKeyed inserts a submission that carries an idempotency key, inside one
// BEGIN IMMEDIATE transaction: the existence check and the insert cannot
// interleave with a competing writer, so exactly one row results.
func (s *Service) createKeyed(ctx context.Context, sub store.Submission) (store.Submission, bool, error) {
	var (
		out      store.Submission
		replayed bool
	)

	err := s.db.Write(ctx, func(q store.Querier) error {
		existing, err := store.GetSubmissionByKey(ctx, q, sub.Family, sub.SubmissionType, *sub.RunID)
		switch {
		case err == nil:
			if existing.PayloadHash != sub.PayloadHash {
				return replayMismatchError(sub.Family, existing.ID)
			}
			out, replayed = existing, true

			return nil
		case !errors.Is(err, store.ErrNotFound):
			return err
		}

		sub.CreatedAt = nowMicros()
		created, err := store.InsertSubmission(ctx, q, sub)
		if err != nil {
			// Defensive: the IMMEDIATE transaction already serializes
			// writers, but a unique violation must still be reported as a
			// replay or mismatch rather than a 500.
			if store.IsUniqueViolation(err) {
				existing, getErr := store.GetSubmissionByKey(ctx, q, sub.Family, sub.SubmissionType, *sub.RunID)
				if getErr == nil {
					if existing.PayloadHash != sub.PayloadHash {
						return replayMismatchError(sub.Family, existing.ID)
					}
					out, replayed = existing, true

					return nil
				}
			}

			return err
		}
		out = created

		return nil
	})
	if err != nil {
		return store.Submission{}, false, err
	}

	return out, replayed, nil
}

// CreateFriction validates and stores a friction submission. Frictions have no
// run_id; instead, an identical-content friction submitted within
// frictionDedupeWindow is absorbed as a duplicate — the existing row is
// returned with duplicate=true and no new row is inserted. Blind retries are
// therefore safe within the window, with zero client cooperation required.
func (s *Service) CreateFriction(ctx context.Context, in CreateFrictionInput) (store.Submission, bool, error) {
	machineName := strings.TrimSpace(in.MachineName)
	coordinatorModel := strings.TrimSpace(in.CoordinatorModel)
	category := strings.TrimSpace(in.Category)
	summary := strings.TrimSpace(in.Summary)

	if machineName == "" {
		return store.Submission{}, false, fmt.Errorf("%w: machine_name is required", ErrInvalidInput)
	}
	if coordinatorModel == "" {
		return store.Submission{}, false, fmt.Errorf("%w: coordinator_model is required", ErrInvalidInput)
	}
	if category == "" {
		return store.Submission{}, false, fmt.Errorf("%w: category is required", ErrInvalidInput)
	}
	if summary == "" {
		return store.Submission{}, false, fmt.Errorf("%w: summary is required", ErrInvalidInput)
	}
	for field, v := range map[string]string{
		"machine_name": machineName, "coordinator_model": coordinatorModel,
		"category": category, "project": in.Project, "harness": in.Harness,
	} {
		if err := fieldTooLong(field, v, maxIdentifierLen); err != nil {
			return store.Submission{}, false, err
		}
	}
	if err := fieldTooLong("summary", summary, maxSummaryLen); err != nil {
		return store.Submission{}, false, err
	}
	if err := validateContext(in.Context); err != nil {
		return store.Submission{}, false, err
	}

	payloadShape := frictionPayload{
		Category:     category,
		Summary:      summary,
		Details:      in.Details,
		SuggestedFix: in.SuggestedFix,
		Project:      in.Project,
		Harness:      in.Harness,
		Context:      in.Context,
	}
	// The dedupe hash covers the semantic content only. Context (occurred_at,
	// commit, cwd, ...) varies between attempts of the SAME friction and must
	// not defeat duplicate absorption.
	content := payloadShape
	content.Context = nil
	hash, err := canonicalHash(frictionCanonical{
		MachineName:      machineName,
		CoordinatorModel: coordinatorModel,
		Payload:          content,
	})
	if err != nil {
		return store.Submission{}, false, fmt.Errorf("hash friction payload: %w", err)
	}
	payload, err := json.Marshal(payloadShape)
	if err != nil {
		return store.Submission{}, false, fmt.Errorf("marshal friction payload: %w", err)
	}

	var (
		out       store.Submission
		duplicate bool
	)
	err = s.db.Write(ctx, func(q store.Querier) error {
		now := nowMicros()
		existing, err := store.GetRecentFrictionByHash(ctx, q, hash, now-frictionDedupeWindow.Microseconds())
		switch {
		case err == nil:
			out, duplicate = existing, true

			return nil
		case !errors.Is(err, store.ErrNotFound):
			return err
		}

		created, err := store.InsertSubmission(ctx, q, store.Submission{
			Family:           store.FamilyFriction,
			SubmissionType:   store.FamilyFriction,
			MachineName:      machineName,
			CoordinatorModel: coordinatorModel,
			Payload:          payload,
			PayloadHash:      hash,
			CreatedAt:        now,
		})
		if err != nil {
			return err
		}
		out = created

		return nil
	})
	if err != nil {
		return store.Submission{}, false, err
	}

	if !duplicate {
		slog.InfoContext(ctx, "friction submission created",
			"submission_id", out.ID, "machine_name", machineName, "category", category)
	}

	return out, duplicate, nil
}
