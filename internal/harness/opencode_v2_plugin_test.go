package harness

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOpenCodeV2PluginNode runs tests/plugins/opencode-v2.test.mjs on the
// generated opencode-v2 plugin; it needs node and is skipped without it.
func TestOpenCodeV2PluginNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "fake.log")
	bin := filepath.Join(dir, "agentfeedback")
	fake := `#!/bin/sh
{ printf 'ARGS: %s %s %s\n' "$1" "$2" "$3"; cat; printf '\n'; } >> "$AF_FAKE_LOG"
if [ "$3" = "session.idle" ]; then printf '{"additionalContext":"fake note"}\n'; fi
`
	if err := os.WriteFile(bin, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache", "agentfeedback", "hooks")
	plugin := filepath.Join(dir, "ok", "agentfeedback.mjs")
	missing := filepath.Join(dir, "missing", "agentfeedback.mjs")
	for path, content := range map[string][]byte{
		plugin:  opencodeV2Plugin(bin, filepath.Join(cache, "unused.spawn-error.json")),
		missing: opencodeV2Plugin(filepath.Join(dir, "no-such-binary"), filepath.Join(cache, "opencode-v2.spawn-error.json")),
	} {
		put(t, path, string(content))
	}
	test, err := filepath.Abs(filepath.Join("..", "..", "tests", "plugins", "opencode-v2.test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	fileURL := func(p string) string { return (&url.URL{Scheme: "file", Path: p}).String() }
	cmd := exec.Command(node, "--test", test)
	cmd.Env = append(os.Environ(),
		"AF_PLUGIN="+fileURL(plugin),
		"AF_PLUGIN_MISSING="+fileURL(missing),
		"AF_FAKE_LOG="+log,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}
