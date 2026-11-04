import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { PushState } from "@/lib/push";
import { Toaster } from "@/ui/primitives/toast";

let state: PushState;
const enablePush = vi.fn<() => Promise<PushState>>();
const disablePush = vi.fn<() => Promise<PushState>>();
let pending = false;
const errorListeners = new Set<(m: string) => void>();
vi.mock("@/lib/push", () => ({
  pushState: () => state,
  pushPending: () => pending,
  onPushState: () => () => {},
  onPushError: (cb: (m: string) => void) => {
    errorListeners.add(cb);
    return () => errorListeners.delete(cb);
  },
  enablePush: () => enablePush(),
  disablePush: () => disablePush(),
  PushError: class extends Error {},
}));

const { NotificationsControl } = await import("./NotificationsControl");

beforeEach(() => {
  state = "off";
  pending = false;
  errorListeners.clear();
  enablePush.mockReset();
  disablePush.mockReset();
});
afterEach(() => vi.useRealTimers());

const group = () => screen.getByRole("radiogroup", { name: "Notifications" });

test("On subscribes and Off unsubscribes", async () => {
  const user = userEvent.setup();
  render(<NotificationsControl />);
  expect(screen.getByRole("radio", { name: "Off" }).getAttribute("aria-checked")).toBe("true");

  enablePush.mockImplementation(async () => (state = "on"));
  await user.click(screen.getByRole("radio", { name: "On" }));
  expect(enablePush).toHaveBeenCalledTimes(1);
  expect(screen.getByRole("radio", { name: "On" }).getAttribute("aria-checked")).toBe("true");

  disablePush.mockImplementation(async () => (state = "off"));
  await user.click(screen.getByRole("radio", { name: "Off" }));
  expect(disablePush).toHaveBeenCalledTimes(1);
  expect(screen.getByRole("radio", { name: "Off" }).getAttribute("aria-checked")).toBe("true");
  expect(group()).toBeTruthy();
});

test("a failure says why in a toast and stays off", async () => {
  const user = userEvent.setup();
  render(
    <>
      <NotificationsControl />
      <Toaster />
    </>,
  );
  enablePush.mockRejectedValue(new Error("Notifications are not available on this server: it needs an https address."));
  await user.click(screen.getByRole("radio", { name: "On" }));
  expect(await screen.findByText("Notifications are not available on this server: it needs an https address.")).toBeTruthy();
  expect(screen.getByRole("radio", { name: "Off" }).getAttribute("aria-checked")).toBe("true");
});

test("blocked and unsupported cannot be switched and say why", () => {
  state = "denied";
  const { rerender } = render(<NotificationsControl />);
  expect(screen.getByText(/blocked in this browser's site settings/i)).toBeTruthy();
  expect((screen.getByRole("radio", { name: "On" }) as HTMLButtonElement).disabled).toBe(true);

  state = "unsupported";
  rerender(<NotificationsControl key="2" />);
  expect(screen.getByText(/Home Screen/)).toBeTruthy();
  expect((screen.getByRole("radio", { name: "On" }) as HTMLButtonElement).disabled).toBe(true);
});

test("a change waiting for the server says so, and a later refusal shows a toast", async () => {
  const user = userEvent.setup();
  render(
    <>
      <NotificationsControl />
      <Toaster />
    </>,
  );
  // Offline: queued, not answered. The control is free again at once.
  enablePush.mockImplementation(async () => {
    pending = true;
    return (state = "on");
  });
  await user.click(screen.getByRole("radio", { name: "On" }));
  expect(screen.getByText(/Athena is told when it is reachable/)).toBeTruthy();
  expect((screen.getByRole("radio", { name: "Off" }) as HTMLButtonElement).disabled).toBe(false);

  act(() => {
    pending = false;
    state = "off";
    for (const l of errorListeners) l("The server is busy. Try again in a moment.");
  });
  expect(await screen.findByText("The server is busy. Try again in a moment.")).toBeTruthy();
});
