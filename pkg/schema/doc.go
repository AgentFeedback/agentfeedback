// Package schema is the schema engine of the v1 contract: it embeds the files
// under schemas/, compiles them once at package init (no I/O at call time),
// implements the custom x- keywords, exposes the text primitives the decoder
// applies to envelope members, and validates a payload against a kind schema
// as a guide.
//
// # Guide validation
//
// A kind schema never rejects. Its transforming keywords (x-trim,
// x-normalize: token, x-on-violation: truncate) rewrite the payload in place,
// silently, before identity; every other violation becomes one Detail whose
// code comes from the keyword that failed (conformance/warnings.json lists the
// mapping). A member the schema does not list is kept and reported as
// unknown_field, except the top-level members inference placed, which the
// caller names in placed. A type mismatch stops further checks on that
// member. The walker descends only where the schema has a subschema, so its
// recursion is bounded by the schema, not by the payload.
//
// # Value model
//
// Guide, Validate and every helper here work on the JSON tree the decoder
// produces: map[string]any, []any, string, json.Number, bool and nil. Numbers
// are json.Number carrying the source spelling and are never converted; type
// integer means a number with a zero fraction, whatever its spelling, and
// range and enum comparisons are decimal comparisons of the spelling. The tree
// is the write path's working form: it is canonicalised before storage and
// never rebuilt on the way out. Any other Go value passed to the guide is a
// programming error and panics.
//
// # Explicit kinds only
//
// Validate expects a kind the producer sent, token-normalised. An inferred
// kind (unknown, with missing_kind) is never validated: the decoder must not
// call Validate for it, because the answer would be a spurious no_schema.
//
// # Strictness at init
//
// A shipped schema is compiled against an allow-list of keywords and value
// shapes identical to the reference implementation's. A file that uses
// anything else fails at package init, so no keyword is ever silently
// ignored. Metaschema validity of the files is the contract gate's job (CI).
package schema
