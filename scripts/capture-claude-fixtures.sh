#!/usr/bin/env bash
# Captures real `claude -p --output-format stream-json` transcripts into
# internal/runtime/claude/testdata/claude/. Costs a few cents (haiku, tiny prompts).
# Requires a logged-in `claude`. Paths under $HOME and the temp dir are scrubbed.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/internal/runtime/claude/testdata/claude"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$out" "$work/work"
cd "$work/work"

# Same isolation flags the driver uses: only --settings applies, no inherited MCP.
echo '{}' > "$work/settings.json"
echo '{"mcpServers":{}}' > "$work/mcp.json"
common=(-p --input-format stream-json --output-format stream-json --verbose
  --model haiku --settings "$work/settings.json" --setting-sources ""
  --mcp-config "$work/mcp.json" --strict-mcp-config --permission-mode acceptEdits)

msg() { # msg <text>: one stream-json user message line
  printf '{"type":"user","message":{"role":"user","content":%s}}\n' "$(printf '%s' "$1" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')"
}

scrub() { # scrub <file>
  python3 - "$1" "$work" <<'PY'
import sys, os
path, work = sys.argv[1:3]
home = os.environ["HOME"]
s = open(path).read()
for p in sorted({work, os.path.realpath(work)}, key=len, reverse=True):
    s = s.replace(p + "/work", "/home/user/work").replace(p, "/home/user/tmp")
for h in sorted({os.path.realpath(home), home}, key=len, reverse=True):
    s = s.replace(h, "/home/user")
import re
s = re.sub(r"-private-var-folders-[^/\"]*", "-home-user-work", s)
s = s.replace("/private/tmp/claude-501", "/tmp/claude-1000")
open(path, "w").write(s)
PY
}

echo "== simple-turn"
msg "Create a file hi.txt containing hi, then say done." | claude "${common[@]}" > "$out/simple-turn.jsonl"
sid="$(python3 -c 'import json,sys
for l in open(sys.argv[1]):
    d=json.loads(l)
    if d.get("type")=="system" and d.get("subtype")=="init": print(d["session_id"]); break' "$out/simple-turn.jsonl")"

echo "== resume-turn ($sid)"
msg "What was the name of the file you created? Reply with the file name only." | claude "${common[@]}" --resume "$sid" > "$out/resume-turn.jsonl"

echo "== mid-turn-input"
{ msg "Create a file a.txt containing a, then say done."; sleep 2; msg "Also create b.txt containing b."; sleep 25; } \
  | claude "${common[@]}" > "$out/mid-turn-input.jsonl" || true

for f in simple-turn resume-turn mid-turn-input; do scrub "$out/$f.jsonl"; done
wc -l "$out"/*.jsonl
