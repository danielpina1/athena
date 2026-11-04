package web

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/danielpina1/athena/internal/pki"
)

// The dashboard's HTTPS endpoint: the placeholder pages, the login, the WebSocket, the push key, the CA for devices
// to install and a health check, every response with the same security headers.

// assets are the placeholder pages; plan 4's dashboard replaces them.
//
//go:embed assets
var assets embed.FS

// Security headers of every response.
const (
	contentSecurityPolicy = "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'"
	strictTransport       = "max-age=31536000"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	maxHeaderBytes    = 64 << 10
)

// ServerConfig is what a Server serves.
type ServerConfig struct {
	Auth      *Auth
	Dashboard *Dashboard
	// VAPID serves /push/key; nil (no https origin) leaves the route out.
	VAPID *VAPID
	// CA serves /ca.pem and /ca.mobileconfig, to anyone: it is public. Set it only in TLS mode ca.
	CA *pki.CA
	// TLS is the listener's configuration (TLSManager.Config); nil serves plain HTTP, which mode off allows on
	// loopback only, and leaves Strict-Transport-Security out.
	TLS *tls.Config
	Log *slog.Logger
}

// Server is the dashboard's HTTP server.
type Server struct {
	srv  *http.Server
	dash *Dashboard
	h    http.Handler
}

// NewServer builds the routes:
//
//	GET /, /enroll, /app.js   the placeholder pages (passkey login and enrollment, a live count of open Tickets)
//	GET /ws                   the dashboard connection (Dashboard.ServeConn)
//	/auth/*                   the login (Auth.Mount)
//	GET /push/key             the VAPID public key
//	GET /ca.pem, /ca.mobileconfig   the CA and its Apple profile (mode ca only)
//	GET /healthz              "ok", for a monitor: nothing else, no session needed
func NewServer(cfg ServerConfig) *Server {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	page := func(name, contentType string) http.HandlerFunc {
		body, err := assets.ReadFile("assets/" + name)
		if err != nil {
			panic("web: missing embedded asset " + name) // the embed directive lists them
		}
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(body)
		}
	}
	mux.HandleFunc("GET /{$}", page("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /enroll", page("enroll.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /app.js", page("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /ws", cfg.Dashboard.ServeConn)
	cfg.Auth.Mount(mux)
	if cfg.VAPID != nil {
		mux.Handle("/push/key", cfg.VAPID.Handler())
	}
	if ca := cfg.CA; ca != nil {
		caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.DER()})
		profile := pki.MobileConfig(ca)
		mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			w.Header().Set("Content-Disposition", `attachment; filename="athena-ca.pem"`)
			_, _ = w.Write(caPEM)
		})
		mux.HandleFunc("GET /ca.mobileconfig", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-apple-aspen-config")
			w.Header().Set("Content-Disposition", `attachment; filename="athena-ca.mobileconfig"`)
			_, _ = w.Write(profile)
		})
	}
	h := securityHeaders(mux, cfg.TLS != nil)
	return &Server{dash: cfg.Dashboard, h: h, srv: &http.Server{
		Handler:           h,
		TLSConfig:         cfg.TLS,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		// Failed handshakes from whatever reaches the port are noise; the certificate's own health is the tls item.
		ErrorLog: slog.NewLogLogger(log.With("component", "web").Handler(), slog.LevelDebug),
	}}
}

// securityHeaders sets the headers every response carries; HSTS only over TLS.
func securityHeaders(next http.Handler, overTLS bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		if overTLS {
			h.Set("Strict-Transport-Security", strictTransport)
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// Handler is the routes with the security headers.
func (s *Server) Handler() http.Handler { return s.h }

// Serve serves on l, over TLS when the configuration has it, until Shutdown; then it returns nil.
func (s *Server) Serve(l net.Listener) error {
	var err error
	if s.srv.TLSConfig != nil {
		err = s.srv.ServeTLS(l, "", "") // the certificate comes from TLSConfig.GetCertificate
	} else {
		err = s.srv.Serve(l)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown closes the listener, waits (up to ctx) for the requests in flight, then ends the dashboard connections
// and waits for them.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	s.dash.Close()
	return err
}
