# Types, properties and governance

Applies to CLI 1.0.3, supported v1/v2 leaves. Workspace/project scope is
explicit, never auto-selected. Feature reads use `workspaces.features:read`;
type schema reads use `projects.work_item_types:read`. Write permissions and
workspace governance must already permit the operation.

```sh recipe
plane-cli workspace-feature get --workspace "$WORKSPACE"
plane-cli work-item-type get-schema --workspace "$WORKSPACE" --project "$PROJECT" --id "$TYPE"
```

Both reads return the native response at `.data` without changing governance.
For v2 type schema, custom fields are under `.data.custom_fields`. A field's
returned property `name`, **not display_name**, is the `custom_fields` key.
Read required/cardinality/option constraints from the actual type schema;
the CLI does not fetch it implicitly or invent handler-specific validation.

Project definition groups are `work-item-type`, `work-item-property`,
`work-item-property-option`, `work-item-type-property`. Workspace equivalents
start with `workspace-`; `work-item-property-context` is workspace-scoped.
Definition creation, attachment, context overrides and values are distinct
resources. Discover each exact path, e.g.
`plane-cli schema work-item-property create` and
`plane-cli schema work-item-type-property attach`.

After approved definition creation/attachment and schema inspection, v2 values
use `work-item update --body-file PATH` with a native body such as
`{"custom_fields":{"RETURNED_PROPERTY_NAME":"See runbook"}}`.
Use real returned property names and the requested item; do not send this example
literally. V1 values instead use documented `work-item-property-value` commands.
Omitted PATCH properties keep their values; null/zero/false/empty have different
semantics and must be permitted by the leaf schema.

Context updates require `--yes`: option replacement can delete recorded values.
Do not infer or create a default context from conflicting upstream overviews.
Workspace `is_epic` is read-only; a type named Epic does not create a system epic.
Mutations do not probe modes, switch governance, retry a different scope or enable
features. A 409 remains a sanitized API error with HTTP status, not a request to
change workspace policy. Recover unknown outcomes by reading definitions,
attachments and target item before any explicitly authorized retry.
