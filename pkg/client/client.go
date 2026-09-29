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
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

const (
	submissionsPath  = "/api/v1/submissions"
	responseLimit    = 48 << 20
	defaultTimeout   = 10 * time.Second
	defaultDialLimit = 2 * time.Second
)

// Config is what a Client needs. URL, APIKey and CacheDir are required.
type Config struct {
	// URL is the server base URL, e.g. http://host:8080 or https://h/prefix;
	// a trailing slash is trimmed.
	URL    string
	APIKey string
	// CacheDir is ${XDG_CACHE_HOME:-~/.cache}/agentfeedback, resolved by the
	// caller.
	CacheDir string
	// HTTP is optional; nil means a client with a 2 s dial and 10 s overall
	// timeout that honours the proxy environment. Redirects are never
	// followed either way; the caller's client is copied, not changed.
	HTTP *http.Client
	// Now is optional; nil means time.Now.
	Now func() time.Time
	// Stderr is optional; nil discards the notes meant for a person.
	Stderr io.Writer
	// Version is the client version sent as User-Agent agentfeedback/<v>.
	Version string
}

// Client sends submissions and flushes the spool.
type Client struct {
	endpoint string
	baseURL  string
	apiKey   string
	cacheDir string
	http     *http.Client
	now      func() time.Time
	stderr   io.Writer
	version  string
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if base == "" {
		return nil, errors.New("AGENT_FEEDBACK_URL is not set; export it or run agentfeedback doctor --init")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("the server URL does not parse (%v); set AGENT_FEEDBACK_URL to a base URL like https://host:8080", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("the server URL must start with http:// or https://; set AGENT_FEEDBACK_URL to a base URL like https://host:8080")
	}
	if u.Host == "" {
		return nil, errors.New("the server URL has no host; set AGENT_FEEDBACK_URL to a base URL like https://host:8080")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("the server URL must not carry credentials, a query or a fragment; set AGENT_FEEDBACK_URL to the bare base URL")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("AGENT_FEEDBACK_API_KEY is not set; export it or run agentfeedback doctor --init")
	}
	if strings.ContainsAny(cfg.APIKey, "\r\n") {
		return nil, errors.New("the API key contains a line break; set AGENT_FEEDBACK_API_KEY to the key alone")
	}
	if cfg.CacheDir == "" {
		return nil, errors.New("the cache directory is not set; set XDG_CACHE_HOME or HOME")
	}

	var hc http.Client
	if cfg.HTTP != nil {
		hc = *cfg.HTTP
	} else {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = http.ProxyFromEnvironment
		t.DialContext = (&net.Dialer{Timeout: defaultDialLimit, KeepAlive: 30 * time.Second}).DialContext
		hc = http.Client{Transport: t, Timeout: defaultTimeout}
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	c := &Client{
		endpoint: base + submissionsPath,
		baseURL:  base,
		apiKey:   cfg.APIKey,
		cacheDir: cfg.CacheDir,
		http:     &hc,
		now:      cfg.Now,
		stderr:   cfg.Stderr,
		version:  cfg.Version,
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.stderr == nil {
		c.stderr = io.Discard
	}

	return c, nil
}

// errBody is returned by PrepareBody for a body that is not a JSON object.
var errBody = errors.New("the submission is not one JSON object; send one JSON object")

// unknownKind is the kind the server infers when kind is absent, not a
// string, or blank after normalisation.
const unknownKind = "unknown"

// member is the last top-level occurrence of a member name: its raw value
// and the byte span of that value in the body.
type member struct {
	raw        json.RawMessage
	start, end int64
}

// topLevel walks a JSON object and returns the last occurrence of each
// member named exactly in names (case-sensitive, as the server's decoder:
// the last value wins).
func topLevel(body []byte, names ...string) (map[string]member, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errBody
	}
	out := map[string]member{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errBody
		}
		name, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, errBody
		}
		if slices.Contains(names, name) {
			end := dec.InputOffset()
			out[name] = member{raw: raw, start: end - int64(len(raw)), end: end}
		}
	}

	return out, nil
}

// PrepareBody checks that body is one JSON object and returns it ready to
// send, with the kind and key the server's answer must name.
//
// Member names are exact and the last occurrence wins, as on the server.
// kind is schema.Token of the last "kind" string, or "unknown" when it is
// absent, not a string, or blank. key is schema.Trim of the last "key"
// string. When "key" is absent a generated UUID is spliced in after the
// opening brace; when the last "key" is null or blank, that value alone is
// replaced in place with the UUID; any other type is an error. No other
// byte of the body changes.
func PrepareBody(body []byte) (prepared []byte, kind, key string, err error) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if !json.Valid(body) || len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, "", "", errBody
	}
	members, err := topLevel(body, "kind", "key")
	if err != nil {
		return nil, "", "", err
	}

	kind = unknownKind
	if m, ok := members["kind"]; ok {
		var s string
		if json.Unmarshal(m.raw, &s) == nil {
			if t := schema.Token(s); t != "" {
				kind = t
			}
		}
	}

	m, ok := members["key"]
	if ok && string(m.raw) != "null" {
		var s string
		if json.Unmarshal(m.raw, &s) != nil {
			return nil, "", "", errors.New("key must be a string when given; fix it or leave key out to have one generated")
		}
		if key = schema.Trim(s); key != "" {
			return body, kind, key, nil
		}
	}

	key = uuid.NewString()
	quoted := `"` + key + `"`
	if ok {
		out := make([]byte, 0, len(body)+len(quoted))
		out = append(out, body[:m.start]...)
		out = append(out, quoted...)
		out = append(out, body[m.end:]...)

		return out, kind, key, nil
	}
	open := len(body) - len(trimmed)
	splice := `"key":` + quoted
	rest := bytes.TrimLeft(trimmed[1:], " \t\r\n")
	if len(rest) == 0 || rest[0] != '}' {
		splice += ","
	}
	out := make([]byte, 0, len(body)+len(splice))
	out = append(out, body[:open+1]...)
	out = append(out, splice...)
	out = append(out, body[open+1:]...)

	return out, kind, key, nil
}

// Submit sends one submission once and returns its outcome. What cannot be
// delivered now is kept in the spool; what the server refuses for good goes
// to rejected/. Every outcome is appended to the client log.
func (c *Client) Submit(ctx context.Context, body []byte) Outcome {
	prepared, kind, key, err := PrepareBody(body)
	if err != nil {
		o := Outcome{Outcome: OutcomeRejected, Reason: "invalid_body", Message: err.Error()}
		c.echoBody(err.Error()+"; it was not sent", body)
		c.Log(o)

		return o
	}

	res := c.send(ctx, prepared, kind, key)
	now := c.now()
	o := Outcome{Kind: kind, Key: key, RequestID: res.requestID, Reason: res.reason}
	switch res.action {
	case actAccept:
		o.Outcome, o.ID, o.Reason = OutcomeSubmitted, res.id, ""
		if res.duplicate {
			o.Outcome = OutcomeDuplicate
		}
	case actRetry, actHold:
		e := newEntry(kind, key, prepared, now)
		e.Attempts = 1
		e.LastError = res.reason
		e.NotBefore = now
		if res.action == actRetry {
			e.NotBefore = now.Add(backoff(1, res.retryAfter))
		}
		if err := c.writeEntry(SpoolDir(c.cacheDir), newEntryName(now), e); err != nil {
			o.Outcome, o.Reason = OutcomeError, "spool_unwritable"
			o.Message = fmt.Sprintf("the spool directory %s is not writable (%v); fix its permissions and submit again", SpoolDir(c.cacheDir), err)
			c.echoBody(o.Message, prepared)
		} else {
			o.Outcome = OutcomeSpooled
		}
	case actMismatch:
		o.Outcome, o.Message = OutcomeMismatch, res.message
	case actReject:
		o.Outcome, o.Message = OutcomeRejected, res.message
		e := newEntry(kind, key, prepared, now)
		e.Attempts = 1
		e.LastError = res.reason
		if err := c.writeRejected(newEntryName(now), e, res, now); err != nil {
			o.Reason = "rejected_unwritable"
			c.echoBody(fmt.Sprintf("the rejected directory %s is not writable (%v)", RejectedDir(c.cacheDir), err), prepared)
		}
	}
	c.Log(o)

	return o
}

// echoBody writes a body that could not be persisted to stderr, after one
// line saying so, so a person can recover it.
func (c *Client) echoBody(why string, body []byte) {
	fmt.Fprintf(c.stderr, "agentfeedback: %s; the submission was NOT persisted and is echoed below for recovery\n", why)
	_, _ = c.stderr.Write(body)
	fmt.Fprintln(c.stderr)
}
