package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/agentfeedback/agentfeedback/internal/core"
	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

// handleCreateFriction handles POST /api/v1/frictions.
func (s *Server) handleCreateFriction(w http.ResponseWriter, r *http.Request) {
	var req CreateFrictionRequest
	if !decodeRequestBody(w, r, &req) {
		return
	}

	sub, duplicate, err := s.svc.CreateFriction(r.Context(), core.CreateFrictionInput{
		MachineName:      req.MachineName,
		CoordinatorModel: req.CoordinatorModel,
		Category:         req.Category,
		Summary:          req.Summary,
		Details:          req.Details,
		SuggestedFix:     req.SuggestedFix,
		Project:          req.Project,
		Harness:          req.Harness,
		Context:          req.Context,
	})
	if err != nil {
		status := mapSubmissionError(err)
		if status >= http.StatusInternalServerError {
			slog.ErrorContext(r.Context(), "failed to create friction submission", "error", err, "category", req.Category)
		} else {
			slog.WarnContext(r.Context(), "rejected friction submission", "error", err, "category", req.Category)
		}
		s.metrics.observeSubmission(store.FamilyFriction, outcomeRejected)
		writeError(w, status, "create_friction_failed", errorMessage(status, err))

		return
	}

	// A duplicate absorbed inside the dedupe window returns the existing row
	// with 200 — the submitter's retry succeeded either way.
	status := http.StatusCreated
	outcome := outcomeCreated
	if duplicate {
		status, outcome = http.StatusOK, outcomeDuplicate
	}
	s.metrics.observeSubmission(store.FamilyFriction, outcome)
	writeJSON(w, status, core.ToRecord(sub))
}

// handleCreateReview handles POST /api/v1/reviews.
func (s *Server) handleCreateReview(w http.ResponseWriter, r *http.Request) {
	var req CreateReviewRequest
	if !decodeRequestBody(w, r, &req) {
		return
	}

	reviewers := make([]core.ReviewerInput, len(req.Reviewers))
	for i, rv := range req.Reviewers {
		reviewers[i] = core.ReviewerInput{
			Slot:      rv.Slot,
			Model:     rv.Model,
			Status:    rv.Status,
			DurationS: rv.DurationS,
			Bytes:     rv.Bytes,
			Output:    rv.Output,
			Score:     rv.Score,
			Valid:     rv.Valid,
			Invalid:   rv.Invalid,
			Note:      rv.Note,
		}
	}

	sub, replayed, err := s.svc.CreateReview(r.Context(), core.CreateReviewInput{
		Skill:            req.Skill,
		MachineName:      req.MachineName,
		CoordinatorModel: req.CoordinatorModel,
		RunID:            req.RunID,
		Prompt:           req.Prompt,
		Reviewers:        reviewers,
	})
	if err != nil {
		s.writeCreateError(w, r, store.FamilyReview, "create_review_failed", err,
			slog.String("skill", req.Skill), slog.String("run_id", req.RunID))

		return
	}

	status := http.StatusCreated
	outcome := outcomeCreated
	if replayed {
		status, outcome = http.StatusOK, outcomeReplayed
	}
	s.metrics.observeSubmission(store.FamilyReview, outcome)
	writeJSON(w, status, core.ToRecord(sub))
}

// handleCreateEvent handles POST /api/v1/events.
func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	var req CreateEventRequest
	if !decodeRequestBody(w, r, &req) {
		return
	}

	sub, replayed, err := s.svc.CreateEvent(r.Context(), core.CreateEventInput{
		Kind:             req.Kind,
		Key:              req.Key,
		MachineName:      req.MachineName,
		CoordinatorModel: req.CoordinatorModel,
		Payload:          req.Payload,
	})
	if err != nil {
		s.writeCreateError(w, r, store.FamilyEvent, "create_event_failed", err,
			slog.String("kind", req.Kind), slog.String("key", req.Key))

		return
	}

	status := http.StatusCreated
	outcome := outcomeCreated
	if replayed {
		status, outcome = http.StatusOK, outcomeReplayed
	}
	s.metrics.observeSubmission(store.FamilyEvent, outcome)
	writeJSON(w, status, core.ToRecord(sub))
}

// writeCreateError is shared by the two keyed families: a mismatch has its own
// error code and 409 status, everything else follows the sentinel mapping.
func (s *Server) writeCreateError(w http.ResponseWriter, r *http.Request, family, errType string, err error, attrs ...slog.Attr) {
	status := mapSubmissionError(err)
	outcome := outcomeRejected
	if errors.Is(err, core.ErrReplayMismatch) {
		errType, outcome = "replay_mismatch", outcomeMismatch
	}

	args := make([]any, 0, len(attrs)+2)
	args = append(args, slog.Any("error", err))
	for _, a := range attrs {
		args = append(args, a)
	}
	if status >= http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "failed to create "+family+" submission", args...)
	} else {
		slog.WarnContext(r.Context(), "rejected "+family+" submission", args...)
	}

	s.metrics.observeSubmission(family, outcome)
	writeError(w, status, errType, errorMessage(status, err))
}
