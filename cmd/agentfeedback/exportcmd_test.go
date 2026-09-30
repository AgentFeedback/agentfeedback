package main

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExport_RoundTripThroughImport(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	l.seed(t, `{"kind":"friction","key":"k1","summary":"docs drifted","payload":{"category":"documentation","n":1.50}}`)
	l.seed(t, `{"kind":"mystery","summary":"unknown kind","payload":{"x":[1,2]}}`)
	l.seed(t, `{"kind":"friction","summary":"processed","payload":{"category":"tooling"}}`)
	l.seed(t, `{"kind":"friction","summary":"to redact","context":{"cwd":"/x"},"payload":{"details":"secret"}}`)
	l.call(t, http.MethodPost, "/api/v1/submissions/processed", `{"ids":[3],"verdict":"fixed","ref":"abc@1"}`)
	l.call(t, http.MethodDelete, "/api/v1/submissions/4", "")

	r := runCLI(t, "", "export")
	if r.code != 0 || strings.Count(r.stdout, "\n") != 6 {
		t.Fatalf("export %+v", r)
	}
	_, first, _ := strings.Cut(r.stdout, "\n")
	dir := t.TempDir()
	file := filepath.Join(dir, "export.ndjson")
	if err := os.WriteFile(file, []byte(r.stdout), 0o600); err != nil {
		t.Fatal(err)
	}

	fresh := filepath.Join(dir, "fresh.db")
	t.Setenv("DATABASE_PATH", fresh)
	if code := run([]string{"import", file}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("import exit %d", code)
	}
	_, again := exportBody(t, newService(openDB(t, fresh)))
	if again != first {
		t.Fatalf("record lines differ\n got %s\nwant %s", again, first)
	}
}

func TestExport_ForwardsParams(t *testing.T) {
	isolate(t)
	var got url.Values
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = io.WriteString(w, `{"export_format":2}`+"\n"+
			`{"export_complete":true,"count":0,"first_id":null,"last_id":null,"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}`+"\n")
	})
	r := runCLI(t, "", "export", "--after-id", "0", "--kind", "friction", "--since", "2026-09-01T00:00:00Z", "--limit", "5")
	if r.code != 0 || got.Encode() != "after_id=0&kind=friction&limit=5&since=2026-09-01T00%3A00%3A00Z" {
		t.Fatalf("%+v sent %v", r, got)
	}
}

func TestExport_IncompleteExitsOne(t *testing.T) {
	isolate(t)
	header := `{"export_format":2}` + "\n"
	rec := `{"id":1}` + "\n"
	for name, body := range map[string]string{
		"no trailer":    header + rec,
		"header only":   header,
		"wrong count":   header + rec + `{"export_complete":true,"count":2,"sha256":"x"}` + "\n",
		"wrong sha256":  header + rec + `{"export_complete":true,"count":1,"sha256":"00"}` + "\n",
		"not completed": header + rec + `{"export_complete":false,"count":1,"sha256":"00"}` + "\n",
	} {
		stub(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
		r := runCLI(t, "", "export")
		if r.code != 1 || r.stdout != body || !strings.Contains(r.stderr, "the export is incomplete") {
			t.Fatalf("%s: %+v", name, r)
		}
	}
}

func TestExport_BrokenStreamAndLongLine(t *testing.T) {
	isolate(t)
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"export_format":2}`+"\n"+`{"id":1`)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	if r := runCLI(t, "", "export"); r.code != 1 || !strings.Contains(r.stderr, "the export is incomplete: the stream broke") {
		t.Fatalf("broken %+v", r)
	}

	old := exportLineMax
	exportLineMax = 32
	t.Cleanup(func() { exportLineMax = old })
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"export_format":2}`+"\n"+`{"id":1,"summary":"`+strings.Repeat("x", 100)+`"}`+"\n")
	})
	r := runCLI(t, "", "export")
	if r.code != 1 || r.stdout != `{"export_format":2}`+"\n" || !strings.Contains(r.stderr, "the export is incomplete: a line is longer than") {
		t.Fatalf("long line %+v", r)
	}
}
