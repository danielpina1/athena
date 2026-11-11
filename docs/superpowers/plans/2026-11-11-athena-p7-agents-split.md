# Athena Plan 7: Agent-to-agent and Splitting Work Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One Ticket can become a lead with child Tickets: up front, from a planner's breakdown the owner approves in the dashboard, or mid-work, when its worker calls `hand_off`. Workers on any node can also ask each other questions (`ask_agent`), wait for each other (`wait_for`) and see who else is working (`agents`), with loop protection.

**Architecture:**
- **One Engine command makes children.** `Engine.SpawnChildren(leadID, specs)` checks the specs (a DAG, a fan-out cap, a depth limit, the WIP brake) and creates the child Tickets in one transaction.
  - A child carries a copy of the lead's Mandate, a `Lead` link and its `Deps`.
  - It gets its first Assignment only when every dep has ended (Ruling R1).
  - Both `hand_off` and an approved breakdown call it.
- **Children deliver commits, the lead delivers the result.**
  - A child works on its own branch `athena/<child>`, starting either from the lead's last commit (`hand_off`) or from the default branch (up-front split).
  - It cannot push or open a pull request. Its ready report needs a clean worktree and records the branch head.
  - The lead is told by an Input, and merges with git itself.
  - When the child ran on another node, the fleet ships its branch from the server's mirror (plan 6's checkpoint bundles) to the lead's node before the lead's next Start or Deliver.
- **Agent messages are Engine records plus Inputs.**
  - `AskAgent` stores an `AgentMessage` and gives the target an `InputAgent`. The target answers with `reply{message_id}`, which stores the answer and gives the asker an `InputAgent`.
  - Hop count and a per-Ticket rate limit are checked in the same transaction. Exceeding either raises an Attention on the sender and refuses the call.
  - All four tools reach the Engine through `node.EngineCallbacks`. A remote node sends them as plan 6 request calls, which the fleet scopes to the calling Ticket.
- **Plan first is an effect.**
  - A task submitted with `PlanFirst` gets no Assignment. Instead, an `EffectPlan` asks `internal/planner` for a breakdown: the default `llm` strategy, one `claude -p` turn on haiku, with no tools.
  - The draft lives on the Ticket (`Ticket.Plan`). The owner edits, approves or rejects it in a lazy dashboard editor.
  - Approval calls `SpawnChildren`, and the Ticket becomes the lead. It starts once all its children have ended, and merges and delivers their work.

**Tech Stack:**
- **Already in the repo:** Go 1.27, `modernc.org/sqlite`, the MCP Go SDK, protobuf via `buf` and protobuf-es, React 19 with Vite, Vitest and Playwright.
- **New:** none. No new Go modules and no new npm packages.

**Spec:** `docs/superpowers/specs/2026-11-03-athena-design.md`. Read:
- **Binding:** §8.4, §8.5, §6.1a (Split pairing), §6.4 (worker tools), and §5 "Agent message".
- **For context:**
  - §6.1 (worktrees and branches)
  - §10 (dashboard)
  - §12 (cheap models, no polling)
  - §15–16
  - §18.1 (WIP brake)

Also read:
- `docs/superpowers/plans/2026-11-10-athena-p6-multi-node.md`: request calls, the mirror and seeds.
- `docs/superpowers/plans/2026-11-10-athena-p6-deferred.md`: "Plan 7: `ask_agent`/`hand_off` across nodes ride this plan's request calls".
- In `internal/core`:
  - `review.go`: the closest analogue, an Engine-owned second Assignment.
  - `brake.go`: `OwedAttention`.
  - `stage.go`.
- In `internal/node`:
  - `workertools.go`, `mcpcmd.go` and `seed.go`
  - `checkpoint.go` and `node.go:2510-2590`: `ensureWorktree`, and the cleanup at `2690-2745`
- In `internal/fleet`:
  - `calls.go`: the scopes `currentOn`, `assignmentOn` and `pastOn`
  - `seed.go` and `mirror.go`

## Rulings

Owner decisions that the code forces into a shape, or that left a choice open:

- **R1. A held Ticket has no Assignment.**
  - A child whose deps have not all ended, and a plan-first Ticket before its breakdown is settled, get their first Assignment only when they may start.
  - So server recovery, fleet reconcile and `addInput` (which start any pending Assignment) need no change.
  - `Ticket.Waiting` lists the deps not yet ended.
- **R2. Splitting needs a git branch.**
  - The lead must have a `Repo`, or a `Directory` that is in a git repository. A plain folder is refused with "Splitting needs a git repository: each child works on its own branch."
  - Children of a folder Ticket use the same folder's repository and are pinned to the lead's folder node (`lead.Execution.Node`, or the node of its Directory placement), because a folder never moves (plan 6). Their node hint is ignored. A plan-first folder Ticket records that node when it is submitted, since it has no Assignment to pin from.
- **R3. A child's Mandate is the lead's, copied unchanged** (strength, trust class, basis, scope). So it never has more authority than the lead.
  - Children are never paired with review: the lead's own review covers the merged result.
  - Children have no Channels, so their `reply{trigger_id}` and `ack` are refused, as for an owner repo task.
  - A child may still answer an agent message: `reply{message_id}` is allowed, and `trigger_id` is then optional in the schema.
- **R4. Bases.**
  - A `hand_off` child branches from the lead's last commit. The node checkpoints the lead, then reads `HEAD`. Uncommitted lead work is not in the child, and the tool's description says so.
  - Up-front children branch from the default branch, because the lead has no branch yet. The lead starts after all its children have ended, then merges them and delivers.
- **R5. Dependents are not merged for.** A child with deps starts from its base and is given its deps' branches (an Input naming each one, with the branches shipped to its node). It merges what it needs.
- **R6. Children deliver commits.**
  - A child's ready report is refused while its worktree has uncommitted changes. The node then records `Report.Head`, the branch head.
  - A child cannot push or open a pull request: `git push` is denied and `athena gh pr create` is refused, each with the same sentence.
- **R7. Cross-node branches go through the server's mirror, not node to node.**
  - A `hand_off` child placed away from the lead is seeded with a *fork* seed: the lead's checkpoint bundle, with the branch created at the lead's head instead of restoring the tree.
  - Before a Start or Deliver of a Ticket, the fleet ships the branch of each ended child or dep that ran on another node, as a bundle of that Ticket's checkpoint from the mirror (new call `importBranch`).
  - Node-to-node git stays deferred.
- **R8. Failure actions.**
  - A child's `failed` or `blocked` report sets `Attention{Child}` on the lead, and the lead's stage becomes `person/child`.
  - **Retry** is the existing `RetryTicket` on the child.
  - **Drop** is `StopTicket` on the child. It also stops the child's sibling dependents (children of the same lead) that never started. It never stops the lead: a plan-first lead only removes the child from its `Deps` and `Waiting`, and starts when the rest have ended.
  - **Continue without it** is the new `ContinueWithout(childID)`. It stops only the child, and its dependents start.
  - Each of the three clears the lead's Attention. Needs you shows one card, on the lead.
- **R9. The lead cannot outrun its children.** A lead's ready report is refused while any child is open. Ending a lead, however it ends, stops its open children with reason `lead ended`.
- **R10. One limit for chains.** `MaxHops` (default 3) bounds both the hops of an agent-message chain and the nesting depth of children.
- **R11. `ask_agent` targets.**
  - The target is a Ticket id: case-free, and `T-12`, `t-0012` and `12` all work. Tickets have no unique names, so "or name" is ruled as the id only. Capability targets ("any agent with capability X") are deferred.
  - A target must be open and **visible** to the asker: its lead, its child or sibling, or an open Ticket of the same repository.
- **R12. `wait_for` waits at most `WaitForMax = 20 s`**, below plan 6's 30 s request timeout, then answers "not yet". Results always arrive as Inputs anyway, so a worker can end its turn instead.
- **R13. The four agent tools are not offered to restricted sessions** (they join `restrictedWithout`) **nor to review sessions.**
- **R14. The WIP brake.**
  - Children are Tickets in the lead's repository, so `OwedAttention` counts them with no change.
  - `SpawnChildren` is braked only for a lead Athena chose itself (`Mandate.Strength == "inferred"` or `Basis.Via == ViaAutopilot`), exactly as §18.1 brakes starts. An explicit lead is never braked.
- **R15. The fan-out cap** (default 5) counts every child a lead ever had, ended ones included.
- **R16. No new pairing mode.**
  - `hand_off` without a runtime uses the lead's runtime choice, which is `auto` by default.
  - §6.1a's Split pairing ("children pinned to the other runtime") is `hand_off(runtime: …)`, plus a sentence in the worker prompt.
- **R17. Where the planner runs.**
  - It runs on the control server: one `claude -p` turn with `--tools ""`, an empty strict MCP config, a settings file with no hooks, on `haiku`. There is nothing for a hook to guard.
  - A failed plan shows its reason in the draft, with **Try again** and **Run as one ticket**. It is never retried on its own.
- **R18. A breakdown of fewer than 2 sub-tasks runs as one Ticket**, without asking the owner. A note goes on the timeline.
- **R19. Triggers built:** `TaskOptions.PlanFirst`, `SubmitTask.plan_first`, the New task tick box and `athena task --plan-first`. The Policy-rule trigger and the router proposal are **deferred**: the Engine entry point (`TaskOptions.PlanFirst`) is what they will set.
- **R20. Auto-approve** (Policy `split.autoApprove`, default 0, which is off) applies only when 2 ≤ sub-tasks ≤ N and the draft flags no overlap.
- **R21. The draft is a list**, with deps as "after a, b" labels and overlap warnings. The graph view is deferred. The editor is a lazy chunk.
- **R22. The protocol bump.**
  - Node `Protocol` goes from 1 to 2 once, in Task 9, for every Args change in this plan.
  - The bump rule is written in `internal/nodelink/client.go` (`Protocol`'s comment), not in `docs/dev/nodes.md`. Task 14 copies it into nodes.md.
- **R23. New stages:**
  - `working/planning`
  - `person/plan` (a draft or a failed plan)
  - `working/waiting` (deps not done; no Assignment yet)
  - `person/child` (a child failed or is blocked)
- **R24. Hints instead of a target.**
  - `ChildSpec` and `SubTaskView` carry an optional `Model` hint beside `Runtime` and `Node` (§8.5).
  - §6.4's `hand_off(target, task)` is `hand_off{task, runtime?, node?, model?}`. A child is always a new Ticket, so there is no target Ticket.
- **R25. A braked spawn is refused, not recorded.**
  - Over the WIP brake, `SpawnChildren` is refused to the worker with a sentence, and the worker may try again later. No `wip_brake` Decision is recorded, because a child is not a router candidate. This deviates from §18.1.

## Global Constraints

- Every plan 1–6, 10 and 11 Global Constraint still holds: the CSP, the Precision tokens, the client link and storage rules, and `make check` at the end of every task.
- Every task that changes the UI also runs `make e2e`, with screenshots at 390, 820 and 1440 px, light and dark, and axe clean.
- **No new Go modules and no new npm packages.** There is no graph library.
- **The JS bundle budget** is 200 KB gzipped for the initial route, and it is at 197.1 KB now.
  - This plan may add **at most 2.5 KB** to the initial route: the Plan first tick box, the Relatives block, `PlanCard`, `ChildCard` and the new timeline copy.
  - The breakdown editor is lazy, with the sentinel `athena-lazy-chunk:breakdown-editor` added to `web/scripts/bundle-budget.mjs`.
  - The new timeline copy (`agent`, `child` and `split` entries) goes in `timelineModel.ts`, where timeline copy lives today. `ReportCard` imports it, so it is in the initial route and counts inside the 2.5 KB: keep it to short string tables.
  - If the 2.5 KB does not hold, `Relatives` becomes lazy too.
- **Limits** are named constants in `internal/core/split.go`, overridable by Policy `split` settings:
  - `DefaultMaxChildren = 5`
  - `DefaultMaxHops = 3`
  - `DefaultMessageLimit = 10`
  - `DefaultMessageWindow = 10 * time.Minute`
  - `DefaultAutoApprove = 0`
  - `WaitForMax = 20 * time.Second`
  - `PlanTimeout = 2 * time.Minute`
- **Messages grant nothing.** An agent Input is rendered as data from another agent. It never answers a person's question, never clears an Attention and never lifts a blocked or failed report. It is `automatic`.
- **Protocol:** node `Protocol = 2` (R22). A dashboard client's `Hello.protocol` stays 1, since only fields are added.
- **The planner is cheap.**
  - It makes one model turn per plan (model `haiku`, configurable in `planner.model`), with no tools.
  - The fixed prompt lives in `prompts/planner.md` and stays under 2,000 characters, so the prompt cache is reused.
  - The task text goes in as quoted data.
  - Small requests never pay for planning: planning happens only when it is asked for.
- **Copy:**
  - The UI calls them "lead" and "child tickets". The plan is a "split", and its rows are "sub-tasks".
  - Actions: "Plan first", "Approve and start", "Run as one ticket", "Try again", "Retry", "Drop", "Continue without it".
- **Tests never use the owner's machine state.** Git work runs in `t.TempDir()` repositories. Sessions are `fakeclaude`/`fakecodex`. Two-node tests reuse plan 6's loopback harness.

## Review Focus

1. **An owner-edited draft whose deps form a cycle, or name a sub-task that is not there.**
   - Approve and Save are refused with a sentence that names the keys, and no Ticket is made.
   - Tests: Task 1 `TestCheckSpecsRefusals`, Task 5 `TestApproveRefusesBadDraft`.
2. **A lead reports ready, or is stopped, while children still run.**
   - The ready report is refused with the sentence that names them. A stop also stops the children, and nothing is left running.
   - Tests: Task 3 `TestLeadReadyRefusedWithOpenChildren`, `TestEndingLeadStopsChildren`.
3. **Two agents ask each other back and forth, or one floods messages.**
   - The 4th hop and the 11th message in 10 minutes are refused. Each raises one Attention on the sender, and the model reads why.
   - Tests: Task 4 `TestHopLimitRaisesAttention`, `TestRateLimitRaisesAttention`.
4. **A child ran on another node, and its checkpoint reaches the mirror after its ready report.**
   - The lead's next Start or Deliver waits for the branch (up to `shipWait = 5 min`), and the lead's node then has `athena/<child>` at the reported head.
   - Test: Task 9 `TestShipWaitsForChildCheckpoint`.
5. **A child reports ready with uncommitted changes.**
   - The report is refused with the commit sentence, and the child keeps working. No Input reaches the lead.
   - Test: Task 7 `TestChildReadyNeedsCleanWorktree`.

---

### Task 1: Core types and the pure checks of a split

**Files:**
- Create: `internal/core/split.go`, `internal/core/split_test.go`
- Modify:
  - `internal/core/model.go`: the fields below
  - `internal/core/stage.go`: the R23 stages
  - `internal/core/engine.go`: `automatic` counts `InputAgent`, `InputChild` and `InputTask`
- Test: `internal/core/split_test.go`, `internal/core/stage_test.go`

**Interfaces:**
- Produces, in `split.go`:
  - `type ChildSpec struct{ Key, Title, Instructions string; Areas, Deps []string; Runtime, Node, Model, Base string }`.
    - All JSON fields use camelCase and are `omitempty`.
    - `Key` is local to one call or breakdown. A `Deps` entry is a Key there first, and otherwise only a `T-`-prefixed id of a child of the lead (`12` is not a dep).
    - `Model` is an optional model hint (R24).
    - `Base` is a commit to branch from (`hand_off` only).
  - `type Overlap struct{ A, B string; Areas []string }`
  - `type Breakdown struct{ Revision int; Tasks []ChildSpec; Overlaps []Overlap; Note string; Strategy string }`
  - `type PlanState struct{ State string; Breakdown *Breakdown; Error string; At time.Time }`
    - States: `PlanPlanning = "planning"`, `PlanDraft = "draft"`, `PlanFailed = "failed"`, `PlanApproved = "approved"`, `PlanRejected = "rejected"`.
  - `type SplitLimits struct{ MaxChildren, MaxHops, MessageLimit, AutoApprove int; MessageWindow time.Duration }`, and `func (l SplitLimits) WithDefaults() SplitLimits`, which fills the zero fields from the constants in Global Constraints.
  - `type AgentMessage struct{ ID, From, To, Text, ReplyTo string; Hops int; At time.Time }`. `ReplyTo` is set on an answer.
  - `type AgentView struct{ ID, Title, Stage, Action, Relation, Node, NodeName, Runtime, Branch, Repo string }`. `Relation` is one of `lead|child|sibling|repo`.
  - `type WaitResult struct{ Done bool; Ticket *AgentView; Summary, Head string; Answer *AgentMessage; Note string }`
  - `func CheckSpecs(specs []ChildSpec, existing []string, max int) ([]ChildSpec, error)`:
    - It returns the specs with empty Keys numbered `"s1"`, `"s2"`, …, and its refusals are `*UserError`s.
    - It is pure: it knows no lead id.
    - Its rules:
      - 1 to `max−len(existing)` specs, else "a split needs between 1 and <n> sub-tasks; this one has <k>".
      - Title 1–80 runes, and Instructions are required.
      - Keys are unique, match `^[a-z0-9-]{1,16}$` and contain a letter, so a key never parses as a Ticket id. An all-digit key is refused with `sub-task key "1" needs a letter, so it cannot be mistaken for a ticket id`.
      - Each dep is a Key or a `T-`-prefixed id in `existing`, and is not itself.
      - There are no cycles (Kahn's algorithm).
      - Runtime is empty or `ValidRuntime`.
      - Model is empty or at most 64 runes.
  - `func Overlaps(tasks []ChildSpec) []Overlap`:
    - Two areas overlap when they are equal, or one is a path-segment prefix of the other (`internal/core` contains `internal/core/engine.go`, but `internal/co` does not contain it). Trailing `/` and `./` are ignored.
    - It reports pairs in task order.
  - `func ParseTicketID(s string) (string, bool)`, which gives `"T-%04d"` from `T-12`, `t-0012` or `12`.
- Produces, in `model.go`:
  - `Ticket`:
    - `Lead string` (`json:"lead,omitempty"`)
    - `Children []string`
    - `Deps []string`
    - `Waiting []string`
    - `Depth int`
    - `Plan *PlanState`
    - `SplitKey string`: the sub-task key the child was made from (set by Task 3)
  - `Execution.Base string`: the commit a forked child's branch starts at.
  - `Report.Head string`: a child's branch head at its ready report.
  - `Attention.Child string`: the child whose failure raised it.
  - `ReportInput.Head string`.
  - Input kinds `InputAgent = "agent"`, `InputChild = "child"`, `InputTask = "task"`, and the `Input` fields `From string`, `MessageID string` and `Answer bool`.
  - `const SourceAgent = "agent"`, `ReasonLeadEnded = "lead ended"`, `ReasonContinuedWithout = "continued without it"`.
- `TicketStage` checks these first, in order, after `ended` and `offline`:
  1. `Plan.State == planning` gives `working/planning`.
  2. `Plan.State` is `draft` or `failed` gives `person/plan`.
  3. `Attention != nil && Attention.Child != ""` gives `person/child`, before `answer`.
  4. No Assignment and `len(Waiting) > 0` gives `working/waiting`.

- [ ] **Step 1: Write the failing tests.**
  - `TestCheckSpecsRefusals`, table-driven:
    - zero specs
    - six specs with max 5
    - two with existing 4 and max 5, with the count sentence above
    - a duplicate key
    - an all-digit key `"1"`, with the letter sentence
    - a dep `"7"` (not `T-`-prefixed), refused as unknown
    - a dep on itself
    - `a→b→a`, with the message `the sub-tasks' dependencies form a cycle: a → b → a`
    - an unknown dep, with the message `sub-task "b" depends on "x", which is not a sub-task of this split or a child of the lead`
    - a bad runtime
    - an empty title
  - `TestCheckSpecsNumbersKeys`: keys `"s1"` and `"s2"`, and a dep on an existing `T-0007` is kept.
  - `TestOverlaps`: `internal/core` against `internal/core/engine.go` is flagged, `internal/co` against `internal/core` is not, and `web/` against `web` is flagged.
  - `TestParseTicketID`.
  - `TestStagesOfSplit`: the four new stages, plus a child Attention outranking a plain question.
- [ ] **Step 2:** Run `go test ./internal/core -run 'CheckSpecs|Overlaps|ParseTicketID|StagesOfSplit' -race`. Expected: FAIL to compile.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/core -race`. Expected: PASS, with every existing stage test unchanged.
- [ ] **Step 5:** Run `make check`, then commit `feat(core): split types, spec checks and their stages`.

### Task 2: Store: agent messages

**Files:**
- Modify:
  - `internal/store/store.go`: migration 14
  - `internal/core/ports.go`: `Tx` methods
  - `internal/core/fake_test.go`: the in-memory Tx
- Create: `internal/store/agents.go`, `internal/store/agents_test.go`

**Interfaces:**
- Migration 14: `CREATE TABLE agent_messages (id TEXT PRIMARY KEY, from_ticket TEXT NOT NULL, to_ticket TEXT NOT NULL, reply_to TEXT NOT NULL DEFAULT '', at TEXT NOT NULL, doc TEXT NOT NULL); CREATE INDEX agent_messages_from ON agent_messages (from_ticket, at); CREATE INDEX agent_messages_to ON agent_messages (to_ticket, at); CREATE INDEX agent_messages_reply ON agent_messages (reply_to);`
- Produces, on `core.Tx` and implemented by the store:
  - `PutAgentMessage(m *AgentMessage) error`
  - `GetAgentMessage(id string) (*AgentMessage, error)`, which returns nil, nil when there is none
  - `AgentMessagesOf(ticketID string) ([]*AgentMessage, error)`: from or to the Ticket, oldest first
  - `CountAgentMessagesFrom(ticketID string, since time.Time) (int, error)`
  - `AgentAnswer(messageID string) (*AgentMessage, error)`: the message whose `reply_to` is `messageID`, or nil
- Ticket fields ride in the existing `doc` column. There is no new Ticket column, because the lead holds `Children` (R1).

- [ ] **Step 1: Write the failing tests:** `TestAgentMessageRoundTrip`, `TestCountAgentMessagesFromWindow` (three at −11, −5 and −1 minutes, since −10, which gives 2), `TestAgentAnswer`, and `TestMigration14OnExistingDB` (opening a v13 database file keeps its Tickets).
- [ ] **Step 2:** Run `go test ./internal/store -run 'Agent|Migration14' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement, and mirror the methods in `fake_test.go`.
- [ ] **Step 4:** Run `go test ./internal/store ./internal/core -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(store): agent messages`.

### Task 3: Engine: `SpawnChildren` and the life of a lead and its children

**Files:**
- Create: `internal/core/children.go`, `internal/core/children_test.go`
- Modify:
  - `internal/core/engine.go`:
    - `Options.Split func() SplitLimits` and `Options.Nodes NodeDirectory`
    - the hooks in `FileReport`, `endAt`, `RetryTicket` and `StopTicket`
  - `internal/core/ports.go`: `EventChildrenSpawned = "children.spawned"`
  - `internal/policy/settings.go`: `Settings.Split SplitSettings` and its validation
- Test: `internal/core/children_test.go`, `internal/policy/settings_test.go`

**Interfaces:**
- Produces:
  - `type NodeDirectory interface{ Resolve(nameOrID string) (id string, ok bool); Name(id string) string }`. Without one, a node hint is refused with "no such node: <hint>".
  - `func (e *Engine) SpawnChildren(leadID string, specs []ChildSpec) ([]string, error)`. It returns the new child ids in spec order. Its refusals are `*UserError`s, checked in this order:
    1. The lead is open and not in `planning`, `draft` or `failed`. This check belongs to the `SpawnChildren` wrapper only. `o.spawn`, which `ApproveBreakdown` shares (Task 5), runs checks 2–6 and has no lead-state check.
    2. The lead has `Repo` or `Execution.Directory` (R2). The node checks that a folder is a git repository, in Task 7.
    3. `lead.Depth+1 ≤ MaxHops`, else "Child tickets nest at most 3 deep; do this part yourself."
    4. R14's brake, which gives "Athena holds back new work in <repo>: <owed> things wait on the owner there (the ceiling is <c>). Do this part yourself, or wait." It is refused to the worker, who may try again; no Decision is recorded (R25).
    5. The cap, checked in `o.spawn`, which knows the lead id: `len(lead.Children)+len(specs) > MaxChildren` gives "<lead> may have at most 5 child tickets (it has 4); split into fewer pieces or do the rest yourself."
    6. `CheckSpecs(specs, lead.Children, MaxChildren)`.
  - **Each child**:
    - ID `T-%04d`
    - Title and an accept `Decision` with reason `{Code:"split", Values:{"lead": id}}`
    - `Mandate = lead.Mandate`, `Origin = lead.Origin` and `Repo = lead.Repo`
    - `Execution{Directory: lead's, Runtime: spec.Runtime or lead's, Model: spec.Model or lead's, Node: the resolved hint (or, for a folder lead, lead.Execution.Node, else the node of its Directory placement), Base: spec.Base}`
    - `Lead`, `SplitKey` (the spec's Key, numbered), `Deps` (resolved to ids) and `Waiting` (the deps not yet ended), with `Depth = lead.Depth+1` and `Pairing = ""`
    - One `InputTask` brief, whose text names:
      - the lead
      - the title and the instructions
      - the areas
      - the branch and its base
      - the sentence "Commit your work on your branch and report ready; do not push or open a pull request: <lead> merges your branch."
  - A child with an empty `Waiting` gets its Assignment and starts in the same command. `lead.Children` gains the ids.
  - **Child end** (a hook at the end of `endAt` when `t.Lead != ""` and the lead is still open; an ended lead gets nothing), in one transaction with the end:
    - **The lead gets an `InputChild`** with `From` set to the child. Its text depends on how the child ended:
      - completed: "<id> (<title>) is ready: <summary>. Its branch athena/<id> is at <head7>; merge it into yours with git merge athena/<id>."
      - dropped by its worker: "<id> found nothing to do: <summary>."
      - stopped by the owner: "<id> was dropped by the owner."
      - `ReasonContinuedWithout`: "The owner chose to continue without <id>."
    - The lead's `Attention` is cleared when it names this child.
    - **Every sibling and the lead drop the id from `Waiting`, and the lead also drops it from `Deps`.**
      - One that is left with an empty `Waiting` and has no Assignment gets one and starts.
      - A sibling also gets an `InputChild` per dep that completed, naming the dep's branch and head.
    - **A stop that is not `ReasonContinuedWithout`** also stops the sibling children (Tickets with the same `Lead`), transitively, that list the child in `Deps` and never got an Assignment. It never stops the lead: a plan-first lead only drops the id as above.
    - **The Engine records events** on the lead's timeline: `child/spawned` for each child made, `child/ended` at each child end (the outcome is in the event: ready, dropped, stopped or continued), and `child/failed` for a failed or blocked child report.
  - **Lead end** (`endAt` when `len(t.Children) > 0`): each open child ends `EndByExecutor`, `OutcomeStopped`, `ReasonLeadEnded`.
  - **`FileReport`:**
    - A ready report of a lead with an open child is refused with "<lead> has open child tickets (<ids>): wait_for them, or ask the owner to drop them, before you report ready."
    - A child's ready report stores `Head` on the Report.
    - A child's failed or blocked report sets the lead's `Attention{Child: id, Question: "Child <id> (<title>) failed: <summary>. Retry it, drop it, or continue without it."}`. The text reads "is blocked" for a blocked report.
  - `func (e *Engine) ContinueWithout(childID string) error`. It is allowed only for an open child, else "<id> is not a child ticket" or "<id> has ended". It ends the child `EndByHuman`, `OutcomeStopped`, `ReasonContinuedWithout`.
  - `RetryTicket` on a child clears the lead's Attention when it names that child.
- `policy.SplitSettings{MaxChildren, MaxHops, MessageLimit, MessageWindowMinutes, AutoApprove int}` (JSON `split`), with these validation ranges:
  - MaxChildren 1–20
  - MaxHops 1–5
  - MessageLimit 1–100
  - MessageWindowMinutes 1–120
  - AutoApprove 0–20
  - `(Settings).SplitLimits() core.SplitLimits` applies the defaults.

- [ ] **Step 1: Write the failing tests**, with the fake repos.
  - `TestSpawnChildrenCopiesMandateAndStarts`: two children, no deps. Both start, both Mandates equal the lead's, and neither has a Channel.
  - `TestDepsHoldUntilEnded`:
    - b deps on a. b has no Assignment and its stage is `working/waiting`.
    - a ends ready with Head `abc1234`. b starts, and its first Inputs are its brief and an `InputChild` naming `athena/t-000N`.
  - `TestInputDoesNotStartWaitingChild`: an `AddPersonInput` to a waiting child records the Input and starts nothing.
  - `TestSpawnFolderChildrenPinned`: children of a folder lead get `Execution.Node` equal to `lead.Execution.Node`, and a node hint is ignored.
  - `TestSpawnRefusals`: depth 4, the cap at 5 (with the cap sentence naming the lead and its count), a lead in `planning` (refused by `SpawnChildren`, but not by `o.spawn`), a cycle, a Dir-less and Repo-less lead, an inferred lead under the brake, and an explicit lead at the same owed count, which is allowed.
  - `TestLeadReadyRefusedWithOpenChildren`.
  - `TestEndingLeadStopsChildren`.
  - `TestChildFailureAttentionOnLead`: the stage is `person/child`. `RetryTicket(child)` clears it.
  - `TestDropStopsUnstartedDependents`: StopTicket(a) stops b, which deps on a and never started, but not c.
  - `TestDropKeepsPlanFirstLead`: a lead with `Deps` and `Waiting` set as `ApproveBreakdown` sets them. StopTicket(a) does not stop the lead or the other children, and the lead drops a from both lists.
  - `TestChildEventsRecorded`: `child/spawned`, `child/ended` and `child/failed` are on the lead's timeline.
  - `TestContinueWithoutStartsDependents`.
  - `TestChildReadyTellsLead`: one `InputChild` with the branch and head, delivered to the running lead.
  - `TestSplitSettingsValidate`.
- [ ] **Step 2:** Run `go test ./internal/core ./internal/policy -run 'Spawn|Deps|Waiting|Lead|Child|Drop|Continue|SplitSettings' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement. Each command follows the Engine's pattern: one Tx, then Events and Executor calls after commit. A child is put with `PutTicket` in the same Tx as the lead.
- [ ] **Step 4:** Run `go test ./internal/core ./internal/policy -race`. Expected: PASS, with every existing test unchanged.
- [ ] **Step 5:** Run `make check`, then commit `feat(core): child tickets with dependencies, under their lead`.

### Task 4: Engine: agent messages with loop protection

**Files:**
- Create: `internal/core/agents.go`, `internal/core/agents_test.go`
- Modify:
  - `internal/core/engine.go`: the `agentWaiters` map beside `waiters`, under `waitMu`
  - `internal/core/ports.go`: `EventAgentMessage = "agent.message"`

**Interfaces:**
- Produces:
  - `func (e *Engine) AskAgent(fromID, target, text string) (AgentMessage, error)`:
    - It checks, in order:
      1. The text is 1–4,000 runes.
      2. `ParseTicketID(target)`.
      3. The target is not the sender.
      4. The target exists and is open, else "<id> has ended; agents() lists the live ones."
      5. The target is visible (R11), else "<id> is not one your ticket can reach: agents() lists those it can."
      6. The hop limit.
      7. The rate limit.
    - Hops are `1 + max(Hops)` of the messages to the sender with no answer yet **and sent within `MessageWindow`** (an older unanswered message does not count). The fourth hop is refused with "This message would be hop 4 of a chain of agent messages, past the limit of 3. Answer from what you know, or report waiting with a question for the owner."
    - The rate limit counts `CountAgentMessagesFrom(fromID, now−window)` (asks and answers alike). The eleventh is refused with "Your ticket sent 10 agent messages in the last 10 minutes, the limit. Wait, or report waiting with a question for the owner."
    - **On a hop or rate refusal**, the Tx function sets `sender.Attention = {Question: "Agents are messaging in a loop. " + sentence + " Reply to let <from> go on, or stop it."}` (unless an Attention is already set) and returns nil, so the Attention commits (`Engine.run` rolls the Tx back on any error from the function). After the commit the command returns the sentence as a `*UserError`. The tool sees only that sentence.
    - **On success**, it stores the message with ID `"m-" + newID()` and gives the target an `InputAgent{From, MessageID, Text}` through `addInput`, so it is delivered to a running target or starts a pending one. It also emits `EventAgentMessage = "agent.message"` for both Tickets.
  - `func (e *Engine) AnswerAgent(fromID, messageID, text string) (AgentMessage, error)`:
    - The message must be to `fromID`, else "message <id> was not sent to your ticket". It must not be answered already, else "you answered message <id> already".
    - The answer counts against the rate limit, but not the hop limit.
    - It stores `{ReplyTo: messageID}`. An open asker gets an `InputAgent{Answer: true, MessageID, From}`. An ended asker gets nothing more.
  - `func (e *Engine) WaitFor(ctx context.Context, ticketID, target string) (WaitResult, error)`:
    - **A message id** is a target starting with `m-`, matched before `ParseTicketID`. It must be from or to `ticketID`. Done means it has an answer.
    - **A Ticket target** (`ParseTicketID`) must be visible. Done means the target ended, and the result carries its `AgentView`, its last summary and its Head.
    - It waits on an `agentWaiters` channel, keyed `ticket:<id>` or `msg:<id>` and closed after the commit that ends the Ticket or stores the answer. The wait is bounded by `min(ctx, WaitForMax)`.
    - Not done yields `Note: "Not yet. End your turn with report waiting; Athena delivers the result to you as a message when it arrives."`
  - `func (e *Engine) Agents(ticketID string) ([]AgentView, error)`:
    - It lists the visible Tickets: the lead first, then children and siblings in id order, then open Tickets of the same repository by most recent update, 20 at most in all.
    - It fills `NodeName` through `Options.Nodes`, and `Branch` for a completed child.
- A target that is neither `m-…` nor a Ticket id is refused as a bad id. An unknown message gives "no message <id> of your ticket".

- [ ] **Step 1: Write the failing tests.**
  - `TestAskAgentDeliversInput`: the input is `automatic`, it does not clear the target's Attention, and the message is recorded once and listed for both Tickets.
  - `TestAnswerAgentReachesAsker`.
  - `TestHopLimitRaisesAttention`: A asks B (hop 1), B asks C (hop 2), C asks D (hop 3), and D asks A (hop 4), which is refused with the sentence as a `*UserError`. D's stage is `person/answer`, and its Attention text holds the sentence, so the Attention survived the refusal.
  - `TestStaleChainDoesNotBlock`: on a fake clock, an unanswered hop-3 message to A is older than the window. A's new ask is hop 1 and passes.
  - `TestRateLimitRaisesAttention`: on a fake clock, 10 pass and the 11th is refused. At +10 min 1 s, the next passes.
  - `TestAskRefusals`: self, an ended target, an invisible Ticket in another repository, and a bad id.
  - `TestWaitForTicketAndMessage`:
    - It returns done at once for an ended child.
    - A message id whose hex part is all digits (`m-123456789012`) is read as a message, not a Ticket.
    - It wakes within 50 ms when the answer is stored from another goroutine.
    - It returns the note after `WaitForMax`, shortened by a test hook.
  - `TestAgentsVisibleSet`.
- [ ] **Step 2:** Run `go test ./internal/core -run 'Agent|Hop|RateLimit|Ask|WaitFor' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/core -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(core): agent messages, wait_for and agents, with hop and rate limits`.

### Task 5: Engine: plan first and the breakdown draft

**Files:**
- Create: `internal/core/plan.go`, `internal/core/plan_test.go`
- Modify: `internal/core/engine.go`: `TaskOptions.PlanFirst bool` in `SubmitOwnerTask`

**Interfaces:**
- Produces:
  - `const EffectPlan = "plan"`. The key is the Ticket id, and the payload is `{}`.
  - `SubmitOwnerTask` with `PlanFirst` behaves as follows:
    - It needs `Repo` or `Dir` (R2's sentence).
    - It makes the Ticket with its Input and **no Assignment** (R1), and `Plan = {State: planning, At}`.
    - For a `Dir`, it records `Execution.Node` as the folder's node, because children are pinned to it (R2).
    - It enqueues `EffectPlan` in the same Tx.
  - `PlanDrafted(ticketID string, b Breakdown) error`:
    - It applies only when the state is `planning`. Otherwise it is a no-op, as for a late effect.
    - It sets `b.Revision = 1` and `b.Overlaps = Overlaps(b.Tasks)`, and the state becomes `draft`.
    - With `len(b.Tasks) < 2` (R18), it sets `rejected` with the note "The planner found nothing to split; it runs as one ticket." and starts the Ticket.
  - **Events:** the Engine records `split/drafted`, `split/approved`, `split/rejected`, `split/failed` (from `PlanFailed`) and `split/one` (the R18 case) on the Ticket's timeline.
    - When R20 holds, it approves at once, as `ApproveBreakdown` does.
  - `PlanFailed(ticketID, why string) error`: the state becomes `failed`, and `Error` is cut to 300 runes.
  - `UpdateBreakdown(ticketID string, revision int, tasks []ChildSpec) (Breakdown, error)`:
    - It needs the state `draft`, and `revision` equal to the current one, else "The split changed meanwhile; reload it and try again."
    - It runs `CheckSpecs(tasks, nil, MaxChildren)`, recomputes the Overlaps and increments the Revision.
  - `ApproveBreakdown(ticketID string, revision int) ([]string, error)`:
    - It needs the state `draft` and the same revision.
    - It runs the spawn logic of Task 3 inside the same command, as `o.spawn`, which both commands share. Its own lead check is: state `planning` with a draft. `o.spawn` has no lead-state check.
    - It sets `lead.Deps = lead.Waiting = children`, the state `approved`, and gives the lead an `InputTask`: "Your task was split into <n> child tickets: <id: title, …>. When they are done, merge their branches into yours (git merge athena/<id>) and deliver the result."
  - `RejectBreakdown(ticketID string) error`: from `draft` or `failed` to `rejected`. The Ticket gets its Assignment and starts as one Ticket.
  - `Replan(ticketID string) error`: from `draft` or `failed` to `planning`, and enqueues `EffectPlan` again.
- `OwedAttention` counts `person/plan` like any person stage. No change is needed.

- [ ] **Step 1: Write the failing tests.**
  - `TestPlanFirstMakesNoAssignment`: the stage is `working/planning`, and one `plan` effect is queued. A folder submit has `Execution.Node` set to the folder's node.
  - `TestPlanDraftedThenApproved`: three tasks, c deps on a. a and b start, c waits, and the lead has no Assignment. When all three end, the lead starts with the split Input and the three child Inputs, in order.
  - `TestApproveRefusesBadDraft`: a cycle written by `UpdateBreakdown` is refused there, and a stale revision is refused at approve.
  - `TestTinyBreakdownRunsAsOne`.
  - `TestAutoApproveOnlyWithoutOverlap`: AutoApprove 3 approves a draft of 3 with no overlap, but a draft of 3 with an overlap stays a draft.
  - `TestRejectStartsAsOne`.
  - `TestPlanFirstLeadSurvivesDrop`: a, b, c are approved. StopTicket(a) removes a from the lead's `Deps`, and the lead starts when b and c have ended.
  - `TestSplitEventsRecorded`: each of the five `split/*` events is on the timeline in its case.
  - `TestReplanFromFailed`.
  - `TestLatePlanEffectIgnored`: `PlanDrafted` after `RejectBreakdown` changes nothing.
- [ ] **Step 2:** Run `go test ./internal/core -run 'Plan|Breakdown|Replan|Approve|Reject|Split' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/core -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(core): plan first, the breakdown draft and its approval`.

### Task 6: `internal/planner`: the Strategy and the LLM default

**Files:**
- Create:
  - `internal/planner/planner.go`
  - `internal/planner/llm.go`
  - `internal/planner/claude.go`
  - `internal/planner/*_test.go`
  - `prompts/planner.md`
- Modify:
  - `prompts/prompts.go`: `var Planner string`, not owner-editable in this plan
  - `internal/runtime/claude/testdata/fakeclaude/main.go`: `ATHENA_FAKE_PLANNER_FIXTURE`, replayed instead of `ATHENA_FAKE_FIXTURE` when `ATHENA_ROLE=planner`

**Interfaces:**
- Produces:
  - `type Request struct{ TicketID, Title, Text, Repo, Dir string; MaxTasks int; Runtimes, Nodes []string }`
  - `type Breakdown = core.Breakdown`
  - `type Strategy interface{ Name() string; Plan(ctx context.Context, r Request) (Breakdown, error) }`
  - `type Config struct{ Strategy, Model, Binary, StateDir string }`. The defaults are `"llm"`, `"haiku"` and `"claude"`. `"off"` is a Strategy whose `Plan` returns `ErrOff` ("planning is off in this server's configuration").
  - `func New(cfg Config) (Strategy, error)`. An unknown name is an error that lists the known ones.
  - `type Completer interface{ Complete(ctx context.Context, system, user string) (string, error) }`
  - `type LLM struct{ C Completer }` implements Strategy (the model belongs to the Completer):
    - It calls `Complete(ctx, prompts.Planner, userMessage(r))` under `PlanTimeout`. `userMessage` holds MaxTasks, the runtimes and nodes, and the task between `<task>` tags, which the prompt calls data.
    - It parses the first JSON object in the reply, fenced or bare, of the shape `{"tasks":[{"key","title","instructions","areas":[],"deps":[],"runtime":"","node":"","model":""}],"note":""}`.
    - With 2 or more tasks it validates with `core.CheckSpecs(tasks, nil, r.MaxTasks)`. A reply that does not parse or does not validate gives an error: "the planner's split is not usable: <why>".
    - A reply with 0 or 1 tasks is returned as is, without `CheckSpecs`: the Engine runs it as one Ticket (R18).
    - It sets `Strategy: "llm"` and leaves the overlaps to the Engine.
  - `type Claude struct{ Binary, Model, Dir, StateDir string }` implements Completer. `New` builds it from `Config.Binary`, `Config.Model` and `Config.StateDir`:
    - It writes `<Dir>/.athena/settings.json` as `{"hooks":{}}` and `mcp.json` as `{"mcpServers":{}}`.
    - It runs `claude.Start` with `Tools: &""`, `Env: ATHENA_ROLE=planner` and `TranscriptPath: <StateDir>/transcripts/planner.jsonl`.
    - It sends one message, reads Events up to the `Result`, returns `Result.Text` (an error result is an error) and then `Close`s.
- `prompts/planner.md` stays under 2,000 characters. It says:
  - to split only separable work
  - each sub-task should be doable on its own branch
  - name expected files or directories in areas
  - deps only where one sub-task needs another's result
  - at most MaxTasks
  - answer with JSON only
  - a request that should not be split returns one task

- [ ] **Step 1: Write the failing tests.**
  - `TestLLMParsesFencedJSON`.
  - `TestLLMRefusesCycle`.
  - `TestLLMRefusesProse`.
  - `TestLLMSingleTaskPassesThrough`: replies with 0 and with 1 task return a Breakdown, not an error.
  - `TestNewUnknownStrategy`.
  - `TestClaudeCompleterOneTurn`, with fakeclaude and a planner fixture whose result text is JSON:
    - The args hold `--tools ""` and `--model haiku`.
    - The transcript is at `<StateDir>/transcripts/planner.jsonl`.
    - `Complete` returns the text.
    - The process has exited.
  - `TestPlannerPromptSize`: under 2,000 characters.
- [ ] **Step 2:** Run `go test ./internal/planner ./prompts -race`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/planner ./prompts ./internal/runtime/claude -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(planner): a pluggable planner, with a one-turn haiku default`.

### Task 7: Node: the agent tools, the Inputs and the child's rules

**Files:**
- Create: `internal/node/agenttools.go`, `internal/node/agenttools_test.go`
- Modify:
  - `internal/nodelink/proxies.go`: the five request Args and `engineProxy` methods below, so that `var _ node.EngineCallbacks = engineProxy{}` still compiles
  - `internal/node/node.go`:
    - `EngineCallbacks`, with the five methods below
    - `renderInputs`, with cases for `InputAgent`, `InputChild` and `InputTask`
  - `internal/node/workertools.go`:
    - the tools listed, with `reply` gaining `message_id`
    - `restrictedWithout` (R13)
    - the ready-report path, for a child
  - `internal/node/shadow.go`: `childPushRule`, and `athena gh pr create` for a child
  - `internal/node/rules.go`: the `ctx.Child` lead id
  - `prompts/worker.md`: one short section on the agent tools and §6.1a split use

**Interfaces:**
- Consumes: Tasks 3–4's Engine methods.
- Produces:
  - `EngineCallbacks` gains these methods, which `*core.Engine` already satisfies:
    - `SpawnChildren(leadID string, specs []core.ChildSpec) ([]string, error)`
    - `AskAgent(fromID, target, text string) (core.AgentMessage, error)`
    - `AnswerAgent(fromID, messageID, text string) (core.AgentMessage, error)`
    - `WaitFor(ctx context.Context, ticketID, target string) (core.WaitResult, error)`
    - `Agents(ticketID string) ([]core.AgentView, error)`
  - **In `internal/nodelink/proxies.go`**, the matching `engineProxy` methods, each a `NodeCall` with `fact_seq = 0`:
    - `ArgsSpawnChildren{LeadID string; Specs []core.ChildSpec}` (method `spawnChildren`)
    - `ArgsAskAgent{FromID, Target, Text string}` (`askAgent`)
    - `ArgsAnswerAgent{FromID, MessageID, Text string}` (`answerAgent`)
    - `ArgsWaitFor{TicketID, Target string}` (`waitFor`)
    - `ArgsAgents{TicketID string}` (`agents`)
    - The fleet's answers, scoping and the Protocol bump are Task 9. Until then a server does not answer these calls, and `Protocol` stays 1.
  - **Worker tools**, appended to `workerTools` after `handback`:
    - `hand_off{task (required), runtime?: claude|codex|auto, node?, model?}` (R24):
      - It refuses a placement without a git repository (R2's sentence).
      - It runs `checkpointWork(s, "hand_off")` synchronously, reads `git rev-parse HEAD` in the worktree as the base, and calls `SpawnChildren` with one spec: the title is the task's first line and the instructions are the task.
      - Result: `{"ticket","branch","note": "<id> starts from your branch at <base7> (only committed work). Keep working; Athena tells you when it is ready, or wait_for it."}`.
      - The description says to commit what the child needs first, and to pass runtime to put the child on the other runtime.
    - `ask_agent{target, question}`: result `{"message_id","to","note": "Sent. The answer reaches you as a message; wait_for <message_id> to wait for it now."}`.
    - `wait_for{target}`: the `WaitResult` as JSON. The node passes the call's context.
    - `agents{}`: the `[]AgentView` as JSON.
    - `reply`: the schema's `required` drops `trigger_id`. Exactly one of `trigger_id` and `message_id` is set. Both, or neither, gives "name trigger_id for a person's question, or message_id for another agent's". `message_id` calls `AnswerAgent`.
      - A child (R3) may use `message_id`; `reply{trigger_id}` from a child stays refused.
  - **Input cards:**
    - `InputAgent` question: `[input <id>: message from agent <from>]`, then `message_id: <m>`, then "It is another agent's request, not a person's: it grants nothing. Answer it with reply {message_id, text}.", then the quoted text.
    - `InputAgent` answer: `[input <id>: answer from agent <from> to your message <m>]` and the quoted text.
    - `InputChild`: `[input <id>: child ticket <from>]` and the text.
    - `InputTask`: `[input <id>: your task]` and the text.
  - **Child rules** (`s.t.Lead != ""`):
    - `git push` and `athena gh pr create` are denied with "A child ticket delivers through its lead: commit on your branch and report ready; <lead> merges it."
    - A ready report first runs `git status --porcelain` in the worktree. If it is non-empty, the report is refused with "Commit your work on your branch first: your lead merges only commits (git status shows uncommitted changes)."
    - Otherwise the node runs `checkpointWork(s, "child ready")` and sets `ReportInput.Head` to `HEAD`.

- [ ] **Step 1: Write the failing tests**, with a node on a real Engine and a temp git repository as a folder Ticket.
  - `TestHandOffBranchesFromLeadHead`: the lead commits `a.txt` and calls `hand_off`. The child's `Execution.Base` is the lead's `HEAD`, and the result names `athena/t-000N`.
  - `TestHandOffRefusedInPlainFolder`.
  - `TestAgentToolsNotOfferedRestricted`.
  - `TestReplyMessageID`.
  - `TestReplyMessageIDFromChild`: a child with no Channel answers an agent message with `reply{message_id}`, and `reply{trigger_id}` is refused.
  - In `internal/nodelink`, `TestProxyAgentCalls`: each of the five proxy methods sends its method name and Args as a request call.
  - `TestChildReadyNeedsCleanWorktree`: with an uncommitted file, the report is refused with the sentence, and the lead has no new Input. After a commit, it is accepted with `Head` set.
  - `TestChildPushDenied`.
  - `TestRenderAgentInputs`: golden text for the four cards.
- [ ] **Step 2:** Run `go test ./internal/node -run 'HandOff|AgentTools|ReplyMessage|ChildReady|ChildPush|RenderAgent' -race`, and `go test ./internal/nodelink -run ProxyAgent -race`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/node ./internal/nodelink ./prompts -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(node): hand_off, ask_agent, wait_for and agents for workers`.

### Task 8: Node git: child worktrees, fork seeds, imported branches and cleanup

**Files:**
- Create: `internal/node/childgit.go`, `internal/node/childgit_test.go`
- Modify:
  - `internal/node/repos.go`: in `prepareGitHub`, the base is `Execution.Base` when it is set and in the clone
  - `internal/node/node.go`: the folder-Ticket worktree base, likewise, and the cleanup at `2690-2745`
  - `internal/node/seed.go`: `Seed.Fork`, `Seed.Base`

**Interfaces:**
- Produces:
  - **Child worktrees.** `ensureWorktree(clone, workDir, branchName(child), base, env)` takes `base = Execution.Base` when `git cat-file -e <base>^{commit}` succeeds in the clone, which is always true on the lead's own node.
    - With a Base that is missing and no fork seed, the launch fails with "the lead's commit <base7> is not on this node". The fleet prevents this with fork seeds (Task 9).
  - **Fork seeds.** With `Seed.Fork = true` and `Seed.Base = <sha>`, `seedWorktree`:
    - fetches the bundle into `refs/athena/<child>/seed`
    - creates `athena/<child>` at `Base` (which must be in the bundle's history)
    - restores nothing, and sets `movedLine` to "Your branch starts from <lead>'s branch at <base7>, fetched from the server."
  - `type BranchImport struct{ TicketID, Child, SHA, Bundle string }`.
  - `func (n *Node) ImportBranch(bi BranchImport) error`:
    - It fetches the bundle into the Ticket's clone at `refs/athena/<lowercased ticket>/import-<lowercased child>`, then runs `git branch -f athena/<child> <SHA>`, which must succeed.
    - It refuses to move a branch that a worktree on this node has checked out.
    - Importing the same SHA again is a no-op.
    - It uses the `gitIn` pins and `seedTimeout`.
  - **Cleanup:**
    - A child's clean worktree is removed even with unpushed commits, and its branch is kept. Its checkpoint and seed refs go as before.
    - When a lead's worktree is removed, the node also deletes `athena/<child>` for each `t.Children` in that clone, along with its `import-*` refs.

- [ ] **Step 1: Write the failing tests.**
  - `TestChildWorktreeFromBase`.
  - `TestForkSeedStartsAtBase`: a bundle of a lead checkpoint made in another clone. The child's branch is at Base, and the worktree is clean.
  - `TestImportBranch`: it is idempotent, and it is refused on a checked-out branch.
  - `TestChildCleanupKeepsBranch`.
  - `TestLeadCleanupDeletesChildBranches`.
- [ ] **Step 2:** Run `go test ./internal/node -run 'ChildWorktree|ForkSeed|ImportBranch|ChildCleanup|LeadCleanup' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement. Every git call runs with no shell, through `gitIn` or `runGit`.
- [ ] **Step 4:** Run `go test ./internal/node -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(node): child branches from their base, fork seeds and imported branches`.

### Task 9: Nodelink and fleet: the calls across nodes, shipped branches, Protocol 2

**Files:**
- Modify:
  - `internal/nodelink/client.go`: `Protocol = 2`
  - `internal/nodelink/serve.go`: `ArgsSeed.Fork`, `ArgsSeed.Base` and `ArgsImportBranch`
  - `internal/fleet/calls.go`: the answers and scopes
  - `internal/fleet/seed.go`: fork seeds
  - `internal/fleet/mirror.go`: `Has` and the sweep
  - `internal/fleet/fleet.go`: `NodeDirectory`
  - `internal/fleet/fleet_test.go`: `TestHelloChecked` for Protocol 2
- Create:
  - `internal/nodelink/branch.go`: BLOB kind `branch` in, then `importBranch`
  - `internal/fleet/branches.go`: shipping
- Test: `internal/nodelink/branch_test.go`, `internal/fleet/split_test.go`

**Interfaces:**
- Produces:
  - **The request calls** are Task 7's five Args and `engineProxy` methods (`spawnChildren`, `askAgent`, `answerAgent`, `waitFor`, `agents`; each a `NodeCall` with `fact_seq = 0`). This task adds the fleet's answers:
    - The fleet scopes all five with `currentOn(eng, nodeID, <the calling Ticket>)`.
    - `waitFor` runs under the request's context, which is bounded by `WaitForMax` in the Engine.
  - **Seeds and imports, server to node:**
    - `ArgsSeed` gains `Fork bool` and `Base string`, both `omitempty`.
    - New method `importBranch`, with `ArgsImportBranch{TicketID, Child, SHA, Transfer string}`, after BLOB chunks of kind `branch`. It maps to `node.ImportBranch`.
  - `(*Mirror).Has(repo, sha string) (bool, error)`: `cat-file -e` in the repository's bare mirror.
  - **Fork seeding.** `seedAndStart` for an Assignment whose Ticket has `Execution.Base` and no Assignment before, placed on a node other than the lead's latest node:
    - It waits for `Mirror.Has(repo, Base)`, woken by `recordCheckpoint` and bounded by `shipWait = 5 * time.Minute`, then sends a fork seed of the lead's checkpoint bundle.
    - On timeout it seeds without a bundle. The node then starts from the default branch, and the moved line says "<lead>'s commit did not reach the server in time; you start from the default branch."
  - `func needed(t core.Ticket, get func(id string) (core.Ticket, bool)) []core.Ticket` returns the ended children and ended deps of `t` that completed with a `Head`.
  - **Shipping.** Before the fleet forwards a `Start` or `Deliver` of a Ticket with a Repo, it ships each needed Ticket whose last Assignment ran on another node than the target and is not yet shipped there.
    - The shipped set is in memory, keyed by node, child and sha. A reconnect clears it for that node.
    - A ship waits for `Mirror.Has(repo, Head)`, bounded by `shipWait`, then sends `Mirror.Bundle(repo, child)` and `importBranch`.
    - **Shipping is serialized per Ticket** (a mutex or queue per Ticket; the design is the implementer's). `Fleet.Start` and `Fleet.Deliver` to a Ticket with branches to ship go through it: ship first, then forward, in call order, so Delivers keep their order. The wait runs off the Engine's calling goroutine, so Executor calls never block. Delivery to that Ticket waits up to `shipWait`. A Ticket with nothing to ship forwards inline as before.
    - A ship that fails is logged and the forward goes on: the lead's Input already names the branch, and the lead can ask the owner.
  - `(*Fleet)` implements `core.NodeDirectory`: it resolves a node's id or name (not revoked), and `Name` is `nodeName`.
  - `SweepMirror` keeps a child's mirror ref while its lead is open.

- [ ] **Step 1: Write the failing tests**, with plan 6's two-node loopback harness and `fakeclaude`.
  - `TestRemoteHandOffSpawns`: a worker on the remote node calls `hand_off`, and the child is created. The same call naming another node's Ticket is refused as "not on this node".
  - `TestRemoteAskAgentAndAnswer`: across nodes, both Inputs are delivered.
  - `TestForkSeedAcrossNodes`: the lead is on n1 and the child is pinned to n2. The child's worktree branch is at the lead's head.
  - `TestShipWaitsForChildCheckpoint`:
    - The child on n2 reports ready with its checkpoint held back.
    - The lead's Deliver on n1 waits.
    - The checkpoint is released, and n1 has `athena/<child>` at Head before the Input is delivered.
  - `TestHelloChecked` (existing, in `fleet_test.go`) sends `nodelink.Protocol+1` and expects the message "node protocol <Protocol+1>, server protocol <Protocol>; update the node", computed from the constant. There is no separate protocol-mismatch test.
  - `TestShipKeepsDeliverOrder`: two Delivers to a Ticket that waits on a shipment reach the node in call order, after the branch arrives.
  - `TestBranchBlobChunksBounded`: a transfer past `maxSeeds` (4) concurrent ones on one link is dropped, as with seeds.
- [ ] **Step 2:** Run `go test ./internal/nodelink ./internal/fleet -run 'Remote|ForkSeed|Ship|HelloChecked|BranchBlob' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/nodelink ./internal/fleet ./internal/server -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(fleet): agent calls across nodes and child branches through the mirror`.

### Task 10: Protocol, hub views and dashboard commands

**Files:**
- Modify:
  - `proto/athena/v1/commands.proto`
  - `proto/athena/v1/state.proto`
  - `internal/hub/views.go`: summary, detail and timeline
  - `internal/hub/hub.go`: the agent messages in the detail
  - `internal/server/hubsource.go`: `AgentMessages`
  - `internal/server/dispatch.go`
- Generated: `internal/pb/athenav1/*`, `web/src/gen/athena/v1/*` (via `make proto`)
- Test: `internal/hub/views_test.go`, `internal/server/dispatch_test.go`

**Interfaces:**
- `commands.proto`:
  - `SubmitTask.plan_first = 8 (bool)`
  - Command oneof:
    - `UpdateBreakdown update_breakdown = 41` (`ticket_id`, `revision`, `repeated SubTaskView tasks`)
    - `ApproveBreakdown approve_breakdown = 42` (`ticket_id`, `revision`)
    - `RejectBreakdown reject_breakdown = 43`
    - `Replan replan = 44`
    - `ContinueWithout continue_without = 45` (`ticket_id`, which is the child)
- `state.proto`:
  - `SubTaskView{key, title, instructions, repeated areas, repeated deps, runtime, node, model}`
  - `OverlapView{a, b, repeated areas}`
  - `BreakdownView{revision, repeated SubTaskView tasks, repeated OverlapView overlaps, note, state, error, max_children}`, with `max_children` from the current `SplitLimits`
  - `TicketSummary` gains:
    - `lead = 26`
    - `repeated children = 27`
    - `repeated deps = 28`
    - `repeated waiting = 29`
    - `plan_state = 30`
    - `attention_child = 31`
    - `split_key = 32`, from `Ticket.SplitKey`
  - `TicketDetail.breakdown = 6`
  - `TimelineEntry` gains these kinds, written down in the proto comment:
    - `agent`, with subkinds `asked`, `received`, `answered` and `answer`, and `subject` set to the other Ticket
    - `child`, with subkinds `spawned`, `ready`, `failed`, `dropped` and `continued`, and `subject` set to the child
    - `split`, with subkinds `drafted`, `approved`, `rejected`, `failed` and `single`
- `hubSource.AgentMessages(ticketID string) ([]core.AgentMessage, error)` and `hubSource.SplitLimits() core.SplitLimits`. The detail merges the messages into the timeline, redacted and cut like other text, and merges the Ticket's recorded `split/*` and `child/*` events (Tasks 3 and 5): `child/ended` maps to the subkinds `ready`, `dropped` or `continued`, and `child/failed` to `failed`.
- **Dispatch:**
  - `SubmitTask.plan_first` sets `TaskOptions.PlanFirst`. For a folder that `facts.lookup(dir).git` says is not a git repository, it is refused with R2's sentence.
  - The five commands call Task 3 and 5's methods. A `UserError` comes back as the command's `invalid` result, with its sentence.
  - `ApproveBreakdown` is not sensitive, because it grants nothing beyond the lead's Mandate.

- [ ] **Step 1: Write the failing tests.**
  - `TestSummaryCarriesSplitFields`, including `split_key`.
  - `TestDetailBreakdownAndAgentTimeline`: one message each way, rendered as `agent/asked` and `agent/answer`. Recorded events render as `split/drafted`, `split/approved`, `split/rejected`, `split/failed`, `split/single`, `child/spawned`, `child/ready`, `child/failed`, `child/dropped` and `child/continued`. The `BreakdownView` carries `max_children`.
  - `TestDispatchPlanFirstAndApprove`.
  - `TestDispatchUpdateStaleRevision`: the result's message is the stale-revision sentence.
  - `TestDispatchContinueWithout`.
- [ ] **Step 2:** Run `go test ./internal/hub ./internal/server -run 'Split|Breakdown|AgentTimeline|PlanFirst|StaleRevision|ContinueWithout' -race`. Expected: FAIL.
- [ ] **Step 3:** Write the protos, run `make proto`, and implement.
- [ ] **Step 4:** Run `go test ./internal/hub ./internal/server -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(hub): splits, children and agent messages on the dashboard link`.

### Task 11: Server wiring: the planner effect, config, options and CLI

**Files:**
- Create: `internal/server/planner.go`, `internal/server/planner_test.go`
- Modify:
  - `internal/server/config.go`: `FileConfig.Planner PlannerConfig{Strategy, Model string}`, defaults `llm` and `haiku`
  - `internal/server/server.go`: `fx.Handle(core.EffectPlan, …)`, plus `core.Options.Split` and `Options.Nodes`
  - `internal/server/wiring.go`: builds `planner.Config{Strategy, Model}` from `PlannerConfig`, with `Binary` from the server's `--claude` path and `StateDir` from its state directory
  - `internal/cli/cli.go`: `task --plan-first`, and the control socket's submit op carries it

**Interfaces:**
- Produces:
  - `func planHandler(eng *core.Engine, s planner.Strategy, paused func() bool, nodes func() ([]string, []string), limits func() core.SplitLimits, log *slog.Logger) effects.Handler`. It does the following:
    - It reads the Ticket. Unless its state is `planning`, it returns nil.
    - While paused, it returns `effects.Later(30 * time.Second)`.
    - It builds the `Request`: the title, the first Input's text, the repo or dir, `MaxTasks` from the limits, the ready runtimes and the node names.
    - It calls `Plan`, then `PlanDrafted`, or on error `PlanFailed(err.Error())`.
    - It returns nil in every case except a store error, so a planner failure is never retried (R17).
  - **Options.** `core.Options.Split` is `func() core.SplitLimits` from the current Policy settings. `core.Options.Nodes` is the Fleet, reached through a holder set once the fleet is built.
  - `athena task --plan-first "<text>" --repo o/n|--dir D` prints `T-0012: planning a split; review it in the dashboard`.

- [ ] **Step 1: Write the failing tests.**
  - `TestPlanEffectDraftsWithFakeClaude`: a server started with `--claude` pointing at fakeclaude and `ATHENA_FAKE_PLANNER_FIXTURE` giving 3 tasks. A plan-first task reaches `person/plan` with 3 tasks and one overlap, and the planner ran the fakeclaude binary (its transcript is under the state directory). No test runs the real `claude`.
  - `TestPlanEffectFailureShown`: a prose reply gives `failed`, and the error has the sentence.
  - `TestPlanEffectSurvivesRestart`: stop the server while the effect waits (held by `ATHENA_FAKE_HOLD_RESULT`), start it again, and the draft arrives.
  - `TestPlanHeldWhilePaused`.
  - `TestTaskPlanFirstFlag`.
- [ ] **Step 2:** Run `go test ./internal/server ./internal/cli -run 'PlanEffect|PlanHeld|PlanFirst' -race`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./... -race`. Expected: PASS.
- [ ] **Step 5:** Run `make check`, then commit `feat(server): run the planner as an effect, and plan-first tasks from the CLI`.

### Task 12: Dashboard: Plan first, relatives, timeline and Needs you

**Files:**
- Modify:
  - `web/src/features/new-task/NewTask.tsx`: a "Plan first" `Switch`, with the hint "Athena drafts a split into sub-tasks for you to approve first."
    - It is enabled only with a repository, or a folder the repo list marks as git. Otherwise its hint reads "Needs a repository."
  - `web/src/lib/commands/actions.ts`: `submitTask({…, planFirst})`, `updateBreakdown`, `approveBreakdown`, `rejectBreakdown`, `replan`, `continueWithout`
  - `web/src/lib/store/selectors.ts`:
    - kinds `plan` and `child`
    - a child whose lead's `attentionChild` names it is not listed on its own (R8)
  - `web/src/features/needs-you/NeedsYou.tsx`
  - `web/src/features/tickets/TicketDetail.tsx`: mount `Relatives`
  - `web/src/features/tickets/timelineModel.ts` and `Timeline.tsx`: the `agent`, `child` and `split` entries
- Create:
  - `web/src/features/tickets/Relatives.tsx`
  - `web/src/features/needs-you/PlanCard.tsx`
  - `web/src/features/needs-you/ChildCard.tsx`
- Test: co-located `*.test.tsx` and `timelineModel.test.ts`

**Interfaces:**
- Consumes: Task 10's fields and commands.
- **UI rules:**
  - **`Relatives`** is a "Lead" row (a link and its stage pill), then a "Child tickets" list: id, title, stage pill and "after a, b" for deps still waiting. The labels are the deps' `splitKey`s, found among the lead's child summaries (the id when none is found). It renders nothing when the Ticket has neither.
  - **`PlanCard`** reads one of:
    - "A split of <title> waits for you (<n> sub-tasks)" for a draft
    - "Planning <title> failed: <error>" when it failed
    - It has no "Review split" action in this task; Task 13 adds it with the editor.
  - **`ChildCard`**, on the lead, shows the lead's attention question and three actions:
    - "Retry", which runs `retry(child)`
    - "Drop", which runs `stop(child)` behind a confirm ("Drop <child>? Child tickets that wait for it are dropped too.")
    - "Continue without it"
  - **Timeline copy:**
    - `agent/asked` reads "Asked <T>", `agent/received` "<T> asked", `agent/answered` "Answered <T>" and `agent/answer` "<T> answered".
    - `child/*` reads "Child <T> <spawned|is ready|failed|was dropped|was left out>".
    - `split/*` reads "Split drafted (<n>)", "Split approved", "Run as one ticket" and "Planning failed".
- **Budget:** these additions, including the timeline copy in `timelineModel.ts`, stay within the 2.5 KB of Global Constraints.

- [ ] **Step 1: Write the failing Vitest tests.**
  - `NewTask.test.tsx`: Plan first is disabled without a repository, and is sent as `planFirst`.
  - `Relatives.test.tsx`: "after a, b" shows keys mapped from dep ids through `splitKey`.
  - `PlanCard.test.tsx`: the draft and failed texts, and no "Review split" button.
  - `ChildCard.test.tsx`: the three actions call the right commands, and Drop confirms first.
  - `selectors.test.ts`: `plan` and `child`, and the deduplicated child.
  - `timelineModel.test.ts`: the new entries.
- [ ] **Step 2:** Run `cd web && npx vitest run src/features/new-task src/features/tickets src/features/needs-you src/lib/store`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `cd web && npx vitest run`, then `make check`. Expected: PASS, with the bundle budget printing ≤ 199.6 KB.
- [ ] **Step 5:** Commit `feat(web): plan first, child tickets and agent messages`.

### Task 13: Dashboard: the breakdown editor, and the split end to end

**Files:**
- Create:
  - `web/src/features/split/BreakdownEditor.tsx`, which exports `LAZY_SENTINEL = "athena-lazy-chunk:breakdown-editor"` and is opened lazily from `PlanCard` and from the Ticket detail of a Ticket in `person/plan`
  - `web/src/features/split/BreakdownEditor.test.tsx`
  - `web/e2e/split.spec.ts`
- Modify:
  - `web/src/features/needs-you/PlanCard.tsx`: the primary action "Review split", which opens the editor
  - `web/scripts/bundle-budget.mjs`: the sentinel
  - `web/e2e/global-setup.ts`: `ATHENA_FAKE_PLANNER_FIXTURE` pointing at `web/e2e/fixtures/planner.jsonl`, which is new and holds 3 tasks: `api`, `ui` (after api), and `docs`, whose areas overlap `ui`

**Interfaces:**
- **Editor:** a list of sub-task rows. Each row has the key (read-only), and title, instructions, areas (comma-separated) and runtime fields that can be edited.
  - Deps are toggle chips of the other keys, shown as "after api".
  - An overlap shows under both rows: "Overlaps with docs: web/src". It is a warning and does not block.
  - Rows can be added and removed, up to the `BreakdownView`'s `max_children`.
  - A row also has an optional model field.
  - The buttons are "Save" (`updateBreakdown` with the revision), "Approve and start" (save if dirty, then `approveBreakdown`) and "Run as one ticket". A failed plan shows "Try again" (`replan`) and "Run as one ticket".
  - A stale revision shows the sentence, and the draft reloads from the store.
  - There is no graph (R21).

- [ ] **Step 1:** Write `BreakdownEditor.test.tsx`:
  - editing a title sends the revision
  - a dep chip toggles
  - the overlap warning shows on both rows
  - the stale-revision sentence shows
  - Approve on a dirty draft saves first
  - adding rows stops at `max_children`
  - in `PlanCard.test.tsx`, "Review split" opens the editor
- [ ] **Step 2:** Run `cd web && npx vitest run src/features/split src/features/needs-you`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Write `web/e2e/split.spec.ts`:
  1. Create a temp git repository with one commit, and pick it as the folder in New task. Tick Plan first and submit.
  2. The PlanCard appears in Needs you. Open "Review split" and see the overlap warning.
  3. Remove the `docs` row and rename `ui`. Approve and start.
  4. The lead's Relatives shows `api` working and `ui` "after api".
  5. Report `api` ready through the harness (`session.report`). `ui` starts.
  6. Report `ui` failed. A ChildCard appears on the lead, and the child is not listed on its own. Continue without it.
  7. The lead starts, and its timeline shows "Child … is ready" and "Child … was left out".
  - Take screenshots per Global Constraints.
- [ ] **Step 5:** Run `make check && make e2e`. Expected: PASS, axe clean, and the sentinel lazy.
- [ ] **Step 6:** Commit `feat(web): the split editor, and splitting end to end`.

### Task 14: Docs, the smoke test, deferrals and the roadmap

**Files:**
- Create:
  - `docs/dev/agents.md`
  - `docs/superpowers/plans/2026-11-11-athena-p7-deferred.md`
- Modify:
  - `docs/dev/nodes.md`: a section "Protocol versions", with the bump rule from `nodelink/client.go` and that plan 7 made it 2
  - `docs/dev/smoke-test.md`: a section "Splitting work"
  - `docs/superpowers/plans/2026-11-03-athena-roadmap.md`: the "**Plan 7 is built**" note, last

**What goes in each:**
- **`docs/dev/agents.md`** covers:
  - Plan first and the draft
  - what a lead and a child are, and their branches (R4–R7)
  - the four tools with their exact refusals
  - the limits and the Policy `split` keys
  - the failure actions (R8)
  - the planner strategies, with how to add one (`planner.New`) and `planner.strategy: off`
  - what crosses nodes and how
- **The smoke section**, run by the owner:
  1. A plan-first task on the Athena repository itself, split into two.
  2. One child pinned to the second machine.
  3. Both children ready, and the lead merges and opens one PR.
  4. A worker's `ask_agent` to its sibling is answered.
  - Record "not run" if the owner has not run it.

- [ ] **Step 1:** Write the docs and the deferred file (below).
- [ ] **Step 2:** Run `make check && make e2e`. Expected: PASS.
- [ ] **Step 3:** Write the roadmap note: branch `p7-agents-split`, the reference `agents.md`, what was built, and what was deferred, with a link to the deferred file. Commit `docs: plan 7 agents, splitting and deferrals`.

---

## Deferred to later plans (decided here)

- **The graph view of a draft** (§8.5 "list and graph"). The list shows deps as labels, and a graph waits until the bundle has room (plan 9 trims it).
- **The Policy rule and the router proposal that trigger plan first** (§8.5 "When"). The Engine entry point exists (`TaskOptions.PlanFirst`), and wiring it to the router's accept needs the router to classify size. That goes to plan 9, with the scoreboard data, or to a follow-up of plan 2's router.
- **`ask_agent` to "any agent with capability X"** (§8.4), and Ticket names as targets.
- **A planning model on local inference.** This is plan 8: the `Completer` interface is where a local backend plugs in.
- **Node-to-node git** for child branches (§8.4 "artifacts node-to-node"). Everything goes through the mirror, as in plan 6.
- **Pruning a child's branch on its own node** when the lead ran elsewhere. It stays in that node's clone until the clone is pruned.
- **Owner-editable planner prompt** in Settings > Prompts.
- **Automatic merging** of dep branches into a dependent, and conflict help for the lead.
- **Plan 9:** per-split metrics (planning cost, children per lead, time saved), and agent-message counts on the Performance page.
