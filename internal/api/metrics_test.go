package api

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var labelRE = regexp.MustCompile(`(route|method)="([^"]*)"`)

func TestMetrics_LabelsAreBounded(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodGet, "/api/v1/submissions/123", "", true)
	e.do(t, http.MethodGet, "/api/v1/submissions/456", "", true)
	e.do(t, http.MethodGet, "/api/v1/submissions/456", "", false)
	e.do(t, http.MethodGet, "/api/v1/nope/xyz", "", true)
	e.do(t, "BREW", "/api/v1/submissions/abc", "", true)
	e.do(t, "BREW", "/api/v1/schemas", "", false)
	e.do(t, http.MethodGet, "/nope", "", false)
	e.do(t, http.MethodPost, "/api/v1/submissions", frictionBody, true)
	e.do(t, http.MethodPost, "/api/v1/submissions", frictionBody, true)
	e.do(t, http.MethodPost, "/api/v1/submissions", "[]", true)

	r := e.do(t, http.MethodGet, "/metrics", "", false)
	if r.status != http.StatusOK {
		t.Fatalf("/metrics = %d", r.status)
	}
	patterns := []string{
		"GET /api/v1/openapi.json", "GET /api/v1/schemas", "GET /api/v1/schemas/{kind}/{version}",
		"POST /api/v1/submissions", "GET /api/v1/submissions", "GET /api/v1/submissions/{id}",
		"PATCH /api/v1/submissions/{id}", "DELETE /api/v1/submissions/{id}", "POST /api/v1/submissions/processed",
		"GET /api/v1/stats", "GET /api/v1/export", "HEAD /api/v1/export", "POST /api/v1/import", "GET /api/v1/meta",
		"GET /health", "GET /ready", "GET /metrics", "/api/v1/", "unmatched",
	}
	methods := []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS", "PATCH", "OTHER"}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(r.body), "\n") {
		if !strings.HasPrefix(line, "http_request") {
			continue
		}
		for _, m := range labelRE.FindAllStringSubmatch(line, -1) {
			seen[m[1]+"="+m[2]] = true
			switch m[1] {
			case "route":
				if !slices.Contains(patterns, m[2]) {
					t.Errorf("route label %q is not a registered pattern", m[2])
				}
			case "method":
				if !slices.Contains(methods, m[2]) {
					t.Errorf("method label %q not allow-listed", m[2])
				}
			}
		}
		for _, raw := range []string{"123", "456", "abc", "xyz", "nope", "BREW"} {
			if strings.Contains(line, raw) && !strings.Contains(line, "le=") {
				t.Errorf("raw client input %q in %s", raw, line)
			}
		}
	}
	for _, want := range []string{"route=GET /api/v1/submissions/{id}", "method=OTHER", "route=/api/v1/"} {
		if !seen[want] {
			t.Errorf("no series with %s", want)
		}
	}
	for _, want := range []string{
		`submissions_created_total{outcome="created"} 1`,
		`submissions_created_total{outcome="existing"} 1`,
		`submissions_created_total{outcome="rejected"} 1`,
		"agentfeedback_submissions_unprocessed 1",
		"agentfeedback_db_bytes ",
		"agentfeedback_sqlite_busy_total 0",
	} {
		if !strings.Contains(string(r.body), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}
