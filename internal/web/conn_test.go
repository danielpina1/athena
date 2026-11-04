package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/hub"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
)

const connWait = 5 * time.Second

// connSource is a hub.Source holding only Tickets; everything else is empty.
type connSource struct {
	mu      sync.Mutex
	tickets map[string]core.Ticket
}

func (s *connSource) put(t core.Ticket) { s.mu.Lock(); s.tickets[t.ID] = t; s.mu.Unlock() }

func (s *connSource) Tickets(bool) ([]core.Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.Ticket
	for _, t := range s.tickets {
		out = append(out, t)
	}
	return out, nil
}

func (s *connSource) Ticket(id string) (core.Ticket, core.Stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[id]
	if !ok {
		return core.Ticket{}, core.Stage{}, &core.UserError{Msg: "ticket " + id + " not found"}
	}
	return t, core.TicketStage(&t), nil
}

func (*connSource) PendingApprovals() ([]core.Approval, error)     { return nil, nil }
func (*connSource) AnsweredApprovals(int) ([]core.Approval, error) { return nil, nil }
func (*connSource) Approval(string) (core.Approval, error) {
	return core.Approval{}, errors.New("none")
}
func (*connSource) RecentTriggers(int) ([]core.Trigger, error)     { return nil, nil }
func (*connSource) Trigger(string) (core.Trigger, error)           { return core.Trigger{}, errors.New("none") }
func (*connSource) ActiveThreads() ([]core.Thread, error)          { return nil, nil }
func (*connSource) RecentThreads(int) ([]core.Thread, error)       { return nil, nil }
func (*connSource) Thread(string) (core.Thread, error)             { return core.Thread{}, errors.New("none") }
func (*connSource) Decisions(string, int) ([]core.Decision, error) { return nil, nil }
func (*connSource) Decision(string) (core.Decision, error) {
	return core.Decision{}, errors.New("none")
}
func (*connSource) Health() []hub.HealthItem             { return nil }
func (*connSource) Policy() (int, string, string, error) { return 0, "v", "{}", nil }
func (*connSource) NodeStatus() node.Status              { return node.Status{} }

// recordingCommander answers ok and remembers the Origin of each Command.
type recordingCommander struct {
	mu      sync.Mutex
	origins []Origin
}

func (r *recordingCommander) Do(_ context.Context, o Origin, c *athenav1.Command) *athenav1.CommandResult {
	r.mu.Lock()
	r.origins = append(r.origins, o)
	r.mu.Unlock()
	return &athenav1.CommandResult{Ok: true}
}

func (r *recordingCommander) last() Origin {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.origins[len(r.origins)-1]
}

// connEnv is a signed-in owner, a hub over a connSource and a Dashboard behind a TLS test server.
type connEnv struct {
	*AuthEnv
	authn  *SoftAuthenticator
	cookie string
	src    *connSource
	hub    *hub.Hub
	cmd    *recordingCommander
	dash   *Dashboard
	srv    *httptest.Server
}

// newConnEnv builds the env; mods change the Dashboard before it serves.
func newConnEnv(t *testing.T, mods ...func(*Dashboard)) *connEnv {
	t.Helper()
	e := &connEnv{AuthEnv: NewAuthEnv(t), src: &connSource{tickets: map[string]core.Ticket{}}, cmd: &recordingCommander{}}
	e.authn = NewSoftAuthenticator(t, testOrigin, testRPID)
	e.Enroll(e.authn, "phone")
	e.cookie = e.Login(e.authn)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.hub = hub.New(e.src, log)
	ctx, cancel := context.WithCancel(context.Background())
	e.hub.Start(ctx)
	e.dash = NewDashboard(DashboardConfig{Auth: e.Auth, Hub: e.hub, Commander: e.cmd, Log: log, Version: "test"})
	for _, m := range mods {
		m(e.dash)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", e.dash.ServeConn)
	e.srv = httptest.NewTLSServer(mux)
	t.Cleanup(func() {
		cancel()
		e.srv.Close()
	})
	return e
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

// dial opens a connection with the given cookie value and Origin, without saying Hello.
func dial(t *testing.T, srv *httptest.Server, cookie, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	h.Set("Origin", origin)
	if cookie != "" {
		h.Set("Cookie", SessionCookie+"="+cookie)
	}
	ctx, cancel := context.WithTimeout(context.Background(), connWait)
	defer cancel()
	return websocket.Dial(ctx, "wss"+strings.TrimPrefix(srv.URL, "https")+"/ws", &websocket.DialOptions{
		HTTPClient: srv.Client(), HTTPHeader: h, Host: "athena.test", Subprotocols: []string{link.Subprotocol},
		CompressionMode: websocket.CompressionNoContextTakeover,
	})
}

// connect dials as the signed-in owner and says Hello.
func (e *connEnv) connect(hello *athenav1.Hello) *wsClient {
	e.T.Helper()
	c, resp, err := dial(e.T.(*testing.T), e.srv, e.cookie, testOrigin)
	if err != nil {
		e.T.Fatalf("dial: %v (%v)", err, resp)
	}
	return startClient(e.T.(*testing.T), c, hello)
}

func startClient(t *testing.T, c *websocket.Conn, hello *athenav1.Hello) *wsClient {
	t.Helper()
	cl := &wsClient{t: t, frames: make(chan *athenav1.Frame, 4096), done: make(chan error, 1)}
	cl.sess = link.New(link.NewWS(c), link.Options{OnFrame: func(f *athenav1.Frame) { cl.frames <- f }})
	if hello != nil {
		if err := cl.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_Hello{Hello: hello}}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cl.cancel = cancel
	go func() { cl.done <- cl.sess.Run(ctx) }()
	t.Cleanup(cl.close)
	return cl
}

// close ends the connection and waits for it; it is idempotent.
func (c *wsClient) close() {
	c.cancel()
	select {
	case err := <-c.done:
		c.done <- err
	case <-time.After(connWait):
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
	deadline := time.After(connWait)
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

func (c *wsClient) do(cmd *athenav1.Command) *athenav1.CommandResult {
	c.t.Helper()
	if err := c.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_Command{Command: cmd}}); err != nil {
		c.t.Fatal(err)
	}
	return c.next("result of "+cmd.GetId(), func(f *athenav1.Frame) bool {
		return f.GetCommandResult() != nil && f.GetCommandResult().GetId() == cmd.GetId()
	}).GetCommandResult()
}

// closed waits for the connection to end and returns its close code.
func (c *wsClient) closed(within time.Duration) websocket.StatusCode {
	c.t.Helper()
	select {
	case err := <-c.done:
		c.done <- err
		return websocket.CloseStatus(err)
	case <-time.After(within):
		c.t.Fatalf("connection still open after %s", within)
	}
	return 0
}

func hello(client string) *athenav1.Hello {
	return &athenav1.Hello{Protocol: Protocol, ClientId: client}
}

func watch(id, ticket string) *athenav1.Command {
	return &athenav1.Command{Id: id, Action: &athenav1.Command_WatchTicket{WatchTicket: &athenav1.WatchTicket{TicketId: ticket}}}
}

func listPasskeys(id string) *athenav1.Command {
	return &athenav1.Command{Id: id, Action: &athenav1.Command_ListPasskeys{ListPasskeys: &athenav1.ListPasskeys{}}}
}

func ticketAt(id string, at time.Time) core.Ticket {
	return core.Ticket{ID: id, Title: "title " + id, CreatedAt: at, UpdatedAt: at}
}

func TestConnRequiresSessionAndOrigin(t *testing.T) {
	e := newConnEnv(t)
	if _, resp, err := dial(t, e.srv, "", testOrigin); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no cookie: %v %v, want 401", err, resp)
	}
	if _, resp, err := dial(t, e.srv, b64.EncodeToString(make([]byte, 32)), testOrigin); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown cookie: %v %v, want 401", err, resp)
	}
	if _, resp, err := dial(t, e.srv, e.cookie, "https://evil.test"); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong origin: %v %v, want 403", err, resp)
	}
	c := e.connect(hello("phone"))
	w := c.welcome()
	if w.GetEpoch() != e.hub.Epoch() || !w.GetSnapshotFollows() || w.GetHeartbeatMs() != 15000 || w.GetServerVersion() != "test" {
		t.Fatalf("welcome = %v", w)
	}
	c.snapshot()
}

func TestHelloIsRequiredAndChecked(t *testing.T) {
	e := newConnEnv(t)
	for name, h := range map[string]*athenav1.Hello{
		"protocol 2":       {Protocol: 2, ClientId: "phone"},
		"no client id":     {Protocol: Protocol},
		"client id with :": {Protocol: Protocol, ClientId: "a:b"},
	} {
		c := e.connect(h)
		f := c.next(name+" error", func(f *athenav1.Frame) bool { return f.GetError() != nil })
		if f.GetError().GetCode() != CodeInvalid {
			t.Fatalf("%s: error %v, want invalid", name, f.GetError())
		}
		if code := c.closed(connWait); code != link.CloseProtocol {
			t.Fatalf("%s: close %d, want %d", name, code, link.CloseProtocol)
		}
	}
	// A first frame that is not a Hello.
	c := e.connect(nil)
	_ = c.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_Resync{Resync: &athenav1.Resync{}}})
	if f := c.next("error", func(f *athenav1.Frame) bool { return f.GetError() != nil }); f.GetError().GetCode() != CodeInvalid {
		t.Fatalf("no hello: %v", f.GetError())
	}
	c.closed(connWait)

	// Silence: the server gives up after its hello timeout.
	e = newConnEnv(t, func(d *Dashboard) { d.helloTimeout = 200 * time.Millisecond })
	c = e.connect(nil)
	if code := c.closed(connWait); code != link.CloseProtocol && code != -1 {
		t.Fatalf("silent client: close %d", code)
	}
}

func TestSensitiveCommandNeedsReauth(t *testing.T) {
	e := newConnEnv(t)
	c := e.connect(hello("phone"))
	c.welcome()
	if r := c.do(listPasskeys("c1")); !r.GetOk() || !e.cmd.last().Sensitive || e.cmd.last().ClientID != "phone" {
		t.Fatalf("right after login: %v %+v, want sensitive (login is a fresh assertion)", r, e.cmd.last())
	}
	e.Advance(ReauthWindow + time.Second)
	c.do(listPasskeys("c2"))
	if e.cmd.last().Sensitive {
		t.Fatal("sensitive after the reauth window passed")
	}
	// A reauthentication over HTTP while the connection stays open counts at the next Command.
	e.Reauth(e.authn)
	c.do(listPasskeys("c3"))
	if !e.cmd.last().Sensitive {
		t.Fatal("not sensitive after a reauthentication")
	}
}

func TestRevokedSessionClosesWithinOneSecond(t *testing.T) {
	e := newConnEnv(t)
	c := e.connect(hello("phone"))
	c.welcome()
	start := time.Now()
	if _, err := e.Auth.RevokePasskey(e.authn.CredentialID()); err != nil {
		t.Fatal(err)
	}
	if code := c.closed(time.Second); code != CloseSessionEnded {
		t.Fatalf("close code %d, want %d", code, CloseSessionEnded)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("closed after %s", d)
	}
	if _, resp, err := dial(t, e.srv, e.cookie, testOrigin); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reconnect after revoke: %v %v", err, resp)
	}
}

func TestCommandAfterSessionEndedClosesConnection(t *testing.T) {
	e := newConnEnv(t)
	c := e.connect(hello("phone"))
	c.welcome()
	// The session ends without telling the Dashboard (deleted from the store directly).
	hash, _ := hashOf(e.cookie)
	if err := e.Store.DeleteWebSession(hash); err != nil {
		t.Fatal(err)
	}
	if err := c.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_Command{Command: listPasskeys("c1")}}); err != nil {
		t.Fatal(err)
	}
	if code := c.closed(connWait); code != CloseSessionEnded {
		t.Fatalf("close %d, want %d", code, CloseSessionEnded)
	}
	e.cmd.mu.Lock()
	defer e.cmd.mu.Unlock()
	if len(e.cmd.origins) != 0 {
		t.Fatal("the command reached the Commander")
	}
}

func TestReconnectWithCursorGetsMissedDeltasOnly(t *testing.T) {
	e := newConnEnv(t)
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	e.src.put(ticketAt("T-0001", t0))
	e.hub.OnCommit(store.Changes{Tickets: []string{"T-0001"}})

	c := e.connect(hello("phone"))
	c.welcome()
	snap := c.snapshot()
	if len(snap.GetTickets()) != 1 {
		t.Fatalf("snapshot tickets = %v", snap.GetTickets())
	}
	e.src.put(ticketAt("T-0002", t0))
	e.hub.OnCommit(store.Changes{Tickets: []string{"T-0002"}})
	d := c.next("delta T-0002", func(f *athenav1.Frame) bool { return f.GetDelta().GetTicket().GetId() == "T-0002" }).GetDelta()
	if d.GetSeq() <= snap.GetCursor() {
		t.Fatalf("delta seq %d not above the snapshot cursor %d", d.GetSeq(), snap.GetCursor())
	}
	cursor := d.GetSeq()
	c.close()

	// Two commits while away.
	for _, id := range []string{"T-0003", "T-0004"} {
		e.src.put(ticketAt(id, t0))
		e.hub.OnCommit(store.Changes{Tickets: []string{id}})
	}
	c = e.connect(&athenav1.Hello{Protocol: Protocol, ClientId: "phone", Epoch: e.hub.Epoch(), Cursor: cursor})
	if w := c.welcome(); w.GetSnapshotFollows() {
		t.Fatal("snapshot follows a cursor still in the ring")
	}
	var got []string
	for len(got) < 2 {
		f := c.next("missed delta", func(f *athenav1.Frame) bool { return f.GetDelta() != nil || f.GetSnapshot() != nil })
		if f.GetSnapshot() != nil {
			t.Fatal("snapshot on resume")
		}
		got = append(got, fmt.Sprintf("%s@%d", f.GetDelta().GetTicket().GetId(), f.GetDelta().GetSeq()))
	}
	if !strings.HasPrefix(got[0], "T-0003@") || !strings.HasPrefix(got[1], "T-0004@") {
		t.Fatalf("missed deltas = %v, want T-0003 then T-0004 only", got)
	}

	// A cursor of another epoch gets a snapshot.
	c.close()
	c = e.connect(&athenav1.Hello{Protocol: Protocol, ClientId: "phone", Epoch: "another", Cursor: cursor})
	if !c.welcome().GetSnapshotFollows() {
		t.Fatal("no snapshot for another epoch")
	}
	c.snapshot()
}

func TestResyncWatchAndConnectionFrames(t *testing.T) {
	e := newConnEnv(t)
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	e.src.put(ticketAt("T-0001", t0))
	c := e.connect(hello("phone"))
	c.welcome()
	c.snapshot()

	if err := c.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_Resync{Resync: &athenav1.Resync{}}}); err != nil {
		t.Fatal(err)
	}
	c.snapshot()

	if r := c.do(watch("w1", "T-0001")); !r.GetOk() {
		t.Fatalf("watch: %v", r)
	}
	c.next("detail", func(f *athenav1.Frame) bool { return f.GetDelta().GetTicketDetail().GetSummary().GetId() == "T-0001" })
	if r := c.do(watch("w2", "T-0009")); r.GetCode() != CodeNotFound {
		t.Fatalf("watch unknown: %v", r)
	}
	// A watched Ticket's commit brings its summary and its detail; the watch survives a resync.
	if err := c.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_Resync{Resync: &athenav1.Resync{}}}); err != nil {
		t.Fatal(err)
	}
	c.snapshot()
	c.next("detail after resync", func(f *athenav1.Frame) bool { return f.GetDelta().GetTicketDetail() != nil })
	e.hub.OnCommit(store.Changes{Tickets: []string{"T-0001"}})
	c.next("summary delta", func(f *athenav1.Frame) bool { return f.GetDelta().GetTicket().GetId() == "T-0001" })
	c.next("detail delta", func(f *athenav1.Frame) bool { return f.GetDelta().GetTicketDetail() != nil })

	_ = c.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_StreamOpen{StreamOpen: &athenav1.StreamOpen{TicketId: "T-0001"}}})
	if f := c.next("stream error", func(f *athenav1.Frame) bool { return f.GetError() != nil }); f.GetError().GetCode() != CodeUnavailable {
		t.Fatalf("stream open: %v", f.GetError())
	}
	if r := c.do(&athenav1.Command{Action: &athenav1.Command_Visibility{Visibility: &athenav1.Visibility{}}}); r.GetCode() != CodeInvalid {
		t.Fatalf("command without id: %v", r)
	}
}
