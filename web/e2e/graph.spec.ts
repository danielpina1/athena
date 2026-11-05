// The task graph, end to end: six Tickets across two repositories and none, in their lanes (four need you, two wait
// for a slot); a node opens its Ticket
// (the detail pane on the desktop, a navigation on the phone); the list fallback has every node; axe passes; and no
// title moves the page sideways. The e2e server has no GitHub source, so the two repository lanes are local git
// directories (LOCAL · api, LOCAL · web) and there are no Threads, hence no thread edges here (layout.test.ts and
// Graph.test.tsx cover them). Every Ticket is stopped at the end (R37); each runs the default fakeclaude fixture, a
// turn that never ends, so its session can report.
//
// Screenshots: x is creation time, which the browser cannot pin, so the page clock is set a day ahead: at a day's
// age the seconds between these Tickets are well under the minimum spacing, and the nodes sit in creation order.
// "Needs you only" keeps the earlier specs' (ended) Tickets out. Only ids and times are masked.
import { execFile } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import type { Page } from "@playwright/test";
import { athena, expect, expectAccessible, signedInPage, test, liveFeed } from "./harness";

const run = promisify(execFile);
const git = (cwd: string, ...args: string[]) =>
  run("git", ["-c", "user.name=Athena E2E", "-c", "user.email=e2e@example.com", ...args], { cwd, encoding: "utf8" });

async function gitRepo(dir: string) {
  mkdirSync(dir);
  await git(dir, "init", "-q", "-b", "main");
  writeFileSync(join(dir, "README.md"), "# Demo\n");
  await git(dir, "add", ".");
  await git(dir, "commit", "-q", "-m", "first");
}

/** Longer than a node holds (the server keeps a title's first 80 characters). */
const LONG = "Untangle the release pipeline so that every stage reports its own failures";

const node = (page: Page, id: string) => page.locator(`svg [data-node="${id}"]`);

/** What differs between runs: the Tickets' ids and every time shown. */
const volatile = (page: Page) => [
  page.locator("svg [data-node-text] text.font-mono"),
  // Not the phone's visually hidden copy of the list: it is not in the picture.
  page.locator("[data-list-node] .font-mono:not([data-hidden-list] *)"),
  page.locator("[data-volatile]"),
  page.locator("time"),
  page.locator("aside .font-mono"),
  page.getByRole("region", { name: "Today" }),
];

async function expectNoSideScroll(page: Page) {
  const { scroll, client } = await page.evaluate(() => ({
    scroll: document.documentElement.scrollWidth,
    client: document.documentElement.clientWidth,
  }));
  expect(scroll, `page scrollWidth ${scroll} > ${client}`).toBeLessThanOrEqual(client);
}

test("six Tickets in lanes by repository, opened from the graph and the list, light and dark", async ({ page, session }, testInfo) => {
  test.setTimeout(120_000);
  await page.clock.setFixedTime(new Date(Date.now() + 86_400_000));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);

  const parent = mkdtempSync(join(tmpdir(), "athena-e2e-graph-"));
  const api = join(parent, "api");
  const web = join(parent, "web");
  await gitRepo(api);
  await gitRepo(web);
  const ids: string[] = [];
  try {
    // The node runs four sessions at once: the first four start and ask the owner something (they need you); the
    // last two wait for a slot.
    const specs: Array<[string, string?]> = [
      ["Refactor the auth client", api],
      [LONG, web],
      ["Summarize the incidents"],
      ["Fix the flaky auth test", api],
      ["Upgrade the bundler", web],
      ["Draft the postmortem"],
    ];
    for (const [title, dir] of specs) ids.push(await athena("task", title, ...(dir ? ["--dir", dir] : [])));
    for (const id of ids.slice(0, 4)) await session.report(id, "waiting", "A question", "Go on?");

    await page.goto("/graph");
    for (const id of ids) await expect(node(page, id)).toBeAttached();
    const lanes = await page.locator("svg [data-lane]").evaluateAll((ls) => ls.map((l) => l.getAttribute("data-label")));
    expect(lanes).toEqual(expect.arrayContaining(["LOCAL · api", "LOCAL · web", "No repository"]));
    expect(lanes.at(-1)).toBe("No repository");
    await expect(node(page, ids[0])).toHaveAttribute("aria-label", `Refactor the auth client, ${ids[0]}, Needs your answer`);
    await expect(node(page, ids[0])).toHaveAttribute("data-look", "needs-you");
    for (const id of ids.slice(4)) await expect(node(page, id)).not.toHaveAttribute("data-look", "needs-you");
    // The long title is cut inside its node, in full in its name.
    await expect(page.locator(`svg [data-node-text="${ids[1]}"] text`).first()).toHaveText(/^Untangle the release.*…$/);
    await expect(node(page, ids[1])).toHaveAttribute("aria-label", `${LONG}, ${ids[1]}, Needs your answer`);
    await expectNoSideScroll(page);

    await page.getByRole("button", { name: "Needs you only" }).click();
    await expect(page).toHaveURL(/needsYou=1/);
    await expect(page.locator("svg [data-node]")).toHaveCount(4);
    await expect(page.locator("svg [data-lane]")).toHaveCount(3);
    await expectNoSideScroll(page);
    await expectAccessible(page);
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot("graph-light.png", { mask: [...volatile(page), ...liveFeed(page)] });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("graph-dark.png", { mask: [...volatile(page), ...liveFeed(page)] });

    // The list fallback: every node, its stage, its lane.
    await page.getByRole("radio", { name: "List" }).click();
    await expect(page).toHaveURL(/view=list/);
    const list = page.getByRole("region", { name: "Tickets as a list" });
    for (const id of ids.slice(0, 4)) await expect(list.locator(`[data-list-node="${id}"]`)).toContainText("Needs your answer");
    await expect(list.getByRole("heading", { name: "LOCAL · api" })).toBeVisible();
    await expectNoSideScroll(page);
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("graph-list-dark.png", { mask: [...volatile(page), ...liveFeed(page)] });
    await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("graph-list-light.png", { mask: [...volatile(page), ...liveFeed(page)] });
    await page.getByRole("radio", { name: "Graph" }).click();

    // A node opens its Ticket: beside the graph on the desktop, as a page on the phone.
    if (testInfo.project.name === "desktop") {
      await node(page, ids[3]).click();
      const pane = page.getByRole("complementary", { name: "Ticket" });
      await expect(pane.getByRole("heading", { name: "Fix the flaky auth test", level: 1 })).toBeVisible();
      await expect(node(page, ids[0])).toBeVisible();
      await expectAccessible(page);
      // Esc closes it and focus is back on the node; Enter opens it again; Close does the same as Esc.
      await page.keyboard.press("Escape");
      await expect(pane).toHaveCount(0);
      await expect(node(page, ids[3])).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(pane.getByRole("heading", { level: 1 })).toHaveText("Fix the flaky auth test");
      await pane.getByRole("button", { name: "Close" }).click();
      await expect(pane).toHaveCount(0);
      await expect(node(page, ids[3])).toBeFocused();
    } else {
      // At 375×812 the scrubber and Replay are above the bottom navigation on load, with nothing to scroll.
      await page.setViewportSize({ width: 375, height: 812 });
      await page.reload();
      await expect(node(page, ids[3])).toBeVisible();
      const replayAboveNav = async (size: string) => {
        const replay = await page.getByRole("button", { name: "Replay" }).boundingBox();
        const nav = await page.getByRole("navigation", { name: "Main" }).boundingBox();
        expect(
          replay && nav && replay.y + replay.height <= nav.y,
          `${size}: Replay ${JSON.stringify(replay)} above nav ${JSON.stringify(nav)}`,
        ).toBe(true);
      };
      await replayAboveNav("375×812");
      const scroll = await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight);
      expect(scroll).toBe(0);
      await expect(page).toHaveScreenshot("graph-375-light.png", { mask: [...volatile(page), ...liveFeed(page)] });
      // A short phone, and one on its side: the page scrolls, and the scrubber stays above the navigation.
      for (const [width, height] of [
        [375, 600],
        [812, 375],
      ]) {
        await page.setViewportSize({ width, height });
        await replayAboveNav(`${width}×${height}`);
      }

      await node(page, ids[3]).click();
      await expect(page).toHaveURL(new RegExp(`/tickets/${ids[3]}$`));
      await expect(page.getByRole("heading", { name: "Fix the flaky auth test", level: 1 })).toBeVisible();
    }
  } finally {
    for (const id of ids) await athena("stop", id).catch(() => {});
    // The server may still be removing the stopped Tickets' worktrees.
    rmSync(parent, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 });
  }
});
