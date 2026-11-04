# Plan 3: deferred items and rulings

Plan 3 was the link protocol and the dashboard backend (`docs/superpowers/plans/2026-11-04-athena-p3-link-dashboard-backend.md`). This file records what its reviews found but left for later, and the decisions made while executing it. Items fixed before merge are not listed.

## Carry into plan 4 (dashboard UI)

- **Owner device checks have not been run yet (R30, R32).** The phone and laptop checks move to plan 4's first owner test: CA profile and full trust on the iPhone, enroll with the QR code, Face ID sign-in, live updates, add to the home screen, a push for an Approval.
- **Snapshot cost.** `Snapshot` loads every Ticket (`Tickets(true)`) before it bounds the result. `AnsweredApprovals` scans and sorts every answered Approval. Both hold the single SQLite connection, and an overflow resync makes every client take a snapshot at once. Fix: bound both in SQL with `ORDER BY … LIMIT` before history grows.
- **Duplicate stream items.** After a catch-up, the items of a partly-sent line can be sent again, because the line offset is shared. `StreamItem` has no per-line index, so the client must tolerate duplicates.
- **Detail trimming.** An oversized TicketDetail is trimmed to its newest timeline entries, with no "trimmed" marker. Add a proto field if the UI needs to show that the detail was trimmed.
- **Timeline gaps.** The timeline has no entries for replies or answered approvals.
- **Push edge cases.**
  - A subscription added after a stage entry does not get that entry.
  - When reading settings or subscriptions fails, the push is skipped and retried only at the next reconciliation.
  - Titles are cut to 80 runes.
  - Reconciliation matches by prefix, so a Ticket that re-enters a stage while the server is down can be under-notified.
  - Pruning covers only ended Tickets seen in the snapshot.
- **Visibility is global.** A visible laptop tab suppresses pushes to the phone, by design. Revisit if that proves annoying.
- **Revoking your own passkey** from the dashboard closes your own session before its CommandResult arrives. The UI should expect the 4002 close.
- **Error codes** are derived from message text, because `core.UserError` has no code. Only a stale policy maps to `conflict`.
- **Placeholder client** (`internal/web/assets/app.js`): untested in a real browser, and it sends Credit frames. Plan 4 replaces it.
- **Login ceremonies.** The map of open ceremonies can be flooded through unauthenticated `login/begin`; this is a DoS only, not a breach. `ReauthFinish` can answer 200 when a revoke lands mid-ceremony.

## Carry into plan 6 (nodes on the link)

- The unacked cap counts frames, not bytes: 1024 × 1 MiB in the worst case.
- `Resume` cannot detect a `PeerLastSeq` beyond anything already sent.
- Orphaned `After` timers.
- `Credit` is a uint32 and wraps after 4G items.
- On the node side, a gap drops items that were already received.

## Smaller items, any time

- **Link tests:** no test at exactly MaxFrame; the `hasUnknown` list/map branches are untested; `make check` leaves the regenerated tree in place when it fails.
- **CA:**
  - A pre-fix CA with no IPs and no excluded ranges still loads.
  - Nothing stops two first runs from creating the CA at the same time.
  - The directory is not fsynced after the rename.
- **ACME:**
  - It is not exercised against a real CA, and there is no ACME-mode server test.
  - The `ResolveAccountByKey` error logs at Info on the default slog.
  - There is a window where the key and cert can be torn.
  - The lego goroutine outlives its ctx.
- **Store:**
  - The commit hook is unguarded: a panic after commit reaches the caller.
  - The `PutPasskey` upsert overwrites `created_at`.
  - `WebSessionsOf` duplicates row parsing.
  - `IssueEnrollToken` uses `time.Now` instead of the injected clock.
- **Tests:**
  - The absolute-expiry test jumps 90 days at once, so it also passes on the idle limit.
  - There is no test for 1013 on a full queue during a snapshot.
  - Two stream tests don't fully discriminate.
  - `TestSessionLinesReachSubscribers` has a dead clause.
  - Some core tests were written without a red run.
- **Web shutdown** force-closes leftover connections after 2 s. This was unrequested but accepted.
- **Bench:**
  - `server_idle_rss` was re-baselined from 17.1 to 22.6 MB (R29).
  - The latency start points are measured from outside the server, so they are upper bounds (R31).
  - `make bench` takes about 3 minutes.

## Rulings

- Ruling R1: Task 6 adds `(*Auth).OnRevoke(func(sessionHashes []string))`; Task 8's connections register and close within 1 s — T8 requires it, T6 lacked the seam — cost if wrong: one callback.
- Ruling R2: a successful login sets `reauth_at = now` (a fresh UV assertion), so push subscribe right after sign-in needs no second prompt — cost if wrong: sensitive window opens at login (5 min).
- Ruling R3: Notifier's entry key uses the `updated_at` of the first TicketSummary delta showing the new person stage; dedupe keys stored in kv `push.sent.<key>` (pruned when the Ticket ends) — cost if wrong: a duplicate push after a restart race.
- Ruling R4: WatchTicket/UnwatchTicket/Visibility/StreamOpen/StreamClose are handled by internal/web/conn.go, not the Commander; the Commander's push actions answer `unavailable` until Task 10 wires them; the sensitivity gate is in Task 8 — keeps task order — cost if wrong: none.
- Ruling R5: Task 11's placeholder app.js hand-encodes Hello/Credit and decodes only Welcome/Snapshot/Delta ticket summaries with a minimal varint reader (no library, CSP self); covered by the owner's phone check (T12) — cost if wrong: the placeholder breaks; plan 4 replaces it anyway.
- Ruling R6: Task 6 also creates `internal/cli/qr.go` (vendored MIT QR encoder, credited) — named in task text — cost if wrong: none.
- Ruling R7: "push subscription keys never appear in a frame" means never in server→client frames, logs or the snapshot; the client→server PushSubscribe necessarily carries them — the browser hands them to us — cost if wrong: none.
- Ruling R8: Delta.seq is the hub cursor (epoch-scoped, authoritative for sync/resume of state); Frame.seq is the link-level durability counter (resend/ack) — carry to Tasks 2, 7, 8 — cost if wrong: confusion only.
- Ruling R9: credit is per-connection and non-durable (Grant sends a non-durable Credit frame; the caller re-grants after Hello/resume); fix the doc comment — credit is flow control for one connection, replaying it across resumes double-counts or loses it — cost if wrong: a re-grant per reconnect.
- Ruling R10: seq stamped at write time (not in Send) accepted — stamping in Send lets prioritised CONTROL overtake lower-seq STREAM frames that the receiver then drops — cost if wrong: none.
- Ruling R11: Task 2 fix round also takes Minor 1 (cap in-flight unacked durable frames → ErrBackpressure; Review Focus 3 bounded memory), Minor 2 (failed resume marks session closed), Minor 4 (refuse BLOB in Send) — cheap, bounded-memory and contract — cost if wrong: none.
- Ruling R12: an existing CA whose constraints don't match the configured host/IPs is an error naming the difference and the fix ("the CA was made for X; move <dir> away to create a new one, then reinstall the profile on your devices"); no silent reuse, no auto-recreate — recreating silently breaks every installed profile — cost if wrong: owner must recreate after a VPN IP change.
- Ruling R13: ACME account registration must not auto-accept the CA's terms implicitly: ACMEConfig gains `AgreeTOS bool` (config `acme.agreeTos`, set by `athena web setup --acme-agree-tos`, Task 11); without it Obtain fails with a health message naming the flag and the CA's terms URL — accepting terms is the owner's act — cost if wrong: one extra flag at setup.
- Ruling R14: the lego root `challenge` package (provider interface type only) is allowed in addition to the listed ones — the constraint exists to keep the all-providers factory out; `challenge` adds no provider code — cost if wrong: none.
- Ruling R15: carry to Task 8 — the dispatcher namespaces idempotency ids as `<client_id>:<command_id>` for TaskOptions.RequestID and person input ids (RequestID is globally unique) and calls PruneCommandResults(now-24h) periodically; carry to Task 7 — hub treats Changes as ids and re-reads current state (no assumption that hook order = commit order); the hub's hook is a non-blocking send — cost if wrong: cross-client id collision.
- Ruling R16: carry to Task 8 — a control-socket op `web.revoke` calls Auth.RevokePasskey in the server; `athena web revoke` uses it when the server runs and falls back to the store otherwise; the connection also re-validates its session against the store before each sensitive command — revocation must reach open sockets — cost if wrong: one op.
- Ruling R17: migration 4 amended in place (FK cascade) accepted — no v4 database has left this branch — cost if wrong: a local dev DB from 440523e lacks the FK (delete ~/.athena dev state).
- Ruling R18: carry to Task 8 (server adapter) — RecentThreads = the Threads of the most recent 200 Triggers (by Trigger.At, deduped, newest first) plus ActiveThreads; AnsweredApprovals/RecentTriggers get store queries ordered by their timestamps — Threads have no recency column — cost if wrong: a quiet thread drops from the snapshot.
- Ruling R19: ticket_detail deltas carry seq 0 and live outside the ring (only to watchers); Task 8 ignores seq 0 for gap detection and sends Detail() when a client starts watching — accepted — cost if wrong: none.
- Ruling R20: Task 8 also builds the server's hub.Source adapter (internal/server/hubsource.go) with the R18 store queries, because its end-to-end tests need a real hub; Task 11 only wires it into Run — cost if wrong: file placement.
- Ruling R21: dashboard link sessions are per connection (Hello.last_seq ignored): server→client state recovers via the hub cursor, client→server actions via idempotent command ids (client resends commands without a result) — spec §8.2's guarantees hold through those two mechanisms; link-level replay matters for nodes (plan 6) — cost if wrong: plan 4 client must resend unanswered commands (documented in docs/dev/dashboard.md, Task 11).
- Ruling R22: the snapshot must always fit one frame: before sending, if its encoded size exceeds MaxFrame minus the link's stamp slack, drop the oldest ended Tickets, answered Approvals, Decisions and Triggers (halving those lists) until it fits, and log the trim — a >1 MiB snapshot otherwise causes a reconnect loop — cost if wrong: less history in a huge snapshot.
- Ruling R23: an open connection re-validates its session every heartbeat (15 s) and closes with 4002 on failure; an open connection counts as "seen" (touch SeenAt at most every 10 min) so a phone left open doesn't idle-expire, while the 90-day absolute limit still applies — expiry must reach watch-only sockets — cost if wrong: one store read per connection per 15 s.
- Ruling R24: "Stream credit starts at 256 items" means each connection starts with an implicit STREAM credit of 256 items the client need not grant; client grants add to it — Global Constraint text — cost if wrong: none.
- Ruling R25: node.Line.GapTo accepted (gap marker as a Line) — needed to carry node-side drops — cost if wrong: none.
- Ruling R26: a starved stream keeps only a position (brief text): drop pending rendered items, record skipFrom; on the next credit send StreamGap + the latest tail (≤200 items) taken from the node's ring — Review Focus 3 CPU/memory — cost if wrong: none.
- Ruling R27: carry to Task 11 — set VAPID.Subject to the configured origin `https://<host>[:port]` (plan: sub = https://<host>); the endpoint-host fallback must not be used in production wiring — cost if wrong: push services may reject/flag the JWT.
- Ruling R28: drop the `pushContact` config field — R27 makes the VAPID subject the origin, an unused setting misleads — cost if wrong: none.
- Ruling R29: accept re-baselining server_idle_rss (17.1→22.6 MB) — plan 3 links lego, webauthn and the web stack into the same binary; the 100 MB absolute budget is the spec's brake and is met with 4× headroom — cost if wrong: a real 5 MB leak hides in the baseline until plan 9's metrics.
- Ruling R30: phone check (Task 12 step 3) not run — needs the owner's iPhone; steps go to the owner in the final message — cost if wrong: placeholder JS/push path unverified on a real device.
- Ruling R31: latency start points measured from outside the server (control request / Reply send) are accepted as upper bounds — conservative against the 250 ms budget, no production hook needed — cost if wrong: budgets slightly pessimistic.
- Ruling R32: the owner defers all hands-on testing (laptop count check and phone check) to plan 4's real dashboard; plan 3 ships on automated tests + bench, and plan 4's first owner check covers the phone steps — cost if wrong: a device-only issue (CA profile, iOS push) surfaces in plan 4 instead.
- Ruling R33: fix Important 1 with a new migration 5 (push_subscriptions.session_hash FK → web_sessions ON DELETE CASCADE), not by amending migration 4 — the owner's ~/.athena DB is already at v4 from today's laptop run — cost if wrong: one extra migration.
- Ruling R34: fix all 6 final-review minors in the one fix dispatch (each is small and in plan 3 code); carry Snapshot SQL bounds, duplicate stream items and frame-count cap into plan 4/6 deferred file — cost if wrong: a slightly larger fix diff.
- Ruling R35: controller fixes the two fitDetail residuals inline (trim a new message, round the halving up; doc Conn.Write as concurrency-safe) instead of another dispatch — 15-line fix with a test, the one-fix-dispatch budget is spent — cost if wrong: none, make check green.
