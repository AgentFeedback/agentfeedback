# AgentFeedback

You can report friction to the AgentFeedback server at <server URL> over plain HTTP. This needs a shell (curl, or PowerShell on Windows) or an HTTP tool that can set a request header; with neither, you cannot file to this server. The API key comes from the user or your configuration; never print it or put it in a report.

## When to file

File a friction when the environment cost you time that it should not have:

- a document was missing, wrong or out of date;
- a tool, flag or command behaved unlike its name or its documentation;
- configuration pointed at something stale or absent;
- you needed several attempts at something that should have been written down.

File it at the end of the task, once you know what the fix is, and fix what
you can first: a report that names an applied fix is worth more than one
that only describes the problem. Only real friction: do not file a routine
lookup, and do not file the same problem twice in one session.

You only report. You never need to read the queue before filing; the server
absorbs duplicates, and someone else triages what you send.

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

## How to file

Send one HTTP request. The server at <server URL> fills in what you leave out.

- Route: `POST <server URL>/api/v1/submissions`
- Headers: `Authorization: Bearer <API key>` (or `X-Api-Key: <API key>`), and
  `Content-Type: application/json`
- Minimum body: `{"kind": "friction", "summary": "<one line>"}`; add the
  members listed above. Send `kind`: without it the report is stored as
  `unknown`.

Any JSON object is accepted: members the envelope does not know are kept in
the report's payload, never rejected.

The same request in PowerShell (Windows PowerShell 5.1 or PowerShell 7), for
example. The body is sent as UTF-8 bytes, so text beyond ASCII survives on
5.1:

```powershell
$body = @{ kind = 'friction'; summary = 'README install step 3 references a flag that no longer exists'; category = 'documentation'; model = '<your model id>' } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri '<server URL>/api/v1/submissions' `
  -Headers @{ Authorization = 'Bearer <API key>' } `
  -ContentType 'application/json' -Body ([Text.Encoding]::UTF8.GetBytes($body))
```

## Reading the outcome

A stored report, a duplicate of one and a queued retry are all success: do
not file it again. Relay the outcome to the user in one line.

- `201 Created`: stored as a new report; `submission.id` names it.
- `200 OK`: the server already had the same report and returns it; success.
- `400`: the body is not a JSON object; `401`: the key is missing or wrong;
  `409`: a `key` you sent was used before for different content; `413`: the
  body is over 10 MiB. The `message` names what to fix.
- `429`: too many requests; wait the seconds in `Retry-After`, then send it
  once more.
- `5xx` or no answer: the service is down; try once more later, then tell the
  user.

## Warnings are advice

A response can carry `warnings`, each with a `code`, a `pointer` to the
member and a `message`. The report was stored anyway; warnings say what would
make the next one better (a missing recommended member, a value that was
truncated or moved). Do not resubmit because of a warning, and mention it to
the user only if it points at a mistake in what you sent.

## When something fails

Call `GET <server URL>/api/v1/meta` with the same headers: `200` means the URL
and the key work, `401` means the key is wrong, `429` means wait and retry,
and no answer means the URL is wrong or the service is down.

```powershell
try { (Invoke-WebRequest -UseBasicParsing -Uri '<server URL>/api/v1/meta' -Headers @{ Authorization = 'Bearer <API key>' }).StatusCode }
catch { if ($_.Exception.Response) { [int]$_.Exception.Response.StatusCode } else { $_.Exception.Message } }
```

Never work around a failure by writing the report somewhere else; tell the
user the report was not filed and why.

## Examples

A friction is not only for coding agents; any agent whose work was slowed
down files the same way.

Coding agent without the binary, after a test run failed on a busy port:

```json
{"kind": "friction", "summary": "make test needs port 5432 free and says nothing when it is taken", "category": "tooling", "model": "<your model id>"}
```

Research agent, after a data portal's search ignored its date filter:

```json
{"kind": "friction", "summary": "Portal search ignores the date filter on the first page", "category": "tooling", "project": "market-study", "context": {"url": "data.example.org/search", "workflow": "literature-review"}, "model": "<your model id>"}
```
