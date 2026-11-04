// The app's runtime: the one link to the server, the store it feeds, the outbox of unsent commands, and the session
// state that decides what the owner sees (checking, signed out, signed in). main.tsx builds it once (boot()); tests
// build their own with createRuntime. Nothing here touches React: SessionGate renders from `gate`.
import { createStore, type StoreApi } from "zustand/vanilla";
import { cancelReauth, requestReauth } from "@/features/auth/ReauthDialog";
import { getSession, bindSession, signOut as endSession } from "@/lib/auth/session";
import { setOutbox } from "@/lib/commands/actions";
import { connectOutbox, linkSender, Outbox } from "@/lib/commands/outbox";
import { LinkClient } from "@/lib/link/client";
import { connectStore, useAthena } from "@/lib/store/state";
import { toast } from "@/ui/primitives/toast";

export type Phase = { kind: "checking" } | { kind: "signedOut"; notice?: string } | { kind: "signedIn" };

export const SIGNED_OUT_NOTICE = "You were signed out on this device.";
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
  bindSession({ outbox, store });
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
  };

  let checked: Promise<void> | undefined;
  return {
    client,
    outbox,
    store,
    gate,
    checkSession() {
      checked ??= (async () => {
        try {
          if ((await getSession()).signedIn) connect();
          else setPhase({ kind: "signedOut" });
        } catch {
          setPhase({ kind: "signedOut", notice: "Could not reach Athena. Check your connection." });
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
      resetCount();
      setPhase({ kind: "signedOut" });
    },
  };
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
  }
  return runtime;
}
