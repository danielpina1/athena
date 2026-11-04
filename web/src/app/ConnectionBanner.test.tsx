import { act, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { ConnectionBanner } from "@/app/ConnectionBanner";
import { fakeLink } from "@/app/testkit";

afterEach(() => vi.useRealTimers());

const UNREACHABLE = /Control server unreachable — retrying/;

test("banner appears only after 1 s offline", () => {
  const t0 = new Date(2026, 10, 4, 15, 0, 0);
  vi.useFakeTimers({ now: t0 });
  const link = fakeLink("open");
  render(<ConnectionBanner link={link} />);
  expect(screen.queryByRole("status")).toBeNull();

  act(() => link.frame()); // the last update: 15:00
  act(() => vi.advanceTimersByTime(5 * 60_000));
  act(() => link.set("offline"));
  expect(screen.queryByText(UNREACHABLE)).toBeNull();
  act(() => vi.advanceTimersByTime(999));
  expect(screen.queryByText(UNREACHABLE)).toBeNull();
  act(() => vi.advanceTimersByTime(1));
  const banner = screen.getByRole("status");
  expect(banner.textContent).toMatch(UNREACHABLE);
  expect(banner.querySelector("time")?.getAttribute("datetime")).toBe(t0.toISOString());

  // Back online: gone at once.
  act(() => link.set("open"));
  expect(screen.queryByRole("status")).toBeNull();

  // A reconnect that takes over 1 s says so; a quick one says nothing.
  act(() => link.set("connecting"));
  act(() => vi.advanceTimersByTime(500));
  act(() => link.set("open"));
  act(() => vi.advanceTimersByTime(1000));
  expect(screen.queryByRole("status")).toBeNull();
  act(() => link.set("connecting"));
  act(() => vi.advanceTimersByTime(1000));
  expect(screen.getByRole("status").textContent).toBe("Connecting…");

  // Signed out is the sign-in screen's business.
  act(() => link.set("signedOut"));
  act(() => vi.advanceTimersByTime(2000));
  expect(screen.queryByRole("status")).toBeNull();
});
