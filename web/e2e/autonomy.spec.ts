// The autonomy brakes on the dashboard: pause from the dashboard and resume with the passkey again, the paused
// banner, the "holding new work" notice in Needs you, and the trust badges on Tickets and Triggers, at the three
// widths, light and dark. The brake and the badges are states the shared server does not reach on its own, so the
// link edits them in (link.ts); pausing is real, and every test resumes in `finally`.
import { create } from "@bufbuild/protobuf";
import type { Page } from "@playwright/test";
import { RepoViewSchema } from "../src/gen/athena/v1/state_pb";
import { athena, expect, expectAccessible, liveFeed, signedInPage, test } from "./harness";
import { editLink, type LinkEdits } from "./link";

/** Pauses from the dashboard: the control (the sidebar's, or the top bar's on the phone), then the confirm. */
async function pauseFromDashboard(page: Page) {
  await page.getByRole("button", { name: "Pause Athena" }).click();
  const dialog = page.getByRole("dialog", { name: "Pause Athena?" });
  await expect(dialog).toContainText("Running sessions stop at the end of their current turn");
  return dialog;
}

const banner = (page: Page) => page.locator("[data-paused-banner]");

async function shot(page: Page, name: string, mask: ReturnType<Page["locator"]>[] = []) {
  for (const scheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: scheme, reducedMotion: "reduce" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", scheme);
    await expectAccessible(page);
    await page.evaluate(() => document.fonts.ready);
    await expect(page).toHaveScreenshot(`${name}-${scheme}.png`, { mask: [page.locator("time"), ...mask, ...liveFeed(page)] });
  }
}

test("pause from the dashboard, see it in status, resume with the passkey", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  await expect(banner(page)).toHaveCount(0);
  try {
    const dialog = await pauseFromDashboard(page);
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("pause-confirm-light.png", { mask: [...liveFeed(page)] });
    // Cancel does nothing.
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(banner(page)).toHaveCount(0);
    expect(await athena("status")).not.toContain("paused");

    await (await pauseFromDashboard(page)).getByRole("button", { name: "Pause", exact: true }).click();
    await expect(banner(page)).toBeVisible();
    await expect(banner(page)).toContainText("Athena is paused since");
    await expect(banner(page)).toContainText("Nothing new starts and replies wait.");
    await expect(page.getByRole("button", { name: "Pause Athena" })).toHaveCount(0);
    expect(await athena("status")).toContain("paused since");
    await shot(page, "paused");

    // Resume is sensitive: the server asks for the passkey again once the sign-in is older than its five-minute
    // window (a fresh sign-in, as here, is inside it, so the step-up is skipped; ReauthDialog.test and the outbox
    // tests cover the ask). When it does ask, the virtual authenticator answers.
    await banner(page).getByRole("button", { name: "Resume" }).click();
    const reauth = page.getByRole("dialog", { name: "Confirm it is you" });
    await Promise.race([reauth.waitFor(), expect(banner(page)).toHaveCount(0)]).catch(() => {});
    if (await reauth.isVisible()) await reauth.getByRole("button", { name: "Confirm with passkey" }).click();
    await expect(banner(page)).toHaveCount(0);
    expect(await athena("status")).not.toContain("paused");
    await expect(page.getByRole("button", { name: "Pause Athena" })).toBeVisible();
  } finally {
    await athena("resume").catch(() => {});
  }
});

test("a pause from the CLI shows the banner, and a repository's brake says what is held", async ({ page }) => {
  const edits: LinkEdits = {
    repos: (r) => {
      r.splice(
        0,
        r.length,
        create(RepoViewSchema, { name: "acme/web", kind: "github", owed: 3, ceiling: 3, braked: 2, mode: "live" }),
        create(RepoViewSchema, { name: "acme/api", kind: "github", owed: 1, ceiling: 3, braked: 0, mode: "live" }),
      );
    },
  };
  await editLink(page, edits);
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const held = page.getByRole("list", { name: "Held back" });
  await expect(held).toHaveText("Holding 2 new items in acme/web until you catch up: 3 already wait on you (limit 3).");
  await expect(held.getByRole("listitem")).toHaveCount(1);
  try {
    await athena("pause");
    await expect(banner(page)).toBeVisible();
    await shot(page, "brake-paused");
  } finally {
    await athena("resume").catch(() => {});
  }
  await expect(banner(page)).toHaveCount(0);
});

test("trust badges on a Ticket's card, its page and a Trigger", async ({ page, session }) => {
  const edits: LinkEdits = {
    ticket: (t) => {
      t.trust = "stranger";
    },
    trigger: (t) => {
      t.trust = "bot";
    },
  };
  await editLink(page, edits);
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await signedInPage(page);
  const ticket = await athena("task", "Fix the flaky login test");
  try {
    const verdict = session.requestApproval(ticket, "Bash", { command: "make deploy" });
    const card = page.getByRole("group", { name: /Fix the flaky login test/ });
    await expect(card.getByText("From outside your allow list · restricted tools")).toBeVisible();
    const unstable = [page.locator('main a[href^="/tickets/"]'), page.getByText(/^Today: /), page.getByText(/need you/)];
    await shot(page, "trust-needs-you", unstable);

    await page.goto(`/tickets/${ticket}`);
    await expect(page.getByRole("heading", { level: 1, name: "Fix the flaky login test" })).toBeVisible();
    await expect(page.locator("header").getByText("From outside your allow list · restricted tools")).toBeVisible();

    await page.goto("/routing?tab=triggers");
    await expect(page.locator("[data-trigger-row]").first().getByText("From a bot")).toBeVisible();
    await expectAccessible(page);
    void verdict.catch(() => {});
  } finally {
    await athena("stop", ticket).catch(() => {});
  }
});
