package api

import (
	"log/slog"
	"net/http"

	"github.com/agentfeedback/agentfeedback/internal/v3/core"
)

// handleSetProcessed handles POST /api/v1/submissions/processed — the batch
// mark/unmark used by the processor. The only write that changes an existing row.
func (s *Server) handleSetProcessed(w http.ResponseWriter, r *http.Request) {
	var req SetProcessedRequest
	if !decodeRequestBody(w, r, &req) {
		return
	}

	processed := true
	if req.Processed != nil {
		processed = *req.Processed
	}

	res, err := s.svc.SetProcessed(r.Context(), core.SetProcessedInput{
		IDs:        req.IDs,
		Processed:  processed,
		Resolution: req.Resolution,
	})
	if err != nil {
		status := mapSubmissionError(err)
		if status >= http.StatusInternalServerError {
			slog.ErrorContext(r.Context(), "failed to set processed state", "error", err)
		} else {
			slog.WarnContext(r.Context(), "rejected processed state request", "error", err)
		}
		writeError(w, status, "set_processed_failed", errorMessage(status, err))

		return
	}

	writeJSON(w, http.StatusOK, SetProcessedResponse{
		Processed:  processed,
		Resolution: res.Resolution,
		Updated:    res.Updated,
		Unchanged:  res.Unchanged,
		NotFound:   res.NotFound,
	})
}
