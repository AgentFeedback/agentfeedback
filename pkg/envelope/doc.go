// Package envelope is the decoder of the v1 contract: a raw request body in,
// the stored envelope and its warnings out. It implements the write path
// that conformance/README.md specifies and the fixtures under conformance/
// pin: the token-stream parse, the inference table, the normalisation
// order, the kind schema as a guide and the recommended-member check.
//
// # Rejections
//
// Decode refuses exactly four bodies, each as a *Rejection carrying the
// contract's status and error code: over BodyLimit bytes (413
// request_too_large); not RFC 8259 JSON, JSON but not an object, or
// containers nested deeper than MaxDepth (400 bad_request). Every other JSON
// object is stored, whatever it contains; what the envelope cannot use moves
// into the payload with a warning.
//
// # Parsing
//
// The parser is the contract's own token reader. It keeps what encoding/json
// loses: the source spelling of numbers (json.Number, never converted),
// duplicate member names (the last value wins, one duplicate_key, and the
// warnings about the discarded value go with it) and the exact strings where
// invalid UTF-8 or an escaped lone surrogate was replaced by U+FFFD (one
// replacement per maximal ill-formed subsequence, the WHATWG decoder's
// behaviour, and one invalid_utf8 per affected string or member name).
//
// # Value model
//
// The payload is the JSON tree pkg/schema and pkg/canonjson work on:
// map[string]any, []any, string, json.Number, bool and nil. Envelope holds
// the members the server keeps as Go fields; Tree returns the same envelope
// as that tree, which is what identity and the store consume. The payload
// map is shared between the two, not copied, and the decoder never touches
// the result again once Decode returns; a caller that mutates it owns the
// consequences.
//
// # Limits
//
// The byte limits, the trim and token rules, the truncation policy, the
// context entry and value limits and the recommended list are read from the
// compiled envelope schema at package init, which panics if the schema lacks
// a member or a rule this decoder depends on. The body limit, the nesting
// limit, the schema_version spelling rule and its maximum are constants
// stated by the contract's prose and checked against conformance/ by the
// tests.
//
// Decode takes the whole body as bytes, so it can only reject an oversize
// body after the caller read it. The HTTP layer reads through a limiter of
// BodyLimit+1 bytes so that the limit bounds memory as well.
package envelope
