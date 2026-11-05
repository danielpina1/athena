# Codex

Athena can run its workers on OpenAI's Codex CLI as well as on Claude Code. This page is being written along with the
Codex runtime; the sections marked *to be written* are not there yet.

## 1. What Athena runs

Athena drives `codex app-server --listen stdio:// --strict-config`, one process per session, never the shared daemon,
`remote-control` or `--remote`. It runs Codex 0.160.0 or newer; 0.160.1 is the version it was verified against. A newer
version runs too, once its hooks check passes, and `athena codex status` calls it untested.

## 2. Install and log in

Install the Codex CLI (`codex --version` must print `codex-cli 0.160.0` or newer), then log it in for Athena:

```sh
athena codex login                 # signs in with ChatGPT in the browser
athena codex login --with-api-key  # or reads an OpenAI API key from stdin
athena codex status
```

This is a separate login from the one your own `codex` uses. Athena's Codex lives in its own `CODEX_HOME`,
`<stateDir>/codex` (`~/.athena/codex` by default, mode 0700): its configuration, its credential (`auth.json`, mode 0600,
written by `codex login` itself) and its sessions. Athena never reads or writes `~/.codex`, so your own Codex settings,
hooks and plugins never reach a worker, and a worker's never reach yours.

`athena codex status` prints what the check found: whether Codex is installed, its version and whether it is tested,
whether it is logged in (the kind of account only, never its id or email), Athena's hooks, and whether it is ready.

## 3. `config.json` `runtimes`

```json
{"runtimes": {"codex": {"enabled": true, "binary": "codex", "model": ""}}}
```

- `enabled`: on unless set to `false`. An enabled Codex that is not ready is never chosen, and the dashboard says why.
- `binary`: the codex executable; `athena server --codex PATH` overrides it.
- `model`: the default Codex model; empty leaves it to Codex.

*More settings are to be written.*

## 4. Hooks and trust

Athena writes `<stateDir>/codex/config.toml` itself, every time it checks Codex; do not edit it. It holds Athena's
settings: approvals asked of Athena (`approval_policy = "on-request"`, `approvals_reviewer = "user"`), the
`workspace-write` sandbox, no update check, no analytics, the features that act outside the worktree or spend quota
unasked turned off (apps, plugins, browser and computer use, multi-agent, image generation, memories, realtime), Athena's
MCP server, and Athena's hooks: `athena hook --runtime codex <Event>` for `PreToolUse`, `PermissionRequest`,
`PostToolUse`, `Stop` and `SessionStart`.

**Deviation from the design (§6.1):** the hooks are in `config.toml`, not in a separate `hooks.json`. Codex 0.160 loads
`[[hooks.<Event>]]` tables from `config.toml`; one file is written in one step, and the trust check below sees the hooks
where Codex loaded them from. What the design asks for holds: the hooks live in Athena's own `CODEX_HOME`, never in your
global configuration or the repository.

Codex runs a hook from a configuration file only once it is trusted. Athena trusts only the hooks it wrote: it asks
Codex for the hooks it would run (`hooks/list`), records the trust of its own (`hooks.state."<key>".trusted_hash`,
written by Codex at the end of `config.toml`), and lists them again. Codex is ready only when Athena's five hooks are
there, enabled and trusted, and **no other hook is enabled**, trusted or not, from whatever layer (a repository's
`.codex/`, a plugin, a managed configuration). Otherwise Athena refuses to start Codex and says why: there is no silent
fallback to a session its hooks do not control.

## 5. Sandbox

*To be written.* Found while building the runtime, with Codex 0.160.1's `codex sandbox`: Codex keeps a workspace's own
git directory read-only even inside a writable root, so a Ticket's worktree can commit only when the worktree's git
directory (`<common>/worktrees/<name>`) and the common directory's `objects`, `refs` and `logs` are writable roots;
the common directory as a root is not enough. Those roots leave the common directory's `config` and `hooks` read-only.

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
| `codex app-server initialize: … unknown configuration field …` | This Codex does not know a key Athena writes: use a tested version. |
