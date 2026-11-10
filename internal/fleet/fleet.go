// Package fleet is the server's side of its nodes: the embedded local node and the remote ones that connect over the
// node link (internal/nodelink is their side). It keeps a Remote per enrolled node, applies each node's facts to the
// Engine once and in order, answers its requests, and on every connect reconciles what the node runs with what the
// Engine says it should.
package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
)

// Timing of the node links.
const (
	// OfflineAfter is how long a node may be away before its Tickets show the "node offline" person stage.
	OfflineAfter = 2 * time.Minute
	// RequestTimeout bounds a call to a node, and a node's request to the server (except WaitApproval, which the node
	// bounds itself).
	RequestTimeout = 30 * time.Second
	// Heartbeat and DeadAfter are every node link's: a dropped node is noticed in under 10 s.
	Heartbeat = 3 * time.Second
	DeadAfter = 9 * time.Second
	// helloTimeout bounds the wait for a node's Hello.
	helloTimeout = 15 * time.Second
	// retryPause is the pause before a Send refused with backpressure is tried again.
	retryPause = 20 * time.Millisecond
)

// replaceWait bounds the wait for a replaced link's fact applier; a variable for tests.
var replaceWait = 5 * time.Second

// Close codes and reasons of a node link the fleet ends.
const (
	// CloseRevoked closes the link of a node the owner revoked.
	CloseRevoked  = 4003
	reasonRevoked = "this node was revoked"
	reasonIDCert  = "node id does not match its certificate"
	// closeTryLater is a node that connects before the server is ready (no Engine yet).
	closeTryLater = 1013
)

// Node states in a NodeView.
const (
	StateOnline   = "online"
	StateOffline  = "offline"
	StateDraining = "draining"
	StateRevoked  = "revoked"
)

// ErrNodeOffline is in the error of a call to a node that is not connected; its message is "<name> is offline".
var ErrNodeOffline = core.ErrNodeOffline

// offline is the UserError of a call to the named node while it is offline.
func offline(name string) error {
	return &core.UserError{Msg: name + " is offline", Err: ErrNodeOffline}
}

// Target is what the server runs a Ticket on: the local *node.Node, or a *Remote over its link.
type Target interface {
	Start(t core.Ticket, a core.Assignment)
	Deliver(t core.Ticket, a core.Assignment)
	Stop(t core.Ticket, a core.Assignment, reason string)
	Interrupt(ticketID string) error
	Subscribe(ticketID string, from int64) ([]node.Line, <-chan node.Line, func(), error)
	TicketDiff(ctx context.Context, ticketID string) (node.Diff, error)
	TicketFiles(ctx context.Context, ticketID string) ([]node.FileEntry, bool, error)
	OpenTicketFile(ctx context.Context, ticketID, rel string) (*os.File, error)
	ApplyProposal(ctx context.Context, p core.Proposal) error
	SetPaused(paused bool)
	DropTokens(repo string)
	Status() node.Status
}

var (
	_ Target = (*node.Node)(nil)
	_ Target = (*Remote)(nil)
)

// Config is what a Fleet serves its nodes with.
type Config struct {
	// Local is the node embedded in the server, LocalID its id and LocalName its name ("this machine").
	Local     *node.Node
	LocalID   string
	LocalName string
	// Engine is the Engine the nodes' facts and requests go to; it may be nil here and set with SetEngine (the
	// Engine's Executor is the Fleet).
	Engine *core.Engine
	Store  *store.Store
	// Creds, Policy and Prompts answer the nodes' requests for them: the server's own.
	Creds   node.Credentials
	Policy  node.Policy
	Prompts func() (worker, reviewer string)
	// Mirror keeps the nodes' checkpoints (mirror.go); nil keeps none, and the nodes' CheckpointSent facts are refused.
	Mirror *Mirror
	// OfflineAfter is how long a remote node may be away before its Tickets show the node offline (0: OfflineAfter).
	OfflineAfter time.Duration
	// Now is the clock and AfterFunc its timers (nil: time.Now and time.AfterFunc); tests move both.
	Now       func() time.Time
	AfterFunc func(d time.Duration, fn func()) (stop func() bool)
	Log       *slog.Logger
}

// Fleet is the registry of the server's nodes.
type Fleet struct {
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
	epoch string
	eng   atomic.Pointer[core.Engine]

	ctx    context.Context // ends with Close; every node link's context derives from it
	cancel context.CancelFunc
	wg     sync.WaitGroup // ServeNode calls

	mu            sync.Mutex
	closed        bool
	remotes       map[string]*Remote
	subs          []func()
	localDraining bool

	// Placement: the pending Assignments waiting for a node, by Ticket id (pmu), and the goroutine that places them.
	pmu       sync.Mutex
	waiting   map[string]*waitEntry
	pseq      uint64
	placeWake chan struct{}
	placeDone chan struct{}

	// pubMu orders what every node must hear before it takes work (the kill switch, the repositories whose write
	// tokens were dropped) against a link's publication: a remote that connects gets the latest of each first.
	pubMu   sync.Mutex
	paused  bool
	dropped map[string]time.Time // repo -> when its write tokens were dropped

	// The embedded node's checkpoints waiting for the mirror (mirror.go): Ticket id -> repo.
	ckMu    sync.Mutex
	ckLocal map[string]string
	ckWake  chan struct{}
	// localBundle is the embedded node's CheckpointBundle.
	localBundle func(ctx context.Context, ticketID, since string, w io.Writer) (string, error)
	// localTooBig are the embedded node's Tickets whose bundle went over the bound (localCheckpoints' alone).
	localTooBig map[string]bool
	bg          sync.WaitGroup // the mirror's goroutines

	// The offline timers (offline.go), by node id.
	offMu     sync.Mutex
	offTimers map[string]*offTimer
	offGen    uint64
	// seeds are the launches that seed a worktree first (seed.go); Close waits for them.
	seeds sync.WaitGroup
}

// New returns a Fleet; it serves nodes once its Engine is set.
func New(cfg Config) *Fleet {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.OfflineAfter <= 0 {
		cfg.OfflineAfter = OfflineAfter
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, fn func()) func() bool { return time.AfterFunc(d, fn).Stop }
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	ctx, cancel := context.WithCancel(context.Background())
	f := &Fleet{cfg: cfg, log: cfg.Log.With("component", "fleet"), now: cfg.Now, epoch: hex.EncodeToString(b),
		ctx: ctx, cancel: cancel, remotes: map[string]*Remote{}, waiting: map[string]*waitEntry{}, dropped: map[string]time.Time{},
		placeWake: make(chan struct{}, 1), placeDone: make(chan struct{}), ckLocal: map[string]string{}, ckWake: make(chan struct{}, 1),
		localTooBig: map[string]bool{}, offTimers: map[string]*offTimer{}}
	if cfg.Engine != nil {
		f.eng.Store(cfg.Engine)
	}
	if cfg.Local != nil {
		f.localBundle = cfg.Local.CheckpointBundle
	}
	if cfg.Mirror != nil {
		f.bg.Add(2)
		go f.localCheckpoints()
		go f.sweepLoop()
	}
	// A node that connects, reports a status or is undrained may take what waits.
	f.subs = []func(){func() { signal(f.placeWake) }}
	go f.placeLoop()
	f.armAtStart()
	return f
}

// SetEngine sets the Engine the nodes report to and placement records into.
func (f *Fleet) SetEngine(e *core.Engine) {
	f.eng.Store(e)
	signal(f.placeWake)
}

func (f *Fleet) engine() *core.Engine { return f.eng.Load() }

// Close ends every node link and placement and waits for them, so the store can close after it. Nodes that connect
// later are refused.
func (f *Fleet) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.cancel()
	f.disarmAll()
	f.wg.Wait()
	<-f.placeDone
	f.seeds.Wait()
	f.bg.Wait()
}

// OnChange registers fn, called whenever a node connects, leaves, changes status, or is drained or revoked. fn must
// not block.
func (f *Fleet) OnChange(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, fn)
}

func (f *Fleet) changed() {
	f.mu.Lock()
	subs := append([]func(){}, f.subs...)
	f.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
}

// Target is the node id's Target: the local node for "" or LocalID (nil when there is none), else its Remote.
func (f *Fleet) Target(id string) Target {
	if id == "" || id == f.cfg.LocalID {
		if f.cfg.Local == nil {
			return nil
		}
		return f.cfg.Local
	}
	return f.Remote(id)
}

// Remote returns the node's Remote, making it on first use; it is offline until the node connects.
func (f *Fleet) Remote(id string) *Remote {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.remotes[id]
	if r == nil {
		r = &Remote{f: f, id: id, name: id}
		if n, err := f.cfg.Store.GetNode(id); err == nil && n != nil {
			r.name = n.Name
		}
		f.remotes[id] = r
	}
	return r
}

// NodeView is one node as the dashboard shows it.
type NodeView struct {
	ID, Name string
	// Local marks the node embedded in the server.
	Local bool
	// State is StateOnline, StateOffline, StateDraining or StateRevoked.
	State    string
	LastSeen time.Time
	// Caps are what the node said it has in its last Hello (nil before any).
	Caps    *athenav1.Capabilities
	Version string
	// Status is the node's last status (zero while it never reported one).
	Status node.Status
}

// Nodes lists the local node first, then every enrolled node, revoked ones included, by name.
func (f *Fleet) Nodes() []NodeView {
	var out []NodeView
	if f.cfg.Local != nil {
		f.mu.Lock()
		state := StateOnline
		if f.localDraining {
			state = StateDraining
		}
		f.mu.Unlock()
		out = append(out, NodeView{ID: f.cfg.LocalID, Name: f.cfg.LocalName, Local: true, State: state,
			LastSeen: f.now(), Status: f.cfg.Local.Status()})
	}
	rows, err := f.cfg.Store.Nodes()
	if err != nil {
		f.log.Error("list nodes", "error", err)
		return out
	}
	for _, n := range rows {
		v := NodeView{ID: n.ID, Name: n.Name, LastSeen: n.LastSeen}
		f.mu.Lock()
		r := f.remotes[n.ID]
		f.mu.Unlock()
		online := false
		if r != nil {
			r.mu.Lock()
			online = r.lk != nil
			v.Caps, v.Version, v.Status = r.caps, r.version, r.status
			if online || r.lastSeen.After(v.LastSeen) {
				v.LastSeen = r.lastSeen
			}
			r.mu.Unlock()
		}
		if v.Caps == nil && len(n.Caps) > 0 {
			c := &athenav1.Capabilities{}
			if proto.Unmarshal(n.Caps, c) == nil {
				v.Caps = c
			}
		}
		switch {
		case n.RevokedAt != nil:
			v.State = StateRevoked
		case !online:
			v.State = StateOffline
		case n.Draining:
			v.State = StateDraining
		default:
			v.State = StateOnline
		}
		out = append(out, v)
	}
	return out
}

// SetDraining marks the node as taking no new Assignments (or takes the mark off).
func (f *Fleet) SetDraining(id string, on bool) error {
	if id == f.cfg.LocalID && f.cfg.Local != nil {
		f.mu.Lock()
		f.localDraining = on
		f.mu.Unlock()
	} else if err := f.cfg.Store.SetNodeDraining(id, on); err != nil {
		return err
	}
	f.changed()
	return nil
}

// Draining reports whether the node takes no new Assignments.
func (f *Fleet) Draining(id string) bool {
	if id == f.cfg.LocalID && f.cfg.Local != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.localDraining
	}
	n, err := f.cfg.Store.GetNode(id)
	return err == nil && n != nil && n.Draining
}

// Revoke records that the node's certificate no longer counts and closes its live link with CloseRevoked.
func (f *Fleet) Revoke(id string) error {
	if id == f.cfg.LocalID {
		return &core.UserError{Msg: "the server's own node cannot be revoked"}
	}
	if err := f.cfg.Store.RevokeNode(id, f.now()); err != nil {
		return err
	}
	r := f.Remote(id)
	// Under connMu: a link being welcomed either saw the revocation or is attached before this looks.
	r.connMu.Lock()
	lk := r.link()
	r.connMu.Unlock()
	if lk != nil {
		lk.end(CloseRevoked, reasonRevoked)
	}
	f.log.Info("node revoked", "node", id)
	f.changed()
	return nil
}

// ServeNode serves one link of the node nodeID, which its verified certificate names (web.NodeLinkHandler): the
// Hello, the Welcome with the node's applied-fact mark, reconcile, then the node's calls until the link ends.
func (f *Fleet) ServeNode(nodeID string, c link.Conn) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = c.Close(1001, "the server is shutting down")
		return
	}
	f.wg.Add(1)
	f.mu.Unlock()
	defer f.wg.Done()
	if f.engine() == nil {
		_ = c.Close(closeTryLater, "the server is not ready yet")
		return
	}

	lk := newNodeLink(f, nodeID, c)
	runErr := make(chan error, 1)
	go func() {
		err := lk.sess.Run(lk.ctx)
		lk.cancel() // a link that ended by itself ends its goroutines too
		runErr <- err
	}()
	defer func() {
		err := <-runErr
		lk.cancel()
		lk.closeSubs()
		lk.dropTransfers() // the reader is done: no chunk comes after this
		lk.wg.Wait()
		close(lk.done)
		if r := lk.r; r != nil && r.detach(lk) {
			f.log.Info("node left", "node", nodeID, "error", err)
			f.armOffline(nodeID, f.now(), f.cfg.OfflineAfter)
			if err := f.cfg.Store.TouchNode(nodeID, f.now()); err != nil {
				f.log.Error("touch node", "node", nodeID, "error", err)
			}
			f.changed()
		}
	}()

	var h *athenav1.NodeHello
	t := time.NewTimer(helloTimeout)
	select {
	case h = <-lk.hello:
		t.Stop()
	case <-lk.ctx.Done():
		t.Stop()
		return
	case <-t.C:
		lk.end(link.CloseProtocol, "no hello")
		return
	}
	if h.GetProtocol() != nodelink.Protocol {
		lk.end(link.CloseProtocol, fmt.Sprintf("node protocol %d, server protocol %d; update the node", h.GetProtocol(), nodelink.Protocol))
		return
	}
	if h.GetNodeId() != nodeID {
		f.log.Warn("node hello names another node", "node", nodeID)
		lk.end(link.CloseProtocol, reasonIDCert)
		return
	}
	if !f.welcome(lk, h) {
		return
	}
	f.changed()
	lk.wg.Add(3)
	go lk.applyFacts()
	go lk.refreshStatus()
	go func() {
		defer lk.wg.Done()
		lk.reconcile()
	}()
	<-lk.ctx.Done()
}

// welcome replaces the node's previous link, if one is still up, sends the Welcome with the node's mark and attaches
// the link to its Remote; false ends the link.
func (f *Fleet) welcome(lk *nodeLink, h *athenav1.NodeHello) bool {
	id := lk.nodeID
	r := f.Remote(id)
	r.connMu.Lock()
	defer r.connMu.Unlock()
	if old := r.link(); old != nil {
		// The node came back before its old link was seen dead. That link's applier must stop before the mark is read;
		// it stops at its context's end, after at most the fact in hand, whatever state the old socket is in.
		old.end(1000, "replaced by a new link")
		select {
		case <-old.applierDone:
		case <-time.After(replaceWait):
			f.log.Warn("node's previous link still applies a fact; refusing the new one", "node", id)
			lk.end(closeTryLater, "the previous link is still ending; try again")
			return false
		}
	}
	n, err := f.cfg.Store.GetNode(id)
	switch {
	case err != nil:
		f.log.Error("read node", "node", id, "error", err)
		lk.end(1011, "internal error")
		return false
	case n == nil:
		lk.end(link.CloseProtocol, "unknown node")
		return false
	case n.RevokedAt != nil:
		lk.end(CloseRevoked, reasonRevoked)
		return false
	}
	at := f.now()
	if err := f.cfg.Store.TouchNode(id, at); err != nil {
		f.log.Error("touch node", "node", id, "error", err)
	}
	if b, err := proto.Marshal(h.GetCaps()); err == nil {
		if err := f.cfg.Store.SetNodeCaps(id, b); err != nil {
			f.log.Error("set node caps", "node", id, "error", err)
		}
	}
	lk.mark = n.AppliedFactSeq
	lk.r = r
	lk.welcomed.Store(true)
	w := &athenav1.NodeWelcome{Epoch: f.epoch, AppliedFactSeq: n.AppliedFactSeq,
		HeartbeatMs: uint32(Heartbeat.Milliseconds()), NodeName: n.Name}
	if err := lk.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeWelcome{NodeWelcome: w}}); err != nil {
		lk.end(1011, "internal error")
		return false
	}
	// Published only after the kill switch and the token drops are queued: nothing placement or reconcile starts on the
	// node reaches it before them.
	f.pubMu.Lock()
	f.sendState(lk)
	r.attach(lk, n.Name, h, at)
	f.pubMu.Unlock()
	// Under connMu, as the offline timer's check: the node's Tickets are not flagged offline after this.
	f.nodeBack(id)
	f.log.Info("node linked", "node", id, "name", n.Name, "applied", n.AppliedFactSeq, "version", h.GetVersion())
	return true
}
