import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, expect, test } from "vitest";
import { useAthena } from "@/lib/store/state";
import { at, load } from "../tickets/fixtures";
import { Nodes } from "./Nodes";

afterEach(() => useAthena.getState().reset());

function renderNodes() {
  return render(
    <MemoryRouter>
      <Nodes />
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
