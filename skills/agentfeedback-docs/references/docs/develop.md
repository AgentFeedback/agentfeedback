# Develop AgentFeedback

How to change the service and the skills, verify, and release. Read
[api.md](api.md) first if the change touches the HTTP surface.

## Layout

```
cmd/agentfeedback/              main: bare invocation prints help; server `serve` (internal/api; key from API_KEY or API_KEY_FILE), `serve --init` (api-key and serve.env), `server install --systemd|--launchd|--compose` (writes one service file, starts nothing), `import [--dry-run] <export.ndjson>` (restore keeping ids), `backup <dest.db>`; client `doctor`, `doctor --init`, `doctor --e2e` (submit, list, mark one install-check row, no spool), `submit` (friction|review|<kind>, --stdin, --dry-run), `flush`, `hook <harness> <event>` (what the harness hooks run: per-session failure counts over internal/detect, the note in the harness's output form, the end-of-turn flush in remote mode), `ingest` (the file inbox, also delivered by the start-up pass of every local-mode command), `mcp` (the MCP tools over stdio on the local database, plus the session tools; refused in remote mode), `sessions` (list, digest, mark, status over internal/sessions, always on the data-directory database; `submit --context-from` files from a digest event), `ui` (read-only loopback web page over internal/ui), `version`, `schema`, `skill` (render, render docs, reminder), `prime` (the guidance the session-start hooks print), `install` and `uninstall` (wire the skill, the hooks, the rule section or the MCP entry into the coding-agent harnesses through internal/harness; `install --check` reads the rule sections, `install --project` writes the pointer section into a repository's instruction file), `flush --hook` (what older installs wired), read and processing `list`, `get`, `stats`, `done`, `undo`, `redact`, `rekind`, `export` (streamed, trailer verified), `digest`, `migrate` (to cloud or a URL through its import route, target key from stdin), settings resolved flag > AGENT_FEEDBACK_* env > config.toml (API key: env > config only); no url configured means local mode: the same commands over the API handler in-process on the data-directory database (internal/localmode), `--local` and `--server URL` override per invocation; coverage.toml maps every OpenAPI operation, parameter and body property to a command, flag or argument, or lists it with a reason, and its [local] table lists every flag and argument of each `sessions` subcommand, `install` and `prime` (checked by coverage_test.go, also against the [local] tools of internal/mcp/coverage.toml)
internal/api/                   v1 HTTP transport over internal/core: mux, middleware, query grammar, Problem mapping, bundled openapi.json, /mcp (key, body limit, Error shape around internal/mcp), /skill and discovery; conformance test against docs/openapi.yaml
internal/mcp/                   MCP server over internal/core (go-sdk): NewServer builds one connection's server, served stateless over Streamable HTTP by Handler and over stdio by `agentfeedback mcp`; six tools answering with the REST bodies, strict argument decoding, the /mcp/{project} preset, server instructions from internal/skillgen; mounted by internal/api; coverage.toml maps every OpenAPI operation, parameter and body property to a tool and argument, or lists it with a reason (checked by coverage_test.go), its [transports] table names both transports, which coverage_test.go checks register the same tools, and its [local] table lists the session tools only stdio registers (Config.Sessions), each mapped to a `sessions` subcommand and its flags
internal/core/                  v1 service, no net/http: create with identity and dedupe, get, list, marks, redaction, stats, export, import and restore of format 2, meta; typed problems
internal/store/                 SQLite for the v1 API: open + pragmas + the application_id stamp, the numbered migrations (forward-only), hand-written SQL, query plans pinned by a test
internal/ui/                    the read-only loopback web page of `agentfeedback ui`: its own handler (Host, Origin, launch token, GET only), html/template views and static assets by go:embed, reading through pkg/client
internal/harness/               harness wiring for install and uninstall: the adapter registry (one entry per harness, with its verification record), a byte-preserving JSON/JSONC editor, the Codex TOML block, the marked rule section in global instruction files, backups, atomic writes and the install.json manifest
internal/detect/                the hook's rule, no net/http: each harness's hook payload parsed into an event (never a file it names), per-session failure counters in the cache directory under a file lock, the nudge thresholds, the note text (under 600 bytes) and each harness's output form
internal/sessions/              session logs of the harnesses, read on demand: one reader per log format (Claude Code, Codex, Copilot CLI, Gemini CLI, OpenCode), the labelled corpus test, the [collect] policy per session before a log is opened, states and watermarks in the data-directory database, the bounded scrubbed digest, Locate and Key for submit --context-from
internal/localmode/              the local-mode target: the v1 API handler in-process over a core.Service on the data-directory database, served to pkg/client through its transport seam; no /mcp, metrics or health routes; MCPServer builds the stdio MCP server over the same service
internal/skillgen/               skill generator: source/ (skill.json plus one Markdown fragment per teaching point) rendered into skill-md, agents-md, cursor, prompt (curl), prompt-powershell and mcp; source/docs.json and the reference files rendered into the docs skill; source/plugin.json rendered into the agent-plugin bundle and the marketplace root; `skill render`
pkg/schema/                     v1 schema engine: embedded schemas compiled at init, the x- keywords, guide validation, the text and date-time rules
pkg/envelope/                   v1 decoder: token-stream parse (spellings, duplicates, UTF-8 repair), inference table, normalisation order, guide and recommended checks; the content_hash member set
pkg/canonjson/                  v1 canonical JSON writer on the write path's JSON tree and its SHA-256; identity hashes and stored bytes are written with it
pkg/client/                     v1 client transport: both auth headers, no redirects, the retry table, the spool in the data directory (spool/, rejected/ beside it, every entry bound to its destination, retention), outcome lines and exit codes, the owner-only client.jsonl in the cache directory; `Do` and `Stream` for every other route (`APIError`, `TransportError`)
pkg/scrub/                      known secret formats in text replaced with [REDACTED:<class>] and counted per class: String, Tree (in place on the JSON tree), JSON (token level, spellings kept); used by submit --scrub and INGEST_SCRUB
pkg/collect/                    client context collection: project, machine and harness groups (git metadata with or without git, env allow-list looked up by name), deny_paths/opt-in narrowing, repository .agentfeedback.toml that may only narrow
infra/agentfeedback/            compose stacks (local build, image-based deploy) and .env.example
scripts/                        in-container.sh (runs a command in the gate toolchain image), live-harness.sh (the live Claude Code check), e2e.sh (live v1 contract suite: every openapi.yaml operation, fails on an uncovered one), gate-e2e.sh, deploy.sh, release.sh (the release step behind
                                `just release`), eval-cluster.py (live cluster.py calibration; discloses report text), playbooks.py (the playbook gate: routes through both
                                install playbooks, the Verify condition of each step, `check`, `list`, `run`)
references.go                   embeds the docs, schemas, OpenAPI document and install playbooks for the docs skill (go:embed cannot reach the root from internal/)
.goreleaser.yaml                release build: six archives, install.sh asset, SHA256SUMS over both (docs/releases.md)
AGENT-INSTALL.md                client install playbook for agents: one command, one verification, one JSON outcome per step
AGENT-INSTALL-STACK.md          stack install playbook for agents: server and client on one machine; includes AGENT-INSTALL.md by step number
llms.txt                        index for agents: raw URLs of both playbooks, docs/openapi.yaml, the schemas and docs/api.md
skills/agentfeedback/           submission skill: SKILL.md generated from internal/skillgen/source, and scripts/install.sh, which installs the release binary (copied as-is into a harness; no tests inside)
skills/agentfeedback-docs/      reference skill for integrators and operators, generated by `skill render docs` (installed by `install --docs`)
plugins/agentfeedback/          Agent Plugins bundle (plugin.json, .claude-plugin/plugin.json, skills/agentfeedback/), generated by `skill render marketplace`; no mcp.json: `skill render agent-plugin --server URL` adds one for a single server
.claude-plugin/marketplace.json Claude Code marketplace listing plugins/agentfeedback/, generated by `skill render marketplace`
.agents/plugins/marketplace.json Codex marketplace listing plugins/agentfeedback/, generated by `skill render marketplace`
skills/agentfeedback-triage/    processor skill on the agentfeedback CLI or its stdio MCP tools (SKILL.md routing to playbooks/, six playbooks, fix-it-session.md starting from the session logs on an empty queue; optional scripts/cluster.py + reference/clustering.md)
tests/skill/                    hermetic tests: run-tests.sh for install.sh (offline fixture release), test_cluster.py for cluster.py (no requests), triage-playbooks.py runs every triage playbook's bash blocks against a throwaway server, and the fix-it session's start (bash blocks step by step, then its MCP-only tool calls against `agentfeedback mcp`) on an empty queue over a fixture session store per harness
tests/playbooks/                test_playbooks.py for scripts/playbooks.py (stub playbooks, no Docker); Dockerfile of the gate's clean machine (systemd, users stack and client)
tests/ci/                       Dockerfile of the gate toolchain image: the Go toolchain go.mod pins, Node, uv, just, shellcheck and the Claude Code CLI; base images pinned by digest, CLIs by version
tests/live/                     claude-code.sh, the live harness check's assertions (run by scripts/live-harness.sh)
docs/                           api.md (contract of the running service), openapi.yaml (v1 contract of the next major release; embed.go makes it a Go package for internal/api), operate.md, develop.md, security.md, releases.md; recipes/ holds http-curl.md and http-powershell.md, the prompt forms rendered by `just skills`
schemas/                        JSON Schema 2020-12: the submission envelope and the kind schemas (friction, review); embed.go makes them a Go package for pkg/schema
conformance/                    the executable contract: decode fixtures, hash vectors, the warning list, a Python reference implementation (README inside)
```

Go toolchain and module versions are pinned in `go.mod`. Tools: `just`, `git`,
`python3` and Docker on Linux, rootful and without user-namespace remapping
(the gates run in containers as your uid, and the uid mapping assumes
container uid equals host uid; `scripts/in-container.sh` refuses anything
else; buildx for the image); everything else the gates call is in the
toolchain image (`tests/ci/Dockerfile`). `just ci-host` and the bare recipes also need `go`,
`shellcheck`, `uv`, `npx` and `jq` on the host. GoReleaser at the version
[releases.md](releases.md) pins, for the release step only, which also builds
and pushes the image.

## Commands

```bash
just ci             # the merge gate: ci-host inside the toolchain container, then just live-harness; there is no hosted CI
just box <recipe|command> [args]  # run a recipe or a command inside the toolchain container: just box test, just box go test ./pkg/client; just box -- <command> for a command named like a recipe
just live-harness   # the real Claude Code CLI against this tree's binary in a network-less container (tests/live/claude-code.sh)
just ci-host        # every gate of "Verification before you are done", in order, on the host: for debugging, never the merge gate
just check          # gofmt, go vet, go mod tidy, build — the pre-commit gate
just staticcheck    # staticcheck at the version pinned in the justfile
just build-all      # CGO_ENABLED=0 cross-compile for linux, darwin, windows × amd64, arm64 into dist/<os>-<arch>/
just test           # go test -race -count=1 ./...  (SQLite on temp files; no services needed)
just fuzz           # go test -fuzz=FuzzDecode -fuzztime=30s ./pkg/envelope: the decoder on top of its seed corpus (every fixture body)
just skills         # regenerate the checked-in skill renders: skills/agentfeedback/SKILL.md from internal/skillgen/source, skills/agentfeedback-docs/ from the reference files, the plugin bundle, both marketplace manifests and docs/recipes/
just e2e            # live contract suite against a fresh `serve` on a temporary database (scripts/gate-e2e.sh, port 18080, E2E_ADDR overrides)
just e2e-local      # the client commands in local mode against a temporary data directory, no server process (scripts/gate-local.sh)
just run-local      # serve on 127.0.0.1:8090 with a database in ./local/
just playbooks <tag> [github|tree|<release-dir>]  # both install playbooks in a clean Linux container (Docker, privileged for systemd); `just release` runs it
just docker-build                             # the image from source, tagged agentfeedback (the published image comes from the release step)
just release-check <tag> <title> <notes.md>   # every release precondition and a snapshot build; publishes nothing
just release <tag> <title> <notes.md>         # publish a release (maintainers; releases.md)
bash scripts/e2e.sh <API_KEY> [BASE_URL]      # live v1 contract suite against a running service; creates rows
bash tests/skill/run-tests.sh                 # install.sh against an offline fixture release
python3 tests/skill/test_cluster.py           # cluster.py: disclosure boundaries and failure handling, no requests
python3 tests/skill/triage-playbooks.py        # triage playbooks: structure and required rules, every bash block against a throwaway server, the fix-it session start per harness over the CLI and the stdio MCP tools (needs bin/ from just check)
python3 scripts/playbooks.py check            # the playbooks' structure: one command, Verify and JSON Outcome per routed or excluded step, no unrouted sh block
python3 scripts/playbooks.py list             # the extracted commands in run order with the human's answers filled in; nothing runs
python3 scripts/playbooks.py --help           # the AF_PLAYBOOK_* inputs
python3 tests/playbooks/test_playbooks.py     # the extractor's tests
just contract                                 # contract gate: the two commands below
uv run --locked --script scripts/contract-check.py   # schemas valid 2020-12, every example validates, fixtures agree with conformance/reference
npx --yes @redocly/cli@2.54.2 lint docs/openapi.yaml # OpenAPI lint (recommended ruleset, redocly.yaml)
python3 conformance/reference/fixtures.py     # the fixtures alone, no dependencies
shellcheck -x -P SCRIPTDIR skills/*/scripts/*.sh tests/skill/*.sh
shellcheck -x scripts/*.sh                    # the repository scripts, release.sh included
python3 scripts/eval-cluster.py <export.ndjson> <labels.json> --allow-repo <remote>... [--live]   # cluster.py calibration
```

## Rules that are not visible in the code

- **An API change is a six-artifact change**, in one commit: `internal/`
  code, the contract files, `scripts/e2e.sh`, the CLI in
  `cmd/agentfeedback/` (with `coverage.toml`), the skill (its source in
  `internal/skillgen/source/` when the commands an agent runs change, and
  `skills/agentfeedback/scripts/install.sh` with `tests/skill/run-tests.sh`
  when the release assets change), and the playbooks `AGENT-INSTALL.md`
  and `AGENT-INSTALL-STACK.md` when a command, flag, route or outcome they
  quote changes. Any other change that alters one of those (the CLI,
  `internal/harness`, `install.sh`, the release assets) updates both
  playbooks in the same commit too; renumbering a step of
  `AGENT-INSTALL.md` updates the stack playbook, which runs client steps
  by number. Renumbering, adding or removing a playbook step updates the
  routes or the exclusions in `scripts/playbooks.py` (`playbooks.py check`
  fails on a step either names but the playbook lacks, and on an `sh` block
  neither covers), and a new human question gets its `AF_PLAYBOOK_*` input
  there.
  Adding, moving or renaming a document `llms.txt` lists
  updates it in the same commit. Producers build their calls from the
  contract without reading the code.
  `scripts/e2e.sh` fails on any `docs/openapi.yaml` operation it does not
  exercise, so a new route needs its check in the same commit.
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
  `just skills`, and commit both. `skills/agentfeedback-docs/` is the `docs`
  render of `docs/*.md`, `docs/recipes/*.md`, `docs/openapi.yaml`, `schemas/` and
  `AGENT-INSTALL*.md`, so a change to any of those files needs `just skills`
  and the render committed in the same commit. `plugins/agentfeedback/`,
  `.claude-plugin/marketplace.json` and `.agents/plugins/marketplace.json`
  are the `marketplace` render of `internal/skillgen/source/` and
  `skills/agentfeedback/scripts/install.sh`; `just skills` regenerates them
  and they are committed with their source. `docs/recipes/http-curl.md` and
  `docs/recipes/http-powershell.md` are the `prompt` and `prompt-powershell`
  renders; `GET /skill?format=prompt` serves the curl one with the server's
  URL, `scripts/e2e.sh` runs its bash blocks, and the docs skill embeds both
  (`just skills` writes them and rebuilds before rendering it). Text outside a
  `<!-- only: … -->` block reaches every form; the prompt forms assume nothing
  but HTTP and a shell, and no checked-in render names a server URL.
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
- **Gates touch nothing outside the repository and Docker.** Run every
  check as `just ci` or `just box <recipe>`, never as a bare recipe: `just
  ci` and `just box` run in the toolchain container
  (`scripts/in-container.sh`), with the repository mounted read-write at its
  own path, the git directories read-only, `.private/`, `.kitchen/`,
  `.claude/` and `.env` files hidden, HOME and `/tmp` a tmpfs, caches in the
  per-uid volume `agentfeedback-ci-cache-<uid>`, no host configuration,
  credential or socket and no published port. The network stays open for
  module and tool downloads, so a gate still runs code fetched at run time
  (staticcheck, redocly, Go modules) with write access to the checkout. A
  gate that runs a client or a harness gives it a fresh HOME and XDG
  directories and an empty environment (`env -i`). A check against a real
  harness runs in a network-less container from the same image, as `just
  live-harness` does. Exceptions that run on the host: `just ci-host` and the
  bare recipes (debugging only), `just playbooks`, which drives a privileged
  systemd container (not a boundary that protects the host) and stages the
  release in a host temporary directory, and the release step
  ([releases.md](releases.md)), which publishes with the maintainer's
  credentials. Pin every tool `tests/ci/Dockerfile` adds; the Debian packages
  are those of the base image's release. A changed Dockerfile is a new image
  tag, `AF_CI_IMAGE_REBUILD=1` rebuilds from fresh base layers, and `ci-host`
  fails when the image's Go differs from `go.mod`'s `toolchain`.
- **Compatibility.** Everything in API 1.0 keeps working. Additive changes
  bump the API minor in api.md's version line and "Changes" section; anything else is a major
  release. A server change that breaks older clients also raises
  `core.ClientMinVersion`, which `/api/v1/meta` serves as `client.min_version`.

## Verification before you are done

1. `just check` clean.
2. `just staticcheck` clean and `just build-all` builds every platform.
3. `just skills` leaves every checked-in render unchanged, the plugin bundle and both
   marketplace manifests included (`git status --porcelain` on the renders is empty), and
   `claude plugin validate --strict` passes on `plugins/agentfeedback` and the repository
   root (`just ci` runs it when the `claude` CLI is installed).
4. `just test` (`go test -race -count=1 ./...`) green.
5. Decoder touched (`pkg/envelope/`): `just fuzz` green; a crash it finds is
   committed under `pkg/envelope/testdata/fuzz/FuzzDecode/` as a regression
   seed beside the fix.
6. API touched: `just e2e` green (builds, serves on a temp database, runs
   `scripts/e2e.sh`, which also fails on an uncovered operation), and
   `cmd/agentfeedback/coverage.toml` and `internal/mcp/coverage.toml`
   updated (`just test` fails otherwise).
7. Client commands or the local target touched: `just e2e-local` green (the
   client commands in local mode against a temporary data directory, no
   server process).
8. Skill scripts or triage playbooks touched: the shellcheck command above,
   `bash tests/skill/run-tests.sh`, `python3 tests/skill/test_cluster.py` and
   `python3 tests/skill/triage-playbooks.py` all green.
9. `shellcheck -x scripts/*.sh` clean.
10. Docs touched: every relative link resolves.
11. Contract files touched (`schemas/`, `docs/openapi.yaml`, `conformance/`):
    `just contract` green. It also validates the plugin manifest and the
    rendered mcp.json against the vendored Agent Plugins 1.0.0 schemas
    (`internal/skillgen/testdata/agent-plugins/`).
12. Playbooks or the extractor touched: `python3 tests/playbooks/test_playbooks.py`
    and `python3 scripts/playbooks.py check` green. A change to a playbook
    command, the CLI or `install.sh` also runs the playbook gate,
    `just playbooks <tag> tree` (any release tag, built from the working
    tree; Docker), which runs every routed step as a non-root user in a
    clean Debian container with systemd and fails on a non-zero exit or a
    Verify condition that does not hold. The human's answers come from
    `AF_PLAYBOOK_*` variables (`python3 scripts/playbooks.py --help` lists
    them); unset is no, and the step that needs the answer is reported
    skipped. Steps that need a third-party CLI (client 2.4 and 2.5, stack
    S6) are checked for structure only; the release's paste check covers
    them. `just release` runs it against the local build before the tag
    is pushed; `just playbooks <tag>` checks a published release.

13. Every change: `just live-harness` green (the real Claude Code CLI
    installs, lists and uninstalls the MCP entry, skill and hooks, runs the
    installed hooks on fixture payloads, with
    `CLAUDE_CONFIG_DIR` unset and set to `~/.claude`).

`just ci` runs items 1 to 11 and the static half of item 12 in order inside the toolchain container,
then item 13, and fails if `just check` rewrote a
file or any gate changed the tree. Run a single item with `just box <recipe>`. There is no hosted CI: `just ci` green on the tree that is merged is the
merge gate, and the image is built and published by the release step
([releases.md](releases.md)). A change that adds a gate adds it to the `ci`
recipe in the `justfile` and to the list above, in the same commit.

## Release

Every delivered change ships in a release, which a maintainer cuts when they
decide; versioning rules and steps are in [releases.md](releases.md).
