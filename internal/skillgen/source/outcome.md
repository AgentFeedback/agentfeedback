## Reading the outcome

A stored report, a duplicate of one and a queued retry are all success: do
not file it again. Relay the outcome to the user in one line.

<!-- only: cli -->
The last line on stdout is one JSON outcome:

| Outcome | Meaning | Exit |
|---|---|---|
| `submitted` | stored as a new report; `id` names it | 0 |
| `duplicate` | the server already had the same report; `id` names it | 0 |
| `spooled` | the server was unreachable; saved locally and sent on the next command or `agentfeedback flush` | 0 |
| `valid` | `--dry-run` passed; nothing was sent | 0 |
| `disabled` | configuration excludes this directory from reporting; nothing was sent, and that is fine | 0 |
| `rejected` | the server refused the body; the message names the member; fix it before sending again | 1 |
| `mismatch` | the same `key` was already used for different content; send the correction under a new key | 1 |
| `error` | configuration or transport failure; nothing was sent | 1 |
<!-- end -->
<!-- only: http -->
- `201 Created`: stored as a new report; `submission.id` names it.
- `200 OK`: the server already had the same report and returns it; success.
- `400`: the body is not a JSON object; `401`: the key is missing or wrong;
  `409`: a `key` you sent was used before for different content; `413`: the
  body is over 10 MiB. The `message` names what to fix.
- `429`: too many requests; wait the seconds in `Retry-After`, then send it
  once more.
- `5xx` or no answer: the service is down; try once more later, then tell the
  user.
<!-- end -->
<!-- only: mcp -->
The tool returns the stored report (`submission.id` names it) and a list of
warnings. A replay of an existing report returns that report. A tool error
carries a `message` that names what to fix.
<!-- end -->
