package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

const (
	testOrg  = "6bfa469e-5786-4d21-907b-dac2cef3acbb"
	otherOrg = "c201b1a0-451e-4a46-90f8-32196894ef40"
)

func policyJSON(org string, enabled bool) string {
	return fmt.Sprintf(`{"org_id":%q,"enabled":%t,"notice_version":"1","max_part_bytes":10485760,"max_session_bytes":25165824,"incomplete_upload_ttl_seconds":86400}`, org, enabled)
}

type testTransport func(*http.Request) (*http.Response, error)

func (fn testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

type policyFixture struct {
	mu              sync.Mutex
	server          *httptest.Server
	db              *store.DB
	client          *client.Client
	body            string
	status          int
	contentType     string
	retryAfter      string
	discovery       string
	discoveryStatus int
	calls           map[string]int
	attempts        int
	networkFailure  bool
	badAuth         bool
}

func newPolicyFixture(t *testing.T) *policyFixture {
	t.Helper()
	f := &policyFixture{
		body: policyJSON(testOrg, true), status: http.StatusOK,
		contentType: "application/json", discoveryStatus: http.StatusOK,
		discovery: `{"openapi":"3.1.0","paths":{"/api/v1/sessions/policy":{"get":{}}}}`,
		calls:     make(map[string]int),
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[r.URL.Path]++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer policy-test-secret" ||
			r.Header.Get("X-Api-Key") != "policy-test-secret" {
			f.badAuth = true
		}
		if r.URL.Path == discoveryPath {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.discoveryStatus)
			_, _ = w.Write([]byte(f.discovery))

			return
		}
		w.Header().Set("Content-Type", f.contentType)
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(f.server.Close)
	ctx := context.Background()
	var err error
	f.db, err = store.Open(ctx, filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Close() })
	f.client, err = client.New(client.Config{
		URL: f.server.URL, APIKey: "policy-test-secret",
		DataDir: t.TempDir(), CacheDir: t.TempDir(), HTTP: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
			f.mu.Lock()
			f.attempts++
			fail := f.networkFailure
			f.mu.Unlock()
			if fail {
				return nil, errors.New("network diagnostic containing policy-test-secret")
			}

			return http.DefaultTransport.RoundTrip(r)
		})},
	})
	if err != nil {
		t.Fatal(err)
	}

	return f
}

func (f *policyFixture) refresh(now time.Time) (Policy, error) {
	return Refresh(context.Background(), f.db, f.client, Options{Server: f.server.URL, Now: now})
}

func (f *policyFixture) requestCounts() (policy, discovery, attempts int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls[policyPath], f.calls[discoveryPath], f.attempts
}

func TestLocalPolicyMakesNoRequests(t *testing.T) {
	t.Parallel()
	f := newPolicyFixture(t)
	for _, c := range []*client.Client{nil, f.client} {
		p, err := Refresh(context.Background(), nil, c, Options{Local: true, Server: f.server.URL})
		if err != nil || p.State != NotApplicable || p.Enabled || p.Quiet != "5m" {
			t.Fatalf("local policy = %+v, %v", p, err)
		}
	}
	_, _, attempts := f.requestCounts()
	if attempts != 0 {
		t.Fatalf("local mode attempted %d requests", attempts)
	}
}

func TestUnsupportedPolicyCachesForOneDay(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"404", "405", "non-json", "absent-route", "head-only"} {
		t.Run(kind, func(t *testing.T) {
			f := newPolicyFixture(t)
			f.mu.Lock()
			switch kind {
			case "404":
				f.status = http.StatusNotFound
			case "405":
				f.status = http.StatusMethodNotAllowed
			case "non-json":
				f.contentType, f.body = "text/html", "<html>legacy server</html>"
			case "absent-route":
				f.discovery = `{"paths":{}}`
			case "head-only":
				f.discovery = `{"paths":{"/api/v1/sessions/policy":{"head":{}}}}`
			}
			f.mu.Unlock()
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
			p, err := f.refresh(now)
			if err != nil || p.State != Unsupported || p.Enabled {
				t.Fatalf("unsupported observation = %+v, %v", p, err)
			}
			_, _, initial := f.requestCounts()
			p, err = f.refresh(now.Add(24*time.Hour - time.Microsecond))
			_, _, cached := f.requestCounts()
			if err != nil || p.State != Unsupported || cached != initial {
				t.Fatalf("unsupported cache: state=%s error=%v requests=%d initial=%d", p.State, err, cached, initial)
			}
			if _, err := f.refresh(now.Add(24 * time.Hour)); err != nil {
				t.Fatal(err)
			}
			_, _, expired := f.requestCounts()
			if expired != initial*2 {
				t.Fatalf("daily expiry requests=%d, want %d", expired, initial*2)
			}
		})
	}
}

func TestEnabledPolicyTTLAndJSON(t *testing.T) {
	t.Parallel()
	f := newPolicyFixture(t)
	f.mu.Lock()
	f.body = strings.TrimSuffix(policyJSON(testOrg, true), "}") + `,"org_name":"Example Org","retention":{"mode":"retain","days":30}}`
	f.mu.Unlock()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	p, err := f.refresh(now)
	if err != nil || !p.Enabled || p.State != Enabled || !p.Since.Equal(now) {
		t.Fatalf("enabled policy = %+v, %v", p, err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var encoded struct {
		OrgName   string    `json:"org_name"`
		Quiet     string    `json:"quiet"`
		Since     time.Time `json:"since"`
		Enabled   bool      `json:"enabled"`
		Retention Retention `json:"retention"`
		Bounds
	}
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatal(err)
	}
	if encoded.OrgName != "Example Org" || encoded.Quiet != "5m" || !encoded.Enabled ||
		!encoded.Since.Equal(now) || encoded.Retention.Days != 30 || encoded.MaxPartBytes != 10485760 {
		t.Fatalf("JSON lost policy semantics: %s", raw)
	}
	if err := MarkNotice(context.Background(), f.db, p); err != nil {
		t.Fatal(err)
	}
	p, err = f.refresh(now.Add(5*time.Minute - time.Microsecond))
	_, _, cached := f.requestCounts()
	if err != nil || cached != 2 || p.LastNoticeVersion != p.NoticeVersion {
		t.Fatalf("enabled cache = %+v, %v, requests=%d", p, err, cached)
	}
	p, err = f.refresh(now.Add(5 * time.Minute))
	_, _, expired := f.requestCounts()
	f.mu.Lock()
	badAuth := f.badAuth
	f.mu.Unlock()
	if err != nil || expired != 4 || !p.Since.Equal(now) || badAuth {
		t.Fatalf("enabled expiry = %+v, %v, requests=%d badAuth=%t", p, err, expired, badAuth)
	}
	f.mu.Lock()
	f.body = strings.ReplaceAll(f.body, `"notice_version":"1"`, `"notice_version":"2"`)
	f.mu.Unlock()
	p, err = f.refresh(now.Add(10 * time.Minute))
	if err != nil || p.NoticeVersion != "2" || p.LastNoticeVersion != "1" {
		t.Fatalf("new notice was incorrectly marked printed: %+v, %v", p, err)
	}
}

func TestUnauthorizedCannotBeMaskedByDiscovery(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newPolicyFixture(t)
			f.mu.Lock()
			f.status, f.discovery = status, `{"paths":{}}`
			f.mu.Unlock()
			now := time.Now().UTC().Truncate(time.Second)
			p, err := f.refresh(now)
			policy, discovery, _ := f.requestCounts()
			if err != nil || p.State != Unauthorized || p.Enabled || policy != 1 || discovery != 0 {
				t.Fatalf("unauthorized = %+v, %v; requests %d/%d", p, err, policy, discovery)
			}
			f.mu.Lock()
			f.status, f.discovery = http.StatusOK, `{"paths":{"/api/v1/sessions/policy":{"get":{}}}}`
			f.mu.Unlock()
			if p, err = f.refresh(now.Add(4 * time.Minute)); err != nil || p.State != Unauthorized {
				t.Fatalf("unauthorized cache = %+v, %v", p, err)
			}
			if _, _, n := f.requestCounts(); n != 1 {
				t.Fatalf("unauthorized cache made %d requests", n)
			}
			if p, err = f.refresh(now.Add(5 * time.Minute)); err != nil || !p.Enabled {
				t.Fatalf("unauthorized expiry = %+v, %v", p, err)
			}
		})
	}
	f := newPolicyFixture(t)
	f.mu.Lock()
	f.discoveryStatus = http.StatusForbidden
	f.mu.Unlock()
	p, err := f.refresh(time.Now())
	if err != nil || p.State != Unauthorized || p.Enabled {
		t.Fatalf("discovery authorization = %+v, %v", p, err)
	}
}

func TestSuspensionExpiresAtRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		value string
		wait  time.Duration
	}{
		{"seconds", "90", 90 * time.Second},
		{"http-date", now.Add(10 * time.Minute).Format(http.TimeFormat), 10 * time.Minute},
		{"default", "", time.Minute},
		{"invalid", "policy-test-secret", time.Minute},
		{"negative", "-1", time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPolicyFixture(t)
			f.mu.Lock()
			f.status, f.retryAfter = http.StatusTooManyRequests, tc.value
			f.mu.Unlock()
			p, err := f.refresh(now)
			if err != nil || p.State != Suspended || p.Enabled || !p.RetryAt.Equal(now.Add(tc.wait)) {
				t.Fatalf("suspension = %+v, %v", p, err)
			}
			f.mu.Lock()
			f.status = http.StatusOK
			f.mu.Unlock()
			if p, err = f.refresh(now.Add(tc.wait - time.Microsecond)); err != nil || p.State != Suspended {
				t.Fatalf("cached suspension = %+v, %v", p, err)
			}
			if _, _, n := f.requestCounts(); n != 1 {
				t.Fatalf("suspended policy made %d requests", n)
			}
			if p, err = f.refresh(now.Add(tc.wait)); err != nil || !p.Enabled {
				t.Fatalf("suspension expiry = %+v, %v", p, err)
			}
		})
	}
}

func TestPolicyFailuresFailClosedAndKeepConsentEpoch(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"network", "503", "malformed-json", "bad-identity", "bad-bounds", "bad-type", "null-enabled", "missing-notice", "bad-retention", "duplicate-enabled", "bad-discovery"} {
		t.Run(kind, func(t *testing.T) {
			f := newPolicyFixture(t)
			now := time.Now().UTC().Truncate(time.Second)
			first, err := f.refresh(now)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			switch kind {
			case "network":
				f.networkFailure = true
			case "503":
				f.status, f.body = 503, `{"message":"policy-test-secret"}`
			case "malformed-json":
				f.body = `{"policy-test-secret":`
			case "bad-identity":
				f.body = strings.ReplaceAll(f.body, testOrg, "policy-test-secret")
			case "bad-bounds":
				f.body = strings.ReplaceAll(f.body, "10485760", "10485761")
			case "bad-type":
				f.body = strings.ReplaceAll(f.body, `"enabled":true`, `"enabled":"policy-test-secret"`)
			case "null-enabled":
				f.body = strings.ReplaceAll(f.body, `"enabled":true`, `"enabled":null`)
			case "missing-notice":
				f.body = strings.ReplaceAll(f.body, `"notice_version":"1",`, "")
			case "bad-retention":
				f.body = strings.TrimSuffix(f.body, "}") + `,"retention":{"mode":"retain","days":"policy-test-secret"}}`
			case "duplicate-enabled":
				f.body = strings.TrimSuffix(f.body, "}") + `,"enabled":true}`
			case "bad-discovery":
				f.discovery = `{"paths":null}`
			}
			f.mu.Unlock()
			failed, err := f.refresh(now.Add(5 * time.Minute))
			if err == nil || failed.Enabled || failed.State != Error || !failed.Since.Equal(first.Since) || strings.Contains(err.Error(), "policy-test-secret") {
				t.Fatalf("failed refresh = %+v, %v", failed, err)
			}
			_, _, before := f.requestCounts()
			failed, err = f.refresh(now.Add(5*time.Minute + time.Second))
			_, _, after := f.requestCounts()
			if err == nil || failed.Enabled || after <= before {
				t.Fatalf("error cache revived permission: %+v, %v; attempts %d -> %d", failed, err, before, after)
			}
			f.mu.Lock()
			f.networkFailure, f.status, f.body = false, http.StatusOK, policyJSON(testOrg, true)
			f.discovery = `{"paths":{"/api/v1/sessions/policy":{"get":{}}}}`
			f.mu.Unlock()
			recovered, err := f.refresh(now.Add(5*time.Minute + 2*time.Second))
			if err != nil || !recovered.Enabled || !recovered.Since.Equal(first.Since) {
				t.Fatalf("recovery reset consent epoch: %+v, %v", recovered, err)
			}
		})
	}
}

func TestConsentEpochOffOnAndOrgSwitching(t *testing.T) {
	t.Parallel()
	f := newPolicyFixture(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	observe := func(org string, enabled bool, status int) Policy {
		t.Helper()
		f.mu.Lock()
		f.body, f.status = policyJSON(org, enabled), status
		f.mu.Unlock()
		p, err := f.refresh(now)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(5 * time.Minute)

		return p
	}
	first := observe(testOrg, true, http.StatusOK)
	off := observe(testOrg, false, http.StatusOK)
	if off.Enabled || off.State != Disabled || !off.Since.Equal(first.Since) {
		t.Fatalf("off observation changed previous epoch: %+v", off)
	}
	on := observe(testOrg, true, http.StatusOK)
	if !on.Enabled || !on.Since.After(first.Since) {
		t.Fatalf("off-on did not reset epoch: first=%+v on=%+v", first, on)
	}
	other := observe(otherOrg, true, http.StatusOK)
	if other.OrgID != otherOrg || !other.Since.After(on.Since) {
		t.Fatalf("other org lacks independent epoch: %+v", other)
	}
	back := observe(testOrg, true, http.StatusOK)
	if back.OrgID != testOrg || !back.Since.Equal(on.Since) {
		t.Fatalf("switching org reset epoch: %+v", back)
	}
	unauthorized := observe(testOrg, true, http.StatusForbidden)
	if unauthorized.Enabled || unauthorized.State != Unauthorized || !unauthorized.Since.Equal(on.Since) {
		t.Fatalf("authorization failure changed consent: %+v", unauthorized)
	}
	back = observe(testOrg, true, http.StatusOK)
	if !back.Enabled || !back.Since.After(on.Since) {
		t.Fatalf("authorization recovery did not reset epoch: %+v", back)
	}
	on = back
	suspended := observe(testOrg, true, http.StatusTooManyRequests)
	if suspended.Enabled || suspended.State != Suspended || !suspended.Since.Equal(on.Since) {
		t.Fatalf("suspension changed consent: %+v", suspended)
	}
	back = observe(testOrg, true, http.StatusOK)
	if !back.Enabled || !back.Since.Equal(on.Since) {
		t.Fatalf("suspension recovery reset epoch: %+v", back)
	}
}

func TestStatusRemovesTerminalControlsAndCredentials(t *testing.T) {
	t.Parallel()
	p := Policy{
		State: Enabled, Enabled: true, OrgID: testOrg,
		OrgName: "Example\n\x1b[31m\u202e Bearer status-secret-long-value", Quiet: "5m",
		Server: "https://user:password@example.test",
	}
	status := Status(p)
	if strings.Contains(status, "status-secret") || strings.Contains(status, "password") {
		t.Fatalf("status disclosed credentials: %q", status)
	}
	for _, r := range status {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Fatalf("status retained terminal control %U", r)
		}
	}
}

func TestConfiguredPolicyTTLAndQuiet(t *testing.T) {
	t.Parallel()
	f := newPolicyFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	refresh := func(at time.Time) Policy {
		t.Helper()
		p, err := Refresh(context.Background(), f.db, f.client, Options{
			Server: f.server.URL, Now: at, PolicyTTL: time.Minute, Quiet: 2 * time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}

		return p
	}
	if p := refresh(now); p.Quiet != "2m" || !p.Enabled {
		t.Fatalf("configured policy = %+v", p)
	}
	refresh(now.Add(time.Minute - time.Microsecond))
	if _, _, n := f.requestCounts(); n != 2 {
		t.Fatalf("configured TTL cache made %d requests", n)
	}
	refresh(now.Add(time.Minute))
	if _, _, n := f.requestCounts(); n != 4 {
		t.Fatalf("configured TTL expiry made %d requests", n)
	}
}
