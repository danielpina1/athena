import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { useAthena } from "@/lib/store/state";
import { bindSession, enroll, getSession, reauth, signIn, signOut } from "./session";
import { ASSERTION, CREATION, expectedAttestation, fakeCredentials, fakeFetch } from "./testkit";

beforeEach(() => {
  bindSession({ outbox: { clear: vi.fn() } });
});
afterEach(() => {
  vi.unstubAllGlobals();
  useAthena.getState().reset();
});

test("getSession maps the server's answer", async () => {
  const until = "2026-11-04T12:30:00Z";
  const { calls } = fakeFetch({
    "GET /auth/session": { json: { authenticated: true, passkeyId: "p1", reauthValidUntil: until } },
  });
  expect(await getSession()).toEqual({ signedIn: true, reauthAt: new Date(until) });
  expect(calls[0]).toMatchObject({ method: "GET", credentials: "same-origin" });

  fakeFetch({ "GET /auth/session": { json: { authenticated: true, passkeyId: "p1" } } });
  expect(await getSession()).toEqual({ signedIn: true, reauthAt: undefined });
  fakeFetch({ "GET /auth/session": { json: { authenticated: false } } });
  expect((await getSession()).signedIn).toBe(false);
});

test("signIn runs login begin and finish, conditional when asked", async () => {
  const { calls } = fakeFetch({
    "POST /auth/login/begin": { json: ASSERTION },
    "POST /auth/login/finish": { json: { authenticated: true } },
  });
  const creds = fakeCredentials();
  const signal = new AbortController().signal;
  await signIn({ conditional: true, signal });
  expect(calls.map((c) => c.path)).toEqual(["/auth/login/begin", "/auth/login/finish"]);
  expect(calls.every((c) => c.credentials === "same-origin")).toBe(true);
  expect(creds.get.mock.calls[0][0]).toMatchObject({ mediation: "conditional", signal });
  expect(calls[1].body).toMatchObject({ id: expectedAttestation.id, rawId: expectedAttestation.rawId, type: "public-key" });

  await signIn();
  expect(creds.get.mock.calls[1][0]?.mediation).toBeUndefined();
});

test("enroll registers the passkey, then signs in", async () => {
  const { calls } = fakeFetch({
    "POST /auth/register/begin": { json: CREATION },
    "POST /auth/register/finish": { json: { id: "Y3JlZA", name: "Mac" } },
    "POST /auth/login/begin": { json: ASSERTION },
    "POST /auth/login/finish": { json: { authenticated: true } },
  });
  const creds = fakeCredentials();
  await enroll("tok-123", "Mac");
  expect(calls.map((c) => c.path)).toEqual([
    "/auth/register/begin",
    "/auth/register/finish",
    "/auth/login/begin",
    "/auth/login/finish",
  ]);
  expect(calls[0].body).toEqual({ token: "tok-123", name: "Mac" });
  // The token travels once, in that body: no other request carries it, and none has it in the URL.
  expect(JSON.stringify(calls.slice(1))).not.toContain("tok-123");
  expect(calls.every((c) => !c.path.includes("tok-123") && !c.path.includes("?"))).toBe(true);
  expect(creds.create).toHaveBeenCalledOnce();
  expect(creds.get).toHaveBeenCalledOnce();
});

test("enroll reports a failed sign-in after the passkey was saved", async () => {
  fakeFetch({
    "POST /auth/register/begin": { json: CREATION },
    "POST /auth/register/finish": { json: { id: "Y3JlZA", name: "Mac" } },
    "POST /auth/login/begin": { json: ASSERTION },
  });
  fakeCredentials({
    get: () => {
      throw new DOMException("dismissed", "NotAllowedError");
    },
  });
  await expect(enroll("tok", "Mac")).rejects.toMatchObject({ code: "enrolled" });
});

test("reauth runs the step-up ceremony", async () => {
  const { calls } = fakeFetch({
    "POST /auth/reauth/begin": { json: ASSERTION },
    "POST /auth/reauth/finish": { json: { reauthValidUntil: "2026-11-04T12:30:00Z" } },
  });
  fakeCredentials();
  await reauth();
  expect(calls.map((c) => c.path)).toEqual(["/auth/reauth/begin", "/auth/reauth/finish"]);
});

test("a refused request throws with the server's code", async () => {
  fakeFetch({ "POST /auth/login/begin": { status: 401, json: { code: "unauthenticated", error: "no" } } });
  fakeCredentials();
  await expect(signIn()).rejects.toMatchObject({ code: "unauthenticated", status: 401 });
});

test("sign out clears outbox and store", async () => {
  const outbox = { clear: vi.fn() };
  bindSession({ outbox });
  useAthena.setState({ epoch: "e1", cursor: 9n });
  const { calls } = fakeFetch({ "POST /auth/logout": { json: { authenticated: false } } });
  await signOut();
  expect(calls[0]).toMatchObject({ method: "POST", path: "/auth/logout", credentials: "same-origin" });
  expect(outbox.clear).toHaveBeenCalledOnce();
  expect(useAthena.getState().epoch).toBe("");
  expect(useAthena.getState().cursor).toBe(0n);

  // A logout the server did not take leaves the device as it was: the owner is still signed in.
  useAthena.setState({ epoch: "e2", cursor: 3n });
  fakeFetch({ "POST /auth/logout": { status: 500, json: { code: "internal", error: "x" } } });
  await expect(signOut()).rejects.toBeTruthy();
  expect(outbox.clear).toHaveBeenCalledOnce();
  expect(useAthena.getState().epoch).toBe("e2");
});
