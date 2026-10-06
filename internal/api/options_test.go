package api

import (
	"net/http"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
)

// assertNotFound checks a 404 in the contract's Error shape.
func assertNotFound(t *testing.T, r response) {
	t.Helper()
	if r.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", r.status, r.body)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	v := r.json(t)
	if v["error"] != core.CodeNotFound {
		t.Errorf("error = %v, want %s: %s", v["error"], core.CodeNotFound, r.body)
	}
	if m, _ := v["message"].(string); m == "" {
		t.Errorf("message empty: %s", r.body)
	}
	if id := r.header.Get("X-Request-Id"); id == "" || v["request_id"] != id {
		t.Errorf("request_id = %v, X-Request-Id = %q", v["request_id"], id)
	}
}

func TestOptionsUnmountSurfaces(t *testing.T) {
	e := newEnvWith(t, Config{NoMCP: true, NoMetrics: true, NoHealth: true})
	for _, c := range []struct {
		method, path string
		auth         bool
	}{
		{"GET", "/health", false},
		{"GET", "/ready", false},
		{"GET", "/metrics", false},
		{"POST", "/mcp", false},
		{"POST", "/mcp", true},
		{"GET", "/mcp/x", false},
	} {
		t.Run(c.method+c.path, func(t *testing.T) {
			assertNotFound(t, e.do(t, c.method, c.path, "", c.auth))
		})
	}
	if r := e.do(t, "GET", "/api/v1/meta", "", true); r.status != http.StatusOK {
		t.Errorf("GET /api/v1/meta = %d, want 200: %s", r.status, r.body)
	}
	if r := e.do(t, "POST", "/api/v1/submissions", frictionBody, true); r.status != http.StatusCreated {
		t.Errorf("POST /api/v1/submissions = %d, want 201: %s", r.status, r.body)
	}
}

func TestOptionsDefaultKeepsSurfaces(t *testing.T) {
	e := newEnv(t)
	if r := e.do(t, "GET", "/health", "", false); r.status != http.StatusOK {
		t.Errorf("GET /health = %d, want 200: %s", r.status, r.body)
	}
	if r := e.do(t, "POST", "/mcp", "", false); r.status != http.StatusUnauthorized {
		t.Errorf("POST /mcp without key = %d, want 401: %s", r.status, r.body)
	}
}
