package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// geminiFixture is the Gemini CLI fixture session, relative to testdata.
const geminiFixture = "stores/gemini-cli/tmp/golden-proj/chats/session-2026-10-08T07-39-{{SID}}.jsonl"

// gworld is a world with the path of its Gemini CLI store, the world
// env's GeminiDir.
type gworld struct {
	*world
	dir string
}

func newGeminiWorld(t *testing.T) *gworld {
	t.Helper()
	w := newWorld(t)

	return &gworld{world: w, dir: w.env().GeminiDir}
}

// put writes content as session file session-<sid>.jsonl of the project
// slug whose .project_root names cwd ("" writes none) and returns its path.
func (g *gworld) put(slug, cwd, sid, content string) string {
	g.t.Helper()
	dir := filepath.Join(g.dir, "tmp", slug)
	if err := os.MkdirAll(filepath.Join(dir, "chats"), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if cwd != "" {
		if err := os.WriteFile(filepath.Join(dir, ".project_root"), []byte(cwd+"\n"), 0o644); err != nil {
			g.t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "chats", "session-"+sid+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}

	return p
}

func gref(sid string) string { return Ref(HarnessGeminiCLI, "session-"+sid) }

func (g *gworld) list() Listing {
	g.t.Helper()
	l, err := List(g.ctx, g.db, g.env(), ListOptions{Harness: HarnessGeminiCLI})
	if err != nil {
		g.t.Fatal(err)
	}

	return l
}

func (g *gworld) session(sid string) Session {
	g.t.Helper()
	for _, s := range g.list().Sessions {
		if s.SessionID == "session-"+sid {
			return s
		}
	}
	g.t.Fatalf("session %s not listed", sid)

	return Session{}
}

func (g *gworld) digest(req DigestRequest) DigestOutput {
	g.t.Helper()
	out, err := Digest(g.ctx, g.db, g.env(), req)
	if err != nil {
		g.t.Fatal(err)
	}

	return out
}

func (g *gworld) digestOne(sid string) SessionDigest {
	g.t.Helper()
	out := g.digest(DigestRequest{Refs: []string{gref(sid)}})
	if len(out.Sessions) != 1 {
		g.t.Fatalf("digest of %s: %+v", sid, out)
	}

	return out.Sessions[0]
}

func (g *gworld) mark(sid string) {
	g.t.Helper()
	if err := Mark(g.ctx, g.db, g.env(), []string{gref(sid)}, OutcomeNothing, nil); err != nil {
		g.t.Fatal(err)
	}
}

const geminiPrompt = `{"id":"m-0100","timestamp":"2026-10-08T08:00:00.000Z","type":"user","content":[{"text":"Still failing after the fix"}]}` + "\n"

func TestGeminiStates(t *testing.T) {
	t.Parallel()

	t.Run("new", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		g.put("proj", defaultCwd, "s1", fixture(t, geminiFixture, "s1", defaultCwd))
		s := g.session("s1")
		if s.State != StateNew || s.Project != defaultCwd || s.Counts == nil ||
			*s.Counts != (Counts{Entries: 12, Prompts: 2, ToolCalls: 7, ToolErrors: 3, Interrupts: 1, Denials: 2, Unparsed: 1}) {
			t.Fatalf("%+v %+v", s, s.Counts)
		}
	})
	t.Run("processed", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		g.put("proj", defaultCwd, "s1", fixture(t, geminiFixture, "s1", defaultCwd))
		g.digestOne("s1")
		g.mark("s1")
		if s := g.session("s1"); s.State != StateProcessed {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("changed appended", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		content := fixture(t, geminiFixture, "s1", defaultCwd)
		p := g.put("proj", defaultCwd, "s1", content)
		g.digestOne("s1")
		g.mark("s1")
		g.appendTo(p, geminiPrompt)
		if s := g.session("s1"); s.State != StateChanged || s.Reason != ChangeAppended {
			t.Fatalf("%+v", s)
		}
		out := g.digest(DigestRequest{Unprocessed: true, Harness: HarnessGeminiCLI})
		if len(out.Sessions) != 1 {
			t.Fatalf("%+v", out)
		}
		d := out.Sessions[0]
		if d.FromOffset != int64(len(content)) || strings.Join(spans(d), ",") != "m-0100" || d.Counts.Entries != 1 {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("partial tail", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		content := fixture(t, geminiFixture, "s1", defaultCwd)
		p := g.put("proj", defaultCwd, "s1", content+strings.TrimSuffix(geminiPrompt, "\n"))
		d := g.digestOne("s1")
		if strings.Contains(strings.Join(spans(d), ","), "m-0100") || d.Counts.Entries != 12 {
			t.Fatalf("partial line read: %+v", d)
		}
		g.mark("s1")
		if s := g.session("s1"); s.State != StateProcessed {
			t.Fatalf("a partial tail must not count: %+v", s)
		}
		g.appendTo(p, "\n")
		if d := g.digestOne("s1"); d.FromOffset != int64(len(content)) || strings.Join(spans(d), ",") != "m-0100" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("truncation", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		content := fixture(t, geminiFixture, "s1", defaultCwd)
		g.put("proj", defaultCwd, "s1", content)
		g.digestOne("s1")
		g.mark("s1")
		lines := strings.SplitAfter(content, "\n")
		g.put("proj", defaultCwd, "s1", strings.Join(lines[:4], ""))
		if s := g.session("s1"); s.State != StateChanged || s.Reason != ChangeTruncated {
			t.Fatalf("%+v", s)
		}
		if d := g.digestOne("s1"); d.FromOffset != 0 || strings.Join(spans(d), ",") != "m-0002" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("rotation", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		content := fixture(t, geminiFixture, "s1", defaultCwd)
		g.put("proj", defaultCwd, "s1", content)
		g.digestOne("s1")
		g.mark("s1")
		g.put("proj", defaultCwd, "s1", geminiPrompt+content)
		if s := g.session("s1"); s.State != StateChanged || s.Reason != ChangeRotated {
			t.Fatalf("%+v", s)
		}
		if d := g.digestOne("s1"); d.FromOffset != 0 || d.Events[0].Span != "m-0100" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("unsupported-format", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		g.put("proj", defaultCwd, "s1", "not json\n[1,2]\n{\"$set\":5}\n{\"type\":\"user\"}\n")
		if s := g.session("s1"); s.State != StateUnsupportedFormat {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("legacy json", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		p := g.put("proj", defaultCwd, "s1", "")
		legacy := filepath.Join(filepath.Dir(p), "session-old.json")
		if err := os.WriteFile(legacy, []byte(`{"sessionId":"x","messages":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		s := g.session("old")
		if s.State != StateUnsupportedFormat || !strings.Contains(s.Reason, "legacy single-document session file") {
			t.Fatalf("%+v", s)
		}
		if g.opens[legacy] != 0 {
			t.Fatalf("legacy file opened %d times", g.opens[legacy])
		}
	})
	t.Run("denied before open", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		p := g.put("proj", defaultCwd, "s1", fixture(t, geminiFixture, "s1", defaultCwd))
		other := g.put("other", "/allowed-af/proj", "s2", fixture(t, geminiFixture, "s2", "/allowed-af/proj"))
		g.pol.DenyPaths = []string{defaultCwd}
		if s := g.session("s1"); s.State != StateDenied || s.Reason != collect.ReasonDenyPaths || s.Counts != nil {
			t.Fatalf("%+v", s)
		}
		if s := g.session("s2"); s.State != StateNew {
			t.Fatalf("%+v", s)
		}
		out := g.digest(DigestRequest{Refs: []string{gref("s1")}})
		if len(out.Errors) != 1 || out.Errors[0].State != StateDenied {
			t.Fatalf("%+v", out)
		}
		if g.opens[p] != 0 || g.opens[other] == 0 {
			t.Fatalf("opens %v", g.opens)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		p := g.put("proj", defaultCwd, "s1", fixture(t, geminiFixture, "s1", defaultCwd))
		g.pol.Disabled = true
		if s := g.session("s1"); s.State != StateDisabled || g.opens[p] != 0 {
			t.Fatalf("%+v %v", s, g.opens)
		}
	})
	t.Run("no project root", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		g.put("proj", "", "s1", strings.Replace(fixture(t, geminiFixture, "s1", defaultCwd), `"directories":["`+defaultCwd+`"]`, `"directories":[]`, 1))
		if s := g.session("s1"); s.State != StateUnknownProject || len(s.Cwds) != 0 {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("sub-agent sessions not read", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		p := g.put("proj", defaultCwd, "s1", fixture(t, geminiFixture, "s1", defaultCwd))
		sub := filepath.Join(filepath.Dir(p), "parent-id")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "session-sub.jsonl"), []byte(fixture(t, geminiFixture, "sub", defaultCwd)), 0o644); err != nil {
			t.Fatal(err)
		}
		if l := g.list(); len(l.Sessions) != 1 || l.Sessions[0].SessionID != "session-s1" {
			t.Fatalf("%+v", l.Sessions)
		}
	})
	t.Run("store absent", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		l := g.list()
		if len(l.Stores) != 1 || l.Stores[0].State != StateAbsent || l.Stores[0].Location != filepath.Join(g.dir, "tmp") {
			t.Fatalf("%+v", l)
		}
	})
}

func TestGeminiDigestEvents(t *testing.T) {
	t.Parallel()
	g := newGeminiWorld(t)
	g.put("proj", defaultCwd, "s1", fixture(t, geminiFixture, "s1", defaultCwd))
	d := g.digestOne("s1")
	if d.Counts != (Counts{Entries: 12, Prompts: 2, ToolCalls: 7, ToolErrors: 3, Interrupts: 1, Denials: 2, Unparsed: 1}) {
		t.Fatalf("counts %+v", d.Counts)
	}
	// m-0001 is session context and m-0009 only function responses: not
	// prompts. The re-appended m-0003 is one event.
	if got := strings.Join(spans(d), ","); got != "m-0002,m-0003,m-0005,m-0006,m-0006,m-0007,m-0007,m-0008" {
		t.Fatalf("spans %s", got)
	}
	want := []struct {
		status, class string
		exit          int // -1 for none
	}{
		{"", "", -1}, {statusError, "exit", 1}, {statusDenied, "denied", -1}, {statusInterrupted, "interrupted", -1},
		{statusDenied, "denied", -1}, {statusError, "exit", 1}, {statusError, "tool_error", -1}, {"", "", -1},
	}
	for i, w := range want {
		e := d.Events[i]
		if e.Status != w.status || e.ErrorClass != w.class || (e.ExitCode == nil) != (w.exit < 0) || (e.ExitCode != nil && *e.ExitCode != w.exit) {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	if d.Events[1].Tool != "run_shell_command" || d.Events[1].Self || !d.Events[5].Self || d.Events[6].Tool != "read_file" {
		t.Fatalf("%+v", d.Events)
	}
	if !strings.HasPrefix(d.Events[1].Excerpt, "main.go:3: undefined: foo") || strings.Contains(d.Events[1].Excerpt, "untrusted_context") {
		t.Fatalf("excerpt %q", d.Events[1].Excerpt)
	}
	if len(d.Retries) != 1 || d.Retries[0].Span != "m-0004" || d.Retries[0].OfSpan != "m-0003" {
		t.Fatalf("retries %+v", d.Retries)
	}
	if strings.Join(d.Denials, ",") != "m-0005,m-0006" {
		t.Fatalf("denials %+v", d.Denials)
	}
	if d.Model != "gemini-test-2" || d.FinalExcerpt != "Done. The build passes now." || d.Project != defaultCwd ||
		d.Start != "2026-10-08T07:39:40.000Z" || d.End != "2026-10-08T07:40:40.000Z" {
		t.Fatalf("%+v", d)
	}
	var phrases []string
	for _, f := range d.Flagged {
		phrases = append(phrases, f.Span+":"+f.Phrase)
	}
	if got := strings.Join(phrases, ","); got != "m-0002:doesn't work,m-0008:still failing,m-0008:why did you,m-0008:again" {
		t.Fatalf("flagged %s", got)
	}
}

// geminiCall is a gemini message m-1 with one shell call in status, and a
// result when the status is terminal; extra is spliced in at the end.
func geminiCall(status, extra string) string {
	result := ""
	if status == "error" || status == "success" {
		result = `"result":[{"functionResponse":{"id":"c-1","name":"run_shell_command","response":{"error":"boom"}}}],`
	}

	return `{"id":"m-1","timestamp":"2026-10-08T07:00:00.000Z","type":"gemini","content":[],"model":"m","toolCalls":[{"id":"c-1","name":"run_shell_command","args":{"command":"make"},` +
		result + `"status":"` + status + `"}]` + extra + "}\n"
}

const geminiMeta = `{"sessionId":"s","projectHash":"h","startTime":"2026-10-08T06:59:00.000Z","lastUpdated":"2026-10-08T06:59:00.000Z"}` + "\n"

func TestGeminiReappend(t *testing.T) {
	t.Parallel()

	t.Run("finished before the mark", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		p := g.put("proj", defaultCwd, "s1", geminiMeta+geminiCall("executing", "")+geminiCall("error", ""))
		if d := g.digestOne("s1"); strings.Join(spans(d), ",") != "m-1" || d.Counts.Entries != 1 || d.Counts.ToolCalls != 1 {
			t.Fatalf("%+v", d)
		}
		g.mark("s1")
		// Tokens arrive: the file grows, nothing new happened.
		g.appendTo(p, geminiCall("error", `,"tokens":{"total":3}`))
		if s := g.session("s1"); s.State != StateChanged || s.Reason != ChangeAppended {
			t.Fatalf("%+v", s)
		}
		d := g.digestOne("s1")
		if len(d.Events) != 0 || d.Counts != (Counts{}) {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("finished after the mark", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		p := g.put("proj", defaultCwd, "s1", geminiMeta+geminiCall("scheduled", "")+geminiCall("executing", ""))
		if d := g.digestOne("s1"); len(d.Events) != 0 {
			t.Fatalf("%+v", d)
		}
		g.mark("s1")
		g.appendTo(p, geminiCall("error", ""))
		d := g.digestOne("s1")
		if strings.Join(spans(d), ",") != "m-1" || d.Events[0].Status != statusError || d.Counts.Entries != 0 {
			t.Fatalf("%+v", d)
		}
		g.mark("s1")
		// A later rewrite neither moves the result nor changes its status.
		g.appendTo(p, geminiCall("success", ""))
		if d := g.digestOne("s1"); len(d.Events) != 0 {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("set messages checkpoint", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		set := `{"$set":{"messages":[` + strings.TrimSuffix(geminiCall("error", ""), "\n") + `,{"id":"m-2","timestamp":"2026-10-08T07:00:05.000Z","type":"user","content":"again please"}]}}` + "\n"
		g.put("proj", defaultCwd, "s1", geminiMeta+geminiCall("executing", "")+set)
		d := g.digestOne("s1")
		if strings.Join(spans(d), ",") != "m-1,m-2" || d.Counts.Entries != 2 {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("rewound messages kept", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		g.put("proj", defaultCwd, "s1", geminiMeta+geminiCall("error", "")+`{"$rewindTo":"m-1"}`+"\n"+geminiPrompt)
		d := g.digestOne("s1")
		if strings.Join(spans(d), ",") != "m-1,m-0100" || d.Counts.Unparsed != 0 {
			t.Fatalf("%+v", d)
		}
	})
}

func TestGeminiUserContent(t *testing.T) {
	t.Parallel()
	g := newGeminiWorld(t)
	content := geminiMeta +
		`{"id":"u-1","timestamp":"2026-10-08T07:00:00.000Z","type":"user","content":"<session_context>\nctx\n</session_context>"}` + "\n" +
		`{"id":"u-2","timestamp":"2026-10-08T07:00:01.000Z","type":"user","content":"/help"}` + "\n" +
		`{"id":"u-3","timestamp":"2026-10-08T07:00:02.000Z","type":"user","content":[{"functionResponse":{"id":"c","name":"n","response":{"output":"x"}}}]}` + "\n" +
		`{"id":"u-4","timestamp":"2026-10-08T07:00:03.000Z","type":"user","content":[{"text":"a real"},{"text":"prompt"}],"displayContent":"x","extra":{"y":1}}` + "\n" +
		`{"id":"u-5","timestamp":"2026-10-08T07:00:04.000Z","type":"user","content":5}` + "\n" +
		`{"id":"w-6","timestamp":"2026-10-08T07:00:05.000Z","type":"warning","content":"careful"}` + "\n"
	g.put("proj", defaultCwd, "s1", content)
	d := g.digestOne("s1")
	if strings.Join(spans(d), ",") != "u-4" || d.Events[0].Summary != "a real" || d.Counts.Entries != 6 || d.Counts.Prompts != 1 || d.Counts.Unparsed != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestGeminiStatus(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ status, content, want string }{
		{"success", "ok", statusOK},
		{"error", "Command failed with exit code 1.", statusError},
		{"error", "Tool execution denied by policy.", statusDenied},
		{"cancelled", "[Operation Cancelled] Reason: User denied execution.", statusDenied},
		{"cancelled", "[Operation Cancelled] Reason: Operation cancelled by user", statusInterrupted},
		{"cancelled", "User cancelled tool execution.", statusInterrupted},
		{"cancelled", "Operation cancelled.", statusInterrupted},
	} {
		if got := geminiStatus(c.status, c.content); got != c.want {
			t.Fatalf("%s %q: %s, want %s", c.status, c.content, got, c.want)
		}
	}
	if got := geminiResult([]byte(`[{"functionResponse":{"response":{"output":"{\n  \"error\": \"x\"\n}"}}},{"text":"t"},{"functionResponse":{"response":{"error":"e","output":"o"}}}]`)); got != "{\n  \"error\": \"x\"\n}\ne" {
		t.Fatalf("%q", got)
	}
}

func TestGeminiShell(t *testing.T) {
	t.Parallel()
	wrap := func(s string) string { return "<untrusted_context>\n" + s + "\n</untrusted_context>" }
	for _, c := range []struct {
		content, status, text, want string
		exit                        int // -1 for none
	}{
		{wrap("Output: ok\nProcess Group PGID: 1"), statusOK, "ok\nProcess Group PGID: 1", statusOK, -1},
		{wrap("Output: bad\nExit Code: 2\nProcess Group PGID: 1"), statusOK, "bad\nExit Code: 2\nProcess Group PGID: 1", statusError, 2},
		{wrap("Output: (empty)\nExit Code: 0"), statusOK, "(empty)\nExit Code: 0", statusOK, -1},
		{wrap("Output: (empty)\nSignal: SIGKILL"), statusOK, "(empty)\nSignal: SIGKILL", statusError, -1},
		{wrap("Output: (empty)\nError: spawn failed\nExit Code: 127"), statusError, "(empty)\nError: spawn failed\nExit Code: 127", statusError, 127},
		{"Command was cancelled by user before it could start.", statusInterrupted, "Command was cancelled by user before it could start.", statusInterrupted, -1},
		{wrap("Output: partial\nExit Code: 130"), statusInterrupted, "partial\nExit Code: 130", statusInterrupted, -1},
		// Lines the command printed are not the trailer.
		{wrap("Output: Exit Code: 0\nExit Code: 0\ndone\nExit Code: 2\nProcess Group PGID: 1"), statusOK, "Exit Code: 0\nExit Code: 0\ndone\nExit Code: 2\nProcess Group PGID: 1", statusError, 2},
		{wrap("Output: Exit Code: 3\ndone\nProcess Group PGID: 1"), statusOK, "Exit Code: 3\ndone\nProcess Group PGID: 1", statusOK, -1},
		{wrap("Output: Signal: x\ndone\nExit Code: 0\nProcess Group PGID: 1"), statusOK, "Signal: x\ndone\nExit Code: 0\nProcess Group PGID: 1", statusOK, -1},
		{wrap("Output: Signal: x\ndone"), statusOK, "Signal: x\ndone", statusOK, -1},
	} {
		text, status, exit := geminiShell(c.content, c.status)
		if text != c.text || status != c.want || (exit == nil) != (c.exit < 0) || (exit != nil && *exit != c.exit) {
			t.Fatalf("%q: %q %s %v", c.content, text, status, exit)
		}
	}
}

func TestGeminiLocate(t *testing.T) {
	t.Parallel()
	g := newGeminiWorld(t)
	g.put("proj", defaultCwd, "s1", fixture(t, geminiFixture, "s1", defaultCwd))
	f, err := Locate(g.ctx, g.env(), gref("s1"), "m-0008")
	if err != nil {
		t.Fatal(err)
	}
	if f.Model != "gemini-test-2" || f.Cwd != defaultCwd || f.CwdExists || f.At.Format(timeLayout) != "2026-10-08T07:40:30.000Z" || f.State != "" ||
		f.Detector != "gemini-cli-jsonl/1" {
		t.Fatalf("%+v", f)
	}
	// A re-appended message keeps the time of its first write; a rewound
	// one is still there.
	if f, err := Locate(g.ctx, g.env(), gref("s1"), "m-0003"); err != nil || f.Model != "gemini-test-1" || f.At.Format(timeLayout) != "2026-10-08T07:39:50.000Z" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := Locate(g.ctx, g.env(), gref("s1"), "m-0010"); err != nil {
		t.Fatal(err)
	}
	if _, err := Locate(g.ctx, g.env(), gref("s1"), "nope"); !errors.Is(err, ErrSpanNotFound) {
		t.Fatalf("got %v", err)
	}
	env := g.env()
	env.Policy.DenyPaths = []string{defaultCwd}
	var refused *RefusedError
	if _, err := Locate(g.ctx, env, gref("s1"), "m-0008"); !errors.As(err, &refused) || refused.State != StateDenied {
		t.Fatalf("got %v", err)
	}
}

func TestGeminiSlashCommand(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]bool{
		"/help":                 true,
		"/memory add x":         true,
		"  /chat-save_2 now":    true,
		"/home/u/x fix this":    false,
		"/":                     false,
		"/1abc":                 false,
		"fix /home/u/x":         false,
		"/dir add /nonexistent": true,
	} {
		if got := geminiSlashCommand(text); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
	g := newGeminiWorld(t)
	content := `{"sessionId":"s1","startTime":"2026-10-08T07:39:40.000Z","kind":"main"}
{"id":"m-1","timestamp":"2026-10-08T07:39:41.000Z","type":"user","content":[{"text":"/home/u/x fix this"}]}
{"id":"m-2","timestamp":"2026-10-08T07:39:42.000Z","type":"user","content":[{"text":"/help"}]}
`
	g.put("proj", defaultCwd, "s1", content)
	if s := g.session("s1"); s.Counts == nil || s.Counts.Prompts != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestGeminiProjectRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := projectRoot(sub); got != "" || err != nil {
		t.Fatalf("directory: %q %v", got, err)
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(defaultCwd+strings.Repeat(" ", projectRootMax)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := projectRoot(big); got != "" || err != nil {
		t.Fatalf("oversized: %q %v", got, err)
	}
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte(defaultCwd+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := projectRoot(ok); got != defaultCwd || err != nil {
		t.Fatalf("regular: %q %v", got, err)
	}

	t.Run("not read when disabled", func(t *testing.T) {
		t.Parallel()
		g := newGeminiWorld(t)
		g.put("proj", defaultCwd, "s1", fixture(t, geminiFixture, "s1", defaultCwd))
		// A .project_root that cannot be read would be a problem.
		root := filepath.Join(g.dir, "tmp", "proj", ".project_root")
		if err := os.Remove(root); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(root, root); err != nil {
			t.Fatal(err)
		}
		if l := g.list(); len(l.Stores[0].Problems) != 1 {
			t.Fatalf("enabled: %+v", l)
		}
		g.pol.Disabled = true
		l := g.list()
		if len(l.Stores[0].Problems) != 0 || len(l.Sessions) != 1 || l.Sessions[0].State != StateDisabled {
			t.Fatalf("disabled: %+v", l)
		}
	})
}

func TestGeminiDirectories(t *testing.T) {
	t.Parallel()
	g := newGeminiWorld(t)
	content := `{"sessionId":"s1","startTime":"2026-10-08T07:39:40.000Z","kind":"main","directories":["/nonexistent-af/extra","relative","` + defaultCwd + `"]}
{"id":"m-1","timestamp":"2026-10-08T07:39:41.000Z","type":"user","content":[{"text":"hi"}]}
`
	g.put("proj", defaultCwd, "s1", content)
	if s := g.session("s1"); strings.Join(s.Cwds, ",") != defaultCwd+",/nonexistent-af/extra" || s.Project != defaultCwd {
		t.Fatalf("%+v", s)
	}
	g.pol.DenyPaths = []string{"/nonexistent-af/extra"}
	if s := g.session("s1"); s.State != StateDenied {
		t.Fatalf("%+v", s)
	}
}
