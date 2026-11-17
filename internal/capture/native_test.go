package capture

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

func nativeJournal(harness, text string) string {
	version := 3
	return fmt.Sprintf(`{"type":"session","version":%d,"id":"native-session","cwd":%q,"timestamp":"2026-11-01T00:00:00Z"}`+"\n"+`{"type":"message","id":"user","parentId":null,"message":{"role":"user","content":%q,"timestamp":1793491200000}}`+"\n", version, pushTestCwd, text)
}

func TestNativeCaptureMonotonicRevisions(t *testing.T) {
	for _, harness := range []string{"omp", "pi"} {
		t.Run(harness, func(t *testing.T) {
			f := newPushFixture(t)
			root := filepath.Join(f.env.Home, harness, "sessions")
			if harness == "omp" {
				f.env.OMPSessionsDir = root
			} else {
				f.env.PiSessionsDir = root
			}
			path := filepath.Join(root, "project", "native.jsonl")
			opts := f.options(harness + ":native-session")
			var previousKey string
			for i, text := range []string{"original message", "rewritten message", "rewritten messagE", "small"} {
				f.write(path, nativeJournal(harness, text))
				f.completed = false
				result := f.push(opts)
				want := fmt.Sprint(i + 1)
				if len(result) != 1 || result[0].Status != "pushed" || result[0].Revision != want {
					t.Fatalf("snapshot %d: %+v", i, result)
				}
				envelope := f.manifest["envelope"].(map[string]any)
				if envelope["harness"] != harness || envelope["key"] == previousKey {
					t.Fatalf("source identity: %+v", envelope)
				}
				previousKey = envelope["key"].(string)
				if f.manifest["revision"] != want || !strings.Contains(f.parts[1], text) {
					t.Fatalf("transport snapshot: %+v %q", f.manifest, f.parts[1])
				}
				summary, err := PushSummary(context.Background(), f.db, f.env, f.policy())
				if err != nil || summary.Pushed != 1 || summary.Pending != 0 {
					t.Fatalf("uploaded native summary: %+v, %v", summary, err)
				}
				calls := len(f.calls)
				replay := f.push(opts)
				if len(replay) != 1 || replay[0].Status != "replayed" || replay[0].Revision != want {
					t.Fatalf("exact replay: %+v", replay)
				}
				for _, stage := range f.calls[calls:] {
					if stage == "begin" || stage == "part" || stage == "complete" {
						t.Fatalf("replay uploaded: %v", f.calls[calls:])
					}
				}
			}
		})
	}
}

func TestNativeSuppressionDoesNotReserve(t *testing.T) {
	for _, harness := range []string{"omp", "pi"} {
		for _, status := range []string{"pushed", "replay_mismatch", "too_large"} {
			t.Run(harness+"/"+status, func(t *testing.T) {
				f := newPushFixture(t)
				root := filepath.Join(f.env.Home, harness, "sessions")
				if harness == "omp" {
					f.env.OMPSessionsDir = root
				} else {
					f.env.PiSessionsDir = root
				}
				path := filepath.Join(root, "project", "native.jsonl")
				f.write(path, nativeJournal(harness, "original message"))
				opts := f.options(harness + ":native-session")
				f.push(opts)
				p := f.policy()
				var rows []store.SessionPush
				if err := f.db.Write(context.Background(), func(q store.Querier) error {
					var err error
					rows, err = store.ListSessionPushes(context.Background(), q, policyScope(p))
					if err != nil {
						return err
					}
					if len(rows) != 1 {
						t.Fatalf("initial rows: %+v", rows)
					}
					rows[0].Status = status
					rows[0].MaxSessionBytes = int64(p.MaxSessionBytes)
					return store.SetSessionPush(context.Background(), q, rows[0])
				}); err != nil {
					t.Fatal(err)
				}
				got := f.push(opts)
				want := "already_pushed"
				if status != "pushed" {
					want = status
				}
				if len(got) != 1 || got[0].Reason != want {
					t.Fatalf("suppression: %+v", got)
				}
				if err := f.db.Read(context.Background(), func(q store.Querier) error {
					var err error
					rows, err = store.ListSessionPushes(context.Background(), q, policyScope(p))
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0].Status != status || rows[0].Revision != "1" {
					t.Fatalf("suppression changed durable rows: %+v", rows)
				}
				summary, err := PushSummary(context.Background(), f.db, f.env, p)
				if err != nil || summary.Pending != 0 {
					t.Fatalf("suppressed summary: %+v, %v", summary, err)
				}
			})
		}
	}
}

func TestNativeBatchLimitDoesNotReserve(t *testing.T) {
	for _, harness := range []string{"omp", "pi"} {
		t.Run(harness, func(t *testing.T) {
			f := newPushFixture(t)
			root := filepath.Join(f.env.Home, harness, "sessions")
			if harness == "omp" {
				f.env.OMPSessionsDir = root
			} else {
				f.env.PiSessionsDir = root
			}
			for _, sid := range []string{"native-first", "native-second"} {
				body := strings.ReplaceAll(nativeJournal(harness, "message"), "native-session", sid)
				f.write(filepath.Join(root, "project", sid+".jsonl"), body)
			}
			opts := f.options()
			opts.Limit = 1
			result := f.push(opts)
			pushed, limited := 0, 0
			for _, row := range result {
				if row.Status == "pushed" {
					pushed++
				}
				if row.Reason == "batch_limit" {
					limited++
				}
			}
			if pushed != 1 || limited != 1 {
				t.Fatalf("batch outcomes: %+v", result)
			}
			p := f.policy()
			if err := f.db.Read(context.Background(), func(q store.Querier) error {
				rows, err := store.ListSessionPushes(context.Background(), q, policyScope(p))
				if err == nil && (len(rows) != 1 || rows[0].Status != "pushed") {
					t.Fatalf("batch reserved unselected candidate: %+v", rows)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
