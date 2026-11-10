package sessions

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

type cursorReadProbe struct {
	*os.File
	reads *int
}

func (f cursorReadProbe) Read(b []byte) (int, error) {
	(*f.reads)++
	return f.File.Read(b)
}

func TestCursorRejectsSubstitutedDescriptorBeforeRead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := cursorEnv(w)
	path := putCursor(t, w, "descriptor", defaultCwd, "opaque\n")
	outside := filepath.Join(filepath.Dir(w.home), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := 0
	e.Open = func(string) (io.ReadCloser, error) {
		f, err := os.Open(outside)
		if err != nil {
			return nil, err
		}
		return cursorReadProbe{File: f, reads: &reads}, nil
	}
	c := captureOne(t, w, e, "cursor:descriptor")
	if c.State != StateChanged || c.Size != 0 || reads != 0 {
		t.Fatalf("substituted descriptor read or accepted: state=%s reads=%d source=%s", c.State, reads, path)
	}
}

func TestCursorCaptureRechecksRepositoryBeforeReopen(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := cursorEnv(w)
	cwd := filepath.Join(w.home, "repo")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	path := putCursor(t, w, "repo-policy", cwd, "opaque\n")
	c := captureOne(t, w, e, "cursor:repo-policy")
	before := w.opens[path]
	if err := os.WriteFile(filepath.Join(cwd, ".agentfeedback.toml"), []byte("[sessions]\npush = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := OpenCapture(w.ctx, e, c); raw != nil || err == nil || w.opens[path] != before {
		t.Fatalf("new repository refusal did not prevent reopening: %v", err)
	}
}
