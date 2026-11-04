// Tickets, end to end: the list with its filters, the detail's live timeline and inline question, the Changes of a
// Ticket in a temp git repository (the worker's edit is made here, as the fake session does not write), the Files of
// one without a repository, and hostile content (a 2,000-character title, RTL, ANSI, a 3,000-character diff line)
// that must never move the page sideways. Every Ticket a test opens is stopped at its end (R37).
import { execFile } from "node:child_process";
import { mkdirSync, mkdtempSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import type { Page } from "@playwright/test";
import { athena, expect, expectAccessible, signedInPage, test } from "./harness";

const run = promisify(execFile);
const git = (cwd: string, ...args: string[]) =>
  run("git", ["-c", "user.name=Athena E2E", "-c", "user.email=e2e@example.com", ...args], { cwd, encoding: "utf8" });

type Repo = { dir: string; id: string; worktree: string };

/**
 * Opens a Ticket in a fresh git repository, waits for its worktree and commits `edits` there on its branch, as the
 * worker would. The temp repository is the caller's to remove.
 */
async function ticketWithEdit(text: string, edits: Record<string, string>): Promise<Repo> {
  const dir = mkdtempSync(join(tmpdir(), "athena-e2e-repo-"));
  await git(dir, "init", "-q", "-b", "main");
  writeFileSync(join(dir, "README.md"), "# Demo\n");
  await git(dir, "add", ".");
  await git(dir, "commit", "-q", "-m", "first");
  const id = await athena("task", text, "--dir", dir);
  const branch = `refs/heads/athena/${id.toLowerCase()}`;
  let worktree = "";
  await expect
    .poll(
      async () => {
        const list = (await git(dir, "worktree", "list", "--porcelain")).stdout;
        const block = list.split("\n\n").find((b) => b.includes(`branch ${branch}`));
        worktree = block?.match(/^worktree (.+)$/m)?.[1] ?? "";
        return worktree;
      },
      { message: `the worktree of ${id}`, timeout: 20_000 },
    )
    .not.toBe("");
  for (const [name, body] of Object.entries(edits)) writeFileSync(join(worktree, name), body);
  await git(worktree, "add", ".");
  await git(worktree, "commit", "-q", "-m", "the worker's edit");
  return { dir, id, worktree };
}

async function finish(...tickets: Array<string | Repo>) {
  for (const t of tickets) {
    await athena("stop", typeof t === "string" ? t : t.id).catch(() => {});
    if (typeof t !== "string") rmSync(t.dir, { recursive: true, force: true });
  }
}

/** The page, the Ticket's column and the detail pane (desktop) all fit their width. */
async function expectNoSideScroll(page: Page, opts: { column?: boolean } = {}) {
  const m = await page.evaluate(() => {
    const size = (el: Element | null) => (el ? { scroll: el.scrollWidth, client: el.clientWidth } : undefined);
    return {
      page: size(document.documentElement),
      column: size(document.querySelector('[data-testid="ticket-detail"]')),
      pane: size(document.querySelector('[data-pane="detail"]')),
    };
  });
  for (const [name, s] of Object.entries(m)) {
    if (s) expect(s.scroll, `${name} scrollWidth ${s.scroll} > clientWidth ${s.client}`).toBeLessThanOrEqual(s.client);
  }
  if (opts.column ?? true) expect(m.column).toBeTruthy();
}

test("Changes shows the worker's edit, three panes, light and dark", async ({ page }, testInfo) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const title = "Add a greeting to the docs";
  const repo = await ticketWithEdit(title, { "GREETING.md": "# Hello\n\nWelcome aboard.\n" });
  try {
    // The filters keep the list to this Ticket (the others of the shared server are ended), so the screenshot does
    // not depend on the specs and projects that ran before.
    await page.goto(`/tickets/${repo.id}?tab=changes&stage=working&q=greeting`);
    await expect(page.getByRole("heading", { name: title, level: 1 })).toBeVisible();
    await expect(page.getByRole("tab", { name: "Changes" })).toHaveAttribute("aria-selected", "true");
    const files = page.getByRole("list", { name: "Changed files" });
    await expect(files).toContainText("GREETING.md");
    await expect(files).toContainText("+3");
    await expect(page.getByText("Welcome aboard.")).toBeVisible();
    await expect(page.getByRole("button", { name: "Interrupt" })).toBeVisible();

    if (testInfo.project.name === "desktop") {
      // Sidebar, list and detail side by side, the open Ticket marked in the list.
      await expect(page.locator("aside")).toBeVisible();
      await expect(page.locator('main [data-pane="list"] [aria-current="page"]')).toContainText(title);
      await expect(page.locator('main [data-pane="detail"]')).toContainText(repo.id);
    }
    await expectNoSideScroll(page);
    await expectAccessible(page);

    // What differs from run to run: times, the Ticket's id and directory, the commit the diff is against, the
    // node's load and the day's counts. The diff itself and the layout are pinned.
    const unstable = [
      page.locator("time"),
      page.locator("aside .font-mono"),
      page.getByRole("region", { name: "Today" }),
      page.getByRole("region", { name: "Facts" }),
      page.getByTestId("ticket-detail").locator("header .font-mono"),
      page.getByText(/ against [0-9a-f]{7}$/),
      page.locator("[data-ticket-row] .font-mono"),
    ];
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot("ticket-changes-light.png", { mask: unstable });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("ticket-changes-dark.png", { mask: unstable });
  } finally {
    await finish(repo);
  }
});

test("the detail follows its Ticket live", async ({ page, session }) => {
  await signedInPage(page);
  const ticket = await athena("task", "Choose the cache layer");
  try {
    await page.goto(`/tickets/${ticket}`);
    await expect(page.getByRole("heading", { name: "Choose the cache layer", level: 1 })).toBeVisible();
    await expect(page.getByRole("tab", { name: "Files" })).toBeVisible();

    // The Report arrives while the page is open: its question is inline and its text is on the timeline.
    await session.report(ticket, "waiting", "Two caches fit", "Redis or in-process?");
    const question = page.getByRole("group", { name: /Choose the cache layer/ });
    await expect(question.getByText("Redis or in-process?")).toBeVisible();
    await expect(page.getByText("Two caches fit")).toBeVisible();
    await expectAccessible(page);

    const box = question.getByRole("textbox", { name: "Your answer" });
    await box.fill("In-process");
    await box.press("Enter");
    await expect(question).toHaveCount(0);
  } finally {
    await finish(ticket);
  }
});

test("Files lists what a Ticket without a repository holds", async ({ page }) => {
  await signedInPage(page);
  const ticket = await athena("task", "Write the release notes");
  try {
    const work = join(process.env.ATHENA_E2E_STATE ?? "", "work");
    await expect.poll(() => readdirSync(work).find((d) => d.toLowerCase().endsWith(`-${ticket.toLowerCase()}`))).toBeTruthy();
    const dir = join(work, readdirSync(work).find((d) => d.toLowerCase().endsWith(`-${ticket.toLowerCase()}`))!);
    mkdirSync(join(dir, "docs"), { recursive: true });
    writeFileSync(join(dir, "notes.md"), "# Notes\n");
    writeFileSync(join(dir, "docs", "a b.txt"), "spaced\n");

    await page.goto(`/tickets/${ticket}?tab=files`);
    const files = page.getByRole("list", { name: "Files" });
    await expect(files).toContainText("notes.md");
    await expectAccessible(page);
    const download = page.waitForEvent("download");
    await files.getByRole("link", { name: "docs/a b.txt" }).click();
    const d = await download;
    expect(d.suggestedFilename()).toBe("a b.txt");
  } finally {
    await finish(ticket);
  }
});

test("long and ANSI content stays inside the column", async ({ page, session }, testInfo) => {
  if (testInfo.project.name === "phone") await page.setViewportSize({ width: 320, height: 640 });
  await signedInPage(page);
  const title = `שלום עולם ${"Long ".repeat(10)}${"W".repeat(1900)}`;
  const wide = `const wide = "${"x".repeat(3000)}";\nשלום עולם ${"ב".repeat(1500)}\n`;
  const repo = await ticketWithEdit(title, { "wide.ts": wide });
  try {
    await session.report(repo.id, "waiting", `\u001b[31mred failure\u001b[0m ${"z".repeat(2500)}`, "Go on?");
    await page.goto(`/tickets/${repo.id}?tab=timeline`);
    await expect(page.getByTestId("ticket-detail").getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.getByText("red failure")).toBeVisible();
    expect(await page.locator("body").innerText()).not.toContain("\u001b");
    await expectNoSideScroll(page);

    await page.goto(`/tickets/${repo.id}?tab=changes`);
    await expect(page.getByRole("list", { name: "Changed files" })).toContainText("wide.ts");
    await expect(page.getByText("const wide", { exact: false })).toBeVisible();
    await expectNoSideScroll(page);

    await page.goto(`/tickets`);
    await expect(page.getByRole("heading", { name: "Tickets", level: 1 })).toBeVisible();
    await expectNoSideScroll(page, { column: false });
    await expectAccessible(page);
  } finally {
    await finish(repo);
  }
});

test("the list filters by search and stage, and the keyboard opens a Ticket", async ({ page }, testInfo) => {
  await signedInPage(page);
  // The projects share the server and an earlier run's Tickets stay listed (ended): a tag makes these ones unique.
  const tag = Math.random().toString(36).slice(2, 8);
  const keep = await athena("task", `Zebra ${tag} stripes for the chart`);
  const other = await athena("task", `Giraffe ${tag} necks for the chart`);
  try {
    await page.goto("/tickets");
    const rows = page.locator("[data-ticket-row]");
    await page.getByRole("searchbox", { name: "Search Tickets" }).fill(`zebra ${tag}`);
    await expect(page).toHaveURL(/q=zebra/);
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText("Zebra");

    await page.getByRole("radio", { name: "Ended" }).click();
    await expect(page).toHaveURL(/stage=ended/);
    await expect(page.getByText("No Tickets match these filters.")).toBeVisible();
    await page.getByRole("button", { name: "Clear filters" }).click();
    await expect(page).toHaveURL(/\/tickets$/);
    await expect(page.getByRole("searchbox", { name: "Search Tickets" })).toHaveValue("");
    await expect(page.getByRole("radio", { name: "Ended" })).not.toBeChecked();

    await page.getByRole("searchbox", { name: "Search Tickets" }).fill(tag);
    await expect(rows).toHaveCount(2);
    await page.getByRole("radio", { name: "Working" }).click();
    await expect(page).toHaveURL(/stage=working/);
    await expectAccessible(page);

    if (testInfo.project.name === "desktop") {
      await page.locator("body").click({ position: { x: 700, y: 5 } });
      await page.keyboard.press("j");
      await expect(rows.first()).toBeFocused();
      await page.keyboard.press("j");
      await expect(rows.nth(1)).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(page).toHaveURL(/\/tickets\/T-\d+/);
      // Filters travel with the row: the list is still the filtered one.
      await expect(page).toHaveURL(new RegExp(`q=${tag}`));
      await expect(page.locator('main [data-pane="detail"] h1')).toBeVisible();
    }
  } finally {
    await finish(keep, other);
  }
});
