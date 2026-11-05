# Codex hook fixtures

- `pre-tool-use.command.output.json` and `permission-request.command.output.json` are the JSON schemas of a command
  hook's output that `codex-cli` 0.160.1 embeds (extracted from the binary: each follows its title string). Codex
  checks a hook's stdout against them; `TestCodexPermissionRequestOutputGolden` and `TestCodexDenyHasReason` pin what
  `athena hook --runtime codex` prints to them. Besides the schema, 0.160.1 refuses a `PreToolUse` output with
  `permissionDecision` `allow` or `ask` ("unsupported"), and a `deny` without a non-empty `permissionDecisionReason`.
- `PreToolUse.json` and `PermissionRequest.json` are synthetic hook inputs in the shape of the same binary's
  `pre-tool-use.command.input` and `permission-request.command.input` schemas (placeholder ids, no real paths).
