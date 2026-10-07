package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/api"
	"github.com/agentfeedback/agentfeedback/v4/internal/core"
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
