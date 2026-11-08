package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

func captureOne(t *testing.T, w *world, env Env, ref string) CaptureCandidate {
	t.Helper()
	cs, err := CaptureCandidates(w.ctx, w.db, env, []string{ref})
	if err != nil || len(cs) != 1 {
		t.Fatalf("candidates: count=%d error=%v", len(cs), err)
	}
	return cs[0]
}

func readCapture(t *testing.T, env Env, c CaptureCandidate) ([]byte, CaptureMeta) {
	t.Helper()
	r, meta, err := OpenCapture(context.Background(), env, c)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	cerr := r.Close()
	if err != nil || cerr != nil {
		t.Fatalf("read=%v close=%v", err, cerr)
	}
	return b, meta
}

func assertCaptureRefused(t *testing.T, env Env, c CaptureCandidate, state string) {
	t.Helper()
	if c.State != state || c.Size != 0 || c.Head != "" || c.Cwd != "" || len(c.Cwds) != 0 || c.Model != "" || c.Counts != (Counts{}) {
		t.Fatalf("refusal retained transcript metadata: state=%s size=%d", c.State, c.Size)
	}
	r, _, err := OpenCapture(context.Background(), env, c)
	var refused *RefusedError
	if r != nil || !errors.As(err, &refused) || refused.State != state {
		t.Fatalf("raw refusal: reader=%v error=%v", r != nil, err)
	}
}

func TestCaptureClaudeCompleteLines(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	complete := strings.ReplaceAll(fixture(t, "basic.jsonl", "capture", defaultCwd), "\n", "\r\n")
	w.put(defaultCwd, "capture", complete+`{"type":"user","message":{"content":"unfinished`)
	normal, err := (claudeReader{}).parse(strings.NewReader(complete))
	if err != nil {
		t.Fatal(err)
	}
	c := captureOne(t, w, w.env(), "claude-code:capture")
	wantCounts := Counts{Entries: 13, Prompts: 2, ToolCalls: 4, ToolErrors: 1, Interrupts: 1, Denials: 1, Unparsed: 1}
	if c.State != StateNew || c.Size != int64(len(complete)) || c.Cwd != defaultCwd || c.Model != "claude-test-2" || c.Counts != wantCounts {
		t.Fatalf("incorrect complete-line metadata: state=%s bytes=%d counts=%+v", c.State, c.Size, c.Counts)
	}
	if c.Counts != normal.total || c.Cwd != normal.lastCwd || !reflect.DeepEqual(c.Cwds, normal.cwds) || c.Model != normal.lastModel || !c.Start.Equal(normal.start) || !c.End.Equal(normal.end) {
		t.Fatal("Claude capture metadata differs from the normal reader")
	}
	first, _, _ := strings.Cut(complete, "\r\n")
	sum := sha256.Sum256([]byte(first))
	if c.Head != hex.EncodeToString(sum[:]) {
		t.Fatal("head is not the digest first-line hash")
	}
	b, meta := readCapture(t, w.env(), c)
	if !bytes.Equal(b, []byte(complete)) {
		t.Fatal("capture did not preserve complete CRLF bytes")
	}
	if meta.Format != "claude-code-jsonl" || meta.LineCount != 14 || meta.Counts != wantCounts || meta.Model != "claude-test-2" || meta.Start.Format(time.RFC3339) != "2026-10-08T07:39:40Z" || meta.End.Format(time.RFC3339) != "2026-10-08T07:40:40Z" {
		t.Fatalf("incorrect capture metadata: %+v", meta)
	}
	if err := w.db.Read(w.ctx, func(q store.Querier) error {
		rows, err := store.ListSessions(w.ctx, q, HarnessClaudeCode)
		if len(rows) != 0 {
			t.Fatal("capture persisted digest watermarks")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureCodexCompleteLines(t *testing.T) {
	t.Parallel()
	w := newCodexWorld(t, defaultCwd)
	p := w.paths[cxLegacy]
	complete, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	w.appendTo(p, `{"type":"event_msg","payload":{"type":"user_message"`)
	c := captureOne(t, w.world, w.env(), Ref(HarnessCodex, cxLegacy))
	b, meta := readCapture(t, w.env(), c)
	if !bytes.Equal(b, complete) || c.Size != int64(len(complete)) {
		t.Fatal("Codex capture changed complete bytes or included partial line")
	}
	if meta.Format != "codex-rollout" || meta.LineCount != 30 || meta.Model != "gpt-test-2" || meta.Counts != c.Counts || meta.Counts.Prompts != 2 || meta.Counts.ToolCalls != 7 || meta.Start.Format(time.RFC3339) != "2026-10-08T07:39:40Z" || meta.End.Format(time.RFC3339) != "2026-10-08T07:40:41Z" {
		t.Fatalf("incorrect Codex capture metadata: %+v", meta)
	}
	normal, err := (codexReader{}).parse(bytes.NewReader(complete))
	if err != nil {
		t.Fatal(err)
	}
	if c.Counts != normal.total || c.Cwd != normal.lastCwd || !reflect.DeepEqual(c.Cwds, normal.cwds) || c.Model != normal.captureModel || !c.Start.Equal(normal.start) || !c.End.Equal(normal.end) {
		t.Fatal("Codex capture metadata differs from the normal reader")
	}
	before := w.opens[w.paths[cxZst]]
	compressed := captureOne(t, w.world, w.env(), Ref(HarnessCodex, cxZst))
	assertCaptureRefused(t, w.env(), compressed, StateUnsupportedFormat)
	if w.opens[w.paths[cxZst]] != before {
		t.Fatal("compressed rollout was opened")
	}
}

func TestCaptureOpenCodeVirtualLog(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.create(defaultCwd)
	w.exec(`DELETE FROM part; DELETE FROM message;
INSERT INTO message VALUES ('m1', '` + ocSID + `', 1000, 1000, '{"role":"user","time":{"created":1000},"mutable":"omit"}');
INSERT INTO part VALUES ('p1', 'm1', '` + ocSID + `', 1000, 1000, '{"type":"text","text":"prompt"}');
INSERT INTO message VALUES ('m2', '` + ocSID + `', 2000, 3000, '{"role":"assistant","time":{"created":2000,"completed":3000},"modelID":"last-model","cost":99}');
INSERT INTO part VALUES ('p2', 'm2', '` + ocSID + `', 2000, 3000, '{"type":"text","text":"answer"}');
INSERT INTO message VALUES ('m3', '` + ocSID + `', 4000, 4000, '{"role":"assistant","time":{"created":4000},"modelID":"incomplete"}');
INSERT INTO part VALUES ('p3', 'm3', '` + ocSID + `', 4000, 4000, '{"type":"text","text":"not complete"}');`)
	c := captureOne(t, w.world, w.env(), "opencode:"+ocSID)
	b, meta := readCapture(t, w.env(), c)
	want := `{"id":"m1","kind":"message","role":"user","time":{"created":1000}}
{"id":"p1","kind":"part","text":"prompt","type":"text"}
{"id":"m2","kind":"message","modelID":"last-model","role":"assistant","time":{"completed":3000,"created":2000}}
{"id":"p2","kind":"part","text":"answer","type":"text"}
`
	if string(b) != want || c.Ref != Ref(HarnessOpenCode, ocSID) || c.Size != int64(len(want)) {
		t.Fatal("OpenCode virtual bytes differ from final-field record layout")
	}
	if meta.Format != "opencode-virtual-log" || meta.LineCount != 4 || meta.Model != "last-model" || meta.Counts != (Counts{Entries: 3, Prompts: 1}) || !meta.Start.Equal(time.UnixMilli(1000)) || !meta.End.Equal(time.UnixMilli(2000)) {
		t.Fatalf("incorrect OpenCode capture metadata: %+v", meta)
	}
	w.exec(ocAppend)
	if r, _, err := OpenCapture(w.ctx, w.env(), c); r != nil || !errors.Is(err, ErrCaptureChanged) {
		t.Fatalf("changed virtual log accepted: %v", err)
	}
}

func TestCaptureSnapshotChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"append", "partial append", "same-size rewrite", "mtime", "truncate", "rotate"} {
		t.Run(change, func(t *testing.T) {
			w := newWorld(t)
			body := fixture(t, "basic.jsonl", "stable", defaultCwd)
			p := w.put(defaultCwd, "stable", body)
			c := captureOne(t, w, w.env(), "claude-code:stable")
			switch change {
			case "append":
				w.appendTo(p, newPrompt)
			case "partial append":
				w.appendTo(p, `{"type":`)
			case "same-size rewrite":
				if err := os.WriteFile(p, []byte(strings.Replace(body, "Done.", "Gone.", 1)), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(p, c.Mtime.Add(time.Second), c.Mtime.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			case "mtime":
				if err := os.Chtimes(p, c.Mtime.Add(time.Second), c.Mtime.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			case "truncate":
				if err := os.Truncate(p, 1); err != nil {
					t.Fatal(err)
				}
			case "rotate":
				if err := os.Rename(p, p+".old"); err != nil {
					t.Fatal(err)
				}
				w.put(defaultCwd, "stable", body)
			}
			if r, _, err := OpenCapture(w.ctx, w.env(), c); r != nil || !errors.Is(err, ErrCaptureChanged) {
				t.Fatalf("changed source accepted: %v", err)
			}
		})
	}
}

func TestCaptureChecksReadAndClose(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"EOF", "close"} {
		t.Run(boundary, func(t *testing.T) {
			w := newWorld(t)
			p := w.put(defaultCwd, "stable", fixture(t, "basic.jsonl", "stable", defaultCwd))
			c := captureOne(t, w, w.env(), "claude-code:stable")
			r, _, err := OpenCapture(w.ctx, w.env(), c)
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "close" {
				if _, err := io.Copy(io.Discard, r); err != nil {
					t.Fatal(err)
				}
			}
			w.appendTo(p, newPrompt)
			if boundary == "EOF" {
				if _, err := io.Copy(io.Discard, r); !errors.Is(err, ErrCaptureChanged) {
					t.Fatalf("read accepted changed source: %v", err)
				}
			}
			if err := r.Close(); !errors.Is(err, ErrCaptureChanged) {
				t.Fatalf("close accepted changed source: %v", err)
			}
		})
	}
}

func captureRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "nested", "project")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, cwd
}

func TestCapturePolicyRefusals(t *testing.T) {
	t.Parallel()
	t.Run("disabled and denied never open Claude", func(t *testing.T) {
		w := newWorld(t)
		p := w.put(defaultCwd, "blocked", fixture(t, "basic.jsonl", "blocked", defaultCwd))
		for _, policy := range []collect.Policy{{Disabled: true}, {DenyPaths: []string{defaultCwd}}} {
			w.pol = policy
			c := captureOne(t, w, w.env(), "claude-code:blocked")
			state := StateDenied
			if policy.Disabled {
				state = StateDisabled
			}
			assertCaptureRefused(t, w.env(), c, state)
		}
		if w.opens[p] != 0 {
			t.Fatal("pre-read refusal opened transcript")
		}
	})
	t.Run("all recorded cwds and last cwd", func(t *testing.T) {
		w := newWorld(t)
		root, last := captureRepo(t)
		body := fixture(t, "basic.jsonl", "mixed", defaultCwd)
		body += strings.ReplaceAll(newPrompt, defaultCwd, last)
		body += newPrompt
		w.put(defaultCwd, "mixed", body)
		c := captureOne(t, w, w.env(), "claude-code:mixed")
		if c.Cwd != defaultCwd || len(c.Cwds) != 2 {
			t.Fatal("last repeated cwd was not preserved")
		}
		if err := os.WriteFile(filepath.Join(root, collect.RepoFile), []byte("[sessions]\npush = false\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := w.opens[c.source.c.path]
		if r, _, err := OpenCapture(w.ctx, w.env(), c); r != nil || err == nil {
			t.Fatal("new repository veto did not refuse existing candidate")
		}
		if w.opens[c.source.c.path] != before {
			t.Fatal("repository veto reopened raw content")
		}
		assertCaptureRefused(t, w.env(), captureOne(t, w, w.env(), c.Ref), StateDisabled)
	})
	t.Run("Codex repository veto reads metadata only", func(t *testing.T) {
		root, cwd := captureRepo(t)
		w := newCodexWorld(t, cwd)
		if err := os.WriteFile(filepath.Join(root, collect.RepoFile), []byte("[sessions]\npush = false\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(w.paths[cxLegacy])
		if err != nil {
			t.Fatal(err)
		}
		read := 0
		e := w.env()
		e.Open = func(string) (io.ReadCloser, error) {
			return &captureCountReader{Reader: bytes.NewReader(content), count: &read}, nil
		}
		c := captureOne(t, w.world, e, Ref(HarnessCodex, cxLegacy))
		assertCaptureRefused(t, e, c, StateDisabled)
		if read != bytes.IndexByte(content, '\n')+1 {
			t.Fatal("repository veto consumed more than the Codex metadata line")
		}
	})
	t.Run("OpenCode repository veto never loads messages", func(t *testing.T) {
		root, cwd := captureRepo(t)
		w := newOCWorld(t)
		p := w.create(cwd)
		if err := os.WriteFile(filepath.Join(root, collect.RepoFile), []byte("[sessions]\npush = false\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := ocLoadCount(p)
		c := captureOne(t, w.world, w.env(), Ref(HarnessOpenCode, ocSID))
		assertCaptureRefused(t, w.env(), c, StateDisabled)
		if ocLoadCount(p) != before {
			t.Fatal("repository veto loaded OpenCode message content")
		}
	})
	t.Run("unknown project", func(t *testing.T) {
		w := newWorld(t)
		w.put(defaultCwd, "unknown", fixture(t, "nocwd.jsonl", "unknown", ""))
		assertCaptureRefused(t, w.env(), captureOne(t, w, w.env(), "claude-code:unknown"), StateUnknownProject)
	})
}

type captureCountReader struct {
	io.Reader
	count *int
}

func (r *captureCountReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	*r.count += n
	return n, err
}

func (*captureCountReader) Close() error { return nil }

func TestCaptureNamedOutcomesAndTimestampFallback(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "untimed", `{"type":"user","cwd":"`+defaultCwd+`","message":{"content":"prompt"}}`+"\n")
	c := captureOne(t, w, w.env(), "claude-code:untimed")
	_, meta := readCapture(t, w.env(), c)
	if !meta.Start.Equal(c.Mtime) || !meta.End.Equal(c.Mtime) {
		t.Fatal("missing timestamps did not fall back to mtime")
	}
	cs, err := CaptureCandidates(w.ctx, w.db, w.env(), []string{"claude-code:missing", "copilot:later", "gemini-cli:later", "cursor:later"})
	if err != nil || len(cs) != 4 {
		t.Fatalf("named outcomes vanished: %v", err)
	}
	for i, c := range cs {
		state := StateUnsupportedFormat
		if i == 0 {
			state = StateAbsent
		}
		assertCaptureRefused(t, w.env(), c, state)
	}
	all, err := CaptureCandidates(w.ctx, w.db, w.env(), nil)
	if err != nil || len(all) != 1 || all[0].Ref != "claude-code:untimed" {
		t.Fatalf("unexpected supported-source enumeration: count=%d error=%v", len(all), err)
	}
}

func TestCaptureRejectsMutationDuringRawLoad(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := w.put(defaultCwd, "racing", fixture(t, "basic.jsonl", "racing", defaultCwd))
	c := captureOne(t, w, w.env(), "claude-code:racing")
	e := w.env()
	e.Open = func(path string) (io.ReadCloser, error) {
		f, err := os.Open(path)
		if err == nil {
			w.appendTo(p, newPrompt)
		}
		return f, err
	}
	if r, _, err := OpenCapture(w.ctx, e, c); r != nil || !errors.Is(err, ErrCaptureChanged) {
		t.Fatalf("mutation during raw load accepted: %v", err)
	}
}

func TestCaptureRechecksUnscrubbedCwdPolicy(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	root, _ := captureRepo(t)
	cwd := filepath.Join(root, "ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	w.put(cwd, "secret-path", fixture(t, "basic.jsonl", "secret-path", cwd))
	c := captureOne(t, w, w.env(), "claude-code:secret-path")
	if c.Cwd != cwd || len(c.Cwds) != 1 || c.Cwds[0] != cwd {
		t.Fatal("capture scrubbed a path needed for filesystem policy")
	}
	if err := os.WriteFile(filepath.Join(root, collect.RepoFile), []byte("[sessions]\npush = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Public metadata is not the authority for the source's recorded paths.
	c.Cwds = nil
	r, _, err := OpenCapture(w.ctx, w.env(), c)
	var refused *RefusedError
	if r != nil || !errors.As(err, &refused) || refused.State != StateDisabled {
		t.Fatalf("unscrubbed source path did not enforce repository policy: %v", err)
	}
}

func TestCaptureCodexKeepsLastAssistantModel(t *testing.T) {
	t.Parallel()
	w := newCodexWorld(t, defaultCwd)
	w.appendTo(w.paths[cxLegacy], `{"timestamp":"2026-10-08T07:41:00Z","type":"turn_context","payload":{"cwd":"`+defaultCwd+`","model":"not-used-yet"}}`+"\n")
	c := captureOne(t, w.world, w.env(), Ref(HarnessCodex, cxLegacy))
	_, meta := readCapture(t, w.env(), c)
	if c.Model != "gpt-test-2" || meta.Model != "gpt-test-2" {
		t.Fatal("an unused turn model replaced the last assistant model")
	}
}

func TestCaptureCompletionRechecksHeadAndCompleteSize(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"head", "complete boundary"} {
		t.Run(mutation, func(t *testing.T) {
			w := newWorld(t)
			body := fixture(t, "basic.jsonl", "fingerprint", defaultCwd) + "unfinished"
			p := w.put(defaultCwd, "fingerprint", body)
			c := captureOne(t, w, w.env(), "claude-code:fingerprint")
			r, _, err := OpenCapture(w.ctx, w.env(), c)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "head":
				body = strings.Replace(body, "enqueue", "dequeue", 1)
			case "complete boundary":
				body = body[:len(body)-1] + "\n"
			}
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, c.Mtime, c.Mtime); err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, r); !errors.Is(err, ErrCaptureChanged) {
				t.Fatalf("same-stat fingerprint change accepted: %v", err)
			}
			if err := r.Close(); !errors.Is(err, ErrCaptureChanged) {
				t.Fatalf("same-stat fingerprint change accepted at close: %v", err)
			}
		})
	}
}

func TestCapturePostReadCwdRefusal(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const denied = "/nonexistent-af/denied"
	body := fixture(t, "basic.jsonl", "mixed-policy", defaultCwd)
	body += strings.ReplaceAll(newPrompt, defaultCwd, denied)
	p := w.put(defaultCwd, "mixed-policy", body)
	w.pol = collect.Policy{DenyPaths: []string{denied}}
	c := captureOne(t, w, w.env(), "claude-code:mixed-policy")
	assertCaptureRefused(t, w.env(), c, StateDenied)
	if w.opens[p] != 1 {
		t.Fatal("post-read refusal did not discard the parsed transcript")
	}
}

type captureCancelReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *captureCancelReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.cancel()
	return n, err
}

func TestCaptureCancellationKeepsEarlierOutcomes(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{HarnessClaudeCode, HarnessCodex} {
		t.Run(harness, func(t *testing.T) {
			w := newWorld(t)
			var env Env
			var ref string
			if harness == HarnessClaudeCode {
				w.put(defaultCwd, "cancelled", fixture(t, "basic.jsonl", "cancelled", defaultCwd))
				env, ref = w.env(), "claude-code:cancelled"
			} else {
				cw := newCodexWorld(t, defaultCwd)
				w, env, ref = cw.world, cw.env(), Ref(HarnessCodex, cxLegacy)
			}
			ctx, cancel := context.WithCancel(w.ctx)
			defer cancel()
			env.Open = func(path string) (io.ReadCloser, error) {
				f, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				return &captureCancelReader{ReadCloser: f, cancel: cancel}, nil
			}
			cs, err := CaptureCandidates(ctx, w.db, env, []string{"cursor:earlier", ref, "cursor:later"})
			if !errors.Is(err, context.Canceled) || len(cs) != 1 || cs[0].Ref != "cursor:earlier" {
				t.Fatalf("cancellation lost completed outcomes: candidates=%v error=%v", cs, err)
			}
		})
	}
}

func TestCaptureCancelledBeforeEnumerationOrExport(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	p := w.put(defaultCwd, "cancelled", fixture(t, "basic.jsonl", "cancelled", defaultCwd))
	c := captureOne(t, w, w.env(), "claude-code:cancelled")
	ctx, cancel := context.WithCancel(w.ctx)
	cancel()
	before := w.opens[p]
	cs, err := CaptureCandidates(ctx, w.db, w.env(), nil)
	if len(cs) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled enumeration: count=%d error=%v", len(cs), err)
	}
	if r, _, err := OpenCapture(ctx, w.env(), c); r != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled export: reader=%v error=%v", r, err)
	}
	if err := CheckCapture(ctx, w.env(), c); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot check: %v", err)
	}
	if w.opens[p] != before {
		t.Fatal("cancelled operation opened transcript")
	}
}

func TestCaptureStreamingCancellation(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "cancelled", fixture(t, "basic.jsonl", "cancelled", defaultCwd))
	c := captureOne(t, w, w.env(), "claude-code:cancelled")
	ctx, cancel := context.WithCancel(w.ctx)
	r, _, err := OpenCapture(ctx, w.env(), c)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read ignored cancellation: %v", err)
	}
	if err := r.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("close ignored cancellation: %v", err)
	}
}

func TestCaptureStreamsWithoutRawBuffer(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	body := fixture(t, "basic.jsonl", "streaming", defaultCwd)
	w.put(defaultCwd, "streaming", body)
	c := captureOne(t, w, w.env(), "claude-code:streaming")
	env := w.env()
	var consumed []*int
	env.Open = func(string) (io.ReadCloser, error) {
		count := new(int)
		consumed = append(consumed, count)
		return &captureCountReader{Reader: strings.NewReader(body), count: count}, nil
	}
	r, _, err := OpenCapture(w.ctx, env, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(consumed) != 2 || *consumed[0] != len(body) || *consumed[1] != 0 {
		t.Fatalf("export eagerly buffered raw bytes: reads=%v", consumed)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != body {
		t.Fatalf("streamed bytes differ: error=%v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCaptureRechecksSourceAndRepository(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"source", "repository", "policy"} {
		t.Run(mutation, func(t *testing.T) {
			w := newWorld(t)
			root, cwd := captureRepo(t)
			p := w.put(cwd, "checked", fixture(t, "basic.jsonl", "checked", cwd))
			c := captureOne(t, w, w.env(), "claude-code:checked")
			env := w.env()
			if err := CheckCapture(w.ctx, env, c); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "source":
				w.appendTo(p, newPrompt)
			case "repository":
				if err := os.WriteFile(filepath.Join(root, collect.RepoFile), []byte("[sessions]\npush = false\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "policy":
				env.Policy.Disabled = true
			}
			before := w.opens[p]
			err := CheckCapture(w.ctx, env, c)
			if mutation == "source" {
				if !errors.Is(err, ErrCaptureChanged) {
					t.Fatalf("changed source accepted: %v", err)
				}
			} else {
				var refused *RefusedError
				if !errors.As(err, &refused) || refused.State != StateDisabled {
					t.Fatalf("revoked permission accepted: %v", err)
				}
			}
			if w.opens[p] != before {
				t.Fatal("snapshot or permission refusal opened transcript")
			}
		})
	}
}

func TestCaptureOpenCodeCancelsCandidateAndMessageReads(t *testing.T) {
	// These hooks replace package-level seams, so this test must not run
	// in parallel with other readers.
	for _, stage := range []string{"candidates", "messages"} {
		t.Run(stage, func(t *testing.T) {
			w := newOCWorld(t)
			w.create(defaultCwd)
			ctx, cancel := context.WithCancel(w.ctx)
			defer cancel()
			if stage == "candidates" {
				old := opencodeStat
				opencodeStat = func(path string) (os.FileInfo, error) {
					cancel()
					return os.Stat(path)
				}
				defer func() { opencodeStat = old }()
			} else {
				old := opencodeLoadHook
				opencodeLoadHook = func(candidate) { cancel() }
				defer func() { opencodeLoadHook = old }()
			}
			cs, err := CaptureCandidates(ctx, w.db, w.env(), []string{"cursor:earlier", Ref(HarnessOpenCode, ocSID)})
			if !errors.Is(err, context.Canceled) || len(cs) != 1 || cs[0].Ref != "cursor:earlier" {
				t.Fatalf("SQLite cancellation lost completed outcomes: candidates=%v error=%v", cs, err)
			}
		})
	}
}

func TestCaptureCancelledSweepKeepsEvaluatedCandidates(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "a-first", fixture(t, "basic.jsonl", "a-first", defaultCwd))
	second := w.put(defaultCwd, "b-second", fixture(t, "basic.jsonl", "b-second", defaultCwd))
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	env := w.env()
	env.Open = func(path string) (io.ReadCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		if path == second {
			return &captureCancelReader{ReadCloser: f, cancel: cancel}, nil
		}
		return f, nil
	}
	cs, err := CaptureCandidates(ctx, w.db, env, nil)
	if !errors.Is(err, context.Canceled) || len(cs) != 1 || cs[0].Ref != "claude-code:a-first" || cs[0].State != StateNew {
		t.Fatalf("cancelled sweep lost evaluated candidate: candidates=%v error=%v", cs, err)
	}
}

func TestCheckCaptureCancellationDuringValidation(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(defaultCwd, "checked", fixture(t, "basic.jsonl", "checked", defaultCwd))
	c := captureOne(t, w, w.env(), "claude-code:checked")
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	env := w.env()
	env.Open = func(path string) (io.ReadCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &captureCancelReader{ReadCloser: f, cancel: cancel}, nil
	}
	if err := CheckCapture(ctx, env, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("validation hid cancellation as refusal: %v", err)
	}
}

func TestCheckCaptureRechecksSameStatRecordedCwd(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const allowed, denied = "/capture/allowed", "/capture/refused"
	body := fixture(t, "basic.jsonl", "checked", allowed)
	p := w.put(allowed, "checked", body)
	env := w.env()
	env.Policy.DenyPaths = []string{denied}
	c := captureOne(t, w, env, "claude-code:checked")
	first, rest, _ := strings.Cut(body, "\n")
	body = first + "\n" + strings.ReplaceAll(rest, allowed, denied)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, c.Mtime, c.Mtime); err != nil {
		t.Fatal(err)
	}
	var refused *RefusedError
	if err := CheckCapture(w.ctx, env, c); !errors.As(err, &refused) || refused.State != StateDenied {
		t.Fatalf("unchanged size/head/stat bypassed recorded cwd policy: %v", err)
	}
}

// assertCaptureMetadata checks every retained field, including call pointers
// that the parsers' result maps also reference, without changing the normal read.
func assertCaptureMetadata(t *testing.T, normal, captured *transcript) {
	t.Helper()
	want := *normal
	want.captureOnly = captured.captureOnly
	want.entries = make([]entry, len(normal.entries))
	for i, e := range normal.entries {
		e.text = ""
		if e.calls != nil {
			e.calls = append([]*call(nil), e.calls...)
			for j, c := range e.calls {
				metadata := *c
				metadata.input, metadata.command, metadata.content = nil, "", ""
				e.calls[j] = &metadata
			}
		}
		want.entries[i] = e
	}
	if !reflect.DeepEqual(&want, captured) {
		t.Fatal("capture retained record content or changed reader metadata")
	}
}

func TestCaptureReadersDiscardRecordContent(t *testing.T) {
	t.Parallel()
	w := newCodexWorld(t, defaultCwd)
	claude := fixture(t, "basic.jsonl", "metadata", defaultCwd) + strings.ReplaceAll(newPrompt, defaultCwd, defaultCwd+"/nested")
	claudePath := w.put(defaultCwd, "metadata", claude)
	for _, tc := range []struct {
		name    string
		content string
		reader  reader
		path    string
	}{
		{"claude", claude, claudeReader{}, claudePath},
		{"codex legacy", w.content(cxLegacy), codexReader{}, w.paths[cxLegacy]},
		{"codex paginated", w.content(cxPaginated), codexReader{}, w.paths[cxPaginated]},
		{"codex archived", w.content(cxArchived), codexReader{}, w.paths[cxArchived]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			normal, captured := &transcript{}, &transcript{captureOnly: true}
			var normalLine, captureLine func(int64, []byte) bool
			if _, ok := tc.reader.(claudeReader); ok {
				normalCalls, captureCalls := map[string]*call{}, map[string]*call{}
				normalLine = func(offset int64, body []byte) bool { return normal.addLine(offset, body, normalCalls) }
				captureLine = func(offset int64, body []byte) bool { return captured.addLine(offset, body, captureCalls) }
			} else {
				n := &codexParse{t: normal, calls: map[string]*call{}}
				c := &codexParse{t: captured, calls: map[string]*call{}}
				normalLine, captureLine = n.line, c.line
			}
			var offset int64
			for line := range strings.SplitAfterSeq(tc.content, "\n") {
				if !strings.HasSuffix(line, "\n") {
					break
				}
				body := []byte(strings.TrimRight(line, "\r\n"))
				if normalLine(offset, body) != captureLine(offset, body) {
					t.Fatal("capture changed record parsing")
				}
				assertCaptureMetadata(t, normal, captured)
				offset += int64(len(line))
			}
			for _, ctx := range []context.Context{nil, context.Background()} {
				env := w.env()
				env.ctx = ctx
				source := candidate{path: tc.path, gated: &gatedMeta{cwd: defaultCwd}}
				n, err := tc.reader.load(env, source)
				if err != nil {
					t.Fatal(err)
				}
				env.captureOnly = true
				c, err := tc.reader.load(env, source)
				if err != nil {
					t.Fatal(err)
				}
				if !c.captureOnly {
					t.Fatal("reader wrappers lost capture-only mode")
				}
				assertCaptureMetadata(t, n, c)
			}
		})
	}
}
