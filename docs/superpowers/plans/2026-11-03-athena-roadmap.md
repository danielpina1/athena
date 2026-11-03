# Athena roadmap

Spec: `docs/superpowers/specs/2026-11-03-athena-design.md`

The spec covers several subsystems, so it is built as a sequence of plans. Each plan ends in working, tested software and is written just before it starts, so it can use what the previous plans actually produced.

| # | Plan | Ends with | Spec |
| --- | --- | --- | --- |
| 1 | **Foundation: single-machine core + Claude Code** (`2026-11-03-athena-p1-foundation.md`) | `athena server` on one machine: owner tasks from the CLI become Tickets worked by headless Claude Code sessions, with hooks enforcing rules, Approvals, Reports, stages, resume after crash/restart | §4, §5 (subset), §6.1–6.2, §6.4 (`report`), §11 containment |
| 2 | Router, Policy and GitHub | GitHub mentions/issues/PR comments → router decides → Tickets → PRs and replies as the GitHub App; Outbox; subscriptions and follow-ups | §5 (rest), §6.3, §11 GitHub App |
| 3 | Link protocol and dashboard backend | Protobuf over WebSocket with seq/ack/resume, snapshot + deltas, passkey login, TLS (ACME + own CA), Web Push | §8.1–8.3, §11 TLS/auth |
| 4 | Dashboard UI | React PWA in the Precision style, phone and desktop layouts, live session view, Approvals, new task | §10, §10.1 |
| 5 | Codex runtime | `codex app-server` driver with hooks and MCP; runtime choice per Ticket | §6.1–6.2 (Codex) |
| 6 | Multi-node | `athena node` with enrollment, mTLS, capabilities, dispatcher, heartbeats, buffering, checkpoints, Retry elsewhere, per-Ticket GitHub tokens on nodes | §7, §11 device identity |
| 7 | Agent-to-agent | `ask_agent`, `hand_off`, `wait_for`, `agents` across nodes with loop protection | §8.4 |
| 8 | Local inference | Ollama/vLLM detection, authenticated proxy, Codex and Claude Code on local models, router/chores on local, `local_model` tool | §9 |
| 9 | Visibility | System page, audit log, budgets and alerts, OTLP receiver, `athena top`, Prometheus export | §10.2 |

Basic structured logging and `athena status` / `athena logs` arrive in plan 1; plan 9 builds the full visibility layer on the events every earlier plan already emits.
