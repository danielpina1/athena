# Athena — design

Date: 2026-11-03
Repository: https://github.com/danielpina1/athena
Reference implementation: https://github.com/deepseek-ai/dsh-superagent-prototype (MIT), called "the reference" below.

## 1. Intent

Athena is a personal, proactive agent system that runs Claude Code and Codex sessions across several of the owner's machines and is supervised from a dashboard that works equally well on a phone, tablet or desktop.

It takes the reference's way of working — *take as little human attention as possible and give as much value as possible* (reference `docs/000-philosophy.md`) — and its domain model (requests become Tickets, each worked by its own agent session, which reports back and only interrupts a person when one must act), and rebuilds it:

- on **Claude Code and Codex** headless sessions instead of DeepSeek Harness;
- as a **standalone Go system** instead of a Harness plugin;
- across **multiple machines (nodes)** instead of one process on one host;
- with a **mobile-first dashboard** instead of a Harness sidebar panel;
- with **local model inference** available on a node.

The owner asked for it to be *extremely efficient* and to support *advanced communication between devices*. Both are first-class requirements, defined in §2.

## 2. Goals, non-goals, anti-goals

### Goals

1. A request (GitHub mention/issue/PR comment, or a task typed in the dashboard) becomes a Ticket and is worked to completion by a Claude Code or Codex session on a suitable node, with the result delivered where it was asked (e.g. a pull request and a reply on GitHub).
2. **Any repository, any task** (owner, repeated as a hard requirement). Nothing in Athena assumes a language, framework, build system, repository host or task type:
   - **Any git repository**: GitHub, GitLab, Bitbucket, Gitea/self-hosted, or local-only with no remote. Worktrees (§6.1) work for all of them.
   - **Any directory**: a non-git folder can be a Ticket's target (one active Ticket per non-git folder at a time, since it cannot be isolated with a worktree; further Tickets on it queue).
   - **No repository**: research, writing, scripts, data work, ops tasks in a scratch directory.
   - **Delivery adapts to the target**: GitHub → PR as the GitHub App (plan 2); other hosts → push the Ticket's branch and report its link (a host adapter for PR/MR creation, e.g. GitLab, plugs into the same Source/delivery interface later); local-only repos → the branch stays local and the dashboard shows its diff for review; no repository → the result is the Report plus any files left in the Ticket directory, viewable/downloadable from the dashboard.
   - The worker prompt tells sessions to discover how a repository builds and tests itself (README, CI config, manifests) rather than assuming a toolchain.
3. Runtime is selectable per Ticket: `claude`, `codex`, or a local model through either; with a configurable default.
4. Multiple machines: a control server plus any number of nodes (macOS, Linux). Work is dispatched by node capability and load.
5. Advanced inter-device communication, meaning all of:
   - **agent ↔ agent across nodes**: a session can ask another session (on any node) a question, hand off a piece of work, or wait on its result;
   - **work moves between nodes**: a Ticket's branch/workspace can be continued on another node;
   - **instant multi-screen sync**: every dashboard (phone, laptop) reflects every change in well under a second;
   - **one persistent, resumable link per device** carrying commands, live session output and state, surviving drops without loss or duplication.
6. Extremely efficient, meaning all of:
   - **latency**: dashboard actions reach the session, and session output reaches the dashboard, in < 250 ms on the VPN (excluding model time);
   - **footprint**: the node program idles at < 30 MB RSS and ~0% CPU; the control server < 100 MB with dashboard served;
   - **AI spend**: routing and small chores use a cheap or local model; sessions are resumed, not restarted, so prompt caches are reused; no polling loops that call a model;
   - **human attention**: notifications only for things that need a person (the reference's stage model, §5).
7. Dashboard on phone/tablet/desktop: see what needs you, answer questions, approve risky actions, retry/stop Tickets, start new Tickets (choose repo, node, runtime/model), watch any session live and type into it, see nodes and their load. Installable as a PWA with push notifications.
8. Local model inference on at least one node, usable by sessions on that node and, through it, by other nodes.
9. **A beautiful, clean, impeccable UI** (owner's words). Visual and interaction quality is a requirement on par with correctness, not polish to add later (§10.1).

### Non-goals (v1)

- Multiple users, teams or roles. One owner.
- Feishu source (reference has one; may be ported later — keep the Source interface so it can be).
- Windows nodes.
- Running in the cloud / serverless. Everything runs on the owner's machines.
- Native mobile apps. The PWA is the mobile app.
- Replacing the VPN: Athena relies on it for reachability.

### Anti-goals (failure even if it "works")

- An agent that spams: messages, notifications or GitHub comments when no one needs to act.
- Any secret (GitHub token, API keys, device keys) leaving the control server or the node that owns it, appearing in logs, prompts, URLs or the dashboard.
- A node or session able to act beyond its Ticket's mandate (the reference's authorization model, §5).
- Silent loss: a message, report, approval or Ticket that disappears on a disconnect or restart.
- A dashboard that only works well on desktop.
- A dashboard that looks like a generic admin template.

## 3. Decided by the owner

These were decided in conversation and are not open:

- Build off dsh-superagent-prototype.
- Workers are Claude Code and Codex sessions, **headless**, controlled with **hooks**.
- Multi-device dashboard; multi-node environment.
- Extremely efficient; advanced inter-device communication.
- **Go** for the backend.
- **UniFi VPN** (WireGuard) for inter-device networking.
- Local model inference on one of the nodes.
- **vLLM** (not Ollama) serves local models (owner, 2026-11-04), and **macOS on Apple silicon is the primary local-inference target, tuned for the best performance possible** (owner, 2026-11-04). Linux/NVIDIA uses the same vLLM API.
- Code lives in `danielpina1/athena`.

Everything else in this document is the designer's call unless marked otherwise; §15 lists the main ones.

## 4. Relationship to the reference

The reference is TypeScript on DeepSeek Harness (`@deepseek-ai/cordis`, `dsh-session`, `dsh-agent`, …). With a Go backend, **its code is not reused; its design is ported.** It is the behavioral specification.

| Reference | Athena |
| --- | --- |
| `src/core/*` (Engine, tickets, stages, intake, assignments, replies, memory, evals) | Ported to Go `internal/core`, same concepts, names, invariants. Single writer of domain state; synchronous commands; one transaction per command; ports enqueue external work that adapters perform after commit (reference `docs/007-architecture.md`). |
| `src/core/ports.ts` (`Repositories`, `Assessor`, `Executor`, `Outbox`, `SourceScope`) | Same ports as Go interfaces. `Executor` is implemented by the dispatcher (§7). `Assessor` by the router (§6.3). |
| `src/app/store.ts` (node:sqlite, WAL) | Go store on SQLite (WAL, `synchronous=FULL`), same tables in spirit. No schema migration from the reference is required. |
| `src/sources/github*` | Ported: GitHub source as the GitHub App (§11): REST over `net/http` with installation tokens, polling with conditional requests. A GitHub App cannot read notifications, so the Source polls each installed repository instead of the agent account's notifications. |
| `src/sources/feishu*` | Not in v1. |
| `src/executor/dsh.ts` (DshExecutor) | Replaced by the dispatcher + node runtimes (§6, §7). |
| Main "router" Session (`dsh-superagent-router` preset) | Replaced by the router (§6.3). |
| `prompts/*.md` | Carried over and adapted (remove Harness specifics, add Athena tools). Overrides/versioning per reference `docs/004-prompts.md` kept. |
| `src/client/*` (React in Harness sidebar) | Replaced by a new standalone React PWA (§10). Panel principles from reference `docs/005-panel.md` kept: work needing a person first, then health, then history; domain terms; consistent controls. |
| `tests/*.spec.ts` | Used as the checklist for the port: each relevant spec becomes Go tests asserting the same behavior. |
| `docs/000–008` | Kept in `docs/reference/` (with MIT notice) and updated where Athena differs. |

The reference's MIT license notice is preserved in `LICENSE`/`NOTICE`.

## 5. Domain (ported, summarized)

Concepts are as defined in reference `docs/001-domain.md` through `003-execution.md`; the port must preserve their behavior. In brief:

- **Source / Channel / Thread / Trigger**: an inbound event (e.g. a GitHub comment) is a Trigger on a Thread from a Source. The dashboard is a new Source (`dashboard`), whose Threads are dashboard conversations.
- **Policy / authorization**: who may ask for what; explicit requests vs inferred candidates; Mandates. Dashboard requests from the owner are explicit and authorized.
- **Eval / Decision**: the router decides accept / ignore / attach / stop for candidates.
- **Ticket / Assignment / Input / Report**: an accepted Decision creates a Ticket with an Assignment; Inputs (messages) are delivered to its executor session; the session files Reports: `progress`, `waiting`, `ready`, `blocked`, `failed`, `dropped`, optionally with a question (an Attention) for a person.
- **Stages** (reference `003-execution.md` table): person/delivery, person/answer, person/review, person/blocked·failed, working/queued, idle, working. The "person" stages are exactly what the dashboard surfaces and what triggers push notifications.
- **Subscriptions / follow-ups**: a Ticket follows Threads (e.g. its PR); follow-ups route to the owning Ticket.
- **Outbox**: replies are queued in the same transaction as the change that caused them, sent later, idempotent by id.

New in Athena (additions to the domain, not changes):

- **Node**: id, name, OS/arch, capabilities (§7.1), status (online/offline/draining), slots, last seen.
- **Placement**: an Assignment records which node and runtime run it.
- **Approval**: a pending permission request from a session's hook (§6.2), answered by a person or by Policy.
- **Agent message**: a message from one session to another (§8.4), recorded against both Tickets.

## 6. Execution on Claude Code and Codex

### 6.1 Headless sessions

Each Assignment runs in one long-lived **worker session** on one node, in a per-Ticket working directory under the node's execution root (default `<stateDir>/work/<ticket-short-name>`), as in the reference.

**Parallel Tickets on the same repository** (owner asked): a Ticket that targets a repository never runs in the owner's checkout. The node creates a **git worktree** for it from the node's clone (`git worktree add -b athena/<ticket-id> <work-root>/<ticket-short-name> <base>`; base = the repository's default branch, freshly fetched), so any number of Tickets can work on the same repository at once, each on its own branch, and each delivers its own PR. When a Ticket ends, its worktree is removed (`git worktree remove`) after its branch is pushed or the Ticket is dropped; the branch is kept until its PR closes. Merge conflicts between parallel Tickets surface on their PRs and are resolved by the later Ticket's session (rebase), escalating to a person when a resolution needs judgment.

| | Claude Code | Codex |
| --- | --- | --- |
| Process | `claude -p --input-format stream-json --output-format stream-json --verbose` | `codex app-server` (JSON-RPC over stdio — the interface the official Codex SDK and IDE extensions use) |
| Resume | `--resume <session-id>` | thread resume via app-server |
| Mid-turn input | write a user message to stdin (stream-json input) | app-server turn/input methods |
| Interrupt | app-level interrupt message / signal | app-server interrupt |
| Model | `--model`; local via `ANTHROPIC_BASE_URL` (§9) | `model`/`model_provider` config; local via custom provider (§9) |
| Config isolation | `--settings <athena-generated.json>`, `--mcp-config <athena.json>`, `--strict-mcp-config` | `CODEX_HOME=<athena-managed dir per node>` holding `config.toml`, `hooks.json` |

Exact flags and methods must be verified against the installed CLI versions during implementation; the node program pins and reports the versions it supports. The node program owns the child processes: it parses the event stream into the protocol's session events (§8), persists the session id, and restarts/resumes on crash with a recovery note (reference behavior).

The reference's background-work rules (shell commands backgrounded after a few seconds so the worker can receive new Inputs; a 30-minute cap on post-turn background work; handoff frees the slot) are re-expressed with each tool's own mechanisms. Where a tool cannot express a rule exactly, the builder documents the deviation.

### 6.1a Multi-runtime Tickets (use both usage limits)

Owner asked to combine Claude Code and Codex in one Ticket so both accounts' usage limits are used. A Ticket's runtime may be `claude`, `codex`, or **`auto`** (default once plan 5 lands), and a Ticket's Assignments may run on different runtimes over its life. Each Assignment records its own `Runtime` and `Model`.

- **Quota-aware choice.** Each node tracks, per account/runtime, live usage state from the runtimes themselves (rate-limit/usage-limit events and errors in the stream, reset times when reported, recent token burn) and reports it with its heartbeat. For `auto`, the dispatcher picks the runtime with headroom (ties → the Ticket's previous runtime for a warm session, then the owner's preferred default). The dashboard shows both accounts' headroom and reset times.
- **Failover mid-Ticket.** When a session hits a usage limit, the node ends that Assignment cleanly and the Engine starts a new Assignment on the other runtime **in the same worktree**: work-in-progress is committed to the Ticket branch first (checkpoint), and the new session starts with a handoff note (the Ticket's Inputs, latest Reports, `git log`/`diff --stat` of the branch, and a summary of the previous session's last turn). When the first runtime's limit resets, later Assignments may move back. The Ticket's stage never shows a person stage just because a limit was hit.
- **Pairing within one Ticket** (owner-selectable per Ticket, or a Policy default): 
  - *Implement + review*: one runtime implements; before `ready`, a session on the other runtime reviews the branch diff read-only and returns findings; the implementer addresses them (bounded rounds). Spreads usage and adds an independent second opinion.
  - *Split*: the lead session uses `hand_off` (§8.4) to create child Tickets pinned to the other runtime for separable pieces; children work in their own worktrees branched from the parent's branch and the lead merges their branches back. Two sessions never write the same worktree at the same time.
- **Accounting**: spend, tokens and attention cost are tracked per Assignment and per runtime, so the Performance page (§10.3) shows how each Ticket used each account.

### 6.2 Hooks

Athena installs its own hooks in every session it starts (via the generated settings / `CODEX_HOME`, never by editing the user's global config or the repository). Every hook command is the node binary itself: `athena hook <event>`, which talks to the local node program over a Unix socket and returns within milliseconds.

Both tools support equivalent events (Claude Code: `PreToolUse`, `PermissionRequest`, `PostToolUse`, `Stop`, `SessionStart`, `UserPromptSubmit`, `Notification`, …; Codex hooks — stable since CLI v0.124 — `PreToolUse`, `PermissionRequest`, `PostToolUse`, `UserPromptSubmit`, `Stop`, …).

| Hook | Athena use |
| --- | --- |
| PreToolUse | Enforce blocked commands and Policy deterministically (reference `DSH_SUPERAGENT_BLOCKED_COMMANDS`), deny writes outside the Ticket directory, deny credential reads. Allow / deny / escalate. |
| PermissionRequest | Escalations become an **Approval** shown on the dashboard and pushed to the phone; the session waits; the owner's answer (or a Policy rule) is returned as the hook result. Timeout → deny with a reason the model sees. |
| PostToolUse | Record activity (tool, duration, outcome) for the Ticket timeline. Not used for live display. |
| Stop | Authoritative "turn ended" signal (`Engine.turnEnded`), including background-work bookkeeping. |
| SessionStart | Inject Ticket context: execution variables (reference `003-execution.md` table, renamed `ATHENA_*`), Mandate, waiting Inputs. |

Hooks are for control. Live display comes from the session's output stream. If a hook mechanism is unavailable for a runtime version, the node refuses to start that runtime and reports why (no silent fallback to uncontrolled sessions).

### 6.3 Router

The reference's main Session (routing candidates, deciding, relaying reports) becomes the **router**: a long-lived headless session (default: Claude Code on a small model, e.g. Haiku-class; configurable to a local model, §9) on the control server's local node, with only routing tools (the reference's `dsh-superagent-router` tool set, re-implemented as Athena MCP tools: `decide`, `attach`, `note`, `request_status`, `reply`, …). The router never gets work tools.

Batching: notices arriving during a router turn are delivered together at its next step (reference `notices.ts` behavior) to keep model calls down.

### 6.4 Athena tools for sessions (MCP)

Each node serves an MCP server (official Go MCP SDK, `github.com/modelcontextprotocol/go-sdk`) over stdio/Unix socket to its sessions, scoped to the session's Ticket. Worker tools:

- `report(status, text, question?)` — files a Report (replaces reference Harness tools).
- `reply(purpose, text)` — queues a message to the Ticket's Channel via the Outbox.
- `follow_thread`, `mark_unrelated` — subscriptions (reference Executor commands).
- `ask_agent(target, question)`, `hand_off(target, task)`, `wait_for(ticket|message)`, `agents()` — cross-node agent communication (§8.4).
- `local_model(prompt, model?)` — call a local model for a cheap sub-task (§9).

## 7. Multi-node

### 7.1 Roles

- **Control server** (`athena server`): Engine, store, router, Sources, Outbox, dispatcher, dashboard (static assets embedded), push notifications, device registry. Exactly one. It also runs an embedded local node so a single machine works out of the box.
- **Node** (`athena node`): connects out to the control server, advertises capabilities, runs sessions, serves MCP + hooks to them, buffers events while disconnected.

Capabilities advertised by a node: OS/arch; installed runtimes and versions (`claude`, `codex`); local inference (backend, models, VRAM free); repositories present (remote URL → local path); max concurrent sessions (slots); labels set by the owner (e.g. `mac`, `gpu`).

### 7.2 Dispatch

The dispatcher implements the Engine's `Executor` port. For each pending Assignment it chooses a node by: hard constraints (runtime installed, repository present or clonable, local model present, owner pin/labels) → prefer the node that last ran this Ticket (warm workspace, resumable session) → least loaded. It sends `StartAssignment`; the node acknowledges, then reports `assignmentStarted`, Input deliveries/reads, Reports and turn ends, mapping 1:1 to the Engine's `EngineForExecutor` callbacks.

### 7.3 Node offline / failure

- Heartbeat over the link; a node is offline after a missed interval (builder picks values; target detection < 10 s).
- A node that drops keeps running its sessions and buffers events locally (SQLite outbox); on reconnect it resumes the stream from the last acknowledged sequence (§8.2). Nothing is lost or duplicated.
- If a node stays offline past a threshold, its Tickets show a "node offline" person-stage in the dashboard with **Retry elsewhere** (§7.4). No automatic migration in v1.
- Draining: the owner can mark a node draining (no new Assignments).

### 7.4 Moving work between nodes

**Checkpoints** (designer's call): at every turn end, a node commits the Ticket's work-in-progress to a hidden ref (`refs/athena/<ticket-id>`) and pushes it to a bare mirror kept by the control server (sent over the link's `blob` channel). The owner's GitHub repositories are not cluttered, and "Retry elsewhere" on an offline node loses at most the current turn's work.

Workspace state moves by **git**: the source node (or, if it is offline, the control server's latest checkpoint) commits work-in-progress to the Ticket's branch and pushes it (to the origin remote, or directly node-to-node over the VPN using a git remote served by the source node's program); the target node fetches and continues. Session transcripts are not moved; the new session starts with a recovery note summarizing the Ticket and the old session's last Reports. Tickets with no repository move with their directory as a tar stream over the link.

## 8. Device protocol

### 8.1 Transport

- One **WebSocket** per device (node or dashboard client) to the control server, over **TLS** (§11). `github.com/coder/websocket`.
- Messages are **Protocol Buffers**, binary frames. Schemas in `proto/`, managed with `buf`; Go types via `protoc-gen-go`, TypeScript types via `protobuf-es` for the dashboard. This is the language-neutral contract that lets any component (e.g. a future Rust node) be swapped.
- Logical channels are multiplexed in one connection: `control` (commands/acks), `events` (domain + session events), `stream` (live session output), `blob` (chunked transfers).

### 8.2 Reliability

- Every durable message carries a per-direction monotonically increasing sequence number; receivers ack cumulatively; senders keep un-acked messages in a local SQLite outbox and replay from the peer's last ack on reconnect. Exactly-once effect via idempotency ids (as in the reference's Outbox).
- Live session output (`stream`) is best-effort with a bounded per-session ring buffer; on reconnect a client asks for the tail it missed (by offset). Durable transcript is kept on the node.
- Backpressure: per-channel credit; a slow phone never stalls nodes (it falls back to "skip to latest" on `stream`).

### 8.3 Dashboard sync

The control server pushes state deltas (Ticket stage changes, Reports, Approvals, node status) on `events`; each dashboard keeps a local store and applies deltas. A newly connected or long-disconnected client gets a snapshot, then deltas from a cursor. Any action taken on one screen is reflected on all others through the same deltas — there is no polling anywhere.

### 8.4 Agent-to-agent communication

Agent messages are routed through the control server (single place for authorization, recording and delivery when targets are asleep or on an offline node):

- `ask_agent(target, question)`: target is a Ticket id/name or "any agent with capability X". Delivered to the target session as an Input marked as from another agent; the target answers with `reply` to that message; the asking session receives the answer as an Input (or via `wait_for`).
- `hand_off(target, task)`: creates a child Ticket (new Decision with the parent's Mandate as an upper bound — a child can never have more authority than its parent), optionally pinned to a node/runtime; the parent can `wait_for` it.
- Messages grant nothing (as the reference's `note`): authority comes only from Mandates.
- Loop protection: max hop count and per-Ticket message rate limit; exceeding them raises an Attention.

Large artifacts between nodes go directly node-to-node (git, or `blob` stream relayed by the control server only if a direct connection is unavailable).

### 8.5 Splitting work: up front and mid-work (owner, 2026-11-04)

The owner asked for both ways of splitting a task, built "as modular and efficient as possible".

- **Mid-work split**: the lead session calls `hand_off` (§8.4) for a separable piece it discovers while working. Children are child Tickets whose Mandate is bounded by the parent's, each in its own worktree branched from the parent's branch; the lead `wait_for`s them and merges their branches back.
- **Up-front split (plan first)**: before any work starts, a **planning step** turns one request into a **breakdown**: sub-tasks, each with a title, the instructions, the files or areas it expects to touch, dependencies between sub-tasks (a DAG), and an optional runtime/node/model hint. The original Ticket becomes the **lead** (it owns the branch, merges, and delivers the result); each sub-task becomes a child Ticket started when its dependencies are done.
  - **When**: the owner ticks "Plan first" in New task, a Policy rule asks for it (e.g. for requests the router classifies as large), or the router proposes it. Small requests never pay for a planning step.
  - **Approval**: the breakdown is shown as a draft (list and graph); the owner edits, approves or rejects it. A Policy setting may auto-approve breakdowns up to N sub-tasks with no new Mandate scope. Children never get more authority than the lead.
  - **Efficiency**: planning runs on a cheap or local model with a short, fixed prompt (prompt cache reused); fan-out is capped (default 5 sub-tasks, configurable) and children count against the WIP brake (§18.1) and slots like any Ticket; sub-tasks whose expected files overlap are flagged in the draft (and as overlap warnings in the graph) so the owner can merge them before they run.
  - **Failure**: a failed child raises an Attention on the lead; the owner can retry it, drop it, or let the lead continue without it.
- **Modularity**: both modes produce the same thing — child Tickets with dependency edges — through one Engine command (`SpawnChildren(lead, []ChildSpec)`), so the dashboard, graph, dispatcher and merge logic do not care which mode created them. The planner is an interface (`planner.Strategy`: `Plan(ctx, Request) (Breakdown, error)`) with one default implementation (an LLM planning session); other strategies (e.g. a template per repository, or one sub-task per failing CI job) plug in without touching the rest.

## 9. Local model inference

- **Pluggable inference backend** (owner, 2026-11-04: "take into account the modularity in the backend we use for inference"). Nothing outside one node package knows which engine runs. Each engine is an adapter behind one Go interface, `inference.Backend`:
  - `Detect(ctx) (Info, error)`: is the engine installed or running here; version; platform facts.
  - `Start(ctx, ModelSpec) error` / `Stop(ctx) error`: for engines the node manages (a `launchd` agent on macOS, a user service on Linux); a no-op for an engine the owner runs.
  - `Endpoint() string`: the localhost base URL of its OpenAI-compatible API, the one wire format every backend must speak (chat completions; `responses` when the engine has it).
  - `Models(ctx) ([]Model, error)`, `Health(ctx) error`, `Telemetry(ctx) (Usage, error)` (GPU load, memory, loaded models).
  - `Capabilities() Caps`: native Anthropic Messages, tool calling, prefix caching, continuous batching, max context.
  
  Everything else is built on those: the authenticated proxy (§9 below) forwards to `Endpoint()`, translates Anthropic Messages ↔ chat completions only when `Caps` says the engine lacks them, and the dashboard shows `Info`/`Telemetry` without engine-specific code. A node's config names its backend (`inference.backend`, default `auto`, which picks by platform), so adding an engine — mlx-lm, llama.cpp, Ollama or a future one — is one new adapter with its contract tests, and switching is a config change. Contract tests run every adapter against the same suite (detect, serve a request, stream, tool call when supported, health, telemetry), with a fake engine for CI.
- **Engine: vLLM everywhere** (the default adapters). On a Mac (Apple silicon, the primary target) the node runs **vLLM with the `vllm-metal` plugin** (vllm-project's Apple silicon backend: MLX compute, paged attention, continuous batching, concurrent serving). On Linux with NVIDIA GPUs it runs stock vLLM (CUDA). Both expose the same OpenAI-compatible API, so the node, the proxy and the dashboard treat them as one backend. Ollama is not used. The builder verifies vllm-metal's current release on the installed macOS before building (it is young; pin a version).
- **macOS tuning** (the owner asked for the most optimized setup):
  - **Models in MLX format** (`mlx-community` 4-bit or 8-bit quantizations), sized to leave headroom in unified memory; the node refuses a model that would not fit with the session load it expects.
  - **Resident and warm**: the model loads at node start and stays loaded (no idle unload); a warm-up request runs before the node advertises it.
  - **Prefix caching on**, so the router's and chores' fixed system prompts are computed once and reused across requests (matches §12's prompt-cache reuse).
  - **Concurrency through continuous batching**: the router, chores and any local-model sessions share one engine instead of queueing whole requests.
  - **GPU memory ceiling**: `athena node` setup documents raising macOS's GPU wired-memory limit (`sysctl iogpu.wired_limit_mb`) for large models. It is an owner action needing `sudo`; Athena never changes system settings itself.
  - **No sleep while serving**: the node holds a power assertion while requests are in flight, and only then.
  - **Telemetry without root**: GPU utilization from IOKit's accelerator statistics, unified memory pressure from the VM stats, the chip and GPU core count from `sysctl`/`system_profiler`; no `powermetrics`.
  - **Service**: vLLM runs as a `launchd` user agent the node manages (start, health check, restart with backoff), bound to localhost.
- The node program detects the running engine, lists its served models, memory and GPU facts, and advertises them. A machine with neither Apple silicon nor an NVIDIA GPU serves no local models.
- The inference server binds to localhost only. Other nodes reach it **through the GPU node's Athena program** (authenticated proxy on the link), never directly.
- Uses:
  1. **Codex on a local model**: Codex custom provider (`[model_providers.athena_local]`, a non-reserved id, `base_url` pointing at the node's local proxy). The builder verifies the wire API (`responses` vs `chat`) for the installed Codex version.
  2. **Claude Code on a local model**: via `ANTHROPIC_BASE_URL` pointing at the node's proxy, which speaks the Anthropic Messages API. The builder verifies whether the installed vLLM (with vllm-metal on macOS) serves `/v1/messages` itself; if not, the node's proxy translates Messages ↔ chat completions, streaming and tool use included. **Experimental**: keep behind a feature flag until verified with tool use and streaming.
  3. **Router and chores on a local model** (classification, summaries, log digestion) — the main cost saving.
  4. `local_model` MCP tool for any session on any node.
- Dashboard: "Local · <model> on <node>" is a choice when starting a Ticket; node card shows GPU load and loaded models.

## 10. Dashboard

- **React 19 + Vite + TypeScript**, Tailwind CSS v4, shadcn/ui components, a small client store (e.g. Zustand) fed by protocol deltas. Built assets embedded in the Go binary (`embed`) and served by the control server.
- **PWA**: installable to home screen; offline shell; **Web Push** notifications (VAPID keys generated by the server) only for person-stages and Approvals.
- Same features on all sizes, with **two first-class layouts** (owner asked for a normal PC dashboard as well as the phone):
  - **Phone/tablet**: single column, bottom navigation, one-tap actions (mockup: `visual-direction.html`, option C).
  - **Desktop** (≥ 1024 px): three panes — sidebar (navigation, nodes with live load, today's stats) · list (Needs you / Tickets, filterable) · detail (Ticket header, key facts, pending question or approval inline, live session view with a composer). Keyboard-first: ⌘K command palette, `j`/`k` to move, `⏎` to open, `a` approve, `r` reply, `⌘⏎` interrupt a session. Wide screens can pin several live sessions side by side. Mockup: `desktop-layout.html`.

### 10.1 Design quality bar

The owner wants it beautiful, clean and impeccable.

**Chosen direction (owner, from mockups): "Precision".** Stark white surfaces, crisp 1 px black hairlines, small radii (≈4 px), one signal-orange accent (≈ `#FF5A1F`) used only for what needs action, IBM Plex Sans for UI with IBM Plex Mono for metadata, commands and identifiers. Engineered, Swiss-poster sharp. The dark theme inverts it (near-black surfaces, light hairlines, same orange). Reference mockup: `.superpowers/brainstorm/*/content/visual-direction.html`, option C.

Concretely:

- **A defined visual system before screens**: design tokens (color, type scale, spacing, radius, motion) on CSS variables; light and dark themes, both first-class; one accent color; restrained palette where color carries meaning (stage, health), never decoration.
- **Typography-led, calm layout**: generous whitespace, a clear hierarchy, one primary action per view, dense information (logs, timelines) made legible rather than hidden.
- **Feels native on the phone**: thumb-reachable primary actions, bottom navigation on small screens, safe-area insets, 44 px touch targets, no horizontal scroll, instant feedback on every tap (optimistic updates reconciled by protocol deltas).
- **Motion with purpose**: short transitions for state changes (stage moves, new output), respecting `prefers-reduced-motion`.
- **Live session view as a first-class surface**: readable streaming output (monospace, syntax-aware, collapsible tool calls), auto-scroll that yields when the user scrolls up.
- **Empty, loading and error states designed**, not defaulted. Skeletons over spinners.
- **Accessibility**: WCAG 2.2 AA contrast, full keyboard use on desktop, screen-reader labels.
- **Consistency**: same information looks the same everywhere (reference panel principle 7); shadcn/ui components themed to the token system, not used stock.
- **Process**: the visual direction is approved by the owner from mockups before screen implementation; key flows are checked in Playwright screenshots at phone (390 px), tablet (820 px) and desktop (1440 px) widths, light and dark.

Views (priority order, following reference `docs/005-panel.md`):

1. **Needs you**: Tickets in person-stages, pending Approvals, offline-node Tickets — each with its one-tap action (answer, approve/deny, review link, retry, retry elsewhere, stop).
2. **Tickets**: filter by stage/node/runtime/repo; Ticket detail with timeline (Triggers, Decisions, Reports, Approvals, agent messages), Placement, and a **live session view** (streamed output, tool activity) with an input box that sends an Input to the session.
3. **Graph** (owner asked to see tasks as a graph): an interactive graph of work. Nodes are Tickets (shape/colour by stage, orange = needs you, ring = running), grouped into lanes by repository (or "no repository") with an optional machine overlay. Edges: `hand_off` parent → child, `wait_for` dependencies, `ask_agent` conversations (dashed, with message count), follow-ups routed to a Ticket, and **overlap warnings** between parallel Tickets on the same repository that touch the same files (computed from their branches' changed paths). Time can be scrubbed to replay how the graph grew. Clicking a node opens the Ticket detail; on the phone the graph is pan/zoom with a list fallback. Rendered with a force/DAG layout on canvas/SVG (builder's choice of library, e.g. `@xyflow/react` or `d3-force`), fed by the same protocol deltas.
4. **New task**: free text; optional repository (from nodes' repositories), node pin, runtime, model (incl. local).
5. **Nodes**: online state, slots in use, runtimes/versions, local models and GPU load, draining toggle, enroll a new node (§11).
6. **Settings**: Policy, prompts (with the reference's override/versioning UX), execution environment, GitHub source, notification preferences.

### 10.2 Visibility over the running system

Owner asked for concrete visibility. Mockup: `system-visibility.html` (desktop "System" page; phone stacks the same tiles).

**Where the data comes from** — all collected by Athena itself, no extra services required:

| Signal | Source |
| --- | --- |
| Domain events (Triggers, Decisions, stage changes, Reports, Approvals, agent messages) | Engine `emit` (reference `DomainEvents`) |
| Session activity (turns, tool calls, durations, exit codes) | Runtime event streams (§6.1) + PostToolUse hooks |
| Guardrail decisions (allow / escalate / deny, with rule) | PreToolUse / PermissionRequest hooks |
| Tokens and spend per session, Ticket, model, node | Runtime result events (usage fields); both CLIs' native OpenTelemetry export (Claude Code `CLAUDE_CODE_ENABLE_TELEMETRY`, Codex `[otel]`) pointed at the node's built-in OTLP receiver |
| Node health (CPU, memory, GPU, disk of work root, link RTT, buffered events, CLI versions) | Node program, sent with heartbeats |
| Integrations (GitHub App token, API rate limit, vLLM, certificate expiry, store backups) | Health checks run by server/nodes on a schedule |

**Storage**: events and metrics go to the control server's SQLite (separate database file from domain state), downsampled after 7 days (1-minute → 1-hour buckets), raw events kept 30 days (configurable). Audit records (every command run by a session, every hook decision, every Approval, every GitHub write) are append-only and kept 1 year.

**Surfaces**:
- **System page**: KPI tiles with sparklines (sessions/slots, queue, needs-you and oldest age, spend today vs budget, link p95, errors), node table, spend by model (local tokens shown at $0), live event stream with filters, health checks, guardrail counts.
- **Ticket page**: full timeline plus per-Ticket cost, tool calls and durations.
- **Audit log**: searchable, filterable by Ticket/node/rule, exportable as JSONL.
- **Budgets & alerts**: daily/monthly spend budgets per model or account; push alerts for budget thresholds, node offline > N min, failing health check, error spikes — each alert type can be muted. A budget that runs out is a **hard brake**: no new Assignment starts on that model or account until the period resets or the owner raises it (§18.2); running sessions finish their turn.
- **CLI**: `athena status`, `athena top` (live sessions), `athena logs -f [--ticket T]`, `athena audit`.
- **Optional export**: Prometheus `/metrics` and OTLP forwarding for owners who want Grafana; off by default.

Structured logging (`log/slog`, JSON) with Ticket/Assignment/node ids on every line; secrets redacted at the logger.

### 10.3 Performance metrics (owner asked for these to be rich)

Measured from plan 1 onward by an in-process metrics core (`internal/metrics`: counters, gauges, and log-bucketed histograms giving p50/p95/p99; bounded label sets: runtime, model, node, tool, outcome, repo). Every metric below is sliceable by runtime, model, node and repository and comparable over time.

**Agent performance** (per Ticket, stored on the Ticket, and aggregated):

| Area | Metrics |
| --- | --- |
| Outcomes | completed / merged / dropped / failed / stopped rates; **first-try success** (no retry, no crash, no human correction); retry and crash counts |
| Speed | queue wait; time to first output; time to first Report; active time vs time waiting on a person; wall time to `ready`; time to merge |
| Human attention (the reference's core metric) | questions asked, Approvals requested, approval wait, person messages per Ticket, notifications sent — "attention cost" per Ticket |
| Cost | input/output tokens, **prompt-cache hit ratio**, $ per Ticket, $ per merged PR, $ saved by local models |
| Work | turns, tool calls by tool, tool error rate, hook denials/escalations, lines changed, CI passing on first push, PR review rounds |

**Runtime/model comparison**: a scoreboard of success rate, median time, median cost and attention cost per runtime × model × task kind (repo task / no-repo / ops), so the owner can see where Claude, Codex or a local model performs best and set defaults from evidence.

**System performance**:

| Area | Metrics |
| --- | --- |
| Engine & store | command latency, SQLite transaction latency, queue depth, effect backlog |
| Execution | dispatch latency (pending → started), session spawn time, time to `Init`, **hook decision latency**, MCP tool latency, worker slot utilization |
| Link (plan 3+) | RTT, event lag (node event → every dashboard), reconnects, buffered events, replayed messages, bytes/s per channel |
| Router | decisions per hour, decision latency, tokens per decision |
| Resources | RSS, CPU, goroutines per process; disk use of work roots; GPU utilization/VRAM (local inference) |

**Performance budgets, enforced** (the §2 efficiency targets become tests): `make bench` runs Go benchmarks and an end-to-end harness with a fake runtime and asserts: hook decision p99 < 10 ms; engine command p99 < 5 ms; dispatch latency p99 < 100 ms (excluding model time, and — for repository Tickets — excluding the external `git worktree add` process, whose cost scales with repository size; raw repository dispatch is still guarded by the regression rule); dashboard action → session input p95 < 250 ms (plan 3+); node idle RSS < 30 MB, control server < 100 MB. Results are stored as a baseline in the repo; CI fails on any budget breach or a > 10% regression against the baseline.

**Surfaces**: plan 1 ships `athena metrics` (current values and percentiles over the last 1 h / 24 h) and per-Ticket stats in `athena status <id>`; plan 9 adds the dashboard Performance page (scoreboard, latency percentiles over time, cost and attention trends) and Prometheus export of the same metrics.

## 11. Security

- **Network**: UniFi WireGuard VPN provides reachability and encryption between devices (incl. phone). Athena does not rely on it for authentication.
- **TLS everywhere** on the link and the dashboard. **(Open — owner's answer needed, see §16.1.)**
- **Device identity**: each node has its own key pair and a certificate issued by Athena's own CA at enrollment; node ↔ server uses mutual TLS. Enrollment uses a one-time, short-lived join token created in the dashboard (`athena node join <url> <token>`). Revocation from the Nodes view.
- **Dashboard login**: single owner, **passkey (WebAuthn)**; HTTP-only, Secure, SameSite=Strict session cookie; re-auth for sensitive settings.
- **Secrets**: long-lived secrets live only where used (the GitHub App private key on the control server; each node's own `claude`/`codex` logins), stored with file mode 0600 or the OS keychain. Never sent over the link, never placed in prompts, logs, URLs or the dashboard.
- **GitHub access for workers on any node** (designer's call): Athena acts on GitHub as a **GitHub App** installed on the owner's repositories, so its commits, PRs and comments are attributed to the app's bot identity, separate from the owner (the reference's agent-account split). The control server holds the App's private key and, per Assignment, mints a short-lived installation token (≤ 1 h, auto-refreshed while the Assignment runs) scoped to that Ticket's repository and the minimum permissions. The token is sent to the node over the mTLS link, held in memory only, and exposed to the session solely through a git credential helper and `gh` wrapper (`athena git-credential`, `athena gh`) that the node controls; it is revoked/dropped when the Assignment ends. Sources (reading notifications, polling) use the App on the control server.
- **Session containment**: by default worker sessions run as the owner's OS user (as the reference does, so existing `claude`/`codex` logins and checkouts work), confined by PreToolUse rules (Ticket directory, blocked commands, no credential files) plus each runtime's own sandbox mode where available. A dedicated restricted OS user per node is an opt-in hardening (reference `runAsWorker`), documented in setup. The router has no work tools.
- **Input validation**: every protocol message is schema-checked on receipt; unknown fields/versions are rejected with a clear error.
- **Supply chain**: minimal dependencies (Go standard library first); `govulncheck` and dependency pinning in CI.

## 12. Efficiency principles (how §2 goal 6 is met)

- Single Go binary per machine; no extra services (no Redis, Postgres, queue, Node runtime on nodes).
- Push only; no polling (except the GitHub source where webhooks are unavailable — the reference polls; the builder should prefer webhooks over the VPN-unreachable internet only if a relay is added later, otherwise keep polling with ETags/conditional requests).
- Binary protobuf frames, multiplexed on one connection per device; permessage compression for `stream`.
- Cheap/local model for routing; resumed sessions (prompt cache reuse); batched router notices.
- Work placed where the repository and warm session already are.
- Budgets measured: a benchmark suite checks the latency and footprint targets in §2.

## 13. Tech stack

| Layer | Choice |
| --- | --- |
| Backend | Go (current stable), one module, one binary `athena` with subcommands `server`, `node`, `hook`, `join`, `version` |
| Storage | SQLite via `modernc.org/sqlite` (pure Go; no cgo, easy cross-compile), WAL |
| HTTP / WS | `net/http` + `github.com/coder/websocket` |
| Protocol | Protocol Buffers (`buf`, `protoc-gen-go`, `protobuf-es`) |
| MCP | `github.com/modelcontextprotocol/go-sdk` |
| Workers | `claude` CLI (stream-json), `codex app-server` |
| GitHub | GitHub App over the REST API (server); workers use `gh` and git through Athena's credential helper with a short-lived, repository-scoped App token (§11) |
| Local inference | vLLM: `vllm-metal` (MLX) on Apple silicon Macs, the primary target; CUDA on Linux/NVIDIA (owner) |
| Network | UniFi WireGuard VPN |
| Dashboard | React 19, Vite, TypeScript, Tailwind v4, shadcn/ui, PWA + Web Push |
| Tests | Go `testing` (+ race detector), Vitest for the dashboard, Playwright for key dashboard flows on mobile + desktop viewports |
| Release | Cross-compiled binaries for darwin/arm64, darwin/amd64, linux/amd64, linux/arm64 |

## 14. Repository layout (proposed)

```
athena/
  cmd/athena/            main, subcommands
  internal/core/         ported Engine, domain types, stages, policy (no I/O)
  internal/store/        SQLite repositories, outbox, migrations
  internal/server/       control server: link hub, dispatcher, router host, sources, push, auth
  internal/node/         node program: runtimes, hooks socket, MCP server, local inference proxy, event buffer
  internal/runtime/claude/  Claude Code process driver
  internal/runtime/codex/   Codex app-server driver
  internal/sources/github/
  internal/link/         protocol framing, seq/ack, reconnect, channels
  proto/                 .proto files (single source of truth for the wire)
  prompts/               adapted from the reference
  web/                   React PWA
  docs/                  Athena docs; docs/reference/ = reference docs (MIT)
```

## 15. Designer's calls (owner should check)

- Port the reference's domain model faithfully rather than redesign it; Feishu deferred.
- Router = Claude Code on a small model by default.
- Control server also runs a local node.
- Agent-to-agent messages routed via the control server; artifacts node-to-node.
- Work moves between nodes via git; no automatic migration in v1.
- Protobuf over WebSocket; SQLite outbox on every device for resumable delivery.
- Claude Code-on-local behind a flag (vLLM is the owner's choice, §3).
- Passkey login; Athena's own CA + mTLS for nodes.
- GitHub App identity with short-lived, per-Ticket tokens minted by the control server.
- Workers run as the owner's OS user by default; dedicated restricted user is opt-in.
- Turn-end checkpoints to a bare mirror on the control server.
- React/Tailwind/shadcn PWA embedded in the Go binary.
- Single owner, no teams.

## 16. Open questions

### 16.1 Owner questions, resolved by default

The owner has not answered these yet. The design adopts the defaults below (designer's calls) so building isn't blocked. Each is a setting, not an architectural fork, and can change later.

1. **Approvals (default Policy)**: allowed without asking inside the Ticket directory: edit files, run tests/builds/linters, local servers, `git commit`, push the Ticket's own branch, open/update its PR, comment on its Threads. **Requires an Approval**: any write or command outside the Ticket directory; migrations, deploys or commands targeting non-local environments (matched by rules such as `--env (staging|prod)`, `deploy`, `kubectl`, `terraform apply`); system-wide package installs (`brew`, `apt`, `npm -g`, `pip` outside a venv); deleting branches or force-pushing; merging. Always denied: reading credential stores, the blocked-commands list. Rules are editable in Settings. Headless sessions run in each runtime's non-bypass permission mode so these rules actually fire.
2. **No-repo "ops" Tickets**: run in a scratch directory. Host-level actions (restart services, edit system configs, install software) are possible only through Approvals under rule 1; there is no blanket host Mandate. Only owner-originated (dashboard) Tickets may request host actions.
3. **Claude/Codex accounts**: both subscriptions and API keys are supported (whatever each node's `claude`/`codex` login uses). The dispatcher enforces a configurable **per-account concurrency limit** across all nodes, in addition to per-node slots, so shared subscription rate limits aren't exceeded; rate-limit errors back off and requeue rather than fail.
4. **TLS**: both paths are built. Setup asks: with a domain, ACME DNS-01 (via `go-acme/lego`, which supports most DNS providers) for a private hostname resolving to the control server's VPN address; without one, Athena's CA plus a one-tap install profile (iOS `.mobileconfig`, macOS/Linux instructions). The dashboard warns that iOS home-screen push needs a trusted certificate.
5. **Machines**: setup asks which machine is the control server. Design assumes it may sleep: nodes keep working and buffer (§8.2), dashboards show "control server unreachable" and reconnect automatically, and Sources catch up on wake. Recommended: an always-on desktop. Local inference uses vLLM, with vllm-metal on Apple silicon Macs as the primary target and CUDA on Linux/NVIDIA (owner, §3).
6. **VPN**: confirmed Ubiquiti **UniFi** (WireGuard VPN server / Teleport).

### 16.2 For the builder to verify

- Exact `claude` stream-json input/output schema, resume and interrupt semantics, and hook payloads for the installed version.
- `codex app-server` methods for start/resume/input/interrupt, and how its approval requests interact with Codex `PermissionRequest` hooks; hook trust requirements when hooks come from `CODEX_HOME`.
- vllm-metal's current release on the installed macOS: model coverage, tool calling, prefix caching and concurrent serving; codex custom-provider wire API (`responses` vs `chat`) against vLLM; Claude Code ↔ vLLM (Messages API or proxy translation) stability with tool use.
- Web Push on iOS PWA under the chosen certificate approach.

## 17. Left to the builder

Internal package boundaries beyond §14, SQLite schema details, timing constants (heartbeats, retries, timeouts) within the stated targets, dashboard visual design within the reference's panel principles, logging/metrics format, CI setup, and the order of implementation (suggested: core port + store → single-machine server with Claude Code runtime + dashboard → Codex runtime → multi-node link → agent-to-agent → local inference).

## 18. Additions: autonomy brakes, trust and autopilot (owner, 2026-11-04)

The owner asked to take functional inspiration from [nucleos](https://github.com/zDud4s/nucleos) (Apache-2.0). These five ideas were adopted; they are reimplemented in Go from their descriptions, and no nucleos code is copied.

### 18.1 WIP brake

Athena stops starting work it chose on its own (inferred accepts, autopilot Tickets) in a repository while too much of the owner's attention is already owed there: Tickets waiting in a person stage plus pending Approvals at or above a ceiling (default 3 per repository, a Policy setting). It clears itself as soon as the owner catches up. Explicit requests (an allowed person mentioning the App, an owner task) are never braked. A braked candidate is recorded as a Decision with reason `wip_brake` and re-evaluated when the count drops. Serves goal 6 (human attention) and the anti-spam anti-goal.

### 18.2 Kill switch and hard spend brake

- **Kill switch**: one owner action (`athena pause`, a dashboard control, re-auth required to resume) stops all new Assignments and all autonomous activity at once: running sessions are interrupted at the end of their current turn, router decisions stop, the Outbox holds replies. `athena resume` restarts everything; nothing queued is lost. This subsumes plan 2's deferred "Pause processing".
- **Hard spend brake**: §10.2 budgets fail closed, as stated there.

### 18.3 Trust class per Source

Every Trigger carries a trust class derived from its Source and sender: `owner`, `allowed` (on the Policy list), `collaborator`, `stranger`, `bot`, `ci`. The trust class of the Ticket's originating Trigger selects a **tool policy** for its worker sessions — `stranger`-originated work (only possible through an `explicit` or `inferred` unlisted cell) runs with no network beyond its Git host, no Approval-escalable commands outside the worktree, and no MCP tools that read other Tickets. Untrusted text (third-party comments, fetched web pages) reaches a worker session marked as untrusted data, and, where a local model exists (§9), is first summarised by it so instructions embedded in it never reach the working session verbatim. Policy, not the router, owns the mapping.

### 18.4 Shadow mode and Approval promotion

A repository can be in `shadow` mode (a Policy setting per repository; default `live`): Athena works Tickets there but every outward effect (push, PR, GitHub reply) is held as a proposal the owner can apply or discard from the dashboard. Separately, when the owner approves the same action class (tool + rule + repository) N times (default 5) without a denial, the dashboard offers a one-tap "always allow this here" that writes a Policy rule; the offer never applies itself.

### 18.5 Autopilot: scheduled and event-started Tickets

A `schedule` Source emits Triggers on cron-like schedules and on repository events (default branch moved, dependency manifest changed, CI red on the default branch for > N hours). Each autopilot entry has a name, instructions, a target repository and a schedule, edited in Settings. Its Triggers go through the same router, Policy and admission as any other Source (strength `inferred`, so `observe` mode only records "would accept"), and are subject to the WIP brake (§18.1) and the kill switch (§18.2).

