package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
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
	for _, bad := range []string{"example.com", testHost, "xathena.tailnet.example", ""} {
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
