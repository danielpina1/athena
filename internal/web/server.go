package web

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	stdlog "log"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/pki"
	"github.com/danielpina1/athena/internal/web/ui"
)

// The dashboard's HTTPS endpoint: the single-page app, the login, the WebSocket, the push key, the CA for devices
// to install and a health check, every response with the same security headers.

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
	// Artifacts serves /api/tickets/{id}/diff and /files; nil leaves the routes out (they answer 404).
	Artifacts Artifacts
	// UI is the built dashboard (ui.FS()); tests inject a fstest.MapFS. Without an index.html in it every page is the
	// "not built" fallback.
	UI fs.FS
}

// Server is the dashboard's HTTP server.
type Server struct {
	srv  *http.Server
	dash *Dashboard
	h    http.Handler
}

// NewServer builds the routes:
//
//	GET /ws                   the dashboard connection (Dashboard.ServeConn)
//	GET /api/tickets/{id}/diff, /files, /files/{path...}   a Ticket's diff and files (mountArtifacts)
//	/auth/*                   the login (Auth.Mount)
//	GET /push/key             the VAPID public key
//	GET /ca.pem, /ca.mobileconfig   the CA and its Apple profile (mode ca only)
//	GET /healthz              "ok", for a monitor: nothing else, no session needed
//	GET /                     everything else: the built single-page app (serveUI)
func NewServer(cfg ServerConfig) *Server {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /ws", cfg.Dashboard.ServeConn)
	cfg.Auth.Mount(mux)
	if cfg.Artifacts != nil {
		mountArtifacts(mux, cfg.Auth, cfg.Artifacts, log)
	}
	if cfg.VAPID != nil {
		mux.Handle("GET /push/key", cfg.VAPID.Handler())
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
	mux.Handle("GET /", uiHandler(cfg.UI))
	h := securityHeaders(mux, cfg.TLS != nil)
	return &Server{dash: cfg.Dashboard, h: h, srv: &http.Server{
		Handler:           h,
		TLSConfig:         cfg.TLS,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          stdlog.New(serverErrors{log.With("component", "web")}, "", 0),
	}}
}

// notBuiltPage is served for every page when the build is absent; it has no script.
const notBuiltPage = "<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><title>Athena</title></head><body><p>Dashboard not built: run make web</p></body></html>\n"

// uiHandler serves the built single-page app: a file that exists as itself (hashed files under /assets/ cached for a
// year, everything else revalidated), a path without an extension as index.html (the client-side routes), any other
// path as 404.
func uiHandler(fsys fs.FS) http.Handler {
	built := ui.IsBuilt(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		// The server's own routes are not client-side routes: an unregistered one (no VAPID key, no CA) is a 404.
		first, _, _ := strings.Cut(name, "/")
		if apiRoots[first] {
			http.NotFound(w, r)
			return
		}
		// The embedded tree has hidden files (.gitkeep, .vite/manifest.json): none is served. Nor is a directory of
		// assets listed or answered with the app.
		for _, seg := range strings.Split(name, "/") {
			if strings.HasPrefix(seg, ".") {
				http.NotFound(w, r)
				return
			}
		}
		if first == "assets" {
			if st, err := fs.Stat(fsys, name); err != nil || st.IsDir() || !built {
				http.NotFound(w, r)
				return
			}
		}
		if !built {
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = io.WriteString(w, notBuiltPage)
			return
		}
		if name != "" {
			if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
				serveUIFile(w, fsys, name, strings.HasPrefix(name, "assets/"))
				return
			}
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}
		serveUIFile(w, fsys, "index.html", false)
	})
}

// apiRoots are the first path segments the server's own routes use; the single-page fallback never answers them.
var apiRoots = map[string]bool{"auth": true, "push": true, "api": true, "ws": true, "healthz": true, "ca.pem": true, "ca.mobileconfig": true}

// uiTypes are the content types of the build's files, fixed so the host's mime database cannot change them.
var uiTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".woff2":       "font/woff2",
}

func serveUIFile(w http.ResponseWriter, fsys fs.FS, name string, immutable bool) {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ct := uiTypes[path.Ext(name)]
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(name))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(body)
}

// serverErrors is the http.Server's error log: a failed TLS handshake from whatever reaches the port is noise and goes
// to debug (the certificate's own health is the tls item); anything else, a handler's panic included, is a warning.
type serverErrors struct{ log *slog.Logger }

func (e serverErrors) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if strings.HasPrefix(msg, "http: TLS handshake error") {
		e.log.Debug(msg)
	} else {
		e.log.Warn(msg)
	}
	return len(p), nil
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

// Shutdown closes the listener, waits (up to ctx) for the requests in flight and closes what is left, then ends the
// dashboard connections and waits for them.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if err != nil {
		_ = s.srv.Close()
	}
	s.dash.Close()
	return err
}
