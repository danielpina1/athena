package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/server"
	"github.com/danielpina1/athena/internal/store"
)

// webState is a state directory with a database (as after one server run) and the web part of the configuration.
func webState(t *testing.T, config string) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, server.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, server.ConfigFile), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, st
}

func runWeb(dir string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = Run(context.Background(), "web", append(args, "--state-dir", dir), &out, &errb)
	return code, out.String(), errb.String()
}

func TestWebEnrollPrintsLinkAndQR(t *testing.T) {
	dir, st := webState(t, `{"web":{"host":"athena.example.net","listen":"0.0.0.0:8443"}}`)
	var logs bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(syncWriter{&mu, &logs}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	code, out, errs := runWeb(dir, "enroll")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	const prefix = "https://athena.example.net:8443/enroll#"
	i := strings.Index(out, prefix)
	if i < 0 {
		t.Fatalf("no link in output:\n%s", out)
	}
	token := strings.Fields(out[i+len(prefix):])[0]
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token %q is not 32 base64url bytes", token)
	}
	if !strings.ContainsAny(out, "▀▄█") {
		t.Fatalf("no QR code in output:\n%s", out)
	}
	// The token is live (and then spent), and it is what the QR carries.
	h := sha256.Sum256(raw)
	if ok, err := st.UseEnrollToken(h[:], time.Now()); err != nil || !ok {
		t.Fatalf("token not stored: %v %v", ok, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), token) {
		t.Fatalf("token in the log: %s", logs.String())
	}
}

func TestWebEnrollWithoutWebConfig(t *testing.T) {
	for name, cfg := range map[string]string{"no file": "", "no web": `{}`, "no host": `{"web":{"listen":":8443"}}`} {
		dir, _ := webState(t, cfg)
		code, out, errs := runWeb(dir, "enroll")
		if code != 1 || !strings.Contains(errs, "athena web setup") || out != "" {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q", name, code, out, errs)
		}
	}
}

func TestWebEnrollLinkOmitsDefaultPort(t *testing.T) {
	if got := enrollURL("https://h.net", "T"); got != "https://h.net/enroll#T" {
		t.Fatal(got)
	}
	dir, _ := webState(t, `{"web":{"host":"h.net","listen":":443"}}`)
	if _, out, _ := runWeb(dir, "enroll"); !strings.Contains(out, "https://h.net/enroll#") {
		t.Fatalf("port 443 shown: %s", out)
	}
	dir, _ = webState(t, `{"web":{"host":"localhost","listen":"127.0.0.1:8080","tls":"off"}}`)
	if _, out, _ := runWeb(dir, "enroll"); !strings.Contains(out, "http://localhost:8080/enroll#") {
		t.Fatalf("tls off link: %s", out)
	}
}

func TestWebSetupStatusAndCA(t *testing.T) {
	dir, st := webState(t, `{"router":{"model":"opus"}}`)
	code, out, errs := runWeb(dir, "setup", "--host", "Athena.Home.Arpa", "--ip", "10.8.0.1", "--listen", "10.8.0.1:8443")
	if code != 0 || !strings.Contains(out, "https://athena.home.arpa:8443") || !strings.Contains(out, "CA fingerprint") ||
		strings.Contains(out, "every interface") {
		t.Fatalf("setup: %d %q %s", code, out, errs)
	}
	cfg, err := server.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Web; !cfg.WebOn() || w.Host != "athena.home.arpa" || w.TLS != "ca" || len(w.IPs) != 1 || cfg.Router.Model != "opus" {
		t.Fatalf("config after setup: %+v %+v", cfg.Web, cfg.Router)
	}
	if err := st.PutPasskey(store.Passkey{ID: "p1", Name: "phone", Credential: []byte("{}"), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	code, out, errs = runWeb(dir, "ca")
	if code != 0 {
		t.Fatalf("ca: %d %s", code, errs)
	}
	fp := strings.TrimSpace(strings.TrimPrefix(strings.Split(out, "\n")[0], "fingerprint (SHA-256):"))
	if len(fp) != 95 || !strings.Contains(out, filepath.Join(dir, "pki", "ca.pem")) {
		t.Fatalf("ca output:\n%s", out)
	}
	profile, err := os.ReadFile(filepath.Join(dir, "pki", "athena-ca.mobileconfig"))
	if err != nil || !strings.Contains(string(profile), "PayloadContent") {
		t.Fatalf("profile: %v", err)
	}

	code, out, errs = runWeb(dir, "status")
	for _, want := range []string{"https://athena.home.arpa:8443 (enabled)", "TLS:", "ca:", fp, "Passkeys:", "1", "server not running"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("status lacks %q: %d %q %s", want, code, out, errs)
		}
	}

	// A new host does not silently reuse the CA made for the old one.
	if code, _, errs := runWeb(dir, "setup", "--host", "other.home.arpa"); code != 1 || !strings.Contains(errs, "athena.home.arpa") {
		t.Fatalf("setup with another host: %d %s", code, errs)
	}
	// Switching to ACME keeps the host; without the terms agreed it says how to agree.
	code, out, errs = runWeb(dir, "setup", "--tls", "acme", "--acme-provider", "exec", "--acme-email", "me@example.com", "--listen", ":8443")
	if code != 0 || !strings.Contains(out, "--acme-agree-tos") || !strings.Contains(out, "every interface") {
		t.Fatalf("acme setup: %d %q %s", code, out, errs)
	}
	if code, out, _ := runWeb(dir, "setup", "--acme-agree-tos"); code != 0 || strings.Contains(out, "--acme-agree-tos") {
		t.Fatalf("agree: %d %q", code, out)
	}
	if cfg, _ := server.LoadConfig(dir); !cfg.Web.ACME.AgreeTOS || cfg.Web.Host != "athena.home.arpa" || cfg.Web.ACME.Provider != "exec" {
		t.Fatalf("acme config %+v", cfg.Web)
	}
	if code, _, errs := runWeb(dir, "ca"); code != 1 || !strings.Contains(errs, "mode ca") {
		t.Fatalf("ca in acme mode: %d %s", code, errs)
	}
	if code, _, errs := runWeb(t.TempDir(), "setup", "--host", "h.test", "--ip", "10.0.0.1,nope"); code != 2 || !strings.Contains(errs, `"nope" is not an IP address`) {
		t.Fatalf("bad --ip: %d %s", code, errs)
	}
	// Errors name the field.
	if code, _, errs := runWeb(t.TempDir(), "setup"); code != 1 || !strings.Contains(errs, "web.host") {
		t.Fatalf("setup without host: %d %s", code, errs)
	}
	if code, _, errs := runWeb(t.TempDir(), "setup", "--host", "h.test", "--tls", "off"); code != 1 || !strings.Contains(errs, "web.tls off") {
		t.Fatalf("tls off on every interface: %d %s", code, errs)
	}
}

func TestWebPasskeysAndRevoke(t *testing.T) {
	dir, st := webState(t, "")
	if code, out, _ := runWeb(dir, "passkeys"); code != 0 || !strings.Contains(out, "no passkeys") {
		t.Fatalf("empty list: %d %q", code, out)
	}
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	for _, p := range []store.Passkey{
		{ID: "AAAAphone", Name: "phone", Credential: []byte("{}"), CreatedAt: t0, LastUsedAt: t0.Add(time.Hour)},
		{ID: "AAAAlaptop", Name: "laptop", Credential: []byte("{}"), CreatedAt: t0.Add(time.Minute)},
		{ID: "BBBBtablet", Name: "tablet", Credential: []byte("{}"), CreatedAt: t0.Add(2 * time.Minute)},
	} {
		if err := st.PutPasskey(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []store.WebSession{
		{Hash: []byte("h1"), CreatedAt: t0, SeenAt: t0, PasskeyID: "AAAAphone"},
		{Hash: []byte("h2"), CreatedAt: t0, SeenAt: t0, PasskeyID: "AAAAphone"},
		{Hash: []byte("h3"), CreatedAt: t0, SeenAt: t0, PasskeyID: "BBBBtablet"},
	} {
		if err := st.PutWebSession(s); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ := runWeb(dir, "passkeys")
	if code != 0 || !strings.Contains(out, "AAAAphone") || !strings.Contains(out, "never") || !strings.Contains(out, "tablet") {
		t.Fatalf("list:\n%s", out)
	}

	// An ambiguous prefix and an unknown id change nothing.
	if code, _, errs := runWeb(dir, "revoke", "AAAA"); code != 1 || !strings.Contains(errs, "2 passkeys") {
		t.Fatalf("ambiguous: %d %s", code, errs)
	}
	if code, _, _ := runWeb(dir, "revoke", "ZZZZ"); code != 1 {
		t.Fatalf("unknown id: %d", code)
	}
	if list, _ := st.Passkeys(); len(list) != 3 {
		t.Fatalf("%d passkeys after refused revokes", len(list))
	}

	code, out, errs := runWeb(dir, "revoke", "AAAAp")
	if code != 0 || !strings.Contains(out, "2 session") {
		t.Fatalf("revoke: %d %q %s", code, out, errs)
	}
	if _, ok, _ := st.Passkey("AAAAphone"); ok {
		t.Fatal("passkey survived")
	}
	for _, h := range []string{"h1", "h2"} {
		if _, ok, _ := st.WebSession([]byte(h)); ok {
			t.Fatalf("session %s survived", h)
		}
	}
	if _, ok, _ := st.WebSession([]byte("h3")); !ok {
		t.Fatal("another passkey's session was ended")
	}
}

func TestWebNoDatabase(t *testing.T) {
	dir := t.TempDir()
	if code, _, errs := runWeb(dir, "passkeys"); code != 1 || !strings.Contains(errs, "no database") {
		t.Fatalf("%d %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, server.DBFile)); err == nil {
		t.Fatal("a database was created")
	}
}

type syncWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestWebEnrollTTLBounds(t *testing.T) {
	dir, _ := webState(t, `{"web":{"host":"h.net"}}`)
	for _, ttl := range []string{"0", "-5m", "25h"} {
		if code, out, errs := runWeb(dir, "enroll", "--ttl", ttl); code != 1 || out != "" || !strings.Contains(errs, "--ttl") {
			t.Fatalf("ttl %s: %d %q %q", ttl, code, out, errs)
		}
	}
	if code, out, _ := runWeb(dir, "enroll", "--ttl", "2h"); code != 0 || !strings.Contains(out, "good for 2h0m0s") {
		t.Fatalf("ttl 2h: %d %q", code, out)
	}
}

func TestWebPasskeysStripsControlCharacters(t *testing.T) {
	dir, st := webState(t, "")
	if err := st.PutPasskey(store.Passkey{ID: "x", Name: "a\x1b[2Jb", Credential: []byte("{}"), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := runWeb(dir, "passkeys"); strings.Contains(out, "\x1b") || !strings.Contains(out, "a[2Jb") {
		t.Fatalf("%q", out)
	}
}

func TestWebRevokeThroughRunningServer(t *testing.T) {
	dir := startServer(t)
	st, err := store.Open(filepath.Join(dir, server.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Now()
	if err := st.PutPasskey(store.Passkey{ID: "CCCCphone", Name: "phone", Credential: []byte("{}"), CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"h1", "h2"} {
		if err := st.PutWebSession(store.WebSession{Hash: []byte(h), CreatedAt: t0, SeenAt: t0, PasskeyID: "CCCCphone"}); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errs := runWeb(dir, "revoke", "CCCC")
	if code != 0 || !strings.Contains(out, `revoked "phone" and ended 2 session(s)`) {
		t.Fatalf("revoke: %d %q %s", code, out, errs)
	}
	if _, ok, _ := st.Passkey("CCCCphone"); ok {
		t.Fatal("passkey survived")
	}

	// status asks the running server, which serves no dashboard: its web part is not enabled.
	if err := os.WriteFile(filepath.Join(dir, server.ConfigFile), []byte(`{"web":{"host":"h.test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = runWeb(dir, "status")
	if code != 0 || !strings.Contains(out, "(disabled") || !strings.Contains(out, "does not serve the dashboard") ||
		strings.Contains(out, "server not running") {
		t.Fatalf("status: %d %q %s", code, out, errs)
	}
	// status reads only: no CA is created for it.
	if _, err := os.Stat(filepath.Join(dir, "pki")); !os.IsNotExist(err) {
		t.Fatalf("status created the CA: %v", err)
	}
}

// A passkey id is base64url, so about one in 64 starts with "-": revoke takes it as the id, not as a flag, with
// --state-dir before or after it, and passkeys says so next to the list.
func TestWebRevokeIDStartingWithDash(t *testing.T) {
	dir, st := webState(t, "")
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	for _, p := range []store.Passkey{
		{ID: "-UaLIDzW5grOf1r9Ind", Name: "phone", Credential: []byte("{}"), CreatedAt: t0},
		{ID: "--x2Kq", Name: "laptop", Credential: []byte("{}"), CreatedAt: t0.Add(time.Minute)},
		{ID: "BBBBtablet", Name: "tablet", Credential: []byte("{}"), CreatedAt: t0.Add(2 * time.Minute)},
	} {
		if err := st.PutPasskey(p); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ := runWeb(dir, "passkeys")
	if code != 0 || !strings.Contains(out, `starts with "-"`) {
		t.Fatalf("no hint for a dash id:\n%s", out)
	}
	// A "-" prefix of 8 characters or more is an id prefix.
	if code, out, errs := runWeb(dir, "revoke", "-UaLIDzW"); code != 0 || !strings.Contains(out, `revoked "phone"`) {
		t.Fatalf("revoke -prefix: %d %q %s", code, out, errs)
	}
	// A short "-" argument that is not an exact id is a flag, and an unknown one: usage, nothing revoked. An owner
	// typing -y for "yes" must not revoke the one passkey whose id happens to start with "-y".
	for _, arg := range []string{"--x2", "-y", "-"} {
		if code, out, errs := runWeb(dir, "revoke", arg); code != 2 || !strings.Contains(strings.ToLower(errs), "usage") {
			t.Fatalf("revoke %s: %d %q %s", arg, code, out, errs)
		}
	}
	if list, _ := st.Passkeys(); len(list) != 2 {
		t.Fatalf("%d passkeys after refused dash arguments", len(list))
	}
	// The exact id works however short, the flag after it (runWeb adds --state-dir at the end).
	if code, out, errs := runWeb(dir, "revoke", "--x2Kq"); code != 0 || !strings.Contains(out, `revoked "laptop"`) {
		t.Fatalf("revoke exact --id: %d %q %s", code, out, errs)
	}
	if list, _ := st.Passkeys(); len(list) != 1 || list[0].ID != "BBBBtablet" {
		t.Fatalf("left %v", list)
	}
	// Without a dash id, no hint.
	if _, out, _ := runWeb(dir, "passkeys"); strings.Contains(out, `starts with "-"`) {
		t.Fatalf("hint without a dash id:\n%s", out)
	}
	// A real flag is still a flag: a bad one is a usage error, and --help is help.
	if code, _, _ := runWeb(dir, "revoke", "--nope=1", "BBBBtablet"); code != 2 {
		t.Fatalf("unknown flag: %d", code)
	}
	if code, _, errs := runWeb(dir, "revoke", "--help"); code != 0 || !strings.Contains(strings.ToLower(errs), "usage") {
		t.Fatalf("--help: %d %s", code, errs)
	}
	if list, _ := st.Passkeys(); len(list) != 1 {
		t.Fatalf("%d passkeys after --help", len(list))
	}
}
