// Package webauthntest is a software WebAuthn authenticator for tests: the web package's own and the server's.
package webauthntest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
)

var b64 = base64.RawURLEncoding

// SoftAuthenticator is a software WebAuthn authenticator: ECDSA P-256 (alg -7), "none" attestation, a discoverable
// credential, and user presence and user verification flags set on everything it signs.
type SoftAuthenticator struct {
	Origin string
	RPID   string

	key        *ecdsa.PrivateKey
	credID     []byte
	userHandle []byte
	signCount  uint32

	// NoUV clears the user-verification flag on everything the authenticator signs, as a key without a PIN or biometric.
	NoUV bool
}

// New makes an authenticator with a fresh key and credential id, speaking for origin and the relying party rpID.
func New(t testing.TB, origin, rpID string) *SoftAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	rand.Read(id)
	return &SoftAuthenticator{Origin: origin, RPID: rpID, key: key, credID: id}
}

// For returns an authenticator holding the same credential that signs client data for another origin.
func (a *SoftAuthenticator) For(origin string) *SoftAuthenticator {
	return &SoftAuthenticator{Origin: origin, RPID: a.RPID, key: a.key, credID: a.credID, userHandle: a.userHandle}
}

// UserHandle is the user handle the authenticator learned at Create; SetUserHandle replaces it (a stranger's
// assertion claiming the owner).
func (a *SoftAuthenticator) UserHandle() []byte { return a.userHandle }

func (a *SoftAuthenticator) SetUserHandle(h []byte) { a.userHandle = h }

// CredentialID is the id the passkey will have in the store.
func (a *SoftAuthenticator) CredentialID() string { return b64.EncodeToString(a.credID) }

// Create answers the options a register/begin response carries with the body register/finish takes.
func (a *SoftAuthenticator) Create(t testing.TB, options []byte) []byte {
	t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		t.Fatalf("creation options: %v: %s", err, options)
	}
	var err error
	if a.userHandle, err = b64.DecodeString(o.PublicKey.User.ID); err != nil {
		t.Fatal(err)
	}
	// authenticator data with attested credential data: rpIdHash, flags AT|UV|UP, counter, aaguid, id, COSE key
	rp := sha256.Sum256([]byte(a.RPID))
	ad := append(rp[:], a.flags(0x40), 0, 0, 0, 0)
	ad = append(ad, make([]byte, 16)...)
	ad = binary.BigEndian.AppendUint16(ad, uint16(len(a.credID)))
	ad = append(ad, a.credID...)
	ad = append(ad, a.coseKey()...)
	// attestation object {"fmt": "none", "attStmt": {}, "authData": ad}
	obj := []byte{0xa3}
	obj = append(obj, cborText("fmt")...)
	obj = append(obj, cborText("none")...)
	obj = append(obj, cborText("attStmt")...)
	obj = append(obj, 0xa0)
	obj = append(obj, cborText("authData")...)
	obj = append(obj, cborBytes(ad)...)
	return mustJSON(t, map[string]any{
		"id": a.CredentialID(), "rawId": a.CredentialID(), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(a.clientData(t, "webauthn.create", o.PublicKey.Challenge)),
			"attestationObject": b64.EncodeToString(obj),
		},
	})
}

// Assert answers the options a login/begin or reauth/begin response carries with the body of the matching finish.
func (a *SoftAuthenticator) Assert(t testing.TB, options []byte) []byte {
	t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		t.Fatalf("assertion options: %v: %s", err, options)
	}
	a.signCount++
	rp := sha256.Sum256([]byte(a.RPID))
	ad := append(rp[:], a.flags(0))
	ad = binary.BigEndian.AppendUint32(ad, a.signCount)
	cd := a.clientData(t, "webauthn.get", o.PublicKey.Challenge)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return mustJSON(t, map[string]any{
		"id": a.CredentialID(), "rawId": a.CredentialID(), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(cd),
			"authenticatorData": b64.EncodeToString(ad),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(a.userHandle),
		},
	})
}

// flags is UP plus UV (unless NoUV) plus extra.
func (a *SoftAuthenticator) flags(extra byte) byte {
	f := 0x01 | extra
	if !a.NoUV {
		f |= 0x04
	}
	return f
}

func (a *SoftAuthenticator) clientData(t testing.TB, typ, challenge string) []byte {
	return mustJSON(t, map[string]any{"type": typ, "challenge": challenge, "origin": a.Origin, "crossOrigin": false})
}

// coseKey is the credential public key as a COSE_Key: {1: 2 (EC2), 3: -7 (ES256), -1: 1 (P-256), -2: x, -3: y}.
func (a *SoftAuthenticator) coseKey() []byte {
	x := a.key.PublicKey.X.FillBytes(make([]byte, 32))
	y := a.key.PublicKey.Y.FillBytes(make([]byte, 32))
	k := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21}
	k = append(k, cborBytes(x)...)
	k = append(k, 0x22)
	return append(k, cborBytes(y)...)
}

// cborText and cborBytes encode short strings (under 256 bytes) as CBOR.
func cborText(s string) []byte  { return append(cborHead(0x60, len(s)), s...) }
func cborBytes(b []byte) []byte { return append(cborHead(0x40, len(b)), b...) }
func cborHead(major byte, n int) []byte {
	switch {
	case n < 24:
		return []byte{major | byte(n)}
	case n < 256:
		return []byte{major | 24, byte(n)}
	}
	return []byte{major | 25, byte(n >> 8), byte(n)}
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
