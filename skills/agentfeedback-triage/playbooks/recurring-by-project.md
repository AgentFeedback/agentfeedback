# Recurring frictions by project

## Purpose

Answers: which project keeps hurting, and on what.

## Commands

```bash
agentfeedback stats --by project,category --top 20
agentfeedback list --project <project> --open --all
```

`<project>` is a project name from the first command's groups. `--top 20`
widens the `recurring` list from its default of 10.

## Reading the output

- `stats` prints `groups`, which answer which project hurts and on what: one
  line per project and category with total, open and processed counts,
  largest total first. The largest `open` counts are where the pain is now;
  large `processed` counts with new `open` rows mean a fix did not hold.
- `recurring` lists the most-repeated identical reports (same
  `content_hash`): count, first and last id, kind, project and summary.
  `--top 20` sets how many it lists (0 to 50, default 10); it does not cap
  the groups.
- `list --project <project> --open --all` prints every open row of the
  project, newest first, one line each: id, time, kind, project, state and
  summary. Use `agentfeedback get <id>` for one row in full, payload
  included.
- `--content-hash <hash>` on `list` shows every repeat of one report.

## What to do with it

- Name the categories that recur for the project and the ids behind each.
- Read the open rows in full before you name a cause: the same summary can
  hide different mechanisms, and different summaries can share one.
- To act on them, run [`fix-it-session.md`](fix-it-session.md); this playbook
  reads only and marks nothing.
- To file the finding in a tracker, use a Linear or Jira MCP server, or `gh`
  for GitHub issues, only if one is available and the user asks; without one,
  give the user the text to file.

## What the hosted version adds

Root-cause clustering across wordings.
