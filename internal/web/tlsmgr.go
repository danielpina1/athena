// Package web serves the dashboard. This file decides which certificate the web listener presents and keeps it fresh
// without a restart.
package web

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/pki"
)

const (
	tlsDirName = "tls"
	caDirName  = "pki"
	certFile   = "server.pem"
	keyFile    = "server.key"

	// renewBefore is how long before expiry a certificate is renewed (ACME) or re-issued (own CA).
	renewBefore   = 30 * 24 * time.Hour
	checkInterval = 12 * time.Hour
	// retryMin and retryMax bound the wait after a failed ACME attempt: it starts at 1 h and doubles up to 12 h.
	retryMin = time.Hour
	retryMax = 12 * time.Hour
)

// TLSConfig says how the web listener gets its certificate. Mode is "ca" (default: Athena's own CA), "acme" (a public
// certificate through DNS-01) or "off" (plain HTTP, loopback only).
type TLSConfig struct {
	Mode   string
	Host   string
	IPs    []net.IP
	Listen string
	ACME   ACMEConfig
}

// ACMEConfig configures mode "acme". Provider is cloudflare, exec or httpreq; Directory defaults to Let's Encrypt
// production.
type ACMEConfig struct {
	Email     string
	Provider  string
	Resolvers []string
	Directory string
	// AgreeTOS is the owner's agreement to the CA's terms of service; without it no account is registered.
	AgreeTOS bool `json:"agreeTos"`
}

// TLSManager holds the certificate the web listener presents and replaces it before it expires.
type TLSManager struct {
	mode string
	host string
	ips  []net.IP
	dir  string
	ca   *pki.CA  // mode ca
	obt  Obtainer // mode acme
	log  *slog.Logger
	now  func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate // nil until there is one
	lastErr error            // last failed renewal, nil after a success
	retry   time.Duration    // current ACME backoff
}

// NewTLSManager prepares the certificate for c under stateDir/tls. In mode ca the CA (stateDir/pki) is loaded or
// created and a certificate issued or reused at once. In mode acme a stored certificate is reused, and getting one is
// left to Run, so a CA outage does not stop the server from starting. o is only used in mode acme.
func NewTLSManager(stateDir string, c TLSConfig, o Obtainer, log *slog.Logger, now func() time.Time) (*TLSManager, error) {
	mode := c.Mode
	if mode == "" {
		mode = "ca"
	}
	m := &TLSManager{mode: mode, host: strings.ToLower(c.Host), ips: c.IPs, dir: filepath.Join(stateDir, tlsDirName), obt: o, log: log, now: now}
	switch mode {
	case "off":
		if !loopbackListen(c.Listen) {
			return nil, fmt.Errorf("web: tls off is allowed only on a loopback address (listen %q)", c.Listen)
		}
		return m, nil
	case "ca":
	case "acme":
		if o == nil {
			return nil, errors.New("web: tls mode acme needs an obtainer")
		}
		if m.host == "" {
			return nil, errors.New("web: tls mode acme needs a host")
		}
	default:
		return nil, fmt.Errorf("web: unknown tls mode %q (want ca, acme or off)", c.Mode)
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	if err := os.Chmod(m.dir, 0o700); err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	if mode == "ca" {
		ca, err := pki.LoadOrCreateCA(CADir(stateDir), c.Host, c.IPs)
		if err != nil {
			return nil, err
		}
		m.ca = ca
		m.host = ca.Host()
	}
	if cert, err := m.loadStored(); err != nil {
		log.Warn("tls: stored certificate unusable, ignoring it", "err", err)
	} else {
		m.cert = cert
	}
	if mode == "ca" && m.due() {
		if err := m.issueCA(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// CADir is where mode ca keeps Athena's CA (ca.pem, ca.key) under the state directory.
func CADir(stateDir string) string { return filepath.Join(stateDir, caDirName) }

// CertFile is the certificate the web listener presents, under the state directory.
func CertFile(stateDir string) string { return filepath.Join(stateDir, tlsDirName, certFile) }

// Mode is the TLS mode: ca, acme or off.
func (m *TLSManager) Mode() string { return m.mode }

// CA is Athena's CA in mode ca, nil otherwise.
func (m *TLSManager) CA() *pki.CA { return m.ca }

// loopbackListen reports whether addr (host:port) binds only to the loopback interface. An empty or wildcard host
// listens on every interface and is not loopback.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Config is the tls.Config for the web listener, or nil for mode off. It reads the current certificate on every
// handshake, so a renewal takes effect without a restart. With no certificate yet a handshake fails.
func (m *TLSManager) Config() *tls.Config {
	if m.mode == "off" {
		return nil
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: m.getCertificate,
	}
}

func (m *TLSManager) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cert == nil {
		return nil, errors.New("no certificate available yet")
	}
	return m.cert, nil
}

// Run checks the certificate now and then every 12 h until ctx ends, renewing or re-issuing it when 30 days or fewer
// remain. After a failed ACME attempt it retries sooner, from 1 h doubling to 12 h. The current certificate keeps
// serving through failures.
func (m *TLSManager) Run(ctx context.Context) {
	if m.mode == "off" {
		<-ctx.Done()
		return
	}
	for {
		t := time.NewTimer(m.check(ctx))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// check renews the certificate if it is due and returns how long to wait before the next check.
func (m *TLSManager) check(ctx context.Context) time.Duration {
	if !m.due() {
		return checkInterval
	}
	var err error
	if m.mode == "ca" {
		err = m.issueCA()
	} else {
		err = m.renewACME(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastErr = err
	if err == nil {
		m.retry = 0
		return checkInterval
	}
	if m.retry == 0 {
		m.retry = retryMin
	} else {
		m.retry = min(m.retry*2, retryMax)
	}
	m.log.Error("tls: certificate renewal failed", "mode", m.mode, "err", err, "retry_in", m.retry)
	return m.retry
}

// due reports whether there is no certificate or it expires within renewBefore.
func (m *TLSManager) due() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cert == nil || m.cert.Leaf.NotAfter.Sub(m.now()) <= renewBefore
}

func (m *TLSManager) issueCA() error {
	cert, err := m.ca.ServerCert(m.host, m.ips)
	if err != nil {
		return err
	}
	return m.install(cert)
}

func (m *TLSManager) renewACME(ctx context.Context) error {
	certPEM, keyPEM, err := m.obt.Obtain(ctx, m.host)
	if err != nil {
		return err
	}
	cert, err := parsePair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("web: obtained certificate: %w", err)
	}
	if err := cert.Leaf.VerifyHostname(m.host); err != nil {
		return fmt.Errorf("web: obtained certificate: %w", err)
	}
	if !cert.Leaf.NotAfter.After(m.now()) {
		return errors.New("web: obtained certificate is already expired")
	}
	return m.install(*cert)
}

// install stores cert on disk and then makes it the one served. A failed write leaves the old certificate serving.
func (m *TLSManager) install(cert tls.Certificate) error {
	certPEM, keyPEM, err := encodePair(cert)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(m.dir, keyFile), keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(m.dir, certFile), certPEM, 0o644); err != nil {
		return err
	}
	m.mu.Lock()
	m.cert = &cert
	m.mu.Unlock()
	m.log.Info("tls: certificate installed", "mode", m.mode, "not_after", cert.Leaf.NotAfter.Format(time.RFC3339))
	return nil
}

// loadStored reads the certificate from a previous run. It returns nil with no error when there is none, and an error
// for a pair that is torn, for another host, or (mode ca) not issued by the current CA.
func (m *TLSManager) loadStored() (*tls.Certificate, error) {
	certPEM, err := os.ReadFile(filepath.Join(m.dir, certFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(m.dir, keyFile))
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	cert, err := parsePair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if err := cert.Leaf.VerifyHostname(m.host); err != nil {
		return nil, err
	}
	if m.mode == "ca" {
		for _, ip := range m.ips {
			if err := cert.Leaf.VerifyHostname(ip.String()); err != nil {
				return nil, err
			}
		}
		ca, err := x509.ParseCertificate(m.ca.DER())
		if err != nil {
			return nil, fmt.Errorf("web: %w", err)
		}
		roots := x509.NewCertPool()
		roots.AddCert(ca)
		// Checked at the leaf's own start: expiry is the renewal logic's business, issuer and names are the question here.
		if _, err := cert.Leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: cert.Leaf.NotBefore.Add(time.Minute), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return nil, err
		}
	}
	return cert, nil
}

// Health reports whether the web certificate is usable and renewing, with a message for the owner and the expiry of
// the certificate being served (zero when there is none, and for mode off).
func (m *TLSManager) Health() (ok bool, msg string, notAfter time.Time) {
	if m.mode == "off" {
		return true, "tls off (loopback only)", time.Time{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cert == nil {
		if m.lastErr != nil {
			return false, "no certificate yet: " + m.lastErr.Error(), time.Time{}
		}
		return false, "no certificate yet", time.Time{}
	}
	notAfter = m.cert.Leaf.NotAfter
	left := notAfter.Sub(m.now())
	switch {
	case left <= 0:
		return false, "certificate expired " + notAfter.Format(time.DateOnly), notAfter
	case m.lastErr != nil:
		return false, fmt.Sprintf("renewal failing (certificate valid until %s): %v", notAfter.Format(time.DateOnly), m.lastErr), notAfter
	}
	return true, "certificate valid until " + notAfter.Format(time.DateOnly), notAfter
}

func parsePair(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	return &cert, nil
}

func encodePair(cert tls.Certificate) (certPEM, keyPEM []byte, err error) {
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("web: %w", err)
	}
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// writeFileAtomic replaces path with data at mode perm in one rename, so a crash never leaves a half-written file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return fmt.Errorf("web: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("web: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("web: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("web: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("web: %w", err)
	}
	return nil
}
