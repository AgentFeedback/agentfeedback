package client

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// TestLog is AC4: one line per outcome, owner-only, no secrets, rotation.
func TestLog(t *testing.T) {
	var hits atomic.Int64
	e := newTestEnv(t, acceptingServer(t, &hits).URL, nil)
	e.c.Submit(context.Background(), []byte(`{"kind":"friction","summary":"SECRETPAYLOAD"}`))
	e.spoolEntry(t, entryName(1), "event", "a", t0, t0)
	e.spoolEntry(t, entryName(2), "event", "b", t0, t0)
	e.c.Flush(context.Background())

	lines := logLines(t, e.cache)
	if len(lines) != 3 || lines[0]["outcome"] != OutcomeSubmitted || lines[1]["outcome"] != OutcomeFlushed || lines[2]["outcome"] != OutcomeFlushed {
		t.Fatalf("log = %v", lines)
	}
	data, _ := os.ReadFile(LogPath(e.cache))
	for _, secret := range []string{testKey, "SECRETPAYLOAD", "127.0.0.1"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("log holds %q", secret)
		}
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(LogPath(e.cache))
		dst, _ := os.Stat(filepath.Dir(LogPath(e.cache)))
		if st.Mode().Perm() != 0o600 || dst.Mode().Perm() != 0o700 {
			t.Errorf("modes file %v dir %v", st.Mode().Perm(), dst.Mode().Perm())
		}
	}

	old := logMaxBytes
	logMaxBytes = 300
	defer func() { logMaxBytes = old }()
	for range 10 {
		e.c.Log(Valid("friction", "k"))
	}
	st, err := os.Stat(LogPath(e.cache) + ".1")
	if err != nil {
		t.Fatalf("no rotated log: %v", err)
	}
	cur, _ := os.Stat(LogPath(e.cache))
	if cur.Size() > 300 || st.Size() == 0 {
		t.Errorf("sizes current %d rotated %d", cur.Size(), st.Size())
	}
}

// TestRotateLostRace: a writer whose file is no longer the one at path
// (another process rotated it) reopens without renaming, so .1 survives.
func TestRotateLostRace(t *testing.T) {
	old := logMaxBytes
	logMaxBytes = 10
	defer func() { logMaxBytes = old }()
	path := filepath.Join(t.TempDir(), "client.log")
	if err := os.WriteFile(path, []byte("first file, full"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	// The other process rotates and starts a new file.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second file, full"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err = rotateIfFull(f, path, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if got, _ := os.ReadFile(path + ".1"); string(got) != "first file, full" {
		t.Errorf(".1 = %q", got)
	}
	if got, _ := os.ReadFile(path); string(got) != "second file, full" {
		t.Errorf("path = %q", got)
	}
}

// TestLogPathSingleSource pins where the log's name and path live: the
// string "client.jsonl" only in pkg/client/log.go, LogPath( only in
// pkg/client and the doctor. Comments are not code and are not counted.
func TestLogPathSingleSource(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata", ".worktrees":
				return filepath.SkipDir
			}

			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING && strings.Contains(n.Value, "client.jsonl") && rel != "pkg/client/log.go" {
					t.Errorf("%s names client.jsonl; use client.LogPath", rel)
				}
			case *ast.CallExpr:
				name := ""
				switch fn := n.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				if name == "LogPath" && !strings.HasPrefix(rel, "pkg/client/") && rel != "cmd/agentfeedback/doctor.go" {
					t.Errorf("%s calls LogPath", rel)
				}
			}

			return true
		})

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
