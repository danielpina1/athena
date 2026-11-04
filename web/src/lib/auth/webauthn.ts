// WebAuthn's wire conversions, by hand (no library): the server (go-webauthn) speaks JSON with base64url strings
// where the browser API wants ArrayBuffers, so options are decoded on the way in and credentials encoded on the way
// out. Only the fields the server's options carry as binary are touched; the rest passes through unchanged.

const ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
const LOOKUP: Record<string, number> = Object.fromEntries([...ALPHABET].map((c, i) => [c, i]));

/** base64url (RFC 4648 §5, no padding) of buf. */
export function b64uEncode(buf: ArrayBuffer | ArrayBufferView): string {
  const b = buf instanceof ArrayBuffer ? new Uint8Array(buf) : new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength);
  let out = "";
  for (let i = 0; i < b.length; i += 3) {
    const n = (b[i] << 16) | ((b[i + 1] ?? 0) << 8) | (b[i + 2] ?? 0);
    out += ALPHABET[(n >> 18) & 63] + ALPHABET[(n >> 12) & 63];
    if (i + 1 < b.length) out += ALPHABET[(n >> 6) & 63];
    if (i + 2 < b.length) out += ALPHABET[n & 63];
  }
  return out;
}

/** The bytes of a base64url string (padding tolerated); throws on any other character or an impossible length. */
export function b64uDecode(s: string): ArrayBuffer {
  const t = s.replace(/=+$/, "");
  if (t.length % 4 === 1) throw new Error("webauthn: bad base64url length");
  const out = new Uint8Array(Math.floor((t.length * 3) / 4));
  let acc = 0;
  let bits = 0;
  let o = 0;
  for (const c of t) {
    const v = LOOKUP[c];
    if (v === undefined) throw new Error("webauthn: bad base64url character");
    acc = (acc << 6) | v;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[o++] = (acc >> bits) & 255;
      acc &= (1 << bits) - 1;
    }
  }
  return out.buffer;
}

type DescriptorJSON = { id: string; type: string; transports?: string[] };

// What go-webauthn sends: the browser's own option names, with the binary fields as base64url strings. The rest is
// passed on as it came, so the types only name what is converted.
type CreationJSON = {
  publicKey: {
    challenge: string;
    user: { id: string; [k: string]: unknown };
    excludeCredentials?: DescriptorJSON[];
    [k: string]: unknown;
  };
};

type RequestJSON = {
  mediation?: CredentialMediationRequirement;
  publicKey: { challenge: string; allowCredentials?: DescriptorJSON[]; [k: string]: unknown };
};

const descriptor = (d: DescriptorJSON) => ({ ...d, id: b64uDecode(d.id) }) as PublicKeyCredentialDescriptor;

/** The server's /auth/register/begin answer as the argument of navigator.credentials.create. */
export function creationOptionsFromJSON(json: CreationJSON): CredentialCreationOptions {
  const { challenge, user, excludeCredentials, ...rest } = json.publicKey;
  const publicKey = {
    ...rest,
    challenge: b64uDecode(challenge),
    user: { ...user, id: b64uDecode(user.id) },
  } as unknown as PublicKeyCredentialCreationOptions;
  if (excludeCredentials?.length) publicKey.excludeCredentials = excludeCredentials.map(descriptor);
  return { publicKey };
}

/** The server's /auth/login/begin or /auth/reauth/begin answer as the argument of navigator.credentials.get. */
export function requestOptionsFromJSON(json: RequestJSON): CredentialRequestOptions {
  const { challenge, allowCredentials, ...rest } = json.publicKey;
  const publicKey = { ...rest, challenge: b64uDecode(challenge) } as unknown as PublicKeyCredentialRequestOptions;
  if (allowCredentials?.length) publicKey.allowCredentials = allowCredentials.map(descriptor);
  return json.mediation ? { publicKey, mediation: json.mediation } : { publicKey };
}

/** What register/finish and login/finish or reauth/finish read: the credential with every buffer as base64url. */
export function credentialToJSON(cred: PublicKeyCredential): Record<string, unknown> {
  const r = cred.response as AuthenticatorResponse &
    Partial<AuthenticatorAttestationResponse & AuthenticatorAssertionResponse>;
  const response: Record<string, unknown> = { clientDataJSON: b64uEncode(r.clientDataJSON) };
  if (r.attestationObject) {
    response.attestationObject = b64uEncode(r.attestationObject);
    const transports = r.getTransports?.();
    if (transports?.length) response.transports = transports;
  }
  if (r.authenticatorData) response.authenticatorData = b64uEncode(r.authenticatorData);
  if (r.signature) response.signature = b64uEncode(r.signature);
  if (r.userHandle) response.userHandle = b64uEncode(r.userHandle);
  const out: Record<string, unknown> = {
    id: cred.id,
    rawId: b64uEncode(cred.rawId),
    type: cred.type,
    clientExtensionResults: cred.getClientExtensionResults?.() ?? {},
    response,
  };
  if (cred.authenticatorAttachment) out.authenticatorAttachment = cred.authenticatorAttachment;
  return out;
}
