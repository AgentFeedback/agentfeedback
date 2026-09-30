package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedDigest fills the live server: two exact repeats, a row without a
// summary, an unknown kind without project or category, a long summary, an
// install-check row (6) and a processed row (7).
func seedDigest(t *testing.T, l *live) {
	t.Helper()
	for _, body := range []string{
		`{"kind":"friction","key":"k1","summary":"docs drifted","machine":"m1","model":"x-1","harness":"claude-code","project":"alpha","payload":{"category":"documentation","suggested_fix":"update the\nREADME"}}`,
		`{"kind":"friction","key":"k2","summary":"docs drifted","machine":"m1","model":"x-1","harness":"claude-code","project":"alpha","payload":{"category":"documentation","suggested_fix":"update the\nREADME"}}`,
		`{"kind":"friction","project":"alpha","payload":{"category":"tooling","details":"line one\nline two","n":3}}`,
		`{"kind":"mystery","payload":{"x":"y","z":{"deep":"skipped"}}}`,
		`{"kind":"friction","project":"beta","summary":"` + strings.Repeat("long ", 50) + `end","payload":{"category":"tooling"}}`,
		`{"kind":"install-check","summary":"install ok","project":"alpha"}`,
		`{"kind":"friction","project":"alpha","summary":"already done","payload":{"category":"tooling"}}`,
	} {
		l.seed(t, body)
	}
	l.call(t, http.MethodPost, "/api/v1/submissions/processed", `{"ids":[7],"verdict":"fixed"}`)
}

func TestDigest_Golden(t *testing.T) {
	isolate(t)
	l := liveServer(t)
	seedDigest(t, l)
	out := filepath.Join(t.TempDir(), "d")

	r := runCLI(t, "", "digest", "--out", out)
	if r.code != 0 || lastLine(r.stdout) != out || !strings.Contains(r.stderr, "pulled 5 row(s) (server total 5)") {
		t.Fatalf("digest %+v", r)
	}
	got, err := os.ReadFile(filepath.Join(out, "digest.md"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "digest.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("digest.md differs from testdata/digest.md\n got:\n%s", got)
	}

	var index []map[string]any
	raw, err := os.ReadFile(filepath.Join(out, "index.json"))
	if err != nil || json.Unmarshal(raw, &index) != nil || len(index) != 5 {
		t.Fatalf("index.json %s %v", raw, err)
	}
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		if _, err := os.Stat(filepath.Join(out, id+".json")); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"6", "7"} {
		if _, err := os.Stat(filepath.Join(out, id+".json")); !os.IsNotExist(err) {
			t.Fatalf("%s.json pulled: %v", id, err)
		}
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v %v", info.Mode(), err)
	}

	if r := runCLI(t, "", "digest", "--out", out); r.code != 1 || !strings.Contains(r.stderr, "is not empty") {
		t.Fatalf("non-empty --out %+v", r)
	}

	out2 := filepath.Join(t.TempDir(), "d2")
	if r := runCLI(t, "", "digest", "--out", out2, "--include-kind", "install-check"); r.code != 0 {
		t.Fatalf("include %+v", r)
	}
	if _, err := os.Stat(filepath.Join(out2, "6.json")); err != nil {
		t.Fatalf("install-check not pulled: %v", err)
	}
}

func TestDigest_DefaultDirUnderCache(t *testing.T) {
	_, cache := isolate(t)
	fixClock(t, time.Date(2026, 9, 30, 8, 9, 10, 0, time.UTC))
	liveServer(t)

	r := runCLI(t, "", "digest")
	dir := lastLine(r.stdout)
	if r.code != 0 || filepath.Dir(dir) != filepath.Join(cache, "digest") || !strings.HasPrefix(filepath.Base(dir), "20260930T080910Z-") {
		t.Fatalf("digest %+v", r)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v %v", info, err)
	}
	if md, err := os.ReadFile(filepath.Join(dir, "digest.md")); err != nil || !strings.Contains(string(md), "pulled: 0") {
		t.Fatalf("digest.md %s %v", md, err)
	}
}

func TestDigest_ProcessedRowExitsTwo(t *testing.T) {
	isolate(t)
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"submissions":[{"id":3,"uid":"u","kind":"friction","schema_version":1,"content_hash":"h",`+
			`"created_at":"2026-09-29T12:00:00.000000Z","processed_at":"2026-09-29T13:00:00.000000Z","payload":{}}],`+
			`"limit":100,"total":1,"has_more":false,"next_before_id":null,"next_after_id":null}`)
	})
	out := filepath.Join(t.TempDir(), "d")
	r := runCLI(t, "", "digest", "--out", out)
	if r.code != 2 || lastLine(r.stdout) != out || !strings.Contains(r.stderr, "contaminated") {
		t.Fatalf("%+v", r)
	}
}

func TestDigest_HalvesThePageWhenTooLarge(t *testing.T) {
	isolate(t)
	old := digestPage
	t.Cleanup(func() { digestPage = old })
	digestPage = 4
	var limits []string
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		limit := r.URL.Query().Get("limit")
		limits = append(limits, limit)
		if limit != "1" {
			writeOversized(w)

			return
		}
		_, _ = io.WriteString(w, `{"submissions":[{"id":1,"uid":"u","kind":"friction","schema_version":1,"content_hash":"h",`+
			`"created_at":"2026-09-29T12:00:00.000000Z","payload":{}}],"limit":1,"total":1,"has_more":false,"next_before_id":null,"next_after_id":null}`)
	})
	out := filepath.Join(t.TempDir(), "d")
	if r := runCLI(t, "", "digest", "--out", out); r.code != 0 || strings.Join(limits, ",") != "4,2,1" {
		t.Fatalf("%+v limits %v", r, limits)
	}

	digestPage = 1
	stub(t, func(w http.ResponseWriter, _ *http.Request) { writeOversized(w) })
	out2 := filepath.Join(t.TempDir(), "d2")
	if r := runCLI(t, "", "digest", "--out", out2); r.code != 1 || !strings.Contains(r.stderr, "larger than 48 MiB") {
		t.Fatalf("still too large %+v", r)
	}
	if _, err := os.Stat(out2); !os.IsNotExist(err) {
		t.Fatalf("created --out left behind: %v", err)
	}
}

func TestDigest_OutputSafety(t *testing.T) {
	cache := func() string { _, c := isolate(t); return c }()
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"internal_error","message":"internal error","request_id":"r"}`)
	})

	// A directory digest made is removed on failure; one that existed stays.
	if r := runCLI(t, "", "digest"); r.code != 1 {
		t.Fatalf("default %+v", r)
	}
	if entries, err := os.ReadDir(filepath.Join(cache, "digest")); err != nil || len(entries) != 0 {
		t.Fatalf("default dir left behind: %v %v", entries, err)
	}
	existing := t.TempDir()
	if r := runCLI(t, "", "digest", "--out", existing); r.code != 1 {
		t.Fatalf("existing %+v", r)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Fatalf("existing --out removed: %v", err)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if r := runCLI(t, "", "digest", "--out", link); r.code != 1 || !strings.Contains(r.stderr, "is a symbolic link") {
		t.Fatalf("symlink %+v", r)
	}

	path := filepath.Join(t.TempDir(), "digest.md")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createFile(path, []byte("new")); err == nil {
		t.Fatal("createFile replaced an existing file")
	}
}
