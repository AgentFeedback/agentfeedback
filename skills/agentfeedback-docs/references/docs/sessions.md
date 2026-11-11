# Session logs

```bash
agentfeedback sessions status                         # stores, counts per state, the saved selection
agentfeedback sessions list --unprocessed             # new and changed sessions
agentfeedback sessions digest --unprocessed --json    # bounded, scrubbed events; records the digest watermark
agentfeedback submit friction --stdin --context-from claude-code:<session id>#<span> < report.json
agentfeedback sessions mark claude-code:<session id> --outcome filed --ref <uid>
```

`agentfeedback sessions` reads the session logs a coding-agent harness keeps
on disk, on demand and read-only, so an agent can turn what went wrong in a
past session into feedback. The policy of the config file's `[collect]`
table is applied per session, by its recorded working directory, before the
log is opened. Digesting and marking store only watermarks; [Push](#push)
uploads eligible, scrubbed transcripts to a configured server whose
organisation enables processing, manually or through the [automatic sweep](#automatic-sweep).

Session state always lives in the data-directory database
(`${XDG_DATA_HOME:-~/.local/share}/agentfeedback/agentfeedback.db`), in local
and in remote mode. `sessions status` also reads the configured server's
session policy, subject to its cache and backoff, and prints the org's
processing status (`capture` in JSON). Listing, digesting and marking do
their own work locally; eligible CLI invocations can also trigger the sweep.
`submit` and `sessions push` send their respective content to the server.
See [Session policy settings](operate.md#session-policy-settings).
Use `agentfeedback config` or `config set sessions.quiet 10m` to change this
machine's quiet interval. The menu shows organisation policy and repository
restrictions read-only; changing a setting never grants capture permission.
See [Machine settings](operate.md#machine-settings) for all editable keys.

Server authentication is separate from collection permission. The four
[onboarding sources](operate.md#authenticate-a-client) (paste key, email
device login, agent self-enrolment, stdin enrolment token) and explicit
`agentfeedback login` reauthentication do not enable capture or relax
repository restrictions. A changed key invalidates credential-scoped cached
policy; the server's current policy still governs uploads. Keep keys,
tokens, device codes, verification URLs and claim URLs out of session logs.

## Push

```bash
agentfeedback sessions push claude-code:<session-id> --json
agentfeedback sessions push --due --limit 10 --json
agentfeedback sessions push codex:<session-id> --since 2026-01-01T00:00:00Z --dry-run --json
```

Push supports Claude Code (`claude-code-jsonl`), Codex (`codex-rollout`),
OpenCode V1 and 2 (`opencode-virtual-log`) and Cursor
(`cursor-agent-transcript`, storage only). It sends the whole complete-line
revision, not the digest or only new entries. The revision is the decimal
source byte count. An unfinished last line is excluded; changes during
capture defer the upload.

The configured server must enable processing. Collection rules and every
recorded repository's `[sessions] push = false` veto apply before export.
Unreadable, unsupported, unknown-project, disabled and denied sessions do
not upload. The last entry must be at or after this machine first observed
enabled policy; `--since` explicitly replaces that cutoff. `--due` selects
quiet sessions oldest-last-entry first, up to `--limit` (default
`sessions.push_batch`); named references bypass quiet time, not safety checks.
The run respects `sessions.push_budget` and ten-second request timeouts.

All transcript text is scrubbed before hashing or splitting into parts of
at most 4 MiB, at line boundaries where possible. JSON string values are
decoded for scrubbing, including escaped credentials; multiline private
keys are redacted across records. Capture processes records incrementally,
retaining bounded scrubbed output rather than the whole raw transcript.
Individual records still require record-sized decoding buffers. The
assembled canonical envelope must fit the server's size cap; there is no
separate raw-source size cap. Uploads resume acknowledged parts and complete
as one `kind: session` submission. An unchanged successful revision is
`replayed` without sending parts again.

Outcomes are `pushed`, `replayed`, `skipped` with a reason, `deferred`,
`suspended`, or `error`. Dry runs report `dry-run`, revision, byte and part
counts, and at most 2 KiB of scrubbed preview; they discover policy but send
no transcript. A named session that cannot be pushed makes the command exit
1. A 429 suspends the run and policy until retry is allowed; network and
server failures leave it for a later invocation. A begin conflict is
`replay_mismatch` and is not retried at that source size.

`sessions_pushed` stores only upload metadata and outcomes, scoped to the
destination, organisation, credential and consent period, separately from
digest watermarks. `session_manifests` retains the initial scrubbed upload
metadata and partition size while an upload is pending, so retries do not
change identity when repository facts or file timestamps change. Current
collection rules and repository vetoes still apply before each request.
Pending metadata is removed on completion, terminal rejection, a new
revision or a changed consent scope. Transcript text and previews are never
persisted to the database, cache, log or spool. `too_large` is not retried unless the source
shrinks or rotates, or the server's cap grows. `sessions list` shows the
pushed revision; `sessions status` shows pushed, pending and skipped counts
and the last push time for the current scope.

### Automatic sweep

In remote mode, existing turn-end hooks and eligible CLI invocations can
launch a detached, short-lived `agentfeedback sessions push --due --json`.
The trigger reads only cached policy matching the destination and credential;
it makes no network request. Previously enabled consent can retry after a
transient policy error or an elapsed `Retry-After`. A changed credential,
authorization failure, disabled or unsupported policy cannot revive consent.
The upload child always refreshes policy before uploading.

Commands that open a validated remote API client can trigger the sweep,
including `mcp` and `ui` startup. Commands with no such client, and the
diagnostic `doctor`, explicit `sessions push`, `flush` and `ingest` paths, do
not. Existing turn-end `hook` handling triggers after its bounded flush.
No new hook registrations, resident daemon or scheduler are installed.

A nonblocking `push.lock`, inherited by the child, allows at most one sweep
at a time. `push.last-started` throttles launches by `sessions.quiet`.
An abandoned run can be retried after `sessions.push_budget` plus one minute
once the operating system has released its lock; an old timestamp never
permits bypassing a still-running child's lock. Malformed, zero or future
start timestamps are repaired only after acquiring the lock. The parent
launches its own running executable, not a PATH lookup, and does not wait
for uploads; the whole hook trigger shares the hook's deadline.
Each child applies the quiet-session selection,
`sessions.push_batch` and `sessions.push_budget` described above and exits.
The cache's owner-only `push.last-run.json` records start and finish times,
pushed, replayed, skipped, deferred and error counts, and any suspension
deadline. `doctor` prints it as `last push`; both `doctor --json` and
`sessions status --json` expose it as `last_push`. It contains no transcript
text, credentials or session identifiers.

## Harnesses

| Harness | Reader | Store (moved by) | Working directory, checked before the session is opened |
|---|---|---|---|
| `claude-code` | `claude-code-jsonl` | `<config dir>/projects/<encoded launch directory>/<session id>.jsonl` (`$CLAUDE_CONFIG_DIR`, else `~/.claude`) | the encoded project directory name, before; each entry's `cwd`, after |
| `codex` | `codex-rollout` | `<codex home>/sessions/YYYY/MM/DD/rollout-*.jsonl` and `<codex home>/archived_sessions/rollout-*.jsonl` (`$CODEX_HOME`, else `~/.codex`) | the `session_meta` first line, read alone, before; `turn_context`, after |
| `copilot` | `copilot-events` | `<copilot home>/session-state/<session id>/events.jsonl` (`$COPILOT_HOME`, else `~/.copilot`) | `workspace.yaml` beside the log, before; `session.start` and `session.context_changed`, after |
| `gemini-cli` | `gemini-cli-jsonl` | `<gemini dir>/tmp/<project>/chats/session-*.jsonl` (`$GEMINI_CLI_HOME/.gemini`, else `~/.gemini`) | `<project>/.project_root`, before; the metadata line's `directories`, which the log records only for sub-agents and `/dir add`, after |
| `opencode-v1` | `opencode-sqlite` | `$OPENCODE_DB` (a relative one under `<data home>/opencode`), else `opencode.db` and `opencode-*.db` in `<data home>/opencode` (`$XDG_DATA_HOME`, else `~/.local/share`) | the session row's `directory`, before; each assistant message's `path.cwd`, after |
| `opencode-v2` | `opencode-v2-sqlite` | the same databases, read only when they have a `session_v2` table | the session row's `directory`, before |

OpenCode V1 session references use `opencode-v1:<id>`. The input alias
`opencode:<id>` resolves and marks the same session. Existing local session
state migrates to the canonical name without resetting processed progress;
saved harness selections accept the alias too. The store paths, SQLite
reader format, detector version and finding idempotency keys remain unchanged:
retrying a previously filed span still uses its original key.

OpenCode 2 (checked with 2.2.6) stores sessions in a different schema in the
same database files, and migrates a V1 database in place: it adds its own
rows with the same session ids and keeps V1's tables. A database with a
`session_v2` table is therefore read only by `opencode-v2`; the V1 reader
skips it, so a session is never listed under both names. Sessions migrated
this way are listed once, as `opencode-v2`, and their V1 state does not
carry over: they read as new, so a scan files their findings again under new
keys and `sessions push` uploads them again under `opencode-v2`.

A directory variable that is not an absolute path is ignored. A store
directory that cannot be listed, or a session file whose directory entry
cannot be read, is named in the store's `problems` and its sessions are not
listed; a watermark row whose file it hides is `unreadable`, not `absent`. A
recorded working directory that is not an absolute path is ignored, as if not
recorded. Every reader takes a result's status from the harness's own record
where there is one and otherwise from fixed texts of the harness, matched
literally, so a reworded denial or abort message in a new harness release is
missed: Claude Code and OpenCode read it as a plain error; Codex as ok
unless an exit code says otherwise; Gemini CLI as an error, or as
interrupted on a cancelled call; Copilot CLI takes denials and aborts from
records, not texts.

A session is named by its ref, `<harness>:<session id>`; an event of a
digest adds `#<span>`, the reader's id of the log entry that carries it
(per reader below).

### Claude Code

- Span: the entry's `uuid`; a tool call's span is that of the assistant
  entry carrying its `tool_use`.
- Prompt: a `user` entry with text that is not `isMeta` and carries no
  `tool_result`; one starting `[Request interrupted by user` is an interrupt.
- Results: `is_error` makes a result fail; it is denied when it contains
  `The user doesn't want to proceed with this tool use` or `has been
  denied`, interrupted when `toolUseResult.interrupted` is true or it
  contains `[Request interrupted by user`. The log records no exit code: the
  digest reads it from an `Exit code N` first line.
- Not read: subdirectories of a project directory, sub-agent transcripts
  among them. The encoding turns every byte outside `[A-Za-z0-9]` into `-`.

### Codex

- Span: the call's `call_id`; a prompt from an `item_completed`
  `UserMessage` item, its item `id`; any other entry `L<offset>`, its line's
  byte offset. A rewritten rollout changes `L` spans.
- Prompt: `event_msg` `user_message` and `item_completed` `UserMessage`, never
  `response_item` user messages, which also carry injected context.
- Results: the first output line of a call is its result. A legacy result
  (`function_call_output` text) is classified from the text Codex writes,
  never the command's output: the header lines before the first line that
  is exactly `Output:`. An exit code of 0 there is ok. Otherwise `exec
  command rejected by user`, `patch rejected by user`, `rejected by
  configuration` or `automatic approval review denied the action` in the
  header is denied (without an `Output:` line, only when the text starts
  with it), a line starting `aborted by user` interrupted, and a non-zero
  exit code in a `Process exited with code N` or `Exit code: N` header an
  error. The oldest form (`{"output", "metadata"}`) is classified by its
  `metadata.exit_code` alone. A later `item_completed` (`failed`,
  `declined`), `patch_apply_end` or `mcp_tool_call_end` refines the status,
  and the exit code when it records one; one that changes the status moves
  the call's result to its line, so an incremental digest reports it. A
  `turn_aborted` with reason `interrupted` interrupts every call still
  without a result.
- Metadata: the `session_meta` first line is read alone, one byte at a
  time, nothing past its newline, at most 1 MiB. When it is longer, is not
  `session_meta` or does not decode, and the policy has `deny_paths` or
  `opt_in_only`, the session is `denied` without being read ("the session
  metadata could not be read, so the policy could not be checked"); without
  such a policy it is read and checked by its recorded working directories.
  The read checks its first line again: a `cwd` other than the one the gate
  checked makes the session `unreadable` ("the session changed since its
  policy was checked").
- Not read: a compressed rollout (`.jsonl.zst`, listed `unsupported-format`),
  and the `history_base` prefix of a paginated rollout, kept in another file.
  The session id is the file name after `rollout-<time>-`, without the
  extension; a session id in both `sessions` and `archived_sessions` is
  listed from `sessions`, the other named in `problems`.

### Copilot CLI

- Span: the event `id`; a tool call's span is that of its
  `tool.execution_start`.
- Prompt: `user.message` with source `user` (or none) that is not an
  autopilot continuation.
- Results: the first `tool.execution_complete` of a call: error code
  `rejected` or `denied` is denied; success with no exit code or exit code 0
  is ok; anything else, success with a non-zero exit code included, is an
  error. The exit code is `shellExecution.exitCode`, else, for a shell call
  (tool `bash` or `powershell`, a start with `shellToolInfo`, or a complete
  with `shellExecution`), the closing `completed with exit code N` line.
  `permission.completed` with a `denied-` kind denies a call without a
  result, with `cancelled` interrupts it; one after the result changes
  nothing. `abort` interrupts every call without a result.
- Not read: events with an `agentId` (sub-agents), the `session-store.db`
  index and the legacy flat transcripts. Only a top-level `cwd:` scalar (or
  the JSON member `cwd`) of `workspace.yaml` is read, and only from a
  regular file of at most 64 KiB.

### Gemini CLI

- Span: the message `id`. A message written again under its id updates its
  entry in place: a rewrite of an already-digested message updates its text
  or model without reporting it again. A call's result is the first line
  that records it in a terminal status.
- Prompt: a `user` message with text, not only function responses, not
  starting with `<session_context>`, and not a slash command (a first word
  matching `/[A-Za-z][A-Za-z0-9_-]*`, so `/home/u/x fix this` is a prompt).
- Results: status `error` is an error, `cancelled` interrupted; `[Operation
  Cancelled] Reason: User denied execution.` or `Tool execution denied by
  policy.` is denied. Gemini CLI records no exit code: a `run_shell_command`
  result whose trailer (the final lines starting `Error: `, `Exit Code: `,
  `Signal: `, `Background PIDs: ` or `Process Group PGID: `) has an `Exit
  Code: N` line (N not 0) is an error with that exit code, one with a
  `Signal:` line an error without; lines the command printed are not read.
- Working directory: `.project_root` is read only from a regular file of at
  most 4 KiB, and not at all when collection is disabled.
- Not read: `chats/<parent session id>/` subdirectories (sub-agents), and
  `session-*.json` single-document sessions of releases before v0.39.0,
  listed `unsupported-format`. A `$patch` record is not applied, and
  messages a `$rewindTo` removes are kept: they happened.

### OpenCode

- Span: the message id of a prompt, the part id of a tool call.
- Prompt: a user message's text parts that are neither synthetic nor
  ignored; a user message counts once it has a part.
- Results: a `completed` tool part is ok unless `metadata.exit` is a
  non-zero integer (an error with that exit code), or null with the shell
  tool's abort note (interrupted) or timeout note (an error). An `error` part
  is interrupted when aborted, denied when the user or a rule refused it or a
  question was dismissed, else an error. A `metadata.exit` of 1.0 is exit
  code 1. An assistant message ended by
  `MessageAbortedError` is an interrupt.
- Not read: child sessions (`parent_id` set: sub-agents). A session is laid
  out as a virtual append-only log, one record per message then per part, so
  offsets, sizes and watermarks work as for a file. A record holds only the
  fields that are final once the row is complete, so OpenCode's later
  rewrites (a user message's summary, a tool part's compaction time) do not
  change it. A user message without parts, an assistant message not
  completed, or a tool part not finished, ends it like a final line without
  its newline, and the session's mtime is its `time_updated`. A session id
  in two databases is listed from the first, the other named in `problems`.
  A session whose `directory` changed since it was listed is `unreadable`.
- Open: never written, and never opened `mode=ro` (SQLite opens and creates
  the `-shm` file read-write even then): always `immutable=1`, so no lock is
  taken and no `-wal` or `-shm` file is opened or created. The database file
  and its `-wal` are checked (existence, size, mtime) before and after the
  read; a change discards the read and reads once more, and a second change
  makes it `unreadable` ("the database changed during the read"). Rows still
  only in OpenCode's write-ahead log are not seen until OpenCode checkpoints
  them into the database file, at the latest when it exits; the session
  then reads as `appended`.

### OpenCode 2

- Store: tables `session_v2`, `session_message` (one row per message, typed
  `user`, `assistant`, `system`, `idle` and others, ordered by `seq`) and
  `session_part` when present; opened, checked and retried as for V1.
- Span: the message id of a prompt, `<message id>/<index>` of a tool call
  embedded in an assistant message, `<message id>/<ordinal>` of a part row.
- Prompt: a `user` message with non-empty text.
- Results: a tool is `completed` (ok, unless `state.metadata.exit` is a
  non-zero integer: an error with that exit code) or `error` (an error; the
  2.2.6 samples show no abort or denial text to classify). An `idle` row with
  outcome `interrupted` is an interrupt.
- Layout: a virtual append-only log, one record per message then per part,
  each holding only fields final once complete; an assistant message without
  `time.completed`, or with a streaming or running tool, ends it.
  A file a tool returned is recorded without its inline `data:` content (its
  type, name and MIME type stay). A session moved to another directory
  (`location-switched`) records both directories, and both are checked
  against the `[collect]` rules.
- Not read: child sessions (`parent_id` set).

### Cursor

Hook-indexed listing and capture are supported; semantic digests are not.
`postToolUseFailure`, `stop` and `sessionEnd` record the conversation id,
absolute transcript path beneath the home directory, workspace roots and
reported cwd in the existing owner-only session counters cache (seven-day retention).
`install cursor` wires all three events; `sessionEnd` is fire-and-forget.
No transcript is opened by the hook.

Listing and push apply collection policy and repository upload vetoes to every
recorded workspace root and the reported cwd before opening the transcript.
The first root supplies the displayed project. Missing or invalid roots are
`unknown-project`; symlinks (including ancestor components) and nonregular
files are refused. A symlinked home or ancestor therefore prevents capture.
Only complete lines are captured, labelled `cursor-agent-transcript` for
both IDE and CLI sessions. Transcript mtime supplies the capture timestamps;
tool counts and model metadata are unavailable.
Automatic mixed-harness digests skip Cursor without opening its transcripts.
After seven days without a hook update, cache pruning removes the transcript
pointer; the transcript remains untouched but is no longer discoverable until
another hook indexes it.

Verification is documentation-based, using the official
[hooks contract](https://cursor.com/docs/hooks) and
[CLI changelog](https://cursor.com/docs/cli/changelog), checked 2026-11-10,
plus synthetic IDE and CLI fixtures. No installed Cursor version was
live-verified. The CLI changelog's Claude Code-compatible JSONL claim does
not establish digest compatibility; both surfaces remain storage-only.
The IDE format lacks tool results, timestamps, call ids and cwd. The
undocumented SQLite/protobuf stores (`store.db`, `state.vscdb`) are not read.

### Antigravity

Not read. `transcript.jsonl` under `<app data dir>/brain/<conversation
id>/.system_generated/logs/` records steps with a status but no working
directory or call ids; its directory differs per product surface, it is
rewritten on compaction, and versions before agy 1.2.4 omit failed steps.
`conversations/*.pb` are encrypted and the `*.db` files undocumented.

Devin: no documented local session store; not read (best effort).

## Commands

| Command | Flags | What |
|---|---|---|
| `sessions list` | `--harness H`, `--since T`, `--unprocessed`, `--project PATH` (repeatable), `--json` | every session with its state; `--since` keeps sessions whose last entry is at or after T (RFC 3339 or `30m`, `12h`, `7d`, `2w`); `--unprocessed` keeps `new` and `changed`; `--project` keeps the sessions in PATH or beneath it ([Projects](#projects)) |
| `sessions digest <harness:id>...` | `--allow-unknown-project`, `--json` | the digest of the named sessions; a `processed` session named here is digested whole again |
| `sessions digest --unprocessed` | `--harness H`, `--limit N`, `--allow-unknown-project`, `--json` | the digest of the `new` and `changed` sessions (and the `unknown-project` ones with `--allow-unknown-project`), oldest last entry first, at most N (0: no limit) |
| `sessions mark <harness:id>...` | `--outcome filed\|nothing\|skipped`, `--ref UID` (repeatable), `--json` | records the outcome and the uids filed; all refs or none; a session not digested yet is refused, and so is a uid not in the submission uid form (an RFC 9562 UUID, 8-4-4-4-12 hex) |
| `sessions status` | `--json` | per harness: the store location and state, counts per session state; the saved selection |
| `sessions status --set-selection` | `--harness H` (repeatable), `--since T`, `--limit N`, `--json` | saves the processor's selection (`since` is kept as written), then prints the status |
| `sessions skills` | `--project PATH` (repeatable), `--global`, `--json` | the skills installed for the projects at PATH and, with `--global`, the user-level ones of every harness ([Skills](#skills)); one of the two is required |

Without `--json` the output is a table (`list`), readable text with the
notice first (`digest`) or a summary (`status`, `mark`). `sessions list`
names on stderr a store that is `unreadable` or `unsupported-format`, and
each of its `problems`, always; an `absent` store only when `--harness` names
its harness. `digest` exits 1
when sessions were requested and none could be digested; its JSON then names
each in `errors`. A wrong command line exits 2.

`sessions list --json` gives a session that was read and used a skill
`skills_used`: `[{name, span}]`, the first use of each skill in log order
(the member is left out otherwise, and the table does not show it).
`sessions skills` prints the table NAME, SCOPE, HARNESSES, PATH, its
`problems` on stderr; `--json` prints `{skills, problems}`: per SKILL.md
`{name, description, path, scope, harnesses}` (`scope` `project` or
`global`; a directory several harnesses read, such as `.agents/skills`, is
one entry naming them all), sorted by scope, name and path, and per SKILL.md
or skill directory that could not be read `{path, reason}`. A relative
`--project` is taken from the working directory.

### Projects

A session is in a project when a directory it is known by is the project
directory or lies beneath it, matched as `[collect]` matches: each as
written and with its symbolic links resolved, without case on macOS and
Windows. The directories are the working directories the read recorded; for
a session refused before the read, the one checked before the open
([Harnesses](#harnesses)) and, for Claude Code, the encoded project
directory name. That name has lost the path's separators, so a Claude Code
session refused before it is opened in a sibling such as `/p/foo-bar` is
listed under `--project /p/foo` too, with its refused state only and nothing
read from it. The filter never changes a state: a `denied` session in the
project is listed `denied`, with nothing read from it. An `absent` row
records no directory and is in no project.

## Skills

A harness reads skills from user-level directories (the ones `install`
writes the skill to) and from project directories, each holding
`<name>/SKILL.md`, one level deep; a symbolically linked skill directory is
followed. The project directories, relative to the project directory and
each of its ancestors up to and including the nearest one that holds `.git`
(the project directory alone when none does). The walk stops below the
home directory: neither home nor a directory above it is a project
directory, so a `.git` in home does not turn the user-level skill
directories into project ones, and `--project` on home lists no project
skill. `sessions skills` applies the `[collect]` policy to each `--project`
path first, as for a session's working directory: a disabled or denied
project is not read, and a `problems` entry names the reason:

| Harness | Project skill directories |
|---|---|
| `claude-code` | `.claude/skills` |
| `codex` | `.agents/skills`, `.codex/skills` |
| `copilot` | `.github/skills`, `.agents/skills`, `.claude/skills` |
| `gemini-cli` | `.gemini/skills`, `.agents/skills` |
| `opencode-v1`, `opencode-v2` | `.opencode/skill`, `.opencode/skills`, `.claude/skills`, `.agents/skills` |
| `cursor` | `.cursor/skills`, `.agents/skills`, `.claude/skills`, `.codex/skills` |

Other harnesses have no project skill directories here: they were not
verified. Only the front matter of a SKILL.md is read: the YAML between a
first line `---` and the next line `---`, within the first 64 KiB, for
`name` and `description`. A SKILL.md that is not a regular file (after
symbolic links) is never opened; it, and one without that block, with front
matter that is not YAML or without a `name`, is reported in `problems`; a
subdirectory without a SKILL.md is not a skill. `name`, `path`,
`description` and `problems` are scrubbed.

`skills_used` (in `sessions list --json` and in each digested session)
names the skills a session used, from the harness's own record of a skill
use, kept only when a skill directory holds the skill: the user-level skills
of every harness and the project skills of every working directory the
session records that the `[collect]` policy allows. A use names its skill by its exact front-matter `name`, or, when
the log names a SKILL.md path, by the skill directory's base name; the
`name` reported is the front-matter name. A plugin's skill
(`plugin:skill`) is not in a skill directory and is not reported. Per reader:

| Reader | Skill use | Not seen |
|---|---|---|
| `claude-code-jsonl` | a `Skill` tool call's `input.skill`; a user text, not `isMeta`, made of the slash-command tags (`<command-name>/X</command-name>` with or without `<command-message>` before it), when X is a skill | a skill whose body was read with another tool |
| `codex-rollout` | the `<skill><name>X</name>` user message Codex injects for a `$X` mention; a tool call whose command or `path` argument names `…/skills/<X>/SKILL.md` (or `…/skill/<X>/SKILL.md`; `/` or `\` separators) | a skill Codex loaded without either; a shell command that edits a SKILL.md counts as a use |
| `copilot-events` | a `skill.invoked` event's `data.name` | a sub-agent's skill use (sub-agent events are not read) |
| `gemini-cli-jsonl` | an `activate_skill` tool call's `args.name`, as last written, at the line that first wrote it | |
| `opencode-sqlite` | a `skill` tool part's `input.name` | |
| `opencode-v2-sqlite` | a `skill` tool call's `input.id`; a `session_message` row of type `skill` (a skill the user invoked), its `name`, else its `skill` | |

The SKILL.md path rule applies to Codex only: the other harnesses record
their skill uses, and a path rule would count an agent editing a SKILL.md as
a use. Cursor sessions are storage-only and report no skill use.

## States

Precedence, first match wins:

| State | Meaning |
|---|---|
| `disabled` | `[collect] disabled`, or a recorded working directory is switched off (also by its repository's `.agentfeedback.toml`). Readers with store metadata (Codex's first line, Copilot's `workspace.yaml`, Gemini CLI's `.project_root`, OpenCode's session row) apply the whole policy, the repository's `.agentfeedback.toml` included, to that directory before the session is opened; Claude Code opens the file unless the user's `[collect] disabled` is set and finds a repository's `disabled` after the read, from the recorded working directory |
| `denied` | the working directory checked before the open ([Harnesses](#harnesses)), or a recorded one, lies within a `deny_paths` entry, or outside every `opt_in_paths` entry with `opt_in_only`; each entry matches as written and with its symbolic links resolved, without case on macOS and Windows, and a filesystem root matches every directory; the file is not opened when the directory checked before the open matches. Claude Code's encoded name loses the path's separators, so its pre-open gate is exact for `deny_paths` but may over-deny a sibling whose encoded name shares the prefix (`/a/b-c` under a `/a/b` entry), and approximate for `opt_in_paths`: such a sibling is opened, then refused once its recorded working directory is read |
| `unreadable` | opening or reading the file failed (`reason` says why) |
| `unsupported-format` | no complete line parses as an entry of the reader's format, or the session is a compressed Codex rollout or a legacy Gemini CLI `.json` session; as a store state, an OpenCode database without the expected tables and columns |
| `unknown-project` | the log records no working directory, so no policy can be checked; digested only with `--allow-unknown-project` |
| `absent` | a watermark row whose file is gone |
| `new` | no watermark row, or one never marked |
| `changed` | the file differs from the marked watermark: `reason` `appended` (larger; digested from the marked size), `truncated` (smaller) or `rotated` (different first line); the last two are digested from the start |
| `processed` | the file's complete-line size and first-line hash equal the marked watermark |

A recorded working directory that no longer exists gets the user rules only;
the repository file cannot be read and is skipped. A session refused after
the read (`disabled` or `denied` by a recorded working directory) is listed
with nothing read from the file: no `project`, `cwds`, `start`, `end`,
`counts` or `skills_used`. `list` and `status` scrub `path`, `project`,
`cwds`, `skills_used` and `problems` with the formats of `submit --scrub`.

## Watermark

Only complete lines count: a final line without its newline is neither
parsed nor counted, so a session still being written is digested up to its
last complete entry. `digest` records, per digested session, the
size it covered, the SHA-256 of the first line, the file's mtime and the
time; `mark` copies that digest watermark into the marked one. The size
covered is the complete-line size, or, when the digest is `truncated`, the
offset that completes the first event it left out: under the cap, events
are kept in the order their prompt or result line completes them, so the
watermark always lies past every kept event and the next digest reports the
rest. A retry is kept with the failed call it repeats, so a pair split by
the cap may not be reported. The mtime is recorded but does not decide the
state. A crash after `digest` and before `mark` leaves the
session `new` or `changed`, and the next run digests it again from the last
mark; a crash after `mark` loses nothing. `mark` records the watermark of the
session's latest digest, so run one triage at a time per machine: a second
run digesting the same session in between moves the watermark the first one
marks.

The store is taken as append-only. A file larger than the marked size with
the same first line is `changed` (`appended`), and the next digest starts at
the marked size; a rewrite that keeps the first line and is the same size or
larger is not detected (a larger one is read as an append). Truncation or a
rewritten first line start it over (`truncated`, `rotated`).

## Digest format

`sessions digest --json` prints one object; the MCP tool answers with the
same object.

| Member | What |
|---|---|
| `notice` | the provider-boundary notice below |
| `detector_version` | version of the digest rules; it enters the key of a submission filed from a digest |
| `sessions[]` | one digest per session, below |
| `errors[]` | `{ref, state, message}` per session refused or not found |
| `deferred[]` | refs left out to keep the run under its cap; they keep their watermark and come next run |

Each session:

| Member | What |
|---|---|
| `ref`, `harness`, `session_id` | the session |
| `project` | the first recorded working directory |
| `cwd` | the working directory of the last entry that records one |
| `start`, `end` | first and last entry time, UTC with milliseconds |
| `model` | the model of the last entry that names one (an assistant entry; for Codex the turn context) |
| `from_offset` | byte offset the digest starts at (0, or the marked size when `appended`); an incremental digest also reports a tool call made before it whose result line lies at or after it |
| `truncated` | events were left out to stay under the session cap |
| `counts` | `entries`, `prompts`, `tool_calls`, `tool_errors`, `interrupts`, `denials`, `unparsed` (complete lines that are not an entry), from `from_offset` |
| `events[]` | prompts and tool calls that did not succeed, in log order of the prompt or the call: `span`, `ref` (`<harness>:<id>#<span>`), `at`, `type` (`prompt` or `tool_call`); a prompt has `summary` (its first non-empty line); a tool call has `tool`, `args_digest` (a digest of the command, or of the input when there is no command), `status` (`error`, `denied`, `interrupted`), `error_class` (`exit`, `tool_error`, `denied`, `interrupted`), `exit_code` (the exit code the harness records, else an `Exit code N` first line of the result), `excerpt` of the result, and `self` (true when the call is agentfeedback's own command, by the same rule the hook uses; such a failure is rarely friction about the project) |
| `retries[]` | `{span, of_span, tool, args_digest}`: a call repeating an earlier failed one with the same tool and arguments |
| `denials[]` | spans of the denied tool calls |
| `flagged[]` | `{span, phrase}`: prompts with a phrase such as `still failing`, `doesn't work`, `wrong`, `why did you`, `again` |
| `final_excerpt` | the last assistant text |
| `skills_used` | `[{name, span}]`: the skills first used in the range the events cover, in log order ([Skills](#skills)): at or after `from_offset` and, in a `truncated` digest, before its watermark, so a skill past the cut comes in the next digest; counted in the session cap; `[]` when none |

Caps: a summary is at most 160 bytes, an excerpt 240; one session's digest is
at most 64 KiB (the events completed first that fit are kept, listed in log
order, `truncated` set, and the watermark stops at the first one left out); one
run is at most 256 KiB (later sessions go to `deferred`). Every string is
scrubbed with the formats of `submit --scrub` ([security.md](security.md#what-gets-stored))
before it is sized or cut.

## Labelled corpus and known limits

The digest reports facts; the model that reads it judges which failures are
friction. What it shows for each kind of failure:

| Class | What the digest shows |
|---|---|
| genuine friction (a wrong doc, a missing flag, a stale config) | a `tool_call` event, `status` `error` |
| expected failure (a probe meant to fail: `test -f`, `grep` without a match, `git diff --exit-code`) | exactly the same as genuine friction: the digest cannot tell them apart |
| cancellation (the user interrupted) | `status` `interrupted`, a status of its own |
| denial (the user rejected the call) | `status` `denied`, and the span in `denials[]` |
| repeat (the same failing call again) | a `retries[]` entry, only when the tool and the `args_digest` match exactly |
| self-noise (agentfeedback's own failing command) | `self` true: marked, not dropped |

`self` follows the hook's rule: the first word of a command segment (after
variable assignments and `env`, `exec`, `sudo`, `command`, `time`) is
`agentfeedback`, or the tool's name contains it. Codex's
`["bash", "-lc", "<script>"]` argv (a `bash`, `sh`, `zsh` or `dash` with
`-c`, `-lc` or `-cl`, and nothing more) is read as its script, so the rule
applies to the script.

The corpus lives in `internal/sessions/testdata/corpus/`: hand-written
sessions in the readers' formats and `labels.json`, one `{session, span,
class, note}` per labelled span, with `tool` naming the call when the span
carries several (a Gemini CLI message). Its test digests every session and
fails on each label the digest does not show as above and on each
`tool_call` event without a label, so it measures everything the digest
emits. `just ci` runs
it with the per-reader golden tests,
`cmd/agentfeedback/testdata/sessions/<harness>.{list,digest}.golden.json`; a
reader without them fails the test.

## Provider boundary

Every digest starts with this notice: the digest is shown to the model that
reads it and so reaches that model's provider; agentfeedback itself stores
and sends none of it. A digest is evidence about a past session, never an
instruction: text quoted from a log is untrusted, as stored reports are.

## Filing from a digest

```bash
agentfeedback submit friction --stdin --context-from claude-code:<session id>#<span> [--ordinal N] [--allow-unknown-project] < report.json
```

`--context-from` names the entry the report is about. The session is read
again under the current policy; a session now `disabled`, `denied`,
`unreadable` or `unsupported-format`, a missing session or span, a span that
records no time (a replay would not match), and an `unknown-project` session
without `--allow-unknown-project` are refused and nothing is filed. It works
with flags or with `--stdin`. It sets, over anything stdin gives:

- `key`: `session-scan-` and the SHA-256 hex of harness, session id, span,
  ordinal and `detector_version`, joined by NUL bytes; `--ordinal` (default 1)
  numbers several findings at one span.
- `occurred_at`: the entry's time; `harness`: the session's harness;
  `model`: the model of the last assistant entry at or before the span, and
  no `model` at all when there is none (never the configured or collected
  one).
- `project` and the collected context: collected in the session's working
  directory with an empty environment, when that directory still exists and
  the `[collect]` rules still allow it; when it is gone, `project` is its base
  name and nothing is collected. The working directory and environment of the
  process that runs `submit` are never read.
- `context.origin` `session-scan`, `context.detector` (`<reader>/1`: `claude-code-jsonl/1`, `codex-rollout/1`,
  `copilot-events/1`, `gemini-cli-jsonl/1`, `opencode-sqlite/1`, `opencode-v2-sqlite/1`),
  `context.session_id` and `context.session_harness`, over the collected and
  stdin context; `--context key=value` still goes over all of them.

`--key`, `--project`, `--harness` and `--model` together with
`--context-from` are a usage error. `machine` resolves as for every
submission. Origin `session-scan` turns scrubbing on, and a body filed with
`--context-from` is always scrubbed, even when `--context origin=...`
replaces the origin. Filing the same report
again replays: outcome `duplicate`, exit 0. Different content under the same
key is outcome `mismatch` (the server's 409), exit 1: file a second finding
at that span with the next `--ordinal`.

Over MCP, `submit_feedback` of `agentfeedback mcp` takes `context_from`
(`<harness>:<session id>#<span>`) and `ordinal` (default 1): the other
arguments are what `--stdin` would read, `kind` is required, and the body
is the one `submit <kind> --stdin --context-from <ref> --ordinal <n>` builds,
so the same report filed both ways is one row (the second filing is a
duplicate). An `unknown-project` session is refused: admitting it is a
human's decision, `--allow-unknown-project` on the CLI. A refused call is a
validation error on `context_from` with the CLI's message. In remote mode
the body is built on this machine and forwarded to the server's
`submit_feedback`; the HTTP `/mcp` never takes these arguments.

## Checkup report

```bash
agentfeedback report check report.json [--json]          # validate; print the grade the binary computes
agentfeedback report render report.json [--out FILE] [--open]   # validate, then write one HTML page; prints its path
```

An agent that judged a sample of sessions writes a checkup report: one JSON
file, [`checkup-report.v1.json`](../schemas/checkup-report.v1.json) (JSON
Schema 2020-12, with a full example). The agent records a label and a
reason per rubric and session; the binary computes every number. Both
commands take `-` for stdin. The report is never sent or stored.

Members (no others are allowed at any level):

| Member | What |
|---|---|
| `version` | `1` |
| `generated_at` | RFC 3339 date-time the report was written |
| `scope` | `projects[]`, `harnesses[]`, `since` (as given to `sessions list --since`, `""` for none), `sessions_scanned`, `sessions_sampled` |
| `skills` | `found[]`: `{name, harness, scope}` (`scope` `project` or `global`), from `sessions skills`; `used[]`: `{name, sessions}` |
| `sessions[]` | per sampled session: `ref` (`<harness>:<id>`), `rules_source`, `skills_used[]` (the names in the session's `skills_used`), `labels` with exactly `efficiency`, `code_quality`, `procedure_compliance` and `verbosity`, each `{label, reason}` |
| `top_findings[]` | at most 3: `{text, refs[]}` |
| `suggestions[]` | `{surface, change, evidence[], filed_uid?, diff?}`; `diff` is a unified diff, shown and never applied; `filed_uid` the uid of a submission filed for it |

Beyond the schema, `check` requires the counts to agree with the
sessions listed: `scope.sessions_sampled` equals the number of `sessions`,
and each `skills.used` entry names a skill some session's `skills_used`
names, with `sessions` the number of sessions that name it. Counts may be
spelled as any integral JSON number (`3`, `3.0`, `3e0`).

`rules_source` is `session-log` when the rules were read from what the
session itself loaded, `current-files` when from the instruction files as
they are now (which may have changed since the session). `reason` is
required and non-blank for every label, `insufficient_evidence` included.

Labels and their scores:

| Rubric | Weight | Labels |
|---|---|---|
| `efficiency` | 0.30 | `efficient` 1, `minor_detours` 0.67, `major_detours` 0.33, `unproductive` 0 |
| `code_quality` | 0.25 | `sound` 1, `minor_issues` 0.5, `unsound` 0 |
| `procedure_compliance` | 0.25 | `followed` 1, `minor_lapse` 0.5, `breached` 0 |
| `verbosity` | 0.20 | `proportionate` 1, `uneven` 0.5, `disproportionate` 0 |

Every rubric also takes `insufficient_evidence`, which is left out of its
mean. The grade:

- A rubric's score is the mean label score over its sessions not labelled
  `insufficient_evidence`. A rubric with no such session drops out, and the
  remaining weights are rescaled to sum to 1. No curve.
- The overall score is the weighted sum, rounded to four decimals, then
  lettered: A at 0.90 or more, B at 0.80, C at 0.70, D at 0.60, F below.
- When every rubric dropped out (every label `insufficient_evidence`, or no
  session), the grade is `n/a` and the score null, not F.
- Coverage (sessions with a non-empty `skills_used` out of the sessions in
  the report) and the unused skills (`skills.found` names no session's
  `skills_used` names) are shown beside the grade and never move it.

`report check` prints the grade, a table per rubric (score, effective
weight, sessions judged and `insufficient_evidence`), the coverage, the
used skills with their session counts and the unused skills. An invalid report prints one line per problem on stderr,
`<JSON pointer>: <message>`, sorted by pointer (a missing or unexpected
member is named at its own pointer), and exits 2. `--json` prints
`{valid, problems: [{pointer, message}], scores}` on stdout instead
(`scores` null when invalid): `scores` is `{grade, score, rubrics: [{name,
weight, score, judged, insufficient, labels}], coverage: {with_skills,
sessions, ratio}, used_skills: [{name, sessions}], unused_skills}`. An unreadable file exits 1, a wrong
command line 2.

`report render` checks first: an invalid report prints its problems, exits
2 and writes nothing. A valid one becomes one self-contained HTML file,
written atomically (synced, then moved into place), owner-only:
`--out FILE`, which replaces an existing file but is refused (exit 2) when
it is the report being rendered; else
`${XDG_CACHE_HOME:-~/.cache}/agentfeedback/reports/checkup-<UTC
yyyymmddThhmmssZ>.html` (directory `0700`), never replacing a page: an
existing name gets `-2`, `-3`, ... before `.html`. It prints the path
written. `--open` then starts the platform's opener (`xdg-open`, `open`,
or `rundll32 url.dll,FileProtocolHandler` on Windows) on the absolute path
without waiting; a failure to open is a warning, not an error. The page
shows the grade, the rubrics with their label counts, the scope as counts,
harnesses and `since`, coverage and the used and unused skills (computed
from `sessions`), the findings and the suggestions with their diffs (a
finding or suggestion shows how many sessions back it, not their refs).
The binary adds no path, session ref or project name of its own; the
findings, suggestion surfaces, changes and diffs appear as the agent wrote
them, scrubbed of the formats of `submit --scrub` and escaped. Its share
button draws a 1200x675 card carrying only the grade, the rubric bars, the
three findings and `agentfeedback.dev` on a canvas in the browser and saves
it as a PNG; the page loads and sends nothing
([security.md](security.md#the-client-on-a-machine)).

## MCP tools

`agentfeedback mcp` (stdio) serves three more tools over the same database
and policy, in local and in remote mode (the session state stays in the
data-directory database either way); the HTTP `/mcp` never serves them.

| Tool | Arguments | CLI |
|---|---|---|
| `sessions_list` | `harness`, `since`, `unprocessed`, `projects[]` (absolute paths; a relative one is refused) | `sessions list` |
| `sessions_digest` | `refs[]` or `unprocessed`; `harness`, `limit` (with `unprocessed`) | `sessions digest` (records the watermark, so not read-only); `--allow-unknown-project` is a human's decision and has no argument |
| `sessions_mark` | `refs[]`, `outcome`, `uids[]` | `sessions mark` |

Unknown, repeated or mistyped arguments are refused with a validation error,
as for the other tools. Their results reach the provider of the model that
calls them.
