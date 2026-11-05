import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { bindSession } from "@/lib/auth/session";
import { ASSERTION, fakeCredential, fakeCredentials, fakeFetch } from "@/lib/auth/testkit";
import { REARM_BEFORE_EXPIRY_MS, SignIn } from "./SignIn";

beforeEach(() => bindSession({ outbox: { clear: vi.fn() } }));
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const routes = {
  "POST /auth/login/begin": { json: ASSERTION },
  "POST /auth/login/finish": { json: { authenticated: true } },
};

test("the button signs in with a passkey", async () => {
  vi.stubGlobal("PublicKeyCredential", {});
  fakeFetch(routes);
  const creds = fakeCredentials();
  const done = vi.fn();
  const user = userEvent.setup();
  render(<SignIn onSignedIn={done} notice="You were signed out on this device." />);
  expect(screen.getByText("You were signed out on this device.")).toBeTruthy();
  await user.click(screen.getByRole("button", { name: "Sign in with passkey" }));
  await waitFor(() => expect(done).toHaveBeenCalledOnce());
  expect(creds.get.mock.calls[0][0]?.mediation).toBeUndefined();
});

test("a browser with passkey autofill signs in from the field", async () => {
  vi.stubGlobal("PublicKeyCredential", { isConditionalMediationAvailable: async () => true });
  fakeFetch(routes);
  const creds = fakeCredentials();
  const done = vi.fn();
  render(<SignIn onSignedIn={done} />);
  await waitFor(() => expect(done).toHaveBeenCalledOnce());
  expect(creds.get.mock.calls[0][0]).toMatchObject({ mediation: "conditional" });
  expect(screen.getByLabelText("Passkey").getAttribute("autocomplete")).toBe("username webauthn");
});

test("a dismissed prompt leaves the button, a refusal says so", async () => {
  vi.stubGlobal("PublicKeyCredential", {});
  fakeFetch(routes);
  let n = 0;
  fakeCredentials({
    get: () => {
      if (n++ === 0) throw new DOMException("dismissed", "NotAllowedError");
      throw new Error("boom");
    },
  });
  const user = userEvent.setup();
  const done = vi.fn();
  render(<SignIn onSignedIn={done} />);
  await user.click(screen.getByRole("button", { name: "Sign in with passkey" }));
  await waitFor(() => expect((screen.getByRole("button", { name: "Sign in with passkey" }) as HTMLButtonElement).disabled).toBe(false));
  expect(screen.queryByRole("alert")).toBeNull();
  await user.click(screen.getByRole("button", { name: "Sign in with passkey" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Could not sign in");
  expect(done).not.toHaveBeenCalled();
});

/** A get() that stays open until the page aborts it, as a conditional ceremony does. */
const pendingGet = (o?: CredentialRequestOptions) =>
  new Promise<never>((_, reject) => {
    const abort = () => reject(new DOMException("aborted", "AbortError"));
    if (o?.signal?.aborted) abort(); // like the browser: an already aborted signal rejects at once
    o?.signal?.addEventListener("abort", abort);
  });

test("a server refusal on the autofill path shows the failure and tries again", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("PublicKeyCredential", { isConditionalMediationAvailable: async () => true });
  let finishes = 0;
  const { calls } = fakeFetch({
    "POST /auth/login/begin": { json: ASSERTION },
    "POST /auth/login/finish": () => (++finishes === 1 ? { status: 401, json: { code: "unauthenticated", error: "no" } } : { json: {} }),
  });
  fakeCredentials();
  const done = vi.fn();
  render(<SignIn onSignedIn={done} />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(screen.getByRole("alert").textContent).toContain("Could not sign in");
  expect(done).not.toHaveBeenCalled();
  // Not at once (no hot loop), but soon: a fresh begin, which this time works.
  await act(() => vi.advanceTimersByTimeAsync(5_000));
  expect(done).toHaveBeenCalledOnce();
  expect(calls.filter((c) => c.path === "/auth/login/begin")).toHaveLength(2);
});

test("the autofill ceremony is renewed before the server forgets it", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("PublicKeyCredential", { isConditionalMediationAvailable: async () => true });
  const { calls } = fakeFetch({ "POST /auth/login/begin": { json: ASSERTION } });
  const creds = fakeCredentials({ get: pendingGet });
  const { unmount } = render(<SignIn onSignedIn={() => {}} />);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(creds.get).toHaveBeenCalledTimes(1);

  await act(() => vi.advanceTimersByTimeAsync(REARM_BEFORE_EXPIRY_MS - 1));
  expect(creds.get).toHaveBeenCalledTimes(1);
  await act(() => vi.advanceTimersByTimeAsync(1));
  expect(creds.get).toHaveBeenCalledTimes(2);
  expect(calls.filter((c) => c.path === "/auth/login/begin")).toHaveLength(2);
  // The renewal is quiet: the aborted run is not a failure.
  expect(screen.queryByRole("alert")).toBeNull();

  unmount();
  await act(() => vi.advanceTimersByTimeAsync(REARM_BEFORE_EXPIRY_MS * 2));
  expect(creds.get).toHaveBeenCalledTimes(2);
});

test("the button waits for the autofill login/begin before starting its own", async () => {
  vi.stubGlobal("PublicKeyCredential", { isConditionalMediationAvailable: async () => true });
  const order: string[] = [];
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  let begins = 0;
  fakeFetch({
    "POST /auth/login/begin": async () => {
      const n = ++begins;
      order.push(`begin${n}:start`);
      if (n === 1) await gate;
      order.push(`begin${n}:end`);
      return { json: ASSERTION };
    },
    "POST /auth/login/finish": { json: {} },
  });
  fakeCredentials({ get: (o) => (o?.mediation === "conditional" ? pendingGet(o) : fakeCredential("get")) });
  const done = vi.fn();
  const user = userEvent.setup();
  render(<SignIn onSignedIn={done} />);
  await waitFor(() => expect(order).toEqual(["begin1:start"]));

  await user.click(screen.getByRole("button", { name: "Sign in with passkey" }));
  // The button's begin has not been asked for while the first is still in flight.
  expect(order).toEqual(["begin1:start"]);
  release();
  await waitFor(() => expect(done).toHaveBeenCalledOnce());
  expect(order).toEqual(["begin1:start", "begin1:end", "begin2:start", "begin2:end"]);
});
