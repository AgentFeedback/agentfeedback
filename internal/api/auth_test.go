package api

import (
	"net/http"
	"testing"
)

func TestAuth_Accepted(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name    string
		headers [][2]string
	}{
		{"Bearer", [][2]string{{"Authorization", "Bearer " + testKey}}},
		{"bearer lower case", [][2]string{{"Authorization", "bearer " + testKey}}},
		{"BEARER upper case", [][2]string{{"Authorization", "BEARER " + testKey}}},
		{"X-Api-Key", [][2]string{{"X-Api-Key", testKey}}},
		{"wrong bearer, right X-Api-Key", [][2]string{{"Authorization", "Bearer nope"}, {"X-Api-Key", testKey}}},
		{"right X-Api-Key, wrong bearer", [][2]string{{"X-Api-Key", testKey}, {"Authorization", "Bearer nope"}}},
		{"right bearer, wrong X-Api-Key", [][2]string{{"Authorization", "Bearer " + testKey}, {"X-Api-Key", "nope"}}},
		{"wrong X-Api-Key, right bearer", [][2]string{{"X-Api-Key", "nope"}, {"Authorization", "Bearer " + testKey}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := e.request(t, http.MethodGet, "/api/v1/meta", nil, false)
			for _, h := range c.headers {
				req.Header.Set(h[0], h[1])
			}
			r := send(t, req)
			if r.status != http.StatusOK {
				t.Fatalf("status = %d: %s", r.status, r.body)
			}
			if cc := r.header.Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q", cc)
			}
		})
	}
}

func TestAuth_Rejected(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name    string
		headers [][2]string
	}{
		{"none", nil},
		{"wrong bearer", [][2]string{{"Authorization", "Bearer nope"}}},
		{"wrong X-Api-Key", [][2]string{{"X-Api-Key", "nope"}}},
		{"basic scheme", [][2]string{{"Authorization", "Basic " + testKey}}},
		{"bare key", [][2]string{{"Authorization", testKey}}},
		{"both wrong", [][2]string{{"Authorization", "Bearer a"}, {"X-Api-Key", "b"}}},
		{"empty bearer", [][2]string{{"Authorization", "Bearer "}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, path := range []string{"/api/v1/meta", "/api/v1/nope/xyz", "/api/v1/submissions/1"} {
				req := e.request(t, http.MethodGet, path, nil, false)
				for _, h := range c.headers {
					req.Header.Set(h[0], h[1])
				}
				r := send(t, req)
				assertError(t, r, http.StatusUnauthorized, "unauthorized")
				if got := r.header.Values("WWW-Authenticate"); len(got) != 1 || got[0] != "Bearer" {
					t.Errorf("%s: WWW-Authenticate = %q, want exactly Bearer", path, got)
				}
			}
		})
	}
}

func TestAuth_EmptyConfiguredKeyAuthorisesNothing(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer ")
	req.Header.Set("X-Api-Key", "")
	if authorised(req, "") {
		t.Fatal("empty configured key authorised a request")
	}
}

func TestAuth_PublicRoutesNeedNoKey(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/api/v1/openapi.json", "/api/v1/schemas", "/api/v1/schemas/envelope/1", "/health", "/ready", "/metrics"} {
		r := e.do(t, http.MethodGet, path, "", false)
		if r.status != http.StatusOK {
			t.Errorf("GET %s = %d: %s", path, r.status, r.body)
		}
		if cc := r.header.Get("Cache-Control"); cc != "" {
			t.Errorf("GET %s: Cache-Control = %q on a public 2xx", path, cc)
		}
	}
}
