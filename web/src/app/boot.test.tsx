import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { IDBFactory } from "fake-indexeddb";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { createRuntime, SIGNED_OUT_NOTICE } from "@/app/boot";
import { SessionGate } from "@/app/SessionGate";
import { ASSERTION, fakeCredentials, fakeFetch } from "@/lib/auth/testkit";
import { FakeServer } from "@/lib/link/fakeServer";
import { useAthena } from "@/lib/store/state";

beforeEach(() => useAthena.getState().reset());
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function setup() {
  const server = new FakeServer();
  const runtime = createRuntime({
    url: "ws://athena.test/ws",
    clientId: "abcdefghijklmnop",
    WebSocketImpl: server.WebSocket,
    indexedDB: new IDBFactory(),
  });
  return { server, runtime };
}

const signInRoutes = {
  "POST /auth/login/begin": { json: ASSERTION },
  "POST /auth/login/finish": { json: { authenticated: true } },
};

const gate = (runtime: ReturnType<typeof setup>["runtime"]) =>
  render(
    <SessionGate runtime={runtime}>
      <p>the app</p>
    </SessionGate>,
  );

test("close 4002 signs out and keeps the outbox", async () => {
  const { server, runtime } = setup();
  fakeFetch({ "GET /auth/session": { json: { authenticated: true } }, ...signInRoutes });
  fakeCredentials();
  const user = userEvent.setup();
  gate(runtime);
  await screen.findByText("the app");
  server.welcome();
  await waitFor(() => expect(runtime.client.status).toBe("open"));

  // A command in flight when the session ends.
  const result = runtime.outbox.submit({ case: "reply", value: { ticketId: "T-1", text: "hi" } });
  await runtime.outbox.settled();
  const sent = server.socket.sent("command");
  expect(sent).toHaveLength(1);
  const id = sent[0].id;

  act(() => server.socket.serverClose(4002));
  expect(await screen.findByText(SIGNED_OUT_NOTICE)).toBeTruthy();
  expect(screen.queryByText("the app")).toBeNull();
  // Nothing reconnects behind the sign-in page, and the command is still waiting, unresolved.
  expect(server.sockets).toHaveLength(1);
  expect(runtime.outbox.pending().map((c) => c.id)).toEqual([id]);

  await user.click(screen.getByRole("button", { name: "Sign in with passkey" }));
  await screen.findByText("the app");
  expect(server.sockets).toHaveLength(2);
  server.welcome();
  await runtime.outbox.settled();
  // The same command, by the same id, now that the owner is back.
  expect(server.socket.sent("command").map((c) => c.id)).toEqual([id]);
  void result;
});

test("signing out on 4002 leaves the next connect resuming from an empty store", async () => {
  const { server, runtime } = setup();
  fakeFetch({ "GET /auth/session": { json: { authenticated: true } }, ...signInRoutes });
  fakeCredentials();
  const user = userEvent.setup();
  gate(runtime);
  await screen.findByText("the app");
  server.welcome("e1");
  useAthena.setState({ epoch: "e1", cursor: 5n });

  act(() => server.socket.serverClose(4002));
  await screen.findByText(SIGNED_OUT_NOTICE);
  expect(useAthena.getState().epoch).toBe("");
  await user.click(screen.getByRole("button", { name: "Sign in with passkey" }));
  await screen.findByText("the app");
  server.welcome("e2");
  const hello = server.socket.sent("hello")[0];
  expect(hello.epoch).toBe("");
  expect(hello.cursor).toBe(0n);
});

test("an explicit sign out clears the queue and resumes from an empty store", async () => {
  const { server, runtime } = setup();
  const { calls } = fakeFetch({
    "GET /auth/session": { json: { authenticated: true } },
    "POST /auth/logout": { json: { authenticated: false } },
    ...signInRoutes,
  });
  fakeCredentials();
  const user = userEvent.setup();
  gate(runtime);
  await screen.findByText("the app");
  server.welcome("e1");
  useAthena.setState({ epoch: "e1", cursor: 7n });
  const waiting = runtime.outbox.submit({ case: "reply", value: { ticketId: "T-1", text: "hi" } });
  await runtime.outbox.settled();

  await act(() => runtime.signOut());
  expect(calls.some((c) => c.path === "/auth/logout")).toBe(true);
  expect((await waiting).code).toBe("unauthenticated");
  expect(runtime.outbox.pending()).toEqual([]);
  expect(await screen.findByRole("button", { name: "Sign in with passkey" })).toBeTruthy();
  expect(screen.queryByText(SIGNED_OUT_NOTICE)).toBeNull();
  expect(server.sockets).toHaveLength(1);

  await user.click(screen.getByRole("button", { name: "Sign in with passkey" }));
  await screen.findByText("the app");
  server.welcome("e2");
  const hello = server.socket.sent("hello")[0];
  expect([hello.epoch, hello.cursor]).toEqual(["", 0n]);
});

test("three failed connects check the session", async () => {
  vi.useFakeTimers();
  const { server, runtime } = setup();
  let asked = 0;
  const { calls } = fakeFetch({
    // The first answer is the start-up check; after that the session is gone.
    "GET /auth/session": () => ({ json: { authenticated: asked++ === 0 } }),
  });
  const checks = () => calls.filter((c) => c.path === "/auth/session").length;
  gate(runtime);
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(screen.getByText("the app")).toBeTruthy();
  expect(checks()).toBe(1);

  // An upgrade refused with 401 reaches the page as a plain close, never a Welcome.
  const fail = () => act(() => vi.advanceTimersByTimeAsync(20_000).then(() => server.socket.serverClose(1006)));
  server.socket.serverClose(1006);
  await fail();
  expect(checks()).toBe(1);
  expect(screen.getByText("the app")).toBeTruthy();
  await fail();
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(checks()).toBe(2);
  expect(screen.getByText(SIGNED_OUT_NOTICE)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Sign in with passkey" })).toBeTruthy();
  // And the link stops trying with a dead session.
  const n = server.sockets.length;
  await act(() => vi.advanceTimersByTimeAsync(60_000));
  expect(server.sockets).toHaveLength(n);
});

test("the session is asked once per three failures, and a Welcome starts the count again", async () => {
  vi.useFakeTimers();
  const { server, runtime } = setup();
  const { calls } = fakeFetch({ "GET /auth/session": { json: { authenticated: true } } });
  const checks = () => calls.filter((c) => c.path === "/auth/session").length;
  gate(runtime);
  await act(() => vi.advanceTimersByTimeAsync(0));
  const fail = () => act(() => vi.advanceTimersByTimeAsync(20_000).then(() => server.socket.serverClose(1006)));
  server.socket.serverClose(1006);
  await fail();
  // A connection that did reach Welcome and then dropped is the network, not the session.
  await act(() => vi.advanceTimersByTimeAsync(20_000));
  server.welcome();
  server.socket.serverClose(1006);
  await fail();
  await fail();
  expect(checks()).toBe(1); // 2 failures after the Welcome: not yet
  await fail();
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(checks()).toBe(2);
  // Still signed in, so still connecting; the next check needs three more failures.
  await fail();
  await fail();
  expect(checks()).toBe(2);
  await fail();
  await act(() => vi.advanceTimersByTimeAsync(0));
  expect(checks()).toBe(3);
  expect(screen.getByText("the app")).toBeTruthy();
});
