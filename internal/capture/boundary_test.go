package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
)

const (
	boundaryServer    = "https://hosted.example"
	boundaryDiscovery = `{"paths":{"/api/v1/sessions/policy":{"get":{}}}}`
)

func boundaryClient(t *testing.T, key string, transport testTransport) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{
		URL: boundaryServer, APIKey: key, DataDir: t.TempDir(), CacheDir: t.TempDir(),
		HTTP: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func boundaryResponse(status int, body, retry string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	if retry != "" {
		h.Set("Retry-After", retry)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func boundaryDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestCredentialSwitchRequiresAuthenticationWithinTTL(t *testing.T) {
	t.Parallel()
	db := boundaryDB(t, filepath.Join(t.TempDir(), "capture.db"))
	calls := make(map[string]int)
	transport := testTransport(func(r *http.Request) (*http.Response, error) {
		key := r.Header.Get("X-Api-Key")
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("authentication headers disagree")
		}
		calls[key]++
		if r.URL.Path == discoveryPath {
			return boundaryResponse(http.StatusOK, boundaryDiscovery, ""), nil
		}
		switch key {
		case "key-a":
			return boundaryResponse(http.StatusOK, policyJSON(testOrg, true), ""), nil
		case "key-b-disabled":
			return boundaryResponse(http.StatusOK, policyJSON(otherOrg, false), ""), nil
		case "key-b-enabled":
			return boundaryResponse(http.StatusOK, policyJSON(otherOrg, true), ""), nil
		case "key-unsupported":
			return boundaryResponse(http.StatusNotFound, `{}`, ""), nil
		case "key-suspended":
			return boundaryResponse(http.StatusTooManyRequests, `{}`, "9000000000"), nil
		default:
			return boundaryResponse(http.StatusUnauthorized, `{}`, ""), nil
		}
	})
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var firstEpoch time.Time
	for i, tc := range []struct {
		key, state, org string
	}{
		{"key-a", Enabled, testOrg},
		{"key-b-disabled", Disabled, otherOrg},
		{"key-invalid", Unauthorized, ""},
		{"key-b-enabled", Enabled, otherOrg},
		{"key-unsupported", Unsupported, ""},
		{"key-suspended", Suspended, ""},
		{"key-a", Enabled, testOrg},
	} {
		c := boundaryClient(t, tc.key, transport)
		at := now.Add(time.Duration(i) * time.Second)
		p, err := Refresh(context.Background(), db, c, Options{Server: boundaryServer, Now: at})
		if err != nil || p.State != tc.state || p.Enabled != (tc.state == Enabled) || (tc.org != "" && p.OrgID != tc.org) {
			t.Fatalf("switch %d = %+v, %v", i, p, err)
		}
		if i == 0 {
			firstEpoch = p.Since
		}
		if i == 6 && !p.Since.Equal(firstEpoch) {
			t.Fatalf("switch back reset independent org epoch: %+v", p)
		}
		before := calls[tc.key]
		cached, err := Refresh(context.Background(), db, c, Options{Server: boundaryServer, Now: at.Add(time.Millisecond)})
		if err != nil || cached.State != tc.state || cached.Enabled != p.Enabled || calls[tc.key] != before {
			t.Fatalf("same-credential cache = %+v, %v; requests %d -> %d", cached, err, before, calls[tc.key])
		}
		if err := db.Read(context.Background(), func(q store.Querier) error {
			row, found, err := store.CurrentCapturePolicy(context.Background(), q, boundaryServer)
			sum := sha256.Sum256([]byte(tc.key))
			if err == nil && (!found || row.CredentialFingerprint != hex.EncodeToString(sum[:])) {
				t.Errorf("cache is not bound to actual authenticated client: %+v", row)
			}
			stored, _ := json.Marshal(row)
			output, _ := json.Marshal(p)
			if strings.Contains(string(stored), tc.key) || strings.Contains(string(output), tc.key) || strings.Contains(string(output), row.CredentialFingerprint) {
				t.Error("credential or internal fingerprint leaked into policy output")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOverlappingRefreshesOnlyNewestGenerationCommits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                   string
		newKey                 string
		newOrg                 string
		oldStatus, newStatus   int
		oldEnabled, newEnabled bool
	}{
		{"enabled-then-disabled", "new-key", testOrg, http.StatusOK, http.StatusOK, true, false},
		{"enabled-then-suspended", "new-key", testOrg, http.StatusOK, http.StatusTooManyRequests, true, false},
		{"disabled-then-enabled", "new-key", otherOrg, http.StatusOK, http.StatusOK, false, true},
		{"suspended-then-enabled", "new-key", otherOrg, http.StatusTooManyRequests, http.StatusOK, false, true},
		{"same-key-enabled-then-disabled", "old-key", testOrg, http.StatusOK, http.StatusOK, true, false},
		{"same-key-enabled-then-suspended", "old-key", testOrg, http.StatusOK, http.StatusTooManyRequests, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture.db")
			oldDB, newDB := boundaryDB(t, path), boundaryDB(t, path)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			oldClient := boundaryClient(t, "old-key", testTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == discoveryPath {
					return boundaryResponse(http.StatusOK, boundaryDiscovery, ""), nil
				}
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				return boundaryResponse(tc.oldStatus, policyJSON(testOrg, tc.oldEnabled), "90"), nil
			}))
			newClient := boundaryClient(t, tc.newKey, testTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == discoveryPath {
					return boundaryResponse(http.StatusOK, boundaryDiscovery, ""), nil
				}
				return boundaryResponse(tc.newStatus, policyJSON(tc.newOrg, tc.newEnabled), "90"), nil
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
			opts := Options{Server: boundaryServer, Now: now}
			type result struct {
				policy Policy
				err    error
			}
			oldDone := make(chan result, 1)
			go func() {
				p, err := Refresh(ctx, oldDB, oldClient, opts)
				oldDone <- result{p, err}
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("older refresh did not reach network")
			}
			latest, err := Refresh(ctx, newDB, newClient, opts)
			wantState := Disabled
			if tc.newStatus == http.StatusTooManyRequests {
				wantState = Suspended
			}
			if tc.newEnabled {
				wantState = Enabled
			}
			if err != nil || latest.State != wantState || latest.Enabled != tc.newEnabled {
				t.Fatalf("new refresh while older network request blocked = %+v, %v", latest, err)
			}
			unblock()
			older := <-oldDone
			if older.err == nil || !strings.Contains(older.err.Error(), "superseded") || older.policy.State != Error || older.policy.Enabled || older.policy.OrgID != "" {
				t.Fatalf("superseded caller inherited permission/identity: %+v, %v", older.policy, older.err)
			}
			cached, err := Refresh(ctx, newDB, newClient, opts)
			if err != nil || cached.State != wantState || cached.Enabled != latest.Enabled || cached.OrgID != latest.OrgID || !cached.Since.Equal(latest.Since) || !cached.RetryAt.Equal(latest.RetryAt) {
				t.Fatalf("older refresh overwrote newest observation: %+v, %v", cached, err)
			}
			if err := newDB.Read(ctx, func(q store.Querier) error {
				row, found, err := store.GetCapturePolicy(ctx, q, boundaryServer, testOrg)
				if err == nil && latest.OrgID == testOrg && (!found || row.Enabled || row.Since != nil || row.Generation != 2) {
					t.Errorf("superseded enabled observation changed disabled consent history: %+v", row)
				}
				if err == nil && latest.OrgID != testOrg && found {
					t.Errorf("superseded observation created consent history: %+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetryAfterStartsAtResponseReceipt(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, route := range []string{policyPath, discoveryPath} {
		for _, tc := range []struct {
			name, header string
			want         time.Time
		}{
			{"seconds", "60", start.Add(68 * time.Second)},
			{"default", "", start.Add(68 * time.Second)},
			{"absolute", start.Add(time.Hour).Format(http.TimeFormat), start.Add(time.Hour)},
			{"uncapped", "9000000000", start.Add(8 * time.Second).Add(9000000000 * time.Second)},
		} {
			t.Run(fmt.Sprintf("%s/%s", route, tc.name), func(t *testing.T) {
				db := boundaryDB(t, filepath.Join(t.TempDir(), "capture.db"))
				clock := start
				c := boundaryClient(t, "clock-key", testTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == route {
						clock = start.Add(8 * time.Second)
						return boundaryResponse(http.StatusTooManyRequests, `{}`, tc.header), nil
					}
					return boundaryResponse(http.StatusOK, policyJSON(testOrg, true), ""), nil
				}))
				p, err := Refresh(context.Background(), db, c, Options{Server: boundaryServer, Clock: func() time.Time { return clock }})
				if err != nil || p.State != Suspended || p.Enabled || !p.RetryAt.Equal(tc.want) {
					t.Fatalf("response-relative backoff = %+v, %v; want %s", p, err, tc.want)
				}
			})
		}
	}
}

func TestLocalPolicyOmitsCredentialBearingServer(t *testing.T) {
	t.Parallel()
	for _, server := range []string{"https://user:password@hosted.example", "https://hosted.example?token=secret", "not a URL secret"} {
		p, err := Refresh(context.Background(), nil, nil, Options{Local: true, Server: server})
		raw, marshalErr := json.Marshal(p)
		if err != nil || marshalErr != nil || p.Server != "" || strings.Contains(string(raw), `"server"`) || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "password") {
			t.Fatalf("local policy exposed configured server: %s, %v, %v", raw, err, marshalErr)
		}
	}
}

func TestServerAndOrgCaseShareConsentEpoch(t *testing.T) {
	t.Parallel()
	db := boundaryDB(t, filepath.Join(t.TempDir(), "capture.db"))
	calls := 0
	c := boundaryClient(t, "case-key", testTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == discoveryPath {
			return boundaryResponse(http.StatusOK, boundaryDiscovery, ""), nil
		}
		org := strings.ToUpper(testOrg)
		if calls > 2 {
			org = testOrg
		}
		return boundaryResponse(http.StatusOK, policyJSON(org, true), ""), nil
	}))
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	first, err := Refresh(context.Background(), db, c, Options{Server: "https://HOSTED.Example/", Now: now})
	if err != nil || first.OrgID != testOrg || first.Server != boundaryServer {
		t.Fatalf("normalization = %+v, %v", first, err)
	}
	cached, err := Refresh(context.Background(), db, c, Options{Server: boundaryServer, Now: now.Add(time.Second)})
	if err != nil || calls != 2 || !cached.Since.Equal(first.Since) {
		t.Fatalf("hostname case split cache: %+v, %v; calls=%d", cached, err, calls)
	}
	latest, err := Refresh(context.Background(), db, c, Options{Server: boundaryServer, Now: now.Add(5 * time.Minute)})
	if err != nil || calls != 4 || !latest.Since.Equal(first.Since) {
		t.Fatalf("UUID case split epoch: %+v, %v; calls=%d", latest, err, calls)
	}
}

func TestEnabledStatusDescribesPermissionNotActiveUploads(t *testing.T) {
	t.Parallel()
	p := Policy{
		State: Enabled, Enabled: true, OrgID: testOrg, OrgName: "Example Org",
		Server: boundaryServer, Quiet: "5m", Retention: &Retention{Mode: "retain", Days: 30},
	}
	status := Status(p)
	for _, required := range []string{
		"Example Org", "Policy permits sessions quiet for 5m", "scrubbed for known secret formats",
		"uploaded to " + boundaryServer, "prompts, responses, tool calls and output",
		"Retention: 30 days", "Sessions under [collect] deny paths stay local",
	} {
		if !strings.Contains(status, required) {
			t.Errorf("status missing %q: %s", required, status)
		}
	}
	if strings.Contains(status, "is scrubbed") || strings.Contains(status, "processes session transcripts") {
		t.Fatalf("status claims active upload behavior: %s", status)
	}
}

func TestSupersededRefreshFailsBeforeNewestResponseArrives(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "capture.db")
	oldDB, newDB := boundaryDB(t, path), boundaryDB(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := []chan struct{}{make(chan struct{}), make(chan struct{})}
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	controlledClient := func(index int, org string) *client.Client {
		return boundaryClient(t, fmt.Sprintf("key-%d", index), testTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == discoveryPath {
				return boundaryResponse(http.StatusOK, boundaryDiscovery, ""), nil
			}
			close(started[index])
			select {
			case <-release[index]:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return boundaryResponse(http.StatusOK, policyJSON(org, true), ""), nil
		}))
	}
	oldClient, newClient := controlledClient(0, testOrg), controlledClient(1, otherOrg)
	type result struct {
		policy Policy
		err    error
	}
	done := []chan result{make(chan result, 1), make(chan result, 1)}
	opts := Options{Server: boundaryServer, Now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	start := func(index int, db *store.DB, c *client.Client) {
		go func() {
			p, err := Refresh(ctx, db, c, opts)
			done[index] <- result{p, err}
		}()
		select {
		case <-started[index]:
		case <-ctx.Done():
			t.Fatal("refresh did not reach network")
		}
	}
	start(0, oldDB, oldClient)
	start(1, newDB, newClient)
	// The newer request has allocated its generation but has not supplied an
	// observation yet. Even now the old response must not grant permission.
	close(release[0])
	older := <-done[0]
	if older.err == nil || older.policy.State != Error || older.policy.Enabled || older.policy.OrgID != "" {
		t.Fatalf("old observation committed while newest request was pending: %+v, %v", older.policy, older.err)
	}
	close(release[1])
	newer := <-done[1]
	if newer.err != nil || !newer.policy.Enabled || newer.policy.OrgID != otherOrg {
		t.Fatalf("newest request did not commit its own permission: %+v, %v", newer.policy, newer.err)
	}
}
