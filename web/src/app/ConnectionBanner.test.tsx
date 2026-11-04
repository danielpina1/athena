import { act, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { ConnectionBanner } from "@/app/ConnectionBanner";
import { fakeLink } from "@/app/testkit";

afterEach(() => vi.useRealTimers());

const UNREACHABLE = /Control server unreachable — retrying/;
/** The banner's live region: always there, empty while there is nothing to say. */
const banner = () => screen.getByRole("status");

test("banner appears only after 1 s offline", () => {
  const t0 = new Date(2026, 10, 4, 15, 0, 0);
  vi.useFakeTimers({ now: t0 });
  const link = fakeLink("open");
  render(<ConnectionBanner link={link} />);
  expect(banner().textContent).toBe("");

  act(() => link.frame()); // the last update: 15:00
  act(() => vi.advanceTimersByTime(5 * 60_000));
  act(() => link.set("offline"));
  expect(banner().textContent).toBe("");
  act(() => vi.advanceTimersByTime(999));
  expect(banner().textContent).toBe("");
  act(() => vi.advanceTimersByTime(1));
  expect(banner().textContent).toMatch(UNREACHABLE);
  expect(banner().querySelector("time")?.getAttribute("datetime")).toBe(t0.toISOString());

  // Back online: gone at once.
  act(() => link.set("open"));
  expect(banner().textContent).toBe("");

  // A reconnect that takes over 1 s says so; a quick one says nothing.
  act(() => link.set("connecting"));
  act(() => vi.advanceTimersByTime(500));
  act(() => link.set("open"));
  act(() => vi.advanceTimersByTime(1000));
  expect(banner().textContent).toBe("");
  act(() => link.set("connecting"));
  act(() => vi.advanceTimersByTime(1000));
  expect(banner().textContent).toBe("Connecting…");

  // Signed out is the sign-in screen's business.
  act(() => link.set("signedOut"));
  act(() => vi.advanceTimersByTime(2000));
  expect(banner().textContent).toBe("");
});

test("a retrying link keeps one banner from 1 s until it is open", () => {
  vi.useFakeTimers({ now: new Date(2026, 10, 4, 15, 0, 0) });
  const link = fakeLink("open");
  render(<ConnectionBanner link={link} />);
  const region = banner();

  // The client flips offline ↔ connecting on every retry, faster than the delay at first.
  act(() => link.set("offline"));
  for (let i = 0; i < 3; i++) {
    act(() => vi.advanceTimersByTime(300));
    act(() => link.set(i % 2 === 0 ? "connecting" : "offline"));
  }
  // 900 ms down: still quiet, though the status changed three times.
  expect(region.textContent).toBe("");
  act(() => vi.advanceTimersByTime(100));
  expect(region.textContent).toMatch(UNREACHABLE);

  for (let i = 0; i < 10; i++) {
    act(() => link.set(i % 2 === 0 ? "connecting" : "offline"));
    expect(region.textContent).toMatch(UNREACHABLE);
    act(() => vi.advanceTimersByTime(700));
    expect(region.textContent).toMatch(UNREACHABLE);
  }
  // The same element all along: nothing was mounted again to be announced again.
  expect(banner()).toBe(region);

  act(() => link.set("open"));
  expect(region.textContent).toBe("");
});
