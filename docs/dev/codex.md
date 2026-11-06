# Codex

Athena can run its workers on OpenAI's Codex CLI as well as on Claude Code: a Ticket runs on `claude`, `codex` or
`auto`, moves to the other runtime when one reaches its usage limit, and can have each ready report reviewed by a
read-only session on the other runtime. This page is the setup guide and the reference for how Athena controls Codex.

Quick start, once Codex is installed: `athena codex login`, then `athena codex status` (it must say ready), then
`athena task --runtime auto --review "..."` (or `--runtime codex`). The node card of the dashboard shows both runtimes
and their quota.

## 1. What Athena runs

Athena drives `codex app-server --listen stdio:// --strict-config`, one process per session, never the shared daemon,
`remote-control` or `--remote`. It runs Codex 0.160.0 or newer; 0.160.1 is the version it was verified against. A newer
version runs too, once its hooks check passes, and `athena codex status` calls it untested.

Each session's process runs in the Ticket's worktree with `CODEX_HOME` set to Athena's home (§2) and only two `-c`
overrides, for Athena's MCP server: its arguments (`mcp --ticket <id>`, plus `--role review` for a reviewer) and its
environment (the node's socket and the Assignment). No override ever sets `hooks.*` or `features.*`. On that process,
before any thread starts, Athena checks the session is under its control (§4); then it starts the thread
(`thread/start`, with the worker's instructions, approvals asked of Athena and the `workspace-write` sandbox, or
`read-only` for a reviewer) or resumes it (`thread/resume`, which keeps the thread's instructions), and reads the account's
usage limits once (free). A message sent while a turn runs is steered into it (`turn/steer`). Codex's approvals
(commands, network access, file changes, more permissions) go to Athena's Approvals, showing everything accepting would
grant (a permissions request's write paths, the paths it would make readable, and network access as host `*`). What
Athena cannot show in full is declined without asking, and the transcript notes why: input typed into a running
terminal (Codex's `writeStdin` approval, whose input the request does not carry), a file change whose paths Athena did
not see, and a permissions request holding anything but plain paths and network access (a special path such as the
filesystem root, or a field Athena does not know). Every other request Codex makes of
Athena (a question for the user, an MCP elicitation, a client-side tool call, a token refresh, an attestation, anything
newer) is answered at once, declined or refused, so a turn never waits on one. The session's transcript keeps what
Codex printed on its own (notifications and requests), never Codex's answers to Athena, which echo the instructions and
name the account. Each line is `{"at":"<time>","codex":<what Codex printed>}`, or
`{"at":"<time>","athena":{"declined":<method>,"id":<request id>,"reason":<why>}}` for an approval Athena declined on its
own.

### Which Tickets run on Codex

A task names its runtime: `claude`, `codex` or `auto` (the control socket's `submit` and `athena task --runtime`
take it); a task that names none, and every Ticket the router accepts, gets `runtimes.default` (§3, `auto` unless set).
Each Assignment records the runtime its session actually ran on (`Assignment.Runtime`), so one Ticket may move between
runtimes; the metrics and the per-runtime stats (`stats.byRuntime`) name that runtime. Before each launch the node's
runtime chooser picks the runtime (§7). A Ticket pinned to `codex` runs only there: the node asks whether Codex is ready
(the check of §2, cached for 10 minutes when ready and 1 minute when not; the server re-checks every minute, so a
login after the server started shows on the dashboard within a minute) and, when it is not, the Assignment reports failed with why ("codex cannot
run this ticket: Codex is not logged in for Athena: run athena codex login"); `auto` never chooses a runtime that is
not ready. A session that refuses to start because a hook or a setting is not Athena's (§4) fails the same way, with the
refusal's why, and the next launch checks Codex (and trusts Athena's hooks) again. A Codex session's environment has
`ATHENA_RUNTIME=codex`, and its first message lists it with the other execution variables. Every process a Ticket starts,
on either runtime, first writes a marker line to the transcript (`{"athena":"session","runtime":…,"model":…,
"assignment":…,"role":"implement","reason":"start|resume"}`), so the session view shows where a runtime begins. A Codex
Ticket with no model of its own runs Codex's default (§3), which the thread names.

### Hooks, approvals and the rules

Codex's hooks reach the same rules as Claude Code's (`athena hook --runtime codex <Event>`). Codex's tools are mapped
onto Claude Code's vocabulary first:

| Codex tool | judged as |
| --- | --- |
| `Bash`, `shell`, `local_shell`, `exec_command`, `unified_exec` | `Bash` with the command (an argv array joined with POSIX quoting), run in its `workdir`; a command that runs `apply_patch` (`cd sub && apply_patch <<'EOF'`, or the argv `["apply_patch", patch]`) is also a `Write` per path, relative paths from the directory it runs in after `cd`/`pushd` (one the rules cannot follow asks the owner); a command that only mentions a patch header (`grep`) is plain `Bash` |
| `apply_patch` | one `Write` per path the patch adds, updates, deletes or moves to (indented headers too); the strictest verdict wins |
| `write_stdin` | what it types, by the `Bash` text rules: a credential or state path, a token variable, a blocked command or an AI attribution denies; anything else is allowed |
| Claude Code's tool names (`Edit`, `Write`, `Read`, `WebFetch`, …), `mcp__…`, `web_search` | as Claude Code's |
| any other | allowed unless one of its strings names a credential or Athena's state directory |

A command the hook cannot read, and a patch whose paths it cannot all read (a line naming a patch header it cannot
parse), ask the owner. Codex's own approval requests are judged the same way: a command as a `PermissionRequest` on
`Bash`, a file change on a `Write` of each path, while network access (rule `network_access`, "Network: <host>", "any
host" for `*`) and more permissions (rule `codex_permissions`) always ask the owner. Escalations become Approvals exactly
as Claude Code's do (the timeout, the expiry, the end with the session); a file Approval's summary names every path the
rules escalate and Codex's reason (with any write grant).

A command or file approval, and the `PermissionRequest` hook, ask to run the action outside Codex's sandbox. On a node
with `sandbox` set, where a Claude Code worker can never run anything unsandboxed, the rules' deny stays a deny and
their allow becomes an Approval (rule `codex_unsandboxed`, "Run outside the sandbox: <command or paths>"). Without
`sandbox`, where Claude Code's commands run unsandboxed under the rules alone, the rules' verdict stands. Inside Codex's
sandbox the network stays off: Codex cannot allow hosts by name the way Claude Code's sandbox does, so a command that
needs the network (`git push`, a package install) asks to leave the sandbox, and on a sandboxed node that is an Approval.

Codex may ask about one action outside its sandbox through both its `PermissionRequest` hook and an approval request,
in either order: the owner answers one Approval, which the other door's ask about exactly the same action (the same
turn, and the same commands' exact text or the same paths, every one) gets once, while it waits or within 10 minutes of
its answer. A `PreToolUse` ask (to run the action in the sandbox) shares nothing, and the same door asking again asks
anew. Whether Codex 0.160.1 does ask twice, and in which order, is not verified yet (it takes a real turn).

A call Codex's `PreToolUse` hook denies shows no result in Codex's events; the next item (not one of Athena's own
tools, such as the report of the same step) means the model has seen the deny, so a report after it is accepted (Claude
Code's waits for the failed result).

What the hook prints follows the output schemas Codex 0.160.1 embeds (`pre-tool-use.command.output`,
`permission-request.command.output`; pinned in `internal/node/testdata/hooks/codex`). They are Claude Code's shapes with
two exceptions Codex enforces: a `PreToolUse` that allows prints nothing (Codex refuses `permissionDecision: "allow"`),
and a deny always carries a non-empty reason.

Codex's sandbox cannot keep Athena's files and the repository's git config and hooks read-only inside its writable roots
(§5), so for Codex sessions the rules deny, with rule `protected_path`, any write (a patch, a file change, a shell
redirection or file command) to `.athena/settings.json`, `.athena/mcp.json`, `.athena/.gitignore`, the git common
directory's `config`, `config.worktree` and `hooks/`, the git directory's `commondir` and, for a worktree, its own
`config.worktree` and `gitdir` and the working directory's `.git` file: the same list Claude Code's sandbox denies.
The node's own git (creating and removing worktrees, `git status`) runs with `core.fsmonitor=false` and
`core.hooksPath=/dev/null`, whatever a repository's config says, and with `GIT_DIR`, `GIT_COMMON_DIR` and
`GIT_WORK_TREE` pinned to the directories Athena made (`<common>/worktrees/<name>`), so a rewritten `.git`,
`commondir` or `gitdir` cannot hand it another config (a filter driver). A worktree an earlier node made under git's
`<name><number>` is found only when its `gitdir` names the worktree and the worktree is in the work root; otherwise the
node keeps away from it (and logs it). Where a Ticket works (a plain directory, a worktree, the node's clone) is recorded
in the state directory at its first placement (`placements/<ticket>.json`) and never derived again, so a worker that
runs `git init` in a plain directory cannot have the next launch make a worktree of it. The record pins the absolute
working directory (and the repository and its git directory): changing the work root later does not move a Ticket
already placed, which keeps working where it was recorded. A Ticket that ran before records existed and is now found
in a repository is placed there only when its worktree exists and its git directory is the node's; otherwise it is
refused ("the directory became a git repository after the Ticket started, or its worktree was removed; resubmit") and
nothing is recorded. The read-only views (diff, files) of such a Ticket still use what is derived, without recording
it: a diff between trees (no external diff, textconv, hook or fsmonitor) and, only when the worktree exists and its git
directory is the node's, a listing of its working tree (no filter), so a cleaned-up Ticket keeps its diff.

### Keep one athena binary

`config.toml` names the athena binary its hooks and MCP server run, and the server rewrites the file when it starts. Run
`athena codex status|login` with the same binary the server runs (deploy by replacing `bin/athena` and restarting the
server): a different binary finds the server's file stale and leaves it as it is (§2).

## 2. Install and log in

Install the Codex CLI (`codex --version` must print `codex-cli 0.160.0` or newer), then log it in for Athena:

```sh
athena codex login                 # signs in with ChatGPT in the browser
athena codex login --with-api-key  # or reads an OpenAI API key from stdin
athena codex status
```

This is a separate login from the one your own `codex` uses. Athena's Codex lives in its own `CODEX_HOME`,
`<stateDir>/codex` (`~/.athena/codex` by default, mode 0700): its configuration, its credential (`auth.json`, mode 0600,
written by `codex login` itself) and its sessions. Athena never reads or writes `~/.codex`, so nothing in your own
Codex home (settings, hooks, plugins) reaches a worker, and a worker's never reach yours. A repository can carry Codex
settings of its own (`.codex/`): Athena refuses to run a session while any hook it did not write would run, or while a
repository's settings would load at all (§4).

`athena codex status` prints what the check found: whether Codex is installed, its version and whether it is tested,
whether `config.toml` is what this `athena` would write, whether it is logged in (the kind of account only, never its
id or email), Athena's hooks, and whether it is ready. `athena codex status` and `athena codex login` write
`config.toml` only when there is none: an existing one is the server's (its binary, and the trust Codex recorded), so
they leave it as it is and say when it differs; restarting the server rewrites it.

## 3. `config.json` `runtimes`

```json
{"runtimes": {"default": "auto", "preferred": "claude", "avoidAbove": {"5h": 85, "7d": 90},
              "claude": {"maxSessions": 0},
              "codex": {"enabled": true, "binary": "codex", "model": "", "maxSessions": 0}}}
```

- `default`: the runtime of a task that names none: `claude`, `codex` or `auto` (the default). `codex` needs Codex
  enabled.
- `preferred`: the runtime `auto` picks when no warm session decides (§7): `claude` (the default) or `codex`.
- `avoidAbove`: per quota window (`5h`, `7d`, or `<n>m` for a window of n minutes; any other key is an error), the
  percent used above which `auto` avoids a runtime unless every runtime is above (nucleos' quota brake). Left out it is
  `{"5h": 85, "7d": 90}`; `{}` brakes nothing.
- `claude.maxSessions`, `codex.maxSessions`: how many sessions the runtime runs at once; 0 is no cap (the node's slots
  still bound them).
- `enabled`: on unless set to `false`. An enabled Codex that is not ready is never chosen, and the dashboard says why.
- Reviews (§8): Codex reviews whenever it is ready. Claude Code reviews only with `sandbox.required: true` in
  `config.json` (on macOS or Linux), so a node that pairs Tickets with review needs one of the two.
- `binary`: the codex executable; `athena server --codex PATH` overrides it.
- `model`: the default Codex model; empty leaves it to Codex.

## 4. Hooks and trust

The server writes `<stateDir>/codex/config.toml` itself, whenever it checks Codex and the file differs; do not edit
it. It holds Athena's
settings: approvals asked of Athena (`approval_policy = "on-request"`, `approvals_reviewer = "user"`), the
`workspace-write` sandbox, no update check, no analytics, the features that act outside the worktree or spend quota
unasked turned off (apps, plugins, browser and computer use, multi-agent, image generation, memories, realtime), Athena's
MCP server, and Athena's hooks: `athena hook --runtime codex <Event>` for `PreToolUse`, `PermissionRequest`,
`PostToolUse`, `Stop` and `SessionStart`. The two that wait for an Approval (`PreToolUse`, `PermissionRequest`) wait
up to 24 hours, whatever `--approval-timeout` is, so the file does not depend on how `athena` was started; the node
answers them when the Approval times out, and the server refuses an approval timeout longer than 23 h 55 min while
Codex is on.

**Deviation from the design (§6.1):** the hooks are in `config.toml`, not in a separate `hooks.json`. Codex 0.160 loads
`[[hooks.<Event>]]` tables from `config.toml`; one file is written in one step, and the trust check below sees the hooks
where Codex loaded them from. What the design asks for holds: the hooks live in Athena's own `CODEX_HOME`, never in your
global configuration or the repository.

Codex runs a hook from a configuration file only once it is trusted. Athena trusts only the hooks it wrote: it asks
Codex for the hooks it would run (`hooks/list`), records the trust of its own (`hooks.state."<key>".trusted_hash`,
written by Codex at the end of `config.toml`), and lists them again. A hook is Athena's only when it is exactly what
Athena wrote: in its `config.toml`, the same event and command, the same timeout, synchronous, with no matcher. Codex
is ready only when Athena's five hooks are there, enabled and trusted, **no other hook is enabled**, trusted or not,
from whatever layer (a repository's `.codex/`, a plugin), and no managed hook (which Athena cannot turn off) is
configured at all. Otherwise Athena refuses to start Codex and says why: there is no silent
fallback to a session its hooks do not control.

Every session checks again, on its own app-server and for its own worktree, since a repository's `.codex/` applies only
there: the hooks as above (`hooks/list`; a session never records trust, the readiness check does), and the settings
Codex would run with (`config/read`). It refuses when any feature Athena turns off is on, when an MCP server other than
Athena's is configured or Athena's runs another command, or when a repository's `.codex/` layer is loaded at all. Codex
0.160.1 loads one only for a project the user configuration trusts, which Athena's never does; a loaded one could
change the model provider, the sandbox or add a notify command, none of which the effective features and servers show.
The check also fails when Codex does not list its configuration layers, or lists none from Athena's `config.toml` as its
user configuration. It does not refuse the layers of the machine itself: `/etc/codex/config.toml` (system), macOS
managed preferences (MDM) and an enterprise-managed configuration are the machine owner's or administrator's, and Athena
does not check what they set beyond the features and MCP servers above.

## 5. Sandbox

A Codex worker runs in Codex's own `workspace-write` sandbox (a reviewer in `read-only`), with network access off,
for every command Codex runs itself. Athena passes the writable roots in every `turn/start`'s `sandboxPolicy`
(`workspaceWrite` with `writableRoots`, network off; `readOnly` for a reviewer); the session's working directory is
always writable, and the CA variables a sandboxed command needs are in the session process's environment, which its
commands inherit. It is not the same sandbox as Claude Code's, and three things differ:

- **No denied sub-paths.** Claude Code's sandbox can keep a path read-only inside a writable directory; Codex's has no
  deny list. Athena therefore makes writable only what a worktree needs: the worktree's git directory
  (`<common>/worktrees/<name>`) and the common directory's `objects`, `refs` and `logs`, never the common directory as
  a whole, so its `config` and `hooks` stay read-only. The owner's `sandbox.allowWrite` paths (a build cache) are
  writable roots too, less any that holds a protected path or lies in one (the node logs each it leaves out).
- **No domain allowlist.** Claude Code's sandbox lets named hosts through (GitHub and `sandbox.allowedDomains`);
  Codex's network is all or nothing. Inside it the network stays off, so a command that needs it (`git push`, a
  package install) asks to leave the sandbox, which on a node with `sandbox` set is an Approval (§1, rule
  `codex_unsandboxed`). A Ticket from a `stranger` that must not reach the network should be pinned to `claude`
  until Codex's network proxy is stable.
- **Protected paths through the rules.** What Claude Code's sandbox denies by path, Athena's hook rules deny for
  Codex (rule `protected_path`): writes to `.athena/settings.json`, `.athena/mcp.json`, `.athena/.gitignore`, the git
  common directory's `config`, `config.worktree` and `hooks/`, the git directory's `commondir`, a worktree's own
  `config.worktree` and `gitdir`, and the working directory's `.git` file (§1). `<gitDir>/config.worktree` stays
  writable under the roots above, so the rules are what protect it.

**The git links are put back after each session.** The rules read a command's text, so an indirect write gets past
them: a script, `python3 -c "open('.git','w')…"`, a test or a Makefile target can repoint the working directory's
`.git` file, or the git directory's `commondir` or `gitdir`, at a directory of the worker's, or write
`config.worktree`. The node's own git never reads those files (its git directories are pinned), but the owner's git
does: run in the worktree, by hand or by an IDE or a shell prompt, it would follow them to a config of the worker's
(`core.fsmonitor`, `core.hooksPath`, a filter) and run a program outside any sandbox. So after each Codex session
process ends, before every launch in the worktree (of either runtime: the next session's sandbox is built from these
links, so links left repointed by a node that died mid-session never become its writable roots), and again before an
ended Ticket's worktree is kept or removed, the node checks the three link files
against the Ticket's placement record (the git directory Athena made, the repository's git directory) and rewrites
any that differ, and puts `config.worktree` back to what git made when the worktree was created (recorded at the
Ticket's first launch: git copies the main worktree's when the repository has `extensions.worktreeConfig`), removing
one git did not make. Each restore is logged as a warning ("restored a git link a session changed"). What is left:

- **While a Codex session runs**, the links may point elsewhere. Do not run git (or open an IDE) in the worktree of a
  running Codex Ticket.
- A process the session left running after it ended could write them again. Whether Codex's background terminals
  leave the session's process group is not verified yet ([smoke test](smoke-test.md), Manual Codex session).
- A Ticket placed before the node recorded `config.worktree` has its `config.worktree` left as it is (the links are
  still checked), and a worktree made before Athena checked its git directory's name is checked only while its
  `gitdir` still names it; otherwise the node logs "git links not checked", moves the worktree's `.git` aside to
  `.git.untrusted` (so no git follows it), launches nothing there, and keeps away from it.
- A kept worktree's files are the worker's. The log line that keeps it says so: read them before running anything
  there (a `Makefile`, an `.envrc`, a test).

Found with Codex 0.160.1's `codex sandbox`: Codex keeps a workspace's own git directory read-only even inside a
writable root, so a worktree commits only with the roots above. This was verified with `codex sandbox -c
sandbox_workspace_write.writable_roots=…`, not with a session's `sandboxPolicy.writableRoots` (checking that takes a
real turn). git prints `error: Unable to create '…/packed-refs.lock': Operation not permitted` during a commit; it is
harmless, and the commit succeeds.

## 6. Attribution

Commits and pull requests carry no AI attribution, whichever runtime made them. Codex tells its model to add a
`Co-authored-by: Codex <noreply@openai.com>` trailer unless the ChatGPT *workspace* turns that off; no local setting
does, so Athena cannot switch it off in `config.toml` and enforces the rule itself. The rules deny (rule `attribution`, for
both runtimes, right after the blocked commands) a `git commit` or a `gh pr create|edit` (also as `athena gh`, or inside
`sh -c`) whose message, `--trailer`, title or body matches a co-author trailer naming Codex or Claude, a "Generated
with Codex" or "Generated with [Claude Code]" footer, or a `noreply@openai.com` or `noreply@anthropic.com` address. The
rule reads `-m`/`--message`, `--trailer` (and git's abbreviations, `--mess`), `--title`/`-t` and `--body`/`-b`, and the
files `-F`/`--file`/`--body-file` name when they are in the Ticket's directory (up to 64 KiB; relative to where the
command runs them: its `workdir`, a `cd`, `git -C`); a message read from stdin (`-F -`, `/dev/stdin`, a here-document)
or from a file the rule cannot read there (outside the Ticket's directory, or written by the same command) is looked
for in the whole command. Wrappers are seen through (`env`, `sudo -u x`, `nice -n 5`, `timeout 60`). Its reason tells the worker: "remove the AI attribution trailer or footer: commits and
pull requests carry none". It is a guard against a model's habit, not against a worker set on evading it (a message
built by command substitution is not read, nor is a file outside the Ticket's directory).

## 7. Quota and failover

**Quota.** Each session reports its usage limits: Codex from `account/rateLimits/read` at start and
`account/rateLimits/updated`, Claude Code from `rate_limit_event` (a window is limited only by `status: "rejected"`).
The node's tracker (`internal/quota`) keeps the latest reading of every window per runtime, keyed by bucket and name:
a Codex window's bucket is its snapshot's `limitId` (`codex`, or a per-model limit's id), so two limits' `5h` windows
never overwrite each other; Claude Code's windows have no bucket. The runtime is judged as a whole, conservatively: a
limited window in any one Codex bucket limits all of Codex, and every bucket's windows count toward its headroom and
`avoidAbove` (Athena does not yet know which models a bucket covers). A window whose reset has passed is stale and no
longer counts. A runtime's headroom is 1 − the largest `used` of its windows that have not reset (1 while nothing is
measured).

A runtime is limited until the reset of a window reported as limited, or after a turn stopped by a limit until that
limit's reset. A limit that a window's reading set lifts early when the same window (same bucket and name) later reads
not limited and below 100% used, as after a plan upgrade, overage turned on or an early reset, unless another window
still reads limited; a limit that stopped a turn waits for its reset. A limit with no reset, or with one already passed, backs off instead: 15 min, doubling per consecutive
limit up to 2 h; a transient one (a rate of requests) 1, 2, 4 … min up to 30 min. A turn that ends well starts the
backoff over. The tracker writes `<stateDir>/quota.json` (0600, whole, on every change): per runtime the last reading of
each window with its `read_at`, the limit and the backoff count. There is no history.

**The bars.** The dashboard's node card shows each runtime with its version ("untested" when Athena was not verified
against it), whether it is ready (or why not), and one bar per quota window: the percent used of that window (amber
from 85%) and when it resets. "Not measured yet" means no session has reported a reading since the server started
(Codex reads it at each session's start, Claude Code reports it during a turn); "Limited until <time>" means the
runtime is limited (below) and `auto` places nothing there until then; a window whose reset has passed shows "reset
since read", and a reading over 10 minutes old says its age. The sidebar shows each runtime's fullest window. A limit
is amber, never orange: it waits by itself and needs nobody.

**The chooser.** Before each launch, and before it prepares the Ticket's directory, the node chooses the runtime:

1. An Assignment that ran keeps its runtime (a resume continues its session); a Ticket pinned to `claude` or `codex`
   runs there, except its reviews, which are chosen as under `auto`. Not ready, the launch fails with why; limited, it
   waits until the reset; at its `maxSessions`, it waits for a session to end. No other runtime takes its place, with
   one exception: a running Assignment of an `auto` Ticket, or a review, whose runtime only its session pins and is
   limited when the session would resume fails over (below) when rule 2 would launch it now; otherwise it waits until
   the reset, or until another runtime frees, whichever comes first.
2. `auto`: the candidates are the ready runtimes that are not limited and below their `maxSessions`. A review drops the
   implementer's runtime when another candidate is left. Candidates above an `avoidAbove` window are dropped unless
   that leaves none. Then the runtime of the Ticket's latest Assignment of the same role that ran wins (a warm
   session), else `preferred`, else the largest headroom.
3. No candidate: the launch waits until the earliest reset, or for a session to end when a session cap was the reason.
   No runtime ready at all fails with "no runtime is ready: <runtime>: <why>; …".

A waiting launch holds no slot: the Ticket shows working/queued and its runner logs why ("launch waits for its
runtime"). A new Assignment continues the Ticket's last session only when it runs on that session's runtime and role; a
review always starts fresh. The node's status lists each runtime with its last known readiness (Claude Code is taken
as ready), its quota and its live sessions; the server checks the other runtimes once at start.

**Failover.** A turn that stops at its usage limit is no failure and asks no person. Codex says so in the turn's
error (`usageLimitExceeded`, or `rateLimitExceeded` for a transient limit); Claude Code with a `rate_limit_event`
whose status is `rejected` (whichever window it names, one Athena does not map too) followed by an error `result`, or
with an error `result` whose text says it hit a limit. A text that says "rate limit" but neither "usage limit" nor
"hit your limit" is transient (a rate of requests: the short backoff); "Claude AI usage limit reached|<unix time>"
resets at that time. The node then:

1. records the limit in the quota (above);
2. checkpoints the work in progress, when the Ticket runs in a git worktree (below);
3. ends the Assignment with reason `usage_limit` and its limit (runtime, reset, message) and queues a new Assignment of
   the same role (`UsageLimited`): pinned to the limited runtime when the Ticket names one, else for the chooser to
   place. The trigger Inputs the limited session never read go to it first. No Report is filed and no question raised:
   the Ticket shows working/queued until the new Assignment runs;
4. ends the limited session as if stopped. Its process has exited before the next session starts: a Ticket has one
   live session, so two sessions never write the same worktree at once.

A limit on a turn in which the worker had already reported `blocked`, `waiting` or `ready` (since its last Input) is
not failed over: the turn ends as one that ended well (a ready worker on a Ticket that waits for its pull request
parks), the question or the hand-off stands, and only the quota records the limit.

**Failover before a resume.** A session with no live process can find its runtime limited when it would resume: a
worker parked on its pull request whose pull request gets a comment, an idle Assignment after a server restart, the
Assignment that applied a held review report (§8) when an Input reaches it, or a session whose limit the quota
recorded before a crash kept the Engine from hearing of it. On an `auto` Ticket (and for a review, which its Ticket
never pins) the node does not wait out the reset when another runtime is free now: it checkpoints the work, ends the
Assignment with reason `usage_limit` and the quota's reset, and queues a new one, as in steps 2 and 3 above, before
any process starts. The new Assignment runs on the free runtime with the handoff note, and the Input that woke the
session goes to it. With no runtime free, it waits until its runtime's reset or until another frees, whichever is
first, and then looks again. A Ticket pinned to `claude` or `codex` waits for its runtime's reset, then resumes its
session.

The new Assignment launches through the chooser, the only wait there is: an `auto` Ticket goes to the other runtime
(the limited one is no candidate), or, with both limited, waits until the earliest reset; a pinned Ticket waits for its
runtime's reset and then resumes its session. A reset that is unknown or already past is never waited on: the quota
backs off (15 min, doubling) and the limit's reset shows as unknown. The wait for a reset counts toward the Ticket's
`queueWait` stat (from the new Assignment's creation to its start), so after a limit it can be hours.

The new session works in the same directory (the Ticket's recorded placement). Its transcript marker says
`"reason":"failover"` when it moved to the other runtime. A new worker session whose previous worker Assignment ended
at its limit, and a resumed session that another runtime's work came after, get a handoff note after the worker
preamble: who stopped where, the Ticket's last 20 Inputs, its last 3 Reports, the branch's commits since its base, its
uncommitted changes and untracked file count, and what the previous session said last; at most 32 KiB, every part
quoted so nothing in it reads as an Input. A review, a retry or a crash recovery gets none. The timeline shows the end
as `assignment`/`limited` (subject the runtime, text the RFC 3339 reset or empty).

**The checkpoint (a deviation from §6.1a).** The spec says the work in progress "is committed to the Ticket branch
first". Athena commits it instead to the side ref `refs/athena/<lowercased ticket id>/checkpoint` (a commit on the
branch's HEAD, through an index of its own: `git add -A` respecting `.gitignore`, `write-tree`, `commit-tree`,
`update-ref`), and leaves the branch, the index and the files as they are. Nothing is lost, because the next session
works in the same worktree, and no checkpoint commit lands on the pull request. The node's git runs it with no shell,
its git directories pinned, a minimal environment and a 30 s timeout; a failure is logged and does not hold up the
failover. The ref is deleted when the Ticket's worktree is cleaned up (a worktree kept for its changes keeps its
checkpoint too), so no work in progress, an untracked `.env` say, stays reachable in the repository after that. Moving
the branch instead would be a small change.

## 8. Review pairing

A Ticket paired with review (`athena task --review`, the submit op's `review`, the New task screen's "Review each
ready report" switch, shown with two ready runtimes or a Policy that pairs, or the Policy's
`"pairing": {"review": "on", "maxRounds": 2}` for every Ticket that names none; `--review=false`, or the switch turned
off, turns it off for one task) has each ready report of its implementer reviewed before it counts:

1. The ready report is recorded and held (`Ticket.review`); the implementer's Assignment ends with reason `review` and a
   pending review Assignment (role `review`) starts. Nothing is said yet: no report event, no relay to the router, no
   reply. The Ticket shows working.
2. The chooser places the review on the other runtime than the implementer's when that one is ready (a pinned Ticket
   pins its implementer, not its reviews); with one runtime, it reviews on that one. A limit on a review fails over
   like any session's, with no handoff note.
   - **A review runs on Claude Code only when `sandbox.required` is `true`** (on macOS or Linux), so that Claude Code's
     OS sandbox bounds every command the review runs. Otherwise Claude Code is not eligible for reviews ("a Claude Code
     review needs sandbox.required: true"), and the review goes to Codex, even when Codex implemented the Ticket, and
     whatever runtime the Ticket names. With no eligible runtime (a Claude-only node without a required sandbox, or
     Codex not ready) the review fails at once: the held report applies (6) and the timeline's review row says why.
3. The review session starts fresh, in the implementer's directory, after the implementer's process has exited, and
   is read-only through these layers. The OS sandbox is the guarantee: Codex's `read-only` sandbox, or Claude Code's
   required sandbox. The hook rules and the rest are defence in depth:
   - **Always: the hook rules** (rule `read_only`). Write tools, Bash write targets and Codex's `write_stdin` are
     denied, and a Bash command runs only when every command in it is on an allowlist: `git status|diff|log|show|
     blame|grep|rev-parse|ls-files|merge-base|cat-file` (no `--output`, `-c`, `--config-env`, `--exec-path`,
     `--ext-diff` or grep's `-O`), `cat`, `ls`, `rg` (no `--pre` or `--hostname-bin`), `grep`, `find` (no `-delete`,
     `-exec`, `-ok`, `-fprint`, `-fls`), `head`, `tail`, `wc`, `file` (no `-C`), `stat`, `cd`, `pwd`, `echo`, `true`,
     and the repository's test and vet runners (`go test|vet`, `cargo test|check`, `make test|check`, `npm|pnpm|yarn
     test` or `run test`, `pytest`, `python -m pytest`) without output or profile flags or an option that names a
     program or build file: go's `-exec`, `-toolexec`, `-vettool`; make's `NAME=value`, `-f`, `--file`, `--makefile`,
     `-e`, `--eval`, `-C`, `--directory`; cargo's `--config`, `-Z`; npm's, pnpm's and yarn's `--script-shell`,
     `--node-options`; pytest's `-p`, `-c`, `--config-file`, `-o`, `--override-ini`. A command named by a path runs
     only from `/bin`, `/usr/bin`, `/usr/local/bin`, `/opt/homebrew/bin`, `/usr/sbin` or `/sbin` (`./bin/cat` is
     denied). Everything else, `gh`, `curl` and `sudo` included, is denied, and so is a variable assignment before a
     command (`GIT_PAGER=… git log`). The allowlist was written and tested case by case, not probed adversarially:
     it is not what keeps a review read-only. A test runner runs the repository's own code, which the review's OS
     sandbox confines.
   - **Always: no escalation.** Whatever would ask the owner (a rules escalation, a Codex approval request for a
     command, a file or permissions, whether or not `sandbox` is configured) is denied: a review never makes a person
     stage.
   - **Always: no GitHub credentials.** A review gets no token: `athena git-credential` and `athena gh` refuse it, and
     its git has no credential helper. It reviews the local branch.
   - **Codex:** the `read-only` sandbox, always.
   - **Claude Code:** started without its file-writing tools, in its required sandbox, which keeps the owner's
     `allowWrite` paths outside the working directory (build caches, so tests can run), not the git directory, and
     denies the working directory.

   Its MCP server (`athena mcp --ticket <id> --role review`) has only the `review` tool. Its first message names the
   round, the branch, its base, `git diff --stat <base>...HEAD` and the held report's summary; its system prompt is the
   `reviewer` prompt part. What the implementer received before its ready report counts as read then. Inputs that
   reach the Ticket during the review wait for the implementer.
4. `review(verdict, findings)`: findings are lines `- [high|medium|low] <file>:<line> — <what>`, or `- none`; a high or
   medium finding needs `changes`, `- none` needs `approved`, and anything else is refused for the reviewer to correct.
   The review ends with reason `reviewed` and its outcome (verdict and first finding).
5. `approved`, or `changes` once `maxRounds` rounds are used: the held report applies as if the implementer filed it
   now, on a new Assignment that continues the implementer's session. Without a pull request the Ticket ends completed
   with the usual result reply; with one it waits handed off, and the next Input resumes the implementer's session (one
   that arrived during the review resumes it at once).
   - **Unless a person's or a Thread's message arrived during the review** and the report would end the Ticket: then the
     held report is dropped (no report event, no reply) and a new implement Assignment, placed as in a `changes`
     round, resumes the implementer's session with those messages; its next ready report settles the Ticket, and is
     reviewed again while rounds remain.
   - `changes` within the rounds: a new implement Assignment (pinned to the implementer's runtime when the Ticket names
     one) resumes the implementer's session with the findings as a `review` Input; its next ready starts the next round.
6. A review that reports failure (it crashed past its restarts, its launch failed, or it ended two turns without a
   verdict after one nudge) counts as a round, outcome `failed`, and applies the held report as in 5.
7. The rounds cap is per Ticket, not per ready report: on a long-lived pull-request Ticket, follow-ups after the first
   rounds get no review once `maxRounds` is used. The Policy's `maxRounds` is read at each ready report.
8. **A review waits for a runtime like any launch.** With the review's runtime limited (both runtimes, on a two-runtime
   node; the one, on a node where only one can review), the review waits until the earliest reset, or 15-minute
   backoffs when none is known, and the implementer's finished report stays held all that time. A person's message
   that arrives meanwhile waits for the implementer, who gets it once the review ends. This is a known wait: nothing
   fails the review after a bound yet.

## 9. Fixtures

Every Go and browser test runs `fakecodex` (`internal/runtime/codex/testdata/fakecodex`), which replays recorded
app-server lines, never this machine's `codex` or its quota. Its fixtures come from a real Codex:

- `scripts/capture-codex-fixtures.sh schema` regenerates `internal/runtime/codex/testdata/codex/protocol.json` from the
  installed Codex. It is free.
- `ATHENA_CAPTURE_CONSENT=spend-codex-quota scripts/capture-codex-fixtures.sh turns` runs one short real turn and
  spends a little quota; no `make` target runs it.

## 10. Troubleshooting

| `athena codex status` says | Do |
| --- | --- |
| `Codex is not installed (codex not found)` | Install the Codex CLI, or set `runtimes.codex.binary` / `--codex`. |
| `Codex 0.x is older than the supported 0.160.0` | Update Codex. |
| `Codex is not logged in for Athena: run athena codex login` | Run it. |
| `a … hook Athena did not write is enabled (…)` | Remove or disable that hook (the path says where it is). |
| `Athena's … hook is "untrusted", not trusted` | Codex did not take Athena's trust; check `<stateDir>/codex/config.toml` is writable by you. |
| `config.toml was written by another athena …: restart the server to rewrite it` | The file is the running (or last) server's, for another binary; restart `athena server` with the binary you want. |
| `codex settings are not under Athena's control: the Codex feature … is on` | Something besides Athena's `config.toml` turns it on (a managed configuration, say); remove that. |
| `codex settings are not under Athena's control: an MCP server Athena did not configure is set up (…)` | Remove that server from the configuration that adds it. |
| `codex settings are not under Athena's control: a repository's Codex settings are loaded (…)` | Athena's home trusts a project; restart the server to rewrite `config.toml`. |
| `codex app-server initialize: … unknown configuration field …` | This Codex does not know a key Athena writes: use a tested version. |
