package core

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
)

// fakeAWS is the documented example access key id, not a credential.
const fakeAWS = "AKIAIOSFODNN7EXAMPLE"

func newScrubService(t *testing.T, on bool) *Service {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := &testClock{t: time.Date(2026, 9, 27, 10, 0, 1, 0, time.UTC)}
	return New(db, Config{Version: "4.0.0", Features: Features, ScrubIngest: on}, WithClock(clock.now))
}

func TestCreateScrubIngest(t *testing.T) {
	t.Parallel()
	body := `{"kind":"note","summary":"leaked ` + fakeAWS + `","payload":{"log":"Authorization: Bearer ` +
		strings.Repeat("t", 20) + `","n":1.0,"list":["` + fakeAWS + `"]},"context":{"cmd":"password=x"}}`
	scrubbed := `{"kind":"note","summary":"leaked [REDACTED:aws_access_key]","payload":{"log":"Authorization: Bearer [REDACTED:bearer_token]","n":1.0,"list":["[REDACTED:aws_access_key]"]}}`

	s := newScrubService(t, true)
	first := mustCreate(t, s, body)
	if !first.Created {
		t.Fatal("not created")
	}
	if got := first.Scrubbed.String(); got != "assignment 1, aws_access_key 2, bearer_token 1" {
		t.Errorf("Scrubbed = %q", got)
	}
	rec := first.Record
	if rec.Summary != "leaked [REDACTED:aws_access_key]" || strings.Contains(string(rec.Payload), fakeAWS) ||
		!strings.Contains(string(rec.Payload), "Bearer [REDACTED:bearer_token]") || !strings.Contains(string(rec.Payload), `"n":1.0`) ||
		!strings.Contains(string(rec.Context), "password=[REDACTED:assignment]") {
		t.Fatalf("stored summary %q payload %s context %s", rec.Summary, rec.Payload, rec.Context)
	}
	env, _, err := envelope.Decode([]byte(scrubbed))
	if err != nil {
		t.Fatal(err)
	}
	if rec.ContentHash != env.ContentHash() {
		t.Errorf("content_hash %s, want the scrubbed content's %s", rec.ContentHash, env.ContentHash())
	}
	if again := mustCreate(t, s, body); again.Created || again.Record.ID != rec.ID {
		t.Errorf("identical submission not deduped: created %v id %d", again.Created, again.Record.ID)
	}
	out, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "crubbed") || strings.Contains(string(out), "aws_access_key 2") {
		t.Errorf("response body exposes the counts: %s", out)
	}

	off := newScrubService(t, false)
	plain := mustCreate(t, off, body)
	if plain.Scrubbed.Total() != 0 || !strings.Contains(plain.Record.Summary, fakeAWS) || !strings.Contains(string(plain.Record.Payload), fakeAWS) ||
		!strings.Contains(string(plain.Record.Context), "password=x") {
		t.Errorf("scrubbing off changed the content: %q %s", plain.Record.Summary, plain.Record.Payload)
	}
}

func TestCreateScrubIngestSummaryTruncation(t *testing.T) {
	t.Parallel()
	s := newScrubService(t, true)
	// The key straddles the 2000-byte summary limit: it is redacted before
	// the cut, never cut into a partial secret.
	summary := strings.Repeat("a", 1990) + " " + fakeAWS
	res := mustCreate(t, s, `{"kind":"note","summary":"`+summary+`","payload":{}}`)
	if res.Scrubbed["aws_access_key"] != 1 {
		t.Fatalf("Scrubbed = %v", res.Scrubbed)
	}
	if got := len(res.Record.Summary); got > 2000 {
		t.Fatalf("summary is %d bytes", got)
	}
	if strings.Contains(res.Record.Summary, "AKIA") || !strings.HasSuffix(res.Record.Summary, " [REDACTED") {
		t.Errorf("summary tail %q", res.Record.Summary[1980:])
	}
}

func TestCreateScrubIngestInvalidJSON(t *testing.T) {
	t.Parallel()
	s := newScrubService(t, true)
	_, err := s.Create(context.Background(), []byte(`{"summary":"`+fakeAWS))
	if p := asProblem(t, err); p.Status != 400 {
		t.Fatalf("problem %+v", p)
	}
}
