package mcp

import (
	"context"
	"encoding/json"
	"maps"
)

// The [local] arguments of submit_feedback: served only when
// Config.ContextFrom is set, which only the stdio transport does. They name
// an entry of a session log on the machine the server runs on, so the HTTP
// transport never takes them.
const (
	ArgContextFrom = "context_from"
	ArgOrdinal     = "ordinal"
)

// SubmitLocalArgs are the [local] arguments of submit_feedback.
var SubmitLocalArgs = []string{ArgContextFrom, ArgOrdinal}

// ContextFrom turns the arguments of a submit_feedback call that carries
// context_from or ordinal into the submission body, as submit --context-from
// builds it from the same members on stdin. An error that is a
// *core.Problem is answered as that problem; any other is an internal error.
type ContextFrom interface {
	Body(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// UsesContextFrom reports whether args carry a [local] argument of
// submit_feedback. Arguments that are not one object carry none.
func UsesContextFrom(args json.RawMessage) bool {
	if isAbsent(args) {
		return false
	}
	members, err := scan(args)
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.name == ArgContextFrom || m.name == ArgOrdinal {
			return true
		}
	}
	return false
}

// contextFromProps are the input-schema properties of the [local]
// arguments of submit_feedback.
var contextFromProps = map[string]any{
	ArgContextFrom: prop("string", "<harness>:<session id>#<span>, the ref of a sessions_digest event: key, occurred_at, harness, model, "+
		"project and context are taken from that entry of the session log on this machine, and the body is scrubbed; "+
		"do not send those members yourself"),
	ArgOrdinal: prop("integer", "with context_from: the number of this finding at that span, from 1 (default 1); enters the key"),
}

// withContextFrom is the input schema input plus the [local] arguments of
// submit_feedback; input is not changed.
func withContextFrom(input any) any {
	m, ok := input.(map[string]any)
	if !ok {
		return input
	}
	out := maps.Clone(m)
	props, _ := m["properties"].(map[string]any)
	props = maps.Clone(props)
	if props == nil {
		props = map[string]any{}
	}
	maps.Copy(props, contextFromProps)
	out["properties"] = props
	return out
}
