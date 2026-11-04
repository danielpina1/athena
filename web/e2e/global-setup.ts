// Builds the dashboard and the binaries (into bin/e2e, never bin/athena), then runs one `athena server` for every spec: a temp state dir (under /tmp,
// so the Unix socket paths stay short) whose config.json turns the dashboard on at http://localhost:<free port> with
// TLS off and no GitHub source, and fakeclaude as the runtime (`athena server --claude`). fakeclaude replays
// <state>/fixture-<ticket>.jsonl, or fixture-default.jsonl (a turn that never ends, so every Ticket's session stays
// live) when a spec wrote none; after each later message it replays <state>/later-<ticket>.jsonl if a spec wrote one
// (R37: each spec gives its own Tickets their fixtures, see harness.ts). It logs each session's environment to
// <state>/env-<ticket>.log (ATHENA_FAKE_ENV_LOG) for sessionctl and its stdin to <state>/stdin-<ticket>.log. The
// harness reads the location from ATHENA_E2E_* variables. Set ATHENA_E2E_KEEP=1 to keep the state dir (with server.log) afterwards.
import { execFileSync, spawn } from "node:child_process";
import { closeSync, mkdtempSync, openSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { join } from "node:path";
import { ENROLL_LINK, ROOT, BIN } from "./paths";

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

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function waitUntil(what: string, ok: () => Promise<boolean>, ms = 30_000) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await ok().catch(() => false)) return;
    await sleep(100);
  }
  throw new Error(`e2e setup: timed out waiting for ${what}`);
}

export default async function globalSetup() {
  const go = (out: string, pkg: string) =>
    execFileSync("go", ["build", "-o", join(BIN, out), pkg], { cwd: ROOT, stdio: "inherit" });
  // The server embeds the dashboard, so it is built first.
  execFileSync("npm", ["run", "build"], { cwd: join(ROOT, "web"), stdio: "inherit" });
  // The build empties dist, .gitkeep included (as `make web` does, put it back).
  closeSync(openSync(join(ROOT, "internal/web/ui/dist/.gitkeep"), "a"));
  go("athena", "./cmd/athena");
  go("fakeclaude", "./internal/runtime/claude/testdata/fakeclaude");
  go("sessionctl", "./internal/testutil/sessionctl");

  const state = mkdtempSync("/tmp/athena-e2e-");
  const port = await freePort();
  const url = `http://localhost:${port}`;
  writeFileSync(
    join(state, "config.json"),
    // No local repository discovery: the picker lists only what the specs create, not this machine's repositories.
    JSON.stringify(
      { web: { enabled: true, listen: `127.0.0.1:${port}`, tls: "off", host: "localhost" }, localRepoRoots: [] },
      null,
      2,
    ),
    { mode: 0o600 },
  );
  // A turn that never ends: the init line of a captured one, and no result.
  const captured = readFileSync(join(ROOT, "internal/runtime/claude/testdata/claude/simple-turn.jsonl"), "utf8");
  writeFileSync(join(state, "fixture-default.jsonl"), captured.split("\n")[0] + "\n");

  const log = openSync(join(state, "server.log"), "a");
  const server = spawn(join(BIN, "athena"), ["server", "--state-dir", state, "--claude", join(BIN, "fakeclaude")], {
    env: {
      ...process.env,
      ATHENA_FAKE_FIXTURE: join(state, "fixture-{ticket}.jsonl"),
      ATHENA_FAKE_LATER_FIXTURE: join(state, "later-{ticket}.jsonl"),
      ATHENA_FAKE_STDIN_LOG: join(state, "stdin-{ticket}.log"),
      ATHENA_FAKE_ENV_LOG: join(state, "env-{ticket}.log"),
    },
    stdio: ["ignore", log, log],
  });
  const exited = new Promise<void>((resolve) => server.once("exit", () => resolve()));
  /** Stops the server: SIGTERM, then SIGKILL if it has not exited in 10 s; resolves once it has exited. */
  const stopServer = async () => {
    if (server.exitCode === null && server.signalCode === null) {
      server.kill("SIGTERM");
      const stopped = await Promise.race([exited.then(() => true), sleep(10_000).then(() => false)]);
      if (!stopped) {
        server.kill("SIGKILL");
        await exited;
      }
    }
    closeSync(log);
    if (!process.env.ATHENA_E2E_KEEP) rmSync(state, { recursive: true, force: true });
  };

  try {
    await waitUntil("the dashboard", async () => (await fetch(`${url}/auth/session`)).ok);
    // The enrollment path works and its link has the shape the harness parses.
    const out = execFileSync(join(BIN, "athena"), ["web", "enroll", "--state-dir", state], { encoding: "utf8" });
    if (!ENROLL_LINK.test(out)) throw new Error("e2e setup: athena web enroll printed no enroll link");
  } catch (e) {
    // No teardown runs after a failed setup: the server must not outlive it.
    await stopServer();
    throw e;
  }

  process.env.ATHENA_E2E_URL = url;
  process.env.ATHENA_E2E_STATE = state;

  return stopServer;
}
