// New task, end to end: the form with its repository picker open (free entry of owner/name), then a task without a
// repository started on the page, landing on the new Ticket's detail. The Ticket is stopped at the end (R37).
import { athena, expect, expectAccessible, signedInPage, test, liveFeed } from "./harness";

test("a no-repo task starts from the form and lands on its detail, light and dark", async ({ page }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await page.goto("/new");
  const text = page.getByRole("textbox", { name: "What should Athena do?" });
  await expect(text).toBeVisible();
  const repo = page.getByRole("combobox", { name: "Repository" });
  await expect(repo).toHaveAttribute("placeholder", "No repository");

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
