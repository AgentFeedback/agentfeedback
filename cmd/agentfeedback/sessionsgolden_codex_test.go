package main

import (
	"path/filepath"
	"testing"
)

// The Codex golden store: testdata/stores/codex of internal/sessions under
// the Codex home. Its rollouts record a fixed working directory that does
// not exist: the spans of entries without an id are line offsets, which a
// working directory of varying length would move.
func init() {
	registerGoldenStore("codex", func(t *testing.T, home string) string {
		t.Helper()
		cwd := "/nonexistent-af/golden-proj"
		copyStore(t, filepath.Join("..", "..", "internal", "sessions", "testdata", "stores", "codex"), filepath.Join(home, ".codex"),
			map[string]string{"{{SID}}": "0199c3a1-0000-7000-8000-00000000000", "{{CWD}}": cwd})

		return cwd
	})
}
