# Conformance kit

The executable part of the v1 contract. `docs/openapi.yaml` and the schemas
under `schemas/` say what the API looks like; this directory says what a
server must do with a body, byte for byte. Any implementation of the write
path, in any language, must pass every fixture unchanged. The rules the
fixtures pin are written out below; the fixtures are the test of them.

```bash
python3 conformance/reference/fixtures.py                 # run every fixture through the reference
python3 conformance/reference/fixtures.py --write <dir>   # print what the reference makes of a fixture's body
just contract                                             # the full contract gate (schemas, OpenAPI, examples, fixtures)
```

## Layout

| Path | What |
|---|---|
| `decode/rows/<nn>-<slug>/` | one fixture per row of the inference table below, numbered in the table's order |
| `decode/interactions/<slug>/` | rows combined, and the cases the rules name one by one (null members, empty members, reserved-name collisions, the UTF-8 matrix, pointer escaping, `occurred_at` forms, nesting depth, kind-schema guides) |
| `hash/<slug>/` | canonical-JSON and `content_hash` vectors, one per rule of the identity section below |
| `warnings.json` | the closed list of warning codes, one example each, and the keyword-to-code mapping for kind schemas |
| `manifest.json` | every fixture directory by name, and the nesting limit; the contract check refuses a tree that differs from it |
| `reference/` | a standard-library Python implementation of the write path (decode, normalise, guide, canonical JSON, hash) that every fixture is checked against in CI |

## Fixture files

Every fixture is a directory.

| File | Content |
|---|---|
| `body.json` | the raw request body, exactly the bytes a client sent; may be invalid JSON, contain invalid UTF-8 or duplicate member names |
| `body.gen.json` | instead of `body.json` for large bodies: `{"prefix", "pad", "suffix", "total_bytes"}`; the body is prefix, then the one-byte `pad` repeated, then suffix, `total_bytes` long |
| `expected.json` | decode: `{"row", "status", "error"?, "warnings"?, "note"?}`; hash: `{"rule", "sha256", "note"?}` |
| `stored.json` | decode fixtures with status 201: the stored envelope (every envelope member the server keeps, server fields excluded) as canonical JSON bytes |
| `canonical.json` | hash fixtures: the canonical JSON bytes of the identity input |

`.gitattributes` marks the directory `-text` so the bytes survive checkout on
every platform. Files carry no trailing newline where the bytes matter
(`stored.json`, `canonical.json`, raw bodies).

## How an implementation is compared

- **Status and error.** `status` is the HTTP status; `error` is the `error`
  member of the error body for 400 and 413. Identity (200 on replay, 409 on
  mismatch) needs stored state and is outside this kit; every accepted body
  here is a 201.
- **Stored envelope.** Serialise the stored envelope as canonical JSON and
  compare bytes with `stored.json`. Canonical form sorts members, so the
  comparison is order-independent, and it keeps number spellings, so
  `1.0` and `1` are different results.
- **Warnings.** Compare the multiset of `(code, pointer)` pairs with
  `expected.json`'s `warnings`; order and `message` are not compared.
  Messages are advisory, including the "did you mean" hint.
- **Hash.** Serialise the identity input as canonical JSON, compare bytes
  with `canonical.json`, and compare its SHA-256 hex digest with `sha256`.

## The write path

### Rejections

Only four bodies are rejected; everything else is stored.

| Body | Response |
|---|---|
| more than 10485760 bytes (checked before parsing; exactly 10485760 is accepted) | 413 `request_too_large` |
| not RFC 8259 JSON | 400 `bad_request` |
| JSON but not an object | 400 `bad_request` |
| containers nested deeper than 512 levels, the body object being level 1 | 400 `bad_request` |

### Inference table

Applied in this order. Every applicable row fires, so one member can carry
several warnings. Member names match case-sensitively; when an unknown
member matches an envelope member case-insensitively (`Kind`), the warning
message says `did you mean kind`.

| Input | Result | Warning |
|---|---|---|
| invalid UTF-8 in a string or member name | one U+FFFD per maximal ill-formed subsequence (Unicode Standard ch. 3, table 3-8) | `invalid_utf8`, one per affected string |
| escaped lone surrogate (`\ud800`) | U+FFFD | `invalid_utf8` |
| duplicate member name in any object (names compare after unescaping: `"a"` and `"a"` are the same member) | last value wins; warnings about the discarded value are dropped | `duplicate_key` |
| envelope member that is JSON `null` | treated as absent; the absent rules then apply | `coerced` |
| `payload` present but not an object | wrapped as `{"value": <it>}` | `payload_wrapped` |
| top-level member the envelope does not know | moved into `payload`, one member at a time in code point order of their names | `moved_to_payload` |
| moved member, `context_raw` or `context_overflow` whose name already exists in `payload` | stored under `payload.moved.<name>`; a non-object `payload.moved` is wrapped first (`payload_wrapped`); a value already under `payload.moved.<name>` is replaced (`duplicate_key`) | `moved_to_payload` (`truncated` for `context_overflow`) |
| `payload` absent | the moved members become the payload; none: `{}` | `payload_inferred` |
| `kind` absent, null, empty after normalisation, or not a string | `unknown` | `missing_kind` (and nothing else about `kind`) |
| `schema_version` absent or null | `1` | none (null: `coerced` only) |
| `schema_version` not spelled `^[1-9][0-9]*$` or above 2^53−1 (`2.0`, `1e0`, `"2"`, `0`, a 5000-digit number) | `1` | `schema_version_defaulted` |
| `key`, `summary`, `machine`, `model`, `harness`, `project` or `occurred_at` of the wrong JSON type | encoded as canonical JSON into a string, then treated as a string; warnings about the value's parts collapse onto the member | `coerced` |
| `summary`, `machine`, `model`, `harness`, `project` over their byte limit (2000, 200, 200, 200, 200) | truncated | `truncated` |
| `summary` with newlines | CR LF, lone CR and lone LF each become one space | none |
| `occurred_at` unparseable | dropped; the text kept as `context.occurred_at_raw` (replacing a producer's own with `duplicate_key`); its warnings move there | `invalid_format` |
| `context` not an object | moved to `payload.context_raw` | `moved_to_payload` |
| `context` over 32 entries (after `occurred_at_raw` was added) | the first 32 by key in code point order kept; the rest moved to `payload.context_overflow` as an object | `truncated` |
| `context` value not a string | encoded as canonical JSON into a string | `coerced` |
| `context` value over 2000 bytes | truncated | `truncated` |

`key`, `kind` and context keys have no byte limit and are never truncated;
an empty context key is kept. The value of a non-string `kind` is not kept.

### Normalisation

After inference, each string member goes through these steps once, in this
order: (1) trim, (2) in `summary`, newlines to spaces, (3) token
normalisation for `kind` and `harness` (trim, lower-case, runs of whitespace
to one `-`), (4) truncation at the last complete code point at or before the
byte limit. Nothing is re-trimmed after a cut, so a token cut at its limit
may end in `-`. A member that is empty after step (3) is omitted silently
(`missing_recommended` still applies to `summary`, `machine` and `model`;
an empty `key` means keyless).

`occurred_at` is an RFC 3339 section 5.6 date-time with ASCII digits, `T`
and `Z` in either case, no space separator, no leap second (`:60` does not
parse), `-00:00` read as UTC; fractional digits beyond six are truncated; a
UTC result outside years 0001 to 9999 does not parse. It is stored as
`YYYY-MM-DDTHH:MM:SS.ffffffZ`, the year always four digits.

### Kind schemas as guides

When the server ships a schema for `(kind, schema_version)`, the payload is
validated against it after normalisation. Nothing is refused:

- `x-trim`, `x-normalize` and `x-on-violation: truncate` inside the schema
  rewrite the payload silently, before identity.
- Every other failing keyword produces one warning at the member's pointer;
  `warnings.json` `keyword_codes` maps the keyword to the code. `type
  integer` accepts any number with a zero fraction (`2.0`); a type mismatch
  stops further checks on that member. `x-max-bytes` without truncate keeps
  the string whole with `too_long`.
- A member the schema does not list is kept with `unknown_field`, except
  members inference placed (`moved`, `context_raw`, `context_overflow`,
  `value` and moved names), which were reported once already.
- `x-unique-by` compares string values only; a duplicate is reported on the
  later item (`duplicate_slot` at `/payload/reviewers/1`).
- `x-recommended` in the envelope and in the schema gives `missing_recommended`
  for each absent member, pointing where it would be.

An explicit kind the server has no schema for gets `no_schema`; a known kind
with a version the server does not ship gets `unknown_schema_version`; an
inferred `unknown` kind gets `missing_kind` alone.

### Pointers

`pointer` is an RFC 6901 JSON Pointer (`~0`, `~1` escaping) into the stored
record, naming where a member's content ended up: a moved member at
`/payload/<name>` or `/payload/moved/<name>`, an unparseable `occurred_at`
at `/context/occurred_at_raw`, a member that overflowed the context at
`/payload/context_overflow/<name>`, a member coerced to a string at the
member itself. A missing recommended member points where it would be.

## Identity

`content_hash` is the SHA-256 hex digest of the canonical JSON of the object
holding `kind`, `schema_version`, `machine`, `model`, `harness`, `project`,
`summary` and `payload` after inference, normalisation and the kind-schema
transforms; absent members are omitted and `schema_version` is always
present. `key`, `occurred_at`, `context` and every server field are outside
identity.

Canonical JSON:

- UTF-8; no whitespace outside strings.
- Object members sorted by key in code point order (the byte order of the
  UTF-8 keys) at every level; U+1F600 sorts after U+FB01, unlike a UTF-16
  comparison. Arrays keep their order.
- Strings: `"` as `\"`, `\` as `\\`, U+000A as `\n`, U+000D as `\r`, U+0009
  as `\t`, every other code point below U+0020 (including U+0008 and
  U+000C) as `\u00xx` with lower-case hex, everything else raw: `/`,
  `<`, `>`, `&`, U+007F, U+2028, U+2029, every non-ASCII code point.
- Numbers verbatim as received (`1.0`, `1e2`, `1E2`, `-0`, `0.10`, a
  30-digit integer all stay as spelled); `schema_version` is the one
  exception, written as the plain decimal integer the decoder accepted.
- `true`, `false` and `null` as such; a `null` inside `payload` is content.

## Text rules

- Whitespace, for trimming and for the token rule, is the Unicode
  `White_Space` property: U+0009–U+000D, U+0020, U+0085, U+00A0, U+1680,
  U+2000–U+200A, U+2028, U+2029, U+202F, U+205F, U+3000 (unchanged since
  Unicode 6.3). U+FEFF is not whitespace. JavaScript's `trim()` does not
  remove U+0085.
- Lower-casing is the simple, context-free mapping of each code point from
  Unicode 15.0: U+0130 becomes U+0069 alone (the full mapping, which
  JavaScript's `toLowerCase` and Python's `str.lower` apply, gives two code
  points, so an implementation on those needs an override), a capital sigma
  becomes U+03C3 wherever it stands (full mappings give U+03C2 at the end of
  a word), KELVIN SIGN becomes `k`. Fixtures use only code points whose
  properties are unchanged since Unicode 6.3, so an implementation on newer
  tables passes.
- Invalid UTF-8 becomes one U+FFFD per maximal ill-formed subsequence, the
  behaviour of WHATWG `TextDecoder` and Python; a byte-at-a-time decoder
  produces more.
- Numbers keep their source spelling, so an implementation needs a
  token-level parser; `JSON.parse` and `json.loads` cannot.

## The reference implementation

`reference/` shares no code with the Go service. It exists so that the
fixtures are verified by two independent implementations: the reference in
this directory and the Go decoder in its own tests. A fixture is authored by
hand from the rules above, then checked against the reference; a
disagreement means one of the two is wrong and is settled by reading the
rules, never by regenerating the fixture from either implementation.
