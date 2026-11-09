package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

var uiURLRe = regexp.MustCompile(`^agentfeedback ui: (http://127\.0\.0\.1:[0-9]+/[A-Za-z0-9_-]{43}/)\n$`)

// startUI runs serveUI with args until the test ends and returns the URL it
// printed.
func startUI(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := serveUI(ctx, args, pw, io.Discard)
		_ = pw.CloseWithError(io.EOF)
		done <- err
	}()
	line, err := bufio.NewReader(pr).ReadString('\n')
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serveUI: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serveUI did not stop")
		}
		closeLocal()
	})
	m := uiURLRe.FindStringSubmatch(line)
	if err != nil || m == nil {
		cancel()
		t.Fatalf("printed %q (%v): %v", line, err, <-done)
	}

	return m[1]
}

func fetch(t *testing.T, u string) string {
	t.Helper()
	res, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET %s: %d %s", u, res.StatusCode, b)
	}

	return string(b)
}

// checkUIPages fetches the queue, the record of id 1 and the stats.
func checkUIPages(t *testing.T, base string, mode string, summaries ...string) {
	t.Helper()
	queue := fetch(t, base)
	for _, want := range append([]string{mode, "submissions/1"}, summaries...) {
		if !strings.Contains(queue, want) {
			t.Errorf("queue lacks %q:\n%s", want, queue)
		}
	}
	record := fetch(t, base+"submissions/1")
	if !strings.Contains(record, summaries[0]) || !strings.Contains(record, "Warnings (recomputed from the stored record)") {
		t.Errorf("record:\n%s", record)
	}
	stats := fetch(t, base+"stats")
	if !strings.Contains(stats, "<th>total</th><td>2</td>") || !strings.Contains(stats, "By category") {
		t.Errorf("stats:\n%s", stats)
	}
	res, err := http.Get(strings.TrimSuffix(base, "/") + "x/")
	if err != nil || res.StatusCode != http.StatusNotFound {
		t.Errorf("wrong token: %v %v", res, err)
	}
	if res != nil {
		_ = res.Body.Close()
	}
}

func TestUI_Server(t *testing.T) {
	isolate(t)
	t.Setenv(envAPIKey, testKey)
	db := openDB(t, filepath.Join(t.TempDir(), "ui.db"))
	svc := core.New(db, core.Config{Version: "4.0.1", Features: api.Features})
	srv := httptest.NewServer(api.New(api.Config{Service: svc, DB: db, APIKey: testKey}).Handler())
	t.Cleanup(srv.Close)
	c, err := client.New(client.Config{URL: srv.URL, APIKey: testKey, DataDir: t.TempDir(), CacheDir: t.TempDir(), Version: "4.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"kind":"friction","summary":"server one","machine":"m","model":"x","payload":{"category":"docs"}}`,
		`{"kind":"friction","summary":"server two","machine":"m","payload":{"category":"tooling"}}`,
	} {
		if o := c.Submit(context.Background(), []byte(body)); o.Outcome != client.OutcomeSubmitted {
			t.Fatalf("submit: %+v", o)
		}
	}
	base := startUI(t, "--server", srv.URL)
	checkUIPages(t, base, "server "+strings.TrimPrefix(srv.URL, "http://"), "server one", "server two")
}

func TestUI_Local(t *testing.T) {
	isolate(t)
	localFixture(t)
	base := startUI(t, "--local")
	checkUIPages(t, base, ">local<", "eq one", "eq two")
}

func TestUI_SessionsStayLocalAndReadOnly(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "server"},
		{name: "local", args: []string{"--local"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			isolateCLI(t)
			home := os.Getenv("HOME")
			cfg := filepath.Join(home, ".claude")
			t.Setenv("CLAUDE_CONFIG_DIR", cfg)
			t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
			t.Setenv("COPILOT_HOME", filepath.Join(home, ".copilot"))
			t.Setenv("GEMINI_CLI_HOME", home)
			t.Setenv("OPENCODE_DB", filepath.Join(home, "missing-opencode.db"))
			if err := os.MkdirAll(filepath.Join(home, "project"), 0o700); err != nil {
				t.Fatal(err)
			}
			putSession(t, cfg, "local-session", filepath.Join(home, "project"))
			before := snapshot(t, home)

			var requests atomic.Int64
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				http.Error(w, "sessions must stay local", http.StatusInternalServerError)
			}))
			t.Cleanup(remote.Close)
			t.Setenv(envURL, remote.URL)
			t.Setenv(envAPIKey, testKey)

			path, err := localDBPath(os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			db := openDB(t, path)
			ctx := context.Background()
			if err := sessions.SetSelection(ctx, db, sessions.Selection{
				Harnesses: []string{sessions.HarnessClaudeCode}, Since: "17d", Limit: 23,
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Write(ctx, func(q store.Querier) error {
				_, err := q.ExecContext(ctx, `INSERT INTO sessions_seen
					(harness, session_id, path, mtime, size, head, digested_at)
					VALUES (?, ?, ?, 1, 1, 'seeded-head', 1)`,
					sessions.HarnessClaudeCode, "retired-session", filepath.Join(cfg, "projects", "retired.jsonl"))
				if err != nil {
					return err
				}
				// A successful page must not even attempt to digest, mark, or
				// change selection, including an update that preserves values.
				for _, table := range []string{"sessions_seen", "triage_state"} {
					for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
						_, err := q.ExecContext(ctx, "CREATE TRIGGER ui_readonly_"+table+"_"+action+
							" BEFORE "+action+" ON "+table+
							" BEGIN SELECT RAISE(ABORT, 'sessions page wrote local state'); END")
						if err != nil {
							return err
						}
					}
				}

				return nil
			}); err != nil {
				t.Fatal(err)
			}

			base := startUI(t, mode.args...)
			if mode.name == "server" && localTarget != nil {
				t.Fatal("sessions database opened before a sessions request")
			}
			type response struct {
				status int
				cache  string
				body   string
				err    error
			}
			const concurrent = 8
			responses := make(chan response, concurrent)
			for range concurrent {
				go func() {
					res, err := http.Get(base + "sessions")
					if err != nil {
						responses <- response{err: err}

						return
					}
					body, err := io.ReadAll(res.Body)
					_ = res.Body.Close()
					responses <- response{res.StatusCode, res.Header.Get("Cache-Control"), string(body), err}
				}()
			}
			for range concurrent {
				res := <-responses
				if res.err != nil || res.status != http.StatusOK || res.cache != "no-store" {
					t.Errorf("sessions: status=%d cache=%q err=%v body=%s", res.status, res.cache, res.err, res.body)
					continue
				}
				for _, want := range []string{
					filepath.Join(cfg, "projects"),
					"<td>new</td><td>1</td>",
					"<td>absent</td><td>1</td>",
					"<dt>Since</dt><dd>17d</dd>",
					"<dt>Limit</dt><dd>23</dd>",
				} {
					if !strings.Contains(res.body, want) {
						t.Errorf("sessions lacks %q:\n%s", want, res.body)
					}
				}
				for _, content := range []string{"The deploy is still failing", "make deploy", "Exit code 2", "local-session", "retired-session"} {
					if strings.Contains(res.body, content) {
						t.Errorf("sessions exposed transcript or session detail %q", content)
					}
				}
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("sessions made %d remote requests", got)
			}
			sameTree(t, "sessions requests", snapshot(t, home), before)
		})
	}
}

func TestUI_SessionsInitRetriesAfterFailure(t *testing.T) {
	isolateCLI(t)
	t.Setenv(envAPIKey, testKey)
	t.Setenv("OPENCODE_DB", filepath.Join(t.TempDir(), "missing.db"))
	base := startUI(t, "--server", "http://127.0.0.1:1")
	path, err := configPath(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Break configuration after startup so the lazy session opener fails,
	// rather than the UI's initial API-client configuration.
	if err := os.WriteFile(path, []byte("[invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(base + "sessions")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("invalid configuration: status %d", res.StatusCode)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fetch(t, base+"sessions")
}

func TestUI_SessionsInitializationHonorsCancellation(t *testing.T) {
	isolateCLI(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc, err := newSessionsService(ctx, os.Getenv, io.Discard)
	if err == nil || svc != nil || localTarget != nil {
		closeLocal()
		t.Fatalf("canceled initialization: service=%v error=%v", svc, err)
	}
}

func TestUI_Refusals(t *testing.T) {
	isolate(t)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"ui", "--addr", "0.0.0.0:0"}, "not a loopback address"},
		{[]string{"ui", "--addr", "example.com:0"}, "not a loopback address"},
		{[]string{"ui", "--addr", "[::]:0"}, "not a loopback address"},
		{[]string{"ui", "--addr", "127.0.0.1"}, "is not host:port"},
		{[]string{"ui", "--local", "--server", "http://127.0.0.1:1"}, "--local and --server are both set"},
		{[]string{"ui", "extra"}, "wrong number of arguments"},
	} {
		r := runCLI(t, "", tt.args...)
		if r.code != 2 || !strings.Contains(r.stderr, tt.want) || r.stdout != "" {
			t.Errorf("%v: %+v", tt.args, r)
		}
	}
}
