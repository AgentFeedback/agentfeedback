package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const onboardingTestKey = "af_live_0123456789abcdefghijklmnopqrstuv"

func onboardingTestClient(t *testing.T, handler http.HandlerFunc) *OnboardingClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewOnboardingClient(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func onboardingCall(c *OnboardingClient, method string) error {
	ctx := context.Background()
	switch method {
	case "capabilities":
		_, err := c.Capabilities(ctx)
		return err
	case "enrol":
		_, err := c.Enrol(ctx, "private-enrolment-token", "machine")
		return err
	case "self":
		_, err := c.SelfEnrol(ctx, "machine")
		return err
	case "device":
		_, err := c.Device(ctx, "person@example.test", "machine")
		return err
	case "token":
		_, err := c.DeviceToken(ctx, "private-device-code")
		return err
	default:
		panic("unknown onboarding test method")
	}
}

func TestNewOnboardingClient(t *testing.T) {
	for _, base := range []string{
		"", "host", "ftp://host", "https://", "http://:8080", "https://user:private-value@host",
		"https://host?private-value", "https://host?", "https://host#private-value", "https://host#",
		"https://host/%zz", "https://host/\nprivate-value",
	} {
		t.Run(base, func(t *testing.T) {
			c, err := NewOnboardingClient(base, nil)
			if err == nil || c != nil {
				t.Fatal("invalid base URL accepted")
			}
			if strings.Contains(err.Error(), "private-value") {
				t.Fatal("base URL secret disclosed")
			}
		})
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	redirectErr := errors.New("original redirect policy")
	original := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return redirectErr
		},
	}
	c, err := NewOnboardingClient(" https://host/prefix/// ", original)
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "https://host/prefix" || c.http == original || c.http.Timeout != defaultTimeout || c.http.Jar != nil {
		t.Fatal("base normalization, client copy, bounded timeout or cookie policy incorrect")
	}
	if original.Jar != jar || original.Timeout != 0 || original.CheckRedirect(nil, nil) != redirectErr {
		t.Fatal("supplied HTTP client mutated")
	}
	if c.http.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects not refused")
	}
	original.Timeout = time.Minute
	c, err = NewOnboardingClient("http://host", original)
	if err != nil || c.http.Timeout != time.Minute {
		t.Fatal("explicit caller timeout not retained")
	}
	c, err = NewOnboardingClient("http://host", nil)
	if err != nil || c.http.Timeout != defaultTimeout {
		t.Fatal("default client has no bounded timeout")
	}
}

func TestOnboardingHTTPExchanges(t *testing.T) {
	var calls atomic.Int64
	requests := map[string]map[string]string{
		"/prefix/api/v1/enrol":             {"enrolment_token": "private-enrolment-token", "machine_name": "my machine"},
		"/prefix/api/v1/self-enrol":        {"machine_name": "my machine"},
		"/prefix/api/v1/auth/device":       {"email": "person@example.test", "machine_name": "my machine"},
		"/prefix/api/v1/auth/device/token": {"device_code": "private-device-code"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "" || r.Header.Get("Cookie") != "" {
			t.Error("onboarding sent authentication or browser session headers")
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Error("missing JSON Accept header")
		}
		if r.URL.Path == "/prefix/api/v1/openapi.json" {
			if r.Method != http.MethodGet || r.ContentLength != 0 {
				t.Error("discovery was not an empty GET")
			}
			io.WriteString(w, `{"openapi":"3.1.1","paths":{"/api/v1/enrol":{"post":{}},"/api/v1/self-enrol":{"post":{}},"/api/v1/auth/device":{"post":{}},"/api/v1/auth/device/token":{"post":{}}}}`)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("onboarding exchange was not a JSON POST")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if want, ok := requests[r.URL.Path]; !ok || !reflect.DeepEqual(body, want) {
			t.Errorf("unexpected request path or payload for %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/prefix/api/v1/enrol":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"key":%q}`, onboardingTestKey)
		case "/prefix/api/v1/self-enrol":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"key":%q,"claim_url":"https://host.test/claim/private-claim","claim_expires_at":"2026-12-08T00:00:00Z"}`, onboardingTestKey)
		case "/prefix/api/v1/auth/device":
			io.WriteString(w, `{"device_code":"private-device-code","verify_url":"https://host.test/verify","expires_in":600,"interval":7}`)
		case "/prefix/api/v1/auth/device/token":
			fmt.Fprintf(w, `{"key":%q}`, onboardingTestKey)
		}
	}))
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "private-browser-session"}})
	c, err := NewOnboardingClient(server.URL+"/prefix/", &http.Client{Jar: jar})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	caps, err := c.Capabilities(ctx)
	if err != nil || caps != (OnboardingCapabilities{Email: true, Agent: true, Enrol: true}) {
		t.Fatalf("capabilities = %+v; error = %v", caps, err)
	}
	key, err := c.Enrol(ctx, "private-enrolment-token", "my machine")
	if err != nil || key != onboardingTestKey {
		t.Fatalf("enrol result mismatch; error = %v", err)
	}
	self, err := c.SelfEnrol(ctx, "my machine")
	if err != nil || self != (SelfEnrolResult{Key: onboardingTestKey, ClaimURL: "https://host.test/claim/private-claim", ClaimExpiresAt: "2026-12-08T00:00:00Z"}) {
		t.Fatalf("self-enrol result mismatch; error = %v", err)
	}
	device, err := c.Device(ctx, "person@example.test", "my machine")
	if err != nil || device != (DeviceAuthorization{DeviceCode: "private-device-code", VerifyURL: "https://host.test/verify", ExpiresIn: 600 * time.Second, Interval: 7 * time.Second}) {
		t.Fatalf("device result mismatch; error = %v", err)
	}
	key, err = c.DeviceToken(ctx, "private-device-code")
	if err != nil || key != onboardingTestKey {
		t.Fatalf("device token result mismatch; error = %v", err)
	}
	if calls.Load() != 5 {
		t.Fatalf("got %d requests; discovery or exchange retried", calls.Load())
	}
}

func TestOnboardingDiscovery(t *testing.T) {
	cases := []struct {
		name string
		body string
		want OnboardingCapabilities
		bad  bool
	}{
		{name: "OSS", body: `{"openapi":"3.0.3","paths":{}}`},
		{name: "enrol", body: `{"openapi":"3.1.1","paths":{"/api/v1/enrol":{"post":{}}}}`, want: OnboardingCapabilities{Enrol: true}},
		{name: "agent", body: `{"openapi":"3.1.1","paths":{"/api/v1/self-enrol":{"post":{}}}}`, want: OnboardingCapabilities{Agent: true}},
		{name: "device without token", body: `{"openapi":"3.1.1","paths":{"/api/v1/auth/device":{"post":{}}}}`},
		{name: "token without device", body: `{"openapi":"3.1.1","paths":{"/api/v1/auth/device/token":{"post":{}}}}`},
		{name: "email", body: `{"openapi":"3.1.1","paths":{"/api/v1/auth/device":{"post":{}},"/api/v1/auth/device/token":{"post":{}}}}`, want: OnboardingCapabilities{Email: true}},
		{name: "GET is not POST", body: `{"openapi":"3.1.1","paths":{"/api/v1/enrol":{"get":{}}}}`},
		{name: "null operation", body: `{"openapi":"3.1.1","paths":{"/api/v1/enrol":{"post":null}}}`},
		{name: "array operation", body: `{"openapi":"3.1.1","paths":{"/api/v1/enrol":{"post":[]}}}`},
		{name: "string operation", body: `{"openapi":"3.1.1","paths":{"/api/v1/enrol":{"post":"private-payload"}}}`},
		{name: "null path", body: `{"openapi":"3.1.1","paths":{"/api/v1/enrol":null}}`},
		{name: "reference not followed", body: `{"openapi":"3.1.1","paths":{"/api/v1/enrol":{"$ref":"https://host.test/private-ref"}}}`},
		{name: "missing version", body: `{"paths":{}}`, bad: true},
		{name: "version case mismatch", body: `{"OPENAPI":"3.1.1","paths":{}}`, bad: true},
		{name: "paths case mismatch", body: `{"openapi":"3.1.1","PATHS":{}}`, bad: true},
		{name: "swagger 2", body: `{"openapi":"2.0.0","paths":{}}`, bad: true},
		{name: "malformed version", body: `{"openapi":"3.private-value","paths":{}}`, bad: true},
		{name: "missing paths", body: `{"openapi":"3.1.1"}`, bad: true},
		{name: "null paths", body: `{"openapi":"3.1.1","paths":null}`, bad: true},
		{name: "array paths", body: `{"openapi":"3.1.1","paths":[]}`, bad: true},
		{name: "array root", body: `[]`, bad: true},
		{name: "null root", body: `null`, bad: true},
		{name: "trailing JSON", body: `{"openapi":"3.1.1","paths":{}} {"private-payload":true}`, bad: true},
		{name: "malformed JSON", body: `{"private-payload":`, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := onboardingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			})
			got, err := c.Capabilities(context.Background())
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("capabilities = %+v, error = %v; want %+v, error = %t", got, err, tc.want, tc.bad)
			}
			if err != nil && strings.Contains(err.Error(), "private-") {
				t.Fatal("malformed discovery disclosed payload")
			}
		})
	}
}

func TestOnboardingRequiredPayloads(t *testing.T) {
	cases := []struct {
		name   string
		method string
		body   string
	}{
		{"missing enrol key", "enrol", `{}`},
		{"empty enrol key", "enrol", `{"key":""}`},
		{"null enrol key", "enrol", `{"key":null}`},
		{"numeric enrol key", "enrol", `{"key":42}`},
		{"whitespace key", "enrol", `{"key":" private-key "}`},
		{"newline key", "enrol", `{"key":"private-key\n"}`},
		{"terminal key", "enrol", `{"key":"private-key\u001b"}`},
		{"unicode key", "enrol", `{"key":"private-key\u200b"}`},
		{"missing token key", "token", `{}`},
		{"unsafe token key", "token", `{"key":"private-key\r"}`},
		{"missing self key", "self", `{"claim_url":"https://host.test/claim/private-claim","claim_expires_at":"2026-12-08T00:00:00Z"}`},
		{"missing claim", "self", `{"key":"private-key","claim_expires_at":"2026-12-08T00:00:00Z"}`},
		{"relative claim", "self", `{"key":"private-key","claim_url":"/claim/private-claim","claim_expires_at":"2026-12-08T00:00:00Z"}`},
		{"credential claim", "self", `{"key":"private-key","claim_url":"https://user:private-value@host.test/claim","claim_expires_at":"2026-12-08T00:00:00Z"}`},
		{"terminal claim", "self", `{"key":"private-key","claim_url":"https://host.test/claim/private-value\u001b","claim_expires_at":"2026-12-08T00:00:00Z"}`},
		{"unicode control claim", "self", `{"key":"private-key","claim_url":"https://host.test/claim/private-value\u0085","claim_expires_at":"2026-12-08T00:00:00Z"}`},
		{"unicode line break claim", "self", `{"key":"private-key","claim_url":"https://host.test/claim/private-value\u2028","claim_expires_at":"2026-12-08T00:00:00Z"}`},
		{"non HTTP claim", "self", `{"key":"private-key","claim_url":"javascript:private-value","claim_expires_at":"2026-12-08T00:00:00Z"}`},
		{"missing deadline", "self", `{"key":"private-key","claim_url":"https://host.test/claim/private-claim"}`},
		{"invalid deadline", "self", `{"key":"private-key","claim_url":"https://host.test/claim/private-claim","claim_expires_at":"private-deadline"}`},
		{"missing device code", "device", `{"verify_url":"https://host.test/verify","expires_in":600,"interval":5}`},
		{"unsafe device code", "device", `{"device_code":"private-code\n","verify_url":"https://host.test/verify","expires_in":600,"interval":5}`},
		{"missing verify URL", "device", `{"device_code":"private-code","expires_in":600,"interval":5}`},
		{"credential verify URL", "device", `{"device_code":"private-code","verify_url":"http://user:private-value@host.test/verify","expires_in":600,"interval":5}`},
		{"non HTTP verify URL", "device", `{"device_code":"private-code","verify_url":"file:///private-value","expires_in":600,"interval":5}`},
		{"terminal verify URL", "device", `{"device_code":"private-code","verify_url":"https://host.test/private-value\u001b","expires_in":600,"interval":5}`},
		{"unicode control verify URL", "device", `{"device_code":"private-code","verify_url":"https://host.test/private-value\u0085","expires_in":600,"interval":5}`},
		{"zero expiry", "device", `{"device_code":"private-code","verify_url":"https://host.test/verify","expires_in":0,"interval":5}`},
		{"negative expiry", "device", `{"device_code":"private-code","verify_url":"https://host.test/verify","expires_in":-1,"interval":5}`},
		{"missing interval", "device", `{"device_code":"private-code","verify_url":"https://host.test/verify","expires_in":600}`},
		{"negative interval", "device", `{"device_code":"private-code","verify_url":"https://host.test/verify","expires_in":600,"interval":-1}`},
		{"fractional interval", "device", `{"device_code":"private-code","verify_url":"https://host.test/verify","expires_in":600,"interval":0.5}`},
		{"string interval", "device", `{"device_code":"private-code","verify_url":"https://host.test/verify","expires_in":600,"interval":"5"}`},
		{"expiry duration overflow", "device", `{"device_code":"private-code","verify_url":"https://host.test/verify","expires_in":9223372037,"interval":5}`},
		{"interval duration overflow", "device", `{"device_code":"private-code","verify_url":"https://host.test/verify","expires_in":600,"interval":9223372037}`},
		{"integer overflow", "device", `{"device_code":"private-code","verify_url":"https://host.test/verify","expires_in":9223372036854775808,"interval":5}`},
		{"trailing response", "token", `{"key":"private-key"} {"extra":"private-value"}`},
		{"null response", "token", `null`},
		{"array response", "token", `[{"key":"private-key"}]`},
		{"empty response", "token", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := onboardingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			})
			err := onboardingCall(c, tc.method)
			if err == nil {
				t.Fatal("invalid success payload accepted")
			}
			if strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), onboardingTestKey) {
				t.Fatal("invalid success payload disclosed")
			}
		})
	}
}

func TestOnboardingDeviceTimingBoundary(t *testing.T) {
	const maxSeconds = int64((1<<63 - 1) / time.Second)
	c := onboardingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"device_code":"private-code","verify_url":"http://host.test/verify","expires_in":%d,"interval":%d}`, maxSeconds, maxSeconds)
	})
	result, err := c.Device(context.Background(), "person@example.test", "machine")
	if err != nil || result.ExpiresIn != time.Duration(maxSeconds)*time.Second || result.Interval != time.Duration(maxSeconds)*time.Second {
		t.Fatalf("largest safe timing rejected or changed: %v", err)
	}
}

func TestOnboardingErrorRedaction(t *testing.T) {
	for _, method := range []string{"capabilities", "enrol", "self", "device", "token"} {
		for _, code := range []string{"authorization_pending", "slow_down", "access_denied", "expired_token", "private-arbitrary-error"} {
			t.Run(method+"/"+code, func(t *testing.T) {
				var calls atomic.Int64
				c := onboardingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("X-Request-Id", "private-header-request-id")
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprintf(w, `{"error":%q,"message":"private-message private-key private-claim private-code","request_id":"private-body-request-id"}`, code)
				})
				err := onboardingCall(c, method)
				var failure *OnboardingError
				if !errors.As(err, &failure) || failure.Status != http.StatusBadRequest {
					t.Fatalf("missing typed HTTP error: %v", err)
				}
				wantCode := ""
				if method == "token" && code != "private-arbitrary-error" {
					wantCode = code
				}
				if failure.Code != wantCode || strings.Contains(fmt.Sprintf("%+v", err), "private-") {
					t.Fatal("server-controlled error text or unrecognized code disclosed")
				}
				if calls.Load() != 1 {
					t.Fatal("HTTP error caused a retry")
				}
			})
		}
	}
}

func TestOnboardingRedirectSafety(t *testing.T) {
	var followed atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, method := range []string{"capabilities", "enrol", "self", "device", "token"} {
			t.Run(fmt.Sprintf("%d/%s", status, method), func(t *testing.T) {
				c := onboardingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", target.URL+"/private-redirect")
					w.WriteHeader(status)
					io.WriteString(w, "private-redirect-body")
				})
				err := onboardingCall(c, method)
				var failure *OnboardingError
				if !errors.As(err, &failure) || failure.Status != status || failure.Code != "" {
					t.Fatalf("redirect not returned as a redacted HTTP failure: %v", err)
				}
				if strings.Contains(err.Error(), "private-") {
					t.Fatal("redirect target or body disclosed")
				}
			})
		}
	}
	if followed.Load() != 0 {
		t.Fatal("onboarding followed a redirect")
	}
}

func TestOnboardingResponseReadSafety(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		large  bool
	}{
		{"oversized success", http.StatusOK, true},
		{"oversized error", http.StatusBadRequest, true},
		{"truncated success", http.StatusOK, false},
		{"truncated error", http.StatusBadRequest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := onboardingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if !tc.large {
					w.Header().Set("Content-Length", "1000")
				}
				w.WriteHeader(tc.status)
				if tc.large {
					io.WriteString(w, `{"error":"authorization_pending","private-payload":"`+strings.Repeat("x", onboardingResponseLimit)+`"}`)
				} else {
					io.WriteString(w, `{"error":"authorization_pending","key":"private-key"}`)
				}
			})
			err := onboardingCall(c, "token")
			if err == nil || strings.Contains(err.Error(), "private-") {
				t.Fatal("oversized or truncated response accepted or disclosed")
			}
			if tc.status == http.StatusBadRequest {
				var failure *OnboardingError
				if !errors.As(err, &failure) || failure.Status != tc.status || failure.Code != "" {
					t.Fatal("incomplete HTTP error exposed an error code or lost status")
				}
			}
		})
	}
}

func TestOnboardingTransportErrorSafety(t *testing.T) {
	c, err := NewOnboardingClient("https://host.test", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private-transport-details")
	})})
	if err != nil {
		t.Fatal(err)
	}
	err = onboardingCall(c, "token")
	if err == nil || strings.Contains(err.Error(), "private-") || errors.Unwrap(err) != nil {
		t.Fatal("transport error missing or transport internals disclosed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.DeviceToken(ctx, "private-device-code")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("context cancellation was not preserved")
	}
}
