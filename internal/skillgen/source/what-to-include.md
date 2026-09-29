## What to include

- `summary`: one line, what slowed you down. Always send it; every other
  member is optional.
- `category`: one word such as `documentation`, `tooling`, `config`,
  `environment` or `process`.
- `details`: what you expected, what happened, and what it cost you.
- `suggested_fix`: the concrete fix, even if you already applied it.
- `fix_status` and `fix_ref`: `applied` with the commit (`repo@sha`), pull
  request or URL when the fix is in; `proposed` when it is written down but
  not applied; omit both otherwise.
- `model`: your model id. The harness usually cannot tell, and a report
  without a model is hard to act on.
- `project`: the repository, application, workflow or team space the friction
  belongs to, when it cannot be inferred.
- `context`: string pairs about where it happened when there is no repository
  to describe it, such as `app`, `workspace`, `url`, `channel`, `task_id` or
  `workflow`.

Never include credentials, tokens, private keys or personal data. Quote
commands, paths and error text; do not paste private payloads.
