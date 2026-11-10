package sessions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/internal/detect"
	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

var cursorFixtureTime = time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)

func cursorEnv(w *world) Env {
	e := w.env()
	e.CacheDir = filepath.Join(w.home, ".cache", "agentfeedback")
	return e
}

func writeCursorIndex(t *testing.T, env Env, id, path, cwd string) string {
	t.Helper()
	return writeCursorMetadata(t, env, detect.CursorSession{
		ConversationID: id, TranscriptPath: path, WorkspaceRoots: []string{cwd},
	})
}

func writeCursorMetadata(t *testing.T, env Env, meta detect.CursorSession) string {
	t.Helper()
	index := detect.StatePath(env.CacheDir, HarnessCursor, meta.ConversationID)
	if err := os.MkdirAll(filepath.Dir(index), 0o700); err != nil {
		t.Fatal(err)
	}
	state := detect.State{Cursor: &meta}
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return index
}

func putCursor(t *testing.T, w *world, id, cwd, content string) string {
	t.Helper()
	path := filepath.Join(w.home, ".cursor", "agent-transcripts", id+".txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, cursorFixtureTime, cursorFixtureTime); err != nil {
		t.Fatal(err)
	}
	writeCursorIndex(t, cursorEnv(w), id, path, cwd)
	return path
}

func TestCursorSyntheticListGolden(t *testing.T) {
	t.Parallel()
	for _, surface := range []string{"ide", "cli"} {
		t.Run(surface, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			name := "cursor-ide-transcript.txt"
			if surface == "cli" {
				name = "cursor-cli-transcript.jsonl"
			}
			putCursor(t, w, surface, defaultCwd, fixture(t, name, surface, defaultCwd))
			l, err := List(w.ctx, w.db, cursorEnv(w), ListOptions{Harness: HarnessCursor})
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(l, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = []byte(strings.ReplaceAll(string(got), filepath.ToSlash(w.home), "{{HOME}}") + "\n")
			want, err := os.ReadFile(filepath.Join("testdata", "cursor-"+surface+"-list.golden.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("synthetic %s list differs:\n%s", surface, got)
			}
		})
	}
}

func TestCursorCaptureCompleteLines(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"cursor-ide-transcript.txt", "cursor-cli-transcript.jsonl"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := cursorEnv(w)
			complete := strings.ReplaceAll(fixture(t, name, "capture", defaultCwd), "\n", "\r\n")
			putCursor(t, w, "capture", defaultCwd, complete+"unfinished opaque tail")
			c := captureOne(t, w, e, "cursor:capture")
			if c.State != StateNew || c.Reader != "cursor-agent-transcript" || c.Size != int64(len(complete)) || c.Cwd != defaultCwd || c.Counts != (Counts{Entries: strings.Count(complete, "\n")}) || c.Model != "" {
				t.Fatalf("capture metadata: %+v", c)
			}
			first, _, _ := strings.Cut(complete, "\r\n")
			sum := sha256.Sum256([]byte(first))
			if c.Head != hex.EncodeToString(sum[:]) || !c.Start.Equal(cursorFixtureTime) || !c.End.Equal(cursorFixtureTime) {
				t.Fatal("head or mtime timestamp fallback differs")
			}
			b, meta := readCapture(t, e, c)
			if string(b) != complete || meta.Format != "cursor-agent-transcript" || meta.LineCount != int64(strings.Count(complete, "\n")) || !meta.Start.Equal(cursorFixtureTime) || !meta.End.Equal(cursorFixtureTime) {
				t.Fatalf("raw capture or storage-only metadata differs: %+v", meta)
			}
			// Exercise the rooted production opener, not only Env.Open.
			e.Open = nil
			b, _ = readCapture(t, e, c)
			if string(b) != complete {
				t.Fatal("rooted opener changed bytes")
			}
			if err := w.db.Read(w.ctx, func(q store.Querier) error {
				rows, err := store.ListSessions(w.ctx, q, HarnessCursor)
				if len(rows) != 0 {
					t.Fatal("storage capture wrote digest state")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCursorPolicyBeforeTranscriptOpen(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"disabled", "denied", "opt-in", "missing-workspace", "relative-workspace", "repo-disabled", "repo-push-disabled"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			cwd := defaultCwd
			want := StateDenied
			switch mode {
			case "disabled":
				w.pol.Disabled, want = true, StateDisabled
			case "denied":
				w.pol.DenyPaths = []string{cwd}
			case "opt-in":
				w.pol.OptInOnly = true
			case "missing-workspace":
				cwd, want = "", StateUnknownProject
			case "relative-workspace":
				cwd, want = "relative", StateUnknownProject
			case "repo-disabled", "repo-push-disabled":
				cwd = filepath.Join(w.home, "repo")
				if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cwd, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				config := "[collect]\ndisabled = true\n"
				if mode == "repo-push-disabled" {
					config = "[sessions]\npush = false\n"
				}
				if err := os.WriteFile(filepath.Join(cwd, collect.RepoFile), []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				want = StateDisabled
			}
			path := putCursor(t, w, "blocked", cwd, "opaque complete line\n")
			e := cursorEnv(w)
			if mode == "missing-workspace" {
				writeCursorMetadata(t, e, detect.CursorSession{ConversationID: "blocked", TranscriptPath: path})
			}
			c := captureOne(t, w, e, "cursor:blocked")
			assertCaptureRefused(t, e, c, want)
			if w.opens[path] != 0 {
				t.Fatalf("blocked transcript opened %d times", w.opens[path])
			}
			l, err := List(w.ctx, w.db, e, ListOptions{Harness: HarnessCursor})
			if err != nil || len(l.Sessions) != 1 || l.Sessions[0].State != want || l.Sessions[0].Counts != nil || w.opens[path] != 0 {
				t.Fatalf("list did not gate before opening: %+v %v", l, err)
			}
		})
	}
}

func TestCursorAllPolicyLocationsBeforeTranscriptOpen(t *testing.T) {
	t.Parallel()
	for _, location := range []string{"secondary-root", "cwd"} {
		for _, mode := range []string{"denied", "repo-disabled", "push-disabled", "relative", "nul"} {
			t.Run(location+"/"+mode, func(t *testing.T) {
				t.Parallel()
				w := newWorld(t)
				e := cursorEnv(w)
				path := putCursor(t, w, "all-roots", defaultCwd, "opaque\n")
				policyPath := filepath.Join(w.home, "private")
				want := StateDenied
				switch mode {
				case "denied":
					e.Policy.DenyPaths = []string{policyPath}
				case "repo-disabled", "push-disabled":
					if err := os.MkdirAll(filepath.Join(policyPath, ".git"), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(policyPath, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					config := "[collect]\ndisabled = true\n"
					if mode == "push-disabled" {
						config = "[sessions]\npush = false\n"
					}
					if err := os.WriteFile(filepath.Join(policyPath, collect.RepoFile), []byte(config), 0o600); err != nil {
						t.Fatal(err)
					}
					want = StateDisabled
				case "relative":
					policyPath, want = "relative", StateUnknownProject
				case "nul":
					policyPath, want = policyPath+"\x00", StateUnknownProject
				}
				meta := detect.CursorSession{
					ConversationID: "all-roots", TranscriptPath: path, WorkspaceRoots: []string{defaultCwd},
				}
				if location == "secondary-root" {
					meta.WorkspaceRoots = append(meta.WorkspaceRoots, policyPath)
				} else {
					meta.Cwd = policyPath
				}
				writeCursorMetadata(t, e, meta)
				c := captureOne(t, w, e, "cursor:all-roots")
				assertCaptureRefused(t, e, c, want)
				l, err := List(w.ctx, w.db, e, ListOptions{Harness: HarnessCursor})
				if err != nil || len(l.Sessions) != 1 || l.Sessions[0].State != want ||
					l.Sessions[0].Counts != nil || w.opens[path] != 0 {
					t.Fatalf("policy location failed to veto before open: %+v %v opens=%d", l, err, w.opens[path])
				}
			})
		}
	}
}

func TestCursorRootedCaptureRetainsPolicyLocations(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := cursorEnv(w)
	e.Open = nil
	path := putCursor(t, w, "rooted", defaultCwd, "opaque\nunfinished")
	roots := []string{defaultCwd, filepath.Join(w.home, "second")}
	cwd := filepath.Join(w.home, "tool-cwd")
	writeCursorMetadata(t, e, detect.CursorSession{
		ConversationID: "rooted", TranscriptPath: path, WorkspaceRoots: roots, Cwd: cwd,
	})
	c := captureOne(t, w, e, "cursor:rooted")
	wantCwds := []string{roots[0], roots[1], cwd}
	if c.State != StateNew || c.Cwd != roots[0] || len(c.Cwds) != len(wantCwds) {
		t.Fatalf("rooted capture metadata: %+v", c)
	}
	for i, want := range wantCwds {
		if c.Cwds[i] != want {
			t.Fatalf("policy locations: %q, want %q", c.Cwds, wantCwds)
		}
	}
	raw, _ := readCapture(t, e, c)
	if string(raw) != "opaque\n" {
		t.Fatalf("rooted capture included incomplete content: %q", raw)
	}
}

func TestCursorRefusesUnsafeSources(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"outside-home", "sibling-home", "relative", "symlink", "ancestor-symlink", "directory", "missing"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := cursorEnv(w)
			path := putCursor(t, w, "unsafe", defaultCwd, "opaque\n")
			switch mode {
			case "outside-home", "sibling-home":
				path = filepath.Join(filepath.Dir(w.home), "outside.txt")
				if mode == "sibling-home" {
					path = w.home + "-sibling.txt"
				}
				if err := os.WriteFile(path, []byte("outside\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "relative":
				path = "relative.txt"
			case "symlink":
				link := path + ".link"
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			case "ancestor-symlink":
				link := filepath.Join(w.home, "linked-transcripts")
				if err := os.Symlink(filepath.Dir(path), link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, filepath.Base(path))
			case "directory":
				path = filepath.Dir(path)
			case "missing":
				path += ".absent"
			}
			writeCursorIndex(t, e, "unsafe", path, defaultCwd)
			c := captureOne(t, w, e, "cursor:unsafe")
			assertCaptureRefused(t, e, c, StateUnreadable)
			l, err := List(w.ctx, w.db, e, ListOptions{Harness: HarnessCursor})
			if err != nil || len(l.Sessions) != 1 || l.Sessions[0].State != StateUnreadable || w.opens[path] != 0 {
				t.Fatalf("unsafe source opened or accepted: %+v %v", l, err)
			}
		})
	}
}

func TestCursorCaptureRevalidatesIndexAndPolicy(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"index-path", "index-workspace", "index-secondary-workspace", "index-cwd", "index-removed", "policy", "policy-secondary-workspace", "policy-cwd", "ancestor-symlink", "append"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := cursorEnv(w)
			path := putCursor(t, w, "selected", defaultCwd, "opaque\n")
			meta := detect.CursorSession{
				ConversationID: "selected", TranscriptPath: path,
				WorkspaceRoots: []string{defaultCwd, filepath.Join(w.home, "second")},
				Cwd:            filepath.Join(w.home, "tool-cwd"),
			}
			writeCursorMetadata(t, e, meta)
			c := captureOne(t, w, e, "cursor:selected")
			if c.State != StateNew {
				t.Fatalf("selection: %+v", c)
			}
			before := w.opens[path]
			switch change {
			case "index-path":
				writeCursorIndex(t, e, "selected", path+".other", defaultCwd)
			case "index-workspace":
				writeCursorIndex(t, e, "selected", path, defaultCwd+"-other")
			case "index-secondary-workspace":
				meta.WorkspaceRoots[1] += "-changed"
				writeCursorMetadata(t, e, meta)
			case "index-cwd":
				meta.Cwd += "-changed"
				writeCursorMetadata(t, e, meta)
			case "index-removed":
				if err := os.Remove(detect.StatePath(e.CacheDir, HarnessCursor, "selected")); err != nil {
					t.Fatal(err)
				}
			case "policy":
				e.Policy.DenyPaths = []string{defaultCwd}
			case "policy-secondary-workspace":
				e.Policy.DenyPaths = []string{meta.WorkspaceRoots[1]}
			case "policy-cwd":
				e.Policy.DenyPaths = []string{meta.Cwd}
			case "ancestor-symlink":
				dir := filepath.Dir(path)
				if err := os.Rename(dir, dir+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+"-moved", dir); err != nil {
					t.Fatal(err)
				}
			case "append":
				w.appendTo(path, "more\n")
			}
			if raw, _, err := OpenCapture(w.ctx, e, c); raw != nil || err == nil {
				t.Fatalf("changed selection reopened: %v", err)
			}
			if w.opens[path] != before {
				t.Fatal("changed selection opened transcript")
			}
		})
	}
}

func TestCursorCaptureRejectsIndexMutationDuringOpenAndRead(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"open", "read"} {
		t.Run(when, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := cursorEnv(w)
			path := putCursor(t, w, "race", defaultCwd, "opaque\n")
			c := captureOne(t, w, e, "cursor:race")
			if when == "open" {
				e.Open = func(path string) (io.ReadCloser, error) {
					f, err := os.Open(path)
					writeCursorIndex(t, e, "race", path, defaultCwd+"-changed")
					return f, err
				}
				if raw, _, err := OpenCapture(w.ctx, e, c); raw != nil || err == nil {
					t.Fatalf("open mutation accepted: %v", err)
				}
				return
			}
			raw, _, err := OpenCapture(w.ctx, e, c)
			if err != nil {
				t.Fatal(err)
			}
			writeCursorIndex(t, e, "race", path, defaultCwd+"-changed")
			_, readErr := io.ReadAll(raw)
			closeErr := raw.Close()
			if readErr == nil || closeErr == nil {
				t.Fatalf("index mutation during stream accepted: read=%v close=%v", readErr, closeErr)
			}
		})
	}
}

func TestCursorDigestExplicitlyUnsupported(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := cursorEnv(w)
	path := putCursor(t, w, "storage-only", defaultCwd, fixture(t, "cursor-cli-transcript.jsonl", "storage-only", defaultCwd))
	for _, req := range []DigestRequest{{Refs: []string{"cursor:storage-only"}}, {Unprocessed: true, Harness: HarnessCursor}, {Refs: []string{"cursor:storage-only"}, AllowUnknownProject: true}} {
		out, err := Digest(w.ctx, w.db, e, req)
		if err != nil || len(out.Sessions) != 0 || len(out.Errors) != 1 || out.Errors[0].State != StateUnsupportedFormat || !strings.Contains(out.Errors[0].Message, "storage capture only") {
			t.Fatalf("storage-only digest: %+v %v", out, err)
		}
	}
	before := w.opens[path]
	_, err := Locate(w.ctx, e, "cursor:storage-only", "L0")
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.State != StateUnsupportedFormat || w.opens[path] != before || DetectorFor(HarnessCursor) != "" {
		t.Fatalf("storage-only span lookup: %v", err)
	}
	if err := w.db.Read(w.ctx, func(q store.Querier) error {
		rows, err := store.ListSessions(w.ctx, q, HarnessCursor)
		if len(rows) != 0 {
			t.Fatal("unsupported digest persisted a watermark")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCursorIndexOnlyDiscovery(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := cursorEnv(w)
	path := putCursor(t, w, "indexed", defaultCwd, "opaque\n")
	index := detect.StatePath(e.CacheDir, HarnessCursor, "indexed")
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}
	cs, problems, err := (cursorReader{}).candidates(e)
	if err != nil || len(cs) != 0 || len(problems) != 0 || w.opens[path] != 0 {
		t.Fatalf("unindexed transcript discovered: %+v %+v %v", cs, problems, err)
	}
	// Counter-only records, transient locks and temp files are not transcripts.
	for _, name := range []string{"counter.json", "entry.json.lock", "entry.json.tmp"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(index), name), []byte(`{"failures":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cs, problems, err = (cursorReader{}).candidates(e)
	if err != nil || len(cs) != 0 || len(problems) != 0 {
		t.Fatalf("counter-only record became transcript: %+v %+v %v", cs, problems, err)
	}
	// A malformed cache record is a problem, not an invented session id.
	if err := os.WriteFile(index, []byte(`{"cursor":{"conversation_id":"different","transcript_path":"`+path+`","workspace_root":"`+defaultCwd+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, problems, err = (cursorReader{}).candidates(e)
	if err != nil || len(cs) != 0 || len(problems) != 1 {
		t.Fatalf("invalid index accepted: %+v %+v %v", cs, problems, err)
	}
	for _, cache := range []string{"", "relative"} {
		e.CacheDir = cache
		if _, _, err := (cursorReader{}).candidates(e); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("relative/unset cache scanned: %v", err)
		}
	}
}
