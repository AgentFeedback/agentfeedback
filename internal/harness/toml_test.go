package harness

import (
	"errors"
	"strings"
	"testing"
)

func TestTOMLBlock_AppendRemove(t *testing.T) {
	block := codexBlock("https://feedback.example.test")
	for _, doc := range []string{"", "# settings\nmodel = \"gpt-5\"\n", "model = \"gpt-5\"", "[mcp_servers.other]\nurl = \"u\" # keep\n"} {
		out, inserted, err := tomlAppend([]byte(doc), block)
		if err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if !strings.HasSuffix(string(out), block) || !strings.HasPrefix(string(out), doc) {
			t.Fatalf("%q: appended\n%s", doc, out)
		}
		back, err := tomlRemove("config.toml", out, inserted)
		if err != nil || string(back) != doc {
			t.Fatalf("%q: removed to %q, %v", doc, back, err)
		}
	}
}

func TestTOMLBlock_Foreign(t *testing.T) {
	block := codexBlock("https://feedback.example.test")
	tests := []struct {
		doc  string
		want bool
	}{
		{"model = \"gpt-5\"\n", false},
		{"[mcp_servers.other]\nurl = \"u\"\n", false},
		{block, false},
		{"[mcp_servers.agentfeedback]\nurl = \"mine\"\n", true},
		{"[mcp_servers]\nagentfeedback = { url = \"mine\" }\n", true},
	}
	for _, tt := range tests {
		got, err := tomlForeign("config.toml", []byte(tt.doc))
		if err != nil || got != tt.want {
			t.Errorf("%q: foreign %v %v, want %v", tt.doc, got, err, tt.want)
		}
	}
}

func TestTOMLBlock_MissingEndMarker(t *testing.T) {
	doc := []byte("model = \"gpt-5\"\n" + tomlBegin + "\n[mcp_servers.agentfeedback]\nurl = \"u\"\n\n[profiles.mine]\nmodel = \"o3\"\n")
	var r *Refusal
	if _, err := tomlForeign("/h/.codex/config.toml", doc); !errors.As(err, &r) ||
		err.Error() != "the agentfeedback block in /h/.codex/config.toml has no end marker; remove the block by hand." {
		t.Errorf("foreign: %v", err)
	}
	out, err := tomlRemove("/h/.codex/config.toml", doc, "not there")
	if !errors.As(err, &r) || out != nil {
		t.Errorf("remove truncated or did not refuse: %q %v", out, err)
	}
}
