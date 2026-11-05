# Codex

Athena can run its workers on OpenAI's Codex CLI as well as on Claude Code. This page is being written along with the
Codex runtime; the sections marked *to be written* are not there yet.

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
(commands, network access, file changes, more permissions) go to Athena's Approvals; every other request Codex makes of
Athena (a question for the user, an MCP elicitation, a client-side tool call, a token refresh, an attestation, anything
newer) is answered at once, declined or refused, so a turn never waits on one. The session's transcript keeps what
Codex printed on its own (notifications and requests), never Codex's answers to Athena, which echo the instructions and
name the account.

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
{"runtimes": {"codex": {"enabled": true, "binary": "codex", "model": ""}}}
```

- `enabled`: on unless set to `false`. An enabled Codex that is not ready is never chosen, and the dashboard says why.
- `binary`: the codex executable; `athena server --codex PATH` overrides it.
- `model`: the default Codex model; empty leaves it to Codex.

*More settings are to be written.*

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

## 5. Sandbox

*To be written.* Found while building the runtime, with Codex 0.160.1's `codex sandbox`: Codex keeps a workspace's own
git directory read-only even inside a writable root, so a Ticket's worktree can commit only when the worktree's git
directory (`<common>/worktrees/<name>`) and the common directory's `objects`, `refs` and `logs` are writable roots;
the common directory as a root is not enough. Those roots leave the common directory's `config` and `hooks` read-only.
Caveats:

- This was verified with `codex sandbox -c sandbox_workspace_write.writable_roots=…` only, not with the
  `sandboxPolicy.writableRoots` a session's `turn/start` carries (checking that takes a real turn).
- `<gitDir>/config.worktree` stays writable under those roots, so Athena's PreToolUse rules must protect it.
- git prints `error: Unable to create '…/packed-refs.lock': Operation not permitted` during the commit; it is harmless,
  and the commit succeeds.

A session passes the node's writable roots, as the node computed them, in every `turn/start`'s `sandboxPolicy`
(`workspaceWrite` with `writableRoots`, network off; `readOnly` for a reviewer). The session's working directory is
always writable. The CA variables a sandboxed command needs are in the session process's environment, which its
commands inherit.

## 6. Attribution

*To be written.*

## 7. Quota and failover

*To be written.*

## 8. Review pairing

*To be written.*

## 9. Fixtures

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
