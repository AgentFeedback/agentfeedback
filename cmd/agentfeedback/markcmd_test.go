package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestDoneAndUndo(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	l.seed(t, `{"kind":"friction","summary":"one"}`)
	l.seed(t, `{"kind":"friction","summary":"two"}`)

	r := runCLI(t, "", "done", "1", "2", "999", "--verdict", "fixed", "--resolution", "r", "--ref", "abc@1", "--processed-by", "me")
	if r.code != 1 || lastLine(r.stdout) != `{"processed":true,"verdict":"fixed","updated":[1,2],"unchanged":[],"not_found":[999]}` ||
		!strings.Contains(r.stderr, "marked 2, unchanged 0, not found 1") {
		t.Fatalf("done %+v", r)
	}
	reqs := l.requests()
	if !slices.Contains(reqs, "POST /api/v1/submissions/processed") || slices.ContainsFunc(reqs, func(s string) bool { return strings.HasPrefix(s, "PATCH") }) {
		t.Fatalf("requests %v", reqs)
	}
	rec := l.record(t, 2)
	if rec["verdict"] != "fixed" || rec["resolution"] != "r" || rec["ref"] != "abc@1" || rec["processed_by"] != "me" {
		t.Fatalf("record %v", rec)
	}

	r = runCLI(t, "", "done", "--verdict", "fixed", "1", "2")
	var out batchResult
	if r.code != 0 || json.Unmarshal([]byte(lastLine(r.stdout)), &out) != nil || len(out.Unchanged) != 2 ||
		!strings.Contains(r.stderr, "marked 0, unchanged 2, not found 0") {
		t.Fatalf("rerun %+v", r)
	}

	r = runCLI(t, "", "undo", "1")
	if r.code != 0 || lastLine(r.stdout) != `{"processed":false,"updated":[1],"unchanged":[],"not_found":[]}` {
		t.Fatalf("undo %+v", r)
	}
	if rec := l.record(t, 1); rec["processed_at"] != nil || rec["verdict"] != nil {
		t.Fatalf("undo left %v", rec)
	}

	for _, args := range [][]string{{"done", "1"}, {"done", "--verdict", "fixed"}, {"done", "x", "--verdict", "fixed"}, {"undo"}, {"undo", "-1"}} {
		if r := runCLI(t, "", args...); r.code != 2 {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

func TestRedact(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	l.seed(t, `{"kind":"friction","summary":"secret","context":{"cwd":"/x"},"payload":{"details":"secret"}}`)

	r := runCLI(t, "", "redact", "1")
	var tomb map[string]json.RawMessage
	if r.code != 0 || json.Unmarshal([]byte(lastLine(r.stdout)), &tomb) != nil || string(tomb["payload"]) != `{"redacted":true}` ||
		tomb["redacted_at"] == nil || tomb["summary"] != nil || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("redact %+v", r)
	}
	if r := runCLI(t, "", "redact", "999"); r.code != 1 || !strings.Contains(r.stderr, "submission 999 not found") {
		t.Fatalf("404 %+v", r)
	}
}

func TestRekind(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	_, uid := l.seed(t, `{"kind":"friction","summary":"wrong kind","machine":"m1","project":"p","context":{"cwd":"/x"},"payload":{"category":"documentation","details":"d"}}`)

	r := runCLI(t, "", "rekind", "1", "note")
	var o map[string]any
	if r.code != 0 || json.Unmarshal([]byte(lastLine(r.stdout)), &o) != nil || o["outcome"] != "submitted" || o["id"] != 2.0 ||
		o["key"] != "rekind-"+uid || o["kind"] != "note" {
		t.Fatalf("rekind %+v", r)
	}
	nr := l.record(t, 2)
	ctx, _ := nr["context"].(map[string]any)
	if nr["kind"] != "note" || nr["summary"] != "wrong kind" || nr["machine"] != "m1" || nr["project"] != "p" ||
		ctx["rekinded_from"] != uid || ctx["cwd"] != "/x" || fmt.Sprint(nr["payload"]) != "map[category:documentation details:d]" {
		t.Fatalf("new row %v", nr)
	}
	check := func() {
		t.Helper()
		orig := l.record(t, 1)
		if orig["verdict"] != "duplicate" || orig["ref"] != nr["uid"] || orig["resolution"] != "re-kinded as note, #2" {
			t.Fatalf("original %v", orig)
		}
	}
	check()

	r = runCLI(t, "", "rekind", "1", "note")
	if r.code != 0 || json.Unmarshal([]byte(lastLine(r.stdout)), &o) != nil || o["outcome"] != "duplicate" || o["id"] != 2.0 {
		t.Fatalf("second rekind %+v", r)
	}
	check()

	if r := runCLI(t, "", "rekind", "2", "Note"); r.code != 1 || !strings.Contains(r.stderr, "already of kind note") {
		t.Fatalf("same kind %+v", r)
	}
	l.call(t, http.MethodDelete, "/api/v1/submissions/1", "")
	if r := runCLI(t, "", "rekind", "1", "review"); r.code != 1 || !strings.Contains(r.stderr, "is redacted") {
		t.Fatalf("redacted %+v", r)
	}
	if r := runCLI(t, "", "rekind", "999", "review"); r.code != 1 || !strings.Contains(r.stderr, "submission 999 not found") {
		t.Fatalf("404 %+v", r)
	}
	for _, args := range [][]string{{"rekind", "1"}, {"rekind", "1", "  "}, {"rekind", "x", "note"}} {
		if r := runCLI(t, "", args...); r.code != 2 {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

func TestDone_GivenEmptyFieldsClear(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	l.seed(t, `{"kind":"friction","summary":"one"}`)
	if r := runCLI(t, "", "done", "1", "--verdict", "fixed", "--ref", "abc@1", "--processed-by", "me", "--resolution", "r"); r.code != 0 {
		t.Fatalf("done %+v", r)
	}
	if r := runCLI(t, "", "done", "1", "--verdict", "fixed", "--ref", "", "--processed-by", ""); r.code != 0 {
		t.Fatalf("clear %+v", r)
	}
	if rec := l.record(t, 1); rec["ref"] != nil || rec["processed_by"] != nil || rec["resolution"] != "r" {
		t.Fatalf("record %v", rec)
	}
	if r := runCLI(t, "", "done", "1", "--verdict", "fixed", "--resolution", ""); r.code != 1 || !strings.Contains(r.stderr, "resolution must not be blank") {
		t.Fatalf("blank resolution %+v", r)
	}
	if r := runCLI(t, "", "done", "1", "--verdict", ""); r.code != 2 {
		t.Fatalf("empty verdict %+v", r)
	}
}

func TestDone_BatchLimit(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	l.seed(t, `{"kind":"friction","summary":"one"}`)
	many := make([]string, 0, 501)
	for i := range 501 {
		many = append(many, fmt.Sprint(i+1))
	}
	before := len(l.requests())
	for _, args := range [][]string{append([]string{"done", "--verdict", "fixed"}, many...), append([]string{"undo"}, many...)} {
		if r := runCLI(t, "", args...); r.code != 2 || !strings.Contains(r.stderr, "at most 500") {
			t.Fatalf("%s: %+v", args[0], r)
		}
	}
	if len(l.requests()) != before {
		t.Fatalf("requests sent: %v", l.requests()[before:])
	}
	dups := append([]string{"done", "--verdict", "fixed"}, slices.Repeat([]string{"1"}, 600)...)
	if r := runCLI(t, "", dups...); r.code != 0 || !strings.Contains(r.stderr, "marked 1") {
		t.Fatalf("duplicates %+v", r)
	}
}

func TestDone_MalformedBatchResponse(t *testing.T) {
	isolate(t)
	for _, body := range []string{`{}`, `{"updated":[1],"unchanged":[1],"not_found":[]}`, `{"updated":[1,9],"unchanged":[],"not_found":[]}`} {
		stub(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
		if r := runCLI(t, "", "done", "1", "--verdict", "fixed"); r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "is not readable") {
			t.Fatalf("%s: %+v", body, r)
		}
	}
}

func TestRekind_Refusals(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	l.seed(t, `{"kind":"friction","summary":"marked"}`)
	if r := runCLI(t, "", "done", "1", "--verdict", "fixed"); r.code != 0 {
		t.Fatal(r)
	}
	if r := runCLI(t, "", "rekind", "1", "note"); r.code != 1 || !strings.Contains(r.stderr, "already marked fixed; run agentfeedback undo 1 first") {
		t.Fatalf("processed %+v", r)
	}

	entries := make([]string, 0, 32)
	for i := range 32 {
		entries = append(entries, fmt.Sprintf(`"c%02d":"v"`, i))
	}
	l.seed(t, `{"kind":"friction","summary":"full context","context":{`+strings.Join(entries, ",")+`}}`)
	before := len(l.requests())
	if r := runCLI(t, "", "rekind", "2", "note"); r.code != 1 || !strings.Contains(r.stderr, "32-entry context limit") {
		t.Fatalf("full context %+v", r)
	}
	if reqs := l.requests()[before:]; slices.Contains(reqs, "POST /api/v1/submissions") {
		t.Fatalf("submitted anyway: %v", reqs)
	}
}

// rekindStub serves #1 and answers the create with create, recording every
// request.
func rekindStub(t *testing.T, create func(w http.ResponseWriter, body []byte)) *[]string {
	t.Helper()
	var mu sync.Mutex
	var log []string
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		log = append(log, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/submissions/1":
			_, _ = io.WriteString(w, `{"id":1,"uid":"u1","kind":"friction","schema_version":1,"summary":"s","payload":{},"content_hash":"h","created_at":"2026-09-29T12:00:00.000000Z"}`)
		case "GET /api/v1/submissions/2":
			_, _ = io.WriteString(w, `{"id":2,"uid":"u2","kind":"note","schema_version":1,"summary":"s","payload":{},"content_hash":"h2","created_at":"2026-09-29T12:00:01.000000Z"}`)
		case "POST /api/v1/submissions":
			body, _ := io.ReadAll(r.Body)
			create(w, body)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"internal_error","message":"internal error","request_id":"r9"}`)
		}
	})

	return &log
}

func TestRekind_MarkFailed(t *testing.T) {
	isolate(t)
	log := rekindStub(t, func(w http.ResponseWriter, body []byte) {
		var b struct{ Key string }
		_ = json.Unmarshal(body, &b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"submission":{"id":2,"kind":"note","key":%q},"warnings":[]}`, b.Key)
	})
	r := runCLI(t, "", "rekind", "1", "note")
	var o map[string]any
	if r.code != 1 || json.Unmarshal([]byte(lastLine(r.stdout)), &o) != nil || o["outcome"] != "error" || o["reason"] != "mark_failed" ||
		!strings.Contains(o["message"].(string), "rerun agentfeedback rekind 1 note") {
		t.Fatalf("%+v", r)
	}
	if !slices.Contains(*log, "POST /api/v1/submissions/processed") {
		t.Fatalf("requests %v", *log)
	}
}

func TestRekind_Spooled(t *testing.T) {
	isolate(t)
	log := rekindStub(t, func(w http.ResponseWriter, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"unavailable","message":"busy","request_id":"r"}`)
	})
	r := runCLI(t, "", "rekind", "1", "note")
	var o map[string]any
	if r.code != 0 || json.Unmarshal([]byte(lastLine(r.stdout)), &o) != nil || o["outcome"] != "spooled" ||
		!strings.Contains(r.stderr, "rerun agentfeedback rekind 1 note after the spool is flushed") {
		t.Fatalf("%+v", r)
	}
	if slices.Contains(*log, "POST /api/v1/submissions/processed") {
		t.Fatalf("original marked: %v", *log)
	}
}
