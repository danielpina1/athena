// Nodes and Settings, end to end. The Policy mode changed on one screen shows on a second one through Deltas, and a
// stale save there comes back "Changed on another device" with the edit kept; Add a device makes a link whose QR
// screen enrolls a second browser. Every section renders, passes axe and fits 320 px. The Policy ends as it started;
// no Ticket is created (R37). Screenshots mask only what changes between runs: times, counts, the node's queue and
// live Tickets, and the enrollment link, its QR code and its countdown.
import type { Page } from "@playwright/test";
import { addAuthenticator, expect, expectAccessible, expectShell, signedInPage, test, liveFeed } from "./harness";

const SECTIONS = ["general", "github", "notifications", "devices", "prompts", "execution", "system"];

const volatile = (page: Page) => [
  page.locator("aside .font-mono"),
  page.getByRole("region", { name: "Today" }),
  page.locator("time"),
  page.locator("[data-volatile]"),
];

async function shot(page: Page, name: string) {
  await page.evaluate(() => document.fonts.ready);
  await expect(page).toHaveScreenshot(name, { mask: [...volatile(page), ...liveFeed(page)] });
}

async function themes(page: Page, name: string) {
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await shot(page, `${name}-light.png`);
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectAccessible(page);
  await shot(page, `${name}-dark.png`);
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
}

test("nodes shows the node and its health", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await page.goto("/nodes");
  await expect(page.getByRole("heading", { name: "Nodes", level: 1 })).toBeVisible();
  await expect(page.getByText(/ of 4 slots in use$/)).toBeVisible();
  await expect(page.getByRole("meter", { name: "Slots in use" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Health" })).toBeVisible();
  await expectAccessible(page);
  await themes(page, "nodes");
});

test("every settings section renders and is accessible", async ({ page }, testInfo) => {
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const phone = testInfo.project.name !== "desktop";
  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "Settings", level: 1 })).toBeVisible();
  if (phone) {
    // The list first; a section opens on tap.
    await expect(page.getByRole("navigation", { name: "Settings sections" })).toBeVisible();
    await expectAccessible(page);
    await themes(page, "settings-list");
  } else {
    await expect(page.getByRole("heading", { name: "General", level: 2 })).toBeVisible();
  }
  for (const section of SECTIONS) {
    await page.goto(`/settings/${section}`);
    await expect(page.getByRole("heading", { level: phone ? 1 : 2 }).last()).toBeVisible();
    await expect(page.getByText("Waiting for the server.")).toHaveCount(0);
    await expectAccessible(page);
  }
  await page.goto("/settings/general");
  await expect(page.getByRole("radio", { name: /^Observe/ })).toBeChecked();
  await themes(page, "settings-general");
});

test("settings fit 320 px", async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== "phone", "one narrow phone is enough");
  await page.setViewportSize({ width: 320, height: 640 });
  await signedInPage(page);
  for (const path of ["/nodes", "/settings", ...SECTIONS.map((s) => `/settings/${s}`)]) {
    await page.goto(path);
    await expectShell(page);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    const { scrollWidth, clientWidth } = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }));
    expect(scrollWidth, `${path} scrolls sideways`).toBeLessThanOrEqual(clientWidth);
  }
});

const MODES = ["Off", "Observe", "Auto"] as const;

async function checkedMode(page: Page): Promise<(typeof MODES)[number]> {
  for (const m of MODES) if (await page.getByRole("radio", { name: new RegExp(`^${m}`) }).isChecked()) return m;
  throw new Error("no Policy mode is checked");
}

const mode = (page: Page, m: string) => page.getByRole("radio", { name: new RegExp(`^${m}`) });
const saveBar = (page: Page) => page.getByRole("region", { name: "Save General" });

test("a Policy mode change shows on a second screen, and a stale save there keeps its edit", async ({
  page,
  browser,
  baseURL,
}, testInfo) => {
  test.skip(testInfo.project.name !== "desktop", "two screens at one width are enough");
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const other = await browser.newContext({ baseURL, viewport: { width: 1440, height: 900 } });
  try {
    const second = await signedInPage(await other.newPage());
    await second.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
    await page.goto("/settings/general");
    await second.goto("/settings/general");
    const start = await checkedMode(page);
    expect(await checkedMode(second)).toBe(start);
    const [x, y] = MODES.filter((m) => m !== start);

    // One screen saves; the other shows it at once, from the Delta, with no reload.
    await mode(page, x).check();
    await saveBar(page).getByRole("button", { name: "Save" }).click();
    await expect(saveBar(page)).toHaveCount(0);
    await expect(mode(second, x)).toBeChecked();

    // The second screen edits from that revision while the first saves again: its save is stale.
    await mode(second, y).check();
    await mode(page, start).check();
    await saveBar(page).getByRole("button", { name: "Save" }).click();
    await expect(saveBar(page)).toHaveCount(0);
    await saveBar(second).getByRole("button", { name: "Save" }).click();
    await expect(saveBar(second).getByText("Changed on another device")).toBeVisible();
    await expect(mode(second, y)).toBeChecked();
    await expect(saveBar(second).getByRole("button", { name: "Overwrite" })).toBeVisible();
    await expectAccessible(second);
    await second.mouse.move(0, 0);
    await shot(second, "policy-conflict.png");

    // Reload drops the edit and shows what the first screen saved: the Policy is back where it started.
    await saveBar(second).getByRole("button", { name: "Reload" }).click();
    await expect(mode(second, start)).toBeChecked();
    await expect(saveBar(second)).toHaveCount(0);
  } finally {
    await other.close();
  }
});

test("add a device and enroll it through the QR link", async ({ page, browser, baseURL }, testInfo) => {
  test.skip(testInfo.project.name === "tablet", "the phone and the desktop are enough");
  const logs: string[] = [];
  page.on("console", (m) => logs.push(m.text()));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await page.goto("/settings/devices");
  const passkeys = page.getByRole("region", { name: "Passkeys" }).getByRole("listitem");
  await expect(passkeys.first()).toBeVisible();
  const before = await passkeys.count();
  await expect(passkeys.filter({ hasText: "This device" })).toHaveCount(1);

  await page.getByRole("button", { name: "Add a device" }).click();
  const qr = page.getByRole("img", { name: "QR code of the enrollment link" });
  await expect(qr).toBeVisible();
  await expect(page.getByText(/^Expires in \d+:\d\d$/)).toBeVisible();
  const link = (await page.locator('[data-volatile="enroll-link"]').innerText()).trim();
  expect(link).toMatch(/\/enroll#.+/);
  const token = link.slice(link.indexOf("#") + 1);
  // The token is shown, never put in this page's address, history or console.
  expect(page.url()).not.toContain(token);
  expect(await page.evaluate(() => JSON.stringify(history.state))).not.toContain(token);
  await expectAccessible(page);
  await shot(page, "add-device.png");

  // The second device opens the link (what scanning the code does) and enrolls its own passkey.
  const other = await browser.newContext({ baseURL });
  try {
    const second = await other.newPage();
    await addAuthenticator(second);
    await second.goto(link);
    await second.getByRole("textbox", { name: "Device name" }).fill("Second screen");
    await second.getByRole("button", { name: "Create passkey" }).click();
    await expect(second).toHaveURL(/\/$/);
    await expectShell(second);
  } finally {
    await other.close();
  }

  await page.getByRole("button", { name: "Done" }).click();
  await expect(passkeys).toHaveCount(before + 1);
  await expect(passkeys.filter({ hasText: "Second screen" })).toHaveCount(1);
  for (const line of logs) expect(line).not.toContain(token);
});
