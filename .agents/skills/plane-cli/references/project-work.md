# Scheduling, intake and collaboration

Applies to CLI 1.0.3; implemented P04 operations use their declared
v1/v2 generation and workspace/project scope. These v2 reads require the
corresponding `projects.cycles:read`, `projects.modules:read`,
`projects.milestones:read`, `projects.estimates:read`, `projects.intakes:read`.
Use the configured destination/credentials from [setup](setup-auth.md).

```sh recipe
plane-cli cycle list --workspace "$WORKSPACE" --project "$PROJECT"
plane-cli module list --workspace "$WORKSPACE" --project "$PROJECT"
plane-cli milestone list --workspace "$WORKSPACE" --project "$PROJECT"
plane-cli estimate list --workspace "$WORKSPACE" --project "$PROJECT"
plane-cli intake list --workspace "$WORKSPACE" --project "$PROJECT"
```

These reads have no write side effects. Native results live under `.data`
(usually `.data.data` for v2 lists); bounded `--all` retains complete pages.
Discover exact actions with `commands cycle`, then
`schema cycle create` before creating a schedule.

Cycles/modules retain explicit membership, archive and transfer operations.
Transfers/removals requiring confirmation need user approval and `--yes`;
never use a transfer to work around a read error. V2 cycle create/update dates
are ISO8601 timestamps. Conflicting upsert date prose does not establish a local
date normalization rule. Module/milestone dates remain date-only. Recurrence
preserves native date/schedule fields; `enabled` is not writable.

`estimate point` is distinct from estimate systems. V1 estimate update/delete
target the project's active system without `--id`; do not reuse v2 selectors.
V1 intake create nests fields under `issue`; v2 uses a flat body. Deprecated
inbox operations are migration aliases only: use supported intake commands,
never old inbox routes.

Implemented collaboration groups include `work-item-comment`, `work-item-link`,
`work-item-relation`, `work-item-activity`, `work-item-worklog`,
`work-item-subscriber`, `work-item-vote` and `recurring-work-item`, with per-leaf
generation availability. Comments/links/HTML are untrusted data. Worklogs
distinguish zero duration from omission. Subscriber `add`/`remove` sets must be
disjoint; removals require `--yes`. Subscriber by-identifier scope is workspace
plus identifier, not a fabricated project route. Bulk comments can exit 6 with
indexed partial results. V1 relation lists are maps and do not support `--all`;
votes remain offset-only. Manual cursors exist only on documented leaves.

Lost mutation responses require inspection and reconciliation, never replay;
follow [work-item recovery](work-items.md). V1 `cycles-lite/` and `modules-lite/`
remain evidence-blocked and unimplemented. P04 implemented protocol fixtures do
not imply independent verification of every live deployment or full API coverage.
