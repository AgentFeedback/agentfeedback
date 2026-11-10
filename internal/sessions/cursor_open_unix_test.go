//go:build unix

package sessions

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCursorRejectsSwappedFIFOBeforeRead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := cursorEnv(w)
	path := putCursor(t, w, "fifo-race", defaultCwd, "opaque\n")
	r := cursorReader{}
	candidates, problems, err := r.candidates(e)
	if err != nil || len(problems) != 0 || len(candidates) != 1 {
		t.Fatalf("candidates: %+v %+v %v", candidates, problems, err)
	}
	root, err := os.OpenRoot(e.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	rel, err := filepath.Rel(e.Home, path)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	e.Open = func(string) (io.ReadCloser, error) {
		// The reader already checked the regular source. Swap it immediately
		// before the same rooted helper that the production opener uses.
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			return nil, err
		}
		f, err := openCursorFile(root, rel)
		if err != nil {
			return nil, err
		}
		return cursorReadProbe{File: f, reads: &reads}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.load(e, candidates[0])
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCaptureChanged) || reads != 0 {
			t.Fatalf("swapped FIFO accepted or read: err=%v reads=%d", err, reads)
		}
	case <-time.After(2 * time.Second):
		// Also bound failure on a blocking implementation: a writer releases
		// its open, so the regression cannot leave a blocked reader behind.
		fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("Cursor open blocked; could not release FIFO: %v", err)
		}
		defer func() { _ = syscall.Close(fd) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Cursor open remained blocked after FIFO release")
		}
		t.Fatal("Cursor open blocked on a substituted FIFO")
	}
}
