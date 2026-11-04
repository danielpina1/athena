// The app's runtime: the one link to the server, the store it feeds, the outbox of unsent commands, and the session
// state that decides what the owner sees (checking, signed out, signed in). main.tsx builds it once (boot()); tests
// build their own with createRuntime. Nothing here touches React: SessionGate renders from `gate`.
import { createStore, type StoreApi } from "zustand/vanilla";
import { cancelReauth, requestReauth } from "@/features/auth/ReauthDialog";
import { AuthError, getSession, bindSession, signOut as endSession } from "@/lib/auth/session";
import { setOutbox } from "@/lib/commands/actions";
import { connectOutbox, linkSender, Outbox } from "@/lib/commands/outbox";
import { LinkClient } from "@/lib/link/client";
import { resubscribePush } from "@/lib/push";
import { connectStreams } from "@/lib/stream/useSessionStream";
import { connectStore, useAthena } from "@/lib/store/state";
import { appUpdates } from "@/lib/update";
import { trackVisibility } from "@/lib/visibility";
import { toast } from "@/ui/primitives/toast";

export type Phase = { kind: "checking" } | { kind: "signedOut"; notice?: string } | { kind: "signedIn" };

export const SIGNED_OUT_NOTICE = "You were signed out on this device.";
export const SERVER_ERROR_NOTICE = "Athena's server had an error. Try again in a moment.";
/** Link attempts that fail before a Welcome, in a row, after which the session is checked. */
export const FAILURES_BEFORE_CHECK = 3;

export type Runtime = {
  client: LinkClient;
  outbox: Outbox;
  store: typeof useAthena;
  gate: StoreApi<{ phase: Phase }>;
  /** Asks the server who this is and, if signed in, connects. Safe to call twice: it asks once. */
  checkSession(): Promise<void>;
  /** The owner just signed in: connect afresh with the new session. Queued commands go out on the first open. */
  signedIn(): void;
  /** Stops the link and the listeners; for tests and teardown. */
  dispose(): void;
  /** Explicit sign-out: ends the session, forgets the queue and the state. Throws if the server did not take it. */
  signOut(): Promise<void>;
};

export type RuntimeOptions = {
  url: string;
  clientId: string;
  WebSocketImpl?: typeof WebSocket;
  indexedDB?: IDBFactory;
  store?: typeof useAthena;
};

export function createRuntime(opts: RuntimeOptions): Runtime {
  const store = opts.store ?? useAthena;
  const client = new LinkClient({ url: opts.url, clientId: opts.clientId, WebSocketImpl: opts.WebSocketImpl });
  connectStore(client, store);
  const outbox = new Outbox(opts.indexedDB ?? indexedDB, linkSender(client), {
    store,
    notify: (title, description) => toast({ title, description, tone: "danger" }),
  });
  connectOutbox(client, outbox);
  bindSession({ outbox, store, onEnded: () => leave(SIGNED_OUT_NOTICE) });
  outbox.setReauthHandler(requestReauth);

  const gate = createStore<{ phase: Phase }>(() => ({ phase: { kind: "checking" } }));
  const setPhase = (phase: Phase) => gate.setState({ phase });
  const phase = () => gate.getState().phase.kind;

  // Pre-Welcome failures in a row: an upgrade refused with 401 shows up as a plain close, so after a few the session
  // is asked about, once per run of failures.
  let failures = 0;
  let welcomed = false;
  const resetCount = () => {
    failures = 0;
    welcomed = false;
  };

  /**
   * Back to sign-in. The state is emptied (the next connect resumes from nothing, so its Snapshot replaces
   * everything and no stale epoch or cursor is used) but the outbox is kept: its commands go out after sign-in.
   */
  const leave = (notice?: string) => {
    // Commands held for a step-up stay (released, not failed) and go out after the next sign-in.
    outbox.releaseHeld();
    cancelReauth();
    client.stop();
    store.getState().reset();
    resetCount();
    setPhase({ kind: "signedOut", notice });
  };

  const verifySession = async () => {
    try {
      const s = await getSession();
      if (!s.signedIn && phase() === "signedIn") leave(SIGNED_OUT_NOTICE);
    } catch {
      // No network to ask: the link's own reconnects carry on.
    }
  };

  client.onStatus((s) => {
    if (s === "signedOut") {
      leave(SIGNED_OUT_NOTICE);
      return;
    }
    if (phase() !== "signedIn") return;
    if (s === "open") {
      welcomed = true;
      failures = 0;
    } else if (s === "offline") {
      if (welcomed) {
        welcomed = false; // a drop after a Welcome is the network, not the session
      } else if (++failures >= FAILURES_BEFORE_CHECK) {
        failures = 0;
        void verifySession();
      }
    }
  });
  // The server refused a command as unauthenticated: the session is gone though the link had not noticed yet.
  outbox.setSignOutHandler(() => leave(SIGNED_OUT_NOTICE));

  const connect = () => {
    resetCount();
    client.stop();
    client.start();
    setPhase({ kind: "signedIn" });
    // Plan 3 ends a push subscription with its session: this session is given the device's again.
    void resubscribePush();
  };

  // The start-up check failed (no network, or a server error): ask again when the network is back or the page is shown.
  // In the offline shell the link keeps trying on its own (backoff up to 15 s): each new attempt asks again too, so a
  // device that never went offline (a server restart behind a proxy) recovers without an online or visibility event.
  let checked: Promise<void> | undefined;
  let watching = false;
  let offLinkAttempts: (() => void) | undefined;
  const recheck = () => {
    unwatchOnline();
    checked = undefined;
    void runtime.checkSession();
  };
  const retryCheck = (ev: Event) => {
    if (ev.type === "visibilitychange" && document.visibilityState !== "visible") return;
    recheck();
  };
  function unwatchOnline() {
    if (!watching) return;
    watching = false;
    window.removeEventListener("online", retryCheck);
    document.removeEventListener("visibilitychange", retryCheck);
    offLinkAttempts?.();
    offLinkAttempts = undefined;
  }
  const watchOnline = () => {
    if (watching) return;
    watching = true;
    window.addEventListener("online", retryCheck);
    document.addEventListener("visibilitychange", retryCheck);
    offLinkAttempts = client.onStatus((st) => {
      if (st === "connecting" || st === "open") recheck();
    });
  };
  const runtime: Runtime = {
    client,
    outbox,
    store,
    gate,
    checkSession() {
      checked ??= (async () => {
        try {
          if ((await getSession()).signedIn) {
            // Already showing the offline shell: its link is running and reconnects on its own.
            if (phase() !== "signedIn") connect();
          } else if (phase() === "signedIn") {
            leave(SIGNED_OUT_NOTICE); // the offline shell, and the session ended meanwhile
          } else {
            setPhase({ kind: "signedOut" });
          }
        } catch (e) {
          if (e instanceof AuthError) {
            // The server answered with an error (a 5xx, or a proxy's while Athena restarts): it did not say "signed
            // out". At start-up that is sign-in with an honest notice; in the offline shell the shell, its store and
            // its link stay, and the check runs again. Only a definite signed-out answer leaves.
            if (phase() !== "signedIn") setPhase({ kind: "signedOut", notice: SERVER_ERROR_NOTICE });
          } else if (navigator.serviceWorker?.controller) {
            // No network, in the installed app (the service worker served this page from its cache): the app, with
            // the link's "unreachable" banner, until the server answers.
            if (phase() !== "signedIn") connect();
          } else {
            setPhase({ kind: "signedOut", notice: "Could not reach Athena. Check your connection." });
          }
          watchOnline();
        }
      })();
      return checked;
    },
    signedIn: connect,
    async signOut() {
      client.stop();
      try {
        await endSession();
      } catch (e) {
        client.start();
        throw e;
      }
      cancelReauth(); // no stale step-up request survives into the next login
      resetCount();
      setPhase({ kind: "signedOut" });
    },
    dispose() {
      client.stop();
      unwatchOnline();
    },
  };
  return runtime;
}

const CLIENT_ID_KEY = "athena.clientId";
const BASE32 = "abcdefghijklmnopqrstuvwxyz234567";

/** This device's stable link id: 16 random base32 characters, kept in localStorage (the only thing kept there but the theme). */
export function deviceClientId(): string {
  try {
    const have = localStorage.getItem(CLIENT_ID_KEY);
    if (have && /^[a-z2-7]{16}$/.test(have)) return have;
  } catch {
    // storage off: a fresh id for this page load
  }
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  const id = Array.from(bytes, (b) => BASE32[b & 31]).join("");
  try {
    localStorage.setItem(CLIENT_ID_KEY, id);
  } catch {
    // not kept: the next load gets another
  }
  return id;
}

let runtime: Runtime | undefined;

/** The app's one runtime, built on first use. */
export function boot(): Runtime {
  if (!runtime) {
    const scheme = location.protocol === "https:" ? "wss" : "ws";
    runtime = createRuntime({ url: `${scheme}://${location.host}/ws`, clientId: deviceClientId() });
    setOutbox(runtime.outbox);
    connectStreams(runtime.client);
    trackVisibility(runtime.client);
    // A server update restarts the server: after each reconnect, ask whether a new app is waiting.
    let opened = 0;
    runtime.client.onStatus((s) => {
      if (s === "open" && ++opened > 1) appUpdates()?.check();
    });
  }
  return runtime;
}
