---
name: deliver-ticket
# Folded block scalar, NOT a plain scalar: the text contains ": " and "#".
description: >-
  Deliver one AgentFeedback OSS ticket, or a batch of them, end-to-end in one
  run: read the ticket (kaneo), move it to In Progress, plan and
  interview (once per run), implement, run the project's fast quality gates,
  run the multi-model review and address findings, land (PR or direct),
  deploy once at the end when asked, verify the deployment, and mark the
  ticket Done. EXPLICIT
  INVOCATION ONLY. Run it when the user invokes the deliver-ticket skill by
  name, in whatever syntax the harness uses for skills (a slash command, a
  skill mention, or plain words naming it). Do NOT trigger it from
  ticket-shaped phrasing alone ("pick up ABC-12", "work on the login bug"),
  which is ambiguous between reading, editing and shipping; this skill merges,
  deploys and closes with at most one standing human checkpoint. To view or
  update a ticket use the tracker's own CLI.
license: MIT
metadata:
  author: "foae"
  template: adopt-deliver-ticket
  template_version: "1.7.0"
  adopted: "2026-09-27"
---

# Deliver a ticket (end-to-end)

One invocation runs the whole delivery workflow. Project facts (tracker
commands, workspace layout, quality gates, PR conventions, deploy and watch
recipes, autonomy overrides) live in the sidecar
`.agents/skills/deliver-ticket/deliver-ticket.yaml`, relative to the **main checkout**. **Read the sidecar
first, in full, every run**, then every file it lists under
`project.agent_instructions`; those files outrank this one wherever they
disagree. This file is the workflow; the sidecar is the configuration. Where
this file says "per sidecar", the sidecar key is named in parentheses.
The skill and sidecar are tracked in Git and available in every worktree.
Edit configuration in the delivery worktree so changes ship with the task.

The skill is harness-agnostic. "Ask the user" means your harness's question
mechanism with a recommended option listed first. "In the background" means a
background process, or a subagent when your harness has one. "Subtasks" means
your harness's task list.

**Capability states gate whole phases.** `tracker.state`, `gates.state`,
`pr.state` and `deploy.state` are each `adopted`, `substituted`, `skipped` or
`impossible`. Only `adopted` and `substituted` run the phases that use that
capability; `skipped` and `impossible` turn them into no-ops that the wrap-up
names. Tracker skipped: every ticket step is a no-op (as ticket-less mode).
Gates skipped: Phase 6 is skipped and the delivery summary (and the PR body,
if one exists) says so. PR skipped (the default, no PR flow): Phases 7 to 10
collapse into "review the local range against the base, then land on the base
directly once Phase 10's gates pass". Deploy skipped or
impossible: Phases 11 and 12 are skipped.

## Inputs

- **Ticket reference**: the argument after the skill name. Either a tracker
  ID (`ABC-123`, `#42`, `task-17`), **a list of them** (`ABC-123, 124, 125`
  or `ABC-123 ABC-124`: comma- or space-separated, where a bare number
  takes the prefix of the nearest full ID before it), or, when the sidecar
  allows ticket-less mode (`tracker.ticketless`), a quoted task description.
  Two or more IDs make this a **batch run** (see "Batch runs" below). If
  absent, ask before doing anything.
- The reference ends at the first token that is neither an ID, a comma nor
  a bare number directly following an ID; everything from there on is
  constraint text to fold into the plan (`ABC-12 keep it to 2 files` is one
  ticket plus a constraint, never a batch). In a batch run the constraint
  applies to every ticket unless it names one.
- Per-run overrides in plain words are honoured: "pause before merge",
  "pause before deploy", "do not deploy", "and deploy" (or "then deploy",
  "deploy at the end"), "skip the panel", "panel pro", "interview me". A
  panel override ("panel pro", "panel fast", …) sets the panel outright:
  Phase 3 neither asks about it nor replaces it with the default.
  "Interview me" forces the Phase 3 interview when `policy.interview` is
  `conditional`. The deploy request is the one a `deploy.on_request`
  project needs: it records `approved` for this run (when deploy is live),
  is never implied, and counts only as its own instruction, never as the
  word "deploy" inside a sentence about the change ("fix the deploy
  script" requests nothing).
- **The deploy decision** is recorded in the subtask list, first at Phase 0
  step 3 from any invocation override, so Phase 2 already sees a refusal.
  Values: `approved` (a yes, or `deploy.auto` true), `declined`, `pending`
  (asked or paused, not yet answered), `deferred` (`deploy.on_request` is
  true and nothing requested a deploy: the change lands, the deploy waits
  for a later request, and the ticket still reaches done; `on_request` is
  incompatible with `merge_deploys`, where the merge is the deploy, and the
  checker rejects the pair) and `n/a` (deploy not live). A
  refusal ("do not deploy", "skip deployment", or a no to the deploy
  question) records `declined`. It persists for the whole run, overrides
  `deploy.auto` and "pause before deploy", is never asked again, and forbids
  even read-only production probes; only the user can reverse it. "Do not
  deploy yet" or "pause before deploy" records `pending` with a pause
  checkpoint. Silence is never approval, and a refusal is never a pause.

## Hard rules

- **One standing human checkpoint: the plan interview (Phase 3),
  unconditional unless `policy.interview` is `conditional`**, in which case
  it runs only when Phase 3 step 0 finds something to decide. It happens
  once per run; a batch gets one interview for all its tickets. Other stops
  are conditional and listed in the gate policy below. Never add a stop that
  the policy does not name, and never skip one it does.
- **Parallelise everything independent.** The parallel map below says what
  may overlap. The single constraint: **nothing mutates the working tree while
  a pinned review is reading it** — from the Phase 8 launch until every
  reviewer and bot has reported. Phase 9 then edits freely; that is its job.
- **Track a subtask per phase** and update it as you go. Record interview
  answers verbatim in the subtask list; Phase 8 is many tool calls later and
  an unrecorded panel choice gets silently replaced by a default.
- **Repo beats ticket.** When the ticket's verification text conflicts with
  the repo's CI or agent instructions, the repo is authoritative and the
  conflict goes in the delivery summary.
- **Pre-existing issues are yours.** A bug or smell in code you depend on is
  fixed inline when small and safe (a quick win now spares the next agent
  the same trap), or raised at the next checkpoint with file and line, never
  noted and stepped around.
- **The human is the exception, not the loop.** The ticket is expected to
  carry every detail that matters; the plan interview is the one place to
  settle what it does not. Afterwards, ask only when genuinely stuck, and
  only after the self-unblocking options are exhausted: a mock or stub for
  a missing collaborator, narrowing to the part that does not depend on the
  open point, or a stated assumption recorded in the plan and the summary.
  A question that a file, a test run or the ticket's comments can answer is
  never asked. The stops the gate policy names (Phase 9's low-confidence
  stop among them) remain; this rule governs everything outside them.
- **Keep end-to-end tests simple.** Prove behaviour with unit and
  integration tests that mock the collaborators the project does not own.
  Write a true end-to-end test only when the repo already has an easy,
  reproducible medium for one (a fixture stack, a recorded harness); a
  change proven with mocks is done. The heavy end-to-end suites belong in
  `deploy.gates`, which run once before a deploy, never per ticket.
- **Never read secret-bearing files at all.** The sidecar lists them
  (`project.secret_paths`). Not `cat`, and not `grep` either: `grep API_KEY
  .env` prints the whole line, value included, straight into your context and
  from there into a plan or a PR body. Check existence with `test -f`, and
  when you need to know which keys a file defines, read the repo's example
  file (`.env.example`) or its documentation instead.
- **Every sidecar key is load-bearing.** Beyond the keys named in the phases:
  `tracker.id_pattern` decides whether the argument is an ID or free text,
  `tracker.commands.auth_probe` runs once in Phase 0 before the first tracker
  call (a failed probe stops the run, except that with `tracker.ticketless`
  true and a task description in the invocation tracker steps degrade to
  no-ops and the wrap-up says so; Phase 0 step 1), `tracker.commands.resolve_id` — when set — turns that typed
  reference into the id every other tracker command needs,
  `gates.source` is the file to re-read when a gate looks stale,
  `deploy.target_class` goes in the Phase 3 deploy question and the wrap-up so
  the human knows what they authorised, and every `notes:` list is prose you
  must read before using that capability's commands. `project.languages`,
  `project.package_manager` and `project.monorepo` are descriptive: they tell
  you which conventions and toolchain apply when a phase leaves a choice
  open.
- **Capability checks, not assumptions.** Before Phases 5, 8 and 11 probe for
  the skill or tool you are about to use. Absent, apply the documented
  fallback and say so in the wrap-up. Never fail a phase because an optional
  skill is missing on this machine.
- **Run tools directly.** Ask the user only for genuinely interactive steps
  (a browser login, a hardware token).

## Forge commands

Every PR step below is written once, in `gh` form, with the `glab` equivalent
here. `pr.forge` picks the column. Rows marked **verify** were not exercised on
this template version; check `--help` before relying on them.

`<strategy>` is `pr.merge_strategy` (`squash`, `merge` or `rebase`), and the
branch-deletion flag appears only when `pr.delete_branch` is true. Neither is
hardcoded here; the table shows where they go.

| Step | `gh` (GitHub) | `glab` (GitLab) |
|---|---|---|
| Open | `gh pr create --base <base> --head <branch> --title <t> --body-file <f>` | `glab mr create --source-branch <branch> --target-branch <base> --title <t> --description-file <f>` **verify** |
| CI status on the head SHA | `gh pr checks <N>` | `glab ci status --branch <branch>` |
| Reviews and bot comments | `gh pr view <N> --json reviews`; `gh api repos/<o>/<r>/pulls/<N>/comments` | `glab mr view <N> --comments` |
| Protection and required approvals | `gh api repos/<o>/<r>/rules/branches/<base>` (never `/protection` alone) | `glab api projects/:id/protected_branches/<base>`; `glab api projects/:id/merge_requests/<N>/approvals` **verify** |
| Mergeability | `gh pr view <N> --json mergeable,mergeStateStatus,state` | `glab mr view <N>` (detailed merge status) **verify** |
| Merge | `gh pr merge <N> --<strategy> [--delete-branch] --match-head-commit <sha> --subject <s> --body <b>` | `glab mr merge <N> [--squash] [--remove-source-branch] --yes` |
| Verify merged | `gh pr view <N> --json state,mergeCommit` | `glab mr view <N>` shows `merged` |
| Revert PR | same as open, on a `revert/<ticket>` branch | same |

## Gate policy

| Gate | When it stops | Override |
|---|---|---|
| Ticket eligibility (Phase 0, Phase 3 for a blocked ticket) | When `tracker.states.ready` is set: ends the run, with a report and no state change, when the ticket is in none of `ready`, `in_progress`, `blocked` or `in_review`; is in `ready` with an unfinished blocker; the relation or comment reads `view` chains fail; or a `blocked` ticket's impediment is not confirmed cleared. With `ready` null none of this runs, and a failed `view` stops the run as in 1.4 | sidecar `tracker.states.ready` (null turns the check off) |
| Plan interview (Phase 3) | Always when `policy.interview` is `always`; when `conditional`, only when Phase 3 step 0 finds a trigger. In a batch run it happens **once**, for every ticket together, before the first ticket starts | sidecar `policy.interview`; "interview me" |
| Required gate (Phases 6 and 9) | When a required gate cannot run on this machine and no CI check on the PR runs it (always the case when `pr.state` is skipped or impossible). A gate that runs and fails is fixed per Phase 6, never a stop | none |
| Review-fix direction (Phase 9) | Only when a valid finding contradicts the agreed plan or your confidence is low | none |
| Merge (Phase 10) | Stops when `policy.merge` is `pause`, or the user said "pause before merge". `pr.required_approvals` > 0 is a **wait**, not a stop | sidecar `policy.merge` |
| Deploy (Phase 11, or before merge/push when `deploy.merge_deploys`) | `declined` skips deploy and watch without asking again (the ticket then does not reach done); with `merge_deploys` it ends the run before the merge/push instead. `deferred` (`deploy.on_request` with no request) skips them too, and the ticket does reach done. `approved` proceeds. `pending` (an unanswered question or a "pause before deploy" checkpoint) stops and asks once. A yes **is** the human authorisation this gate exists to obtain, for this run only; it never changes the sidecar. In a batch run the deploy happens once, after the last ticket lands | sidecar `deploy.auto`, `deploy.on_request`; a refusal overrides both |
| Pre-deploy gates (Phase 11, or Phase 10 step 4 with `merge_deploys`) | A failing command in `deploy.gates` blocks the deploy. One fix-forward round is allowed when the cause is in this run's landed changes and the fix is small; otherwise the run ends with the change landed and the deploy not done, and the report says why | none |
| Batch reconciliation (batch runs) | Only when the next ticket cannot proceed past a ticket this run blocked, and no self-unblocking option (a mock or stub, delivering the independent part, a recorded assumption) is attainable | none |
| Review without a panel (Phase 8) | Stops when `multi-llm-review` is absent and either `review.allow_harness_fallback` is false or the harness has no reviewer subagent; asks whether to proceed on the harness's single review, wait, or skip | sidecar `review.allow_harness_fallback` |
| Post-watch failure (Phase 12) | Never; the recovery procedure runs autonomously and reports. `policy.fix_forward` false skips the fix-forward round and rolls back at once | sidecar `policy.fix_forward` |

## Parallel map

| Phase | Runs in the background meanwhile |
|---|---|
| 0 Intake | Workspace creation and dependency install (Phase 1); the review roster precheck when `multi-llm-review` is installed (`~/.agents/skills/multi-llm-review/SKILL.md` exists): `python3 ~/.agents/skills/_lib/review roster --panel <review.default_panel> --caller-model <your model>` is offline and prints the seats that would run, the gateway state and any `REVIEW_SKIP_SLOT` skip, so a thin roster is known before Phase 3 settles the panel rather than 15 minutes after it. If Phase 3 or an invocation override picks a panel other than `review.default_panel`, re-run the precheck for that panel. The live health check itself happens at launch in Phase 8 |
| 7 Review-ready | Phase 8 review panel and, only when a PR and its checks exist, the CI / PR-bot wait run **concurrently against the same pinned SHA** |
| 11 Deploy | Delivery summary drafted while the rollout progresses |
| 12 Watch | Delivery summary finalised; worktree cleanup prepared but not executed |

Anything not in this table runs sequentially.

## Batch runs

Two or more ticket IDs in the invocation make a batch. The tickets are still
delivered **one at a time**, each through its own branch, gates, review and
landing, but the planning, the interview and the deploy happen once:

1. **Intake for all** (Phase 0 steps 1 and 2, every ticket): resolve, read
   and check the eligibility of each ticket before touching any. A ticket
   that is not eligible is reported and dropped from the batch; it is not a
   stop. A `blocks` relation whose source is an earlier ticket of the same
   batch does not make the target ineligible: order the batch as typed,
   except that such a ticket goes after its blocker, and re-check it when
   its turn comes (a blocker that did not land makes it the reconciliation
   case of item 3). The move to in progress (step 4) and Phase 1 happen per
   ticket, when its turn starts. A ticket that already landed (`in_review`
   resume) contributes no branch and waits for the batch deploy.
2. **Pre-work and draft plans for all** (Phase 2), then **one interview**
   (Phase 3; a batch always runs it, whatever `policy.interview` says)
   covering every ticket: the plans, tightly; the genuine decisions per
   ticket; the review panel per ticket (step 2's ladder: propose `fast` for
   a simple change, `default` for ordinary work, `pro` or above for
   sensitive or hard-to-reverse work, and say when one panel fits the whole
   batch; a panel override in the invocation settles it for all); and the
   deploy decision, asked **once** for the end of the batch (step 3,
   including when `deploy.on_request` is true, where a no records
   `deferred`, not `declined`: landing is done there) unless the invocation
   already recorded it. Record every answer per ticket in the subtask list;
   nothing later re-asks.
3. **Deliver each ticket in order**: Phase 1 (branch off the updated base,
   which now contains the previous ticket), Phase 4 (post its plan, re-read
   against the code as it stands now), Phases 5 to 10, then Phase 13 step 3
   (clean up the branch so the next ticket can start). Re-plan silently
   when the landed tickets changed the ground; ask only on a genuinely new
   decision, per the hard rule. A ticket that cannot be finished (a gate that
   cannot pass, an impediment, a spec gap) is moved to `blocked` with the
   Phase 0 step 4 comment and the batch continues with the next ticket:
   first commit its work on its own branch (a plain commit the comment
   names) and check out the base, so the next ticket starts from a clean
   tree and never carries the blocked work into its own branch.
   **Reconciliation stop**: when the next ticket cannot proceed past a ticket
   this run blocked, first try to self-unblock (mock or stub the missing
   piece, deliver the independent part, record an assumption); only when
   none of that is attainable, stop and ask how to reconcile, naming both
   tickets and what was tried. Never pass a blocked ticket's work off as
   done.
4. **Deploy once** (Phases 11 and 12) after the last ticket lands, from the
   merged base, with `deploy.gates` first. A `declined` or `deferred`
   decision skips both. **`merge_deploys` is the exception**: every landing
   is a deploy, so the interview's single deploy decision applies to each
   landing, Phase 10 step 4 and Phase 12 run after **each** ticket, and the
   first Phase 10 step 4 capture of the batch is the rollback target for the
   whole batch (later captures are notes). A `declined` there ends the run
   at the first Phase 10, as in a single run.
5. **Wrap up each ticket** (Phase 13 steps 1 and 2) once the deploy outcome
   is known, so every summary names it; when no deploy runs this batch, do
   it right after each ticket lands. For shape `pseudo-container`, run the
   cleanup of Phase 13 step 3 once, after the batch's watch, since the
   verification container exists only then. Then one report for the batch: a row
   per ticket (landed SHA, ticket state, panel and its real coverage, blocked
   or dropped with the reason), the deploy and verification result, and the
   follow-ups.

Subtasks: one per ticket per phase, so the harness's task list shows where
the batch stands and which answers belong to which ticket.

---

## Phase 0: Intake

1. Split the reference on commas and whitespace; expand a bare number with
   the prefix of the nearest full ID before it (`ABC-12, 13` is `ABC-12`
   and `ABC-13`). Resolve each piece against `tracker.id_pattern`: a match
   is a ticket ID, anything else is free text for ticket-less mode (IDs and
   free text do not mix; a batch is IDs only). Free text
   without a ticket ID is accepted only when `tracker.ticketless` is true;
   otherwise stop and ask for a ticket ID. When the tracker is live, run
   `tracker.commands.auth_probe` first. If it fails and
   the invocation carried **only an ID**, stop: the ticket was the whole
   specification and you cannot invent it. Report the probe failure and ask
   for the task description or for the credential to be fixed. If it fails
   but the invocation also carried a task description, continue with every
   tracker step as a no-op and say so in the wrap-up, **only when
   `tracker.ticketless` is true**. When it is false the tracker is
   mandatory: a failed probe always stops the run with a report, whatever
   else the invocation carried. For a tracker ID, and
   only when the probe succeeded: **if `tracker.commands.resolve_id` is set,
   run it before any other tracker command.** The reference the user typed is
   a display label and the API takes a different id. It must print exactly one
   id; zero or several means the label is wrong or ambiguous, which is a stop,
   not a guess. Every later `<ID>` in this file is the resolved id, and the
   label is what you write in prose for the human. In ticket-less mode there
   is nothing to resolve and this step does not run.
   Then read the ticket with `tracker.commands.view` **in
   full**: description, every comment, and every linked document or spec.
   There is no marker convention; planning notes, heads-ups and constraints
   appear anywhere in the body or comments and all of them bind. Extract them
   into the subtask list under "Ticket constraints".
   **Eligibility, when `tracker.states.ready` is set**, right after `view`
   and before any state change: deliver only a ticket in `ready` (a start)
   or in `in_progress`, `blocked` or `in_review` (a resume). Any other state
   ends the run with a report and no state change (in a batch: drops that
   ticket and continues). So does a `ready` ticket
   that is the **target** of a `blocks` relation whose source is not done;
   relations where this ticket is the source are ignored. How to read
   relations is tracker-specific (`tracker.notes`). A failed read of the
   relations or comments `view` chains is a stop too. With `ready` null this
   check does not run. **Repo-backed tracker**: when the ticket branch
   exists, read the ticket file on that branch, where in-flight state edits
   live, not on the base.
2. Ticket-less mode (`tracker.ticketless` true and the argument is text): the
   argument is the ticket; every status transition and ticket comment below
   becomes a no-op and the delivery summary goes to the local plan file only.
3. Create the subtasks, one per phase (per ticket in a batch), and record
   the deploy decision: `declined` or `pending` from an invocation override,
   `approved` from a "deploy" request, `n/a` when deploy is not live,
   `approved` when `deploy.auto` is true, `deferred` when `deploy.on_request`
   is true and this is a single-ticket run with no request; otherwise
   Phase 3 step 3 sets it.
4. **Resuming** (eligibility found an in-flight state). Every move to
   `tracker.states.blocked`, wherever it happens, posts a comment naming
   (a) the phase to resume at and (b) the state the ticket left. On resume,
   read the latest such comment and the plan comment, and say in the plan
   which phase you resume at:
   - `blocked`: Phase 3 confirms the impediment cleared (step 1); then move
     the ticket back to the state the comment names and resume at the phase
     it names, reusing the posted plan: no re-plan, no new plan comment.
   - `in_progress` without such a comment: read the plan comment and the
     branch, and resume at the first incomplete phase.
   - `in_review`: if the change already landed, resume at Phase 11, or at
     Phase 12 or 13 when the ticket comments show those started; if a PR is
     open, resume at Phase 8.
   A resumed `in_progress` or `in_review` ticket is not moved. Phase 1
   reuses the existing branch and worktree; Phase 7 reuses an open PR/MR
   for that branch when one exists. Otherwise, move the ticket to in progress: `tracker.commands.set_state` with
   `tracker.states.in_progress`, substituting `<PREVIOUS>` with the state the
   ticket is leaving (label-based trackers need it to remove the old label;
   read it from the ticket you just viewed). **Repo-backed trackers**
   (`tracker.repo_backed`, Backlog.md): the state change is a file edit, so
   make it **inside the workspace after Phase 1**, never in the main
   checkout; it rides the PR. The done edit in Phase 13 is a separate commit
   on the base branch.
5. Start Phase 1 and the review roster precheck in the background.

## Phase 1: Workspace

Per sidecar (`workspace`):

- **in-place** mode (the default: tickets run one at a time): `git fetch`,
  then create the branch from `workspace.base_ref` in the main checkout,
  named by `workspace.branch_pattern`. Branch off the remote ref, never
  local `main`; local `main` is often behind. Require a clean tree first; if
  it is dirty, stop and ask. Run `workspace.setup` only when it names
  something the checkout lacks (a lockfile that moved on fetch).
- **worktree** mode (only for projects that deliver in parallel): `git fetch
  <remote>` then create a worktree and branch off `workspace.base_ref` under
  `workspace.worktree_dir`, named by `workspace.branch_pattern`. All edits,
  builds and tests happen inside the worktree. A fresh worktree has **no
  dependencies installed**; run `workspace.setup` before anything else.
- **Resume** (Phase 0 step 4): reuse the ticket's existing branch, and its
  worktree in worktree mode, instead of creating new ones. In-place, when
  the checked-out branch is already the ticket branch, skip the clean-tree
  requirement: the uncommitted work is the ticket's.

## Phase 2: Pre-work and draft plan

1. Read the code the ticket touches until you can name files, approach and
   trade-offs. Probe running systems only through read-only commands; a
   recorded deploy refusal (`declined`) forbids even read-only production
   probes.
2. Draft the plan: scope, files, approach, tests, docs, risks, what is out of
   scope. Note every decision you could not derive and every point where your
   confidence is low.

## Phase 3: Plan interview (the checkpoint; conditional when `policy.interview` is `conditional`)

0. **Decide whether the interview runs.** With `policy.interview: always`
   it always runs; go to step 1. With `conditional` it runs when any of
   these holds: (a) Phase 2 recorded a genuine design decision the ticket,
   the repo and its docs do not settle, or a point where your confidence is
   low; (b) the panel question fires (step 2); (c) a deploy question is due
   (step 3); (d) the ticket is `blocked` and its impediment must be
   confirmed cleared (step 1); (e) the invocation says "interview me";
   (f) the run is a batch.
   With `ask_panel_when` empty, trigger (b) always fires, so `conditional` saves a stop only alongside a non-empty list.
   When none holds, say so in one line, record the panel
   and the deploy decision (step 4), and continue to Phase 4; the plan
   comment there is still posted before any code changes.
1. When the ticket is `blocked`, first ask whether its impediment has
   cleared, quoting the blocked comment. A no, or no answer, ends the run
   with a report and the ticket stays blocked. Then present the plan
   tightly (on a resume, the posted plan). Then ask the genuine design decisions only,
   typically two or three, recommended option first and labelled. Decide the
   obvious ones yourself and state them.
2. **Review panel.** A panel override in the invocation settles this step
   with no question. Otherwise, when `review.ask_panel_when` is empty, **ask
   it last, always**: only the user knows what this change is worth. When
   it is non-empty, check the plan against each trigger: if none matches,
   do not ask; state in the plan that `review.default_panel` runs and how
   many seats are live (the Phase 0 roster precheck). If one matches, ask,
   naming the trigger. Either way the question offers the panels the
   installed `multi-llm-review` skill (`~/.agents/skills/multi-llm-review/SKILL.md`
   exists) defines (`fast`, `default`, `pro`,
   `ultra` as of template 1.1), recommendation first, based on blast radius
   and reversibility, not diff size: `fast` for a simple, contained change,
   `default` for ordinary work, `pro` or `ultra` for sensitive or
   hard-to-reverse work (auth, data migrations, payments, deploy tooling);
   `review.default_panel` is the recommendation when nothing argues
   otherwise. Every seat runs, including
   the one matching your own model. If the skill is absent on
   this machine, say so and offer the fallback: independent reviewer
   subagents from the harness, ideally two with different models.
3. **Deploy decision.** A decision the invocation already recorded at
   Phase 0 stands: a refusal stays `declined` (do not ask, and do not
   contact production), a deploy request stays `approved` (do not ask
   again, in a batch either). Otherwise, if the
   user said "pause before deploy" this run, record `pending` with that
   checkpoint: it forces a stop at the deploy gate no matter what
   `deploy.auto` says, and you skip the question below. Otherwise, if
   `deploy.auto` is false and deploy is live (`deploy.state` adopted or
   substituted, shape not `none`), ask now whether to deploy this run:
   record `approved` for a yes, `declined` for a no, and `pending` for no
   answer, never approval. **`deploy.on_request` true changes one thing**:
   a single-ticket run does not ask; unless the invocation recorded a
   decision above (a request, a refusal, a pause), it is `deferred`, so a
   conditional interview is not forced open by this step. A batch run asks
   once regardless, for the end of the batch, and there a no is `deferred`
   too. Phase 11 (or Phase 10) asks again only while the
   decision is `pending`, never an answered one: Phase 11 acts on the
   decision, and when `deploy.merge_deploys` is
   true so does Phase 10, because there the merge is the deploy. A
   `declined` with `merge_deploys` ends the run at Phase 10 with the PR open
   (or the reviewed commits unpushed), and the report says why.
4. Record every answer and invocation override verbatim in the subtask
   list, including the selected review panel, the deploy decision
   (`approved`, `declined`, `pending`, `deferred` or `n/a`) with any pause checkpoint, and a
   step-0 skip, so later phases read decisions from the subtasks and never
   from memory.

## Phase 4: Final plan

1. Write the plan to the plan artifact, file named `<ticket>-<slug>.md`
   under `plan.local_dir`. Where that file lives follows `plan.commit`: true
   means inside the workspace, so it is committed in Phase 7 and ships with
   the PR; false means under the **main checkout** or `/tmp`, never inside a
   worktree that Phase 13 removes. Post the same plan as a ticket comment
   (`tracker.commands.comment`) unless ticket-less.

## Phase 5: Implement

- Work in the workspace. Match surrounding style, naming, comment density and
  idioms. Write tests that prove the behaviour, not tests that go through the
  motions; mirror the repo's pattern for skipping when a toolchain is absent.
- Update docs the change requires, respecting feature gating noted in
  `project_notes` below.
- Confirm `git status` shows only intended files before Phase 6.

## Phase 6: Quality gates

Skip this phase when `gates.state` is `skipped` or `impossible`; say so here
and record the missing local evidence in the delivery summary and, if one
exists, the PR body.
Otherwise follow `gates.notes` for prerequisites, then run every command in
`gates.commands`, in order, and make them green. In this project, when a change
affects generated skill renders, edit their sources, run `just skills`, review
the source and render changes, and commit them together with explicit paths
before `just ci`: its staleness gate requires committed renders. This is a
local preparation commit, not permission to push or merge. The same ordering
applies to gate fixes and Phase 9 review fixes. Phase 7 commits only remaining
changes; do not create an empty commit.
These are the **fast** gates (unit tests, lint, typecheck, docs checks): the
feedback loop every ticket pays for. The heavy ones (end-to-end suites,
smoke runs, audits) live in `deploy.gates` and run once before a deploy,
never here. Apply
the autofixers in `gates.autofix` first when a gate fails on formatting or
lint. Fix what the tooling surfaces; do not disable a check to pass it. If a
gate cannot run on this machine (missing toolchain), you may rely on CI for
it **only when a CI check on the PR actually runs that gate**; name it in
the delivery summary and the PR body. Otherwise (no such check, which is
always the case when `pr.state` is `skipped` or `impossible`) an unavailable
required gate stops delivery: report the missing prerequisite, move the
ticket to `tracker.states.blocked` when the tracker has one, with the
Phase 0 step 4 comment (resume at Phase 6, and the state it left), and
never land with a required gate unverified.

## Phase 7: Commit and open the PR

When `pr.state` is `skipped` or `impossible` there is no PR flow: commit, run
the Phase 8 review against the local range `<base>...HEAD`, address findings
and re-run the gates in Phase 9, then go through Phase 10 before pushing to
`pr.base`. **No push in Phase 7 or 9.** That push is the merge, so the gates
do not disappear with the PR: Phase 10 applies its deploy gate (step 0) and
its merge gate (step 3) first, and when `deploy.merge_deploys` is true,
step 4 captures the previous ref and the log baseline immediately before the
push. The wrap-up says the change landed without a PR. Recovery in Phase 12
reverts through the same landing path, a pushed revert commit rather than a
revert PR. Otherwise:

```bash
git add <explicit paths>                       # never add -A
git commit -m "<pr.title_pattern rendered>" -m "<body>"
git push -u <remote> <branch>
gh pr create --base <pr.base> --head <branch> --title "<title>" --body-file <file>
```

Use the repo's forge CLI (`pr.forge`: `gh` or `glab`). Write the PR body to a
file; heredocs are often blocked and files avoid escaping artifacts. Capture
the pushed SHA: `git rev-parse HEAD`. That SHA is what Phase 8 reviews.
When resuming and an open PR/MR for the branch exists, push to it rather
than opening a second.

## Phase 8: Review

**Pin the review to the reviewed HEAD SHA (pushed only with PR flow) and freeze the tree until findings are
synthesised.** Reviewers read the tree at their own execution time, so any
edit mid-run makes their findings drift.

Review is never skipped entirely. When the recorded panel is "skip the
panel", do not run `multi-llm-review`: run the harness fallback of step 2
(one independent reviewer subagent, or your own careful re-read where the
harness has none) and say so in the wrap-up.

1. If `multi-llm-review` is installed
   (`~/.agents/skills/multi-llm-review/SKILL.md` exists): run it with the
   panel recorded in Phase 3, passed explicitly (`REVIEW_PANEL=<panel>` before the command or
   `--panel <panel>` after it) so that skill never asks again. Follow its
   SKILL.md exactly for the launch recipe of your harness, the review root
   (the workspace path: the worktree in worktree mode, the main checkout in
   in-place mode), the caller-model variable, and the mandatory prompt
   footer. It is direct-invocation only, so a skill tool may refuse it or
   not list it: read that SKILL.md by path and follow it; the panel
   recorded in Phase 3 is the user's request for this run. Launch it in
   the background; it takes several minutes and a foreground call that dies
   loses every review.
2. If it is absent and `review.allow_harness_fallback` is true and the
   harness has reviewer subagents: launch at least one, preferably two,
   independent read-only reviewer subagents against the same SHA and range,
   with the same light prompt (what changed, why, files, design intent, focus
   on correctness). Name the degraded coverage in the wrap-up. If the flag is
   false, or the harness has no subagents, this is a conditional stop: ask
   whether to proceed on your own single review, wait for a machine with the
   panel, or skip review for this run, recommendation first.
3. **Concurrently**, when a PR and its checks exist, wait for CI and PR bots
   on the same SHA: `gh pr checks <N>`, the PR reviews, and inline comments.
   Expect whatever `pr.bots` lists and gate on what `gh pr checks` actually
   reports; bot rosters drift. Without a PR or checks, record this wait as
   skipped; the local gates and the panel review remain required.
4. Hold all edits until the panel and any applicable bots have reported.

## Phase 9: Address findings

1. Validate every finding against the actual code at HEAD. Categorise valid,
   invalid, acknowledged. Cross-referenced findings carry more weight;
   scrutinise singletons. Reason over the evidence, never the paraphrase.
2. Fix the valid ones, follow Phase 6's generated-render preparation when
   applicable, and **re-run every gate in `gates.commands`** on the fixed tree
   (skip when `gates.state` is not live, as in Phase 6), then commit any
   remaining changes once they pass; do not create an empty commit. Push once only when PR flow is live; otherwise
   defer the push to Phase 10. Acknowledge out-of-scope or infrastructure
   findings with a reason instead of expanding the PR. Capture the new head
   SHA; Phase 10 requires the gates (and CI, where it exists) green on
   **that** tree and SHA, not the reviewed one. Phase 6's rule for a gate
   that cannot run here applies to the re-run too.
3. **Conditional stop**: if a valid finding contradicts the agreed plan or you
   are not confident whether it is valid, ask the user with your
   recommendation first. Otherwise do not stop.
4. Score the reviewers if the review skill asks for it (see its SKILL.md).
5. A blocking bot review you fix gets re-reviewed after the push; wait for
   the new review rather than overriding.

## Phase 10: Merge

**With no PR flow** (`pr.state` skipped or impossible) the same gate
ordering applies. In step 1 use the local HEAD SHA and the latest gate
result (Phase 6, or Phase 9 if it re-ran) instead of the forge PR commands,
and replace the rulesets check with `git push --dry-run <remote>
HEAD:<base>`, which tells you whether the base accepts a direct push at all
without needing a forge CLI. Skip steps 2 and 6, which exist
only for a PR; in step 5 land locally per `pr.merge_strategy` and push
`pr.base` instead of a PR merge, **in the workspace, never on local
`<base>`** (in-place that branch is the user's and may be behind or dirty;
in worktree mode it is checked out elsewhere): `git fetch <remote>` first,
and if `<remote>/<base>` moved past the branch point, rebase the branch onto
it and re-run the gates before going on. Then `squash` is `git checkout
--detach <remote>/<base> && git merge --squash <branch> && git commit` with
the subject from `pr.title_pattern`; `merge` is `git checkout --detach
<remote>/<base> && git merge --no-ff <branch>`; `rebase` is the rebased
branch itself. Push with `git push <remote> HEAD:<base>` and record the
pushed SHA as the delivery; Phase 13 step 3 brings local `<base>` up to
date. Never force-push over a concurrent update.

0. **If `deploy.merge_deploys` is true, the merge (or push) is the
   deployment.** Apply the deploy gate now; there is no way to land without
   deploying. A `declined` decision **ends the run here**: keep the PR open
   (or the reviewed commits unpushed), the branch and the workspace, leave
   the ticket's state unchanged, and report why, with no further prompt and no
   production reads; Phases 11 to 13 do not run. An uncleared "pause before
   deploy" checkpoint requires asking for approval of the exact merge or
   push command, even when `deploy.auto` is true. Without a pause, ask only
   when neither `deploy.auto` nor this run's `approved` authorises the
   deploy. A yes records `approved` and clears the checkpoint; a no records
   `declined` and ends the run as above; no answer leaves `pending`: stop
   and wait with the workspace retained, without entering Phases 11 to 13.
   Once authorised, Phase 11 has nothing to run and Phase 12 starts at the
   merge. The rollback ref and the log baseline are captured in step 4
   below, not here: recorded now they would go stale while steps 1 to 3
   wait on CI and approvals.
1. Capture the head SHA and confirm CI is green **on it**:
   `gh pr view <N> --json headRefOid` gives the SHA, `gh pr checks <N>`
   reports the runs (it does **not** print which SHA it checked, so compare
   deliberately rather than trusting the listing). A push in Phase 9
   invalidates earlier results. Pass that SHA to the merge as
   `--match-head-commit <sha>` so a concurrent push between the check and the
   merge aborts instead of merging unreviewed code. Then confirm
   mergeability: `gh pr view <N> --json mergeable,mergeStateStatus,state`.
   Check protection with the rulesets endpoint
   (`gh api repos/<o>/<r>/rules/branches/<base>`), never the legacy
   `/protection` endpoint alone, which 404s under rulesets. A 403 reading
   "Upgrade to GitHub Pro or make this repository public" means a private
   repo on a free plan: no protection exists, nothing enforces CI
   server-side, and the green-on-head-SHA check above is the only gate; any
   other 403 is an auth failure, so stop. Repos without CI rely on the latest
   gate run (Phase 6, or Phase 9 if it re-ran); say so in the summary.
2. If `pr.required_approvals` > 0, request `pr.reviewers` and **wait**,
   polling; this is a wait, not a stop.
3. **Merge gate**: if `policy.merge` is `pause`, or the user said "pause
   before merge", stop here, present the merge command and the state you
   verified, and wait.
4. **Immediately before merging**, when `deploy.merge_deploys` is true: run
   `deploy.gates` on the tree about to land (Phase 11's pre-deploy gates).
   Nothing has landed yet, so a failure is fixed **on the ticket branch**
   when the fix is small and `policy.fix_forward` is true, with
   `gates.commands`, one independent review of the fix and `deploy.gates`
   re-run before merging; otherwise end the run here with the PR open (or
   the commits unpushed) and the ticket unchanged, and report the failing
   gate. Then run
   `deploy.previous_ref_command` and record the result, and capture the log
   baseline (`deploy.watch.logs` over the last `duration_minutes`). Last thing
   before the merge, so neither goes stale in an approval wait. Label it the
   **pre-ticket** rollback target, distinct from any later capture.
5. Merge with `pr.merge_strategy` (squash by default), deleting the branch if
   `pr.delete_branch`. Subject follows `pr.title_pattern` plus the PR number.
6. Known quirk: from inside a worktree, `--delete-branch` fails its local
   cleanup with "main is already used by worktree" while the remote merge and
   branch delete succeed. Verify instead of retrying:
   `gh pr view <N> --json state,mergeCommit`, then
   `git ls-remote --heads <remote> <branch>` (empty means deleted).
7. Move the ticket to in review or the state the sidecar maps to "merged,
   not yet verified" (`tracker.states.in_review`). Skip if the tracker has no
   such state. **Repo-backed tracker**: this is a post-merge file edit, so
   make it in the main checkout on the updated base exactly as Phase 13
   describes for the done edit, never in the worktree Phase 13 removes.

## Phase 11: Deploy

Per sidecar (`deploy`). Shape `none`, or `deploy.state` skipped or
impossible: skip to Phase 13 and say so. `merge_deploys` true: only a
completed Phase 10 merge or push reaches this phase, and the deploy already
happened there; go straight to Phase 12. Otherwise **a `declined` decision
skips straight to Phase 13**: report "deployment declined for this run", do
not ask again, and run no production reads, deploy, watch or rollback
commands. **A `deferred` decision skips to Phase 13 the same way**, with
"landed, deployment deferred (on request)", and the ticket still reaches
done. In a batch run this phase runs once, after the last ticket landed.

- Gate: `approved` proceeds. Ask only when the decision is `pending` (an
  unanswered question, or a "pause before deploy" checkpoint, which applies
  even when `deploy.auto` is true): present the exact deploy commands and
  ask. A yes records `approved` and clears the checkpoint; a no records
  `declined` and skips to Phase 13; no answer leaves `pending`: stop and
  wait with the ticket's state unchanged and the workspace retained,
  without entering Phases 12 or 13.
- **Pre-deploy gates first.** From the merged base (checked out as the
  bullet after next describes), run `deploy.gates` in order, once per run,
  before the captures below so they do not go stale while the suites run
  (with `merge_deploys`, Phase 10 step 4 does this instead). They are the
  heavy suites (end-to-end, smoke, audits) that are too slow for every
  ticket. A failure blocks the deploy. When the cause is in this run's
  landed changes, the fix is small and `policy.fix_forward` is true, take
  **one** fix-forward round: fix on a branch off the base, run
  `gates.commands`, one independent review of the fix (as Phase 12 step 1
  describes), land it through the configured landing path, and re-run
  `deploy.gates`. Otherwise stop here: the change stays landed, nothing
  deploys, the ticket stays in its current state, and the report names the
  failing gate and its output. Never skip or trim a pre-deploy gate to get
  a deploy through.
- **Before** deploying, unless the shape is `pseudo-container`: run
  `deploy.previous_ref_command` and record the result so rollback has an
  explicit target. **Keep the first one**: label it the *pre-ticket* target
  and never overwrite it, because the Phase 12 fix-forward round comes back
  through this phase and a second capture would record the broken original as
  the thing to roll back to. Later captures are notes, not replacements.
  Then capture the log baseline by running
  `deploy.watch.logs` over the last `duration_minutes`. A baseline taken after
  the deploy would normalise a regression that starts at once.
  `pseudo-container` skips both: there is no previous revision, and the
  container the log command names does not exist until `deploy.commands`
  creates it. Its baseline is "no errors at all", which is the right bar for a
  container that just started.
- **Run from the merged base, not the feature branch.** After Phase 10 the
  deployable code is `pr.base` at the merge commit, which can also carry
  other work merged while this ticket ran. In the main checkout,
  `git fetch <remote> && git checkout <base> && git pull`, and run the deploy
  from there. A `docker build .` or a local chart apply from the worktree
  would ship the branch, not what actually merged.
- Run `deploy.commands` in order.
- Shape `pseudo-container` (no real deployment exists): build the artifact,
  run it locally as the sidecar describes, and treat the running container as
  the deployment for Phase 12. If a command fails partway, run
  `deploy.cleanup` (when set) before reporting, so nothing the commands
  created outlives the run. `deploy.cleanup` applies only to this shape and
  is ignored for every other one.
- Draft the delivery summary while the rollout progresses.

## Phase 12: Watch

Watch for `deploy.watch.duration_minutes` (default 3). **Zero means one
immediate verification pass**: run the health check, read the logs once
(the log command's own lookback is not a wait) and run the smoke, right
after readiness, with no sleep or poll; when health and smoke are the same
command, run it once for both. With zero, the pre-deploy baseline and the
post-deploy read both use the log command's own fixed lookback, so that
command must carry one (`--since 3m`, not `--since <duration>m`), or both
reads come back empty and the log check passes without looking.
Pass criteria, all of which must hold:

- `deploy.watch.health` succeeds (an HTTP check, a rollout status, a
  container staying up);
- `deploy.watch.logs` over the whole post-deploy window shows no error-level
  entries that were absent from the pre-deploy baseline and are attributable
  to the change;
- `deploy.watch.smoke`, when defined, exercises the delivered functionality
  and passes. If the ticket's feature is reachable and the sidecar has no
  smoke command, attempt a read-only check anyway and report what you did.

**On failure**, the recovery procedure runs autonomously:

1. Diagnose from the logs. If `policy.fix_forward` is false, skip to step 2.
   Otherwise attempt **one** fix-forward round through the shortened gate
   chain: the Phase 6 gates, one independent review (no panel), the
   configured landing path (PR merged on green CI when PR flow is live,
   otherwise the direct push of Phase 10), deploy, watch again for the full
   duration. Note the panel
   review of the fix as a follow-up in the summary. The review here is a
   harness reviewer subagent when one exists; where Phase 8 established that
   this harness has none, it is your own careful re-read of the fix, and the
   report says which it was. A missing subagent never blocks recovery and
   never silently skips the fix-forward.
2. If the fix-forward fails, is skipped, or you cannot diagnose within the
   round, **roll back to the pre-ticket state**:
   - Run `deploy.rollback` naming the revision recorded **before the original
     deploy**. Never "the previous one": after a fix-forward that is the
     broken original, and rolling back to it restores the outage. When
     `deploy.rollback` is empty, that is the `pseudo-container` case and is
     not an omission: stopping and removing the verification container is the
     whole deployment rollback, and the code revert below does the rest.
   - Revert on `pr.base` **exactly what actually landed** — the original
     change alone if the fix never landed (a fix that failed its gates, its
     review or CI landed nothing), both if it did — through the configured
     landing path after the local gates pass, so the tree matches the
     deployment. Require a PR and green CI only when they are configured.
     When both landed, revert them **newest first**: the fix, then the
     original.
     Reverting the original first conflicts with the fix that builds on it,
     and a partial revert leaves the broken original restored. Close an
     unmerged fix PR with a comment; it needs no revert. **In a batch**, "what
     actually landed" is every ticket this run landed: revert them all,
     newest first (the fix, then each ticket in reverse landing order), so
     the base matches the pre-batch revision the rollback restored; their
     branches are already gone (batch item 3), and the landed commits on the
     base are what the reverts target.
   - **Do not revert twice.** When `deploy.merge_deploys` is true the rollback
     recipe *is* this revert, landed through the configured path, plus a
     sync, so run it once: the revert lands,
     the controller reconciles, and there is no separate `deploy.rollback`
     step. Read `deploy.rollback` before acting and skip whichever half it
     already performs.
   - If the shape is `pseudo-container`, save the verification container's
     logs into the report first, then run `deploy.cleanup` (or, when it is
     empty, remove the container with `docker rm -f`), and run the Phase 13
     cleanup step: this branch never reaches Phase 13 on its own, and
     skipping it leaks a worktree and containers. Every other shape keeps
     the workspace and branch for the human.
   - Move the ticket back to in progress with a comment stating what broke,
     what was tried, and the current state; in a batch, every reverted
     ticket, each with the same comment. Stop with a full report.
3. Never attempt a second fix-forward round.

**When `pr.state` is not live**, the same procedure runs without PRs: the
fix-forward round ends in a direct push to `pr.base` once the gates and the
review pass, and the rollback is a revert commit pushed to `pr.base` rather
than a revert PR. Nothing else changes.

## Phase 13: Done and wrap-up

Enter this phase only after the change has landed and the deploy and watch
have passed or were explicitly skipped, `declined` or `deferred`. A `pending`
decision, or a `declined` one with `merge_deploys` (which ends the run at
Phase 10), never reaches this phase. In a batch run, steps 1 and 2 wait for
the batch's deploy outcome (or run right after landing when no deploy runs
this batch), while step 3 runs as soon as each ticket lands.

1. Post the delivery summary as a ticket comment: what landed (PR, merge
   SHA), approach, review findings fixed and acknowledged, deploy and watch
   result, known limitations, follow-ups. When `tracker.commands.create`
   and `tracker.states.backlog` are both set, file each follow-up found
   during the run as its own ticket in `backlog`, never in `ready`, with
   only what is known, and name its key in the summary; otherwise list the
   follow-ups in the summary only. A `declined` deploy makes the summary
   say "landed, deployment declined", a `deferred` one "landed, deployment
   deferred (on request)". **Repo-backed tracker**: the comment
   is a file edit too, so write it in the main checkout on the updated base
   and land it in the same one-file commit as the done state below. Write the same summary to the plan
   artifact when it lives outside the workspace; a committed plan file
   already shipped with the PR and the summary belongs in the ticket and the
   PR description.
2. **Not when the deploy was `declined`**: the ticket stays in its current
   state (`in_review` when the tracker has it, otherwise unchanged), and the
   wrap-up says so; go to step 3. A `deferred` deploy is not a refusal
   (landing is the definition of done where deploys are on request), so it
   takes the normal path. Otherwise move the ticket to done:
   `tracker.commands.close` when the sidecar defines
   one (trackers where done is a distinct operation rather than another state,
   such as GitHub and GitLab issues), otherwise `tracker.commands.set_state`
   with `tracker.states.done`. **Repo-backed tracker**:
   this is a file edit on the base branch after the merge, and the only step
   that touches the user's main checkout. Check it first with
   `git -C <main> status --porcelain`. If it is clean, make the edit there on
   a fresh branch off the updated base and land it as a one-file commit:
   direct push when the base accepts it, otherwise a tiny PR merged on green
   CI. **If it is dirty, or another session holds it, do not touch it**: make
   the edit in a throwaway worktree off the updated base
   (`git -C <main> worktree add --detach $(mktemp -d) <base>`), push from
   there, and remove it. Never make the edit in the delivery worktree you are
   about to remove.
3. Clean up, **only after every record above is persisted** (ticket comment
   posted, done state landed or deliberately withheld, plan file outside
   the workspace). In a batch this step runs as soon as the ticket lands,
   before steps 1 and 2, which wait for the batch's deploy outcome; the
   plan file is already outside the workspace or committed, so nothing is
   lost. Per
   `workspace.mode`:
   - **worktree**: `git -C <main> worktree remove --force <path>`, then
     delete the branch with `-D` (a squash-merged branch is not an ancestor
     of the base).
   - **in-place**: there is no worktree to remove and the branch may still be
     checked out. `git checkout <base> && git pull`, then delete the branch
     with `-D`. Never run `worktree remove` here; it has nothing to remove.

   In both modes: for shape `pseudo-container`, run `deploy.cleanup` (after
   saving any container logs the report needs), which removes everything
   `deploy.commands` created; when it is empty, remove the container with
   `docker rm -f` (stopping alone leaves it holding the name for the next
   run). Other shapes ignore `deploy.cleanup`. Report what was removed.
4. Report plainly: PR number and merge SHA, ticket state, gates run, deploy
   and watch outcome including `deploy.target_class` so the human sees what
   was touched, and what was cleaned up. **Name the review panel and its
   real coverage** as `N of <panel size>` reviewers that ran, or "harness
   fallback, N reviewers". Note if local `main` is behind the remote; do not
   pull for the user if their tree is dirty.
5. Surface friction: anything that fought you, with a concrete fix proposed
   to the sidecar, this skill, or the project docs. Sidecar corrections are
   yours to make now, in the main checkout's copy; skill and doc edits are
   proposals.

---

## Project notes

Prose facts about this project that have no sidecar key. Written at adoption
(2026-09-27) and maintained by hand. Local edits to this file are carried
across template upgrades by the `adopt-deliver-ticket` skill.

- **Portable planning.** Use the tracked repository documentation and the
  ticket as the specification. Missing private notes are not a prerequisite.
  Never copy private notes, credentials or ticket bodies into public artifacts.
- **Plan files** go to a fresh temporary directory outside the worktree,
  remain uncommitted, and are also posted to the ticket.
- **Ticket-less mode is for small changes only**: a typo, a docs fix, a
  dependency bump, a small self-contained bug fix. Anything that touches the
  API contract, schemas, storage, a skill's command contract, or a v4
  roadmap package needs an `AFDEV` ticket; if the free-text task turns out
  to be one of those, stop and ask for a ticket to be filed.
- **Unrefined ideas live in Kaneo's Backlog** (status `planned`). They are
  never delivered; ask the owner to refine one and move it to `to-do` first.
- **Deploy means release, then deploy.** The fleet host only runs published
  images, so Phase 11 cuts the release (`just release`, which runs the heavy
  gates in `just ci-full`) and then runs `scripts/deploy.sh <version>`. It
  happens only when the owner says yes to the Phase 3 deploy question, which
  names the version; that yes is the maintainer's release request that
  `docs/releases.md` requires. Without it, the wrap-up states the release
  class the change needs (patch, minor or major) and that `main` is
  unreleased since the last tag. Never tag, publish or deploy otherwise.
  `.private/deploy.env` is only ever sourced by the sidecar's commands,
  never read or printed.
- **Live credential in the environment**: `AGENT_FEEDBACK_API_KEY` is the
  fleet service's key. Never echo it, never print the environment
  unfiltered, and use throwaway keys (`gate-e2e-key`, `verify-local-key`)
  for local servers.
- **Skill discovery.** Canonical skills and the sidecar live under
  `.agents/skills/`; tracked relative links under `.claude/skills/` expose
  the same files to Claude Code without a machine-local installation.

