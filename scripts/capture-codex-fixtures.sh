#!/usr/bin/env bash
# Codex fixtures for internal/runtime/codex/testdata/codex/.
#
#   scripts/capture-codex-fixtures.sh schema
#       Regenerates protocol.json (the required params of the app-server methods Athena uses) from the schema the
#       installed `codex` writes with `codex app-server generate-json-schema`. Free: it sends no prompt, and it runs
#       with a throwaway CODEX_HOME.
#
#   ATHENA_CAPTURE_CONSENT=spend-codex-quota scripts/capture-codex-fixtures.sh turns
#       Runs ONE short turn on the real Codex and saves what the app-server printed, sanitized, into
#       testdata/codex/captured/ (tool-turn.jsonl, rate-limits.jsonl, hook-inputs.jsonl). It spends a little of the
#       owner's Codex quota, so it refuses without the consent variable, and no make target runs it. Compare the result
#       with the synthetic fixtures, then copy over the ones you want (TestTranslateToolTurn expects the exact events
#       of the synthetic tool-turn.jsonl, so update it with the copy) and edit testdata/codex/README.md.
#
# The turn runs in a temporary git repository against Athena's own CODEX_HOME ($ATHENA_CODEX_HOME, default
# ${ATHENA_STATE_DIR:-$HOME/.athena}/codex), in place: no copy of its credential is made, so a token Codex rotates
# stays where Athena looks for it. The run's thread rollout, history and logs land in that home too (harmless, but
# they stay there). It never touches ~/.codex. Hooks that only log their stdin are passed as -c overrides (the
# schema's shape: [[hooks.<Event>]] matcher groups holding [[hooks.<Event>.hooks]] handlers with type = "command" and
# async = false). Codex runs a -c ("sessionFlags") hook only once trusted, and a -c hooks.<Event> list adds to the
# hooks of config.toml rather than replacing them, so the script starts the app-server twice: once to list the hooks,
# then with a -c hooks.state override that trusts its own logging hooks and disables every other enabled hook (Athena's
# `athena hook` ones among them). It refuses to run the turn if any other hook is still enabled.
# ATHENA_CODEX_BIN names another codex binary (a test double, say).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/internal/runtime/codex/testdata/codex"
bin="${ATHENA_CODEX_BIN:-codex}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

version() { "$bin" --version | sed -n 's/^codex-cli \([0-9][0-9.]*\).*/\1/p'; }

case "${1:-}" in
schema)
  mkdir -p "$work/home" "$work/schema"
  CODEX_HOME="$work/home" "$bin" app-server generate-json-schema --out "$work/schema" >/dev/null
  v="$(version)"
  [ -n "$v" ] || { echo "cannot read the codex version from '$bin --version'" >&2; exit 1; }
  (cd "$root" && go run ./internal/runtime/codex/testdata/protoextract -version "$v" -o "$out/protocol.json" "$work/schema")
  echo "wrote $out/protocol.json for codex $v"
  ;;
turns)
  if [ "${ATHENA_CAPTURE_CONSENT:-}" != "spend-codex-quota" ]; then
    echo "refusing: this runs a real Codex turn and spends quota. Set ATHENA_CAPTURE_CONSENT=spend-codex-quota to go on." >&2
    exit 1
  fi
  athena_home="${ATHENA_CODEX_HOME:-${ATHENA_STATE_DIR:-$HOME/.athena}/codex}"
  [ -d "$athena_home" ] || { echo "no Athena CODEX_HOME at $athena_home (run \`athena codex login\` first)" >&2; exit 1; }
  mkdir -p "$work/hooks" "$work/repo" "$out/captured"
  git -C "$work/repo" init -q
  git -C "$work/repo" -c user.name=capture -c user.email=capture@example.invalid commit -q --allow-empty -m init
  ATHENA_CAPTURE_REPO="$work/repo" ATHENA_CAPTURE_HOOKS="$work/hooks" CODEX_HOME="$athena_home" \
    python3 -I - "$bin" "$work" "$out/captured" <<'PY'
import json, os, re, subprocess, sys, threading, uuid

binary, work, outdir = sys.argv[1:4]
repo = os.environ["ATHENA_CAPTURE_REPO"]
hooks_dir = os.environ["ATHENA_CAPTURE_HOOKS"]
# ATHENA_FAKE_ variables pass through so the script can be tried against fakecodex (ATHENA_CODEX_BIN).
env = {k: v for k, v in os.environ.items() if k in ("PATH", "HOME", "LANG", "CODEX_HOME") or k.startswith("ATHENA_FAKE_")}
hook_flags = []
for ev in ("SessionStart", "PreToolUse", "PermissionRequest", "PostToolUse", "Stop"):
    cmd = "cat >> %s/%s.jsonl" % (hooks_dir, ev)
    hook_flags += ["-c", "hooks.%s=[{hooks=[{type=\"command\",command=%s,async=false}]}]" % (ev, json.dumps(cmd))]
proc = None
next_id = [0]
captured = []  # notifications and server requests, as printed

def start(flags):
    global proc
    proc = subprocess.Popen([binary, "app-server", "--listen", "stdio://", "--strict-config"] + flags, stdin=subprocess.PIPE,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, cwd=repo, env=env)
    watchdog = threading.Timer(300, proc.kill)
    watchdog.daemon = True  # a refusal (SystemExit) must not wait for it
    watchdog.start()
    return watchdog

def stop(watchdog):
    proc.stdin.close()
    proc.wait(timeout=30)
    watchdog.cancel()

def send(obj):
    proc.stdin.write((json.dumps(obj) + "\n").encode())
    proc.stdin.flush()

def read():
    line = proc.stdout.readline()
    if not line:
        raise SystemExit("the app-server ended early")
    return json.loads(line)

def answer(msg):
    # Approvals are declined: this captures a turn, it does not let Codex change anything outside its sandbox.
    # item/permissions/requestApproval answers with permissions, not a decision, so it gets an error.
    if msg["method"] in ("item/commandExecution/requestApproval", "item/fileChange/requestApproval"):
        send({"id": msg["id"], "result": {"decision": "decline"}})
    else:
        send({"id": msg["id"], "error": {"code": -32601, "message": "not handled by the capture"}})

def call(method, params):
    next_id[0] += 1
    rid = next_id[0]
    send({"id": rid, "method": method, "params": params})
    while True:
        msg = read()
        if "method" in msg:
            captured.append(msg)
            if "id" in msg:
                answer(msg)
        elif msg.get("id") == rid:
            if "error" in msg:
                raise SystemExit("%s: %s" % (method, msg["error"]))
            return msg["result"]

def list_hooks():
    return call("hooks/list", {"cwds": [repo]})["data"][0]["hooks"]

# First run: what hooks would run, and their keys and hashes.
watchdog = start(hook_flags)
call("initialize", {"clientInfo": {"name": "athena-capture", "version": "0"}})
send({"method": "initialized"})
state = []
for h in list_hooks():
    if h["source"] == "sessionFlags":
        state.append("%s={trusted_hash=%s}" % (json.dumps(h["key"]), json.dumps(h["currentHash"])))
    elif h["enabled"]:
        state.append("%s={enabled=false}" % json.dumps(h["key"]))
stop(watchdog)
captured.clear()

# Second run: the logging hooks trusted, every other hook off.
flags = hook_flags + (["-c", "hooks.state={%s}" % ",".join(state)] if state else [])
watchdog = start(flags)
call("initialize", {"clientInfo": {"name": "athena-capture", "version": "0"}})
send({"method": "initialized"})
hooks = list_hooks()
others = [h["key"] for h in hooks if h["enabled"] and h["source"] != "sessionFlags"]
if others:
    raise SystemExit("refusing: hooks besides the capture's are still enabled: %s" % others)
logging = [h for h in hooks if h["source"] == "sessionFlags" and h["enabled"] and h["trustStatus"] == "trusted"]
if len(logging) != 5:
    print("warning: %d of the 5 logging hooks are trusted; hook-inputs.jsonl will miss the others" % len(logging), file=sys.stderr)
captured.clear()
limits = call("account/rateLimits/read", {})
thread = call("thread/start", {"cwd": repo, "approvalPolicy": "on-request", "sandbox": "workspace-write"})["thread"]["id"]
prompt = "Create a file hi.txt containing hi, then say done."
client_id = str(uuid.uuid4())
call("turn/start", {"threadId": thread, "clientUserMessageId": client_id,
                    "input": [{"type": "text", "text": prompt, "text_elements": []}]})
while True:
    msg = read()
    if "method" in msg:
        captured.append(msg)
        if "id" in msg:
            answer(msg)
        if msg["method"] == "turn/completed":
            break
stop(watchdog)

hook_lines = []
for name in sorted(os.listdir(hooks_dir)):
    with open(os.path.join(hooks_dir, name)) as f:
        for l in f:
            if l.strip():
                hook_lines.append(json.dumps({"hook": name[:-len(".jsonl")], "input": json.loads(l)}))

# Sanitize: placeholder ids, no paths of this machine, no account data.
item_ids = []
for m in captured:
    item = (m.get("params") or {}).get("item") or {}
    if item.get("id") and item["id"] not in item_ids:
        item_ids.append(item["id"])
turn_ids = {t for m in captured for t in [((m.get("params") or {}).get("turn") or {}).get("id"), (m.get("params") or {}).get("turnId")] if t}
subs = [(thread, "thr_fixture_0001"), (client_id, "c0ffee00-0000-4000-8000-000000000002")]
subs += [(t, "turn_fixture_0001") for t in sorted(turn_ids)]
subs += [(i, "item_%04d" % (n + 1)) for n, i in enumerate(item_ids)]
if limits.get("accountId"):
    subs.append((limits["accountId"], "acct_fixture"))
for p, to in sorted({(os.path.realpath(repo), "/home/user/work"), (repo, "/home/user/work"),
                     (os.path.realpath(work), "/home/user/tmp"), (work, "/home/user/tmp"),
                     (os.path.realpath(os.environ["HOME"]), "/home/user"), (os.environ["HOME"], "/home/user")},
                    key=lambda x: -len(x[0])):
    subs.append((p, to))

def sanitize(s):
    for a, b in subs:
        s = s.replace(a, b)
    s = re.sub(r'"(sessionId)":"[^"]*"', r'"\1":"thr_fixture_0001"', s)
    s = re.sub(r'"(accountId)":"[^"]*"', r'"\1":"acct_fixture"', s)
    s = re.sub(r'[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+', 'redacted', s)
    return re.sub(r'sk-[A-Za-z0-9_-]+', 'redacted', s)

def write(name, lines):
    with open(os.path.join(outdir, name), "w") as f:
        for l in lines:
            f.write(sanitize(l) + "\n")

write("tool-turn.jsonl", [json.dumps(m, separators=(",", ":")) for m in captured])
write("rate-limits.jsonl", [json.dumps({"method": "account/rateLimits/updated", "params": {"rateLimits": limits["rateLimits"]}}, separators=(",", ":"))])
write("hook-inputs.jsonl", hook_lines)
print("captured %d messages and %d hook inputs into %s" % (len(captured), len(hook_lines), outdir))
PY
  ;;
*)
  echo "usage: $0 schema | turns" >&2
  exit 2
  ;;
esac
