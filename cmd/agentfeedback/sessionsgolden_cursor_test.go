package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func init() {
	registerGoldenStore("cursor", func(t *testing.T, home string) string {
		t.Helper()
		cwd := filepath.Join(home, "cursor-project")
		if err := os.MkdirAll(cwd, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, sample := range []struct{ id, file, event string }{
			{"ide", "cursor-ide-transcript.txt", "stop"},
			{"cli", "cursor-cli-transcript.jsonl", "sessionEnd"},
		} {
			raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "sessions", "testdata", sample.file))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, sample.file)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			stamp := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			if err := os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(map[string]any{"conversation_id": sample.id, "transcript_path": path, "workspace_roots": []string{cwd}, "status": "completed"})
			if err != nil {
				t.Fatal(err)
			}
			if r := runCLI(t, string(payload), "hook", "cursor", sample.event); r.code != 0 {
				t.Fatal(r)
			}
		}
		return cwd
	})
}
