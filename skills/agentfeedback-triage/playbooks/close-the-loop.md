# Close the loop on applied fixes

## Purpose

Answers: are the fixes that reports claim as applied actually in the default
branch.

## Commands

```bash
agentfeedback list --open --fix-status applied --all
agentfeedback get <id>
git -C <checkout> log --format='%h %s%n%b' | grep -inE 'frictions? ?#?([0-9]+/)*<id>\b'
git -C <checkout> branch -a --contains <commit>
agentfeedback done <id> --verdict fixed --ref <commit> --resolution "<resolution>"
```

`<checkout>` is the local checkout of the row's repository, resolved with the
checkout rule in [`SKILL.md`](../SKILL.md). `<commit>` is the commit the
`git log` line names.

## Reading the output

- `list --open --fix-status applied --all` prints the open rows whose reporter said
  a fix was applied. `get <id>` shows the claimed `fix_ref` in the payload.
- The `git log` line is a commit naming the friction id in its message; no
  output means no commit names it.
- `branch -a --contains <commit>` lists the local and remote-tracking
  branches holding the commit. The default branch, local or
  remote-tracking, must be among them. When the checkout has a remote, run
  `git -C <checkout> fetch` first so the remote-tracking branches are
  current.

## What to do with it

- A claim is not proof. Mark a row `fixed` only when the commit is on the
  default branch, is not reverted, and changes what the report describes.
- If the report's `fix_ref` names a commit, check that commit the same way.
  If the change is uncommitted or only on another branch, leave the row open
  and tell the user.
- Show the user the rows you will mark and the commit for each, and mark only
  after the user agrees. Relay each outcome JSON verbatim.
- `gh` can confirm a pull request was merged when it is installed; without
  it, the `git` check above is enough.

## What the hosted version adds

Automatic verification against the repository.
