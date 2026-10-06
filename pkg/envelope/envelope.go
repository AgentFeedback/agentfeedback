package envelope

import (
	"encoding/json"
	"strconv"

	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
)

// BodyLimit is the largest body the contract accepts, in bytes. Exactly
// BodyLimit is accepted; one byte more is request_too_large.
const BodyLimit = 10485760

// MaxDepth is the deepest container nesting the contract accepts, the body
// object being level 1. conformance/manifest.json states the same number.
const MaxDepth = 512

// StoredDepthHeadroom is how many levels deeper than its body a stored
// envelope can nest, so a stored envelope accepted at MaxDepth re-decodes
// within MaxDepth+StoredDepthHeadroom. Derivation from the inference table,
// the body object being level 1 and a top-level member's container level 2:
//   - an unknown top-level member moved to payload.<name>: level 3, +1;
//     under payload.moved.<name> after a collision: level 4, +2;
//   - a non-object payload wrapped as payload.value: +1;
//   - a non-object payload.moved wrapped as payload.moved.value: its value
//     came from a top-level member (level 2) and lands at level 4, +2;
//   - a non-object context moved to payload.context_raw (+1) or
//     payload.moved.context_raw (+2);
//   - context values, occurred_at_raw and coerced members are strings, and
//     payload.context_overflow holds only strings: no container moves.
//
// No rule places a value below payload.moved.<name> or payload.moved.value,
// and payload.moved is the only nested destination, so +2 is the maximum.
const StoredDepthHeadroom = 2

// Envelope is the stored envelope: every member the server keeps, server
// fields excluded. An empty string means the member is absent; the contract
// never stores an empty string (a member empty after normalisation is
// omitted, an empty key is keyless).
type Envelope struct {
	Kind          string // never empty; "unknown" when inferred
	SchemaVersion uint64 // always present; 1 when absent or defaulted
	Key           string // "" is keyless
	Summary       string
	Machine       string
	Model         string
	Harness       string
	Project       string
	OccurredAt    string            // the stored form YYYY-MM-DDTHH:MM:SS.ffffffZ, or ""
	Context       map[string]string // nil when absent; at most the schema's entry limit
	Payload       map[string]any    // never nil; the JSON tree after inference and the guide's transforms
}

// Tree returns the stored envelope as the JSON tree: absent members omitted,
// schema_version as a json.Number, context as map[string]any. The payload map
// is the envelope's own, not a copy.
func (e *Envelope) Tree() map[string]any {
	t := map[string]any{
		"kind":           e.Kind,
		"schema_version": json.Number(strconv.FormatUint(e.SchemaVersion, 10)),
		"payload":        e.Payload,
	}
	for name, value := range map[string]string{
		"key": e.Key, "summary": e.Summary, "machine": e.Machine, "model": e.Model,
		"harness": e.Harness, "project": e.Project, "occurred_at": e.OccurredAt,
	} {
		if value != "" {
			t[name] = value
		}
	}
	if e.Context != nil {
		context := make(map[string]any, len(e.Context))
		for k, v := range e.Context {
			context[k] = v
		}
		t["context"] = context
	}
	return t
}

// identityMembers are the members of the stored envelope that content
// identity covers. key, occurred_at, context and every server field are
// outside identity.
var identityMembers = []string{"kind", "schema_version", "machine", "model", "harness", "project", "summary", "payload"}

// IdentityTree returns the identity input: the stored envelope's tree
// restricted to identityMembers, absent members omitted, schema_version
// always present. The payload map is the envelope's own, not a copy.
func (e *Envelope) IdentityTree() map[string]any {
	tree := e.Tree()
	identity := make(map[string]any, len(identityMembers))
	for _, name := range identityMembers {
		if v, ok := tree[name]; ok {
			identity[name] = v
		}
	}
	return identity
}

// ContentHash returns content_hash: the lower-case hex SHA-256 of the
// canonical JSON of IdentityTree.
func (e *Envelope) ContentHash() string {
	return canonjson.Sum(e.IdentityTree())
}

// Rejection is the error Decode returns for the four bodies the contract
// refuses: the HTTP status (400 or 413), the error code (bad_request or
// request_too_large) and an advisory message that never quotes the body.
type Rejection struct {
	Status  int
	Code    string
	Message string
}

func (r *Rejection) Error() string { return r.Code + ": " + r.Message }
