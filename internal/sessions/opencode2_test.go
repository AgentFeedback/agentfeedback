package sessions

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// createV2 executes the OpenCode 2 fixture store (a migrated V1 database:
// V1's tables and session_v2 with the same session id).
func (w *ocWorld) createV2(cwd string) {
	w.t.Helper()
	w.exec(fixture(w.t, filepath.Join("stores", "opencode-v2", "opencode.sql"), ocSID, cwd))
}

func (w *ocWorld) listHarness(h string) Listing {
	w.t.Helper()
	l, err := List(w.ctx, w.db, w.env(), ListOptions{Harness: h})
	if err != nil {
		w.t.Fatal(err)
	}

	return l
}

func (w *ocWorld) v2Counts() Counts {
	w.t.Helper()
	l := w.listHarness(HarnessOpenCodeV2)
	if len(l.Sessions) != 1 || l.Sessions[0].SessionID != ocSID {
		w.t.Fatalf("opencode-v2 listing: %+v", l)
	}

	return *l.Sessions[0].Counts
}

var ocV2FixtureCounts = Counts{Entries: 13, Prompts: 2, ToolCalls: 1, ToolErrors: 1, Interrupts: 1}

func TestOpenCodeV2List(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.createV2(defaultCwd)
	l := w.listHarness(HarnessOpenCodeV2)
	if len(l.Sessions) != 1 {
		t.Fatalf("child session listed or root missing: %+v", l.Sessions)
	}
	s := l.Sessions[0]
	if s.SessionID != ocSID || s.Project != defaultCwd || *s.Counts != ocV2FixtureCounts {
		t.Fatalf("%+v", s)
	}
	if !s.Start.Equal(time.UnixMilli(1794220000500)) || !s.End.Equal(time.UnixMilli(1794220051000)) {
		t.Fatalf("start %v end %v", s.Start, s.End)
	}
	// The migrated database also has V1's tables with the same session id:
	// the V1 reader skips it, without a problem.
	v1 := w.listHarness(HarnessOpenCode)
	if len(v1.Sessions) != 0 || len(v1.Stores) != 1 || len(v1.Stores[0].Problems) != 0 {
		t.Fatalf("V1 listed a database with session_v2: %+v", v1)
	}
}

func TestOpenCodeV2SkipsV1Database(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.create(defaultCwd)
	v2 := w.listHarness(HarnessOpenCodeV2)
	if len(v2.Sessions) != 0 || len(v2.Stores) != 1 || len(v2.Stores[0].Problems) != 0 {
		t.Fatalf("V2 listed a database without session_v2: %+v", v2)
	}
	if v1 := w.listHarness(HarnessOpenCode); len(v1.Sessions) != 1 {
		t.Fatalf("V1 listing: %+v", v1)
	}
}

func TestOpenCodeV2Unsupported(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.exec("CREATE TABLE session_v2 (id text PRIMARY KEY, parent_id text, directory text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);" +
		"CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, time_created integer NOT NULL, data text NOT NULL);")
	_, problems, err := opencodeV2Reader{}.candidates(w.env())
	if !errors.Is(err, errUnsupported) || len(problems) != 1 {
		t.Fatalf("%v %+v", err, problems)
	}
}

func TestOpenCodeV2IncompleteCut(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.createV2(defaultCwd)
	w.exec(`INSERT INTO session_message VALUES ('msg_0009', '` + ocSID + `', 'assistant', 9, 1794220060000, 1794220060000, '{"time":{"created":1794220060000},"model":{"id":"m"},"content":[]}');
INSERT INTO session_message VALUES ('msg_0010', '` + ocSID + `', 'user', 10, 1794220061000, 1794220061000, '{"time":{"created":1794220061000},"text":"later"}');`)
	if c := w.v2Counts(); c != ocV2FixtureCounts {
		t.Fatalf("assistant without time.completed counted: %+v", c)
	}
	w.exec(`UPDATE session_message SET data = '{"time":{"created":1794220060000,"completed":1794220062000},"model":{"id":"m"},"content":[{"type":"tool","id":"c9","name":"shell","state":{"status":"running","input":{"command":"sleep 9"},"metadata":{}}}]}' WHERE id = 'msg_0009';`)
	if c := w.v2Counts(); c != ocV2FixtureCounts {
		t.Fatalf("running tool counted: %+v", c)
	}
	w.exec(`UPDATE session_message SET data = '{"time":{"created":1794220060000,"completed":1794220062000},"model":{"id":"m"},"content":[{"type":"tool","id":"c9","name":"shell","state":{"status":"error","input":{"command":"sleep 9"},"error":{"type":"unknown","message":"failed"}}}]}' WHERE id = 'msg_0009';`)
	want := ocV2FixtureCounts
	want.Entries += 3
	want.Prompts++
	want.ToolCalls++
	want.ToolErrors++
	if c := w.v2Counts(); c != want {
		t.Fatalf("complete rows: %+v", c)
	}
}

func TestOpenCodeV2Parts(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.createV2(defaultCwd)
	w.exec(`INSERT INTO session_part VALUES ('msg_0003', '` + ocSID + `', 0, 'tool', 'c8', 1794220012000, 1794220012000, '{"type":"tool","id":"c8","name":"shell","state":{"status":"completed","input":{"command":"false"},"content":[{"type":"text","text":"x"}],"metadata":{"exit":1}}}');`)
	// The part replaces msg_0003's embedded text item: one entry each.
	want := ocV2FixtureCounts
	want.ToolCalls++
	want.ToolErrors++
	if c := w.v2Counts(); c != want {
		t.Fatalf("part not read: %+v", c)
	}
	// A streaming part cuts the log before it.
	w.exec(`INSERT INTO session_part VALUES ('msg_0003', '` + ocSID + `', 1, 'tool', 'c9', 1794220013000, 1794220013000, '{"type":"tool","id":"c9","name":"shell","state":{"status":"streaming","input":"{\"comm"}}');`)
	c := w.v2Counts()
	if c.Prompts != 1 || c.ToolCalls != 2 || c.Interrupts != 0 {
		t.Fatalf("streaming part not cut: %+v", c)
	}
}

func TestCaptureOpenCodeV2VirtualLog(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.createV2(defaultCwd)
	w.exec(`DELETE FROM session_message;
INSERT INTO session_message VALUES ('m1', '` + ocSID + `', 'user', 1, 1000, 1000, '{"time":{"created":1000},"text":"prompt","files":[]}');
INSERT INTO session_message VALUES ('m2', '` + ocSID + `', 'assistant', 2, 2000, 3000, '{"time":{"created":2000,"streamed":2500,"completed":3000},"model":{"id":"last-model","providerID":"p"},"content":[{"type":"text","text":"answer"}],"cost":99,"tokens":{"input":1}}');
INSERT INTO session_message VALUES ('m3', '` + ocSID + `', 'assistant', 3, 4000, 4000, '{"time":{"created":4000},"model":{"id":"incomplete"},"content":[]}');`)
	c := captureOne(t, w.world, w.env(), Ref(HarnessOpenCodeV2, ocSID))
	b, meta := readCapture(t, w.env(), c)
	want := `{"id":"m1","kind":"message","text":"prompt","time":{"created":1000},"type":"user"}
{"content":[{"text":"answer","type":"text"}],"id":"m2","kind":"message","model":{"id":"last-model","providerID":"p"},"time":{"completed":3000,"created":2000},"type":"assistant"}
`
	if string(b) != want || c.Size != int64(len(want)) {
		t.Fatalf("OpenCode 2 virtual bytes differ:\n%s", b)
	}
	if meta.Format != "opencode-virtual-log" || meta.LineCount != 2 || meta.Model != "last-model" || meta.Counts != (Counts{Entries: 3, Prompts: 1}) {
		t.Fatalf("incorrect OpenCode 2 capture metadata: %+v", meta)
	}
}

// A session moved out of a denied directory is denied: the policy sees the
// previous directory of the fixture's location-switched row.
func TestOpenCodeV2LocationSwitchDenied(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.createV2(defaultCwd)
	w.pol.DenyPaths = []string{defaultCwd + "/old"}
	l := w.listHarness(HarnessOpenCodeV2)
	if len(l.Sessions) != 1 || l.Sessions[0].State != StateDenied || l.Sessions[0].Reason != collect.ReasonDenyPaths {
		t.Fatalf("%+v", l.Sessions)
	}
}

func TestCaptureOpenCodeV2OmitsFileData(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.createV2(defaultCwd)
	payload := "aGVsbG8gd29ybGQgcGF5bG9hZA=="
	w.exec(`DELETE FROM session_message;
INSERT INTO session_message VALUES ('m1', '` + ocSID + `', 'assistant', 1, 1000, 2000, '{"time":{"created":1000,"completed":2000},"model":{"id":"m"},"content":[{"type":"tool","id":"c1","name":"read","state":{"status":"completed","input":{"path":"a.png"},"content":[{"type":"text","text":"read"},{"type":"file","uri":"data:image/png;base64,` + payload + `","mime":"image/png","name":"a.png","extra":1}],"metadata":{}}}]}');`)
	w.exec(`INSERT INTO session_part VALUES ('m1', '` + ocSID + `', 0, 'tool', 'c2', 1500, 1500, '{"type":"tool","id":"c2","name":"read","state":{"status":"completed","input":{},"content":[{"type":"file","uri":"data:image/png;base64,` + payload + `","mime":"image/png"},{"type":"file","uri":"file:///x.png","mime":"image/png"}],"metadata":{}}}');`)
	c := captureOne(t, w.world, w.env(), Ref(HarnessOpenCodeV2, ocSID))
	b, _ := readCapture(t, w.env(), c)
	if strings.Contains(string(b), payload) || strings.Contains(string(b), `"extra"`) {
		t.Fatalf("file data in the log:\n%s", b)
	}
	for _, want := range []string{
		`{"text":"read","type":"text"},{"mime":"image/png","name":"a.png","type":"file","uri":"data:image/png;base64,"}`,
		`{"mime":"image/png","type":"file","uri":"file:///x.png"}`,
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s in:\n%s", want, b)
		}
	}
}

// A V2 database whose stamp changes on every read is still skipped by the
// V1 reader without a problem.
func TestOpenCodeV1SkipsChangingV2Database(t *testing.T) {
	w := newOCWorld(t)
	w.createV2(defaultCwd)
	p := w.dbPath()
	defer func() { opencodeStat = os.Stat }()
	grown := int64(0)
	opencodeStat = func(name string) (os.FileInfo, error) {
		info, err := os.Stat(name)
		if err != nil || name != p {
			return info, err
		}
		grown++
		return grownInfo{info, grown}, nil
	}
	_, problems, err := opencodeReader{}.candidates(w.env())
	if !errors.Is(err, fs.ErrNotExist) || len(problems) != 0 {
		t.Fatalf("%v %+v", err, problems)
	}
}

// An assistant row that does not decode as a message counts as complete
// with no content; later rows are still read.
func TestOpenCodeV2UndecodableAssistant(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.createV2(defaultCwd)
	w.exec(`INSERT INTO session_message VALUES ('msg_0009', '` + ocSID + `', 'assistant', 9, 1794220060000, 1794220060000, '{"time":"soon","content":"x"}');
INSERT INTO session_message VALUES ('msg_0010', '` + ocSID + `', 'user', 10, 1794220061000, 1794220061000, '{"time":{"created":1794220061000},"text":"later"}');`)
	want := ocV2FixtureCounts
	want.Entries += 2
	want.Prompts++
	if c := w.v2Counts(); c != want {
		t.Fatalf("%+v", c)
	}
}

// A message with both embedded content and part rows counts its tool call
// once, from the part rows.
func TestOpenCodeV2PartsReplaceContent(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.createV2(defaultCwd)
	w.exec(`INSERT INTO session_part VALUES ('msg_0002', '` + ocSID + `', 0, 'reasoning', NULL, 1794220003000, 1794220003000, '{"type":"reasoning","text":"Run the build first."}');
INSERT INTO session_part VALUES ('msg_0002', '` + ocSID + `', 1, 'tool', 'call_0001', 1794220004000, 1794220009000, '{"type":"tool","id":"call_0001","name":"shell","state":{"status":"completed","input":{"command":"make build"},"content":[{"type":"text","text":"Exited with code 2"}],"metadata":{"exit":2}}}');`)
	if c := w.v2Counts(); c != ocV2FixtureCounts {
		t.Fatalf("tool call counted twice or lost: %+v", c)
	}
	c := captureOne(t, w.world, w.env(), Ref(HarnessOpenCodeV2, ocSID))
	if _, meta := readCapture(t, w.env(), c); meta.Counts != ocV2FixtureCounts {
		t.Fatalf("%+v", meta.Counts)
	}
}

// A switch row after the point where the log stops still brings its
// previous directory to the policy.
func TestOpenCodeV2LocationSwitchAfterCut(t *testing.T) {
	t.Parallel()
	w := newOCWorld(t)
	w.createV2(defaultCwd)
	w.exec(`INSERT INTO session_message VALUES ('msg_0009', '` + ocSID + `', 'assistant', 9, 1794220060000, 1794220060000, '{"time":{"created":1794220060000},"model":{"id":"m"},"content":[]}');
INSERT INTO session_message VALUES ('msg_0010', '` + ocSID + `', 'location-switched', 10, 1794220061000, 1794220061000, '{"time":{"created":1794220061000},"location":{"directory":"` + defaultCwd + `"},"previous":{"location":{"directory":"/nonexistent-af/secret"}}}');`)
	w.pol.DenyPaths = []string{"/nonexistent-af/secret"}
	l := w.listHarness(HarnessOpenCodeV2)
	if len(l.Sessions) != 1 || l.Sessions[0].State != StateDenied || l.Sessions[0].Reason != collect.ReasonDenyPaths {
		t.Fatalf("%+v", l.Sessions)
	}
}

func TestOC2ContentMixed(t *testing.T) {
	in := `["plain","Data:image/png;base64,UEFZTE9BRA==",5,{"type":"text","text":"t","x":1},{"type":"file","uri":"DATA:image/png;base64,UEFZTE9BRA==","mime":"image/png"},{"type":"other","secret":1}]`
	want := `["plain","Data:image/png;base64,",5,{"text":"t","type":"text"},{"mime":"image/png","type":"file","uri":"data:image/png;base64,"},{"type":"other"}]`
	if got := string(oc2Content([]byte(in))); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if got := string(oc2Content([]byte(`"not an array"`))); got != `"not an array"` {
		t.Fatalf("non-array: %s", got)
	}
}
