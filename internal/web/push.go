package web

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/store"
)

// Web Push (RFC 8030) with message encryption (RFC 8291, aes128gcm) and VAPID (RFC 8292), on the standard library.

const (
	// PushKind is the effects kind that delivers a notification.
	PushKind = "push"
	// pushTTL is how long a push service keeps an undelivered message.
	pushTTL = 24 * time.Hour
	// vapidLife is the lifetime of a VAPID token (the limit is 24 h).
	vapidLife = 12 * time.Hour
	// recordSize is the aes128gcm record size; a message is one record, so it must fit.
	recordSize = 4096
	// maxPushFailures is how many refused deliveries in a row (a status that retrying cannot fix) drop a subscription.
	maxPushFailures = 5
)

var pushB64 = base64.RawURLEncoding

// VAPID is the server's application server key pair.
type VAPID struct {
	key *ecdsa.PrivateKey
	// Subject is the token's sub claim, an https URL naming the server. Empty: the origin of the push endpoint.
	Subject string
}

// LoadOrCreateVAPID loads the P-256 key at <dir>/push/vapid.key, or creates it (0600) if there is none.
func LoadOrCreateVAPID(dir string) (*VAPID, error) {
	path := filepath.Join(dir, "push", "vapid.key")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("web: generate VAPID key: %w", err)
		}
		priv, err := key.Bytes()
		if err != nil {
			return nil, fmt.Errorf("web: encode VAPID key: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("web: create %s: %w", filepath.Dir(path), err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreateVAPID(dir) // created meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("web: create %s: %w", path, err)
		}
		_, err = f.WriteString(pushB64.EncodeToString(priv) + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("web: write %s: %w", path, err)
		}
		return &VAPID{key: key}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("web: read %s: %w", path, err)
	}
	priv, err := pushB64.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("web: %s is not a VAPID key", path)
	}
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), priv)
	if err != nil {
		return nil, fmt.Errorf("web: %s is not a VAPID key: %w", path, err)
	}
	return &VAPID{key: key}, nil
}

// PublicKey is the application server key, the uncompressed P-256 point in base64url: what the browser passes to
// pushManager.subscribe.
func (v *VAPID) PublicKey() string {
	pub, err := v.key.PublicKey.Bytes()
	if err != nil {
		return "" // a generated or parsed P-256 key always encodes
	}
	return pushB64.EncodeToString(pub)
}

// Handler serves the public key as text at the path it is mounted on.
func (v *VAPID) Handler() http.Handler {
	pub := v.PublicKey()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, pub)
	})
}

// VAPIDHeader is the Authorization header value for a push to endpoint (RFC 8292): "vapid t=<jwt>, k=<key>". The
// token's aud is the endpoint's origin, exp is 12 h out and sub is an https URL, never an email address.
func VAPIDHeader(v *VAPID, endpoint string, now time.Time) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	aud := u.Scheme + "://" + u.Host
	sub := v.Subject
	if sub == "" {
		sub = "https://" + u.Hostname()
	}
	claims, _ := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{aud, now.Add(vapidLife).Unix(), sub})
	signing := pushB64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + pushB64.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	der, err := ecdsa.SignASN1(rand.Reader, v.key, sum[:])
	if err != nil {
		return ""
	}
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &sig); err != nil {
		return ""
	}
	raw := make([]byte, 64)
	sig.R.FillBytes(raw[:32])
	sig.S.FillBytes(raw[32:])
	return "vapid t=" + signing + "." + pushB64.EncodeToString(raw) + ", k=" + v.PublicKey()
}

// Subscription is a browser's push subscription: the endpoint and the keys the browser made for it (base64url).
type Subscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// Encrypt makes the body of a push message for sub (RFC 8291 aes128gcm): the 86-byte header (salt, record size, the
// server key as key id) and one record of plaintext. serverKey is a key made for this message alone; salt is 16 random
// bytes.
func Encrypt(sub Subscription, plaintext, salt []byte, serverKey *ecdh.PrivateKey) ([]byte, error) {
	if len(salt) != 16 {
		return nil, errors.New("web: push salt must be 16 bytes")
	}
	if len(plaintext) > recordSize-16-1-86 {
		return nil, errors.New("web: push message too large")
	}
	uaRaw, err := pushB64.DecodeString(sub.P256dh)
	if err != nil {
		return nil, fmt.Errorf("web: push subscription key: %w", err)
	}
	authSecret, err := pushB64.DecodeString(sub.Auth)
	if err != nil || len(authSecret) != 16 {
		return nil, errors.New("web: push subscription auth secret must be 16 bytes")
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaRaw)
	if err != nil {
		return nil, fmt.Errorf("web: push subscription key: %w", err)
	}
	shared, err := serverKey.ECDH(uaPub)
	if err != nil {
		return nil, fmt.Errorf("web: push key agreement: %w", err)
	}
	asPub := serverKey.PublicKey().Bytes()

	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(uaRaw)+string(asPub), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 16+4+1+len(asPub)+len(plaintext)+1+aead.Overhead())
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPub)))
	out = append(out, asPub...)
	record := append(append([]byte{}, plaintext...), 0x02) // 0x02: the last record
	return aead.Seal(out, nonce, record, nil), nil
}

// Pusher delivers push messages: it is the handler of the "push" effects.
type Pusher struct {
	st     *store.Store
	vapid  *VAPID
	client *http.Client
	log    *slog.Logger
}

// NewPusher returns a Pusher over the store's subscriptions. A nil client gets one with a 20 s timeout; a nil log
// discards logs.
func NewPusher(st *store.Store, v *VAPID, client *http.Client, log *slog.Logger) *Pusher {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Pusher{st: st, vapid: v, client: client, log: log}
}

// Register makes p the handler of the push effects: retries back off from 5 s to 300 s.
func (p *Pusher) Register(r *effects.Runner) {
	r.Handle(PushKind, p.Handle, effects.Options{Concurrency: 2, Backoff: effects.Backoff{First: 5 * time.Second, Max: 300 * time.Second}})
}

// pushJob is the payload of a push effect: the message the browser shows and how urgent it is.
type pushJob struct {
	Urgency string          `json:"urgency"`
	Message json.RawMessage `json:"msg"`
}

// Handle sends the effect's message to every subscription: 2xx is done, 404 and 410 delete the subscription, 429 and
// 5xx (and a failed connection) fail the effect so it is retried, and any other refusal counts against the
// subscription, which is dropped after a few in a row.
func (p *Pusher) Handle(ctx context.Context, _ string, payload []byte) error {
	var job pushJob
	if err := json.Unmarshal(payload, &job); err != nil {
		return nil // a payload that cannot be read cannot be fixed by retrying
	}
	subs, err := p.st.PushSubscriptions()
	if err != nil {
		return err
	}
	var retry error
	for _, s := range subs {
		if err := p.send(ctx, s, job); err != nil {
			if retry == nil || errors.Is(retry, effects.ErrLater) && !errors.Is(err, effects.ErrLater) {
				retry = err
			}
		}
	}
	return retry
}

func (p *Pusher) send(ctx context.Context, s store.PushSubscription, job pushJob) error {
	// The endpoint is logged by host only: the path is the capability to push to the browser.
	host := "?"
	if u, err := url.Parse(s.Endpoint); err == nil {
		host = u.Host
	}
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	body, err := Encrypt(Subscription{Endpoint: s.Endpoint, P256dh: s.P256dh, Auth: s.Auth}, job.Message, salt, key)
	if err != nil {
		p.log.Warn("push: subscription unusable, dropped", "host", host, "error", err)
		return p.st.DeletePushSubscription(s.Endpoint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return p.st.DeletePushSubscription(s.Endpoint)
	}
	req.Header.Set("Authorization", VAPIDHeader(p.vapid, s.Endpoint, time.Now()))
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(pushTTL/time.Second)))
	req.Header.Set("Urgency", job.Urgency)
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("push: send to %s: %w", host, errors.Unwrap(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch c := resp.StatusCode; {
	case c >= 200 && c < 300:
		return nil
	case c == http.StatusNotFound || c == http.StatusGone:
		p.log.Info("push: subscription gone, deleted", "host", host)
		return p.st.DeletePushSubscription(s.Endpoint)
	case c == http.StatusTooManyRequests:
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			return effects.Later(min(time.Duration(secs)*time.Second, 300*time.Second))
		}
		return fmt.Errorf("push: %s answered 429", host)
	case c >= 500:
		return fmt.Errorf("push: %s answered %d", host, c)
	default:
		n, err := p.st.BumpPushFailures(s.Endpoint)
		p.log.Warn("push: refused", "host", host, "status", c, "failures", n)
		if err == nil && n >= maxPushFailures {
			return p.st.DeletePushSubscription(s.Endpoint)
		}
		return err
	}
}

// ValidSubscriptionKeys reports whether p256dh is a base64url P-256 point and auth a base64url 16-byte secret.
func ValidSubscriptionKeys(p256dh, auth string) bool {
	raw, err := pushB64.DecodeString(p256dh)
	if err != nil {
		return false
	}
	if _, err := ecdh.P256().NewPublicKey(raw); err != nil {
		return false
	}
	a, err := pushB64.DecodeString(auth)
	return err == nil && len(a) == 16
}
