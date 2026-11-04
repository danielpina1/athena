package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web/webauthntest"
)

// This file holds what tests of the login need and later tasks' tests reuse (the WebSocket and server tests log in
// through it): a software authenticator and an Auth on a temporary store with a steerable clock.

const (
	testOrigin = "https://athena.test"
	testRPID   = "athena.test"
)

var b64 = base64.RawURLEncoding

// SoftAuthenticator is the software authenticator of webauthntest; server tests log in with it too.
type SoftAuthenticator = webauthntest.SoftAuthenticator

func NewSoftAuthenticator(t testing.TB, origin, rpID string) *SoftAuthenticator {
	return webauthntest.New(t, origin, rpID)
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
