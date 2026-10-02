# Fix-it session

## Purpose

Answers: what today's triage does with the open queue. The flow covers
frictions only. Pull every open friction, cluster by root cause, verify each cluster read-only, interview the
user once, act on what the user selected, and mark rows processed last. The
interview is the action checkpoint: no repository edits and no processed
marks before it.

## Commands

**Phase 0: pull.**

```bash
DIGEST=$(agentfeedback digest --kind friction)
cat "$DIGEST/digest.md"
```

`digest --kind friction` pulls every open friction with its payload into a
fresh directory under the cache directory and prints only that directory on
stdout. On a non-zero exit, stop; after exit 2 (a pulled row was already
processed), pull again. For one row in full, or every open friction as JSON
with its payload (one body per page):

```bash
agentfeedback get <id>
agentfeedback list --open --kind friction --all --json --include payload
```

**Phase 1: cross-check what is already fixed.** The repositories come from
`context.git_remote` in `$DIGEST/index.json` (or each `<id>.json`);
`digest.md` carries no remote. Resolve each to a local checkout with the
checkout rule in [`SKILL.md`](../SKILL.md), then in every checkout:

```bash
git -C <checkout> log --since="<previous-triage-date> 00:00:00" --format='%h %s%n%b' | grep -inE 'frictions? ?#?[0-9]+(/[0-9]+)*'
```

Phases 2 to 5 run no fixed commands; they are described below.

**Phase 6: close out.** Re-check the queue, then one `done` call per distinct
verdict and resolution:

```bash
agentfeedback list --open --kind friction --all
agentfeedback done <id> --verdict fixed --ref <commit> --resolution "<resolution>"
agentfeedback done <duplicate-id> --verdict duplicate --ref <uid> --resolution "<resolution>"
```

## Reading the output

**Digest.** The directory holds one `<id>.json` per row, `index.json` (every
row) and `digest.md`, which groups rows by `project`, then `category`, and
marks rows sharing a `content_hash` as exact repeats. Read `digest.md` in
full before anything else. If it reports zero rows, say so and stop.

**Phase 1.** Keep the `00:00:00`: git reads a bare date as that date at the
current clock time, so commits earlier that day silently disappear. Any
pulled id named in a commit goes into the no-action ledger as
`FIXED (commit <sha>)` once you confirm the commit is on the default branch
and not reverted. A commit that names an id is supporting evidence, not
proof.

**Phase 2: cluster by root cause.** Group by mechanism, not wording. Two
reports with different summaries about the same broken flag are one cluster;
one report that describes two defects is two. Signals, in priority order:

1. Same `content_hash`: exact repeats, already marked in the digest.
2. Same `project` and overlapping `details` or `suggested_fix`.
3. Same tool or file named across projects.

Count reports per cluster. More independent reports means higher priority,
but a single severe report (data loss, silent wrong result) outranks a
frequent cosmetic one. Watch for later reports that retract or correct
earlier ones. Write the id-to-cluster map down and check every pulled id
appears exactly once.

Optional advice: `scripts/cluster.py` suggests same-defect pairs through
TypeSafe, which sends report text off the machine. Use it only after the
user approves each exact repository remote in this run, and read
[`reference/clustering.md`](../reference/clustering.md) first for the
consent steps, commands and output. Its groups are suggestions; phase 3
still decides.

**Phase 3: validate each cluster, read-only.** Establish one verdict with
evidence:

| Verdict | Meaning |
|---|---|
| `CONFIRMED-OPEN` | defect reproduced or located at file:line; fix venue is ours |
| `CONFIRMED-OPEN-UPSTREAM` | real, but the fix belongs to a project we do not own |
| `FIXED` | already fixed; cite the commit or the current file:line |
| `INVALID` | the premise is wrong; say why with evidence |
| `DUPLICATE-OF-<id>` | fully covered by another open row |
| `UNVERIFIABLE` | state exactly what blocked verification |

- Do not run the tool under investigation just to read its version; read its
  manifest (running it can write).
- "Fixed" or "applied" claims inside a report are verified, never trusted.
  Check the commit exists on the default branch. If the report says the
  change is uncommitted, treat it as open.
- Check the report's repository for fixes that landed after filing.
- State your clustering premise as falsifiable; a validation may overturn the
  mechanism, not just the status.
- If the harness offers read-only subagents, run one per cluster in parallel
  with this mandate and the row file paths, and require verbatim evidence
  (file:line excerpts, exact commands with unedited output). Without
  subagents, validate sequentially with the same evidence standard.

Record verdicts in the ledger.

**Phase 6 outcomes.** `done` prints one outcome JSON per call: `updated`,
`unchanged` and `not_found` ids. `unchanged` means the row already had that
mark; a row another session marked after your pull comes back `updated` and
its mark is replaced, which is why `list --open` comes first.

## What to do with it

**Phase 4: one consolidated interview.** Present, in this order:

1. A summary table: cluster, verdict, report count, machines, project,
   severity, proposed action.
2. The no-action set (`FIXED`, `INVALID`, `DUPLICATE-OF`, upstream) with
   per-id verdicts, as one item: "mark these N processed now?"
3. One item per `CONFIRMED-OPEN` cluster with the options below and your
   recommendation first, tagged "(Recommended)". Include the trade-off in one
   sentence when there is one.
4. Every `UNVERIFIABLE` item with a proposed disposition.

Use the harness's structured multi-select question tool when it has one
(Claude Code: AskUserQuestion with `multiSelect: true`); otherwise print a
numbered list and ask the user to reply with numbers. Options per cluster:

- **Fix now**: this session applies the fix in the local checkout.
- **Create a TODO**: write an action item (an issue, a ticket, or a TODO file
  the user names) and mark the rows `deferred` with where it lives.
- **Autonomous**: this session may clone or reach the repository and fix it
  end to end; offer only when the user has said this is acceptable.
- **Won't fix**: mark processed with a reason.
- **Leave open**: no action, the rows stay in the queue.

Even small mechanical fixes go through this interview; group them as one
no-trade-off item so they cost a single answer. If no local checkout matches
a cluster's repository, do not offer "Fix now"; ask TODO versus autonomous
instead. A TODO goes to a tracker through `gh`, a Linear or Jira MCP server
only when one is available; without one, write it to a file the user names.

**Phase 5: act.** Only what the user selected.

- Apply fixes with the smallest diff that resolves the mechanism. Verify
  (build, test, or the one command that proves it).
- Name every friction id in the commit body: `friction 43`,
  `frictions 44/45`. That trailer is what phase 1 of the next run reads.
- If two fixes touch the same repository, run them sequentially or with
  disjoint file sets.
- Follow the repository's own commit and merge rules; do not push, merge or
  open pull requests unless the user selected that.

**Phase 6: close out.** Marking is the last action, after the final commit.
Map each verdict to a `done` call:

| Ledger verdict | `done` flags |
|---|---|
| `FIXED` | `--verdict fixed --ref <commit>` |
| `INVALID` | `--verdict invalid` |
| `DUPLICATE-OF-<id>` | `--verdict duplicate --ref <uid>`, the uid of the kept row |
| `CONFIRMED-OPEN-UPSTREAM` | `--verdict upstream` |
| Won't fix | `--verdict wont_fix` |
| Create a TODO | `--verdict deferred --ref <where>` |
| `UNVERIFIABLE` | `--verdict unverifiable`, only when the user chose to close it |

Every call carries `--resolution` saying what was done. Several ids with the
same verdict and resolution go in one call. Relay each outcome JSON
verbatim.

Approved fixes that did not land in this session leave their rows open on
purpose. List those ids in the report so the next run marks them instead of
re-validating.

Final report: processed and total, per-cluster outcome, commits made, TODOs
created, new frictions filed, ids left open and why.

## What the hosted version adds

Proposed patches and pull requests.
