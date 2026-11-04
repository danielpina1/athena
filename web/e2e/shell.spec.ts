// The shell after a real sign-in, at the three widths: the phone shell under 1024 px (the tablet included), the
// three-pane desktop at 1440; light and dark screenshots on a fixed clock; axe on every screen; the palette.
import { expect, expectAccessible, expectShell, signedInPage, test } from "./harness";

test("the shell renders, light and dark", async ({ page }, testInfo) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await expectShell(page);
  await expect(page.getByRole("heading", { name: "Needs you", level: 1 })).toBeVisible();

  const nav = page.getByRole("navigation", { name: "Main" });
  if (testInfo.project.name === "desktop") {
    await expect(nav.getByRole("link")).toHaveText(["Needs you", "Tickets", "New", "Graph", "Routing", "Nodes", "Settings"]);
    await expect(page.locator('main [data-pane="list"]')).toBeVisible();
    await expect(page.locator('main [data-pane="detail"]')).toBeVisible();
    await expect(page.getByRole("region", { name: "Today" })).toBeVisible();
  } else {
    await expect(nav.getByRole("link")).toHaveCount(5);
    await expect(page.getByRole("region", { name: "Today" })).toHaveCount(0);
  }
  await expectAccessible(page);
  await page.evaluate(() => document.fonts.ready);
  await expect(page).toHaveScreenshot("shell-light.png");

  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectAccessible(page);
  await expect(page).toHaveScreenshot("shell-dark.png");
});

test("the palette opens and goes to a route", async ({ page }, testInfo) => {
  await signedInPage(page);
  if (testInfo.project.name === "desktop") await page.keyboard.press("ControlOrMeta+k");
  else await page.getByRole("button", { name: "Search and commands" }).click();
  const palette = page.getByRole("dialog", { name: "Command palette" });
  await expect(palette).toBeVisible();
  await expectAccessible(page);
  await palette.getByRole("combobox").fill("nodes");
  await palette.getByRole("option", { name: "Nodes" }).click();
  await expect(page).toHaveURL(/\/nodes$/);
  await expect(page.getByRole("heading", { name: "Nodes", level: 1 })).toBeVisible();
  await expectAccessible(page);
});

test("an unknown path shows the 404 in the shell", async ({ page }) => {
  await signedInPage(page);
  await page.goto("/no/such/page");
  await expectShell(page);
  await expect(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
  await expectAccessible(page);
});
