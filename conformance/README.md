# Conformance kit

The executable part of the v1 contract. `docs/openapi.yaml` and the schemas
under `schemas/` say what the API looks like; the fixtures here say what a
server must do with a body, byte for byte. Any implementation of the write
path, in any language, must pass every fixture unchanged.

```bash
python3 conformance/reference/fixtures.py                 # run every fixture through the reference
python3 conformance/reference/fixtures.py --write <dir>   # print what the reference makes of a fixture's body
just contract                                             # the full contract gate (schemas, OpenAPI, examples, fixtures)
```

## Layout

| Path | What |
|---|---|
| `decode/rows/<nn>-<slug>/` | one fixture per row of the inference table, numbered in the table's order |
| `decode/interactions/<slug>/` | cases where rows combine, and the cases the contract names one by one (null members, empty members, reserved-name collisions, UTF-8 matrix, pointer escaping, `occurred_at` forms, kind-schema guides) |
| `hash/<slug>/` | canonical-JSON and `content_hash` vectors, one per rule of the identity section |
| `warnings.json` | the closed list of warning codes, one example each, and the keyword-to-code mapping for kind schemas |
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
- **Hash.** Serialise the identity input (`kind`, `schema_version`,
  `machine`, `model`, `harness`, `project`, `summary`, `payload`, absent
  members omitted) as canonical JSON, compare bytes with `canonical.json`,
  and compare its SHA-256 hex digest with `sha256`.

Pointers are RFC 6901 JSON Pointers into the stored record and name where a
member's content ended up: a moved member at `/payload/<name>` or
`/payload/moved/<name>`, an unparseable `occurred_at` at
`/context/occurred_at_raw`, a member that overflowed the context at
`/payload/context_overflow/<name>`; a missing recommended member points where
it would be.

## The reference implementation

`reference/` shares no code with the Go service. It exists so that the
fixtures are verified by two independent implementations: the reference in
this directory and the Go decoder in its own tests. A fixture is authored by
hand from the contract, then checked against the reference; disagreement
means one of the two is wrong and is settled by reading the contract, never
by regenerating the fixture from either implementation.

Facts the reference pins that a host language may get wrong:

- Invalid UTF-8 becomes one U+FFFD per maximal ill-formed subsequence
  (Unicode Standard chapter 3, table 3-8), the behaviour of WHATWG
  `TextDecoder` and Python; a byte-at-a-time decoder produces more.
- Whitespace is the Unicode `White_Space` property (25 code points); U+FEFF
  is not whitespace. Lower-casing is the simple, per-code-point mapping:
  U+0130 becomes U+0069 alone, a final capital sigma becomes U+03C3.
  JavaScript's `toLowerCase` and Python's `str.lower` are full mappings and
  differ on both. Unicode 15.0 is the reference; fixtures only use code
  points whose properties are unchanged since Unicode 6.0.
- Numbers are kept as their source spelling; `JSON.parse` and `json.loads`
  cannot, so an implementation needs a token-level parser.
- Duplicate member names compare after unescaping (`"a"` and `"a"`
  are the same member); the last value wins.
