package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The Claude Code golden store: testdata/stores/claude-code of
// internal/sessions under the configuration directory, its project
// directory named after a real working directory.
func init() {
	registerGoldenStore("claude-code", func(t *testing.T, home string) string {
		t.Helper()
		cwd := filepath.Join(t.TempDir(), "golden-proj")
		if err := os.Mkdir(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		copyStore(t, filepath.Join("..", "..", "internal", "sessions", "testdata", "stores", "claude-code"), os.Getenv("CLAUDE_CONFIG_DIR"),
			map[string]string{"{{SID}}": "golden-1", "{{CWD}}": cwd, "{{CWD_ENC}}": encodeCwd(cwd)})

		return cwd
	})
}
