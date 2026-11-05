// Routing, end to end. The shared server has no GitHub, but earlier specs' Tickets leave Triggers behind, so these
// tests do not assume the lists are empty: each tab's count must equal the rows it lists (or show its empty state at
// zero), the filters live in the URL, and `/` goes to the tab's own search box. The screenshots mask the counts and the
// rows, which depend on what ran before. This spec creates no Tickets (R37).
import type { Page } from "@playwright/test";
import { expect, expectAccessible, signedInPage, test, liveFeed } from "./harness";

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

test("the empty state and a filter that matches nothing, light and dark", async ({ page }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  // Only the counts and the rows are volatile (earlier specs' Tickets leave Triggers and Decisions behind).
  const unstable = [
    page.locator("aside .font-mono"),
    page.getByRole("region", { name: "Today" }),
    tabs(page).locator(".font-mono"),
    page.locator("[data-trigger-row], [data-thread-row], [data-decision-row]"),
  ];
  const shot = async (name: string) => {
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot(name, { mask: [...unstable, ...liveFeed(page)] });
  };

  // Without GitHub there is no Thread: that tab is empty on every run and says what will appear there.
  await page.goto("/routing?tab=threads");
  await expect(page.getByRole("heading", { name: "No Threads yet." })).toBeVisible();
  await expect(page.getByText("No GitHub issue or pull request is followed yet.")).toBeVisible();
  await expectAccessible(page);
  await shot("empty-light.png");
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectAccessible(page);
  await shot("empty-dark.png");
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");

  // A filter nothing satisfies. "No Triggers yet." on a server that received none, so only a server that has some
  // shows the filtered state.
  await page.goto("/routing?tq=nothing-has-this-text");
  await expect(page.getByRole("heading", { name: /^No Triggers (yet|match these filters)\.$/ })).toBeVisible();
  await expect(tabs(page).getByRole("tab", { name: /Triggers/ })).toHaveText("Triggers 0");

  // `/` goes to the tab's own search box, which keeps the text from the URL.
  await page.keyboard.press("/");
  const box = page.getByRole("searchbox", { name: "Search Triggers" });
  await expect(box).toBeFocused();
  await expect(box).toHaveValue("nothing-has-this-text");
  await page.getByRole("heading", { name: "Routing", level: 1 }).click();
  await expectAccessible(page);
  if (await page.getByRole("button", { name: "Clear filters" }).count()) {
    await shot("filtered-light.png");
    await page.getByRole("button", { name: "Clear filters" }).click();
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
