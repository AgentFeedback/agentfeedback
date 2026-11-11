package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentfeedback/agentfeedback/v4/schemas"
)

// checkupExample is the report schema's first example, as JSON.
func checkupExample(t *testing.T) string {
	t.Helper()
	raw, err := schemas.FS.ReadFile("checkup-report.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Examples []json.RawMessage `json:"examples"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	return string(doc.Examples[0])
}

// invalidCheckup is the example with an unknown efficiency label.
func invalidCheckup(t *testing.T) string {
	t.Helper()
	bad := strings.Replace(checkupExample(t), `"minor_detours"`, `"speedy"`, 1)
	if bad == checkupExample(t) {
		t.Fatal("the example has no minor_detours label to replace")
	}

	return bad
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

// noOpener records the pages --open would show, and starts nothing.
func noOpener(t *testing.T, fail error) *[]string {
	t.Helper()
	var opened []string
	old := openFile
	openFile = func(p string) error {
		opened = append(opened, p)

		return fail
	}
	t.Cleanup(func() { openFile = old })

	return &opened
}

func TestReportCheck(t *testing.T) {
	isolate(t)
	good := writeTemp(t, checkupExample(t))

	r := runCLI(t, "", "report", "check", good)
	if r.code != 0 || !strings.Contains(r.stdout, "grade D (score 0.6792)") || !strings.Contains(r.stdout, "coverage 1 of 3") || r.stderr != "" {
		t.Fatalf("valid report: %+v", r)
	}

	r = runCLI(t, checkupExample(t), "report", "check", "-", "--json")
	var out checkOutput
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || !out.Valid || len(out.Problems) != 0 || out.Scores == nil || out.Scores.Grade != "D" {
		t.Fatalf("valid report from stdin, --json: %+v", r)
	}

	r = runCLI(t, invalidCheckup(t), "report", "check", "-")
	if r.code != 2 || r.stdout != "" || !strings.HasPrefix(r.stderr, "/sessions/0/labels/efficiency/label: ") || strings.Count(r.stderr, "\n") != 1 {
		t.Fatalf("invalid report: %+v", r)
	}

	r = runCLI(t, invalidCheckup(t), "report", "check", "--json", "-")
	out = checkOutput{}
	if r.code != 2 || r.stderr != "" || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Valid || out.Scores != nil ||
		len(out.Problems) != 1 || out.Problems[0].Pointer != "/sessions/0/labels/efficiency/label" {
		t.Fatalf("invalid report, --json: %+v", r)
	}

	if r := runCLI(t, "", "report", "check"); r.code != 2 {
		t.Fatalf("no file: %+v", r)
	}
	if r := runCLI(t, "", "report", "check", good, good); r.code != 2 {
		t.Fatalf("two files: %+v", r)
	}
	if r := runCLI(t, "", "report", "grade", good); r.code != 2 || !strings.Contains(r.stderr, `unknown report subcommand "grade"`) {
		t.Fatalf("unknown subcommand: %+v", r)
	}
	if r := runCLI(t, "", "report", "check", filepath.Join(t.TempDir(), "missing.json")); r.code != 1 || !strings.Contains(r.stderr, "cannot read") {
		t.Fatalf("missing file: %+v", r)
	}
}

func TestReportRender_Out(t *testing.T) {
	isolate(t)
	opened := noOpener(t, nil)
	out := filepath.Join(t.TempDir(), "page.html")
	r := runCLI(t, checkupExample(t), "report", "render", "-", "--out", out, "--open")
	if r.code != 0 || r.stdout != out+"\n" || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	page, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(page), `<div class="letter">D</div>`) {
		t.Fatalf("page: %v", err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
			t.Fatalf("page mode %v, want 0600", st.Mode().Perm())
		}
	}
	if len(*opened) != 1 || (*opened)[0] != out {
		t.Fatalf("opened %v", *opened)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(out), ".*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestReportRender_CacheDefault(t *testing.T) {
	_, cache := isolate(t)
	opened := noOpener(t, errors.New("no opener"))
	old := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, 11, 10, 9, 30, 5, 0, time.FixedZone("x", 3600)) }
	t.Cleanup(func() { nowFunc = old })

	r := runCLI(t, "", "report", "render", writeTemp(t, checkupExample(t)), "--open")
	want := filepath.Join(cache, "reports", "checkup-20261110T083005Z.html")
	if r.code != 0 || r.stdout != want+"\n" || !strings.Contains(r.stderr, "warning: cannot open the page: no opener") {
		t.Fatalf("%+v", r)
	}
	if len(*opened) != 1 {
		t.Fatalf("opened %v", *opened)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Dir(want)); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("reports dir: %v %v", st, err)
		}
	}
}

func TestReportRender_InvalidWritesNothing(t *testing.T) {
	_, cache := isolate(t)
	opened := noOpener(t, nil)
	out := filepath.Join(t.TempDir(), "page.html")
	r := runCLI(t, invalidCheckup(t), "report", "render", "-", "--out", out, "--open")
	if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, "/sessions/0/labels/efficiency/label: ") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--out written: %v", err)
	}
	if r := runCLI(t, invalidCheckup(t), "report", "render", "-"); r.code != 2 {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(cache, "reports")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reports dir created: %v", err)
	}
	if len(*opened) != 0 {
		t.Fatalf("opened %v", *opened)
	}
}

// The default name never replaces a page: a collision takes the next free
// suffix.
func TestReportRender_DefaultNeverReplaces(t *testing.T) {
	_, cache := isolate(t)
	old := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, 11, 10, 9, 30, 5, 0, time.UTC) }
	t.Cleanup(func() { nowFunc = old })
	in := writeTemp(t, checkupExample(t))
	base := filepath.Join(cache, "reports", "checkup-20261110T093005Z")
	for i, want := range []string{base + ".html", base + "-2.html", base + "-3.html"} {
		r := runCLI(t, "", "report", "render", in)
		if r.code != 0 || r.stdout != want+"\n" {
			t.Fatalf("render %d: %+v", i+1, r)
		}
	}
	if err := os.WriteFile(base+".html", []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := runCLI(t, "", "report", "render", in); r.code != 0 || r.stdout != base+"-4.html\n" {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(base + ".html"); string(b) != "kept" {
		t.Fatal("an existing page was replaced")
	}
	if left, _ := filepath.Glob(filepath.Join(cache, "reports", ".*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestReportRender_OutReplacesButNotTheInput(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "page.html")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := writeTemp(t, checkupExample(t))
	if r := runCLI(t, "", "report", "render", in, "--out", out); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "<!doctype html>") {
		t.Fatal("--out did not replace the existing file")
	}

	targets := []string{in, filepath.Join(filepath.Dir(in), ".", filepath.Base(in))}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link.json")
		if err := os.Symlink(in, link); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, link)
	}
	for _, target := range targets {
		r := runCLI(t, "", "report", "render", in, "--out", target)
		if r.code != 2 || !strings.Contains(r.stderr, "is the report being rendered") {
			t.Fatalf("--out %s: %+v", target, r)
		}
	}
	if b, _ := os.ReadFile(in); string(b) != checkupExample(t) {
		t.Fatal("the input report was changed")
	}
}

func TestReportRender_OpensAbsolutePath(t *testing.T) {
	isolate(t)
	opened := noOpener(t, nil)
	dir := t.TempDir()
	t.Chdir(dir)
	r := runCLI(t, checkupExample(t), "report", "render", "-", "--out", "page.html", "--open")
	if r.code != 0 || r.stdout != "page.html\n" {
		t.Fatalf("%+v", r)
	}
	want, _ := filepath.Abs("page.html")
	if len(*opened) != 1 || (*opened)[0] != want || !filepath.IsAbs((*opened)[0]) {
		t.Fatalf("opened %v, want %s", *opened, want)
	}
}
