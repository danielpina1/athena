import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayoutProvider, type Layout } from "@/app/Panes";
import { Where } from "@/app/testkit";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { at, load } from "../tickets/fixtures";
import { Graph } from "./Graph";
import { GraphList } from "./GraphList";
import { layout } from "./layout";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  watch: vi.fn(),
  unwatch: vi.fn(),
  onWatchLost: vi.fn(),
}));
const ok = { ok: true, code: "" } as CommandResult;

const T = "github:acme/api#12";
const LONG = `Rewrite ${"W".repeat(2000)}`;
const tickets = [
  { id: "T-1", title: "Refactor auth client", stage: "working", repo: "acme/api", createdAt: at(0) },
  { id: "T-2", title: "Fix flaky auth test", stage: "person", action: "answer", repo: "acme/api", createdAt: at(60_000), attentionQuestion: "Retry in client or helper?" },
  { id: "T-3", title: "Approve the deploy", stage: "person", action: "approval", pendingApprovalId: "A-1", dir: "/srv/ops", createdAt: at(120_000) },
  { id: "T-4", title: LONG, stage: "working", action: "queued", createdAt: at(180_000) },
];
const threads = [{ key: T, queued: 0, decisions: 1, tickets: ["T-1(own)", "T-2"] }];
const approvals = [{ id: "A-1", ticketId: "T-3", state: "pending", createdAt: at(120_000) }];

function renderGraph(path = "/graph", mode: Layout = "desktop") {
  return render(
    <LayoutProvider value={mode}>
      <MemoryRouter initialEntries={[path]}>
        <Graph />
        <Where />
      </MemoryRouter>
    </LayoutProvider>,
  );
}
const where = () => screen.getByTestId("where").textContent;
const node = (id: string) => document.querySelector<SVGAElement>(`svg [data-node="${id}"]`)!;
const transform = () => document.querySelector("svg [data-viewport]")?.getAttribute("transform");

function stubReducedMotion(reduce: boolean) {
  window.matchMedia = ((query: string) =>
    ({
      matches: reduce && query.includes("prefers-reduced-motion"),
      media: query,
      addEventListener() {},
      removeEventListener() {},
    }) as unknown as MediaQueryList) as typeof window.matchMedia;
}

beforeEach(() => {
  vi.mocked(actions.watch).mockResolvedValue(ok);
  vi.mocked(actions.unwatch).mockResolvedValue(ok);
  vi.mocked(actions.onWatchLost).mockReturnValue(() => {});
  vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})));
  load({ tickets, threads, approvals, node: { name: "mac-studio" } });
});
afterEach(() => {
  vi.useRealTimers();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
  // @ts-expect-error jsdom has none; tests that stub it put it back.
  delete window.matchMedia;
  useAthena.getState().reset();
});

describe("Graph", () => {
  test("every node is focusable with its title and id as its name", () => {
    renderGraph();
    for (const t of tickets) {
      const n = node(t.id);
      expect(n, t.id).toBeTruthy();
      expect(n.getAttribute("href")).toBe(`/tickets/${t.id}`);
      expect(n.getAttribute("aria-label")).toContain(t.id);
    }
    expect(node("T-2").getAttribute("aria-label")).toBe("Fix flaky auth test, T-2, Needs your answer");
    expect(screen.getByRole("link", { name: /Refactor auth client, T-1, Working/ })).toBe(node("T-1"));
  });

  test("a long title is cut in the SVG, in full in its title and name", () => {
    renderGraph();
    const n = node("T-4");
    const text = document.querySelector('svg [data-node-text="T-4"]') as unknown as HTMLElement;
    const shown = within(text).getByText(/^Rewrite W+…$/);
    expect(Array.from(shown.textContent ?? "").length).toBeLessThanOrEqual(22);
    expect(n.querySelector("title")?.textContent).toBe(`${LONG} · T-4`);
    expect(n.getAttribute("aria-label")).toContain(LONG);
  });

  test("orange marks only what needs you, by the Needs you rule", () => {
    // T-3's approval is being answered here: it no longer waits on the owner (R30).
    useAthena.getState().markApprovalAnswered("A-1", true);
    renderGraph();
    expect(node("T-2").dataset.look).toBe("needs-you");
    expect(node("T-3").dataset.look).not.toBe("needs-you");
    expect(node("T-1").dataset.look).toBe("running");
    // The running Ticket carries a ring.
    expect(node("T-1").querySelector("[data-ring]")).toBeTruthy();
    expect(node("T-2").querySelector("[data-ring]")).toBeNull();
  });

  test("needs-you filter keeps the URL and shows only what needs you", async () => {
    renderGraph();
    await userEvent.click(screen.getByRole("button", { name: "Needs you only" }));
    expect(where()).toBe("/graph?needsYou=1");
    expect(node("T-2")).toBeTruthy();
    expect(node("T-3")).toBeTruthy();
    expect(node("T-1")).toBeNull();
    expect(node("T-4")).toBeNull();
  });

  test("lanes by repository, or one lane by machine, in the URL", async () => {
    renderGraph();
    const lanes = () => [...document.querySelectorAll("svg [data-lane]")].map((l) => l.getAttribute("data-label"));
    expect(lanes()).toEqual(["GITHUB · ACME/API", "LOCAL · ops", "No repository"]);
    await userEvent.click(screen.getByRole("radio", { name: "By machine" }));
    expect(where()).toBe("/graph?groupBy=machine");
    expect(lanes()).toEqual(["mac-studio"]);
  });

  test("hover or focus shows the title and id", () => {
    renderGraph();
    const selected = screen.getByRole("region", { name: "Selected" });
    act(() => node("T-2").focus());
    expect(within(selected).getByText("Fix flaky auth test")).toBeTruthy();
    expect(within(selected).getByText(/T-2/)).toBeTruthy();
    expect(within(selected).getByText(/Retry in client or helper\?/)).toBeTruthy();
    fireEvent.mouseEnter(node("T-1"));
    expect(within(selected).getByText("Refactor auth client")).toBeTruthy();
  });

  test("Enter on a node opens the Ticket in the detail pane on the desktop", async () => {
    renderGraph();
    act(() => node("T-1").focus());
    await userEvent.keyboard("{Enter}");
    expect(where()).toBe("/graph?ticket=T-1");
    const pane = screen.getByRole("complementary", { name: "Ticket" });
    expect(within(pane).getByRole("heading", { name: "Refactor auth client", level: 1 })).toBeTruthy();
    // The graph stays beside it.
    expect(node("T-2")).toBeTruthy();
    await userEvent.click(within(pane).getByRole("button", { name: "Close" }));
    expect(where()).toBe("/graph");
  });

  test("a tap on a node navigates to the Ticket on the phone", async () => {
    renderGraph("/graph", "phone");
    await userEvent.click(node("T-2"));
    expect(where()).toBe("/tickets/T-2");
  });

  test("+, - and 0 zoom from 0.25× to 4×", async () => {
    stubReducedMotion(true);
    renderGraph();
    const k = () => Number(/scale\(([\d.]+)\)/.exec(transform() ?? "")?.[1]);
    expect(k()).toBe(1);
    await userEvent.keyboard("+");
    expect(k()).toBeGreaterThan(1);
    for (let i = 0; i < 20; i++) await userEvent.keyboard("+");
    expect(k()).toBe(4);
    for (let i = 0; i < 40; i++) await userEvent.keyboard("-");
    expect(k()).toBe(0.25);
    await userEvent.keyboard("0");
    expect(k()).toBe(1);
  });

  test("replay runs the whole window in 10 s; with reduced motion it jumps", () => {
    stubReducedMotion(true);
    vi.useFakeTimers();
    renderGraph();
    fireEvent.click(screen.getByRole("button", { name: "Replay" }));
    // At the start of the window only the oldest Ticket exists.
    expect(node("T-1")).toBeTruthy();
    expect(node("T-2")).toBeNull();
    act(() => vi.advanceTimersByTime(1000));
    const scrub = screen.getByRole("slider", { name: "Time" }) as HTMLInputElement;
    const first = Number(scrub.value);
    act(() => vi.advanceTimersByTime(1000));
    expect(Number(scrub.value)).toBeGreaterThan(first);
    act(() => vi.advanceTimersByTime(8000));
    // Back to now, every Ticket shown, the replay done.
    expect(node("T-4")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Replay" })).toBeTruthy();
    // The scrubber is local state, never the URL.
    expect(where()).toBe("/graph");
  });

  test("the scrubber shows the past", () => {
    renderGraph();
    const scrub = screen.getByRole("slider", { name: "Time" }) as HTMLInputElement;
    fireEvent.change(scrub, { target: { value: String(Number(scrub.min) + 90_000) } });
    expect(node("T-2")).toBeTruthy();
    expect(node("T-3")).toBeNull();
    expect(where()).toBe("/graph");
  });

  test("the list toggle shows the list on any size, in the URL", async () => {
    renderGraph();
    await userEvent.click(screen.getByRole("radio", { name: "List" }));
    expect(where()).toBe("/graph?view=list");
    expect(document.querySelector("svg [data-node]")).toBeNull();
    expect(screen.getByRole("link", { name: /Fix flaky auth test/ }).getAttribute("href")).toBe("/tickets/T-2");
  });

  test("the legend matches the mockup", () => {
    renderGraph();
    const legend = screen.getByRole("region", { name: "Legend" });
    const items = [...legend.querySelectorAll("[data-legend]")].map((i) => i.textContent);
    expect(items).toEqual([
      "Running",
      "Needs you",
      "Ready for review",
      "Queued · waiting",
      "Ended",
      "Same Thread",
      "Follow-up",
      "Hand-off",
      "Waits for",
      "Agents talking",
      "Same files touched",
    ]);
  });

  test("nothing yet says so", () => {
    useAthena.getState().reset();
    load({});
    renderGraph();
    expect(screen.getByText("No Tickets yet")).toBeTruthy();
  });
});

describe("GraphList", () => {
  test("list fallback has every node", () => {
    const decisions = [
      { id: "d-1", at: at(1000), kind: "accept", thread: T, ticketId: "T-1" },
      { id: "d-2", at: at(200_000), kind: "ignore", thread: T, reasons: [{ code: "routed", values: { ticket: "T-2" } }] },
    ];
    load({ tickets, threads, decisions });
    const s = useAthena.getState();
    const m = layout(s.tickets.values(), s.threads.values(), s.decisions.values(), {
      groupBy: "repo",
      now: new Date(Date.parse("2026-11-04T10:00:00Z")),
    });
    render(
      <MemoryRouter>
        <GraphList model={m} />
      </MemoryRouter>,
    );
    const list = screen.getByRole("region", { name: "Tickets as a list" });
    for (const t of tickets) {
      const row = list.querySelector(`[data-list-node="${t.id}"]`) as HTMLElement;
      expect(row, t.id).toBeTruthy();
      expect(within(row).getByRole("link").getAttribute("href")).toBe(`/tickets/${t.id}`);
    }
    // The lanes are headings, each node says its stage, and edges are in words.
    expect(within(list).getAllByRole("heading").map((h) => h.textContent)).toEqual([
      "GITHUB · ACME/API",
      "LOCAL · ops",
      "No repository",
    ]);
    const row = (id: string) => list.querySelector(`[data-list-node="${id}"]`) as HTMLElement;
    expect(row("T-2").textContent).toContain("Needs your answer");
    expect(row("T-4").textContent).toContain("Queued");
    expect(row("T-1").textContent).toContain(`Same Thread as T-2 (${T})`);
    expect(row("T-2").textContent).toContain(`Same Thread as T-1 (${T})`);
    expect(row("T-1").textContent).toContain("Follow-up routed to T-2");
    expect(row("T-2").textContent).toContain("Follow-up from T-1");
    expect(row("T-3").textContent).toContain("No links");
  });

  test("the list shows 200 rows, then Show more", async () => {
    const many = Array.from({ length: 250 }, (_, i) => ({ id: `T-${i}`, title: `Task ${i}`, stage: "idle", createdAt: at(i * 1000) }));
    load({ tickets: many });
    const s = useAthena.getState();
    const m = layout(s.tickets.values(), [], [], { groupBy: "repo", now: new Date(Date.parse("2026-11-04T10:00:00Z")) });
    render(
      <MemoryRouter>
        <GraphList model={m} />
      </MemoryRouter>,
    );
    expect(document.querySelectorAll("[data-list-node]")).toHaveLength(200);
    await userEvent.click(screen.getByRole("button", { name: "Show more" }));
    expect(document.querySelectorAll("[data-list-node]")).toHaveLength(250);
  });
});
