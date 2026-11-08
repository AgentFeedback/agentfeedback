package capture

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
	"github.com/agentfeedback/agentfeedback/v4/pkg/client"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

const (
	uploadTestID      = "4f877165-f34c-4e5c-a27d-031427c07c69"
	submissionTestUID = "82c6199d-8a9d-476d-b173-af04d731c198"
	pushTestCwd       = "/nonexistent-af/proj"
)

// pushFixture is a fake authenticated hosted endpoint, not a spool transport.
// Do's configured RoundTripper sees each real serialized protocol request.
type pushFixture struct {
	t                   *testing.T
	db                  *store.DB
	client              *client.Client
	env                 sessions.Env
	now                 time.Time
	data, cache, dbPath string
	calls               []string
	manifest            map[string]any
	parts               map[int]string
	completed           bool
	resume              []int
	org                 string
	enabled             bool
	hook                func(*http.Request, string) (*http.Response, error)
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	base := t.TempDir()
	f := &pushFixture{t: t, now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), data: filepath.Join(base, "data"), cache: filepath.Join(base, "cache"), org: testOrg, enabled: true, parts: map[int]string{}}
	for _, dir := range []string{f.data, f.cache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.dbPath = filepath.Join(f.data, "agentfeedback.db")
	f.db = boundaryDB(t, f.dbPath)
	home := filepath.Join(base, "home")
	f.env = sessions.Env{Home: home, ClaudeConfigDir: filepath.Join(home, ".claude"), CodexHome: filepath.Join(home, ".codex"), DataHome: filepath.Join(home, ".local", "share"), Now: func() time.Time { return f.now }}
	var err error
	f.client, err = client.New(client.Config{URL: boundaryServer, APIKey: "push-test-credential", DataDir: f.data, CacheDir: f.cache, HTTP: &http.Client{Transport: testTransport(f.serve)}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *pushFixture) serve(r *http.Request) (*http.Response, error) {
	stage := "unknown"
	switch {
	case r.URL.Path == policyPath:
		stage = "policy"
	case r.URL.Path == discoveryPath:
		stage = "discovery"
	case r.URL.Path == "/api/v1/meta":
		stage = "meta"
	case r.URL.Path == uploadsPath:
		stage = "begin"
	case strings.Contains(r.URL.Path, "/parts/"):
		stage = "part"
	case strings.HasSuffix(r.URL.Path, "/complete"):
		stage = "complete"
	}
	f.calls = append(f.calls, stage)
	if r.Header.Get("Authorization") != "Bearer push-test-credential" || r.Header.Get("X-Api-Key") != "push-test-credential" {
		f.t.Fatal("missing configured client authentication")
	}
	if f.hook != nil {
		if response, err := f.hook(r, stage); response != nil || err != nil {
			return response, err
		}
	}
	switch stage {
	case "policy":
		return boundaryResponse(200, policyJSON(f.org, f.enabled), ""), nil
	case "discovery":
		return boundaryResponse(200, boundaryDiscovery, ""), nil
	case "meta":
		return boundaryResponse(200, `{"client":{"min_version":"4.0.0"}}`, ""), nil
	case "begin":
		if r.Method != http.MethodPost {
			f.t.Fatal("begin method")
		}
		f.manifest = decodeTestTree(f.t, r.Body)
		received := f.resume
		var submission any
		status := 201
		if len(received) > 0 {
			status = 200
		}
		if f.completed {
			status, submission = 200, "9007199254740993"
			received = make([]int, treeInt(f.manifest["part_count"]))
			for i := range received {
				received[i] = i + 1
			}
		}
		if received == nil {
			received = []int{}
		}
		body, _ := json.Marshal(map[string]any{"id": uploadTestID, "expires_at": f.now.Add(time.Hour).Format(time.RFC3339Nano), "received_parts": received, "submission_id": submission})
		return boundaryResponse(status, string(body), ""), nil
	case "part":
		if r.Method != http.MethodPut {
			f.t.Fatal("part method")
		}
		p := decodeTestTree(f.t, r.Body)
		content := p["content"].(string)
		if p["sha256"] != shaText(content) {
			f.t.Fatal("incorrect part digest")
		}
		_, index, _ := strings.Cut(r.URL.Path, "/parts/")
		n, _ := strconv.Atoi(index)
		f.parts[n] = content
		return boundaryResponse(200, fmt.Sprintf(`{"id":%q,"part":%d}`, uploadTestID, n), ""), nil
	case "complete":
		if r.Method != http.MethodPost || r.Body != nil {
			f.t.Fatal("complete must have an empty request body")
		}
		f.completed = true
		return boundaryResponse(201, fmt.Sprintf(`{"submission":{"id":"9007199254740993","uid":%q},"warnings":[]}`, submissionTestUID), ""), nil
	default:
		f.t.Fatalf("unexpected endpoint %s", r.URL.Path)
	}
	return nil, errors.New("unreachable")
}

func treeInt(v any) int { n, _ := strconv.Atoi(string(v.(json.Number))); return n }
func decodeTestTree(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	d := json.NewDecoder(r)
	d.UseNumber()
	var v map[string]any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *pushFixture) options(refs ...string) PushOptions {
	return PushOptions{Policy: Options{Server: boundaryServer, Clock: func() time.Time { return f.now }}, Refs: refs, Since: time.Unix(1, 0), Config: Config{Quiet: 5 * time.Minute, PushBatch: 5, PushBudget: 90 * time.Second}, ClientVersion: "test"}
}

func (f *pushFixture) push(opts PushOptions) []PushResult {
	f.t.Helper()
	out, err := Push(context.Background(), f.db, f.client, f.env, opts)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *pushFixture) policy() Policy {
	f.t.Helper()
	p, err := CachedPolicy(context.Background(), f.db, f.client, Options{Server: boundaryServer})
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *pushFixture) write(path, body string) string {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
	at := f.now.Add(-time.Hour)
	if err := os.Chtimes(path, at, at); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func testFixtureText(t *testing.T, name, sid, cwd string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "sessions", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer("{{SID}}", sid, "{{CWD}}", cwd).Replace(string(b))
}

func (f *pushFixture) claude(sid, body string) string {
	return f.write(filepath.Join(f.env.ClaudeConfigDir, "projects", "-nonexistent-af-proj", sid+".jsonl"), body)
}

func (f *pushFixture) basic(sid string) (string, string) {
	body := testFixtureText(f.t, "basic.jsonl", sid, pushTestCwd)
	return f.claude(sid, body), body
}

func (f *pushFixture) noLeak(secret string) {
	f.t.Helper()
	for _, root := range []string{f.data, f.cache} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(b, []byte(secret)) {
				f.t.Errorf("secret persisted in %s", filepath.Base(path))
			}
			if strings.Contains(path, "spool") || strings.Contains(path, "rejected") {
				f.t.Errorf("unexpected spool entry")
			}
			return nil
		})
		if err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestPushExactSupportedSourceEnvelopes(t *testing.T) {
	for _, source := range []string{"claude-code", "codex", "opencode-v1"} {
		t.Run(source, func(t *testing.T) {
			f := newPushFixture(t)
			var ref, want, format, model string
			var lines, prompts, calls, toolErrors int
			switch source {
			case "claude-code":
				_, want = f.basic("capture")
				ref = "claude-code:capture"
				format = "claude-code-jsonl"
				model = "claude-test-2"
				lines, prompts, calls, toolErrors = 14, 2, 4, 1
			case "codex":
				const sid = "0199c3a1-0000-7000-8000-00000000000"
				want = testFixtureText(t, filepath.Join("stores", "codex", "sessions", "2026", "10", "08", "rollout-2026-10-08T07-39-40-{{SID}}1.jsonl"), sid, pushTestCwd)
				f.write(filepath.Join(f.env.CodexHome, "sessions", "2026", "10", "08", "rollout-2026-10-08T07-39-40-"+sid+"1.jsonl"), want)
				ref = "codex:" + sid + "1"
				format = "codex-rollout"
				model = "gpt-test-2"
				lines, prompts, calls, toolErrors = 30, 2, 7, 3
			case "opencode-v1":
				p := filepath.Join(f.env.DataHome, "opencode", "opencode.db")
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", store.FileURI(p, nil))
				if err != nil {
					t.Fatal(err)
				}
				fixture := testFixtureText(t, filepath.Join("stores", "opencode", "opencode.sql"), "capture", pushTestCwd)
				_, err = db.Exec(fixture + `; DELETE FROM part; DELETE FROM message;
INSERT INTO message VALUES ('m1','capture',1000,1000,'{"role":"user","time":{"created":1000}}');
INSERT INTO part VALUES ('p1','m1','capture',1000,1000,'{"type":"text","text":"prompt"}');
INSERT INTO message VALUES ('m2','capture',2000,3000,'{"role":"assistant","time":{"created":2000,"completed":3000},"modelID":"last-model"}');
INSERT INTO part VALUES ('p2','m2','capture',2000,3000,'{"type":"text","text":"answer"}');`)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				want = "{\"id\":\"m1\",\"kind\":\"message\",\"role\":\"user\",\"time\":{\"created\":1000}}\n{\"id\":\"p1\",\"kind\":\"part\",\"text\":\"prompt\",\"type\":\"text\"}\n{\"id\":\"m2\",\"kind\":\"message\",\"modelID\":\"last-model\",\"role\":\"assistant\",\"time\":{\"completed\":3000,\"created\":2000}}\n{\"id\":\"p2\",\"kind\":\"part\",\"text\":\"answer\",\"type\":\"text\"}\n"
				ref = "opencode-v1:capture"
				format = "opencode-virtual-log"
				model = "last-model"
				lines, prompts = 4, 1
			}
			out := f.push(f.options(ref))
			if len(out) != 1 || out[0].Status != "pushed" || out[0].SubmissionUID != submissionTestUID {
				t.Fatalf("outcomes: %+v", out)
			}
			want, _ = scrub.String(want)
			if f.parts[1] != want || f.manifest["content_sha256"] != shaText(want) || treeInt(f.manifest["content_bytes"]) != len(want) {
				t.Fatal("exact transcript/hash/byte mismatch")
			}
			e := f.manifest["envelope"].(map[string]any)
			p := e["payload"].(map[string]any)
			if _, exists := p["transcript"]; exists {
				t.Fatal("manifest contained transcript")
			}
			if e["kind"] != "session" || e["harness"] != source || e["model"] != model || e["project"] != "proj" || p["format"] != format || treeInt(p["line_count"]) != lines {
				t.Fatalf("envelope metadata: %+v", e)
			}
			counts := p["counts"].(map[string]any)
			if treeInt(counts["prompts"]) != prompts || treeInt(counts["tool_calls"]) != calls || treeInt(counts["tool_errors"]) != toolErrors {
				t.Fatalf("counts: %+v", counts)
			}
			p["transcript"] = want
			if len(canonjson.Marshal(e)) != treeInt(f.manifest["envelope_bytes"]) {
				t.Fatal("noncanonical assembled accounting")
			}
			delete(p, "transcript")
			before := len(f.calls)
			out = f.push(f.options(ref))
			if out[0].Status != "replayed" {
				t.Fatalf("repeat: %+v", out)
			}
			for _, stage := range f.calls[before:] {
				if stage == "part" || stage == "begin" {
					t.Fatal("local replay uploaded content")
				}
			}
			if err := f.db.Read(context.Background(), func(q store.Querier) error {
				rows, err := store.ListSessions(context.Background(), q, source)
				if len(rows) != 0 {
					t.Fatal("push wrote digest watermark")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPushWholeTextScrubDryRunAndNoPersistence(t *testing.T) {
	f := newPushFixture(t)
	secret := "ghp_" + strings.Repeat("x", 36)
	_, body := f.basic("secret")
	body = strings.Replace(body, "Done.", secret, 1)
	f.claude("secret", body)
	var logs []string
	opts := f.options("claude-code:secret")
	opts.Log = func(s string) { logs = append(logs, s) }
	opts.DryRun = true
	out := f.push(opts)
	if len(out) != 1 || out[0].Status != "dry-run" || len(out[0].Preview) > 2048 || !utf8.ValidString(out[0].Preview) {
		t.Fatalf("dry run %+v", out)
	}
	for _, stage := range f.calls {
		if stage == "begin" || stage == "part" || stage == "complete" {
			t.Fatal("dry run made upload request")
		}
	}
	f.noLeak(secret)
	opts.DryRun = false
	f.push(opts)
	if strings.Contains(f.parts[1], secret) || !strings.Contains(f.parts[1], "[REDACTED:github_token]") {
		t.Fatal("whole text scrub missing")
	}
	for _, line := range logs {
		if strings.Contains(line, secret) || strings.Contains(line, "Done.") || strings.Contains(line, "claude-code:secret") {
			t.Fatal("log leaked content/identity")
		}
	}
	f.noLeak(secret)
}

func TestPushPartitionEscapingUTF8AndWholeBlockScrub(t *testing.T) {
	for _, text := range []string{strings.Repeat("é", maxRawPart), strings.Repeat("\x00", maxRawPart), strings.Repeat("line\n", maxRawPart/3)} {
		parts := splitTranscript(text, maxRawPart)
		if len(parts) < 2 || strings.Join(parts, "") != text {
			t.Fatal("partition does not reconstruct text")
		}
		for _, part := range parts {
			if len(part) > maxRawPart || !utf8.ValidString(part) || len(canonjson.Marshal(map[string]any{"content": part, "sha256": shaText(part)})) > maxRequest {
				t.Fatal("unsafe request partition")
			}
		}
	}
	text := "before\n-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("a\n", maxRawPart) + "-----END PRIVATE KEY-----\nafter\n"
	clean, _ := scrub.String(text)
	if strings.Contains(clean, "BEGIN PRIVATE KEY") || !strings.Contains(clean, "[REDACTED:private_key]") || len(splitTranscript(clean, 64)) == 0 {
		t.Fatal("multi-part private block was not scrubbed whole")
	}
}

func TestPushFailureStatusMatrixAndNoServerLeaks(t *testing.T) {
	for _, stage := range []string{"begin", "part", "complete"} {
		for _, status := range []int{400, 401, 403, 404, 405, 409, 413, 429, 500, 503} {
			t.Run(fmt.Sprintf("%s-%d", stage, status), func(t *testing.T) {
				f := newPushFixture(t)
				f.basic("capture")
				secret := "server-body-credential"
				f.hook = func(r *http.Request, current string) (*http.Response, error) {
					if current == stage {
						return boundaryResponse(status, `{"message":"`+secret+`","request_id":"`+secret+`"}`, "120"), nil
					}
					return nil, nil
				}
				out := f.push(f.options("claude-code:capture"))
				if len(out) != 1 {
					t.Fatalf("outcomes %+v", out)
				}
				want := "error"
				if status == 413 {
					want = "skipped"
				}
				if status == 429 {
					want = "suspended"
				}
				if status >= 500 {
					want = "deferred"
				}
				if out[0].Status != want {
					t.Fatalf("outcome %+v", out)
				}
				encoded, _ := json.Marshal(out)
				if strings.Contains(string(encoded), secret) {
					t.Fatal("server payload leaked")
				}
				f.noLeak(secret)
				if status == 429 {
					p := f.policy()
					if p.State != Suspended || p.Enabled || !p.RetryAt.Equal(f.now.Add(120*time.Second)) {
						t.Fatalf("suspension %+v", p)
					}
				}
				if status == 409 || status == 413 {
					before := len(f.calls)
					f.hook = nil
					again := f.push(f.options("claude-code:capture"))
					if again[0].Status != want {
						t.Fatalf("suppression %+v", again)
					}
					for _, s := range f.calls[before:] {
						if s == "begin" || s == "part" || s == "complete" {
							t.Fatal("suppressed revision retried")
						}
					}
				}
			})
		}
	}
}

func TestPushTransportAndMalformedReceipts(t *testing.T) {
	for _, stage := range []string{"begin", "part", "complete"} {
		for _, body := range []string{"network", `{`, `{"id":"not-a-uuid","expires_at":"bad","received_parts":[],"submission_id":null}`, `{"id":"` + uploadTestID + `","expires_at":"2099-01-01T00:00:00Z","received_parts":[1,1],"submission_id":null}`} {
			t.Run(stage+body, func(t *testing.T) {
				f := newPushFixture(t)
				f.basic("capture")
				f.hook = func(r *http.Request, current string) (*http.Response, error) {
					if current != stage {
						return nil, nil
					}
					if body == "network" {
						return nil, errors.New("secret network details")
					}
					return boundaryResponse(200, body, ""), nil
				}
				out := f.push(f.options("claude-code:capture"))
				want := "error"
				if body == "network" {
					want = "deferred"
				}
				if len(out) != 1 || out[0].Status != want {
					t.Fatalf("outcomes %+v", out)
				}
				f.noLeak("secret network details")
			})
		}
	}
}

func TestPushCompletedBeginAndPartialResume(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(strconv.FormatBool(completed), func(t *testing.T) {
			f := newPushFixture(t)
			_, body := f.basic("capture")
			f.completed = completed
			f.resume = []int{1}
			f.parts[1], _ = scrub.String(body)
			out := f.push(f.options("claude-code:capture"))
			want := "pushed"
			if completed {
				want = "replayed"
			}
			if out[0].Status != want {
				t.Fatalf("resume %+v", out)
			}
			for _, stage := range f.calls {
				if stage == "part" {
					t.Fatal("already received part uploaded")
				}
				if completed && stage == "complete" {
					t.Fatal("completed begin issued complete")
				}
			}
		})
	}
}

func TestPushConsentCutoffQuietNamedAndLimit(t *testing.T) {
	f := newPushFixture(t)
	p, _ := f.basic("old")
	f.basic("new")
	if err := os.Chtimes(p, f.now, f.now); err != nil {
		t.Fatal(err)
	}
	opts := f.options()
	opts.Since = time.Time{}
	out := f.push(opts)
	for _, r := range out {
		if r.Status == "pushed" {
			t.Fatal("consent cutoff widened")
		}
	}
	opts = f.options()
	opts.Limit = 1
	out = f.push(opts)
	pushed := 0
	for _, r := range out {
		if r.Status == "pushed" {
			pushed++
		}
		if r.Ref == "claude-code:old" && r.Status != "deferred" {
			t.Fatal("due ignored quiet")
		}
	}
	if pushed != 1 {
		t.Fatalf("eligible limit %+v", out)
	}
	f.completed = false
	f.parts = map[int]string{}
	out = f.push(f.options("claude-code:old"))
	if out[0].Status != "pushed" {
		t.Fatalf("named quiet override %+v", out)
	}
}

func TestPushPolicyRefusalPrecedesAnySourceRead(t *testing.T) {
	for _, policyDisabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(policyDisabled), func(t *testing.T) {
			f := newPushFixture(t)
			f.basic("capture")
			opens := 0
			f.env.Open = func(path string) (io.ReadCloser, error) { opens++; return os.Open(path) }
			if policyDisabled {
				f.enabled = false
			} else {
				f.env.Policy = collect.Policy{Disabled: true}
			}
			out := f.push(f.options("claude-code:capture"))
			if len(out) != 1 || out[0].Status != "skipped" || opens != 0 {
				t.Fatalf("refusal %+v opens=%d", out, opens)
			}
			for _, s := range f.calls {
				if s == "begin" || s == "part" {
					t.Fatal("refused source uploaded")
				}
			}
		})
	}
}

func TestPushBudgetStopsBeforeNextStage(t *testing.T) {
	f := newPushFixture(t)
	f.basic("capture")
	f.hook = func(r *http.Request, stage string) (*http.Response, error) {
		if stage == "begin" {
			f.now = f.now.Add(91 * time.Second)
		}
		return nil, nil
	}
	out := f.push(f.options("claude-code:capture"))
	if out[0].Status != "deferred" || out[0].Reason != "push_budget" {
		t.Fatalf("budget %+v", out)
	}
	for _, s := range f.calls {
		if s == "part" || s == "complete" {
			t.Fatal("stage after budget")
		}
	}
}

func TestPushWatermarkSuppressionGrowthRotationAndCap(t *testing.T) {
	c := sessions.CaptureCandidate{Harness: "claude-code", SessionID: "s", Size: 100, Head: "h"}
	row := store.SessionPush{Harness: c.Harness, SessionID: c.SessionID, Revision: "100", SourceSize: 100, Head: "h", Status: "too_large", MaxSessionBytes: 200}
	for _, tc := range []struct {
		name string
		size int64
		head string
		cap  int
		want string
	}{{"same", 100, "h", 200, "too_large"}, {"larger", 101, "h", 200, "too_large"}, {"smaller", 99, "h", 200, ""}, {"rotated", 100, "new", 200, ""}, {"cap grows", 100, "h", 201, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			c.Size, c.Head = tc.size, tc.head
			_, reason := suppressed(c, []store.SessionPush{row}, tc.cap)
			if reason != tc.want {
				t.Fatalf("suppression %q", reason)
			}
		})
	}
	row.Status = "replay_mismatch"
	c.Size, c.Head = 100, "new"
	if _, reason := suppressed(c, []store.SessionPush{row}, 200); reason != "replay_mismatch" {
		t.Fatal("same-size mismatch retried")
	}
}

func TestPushCachedScopeSummaryAndCredentialIsolation(t *testing.T) {
	f := newPushFixture(t)
	f.basic("capture")
	f.push(f.options("claude-code:capture"))
	p := f.policy()
	before := len(f.calls)
	m, err := PushedRevisions(context.Background(), f.db, p)
	if err != nil || m["claude-code:capture"] == "" {
		t.Fatalf("pushed revisions %+v %v", m, err)
	}
	if len(f.calls) != before {
		t.Fatal("offline list performed HTTP")
	}
	st, err := PushSummary(context.Background(), f.db, f.env, p)
	if err != nil || st.Pushed != 1 || st.Pending != 0 || st.LastPush == nil {
		t.Fatalf("summary %+v %v", st, err)
	}
	for _, mutate := range []func(*Policy){func(p *Policy) { p.Server = "https://other.example" }, func(p *Policy) { p.OrgID = otherOrg }, func(p *Policy) { p.credentialFingerprint = "other" }, func(p *Policy) { p.Since = p.Since.Add(time.Second) }} {
		other := p
		mutate(&other)
		m, err := PushedRevisions(context.Background(), f.db, other)
		if err != nil || len(m) != 0 {
			t.Fatalf("cross-scope revisions %+v %v", m, err)
		}
	}
	f.org = otherOrg
	f.completed = false
	f.parts = map[int]string{}
	out := f.push(f.options("claude-code:capture"))
	if out[0].Status != "pushed" {
		t.Fatalf("org change inherited watermark %+v", out)
	}
}

func TestPush429DoesNotReplaceFreshConsent(t *testing.T) {
	f := newPushFixture(t)
	f.basic("capture")
	f.hook = func(r *http.Request, stage string) (*http.Response, error) {
		if stage != "begin" {
			return nil, nil
		}
		// Simulate a refresh begun after the upload's policy was selected.
		err := f.db.Write(context.Background(), func(q store.Querier) error {
			row, _, err := store.CurrentCapturePolicy(context.Background(), q, boundaryServer)
			if err != nil {
				return err
			}
			row.Generation, err = store.NextCaptureGeneration(context.Background(), q, boundaryServer)
			if err != nil {
				return err
			}
			row.State, row.Enabled = Enabled, true
			return store.SetCapturePolicy(context.Background(), q, row)
		})
		if err != nil {
			t.Fatal(err)
		}
		return boundaryResponse(429, `{"message":"private"}`, "60"), nil
	}
	out := f.push(f.options("claude-code:capture"))
	if out[0].Status != "suspended" {
		t.Fatalf("outcome %+v", out)
	}
	if p := f.policy(); p.State != Enabled || !p.Enabled {
		t.Fatalf("stale 429 replaced consent %+v", p)
	}
}

func TestPushPreviewUTF8AndMetadataScrub(t *testing.T) {
	if got := preview(strings.Repeat("é", 2000)); len(got) > 2048 || !utf8.ValidString(got) {
		t.Fatal("invalid UTF-8 preview")
	}
	f := newPushFixture(t)
	c := sessions.CaptureCandidate{Harness: "claude-code", SessionID: "source", Reader: "claude-code-jsonl", Size: 12, Cwd: "/gone/ghp_" + strings.Repeat("x", 36)}
	m := sessions.CaptureMeta{Format: "claude-code-jsonl", End: f.now, Model: "ghp_" + strings.Repeat("x", 36)}
	e, counts, allowed := captureEnvelope(f.env, c, m, "12", f.options())
	if !allowed {
		t.Fatal("unexpected context refusal")
	}
	if counts.Total() != 2 || strings.Contains(string(canonjson.Marshal(e)), strings.Repeat("x", 36)) {
		t.Fatal("metadata not scrubbed")
	}
	if e["key"] != "session-"+shaText("claude-code\x00source\x0012") {
		t.Fatal("source identity changed")
	}
	if !reflect.DeepEqual(e["context"].(map[string]any)["origin"], "session-capture") {
		t.Fatal("capture origin")
	}
}
