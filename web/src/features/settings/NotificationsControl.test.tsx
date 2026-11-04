import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { PushState } from "@/lib/push";
import { Toaster } from "@/ui/primitives/toast";

let state: PushState;
const enablePush = vi.fn<() => Promise<PushState>>();
const disablePush = vi.fn<() => Promise<PushState>>();
vi.mock("@/lib/push", () => ({
  pushState: () => state,
  onPushState: () => () => {},
  enablePush: () => enablePush(),
  disablePush: () => disablePush(),
  PushError: class extends Error {},
}));

const { NotificationsControl } = await import("./NotificationsControl");

beforeEach(() => {
  state = "off";
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
