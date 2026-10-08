package sessions

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// ocLoads counts the loads of each OpenCode database path.
var (
	ocLoadsMu sync.Mutex
	ocLoads   = map[string]int{}
)

func init() {
	opencodeLoadHook = func(c candidate) {
		ocLoadsMu.Lock()
		ocLoads[c.path]++
		ocLoadsMu.Unlock()
	}
}

func ocLoadCount(path string) int {
	ocLoadsMu.Lock()
	defer ocLoadsMu.Unlock()

	return ocLoads[path]
}

const ocSID = "ses_test0001"

// ocWorld is a world with an OpenCode data directory.
type ocWorld struct {
	*world
	data string
}

func newOCWorld(t *testing.T) *ocWorld {
	t.Helper()
	w := &ocWorld{world: newWorld(t)}
	w.data = filepath.Join(w.home, ".local", "share")
	if err := os.MkdirAll(filepath.Join(w.data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}

	return w
}

func (w *ocWorld) env() Env {
	e := w.world.env()
	e.DataHome = w.data

	return e
}

func (w *ocWorld) dbPath() string { return filepath.Join(w.data, "opencode", "opencode.db") }

// create executes the fixture store into a fresh WAL-mode database and
// closes it, so no -wal file remains.
func (w *ocWorld) create(cwd string) string {
	w.t.Helper()
	w.exec(fixture(w.t, filepath.Join("stores", "opencode", "opencode.sql"), ocSID, cwd))

	return w.dbPath()
}

// exec runs statements on a writer connection and closes it.
func (w *ocWorld) exec(stmts string) {
	w.t.Helper()
	db, err := sql.Open("sqlite", ocWriterDSN(w.dbPath()))
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		w.t.Fatal(err)
	}
	if _, err := db.Exec(stmts); err != nil {
		w.t.Fatal(err)
	}
}

// ocWriterDSN is a read-write DSN of the database at path.
func ocWriterDSN(path string) string {
	return "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
}

func (w *ocWorld) list() Listing {
	w.t.Helper()
	l, err := List(w.ctx, w.db, w.env(), ListOptions{Harness: HarnessOpenCode})
	if err != nil {
		w.t.Fatal(err)
	}

	return l
}

func (w *ocWorld) session(sid string) Session {
	w.t.Helper()
	for _, s := range w.list().Sessions {
		if s.SessionID == sid {
			return s
		}
	}
	w.t.Fatalf("session %s not listed", sid)

	return Session{}
}

func (w *ocWorld) digest(req DigestRequest) DigestOutput {
	w.t.Helper()
	out, err := Digest(w.ctx, w.db, w.env(), req)
	if err != nil {
		w.t.Fatal(err)
	}

	return out
}

func (w *ocWorld) digestOne(sid string) SessionDigest {
	w.t.Helper()
	out := w.digest(DigestRequest{Refs: []string{Ref(HarnessOpenCode, sid)}})
	if len(out.Sessions) != 1 {
		w.t.Fatalf("digest of %s: %+v", sid, out)
	}

	return out.Sessions[0]
}

func (w *ocWorld) mark(sid string) {
	w.t.Helper()
	if err := Mark(w.ctx, w.db, w.env(), []string{Ref(HarnessOpenCode, sid)}, OutcomeNothing, nil); err != nil {
		w.t.Fatal(err)
	}
}

// watermarkSize is the size the session's latest digest recorded.
func (w *ocWorld) watermarkSize(sid string) int64 {
	w.t.Helper()
	var row store.SessionSeen
	if err := w.db.Read(w.ctx, func(q store.Querier) (err error) {
		row, _, err = store.GetSession(w.ctx, q, HarnessOpenCode, sid)
		return err
	}); err != nil {
		w.t.Fatal(err)
	}

	return row.Size
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}

	return out
}

// ocAppend is a user prompt and a completed assistant message with a failed
// tool call, after the fixture's last message.
const ocAppend = `
INSERT INTO message VALUES ('msg_0009', '` + ocSID + `', 1791442870000, 1791442870000, '{"role":"user","time":{"created":1791442870000}}');
INSERT INTO part VALUES ('prt_0016', 'msg_0009', '` + ocSID + `', 1791442870000, 1791442870000, '{"type":"text","text":"Why did you stop?"}');
INSERT INTO message VALUES ('msg_0010', '` + ocSID + `', 1791442871000, 1791442875000, '{"role":"assistant","time":{"created":1791442871000,"completed":1791442875000},"modelID":"test-model-3","path":{"cwd":"` + defaultCwd + `"}}');
INSERT INTO part VALUES ('prt_0017', 'msg_0010', '` + ocSID + `', 1791442872000, 1791442873000, '{"type":"tool","callID":"call_0007","tool":"bash","state":{"status":"error","input":{"command":"make lint"},"error":"lint failed","time":{"start":1791442872000,"end":1791442873000}}}');
`

func TestOpenCodeStates(t *testing.T) {
	t.Parallel()

	t.Run("new", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		l := w.list()
		if len(l.Stores) != 1 || l.Stores[0].State != StorePresent || l.Stores[0].Location != filepath.Join(w.data, "opencode") {
			t.Fatalf("%+v", l.Stores)
		}
		if len(l.Sessions) != 1 {
			t.Fatalf("child session listed: %+v", l.Sessions)
		}
		s := l.Sessions[0]
		want := Counts{Entries: 21, Prompts: 4, ToolCalls: 9, ToolErrors: 5, Interrupts: 1, Denials: 1}
		if s.State != StateNew || s.SessionID != ocSID || s.Project != defaultCwd || s.Path != w.dbPath() || s.Counts == nil || *s.Counts != want {
			t.Fatalf("%+v %+v", s, s.Counts)
		}
	})
	t.Run("processed", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		w.digestOne(ocSID)
		w.mark(ocSID)
		if s := w.session(ocSID); s.State != StateProcessed {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("appended", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		first := w.digestOne(ocSID)
		w.mark(ocSID)
		size := w.watermarkSize(ocSID)
		w.exec(ocAppend)
		if s := w.session(ocSID); s.State != StateChanged || s.Reason != ChangeAppended {
			t.Fatalf("%+v", s)
		}
		out := w.digest(DigestRequest{Unprocessed: true})
		if len(out.Sessions) != 1 {
			t.Fatalf("%+v", out)
		}
		d := out.Sessions[0]
		if d.FromOffset != size || strings.Join(spans(d), ",") != "msg_0009,prt_0017" || d.Counts.Entries != 3 || d.Model != "test-model-3" {
			t.Fatalf("%+v", d)
		}
		if len(first.Events) == 0 || slices.Contains(spans(first), "msg_0009") {
			t.Fatalf("first %+v", first)
		}
	})
	t.Run("running tool part", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		w.digestOne(ocSID)
		w.mark(ocSID)
		// The call starts before the mark and its result lands after it.
		w.exec(`INSERT INTO message VALUES ('msg_0009', '` + ocSID + `', 1791442870000, 1791442880000, '{"role":"assistant","time":{"created":1791442870000,"completed":1791442880000},"modelID":"m"}');
INSERT INTO part VALUES ('prt_0016', 'msg_0009', '` + ocSID + `', 1791442871000, 1791442871000, '{"type":"tool","callID":"c9","tool":"bash","state":{"status":"running","input":{"command":"sleep 9"},"time":{"start":1791442871000}}}');
INSERT INTO part VALUES ('prt_0017', 'msg_0009', '` + ocSID + `', 1791442872000, 1791442872000, '{"type":"text","text":"waiting"}');`)
		s := w.session(ocSID)
		if s.State != StateChanged || s.Reason != ChangeAppended {
			t.Fatalf("%+v", s)
		}
		d := w.digest(DigestRequest{Unprocessed: true}).Sessions[0]
		if len(d.Events) != 0 || d.Counts.Entries != 1 || d.Counts.ToolCalls != 0 {
			t.Fatalf("running part counted: %+v", d)
		}
		w.mark(ocSID)
		w.exec(`UPDATE part SET data = '{"type":"tool","callID":"c9","tool":"bash","state":{"status":"error","input":{"command":"sleep 9"},"error":"killed","time":{"start":1791442871000,"end":1791442879000}}}' WHERE id = 'prt_0016';`)
		d = w.digest(DigestRequest{Unprocessed: true}).Sessions[0]
		if strings.Join(spans(d), ",") != "prt_0016" || d.Events[0].Status != statusError || d.Counts.Entries != 2 {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("incomplete assistant message", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		before := w.session(ocSID).Counts.Entries
		w.exec(`INSERT INTO message VALUES ('msg_0009', '` + ocSID + `', 1791442870000, 1791442870000, '{"role":"assistant","time":{"created":1791442870000},"modelID":"m"}');
INSERT INTO message VALUES ('msg_0010', '` + ocSID + `', 1791442871000, 1791442871000, '{"role":"user","time":{"created":1791442871000}}');`)
		if got := w.session(ocSID).Counts.Entries; got != before {
			t.Fatalf("entries %d, want %d", got, before)
		}
	})
	t.Run("truncated by a revert", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		w.digestOne(ocSID)
		w.mark(ocSID)
		w.exec(`DELETE FROM part WHERE message_id = 'msg_0008'; DELETE FROM message WHERE id = 'msg_0008';`)
		if s := w.session(ocSID); s.State != StateChanged || s.Reason != ChangeTruncated {
			t.Fatalf("%+v", s)
		}
		if d := w.digestOne(ocSID); d.FromOffset != 0 || d.Events[0].Span != "msg_0001" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("rotated", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		w.digestOne(ocSID)
		w.mark(ocSID)
		w.exec(`DELETE FROM part WHERE message_id = 'msg_0001'; DELETE FROM message WHERE id = 'msg_0001';`)
		if s := w.session(ocSID); s.State != StateChanged || s.Reason != ChangeRotated {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("unsupported store", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		w.exec("DROP TABLE part;")
		l := w.list()
		if len(l.Stores) != 1 || l.Stores[0].State != StateUnsupportedFormat || len(l.Sessions) != 0 {
			t.Fatalf("%+v", l)
		}
		st, err := Status(w.ctx, w.db, w.env())
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(st.Harnesses, func(h HarnessStatus) bool { return h.Harness == HarnessOpenCode })
		if i < 0 || st.Harnesses[i].Store != StateUnsupportedFormat {
			t.Fatalf("%+v", st)
		}
	})
	t.Run("absent store", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		if l := w.list(); len(l.Stores) != 1 || l.Stores[0].State != StateAbsent {
			t.Fatalf("%+v", l.Stores)
		}
	})
	t.Run("denied before the read", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		p := w.create(defaultCwd)
		w.pol.DenyPaths = []string{defaultCwd}
		if s := w.session(ocSID); s.State != StateDenied || s.Reason != collect.ReasonDenyPaths || s.Counts != nil {
			t.Fatalf("%+v", s)
		}
		out := w.digest(DigestRequest{Refs: []string{Ref(HarnessOpenCode, ocSID)}})
		if len(out.Errors) != 1 || out.Errors[0].State != StateDenied {
			t.Fatalf("%+v", out)
		}
		if n := ocLoadCount(p); n != 0 {
			t.Fatalf("denied session loaded %d times", n)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		p := w.create(defaultCwd)
		w.pol.Disabled = true
		if s := w.session(ocSID); s.State != StateDisabled || ocLoadCount(p) != 0 {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("unparsable data", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		w.exec(`INSERT INTO message VALUES ('msg_0009', '` + ocSID + `', 1791442870000, 1791442870000, 'not json');
INSERT INTO part VALUES ('prt_0016', 'msg_0009', '` + ocSID + `', 1791442870000, 1791442870000, '{"type":"text","text":"x","futurePart":[1,2]}');`)
		s := w.session(ocSID)
		if s.State != StateNew || s.Counts.Unparsed != 1 || s.Counts.Entries != 22 {
			t.Fatalf("%+v %+v", s, s.Counts)
		}
	})
	t.Run("opencode db", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		p := w.create(defaultCwd)
		other := filepath.Join(w.data, "opencode", "opencode-dev.db")
		if err := os.Rename(p, other); err != nil {
			t.Fatal(err)
		}
		env := w.env()
		env.OpenCodeDB = "opencode-dev.db"
		r, _ := readerFor(HarnessOpenCode)
		if r.location(env) != other {
			t.Fatalf("location %s", r.location(env))
		}
		cands, _, err := r.candidates(env)
		if err != nil || len(cands) != 1 || cands[0].path != other {
			t.Fatalf("%+v %v", cands, err)
		}
		env.OpenCodeDB = filepath.Join(w.data, "missing.db")
		if _, _, err := r.candidates(env); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("same session in two databases", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		p := w.create(defaultCwd)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		second := filepath.Join(w.data, "opencode", "opencode-copy.db")
		if err := os.WriteFile(second, b, 0o644); err != nil {
			t.Fatal(err)
		}
		// A -wal file name is not a database.
		if err := os.WriteFile(second+"-wal", nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(second + "-wal"); err != nil {
			t.Fatal(err)
		}
		l := w.list()
		if len(l.Sessions) != 1 || l.Sessions[0].Path != p || len(l.Stores[0].Problems) != 1 || !strings.HasPrefix(l.Stores[0].Problems[0], second+": ") {
			t.Fatalf("%+v", l)
		}
	})
}

// dirSizes lists the names and sizes of the files in dir.
func dirSizes(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s:%d", e.Name(), info.Size()))
	}

	return out
}

// TestOpenCodeReadOnly: reading never adds or changes a file next to the
// database, and rows only in the write-ahead log are not seen.
func TestOpenCodeReadOnly(t *testing.T) {
	t.Parallel()

	t.Run("no wal", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		dir := filepath.Join(w.data, "opencode")
		if names := dirNames(t, dir); !slices.Equal(names, []string{"opencode.db"}) {
			t.Fatalf("fixture left %v", names)
		}
		before := dirSizes(t, dir)
		w.list()
		w.digestOne(ocSID)
		if _, err := Locate(w.ctx, w.env(), Ref(HarnessOpenCode, ocSID), "prt_0009"); err != nil {
			t.Fatal(err)
		}
		if after := dirSizes(t, dir); !slices.Equal(after, before) {
			t.Fatalf("files %v, before %v", after, before)
		}
	})
	t.Run("live writer with uncheckpointed rows", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		writer, err := sql.Open("sqlite", ocWriterDSN(w.dbPath()))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = writer.Close() }()
		writer.SetMaxOpenConns(1)
		if _, err := writer.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Exec(ocAppend); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(w.data, "opencode")
		if _, err := os.Stat(w.dbPath() + "-wal"); err != nil {
			t.Fatalf("no -wal file: %v", err)
		}
		before := dirSizes(t, dir)
		s := w.session(ocSID)
		if s.State != StateNew || s.Counts.Prompts != 4 || s.Counts.Entries != 21 {
			t.Fatalf("%+v %+v", s, s.Counts)
		}
		if d := w.digestOne(ocSID); slices.Contains(spans(d), "msg_0009") {
			t.Fatalf("uncheckpointed row seen: %+v", d)
		}
		if after := dirSizes(t, dir); !slices.Equal(after, before) {
			t.Fatalf("files %v, before %v", after, before)
		}
	})
	t.Run("path needing escapes", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.data = filepath.Join(w.home, "da ta?#%")
		if err := os.MkdirAll(filepath.Join(w.data, "opencode"), 0o755); err != nil {
			t.Fatal(err)
		}
		w.create(defaultCwd)
		if s := w.session(ocSID); s.State != StateNew {
			t.Fatalf("%+v", s)
		}
		if names := dirNames(t, filepath.Join(w.data, "opencode")); !slices.Equal(names, []string{"opencode.db"}) {
			t.Fatalf("%v", names)
		}
	})
}

// grownInfo is a FileInfo whose size is larger by n.
type grownInfo struct {
	os.FileInfo
	n int64
}

func (g grownInfo) Size() int64 { return g.FileInfo.Size() + g.n }

// TestOpenCodeReadRetry replaces the package's stat hook, so it does not
// run in parallel.
func TestOpenCodeReadRetry(t *testing.T) {
	w := newOCWorld(t)
	p := w.create(defaultCwd)
	defer func() { opencodeStat = os.Stat }()
	// run reads with the database file reported grown after each of the
	// first changes reads.
	run := func(changes int) (runs int, err error) {
		calls, grown := 0, int64(0)
		opencodeStat = func(name string) (os.FileInfo, error) {
			info, err := os.Stat(name)
			if err != nil || name != p {
				return info, err
			}
			calls++
			// The second stat of the database in an attempt is the one after
			// the read.
			if calls%2 == 0 && calls/2 <= changes {
				grown++
			}

			return grownInfo{info, grown}, nil
		}
		err = readOpenCode(p, func(*sql.DB) error {
			runs++

			return nil
		})

		return runs, err
	}
	if runs, err := run(0); err != nil || runs != 1 {
		t.Fatalf("unchanged: %d runs, %v", runs, err)
	}
	if runs, err := run(1); err != nil || runs != 2 {
		t.Fatalf("changed once: %d runs, %v", runs, err)
	}
	runs, err := run(2)
	if runs != 2 || err == nil || err.Error() != "the database changed during the read" {
		t.Fatalf("changed twice: %d runs, %v", runs, err)
	}
	// A -wal stat error other than not-exist is a change too.
	opencodeStat = func(name string) (os.FileInfo, error) {
		if name == p+"-wal" {
			return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrPermission}
		}

		return os.Stat(name)
	}
	if err := readOpenCode(p, func(*sql.DB) error { return nil }); err == nil {
		t.Fatal("wal stat error ignored")
	}
	opencodeStat = os.Stat
	r, _ := readerFor(HarnessOpenCode)
	cands, _, err := r.candidates(w.env())
	if err != nil || len(cands) != 1 {
		t.Fatalf("%+v %v", cands, err)
	}
	calls := 0
	opencodeStat = func(name string) (os.FileInfo, error) {
		info, err := os.Stat(name)
		if err != nil || name != p {
			return info, err
		}
		calls++

		return grownInfo{info, int64(calls)}, nil
	}
	if ev := read(w.env(), r, cands[0], nil, false); ev.state != StateUnreadable || ev.reason != "the database changed during the read" {
		t.Fatalf("%s %s", ev.state, ev.reason)
	}
}

func TestOpenCodeRewrites(t *testing.T) {
	t.Parallel()

	t.Run("summary and compaction keep the session processed", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		w.digestOne(ocSID)
		w.mark(ocSID)
		size := w.watermarkSize(ocSID)
		w.exec(`UPDATE message SET data = json_set(data, '$.summary', json('{"diffs":[{"file":"a.go","additions":3}]}')) WHERE id = 'msg_0001';
UPDATE part SET data = json_set(data, '$.state.time.compacted', 1791442880000) WHERE id = 'prt_0006';`)
		if s := w.session(ocSID); s.State != StateProcessed {
			t.Fatalf("%+v", s)
		}
		w.exec(ocAppend)
		d := w.digest(DigestRequest{Unprocessed: true}).Sessions[0]
		if d.FromOffset != size || strings.Join(spans(d), ",") != "msg_0009,prt_0017" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("user message before its part", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		w.exec(`INSERT INTO message VALUES ('msg_0009', '` + ocSID + `', 1791442870000, 1791442870000, '{"role":"user","time":{"created":1791442870000}}');`)
		w.digestOne(ocSID)
		w.mark(ocSID)
		w.exec(`INSERT INTO part VALUES ('prt_0016', 'msg_0009', '` + ocSID + `', 1791442870000, 1791442870000, '{"type":"text","text":"Why did you stop?"}');`)
		d := w.digest(DigestRequest{Unprocessed: true}).Sessions[0]
		if len(d.Events) != 1 || d.Events[0].Span != "msg_0009" || d.Events[0].Type != "prompt" || d.Events[0].Summary != "Why did you stop?" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("directory changed since listed", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		w.create(defaultCwd)
		r, _ := readerFor(HarnessOpenCode)
		cands, _, err := r.candidates(w.env())
		if err != nil || len(cands) != 1 {
			t.Fatalf("%+v %v", cands, err)
		}
		w.exec(`UPDATE session SET directory = '/elsewhere' WHERE id = '` + ocSID + `';`)
		before := ocLoadCount(cands[0].path)
		ev := read(w.env(), r, cands[0], nil, false)
		if ev.state != StateUnreadable || ev.reason != "the session's directory changed since it was listed" || ev.tr != nil {
			t.Fatalf("%s %s", ev.state, ev.reason)
		}
		if ocLoadCount(cands[0].path) != before+1 {
			t.Fatal("not loaded")
		}
	})
}

func TestOpenCodeDigest(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.create(defaultCwd)
	d := w.digestOne(ocSID)
	var got []string
	for _, e := range d.Events {
		s := e.Span + ":" + e.Type
		if e.Type == "tool_call" {
			s += ":" + e.Status
		}
		if e.Self {
			s += ":self"
		}
		got = append(got, s)
	}
	want := "msg_0001:prompt,prt_0006:tool_call:error,msg_0003:prompt,prt_0009:tool_call:error,prt_0010:tool_call:denied," +
		"prt_0011:tool_call:error:self,prt_0011b:tool_call:error,prt_0011c:tool_call:error,msg_0005:prompt,prt_0013:tool_call:interrupted,msg_0007:prompt"
	if strings.Join(got, ",") != want {
		t.Fatalf("events %s", strings.Join(got, ","))
	}
	if len(d.Retries) != 1 || d.Retries[0].Span != "prt_0009" || d.Retries[0].OfSpan != "prt_0006" {
		t.Fatalf("retries %+v", d.Retries)
	}
	if len(d.Flagged) != 2 || d.Flagged[0].Span != "msg_0003" {
		t.Fatalf("flagged %+v", d.Flagged)
	}
	if !slices.Equal(d.Denials, []string{"prt_0010"}) {
		t.Fatalf("%+v", d)
	}
	// The shell's exit code is native; a timeout and another tool's error
	// carry none.
	for i, want := range map[int]string{1: "exit:1", 3: "exit:1", 5: "exit:1", 6: "tool_error", 7: "tool_error"} {
		e := d.Events[i]
		got := e.ErrorClass
		if e.ExitCode != nil {
			got += fmt.Sprintf(":%d", *e.ExitCode)
		}
		if got != want {
			t.Errorf("%s: %s, want %s", e.Span, got, want)
		}
	}
	if d.FinalExcerpt != "TestParse still fails on empty input; the linter run was stopped." || d.Model != "test-model-2" || d.Cwd != defaultCwd {
		t.Fatalf("%+v", d)
	}
	if d.Start != "2026-10-08T07:00:01.000Z" || d.End != "2026-10-08T07:00:46.000Z" {
		t.Fatalf("start %s end %s", d.Start, d.End)
	}
}

func TestOpenCodeToolStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status, err, output, exit string
		interrupted               bool
		want                      string
		code                      int // -1 for none
	}{
		{"completed", "", "ok", "", false, statusOK, -1},
		{"completed", "", "ok", "0", false, statusOK, -1},
		{"completed", "", "FAIL", "1", false, statusError, 1},
		{"completed", "", "x", "137", false, statusError, 137},
		{"completed", "", "x", "1.0", false, statusError, 1},
		{"completed", "", "x", `"1"`, false, statusOK, -1},
		{"completed", "", "x\n\n<shell_metadata>\nUser aborted the command\n</shell_metadata>", "null", false, statusInterrupted, -1},
		{"completed", "", "x\n\n<shell_metadata>\nshell tool terminated command after exceeding timeout 5000 ms.\n</shell_metadata>", "null", false, statusError, -1},
		{"completed", "", "User aborted the command", "null", false, statusOK, -1},
		{"error", "Tool execution aborted", "", "", true, statusInterrupted, -1},
		{"error", "Tool execution aborted", "", "", false, statusInterrupted, -1},
		{"error", "boom", "", "", true, statusInterrupted, -1},
		{"error", "The user rejected permission to use this specific tool call.", "", "", false, statusDenied, -1},
		{"error", "The user rejected permission to use this specific tool call with the following feedback: no", "", "", false, statusDenied, -1},
		{"error", "The user has specified a rule which prevents you from using this specific tool call. Ask them.", "", "", false, statusDenied, -1},
		{"error", "The user dismissed this question", "", "", false, statusDenied, -1},
		{"error", "exit status 2", "", "", false, statusError, -1},
	} {
		var p ocPart
		p.Type, p.State.Status, p.State.Error, p.State.Output, p.State.Metadata.Interrupted = "tool", tc.status, tc.err, tc.output, tc.interrupted
		if tc.exit != "" {
			p.State.Metadata.Exit = json.RawMessage(tc.exit)
		}
		got, code := toolStatus(&p)
		gotCode := -1
		if code != nil {
			gotCode = *code
		}
		if got != tc.want || gotCode != tc.code {
			t.Errorf("%s %q exit %s: %s %d, want %s %d", tc.status, tc.err+tc.output, tc.exit, got, gotCode, tc.want, tc.code)
		}
	}
}

func TestOpenCodeLocate(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.create(defaultCwd)
	f, err := Locate(w.ctx, w.env(), Ref(HarnessOpenCode, ocSID), "prt_0009")
	if err != nil {
		t.Fatal(err)
	}
	if f.Model != "test-model-2" || f.Cwd != defaultCwd || f.At.Format(timeLayout) != "2026-10-08T07:00:17.000Z" || f.Detector != "opencode-sqlite/1" {
		t.Fatalf("%+v", f)
	}
	f, err = Locate(w.ctx, w.env(), Ref(HarnessOpenCode, ocSID), "msg_0001")
	if err != nil || f.Model != "" || f.Cwd != defaultCwd || f.At.Format(timeLayout) != "2026-10-08T07:00:01.000Z" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := Locate(w.ctx, w.env(), Ref(HarnessOpenCode, "ses_child0001"), "msg_c001"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("child session: %v", err)
	}
}

// TestStoreErrorRefusesRefs: a store whose candidates fail other than by
// being absent refuses every ref of its harness.
func TestStoreErrorRefusesRefs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, state string
		spoil       func(w *ocWorld)
	}{
		{"unsupported", StateUnsupportedFormat, func(w *ocWorld) { w.exec("DROP TABLE part;") }},
		{"unreadable", StateUnreadable, func(w *ocWorld) {
			if err := os.WriteFile(w.dbPath(), []byte(strings.Repeat("not a database ", 100)), 0o644); err != nil {
				w.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newOCWorld(t)
			w.create(defaultCwd)
			tc.spoil(w)
			ref := Ref(HarnessOpenCode, ocSID)
			out := w.digest(DigestRequest{Refs: []string{ref, Ref(HarnessOpenCode, "ses_other")}})
			if len(out.Sessions) != 0 || len(out.Errors) != 2 {
				t.Fatalf("%+v", out)
			}
			for _, e := range out.Errors {
				if e.State != tc.state || !strings.Contains(e.Message, "session is "+tc.state+" (") {
					t.Fatalf("%+v", e)
				}
			}
			var refused *RefusedError
			if _, err := Locate(w.ctx, w.env(), ref, "prt_0009"); !errors.As(err, &refused) || refused.State != tc.state || refused.Reason == "" {
				t.Fatalf("got %v", err)
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		w := newOCWorld(t)
		out := w.digest(DigestRequest{Refs: []string{Ref(HarnessOpenCode, ocSID)}})
		if len(out.Errors) != 1 || out.Errors[0].State != StateAbsent {
			t.Fatalf("%+v", out)
		}
		if _, err := Locate(w.ctx, w.env(), Ref(HarnessOpenCode, ocSID), "x"); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("got %v", err)
		}
	})
}
