package web

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/effects"
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
	v, err := LoadOrCreateVAPID(dir, testSubject)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "push", "vapid.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	again, err := LoadOrCreateVAPID(dir, testSubject)
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
	v, err := LoadOrCreateVAPID(t.TempDir(), testSubject)
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
	if claims.Sub != testSubject {
		t.Errorf("sub %q, want %q", claims.Sub, testSubject)
	}
	sig := mustB64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes", len(sig))
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&v.key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Error("signature does not verify")
	}
	if _, err := LoadOrCreateVAPID(t.TempDir(), "mailto:me@example.com"); err == nil {
		t.Error("an email subject was accepted")
	}
	if _, err := LoadOrCreateVAPID(t.TempDir(), ""); err == nil {
		t.Error("an empty subject was accepted")
	}
	if h := VAPIDHeader(&VAPID{key: v.key}, "https://push.example.net/x", now); h != "" {
		t.Error("a header was made without a subject")
	}
}

const testSubject = "https://athena.example"

// pushEnv is a store with subscriptions pointing at fake push services.
type pushEnv struct {
	t     *testing.T
	st    *store.Store
	vapid *VAPID
	stage func(string) (core.Stage, error)
	vis   func() bool
	now   time.Time
	svcs  []*fakeService
	keys  []*ecdh.PrivateKey
	auths [][]byte
}

// fakeService is a push service answering a settable status and keeping what it received.
type fakeService struct {
	mu     sync.Mutex
	status int
	retry  string
	reqs   []*http.Request
	bodies [][]byte
	srv    *httptest.Server
}

func (f *fakeService) set(code int) { f.mu.Lock(); f.status = code; f.mu.Unlock() }
func (f *fakeService) count() int   { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reqs) }

func newPushEnv(t *testing.T, statuses ...int) *pushEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, err := LoadOrCreateVAPID(t.TempDir(), testSubject)
	if err != nil {
		t.Fatal(err)
	}
	hash := seedSession(t, st)
	e := &pushEnv{t: t, st: st, vapid: v, now: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	for i, code := range statuses {
		f := &fakeService{status: code}
		f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.reqs = append(f.reqs, r)
			f.bodies = append(f.bodies, b)
			code, retry := f.status, f.retry
			f.mu.Unlock()
			if retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			w.WriteHeader(code)
		}))
		t.Cleanup(f.srv.Close)
		ua, _ := ecdh.P256().GenerateKey(rand.Reader)
		auth := make([]byte, 16)
		auth[0] = byte(i + 1)
		err := st.PutPushSubscription(store.PushSubscription{SessionHash: hash, Endpoint: f.srv.URL + "/sub/1",
			P256dh: pushB64.EncodeToString(ua.PublicKey().Bytes()), Auth: pushB64.EncodeToString(auth), CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		e.svcs, e.keys, e.auths = append(e.svcs, f), append(e.keys, ua), append(e.auths, auth)
	}
	return e
}

func (e *pushEnv) pusher() *Pusher {
	return NewPusher(PusherConfig{Store: e.st, VAPID: e.vapid, Client: http.DefaultClient, Stage: e.stage, Visible: e.vis,
		Now: func() time.Time { return e.now }})
}

// job is the payload of a push to service i for a Ticket waiting on an answer, enqueued at e.now.
func (e *pushEnv) job(i int, urgency, body string) []byte {
	msg, _ := json.Marshal(map[string]string{"title": "Athena", "body": body})
	p, _ := json.Marshal(pushJob{Endpoint: e.svcs[i].srv.URL + "/sub/1", Urgency: urgency, Ticket: "T-1", Action: "answer",
		At: e.now.Unix(), Message: msg})
	return p
}

// decrypt is the browser's end of RFC 8291.
func decrypt(t *testing.T, ua *ecdh.PrivateKey, auth, body []byte) []byte {
	t.Helper()
	salt, idlen := body[:16], int(body[20])
	asRaw := body[21 : 21+idlen]
	asPub, err := ecdh.P256().NewPublicKey(asRaw)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := ua.ECDH(asPub)
	prkKey, _ := hkdf.Extract(sha256.New, shared, auth)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(ua.PublicKey().Bytes())+string(asRaw), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	blk, _ := aes.NewCipher(cek)
	g, _ := cipher.NewGCM(blk)
	plain, err := g.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain[len(plain)-1] != 0x02 {
		t.Fatalf("padding delimiter %x", plain[len(plain)-1])
	}
	return plain[:len(plain)-1]
}

func TestPushSendsHeadersAndEncryptedBody(t *testing.T) {
	e := newPushEnv(t, 201)
	if err := e.pusher().Handle(context.Background(), "k", e.job(0, "high", "x needs approval")); err != nil {
		t.Fatal(err)
	}
	f := e.svcs[0]
	if f.count() != 1 {
		t.Fatalf("%d requests", f.count())
	}
	r := f.reqs[0]
	if r.Header.Get("Urgency") != "high" || r.Header.Get("TTL") != "86400" || r.Header.Get("Content-Encoding") != "aes128gcm" ||
		!strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") {
		t.Errorf("headers %v", r.Header)
	}
	var msg map[string]string
	if err := json.Unmarshal(decrypt(t, e.keys[0], e.auths[0], f.bodies[0]), &msg); err != nil || msg["body"] != "x needs approval" || msg["title"] != "Athena" {
		t.Errorf("browser reads %v %v", msg, err)
	}
}

func TestGoneSubscriptionDeleted(t *testing.T) {
	for _, code := range []int{404, 410} {
		e := newPushEnv(t, code)
		if err := e.pusher().Handle(context.Background(), "k", e.job(0, "normal", "x")); err != nil {
			t.Fatalf("%d: %v", code, err)
		}
		if subs, _ := e.st.PushSubscriptions(); len(subs) != 0 {
			t.Errorf("%d: subscription kept", code)
		}
	}
}

func TestPushRetriesOnOverloadAndKeepsSubscription(t *testing.T) {
	for _, code := range []int{429, 500, 503} {
		e := newPushEnv(t, code)
		if err := e.pusher().Handle(context.Background(), "k", e.job(0, "normal", "x")); err == nil {
			t.Errorf("%d: no retry", code)
		}
		if subs, _ := e.st.PushSubscriptions(); len(subs) != 1 {
			t.Errorf("%d: subscription dropped", code)
		}
	}
}

func TestPushRetryAfterIsLater(t *testing.T) {
	e := newPushEnv(t, 429)
	e.svcs[0].retry = "42"
	err := e.pusher().Handle(context.Background(), "k", e.job(0, "normal", "x"))
	if !errors.Is(err, effects.ErrLater) {
		t.Fatalf("429 with Retry-After: %v", err)
	}
	e.svcs[0].retry = "99999"
	if err := e.pusher().Handle(context.Background(), "k", e.job(0, "normal", "x")); !errors.Is(err, effects.ErrLater) {
		t.Fatalf("long Retry-After: %v", err)
	}
}

func TestPushRefusalCountsThenDrops(t *testing.T) {
	e := newPushEnv(t, 403)
	for i := 0; i < maxPushFailures; i++ {
		if err := e.pusher().Handle(context.Background(), "k", e.job(0, "normal", "x")); err != nil {
			t.Fatal(err)
		}
	}
	if subs, _ := e.st.PushSubscriptions(); len(subs) != 0 {
		t.Errorf("subscription kept after %d refusals", maxPushFailures)
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	e := newPushEnv(t, 403)
	_ = e.pusher().Handle(context.Background(), "k", e.job(0, "normal", "x"))
	e.svcs[0].set(201)
	_ = e.pusher().Handle(context.Background(), "k", e.job(0, "normal", "x"))
	if subs, _ := e.st.PushSubscriptions(); len(subs) != 1 || subs[0].Failures != 0 {
		t.Errorf("subscriptions %+v", subs)
	}
}

// One failing push service is retried alone: the healthy device gets the notification once.
func TestPartialFailureSendsHealthyOnce(t *testing.T) {
	e := newPushEnv(t, 201, 503)
	p := e.pusher()
	ctx := context.Background()
	if err := p.Handle(ctx, "k|a", e.job(0, "normal", "x")); err != nil {
		t.Fatal(err)
	}
	for range 3 { // the runner retries only the failing effect
		if err := p.Handle(ctx, "k|b", e.job(1, "normal", "x")); err == nil {
			t.Fatal("the failing service did not fail")
		}
	}
	if e.svcs[0].count() != 1 || e.svcs[1].count() != 3 {
		t.Errorf("healthy got %d, failing got %d", e.svcs[0].count(), e.svcs[1].count())
	}
}

func TestStaleOrExpiredOrVisiblePushNotSent(t *testing.T) {
	ctx := context.Background()
	e := newPushEnv(t, 201)
	job := e.job(0, "normal", "x")

	e.stage = func(string) (core.Stage, error) { return core.Stage{Stage: "working"}, nil }
	if err := e.pusher().Handle(ctx, "k", job); err != nil || e.svcs[0].count() != 0 {
		t.Fatalf("ticket moved on: %v, %d sent", err, e.svcs[0].count())
	}
	e.stage = func(string) (core.Stage, error) { return core.Stage{Stage: "person", Action: "review"}, nil }
	if err := e.pusher().Handle(ctx, "k", job); err != nil || e.svcs[0].count() != 0 {
		t.Fatalf("other action: %v, %d sent", err, e.svcs[0].count())
	}
	e.stage = func(string) (core.Stage, error) { return core.Stage{}, &core.UserError{Msg: "ticket T-1 not found"} }
	if err := e.pusher().Handle(ctx, "k", job); err != nil || e.svcs[0].count() != 0 {
		t.Fatalf("ticket gone: %v, %d sent", err, e.svcs[0].count())
	}
	e.stage = func(string) (core.Stage, error) { return core.Stage{}, errors.New("db busy") }
	if err := e.pusher().Handle(ctx, "k", job); err == nil {
		t.Fatal("a failed stage read did not retry")
	}
	e.stage = func(string) (core.Stage, error) { return core.Stage{Stage: "person", Action: "answer"}, nil }
	e.vis = func() bool { return true }
	if err := e.pusher().Handle(ctx, "k", job); err != nil || e.svcs[0].count() != 0 {
		t.Fatalf("visible: %v, %d sent", err, e.svcs[0].count())
	}
	e.vis = nil
	e.now = e.now.Add(pushTTL + time.Minute)
	if err := e.pusher().Handle(ctx, "k", job); err != nil || e.svcs[0].count() != 0 {
		t.Fatalf("expired: %v, %d sent", err, e.svcs[0].count())
	}
	e.now = e.now.Add(-pushTTL - time.Minute)
	if err := e.pusher().Handle(ctx, "k", job); err != nil || e.svcs[0].count() != 1 {
		t.Fatalf("current: %v, %d sent", err, e.svcs[0].count())
	}
}

func TestUnreadablePayloadDroppedAndLogged(t *testing.T) {
	e := newPushEnv(t, 201)
	var buf bytes.Buffer
	p := NewPusher(PusherConfig{Store: e.st, VAPID: e.vapid, Log: slog.New(slog.NewTextHandler(&buf, nil))})
	if err := p.Handle(context.Background(), "k", []byte("{")); err != nil || !strings.Contains(buf.String(), "unreadable payload") {
		t.Errorf("%v %q", err, buf.String())
	}
}

func TestBadKeysDeleteButOversizeKeepsSubscription(t *testing.T) {
	e := newPushEnv(t, 201)
	ep := e.svcs[0].srv.URL + "/sub/1"
	big, _ := json.Marshal(strings.Repeat("x", 5000))
	job, _ := json.Marshal(pushJob{Endpoint: ep, Ticket: "T-1", Action: "answer", At: e.now.Unix(), Message: big})
	if err := e.pusher().Handle(context.Background(), "k", job); err != nil || e.svcs[0].count() != 0 {
		t.Fatalf("oversize: %v", err)
	}
	if subs, _ := e.st.PushSubscriptions(); len(subs) != 1 {
		t.Fatal("an oversize message dropped the subscription")
	}
	_ = e.st.PutPushSubscription(store.PushSubscription{SessionHash: []byte("seed-session-hash"), Endpoint: ep, P256dh: "AAAA", Auth: "AAAA", CreatedAt: time.Now()})
	if err := e.pusher().Handle(context.Background(), "k", e.job(0, "normal", "x")); err != nil {
		t.Fatal(err)
	}
	if subs, _ := e.st.PushSubscriptions(); len(subs) != 0 {
		t.Error("a subscription with bad keys was kept")
	}
}
