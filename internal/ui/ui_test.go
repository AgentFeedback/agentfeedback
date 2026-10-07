package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/localmode"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

const (
	testAddr = "127.0.0.1:4242"
	testHost = "localhost:4242"
)

var testNow = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// fixture opens a local target over a temporary database and submits:
// 1 alpha friction (documentation, origin agent, processed), 2 beta friction
// without a model whose summary is markup (tooling, origin inbox), 3 a note
// of another kind in alpha.
func fixture(t *testing.T) *client.Client {
	t.Helper()
	dir := t.TempDir()
	tg, err := localmode.Open(context.Background(), filepath.Join(dir, "data", "agentfeedback.db"), "0.0.0-test", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = tg.Close() })
	c, err := client.New(client.Config{Transport: tg.Transport(), APIKey: localmode.Key, DataDir: dir, CacheDir: t.TempDir(), Version: "0.0.0-test"})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	ctx := context.Background()
	for i, body := range []string{
		`{"kind":"friction","summary":"alpha one","machine":"m","model":"model-a","harness":"claude-code","project":"alpha","context":{"origin":"agent"},"payload":{"category":"documentation","details":"d","n":12345678901234567890}}`,
		`{"kind":"friction","summary":"<script>alert(1)</script>","machine":"m","harness":"codex","project":"beta","context":{"origin":"inbox"},"payload":{"category":"tooling","details":"d"}}`,
		`{"kind":"note","summary":"alpha note","machine":"m","model":"model-b","harness":"claude-code","project":"alpha","payload":{"text":"t"}}`,
	} {
		o := c.Submit(ctx, []byte(body))
		if o.Outcome != client.OutcomeSubmitted || o.ID != int64(i+1) {
			t.Fatalf("submit %d: %+v", i+1, o)
		}
	}
	if _, err := c.Do(ctx, http.MethodPost, "/api/v1/submissions/processed", nil, []byte(`{"ids":[1],"processed":true,"verdict":"fixed"}`)); err != nil {
		t.Fatalf("mark: %v", err)
	}

	return c
}

func newHandler(t *testing.T, src Source) (http.Handler, string) {
	t.Helper()
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Source: src, Token: token, Addr: testAddr, Mode: "local", Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return h, token
}

type req struct {
	method, target, host string
	origin               *string
}

func serve(h http.Handler, q req) *httptest.ResponseRecorder {
	if q.method == "" {
		q.method = http.MethodGet
	}
	r := httptest.NewRequest(q.method, "http://x"+q.target, nil)
	r.Host = q.host
	if r.Host == "" {
		r.Host = testAddr
	}
	if q.origin != nil {
		r.Header.Set("Origin", *q.origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

func get(t *testing.T, h http.Handler, target string) string {
	t.Helper()
	w := serve(h, req{target: target})
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, w.Code, w.Body)
	}

	return w.Body.String()
}

func TestNewToken(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if len(a) != 43 || a == b || strings.ContainsAny(a, "+/=") {
		t.Fatalf("tokens %q %q", a, b)
	}
}

func TestNew_Refuses(t *testing.T) {
	for _, cfg := range []Config{
		{Token: strings.Repeat("a", 43), Addr: testAddr},
		{Source: &fakeSource{}, Token: "short", Addr: testAddr},
		{Source: &fakeSource{}, Token: strings.Repeat("a", 43), Addr: "0.0.0.0:1"},
		{Source: &fakeSource{}, Token: strings.Repeat("a", 43), Addr: "127.0.0.1:0"},
		{Source: &fakeSource{}, Token: strings.Repeat("a", 43), Addr: "localhost:1"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) accepted", cfg)
		}
	}
}

// AC1: the token gates every page.
func TestToken(t *testing.T) {
	h, token := newHandler(t, fixture(t))
	wrong := strings.Repeat("A", len(token))
	for target, want := range map[string]int{
		"/":                       http.StatusNotFound,
		"/stats":                  http.StatusNotFound,
		"/" + wrong + "/":         http.StatusNotFound,
		"/" + token[:10] + "/":    http.StatusNotFound,
		"/" + token + "x/":        http.StatusNotFound,
		"/" + token:               http.StatusNotFound,
		"/" + token + "/":         http.StatusOK,
		"/" + token + "/stats":    http.StatusOK,
		"/" + wrong + "/stats":    http.StatusNotFound,
		"/" + token + "/nowhere":  http.StatusNotFound,
		"/" + token + "/static/x": http.StatusNotFound,
	} {
		if w := serve(h, req{target: target}); w.Code != want {
			t.Errorf("GET %s: %d, want %d", target, w.Code, want)
		}
	}
}

func checkHeaders(t *testing.T, label string, w *httptest.ResponseRecorder) {
	t.Helper()
	hd := w.Result().Header
	for name := range hd {
		if strings.HasPrefix(strings.ToLower(name), "access-control-") {
			t.Errorf("%s: header %s", label, name)
		}
	}
	csp := hd.Get("Content-Security-Policy")
	if hd.Get("Cache-Control") != "no-store" || !strings.HasPrefix(csp, "default-src 'none'") || strings.Contains(csp, "http") ||
		hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("%s: headers %v", label, hd)
	}
}

// AC2: host, origin and method checks on every route, the headers on every
// response, and nothing of the API or /mcp mounted.
func TestRequestChecks(t *testing.T) {
	h, token := newHandler(t, fixture(t))
	p := "/" + token + "/"
	routes := []struct {
		path string
		ok   bool // 200 on a valid GET
	}{
		{p, true}, {p + "submissions/1", true}, {p + "stats", true}, {p + "sessions", true},
		{p + "static/app.css", true}, {p + "static/app.js", true},
		{p + "nowhere", false}, {p + "submissions/x", false}, {p + "submissions/99", false},
		{p + "api/v1/submissions", false}, {p + "mcp", false},
		{"/api/v1/submissions", false}, {"/mcp", false}, {"/", false},
	}
	str := func(s string) *string { return &s }
	for _, rt := range routes {
		label := rt.path
		withToken := strings.HasPrefix(rt.path, p)
		w := serve(h, req{target: rt.path})
		checkHeaders(t, "GET "+label, w)
		switch {
		case rt.ok && w.Code != http.StatusOK:
			t.Errorf("GET %s: %d", label, w.Code)
		case !rt.ok && w.Code != http.StatusNotFound:
			t.Errorf("GET %s: %d, want 404", label, w.Code)
		}
		for _, host := range []string{"evil.example:4242", "127.0.0.1:4243", "localhost", "127.0.0.2:4242", ""} {
			r := httptest.NewRequest(http.MethodGet, "http://x"+rt.path, nil)
			r.Host = host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			checkHeaders(t, "host "+host+" "+label, w)
			if w.Code != http.StatusForbidden {
				t.Errorf("Host %q %s: %d", host, label, w.Code)
			}
		}
		for _, o := range []string{"http://evil.example", "null", "https://127.0.0.1:4242", "http://127.0.0.1:4243", ""} {
			w := serve(h, req{target: rt.path, origin: str(o)})
			checkHeaders(t, "origin "+o+" "+label, w)
			if w.Code != http.StatusForbidden {
				t.Errorf("Origin %q %s: %d", o, label, w.Code)
			}
		}
		for _, o := range []string{"http://" + testAddr, "http://" + testHost} {
			w := serve(h, req{target: rt.path, origin: str(o)})
			if (rt.ok && w.Code != http.StatusOK) || (!rt.ok && w.Code != http.StatusNotFound) {
				t.Errorf("Origin %q %s: %d", o, label, w.Code)
			}
		}
		if w := serve(h, req{target: rt.path, host: testHost}); (rt.ok && w.Code != http.StatusOK) || (!rt.ok && w.Code != http.StatusNotFound) {
			t.Errorf("Host localhost %s: %d", label, w.Code)
		}
		if rt.ok {
			if w := serve(h, req{method: http.MethodHead, target: rt.path}); w.Code != http.StatusOK {
				t.Errorf("HEAD %s: %d", label, w.Code)
			}
		}
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
			w := serve(h, req{method: m, target: rt.path, origin: str("http://" + testAddr)})
			checkHeaders(t, m+" "+label, w)
			want := http.StatusNotFound
			if withToken {
				want = http.StatusMethodNotAllowed
				if got := w.Header().Get("Allow"); got != "GET, HEAD" {
					t.Errorf("%s %s: Allow %q", m, label, got)
				}
			}
			if w.Code != want {
				t.Errorf("%s %s: %d, want %d", m, label, w.Code, want)
			}
		}
	}
	// A CORS preflight is refused like any other non-GET.
	r := httptest.NewRequest(http.MethodOptions, "http://x"+p, nil)
	r.Host = testAddr
	r.Header.Set("Origin", "http://evil.example")
	r.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	checkHeaders(t, "preflight", w)
	if w.Code != http.StatusForbidden {
		t.Errorf("preflight: %d", w.Code)
	}
}

func TestStaticContentTypes(t *testing.T) {
	h, token := newHandler(t, &fakeSource{})
	for name, ct := range map[string]string{"app.css": "text/css; charset=utf-8", "app.js": "text/javascript; charset=utf-8"} {
		w := serve(h, req{target: "/" + token + "/static/" + name})
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != ct || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q", name, w.Code, w.Header().Get("Content-Type"))
		}
	}
}

// rowIDs returns the ids the queue page links to, in page order.
var rowLink = regexp.MustCompile(`<td><a href="/[A-Za-z0-9_-]+/submissions/([0-9]+)">`)

func rowIDs(body string) string {
	var ids []string
	for _, m := range rowLink.FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
	}

	return strings.Join(ids, ",")
}

// AC3: the queue lists the rows and applies each filter.
func TestQueue(t *testing.T) {
	h, token := newHandler(t, fixture(t))
	base := "/" + token + "/"
	body := get(t, h, base)
	for _, want := range []string{"3 matching", "alpha one", "model-a", "claude-code", "processed: fixed", ">open<", ">agent<", ">inbox<", `<time datetime="`, "static/app.css"} {
		if !strings.Contains(body, want) {
			t.Errorf("queue lacks %q", want)
		}
	}
	if got := rowIDs(body); got != "3,2,1" {
		t.Errorf("queue rows %s", got)
	}
	for _, tt := range []struct{ query, want string }{
		{"project=alpha", "3,1"},
		{"project=beta", "2"},
		{"category=tooling", "2"},
		{"harness=codex", "2"},
		{"model=model-b", "3"},
		{"kind=note", "3"},
		{"origin=agent", "1"},
		{"origin=inbox", "2"},
		{"state=open", "3,2"},
		{"state=processed", "1"},
		{"state=all", "3,2,1"},
		{"since=1h", ""},
		{"since=all&project=alpha", "3,1"},
	} {
		t.Run(tt.query, func(t *testing.T) {
			if got := rowIDs(get(t, h, base+"?"+tt.query)); got != tt.want {
				t.Errorf("rows %q, want %q", got, tt.want)
			}
		})
	}
	for _, q := range []string{"state=nope", "since=2d", "before_id=x", "before_id=0"} {
		if w := serve(h, req{target: base + "?" + q}); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
	if got := rowIDs(get(t, h, base+"?before_id=3")); got != "2,1" {
		t.Errorf("before_id=3: %s", got)
	}
}

// The since preset is computed from the injected clock.
func TestQueue_SinceFromClock(t *testing.T) {
	src := &fakeSource{bodies: map[string]string{submissionsPath: `{"submissions":[],"total":0,"has_more":false}`}}
	h, token := newHandler(t, src)
	get(t, h, "/"+token+"/?since=24h&project=p&state=open")
	q := src.last
	if q.Get("since") != "2030-01-01T03:04:05Z" || q.Get("project") != "p" || q.Get("processed") != "false" ||
		q.Get("limit") != "50" || q.Has("include") || q.Get("exclude_kind") != "install-check" {
		t.Fatalf("query %v", q)
	}
	get(t, h, "/"+token+"/?kind=install-check")
	if src.last.Has("exclude_kind") || src.last.Get("kind") != "install-check" {
		t.Fatalf("kind query %v", src.last)
	}
}

func TestQueue_Paging(t *testing.T) {
	src := &fakeSource{bodies: map[string]string{submissionsPath: `{"submissions":[{"id":60,"created_at":"2030-01-01T00:00:00Z","kind":"friction","summary":"s"}],"total":120,"has_more":true,"next_before_id":60}`}}
	h, token := newHandler(t, src)
	body := get(t, h, "/"+token+"/?project=p")
	if !strings.Contains(body, `href="/`+token+`/?before_id=60&amp;project=p">Older`) || !strings.Contains(body, "120 matching") {
		t.Fatalf("paging link missing:\n%s", body)
	}
}

func TestRecord(t *testing.T) {
	h, token := newHandler(t, fixture(t))
	body := get(t, h, "/"+token+"/submissions/2")
	for _, want := range []string{"Warnings (recomputed from the stored record)", "missing_recommended", "/model", "model is recommended", "&#34;category&#34;: &#34;tooling&#34;", "content_hash", "<pre>"} {
		if !strings.Contains(body, want) {
			t.Errorf("record 2 lacks %q", want)
		}
	}
	body = get(t, h, "/"+token+"/submissions/1")
	if strings.Contains(body, "missing_recommended") || !strings.Contains(body, "12345678901234567890") || !strings.Contains(body, "unknown_field") {
		t.Errorf("record 1:\n%s", body)
	}
	if w := serve(h, req{target: "/" + token + "/submissions/99"}); w.Code != http.StatusNotFound {
		t.Errorf("missing id: %d", w.Code)
	}
}

func TestRecord_Redacted(t *testing.T) {
	src := &fakeSource{bodies: map[string]string{submissionsPath + "/5": `{"id":5,"kind":"friction","redacted_at":"2030-01-01T00:00:00Z","payload":{"redacted":true}}`}}
	h, token := newHandler(t, src)
	body := get(t, h, "/"+token+"/submissions/5")
	if !strings.Contains(body, "A redacted record has no warnings.") || strings.Contains(body, "missing_recommended") {
		t.Fatalf("redacted:\n%s", body)
	}
}

func TestRecord_InferredKind(t *testing.T) {
	src := &fakeSource{bodies: map[string]string{submissionsPath + "/6": `{"id":6,"kind":"unknown","summary":"s","machine":"m","model":"x","payload":{"a":1}}`}}
	h, token := newHandler(t, src)
	body := get(t, h, "/"+token+"/submissions/6")
	if !strings.Contains(body, "missing_kind") {
		t.Fatalf("an inferred kind is inferred again:\n%s", body)
	}
}

func TestStats(t *testing.T) {
	h, token := newHandler(t, fixture(t))
	body := get(t, h, "/"+token+"/stats")
	for _, want := range []string{"<th>total</th><td>3</td>", "<th>open</th><td>2</td>", "<th>processed</th><td>1</td>", "<th>redacted</th><td>0</td>",
		"By kind", "By project", "By harness", "By category", "By origin", "<td>inbox</td>", "<td>alpha</td><td>2</td>", "<td>documentation</td>", "Recurring", "service 0.0.0-test"} {
		if !strings.Contains(body, want) {
			t.Errorf("stats lacks %q", want)
		}
	}
	body = get(t, h, "/"+token+"/stats?project=beta")
	if !strings.Contains(body, "<th>total</th><td>1</td>") || !strings.Contains(body, `action="/`+token+`/stats"`) {
		t.Errorf("filtered stats:\n%s", body)
	}
}

func TestSessions(t *testing.T) {
	h, token := newHandler(t, &fakeSource{})
	if body := get(t, h, "/"+token+"/sessions"); !strings.Contains(body, "not available in this version") {
		t.Fatal(body)
	}
}

func TestEscaping(t *testing.T) {
	h, token := newHandler(t, fixture(t))
	for _, target := range []string{"/" + token + "/", "/" + token + "/submissions/2", "/" + token + "/stats?project=%3Cscript%3E"} {
		body := get(t, h, target)
		if strings.Contains(body, "<script>alert") || strings.Contains(body, `value="<script>`) {
			t.Errorf("%s renders markup unescaped", target)
		}
	}
	if body := get(t, h, "/"+token+"/"); !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("queue lacks the escaped summary")
	}
}

func TestUpstreamErrors(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want int
		msg  string
	}{
		{&client.APIError{Status: 404, Message: "no <b>such</b> submission"}, 404, "no &lt;b&gt;such&lt;/b&gt; submission"},
		{&client.APIError{Status: 400, Message: "bad origin"}, 400, "bad origin"},
		{&client.TransportError{Err: errors.New("connection refused")}, 502, "connection refused"},
		{errors.New("boom"), 500, "boom"},
	} {
		h, token := newHandler(t, &fakeSource{err: tt.err})
		for _, p := range []string{"", "submissions/1", "stats"} {
			w := serve(h, req{target: "/" + token + "/" + p})
			checkHeaders(t, p, w)
			if w.Code != tt.want || !strings.Contains(w.Body.String(), tt.msg) {
				t.Errorf("%v on %q: %d %s", tt.err, p, w.Code, w.Body)
			}
		}
	}
}

// remoteRef is a URL reference in an asset: an optional scheme and two
// slashes followed by a host character, after a quote, paren, equals sign,
// whitespace or the start of a line.
var remoteRef = regexp.MustCompile(`(?im)(^|[\s"'(=])(https?:)?//[a-z0-9]`)

func TestAssets_NoExternalURL(t *testing.T) {
	n := 0
	err := fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		n++
		if m := remoteRef.Find(b); m != nil || strings.Contains(strings.ToLower(string(b)), "http:") || strings.Contains(strings.ToLower(string(b)), "https:") {
			t.Errorf("%s references a remote URL: %q", path, m)
		}

		return nil
	})
	if err != nil || n < 8 {
		t.Fatalf("walk: %v, %d files", err, n)
	}
	if !remoteRef.MatchString(`<a href="//cdn.example/x">`) || !remoteRef.MatchString(`url(https://x)`) || remoteRef.MatchString(`/* a comment */`) {
		t.Fatal("remoteRef does not discriminate")
	}
}

// fakeSource answers from canned bodies by path, or fails with err, and
// records the last query and refuses any method but GET.
type fakeSource struct {
	bodies map[string]string
	err    error
	last   url.Values
}

func (f *fakeSource) Do(_ context.Context, method, path string, q url.Values, body []byte) (*client.Response, error) {
	if method != http.MethodGet || body != nil {
		return nil, errors.New("not a GET")
	}
	f.last = q
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.bodies[path]
	if !ok {
		return nil, &client.APIError{Status: 404, Message: "not found"}
	}
	if !json.Valid([]byte(b)) {
		return nil, errors.New("invalid fixture")
	}

	return &client.Response{Status: 200, Body: []byte(b)}, nil
}

// No response before the token is verified carries the token.
func TestRejected_NoToken(t *testing.T) {
	h, token := newHandler(t, fixture(t))
	wrong := strings.Repeat("A", len(token))
	p := "/" + token + "/"
	str := func(s string) *string { return &s }
	var cases []req
	for _, path := range []string{p, p + "stats", p + "submissions/1", p + "static/app.css"} {
		cases = append(cases, req{target: path, host: "evil.example:4242"},
			req{target: path, origin: str("http://evil.example")}, req{target: path, origin: str("null")})
	}
	cases = append(cases, req{target: "/"}, req{target: "/" + wrong + "/"}, req{method: http.MethodPost, target: "/"},
		req{method: http.MethodPost, target: "/" + wrong + "/"})
	for _, route := range []string{"", "stats", "sessions", "submissions/1", "static/app.css", "static/app.js"} {
		cases = append(cases, req{target: "/" + wrong + "/" + route})
	}
	for _, q := range cases {
		w := serve(h, q)
		label := q.method + " " + q.target + " host " + q.host
		if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", label, w.Code)
		}
		checkHeaders(t, label, w)
		if strings.Contains(w.Body.String(), token) {
			t.Errorf("%s: body carries the token: %s", label, w.Body)
		}
		for name, vs := range w.Result().Header {
			for _, v := range vs {
				if strings.Contains(v, token) {
					t.Errorf("%s: header %s carries the token", label, name)
				}
			}
		}
	}
}

func TestStatic_Head(t *testing.T) {
	h, token := newHandler(t, &fakeSource{})
	for _, name := range []string{"app.css", "app.js"} {
		w := serve(h, req{method: http.MethodHead, target: "/" + token + "/static/" + name})
		if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
			t.Errorf("HEAD %s: %d, %d body bytes", name, w.Code, w.Body.Len())
		}
	}
}

func TestRecord_IDShape(t *testing.T) {
	h, token := newHandler(t, fixture(t))
	for _, id := range []string{"007", "01", "0", "-1", "+1", "1x"} {
		if w := serve(h, req{target: "/" + token + "/submissions/" + id}); w.Code != http.StatusNotFound {
			t.Errorf("id %q: %d", id, w.Code)
		}
	}
	if w := serve(h, req{target: "/" + token + "/submissions/1"}); w.Code != http.StatusOK {
		t.Errorf("id 1: %d", w.Code)
	}
}

// The origin filter is sent as typed: it is exact and stored verbatim.
func TestQueue_OriginVerbatim(t *testing.T) {
	c := fixture(t)
	o := c.Submit(context.Background(), []byte(`{"kind":"friction","summary":"padded","machine":"m","model":"x","harness":"h","project":"gamma","context":{"origin":" agent "},"payload":{"category":"tooling","details":"d"}}`))
	if o.Outcome != client.OutcomeSubmitted || o.ID != 4 {
		t.Fatalf("submit: %+v", o)
	}
	h, token := newHandler(t, c)
	base := "/" + token + "/"
	body := get(t, h, base+"?origin=+agent+")
	if got := rowIDs(body); got != "4" {
		t.Errorf("origin \" agent \": rows %q", got)
	}
	if !strings.Contains(body, `name="origin" value=" agent "`) {
		t.Errorf("form value does not round-trip:\n%s", body)
	}
	if got := rowIDs(get(t, h, base+"?origin=agent")); got != "1" {
		t.Errorf("origin agent: rows %q", got)
	}
}
