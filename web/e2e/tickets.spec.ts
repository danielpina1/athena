// Tickets, end to end: the list with its filters, the detail's live timeline and inline question, the Changes of a
// Ticket in a temp git repository (the worker's edit is made here, as the fake session does not write), the Files of
// one without a repository (docs.spec.ts has the viewer), and hostile content (a 2,000-character title, RTL, ANSI, a 3,000-character diff line)
// that must never move the page sideways; each row's runtime and the runtime filter, and a timeline with a failover
// and a review. Every Ticket a test opens is stopped at its end (R37).
import { execFile } from "node:child_process";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import type { Page } from "@playwright/test";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { athena, expect, expectAccessible, signedInPage, test, liveFeed } from "./harness";
import { editLink, timelineEntry, type LinkEdits } from "./link";

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
    await expect(page).toHaveScreenshot("ticket-changes-light.png", { mask: [...unstable, ...liveFeed(page)] });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("ticket-changes-dark.png", { mask: [...unstable, ...liveFeed(page)] });
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

    // The Report arrives while the page is open: its question is inline, the report card shows it, and the timeline
    // has a row for it that washes in.
    await session.report(ticket, "waiting", "Two caches fit", "Redis or in-process?");
    const question = page.getByRole("group", { name: /Choose the cache layer/ });
    await expect(question.getByText("Redis or in-process?")).toBeVisible();
    await expect(page.getByRole("region", { name: "Report" }).getByText("Two caches fit")).toBeVisible();
    // The open question is the newest report's row, with the orange dot.
    const row = page.getByTestId("timeline-row").filter({ hasText: "Asked you a question" });
    await expect(row).toContainText("Redis or in-process?");
    await expect(row.locator("[data-dot]")).toHaveAttribute("data-tone", "attention");
    await expect(row).toHaveClass(/motion-wash/);
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
    const files = page.getByRole("list", { name: "Documents" });
    await expect(files).toContainText("notes.md");
    await expectAccessible(page);
    const download = page.waitForEvent("download");
    await files.getByRole("link", { name: "Download docs/a b.txt" }).click();
    const d = await download;
    expect(d.suggestedFilename()).toBe("a b.txt");
    // A file opens in the viewer.
    await files.getByRole("link", { name: "notes.md", exact: true }).click();
    await expect(page.getByRole("dialog").getByRole("heading", { level: 3, name: "Notes" })).toBeVisible();
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
    await expect(page.getByRole("region", { name: "Report" }).getByText(/red failure/)).toBeVisible();
    await expect(page.getByTestId("timeline-row").filter({ hasText: "Asked you a question" })).toContainText("Go on?");
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

/** The Assignment the Ticket's session runs, from the environment fakeclaude logged for it. */
function sessionAssignment(ticket: string): string {
  try {
    const env = readFileSync(join(process.env.ATHENA_E2E_STATE ?? "", `env-${ticket}.log`), "utf8");
    return /^ATHENA_ASSIGNMENT=(.*)$/m.exec(env)?.[1] ?? "";
  } catch {
    return "";
  }
}

const REVIEW = [
  "## Review of the login flow",
  "",
  "The login form works, but three things need fixing before it ships:",
  "",
  "1. **Login doesn't type-check `password`**: an empty string passes the guard in `auth/login.ts`.",
  "2. The session cookie is set without `SameSite`; it is `Lax` now.",
  "3. Rate limiting counts per IP only, so one account can be guessed from many addresses.",
  "",
  "### Checks run",
  "",
  "- `npm test`: 142 passed",
  "- `npm run lint`: clean",
  "- `kubectl rollout status deploy/api`: rolled out on preview",
  "",
  "```ts",
  'if (typeof password !== "string" || password.length === 0) {',
  '  return fail("password required");',
  "}",
  "```",
  "",
  "Next: the per-account rate limit, then a pull request.",
].join("\n");

const COMMANDS = ["make deploy ENV=preview", "kubectl get pods -n preview", "kubectl rollout status deploy/api", "terraform apply -target=module.cdn"];

test("the timeline tells the Ticket's story, with the Report first", async ({ page, session }) => {
  // The real clock: the entries are the server's, today.
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const ticket = await athena("task", "Review the login flow");
  try {
    // The filters keep the list to this Ticket, so the screenshots do not depend on the specs that ran before.
    await page.goto(`/tickets/${ticket}?tab=timeline&stage=open&q=login%20flow`);
    // A first try fails and is retried: its session ends and a new one starts.
    await session.report(ticket, "failed", "The test database is down");
    const first = sessionAssignment(ticket);
    await athena("retry", ticket);
    await expect.poll(() => sessionAssignment(ticket), { timeout: 20_000 }).not.toBe(first);
    await expect.poll(() => sessionAssignment(ticket)).not.toBe("");

    // Four commands approved in a row, from the card at the top of the page.
    const card = page.getByRole("group", { name: /Review the login flow/ });
    for (const command of COMMANDS) {
      const verdict = session.requestApproval(ticket, "Bash", { command });
      await card.getByRole("button", { name: "Approve" }).click();
      expect((await verdict).decision).toBe("allow");
      await expect(card).toHaveCount(0);
    }
    await session.report(ticket, "progress", REVIEW);
    // The next one waits on the owner.
    void session.requestApproval(ticket, "Bash", { command: "git push --force-with-lease origin athena/login" });
    await expect(card.getByRole("button", { name: "Approve" })).toBeVisible();

    // The Report, first and readable: Markdown, clamped until asked.
    const report = page.getByRole("region", { name: "Report" });
    await expect(report.getByText("Progress", { exact: true })).toBeVisible();
    await expect(report.locator("ol > li")).toHaveCount(3);
    await expect(report.locator("code", { hasText: "password" }).first()).toBeVisible();
    const body = report.getByTestId("report-body").first();
    await expect(body).toHaveAttribute("data-clamped", "true");

    // The rail, newest first.
    const rows = page.getByTestId("timeline-row");
    await expect(rows.first()).toContainText("Needs your approval");
    await expect(rows.first()).toContainText("Bash: git push --force-with-lease origin athena/login");
    await expect(rows.first().locator("[data-dot]")).toHaveAttribute("data-tone", "attention");
    await expect(rows.nth(1)).toContainText("Progress update");
    await expect(rows.nth(1)).toContainText("Review of the login flow");
    const group = rows.filter({ hasText: "You approved 4 commands" });
    await expect(group).toHaveCount(1);
    await expect(rows.filter({ hasText: "Retried" })).toHaveCount(1);
    await expect(rows.filter({ hasText: "Started work" })).toHaveCount(2);
    await expect(rows.filter({ hasText: "Started from your request" })).toContainText("Review the login flow");

    await expectAccessible(page);
    const unstable = [
      page.locator("time"),
      page.locator("aside .font-mono"),
      page.getByRole("region", { name: "Today" }),
      page.getByRole("region", { name: "Facts" }),
      page.getByTestId("ticket-detail").locator("header .font-mono"),
      page.locator("[data-ticket-row] .font-mono"),
    ];
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot("timeline-story-light.png", { mask: [...unstable, ...liveFeed(page)], fullPage: true });

    // Opened: the group lists what was approved; the full report reads in place.
    await group.getByText("You approved 4 commands").click();
    await expect(group.getByRole("list", { name: "What you approved" }).getByRole("listitem")).toHaveText(COMMANDS.map((c) => `Bash: ${c}`));
    await report.getByRole("button", { name: "Show the full report" }).click();
    await expect(body).toHaveAttribute("data-clamped", "false");
    await expect(report.getByText("Next: the per-account rate limit, then a pull request.")).toBeVisible();
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("timeline-story-dark.png", { mask: [...unstable, ...liveFeed(page)], fullPage: true });

    // View report brings the card into view.
    await rows.nth(1).getByRole("button", { name: "View report" }).click();
    await expect(report).toBeFocused();
  } finally {
    await finish(ticket);
  }
});

test("each row says its runtime and the list filters by it, light and dark", async ({ page }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const tickets: string[] = [];
  try {
    await page.goto("/tickets?stage=working&q=runtime%20list");
    // One at a time, each once its session started, so the list's order (newest change first) is the same every run.
    const rows = page.locator("[data-ticket-row]");
    for (const [title, args, badge] of [
      ["Runtime list: write the release notes", ["--runtime", "claude"], "Claude Code"],
      ["Runtime list: port the tokenizer", ["--runtime", "codex"], "Codex"],
      ["Runtime list: choose the cache layer", [], "Claude Code · auto"],
    ] as const) {
      tickets.push(await athena("task", title, ...args));
      await expect(rows.filter({ hasText: title })).toContainText(badge, { timeout: 20_000 });
    }
    await expect(rows).toHaveCount(3);
    await expect(page.getByText("3 Tickets")).toBeVisible();
    await expectAccessible(page);
    const unstable = [page.locator("time"), page.locator("aside .font-mono"), page.getByRole("region", { name: "Today" })];
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot("list-runtimes-light.png", { mask: [...unstable, ...liveFeed(page)] });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expect(page).toHaveScreenshot("list-runtimes-dark.png", { mask: [...unstable, ...liveFeed(page)] });

    // Filtered to Codex: the count is the rows shown.
    await page.getByLabel("Runtime", { exact: true }).selectOption("codex");
    await expect(page).toHaveURL(/runtime=codex/);
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText("Runtime list: port the tokenizer");
    await expect(page.getByText("1 Ticket", { exact: true })).toBeVisible();
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("list-codex-dark.png", { mask: [...unstable, ...liveFeed(page)] });
    await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
    await expect(page).toHaveScreenshot("list-codex-light.png", { mask: [...unstable, ...liveFeed(page)] });
    await page.getByLabel("Runtime", { exact: true }).selectOption("claude");
    await expect(rows).toHaveCount(2);
    await expect(page.getByText("2 Tickets")).toBeVisible();
  } finally {
    await finish(...tickets);
  }
});

test("the timeline tells a failover and a review, light and dark", async ({ page }) => {
  const day = (h: number, m: number) => new Date(2026, 10, 4, h, m).getTime();
  await page.clock.setFixedTime(day(16, 0));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  const title = "Port the parser to the new grammar";
  let ticket = "";
  const entry = (h: number, m: number, kind: string, subkind: string, runtime = "", subject = "", text = "") =>
    timelineEntry({ at: timestampFromMs(day(h, m)), kind, subkind, runtime, subject, text, refId: `${kind}-${h}${m}` });
  // The story a shared server cannot tell on its own: Claude Code hits its limit, Codex continues, a server restart
  // cuts the session short and it resumes, then a review that could not finish, one that asked for changes and one
  // that approved.
  const story = [
    entry(9, 0, "trigger", "owner", "", "", title),
    entry(9, 1, "assignment", "started", "claude", "claude-sonnet-4-5"),
    entry(10, 12, "assignment", "limited", "claude", "claude", new Date(day(15, 0)).toISOString()),
    entry(10, 13, "assignment", "started", "codex", "gpt-5.5"),
    entry(11, 30, "assignment", "restart", "codex"),
    entry(11, 31, "assignment", "resumed", "codex"),
    entry(12, 5, "assignment", "review", "claude", "claude-opus-4-1"),
    entry(12, 9, "review", "failed", "claude", "claude", "The review session ended before it gave a verdict"),
    entry(12, 30, "assignment", "review", "claude", "claude-opus-4-1"),
    entry(12, 48, "review", "changes", "claude", "claude", "The parser drops the last token of a file without a newline"),
    entry(12, 50, "assignment", "started", "codex", "gpt-5.5"),
    entry(13, 40, "assignment", "review", "claude", "claude-opus-4-1"),
    entry(13, 55, "review", "approved", "claude", "claude"),
  ];
  const edits: LinkEdits = {
    detail: (d) => {
      if (d.summary?.id === ticket) d.timeline = story;
    },
  };
  await editLink(page, edits);
  await signedInPage(page);
  ticket = await athena("task", title);
  try {
    await page.goto(`/tickets/${ticket}?tab=timeline&stage=open&q=new%20grammar`);
    await expect(page.getByRole("heading", { name: title, level: 1 })).toBeVisible();
    const rows = page.getByTestId("timeline-row");
    await expect(rows).toHaveText([
      /Review approved/,
      /Review started · Claude Code/,
      /Started work · Codex · gpt-5.5/,
      /Changes requested.*The parser drops the last token/,
      /Review started · Claude Code/,
      /Review could not finish.*ended before it gave a verdict/,
      /Review started · Claude Code/,
      /Resumed automatically/,
      /Interrupted by a server restart/,
      /Continued on Codex · gpt-5.5/,
      /Usage limit reached on Claude Code.*Resets at 15:00/,
      /Started work · Claude Code · sonnet 4.5/,
      /Started from your request/,
    ]);
    const tone = (i: number) => rows.nth(i).locator("[data-dot]");
    await expect(tone(0)).toHaveAttribute("data-tone", "ok");
    await expect(tone(3)).toHaveAttribute("data-tone", "neutral");
    await expect(tone(5)).toHaveAttribute("data-tone", "warn");
    await expect(tone(7)).toHaveAttribute("data-tone", "ok");
    await expect(tone(8)).toHaveAttribute("data-tone", "warn");
    await expect(tone(10)).toHaveAttribute("data-tone", "warn");
    await expectAccessible(page);
    const unstable = [
      page.locator("aside .font-mono"),
      page.getByRole("region", { name: "Today" }),
      page.getByRole("region", { name: "Facts" }),
      page.getByTestId("ticket-detail").locator("header .font-mono"),
      page.locator("[data-ticket-row] time"),
    ];
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot("timeline-failover-light.png", { mask: [...unstable, ...liveFeed(page)], fullPage: true });
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("timeline-failover-dark.png", { mask: [...unstable, ...liveFeed(page)], fullPage: true });
  } finally {
    await finish(ticket);
  }
});
