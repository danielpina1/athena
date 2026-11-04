/// <reference lib="webworker" />
// Athena's service worker, served at /sw.js with scope /. It keeps the app's shell for offline starts, serves the
// build's hashed files from its cache, shows Web Push notifications and opens their Ticket on a tap. It imports
// nothing and exports nothing, so the build emits it as a classic script.
//
//   install   precaches /, /index.html and every hashed file of this build (the list is written in at build time).
//   activate  deletes the caches of older builds and takes control of the open pages.
//   fetch     navigations: the network, or after 3 s or offline the cached index.html (the offline shell);
//             /assets/*: the cache first; the server's own routes (/ws, /auth, /api, /push, /healthz, /enroll, the
//             CA downloads) and anything not a same-origin GET: never touched.
//   push      plan 3's notification JSON {title, body, url, tag}; the tag is the Ticket id, so a later push for the
//             same Ticket replaces the earlier notification.
//   updates   a new worker waits until the page sends SKIP_WAITING (the owner tapped "Reload").

declare const self: ServiceWorkerGlobalScope;
/** Written in by the build (vite.config.ts, precachePlugin): this build's id and its hashed files. */
declare const __ATHENA_BUILD__: { version: string; assets: string[] };

const BUILD = __ATHENA_BUILD__;
const PREFIX = "athena-";
const CACHE = PREFIX + BUILD.version;
const SHELL = "/index.html";
const NAVIGATION_TIMEOUT_MS = 3000;
/** The server's own routes: never cached, never answered with the shell. */
const SERVER_ROUTES = /^\/(ws|auth|api|push|healthz|enroll|ca\.pem|ca\.mobileconfig)(\/|$)/;

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(["/", SHELL, ...BUILD.assets])));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    (async () => {
      for (const name of await caches.keys()) {
        if (name.startsWith(PREFIX) && name !== CACHE) await caches.delete(name);
      }
      await self.clients.claim();
    })(),
  );
});

self.addEventListener("message", (e) => {
  if ((e.data as { type?: string } | null)?.type === "SKIP_WAITING") void self.skipWaiting();
});

self.addEventListener("fetch", (e) => {
  const req = e.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin || SERVER_ROUTES.test(url.pathname)) return;
  if (req.mode === "navigate") e.respondWith(navigation(req));
  else if (url.pathname.startsWith("/assets/")) e.respondWith(cacheFirst(req));
});

/** The network's page, unless it fails or takes over 3 s: then the cached shell (the app's routes are client-side). */
async function navigation(req: Request): Promise<Response> {
  const net = fetch(req);
  net.catch(() => {}); // abandoned after the timeout: its failure is nobody's
  let resolveLate: (v: undefined) => void = () => {};
  const late = new Promise<undefined>((resolve) => (resolveLate = resolve));
  const timer = setTimeout(() => resolveLate(undefined), NAVIGATION_TIMEOUT_MS);
  try {
    const res = await Promise.race([net, late]);
    if (res) return res;
  } catch {
    // offline: the shell below
  } finally {
    clearTimeout(timer);
  }
  const shell = await (await caches.open(CACHE)).match(SHELL);
  return shell ?? net; // nothing cached: as if there were no worker
}

async function cacheFirst(req: Request): Promise<Response> {
  return (await (await caches.open(CACHE)).match(req)) ?? fetch(req);
}

type PushMessage = { title?: unknown; body?: unknown; tag?: unknown };

self.addEventListener("push", (e) => {
  let msg: PushMessage = {};
  try {
    msg = (e.data?.json() as PushMessage | null) ?? {};
  } catch {
    // unreadable: a push must still show something
  }
  const tag = typeof msg.tag === "string" ? msg.tag : "";
  const options: NotificationOptions = {
    body: typeof msg.body === "string" ? msg.body : "",
    data: { url: tag ? `/tickets/${encodeURIComponent(tag)}` : "/" },
    icon: "/icons/icon-192.png",
  };
  if (tag) options.tag = tag;
  e.waitUntil(self.registration.showNotification(typeof msg.title === "string" && msg.title ? msg.title : "Athena", options));
});

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const want = (e.notification.data as { url?: unknown } | null)?.url;
  const url = typeof want === "string" && want.startsWith("/") ? want : "/";
  e.waitUntil(openApp(url));
});

/** Focuses an open window of the app (the focused one, else a visible one, else any) and takes it to url, or opens one. */
async function openApp(url: string): Promise<void> {
  const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  const win = wins.find((c) => c.focused) ?? wins.find((c) => c.visibilityState === "visible") ?? wins[0];
  if (win) {
    try {
      const focused = await win.focus();
      if (await focused.navigate(url)) return;
    } catch {
      // a window this worker does not control cannot be navigated: open one instead
    }
  }
  await self.clients.openWindow(url);
}

export {};
