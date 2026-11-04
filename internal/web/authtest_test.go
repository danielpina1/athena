package web

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/store"
)

// This file holds what tests of the login need and later tasks' tests reuse (the WebSocket and server tests log in
// through it): a software authenticator and an Auth on a temporary store with a steerable clock.

const (
	testOrigin = "https://athena.test"
	testRPID   = "athena.test"
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

func NewSoftAuthenticator(t testing.TB, origin, rpID string) *SoftAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	rand.Read(id)
	return &SoftAuthenticator{Origin: origin, RPID: rpID, key: key, credID: id}
}

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

// AuthEnv is an Auth on a temporary store, the clock it reads, and a tiny HTTP client that carries cookies by hand.
type AuthEnv struct {
	T     testing.TB
	Store *store.Store
	Auth  *Auth
	Mux   *http.ServeMux

	mu      sync.Mutex
	clock   time.Time
	cookies map[string]*http.Cookie
}

func NewAuthEnv(t testing.TB) *AuthEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &AuthEnv{T: t, Store: st, clock: time.Now().UTC().Truncate(time.Second), cookies: map[string]*http.Cookie{}}
	e.Auth, err = NewAuth(st, testOrigin, testRPID, func() time.Time {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.clock
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Mux = http.NewServeMux()
	e.Auth.Mount(e.Mux)
	return e
}

// Now is the test clock; Advance moves it.
func (e *AuthEnv) Now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.clock
}

func (e *AuthEnv) Advance(d time.Duration) {
	e.mu.Lock()
	e.clock = e.clock.Add(d)
	e.mu.Unlock()
}

// Do sends a request with Origin set to the right origin and the client's cookies, and keeps the cookies it sets.
func (e *AuthEnv) Do(method, path string, body []byte) *httptest.ResponseRecorder {
	return e.DoFrom(testOrigin, method, path, body)
}

func (e *AuthEnv) DoFrom(origin, method, path string, body []byte) *httptest.ResponseRecorder {
	e.T.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Origin", origin)
	for _, c := range e.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.Mux.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(e.cookies, c.Name)
		} else {
			e.cookies[c.Name] = c
		}
	}
	return rec
}

// DropCookies forgets every cookie, as a new browser would.
func (e *AuthEnv) DropCookies() { e.cookies = map[string]*http.Cookie{} }

// Enroll issues a token and registers the authenticator with it.
func (e *AuthEnv) Enroll(a *SoftAuthenticator, name string) {
	e.T.Helper()
	token, err := IssueEnrollToken(e.Store, 0)
	if err != nil {
		e.T.Fatal(err)
	}
	e.RegisterWith(a, token, name, http.StatusOK)
}

// RegisterWith runs both register steps with the given token, expecting the finish to answer wantStatus (the begin
// must answer 200 when wantStatus is 200).
func (e *AuthEnv) RegisterWith(a *SoftAuthenticator, token, name string, wantStatus int) {
	e.T.Helper()
	begin := e.Do("POST", "/auth/register/begin", mustJSON(e.T, map[string]string{"token": token, "name": name}))
	if begin.Code != http.StatusOK {
		e.T.Fatalf("register/begin = %d %s", begin.Code, begin.Body)
	}
	fin := e.Do("POST", "/auth/register/finish", a.Create(e.T, begin.Body.Bytes()))
	if fin.Code != wantStatus {
		e.T.Fatalf("register/finish = %d %s", fin.Code, fin.Body)
	}
}

// Login signs in with the authenticator and returns the session cookie value.
func (e *AuthEnv) Login(a *SoftAuthenticator) string {
	e.T.Helper()
	begin := e.Do("POST", "/auth/login/begin", nil)
	if begin.Code != http.StatusOK {
		e.T.Fatalf("login/begin = %d %s", begin.Code, begin.Body)
	}
	fin := e.Do("POST", "/auth/login/finish", a.Assert(e.T, begin.Body.Bytes()))
	if fin.Code != http.StatusOK {
		e.T.Fatalf("login/finish = %d %s", fin.Code, fin.Body)
	}
	return e.cookies[SessionCookie].Value
}

// Reauth runs both reauth steps for the signed-in client.
func (e *AuthEnv) Reauth(a *SoftAuthenticator) {
	e.T.Helper()
	begin := e.Do("POST", "/auth/reauth/begin", nil)
	if begin.Code != http.StatusOK {
		e.T.Fatalf("reauth/begin = %d %s", begin.Code, begin.Body)
	}
	if fin := e.Do("POST", "/auth/reauth/finish", a.Assert(e.T, begin.Body.Bytes())); fin.Code != http.StatusOK {
		e.T.Fatalf("reauth/finish = %d %s", fin.Code, fin.Body)
	}
}

// Request is a bare GET carrying the client's cookies, for Auth.Session.
func (e *AuthEnv) Request() *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range e.cookies {
		req.AddCookie(c)
	}
	return req
}
