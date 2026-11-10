// A server of a spec's own, for nodes.spec.ts: nodes join only over TLS (the node listener needs Athena's own CA), and
// the shared server runs with TLS off. It is set up as global-setup.ts sets up the shared one (fakeclaude, fakecodex,
// no repository discovery, a temp state dir under /tmp), plus web TLS "ca", its node listener on a free port, this
// machine's node named "studio" (not the host's name, so screenshots hold on any machine), and ATHENA_TEST=1 with
// ATHENA_TEST_OFFLINE_AFTER=2s (Ruling R5): a dropped node's Tickets show it offline 2 s after the link is dead (9 s).
// A second node is a real `athena node run` in a state dir of its own, joined with the command the dashboard shows.
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { closeSync, existsSync, mkdtempSync, openSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { join } from "node:path";
import { promisify } from "node:util";
import { BIN, ROOT } from "./paths";

const run = promisify(execFile);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.once("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const addr = srv.address();
      srv.close(() => (typeof addr === "object" && addr ? resolve(addr.port) : reject(new Error("no port"))));
    });
  });
}

export async function waitUntil(what: string, ok: () => Promise<boolean> | boolean, ms = 30_000): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await Promise.resolve(ok()).catch(() => false)) return;
    await sleep(200);
  }
  throw new Error(`timed out waiting for ${what}`);
}

/** The fakeclaude and fakecodex variables of global-setup.ts, on this state dir. */
function fakeEnv(state: string): NodeJS.ProcessEnv {
  return {
    ...process.env,
    ATHENA_TEST: "1",
    ATHENA_FAKE_FIXTURE: join(state, "fixture-{ticket}.jsonl"),
    ATHENA_FAKE_LATER_FIXTURE: join(state, "later-{ticket}.jsonl"),
    ATHENA_FAKE_LATER_HOLD: join(state, "hold-{ticket}"),
    ATHENA_FAKE_STDIN_LOG: join(state, "stdin-{ticket}.log"),
    ATHENA_FAKE_ENV_LOG: join(state, "env-{ticket}.log"),
    ATHENA_FAKE_CODEX_FIXTURE: join(state, "codex-fixture-{ticket}.jsonl"),
  };
}

/** Stops a child: SIGTERM, then SIGKILL after 10 s. */
async function stopChild(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise<void>((r) => child.once("exit", () => r()));
  child.kill("SIGTERM");
  if (!(await Promise.race([exited.then(() => true), sleep(10_000).then(() => false)]))) {
    child.kill("SIGKILL");
    await exited;
  }
}

export type TlsServer = {
  url: string;
  state: string;
  /** athena <args> --state-dir <this server's>, its stdout trimmed. */
  athena(...args: string[]): Promise<string>;
  /** The row of `athena node list` for the node name, its fields joined by one space; "" when it is not listed. */
  nodeRow(name: string): Promise<string>;
  stop(): Promise<void>;
};

export async function startTlsServer(): Promise<TlsServer> {
  const state = mkdtempSync("/tmp/athena-e2e-nodes-");
  const [port, nodePort] = [await freePort(), await freePort()];
  writeFileSync(
    join(state, "config.json"),
    JSON.stringify(
      {
        web: { enabled: true, listen: `127.0.0.1:${port}`, nodeListen: `127.0.0.1:${nodePort}`, tls: "ca", host: "localhost" },
        localRepoRoots: [],
        runtimes: { codex: { enabled: true, binary: join(BIN, "fakecodex") } },
      },
      null,
      2,
    ),
    { mode: 0o600 },
  );
  const captured = readFileSync(join(ROOT, "internal/runtime/claude/testdata/claude/simple-turn.jsonl"), "utf8");
  writeFileSync(join(state, "fixture-default.jsonl"), captured.split("\n")[0] + "\n");

  const log = openSync(join(state, "server.log"), "a");
  const server = spawn(
    join(BIN, "athena"),
    ["server", "--state-dir", state, "--claude", join(BIN, "fakeclaude"), "--node-name", "studio"],
    { env: { ...fakeEnv(state), ATHENA_TEST_OFFLINE_AFTER: "2s" }, stdio: ["ignore", log, log] },
  );
  const athena = async (...args: string[]) =>
    (await run(join(BIN, "athena"), [...args, "--state-dir", state], { encoding: "utf8" })).stdout.trim();
  const nodeRow = async (name: string) => {
    for (const line of (await athena("node", "list")).split("\n")) {
      if (line.startsWith(`${name} `)) return line.split(/\s+/).join(" ");
    }
    return "";
  };
  const stop = async () => {
    await stopChild(server);
    closeSync(log);
    if (!process.env.ATHENA_E2E_KEEP) rmSync(state, { recursive: true, force: true });
  };
  try {
    await waitUntil("the TLS server", async () => (await nodeRow("studio")) !== "");
  } catch (e) {
    await stop();
    throw e;
  }
  return { url: `https://localhost:${port}`, state, athena, nodeRow, stop };
}

/** The join command the dashboard shows: athena node join <url> <token> [--ca-sha256 <fp>]. */
const JOIN = /^athena node join (\S+) (\S+)(?: --ca-sha256 (\S+))?$/;

export type RunningNode = {
  dir: string;
  /** SIGSTOP: the process stays but answers nothing, so its link dies as a dropped machine's does. */
  freeze(): void;
  /** SIGKILL, waiting for the exit. */
  kill(): Promise<void>;
  stop(): Promise<void>;
};

/** Joins a node with the dashboard's join command and runs it (fakeclaude, no Codex, 2 slots). */
export async function joinAndRun(server: TlsServer, command: string, name: string): Promise<RunningNode> {
  const m = JOIN.exec(command.trim());
  if (!m) throw new Error(`not a join command: ${command}`);
  const dir = mkdtempSync("/tmp/athena-e2e-node-");
  const join_ = ["node", "join", m[1], m[2], ...(m[3] ? ["--ca-sha256", m[3]] : []), "--name", name, "--state", dir];
  await run(join(BIN, "athena"), join_, { encoding: "utf8" });
  const log = openSync(join(dir, "run.log"), "a");
  const child = spawn(
    join(BIN, "athena"),
    ["node", "run", "--state", dir, "--claude", join(BIN, "fakeclaude"), "--codex", "/nonexistent", "--slots", "2"],
    { env: fakeEnv(server.state), stdio: ["ignore", log, log] },
  );
  const exited = new Promise<void>((r) => child.once("exit", () => r()));
  const cleanup = () => {
    closeSync(log);
    if (!process.env.ATHENA_E2E_KEEP) rmSync(dir, { recursive: true, force: true });
  };
  // It serves sessions once it has the server's prompts: its socket says so.
  await waitUntil(`${name} online`, async () => (await server.nodeRow(name)).includes(" online "));
  await waitUntil(`${name}'s socket`, () => existsSync(join(dir, "node.sock")));
  return {
    dir,
    freeze: () => child.kill("SIGSTOP"),
    kill: async () => {
      if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
      await exited;
    },
    stop: async () => {
      if (child.signalCode === "SIGKILL") {
        await exited;
      } else {
        child.kill("SIGCONT");
        await stopChild(child);
      }
      cleanup();
    },
  };
}
