# Weekly digest

## Purpose

Answers: what came in this week, what was closed, and what recurs.

## Commands

```bash
agentfeedback stats --since 7d --by project,category --bucket day
agentfeedback list --since 7d --processed --all
agentfeedback list --processed --all --tsv
```

## Reading the output

- `stats` prints the totals (`total`, `open`, `processed`, `redacted`), one
  group line per project and category with total, open and processed counts,
  and a daily series of total and open rows.
- A group whose `open` count stays close to its `total` is not being
  processed. A group with a high `total` recurs.
- `list --since 7d --processed --all` prints the rows filed this week and
  already processed, with the verdict in place of `open`. `--since` applies
  to `created_at`, so it does not show what was closed this week.
- `list --processed --all --tsv` prints every processed row as
  tab-separated values with a header. Rows closed in the last seven days are
  those whose `processed_at` column is within seven days of now; the
  `verdict` column says how each was closed. Use `agentfeedback get <id>`
  for a row's `resolution` and `ref`.

## What to do with it

- Report three things to the user: the week's intake per project and
  category, what was closed and with which verdicts, and the groups that
  recur.
- For a recurring group, run
  [`recurring-by-project.md`](recurring-by-project.md) on its project.
- For a growing open count, offer a [`fix-it-session.md`](fix-it-session.md).
- To post the digest to a team channel, use a Slack CLI or MCP server only if
  one is configured; without one, give the user the text to post.

## What the hosted version adds

Trend detection across weeks and teams.
