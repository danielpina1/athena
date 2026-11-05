import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { act, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, expect, test } from "vitest";
import { SnapshotSchema, type TicketSummarySchema } from "@/gen/athena/v1/state_pb";
import { useAthena } from "@/lib/store/state";
import { Shell } from "./Shell";
import { fakeLink, stubWidth } from "./testkit";
import { AthenaMark } from "./AthenaMark";

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

test("the mark carries the link's state, in words for screen readers", () => {
  load([{ id: "T-1", stage: "working" }]);
  const link = fakeLink("open");
  const el = mark(link);
  expect(el.getAttribute("data-link")).toBe("open");
  expect(screen.getByText("Connected")).toBeTruthy();
  act(() => link.set("connecting"));
  expect(el.getAttribute("data-link")).toBe("connecting");
  expect(screen.getByText("Connecting")).toBeTruthy();
  act(() => link.set("offline"));
  expect(el.getAttribute("data-link")).toBe("offline");
  expect(screen.getByText("Offline")).toBeTruthy();
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
