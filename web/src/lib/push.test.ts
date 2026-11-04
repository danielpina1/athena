import { create } from "@bufbuild/protobuf";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { IDBFactory } from "fake-indexeddb";
import { createElement } from "react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { createRuntime, type Runtime } from "@/app/boot";
import { SessionGate } from "@/app/SessionGate";
import { CommandResultSchema, type Command } from "@/gen/athena/v1/commands_pb";
import { ASSERTION, fakeCredentials, fakeFetch } from "@/lib/auth/testkit";
import * as actions from "@/lib/commands/actions";
import { Outbox } from "@/lib/commands/outbox";
import { FakeServer } from "@/lib/link/fakeServer";
import { useAthena } from "@/lib/store/state";
import { __resetPushForTest, disablePush, enablePush, onPushError, pushPending, pushState } from "./push";

// The server's VAPID public key as GET /push/key sends it: base64url of an uncompressed P-256 point (65 bytes).
const KEY_BYTES = Uint8Array.from({ length: 65 }, (_, i) => (i === 0 ? 4 : i));
const KEY = btoa(String.fromCharCode(...KEY_BYTES)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const ENDPOINT = "https://push.example/send/abc";

type FakeSub = { endpoint: string; options: { applicationServerKey: ArrayBuffer | null }; toJSON(): PushSubscriptionJSON; unsubscribe: ReturnType<typeof vi.fn> };

function fakeSub(key: Uint8Array = KEY_BYTES): FakeSub {
  return {
    endpoint: ENDPOINT,
    options: { applicationServerKey: key.slice().buffer },
    toJSON: () => ({ endpoint: ENDPOINT, keys: { p256dh: "pk", auth: "au" } }),
    unsubscribe: vi.fn(async () => true),
  };
}

let current: FakeSub | null;
let permission: NotificationPermission;
let pushManager: { getSubscription: ReturnType<typeof vi.fn>; subscribe: ReturnType<typeof vi.fn> };
let requestPermission: ReturnType<typeof vi.fn>;

beforeEach(() => {
  useAthena.getState().reset();
  __resetPushForTest();
  current = null;
  permission = "default";
  requestPermission = vi.fn(async () => (permission = "granted"));
  pushManager = {
    getSubscription: vi.fn(async () => current),
    subscribe: vi.fn(async () => (current = fakeSub())),
  };
  const reg = { pushManager };
  vi.stubGlobal("PushManager", class {});
  vi.stubGlobal("Notification", {
    get permission() {
      return permission;
    },
    requestPermission,
  });
  Object.defineProperty(navigator, "serviceWorker", {
    configurable: true,
    value: { getRegistration: vi.fn(async () => reg), ready: Promise.resolve(reg) },
  });
});

afterEach(() => {
  actions.setOutbox(undefined);
  vi.unstubAllGlobals();
  delete (navigator as { serviceWorker?: unknown }).serviceWorker;
});

/** fetch answering GET /push/key with the key as text. */
function keyFetch() {
  const fn = vi.fn(async (input: RequestInfo | URL) => {
    if (String(input) !== "/push/key") throw new Error(`unexpected request ${String(input)}`);
    return new Response(KEY, { headers: { "Content-Type": "text/plain" } });
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

describe("enablePush", () => {
  let sent: Command[];
  let outbox: Outbox;
  beforeEach(() => {
    sent = [];
    outbox = new Outbox(new IDBFactory(), (c) => {
      sent.push(c);
      return true;
    });
    outbox.onOpen();
    actions.setOutbox(outbox);
  });

  /** Answers every command sent so far with ok, as the server would. */
  const answerAll = async () => {
    await vi.waitFor(() => expect(sent.length).toBeGreaterThan(0));
    for (const c of sent) outbox.onResult(create(CommandResultSchema, { id: c.id, ok: true }));
  };

  test("enablePush subscribes and sends PushSubscribe", async () => {
    const fetch = keyFetch();
    expect(pushState()).toBe("off");
    const done = enablePush();
    await answerAll();
    expect(await done).toBe("on");
    expect(pushState()).toBe("on");

    expect(requestPermission).toHaveBeenCalledTimes(1);
    expect(fetch).toHaveBeenCalledWith("/push/key", expect.objectContaining({ credentials: "same-origin", cache: "no-store" }));
    const opts = pushManager.subscribe.mock.calls[0][0] as PushSubscriptionOptionsInit;
    expect(opts.userVisibleOnly).toBe(true);
    expect(new Uint8Array(opts.applicationServerKey as Uint8Array)).toEqual(KEY_BYTES);
    expect(sent.map((c) => c.action)).toEqual([
      { case: "pushSubscribe", value: expect.objectContaining({ endpoint: ENDPOINT, p256dh: "pk", auth: "au" }) },
    ]);
  });

  test("a subscription made with an older server key is replaced", async () => {
    keyFetch();
    permission = "granted";
    const stale = (current = fakeSub(new Uint8Array(65).fill(9)));
    const done = enablePush();
    await answerAll();
    expect(await done).toBe("on");
    expect(stale.unsubscribe).toHaveBeenCalled();
    expect(pushManager.subscribe).toHaveBeenCalledTimes(1);
  });

  test("a refused permission sends nothing and says denied", async () => {
    const fetch = keyFetch();
    requestPermission.mockImplementation(async () => (permission = "denied"));
    expect(await enablePush()).toBe("denied");
    expect(pushState()).toBe("denied");
    expect(fetch).not.toHaveBeenCalled();
    expect(pushManager.subscribe).not.toHaveBeenCalled();
    expect(sent).toEqual([]);
  });

  test("offline, enablePush resolves once queued and stays pending until the server answers", async () => {
    keyFetch();
    // No answer comes (offline, the outbox holds the command): enablePush must not wait for one.
    const errors: string[] = [];
    onPushError((m) => errors.push(m));
    expect(await enablePush()).toBe("on");
    expect(pushPending()).toBe(true);

    // The server refuses it later: off again, and the owner is told why.
    await vi.waitFor(() => expect(sent).toHaveLength(1));
    outbox.onResult(create(CommandResultSchema, { id: sent[0].id, ok: false, code: "unavailable" }));
    await vi.waitFor(() => expect(pushPending()).toBe(false));
    expect(pushState()).toBe("off");
    expect(errors).toEqual([actions.errorMessage("unavailable")]);
  });

  test("a refused PushUnsubscribe is told too", async () => {
    keyFetch();
    const on = enablePush();
    await answerAll();
    await on;
    sent.length = 0;
    const errors: string[] = [];
    onPushError((m) => errors.push(m));
    expect(await disablePush()).toBe("off");
    expect(pushPending()).toBe(true);
    await vi.waitFor(() => expect(sent).toHaveLength(1));
    outbox.onResult(create(CommandResultSchema, { id: sent[0].id, ok: false, code: "internal" }));
    await vi.waitFor(() => expect(errors).toEqual([actions.errorMessage("internal")]));
    expect(pushPending()).toBe(false);
    expect(pushState()).toBe("off");
  });

  test("no push key on the server (plain HTTP) fails without subscribing", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("404 page not found", { status: 404 })));
    await expect(enablePush()).rejects.toThrow(/not available/i);
    expect(pushManager.subscribe).not.toHaveBeenCalled();
    expect(pushState()).toBe("off");
  });

  test("disablePush unsubscribes here and tells the server", async () => {
    keyFetch();
    const on = enablePush();
    await answerAll();
    await on;
    const sub = current!;
    sent.length = 0;
    const off = disablePush();
    await answerAll();
    expect(await off).toBe("off");
    expect(sub.unsubscribe).toHaveBeenCalled();
    expect(sent.map((c) => c.action)).toEqual([{ case: "pushUnsubscribe", value: expect.objectContaining({ endpoint: ENDPOINT }) }]);
  });
});

test("pushState is unsupported without the Push API (iOS Safari outside the Home Screen)", () => {
  vi.stubGlobal("PushManager", undefined);
  expect(pushState()).toBe("unsupported");
});

describe("after sign-in", () => {
  const made: Runtime[] = [];
  afterEach(() => {
    for (const r of made.splice(0)) r.dispose();
  });

  function signInFlow() {
    const server = new FakeServer();
    const runtime = createRuntime({
      url: "ws://athena.test/ws",
      clientId: "abcdefghijklmnop",
      WebSocketImpl: server.WebSocket,
      indexedDB: new IDBFactory(),
    });
    made.push(runtime);
    actions.setOutbox(runtime.outbox);
    fakeFetch({
      "GET /auth/session": { json: { authenticated: false } },
      "POST /auth/login/begin": { json: ASSERTION },
      "POST /auth/login/finish": { json: { authenticated: true } },
    });
    fakeCredentials();
    render(createElement(SessionGate, { runtime, children: createElement("p", null, "the app") }));
    return { server, runtime };
  }

  async function signIn(server: FakeServer, runtime: Runtime) {
    await userEvent.setup().click(await screen.findByRole("button", { name: "Sign in with passkey" }));
    await screen.findByText("the app");
    server.welcome();
    await waitFor(() => expect(runtime.client.status).toBe("open"));
    await runtime.outbox.settled();
  }

  test("re-subscribes after sign-in", async () => {
    // Plan 3 ends a subscription with the session it was made in: the next session sends it again, quietly.
    permission = "granted";
    current = fakeSub();
    const { server, runtime } = signInFlow();
    await signIn(server, runtime);
    await waitFor(() =>
      expect(server.socket.sent("command").map((c) => c.action)).toContainEqual({
        case: "pushSubscribe",
        value: expect.objectContaining({ endpoint: ENDPOINT, p256dh: "pk", auth: "au" }),
      }),
    );
    expect(requestPermission).not.toHaveBeenCalled();
    expect(pushManager.subscribe).not.toHaveBeenCalled();
    expect(pushState()).toBe("on");
  });

  test("never prompts and sends nothing without permission or a subscription", async () => {
    // Permission not granted: nothing, and no prompt.
    current = fakeSub();
    const first = signInFlow();
    await signIn(first.server, first.runtime);
    // Granted, but the owner turned notifications off here (no subscription): it stays off.
    permission = "granted";
    current = null;
    first.runtime.signedIn();
    first.server.welcome();
    await first.runtime.outbox.settled();
    await new Promise((r) => setTimeout(r, 0));
    const cases = first.server.sockets.flatMap((s) => s.sent("command").map((c) => c.action.case));
    expect(cases).not.toContain("pushSubscribe");
    expect(requestPermission).not.toHaveBeenCalled();
    expect(pushState()).toBe("off");
  });
});
