# Athena Plan 4: Dashboard UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `athena server` serves the real dashboard: an installable React PWA in the Precision style. It works equally well on a phone and a desktop, and the owner uses it to:
- see what needs them and act on it in one tap
- watch any session live and type into it
- start Tickets
- follow routing (Triggers, Threads, Decisions) and the task graph
- manage the node, devices, push, Policy and prompts

The UI covers every feature plan 3's backend serves. Plan 4 adds the few backend pieces the UI still needs: known repositories, notification preferences, timeline completeness, enrolling another device from the dashboard, Ticket diffs and files, and prompt overrides.

**Architecture:**
- **App.** `web/` is a Vite + React 19 + TypeScript app.
- **Link and state.** It speaks plan 3's link protocol from TypeScript (`protobuf-es` types generated from `proto/`). One `LinkClient` per tab feeds a Zustand store with the Snapshot and Deltas.
- **Commands.** Every action is a Command kept in an IndexedDB outbox until its CommandResult arrives, and resent by id after a reconnect (exactly-once on the server).
- **Build and serving.** The built app goes into `internal/web/ui/dist` and is embedded in the Go binary. The server serves it with an SPA fallback, under plan 3's unchanged CSP.
- **Service worker.** It gives an offline shell and Web Push.
- **New backend pieces.** They follow plan 3's patterns: views in `internal/hub`, commands in `internal/server/dispatch.go`, and HTTPS GET endpoints in `internal/web` for content too large for a frame (diffs, files).

**Tech Stack:**
- **Already in the repo:** Go 1.27; `buf` with a new TS target `@bufbuild/protoc-gen-es`.
- **Web runtime:** React ≥ 19, Vite, TypeScript (strict), Tailwind CSS ≥ 4 (`@tailwindcss/vite`), shadcn/ui (Radix primitives, copied into the repo and themed), Zustand, react-router ≥ 7 (library mode), `@bufbuild/protobuf` ≥ 2, `cmdk`, `lucide-react`, `uqr` (QR as SVG), and `@fontsource/ibm-plex-sans` / `@fontsource/ibm-plex-mono`.
- **Web tests:** Vitest with jsdom, Testing Library and `fake-indexeddb`; Playwright with `@axe-core/playwright`.
- **Package manager:** npm, with `package-lock.json` committed.

**Spec:** `docs/superpowers/specs/2026-11-03-athena-design.md`. Read:
- §2 goals 2, 5, 6, 7 and 9, and the anti-goals
- §8.3
- §10 and §10.1 in full (the design bar and views 1–6)
- §10.2 "Surfaces" for what is **not** here (plan 9)
- §11 dashboard login
- §18.2 (the kill switch is plan 10; it subsumes "Pause processing")

Also read:
- `docs/dev/dashboard.md` "For developers: the client contract": the link rules this client must follow.
- Plan 3's deferred items, `docs/superpowers/plans/2026-11-04-athena-p3-deferred.md`, section "Carry into plan 4".

**Mockups** (the owner chose Precision from them). They live in the git-ignored `.superpowers/brainstorm/28700-1793722207/content/`, and Task 1 copies them into `docs/design/mockups/`:
- `visual-direction.html`: option C is the phone "Needs you" screen.
- `desktop-layout.html`: three panes.
- `task-graph.html`
- `system-visibility.html`: plan 9, for reference only.

## Global Constraints

- Every plan 1–3 Global Constraint still holds, plan 3's CSP included, unchanged: no inline scripts, styles or `eval`, and no third-party origins.
- `make check` passes at the end of every task. From Task 1 on it also runs `make web-check`.
- **Dependencies:**
  - **Go:** no new Go modules.
  - **Web, runtime:** exactly the libraries in Tech Stack, plus the `@radix-ui/react-*` packages that the chosen shadcn components import, `class-variance-authority`, `clsx` and `tailwind-merge`.
  - **Web, dev:** `vite`, `@vitejs/plugin-react`, `typescript`, `vitest`, `jsdom`, `@testing-library/react`, `@testing-library/user-event`, `fake-indexeddb`, `@playwright/test`, `@axe-core/playwright`, `@bufbuild/protoc-gen-es`, `eslint`, `typescript-eslint` and `eslint-plugin-react-hooks`.
  - **Versions:** pinned exactly (no `^` or `~`). Each is the newest stable release that is at least 14 days old when Task 1 installs it.
  - Any other package needs a ruling.
- **Build:**
  - Generated TS (`web/src/gen/`) and the Go protobuf code are committed. `buf generate` writes both, and `make check` fails if they drift.
  - `internal/web/ui/dist/` is git-ignored except `.gitkeep`, so `go build` and `go test` work without Node. When `index.html` is missing, the server serves a plain fallback page saying `Dashboard not built: run make web`.
  - `make build` runs `make web` first.
- **Precision tokens:** CSS variables on `:root` and `[data-theme=dark]`. The light theme is exactly these values:

  | Token | Value |
  | --- | --- |
  | `--bg` | `#FFFFFF` |
  | `--surface` | `#FFFFFF` |
  | `--surface-2` | `#F5F5F5` |
  | `--stage` | `#E9E9E9` |
  | `--line` | `#111111` (1 px hairline) |
  | `--line-soft` | `#D9D9D9` |
  | `--text` | `#0A0A0A` |
  | `--muted` | `#6B6B6B` |
  | `--accent` | `#FF5A1F` |
  | `--accent-fg` | `#0A0A0A` |
  | `--accent-ink` | `#C2410C` |
  | `--ok` | `#0A0A0A` |
  | `--danger` | `#B42318` |

  The dark theme inverts the surfaces and keeps the orange:

  | Token | Value |
  | --- | --- |
  | `--bg` | `#0A0A0A` |
  | `--surface` | `#0F0F0F` |
  | `--surface-2` | `#1A1A1A` |
  | `--stage` | `#000000` |
  | `--line` | `#EDEDED` |
  | `--line-soft` | `#2E2E2E` |
  | `--text` | `#F5F5F5` |
  | `--muted` | `#9A9A9A` |
  | `--accent` | `#FF5A1F` |
  | `--accent-fg` | `#0A0A0A` |
  | `--accent-ink` | `#FF5A1F` |
  | `--ok` | `#F5F5F5` |
  | `--danger` | `#FF6B5E` |

  Other tokens:
  - `--radius: 4px`
  - Type: IBM Plex Sans for the UI and IBM Plex Mono for ids, commands, metadata and session output. Headings weigh 600; body text is 15 px on the phone and 14 px on the desktop.
  - Spacing on a 4 px grid.
  - Motion is 120–200 ms ease-out, and none under `prefers-reduced-motion`.

  Orange marks only what needs the owner. Orange *text* uses `--accent-ink`; an orange fill carries `--accent-fg` text. Every text and background pair meets 4.5:1, and hairlines and indicators meet 3:1. Task 5 enforces this with a test.
- **Layouts:**
  - Under 1024 px: one column with bottom navigation (Needs you · Tickets · New · Graph · Settings), safe-area insets, 44 px minimum touch targets, and no horizontal scroll from 320 px up.
  - At 1024 px and wider: three panes, laid out as sidebar · list · detail. The sidebar holds the navigation, the node with live load and today's counts. Wide screens can pin up to 3 live sessions side by side.
  - Screenshots are taken at 390, 820 and 1440 px, light and dark.
- **Keyboard** (desktop):

  | Key | Action |
  | --- | --- |
  | `⌘K` / `Ctrl K` | command palette |
  | `j` / `k` | move |
  | `⏎` | open |
  | `a` | approve |
  | `d` | deny |
  | `r` | reply |
  | `⌘⏎` | interrupt the session |
  | `/` | search |
  | `g n` | go to Needs you |
  | `g t` | go to Tickets |
  | `g g` | go to Graph |
  | `?` | shortcut list |
  | `Esc` | close |

  Single-key shortcuts never fire while focus is in a text field.
- **Copy:**
  - Domain terms: Ticket, Approval, Trigger, Thread and Decision. A stage label says whom a row waits on and what it needs (reference panel principle 2).
  - Stage labels:

    | Stage / action | Label |
    | --- | --- |
    | `person/answer` | "Needs your answer" |
    | `person/approval` | "Needs approval" |
    | `person/review` | "Ready for review" |
    | `person/delivery` | "Delivery failed" |
    | `person/blocked` | "Blocked" |
    | `person/failed` | "Failed" |
    | `working/queued` | "Queued" |
    | `working` | "Working" |
    | `idle` | "Idle" |
    | `ended` | the outcome: "Merged", "Stopped", "Dropped", "Completed" or "Failed" (from `outcome`) |

  - Destructive actions (Stop, Revoke, Reset prompt) need confirmation.
  - Each page's explanation is at most one sentence; any more goes in a tooltip.
- **Honest state:** a count always equals the length of the list it links to (reference panel principle 5).
- **Client link rules:** follow `docs/dev/dashboard.md`'s client contract:
  - Hello with `client_id`, `epoch` and `cursor`; answer Ping with Pong.
  - Ack durable frames.
  - Re-grant Credit after every reconnect.
  - Re-send WatchTicket after a reconnect.
  - Ignore `seq` 0 deltas for cursor and gap tracking.
  - Treat close 4002 as signed out, and 1001/1006/1011/1013 as "reconnect".
- **Reconnect backoff:** 0.5 s doubling to 15 s, with ±20 % jitter. It resets after 10 s connected and fires at once on `online` and `visibilitychange` to visible.
- **The browser never polls.** The only HTTP calls are:
  - auth (`/auth/*`)
  - `/push/key`
  - `/api/tickets/{id}/diff`, `/api/tickets/{id}/files` and `/api/tickets/{id}/files/{path...}` (Task 3)
  - `/auth/enroll-link` (Task 2)
- **Storage on the device:**
  - `localStorage` holds only `athena.clientId` (random, 16 base32 characters) and `athena.theme` (`system|light|dark`).
  - IndexedDB `athena` holds only the `outbox` store.
  - No cookies are set by JS, and no secret is stored on the device.
- **Bundle budget:** the initial route loads at most 200 KB of JS and 30 KB of CSS, gzipped. Graph, Settings › Prompts and the diff viewer are lazy chunks. `make web-check` enforces it.
- **Responsiveness:** applying a Delta and rendering it takes under 16 ms with 500 Tickets in the store. Lists render at most 200 rows, then "Show more". The live session view keeps at most 2000 items.
- **Browsers:** current Chrome, Edge, Firefox and Safari; iOS/iPadOS Safari 17 and later. Home-screen web push needs 16.4 or later.

## Review Focus

These are the inputs the spec implies that are most likely to bite the owner, most likely first. Each one has its test in the owning task.

1. **The phone sleeps or loses the network mid-action.** A tap on Approve, then a lost link, then a reconnect: the approval happens once, and the card shows one result, never "failed" followed by a second approve. Task 7: `outbox resends by id after reconnect and resolves once`.
2. **The session ends while the app is open.** The passkey was revoked on another device, the session expired, or the owner revoked this very device. The app shows sign-in at once, keeps unsent commands, and sends them after sign-in. Task 8: `close 4002 signs out and keeps the outbox`.
3. **Hostile or huge content.** A 2,000-character title, a tool result of 1 MiB, ANSI escapes, RTL text, or a 10,000-line diff. None of it breaks the layout or adds horizontal scroll: text wraps or truncates with a full view, and ANSI is stripped. Tasks 11, 12 and 3: `long and ANSI content stays inside the column`.
4. **Two screens edit the same settings.** A stale `base_revision` gets `conflict`. The form keeps the owner's edits, shows "Changed on another device", and offers to reload or overwrite with the fresh revision. Task 17: `policy conflict keeps edits and offers reload`.
5. **Many records.** With 500 Tickets, 2000 stream items and 200 Decisions, the app stays responsive and counts match lists. Task 6: `500 tickets apply under budget`. Task 12: `stream keeps 2000 items`.

## File structure

```
proto/athena/v1/state.proto        + RepoView, RepoList, NotificationPrefsView, PromptsView; TicketSummary.branch/runtime; TicketDetail.timeline_trimmed
proto/athena/v1/commands.proto     + GetPrompt, SetPrompt, ResetPrompt; CommandResult.prompt
buf.gen.yaml                       + es target → web/src/gen
internal/hub/views.go              repos, notifications and prompts views; timeline replies/answered approvals; SQL-bounded snapshot reads
internal/store/tickets.go          bounded Tickets/AnsweredApprovals queries (plan 3 deferred)
internal/store/prompts.go          migration 6: prompt_revisions
internal/web/enroll_link.go        POST /auth/enroll-link
internal/web/artifacts.go          GET /api/tickets/{id}/diff|files|files/{path...}
internal/node/artifacts.go         TicketDiff, TicketFiles, OpenTicketFile (git without a shell, os.Root)
prompts/prompts.go                 Parts(), Effective(overrides)
internal/web/ui/embed.go           //go:embed all:dist; FS(), Built()
internal/web/server.go             SPA serving + fallback page; placeholder assets removed
web/                               the app (package.json, vite.config.ts, index.html, src/…)
web/src/gen/                       protobuf-es output (committed)
web/src/lib/link/                  frame.ts, client.ts (LinkClient)
web/src/lib/store/                 state.ts (Zustand), selectors.ts
web/src/lib/commands/              outbox.ts, actions.ts
web/src/lib/auth/                  webauthn.ts, session.ts
web/src/lib/stream/                useSessionStream.ts, ansi.ts
web/src/ui/                        tokens.css, theme, shadcn components (ui/primitives), Athena primitives
web/src/app/                       routes, layouts (PhoneShell, DesktopShell), palette, shortcuts, connection banner
web/src/features/                  needs-you, tickets, session, new-task, routing, graph, nodes, settings, auth
web/src/sw.ts                      service worker: precache, push, notificationclick
web/e2e/                           Playwright: harness (real server + fakeclaude + virtual authenticator), flows, screenshots
docs/design/mockups/               the four mockups, committed
docs/dev/dashboard.md              "Working on the UI" section; feature notes
Makefile                           web, web-check, e2e; check depends on web-check
```

---

### Task 1: Web app scaffold, TS generation and serving the build

**Files:**
- Create:
  - `web/package.json` and `web/package-lock.json`
  - `web/tsconfig.json`, `web/vite.config.ts`, `web/eslint.config.js`, `web/index.html`
  - `web/src/main.tsx` and `web/src/app/App.tsx`, with one "Athena" heading route
  - `web/scripts/bundle-budget.mjs`
  - `internal/web/ui/embed.go` and `internal/web/ui/dist/.gitkeep`
  - `docs/design/mockups/*.html`, copied from the mockup directory
- Modify:
  - `buf.gen.yaml`: add an `es` plugin, `local: web/node_modules/.bin/protoc-gen-es`, out `web/src/gen`, options `target=ts` and `import_extension=js`.
  - `internal/web/server.go`
  - `Makefile`, `.gitignore`, `docs/dev/dashboard.md`
- Delete: `internal/web/assets/index.html`, `enroll.html`, `app.js`, and their routes.
- Test: `internal/web/server_test.go`, `web/src/app/App.test.tsx`, `web/src/gen/gen.test.ts`

**Interfaces:**
- Produces:
  - `ui.FS() fs.FS`: the embedded `dist`.
  - `ui.Built() bool`: true when `index.html` is present.
  - `web.ServerConfig.UI fs.FS`: tests inject a `fstest.MapFS`.
  - Generated TS types: `import { FrameSchema, … } from "@/gen/athena/v1/link_pb.js"`.
  - The path alias `@/` maps to `web/src/`.
- Serving, on the `GET /` catch-all, registered last:
  - **A file that exists:**
    - Files under `/assets/` get `Cache-Control: public, max-age=31536000, immutable`.
    - Everything else, `sw.js` and `manifest.webmanifest` included, gets `no-cache`.
    - The content type comes from the extension; `.webmanifest` is `application/manifest+json`.
  - **A path with no extension:** serve `index.html` with `no-cache`. This is the SPA fallback, `/enroll` and `/tickets/T-1` included.
  - **Anything else:** 404.
  - **Not built:** when `ui.Built()` is false, every SPA path gets the fallback page. It is plain HTML with no script, and its text is `Dashboard not built: run make web`.
  - Security headers are unchanged, and the CSP string stays byte-identical.
- Vite:
  - `root: web`, with `build.outDir` set to `../internal/web/ui/dist` and `emptyOutDir: true`.
  - Hashed assets go under `assets/`.
  - No inline script or style in the built HTML: check that `index.html` has no `<script>` without `src` and no `style=`.
  - The dev server proxies `/ws`, `/auth`, `/push` and `/api` to `ATHENA_DEV_SERVER` (default `http://127.0.0.1:8443`), rewriting `Origin` to that server's origin.
- Make:

  | Target | Runs |
  | --- | --- |
  | `web` | `npm --prefix web ci` (only if `web/node_modules` is missing), then `npm --prefix web run build`, then `touch internal/web/ui/dist/.gitkeep` |
  | `web-check` | `typecheck` (tsc --noEmit), `lint`, `test` (vitest run), then `make web`, then `bundle-budget` |
  | `check` | adds `web-check` after the Go checks |
  | `build` | depends on `web` |
  | `e2e` | Task 9 fills it in |

  The proto drift check regenerates both Go and TS.
- `bundle-budget.mjs` reads `dist/.vite/manifest.json`, sums the gzip size of the entry chunk's static imports, and fails over 200 KB JS or 30 KB CSS. Each budget line it prints looks like `bundle js 42.1 KB / 200 KB`.

- [ ] **Step 1: Write the failing Go tests:**
  - `TestSPAServesAssetsImmutable`
  - `TestSPAFallbackServesIndexNoCache`: `/tickets/T-1` and `/enroll`.
  - `TestSPAUnknownFileIs404`: `/nope.js`.
  - `TestUnbuiltServesFallbackPage`: the body contains `Dashboard not built: run make web` and has no `<script`.
  - `TestCSPUnchanged`: the header equals plan 3's constant.
  - `TestPlaceholderRoutesGone`: `/app.js` gives 404.
- [ ] **Step 2: Write the failing web tests:**
  - `App renders the Athena heading`
  - `gen round-trips a Frame`: encode a `Hello`, decode it, and the fields are equal.
- [ ] **Step 3: Run** `go test ./internal/web` and `npm --prefix web test` → FAIL.
- [ ] **Step 4: Implement.**
  - Install the packages for this task: React, Vite, TS, Tailwind, protobuf-es, the test tooling and eslint. Later tasks add their own from the Global Constraints list.
  - Run `buf generate`.
  - Add the "Working on the UI" section to `docs/dev/dashboard.md`: `make web`, then `npm --prefix web run dev` against a TLS-off loopback server, then `make e2e`.
- [ ] **Step 5: Run** `make check` → PASS. `make build` produces a binary that serves the built heading at `/`.
- [ ] **Step 6: Commit** `feat(web): React app scaffold, TS protobuf generation, embedded SPA serving`.

### Task 2: Backend views the UI needs

**Files:**
- Modify:
  - `proto/athena/v1/state.proto`, then regenerate.
  - `internal/hub/views.go`, `internal/hub/hub.go`
  - `internal/server/hubsource.go`
  - `internal/store/tickets.go`
  - `internal/web/notify.go`: publish on save.
  - `internal/web/conn.go`: `fitDetail` sets `timeline_trimmed`.
  - `internal/web/server.go`: the route.
- Create: `internal/web/enroll_link.go`
- Test: `internal/hub/views_test.go`, `internal/server/hubsource_test.go`, `internal/web/enroll_link_test.go`, `internal/store/tickets_test.go`

**Interfaces:**
- Proto additions (field numbers are fixed):
  - `TicketSummary`: `string branch = 17`, the worktree branch `athena/<lowercased id>` (empty when there is no repository); `string runtime = 18`, `claude` today.
  - `TicketDetail`: `bool timeline_trimmed = 4`, set by plan 3's `fitDetail` when it drops entries.
  - `message RepoView { string name = 1; string kind = 2; google.protobuf.Timestamp last_used_at = 3; }`. `kind` is one of:
    - `github`: `owner/name`
    - `git`: a local repository path
    - `dir`: a plain folder
  - `message RepoList { repeated RepoView repos = 1; }`
  - `message NotificationPrefsView { repeated string kinds = 1; repeated string muted = 2; }`
  - `Snapshot`: `repeated RepoView repos = 10; NotificationPrefsView notifications = 11;`
  - `Delta`: `RepoList repos = 19; NotificationPrefsView notifications = 20;`
- Known repositories, `Source.Repos() []RepoView`:
  - Built from the configured GitHub repos and the distinct `repo`/`dir` of the 200 most recent Tickets.
  - Ordered by last use; configured repos that were never used go last, by name.
  - At most 50 entries.
  - A `dir` is `git` when it has a `.git` entry.
  - Published as a `repos` delta only when the list changes.
- Notifications:
  - `NotificationPrefsView` comes from `web.NotificationKinds()` and `MutedKinds`.
  - `SaveMutedKinds` success publishes the delta. Give the hub a `PublishNotifications()` entry point, or route it through the change feed with a kv key.
- Timeline: the timeline gains `reply` entries (owner and person replies delivered to the Ticket) and `approval` entries for answered Approvals (text `Approved: <tool>` / `Denied: <tool>`, plus the note). Plan 3 deferred these.
- Snapshot cost (plan 3 deferred):
  - `store.Tickets` gains `RecentlyEnded(limit int)`, and `store.AnsweredApprovals(limit int)` uses SQL `ORDER BY … LIMIT`.
  - The snapshot bounds stay: all open Tickets plus the 100 most recently ended, and pending Approvals plus the last 50 answered.
- `POST /auth/enroll-link`:
  - Needs the Origin check, a valid session and a passkey assertion within 5 min. Without the assertion the answer is 403 with `{"code":"reauth_required"}`.
  - The body is `{"ttlSeconds":N}` (optional; 1…86400, default `web.DefaultEnrollTTL`).
  - The answer is 200 with `{"url":"<origin>/enroll#<token>","expiresAt":"<RFC 3339>"}` and `Cache-Control: no-store`.
  - The token is never logged. This is HTTP, not a frame, so plan 3's "no enrollment token in a frame" holds.

- [ ] **Step 1: Write failing tests:**
  - `TestSnapshotHasReposNewestFirst`: configured `a/b` (never used), Tickets on `c/d` then `/tmp/x` give `[/tmp/x (dir), c/d (github), a/b (github)]`.
  - `TestReposDeltaOnlyOnChange`
  - `TestNotificationsInSnapshotAndDeltaAfterSave`
  - `TestTicketSummaryBranchAndRuntime`
  - `TestTimelineHasRepliesAndAnsweredApprovals`
  - `TestDetailTrimmedFlag`
  - `TestRecentlyEndedUsesLimit`: 300 ended Tickets give 100 rows. Use `EXPLAIN QUERY PLAN` or a row-count spy to show it does not scan everything into Go.
  - `TestEnrollLinkNeedsReauth`: 403 `reauth_required`.
  - `TestEnrollLinkIssuesUsableToken`: the returned fragment token registers a passkey through the SoftAuthenticator.
  - `TestEnrollLinkRejectsBadOriginAndTTL`
  - `TestEnrollLinkNotLogged`: the token is absent from captured slog output.
- [ ] **Step 2: Run** `go test -race ./internal/hub ./internal/server ./internal/web ./internal/store` → FAIL.
- [ ] **Step 3: Implement**, then run `buf generate`. Both Go and TS regenerate.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(hub): known repos, notification prefs, fuller timeline; enroll link endpoint`.

### Task 3: Ticket diffs and files over HTTPS

**Files:**
- Create: `internal/node/artifacts.go`, `internal/web/artifacts.go`
- Modify:
  - `internal/web/server.go` (routes)
  - `internal/server/wiring.go` (pass the node as `web.Artifacts`)
  - `docs/dev/dashboard.md` (client contract: the three endpoints)
- Test: `internal/node/artifacts_test.go`, `internal/web/artifacts_test.go`

**Interfaces:**
- `internal/node` produces:
  - `(*Node).TicketDiff(ctx context.Context, ticketID string) (Diff, error)`, where `Diff{Base, Branch string; Files []DiffFile; Patch string; Truncated bool}` and `DiffFile{Path, OldPath, Status string; Added, Deleted int; Binary bool}`.
  - `(*Node).TicketFiles(ticketID string) ([]FileEntry, bool /*truncated*/, error)`, where `FileEntry{Path string; Size int64; ModTime time.Time}`.
  - `(*Node).OpenTicketFile(ticketID, rel string) (*os.File, error)`
  - Errors: `ErrNoWorkspace` (unknown Ticket or none placed) and `ErrNotFound`.
- Diff rules:
  - Only for Tickets placed in a git repository.
  - `Branch` is `athena/<lowercased id>`.
  - The base is the merge-base of the branch with, first match wins, `refs/remotes/origin/HEAD`, then the main worktree's `HEAD`.
  - It runs `git -C <repo> diff --no-ext-diff --no-color --find-renames <base>...<branch>` twice, once with `--numstat` and once with the patch.
  - It uses `exec.CommandContext` with a 10 s timeout, never a shell, and a minimal env with `GIT_TERMINAL_PROMPT=0` and no `GIT_ASKPASS` or credentials.
  - The patch is cut at 2 MiB, with `Truncated` set.
- Files rules:
  - Only for Tickets placed without a repository; the Ticket directory is the root.
  - The `.athena/` directory and symlinks are skipped.
  - At most 1000 entries, breadth-first, sorted by path.
  - `OpenTicketFile` goes through `os.OpenRoot(ticketDir)`, so `..` and absolute paths fail. A path under `.athena/` is refused.
- HTTP routes, all `GET`, all needing a valid session (otherwise 401 `{"code":"unauthenticated"}`), all with `Cache-Control: no-store`:

  | Route | Answer |
  | --- | --- |
  | `/api/tickets/{id}/diff` | JSON of `Diff` (camelCase) |
  | `/api/tickets/{id}/files` | `{"files":[…],"truncated":bool}` |
  | `/api/tickets/{id}/files/{path...}` | the file, as an attachment with `X-Content-Type-Options: nosniff` and `Content-Type: application/octet-stream` |

  Errors: `ErrNoWorkspace` and `ErrNotFound` become 404 `{"code":"not_found"}`, and any other error becomes 500 `{"code":"internal"}`, logged without file contents.
- `web.Artifacts` is the interface over the three node methods, so web tests use a fake.

- [ ] **Step 1: Write failing tests** (`internal/node`: a temp git repo with a worktree on `athena/t-0001`, and a temp no-repo Ticket directory):
  - `TestTicketDiffAgainstMergeBase`: 2 changed files with the right numstat; the patch contains `+new line`.
  - `TestTicketDiffTruncatesAt2MiB`
  - `TestTicketDiffNoShellNoCredentialsEnv`: run git through a test hook that records argv and env.
  - `TestTicketFilesSkipsAthenaDirAndSymlinks`
  - `TestOpenTicketFileRefusesEscape`: `../x`, `/etc/passwd` and `.athena/socket` all fail.
  - Web: `TestArtifactsNeedSession`, `TestArtifactsNotFound`, `TestFileDownloadHeaders`.
- [ ] **Step 2: Run** `go test -race ./internal/node ./internal/web` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(web): ticket diff and files endpoints`.

### Task 4: Prompt overrides and versions

The reference's Prompts settings (`docs/reference/005-panel.md` "Prompts") let the owner edit prompt parts, see the full assembled prompt and the history, and handle a default that changed under an override.

**Files:**
- Create: `internal/store/prompts.go` (migration 6)
- Modify:
  - `prompts/prompts.go`
  - `internal/node/node.go:935` and `internal/router/host.go:338` (use the effective prompt)
  - `proto/athena/v1/state.proto`, `proto/athena/v1/commands.proto`
  - `internal/server/dispatch.go`, `internal/hub/views.go`
- Test: `prompts/prompts_test.go`, `internal/store/prompts_test.go`, `internal/server/dispatch_test.go`, `internal/hub/views_test.go`

**Interfaces:**
- `prompts` package:
  - `Parts() []Part`, with `Part{Name, Default string}` in the order `worker`, `router`, `admission`, `replies`, `subscriptions`.
  - `Effective(over map[string]string) (worker, router string)`: an override replaces its part's default before `compose`.
  - `Hash(s string) string`: SHA-256 hex.
  - `Worker` and `Router` stay as the defaults' compositions.
- Store:
  - Migration 6 creates `prompt_revisions(revision INTEGER PRIMARY KEY, part TEXT NOT NULL, text TEXT, default_hash TEXT NOT NULL, default_text TEXT NOT NULL, origin TEXT NOT NULL, at INTEGER NOT NULL)`.
  - A NULL `text` is a reset to the default.
  - `PromptOverrides() (map[string]PromptOverride, uint32 /*latest revision*/, error)`
  - `SavePrompt(part string, text *string, base uint32, defaultText string, origin string) (uint32, error)` returns `store.ErrConflict` when `base` ≠ the latest revision.
  - `PromptHistory(part string, limit int)`
- Sessions:
  - The node reads the effective worker prompt when it spawns a new session, and the router host does the same for the router.
  - A resumed session keeps the prompt it was started with. The UI says so in one sentence.
- Proto:
  - `message PromptPartView { string name = 1; bool overridden = 2; bool default_updated = 3; }`. `default_updated` means the override's `default_hash` ≠ `Hash(current default)`.
  - `message PromptsView { uint32 revision = 1; repeated PromptPartView parts = 2; }`
  - `Snapshot.prompts = 12` and `Delta.prompts = 21`.
  - Commands:
    - `GetPrompt get_prompt = 28 { string part = 1; }`, not sensitive.
    - `SetPrompt set_prompt = 29 { string part = 1; uint32 base_revision = 2; string text = 3; }`, sensitive.
    - `ResetPrompt reset_prompt = 30 { string part = 1; uint32 base_revision = 2; }`, sensitive.
  - `CommandResult.prompt = 13`: a `PromptDetail { string name = 1; string default_text = 2; string text = 3; string previous_default_text = 4; string full_worker = 5; string full_router = 6; repeated PromptRevision history = 7; }`
  - `PromptRevision { uint32 revision = 1; google.protobuf.Timestamp at = 2; string origin = 3; bool reset = 4; }`
  - History holds the last 20 entries.
- Behaviour:
  - "Keep my text" is a `SetPrompt` with the same text, which records the current default's hash.
  - "Use the new default" is a `ResetPrompt`.
  - An empty `text` in `SetPrompt` is `invalid`; use `ResetPrompt` instead.
  - A text over 64 KiB is `invalid`.
  - An unknown part is `not_found`.

- [ ] **Step 1: Write failing tests:**
  - `TestEffectiveReplacesOnePart`
  - `TestSavePromptConflictsOnStaleBase`
  - `TestDefaultUpdatedWhenDefaultHashDiffers`: store an override saved against an old default text.
  - `TestKeepMyTextClearsDefaultUpdated`
  - `TestNewSessionUsesOverride`: the fake runtime's args log shows the override text in `--append-system-prompt`.
  - `TestResumedSessionKeepsPrompt`
  - `TestSetPromptIsSensitive`: `reauth_required` without a recent assertion.
  - `TestGetPromptReturnsFullAndHistory`
  - `TestPromptsDeltaOnSave`
- [ ] **Step 2: Run** `go test -race ./prompts ./internal/store ./internal/server ./internal/hub ./internal/node` → FAIL.
- [ ] **Step 3: Implement**, then run `buf generate`.
- [ ] **Step 4: Run** `make check` → PASS.
- [ ] **Step 5: Commit** `feat(prompts): owner overrides with revisions and default-updated detection`.

### Task 5: The Precision design system

**Files:**
- Create:
  - `web/src/ui/tokens.css`: the Global Constraints tokens, mapped into Tailwind v4 with `@theme`.
  - `web/src/ui/theme.ts`
  - `web/src/ui/primitives/*`: the shadcn components, themed. The set: button, input, textarea, select, dialog, sheet (phone bottom sheet), dropdown-menu, tabs, tooltip, popover, toggle-group, switch, scroll-area, command (cmdk), sonner-free toast (a small own `Toaster`), skeleton.
  - `web/src/ui/athena/*`: `StageBadge`, `Mono`, `Kbd`, `Card` (hairline), `EmptyState`, `ErrorState`, `RelativeTime`, `Truncate` (with a "show all" popover), `Confirm`.
  - `web/src/app/routes/design.tsx`: `/design`, a kitchen-sink page that every component renders on, in every state.
- Test: `web/src/ui/contrast.test.ts`, `web/src/ui/athena/*.test.tsx`

**Interfaces:**
- `theme.ts`:
  - `useTheme(): { theme: "system" | "light" | "dark"; resolved: "light" | "dark"; setTheme(t) }`
  - It writes `data-theme` on `<html>` and stores `athena.theme`.
  - A blocking theme-init script is not allowed under the CSP. `main.tsx` sets the theme before the first render, and `index.html` uses the `color-scheme` meta tag plus CSS `@media (prefers-color-scheme)` defaults, so there is no flash.
- `StageBadge({ stage, action, outcome })` renders the Global Constraints stage label. Orange (`--accent` fill) is used only for `person/*`, and `working` shows a ring indicator.
- `RelativeTime({ at })` refreshes once a minute through a single shared timer. That is a local tick, not polling.
- `Truncate({ text, lines })` clamps by line count and never scrolls horizontally.
- The fonts come from `@fontsource` imports in `main.tsx`: Plex Sans 400/500/600 and Plex Mono 400/500, latin and latin-ext only.

- [ ] **Step 1: Write failing tests:**
  - `contrast: every text pair ≥ 4.5 and every line/indicator pair ≥ 3, both themes`. It parses `tokens.css`, computes WCAG relative luminance, and checks the pairs: text/bg, muted/bg, muted/surface-2, accent-fg/accent, accent-ink/bg, danger/bg, line/bg and accent/bg.
  - `StageBadge labels every stage and action exactly`: a table taken from the Global Constraints.
  - `StageBadge uses accent only for person stages`
  - `Truncate never widens its parent` (jsdom layout is limited, so assert the `min-w-0` and `break-words` / `line-clamp` classes)
  - `Confirm requires the second click`
  - `theme persists and resolves system`
- [ ] **Step 2: Run** `npm --prefix web test` → FAIL.
- [ ] **Step 3: Implement.** Theme shadcn to the tokens: 1 px `--line` borders, `--radius`, no shadows except the focus ring (2 px `--accent` outline, offset 2 px) and no gradients. Build `/design` with both themes side by side.
- [ ] **Step 4: Run** `make web-check` → PASS.
- [ ] **Step 5: Commit** `feat(web): Precision tokens, themed components and the design page`.

### Task 6: Link client and state store

**Files:**
- Create: `web/src/lib/link/frame.ts`, `web/src/lib/link/client.ts`, `web/src/lib/store/state.ts`, `web/src/lib/store/selectors.ts`, `web/src/lib/link/fakeServer.ts` (test helper)
- Test: `web/src/lib/link/client.test.ts`, `web/src/lib/store/state.test.ts`, `web/src/lib/store/selectors.test.ts`

**Interfaces:**
- `frame.ts`:
  - `encodeFrame(f: Frame): Uint8Array`
  - `decodeFrame(b: Uint8Array): Frame`. The client tolerates unknown fields, for forward compatibility with a newer server; only the server is strict.
- `client.ts`: `class LinkClient`
  - `constructor(opts: { url: string; clientId: string; WebSocketImpl?: typeof WebSocket; now?: () => number })`
  - `start(): void` and `stop(): void`
  - `status: "connecting" | "open" | "offline" | "signedOut"`
  - `onStatus(cb)`, `onFrame(cb: (f: Frame) => void)`
  - `send(channel: Channel, durable: boolean, body: Frame["body"]): void`
  - `setResume(epoch: string, cursor: bigint): void`
  - `grantStream(items: number): void`
  - Behaviour:
    - Subprotocol `athena.v1`, binary frames.
    - Durable frames get `seq` per connection, starting at 1; every frame carries `ack` = the highest durable seq received.
    - A Ping is answered with a Pong carrying the same `at_unix_ms`. A standalone ack (`Ping{at_unix_ms:0}`) goes out within 1 s of receiving durable frames when nothing else carries the ack.
    - The connection counts as dead after 45 s without any frame. The client closes it and reconnects.
    - Close codes and backoff follow Global Constraints.
    - After every Welcome it re-sends `Credit{STREAM, 256 - inFlight}` and emits `onStatus("open")`.
- `state.ts`: `useAthena`, a Zustand store.
  - **State:** `epoch`, `cursor: bigint`, `tickets: Map<string, TicketSummary>`, `approvals`, `triggers`, `threads`, `decisions`, `node`, `health`, `policy`, `repos`, `notifications`, `prompts` and `details: Map<string, TicketDetail>`.
  - **Actions:**
    - `applySnapshot(s)`
    - `applyDelta(d): "ok" | "gap"`. A gap is `d.seq > cursor + 1` when `d.seq != 0`; the caller then sends `Resync`. Deltas with `seq` 0 (details) never move the cursor.
    - `reset()`
  - **Wiring:** `connectStore(client: LinkClient)` sends `Hello{protocol:1, client_id, epoch, cursor}` on open and routes Snapshot, Delta and Resync.
- `selectors.ts`:
  - `selectNeedsYou(s): NeedsYouItem[]`
    - A `NeedsYouItem` is `{ kind: "approval" | "answer" | "review" | "delivery" | "blocked" | "failed"; ticket; approval? }`.
    - Order: approvals first, then answers, then the rest; oldest first within each kind.
  - `selectTickets(s, f: TicketFilter)`, where `TicketFilter{ stage?: "open" | "needsYou" | "working" | "idle" | "ended"; repo?: string; model?: string; q?: string }`
  - `selectCounts(s): { needsYou, open, working, endedToday, mergedToday }`. Each count equals the length of the matching selector's list.

- [ ] **Step 1: Write failing tests** (with `fakeServer.ts`, an in-memory WebSocket double that speaks frames):
  - `hello carries epoch and cursor after a snapshot`
  - `answers ping with pong of the same time`
  - `acks durable frames cumulatively`
  - `reconnects with backoff and jitter, resets after 10 s open`
  - `reconnects at once on online and visible`
  - `close 4002 sets signedOut and stops reconnecting`
  - `1013 reconnects with the cursor`
  - `re-grants stream credit after reconnect`
  - `delta gap triggers resync`
  - `seq 0 detail does not move cursor`
  - `500 tickets apply under budget`: 500 deltas in under 16 ms mean (`performance.now`, with a generous CI factor ×4).
  - `needs-you order and counts match lists`
- [ ] **Step 2: Run** `npm --prefix web test` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` → PASS.
- [ ] **Step 5: Commit** `feat(web): link client and delta-fed state store`.

### Task 7: Commands, the outbox and optimistic updates

**Files:**
- Create: `web/src/lib/commands/outbox.ts`, `web/src/lib/commands/actions.ts`, `web/src/lib/commands/ids.ts`
- Test: `web/src/lib/commands/outbox.test.ts`, `web/src/lib/commands/actions.test.ts`

**Interfaces:**
- `ids.ts`: `newCommandId(): string`, a 26-character ULID. Monotonic per tab, 1–128 bytes as the server requires.
- `outbox.ts`: `class Outbox`
  - `constructor(db: IDBFactory, send: (cmd: Command) => void)`
  - `submit(action: Command["action"], opts?: { optimistic?: (s) => void; rollback?: (s) => void }): Promise<CommandResult>`
  - `onOpen(): void` resends every pending command in id order, with the same id.
  - `onResult(r: CommandResult): void`
  - `pending(): Command[]`
  - `clear(): void`, used only on explicit sign-out.
  - **Persistence:** IndexedDB `athena`, store `outbox`, keyed by id, holding `{ id, bytes, createdAt }`.
  - A command is removed only when its result arrives. The result resolves the promise exactly once, even when the same result arrives twice (a resend answered from the server's stored result).
  - **Codes:**
    - `reauth_required` keeps the command and calls the registered `onReauth()` handler (Task 8); after a successful step-up the command is resent.
    - `unauthenticated` keeps it and signs out.
    - Any other failure removes it, runs `rollback`, and resolves `{ ok: false }`.
  - **Expiry:** pending commands older than 23 h are dropped with a toast, since the server keeps results 24 h.
- `actions.ts`: typed helpers that return `Promise<CommandResult>`:
  - `submitTask({ text, repo?, dir?, model? })`
  - `reply(ticketId, text)`, `stop(ticketId, reason)`, `retry(ticketId)`, `interrupt(ticketId)`
  - `answerApproval(id, approve, note?)`
  - `reevaluate(triggerId)`, `dismiss(decisionId)`, `unsubscribe(thread)`
  - `setPolicy(base, json)`
  - `watch(id)` / `unwatch(id)`: also tracked so they are re-sent on reconnect.
  - `pushSubscribe(sub)`, `pushUnsubscribe(endpoint)`, `setMuted(kinds)`
  - `visibility(visible)`
  - `listPasskeys()`, `revokePasskey(id)`
  - `getPrompt(part)`, `setPrompt(part, base, text)`, `resetPrompt(part, base)`
  - **Optimistic updates:** `answerApproval` marks the approval answered and clears `pending_approval_id`. `stop` shows the Ticket as "Stopping…". The Deltas that follow are the truth.
  - **Error copy:** `errorMessage(code)` maps each stable code to one plain sentence. For example, `conflict` reads "Changed on another device. Reload to see the latest."

- [ ] **Step 1: Write failing tests** (`fake-indexeddb`):
  - `outbox resends by id after reconnect and resolves once`: submit, drop the socket before the result, reconnect; exactly one send per connection carries the same id, and the promise resolves once.
  - `outbox survives a page reload`: a new Outbox on the same DB resends the command.
  - `reauth_required keeps and resends after step-up`
  - `failure rolls back the optimistic change`
  - `commands older than 23 h are dropped with a notice`
  - `watch is re-sent after reconnect`
  - `ids are unique and ordered`
- [ ] **Step 2: Run** `npm --prefix web test` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` → PASS.
- [ ] **Step 5: Commit** `feat(web): IndexedDB outbox with exactly-once resend and optimistic updates`.

### Task 8: Sign-in, enrollment and step-up

**Files:**
- Create:
  - `web/src/lib/auth/webauthn.ts`: base64url and JSON conversion, hand-rolled with no library.
  - `web/src/lib/auth/session.ts`
  - `web/src/features/auth/SignIn.tsx`, `Enroll.tsx`, `ReauthDialog.tsx`
- Test: `web/src/lib/auth/*.test.ts`, `web/src/features/auth/*.test.tsx`

**Interfaces:**
- `session.ts`:
  - `getSession(): Promise<{ signedIn: boolean; reauthAt?: Date }>` calls `GET /auth/session`.
  - `signIn(): Promise<void>` runs `/auth/login/begin|finish`, with `mediation: "conditional"` when available and a button as the fallback.
  - `enroll(token: string, deviceName: string): Promise<void>`
  - `reauth(): Promise<void>` runs `/auth/reauth/begin|finish`.
  - `signOut(): Promise<void>` calls `POST /auth/logout`, then `outbox.clear()` and `store.reset()`.
  - Every POST sends JSON with `credentials: "same-origin"`; the browser sets `Origin`.
- `Enroll.tsx` (route `/enroll`):
  - It reads `location.hash`, then immediately calls `history.replaceState` to drop the fragment.
  - It asks for a device name (default from the user agent: "iPhone", "Mac" and so on), creates the passkey, then signs in.
  - A spent or expired token shows "This link was already used or has expired. Run `athena web enroll` for a new one." There is no retry with the same token.
- `ReauthDialog` is registered as `outbox.onReauth`. It explains in one sentence why the step-up is needed, runs `reauth()`, and on success resends.
- App gate: no session shows `SignIn`. `LinkClient` status `signedOut` (close 4002) shows `SignIn` with "You were signed out on this device." and keeps the outbox.

- [ ] **Step 1: Write failing tests** (mock `navigator.credentials` and `fetch`):
  - `webauthn converts creation and request options both ways`, using a fixture JSON from plan 3's `/auth/register/begin`.
  - `enroll drops the fragment before any request`
  - `spent token shows the exact copy`
  - `close 4002 signs out and keeps the outbox`
  - `reauth dialog resends the held command`
  - `sign out clears outbox and store`
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` → PASS.
- [ ] **Step 5: Commit** `feat(web): passkey sign-in, enrollment and step-up`.

### Task 9: App shell, navigation, palette and the e2e harness

**Files:**
- Create:
  - `web/src/app/routes.tsx`
  - `web/src/app/PhoneShell.tsx`, `DesktopShell.tsx`, `Sidebar.tsx`, `ConnectionBanner.tsx`
  - `web/src/app/shortcuts.ts`, `web/src/app/Palette.tsx`
  - `web/e2e/harness.ts`, `web/e2e/global-setup.ts`, `web/e2e/shell.spec.ts`, `web/playwright.config.ts`
- Modify: `Makefile` (`e2e`)
- Test: `web/src/app/*.test.tsx`, `web/e2e/shell.spec.ts`

**Interfaces:**
- Routes:

  | Path | View | Built in |
  | --- | --- | --- |
  | `/` | Needs you | Task 10 |
  | `/tickets` | Ticket list | Task 11 |
  | `/tickets/:id` | Ticket detail | Task 11 |
  | `/new` | New task | Task 13 |
  | `/graph` | Graph (lazy) | Task 15 |
  | `/routing` | Routing records, with tabs Triggers / Threads / Decisions | Task 14 |
  | `/nodes` | Nodes | Task 17 |
  | `/settings/:group?` | Settings | Task 17 |
  | `/enroll` | Enroll | Task 8 |
  | `/design` | Design page, dev builds only | Task 5 |

  Unknown paths show a designed 404.
- Shells:
  - **`PhoneShell`** (under 1024 px): a top bar with the title and the connection dot; content; a bottom navigation with 5 items (Needs you with its count, Tickets, New, Graph, Settings). Routing and Nodes are reached from Settings › System and from the palette.
  - **`DesktopShell`** (1024 px and up):
    - `Sidebar`, 240 px: the navigation, the node card (`slots_in_use/slots`, queued, live Ticket count) and Today (`selectCounts`: merged today, open).
    - The list pane, 400 px, and the detail pane, taking the rest.
    - Routes with no list (New, Settings, Graph) span both panes.
  - **`ConnectionBanner`**:
    - "Connecting…" appears only after 1 s.
    - "Control server unreachable — retrying" appears when offline, with the time of the last update.
    - "Signed out" is handled by the auth gate (Task 8).
    - It is shown only when the link is not open.
- `shortcuts.ts`:
  - `useShortcut(keys: string, handler, opts?: { when?: () => boolean })`. It supports chords like `g n` within 1 s, and ignores events whose target is an input, a textarea or contenteditable, except `Esc` and `⌘⏎`.
  - `?` opens the shortcut list.
- `Palette`, opened with `⌘K`:
  - Go to any route.
  - Search Tickets by id or title, Triggers by text, and Threads by key (for example `#12` or a GitHub URL).
  - Actions on the selected Ticket: Reply, Stop, Retry, Interrupt and Watch session.
  - New task.
  - Toggle the theme.
- E2E harness:
  - `global-setup.ts` builds `bin/athena` and `fakeclaude` (`go build ./internal/runtime/claude/testdata/fakeclaude`).
  - It writes a temp state dir whose `config.json` points the runtime at fakeclaude, sets `web.enabled`, listen `127.0.0.1:<free port>`, `tls: off` and host `localhost`, and has no GitHub source.
  - It starts `athena server`, runs `athena web enroll`, and parses the link with `/http:\/\/localhost:\d+\/enroll#\S+/`.
  - `harness.ts`:
    - `signedInPage(page)` adds a CDP virtual authenticator (`ctap2`, `internal`, resident key, UV) and runs enroll and sign-in.
    - `athena(args)` runs the CLI against the temp state dir, for example `athena task`.
    - Fixtures come through `ATHENA_FAKE_FIXTURE`, using the fakeclaude env vars.
  - Projects: `phone` (390×844, `isMobile`, touch), `tablet` (820×1180) and `desktop` (1440×900), all Chromium.
  - Every spec runs axe on each screen it visits and fails on any serious or critical violation.
  - Screenshots use `toHaveScreenshot` with `maxDiffPixelRatio: 0.01`, a fixed clock (`page.clock.setFixedTime`), and `animations: "disabled"`. Baselines are committed under `web/e2e/__screenshots__/`.
  - `make e2e` runs `npm --prefix web run e2e`. It is not part of `make check` (it is slow), but every UI task from here on runs it.

- [ ] **Step 1: Write failing tests:**
  - `phone shows bottom nav with needs-you count`
  - `desktop shows three panes at 1024`
  - `shortcuts ignore typing in inputs`
  - `g n chord navigates`
  - `palette finds a ticket by id`
  - `banner appears only after 1 s offline`
  - e2e `shell.spec.ts`: sign in through the virtual authenticator; the shell renders on all three projects; light and dark screenshots; axe passes.
- [ ] **Step 2: Run** `npm --prefix web test` and `make e2e` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` and `make e2e` → PASS. Run `make check` too.
- [ ] **Step 5: Commit** `feat(web): app shell, keyboard, palette and Playwright harness`.

### Task 10: Needs you and Approvals

**Files:**
- Create: `web/src/features/needs-you/NeedsYou.tsx`, `ApprovalCard.tsx`, `AnswerCard.tsx`, `ReviewCard.tsx`, `StalledCard.tsx`
- Test: `web/src/features/needs-you/*.test.tsx`, `web/e2e/needs-you.spec.ts`

**Interfaces:**
- Uses `selectNeedsYou` and `actions`.
- Every card shows the Ticket title, `StageBadge`, repo or dir (Mono), model and age.
  - On the phone, its one primary action is at thumb height.
  - On the desktop, `a`/`d`/`r` act on the focused card and `j`/`k` move between cards.
- The cards:
  - **Approval:** tool, summary, rule, the redacted input (Mono, `Truncate` at 6 lines), a note field (optional) and Deny / Approve. After a tap the card shows "Approved" or "Denied" optimistically and leaves when the Delta lands.
  - **Answer:** the `attention_question` and an inline composer (Enter sends, Shift+Enter adds a newline) that calls `reply`.
  - **Review:** "Ready for review", the PR link (`pr_url`, opens in a new tab with `rel=noopener`) or the local diff (`/tickets/:id#diff`), and Stop.
  - **Delivery / blocked / failed:** the reason, Retry when `can_retry`, and Stop.
- The empty state is designed: "Nothing needs you." with today's counts.
- The header line is "`N` need you · `node` · `slots_in_use` of `slots` running".

- [ ] **Step 1: Write failing tests:**
  - `approval approve calls answerApproval once and shows Approved`
  - `deny with note sends the note`
  - `answer composer sends on Enter, newline on Shift+Enter`
  - `retry only when can_retry`
  - `stop asks for confirmation`
  - `keyboard a approves the focused card`
  - `empty state copy`
  - e2e: a fake fixture raises an Approval; approve it on the phone project; the Ticket moves on; screenshots in both themes; axe passes.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` and `make e2e` → PASS.
- [ ] **Step 5: Commit** `feat(web): Needs you with one-tap approvals, answers and retries`.

### Task 11: Tickets and the Ticket detail

**Files:**
- Create:
  - `web/src/features/tickets/TicketList.tsx`, `Filters.tsx`
  - `web/src/features/tickets/TicketDetail.tsx`, `Timeline.tsx`, `Facts.tsx`, `DiffView.tsx` (lazy), `FilesView.tsx`
- Test: `web/src/features/tickets/*.test.tsx`, `web/e2e/tickets.spec.ts`

**Interfaces:**
- **`TicketList`:**
  - Filters: stage (Open · Needs you · Working · Idle · Ended), repo (from `repos`), model, and a text query. They are kept in the URL search params.
  - Rows show the title (one line, ellipsis), `StageBadge`, repo, model, age, and an orange dot for needs-you.
  - Up to 200 rows, then "Show more".
  - `j`/`k`/`⏎` on the desktop.
- **`TicketDetail`:**
  - On mount it sends `watch(id)` and on unmount `unwatch(id)`.
  - Header: the title (wraps), the id (Mono), `StageBadge`, and the actions Reply, Stop, Retry and Interrupt (only while working).
  - `Facts`: repo or dir, branch, model, runtime, created, updated, ended and outcome, the PR link, and Threads.
  - The pending question or Approval is inline at the top, reusing Task 10's cards.
  - `Timeline` shows its entries newest last, each with a kind icon and the time; `timeline_trimmed` shows "Older entries are hidden".
  - Tabs: Session (Task 12), Timeline, and Changes or Files:
    - **Changes:** `DiffView`, shown when `branch` is set. It fetches `/api/tickets/{id}/diff` and shows a numstat file list plus a unified diff with line wrap and no horizontal page scroll; code blocks scroll inside themselves.
    - **Files:** `FilesView`, shown when there is no repo. It lists files with download links.
  - A loading skeleton and a `not_found` state are designed.

- [ ] **Step 1: Write failing tests:**
  - `filters live in the URL and select the right rows`
  - `long and ANSI content stays inside the column`: a 2,000-character title, RTL text and an ANSI-laden timeline entry give no `scrollWidth > clientWidth` on the column container. Assert it through the classes in jsdom and the real layout in e2e.
  - `detail watches and unwatches`
  - `trimmed timeline notice`
  - `diff view renders numstat and wraps lines`
  - `truncated diff shows the notice`
  - `files view links downloads`
  - `interrupt only while working`
  - e2e: create a Ticket with `athena task` on a temp git repo; the detail shows Changes with the fake's edit; desktop three-pane screenshots light and dark; no horizontal scroll at 320 px.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` and `make e2e` → PASS.
- [ ] **Step 5: Commit** `feat(web): ticket list with filters and the ticket detail`.

### Task 12: The live session view

**Files:**
- Create: `web/src/lib/stream/useSessionStream.ts`, `web/src/lib/stream/ansi.ts`, `web/src/features/session/SessionView.tsx`, `ToolCall.tsx`, `Composer.tsx`, `PinnedSessions.tsx`
- Test: `web/src/lib/stream/*.test.ts`, `web/src/features/session/*.test.tsx`, `web/e2e/session.spec.ts`

**Interfaces:**
- `useSessionStream(ticketId)` returns `{ items: StreamItem[]; gap?: { from: bigint; to: bigint }; live: boolean }`.
  - It sends `StreamOpen{ticket_id, from_offset: -1}` on mount and `StreamClose` on unmount.
  - After a reconnect it re-opens from the last offset + 1.
  - **Dedupe:** items are keyed by `offset:kind:tool_use_id:text-hash`, because plan 3 can resend the items of a partly sent line.
  - It keeps the newest 2000 items.
  - **Credit:** it grants `Credit{STREAM}` in batches of 64 items as they render. This is the per-connection budget shared through `LinkClient.grantStream`.
- `ansi.ts`: `stripAnsi(s: string): string`. It removes CSI/OSC escapes and keeps the text. No colours are rendered in v1.
- `SessionView`:
  - Mono 13 px.
  - `say` items as text; `thinking` collapsed under "Thinking"; `init` as one muted line.
  - `tool_call` and `tool_result` paired by `tool_use_id` in one collapsible `ToolCall` row: the tool, the first line of the input, and status ok, error or running, with its duration.
  - `result` as a closing line.
  - A gap shows "Some output was skipped" with the item counts.
  - **Auto-scroll:** it follows the tail while the owner is within 48 px of the bottom. It stops when they scroll up, and a "Jump to latest" button appears.
  - The `● live` dot shows while the link is open and the Ticket is working.
- `Composer`:
  - Enter sends `reply`; `⌘⏎` (`Ctrl+Enter`) sends `interrupt`.
  - Disabled with a one-line reason when the Ticket has ended.
  - Text stays in the field until the result is ok.
- `PinnedSessions`:
  - Desktop at 1440 px and wider only.
  - Pin up to 3 sessions from the detail header. They show side by side under the list, and the choice is kept in the URL (`?pin=T-1,T-2`).

- [ ] **Step 1: Write failing tests:**
  - `stream dedupes resent items`
  - `stream keeps 2000 items`
  - `re-opens from last offset after reconnect`
  - `grants credit in batches of 64`
  - `tool call and result pair by id`
  - `stripAnsi removes CSI and OSC`
  - `auto-scroll yields when scrolled up and offers jump`
  - `composer enter replies, cmd-enter interrupts`
  - `composer disabled after end with reason`
  - e2e: watch a fakeclaude session replaying a fixture with tool calls; items appear in order; reply reaches fakeclaude's stdin log; screenshots.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` and `make e2e` → PASS.
- [ ] **Step 5: Commit** `feat(web): live session view with composer and pinned sessions`.

### Task 13: New task

**Files:**
- Create: `web/src/features/new-task/NewTask.tsx`, `RepoPicker.tsx`
- Test: `web/src/features/new-task/*.test.tsx`, `web/e2e/new-task.spec.ts`

**Interfaces:**
- The text field is required: it autofocuses on the desktop, grows, and stays readable at 15 px on the phone.
- `RepoPicker` is a combobox over `repos`, grouped by kind (GitHub · Local repositories · Folders). It also takes:
  - free entry of `owner/name`, sent as `repo`;
  - an absolute path, sent as `dir`;
  - "No repository", the default, sent as neither.

  It validates `owner/name` with `^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$` and paths with a leading `/`.
- Model is optional free text, with suggestions from the distinct `model` of recent Tickets and the default. Runtime and node pickers appear only when more than one choice exists, which happens in plans 5 and 6.
- Submit runs `submitTask`. On ok it navigates to `/tickets/<ticket_id>`; on failure the error stays inline and the text is kept.
- `⌘⏎` submits.

- [ ] **Step 1: Write failing tests:**
  - `repo picker sends repo for owner/name and dir for a path`
  - `no repository sends neither`
  - `invalid owner/name is refused inline`
  - `success navigates to the new ticket`
  - `failure keeps the text`
  - e2e: start a no-repo task on the phone and land on its detail; screenshots.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` and `make e2e` → PASS.
- [ ] **Step 5: Commit** `feat(web): new task with repository picker`.

### Task 14: Routing records and search

**Files:**
- Create: `web/src/features/routing/Routing.tsx`, `Triggers.tsx`, `Threads.tsx`, `Decisions.tsx`, `web/src/app/search.ts`
- Test: `web/src/features/routing/*.test.tsx`, `web/src/app/search.test.ts`

**Interfaces:**
- **Triggers:**
  - Newest first: time, source, actor, kind, a "to agent" mark when addressed, the Thread, the one-line text, and the `outcome` in words.
  - Outcome wording: `filtered:<code>` reads "Filtered: <code in words>"; `reached` / `attached` link to their Tickets; `requeued` reads as such.
  - **Evaluate again** (`reevaluate`) is offered only on the outcomes the reference allows: filtered, unavailable Eval, or delivered with no effective judgement.
- **Threads:**
  - The key; queued count; an "Evaluating" badge with strength while `eval` is set; subscribed Tickets with their flags (own, bound, context) as chips; the Decision count; and closed.
  - **Unsubscribe** (`unsubscribe`) asks for confirmation.
- **Decisions:**
  - Time, kind, Thread, Ticket link, triggers, reasons rendered as `code` with values (Mono), a "would accept" badge, and a dismissed state.
  - **Dismiss** (`dismiss`) is offered for undismissed would-accept Decisions.
- Filters per tab, kept in the URL. Counts in the tab labels equal the list lengths.
- `search.ts`: `search(state, q): SearchHit[]` over Tickets (id, title), Triggers (text) and Threads (key, `#n` and GitHub URL forms). The palette (Task 9) and `/` use it.

- [ ] **Step 1: Write failing tests:**
  - `evaluate again only on allowed outcomes`
  - `unsubscribe confirms`
  - `dismiss only undismissed would-accept`
  - `search finds a thread by #12 and by URL`
  - `tab counts equal list lengths`
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` → PASS. Run `make e2e` for a routing smoke spec (an empty state on a server without GitHub, plus screenshots).
- [ ] **Step 5: Commit** `feat(web): routing records and search`.

### Task 15: The task graph

**Files:**
- Create: `web/src/features/graph/Graph.tsx` (lazy route), `layout.ts`, `usePanZoom.ts`, `GraphList.tsx`
- Test: `web/src/features/graph/layout.test.ts`, `web/src/features/graph/Graph.test.tsx`, `web/e2e/graph.spec.ts`

**Interfaces:**
- `layout(tickets, threads, decisions, opts: { groupBy: "repo" | "machine"; at?: Date }): GraphModel`
  - `GraphModel` is `{ lanes: Lane[]; nodes: GNode[]; edges: GEdge[] }`.
  - **Lanes:** one per repository, labelled with the host and name (for example `GITHUB · OWNER/NAME`), plus "No repository". `machine` mode makes one lane per node; today that is one lane.
  - **Nodes:** x is the creation time (a log-compressed scale over the visible window); y stacks inside the lane without overlap.
  - **Edges:** two kinds exist today.
    - `thread`: Tickets subscribed to the same Thread, drawn dashed.
    - `follow-up`: a Decision that routed a Trigger to an existing Ticket, drawn from the Trigger's earlier Ticket on that Thread.
  - `hand_off`, `wait_for`, `ask_agent` and same-file overlap warnings arrive with plan 7. `GEdge.kind` already has these kinds so the legend is stable.
  - `at` filters the model to what existed at that time, which drives the replay.
- Rendering:
  - SVG with no graph library: stage shape and colour, orange for needs-you, a ring for running.
  - Hover or focus shows the title and id. Click opens the Ticket (the detail pane on the desktop, a navigation on the phone).
  - The legend matches the mockup.
- `usePanZoom`: pointer drag, wheel or pinch to zoom from 0.25× to 4×, keyboard `+`/`-`/`0`, and `prefers-reduced-motion` respected.
- Toolbar: By repository / By machine; Needs you only; and a time scrubber with ▶ replay (10 s for the whole window).
- `GraphList` is the accessible list fallback and the default on the phone when the page is read by a screen reader. A toggle shows it on any size.

- [ ] **Step 1: Write failing tests:**
  - `lanes per repo plus no-repo`
  - `no overlapping nodes in a lane`: 50 Tickets in one lane.
  - `thread edges join co-subscribed tickets`
  - `at filters to the past`
  - `needs-you filter`
  - `list fallback has every node`
  - e2e: 6 Tickets across 2 repositories and none; screenshots on phone and desktop; axe passes.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** It must be a lazy chunk; the bundle budget must still pass.
- [ ] **Step 4: Run** `make web-check` and `make e2e` → PASS.
- [ ] **Step 5: Commit** `feat(web): task graph with lanes, replay and list fallback`.

### Task 16: PWA: manifest, service worker and Web Push

**Files:**
- Create:
  - `web/src/sw.ts`
  - `web/public/manifest.webmanifest`
  - `web/public/icons/` (192, 512, maskable 512, and an `apple-touch-icon` at 180): an orange square with a black "A" in IBM Plex Sans 600, generated once and committed as PNG
  - `web/src/lib/push.ts`
  - `web/src/lib/visibility.ts`
- Modify: `web/vite.config.ts` (a second entry `sw` emitted as `/sw.js`, plus a ~30-line plugin that writes the precache list into it), `web/index.html` (the manifest link, `theme-color` for both schemes, apple meta tags)
- Test: `web/src/sw.test.ts`, `web/src/lib/push.test.ts`, `web/src/lib/visibility.test.ts`, `web/e2e/pwa.spec.ts`

**Interfaces:**
- Manifest:
  - `name: "Athena"`, `short_name: "Athena"`
  - `display: "standalone"`, `start_url: "/"`, `scope: "/"`
  - `background_color: "#FFFFFF"` and `theme_color: "#FFFFFF"`. The dark `theme-color` is set through a media-query meta tag.
- `sw.ts`:
  - **install:** precache `/`, `index.html` and the hashed assets of the current build.
  - **activate:** delete old caches and `clients.claim()`.
  - **fetch:**
    - Navigations go network-first with a 3 s timeout, then the cached `index.html` (the offline shell).
    - `/assets/*` is served cache-first.
    - `/ws`, `/auth/*`, `/api/*` and `/push/key` are never cached.
  - **push:** the payload is plan 3's notification JSON. It shows `showNotification(title, { body, tag: ticketId, data: { url: "/tickets/<id>" } })`, and the same tag replaces the earlier one.
  - **notificationclick:** focus an existing client and navigate it, or open a window at the URL.
  - **Updates:** a new SW waits. The app shows "A new version is ready — Reload", and a tap sends `SKIP_WAITING`.
- `push.ts`:
  - `pushState(): "unsupported" | "denied" | "off" | "on"`
  - `enablePush()`: request permission, `GET /push/key`, `pushManager.subscribe({ userVisibleOnly: true, applicationServerKey })`, then `actions.pushSubscribe`.
  - `disablePush()`
  - After each sign-in the app re-subscribes when permission is granted, because plan 3 ties subscriptions to the session.
- `visibility.ts`: sends `Visibility{visible}` on `visibilitychange`, on focus and blur, and after every reconnect. Plan 3 suppresses push while a client reports visible.

- [ ] **Step 1: Write failing tests:**
  - `sw push shows title and tag and opens the ticket on click` (SW unit tests with a mocked `self`)
  - `sw navigation falls back to the cached shell offline`
  - `sw never caches ws auth api`
  - `enablePush subscribes and sends PushSubscribe`
  - `re-subscribes after sign-in`
  - `visibility sent on change and after reconnect`
  - e2e: the manifest is valid and installable (Chromium's `installability` check through CDP `Page.getInstallabilityErrors` is empty); offline reload shows the shell with the "unreachable" banner; a push simulated through CDP `ServiceWorker.deliverPushMessage` shows a notification; screenshots of the offline state.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` and `make e2e` → PASS.
- [ ] **Step 5: Commit** `feat(web): installable PWA, offline shell and web push`.

### Task 17: Nodes and Settings

**Files:**
- Create:
  - `web/src/features/nodes/Nodes.tsx`
  - `web/src/features/settings/Settings.tsx`, `PolicyForm.tsx`, `AdmissionTable.tsx`, `Notifications.tsx`, `Devices.tsx`, `Prompts.tsx` (lazy), `Execution.tsx`, `System.tsx`, `SaveBar.tsx`
- Test: `web/src/features/{nodes,settings}/*.test.tsx`, `web/e2e/settings.spec.ts`

**Interfaces:**
- **`Nodes`:** the node card: name, slots in use of slots (a bar), queued, live Tickets (links), and the Claude version. The `Health` items come below. Healthy items collapse into one "All checks passing" line; failing ones show their message and since.
  - Plan 6 adds draining, enrolling a node and more nodes; the empty space says nothing about them.
- **Settings groups:**
  - **General:** Policy mode (`off` · `observe` · `auto`, with one sentence each) and Rules (name, instructions, enabled; add, remove, reorder).
  - **Sources › GitHub:** allowed list, bots, and the admission table. The table's rows are issues, pulls, comments and CI. Its columns are listed unaddressed, unlisted addressed and unlisted unaddressed. Each cell offers the choices the backend allows: explicit, inferred or ignore, and CI only `unlistedUnaddressed`.
    - This edits `PolicyView.settings_json` and saves with `setPolicy(revision, json)`.
    - Unknown JSON fields are preserved: parse, edit the known fields, and keep the rest.
  - **Notifications** (uses Task 16's `pushState`/`enablePush`/`disablePush`): an enable or disable switch with the push state (unsupported, denied, off, on); a mute switch per kind (`notifications.kinds`) using `setMuted`; and the iOS note "Add Athena to your Home Screen to get notifications on iPhone." when on iOS and not standalone.
  - **Devices:**
    - `listPasskeys` shows name, created and last used. **Revoke** confirms; revoking the current device warns that this device will be signed out.
    - **Add a device** calls `POST /auth/enroll-link`, with step-up on `reauth_required`. It shows the QR code (`uqr` SVG) and the link with a Copy button, plus the expiry countdown. The token is never put in the page URL or logged to the console.
  - **Prompts:**
    - The parts list with an "Edited" mark and a "Default updated" badge.
    - The editor (a Mono textarea): Save runs `setPrompt`, and Reset (confirmed) runs `resetPrompt`.
    - "Full prompt" shows the assembled worker and router prompts.
    - History lists the revisions.
    - "Default updated" shows the old and new defaults compared above the owner's text, with Keep my text / Use the new default.
    - One sentence notes that edits apply to new sessions.
  - **Execution** (read-only): slots, the Claude version and the default model from `node`. "Edit in config.json" explains where.
  - **System:** links to Routing and Nodes; the server version (from `Welcome.server_version`); and Appearance (theme).
- **`SaveBar`** (reference panel principle 7): one per settings block, visible only while there are changes, a save is running or a save failed.
  - Every Policy and prompt save handles `conflict` the same way: keep the edits, show "Changed on another device", and offer **Reload** (discard) or **Overwrite** (resend with the fresh revision).

- [ ] **Step 1: Write failing tests:**
  - `policy conflict keeps edits and offers reload`
  - `unknown policy fields survive a save`
  - `admission table offers only allowed choices per row`
  - `save bar only while dirty, saving or failed`
  - `revoke current device warns`
  - `add device shows QR and never puts the token in location or console`
  - `prompts default-updated keep and use-new`
  - `muting a kind calls setMuted with the full list`
  - e2e: change the Policy mode on desktop and see it reflected on a second page through Deltas (two contexts); add a device and enroll it in a second context through the QR link; screenshots.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make web-check` and `make e2e` → PASS.
- [ ] **Step 5: Commit** `feat(web): nodes, settings, devices, notifications and prompts`.

### Task 18: Full-flow check, polish pass and the owner's phone check

**Files:**
- Create: `web/e2e/full-flow.spec.ts`
- Modify:
  - screenshot baselines as needed
  - `docs/dev/dashboard.md`: replace the placeholder wording, describe the dashboard's features, and document the phone check
  - `bench/` only if a budget needs the real UI
- Test: `web/e2e/full-flow.spec.ts`

- [ ] **Step 1: Write `full-flow.spec.ts`.** It runs on the phone and desktop projects:
  1. sign in
  2. New task, no repository
  3. watch it live
  4. answer its question (fixture)
  5. approve its Approval (fixture)
  6. see it Ready for review
  7. stop a second Ticket, with confirmation
  8. open the graph
  9. sign out

  In every state: light and dark screenshots, axe clean, no horizontal scroll at 320 px, and no console errors. Fail on any `console.error` and any CSP violation report.
- [ ] **Step 2: Polish pass.** Go through every screenshot against the mockups and the §10.1 list, checking:
  - one primary action per view
  - designed empty, loading and error states with skeletons, not spinners
  - 44 px targets
  - safe-area insets
  - focus rings
  - reduced motion

  Fix what differs. List any intentional difference from the mockups in the report.
- [ ] **Step 3: Run** `make check`, `make e2e` and `make bench` → PASS. `make bench`'s existing budgets must still pass with the real UI embedded; the RSS is the one to watch.
- [ ] **Step 4: Owner phone check** (the owner runs it; it carries plan 3's R30/R32):
  1. `make build`, then `athena web setup --host <vpn-name> --ip <vpn-ip> --listen <vpn-ip>:8443`, then restart the server.
  2. On the iPhone, install the CA profile and turn on full trust (`docs/dev/dashboard.md` 3a).
  3. Run `athena web enroll`, scan the QR code, and create the passkey with Face ID.
  4. Add Athena to the Home Screen and open it from there. Settings › Notifications › Enable.
  5. Start a task from the phone, watch it live, reply, then approve an Approval from a lock-screen push.
  6. On the Mac, open the same dashboard: actions on one screen show on the other in under a second.
  7. Turn on Airplane mode mid-approve, turn it off again: the approval happens exactly once.

  If the owner hasn't run it, record "not run" with the reason in the report and in the ledger.
- [ ] **Step 5: Commit** `test(web): full-flow e2e and polish pass`.

---

## Deferred to later plans (decided here)

- **Plan 5:** the runtime picker (Codex, `auto`), runtime badges with real values, and the Codex session item kinds.
- **Plan 6:**
  - offline-node Tickets in Needs you ("retry elsewhere")
  - more nodes, the machine lanes with real data, node enrollment and draining in Nodes, and Retry elsewhere
  - diffs and files for Tickets on remote nodes (the endpoints call the owning node over the link)
- **Plan 7:** agent messages in the timeline, and `hand_off`, `wait_for` and `ask_agent` graph edges, plus same-file overlap warnings, computed from each branch's changed paths with Task 3's diff code.
- **Plan 8:** the "Local · <model> on <node>" choice in New task, and GPU load and loaded models on the node card.
- **Plan 9:** the System and Performance pages, the audit log, budgets and alerts, and spend in the sidebar's Today.
- **Plan 10:** the kill switch control (it subsumes "Pause processing", spec §18.2), the WIP brake indicators, trust classes in Sources and shadow mode.
- **Plan 11:** the schedule Source editor in Settings.
- **Still deferred from plan 3**, not UI work:
  - the push edge cases
  - the login ceremony flood
  - error codes taken from message text
  - global visibility suppression, which stays by design
- **Editable Execution settings** (slots, default model, directory root, blocked commands): they live in `config.json` until a settings block exists for them, so Settings shows them read-only. Revisit with plan 6, where they become per node.
