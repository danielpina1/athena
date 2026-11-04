package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/pki"
)

const wantCSP = "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'"

// newServerHandler is a Server's handler over a signed-in connEnv; mod sets the CA, the VAPID key or plain HTTP.
func newServerHandler(t *testing.T, mod func(*ServerConfig)) (*connEnv, http.Handler) {
	t.Helper()
	e := newConnEnv(t)
	cfg := ServerConfig{Auth: e.Auth, Dashboard: e.dash, TLS: &tls.Config{}, Log: quietLog()}
	if mod != nil {
		mod(&cfg)
	}
	return e, NewServer(cfg).Handler()
}

func get(t *testing.T, h http.Handler, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSecurityHeaders(t *testing.T) {
	v, err := LoadOrCreateVAPID(t.TempDir(), testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadOrCreateCA(t.TempDir(), testRPID, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, h := newServerHandler(t, func(c *ServerConfig) { c.VAPID, c.CA = v, ca })
	for _, path := range []string{"/", "/enroll", "/app.js", "/healthz", "/auth/session", "/push/key", "/ca.pem", "/ca.mobileconfig", "/ws", "/nowhere"} {
		rec := get(t, h, path)
		hd := rec.Header()
		if got := hd.Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%s: CSP %q", path, got)
		}
		if got := hd.Get("Strict-Transport-Security"); got != "max-age=31536000" {
			t.Errorf("%s: HSTS %q", path, got)
		}
		if got := hd.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options %q", path, got)
		}
		if got := hd.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy %q", path, got)
		}
	}
	// The pages load their one script from the origin and run nothing inline, so the CSP holds them.
	for _, path := range []string{"/", "/enroll"} {
		rec := get(t, h, path)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(body, `<script src="/app.js"`) || strings.Count(body, "<script") != 1 || strings.Contains(body, "style=") || strings.Contains(body, "onclick") {
			t.Fatalf("%s: page is not CSP-clean:\n%s", path, body)
		}
	}
	if rec := get(t, h, "/app.js"); rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("app.js = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get(t, h, "/push/key"); rec.Code != http.StatusOK || rec.Body.String() != v.PublicKey() {
		t.Fatalf("push key = %d %q", rec.Code, rec.Body)
	}

	// Plain HTTP (mode off, loopback only) sends no HSTS: a browser would pin a scheme the endpoint does not speak.
	_, plain := newServerHandler(t, func(c *ServerConfig) { c.TLS = nil })
	rec := get(t, plain, "/")
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS over plain http: %q", got)
	}
	if rec.Header().Get("Content-Security-Policy") != wantCSP {
		t.Fatal("plain http lost the CSP")
	}
}

func TestHealthzLeaksNothing(t *testing.T) {
	e, h := newServerHandler(t, nil)
	session := &http.Cookie{Name: SessionCookie, Value: e.cookie}
	for _, cookies := range [][]*http.Cookie{nil, {session}} {
		rec := get(t, h, "/healthz", cookies...)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("healthz = %d %q", rec.Code, rec.Body)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("healthz set cookies %v", rec.Result().Cookies())
		}
		for k := range rec.Header() {
			switch k {
			case "Content-Type", "Cache-Control", "Content-Security-Policy", "Strict-Transport-Security", "X-Content-Type-Options", "Referrer-Policy":
			default:
				t.Fatalf("healthz sends header %s: %v", k, rec.Header()[k])
			}
		}
	}
	req := httptest.NewRequest("POST", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST healthz = %d", rec.Code)
	}
}

func TestCAEndpointsOnlyInCAMode(t *testing.T) {
	ca, err := pki.LoadOrCreateCA(t.TempDir(), testRPID, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, h := newServerHandler(t, func(c *ServerConfig) { c.CA = ca })
	rec := get(t, h, "/ca.pem")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-pem-file" {
		t.Fatalf("ca.pem = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	b, _ := pem.Decode(rec.Body.Bytes())
	if b == nil || b.Type != "CERTIFICATE" || !bytes.Equal(b.Bytes, ca.DER()) {
		t.Fatalf("ca.pem is not the CA: %q", rec.Body)
	}
	rec = get(t, h, "/ca.mobileconfig")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-apple-aspen-config" ||
		!bytes.Equal(rec.Body.Bytes(), pki.MobileConfig(ca)) {
		t.Fatalf("mobileconfig = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	// Modes acme and off have no CA to hand out.
	_, h = newServerHandler(t, nil)
	for _, path := range []string{"/ca.pem", "/ca.mobileconfig"} {
		if rec := get(t, h, path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s without a CA = %d", path, rec.Code)
		}
	}
	// No push key without VAPID (an http origin has none).
	if rec := get(t, h, "/push/key"); rec.Code != http.StatusNotFound {
		t.Fatalf("push key without VAPID = %d", rec.Code)
	}
}

// TestServerShutdownClosesConnections: Shutdown stops the listener and ends the open dashboard connections, which
// the HTTP server does not track once upgraded.
func TestServerShutdownClosesConnections(t *testing.T) {
	e := newConnEnv(t)
	s := NewServer(ServerConfig{Auth: e.Auth, Dashboard: e.dash, Log: quietLog()})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(l) }()
	h := http.Header{}
	h.Set("Origin", testOrigin)
	h.Set("Cookie", SessionCookie+"="+e.cookie)
	ctx, cancel := context.WithTimeout(context.Background(), connWait)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+l.Addr().String()+"/ws", &websocket.DialOptions{HTTPHeader: h, Host: testRPID,
		Subprotocols: []string{link.Subprotocol}, CompressionMode: websocket.CompressionNoContextTakeover})
	if err != nil {
		t.Fatal(err)
	}
	c := startClient(t, ws, &athenav1.Hello{Protocol: Protocol, ClientId: "phone"})
	c.next("welcome", func(f *athenav1.Frame) bool { return f.GetWelcome() != nil })
	if n := e.dash.Connections(); n != 1 {
		t.Fatalf("%d connections", n)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve = %v", err)
	}
	select {
	case err := <-c.done:
		c.done <- err // for the cleanup's close
		if code := websocket.CloseStatus(err); code != websocket.StatusGoingAway {
			t.Fatalf("closed with %d (%v)", code, err)
		}
	case <-time.After(connWait):
		t.Fatal("connection open after Shutdown")
	}
	if n := e.dash.Connections(); n != 0 {
		t.Fatalf("%d connections after Shutdown", n)
	}
	if resp, err := http.Get("http://" + l.Addr().String() + "/healthz"); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		t.Fatal("listener still serving")
	}
}
