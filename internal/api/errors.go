package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
)

// Error codes of the contract's Error body that only the transport produces.
const (
	codeUnauthorized     = "unauthorized"
	codeMethodNotAllowed = "method_not_allowed"
	codeInternal         = "internal_error"
)

// errorBody is the contract's Error body; details are omitted when empty.
type errorBody struct {
	Error     string        `json:"error"`
	Message   string        `json:"message"`
	RequestID string        `json:"request_id"`
	Details   []core.Detail `json:"details,omitempty"`
}

// writeJSON writes v as JSON (no HTML escaping) with status.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := core.Marshal(v)
	if err != nil {
		slog.ErrorContext(r.Context(), "encode response", "error", err, "request_id", RequestIDFromContext(r.Context()))
		status = http.StatusInternalServerError
		body, _ = core.Marshal(errorBody{Error: codeInternal, Message: "internal error", RequestID: RequestIDFromContext(r.Context())})
	}
	writeRaw(w, status, "application/json", body)
}

func writeRaw(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError writes the Error body with Cache-Control: no-store; a 503 also
// gets Retry-After: 1.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details []core.Detail) {
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, r, status, errorBody{Error: code, Message: message, RequestID: RequestIDFromContext(r.Context()), Details: details})
}

// writeErr maps err: a *core.Problem 1:1, anything else a logged 500.
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var p *core.Problem
	if errors.As(err, &p) {
		writeError(w, r, p.Status, p.Code, p.Message, p.Details)
		return
	}
	slog.ErrorContext(r.Context(), "internal error", "error", err, "path", r.URL.Path,
		"request_id", RequestIDFromContext(r.Context()))
	writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error", nil)
}

// invalid builds a 400 validation_error whose one detail repeats the message.
func invalid(code, pointer, message string) *core.Problem {
	return &core.Problem{Status: http.StatusBadRequest, Code: core.CodeValidation, Message: message,
		Details: []core.Detail{{Code: code, Pointer: pointer, Message: message}}}
}
