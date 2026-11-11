# Agents: splitting work and talking to each other

A worker can split a task into child Tickets that run in parallel, and agents can message each other. This page says
what exists, what each tool refuses, and what the limits are. Every claim is in the code named beside it. Rulings are
written R1 to R25 in `docs/superpowers/plans/2026-11-11-athena-p7-agents-split.md`. What was left for later is in
[the plan 7 deferred file](../superpowers/plans/2026-11-11-athena-p7-deferred.md).

## 1. Leads and children

- A **lead** is a Ticket with child Tickets. A **child** is a Ticket with a `Lead`. A child is a normal Ticket in the
  same repository, so it shows on the dashboard and counts in the waiting-on-you brake.
- Two things make children: the worker tool `hand_off` (mid-work), and an approved plan (up front). Both end in one
  Engine command, `SpawnChildren` (`internal/core/children.go`).
- Splitting needs a git branch (R2). The lead needs a `Repo`, or a `Directory` in a git repository. A plain folder is
  refused with "Splitting needs a git repository: each child works on its own branch." The children of a folder
  Ticket run in that same folder's repository, pinned to the lead's folder node; their node hint is ignored.
- A child's Mandate is the lead's, copied unchanged (R3). A child has no more authority than its lead, no Channels,
  and no review pairing: the lead's own review covers the merged result.
- A child that has deps starts only when they have ended (R1). Until then it has no Assignment, its stage is
  `working/waiting`, and `Ticket.Waiting` lists the deps still open.

### Branches (R4 to R7)

- A `hand_off` child branches from the lead's last commit. The node checkpoints the lead, then reads `HEAD`.
  Uncommitted work of the lead is not in the child, and the tool's description says so.
- Up-front children branch from the default branch, because the lead has no branch yet. The lead starts after all its
  children have ended, then merges them and delivers. It is the only one that pushes or opens a pull request.
- A child with deps is given its deps' branches (an Input naming each one) and merges what it needs. Athena does not
  merge for it.
- A child delivers **commits**. Its `ready` report is refused while its worktree has uncommitted changes: "Commit your
  work on your branch first: your lead merges only commits (git status shows uncommitted changes)." The node then
  records `Report.Head`.
- A child cannot push or open a pull request. `git push` is denied and `athena gh pr create` is refused, with the
  same sentence: "A child ticket delivers through its lead: commit on your branch and report ready; T-n merges it."
  A child in a repository in shadow mode proposes no push either.
- A child's end **keeps** its checkpoint ref and its branch. The lead's cleanup on the same node deletes them. On
  another node they stay until pruning exists (deferred), so the child's final head is never lost before the mirror
  has it.

### What a lead cannot do (R9)

- A lead's `ready` report is refused while any child is open: "T-n has open child tickets (T-a, T-b): wait_for them,
  or ask the owner to drop them, before you report ready."
- When a lead ends, however it ends, its open children are stopped with reason `lead ended`.

## 2. Plan first

Plan first holds a task for a planner, which drafts a split for you to edit and approve (`internal/planner`,
`internal/core/plan.go`).

- **How to ask for it.** The New task tick box "Plan first", `athena task --plan-first`, `SubmitTask.plan_first`, or
  `TaskOptions.PlanFirst` in the Engine. It needs a repository or a git folder. Small requests never pay for planning:
  it happens only when asked for. A Policy rule and a router proposal that ask for it are deferred.
- **The flow.** The Ticket is `working/planning` while the planner runs. Then it is `person/plan`, with a draft: a
  list of sub-tasks (title, instructions, areas, deps, optional runtime, node and model hints) and warnings where two
  sub-tasks touch the same areas. The dashboard's split editor (a lazy chunk) lets you change them. **Approve and
  start** spawns the children. **Run as one ticket** drops the plan and starts the Ticket as it is.
- **A failed plan** shows its reason in the draft, with **Try again** and **Run as one ticket**. It is never retried
  on its own. The stage is `person/plan` too.
- **Fewer than 2 sub-tasks** (R18) runs as one Ticket without asking you. A note goes on the timeline
  (`split/one`).
- **Auto-approve** (R20). Policy `split.autoApprove` is 0 (off) by default. With a number N, a draft of 2 to N
  sub-tasks that flags no overlap starts at once. A refused auto-approve leaves the draft for you.
- **Stale drafts.** An edit or an approve names the draft's revision. If the draft changed meanwhile, it is refused
  with "The split changed meanwhile; reload it and try again."
- The lead of a plan-first Ticket starts only when all its children have ended. Dropping a child of a plan-first lead
  removes it from the lead's deps; the lead is never stopped by it.

### The planner

- It runs on the control server, as one `claude -p` turn on `haiku` with no tools, an empty MCP config and a settings
  file with no hooks (R17). It runs in `<state dir>/planner`, a private empty directory, so its prompt cache stays the
  same and it cannot read a repository. Its fixed prompt is `prompts/planner.md`; the task goes in as quoted data.
- `planner.New(planner.Config)` picks a **Strategy** by name. There are two: `llm` (default) and `off`.
  - `planner.strategy` and `planner.model` are in the server's `config.json`. With `"strategy": "off"` every
    plan-first task fails its plan with "planning is off in this server's configuration", and the owner can still
    run it as one Ticket.
  - To add a Strategy, implement `planner.Strategy` (`Name()` and `Plan(ctx, Request) (Breakdown, error)`), add it to
    the `strategies` map in `internal/planner/planner.go`, and name it in `planner.strategy`. A Strategy that plans on
    local models plugs in here (plan 8).
- The plan runs as an effect, so a server restart in the middle of a plan runs it again instead of losing it.
  A plan has `PlanTimeout` (2 minutes).

## 3. The four worker tools

They are not offered to restricted sessions (R13: "work started by someone outside your allow list cannot hand off
work or message other agents") nor to review sessions. Each refusal below is a UserError the worker reads.

### `hand_off {task, runtime?, node?, model?}`

Splits one sub-task off as a child that branches from your last commit (R4, R24). The first line of `task` is the
title (cut to 80 characters). `runtime` puts the child on the other runtime: that is how §6.1a's split pairing is
done (R16). A child is always a new Ticket, so there is no target Ticket.

Refused when:

- the Ticket has no git repository: "Splitting needs a git repository: each child works on its own branch.";
- the lead is nested too deep: "Child tickets nest at most N deep; do this part yourself.";
- the lead already had its cap of children, ended ones included (R15): "T-n may have at most N child tickets (it has
  M); split into fewer pieces or do the rest yourself.";
- the lead was chosen by Athena and the repository is over the waiting-on-you ceiling (R14, R25): "Athena holds back
  new work in <repo>: N things wait on the owner there (the ceiling is C). Do this part yourself, or wait."
  This is refused to the worker, who may try again. **It is not recorded as a `wip_brake` Decision**, which differs
  from §18.1: a child is not a router candidate. An explicit lead (one you started) is never braked;
- the lead waits for its split plan to be settled;
- the sub-task is bad: an unknown runtime, a model hint over 64 characters, or an unknown node.

### `ask_agent {target, question}`

Messages another agent's Ticket. The answer reaches the asker as a message. The target is a Ticket id, and `T-12`,
`t-0012` and `12` all work (R11).

Refused when:

- the target is not an id: `"x" is not a ticket id`;
- it is the sender itself: "a ticket cannot send a message to itself";
- it does not exist: "there is no ticket T-n; agents() lists the live ones.";
- it has ended: "T-n has ended; agents() lists the live ones.";
- it is not visible: "T-n is not one your ticket can reach: agents() lists those it can." Visible means open and
  one of: the lead, a child, a sibling, or an open Ticket of the same repository;
- the text is empty or over the limit: "a message needs 1 to N characters; this one has M";
- the chain is too long: "This message would be hop H of a chain of agent messages, past the limit of N. Answer
  from what you know, or report waiting with a question for the owner.";
- the sender went over the rate limit: "Your ticket sent N agent messages in the last W, the limit. Wait, or report
  waiting with a question for the owner." This one also puts an Attention on the sender: "Agents are messaging in a
  loop. ... Reply to let T-n go on, or stop it."

A message to a held child is stored and delivered when the child starts (R1).

The receiver answers with `reply {message_id, text}`. A child may do this although it has no Channel (R3), and
`trigger_id` is then optional. Refused: an unknown message, one not sent to you, an answer ("message m-x is an
answer; it needs none"), or a second answer to the same message.

**Messages grant nothing.** An agent message is shown to the receiver as data from another agent, quoted. It is an
`automatic` Input: it never answers a person's question, clears an Attention, or lifts a blocked or failed report.

### `wait_for {target}`

Waits at most `WaitForMax` (20 seconds, R12), below the 30 second request timeout of plan 6. The target is a Ticket
id (it waits for it to end) or a message id `m-…` (it waits for the answer). If it is not done, the answer is: "Not
yet. End your turn with report waiting; Athena delivers the result to you as a message when it arrives." Results
arrive as Inputs anyway, so a worker can end its turn. Refused: a target that is neither id ("... is not a ticket id
or a message id"), an unknown Ticket ("ticket T-n not found") or one not visible ("T-n is not one your ticket can
reach: agents() lists those it can.").

### `agents {}`

Lists the open Tickets the worker can reach, with id, title, stage, action, relation (`lead`, `child`, `sibling` or
`repo`), node, runtime and branch. It never lists others.

## 4. The limits

They are named constants in `internal/core/split.go`. Policy `split` settings override them (Settings, the Policy
file). A 0 or a missing key takes the default. A value out of its range is refused when the Policy is saved.

| Policy key | Default | Range | What it limits |
| --- | --- | --- | --- |
| `split.maxChildren` | 5 | 1 to 20 | children a lead ever had (R15), ended ones included, and the sub-tasks of a draft |
| `split.maxHops` | 3 | 1 to 5 | the hops of an agent-message chain, and how deep children nest (R10) |
| `split.messageLimit` | 10 | 1 to 100 | messages one Ticket sends per window |
| `split.messageWindowMinutes` | 10 | 1 to 120 | the window of the rate limit and of the hop count |
| `split.autoApprove` | 0 (off) | 0 to 20 | the largest draft that starts without you (R20) |

Fixed: `WaitForMax` 20 seconds and `PlanTimeout` 2 minutes.

- Hops and the rate limit count only messages inside the window (F8). A slow chain of over ten minutes is not caught
  by hops, though the rate limit still is.
- Sub-task keys are 1 to 16 characters of `a-z`, `0-9` and `-`, and must contain a letter, so a key is never taken for
  a Ticket id. Auto-numbered keys are `s1`, `s2`, and so on.

## 5. When a child fails (R8)

- A child's `failed` or `blocked` report sets `Attention{Child}` on the lead. The lead's stage is `person/child`, and
  Needs you shows one card, on the lead: "Child T-n (title) failed: ... Retry it, drop it, or continue without it."
- **Retry** is `RetryTicket` on the child.
- **Drop** is `StopTicket` on the child. It also stops, transitively, the children of the same lead that depend on it
  and never started. It never stops the lead. A plan-first lead removes the dropped child from its deps and starts
  when the rest have ended.
- **Continue without it** is `ContinueWithout(child)`. It stops only that child, and its dependents start.
- Each one clears the lead's Attention. A child's failure replaces whatever Attention the lead had.
- The lead is told how each child ended, as an Input (`child/ended` on the timeline). The outcome is `ready` (its
  branch and head, and the `git merge` command), `dropped` (found nothing to do), `stopped` (the child was dropped by
  the owner, or stopped, or the lead ended), or `continued` (you chose to continue without it). The timeline of a
  lead shows `child/stopped` as well as the other three.

## 6. Across nodes

- **Everything goes through the server's mirror.** Node-to-node git is deferred (R7).
- A `hand_off` child placed away from the lead gets a **fork seed**: the lead's checkpoint bundle, with the branch
  created at the lead's head and no tree restored.
  - If the lead's commit has not reached the server yet, the child waits once for it. If it still has not arrived,
    the child **starts from the default branch** and is told so: "T-n's commit did not reach the server in time; you
    start from the default branch."
  - A child may only be on the lead's node when it is a folder child (R2).
- Before a Start or a Deliver, the fleet ships the branch of each ended child or dep that ran on another node, as a
  bundle of its checkpoint from the mirror (the node call `importBranch`). Shipping and forwarding of one Ticket is
  serialised, so delivery to a Ticket with branches to ship waits up to `shipWait`.
- Agent messages, `wait_for` and `agents` use plan 6's request calls. The fleet answers for the Ticket's node.
- A child's branch stays in the clone of its own node when the lead ran elsewhere, until pruning exists (deferred).
- The node link is Protocol 2 (see [nodes.md](nodes.md)). A node of Protocol 1 is refused.

## 7. Stages and timeline

Four stages are new (R23). They are in [003 Execution](../reference/003-execution.md).

`working/planning`, `person/plan`, `working/waiting` and `person/child`.

The timeline has `agent/*`, `child/*` and `split/*` entries: `child/spawned`, `child/ended`, `child/failed`,
`split/drafted`, `split/approved`, `split/rejected`, `split/failed` and `split/one`.
