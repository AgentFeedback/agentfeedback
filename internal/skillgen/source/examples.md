## Examples

A friction is not only for coding agents; any agent whose work was slowed
down files the same way.

<!-- only: cli -->
Coding agent, in a repository, after a test run failed on a busy port:

```bash
agentfeedback submit friction --category tooling \
  --summary "make test needs port 5432 free and says nothing when it is taken" \
  --suggested-fix "Check the port first and name it in the error." --model "<your model id>"
```

Operations agent, outside any repository, after a weekly report job read the
wrong export folder:

```bash
agentfeedback submit friction --stdin <<'JSON'
{
  "summary": "Weekly report job reads last week's export folder",
  "category": "config",
  "project": "weekly-report",
  "context": {"workflow": "weekly-report", "task_id": "REP-12"},
  "model": "<your model id>"
}
JSON
```
<!-- end -->
<!-- only: http -->
Coding agent without the binary, after a test run failed on a busy port:

```json
{"kind": "friction", "summary": "make test needs port 5432 free and says nothing when it is taken", "category": "tooling", "model": "<your model id>"}
```

Research agent, after a data portal's search ignored its date filter:

```json
{"kind": "friction", "summary": "Portal search ignores the date filter on the first page", "category": "tooling", "project": "market-study", "context": {"url": "data.example.org/search", "workflow": "literature-review"}, "model": "<your model id>"}
```
<!-- end -->
<!-- only: mcp -->
Coding agent, after a test run failed on a busy port:
`submit_feedback` with `kind` `friction`, `summary` "make test needs port 5432
free and says nothing when it is taken", `category` `tooling`.

Support agent, after the help-desk search returned archived articles only:
`submit_feedback` with `kind` `friction`, `summary` "Help-desk search returns
only archived articles", `category` `tooling`, `project` `support-desk`, and
`context` `{"app": "helpdesk", "channel": "email"}`.
<!-- end -->
