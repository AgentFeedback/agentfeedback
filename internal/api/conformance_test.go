package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/agentfeedback/agentfeedback/internal/core"
	"github.com/agentfeedback/agentfeedback/internal/mcp"
	"github.com/agentfeedback/agentfeedback/pkg/envelope"
)

func init() {
	openapi3filter.RegisterBodyDecoder("application/x-ndjson",
		func(r io.Reader, _ http.Header, _ *openapi3.SchemaRef, _ openapi3filter.EncodingFn) (any, error) {
			b, err := io.ReadAll(r)
			return string(b), err
		})
	for _, ct := range []string{"text/event-stream", "text/markdown"} {
		openapi3filter.RegisterBodyDecoder(ct,
			func(r io.Reader, _ http.Header, _ *openapi3.SchemaRef, _ openapi3filter.EncodingFn) (any, error) {
				b, err := io.ReadAll(r)
				return string(b), err
			})
	}
	openapi3filter.RegisterBodyDecoder("application/schema+json",
		func(r io.Reader, _ http.Header, _ *openapi3.SchemaRef, _ openapi3filter.EncodingFn) (any, error) {
			var v any
			err := json.NewDecoder(r).Decode(&v)
			return v, err
		})
}

// externalRefs lists every $ref in v that does not start with #.
func externalRefs(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if s, ok := c.(string); ok && k == "$ref" && !strings.HasPrefix(s, "#") {
				*out = append(*out, s)
			}
			externalRefs(c, out)
		}
	case []any:
		for _, c := range x {
			externalRefs(c, out)
		}
	}
}

// conformance validates every response of a scripted scenario against the
// bundled document the server serves.
type conformance struct {
	t         *testing.T
	doc       *openapi3.T
	router    routers.Router
	exercised map[string]bool
}

// call sends req and validates the response against the operation the
// contract routes it to; the response must pass openapi3filter with
// undocumented statuses rejected, and the transport headers are checked.
func (c *conformance) call(req *http.Request, wantStatus int) response {
	c.t.Helper()
	r := send(c.t, req)
	if r.status != wantStatus {
		c.t.Fatalf("%s %s = %d, want %d: %s", req.Method, req.URL.RequestURI(), r.status, wantStatus, r.body)
	}
	c.checkHeaders(req, r)
	route, params, err := c.router.FindRoute(req)
	if err != nil {
		c.t.Fatalf("%s %s: not a contract operation: %v", req.Method, req.URL.Path, err)
	}
	c.exercised[route.Operation.OperationID] = true
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 r.status,
		Header:                 r.header,
		Options:                &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	}
	input.SetBodyBytes(r.body)
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		c.t.Errorf("%s %s (%s) %d: %v\nbody: %.2000s", req.Method, req.URL.RequestURI(), route.Operation.OperationID, r.status, err, r.body)
	}
	return r
}

// callUnrouted checks a response the contract has no operation for (an
// unknown path, a wrong method) against the Error schema.
func (c *conformance) callUnrouted(req *http.Request, wantStatus int) response {
	c.t.Helper()
	r := send(c.t, req)
	if r.status != wantStatus {
		c.t.Fatalf("%s %s = %d, want %d: %s", req.Method, req.URL.RequestURI(), r.status, wantStatus, r.body)
	}
	c.checkHeaders(req, r)
	var v any
	if err := json.Unmarshal(r.body, &v); err != nil {
		c.t.Fatalf("body not JSON: %s", r.body)
	}
	if err := c.doc.Components.Schemas["Error"].Value.VisitJSON(v, openapi3.EnableJSONSchema2020()); err != nil {
		c.t.Errorf("%s %s: Error schema: %v", req.Method, req.URL.Path, err)
	}
	if wantStatus == http.StatusMethodNotAllowed && r.header.Get("Allow") == "" {
		c.t.Errorf("405 without Allow")
	}
	return r
}

var publicPaths = []string{"/api/v1/openapi.json", "/api/v1/schemas", "/api/v1/schemas/", "/skill", "/.well-known/agentfeedback.json"}

// operationalPaths answer 2xx without Cache-Control; their errors carry it.
var operationalPaths = []string{"/health", "/ready", "/metrics"}

func (c *conformance) checkHeaders(req *http.Request, r response) {
	c.t.Helper()
	if r.header.Get("X-Request-Id") == "" {
		c.t.Errorf("%s %s: no X-Request-Id", req.Method, req.URL.Path)
	}
	public := slices.ContainsFunc(publicPaths, func(p string) bool {
		return req.URL.Path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(req.URL.Path, p))
	})
	cc := r.header.Get("Cache-Control")
	if slices.Contains(operationalPaths, req.URL.Path) && r.status < 300 {
		if cc != "" {
			c.t.Errorf("%s %s: Cache-Control %q on an operational 2xx", req.Method, req.URL.Path, cc)
		}
	} else if public && r.status < 300 {
		if cc != "" {
			c.t.Errorf("%s %s: Cache-Control %q on a public 2xx", req.Method, req.URL.Path, cc)
		}
	} else if cc != "no-store" {
		c.t.Errorf("%s %s %d: Cache-Control %q, want no-store", req.Method, req.URL.Path, r.status, cc)
	}
	if r.status >= 400 && req.Method != http.MethodHead {
		var v map[string]any
		if err := json.Unmarshal(r.body, &v); err == nil && v["jsonrpc"] != nil {
			return // a JSON-RPC error of the MCP transport carries no request_id
		}
		if err := json.Unmarshal(r.body, &v); err != nil || v["request_id"] != r.header.Get("X-Request-Id") {
			c.t.Errorf("%s %s: request_id %v vs X-Request-Id %q", req.Method, req.URL.Path, v["request_id"], r.header.Get("X-Request-Id"))
		}
	}
}

func TestConformance(t *testing.T) {
	a, b := newEnv(t), newEnv(t)

	// The bundled document, fetched from the server.
	raw := a.do(t, http.MethodGet, "/api/v1/openapi.json", "", false)
	if raw.status != http.StatusOK || raw.header.Get("Content-Type") != "application/json" {
		t.Fatalf("openapi.json = %d %q", raw.status, raw.header.Get("Content-Type"))
	}
	var tree any
	if err := json.Unmarshal(raw.body, &tree); err != nil {
		t.Fatal(err)
	}
	var ext []string
	externalRefs(tree, &ext)
	if len(ext) > 0 {
		t.Fatalf("bundle keeps external refs: %v", ext)
	}
	doc, err := openapi3.NewLoader().LoadFromData(raw.body)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("bundle does not validate: %v", err)
	}
	doc.Servers = openapi3.Servers{{URL: a.srv.URL}, {URL: b.srv.URL}}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	c := &conformance{t: t, doc: doc, router: router, exercised: map[string]bool{}}
	req := func(e *testEnv, method, path, body string, auth bool) *http.Request {
		var bb []byte
		if body != "" {
			bb = []byte(body)
		}
		return e.request(t, method, path, bb, auth)
	}
	obj := func(r response) map[string]any { return r.json(t) }

	// discovery and schemas
	c.call(req(a, "GET", "/api/v1/openapi.json", "", false), 200)
	c.call(req(a, "GET", "/api/v1/schemas", "", false), 200)
	sd := c.call(req(a, "GET", "/api/v1/schemas/friction/1", "", false), 200)
	if sd.header.Get("Content-Type") != "application/schema+json" {
		t.Errorf("schema Content-Type %q", sd.header.Get("Content-Type"))
	}
	c.call(req(a, "GET", "/api/v1/schemas/envelope/1", "", false), 200)
	c.call(req(a, "GET", "/api/v1/schemas/friction/9", "", false), 404)
	c.call(req(a, "GET", "/api/v1/schemas/friction/0", "", false), 400)
	c.call(req(a, "GET", "/api/v1/meta", "", true), 200)
	c.call(req(a, "GET", "/api/v1/meta", "", false), 401)
	// no 400 documented: the query string is ignored
	c.call(req(a, "GET", "/api/v1/meta?x=1", "", true), 200)
	c.call(req(a, "GET", "/api/v1/schemas?x=1", "", false), 200)
	c.call(req(a, "GET", "/api/v1/openapi.json?x=1", "", false), 200)
	c.call(req(a, "GET", "/api/v1/schemas/envelope/1?x=1", "", false), 400)

	// create: 201, replay 200, keyless duplicate 200, 409, 400, 413
	keyed := `{"kind":"friction","key":"conf-1","summary":"s1","machine":"m","project":"p1","occurred_at":"2026-09-27T09:58:12Z","context":{"git_commit":"a1b2c3d"},"payload":{"category":"tooling","details":"d1","fix_status":"none"}}`
	first := obj(c.call(req(a, "POST", "/api/v1/submissions", keyed, true), 201))
	id1 := int64(first["submission"].(map[string]any)["id"].(float64))
	c.call(req(a, "POST", "/api/v1/submissions", keyed, true), 200)
	c.call(req(a, "POST", "/api/v1/submissions", strings.Replace(keyed, "d1", "other", 1), true), 409)
	c.call(req(a, "POST", "/api/v1/submissions", frictionBody, true), 201)
	c.call(req(a, "POST", "/api/v1/submissions", frictionBody, true), 200)
	c.call(req(a, "POST", "/api/v1/submissions", `{"kind":"review","summary":"r","payload":{"verdict":"pass"},"extra":1}`, true), 201)
	c.call(req(a, "POST", "/api/v1/submissions", `{"summary":"no kind","payload":{"x":1}}`, true), 201)
	c.call(req(a, "POST", "/api/v1/submissions", "nope", true), 400)
	c.call(req(a, "POST", "/api/v1/submissions", strings.Repeat(" ", envelope.BodyLimit+1), true), 413)
	c.call(req(a, "POST", "/api/v1/submissions", keyed, false), 401)

	// list: filters, cursors, include, 400
	list := obj(c.call(req(a, "GET", "/api/v1/submissions?limit=2", "", true), 200))
	if list["has_more"] != true {
		t.Fatalf("list has_more = %v", list["has_more"])
	}
	next := int64(list["next_before_id"].(float64))
	c.call(req(a, "GET", fmt.Sprintf("/api/v1/submissions?limit=2&before_id=%d", next), "", true), 200)
	c.call(req(a, "GET", "/api/v1/submissions?after_id=0&limit=1&include=payload", "", true), 200)
	c.call(req(a, "GET", "/api/v1/submissions?kind=friction&exclude_kind=review&exclude_kind=unknown&processed=false&redacted=false&project=p1&machine=m&key=conf-1&category=tooling&fix_status=none&schema_version=1&q=s1&since=2026-01-01T00:00:00Z&until=2030-01-01T00:00:00Z&on=created_at", "", true), 200)
	c.call(req(a, "GET", "/api/v1/submissions?limit=0", "", true), 400)
	c.call(req(a, "GET", "/api/v1/submissions?nope=1", "", true), 400)

	// get, 404, 400
	c.call(req(a, "GET", fmt.Sprintf("/api/v1/submissions/%d", id1), "", true), 200)
	c.call(req(a, "GET", "/api/v1/submissions/999999", "", true), 404)
	c.call(req(a, "GET", "/api/v1/submissions/01", "", true), 400)

	// marks
	c.call(req(a, "PATCH", fmt.Sprintf("/api/v1/submissions/%d", id1), `{"processed":true,"verdict":"fixed","resolution":"done","ref":"x@1","processed_by":"conf"}`, true), 200)
	c.call(req(a, "PATCH", "/api/v1/submissions/999999", `{"processed":true}`, true), 404)
	c.call(req(a, "PATCH", fmt.Sprintf("/api/v1/submissions/%d", id1), `{"processed":"yes"}`, true), 400)
	c.call(req(a, "PATCH", fmt.Sprintf("/api/v1/submissions/%d", id1), strings.Repeat(" ", envelope.BodyLimit+1), true), 413)
	c.call(req(a, "POST", "/api/v1/submissions/processed", fmt.Sprintf(`{"ids":[%d,2,999999],"processed":true,"verdict":"duplicate"}`, id1), true), 200)
	c.call(req(a, "POST", "/api/v1/submissions/processed", `{"ids":[]}`, true), 400)
	c.call(req(a, "POST", "/api/v1/submissions/processed", strings.Repeat(" ", envelope.BodyLimit+1), true), 413)

	// redact
	c.call(req(a, "DELETE", "/api/v1/submissions/2", "", true), 200)
	c.call(req(a, "DELETE", "/api/v1/submissions/2", "", true), 200)
	c.call(req(a, "DELETE", "/api/v1/submissions/999999", "", true), 404)

	// stats
	c.call(req(a, "GET", "/api/v1/stats", "", true), 200)
	st := obj(c.call(req(a, "GET", "/api/v1/stats?by=kind,project&top=5&bucket=day", "", true), 200))
	if _, ok := st["groups"]; !ok {
		t.Errorf("stats with by has no groups")
	}
	c.call(req(a, "GET", "/api/v1/stats?by=nope", "", true), 400)

	// export GET and HEAD
	exp := c.call(req(a, "GET", "/api/v1/export", "", true), 200)
	if exp.header.Get("Content-Type") != "application/x-ndjson" {
		t.Errorf("export Content-Type %q", exp.header.Get("Content-Type"))
	}
	lines := strings.Split(strings.TrimSuffix(string(exp.body), "\n"), "\n")
	if len(lines) < 3 || !strings.Contains(lines[len(lines)-1], `"export_complete":true`) {
		t.Fatalf("export body: %s", exp.body)
	}
	c.call(req(a, "GET", "/api/v1/export?kind=friction&after_id=0&limit=2&since=2026-01-01T00:00:00Z", "", true), 200)
	c.call(req(a, "GET", "/api/v1/export?limit=0", "", true), 400)
	head := c.call(req(a, "HEAD", "/api/v1/export?limit=5", "", true), 200)
	if head.header.Get("Content-Type") != "application/x-ndjson" || len(head.body) != 0 {
		t.Errorf("HEAD export: %q %q", head.header.Get("Content-Type"), head.body)
	}
	c.call(req(a, "HEAD", "/api/v1/export?limit=0", "", true), 400)

	// import into a fresh server, then again (everything skipped)
	imp := obj(c.call(req(b, "POST", "/api/v1/import", string(exp.body), true), 200))
	if int(imp["imported"].(float64)) != len(lines)-2 {
		t.Errorf("imported %v, want %d", imp["imported"], len(lines)-2)
	}
	again := obj(c.call(req(b, "POST", "/api/v1/import", string(exp.body), true), 200))
	if again["imported"].(float64) != 0 || int(again["skipped"].(float64)) != len(lines)-2 {
		t.Errorf("re-import = %v", again)
	}
	c.call(req(b, "POST", "/api/v1/import", "{}\n", true), 400)
	c.call(req(b, "POST", "/api/v1/import", strings.Repeat("x", core.ImportLimit+1), true), 413)

	// operational endpoints
	c.call(req(a, "GET", "/health", "", false), 200)
	c.call(req(a, "GET", "/ready", "", false), 200)
	c.call(req(a, "GET", "/metrics", "", false), 200)
	a.down.Store(true)
	c.call(req(a, "GET", "/ready", "", false), 503)
	a.down.Store(false)
	c.callUnrouted(req(a, "POST", "/health", "", false), 405)

	// discovery and guidance
	c.call(req(a, "GET", "/.well-known/agentfeedback.json", "", false), 200)
	c.call(req(a, "GET", "/.well-known/agentfeedback.json?x=1", "", false), 200)
	for _, q := range []string{"", "?format=skill-md", "?format=agents-md", "?format=prompt"} {
		sk := c.call(req(a, "GET", "/skill"+q, "", false), 200)
		if sk.header.Get("Content-Type") != "text/markdown; charset=utf-8" {
			t.Errorf("skill Content-Type %q", sk.header.Get("Content-Type"))
		}
	}
	c.call(req(a, "GET", "/skill?format=cursor", "", false), 400)
	c.call(req(a, "GET", "/skill?x=1", "", false), 400)
	c.callUnrouted(req(a, "POST", "/skill", "", false), 405)
	c.callUnrouted(req(a, "POST", "/.well-known/agentfeedback.json", "", false), 405)

	// MCP: the transport's answers and the Error shape around it
	mcpReq := func(path, body string, auth bool, header ...string) *http.Request {
		r := req(a, "POST", path, body, auth)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		for i := 0; i < len(header); i += 2 {
			r.Header.Set(header[i], header[i+1])
		}
		return r
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	for _, path := range []string{"/mcp", "/mcp/p1"} {
		c.call(mcpReq(path, initialize, true), 200)
		c.call(mcpReq(path, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, true), 202)
		c.call(mcpReq(path, `{"jsonrpc":"2.0","id":2,"method":"server/discover","params":{`+meta+`}}`, true,
			"Mcp-Protocol-Version", "2026-07-28", "Mcp-Method", "server/discover"), 200)
		c.call(mcpReq(path, `{"jsonrpc":"2.0","id":3,"method":"nope/x","params":{`+meta+`}}`, true,
			"Mcp-Protocol-Version", "2026-07-28", "Mcp-Method", "nope/x"), 404)
		c.call(mcpReq(path, `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`, true,
			"Mcp-Protocol-Version", "2099-01-01", "Mcp-Method", "tools/list"), 400)
		c.call(mcpReq(path, initialize, true, "Accept", "application/json"), 400)
		c.call(mcpReq(path, initialize, true, "Content-Type", "text/plain"), 415)
		c.call(mcpReq(path, strings.Repeat(" ", mcp.RequestLimit+1), true), 413)
		c.call(mcpReq(path, initialize, false), 401)
		c.callUnrouted(req(a, "GET", path, "", true), 405)
	}
	c.call(mcpReq("/mcp/%20", initialize, true), 400)
	c.callUnrouted(mcpReq("/mcp/a/b", initialize, true), 404)
	c.callUnrouted(mcpReq("/mcp/a/b", initialize, false), 401)

	// unrouted errors
	c.callUnrouted(req(a, "GET", "/api/v1/nope", "", true), 404)
	c.callUnrouted(req(a, "PUT", "/api/v1/meta", "", true), 405)
	c.callUnrouted(req(a, "POST", "/api/v1/schemas", "", false), 405)
	c.callUnrouted(req(a, "GET", "/api/v1/nope", "", false), 401)

	var missing []string
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			if !c.exercised[op.OperationID] {
				missing = append(missing, op.OperationID)
			}
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("operations never exercised: %v", missing)
	}
}
