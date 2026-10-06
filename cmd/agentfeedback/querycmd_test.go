package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
)

var clockStart = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// live is the v1 API over a fresh temporary database, with a clock that moves
// one second per reading and a log of every request's method and path.
type live struct {
	srv *httptest.Server
	key string
	mu  sync.Mutex
	log []string
}

// liveServer starts the server and points the client at it; call isolate
// first.
func liveServer(t *testing.T) *live {
	t.Helper()
	l := newLive(t, testKey)
	t.Setenv(envURL, l.srv.URL)
	t.Setenv(envAPIKey, testKey)

	return l
}

// newLive starts the server with key as its API key and leaves the client's
// environment alone.
func newLive(t *testing.T, key string) *live {
	t.Helper()
	db := openDB(t, filepath.Join(t.TempDir(), "live.db"))
	var clockMu sync.Mutex
	tick := clockStart
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		tick = tick.Add(time.Second)

		return tick
	}
	svc := core.New(db, core.Config{Version: "4.0.0", Features: api.Features}, core.WithClock(clock))
	h := api.New(api.Config{Service: svc, DB: db, APIKey: key}).Handler()
	l := &live{key: key}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.log = append(l.log, r.Method+" "+r.URL.Path)
		l.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(l.srv.Close)

	return l
}

func (l *live) requests() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.log...)
}

// call sends one request with the server's key and returns status and body.
func (l *live) call(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, l.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+l.key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, raw
}

// seed creates one submission through the server and returns its id and uid.
func (l *live) seed(t *testing.T, body string) (int64, string) {
	t.Helper()
	status, raw := l.call(t, http.MethodPost, "/api/v1/submissions", body)
	var out struct {
		Submission struct {
			ID  int64  `json:"id"`
			UID string `json:"uid"`
		} `json:"submission"`
	}
	if status != http.StatusCreated || json.Unmarshal(raw, &out) != nil {
		t.Fatalf("seed %s: %d %s", body, status, raw)
	}

	return out.Submission.ID, out.Submission.UID
}

// record returns one record as the server stores it.
func (l *live) record(t *testing.T, id int64) map[string]any {
	t.Helper()
	status, raw := l.call(t, http.MethodGet, fmt.Sprintf("/api/v1/submissions/%d", id), "")
	var rec map[string]any
	if status != http.StatusOK || json.Unmarshal(raw, &rec) != nil {
		t.Fatalf("get %d: %d %s", id, status, raw)
	}

	return rec
}

// stub serves fn and points the client at it; call isolate first.
func stub(t *testing.T, fn http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	t.Setenv(envURL, srv.URL)
	t.Setenv(envAPIKey, testKey)
}

// lastLine is the last line of stdout.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")

	return lines[len(lines)-1]
}

func fixClock(t *testing.T, now time.Time) {
	t.Helper()
	old := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })
}

const emptyPage = `{"submissions":[],"limit":50,"total":0,"has_more":false,"next_before_id":null,"next_after_id":null}`

func TestFilters_ReachQueryParams(t *testing.T) {
	isolate(t)
	fixClock(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	var got url.Values
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		if r.URL.Path == "/api/v1/stats" {
			_, _ = io.WriteString(w, `{"total":0,"open":0,"processed":0,"redacted":0}`)

			return
		}
		_, _ = io.WriteString(w, emptyPage)
	})
	ex := "install-check"
	tests := []struct {
		args []string
		want url.Values
	}{
		{nil, url.Values{"exclude_kind": {ex}}},
		{[]string{"--kind", "friction"}, url.Values{"kind": {"friction"}}},
		{[]string{"--include-kind", "install-check"}, url.Values{}},
		{[]string{"--include-kind", "review"}, url.Values{"exclude_kind": {ex}}},
		{[]string{"--schema-version", "1"}, url.Values{"schema_version": {"1"}, "exclude_kind": {ex}}},
		{[]string{"--key", "k"}, url.Values{"key": {"k"}, "exclude_kind": {ex}}},
		{[]string{"--machine", "m"}, url.Values{"machine": {"m"}, "exclude_kind": {ex}}},
		{[]string{"--model", "x"}, url.Values{"model": {"x"}, "exclude_kind": {ex}}},
		{[]string{"--project", "p"}, url.Values{"project": {"p"}, "exclude_kind": {ex}}},
		{[]string{"--harness", "h"}, url.Values{"harness": {"h"}, "exclude_kind": {ex}}},
		{[]string{"--category", "tooling"}, url.Values{"category": {"tooling"}, "exclude_kind": {ex}}},
		{[]string{"--fix-status", "applied"}, url.Values{"fix_status": {"applied"}, "exclude_kind": {ex}}},
		{[]string{"--exclude-kind", "a", "--exclude-kind", "b"}, url.Values{"exclude_kind": {"a", "b", ex}}},
		{[]string{"--verdict", "fixed"}, url.Values{"verdict": {"fixed"}, "exclude_kind": {ex}}},
		{[]string{"--open"}, url.Values{"processed": {"false"}, "exclude_kind": {ex}}},
		{[]string{"--processed"}, url.Values{"processed": {"true"}, "exclude_kind": {ex}}},
		{[]string{"--redacted", "true"}, url.Values{"redacted": {"true"}, "exclude_kind": {ex}}},
		{[]string{"--content-hash", "ab"}, url.Values{"content_hash": {"ab"}, "exclude_kind": {ex}}},
		{[]string{"--since", "2026-09-01T00:00:00+02:00"}, url.Values{"since": {"2026-09-01T00:00:00+02:00"}, "exclude_kind": {ex}}},
		{[]string{"--since", "2d"}, url.Values{"since": {"2026-09-28T12:00:00Z"}, "exclude_kind": {ex}}},
		{[]string{"--until", "90m"}, url.Values{"until": {"2026-09-30T10:30:00Z"}, "exclude_kind": {ex}}},
		{[]string{"--since", "1w", "--until", "3h"}, url.Values{"since": {"2026-09-23T12:00:00Z"}, "until": {"2026-09-30T09:00:00Z"}, "exclude_kind": {ex}}},
		{[]string{"--on", "occurred_at"}, url.Values{"on": {"occurred_at"}, "exclude_kind": {ex}}},
		{[]string{"--q", "a b"}, url.Values{"q": {"a b"}, "exclude_kind": {ex}}},
	}
	for _, cmd := range []string{"list", "stats"} {
		for _, tt := range tests {
			r := runCLI(t, "", append([]string{cmd}, tt.args...)...)
			if r.code != 0 || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("%s %v: code %d stderr %q\n got %v\nwant %v", cmd, tt.args, r.code, r.stderr, got, tt.want)
			}
		}
	}

	listOnly := []struct {
		args []string
		want url.Values
	}{
		{[]string{"--limit", "7"}, url.Values{"limit": {"7"}, "exclude_kind": {ex}}},
		{[]string{"--limit", "0"}, url.Values{"limit": {"0"}, "exclude_kind": {ex}}},
		{[]string{"--all", "--limit", "3"}, url.Values{"limit": {"3"}, "exclude_kind": {ex}}},
		{[]string{"--before-id", "9"}, url.Values{"before_id": {"9"}, "exclude_kind": {ex}}},
		{[]string{"--after-id", "0"}, url.Values{"after_id": {"0"}, "exclude_kind": {ex}}},
		{[]string{"--include", "payload"}, url.Values{"include": {"payload"}, "exclude_kind": {ex}}},
		{[]string{"--all"}, url.Values{"limit": {"500"}, "exclude_kind": {ex}}},
		{[]string{"--all", "--include", "payload"}, url.Values{"limit": {"100"}, "include": {"payload"}, "exclude_kind": {ex}}},
	}
	for _, tt := range listOnly {
		if r := runCLI(t, "", append([]string{"list"}, tt.args...)...); r.code != 0 || !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("list %v: %+v\n got %v\nwant %v", tt.args, r, got, tt.want)
		}
	}
	statsOnly := []struct {
		args []string
		want url.Values
	}{
		{[]string{"--by", "project,category"}, url.Values{"by": {"project,category"}, "exclude_kind": {ex}}},
		{[]string{"--top", "0"}, url.Values{"top": {"0"}, "exclude_kind": {ex}}},
		{[]string{"--bucket", "week"}, url.Values{"bucket": {"week"}, "exclude_kind": {ex}}},
	}
	for _, tt := range statsOnly {
		if r := runCLI(t, "", append([]string{"stats"}, tt.args...)...); r.code != 0 || !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("stats %v: %+v\n got %v\nwant %v", tt.args, r, got, tt.want)
		}
	}

	for _, args := range [][]string{
		{"list", "--open", "--processed"}, {"stats", "--open", "--processed"}, {"list", "--json", "--tsv"},
		{"list", "--since", "yesterday"}, {"list", "--until", "5y"}, {"list", "extra"},
		{"list", "--since", "106752d"}, {"stats", "--until", "100000w"}, {"list", "--since", "99999999999999999999m"},
	} {
		got = nil
		if r := runCLI(t, "", args...); r.code != 2 || got != nil {
			t.Fatalf("%v: %+v, sent %v", args, r, got)
		}
	}
}

func TestList_LiveServer(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	l.seed(t, `{"kind":"friction","project":"alpha","summary":"tabs\there\nand a newline","payload":{"category":"tooling","d":"x\\y"}}`)
	l.seed(t, `{"kind":"install-check","summary":"install ok"}`)
	l.seed(t, `{"kind":"mystery","summary":"`+strings.Repeat("x", 120)+`","payload":{"a":"b"}}`)
	l.call(t, http.MethodPost, "/api/v1/submissions/processed", `{"ids":[3],"processed":true,"verdict":"invalid"}`)

	r := runCLI(t, "", "list")
	want := "#3  2026-09-29T12:00  mystery  -  invalid  " + strings.Repeat("x", 100) + "…\n" +
		"#1  2026-09-29T12:00  friction  alpha  open  tabs here and a newline\n" +
		"2 of 2\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("human %+v\nwant %q", r, want)
	}

	r = runCLI(t, "", "list", "--json")
	_, body := l.call(t, http.MethodGet, "/api/v1/submissions?exclude_kind=install-check", "")
	if r.code != 0 || r.stdout != string(body)+"\n" {
		t.Fatalf("json %+v\nwant %s", r, body)
	}

	r = runCLI(t, "", "list", "--tsv", "--include-kind", "install-check", "--kind", "friction", "--include", "payload")
	rec := l.record(t, 1)
	wantTSV := strings.Join(append(tsvColumns[:len(tsvColumns):len(tsvColumns)], "payload"), "\t") + "\n" +
		strings.Join([]string{"1", rec["uid"].(string), "friction", "1", "", rec["created_at"].(string), "",
			"", "", "", "alpha", "", "", "", rec["content_hash"].(string), `tabs\there and a newline`, `{"category":"tooling","d":"x\\\\y"}`}, "\t") + "\n"
	if r.code != 0 || r.stdout != wantTSV {
		t.Fatalf("tsv %+v\nwant %q", r, wantTSV)
	}

	r = runCLI(t, "", "list", "--include-kind", "install-check", "--limit", "1")
	if r.code != 0 || !strings.HasSuffix(r.stdout, "1 of 3; next page: --before-id 3\n") {
		t.Fatalf("install-check lifted %+v", r)
	}
}

func TestList_AllPaging(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	for i := range 5 {
		l.seed(t, fmt.Sprintf(`{"kind":"friction","summary":"row %d"}`, i+1))
	}
	r := runCLI(t, "", "list", "--all", "--limit", "2", "--json")
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if r.code != 0 || len(lines) != 3 {
		t.Fatalf("json pages %+v", r)
	}
	var ids []int64
	for _, line := range lines {
		var p struct{ Submissions []struct{ ID int64 } }
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatal(err)
		}
		for _, s := range p.Submissions {
			ids = append(ids, s.ID)
		}
	}
	if !reflect.DeepEqual(ids, []int64{5, 4, 3, 2, 1}) {
		t.Fatalf("ids %v", ids)
	}
	r = runCLI(t, "", "list", "--all", "--limit", "2", "--after-id", "0")
	if r.code != 0 || strings.Count(r.stdout, "\n") != 6 || !strings.HasPrefix(r.stdout, "#1 ") || !strings.HasSuffix(r.stdout, "5 of 5\n") {
		t.Fatalf("human after %+v", r)
	}
	r = runCLI(t, "", "list", "--all", "--tsv")
	if r.code != 0 || strings.Count(r.stdout, "\n") != 6 {
		t.Fatalf("tsv all %+v", r)
	}

	for name, page := range map[string]string{
		"stuck cursor": `{"submissions":[{"id":10}],"limit":1,"total":9,"has_more":true,"next_before_id":10,"next_after_id":null}`,
		"empty page":   `{"submissions":[],"limit":1,"total":9,"has_more":true,"next_before_id":3,"next_after_id":null}`,
		"no cursor":    `{"submissions":[{"id":10}],"limit":1,"total":9,"has_more":true,"next_before_id":null,"next_after_id":null}`,
	} {
		calls := 0
		stub(t, func(w http.ResponseWriter, _ *http.Request) {
			calls++
			_, _ = io.WriteString(w, page)
		})
		r := runCLI(t, "", "list", "--all", "--json")
		if r.code != 1 || !strings.Contains(r.stderr, "pagination is inconsistent") || calls > 2 {
			t.Fatalf("%s: %+v after %d call(s)", name, r, calls)
		}
	}
}

func TestGet(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	l.seed(t, `{"kind":"friction","key":"k1","summary":"s","context":{"cwd":"/x"},"payload":{"category":"documentation","n":1.50}}`)

	r := runCLI(t, "", "get", "1")
	for _, part := range []string{"id: 1\n", "kind: friction\n", "key: k1\n", "summary: s\n", "schema_version: 1\n",
		"context:\n  {\n    \"cwd\": \"/x\"\n  }\n", "payload:\n  {\n    \"category\": \"documentation\",\n    \"n\": 1.50\n  }\n"} {
		if r.code != 0 || !strings.Contains(r.stdout, part) {
			t.Fatalf("human missing %q: %+v", part, r)
		}
	}
	if strings.Index(r.stdout, "summary:") > strings.Index(r.stdout, "context:") {
		t.Fatalf("scalars must come first: %s", r.stdout)
	}

	_, body := l.call(t, http.MethodGet, "/api/v1/submissions/1", "")
	for _, args := range [][]string{{"get", "1", "--json"}, {"get", "--json", "1"}} {
		if r := runCLI(t, "", args...); r.code != 0 || r.stdout != string(body)+"\n" {
			t.Fatalf("%v: %+v", args, r)
		}
	}

	if r := runCLI(t, "", "get", "999"); r.code != 1 || !strings.Contains(r.stderr, "submission 999 not found") {
		t.Fatalf("404 %+v", r)
	}
	for _, args := range [][]string{{"get", "abc"}, {"get", "0"}, {"get"}, {"get", "1", "2"}} {
		if r := runCLI(t, "", args...); r.code != 2 {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

func TestStats(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	l.seed(t, `{"kind":"friction","key":"k1","project":"alpha","summary":"same","payload":{"category":"tooling"}}`)
	l.seed(t, `{"kind":"friction","key":"k2","project":"alpha","summary":"same","payload":{"category":"tooling"}}`)
	l.seed(t, `{"kind":"install-check","summary":"ok"}`)

	r := runCLI(t, "", "stats", "--by", "project", "--bucket", "day")
	for _, part := range []string{"total 2  open 2  processed 0  redacted 0\n", "project=alpha", "×2  #1..#2  friction  alpha  same", "2026-09-29"} {
		if r.code != 0 || !strings.Contains(r.stdout, part) {
			t.Fatalf("human missing %q: %+v", part, r)
		}
	}

	r = runCLI(t, "", "stats", "--json", "--by", "project")
	_, body := l.call(t, http.MethodGet, "/api/v1/stats?by=project&exclude_kind=install-check", "")
	if r.code != 0 || r.stdout != string(body)+"\n" {
		t.Fatalf("json %+v\nwant %s", r, body)
	}
}

func TestAPIErrors(t *testing.T) {
	isolate(t)
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized","message":"missing or invalid API key","request_id":"r1"}`)
	})
	if r := runCLI(t, "", "list"); r.code != 1 || !strings.Contains(r.stderr, "the server refused the API key (HTTP 401); run agentfeedback doctor.") {
		t.Fatalf("401 %+v", r)
	}

	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"validation_error","message":"limit must be between 1 and 500, got 0","request_id":"r2",`+
			`"details":[{"code":"out_of_range","pointer":"?limit","message":"limit must be between 1 and 500, got 0"}]}`)
	})
	r := runCLI(t, "", "stats")
	if r.code != 1 || !strings.Contains(r.stderr, `{"code":"out_of_range","pointer":"?limit","message":"limit must be between 1 and 500, got 0"}`+"\n") ||
		!strings.Contains(r.stderr, "the server rejected the request: limit must be between 1 and 500, got 0") {
		t.Fatalf("400 %+v", r)
	}

	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"unavailable","message":"busy","request_id":"r3"}`)
	})
	if r := runCLI(t, "", "get", "1"); r.code != 1 || !strings.Contains(r.stderr, "HTTP 503: busy (request id r3)") {
		t.Fatalf("503 %+v", r)
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	t.Setenv(envURL, srv.URL)
	if r := runCLI(t, "", "list"); r.code != 1 || !strings.Contains(r.stderr, "cannot reach the server") {
		t.Fatalf("unreachable %+v", r)
	}

	t.Setenv(envAPIKey, "")
	if r := runCLI(t, "", "list"); r.code != 1 || !strings.Contains(r.stderr, "no API key is set") {
		t.Fatalf("no key %+v", r)
	}
	t.Setenv(envURL, "")
	if r := runCLI(t, "", "export"); r.code != 0 {
		t.Fatalf("no url is local mode %+v", r)
	}
}

func TestParseInterleaved(t *testing.T) {
	fs := newFlagSet("x")
	v := fs.String("verdict", "", "")
	j := fs.Bool("json", false, "")
	pos, err := parseInterleaved(fs, []string{"1", "--verdict", "fixed", "2", "--json", "--", "-3"}, io.Discard)
	if err != nil || !reflect.DeepEqual(pos, []string{"1", "2", "-3"}) || *v != "fixed" || !*j {
		t.Fatalf("%v %v %q %v", pos, err, *v, *j)
	}
}

func TestGet_PathFromParsedID(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	l.seed(t, `{"kind":"friction","summary":"one"}`)
	for _, arg := range []string{"01", "+1"} {
		if r := runCLI(t, "", "get", arg, "--json"); r.code != 0 {
			t.Fatalf("get %s: %+v", arg, r)
		}
	}
	if r := runCLI(t, "", "redact", "+01"); r.code != 0 {
		t.Fatalf("redact: %+v", r)
	}
	var paths []string
	for _, req := range l.requests() {
		if !strings.HasPrefix(req, "POST") {
			paths = append(paths, req)
		}
	}
	want := []string{"GET /api/v1/submissions/1", "GET /api/v1/submissions/1", "DELETE /api/v1/submissions/1"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("requests %v", paths)
	}
}

func TestAPIErrors_RedirectAndTooLarge(t *testing.T) {
	isolate(t)
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://elsewhere.example/api/v1/submissions")
		w.WriteHeader(http.StatusFound)
	})
	r := runCLI(t, "", "list")
	if r.code != 1 || !strings.Contains(r.stderr, "the server redirected (HTTP 302) to https://elsewhere.example/api/v1/submissions; set AGENT_FEEDBACK_URL") {
		t.Fatalf("redirect %+v", r)
	}

	stub(t, func(w http.ResponseWriter, _ *http.Request) { writeOversized(w) })
	if r := runCLI(t, "", "list"); r.code != 1 || !strings.Contains(r.stderr, "larger than 48 MiB; pass a lower --limit.") {
		t.Fatalf("too large %+v", r)
	}
}

// writeOversized answers 200 with a body one byte over the client's limit.
func writeOversized(w http.ResponseWriter) {
	chunk := []byte(strings.Repeat(" ", 1<<20))
	for range 48 {
		_, _ = w.Write(chunk)
	}
	_, _ = w.Write([]byte(" "))
}

func TestHumanOutput_ControlCharactersNeverReachStdout(t *testing.T) {
	isolate(t)
	rec := `{"id":1,"uid":"u","kind":"friction","schema_version":1,"project":"p\u001b]0;x\u0007","summary":"a\u001b[31mb\u0085c\u007f",` +
		`"content_hash":"h","created_at":"2026-09-29T12:00:00.000000Z","payload":{"details":"d\u009bx"}}`
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/submissions":
			_, _ = io.WriteString(w, `{"submissions":[`+rec+`],"limit":50,"total":1,"has_more":false,"next_before_id":null,"next_after_id":null}`)
		case "/api/v1/stats":
			_, _ = io.WriteString(w, `{"total":1,"open":1,"processed":0,"redacted":0,"groups":[{"keys":{"project":"p\u001b"},"total":1,"open":1,"processed":0}],`+
				`"recurring":[{"content_hash":"h","count":2,"first_id":1,"last_id":1,"summary":"s\u001b","kind":"friction"}]}`)
		default:
			_, _ = io.WriteString(w, rec)
		}
	})
	for _, args := range [][]string{{"list"}, {"get", "1"}, {"stats", "--by", "project"}} {
		r := runCLI(t, "", args...)
		if r.code != 0 || strings.ContainsFunc(r.stdout, func(c rune) bool { return (c < 0x20 && c != '\n') || (c >= 0x7f && c <= 0x9f) }) ||
			!strings.Contains(r.stdout, "\uFFFD") {
			t.Fatalf("%v: %q", args, r.stdout)
		}
	}
	out := filepath.Join(t.TempDir(), "d")
	if r := runCLI(t, "", "digest", "--out", out); r.code != 0 {
		t.Fatalf("digest %+v", r)
	}
	md, err := os.ReadFile(filepath.Join(out, "digest.md"))
	if err != nil || strings.ContainsRune(string(md), 0x1b) || strings.ContainsRune(string(md), 0x85) {
		t.Fatalf("digest.md %q %v", md, err)
	}
}

func TestList_TSVHeaderOnlyAfterFirstPage(t *testing.T) {
	isolate(t)
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"internal_error","message":"internal error","request_id":"r"}`)
	})
	if r := runCLI(t, "", "list", "--tsv"); r.code != 1 || r.stdout != "" {
		t.Fatalf("%+v", r)
	}
}
