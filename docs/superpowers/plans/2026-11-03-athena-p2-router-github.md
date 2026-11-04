# Athena Plan 2: Router, Policy and GitHub Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** GitHub issues, pull requests and comments on the owner's repositories become Triggers. The Policy gate and a router session decide accept / attach / stop / ignore. Accepted Tickets are worked in a worktree of the repository and delivered as a pull request and replies made as the GitHub App. Follow-ups reach the owning Ticket, and Tickets end when their pull requests close.

**Architecture:** `internal/core` gains the reference's remaining parts as separate files: Intake, Evals, Threads/Subscriptions and Replies. The Engine stays the only writer, with one transaction per command. New external work (replies, candidate delivery, relays to the router) goes through a durable effects queue that is enqueued in the same transaction. `internal/policy` is pure: it holds the gate, admission, Policy version, revalidation and assessment outcome. `internal/sources/github` covers the GitHub App: JWT, installation tokens, a polling Source with ETags, a Channel and a thread-context reader, all on the standard library's HTTP client. `internal/router` hosts a long-lived headless Claude Code session that has no built-in tools and only Athena's router MCP tools. Workers on GitHub Tickets reach GitHub through `athena git-credential` and an `athena gh` shim, which serve a short-lived, repository-scoped token held by the node.

**Tech Stack:** Go 1.27, `modernc.org/sqlite`, `github.com/modelcontextprotocol/go-sdk`, `net/http`, `crypto/rsa` (RS256 JWT, no JWT library), Claude Code CLI 2.1.x, `gh` CLI (workers only), GitHub REST API v3 (`X-GitHub-Api-Version: 2022-11-28`).

**Spec:** `docs/superpowers/specs/2026-11-03-athena-design.md`. Read §2 goals 1, 2 and 6, §5, §6.3, §6.4, §11 and §16.1. Behavioural reference: `docs/reference/001-domain.md`, `002-authorization.md`, `003-execution.md` and `007-architecture.md`, which are binding for domain behaviour. The reference prompts are read with `gh api repos/deepseek-ai/dsh-superagent-prototype/contents/prompts/<file> --jq .content | base64 -d`. Plan 1 deferred items are in `docs/superpowers/plans/2026-11-03-athena-p1-deferred.md`; R33 and R34 belong to this plan (Task 12).

## Global Constraints

- Every plan 1 Global Constraint still holds. `make check` passes at the end of every task.
- Third-party Go modules stay `modernc.org/sqlite` and `github.com/modelcontextprotocol/go-sdk`. GitHub is reached over `net/http`; there is no go-github and no JWT library.
- GitHub requests send `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28` and `User-Agent: athena/<version>`.
- Domain names follow the reference exactly: Source, Trigger, Thread, Correlation, Channel, Policy, Policy version, Eval, Gate, Assessment, Decision (`accept|stop|ignore|unavailable`), Mandate, Strength (`explicit|inferred`), Subscription, Disposition, Attention. Accept mode values are `off|observe|auto`, and `observe` is the default.
- Thread keys are `<source>:<key>`. GitHub uses `github:<owner>/<name>#<number>` for an issue or PR and `github:<owner>/<name>@<default-branch>` for default-branch CI.
- Trigger IDs are stable and come from the Source's own ids, for example `github:issuecomment:<id>`, `github:issue:<id>`, `github:review:<id>` and `github:checksuite:<id>`. Storing the same ID twice returns `duplicate`.
- Execution variables use the `ATHENA_` prefix: `ATHENA_GITHUB_REPOSITORY`, `ATHENA_GITHUB_ACCOUNT`, `ATHENA_GITHUB_COMMIT_EMAIL`, `ATHENA_GITHUB_NUMBER` and `ATHENA_GITHUB_PR_HEAD`. None of them holds a credential.
- Secrets: the App private key is read only on the server, from a 0600 file. Installation tokens are held in memory only and never written to SQLite, logs, prompts, Inputs, transcripts or a worker's environment. `redact` patterns cover `ghs_`, `ghu_`, `gho_`, `ghp_` and `github_pat_` tokens and the `Authorization` header.
- Anti-spam: the App's own comments never become Triggers, and a reply is sent only by the rule its purpose names (acknowledgement, answer, result or attention). A Ticket sends at most one acknowledgement per request.
- Polling uses conditional requests (`If-None-Match`, `since`). The default interval is 30 s per repository. It backs off on `403/429` with a rate-limit reset or a `Retry-After`. No call to a model is ever made from a polling loop.
- Routing runs on the router model from config (default `haiku`). The router session is started with `--tools ""` and only the Athena router MCP server.

## Review Focus

1. **The same GitHub event is seen twice** (overlapping polls, a restart between fetch and cursor save, or an ETag miss): it creates one Trigger and at most one Ticket. The second `Receive` answers `duplicate`. (Tasks 4, 8)
2. **The App's own comment, or another bot's comment, on a Ticket's issue:** the App's comment never becomes a Trigger. Another bot's comment fans out to subscribed Tickets as context but never resumes an ended Ticket and never reaches Eval unless the bot is listed. No reply loop is possible. (Tasks 4, 8)
3. **Installation token leakage:** the token never appears in `athena logs`, a transcript, an Input, a Report, the worker's environment or the SQLite file. A `Bash` call to `athena git-credential` or `gh auth token` is denied. (Tasks 7, 11)
4. **GitHub fails mid-poll** (5xx, a 404 on a private repository, a rate limit or a timeout): the cursor does not advance past unprocessed items, and the next successful poll delivers the missed comments exactly once. The Source reports an error after three failed polls in a row. (Task 8)
5. **A person comments on an ended Ticket's merged PR:** the Ticket resumes under its Mandate with no Eval, after revalidation. The same comment from CI or a bot does not resume it. A person-stopped Ticket receives nothing. (Tasks 4, 6)

---

## File Structure

```
internal/core/model.go          + Thread, Subscription, Evaluation, Disposition, MandateBasis, Follow, Reason; Trigger/Decision/Mandate extended
internal/core/ports.go          + Policy, SourceScope ports; Tx gains thread/effect/receipt/kv methods
internal/core/intake.go         Receive, BindThread, ThreadClosed, fan-out, follow-up ownership, resume
internal/core/evals.go          EvaluateDue, Candidate, CandidateDelivered, Decide, Attach, Handback, Reevaluate, Dismiss, PolicyChanged
internal/core/threads.go        FollowForContext, MarkUnrelated, Unsubscribe, Note, RequestStatus, followsDone, release on end
internal/core/replies.go        Reply(request): purpose rules, receipts, reply effect
internal/store/migrate.go       migration 3: threads, effects, receipts, settings
internal/store/threads.go       thread/eval/decision/trigger queries
internal/store/effects.go       effects rows (kind,key) + receipts
internal/effects/effects.go     runner: Handle, Later, backoff, ordering, health
internal/policy/policy.go       settings schema, Gate, Revalidate, AdmitsInferred, Version, AssessmentOutcome, Effects
internal/policy/settings.go     revisioned settings blocks in the store
internal/sources/registry.go    Source/Channel/ThreadContext interfaces, registry implementing core.SourceScope
internal/sources/github/app.go      JWT, installation tokens, scoped tokens, token cache
internal/sources/github/client.go   REST client: ETag cache, pagination, rate limit, error classes
internal/sources/github/source.go   poller → Triggers, Threads, closes, CI, PR binding
internal/sources/github/channel.go  comments, review replies, gone detection, links
internal/sources/github/context.go  thread window for assessments
internal/outbox/outbox.go       reply effect handler: ordering per Ticket+conversation, retry, drop
internal/router/host.go         router session, assessment/relay/notice handlers, batching
internal/router/tools.go        router MCP tool handlers → Engine (route, eval, eval_decide, read, reply, note, request_status, ticket_retry, trigger_requeue, thread_unsubscribe)
internal/node/repos.go          per-repository clone under <stateDir>/repos, fetch, worktree base
internal/node/credentials.go    per-Assignment token holder, git-credential + gh ops
internal/node/credcmd.go        `athena git-credential`, `athena gh`
internal/node/mcpcmd.go         + worker tools ack, follow_thread, mark_unrelated, handback; report gains pull
prompts/router.md, prompts/admission.md, prompts/replies.md, prompts/subscriptions.md   adapted from the reference
prompts/worker.md               + GitHub delivery, follow-up handling
internal/server/server.go       wiring: sources, router, effects, outbox, credentials
internal/cli/cli.go             + github setup/check, policy show/set, triggers, threads, decisions, reevaluate, dismiss, unsubscribe, task --repo
docs/dev/github-app.md          creating and installing the GitHub App
```

---

### Task 1: Domain types and store migration 3

**Files:**
- Modify: `internal/core/model.go`, `internal/core/ports.go`, `internal/core/fake_test.go`, `internal/store/store.go`, `internal/store/tickets.go`, `internal/store/repos.go`
- Create: `internal/store/threads.go`, `internal/store/effects.go`
- Test: `internal/store/threads_test.go`, `internal/store/effects_test.go`

**Interfaces:**
- Produces (in core, JSON documents with camelCase tags, ported from reference `model.ts`):
  - `Trigger{ID, Source, Kind (message|review|ci|closed), Actor, Human, Bot bool, Addressed ("mention"|"direct"|""), Scope (the receiving-scope row: issues|pulls|comments|ci|owner), Threads []string (Threads[0] = primary), ReplyTo, At, Text, Meta map[string]string, Channels []ChannelRef, Disposition Disposition}`
  - `Disposition{Filtered string, Reached []string, Attached []string, Requeued *time.Time, HandedBack map[string]string, Acknowledged []string}`
  - `Thread{Key, Source, Label, Subscriptions []Subscription, Queued []string, Eval *Evaluation, Expired int, Decisions []string, Closed *Close, Meta map[string]string}`; `Subscription{TicketID, Context, Bound bool, Own bool}`; `Close{At, Outcome}`
  - `Evaluation{ID, Thread, Primary string, TriggerIDs []string, Strength string, Basis MandateBasis, PolicyVersion string, StartedAt time.Time, DeliveredAt *time.Time}`
  - `MandateBasis{Via (addressed|followup|admission|bot|owner), Source, Scope, Row, Sender string, Rules []string, Observation bool}`, and `Mandate` gains `Basis MandateBasis`
  - `Decision` gains `Thread string, Reasons []Reason, WouldAccept, IgnoredFromCard bool, Dismissed *time.Time`; `Reason{Code string, Values map[string]string, TriggerID string}`
  - `Ticket` gains `Follows []Follow`, `Repo string` (`owner/name`, or empty); `Follow{Thread string, Own bool, Closed *Close}`
- Produces (`core.Tx` additions): `GetTrigger(id)`, `GetThread(key)`, `PutThread(*Thread)`, `ActiveThreads()` (threads with queued Triggers or a running Eval), `GetDecision(id)`, `Decisions(thread string, limit int)`, `Enqueue(kind, key string, payload []byte) error`, `KVGet(key) (string, bool, error)`, `KVSet(key, value string) error`
- Produces (store): `effects(kind, key, payload, seq, due_at, attempts, last_error, PRIMARY KEY(kind,key))`. Enqueuing a waiting key replaces its payload and makes it due now, keeping its `seq`. Also `receipts(id PK, ticket_id, conversation, state, attempts, doc)` and `settings(block, revision, doc, PRIMARY KEY(block, revision))`.

- [ ] **Step 1: Write failing tests.** `TestThreadRoundTrip` (put/get a thread with an Eval and Subscriptions; `ActiveThreads` lists only threads with queued Triggers or an Eval). `TestTriggerDisposition` round trip. `TestEnqueueReplacesWaitingKey` (enqueue `reply/k` twice: one row, second payload, same seq, due ≤ now). `TestEnqueueRollsBack` (enqueue inside a Tx that returns an error, then no row). `TestMigration3FromV2` (a v2 database opens and upgrades; plan 1 tickets are still readable).
- [ ] **Step 2: Run** `go test ./internal/store` → FAIL.
- [ ] **Step 3: Implement** the types, migration 3 and the Tx methods, plus `fake_test.go` implementations of the new Tx methods. Plan 1 JSON documents must decode unchanged, so new fields are omitempty.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(core,store): threads, evaluations, effects and receipts`.

### Task 2: Durable effects runner

**Files:**
- Create: `internal/effects/effects.go`
- Test: `internal/effects/effects_test.go`

**Interfaces:**
- Consumes: the store's effects table (Task 1).
- Produces: `type Runner`; `New(st *store.Store, log *slog.Logger) *Runner`; `Handle(kind string, h func(ctx context.Context, key string, payload []byte) error, o Options)`. `Options{Concurrency int; Ordered bool; Backoff Backoff{First, Max time.Duration}}`. `var ErrLater` wraps a delay: `Later(d time.Duration) error` reschedules without counting a failure. Also `Start(ctx)`, `Wake()` (called after an Engine commit that enqueued), `Drain(timeout)`, and `Health() map[string]error`, which reports a kind once one of its effects has failed three times in a row.

Reference behaviour (`007-architecture.md` Effects): rows run oldest first by `seq`. A handler returning nil deletes the row. An error retries with exponential backoff (attempts persisted). With `Ordered`, rows of the kind run one at a time in `seq` order, and a failing row holds back the later ones. Payload replacement makes the row due at once.

- [ ] **Step 1: Write failing tests.** `TestRunsAfterWake`. `TestRetryBackoffDoubles` (an injected clock gives due times 2s, 4s, 8s capped at Max). `TestLaterDoesNotCountFailure`. `TestOrderedHoldsBackLater` (row 1 fails, so row 2 does not run until row 1 succeeds). `TestHealthAfterThreeFailures`. `TestSurvivesRestart` (enqueue, stop the runner, open a new runner on the same store, and it runs). `TestDrainKeepsPendingRows`.
- [ ] **Step 2: Run** `go test -race ./internal/effects` → FAIL.
- [ ] **Step 3: Implement.** One goroutine per kind is enough. The next due time comes from `MIN(due_at)`, using a timer plus `Wake`, with no polling loop faster than the next due time.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(effects): durable effects runner`.

### Task 3: Policy

**Files:**
- Create: `internal/policy/policy.go`, `internal/policy/settings.go`
- Modify: `internal/core/ports.go` (the `Policy` and `SourceScope` ports)
- Test: `internal/policy/policy_test.go`, `internal/policy/settings_test.go`

**Interfaces:**
- Produces (core ports):
  - `type Policy interface { Version() string; EvalTimeout() time.Duration; Gate(t Trigger, th Thread, tickets []Ticket) Gate; Revalidate(m Mandate) bool; AdmitsInferred(rules []string) bool; Assess(a Assessment, strength string) (kind string, reasons []Reason, wouldAccept bool, err error) }`. `internal/policy` imports core, never the reverse.
  - `Assessment{Rules []RuleJudgement{Name string; Matches bool; Evidence string}; Accept bool; Reason string}`, defined in core
  - `Gate{Strength string /* explicit|inferred|"" */, Basis MandateBasis, Reason Reason}`
  - `type SourceScope interface { Listens(thread string) bool }`
- Produces (policy):
  - `Settings{Mode string; Rules []Rule{Name, Instructions string; Enabled bool}; Sources map[string]SourcePart}`
  - `SourcePart{Allowed []string; Bots []string; Admission map[string]Row}`, where the row keys are `issues|pulls|comments|ci` and `Row{ListedUnaddressed, UnlistedAddressed, UnlistedUnaddressed string}`
  - `New(s Settings, scope core.SourceScope, evalTimeout time.Duration) core.Policy`
  - `Version(s Settings) string`: the first 12 hex characters of the sha256 of the canonical JSON of the mode, enabled rules and source parts
  - `AssessmentOutcome(a core.Assessment, s Settings, strength string) (kind string, reasons []core.Reason, wouldAccept bool, err error)`, which is what `Policy.Assess` returns
  - `Effects(tickets []core.Ticket, old, cur Settings) map[string]string`: Ticket id to the notice text
  - `Store.Current() (Settings, int, error)`, `Store.Save(s Settings, baseRevision int, origin string) (int, error)` (validates; a stale `baseRevision` is refused) and `Store.History(limit)`

Defaults: mode `observe`, no rules, and the GitHub source part with `Allowed` = the installation account's login (filled in by Task 12). Admission cells default to the reference's: listed-unaddressed `inferred`; unlisted-addressed `ignore`; unlisted-unaddressed `ignore`. Gate rules are copied from reference `002-authorization.md` "Strength" and "Admission", including that an explicit unlisted-addressed cell outranks follow-up strength, that unaddressed cells apply only where the Source receives every message (`Trigger.Meta["receiveAll"]=="true"`), that CI has only the last cell, and that bots use the bot list. Revalidation is copied from "Revalidation before resume".

- [ ] **Step 1: Write failing tests.** One table test per gate case in the reference's Strength table (at least 12 rows: listed+mention → explicit/addressed; listed follow-up → explicit/followup; unlisted follow-up → inferred; unlisted follow-up in mode off → not admitted; unlisted+mention with cell explicit → explicit/admission; listed bot+mention → inferred/bot; unlisted bot → none; CI with cell inferred → inferred/admission; ...). `TestRevalidate` covers each basis. `TestAssessmentRequiresEveryEnabledRule`. `TestObserveRecordsWouldAccept`. `TestAutoAcceptsWithMatchingRule`. `TestVersionStableUnderKeyOrder`. `TestEffectsRuleChanged`, `TestEffectsRequesterRemoved` and `TestEffectsModeLeftAuto`. `TestSaveStaleRevisionRefused`.
- [ ] **Step 2: Run** `go test ./internal/policy` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(policy): gate, admission, revalidation and assessment outcome`.

### Task 4: Engine Intake

**Files:**
- Create: `internal/core/intake.go`
- Modify: `internal/core/engine.go` (`NewEngine` gains an `Options` struct carrying `Policy func() Policy`, `Scope SourceScope` and `Wake func()`; the plan 1 constructor stays as a wrapper), `internal/core/engine_test.go`
- Test: `internal/core/intake_test.go`

**Interfaces:**
- Produces:
  - `(*Engine).Receive(t Trigger) (string, error)`, returning `"stored"|"duplicate"|"filtered"`
  - `(*Engine).BindThread(ticketID, thread string, own bool) error`
  - `(*Engine).ThreadClosed(thread string, outcome TicketOutcome, at time.Time) error`
  - `func (t *Ticket) FollowsDone() (TicketOutcome, bool)`
  - events `trigger.received`, `thread.subscribed` and `ticket.resumed`
- Behaviour (reference `007-architecture.md` Engine table, `001-domain.md` Subscription):
  - A command that enqueued an effect calls `Options.Wake` after commit.
  - Receive stores the Trigger once and records it on every Thread in `Threads`, creating Threads as needed. It is `filtered` when `Scope.Listens(primary)` is false.
  - It fans out an `InputTrigger` Input to every live Ticket subscribed to any of its Threads (`Human` copied).
  - A person's message on a Ticket's own (non-context) Thread stays that Ticket's: no Eval; ownership is recorded in `Disposition.Reached`.
  - An ended Ticket that a person did not stop, reached by a person's message on a non-context Thread, resumes at once after `Policy.Revalidate(Mandate)`. A new Assignment starts with the waiting Inputs. When revalidation fails, the message is treated as reaching no Ticket.
  - Everything else is queued on the primary Thread for Eval.
  - Bot and CI Triggers never resume an ended Ticket. A person-stopped Ticket receives nothing.
  - `ThreadClosed` records the close on each open Ticket that follows the Thread and ends it once `FollowsDone`, with the outcome of the latest close and subject to the reference's "unless" conditions.

- [ ] **Step 1: Write failing tests.** `TestReceiveDuplicate` (Review Focus 1). `TestReceiveFilteredWhenNotListening`. `TestFanOutToSubscribedTickets`. `TestPersonFollowupOwnedNoEval`. `TestBotMessageFansOutButQueuesForEval`. `TestPersonResumesEndedTicket` and `TestBotDoesNotResumeEndedTicket` (Review Focus 5, with `TestStoppedTicketReceivesNothing`). `TestResumeRefusedWhenRevalidationFails` (the message goes to Eval). `TestThreadClosedEndsWhenAllOwnPullsClosed` (two own PRs: one merged → still open; second closed → ends `closed`). `TestCloseWaitsForUnfinishedResumedWork`.
- [ ] **Step 2: Run** `go test ./internal/core -run 'Receive|FanOut|Followup|Resume|ThreadClosed|Close'` → FAIL.
- [ ] **Step 3: Implement** using the existing `op` and effects pattern. `WaitsForPerson` drops its TODO and ignores context Inputs.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(core): intake, fan-out, follow-up ownership and pull-request closes`.

### Task 5: Engine Evals and Decisions

**Files:**
- Create: `internal/core/evals.go`
- Test: `internal/core/evals_test.go`

**Interfaces:**
- Produces:
  - `(*Engine).EvaluateDue() error`
  - `(*Engine).Candidate(triggerID string) (CandidateView, error)`, with `CandidateView{Eval Evaluation; Triggers []Trigger; Thread Thread; Tickets []Ticket; HandedBack map[string]string}`
  - `(*Engine).CandidateDelivered(evalID string) error`
  - `(*Engine).Decide(d DecisionInput) (DecisionResult, error)`, with `DecisionInput{TriggerID, Kind (accept|ignore|stop), Reason, Title, Ack, TicketID (for stop); Assessment *Assessment; IgnoredFromCard bool}` and `DecisionResult{Decision Decision; Ticket *Ticket; Refused string}`
  - `(*Engine).Attach(ticketID, triggerID, note string) error`
  - `(*Engine).Handback(ticketID, triggerID, reason string) (queued bool, err error)`
  - `(*Engine).Reevaluate(triggerID string) error` and `(*Engine).Dismiss(decisionID string) error`
  - `(*Engine).PolicyChanged(version string, notices map[string]string) error` (the caller computes `notices` with `policy.Effects`, as reference `policyChanged(from, to, effects)`)
  - events `eval.started`, `decision.recorded`, `ticket.created` and `trigger.requeued`
  - effect kind `assessment`, key = the eval id
- Behaviour (reference `002-authorization.md` "Routing candidates", "When Eval does not finish" and "Handing back and releasing follow-ups"; `007-architecture.md` Evals):
  - `EvaluateDue` starts one Eval per Thread whose queued Triggers wait, batching every queued Trigger. It runs the Gate on each and picks the strongest as `primary`. A batch with nothing admitted records `ignore` with the gate's reasons, without the router. Otherwise it enqueues `assessment`.
  - An Eval delivered more than `EvalTimeout` ago is requeued. The third consecutive expiry records `unavailable`. An undelivered Eval never expires.
  - Accept creates the Ticket, its Mandate (strength, basis, Policy version, matched rules) and first Assignment. It subscribes the Ticket to every Thread of the batch (the primary one owned), sets `Repo` from the primary Thread's Source meta (`Meta["repo"]`), and queues the acknowledgement through Replies (Task 6) when `Ack` is set.
  - A direct accept of a non-explicit Trigger is refused. An inferred accept needs an `Assessment`, judged by `Policy.Assess`. In observe mode it becomes `ignore` with `WouldAccept`.
  - While `followupOwners` own the Trigger, only `stop` is allowed.
  - A Decision made under an outdated Policy version records `unavailable` and requeues the batch.
  - A Ticket's end releases its unread, unacknowledged and not-handed-back human follow-ups to Eval (wire this into the existing end paths).
  - `PolicyChanged` gives each Ticket in `notices` an `InputNote` with its text, and requeues running Evals.

- [ ] **Step 1: Write failing tests.** `TestBatchOnePerThread` (three Triggers on one Thread → one Eval, then a fourth arriving meanwhile queues for the next). `TestNotAdmittedIgnoredWithoutRouter`. `TestAcceptCreatesTicketMandateSubscriptions`. `TestAcceptTwiceOneTicket` (Review Focus 1). `TestDirectAcceptOfInferredRefused`. `TestObserveModeWouldAccept`. `TestExpiryRequeuesThenUnavailable`. `TestUndeliveredNeverExpires`. `TestOwnedFollowupOnlyStop`. `TestHandbackQueuesOnceNoOtherOwner`. `TestEndReleasesUnreadFollowups`. `TestPolicyChangedNotifiesAffectedOnly`. `TestOutdatedVersionUnavailable`.
- [ ] **Step 2: Run** `go test ./internal/core -run 'Batch|Admitted|Accept|Observe|Expiry|Undelivered|Owned|Handback|Release|PolicyChanged|Outdated'` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(core): evals, decisions, attach and hand-back`.

### Task 6: Threads, Replies and the Outbox

**Files:**
- Create: `internal/core/threads.go`, `internal/core/replies.go`, `internal/outbox/outbox.go`, `internal/sources/registry.go`
- Test: `internal/core/threads_test.go`, `internal/core/replies_test.go`, `internal/outbox/outbox_test.go`

**Interfaces:**
- Produces (core):
  - `FollowForContext(ticketID, thread string) error`
  - `MarkUnrelated(ticketID, thread, reason string) error`
  - `Unsubscribe(thread string) error`
  - `Note(ticketID, text string) error`
  - `RequestStatus(ticketID string) (asked bool, err error)`, which delivers an `InputText` status request to a running, idle Assignment
  - `Reply(r ReplyRequest) (receiptID string, err error)`, with `ReplyRequest{TicketID, TriggerID, Purpose (acknowledgement|answer|result|attention), Text string}`
- Produces (core events):
  - Replies enqueue effect kind `reply`, key = receipt id: `sha256(ticket|purpose|trigger|text)[:16]`, so the same reply twice queues once.
  - The receipt's `conversation` is the Channel's Thread.
  - An acknowledgement records `Disposition.Acknowledged`.
- Produces (sources):
  - `type Channel interface { Send(ctx context.Context, ch core.ChannelRef, text string) (SendResult, error) }`, with `SendResult` one of `sent|gone`; `Link(ch) string`
  - `type Source interface { Name() string; Listens(thread string) bool; Run(ctx context.Context) error; Health() error }`
  - `type ThreadContext interface { Context(ctx context.Context, thread, triggerID string) (ContextWindow, error) }`
  - `Registry` implements `core.SourceScope` and routes by the `source` prefix of a Thread key.
- Produces (outbox): `Handler(reg *sources.Registry, st *store.Store) func(ctx, key, payload) error`.
  - Messages of one Ticket to one conversation go in queue order: a message waits (`effects.Later(5s)`) while an earlier one has not gone out and has failed fewer than three times.
  - `gone` drops the message, keeps the receipt with `state=dropped` and its reason, and it is never requeued.
  - A message whose Source no longer listens to the conversation is dropped. One whose Source is not running waits.
  - Backoff is 5 s doubling to 300 s.
- Purpose rules (reference prompts `replies.md` and `007` Replies):
  - `acknowledgement`: at most once per Trigger per Ticket, ≤ 100 words.
  - `answer`: only to a Trigger that reached the Ticket.
  - `result`: once per Report `ready`/`dropped` round.
  - `attention`: only while the Ticket has an Attention.
  - Anything else is refused with a `UserError`.

- [ ] **Step 1: Write failing tests.** `TestMarkUnrelatedHandsBackOwned`. `TestCannotUnrelateOriginThread`. `TestFollowForContextNoResume`. `TestUnsubscribeDropsAll`. `TestReplyIdempotent`. `TestSecondAckRefused`. `TestAttentionOnlyWithAttention`. `TestOutboxOrderPerConversation` (the ack fails twice, so the result waits, then both go out in order). `TestOutboxGoneDropsNotRetried`. `TestOutboxOtherTicketNotBlocked`.
- [ ] **Step 2: Run** `go test ./internal/core ./internal/outbox` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(core,outbox): subscriptions, replies and ordered delivery`.

### Task 7: GitHub App client

**Files:**
- Create: `internal/sources/github/app.go`, `internal/sources/github/client.go`
- Modify: `internal/redact/redact.go` (GitHub token patterns)
- Test: `internal/sources/github/app_test.go`, `internal/sources/github/client_test.go` (all against `httptest.Server`)

**Interfaces:**
- Produces:
  - `AppConfig{AppID int64; Slug string; KeyFile string; InstallationID int64; APIURL string /* default https://api.github.com */}`
  - `NewApp(cfg AppConfig, hc *http.Client, now func() time.Time) (*App, error)`, which refuses a key file whose mode is wider than 0600
  - `(*App).JWT() (string, error)`: RS256, `iat` = now−60 s, `exp` = now+9 min, `iss` = AppID
  - `(*App).InstallationToken(ctx) (Token, error)`: cached, refreshed 5 min before expiry
  - `(*App).ScopedToken(ctx, repo string, perms map[string]string) (Token, error)`: `POST /app/installations/{id}/access_tokens` with `repositories:[name]` and `permissions`
  - `Token{Value string; ExpiresAt time.Time}`, whose `String()` returns `"ghs_…redacted"`
  - `(*App).Bot(ctx) (login string, id int64, err error)`: `<slug>[bot]` and its user id, from `GET /users/<slug>[bot]`
  - `(*App).Repos(ctx) ([]Repo, error)`
  - `Client.Get(ctx, path string, out any) (notModified bool, err error)`, with ETag kept per path, and also `Post`, `Patch` and `Paginate(ctx, path, fn func(json.RawMessage) error)`
  - error classes `ErrTransient` (5xx, 408, timeouts, 404 on a read: reference "a 404 is retried"), `ErrRateLimited{Reset time.Time}` (403/429 with `x-ratelimit-remaining: 0` or `Retry-After`), `ErrGone` (410, or 404/422 on a write to a locked or deleted target) and `ErrPermanent`
- Worker token permissions: `contents: write`, `pull_requests: write`, `issues: write`, `checks: read`, `actions: read`, `metadata: read`.

- [ ] **Step 1: Write failing tests.** `TestJWTVerifiesWithPublicKey` (parse and verify with `rsa.VerifyPKCS1v15`; claims as above). `TestInstallationTokenCachedUntilNearExpiry`. `TestScopedTokenRequestBody`. `TestKeyFileModeRefused`. `TestETagNotModified` (second Get sends If-None-Match; a 304 returns `notModified`). `TestRateLimitedReset`. `TestErrorClasses` (table). `TestTokenNeverFormatted` (`fmt.Sprintf("%v %+v", tok, tok)` and slog JSON of a struct containing it show no `ghs_` secret; `redact.String("token ghs_abc…")` masks it).
- [ ] **Step 2: Run** `go test ./internal/sources/github ./internal/redact` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(github): app authentication and REST client`.

### Task 8: GitHub Source

**Files:**
- Create: `internal/sources/github/source.go`
- Test: `internal/sources/github/source_test.go` (a fake GitHub serving recorded-shape JSON in `testdata/github/*.json`)

**Interfaces:**
- Consumes: `Client` (Task 7), `core.Engine.Receive/BindThread/ThreadClosed` through an interface `Intake`, and a cursor store (`KVGet/KVSet` via the store).
- Produces: `NewSource(cfg SourceConfig, app *App, in Intake, kv KV, log *slog.Logger) *Source`, which implements `sources.Source` with name `github`. `SourceConfig{Repos []string; Interval time.Duration; ReceiveAll map[string]bool /* per scope row */}`.
- Per repository, each poll does:
  1. `GET /repos/{r}/issues?state=all&sort=updated&direction=asc&since=<cursor>` (issues and PRs). New issues become `Kind=message, Scope=issues|pulls` Triggers with ID `github:issue:<id>`. Opened, closed and merged PRs are found here.
  2. `GET /repos/{r}/issues/comments?sort=updated&direction=asc&since=<cursor>` gives `Scope=comments`, ID `github:issuecomment:<id>`, and `ReplyTo` from quote-free context.
  3. `GET /repos/{r}/pulls/comments?since=` gives review comments, ID `github:reviewcomment:<id>`. Their Channel address holds `comment_id` so replies thread under it.
  4. For PRs that a live Ticket follows, `GET /repos/{r}/pulls/{n}/reviews` gives reviews, including empty-body approvals. ID `github:review:<id>`.
  5. Check suites for the default branch head, and for the head of each followed PR, give `Kind=ci, Human=false`, ID `github:checksuite:<id>:<conclusion>`.
- Translation rules:
  - The Actor is `{Source:"github", ID: login}`. `Bot` is true for `type=="Bot"` or a login ending in `[bot]`. The App's own login is dropped before `Receive` (Review Focus 2).
  - `Addressed="mention"` when the body contains `@<slug>` as a whole word, case-insensitively.
  - `Threads[0]` is `github:<repo>#<n>`. `Meta` holds `repo`, `number`, `kind` (issue|pull), `head` (for a PR), `url` and `receiveAll`.
  - Channels hold `{Source:"github", Thread, Address{repo, number, comment_id?}}`.
  - A PR whose head ref is `athena/<ticket-id>` gets `BindThread(ticketID, prThread, own=true)` the first time it is seen, and again when it is reopened.
  - A closed or merged PR or issue calls `ThreadClosed` with `merged|closed`.
- Cursors are per repository and per endpoint, stored in kv as `github.cursor.<repo>.<endpoint>`. A cursor advances only to the `updated_at` of the last item whose `Receive` succeeded. Because `since` is inclusive, the first item seen again comes back as `duplicate` and does no harm.
- Failure handling: a failing repository read keeps its error and does not move the cursor. Three failed polls in a row set `Health()` to an error, and a success resets it. `ErrRateLimited` sleeps until `Reset`.

- [ ] **Step 1: Write failing tests.**
  - `TestNewIssueMentionBecomesTrigger` (fields as above).
  - `TestOwnCommentDropped` (Review Focus 2).
  - `TestOtherBotMarkedBot`.
  - `TestRestartBetweenFetchAndCursorNoDuplicateTicket` (Review Focus 1: the fake Intake counts `stored` once).
  - `TestFailedPollDoesNotAdvanceCursor` (the fake returns 500 for comments on poll 1 and serves them on poll 2: each comment is received exactly once, Review Focus 4).
  - `TestHealthAfterThreeFailures`.
  - `TestPRFromTicketBranchBound`.
  - `TestMergedPRCallsThreadClosed`.
  - `TestReviewApprovalWithoutBody`.
  - `TestCIOnFollowedPRNotHuman`.
  - `TestRateLimitWaitsForReset`.
  - `TestNotModifiedCostsNothing` (a 304 calls no Intake).
- [ ] **Step 2: Run** `go test ./internal/sources/github -run Source` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(github): polling source`.

### Task 9: GitHub Channel and thread context

**Files:**
- Create: `internal/sources/github/channel.go`, `internal/sources/github/context.go`
- Test: `internal/sources/github/channel_test.go`, `internal/sources/github/context_test.go`

**Interfaces:**
- Produces: `(*Source).Send(ctx, ch, text) (sources.SendResult, error)`.
  - With `comment_id`, it calls `POST /repos/{r}/pulls/{n}/comments/{id}/replies`. If that reply target is gone, it falls back to `POST /repos/{r}/issues/{n}/comments`. Otherwise it posts the issue comment directly.
  - It maps `ErrGone` and `422` "locked" to `gone`, and other errors stay errors for retry.
  - Also `Link(ch) string` (the html URL) and `(*Source).Context(ctx, thread, triggerID) (sources.ContextWindow, error)`.
- `ContextWindow{Messages []Msg{ID, From, At, Text string; Chars int}; Stubs []Stub{ID, From, Preview string}}`, following reference `007` "Result shapes and assessment context": the first five messages plus five on either side of the Trigger. Text is cut to 400 characters (1,200 for the two either side), with `Chars` when cut. Other messages become 50-character stubs. The issue or PR body is message 0.

- [ ] **Step 1: Write failing tests.** `TestSendIssueComment` (body and path). `TestReplyToReviewComment`. `TestDeletedReviewCommentFallsBack`. `TestLockedIsGone`. `TestServerErrorRetriable`. `TestContextWindowShape` (30 comments, Trigger at 20 → 5 first + 11 around, truncation and stubs as specified).
- [ ] **Step 2: Run** `go test ./internal/sources/github -run 'Send|Reply|Locked|Context|Retriable|Deleted'` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(github): channel and assessment context`.

### Task 10: Router

**Files:**
- Create: `internal/router/host.go`, `internal/router/tools.go`, `prompts/router.md`, `prompts/admission.md`, `prompts/replies.md`, `prompts/subscriptions.md`
- Modify: `prompts/prompts.go`, `internal/node/socket.go` (a `router` role: a session-less op set validated by a per-start random token instead of ticket ids), `internal/node/mcpcmd.go` (`athena mcp --role router`), `internal/node/rules.go` (`RouterRules`: deny every tool not named `mcp__athena__*`), `internal/runtime/claude/session.go` (an `Options.Tools *string` that passes `--tools`)
- Test: `internal/router/host_test.go`, `internal/router/tools_test.go` (fakeclaude), `internal/node/rules_test.go`

**Interfaces:**
- Consumes: Engine (Tasks 4–6), `effects.Runner`, `claude.Session`, `sources.Registry` (thread context).
- Produces: `router.New(cfg Config, eng *core.Engine, fx *effects.Runner, reg *sources.Registry, log *slog.Logger) *Host`, with `Config{Bin, StateDir, Model, Socket string}`, plus `Start(ctx)` and `Stop()`. Handlers for effect kinds:
  - `assessment`, ordered, concurrency 1: renders the candidate card, sends it into the router session, then calls `CandidateDelivered`.
  - `relay`, ordered: `follow_up`, `worker_report` and `worker_attention`, from Engine events `trigger.received` (human, delivered to Tickets), `report.filed` and Attention.
  - `notice`, ordered: `ticket_ended`, `trigger_requeued` and `thread_unsubscribed`. Notices wait and are prepended to the next relay or assessment message, so they never wake the router alone.
- Router MCP tools follow reference `007` "Main Session tools", under Athena names:
  - `route{decisions:[1..20]{trigger_id, kind: accept|ignore|stop|attach, ticket_id?, title?, ack?, reason, ignored_from_card?}}`. Each item runs as its own Engine command, and the results keep item order: `{trigger_id, kind, decision?, ticket?, refused?}`.
  - `eval{trigger_id}`: the candidate, its context window, up to 8 related Tickets (open first), `tickets_total`, the Policy version, and the enabled rules.
  - `eval_read{trigger_id, ids[]}`.
  - `eval_decide{trigger_id, rules:[{name, matches, evidence}], accept, reason, title?, ack?}`.
  - `read{resource}`, with resources `tickets`, `ticket/<id>`, `thread/<key>`, `trigger/<id>` and `decisions`, paged as in reference `007` Read resources (4,000 characters per text page, 20 items per list by default).
  - `reply{ticket_id, trigger_id?, purpose, text}`, `note{ticket_id, text}`, `request_status{ticket_id}`, `ticket_retry{ticket_id}`, `trigger_requeue{trigger_id}` and `thread_unsubscribe{thread}`.
- The session runs in `<stateDir>/router` with `--tools ""`, `--model <cfg.Model>`, the router settings and hooks, and its session id stored in kv `router.session` and resumed on restart. One message is sent per handled effect. Claude Code queues mid-turn stdin messages, so batching is delivery at the next step (spec §6.3). A crashed router restarts with resume (the plan 1 crash policy).

- [ ] **Step 1: Port the prompts.** Fetch the reference's `orchestrator.md`, `admission.md`, `replies.md`, `subscriptions.md` and `shared.md`. Adapt them by replacing the Harness and Feishu specifics, renaming the tools to the Athena names above, and keeping one sentence per line with the MIT header comment.
- [ ] **Step 2: Write failing tests.**
  - `TestRouterRulesDenyWorkTools` (Bash, Write, Read, WebFetch and `mcp__other__x` are denied; `mcp__athena__route` is allowed).
  - `TestRouterStartsWithNoTools` (the fakeclaude argv contains `--tools` followed by an empty value, and `--strict-mcp-config`).
  - `TestAssessmentDeliveredThenDeadline` (a handled effect calls `CandidateDelivered`).
  - `TestNoticesRideNextMessage` (two notices then one relay → one stdin message containing all three, in order).
  - `TestRouteBatchIndependentItems` (item 2 refused; items 1 and 3 applied).
  - `TestEvalDecideObserveMode`.
  - `TestRouterResumesSession`.
  - `TestRouterToolTokenRequired` (a socket op without the start token is refused).
- [ ] **Step 3: Run** `go test ./internal/router ./internal/node -run Router` → FAIL.
- [ ] **Step 4: Implement.**
- [ ] **Step 5: Run** `make check` → PASS.
- [ ] **Step 6: Commit** `feat(router): router session, tools and prompts`.

### Task 11: GitHub work on nodes

**Files:**
- Create: `internal/node/repos.go`, `internal/node/credentials.go`, `internal/node/credcmd.go`
- Modify: `internal/node/node.go` (placement for `Ticket.Repo`, execution variables, environment for credentials), `internal/node/mcpcmd.go`, `internal/node/socket.go`, `internal/node/rules.go`, `prompts/worker.md`, `cmd/athena/main.go` (`git-credential`, `gh`)
- Test: `internal/node/repos_test.go`, `internal/node/credentials_test.go`, `internal/node/node_test.go`, `internal/node/rules_test.go`

**Interfaces:**
- Consumes: a `Credentials` port given to the node by the server: `type Credentials interface { Token(ctx context.Context, repo string) (string, time.Time, error); Identity() (login, email string) }`.
- Produces:
  - `EnsureClone(ctx, stateDir, repo string, tok string) (dir string, defaultBranch string, err error)`: a bare-free clone at `<stateDir>/repos/<owner>/<name>`, fetched before each new worktree. The token goes in only through the credential helper, never in the remote URL.
  - Worktrees branch from `origin/<default>`.
  - `athena git-credential get` answers `username=x-access-token` and `password=<token>` for `host=github.com`, only for the session's own repository (`path` when `credential.useHttpPath`). It asks the node socket with the session's ticket and assignment ids.
  - `athena gh <args>` executes the real `gh` with `GH_TOKEN`, `GH_HOST=github.com`, `GH_CONFIG_DIR=<TicketDir>/.athena/gh` and `GH_PROMPT_DISABLED=1`. It refuses `auth` subcommands.
  - Worker sessions get these:
    - `PATH` prefixed with `<stateDir>/shims` (a `gh` shim that runs `athena gh`)
    - `GIT_CONFIG_COUNT/KEY/VALUE` for `credential.helper=!<bin> git-credential` and `credential.useHttpPath=true`
    - `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME` and `GIT_COMMITTER_EMAIL` set to the App bot
    - `ATHENA_GITHUB_*` execution variables
  - The node holds the token per Assignment in memory, refreshes it 5 min before expiry, and drops it when the Assignment stops.
  - Blocked commands gain `gh auth`, `athena git-credential` and `athena gh auth`, and a `Bash` reference to `ATHENA_STATE`/`GH_TOKEN` is denied (Review Focus 3).
  - Worker MCP tools follow reference `007` "Executor tools":
    - `ack{trigger_id, text}` calls `Reply(acknowledgement)`.
    - `reply{purpose, text, trigger_id?}`.
    - `follow_thread{thread}`.
    - `mark_unrelated{thread, reason}`.
    - `handback{trigger_id, reason}`.
    - `report` gains `pull?: number`, which binds `github:<repo>#<n>` own.
  - Inputs that carry a Trigger render as a card: `from`, `at`, the Thread label, the URL, and the text up to 600 characters with a read pointer when cut.
  - `prompts/worker.md` gains reference `work.md` and `subscriptions.md` delivery rules: branch `athena/<ticket-id>`, push, open the PR with `gh pr create`, report `ready` with `pull`, and handle follow-ups or hand them back.

- [ ] **Step 1: Write failing tests.**
  - `TestCloneUsesHelperNotURL` (`git remote get-url origin` shows no token; a local bare repository stands in for GitHub through `url.<file>.insteadOf`).
  - `TestWorktreeFromDefaultBranch`.
  - `TestCredentialOnlyOwnRepo`.
  - `TestCredentialRefusedForOtherSession`.
  - `TestGhRefusesAuth`.
  - `TestWorkerEnvHasNoToken` (Review Focus 3: a fakeclaude env dump contains no token value, and the transcript and logs contain none).
  - `TestTokenDroppedOnStop`.
  - `TestBlockedCredentialCommands`.
  - `TestReportPullBindsThread`.
  - `TestTriggerInputCard`.
- [ ] **Step 2: Run** `go test ./internal/node -run 'Clone|Worktree|Credential|Gh|WorkerEnv|Token|BlockedCredential|ReportPull|TriggerInput'` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(node): github worktrees, scoped credentials and worker tools`.

### Task 12: Server wiring, CLI and recovery

**Files:**
- Modify: `internal/server/server.go`, `internal/server/control.go`, `internal/server/render.go`, `internal/cli/cli.go`, `cmd/athena/main.go`, `internal/core/engine.go`, `internal/node/node.go`
- Create: `internal/server/config.go`
- Test: `internal/server/server_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Produces:
  - `<stateDir>/config.json` (0600): `{"github": {"appId", "slug", "keyFile", "installationId", "repos": [], "intervalSeconds": 30}, "router": {"model": "haiku"}, "evalTimeoutSeconds": 600}`.
  - CLI commands:
    - `athena github setup --app-id N --slug S --key FILE --installation N --repo owner/name...` writes the config.
    - `athena github check` mints a token, prints the bot login and the reachable repositories, and exits 1 with the reason on failure.
    - `athena policy show`.
    - `athena policy set FILE` validates and saves with an `origin=cli` revision, then calls `PolicyChanged`.
    - `athena triggers [--thread K]`, `athena threads`, `athena decisions [--thread K]`, `athena reevaluate TRIGGER`, `athena dismiss DECISION` and `athena unsubscribe THREAD`.
    - `athena task --repo owner/name TEXT` creates an owner Ticket on that repository (direct accept, `Basis.Via="owner"`).
  - `athena status` shows Source health, effects health and the router state.
  - The server builds the store, the Policy store (seeding the default with `Allowed=[installation account login]` on first start), the Registry, the GitHub Source, the effects runner (`reply` ordered with 5 s→300 s, `assessment` with concurrency 4 and 2 s→60 s, `relay` and `notice` ordered with 2 s→60 s), the outbox, the router, credentials, the node and the Engine with `Wake` = `fx.Wake`.
  - A ticker every 2 s calls `EvaluateDue` only when `ActiveThreads` is non-empty; this is not a model call.
  - Startup: pending effects resume, Evals delivered before the restart keep their deadline, and undelivered ones are re-enqueued.
  - Plan 1 deferred R33: a nudge goes through `InputsDelivered` so `DeliveredAt` moves.
  - R34: a Ticket whose latest Report is `failed` or `blocked` is not interrupted at recovery.
  - Metrics: `trigger_intake_seconds` (Receive), `eval_seconds` (start to Decision), `reply_send_seconds`, `github_requests_total{status}` and `github_rate_remaining`.

- [ ] **Step 1: Write failing tests.**
  - `TestServerGitHubFlowWithFakes`: a fake GitHub (httptest), fakeclaude as both router and worker, scripted through `FAKECLAUDE_SCRIPT`. A mention comment from the allowed login leads to: router `route accept` with ack, the ack comment posted, the worker reporting `ready` with `pull=7`, the PR merged in the fake, `ThreadClosed`, and the Ticket ended `merged`. The test asserts exactly one ack comment and no comment by any other path.
  - `TestRecoveryKeepsDeliveredEvalDeadline`.
  - `TestFailedTicketNotRelaunchedOnRestart` (R34).
  - `TestNudgeMovesDeliveredAt` (R33).
  - `TestPolicySetNotifiesAffectedTicket`.
  - `TestGithubCheckReportsBot`.
  - CLI golden tests for the new commands.
- [ ] **Step 2: Run** `go test ./internal/server ./internal/cli` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` and `make bench` → PASS (the new metrics are in `bench/baseline.json` via `make bench-baseline` only for new keys).
- [ ] **Step 5: Commit** `feat(server): github source, router and policy wired end to end`.

### Task 13: Setup guide and live test

**Files:**
- Create: `docs/dev/github-app.md`, `internal/server/github_live_test.go` (build tag `live`)
- Modify: `docs/dev/smoke-test.md`, `README.md` (one line)

**Interfaces:**
- Consumes: everything above.
- `docs/dev/github-app.md` covers:
  - Creating the App (name, homepage, no webhook).
  - Permissions: Contents RW, Issues RW, Pull requests RW, Checks R, Actions R, Metadata R, Commit statuses R.
  - "Only on this account" installation on the chosen repositories.
  - Downloading the key to `~/.athena/github-app.pem` with `chmod 600`.
  - `athena github setup` and `athena github check`.
  - Writing a Policy with the owner's login.
  - Safety notes: the App identity is separate from the owner, the repository is public, and observe mode is the default.
- `TestLiveGitHubMention` runs only with `ATHENA_LIVE_GITHUB=1` and `ATHENA_LIVE_REPO=owner/name`, against a sandbox repository the owner names and the real CLI with `haiku`:
  1. It creates an issue mentioning the App: "create a file hello.txt containing hi and open a PR".
  2. It waits ≤ 10 min for an ack comment by `<slug>[bot]` and a PR from `athena/<ticket-id>` touching `hello.txt`.
  3. It merges the PR through the API with the App token.
  4. It waits for the Ticket to end `merged`.
  5. It asserts no second ack, no token string in `athena logs` or the transcript, and cleans up by closing the issue and deleting the branch.

- [ ] **Step 1: Write the guide.** Check every permission name against the GitHub UI wording, using the GitHub docs page "Choosing permissions for a GitHub App".
- [ ] **Step 2: Write** `TestLiveGitHubMention`. With the variables unset, it skips with the message `set ATHENA_LIVE_GITHUB=1 and ATHENA_LIVE_REPO to run`.
- [ ] **Step 3: Run** it against the owner's sandbox repository if the owner has installed the App. Otherwise record in the report that it was not run and why, and ledger it. This is the only step that needs the owner.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `docs: github app setup and live end-to-end test`.

---

## Deferred to later plans (decided here)

- Pause processing (reference "Pause processing") moves to plan 4 with its dashboard control.
- Router tools for settings and prompt saves, and Policy sync from repository collaborators, move to plan 4. The owner edits the Policy with `athena policy set` in plan 2.
- Webhooks move to a later plan (spec §12: the VPN-only server is not reachable from GitHub, so polling with ETags stays).
- GitLab and other host adapters, and the Feishu Source, are out of scope (spec §2 goal 2, non-goals). Delivery for a non-GitHub remote (push the Ticket branch and report its link) lands with the first host adapter. Until then, owner Tickets on such repositories keep plan 1 behaviour: the branch stays local.
- Minting tokens on a remote node over mTLS is plan 6. In plan 2 the server and the node share a process, and the `Credentials` port is the seam.
