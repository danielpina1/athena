import { expect, test } from "vitest";
import { b64uDecode, b64uEncode, credentialToJSON, creationOptionsFromJSON, requestOptionsFromJSON } from "./webauthn";

// The three fixtures are what plan 3's server (go-webauthn) answered to /auth/register/begin, /auth/login/begin and
// /auth/reauth/begin in internal/web's tests: the real shapes, not ones this file made up.
const creationJSON = {
  publicKey: {
    rp: { name: "Athena", id: "athena.test" },
    user: { name: "owner", displayName: "Athena owner", id: "YXRoZW5hLW93bmVy" },
    challenge: "yK76pFVnpwUrg4YyguRjuFtus0XYyQf0TvCrvJQ1eAc",
    pubKeyCredParams: [
      { type: "public-key", alg: -7 },
      { type: "public-key", alg: -257 },
    ],
    timeout: 300000,
    excludeCredentials: [{ type: "public-key", id: "venxtM50IX_sX2dMcp1_QFgQHb7tg_w7iCquC6tl5w8" }],
    authenticatorSelection: { requireResidentKey: true, residentKey: "required", userVerification: "required" },
    attestation: "none",
  },
};
const loginJSON = {
  publicKey: {
    challenge: "dzsXO2IU62mHMIJA2LGWqV_Y2CXTCBnlBTpK-NIJhJY",
    timeout: 300000,
    rpId: "athena.test",
    userVerification: "required",
  },
};
const reauthJSON = {
  publicKey: {
    challenge: "HL31oihcrY4nxIB-rnqw4HBW6UPflMnvViRU_Fwq5fU",
    timeout: 300000,
    rpId: "athena.test",
    allowCredentials: [{ type: "public-key", id: "venxtM50IX_sX2dMcp1_QFgQHb7tg_w7iCquC6tl5w8" }],
    userVerification: "required",
  },
};

const bytes = (b: BufferSource) => [...new Uint8Array(b instanceof ArrayBuffer ? b : (b as ArrayBufferView).buffer)];
const buf = (...n: number[]) => new Uint8Array(n).buffer;

test("webauthn converts creation and request options both ways", () => {
  // base64url, with and without padding needs, and the characters that differ from base64.
  expect(bytes(b64uDecode("YXRoZW5hLW93bmVy"))).toEqual([...new TextEncoder().encode("athena-owner")]);
  expect(bytes(b64uDecode("-_8"))).toEqual([0xfb, 0xff]);
  expect(b64uEncode(buf(0xfb, 0xff))).toBe("-_8");
  expect(b64uEncode(new Uint8Array([1, 2, 3, 4]))).toBe("AQIDBA");
  expect(b64uEncode(new ArrayBuffer(0))).toBe("");
  for (const s of [creationJSON.publicKey.challenge, loginJSON.publicKey.challenge, "YQ", "YWI", "YWJj"]) {
    expect(b64uEncode(b64uDecode(s))).toBe(s);
  }
  expect(() => b64uDecode("a*b")).toThrow();

  // Creation options: the three binary fields become buffers, everything else passes through unchanged.
  const creation = creationOptionsFromJSON(creationJSON).publicKey!;
  expect(bytes(creation.challenge)).toEqual(bytes(b64uDecode(creationJSON.publicKey.challenge)));
  expect(new TextDecoder().decode(creation.user.id as ArrayBuffer)).toBe("athena-owner");
  expect(creation.user.name).toBe("owner");
  expect(creation.user.displayName).toBe("Athena owner");
  expect(creation.rp).toEqual({ name: "Athena", id: "athena.test" });
  expect(creation.pubKeyCredParams).toEqual(creationJSON.publicKey.pubKeyCredParams);
  expect(creation.authenticatorSelection).toEqual(creationJSON.publicKey.authenticatorSelection);
  expect(creation.attestation).toBe("none");
  expect(creation.timeout).toBe(300000);
  expect(creation.excludeCredentials).toHaveLength(1);
  expect(creation.excludeCredentials![0].type).toBe("public-key");
  expect(b64uEncode(creation.excludeCredentials![0].id)).toBe(creationJSON.publicKey.excludeCredentials[0].id);

  // Request options: the challenge, and the allowed credentials when there are some.
  const login = requestOptionsFromJSON(loginJSON).publicKey!;
  expect(b64uEncode(login.challenge)).toBe(loginJSON.publicKey.challenge);
  expect(login.rpId).toBe("athena.test");
  expect(login.userVerification).toBe("required");
  expect(login.allowCredentials).toBeUndefined();
  const reauth = requestOptionsFromJSON(reauthJSON).publicKey!;
  expect(reauth.allowCredentials).toHaveLength(1);
  expect(b64uEncode(reauth.allowCredentials![0].id)).toBe(reauthJSON.publicKey.allowCredentials[0].id);

  // The credential goes back as the JSON the server's FinishRegistration / FinishLogin read.
  const created = {
    id: "Y3JlZA",
    rawId: buf(0x63, 0x72, 0x65, 0x64),
    type: "public-key",
    authenticatorAttachment: "platform",
    response: {
      clientDataJSON: buf(1, 2),
      attestationObject: buf(3, 4, 5),
      getTransports: () => ["internal", "hybrid"],
    },
    getClientExtensionResults: () => ({}),
  };
  expect(credentialToJSON(created as never)).toEqual({
    id: "Y3JlZA",
    rawId: "Y3JlZA",
    type: "public-key",
    authenticatorAttachment: "platform",
    clientExtensionResults: {},
    response: { clientDataJSON: "AQI", attestationObject: "AwQF", transports: ["internal", "hybrid"] },
  });
  const asserted = {
    id: "Y3JlZA",
    rawId: buf(0x63, 0x72, 0x65, 0x64),
    type: "public-key",
    response: { clientDataJSON: buf(1), authenticatorData: buf(2), signature: buf(3), userHandle: buf(4) },
    getClientExtensionResults: () => ({}),
  };
  expect(credentialToJSON(asserted as never).response).toEqual({
    clientDataJSON: "AQ",
    authenticatorData: "Ag",
    signature: "Aw",
    userHandle: "BA",
  });
  // A null userHandle (a non-discoverable credential) is left out, not sent as "".
  const noHandle = { ...asserted, response: { ...asserted.response, userHandle: null } };
  expect("userHandle" in (credentialToJSON(noHandle as never).response as object)).toBe(false);
});
