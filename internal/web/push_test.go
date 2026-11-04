package web

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/store"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := pushB64.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestEncryptRFC8291Vector checks Encrypt against the exact bytes of RFC 8291 Appendix A.
func TestEncryptRFC8291Vector(t *testing.T) {
	sub := Subscription{
		Endpoint: "https://push.example.net/push/JzLQ3raZJfFBR0aqvOMsLrt54w4rJUsV",
		P256dh:   "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
		Auth:     "BTBZMqHH6r4Tts7J_aSIgg",
	}
	serverKey, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	salt := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	got, err := Encrypt(sub, []byte("When I grow up, I want to be a watermelon"), salt, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if g := pushB64.EncodeToString(got); g != want {
		t.Fatalf("body\n got %s\nwant %s", g, want)
	}
}

func TestEncryptRejectsBadInput(t *testing.T) {
	key, _ := ecdh.P256().GenerateKey(rand.Reader)
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	sub := Subscription{P256dh: pushB64.EncodeToString(ua.PublicKey().Bytes()), Auth: pushB64.EncodeToString(make([]byte, 16))}
	if _, err := Encrypt(sub, []byte("x"), make([]byte, 8), key); err == nil {
		t.Error("a short salt was accepted")
	}
	if _, err := Encrypt(sub, make([]byte, 5000), make([]byte, 16), key); err == nil {
		t.Error("a message over one record was accepted")
	}
	sub.Auth = "AAAA"
	if _, err := Encrypt(sub, []byte("x"), make([]byte, 16), key); err == nil {
		t.Error("a short auth secret was accepted")
	}
}

func TestVAPIDKeyPersists(t *testing.T) {
	dir := t.TempDir()
	v, err := LoadOrCreateVAPID(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "push", "vapid.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	again, err := LoadOrCreateVAPID(dir)
	if err != nil || again.PublicKey() != v.PublicKey() {
		t.Fatalf("reloaded key differs: %v", err)
	}
	if got := mustB64(t, v.PublicKey()); len(got) != 65 || got[0] != 4 {
		t.Errorf("public key is not an uncompressed point: %x", got)
	}
	rec := httptest.NewRecorder()
	v.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/push/key", nil))
	if rec.Code != 200 || rec.Body.String() != v.PublicKey() {
		t.Errorf("/push/key: %d %q", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	v.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/push/key", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /push/key: %d", rec.Code)
	}
	if strings.Contains(v.PublicKey(), "\n") {
		t.Error("public key has a newline")
	}
}

func TestVAPIDHeaderVerifies(t *testing.T) {
	v, err := LoadOrCreateVAPID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	h := VAPIDHeader(v, "https://push.example.net:8443/send/abc", now)
	rest, ok := strings.CutPrefix(h, "vapid t=")
	if !ok {
		t.Fatalf("header %q", h)
	}
	jwt, k, ok := strings.Cut(rest, ", k=")
	if !ok || k != v.PublicKey() {
		t.Fatalf("header %q: key %q", h, k)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q", jwt)
	}
	var hdr struct{ Typ, Alg string }
	var claims struct {
		Aud string
		Exp int64
		Sub string
	}
	if err := json.Unmarshal(mustB64(t, parts[0]), &hdr); err != nil || hdr.Alg != "ES256" || hdr.Typ != "JWT" {
		t.Errorf("jwt header %+v %v", hdr, err)
	}
	if err := json.Unmarshal(mustB64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://push.example.net:8443" {
		t.Errorf("aud %q", claims.Aud)
	}
	if d := time.Unix(claims.Exp, 0).Sub(now); d <= 0 || d > 24*time.Hour {
		t.Errorf("exp is %v from now", d)
	}
	if !strings.HasPrefix(claims.Sub, "https://") || strings.Contains(claims.Sub, "@") {
		t.Errorf("sub %q", claims.Sub)
	}
	sig := mustB64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes", len(sig))
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&v.key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Error("signature does not verify")
	}
	v.Subject = "https://athena.example" // a configured subject replaces the endpoint's origin
	h = VAPIDHeader(v, "https://push.example.net/x", now)
	jwt = strings.Split(strings.TrimPrefix(h, "vapid t="), ", k=")[0]
	if err := json.Unmarshal(mustB64(t, strings.Split(jwt, ".")[1]), &claims); err != nil || claims.Sub != "https://athena.example" {
		t.Errorf("configured sub: %q %v", claims.Sub, err)
	}
}

// pushEnv is a store with one subscription pointing at a fake push service.
type pushEnv struct {
	st      *store.Store
	pusher  *Pusher
	ua      *ecdh.PrivateKey
	auth    []byte
	mu      sync.Mutex
	status  int
	got     []*http.Request
	bodies  [][]byte
	srv     *httptest.Server
	subName string
}

func newPushEnv(t *testing.T, status int) *pushEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, err := LoadOrCreateVAPID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &pushEnv{st: st, status: status}
	e.ua, _ = ecdh.P256().GenerateKey(rand.Reader)
	e.auth = make([]byte, 16)
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.got = append(e.got, r)
		e.bodies = append(e.bodies, b)
		code := e.status
		e.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(e.srv.Close)
	e.pusher = NewPusher(st, v, e.srv.Client(), nil)
	e.subName = e.srv.URL + "/sub/1"
	err = st.PutPushSubscription(store.PushSubscription{Endpoint: e.subName,
		P256dh: pushB64.EncodeToString(e.ua.PublicKey().Bytes()), Auth: pushB64.EncodeToString(e.auth), CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *pushEnv) setStatus(c int) { e.mu.Lock(); e.status = c; e.mu.Unlock() }

func jobPayload(urgency, body string) []byte {
	msg, _ := json.Marshal(map[string]string{"title": "Athena", "body": body})
	p, _ := json.Marshal(pushJob{Urgency: urgency, Message: msg})
	return p
}

func TestPushSendsHeadersAndEncryptedBody(t *testing.T) {
	e := newPushEnv(t, 201)
	if err := e.pusher.Handle(context.Background(), "k", jobPayload("high", "x needs approval")); err != nil {
		t.Fatal(err)
	}
	if len(e.got) != 1 {
		t.Fatalf("%d requests", len(e.got))
	}
	r := e.got[0]
	if r.Header.Get("Urgency") != "high" || r.Header.Get("TTL") != "86400" || r.Header.Get("Content-Encoding") != "aes128gcm" ||
		!strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") {
		t.Errorf("headers %v", r.Header)
	}
	// The browser's end: header is salt(16) rs(4) idlen(1) keyid, then the record.
	b := e.bodies[0]
	if len(b) < 86+17 || b[20] != 65 {
		t.Fatalf("body of %d bytes", len(b))
	}
	if strings.Contains(string(b), "approval") {
		t.Error("the message is not encrypted")
	}
}

func TestGoneSubscriptionDeleted(t *testing.T) {
	for _, code := range []int{404, 410} {
		e := newPushEnv(t, code)
		if err := e.pusher.Handle(context.Background(), "k", jobPayload("normal", "x")); err != nil {
			t.Fatalf("%d: %v", code, err)
		}
		subs, _ := e.st.PushSubscriptions()
		if len(subs) != 0 {
			t.Errorf("%d: subscription kept", code)
		}
	}
}

func TestPushRetriesOnOverloadAndKeepsSubscription(t *testing.T) {
	for _, code := range []int{429, 500, 503} {
		e := newPushEnv(t, code)
		if err := e.pusher.Handle(context.Background(), "k", jobPayload("normal", "x")); err == nil {
			t.Errorf("%d: no retry", code)
		}
		if subs, _ := e.st.PushSubscriptions(); len(subs) != 1 {
			t.Errorf("%d: subscription dropped", code)
		}
	}
}

func TestPushRefusalCountsThenDrops(t *testing.T) {
	e := newPushEnv(t, 403)
	for i := 0; i < maxPushFailures; i++ {
		if err := e.pusher.Handle(context.Background(), "k", jobPayload("normal", "x")); err != nil {
			t.Fatal(err)
		}
	}
	if subs, _ := e.st.PushSubscriptions(); len(subs) != 0 {
		t.Errorf("subscription kept after %d refusals", maxPushFailures)
	}
}
