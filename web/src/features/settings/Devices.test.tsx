import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import * as reauthDialog from "@/features/auth/ReauthDialog";
import * as session from "@/lib/auth/session";
import * as actions from "@/lib/commands/actions";
import { Toaster } from "@/ui/primitives/toast";
import { Devices } from "./Devices";

const wrapper = ({ children }: { children: ReactNode }) => <MemoryRouter>{children}</MemoryRouter>;

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  listPasskeys: vi.fn(),
  revokePasskey: vi.fn(),
}));
vi.mock("@/lib/auth/session", async (orig) => ({
  ...(await orig<typeof import("@/lib/auth/session")>()),
  getSession: vi.fn(),
  createEnrollLink: vi.fn(),
  sessionEnded: vi.fn(),
  requestSignOut: vi.fn(),
}));
vi.mock("@/features/auth/ReauthDialog", async (orig) => ({
  ...(await orig<typeof import("@/features/auth/ReauthDialog")>()),
  requestReauth: vi.fn(),
}));
const listPasskeys = vi.mocked(actions.listPasskeys);
const revokePasskey = vi.mocked(actions.revokePasskey);
const getSession = vi.mocked(session.getSession);
const createEnrollLink = vi.mocked(session.createEnrollLink);
const requestReauth = vi.mocked(reauthDialog.requestReauth);
const sessionEnded = vi.mocked(session.sessionEnded);

const NOW = Date.parse("2026-11-04T12:00:00Z");
const passkeys = [
  { id: "pk-mac", name: "Mac", createdAt: timestampFromMs(NOW - 86_400_000 * 3), lastUsedAt: timestampFromMs(NOW - 60_000) },
  { id: "pk-phone", name: "iPhone", createdAt: timestampFromMs(NOW - 86_400_000) },
];
const listed = { ok: true, code: "", result: { case: "passkeys", value: { passkeys } } } as unknown as CommandResult;
const ok = { ok: true, code: "" } as CommandResult;

beforeEach(() => {
  listPasskeys.mockResolvedValue(listed);
  revokePasskey.mockResolvedValue(ok);
  getSession.mockResolvedValue({ signedIn: true, passkeyId: "pk-mac" });
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.clearAllMocks();
});

const row = (name: string) => screen.getByRole("listitem", { name });

test("devices list name, created and last used, and mark this device", async () => {
  render(<Devices />, { wrapper });
  expect(screen.getByText("Waiting for the server.")).toBeTruthy();
  const mac = await screen.findByRole("listitem", { name: "Mac" });
  expect(within(mac).getByText("This device")).toBeTruthy();
  expect(within(mac).getByText(/Last used/)).toBeTruthy();
  expect(within(row("iPhone")).queryByText("This device")).toBeNull();
  expect(within(row("iPhone")).getByText("Never used")).toBeTruthy();
});

test("revoke current device warns", async () => {
  const user = userEvent.setup();
  render(<Devices />, { wrapper });
  const mac = await screen.findByRole("listitem", { name: "Mac" });
  expect(within(mac).getByText("Revoking it signs this device out.")).toBeTruthy();
  await user.click(within(mac).getByRole("button", { name: "Revoke" }));
  expect(revokePasskey).not.toHaveBeenCalled();
  await new Promise((r) => setTimeout(r, 350));
  await user.click(within(mac).getByRole("button", { name: "Revoke and sign out this device" }));
  // This device's revoke ends the session: it never replays after the next sign-in, and on success the app leaves
  // at once instead of waiting for the server's close.
  expect(revokePasskey).toHaveBeenCalledWith("pk-mac", { thisDevice: true });
  await vi.waitFor(() => expect(sessionEnded).toHaveBeenCalledTimes(1));

  // Another device's passkey says only that it is revoked.
  const phone = row("iPhone");
  expect(within(phone).queryByText("Revoking it signs this device out.")).toBeNull();
  await user.click(within(phone).getByRole("button", { name: "Revoke" }));
  await new Promise((r) => setTimeout(r, 350));
  await user.click(within(phone).getByRole("button", { name: "Revoke this passkey" }));
  expect(revokePasskey).toHaveBeenLastCalledWith("pk-phone", { thisDevice: false });
  expect(sessionEnded).toHaveBeenCalledTimes(1);
});

test("the QR code encodes exactly the link", async () => {
  const url = "https://athena.test/enroll#exact-token-123";
  createEnrollLink.mockResolvedValue({ url, expiresAt: new Date(Date.now() + 60_000) });
  const user = userEvent.setup();
  render(<Devices />, { wrapper });
  await user.click(screen.getByRole("button", { name: "Add a device" }));
  const qr = await screen.findByRole("img", { name: "QR code of the enrollment link" });
  const { encode } = await import("uqr");
  const want = encode(url, { ecc: "M", border: 4 });
  expect(qr.getAttribute("viewBox")).toBe(`0 0 ${want.size} ${want.size}`);
  // Each dark module of the code for exactly this link, and no other.
  const modules = new Set<string>();
  want.data.forEach((row, y) => row.forEach((on, x) => on && modules.add(`${x},${y}`)));
  const drawn = new Set([...(qr.querySelector("path")?.getAttribute("d") ?? "").matchAll(/M(\d+),(\d+)/g)].map((m) => `${m[1]},${m[2]}`));
  expect(drawn).toEqual(modules);
});

test("add device shows QR and never puts the token in location or console", async () => {
  const token = "Sx9-SECRET-enroll-token_42";
  const url = `https://athena.test/enroll#${token}`;
  const spies = (["log", "info", "warn", "error", "debug"] as const).map((m) => vi.spyOn(console, m));
  const user = userEvent.setup();
  // The first try needs a step-up; after the passkey it goes through.
  createEnrollLink
    .mockRejectedValueOnce(new session.AuthError("reauth_required", 403, "confirm with your passkey first"))
    .mockResolvedValueOnce({ url, expiresAt: new Date(Date.now() + 10 * 60_000) });
  requestReauth.mockResolvedValue(true);
  const before = location.href;
  const historyLength = history.length;
  render(
    <>
      <Devices />
      <Toaster />
    </>,
    { wrapper },
  );
  await screen.findByRole("listitem", { name: "Mac" });

  await user.click(screen.getByRole("button", { name: "Add a device" }));
  expect(requestReauth).toHaveBeenCalledTimes(1);
  expect(createEnrollLink).toHaveBeenCalledTimes(2);
  const qr = await screen.findByRole("img", { name: "QR code of the enrollment link" });
  expect(qr.tagName.toLowerCase()).toBe("svg");
  expect(qr.querySelector("path")?.getAttribute("d")).toMatch(/^M/);
  expect(screen.getByText(url)).toBeTruthy();
  expect(screen.getByText(/^Expires in (9:5\d|10:00)$/)).toBeTruthy();

  const writeText = vi.fn(async () => {});
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  await user.click(screen.getByRole("button", { name: "Copy" }));
  expect(writeText).toHaveBeenCalledWith(url);
  expect(await screen.findByText("Link copied")).toBeTruthy();

  expect(location.href).toBe(before);
  expect(location.href).not.toContain(token);
  expect(history.length).toBe(historyLength);
  expect(JSON.stringify(history.state ?? null)).not.toContain(token);
  for (const spy of spies) {
    for (const args of spy.mock.calls) expect(args.map(String).join(" ")).not.toContain(token);
  }
});

test("the countdown ends in Expired — create a new link", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  createEnrollLink.mockResolvedValue({ url: "https://athena.test/enroll#t", expiresAt: new Date(Date.now() + 3_000) });
  render(<Devices />, { wrapper });
  await act(async () => {});
  fireEvent.click(screen.getByRole("button", { name: "Add a device" }));
  await act(async () => {});
  expect(screen.getByText(/^Expires in 0:0[23]$/)).toBeTruthy();
  await act(async () => {
    vi.advanceTimersByTime(4_000);
  });
  expect(screen.getByText("Expired — create a new link")).toBeTruthy();
  expect(screen.queryByRole("img", { name: "QR code of the enrollment link" })).toBeNull();
  expect(screen.getByRole("button", { name: "Create a new link" })).toBeTruthy();
});

test("a refused step-up leaves no link and says why", async () => {
  const user = userEvent.setup();
  createEnrollLink.mockRejectedValue(new session.AuthError("reauth_required", 403));
  requestReauth.mockResolvedValue(false);
  render(
    <>
      <Devices />
      <Toaster />
    </>,
    { wrapper },
  );
  await user.click(screen.getByRole("button", { name: "Add a device" }));
  expect(await screen.findByText("Confirm with your passkey to make this change.")).toBeTruthy();
  expect(screen.queryByRole("img", { name: "QR code of the enrollment link" })).toBeNull();
});

test("a revoke of this device that did not happen says so; one not confirmed says to check", async () => {
  const user = userEvent.setup();
  render(
    <>
      <Devices />
      <Toaster />
    </>,
    { wrapper },
  );
  const mac = await screen.findByRole("listitem", { name: "Mac" });
  const revoke = async () => {
    await user.click(within(mac).getByRole("button", { name: "Revoke" }));
    await new Promise((r) => setTimeout(r, 350));
    await user.click(within(mac).getByRole("button", { name: "Revoke and sign out this device" }));
  };
  // Signed out first, and the server never ran it: the sign-in screen must not read as success.
  revokePasskey.mockResolvedValueOnce({ ok: false, code: "not_done" } as CommandResult);
  await revoke();
  expect(await screen.findByText("You were signed out first. Sign in and revoke it again.")).toBeTruthy();
  expect(sessionEnded).not.toHaveBeenCalled();
  // Sent, and the session ended before the answer: it may have happened.
  revokePasskey.mockResolvedValueOnce({ ok: false, code: "signed_out" } as CommandResult);
  await revoke();
  expect(await screen.findByText("You were signed out before Athena answered. Sign in and check the device list.")).toBeTruthy();
});

test("sign out is confirmed, then ends the session; a refusal keeps the owner in and says so", async () => {
  const user = userEvent.setup();
  const requestSignOut = vi.mocked(session.requestSignOut);
  requestSignOut.mockRejectedValueOnce(new session.AuthError("internal", 500));
  render(
    <>
      <Devices />
      <Toaster />
    </>,
    { wrapper },
  );
  await screen.findByRole("listitem", { name: "Mac" });
  await user.click(screen.getByRole("button", { name: "Sign out" }));
  expect(requestSignOut).not.toHaveBeenCalled();
  await new Promise((r) => setTimeout(r, 350));
  await user.click(screen.getByRole("button", { name: "Sign out of this device" }));
  expect(await screen.findByText("Not signed out")).toBeTruthy();
  expect(requestSignOut).toHaveBeenCalledTimes(1);

  await user.click(screen.getByRole("button", { name: "Sign out" }));
  await new Promise((r) => setTimeout(r, 350));
  await user.click(screen.getByRole("button", { name: "Sign out of this device" }));
  expect(requestSignOut).toHaveBeenCalledTimes(2);
});
