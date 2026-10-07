## How to file

<!-- only: cli -->
Run the `agentfeedback` binary. It collects the context (repository facts,
machine, harness) for you, retries when the server is unreachable, and never
prints the API key.

```bash
agentfeedback submit friction --category documentation \
  --summary "README install step 3 references a flag that no longer exists" \
  --model "<your model id>"
```

For prose with quotes or newlines, send a JSON object on stdin; flags given
beside it win:

```bash
agentfeedback submit friction --stdin <<'JSON'
{
  "summary": "README install step 3 references a flag that no longer exists",
  "category": "documentation",
  "details": "README says --legacy; the CLI rejects it since 1.4 and finding that took two failed runs.",
  "suggested_fix": "Replace --legacy with --compat in README step 3.",
  "fix_status": "applied",
  "fix_ref": "example@a1b2c3d",
  "model": "<your model id>"
}
JSON
```

The members above have flags of the same name with dashes (`--suggested-fix`,
`--fix-status`, `--fix-ref`, `--project`); `context` goes in the stdin JSON.
`--dry-run` prints the body that would be sent and outcome `valid`, and sends
nothing.
`--scrub` replaces known secret formats (keys, tokens, private keys, URL
credentials) in every string value of the body with `[REDACTED:<class>]`
before checking or sending; use it when the text quotes logs or config. The
outcome line then carries one `scrubbed` warning per member it changed.
<!-- end -->
<!-- only: http -->
Send one HTTP request. The server at {{server}} fills in what you leave out.

- Route: `POST {{server}}/api/v1/submissions`
- Headers: `Authorization: Bearer <API key>` (or `X-Api-Key: <API key>`), and
  `Content-Type: application/json`
- Minimum body: `{"kind": "friction", "summary": "<one line>"}`; add the
  members listed above. Send `kind`: without it the report is stored as
  `unknown`.

Any JSON object is accepted: members the envelope does not know are kept in
the report's payload, never rejected.
<!-- end -->
<!-- only: curl -->

The same request with curl, for example:

```bash
curl -sS -X POST "{{server}}/api/v1/submissions" \
  -H "Authorization: Bearer <API key>" \
  -H "Content-Type: application/json" \
  -d '{"kind": "friction", "summary": "README install step 3 references a flag that no longer exists", "category": "documentation", "model": "<your model id>"}'
```
<!-- end -->
<!-- only: powershell -->

The same request in PowerShell (Windows PowerShell 5.1 or PowerShell 7), for
example. The body is sent as UTF-8 bytes, so text beyond ASCII survives on
5.1:

```powershell
$body = @{ kind = 'friction'; summary = 'README install step 3 references a flag that no longer exists'; category = 'documentation'; model = '<your model id>' } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri '{{server}}/api/v1/submissions' `
  -Headers @{ Authorization = 'Bearer <API key>' } `
  -ContentType 'application/json' -Body ([Text.Encoding]::UTF8.GetBytes($body))
```
<!-- end -->
<!-- only: mcp -->
Call the `submit_feedback` tool with `kind` set to `friction`, a `summary`,
and the members listed above as arguments. The server fills in what you
leave out.
<!-- end -->
