# Plan 7: deferred items and rulings

Plan 7 was agents and splitting work (`docs/superpowers/plans/2026-11-11-athena-p7-agents-split.md`): child Tickets, the worker tools `ask_agent`, `wait_for`, `agents` and `hand_off`, plan first with the split editor, cross-node branches through the server's mirror, and node Protocol 2. The reference is `docs/dev/agents.md`. This file records what was left for later, where each item goes, and where the build differs from the plan.

## Owner checks not run yet

- **The splitting smoke test** in `docs/dev/smoke-test.md` ("Splitting work"): plan first on the Athena repository split into two, one child on the second machine, both ready and the lead opening one pull request, and an `ask_agent` answered. Not run.

## Deferred to later plans (decided here)

- **The graph view of a draft** (§8.5 "list and graph"). The list shows deps as labels. A graph waits until the bundle has room (plan 9 trims it).
- **The Policy rule and the router proposal that trigger plan first** (§8.5 "When"). The Engine entry point exists (`TaskOptions.PlanFirst`). Wiring it to the router's accept needs the router to classify size. That goes to plan 9, with the scoreboard data, or to a follow-up of plan 2's router.
- **`ask_agent` to "any agent with capability X"** (§8.4), and Ticket names as targets.
- **A planning model on local inference.** This is plan 8: the `planner.Strategy` interface is where a local backend plugs in.
- **Node-to-node git** for child branches (§8.4). Everything goes through the mirror, as in plan 6.
- **Pruning a child's branch on its own node** when the lead ran elsewhere. A child's end keeps its checkpoint ref and branch; the lead's cleanup on the same node deletes them. On another node they stay in that node's clone until the clone is pruned.
- **Owner-editable planner prompt** in Settings > Prompts.
- **Automatic merging** of dep branches into a dependent, and conflict help for the lead.
- **Plan 9:** per-split metrics (planning cost, children per lead, time saved), and agent-message counts on the Performance page.

## Where the build differs from the plan

- **A child's end can be `stopped`.** The lead's timeline and Input say "dropped by the owner" or "stopped" for it, beside `ready`, `dropped` and `continued`.
- **The checkpoint is kept** until the lead's cleanup (see the pruning item above). The plan deleted it at the child's end, which could lose the child's final head before the mirror had it.
- **A fork falls back to the default branch.** If the lead's commit has not reached the server after one wait, the child starts from the default branch and is told so.
- **R25.** A spawn over the WIP brake is refused to the worker with a sentence. It is not recorded as a `wip_brake` Decision, which differs from §18.1.
- **R24.** §6.4's `hand_off(target, task)` is `hand_off{task, runtime?, node?, model?}`.
- **Dropping a child** of a plan-first lead never stops the lead (F6).
- **A reply to an agent message is allowed for a child** (F9, R3 amended).

## Minor items found in review and not fixed

- A sub-task key that parses as a Ticket id (`t-12`) is not refused. Keys with only digits are (F3).
- A sub-task key `s1` can collide with an automatic `s1`.
- The cap sentence reads "between 1 and 0" when the cap is already used.
- Plan Revision restarts at 1 after Replan, so a stale client could approve a new draft.
- In the split editor: Save then a quick Approve is refused as stale, keystrokes during a Save are lost, dep chips allow cycles (the server refuses them), and closing discards a dirty draft.
- `WaitFor` on a same-repository Ticket that ended before the wait began is refused rather than answered Done.
- A child's failure overwrites a pending Attention of the lead, so the lead's own question leaves Needs you.
- The lead's timeline shows a child's end twice (`child/*` and `input/child`); the UI dedupes by instant.
- An Autopilot firing under the brake is not covered by a test of `SpawnChildren`.
- A refused auto-approve leaves no note on the timeline.
- Parallel plans share the planner's settings and transcript files.
- The fork's first-message path and the GitHub base path have no end-to-end test. The two-machine path is the owner's smoke test.
- One unidentified `internal/node` test failure was seen once in an early `make check` and did not reproduce.
