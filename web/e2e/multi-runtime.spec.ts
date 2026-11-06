// One `auto` Ticket paired with review, across both runtimes, on the fakes (never this machine's claude or codex),
// on the desktop and the phone: it starts on Claude Code (fakeclaude), whose turn stops at its usage limit; it
// continues on Codex (fakecodex) and reports ready; the review runs on Claude Code, whose limit has reset by then (the
// e2e node's Claude Code sandbox is required, so Claude Code may review), and asks for changes; the implementer resumes
// on Codex and reports ready again; round 2 approves, and the Ticket ends completed on its "Ready for review" report.
// Throughout, the Ticket never shows a person stage (its stage is polled through the store: `athena status`), every
// step is screenshotted light and dark and checked with axe, and the test fails on any console error or page error.
//
// The screenshots are kept, not compared (the Ticket's id, times and live session differ from run to run): set
// ATHENA_E2E_SHOTS=<dir> to keep them there (multi-runtime-<step>-<light|dark>-<project>.png); otherwise they go with
// the test's output.
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { Page, TestInfo } from "@playwright/test";
import {
  athena,
  codexFixture,
  expect,
  expectAccessible,
  laterFixture,
  sessionAssignment,
  signedInPage,
  statePath,
  test,
  untilNotLimited,
} from "./harness";

const shotsDir = process.env.ATHENA_E2E_SHOTS;
if (shotsDir) mkdirSync(shotsDir, { recursive: true });

/** The step's screenshots, light and dark, each axe clean; then light again. */
async function step(page: Page, testInfo: TestInfo, name: string) {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await page.evaluate(() => document.fonts.ready);
  for (const scheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: scheme, reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", scheme);
    await expectAccessible(page);
    const path = join(shotsDir ?? testInfo.outputDir, `multi-runtime-${name}-${scheme}-${testInfo.project.name}.png`);
    await page.screenshot({ path, fullPage: true });
  }
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
}

/** Polls the Ticket's stage through the store (`athena status --all`) until stopped: every stage it showed, in order. */
function watchStages(ticket: string) {
  const seen: string[] = [];
  let on = true;
  const loop = (async () => {
    while (on) {
      const out = await athena("status", "--all").catch(() => "");
      const stage = out
        .split("\n")
        .map((l) => l.split(/\s+/))
        .find((cols) => cols[0] === ticket)?.[1];
      if (stage && seen[seen.length - 1] !== stage) seen.push(stage);
      await new Promise((r) => setTimeout(r, 150));
    }
  })();
  return {
    seen,
    async stop() {
      on = false;
      await loop;
    },
  };
}

// Claude Code says something, then its turn stops at the usage limit, resetting a few seconds on: still limited when
// the Ticket moves to Codex, reset (the spec waits for it) before the review is placed.
const LIMIT_RESET_S = 10;
const CLAUDE_LIMITED = [
  { type: "assistant", message: { content: [{ type: "text", text: "The lexer reads integers; fractions are next." }] } },
  { type: "result", subtype: "success", is_error: true, result: `Claude AI usage limit reached|{now+${LIMIT_RESET_S}s}`, num_turns: 1 },
];

const THREAD = "thr_fixture_0001";
const TURN = "turn_fixture_0001";
const at = 1893456001000;

/**
 * A Codex turn (fakecodex replays it on its session's first turn): the message it was sent, what it says, then its
 * ready report through Athena's MCP server (shown as the report tool call, and filed for real by the mcp directive).
 * With release, it waits for that file before it reports.
 */
function codexTurn(said: string, summary: string, release?: string): object[] {
  const ev = (method: string, item: object, ms: number) => ({
    method,
    params: { threadId: THREAD, turnId: TURN, item, ...(method === "item/started" ? { startedAtMs: ms } : { completedAtMs: ms }) },
  });
  const user = {
    id: "item_0001",
    type: "userMessage",
    content: [{ type: "text", text: "(the message)", text_elements: [] }],
    clientId: "c0ffee00-0000-4000-8000-000000000001",
  };
  const message = { id: "item_0002", type: "agentMessage", text: said, phase: "final_answer" };
  const args = { status: "ready", summary };
  const call = (status: string, result: object | null) => ({
    id: "item_0003",
    type: "mcpToolCall",
    server: "athena",
    tool: "report",
    status,
    arguments: args,
    result,
    error: null,
    durationMs: result ? 40 : null,
  });
  return [
    { method: "turn/started", params: { threadId: THREAD, turn: { id: TURN, items: [], status: "inProgress", startedAt: at / 1000 } } },
    ev("item/started", user, at),
    ev("item/completed", user, at + 250),
    ev("item/started", { ...message, text: "" }, at + 1000),
    ev("item/completed", message, at + 1250),
    ...(release ? [{ fake: "wait", path: release }] : []),
    ev("item/started", call("inProgress", null), at + 2000),
    ev("item/completed", call("completed", { content: [{ type: "text", text: "Report filed (ready)." }], structuredContent: null }), at + 2250),
    { fake: "mcp", tool: "report", arguments: args },
  ];
}

const FINDING = "- [medium] lexer.go:42 — a fraction with no digits after the point is accepted";

test("an auto Ticket fails over to Codex, is reviewed on Claude Code and ends ready", async ({ page, session }, testInfo) => {
  test.skip(testInfo.project.name === "tablet", "the flow runs on the desktop and the phone");
  test.setTimeout(150_000);
  const errors: string[] = [];
  page.on("console", (m) => {
    if (m.type() === "error") errors.push(`console.error: ${m.text()}`);
  });
  page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);

  const title = "Lex number fractions, reviewed";
  const ticket = await athena("task", title, "--runtime", "auto", "--review");
  const stages = watchStages(ticket);
  const release = statePath(`release-${ticket}`);
  const release2 = statePath(`release2-${ticket}`);
  const unsetLater = laterFixture(ticket, CLAUDE_LIMITED);
  let unsetCodex = codexFixture(
    ticket,
    codexTurn("Picking up where the Claude Code session stopped: fractions next.", "Fractions lex; the parser tests pass.", release),
  );
  const detail = page.getByTestId("ticket-detail");
  const log = detail.getByRole("log", { name: "Session output" });
  const rows = page.getByTestId("timeline-row");
  const open = (tab: "session" | "timeline") => page.goto(`/tickets/${ticket}?tab=${tab}&q=${ticket}`);
  try {
    // 1. It starts on Claude Code.
    await open("session");
    await expect(page.getByRole("heading", { name: title, level: 1 })).toBeVisible();
    await expect(log.locator("[data-kind=runtime]").first()).toHaveAttribute("aria-label", /^Claude Code/);
    await expect(log).toContainText("Session started");
    await expect.poll(() => sessionAssignment(ticket)).not.toBe("");
    const implementer = sessionAssignment(ticket);
    await step(page, testInfo, "1-claude");

    // 2. Its turn stops at the usage limit (fakeclaude's result says so, after the next message).
    const box = page.getByRole("textbox", { name: "Reply to the session" });
    await box.fill("Keep going");
    await box.press("Enter");
    await expect(log.locator("[data-kind=result][data-limit]")).toContainText("Usage limit reached on Claude Code");
    unsetLater();
    // The Codex session took over, and waits (its fixture's wait) until Claude Code's limit has reset.
    await expect(log.locator("[data-kind=runtime]", { hasText: "continued after a usage limit" })).toHaveAttribute(
      "aria-label",
      /^Codex( · \S+)? · continued after a usage limit$/,
    );
    await expect(log).toContainText("Picking up where the Claude Code session stopped");
    await step(page, testInfo, "2-limit");

    // 3. The timeline says it: the limit on Claude Code, then the work continued on Codex.
    await open("timeline");
    await expect(rows.filter({ hasText: "Usage limit reached on Claude Code" })).toHaveCount(1);
    await expect(rows.filter({ hasText: /Continued on Codex/ })).toHaveCount(1);
    await expect(rows.filter({ hasText: "Usage limit reached on Claude Code" })).toContainText("Resets at");
    await step(page, testInfo, "3-continued-on-codex");

    // 4. Once Claude Code's limit has reset, the Codex session goes on and reports ready.
    await untilNotLimited("claude");
    writeFileSync(release, "");
    await open("session");
    await expect(log.getByTestId("tool-call").filter({ hasText: "Reported ready" })).toHaveCount(1);
    await step(page, testInfo, "4-codex-ready");

    // 5. The review runs on Claude Code (the other runtime) and requests changes. The implementer resumes on Codex
    // with a fixture of its own, written before the verdict sends it there; it waits to report until step 6.
    await open("timeline");
    await expect(rows.filter({ hasText: "Review started · Claude Code" })).toHaveCount(1);
    await expect.poll(() => sessionAssignment(ticket)).not.toBe(implementer);
    const firstReview = sessionAssignment(ticket);
    unsetCodex();
    unsetCodex = codexFixture(
      ticket,
      codexTurn("Fractions now need a digit after the point, as the review asked.", "A fraction needs a digit after the point; the tests cover it.", release2),
    );
    await session.review(ticket, "changes", FINDING);
    const changes = rows.filter({ hasText: "Changes requested" });
    await expect(changes).toHaveCount(1);
    await expect(changes).toContainText("a fraction with no digits after the point is accepted");
    await step(page, testInfo, "5-changes-requested");

    // 6. The implementer resumes on Codex (its session, with the findings) and reports ready again.
    await open("session");
    await expect(log).toContainText("Fractions now need a digit after the point, as the review asked.");
    await expect(log).toContainText("requested changes");
    writeFileSync(release2, "");
    await expect(log.getByTestId("tool-call").filter({ hasText: "Reported ready" })).toHaveCount(2);
    await step(page, testInfo, "6-codex-ready-again");

    // 7. Round 2, again on Claude Code, approves.
    await open("timeline");
    await expect(rows.filter({ hasText: "Review started · Claude Code" })).toHaveCount(2);
    await expect.poll(() => sessionAssignment(ticket)).not.toBe(firstReview);
    await session.review(ticket, "approved", "- none");
    await expect(rows.filter({ hasText: "Review approved" })).toHaveCount(1);
    await step(page, testInfo, "7-review-approved");

    // 8. The held report applies: the newest report row is round 2's "Ready for review", and the Ticket (it has no
    // repository or Threads) ends completed.
    const ready = rows.filter({ hasText: "Ready for review" }).first();
    await expect(ready).toContainText("A fraction needs a digit after the point");
    await expect(detail.getByText("Completed").first()).toBeVisible();
    await step(page, testInfo, "8-ready-for-review");
  } finally {
    await stages.stop();
    unsetLater();
    unsetCodex();
    await athena("stop", ticket).catch(() => {});
    // The specs after this one start their Tickets on Claude Code: its limit must have reset.
    await untilNotLimited("claude");
    if (errors.length) await testInfo.attach("console errors", { body: errors.join("\n"), contentType: "text/plain" });
    const dir = shotsDir ?? testInfo.outputDir;
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, `multi-runtime-stages-${testInfo.project.name}.txt`), stages.seen.join("\n") + "\n");
  }
  // No person stage at any point: a limit, a failover and a review ask no one.
  expect(stages.seen.filter((s) => s.startsWith("person")), `stages: ${stages.seen.join(" → ")}`).toEqual([]);
  expect(stages.seen[stages.seen.length - 1]).toMatch(/completed/);
  expect(errors, "console errors").toEqual([]);
});
