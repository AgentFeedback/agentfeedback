package sessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// corpusLabel is one entry of testdata/corpus/labels.json: what a span of
// a corpus session is, as a reader of the session would judge it. Tool
// names the call when its span has several (a Gemini CLI message).
type corpusLabel struct {
	Session string `json:"session"`
	Span    string `json:"span"`
	Tool    string `json:"tool,omitempty"`
	Class   string `json:"class"`
	Note    string `json:"note"`
}

// Corpus classes.
const (
	classFriction     = "genuine-friction"
	classExpected     = "expected-failure"
	classCancellation = "cancellation"
	classDenial       = "denial"
	classRepeat       = "repeat"
	classSelf         = "self-noise"
)

// corpusHarness is the harness of a corpus file, by its name's prefix.
func corpusHarness(t *testing.T, name string) string {
	t.Helper()
	for _, h := range []string{HarnessClaudeCode, HarnessCodex, HarnessCopilot, HarnessGeminiCLI} {
		if strings.HasPrefix(name, h+"-") {
			return h
		}
	}
	t.Fatalf("corpus file %s: no harness prefix", name)

	return ""
}

// materialiseCorpus writes every corpus session into w in its reader's
// store layout, the session id being the file's stem, and returns the
// corpus file of each session ref.
func materialiseCorpus(t *testing.T, w *world) map[string]string {
	t.Helper()
	dir := filepath.Join("testdata", "corpus")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		sid := strings.TrimSuffix(name, ".jsonl")
		content := fixture(t, filepath.Join("corpus", name), sid, defaultCwd)
		h := corpusHarness(t, name)
		switch h {
		case HarnessClaudeCode:
			w.put(defaultCwd, sid, content)
		case HarnessCodex:
			p := filepath.Join(w.home, ".codex", "sessions", "2026", "10", "08", "rollout-2026-10-08T08-00-00-"+sid+".jsonl")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		case HarnessCopilot:
			cpPut(w, sid, content, "")
		case HarnessGeminiCLI:
			dir := filepath.Join(w.home, ".gemini", "tmp", sid)
			if err := os.MkdirAll(filepath.Join(dir, "chats"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".project_root"), []byte(defaultCwd+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			sid = "session-" + sid
			if err := os.WriteFile(filepath.Join(dir, "chats", sid+".jsonl"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		files[Ref(h, sid)] = name
	}

	return files
}

// TestCorpus digests the labelled corpus and checks that the digest shows
// every labelled span the way its class says, and that every tool call
// event the digests emit is labelled.
func TestCorpus(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("testdata", "corpus", "labels.json"))
	if err != nil {
		t.Fatal(err)
	}
	var labels []corpusLabel
	if err := json.Unmarshal(b, &labels); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	files := materialiseCorpus(t, w)
	out := w.digest(DigestRequest{Unprocessed: true})

	var bad []string
	failf := func(format string, args ...any) { bad = append(bad, fmt.Sprintf(format, args...)) }
	if len(out.Errors) > 0 || len(out.Deferred) > 0 {
		failf("errors %+v, deferred %v", out.Errors, out.Deferred)
	}
	// Events are keyed by file, span and tool: a span may carry several
	// calls.
	type key struct{ file, span, tool string }
	type spanKey struct{ file, span string }
	var events []key
	byKey := map[key]Event{}
	retries := map[key]Retry{}
	denials := map[spanKey]bool{}
	digested := map[string]bool{}
	ordered := map[string][]Event{} // tool call events per file, in digest order
	for _, sd := range out.Sessions {
		file, ok := files[sd.Ref]
		if !ok {
			failf("%s: not a corpus session", sd.Ref)

			continue
		}
		digested[file] = true
		for _, e := range sd.Events {
			if e.Type == "tool_call" {
				k := key{file, e.Span, e.Tool}
				if _, dup := byKey[k]; dup {
					failf("%s#%s: two %s calls in one span", file, e.Span, e.Tool)
				}
				events = append(events, k)
				byKey[k] = e
				ordered[file] = append(ordered[file], e)
			}
		}
		for _, r := range sd.Retries {
			retries[key{file, r.Span, r.Tool}] = r
		}
		for _, d := range sd.Denials {
			denials[spanKey{file, d}] = true
		}
	}
	for _, file := range files {
		if !digested[file] {
			failf("%s: not digested", file)
		}
	}

	// find is the key of the label's event: the one with its tool, or the
	// only one of its span when it names none.
	find := func(l corpusLabel) (key, bool) {
		if l.Tool != "" {
			k := key{l.Session, l.Span, l.Tool}
			_, ok := byKey[k]

			return k, ok
		}
		var found []key
		for _, k := range events {
			if k.file == l.Session && k.span == l.Span {
				found = append(found, k)
			}
		}
		if len(found) > 1 {
			failf("%s#%s: the span has %d calls; the label must name its tool", l.Session, l.Span, len(found))
		}
		if len(found) == 0 {
			return key{l.Session, l.Span, ""}, false
		}

		return found[0], true
	}
	labelled := map[key]bool{}
	for _, l := range labels {
		k, ok := find(l)
		if l.Class != classRepeat && ok {
			labelled[k] = true
		}
		e := byKey[k]
		switch l.Class {
		case classFriction, classExpected:
			if !ok || e.Status != statusError || e.Self {
				failf("%s#%s (%s): want a tool_call event with status error and no self, got %+v (found %v)", l.Session, l.Span, l.Class, e, ok)
			}
		case classCancellation:
			if !ok || e.Status != statusInterrupted {
				failf("%s#%s (%s): want status interrupted, got %+v (found %v)", l.Session, l.Span, l.Class, e, ok)
			}
		case classDenial:
			if !ok || e.Status != statusDenied || !denials[spanKey{k.file, k.span}] {
				failf("%s#%s (%s): want status denied and the span in denials, got %+v (found %v, in denials %v)", l.Session, l.Span, l.Class, e, ok, denials[spanKey{k.file, k.span}])
			}
		case classSelf:
			if !ok || !e.Self {
				failf("%s#%s (%s): want an event with self, got %+v (found %v)", l.Session, l.Span, l.Class, e, ok)
			}
		case classRepeat:
			var r Retry
			if ok {
				r, ok = retries[k]
			}
			if !ok {
				failf("%s#%s (%s): want a retries entry", l.Session, l.Span, l.Class)

				break
			}
			first := firstFailure(ordered[l.Session], r)
			if r.OfSpan != first {
				failf("%s#%s (%s): of_span %s, want the first failure %s", l.Session, l.Span, l.Class, r.OfSpan, first)
			}
		default:
			failf("%s#%s: unknown class %q", l.Session, l.Span, l.Class)
		}
	}
	var unlabelled []string
	for _, k := range events {
		if !labelled[k] {
			unlabelled = append(unlabelled, k.file+"#"+k.span+" ("+k.tool+")")
		}
	}
	sort.Strings(unlabelled)
	for _, u := range unlabelled {
		failf("%s: tool_call event without a label", u)
	}
	if len(bad) > 0 {
		t.Fatalf("corpus mismatches:\n  %s", strings.Join(bad, "\n  "))
	}
}

// firstFailure is the span of the earliest tool call event among events
// with the retry's tool and arguments digest, "" when there is none.
func firstFailure(events []Event, r Retry) string {
	for _, e := range events {
		if e.Tool == r.Tool && e.ArgsDigest == r.ArgsDigest {
			return e.Span
		}
	}

	return ""
}
