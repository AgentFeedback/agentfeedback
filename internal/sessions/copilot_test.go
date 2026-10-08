package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// cpFixture is the fixture store's events.jsonl with its placeholders filled.
func cpFixture(t *testing.T, sid, cwd string) string {
	t.Helper()

	return fixture(t, filepath.Join("stores", "copilot", "session-state", "{{SID}}", "events.jsonl"), sid, cwd)
}

// cpYAML is a workspace.yaml in YAML form recording cwd.
func cpYAML(sid, cwd string) string {
	return "id: " + sid + "\ncwd: " + cwd + "\nbranch: main\ncreated_at: 2026-10-08T07:39:40.000Z\n"
}

// cpPut writes session sid's events and, unless workspace is "", its
// workspace.yaml, and returns the events path.
func cpPut(w *world, sid, events, workspace string) string {
	w.t.Helper()
	dir := filepath.Join(w.env().CopilotHome, "session-state", sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	if workspace != "" {
		if err := os.WriteFile(filepath.Join(dir, copilotWorkspace), []byte(workspace), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
	p := filepath.Join(dir, copilotEvents)
	if err := os.WriteFile(p, []byte(events), 0o644); err != nil {
		w.t.Fatal(err)
	}

	return p
}

func cpSession(w *world, sid string) Session {
	w.t.Helper()
	l, err := List(w.ctx, w.db, w.env(), ListOptions{Harness: HarnessCopilot})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, s := range l.Sessions {
		if s.SessionID == sid {
			return s
		}
	}
	w.t.Fatalf("session %s not listed: %+v", sid, l)

	return Session{}
}

func cpDigest(w *world, req DigestRequest) DigestOutput {
	w.t.Helper()
	req.Harness = HarnessCopilot
	out, err := Digest(w.ctx, w.db, w.env(), req)
	if err != nil {
		w.t.Fatal(err)
	}

	return out
}

func cpDigestOne(w *world, sid string) SessionDigest {
	w.t.Helper()
	out := cpDigest(w, DigestRequest{Refs: []string{Ref(HarnessCopilot, sid)}})
	if len(out.Sessions) != 1 {
		w.t.Fatalf("digest of %s: %+v", sid, out)
	}

	return out.Sessions[0]
}

func cpMark(w *world, sid string) {
	w.t.Helper()
	if err := Mark(w.ctx, w.db, w.env(), []string{Ref(HarnessCopilot, sid)}, OutcomeNothing, nil); err != nil {
		w.t.Fatal(err)
	}
}

// cpLines joins event lines, each ended by '\n'.
func cpLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

const (
	cpStart     = `{"type":"session.start","data":{"sessionId":"s1","selectedModel":"m1","context":{"cwd":"` + defaultCwd + `"}},"id":"e-0","timestamp":"2026-10-08T07:00:00.000Z","parentId":null}`
	cpNewPrompt = `{"type":"user.message","data":{"content":"Still failing after the fix","source":"user"},"id":"e-0100","timestamp":"2026-10-08T08:00:00.000Z","parentId":null}` + "\n"
)

// cpCall is a tool.execution_start of call id running command.
func cpCall(span, id, command string) string {
	return `{"type":"tool.execution_start","data":{"toolCallId":"` + id + `","toolName":"bash","arguments":{"command":"` + command + `"}},"id":"` + span + `","timestamp":"2026-10-08T07:00:01.000Z","parentId":null}`
}

// cpComplete is a tool.execution_complete of call id with data members rest.
func cpComplete(span, id, rest string) string {
	return `{"type":"tool.execution_complete","data":{"toolCallId":"` + id + `",` + rest + `},"id":"` + span + `","timestamp":"2026-10-08T07:00:02.000Z","parentId":null}`
}

func TestCopilotStates(t *testing.T) {
	t.Parallel()

	t.Run("new", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		cpPut(w, "s1", cpFixture(t, "s1", defaultCwd), cpYAML("s1", defaultCwd))
		s := cpSession(w, "s1")
		if s.State != StateNew || s.Project != defaultCwd || s.Counts == nil || s.Counts.Prompts != 2 || s.Counts.Unparsed != 1 {
			t.Fatalf("%+v %+v", s, s.Counts)
		}
	})
	t.Run("processed", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		cpPut(w, "s1", cpFixture(t, "s1", defaultCwd), cpYAML("s1", defaultCwd))
		cpDigestOne(w, "s1")
		cpMark(w, "s1")
		if s := cpSession(w, "s1"); s.State != StateProcessed {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("changed", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		content := cpFixture(t, "s1", defaultCwd)
		p := cpPut(w, "s1", content, cpYAML("s1", defaultCwd))
		cpDigestOne(w, "s1")
		cpMark(w, "s1")
		w.appendTo(p, cpNewPrompt)
		if s := cpSession(w, "s1"); s.State != StateChanged || s.Reason != ChangeAppended {
			t.Fatalf("%+v", s)
		}
		out := cpDigest(w, DigestRequest{Unprocessed: true})
		if len(out.Sessions) != 1 {
			t.Fatalf("%+v", out)
		}
		d := out.Sessions[0]
		if d.FromOffset != int64(len(content)) || strings.Join(spans(d), ",") != "e-0100" || d.Counts.Entries != 1 {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("result after the mark", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		p := cpPut(w, "s1", cpLines(cpStart, cpCall("e-1", "c1", "make")), "")
		if d := cpDigestOne(w, "s1"); len(d.Events) != 0 {
			t.Fatalf("%+v", d)
		}
		cpMark(w, "s1")
		w.appendTo(p, cpLines(cpComplete("e-2", "c1", `"success":false,"error":{"message":"boom","code":"failure"},"shellExecution":{"exitCode":2}`)))
		out := cpDigest(w, DigestRequest{Unprocessed: true})
		if len(out.Sessions) != 1 || strings.Join(spans(out.Sessions[0]), ",") != "e-1" {
			t.Fatalf("%+v", out)
		}
		e := out.Sessions[0].Events[0]
		if e.Status != statusError || e.ExitCode == nil || *e.ExitCode != 2 || e.ErrorClass != "exit" {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("partial tail", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		content := cpFixture(t, "s1", defaultCwd)
		p := cpPut(w, "s1", content+strings.TrimSuffix(cpNewPrompt, "\n"), cpYAML("s1", defaultCwd))
		d := cpDigestOne(w, "s1")
		if slices.Contains(spans(d), "e-0100") || d.Counts.Entries != 21 || d.Counts.Unparsed != 1 {
			t.Fatalf("partial line read: %+v", d)
		}
		cpMark(w, "s1")
		if s := cpSession(w, "s1"); s.State != StateProcessed {
			t.Fatalf("a partial tail must not count: %+v", s)
		}
		w.appendTo(p, "\n")
		d = cpDigestOne(w, "s1")
		if d.FromOffset != int64(len(content)) || strings.Join(spans(d), ",") != "e-0100" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("truncation", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		content := cpFixture(t, "s1", defaultCwd)
		cpPut(w, "s1", content, cpYAML("s1", defaultCwd))
		cpDigestOne(w, "s1")
		cpMark(w, "s1")
		lines := strings.SplitAfter(content, "\n")
		cpPut(w, "s1", strings.Join(lines[:5], ""), "")
		if s := cpSession(w, "s1"); s.State != StateChanged || s.Reason != ChangeTruncated {
			t.Fatalf("%+v", s)
		}
		if d := cpDigestOne(w, "s1"); d.FromOffset != 0 || d.Events[0].Span != "e-0002" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("rotation", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		content := cpFixture(t, "s1", defaultCwd)
		cpPut(w, "s1", content, cpYAML("s1", defaultCwd))
		cpDigestOne(w, "s1")
		cpMark(w, "s1")
		cpPut(w, "s1", cpNewPrompt+content, "")
		if s := cpSession(w, "s1"); s.State != StateChanged || s.Reason != ChangeRotated {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("unsupported-format", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		cpPut(w, "s1", fixture(t, "unsupported.jsonl", "s1", defaultCwd), cpYAML("s1", defaultCwd))
		if s := cpSession(w, "s1"); s.State != StateUnsupportedFormat {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("denied before open", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		p := cpPut(w, "s1", cpFixture(t, "s1", defaultCwd), cpYAML("s1", defaultCwd))
		other := cpPut(w, "s2", cpFixture(t, "s2", defaultCwd+"x"), cpYAML("s2", defaultCwd+"x"))
		w.pol.DenyPaths = []string{defaultCwd}
		if s := cpSession(w, "s1"); s.State != StateDenied || s.Reason != collect.ReasonDenyPaths {
			t.Fatalf("%+v", s)
		}
		if s := cpSession(w, "s2"); s.State != StateNew {
			t.Fatalf("sibling: %+v", s)
		}
		out := cpDigest(w, DigestRequest{Refs: []string{Ref(HarnessCopilot, "s1")}})
		if len(out.Errors) != 1 || out.Errors[0].State != StateDenied {
			t.Fatalf("%+v", out)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.opens[p] != 0 || w.opens[other] == 0 {
			t.Fatalf("opens %v", w.opens)
		}
	})
	t.Run("denied after read without workspace", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		cpPut(w, "s1", cpFixture(t, "s1", defaultCwd), "")
		w.pol.DenyPaths = []string{defaultCwd}
		if s := cpSession(w, "s1"); s.State != StateDenied || s.Counts != nil {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		p := cpPut(w, "s1", cpFixture(t, "s1", defaultCwd), "")
		w.pol.Disabled = true
		if s := cpSession(w, "s1"); s.State != StateDisabled || w.opens[p] != 0 {
			t.Fatalf("%+v %v", s, w.opens)
		}
	})
	t.Run("unknown-project", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		cpPut(w, "s1", cpLines(`{"type":"user.message","data":{"content":"hi"},"id":"e-1","timestamp":"2026-10-08T07:00:00.000Z"}`), "cwd: relative/dir\n")
		if s := cpSession(w, "s1"); s.State != StateUnknownProject {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("store absent", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l, err := List(w.ctx, w.db, w.env(), ListOptions{Harness: HarnessCopilot})
		if err != nil {
			t.Fatal(err)
		}
		if len(l.Stores) != 1 || l.Stores[0].State != StateAbsent || len(l.Sessions) != 0 {
			t.Fatalf("%+v", l)
		}
	})
}

func TestCopilotDigest(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cpPut(w, "s1", cpFixture(t, "s1", defaultCwd), cpYAML("s1", defaultCwd))
	d := cpDigestOne(w, "s1")
	if d.Counts != (Counts{Entries: 21, Prompts: 2, ToolCalls: 6, ToolErrors: 3, Interrupts: 2, Denials: 1, Unparsed: 1}) {
		t.Fatalf("counts %+v", d.Counts)
	}
	if got := strings.Join(spans(d), ","); got != "e-0002,e-0004,e-0008,e-0012,e-0020,e-0015,e-0017" {
		t.Fatalf("spans %s", got)
	}
	ev := d.Events
	if ev[1].Status != statusError || ev[1].ErrorClass != "exit" || ev[1].ExitCode == nil || *ev[1].ExitCode != 1 || ev[1].Tool != "bash" || ev[1].Self ||
		ev[1].Excerpt != "main.go:3: undefined: foo (key [REDACTED:aws_access_key])" {
		t.Fatalf("%+v", ev[1])
	}
	if ev[2].Status != statusDenied || ev[2].Excerpt != "The user rejected this tool call." {
		t.Fatalf("%+v", ev[2])
	}
	if ev[3].Status != statusError || !ev[3].Self || ev[3].ExitCode == nil || *ev[3].ExitCode != 1 || ev[3].Excerpt != "no server" {
		t.Fatalf("%+v", ev[3])
	}
	if ev[4].Status != statusError || ev[4].ExitCode != nil || ev[4].ErrorClass != "tool_error" || ev[4].Tool != "github-create_pull_request" {
		t.Fatalf("%+v", ev[4])
	}
	if ev[5].Status != statusInterrupted || ev[5].Excerpt != abortedText {
		t.Fatalf("%+v", ev[5])
	}
	if len(d.Retries) != 1 || d.Retries[0].Span != "e-0006" || d.Retries[0].OfSpan != "e-0004" {
		t.Fatalf("retries %+v", d.Retries)
	}
	if len(d.Denials) != 1 || d.Denials[0] != "e-0008" {
		t.Fatalf("denials %+v", d.Denials)
	}
	if d.Model != "gpt-test-2" || d.FinalExcerpt != "Done. The build passes now." || d.Project != defaultCwd {
		t.Fatalf("%+v", d)
	}
	var phrases []string
	for _, f := range d.Flagged {
		phrases = append(phrases, f.Span+":"+f.Phrase)
	}
	// The sub-agent's message (e-0014) is not a prompt.
	if got := strings.Join(phrases, ","); got != "e-0002:doesn't work,e-0017:still failing,e-0017:why did you,e-0017:again" {
		t.Fatalf("flagged %s", got)
	}
}

// cpParse parses content with the workspace cwd defaultCwd.
func cpParse(t *testing.T, content string) *transcript {
	t.Helper()
	tr, err := copilotReader{}.parse(strings.NewReader(content), defaultCwd)
	if err != nil {
		t.Fatal(err)
	}

	return tr
}

// cpCallOf is the call of the entry with span.
func cpCallOf(t *testing.T, tr *transcript, span string) *call {
	t.Helper()
	for _, e := range tr.entries {
		if e.span == span && len(e.calls) == 1 {
			return e.calls[0]
		}
	}
	t.Fatalf("no call at %s", span)

	return nil
}

// cpOffset is the offset of the entry with span.
func cpOffset(t *testing.T, tr *transcript, span string) int64 {
	t.Helper()
	for _, e := range tr.entries {
		if e.span == span {
			return e.offset
		}
	}
	t.Fatalf("no entry %s", span)

	return 0
}

func cpPermitted(span, id, kind string) string {
	return `{"type":"permission.completed","data":{"requestId":"r","toolCallId":"` + id + `","result":{"kind":"` + kind + `"}},"id":"` + span + `","timestamp":"2026-10-08T07:00:02.000Z"}`
}

const cpAbort = `{"type":"abort","data":{"reason":"user_initiated"},"id":"e-ab","timestamp":"2026-10-08T07:00:03.000Z"}`

func TestCopilotStatuses(t *testing.T) {
	t.Parallel()
	tr := cpParse(t, cpLines(
		cpStart,
		cpCall("s-ok", "ok", "true"), cpComplete("r-ok", "ok", `"success":true,"result":{"content":"fine"}`),
		cpCall("s-fail", "fail", "false"), cpComplete("r-fail", "fail", `"success":false,"error":{"message":"nope","code":"failure"}`),
		cpCall("s-timeout", "timeout", "sleep"), cpComplete("r-timeout", "timeout", `"success":false,"error":{"message":"timed out","code":"timeout"}`),
		cpCall("s-rej", "rej", "rm"), cpComplete("r-rej", "rej", `"success":false,"error":{"message":"rejected","code":"rejected"}`),
		cpCall("s-den", "den", "rm"), cpComplete("r-den", "den", `"success":false,"error":{"message":"denied","code":"denied"}`),
		cpCall("s-perm", "perm", "rm"), cpPermitted("p-perm", "perm", "denied-by-rules"),
		cpComplete("r-perm", "perm", `"success":false,"error":{"message":"blocked","code":"failure"},"shellExecution":{"exitCode":126}`),
		cpCall("s-cancel", "cancel", "x"), cpPermitted("p-cancel", "cancel", "cancelled"),
		cpCall("s-approved", "approved", "y"), cpPermitted("p-approved", "approved", "approved"),
		cpComplete("r-approved", "approved", `"success":true`),
		cpComplete("r-twice", "approved", `"success":false,"error":{"message":"late","code":"failure"}`),
		cpCall("s-pending", "pending", "sleep 100"),
		cpAbort,
		cpComplete("r-late", "pending", `"success":true,"result":{"content":"late"},"shellExecution":{"exitCode":0}`),
		cpCall("s-open", "open", "z"),
	))
	for _, c := range []struct {
		span, status, content string
		result                bool
		at                    string
	}{
		{"s-ok", statusOK, "fine", true, "r-ok"},
		{"s-fail", statusError, "nope", true, "r-fail"},
		{"s-timeout", statusError, "timed out", true, "r-timeout"},
		{"s-rej", statusDenied, "rejected", true, "r-rej"},
		{"s-den", statusDenied, "denied", true, "r-den"},
		// Denied by the permission, kept with its offset; the late
		// complete fills the content and the exit code.
		{"s-perm", statusDenied, "blocked", true, "p-perm"},
		{"s-cancel", statusInterrupted, "", true, "p-cancel"},
		// Only the first complete counts.
		{"s-approved", statusOK, "", true, "r-approved"},
		// The abort settles the pending call; the late complete changes nothing.
		{"s-pending", statusInterrupted, abortedText, true, "e-ab"},
		{"s-open", "", "", false, ""},
	} {
		k := cpCallOf(t, tr, c.span)
		if k.status != c.status || k.content != c.content || k.result != c.result {
			t.Fatalf("%s: %+v", c.span, k)
		}
		if c.result && k.resultOffset != cpOffset(t, tr, c.at) {
			t.Fatalf("%s: result offset %d, want that of %s", c.span, k.resultOffset, c.at)
		}
	}
	if k := cpCallOf(t, tr, "s-perm"); k.exitCode == nil || *k.exitCode != 126 {
		t.Fatalf("%+v", k)
	}
	if k := cpCallOf(t, tr, "s-pending"); k.exitCode != nil {
		t.Fatalf("%+v", k)
	}
	if c := tr.counts(0); c.Interrupts != 3 || c.Denials != 3 || c.ToolErrors != 2 || c.ToolCalls != 10 {
		t.Fatalf("%+v", c)
	}
}

func TestCopilotSubAgentIgnored(t *testing.T) {
	t.Parallel()
	tr := cpParse(t, cpLines(
		cpStart,
		`{"type":"user.message","data":{"content":"doesn't work"},"id":"e-1","timestamp":"2026-10-08T07:00:01.000Z","agentId":"a1"}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"c1","toolName":"bash","arguments":{"command":"ls"}},"id":"e-2","timestamp":"2026-10-08T07:00:02.000Z","agentId":"a1"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"c1","success":false},"id":"e-3","timestamp":"2026-10-08T07:00:03.000Z","agentId":"a1"}`,
		`{"type":"session.context_changed","data":{"cwd":"/nonexistent-af/sub"},"id":"e-4","timestamp":"2026-10-08T07:00:04.000Z","agentId":"a1"}`,
	))
	if c := tr.counts(0); c != (Counts{Entries: 5}) || !slices.Equal(tr.cwds, []string{defaultCwd}) {
		t.Fatalf("%+v %v", c, tr.cwds)
	}
}

func TestCopilotTolerant(t *testing.T) {
	t.Parallel()
	tr := cpParse(t, cpLines(
		cpStart,
		// Unknown members and event types are ignored; data of another
		// type keeps the line as an entry.
		`{"type":"user.message","data":{"content":"hi","novel":{"a":[1]}},"id":"e-1","timestamp":"2026-10-08T07:00:01.000Z","extra":true}`,
		`{"type":"future.event","data":{"x":1},"id":"e-2","timestamp":"2026-10-08T07:00:02.000Z"}`,
		`{"type":"user.message","data":"text","id":"e-3","timestamp":"2026-10-08T07:00:03.000Z"}`,
		`{"type":"user.message","data":{"content":"bot","source":"system"},"id":"e-4","timestamp":"2026-10-08T07:00:04.000Z"}`,
		`{"type":"user.message","data":{"content":"go on","isAutopilotContinuation":true},"id":"e-5","timestamp":"2026-10-08T07:00:05.000Z"}`,
		`{"type":"session.context_changed","data":{"cwd":"/nonexistent-af/other"},"id":"e-6","timestamp":"2026-10-08T07:00:06.000Z"}`,
		`{"type":7}`,
		"{\"type\":\"user.message\",\"data\":{\"content\":\"torn",
	))
	c := tr.counts(0)
	if c.Entries != 7 || c.Prompts != 1 || c.Unparsed != 2 || tr.sessionID != "s1" {
		t.Fatalf("%+v %q", c, tr.sessionID)
	}
	if !slices.Equal(tr.cwds, []string{defaultCwd, "/nonexistent-af/other"}) || tr.lastCwd != "/nonexistent-af/other" {
		t.Fatalf("%v %s", tr.cwds, tr.lastCwd)
	}
}

func TestCopilotWorkspaceCwd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i, c := range []struct{ body, want string }{
		{"id: x\ncwd: /a/b\nbranch: main\n", "/a/b"},
		{"id: x\r\ncwd:   /a/b c  \r\n", "/a/b c"},
		{"cwd: \"/a/\\\"q\\\"\"\n", `/a/"q"`},
		{"cwd: '/a/it''s'\n", "/a/it's"},
		{"cwd: /a/b # comment\n", "/a/b"},
		{"cwd: \"/p\" # c\n", "/p"},
		{"cwd: '/p' # c\n", "/p"},
		{"cwd: '/it''s' # c\n", "/it's"},
		{"cwd: \"/p\"x\n", ""},
		{"repo:\n  cwd: /nested\n", ""},
		{"cwd: relative\n", ""},
		{`{"id":"x","cwd":"/a/json","created_at":"2026-10-08T07:00:00Z"}`, "/a/json"},
		{"  {\"cwd\": \"rel\"}", ""},
		{"", ""},
	} {
		p := filepath.Join(dir, "w"+string(rune('a'+i))+".yaml")
		if err := os.WriteFile(p, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := workspaceCwd(p); got != c.want {
			t.Fatalf("%q: got %q, want %q", c.body, got, c.want)
		}
	}
	if got := workspaceCwd(filepath.Join(dir, "absent.yaml")); got != "" {
		t.Fatalf("absent: %q", got)
	}
	sub := filepath.Join(dir, "dir.yaml")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := workspaceCwd(sub); got != "" {
		t.Fatalf("directory: %q", got)
	}
	big := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(big, []byte("cwd: /a/b\n"+strings.Repeat("#\n", workspaceMax)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := workspaceCwd(big); got != "" {
		t.Fatalf("oversized: %q", got)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(filepath.Join(dir, "wa.yaml"), link); err != nil {
		t.Fatal(err)
	}
	if got := workspaceCwd(link); got != "/a/b" {
		t.Fatalf("symlink: %q", got)
	}
}

func TestCopilotWorkspaceJSONGate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := cpPut(w, "s1", cpFixture(t, "s1", defaultCwd), `{"id":"s1","cwd":"`+defaultCwd+`"}`)
	w.pol.OptInOnly, w.pol.OptInPaths = true, []string{"/allowed-af"}
	if s := cpSession(w, "s1"); s.State != StateDenied || s.Reason != collect.ReasonOptInOnly || w.opens[p] != 0 {
		t.Fatalf("%+v %v", s, w.opens)
	}
}

func TestCopilotLocate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cpPut(w, "s1", cpFixture(t, "s1", defaultCwd), cpYAML("s1", defaultCwd))
	env := w.env()
	f, err := Locate(w.ctx, env, "copilot:s1", "e-0017")
	if err != nil {
		t.Fatal(err)
	}
	if f.Model != "gpt-test-1" || f.Cwd != defaultCwd || f.CwdExists || f.At.Format(timeLayout) != "2026-10-08T07:40:30.000Z" || f.Detector != "copilot-events/"+DetectorVersion {
		t.Fatalf("%+v", f)
	}
	if f, err := Locate(w.ctx, env, "copilot:s1", "e-0019"); err != nil || f.Model != "gpt-test-2" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := Locate(w.ctx, env, "copilot:s1", "nope"); !errors.Is(err, ErrSpanNotFound) {
		t.Fatalf("got %v", err)
	}
	env.Policy.DenyPaths = []string{defaultCwd}
	var refused *RefusedError
	if _, err := Locate(w.ctx, env, "copilot:s1", "e-0017"); !errors.As(err, &refused) || refused.State != StateDenied {
		t.Fatalf("got %v", err)
	}
}

func TestCopilotShellExit(t *testing.T) {
	t.Parallel()
	tr := cpParse(t, cpLines(
		cpStart,
		cpCall("s-native", "native", "make"), cpComplete("r-native", "native", `"success":true,"result":{"content":"fail\n<shellId: 3 completed with exit code 2>"},"shellExecution":{"exitCode":2}`),
		cpCall("s-marker", "marker", "make"), cpComplete("r-marker", "marker", `"success":true,"result":{"content":"fail\n<detached command with shellId: 7 completed with exit code -1>  \n<sandbox is active here>\n<this failure may be caused by the sandbox>\n"}`),
		cpCall("s-zero", "zero", "true"), cpComplete("r-zero", "zero", `"success":true,"result":{"content":"done\n<shellId: 3 completed with exit code 0>"}`),
		cpCall("s-none", "none", "true"), cpComplete("r-none", "none", `"success":true,"result":{"content":"done"}`),
		// The marker is only read from the last line.
		cpCall("s-mid", "mid", "true"), cpComplete("r-mid", "mid", `"success":true,"result":{"content":"<shellId: 3 completed with exit code 1>\nmore"}`),
		cpCall("s-rej", "rej", "rm"), cpComplete("r-rej", "rej", `"success":false,"error":{"message":"rejected","code":"rejected"},"shellExecution":{"exitCode":1}`),
		cpCall("s-perm", "perm", "rm"), cpPermitted("p-perm", "perm", "denied-by-rules"),
		cpComplete("r-perm", "perm", `"success":true,"result":{"content":"x\n<shellId: 3 completed with exit code 1>"}`),
		cpCall("s-ab", "ab", "sleep"), cpAbort,
		cpComplete("r-ab", "ab", `"success":true,"result":{"content":"x\n<shellId: 3 completed with exit code 1>"},"shellExecution":{"exitCode":1}`),
	))
	for _, c := range []struct {
		span, status, content string
		exit                  *int
	}{
		{"s-native", statusError, "fail", cpInt(2)},
		{"s-marker", statusError, "fail", cpInt(-1)},
		{"s-zero", statusOK, "done", cpInt(0)},
		{"s-none", statusOK, "done", nil},
		{"s-mid", statusOK, "<shellId: 3 completed with exit code 1>\nmore", nil},
		{"s-rej", statusDenied, "rejected", cpInt(1)},
		{"s-perm", statusDenied, "x", cpInt(1)},
		{"s-ab", statusInterrupted, abortedText, nil},
	} {
		k := cpCallOf(t, tr, c.span)
		if k.status != c.status || k.content != c.content || (k.exitCode == nil) != (c.exit == nil) || (c.exit != nil && *k.exitCode != *c.exit) {
			t.Fatalf("%s: %+v exit %v", c.span, k, k.exitCode)
		}
	}
}

func cpInt(n int) *int { return &n }

// TestCopilotLatePermission: a permission.completed after the call's result
// changes nothing, and one before it settles the call.
func TestCopilotLatePermission(t *testing.T) {
	t.Parallel()
	tr := cpParse(t, cpLines(
		cpStart,
		cpCall("s-after", "after", "rm"), cpComplete("r-after", "after", `"success":true,"result":{"content":"done"}`),
		cpPermitted("p-after", "after", "denied-by-rules"),
		cpCall("s-cancel", "cancel", "rm"), cpComplete("r-cancel", "cancel", `"success":false,"error":{"message":"nope","code":"failure"}`),
		cpPermitted("p-cancel", "cancel", "cancelled"),
		cpCall("s-before", "before", "rm"), cpPermitted("p-before", "before", "denied-by-rules"),
		cpComplete("r-before", "before", `"success":true,"result":{"content":"done"}`),
	))
	for _, c := range []struct{ span, status, at string }{
		{"s-after", statusOK, "r-after"},
		{"s-cancel", statusError, "r-cancel"},
		{"s-before", statusDenied, "p-before"},
	} {
		k := cpCallOf(t, tr, c.span)
		if k.status != c.status || k.resultOffset != cpOffset(t, tr, c.at) {
			t.Fatalf("%s: %+v", c.span, k)
		}
	}
}

// TestCopilotShellMarkerShellOnly: the exit line is read only from a shell
// tool's result.
func TestCopilotShellMarkerShellOnly(t *testing.T) {
	t.Parallel()
	const out = `"success":true,"result":{"content":"log\n<shellId: 3 completed with exit code 1>"}`
	tr := cpParse(t, cpLines(
		cpStart,
		`{"type":"tool.execution_start","data":{"toolCallId":"view","toolName":"view","arguments":{"path":"/x"}},"id":"s-view","timestamp":"2026-10-08T07:00:01.000Z"}`,
		cpComplete("r-view", "view", out),
		`{"type":"tool.execution_start","data":{"toolCallId":"ps","toolName":"powershell","arguments":{"command":"x"}},"id":"s-ps","timestamp":"2026-10-08T07:00:01.000Z"}`,
		cpComplete("r-ps", "ps", out),
		`{"type":"tool.execution_start","data":{"toolCallId":"info","toolName":"run","arguments":{"command":"x"},"shellToolInfo":{"shell":"zsh"}},"id":"s-info","timestamp":"2026-10-08T07:00:01.000Z"}`,
		cpComplete("r-info", "info", out),
		`{"type":"tool.execution_start","data":{"toolCallId":"exec","toolName":"run","arguments":{"command":"x"}},"id":"s-exec","timestamp":"2026-10-08T07:00:01.000Z"}`,
		cpComplete("r-exec", "exec", `"success":true,"result":{"content":"log\n<shellId: 3 completed with exit code 1>"},"shellExecution":{}`),
	))
	if k := cpCallOf(t, tr, "s-view"); k.status != statusOK || k.exitCode != nil || k.content != "log\n<shellId: 3 completed with exit code 1>" {
		t.Fatalf("view: %+v", k)
	}
	for _, span := range []string{"s-ps", "s-info", "s-exec"} {
		if k := cpCallOf(t, tr, span); k.status != statusError || k.exitCode == nil || *k.exitCode != 1 || k.content != "log" {
			t.Fatalf("%s: %+v", span, k)
		}
	}
}

// TestCopilotFailedExitZero: a failed call whose native exit code is 0 is a
// tool error without exit code.
func TestCopilotFailedExitZero(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cpPut(w, "s1", cpLines(
		cpStart,
		cpCall("s-z", "z", "make"),
		cpComplete("r-z", "z", `"success":false,"error":{"message":"failed","code":"failure"},"shellExecution":{"exitCode":0}`),
	), cpYAML("s1", defaultCwd))
	d := cpDigestOne(w, "s1")
	if len(d.Events) != 1 || d.Events[0].ErrorClass != "tool_error" || d.Events[0].ExitCode != nil {
		t.Fatalf("%+v", d.Events)
	}
}
