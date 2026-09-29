package api

import (
	"net/http"
	"testing"
)

// noRedirect sends req without following redirects, so a ServeMux redirect
// would surface as its own 3xx.
func noRedirect(t *testing.T, req *http.Request) response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	old := http.DefaultClient
	http.DefaultClient = client
	defer func() { http.DefaultClient = old }()
	return send(t, req)
}

func TestRouteErrors_NonCanonicalPathsNeverRedirect(t *testing.T) {
	e := newEnv(t)
	apiPaths := []string{
		"//api/v1/meta", "/api/v1//meta", "/api/v1/./meta", "/api/v1/x/../meta", "/api/v1",
		"/api/v1/submissions/", "//api/v1/openapi.json", "/api/v1/./schemas", "/api/v1//submissions/processed",
	}
	for _, p := range apiPaths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			assertError(t, noRedirect(t, e.request(t, method, p, nil, false)), http.StatusUnauthorized, "unauthorized")
			assertError(t, noRedirect(t, e.request(t, method, p, nil, true)), http.StatusNotFound, "not_found")
		}
	}
	for _, p := range []string{"//health", "/./ready", "//metrics", "/x/../health", "//nope"} {
		for _, auth := range []bool{false, true} {
			assertError(t, noRedirect(t, e.request(t, http.MethodGet, p, nil, auth)), http.StatusNotFound, "not_found")
		}
	}
}

func TestRouteErrors_OperationalErrorsAreJSONNoStore(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/health", "/ready", "/metrics"} {
		r := e.do(t, http.MethodPost, p, "", false)
		assertError(t, r, http.StatusMethodNotAllowed, "method_not_allowed")
		if r.header.Get("Allow") != "GET, HEAD" {
			t.Errorf("POST %s: Allow = %q", p, r.header.Get("Allow"))
		}
	}
	for _, p := range []string{"/nope", "/health/x", "/api"} {
		assertError(t, e.do(t, http.MethodGet, p, "", false), http.StatusNotFound, "not_found")
	}
}

func TestRouteErrors_ProcessedIsPostOnly(t *testing.T) {
	e := newEnv(t)
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete, http.MethodPut} {
		body := ""
		if method == http.MethodPatch {
			body = `{"processed":true}`
		}
		assertError(t, e.do(t, method, "/api/v1/submissions/processed", body, false), http.StatusUnauthorized, "unauthorized")
		r := e.do(t, method, "/api/v1/submissions/processed", body, true)
		assertError(t, r, http.StatusMethodNotAllowed, "method_not_allowed")
		if got := r.header.Get("Allow"); got != http.MethodPost {
			t.Errorf("%s: Allow = %q, want POST", method, got)
		}
	}
}

func TestReady_Unavailable(t *testing.T) {
	e := newEnv(t)
	e.down.Store(true)
	r := e.do(t, http.MethodGet, "/ready", "", false)
	v := assertError(t, r, http.StatusServiceUnavailable, "unavailable")
	if v["message"] != "the service is shutting down" || r.header.Get("Retry-After") != "1" {
		t.Errorf("shutting down: %s, Retry-After %q", r.body, r.header.Get("Retry-After"))
	}
	e.down.Store(false)
	if r := e.do(t, http.MethodGet, "/ready", "", false); r.status != http.StatusOK || string(r.body) != "READY" ||
		r.header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("ready = %d %q %q", r.status, r.header.Get("Content-Type"), r.body)
	}
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	r = e.do(t, http.MethodGet, "/ready", "", false)
	v = assertError(t, r, http.StatusServiceUnavailable, "unavailable")
	if v["message"] != "the database is unavailable or its schema is not the expected version" || r.header.Get("Retry-After") != "1" {
		t.Errorf("db down: %s, Retry-After %q", r.body, r.header.Get("Retry-After"))
	}
}
