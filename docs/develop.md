# Develop AgentFeedback

How to change the service and the skills, verify, and release. Read
[api.md](api.md) first if the change touches the HTTP surface.

## Layout

```
cmd/agentfeedback/              main: `serve` (default), `import <jsonl>`, `backup <dest.db>`
internal/api/                   HTTP: mux, middleware (auth, recovery, request id, log, metrics, body limits), handlers, DTOs
internal/core/                  validation, per-family canonical hashing, create/list/get/processed/export
internal/store/                 SQLite: open + pragmas, embedded forward-only migrations, hand-written SQL
internal/canonjson/             canonical JSON for event hashing
infra/agentfeedback/            compose stacks (local build, image-based deploy) and .env.example
scripts/                        e2e.sh (live contract suite), deploy.sh, release.py,
                                eval-cluster.py (live cluster.py calibration; discloses report text)
skills/agentfeedback/           submit/query/process client skill (copied as-is into a harness; no tests inside)
skills/agentfeedback-triage/    processor skill (SKILL.md, digest.sh; optional cluster.py + reference/clustering.md)
tests/skill/                    hermetic tests for both skills' scripts (mock server, isolated HOME)
docs/                           api.md (contract of the running service), openapi.yaml (v1 contract of the next major release), operate.md, develop.md, security.md, releases.md
schemas/                        JSON Schema 2020-12: the submission envelope and the kind schemas (friction, review)
conformance/                    the executable contract: decode fixtures, hash vectors, the warning list, a Python reference implementation (README inside)
```

Go toolchain and module versions are pinned in `go.mod`. Tools: `just`,
`shellcheck`, `python3`, `uv` and `npx` (contract gate), Docker (only for the
compose stack and image).

## Commands

```bash
just check          # gofmt, go vet, go mod tidy, build — the pre-commit gate
just test           # go test -race -count=1 ./...  (SQLite on temp files; no services needed)
just run-local      # serve on 127.0.0.1:8090 with a database in ./local/
bash scripts/e2e.sh <API_KEY> [BASE_URL]      # live contract suite against a running service
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
  code, [api.md](api.md), `scripts/e2e.sh`, the client scripts in
  `skills/agentfeedback/scripts/`, and `tests/skill/`. Producers build their
  calls from api.md without reading the code.
- **Write-once payloads.** Only `processed_at` and `resolution` ever change
  after insert. Never add an update path for content; a correction is a new
  submission.
- **The contract is files first.** `docs/openapi.yaml`, `schemas/` and
  `conformance/` are the normative v1 contract; Go code and any second
  implementation follow them, never the reverse. A contract change edits
  those files and the fixtures in the same commit, and `just contract` must
  stay green. Fixtures are written by hand from the contract and checked
  against `conformance/reference/`; a disagreement is settled by reading the
  contract, never by regenerating a fixture from an implementation. Until the
  v1 server packages land, the running service still implements
  [api.md](api.md).
- **Hash forms are frozen.** Frictions and reviews hash the exact Go structs
  from API 1.0 (field order matters); events hash canonical JSON. Changing
  either turns every stored row into a replay mismatch. A test pins a fixed
  vector for each.
- **Migrations are forward-only and append-only.** New numbered file under
  `internal/store/migrations/`, applied in one transaction, version recorded
  in `schema_version`. A binary that meets a newer schema refuses to start.
- **Payloads pass through as raw JSON.** Never decode a stored payload into
  `map[string]any` on the way out; it changes large integers.
- **Metrics labels are bounded.** Route pattern, allow-listed method, status
  code. Never a raw path, never client input.
- **Docs are written for agents first.** Lead with the command, state the
  rule, skip the anecdote. One doc per task, no duplicated facts; keep the
  README route table true. Nothing machine-, user- or organization-specific.
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
  release.

## Verification before you are done

1. `just check` clean.
2. `go test -race -count=1 ./...` green.
3. API touched: build, serve on a temp database, `bash scripts/e2e.sh` all
   green.
4. Skill scripts touched: the shellcheck command above and `bash tests/skill/run-tests.sh`
   all green.
5. Docs touched: every relative link resolves.
6. Contract files touched (`schemas/`, `docs/openapi.yaml`, `conformance/`):
   `just contract` green.

CI runs the same gates and, on `main`, publishes the image as
`ghcr.io/agentfeedback/agentfeedback:<sha>` and `:latest`.

## Release

Every delivered change ships in a release; versioning rules and steps are in
[releases.md](releases.md).
