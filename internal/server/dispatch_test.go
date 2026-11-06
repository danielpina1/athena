package server

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/hub"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
	"github.com/danielpina1/athena/internal/web/webauthntest"
)

// The dashboard end to end: a real server (fake claude) with web.enabled, tls ca on 127.0.0.1 and an ephemeral port,
// reached over HTTPS with the server's own CA as the only root.

const webHost = "athena.test"

type webServer struct {
	*testServer
	d  *Dispatcher
	ep *webEndpoint
	// hub is the endpoint's; origin its https://athena.test:<port>; base the URL the client dials (the loopback
	// address, with athena.test as the TLS server name); client trusts only <stateDir>/pki/ca.pem.
	hub    *hub.Hub
	origin string
	base   string
	host   string
	client *http.Client
	down   bool
	stop   func()
	// skew moves the login's clock ahead of real time; authn is the owner's passkey once enrolled.
	skew  atomic.Int64
	authn *webauthntest.SoftAuthenticator
}

// startWeb starts the server in stateDir with its dashboard's endpoint.
func startWeb(t *testing.T, stateDir string) *webServer {
	t.Helper()
	return startWebWith(t, stateDir, nil)
}

// startWebWith is startWeb with the test configuration changed by mod.
func startWebWith(t *testing.T, stateDir string, mod func(*Config)) *webServer {
	t.Helper()
	if err := SetupWeb(stateDir, WebConfig{Listen: "127.0.0.1:0", Host: webHost, TLS: "ca"}); err != nil {
		t.Fatal(err)
	}
	w := &webServer{}
	ready := make(chan *Dispatcher, 1)
	eps := make(chan *webEndpoint, 1)
	ts := startServerWith(t, stateDir, func(c *Config) {
		c.dispatcherReady = func(d *Dispatcher) { ready <- d }
		c.web.now = func() time.Time { return time.Now().Add(time.Duration(w.skew.Load())) }
		c.web.ready = func(ep *webEndpoint) { eps <- ep }
		if mod != nil {
			mod(c)
		}
	})
	w.testServer, w.d, w.ep = ts, <-ready, <-eps
	w.hub, w.origin = w.ep.hub, w.ep.origin
	_, port, _ := net.SplitHostPort(w.ep.l.Addr().String())
	w.base, w.host = "https://127.0.0.1:"+port, net.JoinHostPort(webHost, port)
	pemBytes, err := os.ReadFile(filepath.Join(stateDir, "pki", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		t.Fatal("ca.pem holds no certificate")
	}
	w.client = &http.Client{Timeout: waitTimeout, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: webHost, MinVersion: tls.VersionTLS12}}}
	w.stop = func() {
		if w.down {
			return
		}
		w.down = true
		ts.stop()
		w.client.CloseIdleConnections()
	}
	t.Cleanup(w.stop)
	return w
}

// browser is a signed-in browser: the cookies the login handlers set.
type browser struct {
	w       *webServer
	cookies map[string]*http.Cookie
}

// post sends a POST with the configured Origin and the browser's cookies, keeps the cookies it sets and returns the
// body; anything but 200 fails the test.
func (b *browser) post(path string, body []byte) []byte {
	t := b.w.t
	t.Helper()
	req, err := http.NewRequest("POST", b.w.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", b.w.origin)
	req.Header.Set("Content-Type", "application/json")
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	req.Host = b.w.host
	resp, err := b.w.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s = %d %s", path, resp.StatusCode, out)
	}
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return out
}

// session is the session cookie's value.
func (b *browser) session() string { return b.cookies[web.SessionCookie].Value }

// reauth runs a passkey reauthentication for the browser's session.
func (b *browser) reauth() {
	b.w.t.Helper()
	begin := b.post("/auth/reauth/begin", nil)
	b.post("/auth/reauth/finish", b.w.authn.Assert(b.w.t, begin))
}

// login enrolls the software authenticator (the first time) and signs in with it through the login handlers.
func (w *webServer) login() *browser {
	w.t.Helper()
	if w.authn == nil {
		w.authn = webauthntest.New(w.t, w.origin, webHost)
		token, err := web.IssueEnrollToken(w.d.st, 0)
		if err != nil {
			w.t.Fatal(err)
		}
		reg := &browser{w: w, cookies: map[string]*http.Cookie{}}
		body, _ := json.Marshal(map[string]string{"token": token, "name": "phone"})
		begin := reg.post("/auth/register/begin", body)
		reg.post("/auth/register/finish", w.authn.Create(w.t, begin))
	}
	b := &browser{w: w, cookies: map[string]*http.Cookie{}}
	begin := b.post("/auth/login/begin", nil)
	b.post("/auth/login/finish", w.authn.Assert(w.t, begin))
	return b
}

// wsClient is a dashboard's end of the link.
type wsClient struct {
	t       *testing.T
	sess    *link.Session
	frames  chan *athenav1.Frame
	backlog []*athenav1.Frame
	done    chan error
	cancel  context.CancelFunc
}

func (w *webServer) connect(cookie string, hello *athenav1.Hello) *wsClient {
	t := w.t
	t.Helper()
	h := http.Header{}
	h.Set("Origin", w.origin)
	h.Set("Cookie", web.SessionCookie+"="+cookie)
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(w.base, "https")+"/ws", &websocket.DialOptions{
		HTTPClient: w.client, HTTPHeader: h, Host: w.host, Subprotocols: []string{link.Subprotocol},
		CompressionMode: websocket.CompressionNoContextTakeover,
	})
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	cl := &wsClient{t: t, frames: make(chan *athenav1.Frame, 4096), done: make(chan error, 1)}
	cl.sess = link.New(link.NewWS(c), link.Options{OnFrame: func(f *athenav1.Frame) { cl.frames <- f }})
	if err := cl.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_Hello{Hello: hello}}); err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	cl.cancel = rcancel
	go func() { cl.done <- cl.sess.Run(rctx) }()
	t.Cleanup(cl.close)
	return cl
}

// close ends the connection and waits for it; it is idempotent.
func (c *wsClient) close() {
	c.cancel()
	select {
	case err := <-c.done:
		c.done <- err
	case <-time.After(waitTimeout):
	}
}

// next returns the first frame not taken yet that match accepts; the frames it passes over stay for later calls.
func (c *wsClient) next(what string, match func(*athenav1.Frame) bool) *athenav1.Frame {
	c.t.Helper()
	for i, f := range c.backlog {
		if match(f) {
			c.backlog = append(c.backlog[:i], c.backlog[i+1:]...)
			return f
		}
	}
	deadline := time.After(waitTimeout)
	for {
		select {
		case f := <-c.frames:
			if match(f) {
				return f
			}
			c.backlog = append(c.backlog, f)
		case err := <-c.done:
			c.done <- err
			c.t.Fatalf("waiting for %s: connection ended: %v", what, err)
		case <-deadline:
			c.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (c *wsClient) welcome() *athenav1.Welcome {
	c.t.Helper()
	return c.next("welcome", func(f *athenav1.Frame) bool { return f.GetWelcome() != nil }).GetWelcome()
}

func (c *wsClient) snapshot() *athenav1.Snapshot {
	c.t.Helper()
	return c.next("snapshot", func(f *athenav1.Frame) bool { return f.GetSnapshot() != nil }).GetSnapshot()
}

func (c *wsClient) send(cmd *athenav1.Command) {
	c.t.Helper()
	if err := c.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_Command{Command: cmd}}); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wsClient) do(cmd *athenav1.Command) *athenav1.CommandResult {
	c.t.Helper()
	c.send(cmd)
	return c.next("result of "+cmd.GetId(), func(f *athenav1.Frame) bool {
		return f.GetCommandResult() != nil && f.GetCommandResult().GetId() == cmd.GetId()
	}).GetCommandResult()
}

func phoneHello() *athenav1.Hello { return &athenav1.Hello{Protocol: web.Protocol, ClientId: "phone"} }

func submitCmd(id, text string) *athenav1.Command {
	return &athenav1.Command{Id: id, Action: &athenav1.Command_SubmitTask{SubmitTask: &athenav1.SubmitTask{Text: text}}}
}

func (w *webServer) tickets() []TicketSummary {
	w.t.Helper()
	var list []TicketSummary
	w.ok(Request{Op: OpTickets, All: true}, &list)
	return list
}

func TestHelloWelcomeSnapshotThenDeltas(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	w := startWeb(t, newStateDir(t))
	c := w.connect(w.login().session(), phoneHello())
	if wl := c.welcome(); !wl.GetSnapshotFollows() || wl.GetEpoch() != w.hub.Epoch() {
		t.Fatalf("welcome = %v", wl)
	}
	snap := c.snapshot()
	if len(snap.GetTickets()) != 0 || snap.GetPolicy() == nil || snap.GetNode().GetSlots() == 0 {
		t.Fatalf("snapshot = %v", snap)
	}

	id := w.submit("say hello") // through the control socket
	d := c.next("ticket delta", func(f *athenav1.Frame) bool { return f.GetDelta().GetTicket().GetId() == id }).GetDelta()
	if d.GetSeq() <= snap.GetCursor() || d.GetTicket().GetTitle() != "say hello" {
		t.Fatalf("delta = %v, snapshot cursor %d", d, snap.GetCursor())
	}
}

func TestServerRestartForcesSnapshot(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	w := startWeb(t, dir)
	cookie := w.login().session()
	c := w.connect(cookie, phoneHello())
	epoch := c.welcome().GetEpoch()
	cursor := c.snapshot().GetCursor()
	id := w.submit("before the restart")
	cursor = c.next("delta", func(f *athenav1.Frame) bool { return f.GetDelta().GetTicket().GetId() == id }).GetDelta().GetSeq()
	c.close()
	w.stop()

	w = startWeb(t, dir)
	c = w.connect(cookie, &athenav1.Hello{Protocol: web.Protocol, ClientId: "phone", Epoch: epoch, Cursor: cursor})
	wl := c.welcome()
	if wl.GetEpoch() == epoch || !wl.GetSnapshotFollows() {
		t.Fatalf("welcome after restart = %v (old epoch %s)", wl, epoch)
	}
	snap := c.snapshot()
	if len(snap.GetTickets()) != 1 || snap.GetTickets()[0].GetId() != id {
		t.Fatalf("snapshot tickets = %v, want %s", snap.GetTickets(), id)
	}
}

func TestRetriedCommandActsOnce(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	w := startWeb(t, newStateDir(t))
	cookie := w.login().session()
	c := w.connect(cookie, phoneHello())
	c.welcome()
	c.send(submitCmd("c1", "only once"))
	// The server acted; the connection drops before the client reads the result.
	eventually(t, "the ticket exists", func() bool { return len(w.tickets()) == 1 })
	c.close()

	raw, ok, err := w.d.st.CommandResult("phone", "c1")
	if err != nil || !ok {
		t.Fatalf("stored result: %v %v", ok, err)
	}
	first := &athenav1.CommandResult{}
	if err := proto.Unmarshal(raw, first); err != nil {
		t.Fatal(err)
	}
	c = w.connect(cookie, phoneHello())
	c.welcome()
	again := c.do(submitCmd("c1", "only once"))
	if !again.GetOk() || again.GetTicketId() == "" || again.GetTicketId() != first.GetTicketId() {
		t.Fatalf("resent result %v, first %v", again, first)
	}
	if list := w.tickets(); len(list) != 1 || list[0].ID != again.GetTicketId() {
		t.Fatalf("tickets = %v", list)
	}
	// A different client's c1 is another command.
	other := w.connect(cookie, &athenav1.Hello{Protocol: web.Protocol, ClientId: "laptop"})
	other.welcome()
	if r := other.do(submitCmd("c1", "from the laptop")); !r.GetOk() || r.GetTicketId() == first.GetTicketId() {
		t.Fatalf("laptop c1 = %v", r)
	}

	// A reply resent is one Input.
	tid := first.GetTicketId()
	reply := &athenav1.Command{Id: "r1", Action: &athenav1.Command_Reply{Reply: &athenav1.Reply{TicketId: tid, Text: "more"}}}
	for range 2 {
		if r := c.do(reply); !r.GetOk() {
			t.Fatalf("reply: %v", r)
		}
	}
	tk, _, err := w.d.eng.Ticket(tid)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tk.Inputs); n != 2 {
		t.Fatalf("%d inputs, want the task and one reply", n)
	}
	if r := c.do(&athenav1.Command{Id: "s1", Action: &athenav1.Command_Stop{Stop: &athenav1.Stop{TicketId: "T-9999"}}}); r.GetCode() != web.CodeNotFound {
		t.Fatalf("stop unknown = %v", r)
	}
}

func TestSensitiveCommandNeedsReauth(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	var shown PolicyView
	w.ok(Request{Op: OpPolicyShow}, &shown)
	settings, err := json.Marshal(shown.Settings)
	if err != nil {
		t.Fatal(err)
	}
	setPolicy := func(id string, base uint32) *athenav1.Command {
		return &athenav1.Command{Id: id, Action: &athenav1.Command_SetPolicy{SetPolicy: &athenav1.SetPolicy{BaseRevision: base, SettingsJson: string(settings)}}}
	}
	sensitive := []*athenav1.Command{
		setPolicy("p1", 0),
		{Id: "k1", Action: &athenav1.Command_RevokePasskey{RevokePasskey: &athenav1.RevokePasskey{Id: "any"}}},
		{Id: "u1", Action: &athenav1.Command_PushSubscribe{PushSubscribe: &athenav1.PushSubscribe{Endpoint: "https://push.test/x"}}},
		{Id: "u2", Action: &athenav1.Command_PushUnsubscribe{PushUnsubscribe: &athenav1.PushUnsubscribe{Endpoint: "https://push.test/x"}}},
		{Id: "u3", Action: &athenav1.Command_SetNotificationPrefs{SetNotificationPrefs: &athenav1.SetNotificationPrefs{}}},
	}
	b := w.login()
	w.skew.Store(int64(web.ReauthWindow + time.Minute)) // the login's assertion is too old now
	c := w.connect(b.session(), phoneHello())
	c.welcome()
	for _, cmd := range sensitive {
		if r := c.do(cmd); r.GetCode() != web.CodeReauthRequired {
			t.Fatalf("%s without reauth = %v", cmd.GetId(), r)
		}
	}
	if r := c.do(&athenav1.Command{Id: "l1", Action: &athenav1.Command_ListPasskeys{ListPasskeys: &athenav1.ListPasskeys{}}}); !r.GetOk() || len(r.GetPasskeys().GetPasskeys()) != 1 {
		t.Fatalf("list passkeys = %v", r)
	}
	if _, rev := w.d.policy.current(); rev != 0 {
		t.Fatalf("policy revision %d after refused sets", rev)
	}

	// The same command id goes through after a reauthentication: a reauth_required result is not kept.
	b.reauth()
	fresh := c
	if r := fresh.do(setPolicy("p1", 0)); !r.GetOk() || r.GetPolicyRevision() != 1 {
		t.Fatalf("set policy = %v", r)
	}
	if r := fresh.do(setPolicy("p2", 0)); r.GetCode() != web.CodeConflict {
		t.Fatalf("set policy on a stale base = %v", r)
	}
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sub := &athenav1.PushSubscribe{Endpoint: "https://push.test/x", P256Dh: base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()),
		Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}
	if r := fresh.do(&athenav1.Command{Id: "u1", Action: &athenav1.Command_PushSubscribe{PushSubscribe: &athenav1.PushSubscribe{Endpoint: "https://push.test/x"}}}); r.GetCode() != web.CodeInvalid {
		t.Fatalf("push subscribe without keys = %v", r)
	}
	if r := fresh.do(&athenav1.Command{Id: "u1b", Action: &athenav1.Command_PushSubscribe{PushSubscribe: &athenav1.PushSubscribe{Endpoint: "http://push.test/x", P256Dh: sub.P256Dh, Auth: sub.Auth}}}); r.GetCode() != web.CodeInvalid {
		t.Fatalf("push subscribe over http = %v", r)
	}
	if r := fresh.do(&athenav1.Command{Id: "u1c", Action: &athenav1.Command_PushSubscribe{PushSubscribe: sub}}); !r.GetOk() {
		t.Fatalf("push subscribe = %v", r)
	}
	if subs, _ := w.d.st.PushSubscriptions(); len(subs) != 1 || subs[0].Endpoint != sub.Endpoint {
		t.Fatalf("subscriptions %v", subs)
	}
	if r := fresh.do(&athenav1.Command{Id: "u3b", Action: &athenav1.Command_SetNotificationPrefs{SetNotificationPrefs: &athenav1.SetNotificationPrefs{Muted: []string{"review", "failed"}}}}); !r.GetOk() {
		t.Fatalf("prefs = %v", r)
	}
	if m, _ := web.MutedKinds(w.d.st); len(m) != 2 {
		t.Fatalf("muted %v", m)
	}
	if r := fresh.do(&athenav1.Command{Id: "u3c", Action: &athenav1.Command_SetNotificationPrefs{SetNotificationPrefs: &athenav1.SetNotificationPrefs{Muted: []string{"nope"}}}}); r.GetCode() != web.CodeInvalid {
		t.Fatalf("bad prefs = %v", r)
	}
	if r := fresh.do(&athenav1.Command{Id: "u2b", Action: &athenav1.Command_PushUnsubscribe{PushUnsubscribe: &athenav1.PushUnsubscribe{Endpoint: sub.Endpoint}}}); !r.GetOk() {
		t.Fatalf("unsubscribe = %v", r)
	}
	if subs, _ := w.d.st.PushSubscriptions(); len(subs) != 0 {
		t.Fatalf("subscriptions %v", subs)
	}
	fresh.next("policy delta", func(f *athenav1.Frame) bool { return f.GetDelta().GetPolicy().GetRevision() == 1 })
}

func TestInterruptReachesSession(t *testing.T) {
	fe := newFakeEnv(t, openTurnFixture(t))
	w := startWeb(t, newStateDir(t))
	id := w.submit("long task")
	w.running(id)
	c := w.connect(w.login().session(), phoneHello())
	c.welcome()
	interrupt := func(cmdID, ticket string) *athenav1.CommandResult {
		return c.do(&athenav1.Command{Id: cmdID, Action: &athenav1.Command_Interrupt{Interrupt: &athenav1.Interrupt{TicketId: ticket}}})
	}
	if r := interrupt("i1", id); !r.GetOk() {
		t.Fatalf("interrupt = %v", r)
	}
	eventually(t, "the interrupt on the session's stdin", func() bool {
		return strings.Contains(fe.stdin(t), `"subtype":"interrupt"`)
	})
	if r := interrupt("i2", "T-9999"); r.GetCode() != web.CodeNotFound {
		t.Fatalf("interrupt unknown = %v", r)
	}
	w.ok(Request{Op: OpStop, ID: id}, nil)
	eventually(t, "the session ended", func() bool { return w.d.node.Interrupt(id) != nil })
	if r := interrupt("i3", id); r.GetCode() != web.CodeUnavailable {
		t.Fatalf("interrupt without a session = %v", r)
	}
}

func TestWebRevokeClosesConnections(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	c := w.connect(w.login().session(), phoneHello())
	c.welcome()
	var out WebRevokeResult
	w.ok(Request{Op: OpWebRevoke, ID: w.authn.CredentialID()}, &out)
	if out.Sessions != 1 {
		t.Fatalf("ended %d sessions", out.Sessions)
	}
	select {
	case err := <-c.done:
		c.done <- err // for the cleanup's close
		if code := websocket.CloseStatus(err); code != web.CloseSessionEnded {
			t.Fatalf("close %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("connection open 1 s after the revoke")
	}
	if r, err := call(w.stateDir, Request{Op: OpWebRevoke, ID: w.authn.CredentialID()}); err != nil || r.OK || !strings.Contains(r.Error, "not found") {
		t.Fatalf("second revoke = %+v %v", r, err)
	}
}

// TestControlSocketUnchanged pins the control socket's bytes for a few ops; the CLI and server tests cover the rest.
func TestControlSocketUnchanged(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	s := startServer(t, newStateDir(t))
	raw := func(line string) string {
		conn, err := net.DialTimeout("unix", filepath.Join(s.stateDir, ControlSocket), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, line+"\n"); err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(conn)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, c := range []struct{ req, want string }{
		{`{"op":"ping"}`, `{"ok":true}` + "\n"},
		{`{"op":"submit","text":"one"}`, `{"ok":true,"data":{"id":"T-0001"}}` + "\n"},
		{`{"op":"submit","text":"two","dir":"rel"}`, `{"ok":false,"error":"directory rel must be an absolute path"}` + "\n"},
		{`{"op":"reply","id":"T-0042","text":"x"}`, `{"ok":false,"error":"ticket T-0042 not found"}` + "\n"},
		{`{"op":"nope"}`, `{"ok":false,"error":"unknown op \"nope\""}` + "\n"},
	} {
		if got := raw(c.req); got != c.want {
			t.Fatalf("%s:\n got %q\nwant %q", c.req, got, c.want)
		}
	}
}

func TestHubSourceRecentThreads(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	err = st.Tx(func(tx *store.Tx) error {
		for _, th := range []*core.Thread{{Key: "x:a"}, {Key: "x:b"}, {Key: "x:quiet"}, {Key: "x:busy", Queued: []string{"q"}}} {
			if err := tx.PutThread(th); err != nil {
				return err
			}
		}
		for _, tr := range []*core.Trigger{
			{ID: "t1", At: t0, Threads: []string{"x:a"}},
			{ID: "t2", At: t0.Add(2 * time.Minute), Threads: []string{"x:b", "x:a"}},
			{ID: "t3", At: t0.Add(time.Minute), Threads: []string{"x:gone"}},
		} {
			if err := tx.PutTrigger(tr); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	src := newHubSource(&Dispatcher{st: st})
	got, err := src.RecentThreads(100)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, th := range got {
		keys = append(keys, th.Key)
	}
	if strings.Join(keys, " ") != "x:b x:a x:busy" {
		t.Fatalf("recent threads = %v, want the newest Triggers' (stored) Threads, then the active", keys)
	}
}

func TestConcurrentCopiesOfACommandActOnce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eng := core.NewEngine(st.Repos(), nopExec{}, nil, nil)
	tk, err := eng.SubmitOwnerTask("needs approval", core.TaskOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := eng.RequestApproval(core.Approval{TicketID: tk.ID, Tool: "Bash"})
	if err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{st: st, eng: eng, log: discardLogger()}
	var first atomic.Bool
	d.beforeStore = func() {
		if first.CompareAndSwap(false, true) {
			time.Sleep(200 * time.Millisecond) // the first copy acted; the others arrive before its result is stored
		}
	}
	// The old connection's copy and the reconnected client's resend, at once: answering twice would refuse one of them
	// (the Approval is no longer pending) and could store that refusal as the result.
	cmd := &athenav1.Command{Id: "a1", Action: &athenav1.Command_AnswerApproval{AnswerApproval: &athenav1.AnswerApproval{ApprovalId: a.ID, Approve: true}}}
	const copies = 8
	results := make([]*athenav1.CommandResult, copies)
	var wg sync.WaitGroup
	for i := range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = d.Do(context.Background(), web.Origin{ClientID: "phone"}, proto.Clone(cmd).(*athenav1.Command))
		}()
	}
	wg.Wait()
	for i, r := range results {
		if !r.GetOk() || !proto.Equal(r, results[0]) {
			t.Fatalf("copy %d = %v, copy 0 = %v: want every copy ok and alike", i, r, results[0])
		}
	}
	got, err := st.AnsweredApprovals(0)
	if err != nil || len(got) != 1 || got[0].State != core.ApprovalApproved {
		t.Fatalf("answered approvals = %+v %v", got, err)
	}
	if len(d.inflight.m) != 0 {
		t.Fatalf("%d command locks left", len(d.inflight.m))
	}
}

// A push subscription belongs to the session that made it: logout and revoke end it, and a fresh login subscribes again.
func TestPushSubscriptionEndsWithItsSession(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	subscribe := func(b *browser, id, endpoint string) {
		t.Helper()
		ua, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		c := w.connect(b.session(), phoneHello())
		c.welcome()
		sub := &athenav1.PushSubscribe{Endpoint: endpoint, P256Dh: base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()),
			Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}
		if r := c.do(&athenav1.Command{Id: id, Action: &athenav1.Command_PushSubscribe{PushSubscribe: sub}}); !r.GetOk() {
			t.Fatalf("subscribe %s = %v", endpoint, r)
		}
	}
	endpoints := func() []string {
		t.Helper()
		subs, err := w.d.st.PushSubscriptions()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range subs {
			out = append(out, s.Endpoint)
		}
		return out
	}
	one, two := w.login(), w.login()
	subscribe(one, "s1", "https://push.test/one")
	subscribe(two, "s2", "https://push.test/two")
	if got := endpoints(); len(got) != 2 {
		t.Fatalf("subscriptions %v", got)
	}

	one.post("/auth/logout", nil)
	if got := endpoints(); len(got) != 1 || got[0] != "https://push.test/two" {
		t.Fatalf("after logout of the first session: %v", got)
	}

	// A fresh login subscribes the same endpoint again, and it moves to the new session.
	three := w.login()
	subscribe(three, "s3", "https://push.test/one")
	if got := endpoints(); len(got) != 2 {
		t.Fatalf("after resubscribing: %v", got)
	}

	// Revoking the passkey ends every session and so every subscription.
	pks, err := w.d.st.Passkeys()
	if err != nil || len(pks) != 1 {
		t.Fatalf("passkeys %v %v", pks, err)
	}
	if n, err := w.d.revokePasskey(pks[0].ID); err != nil || n != 2 {
		t.Fatalf("revoke = %d, %v", n, err)
	}
	if got := endpoints(); len(got) != 0 {
		t.Fatalf("subscriptions survived the revoke: %v", got)
	}
}

func TestPruneDropsExpiredSessionsAndTokens(t *testing.T) {
	w := startWeb(t, newStateDir(t))
	b := w.login()
	hash, err := hex.DecodeString(w.d.sessionHashOf(t, b))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-store.WebSessionAbsolute - time.Hour)
	if err := w.d.st.PutWebSession(store.WebSession{Hash: []byte("stale"), CreatedAt: old, SeenAt: old, ReauthAt: old, PasskeyID: mustPasskey(t, w.d.st)}); err != nil {
		t.Fatal(err)
	}
	if err := w.d.st.PutPushSubscription(store.PushSubscription{SessionHash: []byte("stale"), Endpoint: "https://push.test/stale", P256dh: "k", Auth: "a", CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := w.d.st.PutEnrollToken([]byte("expired"), old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.d.pruneResults(ctx) // prunes once, then sees the cancelled context
	if _, ok, _ := w.d.st.WebSession([]byte("stale")); ok {
		t.Fatal("expired session not pruned")
	}
	if subs, _ := w.d.st.PushSubscriptions(); len(subs) != 0 {
		t.Fatalf("a pruned session kept its subscription: %v", subs)
	}
	if n, _ := w.d.st.PruneEnrollTokens(time.Now()); n != 0 {
		t.Fatalf("expired token left for the next prune: %d", n)
	}
	if _, ok, _ := w.d.st.WebSession(hash); !ok {
		t.Fatal("the live session was pruned")
	}
}

func mustPasskey(t *testing.T, st *store.Store) string {
	t.Helper()
	pks, err := st.Passkeys()
	if err != nil || len(pks) == 0 {
		t.Fatalf("passkeys %v %v", pks, err)
	}
	return pks[0].ID
}

// sessionHashOf is the hex hash of the browser's session, read from the store through Auth.
func (d *Dispatcher) sessionHashOf(t *testing.T, b *browser) string {
	t.Helper()
	req, _ := http.NewRequest("GET", "/", nil)
	req.AddCookie(b.cookies[web.SessionCookie])
	s, ok := d.auth.Load().Session(req)
	if !ok {
		t.Fatal("no session")
	}
	return s.Hash
}

// A task submitted from the dashboard names its runtime and whether a review pairs it: review unset leaves the Policy's
// default (none here), true and false are explicit.
func TestSubmitTaskRuntimeAndReview(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	w := startWeb(t, newStateDir(t))
	c := w.connect(w.login().session(), phoneHello())
	c.welcome()
	c.snapshot()
	yes, no := true, false
	for i, tc := range []struct {
		runtime string
		review  *bool
		want    string // the Ticket pairing: off is none
		rt      string
	}{
		{"", nil, "", core.RuntimeAuto},
		{core.RuntimeCodex, &yes, core.PairingReview, core.RuntimeCodex},
		{core.RuntimeClaude, &no, "", core.RuntimeClaude},
	} {
		res := c.do(&athenav1.Command{Id: fmt.Sprintf("s%d", i), Action: &athenav1.Command_SubmitTask{
			SubmitTask: &athenav1.SubmitTask{Text: fmt.Sprintf("task %d", i), Runtime: tc.runtime, Review: tc.review}}})
		if !res.GetOk() || res.GetTicketId() == "" {
			t.Fatalf("case %d: result %v", i, res)
		}
		d := w.detail(res.GetTicketId())
		if d.Runtime != tc.rt || d.Pairing != tc.want {
			t.Errorf("case %d: ticket runtime %q pairing %q, want %q %q", i, d.Runtime, d.Pairing, tc.rt, tc.want)
		}
	}
}

func TestSubmitTaskUnknownRuntimeInvalid(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	w := startWeb(t, newStateDir(t))
	c := w.connect(w.login().session(), phoneHello())
	c.welcome()
	c.snapshot()
	res := c.do(&athenav1.Command{Id: "bad", Action: &athenav1.Command_SubmitTask{SubmitTask: &athenav1.SubmitTask{Text: "x", Runtime: "gemini"}}})
	if res.GetOk() || res.GetCode() != web.CodeInvalid || !strings.Contains(res.GetMessage(), "gemini") {
		t.Fatalf("result %v, want invalid naming the runtime", res)
	}
	if n := len(w.tickets()); n != 0 {
		t.Errorf("%d tickets made by an invalid submit", n)
	}
}
