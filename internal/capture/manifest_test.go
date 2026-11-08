package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
)

func TestPushResumeKeepsInitialMetadata(t *testing.T) {
	f := newPushFixture(t)
	path, _ := f.basic("stable")
	f.hook = func(_ *http.Request, stage string) (*http.Response, error) {
		if stage == "part" {
			return boundaryResponse(503, `{}`, ""), nil
		}
		return nil, nil
	}
	opts := f.options("claude-code:stable")
	out := f.push(opts)
	if out[0].Reason != "server_unavailable" {
		t.Fatal(out)
	}
	initial := canonjson.Marshal(f.manifest)
	f.hook = nil
	opts.ClientVersion = "new-client-version"
	at := f.now.Add(-30 * time.Minute)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	out = f.push(opts)
	if out[0].Status != "pushed" || !bytes.Equal(initial, canonjson.Marshal(f.manifest)) {
		t.Fatalf("resume changed initial manifest: %+v", out)
	}
	if err := f.db.Read(context.Background(), func(q store.Querier) error {
		var n int
		if err := q.QueryRowContext(context.Background(), `SELECT count(*) FROM session_manifests`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatalf("completed metadata retained: %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPushRechecksRepositoryVetoBeforeParts(t *testing.T) {
	f := newPushFixture(t)
	cwd := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	f.claude("veto", testFixtureText(t, "basic.jsonl", "veto", cwd))
	f.hook = func(_ *http.Request, stage string) (*http.Response, error) {
		if stage == "begin" {
			if err := os.WriteFile(filepath.Join(cwd, ".agentfeedback.toml"), []byte("[sessions]\npush = false\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return nil, nil
	}
	out := f.push(f.options("claude-code:veto"))
	if out[0].Reason != "source_refused" {
		t.Fatal(out)
	}
	for _, call := range f.calls {
		if call == "part" || call == "complete" {
			t.Fatalf("sent after local veto: %v", f.calls)
		}
	}
}

func TestPendingManifestContainsNoTranscript(t *testing.T) {
	f := newPushFixture(t)
	_, source := f.basic("private")
	f.hook = func(_ *http.Request, stage string) (*http.Response, error) {
		if stage == "part" {
			return boundaryResponse(503, `{}`, ""), nil
		}
		return nil, nil
	}
	if out := f.push(f.options("claude-code:private")); out[0].Reason != "server_unavailable" {
		t.Fatal(out)
	}
	if err := f.db.Read(context.Background(), func(q store.Querier) error {
		var raw []byte
		if err := q.QueryRowContext(context.Background(), `SELECT manifest FROM session_manifests`).Scan(&raw); err != nil {
			return err
		}
		var manifest pendingManifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return err
		}
		payload := manifest.Envelope["payload"].(map[string]any)
		if _, exists := payload["transcript"]; exists || bytes.Contains(raw, []byte(source)) {
			t.Fatal("transcript persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
