package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

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

// web runs `athena web enroll|passkeys|revoke`. They work on the store directly (SQLite lets them run beside the
// server), like `athena github setup` works on the configuration: an enrollment token is just a row, and the server
// reads it when the browser presents it. Revoke goes through a running server, so it closes the passkey's open
// connections. (`setup`, `status` and `ca` are not here yet.)
func (c *command) web(args []string) int {
	if len(args) == 0 {
		c.fs.Usage()
		return 2
	}
	switch args[0] {
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

// webAddress is where the owner reaches the dashboard: web.host and web.listen of config.json. The `web` part is
// defined by `athena web setup`; until that command exists this reads what it will write.
func webAddress(stateDir string) (host, port string, err error) {
	missing := fmt.Errorf("athena web: no web.host in %s; run athena web setup", filepath.Join(stateDir, server.ConfigFile))
	raw, err := os.ReadFile(filepath.Join(stateDir, server.ConfigFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", "", missing
	}
	if err != nil {
		return "", "", err
	}
	var cfg struct {
		Web struct {
			Host   string `json:"host"`
			Listen string `json:"listen"`
		} `json:"web"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", "", fmt.Errorf("athena web: %s: %w", server.ConfigFile, err)
	}
	if cfg.Web.Host == "" {
		return "", "", missing
	}
	if _, p, err := net.SplitHostPort(cfg.Web.Listen); err == nil && p != "" && p != "443" {
		port = p
	}
	return cfg.Web.Host, port, nil
}

func enrollURL(host, port, token string) string {
	h := host
	if port != "" {
		h = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		h = "[" + host + "]"
	}
	return "https://" + h + "/enroll#" + token
}

func (c *command) webEnroll(args []string) int {
	ttl := c.fs.Duration("ttl", web.DefaultEnrollTTL, "how long the link is good (up to 24h)")
	if _, code, ok := c.parse(args, 0, 0); !ok {
		return code
	}
	if *ttl <= 0 || *ttl > maxEnrollTTL {
		return c.fail(fmt.Errorf("athena web enroll: --ttl must be above 0 and at most %s", maxEnrollTTL))
	}
	host, port, err := webAddress(c.stateDir)
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
	link := enrollURL(host, port, token)
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
