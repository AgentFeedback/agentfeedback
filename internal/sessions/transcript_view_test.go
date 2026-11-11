package sessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
)

// fakeSecret is a GitHub token in the form scrub recognises.
const fakeSecret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

// ccLines is a Claude Code session of the given lines, each a JSON object
// that gets the session's cwd, a timestamp and, unless it has one, a uuid.
func ccLines(t *testing.T, cwd string, lines ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for i, l := range lines {
		l["cwd"] = cwd
		l["timestamp"] = fmt.Sprintf("2026-10-08T07:%02d:%02d.000Z", i/60, i%60)
		if _, ok := l["uuid"]; !ok {
			l["uuid"] = fmt.Sprintf("x-%d", i)
		}
		j, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}

	return b.String()
}

func ccPrompt(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
}

func ccCall(uuid, id, tool string, input any) map[string]any {
	return map[string]any{"type": "assistant", "uuid": uuid, "message": map[string]any{"model": "claude-test-1", "role": "assistant",
		"content": []any{map[string]any{"type": "tool_use", "id": id, "name": tool, "input": input}}}}
}

func ccResult(id, content string, isError bool) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isError}}}}
}

func ccInstructions(files ...map[string]any) map[string]any {
	return map[string]any{"type": "attachment", "attachment": map[string]any{"type": "instructions", "files": files}}
}

func (w *world) transcript(req TranscriptRequest) TranscriptOutput {
	w.t.Helper()
	out, err := Transcript(w.ctx, w.env(), req)
	if err != nil {
		w.t.Fatal(err)
	}

	return out
}

func (w *world) transcriptOne(sid string) SessionTranscript {
	w.t.Helper()
	out := w.transcript(TranscriptRequest{Refs: []string{Ref(HarnessClaudeCode, sid)}})
	if len(out.Sessions) != 1 {
		w.t.Fatalf("transcript of %s: %+v", sid, out)
	}

	return out.Sessions[0]
}

// toolEntries are the transcript's tool_call entries by span.
func toolEntries(st SessionTranscript) map[string]TranscriptEntry {
	out := map[string]TranscriptEntry{}
	for _, e := range st.Entries {
		if e.Type == EntryToolCall {
			out[e.Span] = e
		}
	}

	return out
}

// TestTranscriptRefusals: a session is refused as digest refuses it, with
// the same state and message, and a file digest does not open is not
// opened.
func TestTranscriptRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		state  string
		opened bool
		setup  func(t *testing.T, w *world) []string
	}{
		{"disabled", StateDisabled, false, func(t *testing.T, w *world) []string {
			w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
			w.pol.Disabled = true
			return []string{"claude-code:s1"}
		}},
		{"denied", StateDenied, false, func(t *testing.T, w *world) []string {
			w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
			w.pol.DenyPaths = []string{defaultCwd}
			return []string{"claude-code:s1"}
		}},
		{"denied after the read", StateDenied, true, func(t *testing.T, w *world) []string {
			w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd)+strings.ReplaceAll(newPrompt, defaultCwd, "/secret-af/x"))
			w.pol.DenyPaths = []string{"/secret-af"}
			return []string{"claude-code:s1"}
		}},
		{"unknown-project", StateUnknownProject, true, func(t *testing.T, w *world) []string {
			w.put(defaultCwd, "s1", fixture(t, "nocwd.jsonl", "s1", ""))
			return []string{"claude-code:s1"}
		}},
		{"unsupported-format", StateUnsupportedFormat, true, func(t *testing.T, w *world) []string {
			w.put(defaultCwd, "s1", fixture(t, "unsupported.jsonl", "s1", defaultCwd))
			return []string{"claude-code:s1"}
		}},
		{"absent", StateAbsent, false, func(t *testing.T, w *world) []string {
			return []string{"claude-code:nope"}
		}},
		{"unreadable", StateUnreadable, true, func(t *testing.T, w *world) []string {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("mode 000 does not stop root or windows")
			}
			p := w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			return []string{"claude-code:s1"}
		}},
		{"store unreadable", StateUnreadable, false, func(t *testing.T, w *world) []string {
			projects := filepath.Join(w.cfg, "projects")
			if err := os.RemoveAll(projects); err != nil {
				t.Fatal(err)
			}
			// A file where the projects directory should be.
			if err := os.WriteFile(projects, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{"claude-code:s1"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			refs := tt.setup(t, w)
			got := w.transcript(TranscriptRequest{Refs: refs})
			transcriptOpens := w.opens
			w.opens = map[string]int{}
			want := w.digest(DigestRequest{Refs: refs})
			if len(got.Sessions) != 0 || len(got.Errors) != 1 || !reflect.DeepEqual(got.Errors, want.Errors) {
				t.Fatalf("transcript errors %+v, digest errors %+v", got.Errors, want.Errors)
			}
			if got.Errors[0].State != tt.state {
				t.Fatalf("state %q, want %q", got.Errors[0].State, tt.state)
			}
			if !reflect.DeepEqual(transcriptOpens, w.opens) || (len(transcriptOpens) > 0) != tt.opened {
				t.Fatalf("opens: transcript %v, digest %v", transcriptOpens, w.opens)
			}
		})
	}

	t.Run("unknown-project allowed", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.put(defaultCwd, "s1", fixture(t, "nocwd.jsonl", "s1", ""))
		out := w.transcript(TranscriptRequest{Refs: []string{"claude-code:s1"}, AllowUnknownProject: true})
		if len(out.Sessions) != 1 || len(out.Errors) != 0 || out.Sessions[0].Project != "" {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("bad ref", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		if _, err := Transcript(w.ctx, w.env(), TranscriptRequest{Refs: []string{"amp:x"}}); err == nil {
			t.Fatal("a ref without a reader was accepted")
		}
		if _, err := Transcript(w.ctx, w.env(), TranscriptRequest{}); err == nil {
			t.Fatal("no ref was accepted")
		}
	})
}

// TestTranscriptScrubs: a secret in a prompt, a command, a JSON argument,
// a result, an edit payload and an instruction is redacted, and so is one
// that straddles a cap.
func TestTranscriptScrubs(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	filler := strings.Repeat("a", TranscriptMessageBytes-10)
	w.put(defaultCwd, "s1", ccLines(t, defaultCwd,
		ccInstructions(map[string]any{"path": "/home/u/CLAUDE.md", "type": "User", "content": "token " + fakeSecret}),
		ccPrompt("use "+fakeSecret),
		ccPrompt(filler+" "+fakeSecret),
		ccCall("a-1", "t1", "Bash", map[string]any{"command": "git push https://" + fakeSecret + "@example.test/x"}),
		ccResult("t1", "remote: "+fakeSecret, true),
		ccCall("a-2", "t2", "WebFetch", map[string]any{"url": "https://example.test", "headers": map[string]any{"token": fakeSecret}}),
		ccResult("t2", "ok", false),
		ccCall("a-3", "t3", "Edit", map[string]any{"file_path": "/p/x.go", "old_string": "a", "new_string": "key := \"" + fakeSecret + "\""}),
		ccResult("t3", "ok", false),
	))
	st := w.transcriptOne("s1")
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ghp_") {
		t.Fatalf("a secret or part of one is shown: %s", b)
	}
	if n := strings.Count(string(b), "[REDACTED"); n < 6 {
		t.Fatalf("%d redactions in %s", n, b)
	}
	if st.RulesSource != RulesSessionLog || len(st.Instructions) != 1 || !strings.Contains(st.Instructions[0].Text, "[REDACTED:github_token]") {
		t.Fatalf("%+v", st.Instructions)
	}
	// The straddling prompt: scrubbed whole, then cut inside the marker.
	for _, e := range st.Entries {
		if e.Type == EntryPrompt && strings.HasPrefix(e.Text, "aaa") {
			if len(e.Text) != TranscriptMessageBytes || !slices.Equal(e.Truncated, []string{"text"}) {
				t.Fatalf("%d bytes, truncated %v", len(e.Text), e.Truncated)
			}
		}
	}
}

// TestTranscriptLeavesState: a transcript between digest and mark, and
// after the mark, changes no row: the mark records the digest's watermark.
func TestTranscriptLeavesState(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
	row := func() store.SessionSeen {
		var r store.SessionSeen
		if err := w.db.Read(w.ctx, func(q store.Querier) (err error) {
			r, _, err = store.GetSession(w.ctx, q, HarnessClaudeCode, "s1")
			return err
		}); err != nil {
			t.Fatal(err)
		}

		return r
	}
	w.digestOne("s1")
	digested := row()
	w.transcriptOne("s1")
	if got := row(); !reflect.DeepEqual(got, digested) {
		t.Fatalf("transcript changed the row:\n%+v\n%+v", got, digested)
	}
	w.mark("s1")
	marked := row()
	if marked.ProcessedSize == nil || *marked.ProcessedSize != digested.Size {
		t.Fatalf("mark recorded %+v, want the digest's size %d", marked, digested.Size)
	}
	w.transcriptOne("s1")
	if got := row(); !reflect.DeepEqual(got, marked) {
		t.Fatalf("transcript changed the marked row:\n%+v\n%+v", got, marked)
	}
	if s := w.session("s1"); s.State != StateProcessed {
		t.Fatalf("%+v", s)
	}
}

// TestTranscriptCaps: the caps on messages, arguments, results, edit
// payloads, instructions, a session and a run.
func TestTranscriptCaps(t *testing.T) {
	t.Parallel()

	t.Run("entries", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		mid := strings.Repeat("m", 4<<10)
		big := strings.Repeat("b", TranscriptEditBytes+100)
		w.put(defaultCwd, "s1", ccLines(t, defaultCwd,
			ccPrompt(strings.Repeat("p", TranscriptMessageBytes+1)),
			ccCall("a-1", "t1", "Edit", map[string]any{"file_path": "/p/x", "old_string": "a", "new_string": mid}),
			ccResult("t1", strings.Repeat("r", TranscriptResultBytes+1), false),
			ccCall("a-2", "t2", "Write", map[string]any{"file_path": "/p/y", "content": big}),
			ccResult("t2", "ok", false),
			ccCall("a-3", "t3", "Bash", map[string]any{"command": strings.Repeat("c", TranscriptArgsBytes+1)}),
			ccCall("a-4", "t4", "Grep", map[string]any{"pattern": mid}),
		))
		st := w.transcriptOne("s1")
		if p := st.Entries[0]; p.Type != EntryPrompt || len(p.Text) != TranscriptMessageBytes || !slices.Equal(p.Truncated, []string{"text"}) {
			t.Fatalf("prompt: %d %v", len(p.Text), p.Truncated)
		}
		calls := toolEntries(st)
		if e := calls["a-1"]; !e.Edit || !strings.Contains(e.Args, mid) || len(e.Result) != TranscriptResultBytes || !slices.Equal(e.Truncated, []string{"result"}) {
			t.Fatalf("edit between the caps: edit=%v args=%d result=%d %v", e.Edit, len(e.Args), len(e.Result), e.Truncated)
		}
		if e := calls["a-2"]; !e.Edit || len(e.Args) != TranscriptEditBytes || !slices.Equal(e.Truncated, []string{"args"}) {
			t.Fatalf("edit over the cap: edit=%v args=%d %v", e.Edit, len(e.Args), e.Truncated)
		}
		if e := calls["a-3"]; e.Edit || len(e.Args) != TranscriptArgsBytes || e.Status != "" || !slices.Equal(e.Truncated, []string{"args"}) {
			t.Fatalf("command: %+v", e.Truncated)
		}
		if e := calls["a-4"]; e.Edit || len(e.Args) != TranscriptArgsBytes {
			t.Fatalf("a non-edit tool keeps the small cap: %d", len(e.Args))
		}
	})

	t.Run("instructions", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		var files []map[string]any
		for i := range 5 {
			files = append(files, map[string]any{"path": fmt.Sprintf("/p/%d.md", i), "content": strings.Repeat("i", TranscriptInstructionBytes+1)})
		}
		w.put(defaultCwd, "s1", ccLines(t, defaultCwd, ccInstructions(files...), ccPrompt("hi")))
		st := w.transcriptOne("s1")
		total := 0
		for i, in := range st.Instructions {
			total += len(in.Text)
			if !in.Truncated || in.Path != fmt.Sprintf("/p/%d.md", i) {
				t.Fatalf("%d: %+v", i, in.Truncated)
			}
		}
		if len(st.Instructions) != 5 || len(st.Instructions[0].Text) != TranscriptInstructionBytes || total != TranscriptInstructionsBytes || st.Instructions[4].Text != "" {
			t.Fatalf("%d instructions, %d bytes", len(st.Instructions), total)
		}
	})

	t.Run("session", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		var lines []map[string]any
		for i := range 200 {
			lines = append(lines, ccPrompt(fmt.Sprintf("prompt %03d %s", i, strings.Repeat("x", 1500))))
		}
		w.put(defaultCwd, "s1", ccLines(t, defaultCwd, lines...))
		st := w.transcriptOne("s1")
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		cut := slices.IndexFunc(st.Entries, func(e TranscriptEntry) bool { return e.Type == EntryCut })
		if !st.Truncated || len(b) > MaxTranscriptSessionBytes || cut < 0 || st.Entries[cut].Omitted != st.Omitted || st.Omitted == 0 {
			t.Fatalf("truncated=%v bytes=%d cut=%d omitted=%d", st.Truncated, len(b), cut, st.Omitted)
		}
		head, tail := cut, len(st.Entries)-cut-1
		if !strings.HasPrefix(st.Entries[0].Text, "prompt 000") || !strings.HasPrefix(st.Entries[len(st.Entries)-1].Text, "prompt 199") ||
			head+tail+st.Omitted != 200 || head < 2*tail-2 || head > 2*tail+2 {
			t.Fatalf("head %d tail %d omitted %d", head, tail, st.Omitted)
		}
		if st.Counts.Prompts != 200 {
			t.Fatalf("counts cover the whole session: %+v", st.Counts)
		}
	})

	t.Run("run", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		var lines []map[string]any
		for i := range 120 {
			lines = append(lines, ccPrompt(fmt.Sprintf("prompt %03d %s", i, strings.Repeat("x", 1500))))
		}
		var refs []string
		for i := range 4 {
			sid := fmt.Sprintf("s%d", i)
			w.put(defaultCwd, sid, ccLines(t, defaultCwd, lines...))
			refs = append(refs, Ref(HarnessClaudeCode, sid))
		}
		out := w.transcript(TranscriptRequest{Refs: refs})
		total := 0
		for _, s := range out.Sessions {
			b, _ := json.Marshal(s)
			total += len(b)
		}
		if len(out.Deferred) == 0 || total > MaxTranscriptRunBytes || len(out.Sessions)+len(out.Deferred) != 4 || out.Deferred[0] != refs[len(out.Sessions)] {
			t.Fatalf("sessions=%d deferred=%v total=%d", len(out.Sessions), out.Deferred, total)
		}
	})
}

// TestTranscriptEntries: entries in log order, assistant text before the
// calls of its entry, refs Locate finds, rules not recorded without them.
func TestTranscriptEntries(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
	st := w.transcriptOne("s1")
	if st.RulesSource != RulesNotRecorded || len(st.Instructions) != 0 || len(st.SkillsAvailable) != 0 || st.Truncated {
		t.Fatalf("%+v", st)
	}
	if e := st.Entries[1]; e.Type != EntryAssistant || e.Span != "a-0002" || st.Entries[2].Type != EntryToolCall || st.Entries[2].Span != "a-0002" {
		t.Fatalf("%+v %+v", e, st.Entries[2])
	}
	for _, e := range st.Entries {
		_, span, _ := strings.Cut(e.Ref, "#")
		if _, err := Locate(w.ctx, w.env(), st.Ref, span); err != nil {
			t.Fatalf("%s: %v", e.Ref, err)
		}
	}
}

// ccSkillListing is a skill_listing attachment of names.
func ccSkillListing(names []string) map[string]any {
	return map[string]any{"type": "attachment", "attachment": map[string]any{"type": "skill_listing", "names": names}}
}

// TestTranscriptHeaderCaps: the skill listing and the instructions are
// bounded, counted as JSON, and a header that still does not fit is an
// error of its session only.
func TestTranscriptHeaderCaps(t *testing.T) {
	t.Parallel()
	names := func(n, size int, fill string) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("%04d", i)+strings.Repeat(fill, size-4))
		}
		return out
	}

	t.Run("skills", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.put(defaultCwd, "s1", ccLines(t, defaultCwd, ccSkillListing(names(1000, 200, "s")), ccPrompt("hi")))
		st := w.transcriptOne("s1")
		if !st.Truncated || len(st.SkillsAvailable) != TranscriptSkillNames || len(st.SkillsAvailable[0]) != TranscriptSkillNameBytes || !strings.HasPrefix(st.SkillsAvailable[255], "0255") {
			t.Fatalf("truncated=%v names=%d", st.Truncated, len(st.SkillsAvailable))
		}
		w.put(defaultCwd, "s2", ccLines(t, defaultCwd, ccSkillListing(names(3, 10, "s")), ccPrompt("hi")))
		if st := w.transcriptOne("s2"); st.Truncated || len(st.SkillsAvailable) != 3 {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("escaped instructions", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		var files []map[string]any
		for i := range 5 {
			files = append(files, map[string]any{"path": fmt.Sprintf("/p/%d.md", i), "content": strings.Repeat("<\"\x01é", TranscriptInstructionBytes)})
		}
		w.put(defaultCwd, "s1", ccLines(t, defaultCwd, ccInstructions(files...), ccPrompt("hi")))
		st := w.transcriptOne("s1")
		encoded := 0
		for _, in := range st.Instructions {
			b, err := json.Marshal(in.Text)
			if err != nil {
				t.Fatal(err)
			}
			if len(b)-2 > TranscriptInstructionBytes || !in.Truncated {
				t.Fatalf("%s: %d encoded bytes", in.Path, len(b)-2)
			}
			encoded += len(b) - 2
		}
		if encoded > TranscriptInstructionsBytes || encoded < TranscriptInstructionsBytes-16 {
			t.Fatalf("%d encoded bytes of instructions", encoded)
		}
	})

	t.Run("over the cap", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		// 256 names of 128 bytes that each encode to six times as many.
		w.put(defaultCwd, "big", ccLines(t, defaultCwd, ccSkillListing(names(256, 128, "<")), ccPrompt("hi")))
		w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
		out := w.transcript(TranscriptRequest{Refs: []string{"claude-code:big", "claude-code:s1"}})
		if len(out.Errors) != 1 || out.Errors[0].Ref != "claude-code:big" || out.Errors[0].State != StateNew ||
			!strings.Contains(out.Errors[0].Message, "header") || len(out.Sessions) != 1 || out.Sessions[0].Ref != "claude-code:s1" {
			t.Fatalf("errors %+v, %d sessions", out.Errors, len(out.Sessions))
		}
	})
}

// TestTranscriptArgsScrubbed: a secret in an object key and one written
// with a \u escape in a value are redacted.
func TestTranscriptArgsScrubbed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	escaped := `g` + fakeSecret[1:]
	input := json.RawMessage(`{"` + fakeSecret + `":1,"v":"` + escaped + `","n":[{"` + escaped + `":"x"}]}`)
	w.put(defaultCwd, "s1", ccLines(t, defaultCwd, ccCall("a-1", "t1", "WebFetch", input)))
	e := toolEntries(w.transcriptOne("s1"))["a-1"]
	if strings.Contains(e.Args, "hp_abc") || strings.Count(e.Args, "[REDACTED:github_token]") != 3 {
		t.Fatalf("args %s", e.Args)
	}
}

// TestTranscriptDuplicateRefs: a session named twice, also by an alias, is
// shown once.
func TestTranscriptDuplicateRefs(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", fixture(t, "basic.jsonl", "s1", defaultCwd))
	if out := w.transcript(TranscriptRequest{Refs: []string{"claude-code:s1", "claude-code:s1"}}); len(out.Sessions) != 1 || len(out.Errors) != 0 {
		t.Fatalf("%+v", out)
	}
	ow := newOCWorld(t)
	ow.create(defaultCwd)
	out, err := Transcript(ow.ctx, ow.env(), TranscriptRequest{Refs: []string{"opencode:" + ocSID, "opencode-v1:" + ocSID, "claude-code:nope", "claude-code:nope"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].Ref != "opencode-v1:"+ocSID || len(out.Errors) != 1 {
		t.Fatalf("%+v", out)
	}
}

// TestSummaryDropsRules: a read for a listing keeps no recorded rules.
func TestSummaryDropsRules(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "s1", ccLines(t, defaultCwd,
		ccInstructions(map[string]any{"path": "/p/CLAUDE.md", "content": "rule"}), ccSkillListing([]string{"a"}), ccPrompt("hi")))
	r := claudeReader{}
	cands, _, err := r.candidates(w.env())
	if err != nil || len(cands) != 1 {
		t.Fatalf("%v %+v", err, cands)
	}
	if ev := read(w.env(), r, cands[0], nil, false); ev.tr == nil || ev.tr.instructions != nil || ev.tr.skillsAvailable != nil {
		t.Fatalf("%+v", ev.tr)
	}
	if ev := read(w.env(), r, cands[0], nil, true); len(ev.tr.instructions) != 1 || len(ev.tr.skillsAvailable) != 1 {
		t.Fatalf("%+v", ev.tr)
	}
}

// TestTranscriptOpenCodeV2Rules: instruction tables that cannot be read
// leave the rules not recorded and the session readable.
func TestTranscriptOpenCodeV2Rules(t *testing.T) {
	t.Parallel()
	const tables = "CREATE TABLE `instruction_blob` (`hash` text PRIMARY KEY, `value` text);\n" +
		"CREATE TABLE `instruction_state` (`session_id` text PRIMARY KEY, `epoch_start` integer NOT NULL, `through_seq` integer NOT NULL, `initial_values` text NOT NULL, `current_values` text NOT NULL);\n"
	for _, tt := range []struct {
		name, stmts, want string
	}{
		{"valid", tables +
			"INSERT INTO `instruction_blob` VALUES ('h1', '[{\"path\":\"/p/AGENTS.md\",\"content\":\"rule\"}]');\n" +
			"INSERT INTO `instruction_state` VALUES ('{{SID}}', 0, 0, '{\"core/instructions\":\"h1\"}', '{\"core/instructions\":\"h1\"}');\n", RulesSessionLog},
		{"malformed blob", tables +
			"INSERT INTO `instruction_blob` VALUES ('h1', '[{\"path\":');\n" +
			"INSERT INTO `instruction_state` VALUES ('{{SID}}', 0, 0, '{\"core/instructions\":\"h1\"}', '{\"core/instructions\":\"h1\"}');\n", RulesNotRecorded},
		{"malformed values", tables +
			"INSERT INTO `instruction_state` VALUES ('{{SID}}', 0, 0, 'nope', '{\"core/instructions\":7}');\n", RulesNotRecorded},
		{"missing column", "CREATE TABLE `instruction_blob` (`hash` text PRIMARY KEY);\n" +
			"CREATE TABLE `instruction_state` (`session_id` text PRIMARY KEY, `initial_values` text NOT NULL, `current_values` text NOT NULL);\n", RulesNotRecorded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newOCWorld(t)
			w.createV2(defaultCwd)
			w.exec(strings.ReplaceAll(tt.stmts, "{{SID}}", ocSID))
			out, err := Transcript(w.ctx, w.env(), TranscriptRequest{Refs: []string{Ref(HarnessOpenCodeV2, ocSID)}})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Sessions) != 1 || out.Sessions[0].RulesSource != tt.want {
				t.Fatalf("%+v", out)
			}
		})
	}
}
