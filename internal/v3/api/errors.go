package api

import (
	"errors"
	"net/http"

	"github.com/agentfeedback/agentfeedback/internal/v3/core"
)

// ErrorResponse is the standard error body.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// mapSubmissionError maps core-layer errors to HTTP status codes.
func mapSubmissionError(err error) int {
	switch {
	case errors.Is(err, core.ErrSubmissionNotFound):
		return http.StatusNotFound
	case errors.Is(err, core.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, core.ErrReplayMismatch):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// errorMessage returns the message safe to send to the client for the given
// status: validation and other client errors (< 500) surface err's message
// verbatim, but internal errors (>= 500) never leak details such as SQL or
// filesystem errors — the full error still reaches the logs at the call site.
func errorMessage(status int, err error) string {
	if status >= http.StatusInternalServerError {
		return "internal error"
	}

	return err.Error()
}
