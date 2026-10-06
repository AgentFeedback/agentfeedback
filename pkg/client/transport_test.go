package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// answer503 answers every request with a 503 problem body.
func answer503(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 503,
		Header:     http.Header{"Content-Type": []string{"application/problem+json"}},
		Body:       io.NopCloser(strings.NewReader(`{"title":"down","status":503,"message":"later"}`)),
		Request:    r,
	}, nil
}

func TestNewTransport(t *testing.T) {
	var got *http.Request
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r

		return answer503(r)
	})
	c, err := New(Config{APIKey: testKey, CacheDir: t.TempDir(), Transport: rt, NoSpool: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Submit(context.Background(), []byte(`{"kind":"friction"}`))
	if got == nil {
		t.Fatal("the RoundTripper saw no request")
	}
	if u := got.URL.String(); u != LocalURL+submissionsPath {
		t.Errorf("URL = %q, want %q", u, LocalURL+submissionsPath)
	}
	if got.Header.Get("Authorization") != "Bearer "+testKey || got.Header.Get("X-API-Key") != testKey {
		t.Errorf("auth headers = %q, %q", got.Header.Get("Authorization"), got.Header.Get("X-API-Key"))
	}

	if _, err := New(Config{APIKey: testKey, CacheDir: t.TempDir(), Transport: rt, HTTP: &http.Client{}}); err == nil {
		t.Error("Transport plus HTTP was accepted")
	}
	if _, err := New(Config{URL: "ftp://host", APIKey: testKey, CacheDir: t.TempDir(), Transport: rt}); err == nil {
		t.Error("Transport plus an invalid URL was accepted")
	}
}

func TestSubmitNoSpool(t *testing.T) {
	for _, noSpool := range []bool{true, false} {
		cache, stderr := t.TempDir(), &bytes.Buffer{}
		c, err := New(Config{APIKey: testKey, CacheDir: cache, Transport: roundTripFunc(answer503), NoSpool: noSpool, Stderr: stderr, Now: func() time.Time { return t0 }})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		o := c.Submit(context.Background(), []byte(`{"kind":"friction","summary":"s"}`))
		files := af1Files(t, SpoolDir(cache))
		if !noSpool {
			if o.Outcome != OutcomeSpooled || len(files) != 1 {
				t.Errorf("spooling: outcome %q, spool files %v", o.Outcome, files)
			}

			continue
		}
		if o.Outcome != OutcomeError || o.Reason != "unavailable" {
			t.Errorf("outcome %q reason %q, want error unavailable", o.Outcome, o.Reason)
		}
		if len(files) != 0 {
			t.Errorf("spool files %v, want none", files)
		}
		if !strings.Contains(stderr.String(), `"summary":"s"`) {
			t.Errorf("body not echoed; stderr = %q", stderr.String())
		}
	}
}
