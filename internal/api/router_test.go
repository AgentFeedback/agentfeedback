package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/internal/core"
	"github.com/agentfeedback/agentfeedback/internal/v3/store"
)

const testAPIKey = "test-key"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "agentfeedback.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	srv := httptest.NewServer(New(Config{Service: core.New(db), APIKey: testAPIKey}).Handler())
	t.Cleanup(srv.Close)

	return srv
}

// newTestServerWithoutService builds a server whose Service is nil, so any
// handler that reaches the core layer panics.
func newTestServerWithoutService(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(New(Config{APIKey: testAPIKey}).Handler())
	t.Cleanup(srv.Close)

	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, auth bool) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if auth {
		req.Header.Set("X-Api-Key", testAPIKey)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return resp.StatusCode, out
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()

	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}

	return v
}

func TestRouter_OperationalEndpoints(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	for path, want := range map[string]string{"/health": "OK", "/ready": "READY"} {
		status, body := do(t, srv, http.MethodGet, path, "", false)
		if status != http.StatusOK || strings.TrimSpace(string(body)) != want {
			t.Fatalf("GET %s: status %d body %q", path, status, body)
		}
	}

	status, body := do(t, srv, http.MethodGet, "/metrics", "", false)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", status)
	}
	for _, metric := range []string{
		"http_requests_total", "agentfeedback_submissions_unprocessed", "agentfeedback_db_bytes", "agentfeedback_sqlite_busy_total",
	} {
		if !strings.Contains(string(body), metric) {
			t.Fatalf("metrics output is missing %s", metric)
		}
	}
}

func TestRouter_RequiresAPIKey(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	for _, path := range []string{"/api/v1/submissions", "/api/v1/export", "/api/v1/submissions/1"} {
		status, body := do(t, srv, http.MethodGet, path, "", false)
		if status != http.StatusUnauthorized {
			t.Fatalf("GET %s without a key: status %d body %s", path, status, body)
		}
	}
}

func TestRouter_FullFlow(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	// Friction: created, then absorbed as a duplicate.
	friction := `{"machine_name":"workstation-a","coordinator_model":"claude-fable-5","category":"documentation","summary":"docs drifted"}`
	status, body := do(t, srv, http.MethodPost, "/api/v1/frictions", friction, true)
	if status != http.StatusCreated {
		t.Fatalf("create friction: status %d body %s", status, body)
	}
	created := decode[core.Record](t, body)
	if created.Family != "friction" || created.RunID != nil || created.PayloadHash == "" {
		t.Fatalf("unexpected friction record: %+v", created)
	}
	if !strings.HasSuffix(created.CreatedAt, "Z") || len(created.CreatedAt) != len("2026-09-17T20:06:48.123456Z") {
		t.Fatalf("created_at must be microsecond RFC 3339: %q", created.CreatedAt)
	}
	if strings.Contains(string(body), `"processed_at"`) || strings.Contains(string(body), `"resolution"`) {
		t.Fatalf("null fields must be omitted: %s", body)
	}

	status, body = do(t, srv, http.MethodPost, "/api/v1/frictions", friction, true)
	if status != http.StatusOK || decode[core.Record](t, body).ID != created.ID {
		t.Fatalf("duplicate friction: status %d body %s", status, body)
	}

	// Review: created, replayed, mismatched.
	review := `{"skill":"review-panel","machine_name":"workstation-a","coordinator_model":"claude-fable-5",` +
		`"run_id":"run-1","reviewers":[{"slot":"a","model":"m","status":"completed"}]}`
	status, body = do(t, srv, http.MethodPost, "/api/v1/reviews", review, true)
	if status != http.StatusCreated {
		t.Fatalf("create review: status %d body %s", status, body)
	}
	reviewRec := decode[core.Record](t, body)

	status, _ = do(t, srv, http.MethodPost, "/api/v1/reviews", review, true)
	if status != http.StatusOK {
		t.Fatalf("replay review: status %d", status)
	}
	status, body = do(t, srv, http.MethodPost, "/api/v1/reviews",
		strings.Replace(review, `"status":"completed"`, `"status":"timeout"`, 1), true)
	if status != http.StatusConflict || decode[ErrorResponse](t, body).Error != "replay_mismatch" {
		t.Fatalf("review mismatch: status %d body %s", status, body)
	}

	// Event: created, replayed under a different key order, mismatched.
	event := `{"kind":"deploy","key":"k1","machine_name":"workstation-a","coordinator_model":"claude-fable-5","payload":{"b":2,"a":1}}`
	status, body = do(t, srv, http.MethodPost, "/api/v1/events", event, true)
	if status != http.StatusCreated {
		t.Fatalf("create event: status %d body %s", status, body)
	}
	eventRec := decode[core.Record](t, body)
	if string(eventRec.Payload) != `{"b":2,"a":1}` {
		t.Fatalf("event payload must be returned verbatim: %s", eventRec.Payload)
	}

	status, _ = do(t, srv, http.MethodPost, "/api/v1/events",
		strings.Replace(event, `{"b":2,"a":1}`, `{"a":1,"b":2}`, 1), true)
	if status != http.StatusOK {
		t.Fatalf("replay event: status %d", status)
	}
	status, body = do(t, srv, http.MethodPost, "/api/v1/events",
		strings.Replace(event, `{"b":2,"a":1}`, `{"a":9,"b":2}`, 1), true)
	if status != http.StatusConflict {
		t.Fatalf("event mismatch: status %d body %s", status, body)
	}

	// Reserved names.
	status, _ = do(t, srv, http.MethodPost, "/api/v1/events", strings.Replace(event, `"deploy"`, `"friction"`, 1), true)
	if status != http.StatusBadRequest {
		t.Fatalf("kind=friction must be rejected: status %d", status)
	}
	status, _ = do(t, srv, http.MethodPost, "/api/v1/reviews", strings.Replace(review, `"review-panel"`, `"friction"`, 1), true)
	if status != http.StatusBadRequest {
		t.Fatalf("skill=friction must be rejected: status %d", status)
	}

	// Malformed bodies.
	status, _ = do(t, srv, http.MethodPost, "/api/v1/frictions",
		strings.Replace(friction, `{"machine_name"`, `{"bogus":1,"machine_name"`, 1), true)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown field must be rejected: status %d", status)
	}
	status, _ = do(t, srv, http.MethodPost, "/api/v1/frictions",
		friction+strings.Repeat(" ", maxRequestBytes), true)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body must be rejected: status %d", status)
	}

	// Get.
	status, body = do(t, srv, http.MethodGet, "/api/v1/submissions/"+strconv.FormatInt(reviewRec.ID, 10), "", true)
	if status != http.StatusOK || decode[core.Record](t, body).ID != reviewRec.ID {
		t.Fatalf("get: status %d body %s", status, body)
	}
	if status, _ = do(t, srv, http.MethodGet, "/api/v1/submissions/999999", "", true); status != http.StatusNotFound {
		t.Fatalf("missing id must be 404: status %d", status)
	}
	if status, _ = do(t, srv, http.MethodGet, "/api/v1/submissions/abc", "", true); status != http.StatusBadRequest {
		t.Fatalf("non-numeric id must be 400: status %d", status)
	}

	// List.
	status, body = do(t, srv, http.MethodGet, "/api/v1/submissions?limit=2", "", true)
	if status != http.StatusOK {
		t.Fatalf("list: status %d body %s", status, body)
	}
	page := decode[ListSubmissionsResponse](t, body)
	if page.Total != 3 || !page.HasMore || page.NextBeforeID == nil || page.Limit != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}
	for _, row := range page.Submissions {
		if len(row.Payload) != 0 {
			t.Fatal("summaries must not carry the payload")
		}
	}

	status, body = do(t, srv, http.MethodGet,
		"/api/v1/submissions?include=payload&before_id="+strconv.FormatInt(*page.NextBeforeID, 10), "", true)
	if status != http.StatusOK {
		t.Fatalf("list page 2: status %d body %s", status, body)
	}
	page2 := decode[ListSubmissionsResponse](t, body)
	if len(page2.Submissions) != 1 || len(page2.Submissions[0].Payload) == 0 {
		t.Fatalf("unexpected second page: %+v", page2)
	}

	for _, query := range []string{
		"?since=nope", "?processed=maybe", "?family=nope", "?before_id=0", "?before_id=x",
		"?limit=x", "?offset=x", "?include=everything", "?offset=1&before_id=2",
	} {
		if status, body = do(t, srv, http.MethodGet, "/api/v1/submissions"+query, "", true); status != http.StatusBadRequest {
			t.Fatalf("list%s must be 400, got %d (%s)", query, status, body)
		}
	}

	// Processed, with a resolution.
	status, body = do(t, srv, http.MethodPost, "/api/v1/submissions/processed",
		`{"ids":[`+strconv.FormatInt(created.ID, 10)+`,999999],"resolution":"fixed in example@1a2b3c4"}`, true)
	if status != http.StatusOK {
		t.Fatalf("processed: status %d body %s", status, body)
	}
	res := decode[SetProcessedResponse](t, body)
	if len(res.Updated) != 1 || len(res.NotFound) != 1 || res.Resolution == nil {
		t.Fatalf("unexpected processed result: %+v", res)
	}

	status, body = do(t, srv, http.MethodPost, "/api/v1/submissions/processed",
		`{"ids":[`+strconv.FormatInt(created.ID, 10)+`],"processed":false,"resolution":"x"}`, true)
	if status != http.StatusBadRequest {
		t.Fatalf("resolution with processed=false must be 400: status %d body %s", status, body)
	}
	status, _ = do(t, srv, http.MethodPost, "/api/v1/submissions/processed", `{"ids":[]}`, true)
	if status != http.StatusBadRequest {
		t.Fatalf("empty ids must be 400: status %d", status)
	}

	// Export.
	status, body = do(t, srv, http.MethodGet, "/api/v1/export", "", true)
	if status != http.StatusOK {
		t.Fatalf("export: status %d", status)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected header + 3 records + terminator, got %d lines: %s", len(lines), body)
	}
	term := decode[core.ExportTerminator](t, []byte(lines[len(lines)-1]))
	if !term.ExportComplete || term.Count != 3 {
		t.Fatalf("unexpected terminator: %+v", term)
	}
	if got := decode[core.Record](t, []byte(lines[3])); got.ID != eventRec.ID {
		t.Fatalf("export must be ordered by ascending id, last record is %d", got.ID)
	}
}
