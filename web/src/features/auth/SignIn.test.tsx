import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { bindSession } from "@/lib/auth/session";
import { ASSERTION, fakeCredentials, fakeFetch } from "@/lib/auth/testkit";
import { SignIn } from "./SignIn";

beforeEach(() => bindSession({ outbox: { clear: vi.fn() } }));
afterEach(() => {
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
