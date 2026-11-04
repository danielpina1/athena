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
    await expect(page.getByRole("region", { name: "Today" })).toBeVisible();
    // Needs you has no list pane (its cards span both); Tickets has the list and the detail.
    await expect(page.locator('main [data-pane="full"]')).toBeVisible();
    await nav.getByRole("link", { name: "Tickets" }).click();
    // The sidebar is 240 px and the list pane 400 px; the detail pane takes the rest.
    expect((await page.locator("aside").boundingBox())?.width).toBe(240);
    expect((await page.locator('main [data-pane="list"]').boundingBox())?.width).toBe(400);
    expect((await page.locator('main [data-pane="detail"]').boundingBox())?.width).toBe(1440 - 240 - 400);
    await nav.getByRole("link", { name: "Needs you" }).click();
    await expect(page.getByRole("heading", { name: "Needs you", level: 1 })).toBeVisible();
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

test("no horizontal scroll at 320 px", async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== "phone", "one narrow phone is enough");
  await page.setViewportSize({ width: 320, height: 640 });
  await signedInPage(page);
  for (const path of ["/", "/tickets", "/tickets/T-9999", "/new", "/graph", "/settings", "/routing", "/nodes", "/nowhere"]) {
    await page.goto(path);
    await expectShell(page);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    const { scrollWidth, clientWidth } = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }));
    expect(scrollWidth, `${path} scrolls sideways`).toBeLessThanOrEqual(clientWidth);
    await expectAccessible(page);
  }
});
