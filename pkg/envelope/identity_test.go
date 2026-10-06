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
