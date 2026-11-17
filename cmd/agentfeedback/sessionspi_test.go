package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func init() {
	registerGoldenStore("pi", func(t *testing.T, home string) string {
		cwd := filepath.Join(home, "pi-project")
		t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))
		if err := os.MkdirAll(cwd, 0o700); err != nil {
			t.Fatal(err)
		}
		putPiGolden(t, home, cwd, "basic", "Inspect the fixture")
		return cwd
	})
	registerGoldenSkillSessions("pi", func(t *testing.T, home, cwd string) {
		putPiGolden(t, home, cwd, "skill", "Use golden-review to inspect the fixture")
	})
}

func putPiGolden(t *testing.T, home, cwd, id, prompt string) {
	t.Helper()
	root := filepath.Join(home, ".pi", "agent", "sessions", "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"cwd":%q,"timestamp":"2026-10-01T00:00:00Z"}
{"type":"message","id":"prompt","parentId":null,"message":{"role":"user","content":%q,"timestamp":1790812800000}}
{"type":"message","id":"call","parentId":"prompt","message":{"role":"assistant","content":[{"type":"toolCall","id":"tool","name":"bash","arguments":{"command":"false"}}],"timestamp":1790812801000}}
{"type":"message","id":"result","parentId":"call","message":{"role":"toolResult","toolCallId":"tool","toolName":"bash","content":[{"type":"text","text":"Command exited with code 1"}],"isError":true,"timestamp":1790812802000}}
`, id, cwd, prompt)
	if err := os.WriteFile(filepath.Join(root, id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
