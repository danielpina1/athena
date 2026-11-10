package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/fleet"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/server"
	"github.com/danielpina1/athena/internal/store"
)

// startWebServer runs `athena server` with the dashboard on 127.0.0.1 (an ephemeral port, TLS ca for localhost), so
// the server serves nodes; it returns the state directory and the server's stderr.
func startWebServer(t *testing.T) (string, *bytes.Buffer) {
	t.Helper()
	dir := newStateDir(t)
	// No local repositories are listed: the dashboard never scans this machine's folders in a test.
	if err := os.WriteFile(filepath.Join(dir, server.ConfigFile), []byte(`{"localRepoRoots":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.SetupWeb(dir, server.WebConfig{Listen: "127.0.0.1:0", Host: "localhost", TLS: "ca"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var serverErr bytes.Buffer
	lw := &lockedWriter{w: &serverErr}
	go func() {
		done <- Run(ctx, "server", []string{"--state-dir", dir, "--claude", fakeClaude(t), "--codex", noCodex}, io.Discard, lw)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("athena server exit code = %d", code)
			}
		case <-time.After(waitTimeout):
			t.Error("athena server did not stop")
		}
	})
	eventually(t, "server answers", func() bool {
		code, _, _ := athena(context.Background(), "approvals", "--state-dir", dir)
		return code == 0
	})
	return dir, &serverErr
}

var joinLine = regexp.MustCompile(`^athena node join (https://localhost:\d+) ([A-Za-z0-9_-]{43}) --ca-sha256 ((?:[0-9A-F]{2}:){31}[0-9A-F]{2})\n$`)

func TestNodeTokenPrintsJoinCommand(t *testing.T) {
	dir, serverErr := startWebServer(t)
	code, out, errOut := athena(context.Background(), "node", "token", "--state-dir", dir)
	if code != 0 {
		t.Fatalf("node token: exit %d, stderr %q", code, errOut)
	}
	m := joinLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("node token printed %q; want one join command line", out)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "pki", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(caPEM)
	if want := nodelink.Fingerprint(blk.Bytes); m[3] != want {
		t.Fatalf("fingerprint %s, want the dashboard CA's %s", m[3], want)
	}
	// The token is a node join token, good for an hour.
	st, err := store.Open(filepath.Join(dir, server.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	raw, err := base64.RawURLEncoding.DecodeString(m[2])
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	for at, want := range map[time.Duration]string{55 * time.Minute: store.EnrollTokenValid, 65 * time.Minute: store.EnrollTokenExpired} {
		if s, err := st.EnrollTokenState(hash[:], store.EnrollNode, time.Now().Add(at)); err != nil || s != want {
			t.Fatalf("node token in %s: %q %v, want %s", at, s, err, want)
		}
	}
	// The token is never logged: neither on stderr nor in the log file.
	logFile, _ := os.ReadFile(filepath.Join(dir, server.LogDir, server.LogFile))
	if strings.Contains(serverErr.String(), m[2]) || strings.Contains(string(logFile), m[2]) {
		t.Fatal("the join token is in the server's log")
	}

	for _, ttl := range []string{"25h", "0", "-1m"} {
		code, out, errOut := athena(context.Background(), "node", "token", "--ttl", ttl, "--state-dir", dir)
		if code != 1 || out != "" || !strings.Contains(errOut, "24h") {
			t.Fatalf("--ttl %s: exit %d, stdout %q, stderr %q", ttl, code, out, errOut)
		}
	}
	if code, out, _ := athena(context.Background(), "node", "token", "--ttl", "24h", "--state-dir", dir); code != 0 || !joinLine.MatchString(out) {
		t.Fatalf("--ttl 24h: exit %d, stdout %q", code, out)
	}
}

func TestJoinCommandLine(t *testing.T) {
	if got := joinCommand("https://a.example", "tok", "AB:CD"); got != "athena node join https://a.example tok --ca-sha256 AB:CD" {
		t.Fatalf("ca: %q", got)
	}
	// ACME: the system roots verify the server, so no fingerprint is given.
	if got := joinCommand("https://a.example", "tok", ""); got != "athena node join https://a.example tok" {
		t.Fatalf("acme: %q", got)
	}
}

func TestNodeTokenNeedsTheDashboard(t *testing.T) {
	dir := startServer(t)
	code, out, errOut := athena(context.Background(), "node", "token", "--state-dir", dir)
	if code != 1 || out != "" || !strings.Contains(errOut, "athena web setup") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// joinServer is a TLS server that answers a join like the dashboard: its own certificate is its CA, served at
// /ca.pem, and the join signs the CSR. joins counts the join requests; names records the names asked for.
func joinServer(t *testing.T) (ts *httptest.Server, joins *atomic.Int32, names chan string) {
	t.Helper()
	joins, names = &atomic.Int32{}, make(chan string, 4)
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node ca"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	mux := http.NewServeMux()
	ts = httptest.NewUnstartedServer(mux)
	mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, _ *http.Request) {
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	})
	mux.HandleFunc("POST /node/join", func(w http.ResponseWriter, r *http.Request) {
		joins.Add(1)
		var req struct {
			Token, Name string
			CSR         []byte
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token != "tok-123" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"This join token is not valid."}`))
			return
		}
		names <- req.Name
		csr, err := x509.ParseCertificateRequest(req.CSR)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "n0123456789"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, csr.PublicKey, caKey)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"nodeId": "n0123456789", "name": req.Name, "cert": der, "caCert": caDER,
			"nodeURL": "wss://127.0.0.1:1/node"})
	})
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, joins, names
}

func TestJoinChecksFingerprint(t *testing.T) {
	ts, joins, names := joinServer(t)
	stateDir := filepath.Join(t.TempDir(), "node")
	wrong := strings.Repeat("AB:", 31) + "AB"
	code, out, errOut := athena(context.Background(), "node", "join", ts.URL, "tok-123", "--ca-sha256", wrong, "--state", stateDir)
	if code != 1 || out != "" || errOut != "The server's certificate does not match --ca-sha256; check the command you copied.\n" {
		t.Fatalf("wrong fingerprint: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if joins.Load() != 0 {
		t.Fatal("the join was sent to a server whose certificate does not match")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("a refused join wrote the state directory: %v", err)
	}

	right := nodelink.Fingerprint(ts.Certificate().Raw)
	code, out, errOut = athena(context.Background(), "node", "join", ts.URL, "tok-123", "--ca-sha256", strings.ToLower(right), "--state", stateDir)
	if code != 0 {
		t.Fatalf("join: exit %d, stderr %q", code, errOut)
	}
	host, _ := os.Hostname()
	if name := <-names; name != fleet.LocalName(host) {
		t.Fatalf("name %q, want the host name's %q", name, fleet.LocalName(host))
	}
	if !strings.Contains(out, "n0123456789") || !strings.Contains(out, "athena node run") {
		t.Fatalf("join printed %q", out)
	}
	id, err := nodelink.LoadIdentity(stateDir)
	if err != nil || id.NodeID != "n0123456789" || id.ServerFingerprint != right {
		t.Fatalf("identity %+v, %v", id, err)
	}
	fi, err := os.Stat(filepath.Join(stateDir, "node.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("node.key: %v %v", fi, err)
	}

	// A name that is not a node name is refused before anything is sent.
	other := filepath.Join(t.TempDir(), "other")
	if code, _, errOut := athena(context.Background(), "node", "join", ts.URL, "tok-123", "--name", "Bad Name", "--ca-sha256", right, "--state", other); code != 1 || !strings.Contains(errOut, "--name") {
		t.Fatalf("bad name: exit %d, stderr %q", code, errOut)
	}
	// A token that starts with "-" (base64url does) is still the token.
	dash := "-" + strings.Repeat("a", 42)
	code, _, errOut = athena(context.Background(), "node", "join", ts.URL, dash, "--ca-sha256", right, "--state", other)
	if code != 1 || errOut != "This join token is not valid.\n" {
		t.Fatalf("dash token: exit %d, stderr %q", code, errOut)
	}
}

func TestNodeRunNeedsIdentity(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "node")
	code, out, errOut := athena(context.Background(), "node", "run", "--state", stateDir)
	if code != 1 || out != "" || errOut != "This machine has not joined yet; run athena node join first.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// withStdin makes the commands read in as their standard input.
func withStdin(t *testing.T, in string) {
	t.Helper()
	old := stdin
	stdin = strings.NewReader(in)
	t.Cleanup(func() { stdin = old })
}

func TestRevokeAsksFirst(t *testing.T) {
	dir := startServer(t)
	st, err := store.Open(filepath.Join(dir, server.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	for _, n := range []store.Node{{ID: "n00000000b1", Name: "box", CertSerial: "1", CreatedAt: now, LastSeen: now},
		{ID: "n00000000c1", Name: "cube", CertSerial: "2", CreatedAt: now, LastSeen: now}} {
		if err := st.PutNode(n); err != nil {
			t.Fatal(err)
		}
	}
	revoked := func(id string) bool {
		n, err := st.GetNode(id)
		if err != nil || n == nil {
			t.Fatalf("node %s: %v", id, err)
		}
		return n.RevokedAt != nil
	}
	ctx := context.Background()
	const prompt = "Revoke box? Its sessions stop and it must join again. [y/N] "

	withStdin(t, "n\n")
	code, out, _ := athena(ctx, "node", "revoke", "box", "--state-dir", dir)
	if code != 1 || !strings.HasPrefix(out, prompt) || revoked("n00000000b1") {
		t.Fatalf("answered no: exit %d, stdout %q", code, out)
	}
	withStdin(t, "")
	if code, out, _ := athena(ctx, "node", "revoke", "box", "--state-dir", dir); code != 1 || !strings.HasPrefix(out, prompt) || revoked("n00000000b1") {
		t.Fatalf("no answer: exit %d, stdout %q", code, out)
	}
	// By id, the prompt still names the node.
	withStdin(t, "y\n")
	if code, out, errOut := athena(ctx, "node", "revoke", "n00000000b1", "--state-dir", dir); code != 0 || !strings.HasPrefix(out, prompt) || !revoked("n00000000b1") {
		t.Fatalf("answered yes: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	withStdin(t, "")
	if code, out, errOut := athena(ctx, "node", "revoke", "cube", "--yes", "--state-dir", dir); code != 0 || strings.Contains(out, "Revoke cube?") || !revoked("n00000000c1") {
		t.Fatalf("--yes: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, errOut := athena(ctx, "node", "revoke", "nobody", "--yes", "--state-dir", dir); code != 1 || !strings.Contains(errOut, "nobody") {
		t.Fatalf("unknown node: exit %d, stderr %q", code, errOut)
	}
	// The server's own node is never revoked, and no question is asked about it.
	local := fleet.LocalName(mustHostname(t))
	if code, out, errOut := athena(ctx, "node", "revoke", local, "--state-dir", dir); code != 1 || strings.Contains(out, "Revoke") || !strings.Contains(errOut, "cannot be revoked") {
		t.Fatalf("local: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func mustHostname(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestNodeListDrainUndrain(t *testing.T) {
	dir := startServer(t)
	st, err := store.Open(filepath.Join(dir, server.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seen := time.Now().Add(-5 * time.Minute)
	if err := st.PutNode(store.Node{ID: "n00000000b1", Name: "box", CertSerial: "1", CreatedAt: seen, LastSeen: seen}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	row := func(name string) []string {
		t.Helper()
		code, out, errOut := athena(ctx, "node", "list", "--state-dir", dir)
		if code != 0 {
			t.Fatalf("list: exit %d, stderr %q", code, errOut)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if h := strings.Join(strings.Fields(lines[0]), " "); h != "NAME ID STATE SLOTS RUNTIMES LAST SEEN" {
			t.Fatalf("header %q", lines[0])
		}
		for _, l := range lines[1:] {
			if f := strings.Fields(l); f[0] == name {
				return f
			}
		}
		t.Fatalf("no %s in\n%s", name, out)
		return nil
	}
	local := fleet.LocalName(mustHostname(t))
	if f := row(local); f[1] != fleet.LocalID || f[2] != "online" || f[3] != "0/2" || !strings.Contains(f[4], "claude") ||
		strings.Join(f[len(f)-2:], " ") != "this machine" {
		t.Fatalf("local row %v", f)
	}
	if f := row("box"); f[1] != "n00000000b1" || f[2] != "offline" || strings.Join(f[len(f)-2:], " ") != "5m ago" {
		t.Fatalf("box row %v", f)
	}
	if code, out, errOut := athena(ctx, "node", "drain", "box", "--state-dir", dir); code != 0 || out != "box: draining\n" {
		t.Fatalf("drain: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if n, _ := st.GetNode("n00000000b1"); !n.Draining {
		t.Fatal("box is not draining")
	}
	if code, out, errOut := athena(ctx, "node", "undrain", "n00000000b1", "--state-dir", dir); code != 0 || out != "box: taking new work\n" {
		t.Fatalf("undrain: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, _ := athena(ctx, "node", "drain", local, "--state-dir", dir); code != 0 {
		t.Fatal("drain the local node")
	}
	if f := row(local); f[2] != "draining" {
		t.Fatalf("local row after drain %v", f)
	}
	if code, _, errOut := athena(ctx, "node", "drain", "--state-dir", dir); code != 2 || errOut == "" {
		t.Fatalf("drain without a node: exit %d", code)
	}
}

func TestRetryElsewhereCommand(t *testing.T) {
	dir := startServer(t)
	ctx := context.Background()
	work := t.TempDir()
	code, out, errOut := athena(ctx, "task", "Work here", "--dir", work, "--state-dir", dir)
	if code != 0 {
		t.Fatalf("task: %d %q", code, errOut)
	}
	id := strings.TrimSpace(out)
	runningAssignment(t, dir, id)
	local := fleet.LocalName(mustHostname(t))
	want := "This Ticket works in a folder on " + local + "; it continues when " + local + " is back.\n"
	if code, out, errOut := athena(ctx, "retry-elsewhere", id, "--state-dir", dir); code != 1 || out != "" || errOut != want {
		t.Fatalf("retry-elsewhere: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, _ := athena(ctx, "retry-elsewhere", "--state-dir", dir); code != 2 {
		t.Fatalf("retry-elsewhere without a ticket: exit %d", code)
	}
}

// TestNodeJoinAndRun is the whole path on one machine: the server prints a join command, a second state directory
// joins with it and runs as a node (fakeclaude, no codex), and the server lists it online; stopping it shows it
// offline.
func TestNodeJoinAndRun(t *testing.T) {
	dir, _ := startWebServer(t)
	ctx := context.Background()
	code, out, errOut := athena(ctx, "node", "token", "--state-dir", dir)
	m := joinLine.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("node token: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	nodeDir := newStateDir(t)
	if code, out, errOut := athena(ctx, "node", "join", m[1], m[2], "--ca-sha256", m[3], "--name", "box", "--state", nodeDir); code != 0 {
		t.Fatalf("join: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	// The token was single use.
	if code, _, errOut := athena(ctx, "node", "join", m[1], m[2], "--ca-sha256", m[3], "--name", "cube", "--state", newStateDir(t)); code != 1 || !strings.Contains(errOut, "already used") {
		t.Fatalf("second join: exit %d, stderr %q", code, errOut)
	}

	n := startNode(t, nodeDir)
	boxRow := func() string {
		_, out, _ := athena(ctx, "node", "list", "--state-dir", dir)
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "box ") {
				return strings.Join(strings.Fields(l), " ")
			}
		}
		return ""
	}
	eventually(t, "box online with its status", func() bool {
		r := boxRow()
		return strings.Contains(r, " online 0/3 ") && strings.Contains(r, "claude")
	})
	// Once it has the server's prompts, the node serves its own socket in its own state directory.
	eventually(t, "the node's socket", func() bool {
		_, err := os.Stat(filepath.Join(nodeDir, server.NodeSocket))
		return err == nil
	})
	// A second node on the same state directory is refused at once.
	if code, _, errOut := athena(ctx, "node", "run", "--state", nodeDir, "--claude", fakeClaude(t), "--codex", noCodex); code != 1 ||
		errOut != "Another athena node run already uses "+nodeDir+"; stop it first.\n" {
		t.Fatalf("second node run: exit %d, stderr %q", code, errOut)
	}
	if code := n.stop(); code != 0 {
		t.Fatalf("athena node run exit code = %d", code)
	}
	eventually(t, "box offline", func() bool { return strings.Contains(boxRow(), " offline ") })

	// Revoked while it runs: it stops, says so, and exits non-zero.
	n = startNode(t, nodeDir)
	eventually(t, "box online again", func() bool { return strings.Contains(boxRow(), " online ") })
	if code, _, errOut := athena(ctx, "node", "revoke", "box", "--yes", "--state-dir", dir); code != 0 {
		t.Fatalf("revoke: %d %q", code, errOut)
	}
	const revokedLine = "This node was revoked; run athena node join again.\n"
	// The server closed the link as revoked (not a refused handshake later).
	if code := n.wait(); code != 1 || !strings.HasSuffix(n.stderr(), revokedLine) || !strings.Contains(n.stderr(), "the server revoked this node") {
		t.Fatalf("revoked node run: exit %d, stderr %s", code, n.stderr())
	}
	// Started again, the server refuses its certificate at every handshake: it gives up the same way.
	n = startNode(t, nodeDir)
	if code := n.wait(); code != 1 || !strings.HasSuffix(n.stderr(), revokedLine) {
		t.Fatalf("node run after revoke: exit %d, stderr ends %q", code, tail(n.stderr()))
	}
}

func tail(s string) string {
	if len(s) > 300 {
		return s[len(s)-300:]
	}
	return s
}

// runningNode is an `athena node run` in this process.
type runningNode struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan int
	code   *int
	errb   *lockedWriter
	buf    *bytes.Buffer
}

// startNode runs `athena node run` on stateDir (fakeclaude, no codex, 3 slots) until the test ends or stop.
func startNode(t *testing.T, stateDir string) *runningNode {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	buf := &bytes.Buffer{}
	n := &runningNode{t: t, cancel: cancel, done: make(chan int, 1), buf: buf, errb: &lockedWriter{w: buf}}
	go func() {
		n.done <- Run(ctx, "node", []string{"run", "--state", stateDir, "--claude", fakeClaude(t), "--codex", noCodex, "--slots", "3"},
			io.Discard, n.errb)
	}()
	t.Cleanup(func() { n.stop() })
	return n
}

// wait waits for the node to end on its own and returns its exit code.
func (n *runningNode) wait() int {
	n.t.Helper()
	if n.code != nil {
		return *n.code
	}
	select {
	case code := <-n.done:
		n.code = &code
		return code
	case <-time.After(waitTimeout):
		n.t.Fatalf("athena node run did not end; stderr ends %q", tail(n.stderr()))
		return -1
	}
}

// stop stops the node and returns its exit code.
func (n *runningNode) stop() int {
	n.cancel()
	return n.wait()
}

func (n *runningNode) stderr() string {
	n.errb.mu.Lock()
	defer n.errb.mu.Unlock()
	return n.buf.String()
}
