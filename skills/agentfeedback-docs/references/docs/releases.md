# Releases

## Versioning and publication

Source releases are annotated, immutable `vMAJOR.MINOR.PATCH` tags, with
`-rc.N` pre-releases before a stable one ([Tags](#tags)): patch for
compatible fixes, docs and dependency updates; minor for compatible features;
major for breaking API or operational contracts. Every delivered change belongs
to a release. The two skills carry their own `version` in their `SKILL.md`;
bump one only when its command contract changes. The `agentfeedback` skill's
`SKILL.md` is generated: its version is in `internal/skillgen/source/skill.json`,
and `just skills` renders it.

Maintainers decide when to cut a release, by hand, when they judge `main`
ready; landed changes never trigger one. An agent pushes a `v*` tag, creates a
GitHub release or pushes an image (the steps below, recovery included) only
when a maintainer asks for that version.

Maintainers need Git, the tools `just ci` needs ([develop.md](develop.md)),
GoReleaser 2.18.2 exactly (the `v2.18.2` archive from
github.com/goreleaser/goreleaser/releases, or `brew install goreleaser` while
2.18.2 is Homebrew's version; `just release` refuses any other version,
because the archives are only reproducible with the same one), Docker with
buildx logged in to `ghcr.io` (push access to the
`agentfeedback/agentfeedback` package), able to build and run `linux/amd64`
and `linux/arm64` images (on a machine of the other architecture, QEMU
emulation: `docker run --privileged --rm tonistiigi/binfmt --install all`;
`docker buildx ls` lists the platforms) and allowed to run a privileged
container (the playbook gate runs systemd in one), `curl`, and an
authenticated GitHub CLI (`gh`) with write access to the repository and its
tags. There is no hosted CI: every
check runs on the releasing machine.

### Tags

- `vX.Y.Z`: a stable release, marked `latest` on GitHub, image tags
  `X.Y.Z`, the full commit SHA and `latest`.
- `vX.Y.Z-rc.N`: a pre-release, published as a public GitHub release flagged
  pre-release and never `latest`, image tags `X.Y.Z-rc.N` and the full commit
  SHA.
  It exists to verify the release assets, the installer and the install
  playbooks before the stable tag: `just playbooks vX.Y.Z-rc.N` runs both
  playbooks against the published pre-release, downloaded as an agent
  downloads it. No other suffix is accepted.

### Assets

Every release carries exactly these assets, and `install.sh` and the install
playbooks rely on the names:

| Asset | Content |
|---|---|
| `agentfeedback_<version>_<os>_<arch>.tar.gz` | `os` `linux` or `darwin`, `arch` `amd64` or `arm64`: the binary `agentfeedback`, `LICENSE`, `README.md` |
| `agentfeedback_<version>_windows_<arch>.zip` | the same with `agentfeedback.exe` |
| `SHA256SUMS` | SHA-256 of the six archives, in `sha256sum` format |
| `install.sh` | `skills/agentfeedback/scripts/install.sh` from the tagged commit |

`<version>` is the tag without the `v`. The binaries are static
(`CGO_ENABLED=0`), built with `-trimpath` and the toolchain named on the
`toolchain` line of `go.mod`, and stamped with the version and commit that
`agentfeedback version` prints. The exact build line is in
[operate.md](operate.md#build-from-source); `.goreleaser.yaml` holds the same
flags.

### Cutting a release

1. Update the stable version link in `README.md` (stable releases only) and
   add notes to this file. The service version is the build version; nothing
   in the code holds it.
2. Run `just ci`, commit, push `main`.
3. Write the release notes to a file outside the checkout or under the
   gitignored `.private/`.
4. Run the preflight, then publish:

   ```bash
   just release-check vX.Y.Z 'Release name' /path/to/notes.md
   just release vX.Y.Z 'Release name' /path/to/notes.md
   ```

   Both check the tag form, the GoReleaser version, that `install.sh`
   exists, that `origin` is this repository, a clean `main` equal to
   `origin/main`, that the tag exists neither locally nor on `origin`, that
   `gh` can push, `goreleaser check`, and `just ci`. The preflight then builds
   a snapshot of all six targets into `dist/release/`, verifies its checksums,
   builds the two snapshot images and smoke-tests them (each platform's binary
   prints the snapshot version, and the image started with no arguments
   answers `/ready`), and stops. `just release` instead builds the release against a local tag
   (`goreleaser release --skip=publish`), checks it and runs both install
   playbooks against it in a clean container (`just playbooks <tag>
   dist/release`, [develop.md](develop.md#verification-before-you-are-done),
   with every `AF_PLAYBOOK_*` answer unset: no exposure, the default
   harness; run `AF_PLAYBOOK_ADDRESS=0.0.0.0 just playbooks <tag> tree`
   beforehand to cover the exposure branch), then builds and smoke-tests the
   snapshot images as the preflight does, because GoReleaser builds the
   published images only while publishing;
   only then does it push the annotated tag with an absence lease, so a
   build or playbook defect leaves nothing public and the local tag is
   removed. GoReleaser then rebuilds, pushes the
   image (one multi-platform index under every tag above) and uploads the
   release, which stays a draft until every asset is up. The script
   verifies it as a downloader sees it: not a draft, flagged pre-release
   exactly when the tag is one, `latest` exactly when it is stable, exactly
   the assets above, `SHA256SUMS` byte-identical to the local build's,
   `install.sh` identical to the committed one, and every downloaded archive
   matching `SHA256SUMS`; then the image as a puller sees it: every tag names
   the same index, which holds `linux/amd64` and `linux/arm64`, and each
   platform's binary prints the version. Last, it prints the release URL. Deploy the SHA or the version tag,
   never `latest`.

### Verifying a release

```bash
gh release download vX.Y.Z --repo AgentFeedback/agentfeedback --pattern 'agentfeedback_*' --pattern SHA256SUMS
sha256sum -c SHA256SUMS          # Linux
shasum -a 256 -c SHA256SUMS      # macOS
```

Without `gh`, download from
`https://github.com/AgentFeedback/agentfeedback/releases/download/vX.Y.Z/<asset>`.
To check one archive only, feed its line to the same command:
`grep ' agentfeedback_X.Y.Z_linux_amd64.tar.gz$' SHA256SUMS | sha256sum -c`.

### Reproducing a release

Two builds of the same tag produce identical binaries and identical
archives. Check it after a release, from a clean checkout of the tag:

```bash
git checkout vX.Y.Z
export GOTOOLCHAIN=$(awk '$1 == "toolchain" { print $2 }' go.mod) GORELEASER_CURRENT_TAG=vX.Y.Z RELEASE_TITLE=check
goreleaser release --skip=publish --clean && cp dist/release/SHA256SUMS /tmp/SHA256SUMS.first
goreleaser release --skip=publish --clean && diff /tmp/SHA256SUMS.first dist/release/SHA256SUMS
gh release download vX.Y.Z --repo AgentFeedback/agentfeedback --pattern SHA256SUMS --output - | diff - dist/release/SHA256SUMS
```

No output from either `diff` means both local builds match each other and
the published archives. The build line in
[operate.md](operate.md#build-from-source) gives the same binary as the one
in the archive: compare `sha256sum agentfeedback` with
`tar -xOzf agentfeedback_X.Y.Z_linux_amd64.tar.gz agentfeedback | sha256sum`.

### Recovery

When `just release` stops, it says whether the tag is already public. Before
the tag push nothing is public and the script has removed its local tag: fix
the cause and run it again. After the push, inspect the remote tag, the
GitHub release and the image tags on GHCR before doing anything, and never
force-move or delete the published tag:

- **No release, or a draft** (GoReleaser failed before publishing, the
  image push included; it runs before the release is created): on a
  clean checkout of the tagged commit, rerun GoReleaser with the same title.
  It finds the draft by its name, `vX.Y.Z: <title>`, keeps its notes and
  replaces any asset already uploaded with its byte-identical rebuild, and
  pushes the image tags again:

  ```bash
  export GOTOOLCHAIN=$(awk '$1 == "toolchain" { print $2 }' go.mod) GORELEASER_CURRENT_TAG=vX.Y.Z RELEASE_TITLE='Release name'
  GITHUB_TOKEN=$(gh auth token) goreleaser release --clean --release-notes /path/to/notes.md
  ```

- **A published release** (a later check failed): do not run GoReleaser
  again; it would try to create a second release. Run the checks of
  [Verifying a release](#verifying-a-release), confirm the state with
  `gh release view vX.Y.Z --json isDraft,isPrerelease,assets` and the image
  with `docker buildx imagetools inspect ghcr.io/agentfeedback/agentfeedback:X.Y.Z`,
  and fix the release on GitHub by hand if a check failed.
- **A published release whose image check failed**: GoReleaser pushed the
  image before it created the release, so the image exists. A tag that is
  missing or names another index is repointed at the version's index, which
  holds the release's binaries:

  ```bash
  docker buildx imagetools create -t ghcr.io/agentfeedback/agentfeedback:<sha> ghcr.io/agentfeedback/agentfeedback:X.Y.Z
  docker buildx imagetools create -t ghcr.io/agentfeedback/agentfeedback:latest ghcr.io/agentfeedback/agentfeedback:X.Y.Z   # stable only
  ```

  Then repeat the checks: `imagetools inspect` on each tag shows one digest
  with `linux/amd64` and `linux/arm64`, and
  `docker run --rm --pull always --platform linux/<arch> ghcr.io/agentfeedback/agentfeedback:X.Y.Z version`
  prints `X.Y.Z` on both. A version index that lacks a platform or whose
  binary fails is a defect of the release: cut a new version.

The notes file from step 3 is uncommitted, so it does not exist on another
machine: recovering there means rebuilding it from this file's section for
that version. Rewrite the section's relative links to repo-root paths
(`api.md` to `docs/api.md`) — a release body resolves them against the
repository root, not `docs/`. Corrections to source
need a new version; corrections to release prose need no new tag.

## v3.0.0 — AgentFeedback: new home, new names, fresh start

The project moved to [github.com/AgentFeedback/agentfeedback](https://github.com/AgentFeedback/agentfeedback)
and is the open-source, self-hostable AgentFeedback
([agentfeedback.dev](https://agentfeedback.dev)); a hosted version with the
same API runs at [agentfeedback.io](https://agentfeedback.io). Every name
changes and v3 is a fresh start: no migration from or compatibility with
2.x deployments and skill installs. The HTTP API (1.1), payloads and hash forms are unchanged.

- **Skills** `agent-feedback` → `agentfeedback` 4.0 and
  `agent-feedback-triage` → `agentfeedback-triage` 3.0: new directory and
  invocation names (`/agentfeedback-triage`). Client environment variables
  keep their names (`AGENT_FEEDBACK_*`); the spool moves to `~/.cache/agentfeedback` and triage digests to
  `${TMPDIR:-/tmp}/agentfeedback-triage/`. The skills work against a
  self-hosted service or `https://api.agentfeedback.io`.
- **Service**: Go module `github.com/agentfeedback/agentfeedback`; binary
  `agentfeedback` (`cmd/agentfeedback`, `/opt/agentfeedback`, runs as user
  `agentfeedback`); image `ghcr.io/agentfeedback/agentfeedback`; default
  database `/data/agentfeedback.db`; metrics renamed `agentfeedback_*`
  (`agentfeedback_submissions_unprocessed`, `agentfeedback_db_bytes`,
  `agentfeedback_sqlite_busy_total`).
- **Deploy**: stack directory `infra/agentfeedback`, compose service
  `agentfeedback`, volume `agentfeedback-data`, server directory
  `~/agentfeedback`, variables `AGENTFEEDBACK_IMAGE` and
  `AGENTFEEDBACK_BIND_ADDRESS`.
- **Removed**: the 1.x PostgreSQL export script and migration and uninstall
  docs, and the `docs/agent-usage.md` redirect.
- CI lowercases the image name (the org name has capitals).

## v2.2.1 — Documentation sweep

Docs and skill documentation only; the service and scripts behave as in
v2.2.0.

- **Skill `agent-feedback-triage` 2.1**: the optional clustering contract
  moved to `reference/clustering.md`, read only when clustering is used, and
  repeated rules were collapsed: SKILL.md is a third shorter per invocation.
  Install the whole directory, including `reference/`. Resolutions now start
  with their verdict (`FIXED`, `INVALID`, `DUPLICATE-OF-<id>`). Fixed: the
  close-out commands' path to the sibling skill, digest exit codes, the
  meaning of `unchanged` versus `updated`, and cluster.py's statuses, reasons
  and exit codes.
- **Skill `agent-feedback`** (contract unchanged): corrected the `machine`
  override, `--sweep` age/lock/outcome behaviour, backlog-warning scope, TSV
  column 5, `slots` usage, 1xx/3xx outcomes, export digest tools, and added
  `AGENT_FEEDBACK_TRIAGE_ROOTS`.
- [api.md](api.md) states its version (1.1), the non-JSON responses and the
  `include=payload` row shape.
- [operate.md](operate.md): the retention purge runs as root, backups create
  their directory, `openssl` and `DEPLOY_DIR` are documented, and uninstall
  covers the triage variables.
- `CLAUDE.md` routes to [develop.md](develop.md), the single home for
  commands, rules and gates; the README quickstart checks out the latest tag
  and keeps an existing key.

## v2.2.0 — Triage skill renamed; batched, measured clustering

- **Skill `feedback-triage` is now `agent-feedback-triage` 2.0.** The
  invocation name and the install directory change: update installers,
  links and prompts that name `skills/feedback-triage`, and remove the old
  copy ([operate.md](operate.md#uninstall) lists both names). Digests move to
  `${TMPDIR:-/tmp}/agent-feedback-triage/`. No service/API or storage changes.
- The triage skill is direct invocation only: `disable-model-invocation: true`
  (Claude Code) and a description that forbids loading it from phrasing
  about the queue. Invoke it as `/agent-feedback-triage` or by name.
- Docs: install guidance for the triage skill, the rule to re-measure after
  any `cluster.py` prompt, threshold or batching change, and the eval
  script's disclosure boundary in [security.md](security.md).
- `cluster.py` batches comparisons: up to 8 reports per request, every pair
  asked once over a shared state, so 24 reports need 15 requests instead of
  276 (which exceeded the old cap and skipped advice). One request per pair
  remains the fallback for batch sizes below 4 and for chunks over the
  model's token budget; a pair too large alone is marked unassessed without
  a request. One invalid answer in a batch marks only that pair.
- **`--max-pairs` is replaced by `--max-requests`** (default 200); the old
  flag is rejected because its unit changed. New `--batch-size` (default 8).
- Probability sums tolerate the model's per-option two-decimal rounding.
- Calibration recorded in the skill: on 112 labelled pairs, no different
  pair was grouped at the 0.8 threshold; consent, dry-run, key handling and
  complete-link grouping are unchanged. `scripts/eval-cluster.py` repeats
  the measurement and refuses to send any report whose exact remote was not
  approved with `--allow-repo`.
- Go module dependency updates (indirect only).

## v2.1.0 — Optional advisory triage clustering

- **Skill `feedback-triage` 1.1** adds a Python 3.9+ helper that compares
  report mechanisms through TypeSafe and suggests clusters. The manual
  workflow remains the default; no service/API or storage changes.
- Disclosure requires explicit approval for each exact repository identity.
  Preview is local; unapproved or unidentified reports are never sent.
  No credential is copied, no queue item is marked, and no report is removed.
- Advice retains source IDs, the digest hash and individual probabilities.
  Groups require agreement for every member pair, not transitive matches.
  Uncertain, unavailable and invalid answers fall back to manual triage;
  bounded requests preserve partial results without dropping reports.

## v2.0.0 — SQLite, generic events, triage skill

Breaking operational contract, compatible API.

- **Storage**: PostgreSQL replaced by SQLite (`modernc.org/sqlite`, pure Go).
  One container, one named volume, no database service. Physical backups via
  `feedback backup`, logical via `GET /api/v1/export`; restore and migration
  via `feedback import` (all-or-nothing, header/count/digest verified, hashes
  recomputed, `--family` filter, ids never reused). See
  [operate.md](operate.md#restore-and-migration)
  for the 1.x migration procedure; `scripts/export-v1-postgres.sh` produces
  the import file from a 1.x deployment with ids, timestamps, processing state
  and hashes preserved.
- **API 1.1** ([api.md](api.md)): additive. `family` and `payload_hash` on
  every record, `POST /api/v1/events`, `resolution` when marking processed,
  keyset pagination (`before_id`, `has_more`, `next_before_id`, `total`),
  `include=payload`, `GET /api/v1/export`, six-digit timestamps. Every 1.0
  request and response field is unchanged.
- **Service**: stdlib `net/http`, Prometheus with bounded labels plus backlog,
  database size and busy counters. OpenTelemetry, chi, sqlc and the
  PostgreSQL-specific configuration are gone. `GRACEFUL_SHUTDOWN_TIMEOUT`
  default is now 30s; the fixed 5s drain sleep is removed. Payloads are
  returned byte-exact (large integers no longer round).
- **Deployment**: `scripts/deploy.sh <sha>` installs the compose file,
  preserves the host `.env`, pins the image there and runs `docker compose
  pull && up`. Requires the GHCR package to be pullable by the host. The
  `crane` streaming path is gone.
- **Skill `agent-feedback` 3.0**: new `submit-event.sh`; `process.sh list`
  pages through the whole queue and takes `--include-processed` (the old
  `--all` errors), `done` takes `--resolution`; `query.sh export`;
  `submit-review.sh --sweep` scans `REVIEW_LOG_DIR` and
  `AGENT_FEEDBACK_REVIEW_DIRS` only (set the latter where hardcoded cache
  paths were relied on). Fixes: spool persistence is verified (an unwritable
  spool now fails loudly instead of reporting `spooled`), review receipts are
  validated before a run is marked submitted, large prose no longer passes
  through argv, `--dry-run` applies the server's validation, rejected spool
  files are kept 30 days as documented.
- **New skill `feedback-triage` 1.0**: end-to-end queue processing with one
  consolidated interview. `scripts/digest.sh` pulls and groups the open queue.
- **Docs** rewritten for agents first: README route table, `api.md`,
  `operate.md`, `develop.md`, `security.md`. Template-era documents removed;
  `docs/agent-usage.md` redirects to `api.md`.

Upgrade: follow the migration procedure; the 1.x PostgreSQL volume is not
read by 2.x. Producers need skill 3.0 only for the new features; 2.1 clients
keep working against the 2.0.0 service.

## v1.0.1 — Public documentation and release workflow

Compact README with dedicated guides, guarded release tooling, updated Go
modules, toolchain, container bases and CI actions. No API or migration
changes.

## v1.0.0

Initial stable API and companion client release.
