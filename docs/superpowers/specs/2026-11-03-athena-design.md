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
2. Tasks are not tied to one repository: a Ticket targets any repository a node has, or no repository (research, scripts, ops tasks).
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
- Code lives in `danielpina1/athena`.

Everything else in this document is the designer's call unless marked otherwise; §15 lists the main ones.

## 4. Relationship to the reference

The reference is TypeScript on DeepSeek Harness (`@deepseek-ai/cordis`, `dsh-session`, `dsh-agent`, …). With a Go backend, **its code is not reused; its design is ported.** It is the behavioral specification.

| Reference | Athena |
| --- | --- |
| `src/core/*` (Engine, tickets, stages, intake, assignments, replies, memory, evals) | Ported to Go `internal/core`, same concepts, names, invariants. Single writer of domain state; synchronous commands; one transaction per command; ports enqueue external work that adapters perform after commit (reference `docs/007-architecture.md`). |
| `src/core/ports.ts` (`Repositories`, `Assessor`, `Executor`, `Outbox`, `SourceScope`) | Same ports as Go interfaces. `Executor` is implemented by the dispatcher (§7). `Assessor` by the router (§6.3). |
| `src/app/store.ts` (node:sqlite, WAL) | Go store on SQLite (WAL, `synchronous=FULL`), same tables in spirit. No schema migration from the reference is required. |
| `src/sources/github*` | Ported: GitHub source via the `gh` CLI under the agent account, as in the reference. |
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

## 9. Local model inference

- The GPU node runs **Ollama** (default; macOS Metal or Linux CUDA). **vLLM** is an optional backend on a Linux NVIDIA node when throughput matters. The node program detects the backend, lists models and VRAM, and advertises them.
- The inference server binds to localhost only. Other nodes reach it **through the GPU node's Athena program** (authenticated proxy on the link), never directly.
- Uses:
  1. **Codex on a local model**: Codex custom provider (`[model_providers.athena_local]`, a non-reserved id, `base_url` pointing at the node's local proxy). The builder verifies the wire API (`responses` vs `chat`) for the installed Codex version.
  2. **Claude Code on a local model**: Ollama's Anthropic-compatible Messages API (Ollama ≥ 0.14) via `ANTHROPIC_BASE_URL`. **Experimental**: there are reports of Claude Code hanging against Ollama (anthropics/claude-code#51239); keep behind a feature flag until verified.
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
3. **New task**: free text; optional repository (from nodes' repositories), node pin, runtime, model (incl. local).
4. **Nodes**: online state, slots in use, runtimes/versions, local models and GPU load, draining toggle, enroll a new node (§11).
5. **Settings**: Policy, prompts (with the reference's override/versioning UX), execution environment, GitHub source, notification preferences.

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
| Integrations (GitHub App token, API rate limit, Ollama, certificate expiry, store backups) | Health checks run by server/nodes on a schedule |

**Storage**: events and metrics go to the control server's SQLite (separate database file from domain state), downsampled after 7 days (1-minute → 1-hour buckets), raw events kept 30 days (configurable). Audit records (every command run by a session, every hook decision, every Approval, every GitHub write) are append-only and kept 1 year.

**Surfaces**:
- **System page**: KPI tiles with sparklines (sessions/slots, queue, needs-you and oldest age, spend today vs budget, link p95, errors), node table, spend by model (local tokens shown at $0), live event stream with filters, health checks, guardrail counts.
- **Ticket page**: full timeline plus per-Ticket cost, tool calls and durations.
- **Audit log**: searchable, filterable by Ticket/node/rule, exportable as JSONL.
- **Budgets & alerts**: daily/monthly spend budgets per model or account; push alerts for budget thresholds, node offline > N min, failing health check, error spikes — each alert type can be muted.
- **CLI**: `athena status`, `athena top` (live sessions), `athena logs -f [--ticket T]`, `athena audit`.
- **Optional export**: Prometheus `/metrics` and OTLP forwarding for owners who want Grafana; off by default.

Structured logging (`log/slog`, JSON) with Ticket/Assignment/node ids on every line; secrets redacted at the logger.

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
| GitHub | `gh` CLI under the agent account (as reference) |
| Local inference | Ollama (default), vLLM (optional) |
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
- Ollama default for local inference; Claude Code-on-local behind a flag.
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
5. **Machines**: setup asks which machine is the control server. Design assumes it may sleep: nodes keep working and buffer (§8.2), dashboards show "control server unreachable" and reconnect automatically, and Sources catch up on wake. Recommended: an always-on desktop. Local inference supports Ollama on macOS (Metal) and Linux (CUDA) in v1; vLLM is optional on Linux/NVIDIA.
6. **VPN**: confirmed Ubiquiti **UniFi** (WireGuard VPN server / Teleport).

### 16.2 For the builder to verify

- Exact `claude` stream-json input/output schema, resume and interrupt semantics, and hook payloads for the installed version.
- `codex app-server` methods for start/resume/input/interrupt, and how its approval requests interact with Codex `PermissionRequest` hooks; hook trust requirements when hooks come from `CODEX_HOME`.
- Codex custom-provider wire API for Ollama; Claude Code ↔ Ollama stability.
- Web Push on iOS PWA under the chosen certificate approach.

## 17. Left to the builder

Internal package boundaries beyond §14, SQLite schema details, timing constants (heartbeats, retries, timeouts) within the stated targets, dashboard visual design within the reference's panel principles, logging/metrics format, CI setup, and the order of implementation (suggested: core port + store → single-machine server with Claude Code runtime + dashboard → Codex runtime → multi-node link → agent-to-agent → local inference).
