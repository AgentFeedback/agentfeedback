// Package sessions lists, digests and marks the session logs coding-agent
// harnesses keep on disk, so a processor can turn what went wrong in a
// session into feedback. It reads files and the local database only; it
// sends nothing anywhere.
//
// # Readers
//
// One reader per log format, each in its own file, which registers it from
// an init function; readerOrder fixes the listing order by harness, and
// its name is the registry's Sessions.Reader. A reader whose store is not
// in a format it understands reports the store unsupported-format.
// Harnesses without a registered reader are not listed. The readers, by
// registry name:
//
//   - claude-code-jsonl (claude.go): <ClaudeConfigDir>/projects/<encoded
//     launch cwd>/<session-uuid>.jsonl, where the encoding turns every byte
//     outside [A-Za-z0-9] into '-'. Subdirectories of a project directory
//     are not read. Gated on the encoded directory name.
//   - codex-rollout (codex.go): <CodexHome>/sessions/YYYY/MM/DD/rollout-*.jsonl
//     and <CodexHome>/archived_sessions/rollout-*.jsonl. Gated on the cwd of
//     the session_meta first line, read before the rest of the file.
//   - copilot-events (copilot.go): <CopilotHome>/session-state/<session
//     id>/events.jsonl. Gated on the cwd of the workspace.yaml beside it.
//   - gemini-cli-jsonl (gemini.go): <GeminiDir>/tmp/<project>/chats/session-*.jsonl.
//     Gated on the path in <project>/.project_root.
//   - opencode-sqlite (opencode.go): Env.OpenCodeDB, else opencode.db and
//     opencode-*.db in <DataHome>/opencode, opened read-only. Gated on the
//     session row's directory.
//
// Cursor and Antigravity stores were examined and are not read: their
// registry records say why. A directory of a store that cannot be listed
// is named in Store.Problems and its sessions are not listed.
//
// # Policy before opening
//
// The user's [collect] table (Env.Policy) is applied per project directory
// before any session file in it is opened: Disabled makes every session
// there StateDisabled; a deny_paths entry whose encoded form equals the
// directory name, or is a '-'-terminated prefix of it, makes it
// StateDenied, and so does opt_in_only when no opt_in_paths entry matches
// the same way. Each entry is matched as written and, when it differs,
// with its symbolic links resolved. Such a file is listed from its directory entry and never
// opened. After a file is read, every working directory it records is
// checked with collect.Check (collect.CheckUser when the directory no
// longer exists, so no repository file applies); one disabled directory
// makes the session StateDisabled, one denied directory StateDenied.
//
// The Claude Code reader gates on the encoded project directory name. A
// reader whose store records a session's working directory in metadata
// kept apart from the session (a database row, a workspace file, a
// .project_root file), or in a first line read alone (Codex), gates on
// that directory instead: Disabled makes the session StateDisabled, and
// the directory is checked like a recorded one before the session is
// opened. Every directory the session records is checked again after the
// read.
//
// # States
//
// Precedence: disabled, denied, then unreadable (open or read failed),
// unsupported-format (no line parses as an entry, or a compressed Codex
// rollout, a legacy Gemini CLI .json session, an OpenCode database without
// the expected tables),
// unknown-project (the file parses but records no cwd), then the watermark
// states: absent (a sessions_seen row whose file is gone), new (no row, or
// a row never marked), processed (complete-line size and first-line hash
// equal to the marked watermark), changed (Reason "appended": larger,
// digested from the marked size; "truncated" or "rotated": smaller or a
// different first line, digested from 0). Only complete lines count: a
// final line without '\n' is neither parsed nor counted in the size.
//
// # API
//
//   - List: every session of the selected harnesses with its state.
//   - Digest: the bounded, scrubbed digest of the selected sessions (by
//     ref, or the unprocessed ones up to a limit); records the digest
//     watermark of each digested session. Refused sessions come back as
//     errors, sessions beyond MaxRunBytes as Deferred (no watermark).
//   - Mark: records an outcome (filed, nothing, skipped) and the uids of
//     the submissions filed, copying the digest watermark; a session not
//     digested yet is refused with store.ErrNotDigested.
//   - Status and SetSelection: counts per state and the processor's saved
//     selection (triage_state key "selection").
//   - Locate: the facts of one entry (span) of a session, for a submission
//     that cites it; Key: the idempotency key of such a submission.
//
// A ref names a session as "<harness>:<session id>"; an event's ref adds
// "#<span>", the reader's id of the log entry that carries it: the uuid
// (Claude Code), the call id, item id or "L<line offset>" (Codex), the
// event id (Copilot), the message id (Gemini CLI), the message or part id
// (OpenCode).
package sessions
