# Plan 4: deferred items and rulings

Plan 4 was the dashboard UI (`docs/superpowers/plans/2026-11-04-athena-p4-dashboard-ui.md`). It merged as danielpina1/athena#6. Its review follow-ups merged as #8. The owner-requested redesign is #9: readable sessions, a story timeline with a Report card, a live activity feed, motion and an in-app document viewer. This file records what the reviews left for later. Items fixed before merge are not listed.

## Owner checks not run yet

- **The 7-step iPhone check** in `docs/dev/dashboard.md`: enroll, Face ID, Home Screen, push, a lock-screen approval, two screens in sync, and Airplane mode mid-approve. It carries plan 3's R30 and R32.

## Carry into plan 5 (Codex runtime)

- The runtime picker in New task (Codex, `auto`), runtime badges with real values, and the Codex session item kinds in the session view. The session view's tool presenters (`web/src/features/session/tools.ts`) are keyed on Claude Code tool names. Codex items need their own presenters.

## Carry into later plans

- **Auto-resume after a server restart.**
  - A restart, a crash or the terminal closing cuts off running sessions. On start they are parked idle until the next input, and the timeline can't say "Interrupted by a server restart" because core records no restart.
  - The server should also treat SIGHUP as a graceful stop.
  - This is owner-requested and was drafted into plan 10.
- **Plan 6:**
  - offline-node Tickets in Needs you, more nodes, real machine lanes, node enrollment and draining, and Retry elsewhere;
  - diffs and files for Tickets on remote nodes;
  - editable Execution settings, which become per node.
- **Plan 7:** agent messages in the timeline, the `hand_off`/`wait_for`/`ask_agent` graph edges, and same-file overlap warnings.
- **Plan 8:** the "Local · <model> on <node>" choice in New task, the node card's GPU and loaded models, and local-model summaries of untrusted text (§18.3).
- **Plan 9:** the System and Performance pages, the audit log, budgets and alerts, the hard spend brake (§18.2), and spend in the sidebar's Today.
- **Plan 10:** the kill switch control (it subsumes "Pause processing"), the WIP brake indicators, trust classes in Sources and shadow mode.
- **Plan 11:** the schedule Source editor in Settings.

## Known gaps, decided to leave

- **Document viewer:**
  - A Ticket's files vanish from the Files tab once its worktree is cleaned up. Reading committed files from the branch would fix this.
  - Uncommitted edits to tracked files aren't listed, because detecting them would run worker-controlled git filters. A safe follow-up: hash tracked documents in Go and compare with `git ls-tree`.
- **Old transcripts:** echoes written between the redesign's first round and its quoting fix (an unreleased window) can still render a forged "You" block in the owner's own view.
- **Session view:**
  - User-message echo labels rely on the node's exact header formats; an unknown header falls back to a muted line.
  - Map keys in tool input detail are not redacted.
- **Live feed:** a repeated event on the newest row is counted (×N), which hides distinct approvals behind one row.
- **Timeline:** the question pulse sits on the newest report row, so nothing pulses if trimming removed every report. The Report card still shows the question.
- **Dashboard:** more than 64 passkeys blocks enrollment (Chromium's exclude-list limit). The phone header could be more compact.
- **Tests:**
  - `TestEndingTicketExpiresApprovals` and the pwa push e2e are possible rare flakes.
  - The `engine_command` bench check is disk-bound and fails on this machine on the base branch too.
- **Still deferred from plan 3:** the push edge cases, the login ceremony flood, error codes taken from message text, and global visibility suppression (by design).

## Rulings

R1–R64 are in the execution ledger. Rulings that change behaviour:
- **R50:** owner Triggers record Reached; the web says "Sent to T-n" / "Sent by you".
- **R53:** the phone check is the owner's, after the final review.
- **R58:** the owner's redesign overrides Task 12's "mono 13 px": prose is sans 14 px, code stays mono.
- **R59:** green and amber tokens were added to Precision for success and warning.
- **R62:** untrusted text is quoted at the source so it can't forge input headers.
