package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/agentfeedback/agentfeedback/internal/v3/core"
)

func TestMapSubmissionError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"not found", core.ErrSubmissionNotFound, http.StatusNotFound},
		{"invalid input", core.ErrInvalidInput, http.StatusBadRequest},
		{"wrapped invalid input", fmt.Errorf("create review submission: %w", core.ErrInvalidInput), http.StatusBadRequest},
		{"replay mismatch", core.ErrReplayMismatch, http.StatusConflict},
		{"unknown error", errors.New("boom"), http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := mapSubmissionError(tc.err); got != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, got)
			}
		})
	}
}

func TestErrorMessageHidesInternalDetails(t *testing.T) {
	t.Parallel()

	err := errors.New("no such table: submissions")
	if got := errorMessage(http.StatusInternalServerError, err); got != "internal error" {
		t.Fatalf("5xx must not leak details, got %q", got)
	}
	if got := errorMessage(http.StatusBadRequest, err); got != err.Error() {
		t.Fatalf("4xx must surface the message, got %q", got)
	}
}
