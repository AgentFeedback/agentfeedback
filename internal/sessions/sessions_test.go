package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/harness"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

var update = flag.Bool("update", false, "rewrite testdata/digest.golden.json")

// defaultCwd is the cwd fixtures record unless a test sets one: it does not
// exist, so only the user rules apply to it.
const defaultCwd = "/nonexistent-af/proj"

type world struct {
	t     *testing.T
	ctx   context.Context
	home  string
	cfg   string
	db    *store.DB
	pol   collect.Policy
	mu    sync.Mutex
	opens map[string]int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	base := t.TempDir()
	w := &world{t: t, ctx: context.Background(), home: filepath.Join(base, "home"), opens: map[string]int{}}
	w.cfg = filepath.Join(w.home, ".claude")
	if err := os.MkdirAll(filepath.Join(w.cfg, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(w.ctx, filepath.Join(base, "agentfeedback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	w.db = db

	return w
}

func (w *world) env() Env {
	return Env{
		Home: w.home, ClaudeConfigDir: w.cfg, Policy: w.pol,
		CodexHome: filepath.Join(w.home, ".codex"), CopilotHome: filepath.Join(w.home, ".copilot"),
		GeminiDir: filepath.Join(w.home, ".gemini"), DataHome: filepath.Join(w.home, ".local", "share"),
		Now: func() time.Time { return time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC) },
		Open: func(p string) (io.ReadCloser, error) {
			w.mu.Lock()
			w.opens[p]++
			w.mu.Unlock()

			return os.Open(p)
		},
	}
}

// fixture reads testdata/name with its placeholders filled.
func fixture(t *testing.T, name, sid, cwd string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	return strings.NewReplacer("{{SID}}", sid, "{{CWD}}", cwd).Replace(string(b))
}

// put writes content as session sid of the project launched in cwd.
func (w *world) put(cwd, sid, content string) string {
	w.t.Helper()
	dir := filepath.Join(w.cfg, "projects", encodeProject(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	p := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}

	return p
}

func (w *world) appendTo(p, content string) {
	w.t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		w.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) list(opts ListOptions) Listing {
	w.t.Helper()
	l, err := List(w.ctx, w.db, w.env(), opts)
	if err != nil {
		w.t.Fatal(err)
	}

	return l
}

func (w *world) session(sid string) Session {
	w.t.Helper()
	for _, s := range w.list(ListOptions{}).Sessions {
		if s.SessionID == sid {
			return s
		}
	}
	w.t.Fatalf("session %s not listed", sid)

	return Session{}
}

func (w *world) digest(req DigestRequest) DigestOutput {
	w.t.Helper()
	out, err := Digest(w.ctx, w.db, w.env(), req)
	if err != nil {
		w.t.Fatal(err)
	}

	return out
}

func (w *world) digestOne(sid string) SessionDigest {
	w.t.Helper()
	out := w.digest(DigestRequest{Refs: []string{Ref(HarnessClaudeCode, sid)}})
	if len(out.Sessions) != 1 {
		w.t.Fatalf("digest of %s: %+v", sid, out)
	}

	return out.Sessions[0]
}

func (w *world) mark(sid string) {
	w.t.Helper()
	if err := Mark(w.ctx, w.db, w.env(), []string{Ref(HarnessClaudeCode, sid)}, OutcomeNothing, nil); err != nil {
		w.t.Fatal(err)
	}
}

// watermarkSize is the size the session's latest digest recorded.
func (w *world) watermarkSize(sid string) int64 {
	w.t.Helper()
	var row store.SessionSeen
	if err := w.db.Read(w.ctx, func(q store.Querier) (err error) {
		row, _, err = store.GetSession(w.ctx, q, HarnessClaudeCode, sid)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}

	return row.Size
}

func spans(d SessionDigest) []string {
	var out []string
	for _, e := range d.Events {
		out = append(out, e.Span)
	}

	return out
}

const newPrompt = `{"type":"user","uuid":"u-0100","timestamp":"2026-10-08T08:00:00.000Z","cwd":"` + defaultCwd + `","message":{"role":"user","content":"Still failing after the fix"}}` + "\n"

func TestStates(t *testing.T) {
	t.Parallel()

	t.Run("new", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		s := w.session("s1")
		if s.State != StateNew || s.Project != defaultCwd || s.Counts == nil || s.Counts.Prompts != 2 || s.Counts.Unparsed != 1 {
			t.Fatalf("%+v %+v", s, s.Counts)
		}
	})
	t.Run("processed", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		w.digestOne("s1")
		w.mark("s1")
		if s := w.session("s1"); s.State != StateProcessed {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("changed", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		p := w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		w.digestOne("s1")
		w.mark("s1")
		w.appendTo(p, newPrompt)
		if s := w.session("s1"); s.State != StateChanged || s.Reason != ChangeAppended {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		p := w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		w.digestOne("s1")
		w.mark("s1")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if s := w.session("s1"); s.State != StateAbsent || s.Path != p {
			t.Fatalf("%+v", s)
		}
		out := w.digest(DigestRequest{Refs: []string{"claude-code:s1"}})
		if len(out.Errors) != 1 || out.Errors[0].State != StateAbsent {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		p := w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		w.pol.Disabled = true
		if s := w.session("s1"); s.State != StateDisabled || s.Counts != nil {
			t.Fatalf("%+v", s)
		}
		out := w.digest(DigestRequest{Refs: []string{"claude-code:s1"}})
		if len(out.Sessions) != 0 || len(out.Errors) != 1 || out.Errors[0].State != StateDisabled {
			t.Fatalf("%+v", out)
		}
		if w.opens[p] != 0 {
			t.Fatalf("disabled file opened %d times", w.opens[p])
		}
	})
	t.Run("denied", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		p := w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		// A sibling whose name extends the denied one is not beneath it.
		other := w.put(defaultCwd+"x", "s2", fixture(t, "basic.jsonl", "s2", defaultCwd+"x"))
		w.pol.DenyPaths = []string{defaultCwd}
		if s := w.session("s1"); s.State != StateDenied || s.Reason != collect.ReasonDenyPaths {
			t.Fatalf("%+v", s)
		}
		if s := w.session("s2"); s.State != StateNew {
			t.Fatalf("sibling: %+v", s)
		}
		out := w.digest(DigestRequest{Refs: []string{"claude-code:s1"}})
		if len(out.Errors) != 1 || out.Errors[0].State != StateDenied {
			t.Fatalf("%+v", out)
		}
		if w.opens[p] != 0 || w.opens[other] == 0 {
			t.Fatalf("opens %v", w.opens)
		}
	})
	t.Run("denied beneath", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		p := w.put(defaultCwd+"/sub", "s1", fixture(t, "basic.jsonl", "s1", defaultCwd+"/sub"))
		w.pol.DenyPaths = []string{defaultCwd}
		if s := w.session("s1"); s.State != StateDenied || w.opens[p] != 0 {
			t.Fatalf("%+v %v", s, w.opens)
		}
	})
	t.Run("denied through a symlink", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		base := t.TempDir()
		real := filepath.Join(base, "real")
		if err := os.Mkdir(real, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink: %v", err)
		}
		// Claude Code records the cwd as its process saw it: resolved.
		cwd, err := filepath.EvalSymlinks(real)
		if err != nil {
			t.Fatal(err)
		}
		p := w.put(cwd, "s1", fixture(t, "basic.jsonl", "s1", cwd))
		w.pol.DenyPaths = []string{link}
		if s := w.session("s1"); s.State != StateDenied || s.Reason != collect.ReasonDenyPaths || w.opens[p] != 0 {
			t.Fatalf("%+v %v", s, w.opens)
		}
	})
	t.Run("unlistable project directory", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a non-root POSIX user")
		}
		w := newWorld(t)
		w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		w.put("/other-af/proj", "s2", fixture(t, "basic.jsonl", "s2", "/other-af/proj"))
		dir := filepath.Join(w.cfg, "projects", encodeProject("/other-af/proj"))
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		l := w.list(ListOptions{Harness: HarnessClaudeCode})
		if len(l.Stores) != 1 || l.Stores[0].State != StorePresent || len(l.Stores[0].Problems) != 1 ||
			!strings.HasPrefix(l.Stores[0].Problems[0], dir+": ") {
			t.Fatalf("%+v", l.Stores)
		}
		if len(l.Sessions) != 1 || l.Sessions[0].SessionID != "s1" {
			t.Fatalf("%+v", l.Sessions)
		}
		st, err := Status(w.ctx, w.db, w.env())
		if err != nil {
			t.Fatal(err)
		}
		if hs := claudeStatus(t, st); !slices.Equal(hs.Problems, l.Stores[0].Problems) {
			t.Fatalf("status %+v, want the listing's problems", st.Harnesses)
		}
	})
	t.Run("denied by cwd after read", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		// Launched elsewhere, then worked in a denied directory.
		content := fixture(t, "basic.jsonl", "s1", defaultCwd) + strings.ReplaceAll(newPrompt, defaultCwd, "/secret-af/x")
		w.put(defaultCwd, "s1", content)
		w.pol.DenyPaths = []string{"/secret-af"}
		s := w.session("s1")
		if s.State != StateDenied {
			t.Fatalf("%+v", s)
		}
		// Refused after the read: nothing read from the file is shown.
		b, err := json.Marshal(w.list(ListOptions{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range []string{`"project"`, `"cwds"`, `"start"`, `"end"`, `"counts"`, "secret-af"} {
			if strings.Contains(string(b), member) {
				t.Fatalf("%s in %s", member, b)
			}
		}
	})
	t.Run("unlisted directory keeps its rows unreadable", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a non-root POSIX user")
		}
		w := newWorld(t)
		w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		w.digestOne("s1")
		dir := filepath.Join(w.cfg, "projects", encodeProject(defaultCwd))
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		l := w.list(ListOptions{})
		if len(l.Sessions) != 1 || l.Sessions[0].State != StateUnreadable || !strings.Contains(l.Sessions[0].Reason, dir) {
			t.Fatalf("%+v", l.Sessions)
		}
		out := w.digest(DigestRequest{Refs: []string{"claude-code:s1"}})
		if len(out.Errors) != 1 || out.Errors[0].State != StateUnreadable {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("deny the root", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		p := w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		w.pol.DenyPaths = []string{string(filepath.Separator)}
		if s := w.session("s1"); s.State != StateDenied || s.Reason != collect.ReasonDenyPaths || w.opens[p] != 0 {
			t.Fatalf("%+v %v", s, w.opens)
		}
	})
	t.Run("relative cwd", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		rel := `{"type":"user","uuid":"u-1","timestamp":"2026-10-08T07:00:00.000Z","cwd":".","message":{"role":"user","content":"hi"}}` + "\n"
		w.put(defaultCwd, "s1", rel)
		if s := w.session("s1"); s.State != StateUnknownProject || len(s.Cwds) != 0 {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("opt-in", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		p := w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		in := w.put("/allowed-af/proj", "s2", fixture(t, "basic.jsonl", "s2", "/allowed-af/proj"))
		w.pol.OptInOnly, w.pol.OptInPaths = true, []string{"/allowed-af"}
		if s := w.session("s1"); s.State != StateDenied || s.Reason != collect.ReasonOptInOnly {
			t.Fatalf("%+v", s)
		}
		if s := w.session("s2"); s.State != StateNew {
			t.Fatalf("%+v", s)
		}
		if w.opens[p] != 0 || w.opens[in] == 0 {
			t.Fatalf("opens %v", w.opens)
		}
	})
	t.Run("unknown-project", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.put(defaultCwd, "s1", fixture(t, "nocwd.jsonl", "s1", ""))
		if s := w.session("s1"); s.State != StateUnknownProject {
			t.Fatalf("%+v", s)
		}
		out := w.digest(DigestRequest{Refs: []string{"claude-code:s1"}})
		if len(out.Errors) != 1 || out.Errors[0].State != StateUnknownProject {
			t.Fatalf("%+v", out)
		}
		out = w.digest(DigestRequest{Refs: []string{"claude-code:s1"}, AllowUnknownProject: true})
		if len(out.Sessions) != 1 || len(out.Errors) != 0 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("mode 000 does not stop root or windows")
		}
		w := newWorld(t)
		p := w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		if s := w.session("s1"); s.State != StateUnreadable {
			t.Fatalf("%+v", s)
		}
		// Denied wins and the file is not even tried.
		w.pol.DenyPaths = []string{defaultCwd}
		before := w.opens[p]
		if s := w.session("s1"); s.State != StateDenied || w.opens[p] != before {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("unsupported-format", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.put(defaultCwd, "s1", fixture(t, "unsupported.jsonl", "s1", defaultCwd))
		if s := w.session("s1"); s.State != StateUnsupportedFormat {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("repo file disables", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		repo := filepath.Join(t.TempDir(), "repo")
		for _, f := range []struct{ p, body string }{
			{filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n"},
			{filepath.Join(repo, collect.RepoFile), "[collect]\ndisabled = true\n"},
		} {
			if err := os.MkdirAll(filepath.Dir(f.p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.p, []byte(f.body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		w.put(repo, "s1", fixture(t, "basic.jsonl", "s1", repo))
		if s := w.session("s1"); s.State != StateDisabled || s.Reason != collect.ReasonRepoDisabled {
			t.Fatalf("%+v", s)
		}
		// The same cwd once gone: only the user rules apply.
		if err := os.RemoveAll(repo); err != nil {
			t.Fatal(err)
		}
		if s := w.session("s1"); s.State != StateNew {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("store absent", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		if err := os.RemoveAll(filepath.Join(w.cfg, "projects")); err != nil {
			t.Fatal(err)
		}
		l := w.list(ListOptions{Harness: HarnessClaudeCode})
		if len(l.Stores) != 1 || l.Stores[0].State != StateAbsent || len(l.Sessions) != 0 {
			t.Fatalf("%+v", l)
		}
	})
}

func TestListFilters(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "old", fixture(t, "basic.jsonl", "old", defaultCwd))
	w.put(defaultCwd, "done", fixture(t, "basic.jsonl", "done", defaultCwd))
	w.digestOne("done")
	w.mark("done")
	late := w.put(defaultCwd, "late", strings.ReplaceAll(fixture(t, "basic.jsonl", "late", defaultCwd), "2026-10-08", "2026-10-09"))
	// Since skips a file unopened by its mtime: give it one after the cut.
	future := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(late, future, future); err != nil {
		t.Fatal(err)
	}

	refs := func(l Listing) string {
		var out []string
		for _, s := range l.Sessions {
			out = append(out, s.SessionID+"="+s.State)
		}

		return strings.Join(out, ",")
	}
	if got := refs(w.list(ListOptions{})); got != "done=processed,old=new,late=new" {
		t.Fatalf("all: %s", got)
	}
	if got := refs(w.list(ListOptions{Unprocessed: true})); got != "old=new,late=new" {
		t.Fatalf("unprocessed: %s", got)
	}
	if got := refs(w.list(ListOptions{Since: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)})); got != "late=new" {
		t.Fatalf("since: %s", got)
	}
	if _, err := List(w.ctx, w.db, w.env(), ListOptions{Harness: "amp"}); err == nil {
		t.Fatal("a harness without a reader must be refused")
	}
}

func TestDigestGolden(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
	out := w.digest(DigestRequest{Refs: []string{"claude-code:s1"}})
	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	golden := filepath.Join("testdata", "digest.golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("digest differs from %s (rerun with -update to accept)\n got:\n%s", golden, got)
	}
}

func TestDigestScrubsSecrets(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
	d := w.digestOne("s1")
	b, _ := json.Marshal(d)
	s := string(b)
	for _, raw := range []string{"AKIAABCDEFGHIJKLMNOP", "ghp_abcdefghijklmnopqrstuvwxyz0123456789"} {
		if strings.Contains(s, raw) {
			t.Fatalf("secret %s survived: %s", raw, s)
		}
	}
	if !strings.Contains(s, "[REDACTED:aws_access_key]") || !strings.Contains(s, "[REDACTED:github_token]") {
		t.Fatalf("markers missing: %s", s)
	}
}

func TestDigestEvents(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
	d := w.digestOne("s1")
	if d.Counts != (Counts{Entries: 13, Prompts: 2, ToolCalls: 4, ToolErrors: 1, Interrupts: 1, Denials: 1, Unparsed: 1}) {
		t.Fatalf("counts %+v", d.Counts)
	}
	if got := strings.Join(spans(d), ","); got != "u-0001,a-0002,a-0006,a-0008,u-0010" {
		t.Fatalf("spans %s", got)
	}
	e := d.Events[1]
	if e.Status != statusError || e.ErrorClass != "exit" || e.ExitCode == nil || *e.ExitCode != 1 || e.Tool != "Bash" {
		t.Fatalf("%+v", e)
	}
	if len(d.Retries) != 1 || d.Retries[0].Span != "a-0004" || d.Retries[0].OfSpan != "a-0002" {
		t.Fatalf("retries %+v", d.Retries)
	}
	if len(d.Denials) != 1 || d.Denials[0] != "a-0006" || d.Events[2].Status != statusDenied {
		t.Fatalf("denials %+v %+v", d.Denials, d.Events[2])
	}
	if d.Events[3].Status != statusInterrupted || d.Model != "claude-test-2" || d.FinalExcerpt != "Done. The build passes now." {
		t.Fatalf("%+v", d)
	}
	var phrases []string
	for _, f := range d.Flagged {
		phrases = append(phrases, f.Span+":"+f.Phrase)
	}
	if got := strings.Join(phrases, ","); got != "u-0001:doesn't work,u-0010:why did you,u-0010:again" {
		t.Fatalf("flagged %s", got)
	}
}

func TestWatermarks(t *testing.T) {
	t.Parallel()

	t.Run("unchanged rerun", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		w.digestOne("s1")
		w.mark("s1")
		if s := w.session("s1"); s.State != StateProcessed {
			t.Fatalf("%+v", s)
		}
		if out := w.digest(DigestRequest{Unprocessed: true}); len(out.Sessions) != 0 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("append", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		content := fixture(t, "basic.jsonl", "s1", defaultCwd)
		p := w.put(defaultCwd, "s1", content)
		w.digestOne("s1")
		w.mark("s1")
		w.appendTo(p, newPrompt)
		out := w.digest(DigestRequest{Unprocessed: true})
		if len(out.Sessions) != 1 {
			t.Fatalf("%+v", out)
		}
		d := out.Sessions[0]
		if d.FromOffset != int64(len(content)) || strings.Join(spans(d), ",") != "u-0100" || d.Counts.Entries != 1 {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("partial tail", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		content := fixture(t, "basic.jsonl", "s1", defaultCwd)
		p := w.put(defaultCwd, "s1", content+strings.TrimSuffix(newPrompt, "\n"))
		d := w.digestOne("s1")
		if strings.Contains(strings.Join(spans(d), ","), "u-0100") || d.Counts.Entries != 13 {
			t.Fatalf("partial line read: %+v", d)
		}
		w.mark("s1")
		if s := w.session("s1"); s.State != StateProcessed {
			t.Fatalf("a partial tail must not count: %+v", s)
		}
		w.appendTo(p, "\n")
		if s := w.session("s1"); s.State != StateChanged || s.Reason != ChangeAppended {
			t.Fatalf("%+v", s)
		}
		d = w.digestOne("s1")
		if d.FromOffset != int64(len(content)) || strings.Join(spans(d), ",") != "u-0100" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("rotation", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		content := fixture(t, "basic.jsonl", "s1", defaultCwd)
		w.put(defaultCwd, "s1", content)
		w.digestOne("s1")
		w.mark("s1")
		w.put(defaultCwd, "s1", newPrompt+content)
		if s := w.session("s1"); s.State != StateChanged || s.Reason != ChangeRotated {
			t.Fatalf("%+v", s)
		}
		if d := w.digestOne("s1"); d.FromOffset != 0 || d.Events[0].Span != "u-0100" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("truncation", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		content := fixture(t, "basic.jsonl", "s1", defaultCwd)
		w.put(defaultCwd, "s1", content)
		w.digestOne("s1")
		w.mark("s1")
		lines := strings.SplitAfter(content, "\n")
		w.put(defaultCwd, "s1", strings.Join(lines[:5], ""))
		if s := w.session("s1"); s.State != StateChanged || s.Reason != ChangeTruncated {
			t.Fatalf("%+v", s)
		}
		if d := w.digestOne("s1"); d.FromOffset != 0 || d.Events[0].Span != "u-0001" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("crash before mark", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		first := w.digestOne("s1")
		if s := w.session("s1"); s.State != StateNew {
			t.Fatalf("%+v", s)
		}
		second := w.digestOne("s1")
		if strings.Join(spans(first), ",") != strings.Join(spans(second), ",") {
			t.Fatalf("spans moved: %v vs %v", spans(first), spans(second))
		}
	})
	t.Run("result after the mark", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		calls := failingCalls(1)
		lines := strings.SplitAfter(calls, "\n")
		p := w.put(defaultCwd, "s1", lines[0])
		if d := w.digestOne("s1"); len(d.Events) != 0 {
			t.Fatalf("%+v", d)
		}
		w.mark("s1")
		w.appendTo(p, lines[1])
		out := w.digest(DigestRequest{Unprocessed: true})
		if len(out.Sessions) != 1 || strings.Join(spans(out.Sessions[0]), ",") != "a-0" || out.Sessions[0].Events[0].Status != statusError {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("truncated digest continues", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.put(defaultCwd, "big", failingCalls(200))
		first := w.digestOne("big")
		if !first.Truncated {
			t.Fatalf("not truncated: %d events", len(first.Events))
		}
		w.mark("big")
		if s := w.session("big"); s.State != StateChanged || s.Reason != ChangeAppended {
			t.Fatalf("%+v", s)
		}
		out := w.digest(DigestRequest{Unprocessed: true})
		if len(out.Sessions) != 1 {
			t.Fatalf("%+v", out)
		}
		next := fmt.Sprintf("a-%d", len(first.Events))
		if !slices.Contains(spans(out.Sessions[0]), next) {
			t.Fatalf("omitted event %s not in the next digest: %v", next, spans(out.Sessions[0]))
		}
	})
	t.Run("three truncated rounds", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		const n = 400
		w.put(defaultCwd, "big", failingCalls(n))
		seen := map[string]bool{}
		var last int64
		for round := range 3 {
			out := w.digest(DigestRequest{Unprocessed: true})
			if len(out.Sessions) != 1 {
				t.Fatalf("round %d: %+v", round, out)
			}
			for _, sp := range spans(out.Sessions[0]) {
				seen[sp] = true
			}
			size := w.watermarkSize("big")
			if size <= last {
				t.Fatalf("round %d: watermark %d after %d", round, size, last)
			}
			last = size
			w.mark("big")
		}
		if len(seen) != n {
			t.Fatalf("covered %d of %d events", len(seen), n)
		}
		if s := w.session("big"); s.State != StateProcessed {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("result at from makes progress", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		lines := strings.SplitAfter(failingCalls(1), "\n")
		p := w.put(defaultCwd, "s1", lines[0])
		w.digestOne("s1")
		w.mark("s1")
		from := w.watermarkSize("s1")
		// The result lands exactly at the marked size, then enough failures
		// to truncate.
		w.appendTo(p, lines[1]+strings.ReplaceAll(failingCalls(300), `"a-`, `"b-`))
		out := w.digest(DigestRequest{Unprocessed: true})
		if len(out.Sessions) != 1 || !out.Sessions[0].Truncated || out.Sessions[0].FromOffset != from ||
			!slices.Contains(spans(out.Sessions[0]), "a-0") {
			t.Fatalf("%+v", out)
		}
		if size := w.watermarkSize("s1"); size <= from {
			t.Fatalf("watermark %d did not pass %d", size, from)
		}
	})
	t.Run("crash after mark", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		w.digestOne("s1")
		w.mark("s1")
		// A new process, same database: the mark holds.
		if s := w.session("s1"); s.State != StateProcessed {
			t.Fatalf("%+v", s)
		}
	})
}

// failingCalls is a session of n failed tool calls with large outputs.
func failingCalls(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"type":"assistant","uuid":"a-%d","timestamp":"2026-10-08T07:00:00.000Z","cwd":%q,"message":{"model":"m","content":[{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"step %d"}}]}}`+"\n", i, defaultCwd, i, i)
		fmt.Fprintf(&b, `{"type":"user","uuid":"u-%d","timestamp":"2026-10-08T07:00:01.000Z","cwd":%q,"message":{"content":[{"type":"tool_result","tool_use_id":"t%d","is_error":true,"content":"Exit code 2\n%s"}]}}`+"\n", i, defaultCwd, i, strings.Repeat("x", 400))
	}

	return b.String()
}

func TestSizeCaps(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "big", failingCalls(200))
	d := w.digestOne("big")
	b, _ := json.Marshal(d)
	if !d.Truncated || len(b) > MaxSessionBytes || len(d.Events) == 0 || d.Events[0].Span != "a-0" {
		t.Fatalf("truncated=%v bytes=%d events=%d", d.Truncated, len(b), len(d.Events))
	}
	if d.Counts.ToolErrors != 200 {
		t.Fatalf("counts cover the whole range: %+v", d.Counts)
	}

	// Six capped sessions do not fit one run.
	w2 := newWorld(t)
	for i := range 6 {
		w2.put(defaultCwd, fmt.Sprintf("s%d", i), failingCalls(130))
	}
	out := w2.digest(DigestRequest{Unprocessed: true})
	total := 0
	for _, s := range out.Sessions {
		b, _ := json.Marshal(s)
		total += len(b)
	}
	if len(out.Deferred) == 0 || total > MaxRunBytes || len(out.Sessions)+len(out.Deferred) != 6 {
		t.Fatalf("sessions=%d deferred=%d total=%d", len(out.Sessions), len(out.Deferred), total)
	}
	for _, ref := range out.Deferred {
		_, id, _ := ParseRef(ref)
		var ok bool
		if err := w2.db.Read(w2.ctx, func(q store.Querier) error {
			var err error
			_, ok, err = store.GetSession(w2.ctx, q, HarnessClaudeCode, id)

			return err
		}); err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("deferred %s got a watermark", ref)
		}
	}
}

// uid is a submission uid in the form the API issues.
const uid = "01928c4e-7d2a-7b3c-8d4e-5f6a7b8c9d0e"

func TestMark(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
	if err := Mark(w.ctx, w.db, w.env(), []string{"claude-code:s1"}, "bogus", nil); err == nil {
		t.Fatal("bad outcome accepted")
	}
	err := Mark(w.ctx, w.db, w.env(), []string{"claude-code:s1"}, OutcomeFiled, []string{uid})
	if !errors.Is(err, store.ErrNotDigested) || !strings.Contains(err.Error(), "sessions digest") {
		t.Fatalf("got %v", err)
	}
	w.digestOne("s1")
	for _, bad := range []string{"u1", "", uid + "x", "01928c4e7d2a7b3c8d4e5f6a7b8c9d0e"} {
		if err := Mark(w.ctx, w.db, w.env(), []string{"claude-code:s1"}, OutcomeFiled, []string{bad}); !errors.Is(err, ErrInvalidUID) {
			t.Fatalf("uid %q: got %v", bad, err)
		}
	}
	if err := Mark(w.ctx, w.db, w.env(), []string{"claude-code:s1"}, OutcomeFiled, []string{uid}); err != nil {
		t.Fatal(err)
	}
	var row store.SessionSeen
	if err := w.db.Read(w.ctx, func(q store.Querier) (err error) {
		row, _, err = store.GetSession(w.ctx, q, HarnessClaudeCode, "s1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := w.env().Now().UnixMicro(); row.ProcessedAt == nil || *row.ProcessedAt != want {
		t.Fatalf("processed_at %v, want the env clock %d", row.ProcessedAt, want)
	}
	if _, _, err := ParseRef("amp:x"); err == nil {
		t.Fatal("harness without a reader accepted")
	}
	if _, _, err := ParseRef("claude-code:../x"); err == nil {
		t.Fatal("path in a session id accepted")
	}
}

// claudeStatus is the Claude Code entry of st.
func claudeStatus(t *testing.T, st StatusOutput) HarnessStatus {
	t.Helper()
	for _, hs := range st.Harnesses {
		if hs.Harness == HarnessClaudeCode {
			return hs
		}
	}
	t.Fatalf("no %s in %+v", HarnessClaudeCode, st)

	return HarnessStatus{}
}

func TestStatusAndSelection(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
	w.put(defaultCwd, "s2", fixture(t, "unsupported.jsonl", "s2", defaultCwd))
	st, err := Status(w.ctx, w.db, w.env())
	if err != nil {
		t.Fatal(err)
	}
	if hs := claudeStatus(t, st); hs.States[StateNew] != 1 || hs.States[StateUnsupportedFormat] != 1 || st.Selection != nil {
		t.Fatalf("%+v", st)
	}
	if err := SetSelection(w.ctx, w.db, Selection{Harnesses: []string{"amp"}}); err == nil {
		t.Fatal("harness without a reader accepted")
	}
	sel := Selection{Harnesses: []string{HarnessClaudeCode}, Since: "7d", Limit: 5}
	if err := SetSelection(w.ctx, w.db, sel); err != nil {
		t.Fatal(err)
	}
	st, err = Status(w.ctx, w.db, w.env())
	if err != nil {
		t.Fatal(err)
	}
	if st.Selection == nil || st.Selection.Limit != 5 || st.Selection.Since != "7d" {
		t.Fatalf("%+v", st.Selection)
	}
}

func TestLocate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
	f, err := Locate(w.ctx, w.env(), "claude-code:s1", "u-0010")
	if err != nil {
		t.Fatal(err)
	}
	if f.Model != "claude-test-2" || f.Cwd != defaultCwd || f.CwdExists || f.At.Format(timeLayout) != "2026-10-08T07:40:30.000Z" || f.State != "" {
		t.Fatalf("%+v", f)
	}
	if _, err := Locate(w.ctx, w.env(), "claude-code:s1", "nope"); !errors.Is(err, ErrSpanNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := Locate(w.ctx, w.env(), "claude-code:missing", "u-0010"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("got %v", err)
	}
	env := w.env()
	env.Policy.DenyPaths = []string{defaultCwd}
	var refused *RefusedError
	if _, err := Locate(w.ctx, env, "claude-code:s1", "u-0010"); !errors.As(err, &refused) || refused.State != StateDenied {
		t.Fatalf("got %v", err)
	}
}

func TestKey(t *testing.T) {
	t.Parallel()
	base := Key("claude-code", "s", "span", 0, DetectorVersion)
	if base != Key("claude-code", "s", "span", 0, DetectorVersion) || !strings.HasPrefix(base, "session-scan-") || len(base) != len("session-scan-")+64 {
		t.Fatalf("%s", base)
	}
	for _, k := range []string{
		Key("codex", "s", "span", 0, DetectorVersion),
		Key("claude-code", "t", "span", 0, DetectorVersion),
		Key("claude-code", "s", "spam", 0, DetectorVersion),
		Key("claude-code", "s", "span", 1, DetectorVersion),
		Key("claude-code", "s", "span", 0, "2"),
		// The separator keeps fields from sliding into each other.
		Key("claude-code", "sspan", "", 0, DetectorVersion),
	} {
		if k == base {
			t.Fatalf("collision with %s", k)
		}
	}
}

func TestReaderMatchesRegistry(t *testing.T) {
	t.Parallel()
	for _, r := range readers {
		found := false
		for _, a := range harness.Adapters() {
			if a.Name == r.harness() {
				found = true
				if a.Sessions.Reader != r.name() {
					t.Fatalf("%s: registry reader %q, implemented %q", a.Name, a.Sessions.Reader, r.name())
				}
			}
		}
		if !found {
			t.Fatalf("%s: not in the harness registry", r.harness())
		}
	}
	for _, r := range readers {
		if got, want := DetectorFor(r.harness()), r.name()+"/"+DetectorVersion; got != want {
			t.Fatalf("DetectorFor(%s) = %q, want %q", r.harness(), got, want)
		}
	}
	if DetectorFor(HarnessClaudeCode) != "claude-code-jsonl/1" || DetectorFor("no-such-harness") != "" {
		t.Fatalf("DetectorFor: %q, %q", DetectorFor(HarnessClaudeCode), DetectorFor("no-such-harness"))
	}
}

// TestDigestMarksSelf: a failed tool call that runs agentfeedback is marked
// self, another failure is not.
func TestDigestMarksSelf(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	line := func(s string) string { return strings.ReplaceAll(s, "{{CWD}}", defaultCwd) + "\n" }
	content := line(`{"type":"assistant","uuid":"a-1","timestamp":"2026-10-08T07:00:00.000Z","cwd":"{{CWD}}","message":{"model":"m","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"agentfeedback submit friction --stdin"}}]}}`) +
		line(`{"type":"user","uuid":"u-2","timestamp":"2026-10-08T07:00:01.000Z","cwd":"{{CWD}}","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"Exit code 1\nno server"}]}}`) +
		line(`{"type":"assistant","uuid":"a-3","timestamp":"2026-10-08T07:00:02.000Z","cwd":"{{CWD}}","message":{"model":"m","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"make test"}}]}}`) +
		line(`{"type":"user","uuid":"u-4","timestamp":"2026-10-08T07:00:03.000Z","cwd":"{{CWD}}","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":"Exit code 2\nfail"}]}}`)
	w.put(defaultCwd, "s1", content)
	d := w.digestOne("s1")
	if len(d.Events) != 2 || d.Events[0].Span != "a-1" || !d.Events[0].Self || d.Events[1].Span != "a-3" || d.Events[1].Self {
		t.Fatalf("events %+v", d.Events)
	}
}

func TestEncodeProject(t *testing.T) {
	t.Parallel()
	if got := encodeProject("/home/u/Projects/agentfeedback.dev"); got != "-home-u-Projects-agentfeedback-dev" {
		t.Fatal(got)
	}
	for _, fold := range []bool{false, true} {
		if !encodedWithin("-a-b-c", "/a/b", fold) || !encodedWithin("-a-b", "/a/b", fold) || encodedWithin("-a-bc", "/a/b", fold) {
			t.Fatalf("encodedWithin, fold %v", fold)
		}
		// The root contains every project directory.
		if !encodedWithin("-a-b", "/", fold) || !withinAny("-x", []string{"/"}, fold) {
			t.Fatalf("root, fold %v", fold)
		}
	}
	// Where the filesystem folds case, so does the pre-open gate.
	if !encodedWithin("-A-B-c", "/a/b", true) || encodedWithin("-A-B-c", "/a/b", false) || !withinAny("-a-b", []string{"/A/B"}, true) {
		t.Fatal("case folding")
	}
}

func TestListScrubs(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const key = "AKIAABCDEFGHIJKLMNOP"
	cwd := "/nonexistent-af/" + key
	w.put(cwd, "s1", fixture(t, "basic.jsonl", "s1", cwd))
	b, err := json.Marshal(w.list(ListOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), key) || !strings.Contains(string(b), "[REDACTED:aws_access_key]") {
		t.Fatalf("%s", b)
	}
	out := w.digest(DigestRequest{Refs: []string{"claude-code:" + key}})
	b, err = json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) != 1 || strings.Contains(string(b), key) {
		t.Fatalf("%s", b)
	}
}

func TestDigestUnprocessedUnknownProject(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "nocwd.jsonl", "s1", ""))
	if out := w.digest(DigestRequest{Unprocessed: true}); len(out.Sessions) != 0 {
		t.Fatalf("%+v", out)
	}
	if out := w.digest(DigestRequest{Unprocessed: true, AllowUnknownProject: true}); len(out.Sessions) != 1 || out.Sessions[0].SessionID != "s1" {
		t.Fatalf("%+v", out)
	}
}

func TestDigestLastCwd(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	other := "/nonexistent-af/other"
	content := fixture(t, "basic.jsonl", "s1", defaultCwd) + strings.ReplaceAll(newPrompt, defaultCwd, other) +
		strings.ReplaceAll(newPrompt, "u-0100", "u-0101")
	w.put(defaultCwd, "s1", content)
	if d := w.digestOne("s1"); d.Project != defaultCwd || d.Cwd != defaultCwd {
		t.Fatalf("project %s cwd %s", d.Project, d.Cwd)
	}
}

func TestListKeepsSummariesOnly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
	r, _ := readerFor(HarnessClaudeCode)
	_, evs, _, err := evaluateAll(w.ctx, w.db, w.env(), r, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].tr == nil || evs[0].tr.entries != nil || evs[0].tr.badLines != nil || evs[0].tr.total.Entries != 13 {
		t.Fatalf("%+v", evs)
	}
}

func TestUnlistedWording(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "chats")
	problems := []problem{{path: dir, dir: true, reason: ".project_root: permission denied"}}
	if st, reason := unlisted(problems, filepath.Join(dir, "s.json")); st != StateUnreadable || reason != "sessions in "+dir+" could not be listed: .project_root: permission denied" {
		t.Fatalf("%s %s", st, reason)
	}
	if st, _ := unlisted(problems, filepath.Join(t.TempDir(), "s.json")); st != StateAbsent {
		t.Fatalf("%s", st)
	}
}
