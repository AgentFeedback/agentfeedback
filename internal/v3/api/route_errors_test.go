package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestRouter_UnknownRouteReturnsJSON404(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	status, body := do(t, srv, http.MethodGet, "/api/v1/nope", "", true)
	if status != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (body %s)", status, body)
	}
	if got := decode[ErrorResponse](t, body); got.Error != "not_found" || got.Message == "" {
		t.Fatalf("body %s, want a not_found error", body)
	}
}

func TestRouter_WrongMethodReturnsJSON405(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	status, body := do(t, srv, http.MethodDelete, "/api/v1/submissions", "", true)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405 (body %s)", status, body)
	}
	if got := decode[ErrorResponse](t, body); got.Error != "method_not_allowed" {
		t.Fatalf("body %s, want a method_not_allowed error", body)
	}
}

// An unauthenticated caller must not be able to tell a real route from an
// invented one: auth runs before routing.
func TestRouter_UnknownRouteWithoutKeyReturns401(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	status, body := do(t, srv, http.MethodGet, "/api/v1/nope", "", false)
	if status != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 (body %s)", status, body)
	}
}

func TestRouter_PanicIsRecoveredAndObserved(t *testing.T) {
	t.Parallel()

	// A nil Service makes the create handler panic on its first use of the
	// database — a stand-in for any handler bug.
	srv := newTestServerWithoutService(t)

	status, body := do(t, srv, http.MethodPost, "/api/v1/frictions",
		`{"machine_name":"m","coordinator_model":"c","category":"tooling","summary":"boom"}`, true)
	if status != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 (body %s)", status, body)
	}
	if got := decode[ErrorResponse](t, body); got.Error != "internal_error" {
		t.Fatalf("body %s, want an internal_error body", body)
	}

	// observe wraps recoverPanic, so the recovered request is counted with the
	// status it actually returned.
	_, metrics := do(t, srv, http.MethodGet, "/metrics", "", false)
	line := `http_requests_total{code="500",method="POST",route="POST /api/v1/frictions"} 1`
	if !strings.Contains(string(metrics), line) {
		t.Fatalf("metrics do not contain %q:\n%s", line, metrics)
	}
}
