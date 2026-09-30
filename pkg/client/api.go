package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// Response is one 2xx answer of Do: the status, the body exactly as sent and
// the request id from X-Request-Id.
type Response struct {
	Status    int
	Body      []byte
	RequestID string
}

// APIError is any non-2xx answer, 3xx included (redirects are never
// followed), with the fields of the Error body when it parses.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Details   []json.RawMessage
	// Location is the Location header of a 3xx, never followed.
	Location string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("HTTP %d", e.Status)
	if e.Location != "" {
		msg += " to " + e.Location
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.RequestID != "" {
		msg += " (request id " + e.RequestID + ")"
	}

	return msg
}

// TransportError is a request that got no HTTP answer: dial, reset, timeout,
// or a body cut short.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// ErrTooLarge is a response body over the 48 MiB limit of Do.
var ErrTooLarge = errors.New("the response is larger than 48 MiB")

// streamIdle is how long Stream waits for the next byte before it cancels.
var streamIdle = 60 * time.Second

// newRequest builds a request to base+path?query with both auth headers.
func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agentfeedback/"+c.version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, nil
}

// Do sends one request and returns the 2xx answer, an *APIError for any
// other status, or a *TransportError. The body is read up to 48 MiB; a
// longer one is an error.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body []byte) (*Response, error) {
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	if len(raw) > responseLimit {
		return nil, fmt.Errorf("%s %s: %w", method, path, ErrTooLarge)
	}
	rid := resp.Header.Get("X-Request-Id")
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiError(resp, rid, raw)
	}

	return &Response{Status: resp.StatusCode, Body: raw, RequestID: rid}, nil
}

// Stream sends a GET without an overall timeout (the dial limit stays) and
// returns the body for the caller to read and close. When no byte arrives
// for 60 s the request is cancelled and the read fails with a
// *TransportError. A non-2xx answer is an *APIError, its body read and
// closed.
func (c *Client) Stream(ctx context.Context, path string, query url.Values) (body io.ReadCloser, requestID string, err error) {
	ctx, cancel := context.WithCancel(ctx)
	wd := &watchdog{cancel: cancel, idle: streamIdle}
	wd.timer = time.AfterFunc(wd.idle, wd.fire)
	fail := func(err error) (io.ReadCloser, string, error) {
		wd.timer.Stop()
		cancel()

		return nil, "", err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return fail(err)
	}
	hc := *c.http
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return fail(wd.transport(err))
	}
	rid := resp.Header.Get("X-Request-Id")
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, responseLimit))
		_ = resp.Body.Close()
		_, _, err := fail(apiError(resp, rid, raw))

		return nil, rid, err
	}
	wd.body = resp.Body

	return wd, rid, nil
}

// watchdog cancels a streamed request when no byte arrives for idle.
type watchdog struct {
	body   io.ReadCloser
	cancel context.CancelFunc
	idle   time.Duration
	timer  *time.Timer
	fired  atomic.Bool
}

func (w *watchdog) fire() {
	w.fired.Store(true)
	w.cancel()
}

// transport wraps a failure, naming the stall when the watchdog caused it.
func (w *watchdog) transport(err error) error {
	if w.fired.Load() {
		err = fmt.Errorf("no data arrived for %s", w.idle)
	}

	return &TransportError{Err: err}
}

func (w *watchdog) Read(p []byte) (int, error) {
	n, err := w.body.Read(p)
	if n > 0 {
		w.timer.Reset(w.idle)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, w.transport(err)
	}

	return n, err
}

func (w *watchdog) Close() error {
	w.timer.Stop()
	err := w.body.Close()
	w.cancel()

	return err
}

// apiError reads the Error body; the header request id wins over the body's.
func apiError(resp *http.Response, rid string, raw []byte) *APIError {
	var p struct {
		Error string `json:"error"`
		problem
	}
	_ = json.Unmarshal(raw, &p)
	if rid == "" {
		rid = p.RequestID
	}

	return &APIError{
		Status: resp.StatusCode, Code: p.Error, Message: p.Message, RequestID: rid, Details: p.Details,
		Location: resp.Header.Get("Location"),
	}
}
