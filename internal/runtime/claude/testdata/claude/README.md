# Claude Code stream-json fixtures

Captured from the real CLI (`claude` 2.1.280, `--model haiku`) by
`scripts/capture-claude-fixtures.sh`; paths are scrubbed (`$HOME` and the temp
dir become `/home/user`, `/home/user/work`). Re-run the script to refresh.

- `simple-turn.jsonl`: one user message, a `Write` tool use, tool result, final text, `result`.
- `resume-turn.jsonl`: same session continued with `--resume <session_id>`; the model answers from the resumed history.
- `two-turns.jsonl` and `resumed-turn.jsonl`: trimmed (init + `result` lines only) from a live check with `claude` 2.1.280: two turns in one process, then one turn of a `--resume`d process of the same session. `usage` and `duration_ms` are per turn, but `total_cost_usd` is cumulative for the whole session and the resumed process carries the earlier cost on (0.0179, 0.0230, then 0.0269 in the resumed process).
- `mid-turn-input.jsonl`: a second user message written 2 s after the first, while the first turn is running. The CLI folds it into the running turn: one `result` (`num_turns` 3) covers both messages, both files are written.

## stdin message shape (verified)

One JSON object per line; `content` may be a plain string:

    {"type":"user","message":{"role":"user","content":"<text>"}}

Closing stdin lets the CLI finish the queued turn and exit 0. With stdin held
open the process stays alive after each `result` and waits for the next message.

Interrupting the current turn without ending the session (verified, answered by
a `control_response` and the turn ends with `result` subtype
`error_during_execution`; the next user message starts a fresh turn):

    {"type":"control_request","request_id":"r1","request":{"subtype":"interrupt"}}

SIGINT, by contrast, ends the whole process (it prints an `error_during_execution`
result and exits 0). `Session.Interrupt` sends SIGINT as specified; use
`Session.InterruptTurn` for the control request.

## stdout events

Each line is a JSON object with `type`:

- `system` / `subtype: init`: `session_id`, `model`, `cwd`, `tools[]`, `permissionMode`, ...
  Other `system` subtypes seen: `hook_started`, `hook_response`, `thinking_tokens`, `task_summary`, `post_turn_summary`.
- `assistant`: `message.content[]` blocks, one block per line in practice: `thinking`, `text{text}`, `tool_use{id,name,input}`.
- `user`: `message.content[]` with `tool_result{tool_use_id,content,is_error?}` (plus `tool_use_result` metadata).
- `rate_limit_event`.
- `result`: `subtype` (`success`, `error_during_execution`, ...), `is_error`, `session_id`, `total_cost_usd`,
  `usage{input_tokens,output_tokens,cache_read_input_tokens,...}`, `num_turns`, `duration_ms`, `result` (final text).

The parser treats every other type as `Unknown` and keeps the raw line.

## Flags verified against `claude --help` (2.1.x)

`-p`, `--input-format stream-json`, `--output-format stream-json`, `--verbose`, `--model`,
`--settings <file>`, `--setting-sources`, `--mcp-config`, `--strict-mcp-config`,
`--permission-mode`, `--resume <id>`, `--append-system-prompt` all exist.

Difference worth knowing: the `--permission-mode` help text lists
`acceptEdits, auto, bypassPermissions, manual, dontAsk, plan` and does not list
`default`, but `default` is accepted (a `-p` run with `--permission-mode default`
works, and hook inputs and the `init` event report `permission_mode: default`).
The driver passes `default` as the task specifies.

## Settings isolation (R18, verified by experiment)

`--setting-sources` takes a comma-separated list of `user,project,local`. The
driver passes `--setting-sources ""` so none of them load; only `--settings` (and
enterprise-managed settings, which cannot be switched off) apply.

Experiment (project dir with `.claude/settings.json` defining `SessionStart` and
`UserPromptSubmit` hooks that append to a file, `.claude/settings.local.json`
defining a `PreToolUse` hook and `permissions.defaultMode: bypassPermissions`,
plus an Athena `--settings` file with its own `UserPromptSubmit` hook):

| flags | hooks that fired | session `permissionMode` |
| --- | --- | --- |
| `--settings` only | project SessionStart, project UserPromptSubmit, Athena, local PreToolUse | default (flag wins) |
| `--settings --setting-sources ""` | Athena only | default |
| `--settings --setting-sources user` | Athena only (project and local ignored) | - |

With `""` the user's own hooks and plugins also stopped (the `init` event listed
2 plugins instead of 5 and no `hook_started` lines appeared).

The same check is automated in `TestRealClaude` (`real_test.go`), which runs only
with `ATHENA_TEST_REAL_CLAUDE=1`: it generates settings with `WriteSettings`, puts
a project `.claude/settings.json` with bypass mode and hooks in the work dir, runs
a haiku session through `Session`, and asserts that SessionStart, PreToolUse,
PermissionRequest, PostToolUse and Stop all reach the Athena hook command with
`ATHENA_SOCKET` and `ATHENA_TICKET`, that the project hook never fires, that the
PermissionRequest hook's allow lets the `Write` happen, and that the hook input
reports `permission_mode: default`. The MCP server entry from `WriteMCPConfig` was
also started by the CLI with `mcp --ticket <id>` and the `ATHENA_SOCKET` /
`ATHENA_TICKET` env.

`--strict-mcp-config` with the Athena config also drops the user's and claude.ai
MCP servers.
