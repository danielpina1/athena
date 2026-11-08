# Smoke test with real Claude Code

Plan 1 is tested with a fake `claude` in CI. This page shows how to check it against the real, logged-in
Claude Code (2.1.x) on your machine, and Codex in the [manual Codex session](#manual-codex-session). It costs a few
cents (the model is `haiku`) and needs the network.

## Automated

    ATHENA_LIVE=1 go test -tags live -run Live -v ./internal/server

The tests skip unless `ATHENA_LIVE=1` and `claude` is on `PATH`. Each starts a real server in a temp state dir, builds
the `athena` binary for the hooks and the MCP server, and uses `--model haiku`.

- `TestLiveHelloFile`: a task writes `hello.txt` (`hi`) in its directory and reports ready. The Ticket must end
  `completed` within 3 minutes. The test also checks the settings and MCP config the node wrote (hook timeouts, the
  Ticket and Assignment in the MCP env), that no plugin or hook of your own `~/.claude` settings loaded
  (`--setting-sources ""`), and that `<state>/transcripts/<ticket>.jsonl` exists and renders through the log op.
- `TestLiveWriteOutsideEscalates`: a task writes `/tmp/athena-live-outside.txt`. An Approval for the `Write` must
  appear within 2 minutes; the test denies it, then checks the file does not exist (at `/tmp` and `/private/tmp`) and the
  worker's report mentions the denial. Set `ATHENA_LIVE_APPROVAL_DELAY=75s` to hold the answer back and show that the
  hook outlives the CLI's default hook timeout.

- `TestLiveBashWriteOutsideEscalates`: the same write as a shell redirection (`echo x > /tmp/...`) must escalate too,
  with rule `write-outside-ticket`; it is denied and the file must not exist. (A worker denied a `Write` once got the
  file written through Bash; the rules now cover redirections and the common file commands. Bash cannot be confined
  completely without a sandbox: a script can write anywhere without naming the path.)

A model can answer differently on each run; a failure that goes away on a second run is worth a look at the transcript
(the test prints it on failure), but is not necessarily a bug.

## Manual session

Build and start the server in one terminal. Keep the state dir path short: Unix socket paths are limited to about
104 bytes on macOS.

    make build
    ./bin/athena server --state-dir /tmp/athena-smoke --model haiku --approval-timeout 5m

In a second terminal (add `--state-dir /tmp/athena-smoke` to each command, or `export` nothing: the flag is per command):

    S=/tmp/athena-smoke
    ./bin/athena task "Create hello.txt containing exactly hi in your working directory, then report ready." --state-dir $S
    # prints the Ticket id, for example T-0001
    ./bin/athena status --state-dir $S               # list open Tickets and their stage
    ./bin/athena status T-0001 --state-dir $S        # one Ticket: directory, approval, assignments, last report
    ./bin/athena logs T-0001 -f --state-dir $S       # follow the rendered transcript until the Ticket ends

A task that needs a person:

    ./bin/athena task "Write /tmp/athena-smoke-outside.txt with the text x, then report ready." --state-dir $S
    ./bin/athena status --state-dir $S               # stage person/approval
    ./bin/athena approvals --state-dir $S            # the pending Approval, its id and what it would do
    ./bin/athena approve <approval id> --state-dir $S         # or: deny <approval id> --note "not allowed"
    ./bin/athena reply T-0001 "..." --state-dir $S   # send a message to a Ticket's worker
    ./bin/athena stop T-0001 --state-dir $S

What to look for:

- `status` shows `working`, then `person/approval` while a write outside the Ticket directory waits, then
  `ended:completed` (or a worker report such as `blocked` after a denial).
- The Ticket's directory is `<state>/work/<slug>-<id>`; `hello.txt` is there. A task with `--dir <git repo>` gets a
  worktree there on branch `athena/<id>` instead.
- `<state>/transcripts/<id>.jsonl` holds the raw stream-json; `<state>/logs/athena.log` holds one JSON line per
  event and hook decision (never tool inputs).
- Stop the server with Ctrl-C. Running Tickets resume on the next `athena server` start, and Approvals left pending
  are expired.

Clean up with `rm -rf /tmp/athena-smoke /tmp/athena-smoke-outside.txt`.

## Manual Codex session

This spends a little of your Codex quota (and of Claude Code's, for the `auto` part). Codex must be installed
([Codex setup](codex.md), section 2); Athena logs it in separately from your own `codex`, in `<state>/codex`, and never
reads `~/.codex`. With the server above stopped:

    S=/tmp/athena-smoke
    ./bin/athena codex login --state-dir $S          # once per state dir: signs in with ChatGPT in the browser
    ./bin/athena codex status --state-dir $S         # must end with "ready"
    ./bin/athena server --state-dir $S --model haiku --approval-timeout 5m

In a second terminal, on a scratch git repository (`git init /tmp/athena-scratch && git -C /tmp/athena-scratch commit
--allow-empty -m init`):

    ./bin/athena task --runtime codex --dir /tmp/athena-scratch "Add hello.txt containing hi, commit it, then report ready." --state-dir $S
    ./bin/athena task --runtime auto --review --dir /tmp/athena-scratch "Add bye.txt containing bye, commit it, then report ready." --state-dir $S
    ./bin/athena status <id> --state-dir $S          # the runtime of each assignment, and the review rounds
    ./bin/athena logs <id> -f --state-dir $S

What to look for:

- `athena codex status` shows the version, "logged in" (the kind of account only, never its id or email), Athena's five
  hooks trusted, and ready; anything else is in the troubleshooting table of [codex.md](codex.md).
- The `codex` Ticket's assignment runs on `codex` and ends `completed`. The `auto` Ticket runs an implement assignment
  and then a `review` assignment on the other runtime (Codex reviews unless `sandbox.required` is `true`, see
  [codex.md](codex.md) section 8) before it ends; its timeline in the dashboard shows "Review started · …" and the
  verdict.
- `git -C /tmp/athena-scratch log --format=%B athena/<id>` shows no `Co-authored-by` trailer and no "Generated with"
  footer; with a GitHub repository, neither does the pull request.
- The dashboard's node card shows a quota bar per window for each runtime that has run, moving as the sessions run.
- Nothing is ever written to `~/.codex` (`ls -la ~/.codex` before and after).
- A process a Codex worker leaves running ends with its session. Ask a `codex` task to start a long-running background
  command (for example "run `sleep 600` in the background, then report ready"); once its Ticket has ended,
  `pgrep -fl "sleep 600"` must find nothing. Codex's background terminals may start their commands in a session of
  their own, outside the process group Athena stops: if the process is still there, report it (and `kill` it by hand).

Clean up with `rm -rf /tmp/athena-scratch` (and the state dir as above).

## GitHub (needs your GitHub App)

This part needs a GitHub App you created and installed on a **sandbox repository** ([setup guide](github-app.md)). Then:

    S=/tmp/athena-smoke
    ./bin/athena github setup --app-id N --slug S --key ~/.athena/github-app.pem --installation N --repo you/sandbox --state-dir $S
    ./bin/athena github check --state-dir $S        # prints the bot, the account and the repositories
    ./bin/athena server --state-dir $S --model haiku

Mention the App in an issue of the sandbox repository, for example `@<slug> create hello.txt containing hi and open a PR`,
written from your own account (the App's own comments are never Triggers). What to expect, within about a minute (polls
run every 30 s):

- In the default mode, `observe`: `athena triggers` lists the mention, `athena decisions` shows `accept (would accept)`,
  and nothing is written to GitHub.
- After `athena policy show > p.json`, editing `"mode"` to `"auto"` and `athena policy set p.json --state-dir $S`: one
  acknowledgement comment by `<slug>[bot]`, a Ticket (`athena status`) on branch `athena/<ticket>`, and a pull request
  adding `hello.txt`. Merge it on GitHub; the Ticket ends `merged`, and nothing else is posted twice.
- `athena threads` shows the issue's Thread and its Ticket; `athena task --repo you/sandbox "..."` starts a Ticket
  yourself.

The same flow runs unattended with `go test -tags live -run LiveGitHub` (variables and cost in the setup guide).

## Pause, shadow and promotion (needs your GitHub App)

This part uses the same **sandbox repository** and server as the GitHub part, with `"mode": "auto"` in the Policy. The
new commands are `athena pause`, `athena resume`, `athena proposals`, `athena apply` and `athena discard`; the rest is the
dashboard ([dashboard.md](dashboard.md)) and the commands above. The brakes are described in [autonomy.md](autonomy.md).

    S=/tmp/athena-smoke
    ./bin/athena task --repo you/sandbox "Create pause-check.txt containing hi, then report ready." --state-dir $S
    ./bin/athena pause --state-dir $S               # "Athena is paused. Running sessions stop at the end of their turn..."
    ./bin/athena status --state-dir $S              # a "paused since ..." line
    ./bin/athena task --repo you/sandbox "Create second.txt containing two, then report ready." --state-dir $S
    ./bin/athena resume --state-dir $S              # "Athena resumed."

Pause and resume:

- After `pause`, the first Ticket's session stops at the end of its current turn (the dashboard shows the paused
  banner), and the second Ticket is created but does not start: its `status` stays queued. Mention the App in a sandbox
  issue: the Trigger is listed by `athena triggers`, but `athena decisions` shows no new decision and no reply is posted.
- Stop the server with Ctrl-C and start it again while paused: it logs "athena is paused", the dashboard still shows
  the banner, and nothing starts.
- `resume` starts the queued Ticket, and the held reply goes out within a few seconds. In the dashboard, **Resume**
  asks you to confirm with your passkey first; **Pause** does not.

Brake and holding a repository for you. In the dashboard, Settings > Repositories: set the sandbox repository to
"Shadow (held for you)" and save (confirm with your passkey). Then:

    ./bin/athena task --repo you/sandbox "Create held.txt containing held, commit it, open a pull request, then report ready." --state-dir $S
    ./bin/athena proposals --state-dir $S           # one line per held step: id, repository, kind and summary
    ./bin/athena apply <proposal id> --state-dir $S # prints "<id>: applied"
    ./bin/athena discard <proposal id> --state-dir $S

- Until you apply, GitHub shows no `athena/<ticket>` branch and no pull request, and the Ticket shows "Review what
  Athena holds for you" in Needs you. `athena proposals` lists a push ("Push athena/<ticket> to you/sandbox: 1 new
  commit, held for you") and a pull request ("Open the pull request ... held for you").
- Apply the push: the branch appears on GitHub, pushed by `<slug>[bot]`. Apply the pull request: it opens from that
  branch with exactly the title and body the worker gave. Discard one instead: nothing reaches GitHub, and a held
  reply is never sent.
- To see the waiting-on-you brake: in Settings > Repositories set "Hold work in you/sandbox when this many wait on
  you" to 1, leave one Proposal pending, and open a second sandbox issue that does not mention the App. Its decision
  (`athena decisions`, or the dashboard) reads "Held: 1 thing already waits on you in you/sandbox (limit 1)." and no
  Ticket starts for it until you apply or discard the pending Proposal.

Promotion:

- Approve the same kind of escalation (for example a `pip install` outside a virtualenv) five times in a row in the
  same repository, in the dashboard. Needs you then shows "Always allow it here?" for that
  class. Dismiss it, and it stays away until five more approvals. Allow it (with a passkey confirmation), and the next
  such ask runs with no Approval; Settings > Trust lists the standing rule with a Remove button.
- A rule such as `credential-read` or `write-outside-ticket` is never offered, however often you approve it.

Clean up: set the repository back to Live, remove the standing rule, and `rm -rf` the state dir as above.

## Autopilot (needs your GitHub App)

This part uses the sandbox repository and server of the GitHub part, with the sandbox repository already polled by
the GitHub Source. How Autopilot works is in [autonomy.md](autonomy.md) section 9. Use a throwaway repository for the
`auto` step: Autopilot will start a Ticket there on its own.

1. In Settings > Autopilot, add an entry: name `Branch check`, repository `you/sandbox`, "When the default branch
   moves", instructions "Create autopilot-check.txt containing hi, then report ready." Save (confirm with your
   passkey). Leave the accept mode at `observe`.
2. Push a commit to the default branch of `you/sandbox`. Within about 5 minutes the Routing page shows a Decision
   `accept (would accept)` for rule `autopilot/branch-check`. `athena triggers` lists a `schedule` Trigger whose text
   ends with "Why now: ..." and the two commit ids. Nothing is written to GitHub and no Ticket starts. The Source's
   row in health reads `schedule`; it is there only while an entry is enabled.
3. "See its runs" beside the entry opens Routing with `schedule:branch-check`.
4. Switch the mode to `auto` (`athena policy show > p.json`, edit `"mode"`, `athena policy set p.json --state-dir $S`).
   Push another commit. Within about 5 minutes a Ticket appears (`athena status`) on branch `athena/<ticket>`. Push
   again while it is open: nothing new starts, because an entry runs one at a time.
5. Pause and resume: `athena pause --state-dir $S`, push a commit, and wait 5 minutes. `athena decisions` shows no new
   Decision. After `athena resume --state-dir $S`, one Trigger is raised for the newest commit, once the previous
   Ticket has ended.

Clean up: turn the entry off or remove it in Settings > Autopilot, set the mode back to `observe`, and delete the
throwaway repository's branch.

