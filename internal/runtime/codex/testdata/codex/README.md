# Codex app-server fixtures

**Status: synthetic.** Every `.jsonl` file here was written by hand from the JSON schema of `codex-cli` 0.160.1
(`codex app-server generate-json-schema`), not captured from a real session, until the owner runs
`ATHENA_CAPTURE_CONSENT=spend-codex-quota scripts/capture-codex-fixtures.sh turns` (it spends a little Codex quota; see the
script) and copies the captured files over these. The ids, the timestamps (counted from 2030-01-01T00:00:00Z) and the
paths (`/home/user/work`) are placeholders: `thr_fixture_0001`, `turn_fixture_0001`, `item_000N`, `acct_fixture`. A test
checks that no fixture holds an `@`, an `sk-`, a home path or any other id.

Each line is what the server prints on stdout: a notification (`{"method","params"}`), with no `jsonrpc` field. A line
with a `fake` key is a directive for `fakecodex` and is never sent to the client (see its doc comment):
`{"fake":"request",...}` sends a server request, `{"fake":"hook",...}` runs the configured hook.

- `simple-turn.jsonl`: one message, one agent answer (with a text delta), token usage, `turn/completed`.
- `tool-turn.jsonl`: reasoning, two `commandExecution` items (one exits 0, one fails), a `fileChange`, an `mcpToolCall`,
  a `webSearch`, the answer, token usage, rate limits, `turn/completed`. `TestTranslateToolTurn` pins its events.
- `steer-turn.jsonl`: a turn that follows or joins another: the steered user message, an answer, `turn/completed`.
  `fakecodex` replays it on every later `turn/start`, and on `turn/steer` without its `turn/started` and `turn/completed`.
- `usage-limit-turn.jsonl`: the 5h window is used up (the 7d one is at 63%), an `error` with `willRetry: false`, and a
  `turn/completed` `failed` with `codexErrorInfo: "usageLimitExceeded"`.
- `approval-turn.jsonl`: a command that asks for approval (a `fake: request` line standing for the server request) and
  then completes.
- `rate-limits.jsonl`: three `account/rateLimits/updated` notifications: normal usage, a limit reached, a 45-minute window.
- `config.golden.toml`: the `config.toml` `WriteHome` writes for `TestWriteHomeExactKeys` (not a server line). The real
  Codex 0.160.1 loads it under `--strict-config` and lists and trusts its hooks (`TestRealHooksListTrusted`).
- `protocol.json`: written by `scripts/capture-codex-fixtures.sh schema` (`testdata/protoextract`), the required params of
  the methods Athena uses; `TestFixturesUseKnownMethodsAndRequiredFields` checks every fixture line against it.

## What is assumed, not verified

- The `item/started` and `item/completed` item shapes follow the schema; whether a real `webSearch` or `mcpToolCall`
  completes without a start, and what `aggregatedOutput` holds for a failed command, comes from the first real capture.
- `thread/started` follows `thread/start` (`fakecodex` sends it); a real `thread/resume` may not send it.
- A hook's output that denies a tool (exit 2, or `permissionDecision: "deny"`) is the shape Claude Code uses; Codex's
  exact rules are Task 4's to confirm (`hook-inputs.jsonl`, once captured, shows the inputs).
