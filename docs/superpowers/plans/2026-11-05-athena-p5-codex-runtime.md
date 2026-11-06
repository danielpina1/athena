# Athena Plan 5: Codex Runtime and Multi-Runtime Tickets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Codex works Tickets as a peer of Claude Code. Each Assignment records its own runtime. An `auto` Ticket starts on the runtime that has headroom and moves to the other one, in the same worktree, when it hits a usage limit. A Ticket can have its work reviewed by the other runtime before it is ready. Sessions that a server restart cut off resume on their own.

**Architecture:**
- **One runtime interface.** A new package `internal/runtime/rt` holds the runtime-neutral session contract: `Runtime`, `Session`, the `Event` types, the transcript writer and the process helper. `internal/runtime/claude` is refactored behind it with no change in behaviour. `internal/runtime/codex` implements it over `codex app-server`, which speaks JSON-RPC over stdio.
- **The node picks the runtime.** The Engine stays pure. It records which runtime a Ticket asks for (`claude`, `codex` or `auto`) and, per Assignment, the runtime that ran it.
  - A `node.Chooser`, an interface with one default `HeadroomChooser`, picks the runtime at launch from a `quota.Tracker`.
  - The tracker is fed by both runtimes' own rate-limit events and persisted on the node.
- **Failover and review are Engine commands.**
  - `UsageLimited` ends the limited Assignment and queues a new one that the chooser places.
  - `FileReview` closes a review Assignment, which runs with role `review`, read-only.
  - Both work in the Ticket's existing worktree, and the Engine keeps its invariant of at most one live Assignment per Ticket.
- **One Ticket transcript across runtimes.** It holds Claude stream-json lines, Codex lines in an Athena envelope and Athena's own session-marker lines. `internal/render` reads all three.

**Tech Stack:**
- **Already in the repo:** Go 1.27, `modernc.org/sqlite`, the official Go MCP SDK, `buf` and protobuf-es, React 19 with Vite, Vitest and Playwright.
- **New:** the installed Codex CLI, **`codex-cli 0.160.1`** (`/opt/homebrew/bin/codex`), driven through `codex app-server --listen stdio://`. No new Go modules and no new npm packages.

**Spec:** `docs/superpowers/specs/2026-11-03-athena-design.md`. Read:
- **Binding:** §6.1, §6.1a, §6.2 (the Codex part) and §6.4.
- **For context:**
  - §5 (Placement, Approvals, stages)
  - §7.2 and §7.4 (dispatch preferences, checkpoints)
  - §10 and §10.1 (New task, the runtime badges, the design bar)
  - §11 (session containment and secrets)
  - §16.1 items 1 and 3, and §16.2 (what the builder verifies about Codex)

Also read:
- `docs/superpowers/plans/2026-11-04-athena-p4-deferred.md`: "Carry into plan 5" and the auto-resume item.
- `docs/dev/dashboard.md`: the client contract.
- `internal/runtime/claude/testdata/claude/README.md`: how fixtures are captured.

**What the installed Codex speaks.** Verified from `codex app-server generate-json-schema --out <dir>` and `codex --help`, without starting a session.
- **Messages:** newline-delimited JSON-RPC with no `"jsonrpc"` field. The client sends `initialize` and then the `initialized` notification.
- **Client requests used here:**
  - `thread/start`: `cwd`, `model`, `developerInstructions`, `approvalPolicy`, `approvalsReviewer`, `sandbox`
  - `thread/resume`: `threadId`
  - `turn/start`: `threadId`, `input[]`, `clientUserMessageId`, `sandboxPolicy`
  - `turn/steer`: `threadId`, `expectedTurnId`, `input[]`
  - `turn/interrupt`: `threadId`, `turnId`
  - `hooks/list` and `config/batchWrite`
  - `account/read` and `account/rateLimits/read`
- **Notifications:**
  - `thread/started`, `turn/started`, `turn/completed`
  - `item/started` and `item/completed`. A `ThreadItem` is one of `userMessage` (with `clientId`), `agentMessage`, `reasoning`, `commandExecution`, `fileChange`, `mcpToolCall`, `webSearch`, `plan`, `contextCompaction` and others.
  - `thread/tokenUsage/updated`
  - `account/rateLimits/updated`: `primary` and `secondary` windows, each with `usedPercent`, `resetsAt` and `windowDurationMins`, plus `rateLimitReachedType`.
  - `error`: a `TurnError` with `codexErrorInfo`, one of `usageLimitExceeded`, `rateLimitExceeded`, …, and `willRetry`.
  - `hook/started` and `hook/completed`
- **Server requests the client must answer:**
  - `item/commandExecution/requestApproval` and `item/fileChange/requestApproval`: `accept` or `decline`
  - `item/permissions/requestApproval`
  - `item/tool/requestUserInput`
  - `mcpServer/elicitation/request`
  - `item/tool/call`
  - `account/chatgptAuthTokens/refresh`
  - `attestation/generate`
- **`initialize` capabilities** take `optOutNotificationMethods`, which suppress named notifications.
- **Hooks** are stable (`features.hooks`), and their wire format matches Claude Code's: input `tool_name`, `tool_input`, `tool_use_id`, `turn_id`; output `hookSpecificOutput.permissionDecision` `allow|deny|ask`.
  - A hook from user config runs only when trusted: `hooks.state."<key>".trusted_hash`, reported by `hooks/list` as `trustStatus`.
- **Attribution.** Codex adds a `Co-authored-by: Codex <noreply@openai.com>` instruction unless the *workspace* disables it. There is no local config key for this, so Athena enforces attribution itself (Task 7).
- **Sandbox:** `codex sandbox <cmd>` runs a command under the sandbox with no model call.

## Global Constraints

- Every plan 1–4 Global Constraint still holds:
  - plan 3's CSP, unchanged
  - plan 4's Precision tokens, unchanged; status colours use R59's green and amber, and orange stays for what needs the owner
  - plan 4's client link rules and storage rules
  - the bundle budget of 200 KB JS / 30 KB CSS gzipped for the initial route, with the lazy-chunk sentinels in `web/scripts/bundle-budget.mjs`. The entry is about 188 KB today, so this plan may add at most 6 KB gzipped to it.
- `make check` passes at the end of every task. Every task that changes the UI also runs `make e2e`, with screenshots at 390, 820 and 1440 px, light and dark, and axe clean.
- **No new Go modules and no new npm packages.** Codex's `config.toml` is written by a small emitter of the exact keys this plan names (Task 4), not by a TOML library.
- **Never the owner's real Codex or quota in tests.**
  - Every Go and e2e test uses `fakecodex` (Task 3) or `fakeclaude`.
  - `scripts/capture-codex-fixtures.sh turns` spends quota. It runs only by hand with `ATHENA_CAPTURE_CONSENT=spend-codex-quota`, never from `make`.
  - Reading help, generating the schema, `hooks/list`, `account/read`, `account/rateLimits/read` and `codex sandbox` do not send prompts, and are allowed in opt-in real tests (`ATHENA_REAL_CODEX=1`).
- **E2E conventions:** builds go to `bin/e2e/` (now including `fakecodex`), never `bin/athena`. The harness never touches the owner's running server, state directory or `~/.codex`.
- **Every outward effect goes through the Outbox**, as before.
  - Failover, review and resume post nothing on GitHub themselves. Only the existing Report → reply path speaks.
  - The repository is public: fixtures, docs and screenshots carry no account ids, emails, tokens or real thread ids. Fixtures use the placeholders `thr_fixture…` and `acct_fixture`.
- **Spec, verbatim (§6.1a):**
  - "A Ticket's runtime may be `claude`, `codex`, or **`auto`** (default once plan 5 lands), and a Ticket's Assignments may run on different runtimes over its life. Each Assignment records its own `Runtime` and `Model`."
  - "For `auto`, the dispatcher picks the runtime with headroom (ties → the Ticket's previous runtime for a warm session, then the owner's preferred default)."
  - "The Ticket's stage never shows a person stage just because a limit was hit."
  - "Two sessions never write the same worktree at the same time."
- **Spec, verbatim (§6.2):**
  - "Athena installs its own hooks in every session it starts (via the generated settings / `CODEX_HOME`, never by editing the user's global config or the repository)."
  - "If a hook mechanism is unavailable for a runtime version, the node refuses to start that runtime and reports why (no silent fallback to uncontrolled sessions)."
- **Spec, verbatim (§16.1):**
  - item 1: "Headless sessions run in each runtime's non-bypass permission mode so these rules actually fire."
  - item 3: "rate-limit errors back off and requeue rather than fail"
- **Spec, verbatim (§11 Secrets):** "Never sent over the link, never placed in prompts, logs, URLs or the dashboard."
  - Athena's Codex credential lives only in `<stateDir>/codex/auth.json`, mode 0600, written by `codex login` itself.
  - It is never copied from or to `~/.codex`.
- **Codex configuration is Athena's own.**
  - `CODEX_HOME=<stateDir>/codex` (0700), one per node. Its `config.toml` (0600) is rewritten by the node at start and never edited by hand.
  - The driver always runs `codex app-server --listen stdio:// --strict-config`, one process per session. It never uses the shared daemon, `remote-control` or `--remote`.
  - Per-session values are passed as `-c` overrides.
- **Codex version:**
  - The supported minimum is `0.160.0`.
  - `0.160.1` is the version this plan was verified against: `codex.VerifiedVersion`.
  - A newer version starts only if the hook check passes, and the node shows "untested Codex version".
- **Approvals:** never `approvalsReviewer: "auto_review"` or `guardian_subagent`, which would spend quota and decide outside Athena's Policy. Never `approvalPolicy: "never"`. Never sandbox `danger-full-access`.
- **Copy:**

  | Thing | Text |
  | --- | --- |
  | runtime label | `Claude Code` · `Codex` · `Auto` |
  | restart entry | "Interrupted by a server restart" |
  | resume entry | "Resumed automatically" |
  | limit entry | "Usage limit reached on <runtime>", subtitle "Resets at <time>", or "Reset time unknown" when no reset is known |
  | next start after a limit | "Continued on <runtime> · <model>" |
  | review start | "Review started · <runtime>" |
  | review verdicts | "Review approved", "Changes requested" (subtitle: the first finding), "Review could not finish" |
  | quota, unmeasured | "Not measured yet" |
  | quota, limited | "Limited until <time>" |

## Review Focus

These are the inputs the spec implies that are most likely to bite the owner, most likely first. Each one has its test in the owning task.

1. **The server stops mid-turn, and the CLI's Stop hook still fires in the close grace.** The Ticket must resume once after the restart and must not sit idle. A second restart before the resume finishes must record a second interruption, not lose the first. Task 1: `TestStopHookDuringShutdownIsIgnored`, `TestTwoRestartsRecordTwoInterruptions`.
2. **Codex is installed but not usable.** It may be logged out, a hook may be untrusted or modified, or a foreign hook may be enabled. `auto` must never choose it, and a Ticket pinned to `codex` must report why instead of running uncontrolled. Task 4: `TestHookCheckRefusesForeignOrUntrusted`. Task 8: `TestAutoSkipsUnreadyRuntime`, `TestPinnedUnreadyRuntimeFailsWithWhy`.
3. **Both accounts are limited at once, or a reset time is unknown or already past.** The Ticket stays `working/queued` and wakes at the earliest reset, or after a bounded backoff. There is no person stage and no busy loop. Task 9: `TestBothLimitedWaitsForEarliestReset`, `TestUnknownResetBacksOff`.
4. **Codex sends a server request Athena does not handle**: `item/tool/requestUserInput`, `mcpServer/elicitation/request`, `item/tool/call`, or a future method. It is answered at once, so the turn never hangs waiting. Task 5: `TestUnhandledServerRequestsAreAnswered`.
5. **One Ticket's transcript mixes runtimes**: Claude, then Codex after a failover, then Claude again after the reset, plus a 1 MiB command output and ANSI escapes. Every line renders with continuous offsets, a runtime divider at each switch, clipped and redacted output, and no "unreadable line". Task 6: `TestMixedTranscriptRendersEveryRuntime`, `TestHugeCodexOutputIsClipped`.

## File structure

```
internal/runtime/rt/                rt.go (Runtime, Session, StartOptions, Info), events.go, transcript.go, proc.go (spawn, process group, stderr tail, ChildEnv), sandbox.go
internal/runtime/claude/            session.go and events.go behind rt (rate_limit_event parsed), runtime.go (claude.Runtime)
internal/runtime/codex/             rpc.go (Conn), translate.go (Translator), home.go (config.toml, hooks, trust), session.go, runtime.go, version.go
internal/runtime/codex/testdata/    codex/*.jsonl fixtures + README.md + protocol.json, fakecodex/main.go, protoextract/main.go
scripts/capture-codex-fixtures.sh   schema (free) | turns (needs consent)
internal/quota/                     tracker.go (per-runtime windows, limits, persistence)
internal/node/                      runtimes.go (registry, chooser), node.go (runtime-neutral runner), handoff.go, checkpoint.go, orphans.go, review.go, rules.go (+ codex tool mapping, attribution, read-only), hookcmd.go (--runtime), mcpcmd.go (--role review)
internal/render/                    codex.go, marker.go; items.go and render.go dispatch by line format
internal/core/                      model.go (Assignment.Runtime/Role/Interruptions/Limit, Ticket.Pairing/Review), engine.go, review.go, limits.go
internal/hub/views.go, proto/       runtime fields, NodeStatus.runtimes, timeline subkinds, SubmitTask runtime/review
internal/server/                    config.go (runtimes, quota), wiring.go, server.go (recovery), dispatch.go
internal/cli/                       SIGHUP, `athena codex login|status`, task --runtime/--review, server --codex
prompts/reviewer.md, prompts.go     the reviewer part
web/src/features/                   new-task/RuntimePicker.tsx, tickets (badges, filter, timeline copy), nodes/QuotaBars.tsx, session/tools.codex.ts
web/e2e/                            harness builds fakecodex; codex-session.spec.ts, multi-runtime.spec.ts
docs/dev/codex.md                   setting up Codex
```

---

### Task 1: Auto-resume after a server restart, and SIGHUP

Today, a stop, crash or closed terminal can leave a busy session looking idle. The CLI's Stop hook fires during the close grace, so `recoverAssignments` parks the session ("idle assignment resumes on its next input"). A closed terminal sends SIGHUP, which kills the server without a graceful stop and orphans the runtime processes, which run in their own process groups.

**Files:**
- Modify:
  - `internal/core/model.go`, `internal/core/engine.go`
  - `internal/node/node.go` (the shutdown path and the restart note), `internal/node/socket.go` (Stop during shutdown)
  - `internal/server/server.go` (`recoverAssignments`, `interrupted`)
  - `internal/cli/cli.go` (signals)
  - `internal/hub/views.go` (timeline)
  - `web/src/features/tickets/timelineModel.ts`
- Create: `internal/node/orphans.go`
- Test: `internal/core/engine_test.go`, `internal/node/node_test.go`, `internal/node/orphans_test.go`, `internal/server/server_test.go`, `internal/cli/cli_test.go`, `internal/hub/views_test.go`, `web/src/features/tickets/timelineModel.test.ts`

**Interfaces:**
- Produces, in core:
  - `type Interruption struct{ At time.Time; Reason string; ResumedAt *time.Time }`
  - `Assignment.Interruptions []Interruption` (json `interruptions,omitempty`)
  - `(*Assignment).OpenInterruption() *Interruption`: the last one, when its `ResumedAt` is nil.
  - `const InterruptRestart = "server restart"`
  - `(*Engine).AssignmentInterrupted(ticketID, assignmentID, reason string) error`:
    - On the running Assignment of an open Ticket, it appends an Interruption unless one is already open. That makes it idempotent.
    - The Assignment's state is unchanged.
    - Any other target changes nothing.
  - `AssignmentStarted` sets `ResumedAt` on an open interruption.
  - `node.EngineCallbacks` gains `AssignmentInterrupted`. Tasks 9 and 10 add `UsageLimited` and `FileReview` the same way.
- Node:
  - Before a shutdown stops a session, the runner of each **busy** session calls `AssignmentInterrupted(…, InterruptRestart)`.
    - A session is busy when its runner sent an Input or the nudge after its last good turn end, or when its first turn has not ended yet.
    - The call is made on the runner goroutine, never on the loop.
  - A `Stop` hook from a session stopped by shutdown records no `TurnEnded`.
  - A resumed session whose Assignment has an open restart interruption starts with `restartNote = "Athena restarted while you were working; continue the task."` plus `recoveryAdvice`, instead of `recoveryNote`.
- Orphans:
  - After each spawn the node writes `<stateDir>/run/<ticket id>.json` (0600), `{"pid","pgid","started","runtime"}`. `started` is `ps -o lstart= -p <pid>`, trimmed.
  - The file is removed after the process is reaped.
  - `reapOrphans(stateDir string, log *slog.Logger)` runs in `New`. For each file whose pid still has the same `started`, it sends SIGTERM to the group and waits 5 s, then SIGKILL, then removes the file. A reused pid is left alone.
  - A `Pid() int` on the process is needed. Task 2 moves it onto `rt.Session`; here it is added to `claude.Session`.
- Server recovery:
  - `interrupted(t, a)` is also true when `a.OpenInterruption()` is set and `a.IdleAt` is nil or not after its `At`.
  - For an interrupted running Assignment with no open interruption, which is the crash path, recovery first calls `AssignmentInterrupted(…, InterruptRestart)`, then `n.Start`.
  - Idle Assignments still wait for their next Input.
- CLI: `athena server` stops gracefully on SIGINT, SIGTERM **and SIGHUP**.
- Timeline:
  - Each Interruption gives an entry `assignment`/`restart` at `At`, plus `assignment`/`resumed` at `ResumedAt` when that is set.
  - The legacy `interrupted` end entry is unchanged.
  - Web copy: `restart` → "Interrupted by a server restart" (amber). `resumed` → "Resumed automatically" (green).

- [ ] **Step 1: Write failing tests:**
  - core:
    - `TestAssignmentInterruptedOpensOnce`
    - `TestStartClosesOpenInterruption`
    - `TestInterruptIgnoresEndedOrIdleTargets`
  - node:
    - `TestShutdownMarksBusySessionsInterrupted`: fakeclaude holds its result with `ATHENA_FAKE_HOLD_RESULT`. `Close()` makes the fake engine see `AssignmentInterrupted` for the busy Ticket and not for an idle one.
    - `TestStopHookDuringShutdownIsIgnored`
    - `TestRestartResumeUsesRestartNote`
  - orphans:
    - `TestReapOrphansKillsRecordedGroup`: a `sleep 60` child in its own group is killed, and its file is removed.
    - `TestReapIgnoresReusedPid`
  - server:
    - `TestRestartResumesInterruptedSession`: run the server, start a task mid-turn, cancel, run again. The args log shows `--resume`. The first stdin line starts with `restartNote`. The timeline is `restart` then `resumed`.
    - `TestCrashRecoveryRecordsInterruption`
    - `TestIdleAssignmentStillWaits`
    - `TestTwoRestartsRecordTwoInterruptions`
  - cli: `TestServerStopsOnSIGHUP`, a subprocess that logs "athena server stopping" and exits 0.
  - web: `timeline says interrupted and resumed`
- [ ] **Step 2: Run** `go test -race ./internal/core ./internal/node ./internal/server ./internal/cli ./internal/hub` and `npm --prefix web test` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(node): resume sessions a restart interrupted; SIGHUP stops gracefully`.

### Task 2: The runtime interface, with Claude Code behind it

There is no behaviour change. Every existing node, server, render and e2e test passes unmodified, except for the mechanical renames listed here.

**Files:**
- Create:
  - `internal/runtime/rt/rt.go`, `events.go`, `transcript.go`, `proc.go`, `sandbox.go`
  - `internal/runtime/claude/runtime.go`
- Modify:
  - `internal/runtime/claude/session.go`: use `rt.Proc` and `rt.Transcript`; `Events()` yields `rt.Event`; `Send(text, uuid)`.
  - `internal/runtime/claude/events.go`: parse `rate_limit_event`.
  - `internal/runtime/claude/config.go`: take `rt.Sandbox`.
  - `internal/node/node.go`, `sandbox.go`, `router.go`, `status.go`
  - `internal/node/runtimes.go` (create)
- Test: `internal/runtime/rt/*_test.go`, `internal/runtime/claude/session_test.go`, `events_test.go`, `internal/node/node_test.go`

**Interfaces:**
- `rt` events: `type Event interface{ isEvent() }`:
  - `Init{SessionID, Model string}`
  - `AssistantText{Text string}`
  - `ToolUse{ID, Name string; Input json.RawMessage}`
  - `ToolResult{ID string; IsError bool}`
  - `UserEcho{Text, UUID string}`
  - `TurnEnd{SessionID string; OK bool; Subtype, Text string; Errors []string; CostUSD float64; InputTokens, OutputTokens, CacheReadTokens int64; NumTurns int; Duration time.Duration; Limit *LimitHit}`
  - `QuotaUpdate{Windows []Window}`
  - `Unknown{Kind string}`
  - with `Window{Name string; Used float64; ResetsAt *time.Time; Limited bool}`. `Name` is normalized to `5h`, `7d`, or `<n>m`. `Used` is a fraction in [0, 1], never a percent.
  - and `LimitHit{Runtime string; ResetsAt *time.Time; Message string; Transient bool}`
- `rt.Session`: `Events() <-chan Event`, `Send(text, uuid string) error`, `InterruptTurn() error`, `Close() error`, `Wait() error`, `Killed() bool`, `Stderr() string`, `Pid() int`.
- `rt.Runtime`:
  - `Name() string`
  - `Check(ctx context.Context) Info`, with `Info{Installed, Ready, Tested bool; Version, Why string}`. `Tested` is false for a version newer than the one verified.
  - `Prepare(p Prepare) error`: per-session files, given `Prepare{WorkDir, AthenaBin, Socket, TicketID, AssignmentID string; HookTimeout time.Duration; Sandbox *Sandbox; SandboxEnv []string; Role string}`
  - `Start(ctx context.Context, o StartOptions) (Session, error)`, with `StartOptions{Dir, Model, ResumeID, SystemPrompt string; Env []string; Transcript *Transcript; Role string; ReadOnly bool; Sandbox *Sandbox; Approve func(context.Context, ApprovalRequest) Verdict}`
- Approvals: `ApprovalRequest{Kind string /* command|file|network|permissions */; Command, Cwd, Host, Reason string; Paths []string}` and `Verdict{Allow bool; Reason string}`.
- `rt.Sandbox{Required bool; AllowWrite, DenyWrite, DenyRead, AllowedDomains, UnixSockets []string; AllowAllUnixSockets, WeakerNetworkIsolation bool}`: today's `claude.Sandbox` fields, moved.
- `rt.OpenTranscript(path string, onLine func(off int64, line []byte)) (*Transcript, error)`:
  - It creates the directory 0700 and the file 0600.
  - `(*Transcript).Append(line []byte) int64` returns the offset the line starts at, exactly as `handleLine` computes it today, short writes included.
  - `Close() error`
  - The node opens one per process start and closes it after `Wait`.
- `rt.StartProc(ctx context.Context, o ProcOptions) (*Proc, error)` takes `ProcOptions{Binary string; Args []string; Dir string; Env []string; CloseGrace time.Duration}`:
  - own process group, `WaitDelay` 2 s, kill on context cancel, a 4 KiB stderr tail
  - `Stdin() io.WriteCloser`, `Stdout() *os.File`, `CloseStdin()`, `KillGroup() error`, `Wait() error`, `Killed() bool`, `Pid() int`
- `rt.ChildEnv(parent, extra []string, keepPrefixes ...string) []string`:
  - It keeps today's `envNames`/`envPrefixes` rule and `redact.SecretName` filtering.
  - `keepPrefixes` are kept whatever their name: Claude passes `CLAUDE_` and `ANTHROPIC_`; Codex passes `CODEX_` and `OPENAI_`.
- `claude.Runtime{Binary string}` implements `rt.Runtime`:
  - `Prepare` writes `settings.json` and `mcp.json` exactly as `node.prepare` does today.
  - `Check` runs `claude --version`.
- Claude events:
  - A `Result` becomes `TurnEnd`, with `OK = !IsError && (Subtype == "" || Subtype == "success")`.
  - A `rate_limit_event` becomes a `QuotaUpdate`: `unifiedWindows` keys `five_hour` → `5h`, `seven_day` → `7d`, with `utilization` used as a fraction and `status == "rejected"` setting `Limited`.
  - `Limit` stays nil in this task; Task 9 sets it.
- Node:
  - `Config.Runtimes map[string]rt.Runtime`, which defaults to `{"claude": claude.Runtime{Binary: ClaudeBinary}}`.
  - The runner keeps a `rt.Session` and switches on `rt` events.
  - `ErrExited` checks use `rt.ErrExited`, and `claude.ErrExited` is an alias of it.
  - The node ignores `QuotaUpdate` in this task.
  - The router keeps calling `claude.Start` directly; it stays Claude Code (§6.3).

- [ ] **Step 1: Write failing tests:**
  - `TestTranscriptOffsetsMatchFile`: includes a line with no trailing newline.
  - `TestChildEnvKeepsRuntimePrefixesDropsSecrets`
  - `TestProcKillsGroupAfterGrace`
  - `TestClaudeResultBecomesTurnEnd`
  - `TestClaudeRateLimitEventBecomesQuota`: the fixture line from `simple-turn.jsonl` gives `7d` with Used 0.12 and its reset time.
  - `TestClaudeRuntimeImplementsRT`: a compile-time assertion.
- [ ] **Step 2: Run** `go test -race ./internal/runtime/... ./internal/node` → FAIL.
- [ ] **Step 3: Implement.** Move code rather than rewrite it. Only the renames above may touch existing tests: `Send(text)` → `Send(text, "")`, and `claude.Sandbox` → `rt.Sandbox`.
- [ ] **Step 4: Run** `make check` and `make bench` → PASS. Every budget still passes, and no benchmark regresses by more than 10%.
- [ ] **Step 5: Commit** `refactor(runtime): one session interface; claude code behind it`.

### Task 3: The Codex protocol layer: JSON-RPC, event mapping, fixtures and fakecodex

**Files:**
- Create:
  - `internal/runtime/codex/rpc.go`, `translate.go`, `version.go`
  - `internal/runtime/codex/testdata/codex/README.md` and the fixtures `simple-turn.jsonl`, `tool-turn.jsonl`, `steer-turn.jsonl`, `usage-limit-turn.jsonl`, `approval-turn.jsonl`, `rate-limits.jsonl`
  - `internal/runtime/codex/testdata/codex/protocol.json`
  - `internal/runtime/codex/testdata/protoextract/main.go`
  - `internal/runtime/codex/testdata/fakecodex/main.go`
  - `scripts/capture-codex-fixtures.sh`
- Test: `internal/runtime/codex/rpc_test.go`, `translate_test.go`, `fixtures_test.go`, `testdata/fakecodex/main_test.go`

**Interfaces:**
- `codex.Conn`. Messages are one JSON object per line with no `jsonrpc` field.
  - `NewConn(r io.Reader, w io.Writer, onLine func(raw []byte)) *Conn`
  - `Call(ctx context.Context, method string, params, result any) error`: ids are integers from 1; a JSON-RPC error becomes `*RPCError{Code int; Message string}`.
  - `Notify(method string, params any) error`
  - `Run(handle func(Message))`: reads until EOF. Responses go to their callers; notifications and server requests go to `handle`.
  - `Respond(id json.RawMessage, result any) error` and `RespondError(id json.RawMessage, code int, msg string) error`
  - `Message{ID json.RawMessage; Method string; Params json.RawMessage}`
  - `onLine` gets every incoming line, for the transcript. Outgoing lines are never logged, because `thread/start` carries the system prompt.
- `codex.Translator` (stateful, one per session): `Translate(m Message) []rt.Event`.

  | Codex message | rt event |
  | --- | --- |
  | `thread/started` | `Init{SessionID: thread.id, Model: thread.model}` |
  | `item/completed` `agentMessage` | `AssistantText` |
  | `item/started` `commandExecution` | `ToolUse{ID: item.id, Name: "exec_command", Input: {"command","cwd"}}` |
  | `item/completed` `commandExecution` | `ToolResult{IsError: exitCode != 0 \|\| status == "failed" \|\| status == "declined"}` |
  | `item/started` `fileChange` | `ToolUse{Name: "apply_patch", Input: {"changes":[{"path","kind"}]}}` |
  | `item/completed` `fileChange` | `ToolResult{IsError: status != "completed"}` |
  | `mcpToolCall` started / completed | `ToolUse{Name: "mcp__<server>__<tool>", Input: arguments}` / `ToolResult{IsError: error != null}` |
  | `webSearch` | `ToolUse{Name: "web_search"}` then `ToolResult` |
  | `item/completed` `userMessage` | `UserEcho{Text: text parts joined by "\n", UUID: clientId}` |
  | `thread/tokenUsage/updated` | kept; folded into the turn's `TurnEnd` (`last.inputTokens`, `outputTokens`, `cachedInputTokens`) |
  | `account/rateLimits/updated` | `QuotaUpdate` (window name from `windowDurationMins`: 300 → `5h`, 10080 → `7d`, else `<n>m`; `Used = usedPercent/100`; `ResetsAt` from unix seconds; `Limited` when `rateLimitReachedType` is set) |
  | `turn/completed` | `TurnEnd{OK: status=="completed", Subtype: "" \| "interrupted" \| the codexErrorInfo code, Text: error.message, Duration: durationMs, NumTurns: 1, CostUSD: 0}` |
  | `error` with `willRetry: false` | kept for the turn's `TurnEnd.Errors` |
  | anything else | `Unknown{Kind: method or item type}` |

  - `turn/completed` with `codexErrorInfo` `usageLimitExceeded` sets `Limit{Runtime: "codex", ResetsAt: the reset of the latest window with Used ≥ 1, else nil}`.
  - `rateLimitExceeded` sets `Limit{Transient: true}`.
- `version.go`:
  - `VerifiedVersion = "0.160.1"`, `MinVersion = "0.160.0"`
  - `ParseVersion(out string) (string, error)` for `codex-cli X.Y.Z`
  - `Supported(v string) (ok, tested bool)`
- `protocol.json`:
  - Written by `go run ./internal/runtime/codex/testdata/protoextract <schema dir>`. It records `{"codexVersion", "methods": {name: [required param fields]}}` for the client requests, notifications and server requests this plan uses.
  - `scripts/capture-codex-fixtures.sh schema` regenerates it with `codex app-server generate-json-schema` into a temp directory. This is free and sends no prompts.
- `scripts/capture-codex-fixtures.sh turns`:
  - It refuses to run without `ATHENA_CAPTURE_CONSENT=spend-codex-quota`.
  - It runs one short turn in a temp git repository, with Athena's `CODEX_HOME` and its hooks logging their stdin.
  - It rewrites ids to `thr_fixture…`, `acct_fixture` and `item_…` before saving.
  - It is never run by `make`.
  - Until the owner runs it, the fixtures are hand-written from the schema, and the README marks them `synthetic`.
- `fakecodex`. Environment variables mirror fakeclaude:
  - **Fixtures:** `ATHENA_FAKE_CODEX_FIXTURE` (its notifications are replayed on the first `turn/start`; `{ticket}` → `ATHENA_TICKET`), `ATHENA_FAKE_CODEX_LATER_FIXTURE` (on each later `turn/start` or `turn/steer`) and `ATHENA_FAKE_CODEX_HOLD_RESULT`.
  - **Logs:** `ATHENA_FAKE_CODEX_RPC_LOG` (every request it received, one JSON line each) and `ATHENA_FAKE_CODEX_ARGS_LOG` (args plus `CODEX_HOME` plus the `ATHENA_*` env).
  - **Failure modes:** `ATHENA_FAKE_CODEX_EXIT_MID_TURN` and `ATHENA_FAKE_CODEX_HOOKS` (`trusted`, `untrusted` or `foreign`, which shapes the `hooks/list` answer).
  - **Answers:** it answers `initialize`, `thread/start` (thread id `thr_fixture_<ticket>`), `thread/resume`, `turn/start`, `turn/steer`, `turn/interrupt` (it ends the turn `interrupted`), `hooks/list`, `config/batchWrite`, `account/read` (`acct_fixture`, or none with `ATHENA_FAKE_CODEX_LOGGED_OUT`) and `account/rateLimits/read`.
  - **Server requests:** a fixture line `{"fake":"request", …}` makes it send that server request and wait for the answer, which it logs.
  - **Hooks:** a fixture line `{"fake":"hook","event":"PreToolUse","input":{…}}` makes it run the `PreToolUse` command it reads from `$CODEX_HOME/config.toml`. It parses only the exact `[[hooks.<Event>.hooks]]` / `command = "…"` lines Task 4 writes. It pipes the input, and drops the next item when the answer is `deny`.

- [ ] **Step 1: Write failing tests:**
  - `TestConnCallMatchesResponseByID`
  - `TestConnRoutesServerRequestsAndNotifications`
  - `TestConnRPCErrorIsTyped`
  - `TestTranslateToolTurn`: the event sequence from `tool-turn.jsonl` is exact.
  - `TestTranslateUserEchoCarriesClientID`
  - `TestTranslateUsageLimitSetsLimitWithReset`
  - `TestTranslateRateLimitWindowsNormalized`: 300 → `5h`, `usedPercent` 42 → 0.42.
  - `TestTranslateUnknownItemIsUnknown`
  - `TestFixturesUseKnownMethodsAndRequiredFields`: against `protocol.json`.
  - `TestFixturesCarryNoRealIDs`: no `@`, no `sk-`, and only `thr_fixture` thread ids.
  - `TestParseVersionAndSupported`
  - `TestFakecodexSpeaksTheFixture`: drive it with `Conn`.
- [ ] **Step 2: Run** `go test -race ./internal/runtime/codex/...` → FAIL.
- [ ] **Step 3: Implement.** Run `scripts/capture-codex-fixtures.sh schema` once to write `protocol.json`.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(codex): app-server JSON-RPC, event mapping, fixtures and fakecodex`.

### Task 4: Athena's Codex home: config, hooks and their trust, MCP, sandbox, login and version check

**Files:**
- Create: `internal/runtime/codex/home.go`, `internal/runtime/codex/runtime.go` (`Check`, `Prepare`), `internal/runtime/codex/real_test.go` (opt-in, `ATHENA_REAL_CODEX=1`)
- Modify: `internal/cli/cli.go` (`athena codex login`, `athena codex status`), `internal/server/config.go` and `wiring.go` (a `runtimes.codex` part)
- Test: `internal/runtime/codex/home_test.go`, `runtime_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- `codex.Home{Dir, AthenaBin, Socket string; HookTimeout time.Duration; Model string}`.
- `WriteHome(h Home) error` writes `<Dir>/config.toml` (0600, directory 0700). It has a fixed header comment and exactly these keys:

  | Key | Value |
  | --- | --- |
  | `model` | `h.Model` (only when not empty) |
  | `approval_policy` | `"on-request"` |
  | `approvals_reviewer` | `"user"` |
  | `sandbox_mode` | `"workspace-write"` |
  | `check_for_update_on_startup` | `false` |
  | `background_terminal_max_timeout` | `1800000` (§6.1's 30-minute post-turn background cap) |
  | `[analytics] enabled` | `false` |
  | `[shell_environment_policy] inherit` | `"all"` (the process env is already allowlisted by `rt.ChildEnv`) |
  | `[features]` false | `apps`, `plugins`, `remote_plugin`, `browser_use`, `browser_use_external`, `computer_use`, `in_app_browser`, `multi_agent`, `image_generation`, `memories`, `realtime_conversation` (nothing that acts outside the worktree or spends quota unasked) |
  | `[mcp_servers.athena]` | `command = <AthenaBin>`, `args = ["mcp"]`, `default_tools_approval_mode = "approve"`; `args` and `env` are overridden per session |
  | `[[hooks.<Event>]]` with `[[hooks.<Event>.hooks]]` | for each of `PreToolUse`, `PermissionRequest`, `PostToolUse`, `Stop`, `SessionStart`: `type = "command"`, `command = "<shell-quoted AthenaBin> hook --runtime codex <Event>"`, `timeout` = `HookTimeout` seconds for the decision events, else 30 |

  - String values are TOML basic strings, with `"`, `\` and control characters escaped.
  - The hooks live in `config.toml`, not in the separate `hooks.json` that the spec's §6.1 table names. That keeps one file and one write, and `TrustHooks` checks where Codex actually loaded them from.
- `HookSpecs(h Home) []HookSpec`, with `HookSpec{Event, Command string}`: what Athena installed.
- `TrustHooks(ctx context.Context, c *Conn, cwd string, want []HookSpec) error`:
  1. `hooks/list` for `cwd`.
  2. For each listed hook whose command is one of `want` and whose `trustStatus` is `untrusted` or `modified`: `config/batchWrite` `hooks.state."<key>".trusted_hash = <currentHash>`.
  3. `hooks/list` again.
  4. Fail with `ErrHooksUncontrolled{Why string}` unless every `want` is present, `enabled` and `trusted` (or `managed`), and no other hook is enabled.
  - Athena only ever trusts hooks it wrote itself, into its own home.
- `codex.Runtime{Binary, Home string; Model string; AthenaBin, Socket string; HookTimeout time.Duration}`:
  - `Check(ctx)` runs `codex --version` and applies `Supported`.
    - It writes the home, starts a short-lived app-server with no thread, `initialize`, then `account/read`. No account gives `Why = "Codex is not logged in for Athena: run athena codex login"`.
    - Then `TrustHooks` runs for the work root. A failure gives `Ready=false` with its `Why`.
    - The result is cached for 10 minutes, and `Invalidate()` clears it.
  - `Prepare` does nothing per directory: Codex's per-session values are `-c` overrides at `Start`.
- Sandbox, given at `Start` as `sandboxPolicy` on every `turn/start`:
  - `workspaceWrite`, with `writableRoots: [workDir, gitCommonDir]` (from `node.gitDirs`) and `networkAccess: true`.
  - `readOnly` (network on) for `ReadOnly` sessions.
  - Deviations from the Claude workers, written into `docs/dev/codex.md` in Task 14:
    - Codex cannot deny a sub-path of a writable root, so `.git/config`, `.git/hooks` and `.athena/*` are protected by the PreToolUse rules (Task 7).
    - It has no stable domain allowlist (`network_proxy` is experimental), so its network is not domain-restricted.
- CLI:
  - `athena codex login [--with-api-key]` runs `codex login` with `CODEX_HOME=<stateDir>/codex`, stdin and stdout attached.
  - `athena codex status` prints `Check`: installed, version, tested, logged in (account kind only, never ids or emails), hooks.
- Server config: `"runtimes": {"codex": {"enabled": true, "binary": "codex", "model": ""}}`.
  - Codex is registered when enabled. When it is not `Ready`, it is registered but never chosen (Task 8), and its `Why` is shown (Task 11).
  - CLI flag: `athena server --codex <path>`.

- [ ] **Step 1: Write failing tests:**
  - `TestWriteHomeExactKeys`: golden file `testdata/codex/config.golden.toml`.
  - `TestTomlStringEscaping`: quotes, backslash, newline and a non-ASCII path.
  - `TestHookCommandsUseRuntimeFlag`
  - `TestTrustHooksWritesOnlyAthenaHashes`: fakecodex untrusted; the RPC log has one batchWrite per hook and no others.
  - `TestHookCheckRefusesForeignOrUntrusted`: fakecodex `foreign`, and `untrusted` after the write, give `ErrHooksUncontrolled`.
  - `TestCheckNotLoggedIn`
  - `TestCheckUntestedVersionWarns`
  - `TestCodexLoginUsesAthenaHome`: the args log shows `CODEX_HOME=<state>/codex`, and `~/.codex` is untouched.
  - `real_test.go` (opt-in):
    - `TestRealHooksListTrusted`
    - `TestRealSandboxCommitsInWorktree`: `codex sandbox -c sandbox_mode=workspace-write …` runs `git commit` in a worktree successfully, a write outside the roots fails, and nothing is spent.
- [ ] **Step 2: Run** `go test -race ./internal/runtime/codex ./internal/cli ./internal/server` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS. Run `ATHENA_REAL_CODEX=1 go test ./internal/runtime/codex -run Real` once and record the result in the task report. It sends no prompts.
- [ ] **Step 5: Commit** `feat(codex): athena-owned CODEX_HOME with trusted hooks, MCP, sandbox and login`.

### Task 5: The Codex session driver

**Files:**
- Create: `internal/runtime/codex/session.go`
- Modify: `internal/runtime/codex/runtime.go` (`Start`)
- Test: `internal/runtime/codex/session_test.go`

**Interfaces:**
- `(codex.Runtime).Start(ctx, rt.StartOptions) (rt.Session, error)`:
  - **Spawn** `<Binary> app-server --listen stdio:// --strict-config` with these overrides:
    - `-c mcp_servers.athena.args=["mcp","--ticket","<id>"]`, plus `["--role","review"]` for the review role
    - `-c mcp_servers.athena.env={ATHENA_SOCKET=…,ATHENA_ASSIGNMENT=…}`
    - Env: `rt.ChildEnv(os.Environ(), o.Env, "CODEX_", "OPENAI_")` plus `CODEX_HOME=<Home>`.
  - **`initialize`** with `clientInfo{name:"athena", version}`, `capabilities{optOutNotificationMethods: [item/agentMessage/delta, item/commandExecution/outputDelta, item/fileChange/outputDelta, item/reasoning/textDelta, item/reasoning/summaryTextDelta, item/reasoning/summaryPartAdded, item/plan/delta, command/exec/outputDelta]}`, then `initialized`.
  - **Thread:**
    - New: `thread/start{cwd: Dir, model, developerInstructions: SystemPrompt, approvalPolicy:"on-request", approvalsReviewer:"user", sandbox: "workspace-write" | "read-only", serviceName:"athena"}`.
    - Resume: `thread/resume{threadId: ResumeID}`, with the same overrides and without `developerInstructions` (a resumed session keeps its prompt).
  - **Quota:** one `account/rateLimits/read`, emitted as a `QuotaUpdate`. It is free.
  - **Transcript:** every incoming line is appended as `{"at":"<RFC3339Nano>","codex":<line>}`.
- `Send(text, uuid)`:
  - Between turns: `turn/start{threadId, input:[{type:"text",text}], clientUserMessageId: uuid, sandboxPolicy}`.
  - During a turn: `turn/steer{threadId, expectedTurnId, input}`. When that fails with `activeTurnNotSteerable` or a turn-mismatch error, it falls back to `turn/start`. The active turn id is tracked from `turn/started` and `turn/completed`.
  - After the process exits: `rt.ErrExited`.
- `InterruptTurn()`: `turn/interrupt` for the active turn; nil when there is none. The turn then ends `interrupted`, and the node treats it exactly as it treats Claude's `error_during_execution`.
- Server requests, each answered once:

  | Request | Answer |
  | --- | --- |
  | `item/commandExecution/requestApproval` | `Approve(ctx, {Kind:"command" or, with networkApprovalContext, "network"; Command, Cwd, Host, Reason})` → `accept` / `decline` |
  | `item/fileChange/requestApproval` | `Approve({Kind:"file"; Paths, Reason})` |
  | `item/permissions/requestApproval` | `Approve({Kind:"permissions"; Reason})` |
  | `item/tool/requestUserInput`, `mcpServer/elicitation/request` | decline / cancel at once (no person waits inside a turn; the worker asks through `report`) |
  | `item/tool/call`, `account/chatgptAuthTokens/refresh`, `attestation/generate`, any unknown method | JSON-RPC error `-32601` |

  - The `Approve` context is cancelled by `Close`, and a cancelled approval answers `decline`.
- `Close()`: cancel pending approvals, close stdin, wait the grace (10 s), then kill the group. `Stderr()` returns the tail.

- [ ] **Step 1: Write failing tests** (with fakecodex):
  - `TestStartNewThreadSendsPromptAndSandbox`: the RPC log shows the thread/start params.
  - `TestResumeOmitsInstructions`
  - `TestSendStartsThenSteers`
  - `TestSteerFallsBackToStart`
  - `TestInterruptEndsTurnInterrupted`
  - `TestApprovalRequestUsesApprove`: allow → `accept`, deny → `decline`.
  - `TestApprovalCancelledOnClose`
  - `TestUnhandledServerRequestsAreAnswered`: requestUserInput, elicitation, tool/call and an unknown method each get an answer within 1 s.
  - `TestTranscriptEnvelopesIncomingOnly`: the system prompt text is not in the transcript.
  - `TestExitMidTurnEndsEvents`
  - `TestQuotaReadAtStart`
- [ ] **Step 2: Run** `go test -race ./internal/runtime/codex` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(codex): app-server session driver`.

### Task 6: Rendering Codex transcripts, and the session marker

**Files:**
- Create: `internal/render/codex.go`, `internal/render/marker.go`
- Modify:
  - `internal/render/items.go` and `render.go`: dispatch per line.
  - `proto/athena/v1/stream.proto`: comment only, for kind `runtime` and the Codex tool names.
- Test: `internal/render/codex_test.go`, `marker_test.go`, `items_test.go`, `render_test.go`

**Interfaces:**
- Line formats, sniffed once per line by its top-level keys:
  - `"codex"`: a Codex envelope.
  - `"athena"`: an Athena marker.
  - anything else: Claude stream-json, unchanged.
- The marker is `{"athena":"session","at":…,"runtime":"claude|codex","model":…,"assignment":…,"role":"implement|review","reason":"start|resume|failover|review"}`.
  - `MarkerLine(m Marker) []byte` is the writer the node uses (Task 7).
  - It renders to one `StreamItem{Kind:"runtime", Tool: runtime, Text: "<model> · <reason>"}`.
- `Items` for Codex envelopes (`at` from the envelope):

  | Codex line | Items |
  | --- | --- |
  | `thread/started` | `init` ("session <threadId> model <model>") |
  | `agentMessage` completed | `say` |
  | `reasoning` completed with summary text | `thinking` |
  | `commandExecution` started | `tool_call` with tool `exec_command`; `detail` `{"command","cwd"}` |
  | `commandExecution` completed | `tool_result` with `aggregatedOutput` stripped of ANSI, redacted and cut to `ResultMaxBytes`; `is_error` on a non-zero exit |
  | `fileChange` started | `tool_call` with tool `apply_patch`; `detail` `{"changes":[{"path","kind","diff"}]}`, each `diff` clipped as plan 4 clips detail values, with `_lines` |
  | `mcpToolCall` | `tool_call` with tool `mcp__<server>__<tool>` / `tool_result` |
  | `webSearch` | `tool_call` with tool `web_search`, `detail` `{"query"}` |
  | `plan` / `turn/plan/updated` | `tool_call` with tool `update_plan`, `detail` `{"steps":[{"step","status"}]}` |
  | `userMessage` | `user` items through the same `inputItems` parser as Claude echoes (the Input headers are runtime-neutral) |
  | `turn/completed` | `result` (status, duration; `is_error` unless completed) |
  | `error` (final) | `other` with `is_error` |
  | `item/started` of `agentMessage`/`reasoning`/`userMessage`, `turn/started`, `thread/tokenUsage/updated`, `account/rateLimits/updated`, `hook/*`, `serverRequest/resolved` | silent |
  | anything else | `other`, naming it |

- `Renderer.Render` (`athena logs`) gets the same three formats, with Kinds `init`, `say`, `tool`, `input`, `result` and `other`.

- [ ] **Step 1: Write failing tests:**
  - `TestCodexToolTurnItems`: from `tool-turn.jsonl`.
  - `TestCodexUserMessageRendersInputs`
  - `TestMarkerRendersRuntimeItem`
  - `TestMixedTranscriptRendersEveryRuntime`: Claude lines, a marker, Codex lines, a marker, Claude lines. No `other` items, and offsets increase.
  - `TestHugeCodexOutputIsClipped`: a 1 MiB `aggregatedOutput` with ANSI and a token-shaped string is cut, stripped and redacted.
  - `TestCodexSilentNotifications`
  - `TestLogsRenderCodex`
- [ ] **Step 2: Run** `go test -race ./internal/render` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(render): codex transcript lines and runtime markers`.

### Task 7: The node runs Codex sessions: hooks, approvals, attribution and MCP

**Files:**
- Modify:
  - `internal/node/node.go`: pick the runtime by name (from `Execution.Runtime`; `auto` means `claude` until Task 8), write the marker, pass `Approve`.
  - `internal/node/hookcmd.go`: `--runtime`.
  - `internal/node/socket.go`, `socket_types.go`: `HookRequest.Runtime`.
  - `internal/node/rules.go`: the Codex tool mapping and the attribution rule.
  - `internal/node/sandbox.go`
  - `internal/cli/cli.go`: `athena hook --runtime`.
- Test: `internal/node/rules_test.go`, `hookcmd_test.go`, `node_test.go` (Codex cases with fakecodex), `internal/server/server_test.go`

**Interfaces:**
- `athena hook [--runtime claude|codex] <event>`: the default is `claude`, so existing settings files keep working. The runtime goes on `HookRequest.Runtime`.
  - The output shapes are the same for both runtimes: Codex's `pre-tool-use.command.output` matches.
  - For Codex `PermissionRequest`, the output follows the `permission-request.command.output` schema embedded in 0.160.1. The implementer extracts it with `codex app-server generate-json-schema --experimental` or the capture script, and pins it in a golden test.
  - A Codex `PreToolUse` deny always carries a non-empty `permissionDecisionReason`. Codex rejects a deny without one.
- `NormalizeTool(runtime, tool string, input json.RawMessage) []ToolCall`, with `ToolCall{Tool string; Input json.RawMessage}`, maps Codex calls onto the existing rule vocabulary:

  | Codex tool | Rule vocabulary |
  | --- | --- |
  | `Bash`, `shell`, `local_shell`, `exec_command`, `unified_exec` | `Bash{command}`; an argv array is joined with POSIX shell quoting |
  | `apply_patch` | one `Write{file_path}` per path in `*** Add File:`, `*** Update File:`, `*** Delete File:` and `*** Move to:` lines |
  | `write_stdin` | allow |
  | `mcp__…` and `web_search` | unchanged |
  | other | unchanged (generic `Evaluate`: credential paths in any string value) |

  - `decide` evaluates every `ToolCall`, and the strictest verdict wins: deny > escalate > allow.
  - Claude input is returned as one unchanged call.
- `Approve` for Codex server requests is `(n *Node) approveRequest(ctx context.Context, s *session, req rt.ApprovalRequest) rt.Verdict`:
  - `command`: `decide(PermissionRequest, Bash{command})`
  - `file`: the strictest verdict over `Write{path}` for each path
  - `network`: escalate, rule `network_access`, summary "Network: <host>"
  - `permissions`: escalate, rule `codex_permissions`
  - Escalations become Approvals exactly as hooks do (timeout, expiry, the stop wait).
- Attribution rule (runtime-neutral, after the blocked commands): `attributionRule(ctx RuleContext, command string) (Decision, bool)` denies, with rule `attribution`, a `git commit` or `gh pr create|edit` whose message or body matches `(?i)co-authored-by:\s*(codex|claude)|generated with \[?(codex|claude code)|noreply@(openai|anthropic)\.com`.
  - Where it looks: `-m`/`--message` values, and `-F`/`--file`/`--body-file` files inside `TicketDir` up to 64 KiB, plus `--title` and `--body`.
  - Its reason: "remove the AI attribution trailer or footer: commits and pull requests carry none".
- Codex sessions also get `writeRule` protection for `.git/config`, `.git/config.worktree`, `.git/hooks/*` and `.athena/*` under the work dir: deny, rule `protected_path`. Claude's sandbox already protects these; Codex's cannot (Task 4).
- At every process start the node appends `render.MarkerLine` to the transcript, with reason `start` or `resume`; Task 9 adds `failover` and Task 10 adds `review`.
- The execution variables gain `ATHENA_RUNTIME=<name>`.
- The `athena mcp` server is unchanged. Codex reaches it as `mcp__athena__report`.

- [ ] **Step 1: Write failing tests:**
  - rules:
    - `TestCodexShellMapsToBash`: an argv array with spaces.
    - `TestApplyPatchEvaluatesEveryPath`: one path outside the dir escalates the call.
    - `TestApplyPatchToGitConfigDenied`
    - `TestAttributionTrailerDenied`: `-m`, `-F` and `gh pr create --body`.
    - `TestAttributionAllowsPlainMessage`
  - hookcmd:
    - `TestHookRuntimeFlagDefaultsToClaude`
    - `TestCodexDenyHasReason`
    - `TestCodexPermissionRequestOutputGolden`
  - node (fakecodex):
    - `TestCodexTicketRunsAndReports`: a fixture whose `mcpToolCall` is `report ready` reaches the Engine through the real MCP server.
    - `TestCodexHookDenyBlocksItem`: a fixture `fake:"hook"` for `cat ~/.ssh/id_rsa` is denied, and the item is dropped.
    - `TestCodexApprovalRequestBecomesApproval`: a `fake:"request"` command approval for `kubectl apply` creates an Approval; approving it answers `accept`.
    - `TestCodexResumeUsesThreadID`
    - `TestMarkerWrittenPerProcess`
    - `TestCodexNotReadyRefusesWithWhy`: a pinned `codex` Ticket on a logged-out fakecodex reports failed with the `Why`.
- [ ] **Step 2: Run** `go test -race ./internal/node ./internal/server` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` and `make bench`. `BenchmarkHookDecision` gains Codex payloads, and the p99 stays under 10 ms.
- [ ] **Step 5: Commit** `feat(node): codex sessions under athena's hooks, approvals and attribution rule`.

### Task 8: A per-Assignment runtime, quota tracking and the `auto` runtime

**Files:**
- Create: `internal/quota/tracker.go`, `internal/node/runtimes.go` (chooser)
- Modify:
  - `internal/core/model.go`, `engine.go`, `evals.go` (router Tickets use the default runtime), `stats.go` (per-runtime usage)
  - `internal/node/node.go`, `status.go`
  - `internal/server/config.go`, `wiring.go`, `control.go`
  - `internal/cli/cli.go` (`athena task --runtime`)
- Test: `internal/quota/tracker_test.go`, `internal/node/runtimes_test.go`, `internal/core/engine_test.go`, `stats_test.go`, `internal/node/node_test.go`

**Interfaces:**
- Core:
  - `const (RuntimeClaude = "claude"; RuntimeCodex = "codex"; RuntimeAuto = "auto")`
  - `Assignment.Runtime string`: pinned at creation, or set by `AssignmentStarted`.
  - `TaskOptions.Runtime`: empty means `Options.DefaultRuntime`; an unknown name is a `UserError`.
  - `Options.DefaultRuntime string`: empty means `claude`, which keeps plan 1–4 tests unchanged. The server passes the config's default, `auto`.
  - `Execution.Runtime` holds the Ticket's choice.
  - `AssignmentStarted(ticketID, assignmentID, sessionID, model, runtime string) error`
  - The `runtime` metric label is the Assignment's runtime.
  - `UsageDelta.Runtime string`
  - `Stats.ByRuntime map[string]RuntimeStats`, with `RuntimeStats{Turns int; ActiveTime time.Duration; InputTokens, OutputTokens, CacheReadTokens int64; CostUSD float64; Assignments int}`
- Quota:
  - `quota.New(path string, now func() time.Time) (*Tracker, error)`
  - `(*Tracker).Observe(runtime string, ws []rt.Window)`
  - `Hit(runtime string, h rt.LimitHit)`
  - `State(runtime string) State`, with `State{Windows []Window; LimitedUntil *time.Time; ReadAt time.Time; Measured bool}` and `Window{Name string; Used float64; ResetsAt *time.Time; Stale bool}`
  - `Headroom(runtime string) float64`: 1 − the largest `Used` over non-stale windows; 1 when not measured.
  - `OnChange(func())`
  - Rules:
    - A window whose `ResetsAt` has passed is `Stale` and does not count. A rejected `Limited` window sets `LimitedUntil` to its reset.
    - A `Hit` with no `ResetsAt` gives `LimitedUntil = now + 15 min`, doubling for each consecutive hit up to 2 h. A `Transient` hit gives now + 1, 2, 4 … min, up to 30 min.
    - A good `TurnEnd` on the runtime clears the consecutive count.
    - Persisted to `<stateDir>/quota.json` (0600) on every change: the last reading per runtime and window, with `read_at`. No history: that is plan 9.
- `node.Chooser` interface: `Choose(in ChoiceInput) Choice`, with `ChoiceInput{Ticket core.Ticket; Assignment core.Assignment; Ready map[string]rt.Info; Quota func(string) quota.State; Live map[string]int; Now time.Time}` and `Choice{Runtime string; Until time.Time; Why string}`.
  - A zero `Until` means launch now. `Runtime == ""` with a `Why` means fail.
  - `HeadroomChooser{Preferred string; AvoidAbove map[string]float64; MaxSessions map[string]int}`. The defaults are `Preferred "claude"` and `AvoidAbove {"5h": 0.85, "7d": 0.90}`, from nucleos' quota-brake defaults.
  - Its rules, in order:
    1. The Assignment's `Runtime` is set: that runtime. If it is not ready: fail with its `Why`. If it is limited: wait `Until` its reset.
    2. The Ticket's runtime is `claude` or `codex`: the same as 1.
    3. `auto`. The candidates are the ready runtimes that are not limited and below their `MaxSessions`. A review Assignment drops the implementer's runtime when another candidate is left.
       - Drop the candidates over any `AvoidAbove` window, unless that leaves none.
       - Pick the runtime of the Ticket's latest Assignment that ran, if it is a candidate. That keeps a warm session.
       - Else `Preferred`, if it is a candidate.
       - Else the largest `Headroom`.
       - No candidate: wait `Until` the earliest `LimitedUntil`, or now + 15 min when none is known. No runtime ready at all: fail with "no runtime is ready: <whys>".
- Node:
  - The runner calls the chooser before `prepare`. A wait re-queues the launch with `notBefore`, generalizing today's credential wait into `setLaunchWait(d, why)` and logging the `Why`.
  - `QuotaUpdate` events go to `Tracker.Observe`.
  - `previousSessionID(t, assignmentID, runtime, role string)` only resumes a session of the same runtime and role. A review Assignment always starts fresh.
  - `Status.Runtimes []RuntimeStatus`, with `RuntimeStatus{Name, Version, Why string; Ready, Tested bool; Quota quota.State; Live int}`. `OnStatus` fires on quota changes.
- Server config: `"runtimes": {"default": "auto", "preferred": "claude", "avoidAbove": {"5h": 85, "7d": 90}, "claude": {"maxSessions": 0}, "codex": {…, "maxSessions": 0}}`. Percents are in the file; 0 means no cap.
  - The control socket's `task` takes `runtime`, and `athena task --runtime`.

- [ ] **Step 1: Write failing tests:**
  - quota:
    - `TestObserveNormalizesAndPersists`
    - `TestStaleWindowIgnored`
    - `TestHitBackoffDoublesAndCaps`
    - `TestTransientBackoff`
    - `TestReloadAfterRestart`
  - chooser:
    - `TestAutoPrefersWarmRuntime`
    - `TestAutoSkipsLimited`
    - `TestAutoAvoidsAboveThresholdUnlessAll`
    - `TestAutoSkipsUnreadyRuntime`
    - `TestPinnedUnreadyRuntimeFailsWithWhy`
    - `TestPinnedLimitedWaits`
    - `TestReviewPrefersOtherRuntime`
    - `TestMaxSessionsPerRuntime`
  - core:
    - `TestAssignmentRecordsRuntime`
    - `TestDefaultRuntimeAuto`
    - `TestUnknownRuntimeRejected`
    - `TestStatsByRuntime`
  - node:
    - `TestAutoTicketLaunchesOnChosenRuntime`
    - `TestLaunchWaitsThenStarts`
    - `TestResumeOnlySameRuntimeSession`
- [ ] **Step 2: Run** `go test -race ./internal/quota ./internal/core ./internal/node ./internal/server` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(node): per-assignment runtime, quota tracking and the auto runtime`.

### Task 9: Usage-limit failover in the same worktree

**Files:**
- Create: `internal/core/limits.go`, `internal/node/checkpoint.go`, `internal/node/handoff.go`
- Modify: `internal/node/node.go`, `internal/runtime/claude/session.go` (detect limits), `internal/hub/views.go` (the limit entry)
- Test: `internal/core/limits_test.go`, `internal/node/checkpoint_test.go`, `handoff_test.go`, `node_test.go`, `internal/runtime/claude/events_test.go`

**Interfaces:**
- Claude limit detection, on the turn's `TurnEnd`: `Limit` is set when the turn saw a `rate_limit_event` with `status: "rejected"` (its `resetsAt`), or when the Result is an error whose text matches `(?i)usage limit|rate limit|hit your limit|limit reached`.
- Core:
  - `type UsageLimit struct{ Runtime string; ResetsAt *time.Time; Message string }`, with `Assignment.Limit *UsageLimit`
  - `const ReasonUsageLimit = "usage_limit"`
  - `(*Engine).UsageLimited(ticketID, assignmentID string, l UsageLimit) error`, on the running Assignment of an open Ticket:
    1. End it with `ReasonUsageLimit` and `Limit = &l`.
    2. `requeueUntaken`.
    3. Add a new pending Assignment with the old one's `Role`, `Runtime` pinned to `l.Runtime` when the Ticket's runtime is explicit, else unpinned.
    4. `stop` the old one and `start` the new one.
    - It never files a Report, never sets an Attention, and leaves `WaitsForPerson` false.
    - Any other target changes nothing.
- Node, on a `TurnEnd` with `Limit`:
  1. `Tracker.Hit`.
  2. `checkpoint`.
  3. `eng.UsageLimited(…)` instead of `r.fail`.
  4. The runner ends as if stopped.
- `checkpoint(workDir, ticketID string, env []string) (sha string, err error)`:
  - Only for a git worktree. It goes through a temporary index (`GIT_INDEX_FILE=<tmp>`): `git add -A` (which respects `.gitignore`), then `git write-tree`, then `git commit-tree -p HEAD -m checkpoint`, then `git update-ref refs/athena/<lowercased id>/checkpoint <sha>`.
  - The branch, the real index and the files are untouched.
  - It uses `gitIn` with no shell, a minimal env and a 30 s timeout.
  - A failure is logged and does not block the failover.
- `handoffNote(h Handoff) string`, with `Handoff{Ticket core.Ticket; Previous core.Assignment; Inputs string; Reports []core.Report; GitLog, DiffStat, Untracked string; LastTurn string}`:
  - **When:** sent after the preamble in a new session (no resume id) whose Ticket has an earlier Assignment that ran on another runtime.
  - **Opening:** `handoffOpening = "You are continuing this Ticket from a session on <runtime> that stopped at its usage limit. Its work in progress is in your directory; nothing was lost. What it knew follows."`
  - **Inputs:** all of the Ticket's Inputs, rendered by `renderInputs` (quoted), the last 20.
  - **Reports:** the last 3 Report summaries, at most 2 KiB each.
  - **Git:** `git log --oneline <base>..HEAD -20`, `git diff --stat HEAD`, and the count of untracked files.
  - **Last turn:** the previous session's last `say` text, from the transcript's last 1 MiB, at most 4 KiB.
  - At most 32 KiB in all; cuts are marked `[…]`.
- The new process's marker has reason `failover`.
- Timeline:
  - The limited Assignment's end entry is `assignment`/`limited`, with `subject` the runtime and `text` the RFC 3339 reset (or empty).
  - The next start entry carries its runtime (Task 11's field).

- [ ] **Step 1: Write failing tests:**
  - core:
    - `TestUsageLimitedRequeuesOnNewAssignment`
    - `TestUsageLimitedNeverPersonStage`
    - `TestPinnedTicketKeepsRuntimeAfterLimit`
    - `TestUsageLimitedIgnoresStaleTarget`
  - claude: `TestRejectedRateLimitSetsTurnLimit`, `TestUsageLimitResultTextSetsLimit`
  - checkpoint:
    - `TestCheckpointKeepsBranchIndexAndFiles`: includes an untracked file and a gitignored one.
    - `TestCheckpointRefPointsAtWIP`
  - handoff: `TestHandoffNoteSectionsAndCap`, `TestHandoffQuotesInputs`
  - node:
    - `TestClaudeLimitFailsOverToCodex`: fakeclaude replays a rejected limit; then fakecodex starts in the **same work dir**, its first `turn/start` text starts with `handoffOpening`, and the checkpoint ref exists.
    - `TestBothLimitedWaitsForEarliestReset`: no launch before the reset, one launch after, stage `working`.
    - `TestUnknownResetBacksOff`
    - `TestNoTwoSessionsInOneWorktree`: the old process has exited before the new one spawns (pid files).
- [ ] **Step 2: Run** `go test -race ./internal/core ./internal/node ./internal/runtime/claude` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(node): usage-limit failover to the other runtime in the same worktree`.

### Task 10: Implement + review pairing

The minimal useful form of §6.1a's *Implement + review*:
- When the implementer files `ready`, a read-only session reviews the branch, on the other runtime when it is ready.
- It returns structured findings with a closed verdict. The structure is taken from nucleos' reviewer output: severity-tagged findings plus `approved|changes`.
- On `changes`, the implementer's own session resumes with the findings. There are at most 2 rounds.

**Files:**
- Create: `internal/core/review.go`, `internal/node/review.go`, `prompts/reviewer.md`
- Modify:
  - `internal/core/model.go`, `engine.go` (the `FileReport` ready path)
  - `internal/node/mcpcmd.go`, `workertools.go` (`--role review`, the `review` tool)
  - `internal/node/rules.go` (`RuleContext.ReadOnly`), `node.go`
  - `prompts/prompts.go` (the `reviewer` part)
  - `internal/policy/settings.go` (pairing default), `internal/server/wiring.go`
- Test: `internal/core/review_test.go`, `internal/node/review_test.go`, `rules_test.go`, `mcp_test.go`, `prompts/prompts_test.go`, `internal/policy/settings_test.go`

**Interfaces:**
- Core:
  - `const RoleReview = "review"`, with `Assignment.Role string` (empty means implement).
  - `Ticket.Pairing string` (`""` | `"review"`) and `TaskOptions.Pairing`.
  - `Options.Pairing func() PairingDefaults`, with `PairingDefaults{Mode string; MaxRounds int}`. The defaults are `""` and 2, and router Tickets use them.
  - `ReviewState{Round int; Implementer, Runtime string; Held Report}`, on `Ticket.Review`.
  - `Stats.ReviewRounds int`
  - `InputReview` (Input kind) and `ReasonReview`, `ReasonReviewed`.
- The ready path. A `ready` Report from an implement Assignment of a `review` Ticket with `Stats.ReviewRounds < MaxRounds`:
  1. Records the Report on the Assignment.
  2. Sets `Ticket.Review` with the Report held.
  3. Ends the Assignment with `ReasonReview`.
  4. Starts a pending `RoleReview` Assignment (unpinned; the chooser prefers the other runtime).
  - No reply, delivery or Ticket end happens yet. The stage is `working`.
- `(*Engine).FileReview(ticketID, assignmentID, verdict, findings string) error`, from the running review Assignment:
  - `verdict` is `approved` or `changes`.
  - `findings` lines match `^- (\[(high|medium|low)\] \S+:\d+ — .+|none)$`.
  - **Consistency:** a high or medium finding requires `changes`, and `- none` requires `approved`. A violation is a `UserError` the tool returns, so the reviewer corrects it.
  - It ends the review Assignment with `ReasonReviewed`, sets `Assignment.ReviewOutcome = &ReviewOutcome{Verdict, First string}` (`First` is the first finding line, or `failed` for a review failure), increments `ReviewRounds`, and clears `Ticket.Review`.
  - **On `approved`, or when the rounds are used up**, the held Report is applied as if the implementer had filed it now:
    - without a pull request, the Ticket ends completed, with the usual result reply through the Outbox;
    - with one, a new implement Assignment is created already `running` and idle (`Runtime`, `SessionID`, `Model` and `Report` copied from the implementer, `IdleAt` now), so `HandedOff()` holds. Follow-ups resume it as a paused session, with no model call now.
  - **On `changes`**, a new pending implement Assignment is pinned to the implementer's runtime, with an `InputReview` Input: "Review (round N, <runtime>) requested changes:\n<findings>", quoted. The implementer's next `ready` triggers the next round.
- A review Assignment that reports, crashes past its restarts, or ends its turn twice without calling `review` is a **review failure**. The held Report is applied as on `approved`, and the timeline entry is `review`/`failed`. Review never gives a person stage.
- Node:
  - Role review means MCP `athena mcp --role review` offers only `review(verdict, findings)` (with the consistency check above), and `RuleContext.ReadOnly` is set.
    - Under `ReadOnly`, every write is denied, rule `read_only`: write tools, Bash write targets, `git commit|push|merge|rebase|reset|checkout|switch|stash`, and `gh pr create|edit|merge`.
  - Codex uses `ReadOnly` → sandbox `read-only`. The Claude sandbox gets `AllowWrite: []`, temp only.
  - The first message is the reviewer prompt part plus `base`, the branch, `git diff --stat <base>...HEAD`, and the held Report's summary.
  - The nudge for role review is "Your turn ended without a review. Call the review tool now."
  - The marker has reason `review`.
- `prompts.Parts()` gains `reviewer`, last in order. `reviewer.md` says what to read (the diff, the tests the repository runs), the output format and the verdict rule, and to never edit, commit or push.
- Policy settings JSON gains `"pairing": {"review": "off"|"on", "maxRounds": 2}`, with unknown fields preserved as today.

- [ ] **Step 1: Write failing tests:**
  - core:
    - `TestReadyStartsReviewWhenPaired`
    - `TestReadyPassesThroughWithoutPairing`
    - `TestApprovedAppliesHeldReadyWithoutPR`: the Ticket ends completed, and the reply is enqueued once.
    - `TestApprovedWithPRParksImplementer`: `HandedOff`, same session id.
    - `TestChangesResumesImplementerWithFindings`
    - `TestRoundsCapAppliesHeldReady`
    - `TestReviewFailureNeverPersonStage`
    - `TestInconsistentVerdictRejected`
  - node:
    - `TestReviewRunsReadOnlyOnOtherRuntime`: implementer on fakeclaude, reviewer on fakecodex in the same dir; the codex RPC log shows `sandbox: read-only`; a reviewer write attempt is denied with `read_only`.
    - `TestReviewToolOnlyForReviewRole`
    - `TestReviewNudgeThenFailure`
  - prompts: `TestPartsIncludeReviewer`
  - policy: `TestPairingDefaultRoundTrips`
- [ ] **Step 2: Run** `go test -race ./internal/core ./internal/node ./prompts ./internal/policy` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(core): implement and review pairing across runtimes`.

### Task 11: Views and commands for runtimes

**Files:**
- Modify:
  - `proto/athena/v1/state.proto`, `commands.proto`, `stream.proto` (comments), then regenerate.
  - `internal/hub/views.go`, `hub.go`
  - `internal/server/hubsource.go`, `dispatch.go`, `control.go`
  - `internal/web/conn.go`: node status deltas on quota change.
- Test: `internal/hub/views_test.go`, `internal/server/dispatch_test.go`, `hubsource_test.go`

**Interfaces:**
- Proto (field numbers fixed):
  - `TicketSummary`:
    - `runtime = 18`: now the runtime of the latest Assignment that started, else the Ticket's choice.
    - `string runtime_choice = 19` (`claude` | `codex` | `auto`)
    - `string pairing = 20`
    - `uint32 review_rounds = 21`
  - `TimelineEntry`: `string runtime = 7`, set on assignment and review entries.
    - New assignment subkinds: `restart`, `resumed` (Task 1), `limited` (Task 9) and `review` (the review Assignment's start).
    - New kind `review`, with subkinds `approved`, `changes` and `failed`. Its `text` is the first finding line, and its `subject` the runtime.
  - `message QuotaWindow { string name = 1; float used = 2; google.protobuf.Timestamp resets_at = 3; bool stale = 4; }`
  - `message RuntimeStatus { string name = 1; string version = 2; bool ready = 3; bool tested = 4; string why = 5; repeated QuotaWindow windows = 6; google.protobuf.Timestamp limited_until = 7; google.protobuf.Timestamp read_at = 8; bool measured = 9; uint32 live = 10; }`
  - `NodeStatus.runtimes = 7`. `claude_version` is still filled, for older clients.
  - `SubmitTask`: `string runtime = 5; bool review = 6;`. An unknown runtime is `invalid`. `review` false means the Policy default applies.
  - `StreamItem.kind` documents `runtime`, and `tool` documents the Codex tool names from Task 6.
- Hub:
  - Timeline entries come from Assignments and `Interruptions`, plus a review entry at each `ReasonReviewed` end, read from Task 10's `Assignment.ReviewOutcome`.
  - `WhyMaxRunes = 200` and `redact.String` apply to `RuntimeStatus.why`.
  - Quota changes publish a node delta, at most once per second.

- [ ] **Step 1: Write failing tests:**
  - `TestSummaryRuntimeIsLatestStarted`
  - `TestSummaryRuntimeChoiceAndPairing`
  - `TestTimelineRestartResumeLimitReviewEntries`
  - `TestNodeStatusRuntimesWithQuota`
  - `TestRuntimeWhyRedactedAndCut`
  - `TestSubmitTaskRuntimeAndReview`
  - `TestSubmitTaskUnknownRuntimeInvalid`
  - `TestQuotaDeltaThrottled`
- [ ] **Step 2: Run** `go test -race ./internal/hub ./internal/server ./internal/web` → FAIL.
- [ ] **Step 3: Implement**, then run `make proto`.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(hub): runtime, quota, failover and review in views and commands`.

### Task 12: Dashboard: runtime picker, badges, timeline and quota

**Files:**
- Create: `web/src/features/new-task/RuntimePicker.tsx`, `web/src/ui/athena/RuntimeBadge.tsx`, `web/src/features/nodes/QuotaBars.tsx`, `web/src/lib/text/runtime.ts`
- Modify:
  - `web/src/features/new-task/NewTask.tsx`
  - `web/src/features/tickets/TicketList.tsx`, `Filters.tsx`, `Facts.tsx`, `timelineModel.ts`
  - `web/src/lib/store/selectors.ts` (runtime filter)
  - `web/src/lib/commands/actions.ts`
  - `web/src/features/nodes/Nodes.tsx`, `web/src/features/settings/Execution.tsx`
  - the sidebar node block
- Test: the `*.test.tsx` beside each, plus `web/e2e/new-task.spec.ts`, `tickets.spec.ts`, `shell.spec.ts`

**Interfaces:**
- `runtimeLabel(name: string): string` returns `Claude Code`, `Codex` or `Auto`; anything else is shown as it is.
- `RuntimePicker({ value, onChange, runtimes: RuntimeStatus[] })`:
  - A toggle group: Auto · Claude Code · Codex.
  - A runtime that is not ready is disabled, with its `why` in a tooltip and in `aria-describedby`.
  - The default is `auto`.
  - Below it, "Review on the other runtime" (a switch). It is shown when two runtimes are ready, and sends `review`.
- `submitTask({ text, repo?, dir?, model?, runtime?, review? })`.
- `RuntimeBadge({ runtime, choice })`:
  - Mono; for example `Codex`, or `Codex · auto` when the choice was `auto`.
  - Shown on the Ticket list row beside the model, and in Facts as the "Runtime" row, plus "Review rounds" when `pairing` is set.
  - The Filters get a `runtime` URL key (`claude` | `codex`), with "All runtimes".
- `timelineModel` copy: exactly the Global Constraints table.
  - The start title becomes `Started work · <runtime> · <model>`, or `Continued on <runtime> · <model>` right after a `limited` entry.
  - `limited` and `restart` are amber; `resumed` and `review approved` are green; `changes` is neutral; `failed` is amber.
- `QuotaBars({ status: RuntimeStatus })`, on the node card, one block per runtime:
  - name, version ("untested" in amber when not `tested`), and Ready, or its `why`
  - one bar per window (`5h`, `7d`) with the percent and "resets HH:MM"
  - "Limited until HH:MM" in amber when limited
  - "Not measured yet" when not `measured`
  - "as of N min ago" when `read_at` is more than 10 min old
  - Bars use `--line` and `--text`, with amber at ≥ 85%. Orange never, because a limit needs nobody.
- Sidebar node block: one compact line per runtime, `Codex 42% · 5h`, linking to Nodes.
- Settings › Execution lists both runtimes' versions.
- Budget: everything here is in the entry chunk, at most 4 KB gzipped added.

- [ ] **Step 1: Write failing tests:**
  - `runtime picker disables an unready runtime and says why`
  - `new task sends runtime and review`
  - `review switch only with two ready runtimes`
  - `badge shows runtime and auto choice`
  - `runtime filter matches list length`
  - `timeline copy for restart, resumed, limited, continued and review verdicts`
  - `quota bars: unmeasured, stale age, limited, amber at 85`
  - `no orange in quota`
  - e2e: New task with the picker; a list filtered by runtime; the node card with fakecodex quota; screenshots in every state; axe clean.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` and `make e2e` → PASS, and the bundle line shows the entry ≤ 194 KB.
- [ ] **Step 5: Commit** `feat(web): runtime picker, runtime badges, failover and review timeline, quota on the node card`.

### Task 13: Dashboard: Codex items in the session view

**Files:**
- Create: `web/src/features/session/tools.codex.ts`, `web/e2e/codex-session.spec.ts`, `web/e2e/fixtures/codex-*.jsonl`
- Modify:
  - `web/src/features/session/tools.ts`: a dispatch by tool name.
  - `ToolCall.tsx`, `SessionView.tsx`: the `runtime` divider.
  - `web/e2e/global-setup.ts`: build `fakecodex` into `bin/e2e`; config `runtimes.codex` with `--codex bin/e2e/fakecodex`; `CODEX_HOME` under the temp state dir; fakecodex env pointing at `<state>/codex-fixture-<ticket>.jsonl`.
  - `web/e2e/harness.ts`: `codexFixture(ticket, lines)`.
- Test: `web/src/features/session/tools.codex.test.ts`, `SessionView.test.tsx`, `web/e2e/codex-session.spec.ts`

**Interfaces:**
- `codexToolView(item: StreamItem, input: ToolInput): ToolView | undefined`, reached from `toolView` for the Codex tool names:

  | Tool | View |
  | --- | --- |
  | `exec_command` | kind `bash`: label "Ran", the command (paths shortened), cwd when not the worktree root |
  | `apply_patch` | kind `edit` (or `write` when every change is an add): label "Edited" / "Created"; target the single path or "N files"; counts from the `diff` values (about when clipped) |
  | `web_search` | kind `web`, target the query |
  | `update_plan` | kind `todo`, the steps with their status |
  | `mcp__athena__*` | the existing presenters |

  - `tool_result` of `exec_command` shows its exit status through `errorLine`.
- The `runtime` item renders as a divider: "Codex · gpt-5.5", plus "· continued after a usage limit" for `failover` and "· review" for `review`. It is a Mono hairline row, not a card.
- This code stays in the session-view lazy chunk; the sentinel test still passes.

- [ ] **Step 1: Write failing tests:**
  - `exec_command shows the command and exit status`
  - `apply_patch counts lines and names one or many files`
  - `update_plan lists steps`
  - `runtime divider says failover and review`
  - `claude presenters unchanged` (the existing `tools.test.ts` passes)
  - e2e `codex-session.spec.ts`: a Codex Ticket on fakecodex streams a command, a patch, a plan and a `report ready`; a mixed Ticket (fakeclaude, then a failover, then fakecodex) shows the divider; screenshots at all widths and themes; axe clean; no horizontal scroll at 320 px.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` and `make e2e` → PASS. The bundle budget passes, with the session chunk lazy.
- [ ] **Step 5: Commit** `feat(web): codex items and runtime dividers in the session view`.

### Task 14: Codex setup docs, the multi-runtime flow and the budgets

**Files:**
- Create: `docs/dev/codex.md`, `web/e2e/multi-runtime.spec.ts`
- Modify: `docs/dev/smoke-test.md` (a manual Codex session), `README.md` (link), `docs/dev/dashboard.md` (feature notes), `bench/bench_test.go` (Codex hook payloads, if Task 7 did not add them)
- Test: `web/e2e/multi-runtime.spec.ts`, `bench`

**Interfaces:**
- `docs/dev/codex.md` sections, in order:
  1. What Athena runs: `codex app-server`, one process per session; the supported versions.
  2. Install and log in: `athena codex login` and why it is a separate login. Athena's home is `<stateDir>/codex`, and `~/.codex` is never read or written.
  3. `config.json` `runtimes`: default `auto`, `preferred`, `avoidAbove`, `maxSessions`, `model`.
  4. Hooks and trust: what Athena trusts, and what refuses to start.
  5. Sandbox and its deviations from Claude Code: no denied sub-paths, no domain allowlist, and the protected paths through the rules.
  6. Attribution: Codex's workspace trailer, and the `attribution` rule.
  7. Quota and failover: the meaning of the bars, the backoff, and where checkpoints go (`refs/athena/<id>/checkpoint`).
  8. Review pairing: the per-Ticket switch, the Policy default and the rounds.
  9. Fixtures: `capture-codex-fixtures.sh schema` (free) and `turns` (spends quota, needs consent).
  10. Troubleshooting, from `athena codex status`.
- `multi-runtime.spec.ts` (desktop and phone), one `auto` Ticket with review on:
  1. It starts on Claude (fakeclaude).
  2. A fixture hits a rejected limit.
  3. The timeline says "Usage limit reached on Claude Code" and "Continued on Codex".
  4. The Codex session continues (fakecodex) and reports ready.
  5. The review runs on Claude Code (its limit fixture has reset) and requests changes.
  6. The implementer resumes on Codex and reports ready.
  7. Round 2 approves.
  8. The Ticket is "Ready for review".
  - Throughout: no person stage until the end (polled through the store), screenshots of each step, axe clean, no console errors.

- [ ] **Step 1: Write the spec and the docs.**
- [ ] **Step 2: Run** `make check`, `make e2e` and `make bench` → PASS. All the spec §10.3 budgets hold with both runtimes registered: hook p99 < 10 ms, idle RSS.
- [ ] **Step 3: Owner check** (the owner runs it, and it spends a little of both quotas):
  1. `athena codex login`, then `athena codex status` shows ready.
  2. Run one `codex` task and one `auto` task with review on, on a scratch repository.
  3. Check the commits and the PR carry no attribution.
  4. Watch the node card's bars move.
  - If the owner hasn't run it, record "not run" with the reason in the report and in the ledger.
- [ ] **Step 4: Commit** `docs(codex): setup guide; test(e2e): multi-runtime flow`.

---

## Deferred to later plans (decided here)

- **Plan 6 (multi-node):**
  - quota is per account, not per node, so two nodes on one account must merge their readings (keyed by `account/read`'s account id and the Claude org) before the dispatcher chooses
  - §16.1 item 3's per-account concurrency limit across nodes; plan 5 has only the per-node `maxSessions`
  - pushing `refs/athena/<id>/checkpoint` to the server's mirror (§7.4)
  - runtime readiness as a hard dispatch constraint across nodes
- **Plan 7:** *Split* pairing (child Tickets pinned to the other runtime, merged back by the lead), on top of this plan's per-Assignment runtime.
- **Plan 8:** Codex on a local model (a custom `model_providers.athena_local`) and Claude Code on local, as runtime choices. Plan 8 also verifies the Codex wire API against vLLM.
- **Plan 9:**
  - per-runtime spend and the runtime × model scoreboard from `Stats.ByRuntime`
  - a quota history series with retention, and quota-crossing warnings said once per window (nucleos' design)
  - Codex's `[otel]` export into the OTLP receiver
- **Plan 10:**
  - the kill switch must also stop auto-resume after a restart (a paused server resumes nothing)
  - a domain allowlist for Codex workers of `stranger` Tickets, once Codex's network proxy is stable; until then such Tickets should be pinned to `claude`
- **Later, behind the reviewer prompt:** Codex's native `review/start` as a reviewer strategy, and a Claude Code headroom probe that needs no session (none exists in 2.1.280).
- **Plan 4's deferred auto-resume item** is done here (Task 1), not in plan 10.
