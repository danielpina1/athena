package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testHost = "athena.tailnet.example"

var testIPs = []net.IP{net.ParseIP("100.64.0.7"), net.ParseIP("fd7a:115c:a1e0::7")}

func newTestCA(t *testing.T) *CA {
	t.Helper()
	ca, err := LoadOrCreateCA(filepath.Join(t.TempDir(), "ca"), testHost, testIPs)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func pool(ca *CA) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

func TestCARoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	a, err := LoadOrCreateCA(dir, testHost, testIPs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateCA(dir, testHost, testIPs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("fingerprint changed across load: %s vs %s", a.Fingerprint(), b.Fingerprint())
	}
	if len(a.Fingerprint()) != 32*3-1 || !strings.Contains(a.Fingerprint(), ":") {
		t.Fatalf("fingerprint format: %q", a.Fingerprint())
	}
	st, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key mode %04o", st.Mode().Perm())
	}
	if st, _ = os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %04o", st.Mode().Perm())
	}
	if a.cert.Subject.CommonName != "Athena CA ("+testHost+")" || !a.cert.IsCA || !a.cert.MaxPathLenZero {
		t.Fatalf("unexpected CA cert: %+v", a.cert.Subject)
	}
	var critical bool
	for _, e := range a.cert.Extensions {
		if e.Id.String() == "2.5.29.30" {
			critical = e.Critical
		}
	}
	if !critical {
		t.Fatal("name constraints not critical")
	}

	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		if err := os.Chmod(filepath.Join(dir, "ca.key"), mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadOrCreateCA(dir, testHost, testIPs)
		if err == nil || !strings.Contains(err.Error(), "has mode") {
			t.Fatalf("mode %04o: err = %v", mode, err)
		}
	}
}

func TestServerCertVerifiesForHostAndIPs(t *testing.T) {
	ca := newTestCA(t)
	c, err := ca.ServerCert(testHost, testIPs)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{testHost, "100.64.0.7", "fd7a:115c:a1e0::7"} {
		_, err := leaf.Verify(x509.VerifyOptions{
			Roots: pool(ca), DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			t.Errorf("verify %s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool(ca), DNSName: "other." + testHost}); err == nil {
		t.Error("verified a name that is not in the SAN")
	}
	if c.PrivateKey == nil {
		t.Error("no private key")
	}
}

func TestNameConstraintsRefuseOtherNames(t *testing.T) {
	ca := newTestCA(t)
	for _, tc := range []struct {
		name string
		dns  []string
		ips  []net.IP
	}{
		{"dns", []string{"example.com"}, nil},
		{"ip", nil, []net.IP{net.ParseIP("203.0.113.9")}},
	} {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(42),
			Subject:      pkix.Name{CommonName: "evil"},
			NotBefore:    time.Now().Add(-time.Minute),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			DNSNames:     tc.dns,
			IPAddresses:  tc.ips,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(der)
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool(ca)}); err == nil {
			t.Errorf("%s: leaf outside the name constraints verified", tc.name)
		}
	}
}

func TestServerCertValidity397Days(t *testing.T) {
	ca := newTestCA(t)
	c, err := ca.ServerCert(testHost, testIPs)
	if err != nil {
		t.Fatal(err)
	}
	leaf := c.Leaf
	if got := time.Until(leaf.NotAfter); got < 397*24*time.Hour-time.Minute || got > 397*24*time.Hour {
		t.Fatalf("NotAfter in %v, want 397 days", got)
	}
	if span := leaf.NotAfter.Sub(leaf.NotBefore); span > 825*24*time.Hour {
		t.Fatalf("validity span %v exceeds Apple's 825 days", span)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("EKU = %v", leaf.ExtKeyUsage)
	}
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Fatal("no digitalSignature")
	}
}

func TestClientCert(t *testing.T) {
	ca := newTestCA(t)
	c, err := ca.ClientCert("node1." + testHost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Leaf.Verify(x509.VerifyOptions{Roots: pool(ca), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Leaf.Verify(x509.VerifyOptions{Roots: pool(ca), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("client cert verified for serverAuth")
	}
	if _, err := ca.ClientCert("Node2." + strings.ToUpper(testHost)); err != nil {
		t.Errorf("mixed case: %v", err)
	}
	for _, bad := range []string{"example.com", testHost, "xathena.tailnet.example", "", "*." + testHost, "a b." + testHost} {
		if _, err := ca.ClientCert(bad); err == nil {
			t.Errorf("ClientCert(%q) accepted", bad)
		}
	}
}

func TestMobileConfigParses(t *testing.T) {
	ca := newTestCA(t)
	a, b := MobileConfig(ca), MobileConfig(ca)
	if string(a) != string(b) {
		t.Fatal("MobileConfig is not deterministic")
	}

	// Flatten the plist into alternating key/value strings per dict, using only encoding/xml.
	dec := xml.NewDecoder(strings.NewReader(string(a)))
	var texts []string
	var elem string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch v := tok.(type) {
		case xml.StartElement:
			elem = v.Name.Local
		case xml.CharData:
			if s := strings.TrimSpace(string(v)); s != "" && (elem == "key" || elem == "string" || elem == "data") {
				texts = append(texts, elem+"="+s)
			}
		}
	}
	has := func(s string) bool {
		for _, x := range texts {
			if x == s {
				return true
			}
		}
		return false
	}
	if !has("string=com.apple.security.root") || !has("string=Configuration") {
		t.Fatalf("payload types missing: %v", texts)
	}
	if !has("string=Athena CA (" + testHost + ")") {
		t.Fatalf("display name missing: %v", texts)
	}
	if !has("data=" + base64.StdEncoding.EncodeToString(ca.cert.Raw)) {
		t.Fatalf("DER payload missing: %v", texts)
	}
	var uuids []string
	for i, x := range texts {
		if x == "key=PayloadUUID" {
			uuids = append(uuids, texts[i+1])
		}
	}
	if len(uuids) != 2 || uuids[0] == uuids[1] {
		t.Fatalf("UUIDs: %v", uuids)
	}
}

func TestReloadedCASigns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	if _, err := LoadOrCreateCA(dir, testHost, testIPs); err != nil {
		t.Fatal(err)
	}
	ca, err := LoadOrCreateCA(dir, testHost, testIPs)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.ServerCert(testHost, testIPs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Leaf.Verify(x509.VerifyOptions{Roots: pool(ca), DNSName: testHost}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRefusesDifferentHostOrIPs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	if _, err := LoadOrCreateCA(dir, testHost, testIPs); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateCA(dir, "new.example", testIPs)
	if err == nil || !strings.Contains(err.Error(), testHost) || !strings.Contains(err.Error(), "new.example") || !strings.Contains(err.Error(), "move") {
		t.Fatalf("host change: %v", err)
	}
	if _, err := LoadOrCreateCA(dir, testHost, testIPs[:1]); err == nil {
		t.Fatal("IP change accepted")
	}
	// Order of IPs does not matter.
	if _, err := LoadOrCreateCA(dir, strings.ToUpper(testHost), []net.IP{testIPs[1], testIPs[0]}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRefusesMismatchedCertAndKey(t *testing.T) {
	d1, d2 := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	for _, d := range []string{d1, d2} {
		if _, err := LoadOrCreateCA(d, testHost, testIPs); err != nil {
			t.Fatal(err)
		}
	}
	pemB, _ := os.ReadFile(filepath.Join(d2, "ca.pem"))
	if err := os.WriteFile(filepath.Join(d1, "ca.pem"), pemB, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCA(d1, testHost, testIPs); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRefusesWeakCACerts(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*x509.Certificate)
	}{
		{"not a CA", func(c *x509.Certificate) {
			c.IsCA = false
			c.MaxPathLenZero = false
			c.MaxPathLen = -1
			c.KeyUsage = x509.KeyUsageDigitalSignature
		}},
		{"expired", func(c *x509.Certificate) {
			c.NotBefore = time.Now().Add(-48 * time.Hour)
			c.NotAfter = time.Now().Add(-24 * time.Hour)
		}},
		{"no constraints", func(c *x509.Certificate) {
			c.PermittedDNSDomains = nil
			c.PermittedIPRanges = nil
			c.PermittedDNSDomainsCritical = false
		}},
		{"non-critical", func(c *x509.Certificate) { c.PermittedDNSDomainsCritical = false }},
	} {
		dir := filepath.Join(t.TempDir(), "ca")
		ca, err := LoadOrCreateCA(dir, testHost, testIPs)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := *ca.cert
		tmpl.Raw, tmpl.RawTBSCertificate, tmpl.Extensions, tmpl.ExtraExtensions = nil, nil, nil, nil
		tc.mod(&tmpl)
		der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &ca.key.PublicKey, ca.key)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateCA(dir, testHost, testIPs); err == nil {
			t.Errorf("%s: tampered ca.pem accepted", tc.name)
		}
	}
}

func TestIssueRefusesNamesOutsideConstraints(t *testing.T) {
	ca := newTestCA(t)
	if _, err := ca.ServerCert("example.com", nil); err == nil {
		t.Error("issued for a foreign DNS name")
	}
	if _, err := ca.ServerCert(testHost, []net.IP{net.ParseIP("203.0.113.9")}); err == nil {
		t.Error("issued for a foreign IP")
	}
}

func TestNoIPCARefusesIPLeaf(t *testing.T) {
	ca, err := LoadOrCreateCA(filepath.Join(t.TempDir(), "ca"), testHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.ServerCert(testHost, nil); err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"100.64.0.7", "fd7a:115c:a1e0::7"} {
		if _, err := ca.ServerCert(testHost, []net.IP{net.ParseIP(ip)}); err == nil {
			t.Errorf("no-IP CA issued for %s", ip)
		}
	}
}

func TestLoadOrCreateRejectsBadHost(t *testing.T) {
	for _, h := range []string{"", "100.64.0.7", "::1", "*.example", "a b"} {
		if _, err := LoadOrCreateCA(filepath.Join(t.TempDir(), "ca"), h, nil); err == nil {
			t.Errorf("host %q accepted", h)
		}
	}
}

func TestLoadCAIsReadOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	if ca, ok, err := LoadCA(dir, testHost, testIPs); ca != nil || ok || err != nil {
		t.Fatalf("no CA: %v %v %v", ca, ok, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("LoadCA created %s: %v", dir, err)
	}
	made, err := LoadOrCreateCA(dir, testHost, testIPs)
	if err != nil {
		t.Fatal(err)
	}
	ca, ok, err := LoadCA(dir, strings.ToUpper(testHost), testIPs)
	if err != nil || !ok || ca.Fingerprint() != made.Fingerprint() {
		t.Fatalf("load: %v %v", ok, err)
	}
	if _, ok, err := LoadCA(dir, "new.example", testIPs); ok || err == nil || !strings.Contains(err.Error(), "move") {
		t.Fatalf("host change: %v %v", ok, err)
	}
	if CertPath(dir) != filepath.Join(dir, "ca.pem") {
		t.Fatal(CertPath(dir))
	}
}

func csrFor(t *testing.T, key any, cn string) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: cn, Organization: []string{"Evil"}},
		DNSNames: []string{"evil.example", cn},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestSignClientCSR(t *testing.T) {
	ca, err := LoadOrCreateCA(NodeCADir(t.TempDir()), NodeCAHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	name := "n0123456789." + NodeCAHost
	der, serial, err := ca.SignClientCSR(csrFor(t, key, "whatever"), name)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool(ca), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("does not verify for client auth: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool(ca), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("verifies for server auth")
	}
	if cert.Subject.CommonName != name || len(cert.DNSNames) != 1 || cert.DNSNames[0] != name || len(cert.Subject.Organization) != 0 {
		t.Fatalf("subject %v, DNS %v: want only %s", cert.Subject, cert.DNSNames, name)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		t.Fatal("the certificate is not for the CSR's key")
	}
	if serial == "" || serial != cert.SerialNumber.Text(16) {
		t.Fatalf("serial %q, certificate's %s", serial, cert.SerialNumber.Text(16))
	}
	if life := cert.NotAfter.Sub(cert.NotBefore); life < leafValidity || life > leafValidity+clockSkew {
		t.Fatalf("life %s", life)
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ca.SignClientCSR(csrFor(t, rsaKey, name), name); err == nil {
		t.Fatal("an RSA CSR was signed")
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, _, err := ca.SignClientCSR(csrFor(t, p384, name), name); err == nil {
		t.Fatal("a P-384 CSR was signed")
	}
	bad := csrFor(t, key, name)
	bad[len(bad)-3] ^= 0xff // the signature
	if _, _, err := ca.SignClientCSR(bad, name); err == nil {
		t.Fatal("a CSR with a broken signature was signed")
	}
	for _, n := range []string{NodeCAHost, "a.b." + NodeCAHost, "evil.example", "*." + NodeCAHost, "x" + NodeCAHost} {
		if _, _, err := ca.SignClientCSR(csrFor(t, key, n), n); err == nil {
			t.Errorf("name %q was signed", n)
		}
	}
}
