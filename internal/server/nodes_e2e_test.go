package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
)

func postJoin(t *testing.T, client *http.Client, base, token string, csr []byte) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"token": token, "name": "box", "csr": csr})
	resp, err := client.Post(base+"/node/join", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%d %q: %v", resp.StatusCode, raw, err)
	}
	return resp.StatusCode, out
}

// TestNodeJoinAndListenerWired: with TLS on, a node joins on the dashboard port and reaches the node listener on its
// own port with the certificate it got.
func TestNodeJoinAndListenerWired(t *testing.T) {
	dir := newStateDir(t)
	w := startWeb(t, dir)
	if w.ep.nodeL == nil {
		t.Fatal("no node listener with TLS ca")
	}
	_, nodePort, _ := net.SplitHostPort(w.ep.nodeL.Addr().String())
	_, dashPort, _ := net.SplitHostPort(w.ep.l.Addr().String())
	if nodePort == dashPort {
		t.Fatal("the node listener is the dashboard's port")
	}
	tok, err := web.IssueEnrollToken(w.ep.st, store.EnrollNode, 0)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	code, ans := postJoin(t, w.client, w.base, tok, csr)
	if code != http.StatusOK {
		t.Fatalf("join: %d %v", code, ans)
	}
	if want := "wss://" + net.JoinHostPort(webHost, nodePort) + "/node"; ans["nodeURL"] != want {
		t.Fatalf("nodeURL %v, want %s", ans["nodeURL"], want)
	}
	var certDER []byte
	if err := json.Unmarshal([]byte(`"`+ans["cert"].(string)+`"`), &certDER); err != nil {
		t.Fatal(err)
	}
	tlsCfg := w.client.Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsCfg.Certificates = []tls.Certificate{{Certificate: [][]byte{certDER}, PrivateKey: key}}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	c, err := link.Dial(ctx, "wss://127.0.0.1:"+nodePort+"/node", tlsCfg)
	if err != nil {
		t.Fatalf("node dial: %v", err)
	}
	// The fleet serves the link: it waits for the node's Hello (nodes_test.go runs whole nodes over it).
	_ = c.Close(1000, "")
}

// TestNodeJoinRefusedWithTLSOff: with TLS off no node listener runs, and the join says why.
func TestNodeJoinRefusedWithTLSOff(t *testing.T) {
	dir := newStateDir(t)
	if err := SetupWeb(dir, WebConfig{Listen: "127.0.0.1:0", Host: "localhost", TLS: "off"}); err != nil {
		t.Fatal(err)
	}
	eps := make(chan *webEndpoint, 1)
	startServerWith(t, dir, func(c *Config) { c.web.ready = func(ep *webEndpoint) { eps <- ep } })
	ep := <-eps
	if ep.nodeL != nil || ep.nodeCA != nil {
		t.Fatal("node endpoint with TLS off")
	}
	tok, err := web.IssueEnrollToken(ep.st, store.EnrollNode, 0)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: waitTimeout, Transport: &http.Transport{DisableKeepAlives: true}}
	code, ans := postJoin(t, client, "http://"+ep.l.Addr().String(), tok, []byte("x"))
	if code != http.StatusForbidden || ans["error"] != "Nodes need the dashboard on TLS." {
		t.Fatalf("%d %v", code, ans)
	}
}
