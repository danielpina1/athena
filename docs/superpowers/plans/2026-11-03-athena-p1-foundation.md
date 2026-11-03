# Athena Plan 1: Foundation (single-machine core + Claude Code) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `athena server` on one machine turns owner tasks submitted from the CLI into Tickets, each worked by a headless Claude Code session in its own directory, with hooks enforcing rules, Approvals for risky actions, Reports driving the reference's stage model, and recovery after a session crash or server restart.

**Architecture:** One Go binary. `internal/core` is a faithful port of the reference's Ticket/Assignment/Input/Report/stage model behind the reference's ports (`Repositories`, `Executor`), with synchronous commands, one SQLite transaction per command, and external work enqueued for after commit. `internal/node` implements `Executor` by driving `claude -p` in stream-json mode, serving hooks and an MCP server to each session over a Unix socket. The CLI talks to the server over a local Unix socket with JSON lines (the network protocol is plan 3).

**Tech Stack:** Go 1.27, `modernc.org/sqlite`, `github.com/modelcontextprotocol/go-sdk`, `log/slog`, Claude Code CLI 2.1.x.

**Spec:** `docs/superpowers/specs/2026-11-03-athena-design.md` (read §4–§6, §11, §16.1). Roadmap: `docs/superpowers/plans/2026-11-03-athena-roadmap.md`. Behavioral reference: https://github.com/deepseek-ai/dsh-superagent-prototype (read with `gh api repos/deepseek-ai/dsh-superagent-prototype/contents/<path> --jq .content | base64 -d`).

## Global Constraints

- Module path `github.com/danielpina1/athena`; single binary `athena` built from `cmd/athena`.
- `CGO_ENABLED=0 go build ./...` must succeed (pure-Go SQLite).
- `make check` = `go vet ./...` + `go test -race ./...` + `CGO_ENABLED=0 go build ./...`; must pass at the end of every task.
- Standard library first; the only third-party Go modules in this plan are `modernc.org/sqlite` and `github.com/modelcontextprotocol/go-sdk`.
- State dir default `~/.athena` (flag `--state-dir`); work root default `<stateDir>/work`; files containing state or secrets mode 0600, dirs 0700.
- SQLite: WAL, `synchronous=FULL`, one transaction per Engine command.
- Domain names follow the reference exactly: Ticket, Assignment, Input, Report (`progress|waiting|ready|blocked|failed|dropped`), Attention, stage (`person/delivery|answer|review|blocked|failed`, `working[/queued]`, `idle`, `ended`).
- Execution variables use the `ATHENA_` prefix: `ATHENA_DIR`, `ATHENA_NAME`, `ATHENA_TICKET`, `ATHENA_BLOCKED_COMMANDS`, `ATHENA_SOCKET`.
- Logging: `log/slog` JSON handler; every line about a Ticket carries `ticket`, `assignment`; never log tool inputs that match credential paths, env values, or tokens.
- Hooks fail closed: any error reaching the node yields a deny.
- Workers run as the owner's OS user (spec §11 default).
- Keep the reference's MIT notice in `NOTICE`.

## Review Focus

1. **Claude process dies mid-turn** → the Assignment restarts by resuming the same Claude session id with a recovery note; the Ticket never sits in `working` with no process. (Task 8)
2. **Server restarts while Assignments run** → on startup, running Assignments resume with a recovery note and undelivered Inputs are redelivered; nothing is lost or duplicated. (Task 9)
3. **Hook runs while the node socket is unavailable** → deny with a reason, never allow. (Task 6)
4. **Several Inputs arrive while the session is mid-turn** → all delivered in order, exactly once, each marked delivered. (Task 8)
5. **Path escape**: a write to `../x`, an absolute path outside the Ticket dir, or through a symlink pointing outside → denied. (Task 6)

---

## File Structure

```
cmd/athena/main.go              subcommand dispatch: server, task, status, logs, reply, stop, retry, approvals, approve, deny, hook, mcp, version
internal/core/model.go          domain types (ported subset)
internal/core/stage.go          TicketStage, CanRetry
internal/core/ports.go          Repositories, Executor, Clock, Event
internal/core/engine.go         Engine: owner task submission, executor callbacks, person commands
internal/core/approvals.go      Approval type + engine commands
internal/store/store.go         Open, migrations, Tx, kv
internal/store/tickets.go       ticket/trigger/decision/approval repositories
internal/runtime/claude/events.go   stream-json event types + parser
internal/runtime/claude/session.go  process driver
internal/runtime/claude/config.go   settings + MCP config generation
internal/node/node.go           Executor implementation, slots, sessions, recovery
internal/node/socket.go         Unix socket server (hooks + MCP backend), JSON lines
internal/node/rules.go          PreToolUse/PermissionRequest policy
internal/node/hookcmd.go        `athena hook` client side
internal/node/mcpcmd.go         `athena mcp` stdio MCP server
internal/server/server.go       wires store+engine+node, startup recovery, control socket
internal/server/control.go      control socket protocol (CLI ⇄ server)
internal/cli/cli.go             CLI commands over the control socket
prompts/worker.md               adapted from reference prompts/worker.md + shared.md
testdata/claude/*.jsonl         recorded stream-json fixtures
testdata/fakeclaude/main.go     fake `claude` binary for tests
```

---

### Task 1: Repository scaffold

**Files:**
- Create: `go.mod`, `cmd/athena/main.go`, `Makefile`, `LICENSE` (MIT, owner), `NOTICE` (reference's MIT notice), `README.md` (one paragraph + link to spec), `.gitignore` (append `bin/`, `/athena`), `docs/reference/` (copy reference `docs/000–008` verbatim with a header line "From deepseek-ai/dsh-superagent-prototype, MIT")
- Test: `cmd/athena/main_test.go`

**Interfaces:**
- Produces: `func run(args []string, stdout, stderr io.Writer) int` in `cmd/athena/main.go` (all subcommands dispatch through it); `var version = "dev"` (set by `-ldflags`).

- [ ] **Step 1: `git init`, add remote `origin https://github.com/danielpina1/athena.git`, branch `main`.**
- [ ] **Step 2: Write failing test** `TestVersion`: `run([]string{"version"}, &out, &err)` returns 0 and `out` == `"athena dev\n"`; `TestUnknownCommand`: `run([]string{"nope"})` returns 2 and stderr contains `unknown command "nope"`.
- [ ] **Step 3: Run** `go test ./cmd/athena` → FAIL (no module/func).
- [ ] **Step 4: Implement** `go mod init github.com/danielpina1/athena`, `run` with a `switch` over subcommands (only `version` implemented; others print `not implemented` and return 1 until their tasks), `Makefile` targets `check`, `build` (`CGO_ENABLED=0 go build -o bin/athena ./cmd/athena`).
- [ ] **Step 5: Run** `make check` → PASS.
- [ ] **Step 6: Commit** `chore: scaffold athena module` (include the spec, roadmap and this plan in the first commit).

### Task 2: Domain model and stages

**Files:**
- Create: `internal/core/model.go`, `internal/core/stage.go`
- Test: `internal/core/stage_test.go`

Port from reference `src/core/model.ts:211-315` and `src/core/stage.ts`. Times are `time.Time` (UTC). Optional fields are pointers or zero values.

**Interfaces:**
- Produces:
  - `type ReportState string` constants `ReportProgress, ReportWaiting, ReportReady, ReportBlocked, ReportFailed, ReportDropped`
  - `type AssignmentState string` constants `AssignmentPending, AssignmentRunning, AssignmentEnded`
  - `type Report struct{ State ReportState; Summary string; At time.Time }`
  - `type Assignment struct{ ID string; State AssignmentState; CreatedAt time.Time; StartedAt, EndedAt *time.Time; EndedReason string; Report *Report; DeliveredAt, IdleAt *time.Time; SessionID string }` (`SessionID` is new: the runtime session to resume)
  - `type InputKind string` constants `InputTrigger, InputText, InputNote, InputResumed`; `type Input struct{ ID string; Kind InputKind; At time.Time; TriggerID, Text string; Human bool; ReadAt *time.Time; DeliveredTo string; DeliveredAt *time.Time }`
  - `type Attention struct{ ID, Question, TriggerID string }`
  - `type TicketOutcome string` (`completed|merged|closed|dropped|stopped`), `type TicketEndBy string` (`executor|human`), `type Ended struct{ At time.Time; By TicketEndBy; Outcome TicketOutcome; Reason string }`
  - `type Mandate struct{ Strength string; PolicyVersion string; Actor ActorRef; Scope string }`, `type ActorRef struct{ Source, ID string }`, `type ChannelRef struct{ Source, Thread string; Address map[string]string }`
  - `type Execution struct{ Name, Directory, Assignment string; Runtime string; Node string }`
  - `type Ticket struct{ ID, Title, DecisionID string; Mandate Mandate; Channels []ChannelRef; Ended *Ended; Attention *Attention; Assignments []Assignment; Inputs []Input; Undelivered []string; CreatedAt, UpdatedAt time.Time; Origin Origin; Execution *Execution; Delivery *DeliveryFailure }` with `Origin{TriggerID string; TriggerIDs []string}`, `DeliveryFailure{Error string; At time.Time}`
  - `type Trigger struct{ ID, Source string; Actor ActorRef; Human bool; At time.Time; Text string }`; `type Decision struct{ ID, Kind, TicketID string; TriggerIDs []string; At time.Time; Mandate *Mandate }`
  - `func (t *Ticket) LatestAssignment() *Assignment`, `func (t *Ticket) WaitsForPerson() bool` (port `waitsForPerson`: latest report blocked/failed and no human Input after it)
  - `type Stage struct{ Stage string; Action string }`; `func TicketStage(t *Ticket) Stage`; `func CanRetry(t *Ticket) bool`

- [ ] **Step 1: Write failing table test** `TestTicketStage` with one case per row of reference `docs/003-execution.md` "Ticket stages" table plus `ended`, in table order, including: ended → `{ended}`; delivery set → `{person, delivery}`; attention → `{person, answer}`; latest report ready → `{person, review}`; report blocked with no later human input → `{person, blocked}`; report failed then a human Input after it → `{working}` (not person); no assignment → `{working, queued}`; pending assignment → `{working, queued}`; report waiting → `{idle}`; assignment ended → `{idle}`; running, report progress → `{working}`. Also port every case in reference `tests/stage.spec.ts`. `TestCanRetry`: true for blocked-waiting-for-person and for delivery failure; false for ended.
- [ ] **Step 2: Run** `go test ./internal/core -run 'Stage|CanRetry'` → FAIL.
- [ ] **Step 3: Implement** types and functions above.
- [ ] **Step 4: Run** → PASS; `make check` → PASS.
- [ ] **Step 5: Commit** `feat(core): port ticket model and stages`.

### Task 3: SQLite store

**Files:**
- Create: `internal/store/store.go`, `internal/store/tickets.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: core types (Task 2).
- Produces:
  - `func Open(path string) (*Store, error)` — creates parent dir 0700, file 0600, sets `PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON`, applies migrations, records `schema` in `kv`.
  - `func (s *Store) Close() error`
  - `func (s *Store) Tx(fn func(tx *Tx) error) error` — commit on nil, rollback on error or panic (re-panic).
  - On `*Tx`: `GetTicket(id string) (*core.Ticket, error)` (nil,nil if absent), `PutTicket(t *core.Ticket) error`, `OpenTickets() ([]*core.Ticket, error)`, `PutTrigger(*core.Trigger) error`, `PutDecision(*core.Decision) error`, `KVGet(key string, v any) (bool, error)`, `KVSet(key string, v any) error`.
  - Tables (schema 1): `kv(key TEXT PRIMARY KEY, value TEXT NOT NULL)`, `tickets(id TEXT PRIMARY KEY, ended INTEGER NOT NULL, updated_at TEXT NOT NULL, doc TEXT NOT NULL)` + index on `ended`, `triggers(id PK, doc)`, `decisions(id PK, doc)`. Aggregates stored as JSON `doc` (as the reference stores JSON rows).
  - `*Store` implements `core.Repositories` (Task 4) by exposing `Tx`.

- [ ] **Step 1: Write failing tests**: `TestRoundTripTicket` (put → get equal, incl. nested assignments/inputs/times); `TestReopenPersists` (close, reopen same path, ticket still there, `schema`==1); `TestTxRollback` (fn returns error → ticket absent); `TestTxPanicRollsBack`; `TestOpenTicketsExcludesEnded`; `TestFileModes` (db file `0600`).
- [ ] **Step 2: Run** `go test ./internal/store` → FAIL.
- [ ] **Step 3: Implement** with `database/sql` + `modernc.org/sqlite` (driver name `sqlite`), one `*sql.DB` with `SetMaxOpenConns(1)` for writes.
- [ ] **Step 4: Run** → PASS; `make check` → PASS.
- [ ] **Step 5: Commit** `feat(store): sqlite store for tickets`.

### Task 4: Engine (owner tasks, executor callbacks, person commands)

**Files:**
- Create: `internal/core/ports.go`, `internal/core/engine.go`
- Test: `internal/core/engine_test.go` (with an in-memory `Repositories` fake in `internal/core/fake_test.go`)

Port behavior from reference `src/core/engine.ts`, `tickets.ts`, `assignments.ts` and the Engine table in `docs/007-architecture.md` for the commands below only. Port the matching cases from `tests/core-engine.spec.ts` (sections on assignments, inputs, reports, retry, stop); skip cases that need Evals, Policy, Threads or Sources (plan 2).

**Interfaces:**
- Produces (`ports.go`):
  - `type Repositories interface{ Tx(func(Tx) error) error }` and `type Tx interface{ GetTicket(string) (*Ticket, error); PutTicket(*Ticket) error; OpenTickets() ([]*Ticket, error); PutTrigger(*Trigger) error; PutDecision(*Decision) error }` (store's `*Tx` satisfies it; Task 5 extends it)
  - `type Executor interface{ Start(t Ticket, a Assignment); Deliver(t Ticket, a Assignment); Stop(t Ticket, a Assignment, reason string) }` — methods only queue; Engine calls them **after** commit.
  - `type Event struct{ Type string; TicketID, AssignmentID, ApprovalID string; At time.Time; Data map[string]any }` with types `ticket.created, ticket.ended, assignment.started, assignment.ended, report.filed, input.added, approval.requested, approval.answered`.
- Produces (`engine.go`): `func NewEngine(repos Repositories, exec Executor, now func() time.Time, emit func(Event)) *Engine` and methods:
  - `SubmitOwnerTask(text string, opts TaskOptions) (Ticket, error)` — records a Trigger (`source:"owner"`, human), a direct accept Decision with an explicit Mandate (`Scope:"owner"`), a Ticket with title = first line of text truncated to 80 runes, one pending Assignment, the text as the first Input; then `exec.Start`. `TaskOptions{ Dir string; Model string }`.
  - Executor callbacks: `AssignmentStarted(ticketID, assignmentID, sessionID string) error`, `InputsDelivered(ticketID, assignmentID string, inputIDs []string) error`, `DeliveryFailed(ticketID, assignmentID, errMsg string) error`, `InputsRead(ticketID string, inputIDs []string) error`, `TurnEnded(ticketID, assignmentID string) error`, `FileReport(ticketID string, r ReportInput) error` with `ReportInput{ AssignmentID string; State ReportState; Summary, Question string }` — `question` sets Attention; `dropped` ends the Ticket `dropped`; `ready` ends it `completed` (no Threads in plan 1).
  - Person commands: `AddPersonInput(ticketID, text string) error` (human Input; clears Attention; releases blocked/failed; resumes an ended Ticket by starting a new Assignment), `StopTicket(ticketID, reason string) error` (ends `stopped` by human, `exec.Stop`), `RetryTicket(ticketID string) error` (only if `CanRetry`; ends current Assignment, starts a new one, `UserError` otherwise).
  - `type UserError struct{ Msg string }` for invalid requests.
  - Queries: `Ticket(id string) (Ticket, Stage, error)`, `Tickets(includeEnded bool) ([]Ticket, error)`.

- [ ] **Step 1: Write failing tests** (each asserts stored state, emitted events, and executor calls *after* commit): `TestSubmitOwnerTaskStartsAssignment`; `TestAssignmentStartedDeliversWaitingInputs` (Start → AssignmentStarted → `exec.Deliver` called); `TestReportQuestionSetsAttention` (stage `person/answer`); `TestPersonInputClearsAttentionAndDelivers`; `TestReportReadyEndsCompleted`; `TestReportDroppedEnds`; `TestBlockedThenHumanInputReleases` (stage moves from `person/blocked` to `working`); `TestRetryOnlyWhenAllowed` (UserError otherwise); `TestStopEndsAndStopsExecutor`; `TestExecutorNotCalledOnRollback` (repo Tx returns error → no exec calls, no events); `TestInputsDeliveredInOrderExactlyOnce` (three person inputs while running → `Undelivered` drained in order; delivering the same IDs twice is a no-op) — Review Focus 4; plus the ported reference cases.
- [ ] **Step 2: Run** `go test ./internal/core` → FAIL.
- [ ] **Step 3: Implement** — each command: `repos.Tx` → load, mutate, put, collect effects (executor calls + events) in a slice → after successful commit, run effects in order. IDs: `T-%04d` for tickets (counter in kv via Tx), random 12-hex for others.
- [ ] **Step 4: Run** → PASS; `make check` → PASS.
- [ ] **Step 5: Commit** `feat(core): engine for owner tasks and executor callbacks`.

### Task 5: Approvals in the domain

**Files:**
- Create: `internal/core/approvals.go`
- Modify: `internal/core/ports.go` (add to `Tx`: `GetApproval(string) (*Approval, error); PutApproval(*Approval) error; PendingApprovals() ([]*Approval, error)`), `internal/store/tickets.go` + migration 2 (table `approvals(id TEXT PRIMARY KEY, state TEXT NOT NULL, ticket_id TEXT NOT NULL, doc TEXT NOT NULL)`), `internal/core/stage.go`
- Test: `internal/core/approvals_test.go`, `internal/store/store_test.go` (`TestApprovalRoundTrip`, `TestMigration1To2`)

**Interfaces:**
- Produces: `type Approval struct{ ID, TicketID, AssignmentID, Tool, Summary, Rule string; Input json.RawMessage; State string /* pending|approved|denied|expired */; CreatedAt time.Time; AnsweredAt *time.Time; Answer string }`; Engine methods `RequestApproval(a Approval) (Approval, error)`, `AnswerApproval(id string, approve bool, note string) error`, `ExpireApproval(id string) error`, `PendingApprovals() ([]Approval, error)`; waiters: `func (e *Engine) WaitApproval(ctx context.Context, id string) (Approval, error)` returns when answered/expired or ctx done.
- Stage addition: a Ticket with a pending Approval has stage `{person, approval}` placed directly after `delivery` in `TicketStage` (add the test row in Task 2's table in this task).

- [ ] **Step 1: Write failing tests**: `TestRequestApprovalShowsPersonStage`; `TestAnswerApprovalWakesWaiter` (goroutine waiting on `WaitApproval` gets `approved`); `TestExpireApproval` (state `expired`, waiter gets it); `TestAnswerTwiceIsUserError`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS; `make check`.
- [ ] **Step 5: Commit** `feat(core): approvals`.

### Task 6: Hook rules and `athena hook`

**Files:**
- Create: `internal/node/rules.go`, `internal/node/hookcmd.go`
- Modify: `cmd/athena/main.go` (wire `hook`)
- Test: `internal/node/rules_test.go`, `internal/node/hookcmd_test.go`

Hook stdin/stdout follow Claude Code's hook contract (input JSON with `session_id`, `cwd`, `hook_event_name`, `tool_name`, `tool_input`; PreToolUse output `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow|deny|ask","permissionDecisionReason":"..."}}`). Verify against the installed CLI's docs (`claude --help`, Claude Code hooks reference) before coding and record the verified shapes in `internal/node/testdata/hooks/*.json`.

**Interfaces:**
- Produces:
  - `type Decision struct{ Verdict string /* allow|deny|escalate */; Reason, Rule string }`
  - `func Evaluate(ctx RuleContext, tool string, input json.RawMessage) Decision` with `RuleContext{ TicketDir string; Blocked []string; Home string }`
  - Rules (spec §16.1 item 1), in order: credential reads → deny (`~/.ssh`, `~/.aws`, `~/.config/gh`, `~/.netrc`, `~/.claude*` credential files, `~/.codex/auth.json`, `<stateDir>`); blocked commands (`ATHENA_BLOCKED_COMMANDS`, comma-separated, matched as the first word of any `&&`, `;`, `|`-separated segment) → deny; writes (`Write`, `Edit`, `MultiEdit`, `NotebookEdit` `file_path`) outside `TicketDir` after `filepath.Clean` + `EvalSymlinks` of the nearest existing parent → escalate; Bash matching `--env[= ](staging|prod\S*)`, `\bdeploy\b`, `kubectl`, `terraform apply`, `brew install`, `apt(-get)? install`, `npm (i|install) -g`, `pip install` (without an active venv), `git push --force|-f`, `git branch -D`, `git push origin --delete`, `gh pr merge` → escalate; everything else → allow.
  - `func RunHook(event string, stdin io.Reader, stdout, stderr io.Writer, socket string) int` — reads input, sends `{"op":"hook","event":...,"input":...}` to the node socket, writes the hook output; for PreToolUse maps `escalate` → node creates an Approval and blocks until answered (node side, Task 8) and returns the final allow/deny; any socket error → deny with reason `athena node unreachable` and exit 0 with deny JSON.

- [ ] **Step 1: Write failing tests**: table `TestEvaluate` covering each rule plus Review Focus 5 (`../escape.txt`, `/etc/hosts`, a symlink inside TicketDir pointing to `/tmp/out` → escalate; `sub/ok.txt` → allow); `TestBlockedInPipeline` (`echo x | rm -rf /` with `rm` blocked → deny); `TestHookFailsClosed` (no socket → deny JSON, exit 0) — Review Focus 3; `TestHookRoundTrip` against a fake socket server returning allow.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS; `make check`.
- [ ] **Step 5: Commit** `feat(node): hook rules and hook command`.

### Task 7: Claude Code runtime driver

**Files:**
- Create: `internal/runtime/claude/events.go`, `session.go`, `config.go`, `scripts/capture-claude-fixtures.sh`, `testdata/claude/*.jsonl`, `testdata/fakeclaude/main.go`
- Test: `internal/runtime/claude/events_test.go`, `session_test.go`, `config_test.go`

- [ ] **Step 1: Capture real fixtures.** `scripts/capture-claude-fixtures.sh` runs, in a temp dir, `claude -p --input-format stream-json --output-format stream-json --verbose --model haiku` feeding one user message (`"Create a file hi.txt containing hi, then say done."`) and saves stdout to `testdata/claude/simple-turn.jsonl`; a second run with `--resume <session_id>` saves `testdata/claude/resume-turn.jsonl`; a third feeds a second user message mid-turn and saves `testdata/claude/mid-turn-input.jsonl`. Commit fixtures with any secrets/paths scrubbed (script replaces `$HOME` with `/home/user`). Record the exact stdin message shape the CLI accepted in `testdata/claude/README.md`.

**Interfaces:**
- Produces (`events.go`): `type Event interface{ isEvent() }` with `Init{ SessionID, Model, CWD string; Tools []string }`, `AssistantText{ Text string }`, `ToolUse{ ID, Name string; Input json.RawMessage }`, `ToolResult{ ToolUseID string; IsError bool }`, `Result{ SessionID, Subtype string; IsError bool; CostUSD float64; InputTokens, OutputTokens, CacheReadTokens int; NumTurns int; DurationMS int }`, `Unknown{ Type string; Raw json.RawMessage }`; `func ParseEvent(line []byte) (Event, error)` (unknown types → `Unknown`, never an error).
- Produces (`session.go`): `type Options struct{ Binary string /* default "claude" */; Dir, Model, ResumeID, SettingsPath, MCPConfigPath, AppendSystemPrompt string; Env []string }`; `func Start(ctx context.Context, o Options) (*Session, error)`; `(*Session) Send(text string) error` (writes one stream-json user message line); `Events() <-chan Event` (closed when the process exits); `Interrupt() error` (SIGINT); `Close() error` (close stdin, wait ≤ 10 s, then kill); `Wait() error`; `Transcript` — every raw line is also appended to `<Dir>/.athena/transcript.jsonl`.
  - Arguments: `-p --input-format stream-json --output-format stream-json --verbose --model <Model> --settings <SettingsPath> --mcp-config <MCPConfigPath> --strict-mcp-config --permission-mode default` plus `--resume <ResumeID>` and `--append-system-prompt <...>` when set. Verify each flag exists in `claude --help` for 2.1.x; if one differs, use the verified equivalent and note it in the README.
- Produces (`config.go`): `func WriteSettings(path, athenaBin, socket string) error` (hooks `PreToolUse`, `PermissionRequest`, `PostToolUse`, `Stop`, `SessionStart` → `"<athenaBin> hook <event>"` with env `ATHENA_SOCKET`), `func WriteMCPConfig(path, athenaBin, socket, ticketID string) error` (server `athena` → `<athenaBin> mcp --ticket <id>`).

- [ ] **Step 2: Write failing tests**: `TestParseFixtures` (simple-turn yields Init with non-empty SessionID, ≥1 ToolUse `Write`, a Result with Subtype `success`); `TestParseUnknownIsNotError`; `TestSessionAgainstFake` (fakeclaude replays a fixture and echoes received stdin lines to a file; assert Send's line reaches it and Events closes on exit); `TestSessionResumeArgs`; `TestCloseKillsAfterTimeout` (fake ignores stdin close); `TestWriteSettingsShape` (golden JSON).
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.** **Step 5: Run** → PASS; `make check`.
- [ ] **Step 6: Commit** `feat(runtime): claude code stream-json driver`.

### Task 8: Node — executor, socket, MCP `report`, recovery

**Files:**
- Create: `internal/node/node.go`, `internal/node/socket.go`, `internal/node/mcpcmd.go`, `prompts/worker.md`
- Modify: `cmd/athena/main.go` (wire `mcp`)
- Test: `internal/node/node_test.go`, `internal/node/mcp_test.go`

`prompts/worker.md`: adapt reference `prompts/worker.md` + `prompts/shared.md` — remove DeepSeek Harness specifics, keep the attention-saving rules, instruct the worker to file Reports only through the `report` tool, and include the execution variables.

**Interfaces:**
- Consumes: `core.Engine` callbacks (Task 4), `RequestApproval/WaitApproval` (Task 5), `Evaluate` (Task 6), `claude.Start` (Task 7).
- Produces:
  - `func New(cfg Config, eng EngineCallbacks) *Node` with `Config{ WorkRoot, StateDir, AthenaBin, SocketPath string; Slots int /* default 4 */; Blocked []string; Model string /* default "sonnet" */; ClaudeBinary string; ApprovalTimeout time.Duration /* default 30m */ }`; `EngineCallbacks` is the subset interface of `*core.Engine` methods it calls.
  - `(*Node) Start(t core.Ticket, a core.Assignment)`, `Deliver`, `Stop` (implements `core.Executor`; never block the caller — enqueue onto the node's goroutine).
  - `(*Node) Serve(ctx context.Context) error` — Unix socket (0600) JSON lines: ops `hook` (→ Evaluate; `escalate` → `RequestApproval` + `WaitApproval` with `ApprovalTimeout` → allow/deny; `Stop` event → `TurnEnded`; `SessionStart` → returns `additionalContext` with execution variables), `report` (→ `FileReport`), `ping`.
  - `RunMCP(ticketID, socket string) error` — stdio MCP server (go-sdk) exposing tool `report{ status: enum[progress,waiting,ready,blocked,failed,dropped], summary: string, question?: string }` forwarding to the socket.
  - Behavior: Start → create `<WorkRoot>/<ticket-short-name>` (slug of title + ticket id) or use `TaskOptions.Dir`, write `.athena/settings.json` and `.athena/mcp.json`, start the session (resuming `Assignment.SessionID` if set), send the worker prompt + undelivered Inputs as the first message, call `AssignmentStarted` on `Init`, `InputsDelivered` after each `Send`. Deliver while a turn runs → `Send` immediately (stream-json accepts mid-turn input). Process exit without Stop → restart with `ResumeID` and a recovery note ("Your previous process ended unexpectedly; continue the task.") at most 3 times, then `FileReport(failed, "session crashed repeatedly")`. Slots: queue Starts beyond `Slots`.

- [ ] **Step 1: Write failing tests** (fakeclaude + real store + real engine): `TestStartRunsSessionAndReportsStarted`; `TestReportToolEndsTicket` (MCP client in-memory transport calls `report{ready}` → Ticket ended `completed`); `TestEscalationCreatesApprovalAndWaits` (hook op with a deploy command → approval pending → `AnswerApproval(true)` → hook reply allow); `TestApprovalTimeoutDenies` (timeout 50 ms → deny, approval `expired`); `TestCrashResumesSameSession` (fakeclaude exits mid-turn once → second start has `--resume <id>` and the recovery note; stage never stuck) — Review Focus 1; `TestMidTurnInputsInOrder` — Review Focus 4; `TestSlotsQueue` (Slots=1, two tickets → second starts after first ends).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS; `make check`.
- [ ] **Step 5: Commit** `feat(node): local executor with hooks, approvals and report tool`.

### Task 9: Server, control socket and CLI

**Files:**
- Create: `internal/server/server.go`, `internal/server/control.go`, `internal/cli/cli.go`
- Modify: `cmd/athena/main.go`
- Test: `internal/server/server_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Produces:
  - `func Run(ctx context.Context, cfg Config) error` with `Config{ StateDir string; Node node.Config }` — opens store `<StateDir>/athena.db`, builds Engine + Node, serves node socket `<StateDir>/node.sock` and control socket `<StateDir>/athena.sock` (both 0600), writes a pid file, slog JSON to `<StateDir>/logs/athena.log` and stderr.
  - Startup recovery: for every open Ticket with a running Assignment, `node.Start` it again (resume + recovery note); for running Assignments with `Undelivered` Inputs, redeliver after start — Review Focus 2.
  - Control ops (JSON lines, one request → one response, `logs` streams): `submit{text, dir?, model?}`, `tickets{all?}`, `ticket{id}`, `reply{id, text}`, `stop{id, reason}`, `retry{id}`, `approvals`, `answer{id, approve, note}`, `logs{id, follow}` (tails `<dir>/.athena/transcript.jsonl`, rendered as one line per event: time, kind, short text).
  - CLI (`internal/cli`): `athena server [--state-dir]`, `athena task "<text>" [--dir D] [--model M]` (prints ticket id), `athena status` (table: id, stage, title, age), `athena logs <id> [-f]`, `athena reply <id> "<text>"`, `athena stop <id> [--reason]`, `athena retry <id>`, `athena approvals`, `athena approve <id> [--note]`, `athena deny <id> [--note]`. Clear error `athena server is not running (no <path>)` when the socket is missing.
- [ ] **Step 1: Write failing tests**: `TestEndToEndWithFake` (in-process server with fakeclaude; `task` → `status` shows `working` → simulated `report{ready}` via node socket → `status` shows ticket ended); `TestRestartResumesRunningAssignment` (stop server mid-run, start again → fakeclaude invoked with `--resume`, undelivered input delivered once) — Review Focus 2; `TestCLINoServer`; `TestApproveFromCLI`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS; `make check`.
- [ ] **Step 5: Commit** `feat(server): athena server, control socket and CLI`.

### Task 10: Live smoke test with real Claude Code

**Files:**
- Create: `internal/server/live_test.go` (build tag `live`), `docs/dev/smoke-test.md`

- [ ] **Step 1: Write the live tests** (skip unless `ATHENA_LIVE=1` and `claude` is on PATH): `TestLiveHelloFile` — submit "Create hello.txt containing exactly `hi` in your working directory, then report ready." → within 3 min the Ticket ends `completed` and `<dir>/hello.txt` == `hi`; `TestLiveWriteOutsideEscalates` — submit "Write /tmp/athena-live-outside.txt with `x`, then report ready." → an Approval appears within 2 min; deny it → file absent, Ticket gets a report (any state) mentioning the denial.
- [ ] **Step 2: Run** `ATHENA_LIVE=1 go test -tags live -run Live -v ./internal/server` → PASS. If a hook or flag shape differs from the fixtures, fix the driver/rules and add a fixture that captures the real shape.
- [ ] **Step 3: Write** `docs/dev/smoke-test.md`: how to run `athena server` and a manual session (`athena task`, `athena logs -f`, `athena approve`).
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `test: live smoke test with claude code`.

---

## Spec coverage for plan 1

- §4 reference relationship: Tasks 1–4 (port, NOTICE, docs/reference).
- §5 Ticket/Assignment/Input/Report/stages/Approval: Tasks 2, 4, 5.
- §6.1 headless Claude sessions, resume, mid-turn input, per-Ticket dir: Tasks 7, 8.
- §6.2 hooks (PreToolUse, PermissionRequest, Stop, SessionStart; PostToolUse wired, recorded in plan 9): Tasks 6, 7, 8.
- §6.4 `report` tool: Task 8 (other tools in plans 2, 7, 8).
- §11 containment, fail-closed hooks, 0600 files, secret-safe logs: Tasks 3, 6, 8, 9.
- §16.1 item 1 approval defaults: Task 6.
- §10.2 basic `athena status` / `athena logs`: Task 9.

Deferred by design to later plans (see roadmap): router, Policy, GitHub, Outbox (2); link protocol, auth, TLS, push (3); dashboard (4); Codex (5); multi-node (6); agent-to-agent (7); local inference (8); visibility (9).
