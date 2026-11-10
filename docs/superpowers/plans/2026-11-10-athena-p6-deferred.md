# Plan 6: deferred items and rulings

Plan 6 was multi-node (`docs/superpowers/plans/2026-11-10-athena-p6-multi-node.md`): remote nodes with `athena node join` and `athena node run`, the fleet on the server, placement, offline handling, Retry elsewhere and the dashboard's Nodes screens. This file records what was left for later and where each item goes.

## Owner checks not run yet

- **The second-machine smoke test** in `docs/dev/smoke-test.md` ("Second machine"): join the Ryzen box, a pinned and an unpinned task, 30 seconds without its network cable, no Report lost or doubled, then drain and revoke.

## Deferred to later plans (decided here)

- **Quota per account across nodes** (plan 5's deferral) moves to **plan 9**, with the quota history. Two nodes on one account each learn its limit from their own sessions; the cost if wrong is one failed launch and a failover per node.
  - The per-account concurrency limit across nodes (§16.1 item 3) moves with it.
- **Labels** in capabilities and dispatch (§7.2 "owner pin/labels"): only the pin is built. Labels arrive when a second kind of machine needs them (plan 8's GPU node).
- **Node-to-node git** over the VPN (§7.4): every move goes through the server's mirror. A direct remote is an optimisation once repositories are large.
- **Tar moves of Dir Tickets** (§7.4): a Dir Ticket stays on its node, because its folder is the owner's folder on that machine.
- **Certificate renewal**: after 397 days a node must join again. `athena node list` shows the expiry from day 360.
- **`athena node forget`** for revoked rows, and the `node_removed` delta.
- **Editable per-node Execution settings** in the dashboard: they stay node flags, shown read-only per node.
- **Plan 7:** `ask_agent`/`hand_off` across nodes ride this plan's request calls.
- **Plan 8:** local inference capabilities in `Capabilities`.
- **Plan 9:** link metrics per node (RTT, buffered facts, replayed facts, bytes per channel).
