// The installable app: the manifest passes Chromium's installability check, the service worker serves the shell
// offline (with the "unreachable" banner), a push delivered through CDP shows its notification, and a new sw.js
// waits behind "A new version is ready — Reload". No Ticket is created here (R37 holds trivially).
import { type BrowserContext, chromium, type Page } from "@playwright/test";
import { expect, expectAccessible, expectShell, signedInPage, test } from "./harness";


/** The service worker is active and controls the page. */
async function controlled(page: Page): Promise<void> {
  await page.evaluate(() => navigator.serviceWorker.ready.then(() => undefined));
  await page.waitForFunction(() => navigator.serviceWorker.controller !== null);
}

test("the manifest is valid and the app installable", async ({ page }) => {
  await page.goto("/");
  await controlled(page);
  const manifest = await (await page.request.get("/manifest.webmanifest")).json();
  expect(manifest).toMatchObject({
    name: "Athena",
    short_name: "Athena",
    display: "standalone",
    start_url: "/",
    scope: "/",
    background_color: "#FFFFFF",
    theme_color: "#FFFFFF",
  });
  for (const icon of manifest.icons as { src: string }[]) {
    const res = await page.request.get(icon.src);
    expect(res.headers()["content-type"], icon.src).toBe("image/png");
  }
  const sw = await page.request.get("/sw.js");
  expect(sw.headers()["cache-control"]).toBe("no-cache");

  const cdp = await page.context().newCDPSession(page);
  const { installabilityErrors } = await cdp.send("Page.getInstallabilityErrors");
  expect(installabilityErrors).toEqual([]);
});

test("offline reload shows the shell with the unreachable banner", async ({ page, context }) => {
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await controlled(page);

  await context.setOffline(true);
  await page.reload();
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Needs you", level: 1 })).toBeVisible();
  await expect(page.getByText("Control server unreachable — retrying")).toBeVisible();
  // A deep link opens offline too: the routes are the app's.
  await page.goto("/tickets");
  await expect(page.getByRole("heading", { name: "Tickets", level: 1 }).first()).toBeVisible();
  await page.goto("/");
  await expect(page.getByText("Control server unreachable — retrying")).toBeVisible();
  // No Snapshot yet: nothing claims "nothing" or "0".
  await expect(page.getByText("Waiting for the server.").first()).toBeVisible();
  await expect(page.getByText("Nothing needs you.")).toHaveCount(0);
  await expectAccessible(page);
  await page.evaluate(() => document.fonts.ready);
  await expect(page).toHaveScreenshot("offline-light.png");

  // The network is back: the link opens and the banner goes.
  await context.setOffline(false);
  await expectShell(page);
  await expect(page.getByText("Control server unreachable — retrying")).toHaveCount(0);
  await expect(page.getByText("Nothing needs you.")).toBeVisible();
  await expect(page.getByText("Waiting for the server.")).toHaveCount(0);
});

/** The service worker's registration id, from CDP. */
async function registrationId(context: BrowserContext, page: Page) {
  const cdp = await context.newCDPSession(page);
  const id = new Promise<string>((resolve) =>
    cdp.on("ServiceWorker.workerRegistrationUpdated", ({ registrations }) => {
      const reg = registrations.find((r) => r.scopeURL === new URL("/", page.url()).href && !r.isDeleted);
      if (reg) resolve(reg.registrationId);
    }),
  );
  await cdp.send("ServiceWorker.enable");
  return { cdp, id: await id };
}

/**
 * Runs fn in a context of full Chromium (new headless) with notifications allowed: the headless shell the other tests
 * use always denies notifications, and full Chromium's contexts are incognito, which fails the installability check.
 */
async function withNotifications(baseURL: string | undefined, fn: (context: BrowserContext, page: Page) => Promise<void>) {
  const browser = await chromium.launch({ channel: "chromium" });
  try {
    const context = await browser.newContext({ baseURL, permissions: ["notifications"] });
    await fn(context, await context.newPage());
  } finally {
    await browser.close();
  }
}

test("a push shows a notification", async ({ baseURL }, testInfo) => {
  test.skip(testInfo.project.name !== "desktop", "the service worker is the same at every width");
  await withNotifications(baseURL, async (context, page) => {
    await page.goto("/");
    await controlled(page);
    const { cdp, id } = await registrationId(context, page);

    // Plan 3's notification JSON (internal/web/notify.go), twice for one Ticket: the second replaces the first.
    for (const body of ["Fix the login needs approval", "Fix the login needs your answer"]) {
      await cdp.send("ServiceWorker.deliverPushMessage", {
        origin: new URL(page.url()).origin,
        registrationId: id,
        data: JSON.stringify({ title: "Athena", body, url: "/tickets/T-9", tag: "T-9" }),
      });
    }
    await expect
      .poll(() =>
        page.evaluate(async () =>
          (await (await navigator.serviceWorker.ready).getNotifications()).map((n) => ({
            title: n.title,
            body: n.body,
            tag: n.tag,
            data: n.data,
          })),
        ),
      )
      .toEqual([
        { title: "Athena", body: "Fix the login needs your answer", tag: "T-9", data: { url: "/tickets/T-9" } },
      ]);
  });
});

test("Settings has the Notifications control", async ({ baseURL }, testInfo) => {
  await withNotifications(baseURL, async (_, page) => {
    await page.setViewportSize(testInfo.project.use.viewport!);
    await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
    await signedInPage(page);
    await page.goto("/settings/notifications");
    const control = page.getByRole("radiogroup", { name: "Notifications" });
    await expect(control).toBeVisible();
    // Allowed but not subscribed on this device: off, and the owner can turn it on.
    await expect(control.getByRole("radio", { name: "Off" })).toHaveAttribute("aria-checked", "true");
    await expect(control.getByRole("radio", { name: "On" })).toBeEnabled();
    await expectAccessible(page);
  });
});

test("a new version waits behind the update banner and Reload loads it", async ({ page }) => {
  // A changed /sw.js cannot be served here (the server embeds the build, and neither Playwright nor CDP can intercept
  // a worker's script fetch), so this page starts on an "older" worker registered as /sw.js?old, until the test sets
  // e2e.next in the tab's sessionStorage. Then the app's /sw.js is the new version, as after a server update.
  await page.addInitScript(() => {
    const sw = navigator.serviceWorker;
    const register = sw.register.bind(sw);
    sw.register = (url, opts) => register(sessionStorage.getItem("e2e.next") ? url : `${String(url)}?old`, opts);
  });
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await controlled(page);
  expect(await page.evaluate(() => navigator.serviceWorker.controller?.scriptURL)).toMatch(/\/sw\.js\?old$/);

  // The browser finds the new worker; it installs and waits.
  await page.evaluate(async () => {
    sessionStorage.setItem("e2e.next", "1");
    await navigator.serviceWorker.register("/sw.js", { scope: "/" });
  });
  const banner = page.getByRole("status").filter({ hasText: "A new version is ready" });
  await expect(banner).toHaveText("A new version is ready — Reload");
  // The old app keeps running until the owner chooses.
  await expect(page.getByRole("heading", { name: "Needs you", level: 1 })).toBeVisible();
  await expectAccessible(page);
  await page.evaluate(() => document.fonts.ready);
  await expect(page).toHaveScreenshot("update-banner.png");

  await Promise.all([page.waitForEvent("load"), banner.getByRole("button", { name: "Reload" }).click()]);
  await expectShell(page);
  await expect(page.getByText("A new version is ready")).toHaveCount(0);
  const now = await page.evaluate(async () => {
    const reg = await navigator.serviceWorker.getRegistration();
    return { active: reg?.active?.scriptURL, waiting: reg?.waiting ?? null, controls: navigator.serviceWorker.controller?.scriptURL };
  });
  const next = expect.stringMatching(/\/sw\.js$/);
  expect(now).toEqual({ active: next, waiting: null, controls: next });
});
