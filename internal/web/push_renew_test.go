package web

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielpina1/athena/internal/store"
)

// renewBody is what the service worker posts: the old endpoint and the new PushSubscription.toJSON().
func renewBody(t *testing.T, old, endpoint string) []byte {
	t.Helper()
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"oldEndpoint": old, "subscription": map[string]any{"endpoint": endpoint,
		"keys": map[string]string{"p256dh": pushB64.EncodeToString(ua.PublicKey().Bytes()), "auth": pushB64.EncodeToString(make([]byte, 16))}}})
	return b
}

func endpoints(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	subs, err := st.PushSubscriptions()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range subs {
		out[s.Endpoint] = string(s.SessionHash)
	}
	return out
}

// The service worker moves a subscription its session holds to the push service's new endpoint, with the cookie and
// no step-up; it cannot add one the session does not hold, and needs the session and the Origin.
func TestRenewPushMovesOnlyTheSessionsOwnSubscription(t *testing.T) {
	e, a := signedIn(t)
	e.Mux.HandleFunc("POST /push/renew", e.Auth.RenewPush)
	session := func() []byte {
		t.Helper()
		hash, ok := hashOf(e.cookies[SessionCookie].Value)
		if !ok {
			t.Fatal("no session cookie")
		}
		return hash
	}
	put := func(endpoint string, session []byte) {
		t.Helper()
		if err := e.Store.PutPushSubscription(store.PushSubscription{SessionHash: session, Endpoint: endpoint, P256dh: "k", Auth: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	// Another device's session holds https://push.test/theirs; this one holds https://push.test/old.
	put("https://push.test/theirs", session())
	theirs := session()
	e.Login(a)
	hash := session()
	put("https://push.test/old", hash)

	// Past the step-up window: a background worker cannot ask for the passkey.
	e.Advance(ReauthWindow + 1)
	if rec := e.DoFrom("https://evil.test", "POST", "/push/renew", renewBody(t, "https://push.test/old", "https://push.test/new")); rec.Code != http.StatusForbidden {
		t.Errorf("another origin: %d %s", rec.Code, rec.Body)
	}
	if rec := e.Do("POST", "/push/renew", renewBody(t, "https://push.test/old", "http://push.test/new")); rec.Code != http.StatusBadRequest {
		t.Errorf("a plain http endpoint: %d %s", rec.Code, rec.Body)
	}
	if rec := e.Do("POST", "/push/renew", renewBody(t, "https://push.test/unknown", "https://push.test/new")); rec.Code != http.StatusNotFound {
		t.Errorf("an endpoint the session does not hold: %d %s", rec.Code, rec.Body)
	}
	if rec := e.Do("POST", "/push/renew", renewBody(t, "https://push.test/theirs", "https://push.test/new")); rec.Code != http.StatusNotFound {
		t.Errorf("another session's endpoint: %d %s", rec.Code, rec.Body)
	}
	if got := endpoints(t, e.Store); len(got) != 2 || got["https://push.test/old"] != string(hash) || got["https://push.test/theirs"] != string(theirs) {
		t.Fatalf("refused renewals changed the subscriptions: %v", got)
	}
	rec := e.Do("POST", "/push/renew", renewBody(t, "https://push.test/old", "https://push.test/new"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("renew: %d %s", rec.Code, rec.Body)
	}
	if got := endpoints(t, e.Store); len(got) != 2 || got["https://push.test/new"] != string(hash) || got["https://push.test/theirs"] != string(theirs) {
		t.Fatalf("after the renewal: %v", got)
	}

	// Without the session, nothing.
	e.DropCookies()
	if rec := e.Do("POST", "/push/renew", renewBody(t, "https://push.test/new", "https://push.test/newer")); rec.Code != http.StatusUnauthorized {
		t.Errorf("without a session: %d %s", rec.Code, rec.Body)
	}
}
