# Workflows, automations and legacy limits

Applies to CLI 1.0.3. Workflow/automation operations here are **v2** with
explicit project/workspace scope. Reads below require respectively
`projects.workflows:read` and `automations:read`, plus workspace/project access.
No mode switch, graph repair, feature enablement or alternate route runs implicitly.

```sh recipe
plane-cli workflow project list --workspace "$WORKSPACE" --project "$PROJECT"
plane-cli automation project list --workspace "$WORKSPACE" --project "$PROJECT"
plane-cli schema workflow workspace update
plane-cli schema automation project node create
```

List reads have no write effects; native pages stay under `.data`, usually
`.data.data`. Use exact nested leaf schemas for filters, graph bodies and parent
IDs; some activity filters preserve native double hyphens such as
`--created-at--gt`. Every actual workflow/automation mutation requires `--yes`,
including creates/updates/status/rotation. Dry-run validates without auth/network.
The workflow project fallback preview is read-only POST; a zero result is not
proof a supplied workflow exists. Never mutate automatically after a preview.

Workspace workflow update with `states` destructively replaces the chain,
rewrites defaults and can leave transitions referencing dropped states. Read
current detail first; metadata-only and chain replacement are not equivalent.
Catalog state removal has distinct stranded-item/dependency/default checks and
does not delete the state catalog object. A success acknowledges the request,
not background work-item relocation completion; immediate reads may show old
states. No relocation job/status endpoint is documented. Do not repeat mutations
to wait for relocation. Hooks can run remote scripts/webhooks; pre-hook failure
blocks a move, post-hook failure does not roll it back.

One-time webhook hook/node secrets intentionally appear in create/rotate JSON;
use declared `--secret-output-file` protected delivery. Ordinary reads redact
signing secrets. Hook creation `send_webhook` rejects sparse `--fields` to prevent
secret loss; `run_script` rejects secret-delivery flags. Unknown outcomes or
exit 6 require inspection and explicit authorized recovery, not replay/rotation.
Automation node `config` is a declared string, not a guessed JSON object schema.
No handler catalog, graph repair or automatic association emulation is provided.
New automation metadata does not prove execution. Activity reads are not a run
submission/status API.

## Unavailable supplemental v1 operations

Public migration fragments do not supply complete callable contracts for:
AI agents/runs/activities; integration credentials/connections/import jobs;
workflow approvals/state transfers. All remain evidence-blocked/unimplemented.
Do not derive a method, payload, full route or polling command from fragments,
UI routes, scope names or run IDs. Existing documented legacy families in the
other references remain usable; this limitation does not disable them.
Protocol fixtures do not establish live permissions, entitlements or background
completion. For recovery see [work items](work-items.md).
