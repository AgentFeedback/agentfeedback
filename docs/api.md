# AgentFeedback HTTP API

The HTTP API of an AgentFeedback server: write-once submissions from AI
agents, their processing marks, aggregates, export and import, schemas,
remote MCP and discovery. This page is the narrative. The contract is
[openapi.yaml](openapi.yaml), the JSON Schemas under [`schemas/`](../schemas)
and the conformance kit under `conformance/` in the repository; they
win wherever this page disagrees, and they hold the field-by-field tables.

API version: **1.0**. Changes since 1.0 are listed under [Changes](#changes).

Contents:

1. [Who needs HTTP](#who-needs-http)
2. [Base URL, auth, common rules](#base-url-auth-common-rules)
3. [Submissions](#submissions): envelope, identity, warnings
4. Routes: [create](#create), [list](#list), [get](#get),
   [mark one](#mark-one), [mark a batch](#mark-a-batch), [redact](#redact),
   [stats](#stats), [export](#export), [import](#import),
   [schemas](#schemas), [meta](#meta), [OpenAPI document](#openapi-document),
   [MCP](#mcp), [skill](#skill), [discovery](#discovery),
   [health, ready, metrics](#health-ready-metrics)
5. [Evolution policy](#evolution-policy) and [version axes](#version-axes)
6. [Changes](#changes)

## Who needs HTTP

The `agentfeedback` CLI and its stdio MCP tools (`agentfeedback mcp`) are the
normal clients, and they need no server: in local mode they run the same
handler in-process on the data-directory database
([operate.md](operate.md#local-mode-no-server)). HTTP is for a server started
with `agentfeedback serve`, and for producers that cannot run the binary.

Hand-written calls follow the generated recipes
[recipes/http-curl.md](recipes/http-curl.md) and
[recipes/http-powershell.md](recipes/http-powershell.md); they need a shell or
an HTTP tool that can set a request header.

The v3 HTTP API of releases before 4.0.0 is documented at the v3.0.0 tag:
https://github.com/AgentFeedback/agentfeedback/blob/v3.0.0/docs/api.md

## Base URL, auth, common rules

```bash
export AGENT_FEEDBACK_URL=http://127.0.0.1:8090    # the server's base URL
export AGENT_FEEDBACK_API_KEY=<key>                # the server's API_KEY
```

- **Auth.** Send the key in either header; either authorises and neither
  shadows the other:
  `Authorization: Bearer $AGENT_FEEDBACK_API_KEY` (scheme name
  case-insensitive) or `X-Api-Key: $AGENT_FEEDBACK_API_KEY`. Every
  `/api/v1/*` route and `/mcp` need it, except these, which need no key and
  carry no data: `GET /api/v1/schemas`, `GET /api/v1/schemas/{kind}/{version}`,
  `GET /api/v1/openapi.json`, `GET /skill`,
  `GET /.well-known/agentfeedback.json`, `GET /health`, `GET /ready`,
  `GET /metrics`. A missing or wrong key is 401 `unauthorized` with
  `WWW-Authenticate: Bearer`.
- **Headers.** Every response carries `X-Request-Id`. Every response of an
  authenticated route, success or error, carries `Cache-Control: no-store`.
- **Body limits.** Create, mark and batch mark: 10485760 bytes
  (`limits.body_bytes` in [meta](#meta)). Import: 33554432 bytes
  (`limits.import_bytes`). MCP: 10551296 bytes. Over the limit is 413
  `request_too_large`.
- **Errors.** `application/json` with `error` (a code from a closed list),
  `message` (names the field or parameter, the value received when short,
  and the accepted range), `request_id` (equals `X-Request-Id`) and, when
  there is something to point at, `details`: items of `code`, `pointer`,
  `message`. This is the `Error` schema in
  [openapi.yaml](openapi.yaml), not RFC 9457 problem details. A wrong
  method is 405 `method_not_allowed` with `Allow`; an unknown path under
  `/api/v1/` is 404 `not_found`. `rate_limited` (429, with `Retry-After`)
  occurs on the hosted service only; `unavailable` (503) carries
  `Retry-After` when known.
- **Timestamps.** Every timestamp the server writes is RFC 3339 UTC with six
  fractional digits, e.g. `2026-09-27T10:00:01.123456Z`. Inputs (`since`,
  `until`, `occurred_at`) accept any RFC 3339 offset; percent-encode `+` in
  a query string.
- **Write-once.** Content is never modified after creation. The only fields
  that change are the five processing fields (`processed_at`, `verdict`,
  `resolution`, `ref`, `processed_by`), set by [marking](#mark-one), and
  redaction, which replaces content with a tombstone ([redact](#redact)).
- **Optional members** of a record are omitted when unset, never sent as
  `null`.

Example error, a key missing:

<!-- example: createSubmission response 401 -->
```json
{
  "error": "unauthorized",
  "message": "missing or invalid API key; send Authorization: Bearer <key> or X-Api-Key: <key>",
  "request_id": "02c20310252d79435a7d839aa39802e3"
}
```

## Submissions

A submission is any JSON object. The server never rejects a body for its
shape: the [envelope schema](../schemas/envelope.v1.json) describes what is
stored after inference, and deviations are normalised and reported as
warnings. The envelope members are `kind`, `schema_version`, `key`,
`summary`, `machine`, `model`, `harness`, `project`, `occurred_at`, `context`
and `payload`; `payload` is validated as a guide against the kind schema when
the server ships one ([`friction`](../schemas/kinds/friction.v1.json),
[`review`](../schemas/kinds/review.v1.json)). The stored record adds `id`,
`uid` (a UUIDv7), `content_hash`, `created_at`, the processing fields and
`redacted_at` (the `Submission` schema in [openapi.yaml](openapi.yaml)).

**Decoding and inference.** Only four bodies are rejected: over 10485760
bytes (413), not JSON (400 `bad_request`), JSON but not an object (400),
nesting deeper than 512 levels (400). Everything else is stored: an unknown
top-level member moves into `payload`, an absent `payload` is built from the
moved members, a missing `kind` becomes `unknown`, over-long strings are
truncated, an envelope member of the wrong type is encoded into a string
(`coerced`), and a payload that fails its kind schema is kept as sent, with
warnings. The ordered rules, the normalisation
steps and the fixtures every implementation must pass are in
`conformance/README.md` ("The write path").

**Identity.** `content_hash` is the SHA-256 of the canonical JSON of `kind`,
`schema_version`, `machine`, `model`, `harness`, `project`, `summary` and
`payload` after inference (`conformance/README.md`, "Identity").
`key`, `occurred_at` and `context` are outside identity. A create answers:

| Case | Status |
|---|---|
| new record | 201, `Location` of the new record |
| `key` already stored under the same `kind` with the same `content_hash` (keyed replay) | 200, `Location` and body of the existing record |
| no `key`, and an unprocessed record with the same `content_hash` was created within `dedupe_window_s` (86400 s, in [meta](#meta)) | 200, the existing record (keyless duplicate) |
| `key` already stored under the same `kind` with a different `content_hash` | 409 `replay_mismatch`; `details[0].existing_id` names the stored record |

A keyless duplicate of a processed record, or one outside the window, is a
new record: a recurring problem files again after triage. A keyed replay of a
redacted record still answers 200 with the tombstone, because the hash is
kept.

**Warnings.** A create returns `warnings` beside the stored record: items of
`code`, `pointer` (an RFC 6901 pointer into the stored record) and `message`.
Warnings are never stored and never reject. The closed list of codes, one
example each, is `conformance/warnings.json` in the repository.

## Routes

### Create

`POST /api/v1/submissions`

```bash
curl -sS -X POST "$AGENT_FEEDBACK_URL/api/v1/submissions" \
  -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data @friction.json
```

<!-- example: createSubmission request -->
```json
{
  "kind": "friction",
  "key": "friction-workstation-a-019250f2",
  "summary": "README install step 3 references a flag that no longer exists",
  "machine": "workstation-a",
  "model": "claude-fable-5-1",
  "harness": "claude-code",
  "project": "example",
  "occurred_at": "2026-09-27T09:58:12Z",
  "context": {"git_commit": "a1b2c3d", "client": "agentfeedback/4.0.0", "origin": "agent"},
  "payload": {
    "category": "documentation",
    "details": "Step 3 says --init; the flag was removed in 3.0.",
    "fix_status": "applied",
    "fix_ref": "example@a1b2c3d"
  }
}
```

201 with `Location: /api/v1/submissions/44`; the same body again is 200 with
the same record:

<!-- example: createSubmission response 201 -->
```json
{
  "submission": {
    "id": 44,
    "uid": "019250f2-8c3e-7a4b-9d1e-2f3a4b5c6d7e",
    "kind": "friction",
    "schema_version": 1,
    "key": "friction-workstation-a-019250f2",
    "summary": "README install step 3 references a flag that no longer exists",
    "machine": "workstation-a",
    "model": "claude-fable-5-1",
    "harness": "claude-code",
    "project": "example",
    "occurred_at": "2026-09-27T09:58:12.000000Z",
    "context": {"client": "agentfeedback/4.0.0", "git_commit": "a1b2c3d", "origin": "agent"},
    "payload": {
      "category": "documentation",
      "details": "Step 3 says --init; the flag was removed in 3.0.",
      "fix_ref": "example@a1b2c3d",
      "fix_status": "applied"
    },
    "content_hash": "157cf352c13503c9cac14d40ba9ab1bccd2dd8c6bb6567903a9b68881bc79c04",
    "created_at": "2026-09-27T10:00:01.123456Z"
  },
  "warnings": []
}
```

A flat body with no `kind` and no `payload` is stored too, with warnings:

```json
{"summary": "the linter ignores its config file", "category": "tooling", "Model": "claude-fable-5-1", "machine": "workstation-a"}
```

<!-- example: createSubmission response 201 -->
```json
{
  "submission": {
    "id": 45,
    "uid": "019250f4-3d1a-7e52-8c0b-6f1e2d3c4b5a",
    "kind": "unknown",
    "schema_version": 1,
    "summary": "the linter ignores its config file",
    "machine": "workstation-a",
    "payload": {"Model": "claude-fable-5-1", "category": "tooling"},
    "content_hash": "e4e4baa28c3c46a2cd031183d2c1fc85710c9ffb7d593ca9e84302d288f677c2",
    "created_at": "2026-09-27T10:02:40.654321Z"
  },
  "warnings": [
    {"code": "missing_kind", "pointer": "/kind", "message": "kind is missing, empty or not a string; stored as unknown"},
    {"code": "payload_inferred", "pointer": "/payload", "message": "payload was absent; built from the members the envelope does not know"},
    {"code": "moved_to_payload", "pointer": "/payload/Model", "message": "Model is not an envelope member; moved into payload; did you mean model"},
    {"code": "moved_to_payload", "pointer": "/payload/category", "message": "category is not an envelope member; moved into payload"},
    {"code": "missing_recommended", "pointer": "/model", "message": "model is recommended"}
  ]
}
```

The same `key` with different content:

<!-- example: createSubmission response 409 -->
```json
{
  "error": "replay_mismatch",
  "message": "key \"friction-workstation-a-019250f2\" was already used with different content",
  "request_id": "cac965ab7ef7e9962133bac8aa047267",
  "details": [
    {
      "code": "key_reused",
      "pointer": "/key",
      "message": "key \"friction-workstation-a-019250f2\" already names submission 44 with different content",
      "existing_id": 44
    }
  ]
}
```

A body that is JSON but not an object:

<!-- example: createSubmission response 400 -->
```json
{
  "error": "bad_request",
  "message": "body is a JSON value but not an object",
  "request_id": "ec90a683216a22de8d72bdac3f86862b"
}
```

### List

`GET /api/v1/submissions`

<!-- example: listSubmissions -->
```bash
curl -sS -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  "$AGENT_FEEDBACK_URL/api/v1/submissions?processed=false&limit=1"
```

<!-- example: listSubmissions response 200 -->
```json
{
  "submissions": [
    {
      "id": 45,
      "uid": "019250f4-3d1a-7e52-8c0b-6f1e2d3c4b5a",
      "kind": "unknown",
      "schema_version": 1,
      "summary": "the linter ignores its config file",
      "machine": "workstation-a",
      "content_hash": "e4e4baa28c3c46a2cd031183d2c1fc85710c9ffb7d593ca9e84302d288f677c2",
      "created_at": "2026-09-27T10:02:40.654321Z"
    }
  ],
  "limit": 1,
  "total": 2,
  "has_more": true,
  "next_before_id": 45,
  "next_after_id": null
}
```

- **Filters**, all optional, combined with AND: `kind` (`unknown` selects
  un-kinded rows), `schema_version`, `key`, `machine`, `model`, `project`,
  `harness`, `category` and `fix_status` (friction payload members; other
  kinds never match), `origin` (`context.origin`), `exclude_kind`
  (repeatable), `verdict`, `processed` (`false` is the triage queue),
  `redacted` (default: both), `content_hash`, `since` and `until` (RFC 3339,
  inclusive) on the timestamp `on` names (`created_at`, the default, or
  `occurred_at`), and `q`: a case-insensitive substring over `summary` and
  every string value in `payload`, at most 200 bytes. Exact semantics per
  parameter are in [openapi.yaml](openapi.yaml).
- **Strict query.** An unknown parameter name, an empty value or a repeated
  singleton parameter is 400 `validation_error` naming the parameter.
- **Rows** are the record minus `payload`; `include=payload` adds it.
- **Paging.** `limit` is 1–500 (1–100 with `include=payload`), default 50;
  out of range is 400. Without a cursor the page is newest first.
  `before_id=N` pages newest first through `id < N`; `after_id=N` pages
  oldest first through `id > N`; the two are mutually exclusive. When
  `has_more` is true exactly one of `next_before_id` / `next_after_id` is
  set, matching the direction used; pass it back as the same cursor.
  `total` counts every row matching the filters, ignoring cursors and
  `limit`, in the same read transaction as the page.
- **Draining the queue.** Page with `processed=false` following
  `next_before_id` until `has_more` is false. Marking a row on the current
  page does not shift the next one, because the cursor is an id. This is a
  traversal for one processor at a time, not a lease.

<!-- example: listSubmissions response 400 -->
```json
{
  "error": "validation_error",
  "message": "limit must be between 1 and 500, got 900",
  "request_id": "fe5ba4936b4c150d3fe3e06686ec4b1e",
  "details": [
    {"code": "out_of_range", "pointer": "?limit", "message": "limit must be between 1 and 500, got 900"}
  ]
}
```

### Get

`GET /api/v1/submissions/{id}` returns the bare record. `{id}` must match
`^[1-9][0-9]*$`, else 400; an absent record is 404 `not_found`.

<!-- example: getSubmission -->
```bash
curl -sS -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  "$AGENT_FEEDBACK_URL/api/v1/submissions/44"
```

<!-- example: getSubmission response 200 -->
```json
{
  "id": 44,
  "uid": "019250f2-8c3e-7a4b-9d1e-2f3a4b5c6d7e",
  "kind": "friction",
  "schema_version": 1,
  "key": "friction-workstation-a-019250f2",
  "summary": "README install step 3 references a flag that no longer exists",
  "machine": "workstation-a",
  "model": "claude-fable-5-1",
  "harness": "claude-code",
  "project": "example",
  "occurred_at": "2026-09-27T09:58:12.000000Z",
  "context": {"client": "agentfeedback/4.0.0", "git_commit": "a1b2c3d", "origin": "agent"},
  "payload": {
    "category": "documentation",
    "details": "Step 3 says --init; the flag was removed in 3.0.",
    "fix_ref": "example@a1b2c3d",
    "fix_status": "applied"
  },
  "content_hash": "157cf352c13503c9cac14d40ba9ab1bccd2dd8c6bb6567903a9b68881bc79c04",
  "created_at": "2026-09-27T10:00:01.123456Z"
}
```

<!-- example: getSubmission response 404 -->
```json
{
  "error": "not_found",
  "message": "submission 99 not found",
  "request_id": "a68bfb0dfdae5f7b79b9c6d402361bca"
}
```

### Mark one

`PATCH /api/v1/submissions/{id}` sets the processing mark and returns the
record; 404 when absent.

```bash
curl -sS -X PATCH "$AGENT_FEEDBACK_URL/api/v1/submissions/44" \
  -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data @mark.json
```

<!-- example: markSubmission request -->
```json
{
  "processed": true,
  "verdict": "fixed",
  "resolution": "already applied by the reporter",
  "ref": "example@a1b2c3d",
  "processed_by": "workstation-b/triage-2026-09-27"
}
```

<!-- example: markSubmission response 200 -->
```json
{
  "id": 44,
  "uid": "019250f2-8c3e-7a4b-9d1e-2f3a4b5c6d7e",
  "kind": "friction",
  "schema_version": 1,
  "key": "friction-workstation-a-019250f2",
  "summary": "README install step 3 references a flag that no longer exists",
  "machine": "workstation-a",
  "model": "claude-fable-5-1",
  "harness": "claude-code",
  "project": "example",
  "occurred_at": "2026-09-27T09:58:12.000000Z",
  "context": {"client": "agentfeedback/4.0.0", "git_commit": "a1b2c3d", "origin": "agent"},
  "payload": {
    "category": "documentation",
    "details": "Step 3 says --init; the flag was removed in 3.0.",
    "fix_ref": "example@a1b2c3d",
    "fix_status": "applied"
  },
  "content_hash": "157cf352c13503c9cac14d40ba9ab1bccd2dd8c6bb6567903a9b68881bc79c04",
  "created_at": "2026-09-27T10:00:01.123456Z",
  "processed_at": "2026-09-27T14:02:00.000000Z",
  "verdict": "fixed",
  "resolution": "already applied by the reporter",
  "ref": "example@a1b2c3d",
  "processed_by": "workstation-b/triage-2026-09-27"
}
```

- The body is validated strictly: an unknown member, a wrong type or a
  `null` `processed` is 400 `validation_error`. No content field is
  writable.
- `processed` defaults to `true`. `verdict`, `resolution`, `ref` and
  `processed_by` are allowed only with `processed=true`.
- `verdict` is a token of at most 64 bytes; documented values: `fixed`,
  `invalid`, `duplicate`, `wont_fix`, `deferred`, `upstream`,
  `unverifiable`. `resolution` is at most 2000 bytes, blank is 400. `ref`
  and `processed_by` are at most 200 bytes. A `ref` that points at another
  submission holds that row's `uid`, never its `id`.
- Marking sets `processed_at` if unset. Re-marking with a different
  `verdict`, `resolution` or `ref` replaces them and keeps the original
  `processed_at`. `processed=false` clears all five processing fields.
  Redacted rows can be marked.

### Mark a batch

`POST /api/v1/submissions/processed`: the same fields and rules as
[mark one](#mark-one), plus `ids` (1–500 positive integers, duplicates
collapsed). The answer classifies every id.

```bash
curl -sS -X POST "$AGENT_FEEDBACK_URL/api/v1/submissions/processed" \
  -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"ids": [44, 45, 999], "processed": true, "verdict": "fixed"}'
```

<!-- example: markSubmissions request -->
```json
{"ids": [44, 45, 999], "processed": true, "verdict": "fixed"}
```

<!-- example: markSubmissions response 200 -->
```json
{"processed": true, "verdict": "fixed", "updated": [45], "unchanged": [44], "not_found": [999]}
```

`updated`: the row changed. `unchanged`: already in the requested state.
`not_found`: no such id.

### Redact

`DELETE /api/v1/submissions/{id}` replaces the record with a tombstone:
`payload` becomes `{"redacted": true}`, `summary` and `context` are removed,
`redacted_at` is set. `id`, `uid`, `kind`, `schema_version`, `key`,
`machine`, `model`, `harness`, `project`, `occurred_at`, `content_hash`,
`created_at` and the processing fields stay. Repeating it is 200 unchanged;
404 when absent. Redaction is not reversible.

```bash
curl -sS -X DELETE -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  "$AGENT_FEEDBACK_URL/api/v1/submissions/45"
```

<!-- example: redactSubmission response 200 -->
```json
{
  "id": 45,
  "uid": "019250f4-3d1a-7e52-8c0b-6f1e2d3c4b5a",
  "kind": "unknown",
  "schema_version": 1,
  "machine": "workstation-a",
  "payload": {"redacted": true},
  "content_hash": "e4e4baa28c3c46a2cd031183d2c1fc85710c9ffb7d593ca9e84302d288f677c2",
  "created_at": "2026-09-27T10:02:40.654321Z",
  "processed_at": "2026-09-27T14:05:00.000000Z",
  "redacted_at": "2026-09-27T14:10:00.000000Z",
  "verdict": "fixed"
}
```

### Stats

`GET /api/v1/stats` aggregates over the same filters as [list](#list)
(everything except the cursors, `limit` and `include`).

<!-- example: getStats -->
```bash
curl -sS -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  "$AGENT_FEEDBACK_URL/api/v1/stats?by=project,category&top=1&bucket=week"
```

<!-- example: getStats response 200 -->
```json
{
  "total": 245,
  "open": 61,
  "processed": 184,
  "redacted": 2,
  "groups": [
    {"keys": {"project": "example", "category": "documentation"}, "total": 40, "open": 12, "processed": 28}
  ],
  "recurring": [
    {
      "content_hash": "157cf352c13503c9cac14d40ba9ab1bccd2dd8c6bb6567903a9b68881bc79c04",
      "count": 7,
      "first_id": 44,
      "last_id": 1187,
      "summary": "README install step 3 references a flag that no longer exists",
      "kind": "friction",
      "project": "example"
    }
  ],
  "series": [
    {"bucket": "2026-09-21", "total": 31, "open": 4}
  ]
}
```

- `by`: a comma list of up to three of `kind`, `project`, `category`,
  `fix_status`, `machine`, `model`, `harness`, `verdict`,
  `schema_version`, `origin`. Absent: no `groups`. Groups are sorted by
  `total` descending, at most 100; a row without a value for a listed key is
  in no group, so group totals need not add up to `total`.
- `top`: 0–50, default 10; how many recurring content hashes to return.
- `bucket`: `day` or `week` adds a time series on the `on` timestamp.
- Invalid parameters are 400 `validation_error`.

### Export

`GET /api/v1/export` streams NDJSON, export format 2
(`Content-Type: application/x-ndjson`), in one read transaction, ascending
`id`. `HEAD /api/v1/export` answers the same headers with no body.

<!-- example: exportSubmissions -->
```bash
curl -sS -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  "$AGENT_FEEDBACK_URL/api/v1/export?after_id=43" > feedback.ndjson
```

<!-- example: exportSubmissionsHead -->
```bash
curl -sS -I -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  "$AGENT_FEEDBACK_URL/api/v1/export"
```

<!-- example: exportSubmissions -->
```text
{"export_format":2,"kind":null,"since":null,"after_id":43,"exported_at":"2026-09-27T15:00:00.000000Z"}
{"id":44,"uid":"019250f2-8c3e-7a4b-9d1e-2f3a4b5c6d7e","kind":"friction","schema_version":1,"key":"friction-workstation-a-019250f2","summary":"README install step 3 references a flag that no longer exists","machine":"workstation-a","model":"claude-fable-5-1","harness":"claude-code","project":"example","occurred_at":"2026-09-27T09:58:12.000000Z","context":{"client":"agentfeedback/4.0.0","git_commit":"a1b2c3d","origin":"agent"},"payload":{"category":"documentation","details":"Step 3 says --init; the flag was removed in 3.0.","fix_ref":"example@a1b2c3d","fix_status":"applied"},"content_hash":"157cf352c13503c9cac14d40ba9ab1bccd2dd8c6bb6567903a9b68881bc79c04","created_at":"2026-09-27T10:00:01.123456Z","processed_at":"2026-09-27T14:02:00.000000Z","verdict":"fixed","resolution":"already applied by the reporter","ref":"example@a1b2c3d","processed_by":"workstation-b/triage-2026-09-27"}
{"id":45,"uid":"019250f4-3d1a-7e52-8c0b-6f1e2d3c4b5a","kind":"unknown","schema_version":1,"machine":"workstation-a","payload":{"redacted":true},"content_hash":"e4e4baa28c3c46a2cd031183d2c1fc85710c9ffb7d593ca9e84302d288f677c2","created_at":"2026-09-27T10:02:40.654321Z","processed_at":"2026-09-27T14:05:00.000000Z","redacted_at":"2026-09-27T14:10:00.000000Z","verdict":"fixed"}
{"export_complete":true,"count":2,"first_id":44,"last_id":45,"sha256":"dc5af905018c8d65122241337d76b7326e4eb44758113483577274e0a2898076"}
```

- **Header** (line 1): `export_format`, then the filters echoed (`kind`,
  `since`, `after_id`; `null` when unset) and `exported_at`.
- **Records**: one full record per line; tombstones are exported as
  tombstones.
- **Trailer** (last line): `export_complete`, `count`, `first_id`,
  `last_id` and `sha256`, the lowercase hex SHA-256 over the record lines
  only, each with its trailing newline, header and trailer excluded, in
  order. A stream without the trailer, or whose count or digest disagrees
  with it, is damaged.
- **Filters**: `kind`, `since` (inclusive, on `created_at`), `after_id`
  (`id > after_id`), `limit` (1–500; absent means everything). Page a large
  export with `after_id` set to the previous trailer's `last_id`.

Backups, restore and migration with the CLI are in
[operate.md](operate.md#restore-and-migration).

### Import

`POST /api/v1/import` takes an export-format-2 NDJSON body
(`Content-Type: application/x-ndjson`, at most 33554432 bytes).

<!-- example: importSubmissions -->
```bash
curl -sS -X POST "$AGENT_FEEDBACK_URL/api/v1/import" \
  -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  -H 'Content-Type: application/x-ndjson' \
  --data-binary @feedback.ndjson
```

<!-- example: importSubmissions response 200 -->
```json
{
  "imported": 1,
  "skipped": 1,
  "conflicts": [
    {"line": 2, "uid": "019250f2-8c3e-7a4b-9d1e-2f3a4b5c6d7e", "existing_id": 12, "reason": "key_mismatch"}
  ],
  "warnings": [],
  "first_id": 301,
  "last_id": 301
}
```

- The whole body is read and verified (count, digest, one JSON object per
  record line) before anything is written. A record whose `uid` is not an
  RFC 9562 UUID, or whose `created_at`, `occurred_at`, `processed_at` or
  `redacted_at` does not parse as RFC 3339, is 400 `validation_error` naming
  the line, and nothing is written.
- Envelope members pass through the same normalisation and limits as
  create; violations are `warnings` with a `line`, never rejections.
- `content_hash` is recomputed for ordinary records and must equal the
  exported value, else 400 naming the line; tombstones keep the exported
  hash.
- Each imported record gets a new `id`; `uid`, `created_at`, `occurred_at`
  and the processing fields are kept.
- An existing `uid` is skipped and counted in `skipped`; redactions and
  processing marks never propagate to a stored row. An existing
  `(kind, key)` under a different `uid` is skipped and counted when the
  `content_hash` matches, otherwise skipped, counted and listed under
  `conflicts`. A conflict never fails the request.

### Schemas

`GET /api/v1/schemas` lists the kinds and versions the server knows,
including `envelope`. No key.

<!-- example: listSchemas -->
```bash
curl -sS "$AGENT_FEEDBACK_URL/api/v1/schemas"
```

<!-- example: listSchemas response 200 -->
```json
{
  "schemas": [
    {"kind": "envelope", "versions": [1]},
    {"kind": "friction", "versions": [1]},
    {"kind": "review", "versions": [1]}
  ]
}
```

`GET /api/v1/schemas/{kind}/{version}` returns one JSON Schema document
verbatim, `Content-Type: application/schema+json`; `envelope` is a valid
`{kind}`. 404 when the pair is unknown. No key. The body is the file under
[`schemas/`](../schemas).

<!-- example: getSchema -->
```bash
curl -sS "$AGENT_FEEDBACK_URL/api/v1/schemas/friction/1"
```

### Meta

`GET /api/v1/meta` returns versions, limits, kinds and features.
`client.min_version` is the oldest client release that speaks this API,
raised only when a server change breaks older clients; a client older than
it should report plainly that it is too old. `client.latest_known` is the
server version.

<!-- example: getMeta -->
```bash
curl -sS -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  "$AGENT_FEEDBACK_URL/api/v1/meta"
```

<!-- example: getMeta response 200 -->
```json
{
  "service_version": "4.0.0",
  "api_version": "1.0",
  "export_format": 2,
  "dedupe_window_s": 86400,
  "limits": {
    "body_bytes": 10485760,
    "import_bytes": 33554432,
    "list_max": 500,
    "list_max_with_payload": 100,
    "processed_ids_max": 500,
    "context_entries": 32,
    "context_value_bytes": 2000,
    "identifier_bytes": 200,
    "summary_bytes": 2000
  },
  "kinds": [
    {"kind": "friction", "versions": [1]},
    {"kind": "review", "versions": [1]}
  ],
  "features": ["q", "stats", "export.after_id", "export.limit", "import", "mcp", "redaction"],
  "client": {"min_version": "4.0.0-rc.0", "latest_known": "4.0.0"}
}
```

### OpenAPI document

`GET /api/v1/openapi.json` returns [openapi.yaml](openapi.yaml) as JSON, in
bundled form with the referenced schema files inlined. No key.

<!-- example: getOpenApi -->
```bash
curl -sS "$AGENT_FEEDBACK_URL/api/v1/openapi.json"
```

### MCP

`POST /mcp` is a remote MCP server over the MCP Streamable HTTP transport,
authenticated like `/api/v1/*`. `POST /mcp/{project}` is the same with
`project` preset for the connection. The request needs
`Content-Type: application/json` (else 415) and an `Accept` header naming
both `application/json` and `text/event-stream` (else 400). The answer is a
JSON-RPC message, as `application/json` or as a server-sent event stream; a
notification or response from the client is 202 with no body. The body limit
is 10551296 bytes. The tools, their arguments and how the preset scopes them
are in [operate.md](operate.md#mcp-and-discovery).

```bash
curl -sS -X POST "$AGENT_FEEDBACK_URL/mcp" \
  -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}'
```

<!-- example: postMcp request -->
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": {
    "protocolVersion": "2025-06-18",
    "capabilities": {},
    "clientInfo": {"name": "curl", "version": "1"}
  }
}
```

<!-- example: postMcp -->
```text
event: message
data: {"jsonrpc":"2.0","id":1,"result":{"capabilities":{"logging":{},"tools":{"listChanged":true}},"instructions":"# AgentFeedback\n\n…","protocolVersion":"2025-06-18","serverInfo":{"name":"agentfeedback","version":"4.0.0"}}}
```

<!-- example: postMcpProject request -->
```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "submit_feedback",
    "arguments": {"kind": "friction", "summary": "make test needs port 5432 free and says nothing when it is taken", "category": "tooling"}
  }
}
```

<!-- example: postMcpProject -->
```bash
curl -sS -X POST "$AGENT_FEEDBACK_URL/mcp/example" \
  -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  --data @tools-call.json
```

### Skill

`GET /skill` returns the submission guidance as Markdown with this server's
base URL substituted. No key. `format`: `skill-md` (default; the guidance
runs the binary), `agents-md`, or `prompt` (HTTP instructions inline: the
[curl recipe](recipes/http-curl.md) with the server's URL).

<!-- example: getSkill -->
```bash
curl -sS "$AGENT_FEEDBACK_URL/skill?format=prompt"
```

### Discovery

`GET /.well-known/agentfeedback.json` says where the OpenAPI document,
schemas, MCP endpoint and skill live. No key, no data, no secrets.

<!-- example: getDiscovery -->
```bash
curl -sS "$AGENT_FEEDBACK_URL/.well-known/agentfeedback.json"
```

<!-- example: getDiscovery response 200 -->
```json
{
  "service": "agentfeedback",
  "version": "4.0.0",
  "api_version": "1.0",
  "openapi": "/api/v1/openapi.json",
  "schemas": "/api/v1/schemas",
  "mcp": "/mcp",
  "skill": "/skill",
  "auth": {"modes": ["bearer", "x-api-key"]},
  "docs": "https://agentfeedback.dev/docs"
}
```

### Health, ready, metrics

No key; keep them inside the deployment boundary.

<!-- example: getHealth -->
```bash
curl -sS "$AGENT_FEEDBACK_URL/health"    # 200 OK: the process is alive
```

<!-- example: getReady -->
```bash
curl -sS "$AGENT_FEEDBACK_URL/ready"     # 200 READY; 503 unavailable while not ready
```

<!-- example: getMetrics -->
```bash
curl -sS "$AGENT_FEEDBACK_URL/metrics"   # Prometheus text exposition format
```

Metric labels are the route pattern, an allow-listed method and the status
code, never a raw path or client input. What to alert on is in
[operate.md](operate.md#monitoring).

## Evolution policy

Applies to this contract: [openapi.yaml](openapi.yaml), the schemas under
[`schemas/`](../schemas) and the conformance kit under
`conformance/`. Producers and consumers can rely
on it.

- Additive changes never bump a kind's `schema_version`: a new optional
  payload member, a new `context` key, a new documented value of an existing
  member. `friction` `evidence[]` and the `context` keys `origin`, `detector`
  and `session_harness` were added this way to version 1.
- A `schema_version` bump means a member changed meaning or shape. The new
  version is a new schema file beside the old one; a server that does not
  ship a version stores its payload unvalidated with an
  `unknown_schema_version` warning.
- Unknown fields are stored, never rejected: an unknown payload member of a
  kind the server ships a schema for is kept with an `unknown_field` warning
  (a kind without one is stored unvalidated with `no_schema`), an unknown
  top-level member is moved into `payload`, an unknown `context` key is kept
  as sent.
- `context` stays a flat map of strings (32 entries, values up to 2000
  bytes) about how and where a submission came to be. Structured content
  goes in `payload`. `context` is not part of `content_hash`, so a new key
  leaves a submission's identity unchanged while the context stays within 32
  entries; entries beyond the first 32 move into `payload.context_overflow`,
  which is part of identity.
- A documented vocabulary in a `context` value (`origin`: `agent`,
  `hook-nudge`, `session-scan`, `inbox`, `import`) is not enforced: another
  value is stored as sent, without a warning.

## Version axes

The values of this contract, release 4.0.0 and later. Each axis moves on its
own; none implies another.

| Axis | Current | Where it is read | Changes when |
|---|---|---|---|
| API | `1.0` | `info.version` in [openapi.yaml](openapi.yaml); `api_version` in `GET /api/v1/meta` | a route, parameter or response member is added (minor) or changed (major) |
| Envelope | v1 | `$id` of `schemas/envelope.v1.json` | the envelope's members change meaning; additions stay v1 |
| Kind payload | `schema_version` per kind, `1` for `friction` and `review` | the submission's `schema_version`; `kinds` in `GET /api/v1/meta` | a payload member changes meaning or shape (see [Evolution policy](#evolution-policy)) |
| Export format | `2` | `export_format` in the export header line and in `GET /api/v1/meta` | the NDJSON export lines change shape |
| Binary | the release version, e.g. `4.0.0` | `agentfeedback version`; `service_version` in `GET /api/v1/meta` | every release; `client.min_version` in `GET /api/v1/meta` rises only when a server change breaks older clients |
| Skills | `metadata.version` in each `SKILL.md` | the skill's front matter | the commands or rules a skill teaches change |

## Changes

None since API 1.0.
