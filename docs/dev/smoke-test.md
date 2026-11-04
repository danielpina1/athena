# Smoke test with real Claude Code

Plan 1 is tested with a fake `claude` in CI. This page shows how to check it against the real, logged-in
Claude Code (2.1.x) on your machine. It costs a few cents (the model is `haiku`) and needs the network.

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
