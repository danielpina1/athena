// The service worker against a mocked `self`: each test loads sw.ts afresh into a fake worker scope (listeners,
// registration, clients, caches) and dispatches the events the browser would.
import { afterEach, beforeEach, expect, test, vi } from "vitest";

const ORIGIN = "https://athena.test";
const ASSETS = ["/assets/index-abc.js", "/assets/index-abc.css", "/assets/graph-def.js"];

type Listener = (e: unknown) => void;
type Req = { url: string; method: string; mode: string };

/** A CacheStorage in memory: caches by name, each a map from path to the body it holds. */
function fakeCaches(network: (path: string) => Promise<Response>) {
  const store = new Map<string, Map<string, string>>();
  const pathOf = (r: string | Req) => new URL(typeof r === "string" ? r : r.url, ORIGIN).pathname;
  const open = async (name: string) => {
    let c = store.get(name);
    if (!c) store.set(name, (c = new Map()));
    const cache = c;
    return {
      async addAll(paths: string[]) {
        for (const p of paths) cache.set(p, await (await network(p)).text());
      },
      async match(r: string | Req) {
        const body = cache.get(pathOf(r));
        return body === undefined ? undefined : new Response(body);
      },
      async put(r: string | Req, res: Response) {
        cache.set(pathOf(r), await res.text());
      },
    };
  };
  return {
    store,
    api: {
      open: vi.fn(open),
      keys: vi.fn(async () => [...store.keys()]),
      delete: vi.fn(async (name: string) => store.delete(name)),
      match: vi.fn(async () => {
        throw new Error("sw must read its own cache, not every cache");
      }),
    },
  };
}

type Client = { url: string; focused: boolean; visibilityState: string; focus: ReturnType<typeof vi.fn>; navigate: ReturnType<typeof vi.fn> };

function client(url = `${ORIGIN}/`): Client {
  const c: Client = {
    url,
    focused: false,
    visibilityState: "hidden",
    focus: vi.fn(async () => c),
    navigate: vi.fn(async () => c),
  };
  return c;
}

let listeners: Map<string, Listener[]>;
let scope: {
  registration: { showNotification: ReturnType<typeof vi.fn>; pushManager: { subscribe: ReturnType<typeof vi.fn> } };
  clients: { claim: ReturnType<typeof vi.fn>; matchAll: ReturnType<typeof vi.fn>; openWindow: ReturnType<typeof vi.fn> };
  skipWaiting: ReturnType<typeof vi.fn>;
};
let windows: Client[];
let caches: ReturnType<typeof fakeCaches>;
let network: ReturnType<typeof vi.fn<(input: string | Req) => Promise<Response>>>;

beforeEach(async () => {
  listeners = new Map();
  windows = [];
  network = vi.fn<(input: string | Req) => Promise<Response>>(async (input) => new Response(`network ${new URL(typeof input === "string" ? input : input.url, ORIGIN).pathname}`));
  caches = fakeCaches((p) => network(p));
  scope = {
    registration: { showNotification: vi.fn(async () => {}), pushManager: { subscribe: vi.fn(async () => subscription("https://push.test/fresh")) } },
    clients: {
      claim: vi.fn(async () => {}),
      matchAll: vi.fn(async () => windows),
      openWindow: vi.fn(async () => null),
    },
    skipWaiting: vi.fn(async () => {}),
  };
  vi.stubGlobal("self", {
    ...scope,
    location: new URL(`${ORIGIN}/sw.js`),
    addEventListener: (type: string, fn: Listener) => listeners.set(type, [...(listeners.get(type) ?? []), fn]),
  });
  vi.stubGlobal("caches", caches.api);
  vi.stubGlobal("fetch", network);
  vi.stubGlobal("__ATHENA_BUILD__", { version: "v2", assets: ASSETS });
  vi.resetModules();
  await import("./sw");
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

/** Dispatches an extendable event: resolves once every waitUntil promise has settled; respondWith is captured. */
async function dispatch(type: string, fields: object = {}) {
  const waits: Promise<unknown>[] = [];
  let response: Promise<Response> | undefined;
  const e = {
    ...fields,
    waitUntil: (p: Promise<unknown>) => waits.push(p),
    respondWith: (p: Promise<Response> | Response) => (response = Promise.resolve(p)),
  };
  for (const fn of listeners.get(type) ?? []) fn(e);
  await Promise.all(waits);
  return { responded: response !== undefined, response: () => response!.then((r) => r.text()) };
}

const get = (path: string, mode = "cors"): Req => ({ url: ORIGIN + path, method: "GET", mode });

/** A PushSubscription as the worker sees one. */
function subscription(endpoint: string) {
  const json = { endpoint, keys: { p256dh: "pk-" + endpoint, auth: "auth" } };
  return { endpoint, options: { applicationServerKey: new Uint8Array([4, 1, 2]).buffer }, toJSON: () => json };
}

/** The bodies the worker posted to /push/renew. */
function renewals() {
  return (network.mock.calls as unknown as [string | Req, RequestInit][])
    .filter(([url]) => url === "/push/renew")
    .map(([, init]) => {
      expect(init).toMatchObject({ method: "POST", credentials: "same-origin" });
      return JSON.parse(init.body as string) as unknown;
    });
}

async function installed() {
  await dispatch("install");
  network.mockClear();
}

test("sw push shows title and tag and opens the ticket on click", async () => {
  // Plan 3's notification JSON (internal/web/notify.go).
  const payload = { title: "Athena", body: "Fix the login needs approval", url: "/tickets/T-7", tag: "T-7" };
  await dispatch("push", { data: { json: () => payload } });
  expect(scope.registration.showNotification).toHaveBeenCalledWith(
    "Athena",
    expect.objectContaining({ body: "Fix the login needs approval", tag: "T-7", data: { url: "/tickets/T-7" } }),
  );

  // A click with a window open focuses it and takes it to the Ticket.
  const open = client();
  windows = [open];
  const close = vi.fn();
  await dispatch("notificationclick", { notification: { data: { url: "/tickets/T-7" }, close } });
  expect(close).toHaveBeenCalled();
  expect(open.focus).toHaveBeenCalled();
  expect(open.navigate).toHaveBeenCalledWith("/tickets/T-7");
  expect(scope.clients.openWindow).not.toHaveBeenCalled();

  // With none open, a window opens at the Ticket.
  windows = [];
  await dispatch("notificationclick", { notification: { data: { url: "/tickets/T-7" }, close } });
  expect(scope.clients.openWindow).toHaveBeenCalledWith("/tickets/T-7");
});

test("sw push with the same tag replaces the earlier one, and one without a Ticket opens /", async () => {
  await dispatch("push", { data: { json: () => ({ title: "Athena", body: "a", tag: "T-7" }) } });
  await dispatch("push", { data: { json: () => ({ title: "Athena", body: "b", tag: "T-7" }) } });
  const tags = scope.registration.showNotification.mock.calls.map((c) => (c[1] as NotificationOptions).tag);
  expect(tags).toEqual(["T-7", "T-7"]);

  // An unreadable payload still shows something (a push must be visible) and opens the app's start.
  await dispatch("push", { data: { json: () => JSON.parse("{") } });
  const last = scope.registration.showNotification.mock.calls.at(-1)!;
  expect(last[0]).toBe("Athena");
  expect((last[1] as NotificationOptions).data).toEqual({ url: "/" });

  await dispatch("notificationclick", { notification: { data: { url: "/" }, close: vi.fn() } });
  expect(scope.clients.openWindow).toHaveBeenCalledWith("/");
});

test("sw posts a rotated push subscription to /push/renew with the old endpoint", async () => {
  // The browser made the new subscription.
  await dispatch("pushsubscriptionchange", {
    oldSubscription: subscription("https://push.test/old"),
    newSubscription: subscription("https://push.test/new"),
  });
  expect(renewals()).toEqual([
    { oldEndpoint: "https://push.test/old", subscription: subscription("https://push.test/new").toJSON() },
  ]);
  expect(scope.registration.pushManager.subscribe).not.toHaveBeenCalled();

  // It did not: the worker subscribes again with the old one's key.
  network.mockClear();
  await dispatch("pushsubscriptionchange", { oldSubscription: subscription("https://push.test/old"), newSubscription: null });
  expect(scope.registration.pushManager.subscribe).toHaveBeenCalledWith({
    userVisibleOnly: true,
    applicationServerKey: subscription("x").options.applicationServerKey,
  });
  expect(renewals()).toEqual([
    { oldEndpoint: "https://push.test/old", subscription: subscription("https://push.test/fresh").toJSON() },
  ]);

  // No old subscription proves nothing to the server: the next sign-in sends the current one. A failure is quiet.
  network.mockClear();
  await dispatch("pushsubscriptionchange", { oldSubscription: null, newSubscription: subscription("https://push.test/new") });
  expect(renewals()).toEqual([]);
  scope.registration.pushManager.subscribe.mockRejectedValueOnce(new Error("denied"));
  await dispatch("pushsubscriptionchange", { oldSubscription: subscription("https://push.test/old"), newSubscription: null });
  expect(renewals()).toEqual([]);
});

test("sw install precaches the shell and the build's assets; activate drops old caches and claims", async () => {
  caches.store.set("athena-v1", new Map([["/index.html", "old"]]));
  caches.store.set("other-app", new Map());
  await dispatch("install");
  expect([...caches.store.get("athena-v2")!.keys()].sort()).toEqual(["/", "/index.html", ...ASSETS].sort());
  expect(scope.skipWaiting).not.toHaveBeenCalled(); // a new worker waits for the owner's Reload

  await dispatch("activate");
  expect([...caches.store.keys()].sort()).toEqual(["athena-v2", "other-app"]);
  expect(scope.clients.claim).toHaveBeenCalled();

  await dispatch("message", { data: { type: "SKIP_WAITING" } });
  expect(scope.skipWaiting).toHaveBeenCalled();
});

test("sw navigation falls back to the cached shell offline", async () => {
  await installed();
  // Online: the network's page.
  let r = await dispatch("fetch", { request: get("/tickets/T-1", "navigate") });
  expect(await r.response()).toBe("network /tickets/T-1");

  // Offline: the cached index.html, for / and for a deep link alike.
  network.mockRejectedValue(new TypeError("Failed to fetch"));
  for (const path of ["/", "/tickets/T-1"]) {
    r = await dispatch("fetch", { request: get(path, "navigate") });
    expect(await r.response()).toBe("network /index.html");
  }

  // A network that does not answer within 3 s: the shell too.
  vi.useFakeTimers();
  network.mockImplementation(() => new Promise(() => {}));
  r = await dispatch("fetch", { request: get("/tickets/T-2", "navigate") });
  const body = r.response();
  await vi.advanceTimersByTimeAsync(2_999);
  let settled = false;
  void body.then(() => (settled = true));
  await Promise.resolve();
  expect(settled).toBe(false);
  await vi.advanceTimersByTimeAsync(1);
  expect(await body).toBe("network /index.html");
});

test("sw serves /assets/ from the cache first", async () => {
  await installed();
  const r = await dispatch("fetch", { request: get("/assets/graph-def.js") });
  expect(await r.response()).toBe("network /assets/graph-def.js");
  expect(network).not.toHaveBeenCalled();

  // An asset of another build (not precached) comes from the network.
  const other = await dispatch("fetch", { request: get("/assets/index-new.js") });
  expect(await other.response()).toBe("network /assets/index-new.js");
  expect(network).toHaveBeenCalledTimes(1);
});

test("sw never caches ws auth api", async () => {
  await installed();
  const before = JSON.stringify([...caches.store.get("athena-v2")!.entries()]);
  const passThrough: Req[] = [
    get("/ws"),
    get("/auth/session"),
    get("/auth/login/begin"),
    get("/api/tickets/T-1/diff"),
    get("/push/key"),
    get("/healthz"),
    // The enroll link carries its token in the fragment: the page always comes from the server.
    get("/enroll#token", "navigate"),
    get("/ca.mobileconfig", "navigate"),
    { url: `${ORIGIN}/auth/login/finish`, method: "POST", mode: "cors" },
    { url: "https://elsewhere.test/assets/x.js", method: "GET", mode: "cors" },
  ];
  for (const req of passThrough) {
    const r = await dispatch("fetch", { request: req });
    expect(r.responded, `${req.method} ${req.url}`).toBe(false);
  }
  expect(JSON.stringify([...caches.store.get("athena-v2")!.entries()])).toBe(before);
  expect(network).not.toHaveBeenCalled();
});

test("sw renotifies a replaced Ticket notification", async () => {
  await dispatch("push", { data: { json: () => ({ title: "Athena", body: "a", tag: "T-7" }) } });
  expect(scope.registration.showNotification.mock.calls[0][1]).toMatchObject({ tag: "T-7", renotify: true });
  // No tag: nothing to replace, so no renotify (a browser refuses renotify without a tag).
  await dispatch("push", { data: { json: () => ({ title: "Athena", body: "b" }) } });
  expect(scope.registration.showNotification.mock.calls[1][1]).not.toHaveProperty("renotify");
});

test("sw opens only same-origin urls from a notification", async () => {
  const close = vi.fn();
  for (const [want, opened] of [
    ["//evil.example/x", "/"],
    ["/\\evil.example", "/"],
    ["https://evil.example/tickets/T-1", "/"],
    [`${ORIGIN}/tickets/T-1?tab=session`, "/tickets/T-1?tab=session"],
    ["/tickets/T-1", "/tickets/T-1"],
  ]) {
    scope.clients.openWindow.mockClear();
    await dispatch("notificationclick", { notification: { data: { url: want }, close } });
    expect(scope.clients.openWindow, want).toHaveBeenCalledWith(opened);
  }
});
