// Splits, end to end (plan 7): the New task "Plan first" switch, a lead's Relatives block and its timeline of the
// split, its child tickets and an agent message, and the Needs you cards of a split waiting for the owner (PlanCard)
// and of a lead whose child failed (ChildCard). Those screens come from the link edit (link.ts), which shapes real
// Tickets into a lead and its children. The last test splits for real: plan first in a temporary git folder, the
// planner's fixture (e2e/fixtures/planner.jsonl), the breakdown editor, and the children's reports through the
// harness. Every Ticket a test opens is stopped at its end (R37).
import { execFile } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import type { Page } from "@playwright/test";
import { BreakdownViewSchema, RepoViewSchema } from "../src/gen/athena/v1/state_pb";
import { athena, expect, expectAccessible, liveFeed, sessionAssignment, signedInPage, test } from "./harness";
import { editLink, fixedNode, runtimeStatus, timelineEntry, type LinkEdits } from "./link";

async function stopAll(...tickets: string[]) {
  for (const t of tickets) await athena("stop", t).catch(() => {});
}

async function both(page: Page, name: string, mask: ReturnType<Page["locator"]>[], fullPage = false) {
  await expectAccessible(page);
  await page.evaluate(() => document.fonts.ready);
  await expect(page).toHaveScreenshot(`${name}-light.png`, { mask: [...mask, ...liveFeed(page)], fullPage });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectAccessible(page);
  await expect(page).toHaveScreenshot(`${name}-dark.png`, { mask: [...mask, ...liveFeed(page)], fullPage });
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
}

/** The editor dialog alone, light and dark, axe clean. */
async function editorShots(page: Page, editor: ReturnType<Page["locator"]>, name: string) {
  await expectAccessible(page);
  await page.evaluate(() => document.fonts.ready);
  await expect(editor).toHaveScreenshot(`${name}-light.png`);
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectAccessible(page);
  await expect(editor).toHaveScreenshot(`${name}-dark.png`);
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
}

/** The embedded node, named and loaded the same on every run. */
const node = fixedNode([runtimeStatus({ name: "claude", ready: true }), runtimeStatus({ name: "codex", ready: true })]);

const breakdown = (n: number) =>
  create(BreakdownViewSchema, {
    state: "draft",
    revision: 1,
    tasks: Array.from({ length: n }, (_, i) => ({ key: `s${i + 1}`, title: `Sub-task ${i + 1}` })),
  });

test("Plan first is offered with a repository, light and dark", async ({ page }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await editLink(page, { node });
  await signedInPage(page);
  await page.goto("/new");
  const plan = page.getByRole("switch", { name: "Plan first" });
  await expect(plan).toBeDisabled();
  await expect(plan).toHaveAccessibleDescription("Needs a repository.");
  await page.getByRole("textbox", { name: "What should Athena do?" }).fill("Port the parser to the new grammar");
  const repo = page.getByRole("combobox", { name: "Repository" });
  await repo.fill("acme/widgets");
  await repo.press("Escape");
  await expect(plan).toBeEnabled();
  await plan.click();
  await expect(plan).toHaveAttribute("aria-checked", "true");
  await expect(plan).toHaveAccessibleDescription("Athena drafts a split into sub-tasks for you to approve first.");
  await both(page, "plan-first", [page.locator("aside .font-mono"), page.getByRole("region", { name: "Today" })]);
});

test("a lead shows its child tickets and the story of its split, light and dark", async ({ page }) => {
  const day = (h: number, m: number) => new Date(2026, 10, 4, h, m).getTime();
  await page.clock.setFixedTime(day(16, 0));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  const ids = { lead: "", lexer: "", grammar: "" };
  const entry = (h: number, m: number, kind: string, subkind: string, subject = "", text = "") =>
    timelineEntry({ at: timestampFromMs(day(h, m)), kind, subkind, subject, text, refId: `${kind}-${subkind}-${h}${m}` });
  const edits: LinkEdits = {
    node,
    ticket: (t) => {
      if (t.id === ids.lead) Object.assign(t, { stage: "working", action: "", children: [ids.lexer, ids.grammar], planState: "approved" });
      if (t.id === ids.lexer) Object.assign(t, { stage: "working", action: "", lead: ids.lead, splitKey: "lexer" });
      if (t.id === ids.grammar)
        Object.assign(t, { stage: "working", action: "waiting", lead: ids.lead, splitKey: "grammar", deps: [ids.lexer], waiting: [ids.lexer] });
    },
    detail: (d) => {
      if (d.summary?.id !== ids.lead) return;
      d.breakdown = breakdown(2);
      d.timeline = [
        entry(9, 0, "trigger", "owner", "", "Port the parser to the new grammar"),
        entry(9, 1, "split", "drafted"),
        entry(9, 20, "split", "approved", "", `${ids.lexer}, ${ids.grammar}`),
        entry(9, 21, "child", "spawned", ids.lexer, "Parser lexer"),
        entry(9, 21, "child", "spawned", ids.grammar, "Parser grammar"),
        entry(10, 5, "agent", "received", ids.lexer, "Which token set should the lexer emit?"),
        entry(10, 6, "agent", "answered", ids.lexer, "The one in tokens.go"),
        entry(10, 40, "child", "failed", ids.grammar, "The grammar tests fail on trailing commas"),
        entry(11, 10, "child", "continued", ids.grammar),
        // A child's end reaches the lead twice: the input/child twin is not shown.
        entry(11, 30, "child", "ready", ids.lexer),
        entry(11, 30, "input", "child", "", "The lexer is ready on its branch."),
      ];
    },
  };
  await editLink(page, edits);
  await signedInPage(page);
  ids.lead = await athena("task", "Port the parser to the new grammar");
  ids.lexer = await athena("task", "Parser lexer");
  ids.grammar = await athena("task", "Parser grammar");
  try {
    await page.goto(`/tickets/${ids.lead}?tab=timeline&stage=open&q=parser`);
    await expect(page.getByRole("heading", { name: "Port the parser to the new grammar", level: 1 })).toBeVisible();
    const children = page.getByRole("list", { name: "Child tickets" }).getByRole("listitem");
    await expect(children).toHaveText([/Parser lexer.*Working/, /Parser grammar.*Working.*after lexer/]);
    const rows = page.getByTestId("timeline-row");
    await expect(rows).toHaveText([
      new RegExp(`Child ${ids.lexer} is ready`),
      new RegExp(`Child ${ids.grammar} was left out`),
      new RegExp(`Child ${ids.grammar} failed.*trailing commas`),
      new RegExp(`Answered ${ids.lexer}`),
      new RegExp(`${ids.lexer} asked.*Which token set`),
      new RegExp(`Child ${ids.grammar} spawned`),
      new RegExp(`Child ${ids.lexer} spawned`),
      /Split approved/,
      /Split drafted \(2\)/,
      /Started from your request/,
    ]);
    await expect(rows.first().getByRole("link", { name: ids.lexer })).toHaveAttribute("href", `/tickets/${ids.lexer}`);
    const relatives = page.getByRole("region", { name: "Relatives" });
    const mask = [
      page.locator("aside .font-mono"),
      page.getByRole("region", { name: "Today" }),
      page.getByRole("region", { name: "Facts" }),
      page.getByTestId("ticket-detail").locator("header .font-mono"),
      page.locator("[data-ticket-row] time"),
      page.locator("[data-ticket-row] .font-mono"),
      relatives.getByRole("link"),
      rows.getByRole("link"),
      rows.filter({ hasText: "Split approved" }).locator(".text-muted"),
    ];
    await both(page, "split-lead", mask, true);

    // The child names its lead.
    await page.goto(`/tickets/${ids.grammar}?tab=timeline&stage=open&q=parser`);
    await expect(page.getByRole("region", { name: "Relatives" })).toContainText(`Lead${ids.lead}Port the parser to the new grammarWorking`);
    await expectAccessible(page);
  } finally {
    await stopAll(ids.lead, ids.lexer, ids.grammar);
  }
});

test("Needs you asks for a split and for a failed child on its lead, light and dark", async ({ page }) => {
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  const ids = { draft: "", failed: "", lead: "", child: "" };
  const question = "Child T-0102 (Parser lexer) failed: the lexer tests fail. Retry it, drop it, or continue without it.";
  const edits: LinkEdits = {
    node,
    ticket: (t) => {
      if (t.id === ids.draft) Object.assign(t, { stage: "person", action: "plan", planState: "draft" });
      if (t.id === ids.failed) Object.assign(t, { stage: "person", action: "plan", planState: "failed" });
      if (t.id === ids.lead)
        Object.assign(t, { stage: "person", action: "child", children: [ids.child], attentionChild: ids.child, attentionQuestion: question });
      // The failed child itself has no card: its lead's is the one.
      if (t.id === ids.child) Object.assign(t, { stage: "person", action: "failed", lead: ids.lead, splitKey: "lexer" });
    },
    detail: (d) => {
      if (d.summary?.id === ids.draft) d.breakdown = breakdown(3);
      if (d.summary?.id === ids.failed) d.breakdown = create(BreakdownViewSchema, { state: "failed", error: "the planner timed out" });
    },
  };
  await editLink(page, edits);
  await signedInPage(page);
  ids.draft = await athena("task", "Split the release notes");
  ids.failed = await athena("task", "Split the importer");
  ids.lead = await athena("task", "Port the parser");
  ids.child = await athena("task", "Parser lexer");
  try {
    // A reload brings a Snapshot the edits shape, with every id known.
    await page.reload();
    await expect(page.getByText("A split of Split the release notes waits for you (3 sub-tasks)")).toBeVisible();
    await expect(page.getByText("Planning Split the importer failed: the planner timed out")).toBeVisible();
    const card = page.getByRole("group", { name: "Port the parser" });
    await expect(card.getByText(question)).toBeVisible();
    await expect(page.getByRole("group", { name: "Parser lexer" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Review split" })).toHaveCount(2);
    const mask = [
      page.locator("time"),
      page.locator('main a[href^="/tickets/"]'),
      page.getByRole("heading", { name: "Needs you", level: 1 }).locator("xpath=following-sibling::p"),
      page.getByRole("region", { name: "Today" }),
    ];
    await both(page, "split-needs-you", mask);

    // Drop asks first, naming what else goes; Escape takes it back.
    const drop = card.getByRole("button", { name: "Drop" });
    await drop.click();
    const confirm = card.getByRole("button", { name: `Drop ${ids.child}? Child tickets that wait for it are dropped too.` });
    await expect(confirm).toBeVisible();
    await confirm.press("Escape");
    await expect(drop).toBeVisible();
  } finally {
    await stopAll(ids.draft, ids.failed, ids.lead, ids.child);
  }
});

const run = promisify(execFile);
const git = (cwd: string, ...args: string[]) =>
  run("git", ["-c", "user.name=Athena E2E", "-c", "user.email=e2e@example.com", ...args], { cwd, encoding: "utf8" });

test("a split runs end to end: plan first, the editor, children in order, and a child left out", async ({ page, session }) => {
  test.setTimeout(150_000);
  await page.clock.setFixedTime(new Date("2026-11-04T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  // A git folder with one commit: splitting needs a branch (R2). The e2e server has no GitHub repositories.
  const dir = mkdtempSync(join(tmpdir(), "athena-e2e-split-"));
  await git(dir, "init", "-q", "-b", "main");
  writeFileSync(join(dir, "README.md"), "# Settings\n");
  await git(dir, "add", ".");
  await git(dir, "commit", "-q", "-m", "first");
  // The folder is in the picker as a local repository, as the scan of localRepoRoots would list it.
  await editLink(page, { node, repos: (r) => r.push(create(RepoViewSchema, { name: dir, kind: "git" })) });
  await signedInPage(page);
  const ids = { lead: "", api: "", ui: "" };
  try {
    // 1. New task in the folder, Plan first.
    await page.goto("/new");
    await page.getByRole("textbox", { name: "What should Athena do?" }).fill("Add a settings screen");
    const repo = page.getByRole("combobox", { name: "Repository" });
    await repo.fill(dir);
    await page.getByRole("option", { name: dir, exact: true }).click();
    await page.getByRole("switch", { name: "Plan first" }).click();
    await page.getByRole("button", { name: "Start" }).click();
    await expect(page).toHaveURL(/\/tickets\/T-\d+/);
    ids.lead = decodeURIComponent(new URL(page.url()).pathname.split("/").pop()!);

    // 2. The planner's draft waits in Needs you; Review split shows the overlap under both rows.
    await page.goto("/");
    const card = page.getByRole("group", { name: "Add a settings screen" });
    await expect(card.getByText("A split of Add a settings screen waits for you (3 sub-tasks)")).toBeVisible({ timeout: 20_000 });
    await card.getByRole("button", { name: "Review split" }).click();
    const editor = page.getByRole("dialog", { name: "Review split" });
    const row = (key: string) => editor.getByRole("group", { name: `Sub-task ${key}` });
    await expect(row("ui").getByText("Overlaps with docs: web/src")).toBeVisible();
    await expect(row("docs").getByText("Overlaps with ui: web/src")).toBeVisible();
    await expect(row("ui").getByRole("button", { name: "after api" })).toHaveAttribute("aria-pressed", "true");
    // The editor alone: the page behind it holds the folder's temporary path. The overlap under ui is in view.
    await row("ui").getByText("Overlaps with docs: web/src").scrollIntoViewIfNeeded();
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(page.viewportSize()!.width);
    await editorShots(page, editor, "split-editor");

    // 3. Leave docs out, rename ui, approve.
    await row("docs").getByRole("button", { name: "Remove docs" }).click();
    await expect(row("ui").getByText(/Overlaps/)).toHaveCount(0);
    await row("ui").getByRole("textbox", { name: "Title" }).fill("Add the settings page");
    await editor.getByRole("button", { name: "Approve and start" }).click();
    await expect(editor).toHaveCount(0);

    // 4. The lead's children: api works, ui waits for it.
    await page.goto(`/tickets/${ids.lead}?tab=timeline`);
    const children = page.getByRole("list", { name: "Child tickets" }).getByRole("listitem");
    await expect(children).toHaveText([/Add the API.*Working/, /Add the settings page.*after api/], { timeout: 20_000 });
    ids.api = (await children.nth(0).getByRole("link").textContent()) ?? "";
    ids.ui = (await children.nth(1).getByRole("link").textContent()) ?? "";

    // 5. api is ready: ui starts.
    await session.report(ids.api, "ready", "The settings endpoint is in.");
    await expect(children.nth(1)).not.toContainText("after api", { timeout: 20_000 });
    await expect.poll(() => sessionAssignment(ids.ui), { message: `the session of ${ids.ui}`, timeout: 20_000 }).not.toBe("");

    // 6. ui fails: the lead asks, in one card; the child has none of its own. Go on without it.
    await session.report(ids.ui, "failed", "The page tests fail.");
    await page.goto("/");
    const lead = page.getByRole("group", { name: "Add a settings screen" });
    await expect(lead.getByRole("button", { name: "Continue without it" })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByRole("group", { name: "Add the settings page" })).toHaveCount(0);
    await lead.getByRole("button", { name: "Continue without it" }).click();
    await expect(lead).toHaveCount(0);

    // 7. The lead starts, and its timeline tells the split's story.
    await expect.poll(() => sessionAssignment(ids.lead), { message: `the session of ${ids.lead}`, timeout: 20_000 }).not.toBe("");
    await page.goto(`/tickets/${ids.lead}?tab=timeline`);
    const rows = page.getByTestId("timeline-row");
    await expect(rows.filter({ hasText: `Child ${ids.api} is ready` })).toHaveCount(1);
    await expect(rows.filter({ hasText: `Child ${ids.ui} was left out` })).toHaveCount(1);
    await expectAccessible(page);
  } finally {
    await stopAll(ids.ui, ids.api, ids.lead);
    rmSync(dir, { recursive: true, force: true });
  }
});
