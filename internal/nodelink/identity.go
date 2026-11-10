// Package nodelink is a node's side of its link to the control server (plan 6): the identity it joined with, the
// dial loop, the outbox of facts, the requests, and the proxies that give a node.Node its Engine, Credentials and
// Policy over the link.
package nodelink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The files of a joined node in its state directory. The key is made here and never leaves the machine.
const (
	identityFile = "node.json"
	keyFile      = "node.key"
	certFile     = "node.pem"
	// nodeCAFile is the node CA that signed the node's certificate (kept for reference).
	nodeCAFile = "node-ca.pem"
	// serverCAFile is the dashboard CA the server is verified with; absent when the system roots are (ACME).
	serverCAFile = "server-ca.pem"
)

var (
	// ErrNotJoined is LoadIdentity on a state directory with no node.json.
	ErrNotJoined = errors.New("This machine has not joined yet; run athena node join first.")
	// ErrFingerprint is a join whose server CA does not have the fingerprint given.
	ErrFingerprint = errors.New("The server's certificate does not match --ca-sha256; check the command you copied.")
)

// joinTimeout bounds each HTTP step of a join.
const joinTimeout = 30 * time.Second

// Identity is a joined node: its id and name, where its link goes, the server CA's fingerprint ("" for the system
// roots), its client certificate and the roots it verifies the server with (nil: the system roots).
type Identity struct {
	NodeID, Name, NodeURL, ServerFingerprint string
	Cert                                     tls.Certificate
	CAPool                                   *x509.CertPool
}

// TLSConfig is the client TLS configuration of the node's link.
func (id *Identity) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: id.CAPool, Certificates: []tls.Certificate{id.Cert}}
}

type identityJSON struct {
	NodeID            string `json:"nodeId"`
	Name              string `json:"name"`
	NodeURL           string `json:"nodeURL"`
	ServerFingerprint string `json:"serverFingerprint,omitempty"`
}

// LoadIdentity reads a joined node's identity from dir. A directory with no node.json is ErrNotJoined; a key open to
// group or others is refused.
func LoadIdentity(dir string) (*Identity, error) {
	b, err := os.ReadFile(filepath.Join(dir, identityFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotJoined
	}
	if err != nil {
		return nil, fmt.Errorf("nodelink: %w", err)
	}
	var j identityJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("nodelink: %s: %w", identityFile, err)
	}
	keyPath := filepath.Join(dir, keyFile)
	fi, err := os.Stat(keyPath)
	if err != nil {
		return nil, fmt.Errorf("nodelink: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("nodelink: %s is readable by others (mode %v); chmod 600 it", keyPath, fi.Mode().Perm())
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, certFile), keyPath)
	if err != nil {
		return nil, fmt.Errorf("nodelink: certificate: %w", err)
	}
	id := &Identity{NodeID: j.NodeID, Name: j.Name, NodeURL: j.NodeURL, ServerFingerprint: j.ServerFingerprint, Cert: cert}
	if j.ServerFingerprint != "" {
		pemBytes, err := os.ReadFile(filepath.Join(dir, serverCAFile))
		if err != nil {
			return nil, fmt.Errorf("nodelink: %w", err)
		}
		ca, err := parseCAPEM(pemBytes)
		if err != nil {
			return nil, err
		}
		if !sameFingerprint(Fingerprint(ca.Raw), j.ServerFingerprint) {
			return nil, fmt.Errorf("nodelink: %s does not match the fingerprint in %s", serverCAFile, identityFile)
		}
		id.CAPool = x509.NewCertPool()
		id.CAPool.AddCert(ca)
	}
	return id, nil
}

// Fingerprint is a certificate's SHA-256 as pki.(*CA).Fingerprint writes it: upper-case hex pairs joined by colons.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	var b strings.Builder
	for i, v := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", v)
	}
	return b.String()
}

// sameFingerprint compares two fingerprints ignoring case, colons and spaces.
func sameFingerprint(a, b string) bool {
	norm := func(s string) string {
		return strings.ToUpper(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s)))
	}
	return norm(a) != "" && norm(a) == norm(b)
}

func parseCAPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("nodelink: the server's CA is not a PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// Join enrolls this machine with the server at url (the dashboard's https URL) using a join token, and writes the
// identity to dir. fingerprint is the dashboard CA's SHA-256: the CA is fetched from <url>/ca.pem and must match it,
// and the server is then verified against that CA alone. Empty means the system roots (ACME). The key is made here,
// P-256, written 0600, and only a certificate request is sent. Nothing is written unless the server enrolled the node.
func Join(ctx context.Context, url, token, name, fingerprint, dir string) (*Identity, error) {
	url = strings.TrimRight(url, "/")
	if _, err := os.Stat(filepath.Join(dir, identityFile)); err == nil {
		return nil, fmt.Errorf("this machine has already joined (%s exists); use another --state directory", filepath.Join(dir, identityFile))
	}
	var pool *x509.CertPool
	var caPEM []byte
	if fingerprint != "" {
		var err error
		caPEM, err = fetchCA(ctx, url)
		if err != nil {
			return nil, err
		}
		ca, err := parseCAPEM(caPEM)
		if err != nil {
			return nil, err
		}
		if !sameFingerprint(Fingerprint(ca.Raw), fingerprint) {
			return nil, ErrFingerprint
		}
		pool = x509.NewCertPool()
		pool.AddCert(ca)
		fingerprint = Fingerprint(ca.Raw)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Token string `json:"token"`
		Name  string `json:"name"`
		CSR   []byte `json:"csr"`
	}{token, name, csr})
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: joinTimeout, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/node/join", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("join %s: %w", url, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("join %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(rb, &refusal) == nil && refusal.Error != "" {
			return nil, errors.New(refusal.Error)
		}
		return nil, fmt.Errorf("join %s: %s", url, resp.Status)
	}
	var ans struct {
		NodeID  string `json:"nodeId"`
		Name    string `json:"name"`
		Cert    []byte `json:"cert"`
		CACert  []byte `json:"caCert"`
		NodeURL string `json:"nodeURL"`
	}
	if err := json.Unmarshal(rb, &ans); err != nil {
		return nil, fmt.Errorf("join %s: the answer: %w", url, err)
	}
	leaf, err := x509.ParseCertificate(ans.Cert)
	if err != nil {
		return nil, fmt.Errorf("join %s: the certificate: %w", url, err)
	}
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return nil, fmt.Errorf("join %s: the certificate is not for this machine's key", url)
	}
	if ans.NodeID == "" || ans.NodeURL == "" {
		return nil, fmt.Errorf("join %s: the answer has no node id or node URL", url)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ans.Cert})
	j, err := json.MarshalIndent(identityJSON{NodeID: ans.NodeID, Name: ans.Name, NodeURL: ans.NodeURL,
		ServerFingerprint: fingerprint}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	type namedFile struct {
		name string
		data []byte
	}
	files := []namedFile{{keyFile, keyPEM}, {certFile, certPEM}}
	if len(ans.CACert) > 0 {
		files = append(files, namedFile{nodeCAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ans.CACert})})
	}
	if caPEM != nil {
		files = append(files, namedFile{serverCAFile, caPEM})
	}
	// node.json last: it is what makes the directory joined.
	files = append(files, namedFile{identityFile, append(j, '\n')})
	for _, f := range files {
		if err := writeFileAtomic(filepath.Join(dir, f.name), f.data); err != nil {
			return nil, fmt.Errorf("nodelink: write %s: %w", f.name, err)
		}
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &Identity{NodeID: ans.NodeID, Name: ans.Name, NodeURL: ans.NodeURL, ServerFingerprint: fingerprint,
		Cert: cert, CAPool: pool}, nil
}

// fetchCA reads <url>/ca.pem. The connection is not verified, as nothing is trusted yet: the caller trusts the CA only
// once its fingerprint matches the one the owner copied.
func fetchCA(ctx context.Context, url string) ([]byte, error) {
	client := &http.Client{Timeout: joinTimeout, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}} //nolint:gosec // checked by fingerprint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/ca.pem", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch the server's CA: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch the server's CA: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

// writeFileAtomic writes data to path, mode 0600, through a temporary file renamed over it.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
