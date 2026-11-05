// The owner's session, over plan 3's /auth/* routes: who is signed in, the passkey ceremonies (sign-in, enrollment,
// step-up) and sign-out. Every POST sends JSON and the same-origin cookie; the browser adds Origin itself.
import { useAthena } from "@/lib/store/state";
import { creationOptionsFromJSON, credentialToJSON, requestOptionsFromJSON } from "./webauthn";

/** passkeyId is the passkey this session signed in with (Settings › Devices marks it as this device). */
export type SessionInfo = { signedIn: boolean; passkeyId?: string; reauthAt?: Date };

/** The server refused (or the browser did): code is the server's error code, or "cancelled" / "unsupported". */
export class AuthError extends Error {
  constructor(
    readonly code: string,
    readonly status = 0,
    message = code,
  ) {
    super(message);
    this.name = "AuthError";
  }
}

type Clearable = { clear(): void };
let outboxRef: Clearable | undefined;
let storeRef: { getState(): { reset(): void } } = useAthena;

let endedRef: (() => void) | undefined;
let signOutRef: (() => Promise<void>) | undefined;

/**
 * Tells signOut what to clear: the app's outbox (and, in tests, its store). Called once at boot. onEnded is what runs
 * when an HTTP call learns the session is gone (sessionEnded): the app's way back to sign-in.
 */
export function bindSession(b: {
  outbox: Clearable;
  store?: typeof storeRef;
  onEnded?: () => void;
  /** The app's explicit sign-out (the runtime's: it also stops the link and shows sign-in). */
  onSignOut?: () => Promise<void>;
}): void {
  outboxRef = b.outbox;
  storeRef = b.store ?? useAthena;
  endedRef = b.onEnded;
  signOutRef = b.onSignOut;
}

/** The owner's Sign out: ends this session and shows sign-in. Throws when the server did not take it. */
export async function requestSignOut(): Promise<void> {
  await (signOutRef ?? signOut)();
}

/** An HTTP call came back 401: the session ended under the open app. Shows sign-in, keeping unsent commands. */
export function sessionEnded(): void {
  endedRef?.();
}

async function request(method: "GET" | "POST", path: string, body?: unknown): Promise<unknown> {
  const init: RequestInit = { method, credentials: "same-origin", cache: "no-store" };
  if (method === "POST") {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(body ?? {});
  }
  const res = await fetch(path, init);
  let json: unknown;
  try {
    json = await res.json();
  } catch {
    json = undefined;
  }
  if (!res.ok) {
    const e = (json ?? {}) as { code?: string; error?: string };
    throw new AuthError(e.code ?? "internal", res.status, e.error);
  }
  return json;
}

/** GET /auth/session: signedIn, the session's passkey, and reauthAt, the end of the open step-up window, while there is one. */
export async function getSession(): Promise<SessionInfo> {
  const s = (await request("GET", "/auth/session")) as {
    authenticated?: boolean;
    passkeyId?: string;
    reauthValidUntil?: string;
  };
  const info: SessionInfo = {
    signedIn: s.authenticated === true,
    reauthAt: s.reauthValidUntil ? new Date(s.reauthValidUntil) : undefined,
  };
  if (s.passkeyId) info.passkeyId = s.passkeyId;
  return info;
}

export type EnrollLink = { url: string; expiresAt: Date };

/**
 * POST /auth/enroll-link: a single-use link that enrolls another device, /enroll#<token>. It needs a recent step-up
 * (AuthError reauth_required otherwise). The token is in the returned url's fragment: keep it in memory only, never
 * in this page's address, the history or the console.
 */
export async function createEnrollLink(): Promise<EnrollLink> {
  const r = (await request("POST", "/auth/enroll-link")) as { url?: string; expiresAt?: string };
  if (!r.url || !r.expiresAt) throw new AuthError("internal", 200, "the server sent no link");
  return { url: r.url, expiresAt: new Date(r.expiresAt) };
}

/** Whether the browser can offer passkeys in a form field's autofill (conditional mediation). */
export async function conditionalAvailable(): Promise<boolean> {
  try {
    return (await globalThis.PublicKeyCredential?.isConditionalMediationAvailable?.()) === true;
  } catch {
    return false;
  }
}

async function ceremony(kind: "create" | "get", options: unknown, extra?: { conditional?: boolean; signal?: AbortSignal }) {
  const creds = navigator.credentials;
  if (!creds) throw new AuthError("unsupported", 0, "This browser cannot use passkeys here.");
  try {
    let cred: Credential | null;
    if (kind === "create") {
      cred = await creds.create({ ...creationOptionsFromJSON(options as never), signal: extra?.signal });
    } else {
      const req = requestOptionsFromJSON(options as never);
      if (extra?.conditional) req.mediation = "conditional";
      cred = await creds.get({ ...req, signal: extra?.signal });
    }
    if (!cred) throw new AuthError("cancelled");
    return credentialToJSON(cred as PublicKeyCredential);
  } catch (e) {
    if (e instanceof AuthError) throw e;
    // The owner dismissed the prompt, it timed out, or the page aborted it; anything else is the browser refusing.
    const dismissed = e instanceof DOMException && (e.name === "NotAllowedError" || e.name === "AbortError");
    throw new AuthError(dismissed ? "cancelled" : "failed", 0, e instanceof Error ? e.message : undefined);
  }
}

/** Signs in with a passkey: login begin, the browser's ceremony (inline in a field when conditional), login finish. */
export async function signIn(opts: { conditional?: boolean; signal?: AbortSignal } = {}): Promise<void> {
  const options = await request("POST", "/auth/login/begin");
  const cred = await ceremony("get", options, opts);
  await request("POST", "/auth/login/finish", cred);
}

/**
 * Enrolls this device with a single-use token and signs in. The token goes in the first request's body, nowhere
 * else: never in a URL, never logged. It is spent by that first request, so a failure anywhere after it needs a new
 * link and this never retries. If only the sign-in that follows fails, the passkey is saved and the error's code is
 * "enrolled".
 */
export async function enroll(token: string, deviceName: string): Promise<void> {
  const options = await request("POST", "/auth/register/begin", { token, name: deviceName });
  const cred = await ceremony("create", options);
  await request("POST", "/auth/register/finish", cred);
  try {
    await signIn();
  } catch (e) {
    // The passkey is saved; only the sign-in after it failed, which the sign-in page can do again.
    throw new AuthError("enrolled", e instanceof AuthError ? e.status : 0);
  }
}

/** Step-up: confirms the owner with the passkey again, which opens the window for sensitive commands. */
export async function reauth(): Promise<void> {
  const options = await request("POST", "/auth/reauth/begin");
  const cred = await ceremony("get", options);
  await request("POST", "/auth/reauth/finish", cred);
}

/**
 * Ends this session on the server, then forgets this device's queued commands and state. A logout the server did not
 * take throws and clears nothing: the owner is still signed in.
 */
export async function signOut(): Promise<void> {
  await request("POST", "/auth/logout");
  outboxRef?.clear();
  storeRef.getState().reset();
}
