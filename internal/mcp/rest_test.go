package mcp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/mcp"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
)

// deterministic returns options that make two services write identical
// rows for identical calls: a fixed clock and a counting UUID generator.
func deterministic() []core.Option {
	n := 0
	return []core.Option{
		core.WithClock(func() time.Time { return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC) }),
		core.WithUIDGenerator(func() (string, error) {
			n++
			return fmt.Sprintf("01925000-0000-7000-8000-%012d", n), nil
		}),
	}
}

// TestTools_MatchREST runs the same calls against two identical services,
// one through REST and one through the tools, and compares every tool
// result with the REST body (request_id aside).
func TestTools_MatchREST(t *testing.T) {
	rest, tools := newEnv(t, api.Config{}, deterministic()...), newEnv(t, api.Config{}, deterministic()...)
	cs := tools.connect(t, "/mcp", "")
	steps := []struct {
		name    string
		method  string
		path    string
		body    string
		tool    string
		args    any
		wantErr bool
	}{
		{"create", "POST", "/api/v1/submissions", frictionBody, mcp.ToolSubmit, json.RawMessage(frictionBody), false},
		{"create again", "POST", "/api/v1/submissions", frictionBody, mcp.ToolSubmit, json.RawMessage(frictionBody), false},
		{"create inferred", "POST", "/api/v1/submissions", `{"summary":"moved","extra":{"a":1},"payload":{"category":"docs"}}`, mcp.ToolSubmit, json.RawMessage(`{"summary":"moved","extra":{"a":1},"payload":{"category":"docs"}}`), false},
		{"create rejected", "POST", "/api/v1/submissions", `{"key":"k1","summary":"a"}`, mcp.ToolSubmit, json.RawMessage(`{"key":"k1","summary":"a"}`), false},
		{"create conflict", "POST", "/api/v1/submissions", `{"key":"k1","summary":"b"}`, mcp.ToolSubmit, json.RawMessage(`{"key":"k1","summary":"b"}`), true},
		{"list", "GET", "/api/v1/submissions?kind=friction&limit=5&include=payload&exclude_kind=review&processed=false&since=2026-01-01T00:00:00Z", "", mcp.ToolList,
			map[string]any{"kind": "friction", "limit": 5, "include": "payload", "exclude_kind": []string{"review"}, "processed": false, "since": "2026-01-01T00:00:00Z"}, false},
		{"list all", "GET", "/api/v1/submissions", "", mcp.ToolList, nil, false},
		{"list bad limit", "GET", "/api/v1/submissions?limit=0", "", mcp.ToolList, map[string]any{"limit": 0}, true},
		{"get", "GET", "/api/v1/submissions/1", "", mcp.ToolGet, map[string]any{"id": 1}, false},
		{"get missing", "GET", "/api/v1/submissions/999", "", mcp.ToolGet, map[string]any{"id": 999}, true},
		{"stats", "GET", "/api/v1/stats?by=kind,project&top=5&bucket=day", "", mcp.ToolStats, map[string]any{"by": "kind,project", "top": 5, "bucket": "day"}, false},
		{"stats bad by", "GET", "/api/v1/stats?by=nope", "", mcp.ToolStats, map[string]any{"by": "nope"}, true},
		{"mark", "POST", "/api/v1/submissions/processed", `{"ids":[1,999],"processed":true,"verdict":"fixed"}`, mcp.ToolMark, json.RawMessage(`{"ids":[1,999],"processed":true,"verdict":"fixed"}`), false},
		{"mark empty", "POST", "/api/v1/submissions/processed", `{"ids":[]}`, mcp.ToolMark, json.RawMessage(`{"ids":[]}`), true},
		{"schemas", "GET", "/api/v1/schemas", "", mcp.ToolGetSchema, nil, false},
		{"schema", "GET", "/api/v1/schemas/friction/1", "", mcp.ToolGetSchema, map[string]any{"kind": "friction", "version": 1}, false},
		{"schema missing", "GET", "/api/v1/schemas/friction/9", "", mcp.ToolGetSchema, map[string]any{"kind": "friction", "version": 9}, true},
	}
	for _, s := range steps {
		var body []byte
		if s.body != "" {
			body = []byte(s.body)
		}
		r := rest.do(t, s.method, s.path, body, authed)
		got, isErr := call(t, cs, s.tool, s.args)
		if isErr {
			var body struct {
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal(got, &body); err != nil || body.RequestID == "" || body.RequestID != tools.lastRequestID() {
				t.Errorf("%s: request_id %q, X-Request-Id %q", s.name, body.RequestID, tools.lastRequestID())
			}
		}
		if isErr != s.wantErr || (r.status >= 400) != s.wantErr {
			t.Errorf("%s: tool error %v, REST %d, want error %v: %s", s.name, isErr, r.status, s.wantErr, got)
		}
		if !jsonEqual(t, got, r.body) {
			t.Errorf("%s: tool result differs from REST body\ntool: %s\nREST: %s", s.name, got, r.body)
		}
	}
}

// TestTools_ArgumentErrors checks the strict argument decoding: the detail
// code and pointer of the matching query parameter error.
func TestTools_ArgumentErrors(t *testing.T) {
	e := newEnv(t, api.Config{})
	cs := e.connect(t, "/mcp", "")
	for _, tc := range []struct {
		tool          string
		args          any
		code, pointer string
	}{
		{mcp.ToolList, map[string]any{"nope": 1}, "unknown_field", "?nope"},
		{mcp.ToolList, map[string]any{"kind": ""}, "empty", "?kind"},
		{mcp.ToolList, map[string]any{"kind": 1}, "type_mismatch", "?kind"},
		{mcp.ToolList, map[string]any{"limit": "5"}, "type_mismatch", "?limit"},
		{mcp.ToolList, map[string]any{"limit": 1.5}, "type_mismatch", "?limit"},
		{mcp.ToolList, map[string]any{"limit": -1}, "out_of_range", "?limit"},
		{mcp.ToolList, json.RawMessage(`{"limit":99999999999999999999}`), "out_of_range", "?limit"},
		{mcp.ToolList, map[string]any{"processed": "true"}, "type_mismatch", "?processed"},
		{mcp.ToolList, map[string]any{"exclude_kind": "review"}, "type_mismatch", "?exclude_kind"},
		{mcp.ToolList, map[string]any{"since": "yesterday"}, "invalid_format", "?since"},
		{mcp.ToolList, map[string]any{"include": "all"}, "out_of_range", "?include"},
		{mcp.ToolStats, map[string]any{"top": "5"}, "type_mismatch", "?top"},
		{mcp.ToolGet, nil, "required", "?id"},
		{mcp.ToolGetSchema, map[string]any{"kind": "friction"}, "required", "?version"},
		{mcp.ToolGetSchema, map[string]any{"version": 1}, "required", "?kind"},
	} {
		got, isErr := call(t, cs, tc.tool, tc.args)
		var body struct {
			Error   string
			Details []core.Detail
		}
		if err := json.Unmarshal(got, &body); err != nil || !isErr || body.Error != core.CodeValidation ||
			len(body.Details) != 1 || body.Details[0].Code != tc.code || body.Details[0].Pointer != tc.pointer {
			t.Errorf("%s %v: %s, want %s at %s", tc.tool, tc.args, got, tc.code, tc.pointer)
		}
	}
}

// TestTools_NumberSpelling checks that submit_feedback hands core the bytes
// as sent: number spellings reach content_hash as they do through REST.
func TestTools_NumberSpelling(t *testing.T) {
	body := `{"summary":"numbers","payload":{"a":1.0e2,"b":12345678901234567890123456789,"c":-0.50}}`
	rest, tools := newEnv(t, api.Config{}), newEnv(t, api.Config{})
	r := rest.do(t, "POST", "/api/v1/submissions", []byte(body), authed)
	if r.status != http.StatusCreated {
		t.Fatalf("REST create %d: %s", r.status, r.body)
	}
	got, isErr := call(t, tools.connect(t, "/mcp", ""), mcp.ToolSubmit, json.RawMessage(body))
	if isErr {
		t.Fatalf("submit: %s", got)
	}
	hash := func(b []byte) string {
		var v struct {
			Submission struct {
				ContentHash string `json:"content_hash"`
				Payload     json.RawMessage
			}
		}
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(v.Submission.Payload), "12345678901234567890123456789") {
			t.Errorf("large integer changed: %s", v.Submission.Payload)
		}
		return v.Submission.ContentHash
	}
	if a, b := hash(r.body), hash(got); a == "" || a != b {
		t.Errorf("content_hash REST %s, tool %s", a, b)
	}

	// On the wire the structured content keeps the spellings too.
	callBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_feedback","arguments":` + body + `}}`
	w := tools.do(t, "POST", "/mcp", []byte(callBody), map[string]string{"Authorization": "Bearer " + testKey,
		"Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
	if !strings.Contains(string(w.body), `"structuredContent":{"submission":`) ||
		!strings.Contains(string(w.body), `"payload":{"a":1.0e2,"b":12345678901234567890123456789,"c":-0.50}`) {
		t.Errorf("wire result: %s", w.body)
	}
}

// TestTools_BodyLimits: a large create within the request cap reaches core;
// arguments over the create limit are a tool error with core's body.
func TestTools_BodyLimits(t *testing.T) {
	e := newEnv(t, api.Config{})
	cs := e.connect(t, "/mcp", "")
	big := `{"summary":"big","payload":{"details":"` + strings.Repeat("x", 5<<20) + `"}}`
	if got, isErr := call(t, cs, mcp.ToolSubmit, json.RawMessage(big)); isErr {
		t.Fatalf("5 MiB create: %.300s", got)
	}
	over := `{"summary":"over","payload":{"details":"` + strings.Repeat("x", envelope.BodyLimit) + `"}}`
	got, isErr := call(t, cs, mcp.ToolSubmit, json.RawMessage(over))
	if !isErr || !strings.Contains(string(got), `"error":"request_too_large"`) {
		t.Fatalf("over the create limit: %.300s", got)
	}
}

func TestHTTP_Unauthorized(t *testing.T) {
	e := newEnv(t, api.Config{})
	init := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	for _, tc := range []struct {
		method, path string
		header       map[string]string
	}{
		{"POST", "/mcp", nil},
		{"POST", "/mcp", map[string]string{"Authorization": "Bearer wrong"}},
		{"POST", "/mcp", map[string]string{"X-Api-Key": "wrong"}},
		{"GET", "/mcp", nil},
		{"GET", "/mcp", map[string]string{"Authorization": "Bearer wrong"}},
		{"POST", "/mcp/p", nil},
		{"POST", "/mcp/a/b", nil},
	} {
		h := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
		for k, v := range tc.header {
			h[k] = v
		}
		r := e.do(t, tc.method, tc.path, init, h)
		assertError(t, r, http.StatusUnauthorized, "unauthorized")
		if got := r.header.Values("WWW-Authenticate"); len(got) != 1 || got[0] != "Bearer" {
			t.Errorf("%s %s: WWW-Authenticate %q", tc.method, tc.path, got)
		}
		for name, vs := range r.header {
			if strings.Contains(strings.ToLower(name+strings.Join(vs, " ")), "resource_metadata") {
				t.Errorf("%s %s: header %s: %v", tc.method, tc.path, name, vs)
			}
		}
		if strings.Contains(string(r.body), "resource_metadata") {
			t.Errorf("%s %s: body %s", tc.method, tc.path, r.body)
		}
	}
}

// assertError checks the API's Error body, its code and no-store.
func assertError(t *testing.T, r response, status int, code string) {
	t.Helper()
	if r.status != status || r.header.Get("Content-Type") != "application/json" || r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("status %d %q %q, want %d application/json no-store: %s", r.status,
			r.header.Get("Content-Type"), r.header.Get("Cache-Control"), status, r.body)
		return
	}
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil || v["error"] != code || v["message"] == "" ||
		v["request_id"] != r.header.Get("X-Request-Id") {
		t.Errorf("error body %s, want %s", r.body, code)
	}
}

// TestHTTP_TransportErrors checks the rewrite of the transport's plain-text
// errors into the Error shape and the pass-through of its JSON-RPC errors.
func TestHTTP_TransportErrors(t *testing.T) {
	e := newEnv(t, api.Config{})
	std := map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json",
		"Accept": "application/json, text/event-stream"}
	with := func(kv ...string) map[string]string {
		h := map[string]string{}
		for k, v := range std {
			h[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			h[kv[i]] = kv[i+1]
		}
		return h
	}
	list := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)

	r := e.do(t, "GET", "/mcp", nil, std)
	assertError(t, r, http.StatusMethodNotAllowed, "method_not_allowed")
	if r.header.Get("Allow") != "POST" {
		t.Errorf("405 Allow %q", r.header.Get("Allow"))
	}
	assertError(t, e.do(t, "DELETE", "/mcp/p", nil, std), http.StatusMethodNotAllowed, "method_not_allowed")
	assertError(t, e.do(t, "POST", "/mcp", list, with("Content-Type", "text/plain")), http.StatusUnsupportedMediaType, "bad_request")
	assertError(t, e.do(t, "POST", "/mcp", list, with("Accept", "application/json")), http.StatusBadRequest, "bad_request")
	assertError(t, e.do(t, "POST", "/mcp", list, with("Mcp-Protocol-Version", "1999-01-01")), http.StatusBadRequest, "bad_request")
	assertError(t, e.do(t, "POST", "/mcp", []byte(`{nope`), std), http.StatusBadRequest, "bad_request")
	r = e.do(t, "POST", "/mcp", []byte(strings.Repeat(" ", mcp.RequestLimit+1)), std)
	assertError(t, r, http.StatusRequestEntityTooLarge, "request_too_large")
	if !strings.Contains(string(r.body), strconv.Itoa(mcp.RequestLimit)) {
		t.Errorf("413 does not name the limit: %s", r.body)
	}

	// A method the server does not handle on an earlier protocol version:
	// the transport's plain-text 400, in the Error shape.
	for _, v := range []string{"", "2025-06-18"} {
		h := with()
		if v != "" {
			h["Mcp-Protocol-Version"] = v
		}
		assertError(t, e.do(t, "POST", "/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"nope/x","params":{}}`), h),
			http.StatusBadRequest, "bad_request")
	}
	assertError(t, e.do(t, "POST", "/mcp/%20", list, std), http.StatusBadRequest, "validation_error")
	assertError(t, e.do(t, "POST", "/mcp/a/b", list, std), http.StatusNotFound, "not_found")
	assertError(t, e.do(t, "POST", "/mcp/", list, std), http.StatusNotFound, "not_found")

	// A method the 2026-07-28 protocol does not know: the JSON-RPC error
	// passes through with the status the transport sets.
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	r = e.do(t, "POST", "/mcp", []byte(`{"jsonrpc":"2.0","id":7,"method":"nope/x","params":{`+meta+`}}`),
		with("Mcp-Protocol-Version", "2026-07-28", "Mcp-Method", "nope/x"))
	var rpc struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Error   struct{ Code int }
	}
	if r.status != http.StatusNotFound || r.header.Get("Content-Type") != "application/json" ||
		r.header.Get("Cache-Control") != "no-store" || json.Unmarshal(r.body, &rpc) != nil || rpc.Error.Code != -32601 || rpc.ID != 7 {
		t.Errorf("unknown method: %d %v %s", r.status, r.header, r.body)
	}

	// Success responses carry no-store too, and a stateless server issues no
	// session id.
	r = e.do(t, "POST", "/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`), std)
	if r.status != http.StatusOK || r.header.Get("Cache-Control") != "no-store" || r.header.Get("Mcp-Session-Id") != "" {
		t.Errorf("initialize: %d %v", r.status, r.header)
	}
}

const frictionBody = `{"kind":"friction","summary":"the linter ignores its config file","machine":"workstation-a","model":"claude-fable-5-1","project":"example","payload":{"category":"tooling","details":"expected .lintrc to apply; it did not","fix_status":"none"}}`

// created reads submissions_created_total by outcome.
func (e *env) created(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := e.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "submissions_created_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "outcome" {
					out[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}
	return out
}

func TestTools_SubmitCountsLikeREST(t *testing.T) {
	e := newEnv(t, api.Config{})
	cs := e.connect(t, "/mcp/p", "")
	for _, args := range []string{
		`{"summary":"counted"}`,      // created
		`{"summary":"counted"}`,      // existing
		`{"key":"k1","summary":"a"}`, // created
		`{"key":"k1","summary":"b"}`, // mismatch
	} {
		call(t, cs, mcp.ToolSubmit, json.RawMessage(args))
	}
	// Refused by the preset before core: no outcome.
	call(t, cs, mcp.ToolSubmit, json.RawMessage(`{"project":"refused before core"}`))
	got := e.created(t)
	if got["created"] != 2 || got["rejected"] != 0 || got["existing"] != 1 || got["mismatch"] != 1 {
		t.Fatalf("submissions_created_total %v", got)
	}
	// A rejected create: a body core refuses.
	call(t, e.connect(t, "/mcp", ""), mcp.ToolSubmit, json.RawMessage(`[]`))
	if got = e.created(t); got["rejected"] != 1 || got["created"] != 2 {
		t.Fatalf("after a rejected create: %v", got)
	}
}
