package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/agentfeedback/agentfeedback/v4/internal/detect"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// cxSID is the {{SID}} of the codex fixture store: its rollouts append a
// digit to it.
const cxSID = "0199c3a1-0000-7000-8000-00000000000"

const (
	cxLegacy    = cxSID + "1"
	cxPaginated = cxSID + "2_" + cxSID + "8"
	cxArchived  = cxSID + "3"
	cxZst       = cxSID + "4"
)

// cxWorld is a world whose Codex home holds the codex fixture store.
type cxWorld struct {
	*world
	codex string
	paths map[string]string // session id -> rollout path
	open  func(p string) (io.ReadCloser, error)
}

func newCodexWorld(t *testing.T, cwd string) *cxWorld {
	t.Helper()
	w := &cxWorld{world: newWorld(t), paths: map[string]string{}}
	w.codex = filepath.Join(w.home, ".codex")
	src := filepath.Join("testdata", "stores", "codex")
	fill := strings.NewReplacer("{{SID}}", cxSID, "{{CWD}}", cwd)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(w.codex, fill.Replace(rel))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if id, _, ok := codexSessionID(filepath.Base(out)); ok {
			w.paths[id] = out
		}

		return os.WriteFile(out, []byte(fill.Replace(string(b))), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}

	return w
}

func (w *cxWorld) env() Env {
	e := w.world.env()
	e.CodexHome = w.codex
	if w.open != nil {
		inner := e.Open
		e.Open = func(p string) (io.ReadCloser, error) {
			f, err := inner(p)
			if err != nil {
				return nil, err
			}
			_ = f.Close()

			return w.open(p)
		}
	}

	return e
}

func (w *cxWorld) list() Listing {
	w.t.Helper()
	l, err := List(w.ctx, w.db, w.env(), ListOptions{Harness: HarnessCodex})
	if err != nil {
		w.t.Fatal(err)
	}

	return l
}

func (w *cxWorld) session(sid string) Session {
	w.t.Helper()
	for _, s := range w.list().Sessions {
		if s.SessionID == sid {
			return s
		}
	}
	w.t.Fatalf("session %s not listed", sid)

	return Session{}
}

func (w *cxWorld) digestOne(sid string) SessionDigest {
	w.t.Helper()
	out, err := Digest(w.ctx, w.db, w.env(), DigestRequest{Refs: []string{Ref(HarnessCodex, sid)}})
	if err != nil {
		w.t.Fatal(err)
	}
	if len(out.Sessions) != 1 {
		w.t.Fatalf("digest of %s: %+v", sid, out)
	}

	return out.Sessions[0]
}

func (w *cxWorld) mark(sid string) {
	w.t.Helper()
	if err := Mark(w.ctx, w.db, w.env(), []string{Ref(HarnessCodex, sid)}, OutcomeNothing, nil); err != nil {
		w.t.Fatal(err)
	}
}

func (w *cxWorld) content(sid string) string {
	w.t.Helper()
	b, err := os.ReadFile(w.paths[sid])
	if err != nil {
		w.t.Fatal(err)
	}

	return string(b)
}

func (w *cxWorld) write(sid, content string) {
	w.t.Helper()
	if err := os.WriteFile(w.paths[sid], []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// cxLines splits a rollout into its lines, each with its '\n'.
func cxLines(s string) []string { return strings.SplitAfter(strings.TrimSuffix(s, "\n"), "\n") }

// cxLineSpan is the span of the 0-based line i of content.
func cxLineSpan(content string, i int) string {
	return "L" + strconv.Itoa(len(strings.Join(cxLines(content)[:i], "")))
}

// cxPrompt1 and cxPrompt2 are the 0-based lines of the legacy rollout's
// prompts.
const (
	cxPrompt1 = 5
	cxPrompt2 = 19
)

// cxCallIndex is the 0-based line of the legacy rollout carrying the
// function call of call_04, the call the turn_aborted after it interrupts.
const cxCallIndex = 15

func TestCodexStates(t *testing.T) {
	t.Parallel()

	t.Run("new", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		l := w.list()
		if len(l.Stores) != 1 || l.Stores[0].State != StorePresent || l.Stores[0].Location != filepath.Join(w.codex, "sessions") {
			t.Fatalf("%+v", l.Stores)
		}
		var got []string
		for _, s := range l.Sessions {
			got = append(got, s.SessionID+"="+s.State)
		}
		if strings.Join(got, ",") != cxArchived+"=new,"+cxLegacy+"=new,"+cxPaginated+"=new,"+cxZst+"=unsupported-format" {
			t.Fatalf("%s", strings.Join(got, ","))
		}
		s := w.session(cxLegacy)
		if s.Project != defaultCwd || s.Counts == nil || s.Counts.Prompts != 2 || s.Counts.Unparsed != 1 {
			t.Fatalf("%+v %+v", s, s.Counts)
		}
	})
	t.Run("processed", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		w.digestOne(cxLegacy)
		w.mark(cxLegacy)
		if s := w.session(cxLegacy); s.State != StateProcessed {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("changed: an appended result of an earlier call", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		full := w.content(cxLegacy)
		lines := cxLines(full)
		head := strings.Join(lines[:cxCallIndex+1], "")
		w.write(cxLegacy, head)
		if d := w.digestOne(cxLegacy); strings.Join(spans(d), ",") != cxLineSpan(full, cxPrompt1)+",call_01,call_03" {
			t.Fatalf("first digest %v", spans(d))
		}
		w.mark(cxLegacy)
		w.write(cxLegacy, head+strings.Join(lines[cxCallIndex+1:], ""))
		if s := w.session(cxLegacy); s.State != StateChanged || s.Reason != ChangeAppended {
			t.Fatalf("%+v", s)
		}
		d := w.digestOne(cxLegacy)
		if d.FromOffset != int64(len(head)) || strings.Join(spans(d), ",") != "call_04,"+cxLineSpan(full, cxPrompt2)+",call_05,call_06" {
			t.Fatalf("from %d spans %v", d.FromOffset, spans(d))
		}
		if d.Events[0].Status != statusInterrupted || len(d.Retries) != 0 || len(d.Denials) != 0 {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("partial last line", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		content := w.content(cxArchived)
		extra := `{"timestamp":"2026-10-07T21:00:06.000Z","type":"event_msg","payload":{"type":"user_message","message":"Still failing."}}`
		w.write(cxArchived, content+extra)
		d := w.digestOne(cxArchived)
		if d.Counts.Entries != 6 || d.Counts.Prompts != 1 {
			t.Fatalf("partial line read: %+v", d.Counts)
		}
		w.mark(cxArchived)
		if s := w.session(cxArchived); s.State != StateProcessed {
			t.Fatalf("a partial tail must not count: %+v", s)
		}
		w.write(cxArchived, content+extra+"\n")
		if s := w.session(cxArchived); s.State != StateChanged || s.Reason != ChangeAppended {
			t.Fatalf("%+v", s)
		}
		if d := w.digestOne(cxArchived); d.FromOffset != int64(len(content)) || len(d.Events) != 1 || d.Events[0].Summary != "Still failing." {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("truncation", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		full := w.content(cxLegacy)
		lines := cxLines(full)
		w.digestOne(cxLegacy)
		w.mark(cxLegacy)
		w.write(cxLegacy, strings.Join(lines[:6], ""))
		if s := w.session(cxLegacy); s.State != StateChanged || s.Reason != ChangeTruncated {
			t.Fatalf("%+v", s)
		}
		if d := w.digestOne(cxLegacy); d.FromOffset != 0 || strings.Join(spans(d), ",") != cxLineSpan(full, cxPrompt1) {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("rotation", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		content := w.content(cxLegacy)
		w.digestOne(cxLegacy)
		w.mark(cxLegacy)
		w.write(cxLegacy, strings.Replace(content, `"cli_version":"0.161.0"`, `"cli_version":"0.162.0"`, 1))
		if s := w.session(cxLegacy); s.State != StateChanged || s.Reason != ChangeRotated {
			t.Fatalf("%+v", s)
		}
		if d := w.digestOne(cxLegacy); d.FromOffset != 0 || d.Events[0].Span != cxLineSpan(content, cxPrompt1) {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("unsupported-format", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		w.write(cxArchived, "not json\n[1,2]\n{\"type\":7}\n")
		if s := w.session(cxArchived); s.State != StateUnsupportedFormat {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("compressed rollout", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		s := w.session(cxZst)
		if s.State != StateUnsupportedFormat || s.Reason != "compressed rollout" || s.Counts != nil {
			t.Fatalf("%+v", s)
		}
		if w.opens[w.paths[cxZst]] != 0 {
			t.Fatalf("compressed rollout opened %d times", w.opens[w.paths[cxZst]])
		}
		if _, err := (codexReader{}).load(w.env(), candidate{path: w.paths[cxZst]}); !errors.Is(err, errUnsupported) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("denied from the first line, the rest unread", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, "/secret-af/x")
		// Every rollout yields its first line, then fails.
		w.open = func(p string) (io.ReadCloser, error) {
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			first, _, _ := bytes.Cut(b, []byte("\n"))

			return &firstLineOnly{data: append(first, '\n')}, nil
		}
		w.pol.DenyPaths = []string{"/secret-af"}
		p := w.paths[cxLegacy]
		if s := w.session(cxLegacy); s.State != StateDenied || s.Reason != collect.ReasonDenyPaths || s.Counts != nil || w.opens[p] != 1 {
			t.Fatalf("%+v opens %d", s, w.opens[p])
		}
		out, err := Digest(w.ctx, w.db, w.env(), DigestRequest{Refs: []string{Ref(HarnessCodex, cxLegacy)}})
		if err != nil || len(out.Errors) != 1 || out.Errors[0].State != StateDenied {
			t.Fatalf("%+v %v", out, err)
		}
		// The same reader fails a full read: the denial never reached it.
		w.pol.DenyPaths = nil
		if s := w.session(cxLegacy); s.State != StateUnreadable {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		w.pol.Disabled = true
		for _, s := range w.list().Sessions {
			if s.State != StateDisabled {
				t.Fatalf("%+v", s)
			}
		}
		if len(w.opens) != 0 {
			t.Fatalf("opens %v", w.opens)
		}
	})
	t.Run("store absent", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		if err := os.RemoveAll(w.codex); err != nil {
			t.Fatal(err)
		}
		l := w.list()
		if len(l.Stores) != 1 || l.Stores[0].State != StateAbsent || len(l.Sessions) != 0 {
			t.Fatalf("%+v", l)
		}
	})
	t.Run("archive only", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		if err := os.RemoveAll(filepath.Join(w.codex, "sessions")); err != nil {
			t.Fatal(err)
		}
		l := w.list()
		if len(l.Stores) != 1 || l.Stores[0].State != StorePresent || len(l.Sessions) != 1 || l.Sessions[0].SessionID != cxArchived {
			t.Fatalf("%+v", l)
		}
	})
}

// firstLineOnly yields data on its first read and fails every later one.
type firstLineOnly struct {
	data []byte
}

func (r *firstLineOnly) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errors.New("read past the first line")
	}
	n := copy(p, r.data)
	r.data = r.data[n:]

	return n, nil
}

func (r *firstLineOnly) Close() error { return nil }

func cxParse(t *testing.T, content string) (*transcript, map[string]*call) {
	t.Helper()
	tr, err := codexReader{}.parse(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]*call{}
	for _, e := range tr.entries {
		for _, c := range e.calls {
			calls[c.id] = c
		}
	}

	return tr, calls
}

func TestCodexStatuses(t *testing.T) {
	t.Parallel()
	w := newCodexWorld(t, defaultCwd)
	exit := func(n int) *int { return &n }
	type want struct {
		status string
		exit   *int
	}
	check := func(t *testing.T, calls map[string]*call, wants map[string]want) {
		t.Helper()
		for id, wt := range wants {
			c := calls[id]
			if c == nil || !c.result || c.status != wt.status {
				t.Fatalf("%s: %+v, want %s", id, c, wt.status)
			}
			if (wt.exit == nil) != (c.exitCode == nil) || (wt.exit != nil && *wt.exit != *c.exitCode) {
				t.Fatalf("%s: exit %v, want %v", id, c.exitCode, wt.exit)
			}
		}
	}

	t.Run("legacy", func(t *testing.T) {
		t.Parallel()
		tr, calls := cxParse(t, w.content(cxLegacy))
		check(t, calls, map[string]want{
			"call_01": {statusError, exit(1)}, "call_02": {statusOK, exit(0)}, "call_03": {statusDenied, nil},
			"call_04": {statusInterrupted, nil}, "call_05": {statusError, exit(1)}, "call_06": {statusError, exit(2)},
			"call_07": {statusOK, exit(0)},
		})
		if calls["call_06"].content != "FAIL pkg/x" || calls["call_03"].command != "rm -rf build" || !calls["call_03"].hasCommand ||
			calls["call_01"].command != "go build ./..." || calls["call_07"].hasCommand {
			t.Fatalf("%+v %+v %+v", calls["call_06"], calls["call_03"], calls["call_07"])
		}
		if c := tr.counts(0); c.Prompts != 2 || c.Interrupts != 2 || c.Denials != 1 || c.ToolErrors != 3 {
			t.Fatalf("%+v", c)
		}
		if tr.sessionID != cxLegacy || tr.model() != "gpt-test-2" {
			t.Fatalf("%q %q", tr.sessionID, tr.model())
		}
	})
	t.Run("paginated", func(t *testing.T) {
		t.Parallel()
		content := w.content(cxPaginated)
		lines := cxLines(content)
		offsetOf := func(i int) int64 { return int64(len(strings.Join(lines[:i], ""))) }
		tr, calls := cxParse(t, content)
		check(t, calls, map[string]want{
			"call_p1": {statusError, exit(1)}, "call_p2": {statusDenied, nil}, "call_p3": {statusError, nil},
			"call_p4": {statusOK, nil}, "call_p5": {statusInterrupted, nil},
		})
		// The output line keeps the result; a completion record alone sets it.
		if calls["call_p1"].resultOffset != offsetOf(5) || calls["call_p2"].resultOffset != offsetOf(8) {
			t.Fatalf("%d %d", calls["call_p1"].resultOffset, calls["call_p2"].resultOffset)
		}
		if calls["call_p3"].content != "spawn failed" {
			t.Fatalf("%+v", calls["call_p3"])
		}
		if c := tr.counts(0); c.Prompts != 1 || tr.sessionID != cxSID+"2" {
			t.Fatalf("%+v %q", c, tr.sessionID)
		}
	})
	t.Run("legacy completion events", func(t *testing.T) {
		t.Parallel()
		content := `{"timestamp":"2026-10-08T07:00:00.000Z","type":"session_meta","payload":{"id":"x","cwd":"/nonexistent-af/proj"}}
{"timestamp":"2026-10-08T07:00:01.000Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"p1","name":"apply_patch","input":"*** Begin Patch"}}
{"timestamp":"2026-10-08T07:00:02.000Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"p1","success":false,"status":"declined","stdout":"","stderr":"patch rejected"}}
{"timestamp":"2026-10-08T07:00:03.000Z","type":"response_item","payload":{"type":"function_call","name":"docs__search","arguments":"{}","call_id":"m1"}}
{"timestamp":"2026-10-08T07:00:04.000Z","type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"m1","invocation":{},"result":{"Err":"server gone"}}}
{"timestamp":"2026-10-08T07:00:05.000Z","type":"response_item","payload":{"type":"function_call","name":"docs__search","arguments":"{}","call_id":"m2"}}
{"timestamp":"2026-10-08T07:00:06.000Z","type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"m2","invocation":{},"result":{"Ok":{"content":[],"isError":false}}}}
{"timestamp":"2026-10-08T07:00:07.000Z","type":"response_item","payload":{"type":"local_shell_call","call_id":"s1","status":"completed","action":{"type":"exec","command":["false"]}}}
{"timestamp":"2026-10-08T07:00:08.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"s1","output":"Exit code: 1\nWall time: 0.0 seconds\nOutput:\n"}}
{"timestamp":"2026-10-08T07:00:09.000Z","type":"event_msg","payload":{"type":"turn_aborted","reason":"replaced"}}
`
		tr, calls := cxParse(t, content)
		check(t, calls, map[string]want{
			"p1": {statusDenied, nil}, "m1": {statusError, nil}, "m2": {statusOK, nil}, "s1": {statusError, exit(1)},
		})
		if calls["p1"].content != "patch rejected" || calls["m1"].content != "server gone" || calls["s1"].command != "false" || !calls["s1"].hasCommand {
			t.Fatalf("%+v %+v %+v", calls["p1"], calls["m1"], calls["s1"])
		}
		if c := tr.counts(0); c.Interrupts != 0 {
			t.Fatalf("a replaced turn is not an interrupt: %+v", c)
		}
	})
}

func TestCodexOutput(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		`"Chunk ID: 1\nWall time: 0.1 seconds\nProcess exited with code 3\nOutput:\nline 1\nOutput:\nline 2"`: "line 1\nOutput:\nline 2",
		`"Exit code: 0\nWall time: 0.1 seconds\nOutput:\n"`:                                                   "",
		`"Wall time: 1.2 seconds\naborted by user"`:                                                           "Wall time: 1.2 seconds\naborted by user",
	} {
		if content, _, _ := codexOutput([]byte(raw)); content != want {
			t.Fatalf("%s: content %q, want %q", raw, content, want)
		}
	}
	for _, tc := range []struct {
		output, status string
		exit           int // -1: none
	}{
		{`"Chunk ID: 1\nWall time: 0.1 seconds\nProcess exited with code 3\nOutput:\nx"`, statusError, 3},
		{`"Chunk ID: 1\nWall time: 0.1 seconds\nOutput:\nstill running"`, statusOK, -1},
		{`"Exit code: 0\nWall time: 0.1 seconds\nOutput:\nExit code: 9"`, statusOK, 0},
		{`"{\"output\":\"x\",\"metadata\":{\"exit_code\":0,\"duration_seconds\":0.1}}"`, statusOK, 0},
		{`"patch rejected by user"`, statusDenied, -1},
		{`"rejected by configuration"`, statusDenied, -1},
		{`"automatic approval review denied the action"`, statusDenied, -1},
		{`"aborted by user after 1.2s"`, statusInterrupted, -1},
		{`"Wall time: 1.2 seconds\naborted by user"`, statusInterrupted, -1},
		{`[{"type":"input_text","text":"Process exited with code 4"},{"type":"input_image","image_url":"data:"}]`, statusError, 4},
	} {
		_, status, exit := codexOutput([]byte(tc.output))
		got := -1
		if exit != nil {
			got = *exit
		}
		if status != tc.status || got != tc.exit {
			t.Fatalf("%s: %s %d, want %s %d", tc.output, status, got, tc.status, tc.exit)
		}
	}
}

func TestCodexLocate(t *testing.T) {
	t.Parallel()
	w := newCodexWorld(t, defaultCwd)
	for _, tc := range []struct{ ref, span, at, model string }{
		{Ref(HarnessCodex, cxLegacy), "call_05", "2026-10-08T07:40:31.000Z", "gpt-test-2"},
		{Ref(HarnessCodex, cxLegacy), cxLineSpan(w.content(cxLegacy), cxPrompt1), "2026-10-08T07:39:41.300Z", "gpt-test-1"},
		{Ref(HarnessCodex, cxPaginated), "item-u1", "2026-10-08T08:10:03.000Z", "gpt-test-3"},
	} {
		f, err := Locate(w.ctx, w.env(), tc.ref, tc.span)
		if err != nil {
			t.Fatal(err)
		}
		if f.Model != tc.model || f.Cwd != defaultCwd || f.At.Format(timeLayout) != tc.at || f.Detector != "codex-rollout/1" {
			t.Fatalf("%s: %+v", tc.span, f)
		}
	}
	if _, err := Locate(w.ctx, w.env(), Ref(HarnessCodex, cxLegacy), "nope"); !errors.Is(err, ErrSpanNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := Locate(w.ctx, w.env(), Ref(HarnessCodex, cxZst), "L0"); err == nil {
		t.Fatal("a compressed rollout located")
	}
}

func TestCodexUnknownFields(t *testing.T) {
	t.Parallel()
	content := `{"timestamp":"2026-10-08T07:00:00.000Z","ordinal":1,"type":"session_meta","payload":{"id":"x","cwd":"/nonexistent-af/proj","future":{"a":1}},"metadata":{"k":"v"}}
{"timestamp":"2026-10-08T07:00:01.000Z","type":"inter_agent_communication_v2","payload":{"anything":[1,2]}}
{"timestamp":"2026-10-08T07:00:02.000Z","type":"event_msg","payload":{"type":"user_message","message":"Hello","kind":"plain","extra":true}}
{"timestamp":"2026-10-08T07:00:03.000Z","type":"response_item","payload":{"type":"message","role":["odd"],"content":"not a list"}}
{"timestamp":7,"type":"compacted","payload":null}
{"type":"event_msg"}
`
	tr, _ := cxParse(t, content)
	if tr.parsed != 6 || len(tr.badLines) != 0 {
		t.Fatalf("parsed %d bad %v", tr.parsed, tr.badLines)
	}
	if c := tr.counts(0); c.Prompts != 1 || c.Entries != 6 {
		t.Fatalf("%+v", c)
	}
}

func TestCodexSessionID(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"rollout-2026-10-08T07-39-40-0199c3a1-0000-7000-8000-000000000001.jsonl":     "0199c3a1-0000-7000-8000-000000000001",
		"rollout-2026-10-08T07-39-40-0199c3a1-0000-7000-8000-000000000001_r2.jsonl":  "0199c3a1-0000-7000-8000-000000000001_r2",
		"rollout-2026-10-08T07-39-40-0199c3a1-0000-7000-8000-000000000001.jsonl.zst": "0199c3a1-0000-7000-8000-000000000001",
		"rollout-2026-10-08T07-39-40.jsonl":                                          "",
		"rollout-yesterday-0199c3a1-0000-7000-8000-000000000001.jsonl":               "",
		"session-2026-10-08T07-39-40-0199c3a1-0000-7000-8000-000000000001.jsonl":     "",
		"rollout-2026-10-08T07-39-40-0199c3a1-0000-7000-8000-000000000001.jsonl.bak": "",
	} {
		id, _, ok := codexSessionID(name)
		if ok != (want != "") || id != want {
			t.Fatalf("%s: %q %v, want %q", name, id, ok, want)
		}
	}
}

// TestCodexWatermarkSize: the watermark covers the complete lines.
func TestCodexWatermarkSize(t *testing.T) {
	t.Parallel()
	w := newCodexWorld(t, defaultCwd)
	w.digestOne(cxArchived)
	var row store.SessionSeen
	if err := w.db.Read(w.ctx, func(q store.Querier) (err error) {
		row, _, err = store.GetSession(w.ctx, q, HarnessCodex, cxArchived)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if row.Size != int64(len(w.content(cxArchived))) {
		t.Fatalf("size %d", row.Size)
	}
}

func TestCodexShellScript(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		argv string
		want string
		self bool
	}{
		{`["bash","-lc","agentfeedback submit friction"]`, "agentfeedback submit friction", true},
		{`["/bin/zsh","-c","go test ./..."]`, "go test ./...", false},
		{`["sh","-cl","agentfeedback flush"]`, "agentfeedback flush", true},
		{`["bash","-lc","agentfeedback flush","extra"]`, "bash -lc agentfeedback flush extra", false},
		{`["fish","-c","agentfeedback flush"]`, "fish -c agentfeedback flush", false},
		{`["bash","-x","agentfeedback flush"]`, "bash -x agentfeedback flush", false},
	} {
		got, ok := argvCommand(json.RawMessage(tc.argv))
		if !ok || got != tc.want || detect.IsSelf(got) != tc.self {
			t.Errorf("%s: %q %v self %v, want %q self %v", tc.argv, got, ok, detect.IsSelf(got), tc.want, tc.self)
		}
	}
}

// countingReader counts the bytes read from it.
type countingReader struct {
	r io.Reader
	n *int
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += n

	return n, err
}

func (countingReader) Close() error { return nil }

const cxMeta = `{"timestamp":"2026-10-08T07:00:00.000Z","type":"session_meta","payload":{"id":"x","cwd":"` + defaultCwd + `"}}`

// cxCandidate is the listed candidate of session sid.
func cxCandidate(t *testing.T, w *cxWorld, sid string) candidate {
	t.Helper()
	cands, _, err := codexReader{}.candidates(w.env())
	if err != nil {
		t.Fatal(err)
	}
	c, ok := find(cands, sid)
	if !ok {
		t.Fatalf("%s not listed", sid)
	}

	return c
}

func TestCodexMetaReadStopsAtNewline(t *testing.T) {
	t.Parallel()
	w := newCodexWorld(t, defaultCwd)
	content := w.content(cxArchived)
	first, _, _ := strings.Cut(content, "\n")
	consumed := 0
	w.open = func(string) (io.ReadCloser, error) {
		return countingReader{r: strings.NewReader(content), n: &consumed}, nil
	}
	c := cxCandidate(t, w, cxArchived)
	if state, _ := (codexReader{}).gate(w.env(), c); state != "" || c.gated.cwd != defaultCwd {
		t.Fatalf("%s %+v", state, c.gated)
	}
	if consumed != len(first)+1 {
		t.Fatalf("consumed %d bytes, the first line has %d", consumed, len(first)+1)
	}
}

func TestCodexMetaFailClosed(t *testing.T) {
	t.Parallel()
	big := `{"timestamp":"2026-10-08T07:00:00.000Z","type":"session_meta","payload":{"id":"x","cwd":"` + defaultCwd + `","base_instructions":"` + strings.Repeat("a", codexFirstLineCap) + `"}}`
	rest := `{"timestamp":"2026-10-08T07:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"hi"}}` + "\n"
	for _, tc := range []struct {
		name, first string
		policy      func(w *cxWorld)
		state       string
	}{
		{"oversized, deny_paths", big, func(w *cxWorld) { w.pol.DenyPaths = []string{"/elsewhere-af"} }, StateDenied},
		{"not session_meta, opt_in_only", rest[:len(rest)-1], func(w *cxWorld) {
			w.pol.OptInOnly, w.pol.OptInPaths = true, []string{defaultCwd}
		}, StateDenied},
		{"undecodable, deny_paths", `{"type":"session_meta",`, func(w *cxWorld) { w.pol.DenyPaths = []string{"/elsewhere-af"} }, StateDenied},
		{"oversized, no policy", big, func(*cxWorld) {}, StateNew},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newCodexWorld(t, defaultCwd)
			w.write(cxArchived, tc.first+"\n"+rest)
			tc.policy(w)
			p := w.paths[cxArchived]
			s := w.session(cxArchived)
			if s.State != tc.state {
				t.Fatalf("%+v", s)
			}
			if tc.state == StateDenied && (s.Reason != codexMetaUnread || w.opens[p] != 1) {
				t.Fatalf("%+v opens %d", s, w.opens[p])
			}
			if tc.state == StateNew && (s.Counts == nil || s.Counts.Prompts != 1) {
				t.Fatalf("%+v", s)
			}
		})
	}
}

func TestCodexMetaChangedBeforeLoad(t *testing.T) {
	t.Parallel()
	w := newCodexWorld(t, defaultCwd)
	content := w.content(cxArchived)
	changed := strings.Replace(content, `"cwd":"`+defaultCwd+`"`, `"cwd":"/nonexistent-af/other"`, 1)
	opens := 0
	w.open = func(string) (io.ReadCloser, error) {
		opens++
		if opens == 1 {
			return io.NopCloser(strings.NewReader(content)), nil
		}

		return io.NopCloser(strings.NewReader(changed)), nil
	}
	c := cxCandidate(t, w, cxArchived)
	ev := read(w.env(), codexReader{}, c, nil, false)
	if ev.state != StateUnreadable || ev.reason != "the session changed since its policy was checked" || ev.tr != nil {
		t.Fatalf("%s %s", ev.state, ev.reason)
	}
	// Unchanged, the same candidate reads.
	w.open = nil
	if ev := read(w.env(), codexReader{}, c, nil, false); ev.state != StateNew {
		t.Fatalf("%s %s", ev.state, ev.reason)
	}
}

func TestCodexRefinement(t *testing.T) {
	t.Parallel()
	call := `{"timestamp":"2026-10-08T07:00:01.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"make\"}","call_id":"c1"}}` + "\n"
	output := `{"timestamp":"2026-10-08T07:00:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"Process exited with code 0\nOutput:\nok"}}` + "\n"
	failed := `{"timestamp":"2026-10-08T07:00:03.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"c1","status":"failed","exit_code":1,"aggregated_output":"boom"}}}` + "\n"

	t.Run("a status change after the watermark is reported", func(t *testing.T) {
		t.Parallel()
		w := newCodexWorld(t, defaultCwd)
		head := cxMeta + "\n" + call + output
		w.write(cxArchived, head)
		if d := w.digestOne(cxArchived); len(d.Events) != 0 {
			t.Fatalf("%+v", d.Events)
		}
		w.mark(cxArchived)
		w.write(cxArchived, head+failed)
		d := w.digestOne(cxArchived)
		if d.FromOffset != int64(len(head)) || len(d.Events) != 1 || d.Events[0].Span != "c1" || d.Events[0].Status != statusError {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("a confirmation keeps the offset, a missing exit keeps the exit code", func(t *testing.T) {
		t.Parallel()
		errOut := `{"timestamp":"2026-10-08T07:00:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"Process exited with code 2\nOutput:\nboom"}}` + "\n"
		noExit := `{"timestamp":"2026-10-08T07:00:03.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"c1","status":"failed","aggregated_output":"boom"}}}` + "\n"
		content := cxMeta + "\n" + call + errOut + noExit
		_, calls := cxParse(t, content)
		c := calls["c1"]
		if c.status != statusError || c.resultOffset != int64(len(cxMeta)+1+len(call)) || c.exitCode == nil || *c.exitCode != 2 {
			t.Fatalf("%+v %v", c, c.exitCode)
		}
	})
}

func TestCodexOutputHarnessTextOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		output, status string
		exit           int // -1: none
	}{
		{`"Exit code: 1\nWall time: 0.1 seconds\nOutput:\nexec command rejected by user"`, statusError, 1},
		{`"Exit code: 0\nWall time: 0.1 seconds\nOutput:\naborted by user"`, statusOK, 0},
		{`"Process exited with code 2\nOutput:\naborted by user after 1s"`, statusError, 2},
		{`"Chunk ID: 1\nOutput:\npatch rejected by user"`, statusOK, -1},
		{`"note: patch rejected by user"`, statusOK, -1},
		{`"  patch rejected by user: no"`, statusDenied, -1},
		{`"Exit code: 0\naborted by user"`, statusOK, 0},
		{`"done; aborted by user"`, statusOK, -1},
		{`"{\"output\":\"exec command rejected by user\",\"metadata\":{\"exit_code\":1}}"`, statusError, 1},
		{`"{\"output\":\"aborted by user\",\"metadata\":{\"exit_code\":0}}"`, statusOK, 0},
		{`"{\"output\":\"patch rejected by user\",\"metadata\":{}}"`, statusOK, -1},
	} {
		_, status, exit := codexOutput([]byte(tc.output))
		got := -1
		if exit != nil {
			got = *exit
		}
		if status != tc.status || got != tc.exit {
			t.Errorf("%s: %s %d, want %s %d", tc.output, status, got, tc.status, tc.exit)
		}
	}
}

func TestCodexLocalShellWithoutCommand(t *testing.T) {
	t.Parallel()
	_, calls := cxParse(t, cxMeta+"\n"+`{"timestamp":"2026-10-08T07:00:07.000Z","type":"response_item","payload":{"type":"local_shell_call","call_id":"s1","status":"completed","action":{"type":"exec","command":5}}}`+"\n")
	if c := calls["s1"]; c == nil || c.hasCommand {
		t.Fatalf("%+v", c)
	}
}

func TestCodexSessionInBothDirectories(t *testing.T) {
	t.Parallel()
	w := newCodexWorld(t, defaultCwd)
	archived := w.paths[cxArchived]
	live := filepath.Join(w.codex, "sessions", "2026", "10", "07", filepath.Base(archived))
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(w.content(cxArchived)), 0o644); err != nil {
		t.Fatal(err)
	}
	l := w.list()
	n := 0
	for _, s := range l.Sessions {
		if s.SessionID == cxArchived {
			n++
			if s.Path != live {
				t.Fatalf("%+v", s)
			}
		}
	}
	if n != 1 || len(l.Stores[0].Problems) != 1 || !strings.HasPrefix(l.Stores[0].Problems[0], archived+": session "+cxArchived+" is also in "+live) {
		t.Fatalf("%d %+v", n, l.Stores)
	}
}
