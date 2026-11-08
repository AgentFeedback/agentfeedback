package capture

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

func TestCachedSweepPolicyReadsMatchingObservationWithoutNetwork(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy ?#%.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	requests := 0
	c := boundaryClient(t, "sweep-cache-test-key", testTransport(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("cache read attempted network")
	}))
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour).UnixMicro()
	var generation int64
	if err := db.Write(ctx, func(q store.Querier) error {
		var err error
		generation, err = store.NextCaptureGeneration(ctx, q, boundaryServer)
		if err != nil {
			return err
		}
		return store.SetCapturePolicy(ctx, q, store.CapturePolicy{
			Server: boundaryServer, OrgID: testOrg, Enabled: true, NoticeVersion: "1",
			Since: &since, CheckedAt: now.UnixMicro(), State: Enabled,
			CredentialFingerprint: c.CredentialFingerprint(), Generation: generation,
			Bounds: []byte(`{"max_part_bytes":10485760,"max_session_bytes":25165824,"incomplete_upload_ttl_seconds":86400}`),
		})
	}); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	p, err := CachedSweepPolicy(bounded, path, c, Options{Server: "https://HOSTED.example/", Quiet: 2 * time.Minute})
	if err != nil || !usablePolicy(p) || p.Quiet != "2m" || p.generation != generation || !p.Since.Equal(time.UnixMicro(since)) {
		t.Fatalf("cached sweep policy = %+v, %v", p, err)
	}
	other := boundaryClient(t, "different-sweep-key", testTransport(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("cache read attempted network")
	}))
	if p, err := CachedSweepPolicy(bounded, path, other, Options{Server: boundaryServer}); err != nil || p.Enabled {
		t.Fatalf("changed credential retained permission: %+v, %v", p, err)
	}
	if p, err := CachedSweepPolicy(bounded, path, c, Options{Server: "https://other.example"}); err != nil || p.Enabled {
		t.Fatalf("changed destination retained permission: %+v, %v", p, err)
	}
	if err := db.Write(ctx, func(q store.Querier) error {
		_, err := store.NextCaptureGeneration(ctx, q, boundaryServer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if p, err := CachedSweepPolicy(bounded, path, c, Options{Server: boundaryServer}); err != nil || p.Enabled {
		t.Fatalf("superseded generation retained permission: %+v, %v", p, err)
	}
	if requests != 0 {
		t.Fatalf("cache read made %d network requests", requests)
	}
}

func TestCachedSweepPolicyNeverCreatesOrMigratesDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c := boundaryClient(t, "sweep-cache-test-key", testTransport(func(*http.Request) (*http.Response, error) {
		t.Error("cache read attempted network")
		return nil, errors.New("unexpected network")
	}))
	path := filepath.Join(t.TempDir(), "missing", "policy.db")
	if p, err := CachedSweepPolicy(ctx, path, c, Options{Server: boundaryServer}); err != nil || p.Enabled {
		t.Fatalf("missing database = %+v, %v", p, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache read created missing directory: %v", err)
	}
	path = filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := CachedSweepPolicy(ctx, path, c, Options{Server: boundaryServer}); err == nil || p.Enabled {
		t.Fatalf("uninitialized database = %+v, %v", p, err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("cache read migrated empty database: %v, %v", info, err)
	}
	if p, err := CachedSweepPolicy(ctx, path, c, Options{Local: true}); err != nil || p.State != NotApplicable {
		t.Fatalf("local cached policy = %+v, %v", p, err)
	}
}

func TestCachedSweepPolicyHonorsCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := boundaryClient(t, "sweep-cache-test-key", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p, err := CachedSweepPolicy(ctx, path, c, Options{Server: boundaryServer}); err == nil || p.Enabled {
		t.Fatalf("canceled cached policy = %+v, %v", p, err)
	}
}

func TestCachedSweepPolicyRecoveryBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	past, at, future := now.Add(-time.Microsecond).UnixMicro(), now.UnixMicro(), now.Add(time.Microsecond).UnixMicro()
	for _, tc := range []struct {
		name         string
		state        string
		retry        *int64
		change       func(*store.CapturePolicy)
		server       string
		superseded   bool
		wantEligible bool
		wantErr      bool
	}{
		{name: "error", state: Error, wantEligible: true},
		{name: "suspended-after-retry", state: Suspended, retry: &past, wantEligible: true},
		{name: "suspended-at-retry", state: Suspended, retry: &at, wantEligible: true},
		{name: "suspended-before-retry", state: Suspended, retry: &future},
		{name: "suspended-without-retry", state: Suspended},
		{name: "error-after-disabled", state: Error, change: func(p *store.CapturePolicy) { p.Enabled = false }},
		{name: "suspended-after-disabled", state: Suspended, retry: &past, change: func(p *store.CapturePolicy) { p.Enabled = false }},
		{name: "disabled", state: Disabled},
		{name: "unsupported", state: Unsupported},
		{name: "unauthorized", state: Unauthorized},
		{name: "error-credential-mismatch", state: Error, change: func(p *store.CapturePolicy) { p.CredentialFingerprint = "another-credential" }},
		{name: "suspended-credential-mismatch", state: Suspended, retry: &past, change: func(p *store.CapturePolicy) { p.CredentialFingerprint = "another-credential" }},
		{name: "error-destination-mismatch", state: Error, server: "https://other.example"},
		{name: "suspended-destination-mismatch", state: Suspended, retry: &past, server: "https://other.example"},
		{name: "error-generation-mismatch", state: Error, superseded: true},
		{name: "suspended-generation-mismatch", state: Suspended, retry: &past, superseded: true},
		{name: "error-without-consent-epoch", state: Error, change: func(p *store.CapturePolicy) { p.Since = nil }, wantErr: true},
		{name: "suspended-without-consent-epoch", state: Suspended, retry: &past, change: func(p *store.CapturePolicy) { p.Since = nil }, wantErr: true},
		{name: "error-without-identity", state: Error, change: func(p *store.CapturePolicy) { p.OrgID = "" }, wantErr: true},
		{name: "error-without-notice", state: Error, change: func(p *store.CapturePolicy) { p.NoticeVersion = "" }, wantErr: true},
		{name: "error-invalid-bounds", state: Error, change: func(p *store.CapturePolicy) { p.Bounds = []byte(`{}`) }, wantErr: true},
		{name: "error-invalid-retention", state: Error, change: func(p *store.CapturePolicy) { p.Retention = []byte(`42`) }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "policy.db")
			db := boundaryDB(t, path)
			requests := 0
			c := boundaryClient(t, "sweep-recovery-key", testTransport(func(*http.Request) (*http.Response, error) {
				requests++
				return nil, errors.New("cache read attempted network")
			}))
			since := now.Add(-time.Hour).UnixMicro()
			row := store.CapturePolicy{
				Server: boundaryServer, OrgID: testOrg, OrgName: "Example", Enabled: true,
				NoticeVersion: "1", LastNoticeVersion: "1", Since: &since,
				CheckedAt: now.Add(-time.Minute).UnixMicro(), State: tc.state, RetryAt: tc.retry,
				CredentialFingerprint: c.CredentialFingerprint(),
				Retention:             []byte(`{"mode":"days","days":30}`),
				Bounds:                []byte(`{"max_part_bytes":10485760,"max_session_bytes":25165824,"incomplete_upload_ttl_seconds":86400}`),
			}
			if tc.change != nil {
				tc.change(&row)
			}
			if err := db.Write(ctx, func(q store.Querier) error {
				var err error
				row.Generation, err = store.NextCaptureGeneration(ctx, q, row.Server)
				if err != nil {
					return err
				}
				if err := store.SetCapturePolicy(ctx, q, row); err != nil {
					return err
				}
				if tc.superseded {
					_, err = store.NextCaptureGeneration(ctx, q, row.Server)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			server := tc.server
			if server == "" {
				server = boundaryServer
			}
			p, err := CachedSweepPolicy(ctx, path, c, Options{
				Server: server, Quiet: 2 * time.Minute, Clock: func() time.Time { return now },
			})
			if (err != nil) != tc.wantErr || usablePolicy(p) != tc.wantEligible || p.Enabled != tc.wantEligible {
				t.Fatalf("trigger policy = %+v, %v; eligible=%v, error=%v", p, err, tc.wantEligible, tc.wantErr)
			}
			if tc.wantEligible && (p.State != Enabled || p.Server != row.Server || p.OrgID != row.OrgID ||
				p.OrgName != row.OrgName || p.NoticeVersion != row.NoticeVersion || p.LastNoticeVersion != row.LastNoticeVersion ||
				p.Quiet != "2m" || !p.Since.Equal(time.UnixMicro(since)) ||
				!p.CheckedAt.Equal(time.UnixMicro(row.CheckedAt)) || p.generation != row.Generation ||
				p.credentialFingerprint != row.CredentialFingerprint || p.Retention == nil || p.Retention.Days != 30) {
				t.Fatalf("recovery did not retain stored consent: %+v", p)
			}
			if err := db.Read(ctx, func(q store.Querier) error {
				after, found, err := store.CurrentCapturePolicy(ctx, q, row.Server)
				if err != nil {
					return err
				}
				if !found || !reflect.DeepEqual(after, row) {
					t.Errorf("cache read changed observation: before=%+v, after=%+v", row, after)
				}
				current, err := store.CaptureGenerationCurrent(ctx, q, row.Server, row.Generation)
				if err == nil && current == tc.superseded {
					t.Error("cache read changed refresh generation")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if requests != 0 {
				t.Fatalf("cache read made %d network requests", requests)
			}
		})
	}
}

func TestCachedSweepPolicyAuthorizationDenialsStayClosed(t *testing.T) {
	for _, operation := range []string{policyPath, discoveryPath} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			t.Run(operation+"/"+strconv.Itoa(status), func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(t.TempDir(), "policy.db")
				db := boundaryDB(t, path)
				deny := false
				requests := 0
				c := boundaryClient(t, "sweep-auth-key", testTransport(func(r *http.Request) (*http.Response, error) {
					requests++
					if deny && r.URL.Path == operation {
						return boundaryResponse(status, `{}`, ""), nil
					}
					if r.URL.Path == policyPath {
						return boundaryResponse(http.StatusOK, policyJSON(testOrg, true), ""), nil
					}
					return boundaryResponse(http.StatusOK, boundaryDiscovery, ""), nil
				}))
				now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
				first, err := Refresh(ctx, db, c, Options{Server: boundaryServer, Now: now})
				if err != nil || !usablePolicy(first) {
					t.Fatalf("initial enabled consent = %+v, %v", first, err)
				}
				deny = true
				denied, err := Refresh(ctx, db, c, Options{Server: boundaryServer, Now: now.Add(defaultTTL)})
				if err != nil || denied.State != Unauthorized || denied.Enabled {
					t.Fatalf("auth denial = %+v, %v", denied, err)
				}
				before := requests
				p, err := CachedSweepPolicy(ctx, path, c, Options{Server: boundaryServer, Now: now.Add(24 * time.Hour)})
				if err != nil || p.State != Unauthorized || p.Enabled || !p.Since.Equal(first.Since) {
					t.Fatalf("auth denial became trigger permission: %+v, %v", p, err)
				}
				if requests != before {
					t.Fatal("cache read refreshed an auth denial")
				}
			})
		}
	}
}

func TestCachedSweepPolicyTriggerDoesNotAuthorizePush(t *testing.T) {
	for _, state := range []string{Enabled, Error, Suspended} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "policy.db")
			db := boundaryDB(t, path)
			requests := 0
			c := boundaryClient(t, "sweep-child-key", testTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				switch r.URL.Path {
				case policyPath:
					return boundaryResponse(http.StatusOK, policyJSON(testOrg, false), ""), nil
				case discoveryPath:
					return boundaryResponse(http.StatusOK, boundaryDiscovery, ""), nil
				default:
					t.Errorf("child attempted upload to %s", r.URL.Path)
					return nil, errors.New("unexpected upload")
				}
			}))
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
			since, retry := now.Add(-time.Hour).UnixMicro(), now.UnixMicro()
			if err := db.Write(ctx, func(q store.Querier) error {
				generation, err := store.NextCaptureGeneration(ctx, q, boundaryServer)
				if err != nil {
					return err
				}
				return store.SetCapturePolicy(ctx, q, store.CapturePolicy{
					Server: boundaryServer, OrgID: testOrg, Enabled: true, NoticeVersion: "1",
					Since: &since, CheckedAt: now.UnixMicro(), State: state, RetryAt: &retry,
					CredentialFingerprint: c.CredentialFingerprint(), Generation: generation,
					Bounds: []byte(`{"max_part_bytes":10485760,"max_session_bytes":25165824,"incomplete_upload_ttl_seconds":86400}`),
				})
			}); err != nil {
				t.Fatal(err)
			}
			opts := Options{Server: boundaryServer, Now: now}
			p, err := CachedSweepPolicy(ctx, path, c, opts)
			if err != nil || !usablePolicy(p) || requests != 0 {
				t.Fatalf("trigger policy = %+v, %v; requests=%d", p, err, requests)
			}
			results, err := Push(ctx, db, c, sessions.Env{}, PushOptions{Policy: opts, DryRun: true})
			if err != nil || len(results) != 1 || results[0].Reason != "policy_disabled" || requests != 2 {
				t.Fatalf("child did not independently refresh consent: %+v, %v; requests=%d", results, err, requests)
			}
		})
	}
}

func TestCachedSweepPolicyOlderSchemaIsReadOnlyNoop(t *testing.T) {
	ctx := context.Background()
	c := boundaryClient(t, "sweep-old-schema-key", testTransport(func(*http.Request) (*http.Response, error) {
		t.Error("cache read attempted network")
		return nil, errors.New("unexpected network")
	}))
	for version := range 4 {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.db")
			db, err := sql.Open("sqlite", store.FileURI(path, nil))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.ExecContext(ctx, `CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, version); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if p, err := CachedSweepPolicy(ctx, path, c, Options{Server: boundaryServer}); err != nil || p.Enabled {
				t.Fatalf("pre-capture schema = %+v, %v", p, err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("cache read changed pre-capture database")
			}
		})
	}
}

func TestCachedSweepPolicyDoesNotHideDatabaseFaults(t *testing.T) {
	ctx := context.Background()
	c := boundaryClient(t, "sweep-database-fault-key", nil)
	for _, missing := range []string{"capture_policy", "capture_refresh", "schema_version"} {
		t.Run(missing, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.db")
			db := boundaryDB(t, path)
			if err := db.Write(ctx, func(q store.Querier) error {
				_, err := q.ExecContext(ctx, `DROP TABLE `+missing)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if p, err := CachedSweepPolicy(ctx, path, c, Options{Server: boundaryServer}); err == nil || p.Enabled {
				t.Fatalf("missing current table was hidden: %+v, %v", p, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(path, []byte("not a SQLite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := CachedSweepPolicy(ctx, path, c, Options{Server: boundaryServer}); err == nil || p.Enabled {
		t.Fatalf("corrupt database was hidden: %+v, %v", p, err)
	}
}

func TestCachedSweepPolicyRecoveryRequiresUnbrokenCredentialConsent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		interruption string
		changeKey    bool
		suspend      bool
		wantEligible bool
	}{
		{name: "same-credential-error", wantEligible: true},
		{name: "same-credential-suspended", suspend: true, wantEligible: true},
		{name: "changed-credential-error", changeKey: true},
		{name: "changed-credential-suspended", changeKey: true, suspend: true},
		{name: "unauthorized-then-error", interruption: Unauthorized},
		{name: "unauthorized-then-suspended", interruption: Unauthorized, suspend: true},
		{name: "unsupported-then-error", interruption: Unsupported},
		{name: "disabled-then-error", interruption: Disabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "policy.db")
			db := boundaryDB(t, path)
			phase, requests := 0, 0
			transport := testTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				if phase == 2 {
					if tc.suspend {
						return boundaryResponse(http.StatusTooManyRequests, `{}`, "1"), nil
					}
					return nil, errors.New("transient network failure")
				}
				if phase == 1 {
					switch tc.interruption {
					case Unauthorized:
						return boundaryResponse(http.StatusUnauthorized, `{}`, ""), nil
					case Unsupported:
						return boundaryResponse(http.StatusNotFound, `{}`, ""), nil
					}
				}
				if r.URL.Path == policyPath {
					return boundaryResponse(http.StatusOK, policyJSON(testOrg, phase != 1), ""), nil
				}
				return boundaryResponse(http.StatusOK, boundaryDiscovery, ""), nil
			})
			c := boundaryClient(t, "sweep-original-key", transport)
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
			opts := Options{Server: boundaryServer, Now: now}
			first, err := Refresh(ctx, db, c, opts)
			if err != nil || !usablePolicy(first) {
				t.Fatalf("initial consent = %+v, %v", first, err)
			}
			if tc.interruption != "" {
				phase = 1
				opts.Now = opts.Now.Add(unsupportedTTL)
				p, err := Refresh(ctx, db, c, opts)
				if err != nil || p.State != tc.interruption || p.Enabled {
					t.Fatalf("consent interruption = %+v, %v", p, err)
				}
			}
			if tc.changeKey {
				c = boundaryClient(t, "sweep-replacement-key", transport)
			}
			phase = 2
			opts.Now = opts.Now.Add(unsupportedTTL)
			failed, err := Refresh(ctx, db, c, opts)
			wantState := Error
			if tc.suspend {
				wantState = Suspended
			}
			if failed.State != wantState || failed.Enabled || (err != nil) == tc.suspend {
				t.Fatalf("transient failure = %+v, %v", failed, err)
			}
			before := requests
			opts.Now = opts.Now.Add(time.Second)
			p, err := CachedSweepPolicy(ctx, path, c, opts)
			if err != nil || usablePolicy(p) != tc.wantEligible || p.Enabled != tc.wantEligible {
				t.Fatalf("retained consent trigger = %+v, %v; eligible=%v", p, err, tc.wantEligible)
			}
			if tc.wantEligible && !p.Since.Equal(first.Since) {
				t.Fatalf("transient recovery changed consent epoch: first=%+v, recovered=%+v", first, p)
			}
			if requests != before {
				t.Fatal("cache read refreshed broken credential consent")
			}
		})
	}
}
