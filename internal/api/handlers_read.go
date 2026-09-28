package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/agentfeedback/agentfeedback/internal/v3/core"
)

// handleListSubmissions handles GET /api/v1/submissions.
func (s *Server) handleListSubmissions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	in := core.ListSubmissionsInput{
		Family:  q.Get("family"),
		Type:    q.Get("type"),
		Machine: q.Get("machine"),
		Model:   q.Get("model"),
	}

	switch in.Family {
	case "", "friction", "review", "event":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "family must be friction, review or event")

		return
	}

	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "since must be RFC 3339")

			return
		}
		in.Since = &t
	}

	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "until must be RFC 3339")

			return
		}
		in.Until = &t
	}

	if v := q.Get("processed"); v != "" {
		switch v {
		case "true":
			t := true
			in.Processed = &t
		case "false":
			f := false
			in.Processed = &f
		default:
			writeError(w, http.StatusBadRequest, "bad_request", "processed must be true or false")

			return
		}
	}

	if v := q.Get("include"); v != "" {
		if v != "payload" {
			writeError(w, http.StatusBadRequest, "bad_request", "include must be payload")

			return
		}
		in.IncludePayload = true
	}

	if v := q.Get("limit"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be an integer")

			return
		}
		in.Limit = limit
	}

	hasOffset := q.Get("offset") != ""
	if hasOffset {
		offset, err := strconv.Atoi(q.Get("offset"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "offset must be an integer")

			return
		}
		in.Offset = offset
	}

	if v := q.Get("before_id"); v != "" {
		if hasOffset {
			writeError(w, http.StatusBadRequest, "bad_request", "use before_id or offset, not both")

			return
		}
		beforeID, err := strconv.ParseInt(v, 10, 64)
		if err != nil || beforeID <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "before_id must be a positive integer")

			return
		}
		in.BeforeID = &beforeID
	}

	res, err := s.svc.ListSubmissions(r.Context(), in)
	if err != nil {
		status := mapSubmissionError(err)
		if status >= http.StatusInternalServerError {
			slog.ErrorContext(r.Context(), "failed to list submissions", "error", err)
		}
		writeError(w, status, "list_submissions_failed", errorMessage(status, err))

		return
	}

	resp := ListSubmissionsResponse{
		Submissions:  make([]core.SummaryRecord, len(res.Rows)),
		Limit:        res.Limit,
		Offset:       res.Offset,
		Total:        res.Total,
		HasMore:      res.HasMore,
		NextBeforeID: res.NextBeforeID,
	}
	for i, row := range res.Rows {
		resp.Submissions[i] = core.ToSummaryRecord(row)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleGetSubmission handles GET /api/v1/submissions/{id}.
func (s *Server) handleGetSubmission(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "id must be a numeric submission ID")

		return
	}

	sub, err := s.svc.GetSubmission(r.Context(), id)
	if err != nil {
		status := mapSubmissionError(err)
		if status >= http.StatusInternalServerError {
			slog.ErrorContext(r.Context(), "failed to get submission", "error", err, "submission_id", id)
		}
		writeError(w, status, "get_submission_failed", errorMessage(status, err))

		return
	}

	writeJSON(w, http.StatusOK, core.ToRecord(sub))
}

// handleExport handles GET /api/v1/export: the whole database (or a filtered
// slice of it) as newline-delimited JSON, streamed from one read transaction.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	in := core.ExportInput{Family: q.Get("family")}
	switch in.Family {
	case "", "friction", "review", "event":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "family must be friction, review or event")

		return
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "since must be RFC 3339")

			return
		}
		in.Since = &t
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	rc := http.NewResponseController(w)

	// The status line goes out before the first record: a failure after this
	// point can only be signalled by the missing terminator, which is exactly
	// what a consumer checks for.
	w.WriteHeader(http.StatusOK)

	if err := s.svc.Export(r.Context(), in, w, func() { _ = rc.Flush() }); err != nil {
		slog.ErrorContext(r.Context(), "export failed, stream truncated", "error", err)
	}
}
