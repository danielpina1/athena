package nodelink

import (
	"crypto/ecdsa"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/danielpina1/athena/internal/pki"
)

// joinServer is a dashboard with a CA, serving /ca.pem and /node/join; it records the join's body.
func joinServer(t *testing.T) (srv *httptest.Server, dash *pki.CA, body func() []byte) {
	t.Helper()
	ips := []net.IP{net.ParseIP("127.0.0.1")}
	dash, err := pki.LoadOrCreateCA(filepath.Join(t.TempDir(), "dash"), "athena.test", ips)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := pki.LoadOrCreateCA(filepath.Join(t.TempDir(), "nodes"), pki.NodeCAHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := dash.ServerCert("athena.test", ips)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: dash.DER()}))
	})
	mux.HandleFunc("POST /node/join", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = b
		mu.Unlock()
		var req struct {
			Token, Name string
			CSR         []byte
		}
		if err := json.Unmarshal(b, &req); err != nil || req.Token != "tok" {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "forbidden", "error": "This join token is not valid."})
			return
		}
		der, _, err := nodes.SignClientCSR(req.CSR, "n0123456789."+pki.NodeCAHost)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"nodeId": "n0123456789", "name": req.Name, "cert": der,
			"caCert": nodes.DER(), "nodeURL": "wss://athena.test:8444/node"})
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, dash, func() []byte { mu.Lock(); defer mu.Unlock(); return got }
}

func TestJoinWritesKey0600AndNeverSendsKey(t *testing.T) {
	srv, dash, body := joinServer(t)
	dir := filepath.Join(t.TempDir(), "state")
	id, err := Join(t.Context(), srv.URL, "tok", "mini", dash.Fingerprint(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.NodeID != "n0123456789" || id.Name != "mini" || id.NodeURL != "wss://athena.test:8444/node" || id.CAPool == nil {
		t.Fatalf("identity = %+v", id)
	}
	b := body()
	if len(b) == 0 || strings.Contains(string(b), "PRIVATE KEY") {
		t.Fatalf("join body: %s", b)
	}
	keyPath := filepath.Join(dir, keyFile)
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v", fi.Mode().Perm())
	}
	// The key's own bytes are nowhere in the body either.
	k := id.Cert.PrivateKey.(*ecdsa.PrivateKey)
	d, _ := json.Marshal(k.D.Bytes())
	if strings.Contains(string(b), strings.Trim(string(d), `"`)) {
		t.Fatal("the private scalar was sent")
	}

	back, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back.NodeID != id.NodeID || back.ServerFingerprint != dash.Fingerprint() || back.CAPool == nil ||
		!back.Cert.Leaf.PublicKey.(*ecdsa.PublicKey).Equal(&k.PublicKey) {
		t.Fatalf("loaded = %+v", back)
	}
	if _, err := Join(t.Context(), srv.URL, "tok", "mini", dash.Fingerprint(), dir); err == nil {
		t.Fatal("joined twice into one state directory")
	}
}

func TestJoinRefusalsWriteNothing(t *testing.T) {
	srv, dash, _ := joinServer(t)
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := Join(t.Context(), srv.URL, "tok", "mini", strings.Repeat("AB:", 31)+"AB", dir); err != ErrFingerprint {
		t.Fatalf("wrong fingerprint err = %v", err)
	}
	if _, err := Join(t.Context(), srv.URL, "bad", "mini", dash.Fingerprint(), dir); err == nil ||
		err.Error() != "This join token is not valid." {
		t.Fatalf("refused join err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, identityFile)); !os.IsNotExist(err) {
		t.Fatalf("node.json after refusals: %v", err)
	}
	if _, err := LoadIdentity(dir); err != ErrNotJoined {
		t.Fatalf("LoadIdentity err = %v", err)
	}
}
