package web

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/danielpina1/athena/internal/store"
)

// Cookie names, lifetimes and limits of the owner's login.
const (
	SessionCookie  = "athena_session"
	CeremonyCookie = "athena_ceremony"

	// ReauthWindow is how long a passkey assertion vouches for sensitive commands.
	ReauthWindow = 5 * time.Minute
	// DefaultEnrollTTL is how long an enrollment token is good when the caller names no other.
	DefaultEnrollTTL = 15 * time.Minute

	ceremonyTTL = 5 * time.Minute
	// maxCeremonies bounds the unfinished ceremonies held in memory; begin is open to anyone who can reach the port.
	maxCeremonies = 256
	// touchEvery is how stale a session's seen_at gets before a request writes it again; connTouchEvery is the same
	// for an open connection's periodic check.
	touchEvery     = time.Minute
	connTouchEvery = 10 * time.Minute
	maxBody        = 64 << 10
	maxNameLen     = 64
)

// ownerHandle is the WebAuthn user handle of the one owner; every passkey carries it.
var ownerHandle = []byte("athena-owner")

// Session is a logged-in browser as the rest of the server sees it. Hash identifies it (hex of the SHA-256 of its
// cookie value) and is what OnRevoke callbacks receive.
type Session struct {
	Hash     string
	ReauthAt time.Time
	// PasskeyID is the passkey the session signed in with.
	PasskeyID string
}

// Auth is the owner's passkey login: enrollment, sessions and re-authentication. Its handlers are plain
// http.HandlerFuncs; Mount puts them on a mux.
type Auth struct {
	st     *store.Store
	wa     *webauthn.WebAuthn
	origin string
	secure bool
	now    func() time.Time

	mu         sync.Mutex // serializes session touches and reauths, so one cannot overwrite the other
	ceremonies map[string]*ceremony
	revokeFns  []func(sessionHashes []string)

	// afterLoginVerified, when set (tests), runs between the verification of a login and the opening of its session.
	afterLoginVerified func()
}

type ceremony struct {
	kind      string // "register", "login" or "reauth"
	data      webauthn.SessionData
	name      string // register: the name of the new passkey
	sessionID string // reauth: the session it belongs to
	expires   time.Time
}

// NewAuth makes the login for the configured origin (https://<host>[:<port>], or http:// on a loopback host when TLS is
// off) and relying party id (the host). now is the clock; nil means time.Now.
func NewAuth(st *store.Store, origin, rpID string, now func() time.Time) (*Auth, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("web: origin %q is not scheme://host[:port]", origin)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("web: origin %q: plain http is only for a loopback host", origin)
		}
	default:
		return nil, fmt.Errorf("web: origin %q: scheme must be https", origin)
	}
	if now == nil {
		now = time.Now
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  rpID,
		RPDisplayName:         "Athena",
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("web: webauthn: %w", err)
	}
	return &Auth{st: st, wa: wa, origin: origin, secure: u.Scheme == "https", now: now, ceremonies: map[string]*ceremony{}}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Mount registers the handlers on mux.
func (a *Auth) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/register/begin", a.RegisterBegin)
	mux.HandleFunc("POST /auth/register/finish", a.RegisterFinish)
	mux.HandleFunc("POST /auth/login/begin", a.LoginBegin)
	mux.HandleFunc("POST /auth/login/finish", a.LoginFinish)
	mux.HandleFunc("POST /auth/reauth/begin", a.ReauthBegin)
	mux.HandleFunc("POST /auth/reauth/finish", a.ReauthFinish)
	mux.HandleFunc("POST /auth/logout", a.Logout)
	mux.HandleFunc("GET /auth/session", a.SessionInfo)
	mux.HandleFunc("POST /auth/enroll-link", a.EnrollLink)
}

// OnRevoke registers f to be called with the hashes (Session.Hash) of the sessions that end because a passkey was
// revoked (RevokePasskey), the browser logged out (Logout) or a lookup found them expired, so the web server can close
// their open WebSockets. f runs
// on the revoking goroutine and must not block. Register before serving.
func (a *Auth) OnRevoke(f func(sessionHashes []string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revokeFns = append(a.revokeFns, f)
}

func (a *Auth) revoked(hashes []string) {
	if len(hashes) == 0 {
		return
	}
	a.mu.Lock()
	fns := slices.Clone(a.revokeFns)
	a.mu.Unlock()
	for _, f := range fns {
		f(hashes)
	}
}

// RevokePasskey deletes a passkey and every session it opened, reports the ended sessions to the OnRevoke callbacks
// and returns how many ended. A passkey that does not exist is not an error.
func (a *Auth) RevokePasskey(id string) (ended int, err error) {
	sessions, err := a.st.DeletePasskey(id)
	if err != nil {
		return 0, err
	}
	hashes := make([]string, len(sessions))
	for i, h := range sessions {
		hashes[i] = hex.EncodeToString(h)
	}
	a.revoked(hashes)
	return len(hashes), nil
}

// --- enrollment tokens ---

// IssueEnrollToken makes a single-use enrollment token good for ttl (DefaultEnrollTTL if ttl <= 0): 32 random bytes,
// base64url. Only its SHA-256 is stored. The token must reach the owner out of band and never be logged.
func IssueEnrollToken(st *store.Store, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = DefaultEnrollTTL
	}
	token, hash, err := newSecret()
	if err != nil {
		return "", err
	}
	if err := st.PutEnrollToken(hash, time.Now().Add(ttl)); err != nil {
		return "", err
	}
	slog.Info("web: enrollment token issued", "ttl", ttl.String())
	return token, nil
}

// newSecret returns 32 random bytes as base64url and their SHA-256.
func newSecret() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("web: random: %w", err)
	}
	h := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(b), h[:], nil
}

// hashOf is the SHA-256 of the raw bytes a base64url value encodes; ok is false for a value that is not 32 such bytes.
func hashOf(value string) ([]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(b) != 32 {
		return nil, false
	}
	h := sha256.Sum256(b)
	return h[:], true
}

// --- the owner as a webauthn.User ---

type owner struct{ creds []webauthn.Credential }

func (owner) WebAuthnID() []byte                           { return ownerHandle }
func (owner) WebAuthnName() string                         { return "owner" }
func (owner) WebAuthnDisplayName() string                  { return "Athena owner" }
func (o owner) WebAuthnCredentials() []webauthn.Credential { return o.creds }

func credentialOf(p store.Passkey) (webauthn.Credential, error) {
	var c webauthn.Credential
	if err := json.Unmarshal(p.Credential, &c); err != nil {
		return c, fmt.Errorf("web: passkey %s: %w", p.ID, err)
	}
	return c, nil
}

func passkeyID(credentialID []byte) string { return base64.RawURLEncoding.EncodeToString(credentialID) }

// --- ceremonies ---

// putCeremony stores the ceremony and sets its cookie; false means it answered with an error already.
func (a *Auth) putCeremony(w http.ResponseWriter, c *ceremony) bool {
	id, _, err := newSecret()
	if err != nil {
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return false
	}
	now := a.now()
	c.expires = now.Add(ceremonyTTL)
	a.mu.Lock()
	for k, v := range a.ceremonies {
		if !v.expires.After(now) {
			delete(a.ceremonies, k)
		}
	}
	for len(a.ceremonies) >= maxCeremonies { // still full of live ones: the oldest goes
		var oldest string
		for k, v := range a.ceremonies {
			if oldest == "" || v.expires.Before(a.ceremonies[oldest].expires) {
				oldest = k
			}
		}
		delete(a.ceremonies, oldest)
	}
	a.ceremonies[id] = c
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: CeremonyCookie, Value: id, Path: "/auth", MaxAge: int(ceremonyTTL.Seconds()),
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
	return true
}

// takeCeremony removes and returns the ceremony the request's cookie names, if it is of the given kind and unexpired.
// A ceremony is good for one finish, whatever its outcome.
func (a *Auth) takeCeremony(w http.ResponseWriter, r *http.Request, kind string) (*ceremony, bool) {
	ck, err := r.Cookie(CeremonyCookie)
	if err == nil {
		a.mu.Lock()
		c, ok := a.ceremonies[ck.Value]
		delete(a.ceremonies, ck.Value)
		a.mu.Unlock()
		if ok && c.kind == kind && c.expires.After(a.now()) {
			http.SetCookie(w, &http.Cookie{Name: CeremonyCookie, Path: "/auth", MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
			return c, true
		}
	}
	fail(w, http.StatusBadRequest, "invalid", "no such ceremony")
	return nil, false
}

// --- registration ---

// cleanName makes a passkey name fit to store and print: control characters dropped, at most maxNameLen characters.
func cleanName(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > maxNameLen {
		s = strings.TrimSpace(string(r[:maxNameLen]))
	}
	return s
}

// RegisterBegin starts enrolling a passkey. The body is {"token": ..., "name": ...}; the token is spent here, so a
// ceremony that fails needs a new token.
func (a *Auth) RegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !a.checkOrigin(w, r) {
		return
	}
	var req struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	hash, ok := hashOf(req.Token)
	if !ok {
		fail(w, http.StatusUnauthorized, "unauthenticated", "invalid or expired enrollment token")
		return
	}
	used, err := a.st.UseEnrollToken(hash, a.now())
	if err != nil {
		slog.Error("web: use enrollment token", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !used {
		fail(w, http.StatusUnauthorized, "unauthenticated", "invalid or expired enrollment token")
		return
	}
	name := cleanName(req.Name)
	if name == "" {
		name = "Passkey"
	}
	existing, err := a.st.Passkeys()
	if err != nil {
		slog.Error("web: passkeys", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	var have []webauthn.Credential
	for _, p := range existing {
		if cr, err := credentialOf(p); err == nil {
			have = append(have, cr)
		}
	}
	creation, session, err := a.wa.BeginRegistration(owner{}, webauthn.WithExclusions(webauthn.Credentials(have).CredentialDescriptors()))
	if err != nil {
		slog.Error("web: begin registration", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !a.putCeremony(w, &ceremony{kind: "register", data: *session, name: name}) {
		return
	}
	writeJSON(w, http.StatusOK, creation)
}

// RegisterFinish completes the enrollment: the body is the credential the browser created.
func (a *Auth) RegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !a.checkOrigin(w, r) {
		return
	}
	c, ok := a.takeCeremony(w, r, "register")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	cred, err := a.wa.FinishRegistration(owner{}, c.data, r)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid", "registration refused")
		return
	}
	blob, err := json.Marshal(cred)
	if err != nil {
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	id := passkeyID(cred.ID)
	_, exists, err := a.st.Passkey(id)
	if err != nil {
		slog.Error("web: passkey", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if exists {
		fail(w, http.StatusConflict, "conflict", "passkey already enrolled")
		return
	}
	if err := a.st.PutPasskey(store.Passkey{ID: id, Name: c.name, Credential: blob, CreatedAt: a.now()}); err != nil {
		slog.Error("web: put passkey", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "name": c.name})
}

// --- login and reauth ---

// LoginBegin starts a discoverable-credential login.
func (a *Auth) LoginBegin(w http.ResponseWriter, r *http.Request) {
	if !a.checkOrigin(w, r) {
		return
	}
	assertion, session, err := a.wa.BeginDiscoverableLogin()
	if err != nil {
		slog.Error("web: begin login", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !a.putCeremony(w, &ceremony{kind: "login", data: *session}) {
		return
	}
	writeJSON(w, http.StatusOK, assertion)
}

// LoginFinish verifies the assertion and opens a session: it sets the session cookie, and the assertion counts as a
// re-authentication, so the sensitive window opens at sign-in.
func (a *Auth) LoginFinish(w http.ResponseWriter, r *http.Request) {
	if !a.checkOrigin(w, r) {
		return
	}
	c, ok := a.takeCeremony(w, r, "login")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var loaded store.Passkey
	cred, err := a.wa.FinishDiscoverableLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
		if !bytes.Equal(userHandle, ownerHandle) {
			return nil, errors.New("unknown user")
		}
		p, found, err := a.st.Passkey(passkeyID(rawID))
		if err != nil || !found {
			return nil, errors.New("unknown credential")
		}
		cr, err := credentialOf(p)
		if err != nil {
			return nil, err
		}
		loaded = p
		return owner{creds: []webauthn.Credential{cr}}, nil
	}, c.data, r)
	if err != nil || cred.Authenticator.CloneWarning {
		fail(w, http.StatusUnauthorized, "unauthenticated", "login refused")
		return
	}
	if a.afterLoginVerified != nil {
		a.afterLoginVerified()
	}
	now := a.now()
	if err := a.touchPasskey(loaded.ID, now, cred); err != nil {
		slog.Error("web: touch passkey", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	token, hash, err := newSecret()
	if err != nil {
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := a.st.PutWebSession(store.WebSession{Hash: hash, CreatedAt: now, SeenAt: now, ReauthAt: now, PasskeyID: loaded.ID}); err != nil {
		// The passkey was revoked after the assertion verified: the store refuses a session without one.
		if _, still, perr := a.st.Passkey(loaded.ID); perr == nil && !still {
			fail(w, http.StatusUnauthorized, "unauthenticated", "login refused")
			return
		}
		slog.Error("web: put session", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	http.SetCookie(w, a.sessionCookie(token, int(store.WebSessionAbsolute.Seconds())))
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "reauthValidUntil": now.Add(ReauthWindow)})
}

func (a *Auth) touchPasskey(id string, at time.Time, cred *webauthn.Credential) error {
	blob, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	return a.st.TouchPasskey(id, at, blob)
}

// ReauthBegin starts a fresh assertion by the passkey that opened the caller's session.
func (a *Auth) ReauthBegin(w http.ResponseWriter, r *http.Request) {
	if !a.checkOrigin(w, r) {
		return
	}
	s, ws, ok := a.session(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "unauthenticated", "not signed in")
		return
	}
	user, err := a.userOf(ws.PasskeyID)
	if err != nil {
		fail(w, http.StatusUnauthorized, "unauthenticated", "not signed in")
		return
	}
	assertion, session, err := a.wa.BeginLogin(user)
	if err != nil {
		slog.Error("web: begin reauth", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !a.putCeremony(w, &ceremony{kind: "reauth", data: *session, sessionID: s.Hash}) {
		return
	}
	writeJSON(w, http.StatusOK, assertion)
}

// ReauthFinish verifies the assertion and sets the session's reauth_at to now.
func (a *Auth) ReauthFinish(w http.ResponseWriter, r *http.Request) {
	if !a.checkOrigin(w, r) {
		return
	}
	s, ws, ok := a.session(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "unauthenticated", "not signed in")
		return
	}
	c, ok := a.takeCeremony(w, r, "reauth")
	if !ok {
		return
	}
	if c.sessionID != s.Hash {
		fail(w, http.StatusBadRequest, "invalid", "no such ceremony")
		return
	}
	user, err := a.userOf(ws.PasskeyID)
	if err != nil {
		fail(w, http.StatusUnauthorized, "unauthenticated", "not signed in")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	cred, err := a.wa.FinishLogin(user, c.data, r)
	if err != nil || cred.Authenticator.CloneWarning {
		fail(w, http.StatusUnauthorized, "unauthenticated", "reauthentication refused")
		return
	}
	now := a.now()
	if err := a.touchPasskey(ws.PasskeyID, now, cred); err != nil {
		slog.Error("web: touch passkey", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.mu.Lock()
	err = a.st.TouchWebSession(ws.Hash, now, now)
	a.mu.Unlock()
	if err != nil {
		slog.Error("web: touch session", "err", err)
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "reauthValidUntil": now.Add(ReauthWindow)})
}

func (a *Auth) userOf(id string) (owner, error) {
	p, found, err := a.st.Passkey(id)
	if err != nil || !found {
		return owner{}, errors.New("no passkey")
	}
	cr, err := credentialOf(p)
	if err != nil {
		return owner{}, err
	}
	return owner{creds: []webauthn.Credential{cr}}, nil
}

// --- sessions ---

func (a *Auth) sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: SessionCookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode}
}

// Session returns the session the request's cookie names, if it is live: unseen less than 30 days and created less
// than 90 days ago. Its passkey is still enrolled: the store deletes a passkey's sessions with it and refuses new ones. A live session's seen time is refreshed.
func (a *Auth) Session(r *http.Request) (Session, bool) {
	s, _, ok := a.session(r)
	return s, ok
}

func (a *Auth) session(r *http.Request) (Session, store.WebSession, bool) {
	return a.sessionTouched(r, touchEvery)
}

// connected is Session for an open connection's periodic check: the connection counts as the browser being seen, so
// a device left open does not idle out, but its seen time is written at most every connTouchEvery. The absolute
// lifetime still ends it.
func (a *Auth) connected(r *http.Request) bool {
	_, _, ok := a.sessionTouched(r, connTouchEvery)
	return ok
}

// sessionTouched looks the request's session up, refreshing its seen time when it is touchAfter old. An expired one
// is deleted, and the OnRevoke callbacks hear of it, so its connections close.
func (a *Auth) sessionTouched(r *http.Request, touchAfter time.Duration) (Session, store.WebSession, bool) {
	ck, err := r.Cookie(SessionCookie)
	if err != nil {
		return Session{}, store.WebSession{}, false
	}
	hash, ok := hashOf(ck.Value)
	if !ok {
		return Session{}, store.WebSession{}, false
	}
	a.mu.Lock()
	ws, found, err := a.st.WebSession(hash)
	if err != nil || !found {
		a.mu.Unlock()
		return Session{}, store.WebSession{}, false
	}
	now := a.now()
	if now.Sub(ws.SeenAt) >= store.WebSessionIdle || now.Sub(ws.CreatedAt) >= store.WebSessionAbsolute {
		err := a.st.DeleteWebSession(hash)
		a.mu.Unlock()
		if err == nil {
			a.revoked([]string{hex.EncodeToString(hash)})
		}
		return Session{}, store.WebSession{}, false
	}
	if now.Sub(ws.SeenAt) >= touchAfter {
		if err := a.st.TouchWebSession(hash, now, ws.ReauthAt); err == nil {
			ws.SeenAt = now
		}
	}
	a.mu.Unlock()
	return Session{Hash: hex.EncodeToString(hash), ReauthAt: ws.ReauthAt, PasskeyID: ws.PasskeyID}, ws, true
}

// RecentReauth reports whether a passkey assertion vouched for the session within the last 5 minutes.
func (a *Auth) RecentReauth(s Session) bool {
	return !s.ReauthAt.IsZero() && a.now().Sub(s.ReauthAt) < ReauthWindow
}

// Logout ends the caller's session and clears the cookie.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if !a.checkOrigin(w, r) {
		return
	}
	if s, ws, ok := a.session(r); ok {
		if err := a.st.DeleteWebSession(ws.Hash); err != nil {
			slog.Error("web: delete session", "err", err)
			fail(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		a.revoked([]string{s.Hash})
	}
	http.SetCookie(w, a.sessionCookie("", -1))
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

// SessionInfo answers GET /auth/session with {authenticated, passkeyId, reauthValidUntil}; passkeyId is the passkey the
// session signed in with (so a device list can mark the current one), reauthValidUntil is present only while the
// re-authentication window is open.
func (a *Auth) SessionInfo(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Authenticated    bool       `json:"authenticated"`
		PasskeyID        string     `json:"passkeyId,omitempty"`
		ReauthValidUntil *time.Time `json:"reauthValidUntil,omitempty"`
	}{}
	if s, ok := a.Session(r); ok {
		out.Authenticated = true
		out.PasskeyID = s.PasskeyID
		if a.RecentReauth(s) {
			until := s.ReauthAt.Add(ReauthWindow)
			out.ReauthValidUntil = &until
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// --- plumbing ---

// checkOrigin refuses, with 403, a request whose Origin is not exactly the configured one.
func (a *Auth) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") != a.origin {
		fail(w, http.StatusForbidden, "forbidden", "origin not allowed")
		return false
	}
	return true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err == nil {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid", "invalid request")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "error": msg})
}
