// Nodes, end to end, on a server of this spec's own (node-server.ts: TLS with Athena's own CA, which nodes need, and
// an offline threshold of 2 s): a node enrolled from the Add a node sheet, its join command pasted into a real
// `athena node run`; its card turns online; two tasks pinned to it from New task run there. Then the node drops
// (frozen, then killed): both Tickets show on Needs you as "e2e-node is offline", and its card says since when.
//
// The e2e has no GitHub, and the server refuses a task on a repository without it ("github is not running"), so every
// Ticket here works in a folder. The folder card only says the Ticket continues when the node is back. The repository
// card is shown by presenting the second Ticket with a repository (link.ts edits its summary, nothing else): Wait hides
// it, and its Retry elsewhere reaches the real server, which refuses a folder Ticket with its own sentence (Ruling R2),
// shown as it is. The move itself, a repository Ticket restored on another node, is internal/node's
// TestRetryElsewhereRestoresWork. Screenshots at each width, light and dark, and axe on every screen.
import type { Locator, Page } from "@playwright/test";
import { expect, expectAccessible, liveFeed, signedInPage, test as base } from "./harness";
import { editLink } from "./link";
import { joinAndRun, startTlsServer, waitUntil, type RunningNode, type TlsServer } from "./node-server";

// The pages talk to this spec's server, one per worker; its certificate is from Athena's own CA, which this browser
// does not trust.
const test = base.extend<object, { server: TlsServer }>({
  server: [
    async ({}, use) => {
      const s = await startTlsServer();
      await use(s);
      await s.stop();
    },
    { scope: "worker" },
  ],
  baseURL: async ({ server }, use) => use(server.url),
});
test.use({ ignoreHTTPSErrors: true });

/** What a screenshot must not pin: times, versions, the join command and the sidebar's live numbers. */
function unstable(page: Page): Locator[] {
  return [
    page.locator("time"),
    page.locator("[data-volatile]"),
    page.locator("[data-join-command]"),
    page.locator("[data-expires]"),
    page.locator("aside .font-mono"),
    page.getByRole("region", { name: "Today" }),
    ...liveFeed(page),
  ];
}

async function themes(page: Page, name: string, mask = unstable(page)) {
  await page.evaluate(() => document.fonts.ready);
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await expectAccessible(page);
  await expect(page).toHaveScreenshot(`${name}-light.png`, { mask });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expectAccessible(page);
  await expect(page).toHaveScreenshot(`${name}-dark.png`, { mask });
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
}

const ticketId = (page: Page) => new URL(page.url()).pathname.split("/").pop() ?? "";

/** Starts a task from New task, pinned to the node; returns its Ticket's id. */
async function startPinned(page: Page, text: string, nodeName: string): Promise<string> {
  await page.goto("/new");
  await page.getByRole("textbox", { name: "What should Athena do?" }).fill(text);
  await page.getByRole("radio", { name: nodeName }).click();
  await page.getByRole("button", { name: "Start" }).click();
  await expect(page).toHaveURL(/\/tickets\/[^/?]+$/);
  return ticketId(page);
}

test("a node enrolled from the dashboard runs a pinned task; offline, its Tickets wait or move elsewhere", async ({ page, server }) => {
  test.setTimeout(240_000);
  let node: RunningNode | undefined;
  try {
    await flow(page, server, (n) => (node = n));
  } finally {
    await node?.stop();
  }
});

async function flow(page: Page, server: TlsServer, started: (n: RunningNode) => void) {
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  // The Tickets presented as working in a repository (see the top): set once the second one exists.
  const asRepo = new Set<string>();
  await editLink(page, {
    ticket: (t) => {
      if (asRepo.has(t.id)) {
        t.repo = "acme/widgets";
        t.dir = "";
      }
    },
  });
  await signedInPage(page, server.state);

  // This machine alone: its card, online, and Add a node.
  await page.goto("/nodes");
  await expect(page.getByText("The machines that run sessions, with their load and health.")).toBeVisible();
  const studio = page.getByRole("region", { name: "studio" });
  await expect(studio.getByText("This machine")).toBeVisible();
  await expect(studio.locator("[data-node-state]")).toHaveText("Online");

  // The sheet shows the command once the server answers (a fresh sign-in needs no step-up).
  await page.getByRole("button", { name: "Add a node" }).click();
  const sheet = page.getByRole("dialog", { name: "Add a node" });
  const code = sheet.locator("[data-join-command]");
  await expect(code).toHaveText(/^athena node join https:\/\/localhost:\d+ \S+ --ca-sha256 \S+$/);
  await expect(sheet.getByText(/^Works once, until \d\d:\d\d$/)).toBeVisible();
  await expect(sheet.getByText("Run this on the new machine, connected to your VPN.")).toBeVisible();
  await themes(page, "nodes-add");
  // Pasted into the harness: the join, then the node's run.
  const node = await joinAndRun(server, (await code.textContent()) ?? "", "e2e-node");
  started(node);
  await sheet.getByRole("button", { name: "Close" }).click();
  await expect(sheet).toBeHidden();

  // Its card turns online.
  const card = page.getByRole("region", { name: "e2e-node" });
  await expect(card.locator("[data-node-state]")).toHaveText("Online");
  await expect(card.getByText("This machine")).toHaveCount(0);
  await expect(card.getByText("0 of 2 slots in use")).toBeVisible();
  await themes(page, "nodes-two");

  // New task offers it, Any node by default; a task pinned to it runs there.
  await page.goto("/new");
  await expect(page.getByRole("radio", { name: "Any node" })).toHaveAttribute("aria-checked", "true");
  await expect(page.getByRole("radio", { name: /^studio/ })).toContainText("this machine");
  await page.getByRole("textbox", { name: "What should Athena do?" }).fill("Say hello from the node");
  await page.getByRole("radio", { name: "e2e-node" }).click();
  await themes(page, "nodes-picker");
  await page.getByRole("button", { name: "Start" }).click();
  await expect(page).toHaveURL(/\/tickets\/[^/?]+$/);
  const folder = ticketId(page);
  const header = page.locator("header").filter({ has: page.getByRole("heading", { level: 1 }) });
  await expect(header.getByText("on e2e-node")).toBeVisible();
  await expect(header.locator("[data-stage]")).toHaveText("Working");
  const second = await startPinned(page, "Fix the flaky test", "e2e-node");
  asRepo.add(second);
  await waitUntil("both sessions on e2e-node", async () => (await server.nodeRow("e2e-node")).includes(" online 2/2 "));

  try {
    // The node drops: frozen, it answers nothing, so its link dies (9 s) and its Tickets show it offline 2 s later.
    node.freeze();
    await waitUntil("e2e-node offline", async () => (await server.nodeRow("e2e-node")).includes(" offline "), 30_000);
    await node.kill();

    // A reload: the Snapshot brings the second Ticket through the edit, as working in a repository.
    await page.goto("/");
    const folderCard = page.getByRole("group", { name: "Say hello from the node" });
    const repoCard = page.getByRole("group", { name: "Fix the flaky test" });
    await expect(folderCard).toBeVisible({ timeout: 20_000 });
    await expect(repoCard).toBeVisible({ timeout: 20_000 });
    await expect(folderCard.locator("[data-stage]")).toHaveText("e2e-node is offline");
    await expect(folderCard.getByText("This Ticket works in a folder on e2e-node; it continues when e2e-node is back.")).toBeVisible();
    await expect(folderCard.getByRole("button")).toHaveCount(0);
    await expect(repoCard.getByText(/^e2e-node is offline since \d\d:\d\d$/)).toBeVisible();
    await expect(repoCard.getByRole("button", { name: "Retry elsewhere" })).toBeVisible();
    await themes(page, "nodes-offline", [...unstable(page), page.locator("[data-needs-you-card] .font-mono")]);

    // Retry elsewhere reaches the server, which keeps a folder Ticket on its machine and says why.
    await repoCard.getByRole("button", { name: "Retry elsewhere" }).click();
    await expect(page.getByText("This Ticket works in a folder on e2e-node; it continues when e2e-node is back.").last()).toBeVisible();
    await expect(page.getByText("Could not retry")).toBeVisible();
    // Wait hides the card from this view; the Ticket still waits on the owner.
    await repoCard.getByRole("button", { name: "Wait" }).click();
    await expect(repoCard).toBeHidden();
    await expect(folderCard).toBeVisible();

    // The node's card says since when; the Ticket list and detail name the node.
    await page.goto("/nodes");
    await expect(card.locator("[data-node-state]")).toHaveText("Offline");
    await expect(card.getByText(/^Offline since \d\d:\d\d$/)).toBeVisible();
    await expectAccessible(page);
    await page.goto(`/tickets/${folder}`);
    await expect(header.locator("[data-stage]")).toHaveText("e2e-node is offline");
    await expect(header.getByText("on e2e-node")).toBeVisible();
    await expectAccessible(page);
  } finally {
    for (const id of [folder, second]) await server.athena("stop", id).catch(() => {});
  }
}
