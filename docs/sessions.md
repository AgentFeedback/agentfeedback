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

Only Claude Code is read in this release (`claude-code`, reader
`claude-code-jsonl`): `<config dir>/projects/<encoded launch directory>/<session id>.jsonl`,
where the config directory is `$CLAUDE_CONFIG_DIR` when it is set to an
absolute path, else `~/.claude`, and the encoding turns every byte outside
`[A-Za-z0-9]` into `-`. Subdirectories of a project directory, subagent
transcripts among them, are not read. A project directory that cannot be
listed, or a session file whose directory entry cannot be read, is named in
the store's `problems` and its sessions are not listed; a watermark row whose
file it hides is `unreadable`, not `absent`. A recorded working directory
that is not an absolute path is ignored, as if not recorded.

A session is named by its ref, `<harness>:<session id>`; an event of a
digest adds `#<span>`, the `uuid` of the log entry that carries it.

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
notice first (`digest`) or a summary (`status`, `mark`). `digest` exits 1
when sessions were requested and none could be digested; its JSON then names
each in `errors`. A wrong command line exits 2.

## States

Precedence, first match wins:

| State | Meaning |
|---|---|
| `disabled` | `[collect] disabled`, or a recorded working directory is switched off (also by its repository's `.agentfeedback.toml`); the file is not opened only for the user's `[collect] disabled`: a repository file's `disabled` is found after the file is read, from the recorded working directory |
| `denied` | the project directory, or a recorded working directory, lies within a `deny_paths` entry, or outside every `opt_in_paths` entry with `opt_in_only`; each entry matches as written and with its symbolic links resolved, without case on macOS and Windows, and a filesystem root matches every directory; the file is not opened when the project directory matches. The encoded name loses the path's separators, so this pre-open gate is exact for `deny_paths` but may over-deny a sibling whose encoded name shares the prefix (`/a/b-c` under a `/a/b` entry), and approximate for `opt_in_paths`: such a sibling is opened, then refused once its recorded working directory is read |
| `unreadable` | opening or reading the file failed (`reason` says why) |
| `unsupported-format` | no complete line parses as an object with a string `type` |
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
| `model` | the model of the last assistant entry |
| `from_offset` | byte offset the digest starts at (0, or the marked size when `appended`); an incremental digest also reports a tool call made before it whose result line lies at or after it |
| `truncated` | events were left out to stay under the session cap |
| `counts` | `entries`, `prompts`, `tool_calls`, `tool_errors`, `interrupts`, `denials`, `unparsed` (complete lines that are not an entry), from `from_offset` |
| `events[]` | prompts and tool calls that did not succeed, in log order of the prompt or the call: `span`, `ref` (`<harness>:<id>#<span>`), `at`, `type` (`prompt` or `tool_call`); a prompt has `summary` (its first non-empty line); a tool call has `tool`, `args_digest` (a digest of the command, or of the input when there is no command), `status` (`error`, `denied`, `interrupted`), `error_class` (`exit`, `tool_error`, `denied`, `interrupted`), `exit_code` when the result starts with `Exit code N`, and `excerpt` of the result |
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
- `context.origin` `session-scan`, `context.detector` (`claude-code-jsonl/1`),
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
