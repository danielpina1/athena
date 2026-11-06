// The shell after a real sign-in, at the three widths: the phone shell under 1024 px (the tablet included), the
// three-pane desktop at 1440; light and dark screenshots on a fixed clock; axe on every screen; the palette; the node
// card's runtimes and quota in each of their states, and the sidebar's line per runtime.
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { expect, expectAccessible, expectShell, liveFeed, signedInPage, test } from "./harness";
import { editLink, fixedNode, runtimeStatus, type LinkEdits } from "./link";

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

test("a test can sign in on two screens and both stay signed in", async ({ page, browser, baseURL }, testInfo) => {
  test.skip(testInfo.project.name !== "desktop", "a harness check: one width is enough");
  await signedInPage(page);
  const other = await browser.newContext({ baseURL });
  try {
    const second = await signedInPage(await other.newPage());
    await expectShell(second);
    // The first screen's passkey was kept: its session still works after a reload.
    await page.reload();
    await expectShell(page);
  } finally {
    await other.close();
  }
});

test("the node card shows each runtime's quota in every state, light and dark", async ({ page }, testInfo) => {
  const now = new Date("2026-11-04T10:00:00").getTime();
  const at = (ms: number) => timestampFromMs(ms);
  const MIN = 60_000;
  const resets = (h: number, m = 0) => at(new Date(2026, 10, 4, h, m).getTime());
  const week = at(new Date(2026, 10, 9, 8, 0).getTime());
  const codex = { name: "codex", version: "0.160.1", ready: true, tested: true, measured: true, readAt: at(now - 2 * MIN) };
  const claude = { name: "claude", ready: true, tested: false, measured: true, readAt: at(now - 2 * MIN) };
  // The states the shared server cannot reach by itself, each a fixed node: never measured and normal; amber at 85%,
  // an untested version and a limit; a stale reading and a runtime that is not ready.
  const states = {
    measured: [
      runtimeStatus({ ...claude, measured: false, readAt: undefined }),
      runtimeStatus({ ...codex, windows: [{ name: "5h", used: 0.42, resetsAt: resets(13, 30) }, { name: "7d", used: 0.12, resetsAt: week }] }),
    ],
    limited: [
      runtimeStatus({
        ...claude,
        limitedUntil: resets(14, 0),
        windows: [{ name: "5h", used: 1, resetsAt: resets(14, 0) }, { name: "7d", used: 0.61, resetsAt: week }],
      }),
      runtimeStatus({
        ...codex,
        version: "0.161.0",
        tested: false,
        why: "newer than the verified 0.160.1",
        windows: [{ name: "5h", used: 0.88, resetsAt: resets(12, 15) }, { name: "7d", used: 0.4, resetsAt: week }],
      }),
    ],
    stale: [
      runtimeStatus({ ...claude, ready: false, why: "Claude Code is not installed on this node", measured: false, readAt: undefined }),
      runtimeStatus({
        ...codex,
        readAt: at(now - 25 * MIN),
        windows: [{ name: "5h", used: 0.3, resetsAt: resets(9, 50), stale: true }, { name: "7d", used: 0.2, resetsAt: week }],
      }),
    ],
  };
  await page.clock.setFixedTime(now);
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  const edits: LinkEdits = {};
  await editLink(page, edits);
  await signedInPage(page);
  for (const [name, runtimes] of Object.entries(states)) {
    edits.node = fixedNode(runtimes);
    await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
    await page.goto("/nodes");
    const card = page.getByRole("region", { name: "studio" });
    const blocks = card.getByRole("list", { name: "Runtimes" });
    await expect(blocks.getByRole("region", { name: "Codex" })).toBeVisible();
    if (name === "measured") {
      await expect(blocks.getByRole("region", { name: "Claude Code" })).toContainText("Not measured yet");
      await expect(blocks.getByRole("meter", { name: "Codex 5h used" })).toHaveAttribute("aria-valuenow", "42");
      await expect(blocks.getByRole("region", { name: "Codex" })).toContainText("resets 13:30");
    } else if (name === "limited") {
      await expect(blocks.getByRole("region", { name: "Claude Code" })).toContainText("Limited until 14:00");
      await expect(blocks.getByRole("region", { name: "Codex" })).toContainText("untested");
      await expect(blocks.getByText("88%")).toHaveClass(/text-warning/);
    } else {
      await expect(blocks.getByRole("region", { name: "Codex" })).toContainText("as of 25 min ago");
      await expect(blocks.getByRole("region", { name: "Claude Code" })).toContainText("Not ready");
    }
    if (testInfo.project.name === "desktop") {
      // The sidebar's node: one line per runtime, linking to Nodes.
      const lines = page.locator("aside").getByRole("list", { name: "Runtimes" });
      await expect(lines.getByRole("link")).toHaveCount(2);
      if (name === "measured") await expect(lines.getByRole("link", { name: "Codex 42% · 5h" })).toHaveAttribute("href", "/nodes");
      if (name === "limited") await expect(lines.getByRole("link", { name: "Claude Code limited · 14:00" })).toBeVisible();
    }
    await expectAccessible(page);
    // The health checks' times and the day's counts depend on the specs that ran before.
    const unstable = [page.locator("time"), page.getByRole("region", { name: "Today" }), page.getByRole("region", { name: "Health" })];
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot(`nodes-${name}-light.png`, { mask: [...unstable, ...liveFeed(page)] });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot(`nodes-${name}-dark.png`, { mask: [...unstable, ...liveFeed(page)] });
  }
});
