# Export, back up, migrate

## Purpose

Answers: how to give the data to someone else, back it up, or move it to
another server.

## Commands

Export everything, then only what is newer than the last row of a previous
export:

```bash
agentfeedback export > <file>
agentfeedback export --after-id <id> > <increment-file>
```

Restore an export into a server's database. Run this on the server host with
the service stopped; `<db>` is the database path the service uses:

```bash
DATABASE_PATH=<db> agentfeedback import --dry-run <file>
DATABASE_PATH=<db> agentfeedback import <file>
```

Move the rows to another server over HTTP, from any machine whose client is
configured for the source. `TARGET_KEY` holds the target's API key; the key
goes on stdin, never on the command line:

```bash
printf '%s' "$TARGET_KEY" | agentfeedback migrate --to <target-url> --to-key-from-stdin --dry-run
printf '%s' "$TARGET_KEY" | agentfeedback migrate --to <target-url> --to-key-from-stdin
```

## Reading the output

- `export` writes NDJSON: a header line with `export_format`, one line per
  row, and a trailer with `export_complete`, `count` and `sha256`. A file
  without the trailer is incomplete: export again.
- The trailer's `last_id` is the `<id>` for the next incremental export.
- `import` prints one JSON line: `imported`, `skipped`, `conflicts`,
  `warnings`, `first_id`, `last_id`, `dry_run`. Rows keep their ids; a row
  whose uid is already stored is skipped, so a second run changes nothing.
- `migrate --dry-run` writes nothing on the target but does contact it: it
  reads the target's metadata (the import feature must be listed) and asks
  it about the source's tombstones. It prints the count per kind, the first
  record of each kind and the tombstones the target still holds unredacted.
- The real run says how many records landed. A record whose uid the target
  already holds is skipped silently; a record whose `(kind, key)` the target
  holds under another uid is skipped and printed as one `conflict` line. The
  target assigns new ids.

## What to do with it

- Always run the dry run first and show its counts to the user before the
  real run.
- An export holds every report's text, which can include source excerpts or
  private data: hand it only to someone the user names.
- After a migration, point clients at the new server and run
  `agentfeedback doctor` on one of them.

## What the hosted version adds

Receives a migration through the same import route.
