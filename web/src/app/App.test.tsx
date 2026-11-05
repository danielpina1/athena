import { render, screen } from "@testing-library/react";
import { IDBFactory } from "fake-indexeddb";
import { afterEach, expect, test, vi } from "vitest";
import { App } from "@/app/App";
import { createRuntime, type Runtime } from "@/app/boot";
import { fakeFetch } from "@/lib/auth/testkit";
import { FakeServer } from "@/lib/link/fakeServer";

const started: Runtime[] = [];
afterEach(() => {
  for (const r of started.splice(0)) r.dispose();
  vi.unstubAllGlobals();
  history.replaceState(null, "", "/");
});

const runtime = () => {
  const r = createRuntime({
    url: "ws://x/ws",
    clientId: "abcdefghijklmnop",
    WebSocketImpl: new FakeServer().WebSocket,
    indexedDB: new IDBFactory(),
  });
  started.push(r);
  return r;
};

test("a signed-in owner sees the app", async () => {
  fakeFetch({ "GET /auth/session": { json: { authenticated: true } } });
  render(<App runtime={runtime()} />);
  expect(await screen.findByRole("heading", { name: "Needs you" })).toBeTruthy();
  expect(screen.getByRole("navigation", { name: "Main" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Sign in with passkey" })).toBeNull();
});

test("no session shows sign-in", async () => {
  fakeFetch({ "GET /auth/session": { json: { authenticated: false } } });
  render(<App runtime={runtime()} />);
  expect(await screen.findByRole("button", { name: "Sign in with passkey" })).toBeTruthy();
});

test("/design and /enroll need no session", async () => {
  const { fn } = fakeFetch({});
  history.replaceState(null, "", "/enroll");
  render(<App runtime={runtime()} />);
  expect(await screen.findByRole("alert")).toBeTruthy(); // no token in the link
  expect(fn).not.toHaveBeenCalled();

  history.replaceState(null, "", "/design");
  render(<App runtime={runtime()} />);
  // Rendered, not blank: the kitchen sink's own heading, with no session asked about.
  expect(await screen.findByRole("heading", { name: "Precision" })).toBeTruthy();
  expect(fn).not.toHaveBeenCalled();
});
