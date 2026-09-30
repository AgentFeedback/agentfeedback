package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDo_HeadersQueryAndBody(t *testing.T) {
	var got *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("X-Request-Id", "rid-1")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	e := newTestEnv(t, srv.URL, nil)

	res, err := e.c.Do(context.Background(), http.MethodPost, "/api/v1/x", url.Values{"since": {"2026-01-01T00:00:00+02:00"}}, []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || string(res.Body) != `{"ok":true}` || res.RequestID != "rid-1" {
		t.Fatalf("response %+v", res)
	}
	if got.Header.Get("Authorization") != "Bearer "+testKey || got.Header.Get("X-Api-Key") != testKey ||
		got.Header.Get("User-Agent") != "agentfeedback/test" || got.Header.Get("Accept") != "application/json" ||
		got.Header.Get("Content-Type") != "application/json" || gotBody != `{"a":1}` {
		t.Fatalf("request headers %v body %q", got.Header, gotBody)
	}
	if got.URL.RawQuery != "since=2026-01-01T00%3A00%3A00%2B02%3A00" {
		t.Fatalf("query %q", got.URL.RawQuery)
	}

	if _, err := e.c.Do(context.Background(), http.MethodGet, "/api/v1/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("Content-Type") != "" || got.URL.RawQuery != "" {
		t.Fatalf("GET without body: %v %q", got.Header, got.URL.RawQuery)
	}
}

func TestDo_APIErrorAndRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)

			return
		}
		problemBody(w, http.StatusBadRequest, "limit must be between 1 and 500")
	}))
	t.Cleanup(srv.Close)
	e := newTestEnv(t, srv.URL, nil)

	_, err := e.c.Do(context.Background(), http.MethodGet, "/bad", nil, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Code != "x" || ae.Message != "limit must be between 1 and 500" ||
		ae.RequestID != "body-rid" || len(ae.Details) != 1 {
		t.Fatalf("error %#v", err)
	}
	if !strings.Contains(ae.Error(), "HTTP 400: limit") {
		t.Fatalf("message %q", ae.Error())
	}

	_, err = e.c.Do(context.Background(), http.MethodGet, "/moved", nil, nil)
	if !errors.As(err, &ae) || ae.Status != http.StatusFound || ae.Location != "/elsewhere" {
		t.Fatalf("redirect %#v", err)
	}
}

func TestDo_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	e := newTestEnv(t, base, nil)

	_, err := e.c.Do(context.Background(), http.MethodGet, "/x", nil, nil)
	var te *TransportError
	var ae *APIError
	if !errors.As(err, &te) || errors.As(err, &ae) {
		t.Fatalf("error %#v", err)
	}
}

func TestDo_BodyOverLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, responseLimit+1))
	}))
	t.Cleanup(srv.Close)
	e := newTestEnv(t, srv.URL, nil)

	if _, err := e.c.Do(context.Background(), http.MethodGet, "/x", nil, nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error %v", err)
	}
}

func TestStream_NoOverallTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			problemBody(w, http.StatusUnauthorized, "unauthorized")

			return
		}
		w.Header().Set("X-Request-Id", "rid-s")
		_, _ = io.WriteString(w, "a\n")
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, "b\n")
	}))
	t.Cleanup(srv.Close)
	e := newTestEnv(t, srv.URL, &http.Client{Timeout: 50 * time.Millisecond})

	body, rid, err := e.c.Stream(context.Background(), "/x", url.Values{"limit": {"5"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "a\nb\n" || rid != "rid-s" {
		t.Fatalf("stream %q %q %v", data, rid, err)
	}

	// The caller's client keeps its timeout.
	if _, err := e.c.Do(context.Background(), http.MethodGet, "/x", nil, nil); err == nil {
		t.Fatal("Do ignored the overall timeout")
	}

	_, _, err = e.c.Stream(context.Background(), "/bad", nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("stream error %#v", err)
	}
}

func TestStream_IdleWatchdog(t *testing.T) {
	old := streamIdle
	streamIdle = 100 * time.Millisecond
	t.Cleanup(func() { streamIdle = old })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "a\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	e := newTestEnv(t, srv.URL, nil)

	body, _, err := e.c.Stream(context.Background(), "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	var te *TransportError
	if string(data) != "a\n" || !errors.As(err, &te) || !strings.Contains(err.Error(), "no data arrived") {
		t.Fatalf("stalled stream %q %#v", data, err)
	}
}
