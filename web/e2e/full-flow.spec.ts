// The whole flow in one test, on the phone and the desktop (R37: its Tickets run the default fakeclaude fixture and
// the session helpers act as their workers; both are ended): sign in, New task without a repository, watch it live,
// answer its question, approve its Approval, see it Ready for review, stop a second Ticket (asked twice), open the
// graph, sign out. Every state is checked for axe, a 320 px column without sideways scroll, light and dark; the
// test fails on any console.error (the harness already fails it on a CSP violation) and on any page error.
// Set ATHENA_E2E_SHOTS=<dir> to keep each state's screenshots there (<state>-<light|dark>-<project>.png).
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import type { Locator, Page, TestInfo } from "@playwright/test";
import { athena, expect, expectAccessible, signedInPage, simulateTouch, test } from "./harness";

const shotsDir = process.env.ATHENA_E2E_SHOTS;
if (shotsDir) mkdirSync(shotsDir, { recursive: true });

/** The states each check runs in, light then dark, in one place so a state is never checked in one theme only. */
async function inBothThemes(page: Page, testInfo: TestInfo, state: string) {
  const project = testInfo.project.name;
  const full = page.viewportSize() ?? { width: 390, height: 844 };
  for (const scheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: scheme, reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", scheme);
    await expectAccessible(page);
    await expectNoSideScroll(page);
    if (project === "phone") await expectTouchTargets(page);
    await page.evaluate(() => document.fonts.ready);
    const path = join(shotsDir ?? testInfo.outputDir, `${state}-${scheme}-${project}.png`);
    await page.screenshot({ path });
    // The same page in a 320 px column: nothing is wider than it.
    await page.setViewportSize({ width: 320, height: full.height });
    await expectNoSideScroll(page);
    await page.setViewportSize(full);
    // The layout follows the viewport a few frames later, remounting what is on screen: let it settle.
    await page.waitForTimeout(500);
  }
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
}

/** On the phone every button, link-button, tab and field a thumb must hit is at least 44 px tall (inline text links excepted). */
async function expectTouchTargets(page: Page) {
  const small = await page.evaluate(() =>
    [...document.querySelectorAll<HTMLElement>('button, [role="tab"], [role="radio"], input:not([type="hidden"]), select, textarea, nav a')]
      .filter((el) => {
        const r = el.getBoundingClientRect();
        const hidden = r.width === 0 || r.height === 0 || getComputedStyle(el).visibility === "hidden" || el.closest("[hidden]") !== null;
        return !hidden && !el.matches(".sr-only, .sr-only *") && r.height < 43.5;
      })
      .map((el) => `${el.tagName.toLowerCase()} "${(el.getAttribute("aria-label") || el.textContent || "").trim().slice(0, 30)}" ${Math.round(el.getBoundingClientRect().height)}px`),
  );
  expect(small, "targets under 44 px").toEqual([]);
}

/** The page is no wider than its viewport (polled: a resize re-lays the page out a frame later). */
async function expectNoSideScroll(page: Page) {
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth), {
      message: "page scrollWidth minus clientWidth",
    })
    .toBeLessThanOrEqual(0);
}

/** The 320 px check re-lays the page out (a desktop becomes a phone for a moment), which closes an open confirmation: ask again. */
async function rearm(first: Locator, second: Locator) {
  if (await second.isVisible()) return;
  await first.click();
  await expect(second).toBeVisible();
  await second.page().waitForTimeout(400);
}

test("sign in, task, answer, approve, review, stop, graph, sign out", async ({ page, session }, testInfo) => {
  test.setTimeout(180_000);
  const errors: string[] = [];
  page.on("console", (m) => {
    if (m.type() === "error") errors.push(`console.error: ${m.text()}`);
  });
  page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  const started: string[] = [];
  const nav = page.getByRole("navigation", { name: "Main" });
  try {
    // 1. Sign in (real enrollment and passkey sign-in).
    await signedInPage(page);
    await expect(page.getByText("Nothing needs you.")).toBeVisible();
    await inBothThemes(page, testInfo, "1-signed-in");

    // 2. New task, no repository.
    await nav.getByRole("link", { name: /New/ }).click();
    await expect(page).toHaveURL(/\/new$/);
    const text = page.getByRole("textbox", { name: "What should Athena do?" });
    await text.fill("Pick a database for the shop");
    await expect(page.getByRole("combobox", { name: "Repository" })).toHaveAttribute("placeholder", "No repository");
    await inBothThemes(page, testInfo, "2-new-task");
    // The 320 px check re-laid the page out (a desktop becomes a phone for a moment) and the form began again.
    await text.fill("Pick a database for the shop");
    await page.getByRole("button", { name: "Start" }).click();
    await expect(page).toHaveURL(/\/tickets\/[^/?]+$/);
    const id = new URL(page.url()).pathname.split("/").pop() ?? "";
    started.push(id);

    // 3. Watch it live.
    await expect(page.getByRole("heading", { name: "Pick a database for the shop", level: 1 })).toBeVisible();
    await page.goto(`/tickets/${id}?tab=session`);
    const log = page.getByTestId("ticket-detail").getByRole("log", { name: "Session output" });
    await expect(log).toContainText("model claude-");
    await expect(page.getByTestId("ticket-detail").getByText("live", { exact: true })).toBeVisible();
    await inBothThemes(page, testInfo, "3-live");

    // 4. Its question reaches Needs you; the owner answers inline.
    await session.report(id, "waiting", "Two options fit", "Postgres or SQLite?");
    await page.goto("/");
    const card = page.getByRole("group", { name: /Pick a database/ });
    await expect(card.getByText("Postgres or SQLite?")).toBeVisible();
    await inBothThemes(page, testInfo, "4-question");
    const box = card.getByRole("textbox", { name: "Your answer" });
    await box.fill("SQLite");
    await box.press("Enter");
    await expect(card).toHaveCount(0);

    // 5. Its Approval: one tap.
    const verdict = session.requestApproval(id, "Bash", { command: "make deploy" });
    await expect(card.getByRole("button", { name: "Approve" })).toBeVisible();
    await inBothThemes(page, testInfo, "5-approval");
    await card.getByRole("button", { name: "Approve" }).click();
    expect((await verdict).decision).toBe("allow");
    await expect(card).toHaveCount(0);

    // 6. Done. A Ticket with no Threads to follow ends as Completed on its ready report, so the server never holds
    // it at "Ready for review" here (that card is the Threads' case: the unit tests cover it); it leaves Needs you
    // and its page says how it ended.
    await session.report(id, "ready", "The shop uses SQLite.");
    await page.goto(`/tickets/${id}`);
    await expect(page.getByTestId("ticket-detail").getByText("Completed").first()).toBeVisible();
    await inBothThemes(page, testInfo, "6-completed");

    // 7. A second Ticket: Stop asks twice.
    const second = await athena("task", "Tidy the changelog");
    started.push(second);
    await page.goto(`/tickets/${second}`);
    await expect(page.getByRole("heading", { name: "Tidy the changelog", level: 1 })).toBeVisible();
    const detail = page.getByTestId("ticket-detail");
    await detail.getByRole("button", { name: "Stop", exact: true }).click();
    const confirm = detail.getByRole("button", { name: "Stop this Ticket" });
    await expect(confirm).toBeVisible();
    await inBothThemes(page, testInfo, "7-stop-confirm");
    await rearm(detail.getByRole("button", { name: "Stop", exact: true }), confirm);
    await confirm.click();
    await expect(detail.getByText("Stopped").first()).toBeVisible();
    await inBothThemes(page, testInfo, "7-stopped");

    // 8. The graph.
    await nav.getByRole("link", { name: /Graph/ }).click();
    await expect(page).toHaveURL(/\/graph/);
    await expect(page.locator(`svg [data-node="${id}"]`)).toBeAttached();
    await inBothThemes(page, testInfo, "8-graph");

    // 9. Sign out: Settings › Devices, asked twice; the sign-in screen follows.
    await page.goto("/settings/devices");
    await page.getByRole("button", { name: "Sign out", exact: true }).click();
    await page.waitForTimeout(400);
    await inBothThemes(page, testInfo, "9-sign-out-confirm");
    const out = page.getByRole("button", { name: "Sign out of this device" });
    await rearm(page.getByRole("button", { name: "Sign out", exact: true }), out);
    // No touch while signed out: the sign-in screen waits instead of its passkey autofill signing straight back in.
    await simulateTouch(page, false);
    await out.click();
    await expect(page.getByRole("button", { name: "Sign in with passkey" })).toBeVisible();
    await inBothThemes(page, testInfo, "9-signed-out");
  } finally {
    for (const t of started) await athena("stop", t).catch(() => {});
  }
  expect(errors, "console errors").toEqual([]);
});
