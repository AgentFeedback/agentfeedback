package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSessionPushScopeIsolationAndForwardMigration(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	base := PushScope{Server: "https://one.example", OrgID: "org-one", CredentialFingerprint: "credential-hash", ConsentSince: 1}
	at := int64(20)
	row := SessionPush{PushScope: base, Harness: "claude-code", SessionID: "source", Revision: "100", Head: "head-hash", SourceSize: 100, ContentSHA256: "content-hash", EnvelopeBytes: 200, MaxSessionBytes: 1000, PushedAt: &at, ObservedAt: at, SubmissionUID: "submission", Status: "pushed"}
	if err := db.Write(ctx, func(q Querier) error { return SetSessionPush(ctx, q, row) }); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []PushScope{base, {Server: "https://other.example", OrgID: base.OrgID, CredentialFingerprint: base.CredentialFingerprint, ConsentSince: base.ConsentSince}, {Server: base.Server, OrgID: "org-two", CredentialFingerprint: base.CredentialFingerprint, ConsentSince: base.ConsentSince}, {Server: base.Server, OrgID: base.OrgID, CredentialFingerprint: "other-hash", ConsentSince: base.ConsentSince}, {Server: base.Server, OrgID: base.OrgID, CredentialFingerprint: base.CredentialFingerprint, ConsentSince: 2}} {
		if err := db.Read(ctx, func(q Querier) error {
			rows, err := ListSessionPushes(ctx, q, scope)
			want := 0
			if scope == base {
				want = 1
			}
			if len(rows) != want {
				t.Fatalf("scope %+v returned %d rows", scope, len(rows))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	row.Status, row.Reason, row.PushedAt = "too_large", "too_large", nil
	if err := db.Write(ctx, func(q Querier) error { return SetSessionPush(ctx, q, row) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Read(ctx, func(q Querier) error {
		rows, err := ListSessionPushes(ctx, q, base)
		if len(rows) != 1 || rows[0].Status != "too_large" || rows[0].PushedAt != nil {
			t.Fatalf("outcome update %+v", rows)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionPushReservedRevisionMigrationPreservesData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "push.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	row := SessionPush{
		PushScope: PushScope{Server: "https://one.example", OrgID: "org-one", CredentialFingerprint: "credential-hash", ConsentSince: 1},
		Harness:   "pi", SessionID: "native-session", Revision: "native-revision", Head: "head-hash",
		SourceSize: 123, ContentSHA256: "content-hash", EnvelopeBytes: 456, MaxSessionBytes: 1000,
		ObservedAt: 20, SubmissionUID: "submission", Status: "pushed",
	}
	if err := db.Write(ctx, func(q Querier) error {
		if err := SetSessionPush(ctx, q, row); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, "UPDATE schema_version SET version = 7")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen version-7 database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Read(ctx, func(q Querier) error {
		rows, err := ListSessionPushes(ctx, q, row.PushScope)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0] != row {
			t.Fatalf("push after migration: %+v, want %+v", rows, row)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	row.Revision, row.Status, row.Reason = "reserved-revision", "reserved", "in_flight"
	if err := db.Write(ctx, func(q Querier) error { return SetSessionPush(ctx, q, row) }); err != nil {
		t.Fatalf("persist reserved status: %v", err)
	}
}

func TestSuspendCapturePolicyRequiresExactActiveGeneration(t *testing.T) {
	for _, change := range []string{"none", "generation", "credential", "org", "consent"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			db, err := Open(ctx, filepath.Join(t.TempDir(), "capture.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			since := int64(10)
			scope := PushScope{Server: "https://one.example", OrgID: "org", CredentialFingerprint: "fingerprint", ConsentSince: since}
			var generation int64
			if err := db.Write(ctx, func(q Querier) error {
				var err error
				generation, err = NextCaptureGeneration(ctx, q, scope.Server)
				if err != nil {
					return err
				}
				return SetCapturePolicy(ctx, q, CapturePolicy{Server: scope.Server, OrgID: scope.OrgID, CredentialFingerprint: scope.CredentialFingerprint, Since: &since, Enabled: true, State: "enabled", Generation: generation})
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Write(ctx, func(q Querier) error {
				r, _, err := CurrentCapturePolicy(ctx, q, scope.Server)
				if err != nil {
					return err
				}
				switch change {
				case "generation":
					r.Generation, err = NextCaptureGeneration(ctx, q, scope.Server)
				case "credential":
					r.CredentialFingerprint = "different"
				case "org":
					r.OrgID = "other"
				case "consent":
					n := int64(11)
					r.Since = &n
				}
				if err != nil {
					return err
				}
				if err := SetCapturePolicy(ctx, q, r); err != nil {
					return err
				}
				return SuspendCapturePolicy(ctx, q, scope, generation, 20, 80)
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.Read(ctx, func(q Querier) error {
				r, _, err := CurrentCapturePolicy(ctx, q, scope.Server)
				if change == "none" {
					if r.State != "suspended" || r.RetryAt == nil || *r.RetryAt != 80 || !r.Enabled {
						t.Fatalf("suspension lost consent epoch %+v", r)
					}
				} else if r.State != "enabled" || r.RetryAt != nil {
					t.Fatalf("stale suspension replaced newer policy %+v", r)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
