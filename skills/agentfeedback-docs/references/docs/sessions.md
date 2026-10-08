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
past session into feedback. Nothing runs in the background. The policy of the
config file's `[collect]` table is applied per session, by its recorded
working directory, before the log is opened. Nothing from a log is stored or
sent anywhere: the data-directory database keeps only the watermark rows of
the sessions you digest and mark, and the submissions you file.

Session state always lives in the data-directory database
(`${XDG_DATA_HOME:-~/.local/share}/agentfeedback/agentfeedback.db`), in local
and in remote mode; with a server `url` configured, no `sessions` command
contacts the server, and only what you file with `submit` reaches it.

## Harnesses

| Harness | Reader | Store (moved by) | Working directory, checked before the session is opened |
|---|---|---|---|
| `claude-code` | `claude-code-jsonl` | `<config dir>/projects/<encoded launch directory>/<session id>.jsonl` (`$CLAUDE_CONFIG_DIR`, else `~/.claude`) | the encoded project directory name, before; each entry's `cwd`, after |
| `codex` | `codex-rollout` | `<codex home>/sessions/YYYY/MM/DD/rollout-*.jsonl` and `<codex home>/archived_sessions/rollout-*.jsonl` (`$CODEX_HOME`, else `~/.codex`) | the `session_meta` first line, read alone, before; `turn_context`, after |
| `copilot` | `copilot-events` | `<copilot home>/session-state/<session id>/events.jsonl` (`$COPILOT_HOME`, else `~/.copilot`) | `workspace.yaml` beside the log, before; `session.start` and `session.context_changed`, after |
| `gemini-cli` | `gemini-cli-jsonl` | `<gemini dir>/tmp/<project>/chats/session-*.jsonl` (`$GEMINI_CLI_HOME/.gemini`, else `~/.gemini`) | `<project>/.project_root`, before; the metadata line's `directories`, which the log records only for sub-agents and `/dir add`, after |
| `opencode` | `opencode-sqlite` | `$OPENCODE_DB` (a relative one under `<data home>/opencode`), else `opencode.db` and `opencode-*.db` in `<data home>/opencode` (`$XDG_DATA_HOME`, else `~/.local/share`) | the session row's `directory`, before; each assistant message's `path.cwd`, after |

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

### Cursor

Not read. The agent-transcripts JSONL under
`~/.cursor/projects/*/agent-transcripts/`, the path Cursor's hooks pass,
records no tool results, timestamps, call ids or working directory. The
cursor-agent `store.db` and the IDE's `state.vscdb` hold them, but are
undocumented SQLite with protobuf content that changes between versions.

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
| `sessions list` | `--harness H`, `--since T`, `--unprocessed`, `--json` | every session with its state; `--since` keeps sessions whose last entry is at or after T (RFC 3339 or `30m`, `12h`, `7d`, `2w`); `--unprocessed` keeps `new` and `changed` |
| `sessions digest <harness:id>...` | `--allow-unknown-project`, `--json` | the digest of the named sessions; a `processed` session named here is digested whole again |
| `sessions digest --unprocessed` | `--harness H`, `--limit N`, `--allow-unknown-project`, `--json` | the digest of the `new` and `changed` sessions (and the `unknown-project` ones with `--allow-unknown-project`), oldest last entry first, at most N (0: no limit) |
| `sessions mark <harness:id>...` | `--outcome filed\|nothing\|skipped`, `--ref UID` (repeatable), `--json` | records the outcome and the uids filed; all refs or none; a session not digested yet is refused, and so is a uid not in the submission uid form (an RFC 9562 UUID, 8-4-4-4-12 hex) |
| `sessions status` | `--json` | per harness: the store location and state, counts per session state; the saved selection |
| `sessions status --set-selection` | `--harness H` (repeatable), `--since T`, `--limit N`, `--json` | saves the processor's selection (`since` is kept as written), then prints the status |

Without `--json` the output is a table (`list`), readable text with the
notice first (`digest`) or a summary (`status`, `mark`). `sessions list`
names on stderr a store that is `unreadable` or `unsupported-format`, and
each of its `problems`, always; an `absent` store only when `--harness` names
its harness. `digest` exits 1
when sessions were requested and none could be digested; its JSON then names
each in `errors`. A wrong command line exits 2.

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
with nothing read from the file: no `project`, `cwds`, `start`, `end` or
`counts`. `list` and `status` scrub `path`, `project`, `cwds` and `problems`
with the formats of `submit --scrub`.

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
  `copilot-events/1`, `gemini-cli-jsonl/1`, `opencode-sqlite/1`),
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

## MCP tools

`agentfeedback mcp` (stdio) serves three more tools over the same database
and policy; the HTTP `/mcp` never serves them.

| Tool | Arguments | CLI |
|---|---|---|
| `sessions_list` | `harness`, `since`, `unprocessed` | `sessions list` |
| `sessions_digest` | `refs[]` or `unprocessed`; `harness`, `limit` (with `unprocessed`) | `sessions digest` (records the watermark, so not read-only); `--allow-unknown-project` is a human's decision and has no argument |
| `sessions_mark` | `refs[]`, `outcome`, `uids[]` | `sessions mark` |

Unknown, repeated or mistyped arguments are refused with a validation error,
as for the other tools. Their results reach the provider of the model that
calls them.
