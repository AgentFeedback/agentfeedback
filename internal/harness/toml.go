package harness

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/pelletier/go-toml/v2"
)

const (
	tomlBegin = "# agentfeedback:begin (written by agentfeedback install; agentfeedback uninstall removes it)"
	tomlEnd   = "# agentfeedback:end"
)

// codexBlock is the marked block install appends to Codex's config.toml.
func codexBlock(server string) string {
	return tomlBegin + "\n" +
		"[mcp_servers.agentfeedback]\n" +
		"url = " + tomlString(server+"/mcp") + "\n" +
		"bearer_token_env_var = \"AGENT_FEEDBACK_API_KEY\"\n" +
		tomlEnd + "\n"
}

// tomlString is s as a TOML basic string; JSON string escapes are valid in
// one.
func tomlString(s string) string {
	return string(jsonString(s))
}

// noEndMarker is the refusal for a block whose end marker is gone: where
// the block ends is unknown, so nothing is removed.
func noEndMarker(file string) error {
	return &Refusal{
		Problem: "the agentfeedback block in " + file + " has no end marker",
		Next:    "remove the block by hand",
	}
}

// withoutMarked returns doc with every marked agentfeedback block removed;
// a begin marker without an end marker is an error, never a truncation.
func withoutMarked(file string, doc []byte) ([]byte, error) {
	out := bytes.Clone(doc)
	for {
		i := bytes.Index(out, []byte(tomlBegin))
		if i < 0 {
			return out, nil
		}
		j := bytes.Index(out[i:], []byte(tomlEnd))
		if j < 0 {
			return nil, noEndMarker(file)
		}
		end := i + j + len(tomlEnd)
		if end < len(out) && out[end] == '\n' {
			end++
		}
		out = append(out[:i:i], out[end:]...)
	}
}

// tomlForeign reports whether doc defines mcp_servers.agentfeedback outside
// the marked blocks; a document that does not parse is an error.
func tomlForeign(file string, doc []byte) (bool, error) {
	if err := tomlValid(doc); err != nil {
		return false, err
	}
	rest, err := withoutMarked(file, doc)
	if err != nil {
		return false, err
	}
	var m map[string]any
	if err := toml.Unmarshal(rest, &m); err != nil {
		return false, fmt.Errorf("not valid TOML outside the agentfeedback block: %w", err)
	}
	servers, ok := m["mcp_servers"].(map[string]any)
	if !ok {
		return false, nil
	}
	_, ok = servers["agentfeedback"]

	return ok, nil
}

func tomlValid(doc []byte) error {
	var m map[string]any
	if err := toml.Unmarshal(doc, &m); err != nil {
		return fmt.Errorf("not valid TOML: %w", err)
	}

	return nil
}

// tomlAppend appends block at the end of doc; inserted is exactly the text
// added (a separating line break included).
func tomlAppend(doc []byte, block string) (out []byte, inserted string, err error) {
	inserted = block
	if len(doc) > 0 && doc[len(doc)-1] != '\n' {
		inserted = "\n" + block
	}
	out = append(bytes.Clone(doc), inserted...)
	if err := tomlValid(out); err != nil {
		return nil, "", fmt.Errorf("the edited document would not be valid: %w", err)
	}

	return out, inserted, nil
}

// tomlRemove deletes the inserted text; when it is no longer there verbatim,
// the marked block alone is removed.
func tomlRemove(file string, doc []byte, inserted string) ([]byte, error) {
	var out []byte
	if i := bytes.LastIndex(doc, []byte(inserted)); i >= 0 {
		out = append(bytes.Clone(doc[:i]), doc[i+len(inserted):]...)
	} else {
		var err error
		if out, err = withoutMarked(file, doc); err != nil {
			return nil, err
		}
	}
	if err := tomlValid(out); err != nil {
		return nil, errors.New("removing the agentfeedback block would leave config.toml invalid")
	}

	return out, nil
}
