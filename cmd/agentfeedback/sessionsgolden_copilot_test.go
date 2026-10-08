package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The Copilot CLI golden store: testdata/stores/copilot of
// internal/sessions under the Copilot home, its workspace.yaml naming a
// real working directory.
func init() {
	registerGoldenStore("copilot", func(t *testing.T, home string) string {
		t.Helper()
		cwd := filepath.Join(t.TempDir(), "golden-proj")
		if err := os.Mkdir(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		copyStore(t, filepath.Join("..", "..", "internal", "sessions", "testdata", "stores", "copilot"), filepath.Join(home, ".copilot"),
			map[string]string{"{{SID}}": "golden-1", "{{CWD}}": cwd})

		return cwd
	})
}
