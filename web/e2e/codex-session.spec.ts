// Codex in the session view, on fakecodex (never this machine's codex): a Ticket pinned to Codex streams its plan, its
// commands (a failure says its exit code), a patch of two files, a web search and its Report, and the reading of the
// quota it sent shows on the node card; a Ticket that starts on Claude Code (fakeclaude) and stops at its usage limit
// continues on Codex, the transcript marking the move with a runtime divider and the new session's hand-off note. Every
// Ticket is stopped at the end, and the Claude Code limit has reset before the next spec runs.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { Locator, Page } from "@playwright/test";
import { athena, codexFixture, expect, expectAccessible, laterFixture, liveFeed, signedInPage, test } from "./harness";
import { ROOT } from "./paths";

/** A fixture of app-server lines, web/e2e/fixtures/<name>. */
const fixture = (name: string): object[] =>
  readFileSync(join(ROOT, "web", "e2e", "fixtures", name), "utf8")
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l) as object);

/** Neither the page nor the session log is wider than its box (once the layout has followed a new viewport). */
async function expectNoSideScroll(page: Page) {
  const wide = () =>
    page.evaluate(() =>
      [document.documentElement, ...document.querySelectorAll('[role="log"]'), document.querySelector('[data-pane="detail"]')]
        .filter((el): el is Element => el !== null && el.scrollWidth > el.clientWidth)
        .map((el) => `${el.getAttribute("role") ?? el.tagName}: scrollWidth ${el.scrollWidth} > clientWidth ${el.clientWidth}`),
    );
  await expect.poll(wide, { message: "nothing scrolls sideways" }).toEqual([]);
}

/** What differs from run to run: times, the Ticket id (in the search box too), and the node's load and counts. */
const unstable = (page: Page) => [
  page.locator("time"),
  page.locator("aside .font-mono"),
  page.getByRole("region", { name: "Today" }),
  page.getByRole("region", { name: "Facts" }),
  page.getByTestId("ticket-detail").locator("header .font-mono"),
  page.locator("[data-ticket-row] .font-mono"),
  page.getByPlaceholder("Search by title or id"),
  // A result's text: Claude Code's usage limit names the time it resets at.
  page.locator("[data-kind=result] > :not(:first-child)"),
  ...liveFeed(page),
];

/** Light and dark screenshots of the page as it is, or of one part of it, axe clean in both; then light again. */
async function shots(page: Page, name: string, part?: Locator) {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await page.evaluate(() => document.fonts.ready);
  for (const scheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: scheme, reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", scheme);
    await expectAccessible(page);
    await expect(part ?? page).toHaveScreenshot(`${name}-${scheme}.png`, { mask: unstable(page) });
  }
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
}

/** A new viewport, once the layout has followed it (a few frames later, remounting what is on screen). */
async function resize(page: Page, size: { width: number; height: number }) {
  await page.setViewportSize(size);
  await page.waitForTimeout(500);
}

/** The page in a 320 px column scrolls nothing sideways; then the viewport is as it was. */
async function fitsNarrow(page: Page) {
  const full = page.viewportSize()!;
  await page.setViewportSize({ width: 320, height: full.height });
  await expectNoSideScroll(page);
  await resize(page, full);
}

/** The Session heading at the top of the page and the log at its end (no "Jump to latest"), nothing focused. */
async function showLogEnd(page: Page, log: Locator) {
  await page.getByRole("heading", { name: "Session", level: 2 }).evaluate((h) => h.scrollIntoView({ block: "start" }));
  await log.evaluate((el) => el.scrollTo({ top: el.scrollHeight }));
  await expect(page.getByRole("button", { name: "Jump to latest" })).toHaveCount(0);
}

test("a Codex Ticket streams its plan, commands, patch and report", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const title = "Fix the number lexer";
  // The Ticket's id is known only once it exists, and its session starts at once: the fixture waits as the default.
  const unset = codexFixture("default", fixture("codex-turn.jsonl"));
  let ticket = "";
  try {
    ticket = await athena("task", title, "--runtime", "codex");
    await page.goto(`/tickets/${ticket}?tab=session&q=${ticket}`);
    await expect(page.getByRole("heading", { name: title, level: 1 })).toBeVisible();
    const log = page.getByTestId("ticket-detail").getByRole("log", { name: "Session output" });
    // The ready Report ends the Ticket (it has no repository): its session stops there.
    await expect(log.getByTestId("tool-call").filter({ hasText: "Reported ready" })).toHaveAttribute("data-status", "ok");
    unset();

    // The session's runtime, as a divider; the request it started from, as the Task.
    await expect(log.locator("[data-kind=runtime]").first()).toHaveAttribute("aria-label", /^Codex( · \S+)?$/);
    await expect(log.locator("[data-kind=task]")).toContainText(title);
    const rows = log.getByTestId("tool-call");
    await expect(rows).toHaveCount(8);
    const row = (label: string, n = 0) => rows.filter({ has: page.locator("summary", { hasText: new RegExp(`^${label}`) }) }).nth(n);
    await expect(row("Updated the plan")).toContainText("1 of 3 done");
    await expect(row("Updated the plan", 1)).toContainText("3 of 3 done");
    const failed = row("Ran");
    await expect(failed).toHaveAttribute("data-status", "error");
    await expect(failed).toContainText("go test ./parser/...");
    await expect(failed).toContainText("Exit code 1: --- FAIL: TestLexNumbers (0.00s)");
    await expect(row("Ran", 1)).toContainText("gofmt -l . in parser");
    await expect(row("Ran", 2)).toHaveAttribute("data-status", "ok");
    await expect(row("Edited")).toContainText("2 files");
    await expect(row("Edited").locator("[data-counts]")).toHaveText("+11−1");
    await expect(row("Searched the web")).toContainText("go lexer number literal grammar");
    await expect(row("Reported ready")).toContainText("Fixed the number lexer");
    for (const protocol of ["/bin/zsh", "[input", "ATHENA_", "/home/user", "exitCode"]) {
      expect(await log.innerText(), `the log shows ${protocol}`).not.toContain(protocol);
    }

    await showLogEnd(page, log);
    await shots(page, "codex");
    await fitsNarrow(page);

    // Opened, one at a time: the failed command's terminal, the patch's two files and the plan. A tall viewport holds
    // the whole log, so it rests at its end and no "Jump to latest" floats over the part.
    const full = page.viewportSize()!;
    await resize(page, { width: full.width, height: 2400 });
    const parts = { command: failed, patch: row("Edited"), plan: row("Updated the plan", 1) };
    for (const [name, part] of Object.entries(parts)) {
      await part.locator("summary").click();
      await showLogEnd(page, log);
      await expect(part.locator("summary + div")).toBeVisible();
      await shots(page, `codex-${name}`, part);
      await part.locator("summary").click();
    }
    await resize(page, full);
    await failed.locator("summary").click();
    await expect(failed.getByLabel("Terminal")).toContainText("lex_test.go:14");
    await parts.patch.locator("summary").click();
    await expect(parts.patch.getByRole("group", { name: "Changes in parser/lex.go" }).locator("[data-line=del]")).toHaveCount(1);
    await expect(parts.patch.getByRole("group", { name: "Changes in parser/fraction.go" }).locator("[data-line=add]")).toHaveCount(9);
    await parts.plan.locator("summary").click();
    await expect(parts.plan.locator("[data-todo=completed]")).toHaveCount(3);

    // The quota reading this fakecodex session sent (37% of the 5h window) is the node's.
    await page.goto("/nodes");
    await expect(page.getByRole("meter", { name: "Codex 5h used" })).toHaveAttribute("aria-valuetext", "37%");
    await expect(page.getByRole("meter", { name: "Codex 7d used" })).toHaveAttribute("aria-valuetext", "9%");
  } finally {
    unset();
    if (ticket) await athena("stop", ticket).catch(() => {});
  }
});

// Claude Code says something, then its turn stops at the usage limit (fakeclaude's result says so, resetting a few
// seconds on); the Ticket continues on Codex with no one asked.
const LIMIT_RESET_S = 10;
const CLAUDE_LIMITED = [
  { type: "assistant", message: { content: [{ type: "text", text: "The lexer reads integers; fractions are next." }] } },
  { type: "result", subtype: "success", is_error: true, result: `Claude AI usage limit reached|{now+${LIMIT_RESET_S}s}`, num_turns: 1 },
];

test("a Ticket that moves from Claude Code to Codex shows the divider and the hand-off", async ({ page }) => {
  test.setTimeout(90_000);
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const title = "Lex number fractions";
  const ticket = await athena("task", title, "--runtime", "auto");
  let resetsAt = 0;
  try {
    codexFixture(ticket, fixture("codex-failover.jsonl"));
    laterFixture(ticket, CLAUDE_LIMITED);
    await page.goto(`/tickets/${ticket}?tab=session&q=${ticket}`);
    const log = page.getByTestId("ticket-detail").getByRole("log", { name: "Session output" });
    await expect(log.locator("[data-kind=runtime]").first()).toHaveAttribute("aria-label", /^Claude Code/);
    await expect(log).toContainText("Session started");

    const box = page.getByRole("textbox", { name: "Reply to the session" });
    await box.fill("Keep going");
    resetsAt = Date.now() + LIMIT_RESET_S * 1000;
    await box.press("Enter");

    const moved = log.locator("[data-kind=runtime]", { hasText: "continued after a usage limit" });
    await expect(moved).toHaveAttribute("aria-label", /^Codex( · \S+)? · continued after a usage limit$/);
    const handoff = log.getByRole("region", { name: "Hand-off note" });
    await expect(handoff).toContainText("You are continuing this Ticket from a session on Claude Code that stopped at its usage limit.");
    await expect(handoff).toContainText("The lexer reads integers; fractions are next.");
    await expect(log).toContainText("Picking up where the Claude Code session stopped");
    // In order: Claude Code's words, the divider, the hand-off, Codex's words.
    const text = await log.innerText();
    const order = ["The lexer reads integers", "continued after a usage limit", "You are continuing this Ticket", "Picking up where"].map((s) => text.indexOf(s));
    expect(order.every((at) => at >= 0), text).toBe(true);
    expect([...order].sort((a, b) => a - b)).toEqual(order);
    for (const protocol of ["[input", "ATHENA_", "You are the worker", "Execution variables"]) {
      expect(text, `the log shows ${protocol}`).not.toContain(protocol);
    }

    // The move near the top of the log: the end of Claude Code's turn above it, the hand-off and Codex's words below.
    await page.getByRole("heading", { name: "Session", level: 2 }).evaluate((h) => h.scrollIntoView({ block: "start" }));
    await moved.evaluate((d) => {
      const box = d.closest("[role=log]")!;
      box.scrollTop += d.getBoundingClientRect().top - box.getBoundingClientRect().top - 64;
    });
    await shots(page, "failover");
    await fitsNarrow(page);

    // The hand-off note in full.
    await handoff.getByRole("button", { name: "Show more" }).click();
    await expect(handoff.getByRole("button", { name: "Show less" })).toBeVisible();
    await expect(handoff).toContainText("Untracked files: 0");
    await shots(page, "handoff", handoff);
  } finally {
    await athena("stop", ticket).catch(() => {});
    // The specs after this one start their Tickets on Claude Code: its limit must have reset.
    const left = resetsAt + 1000 - Date.now();
    if (left > 0) await page.waitForTimeout(left);
  }
});
