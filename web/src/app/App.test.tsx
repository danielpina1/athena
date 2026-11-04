import { act, render, screen } from "@testing-library/react";
import { IDBFactory } from "fake-indexeddb";
import { afterEach, expect, test, vi } from "vitest";
import { App } from "@/app/App";
import { createRuntime } from "@/app/boot";
import { fakeFetch } from "@/lib/auth/testkit";
import { FakeServer } from "@/lib/link/fakeServer";

afterEach(() => {
  vi.unstubAllGlobals();
  history.replaceState(null, "", "/");
});

const runtime = () =>
  createRuntime({ url: "ws://x/ws", clientId: "abcdefghijklmnop", WebSocketImpl: new FakeServer().WebSocket, indexedDB: new IDBFactory() });

test("a signed-in owner sees the app", async () => {
  fakeFetch({ "GET /auth/session": { json: { authenticated: true } } });
  render(<App runtime={runtime()} />);
  expect(await screen.findByRole("heading", { name: "Athena" })).toBeTruthy();
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
  await act(async () => {
    render(<App runtime={runtime()} />);
  });
  expect(fn).not.toHaveBeenCalled();
});
