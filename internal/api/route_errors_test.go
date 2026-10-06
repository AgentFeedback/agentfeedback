package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
)

func TestRouteErrors_NotFoundAndMethodNotAllowed(t *testing.T) {
	e := newEnv(t)
	assertError(t, e.do(t, http.MethodGet, "/api/v1/nope/xyz", "", true), http.StatusNotFound, "not_found")
	assertError(t, e.do(t, http.MethodGet, "/api/v1/submissions/1", "", true), http.StatusNotFound, "not_found")

	cases := []struct {
		method, path, allow string
		auth                bool
	}{
		{http.MethodPut, "/api/v1/submissions", "GET, HEAD, POST", true},
		{http.MethodPost, "/api/v1/meta", "GET, HEAD", true},
		{"BREW", "/api/v1/submissions/abc", "DELETE, GET, HEAD, PATCH", true},
		{http.MethodPost, "/api/v1/export", "GET, HEAD", true},
		// A public route answers 405 without a key.
		{http.MethodPost, "/api/v1/schemas", "GET, HEAD", false},
		{http.MethodDelete, "/api/v1/openapi.json", "GET, HEAD", false},
		{http.MethodPut, "/api/v1/schemas/envelope/1", "GET, HEAD", false},
	}
	for _, c := range cases {
		r := e.do(t, c.method, c.path, "", c.auth)
		assertError(t, r, http.StatusMethodNotAllowed, "method_not_allowed")
		if got := r.header.Get("Allow"); got != c.allow {
			t.Errorf("%s %s: Allow = %q, want %q", c.method, c.path, got, c.allow)
		}
	}
}

func TestRouteErrors_BadRequestBodies(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{"not json", "[1,2]", `"s"`, "42", ""} {
		assertError(t, e.do(t, http.MethodPost, "/api/v1/submissions", body, true), http.StatusBadRequest, "bad_request")
	}
}

func TestRouteErrors_BadPathIDs(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"0", "01", "-1", "abc", "99999999999999999999", "1.5"} {
		for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
			body := ""
			if method == http.MethodPatch {
				body = `{"processed":true}`
			}
			r := e.do(t, method, "/api/v1/submissions/"+id, body, true)
			v := assertError(t, r, http.StatusBadRequest, core.CodeValidation)
			d := v["details"].([]any)[0].(map[string]any)
			if d["pointer"] != "/id" {
				t.Errorf("%s %s: pointer %v", method, id, d["pointer"])
			}
		}
	}
	for _, v := range []string{"0", "01", "x", "99999999999999999999"} {
		r := e.do(t, http.MethodGet, "/api/v1/schemas/envelope/"+v, "", false)
		body := assertError(t, r, http.StatusBadRequest, core.CodeValidation)
		if d := body["details"].([]any)[0].(map[string]any); d["pointer"] != "/version" {
			t.Errorf("version %s: pointer %v", v, d["pointer"])
		}
	}
	assertError(t, e.do(t, http.MethodGet, "/api/v1/schemas/nokind/1", "", false), http.StatusNotFound, "not_found")
}

func TestRouteErrors_ReplayMismatch(t *testing.T) {
	e := newEnv(t)
	first := e.do(t, http.MethodPost, "/api/v1/submissions", `{"kind":"friction","key":"k1","payload":{"details":"a"}}`, true)
	if first.status != http.StatusCreated {
		t.Fatalf("create = %d %s", first.status, first.body)
	}
	id := first.json(t)["submission"].(map[string]any)["id"]
	r := e.do(t, http.MethodPost, "/api/v1/submissions", `{"kind":"friction","key":"k1","payload":{"details":"b"}}`, true)
	v := assertError(t, r, http.StatusConflict, "replay_mismatch")
	d := v["details"].([]any)[0].(map[string]any)
	if d["code"] != "key_reused" || d["existing_id"] != id || d["pointer"] != "/key" {
		t.Errorf("detail = %v, want key_reused existing_id %v", d, id)
	}
}

func TestRouteErrors_TooLarge(t *testing.T) {
	e := newEnv(t)
	over := strings.Repeat(" ", envelope.BodyLimit) + "{}"
	assertError(t, e.do(t, http.MethodPost, "/api/v1/submissions", over, true), http.StatusRequestEntityTooLarge, "request_too_large")
	assertError(t, e.do(t, http.MethodPatch, "/api/v1/submissions/1", over, true), http.StatusRequestEntityTooLarge, "request_too_large")
	assertError(t, e.do(t, http.MethodPost, "/api/v1/submissions/processed", over, true), http.StatusRequestEntityTooLarge, "request_too_large")
	assertError(t, e.do(t, http.MethodPost, "/api/v1/import", strings.Repeat("x", core.ImportLimit+1), true), http.StatusRequestEntityTooLarge, "request_too_large")

	// Exactly at the limit is not 413 on a mark (it is a whitespace-padded
	// object core then judges).
	at := strings.Repeat(" ", envelope.BodyLimit-len(`{"processed":true}`)) + `{"processed":true}`
	if r := e.do(t, http.MethodPatch, "/api/v1/submissions/1", at, true); r.status == http.StatusRequestEntityTooLarge {
		t.Errorf("body at the limit answered 413")
	}
}

func TestRouteErrors_ProblemMapping(t *testing.T) {
	cases := []struct {
		err        error
		status     int
		code       string
		retryAfter string
	}{
		{&core.Problem{Status: 503, Code: core.CodeUnavailable, Message: "the database is busy; retry shortly"}, 503, "unavailable", "1"},
		{&core.Problem{Status: 404, Code: core.CodeNotFound, Message: "x"}, 404, "not_found", ""},
		{fmt.Errorf("wrapped: %w", &core.Problem{Status: 400, Code: core.CodeValidation, Message: "m", Details: []core.Detail{{Code: "c", Pointer: "/p", Message: "m"}}}), 400, "validation_error", ""},
		{errors.New("disk on fire"), 500, "internal_error", ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
		req = req.WithContext(ctxWithRequestID(context.Background(), "rid-1"))
		rec.Header().Set("X-Request-Id", "rid-1")
		rec.Header().Set("Cache-Control", "no-store")
		writeErr(rec, req, c.err)
		r := response{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
		v := assertError(t, r, c.status, c.code)
		if got := rec.Header().Get("Retry-After"); got != c.retryAfter {
			t.Errorf("%v: Retry-After = %q, want %q", c.err, got, c.retryAfter)
		}
		if c.status == 500 && v["message"] != "internal error" {
			t.Errorf("internal message leaked: %v", v["message"])
		}
	}
}

func TestRouteErrors_PanicIsJSON500(t *testing.T) {
	srv := httptest.NewServer(New(Config{APIKey: testKey}).Handler())
	defer srv.Close()
	e := &testEnv{srv: srv}
	// A nil Service panics in the handler.
	assertError(t, e.do(t, http.MethodGet, "/api/v1/meta", "", true), http.StatusInternalServerError, "internal_error")
}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestRequestID(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		sent string
		echo bool
	}{
		{"abc.DEF_1-2", true},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false},
		{"bad id!", false},
		{"ünï", false},
		{"", false},
	}
	for _, c := range cases {
		for _, path := range []string{"/api/v1/meta", "/api/v1/nope", "/health"} {
			req := e.request(t, http.MethodGet, path, nil, true)
			if c.sent != "" {
				req.Header.Set("X-Request-Id", c.sent)
			}
			r := send(t, req)
			got := r.header.Get("X-Request-Id")
			if c.echo && got != c.sent {
				t.Errorf("%s: sent %q, got %q", path, c.sent, got)
			}
			if !c.echo && !hex32.MatchString(got) {
				t.Errorf("%s: sent %q, got %q, want 32 hex digits", path, c.sent, got)
			}
			if r.status >= 400 {
				assertError(t, r, r.status, "not_found")
			}
		}
	}
}
