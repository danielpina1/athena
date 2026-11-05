package server

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/web"
	"github.com/danielpina1/athena/internal/web/ui"
)

func TestWebDisabledByDefault(t *testing.T) {
	dir := newStateDir(t)
	cfg, err := LoadConfig(dir)
	if err != nil || cfg.WebOn() || cfg.Web != nil {
		t.Fatalf("default config: web %+v, %v", cfg.Web, err)
	}
	// A web part that is not enabled serves nothing either (athena web enroll can read its host).
	if err := SaveConfig(dir, FileConfig{Web: &WebConfig{Host: "athena.test", Listen: "127.0.0.1:0"}}); err != nil {
		t.Fatal(err)
	}
	ready := make(chan *Dispatcher, 1)
	s := startServerWith(t, dir, func(c *Config) { c.dispatcherReady = func(d *Dispatcher) { ready <- d } })
	if d := <-ready; d.web != nil || d.auth.Load() != nil {
		t.Fatal("the dashboard's endpoint runs without web.enabled")
	}
	var st WebStatusResult
	s.ok(Request{Op: OpWebStatus}, &st)
	if st.Serving || st.URL != "" {
		t.Fatalf("web.status = %+v", st)
	}
	for _, sub := range []string{"tls", "pki", "push"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); !os.IsNotExist(err) {
			t.Fatalf("%s created without web.enabled: %v", sub, err)
		}
	}
}

func TestWebConfigNamesTheField(t *testing.T) {
	for _, c := range []struct{ web, want string }{
		{`{"enabled":true}`, "web.host is required"},
		{`{"enabled":true,"host":"10.0.0.1"}`, "web.host"},
		{`{"enabled":true,"host":"a b"}`, "web.host"},
		{`{"host":"h.test","listen":"8443"}`, "web.listen"},
		{`{"host":"h.test","ips":["nope"]}`, "web.ips"},
		{`{"host":"h.test","tls":"maybe"}`, "web.tls"},
		{`{"host":"h.test","tls":"off","listen":":8080"}`, "web.tls off"},
		{`{"host":"h.test","tls":"off","listen":"127.0.0.1:8080"}`, "web.host must be localhost"},
		{`{"host":"h.test","tls":"acme"}`, "web.acme.provider is required"},
		{`{"host":"h.test","tls":"acme","acme":{"provider":"route53"}}`, "web.acme.provider"},
		{`{"host":"h.test","tls":"acme","acme":{"provider":"exec","directory":"http://x"}}`, "web.acme.directory"},
		{`{"host":"h.test","acme":{"agreeTOS":true,"bogus":1}}`, "unknown field"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(`{"web":`+c.web+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want %q", c.web, err, c.want)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(`{"web":{"enabled":true,"host":"Athena.Home.Arpa","ips":["10.8.0.1"],
		"tls":"acme","acme":{"provider":"cloudflare","email":"me@example.com","agreeTos":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Web; !cfg.WebOn() || w.Listen != DefaultWebListen || w.Host != "athena.home.arpa" || !w.ACME.AgreeTOS ||
		w.URL() != "https://athena.home.arpa:8443" {
		t.Fatalf("web = %+v (url %s)", w, w.URL())
	}
	if u := (WebConfig{Host: "h.test", Listen: ":443"}).URL(); u != "https://h.test" {
		t.Fatal(u)
	}
	if u := (WebConfig{Host: "localhost", Listen: "127.0.0.1:8080", TLS: "off"}).URL(); u != "http://localhost:8080" {
		t.Fatal(u)
	}
}

// pushService stands in for a browser vendor's push endpoint.
type pushService struct {
	srv  *httptest.Server
	reqs chan *http.Request
}

func newPushService(t *testing.T) *pushService {
	p := &pushService{reqs: make(chan *http.Request, 16)}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		p.reqs <- r
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// hookCall asks the node socket for a hook decision; it blocks while an Approval waits.
func hookCall(stateDir string, req node.HookRequest) (node.HookResponse, error) {
	var resp node.HookResponse
	conn, err := net.DialTimeout("unix", filepath.Join(stateDir, NodeSocket), time.Second)
	if err != nil {
		return resp, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return resp, err
	}
	err = json.NewDecoder(conn).Decode(&resp)
	return resp, err
}

// TestWebE2E is the whole path a phone takes, against `athena server` with web.enabled, tls ca on 127.0.0.1 and the
// software authenticator: the CA and the pages over HTTPS, enroll and log in, the WebSocket's snapshot and deltas, a
// stream, an Approval with its push, and the step-up for a sensitive command.
func TestWebE2E(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	push := newPushService(t)
	dir := newStateDir(t)
	w := startWebWith(t, dir, func(c *Config) { c.web.pushClient = push.srv.Client() })

	// The CA, the pages and the health check, over TLS that only the server's CA vouches for.
	get := func(path string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest("GET", w.base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = w.host
		resp, err := w.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if resp, body := get("/healthz"); resp.StatusCode != 200 || body != "ok" || resp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatalf("healthz = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, body := get("/ca.mobileconfig"); resp.StatusCode != 200 || !strings.Contains(body, "PayloadContent") {
		t.Fatalf("mobileconfig = %d", resp.StatusCode)
	}
	wantIndex := "Dashboard not built: run make web"
	if ui.Built() {
		wantIndex = `id="root"`
	}
	if resp, body := get("/"); resp.StatusCode != 200 || !strings.Contains(body, wantIndex) {
		t.Fatalf("index = %d %q", resp.StatusCode, body)
	}
	if resp, body := get("/push/key"); resp.StatusCode != 200 || body != w.ep.vapid.PublicKey() {
		t.Fatalf("push key = %d %q", resp.StatusCode, body)
	}

	// 1. Enroll and log in; subscribe to push while the login's assertion still vouches for the session.
	b := w.login()
	c := w.connect(b.session(), phoneHello())
	if wl := c.welcome(); !wl.GetSnapshotFollows() || wl.GetEpoch() != w.hub.Epoch() {
		t.Fatalf("welcome = %v", wl)
	}
	// 2. The snapshot.
	snap := c.snapshot()
	if len(snap.GetTickets()) != 0 || snap.GetNode().GetSlots() == 0 {
		t.Fatalf("snapshot = %v", snap)
	}
	if !hasHealth(snap.GetHealth(), "tls", true) {
		t.Fatalf("snapshot health lacks a healthy tls item: %v", snap.GetHealth())
	}
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sub := &athenav1.PushSubscribe{Endpoint: push.srv.URL + "/send/abc", P256Dh: base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()),
		Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}
	if r := c.do(&athenav1.Command{Id: "sub1", Action: &athenav1.Command_PushSubscribe{PushSubscribe: sub}}); !r.GetOk() {
		t.Fatalf("push subscribe = %v", r)
	}
	w.skew.Store(int64(web.ReauthWindow + time.Minute)) // from here on the login's assertion is too old

	// 3. A task through the dashboard; its Ticket's delta.
	r := c.do(submitCmd("task1", "deploy the thing"))
	if !r.GetOk() || r.GetTicketId() == "" {
		t.Fatalf("submit = %v", r)
	}
	id := r.GetTicketId()
	c.next("ticket delta", func(f *athenav1.Frame) bool {
		return f.GetDelta().GetTicket().GetId() == id && f.GetDelta().GetSeq() > snap.GetCursor()
	})
	a := w.running(id)

	// 4. The session's output.
	if err := c.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_StreamOpen{
		StreamOpen: &athenav1.StreamOpen{TicketId: id, FromOffset: 0}}}); err != nil {
		t.Fatal(err)
	}
	items := c.next("stream items", func(f *athenav1.Frame) bool { return f.GetStreamItems().GetTicketId() == id }).GetStreamItems()
	if len(items.GetItems()) == 0 || items.GetItems()[0].GetKind() != "init" {
		t.Fatalf("stream items = %v", items)
	}

	// The Pusher is wired to the Engine's stage and to the dashboards' presence.
	if st, err := w.ep.pusher.Stage(id); err != nil || st.Stage != "working" {
		t.Fatalf("pusher stage = %+v, %v", st, err)
	}
	if w.ep.pusher.Visible == nil || w.ep.pusher.Visible() {
		t.Fatal("pusher visibility is not the presence's")
	}
	w.ep.presence.Report(t, true)
	if !w.ep.pusher.Visible() {
		t.Fatal("pusher does not see a visible dashboard")
	}
	w.ep.presence.Forget(t)

	// 5. The fake session asks to write outside its directory: an Approval, its delta and a push.
	input, _ := json.Marshal(map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": "/etc/athena-e2e", "content": "x"}})
	hooked := make(chan node.HookResponse, 1)
	go func() {
		resp, err := hookCall(dir, node.HookRequest{Op: node.OpHook, Event: node.EventPreToolUse, Ticket: id, Assignment: a.ID, Input: input})
		if err != nil {
			resp.Reason = err.Error()
		}
		hooked <- resp
	}()
	ap := c.next("approval delta", func(f *athenav1.Frame) bool {
		return f.GetDelta().GetApproval().GetTicketId() == id
	}).GetDelta().GetApproval()
	select {
	case req := <-push.reqs:
		if req.URL.Path != "/send/abc" || req.Header.Get("Urgency") != "high" || req.Header.Get("Content-Encoding") != "aes128gcm" {
			t.Fatalf("push %s %v", req.URL.Path, req.Header)
		}
		if sub := vapidSubject(t, req.Header.Get("Authorization")); sub != w.origin {
			t.Fatalf("VAPID subject %q, want the origin %q", sub, w.origin)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no push for the approval")
	}

	// 6. Approving is not sensitive: no reauth needed.
	answer := &athenav1.Command{Id: "ans1", Action: &athenav1.Command_AnswerApproval{AnswerApproval: &athenav1.AnswerApproval{ApprovalId: ap.GetId(), Approve: true}}}
	if r := c.do(answer); !r.GetOk() {
		t.Fatalf("answer = %v", r)
	}
	select {
	case resp := <-hooked:
		if resp.Verdict != node.VerdictAllow {
			t.Fatalf("hook = %+v", resp)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the hook still waits after the approval")
	}

	// 7. A Policy change is: reauth_required, then after a passkey assertion it goes through.
	var shown PolicyView
	w.ok(Request{Op: OpPolicyShow}, &shown)
	settings, err := json.Marshal(shown.Settings)
	if err != nil {
		t.Fatal(err)
	}
	set := &athenav1.Command{Id: "pol1", Action: &athenav1.Command_SetPolicy{SetPolicy: &athenav1.SetPolicy{SettingsJson: string(settings)}}}
	if r := c.do(set); r.GetCode() != web.CodeReauthRequired {
		t.Fatalf("set policy without reauth = %v", r)
	}
	b.reauth()
	if r := c.do(set); !r.GetOk() || r.GetPolicyRevision() != 1 {
		t.Fatalf("set policy after reauth = %v", r)
	}

	var st WebStatusResult
	w.ok(Request{Op: OpWebStatus}, &st)
	if !st.Serving || st.URL != w.origin || st.TLS != "ca" || !st.TLSOK || st.CertNotAfter == nil || st.Clients != 1 {
		t.Fatalf("web.status = %+v", st)
	}

	// Shutdown ends the connection (going away) before the store closes.
	w.stop()
	select {
	case err := <-c.done:
		c.done <- err // for the cleanup's close
		if code := websocket.CloseStatus(err); code != websocket.StatusGoingAway {
			t.Fatalf("closed with %d (%v), want 1001", code, err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("connection open after the server stopped")
	}
	// The hub's loop returned before Run did: a feed taken now ends after its replay, at once.
	w.hub.Wait()
	feed, _ := w.hub.Subscribe(0, w.hub.Epoch())
	for open := true; open; {
		select {
		case _, open = <-feed.C():
		case <-time.After(time.Second):
			t.Fatal("the hub still runs after the server stopped")
		}
	}
}

// TestWebSetupNeedsALoadableConfig: setup creates nothing (no CA) for a configuration it could not save.
func TestWebSetupNeedsALoadableConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(`{"evalTimeoutSeconds":-1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetupWeb(dir, WebConfig{Host: "athena.test"}); err == nil || !strings.Contains(err.Error(), "evalTimeoutSeconds") {
		t.Fatalf("setup over a bad config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pki")); !os.IsNotExist(err) {
		t.Fatalf("a CA was created for a configuration that was not saved: %v", err)
	}
}

func hasHealth(h *athenav1.Health, name string, ok bool) bool {
	for _, it := range h.GetItems() {
		if it.GetName() == name && it.GetOk() == ok {
			return true
		}
	}
	return false
}

// vapidSubject is the sub claim of a "vapid t=<jwt>, k=<key>" Authorization header.
func vapidSubject(t *testing.T, auth string) string {
	t.Helper()
	tok, _, _ := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ",")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("authorization %q", auth)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims.Sub
}

// TestWebTLSOffOnLoopback: mode off serves plain HTTP on loopback, without HSTS and without push (no https origin).
func TestWebTLSOffOnLoopback(t *testing.T) {
	dir := newStateDir(t)
	if err := SetupWeb(dir, WebConfig{Listen: "127.0.0.1:0", Host: "localhost", TLS: "off"}); err != nil {
		t.Fatal(err)
	}
	eps := make(chan *webEndpoint, 1)
	startServerWith(t, dir, func(c *Config) { c.web.ready = func(ep *webEndpoint) { eps <- ep } })
	ep := <-eps
	if !strings.HasPrefix(ep.origin, "http://localhost:") || ep.vapid != nil || ep.pusher != nil || ep.tls.CA() != nil {
		t.Fatalf("origin %s, vapid %v, pusher %v", ep.origin, ep.vapid, ep.pusher)
	}
	base := "http://" + ep.l.Addr().String()
	client := &http.Client{Timeout: waitTimeout, Transport: &http.Transport{DisableKeepAlives: true}}
	for path, want := range map[string]int{"/healthz": 200, "/push/key": 404, "/ca.pem": 404} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want || resp.Header.Get("Strict-Transport-Security") != "" {
			t.Fatalf("%s = %d (HSTS %q)", path, resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "push")); !os.IsNotExist(err) {
		t.Fatalf("push key made without an https origin: %v", err)
	}
}
