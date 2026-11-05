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

func (s *connSource) Tickets(int) ([]core.Ticket, error) {
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
func (*connSource) TicketAnsweredApprovals(string) ([]core.Approval, error) {
	return nil, nil
}
func (*connSource) Repos() ([]*athenav1.RepoView, error)       { return nil, nil }
func (*connSource) Notifications() ([]string, []string, error) { return nil, nil, nil }
func (*connSource) Prompts() (*athenav1.PromptsView, error)    { return &athenav1.PromptsView{}, nil }
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
	if code := c.closed(connWait); code != link.CloseProtocol {
		t.Fatalf("silent client: close %d, want %d", code, link.CloseProtocol)
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

func fastRecheck(d *Dashboard) { d.recheck = 20 * time.Millisecond }

func TestOpenConnectionClosesAtAbsoluteExpiry(t *testing.T) {
	e := newConnEnv(t, fastRecheck)
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	e.src.put(ticketAt("T-0001", t0))
	c := e.connect(hello("phone"))
	c.welcome()
	if r := c.do(watch("w1", "T-0001")); !r.GetOk() { // it only watches from here on: no Command re-checks the session
		t.Fatalf("watch: %v", r)
	}
	e.Advance(store.WebSessionAbsolute)
	if code := c.closed(connWait); code != CloseSessionEnded {
		t.Fatalf("close %d, want %d", code, CloseSessionEnded)
	}
}

func TestOpenConnectionKeepsSessionSeen(t *testing.T) {
	e := newConnEnv(t, fastRecheck)
	c := e.connect(hello("phone"))
	c.welcome()
	hash, _ := hashOf(e.cookie)
	e.Advance(store.WebSessionIdle - 24*time.Hour)
	deadline := time.Now().Add(connWait)
	for {
		ws, ok, err := e.Store.WebSession(hash)
		if err != nil || !ok {
			t.Fatalf("session gone: %v %v", ok, err)
		}
		if ws.SeenAt.Equal(e.Now()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("seen_at %v not refreshed to %v by the open connection", ws.SeenAt, e.Now())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Past the idle limit counted from the login: the connection's touch keeps the session.
	e.Advance(2 * 24 * time.Hour)
	time.Sleep(100 * time.Millisecond) // several rechecks
	select {
	case err := <-c.done:
		t.Fatalf("connection closed: %v", err)
	default:
	}
	if _, ok := e.Auth.Session(e.Request()); !ok {
		t.Fatal("session idled out while its connection was open")
	}
}

func TestSnapshotTrimmedToOneFrame(t *testing.T) {
	e := newConnEnv(t)
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	big := strings.Repeat("x", 12<<10)
	for i := range 100 { // the hub's bound of ended Tickets, 12 KiB each: over one frame
		tk := ticketAt(fmt.Sprintf("T-%04d", i+1), t0.Add(time.Duration(i)*time.Minute))
		tk.Title = big
		tk.Ended = &core.Ended{Outcome: core.OutcomeCompleted, At: tk.UpdatedAt}
		e.src.put(tk)
	}
	open := ticketAt("T-0999", t0)
	e.src.put(open)
	c := e.connect(hello("phone"))
	c.welcome()
	snap := c.snapshot()
	var ended []string
	sawOpen := false
	for _, tk := range snap.GetTickets() {
		if tk.GetEndedAt() == nil {
			sawOpen = tk.GetId() == "T-0999"
			continue
		}
		ended = append(ended, tk.GetId())
	}
	if !sawOpen || len(ended) == 0 || len(ended) >= 100 {
		t.Fatalf("open kept %v, %d ended kept: want the open one and fewer than 100 ended", sawOpen, len(ended))
	}
	if ended[0] != "T-0100" {
		t.Fatalf("newest ended kept first = %s, want T-0100 (the oldest go)", ended[0])
	}

	// A snapshot whose open Tickets alone are over a frame cannot be fitted.
	s := &athenav1.Snapshot{Tickets: []*athenav1.TicketSummary{{Id: "T-1", Title: strings.Repeat("x", link.MaxFrame)}}}
	if fitSnapshot(s) {
		t.Fatal("fitted a snapshot of open tickets over a frame")
	}
}

func TestOlderDetailNeverFollowsNewer(t *testing.T) {
	e := newConnEnv(t)
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	e.src.put(ticketAt("T-0001", t0.Add(2*time.Minute)))
	c := e.connect(hello("phone"))
	c.welcome()
	c.snapshot()
	if r := c.do(watch("w1", "T-0001")); !r.GetOk() {
		t.Fatalf("watch: %v", r)
	}
	isDetail := func(f *athenav1.Frame) bool { return f.GetDelta().GetTicketDetail() != nil }
	at := func(f *athenav1.Frame) time.Time {
		return f.GetDelta().GetTicketDetail().GetSummary().GetUpdatedAt().AsTime()
	}
	if f := c.next("first detail", isDetail); !at(f).Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("first detail at %v", at(f))
	}
	// A read older than what was sent (as a feed delta read before the watch's own read) is not sent.
	e.src.put(ticketAt("T-0001", t0.Add(time.Minute)))
	e.hub.OnCommit(store.Changes{Tickets: []string{"T-0001"}})
	e.src.put(ticketAt("T-0001", t0.Add(3*time.Minute)))
	e.hub.OnCommit(store.Changes{Tickets: []string{"T-0001"}})
	if f := c.next("next detail", isDetail); !at(f).Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("detail after the first is at %v, want the newer read only", at(f))
	}
}

// fakeStreams is a Streams with the node's semantics over lines the test pushes: a history per Ticket, from = -1 the
// last 200 lines, and a fresh live channel per Subscribe.
type fakeStreams struct {
	mu      sync.Mutex
	history map[string][]node.Line
	live    map[string]chan node.Line
	opened  int
	cancels int
	from    int64
}

func newFakeStreams() *fakeStreams {
	return &fakeStreams{history: map[string][]node.Line{}, live: map[string]chan node.Line{}}
}

func (f *fakeStreams) Subscribe(id string, from int64) ([]node.Line, <-chan node.Line, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.from, f.opened = from, f.opened+1
	var back []node.Line
	h := f.history[id]
	if from < 0 {
		back = append(back, h[max(0, len(h)-node.TailLines):]...)
	} else {
		for _, l := range h {
			if l.Offset >= from {
				back = append(back, l)
			}
		}
	}
	ch := make(chan node.Line, 512)
	f.live[id] = ch
	return back, ch, func() { f.mu.Lock(); f.cancels++; f.mu.Unlock() }, nil
}

// push appends a line to a Ticket's history and offers it to its current subscriber.
func (f *fakeStreams) push(id string, l node.Line) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history[id] = append(f.history[id], l)
	if ch := f.live[id]; ch != nil {
		select {
		case ch <- l:
		default:
		}
	}
}

func (f *fakeStreams) state() (opened, cancels int, from int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened, f.cancels, f.from
}

// say is a transcript line of an assistant text block, as the session writes it; n is its offset.
func say(n int, text string) node.Line {
	return node.Line{Offset: int64(n) * 100, Data: []byte(fmt.Sprintf(
		`{"type":"assistant","timestamp":"2026-11-04T09:00:00.000Z","message":{"content":[{"type":"text","text":%q}]}}`, text))}
}

func streamOpen(c *wsClient, ticket string, from int64) {
	c.t.Helper()
	if err := c.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_StreamOpen{
		StreamOpen: &athenav1.StreamOpen{TicketId: ticket, FromOffset: from}}}); err != nil {
		c.t.Fatal(err)
	}
}

// items collects n StreamItems' texts, skipping other frames.
func (c *wsClient) items(n int) (texts []string, offs []int64) {
	c.t.Helper()
	for len(texts) < n {
		f := c.next(fmt.Sprintf("stream items (%d of %d)", len(texts), n), func(f *athenav1.Frame) bool { return f.GetStreamItems() != nil })
		for _, it := range f.GetStreamItems().GetItems() {
			texts, offs = append(texts, it.GetText()), append(offs, it.GetOffset())
		}
	}
	return texts, offs
}

// quiet fails when a stream frame arrives within d; with count set it returns how many items did instead.
func (c *wsClient) quiet(d time.Duration) {
	c.t.Helper()
	if n := c.countItems(d); n != 0 {
		c.t.Fatalf("%d unexpected stream items", n)
	}
}

func (c *wsClient) countItems(d time.Duration) int {
	deadline := time.After(d)
	n := 0
	for {
		select {
		case f := <-c.frames:
			if f.GetStreamItems() != nil {
				n += len(f.GetStreamItems().GetItems())
			} else {
				c.backlog = append(c.backlog, f)
			}
		case <-deadline:
			return n
		}
	}
}

// streamConn connects a client to a Dashboard whose connections start with credit initial.
func streamConn(t *testing.T, initial uint64, tickets ...string) (*connEnv, *fakeStreams, *wsClient) {
	t.Helper()
	fs := newFakeStreams()
	e := newConnEnv(t, func(d *Dashboard) { d.streams, d.streamCredit = fs, initial })
	for _, id := range tickets {
		e.src.put(ticketAt(id, time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)))
	}
	c := e.connect(hello("phone"))
	c.welcome()
	c.snapshot()
	return e, fs, c
}

func TestStreamOverConnWithCredit(t *testing.T) {
	_, fs, c := streamConn(t, 10, "T-0001")
	streamOpen(c, "T-0001", -1)
	eventually(t, func() bool { o, _, _ := fs.state(); return o == 1 })
	for i := 1; i <= 50; i++ {
		fs.push("T-0001", say(i, fmt.Sprintf("line %d", i)))
	}
	texts, offs := c.items(10)
	for i, txt := range texts {
		if txt != fmt.Sprintf("line %d", i+1) || offs[i] != int64(i+1)*100 {
			t.Fatalf("item %d = %q at %d", i, txt, offs[i])
		}
	}
	c.quiet(300 * time.Millisecond) // out of credit: nothing more, however many lines come

	// A grant of 10 finds 40 lines it cannot all have: a gap over the first 30 and the latest 10.
	if err := c.sess.Grant(athenav1.Channel_STREAM, 10); err != nil {
		t.Fatal(err)
	}
	gap := c.next("gap", func(f *athenav1.Frame) bool { return f.GetStreamGap() != nil }).GetStreamGap()
	if gap.GetTicketId() != "T-0001" || gap.GetFrom() != 1100 || gap.GetTo() != 4100 {
		t.Fatalf("gap = %v, want 1100..4100", gap)
	}
	texts, _ = c.items(10)
	if texts[0] != "line 41" || texts[9] != "line 50" {
		t.Fatalf("tail = %v", texts)
	}
	c.quiet(200 * time.Millisecond)

	// With credit left, lines flow in order, one by one.
	if err := c.sess.Grant(athenav1.Channel_STREAM, 5); err != nil {
		t.Fatal(err)
	}
	fs.push("T-0001", say(51, "line 51"))
	if texts, _ := c.items(1); texts[0] != "line 51" {
		t.Fatalf("live = %v", texts)
	}
}

// A grant that comes while lines keep arriving still gets the gap and the latest tail, never the oldest lines.
func TestStreamGrantUnderHeavyOutputSkipsAhead(t *testing.T) {
	_, fs, c := streamConn(t, 0, "T-0001")
	streamOpen(c, "T-0001", -1)
	eventually(t, func() bool { o, _, _ := fs.state(); return o == 1 })
	for i := 1; i <= 100; i++ {
		fs.push("T-0001", say(i, fmt.Sprintf("line %d", i)))
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 101; ; i++ {
			select {
			case <-stop:
				return
			default:
				fs.push("T-0001", say(i, fmt.Sprintf("line %d", i)))
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	if err := c.sess.Grant(athenav1.Channel_STREAM, 20); err != nil {
		t.Fatal(err)
	}
	gap := c.next("gap", func(f *athenav1.Frame) bool { return f.GetStreamGap() != nil }).GetStreamGap()
	_, offs := c.items(1)
	close(stop)
	<-done
	if gap.GetFrom() != 100 || gap.GetTo() <= 100*100 || offs[0] != gap.GetTo() {
		t.Fatalf("gap %v then first item at %d: the oldest lines were sent", gap, offs[0])
	}
}

// Several streams share one credit: together they never send more than it covers.
func TestStreamsShareCreditWithoutOverspending(t *testing.T) {
	ids := []string{"T-0001", "T-0002", "T-0003", "T-0004"}
	_, fs, c := streamConn(t, 0, ids...)
	for _, id := range ids {
		for i := 1; i <= 100; i++ {
			fs.push(id, say(i, "x"))
		}
		streamOpen(c, id, 0)
	}
	eventually(t, func() bool { o, _, _ := fs.state(); return o == len(ids) })
	if n := c.countItems(200 * time.Millisecond); n != 0 {
		t.Fatalf("%d items without credit", n)
	}
	if err := c.sess.Grant(athenav1.Channel_STREAM, 30); err != nil {
		t.Fatal(err)
	}
	if n := c.countItems(500 * time.Millisecond); n > 30 {
		t.Fatalf("%d items sent on a credit of 30", n)
	}
}

func TestStreamResumesFromOffsetAndRedacts(t *testing.T) {
	_, fs, c := streamConn(t, 256, "T-0001")
	fs.push("T-0001", say(6, "before"))
	fs.push("T-0001", say(7, "token=ghp_"+strings.Repeat("a1B2", 10)))
	fs.push("T-0001", say(8, "plain"))
	streamOpen(c, "T-0001", 700)
	texts, offs := c.items(2)
	if _, _, from := fs.state(); from != 700 || offs[0] != 700 || offs[1] != 800 {
		t.Fatalf("from %d, offsets %v", from, offs)
	}
	if strings.Contains(texts[0], "ghp_") || texts[1] != "plain" {
		t.Fatalf("texts = %q", texts)
	}
}

// Bookkeeping lines (system lines other than init, rate limit events) render to no items: the stream sends none for
// them, and a client that resumes after its last item (from_offset = its offset + 1) gets the next real item, with
// no gap over the silent lines.
func TestStreamSkipsSilentLinesAndResumes(t *testing.T) {
	_, fs, c := streamConn(t, 256, "T-0001")
	noise := func(n int, line string) node.Line { return node.Line{Offset: int64(n) * 100, Data: []byte(line)} }
	fs.push("T-0001", say(1, "one"))
	fs.push("T-0001", noise(2, `{"type":"system","subtype":"task_progress","description":"Reading"}`))
	fs.push("T-0001", say(3, "three"))
	fs.push("T-0001", noise(4, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`))
	streamOpen(c, "T-0001", -1)
	texts, offs := c.items(2)
	if texts[0] != "one" || texts[1] != "three" || offs[1] != 300 {
		t.Fatalf("texts %q at %v", texts, offs)
	}
	c.quiet(200 * time.Millisecond)

	// Reconnected: resume after the last item. The silent line after it is read again and still sends nothing.
	streamOpen(c, "T-0001", offs[1]+1)
	eventually(t, func() bool { o, _, _ := fs.state(); return o == 2 })
	fs.push("T-0001", say(5, "five"))
	texts, offs = c.items(1)
	if texts[0] != "five" || offs[0] != 500 {
		t.Fatalf("after resume: %q at %v", texts, offs)
	}
	for _, f := range c.backlog {
		if f.GetStreamGap() != nil {
			t.Fatalf("a gap over silent lines: %v", f.GetStreamGap())
		}
	}
}

func TestStreamOpenErrorsAndClose(t *testing.T) {
	_, fs, c := streamConn(t, 100, "T-0001")
	streamOpen(c, "T-9999", -1)
	if f := c.next("error", func(f *athenav1.Frame) bool { return f.GetError() != nil }); f.GetError().GetCode() != CodeNotFound {
		t.Fatalf("unknown ticket: %v", f.GetError())
	}
	if f := c.next("close", func(f *athenav1.Frame) bool { return f.GetStreamClose() != nil }); f.GetStreamClose().GetTicketId() != "T-9999" {
		t.Fatalf("close: %v", f)
	}
	streamOpen(c, "", -1)
	if f := c.next("error", func(f *athenav1.Frame) bool { return f.GetError() != nil }); f.GetError().GetCode() != CodeInvalid {
		t.Fatalf("no ticket: %v", f.GetError())
	}

	streamOpen(c, "T-0001", -1)
	eventually(t, func() bool { o, _, _ := fs.state(); return o == 1 })
	fs.push("T-0001", say(1, "one"))
	c.items(1)
	if err := c.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_StreamClose{
		StreamClose: &athenav1.StreamClose{TicketId: "T-0001"}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, n, _ := fs.state(); return n == 1 })
	fs.push("T-0001", say(2, "two"))
	c.quiet(200 * time.Millisecond)
}

func TestStreamsStopWhenConnectionEnds(t *testing.T) {
	_, fs, c := streamConn(t, 256, "T-0001")
	streamOpen(c, "T-0001", -1)
	eventually(t, func() bool { o, _, _ := fs.state(); return o == 1 })
	c.close()
	eventually(t, func() bool { _, n, _ := fs.state(); return n == 1 })
}

func TestStreamWithoutStreamsIsUnavailable(t *testing.T) {
	e := newConnEnv(t)
	e.src.put(ticketAt("T-0001", time.Now()))
	c := e.connect(hello("phone"))
	c.welcome()
	c.snapshot()
	streamOpen(c, "T-0001", -1)
	if f := c.next("error", func(f *athenav1.Frame) bool { return f.GetError() != nil }); f.GetError().GetCode() != CodeUnavailable {
		t.Fatalf("got %v", f.GetError())
	}
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(connWait); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("condition not reached")
}

func TestPendingStaysBounded(t *testing.T) {
	p := &pending{skipFrom: -1}
	for i := 0; i < pendingItems+100; i++ {
		p.absorb(say(i, "x"))
	}
	if len(p.items) != pendingItems || p.skipFrom != 0 || p.items[len(p.items)-1].GetOffset() != int64(pendingItems+99)*100 {
		t.Fatalf("pending holds %d items, skipFrom %d", len(p.items), p.skipFrom)
	}
	// Big items: the bytes bound holds too, and the latest item stays.
	q := &pending{skipFrom: -1}
	for i := 0; i < 90; i++ {
		q.absorb(say(i, strings.Repeat("y", 14<<10)))
	}
	if q.bytes > pendingBytes || q.skipFrom != 0 || q.items[len(q.items)-1].GetOffset() != 8900 {
		t.Fatalf("pending holds %d items, %d bytes, skipFrom %d", len(q.items), q.bytes, q.skipFrom)
	}
	// A gap from the node drops what is held, which is older than the loss.
	p.absorb(node.Line{Offset: 900000, GapTo: 950000})
	if len(p.items) != 0 || p.skipFrom != 0 {
		t.Fatalf("after node gap: %d items, skipFrom %d", len(p.items), p.skipFrom)
	}
}

func TestVisibilityReachesPresence(t *testing.T) {
	p := NewPresence(nil)
	e := newConnEnv(t, func(d *Dashboard) { d.presence = p })
	c := e.connect(hello("phone"))
	c.welcome()
	c.snapshot()
	vis := func(id string, v bool) *athenav1.Command {
		return &athenav1.Command{Id: id, Action: &athenav1.Command_Visibility{Visibility: &athenav1.Visibility{Visible: v}}}
	}
	if r := c.do(vis("v1", true)); !r.GetOk() || !p.Visible() {
		t.Fatalf("visible: %v %v", r, p.Visible())
	}
	if r := c.do(vis("v2", false)); !r.GetOk() || p.Visible() {
		t.Fatalf("hidden: %v %v", r, p.Visible())
	}
	c.do(vis("v3", true))
	c.close()
	deadline := time.Now().Add(5 * time.Second)
	for p.Visible() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.Visible() {
		t.Error("a closed connection still counts as visible")
	}
}

func TestOversizedDeltaClosesConnection(t *testing.T) {
	e := newConnEnv(t)
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	c := e.connect(hello("phone"))
	c.welcome()
	c.snapshot()
	// A delta that cannot be sent (over a frame) must end the connection, not leave it open and silent.
	tk := ticketAt("T-0001", t0)
	tk.Title = strings.Repeat("x", link.MaxFrame)
	e.src.put(tk)
	e.hub.OnCommit(store.Changes{Tickets: []string{"T-0001"}})
	if code := c.closed(connWait); code != closeInternal {
		t.Fatalf("closed with %d, want %d", code, closeInternal)
	}
}

func TestSnapshotFailureSendsItsError(t *testing.T) {
	e := newConnEnv(t)
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	tk := ticketAt("T-0001", t0) // open, so it is never trimmed
	tk.Title = strings.Repeat("x", link.MaxFrame)
	e.src.put(tk)
	c := e.connect(hello("phone"))
	f := c.next("error", func(f *athenav1.Frame) bool { return f.GetError() != nil })
	if f.GetError().GetCode() != CodeInternal {
		t.Fatalf("error %v, want code %s", f.GetError(), CodeInternal)
	}
	if code := c.closed(connWait); code != closeInternal {
		t.Fatalf("closed with %d, want %d", code, closeInternal)
	}
}

func TestOversizedDetailKeepsNewestTimeline(t *testing.T) {
	e := newConnEnv(t)
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	tk := ticketAt("T-0001", t0)
	text := strings.Repeat("y ", 250) // one 500-rune timeline entry
	for i := range 4000 {             // about 2 MB of timeline
		tk.Inputs = append(tk.Inputs, core.Input{ID: fmt.Sprintf("i%04d", i), Kind: core.InputKind("note"), At: t0.Add(time.Duration(i) * time.Second), Text: text})
	}
	e.src.put(tk)
	c := e.connect(hello("phone"))
	c.welcome()
	c.snapshot()
	if r := c.do(watch("w1", "T-0001")); !r.GetOk() {
		t.Fatalf("watch: %v", r)
	}
	f := c.next("detail", func(f *athenav1.Frame) bool { return f.GetDelta().GetTicketDetail() != nil })
	tl := f.GetDelta().GetTicketDetail().GetTimeline()
	if len(tl) == 0 || len(tl) >= 4000 {
		t.Fatalf("%d timeline entries, want some but fewer than 4000", len(tl))
	}
	if last := tl[len(tl)-1]; last.GetRefId() != "i3999" {
		t.Fatalf("newest entry kept = %s, want i3999", last.GetRefId())
	}
}

func TestFitDetail(t *testing.T) {
	td := &athenav1.TicketDetail{Summary: &athenav1.TicketSummary{Id: "T-1", Title: strings.Repeat("x", link.MaxFrame)}}
	if _, ok := fitDetail(td); ok {
		t.Fatal("fitted a detail whose summary is over a frame")
	}
}

// A summary near a frame plus one entry must end (the single entry goes), and the shared input stays untouched.
func TestFitDetailTrimsACopyAndEnds(t *testing.T) {
	entry := &athenav1.TimelineEntry{Kind: "input", Text: strings.Repeat("e", 4096)}
	big := &athenav1.TicketDetail{Summary: &athenav1.TicketSummary{Id: "T-1", Title: strings.Repeat("x", maxSnapshot-1024)},
		Timeline: []*athenav1.TimelineEntry{entry}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, ok := fitDetail(big)
		if !ok || len(out.GetTimeline()) != 0 {
			t.Errorf("fit = %v with %d entries, want ok with none", ok, len(out.GetTimeline()))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fitDetail did not end with one oversized entry")
	}
	if len(big.Timeline) != 1 {
		t.Fatalf("input timeline changed to %d entries", len(big.Timeline))
	}

	many := &athenav1.TicketDetail{Summary: &athenav1.TicketSummary{Id: "T-2"}}
	for i := 0; i < 600; i++ {
		many.Timeline = append(many.Timeline, &athenav1.TimelineEntry{Kind: "input", Text: strings.Repeat("y", 4096)})
	}
	out, ok := fitDetail(many)
	if !ok || len(out.Timeline) == 0 || len(out.Timeline) >= 600 {
		t.Fatalf("fit = %v with %d entries", ok, len(out.GetTimeline()))
	}
	if out.Timeline[len(out.Timeline)-1] != many.Timeline[599] || len(many.Timeline) != 600 {
		t.Fatal("trim must keep the newest entries and leave the input as it was")
	}
}

func TestDetailTrimmedFlag(t *testing.T) {
	small := &athenav1.TicketDetail{Summary: &athenav1.TicketSummary{Id: "T-1"},
		Timeline: []*athenav1.TimelineEntry{{Kind: "input", Text: "hi"}}}
	if out, ok := fitDetail(small); !ok || out != small || out.TimelineTrimmed {
		t.Fatalf("a detail that fits: ok %v, same message %v, trimmed %v", ok, out == small, out.GetTimelineTrimmed())
	}
	many := &athenav1.TicketDetail{Summary: &athenav1.TicketSummary{Id: "T-2"}}
	for i := 0; i < 600; i++ {
		many.Timeline = append(many.Timeline, &athenav1.TimelineEntry{Kind: "input", Text: strings.Repeat("y", 4096)})
	}
	out, ok := fitDetail(many)
	if !ok || !out.TimelineTrimmed {
		t.Fatalf("a trimmed detail: ok %v, trimmed %v", ok, out.GetTimelineTrimmed())
	}
	if many.TimelineTrimmed {
		t.Fatal("the hub's shared detail must not be marked")
	}
}

func TestCommandOriginCarriesSessionHash(t *testing.T) {
	e := newConnEnv(t)
	c := e.connect(hello("phone"))
	c.welcome()
	c.do(listPasskeys("c1"))
	s, _ := e.Auth.Session(e.Request())
	if got := e.cmd.last().SessionHash; got == "" || got != s.Hash {
		t.Fatalf("origin session hash %q, want %q", got, s.Hash)
	}
}
