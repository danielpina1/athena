// Needs you, end to end: a session's tool call raises an Approval and a Report raises a question; the owner approves
// with one tap, answers inline and denies from the keyboard. Every Ticket a test opens is stopped at its end, so the
// shared server has no open Tickets for the specs after it.
import type { Page } from "@playwright/test";
import { athena, expect, expectAccessible, signedInPage, test } from "./harness";

const header = (page: Page) => page.getByRole("heading", { name: "Needs you", level: 1 }).locator("xpath=following-sibling::p");

/** What a screenshot must not pin: times, ids and the day's counts vary from run to run. */
const unstable = (page: Page) => [
  page.locator("time"),
  page.locator('main a[href^="/tickets/"]'),
  header(page),
  page.getByText(/^Today: /),
];

async function stopAll(...tickets: string[]) {
  for (const t of tickets) await athena("stop", t).catch(() => {});
}

test("approve with one tap and answer a question, light and dark", async ({ page, session }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const deploy = await athena("task", "Deploy the staging build");
  const database = await athena("task", "Pick a database for the shop");
  try {
    const verdict = session.requestApproval(deploy, "Bash", { command: "make deploy" });
    await session.report(database, "waiting", "Two options fit", "Postgres or SQLite?");

    const approval = page.getByRole("group", { name: /Deploy the staging build/ });
    const answer = page.getByRole("group", { name: /Pick a database/ });
    await expect(approval.getByRole("button", { name: "Approve" })).toBeVisible();
    await expect(answer.getByText("Postgres or SQLite?")).toBeVisible();
    await expect(header(page)).toContainText("2 need you");
    // Approvals come before answers.
    await expect(page.getByRole("group")).toHaveText([/Deploy the staging build/, /Pick a database/]);

    await expectAccessible(page);
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot("needs-you-light.png", { mask: unstable(page) });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("needs-you-dark.png", { mask: unstable(page) });

    // One tap: the count drops and the card says Approved; the session is let through; the card leaves.
    await approval.getByRole("button", { name: "Approve" }).click();
    expect(await verdict).toEqual({ decision: "allow", reason: expect.any(String) });
    await expect(approval).toHaveCount(0);
    await expect(header(page)).toContainText("1 need you");

    // Enter sends the answer; the session goes on, so its card leaves.
    const box = answer.getByRole("textbox", { name: "Your answer" });
    await box.fill("SQLite");
    await box.press("Enter");
    await expect(answer).toHaveCount(0);
    await expect(header(page)).toContainText("0 need you");
    await expectAccessible(page);
  } finally {
    await stopAll(deploy, database);
  }
  await expect(page.getByText("Nothing needs you.")).toBeVisible();
});

test("deny with a note", async ({ page, session }) => {
  await signedInPage(page);
  const ticket = await athena("task", "Clean the build cache");
  try {
    const verdict = session.requestApproval(ticket, "Bash", { command: "make deploy --force" });
    const card = page.getByRole("group", { name: /Clean the build cache/ });
    await card.getByRole("textbox", { name: "Note (optional)" }).fill("keep it");
    await card.getByRole("button", { name: "Deny" }).click();
    await expect(card.getByText("Denied")).toBeVisible();
    expect(await verdict).toEqual({ decision: "deny", reason: "denied by the owner: keep it" });
    await expect(card).toHaveCount(0);
  } finally {
    await stopAll(ticket);
  }
});

test("stop a blocked Ticket asks twice", async ({ page, session }) => {
  await signedInPage(page);
  const ticket = await athena("task", "Migrate the schema");
  try {
    await session.report(ticket, "blocked", "The database is read-only");
    const card = page.getByRole("group", { name: /Migrate the schema/ });
    await expect(card.getByText("Blocked", { exact: true })).toBeVisible();
    await expect(card.getByRole("button", { name: "Retry" })).toBeVisible();
    await card.getByRole("button", { name: "Stop" }).click();
    await expect(card).toBeVisible();
    await page.waitForTimeout(400);
    await card.getByRole("button", { name: "Stop this Ticket" }).click();
    await expect(card).toHaveCount(0);
  } finally {
    await stopAll(ticket);
  }
});

test("the keyboard moves and approves on the desktop", async ({ page, session }, testInfo) => {
  test.skip(testInfo.project.name !== "desktop", "keys are a desktop feature");
  await signedInPage(page);
  const ticket = await athena("task", "Rotate the logs");
  try {
    const verdict = session.requestApproval(ticket, "Bash", { command: "make deploy --dry-run" });
    const card = page.getByRole("group", { name: /Rotate the logs/ });
    await expect(card).toBeVisible();
    await page.keyboard.press("j");
    await expect(card).toBeFocused();
    await page.keyboard.press("a");
    expect((await verdict).decision).toBe("allow");
    await expect(card).toHaveCount(0);
  } finally {
    await stopAll(ticket);
  }
});

test("nothing needs you", async ({ page }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await expect(page.getByText("Nothing needs you.")).toBeVisible();
  await expectAccessible(page);
  await page.evaluate(() => document.fonts.ready);
  await expect(page).toHaveScreenshot("empty-light.png", { mask: unstable(page) });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectAccessible(page);
  await expect(page).toHaveScreenshot("empty-dark.png", { mask: unstable(page) });
});
