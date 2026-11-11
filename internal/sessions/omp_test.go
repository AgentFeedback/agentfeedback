package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// These are synthetic public-schema fixtures, not copied private transcripts.
func ompJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func ompHead(t *testing.T, id, cwd string, dirs ...string) string {
	return ompJSON(t, map[string]any{"type": "session", "version": 3, "id": id, "cwd": cwd, "additionalDirectories": dirs, "timestamp": "2026-10-08T09:00:00Z", "parentSession": "/must/not/be/opened.jsonl"})
}

func ompMsg(t *testing.T, id string, m map[string]any) string {
	return ompJSON(t, map[string]any{"type": "message", "id": id, "parentId": nil, "timestamp": "2026-10-08T09:00:01Z", "message": m})
}

func ompHuman(t *testing.T, id, text string) string {
	return ompMsg(t, id, map[string]any{"role": "user", "attribution": "user", "content": text, "timestamp": 1791450001000})
}

func ompPut(t *testing.T, root, rel, text string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func ompParse(t *testing.T, text string) *transcript {
	t.Helper()
	_, m, err := ompHeader(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := (ompReader{}).parse(strings.NewReader(text), m)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func ompCandidates(t *testing.T, env Env) []candidate {
	t.Helper()
	cs, ps, err := (ompReader{}).candidates(env)
	if err != nil || len(ps) != 0 {
		t.Fatalf("candidates=%+v problems=%+v error=%v", cs, ps, err)
	}
	return cs
}

func TestOMPDiscovery(t *testing.T) {
	w := newWorld(t)
	env := w.env()
	root := (ompReader{}).location(env)
	ompPut(t, root, "project/not-the-id.jsonl", ompHead(t, "header-id", defaultCwd)+ompHuman(t, "p", "Please inspect"))
	ompPut(t, root, "custom.jsonl", ompHead(t, "custom-id", defaultCwd))
	ompPut(t, root, "project/not-the-id/child.jsonl", ompHead(t, "child", defaultCwd))
	ompPut(t, root, "custom/child.jsonl", ompHead(t, "root-child", defaultCwd))
	ompPut(t, root, "project/subdir/deeper.jsonl", ompHead(t, "deeper", defaultCwd))
	cs := ompCandidates(t, env)
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.sessionID)
	}
	slices.Sort(ids)
	if strings.Join(ids, ",") != "custom-id,header-id" {
		t.Fatalf("ids=%v", ids)
	}
	if w.opens[filepath.Join(root, "project", "not-the-id", "child.jsonl")] != 0 || w.opens["/must/not/be/opened.jsonl"] != 0 {
		t.Fatal("followed child or parent")
	}
	if (ompReader{}).location(Env{Home: w.home, OMPSessionsDir: "relative"}) != root {
		t.Fatal("relative override accepted")
	}
	override := t.TempDir()
	if (ompReader{}).location(Env{Home: w.home, OMPSessionsDir: override}) != override {
		t.Fatal("absolute override ignored")
	}
}

type ompTrackedFile struct {
	*os.File
	read *int
	max  int
}

func (f *ompTrackedFile) Read(b []byte) (int, error) {
	if f.max >= 0 && *f.read+len(b) > f.max {
		return 0, errors.New("read beyond metadata gate")
	}
	n, err := f.File.Read(b)
	*f.read += n
	return n, err
}

func TestOMPHeaderPolicyBeforeBody(t *testing.T) {
	for _, name := range []string{"deny-cwd", "deny-additional", "opt-in", "repo-veto", "disabled"} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			env := w.env()
			cwd, extra := filepath.Join(w.home, "allowed"), filepath.Join(w.home, "denied")
			want := StateDenied
			switch name {
			case "deny-cwd":
				env.Policy.DenyPaths = []string{cwd}
			case "deny-additional":
				env.Policy.DenyPaths = []string{extra}
			case "opt-in":
				env.Policy.OptInOnly = true
			case "disabled":
				env.Policy.Disabled = true
				want = StateDisabled
			case "repo-veto":
				if err := os.MkdirAll(filepath.Join(extra, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(extra, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(extra, ".agentfeedback.toml"), []byte("[collect]\ndisabled = true\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				want = StateDisabled
			}
			header := ompHead(t, "policy", cwd, extra)
			ompPut(t, (ompReader{}).location(env), "project/log.jsonl", header+ompHuman(t, "secret", "Never read this body"))
			reads, opens := 0, 0
			env.Open = func(path string) (io.ReadCloser, error) {
				opens++
				f, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				return &ompTrackedFile{File: f, read: &reads, max: len(header)}, nil
			}
			cs := ompCandidates(t, env)
			if name == "disabled" {
				if opens != 0 || reads != 0 || len(cs) != 0 {
					t.Fatalf("disabled opened=%d read=%d candidates=%v", opens, reads, cs)
				}
				return
			}
			if len(cs) != 1 {
				t.Fatalf("candidates=%v", cs)
			}
			ev := read(env, ompReader{}, cs[0], nil, true)
			if ev.state != want || ev.tr != nil || opens != 1 || reads != len(header) {
				t.Fatalf("state=%s reason=%s opens=%d reads=%d", ev.state, ev.reason, opens, reads)
			}
		})
	}
}

func TestOMPOptInAllowsBody(t *testing.T) {
	w := newWorld(t)
	env := w.env()
	cwd := filepath.Join(w.home, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	env.Policy = collect.Policy{OptInOnly: true, OptInPaths: []string{cwd}}
	ompPut(t, (ompReader{}).location(env), "project/log.jsonl", ompHead(t, "opted-in", cwd)+ompHuman(t, "prompt", "Hello"))
	cs := ompCandidates(t, env)
	if len(cs) != 1 {
		t.Fatal(cs)
	}
	ev := read(env, ompReader{}, cs[0], nil, true)
	if ev.state != StateNew || ev.tr == nil || ev.tr.total.Prompts != 1 {
		t.Fatalf("%+v", ev)
	}
}

func TestOMPHeaders(t *testing.T) {
	header := ompHead(t, "identity", defaultCwd)
	title := ompJSON(t, map[string]any{"type": "title", "v": 1, "title": "mutable title", "updatedAt": "2026-10-08T09:00:00Z", "pad": ""})
	fixedTitle := strings.Replace(title, `"pad":""`, `"pad":"`+strings.Repeat(" ", 256-len(title))+`"`, 1)
	for _, prefix := range []string{header, title + header, fixedTitle + header, "\n" + header, strings.ReplaceAll(header, ",", ",\n")} {
		r := strings.NewReader(prefix + "BODY MUST REMAIN")
		got, m, err := ompHeader(r)
		if err != nil || m.ID != "identity" || string(got) != prefix || r.Len() != len("BODY MUST REMAIN") {
			t.Fatalf("prefix=%q meta=%+v err=%v remaining=%d", got, m, err, r.Len())
		}
	}
	for _, text := range []string{"not-json\n", `{"type":"message","id":"x"}` + "\n", `{"type":"session","id":"../escape"}` + "\n", strings.TrimSuffix(header, "\n"), "{\"type\":\"session\",\"id\":\"x\",\"cwd\":1}\n", strings.Repeat(" ", ompHeaderCap) + header} {
		if _, _, err := ompHeader(strings.NewReader(text)); !errors.Is(err, errUnsupported) {
			t.Fatalf("invalid header accepted: len=%d err=%v", len(text), err)
		}
	}
	for _, version := range []string{"-1", "0", "4", "999"} {
		text := strings.Replace(header, `"version":3`, `"version":`+version, 1)
		_, m, err := ompHeader(strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		if state, _ := (ompReader{}).gate(Env{}, candidate{omp: m}); state != StateUnsupportedFormat {
			t.Fatalf("version %s allowed", version)
		}
	}
	for _, text := range []string{strings.Replace(header, `,"version":3`, "", 1), strings.Replace(header, `"version":3`, `"version":1`, 1), strings.Replace(header, `"version":3`, `"version":2`, 1)} {
		_, m, err := ompHeader(strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		if state, _ := (ompReader{}).gate(Env{}, candidate{omp: m}); state != "" {
			t.Fatalf("legacy header refused: %s", state)
		}
	}
}

func TestOMPHeaderRace(t *testing.T) {
	for _, change := range []string{"id", "cwd", "additionalDirectories", "version"} {
		t.Run(change, func(t *testing.T) {
			w := newWorld(t)
			env := w.env()
			header := ompHead(t, "race", defaultCwd, "/allowed")
			path := ompPut(t, (ompReader{}).location(env), "project/log.jsonl", header+ompHuman(t, "p", "private"))
			cs := ompCandidates(t, env)
			if len(cs) != 1 {
				t.Fatal(cs)
			}
			var h map[string]any
			if err := json.Unmarshal([]byte(header), &h); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "id":
				h[change] = "changed"
			case "cwd":
				h[change] = "/denied"
			case "additionalDirectories":
				h[change] = []string{"/denied"}
			case "version":
				h[change] = 2
			}
			changed := ompJSON(t, h)
			if err := os.WriteFile(path, []byte(changed+ompHuman(t, "p", "private")), 0o600); err != nil {
				t.Fatal(err)
			}
			n := 0
			env.Open = func(path string) (io.ReadCloser, error) {
				f, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				return &ompTrackedFile{File: f, read: &n, max: len(changed)}, nil
			}
			if _, err := (ompReader{}).load(env, cs[0]); !errors.Is(err, errOMPChanged) {
				t.Fatalf("race accepted: %v", err)
			}
			if n != len(changed) {
				t.Fatalf("read %d bytes, header %d", n, len(changed))
			}
		})
	}
}

func TestOMPDiscoveryRefusals(t *testing.T) {
	w := newWorld(t)
	env := w.env()
	root := (ompReader{}).location(env)
	ompPut(t, root, "a/one.jsonl", ompHead(t, "duplicate", defaultCwd))
	ompPut(t, root, "b/two.jsonl", ompHead(t, "duplicate", defaultCwd))
	ompPut(t, root, "bad.jsonl", "malformed\n")
	ompPut(t, root, "future.jsonl", strings.Replace(ompHead(t, "future", defaultCwd), `"version":3`, `"version":4`, 1)+"DO NOT READ\n")
	cs, ps, err := (ompReader{}).candidates(env)
	if err != nil || len(cs) != 1 || cs[0].sessionID != "future" || len(ps) != 3 {
		t.Fatalf("candidates=%+v problems=%+v err=%v", cs, ps, err)
	}
	if ev := read(env, ompReader{}, cs[0], nil, true); ev.state != StateUnsupportedFormat {
		t.Fatal(ev.state)
	}
}

func TestOMPNonregularPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink/FIFO fixtures")
	}
	w := newWorld(t)
	env := w.env()
	root := (ompReader{}).location(env)
	real := ompPut(t, root, "project/real.jsonl", ompHead(t, "real", defaultCwd))
	if err := os.Symlink(real, filepath.Join(root, "project", "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "project"), filepath.Join(root, "linked-project")); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("mkfifo", filepath.Join(root, "project", "pipe.jsonl")).Run(); err != nil {
		t.Skip("mkfifo unavailable:", err)
	}
	cs := ompCandidates(t, env)
	if len(cs) != 1 || cs[0].path != real {
		t.Fatal(cs)
	}
	for _, name := range []string{"link.jsonl", "pipe.jsonl"} {
		if w.opens[filepath.Join(root, "project", name)] != 0 {
			t.Fatalf("opened %s", name)
		}
	}
	parent := t.TempDir()
	if err := os.Symlink(root, filepath.Join(parent, "alias")); err != nil {
		t.Fatal(err)
	}
	env.OMPSessionsDir = filepath.Join(parent, "alias")
	if _, _, err := (ompReader{}).candidates(env); err == nil {
		t.Fatal("symlinked root accepted")
	}
	// The configured root itself can be regular, but no ancestor may be a symlink.
	env.OMPSessionsDir = filepath.Join(parent, "alias", "project")
	if _, _, err := (ompReader{}).candidates(env); err == nil {
		t.Fatal("symlinked ancestor accepted")
	}
}

func TestOMPAuthoredJournal(t *testing.T) {
	text := ompHead(t, "journal", defaultCwd)
	text += ompHuman(t, "first", "first human")
	for i, m := range []map[string]any{
		{"role": "user", "content": "ambiguous legacy"},
		{"role": "user", "attribution": "agent", "content": "agent steer"},
		{"role": "user", "attribution": "user", "synthetic": true, "content": "synthetic"},
		{"role": "developer", "attribution": "user", "userInitiated": true, "content": "operator continuation"},
		{"role": "custom", "attribution": "user", "content": "injected custom role"},
		{"role": "user", "attribution": "user", "content": []any{map[string]any{"type": "image", "data": "DO NOT READ IMAGE"}}},
	} {
		text += ompMsg(t, "excluded-"+string(rune('a'+i)), m)
	}
	text += ompJSON(t, map[string]any{"type": "custom_message", "id": "skill", "customType": "skill-prompt", "content": "GENERATED SKILL INSTRUCTIONS", "details": map[string]any{"name": "review", "prompt": "/skill:review do review"}, "display": false, "attribution": "user"})
	text += ompJSON(t, map[string]any{"type": "custom_message", "id": "injected", "customType": "system", "content": "injected extension", "attribution": "agent"})
	for _, customType := range []string{"ultrathink-notice", "attachment-source", "image-description", "unknown-extension"} {
		text += ompJSON(t, map[string]any{"type": "custom_message", "id": customType, "customType": customType, "content": "GENERATED NOTICE", "display": false, "attribution": "user"})
	}
	text += ompJSON(t, map[string]any{"type": "compaction", "id": "compact", "summary": "a synthetic summary", "firstKeptEntryId": "first"})
	text += ompJSON(t, map[string]any{"type": "branch_summary", "id": "branch-summary", "summary": "branch context"})
	text += ompJSON(t, map[string]any{"type": "message", "id": "sibling", "parentId": "first", "activeLeafId": "first", "message": map[string]any{"role": "user", "attribution": "user", "content": "retained sibling"}})
	text += ompJSON(t, map[string]any{"type": "reset_boundary", "id": "clear"})
	text += ompHuman(t, "last", "last human")
	text += "malformed line\n"
	text += `{"type":"message","id":"unfinished"`
	tr := ompParse(t, text)
	var prompts []string
	for _, e := range tr.entries {
		if e.kind == kindPrompt {
			prompts = append(prompts, e.span+":"+e.text)
		}
	}
	if strings.Join(prompts, ",") != "first:first human,skill:/skill:review do review,sibling:retained sibling,last:last human" {
		t.Fatalf("prompts=%v", prompts)
	}
	if tr.total.Prompts != 4 || tr.total.Unparsed != 1 {
		t.Fatalf("counts=%+v", tr.total)
	}
	if tr.size != int64(strings.LastIndex(text, "\n")+1) {
		t.Fatalf("partial tail counted: %d", tr.size)
	}
}

func TestOMPToolPairing(t *testing.T) {
	text := ompHead(t, "tools", defaultCwd)
	text += ompJSON(t, map[string]any{"type": "model_change", "id": "model", "model": "provider/fallback"})
	text += ompHuman(t, "prompt", "run tools")
	var blocks []any
	for _, id := range []string{"fail", "ok", "pending", "aborted", "marker", "synthetic", "interrupted"} {
		blocks = append(blocks, map[string]any{"type": "toolCall", "id": id, "name": "bash", "arguments": map[string]any{"command": "false"}})
	}
	blocks = append(blocks, map[string]any{"type": "thinking", "thinking": "DO NOT READ REASONING"})
	text += ompMsg(t, "assistant", map[string]any{"role": "assistant", "model": "actual-model", "content": blocks, "timestamp": 1791450002000, "completedAt": 1791450003000, "stopReason": "toolUse"})
	for _, id := range []string{"ok", "fail", "synthetic", "interrupted"} {
		m := map[string]any{"role": "toolResult", "toolCallId": id, "toolName": "bash", "content": []any{map[string]any{"type": "text", "text": "output"}}, "isError": false}
		switch id {
		case "fail":
			m["details"] = map[string]any{"exitCode": 7}
		case "synthetic":
			m["details"] = map[string]any{"__synthetic": true, "source": "assistant_stop_aborted", "executed": false}
		case "interrupted":
			m["details"] = map[string]any{"__interrupted": true, "source": "interrupt_skipped", "execution": "started"}
		}
		text += ompMsg(t, "result-"+id, m)
	}
	text += ompMsg(t, "abort", map[string]any{"role": "assistant", "content": []any{}, "stopReason": "aborted", "toolCallAbortMessages": map[string]string{"aborted": "cancelled"}})
	text += ompJSON(t, map[string]any{"type": "custom", "id": "exit", "customType": "session_exit", "data": map[string]any{"kind": "signal", "pendingToolCalls": []any{map[string]any{"toolCallId": "marker", "toolName": "bash"}}}})
	tr := ompParse(t, text)
	calls := map[string]*call{}
	for _, e := range tr.entries {
		for _, c := range e.calls {
			calls[c.id] = c
		}
		if e.span == "assistant" && (e.model != "actual-model" || e.at.UnixMilli() != 1791450002000 || e.text != "") {
			t.Fatalf("assistant=%+v", e)
		}
	}
	for id, want := range map[string]string{"fail": statusError, "ok": statusOK, "pending": "", "aborted": statusInterrupted, "marker": statusInterrupted, "synthetic": statusInterrupted, "interrupted": statusInterrupted} {
		c := calls[id]
		if c == nil || c.status != want || c.result != (id != "pending") || !c.hasCommand || c.command != "false" {
			t.Fatalf("call %s = %+v want %s", id, c, want)
		}
	}
	if calls["fail"].exitCode == nil || *calls["fail"].exitCode != 7 {
		t.Fatal("lost exit code")
	}
	if tr.lastModel != "actual-model" || tr.total.ToolCalls != 7 || tr.total.ToolErrors != 1 {
		t.Fatalf("model=%s counts=%+v", tr.lastModel, tr.total)
	}
}

func TestOMPDuplicateEntryAndCallIDs(t *testing.T) {
	header := ompHead(t, "dups", defaultCwd)
	for _, body := range []string{
		ompHuman(t, "p", "one") + ompHuman(t, "p", "two"),
		ompMsg(t, "a", map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "id": "same", "name": "bash"}, map[string]any{"type": "toolCall", "id": "same", "name": "bash"}}}),
		header,
	} {
		_, m, err := ompHeader(strings.NewReader(header))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (ompReader{}).parse(strings.NewReader(header+body), m); !errors.Is(err, errUnsupported) {
			t.Fatalf("duplicate accepted: %v", err)
		}
	}
}

func TestOMPRevisionWatermark(t *testing.T) {
	title := ompJSON(t, map[string]any{"type": "title", "v": 1, "title": "title A", "updatedAt": "2026-10-08T09:00:00Z", "pad": ""})
	base := title + ompHead(t, "watermark", defaultCwd) + ompHuman(t, "stable", "same prompt")
	tr := ompParse(t, base)
	sum := sha256.Sum256([]byte(base))
	if tr.head != hex.EncodeToString(sum[:]) || tr.size != int64(len(base)) {
		t.Fatalf("hash=%s size=%d", tr.head, tr.size)
	}
	now := time.Now().UnixMilli()
	size := tr.size
	row := &store.SessionSeen{ProcessedAt: &now, ProcessedSize: &size, ProcessedHead: tr.head}
	for _, tt := range []struct{ name, text, state string }{
		{"unchanged", base, StateProcessed},
		{"partial-tail", base + `{"type":"message"`, StateProcessed},
		{"same-size-rewrite", strings.Replace(base, "same prompt", "diff prompt", 1), StateChanged},
		{"title-rewrite", strings.Replace(base, "title A", "title B", 1), StateChanged},
		{"append", base + ompHuman(t, "next", "another prompt"), StateChanged},
		{"truncate", title + ompHead(t, "watermark", defaultCwd), StateChanged},
	} {
		t.Run(tt.name, func(t *testing.T) {
			next := ompParse(t, tt.text)
			state, _, from := watermark(next, row)
			if state != tt.state || from != 0 {
				t.Fatalf("state=%s from=%d", state, from)
			}
			if tt.name == "append" && next.entries[0].span != tr.entries[0].span {
				t.Fatal("stable span changed")
			}
		})
	}
}

func TestOMPEndToEndAndCaptureRefused(t *testing.T) {
	w := newWorld(t)
	env := w.env()
	path := ompPut(t, (ompReader{}).location(env), "project/file.jsonl", ompHead(t, "e2e", defaultCwd)+ompHuman(t, "stable-prompt", "please fix this"))
	ref := Ref(HarnessOMP, "e2e")
	capture, err := CaptureCandidates(w.ctx, w.db, env, []string{ref})
	if err != nil || len(capture) != 1 || capture[0].State != StateUnsupportedFormat || w.opens[path] != 0 {
		t.Fatalf("capture=%+v err=%v opens=%d", capture, err, w.opens[path])
	}
	out, err := Digest(w.ctx, w.db, env, DigestRequest{Refs: []string{ref}})
	if err != nil || len(out.Sessions) != 1 || len(out.Errors) != 0 {
		t.Fatalf("digest=%+v err=%v", out, err)
	}
	if len(out.Sessions[0].Events) != 1 || out.Sessions[0].Events[0].Span != "stable-prompt" {
		t.Fatalf("digest=%+v", out.Sessions[0])
	}
	key := Key(HarnessOMP, "e2e", "stable-prompt", 0, DetectorVersion)
	if err := Mark(w.ctx, w.db, env, []string{ref}, OutcomeNothing, nil); err != nil {
		t.Fatal(err)
	}
	list, err := List(w.ctx, w.db, env, ListOptions{Harness: HarnessOMP})
	if err != nil || len(list.Sessions) != 1 || list.Sessions[0].State != StateProcessed {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	w.appendTo(path, ompHuman(t, "new-prompt", "one more prompt"))
	out, err = Digest(w.ctx, w.db, env, DigestRequest{Refs: []string{ref}})
	if err != nil || len(out.Sessions) != 1 || out.Sessions[0].FromOffset != 0 || len(out.Sessions[0].Events) != 2 {
		t.Fatalf("append digest=%+v err=%v", out, err)
	}
	if key != Key(HarnessOMP, "e2e", out.Sessions[0].Events[0].Span, 0, DetectorVersion) {
		t.Fatal("rescan changed idempotency key")
	}
	// The full-revision rule is local to omp; generic append-only semantics remain.
	x := &transcript{head: "unchanged", size: 20}
	old := int64(10)
	now := time.Now().UnixMilli()
	if state, reason, from := watermark(x, &store.SessionSeen{ProcessedAt: &now, ProcessedSize: &old, ProcessedHead: "unchanged"}); state != StateChanged || reason != ChangeAppended || from != 10 {
		t.Fatal(state, reason, from)
	}
}

func TestOMPAssistantFailureAndPaths(t *testing.T) {
	text := ompHead(t, "failure", defaultCwd)
	text += ompJSON(t, map[string]any{"type": "model_change", "id": "default-model", "model": "provider/default"})
	text += ompJSON(t, map[string]any{"type": "model_change", "id": "small-model", "role": "smol", "model": "provider/smol"})
	text += ompMsg(t, "failed", map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "provider unavailable", "content": []any{}})
	text += ompMsg(t, "read", map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "toolCall", "id": "read-call", "name": "read", "arguments": map[string]any{"path": "/home/test/skills/example/SKILL.md"}}}})
	tr := ompParse(t, text)
	if tr.entries[2].kind != kindAssistantText || tr.entries[2].text != "provider unavailable" || tr.entries[2].model != "provider/default" {
		t.Fatalf("failure=%+v", tr.entries[2])
	}
	if len(tr.skillHits) != 1 || tr.skillHits[0].span != "read" || tr.skillHits[0].dir != "example" {
		t.Fatalf("skill paths=%+v", tr.skillHits)
	}
	if c := tr.entries[3].calls[0]; c.result || c.status != "" || !strings.Contains(string(c.input), "/SKILL.md") {
		t.Fatalf("missing result became success: %+v", c)
	}
}

func TestOMPRefusesReplacedSourceBeforeRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture")
	}
	w := newWorld(t)
	env := w.env()
	root := (ompReader{}).location(env)
	path := ompPut(t, root, "project/log.jsonl", ompHead(t, "replace", defaultCwd))
	cs := ompCandidates(t, env)
	target := ompPut(t, root, "other.jsonl", ompHead(t, "replacement", "/denied"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	env.Open = func(string) (io.ReadCloser, error) {
		t.Fatal("opened symlink replacement")
		return nil, errors.New("unreachable")
	}
	if _, err := (ompReader{}).load(env, cs[0]); err == nil {
		t.Fatal("accepted a replaced symlink")
	}
}

func TestOMPUnsupportedHeaderNeverOpensBody(t *testing.T) {
	w := newWorld(t)
	env := w.env()
	header := strings.Replace(ompHead(t, "future-body", defaultCwd), `"version":3`, `"version":4`, 1)
	ompPut(t, (ompReader{}).location(env), "future.jsonl", header+"PRIVATE BODY\n")
	reads, opens := 0, 0
	env.Open = func(path string) (io.ReadCloser, error) {
		opens++
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &ompTrackedFile{File: f, read: &reads, max: len(header)}, nil
	}
	cs := ompCandidates(t, env)
	if len(cs) != 1 {
		t.Fatal(cs)
	}
	ev := read(env, ompReader{}, cs[0], nil, true)
	if ev.state != StateUnsupportedFormat || opens != 1 || reads != len(header) {
		t.Fatalf("state=%s opens=%d reads=%d", ev.state, opens, reads)
	}
}

func TestOMPPrimaryDirectoryAndRelativePolicy(t *testing.T) {
	w := newWorld(t)
	primary, extra := filepath.Join(w.home, "primary"), filepath.Join(w.home, "extra")
	text := ompHead(t, "directories", primary, extra, "relative-extra") + ompHuman(t, "p", "run tests")
	tr := ompParse(t, text)
	if len(tr.cwds) != 2 || tr.cwds[0] != primary || tr.cwds[1] != extra {
		t.Fatalf("primary directory changed: %v", tr.cwds)
	}
	env := w.env()
	env.Policy.OptInOnly, env.Policy.OptInPaths = true, []string{primary, extra}
	_, meta, err := ompHeader(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := (ompReader{}).gate(env, candidate{omp: meta}); state != "" {
		t.Fatalf("relative extra directory affected policy: %s %s", state, reason)
	}
	meta.Cwd = "relative-primary"
	if state, _ := (ompReader{}).gate(env, candidate{omp: meta}); state != StateDenied {
		t.Fatalf("unknown primary bypassed opt-in policy: %s", state)
	}
}

func TestOMPBranchToolReplay(t *testing.T) {
	row := func(id, parent string, message map[string]any) string {
		return ompJSON(t, map[string]any{"type": "message", "id": id, "parentId": parent, "message": message})
	}
	text := ompHead(t, "replay", defaultCwd) + ompHuman(t, "p", "run tests")
	text += row("a", "p", map[string]any{"role": "assistant", "model": "model-a", "content": []any{map[string]any{"type": "toolCall", "id": "call", "name": "bash", "arguments": map[string]any{"command": "go test ./..."}}}})
	text += row("failed", "a", map[string]any{"role": "toolResult", "toolCallId": "call", "isError": true, "content": "first attempt failed"})
	text += ompJSON(t, map[string]any{"type": "model_change", "id": "other-model", "parentId": "failed", "model": "model-b"})
	text += row("replayed", "a", map[string]any{"role": "toolResult", "toolCallId": "call", "content": "replay passed"})
	tr := ompParse(t, text)
	if tr.total.ToolCalls != 2 || tr.total.ToolErrors != 1 || tr.entries[len(tr.entries)-1].model != "model-a" {
		t.Fatalf("replay counts/model: %+v %+v", tr.total, tr.entries[len(tr.entries)-1])
	}
	first, replay := tr.entries[1].calls[0], tr.entries[len(tr.entries)-1].calls[0]
	if first == replay || first.status != statusError || replay.status != statusOK || first.content != "first attempt failed" {
		t.Fatalf("replay overwrote first attempt: first=%+v replay=%+v", first, replay)
	}
	linear := text + row("duplicate", "replayed", map[string]any{"role": "toolResult", "toolCallId": "call", "content": "duplicate"})
	_, meta, err := ompHeader(strings.NewReader(linear))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (ompReader{}).parse(strings.NewReader(linear), meta); !errors.Is(err, errUnsupported) {
		t.Fatalf("same-branch duplicate result accepted: %v", err)
	}
}
