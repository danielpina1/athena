import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import type { PushState } from "@/lib/push";
import { useAthena } from "@/lib/store/state";
import { Toaster } from "@/ui/primitives/toast";
import { load } from "../tickets/fixtures";

let state: PushState;
const enablePush = vi.fn<() => Promise<PushState>>();
const disablePush = vi.fn<() => Promise<PushState>>();
let pending = false;
const errorListeners = new Set<(m: string) => void>();
const wrapper = ({ children }: { children: ReactNode }) => <MemoryRouter>{children}</MemoryRouter>;

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

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  setMuted: vi.fn(),
}));
const setMuted = vi.mocked(actions.setMuted);

const { Notifications } = await import("./Notifications");

const KINDS = ["approval", "answer", "review", "delivery", "blocked", "failed"];

beforeEach(() => {
  load({ notifications: { kinds: KINDS, muted: ["answer"] } });
  setMuted.mockReset();
  state = "off";
  pending = false;
  errorListeners.clear();
  enablePush.mockReset();
  disablePush.mockReset();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  useAthena.getState().reset();
});

const group = () => screen.getByRole("radiogroup", { name: "Notifications" });

test("On subscribes and Off unsubscribes", async () => {
  const user = userEvent.setup();
  render(<Notifications />, { wrapper });
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
      <Notifications />
      <Toaster />
    </>,
    { wrapper },
  );
  enablePush.mockRejectedValue(new Error("Notifications are not available on this server: it needs an https address."));
  await user.click(screen.getByRole("radio", { name: "On" }));
  expect(await screen.findByText("Notifications are not available on this server: it needs an https address.")).toBeTruthy();
  expect(screen.getByRole("radio", { name: "Off" }).getAttribute("aria-checked")).toBe("true");
});

test("blocked and unsupported cannot be switched and say why", () => {
  state = "denied";
  const { rerender } = render(<Notifications />, { wrapper });
  expect(screen.getByText(/blocked in this browser's site settings/i)).toBeTruthy();
  expect((screen.getByRole("radio", { name: "On" }) as HTMLButtonElement).disabled).toBe(true);

  state = "unsupported";
  rerender(<Notifications key="2" />);
  expect(screen.getByText(/Home Screen/)).toBeTruthy();
  expect((screen.getByRole("radio", { name: "On" }) as HTMLButtonElement).disabled).toBe(true);
});

test("a change waiting for the server says so, and a later refusal shows a toast", async () => {
  const user = userEvent.setup();
  render(
    <>
      <Notifications />
      <Toaster />
    </>,
    { wrapper },
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

test("muting a kind calls setMuted with the full list", async () => {
  const user = userEvent.setup();
  render(<Notifications />, { wrapper });
  const approval = screen.getByRole("switch", { name: "Mute Needs approval" });
  expect(approval.getAttribute("aria-checked")).toBe("false");
  expect(screen.getByRole("switch", { name: "Mute Needs your answer" }).getAttribute("aria-checked")).toBe("true");

  setMuted.mockResolvedValue({ ok: true, code: "" } as CommandResult);
  await user.click(approval);
  // The whole muted list, in the server's order of kinds: the command replaces it.
  expect(setMuted).toHaveBeenCalledWith(["approval", "answer"]);
  expect(approval.getAttribute("aria-checked")).toBe("true");

  await user.click(screen.getByRole("switch", { name: "Mute Needs your answer" }));
  expect(setMuted).toHaveBeenLastCalledWith(["approval"]);
});

test("a refused mute goes back and says why", async () => {
  const user = userEvent.setup();
  render(
    <>
      <Notifications />
      <Toaster />
    </>,
    { wrapper },
  );
  setMuted.mockResolvedValue({ ok: false, code: "internal" } as CommandResult);
  await user.click(screen.getByRole("switch", { name: "Mute Failed" }));
  expect(await screen.findByText("Something went wrong on the server. Try again.")).toBeTruthy();
  expect(screen.getByRole("switch", { name: "Mute Failed" }).getAttribute("aria-checked")).toBe("false");
});

test("the iOS note shows on iPhone outside the Home Screen app only", () => {
  vi.stubGlobal("navigator", { ...navigator, userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X)" });
  const { unmount } = render(<Notifications />, { wrapper });
  expect(screen.getByText("Add Athena to your Home Screen to get notifications on iPhone.")).toBeTruthy();
  unmount();
  vi.stubGlobal("navigator", { ...navigator, userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X)", standalone: true });
  render(<Notifications />, { wrapper });
  expect(screen.queryByText("Add Athena to your Home Screen to get notifications on iPhone.")).toBeNull();
});

test("kinds wait for the server before the first Snapshot", () => {
  useAthena.getState().reset();
  render(<Notifications />, { wrapper });
  expect(screen.getByText("Waiting for the server.")).toBeTruthy();
});
