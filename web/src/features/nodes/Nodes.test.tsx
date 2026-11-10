import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { CommandResultSchema, type CommandResult } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { Toaster } from "@/ui/primitives/toast";
import { at, load } from "../tickets/fixtures";
import { Nodes } from "./Nodes";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  createNodeToken: vi.fn(),
  setNodeDraining: vi.fn(),
  revokeNode: vi.fn(),
}));

const createNodeToken = vi.mocked(actions.createNodeToken);
const setNodeDraining = vi.mocked(actions.setNodeDraining);
const revokeNode = vi.mocked(actions.revokeNode);
const ok = create(CommandResultSchema, { ok: true });

beforeEach(() => {
  vi.clearAllMocks();
  setNodeDraining.mockResolvedValue(ok);
  revokeNode.mockResolvedValue(ok);
});
afterEach(() => {
  vi.useRealTimers();
  useAthena.getState().reset();
});

function renderNodes() {
  return render(
    <MemoryRouter>
      <Nodes />
      <Toaster />
    </MemoryRouter>,
  );
}

const node = { name: "mac-mini", slots: 4, slotsInUse: 2, liveTicketIds: ["T-0007", "T-0009"], queued: 3, claudeVersion: "2.1.3" };

test("the node card shows its load, queue, live Tickets and Claude version", () => {
  load({ nodes: [node], health: { items: [] } });
  renderNodes();
  const card = screen.getByRole("region", { name: "mac-mini" });
  expect(within(card).getByText("2 of 4 slots in use")).toBeTruthy();
  const bar = within(card).getByRole("meter", { name: "Slots in use" });
  expect(bar.getAttribute("aria-valuenow")).toBe("2");
  expect(bar.getAttribute("aria-valuemax")).toBe("4");
  expect(within(card).getByText("3")).toBeTruthy();
  expect(within(card).getByRole("link", { name: "T-0007" }).getAttribute("href")).toBe("/tickets/T-0007");
  expect(within(card).getByRole("link", { name: "T-0009" })).toBeTruthy();
  expect(within(card).getByText("2.1.3")).toBeTruthy();
});

test("the node card shows each runtime's block instead of the Claude version alone", () => {
  load({
    nodes: [
      {
        ...node,
        runtimes: [
          { name: "claude", ready: true, tested: false, measured: false },
          { name: "codex", version: "0.160.1", ready: false, tested: true, why: "Codex is not logged in: run athena codex login" },
        ],
      },
    ],
    health: { items: [] },
  });
  renderNodes();
  const card = screen.getByRole("region", { name: "mac-mini" });
  expect(within(card).queryByText("Claude version")).toBeNull();
  const runtimes = within(card).getByRole("list", { name: "Runtimes" });
  const claude = within(runtimes).getByRole("region", { name: "Claude Code" });
  expect(within(claude).getByText("Not measured yet")).toBeTruthy();
  // An empty version says nothing: never "untested".
  expect(within(claude).queryByText("untested")).toBeNull();
  const codex = within(runtimes).getByRole("region", { name: "Codex" });
  expect(within(codex).getByText("Not ready")).toBeTruthy();
  expect(within(codex).getByText("Codex is not logged in: run athena codex login")).toBeTruthy();
});

test("healthy checks collapse into one line and failing ones say why and since when", async () => {
  const user = userEvent.setup();
  load({
    nodes: [node],
    health: {
      items: [
        { name: "router", ok: true, since: at(0) },
        { name: "tls", ok: true, since: at(0) },
        { name: "push", ok: false, message: "No VAPID key configured.", since: at(-3_600_000) },
      ],
    },
  });
  renderNodes();
  const health = screen.getByRole("region", { name: "Health" });
  expect(within(health).getByText("push")).toBeTruthy();
  expect(within(health).getByText("No VAPID key configured.")).toBeTruthy();
  expect(within(health).getByText(/^Since/)).toBeTruthy();
  // The healthy ones: one line, open to see each.
  const passing = within(health).getByText("2 other checks passing");
  expect(within(health).queryByText("router")).toBeNull();
  await user.click(passing);
  expect(within(health).getByText("router")).toBeTruthy();
});

test("all checks passing is one line", () => {
  load({ nodes: [node], health: { items: [{ name: "router", ok: true }, { name: "tls", ok: true }] } });
  renderNodes();
  expect(within(screen.getByRole("region", { name: "Health" })).getByText("All checks passing")).toBeTruthy();
});

test("waiting for the server before the first Snapshot", () => {
  renderNodes();
  expect(screen.getAllByText("Waiting for the server.").length).toBeGreaterThan(0);
  expect(screen.queryByText(/slots in use/)).toBeNull();
});

// 14:02 and 14:32 local time, whatever the machine's zone.
const local = (h: number, m: number) => timestampFromMs(new Date(2026, 10, 4, h, m, 0).getTime());
const fleet = [
  { id: "local", name: "studio", local: true, state: "online", slots: 4, slotsInUse: 1, os: "darwin", arch: "arm64", version: "0.6.0" },
  { id: "n-mini", name: "mini", state: "draining", slots: 2, os: "linux", arch: "amd64", version: "0.6.0" },
  { id: "n-laptop", name: "laptop", state: "offline", lastSeen: local(14, 2), slots: 2 },
  { id: "n-old", name: "old", state: "revoked", lastSeen: local(9, 0), slots: 2 },
];

test("the subtitle speaks of every machine", () => {
  load({ nodes: fleet, health: { items: [] } });
  renderNodes();
  expect(screen.getByText("The machines that run sessions, with their load and health.")).toBeTruthy();
});

test("one card per node, each with its state pill; this machine is marked", () => {
  load({ nodes: fleet, health: { items: [] } });
  renderNodes();
  const studio = screen.getByRole("region", { name: "studio" });
  const mini = screen.getByRole("region", { name: "mini" });
  const laptop = screen.getByRole("region", { name: "laptop" });
  const old = screen.getByRole("region", { name: "old" });
  expect(within(studio).getByText("This machine")).toBeTruthy();
  expect(within(mini).queryByText("This machine")).toBeNull();
  const pill = (card: HTMLElement) => card.querySelector("[data-node-state]");
  expect(pill(studio)?.textContent).toBe("Online");
  expect(pill(studio)?.getAttribute("data-node-state")).toBe("online");
  expect(pill(mini)?.textContent).toBe("Draining");
  expect(pill(laptop)?.textContent).toBe("Offline");
  expect(pill(old)?.textContent).toBe("Revoked");
  // Green for online, amber for draining, grey for offline and revoked: never the owner's orange.
  expect(pill(studio)?.querySelector(".bg-success")).toBeTruthy();
  expect(pill(mini)?.querySelector(".bg-warning")).toBeTruthy();
  expect(pill(laptop)?.querySelector(".bg-muted")).toBeTruthy();
  expect(pill(old)?.querySelector(".bg-muted")).toBeTruthy();
  for (const card of [studio, mini, laptop, old]) expect(card.querySelector(".bg-accent, .text-accent")).toBeNull();
  expect(within(laptop).getByText((_, el) => el?.tagName === "P" && el.textContent === "Offline since 14:02")).toBeTruthy();
  expect(within(mini).getByText("linux/amd64 · 0.6.0")).toBeTruthy();
});

test("Add a node shows the join command once the result arrives, with its expiry and one sentence", async () => {
  // Only Date is faked: it is 14:00 on the day the command expires.
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(2026, 10, 4, 14, 0, 0));
  const user = userEvent.setup();
  let answer: (r: CommandResult) => void = () => {};
  createNodeToken.mockReturnValue(new Promise((r) => (answer = r)));
  load({ nodes: fleet, health: { items: [] } });
  renderNodes();
  await user.click(screen.getByRole("button", { name: "Add a node" }));
  expect(createNodeToken).toHaveBeenCalledTimes(1);
  const sheet = screen.getByRole("dialog", { name: "Add a node" });
  expect(within(sheet).queryByRole("code")).toBeNull();
  expect(within(sheet).queryByText(/athena node join/)).toBeNull();
  const command = "athena node join https://studio.vpn:8443 tok-123 --ca-sha256 AB:CD";
  answer(
    create(CommandResultSchema, {
      ok: true,
      result: { case: "nodeToken", value: { command, expiresAt: local(14, 32) } },
    }),
  );
  await waitFor(() => expect(within(sheet).getByText(command)).toBeTruthy());
  expect(within(sheet).getByText(command).tagName).toBe("CODE");
  expect(within(sheet).getByText("Works once, until 14:32")).toBeTruthy();
  expect(within(sheet).getByText("Run this on the new machine, connected to your VPN.")).toBeTruthy();
  expect(within(sheet).getByRole("button", { name: "Copy" })).toBeTruthy();
});

test("a refused Add a node says why in the sheet", async () => {
  const user = userEvent.setup();
  createNodeToken.mockResolvedValue(create(CommandResultSchema, { ok: false, code: "unavailable" }));
  load({ nodes: fleet, health: { items: [] } });
  renderNodes();
  await user.click(screen.getByRole("button", { name: "Add a node" }));
  const sheet = screen.getByRole("dialog", { name: "Add a node" });
  await waitFor(() => expect(within(sheet).getByRole("alert").textContent).toBe("The server is busy. Try again in a moment."));
});

test("a card's menu drains and undrains", async () => {
  const user = userEvent.setup();
  load({ nodes: fleet, health: { items: [] } });
  renderNodes();
  await user.click(screen.getByRole("button", { name: "Actions for studio" }));
  await user.click(await screen.findByRole("menuitem", { name: "Drain" }));
  expect(setNodeDraining).toHaveBeenLastCalledWith("local", true);
  await user.click(screen.getByRole("button", { name: "Actions for mini" }));
  await user.click(await screen.findByRole("menuitem", { name: "Undrain" }));
  expect(setNodeDraining).toHaveBeenLastCalledWith("n-mini", false);
});

test("Revoke asks first, then revokes; this machine and a revoked node offer no Revoke", async () => {
  const user = userEvent.setup();
  load({ nodes: fleet, health: { items: [] } });
  renderNodes();
  await user.click(screen.getByRole("button", { name: "Actions for studio" }));
  expect(screen.queryByRole("menuitem", { name: "Revoke" })).toBeNull();
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("button", { name: "Actions for old" })).toBeNull();

  await user.click(screen.getByRole("button", { name: "Actions for mini" }));
  await user.click(await screen.findByRole("menuitem", { name: "Revoke" }));
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("Revoke mini? Its sessions stop, and it must join again to come back.")).toBeTruthy();
  expect(revokeNode).not.toHaveBeenCalled();
  await user.click(within(dialog).getByRole("button", { name: "Revoke mini" }));
  expect(revokeNode).toHaveBeenCalledWith("n-mini");
});
