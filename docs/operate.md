# Operate AgentFeedback

Run AgentFeedback on one machine with no server, or run a server for
several machines; wire the harnesses, back up, upgrade and migrate. One
binary and one SQLite file; a server adds one API key. Every step is a
command an agent can run.

Contents: [Local mode (no server)](#local-mode-no-server) · [Run locally](#run-locally) (a server in Docker) · [Bare binary (no Docker)](#bare-binary-no-docker) ·
[Wire the harnesses](#wire-the-harnesses) · [Build from source](#build-from-source) ·
[Deploy to a host](#deploy-to-a-host) ·
[Configuration](#configuration) · [Backups](#backups) ·
[Restore and migration](#restore-and-migration) · [Upgrading and downgrading](#upgrading-and-downgrading) ·
[Key rotation](#key-rotation) · [Retention](#retention) · [Monitoring](#monitoring) ·
[Uninstall](#uninstall)

## Run locally

A server on this machine, for trying the HTTP API or serving other machines;
reporting from this machine alone needs none ([Local mode](#local-mode-no-server)).
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

Without Docker: `go build -o bin/agentfeedback ./cmd/agentfeedback && API_KEY=dev bin/agentfeedback serve`. It listens on `127.0.0.1:8090` with the database in the data directory, `${XDG_DATA_HOME:-~/.local/share}/agentfeedback/agentfeedback.db`.

## Local mode (no server)

```bash
agentfeedback doctor                             # mode: local and the database path
agentfeedback submit friction --summary "local smoke test"
agentfeedback list
```

With no `url` in the config file and no `AGENT_FEEDBACK_URL`, every client
command works in-process against
`${XDG_DATA_HOME:-~/.local/share}/agentfeedback/agentfeedback.db`, with no
server running; `agentfeedback doctor` prints `mode: local` and the database
path. `--local` forces local mode and `--server URL` targets a server for one
invocation; a configured `url` is never ignored without `--local`. `backup
<dest.db>` and `import` default to the same file, and `serve` started later on
the same machine serves the same database, so the machine keeps one queue.
Several processes may use the database at once (SQLite in WAL mode: commands,
hooks, `ui`, a local `serve`); keep the data directory on a local filesystem,
never on a network share, where SQLite's locking does not hold.

A local write that fails (the database busy past its 5 s timeout, a full
disk) is spooled like a failed send to a server: outcome `spooled`, exit 0,
and the next client command delivers it at start, before its own work, as
does `agentfeedback flush`. When the spool cannot be written either, the body
is echoed on stderr and the command exits 1. Every spool entry records the
destination it was written for, the server URL or `local`: `flush` sends an
entry only to that destination and leaves the others in place, naming each
with the command that sends it (`flush --server URL`, or `flush --local`), so
a `url` that changes never carries pending reports to the new server and
local reports never reach a server.

### Data directory

`${XDG_DATA_HOME:-~/.local/share}/agentfeedback/` holds everything that is
the only copy of a report:

| Path | What |
|---|---|
| `agentfeedback.db` (+ `-wal`, `-shm`) | the local-mode database, also the default of `serve`, `backup` and `import`; in both modes it also holds the session watermarks of `agentfeedback sessions` (tables `sessions_seen` and `triage_state`, see [sessions.md](sessions.md)) |
| `spool/` | submissions waiting to be delivered, one `af1-*.json` file each with its destination |
| `rejected/` | submissions the destination refused for good (kept 30 days) |
| `inbox/` | envelope files written by an agent that cannot run the binary, one `*.json` each (see below) |
| `inbox/done/` | inbox files filed, renamed `<id>-<name>` |
| `inbox/rejected/` | inbox files refused, each with a `<name>.reason` beside it |

The client creates the directory `0700` and the database files `0600`
(SQLite gives `-wal` and `-shm` the database file's mode). A directory or
file that already exists keeps its mode: `agentfeedback doctor` reports every
path other users can reach with the `chmod` to run, and changes nothing. The
cache directory, `${XDG_CACHE_HOME:-~/.cache}/agentfeedback/`, holds nothing
that is the only copy of a report: the client log (`log/client.jsonl`), the
hooks' per-session counters (`sessions/`) and last-run records (`hooks/`),
the `filed/` markers and `digest/` output; a spool left there by a
version before 4.0 is reported by `doctor` and is not read any more.

The same layout applies on every operating system, macOS and Windows
included: with `XDG_DATA_HOME`, `XDG_CONFIG_HOME` and `XDG_CACHE_HOME` unset
(or not absolute), the directories are `.local/share/agentfeedback`,
`.config/agentfeedback` and `.cache/agentfeedback` under the home directory.
On Windows that is `%USERPROFILE%\.local\share\agentfeedback\agentfeedback.db`
for the database and `%USERPROFILE%\.config\agentfeedback\config.toml` for
the config file; `agentfeedback doctor` prints both. Owner-only modes are not
set or checked on Windows.

### File inbox

An agent that cannot run the binary writes one envelope, a JSON object as
`POST /api/v1/submissions` takes it, into `inbox/<name>.json` under the data
directory; the recipe is in the skill. Every local-mode command but `doctor`
delivers the inbox, after the due spool entries, and prints nothing about
it; the end-of-turn hook in remote mode (`agentfeedback hook`, or
`flush --hook` in older installs) does it within its deadline. `agentfeedback
ingest` does the same pass on demand, in either mode, and prints the counts
(`--json` for one JSON object). Nothing watches the directory.

- Each row gets `context.origin` `inbox` (a `context` that is not an object
  is kept in the payload as `context_raw`). A file without a `key` gets one
  derived from its name and bytes, so the same file delivered twice is one
  row.
- Filed (created or already stored): moved to `inbox/done/<id>-<name>`.
  Refused (not one JSON object, over the 10 MiB create limit, refused by the
  server, or a key that names another report): moved to
  `inbox/rejected/<name>` (the time added when that name is taken), with
  `<name>.reason` holding the outcome line and the server's error body. Not
  delivered now (database busy, server unreachable, wrong key or URL): left
  in place, and the pass stops there. A file that is not one JSON object and
  was written in the last minute is left too, since a writer may still be
  writing it.
- Only regular files directly in `inbox/` whose names end in `.json` and do
  not start with `.` are read, so a writer writes `.<name>.tmp` and renames
  it. A symlink or any other entry is never read: `ingest` names it on
  stderr, the client log records it, and it stays until removed. A file over
  the create limit is refused from its size, unread. Every file operation
  stays inside the inbox directory, and a file replaced under the same name
  while it was being delivered is left for the next pass, not moved.
- The writer creates `inbox/` `0700`; the client creates `done/` and
  `rejected/` `0700` and writes `.reason` files `0600`. `doctor` reports
  looser modes, as for the rest of the data directory. The inbox, like the
  database, belongs to the one OS user who owns the data directory.

### Web UI

```bash
agentfeedback ui                                 # prints http://127.0.0.1:<port>/<token>/; Ctrl-C stops it
agentfeedback ui --addr 127.0.0.1:8095           # a fixed port
agentfeedback ui --server https://feedback.example.com   # the same pages over a server, with the configured key
```

`ui` serves a read-only page over the queue until Ctrl-C, from the local
database or, with `--server` or a configured `url`, from that server. In
local mode it opens the database as every client command does, which
migrates it and delivers the due spool and inbox entries at start; the page
itself writes nothing. The pages:

- **Queue:** 50 rows a page, newest first, with filters for project,
  category, harness, model, origin (`context.origin`, matched exactly as
  typed), kind, state (open, processed, all) and since (1 h, 24 h, 7 d,
  30 d, all). `install-check` rows are left out unless a kind is given, as
  `list` does.
- **Record:** every field, the context and the payload, and the warnings
  the stored record still carries, recomputed by decoding it again (the
  warnings returned when it was created are not stored).
- **Stats:** totals, groups by kind, project, harness, category and
  origin, and the recurring content hashes, under the same filters.
- **Sessions:** not available in this version.

`--addr` takes a loopback IP or `localhost` with a port (`0`, the default,
picks one) and refuses anything else. The token in the URL is new at every
launch: open the printed URL, not a bookmark. Writes (`done`, `undo`) are
not available from the page. The threat model is in
[security.md](security.md#access-and-network).

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
`agentfeedback` binary installed, `agentfeedback init` does the whole setup
in one pass: where reports go (local unless a server is named; `config.toml`
is written only for a server), the harnesses (all detected by default), the
`doctor --e2e` check, and the next steps:

```bash
agentfeedback init                               # on a terminal: asks local or a server URL, then which harnesses
agentfeedback init --yes                         # no questions: local, every detected harness
agentfeedback init --server URL --key-from-stdin --harnesses claude-code,codex   # a server, its key on stdin
agentfeedback init --yes --no-hooks              # the skill and the rule, no hooks; --mcp an MCP entry instead, as install --mcp
```

On a terminal, a server's key is asked without echo (Enter skips it, and the
check then waits for it). A configured server is kept: `init --local`, a
different `--server` and `--key-from-stdin` are refused, and an existing
`config.toml` is never edited. With no server configured but one recorded by
an earlier `install --server`, Enter at the prompt keeps it, and without a
terminal `init` needs `--server` or `--local`. The configuration is written
after the wiring, so a refused run leaves none behind. `--json` prints one object (`status`, `mode`, `server`, `config`,
`data_dir`, `database`, `harnesses`, `hooks`, `e2e`, `restart`, `next`).
`init` never writes a repository's instruction file (`install --project`
does).

`install` wires the harnesses alone, against the configured server or local
mode:

```bash
agentfeedback install                            # list the harnesses: detected, mode, skill, mcp, hook, reminder, docs, rule; changes nothing (also --list, --json)
agentfeedback install all                        # every detected harness: the skill, hooks running agentfeedback hook, a session-start hook running agentfeedback prime, and the rule in the global instruction file
agentfeedback install claude-code --with-reminder=false   # without the session-start hook
agentfeedback install claude-code --no-hooks     # no hooks at all, the session-start one included: the skill and the rule
agentfeedback install --check                    # is the rule section current in each detected or recorded harness? changes nothing; exit 3 when one is missing or stale
agentfeedback install --project --yes            # the pointer section in this repository's AGENTS.md (or CLAUDE.md); --uninstall removes it
agentfeedback prime                              # the guidance the session-start hook prints (--format cursor: Cursor's JSON)
agentfeedback install opencode --mcp             # an MCP entry instead of the skill and the hook: stdio (agentfeedback mcp) for local, a URL for a server
agentfeedback install claude-code --docs         # also the agentfeedback-docs skill: the reference docs, schemas and OpenAPI document for integrators and operators
agentfeedback install all --dry-run              # print every file that would change, change nothing
agentfeedback uninstall all                      # remove exactly what install added
```

Harnesses: `claude-code`, `codex`, `cursor`, `opencode`, `omp`, `pi`, `copilot`, `antigravity`, `devin`, `kiro`, `cline`, `amp`, `vscode`, `gemini-cli`. A
harness counts as detected when its binary is on `PATH` or its directory
exists; a named harness that is not detected is wired anyway, with a note.
`all` is every detected harness plus any named beside it.
Unknown names exit 2. The last stdout line is a JSON outcome (`status`
`installed`, `unchanged`, `uninstalled`, `dry_run` or `error`, `harnesses`,
`changed`, `backups`, `next`); progress goes to stderr. Running install twice
changes nothing the second time (`unchanged`); a changed mode, binary path,
server, `--with-reminder`, `--no-hooks` or `--docs` replaces what the previous run added.

- **The rule.** In CLI mode install writes one section into each harness's
  global instruction file (the Rule column): a begin marker
  `<!-- agentfeedback:begin v=<skill version> hash=<16 hex digits> -->`,
  `## AgentFeedback` with one rule (surface friction; run
  `agentfeedback prime` when unsure how), and `<!-- agentfeedback:end -->`.
  It is appended after one blank line, backed up and recorded like every
  other file. The text between the markers is install's: a run replaces a
  section that is stale (another version), edited or unrecorded where it
  stands, and uninstall removes it by its markers, edited or not, with the
  blank line before it; a file with two sections, or a begin marker without
  its end, is refused (uninstall leaves it as it is, noted). `antigravity`
  and `gemini-cli` share `~/.gemini/GEMINI.md` unless `GEMINI_CLI_HOME` is
  set: one section, removed with the last of the two. `--mcp` writes no rule.
- **`--check`.** `install --check [all|<harness>...] [--project] [--json]`
  reads the global instruction files (at the locations the manifest records)
  and prints, per harness, `current`, `stale` (a section other than the one
  this binary writes), `missing` (no section, or no file), or `-` (no global
  file: `cursor`, `vscode`; or a harness the manifest records with `--mcp`);
  with no names, the detected and recorded
  harnesses. It writes nothing and takes no lock. `--json` prints
  `{"status":"current"|"not_current","harnesses":[{"name","file","rule"}]}`,
  plus `project` with `--project`. Exit 0 when every section is current, 3
  when one is missing or stale (`-` does not count), 1 on an error, 2 on
  usage.
- **`--project`.** `install --project [--yes] [--uninstall] [--dry-run]`
  writes the pointer section (the same markers; "if the `agentfeedback`
  command is installed, run `agentfeedback prime`") into the repository
  holding the working directory (the nearest ancestor with `.git`):
  `AGENTS.md`, else `CLAUDE.md` when only that exists, else a new
  `AGENTS.md`. On a terminal it asks first, naming the repository and the
  file; elsewhere it refuses without `--yes`. A section already there is
  replaced in place; `--uninstall` removes it and deletes the file only
  when not a byte is left. A symbolic link or other non-regular file is
  refused, and so is a file that changed between the read and the write.
  It writes atomically, takes no backup, never runs git and never reads or
  writes the manifest; harness names (allowed with `--check`) and `--mcp`
  exit 2.
  Commit the file yourself.

The list (`--list`, or no harness named) shows, per harness wired with the
skill and hook, the binary its hook runs and that binary's version
(`binary`, `binary_version`; `missing` when the file is gone, `unknown` when
it does not answer `version --json`). An MCP entry reads `stdio` or `url` in
the `mcp` column; a stdio entry shows the binary it runs, a URL entry none.
`install` warns on stderr and in the outcome's `warnings` when the
binary it wires is not the `agentfeedback` on `PATH`, or none is on `PATH`:
agents run the one on `PATH` and hooks the one wired, and the newer of two
binaries makes the older one fail on the local database (see
[Upgrading and downgrading](#upgrading-and-downgrading)).

On Windows `install` and `uninstall` refuse (exit 2). `install` (`--list`
included) prints the manual steps for each detected harness, or each named
one, on stderr and in the outcome's `manual` list: the skill path and a
PowerShell command that writes it (`agentfeedback skill render skill-md`, as
UTF-8; none for `vscode`, which has no skill path), the MCP entry (file, key and value): the stdio entry for local, the URL entry when a server URL is configured,
and the instruction-file text (`agentfeedback skill render agents-md`). Hooks
are not wired on Windows; undo the steps by hand.

<!-- harness-table:begin: generated by go test ./internal/harness -run TestDocsHarnessTable -update -->
| Harness | Detected by | Skill | Hook (default) | Reminder | Rule | MCP entry (`--mcp`) | Verified |
|---|---|---|---|---|---|---|---|
| `claude-code` | `claude`, `$CLAUDE_CONFIG_DIR` (default `~/.claude`) | `<claude config>/skills/agentfeedback/SKILL.md` | `<claude config>/settings.json` `hooks.PostToolUseFailure` (note) and `hooks.Stop` | `hooks.SessionStart` runs `agentfeedback prime` | `<claude config>/CLAUDE.md` | `claude mcp add-json agentfeedback ... --scope user` | live-checked 2026-10-07 |
| `codex` | `codex`, `$CODEX_HOME` (default `~/.codex`) | `~/.agents/skills/agentfeedback/SKILL.md` | `<codex home>/hooks.json` `hooks.Stop` (flush only) | `hooks.SessionStart` runs `agentfeedback prime` | `<codex home>/AGENTS.md` | marked `[mcp_servers.agentfeedback]` block in `<codex home>/config.toml` | documented 2026-10-07 |
| `cursor` | `cursor-agent`, or an `agent` that resolves into a Cursor install; `~/.cursor` | `~/.cursor/skills/agentfeedback/SKILL.md` | `~/.cursor/hooks.json` `hooks.postToolUseFailure` (note) and `hooks.stop` | `hooks.sessionStart` runs `agentfeedback prime --format cursor` | none: Cursor's User Rules live in its settings UI, not in a file | `~/.cursor/mcp.json` `mcpServers.agentfeedback` | documented 2026-10-07 |
| `opencode` | `opencode`, `${XDG_CONFIG_HOME:-~/.config}/opencode` | `<opencode>/skills/agentfeedback/SKILL.md` | plugin `<opencode>/plugins/agentfeedback.js` (on `tool.execute.after` for `bash`, and `session.idle`, which delivers the note) | not supported | `<opencode>/AGENTS.md` | `mcp.agentfeedback` in the first of `opencode.jsonc`, `opencode.json` that has an `mcp` member, else an existing `opencode.jsonc`, else `opencode.json` | live-checked 2026-10-07 |
| `omp` | `omp`, `~/.omp` | `~/.omp/agent/skills/agentfeedback/SKILL.md` | extension `~/.omp/agent/extensions/agentfeedback.ts` (on `tool_result` errors, with the note, and `agent_end`, not for subagents) | not supported | `~/.omp/agent/AGENTS.md` | `~/.omp/agent/mcp.json` `mcpServers.agentfeedback` | documented 2026-10-07 |
| `pi` | `pi`, `~/.pi` | `~/.pi/agent/skills/agentfeedback/SKILL.md` | extension `~/.pi/agent/extensions/agentfeedback.ts` (on `tool_result` errors, with the note as a next-turn message, and `agent_settled`) | not supported | `~/.pi/agent/AGENTS.md` | `~/.pi/agent/mcp.json` `mcpServers.agentfeedback`; needs pi 0.99.0 or later | documented 2026-10-07 |
| `copilot` | `copilot`, `~/.copilot` | `~/.copilot/skills/agentfeedback/SKILL.md` | file `~/.copilot/hooks/agentfeedback.json` (`postToolUseFailure`, with the note on exit 2, and `agentStop`) | not supported | `~/.copilot/copilot-instructions.md` | `~/.copilot/mcp-config.json` `mcpServers.agentfeedback` | documented 2026-10-07 |
| `antigravity` | `agy`, `~/.gemini/antigravity-cli` or `~/.gemini/antigravity` | `~/.gemini/config/skills/agentfeedback/SKILL.md` and `~/.gemini/antigravity-cli/skills/agentfeedback/SKILL.md` | `~/.gemini/config/hooks.json` `agentfeedback` (on `PostToolUse`, `PreInvocation`, which delivers the note, and `Stop`) | not supported | `~/.gemini/GEMINI.md` (shared with gemini-cli unless `$GEMINI_CLI_HOME` is set) | `~/.gemini/config/mcp_config.json` `mcpServers.agentfeedback`; the key reference in the header may not be expanded | documented 2026-10-07 |
| `devin` | `devin`, `~/.config/devin` | `~/.config/devin/skills/agentfeedback/SKILL.md` | `~/.config/devin/config.json` `hooks.Stop` (flush only) | not supported | `~/.config/devin/AGENTS.md` | `~/.config/devin/mcp_config.json` `mcpServers.agentfeedback` | documented 2026-10-07 |
| `kiro` | `kiro-cli`, `~/.kiro` | `~/.kiro/skills/agentfeedback/SKILL.md` | none | not supported | `~/.kiro/steering/AGENTS.md` | `~/.kiro/settings/mcp.json` `mcpServers.agentfeedback` | documented 2026-10-07 |
| `cline` | `cline`, `~/.cline` | `~/.cline/skills/agentfeedback/SKILL.md` | none | not supported | `~/.cline/rules/agentfeedback.md` | `~/.cline/data/settings/cline_mcp_settings.json` `mcpServers.agentfeedback`; the key reference in the header may not be expanded | documented 2026-10-07 |
| `amp` | `amp`, `<xdg>/amp` (`<xdg>` is `${XDG_CONFIG_HOME:-~/.config}`) | `<xdg>/amp/skills/agentfeedback/SKILL.md` | none | not supported | `<xdg>/amp/AGENTS.md` | `amp.mcpServers` member `agentfeedback` in `<xdg>/amp/settings.jsonc` when it exists, else `settings.json` | documented 2026-10-07 |
| `vscode` | `code`, `<Code/User>`: `<xdg>/Code/User` on Linux, `~/Library/Application Support/Code/User` on macOS | none: VS Code reads the skill the `copilot` or `claude-code` adapter installs | none | not supported | none: VS Code has no CLI mode | `<Code/User>/mcp.json` `servers.agentfeedback` | documented 2026-10-07 |
| `gemini-cli` | `gemini` (`~/.gemini` is shared with antigravity unless `$GEMINI_CLI_HOME` is set) | `$GEMINI_CLI_HOME/.gemini/skills/agentfeedback/SKILL.md` (default `~/.gemini/skills/agentfeedback/SKILL.md`) | none | not supported | `$GEMINI_CLI_HOME/.gemini/GEMINI.md` (default `~/.gemini/GEMINI.md`) | none | documented 2026-10-07 |
<!-- harness-table:end -->

- **Hooks.** Every hook runs `agentfeedback hook <harness> <event>` on the
  events in the table; `codex` and `devin` get the end-of-turn hook only,
  which flushes; `kiro`, `cline`, `amp`, `vscode` and `gemini-cli` get none
  yet. After upgrading from a release that wired `flush --hook`, run
  `agentfeedback install <harness>` again: the older entries are replaced, and
  `agentfeedback doctor` names each harness that still runs one. The
  session-start hook (on by default, `--with-reminder=false` leaves it out)
  runs `agentfeedback prime`, which prints the guidance (the skill's
  CLI text, under 8,000 bytes) for the harness to add to the session;
  Cursor's runs `prime --format cursor`, which prints
  `{"additional_context": ...}`. A rerun replaces the `skill reminder`
  entry older installs wired; `skill reminder` still works. It is supported
  on `claude-code`, `codex` and `cursor` only; elsewhere an explicit
  `--with-reminder` is noted and skipped.
- **Refused modes.** `vscode` has no skill directory of its own (VS Code
  reads the skill the `copilot` or `claude-code` adapter installs), so
  `install vscode` without `--mcp` is refused. `gemini-cli` has no MCP entry,
  so `install gemini-cli --mcp` is refused. `install all` leaves such a
  harness out with a note instead of refusing.
- **MCP header variables.** Antigravity and Cline document no
  environment-variable syntax for MCP headers. Their URL entries reference
  `${AGENT_FEEDBACK_API_KEY}`, and install notes to check the connection: a
  401 means the reference was not expanded.
- **The registry.** Install also records, per harness, the instruction
  files, hook events, MCP entry forms, session store locations and caveats.
  `install --list --json` prints each harness's `verification` (`level`:
  `documented`, `fixture-tested` or `live-checked`; `date`; `note`), and the
  table's `VERIFIED` column shows level and date.

- **The hook.** `agentfeedback hook <harness> <event>` reads the event's
  payload from stdin (at most 1 MiB) and nothing else: never the transcript
  or any other file the payload names. It counts the failed tool calls of
  each session and, when they pile up, gives the agent one short note that
  it can file a friction report (`agentfeedback submit friction ...
  --context origin=hook-nudge --context session_id=<id> --context
  session_harness=<harness>`) or ignore it. The note is given when, since
  the last note, one tool failed `same_tool` times, one command (whitespace
  collapsed) failed `same_command` times, or `session_failures` tool calls
  failed; at most `max_per_session` notes per session, `interval_minutes`
  apart, and none once the session filed a friction report (the client
  leaves an empty marker `filed/<digest of the session id>` in the cache
  directory when it logs a friction report with a `session_id` context as
  submitted, duplicate, spooled or flushed). Interrupted calls,
  permission denials and the agentfeedback commands and MCP tools themselves
  are not counted. Harnesses that show a note only at another event (the
  `PreInvocation` of Antigravity, the `session.idle` of OpenCode) get it there,
  within the same turn. Copilot shows a note only when the hook exits 2;
  that is the one case the hook exits non-zero. Stdin that does not end
  within 1 second counts as no payload, and the counting gives up 1.5
  seconds after the start (a session file another hook keeps locked
  included), logging one error line and giving no note. At the end of a turn in
  remote mode the hook then sends the spool's due entries, as
  `flush --hook` does: it stops sending 3.5 seconds after it starts and
  returns by 4.5 seconds (inside the 5-second timeout the hook is given);
  local mode needs no flush and opens no database. The hook prints nothing
  but the note, never writes to stderr, logs any failure to `client.jsonl`
  and otherwise exits 0. `agentfeedback flush --hook`, which older installs
  wired, still works unchanged. Hook commands hold the binary's
  resolved absolute path, which may contain only letters, digits and
  `/ . _ - + @ , =`, single-quoted when it holds anything outside ASCII
  letters, digits and `/ . _ - +`; rerun install after moving the binary.
- **Nudge settings.** The `[detect]` table of `config.toml`: `nudge`
  (default `true`; `false` keeps counting and gives no note), `same_tool`
  (3), `same_command` (2), `session_failures` (5), `interval_minutes` (20),
  `max_per_session` (3); a value of 0 or less keeps the default. A
  repository's `.agentfeedback.toml` may set `[detect] nudge = false` for
  work under it, and a directory whose collection is switched off gets no
  note either. A `config.toml` that cannot be read gives no note, and so
  does a payload without an absolute `cwd` when `[collect]` sets
  `disabled` or `opt_in_only`.
- **Hook state and diagnostics.** Counters (no command text) live in
  `${XDG_CACHE_HOME:-~/.cache}/agentfeedback/sessions/<harness>/<session>-<digest>.json`
  (the session id cut to 40 bytes, then 16 hex digits of its SHA-256), with
  a `.lock` file beside each, owner-only, and are deleted 7 days after their
  last change, as are `filed/` markers. Every run
  writes `hooks/<harness>.json` (`ts`, `event`) beside them; the OpenCode,
  omp and pi plugins write `hooks/<harness>.spawn-error.json` when they
  cannot start the binary. `agentfeedback doctor` prints when each
  harness's hook last ran and reports a spawn error newer than that.
- **`--docs`.** The `agentfeedback-docs` skill goes to
  `<skills dir>/agentfeedback-docs/`, beside the skill's directory in the
  table (for `--mcp` too, where no skill is installed): a `SKILL.md` that maps
  `references/`, which holds this binary's copies of `docs/`, the OpenAPI
  document, `schemas/` and both install playbooks. A run without `--docs`
  removes it; an edited file in it is refused like an edited skill.
- **`--mcp`.** The MCP entry replaces the skill and the hook for that
  harness: the server's MCP instructions teach the agent, and nothing is
  spooled without the CLI. The entry follows the server: for local it is a
  stdio entry that runs the binary's resolved absolute path with the argument
  `mcp` (`agentfeedback mcp`, the same tools on the local database, no key);
  for a server it points at `<server>/mcp` and reads the key from
  `AGENT_FEEDBACK_API_KEY` in the harness's environment, and install warns
  when that is not set. Rerun install after moving the binary or changing
  the server: the entry is replaced. `--with-reminder` does not apply with
  `--mcp` (noted when given explicitly).
- **The server.** `--server local|cloud|URL`, else `url` in `config.toml`,
  else the server the last install recorded, else a prompt on a terminal
  (an empty answer means local), else local: with nothing named, install
  wires the harnesses for local mode and records `local`. A `--server` that differs from
  `config.toml` is refused: install never changes the configured server
  (`doctor --init --force` does). Install never writes `config.toml`; when a
  server is named and the file does not exist, `next` holds the
  `doctor --init` command.
- **Edits and backups.** JSON and JSONC files are edited in place, keeping
  comments, trailing commas and formatting; nothing else in them changes.
  Before the first change to an existing file install copies it to
  `<file>.agentfeedback-backup`. Writes are atomic and stop when a file
  changes while install runs. A file with the same key twice in one object
  is refused (uninstall leaves it as it is and keeps its backup). A
  configuration file (JSON, JSONC or TOML) or global instruction file that is a symbolic link is edited
  through the link: the target is written atomically in its own directory,
  the link stays, and the backup sits beside the link. A dangling link, or
  one to something other than a regular file, is refused. Two configuration
  files that are the same file (one links to the other, or both link to one
  target) are refused. A linked skill (file or directory), plugin or hook
  file that install owns is refused: remove it and run install again;
  uninstall leaves such a link in place with a note. Uninstall restores a linked file's target
  from the backup through the link; a file install created that is now a
  link only loses the agentfeedback entries.
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
  reused. The manifest records the Codex home, Claude Code configuration
  directory and Gemini CLI `.gemini` directory of the harnesses it keeps; a
  later run under another `CODEX_HOME`, `CLAUDE_CONFIG_DIR` or
  `GEMINI_CLI_HOME` keeps using the recorded location for
  that harness, notes it, and runs `claude` with `CLAUDE_CONFIG_DIR` as
  recorded: set to the recorded directory when it was set at install, even
  to `~/.claude`, and unset when it was unset. Install refuses a
  manifest that names a path it does not write under the current
  environment: run it with the `HOME`, `XDG_CONFIG_HOME`, `CODEX_HOME`,
  `CLAUDE_CONFIG_DIR` and `GEMINI_CLI_HOME` of the install, or fix the
  manifest.
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
- **`CLAUDE_CONFIG_DIR`** moves Claude Code's settings, skills and
  `.claude.json` (to `$CLAUDE_CONFIG_DIR/.claude.json`), and install follows
  it. Set to `~/.claude` counts as set: Claude Code then reads
  `~/.claude/.claude.json`, not `~/.claude.json`.
- **`GEMINI_CLI_HOME`** replaces the home directory for Gemini CLI, which
  then keeps its skills and `GEMINI.md` in `$GEMINI_CLI_HOME/.gemini`;
  `install gemini-cli` follows an absolute value and ignores a relative one.
  Antigravity does not read it and keeps `~/.gemini`.
- **Claude Code's MCP entry** is checked against `.claude.json`
  (`~/.claude.json`, or `$CLAUDE_CONFIG_DIR/.claude.json` when the variable
  is set; read only): an entry removed by hand is added again by install
  and skipped by uninstall; one pointing at another URL, or running another
  command, is refused by install and left in place by uninstall. When `claude` is missing or
  `claude mcp remove` fails, uninstall goes on and notes the command to run
  by hand:
  `claude mcp remove agentfeedback --scope user`.
- **Codex** runs a new hook only after you trust it in `/hooks`. It passes a
  stdio server only the variables `env_vars` lists, so its stdio entry lists
  `XDG_DATA_HOME`, `XDG_CONFIG_HOME`, `XDG_CACHE_HOME` and `AGENT_FEEDBACK_URL`.
- **Cursor** also runs Claude Code's hooks and skills.
- **Verification status.** The table's Verified column and `install --list`
  show how far each harness was checked: Claude Code by the live harness
  gate; OpenCode loads the skill and connects the MCP entry; the hook
  commands, the OpenCode plugin and the omp extension ran outside a live
  session; every other harness is written from its documentation, dated in
  the table.

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
| `DATABASE_PATH` | `${XDG_DATA_HOME:-~/.local/share}/agentfeedback/agentfeedback.db` (created on first start); the image sets `/data/agentfeedback.db` | SQLite file; its directory must be writable, and an explicitly set one must already exist |
| `HTTP_LISTEN_ADDR` | `127.0.0.1:8090`; the image sets `0.0.0.0:8080`, which Compose maps to `127.0.0.1:8090` | listen address of `serve` |
| `GRACEFUL_SHUTDOWN_TIMEOUT` | `30s` | drain time for in-flight requests |
| `SERVICE_VERSION` | build default | reported in logs |
| `LOG_LEVEL` | `info` | `debug` or `info` |
| `PUBLIC_URL` | unset | base URL that `/skill` and the MCP server instructions name, such as `https://feedback.example.com`; unset derives it per request from `Host` and `X-Forwarded-Proto`; an unusable value (not http or https, credentials, a query) stops `serve` at start |
| `MCP_INSTRUCTIONS` | unset | text appended to the MCP server instructions after a blank line |
| `INGEST_SCRUB` | `off` | `on` replaces known secret formats in every string value of a submission body (never member names) with `[REDACTED:<class>]` before decoding, storing and hashing it, and logs the counts per class (never the text); the response carries no warning. Turning it on changes the `content_hash` of a body that contains a matched secret, so a keyed replay of a row stored with scrubbing off answers `replay_mismatch`. Any value other than `on` or `off` stops `serve`, `import` and `backup` at start (they read the same environment); `import` never scrubs ([security.md](security.md#what-gets-stored)) |

Compose-level variables (`infra/agentfeedback/.env`): `API_KEY`,
`AGENTFEEDBACK_IMAGE` (deploy stack only), `AGENTFEEDBACK_BIND_ADDRESS`,
`PUBLIC_URL`, `MCP_INSTRUCTIONS`, `INGEST_SCRUB`.

### MCP and discovery

- `POST /mcp`: remote MCP server over Streamable HTTP, stateless (no
  `Mcp-Session-Id`; `GET` and `DELETE` answer 405). It takes the API key
  header like `/api/v1/*` (`Authorization: Bearer <key>` or
  `X-Api-Key: <key>`) and checks it before anything else. Six tools:
  `submit_feedback`, `list_submissions`, `get_submission`, `stats`,
  `mark_processed`, `get_schema`, each answering with the REST body of its
  route. `agentfeedback mcp` serves the same tools over stdio on the local
  database, with no key; it refuses when a server URL is configured (pass
  `--local`, or point the harness at `<server>/mcp`).
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

The export carries records only; the session watermarks of
`agentfeedback sessions` (`sessions_seen`, `triage_state`) are not in it, and
a database restored from an export offers every session as `new` again.
`agentfeedback backup` copies the whole database, those tables included.

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
sends no record (it still reads the target's metadata and tombstones): it prints the count per kind, the first record of each, and
the source tombstones the target still holds unredacted; redact those on the
target by hand. `--to cloud` targets the hosted service at
`https://api.agentfeedback.io`.

## Upgrading and downgrading

The newest binary wins. Every binary migrates the database forward when it
opens it (`serve` at startup, a local-mode command on its first request), and
a binary older than the database's schema refuses it rather than writing rows
the schema no longer expects: the command exits 1 with `database schema is
newer than this binary supports`, and `client.jsonl` gets an `error` line
whose reason starts with `schema_too_new`. Harness hooks log the same line and
still exit 0, so a stale hook binary shows up only there and in
`agentfeedback doctor`, which lists the binary each harness's hook runs, its
version, and the `agentfeedback` on `PATH`, and reports a hook binary that is
missing, does not answer `version --json`, or is older than the one running
`doctor`. Keep one binary per machine,
and after an upgrade rerun `agentfeedback install <harness>` from it when
`doctor` names an older one.

Upgrade: replace the binary (or `bash scripts/deploy.sh <new-commit-sha>` for
the image) after a backup. Schema changes are forward only; rolling back the
binary or the image does not roll back the schema.

Downgrade: with the newest binary, `agentfeedback backup <copy.db>` and
`agentfeedback export > records.ndjson` (or keep an export you already have);
then move `agentfeedback.db` and its `-wal` and `-shm` files aside (a
server with its own `DATABASE_PATH` points that at a new file instead) and
run the older binary's `agentfeedback import records.ndjson`, which creates
the database. Import keeps the ids. The backup stays as the copy to return
to.

From v3 to v4 the database starts over: a v4 binary refuses a v3 database
at startup and names the path. Keep the old database with the image that wrote
it, point `DATABASE_PATH` at a new file (a new volume), and start empty. A v3
export is not format 2, so `agentfeedback import` does not take it.

## Key rotation

No overlap window. For the compose stacks, edit `API_KEY` in the host `.env`
and run `docker compose up -d`; for a server from `serve --init`, run
`agentfeedback serve --init --force` and the restart command of your form
([Bare binary](#bare-binary-no-docker)). Then update the key on every client
(`agentfeedback doctor --init --url <URL> --key-from-stdin`, or
`AGENT_FEEDBACK_API_KEY`). Spooled payloads on clients retry with the new
key on their next call. Local mode has no key.

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

Client spools live in `~/.local/share/agentfeedback/spool/` on each producer
and age out on their own (frictions after 20 hours, other kinds after 30
days; an expiry is an `error` line in `client.jsonl`).

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
   sends whatever is spooled; or delete `~/.local/share/agentfeedback/spool/`
   and `~/.local/share/agentfeedback/rejected/` to drop it.
4. Remove the directories or links, then `rm -rf ~/.cache/agentfeedback`
   (the client log and digests), and the binary:
   `rm "$(command -v agentfeedback)"` (`scripts/install.sh` puts it in `~/.local/bin`).
   `~/.local/share/agentfeedback/` also holds the local-mode database; back
   it up with `agentfeedback backup` first if the reports matter, then remove it.
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
