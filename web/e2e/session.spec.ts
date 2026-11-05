// Smoke test of the session helpers the later specs build on: a tool call the rules escalate raises an Approval the
// dashboard counts at once, the owner's answer reaches the hook, and a Report with a question waits on the owner.
// The Ticket is stopped at the end, so the shared server is back to no open Tickets for the specs after it.
import type { Page } from "@playwright/test";
import { READABLE_SESSION } from "./fixtures/readable-session";
import { athena, expect, expectAccessible, holdLaterResult, laterFixture, signedInPage, stdinLog, test, liveFeed } from "./harness";

test("a session's approval and question reach the dashboard", async ({ page, session }) => {
  await signedInPage(page);
  const ticket = await athena("task", "Deploy the staging build");
  const needsYou = page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: /Needs you/ });
  try {
    const verdict = session.requestApproval(ticket, "Bash", { command: "make deploy" });
    await expect(needsYou).toHaveText("Needs you1");
    await expectAccessible(page);

    const approvals = await athena("approvals");
    const id = approvals.split("\n").find((l) => l.includes(ticket))?.split(/\s+/)[0];
    expect(id, approvals).toBeTruthy();
    await athena("deny", id!, "--note", "not today");
    expect(await verdict).toEqual({ decision: "deny", reason: "denied by the owner: not today" });
    await expect(needsYou).toHaveText("Needs you");

    expect(await session.report(ticket, "waiting", "Staging is down", "Retry later or now?")).toBe("Report filed (waiting).");
    await expect(needsYou).toHaveText("Needs you1");
  } finally {
    await athena("stop", ticket);
  }
  await expect(needsYou).toHaveText("Needs you");
});

// The live session: a Ticket's fakeclaude prints a fixture of its own (R37) after the owner's reply, with two tool
// calls, one of them a long ANSI result. The items arrive in order, each call paired with its result; the reply
// reaches the session's stdin; nothing moves the page sideways. Desktop pins a second Ticket's session under the
// panes. Both Tickets are stopped at the end.
const at = (ms: number) => new Date(Date.parse("2026-11-04T10:00:00Z") + ms).toISOString();
const LONG = "\u001b[32mPASS\u001b[0m " + "acme/api/handlers ".repeat(180);
const LATER = [
  { type: "assistant", timestamp: at(0), message: { content: [{ type: "text", text: "Looking at the failing test." }] } },
  {
    type: "assistant",
    timestamp: at(100),
    message: { content: [{ type: "tool_use", id: "toolu_e2e_1", name: "Bash", input: { command: "go test ./api/..." } }] },
  },
  {
    type: "user",
    timestamp: at(1300),
    message: { content: [{ type: "tool_result", tool_use_id: "toolu_e2e_1", content: "ok  acme/api 0.21s" }] },
  },
  {
    type: "assistant",
    timestamp: at(1400),
    message: { content: [{ type: "tool_use", id: "toolu_e2e_2", name: "Bash", input: { command: "go test -v ./api/handlers" } }] },
  },
  { type: "user", timestamp: at(3400), message: { content: [{ type: "tool_result", tool_use_id: "toolu_e2e_2", content: LONG }] } },
  { type: "assistant", timestamp: at(3500), message: { content: [{ type: "text", text: "All green." }] } },
];

/** Neither the page nor the session log is wider than its box. */
async function expectNoSideScroll(page: Page) {
  const m = await page.evaluate(() =>
    [document.documentElement, ...document.querySelectorAll('[role="log"]'), document.querySelector('[data-pane="detail"]')]
      .filter((el): el is Element => el !== null)
      .map((el) => ({ scroll: el.scrollWidth, client: el.clientWidth })),
  );
  for (const s of m) expect(s.scroll, `scrollWidth ${s.scroll} > clientWidth ${s.client}`).toBeLessThanOrEqual(s.client);
}

test("the live session streams tool calls in order and takes a reply", async ({ page }, testInfo) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const desktop = testInfo.project.name === "desktop";
  const title = "Make the session tests pass";
  const ticket = await athena("task", title);
  const pinned = desktop ? await athena("task", "Tidy the changelog") : undefined;
  try {
    laterFixture(ticket, LATER);
    const pin = pinned ? `&pin=${pinned}` : "";
    await page.goto(`/tickets/${ticket}?tab=session&stage=working&q=session+tests${pin}`);
    await expect(page.getByRole("heading", { name: title, level: 1 })).toBeVisible();
    const log = page.getByTestId("ticket-detail").getByRole("log", { name: "Session output" });
    // The default fixture's init line, as a divider.
    await expect(log).toContainText("Session started · haiku 4.5");
    await expect(page.getByTestId("ticket-detail").getByText("live", { exact: true })).toBeVisible();

    const box = page.getByRole("textbox", { name: "Reply to the session" });
    await box.fill("Run the handler tests too");
    await box.press("Enter");
    await expect(box).toHaveValue("");
    await expect.poll(() => stdinLog(ticket), { message: "the reply on the session's stdin" }).toContain("Run the handler tests too");

    await expect(log).toContainText("All green.");
    const text = await log.innerText();
    const order = ["Looking at the failing test.", "go test ./api/...", "go test -v ./api/handlers", "All green."].map((s) => text.indexOf(s));
    expect(order.every((at) => at >= 0), text).toBe(true);
    expect([...order].sort((a, b) => a - b)).toEqual(order);
    const calls = log.getByTestId("tool-call");
    await expect(calls).toHaveCount(2);
    await expect(calls.nth(0)).toHaveAttribute("data-status", "ok");
    await expect(calls.nth(0)).toContainText("1.2 s");
    await expect(calls.nth(1)).toContainText("2.0 s");
    // The long ANSI result, opened: stripped, wrapped inside the column.
    await calls.nth(1).locator("summary").click();
    await expect(calls.nth(1)).toContainText("PASS acme/api/handlers");
    await expect(calls.nth(1)).not.toContainText("\u001b");
    await expectNoSideScroll(page);

    if (desktop) {
      const strip = page.getByRole("region", { name: "Pinned sessions" });
      await expect(strip.getByRole("article")).toHaveCount(1);
      await expect(strip).toContainText("Tidy the changelog");
      await expect(strip.getByRole("log")).toContainText("Session started");
      await expect(page.getByRole("button", { name: "Pin session" })).toHaveAttribute("aria-pressed", "false");
    }
    await expectAccessible(page);
    // The screenshots start at the Session heading, the long result closed and nothing focused.
    await calls.nth(1).locator("summary").click();
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
    await page.getByRole("heading", { name: "Session", level: 2 }).evaluate((h) => h.scrollIntoView({ block: "start" }));

    // What differs from run to run: times, Ticket ids, and the node's load and counts.
    const unstable = [
      page.locator("time"),
      page.locator("aside .font-mono"),
      page.getByRole("region", { name: "Today" }),
      page.getByRole("region", { name: "Facts" }),
      page.getByTestId("ticket-detail").locator("header .font-mono"),
      page.locator("[data-ticket-row] .font-mono"),
      ...(pinned ? [page.getByRole("region", { name: "Pinned sessions" }).getByText(pinned, { exact: true })] : []),
    ];
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot("session-light.png", { mask: [...unstable, ...liveFeed(page)] });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("session-dark.png", { mask: [...unstable, ...liveFeed(page)] });
  } finally {
    for (const t of [ticket, pinned]) if (t) await athena("stop", t).catch(() => {});
  }
});

// A realistic turn reads as a log of work: prose as Markdown, one compact row per tool call (a verb, a target relative
// to the worktree, a check and its time or +N −M), errors inline, the plan as a checklist, the owner's reply as "You"
// and a closing "Done". None of the session's bookkeeping lines show. Opened rows show a terminal, a diff and the plan.
test("the session view reads as a log of the agent's work", async ({ page, session }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const title = "Add rate limiting to the bookings API";
  const ticket = await athena("task", title);
  try {
    const release = holdLaterResult(ticket);
    laterFixture(ticket, [...READABLE_SESSION]);
    // The list filtered to this Ticket: the shared server holds the other specs' Tickets.
    await page.goto(`/tickets/${ticket}?tab=session&q=${ticket}`);
    await expect(page.getByRole("heading", { name: title, level: 1 })).toBeVisible();
    const log = page.getByTestId("ticket-detail").getByRole("log", { name: "Session output" });
    await expect(log).toContainText("Session started");
    const box = page.getByRole("textbox", { name: "Reply to the session" });
    await box.fill("Go ahead, keep the limit per IP");
    await box.press("Enter");
    // The fixture's report call files nothing: the real Report goes in before the turn ends, so no nudge replays it.
    await expect(log).toContainText("To try it locally");
    expect(await session.report(ticket, "ready", "Added a per-IP rate limiter in front of the bookings API.")).toBe("Report filed (ready).");
    release();
    await expect(log.locator("[data-kind=result]")).toContainText("Done");

    const text = await log.innerText();
    for (const noise of ["task_progress", "thinking_tokens", "rate_limit", "system/", "Thinking", "ToolSearch", "/home/dev"]) {
      expect(text, `the log shows ${noise}`).not.toContain(noise);
    }
    await expect(log.locator("[data-kind=user]")).toContainText("Go ahead, keep the limit per IP");
    const rows = log.getByTestId("tool-call");
    await expect(rows).toHaveCount(13);
    const row = (label: string, n = 0) => rows.filter({ has: page.locator("summary", { hasText: label }) }).nth(n);
    await expect(row("Read")).toContainText("internal/api/bookings.go");
    await expect(row("Searched for")).toContainText("in internal");
    await expect(row("Created")).toContainText("internal/api/ratelimit.go");
    await expect(row("Created")).toContainText("+31");
    await expect(row("Edited")).toContainText("+2−1");
    await expect(row("Check the Go toolchain")).toContainText("1.3 s");
    const failedTests = row("Run the API tests");
    await expect(failedTests).toHaveAttribute("data-status", "error");
    await expect(failedTests).toContainText("Exit code 1: --- FAIL: TestRateLimit (0.00s)");
    await expect(row("Run the API tests", 1)).toHaveAttribute("data-status", "ok");
    await expect(row("Check the GitHub login")).toContainText("Blocked: command gh auth is blocked");
    await expect(row("Updated the plan", 1)).toContainText("3 of 4 done");
    await expect(row("Reported ready")).toContainText("PR #42 · Added a per-IP rate limiter");
    // The agent's Markdown: a numbered list, inline code and a code block.
    await expect(log.locator("li[data-row=item] ol li code").first()).toHaveText("internal/api/ratelimit.go");
    await expect(log.locator("pre[aria-label=Code]")).toHaveText("go test ./internal/api/... -run RateLimit -v");
    await expect(log.locator("[data-kind=result]")).toContainText("18 turns · $0.38 · 6 m 12 s");

    // Opened: the failed run's terminal, the router's diff and the plan.
    for (const r of [failedTests, row("Edited"), row("Updated the plan", 1)]) await r.locator("summary").click();
    await expect(failedTests.getByLabel("Terminal")).toContainText("$ go test ./internal/api/... -count=1");
    await expect(row("Edited").locator("[data-line=add]")).toHaveCount(2);
    await expect(row("Updated the plan", 1).locator("[data-todo=completed]")).toHaveCount(3);
    await expectNoSideScroll(page);
    await expectAccessible(page);

    // The screenshots start at the Session heading, the log at its end (the summary and the Done block), nothing focused.
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
    await page.getByRole("heading", { name: "Session", level: 2 }).evaluate((h) => h.scrollIntoView({ block: "start" }));
    await log.evaluate((el) => el.scrollTo({ top: el.scrollHeight }));
    await expect(page.getByRole("button", { name: "Jump to latest" })).toHaveCount(0);
    // What differs from run to run: times, the Ticket id (in the search box too), and the node's load and counts.
    const unstable = [
      page.locator("time"),
      page.locator("aside .font-mono"),
      page.getByRole("region", { name: "Today" }),
      page.getByRole("region", { name: "Facts" }),
      page.getByTestId("ticket-detail").locator("header .font-mono"),
      page.locator("[data-ticket-row] .font-mono"),
      page.getByPlaceholder("Search by title or id"),
    ];
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot("readable-light.png", { mask: [...unstable, ...liveFeed(page)] });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("readable-dark.png", { mask: [...unstable, ...liveFeed(page)] });
  } finally {
    await athena("stop", ticket).catch(() => {});
  }
});
