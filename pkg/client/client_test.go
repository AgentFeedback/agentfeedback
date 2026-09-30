package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestSubmitRetryTable is AC1: one subtest per row of the retry table.
func TestSubmitRetryTable(t *testing.T) {
	var redirectHits atomic.Int64
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirectHits.Add(1) }))
	defer elsewhere.Close()

	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { problemBody(w, code, "status") }
	}
	redirect := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+"/api/v1/submissions", code)
		}
	}
	answer := func(code int, mutate func(kind, key string) (string, string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			kind, key := mutate(sentHead(r))
			created(w, code, 5, kind, key)
		}
	}
	same := func(kind, key string) (string, string) { return kind, key }

	tests := []struct {
		name    string
		handler http.HandlerFunc
		closed  bool
		timeout bool
		want    string
		reason  string
		where   string // spool, rejected, none
	}{
		{name: "transport", closed: true, want: OutcomeSpooled, reason: "network", where: "spool"},
		{name: "timeout", timeout: true, want: OutcomeSpooled, reason: "timeout", where: "spool"},
		{name: "408", handler: status(408), want: OutcomeSpooled, reason: "server_408", where: "spool"},
		{name: "429", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			problemBody(w, 429, "slow down")
		}, want: OutcomeSpooled, reason: "rate_limited", where: "spool"},
		{name: "500", handler: status(500), want: OutcomeSpooled, reason: "server_500", where: "spool"},
		{name: "502", handler: status(502), want: OutcomeSpooled, reason: "server_502", where: "spool"},
		{name: "503", handler: status(503), want: OutcomeSpooled, reason: "server_503", where: "spool"},
		{name: "504", handler: status(504), want: OutcomeSpooled, reason: "server_504", where: "spool"},
		{name: "501", handler: status(501), want: OutcomeSpooled, reason: "server_501", where: "spool"},
		{name: "401", handler: status(401), want: OutcomeSpooled, reason: "unauthorized", where: "spool"},
		{name: "404", handler: status(404), want: OutcomeSpooled, reason: "wrong_url", where: "spool"},
		{name: "301", handler: redirect(301), want: OutcomeSpooled, reason: "wrong_url", where: "spool"},
		{name: "307", handler: redirect(307), want: OutcomeSpooled, reason: "wrong_url", where: "spool"},
		{name: "409", handler: status(409), want: OutcomeMismatch, reason: "mismatch", where: "none"},
		{name: "400", handler: status(400), want: OutcomeRejected, reason: "rejected", where: "rejected"},
		{name: "413", handler: status(413), want: OutcomeRejected, reason: "rejected", where: "rejected"},
		{name: "2xx unparseable", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(201)
			_, _ = w.Write([]byte("not json"))
		}, want: OutcomeSpooled, reason: "malformed_response", where: "spool"},
		{name: "2xx other key", handler: answer(201, func(kind, _ string) (string, string) { return kind, "other" }), want: OutcomeSpooled, reason: "malformed_response", where: "spool"},
		{name: "2xx other kind", handler: answer(201, func(_, key string) (string, string) { return "review", key }), want: OutcomeSpooled, reason: "malformed_response", where: "spool"},
		{name: "202", handler: answer(202, same), want: OutcomeSpooled, reason: "malformed_response", where: "spool"},
		{name: "201", handler: answer(201, same), want: OutcomeSubmitted, where: "none"},
		{name: "200", handler: answer(200, same), want: OutcomeDuplicate, where: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hc *http.Client
			handler := tt.handler
			if tt.timeout {
				hc = &http.Client{Timeout: 100 * time.Millisecond}
				handler = func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-time.After(300 * time.Millisecond):
					}
				}
			}
			if handler == nil {
				handler = status(500)
			}
			srv := httptest.NewServer(handler)
			if tt.closed {
				srv.Close()
			} else {
				defer srv.Close()
			}
			before := redirectHits.Load()
			e := newTestEnv(t, srv.URL, hc)
			o := e.c.Submit(context.Background(), []byte(`{"kind":"friction","key":"k1","summary":"x"}`))
			if o.Outcome != tt.want || o.Reason != tt.reason {
				t.Fatalf("outcome = %+v, want %s/%s; stderr %s", o, tt.want, tt.reason, e.stderr)
			}
			wantExit := 0
			if tt.want == OutcomeMismatch || tt.want == OutcomeRejected {
				wantExit = 1
			}
			if o.ExitCode() != wantExit {
				t.Errorf("exit = %d, want %d", o.ExitCode(), wantExit)
			}
			spooled, rejected := af1Files(t, SpoolDir(e.cache)), af1Files(t, RejectedDir(e.cache))
			got := "none"
			switch {
			case len(spooled) == 1 && len(rejected) == 0:
				got = "spool"
			case len(spooled) == 0 && len(rejected) == 1:
				got = "rejected"
			case len(spooled)+len(rejected) != 0:
				got = "both"
			}
			if got != tt.where {
				t.Fatalf("stored in %s, want %s", got, tt.where)
			}
			if redirectHits.Load() != before {
				t.Errorf("redirect target was hit")
			}
			if got == "spool" {
				en := readEntry(t, filepath.Join(SpoolDir(e.cache), spooled[0]))
				wantNB := t0.Add(30 * time.Second)
				switch tt.reason {
				case "rate_limited":
					wantNB = t0.Add(120 * time.Second)
				case "unauthorized", "wrong_url":
					wantNB = t0
				}
				if !en.NotBefore.Equal(wantNB) || en.Attempts != 1 || en.LastError != tt.reason {
					t.Errorf("entry = %+v, want not_before %v", en, wantNB)
				}
			}
			if got == "rejected" {
				en := readEntry(t, filepath.Join(RejectedDir(e.cache), rejected[0]))
				if en.Status != 409 && en.Status < 400 || en.RejectedAt == nil || len(en.Response) == 0 {
					t.Errorf("rejected entry = %+v", en)
				}
				if !strings.Contains(e.stderr.String(), `{"code":"c1","pointer":"/a","message":"m1"}`) {
					t.Errorf("details not relayed: %s", e.stderr)
				}
			}
		})
	}
}

// TestOutcomeLastLine is AC2: the last stdout line is the outcome.
func TestOutcomeLastLine(t *testing.T) {
	handler := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-Id", "rid-1")
			kind, key := sentHead(r)
			if code < 300 {
				created(w, code, 9, kind, key)

				return
			}
			problemBody(w, code, "no")
		}
	}
	tests := []struct {
		word   string
		code   int
		broken bool
		id     bool
	}{
		{word: OutcomeSubmitted, code: 201, id: true},
		{word: OutcomeDuplicate, code: 200, id: true},
		{word: OutcomeSpooled, code: 503},
		{word: OutcomeMismatch, code: 409},
		{word: OutcomeRejected, code: 400},
		{word: OutcomeValid},
		{word: OutcomeError, code: 503, broken: true},
	}
	for _, tt := range tests {
		t.Run(tt.word, func(t *testing.T) {
			var o Outcome
			if tt.word == OutcomeValid {
				o = Valid("friction", "k1")
			} else {
				srv := httptest.NewServer(handler(tt.code))
				defer srv.Close()
				e := newTestEnv(t, srv.URL, nil)
				if tt.broken {
					file := filepath.Join(t.TempDir(), "file")
					if err := os.WriteFile(file, nil, 0o600); err != nil {
						t.Fatal(err)
					}
					e.c.cacheDir = file
				}
				o = e.c.Submit(context.Background(), []byte(`{"kind":"friction","key":"k1"}`))
				if tt.broken && !strings.Contains(e.stderr.String(), `{"kind":"friction","key":"k1"}`) {
					t.Errorf("body not echoed: %s", e.stderr)
				}
			}
			var stdout bytes.Buffer
			stdout.WriteString("noise line\n")
			if err := o.Write(&stdout); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
			var got map[string]any
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
				t.Fatalf("last line %q: %v", lines[len(lines)-1], err)
			}
			if got["outcome"] != tt.word || got["kind"] != "friction" {
				t.Errorf("line = %v", got)
			}
			if _, ok := got["id"]; ok != tt.id {
				t.Errorf("id present = %v, want %v", ok, tt.id)
			}
			if _, ok := got["request_id"]; ok != (tt.word != OutcomeValid) {
				t.Errorf("request_id present = %v in %v", ok, got)
			}
			wantExit := map[string]int{OutcomeMismatch: 1, OutcomeRejected: 1, OutcomeError: 1}[tt.word]
			if o.ExitCode() != wantExit {
				t.Errorf("exit = %d, want %d", o.ExitCode(), wantExit)
			}
		})
	}
}

func TestDisabledOutcome(t *testing.T) {
	o := Disabled("deny_paths")
	if o.ExitCode() != 0 {
		t.Fatalf("exit = %d, want 0", o.ExitCode())
	}
	var buf bytes.Buffer
	if err := o.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if want := `{"outcome":"disabled","reason":"deny_paths"}` + "\n"; buf.String() != want {
		t.Fatalf("line = %q, want %q", buf.String(), want)
	}
}

// TestWarningsRelayedVerbatim checks the accepted response's warnings reach
// stderr byte for byte.
func TestWarningsRelayedVerbatim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, key := sentHead(r)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"submission":{"id":1,"kind":"` + kind + `","key":"` + key + `"},"warnings":[{ "code" : "c", "pointer":"/x","message":"<m>"}]}`))
	}))
	defer srv.Close()
	e := newTestEnv(t, srv.URL, nil)
	if o := e.c.Submit(context.Background(), []byte(`{"kind":"friction"}`)); o.Outcome != OutcomeSubmitted {
		t.Fatalf("%+v", o)
	}
	want := `agentfeedback: warning: { "code" : "c", "pointer":"/x","message":"<m>"}` + "\n"
	if !strings.Contains(e.stderr.String(), want) {
		t.Errorf("stderr = %q", e.stderr)
	}
}

// TestRecordedHosts is AC5: every request goes to the configured host with
// both auth headers.
func TestRecordedHosts(t *testing.T) {
	var elsewhereHits atomic.Int64
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhereHits.Add(1) }))
	defer elsewhere.Close()
	var redirecting atomic.Bool
	var badAuth atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("X-Api-Key") != testKey {
			badAuth.Add(1)
		}
		if redirecting.Load() {
			http.Redirect(w, r, elsewhere.URL, http.StatusFound)

			return
		}
		kind, key := sentHead(r)
		created(w, 201, 3, kind, key)
	}))
	defer srv.Close()

	rec := &recorder{next: http.DefaultTransport}
	e := newTestEnv(t, srv.URL, &http.Client{Transport: rec})
	redirecting.Store(true)
	e.c.Submit(context.Background(), []byte(`{"kind":"friction"}`))
	e.c.Submit(context.Background(), []byte(`{"kind":"event"}`))
	redirecting.Store(false)
	e.c.Submit(context.Background(), []byte(`{"kind":"friction"}`))
	rep := e.c.Flush(context.Background())
	if rep.Flushed != 2 {
		t.Fatalf("flush = %+v", rep)
	}
	hosts := rec.hosts()
	want := strings.TrimPrefix(srv.URL, "http://")
	if len(hosts) != 5 {
		t.Errorf("hosts = %v", hosts)
	}
	for _, h := range hosts {
		if h != want {
			t.Errorf("request to %s, want only %s", h, want)
		}
	}
	if elsewhereHits.Load() != 0 || badAuth.Load() != 0 {
		t.Errorf("elsewhere hits %d, bad auth %d", elsewhereHits.Load(), badAuth.Load())
	}
}

// recorder records the host of every request.
type recorder struct {
	next http.RoundTripper
	seen atomic.Pointer[[]string]
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	for {
		old := r.seen.Load()
		var n []string
		if old != nil {
			n = append(n, *old...)
		}
		n = append(n, req.URL.Host)
		if r.seen.CompareAndSwap(old, &n) {
			break
		}
	}

	return r.next.RoundTrip(req)
}

func (r *recorder) hosts() []string {
	if p := r.seen.Load(); p != nil {
		return *p
	}

	return nil
}

// TestNewValidates checks the configuration errors.
func TestNewValidates(t *testing.T) {
	good := Config{URL: "https://h.example/prefix/", APIKey: "k", CacheDir: "/c"}
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"no url", func(c *Config) { c.URL = "" }},
		{"ftp", func(c *Config) { c.URL = "ftp://h" }},
		{"no host", func(c *Config) { c.URL = "http://" }},
		{"userinfo", func(c *Config) { c.URL = "http://u:p@h" }},
		{"query", func(c *Config) { c.URL = "http://h/?a=1" }},
		{"fragment", func(c *Config) { c.URL = "http://h/#f" }},
		{"no key", func(c *Config) { c.APIKey = "" }},
		{"key newline", func(c *Config) { c.APIKey = "a\nb" }},
		{"no cache", func(c *Config) { c.CacheDir = "" }},
	}
	c, err := New(good)
	if err != nil || c.endpoint != "https://h.example/prefix/api/v1/submissions" {
		t.Fatalf("good config: %v %v", c, err)
	}
	for _, tt := range tests {
		cfg := good
		tt.edit(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: no error", tt.name)
		}
	}
	caller := &http.Client{}
	if _, err := New(Config{URL: "http://h", APIKey: "k", CacheDir: "/c", HTTP: caller}); err != nil || caller.CheckRedirect != nil {
		t.Errorf("caller's client changed or error %v", err)
	}
}

// TestPrepareBody checks the kind and key rules and that no other byte of
// the body changes.
func TestPrepareBody(t *testing.T) {
	const gen = "<generated>"
	tests := []struct {
		name, in string
		kind     string
		key      string // gen: a UUID; "" with ok false: rejected
		ok       bool
		want     string // expected body with the UUID written as <generated>; "" = unchanged
	}{
		{"absent key", `{"kind":"friction","n":1.0,"m":1e2}`, "friction", gen, true, `{"key":<generated>,"kind":"friction","n":1.0,"m":1e2}`},
		{"other-case names", `{"kind":"x","Key":"k","KIND":"y"}`, "x", gen, true, `{"key":<generated>,"kind":"x","Key":"k","KIND":"y"}`},
		{"last key null", `{"kind":"x","key":"a","key":null}`, "x", gen, true, `{"kind":"x","key":"a","key":<generated>}`},
		{"last key wins", `{"kind":"x","key":null,"key":"b"}`, "x", "b", true, ""},
		{"blank key", `{"kind":"x","key":"  "}`, "x", gen, true, `{"kind":"x","key":<generated>}`},
		{"spaced null", "{\"kind\":\"x\",\"key\" :  null , \"n\":1.50}", "x", gen, true, "{\"kind\":\"x\",\"key\" :  <generated> , \"n\":1.50}"},
		{"given key trimmed", `{"kind":"friction","key":" k ","n":1e2}`, "friction", "k", true, ""},
		{"leading space", " \n{\"kind\":\"friction\"}", "friction", gen, true, " \n{\"key\":<generated>,\"kind\":\"friction\"}"},
		{"empty object", `{}`, unknownKind, gen, true, `{"key":<generated>}`},
		{"missing kind", `{"key":"k"}`, unknownKind, "k", true, ""},
		{"blank kind", `{"kind":" ","key":"k"}`, unknownKind, "k", true, ""},
		{"number kind", `{"kind":5,"key":"k"}`, unknownKind, "k", true, ""},
		{"normalised kind", `{"kind":"Tool  Failure","key":"k"}`, "tool-failure", "k", true, ""},
		{"last kind wins", `{"kind":"a","kind":"b","key":"k"}`, "b", "k", true, ""},
		{"number key", `{"kind":"x","key":5}`, "", "", false, ""},
		{"object key", `{"kind":"x","key":{}}`, "", "", false, ""},
		{"array", `[{"kind":"friction"}]`, "", "", false, ""},
		{"invalid", `{"kind":`, "", "", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, kind, key, err := PrepareBody([]byte(tt.in))
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v", err)
			}
			if !tt.ok {
				return
			}
			if kind != tt.kind {
				t.Errorf("kind = %q, want %q", kind, tt.kind)
			}
			if tt.key != gen {
				if key != tt.key || string(out) != tt.in {
					t.Errorf("key %q body %s", key, out)
				}

				return
			}
			if len(key) != 36 {
				t.Fatalf("key = %q", key)
			}
			if want := strings.Replace(tt.want, gen, `"`+key+`"`, 1); string(out) != want {
				t.Errorf("body = %s, want %s", out, want)
			}
			members, err := topLevel(out, "key")
			var sent string
			if err != nil || json.Unmarshal(members["key"].raw, &sent) != nil || sent != key {
				t.Errorf("sent key member %s, returned %q", members["key"].raw, key)
			}
		})
	}
}

// TestIdentityNormalised: the server's echo of the normalised kind and the
// trimmed key is accepted; any other echo spools.
func TestIdentityNormalised(t *testing.T) {
	for _, tt := range []struct {
		name, kind, key, want string
	}{
		{"normalised", "tool-failure", "k1", OutcomeSubmitted},
		{"raw kind echoed", "Tool  Failure", "k1", OutcomeSpooled},
		{"untrimmed key echoed", "tool-failure", " k1 ", OutcomeSpooled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				created(w, 201, 1, tt.kind, tt.key)
			}))
			defer srv.Close()
			e := newTestEnv(t, srv.URL, nil)
			o := e.c.Submit(context.Background(), []byte(`{"kind":"Tool  Failure","key":" k1 "}`))
			if o.Outcome != tt.want || o.Kind != "tool-failure" || o.Key != "k1" {
				t.Fatalf("outcome = %+v", o)
			}
			if tt.want == OutcomeSpooled {
				en := readEntry(t, filepath.Join(SpoolDir(e.cache), af1Files(t, SpoolDir(e.cache))[0]))
				if en.Kind != "tool-failure" || en.Key != "k1" {
					t.Errorf("entry kind %q key %q", en.Kind, en.Key)
				}
			}
		})
	}
}

// TestLocalRejection: a body that is not one JSON object is echoed, stored
// nowhere and never sent.
func TestLocalRejection(t *testing.T) {
	var hits atomic.Int64
	e := newTestEnv(t, acceptingServer(t, &hits).URL, nil)
	body := []byte(`not json <at all>`)
	o := e.c.Submit(context.Background(), body)
	if o.Outcome != OutcomeRejected || o.ExitCode() != 1 || hits.Load() != 0 {
		t.Fatalf("outcome %+v hits %d", o, hits.Load())
	}
	if len(af1Files(t, SpoolDir(e.cache)))+len(af1Files(t, RejectedDir(e.cache))) != 0 {
		t.Error("stored somewhere")
	}
	if !bytes.Contains(e.stderr.Bytes(), body) || !strings.Contains(e.stderr.String(), "not sent") {
		t.Errorf("stderr = %s", e.stderr)
	}
}

// TestLargeResponse: an accepted answer over 1 MiB is read whole.
func TestLargeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, key := sentHead(r)
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"submission":{"id":2,"kind":%q,"key":%q,"payload":{"pad":%q}},"warnings":[]}`, kind, key, strings.Repeat("x", 3<<20))
	}))
	defer srv.Close()
	e := newTestEnv(t, srv.URL, nil)
	if o := e.c.Submit(context.Background(), []byte(`{"kind":"friction"}`)); o.Outcome != OutcomeSubmitted {
		t.Fatalf("outcome = %+v", o)
	}
}

// TestRetryAfterLong: Retry-After is honoured past the 1 h exponential cap.
func TestRetryAfterLong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7200")
		problemBody(w, 503, "later")
	}))
	defer srv.Close()
	e := newTestEnv(t, srv.URL, nil)
	e.c.Submit(context.Background(), []byte(`{"kind":"friction"}`))
	en := readEntry(t, filepath.Join(SpoolDir(e.cache), af1Files(t, SpoolDir(e.cache))[0]))
	if !en.NotBefore.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("not_before = %v", en.NotBefore)
	}
}

// TestBackoff checks the doubling, Retry-After and the cap.
func TestBackoff(t *testing.T) {
	for _, tt := range []struct {
		attempts int
		ra, want time.Duration
	}{
		{1, 0, 30 * time.Second},
		{2, 0, time.Minute},
		{3, 5 * time.Minute, 5 * time.Minute},
		{8, 0, time.Hour},
		{40, 0, time.Hour},
		{1, 3 * time.Hour, 3 * time.Hour},
		{8, 2 * time.Hour, 2 * time.Hour},
		{1, 48 * time.Hour, 24 * time.Hour},
	} {
		if got := backoff(tt.attempts, tt.ra); got != tt.want {
			t.Errorf("backoff(%d, %v) = %v, want %v", tt.attempts, tt.ra, got, tt.want)
		}
	}
	for v, want := range map[string]time.Duration{
		"120": 2 * time.Minute, "-5": 0, "junk": 0, "999999": 24 * time.Hour, "7200": 2 * time.Hour,
		t0.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		t0.Add(-time.Hour).Format(http.TimeFormat):       0,
	} {
		if got := parseRetryAfter(v, t0); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}
