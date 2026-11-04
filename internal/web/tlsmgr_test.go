package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testHost = "athena.test"

var testIPs = []net.IP{net.ParseIP("127.0.0.1")}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// serve starts an HTTPS listener whose TLS config comes from the manager and returns its address.
func serve(t *testing.T, m *TLSManager) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = m.Config()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// handshake connects to addr and returns the server's leaf certificate. With roots nil the chain is not verified.
func handshake(addr string, roots *x509.CertPool) (*x509.Certificate, error) {
	conf := &tls.Config{ServerName: testHost, RootCAs: roots, InsecureSkipVerify: roots == nil}
	c, err := tls.Dial("tcp", addr, conf)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.ConnectionState().PeerCertificates[0], nil
}

func caRoots(t *testing.T, state string) *x509.CertPool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "pki", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		t.Fatal("ca.pem is not a certificate")
	}
	return pool
}

func caConfig() TLSConfig {
	return TLSConfig{Mode: "ca", Host: testHost, IPs: testIPs, Listen: "127.0.0.1:0"}
}

func TestCAModeServesCAIssuedCert(t *testing.T) {
	state := t.TempDir()
	clk := &clock{t: time.Now()}
	m, err := NewTLSManager(state, caConfig(), nil, quietLog(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	cfg := m.Config()
	if cfg.MinVersion != tls.VersionTLS12 || len(cfg.NextProtos) != 2 || cfg.NextProtos[0] != "h2" || cfg.NextProtos[1] != "http/1.1" {
		t.Fatalf("config = min %x protos %v", cfg.MinVersion, cfg.NextProtos)
	}
	leaf, err := handshake(serve(t, m), caRoots(t, state))
	if err != nil {
		t.Fatalf("handshake verified against the CA: %v", err)
	}
	if leaf.VerifyHostname(testHost) != nil || leaf.VerifyHostname("127.0.0.1") != nil {
		t.Fatalf("certificate names = %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if ok, msg, na := m.Health(); !ok || !na.Equal(leaf.NotAfter) {
		t.Fatalf("health = %v %q %v", ok, msg, na)
	}
	// The certificate and key are stored for a restart and for `athena web status`.
	for name, perm := range map[string]os.FileMode{"tls": 0o700, "tls/server.key": 0o600, "tls/server.pem": 0o644} {
		st, err := os.Stat(filepath.Join(state, name))
		if err != nil || st.Mode().Perm() != perm {
			t.Errorf("%s: %v mode %v, want %v", name, err, st, perm)
		}
	}
	m2, err := NewTLSManager(state, caConfig(), nil, quietLog(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	leaf2, err := handshake(serve(t, m2), caRoots(t, state))
	if err != nil || leaf2.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Fatalf("a restart should reuse the stored certificate: %v", err)
	}
}

func TestReissueBeforeExpiry(t *testing.T) {
	state := t.TempDir()
	clk := &clock{t: time.Now()}
	m, err := NewTLSManager(state, caConfig(), nil, quietLog(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	addr, roots := serve(t, m), caRoots(t, state)
	first, err := handshake(addr, roots)
	if err != nil {
		t.Fatal(err)
	}

	clk.set(first.NotAfter.Add(-31 * 24 * time.Hour))
	m.check(context.Background())
	if again, _ := handshake(addr, roots); again.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatal("re-issued with 31 days left")
	}

	clk.set(first.NotAfter.Add(-29 * 24 * time.Hour))
	if next := m.check(context.Background()); next != checkInterval {
		t.Fatalf("next check in %v", next)
	}
	second, err := handshake(addr, roots)
	if err != nil {
		t.Fatal(err)
	}
	if second.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatal("no new certificate at 29 days left")
	}
	// The stored certificate is the new one.
	b, _ := os.ReadFile(filepath.Join(state, "tls", "server.pem"))
	blk, _ := pem.Decode(b)
	if stored, _ := x509.ParseCertificate(blk.Bytes); stored.SerialNumber.Cmp(second.SerialNumber) != 0 {
		t.Fatal("stored certificate is stale")
	}
}

func TestCAModeReissuesCertFromAnotherCA(t *testing.T) {
	state := t.TempDir()
	clk := &clock{t: time.Now()}
	m, err := NewTLSManager(state, caConfig(), nil, quietLog(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := handshake(serve(t, m), nil)
	if err := os.RemoveAll(filepath.Join(state, "pki")); err != nil {
		t.Fatal(err)
	}
	m2, err := NewTLSManager(state, caConfig(), nil, quietLog(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := handshake(serve(t, m2), caRoots(t, state))
	if err != nil || leaf.SerialNumber.Cmp(old.SerialNumber) == 0 {
		t.Fatalf("a certificate from a replaced CA must not be reused: %v", err)
	}
}

// fakeObtainer issues self-signed certificates and can be made to fail.
type fakeObtainer struct {
	mu    sync.Mutex
	err   error
	host  string // when set, certificates are issued for this name instead of the requested one
	calls int
	valid time.Duration
	clk   *clock
}

func (f *fakeObtainer) Obtain(_ context.Context, host string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, nil, f.err
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	if f.host != "" {
		host = f.host
	}
	now := f.clk.now()
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(f.valid),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), nil
}

func (f *fakeObtainer) fail(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func acmeConfig() TLSConfig {
	return TLSConfig{Mode: "acme", Host: testHost, Listen: "127.0.0.1:0", ACME: ACMEConfig{Provider: "exec"}}
}

func TestACMEFailureKeepsOldCert(t *testing.T) {
	state := t.TempDir()
	clk := &clock{t: time.Now()}
	obt := &fakeObtainer{valid: 90 * 24 * time.Hour, clk: clk}
	m, err := NewTLSManager(state, acmeConfig(), obt, quietLog(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	addr := serve(t, m)

	// No certificate yet: the handshake fails, health says so, and the first Run check gets one.
	if _, err := handshake(addr, nil); err == nil {
		t.Fatal("served a certificate that does not exist")
	}
	if ok, _, _ := m.Health(); ok {
		t.Fatal("health ok with no certificate")
	}
	if next := m.check(context.Background()); next != checkInterval {
		t.Fatalf("next = %v", next)
	}
	first, err := handshake(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := m.Health(); !ok {
		t.Fatal("health failing after a successful obtain")
	}

	// 29 days left and the CA is down: the old certificate keeps serving; retries back off 1 h, 2 h, ... 12 h.
	clk.set(first.NotAfter.Add(-29 * 24 * time.Hour))
	obt.fail(errors.New("dns provider down"))
	for _, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 12 * time.Hour, 12 * time.Hour} {
		if got := m.check(context.Background()); got != want {
			t.Fatalf("backoff = %v, want %v", got, want)
		}
	}
	still, err := handshake(addr, nil)
	if err != nil || still.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatalf("old certificate not served during failure: %v", err)
	}
	ok, msg, na := m.Health()
	if ok || !strings.Contains(msg, "dns provider down") || !na.Equal(first.NotAfter) {
		t.Fatalf("health = %v %q %v", ok, msg, na)
	}

	// Recovery installs the new certificate, clears the failure and resets the backoff.
	obt.fail(nil)
	if got := m.check(context.Background()); got != checkInterval {
		t.Fatalf("next after success = %v", got)
	}
	renewed, _ := handshake(addr, nil)
	if renewed.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatal("not renewed")
	}
	if ok, _, _ := m.Health(); !ok {
		t.Fatal("health still failing after recovery")
	}
	obt.fail(errors.New("again"))
	clk.set(renewed.NotAfter.Add(-29 * 24 * time.Hour))
	if got := m.check(context.Background()); got != time.Hour {
		t.Fatalf("backoff did not restart at 1 h: %v", got)
	}
}

func TestACMEExpiredCertReportsUnhealthy(t *testing.T) {
	state := t.TempDir()
	clk := &clock{t: time.Now()}
	obt := &fakeObtainer{valid: 40 * 24 * time.Hour, clk: clk}
	m, err := NewTLSManager(state, acmeConfig(), obt, quietLog(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	m.check(context.Background())
	_, _, na := m.Health()
	clk.set(na.Add(time.Minute))
	if ok, msg, _ := m.Health(); ok || !strings.Contains(msg, "expired") {
		t.Fatalf("health = %v %q", ok, msg)
	}
}

func TestACMEStartWithStoredCertAndRunStops(t *testing.T) {
	state := t.TempDir()
	clk := &clock{t: time.Now()}
	obt := &fakeObtainer{valid: 90 * 24 * time.Hour, clk: clk}
	m, err := NewTLSManager(state, acmeConfig(), obt, quietLog(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for ok, _, _ := m.Health(); !ok; ok, _, _ = m.Health() {
		if time.Now().After(deadline) {
			t.Fatal("Run did not obtain a certificate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	// A restart reuses the stored certificate without asking the CA.
	obt.fail(errors.New("down"))
	calls := obt.calls
	m2, err := NewTLSManager(state, acmeConfig(), obt, quietLog(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := m2.Health(); !ok || obt.calls != calls {
		t.Fatalf("stored certificate not reused (ok %v)", ok)
	}
}

func TestOffRefusedOnNonLoopback(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8443", ":8443", "100.64.0.5:8443", "[::]:8443", "athena.example.com:8443", "nonsense"} {
		_, err := NewTLSManager(t.TempDir(), TLSConfig{Mode: "off", Listen: listen}, nil, quietLog(), time.Now)
		if err == nil || !strings.Contains(err.Error(), "tls off is allowed only on a loopback address") {
			t.Errorf("%s: err = %v", listen, err)
		}
	}
	for _, listen := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		m, err := NewTLSManager(t.TempDir(), TLSConfig{Mode: "off", Listen: listen}, nil, quietLog(), time.Now)
		if err != nil {
			t.Errorf("%s: %v", listen, err)
			continue
		}
		if m.Config() != nil {
			t.Errorf("%s: off must have no tls.Config", listen)
		}
		if ok, _, _ := m.Health(); !ok {
			t.Errorf("%s: off should be healthy", listen)
		}
	}
}

func TestUnknownModeRejected(t *testing.T) {
	if _, err := NewTLSManager(t.TempDir(), TLSConfig{Mode: "tofu", Host: testHost}, nil, quietLog(), time.Now); err == nil {
		t.Fatal("accepted an unknown mode")
	}
}

func TestLegoObtainerRejectsUnknownProvider(t *testing.T) {
	state := t.TempDir()
	_, err := NewLegoObtainer(state, ACMEConfig{Provider: "route53", Email: "a@b.c"})
	if err == nil || !strings.Contains(err.Error(), "unknown acme provider") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(state, "tls", "acme")); statErr == nil {
		t.Fatal("created account state for a provider that was rejected")
	}
}

func TestLegoObtainerCreatesAccountKey(t *testing.T) {
	t.Setenv("HTTPREQ_ENDPOINT", "http://127.0.0.1:1")
	state := t.TempDir()
	if _, err := NewLegoObtainer(state, ACMEConfig{Provider: "httpreq"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(state, "tls", "acme", "account.key")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("account key: %v %v", st, err)
	}
	before, _ := os.ReadFile(p)
	if _, err := NewLegoObtainer(state, ACMEConfig{Provider: "httpreq"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Fatal("account key was replaced on the second start")
	}
}

func TestACMERejectsWrongHostAndExpiredResult(t *testing.T) {
	clk := &clock{t: time.Now()}
	for name, obt := range map[string]*fakeObtainer{
		"wrong host": {valid: 90 * 24 * time.Hour, clk: clk, host: "other.example"},
		"expired":    {valid: -2 * time.Hour, clk: clk},
	} {
		m, err := NewTLSManager(t.TempDir(), acmeConfig(), obt, quietLog(), clk.now)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.check(context.Background()); got != time.Hour {
			t.Errorf("%s: next = %v", name, got)
		}
		if ok, _, na := m.Health(); ok || !na.IsZero() {
			t.Errorf("%s: a bad result was installed", name)
		}
		if _, err := handshake(serve(t, m), nil); err == nil {
			t.Errorf("%s: served a rejected certificate", name)
		}
	}
}

func TestLegoObtainerNeedsTOSAgreement(t *testing.T) {
	t.Setenv("HTTPREQ_ENDPOINT", "http://127.0.0.1:1")
	state := t.TempDir()
	o, err := NewLegoObtainer(state, ACMEConfig{Provider: "httpreq", Directory: "http://127.0.0.1:1/dir"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := o.Obtain(context.Background(), testHost); err == nil || !strings.Contains(err.Error(), "agreeTos") || !strings.Contains(err.Error(), "letsencrypt.org/repository") {
		t.Fatalf("err = %v", err)
	}
	clk := &clock{t: time.Now()}
	m, err := NewTLSManager(state, acmeConfig(), o, quietLog(), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	m.check(context.Background())
	if ok, msg, _ := m.Health(); ok || !strings.Contains(msg, "agreeTos") {
		t.Fatalf("health = %v %q", ok, msg)
	}
}

func TestLegoObtainerRejectsOpenAccountKey(t *testing.T) {
	t.Setenv("HTTPREQ_ENDPOINT", "http://127.0.0.1:1")
	state := t.TempDir()
	if _, err := NewLegoObtainer(state, ACMEConfig{Provider: "httpreq"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(state, "tls", "acme", "account.key")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLegoObtainer(state, ACMEConfig{Provider: "httpreq"}); err == nil || !strings.Contains(err.Error(), "mode 0644") {
		t.Fatalf("err = %v", err)
	}
}
