package main

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The OpenCode 2 golden store: testdata/stores/opencode-v2/opencode.sql of
// internal/sessions executed into <data home>/opencode/opencode.db, the
// data home under the home directory, in WAL mode and closed so no -wal
// file remains. The store is a migrated V1 database (V1's tables kept, the
// same session id in session_v2), so it is listed under opencode-v2 only.
func init() {
	registerGoldenStore("opencode-v2", func(t *testing.T, home string) string {
		t.Helper()
		cwd := filepath.Join(t.TempDir(), "golden-proj")
		if err := os.Mkdir(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		data := filepath.Join(home, ".local", "share")
		t.Setenv("XDG_DATA_HOME", data)
		dir := filepath.Join(data, "opencode")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join("..", "..", "internal", "sessions", "testdata", "stores", "opencode-v2", "opencode.sql"))
		if err != nil {
			t.Fatal(err)
		}
		stmts := strings.NewReplacer("{{SID}}", "ses_golden0001", "{{CWD}}", cwd).Replace(string(b))
		db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(filepath.Join(dir, "opencode.db"))}).EscapedPath())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(stmts); err != nil {
			t.Fatal(err)
		}

		return cwd
	})
}
