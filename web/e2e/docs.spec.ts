// The documents a Ticket made, end to end: a Ticket in a temp git repository whose worker committed one report and
// left another (and other files) uncommitted in its worktree. The Documents strip opens the report in the viewer; the
// Files tab lists what the worktree added or changed, documents first and secrets never; the viewer shows Markdown
// rendered, code with line numbers, an image, a file past the 1 MB cut, and SVG and HTML only as their source, under
// the app's CSP (the harness fails a test on any violation). Set ATHENA_PREVIEWS_DIR to also save screenshots there.
import { execFile } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { crc32, deflateSync } from "node:zlib";
import type { Page, TestInfo } from "@playwright/test";
import { athena, expect, expectAccessible, signedInPage, test } from "./harness";

const run = promisify(execFile);
const git = (cwd: string, ...args: string[]) =>
  run("git", ["-c", "user.name=Athena E2E", "-c", "user.email=e2e@example.com", ...args], { cwd, encoding: "utf8" });

const REPORT = `# Login review

The login flow is sound. Two issues need a fix before release; neither blocks the merge.

## Findings

| Area | Finding | Severity |
| --- | --- | --- |
| Sessions | Cookies lack \`SameSite=Strict\` | Medium |
| Rate limit | 429 path is untested | Low |

## Next steps

- [x] Reproduce the 500 on an expired token
- [x] Add a regression test
- [ ] Set \`SameSite=Strict\` on the session cookie
- [ ] Cover the 429 path

> The fix is small: one line in \`auth/cookie.go\` and a test.

\`\`\`go
http.SetCookie(w, &http.Cookie{Name: "sid", SameSite: http.SameSiteStrictMode})
\`\`\`
`;

const CODE = `package auth

import "net/http"

// SetSession writes the session cookie: HTTP only, secure, strict.
func SetSession(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    id,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}
`;

/** A small PNG bar chart, made here so no binary is committed. */
function chartPng(): Buffer {
  const [w, h] = [480, 270];
  const bars: [number, number, number[]][] = [
    [40, 120, [255, 90, 31]],
    [130, 180, [17, 17, 17]],
    [220, 90, [107, 107, 107]],
    [310, 210, [255, 90, 31]],
    [400, 150, [17, 17, 17]],
  ];
  const raw = Buffer.alloc((w * 3 + 1) * h);
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      let c = y === h - 30 ? [17, 17, 17] : [245, 245, 245];
      for (const [bx, bh, col] of bars) if (x >= bx && x < bx + 50 && y >= h - 30 - bh && y < h - 30) c = col;
      raw.set(c, y * (w * 3 + 1) + 1 + x * 3);
    }
  }
  const chunk = (type: string, data: Buffer) => {
    const t = Buffer.concat([Buffer.from(type), data]);
    const out = Buffer.alloc(8 + data.length + 4);
    out.writeUInt32BE(data.length, 0);
    t.copy(out, 4);
    out.writeUInt32BE(crc32(t) >>> 0, 8 + data.length);
    return out;
  };
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0);
  ihdr.writeUInt32BE(h, 4);
  ihdr.set([8, 2, 0, 0, 0], 8);
  return Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk("IHDR", ihdr), chunk("IDAT", deflateSync(raw)), chunk("IEND", Buffer.alloc(0))]);
}

type Repo = { dir: string; id: string; worktree: string };

/** A Ticket in a fresh repository, its worktree holding a worker's report (committed) and its other work (not). */
async function ticketWithDocuments(title: string): Promise<Repo> {
  const dir = mkdtempSync(join(tmpdir(), "athena-e2e-docs-"));
  await git(dir, "init", "-q", "-b", "main");
  writeFileSync(join(dir, "README.md"), "# Demo\n");
  writeFileSync(join(dir, ".gitignore"), "build/\n");
  await git(dir, "add", ".");
  await git(dir, "commit", "-q", "-m", "first");
  const id = await athena("task", title, "--dir", dir);
  const branch = `refs/heads/athena/${id.toLowerCase()}`;
  let worktree = "";
  await expect
    .poll(
      async () => {
        const list = (await git(dir, "worktree", "list", "--porcelain")).stdout;
        worktree = list.split("\n\n").find((b) => b.includes(`branch ${branch}`))?.match(/^worktree (.+)$/m)?.[1] ?? "";
        return worktree;
      },
      { message: `the worktree of ${id}`, timeout: 20_000 },
    )
    .not.toBe("");
  mkdirSync(join(worktree, "docs"), { recursive: true });
  writeFileSync(join(worktree, "docs", "REVIEW.md"), "# Review notes\n\nCommitted with the change.\n");
  await git(worktree, "add", "docs/REVIEW.md");
  await git(worktree, "commit", "-q", "-m", "the worker's review notes");
  // Left in the worktree, not committed: the report, code, an image, a large log, markup, a secret, an ignored file.
  writeFileSync(join(worktree, "REPORT.md"), REPORT);
  mkdirSync(join(worktree, "auth"), { recursive: true });
  writeFileSync(join(worktree, "auth", "cookie.go"), CODE);
  writeFileSync(join(worktree, "chart.png"), chartPng());
  writeFileSync(join(worktree, "server.log"), Array.from({ length: 30_000 }, (_, i) => `2026-11-05T10:00:${String(i % 60).padStart(2, "0")}Z GET /api/health 200 ${i}`).join("\n") + "\n");
  writeFileSync(join(worktree, "logo.svg"), `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" onload="alert(1)"><script>alert(2)</script><rect width="10" height="10"/></svg>\n`);
  writeFileSync(join(worktree, "preview.html"), `<!doctype html>\n<html><body><h1>Preview</h1><script>alert(1)</script></body></html>\n`);
  writeFileSync(join(worktree, ".env"), "API_TOKEN=not-a-real-token\n");
  mkdirSync(join(worktree, "build"), { recursive: true });
  writeFileSync(join(worktree, "build", "out.txt"), "ignored\n");
  return { dir, id, worktree };
}

/** Saves a preview when ATHENA_PREVIEWS_DIR is set (the desktop and phone projects only). */
async function preview(page: Page, info: TestInfo, name: string) {
  const dir = process.env.ATHENA_PREVIEWS_DIR;
  if (!dir || info.project.name === "tablet") return;
  const theme = (await page.locator("html").getAttribute("data-theme")) ?? "light";
  await page.evaluate(() => document.fonts.ready);
  await page.screenshot({ path: join(dir, `${name}-${theme}-${info.project.name}.png`) });
}

const viewer = (page: Page) => page.getByRole("dialog");

async function closeViewer(page: Page, info: TestInfo) {
  if (info.project.name === "phone") await viewer(page).getByRole("button", { name: "Back" }).click();
  else await page.keyboard.press("Escape");
  await expect(viewer(page)).toHaveCount(0);
}

test("a Ticket's documents open in the viewer", async ({ page }, testInfo) => {
  test.setTimeout(120_000);
  await page.clock.setFixedTime(new Date("2026-11-05T10:00:00"));
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const title = "Review the login flow";
  const repo = await ticketWithDocuments(title);
  try {
    await page.goto(`/tickets/${repo.id}?tab=timeline&stage=working&q=login%20flow`);
    await expect(page.getByRole("heading", { name: title, level: 1 })).toBeVisible();

    // The report is one tap away, above the tabs.
    const strip = page.getByRole("region", { name: "Documents" });
    await expect(strip.getByRole("link", { name: "REPORT.md" })).toBeVisible();
    await expect(strip.getByRole("link", { name: "REVIEW.md" })).toBeVisible();
    for (const theme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
      await expect(page.locator("html")).toHaveAttribute("data-theme", theme);
      await preview(page, testInfo, "strip");
    }
    await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });

    const open = strip.getByRole("link", { name: "REPORT.md" });
    await open.click();
    await expect(page).toHaveURL(/[?&]file=REPORT\.md/);
    const dialog = viewer(page);
    const heading = dialog.getByRole("heading", { level: 2, name: "REPORT.md" });
    await expect(heading).toBeFocused();
    await expect(dialog.getByRole("heading", { level: 3, name: "Login review" })).toBeVisible();
    await expect(dialog.getByRole("table")).toContainText("SameSite=Strict");
    await expect(dialog.locator("[data-task=done]")).toHaveCount(2);
    await expect(dialog.getByRole("link", { name: "Download" })).toHaveAttribute("href", `/api/tickets/${repo.id}/files/REPORT.md`);
    await expectAccessible(page);
    await expect(dialog).toHaveScreenshot("viewer-markdown-light.png");
    await preview(page, testInfo, "markdown");
    await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expectAccessible(page);
    await expect(dialog).toHaveScreenshot("viewer-markdown-dark.png");
    await preview(page, testInfo, "markdown");
    await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });

    // Its source, a toggle away.
    await dialog.getByRole("radio", { name: "Source" }).click();
    await expect(dialog.getByRole("region", { name: "Source of REPORT.md" })).toContainText("| Area | Finding | Severity |");

    // Closing gives focus back to the link that opened it.
    await closeViewer(page, testInfo);
    await expect(open).toBeFocused();
    await expect(page).not.toHaveURL(/file=/);

    // The Files tab: what the worktree added or changed, documents first; never the secret or the ignored file.
    await page.getByRole("tab", { name: "Files" }).click();
    const docs = page.getByRole("list", { name: "Documents" });
    const other = page.getByRole("list", { name: "Other files" });
    await expect(docs).toContainText("REPORT.md");
    await expect(docs).toContainText("docs/REVIEW.md");
    await expect(docs).toContainText("chart.png");
    await expect(other).toContainText("auth/cookie.go");
    await expect(other).toContainText("logo.svg");
    await expect(page.getByTestId("ticket-detail")).not.toContainText(".env");
    await expect(page.getByTestId("ticket-detail")).not.toContainText("build/out.txt");
    await expectAccessible(page);
    for (const theme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
      await expect(page.locator("html")).toHaveAttribute("data-theme", theme);
      await preview(page, testInfo, "files-tab");
    }
    await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });

    /** Opens path from the Files tab and, once ready() says its content shows, saves its previews. */
    const show = async (path: string, shot: string, ready: () => Promise<void>) => {
      await page.getByRole("link", { name: path, exact: true }).click();
      await expect(viewer(page).getByRole("heading", { level: 2 })).toBeFocused();
      await ready();
      for (const theme of ["light", "dark"] as const) {
        await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
        await expect(page.locator("html")).toHaveAttribute("data-theme", theme);
        await preview(page, testInfo, shot);
      }
      await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
    };

    // Code: numbered lines.
    const code = viewer(page).getByRole("region", { name: "Source of cookie.go" });
    await show("auth/cookie.go", "code", () => expect(code).toBeVisible());
    await expect(code.locator("[data-line-number]")).toHaveCount(CODE.split("\n").length - 1);
    await expect(code).toContainText("SameSite: http.SameSiteStrictMode");
    await expectAccessible(page);
    await closeViewer(page, testInfo);

    // An image, inline.
    const img = viewer(page).getByRole("img", { name: "chart.png" });
    await show("chart.png", "image", () => expect.poll(() => img.evaluate((el: HTMLImageElement) => el.naturalWidth)).toBe(480));
    await closeViewer(page, testInfo);

    // Past 1 MB: the start, and the whole file a download away.
    await show("server.log", "too-big", () => expect(viewer(page).getByRole("note")).toContainText("Showing the first 1 MB."));
    await closeViewer(page, testInfo);

    // SVG and HTML: their source, never rendered.
    await show("logo.svg", "svg-source", () =>
      expect(viewer(page).getByRole("region", { name: "Source of logo.svg" })).toContainText("<script>alert(2)</script>"),
    );
    expect(await viewer(page).locator("svg rect, script, iframe, object, embed").count()).toBe(0);
    await closeViewer(page, testInfo);
    await page.getByRole("link", { name: "preview.html", exact: true }).click();
    await expect(viewer(page).getByRole("region", { name: "Source of preview.html" })).toContainText("<h1>Preview</h1>");
    expect(await viewer(page).getByRole("heading", { name: "Preview", exact: true }).count()).toBe(0);
    await closeViewer(page, testInfo);

    // The view route itself: text/plain, sandboxed, inline.
    const res = await page.request.get(`/api/tickets/${repo.id}/files/logo.svg?view=1`);
    expect(res.headers()["content-type"]).toBe("text/plain; charset=utf-8");
    expect(res.headers()["content-security-policy"]).toContain("sandbox; default-src 'none'");
    expect(res.headers()["content-disposition"]).toBe("inline; filename=logo.svg");
    expect((await page.request.get(`/api/tickets/${repo.id}/files/.env?view=1`)).status()).toBe(404);
    expect((await page.request.get(`/api/tickets/${repo.id}/files/build/out.txt?view=1`)).status()).toBe(404);

    // A shared link opens the file straight away.
    await page.goto(`/tickets/${repo.id}?tab=files&file=docs%2FREVIEW.md`);
    await expect(viewer(page).getByRole("heading", { level: 3, name: "Review notes" })).toBeVisible();
  } finally {
    await athena("stop", repo.id).catch(() => {});
    rmSync(repo.dir, { recursive: true, force: true });
  }
});
