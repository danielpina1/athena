// Smoke test of the session helpers the later specs build on: a tool call the rules escalate raises an Approval the
// dashboard counts at once, the owner's answer reaches the hook, and a Report with a question waits on the owner.
// The Ticket is stopped at the end, so the shared server is back to no open Tickets for the specs after it.
import type { Page } from "@playwright/test";
import { athena, expect, expectAccessible, laterFixture, signedInPage, stdinLog, test } from "./harness";

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
    // The default fixture's init line.
    await expect(log).toContainText("model claude-");
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
    await expect(calls.nth(0)).toContainText("ok · 1.2 s");
    await expect(calls.nth(1)).toContainText("ok · 2.0 s");
    // The long ANSI result, opened: stripped, wrapped inside the column.
    await calls.nth(1).locator("summary").click();
    await expect(calls.nth(1)).toContainText("PASS acme/api/handlers");
    await expect(calls.nth(1)).not.toContainText("\u001b");
    await expectNoSideScroll(page);

    if (desktop) {
      const strip = page.getByRole("region", { name: "Pinned sessions" });
      await expect(strip.getByRole("article")).toHaveCount(1);
      await expect(strip).toContainText("Tidy the changelog");
      await expect(strip.getByRole("log")).toContainText("model claude-");
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
    await expect(page).toHaveScreenshot("session-light.png", { mask: unstable });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("session-dark.png", { mask: unstable });
  } finally {
    for (const t of [ticket, pinned]) if (t) await athena("stop", t).catch(() => {});
  }
});
