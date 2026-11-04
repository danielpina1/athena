package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
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
	for _, path := range []string{"/", "/enroll", "/nope.js", "/healthz", "/auth/session", "/push/key", "/ca.pem", "/ca.mobileconfig", "/ws", "/nowhere"} {
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
	if rec := get(t, h, "/push/key"); rec.Code != http.StatusOK || rec.Body.String() != v.PublicKey() {
		t.Fatalf("push key = %d %q", rec.Code, rec.Body)
	}
	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest("POST", "/push/key", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST push key = %d", post.Code)
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

// recordLevels is a slog handler that keeps each record's level and message.
type recordLevels struct {
	mu   sync.Mutex
	recs map[string]slog.Level
}

func (r *recordLevels) Enabled(context.Context, slog.Level) bool { return true }
func (r *recordLevels) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs[rec.Message] = rec.Level
	return nil
}
func (r *recordLevels) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recordLevels) WithGroup(string) slog.Handler      { return r }

// TestServerErrorLogLevels: the http.Server's handshake noise is debug, a handler's panic a warning.
func TestServerErrorLogLevels(t *testing.T) {
	e := newConnEnv(t)
	rl := &recordLevels{recs: map[string]slog.Level{}}
	s := NewServer(ServerConfig{Auth: e.Auth, Dashboard: e.dash, TLS: &tls.Config{}, Log: slog.New(rl)})
	s.srv.ErrorLog.Printf("http: TLS handshake error from 10.0.0.9:5555: EOF")
	s.srv.ErrorLog.Printf("http: panic serving 10.0.0.9:5556: boom")
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if lv, ok := rl.recs["http: TLS handshake error from 10.0.0.9:5555: EOF"]; !ok || lv != slog.LevelDebug {
		t.Fatalf("handshake error logged at %v (%v)", lv, ok)
	}
	if lv, ok := rl.recs["http: panic serving 10.0.0.9:5556: boom"]; !ok || lv != slog.LevelWarn {
		t.Fatalf("panic logged at %v (%v): %v", lv, ok, rl.recs)
	}
}

var spaFS = fstest.MapFS{
	"index.html":           {Data: []byte(`<!doctype html><script type="module" src="/assets/index-abc.js"></script><div id="root"></div>`)},
	"assets/index-abc.js":  {Data: []byte("console.log(1)")},
	"assets/index-abc.css": {Data: []byte("a{}")},
	".vite/manifest.json":  {Data: []byte("{}")},
	".gitkeep":             {Data: []byte("")},
	"sw.js":                {Data: []byte("self")},
	"manifest.webmanifest": {Data: []byte("{}")},
}

func TestSPAServesAssetsImmutable(t *testing.T) {
	_, h := newServerHandler(t, func(c *ServerConfig) { c.UI = spaFS })
	for path, ct := range map[string]string{"/assets/index-abc.js": "text/javascript", "/assets/index-abc.css": "text/css"} {
		rec := get(t, h, path)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || !strings.HasPrefix(rec.Header().Get("Content-Type"), ct) {
			t.Fatalf("%s = %d %v", path, rec.Code, rec.Header())
		}
	}
	for path, ct := range map[string]string{"/sw.js": "text/javascript", "/manifest.webmanifest": "application/manifest+json"} {
		rec := get(t, h, path)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" || !strings.HasPrefix(rec.Header().Get("Content-Type"), ct) {
			t.Fatalf("%s = %d %v", path, rec.Code, rec.Header())
		}
	}
}

func TestSPAFallbackServesIndexNoCache(t *testing.T) {
	_, h := newServerHandler(t, func(c *ServerConfig) { c.UI = spaFS })
	for _, path := range []string{"/", "/tickets/T-1", "/enroll"} {
		rec := get(t, h, path)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), `id="root"`) {
			t.Fatalf("%s = %d %v %q", path, rec.Code, rec.Header(), rec.Body)
		}
	}
}

func TestSPAUnknownFileIs404(t *testing.T) {
	_, h := newServerHandler(t, func(c *ServerConfig) { c.UI = spaFS })
	for _, path := range []string{"/nope.js", "/assets/missing.css", "/app.js"} {
		if rec := get(t, h, path); rec.Code != 404 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
}

func TestUnbuiltServesFallbackPage(t *testing.T) {
	_, h := newServerHandler(t, nil)
	for _, path := range []string{"/", "/enroll", "/tickets/T-1"} {
		rec := get(t, h, path)
		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(body, "Dashboard not built: run make web") || strings.Contains(body, "<script") {
			t.Fatalf("%s = %d %q", path, rec.Code, body)
		}
	}
}

func TestCSPUnchanged(t *testing.T) {
	if contentSecurityPolicy != wantCSP {
		t.Fatalf("CSP changed: %q", contentSecurityPolicy)
	}
	_, h := newServerHandler(t, func(c *ServerConfig) { c.UI = spaFS })
	if got := get(t, h, "/").Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("CSP header %q", got)
	}
}

func TestPlaceholderRoutesGone(t *testing.T) {
	_, h := newServerHandler(t, func(c *ServerConfig) { c.UI = spaFS })
	if rec := get(t, h, "/app.js"); rec.Code != 404 {
		t.Fatalf("/app.js = %d", rec.Code)
	}
}

func TestSPARefusesDotfilesDirectoriesAndTraversal(t *testing.T) {
	_, h := newServerHandler(t, func(c *ServerConfig) { c.UI = spaFS })
	for _, path := range []string{"/.gitkeep", "/.vite/manifest.json", "/.vite", "/assets/", "/assets", "/go.mod", "/assets/%2e%2e/%2e%2e/go.mod"} {
		if rec := get(t, h, path); rec.Code != 404 {
			t.Fatalf("%s = %d %q", path, rec.Code, rec.Body)
		}
	}
	// The mux cleans a traversal into a redirect to the cleaned path, which is itself a 404.
	rec := get(t, h, "/assets/../../go.mod")
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/go.mod" {
		t.Fatalf("traversal = %d %v", rec.Code, rec.Header())
	}
}
