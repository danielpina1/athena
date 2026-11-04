package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
)

// The dashboard end to end: a real server (fake claude), its Dispatcher, a hub over the server's Source adapter and a
// web.Dashboard behind a TLS test server. Task 11 wires these into Run; here the test builds them around it.

const (
	webOrigin  = "https://athena.test"
	webPasskey = "pk-phone"
)

type webServer struct {
	*testServer
	d    *Dispatcher
	hub  *hub.Hub
	srv  *httptest.Server
	down bool
	stop func()
}

// startWeb starts the server in stateDir and its dashboard.
func startWeb(t *testing.T, stateDir string) *webServer {
	t.Helper()
	ready := make(chan *Dispatcher, 1)
	ts := startServerWith(t, stateDir, func(c *Config) { c.dispatcherReady = func(d *Dispatcher) { ready <- d } })
	d := <-ready
	w := &webServer{testServer: ts, d: d}
	w.hub = hub.New(newHubSource(d), discardLogger())
	d.st.OnCommit(w.hub.OnCommit)
	ctx, cancel := context.WithCancel(context.Background())
	w.hub.Start(ctx)
	auth, err := web.NewAuth(d.st, webOrigin, "athena.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.SetAuth(auth)
	dash := web.NewDashboard(web.DashboardConfig{Auth: auth, Hub: w.hub, Commander: d, Log: discardLogger(), Version: "test"})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", dash.ServeConn)
	w.srv = httptest.NewTLSServer(mux)
	w.stop = func() {
		if w.down {
			return
		}
		w.down = true
		w.srv.Close()
		cancel()
		d.st.OnCommit(nil)
		ts.stop()
	}
	t.Cleanup(w.stop)
	return w
}

// login stores a passkey and a session for it, as a finished login would, and returns the cookie value. reauthAt is
// the session's last passkey assertion.
func (w *webServer) login(reauthAt time.Time) string {
	w.t.Helper()
	if _, ok, _ := w.d.st.Passkey(webPasskey); !ok {
		if err := w.d.st.PutPasskey(store.Passkey{ID: webPasskey, Name: "phone", Credential: []byte("{}"), CreatedAt: time.Now()}); err != nil {
			w.t.Fatal(err)
		}
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	h := sha256.Sum256(raw)
	now := time.Now()
	if err := w.d.st.PutWebSession(store.WebSession{Hash: h[:], CreatedAt: now, SeenAt: now, ReauthAt: reauthAt, PasskeyID: webPasskey}); err != nil {
		w.t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
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
	h.Set("Origin", webOrigin)
	h.Set("Cookie", web.SessionCookie+"="+cookie)
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(w.srv.URL, "https")+"/ws", &websocket.DialOptions{
		HTTPClient: w.srv.Client(), HTTPHeader: h, Host: "athena.test", Subprotocols: []string{link.Subprotocol},
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
	c := w.connect(w.login(time.Now()), phoneHello())
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
	cookie := w.login(time.Now())
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
	cookie := w.login(time.Now())
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
		{Id: "k1", Action: &athenav1.Command_RevokePasskey{RevokePasskey: &athenav1.RevokePasskey{Id: webPasskey}}},
		{Id: "u1", Action: &athenav1.Command_PushSubscribe{PushSubscribe: &athenav1.PushSubscribe{Endpoint: "https://push.test/x"}}},
		{Id: "u2", Action: &athenav1.Command_PushUnsubscribe{PushUnsubscribe: &athenav1.PushUnsubscribe{Endpoint: "https://push.test/x"}}},
		{Id: "u3", Action: &athenav1.Command_SetNotificationPrefs{SetNotificationPrefs: &athenav1.SetNotificationPrefs{}}},
	}
	stale := w.connect(w.login(time.Now().Add(-10*time.Minute)), phoneHello())
	stale.welcome()
	for _, cmd := range sensitive {
		if r := stale.do(cmd); r.GetCode() != web.CodeReauthRequired {
			t.Fatalf("%s without reauth = %v", cmd.GetId(), r)
		}
	}
	if r := stale.do(&athenav1.Command{Id: "l1", Action: &athenav1.Command_ListPasskeys{ListPasskeys: &athenav1.ListPasskeys{}}}); !r.GetOk() || len(r.GetPasskeys().GetPasskeys()) != 1 {
		t.Fatalf("list passkeys = %v", r)
	}
	if _, rev := w.d.policy.current(); rev != 0 {
		t.Fatalf("policy revision %d after refused sets", rev)
	}

	// The same command id goes through once the session is fresh: a reauth_required result is not kept.
	fresh := w.connect(w.login(time.Now()), phoneHello())
	fresh.welcome()
	if r := fresh.do(setPolicy("p1", 0)); !r.GetOk() || r.GetPolicyRevision() != 1 {
		t.Fatalf("set policy = %v", r)
	}
	if r := fresh.do(setPolicy("p2", 0)); r.GetCode() != web.CodeConflict {
		t.Fatalf("set policy on a stale base = %v", r)
	}
	if r := fresh.do(sensitive[2]); r.GetCode() != web.CodeUnavailable {
		t.Fatalf("push subscribe = %v", r)
	}
	fresh.next("policy delta", func(f *athenav1.Frame) bool { return f.GetDelta().GetPolicy().GetRevision() == 1 })
}

func TestInterruptReachesSession(t *testing.T) {
	fe := newFakeEnv(t, openTurnFixture(t))
	w := startWeb(t, newStateDir(t))
	id := w.submit("long task")
	w.running(id)
	c := w.connect(w.login(time.Now()), phoneHello())
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
	c := w.connect(w.login(time.Now()), phoneHello())
	c.welcome()
	var out WebRevokeResult
	w.ok(Request{Op: OpWebRevoke, ID: webPasskey}, &out)
	if out.Sessions != 1 {
		t.Fatalf("ended %d sessions", out.Sessions)
	}
	select {
	case err := <-c.done:
		if code := websocket.CloseStatus(err); code != web.CloseSessionEnded {
			t.Fatalf("close %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("connection open 1 s after the revoke")
	}
	if r, err := call(w.stateDir, Request{Op: OpWebRevoke, ID: webPasskey}); err != nil || r.OK || !strings.Contains(r.Error, "not found") {
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
