package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The Gemini CLI golden store: testdata/stores/gemini-cli of
// internal/sessions under <home>/.gemini, its .project_root naming a real
// working directory.
func init() {
	registerGoldenStore("gemini-cli", func(t *testing.T, home string) string {
		t.Helper()
		cwd := filepath.Join(t.TempDir(), "golden-proj")
		if err := os.Mkdir(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		copyStore(t, filepath.Join("..", "..", "internal", "sessions", "testdata", "stores", "gemini-cli"), filepath.Join(home, ".gemini"),
			map[string]string{"{{SID}}": "c0ffee01", "{{CWD}}": cwd})

		return cwd
	})
}
