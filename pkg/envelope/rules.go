package envelope

import (
	"fmt"
	"slices"

	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

// The envelope's member set and the rules the compiled envelope schema
// states. Read once at init; a schema that lacks what the decoder depends
// on is a programming error and panics, never a silently weaker decode.

// envelopeMembers is the member list in the contract's order.
var envelopeMembers = []string{
	"kind", "schema_version", "key", "summary", "machine", "model",
	"harness", "project", "occurred_at", "context", "payload",
}

// Members returns the top-level members the envelope knows, in the
// contract's order.
func Members() []string { return slices.Clone(envelopeMembers) }

// stringMembers are the members the coerce and normalisation rows apply to;
// kind, schema_version, occurred_at, context and payload have rows of their
// own.
var stringMembers = []string{"key", "summary", "machine", "model", "harness", "project"}

// schemaVersionMax is the largest schema_version the contract accepts,
// 2^53-1; larger spellings default to 1. Stated by the envelope schema's
// maximum, which the tests check.
const schemaVersionMax = 1<<53 - 1

type memberRule struct {
	limit int  // truncation limit in bytes; 0 means never truncated
	token bool // x-normalize: token, otherwise x-trim
}

var (
	rules             map[string]memberRule
	contextEntries    int
	contextValueBytes int
	recommended       []string
	isEnvelopeMember  map[string]bool
)

func init() {
	env := schema.Envelope()
	if got, want := env.PropertyNames(), slices.Sorted(slices.Values(envelopeMembers)); !slices.Equal(got, want) {
		panic(fmt.Sprintf("envelope: schema members %v, decoder knows %v", got, want))
	}
	isEnvelopeMember = map[string]bool{}
	for _, name := range envelopeMembers {
		isEnvelopeMember[name] = true
	}
	rules = map[string]memberRule{}
	for _, name := range stringMembers {
		p, ok := env.Property(name)
		if !ok || !slices.Contains(p.Types(), "string") {
			panic("envelope: schema member " + name + " is not a string")
		}
		r := memberRule{token: p.Normalize() == "token"}
		if !r.token && !p.Trim() {
			panic("envelope: schema member " + name + " has neither x-trim nor x-normalize")
		}
		if p.OnViolation() == "truncate" {
			if r.limit = p.MaxBytes(); r.limit <= 0 {
				panic("envelope: schema member " + name + " truncates without x-max-bytes")
			}
		}
		rules[name] = r
	}
	context, ok := env.Property("context")
	if !ok {
		panic("envelope: schema has no context")
	}
	if contextEntries, ok = context.MaxProperties(); !ok || contextEntries <= 0 {
		panic("envelope: schema context has no maxProperties")
	}
	values := context.AdditionalProperties()
	if values == nil || values.OnViolation() != "truncate" || values.MaxBytes() <= 0 {
		panic("envelope: schema context values do not truncate at x-max-bytes")
	}
	contextValueBytes = values.MaxBytes()
	if recommended = env.Recommended(); len(recommended) == 0 {
		panic("envelope: schema has no x-recommended")
	}
}
