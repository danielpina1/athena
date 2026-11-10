import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useNavigate } from "react-router";
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

/** A button that goes to `to`, as a link elsewhere on the page or a deep link would. */
function GoTo({ to }: { to: string }) {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => navigate(to)}>
      Go to {to}
    </button>
  );
}

function renderGraph(path = "/graph", mode: Layout = "desktop", goTo?: string) {
  return render(
    <LayoutProvider value={mode}>
      <MemoryRouter initialEntries={[path]}>
        <Graph />
        <Where />
        {goTo && <GoTo to={goTo} />}
      </MemoryRouter>
    </LayoutProvider>,
  );
}

/** Makes the canvas (and every SVG root) measure width × height. Returns the undo. */
function stubCanvasSize(width: number, height: number) {
  Object.defineProperty(SVGSVGElement.prototype, "clientWidth", { configurable: true, get: () => width });
  Object.defineProperty(SVGSVGElement.prototype, "clientHeight", { configurable: true, get: () => height });
  return () => {
    delete (SVGSVGElement.prototype as { clientWidth?: number }).clientWidth;
    delete (SVGSVGElement.prototype as { clientHeight?: number }).clientHeight;
  };
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
  load({ tickets, threads, approvals, nodes: [{ name: "mac-studio" }] });
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

  test("a long title is cut in the SVG, in full in its name, and named once", () => {
    renderGraph();
    const n = node("T-4");
    const text = document.querySelector('svg [data-node-text="T-4"]') as unknown as HTMLElement;
    const shown = within(text).getByText(/^Rewrite W+…$/);
    expect(Array.from(shown.textContent ?? "").length).toBeLessThanOrEqual(22);
    expect(n.getAttribute("aria-label")).toContain(LONG);
    // No <title> as well: it would read the whole title again as the description.
    expect(n.querySelector("title")).toBeNull();
  });

  test("wide titles are clipped to their node", () => {
    useAthena.getState().reset();
    load({
      tickets: [
        { id: "T-W", title: "W".repeat(40), stage: "working", createdAt: at(0) },
        { id: "T-CJK", title: "漢字のタイトルはとても長いのでノードからはみ出す", stage: "idle", createdAt: at(60_000) },
      ],
    });
    renderGraph();
    for (const id of ["T-W", "T-CJK"]) {
      const g = document.querySelector(`svg [data-node-text="${id}"]`)!;
      const n = node(id);
      const box = n.querySelector("rect[data-box]")!;
      // The text group sits at the node's corner and is clipped to a box no bigger than the node.
      expect(g.getAttribute("transform")).toBe(`translate(${box.getAttribute("x")} ${box.getAttribute("y")})`);
      const ref = /^url\(#(.+)\)$/.exec(g.getAttribute("clip-path") ?? "")?.[1];
      expect(ref, id).toBeTruthy();
      const clip = document.getElementById(ref!)!;
      expect(clip.tagName.toLowerCase()).toBe("clippath");
      const r = clip.querySelector("rect")!;
      expect(Number(r.getAttribute("x"))).toBeGreaterThanOrEqual(0);
      expect(Number(r.getAttribute("x")) + Number(r.getAttribute("width"))).toBeLessThanOrEqual(Number(box.getAttribute("width")));
      expect(Number(r.getAttribute("height"))).toBeLessThanOrEqual(Number(box.getAttribute("height")));
      // The cut ends in a fade, not a sliced glyph: a mask whose gradient runs out before the clip's edge.
      const maskRef = /^url\(#(.+)\)$/.exec(g.getAttribute("mask") ?? "")?.[1];
      const mask = document.getElementById(maskRef ?? "")!;
      expect(mask?.tagName.toLowerCase(), id).toBe("mask");
      const fill = /^url\(#(.+)\)$/.exec(mask.querySelector("rect")!.getAttribute("fill") ?? "")?.[1];
      expect(document.getElementById(fill ?? "")?.tagName.toLowerCase()).toBe("lineargradient");
    }
    // A running node's title stops well clear of its ring (cx = w − 12, r = 4).
    const ring = document.getElementById(
      /^url\(#(.+)\)$/.exec(document.querySelector('svg [data-node-text="T-W"]')!.getAttribute("clip-path") ?? "")![1],
    )!;
    expect(Number(ring.querySelector("rect")!.getAttribute("width"))).toBeLessThanOrEqual(176 - 28);
  });

  test("the lane labels let taps through to the nodes under them", () => {
    renderGraph();
    expect(document.querySelector("svg [data-lane-labels]")!.getAttribute("pointer-events")).toBe("none");
  });

  test("a selected Ticket off-screen is brought into view", () => {
    // Reduced motion: the pan is a jump, not an animation.
    stubReducedMotion(true);
    const undo = stubCanvasSize(300, 200);
    try {
      // T-4, the newest, sits far to the right; opened by its link, the canvas pans just enough to show it.
      renderGraph("/graph?ticket=T-1");
      const view = () => {
        const m = /translate\(([-\d.]+) ([-\d.]+)\) scale\(([\d.]+)\)/.exec(transform() ?? "")!;
        return { x: Number(m[1]), y: Number(m[2]), k: Number(m[3]) };
      };
      const inView = (id: string) => {
        const box = node(id).querySelector("rect[data-box]")!;
        const v = view();
        const x = v.x + Number(box.getAttribute("x")) * v.k;
        const y = v.y + Number(box.getAttribute("y")) * v.k;
        const w = Number(box.getAttribute("width")) * v.k;
        const h = Number(box.getAttribute("height")) * v.k;
        return x >= 0 && y >= 0 && x + w <= 300 && y + h <= 200;
      };
      expect(inView("T-1")).toBe(true);
      fireEvent.click(node("T-4"));
      expect(where()).toBe("/graph?ticket=T-4");
      expect(inView("T-4")).toBe(true);
    } finally {
      undo();
    }
  });

  test("a pane opened by a link gives focus back to its own node, not an earlier one", async () => {
    renderGraph("/graph", "desktop", "/graph?ticket=T-3");
    await userEvent.click(node("T-1"));
    await userEvent.keyboard("{Escape}");
    expect(document.activeElement).toBe(node("T-1"));
    await userEvent.click(screen.getByRole("button", { name: "Go to /graph?ticket=T-3" }));
    expect(screen.getByRole("complementary", { name: "Ticket" })).toBeTruthy();
    await userEvent.keyboard("{Escape}");
    expect(document.activeElement).toBe(node("T-3"));
  });

  test("while the replay plays on the phone the hidden list stays, with every Ticket", () => {
    stubReducedMotion(true);
    vi.useFakeTimers();
    renderGraph("/graph", "phone");
    fireEvent.click(screen.getByRole("button", { name: "Replay" }));
    expect(node("T-4")).toBeNull();
    const list = document.querySelector("[data-hidden-list]");
    expect(list).toBeTruthy();
    for (const t of tickets) expect(list!.querySelector(`[data-list-node="${t.id}"]`), t.id).toBeTruthy();
  });

  test("the phone starts at the left edge with readable titles and the lane labels in view", () => {
    const width = Object.getOwnPropertyDescriptor(Element.prototype, "clientWidth");
    Object.defineProperty(SVGSVGElement.prototype, "clientWidth", { configurable: true, get: () => 375 });
    try {
      renderGraph("/graph", "phone");
      const m = /translate\(([-\d.]+) ([-\d.]+)\) scale\(([\d.]+)\)/.exec(transform() ?? "")!;
      expect(Number(m[1])).toBe(0);
      // A 12.5 px title at this zoom is at least 11 px on the screen.
      expect(12.5 * Number(m[3])).toBeGreaterThanOrEqual(11);
      // The labels are in a layer that moves only up and down, at the left edge.
      const labels = document.querySelector("svg [data-lane-labels]")!;
      expect(labels.closest("[data-viewport]")).toBeNull();
      expect(labels.getAttribute("transform")).toMatch(/^translate\(0 [-\d.]+\)$/);
      const texts = [...labels.querySelectorAll("text")];
      expect(texts.map((t) => t.textContent)).toEqual(["GITHUB · ACME/API", "LOCAL · OPS", "NO REPOSITORY"]);
      for (const t of texts) expect(Number(t.getAttribute("x"))).toBeLessThan(375);
    } finally {
      if (width) Object.defineProperty(SVGSVGElement.prototype, "clientWidth", width);
      else delete (SVGSVGElement.prototype as { clientWidth?: number }).clientWidth;
    }
  });

  test("on the phone a screen reader reads the hidden list, not the picture too", () => {
    renderGraph("/graph", "phone");
    const svg = document.querySelector("svg[data-canvas]")!;
    expect(svg.getAttribute("aria-hidden")).toBe("true");
    expect(node("T-1").getAttribute("tabindex")).toBe("-1");
    expect(document.querySelector('[data-hidden-list] [data-list-node="T-1"]')).toBeTruthy();
  });

  test("on the desktop the nodes are the reading order", () => {
    renderGraph();
    expect(document.querySelector("svg[data-canvas]")!.getAttribute("aria-hidden")).toBeNull();
    expect(node("T-1").getAttribute("tabindex")).toBe("0");
    expect(document.querySelector("[data-hidden-list]")).toBeNull();
  });

  test("the phone folds the legend into a disclosure", async () => {
    renderGraph("/graph", "phone");
    const toggle = screen.getByText("Legend", { selector: "summary" });
    const details = toggle.closest("details")!;
    expect(details.open).toBe(false);
    await userEvent.click(toggle);
    expect(details.open).toBe(true);
    expect(within(details).getByRole("region", { name: "Legend" })).toBeTruthy();
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
    // Its faint halo pulses (CSS only); other Tickets have none.
    const glow = node("T-1").querySelector("[data-glow]")!;
    const box = node("T-1").querySelector("[data-box]")!;
    expect(glow.classList.contains("motion-glow")).toBe(true);
    // A halo just outside the box, for the eyes only.
    expect(Number(glow.getAttribute("x"))).toBe(Number(box.getAttribute("x")) - 3);
    expect(Number(glow.getAttribute("width"))).toBe(Number(box.getAttribute("width")) + 6);
    expect(glow.getAttribute("aria-hidden")).toBe("true");
    expect(node("T-2").querySelector("[data-glow]")).toBeNull();
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
    // The graph stays beside it, its node marked; the legend steps aside so the graph keeps its width.
    expect(node("T-2")).toBeTruthy();
    expect(node("T-1").getAttribute("aria-current")).toBe("true");
    expect(screen.queryByRole("region", { name: "Legend" })).toBeNull();
    await userEvent.click(within(pane).getByRole("button", { name: "Close" }));
    expect(where()).toBe("/graph");
    // Focus goes back to the node that opened the pane.
    expect(document.activeElement).toBe(node("T-1"));
    expect(screen.getByRole("region", { name: "Legend" })).toBeTruthy();
  });

  test("Esc closes the pane and focus returns to the node, or to the graph when it is gone", async () => {
    renderGraph();
    await userEvent.click(node("T-2"));
    const pane = screen.getByRole("complementary", { name: "Ticket" });
    await userEvent.click(within(pane).getByRole("button", { name: "Reply" }));
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("complementary", { name: "Ticket" })).toBeNull();
    expect(document.activeElement).toBe(node("T-2"));

    // T-1 is not in "Needs you only": once filtered out, closing its pane focuses the graph.
    await userEvent.click(node("T-1"));
    await userEvent.click(screen.getByRole("button", { name: "Needs you only" }));
    expect(node("T-1")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(document.activeElement).toBe(document.querySelector("svg[data-canvas]"));
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

test("graph says waiting before the first snapshot", () => {
  useAthena.getState().reset();
  renderGraph();
  expect(screen.getByText("Waiting for the server.")).toBeTruthy();
  expect(screen.queryByText("No Tickets yet")).toBeNull();
});
