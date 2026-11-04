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

type linkAnswer struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expiresAt"`
	Code      string `json:"code"`
}

func postLink(t *testing.T, e *AuthEnv, body string) (int, linkAnswer, http.Header) {
	t.Helper()
	var raw []byte
	if body != "" {
		raw = []byte(body)
	}
	rec := e.Do("POST", "/auth/enroll-link", raw)
	var got linkAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("answer %q is not JSON: %v", rec.Body, err)
	}
	return rec.Code, got, rec.Header()
}

func signedIn(t *testing.T) (*AuthEnv, *SoftAuthenticator) {
	t.Helper()
	e := NewAuthEnv(t)
	a := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(a, "laptop")
	e.Login(a)
	return e, a
}

func TestEnrollLinkNeedsReauth(t *testing.T) {
	e, a := signedIn(t)
	e.Advance(ReauthWindow + time.Second)
	code, got, _ := postLink(t, e, `{"ttlSeconds":60}`)
	if code != http.StatusForbidden || got.Code != "reauth_required" || got.URL != "" {
		t.Fatalf("without a recent assertion: %d %+v", code, got)
	}
	e.Reauth(a)
	if code, _, _ := postLink(t, e, `{"ttlSeconds":60}`); code != http.StatusOK {
		t.Fatalf("after reauth: %d", code)
	}
	// No session at all is not a reauth question.
	e.DropCookies()
	if code, got, _ := postLink(t, e, ""); code != http.StatusUnauthorized || got.URL != "" {
		t.Fatalf("without a session: %d %+v", code, got)
	}
}

func TestEnrollLinkIssuesUsableToken(t *testing.T) {
	e, _ := signedIn(t)
	before := time.Now()
	code, got, hdr := postLink(t, e, `{"ttlSeconds":600}`)
	if code != http.StatusOK || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("answer %d, Cache-Control %q", code, hdr.Get("Cache-Control"))
	}
	token, ok := strings.CutPrefix(got.URL, testOrigin+"/enroll#")
	if !ok || token == "" || strings.ContainsAny(token, "?&/ ") {
		t.Fatalf("url = %q, want %s/enroll#<token>", got.URL, testOrigin)
	}
	exp, err := time.Parse(time.RFC3339, got.ExpiresAt)
	if err != nil || exp.Before(before.Add(599*time.Second)) || exp.After(time.Now().Add(601*time.Second)) {
		t.Fatalf("expiresAt = %q (%v), want about ten minutes from now", got.ExpiresAt, err)
	}

	// A new device registers a passkey with the fragment's token, once.
	phone := NewSoftAuthenticator(t, testOrigin, testRPID)
	e.DropCookies()
	e.RegisterWith(phone, token, "phone", http.StatusOK)
	if _, found, _ := e.Store.Passkey(phone.CredentialID()); !found {
		t.Fatal("the link's token did not enroll the passkey")
	}
	if rec := e.Do("POST", "/auth/register/begin", mustJSON(t, map[string]string{"token": token, "name": "x"})); rec.Code == http.StatusOK {
		t.Fatal("the token worked a second time")
	}
}

func TestEnrollLinkDefaultTTL(t *testing.T) {
	e, _ := signedIn(t)
	before := time.Now()
	code, got, _ := postLink(t, e, "")
	exp, err := time.Parse(time.RFC3339, got.ExpiresAt)
	if code != http.StatusOK || err != nil || exp.Before(before.Add(DefaultEnrollTTL-time.Second)) || exp.After(time.Now().Add(DefaultEnrollTTL+time.Second)) {
		t.Fatalf("no body: %d %+v (%v)", code, got, err)
	}
	if code, _, _ := postLink(t, e, `{}`); code != http.StatusOK {
		t.Fatalf("empty object: %d", code)
	}
	if code, _, _ := postLink(t, e, `{"ttlSeconds":86400}`); code != http.StatusOK {
		t.Fatalf("a day: %d", code)
	}
	if code, _, _ := postLink(t, e, `{"ttlSeconds":1}`); code != http.StatusOK {
		t.Fatalf("one second: %d", code)
	}
}

func TestEnrollLinkRejectsBadOriginAndTTL(t *testing.T) {
	e, _ := signedIn(t)
	for _, origin := range []string{"", "https://evil.test", testOrigin + "/", "http://" + strings.TrimPrefix(testOrigin, "https://")} {
		rec := e.DoFrom(origin, "POST", "/auth/enroll-link", []byte(`{"ttlSeconds":60}`))
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "/enroll#") {
			t.Errorf("origin %q: %d %s", origin, rec.Code, rec.Body)
		}
	}
	for _, body := range []string{`{"ttlSeconds":0}`, `{"ttlSeconds":-5}`, `{"ttlSeconds":86401}`, `{"ttlSeconds":"60"}`, `{"ttlSeconds":1.5}`, `not json`} {
		if code, got, _ := postLink(t, e, body); code != http.StatusBadRequest || got.URL != "" {
			t.Errorf("body %s: %d %+v, want 400", body, code, got)
		}
	}
	if rec := e.Do("GET", "/auth/enroll-link", nil); rec.Code == http.StatusOK {
		t.Errorf("GET issued a link")
	}
}

func TestEnrollLinkNotLogged(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	e, _ := signedIn(t)
	_, got, _ := postLink(t, e, `{"ttlSeconds":60}`)
	token := strings.TrimPrefix(got.URL, testOrigin+"/enroll#")
	postLink(t, e, `{"ttlSeconds":0}`)
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(buf.String(), "enrollment token issued") || token == "" {
		t.Fatalf("nothing logged for the issue (%q); the check proves nothing", buf.String())
	}
	if strings.Contains(buf.String(), token) || strings.Contains(buf.String(), "/enroll#") {
		t.Fatalf("enrollment token in the log: %s", buf.String())
	}
}
