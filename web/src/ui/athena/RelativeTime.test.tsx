import { act, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { formatRelative, RelativeTime } from "@/ui/athena/RelativeTime";

const NOW = new Date("2026-11-04T12:00:00Z").getTime();
const MIN = 60_000;

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
});
afterEach(() => vi.useRealTimers());

test("formatRelative is compact and honest about direction", () => {
  expect(formatRelative(NOW - 10_000, NOW)).toBe("just now");
  expect(formatRelative(NOW - 4 * MIN, NOW)).toBe("4m ago");
  expect(formatRelative(NOW - 3 * 60 * MIN, NOW)).toBe("3h ago");
  expect(formatRelative(NOW - 2 * 24 * 60 * MIN, NOW)).toBe("2d ago");
  expect(formatRelative(NOW + 5 * MIN, NOW)).toBe("in 5m");
  expect(formatRelative(new Date("2026-10-12T09:00:00Z").getTime(), NOW)).toBe("12 Oct");
  expect(formatRelative(new Date("2025-10-12T09:00:00Z").getTime(), NOW)).toBe("12 Oct 2025");
});

test("RelativeTime refreshes once a minute through a single shared timer", () => {
  const spy = vi.spyOn(globalThis, "setInterval");
  const { unmount } = render(
    <>
      <RelativeTime at={NOW - 4 * MIN} />
      <RelativeTime at={NOW - 10 * MIN} />
      <RelativeTime at={new Date(NOW - 20 * MIN)} />
    </>,
  );
  expect(spy).toHaveBeenCalledTimes(1);
  expect(screen.getByText("4m ago").tagName).toBe("TIME");
  act(() => vi.advanceTimersByTime(MIN));
  expect(screen.getByText("5m ago")).toBeTruthy();
  expect(screen.getByText("11m ago")).toBeTruthy();
  const cleared = vi.spyOn(globalThis, "clearInterval");
  unmount();
  expect(cleared).toHaveBeenCalledTimes(1);
});
