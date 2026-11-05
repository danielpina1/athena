import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { type ActivityItem, useActivity } from "@/lib/store/activity";
import { useAthena } from "@/lib/store/state";
import { LiveFeed } from "./LiveFeed";

let n = 0;
const item = (ticketId: string, event: string, ago = 0): ActivityItem => ({
  id: ++n,
  key: `${ticketId}:${event}:${n}`,
  ticketId,
  title: `title ${ticketId}`,
  event,
  tone: "neutral",
  at: Date.now() - ago,
  count: 1,
});
const push = (...items: ActivityItem[]) => act(() => useActivity.setState((s) => ({ items: [...items, ...s.items] })));

beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
afterEach(() => {
  vi.useRealTimers();
  useActivity.setState({ items: [] });
  useAthena.getState().reset();
});

const feed = (props: Parameters<typeof LiveFeed>[0] = {}) =>
  render(
    <MemoryRouter>
      <LiveFeed {...props} />
    </MemoryRouter>,
  );

test("an empty feed says it is quiet", () => {
  feed();
  expect(screen.getByText("Quiet for now. New events will show here.")).toBeTruthy();
});

test("each row links its Ticket, says the event and how long ago", () => {
  useActivity.setState({ items: [item("T-2", "Needs your approval"), item("T-1", "Started work", 2 * 60_000 + 5_000)] });
  feed();
  const rows = within(screen.getByRole("list", { name: "Live activity" })).getAllByRole("listitem");
  expect(rows.map((r) => r.textContent)).toEqual([
    expect.stringMatching(/Needs your approval.*T-2.*now/),
    expect.stringMatching(/Started work.*T-1.*2m/),
  ]);
  expect(within(rows[0]).getByRole("link", { name: /T-2/ }).getAttribute("href")).toBe("/tickets/T-2");
});

test("new rows wash in, and only new items are announced, throttled", async () => {
  useActivity.setState({ items: [item("T-1", "Started work")] });
  feed();
  const live = screen.getByTestId("live-announcer");
  expect(live.getAttribute("aria-live")).toBe("polite");
  expect(live.textContent).toBe("");
  expect(screen.getAllByRole("listitem")[0].classList.contains("motion-wash")).toBe(false);

  push(item("T-2", "Needs your approval"));
  expect(screen.getAllByRole("listitem")[0].classList.contains("motion-wash")).toBe(true);
  await act(async () => vi.advanceTimersByTimeAsync(10));
  expect(live.textContent).toBe("T-2: Needs your approval");

  // Two more within the throttle window: one announcement for both, after it.
  push(item("T-3", "Finished"));
  push(item("T-4", "Opened PR #3"));
  await act(async () => vi.advanceTimersByTimeAsync(10));
  expect(live.textContent).toBe("T-2: Needs your approval");
  await act(async () => vi.advanceTimersByTimeAsync(5_000));
  expect(live.textContent).toBe("2 new events. Latest: T-4: Opened PR #3");
});

test("the page form shows the latest 5, then all", async () => {
  const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
  useActivity.setState({ items: Array.from({ length: 8 }, (_, i) => item(`T-${i}`, "Started work")) });
  feed({ limit: 5 });
  expect(screen.getAllByRole("listitem")).toHaveLength(5);
  await user.click(screen.getByRole("button", { name: "Show all (8)" }));
  expect(screen.getAllByRole("listitem")).toHaveLength(8);
});
