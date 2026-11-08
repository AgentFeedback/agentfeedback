package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCapturePolicyIndependentEpochsAndCurrentSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTest(t)
	firstEpoch, secondEpoch, otherEpoch := int64(100), int64(300), int64(400)
	row := CapturePolicy{
		Server: "https://one.test", OrgID: "org-one", OrgName: "One",
		Enabled: true, NoticeVersion: "1", State: "enabled", Since: &firstEpoch, CheckedAt: 100,
		Retention: json.RawMessage(`{"mode":"retain","days":30}`),
		Bounds:    json.RawMessage(`{"max_part_bytes":10485760}`),
	}
	put := func(p CapturePolicy) {
		t.Helper()
		if err := db.Write(ctx, func(q Querier) error { return SetCapturePolicy(ctx, q, p) }); err != nil {
			t.Fatal(err)
		}
	}
	get := func(server, org string) CapturePolicy {
		t.Helper()
		var p CapturePolicy
		if err := db.Read(ctx, func(q Querier) error {
			var ok bool
			var err error
			p, ok, err = GetCapturePolicy(ctx, q, server, org)
			if err == nil && !ok {
				t.Fatalf("missing policy for %s/%s", server, org)
			}

			return err
		}); err != nil {
			t.Fatal(err)
		}

		return p
	}
	current := func(server string) CapturePolicy {
		t.Helper()
		var p CapturePolicy
		if err := db.Read(ctx, func(q Querier) error {
			var ok bool
			var err error
			p, ok, err = CurrentCapturePolicy(ctx, q, server)
			if err == nil && !ok {
				t.Fatalf("missing current policy for %s", server)
			}

			return err
		}); err != nil {
			t.Fatal(err)
		}

		return p
	}
	put(row)
	stored := get(row.Server, row.OrgID)
	if !stored.Enabled || stored.Since == nil || *stored.Since != firstEpoch ||
		stored.OrgName != row.OrgName || string(stored.Retention) != string(row.Retention) || string(stored.Bounds) != string(row.Bounds) {
		t.Fatalf("initial observation lost fields: %+v", stored)
	}
	row.Enabled, row.State, row.CheckedAt = false, "disabled", 200
	put(row)
	stored = get(row.Server, row.OrgID)
	if stored.Enabled || stored.State != "disabled" || *stored.Since != firstEpoch {
		t.Fatalf("disabled policy lost previous epoch: %+v", stored)
	}
	row.Enabled, row.State, row.Since, row.CheckedAt = true, "enabled", &secondEpoch, 300
	put(row)
	other := row
	other.OrgID, other.Since, other.CheckedAt = "org-two", &otherEpoch, 400
	put(other)
	// Select the first org at exactly the same timestamp: checked_at ordering
	// alone cannot represent this switch back to its independently stored epoch.
	row.CheckedAt = 400
	put(row)
	if got := current(row.Server); got.OrgID != row.OrgID || *got.Since != secondEpoch {
		t.Fatalf("org switching selected wrong epoch: %+v", got)
	}
	if got := get(row.Server, other.OrgID); *got.Since != otherEpoch {
		t.Fatalf("org switching mutated historical epoch: %+v", got)
	}
	other.Server, other.OrgID = "https://two.test", row.OrgID
	put(other)
	if got := current(row.Server); *got.Since != secondEpoch {
		t.Fatalf("server switching mutated first server: %+v", got)
	}
	if got := current(other.Server); *got.Since != otherEpoch {
		t.Fatalf("server lacks independent epoch: %+v", got)
	}
	// Transient state does not overwrite the last successful consent value.
	row.State, row.CheckedAt = "unauthorized", 500
	put(row)
	if got := current(row.Server); !got.Enabled || got.State != "unauthorized" || *got.Since != secondEpoch {
		t.Fatalf("transient observation lost consent epoch: %+v", got)
	}
}

func TestCaptureNoticeVersionAndAtomicSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTest(t)
	p := CapturePolicy{Server: "https://one.test", OrgID: "one", NoticeVersion: "1", State: "disabled"}
	if err := db.Write(ctx, func(q Querier) error {
		if err := SetCapturePolicy(ctx, q, p); err != nil {
			return err
		}

		return MarkCaptureNotice(ctx, q, p.Server, p.OrgID, p.NoticeVersion)
	}); err != nil {
		t.Fatal(err)
	}
	p.NoticeVersion, p.LastNoticeVersion = "2", "1"
	if err := db.Write(ctx, func(q Querier) error {
		if err := SetCapturePolicy(ctx, q, p); err != nil {
			return err
		}

		return MarkCaptureNotice(ctx, q, p.Server, p.OrgID, "1")
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Read(ctx, func(q Querier) error {
		got, ok, err := CurrentCapturePolicy(ctx, q, p.Server)
		if err == nil && (!ok || got.NoticeVersion != "2" || got.LastNoticeVersion != "1") {
			t.Fatalf("stale notice marked a new version: %+v", got)
		}

		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The selected row and its replacement must roll back together if writing
	// the replacement fails a schema constraint.
	bad := p
	bad.OrgID, bad.State = "two", "invalid-state"
	if err := db.Write(ctx, func(q Querier) error { return SetCapturePolicy(ctx, q, bad) }); err == nil {
		t.Fatal("invalid state unexpectedly committed")
	}
	if err := db.Read(ctx, func(q Querier) error {
		got, ok, err := CurrentCapturePolicy(ctx, q, p.Server)
		if err == nil && (!ok || got.OrgID != p.OrgID) {
			t.Fatalf("failed replacement cleared selection: %+v", got)
		}

		return err
	}); err != nil {
		t.Fatal(err)
	}
}
