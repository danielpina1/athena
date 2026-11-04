import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { NodeStatusSchema, SnapshotSchema } from "@/gen/athena/v1/state_pb";
import { Shell } from "@/app/Shell";
import { fakeLink, stubWidth, Where } from "@/app/testkit";
import { CHORD_MS } from "@/app/shortcuts";
import { useAthena } from "@/lib/store/state";

type SnapshotInit = MessageInitShape<typeof SnapshotSchema>;

function load(init: SnapshotInit) {
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n, ...init }), "e1");
}

function renderShell(path: string, width: number) {
  stubWidth(width);
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Shell link={fakeLink("open")} />
      <Where />
    </MemoryRouter>,
  );
}

const where = () => screen.getByTestId("where").textContent;

beforeEach(() => useAthena.getState().reset());
afterEach(() => vi.useRealTimers());

test("phone shows bottom nav with needs-you count", () => {
  load({
    tickets: [
      { id: "T-1", title: "Deploy", stage: "person", action: "approval", pendingApprovalId: "A-1" },
      { id: "T-2", title: "Question", stage: "person", action: "answer" },
      { id: "T-3", title: "Busy", stage: "working" },
    ],
    approvals: [{ id: "A-1", ticketId: "T-1", state: "pending" }],
  });
  renderShell("/", 390);

  const nav = screen.getByRole("navigation", { name: "Main" });
  const links = within(nav).getAllByRole("link");
  expect(links.map((l) => l.getAttribute("href"))).toEqual(["/", "/tickets", "/new", "/graph", "/settings"]);
  const needsYou = links[0];
  expect(needsYou.getAttribute("aria-current")).toBe("page");
  expect(needsYou.textContent).toBe("Needs you2");
  // The phone has no sidebar: Routing and Nodes are reached from Settings and the palette.
  expect(screen.queryByText("Today")).toBeNull();

  // An approval answered here leaves the count at once (ruling R30).
  act(() => void useAthena.getState().markApprovalAnswered("A-1", true));
  expect(needsYou.textContent).toBe("Needs you1");
  act(() => useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 2n }), "e1"));
  expect(needsYou.textContent).toBe("Needs you");
});

test("desktop shows three panes at 1024", async () => {
  load({
    tickets: [
      { id: "T-1", title: "Open one", stage: "working" },
      { id: "T-2", title: "Open two", stage: "idle" },
    ],
    node: create(NodeStatusSchema, { name: "mac-studio", slots: 4, slotsInUse: 1, queued: 2, liveTicketIds: ["T-1"] }),
  });
  renderShell("/tickets", 1024);

  const nav = screen.getByRole("navigation", { name: "Main" });
  expect(within(nav).getAllByRole("link").map((l) => l.textContent)).toEqual([
    "Needs you",
    "Tickets",
    "New",
    "Graph",
    "Routing",
    "Nodes",
    "Settings",
  ]);
  const node = screen.getByRole("region", { name: "Node" });
  expect(node.textContent).toContain("mac-studio");
  expect(within(node).getByText("1/4")).toBeTruthy();
  expect(within(node).getByTestId("queued").textContent).toBe("2");
  expect(within(node).getByTestId("live").textContent).toBe("1");
  const today = screen.getByRole("region", { name: "Today" });
  expect(within(today).getByTestId("merged").textContent).toBe("0");
  expect(within(today).getByTestId("open").textContent).toBe("2");

  const main = screen.getByRole("main");
  expect(main.querySelector('[data-pane="list"]')).not.toBeNull();
  expect(main.querySelector('[data-pane="detail"]')).not.toBeNull();

  // A route with no list spans both panes.
  await userEvent.click(within(nav).getByRole("link", { name: "New" }));
  expect(main.querySelector('[data-pane="list"]')).toBeNull();
  expect(main.querySelector('[data-pane="full"]')).not.toBeNull();
});

test("g n chord navigates", () => {
  vi.useFakeTimers();
  renderShell("/tickets", 1440);
  fireEvent.keyDown(document.body, { key: "g" });
  fireEvent.keyDown(document.body, { key: "n" });
  expect(where()).toBe("/");

  fireEvent.keyDown(document.body, { key: "g" });
  fireEvent.keyDown(document.body, { key: "t" });
  expect(where()).toBe("/tickets");

  // Too slow: not a chord.
  fireEvent.keyDown(document.body, { key: "g" });
  act(() => vi.advanceTimersByTime(CHORD_MS + 1));
  fireEvent.keyDown(document.body, { key: "g" });
  act(() => vi.advanceTimersByTime(CHORD_MS + 1));
  fireEvent.keyDown(document.body, { key: "n" });
  expect(where()).toBe("/tickets");

  fireEvent.keyDown(document.body, { key: "g" });
  fireEvent.keyDown(document.body, { key: "g" });
  expect(where()).toBe("/graph");
});

test("unknown paths show the 404 inside the shell", () => {
  renderShell("/nowhere", 390);
  expect(screen.getByRole("heading", { name: "Page not found" })).toBeTruthy();
  expect(screen.getByRole("navigation", { name: "Main" })).toBeTruthy();
});

test("? opens the shortcut list", () => {
  renderShell("/", 1440);
  fireEvent.keyDown(document.body, { key: "?" });
  const list = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
  expect(list.textContent).toContain("Go to Needs you");
});
