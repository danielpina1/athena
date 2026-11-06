// The e2e harness every spec imports instead of @playwright/test:
//
//   - test: Playwright's, with baseURL set to the shared server, a `session` fixture (below), and a page that fails
//     the test when the browser reports a Content Security Policy violation.
//   - signedInPage(page): a CDP virtual authenticator (ctap2, internal, resident key, user verification), then the
//     real enrollment (a fresh `athena web enroll` link, after revoking the passkeys of earlier tests; the running
//     test's own are kept, so it may sign in more than once) and sign-in with that passkey. Both screens are checked
//     with axe on the way.
//   - simulateTouch(page, on): hold back or give the virtual authenticator's touch after signedInPage (to rest on sign-in).
//   - addAuthenticator(page): that virtual authenticator alone, for a second device that enrolls through a link.
//   - athena(...args): the CLI against the shared state dir, e.g. athena("task", "Deploy it") → its stdout.
//   - expectAccessible(page): axe on the current screen; any serious or critical violation fails.
//   - session.requestApproval(ticket, tool, input) and session.report(ticket, state, text, question?): act as the
//     Ticket's worker session through sessionctl (internal/testutil/sessionctl), on the real hook and MCP paths.
//   - laterFixture(ticket, lines) and stdinLog(ticket): what the Ticket's fakeclaude prints after each later message
//     (a reply), and what it has read on stdin; holdLaterResult(ticket) holds that fixture's result line back.
//   - codexFixture(ticket, lines): the app-server lines the Ticket's fakecodex replays on its session's first turn.
//
// Every Ticket's session runs fakeclaude replaying the default fixture (global-setup.ts): a turn that never ends.
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { existsSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";
import AxeBuilder from "@axe-core/playwright";
import { test as base, expect, type Page } from "@playwright/test";
import { BIN, ENROLL_LINK } from "./paths";

export { expect };

const run = promisify(execFile);

function env(name: "ATHENA_E2E_URL" | "ATHENA_E2E_STATE"): string {
  const v = process.env[name];
  if (!v) throw new Error(`${name} is not set: run the specs through playwright.config.ts (make e2e)`);
  return v;
}

/** Runs `athena <args> --state-dir <shared>` and returns its stdout, trimmed. Fails on a non-zero exit. */
export async function athena(...args: string[]): Promise<string> {
  const { stdout } = await run(join(BIN, "athena"), [...args, "--state-dir", env("ATHENA_E2E_STATE")], {
    encoding: "utf8",
  });
  return stdout.trim();
}

/** A fresh single-use enrollment link. */
export async function enrollLink(): Promise<string> {
  const link = ENROLL_LINK.exec(await athena("web", "enroll"))?.[0];
  if (!link) throw new Error("athena web enroll printed no link");
  return link;
}

/**
 * Gives the Ticket's fakeclaude stream-json lines to print after each message it reads after the first (a reply from
 * the dashboard, say). Write it before that message is sent.
 */
export function laterFixture(ticket: string, lines: object[]): void {
  writeFileSync(join(env("ATHENA_E2E_STATE"), `later-${ticket}.jsonl`), lines.map((l) => JSON.stringify(l) + "\n").join(""));
}

/**
 * Gives the Ticket's fakecodex the app-server lines (and fakecodex directives) to replay on its session's first turn,
 * returning a function that removes them. Write it before the session starts: ticket "default" serves every Codex
 * session that has no fixture of its own, for a Ticket whose id is not known before it starts.
 */
export function codexFixture(ticket: string, lines: object[]): () => void {
  const file = join(env("ATHENA_E2E_STATE"), `codex-fixture-${ticket}.jsonl`);
  writeFileSync(file, lines.map((l) => JSON.stringify(l) + "\n").join(""));
  return () => rmSync(file, { force: true });
}

/**
 * Holds back the result line of the Ticket's later fixture until the returned release is called: the turn stays open,
 * so a spec can file a real Report in it (a fixture's report tool call files none, and a turn that ends without one
 * gets a nudge, which replays the later fixture again).
 */
export function holdLaterResult(ticket: string): () => void {
  const file = join(env("ATHENA_E2E_STATE"), `hold-${ticket}`);
  writeFileSync(file, "");
  return () => rmSync(file, { force: true });
}

/** Every line the Ticket's fakeclaude has read on stdin so far ("" before the session started). */
export function stdinLog(ticket: string): string {
  const file = join(env("ATHENA_E2E_STATE"), `stdin-${ticket}.log`);
  return existsSync(file) ? readFileSync(file, "utf8") : "";
}

/** Fails on any serious or critical axe violation on the current screen. */
export async function expectAccessible(page: Page): Promise<void> {
  // A dialog still fading in would be measured half transparent: let finite animations end first.
  await page.evaluate(() =>
    Promise.all(
      document
        .getAnimations()
        .filter((a) => a.effect?.getTiming().iterations !== Infinity)
        .map((a) => a.finished.catch(() => {})),
    ),
  );
  const { violations } = await new AxeBuilder({ page }).analyze();
  const bad = violations
    .filter((v) => v.impact === "serious" || v.impact === "critical")
    .map((v) => {
      const where = v.nodes.map((n) => n.target.join(" ")).join(", ");
      return `${v.impact} ${v.id}: ${v.help} — ${where}\n  ${v.nodes[0]?.failureSummary ?? ""}`;
    });
  expect(bad, `axe on ${page.url()}`).toEqual([]);
}

/**
 * What a screenshot must not pin: the live feed, whose rows (the events since the page opened, with their Tickets'
 * ids) and counts depend on what ran before. Every screenshot that shows the feed masks the whole section.
 */
export function liveFeed(page: Page) {
  return [page.locator("[data-live-feed]")];
}

/** The shell is up: the main navigation shows and the link is open. */
export async function expectShell(page: Page): Promise<void> {
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
  await expect(page.locator('[data-mark][data-link="open"]')).toBeAttached();
}

/** The passkeys the running test enrolled: kept, so a test can sign in more than once (two screens, say). */
const ownPasskeys = new Set<string>();

async function passkeyIds(): Promise<string[]> {
  return (await athena("web", "passkeys"))
    .split("\n")
    .slice(1)
    .map((line) => line.split(/\s+/)[0])
    .filter((id) => /^[A-Za-z0-9_-]{16,}$/.test(id));
}

/**
 * Revokes the passkeys earlier tests enrolled, keeping the running test's own. The server lists every passkey in a
 * new one's excludeCredentials, and Chromium refuses a creation once that list is past 64 (the 66th enrollment on
 * one server failed), while every signedInPage enrolls one. The specs run one at a time (one worker), so a passkey
 * that is not the running test's belongs to a test that has finished.
 */
async function revokeEarlierPasskeys(): Promise<void> {
  for (const id of await passkeyIds()) {
    if (!ownPasskeys.has(id)) await athena("web", "revoke", id);
  }
}

/** A CDP virtual authenticator on page's browser (ctap2, internal, resident key, user verified, presence simulated). */
export async function addAuthenticator(page: Page) {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2",
      transport: "internal",
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  return { cdp, authenticatorId };
}

const authenticators = new WeakMap<Page, { cdp: Awaited<ReturnType<typeof addAuthenticator>>["cdp"]; authenticatorId: string }>();

/**
 * Holds back (false) or gives (true) the owner's touch on the page's virtual authenticator, so the sign-in screen
 * shows until the test lets it sign in: with presence simulated, its passkey autofill signs in on its own.
 */
export async function simulateTouch(page: Page, enabled: boolean): Promise<void> {
  const a = authenticators.get(page);
  if (!a) throw new Error("simulateTouch: the page has no virtual authenticator (call signedInPage first)");
  await a.cdp.send("WebAuthn.setAutomaticPresenceSimulation", { authenticatorId: a.authenticatorId, enabled });
}

/**
 * Enrolls a new passkey on a virtual authenticator and signs in with it: page ends on the signed-in shell at "/".
 * Each call enrolls its own passkey; the links are single-use.
 */
export async function signedInPage(page: Page): Promise<Page> {
  await revokeEarlierPasskeys();
  const before = new Set(await passkeyIds());
  const { cdp, authenticatorId } = await addAuthenticator(page);
  authenticators.set(page, { cdp, authenticatorId });

  await page.goto(await enrollLink());
  const create = page.getByRole("button", { name: "Create passkey" });
  await expect(create).toBeVisible();
  await expectAccessible(page);
  await create.click();
  await expect(page).toHaveURL(/\/$/);
  for (const id of await passkeyIds()) if (!before.has(id)) ownPasskeys.add(id);

  // Sign in with the new passkey: start from no session, as a returning owner does. With presence simulated the
  // authenticator would answer the sign-in page's passkey autofill on its own, so the owner's touch is held back
  // until the screen has been checked, and the sign-in then completes through either path (autofill or button).
  await cdp.send("WebAuthn.setAutomaticPresenceSimulation", { authenticatorId, enabled: false });
  await page.context().clearCookies();
  await page.goto("/");
  const signIn = page.getByRole("button", { name: "Sign in with passkey" });
  await expect(signIn).toBeVisible();
  await expectAccessible(page);
  await cdp.send("WebAuthn.setAutomaticPresenceSimulation", { authenticatorId, enabled: true });
  await signIn.click({ timeout: 2_000 }).catch(() => {
    // The autofill request finished first and the button is gone: signed in all the same.
  });
  await expectShell(page);
  return page;
}

/** The hook's answer to a tool call: allow or deny, with the reason the session sees. */
export type HookVerdict = { decision: string; reason: string };

export type Session = {
  /**
   * Runs the Ticket's PreToolUse hook for a tool call (`athena hook`), as its session would. A call the rules
   * escalate raises an Approval; the promise settles with the verdict once the owner answers.
   */
  requestApproval(ticket: string, tool: string, input: Record<string, unknown>): Promise<HookVerdict>;
  /** Files a Report on the Ticket through its MCP server's report tool, as its session would. */
  report(ticket: string, state: string, text: string, question?: string): Promise<string>;
};

/** <state>/env-<ticket>.log, once the Ticket's session has started. */
async function sessionEnv(ticket: string): Promise<string> {
  const file = join(env("ATHENA_E2E_STATE"), `env-${ticket}.log`);
  const deadline = Date.now() + 20_000;
  while (!existsSync(file)) {
    if (Date.now() > deadline) throw new Error(`the session of ${ticket} did not start`);
    await new Promise((r) => setTimeout(r, 100));
  }
  return file;
}

function sessionctl(args: string[], children: Set<ChildProcess>): Promise<string> {
  return new Promise((resolve, reject) => {
    const child = spawn(join(BIN, "sessionctl"), [...args, "-athena", join(BIN, "athena")], {
      stdio: ["ignore", "pipe", "pipe"],
    });
    children.add(child);
    let out = "";
    let err = "";
    child.stdout.on("data", (d: Buffer) => (out += d.toString()));
    child.stderr.on("data", (d: Buffer) => (err += d.toString()));
    child.on("error", reject);
    child.on("exit", (code) => {
      children.delete(child);
      if (code === 0) resolve(out.trim());
      else reject(new Error(`sessionctl ${args[0]} exited ${code}: ${err.trim()}`));
    });
  });
}

export const test = base.extend<{ session: Session; ownPasskeysReset: void }>({
  baseURL: async ({}, use) => {
    await use(env("ATHENA_E2E_URL"));
  },
  // Each test starts owning no passkey: the next signedInPage revokes the previous test's.
  ownPasskeysReset: [
    async ({}, use) => {
      ownPasskeys.clear();
      await use();
    },
    { auto: true },
  ],
  page: async ({ page }, use) => {
    const violations: string[] = [];
    page.on("console", (m) => {
      if (m.type() === "error" && /Content Security Policy/i.test(m.text())) violations.push(m.text());
    });
    await use(page);
    expect(violations, "CSP violations").toEqual([]);
  },
  session: async ({}, use) => {
    const children = new Set<ChildProcess>();
    await use({
      requestApproval(ticket, tool, input) {
        const verdict = (async () => {
          const file = await sessionEnv(ticket);
          const args = ["approval", "-env", file, "-tool", tool, "-input", JSON.stringify(input)];
          const hso = JSON.parse(await sessionctl(args, children)).hookSpecificOutput ?? {};
          return { decision: hso.permissionDecision ?? "", reason: hso.permissionDecisionReason ?? "" };
        })();
        // A test that failed before the answer never awaits it; the kill at teardown must not be an unhandled
        // rejection. Callers that await still see the error.
        verdict.catch(() => {});
        return verdict;
      },
      async report(ticket, state, text, question) {
        const file = await sessionEnv(ticket);
        const args = ["report", "-env", file, "-status", state, "-summary", text];
        if (question) args.push("-question", question);
        return sessionctl(args, children);
      },
    });
    // A hook still waiting for an answer when the test ends is not left behind.
    for (const c of children) c.kill();
  },
});
