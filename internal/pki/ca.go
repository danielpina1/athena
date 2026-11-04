// Package pki is Athena's own certificate authority. Without a public domain the dashboard has no ACME certificate, so
// Athena signs its own from a CA the owner installs on their devices. The CA is name-constrained to the owner's host
// and VPN addresses, so a leaked CA key cannot impersonate any other site. Node client certificates (plan 6) come from
// the same CA.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	caKeyFile  = "ca.key"
	caCertFile = "ca.pem"

	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 397 * 24 * time.Hour // under Apple's 825-day limit for server certificates
	// clockSkew backdates NotBefore so a device whose clock runs a little behind still accepts a fresh certificate.
	clockSkew = 5 * time.Minute
)

// CA is a loaded certificate authority: its certificate and signing key.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	host string
}

// LoadOrCreateCA loads the CA from dir (ca.key, ca.pem), creating it on first use. The CA is constrained to host (and
// its subdomains) and to exactly ips. An existing CA is returned as it is: host and ips only matter when it is created.
// dir is 0700 and ca.key is 0600; a ca.key open to group or others is refused.
func LoadOrCreateCA(dir string, host string, ips []net.IP) (*CA, error) {
	if host == "" {
		return nil, errors.New("pki: host is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	keyPath := filepath.Join(dir, caKeyFile)
	if _, err := os.Stat(keyPath); err == nil {
		return loadCA(dir, host)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("pki: %w", err)
	}
	return createCA(dir, host, ips)
}

func loadCA(dir, host string) (*CA, error) {
	keyPath := filepath.Join(dir, caKeyFile)
	st, err := os.Stat(keyPath)
	if err != nil {
		return nil, fmt.Errorf("pki: key file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("pki: key file %s is not a regular file", keyPath)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("pki: key file %s has mode %04o: group and others must have no access (chmod 0600)", keyPath, perm)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("pki: %s is not a PKCS#8 PEM private key", keyPath)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", keyPath, err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("pki: %s is not an ECDSA P-256 key", keyPath)
	}
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("pki: %s is not a PEM certificate", caCertFile)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", caCertFile, err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, fmt.Errorf("pki: %s does not match %s", caCertFile, caKeyFile)
	}
	return &CA{cert: cert, key: key, host: host}, nil
}

func createCA(dir, host string, ips []net.IP) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "Athena CA (" + host + ")"},
		NotBefore:                   now.Add(-clockSkew),
		NotAfter:                    now.Add(caValidity),
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{host},
	}
	for _, ip := range ips {
		tmpl.PermittedIPRanges = append(tmpl.PermittedIPRanges, hostRange(ip))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	// The certificate goes first and the key last: ca.key is what marks a CA as existing, so a crash in between leaves
	// no half-made CA, only a certificate the next run overwrites.
	if err := writeFileAtomic(filepath.Join(dir, caCertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, caKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, host: host}, nil
}

// hostRange is ip as a /32 (IPv4) or /128 (IPv6) network.
func hostRange(ip net.IP) *net.IPNet {
	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}
	}
	return &net.IPNet{IP: ip.To16(), Mask: net.CIDRMask(128, 128)}
}

// ServerCert issues a TLS server certificate for host and ips, valid for 397 days. Names outside the CA's constraints
// still issue but no client will accept them.
func (c *CA) ServerCert(host string, ips []net.IP) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: host},
		DNSNames:    []string{host},
		IPAddresses: ips,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	return c.issue(tmpl)
}

// ClientCert issues a TLS client certificate for a node. name must be a subdomain of the CA's host, which keeps it
// inside the name constraints.
func (c *CA) ClientCert(name string) (tls.Certificate, error) {
	if !strings.HasSuffix(name, "."+c.host) || len(name) == len(c.host)+1 {
		return tls.Certificate{}, fmt.Errorf("pki: client name %q is not a subdomain of %s", name, c.host)
	}
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: name},
		DNSNames:    []string{name},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return c.issue(tmpl)
}

func (c *CA) issue(tmpl *x509.Certificate) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("pki: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl.SerialNumber = serial
	tmpl.NotBefore = now.Add(-clockSkew)
	tmpl.NotAfter = now.Add(leafValidity)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.BasicConstraintsValid = true
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("pki: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("pki: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// Fingerprint is the SHA-256 of the CA certificate's DER as colon-separated upper-case hex, for the owner to compare
// against what their device shows.
func (c *CA) Fingerprint() string {
	sum := sha256.Sum256(c.cert.Raw)
	var b strings.Builder
	for i, v := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", v)
	}
	return b.String()
}

// Host is the name the CA is constrained to.
func (c *CA) Host() string { return c.host }

// DER is the CA certificate in DER form.
func (c *CA) DER() []byte { return c.cert.Raw }

func newSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("pki: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}

// writeFileAtomic replaces path with data at mode perm in one rename, so a crash never leaves a half-written file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return fmt.Errorf("pki: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("pki: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("pki: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("pki: %w", err)
	}
	return nil
}
