// Test doubles for the auth tests: a fetch that plays plan 3's /auth/* routes and a navigator.credentials that plays
// an authenticator. Test helper only; nothing in the app imports it.
import { vi } from "vitest";
import { b64uEncode } from "./webauthn";

export const CREATION = {
  publicKey: {
    rp: { name: "Athena", id: "athena.test" },
    user: { name: "owner", displayName: "Athena owner", id: "YXRoZW5hLW93bmVy" },
    challenge: "yK76pFVnpwUrg4YyguRjuFtus0XYyQf0TvCrvJQ1eAc",
    pubKeyCredParams: [{ type: "public-key", alg: -7 }],
    timeout: 300000,
    authenticatorSelection: { requireResidentKey: true, residentKey: "required", userVerification: "required" },
    attestation: "none",
  },
};
export const ASSERTION = {
  publicKey: {
    challenge: "dzsXO2IU62mHMIJA2LGWqV_Y2CXTCBnlBTpK-NIJhJY",
    timeout: 300000,
    rpId: "athena.test",
    userVerification: "required",
  },
};

export type Call = { method: string; path: string; body: unknown; credentials?: RequestCredentials };
export type Reply = { status?: number; json: unknown };
type Route = Reply | ((call: Call) => Reply);

/** Replaces global fetch with the routes ("POST /auth/login/begin" and so on); an unlisted route is a test failure. */
export function fakeFetch(routes: Record<string, Route>) {
  const calls: Call[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input);
    const method = init?.method ?? "GET";
    const call: Call = {
      method,
      path,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      credentials: init?.credentials,
    };
    calls.push(call);
    const route = routes[`${method} ${path}`];
    if (!route) throw new Error(`unexpected request ${method} ${path}`);
    const r = typeof route === "function" ? route(call) : route;
    return new Response(JSON.stringify(r.json), { status: r.status ?? 200, headers: { "Content-Type": "application/json" } });
  });
  vi.stubGlobal("fetch", fn);
  return { calls, fn };
}

const buf = (...n: number[]) => new Uint8Array(n).buffer;

/** A credential as the browser returns it: attestation (create) or assertion (get). */
export function fakeCredential(kind: "create" | "get") {
  return {
    id: "Y3JlZA",
    rawId: buf(0x63, 0x72, 0x65, 0x64),
    type: "public-key",
    response:
      kind === "create"
        ? { clientDataJSON: buf(1), attestationObject: buf(2), getTransports: () => ["internal"] }
        : { clientDataJSON: buf(1), authenticatorData: buf(2), signature: buf(3), userHandle: buf(4) },
    getClientExtensionResults: () => ({}),
  };
}

/** Replaces navigator.credentials; each create/get call is recorded with its options. */
export function fakeCredentials(impl: { create?: () => unknown; get?: () => unknown } = {}) {
  const create = vi.fn<(o?: CredentialCreationOptions) => Promise<unknown>>(async () => (impl.create ?? (() => fakeCredential("create")))());
  const get = vi.fn<(o?: CredentialRequestOptions) => Promise<unknown>>(async () => (impl.get ?? (() => fakeCredential("get")))());
  Object.defineProperty(navigator, "credentials", { value: { create, get }, configurable: true });
  return { create, get };
}

export const expectedAttestation = {
  id: "Y3JlZA",
  rawId: b64uEncode(buf(0x63, 0x72, 0x65, 0x64)),
};
