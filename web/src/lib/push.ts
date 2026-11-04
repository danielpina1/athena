// Web Push on this device: whether it can, may and does get notifications, and turning them on or off. A subscription
// is made with the server's VAPID key (GET /push/key) and given to the server with PushSubscribe; plan 3 ties it to
// the web session, so after every sign-in the app sends the existing one again (resubscribePush), quietly: it never
// asks for permission and never subscribes a device the owner turned off.
import * as actions from "@/lib/commands/actions";

export type PushState = "unsupported" | "denied" | "off" | "on";

/** A failure to turn notifications on or off, in a sentence for the owner. */
export class PushError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "PushError";
  }
}

/** Whether this device holds a subscription the server was given; unknown (false) until enable or resubscribe. */
let subscribed = false;
const listeners = new Set<() => void>();

function changed(on: boolean): PushState {
  subscribed = on;
  for (const l of [...listeners]) l();
  return pushState();
}

function supported(): boolean {
  return (
    typeof navigator !== "undefined" &&
    "serviceWorker" in navigator &&
    typeof globalThis.PushManager !== "undefined" &&
    typeof globalThis.Notification !== "undefined"
  );
}

/**
 * unsupported: no Push API here (iOS Safari before the app is on the Home Screen, or no service worker); denied: the
 * browser blocks this site's notifications; on: subscribed and the server told; off: anything else.
 */
export function pushState(): PushState {
  if (!supported()) return "unsupported";
  if (Notification.permission === "denied") return "denied";
  return subscribed && Notification.permission === "granted" ? "on" : "off";
}

/** Calls cb whenever pushState may have changed; returns the unsubscribe. */
export function onPushState(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

/** The service worker's registration, once it is active; none in the dev server, which registers no worker. */
async function registration(): Promise<ServiceWorkerRegistration | undefined> {
  if (!(await navigator.serviceWorker.getRegistration())) return undefined;
  return navigator.serviceWorker.ready;
}

/** The server's VAPID public key, decoded from base64url. */
async function serverKey(): Promise<Uint8Array<ArrayBuffer>> {
  let res: Response;
  try {
    res = await fetch("/push/key", { credentials: "same-origin", cache: "no-store" });
  } catch {
    throw new PushError("Could not reach Athena. Check your connection.");
  }
  // No key: the server has no https origin, which Web Push needs.
  if (!res.ok) throw new PushError("Notifications are not available on this server: it needs an https address.");
  const b64 = (await res.text()).trim().replace(/-/g, "+").replace(/_/g, "/");
  try {
    return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
  } catch {
    throw new PushError("The server sent an unreadable notification key.");
  }
}

function sameKey(a: ArrayBuffer | null | undefined, b: Uint8Array): boolean {
  if (!a) return false;
  const x = new Uint8Array(a);
  return x.length === b.length && x.every((v, i) => v === b[i]);
}

async function tellServer(sub: PushSubscription): Promise<void> {
  const r = await actions.pushSubscribe(sub.toJSON());
  if (!r.ok) throw new PushError(actions.errorMessage(r.code));
}

/**
 * Asks for permission, subscribes with the server's key (replacing a subscription made with an older key) and gives
 * the subscription to the server. Resolves with the new state; a refused permission resolves "denied" or "off".
 * Throws PushError with the sentence to show when it cannot finish.
 */
export async function enablePush(): Promise<PushState> {
  if (!supported()) return "unsupported";
  if ((await Notification.requestPermission()) !== "granted") return changed(false);
  const reg = await registration();
  if (!reg) throw new PushError("Notifications need the installed app; reload the page and try again.");
  const key = await serverKey();
  let sub = await reg.pushManager.getSubscription();
  if (sub && !sameKey(sub.options.applicationServerKey, key)) {
    await sub.unsubscribe();
    sub = null;
  }
  try {
    sub ??= await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
  } catch {
    throw new PushError("This browser would not subscribe to notifications. Try again later.");
  }
  await tellServer(sub);
  return changed(true);
}

/** Unsubscribes this device and tells the server; resolves with the new state. */
export async function disablePush(): Promise<PushState> {
  if (!supported()) return "unsupported";
  const sub = await (await registration())?.pushManager.getSubscription();
  if (sub) {
    await sub.unsubscribe();
    // The outbox delivers it even if the link is down now; a push service refusing the gone endpoint drops it too.
    await actions.pushUnsubscribe(sub.endpoint);
  }
  return changed(false);
}

/**
 * After a sign-in: gives the server this device's existing subscription again, when notifications are allowed.
 * Never prompts and never makes a new subscription; any failure is silent (the setting shows the state).
 */
export async function resubscribePush(): Promise<void> {
  try {
    if (!supported() || Notification.permission !== "granted") return;
    const sub = await (await registration())?.pushManager.getSubscription();
    changed(sub !== null);
    // On at once: the outbox delivers it once the link is open. A refusal turns it off.
    if (sub && !(await actions.pushSubscribe(sub.toJSON())).ok) changed(false);
  } catch {
    // quiet: the Notifications setting offers to turn it on again
  }
}

/** Forgets what this module knows; tests only. */
export function __resetPushForTest(): void {
  subscribed = false;
  listeners.clear();
}
