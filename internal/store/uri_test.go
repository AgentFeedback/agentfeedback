package store

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// TestFileURI: the path is escaped so ?, # and % stay part of it, and an
// ordinary path keeps its bytes.
func TestFileURI(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	q := url.Values{"mode": {"ro"}}
	for path, want := range map[string]string{
		"/tmp/x/agentfeedback.db":         "file:/tmp/x/agentfeedback.db?mode=ro",
		"/tmp/a?b#c%d e/agentfeedback.db": "file:/tmp/a%3Fb%23c%25d%20e/agentfeedback.db?mode=ro",
		"rel/agentfeedback.db":            "file:rel/agentfeedback.db?mode=ro",
	} {
		if got := FileURI(path, q); got != want {
			t.Errorf("FileURI(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestFileURIForms: the drive-letter and leading-// forms, on every platform.
func TestFileURIForms(t *testing.T) {
	t.Parallel()
	q := url.Values{"mode": {"ro"}}
	for _, c := range []struct{ path, volume, want string }{
		{`C:\Users\a b\x.db`, "C:", "file:/C:%5CUsers%5Ca%20b%5Cx.db?mode=ro"},
		{"C:/Users/x.db", "C:", "file:/C:/Users/x.db?mode=ro"},
		{`\\server\share\x.db`, `\\server\share`, "file:%5C%5Cserver%5Cshare%5Cx.db?mode=ro"},
		{"//x/y.db", "", "file:////x/y.db?mode=ro"},
	} {
		if got := fileURI(c.path, c.volume, q); got != c.want {
			t.Errorf("fileURI(%q, %q) = %q, want %q", c.path, c.volume, got, c.want)
		}
	}
}

// TestOpenLeadingDoubleSlash: a path beginning with // opens that path (on
// POSIX the same file as with one slash), never a host.
func TestOpenLeadingDoubleSlash(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("// names a UNC share on Windows")
	}
	dir := t.TempDir()
	if !filepath.IsAbs(dir) || dir[0] != '/' {
		t.Skip("temporary directory is not absolute")
	}
	path := "/" + filepath.Join(dir, "agentfeedback.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if names := entryNames(t, dir); !slices.Contains(names, "agentfeedback.db") {
		t.Fatalf("directory holds %v", names)
	}
}

// TestOpenPathNeedingEscapes: a data directory whose name holds ?, # and %
// gets its database at exactly the given path and nothing beside it.
func TestOpenPathNeedingEscapes(t *testing.T) {
	t.Parallel()
	name := "a?b#c%25d"
	if runtime.GOOS == "windows" {
		name = "a#c%25d" // ? is not valid in a Windows file name
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agentfeedback.db")
	ctx := context.Background()
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := db.SchemaVersion(ctx); err != nil || v == 0 {
		t.Fatalf("schema version %d, %v", v, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if names := entryNames(t, parent); !slices.Equal(names, []string{name}) {
		t.Fatalf("parent holds %v", names)
	}
	if names := entryNames(t, dir); !slices.Contains(names, "agentfeedback.db") {
		t.Fatalf("directory holds %v", names)
	}
	for _, n := range entryNames(t, dir) {
		if n != "agentfeedback.db" && n != "agentfeedback.db-wal" && n != "agentfeedback.db-shm" {
			t.Fatalf("directory holds %v", entryNames(t, dir))
		}
	}
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}

	return names
}
