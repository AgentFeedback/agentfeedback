package envelope

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
)

// TestContentHashVectors runs every conformance/hash vector from its body
// through Decode: the identity input's canonical bytes equal canonical.json
// and ContentHash equals the vector's sha256.
func TestContentHashVectors(t *testing.T) {
	t.Parallel()
	for _, name := range fixtureDirs(t, "hash") {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(conformanceDir, "hash", name)
			env, _, err := Decode(fixtureBody(t, dir))
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := os.ReadFile(filepath.Join(dir, "canonical.json"))
			if err != nil {
				t.Fatal(err)
			}
			if got := canonjson.Marshal(env.IdentityTree()); !bytes.Equal(got, canonical) {
				t.Errorf("identity input\n got %s\nwant %s", got, canonical)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "expected.json"))
			if err != nil {
				t.Fatal(err)
			}
			var want struct {
				SHA256 string `json:"sha256"`
			}
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if got := env.ContentHash(); got != want.SHA256 {
				t.Errorf("content hash %s, want %s", got, want.SHA256)
			}
		})
	}
}

// TestContentHashIgnoresProvenanceContext pins that the context keys naming
// how a submission came to be never change its identity: the same report
// filed by an agent, a hook nudge or a session scan replays as one row.
func TestContentHashIgnoresProvenanceContext(t *testing.T) {
	t.Parallel()
	const head = `{"kind":"friction","summary":"s","machine":"m","model":"x","payload":{"category":"tooling","details":"d"}`
	bodies := []string{
		head + `}`,
		head + `,"context":{"origin":"agent"}}`,
		head + `,"context":{"origin":"session-scan","detector":"agentfeedback-sessions/1","session_harness":"codex"}}`,
		head + `,"context":{"origin":"hook-nudge","detector":"agentfeedback-hook/2","session_harness":"claude-code"}}`,
	}
	var want string
	for i, body := range bodies {
		env, _, err := Decode([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			want = env.ContentHash()
			continue
		}
		if got := env.ContentHash(); got != want {
			t.Errorf("body %d: content hash %s, want %s", i, got, want)
		}
	}
	// evidence[] is payload content, so it is part of identity.
	env, _, err := Decode([]byte(`{"kind":"friction","summary":"s","machine":"m","model":"x","payload":{"category":"tooling","details":"d","evidence":[{"tool":"bash"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if env.ContentHash() == want {
		t.Error("evidence[] did not change the content hash")
	}
}
