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
	"regexp"
	"strings"
	"time"
	"unicode"
)

const onboardingResponseLimit = 1 << 20

// OnboardingClient exchanges onboarding credentials without installation auth,
// cookies, redirects, retries, spooling or client logging.
type OnboardingClient struct {
	baseURL string
	http    *http.Client
}

// OnboardingCapabilities describes the POST operations advertised by the server.
type OnboardingCapabilities struct {
	Email bool
	Agent bool
	Enrol bool
}

// SelfEnrolResult contains secrets for an installation with reduced attribution.
// Neither Key nor ClaimURL may be logged.
type SelfEnrolResult struct {
	Key            string
	ClaimURL       string
	ClaimExpiresAt string
}

// DeviceAuthorization contains a private polling capability. The caller owns
// polling and must wait at least Interval between requests.
type DeviceAuthorization struct {
	DeviceCode string
	VerifyURL  string
	ExpiresIn  time.Duration
	Interval   time.Duration
}

// OnboardingError reports an HTTP failure without disclosing server messages,
// bodies or request IDs. Code is empty except for recognized device-token errors.
type OnboardingError struct {
	Status int
	Code   string
}

func (e *OnboardingError) Error() string {
	return fmt.Sprintf("onboarding request failed (HTTP %d)", e.Status)
}

// NewOnboardingClient accepts the same http(s) base URLs (including path
// prefixes) as New, but never needs or sends an installation API key. A supplied
// HTTP client is copied, with its cookie jar disabled and redirects refused.
func NewOnboardingClient(baseURL string, httpClient *http.Client) (*OnboardingClient, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(base, "#") || strings.ContainsFunc(base, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return nil, errors.New("onboarding requires an http(s) server base URL without credentials, query or fragment")
	}

	var hc http.Client
	if httpClient != nil {
		hc = *httpClient
	} else {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = http.ProxyFromEnvironment
		t.DialContext = (&net.Dialer{Timeout: defaultDialLimit, KeepAlive: 30 * time.Second}).DialContext
		hc.Transport = t
	}
	if hc.Timeout <= 0 {
		hc.Timeout = defaultTimeout
	}
	hc.Jar = nil
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &OnboardingClient{baseURL: base, http: &hc}, nil
}

var onboardingOpenAPIVersion = regexp.MustCompile(`^3\.[0-9]+\.[0-9]+$`)

// Capabilities discovers hosted operations without guessing from a server name
// or following OpenAPI references. Email requires both device POST operations.
func (c *OnboardingClient) Capabilities(ctx context.Context) (OnboardingCapabilities, error) {
	var doc map[string]json.RawMessage
	if err := c.exchange(ctx, http.MethodGet, "/api/v1/openapi.json", nil, &doc); err != nil {
		return OnboardingCapabilities{}, err
	}
	var version string
	var paths map[string]json.RawMessage
	if json.Unmarshal(doc["openapi"], &version) != nil || !onboardingOpenAPIVersion.MatchString(version) || json.Unmarshal(doc["paths"], &paths) != nil || paths == nil {
		return OnboardingCapabilities{}, invalidOnboardingResponse()
	}
	post := func(path string) bool {
		var item map[string]json.RawMessage
		if json.Unmarshal(paths[path], &item) != nil {
			return false
		}
		var operation map[string]json.RawMessage
		return json.Unmarshal(item["post"], &operation) == nil && operation != nil
	}

	return OnboardingCapabilities{
		Email: post("/api/v1/auth/device") && post("/api/v1/auth/device/token"),
		Agent: post("/api/v1/self-enrol"),
		Enrol: post("/api/v1/enrol"),
	}, nil
}

// Enrol exchanges a single-use enrolment token for an installation key.
func (c *OnboardingClient) Enrol(ctx context.Context, token, machine string) (string, error) {
	var result struct {
		Key string `json:"key"`
	}
	request := struct {
		Token   string `json:"enrolment_token"`
		Machine string `json:"machine_name"`
	}{token, machine}
	if err := c.exchange(ctx, http.MethodPost, "/api/v1/enrol", request, &result); err != nil {
		return "", err
	}
	if !safeOnboardingToken(result.Key) {
		return "", invalidOnboardingResponse()
	}

	return result.Key, nil
}

// SelfEnrol creates an installation whose attribution remains reduced until its
// secret claim link is consumed. The caller controls any disclosure of that link.
func (c *OnboardingClient) SelfEnrol(ctx context.Context, machine string) (SelfEnrolResult, error) {
	var result struct {
		Key            string `json:"key"`
		ClaimURL       string `json:"claim_url"`
		ClaimExpiresAt string `json:"claim_expires_at"`
	}
	request := struct {
		Machine string `json:"machine_name"`
	}{machine}
	if err := c.exchange(ctx, http.MethodPost, "/api/v1/self-enrol", request, &result); err != nil {
		return SelfEnrolResult{}, err
	}
	_, err := time.Parse(time.RFC3339, result.ClaimExpiresAt)
	if !safeOnboardingToken(result.Key) || !safeOnboardingURL(result.ClaimURL) || err != nil {
		return SelfEnrolResult{}, invalidOnboardingResponse()
	}

	return SelfEnrolResult{Key: result.Key, ClaimURL: result.ClaimURL, ClaimExpiresAt: result.ClaimExpiresAt}, nil
}

// Device starts email approval without opening a browser or exposing its codes.
func (c *OnboardingClient) Device(ctx context.Context, email, machine string) (DeviceAuthorization, error) {
	var result struct {
		DeviceCode string `json:"device_code"`
		VerifyURL  string `json:"verify_url"`
		ExpiresIn  int64  `json:"expires_in"`
		Interval   int64  `json:"interval"`
	}
	request := struct {
		Email   string `json:"email"`
		Machine string `json:"machine_name"`
	}{email, machine}
	if err := c.exchange(ctx, http.MethodPost, "/api/v1/auth/device", request, &result); err != nil {
		return DeviceAuthorization{}, err
	}
	const maxSeconds = int64((1<<63 - 1) / time.Second)
	if !safeOnboardingToken(result.DeviceCode) || !safeOnboardingURL(result.VerifyURL) || result.ExpiresIn <= 0 || result.ExpiresIn > maxSeconds || result.Interval <= 0 || result.Interval > maxSeconds {
		return DeviceAuthorization{}, invalidOnboardingResponse()
	}

	return DeviceAuthorization{
		DeviceCode: result.DeviceCode,
		VerifyURL:  result.VerifyURL,
		ExpiresIn:  time.Duration(result.ExpiresIn) * time.Second,
		Interval:   time.Duration(result.Interval) * time.Second,
	}, nil
}

// DeviceToken polls once. Pending, slow-down, denial and expiry are returned as
// OnboardingError codes; this method never sleeps or retries.
func (c *OnboardingClient) DeviceToken(ctx context.Context, code string) (string, error) {
	var result struct {
		Key string `json:"key"`
	}
	request := struct {
		Code string `json:"device_code"`
	}{code}
	if err := c.exchange(ctx, http.MethodPost, "/api/v1/auth/device/token", request, &result); err != nil {
		return "", err
	}
	if !safeOnboardingToken(result.Key) {
		return "", invalidOnboardingResponse()
	}

	return result.Key, nil
}

func (c *OnboardingClient) exchange(ctx context.Context, method, path string, payload, result any) error {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return errors.New("cannot encode onboarding request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create onboarding request")
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("onboarding request could not reach the server")
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, onboardingResponseLimit+1))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := &OnboardingError{Status: resp.StatusCode}
		if path == "/api/v1/auth/device/token" && readErr == nil && len(raw) <= onboardingResponseLimit {
			var reply struct {
				Code string `json:"error"`
			}
			if json.Unmarshal(raw, &reply) == nil {
				switch reply.Code {
				case "authorization_pending", "slow_down", "access_denied", "expired_token":
					failure.Code = reply.Code
				}
			}
		}
		return failure
	}
	if readErr != nil || len(raw) > onboardingResponseLimit || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' || json.Unmarshal(raw, result) != nil {
		return invalidOnboardingResponse()
	}

	return nil
}

func invalidOnboardingResponse() error {
	return errors.New("the server returned an invalid onboarding response")
}

// Keys and polling codes must be single, printable ASCII tokens, not values
// that can inject headers, terminal controls or whitespace into a config file.
func safeOnboardingToken(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		if value[i] < '!' || value[i] > '~' {
			return false
		}
	}
	return true
}

func safeOnboardingURL(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.Hostname() != "" && u.User == nil
}
