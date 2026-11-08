package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/v4/internal/sessions"
	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

const (
	maxRawPart  = 4 << 20
	maxRequest  = 10 << 20
	maxEnvelope = 24 << 20
)

func shaText(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func captureEnvelope(env sessions.Env, c sessions.CaptureCandidate, m sessions.CaptureMeta, revision string, opts PushOptions) (map[string]any, scrub.Counts, bool) {
	for _, cwd := range c.Cwds {
		if collect.Check(cwd, env.Home, env.Policy).Disabled {
			return nil, nil, false
		}
		if allowed, _ := collect.SessionPush(cwd); !allowed {
			return nil, nil, false
		}
	}
	r := collect.Result{Context: map[string]string{}}
	if h, err := os.Hostname(); err == nil {
		r.Machine, _, _ = strings.Cut(h, ".")
	}
	if c.Cwd != "" {
		r.Project = filepath.Base(c.Cwd)
		if st, err := os.Stat(c.Cwd); err == nil && st.IsDir() {
			d := collect.Check(c.Cwd, env.Home, env.Policy)
			if d.Disabled {
				return nil, nil, false
			}
			drop := append(append([]string(nil), opts.Drop...), d.Drop...)
			r = collect.Collect(collect.Options{Dir: c.Cwd, Home: env.Home, Getenv: func(string) string { return "" }, ClientVersion: opts.ClientVersion, WithCwd: opts.WithCwd, Drop: drop})
		}
	}
	ctx := make(map[string]any, len(r.Context)+4)
	for k, v := range r.Context {
		ctx[k] = strings.ToValidUTF8(v, "�")
	}
	ctx["origin"], ctx["detector"] = "session-capture", c.Reader+"/"+sessions.DetectorVersion
	ctx["session_id"], ctx["session_harness"] = strings.ToValidUTF8(c.SessionID, "�"), c.Harness
	e := map[string]any{
		"kind": "session", "key": "session-" + shaText(c.Harness+"\x00"+c.SessionID+"\x00"+revision),
		"harness": c.Harness, "occurred_at": m.End.UTC().Format(captureTimeLayout), "context": ctx,
		"payload": map[string]any{
			"format": m.Format, "revision": revision, "size": number(c.Size),
			"line_count": number(m.LineCount), "detector_version": sessions.DetectorVersion,
			"counts": map[string]any{
				"entries": number(int64(m.Counts.Entries)), "prompts": number(int64(m.Counts.Prompts)),
				"tool_calls": number(int64(m.Counts.ToolCalls)), "tool_errors": number(int64(m.Counts.ToolErrors)),
			},
		},
	}
	for k, v := range map[string]string{"project": r.Project, "machine": r.Machine, "model": m.Model} {
		if v != "" {
			e[k] = strings.ToValidUTF8(v, "�")
		}
	}
	counts := scrub.Tree(e)
	// Source identity and revision must survive metadata scrubbing unchanged.
	e["key"], e["harness"] = "session-"+shaText(c.Harness+"\x00"+c.SessionID+"\x00"+revision), c.Harness
	return e, counts, true
}

func number(n int64) json.Number { return json.Number(formatRevision(n)) }

func assembledBytes(envelope map[string]any, transcript string) int64 {
	p := envelope["payload"].(map[string]any)
	p["transcript"] = transcript
	n := int64(len(canonjson.Marshal(envelope)))
	delete(p, "transcript")
	return n
}

// splitTranscript bounds both raw UTF-8 bytes and canonical serialized request
// bytes. Prefer a complete line; a single oversized line is split at codepoints.
func splitTranscript(text string, cap int) []string {
	if cap > maxRawPart {
		cap = maxRawPart
	}
	if cap <= 0 || text == "" {
		return nil
	}
	parts := make([]string, 0, (len(text)+cap-1)/cap)
	for len(text) > 0 {
		n := min(len(text), cap)
		for n > 0 && n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		// Quotes/hash/object framing is fixed; count escapes without allocating
		// repeated multi-megabyte JSON strings during partition selection.
		escaped, fit := 0, 0
		for i := range n {
			cost := 1
			b := text[i]
			if b == '"' || b == '\\' || b == '\n' || b == '\r' || b == '\t' {
				cost = 2
			} else if b < 0x20 {
				cost = 6
			}
			if escaped+cost+90 > maxRequest {
				break
			}
			escaped += cost
			fit = i + 1
		}
		n = fit
		for n > 0 && n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		if n == 0 {
			return nil
		}
		if n < len(text) {
			if line := strings.LastIndexByte(text[:n], '\n'); line >= 0 {
				n = line + 1
			}
		}
		parts = append(parts, text[:n])
		text = text[n:]
		if len(parts) > 256 {
			return nil
		}
	}
	return parts
}

func preview(text string) string {
	n := min(len(text), 2048)
	for n > 0 && n < len(text) && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}
