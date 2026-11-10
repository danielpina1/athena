package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/pki"
	"github.com/danielpina1/athena/internal/store"
)

const testNodeURL = "wss://athena.test:8444/node"

type nodeEnv struct {
	st    *store.Store
	ca    *pki.CA
	mu    sync.Mutex
	clock time.Time
	join  http.Handler
}

func (e *nodeEnv) now() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.clock }
func (e *nodeEnv) advance(d time.Duration) {
	e.mu.Lock()
	e.clock = e.clock.Add(d)
	e.mu.Unlock()
}

func newNodeEnv(t *testing.T) *nodeEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ca, err := pki.LoadOrCreateCA(pki.NodeCADir(dir), pki.NodeCAHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &nodeEnv{st: st, ca: ca, clock: time.Now().UTC().Truncate(time.Second)}
	st.SetClock(e.now)
	e.join = NodeJoin(st, ca, e.now, testNodeURL)
	return e
}

func (e *nodeEnv) token(t *testing.T, kind string) string {
	t.Helper()
	tok, err := IssueEnrollToken(e.st, kind, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func newNodeKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func nodeCSR(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "node"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

type testJoinAnswer struct {
	NodeID  string `json:"nodeId"`
	Name    string `json:"name"`
	Cert    []byte `json:"cert"`
	CACert  []byte `json:"caCert"`
	NodeURL string `json:"nodeURL"`
	Error   string `json:"error"`
}

func (e *nodeEnv) post(t *testing.T, token, name string, csr []byte) (int, testJoinAnswer) {
	t.Helper()
	body := mustJSON(t, map[string]any{"token": token, "name": name, "csr": csr})
	rec := httptest.NewRecorder()
	e.join.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/node/join", bytes.NewReader(body)))
	var a testJoinAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("answer %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	return rec.Code, a
}

func (e *nodeEnv) nodeCount(t *testing.T) int {
	t.Helper()
	ns, err := e.st.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	return len(ns)
}

// joined enrolls a node named name and returns its id and a client certificate for its key.
func (e *nodeEnv) joined(t *testing.T, name string) (string, tls.Certificate) {
	t.Helper()
	key := newNodeKey(t)
	code, a := e.post(t, e.token(t, store.EnrollNode), name, nodeCSR(t, key))
	if code != http.StatusOK {
		t.Fatalf("join: %d %q", code, a.Error)
	}
	leaf, err := x509.ParseCertificate(a.Cert)
	if err != nil {
		t.Fatal(err)
	}
	return a.NodeID, tls.Certificate{Certificate: [][]byte{a.Cert}, PrivateKey: key, Leaf: leaf}
}

func TestJoinIssuesCertOnce(t *testing.T) {
	e := newNodeEnv(t)
	tok := e.token(t, store.EnrollNode)
	key := newNodeKey(t)
	code, a := e.post(t, tok, "build-box", nodeCSR(t, key))
	if code != http.StatusOK {
		t.Fatalf("join: %d %q", code, a.Error)
	}
	if !regexp.MustCompile(`^n[0-9a-f]{10}$`).MatchString(a.NodeID) || a.Name != "build-box" || a.NodeURL != testNodeURL {
		t.Fatalf("answer %+v", a)
	}
	cert, err := x509.ParseCertificate(a.Cert)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(a.CACert)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("certificate does not verify against the answer's CA: %v", err)
	}
	if !bytes.Equal(a.CACert, e.ca.DER()) {
		t.Fatal("caCert is not the node CA")
	}
	if cert.Subject.CommonName != a.NodeID+"."+pki.NodeCAHost {
		t.Fatalf("CN %q", cert.Subject.CommonName)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		t.Fatal("the certificate is not for the node's own key")
	}
	n, err := e.st.GetNode(a.NodeID)
	if err != nil || n == nil {
		t.Fatalf("node row: %v %v", n, err)
	}
	if n.Name != "build-box" || n.CertSerial != cert.SerialNumber.Text(16) || n.RevokedAt != nil || !n.CreatedAt.Equal(e.now()) {
		t.Fatalf("node row %+v", n)
	}

	code, a = e.post(t, tok, "other-box", nodeCSR(t, newNodeKey(t)))
	if code != http.StatusForbidden || a.Error != "This join token was already used; create a new one." || a.Cert != nil {
		t.Fatalf("second use: %d %+v", code, a)
	}
	if e.nodeCount(t) != 1 {
		t.Fatal("the second use made a node")
	}
}

func TestJoinRefusals(t *testing.T) {
	e := newNodeEnv(t)
	if err := e.st.PutNode(store.Node{ID: "n00000000aa", Name: "taken", CreatedAt: e.now()}); err != nil {
		t.Fatal(err)
	}
	spent := e.token(t, store.EnrollNode)
	if ok, err := e.st.UseEnrollToken(hashMust(t, spent), store.EnrollNode, e.now()); err != nil || !ok {
		t.Fatalf("spend: %v %v", ok, err)
	}
	late, err := IssueEnrollToken(e.st, store.EnrollNode, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	passkey := e.token(t, store.EnrollPasskey)
	good := e.token(t, store.EnrollNode)
	e.advance(2 * time.Minute) // late is past its minute, the others within their hour
	random, _, _ := newSecret()

	cases := []struct {
		name, token, node string
		status            int
		want              string
	}{
		{"used", spent, "box-a", http.StatusForbidden, "This join token was already used; create a new one."},
		{"expired", late, "box-b", http.StatusForbidden, "This join token has expired; create a new one."},
		{"unknown", random, "box-c", http.StatusForbidden, "This join token is not valid."},
		{"garbage", "not-a-token", "box-d", http.StatusForbidden, "This join token is not valid."},
		{"passkey kind", passkey, "box-e", http.StatusForbidden, "This join token is not valid."},
		{"name taken", good, "taken", http.StatusBadRequest, "A node named taken already exists."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, a := e.post(t, c.token, c.node, nodeCSR(t, newNodeKey(t)))
			if code != c.status || a.Error != c.want || a.Cert != nil {
				t.Fatalf("got %d %q, want %d %q", code, a.Error, c.status, c.want)
			}
			if n := e.nodeCount(t); n != 1 {
				t.Fatalf("%d nodes after a refusal", n)
			}
		})
	}
	// A refusal for the name does not spend the token: the owner retries with another name.
	if code, a := e.post(t, good, "free", nodeCSR(t, newNodeKey(t))); code != http.StatusOK {
		t.Fatalf("retry after a taken name: %d %q", code, a.Error)
	}
	// A taken name is not told to a caller without a valid token.
	if _, a := e.post(t, random, "taken", nodeCSR(t, newNodeKey(t))); a.Error != "This join token is not valid." {
		t.Fatalf("name check before token check: %q", a.Error)
	}
	// A bad name or CSR is refused without spending the token.
	tok := e.token(t, store.EnrollNode)
	for _, bad := range []string{"", "Upper", "1starts-with-digit", "has space", strings.Repeat("a", 33), "dot.ted"} {
		if code, a := e.post(t, tok, bad, nodeCSR(t, newNodeKey(t))); code != http.StatusBadRequest || a.Cert != nil {
			t.Errorf("name %q: %d %q", bad, code, a.Error)
		}
	}
	if code, a := e.post(t, tok, "ok-name", []byte("not a csr")); code != http.StatusBadRequest || a.Cert != nil {
		t.Errorf("bad csr: %d %q", code, a.Error)
	}
	if code, a := e.post(t, tok, strings.Repeat("a", 32), nodeCSR(t, newNodeKey(t))); code != http.StatusOK {
		t.Fatalf("32-character name after refusals: %d %q", code, a.Error)
	}
}

func TestJoinNeedsTLS(t *testing.T) {
	e := newNodeEnv(t)
	e.join = NodeJoin(e.st, nil, e.now, "")
	code, a := e.post(t, e.token(t, store.EnrollNode), "box", nodeCSR(t, newNodeKey(t)))
	if code != http.StatusForbidden || a.Error != "Nodes need the dashboard on TLS." {
		t.Fatalf("%d %q", code, a.Error)
	}
}

func hashMust(t *testing.T, tok string) []byte {
	t.Helper()
	h, ok := hashOf(tok)
	if !ok {
		t.Fatal("bad token")
	}
	return h
}

type recordingNodes struct{ ids chan string }

func (r *recordingNodes) ServeNode(nodeID string, c link.Conn) {
	r.ids <- nodeID
	_ = c.Close(link.CloseProtocol, "test done")
}

// startNodeListener serves the node listener on loopback with a dashboard certificate for athena.test from its own CA,
// and returns its address and the roots a node trusts for the server.
func startNodeListener(t *testing.T, e *nodeEnv, h NodeLinkHandler) (string, *x509.CertPool) {
	t.Helper()
	dash, err := pki.LoadOrCreateCA(filepath.Join(t.TempDir(), "pki"), testHost, testIPs)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := dash.ServerCert(testHost, testIPs)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NodeListener(NodeListenerConfig{Store: e.st, CA: e.ca, Handler: h, Log: quietLog(),
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &serverCert, nil }})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(l, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	roots := x509.NewCertPool()
	dc, _ := x509.ParseCertificate(dash.DER())
	roots.AddCert(dc)
	return l.Addr().String(), roots
}

func dialNode(addr string, roots *x509.CertPool, cert *tls.Certificate) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := &tls.Config{RootCAs: roots, ServerName: testHost}
	if cert != nil {
		// Sent whatever the server's list of acceptable CAs says: the server, not the client, must refuse it.
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil }
	}
	c, err := link.Dial(ctx, "wss://"+addr+"/node", cfg)
	if err != nil {
		return err
	}
	_, err = c.Read(ctx) // the test handler closes at once
	_ = c.Close(1000, "")
	if err != nil && strings.Contains(err.Error(), "test done") {
		return nil
	}
	return err
}

func TestRevokedNodeRefused(t *testing.T) {
	e := newNodeEnv(t)
	rec := &recordingNodes{ids: make(chan string, 4)}
	addr, roots := startNodeListener(t, e, rec)
	id, cert := e.joined(t, "box")

	if err := dialNode(addr, roots, &cert); err != nil {
		t.Fatalf("enrolled node refused: %v", err)
	}
	if got := <-rec.ids; got != id {
		t.Fatalf("ServeNode got %q, want %q", got, id)
	}

	// No certificate, one from another CA, and a second certificate for the same id (serial mismatch) are refused.
	if err := dialNode(addr, roots, nil); err == nil {
		t.Fatal("a node without a certificate got through")
	}
	other, err := pki.LoadOrCreateCA(filepath.Join(t.TempDir(), "other"), pki.NodeCAHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := other.ClientCert(id + "." + pki.NodeCAHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := dialNode(addr, roots, &forged); err == nil {
		t.Fatal("a certificate from another CA got through")
	}
	// A self-signed copy of the node's certificate (same name, same serial) fails the chain check itself.
	selfKey := newNodeKey(t)
	selfDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: cert.Leaf.SerialNumber, Subject: cert.Leaf.Subject, DNSNames: cert.Leaf.DNSNames,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "self"}}, &selfKey.PublicKey, selfKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := dialNode(addr, roots, &tls.Certificate{Certificate: [][]byte{selfDER}, PrivateKey: selfKey}); err == nil {
		t.Fatal("a self-signed certificate with the node's serial got through")
	}
	key := newNodeKey(t)
	der, _, err := e.ca.SignClientCSR(nodeCSR(t, key), id+"."+pki.NodeCAHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := dialNode(addr, roots, &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}); err == nil {
		t.Fatal("a certificate with another serial got through")
	}
	unknownDER, _, err := e.ca.SignClientCSR(nodeCSR(t, key), "n0000000000."+pki.NodeCAHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := dialNode(addr, roots, &tls.Certificate{Certificate: [][]byte{unknownDER}, PrivateKey: key}); err == nil {
		t.Fatal("a certificate for an unknown node got through")
	}

	if err := e.st.RevokeNode(id, e.now()); err != nil {
		t.Fatal(err)
	}
	if err := dialNode(addr, roots, &cert); err == nil {
		t.Fatal("a revoked node got through")
	}
	select {
	case got := <-rec.ids:
		t.Fatalf("ServeNode called for %q after the first", got)
	default:
	}
}

// asksClientCert reports whether the TLS server at addr sends a CertificateRequest.
func asksClientCert(t *testing.T, addr string) bool {
	t.Helper()
	asked := false
	c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: testHost,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			asked = true
			return &tls.Certificate{}, nil
		}})
	if err == nil {
		_ = c.Close()
	}
	return asked
}

func TestDashboardPortAsksNoClientCert(t *testing.T) {
	e := newNodeEnv(t)
	ce := newConnEnv(t)
	m, err := NewTLSManager(t.TempDir(), caConfig(), nil, quietLog(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(ServerConfig{Auth: ce.Auth, Dashboard: ce.dash, TLS: m.Config(), Log: quietLog(), NodeJoin: e.join})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(l) }()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if asksClientCert(t, l.Addr().String()) {
		t.Fatal("the dashboard port asked for a client certificate")
	}
	// The join is on the dashboard listener.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/node/join", strings.NewReader("{}")))
	if rec.Code == http.StatusNotFound {
		t.Fatal("POST /node/join is not served on the dashboard")
	}
	// The detection works: the node listener does ask.
	addr, _ := startNodeListener(t, e, &recordingNodes{ids: make(chan string, 1)})
	if !asksClientCert(t, addr) {
		t.Fatal("the node listener did not ask for a client certificate")
	}
}
