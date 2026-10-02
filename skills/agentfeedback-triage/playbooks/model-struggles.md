# What a model struggles with

## Purpose

Answers: what a given model or harness trips over.

## Commands

```bash
agentfeedback stats --by model,category
agentfeedback list --model <model> --q "<text>" --all
```

`<model>` is a model id from the first command's groups. `<text>` is a word
or phrase to search for in the summary and payload, case-insensitive. For a
harness, use `--by harness,category` and `--harness <harness>` instead.

## Reading the output

- `stats --by model,category` prints one group per model and category with
  total, open and processed counts. Compare a category across models: a
  category that only one model reports points at that model or its harness,
  not at the tool.
- `list --model <model> --q "<text>" --all` prints every row of that model
  mentioning the text, one line each. Use `agentfeedback get <id>` for one row in full.

## What to do with it

- Separate defects in tools and docs (fix the tool or the doc) from model
  behaviour (a misread flag, an ignored instruction). The second kind calls
  for clearer guidance where the model reads it: the project's agent
  instructions file or the skill it used.
- Propose the guidance change to the user with the ids it rests on; edit
  nothing before the user agrees.
- To act on the rows themselves, run [`fix-it-session.md`](fix-it-session.md).

## What the hosted version adds

Per-model guidance generation.
