package web

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnrollAndLogin(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "phone")

	list, err := e.Store.Passkeys()
	if err != nil || len(list) != 1 || list[0].Name != "phone" || list[0].ID != a.CredentialID() {
		t.Fatalf("passkeys = %+v %v", list, err)
	}

	e.DropCookies()
	if _, ok := e.Auth.Session(e.Request()); ok {
		t.Fatal("session before login")
	}
	begin := e.Do("POST", "/auth/login/begin", nil)
	var ceremonyCookie *http.Cookie
	for _, c := range begin.Result().Cookies() {
		if c.Name == CeremonyCookie {
			ceremonyCookie = c
		}
	}
	if ceremonyCookie == nil || !ceremonyCookie.HttpOnly || !ceremonyCookie.Secure || ceremonyCookie.SameSite != http.SameSiteStrictMode || ceremonyCookie.MaxAge != 300 {
		t.Fatalf("ceremony cookie = %+v", ceremonyCookie)
	}
	if strings.Contains(begin.Body.String(), ceremonyCookie.Value) {
		t.Fatal("ceremony id in the body")
	}
	fin := e.Do("POST", "/auth/login/finish", a.Assert(t, begin.Body.Bytes()))
	if fin.Code != http.StatusOK {
		t.Fatalf("login/finish = %d %s", fin.Code, fin.Body)
	}
	var sc *http.Cookie
	for _, c := range fin.Result().Cookies() {
		if c.Name == SessionCookie {
			sc = c
		}
	}
	if sc == nil || !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteStrictMode || sc.Path != "/" {
		t.Fatalf("session cookie = %+v", sc)
	}
	if raw, err := b64.DecodeString(sc.Value); err != nil || len(raw) != 32 {
		t.Fatalf("cookie value is not 32 base64url bytes: %q", sc.Value)
	}
	s, ok := e.Auth.Session(e.Request())
	if !ok || !e.Auth.RecentReauth(s) {
		t.Fatalf("session after login = %+v %v: login must open the reauth window", s, ok)
	}
	// Only the hash of the cookie is stored.
	if hash, _ := hashOf(sc.Value); s.Hash == sc.Value || len(s.Hash) != 64 {
		t.Fatalf("hash %q (hash of cookie %x)", s.Hash, hash)
	}
	p, _, _ := e.Store.Passkey(a.CredentialID())
	if p.LastUsedAt.IsZero() {
		t.Fatal("login did not record the use of the passkey")
	}

	info := e.Do("GET", "/auth/session", nil)
	var got struct {
		Authenticated    bool      `json:"authenticated"`
		ReauthValidUntil time.Time `json:"reauthValidUntil"`
	}
	if err := json.Unmarshal(info.Body.Bytes(), &got); err != nil || !got.Authenticated || !got.ReauthValidUntil.Equal(e.Now().Add(5*time.Minute)) {
		t.Fatalf("session info = %s", info.Body)
	}
	e.Do("POST", "/auth/logout", nil)
	if _, ok := e.Auth.Session(e.Request()); ok {
		t.Fatal("session after logout")
	}
}

func TestPlainHTTPCookieOnLoopback(t *testing.T) {
	e := NewAuthEnv(t)
	if _, err := NewAuth(e.Store, "http://athena.test", "athena.test", nil); err == nil {
		t.Fatal("plain http accepted for a non-loopback host")
	}
	a, err := NewAuth(e.Store, "http://localhost:8080", "localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.sessionCookie("x", 1).Secure {
		t.Fatal("Secure cookie with TLS off")
	}
}

func TestEnrollTokenSingleUseAndExpiry(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	token, err := IssueEnrollToken(e.Store, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.RegisterWith(a, token, "one", http.StatusOK)
	// Spent: a second use is refused, and nothing is enrolled.
	rec := e.Do("POST", "/auth/register/begin", mustJSON(t, map[string]string{"token": token}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused token: %d", rec.Code)
	}

	// Expired.
	token, _ = IssueEnrollToken(e.Store, time.Minute)
	e.Advance(2 * time.Minute)
	if rec := e.Do("POST", "/auth/register/begin", mustJSON(t, map[string]string{"token": token})); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", rec.Code)
	}
	// Unknown, malformed and missing tokens.
	for _, tok := range []string{"", "nope", b64.EncodeToString(make([]byte, 32))} {
		if rec := e.Do("POST", "/auth/register/begin", mustJSON(t, map[string]string{"token": tok})); rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", tok, rec.Code)
		}
	}
	if list, _ := e.Store.Passkeys(); len(list) != 1 {
		t.Fatalf("%d passkeys, want 1", len(list))
	}
}

func TestSessionAloneCannotEnrollAnotherPasskey(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "phone")
	e.Login(a)
	if rec := e.Do("POST", "/auth/register/begin", []byte(`{"name":"laptop"}`)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("register/begin with a session and no token = %d", rec.Code)
	}
	// With a token a second passkey enrolls.
	b := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(b, "laptop")
	if list, _ := e.Store.Passkeys(); len(list) != 2 {
		t.Fatalf("%d passkeys, want 2", len(list))
	}
}

func TestLoginWrongOriginRefused(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "phone")
	begin := e.Do("POST", "/auth/login/begin", nil)
	body := a.Assert(t, begin.Body.Bytes())
	for _, origin := range []string{"https://evil.test", "", "http://athena.test", testOrigin + "/"} {
		if rec := e.DoFrom(origin, "POST", "/auth/login/finish", body); rec.Code != http.StatusForbidden {
			t.Fatalf("origin %q: %d", origin, rec.Code)
		}
		if rec := e.DoFrom(origin, "POST", "/auth/login/begin", nil); rec.Code != http.StatusForbidden {
			t.Fatalf("begin, origin %q: %d", origin, rec.Code)
		}
	}
	// A client-data origin other than the configured one is refused by the verification even with the right header.
	b := NewSoftAuthenticator(t, "https://evil.test", testRPID)
	b.key, b.credID, b.userHandle = a.key, a.credID, a.userHandle
	begin = e.Do("POST", "/auth/login/begin", nil)
	if rec := e.Do("POST", "/auth/login/finish", b.Assert(t, begin.Body.Bytes())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("assertion for another origin: %d", rec.Code)
	}
	var n int
	for _, c := range e.cookies {
		if c.Name == SessionCookie {
			n++
		}
	}
	if n != 0 {
		t.Fatal("a session cookie was set")
	}
	// No session row: sweep the table through the store.
	if cnt := countSessions(t, e); cnt != 0 {
		t.Fatalf("%d session rows", cnt)
	}
}

func countSessions(t *testing.T, e *AuthEnv) int {
	t.Helper()
	list, err := e.Store.Passkeys()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range list {
		ws, err := e.Store.WebSessionsOf(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		n += len(ws)
	}
	return n
}

func TestCeremonyIsOneShotAndBoundToItsKind(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "phone")
	begin := e.Do("POST", "/auth/login/begin", nil)
	body := a.Assert(t, begin.Body.Bytes())
	cookie := e.cookies[CeremonyCookie]
	if rec := e.Do("POST", "/auth/register/finish", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("login ceremony used to register: %d", rec.Code)
	}
	e.cookies[CeremonyCookie] = cookie // the failed finish consumed it
	// Replay and expiry: a login ceremony goes stale after 5 minutes.
	e.Advance(6 * time.Minute)
	if rec := e.Do("POST", "/auth/login/finish", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("expired ceremony: %d", rec.Code)
	}
	begin = e.Do("POST", "/auth/login/begin", nil)
	body = a.Assert(t, begin.Body.Bytes())
	if rec := e.Do("POST", "/auth/login/finish", body); rec.Code != http.StatusOK {
		t.Fatalf("login: %d", rec.Code)
	}
	e.cookies[CeremonyCookie] = cookie
	if rec := e.Do("POST", "/auth/login/finish", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed ceremony: %d", rec.Code)
	}
}

func TestRevokedPasskeyKillsSessions(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	b := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "phone")
	e.Enroll(b, "laptop")
	e.Login(a)
	s, _ := e.Auth.Session(e.Request())
	cookieA := e.cookies[SessionCookie]
	e.DropCookies()
	e.Login(b)
	if _, ok := e.Auth.Session(e.Request()); !ok {
		t.Fatal("laptop session")
	}

	var mu sync.Mutex
	var got []string
	e.Auth.OnRevoke(func(h []string) { mu.Lock(); got = append(got, h...); mu.Unlock() })
	if err := e.Auth.RevokePasskey(a.CredentialID()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != s.Hash {
		t.Fatalf("OnRevoke got %v, want [%s]", got, s.Hash)
	}
	e.DropCookies()
	e.cookies[SessionCookie] = cookieA
	if _, ok := e.Auth.Session(e.Request()); ok {
		t.Fatal("session of a revoked passkey survived")
	}
	e.DropCookies()
	// The other passkey's session is untouched, and the revoked passkey cannot log in.
	e.Login(b)
	if _, ok := e.Auth.Session(e.Request()); !ok {
		t.Fatal("laptop login after revoke")
	}
	begin := e.Do("POST", "/auth/login/begin", nil)
	if rec := e.Do("POST", "/auth/login/finish", a.Assert(t, begin.Body.Bytes())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked passkey logged in: %d", rec.Code)
	}

	// Logout reports its session too.
	got = nil
	e.Do("POST", "/auth/logout", nil)
	if len(got) != 1 {
		t.Fatalf("OnRevoke after logout got %v", got)
	}
}

func TestSessionIdleAndAbsoluteExpiry(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "phone")

	// Idle: unseen for 30 days.
	e.Login(a)
	e.Advance(30*24*time.Hour - time.Minute)
	if _, ok := e.Auth.Session(e.Request()); !ok {
		t.Fatal("session dead before 30 days idle")
	}
	e.Advance(30*24*time.Hour - time.Minute) // use keeps it alive: seen again just now
	if _, ok := e.Auth.Session(e.Request()); !ok {
		t.Fatal("a session in use died of idleness")
	}
	e.Advance(30*24*time.Hour + time.Minute)
	if _, ok := e.Auth.Session(e.Request()); ok {
		t.Fatal("session alive after 30 days idle")
	}

	// Absolute: in use every day, dead at 90 days.
	e.DropCookies()
	e.Login(a)
	for i := 0; i < 89; i++ {
		e.Advance(24 * time.Hour)
		if _, ok := e.Auth.Session(e.Request()); !ok {
			t.Fatalf("session dead on day %d", i+1)
		}
	}
	e.Advance(24*time.Hour + time.Minute)
	if _, ok := e.Auth.Session(e.Request()); ok {
		t.Fatal("session alive after 90 days")
	}
	if n := countSessions(t, e); n != 0 {
		t.Fatalf("%d expired session rows left", n)
	}
}

func TestReauthWindow(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "phone")
	e.Login(a)

	e.Advance(4*time.Minute + 59*time.Second)
	s, _ := e.Auth.Session(e.Request())
	if !e.Auth.RecentReauth(s) {
		t.Fatal("not recent at 4m59s")
	}
	e.Advance(2 * time.Second)
	s, _ = e.Auth.Session(e.Request())
	if e.Auth.RecentReauth(s) {
		t.Fatal("recent at 5m01s")
	}
	if got := e.Do("GET", "/auth/session", nil).Body.String(); strings.Contains(got, "reauthValidUntil") {
		t.Fatalf("session info after the window: %s", got)
	}

	e.Reauth(a)
	s, _ = e.Auth.Session(e.Request())
	if !e.Auth.RecentReauth(s) || !s.ReauthAt.Equal(e.Now()) {
		t.Fatalf("after reauth: %+v at %v", s, e.Now())
	}
	// A reauth by another passkey's assertion, or none, does not count.
	other := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Advance(10 * time.Minute)
	begin := e.Do("POST", "/auth/reauth/begin", nil)
	other.userHandle = a.userHandle
	if rec := e.Do("POST", "/auth/reauth/finish", other.Assert(t, begin.Body.Bytes())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reauth with a stranger's assertion: %d", rec.Code)
	}
	s, _ = e.Auth.Session(e.Request())
	if e.Auth.RecentReauth(s) {
		t.Fatal("reauth window opened by a stranger")
	}
	// Reauth needs a session.
	e.DropCookies()
	if rec := e.Do("POST", "/auth/reauth/begin", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reauth/begin without a session: %d", rec.Code)
	}
}

func TestEnrollTokenNotLogged(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	token, err := IssueEnrollToken(e.Store, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.RegisterWith(a, token, "phone", http.StatusOK)
	e.Do("POST", "/auth/register/begin", mustJSON(t, map[string]string{"token": token})) // reused: refused
	mu.Lock()
	defer mu.Unlock()
	if buf.Len() == 0 {
		t.Fatal("nothing was logged; the check proves nothing")
	}
	if strings.Contains(buf.String(), token) {
		t.Fatalf("enrollment token in the log: %s", buf.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestRevokeBetweenVerificationAndSessionLeavesNoSession(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "phone")
	e.Auth.afterLoginVerified = func() {
		if err := e.Auth.RevokePasskey(a.CredentialID()); err != nil {
			t.Error(err)
		}
	}
	begin := e.Do("POST", "/auth/login/begin", nil)
	rec := e.Do("POST", "/auth/login/finish", a.Assert(t, begin.Body.Bytes()))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login finishing as its passkey is revoked: %d %s", rec.Code, rec.Body)
	}
	if _, ok := e.Auth.Session(e.Request()); ok {
		t.Fatal("usable session for a revoked passkey")
	}
	// Pruning far in the future deletes whatever rows exist; none may.
	if n, err := e.Store.PruneWebSessions(e.Now().Add(1000 * 24 * time.Hour)); err != nil || n != 0 {
		t.Fatalf("%d session rows, %v", n, err)
	}
}

func TestRevokeReportsEverySessionItDeleted(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "phone")
	var hashes []string
	for i := 0; i < 3; i++ {
		e.DropCookies()
		e.Login(a)
		s, _ := e.Auth.Session(e.Request())
		hashes = append(hashes, s.Hash)
	}
	var got []string
	e.Auth.OnRevoke(func(h []string) { got = append(got, h...) })
	if err := e.Auth.RevokePasskey(a.CredentialID()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("reported %v, want the three sessions %v", got, hashes)
	}
}

func TestNoUserVerificationRefused(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "phone")

	a.NoUV = true
	begin := e.Do("POST", "/auth/login/begin", nil)
	if rec := e.Do("POST", "/auth/login/finish", a.Assert(t, begin.Body.Bytes())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("login without UV: %d", rec.Code)
	}
	a.NoUV = false
	e.Login(a)
	a.NoUV = true
	e.Advance(10 * time.Minute)
	begin = e.Do("POST", "/auth/reauth/begin", nil)
	if rec := e.Do("POST", "/auth/reauth/finish", a.Assert(t, begin.Body.Bytes())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reauth without UV: %d", rec.Code)
	}
	if s, _ := e.Auth.Session(e.Request()); e.Auth.RecentReauth(s) {
		t.Fatal("reauth window opened without UV")
	}
	token, _ := IssueEnrollToken(e.Store, 0)
	b := NewSoftAuthenticator(t, testOrigin, testRPID)
	b.NoUV = true
	e.RegisterWith(b, token, "weak", http.StatusBadRequest)
	if list, _ := e.Store.Passkeys(); len(list) != 1 {
		t.Fatalf("%d passkeys, want 1", len(list))
	}
}

func TestWrongOriginRegisterLeavesTokenUsable(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	token, _ := IssueEnrollToken(e.Store, 0)
	body := mustJSON(t, map[string]string{"token": token})
	if rec := e.DoFrom("https://evil.test", "POST", "/auth/register/begin", body); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong origin: %d", rec.Code)
	}
	e.RegisterWith(a, token, "phone", http.StatusOK)
}

func TestRegisterExcludesEnrolledCredentialsAndCleansName(t *testing.T) {
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "\x1b[31mphone\x07"+strings.Repeat("é", 100))
	list, _ := e.Store.Passkeys()
	if want := "[31mphone" + strings.Repeat("é", 64-len("[31mphone")); list[0].Name != want {
		t.Fatalf("name %q, want %q", list[0].Name, want)
	}
	token, _ := IssueEnrollToken(e.Store, 0)
	begin := e.Do("POST", "/auth/register/begin", mustJSON(t, map[string]string{"token": token}))
	var o struct {
		PublicKey struct {
			Exclude []struct {
				ID string `json:"id"`
			} `json:"excludeCredentials"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(begin.Body.Bytes(), &o); err != nil || len(o.PublicKey.Exclude) != 1 || o.PublicKey.Exclude[0].ID != a.CredentialID() {
		t.Fatalf("excludeCredentials = %+v %v", o, err)
	}
}
