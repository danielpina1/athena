# Athena roadmap

Spec: `docs/superpowers/specs/2026-11-03-athena-design.md`

The spec covers several subsystems, so it is built as a sequence of plans. Each plan ends in working, tested software and is written just before it starts, so it can use what the previous plans actually produced.

| # | Plan | Ends with | Spec |
| --- | --- | --- | --- |
| 1 | **Foundation: single-machine core + Claude Code** (`2026-11-03-athena-p1-foundation.md`) | `athena server` on one machine: owner tasks from the CLI become Tickets worked by headless Claude Code sessions, with hooks enforcing rules, Approvals, Reports, stages, resume after crash/restart | §4, §5 (subset), §6.1–6.2, §6.4 (`report`), §11 containment |
| 2 | Router, Policy and GitHub | GitHub mentions/issues/PR comments → router decides → Tickets → PRs and replies as the GitHub App; Outbox; subscriptions and follow-ups | §5 (rest), §6.3, §11 GitHub App |
| 3 | **Link protocol and dashboard backend** (`2026-11-04-athena-p3-link-dashboard-backend.md`) | Protobuf over WebSocket with seq/ack/resume, snapshot + deltas, passkey login, TLS (ACME + own CA), Web Push | §8.1–8.3, §11 TLS/auth |
| 4 | **Dashboard UI** (`2026-11-04-athena-p4-dashboard-ui.md`) | React PWA in the Precision style, phone and desktop layouts, live session view, Approvals, new task | §10, §10.1 |
| 5 | Codex runtime and multi-runtime Tickets | `codex app-server` driver with hooks and MCP; per-Assignment runtime; `auto` runtime with quota tracking; usage-limit failover in the same worktree; implement + review pairing | §6.1, §6.1a, §6.2 (Codex) |
| 6 | Multi-node | `athena node` with enrollment, mTLS, capabilities, dispatcher, heartbeats, buffering, checkpoints, Retry elsewhere, per-Ticket GitHub tokens on nodes | §7, §11 device identity |
| 7 | Agent-to-agent | `ask_agent`, `hand_off` (incl. child Tickets pinned to the other runtime, merged back by the lead), `wait_for`, `agents` across nodes with loop protection | §8.4, §6.1a split |
| 8 | Local inference | Ollama/vLLM detection, authenticated proxy, Codex and Claude Code on local models, router/chores on local, `local_model` tool | §9 |
| 9 | Visibility and performance | System and Performance pages (runtime/model scoreboard, latency percentiles, cost and attention trends), audit log, budgets and alerts (budgets are a hard brake, §18.2), OTLP receiver, `athena top`, Prometheus export | §10.2, §10.3, §18.2 |
| 10 | Autonomy brakes and trust | WIP brake per repository, kill switch (`athena pause`/`resume` + dashboard control), trust class per Trigger → worker tool policy, untrusted-text handling (local-model summary when available), shadow mode per repository, Approval-promotion offers | §18.1–18.4 |
| 11 | Autopilot | `schedule` Source: cron and repository-event Triggers through the same router/Policy, braked by §18.1–18.2, edited in Settings | §18.5 |

Basic structured logging and `athena status` / `athena logs` arrive in plan 1; plan 9 builds the full visibility layer on the events every earlier plan already emits.

Plans 10 and 11 were added on 2026-11-04 (ideas from nucleos, spec §18). Plan numbers are names, not the build order: plan 10 is built **after plan 4** (so its dashboard controls exist) and before plan 5, because the brakes and trust classes matter as soon as Athena works on its own repository; plan 11 follows plan 10. Plan 9's hard spend brake needs the usage data plan 9 collects.
