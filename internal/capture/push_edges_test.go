package capture

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
)

func TestPushGrowthPartialLinesAndSourceMutation(t *testing.T) {
	f := newPushFixture(t)
	path, body := f.basic("capture")
	first := f.push(f.options("claude-code:capture"))[0]
	f.write(path, body+`{"type":"user"`)
	out := f.push(f.options("claude-code:capture"))
	if out[0].Status != "replayed" || out[0].Revision != first.Revision {
		t.Fatalf("partial line changed revision %+v", out)
	}
	entry := `{"type":"user","uuid":"new","sessionId":"capture","cwd":"` + pushTestCwd + `","timestamp":"2026-11-08T12:00:00Z","message":{"role":"user","content":"new complete prompt"}}` + "\n"
	f.write(path, body+entry)
	f.completed = false
	f.parts = map[int]string{}
	out = f.push(f.options("claude-code:capture"))
	if out[0].Status != "pushed" || out[0].Revision == first.Revision || !strings.Contains(f.parts[1], "new complete prompt") {
		t.Fatalf("growth did not resend whole session %+v", out)
	}
	f2 := newPushFixture(t)
	p, b := f2.basic("capture")
	opens := 0
	f2.env.Open = func(path string) (io.ReadCloser, error) {
		opens++
		if opens == 2 {
			if err := os.WriteFile(p, []byte(b+"changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return os.Open(path)
	}
	out = f2.push(f2.options("claude-code:capture"))
	if out[0].Status != "deferred" || out[0].Reason != "source_changed" {
		t.Fatalf("mutation %+v", out)
	}
	for _, stage := range f2.calls {
		if stage == "begin" || stage == "part" {
			t.Fatal("changed source uploaded")
		}
	}
}

func TestPushTooLargeAccountingSuppressionAndCapGrowth(t *testing.T) {
	f := newPushFixture(t)
	f.basic("capture")
	p, err := Refresh(context.Background(), f.db, f.client, f.options().Policy)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := sessions.CaptureCandidates(context.Background(), f.db, f.env, []string{"claude-code:capture"})
	if err != nil || len(cs) != 1 {
		t.Fatalf("candidates %v", err)
	}
	p.MaxSessionBytes = 100
	r := pushRun{ctx: context.Background(), db: f.db, client: f.client, policy: p, opts: f.options(), clock: func() time.Time { return f.now }, deadline: f.now.Add(time.Minute)}
	result, _, err := r.pushOne(f.env, cs[0])
	if err != nil || result.Status != "skipped" || result.Reason != "too_large" {
		t.Fatalf("too_large %+v %v", result, err)
	}
	var rows []store.SessionPush
	if err := f.db.Read(context.Background(), func(q store.Querier) error {
		var err error
		rows, err = store.ListSessionPushes(context.Background(), q, policyScope(p))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, reason := suppressed(cs[0], rows, 100); reason != "too_large" {
		t.Fatal("cap suppression not recorded")
	}
	if _, reason := suppressed(cs[0], rows, maxEnvelope); reason != "" {
		t.Fatal("cap growth did not reopen source")
	}
	r.policy.MaxSessionBytes = maxEnvelope
	result, _, err = r.pushOne(f.env, cs[0])
	if err != nil || result.Status != "pushed" {
		t.Fatalf("cap growth %+v %v", result, err)
	}
	assembled := map[string]any{"payload": map[string]any{}}
	if assembledBytes(assembled, strings.Repeat("\x00", maxEnvelope/6)) <= maxEnvelope {
		t.Fatal("assembled accounting ignored JSON escaping")
	}
}

func TestPushRetryAfterStagesAndStopRemainingNamed(t *testing.T) {
	for _, stage := range []string{"meta", "begin", "part", "complete"} {
		for _, header := range []string{"", "invalid", "-3", "0", "120", "Sun, 08 Nov 2026 12:05:00 GMT"} {
			t.Run(stage+header, func(t *testing.T) {
				f := newPushFixture(t)
				f.basic("first")
				f.basic("second")
				f.hook = func(r *http.Request, current string) (*http.Response, error) {
					if current == stage {
						return boundaryResponse(429, `{"message":"not printable"}`, header), nil
					}
					return nil, nil
				}
				out := f.push(f.options("claude-code:first", "claude-code:second"))
				if len(out) != 2 || out[0].Status != "suspended" || out[1].Status != "deferred" {
					t.Fatalf("batch outcomes %+v", out)
				}
				p := f.policy()
				want := f.now.Add(time.Minute)
				if header == "120" {
					want = f.now.Add(120 * time.Second)
				}
				if strings.Contains(header, "GMT") {
					want = f.now.Add(5 * time.Minute)
				}
				if !p.RetryAt.Equal(want) {
					t.Fatalf("retry-at %s want %s", p.RetryAt, want)
				}
				before := len(f.calls)
				again := f.push(f.options("claude-code:first"))
				if again[0].Status != "suspended" || len(f.calls) != before {
					t.Fatal("suspended run contacted server")
				}
			})
		}
	}
}

func TestPushContextRecordedRepositoryAndEmptyEnvironment(t *testing.T) {
	f := newPushFixture(t)
	repo := filepath.Join(t.TempDir(), "recorded-repository")
	for name, body := range map[string]string{".git/HEAD": "ref: refs/heads/capture-test\n", ".git/refs/heads/capture-test": strings.Repeat("a", 40) + "\n", ".git/config": "[core]\nrepositoryformatversion = 0\n[remote \"origin\"]\nurl = https://example.invalid/project.git\n", ".agentfeedback.toml": "[context]\ndrop = [\"branch\", \"cwd\"]\n"} {
		f.write(filepath.Join(repo, name), body)
	}
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "process-session")
	t.Setenv("OPENCODE_MODEL", "process-model")
	c := sessions.CaptureCandidate{Harness: "claude-code", SessionID: "recorded-session", Reader: "claude-code-jsonl", Cwd: repo, Cwds: []string{repo}, Size: 123}
	m := sessions.CaptureMeta{Format: "claude-code-jsonl", Model: "recorded-model", End: f.now}
	opts := f.options()
	opts.WithCwd = true
	e, _, allowed := captureEnvelope(f.env, c, m, "123", opts)
	if !allowed {
		t.Fatal("repository refused")
	}
	ctx := e["context"].(map[string]any)
	if e["project"] != "project" || e["model"] != "recorded-model" || ctx["session_id"] != "recorded-session" || ctx["session_harness"] != "claude-code" {
		t.Fatalf("recorded context %+v", e)
	}
	for _, key := range []string{"branch", "cwd", "profile", "effort"} {
		if _, exists := ctx[key]; exists {
			t.Fatalf("repository narrowing/process context leaked %s", key)
		}
	}
	encoded := string(canonjson.Marshal(e))
	if strings.Contains(encoded, "process-session") || strings.Contains(encoded, "process-model") {
		t.Fatal("process environment leaked")
	}
	f.write(filepath.Join(repo, ".agentfeedback.toml"), "[sessions]\npush = false\n")
	if _, _, allowed := captureEnvelope(f.env, c, m, "123", opts); allowed {
		t.Fatal("repository capture veto ignored")
	}
}

func TestPushDueOldestEligibleEndFirst(t *testing.T) {
	f := newPushFixture(t)
	f.basic("oldest")
	body := testFixtureText(t, "basic.jsonl", "newer", pushTestCwd)
	f.claude("newer", strings.ReplaceAll(body, "2026-10-08T07:", "2026-10-08T08:"))
	f.claude("refused", "unsupported\n")
	opts := f.options()
	opts.Limit = 1
	out := f.push(opts)
	if len(out) == 0 {
		t.Fatal("no selected result")
	}
	if f.manifest["session_ref"] != "claude-code:oldest" {
		t.Fatalf("selected %+v", f.manifest["session_ref"])
	}
}

func TestPushSummaryTooLargeOnlyAndOfflineCredentialMiss(t *testing.T) {
	f := newPushFixture(t)
	f.basic("capture")
	f.basic("refused")
	p, err := Refresh(context.Background(), f.db, f.client, f.options().Policy)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := sessions.CaptureCandidates(context.Background(), f.db, f.env, []string{"claude-code:capture"})
	if err != nil {
		t.Fatal(err)
	}
	r := pushRun{ctx: context.Background(), db: f.db, policy: p, clock: func() time.Time { return f.now }}
	if err := r.record(cs[0], outcome(cs[0], "skipped", "too_large"), ""); err != nil {
		t.Fatal(err)
	}
	f.env.Policy.DenyPaths = []string{pushTestCwd}
	st, err := PushSummary(context.Background(), f.db, f.env, p)
	if err != nil || st.Skipped != 0 {
		t.Fatalf("refused counted as too_large %+v %v", st, err)
	}
	f.env.Policy.DenyPaths = nil
	st, err = PushSummary(context.Background(), f.db, f.env, p)
	if err != nil || st.Skipped != 1 {
		t.Fatalf("matching too_large %+v %v", st, err)
	}
	p.credentialFingerprint = "other-key"
	out, err := PushedRevisions(context.Background(), f.db, p)
	if err != nil || len(out) != 0 {
		t.Fatalf("credential miss %+v %v", out, err)
	}
	// Public result JSON contains no private policy scope material.
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "other-key") {
		t.Fatal("private scope serialized")
	}
}
