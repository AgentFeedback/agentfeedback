# Develop AgentFeedback

How to change the service and the skills, verify, and release. Read
[api.md](api.md) first if the change touches the HTTP surface.

## Layout

```
cmd/agentfeedback/              main: bare invocation prints help; server `serve` (internal/api), `import [--dry-run] <export.ndjson>` (restore keeping ids), `backup <dest.db>`; client `doctor`, `doctor --init`, `submit` (friction|review|<kind>, --stdin, --dry-run), `flush`, `version`, `schema`, `skill`, read and processing `list`, `get`, `stats`, `done`, `undo`, `redact`, `rekind`, `export` (streamed, trailer verified), `digest`, settings resolved flag > AGENT_FEEDBACK_* env > config.toml (API key: env > config only)
internal/api/                   v1 HTTP transport over internal/core: mux, middleware, query grammar, Problem mapping, bundled openapi.json; conformance test against docs/openapi.yaml
internal/core/                  v1 service, no net/http: create with identity and dedupe, get, list, marks, redaction, stats, export, import and restore of format 2, meta; typed problems
internal/store/                 SQLite for the v1 API: open + pragmas + the application_id stamp, the single init migration, hand-written SQL, query plans pinned by a test
internal/skillgen/               skill generator: source/ (skill.json plus one Markdown fragment per teaching point) rendered into skill-md, agents-md, cursor, prompt and mcp; `skill render`
pkg/schema/                     v1 schema engine: embedded schemas compiled at init, the x- keywords, guide validation, the text and date-time rules
pkg/envelope/                   v1 decoder: token-stream parse (spellings, duplicates, UTF-8 repair), inference table, normalisation order, guide and recommended checks; the content_hash member set
pkg/canonjson/                  v1 canonical JSON writer on the write path's JSON tree and its SHA-256; identity hashes and stored bytes are written with it
pkg/client/                     v1 client transport: both auth headers, no redirects, the retry table, the spool (spool/, rejected/ beside it, retention), outcome lines and exit codes, the owner-only client.jsonl; `Do` and `Stream` for every other route (`APIError`, `TransportError`)
pkg/collect/                    client context collection: project, machine and harness groups (git metadata with or without git, env allow-list looked up by name), deny_paths/opt-in narrowing, repository .agentfeedback.toml that may only narrow
infra/agentfeedback/            compose stacks (local build, image-based deploy) and .env.example
scripts/                        e2e.sh (live v1 contract suite: every openapi.yaml operation, fails on an uncovered one), gate-e2e.sh, deploy.sh, release.py,
                                eval-cluster.py (live cluster.py calibration; discloses report text)
skills/agentfeedback/           submission skill: SKILL.md generated from internal/skillgen/source; scripts/ is the bash client of the running service, documented in scripts/README.md (copied as-is into a harness; no tests inside)
skills/agentfeedback-triage/    processor skill (SKILL.md, digest.sh; optional cluster.py + reference/clustering.md)
tests/skill/                    hermetic tests for both skills' scripts (mock server, isolated HOME)
docs/                           api.md (contract of the running service), openapi.yaml (v1 contract of the next major release; embed.go makes it a Go package for internal/api), operate.md, develop.md, security.md, releases.md
schemas/                        JSON Schema 2020-12: the submission envelope and the kind schemas (friction, review); embed.go makes them a Go package for pkg/schema
conformance/                    the executable contract: decode fixtures, hash vectors, the warning list, a Python reference implementation (README inside)
```

Go toolchain and module versions are pinned in `go.mod`. Tools: `just`,
`shellcheck`, `python3`, `uv` and `npx` (contract gate), Docker with buildx
(compose stack, image, `just image-push`).

## Commands

```bash
just ci             # every gate of "Verification before you are done", in order; there is no hosted CI, this is the merge gate
just check          # gofmt, go vet, go mod tidy, build — the pre-commit gate
just test           # go test -race -count=1 ./...  (SQLite on temp files; no services needed)
just fuzz           # go test -fuzz=FuzzDecode -fuzztime=30s ./pkg/envelope: the decoder on top of its seed corpus (every fixture body)
just skills         # regenerate the checked-in skill renders (skills/agentfeedback/SKILL.md) from internal/skillgen/source
just e2e            # live contract suite against a fresh `serve` on a temporary database (scripts/gate-e2e.sh, port 18080, E2E_ADDR overrides)
just run-local      # serve on 127.0.0.1:8090 with a database in ./local/
just image-push <tag>...                      # multi-arch image to ghcr.io/agentfeedback/agentfeedback; release step only
bash scripts/e2e.sh <API_KEY> [BASE_URL]      # live v1 contract suite against a running service; creates rows
bash tests/skill/run-tests.sh                 # hermetic client tests (mock server, needs python3)
just contract                                 # contract gate: the two commands below
uv run --locked --script scripts/contract-check.py   # schemas valid 2020-12, every example validates, fixtures agree with conformance/reference
npx --yes @redocly/cli@2.54.2 lint docs/openapi.yaml # OpenAPI lint (recommended ruleset, redocly.yaml)
python3 conformance/reference/fixtures.py     # the fixtures alone, no dependencies
shellcheck -x -P SCRIPTDIR skills/*/scripts/*.sh tests/skill/run-tests.sh
python3 scripts/eval-cluster.py <export.ndjson> <labels.json> --allow-repo <remote>... [--live]   # cluster.py calibration
```

## Rules that are not visible in the code

- **An API change is a five-artifact change**, in one commit: `internal/`
  code, the contract files, `scripts/e2e.sh`, the client scripts in
  `skills/agentfeedback/scripts/`, and `tests/skill/`. Producers build their
  calls from the contract without reading the code. `scripts/e2e.sh` fails
  on any `docs/openapi.yaml` operation it does not exercise, so a new route
  needs its check in the same commit.
- **Write-once payloads.** After insert only the processing fields
  (`processed_at`, `verdict`, `resolution`, `ref`, `processed_by`) change,
  and a redaction replaces the payload with its tombstone. Never add an
  update path for content; a correction is a new submission.
- **The contract is files first.** `docs/openapi.yaml`, `schemas/` and
  `conformance/` are the normative v1 contract; Go code and any second
  implementation follow them, never the reverse. A contract change edits
  those files and the fixtures in the same commit, and `just contract` must
  stay green. Fixtures are written by hand from the contract and checked
  against `conformance/reference/`; a disagreement is settled by reading the
  contract, never by regenerating a fixture from an implementation. `serve`
  implements `docs/openapi.yaml`; [api.md](api.md) describes the v3 service
  of the latest release until the docs are rewritten for v1.
- **One implementation of the contract's text and schema rules.**
  `pkg/schema` owns trimming, token normalisation, byte truncation,
  date-time parsing, RFC 6901 escaping and the x- keywords, and it works on
  the JSON tree the decoder produces (`map[string]any`, `[]any`, `string`,
  `json.Number`, `bool`, `nil`; numbers keep their spelling). The decoder,
  the CLI and the HTTP layer call it and never re-implement a rule. Its tests
  read `conformance/` directly; a fixture change is a test change.
- **Hash forms are frozen.** `content_hash` is the SHA-256 of the canonical
  JSON of the identity members; `conformance/hash/` pins it and
  `pkg/envelope` and `internal/core` run every vector. Changing it turns
  every stored row into a replay mismatch.
- **Migrations are forward-only and append-only.** New numbered file under
  `internal/store/migrations/`, applied in one transaction, version recorded
  in `schema_version`. A binary that meets a newer schema refuses to start,
  and so does one that meets a database without the `application_id` stamp
  `Open` writes before the first migration (a database of the previous
  major version, which is not migrated: start from a new `DATABASE_PATH`).
- **Payloads pass through as raw JSON.** Never decode a stored payload into
  `map[string]any` on the way out; it changes large integers.
- **Metrics labels are bounded.** Route pattern, allow-listed method, status
  code. Never a raw path, never client input.
- **Docs are written for agents first.** Lead with the command, state the
  rule, skip the anecdote. One doc per task, no duplicated facts; keep the
  README route table true. Nothing machine-, user- or organization-specific.
- **Skill renders are generated.** `skills/agentfeedback/SKILL.md` is the
  `skill-md` render of `internal/skillgen/source/`; edit the fragments, run
  `just skills`, and commit both. Text outside a `<!-- only: … -->` block
  reaches every form; the prompt form assumes nothing but HTTP, and no
  checked-in render names a server URL.
- **Skill directories are copied as-is into harnesses.** No tests or tooling inside
  `skills/*/`; tests live in `tests/skill/`. Script comments state rules, not
  history: no dates, incident numbers or machine names.
- **Clustering changes are measured.** Changing `cluster.py`'s instructions,
  criteria, threshold or batching means rerunning `scripts/eval-cluster.py`
  and updating the calibration section of
  `skills/agentfeedback-triage/reference/clustering.md` (the one dated
  statement a skill carries). Ship a prompt change only when the eval
  supports it. The eval discloses report text: it needs the queue owner's
  approval for every exact remote (`--allow-repo`), and without `--live` it
  only lists what would be sent.
- **Triage is user-invoked only.** Keep `disable-model-invocation: true` and
  a description that forbids loading it from phrasing about the queue.
- **Compatibility.** Everything in API 1.0 keeps working. Additive changes
  bump the API minor in api.md's version line and "Changes" section; anything else is a major
  release. A server change that breaks older clients also raises
  `core.ClientMinVersion`, which `/api/v1/meta` serves as `client.min_version`.

## Verification before you are done

1. `just check` clean.
2. `just skills` leaves every checked-in render unchanged (`git diff --exit-code`).
3. `just test` (`go test -race -count=1 ./...`) green.
4. Decoder touched (`pkg/envelope/`): `just fuzz` green; a crash it finds is
   committed under `pkg/envelope/testdata/fuzz/FuzzDecode/` as a regression
   seed beside the fix.
5. API touched: `just e2e` green (builds, serves on a temp database, runs
   `scripts/e2e.sh`, which also fails on an uncovered operation).
6. Skill scripts touched: the shellcheck command above and `bash tests/skill/run-tests.sh`
   all green.
7. Docs touched: every relative link resolves.
8. Contract files touched (`schemas/`, `docs/openapi.yaml`, `conformance/`):
   `just contract` green.

`just ci` runs all of the above in order and fails if `just check` rewrote a
file. There is no hosted CI: `just ci` green on the tree that is merged is the
merge gate, and the image is built and published by the release step
([releases.md](releases.md)). A change that adds a gate adds it to the `ci`
recipe in the `justfile` and to the list above, in the same commit.

## Release

Every delivered change ships in a release, which a maintainer cuts when they
decide; versioning rules and steps are in [releases.md](releases.md).
