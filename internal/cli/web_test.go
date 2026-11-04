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
	if got := enrollURL("h.net", "", "T"); got != "https://h.net/enroll#T" {
		t.Fatal(got)
	}
	dir, _ := webState(t, `{"web":{"host":"h.net","listen":":443"}}`)
	if _, out, _ := runWeb(dir, "enroll"); !strings.Contains(out, "https://h.net/enroll#") {
		t.Fatalf("port 443 shown: %s", out)
	}
	if got := enrollURL("fd00::1", "8443", "T"); got != "https://[fd00::1]:8443/enroll#T" {
		t.Fatal(got)
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
