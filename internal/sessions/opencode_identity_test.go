package sessions

import (
	"path/filepath"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

func TestOpenCodeLegacyIdentityRoundTrip(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.create(defaultCwd)
	legacy := "opencode:" + ocSID
	canonical := "opencode-v1:" + ocSID
	out := w.digest(DigestRequest{Refs: []string{legacy}})
	if len(out.Sessions) != 1 || out.Sessions[0].Ref != canonical || out.Sessions[0].Harness != "opencode-v1" {
		t.Fatalf("alias digest: %+v", out)
	}
	if err := Mark(w.ctx, w.db, w.env(), []string{legacy}, OutcomeNothing, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.db.Write(w.ctx, func(q store.Querier) error {
		_, err := q.ExecContext(w.ctx, `UPDATE sessions_seen SET harness = 'opencode'; UPDATE schema_version SET version = 4`)
		if err != nil {
			return err
		}
		return store.SetTriageState(w.ctx, q, SelectionKey, `{"harnesses":["opencode"],"since":"7d","limit":5}`)
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(w.ctx, filepath.Join(filepath.Dir(w.home), "agentfeedback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	w.db = db
	listing, err := List(w.ctx, w.db, w.env(), ListOptions{Harness: "opencode"})
	if err != nil || len(listing.Sessions) != 1 || listing.Sessions[0].Ref != canonical || listing.Sessions[0].State != StateProcessed {
		t.Fatalf("migrated listing: %+v, %v", listing, err)
	}
	st, err := Status(w.ctx, w.db, w.env())
	if err != nil || st.Selection == nil || len(st.Selection.Harnesses) != 1 || st.Selection.Harnesses[0] != "opencode-v1" || st.Selection.Since != "7d" || st.Selection.Limit != 5 {
		t.Fatalf("legacy selection: %+v, %v", st.Selection, err)
	}
	if err := SetSelection(w.ctx, w.db, Selection{Harnesses: []string{"opencode"}, Limit: 2}); err != nil {
		t.Fatal(err)
	}
	st, err = Status(w.ctx, w.db, w.env())
	if err != nil || st.Selection.Harnesses[0] != "opencode-v1" || st.Selection.Limit != 2 {
		t.Fatalf("alias selection: %+v, %v", st.Selection, err)
	}
	if Ref("opencode", ocSID) != canonical || Key("opencode", ocSID, "span", 1, DetectorVersion) != Key("opencode-v1", ocSID, "span", 1, DetectorVersion) {
		t.Fatal("alias ref or finding key differs")
	}
	if _, err := Locate(w.ctx, w.env(), legacy, "prt_0009"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeFindingKeyPreservesLegacyHash(t *testing.T) {
	t.Parallel()
	// SHA-256 of "opencode\000legacy-session\000span\0001\0001",
	// the deployed pre-rename finding identity.
	const want = "session-scan-9941ede96e4f4454307816a4fc41975e474926c9224721da0db069c3aa9d14f7"
	for _, harness := range []string{"opencode", "opencode-v1"} {
		if got := Key(harness, "legacy-session", "span", 1, DetectorVersion); got != want {
			t.Errorf("Key(%q) = %q, want %q", harness, got, want)
		}
	}
}
