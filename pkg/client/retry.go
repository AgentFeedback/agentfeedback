package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	backoffBase = 30 * time.Second
	backoffCap  = time.Hour
	// retryAfterCap bounds a server's Retry-After; only the exponential
	// part is capped at backoffCap.
	retryAfterCap = 24 * time.Hour
)

// action is what the retry table says to do with one answer.
type action int

const (
	// actAccept: the server stored it (201) or already had it (200).
	actAccept action = iota
	// actRetry: keep it in the spool and retry after a backoff.
	actRetry
	// actHold: keep it in the spool with no backoff; a setting is wrong
	// (the key or the URL), so a flush pass stops here.
	actHold
	// actMismatch: the key already names a different submission (409).
	actMismatch
	// actReject: the server refuses it for good (any other 4xx).
	actReject
)

// result is one classified send.
type result struct {
	action     action
	reason     string
	transport  bool
	status     int
	duplicate  bool
	id         int64
	requestID  string
	message    string
	retryAfter time.Duration
	response   []byte
}

// createResponse is the 2xx body, kept raw where it is only compared or
// relayed.
type createResponse struct {
	Submission *struct {
		ID   json.RawMessage `json:"id"`
		Kind json.RawMessage `json:"kind"`
		Key  json.RawMessage `json:"key"`
	} `json:"submission"`
	Warnings []json.RawMessage `json:"warnings"`
}

// problem is the error body of every 4xx and 5xx.
type problem struct {
	Message   string            `json:"message"`
	RequestID string            `json:"request_id"`
	Details   []json.RawMessage `json:"details"`
}

// send POSTs body once and classifies the answer by the retry table:
//
//	transport error (dial, reset, timeout)   spool, backoff    network | timeout
//	408, 429, 500, 502, 503, 504, other 5xx  spool, backoff    server_<code> | rate_limited
//	401                                      spool, no backoff unauthorized
//	3xx, 404                                 spool, no backoff wrong_url
//	409                                      nowhere           mismatch
//	other 4xx                                rejected/         rejected
//	200/201 naming the expected kind and key delivered
//	any other 2xx, an unreadable body, or a
//	body over 48 MiB                         spool, backoff    malformed_response
//
// The expected kind and key are PrepareBody's (schema.Token and
// schema.Trim), compared exactly. Backoff is 30 s doubled per send, capped
// at 1 h, or the server's Retry-After when longer, capped at 24 h.
//
// The notes for a person (warnings, a refused key, a wrong URL, a refusal)
// go to stderr here, so Submit and Flush say the same thing.
func (c *Client) send(ctx context.Context, body []byte, kind, key string) result {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return result{action: actRetry, reason: "network", transport: true}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agentfeedback/"+c.version)

	resp, err := c.http.Do(req)
	if err != nil {
		reason := "network"
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			reason = "timeout"
		}

		return result{action: actRetry, reason: reason, transport: true}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))

	res := result{status: resp.StatusCode, requestID: resp.Header.Get("X-Request-Id"), response: raw}
	if len(raw) > responseLimit {
		res.action, res.reason, res.response = actRetry, "malformed_response", raw[:responseLimit]

		return res
	}
	code := resp.StatusCode
	if code >= 200 && code < 300 {
		return c.classifyAccepted(res, raw, kind, key)
	}

	var p problem
	_ = json.Unmarshal(raw, &p)
	if res.requestID == "" {
		res.requestID = p.RequestID
	}
	res.message = p.Message

	switch {
	case code == http.StatusUnauthorized:
		res.action, res.reason = actHold, "unauthorized"
		fmt.Fprintln(c.stderr, "agentfeedback: the server refused the API key (HTTP 401); the submission is kept and sent once the key is fixed; run agentfeedback doctor")
	case (code >= 300 && code < 400) || code == http.StatusNotFound:
		res.action, res.reason = actHold, "wrong_url"
		fmt.Fprintf(c.stderr, "agentfeedback: the base URL %s does not point at an AgentFeedback server (HTTP %d); the submission is kept and sent once the URL is fixed; run agentfeedback doctor\n", c.baseURL, code)
	case code == http.StatusTooManyRequests:
		res.action, res.reason = actRetry, "rate_limited"
		res.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
	case code == http.StatusRequestTimeout || code >= 500:
		res.action, res.reason = actRetry, "server_"+strconv.Itoa(code)
		res.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
	case code == http.StatusConflict:
		res.action, res.reason = actMismatch, "mismatch"
		fmt.Fprintf(c.stderr, "agentfeedback: key %s already names a different submission: %s; send a correction under a new key\n", key, p.Message)
	case code >= 400 && code < 500:
		res.action, res.reason = actReject, "rejected"
		fmt.Fprintf(c.stderr, "agentfeedback: the server rejected the submission (HTTP %d): %s; it is kept in %s, fix it and submit again\n", code, p.Message, RejectedDir(c.dataDir))
		for _, d := range p.Details {
			fmt.Fprintf(c.stderr, "%s\n", d)
		}
	default:
		// 1xx cannot reach here through net/http; anything else unknown is
		// treated like a malformed answer.
		res.action, res.reason = actRetry, "malformed_response"
	}

	return res
}

// classifyAccepted accepts a 2xx only when it is 200 or 201 and names a
// positive id, exactly the expected kind and exactly the expected key; then
// it relays the warnings.
func (c *Client) classifyAccepted(res result, raw []byte, kind, key string) result {
	res.action, res.reason = actRetry, "malformed_response"
	if res.status != http.StatusOK && res.status != http.StatusCreated {
		return res
	}
	var cr createResponse
	if json.Unmarshal(raw, &cr) != nil || cr.Submission == nil {
		return res
	}
	var id int64
	var gotKind, gotKey string
	if json.Unmarshal(cr.Submission.ID, &id) != nil || id < 1 ||
		json.Unmarshal(cr.Submission.Kind, &gotKind) != nil ||
		json.Unmarshal(cr.Submission.Key, &gotKey) != nil {
		return res
	}
	if gotKind != kind || gotKey != key {
		return res
	}
	res.action, res.reason, res.id = actAccept, "", id
	res.duplicate = res.status == http.StatusOK
	for _, w := range cr.Warnings {
		fmt.Fprintf(c.stderr, "agentfeedback: warning: %s\n", w)
	}

	return res
}

// backoff is how long to wait after attempts sends: 30 s doubled per send,
// at most one hour, or Retry-After when longer, at most 24 hours.
func backoff(attempts int, retryAfter time.Duration) time.Duration {
	d := backoffCap
	if attempts < 1 {
		attempts = 1
	}
	if attempts <= 8 {
		d = min(backoffBase<<(attempts-1), backoffCap)
	}

	return max(d, min(retryAfter, retryAfterCap))
}

// parseRetryAfter reads delta-seconds or an HTTP-date relative to now; a
// negative or unreadable value is zero, and the result is at most 24 hours.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		if n > int64(retryAfterCap/time.Second) {
			return retryAfterCap
		}

		return time.Duration(n) * time.Second
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}

	return min(max(t.Sub(now), 0), retryAfterCap)
}
