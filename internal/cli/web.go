package cli

import (
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/danielpina1/athena/internal/pki"
	"github.com/danielpina1/athena/internal/server"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
)

// --- web ---

const maxEnrollTTL = 24 * time.Hour

// safeText drops control characters, so a name read from the store cannot inject terminal escapes.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// web runs `athena web setup|status|ca|enroll|passkeys|revoke`. Setup writes the web part of the configuration, like
// `athena github setup`; the others work on the configuration, the CA and the store directly (SQLite lets them run
// beside the server): an enrollment token is just a row, and the server reads it when the browser presents it.
// Revoke goes through a running server, so it closes the passkey's open connections, and status asks a running
// server for its live certificate and connections.
func (c *command) web(args []string) int {
	if len(args) == 0 {
		c.fs.Usage()
		return 2
	}
	switch args[0] {
	case "setup":
		return c.webSetup(args[1:])
	case "status":
		return c.webStatus(args[1:])
	case "ca":
		return c.webCA(args[1:])
	case "enroll":
		return c.webEnroll(args[1:])
	case "passkeys":
		return c.webPasskeys(args[1:])
	case "revoke":
		return c.webRevoke(args[1:])
	}
	fmt.Fprintf(c.stderr, "athena web: unknown subcommand %q\n", args[0])
	c.fs.Usage()
	return 2
}

// openStore opens the server's database; it must exist, since a missing one means --state-dir is wrong.
func (c *command) openStore() (*store.Store, error) {
	path := filepath.Join(c.stateDir, server.DBFile)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("athena web: no database in %s (is --state-dir right, and has athena server run?)", c.stateDir)
	}
	return store.Open(path)
}

// webConfig reads the web part of config.json; it must name a host, or the owner has not run athena web setup.
func webConfig(stateDir string) (server.WebConfig, error) {
	cfg, err := server.LoadConfig(stateDir)
	if err != nil {
		return server.WebConfig{}, err
	}
	if cfg.Web == nil || cfg.Web.Host == "" {
		return server.WebConfig{}, fmt.Errorf("athena web: no web.host in %s; run athena web setup", filepath.Join(stateDir, server.ConfigFile))
	}
	return *cfg.Web, nil
}

// enrollURL is the enrollment link: the token rides in the fragment, which browsers never send to the server.
func enrollURL(base, token string) string { return base + "/enroll#" + token }

func (c *command) webEnroll(args []string) int {
	ttl := c.fs.Duration("ttl", web.DefaultEnrollTTL, "how long the link is good (up to 24h)")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	if *ttl <= 0 || *ttl > maxEnrollTTL {
		return c.fail(fmt.Errorf("athena web enroll: --ttl must be above 0 and at most %s", maxEnrollTTL))
	}
	wc, err := webConfig(c.stateDir)
	if err != nil {
		return c.fail(err)
	}
	st, err := c.openStore()
	if err != nil {
		return c.fail(err)
	}
	defer st.Close()
	token, err := web.IssueEnrollToken(st, *ttl)
	if err != nil {
		return c.fail(err)
	}
	link := enrollURL(wc.URL(), token)
	fmt.Fprintf(c.stdout, "Open this link on the device to enroll (single use, good for %s):\n\n%s\n\n", ttl.Round(time.Second), link)
	if q, err := encodeQR([]byte(link)); err == nil {
		q.render(c.stdout, isTerminal(c.stdout))
	}
	return 0
}

// isTerminal reports whether w is a character device, where colors are welcome.
func isTerminal(w any) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (c *command) webPasskeys(args []string) int {
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	st, err := c.openStore()
	if err != nil {
		return c.fail(err)
	}
	defer st.Close()
	list, err := st.Passkeys()
	if err != nil {
		return c.fail(err)
	}
	if len(list) == 0 {
		fmt.Fprintln(c.stdout, "no passkeys; run athena web enroll")
		return 0
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tCREATED\tLAST USED")
	for _, p := range list {
		used := "never"
		if !p.LastUsedAt.IsZero() {
			used = p.LastUsedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.ID, safeText(p.Name), p.CreatedAt.Local().Format("2006-01-02 15:04"), used)
	}
	w.Flush()
	return 0
}

func (c *command) webRevoke(args []string) int {
	pos, code, ok := c.parse(args, 1, 1)
	if !ok {
		return code
	}
	st, err := c.openStore()
	if err != nil {
		return c.fail(err)
	}
	defer st.Close()
	list, err := st.Passkeys()
	if err != nil {
		return c.fail(err)
	}
	// The id is long: a prefix that names one passkey is enough.
	var match []store.Passkey
	for _, p := range list {
		if p.ID == pos[0] {
			match = []store.Passkey{p}
			break
		}
		if strings.HasPrefix(p.ID, pos[0]) {
			match = append(match, p)
		}
	}
	switch len(match) {
	case 0:
		return c.fail(fmt.Errorf("athena web revoke: no passkey %q (athena web passkeys lists them)", pos[0]))
	case 1:
	default:
		return c.fail(fmt.Errorf("athena web revoke: %q matches %d passkeys; give more of the id", pos[0], len(match)))
	}
	p := match[0]
	ended, err := c.revokePasskey(st, p.ID)
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "revoked %q and ended %d session(s)\n", safeText(p.Name), ended)
	return 0
}

// revokePasskey revokes through the running server (web.revoke), which also closes the passkey's open dashboard
// connections; with no server answering it deletes from the store, where nothing is open to close.
func (c *command) revokePasskey(st *store.Store, id string) (int, error) {
	conn, err := dial(c.stateDir)
	if err != nil {
		sessions, err := st.DeletePasskey(id)
		return len(sessions), err
	}
	defer conn.Close()
	var out server.WebRevokeResult
	if err := exchange(conn, server.Request{Op: server.OpWebRevoke, ID: id}, &out); err != nil {
		return 0, err
	}
	return out.Sessions, nil
}

// --- setup, status, ca ---

// profileFile is where `athena web ca` writes the CA's Apple profile, beside ca.pem.
const profileFile = "athena-ca.mobileconfig"

// ipList is a repeatable --ip flag; a value may also list several, comma-separated. Each must be an address.
type ipList []string

func (l *ipList) String() string { return strings.Join(*l, ",") }
func (l *ipList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if net.ParseIP(p) == nil {
				return fmt.Errorf("%q is not an IP address", p)
			}
			*l = append(*l, p)
		}
	}
	return nil
}

func (c *command) webSetup(args []string) int {
	host := c.fs.String("host", "", "the DNS name your devices reach the dashboard by, for example athena.home.arpa")
	var ips ipList
	c.fs.Var(&ips, "ip", "an address the certificate also names (mode ca), for example the VPN address; repeat for more")
	listen := c.fs.String("listen", "", "the address to listen on (default "+server.DefaultWebListen+"); prefer the VPN interface's, like 10.8.0.1:8443")
	mode := c.fs.String("tls", "", "ca (default: Athena's own CA), acme (a public certificate through DNS-01) or off (loopback only)")
	email := c.fs.String("acme-email", "", "mode acme: the account's contact address")
	provider := c.fs.String("acme-provider", "", "mode acme: the DNS provider, cloudflare, exec or httpreq")
	agree := c.fs.Bool("acme-agree-tos", false, "mode acme: agree to the certificate authority's terms of service (Let's Encrypt: https://letsencrypt.org/repository/)")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	cfg, err := server.LoadConfig(c.stateDir)
	if err != nil {
		return c.fail(err)
	}
	// Setup changes what its flags name and keeps the rest of an earlier web part (resolvers, directory, ...).
	var w server.WebConfig
	if cfg.Web != nil {
		w = *cfg.Web
	}
	c.fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			w.Host = *host
		case "ip":
			w.IPs = ips
		case "listen":
			w.Listen = *listen
		case "tls":
			w.TLS = *mode
		case "acme-email":
			w.ACME.Email = *email
		case "acme-provider":
			w.ACME.Provider = *provider
		case "acme-agree-tos":
			w.ACME.AgreeTOS = *agree
		}
	})
	if err := server.SetupWeb(c.stateDir, w); err != nil {
		return c.fail(err)
	}
	if cfg, err = server.LoadConfig(c.stateDir); err != nil {
		return c.fail(err)
	}
	w = *cfg.Web
	fmt.Fprintf(c.stdout, "web: %s (tls %s, listening on %s) written to %s\n", w.URL(), w.TLS, w.Listen,
		filepath.Join(c.stateDir, server.ConfigFile))
	if h, _, err := net.SplitHostPort(w.Listen); err == nil {
		if ip := net.ParseIP(h); h == "" || ip != nil && ip.IsUnspecified() {
			fmt.Fprintln(c.stdout, "note: this listens on every interface; anything that reaches the port reaches the login. Bind the VPN interface's address with --listen when you can.")
		}
	}
	switch w.TLS {
	case "ca":
		ca, err := pki.LoadOrCreateCA(web.CADir(c.stateDir), w.Host, w.ParsedIPs())
		if err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.stdout, "CA fingerprint (SHA-256): %s\n", ca.Fingerprint())
		fmt.Fprintf(c.stdout, "install the CA on each device: %s/ca.mobileconfig on an iPhone, or athena web ca for the files\n", w.URL())
	case "acme":
		if !w.ACME.AgreeTOS {
			fmt.Fprintln(c.stdout, "note: no certificate until you agree to the certificate authority's terms: run again with --acme-agree-tos")
		}
	}
	fmt.Fprintln(c.stdout, "restart athena server, then run athena web enroll on this machine and open the link on the device")
	return 0
}

func (c *command) webStatus(args []string) int {
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	cfg, err := server.LoadConfig(c.stateDir)
	if err != nil {
		return c.fail(err)
	}
	if cfg.Web == nil || cfg.Web.Host == "" {
		return c.fail(fmt.Errorf("athena web: no web.host in %s; run athena web setup", filepath.Join(c.stateDir, server.ConfigFile)))
	}
	w := *cfg.Web
	tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	state := "enabled"
	if !w.Enabled {
		state = "disabled (athena web setup enables it)"
	}
	fmt.Fprintf(tw, "URL:\t%s (%s)\n", w.URL(), state)

	// A running server knows its certificate's health and its connections; without one the file tells the expiry.
	var live *server.WebStatusResult
	if conn, err := dial(c.stateDir); err == nil {
		var st server.WebStatusResult
		err := exchange(conn, server.Request{Op: server.OpWebStatus}, &st)
		conn.Close()
		if err != nil {
			return c.fail(err)
		}
		live = &st
	}
	switch {
	case live != nil && live.Serving:
		fmt.Fprintf(tw, "TLS:\t%s: %s\n", live.TLS, live.TLSMessage)
	case w.TLS == "off":
		fmt.Fprintf(tw, "TLS:\toff (plain HTTP on loopback)\n")
	case live == nil:
		fmt.Fprintf(tw, "TLS:\t%s: %s (server not running)\n", w.TLS, certExpiry(web.CertFile(c.stateDir)))
	default:
		fmt.Fprintf(tw, "TLS:\t%s: %s (the running server does not serve the dashboard)\n", w.TLS, certExpiry(web.CertFile(c.stateDir)))
	}
	if w.TLS == "ca" {
		// Read only: status never creates the CA (setup does).
		fp := "none yet (athena web setup creates it)"
		if ca, ok, err := pki.LoadCA(web.CADir(c.stateDir), w.Host, w.ParsedIPs()); err != nil {
			fp = err.Error()
		} else if ok {
			fp = ca.Fingerprint()
		}
		fmt.Fprintf(tw, "CA fingerprint:\t%s\n", fp)
	}
	passkeys := "0 (no database yet)"
	if st, err := c.openStore(); err == nil {
		list, err := st.Passkeys()
		st.Close()
		if err != nil {
			return c.fail(err)
		}
		passkeys = strconv.Itoa(len(list))
	}
	fmt.Fprintf(tw, "Passkeys:\t%s\n", passkeys)
	switch {
	case live == nil:
		fmt.Fprintf(tw, "Clients:\tserver not running\n")
	case !live.Serving:
		fmt.Fprintf(tw, "Clients:\tthe running server does not serve the dashboard (restart it after athena web setup)\n")
	default:
		fmt.Fprintf(tw, "Clients:\t%d connected\n", live.Clients)
	}
	return 0
}

// certExpiry reads the stored certificate's expiry, for when no running server reports it.
func certExpiry(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "no certificate yet (the server makes or fetches it)"
	}
	b, _ := pem.Decode(raw)
	if b == nil {
		return "unreadable certificate " + path
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return "unreadable certificate " + path
	}
	return "certificate valid until " + cert.NotAfter.Local().Format("2006-01-02")
}

func (c *command) webCA(args []string) int {
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	w, err := webConfig(c.stateDir)
	if err != nil {
		return c.fail(err)
	}
	if w.TLS != "ca" {
		return c.fail(fmt.Errorf("athena web ca: web.tls is %s; the CA is only for mode ca", w.TLS))
	}
	dir := web.CADir(c.stateDir)
	ca, err := pki.LoadOrCreateCA(dir, w.Host, w.ParsedIPs())
	if err != nil {
		return c.fail(err)
	}
	profile := filepath.Join(dir, profileFile)
	if err := os.WriteFile(profile, pki.MobileConfig(ca), 0o644); err != nil {
		return c.fail(fmt.Errorf("athena web ca: %w", err))
	}
	fmt.Fprintf(c.stdout, "fingerprint (SHA-256): %s\n", ca.Fingerprint())
	fmt.Fprintf(c.stdout, "certificate:           %s\n", pki.CertPath(dir))
	fmt.Fprintf(c.stdout, "profile (iOS, macOS):  %s\n", profile)
	fmt.Fprintf(c.stdout, "also served at %s/ca.pem and %s/ca.mobileconfig\n", w.URL(), w.URL())
	return 0
}
