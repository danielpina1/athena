// Nodes and Settings, end to end. The Policy mode changed on one screen shows on a second one through Deltas, and a
// stale save there comes back "Changed on another device" with the edit kept; Add a device makes a link whose QR
// screen enrolls a second browser. Every section renders, passes axe and fits 320 px. The Policy ends as it started;
// no Ticket is created (R37). Screenshots mask only what changes between runs: times, counts, the node's queue, live
// Tickets and runtimes, and the enrollment link, its QR code and its countdown.
import type { Page } from "@playwright/test";
import { addAuthenticator, athena, expect, expectAccessible, expectShell, signedInPage, test, liveFeed } from "./harness";
import { create } from "@bufbuild/protobuf";
import { RepoViewSchema } from "../src/gen/athena/v1/state_pb";
import { editLink, runtimeStatus } from "./link";

const SECTIONS = ["general", "github", "repositories", "trust", "notifications", "devices", "prompts", "execution", "system"];

const volatile = (page: Page) => [
  page.locator("aside .font-mono"),
  page.getByRole("region", { name: "Today" }),
  page.locator("time"),
  page.locator("[data-volatile]"),
  // The sidebar's runtime lines: whether Claude Code has a quota reading yet depends on the specs that ran before.
  page.locator("aside").getByRole("list", { name: "Runtimes" }),
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
  // The runtimes' blocks grow with what the specs before left (a quota reading, a why): their states are pinned here
  // so the card keeps one height, and shell.spec.ts shows each state.
  await editLink(page, {
    node: (n) => {
      n.runtimes = [
        runtimeStatus({ name: "claude", ready: true, measured: false }),
        runtimeStatus({ name: "codex", version: "0.160.1", ready: true, tested: true, measured: false }),
      ];
    },
  });
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

const githubRepos = {
  repos: (r: import("../src/gen/athena/v1/state_pb").RepoView[]) => {
    r.splice(
      0,
      r.length,
      create(RepoViewSchema, { name: "acme/web", kind: "github", owed: 3, ceiling: 3, braked: 2, mode: "live" }),
      create(RepoViewSchema, { name: "acme/api", kind: "github", owed: 1, ceiling: 3, braked: 0, mode: "live" }),
    );
  },
};

test("a repository switched to Shadow is saved in the Policy, and the Policy ends as it started", async ({ page }) => {
  await editLink(page, githubRepos);
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await page.goto("/settings/repositories");
  await expect(page.getByText("3 of 3 waiting on you, 2 held back")).toBeVisible();
  const own = page.getByRole("combobox", { name: /Hold work in acme\/web/ });
  await expect(own.locator("option:checked")).toHaveText("3 (default)");
  await expectAccessible(page);
  await themes(page, "settings-repositories");

  const bar = page.getByRole("region", { name: "Save Repositories" });
  try {
    await page.getByRole("combobox", { name: "Mode of acme/web" }).selectOption("shadow");
    await own.selectOption("2");
    await bar.getByRole("button", { name: "Save" }).click();
    await expect(bar).toHaveCount(0);
    await expect.poll(async () => (await athena("policy", "show")).replace(/\s+/g, "")).toContain('"mode":"shadow"');
    expect((await athena("policy", "show")).replace(/\s+/g, "")).toContain('"wipCeiling":2');
  } finally {
    await page.getByRole("combobox", { name: "Mode of acme/web" }).selectOption("live");
    await own.selectOption("");
    if (await bar.isVisible()) await bar.getByRole("button", { name: "Save" }).click();
    await expect(bar).toHaveCount(0);
    await expect.poll(async () => (await athena("policy", "show")).replace(/\s+/g, "")).not.toContain('"shadow"');
  }
});

test("trust shows its classes and the standing rules, and the owner row is locked", async ({ page }) => {
  await editLink(page, {
    policy: (p) => {
      const doc = JSON.parse(p.settingsJson);
      doc.allow = [
        { tool: "Bash", rule: "make test", repo: "acme/web", added: "2026-11-01T10:00:00Z" },
        { tool: "Edit", rule: "docs/**", repo: "acme/api", added: "2026-11-03T10:00:00Z" },
      ];
      p.settingsJson = JSON.stringify(doc);
    },
  });
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await page.goto("/settings/trust");
  await expect(page.getByRole("combobox", { name: "You" })).toBeDisabled();
  await expect(page.getByRole("combobox", { name: "Strangers" })).toHaveValue("restricted");
  const rules = page.getByRole("region", { name: "Standing rules" });
  await expect(rules.getByRole("listitem")).toHaveCount(2);
  await expect(rules.getByRole("listitem").first()).toContainText("Bash · make test in acme/web, added 1 Nov 2026");
  await rules.getByRole("button", { name: /^Remove Edit/ }).click();
  await expect(rules.getByRole("listitem")).toHaveCount(1);
  await page.getByRole("button", { name: "Discard" }).click();
  await expect(rules.getByRole("listitem")).toHaveCount(2);
  await expectAccessible(page);
  await themes(page, "settings-trust");
});
