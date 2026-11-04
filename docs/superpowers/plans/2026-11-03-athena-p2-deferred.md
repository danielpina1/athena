# Plan 2 — deferred review items

Minor findings parked during plan 2 reviews, items deferred from the final review, and every plan 2 ruling (R1–R41). Items fixed by the final fix round are marked **resolved**. Each later plan's author checks the items in its area.

## Deferred minors

- Task 1: minor (deferred): string KVSet shares kv table with schema/seq keys — reserve or prefix
- Task 1: minor (deferred): fake memTx.Enqueue discards NextSeq error
- Task 1: minor (deferred): Decisions("") returns all plan 1 decisions
- Task 1: minor (deferred): Enqueue uses time.Now directly (Task 2 may add a clock)
- Task 2: minor (deferred, FINAL MUST-FIX): TestDrainKeepsPendingRows can race (release before Drain closes stopping) → 12s timeout flake — **resolved** (final fix)
- Task 2: minor (deferred): inflight-key skip branch for unordered kinds not deterministically tested
- Task 2: minor (deferred): panic value/stack logged unredacted (effects.go:394)
- Task 2: minor (deferred): byte-identical re-enqueue during a run is merged — document or add generation
- Task 2: minor (deferred): Later floor 100ms undocumented; HeadEffect redundant var; Health "effects" key collision; Drain-timeout doc
- Task 3: minor (deferred): AssessmentOutcome mode off returns no would-accept (unreachable)
- Task 3: minor (deferred): followsATicket predicate differs in form from Engine's "stopped by a person"
- Task 3: minor (deferred): test row "bot gets no follow-up strength" proves nothing (unlisted bot)
- Task 3: minor (deferred): TestSaveDoesNotAliasCaller misnamed; no_evidence/mode_off drop said reason; evidence-less match ignores whole candidate (document)
- Task 3: minor (deferred): Basis.Row holds cell name (document); source parts limited to GitHub rows; Effects wording for bot Mandates
- Task 4: minor (deferred): human context-only Input bypasses blocked/failed hold (automatic=false) — consider holding
- Task 4: minor (deferred): readUntil uses only latest Report per Assignment (progress after ready erases ready time)
- Task 4: minor (deferred): closes missed while a Ticket is ended are never seen on resume
- Task 4: minor (deferred): Options/Wake tests in intake_test.go not engine_test.go; long comment line model.go:385; bind_thread metric untested
- Task 5: minor (deferred, FINAL TRIAGE): attach of one batch Trigger records `unavailable`/routed → would show in evals health as failure; use ignore/routed — **resolved** (final review)
- Task 5: minor (deferred): dead outdated-version branch on gate-only Eval; reason codes snake_case vs reference camelCase (pick before panel); evals.go ~1200 lines — split release logic; `record` closure shadows o.record
- Task 5: minor (deferred): CodeNotExplicit comment stale
- Task 6: minor (deferred): MarkUnrelated lacks validThreadKey; hands back multi-Thread Triggers whose primary is origin; unknown SendResult not counted as attempt; ackRefusal on bot Ack; Register after Start never runs; make check output summarised
- Task 7: minor (deferred, carry to Task 9): 422 "locked" only matched in top-level message; check errors[].message and real locked-issue response (maybe 403)
- Task 7: minor (deferred): unclassified decode/encode/NewRequest errors vs doc "exactly one"; empty 200 body decode on Get; ctx deadline → DeadlineExceeded (Task 8 should know)
- Task 7: minor (deferred): rate-limit test gaps (remaining 0 neutral msg; secondary limit); bare JWT redact pattern; \b token boundary; failed refresh fallback no backoff/log; ETag cache never evicts; LogValuer assertion location; Retry-After HTTP-date
- Task 7: minor (deferred): accountLogin holds accMu across network call; default http.Client follows same-domain redirects with Authorization
- Task 8: minor (deferred): edits adding a mention are invisible (ID has no edit version); mentions inside code spans counted; default-branch CI reads only current head; CI Actor = app slug (cosmetic); github.state.* never pruned; TestPRFromTicketBranchBound tail asserts nothing; reviews on unfollowed PRs never read
- Task 8: minor (deferred): Dropped list in memory only (folded into R19)
- Task 9: minor (deferred): empty-body review Triggers not located in window (use as positions only); Link hard-codes github.com (GHE: store html url in address); Context reads all pages per assessment; dead var line / firstMessages reuse; outbox signature change no RED
- Task 9: minor (deferred): duplicate-check since vs clock skew (pad 1 min); alreadySent doc line long
- Task 9: minor (deferred, FINAL TRIAGE): replyChannel can yield an address-less ChannelRef (Mandate Thread fallback) → parseTarget permanent error → retried forever; should be refused at Reply time or dropped — **resolved** (Outbox drops ErrPermanent as refused)
- Task 10: minor (deferred): duplicate candidate on failed CandidateDelivered; thread/<key> and trigger/<id> pages diverge from reference (no triggers page / neighbours); eval_read can't expand non-Trigger window messages (needs Source full read); title limit 80 vs 200; reply 4,000 limit relies on Engine; eval mode vs eval policy_version; owner Tickets get thread_followed/unrelated notices; pageOf breaks surrogate pairs; router token in process env (same-user threat model); fake RecentDecisions differs from SQLite; tests: notice durability across restart, fixed sleeps
- Task 10: minor (deferred, FINAL TRIAGE): unconfirmed cap 50 evicts untaken messages (loss on crash) → backpressure instead
- Task 10: minor (deferred, FINAL TRIAGE): echo matched by exact text; merged/folded mid-turn messages may never echo (no uuid) → stale duplicates; send uuid and match by it — **resolved** (uuid, text fallback; R40)
- Task 10: minor (deferred): fakeclaude echoes at read not take-in; IsError Result drops taken messages; referenced keeps looking after cap
- Task 11: minor (deferred): CredentialGh token reachable by any process with socket+ids (document at Task 12 token scopes); RED shown after the fact; node.go +125 lines (placement could move to repos.go)
- Task 11: minor (deferred): smudge filters from shared clone config run on node worktree add (no token); envProxy prefers HTTPS_PROXY over https_proxy; owner proxy/CA only in ~/.gitconfig lost on mirror; dead map never pruned; mirror HEAD not refreshed / default-branch rename
- Task 11: minor (deferred): fixed mirror config pins repositoryformatversion 0 (sha256 repos); CA keys (http.sslCAInfo) not pinned in command scope (needs network MITM); shared clone object store writable across Tickets (per-Ticket clone or separate uid)
- Task 11: minor (deferred): forged loose objects in mirror/clone store (same-uid, R29); mirrorStray lacks objects/info/alternates; HEAD-as-directory DoS
- Task 12: minor (deferred): drain often times out after host.Stop (drain before Stop / shorter wait); e2e test drives tools over socket, no direct ThreadClosed assert, pre-made worktree; TestNudgeMovesDeliveredAt weak; apiUrl not https-validated; Ticket Channels without Address still possible; render.go untouched (confirmed not needed)
- Task 12: minor (deferred, FINAL TRIAGE): waiting GitHub launch holds a node slot indefinitely (unreadable key → forever) with no per-Ticket signal — **resolved** (R34)
- Task 12: minor (deferred, FINAL TRIAGE): with no saved Policy and a failing boot check, Defaults(nil) admits nobody → owner's queued Triggers recorded ignore and never revisited; persist last installation account in kv — **resolved** (R36)
- Task 12: minor (deferred): shutdown can stall ≤30 s on in-flight check (pass workCtx); git clone/fetch network failure still fails the Ticket; unwrapped ScopedToken errors default transient; failed tell reported as error though saved
- Task 13: minor (deferred): router transcript location vs log scan unconfirmed
- Task 13: minor (deferred): orphaned liveGHSkip comment; stale "hello.txt" comment; long doc lines — **resolved**

## Deferred from the final review

- Final: parking after question/blocked/failed (reference 003 frees the slot) — plan 6 dispatcher (R37)
- Final: every Input to a parked Ticket resumes it, CI and bots included (R39)
- Final: unconfirmed cap 50 evicts untaken messages after a crash — backpressure instead (Minor 6)
- Final: `athena status` scans every dropped receipt for refused ones — plan 9 (R41)
- Final: `server_idle_rss` bench flake (19.1 MB vs 17.1 MB baseline), also on 26bed5d — plan 9 (R39)
- Final: the live GitHub test has not run; it needs the owner's App (R7)

## Rulings

- Ruling R1: Task 2 may add runner queries (claim due, delete, record failure, next due) to internal/store/effects.go — the runner owns that table's access pattern — cost if wrong: none.
- Ruling R2: Task 8 adds non-Tx `(*store.Store).KVGet/KVSet` helpers for Source cursors — cursors are Source state, not domain state — cost if wrong: trivial.
- Ruling R3: Task 5 wires "release unread human follow-ups to Eval" into every Ticket end path (dropped/ready/stop/ThreadClosed) via one helper — reference 002 "Handing back and releasing" — cost if wrong: a released message missed on one path.
- Ruling R4: Task 5 stores the accept's Ack text on the Decision (`Decision.Ack`) and queues nothing; Task 6 implements Reply and makes Accept queue the acknowledgement through it (test TestAcceptQueuesAck in T6) — keeps task order — cost if wrong: one small edit in T6.
- Ruling R5: relay and notice effects are enqueued by the Engine inside the command's Tx (human follow-up delivered to Tickets, report.filed, Attention raised, ticket ended, trigger requeued, thread unsubscribed); Task 10 adds these enqueues to internal/core and the router host only handles them — reference stores relays as durable effects; event emit is not durable — cost if wrong: core touches in T10.
- Ruling R6: Task 12's fake flow uses a new `ATHENA_FAKE_SCRIPT` env (fakeclaude may be extended), and where fakeclaude cannot call MCP tools the test drives the router/worker tool handlers through the node socket ops directly — matches existing fakeclaude conventions — cost if wrong: test realism; the live test (T13) covers the real path.
- Ruling R7: Task 13 Step 3 (live run) runs only if the owner has installed the App; otherwise ledgered as not run — owner action required — cost if wrong: real GitHub path unverified until the owner runs it.
- Ruling R8: Mandate gains `Thread string` (origin Thread key); Revalidate checks Scope.Listens(m.Thread), not m.Scope (Mandate.Scope keeps the reference meaning "the work a Mandate allows"); Task 5 accept sets Thread to the primary Thread — avoids overloading Scope — cost if wrong: one field.
- Ruling R9: carry to Task 8: Source sets Meta["receiveAll"]="true" for receive-all scopes (incl. CI) and Human=true for people (not Bot, not CI).
- Ruling R10: Observation = admitted through an unaddressed cell (ListedUnaddressed / UnlistedUnaddressed, CI included); an inferred unlisted-addressed cell is not observation — reference 008:149 "observed = receives unaddressed messages in that scope" — cost if wrong: one flag.
- Ruling R11: ⚠️ pause is deferred to plan 4 (plan "Deferred"); Task 5 accept subscribes the batch Threads (Subscriptions) but adds no Follows — only BindThread (PR Threads, Task 8/11) adds Follows, so an issue-only Ticket still ends on ready — cost if wrong: Tickets on issues wait for issue close.
- Ruling R12: the `reply` effect kind is NOT Ordered: Concurrency 1, backoff 5s→300s (outbox.Options); per-Ticket+conversation order is the Outbox's own check — plan Task 12's "reply ordered" contradicts reference "messages of other Tickets do not wait" — carry to Task 12 — cost if wrong: none.
- Ruling R13: carry to Task 9: `Channel.Send` gains the receipt id; the GitHub Channel embeds a hidden marker `<!-- athena:receipt:<id> -->` and, on a retry (attempts>0), first looks for a recent comment by the App carrying that marker and reports `sent` if found — prevents double posts after a lost response (anti-spam) — cost if wrong: one extra list call on retries.
- Ruling R14: R4 reading confirmed — accept queues the acknowledgement only for a person's primary Trigger (reference acknowledgeAccepted; bots/CI get no ack) — cost if wrong: none.
- Ruling R15: plan's receipt-id formula swallows a later round's identical result/attention text; the `trigger` slot of the id uses the round: for `result` the latest Report's assignment id + report time, for `attention` the Attention id (acknowledgement/answer keep the Trigger id) — no lost messages — cost if wrong: id format change before any data exists.
- Ruling R16: security minors enter this fix round: (M3) do()/write refuse a path not starting with "/" and any resolved host ≠ API host; NewApp rejects an unparsable APIURL and plain http for non-loopback hosts; (M5) ScopedToken verifies the repo owner matches the installation account (or mints by repository id) and omits nil permissions — a token must never go to another host or repo — cost if wrong: none.
- Ruling R17: canonical GitHub Thread keys are lower-case `owner/name` (issue/PR/branch Threads, cursor and state keys); exported `ThreadKey(repo, n)` / `BranchThreadKey(repo, branch)` for Tasks 9–12; Meta["repo"] keeps GitHub's full_name casing — GitHub names are case-insensitive — cost if wrong: none (no data yet).
- Ruling R18: replace the ruling's "Paginate further pages" with keyset paging: after a full page, re-ask page 1 with since = last updated_at (duplicates absorbed), capped rounds, page=2 fallback only if a page shares one timestamp; and poll with since = cursor − 60s overlap — avoids permanent skips — cost if wrong: a few duplicate reads.
- Ruling R19: carry to Task 9 (same package): on ErrGone drop the Thread and record state `gone` but do NOT call ThreadClosed (reference 003 ¶3 only drops; a 410 "issues disabled" must not end a Ticket's own PR); also treat a first-failure time older than 2×24h as a fresh start; persist Dropped from kv state on start — cost if wrong: a deleted followed issue no longer ends its Ticket automatically (person can stop it).
- Ruling R20: the Outbox records an attempt as started (persisted) before calling Send, so any re-run after a started send passes attempt>0; the GitHub duplicate check lists comments since the receipt's queued time (not now−1h) — closes the crash double-post window — cost if wrong: one list call per retry.
- Ruling R21: "two on either side" = two per side at 1,200 chars (reading confirmed); Trigger not found → first 5 + last 5 accepted; parseTarget checks the address matches ch.Thread (ThreadKey(repo, n) == Thread) — added to the R20 round as a cheap safety check — cost if wrong: none.
- Ruling R22: fix round also takes three minors: eval_read nil deref (server crash), recovery resend duplicating a just-sent message, metric label from unvalidated tool name — crash/duplicate/injection risks — cost if wrong: none.
- Ruling R23: accept binding the pull before filing the report (a ready report on a Ticket with no follows ends it first) — cost if wrong: none.
- Ruling R24: fix round also takes M1 (node's clone/fetch token passed through the node socket/fd, not the git process env) and M2 (Input fallback must not label a GitHub person's message "from the owner") — cross-repo token exposure and authority confusion — cost if wrong: none.
- Ruling R25: ⚠️ report.pull binding any number as own is accepted: reference 003 trusts the Executor's report of its PR ("once reported by the Executor"); the worker acts within its own Ticket — cost if wrong: a worker could end its own Ticket via an unrelated PR's close.
- Ruling R26: structural fix in the same round before re-review: the token is used only for a bare mirror `<stateDir>/mirrors/<owner>/<name>.git` that the node alone fetches (no worktree attached, hardened config); the worktrees' clone fetches from the mirror over the local file protocol with no token, so nothing a worker writes into the shared clone config reaches a token-bearing git — removes the http.* MITM path — cost if wrong: extra disk per repo.
- Ruling R27: round 2 also makes the nonce single-use (LoadAndDelete) — cheap hardening of the disclosed exposure — cost if wrong: a git that asks twice fails visibly.
- Ruling R28: round 3 fixes (a): the default branch and its SHA come from GitHub inside the token-bearing call (`ls-remote --symref` or the fetch's own output), never from mirror files; plus writeFileAtomic via os.CreateTemp (O_EXCL) — base must not be worker-steerable — cost if wrong: one extra ls-remote per launch.
- Ruling R29: (b) is accepted under the spec §11 same-uid default and documented: any same-uid process can read another's argv/env/fds, so a nonce race equals capabilities a same-uid worker already has; the boundary is the opt-in restricted OS user + Claude Code sandbox (to recommend to the owner) — cost if wrong: a prompt-injected worker could obtain a token for another repo of the same installation for ≤ 1 h.
- Ruling R30: fix round also takes M1 (policySet atomic under saveMu; notices not lost if PolicyChanged fails — retry-safe), M2 (await Source goroutines before store close), M4 (seed Identity from the startup check's bot) — silent loss / bad commits / shutdown hygiene — cost if wrong: none.
- Ruling R31: `athena github check` reading the key in the CLI process is accepted: "only on the server" means only on the control machine (same host, same user), and check must work before a server restart — cost if wrong: none.
- Ruling R32: `athena policy set` accepts either bare Settings or the `policy show` view (uses its `settings`), so the documented round trip works — owner convenience — cost if wrong: none.
- Ruling R33: fix round also takes the live-test minors that would make a correct run fail: health wait 1 min, one shared deadline within the documented timeout, cleanup deletes hello.txt (or uses a unique file name per run), Fatal when ATHENA_LIVE_GITHUB=1 but claude/gh missing — cost if wrong: none.
- Ruling R34: I1 fixed now — on a `ready` report the worker session ends and the node slot is released; the Ticket waits on its Follows with no live process; a human follow-up relay or a request-changes to a waiting Ticket queues a resume (claude --resume in the same worktree) through the normal dispatcher, with revalidation; a launch waiting for credentials does not hold a slot (Minor 5) — reference 003 hand-off frees the slot; 4 open PRs otherwise stall all work — cost if wrong: a resume path that re-launches a session on each follow-up.
- Ruling R35: I3 fixed now — config.json gains per-repo `github.receive_all` (list of repo names, default empty) wired to SourceConfig.ReceiveAll; documented — the admission table's unaddressed/ci rows must be reachable — cost if wrong: one config field.
- Ruling R36: I2/I4 and Minors 1,2,3,4,7 enter the one fix dispatch; Minor 4 chosen fix = skip EvaluateDue/Decide while no Policy saved and check not passed (no guessing an allowed list); Minor 6 left as-is after I4 (uuid echo keeps list short) — cost if wrong: none.
- Ruling R37: parking on question/blocked (fix concern 1) deferred to plan 6 (dispatcher) — R34 scoped to ready; same mechanism extends — cost if wrong: a Ticket waiting on the owner holds a slot until answered.
- Ruling R38: config key renamed `receive_all` → `receiveAll` to match config.json's camelCase (controller edit after re-review) — consistency — cost if wrong: none (no deployed configs).
- Ruling R39: every Input to a parked Ticket resumes it (concern 2), accepted as R34's named cost; the `server_idle_rss` bench flake reproduces on base 26bed5d, deferred to plan 9 (performance) — cost if wrong: extra resumes on CI/bot noise; a flaky optional bench.
- Ruling R40: NB1 fixed with uuid-first, text-fallback matching (warn on fallback) although claude 2.1.280 was verified to echo the sent uuid — defends against a future CLI change; the old test's "different uuid same text is not taken" expectation is reversed — cost if wrong: an identical-text message taken early and lost at a crash (narrow).
- Ruling R41: NB4 (`athena status` scans all dropped receipts) deferred to plan 9 (visibility); the no-Policy window skipping revalidation for owner replies accepted (owner-only, GitHub Source not registered) — cost if wrong: slow status after long history.
