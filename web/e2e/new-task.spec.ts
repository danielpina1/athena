// New task, end to end: the form with its repository picker open (free entry of owner/name), then a task without a
// repository started on the page, landing on the new Ticket's detail; the runtime picker with a runtime that is not
// ready, a task pinned to Codex (fakecodex) and paired with a review, whose Facts say so, and one on a model picked
// from fakecodex's model/list, and the Other… field. Every Ticket is stopped at the end (R37).
import type { Page } from "@playwright/test";
import { athena, expect, expectAccessible, signedInPage, test, liveFeed } from "./harness";
import { editLink, fixedNode, runtimeStatus } from "./link";

const modelOptions = (page: Page) => page.getByRole("combobox", { name: "Model", exact: true }).locator("option");
const claudeModels = [
  { id: "opus", name: "Opus" },
  { id: "sonnet", name: "Sonnet", default: true },
  { id: "haiku", name: "Haiku" },
];

test("a no-repo task starts from the form and lands on its detail, light and dark", async ({ page }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await page.goto("/new");
  const text = page.getByRole("textbox", { name: "What should Athena do?" });
  await expect(text).toBeVisible();
  const repo = page.getByRole("combobox", { name: "Repository" });
  await expect(repo).toHaveAttribute("placeholder", "No repository");

  // Both runtimes of the e2e server are ready (Codex is fakecodex): every runtime can be chosen, Auto by default,
  // and the review switch is offered.
  await expect(page.getByRole("radio", { name: "Auto" })).toHaveAttribute("aria-checked", "true");
  await expect(page.getByRole("radio", { name: "Claude Code" })).toBeEnabled();
  await expect(page.getByRole("radio", { name: "Codex" })).toBeEnabled();
  await expect(page.getByRole("switch", { name: "Review each ready report" })).toBeVisible();
  // Under Auto the model is Claude Code's, from its list, with the node's model (sonnet) as the default.
  await expect(modelOptions(page)).toHaveText(["Default · Sonnet", "Opus", "Sonnet", "Haiku", "Other…"]);
  await expect(page.getByRole("combobox", { name: "Model", exact: true })).toHaveAccessibleDescription(
    "Model on Claude Code — Codex uses its default",
  );

  // Free entry offers itself; the list is open for the screenshots.
  await text.fill("Sketch the release notes");
  await repo.fill("acme/widgets");
  await expect(page.getByRole("option", { name: "Use acme/widgets" })).toBeVisible();
  await expectAccessible(page);
  const unstable = [page.locator("aside .font-mono"), page.getByRole("region", { name: "Today" })];
  await page.evaluate(() => document.fonts.ready);
  await expect(page).toHaveScreenshot("new-task-light.png", { mask: [...unstable, ...liveFeed(page)] });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(page).toHaveScreenshot("new-task-dark.png", { mask: [...unstable, ...liveFeed(page)] });

  // An invalid entry is refused inline; no repository (the default) starts the task.
  await repo.fill("acme");
  await expect(page.getByRole("alert")).toContainText("owner/name");
  await expect(page.getByRole("button", { name: "Start" })).toBeDisabled();
  await repo.fill("");
  // The list is open, with every repository the shared server knows: Start must take the first tap.
  await expect(repo).toBeFocused();
  await expect(page.getByRole("option", { name: "No repository" })).toBeVisible();
  const start = page.getByRole("button", { name: "Start" });
  await expect(start).toBeEnabled();
  await start.click();
  await expect(page).toHaveURL(/\/tickets\/[^/?]+$/);
  const id = new URL(page.url()).pathname.split("/").pop() ?? "";
  try {
    await expect(page.getByRole("heading", { name: "Sketch the release notes", level: 1 })).toBeVisible();
    await expectAccessible(page);
  } finally {
    await athena("stop", id).catch(() => {});
  }
});

test("a runtime that is not ready is disabled and says why, light and dark", async ({ page }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  const why = "Codex is not installed (codex not found)";
  await editLink(page, {
    node: fixedNode([
      runtimeStatus({ name: "claude", ready: true, measured: false, models: claudeModels }),
      runtimeStatus({ name: "codex", version: "0.160.1", ready: false, tested: true, why }),
    ]),
  });
  await signedInPage(page);
  await page.goto("/new");
  const codex = page.getByRole("radio", { name: "Codex" });
  await expect(codex).toBeDisabled();
  await expect(codex).toHaveAttribute("title", why);
  await expect(codex).toHaveAccessibleDescription(why);
  await expect(page.getByText(why)).toBeVisible();
  // One runtime ready, and a Policy that pairs nothing: no review switch.
  await expect(page.getByRole("switch", { name: "Review each ready report" })).toHaveCount(0);
  await page.getByRole("textbox", { name: "What should Athena do?" }).fill("Tidy the changelog");
  await expectAccessible(page);
  const unstable = [page.getByRole("region", { name: "Today" })];
  await page.evaluate(() => document.fonts.ready);
  await expect(page).toHaveScreenshot("new-task-unready-light.png", { mask: [...unstable, ...liveFeed(page)] });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectAccessible(page);
  await expect(page).toHaveScreenshot("new-task-unready-dark.png", { mask: [...unstable, ...liveFeed(page)] });
});

test("a task pinned to Codex with a review says so in its Facts, light and dark", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await page.goto("/new");
  await page.getByRole("textbox", { name: "What should Athena do?" }).fill("Port the tokenizer to the new grammar");
  await page.getByRole("radio", { name: "Codex" }).click();
  const review = page.getByRole("switch", { name: "Review each ready report" });
  await expect(review).toHaveAttribute("aria-checked", "false");
  await review.click();
  await expect(review).toHaveAttribute("aria-checked", "true");
  await page.getByRole("button", { name: "Start" }).click();
  await expect(page).toHaveURL(/\/tickets\/[^/?]+$/);
  const id = new URL(page.url()).pathname.split("/").pop() ?? "";
  try {
    const facts = page.getByRole("region", { name: "Facts" });
    const value = (label: string) => facts.locator("dt", { hasText: label }).locator("xpath=following-sibling::dd[1]");
    // Pinned to Codex, so no "· auto"; it runs on fakecodex.
    await expect(value("Runtime")).toHaveText("Codex");
    await expect(value("Review rounds")).toHaveText("0");
    await expectAccessible(page);
    await expect(facts).toHaveScreenshot("facts-runtime-light.png", { mask: [facts.locator("time")] });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expect(facts).toHaveScreenshot("facts-runtime-dark.png", { mask: [facts.locator("time")] });
  } finally {
    await athena("stop", id).catch(() => {});
  }
});

test("a Codex model picked from its list shows in the Facts; Other… takes any model, light and dark", async ({ page }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await page.goto("/new");
  await page.getByRole("textbox", { name: "What should Athena do?" }).fill("Trim the lexer's lookahead");
  await page.getByRole("radio", { name: "Codex" }).click();
  // fakecodex's model/list, from the node's check: its hidden model is not offered.
  await expect(modelOptions(page)).toHaveText(["Default · GPT-5 Codex", "GPT-5 Codex", "GPT-5 Codex Mini", "Other…"]);
  const model = page.getByRole("combobox", { name: "Model", exact: true });
  await expect(model).not.toHaveAccessibleDescription(/Codex uses its default/);

  // Other…: a free-text field for a model the list lacks.
  await model.selectOption({ label: "Other…" });
  const other = page.getByRole("combobox", { name: "Other model" });
  await other.fill("gpt-5-codex-preview");
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await expectAccessible(page);
  const unstable = [page.locator("aside .font-mono"), page.getByRole("region", { name: "Today" }), ...liveFeed(page)];
  await page.evaluate(() => document.fonts.ready);
  await expect(page).toHaveScreenshot("new-task-other-light.png", { mask: unstable });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectAccessible(page);
  await expect(page).toHaveScreenshot("new-task-other-dark.png", { mask: unstable });

  // The form with both fields fits a 320 px column. A desktop crossing to the phone layout remounts the form, so it is
  // filled again after.
  const full = page.viewportSize()!;
  await page.setViewportSize({ width: 320, height: full.height });
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth), {
      message: "page scrollWidth minus clientWidth at 320 px",
    })
    .toBe(0);
  await page.setViewportSize(full);
  await page.waitForTimeout(500);

  // A listed model: the select alone.
  await page.getByRole("textbox", { name: "What should Athena do?" }).fill("Trim the lexer's lookahead");
  await page.getByRole("radio", { name: "Codex" }).click();
  await model.selectOption({ label: "GPT-5 Codex Mini" });
  await expect(other).toHaveCount(0);
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await expectAccessible(page);
  await expect(page).toHaveScreenshot("new-task-codex-model-dark.png", { mask: unstable });
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await expect(page).toHaveScreenshot("new-task-codex-model-light.png", { mask: unstable });

  await page.getByRole("button", { name: "Start" }).click();
  await expect(page).toHaveURL(/\/tickets\/[^/?]+$/);
  const id = new URL(page.url()).pathname.split("/").pop() ?? "";
  try {
    const facts = page.getByRole("region", { name: "Facts" });
    const value = (label: string) => facts.locator("dt", { hasText: label }).locator("xpath=following-sibling::dd[1]");
    await expect(value("Runtime")).toHaveText("Codex");
    await expect(value("Model")).toHaveText("gpt-5-codex-mini");
    await expectAccessible(page);
  } finally {
    await athena("stop", id).catch(() => {});
  }
});
