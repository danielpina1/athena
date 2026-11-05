import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { act, render } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, expect, test, vi } from "vitest";
import { SnapshotSchema, type TicketSummarySchema } from "@/gen/athena/v1/state_pb";
import { useAthena } from "@/lib/store/state";
import { Shell } from "./Shell";
import { fakeLink, stubWidth } from "./testkit";
import { AthenaMark } from "./AthenaMark";
import { BANNER_DELAY_MS } from "./ConnectionBanner";

afterEach(() => useAthena.getState().reset());

const load = (tickets: MessageInitShape<typeof TicketSummarySchema>[]) =>
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n, tickets }), "e1");
const mark = (link = fakeLink("open")) => render(<AthenaMark link={link} />).container.querySelector("[data-mark]")!;

test("the mark is idle, working or needs you", () => {
  expect(mark().getAttribute("data-state")).toBe("idle");
  load([{ id: "T-1", stage: "idle" }]);
  expect(mark().getAttribute("data-state")).toBe("idle");
  load([{ id: "T-1", stage: "working" }]);
  expect(mark().getAttribute("data-state")).toBe("working");
  load([{ id: "T-1", stage: "working" }, { id: "T-2", stage: "person", action: "review" }]);
  expect(mark().getAttribute("data-state")).toBe("needs-you");
});

test("the mark carries the link's state after the banner's delay, in words after Athena's name", () => {
  vi.useFakeTimers();
  try {
    load([{ id: "T-1", stage: "working" }]);
    const link = fakeLink("open");
    const view = render(<AthenaMark link={link}>Athena</AthenaMark>);
    const el = view.container.querySelector("[data-mark]")!;
    const said = () => view.container.textContent;
    expect(el.getAttribute("data-link")).toBe("open");
    expect(said()).toBe("Athena, Connected");
    // A reconnect blip shorter than the banner's delay does not show.
    act(() => link.set("connecting"));
    expect(el.getAttribute("data-link")).toBe("open");
    expect(said()).toBe("Athena, Connected");
    act(() => vi.advanceTimersByTime(BANNER_DELAY_MS - 1));
    expect(el.getAttribute("data-link")).toBe("open");
    act(() => vi.advanceTimersByTime(1));
    expect(el.getAttribute("data-link")).toBe("connecting");
    expect(said()).toBe("Athena, Connecting");
    act(() => link.set("offline"));
    expect(el.getAttribute("data-link")).toBe("offline");
    expect(said()).toBe("Athena, Offline");
    // Back open at once.
    act(() => link.set("open"));
    expect(el.getAttribute("data-link")).toBe("open");
    act(() => link.set("offline"));
    expect(el.getAttribute("data-link")).toBe("open");
  } finally {
    vi.useRealTimers();
  }
});

test("offline has a cue besides its colour: a slash through a hollow mark", () => {
  const css = readFileSync(join(import.meta.dirname, "..", "index.css"), "utf8");
  const offline = /\.athena-mark\[data-link="offline"\]::before,\s*\.athena-mark\[data-link="signedOut"\]::before\s*\{([^}]*)\}/.exec(css)?.[1] ?? "";
  expect(offline).toMatch(/transform:\s*rotate\(-45deg\)/);
  expect(offline).toMatch(/height:\s*1\.5px/);
});

test.each([
  ["phone", 390],
  ["desktop", 1440],
])("the %s header has one square beside the name: the mark", (_, width) => {
  stubWidth(width);
  const view = render(
    <MemoryRouter>
      <Shell link={fakeLink("open")} />
    </MemoryRouter>,
  );
  // The bar at the top: the sidebar's on the desktop, the header's on the phone.
  const bar = view.container.querySelector(width >= 1024 ? "aside > div" : "header > div")!;
  expect(bar.textContent).toContain("Athena");
  expect(bar.querySelectorAll("[data-mark]")).toHaveLength(1);
  expect(bar.querySelectorAll("[data-status]")).toHaveLength(0);
  view.unmount();
});
