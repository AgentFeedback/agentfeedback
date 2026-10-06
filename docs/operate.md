# Operate AgentFeedback

Install, run, back up, upgrade and migrate the service. One container, one
SQLite file, one API key. Every step is a command an agent can run.

Contents: [Run locally](#run-locally) · [Bare binary (no Docker)](#bare-binary-no-docker) ·
[Wire the harnesses](#wire-the-harnesses) · [Build from source](#build-from-source) ·
[Deploy to a host](#deploy-to-a-host) ·
[Configuration](#configuration) · [Backups](#backups) ·
[Restore and migration](#restore-and-migration) · [Upgrade](#upgrade) ·
[Key rotation](#key-rotation) · [Retention](#retention) · [Monitoring](#monitoring) ·
[Uninstall](#uninstall)

## Run locally

Needs Docker with Compose. From the repository root:

```bash
cd infra/agentfeedback
test -e .env || (umask 077 && printf 'API_KEY=%s\n' "$(openssl rand -hex 32)" > .env)   # keeps an existing key
docker compose up -d --build --wait
curl --fail --silent --show-error --retry 60 --retry-all-errors --retry-delay 1 http://127.0.0.1:8090/ready
```

The service listens on `127.0.0.1:8090` and stores its database in the named
volume `agentfeedback-data` (`/data/agentfeedback.db` in the container). First request:

```bash
export AGENT_FEEDBACK_URL=http://127.0.0.1:8090
export AGENT_FEEDBACK_API_KEY=$(sed -n 's/^API_KEY=//p' .env)
curl -sS -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" -H 'Content-Type: application/json' \
  -d '{"kind":"friction","summary":"local smoke test","payload":{"category":"test"}}' \
  "$AGENT_FEEDBACK_URL/api/v1/submissions"
curl -sS -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" "$AGENT_FEEDBACK_URL/api/v1/submissions?limit=5"
```

The service answers the v1 contract, [openapi.yaml](openapi.yaml); `bash scripts/e2e.sh "$AGENT_FEEDBACK_API_KEY" "$AGENT_FEEDBACK_URL"` from the repository root runs the whole contract suite against it (it creates rows).

Tear down with `docker compose down -v` (deletes the database volume; `.env`
stays).

Without Docker: `go build -o bin/agentfeedback ./cmd/agentfeedback && API_KEY=dev DATABASE_PATH=/tmp/agentfeedback.db HTTP_LISTEN_ADDR=127.0.0.1:8090 bin/agentfeedback serve`.

## Bare binary (no Docker)

With an installed `agentfeedback` binary, on the machine that runs the server:

```bash
agentfeedback serve --init                       # [--dir D] [--db PATH] [--port N] [--force]
agentfeedback server install --systemd           # Linux user unit; or --launchd (macOS), or --compose [--image REF]
```

`serve --init` writes two files, both `0600`, into
`${XDG_CONFIG_HOME:-~/.config}/agentfeedback/server` (`--dir`): `api-key`, a
new random key, and `serve.env`, the three variables `serve` reads
(`API_KEY_FILE`, `DATABASE_PATH`, default
`${XDG_DATA_HOME:-~/.local/share}/agentfeedback/agentfeedback.db`, and
`HTTP_LISTEN_ADDR=127.0.0.1:<port>`, port 8090 by default). Its last stdout
line is a JSON outcome, the only place the key is printed. An existing file
is refused; `--force` replaces both and rotates the key. Paths may hold only
letters, digits and `/ . _ - + @ , =`; `serve --init` and `server install`
are not supported on Windows.
Run it in the foreground with `set -a; . <dir>/serve.env; set +a; agentfeedback serve`.

`server install` reads `serve.env` and writes exactly one file; it starts,
enables and reloads nothing and prints the commands to do so (the JSON
outcome's `next`). An existing file is refused without `--force`.
`serve.env` may also set `PUBLIC_URL` and `MCP_INSTRUCTIONS` (see
[Configuration](#configuration)); `serve --init` writes neither. Write
`MCP_INSTRUCTIONS` as one line in single quotes with no single quote or
backslash inside,
such as `MCP_INSTRUCTIONS='File one report per task.'`: the shell and
systemd both read `serve.env`, and only that form means the same to both.
`PUBLIC_URL` must already be in normalised form (one trailing slash is
allowed), since the shell reads it too. `server install` refuses any other
key, an unusable or non-normalised `PUBLIC_URL` and any other form of
`MCP_INSTRUCTIONS`; the systemd unit reads both through
`serve.env`, and the plist and `compose.yaml` copy them when set.

| Mode | Writes | Next commands |
|---|---|---|
| `--systemd` (refused as root) | `${XDG_CONFIG_HOME:-~/.config}/systemd/user/agentfeedback.service` | `systemctl --user daemon-reload`, `systemctl --user enable --now agentfeedback.service`, `systemctl --user status agentfeedback.service`; `loginctl enable-linger "$USER"` keeps it running without a login session |
| `--launchd` | `~/Library/LaunchAgents/dev.agentfeedback.serve.plist`, logs in `~/Library/Logs/agentfeedback/serve.log` | `launchctl bootstrap gui/$(id -u) <plist>`, `launchctl kickstart -k gui/$(id -u)/dev.agentfeedback.serve`, `launchctl print gui/$(id -u)/dev.agentfeedback.serve`; the plist copies `serve.env`, so after editing it rerun with `--force`, then `launchctl bootout` and `bootstrap` |
| `--compose` | `<dir>/compose.yaml`: the image (`--image`, default the release's own tag), run as your uid:gid, the key as a file secret, the database directory as `/data`, published on `127.0.0.1:<port>` | `docker compose -f <dir>/compose.yaml up -d`, `... ps`, `... logs -f agentfeedback` |

Backup for systemd and launchd: `set -a; . <dir>/serve.env; set +a; agentfeedback backup <db dir>/agentfeedback-$(date -u +%Y%m%dT%H%M%SZ).db`;
for compose, the [physical backup](#backups) with `-f <dir>/compose.yaml`.
Exposing the server beyond `127.0.0.1` is your decision ([security.md](security.md)).
The compose form runs the container as your uid:gid, which rootless Docker or
userns-remap may not map to the owner of the key file and database directory;
there, use the root-based stack of [Run locally](#run-locally).
Rotate the key with `agentfeedback serve --init --force`, then the restart command
for your form (`server install` lists it in `next`).

Wire a client on the same machine and verify the whole path:

```bash
agentfeedback doctor --init --url http://127.0.0.1:8090 --key-from-stdin < ~/.config/agentfeedback/server/api-key
agentfeedback doctor --e2e          # submit, list and mark one install-check row; --json for one line per step
```

`doctor --e2e` sends straight to the server (no spool, no client log line),
stops at the first failing step and exits 1. Its row has kind
`install-check`, which `list`, `stats`, `digest` and `migrate` leave out
unless `--kind` or `--include-kind` names it, and is marked processed with
verdict `install-check`.

## Wire the harnesses

On every machine whose coding agents should file feedback, with the
`agentfeedback` binary installed and the client configured (`doctor --init`):

```bash
agentfeedback install                            # list the harnesses: detected, mode, skill, mcp, hook, reminder, docs; changes nothing (also --list, --json)
agentfeedback install all                        # every detected harness: the skill and a Stop hook running agentfeedback flush --hook
agentfeedback install claude-code codex --with-reminder   # also a session-start hook printing agentfeedback skill reminder
agentfeedback install opencode --mcp             # an MCP entry instead of the skill and the hook
agentfeedback install claude-code --docs         # also the agentfeedback-docs skill: the reference docs, schemas and OpenAPI document for integrators and operators
agentfeedback install all --dry-run              # print every file that would change, change nothing
agentfeedback uninstall all                      # remove exactly what install added
```

Harnesses: `claude-code`, `codex`, `cursor`, `opencode`, `omp`, `pi`. A
harness counts as detected when its binary is on `PATH` or its directory
exists; a named harness that is not detected is wired anyway, with a note.
`all` is every detected harness plus any named beside it.
Unknown names exit 2. The last stdout line is a JSON outcome (`status`
`installed`, `unchanged`, `uninstalled`, `dry_run` or `error`, `harnesses`,
`changed`, `backups`, `next`); progress goes to stderr. Running install twice
changes nothing the second time (`unchanged`); a changed mode, binary path,
server, `--with-reminder` or `--docs` replaces what the previous run added. Not
supported on Windows.

| Harness | Detected by | Skill | Hook (default) | Reminder | MCP entry (`--mcp`) |
|---|---|---|---|---|---|
| `claude-code` | `claude`, `$CLAUDE_CONFIG_DIR` (default `~/.claude`) | `<claude config>/skills/agentfeedback/SKILL.md` | `<claude config>/settings.json` `hooks.Stop` | `hooks.SessionStart` | `claude mcp add-json agentfeedback ... --scope user` |
| `codex` | `codex`, `$CODEX_HOME` (default `~/.codex`) | `~/.agents/skills/agentfeedback/SKILL.md` | `<codex home>/hooks.json` `hooks.Stop` | `hooks.SessionStart` | marked `[mcp_servers.agentfeedback]` block in `<codex home>/config.toml` |
| `cursor` | `cursor-agent`, or an `agent` that resolves into a Cursor install; `~/.cursor` | `~/.cursor/skills/agentfeedback/SKILL.md` | `~/.cursor/hooks.json` `hooks.stop` | `hooks.sessionStart` | `~/.cursor/mcp.json` `mcpServers.agentfeedback` |
| `opencode` | `opencode`, `${XDG_CONFIG_HOME:-~/.config}/opencode` | `<opencode>/skills/agentfeedback/SKILL.md` | plugin `<opencode>/plugins/agentfeedback.js` (on `session.idle`) | not supported | `mcp.agentfeedback` in the first of `opencode.jsonc`, `opencode.json` that has an `mcp` member, else an existing `opencode.jsonc`, else `opencode.json` |
| `omp` | `omp`, `~/.omp` | `~/.omp/agent/skills/agentfeedback/SKILL.md` | extension `~/.omp/agent/extensions/agentfeedback.ts` (on `agent_end`, not for subagents) | not supported | `~/.omp/agent/mcp.json` `mcpServers.agentfeedback` |
| `pi` | `pi`, `~/.pi` | `~/.pi/agent/skills/agentfeedback/SKILL.md` | extension `~/.pi/agent/extensions/agentfeedback.ts` (on `agent_settled`) | not supported | `~/.pi/agent/mcp.json` `mcpServers.agentfeedback`; needs pi 0.99.0 or later |

- **The hook.** `agentfeedback flush --hook` prints nothing on stdout or
  stderr, stops sending 3.5 seconds after it starts and returns by 4.5
  seconds (inside the 5-second timeout the hook is given, so the spool's
  bookkeeping and the failure log finish before the harness stops it),
  returns at once when nothing in the spool is due, logs any failure to
  `client.jsonl` and always exits 0. Hook commands hold the binary's
  resolved absolute path, which may contain only letters, digits and
  `/ . _ - + @ , =`; rerun install after moving the binary.
- **`--docs`.** The `agentfeedback-docs` skill goes to
  `<skills dir>/agentfeedback-docs/`, beside the skill's directory in the
  table (for `--mcp` too, where no skill is installed): a `SKILL.md` that maps
  `references/`, which holds this binary's copies of `docs/`, the OpenAPI
  document, `schemas/` and both install playbooks. A run without `--docs`
  removes it; an edited file in it is refused like an edited skill.
- **`--mcp`.** The MCP entry replaces the skill and the hook for that
  harness: the server's MCP instructions teach the agent, and nothing is
  spooled without the CLI. Entries point at `<server>/mcp` and read the key
  from `AGENT_FEEDBACK_API_KEY` in the harness's environment; install warns
  when it is not set. `--with-reminder` does not apply with `--mcp`.
- **The server.** `--server cloud|URL`, else `url` in `config.toml`, else the
  server the last install recorded, else a prompt on a terminal; with none of
  them install fails and writes nothing. A `--server` that differs from
  `config.toml` is refused: install never changes the configured server
  (`doctor --init --force` does). Install never writes `config.toml`; when it
  does not exist, `next` holds the `doctor --init` command.
- **Edits and backups.** JSON and JSONC files are edited in place, keeping
  comments, trailing commas and formatting; nothing else in them changes.
  Before the first change to an existing file install copies it to
  `<file>.agentfeedback-backup`. Writes are atomic and stop when a file
  changes while install runs. A file with the same key twice in one object
  is refused (uninstall leaves it as it is and keeps its backup). Only
  regular files are edited: a configuration file that is a symbolic link is
  refused, so wire that harness by hand from the table above, or replace the
  link with the file it points to; a linked skill (file or directory) or
  plugin file is refused too: remove it and run install again.
- **One run at a time.** Install, uninstall and `--dry-run` hold an
  exclusive lock on the home directory for the whole run, so no lock file is
  left behind; a second run while one holds it is refused (run it again when
  the first finishes). The lock is taken before the manifest is read. On a
  file system that cannot lock (NFS, for one) install warns on stderr and
  runs without it: make sure no other install runs at the same time. The
  list takes no lock.
- **The manifest.** `${XDG_CONFIG_HOME:-~/.config}/agentfeedback/install.json`
  (`0600`) records every entry, file, backup and directory install made.
  Uninstall removes exactly those: a file nobody changed since goes back to
  its backup byte for byte (or away, if install created it) and the backup is
  deleted; a changed file loses only the recorded entries and keeps its
  backup, listed in `backups`; an edited skill or plugin file is left in
  place, and so is an MCP entry whose value was changed since install (noted).
  A file someone else changed between two installs is marked diverged:
  uninstall never restores its backup or deletes it, but removes the recorded
  entries and keeps the backup, listed. A backup that is missing, or is not
  the copy install took, is not restored either; a backup equal to the file
  it was taken from is deleted. Before its first change of any kind a run
  writes the manifest once with its server, binary and locations; if that
  write fails, the run stops having changed nothing. After that the manifest
  is updated after every claude command and every file. Each file is
  checked for changes before its backup is written and again just before it
  is replaced; when the write fails or the file changed, the backup this run
  wrote for it is deleted. So after a failed write the manifest matches what
  was written, and running the command again finishes the job. Only a hard
  crash in the moment between a write and its manifest update leaves an
  entry unrecorded; the next run then refuses it as foreign, naming it,
  except that a leftover backup equal to its file is taken over as the
  backup and an empty skill directory the manifest records as created is
  reused. The manifest records the Codex home and Claude Code configuration
  directory of the harnesses it keeps; a later run under another
  `CODEX_HOME` or `CLAUDE_CONFIG_DIR` keeps using the recorded location for
  that harness, notes it, and runs `claude` with `CLAUDE_CONFIG_DIR` set to
  the recorded directory (unset when it is `~/.claude`). Install refuses a
  manifest that names a path it does not write under the current
  environment: run it with the `HOME`, `XDG_CONFIG_HOME`, `CODEX_HOME` and
  `CLAUDE_CONFIG_DIR` of the install, or fix the manifest.
  Empty directories install created go, and so does the manifest when
  no harness is left. What a harness added to a file install created (OpenCode
  adds `$schema`) stays.
- **Foreign entries are refused.** An `agentfeedback` MCP entry, TOML table,
  skill directory or plugin file, or an identical hook, that the manifest does
  not record stops the run before anything is written, naming the path:
  remove or rename it and run install again. A skill directory provisioned
  before `install` existed (a copy or symlink of `skills/agentfeedback`) must
  be removed first. A recorded skill or plugin file edited since install is
  refused too.
- **`CLAUDE_CONFIG_DIR`** moves Claude Code's settings and skills, and
  install follows it. Its documentation does not say where `.claude.json`
  lives then, so with it set the check below is skipped: install relies on
  `claude mcp add-json` reporting an existing entry, and uninstall always
  runs `claude mcp remove`.
- **Claude Code's MCP entry** is checked against `~/.claude.json` (read
  only): an entry removed by hand is added again by install and skipped by
  uninstall; one pointing at another URL is refused by install and left in
  place by uninstall. When `claude` is missing or `claude mcp remove` fails,
  uninstall goes on and notes the command to run by hand:
  `claude mcp remove agentfeedback --scope user`.
- **Codex** runs a new hook only after you trust it in `/hooks`.
- **Cursor** also runs Claude Code's hooks and skills.
- **Verification status.** Checked against installed harnesses: Claude Code
  lists the MCP entry; OpenCode loads the skill, the plugin and the MCP entry;
  the hook commands, the OpenCode plugin and the omp extension were run
  outside a live session. The Codex, Cursor and pi wiring is written from
  their documentation and unverified.

## Build from source

The release binaries are built with exactly this line, so building a release
tag yourself gives the same file, byte for byte, as the binary in that
release's archive: what you compiled is what releases ship. Needs Git and Go
1.21 or later; `GOTOOLCHAIN` fetches the toolchain the release pins.

```bash
git clone https://github.com/AgentFeedback/agentfeedback.git && cd agentfeedback
git checkout vX.Y.Z
GOTOOLCHAIN=$(awk '$1 == "toolchain" { print $2 }' go.mod) CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w -X main.version=X.Y.Z -X main.commit=$(git rev-parse HEAD)" \
  -o agentfeedback ./cmd/agentfeedback
./agentfeedback version
```

`X.Y.Z` is the tag without the `v`; `GOOS` is `linux`, `darwin` or `windows`
(name the output `agentfeedback.exe`), `GOARCH` is `amd64` or `arm64`. The
toolchain is the `toolchain` line of `go.mod`, downloaded from the Go module
proxy and verified against the Go checksum database. The binary embeds the
Git revision and whether the tree was modified, so the result is identical
only from a Git checkout of the tag with no changed or untracked files (the
output names above are ignored by Git); a source archive without `.git`
builds a working but different binary. To compare with the release:
`sha256sum agentfeedback` against
`tar -xOzf agentfeedback_X.Y.Z_linux_amd64.tar.gz agentfeedback | sha256sum`
(`shasum -a 256` on macOS). [releases.md](releases.md#reproducing-a-release)
covers the archives.

Without a checkout, `go install` builds the same tag from the module proxy:

```bash
go install github.com/agentfeedback/agentfeedback/v4/cmd/agentfeedback@vX.Y.Z
```

The tag's major version must match the `/v4` in the module path: the Go
tool refuses a `v3` tag for this path. The binary lands in `$(go env GOBIN)`
(`$(go env GOPATH)/bin` when unset). It is not byte-identical to the
release: it is built without the ldflags, `-trimpath` and `CGO_ENABLED=0`
above and from the module proxy's source archive, which carries no Git
metadata, so the version comes from the toolchain's build information and
`agentfeedback version` prints `agentfeedback vX.Y.Z (commit unknown,
go1.N.M)`: the tag with its `v`, no commit, and the Go version that built
it. `doctor`'s minimum-version check reads both spellings.

## Deploy to a host

Prerequisites on the host: Docker with Compose, `curl` 7.71 or newer, `openssl`, SSH access. The
image `ghcr.io/agentfeedback/agentfeedback` is published by each release
([releases.md](releases.md)) for `linux/amd64` and `linux/arm64`, under three
tags:

| Tag | Example | Pin it |
|---|---|---|
| the version, without the `v` | `4.0.0`, `4.0.0-rc.1` | yes |
| the full commit SHA of the release | `df87a9c…` (40 hex digits); a stable release cut from the same commit as its last rc moves it to the stable build | yes |
| `latest` | stable releases only; pre-releases never move it | never |

The image is distroless: the binary `/opt/agentfeedback` (the entrypoint;
`CMD ["serve"]`, so a container started with no arguments serves), CA roots
and timezone data, and no shell, package manager or `curl`, so it carries no
healthcheck; check readiness from the host with `GET /ready`. It runs as uid
10001 with the database at `/data/agentfeedback.db` on the `/data` volume.
The package must be publicly pullable (GitHub package settings) or the host
must be logged in to GHCR.

```bash
DEPLOY_REMOTE=user@host bash scripts/deploy.sh <commit-sha>
```

`scripts/deploy.sh`:

1. copies `infra/agentfeedback/docker-compose.deploy.yml` to
   `~/agentfeedback/docker-compose.yml` on the host;
2. creates `~/agentfeedback/.env` with a generated `API_KEY` on first deploy
   and preserves it afterwards;
3. writes `AGENTFEEDBACK_IMAGE=ghcr.io/agentfeedback/agentfeedback:<commit-sha>` into that
   `.env` so the pinned image survives later `docker compose up` calls;
4. runs `docker compose pull && docker compose up -d --wait` and checks `/ready`.

Always deploy a version or a commit SHA, never `latest`, so the host pins
what it runs. `DEPLOY_REMOTE`, `DEPLOY_IMAGE` and `DEPLOY_DIR`
(remote directory, default `~/agentfeedback`, which the sections below
assume) can live in the gitignored `.private/deploy.env`.

The stack binds `127.0.0.1:8090` on the host. To serve other machines, set
`AGENTFEEDBACK_BIND_ADDRESS=0.0.0.0` in the host `.env` only inside a trusted
network or behind TLS; see [security.md](security.md).

## Configuration

Environment variables read by the binary:

| Variable | Default | Meaning |
|---|---|---|
| `API_KEY` | required | the shared key every client sends |
| `API_KEY_FILE` | unset | `serve` reads the key from this file instead (trimmed; exactly one key); setting both is an error |
| `DATABASE_PATH` | `/data/agentfeedback.db` | SQLite file; its directory must be writable |
| `HTTP_LISTEN_ADDR` | `0.0.0.0:8080` | inside the container; Compose maps it to `127.0.0.1:8090` |
| `GRACEFUL_SHUTDOWN_TIMEOUT` | `30s` | drain time for in-flight requests |
| `SERVICE_VERSION` | build default | reported in logs |
| `LOG_LEVEL` | `info` | `debug` or `info` |
| `PUBLIC_URL` | unset | base URL that `/skill` and the MCP server instructions name, such as `https://feedback.example.com`; unset derives it per request from `Host` and `X-Forwarded-Proto`; an unusable value (not http or https, credentials, a query) stops `serve` at start |
| `MCP_INSTRUCTIONS` | unset | text appended to the MCP server instructions after a blank line |

Compose-level variables (`infra/agentfeedback/.env`): `API_KEY`,
`AGENTFEEDBACK_IMAGE` (deploy stack only), `AGENTFEEDBACK_BIND_ADDRESS`,
`PUBLIC_URL`, `MCP_INSTRUCTIONS`.

### MCP and discovery

- `POST /mcp`: remote MCP server over Streamable HTTP, stateless (no
  `Mcp-Session-Id`; `GET` and `DELETE` answer 405). It takes the API key
  header like `/api/v1/*` (`Authorization: Bearer <key>` or
  `X-Api-Key: <key>`) and checks it before anything else. Six tools:
  `submit_feedback`, `list_submissions`, `get_submission`, `stats`,
  `mark_processed`, `get_schema`, each answering with the REST body of its
  route.
- `POST /mcp/{project}`: the same, with `project` preset for the
  connection; `submit_feedback`, `list_submissions` and `stats` then take no
  `project` argument. `get_submission` and `mark_processed` take ids and are
  not scoped by the preset; `get_schema` neither.
- An MCP request body may be up to 10551296 bytes (the 10 MiB create limit
  plus 64 KiB for the JSON-RPC message); a larger one is 413
  `request_too_large`. The tools apply the create and mark limits to their
  arguments.
- `GET /skill?format=skill-md|agents-md|prompt` and
  `GET /.well-known/agentfeedback.json` need no key and carry no data.
- Behind a reverse proxy, set `PUBLIC_URL`, or forward the public `Host` and
  `X-Forwarded-Proto`, so `/skill` and the MCP instructions name the address
  clients use.

## Backups

Two complementary forms.

**Physical backup** (fastest restore, a complete database file):

```bash
cd ~/agentfeedback
docker compose exec agentfeedback /opt/agentfeedback backup /tmp/backup-$(date -u +%Y%m%dT%H%M%SZ).db
mkdir -p backups
docker compose cp agentfeedback:/tmp/backup-<stamp>.db ./backups/
```

The copy goes to the container's `/tmp`, not the database volume, and is
gone when the container is recreated (the image has no `rm` to delete it
sooner).

`agentfeedback backup <dest.db>` uses SQLite's `VACUUM INTO`, which is safe on
a live database in WAL mode, so the service keeps running. It refuses an
existing destination and a `DATABASE_PATH` that does not exist. Never copy
`agentfeedback.db` from the volume while the service runs; the `-wal` file
holds committed data the main file lacks.

**Logical backup** (portable, inspectable, the migration format):

```bash
curl -sS --fail -H "Authorization: Bearer $AGENT_FEEDBACK_API_KEY" \
  "$AGENT_FEEDBACK_URL/api/v1/export" > feedback-$(date -u +%Y%m%d).ndjson
tail -n 1 feedback-*.ndjson   # the trailer: {"export_complete":true,"count":…,"sha256":…}
```

The export is format 2: a header line, one line per record (tombstones
included), and a trailer with the record count and the SHA-256 over the record
lines. A file without the trailer is incomplete. Store backups off-host and
restrict them: they contain everything agents reported.

## Restore and migration

**Restore a physical backup**: stop the service, replace `/data/agentfeedback.db`
in the volume and remove the `-wal`/`-shm` files beside it, from a throwaway
container (the image has no shell), then start it and check `/ready`:

```bash
docker compose stop agentfeedback
docker run --rm --volumes-from "$(docker compose ps -aq agentfeedback)" -v "$PWD/backups:/backups:ro" alpine:3.24.1 \
  sh -c 'rm -f /data/agentfeedback.db-wal /data/agentfeedback.db-shm && cp /backups/backup-<stamp>.db /data/agentfeedback.db && chown 10001:10001 /data/agentfeedback.db'
docker compose start agentfeedback
```

**Restore a logical export** (format 2) into the service's database:

```bash
docker compose stop agentfeedback
docker compose run --rm -v "$PWD/feedback.ndjson:/import.ndjson:ro" agentfeedback import --dry-run /import.ndjson
docker compose run --rm -v "$PWD/feedback.ndjson:/import.ndjson:ro" agentfeedback import /import.ndjson
docker compose start agentfeedback
```

`agentfeedback import [--dry-run] <file>` verifies the whole file (header,
record count, digest, every record, each `content_hash` recomputed) before it
writes anything, then restores it in one transaction. Records keep their `id`,
`uid`, `content_hash`, payload bytes, timestamps and processing fields, and
tombstones stay tombstones; the id sequence moves past the highest id in the
file, so a restored id is never handed out again. A record whose `uid` is
already stored is skipped whatever the two contain (a mark or redaction in
the file does not reach the stored row), so running the same restore twice
changes nothing.
A record whose `id`, or whose `(kind, key)` with different content, belongs to
another stored record is skipped and listed under `conflicts` (`id_taken`,
`key_mismatch`); conflicts never fail the restore. It prints one JSON line
(`imported`, `skipped`, `conflicts`, `warnings`, `first_id`, `last_id`,
`dry_run`); `--dry-run` reports the same counts and writes nothing; a
rejected file writes nothing either, and a dry run against a `DATABASE_PATH`
that does not exist reports a restore into an empty database without creating
it. Stop the service first, as above: a restore, dry run included, holds the
database's only writer until it finishes. The file is read whole into memory,
so the host needs a few times its size free. Flags go before the file name.

**Move to another server** with `agentfeedback migrate`, run from any
machine whose client is configured with the source's URL and key:

```bash
printf '%s' "$TARGET_KEY" | agentfeedback migrate --to https://target --to-key-from-stdin \
  [--kind K] [--include-kind K] [--since T] [--limit N] [--dry-run]
```

It pages the source's export and writes each chunk to the target's
`POST /api/v1/import` ([openapi.yaml](openapi.yaml)), which keeps uid,
content_hash, the timestamps and the processing fields and assigns new ids.
The target skips uids it already holds, so a re-run only adds what is
missing, and marks or redactions made on the source after a record moved do
not propagate. `install-check` rows stay behind unless `--kind` or
`--include-kind` names that kind. A record whose (kind, key) the target holds
under another uid is printed as one `conflict` line and skipped. The run
stops at the first failure and says how many records landed. `--dry-run`
sends nothing: it prints the count per kind, the first record of each, and
the source tombstones the target still holds unredacted; redact those on the
target by hand. `--to cloud` targets the hosted service at
`https://api.agentfeedback.io`.

## Upgrade

`bash scripts/deploy.sh <new-commit-sha>`. Schema changes apply automatically
at startup, forward only; a binary older than the database's schema refuses
to start rather than corrupting it. Take a physical backup first. Rolling back
the image does not roll back the schema.

From v3 to v4 the database starts over: a v4 binary refuses a v3 database
at startup and names the path. Keep the old database with the image that wrote
it, point `DATABASE_PATH` at a new file (a new volume), and start empty. A v3
export is not format 2, so `agentfeedback import` does not take it.

## Key rotation

No overlap window. Edit `API_KEY` in the host `.env`, `docker compose up -d`,
then update `AGENT_FEEDBACK_API_KEY` on every producer. Spooled payloads on
producers retry with the new key on their next call.

## Retention

Nothing is deleted automatically. `processed` means acted on, not removed.
To purge, export first, then delete with an explicit predicate from a
throwaway container that mounts the service's volume (the image has no shell
and no `sqlite3`), for example rows processed more than a year ago. It is
safe while the service runs (WAL mode, a 5 s busy timeout); the final
`chown` keeps any file it created writable by the service's uid:

```bash
docker run --rm --volumes-from "$(docker compose ps -q agentfeedback)" alpine:3.24.1 sh -c 'apk add --no-cache sqlite >/dev/null && sqlite3 -cmd ".timeout 5000" /data/agentfeedback.db "DELETE FROM submissions WHERE processed_at < (unixepoch() - 31536000) * 1000000"; s=$?; chown 10001:10001 /data/agentfeedback.db*; exit $s'
```

Client spools live in `~/.cache/agentfeedback/spool/` on each producer and
age out on their own.

## Monitoring

- `GET /health`: process is up. `GET /ready`: database answers and schema is
  current; `503` while shutting down.
- `GET /metrics` (Prometheus): `http_requests_total` and
  `http_request_duration_seconds` by route, method and code;
  `submissions_created_total` by outcome;
  `agentfeedback_submissions_unprocessed` (the backlog);
  `agentfeedback_db_bytes` (database plus WAL size); `agentfeedback_sqlite_busy_total`
  (writes that waited out the busy timeout, should stay at zero).
- Logs are JSON on stdout: `docker compose logs -f agentfeedback`.

## Uninstall

Two independent parts: the skills on each machine and the service (the compose stack below, or a service from `server install`). Take a backup before
removing any service; the data is gone with the volume.

### Skills, on every machine that has them

1. Run `agentfeedback uninstall all` first: it removes what `agentfeedback install`
   wired and restores the files it changed.
2. Find the installed copies (`agentfeedback`, `agentfeedback-docs`, `agentfeedback-triage`) in every harness skills directory you
   use, e.g. `ls -la ~/.claude/skills | grep feedback`. Entries may be symlinks into
   a shared checkout; remove the links, then the checkout if nothing else uses it.
3. Flush or discard unsent payloads first: `agentfeedback flush`
   sends whatever is spooled; or delete `~/.cache/agentfeedback/` to drop it.
4. Remove the directories or links, then `rm -rf ~/.cache/agentfeedback`, and the binary:
   `rm "$(command -v agentfeedback)"` (`scripts/install.sh` puts it in `~/.local/bin`).
5. Remove `AGENT_FEEDBACK_URL`, `AGENT_FEEDBACK_API_KEY`, `AGENT_FEEDBACK_MACHINE`,
   `AGENT_FEEDBACK_MODEL`, `AGENT_FEEDBACK_HARNESS`, `AGENT_FEEDBACK_SESSION_ID`
   `AGENT_FEEDBACK_REVIEW_DIRS` and `AGENT_FEEDBACK_TRIAGE_ROOTS` (plus
   `TYPESAFE_API_KEY` if only triage used it) from shell profiles (`grep -n AGENT_FEEDBACK ~/.zshenv ~/.zshrc ~/.bashrc ~/.profile 2>/dev/null`).
6. Remove any directive in your agent system prompt that tells agents to
   submit friction, and any hook in a review runner that submits reviews.

### The service

On the host, in the directory holding `docker-compose.yml` (default `~/agentfeedback`):

```bash
cd ~/agentfeedback
docker compose exec agentfeedback /opt/agentfeedback backup /data/final.db && docker compose cp agentfeedback:/data/final.db ./final-backup.db   # keep a copy elsewhere
docker compose down -v          # stops the container and deletes the agentfeedback-data volume
docker image rm $(sed -n 's/^AGENTFEEDBACK_IMAGE=//p' .env)
cd ~ && rm -rf ~/agentfeedback  # compose file, .env with the API key, local backups
```

Skip `-v` and the last line to keep the data for a later reinstall. Also
remove any reverse-proxy or firewall rule that exposed port 8090, and any
monitoring scrape of `/metrics`.

### A service from `server install`

Back up first with the backup command `server install` printed (its `next`)
and move the backup out of the database directory (it is written beside the
database, `/data` for compose, which the last command below removes). Then
stop the service and remove its file, for the form you installed:

```bash
systemctl --user disable --now agentfeedback.service && rm ~/.config/systemd/user/agentfeedback.service && systemctl --user daemon-reload
launchctl bootout gui/$(id -u)/dev.agentfeedback.serve && rm ~/Library/LaunchAgents/dev.agentfeedback.serve.plist && rm -rf ~/Library/Logs/agentfeedback
docker compose -f ~/.config/agentfeedback/server/compose.yaml down --rmi all
```

Then remove the server directory (`api-key`, `serve.env`, `compose.yaml`) and
the database with its `-wal` and `-shm` files, or keep them for a later
reinstall: `rm -rf ~/.config/agentfeedback/server ~/.local/share/agentfeedback`.
Paths are the defaults; `--dir`, `--db` and the `XDG_*` variables move them.
If you ran `loginctl enable-linger "$USER"` only for this server, undo it with
`loginctl disable-linger "$USER"`.

### Verify

`curl -s -o /dev/null -w '%{http_code}\n' http://<host>:8090/health` must fail
to connect; `docker ps -a | grep agentfeedback` and `docker volume ls | grep
feedback` must be empty; a fresh shell must have no `AGENT_FEEDBACK_*`
variables.
