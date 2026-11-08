# Athena Plan 11: Autopilot Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The owner writes standing instructions in Settings ("every night, update the changelog in acme/widgets"; "when CI on main has been red for four hours, find out why") and Athena starts that work itself, on a clock or a repository event, through the same router, Policy, brake and kill switch as everything else.

**Architecture:**
- **Autopilot entries are Policy settings.** `policy.Settings` gains `Autopilot []AutopilotEntry`, saved and revisioned like the rest of the Policy. No migration: entries live in the settings document, and the Source's memory lives in the KV table.
- **One new Source, `internal/sources/schedule`.** It emits `core.Trigger`s with `Source: "schedule"`, one Thread per entry (`schedule:<slug>`), and `Meta["repo"]`, which is what the brake and `Ticket.Repo` already read. It holds no decision logic beyond *when to fire*.
- **The entry is its own rule.** A schedule Trigger is admitted `inferred` with `Basis.Via = ViaAutopilot` and `Basis.Rules = ["autopilot/<slug>"]`. The router still assesses it, but judges only whether there is work to do now. The accept-mode logic is the same as for any inferred candidate: `observe` records would-accept, `off` refuses, `auto` accepts.
- **Repository facts come through a narrow port.** `schedule.Repos` (head, changed files, CI state) is implemented by `github.Watcher` over the existing ETag-caching `Client`, so an unchanged repository costs only free 304s, and the schedule Source knows nothing about GitHub.
- **The brakes need no new code** apart from the Source declining to fire while paused: the WIP brake keys on `inferred` and `Thread.Meta["repo"]`, and the kill switch stops the eval loop.

**Tech Stack:** As in plans 1–5 and 10: Go 1.27, `modernc.org/sqlite`, React 19, Vite, Vitest, Playwright. No new Go modules and no new npm packages; the cron parser is about 150 lines of our own.

**Spec:** `docs/superpowers/specs/2026-11-03-athena-design.md`.
- **Binding:** §18.5.
- **For context:** §18.1 (brake), §18.2 (kill switch), §18.3 (trust), §5 (Policy, admission, strength), §10.1 (design bar).

Also read `docs/dev/autonomy.md` and `docs/dev/dashboard.md` (the client contract).

**Out of scope:**
- Local-folder targets. An entry targets a GitHub repository the GitHub Source polls.
- Webhooks. Repository events are polled.
- A "runs" table. The Routing screen already lists every Trigger with its outcome; Settings links there.

## Rulings on what the spec leaves open

These are the plan's decisions. Each is the owner's to overturn at review.

| # | Ruling | Why |
|---|---|---|
| A | Autopilot work has trust class `owner`. | The owner wrote the instructions, and the Trigger text carries only those plus facts Athena computed (SHAs, and file names from a fixed list). Without this the worker would be told to treat its own task as untrusted data. No external path can create a `schedule` Trigger. |
| B | An entry needs no matching Policy rule. It *is* the rule, named `autopilot/<slug>`, and no evidence quote is asked for. | Otherwise every GitHub assessment would have to judge every autopilot entry, and the owner would have to write each instruction twice. |
| C | At most one run of an entry at a time. While its Thread has a queued Trigger, an Eval, a brake or an open Ticket, the entry does not fire. | Without it, a nightly job on a Ticket awaiting review queues a new candidate every night and feeds Inputs to the running Ticket. |
| D | A clock firing that comes due while the entry is busy is dropped. A repository event is kept and fires when the entry is free, for the newest head only. | A skipped nightly run has a next night. A branch move has no second chance. |
| E | After downtime or sleep, only the latest missed firing runs, if it is under 24 h old, and its text says it is a catch-up. | The owner's server is a laptop; a nightly job should run when it wakes, once. From nucleos: a late run should say it is late. |
| F | Firings at least 15 minutes apart (cron expressions that fire faster are refused on save, and events respect the same gap). At most 24 firings per entry per local day, counted across restarts. | The anti-spam anti-goal, and a merge by Autopilot moving the default branch can otherwise loop. From nucleos: the cap must survive a restart, because a crash loop is when it matters. |
| G | Cron times are in the server's local time zone, five numeric fields. A wall-clock time that does not exist on a daylight-saving day is skipped; one that happens twice fires once. | The owner thinks in local time. |
| H | CI red default is 4 hours (range 1 to 168). "Red" is at least one completed check suite of the head commit concluding `failure` or `timed_out`. Athena's own suites do not count. | The spec names N but no value. `cancelled` is usually a superseded run. |
| I | While paused, or while the target repository is not yet known to the GitHub Source, nothing fires and nothing is forgotten. | §18.2: nothing queued is lost. |

## Global Constraints

- Every plan 1–5 and plan 10 Global Constraint still holds:
  - plan 3's CSP;
  - plan 4's Precision tokens, with orange only for what needs the owner;
  - the client link and storage rules;
  - `make check` at the end of every task;
  - `make e2e` with screenshots at 390, 820 and 1440 px, light and dark, axe clean, for every task that changes the UI;
  - e2e builds go to `bin/e2e/`;
  - tests use only `fakeclaude`, `fakecodex` and the fake GitHub.
- Bundle: the initial route may not grow. Everything new in the dashboard goes in the lazy Settings chunk, in files `policy.ts` does not import.
- No new Go modules, no new npm packages, no migration.
- Exact values:
  - Source and Thread prefix **`schedule`**; Trigger kind and scope **`schedule`**; `Meta["autopilot"]` holds the slug.
  - Events **`branch_moved`**, **`manifest_changed`**, **`ci_red`**.
  - Via **`autopilot`**. Rule name **`autopilot/<slug>`**. Reason code **`autopilot_gone`**.
  - `MinGap` **15 min**, `DailyCap` **24**, `CatchUpWindow` **24 h**, `catchUpGrace` **5 min**, `TickInterval` **30 s**, `EventInterval` **5 min**, CI red default **4 h**, range **1–168**, instructions at most **4000** characters, at most **50** entries.
- The repository is public: no personal paths or secrets in code, docs or fixtures.

## Review Focus

1. **An entry fires while its last run is unfinished or braked.** Nothing piles up: no second candidate, no Input to the open Ticket. Pinned in Task 4 (`TestBusyEntryDoesNotFire`) and Task 6 (`TestAutopilotBrakedThenReleasedOnce`).
2. **The server was down or asleep across a firing, or restarts right after one.** One catch-up, marked as one; never a backlog, never a double firing. Pinned in Task 4 (`TestCatchUpFiresLatestOnce`, `TestRestartDoesNotRefire`).
3. **The owner edits, disables or deletes an entry while work is pending.** No surprise firing from an edit; a braked candidate dies at the gate; an open Ticket is told. Pinned in Task 3 (`TestGateRefusesGoneEntry`, `TestEffectsTellsAutopilotTicket`) and Task 4 (`TestEditedCronStartsOver`).
4. **Autopilot's own merged pull request moves the branch that started it.** The loop stops at the gap and the cap, and the cap survives a restart. Pinned in Task 4 (`TestDailyCapSurvivesRestart`) and Task 5 (`TestBranchMovedRespectsGap`).
5. **A new entry on a repository with history, or one already red.** Nothing fires for the past, and the red clock starts when Athena first sees red. Pinned in Task 5 (`TestFirstSightSetsBaseline`, `TestRedClockStartsAtFirstSight`).

---

### Task 1: Cron expressions

**Files:**
- Create: `internal/cron/cron.go`, `internal/cron/cron_test.go`

**Interfaces:**
- Produces:
  - `func Parse(expr string) (Schedule, error)`: five space-separated numeric fields (minute 0–59, hour 0–23, day 1–31, month 1–12, weekday 0–7 where 0 and 7 are Sunday); each a `*`, value, range, list or `/step`. Anything else, and an expression that never fires, is an error naming the field.
  - `func (s Schedule) Next(after time.Time) time.Time`: the first firing strictly after `after`, in `after`'s Location; zero if none within 5 years.
  - `func (s Schedule) TooFrequent(min time.Duration) bool`: whether two consecutive firings over a reference week are closer than `min`.

- [ ] **Step 1: Write the failing tests** in `cron_test.go`, table-driven:

```go
func TestNext(t *testing.T) // "0 3 * * *" after 2026-11-08 03:00 → 2026-11-09 03:00 (strictly after);
                            // "*/15 * * * *" after 10:07 → 10:15; "0 9 * * 1" lands on a Monday;
                            // "0 0 29 2 *" finds the next leap year
func TestDayFieldsAreOred(t *testing.T) // "0 0 13 * 5" fires on every 13th and every Friday (standard cron)
func TestWeekdaySevenIsSunday(t *testing.T)
func TestParseRejects(t *testing.T) // "", "* * * *", "60 * * * *", "* * 0 * *", "a * * * *", "@daily", "1-", "*/0 * * * *", "0 0 30 2 *" (never fires)
func TestSpringForwardSkips(t *testing.T) // Europe/Lisbon, "30 1 * * *": 2026-03-28 01:30 → 2026-03-30 01:30
func TestFallBackRepeatsWallClock(t *testing.T) // Europe/Lisbon, "30 1 * * *" from 2026-10-25 00:00: two firings an hour apart with the same wall clock
func TestTooFrequent(t *testing.T) // 15m: "*/15 * * * *" false, "*/10 * * * *" true, "0,5 3 * * *" true, "0 3 * * *" false
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cron/`
Expected: FAIL (package has no code).

- [ ] **Step 3: Implement `internal/cron/cron.go`**

Fields as bitsets. `Next` steps absolute minutes and reads the wall clock in the Location, jumping to the next local midnight when the day does not match, so a sparse expression stays cheap and daylight-saving days behave as the tests say.

- [ ] **Step 4: Run tests, then `make check`**

Expected: PASS.

- [ ] **Step 5: Commit**: `feat(cron): five-field schedules, for Autopilot`

---

### Task 2: Autopilot entries in the Policy settings

**Files:**
- Modify: `internal/policy/settings.go`
- Create: `internal/policy/autopilot.go`, `internal/policy/autopilot_test.go`

**Interfaces:**
- Consumes: `cron.Parse`, `cron.Schedule.TooFrequent` (Task 1).
- Produces:

```go
const ScheduleSource = "schedule"
const (EventBranchMoved = "branch_moved"; EventManifestChanged = "manifest_changed"; EventCIRed = "ci_red")
const (DefaultCIRedHours = 4; MaxAutopilotEntries = 50; MaxAutopilotInstructions = 4000)

type AutopilotEntry struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
	Repo         string `json:"repo"`
	Enabled      bool   `json:"enabled"`
	Cron         string `json:"cron,omitempty"`       // exactly one of Cron and Event
	Event        string `json:"event,omitempty"`
	CIRedHours   int    `json:"ciRedHours,omitempty"` // ci_red only; 0 is DefaultCIRedHours
}

func AutopilotSlug(name string) string // lower-case; each run of spaces and underscores becomes "-"
func AutopilotRule(slug string) string // "autopilot/" + slug
func (s Settings) AutopilotEntry(slug string) (AutopilotEntry, bool)
```

`Settings` gains `Autopilot []AutopilotEntry \`json:"autopilot,omitempty"\``.

- [ ] **Step 1: Write the failing tests**

```go
func TestValidateAutopilot(t *testing.T) // table, each a refused entry with the substring of its error:
// empty name; name not matching ^[A-Za-z0-9][A-Za-z0-9 _-]{0,63}$; two names with one slug ("Night Job", "night-job");
// empty instructions; instructions over 4000 characters; repo not owner/name; neither cron nor event; both;
// unknown event; a bad cron; a cron under 15 minutes apart; ciRedHours 169, or set on a non-ci_red entry; 51 entries.
func TestAutopilotSlug(t *testing.T) // "Night  Job_2" → "night-job-2"
func TestNormalizeKeepsAutopilot(t *testing.T) // trims name and repo; a saved and reloaded entry is equal
func TestVersionIgnoresAbsentAndDisabledEntries(t *testing.T)
// Version(Defaults(x)) equals the literal hash captured from main before this change; adding a disabled entry leaves it;
// adding an enabled one changes it; changing an enabled entry's instructions changes it.
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/policy/ -run 'Autopilot|VersionIgnores'`
Expected: FAIL (undefined). Before editing `canonical`, print `Version(Defaults([]string{"octo"}))` on main and paste the value into the test.

- [ ] **Step 3: Implement**

`validateAutopilot` is called from `Validate`. `normalize` copies the slice with trimmed `Name` and `Repo`. `canonical` gains `Autopilot []AutopilotEntry \`json:"autopilot,omitempty"\``, holding enabled entries sorted by name, so an installation with none keeps its Policy version and no one gets a spurious "policy changed" notice.

- [ ] **Step 4: Run tests, then `make check`**

Expected: PASS.

- [ ] **Step 5: Commit**: `feat(policy): Autopilot entries in the settings`

---

### Task 3: Admitting, assessing and revalidating Autopilot work

**Files:**
- Modify: `internal/core/model.go`, `internal/core/ports.go`, `internal/core/evals.go`, `internal/core/evals_test.go:81`, `internal/core/intake_test.go:45`
- Modify: `internal/policy/policy.go`, `internal/policy/trust.go`, `internal/policy/policy_test.go:402`, `internal/policy/autopilot.go`, `internal/policy/autopilot_test.go`
- Modify: `internal/router/tools.go:377`, `internal/router/tools_test.go`, `prompts/admission.md`
- Modify: `web/src/features/routing/outcome.ts` and its test

**Interfaces:**
- Consumes: `Settings.AutopilotEntry`, `AutopilotRule`, `ScheduleSource` (Task 2).
- Produces:
  - `core.KindSchedule = "schedule"`, `core.ViaAutopilot = "autopilot"`, `policy.CodeAutopilotGone = "autopilot_gone"`.
  - `core.Policy.Assess(a Assessment, strength, via string) (kind string, reasons []Reason, wouldAccept bool, err error)`: `via` is the Eval's `Basis.Via`.

- [ ] **Step 1: Write the failing tests** in `internal/policy/autopilot_test.go`

```go
func TestGateAdmitsEnabledEntry(t *testing.T)
// Trigger{Source:"schedule", Kind:"schedule", Scope:"schedule", Meta:{"autopilot":"night","repo":"o/r"}} in observe:
// Strength "inferred", Basis.Via "autopilot", Basis.Rules ["autopilot/night"], Basis.Sender "night". No Sources part needed.
func TestGateRefusesGoneEntry(t *testing.T)   // missing or disabled: Reason.Code "autopilot_gone"
func TestGateModeOffRefuses(t *testing.T)     // Reason.Code CodeModeOff
func TestAssessAutopilot(t *testing.T)
// via autopilot, no judgements: Accept true → auto: kindAccept; observe: kindIgnore + CodeWouldAccept + wouldAccept true;
// off: kindIgnore + CodeModeOff. Accept false → kindIgnore. Stray rule judgements are ignored, not an error.
func TestAssessOtherViaStillJudgesRules(t *testing.T) // via admission with no judgements of an enabled rule: an error, as today
func TestAdmitsInferredAutopilotRule(t *testing.T)    // auto + enabled entry true; observe false; disabled false
func TestRevalidateAutopilot(t *testing.T)            // true while enabled, Listens and auto; false after the entry goes
func TestTrustOfScheduleIsOwner(t *testing.T)
func TestEffectsTellsAutopilotTicket(t *testing.T)
// open Ticket with Basis.Rules ["autopilot/night"]: entry disabled → a note saying the Autopilot entry no longer applies;
// instructions changed → a note carrying the new text; unrelated entry changed → no note.
```

In `internal/core/evals_test.go`: `TestAcceptAutopilotKeepsBasisRules`: an assessment with no judgements on an Eval with `Basis.Rules ["autopilot/x"]` creates a Ticket whose `Mandate.Basis.Rules` is `["autopilot/x"]` and asks `AdmitsInferred` about exactly that list.

In `internal/router/tools_test.go`: `TestEvalShowsNoRulesForAutopilot`: `rules` is empty for a schedule candidate while an enabled rule exists.

In `web/src/features/routing/outcome.test.ts`: `reasonSentence("autopilot_gone", {})` is `"Not started: this Autopilot entry is off or was removed."`

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/policy/ ./internal/core/ ./internal/router/`
Expected: FAIL (undefined, or a mismatched `Assess` signature).

- [ ] **Step 3: Implement**

- `Gate`: first case, before the Source-part lookup: `t.Source == ScheduleSource`.
- `policy.Assess` routes `via == core.ViaAutopilot` to a new `autopilotOutcome(a, s)`; `AssessmentOutcome` is unchanged.
- `core.accept`: after `Assess`, when `ev.Basis.Via == ViaAutopilot`, set `rules = ev.Basis.Rules`.
- `admitsInferred`: a name with the `autopilot/` prefix looks up the entry instead of `findRule`.
- `requesterHolds`: `ViaAutopilot` holds while the entry exists and is enabled; check it before the Source-part lookup.
- `Trust`: `t.Source == ScheduleSource` returns `core.TrustOwner`, as the second case.
- `Effects`: for each `autopilot/` name on a Ticket's Basis, the notes the tests name.
- `router/tools.go:377`: leave `rules` empty when `v.Eval.Basis.Via == core.ViaAutopilot`.
- Add to `prompts/admission.md`, after the line starting "Explicit candidates are routed":

> A candidate whose source kind is schedule is the owner's own standing instruction, started by a clock or a repository event, and it names no rules: always take it through eval and eval_decide, with an empty rules list and no evidence. Accept unless an open ticket already covers it or the text shows nothing needs doing now. It has no conversation to acknowledge in, so give no ack.

- [ ] **Step 4: Run tests, then `make check`**

Expected: PASS.

- [ ] **Step 5: Commit**: `feat(policy,core): admit and assess Autopilot entries as their own rule`

---

### Task 4: The schedule Source, clock firings

**Files:**
- Create: `internal/sources/schedule/source.go`, `internal/sources/schedule/state.go`, `internal/sources/schedule/source_test.go`
- Modify: `internal/core/intake.go`, `internal/core/intake_test.go`

**Interfaces:**
- Consumes: `cron.Parse`, `Schedule.Next`, `core.KindSchedule`, `sources.Source`.
- Produces:

```go
// core
func (e *Engine) ThreadBusy(thread string) (bool, error)
// true while the Thread has queued Triggers, an Eval, a Brake, or a Subscription of a Ticket that has not ended.

// schedule
const SourceName = "schedule"
const (TickInterval = 30*time.Second; EventInterval = 5*time.Minute; MinGap = 15*time.Minute; CatchUpWindow = 24*time.Hour; DailyCap = 24)

type Entry struct {
	Slug, Name, Instructions, Repo string
	Cron, Event string
	CIRedAfter time.Duration
}
type Intake interface {
	Receive(t core.Trigger) (string, error)
	ThreadBusy(thread string) (bool, error)
}
type KV interface { KVGet(key string) (string, bool, error); KVSet(key, value string) error }
type Config struct {
	Entries       func() []Entry   // the enabled entries, read on every tick
	Known         func(repo string) bool
	Paused        func() bool
	Repos         Repos            // Task 5; nil means no event entry fires
	Now           func() time.Time
	Wait          func(ctx context.Context, d time.Duration) error
	TickInterval, EventInterval time.Duration // zero is the default; tests shorten them
}
func New(cfg Config, in Intake, kv KV, log *slog.Logger) *Source
func ThreadKey(slug string) string // "schedule:" + slug
```

`*Source` implements `sources.Source`. `Listens(thread)` is true for the Thread of a currently enabled entry. State is one KV document per entry, `schedule.state.<slug>`: `Spec` (the Cron or Event it was made for), `Cursor`, `Base`, `RedSince`, `Episode`, `LastFired`, `Date`, `Fires`.

One `fire` helper returns one of three outcomes:
- **fired.**
- **held**: paused, or repository unknown. No state changes. An unknown repository also shows in `Health`.
- **dropped**: busy, inside `MinGap`, or at `DailyCap`.

Trigger: `ID "schedule:<slug>:cron:<wall clock 2006-01-02T15:04>"`, `Source "schedule"`, `Kind "schedule"`, `Scope "schedule"`, `Actor {schedule, slug}`, `Threads [ThreadKey(slug)]`, `Meta {"repo", "autopilot": slug}`, `Text`: the instructions, a blank line, then one "Why now:" line.

- [ ] **Step 1: Write the failing tests** with a fake clock, fake Intake and map KV, using `Local = UTC` unless named

```go
func TestNewEntryStartsNow(t *testing.T)       // first tick records a Cursor and fires nothing
func TestFiresOnSchedule(t *testing.T)         // "0 3 * * *": nothing at 02:59; at 03:00:10 one Trigger with the exact ID, Meta and Text; "Why now: scheduled for 03:00."
func TestBusyEntryDoesNotFire(t *testing.T)    // ThreadBusy true at 03:00: no Receive; the firing is dropped (none at the next tick); it fires the next night
func TestCatchUpFiresLatestOnce(t *testing.T)  // down from 02:00 on day 1 to 08:00 on day 3: exactly one Trigger, for day 3's 03:00; Text says "Why now: catch-up, due 03:00, 5h late; the repository may have moved since."
func TestMissedOver24hFiresNothing(t *testing.T)
func TestRestartDoesNotRefire(t *testing.T)    // a new Source over the same KV at 03:00:40 fires nothing
func TestCrashBeforeStateSaveIsDuplicateOnly(t *testing.T) // Receive said stored, KV write failed: next tick's Receive answers duplicate; one Trigger
func TestEditedCronStartsOver(t *testing.T)    // the Spec changes: the tick records a new Cursor and fires nothing for the past
func TestPausedHoldsAndFiresAfter(t *testing.T) // paused over 03:00: no Receive, Cursor unchanged; unpaused at 03:20: fires once
func TestUnknownRepoHoldsAndReports(t *testing.T) // Known false: no Receive, Health non-nil; Known true: fires, Health nil
func TestDailyCapSurvivesRestart(t *testing.T) // 24 firings on one date, then a new Source: the 25th is dropped; the next date fires
func TestFallBackFiresOnce(t *testing.T)       // Europe/Lisbon 2026-10-25, "30 1 * * *": one Trigger ID across both 01:30s
func TestListens(t *testing.T)                 // enabled entry true; other slug false; github: false
```

In `internal/core/intake_test.go`: `TestThreadBusy`, one case per reason (queued, Eval, Brake, open Ticket subscribed), and false for an ended Ticket's Subscription and for an unknown Thread.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/sources/schedule/ ./internal/core/ -run 'ThreadBusy|Fires|Busy|CatchUp|Restart|Edited|Paused|Unknown|DailyCap|FallBack|Listens|Missed|NewEntry|Crash'`
Expected: FAIL.

- [ ] **Step 3: Implement** clock firings, as the tests pin them

The tick clamps `Cursor` to `now - CatchUpWindow`, walks `Next` to the latest firing at or before `now`, and acts on that one. A run more than `catchUpGrace` late carries the catch-up line. `Receive` comes before the KV write: the Trigger ID makes the repeat harmless, and the reverse order could lose a firing. Local date for `DailyCap` is the wall date in `time.Local` (`cfg.Now()`'s Location in tests).

- [ ] **Step 4: Run tests, then `make check`**

Expected: PASS.

- [ ] **Step 5: Commit**: `feat(schedule): a Source that fires Autopilot entries on a clock`

---

### Task 5: Repository events

**Files:**
- Create: `internal/sources/schedule/events.go`, `internal/sources/schedule/events_test.go`, `internal/sources/github/watch.go`, `internal/sources/github/watch_test.go`

**Interfaces:**
- Consumes: Task 4's `fire`, state document and `Config`; `github.Client.Get` and `Client.Cached`.
- Produces:

```go
// schedule
type CIState int
const (CIUnknown CIState = iota; CIGreen; CIRed) // Unknown: no suites yet, or none failed and some still run
var ErrNoCompare = errors.New("schedule: these commits cannot be compared")
type Repos interface {
	Head(ctx context.Context, repo string) (branch, sha string, err error)
	ChangedFiles(ctx context.Context, repo, base, head string) ([]string, error) // ErrNoCompare after a force-push
	CI(ctx context.Context, repo, sha string) (CIState, error)
}

// github
func NewWatcher(c *Client, appSlug string) *Watcher // implements schedule.Repos
```

Trigger IDs: `schedule:<slug>:head:<sha>`, `schedule:<slug>:manifest:<sha>`, `schedule:<slug>:ci:<RedSince unix>`.

Manifest file names (base name, any depth): `go.mod`, `go.sum`, `package.json`, `package-lock.json`, `pnpm-lock.yaml`, `yarn.lock`, `requirements.txt`, `pyproject.toml`, `poetry.lock`, `uv.lock`, `Pipfile`, `Pipfile.lock`, `Cargo.toml`, `Cargo.lock`, `Gemfile`, `Gemfile.lock`, `composer.json`, `composer.lock`, `pom.xml`, `build.gradle`, `build.gradle.kts`.

- [ ] **Step 1: Write the failing tests** in `events_test.go`, with a fake `Repos`

```go
func TestFirstSightSetsBaseline(t *testing.T)     // branch_moved and manifest_changed: Base = head, no Trigger
func TestBranchMovedFiresOnNewestHead(t *testing.T) // head a→b→c between polls: one Trigger for c; "Why now: main of o/r moved from <a7> to <c7>."
func TestBranchMovedBusyKeepsEvent(t *testing.T)  // busy at b: no Trigger, Base stays a; free at the next poll: fires for b
func TestBranchMovedRespectsGap(t *testing.T)     // fired for b at t; c arrives at t+6m: no Trigger; at t+16m: fires for c
func TestManifestFiresOnListedFile(t *testing.T)  // web/package.json in the diff: fires; "Why now: dependency manifests changed on main of o/r: web/package.json."
func TestManifestIgnoresOtherFiles(t *testing.T)  // README.md only: no Trigger, Base advances
func TestManifestNotComparedWhileBusy(t *testing.T) // busy: ChangedFiles is not called
func TestManifestForcePush(t *testing.T)          // ErrNoCompare: Base advances, no Trigger, Health nil
func TestRedClockStartsAtFirstSight(t *testing.T) // ci_red After 4h: red first seen at t: nothing at t+3h59m; one Trigger after t+4h; "Why now: CI on main of o/r has been red for 4h."
func TestRedFiresOncePerEpisode(t *testing.T)     // still red next day: nothing; green; red again 4h: fires again
func TestUnknownCIKeepsClock(t *testing.T)        // CIUnknown between reds: RedSince unchanged
func TestOneHeadReadPerRepo(t *testing.T)         // two entries on one repo: Head called once per poll
func TestNoEventEntriesNoCalls(t *testing.T)      // cron entries only: Repos untouched
func TestEventErrorsReportAfterThree(t *testing.T) // three failed polls: Health non-nil; a good poll clears it
```

In `watch_test.go`, over an `httptest` server: `TestHeadReadsDefaultBranch`; `TestHeadUsesCachedBodyOn304`; `TestCI`, a table of suites (failure → Red; timed_out → Red; success → Green; cancelled and success → Green; in_progress and success → Unknown; failure and in_progress → Red; none → Unknown; a failing suite of the App's own slug is ignored); `TestChangedFilesAcrossPages`; `TestChangedFiles404IsErrNoCompare`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/sources/schedule/ ./internal/sources/github/ -run 'Head|CI|Changed|Branch|Manifest|Red|Event|Sight'`
Expected: FAIL.

- [ ] **Step 3: Implement**

The event poll runs on its own timer inside `Run`. It skips everything when paused. Dropped events change no state (Ruling D). `Watcher` uses the same read-then-`Cached`-on-304 pattern for every GET. Compare the paths and the 404 error with what `source.go` uses (`repo.path()`, `threadRead`). A comparison of more than GitHub's 300-file limit is judged on the files returned; say so in a comment.

- [ ] **Step 4: Run tests, then `make check`**

Expected: PASS.

- [ ] **Step 5: Commit**: `feat(schedule): start Autopilot entries on repository events`

---

### Task 6: Wiring, health and the brakes end to end

**Files:**
- Modify: `internal/server/server.go`, `internal/server/views.go`, `internal/server/wiring.go`, `internal/server/fakegithub_test.go`
- Create: `internal/server/autopilot.go`, `internal/server/autopilot_test.go`

**Interfaces:**
- Consumes: `schedule.New`, `github.NewWatcher`, `policyCache.current`, `killSwitch.Paused`, `sourceReg.Listens`.
- Produces: `func autopilotEntries(s policy.Settings) []schedule.Entry`: the enabled entries, with slug and `CIRedAfter` filled. The schedule Source appears in `health().Sources` under its name. `server.Config` gains the test-only `ScheduleEventInterval time.Duration`.

- [ ] **Step 1: Write the failing tests** in `autopilot_test.go`, following `startRoutedTicket` in `shadow_test.go`

`fakeGitHub` gains a settable default-branch head and a compare route.

```go
func TestAutopilotEntriesMapping(t *testing.T)     // disabled left out; ci_red 0 → 4h; slug filled
func TestAutopilotObserveRecordsWouldAccept(t *testing.T)
// branch_moved entry, mode observe: move the head; the router's fake assessment accepts; a Decision "ignore" with WouldAccept true; no Ticket.
func TestAutopilotAutoOpensTicket(t *testing.T)
// mode auto: Ticket with Repo set, Mandate.Via "autopilot", Trust "owner"; a further head move while it is open adds no Trigger and no Input.
func TestAutopilotBrakedThenReleasedOnce(t *testing.T)
// ceiling 1 and one person-stage Ticket in the repo: Decision reason wip_brake, a Brake on schedule:<slug>; more head moves add nothing; when the owner catches up, one Eval runs.
func TestAutopilotNothingFiresWhilePaused(t *testing.T)
// pause, move the head: no Trigger; resume: one Trigger.
func TestAutopilotShadowRepoHoldsOutput(t *testing.T)
// repository in shadow: the Ticket's reply or pull request is a Proposal, as for any other Ticket.
func TestAutopilotHealthShown(t *testing.T)        // entry on an unpolled repository: the schedule row of health carries the error
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/server/ -run Autopilot`
Expected: FAIL.

- [ ] **Step 3: Implement**

Register the schedule Source only when `routerOn`, since nothing could assess its candidates without a router. `Known` is `sourceReg.Listens(github.ThreadKey(repo, 1))`, so an entry waits until the GitHub Source is up and polling that repository. `Repos` is `github.NewWatcher(gh.app.Client(), slug)`, with the slug `NewSource` uses. The registry's `Start` and `Wait` already cover start-up and shutdown.

- [ ] **Step 4: Run tests, then `make check`**

Expected: PASS.

- [ ] **Step 5: Commit**: `feat(server): run the schedule Source behind the brakes`

---

### Task 7: Settings › Autopilot

**Files:**
- Create: `web/src/features/settings/autopilot.ts`, `autopilot.test.ts`, `Autopilot.tsx`, `Autopilot.test.tsx`
- Modify: `web/src/features/settings/Settings.tsx`, `web/src/features/routing/Triggers.tsx`, `web/src/features/routing/Routing.test.tsx`, `web/e2e/settings.spec.ts`

**Interfaces:**
- Consumes: `usePolicyDoc`, `savePolicy`, `useSaveBlock`, `SaveBar`, `Section`, `Block`, `useAthena((s) => s.repos)`. Copy the structure of `Repositories.tsx`, and the add/remove/focus handling of `Rules` in `PolicyForm.tsx`.
- Produces in `autopilot.ts`, kept out of `policy.ts` so the initial route does not grow:

```ts
export type When = "cron" | "branch_moved" | "manifest_changed" | "ci_red";
export type Entry = { key: string; name: string; instructions: string; repo: string; enabled: boolean; when: When; cron: string; ciRedHours: number; orig: Json };
export function readAutopilot(doc: Json): Entry[];
export function writeAutopilot(doc: Json, entries: Entry[]): string; // keeps unknown fields of each entry (orig); no key when empty
export function equalAutopilot(a: Entry[], b: Entry[]): boolean;
export function validateAutopilot(entries: Entry[]): string | undefined; // policy.Validate's checks, minus cron meaning, which the server answers
export function autopilotSlug(name: string): string;
export const CRON_PRESETS: { label: string; cron: string }[];
// Every hour "0 * * * *"; Every night at 03:00 "0 3 * * *"; Weekdays at 09:00 "0 9 * * 1-5"; Mondays at 09:00 "0 9 * * 1"
```

Copy (exact): section label **Autopilot**, after Trust in `SECTIONS`. Section text **"Work Athena starts on its own, on a clock or when a repository changes."** Trigger select: **On a schedule**, **When the default branch moves**, **When dependency files change**, **When CI stays red**. Mode line, by Policy mode:
- `off`: **"Accept mode is off, so these entries do nothing yet."**
- `observe`: **"Accept mode is observe, so Athena only records what it would have started."**
- `auto`: no line.

Under each saved entry, a link **See its runs** to `/routing?q=schedule:<slug>`. Under the cron field: **"Minute, hour, day, month, weekday, in this server's local time. At least 15 minutes apart."**

- [ ] **Step 1: Write the failing tests**

`autopilot.test.ts`: read and write round-trip; unknown fields kept; `validateAutopilot` for each refusal (no name, two names with one slug, no instructions, no repository, cron not five fields); the `ciRedHours` default of 4; the key is dropped when empty.

`Autopilot.test.tsx`: adds an entry and saves the expected document; choosing a preset fills the cron field; changing Trigger hides the cron field and shows hours for CI; a server error shows in the save bar and keeps the edit; the mode line matches the Policy; **See its runs** has the right `href` and is absent for an unsaved entry; no GitHub repository known shows a line instead of an empty select.

`Routing.test.tsx`: `?q=schedule:night` opens with the search filled and filtered.

- [ ] **Step 2: Run to verify they fail**

Run: `cd web && npx vitest run src/features/settings/autopilot src/features/settings/Autopilot src/features/routing/Routing`
Expected: FAIL.

- [ ] **Step 3: Implement** and add `"autopilot"` to `SECTIONS` in `web/e2e/settings.spec.ts`

- [ ] **Step 4: Run `make check`, then `make e2e`**

Expected: PASS, with the new baselines at 390, 820 and 1440 px, light and dark, axe clean, and no horizontal scroll at 320 px. Report the bundle size against the budget.

- [ ] **Step 5: Commit**: `feat(web): edit Autopilot entries in Settings`

---

### Task 8: Docs and the roadmap

**Files:**
- Modify: `docs/dev/autonomy.md`, `docs/dev/smoke-test.md`, `docs/superpowers/plans/2026-11-03-athena-roadmap.md`, `docs/superpowers/specs/2026-11-03-athena-design.md`

- [ ] **Step 1: Write the docs**

- `autonomy.md`: a new section "Autopilot": what an entry is; the path of a firing (Source, gate, Eval, Ticket); a worked example of one nightly entry in `observe` and then `auto`; a table of each of Rulings C–I in the owner's words; how the WIP brake, kill switch and shadow mode apply; and that Autopilot work runs with the `owner` trust class. Add it to the settings table in "Where each setting lives", and to the what-each-brake-stops table.
- `smoke-test.md`: "Autopilot (needs your GitHub App)": set up a `branch_moved` entry in `observe`, push, see the would-accept on Routing; switch to `auto` on a throwaway repository, push, see a Ticket; pause, push, resume.
- Spec §18.5: one paragraph listing the settled values (15 minutes, 24 a day, 4 h, one run at a time, one catch-up), pointing at `autonomy.md`.
- Roadmap: a "**Plan 11 is built**" note under the existing plan 10 note. The plans view counts plans by that phrase.

- [ ] **Step 2: Check the docs against the code**

Every constant named in `autonomy.md` matches `internal/sources/schedule` and `internal/policy`; every path in the smoke test exists.

- [ ] **Step 3: Run `make check`**

Expected: PASS.

- [ ] **Step 4: Commit**: `docs: Autopilot`
