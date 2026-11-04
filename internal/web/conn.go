package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/hub"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// The dashboard's connection: one WebSocket per device, athena.v1 frames. The client opens with a Hello, the server
// answers with a Welcome and then either a Snapshot followed by the Deltas after it, or, when the client's cursor of
// this epoch is still in the hub's ring, the Deltas it missed. Commands come on CONTROL and each gets its
// CommandResult there; a Resync gets a fresh Snapshot.
//
// Every connection is a fresh link Session (frames numbered from 1 both ways). What survives a drop is the hub cursor,
// for state, and the Command id, for actions: a client resends every Command it has no result for, with the same id,
// and the Commander answers a repeat from the stored result without acting again.

// Protocol is the Hello.protocol this server speaks.
const Protocol = 1

// HelloTimeout is how long a new connection may take to say Hello.
const HelloTimeout = 10 * time.Second

// Stable error codes of CommandResult.code and Error.code.
const (
	CodeUnauthenticated = "unauthenticated"
	CodeReauthRequired  = "reauth_required"
	CodeInvalid         = "invalid"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeUnavailable     = "unavailable"
	CodeInternal        = "internal"
)

// Close codes of a dashboard connection, besides link's.
const (
	// CloseSessionEnded: the login the connection rode on was revoked, logged out or expired.
	CloseSessionEnded = 4002
	closeGoingAway    = 1001
	closeInternal     = 1011
	closeTryAgain     = 1013
)

const (
	heartbeat = 15 * time.Second
	// eventsQueue bounds the deltas waiting to be written: a whole ring replay fits. A client further behind than that
	// is dropped (1013) and comes back with its cursor.
	eventsQueue = 8192
	// commandQueue bounds the Commands waiting for the one that runs; one more is answered unavailable.
	commandQueue = 32
	maxWatched   = 64
	maxClientID  = 64
)

// Commander acts on a dashboard's Commands; internal/server's Dispatcher is one. Do never returns nil.
type Commander interface {
	Do(ctx context.Context, o Origin, c *athenav1.Command) *athenav1.CommandResult
}

// Origin is who sent a Command: the device's client id, and whether a passkey assertion vouched for its session in
// the last ReauthWindow (sensitive Commands need it).
type Origin struct {
	ClientID  string
	Sensitive bool
}

// DashboardConfig is what a Dashboard serves from.
type DashboardConfig struct {
	Auth      *Auth
	Hub       *hub.Hub
	Commander Commander
	// Streams serves the sessions' output; nil answers every stream_open unavailable.
	Streams Streams
	// Presence hears the clients' Visibility reports (push suppression); nil ignores them.
	Presence *Presence
	Log      *slog.Logger
	// Version is the server version the Welcome names.
	Version string
}

// Dashboard serves the dashboards' connections (ServeConn) and closes those whose session ends.
type Dashboard struct {
	auth    *Auth
	hub     *hub.Hub
	cmd     Commander
	streams Streams
	// presence is nil without push.
	presence *Presence
	// streamCredit is the STREAM credit a connection starts with, before any grant.
	streamCredit uint64
	log          *slog.Logger
	version      string
	// helloTimeout, recheck and linkOpts are fixed at creation; tests shorten them. recheck is how often an open
	// connection checks its session (a heartbeat).
	helloTimeout time.Duration
	recheck      time.Duration
	linkOpts     link.Options

	mu    sync.Mutex
	conns map[string]map[*conn]struct{} // by Session.Hash
	// closed refuses new connections once Close began; serving counts the ServeConn calls still running.
	closed  bool
	serving sync.WaitGroup
}

// NewDashboard returns a Dashboard; it registers with cfg.Auth to hear of ended sessions.
func NewDashboard(cfg DashboardConfig) *Dashboard {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	d := &Dashboard{auth: cfg.Auth, hub: cfg.Hub, cmd: cfg.Commander, streams: cfg.Streams, presence: cfg.Presence, log: log, version: cfg.Version,
		streamCredit: InitialStreamCredit, helloTimeout: HelloTimeout, recheck: heartbeat, conns: map[string]map[*conn]struct{}{}}
	d.auth.OnRevoke(d.revoked)
	return d
}

// revoked closes the connections of sessions that ended. It runs on the revoking goroutine: closing waits for the
// peer, so it happens on goroutines of its own.
func (d *Dashboard) revoked(hashes []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, h := range hashes {
		for c := range d.conns[h] {
			go c.end(CloseSessionEnded, "session ended")
		}
	}
}

// add registers an upgraded connection; false after Close began, when the caller ends it at once.
func (d *Dashboard) add(hash string, c *conn) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	if d.conns[hash] == nil {
		d.conns[hash] = map[*conn]struct{}{}
	}
	d.conns[hash][c] = struct{}{}
	return true
}

// enter counts a ServeConn call in; false after Close began.
func (d *Dashboard) enter() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	d.serving.Add(1)
	return true
}

// Connections is the number of open dashboard connections.
func (d *Dashboard) Connections() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, cs := range d.conns {
		n += len(cs)
	}
	return n
}

// Close ends every open connection with 1001 (going away: the client comes back with its cursor), refuses new ones
// and waits until every ServeConn returned. An upgraded connection is the HTTP server's no more, so its Shutdown
// does not do this.
func (d *Dashboard) Close() {
	d.mu.Lock()
	d.closed = true
	var open []*conn
	for _, cs := range d.conns {
		for c := range cs {
			open = append(open, c)
		}
	}
	d.mu.Unlock()
	for _, c := range open {
		go c.end(closeGoingAway, "server shutting down")
	}
	d.serving.Wait()
}

func (d *Dashboard) remove(hash string, c *conn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.conns[hash], c)
	if len(d.conns[hash]) == 0 {
		delete(d.conns, hash)
	}
}

// ServeConn upgrades a signed-in browser to the link and serves it until it closes. A request whose Origin is not the
// configured one is refused with 403, one without a live session with 401, both before the upgrade.
func (d *Dashboard) ServeConn(w http.ResponseWriter, r *http.Request) {
	if !d.enter() {
		fail(w, http.StatusServiceUnavailable, CodeUnavailable, "server shutting down")
		return
	}
	defer d.serving.Done()
	if !d.auth.checkOrigin(w, r) {
		return
	}
	s, ok := d.auth.Session(r)
	if !ok {
		fail(w, http.StatusUnauthorized, CodeUnauthenticated, "not signed in")
		return
	}
	lc, err := link.Accept(w, r)
	if err != nil {
		return // Accept answered
	}
	c := &conn{d: d, r: r, lc: lc, resync: make(chan struct{}, 1), cmds: make(chan *athenav1.Command, commandQueue),
		details: make(chan detailMsg), watching: map[string]bool{}, sentDetail: map[string]time.Time{},
		streams: map[string]*streamer{}, creditWake: make(chan struct{})}
	if !d.add(s.Hash, c) {
		c.end(closeGoingAway, "server shutting down")
		return
	}
	defer d.remove(s.Hash, c)
	if d.presence != nil {
		defer d.presence.Forget(c)
	}
	// A revoke between the check above and the registration would have found nothing to close.
	if _, ok := d.auth.Session(r); !ok {
		c.end(CloseSessionEnded, "session ended")
		return
	}
	c.serve(r.Context())
}

// conn is one dashboard connection.
type conn struct {
	d        *Dashboard
	r        *http.Request // the upgrade request: its cookie re-validates the session before each Command
	lc       link.Conn
	sess     *link.Session
	clientID string
	resync   chan struct{} // cap 1: the client asked for a Snapshot
	cmds     chan *athenav1.Command
	// details carries a watch's first detail (or an unwatch) to the forwarder, the one goroutine that sends details.
	details chan detailMsg
	done    <-chan struct{} // closed when the connection ends; set before the goroutines start
	// sentDetail is the updated_at of the last detail sent per Ticket (forwarder only): an older one is not sent.
	sentDetail map[string]time.Time

	endOnce sync.Once
	cancel  context.CancelFunc // set before the goroutines start

	mu       sync.Mutex
	sub      *hub.Sub
	watching map[string]bool

	// The session streams (stream.go). smu guards streams, itemsSent and creditWake; streamCtx and streamCancel are
	// set before the Session starts.
	smu          sync.Mutex
	streams      map[string]*streamer
	itemsSent    uint64        // STREAM items sent on this connection, all streams: never more than the client granted
	creditWake   chan struct{} // closed, and replaced, at each grant
	streamCtx    context.Context
	streamCancel context.CancelFunc
	streamWG     sync.WaitGroup
}

// end closes the connection with code, once; Run then returns.
func (c *conn) end(code int, reason string) {
	c.endOnce.Do(func() {
		c.mu.Lock()
		cancel := c.cancel
		c.mu.Unlock()
		_ = c.lc.Close(code, reason) // first: a cancelled Run would close with 1001
		if cancel != nil {
			cancel()
		}
	})
}

func (c *conn) serve(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hello, seq, ok := c.readHello(ctx)
	if !ok {
		return
	}
	c.clientID = hello.GetClientId()
	c.done = ctx.Done()
	sub, needSnapshot := c.d.hub.Subscribe(hello.GetCursor(), hello.GetEpoch())
	c.mu.Lock()
	c.sub, c.cancel = sub, cancel
	c.mu.Unlock()
	defer c.closeSub()

	opts := c.d.linkOpts
	opts.OnFrame = c.onFrame
	opts.OnCredit = c.onCredit
	c.streamCtx, c.streamCancel = context.WithCancel(ctx)
	defer c.streamCancel()
	// A Hello sent durable took seq 1: the client numbers on from it.
	opts.Resume = &link.Resume{Received: seq}
	opts.SendQueue = map[athenav1.Channel]int{athenav1.Channel_EVENTS: eventsQueue}
	c.sess = link.New(c.lc, opts)
	hb := opts.Heartbeat
	if hb <= 0 {
		hb = heartbeat
	}
	_ = c.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_Welcome{Welcome: &athenav1.Welcome{
		Epoch: c.d.hub.Epoch(), ServerVersion: c.d.version, HeartbeatMs: uint32(hb.Milliseconds()), SnapshotFollows: needSnapshot,
	}}})
	floor := hello.GetCursor()
	if needSnapshot {
		var err error
		if floor, err = c.sendSnapshot(); err != nil {
			c.snapshotFailed(err)
			return
		}
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		c.forward(ctx, floor)
	}()
	go func() {
		defer wg.Done()
		c.runCommands(ctx)
	}()
	go func() {
		defer wg.Done()
		c.checkSession(ctx)
	}()
	err := c.sess.Run(ctx)
	cancel()
	c.stopStreams()
	wg.Wait()
	c.d.log.Debug("dashboard connection closed", "client", c.clientID, "error", err)
}

// readHello reads the first frame, which must be a Hello of this protocol from a well-formed client id. It returns
// the Hello and its seq; a refusal is answered with an Error and the close.
func (c *conn) readHello(ctx context.Context) (*athenav1.Hello, uint64, bool) {
	// Not a read deadline: a cancelled read drops the socket without a close frame. The timer closes properly.
	late := time.AfterFunc(c.d.helloTimeout, func() { _ = c.lc.Close(link.CloseProtocol, "no hello in time") })
	b, err := c.lc.Read(ctx)
	if !late.Stop() {
		return nil, 0, false
	}
	if err != nil {
		_ = c.lc.Close(link.CloseProtocol, "no hello")
		return nil, 0, false
	}
	f, err := link.Decode(b)
	h := f.GetHello()
	switch {
	case err != nil || h == nil:
		c.refuse("the first frame must be a Hello")
	case h.GetProtocol() != Protocol:
		c.refuse(fmt.Sprintf("protocol %d is not supported (this server speaks %d)", h.GetProtocol(), Protocol))
	case !validClientID(h.GetClientId()):
		c.refuse("client_id must be 1 to 64 letters, digits, '.', '_' or '-'")
	case f.GetSeq() > 1:
		c.refuse("the Hello must be the first frame (seq 0 or 1)")
	default:
		return h, f.GetSeq(), true
	}
	return nil, 0, false
}

// checkSession re-validates the session every recheck while the connection is open; the open connection counts as
// the browser being seen. A session that ended (expired, or deleted behind Auth's back) closes the connection.
func (c *conn) checkSession(ctx context.Context) {
	t := time.NewTicker(c.d.recheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !c.d.auth.connected(c.r) {
				c.end(CloseSessionEnded, "session ended")
				return
			}
		}
	}
}

// validClientID keeps client ids to a plain alphabet without ':', so "<client_id>:<command_id>" names one command.
func validClientID(id string) bool {
	if id == "" || len(id) > maxClientID {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// refuse answers a bad Hello with an invalid Error, written before any Session exists, and closes.
func (c *conn) refuse(msg string) {
	b, err := link.Encode(&athenav1.Frame{Channel: athenav1.Channel_CONTROL,
		Body: &athenav1.Frame_Error{Error: &athenav1.Error{Code: CodeInvalid, Message: msg}}})
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = c.lc.Write(ctx, b)
		cancel()
	}
	_ = c.lc.Close(link.CloseProtocol, "bad hello")
}

// onFrame runs on the Session's reader: it only hands work on.
func (c *conn) onFrame(f *athenav1.Frame) {
	switch b := f.GetBody().(type) {
	case *athenav1.Frame_Command:
		select {
		case c.cmds <- b.Command:
		default:
			c.reply(&athenav1.CommandResult{Id: b.Command.GetId(), Code: CodeUnavailable, Message: "too many commands in flight"})
		}
	case *athenav1.Frame_Resync:
		select {
		case c.resync <- struct{}{}:
		default:
		}
	case *athenav1.Frame_StreamOpen:
		c.openStream(b.StreamOpen)
	case *athenav1.Frame_StreamClose:
		c.closeStream(b.StreamClose.GetTicketId())
	default:
		c.sendError(CodeInvalid, fmt.Sprintf("a client does not send %T", b))
	}
}

func (c *conn) sendError(code, msg string) {
	_ = c.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_Error{Error: &athenav1.Error{Code: code, Message: msg}}})
}

func (c *conn) reply(res *athenav1.CommandResult) {
	err := c.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_CommandResult{CommandResult: res}})
	if errors.Is(err, link.ErrBackpressure) {
		c.d.log.Warn("dashboard: command results not acked; closing", "client", c.clientID)
		go c.end(closeTryAgain, "too far behind")
	}
}

func (c *conn) currentSub() *hub.Sub {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sub
}

func (c *conn) closeSub() {
	if s := c.currentSub(); s != nil {
		s.Close()
	}
}

// sendSnapshot sends a fresh Snapshot, trimmed to fit one frame, and returns its cursor: the feed's deltas at or
// below it are in it already.
func (c *conn) sendSnapshot() (uint64, error) {
	snap, err := c.d.hub.Snapshot()
	if err != nil {
		c.d.log.Error("dashboard: snapshot", "client", c.clientID, "error", err)
		c.sendError(CodeInternal, "no snapshot")
		return 0, err
	}
	if before := len(snap.GetTickets()) + len(snap.GetApprovals()) + len(snap.GetDecisions()) + len(snap.GetTriggers()); !fitSnapshot(snap) {
		c.d.log.Error("dashboard: snapshot too big for a frame even trimmed", "client", c.clientID)
		c.sendError(CodeInternal, "no snapshot")
		return 0, errors.New("snapshot too big")
	} else if after := len(snap.GetTickets()) + len(snap.GetApprovals()) + len(snap.GetDecisions()) + len(snap.GetTriggers()); after < before {
		c.d.log.Warn("dashboard: snapshot trimmed to fit a frame", "client", c.clientID, "records", before, "kept", after)
	}
	if err := c.sess.Send(athenav1.Channel_EVENTS, false, &athenav1.Frame{Body: &athenav1.Frame_Snapshot{Snapshot: snap}}); err != nil {
		c.d.log.Error("dashboard: send snapshot", "client", c.clientID, "error", err)
		return 0, err
	}
	return snap.GetCursor(), nil
}

// snapshotFailed closes after a Snapshot could not be sent: with 1013 when the client is too far behind (it comes back
// with its cursor), else 1011.
func (c *conn) snapshotFailed(err error) {
	if errors.Is(err, link.ErrBackpressure) {
		go c.end(closeTryAgain, "too far behind")
		return
	}
	go c.end(closeInternal, "no snapshot")
}

// maxSnapshot is the largest Snapshot frame body: Send needs room for the stamp.
const maxSnapshot = link.MaxFrame - link.StampSlack

// fitSnapshot makes s fit one frame: while it is too big, it halves the ended Tickets, the answered Approvals, the
// Decisions and the Triggers, keeping the newest of each (the lists come newest first). Open Tickets, pending
// Approvals, Threads, node, health and Policy stay. It reports false when even that does not fit.
func fitSnapshot(s *athenav1.Snapshot) bool {
	size := func() int {
		return proto.Size(&athenav1.Frame{Channel: athenav1.Channel_EVENTS, Body: &athenav1.Frame_Snapshot{Snapshot: s}})
	}
	for size() > maxSnapshot {
		ended, answered := 0, 0
		for _, t := range s.Tickets {
			if t.GetEndedAt() != nil {
				ended++
			}
		}
		for _, a := range s.Approvals {
			if a.GetState() != core.ApprovalPending {
				answered++
			}
		}
		if ended+answered+len(s.Decisions)+len(s.Triggers) == 0 {
			return false
		}
		s.Tickets = keepFirst(s.Tickets, ended/2, func(t *athenav1.TicketSummary) bool { return t.GetEndedAt() != nil })
		s.Approvals = keepFirst(s.Approvals, answered/2, func(a *athenav1.ApprovalView) bool { return a.GetState() != core.ApprovalPending })
		s.Decisions = s.Decisions[:len(s.Decisions)/2]
		s.Triggers = s.Triggers[:len(s.Triggers)/2]
	}
	return true
}

// keepFirst keeps every element trim does not select, and the first n it does.
func keepFirst[T any](list []T, n int, trim func(T) bool) []T {
	out := list[:0]
	for _, v := range list {
		if trim(v) {
			if n == 0 {
				continue
			}
			n--
		}
		out = append(out, v)
	}
	return out
}

// forward sends the feed's deltas above floor (and every ticket_detail, seq 0), and a fresh Snapshot when the hub
// dropped the feed for falling behind or the client asked with Resync.
func (c *conn) forward(ctx context.Context, floor uint64) {
	sub := c.currentSub()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.resync:
			var ok bool
			if sub, floor, ok = c.resubscribe(); !ok {
				return
			}
		case m := <-c.details:
			if m.detail == nil {
				delete(c.sentDetail, m.ticketID)
			} else {
				c.sendDetail(m.detail)
			}
		case d, open := <-sub.C():
			if !open {
				if !sub.Resync() { // the hub stopped, or the connection is ending
					go c.end(1001, "going away")
					return
				}
				var ok bool
				if sub, floor, ok = c.resubscribe(); !ok {
					return
				}
				continue
			}
			if d.GetSeq() != 0 && d.GetSeq() <= floor {
				continue
			}
			if td := d.GetTicketDetail(); td != nil {
				c.sendDetail(td)
				continue
			}
			if err := c.sess.Send(athenav1.Channel_EVENTS, false, &athenav1.Frame{Body: &athenav1.Frame_Delta{Delta: d}}); err != nil {
				if errors.Is(err, link.ErrBackpressure) {
					c.d.log.Warn("dashboard: client too far behind; closing", "client", c.clientID)
					go c.end(closeTryAgain, "too far behind")
				}
				return
			}
		}
	}
}

// resubscribe replaces the feed with a new one (subscribed before the Snapshot is read, as the hub requires), keeps
// the watches, sends the Snapshot and the watched Tickets' details, and returns the feed and the Snapshot's cursor.
func (c *conn) resubscribe() (*hub.Sub, uint64, bool) {
	sub, _ := c.d.hub.Subscribe(0, "")
	c.mu.Lock()
	old := c.sub
	c.sub = sub
	ids := make([]string, 0, len(c.watching))
	for id := range c.watching {
		sub.Watch(id)
		ids = append(ids, id)
	}
	c.mu.Unlock()
	old.Close()
	floor, err := c.sendSnapshot()
	if err != nil {
		c.snapshotFailed(err)
		return nil, 0, false
	}
	for _, id := range ids {
		if td, err := c.d.hub.Detail(id); err == nil {
			c.sendDetail(td)
		}
	}
	return sub, floor, true
}

// detailMsg is a watch's first detail for the forwarder, or (detail nil) an unwatch.
type detailMsg struct {
	ticketID string
	detail   *athenav1.TicketDetail
}

// sendDetail sends a Ticket's detail unless one with a later updated_at went already: the first detail of a watch and
// the feed's are read at different times, and an older one must not overwrite a newer one. Forwarder only.
func (c *conn) sendDetail(td *athenav1.TicketDetail) {
	id, at := td.GetSummary().GetId(), td.GetSummary().GetUpdatedAt().AsTime()
	if last, ok := c.sentDetail[id]; ok && at.Before(last) {
		return
	}
	c.sentDetail[id] = at
	_ = c.sess.Send(athenav1.Channel_EVENTS, false, &athenav1.Frame{Body: &athenav1.Frame_Delta{Delta: &athenav1.Delta{
		Change: &athenav1.Delta_TicketDetail{TicketDetail: td}}}})
}

// runCommands runs the Commands one at a time, in the order they came. A Command that started finishes even when the
// connection ends meanwhile: its result is stored, and the client's resend gets it.
func (c *conn) runCommands(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-c.cmds:
			if res := c.handle(context.WithoutCancel(ctx), cmd); res != nil {
				c.reply(res)
			}
		}
	}
}

// handle re-validates the session against the store, answers the Commands that are the connection's own (watching a
// Ticket, visibility) and hands the rest to the Commander. A session that ended gets no result: the connection closes
// with CloseSessionEnded, and the Command is resent after the next login.
func (c *conn) handle(ctx context.Context, cmd *athenav1.Command) *athenav1.CommandResult {
	id := cmd.GetId()
	if id == "" {
		return &athenav1.CommandResult{Code: CodeInvalid, Message: "a command needs an id"}
	}
	s, ok := c.d.auth.Session(c.r)
	if !ok {
		go c.end(CloseSessionEnded, "session ended")
		return nil
	}
	var res *athenav1.CommandResult
	switch a := cmd.GetAction().(type) {
	case *athenav1.Command_WatchTicket:
		res = c.watch(a.WatchTicket.GetTicketId())
	case *athenav1.Command_UnwatchTicket:
		id := a.UnwatchTicket.GetTicketId()
		c.mu.Lock()
		delete(c.watching, id)
		c.sub.Unwatch(id)
		c.mu.Unlock()
		c.toForwarder(detailMsg{ticketID: id})
		res = &athenav1.CommandResult{Ok: true}
	case *athenav1.Command_Visibility:
		if c.d.presence != nil {
			c.d.presence.Report(c, a.Visibility.GetVisible())
		}
		res = &athenav1.CommandResult{Ok: true}
	default:
		res = c.d.cmd.Do(ctx, Origin{ClientID: c.clientID, Sensitive: c.d.auth.RecentReauth(s)}, cmd)
		if res == nil {
			res = &athenav1.CommandResult{Code: CodeInternal, Message: "internal error"}
		}
	}
	res.Id = id
	return res
}

// watch starts the ticket_detail deltas of a Ticket and has the forwarder send its current detail, read after the
// watch starts.
func (c *conn) watch(ticketID string) *athenav1.CommandResult {
	if ticketID == "" {
		return &athenav1.CommandResult{Code: CodeInvalid, Message: "watch needs a ticket id"}
	}
	c.mu.Lock()
	if !c.watching[ticketID] && len(c.watching) >= maxWatched {
		c.mu.Unlock()
		return &athenav1.CommandResult{Code: CodeInvalid, Message: fmt.Sprintf("at most %d tickets are watched at once", maxWatched)}
	}
	c.watching[ticketID] = true
	c.sub.Watch(ticketID)
	c.mu.Unlock()
	td, err := c.d.hub.Detail(ticketID)
	if err != nil {
		c.mu.Lock()
		delete(c.watching, ticketID)
		c.sub.Unwatch(ticketID)
		c.mu.Unlock()
		var ue *core.UserError
		if errors.As(err, &ue) {
			return &athenav1.CommandResult{Code: CodeNotFound, Message: ue.Msg}
		}
		c.d.log.Error("dashboard: ticket detail", "ticket", ticketID, "error", err)
		return &athenav1.CommandResult{Code: CodeInternal, Message: "internal error"}
	}
	c.toForwarder(detailMsg{ticketID: ticketID, detail: td})
	return &athenav1.CommandResult{Ok: true}
}

// toForwarder hands m to the forwarder, unless the connection ends first.
func (c *conn) toForwarder(m detailMsg) {
	select {
	case c.details <- m:
	case <-c.done:
	}
}
