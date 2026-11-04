import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StrictMode } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { bindSession } from "@/lib/auth/session";
import { ASSERTION, CREATION, fakeCredentials, fakeFetch } from "@/lib/auth/testkit";
import { Enroll, deviceNameFromUserAgent, forgetEnrollToken } from "./Enroll";

beforeEach(() => {
  bindSession({ outbox: { clear: vi.fn() } });
  forgetEnrollToken();
});
afterEach(() => {
  vi.unstubAllGlobals();
  history.replaceState(null, "", "/");
});

const SPENT = "This link was already used or has expired. Run athena web enroll for a new one.";

test("enroll drops the fragment before any request", async () => {
  history.replaceState(null, "", "/enroll#tok-abc");
  const replace = vi.spyOn(history, "replaceState");
  let hashAtFirstRequest: string | undefined;
  const { calls } = fakeFetch({
    "POST /auth/register/begin": () => {
      hashAtFirstRequest = location.hash;
      return { json: CREATION };
    },
    "POST /auth/register/finish": { json: { id: "Y3JlZA", name: "Mac" } },
    "POST /auth/login/begin": { json: ASSERTION },
    "POST /auth/login/finish": { json: { authenticated: true } },
  });
  fakeCredentials();
  const done = vi.fn();
  const user = userEvent.setup();
  // StrictMode renders twice: the second look at the address must not lose the token.
  render(
    <StrictMode>
      <Enroll onDone={done} />
    </StrictMode>,
  );

  // Gone from the address before anything is sent, and before the owner does anything.
  expect(location.hash).toBe("");
  expect(replace).toHaveBeenCalled();
  expect(calls).toHaveLength(0);

  const name = screen.getByLabelText("Device name") as HTMLInputElement;
  await user.clear(name);
  await user.type(name, "Daniel's phone");
  await user.click(screen.getByRole("button", { name: "Create passkey" }));
  await waitFor(() => expect(done).toHaveBeenCalledOnce());

  expect(hashAtFirstRequest).toBe("");
  expect(calls[0].body).toEqual({ token: "tok-abc", name: "Daniel's phone" });
  expect(calls.every((c) => !c.path.includes("tok-abc"))).toBe(true);
});

test("spent token shows the exact copy", async () => {
  history.replaceState(null, "", "/enroll#tok-old");
  const { calls } = fakeFetch({
    "POST /auth/register/begin": { status: 401, json: { code: "unauthenticated", error: "invalid or expired enrollment token" } },
  });
  const creds = fakeCredentials();
  const user = userEvent.setup();
  render(<Enroll onDone={() => {}} />);
  await user.click(screen.getByRole("button", { name: "Create passkey" }));

  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toContain(SPENT);
  expect(alert.querySelector("code")?.textContent).toBe("athena web enroll");
  // No retry with the same token: nothing to press, nothing more sent.
  expect(screen.queryByRole("button", { name: /create passkey|try again|retry/i })).toBeNull();
  expect(calls).toHaveLength(1);
  expect(creds.create).not.toHaveBeenCalled();
});

test("a link with no token says so and sends nothing", async () => {
  const { calls } = fakeFetch({});
  render(<Enroll onDone={() => {}} />);
  expect((await screen.findByRole("alert")).textContent).toContain("Run athena web enroll for a new one.");
  expect(calls).toHaveLength(0);
});

test("a cancelled prompt cannot be retried either, because the token is spent", async () => {
  history.replaceState(null, "", "/enroll#tok-x");
  fakeFetch({ "POST /auth/register/begin": { json: CREATION } });
  fakeCredentials({
    create: () => {
      throw new DOMException("dismissed", "NotAllowedError");
    },
  });
  const user = userEvent.setup();
  render(<Enroll onDone={() => {}} />);
  await user.click(screen.getByRole("button", { name: "Create passkey" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Run athena web enroll for a new link.");
  expect(screen.queryByRole("button", { name: "Create passkey" })).toBeNull();
});

test("the device name defaults from the user agent", () => {
  expect(deviceNameFromUserAgent("Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Safari")).toBe("iPhone");
  expect(deviceNameFromUserAgent("Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X)")).toBe("iPad");
  expect(deviceNameFromUserAgent("Mozilla/5.0 (Linux; Android 14; Pixel 8)")).toBe("Android phone");
  expect(deviceNameFromUserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")).toBe("Mac");
  expect(deviceNameFromUserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64)")).toBe("Windows PC");
  expect(deviceNameFromUserAgent("Mozilla/5.0 (X11; Linux x86_64)")).toBe("Linux PC");
  expect(deviceNameFromUserAgent("curl/8")).toBe("This device");
});
