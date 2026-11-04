// Routing, end to end. The shared server has no GitHub, but earlier specs' Tickets leave Triggers behind, so these
// tests do not assume the lists are empty: each tab's count must equal the rows it lists (or show its empty state at
// zero), the filters live in the URL, and `/` goes to the tab's own search box. The screenshots hide the list and the
// counts, which depend on what ran before. This spec creates no Tickets (R37).
import type { Page } from "@playwright/test";
import { expect, expectAccessible, signedInPage, test } from "./harness";

const tabs = (page: Page) => page.getByRole("tablist", { name: "Routing records" });

/** The number a tab's label carries. */
async function count(page: Page, name: RegExp): Promise<number> {
  return Number((await tabs(page).getByRole("tab", { name }).innerText()).replace(/\D/g, ""));
}

test("each tab's count equals the rows it lists, or it shows its empty state", async ({ page }) => {
  await signedInPage(page);
  await page.goto("/routing");
  await expect(page.getByRole("heading", { name: "Routing", level: 1 })).toBeVisible();
  const rows = { triggers: "[data-trigger-row]", threads: "[data-thread-row]", decisions: "[data-decision-row]" };
  const empty = { triggers: "No Triggers yet.", threads: "No Threads yet.", decisions: "No Decisions yet." };
  for (const [tab, label] of [["triggers", /Triggers/], ["threads", /Threads/], ["decisions", /Decisions/]] as const) {
    await tabs(page).getByRole("tab", { name: label }).click();
    if (tab !== "triggers") await expect(page).toHaveURL(new RegExp(`tab=${tab}`));
    const n = await count(page, label);
    if (n === 0) await expect(page.getByRole("heading", { name: empty[tab] })).toBeVisible();
    else await expect(page.locator(rows[tab])).toHaveCount(Math.min(n, 200));
    await expectAccessible(page);
  }
});

test("a filter that matches nothing says so, light and dark", async ({ page }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await page.goto("/routing?tq=nothing-has-this-text");
  // Nothing has this text: "No Triggers yet." on a server that received none, else "No Triggers match these filters."
  await expect(page.getByRole("heading", { name: /^No Triggers (yet|match these filters)\.$/ })).toBeVisible();
  await expect(tabs(page).getByRole("tab", { name: /Triggers/ })).toHaveText("Triggers 0");

  // `/` goes to the tab's own search box, which keeps the text from the URL.
  await page.keyboard.press("/");
  const box = page.getByRole("searchbox", { name: "Search Triggers" });
  await expect(box).toBeFocused();
  await expect(box).toHaveValue("nothing-has-this-text");
  await page.getByRole("heading", { name: "Routing", level: 1 }).click();

  await expectAccessible(page);
  const unstable = [page.locator("aside .font-mono"), page.getByRole("region", { name: "Today" })];
  // What the lists hold depends on the specs that ran before: hide them, and give the panel a fixed height.
  await page.evaluate(() => {
    for (const el of document.querySelectorAll<HTMLElement>('[role="tablist"], [role="tabpanel"]')) el.style.visibility = "hidden";
    document.querySelector<HTMLElement>('[role="tabpanel"]')!.style.height = "320px";
  });
  await page.evaluate(() => document.fonts.ready);
  await expect(page).toHaveScreenshot("no-match-light.png", { mask: unstable });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectAccessible(page);
  await expect(page).toHaveScreenshot("no-match-dark.png", { mask: unstable });
  await page.evaluate(() => {
    for (const el of document.querySelectorAll<HTMLElement>('[role="tablist"], [role="tabpanel"]')) el.style.visibility = "";
  });

  // The filter is in the URL; Clear filters (shown once the server has Triggers) takes it out again.
  await expect(page).toHaveURL(/tq=nothing-has-this-text/);
  const clear = page.getByRole("button", { name: "Clear filters" });
  if (await clear.count()) {
    await clear.click();
    await expect(page).toHaveURL(/\/routing$/);
    await expect(box).toHaveValue("");
  }
});

test("the palette finds no Thread for a GitHub URL on a server without GitHub, and opens Routing", async ({ page }, testInfo) => {
  await signedInPage(page);
  if (testInfo.project.name === "desktop") await page.keyboard.press("ControlOrMeta+k");
  else await page.getByRole("button", { name: "Search and commands" }).click();
  const palette = page.getByRole("dialog", { name: "Command palette" });
  await palette.getByRole("combobox").fill("https://github.com/nobody-e2e/nothing/pull/987654");
  await expect(palette.getByText("Nothing matches.")).toBeVisible();
  await palette.getByRole("combobox").fill("routing");
  await palette.getByRole("option", { name: "Routing" }).click();
  await expect(page).toHaveURL(/\/routing$/);
});
