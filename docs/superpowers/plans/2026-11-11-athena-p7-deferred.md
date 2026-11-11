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
- In the split editor: keystrokes during a Save are lost, dep chips allow cycles (the server refuses them), and closing discards a dirty draft.
- `WaitFor` on a same-repository Ticket that ended before the wait began is refused rather than answered Done.
- `ask_agent` refuses a held Ticket that waits on the asker only when the wait is direct. In a longer chain the ask is accepted, and `wait_for` returns "Not yet" after 20 seconds.
- When a child fails while its lead has its own question open, the child's own card shows Retry and Stop but not "Continue without it". The lead's card offers it again once its question is answered.
- One unidentified `internal/node` test failure was seen twice under `-race` in a full run. It did not reproduce alone or with `-count=2` or `-count=3`.
- The lead's timeline shows a child's end twice (`child/*` and `input/child`); the UI dedupes by instant.
- An Autopilot firing under the brake is not covered by a test of `SpawnChildren`.
- A refused auto-approve leaves no note on the timeline.
- Parallel plans share the planner's settings and transcript files.
- The fork's first-message path and the GitHub base path have no end-to-end test. The two-machine path is the owner's smoke test.

## Rulings made during the build

Each line: what was decided, why, and what it costs if wrong. F1-F22 answer the pre-flight scan and are also written into the plan text.

- Ruling F1: T7 adds the nodelink Args types and engineProxy methods for its new EngineCallbacks; T9 adds the fleet answers, scoping and Protocol 2 — keeps make check green per task — cost if wrong: T7 is a bit larger.
- Ruling F2: T9 updates TestHelloChecked for Protocol 2 and drops the duplicate TestProtocolMismatchRefused — reuse the existing test — cost: none.
- Ruling F3: sub-task keys must contain a letter; auto-numbered keys are "s1","s2"…; an all-digit key is refused — keys never parse as Ticket ids — cost: an owner-typed "1" is refused with a sentence.
- Ruling F4: agent message ids are "m-<hex>" — WaitFor can tell a message from a Ticket — cost: none.
- Ruling F5: the lead-state check lives in each entry point (hand_off: lead working; ApproveBreakdown: lead planning with a draft), not in the shared spawn — cost: none.
- Ruling F6: the drop cascade stops only sibling children that never started and depend on the dropped child; a plan-first lead never cascades — it removes the dropped child from its Deps and starts when the rest have ended — keeps R8 — cost: none.
- Ruling F7: folder children pin to the lead's folder node (lead.Execution.Node, or the node of its Directory placement), not to a latest Assignment — R2 holds for plan-first leads — cost: none.
- Ruling F8: hops and the rate limit both count only messages inside MessageWindow — a stale chain cannot block a Ticket forever — cost: a slow ping-pong over >10 min is not caught by hops (rate limit still is).
- Ruling F9: a child's reply is allowed when it answers an agent message (reply{message_id}, trigger_id then optional); reply to a Channel stays refused — R3 amended — cost: none.
- Ruling F10: a refusal that raises an Attention commits (fn returns nil) and the command then returns the refusal error — matches Engine.run's rollback-on-error — cost: none.
- Ruling F11: Fleet.Deliver/Start to a Ticket with branches to ship goes through a per-Ticket serialized ship-then-forward path (implementer's design, per-Ticket mutex or queue) — cost: delivery to that Ticket waits up to shipWait.
- Ruling F12: the Engine records events: T3 child/spawned, child/ended; T5 split/drafted, split/approved, split/rejected, split/failed, split/one (R18); T10 tests them in the timeline — cost: a few more event kinds.
- Ruling F13: a child persists its sub-task key (Ticket.SplitKey) and the view sends it; Relatives and e2e show keys mapped from dep Ticket ids — cost: one column.
- Ruling F14: new timeline copy goes where timeline copy lives today and counts inside the 2.5 KB initial-route allowance; the Global Constraint text is corrected — cost: tighter budget.
- Ruling F15: BreakdownView carries max_children — cost: none.
- Ruling F16: ChildSpec and SubTaskView get an optional model hint; §6.4's hand_off "target" is expressed as the optional runtime/node/model hints (new ruling R24) — cost: none.
- Ruling F17: new ruling R25 — a spawn over the WIP brake is refused to the worker with a sentence (it may try again), not recorded as a wip_brake Decision, because a child is not a router candidate; noted as a deviation from §18.1 — cost: the worker, not the router, retries.
- Ruling F18: planner.Config gets Binary, wired from the server's --claude path; all tests run fakeclaude — cost: none.
- Ruling F19: planner Claude config gets StateDir; LLM.Model is dropped (Claude.Model is the one) — cost: none.
- Ruling F20: T5's test filter covers every T5 test name — cost: none.
- Ruling F21: the cap sentence is built in the Engine wrapper that knows the lead id; a planner reply with 0 or 1 sub-tasks runs as one Ticket (R18), not a failed plan — cost: none.
- Ruling F22: PlanCard's "Review split" action moves to T13 with the editor; T12 renders the card without it — cost: none.
- Task 4: Ruling: accept "there is no ticket <id>; agents() lists the live ones." for a never-existing target — "has ended" would be false — cost: none
- Task 5: Ruling: Approve's lead check is Plan.State == draft with a Breakdown (brief's "planning with a draft" read as the draft state) — the only workable reading — cost: none
- Task 6: Ruling: planner runs in <StateDir>/planner (private empty dir); Request.Dir unused by llm — keeps the prompt cache stable and outside any repo — cost: planner can't read repo files (it has no tools anyway)
- Task 8: Ruling: a child's end keeps its checkpoint ref (and branch); the lead's cleanup on the same node deletes them; on another node they stay (falls under the deferred "pruning a child's branch on its own node") — the mirror upload must never lose the child's final head — cost: a few stale refs per remote child until pruning lands
- Task 9: Ruling: accept test substitutions (TestRemoteHandOffSpawns via engine proxy, TestForkSeedAcrossNodes via scratch repo) — node side covered by T8 TestForkSeedStartsAtBase; owner smoke test covers the real two-machine path — cost: one integration seam untested in Go
- Task 9: Ruling: accept node package changes outside the file list (SeedForkLate, keepImport/applyImports) and the one-time "late" mirror wait per commit — needed by the no-clone ruling; a late commit is already announced to the child — cost: none
- Task 11: Ruling: accept Options.Nodes = flt directly (fleet built before Engine) and ctx-cancel re-queue of the plan effect (effects runner keeps the row on shutdown) — cost: none
- Final: Ruling (I2): a child blocked WITH a question shows its own question card in Needs you (the lead dedupe covers failed and question-less blocked only); answering the child clears the lead's child Attention for it — the owner answers the one who asked — cost: two cards in rare cases
- Final: Ruling (I3): refuse ask_agent to a held Ticket that waits on the sender, with a sentence the model reads — cost: none
- Final: Ruling: fix in the same wave — the must-fix minor (child failure must not replace the lead's own Attention), editor Save-then-Approve (setDraft(next)), Revision continues across Replan, planner JSON cannot set base (clear in PlanDrafted), agents.md wait_for wording — all cheap — cost: none
- Final: parked — Replan from a pre-fix stored draft restarts at revision 1 — Ruling: no plan-first draft exists in any deployed build, so no stored draft predates the fix — cost: none
- Final: parked — held-target refusal checks direct deps only (transitive A→C→B→A accepted) — Ruling: real, deferred; wait_for is bounded at 20 s so nothing hangs — cost: a wasted ask in a 3-deep chain
- Final: parked — a failed child kept beside the lead's own question has no "Continue without it" on its own card; other child card kinds show beside the lead — Ruling: real, deferred; the lead's card returns once its own question is answered — cost: one extra click
