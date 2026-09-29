package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestQueryGrammar(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		path, code, pointer string
	}{
		// unknown names, per route
		{"/api/v1/submissions?bogus=1", detailUnknown, "?bogus"},
		{"/api/v1/submissions?by=kind", detailUnknown, "?by"},
		{"/api/v1/stats?limit=5", detailUnknown, "?limit"},
		{"/api/v1/stats?include=payload", detailUnknown, "?include"},
		{"/api/v1/export?q=x", detailUnknown, "?q"},
		{"/api/v1/submissions/1?x=1", detailUnknown, "?x"},
		{"/api/v1/schemas/envelope/1?x=1", detailUnknown, "?x"},
		// empty values
		{"/api/v1/submissions?kind=", detailEmpty, "?kind"},
		{"/api/v1/submissions?kind", detailEmpty, "?kind"},
		{"/api/v1/submissions?exclude_kind=a&exclude_kind=", detailEmpty, "?exclude_kind"},
		{"/api/v1/stats?by=", detailEmpty, "?by"},
		// repeated singletons
		{"/api/v1/submissions?kind=a&kind=b", detailRepeated, "?kind"},
		{"/api/v1/stats?top=1&top=2", detailRepeated, "?top"},
		{"/api/v1/export?limit=1&limit=1", detailRepeated, "?limit"},
		// integers
		{"/api/v1/submissions?limit=abc", detailType, "?limit"},
		{"/api/v1/submissions?limit=01", detailType, "?limit"},
		{"/api/v1/submissions?limit=-1", detailType, "?limit"},
		{"/api/v1/submissions?limit=+1", detailType, "?limit"},
		{"/api/v1/submissions?limit=1.0", detailType, "?limit"},
		{"/api/v1/submissions?before_id=99999999999999999999", detailRange, "?before_id"},
		{"/api/v1/export?limit=99999999999999999999", detailRange, "?limit"},
		{"/api/v1/submissions?schema_version=x", detailType, "?schema_version"},
		{"/api/v1/stats?top=1e2", detailType, "?top"},
		{"/api/v1/export?after_id=%201", detailType, "?after_id"},
		// booleans
		{"/api/v1/submissions?processed=1", detailType, "?processed"},
		{"/api/v1/submissions?processed=True", detailType, "?processed"},
		{"/api/v1/stats?redacted=yes", detailType, "?redacted"},
		// timestamps
		{"/api/v1/submissions?since=2026-09-27", detailFormat, "?since"},
		{"/api/v1/submissions?until=2026-09-27T10:00:00+02:00", detailFormat, "?until"},
		{"/api/v1/export?since=yesterday", detailFormat, "?since"},
		// values the transport checks
		{"/api/v1/submissions?include=everything", detailRange, "?include"},
		// values core checks
		{"/api/v1/submissions?limit=0", "out_of_range", "?limit"},
		{"/api/v1/submissions?limit=101&include=payload", "out_of_range", "?limit"},
		{"/api/v1/submissions?before_id=1&after_id=1", "invalid_value", "?before_id"},
		{"/api/v1/submissions?on=updated_at", "out_of_range", "?on"},
		{"/api/v1/submissions?content_hash=ABC", "invalid_format", "?content_hash"},
		{"/api/v1/stats?by=kind,nope", "out_of_range", "?by"},
		{"/api/v1/stats?bucket=month", "out_of_range", "?bucket"},
		{"/api/v1/stats?top=51", "out_of_range", "?top"},
		{"/api/v1/export?limit=501", "out_of_range", "?limit"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			auth := true
			r := e.do(t, http.MethodGet, c.path, "", auth)
			assertDetail(t, r, c.code, c.pointer)
		})
	}
	// routes with a body and no query parameters
	for _, c := range []struct{ method, path, code, pointer string }{
		{http.MethodPost, "/api/v1/submissions/processed?x=1", detailUnknown, "?x"},
		{http.MethodPost, "/api/v1/import?x=1", detailUnknown, "?x"},
		{http.MethodPatch, "/api/v1/submissions/1?x=1", detailUnknown, "?x"},
		{http.MethodDelete, "/api/v1/submissions/1?x=1", detailUnknown, "?x"},
		{http.MethodPost, "/api/v1/submissions/processed?x=%zz", detailUnknown, "?x"},
		{http.MethodPatch, "/api/v1/submissions/1?%zz=1", detailFormat, "?"},
		{http.MethodDelete, "/api/v1/submissions/1?a=1;b=2", detailFormat, "?"},
	} {
		body := ""
		if c.method != http.MethodDelete {
			body = `{"processed":true}`
		}
		assertDetail(t, e.do(t, c.method, c.path, body, true), c.code, c.pointer)
	}
	// malformed percent-encoding names its parameter
	for _, c := range []struct{ path, pointer, contains string }{
		{"/api/v1/submissions?kind=%zz", "?kind", "kind"},
		{"/api/v1/submissions?limit=1&kind=%g1", "?kind", "kind"},
		{"/api/v1/submissions?k%zzind=a", "?", `"k%zzind"`},
		{"/api/v1/submissions?kind=a;limit=1", "?", ";"},
	} {
		r := e.do(t, http.MethodGet, c.path, "", true)
		assertDetail(t, r, detailFormat, c.pointer)
		if m, _ := r.json(t)["message"].(string); !strings.Contains(m, c.contains) {
			t.Errorf("%s: message %q does not contain %s", c.path, m, c.contains)
		}
	}
	// HEAD /export validates the same parameters.
	if r := e.do(t, http.MethodHead, "/api/v1/export?limit=0", "", true); r.status != http.StatusBadRequest {
		t.Errorf("HEAD export limit=0 = %d", r.status)
	}
}

func TestQueryGrammar_Accepted(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{
		"/api/v1/submissions?exclude_kind=review&exclude_kind=event&processed=false&redacted=true&since=2026-09-27T10:00:00Z&until=2026-09-28T10:00:00.5%2B02:00&limit=10&include=payload&schema_version=1&q=x",
		"/api/v1/submissions?after_id=0",
		"/api/v1/stats?by=kind,project&top=0&bucket=week&on=occurred_at",
		"/api/v1/export?kind=friction&since=2026-09-27T10:00:00Z&after_id=0&limit=500",
	} {
		if r := e.do(t, http.MethodGet, path, "", true); r.status != http.StatusOK {
			t.Errorf("%s = %d %s", path, r.status, r.body)
		}
	}
	// routes whose contract lists no 400 ignore their query string
	for _, c := range []struct {
		path string
		auth bool
	}{{"/api/v1/meta?x=1", true}, {"/api/v1/schemas?x=1&x=2", false}, {"/api/v1/openapi.json?x=", false}} {
		if r := e.do(t, http.MethodGet, c.path, "", c.auth); r.status != http.StatusOK {
			t.Errorf("%s = %d %s", c.path, r.status, r.body)
		}
	}
	// create ignores its query string
	if r := e.do(t, http.MethodPost, "/api/v1/submissions?anything=&x=1&x=2", frictionBody, true); r.status != http.StatusCreated {
		t.Errorf("create with query = %d %s", r.status, r.body)
	}
}
