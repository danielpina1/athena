package nodelink

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// Protocol is NodeHello.protocol.
//
// Any change to an Args struct (proxies.go, serve.go) or to the fields of a call bumps Protocol. Both ends decode args
// strictly, and a fact the server cannot decode is refused as a UserError, which still advances the applied-fact mark:
// without the bump a node and a server of different versions would drop facts silently instead of refusing the link.
const Protocol = 2

const (
	// heartbeat and deadAfter are every node link's: a dropped node is noticed in under 10 s.
	heartbeat = 3 * time.Second
	deadAfter = 9 * time.Second
	// The reconnect backoff doubles from minBackoff to maxBackoff, with jitter.
	defaultMinBackoff = time.Second
	defaultMaxBackoff = 30 * time.Second
	// defaultRequestTimeout bounds one request.
	defaultRequestTimeout = 30 * time.Second
	// dialTimeout bounds a dial, and welcomeTimeout the wait for the Welcome after the Hello.
	dialTimeout    = 15 * time.Second
	welcomeTimeout = 15 * time.Second
	// retryPause is the pause before a Send refused with backpressure is tried again.
	retryPause = 20 * time.Millisecond
	// factBatch is how many facts are read from the outbox at a time.
	factBatch = 128
	// ticketCacheMax bounds the Ticket snapshots kept for offline answers.
	ticketCacheMax = 4096
)

// ErrOffline is a request made while the node has no link to the server.
var ErrOffline = errors.New("the control server is unreachable; wait a minute and try again")

// refusalsFatal is how many handshakes in a row the server must refuse the node's certificate before Run gives up.
const refusalsFatal = 3

// ErrRevoked ends Run: the server closed the link because the owner revoked this node, or keeps refusing its
// certificate at the handshake. Only a new join brings it back.
var ErrRevoked = errors.New("This node was revoked; run athena node join again.")

// ErrCertExpired ends Run: this node's own certificate is past its NotAfter, so every handshake would fail (which
// would otherwise read as revoked). Renewal is a new join.
var ErrCertExpired = errors.New("This node's certificate has expired; run athena node join again.")

// certExpired reports whether the node's certificate has expired at now; a node without one (tests) never expires.
func (id *Identity) certExpired(now time.Time) bool {
	leaf := id.Cert.Leaf
	if leaf == nil && len(id.Cert.Certificate) > 0 {
		var err error
		if leaf, err = x509.ParseCertificate(id.Cert.Certificate[0]); err != nil {
			return false // the handshake says what is wrong with it
		}
	}
	return leaf != nil && now.After(leaf.NotAfter)
}

// errLinkLost is a request whose link ended before its answer came; it is ErrOffline to the caller.
var errLinkLost = fmt.Errorf("%w", ErrOffline)

// Options configures a Client. The zero value of each field selects the default.
type Options struct {
	// StateDir is the node's state directory; the outbox is <StateDir>/node.db. Required.
	StateDir string
	// Version is the node's version, sent in its Hello and status.
	Version string
	// Caps returns the node's capabilities for each Hello; nil derives them from the attached node's status.
	Caps func() *athenav1.Capabilities
	// Dial opens the link; nil dials the identity's NodeURL with its certificate (link.Dial).
	Dial func(ctx context.Context) (link.Conn, error)
	// MinBackoff and MaxBackoff bound the pause between reconnects: 1 s doubling to 30 s, with jitter.
	MinBackoff, MaxBackoff time.Duration
	// RequestTimeout bounds one request: 30 s.
	RequestTimeout time.Duration
	// Log receives the link's log lines (default slog.Default()). No args, token or key is ever logged.
	Log *slog.Logger
	// HoldUntilServing holds the server's start and deliver calls until Serving is called (`athena node run` calls it
	// once the node's socket serves): a session launched before would find no socket for its hooks and MCP server.
	HoldUntilServing bool
}

// Client is a node's link to the control server: it dials and redials, sends the node's facts from its outbox once
// each and in order, makes its requests, and serves the server's calls onto the attached node.
//
// Wiring order: New, then the proxies (Engine, Credentials, Policy, WorkerPrompt, ReviewerPrompt, StatusChanged,
// Checkpointed) go into node.Config and node.New, then Attach(n), then Run. The Client is made before the Node because the Node's
// Config needs the proxies; the Node is attached after because serving needs the Node.
type Client struct {
	id   *Identity
	opts Options
	log  *slog.Logger
	ob   *outbox

	tgt      atomic.Pointer[targetRef]
	nextID   atomic.Uint64
	factWake chan struct{}
	statWake chan struct{}
	runCtx   atomic.Pointer[context.Context]

	ready     chan struct{}
	readyOnce sync.Once

	mu        sync.Mutex
	cur       *conn         // the link, nil while offline
	onlineSig chan struct{} // closed and replaced whenever cur changes
	tickets   map[string]cachedTicket
	ticketGen uint64
	identity  ResultIdentity
	prompt    ResultPrompts
	hasPrompt bool

	// answered are called with the server's answer to a fact ("" when applied), by fact seq (under ansMu).
	ansMu    sync.Mutex
	answered map[uint64]func(errMsg string)

	// The checkpoints waiting to be sent (serve.go): Ticket id -> repo.
	ckMu      sync.Mutex
	ckPending map[string]string
	ckRetries map[string]int // refusals in a row, by Ticket
	ckWake    chan struct{}
	// tmpDir holds the bundles being sent: <StateDir>/tmp, emptied when the Client is made.
	tmpDir string

	// The calls held until Serving (Options.HoldUntilServing), in arrival order (hold.go).
	holdMu  sync.Mutex
	serving bool
	held    []heldCall
}

type cachedTicket struct {
	t   core.Ticket
	gen uint64
}

type targetRef struct{ t target }

// conn is one link connection: a fresh link Session after a Hello and Welcome.
type conn struct {
	sess   *link.Session
	ctx    context.Context // ends with the connection
	cancel context.CancelFunc
	wel    chan *athenav1.NodeWelcome

	mu      sync.Mutex
	closed  bool
	pending map[uint64]chan *athenav1.NodeResult // requests waiting for their answer
	factIDs map[uint64]uint64                    // call id -> fact seq, of facts not answered yet
	subs    map[uint64]context.CancelFunc        // subscriptions, by the id of their subscribe call
	seeds   map[string]*seedTransfer             // seed and branch bundles on their way in, by transfer id (seed.go)

	// sendMu orders the frames of the link's calls: every fact committed before a request is queued before it.
	sendMu sync.Mutex
	queued uint64 // the highest fact seq queued on this link (under sendMu)

	answered atomic.Uint64 // the highest fact seq the server answered
	ansWake  chan struct{}
}

// New returns a Client for the joined node id, opening its outbox. It does not dial: Run does.
func New(id *Identity, opts Options) (*Client, error) {
	if opts.StateDir == "" {
		return nil, errors.New("nodelink: a state directory is required")
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = defaultMinBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = defaultMaxBackoff
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = opts.MinBackoff
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultRequestTimeout
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Dial == nil {
		opts.Dial = func(ctx context.Context) (link.Conn, error) { return link.Dial(ctx, id.NodeURL, id.TLSConfig()) }
	}
	ob, err := openOutbox(opts.StateDir)
	if err != nil {
		return nil, err
	}
	// Bundles a previous run left half-sent go.
	tmpDir := filepath.Join(opts.StateDir, "tmp")
	if err := os.RemoveAll(tmpDir); err != nil {
		ob.close()
		return nil, fmt.Errorf("nodelink: clear %s: %w", tmpDir, err)
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		ob.close()
		return nil, fmt.Errorf("nodelink: make %s: %w", tmpDir, err)
	}
	return &Client{id: id, opts: opts, log: opts.Log.With("component", "nodelink"), ob: ob,
		factWake: make(chan struct{}, 1), statWake: make(chan struct{}, 1), ready: make(chan struct{}),
		onlineSig: make(chan struct{}), tickets: map[string]cachedTicket{}, answered: map[uint64]func(string){},
		ckPending: map[string]string{}, ckRetries: map[string]int{}, ckWake: make(chan struct{}, 1), tmpDir: tmpDir,
		serving: !opts.HoldUntilServing}, nil
}

// Attach gives the Client the node the server's calls are served onto.
func (c *Client) Attach(n *node.Node) { c.attach(n) }

func (c *Client) attach(t target) { c.tgt.Store(&targetRef{t}) }

func (c *Client) target() target {
	if r := c.tgt.Load(); r != nil {
		return r.t
	}
	return nil
}

// Engine is the node's node.EngineCallbacks over the link.
func (c *Client) Engine() node.EngineCallbacks { return engineProxy{c} }

// Credentials is the node's node.Credentials over the link; it is a node.TokenRevoker too.
func (c *Client) Credentials() node.Credentials { return credsProxy{c} }

// Policy is the node's node.Policy over the link; it fails closed while offline.
func (c *Client) Policy() node.Policy { return policyProxy{c} }

// Online reports whether the link is up: welcomed, and the outbox's backlog queued.
func (c *Client) Online() bool { return c.current() != nil }

// Ready is closed once the server first gave the prompts (after a Welcome); `athena node run` serves only after it.
func (c *Client) Ready() <-chan struct{} { return c.ready }

// StatusChanged is node.Config.OnStatus: it only signals, so it neither blocks nor calls into the node.
func (c *Client) StatusChanged() { signal(c.statWake) }

// Close closes the outbox; call it after Run returned.
func (c *Client) Close() error { return c.ob.close() }

func (c *Client) current() *conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *Client) setCurrent(cn *conn, online bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case online:
		c.cur = cn
	case c.cur == cn:
		c.cur = nil
	default:
		return
	}
	close(c.onlineSig)
	c.onlineSig = make(chan struct{})
}

// waitOnline waits for a link, or for ctx to end (false).
func (c *Client) waitOnline(ctx context.Context) bool {
	for {
		c.mu.Lock()
		cur, sig := c.cur, c.onlineSig
		c.mu.Unlock()
		if cur != nil {
			return true
		}
		select {
		case <-sig:
		case <-ctx.Done():
			return false
		}
	}
}

// Run keeps the link up until ctx ends, redialing after 1 s doubling to 30 s, with jitter; a link that was welcomed
// starts the backoff over.
func (c *Client) Run(ctx context.Context) error {
	c.runCtx.Store(&ctx)
	backoff := c.opts.MinBackoff
	refused := 0
	for {
		// Checked before each dial: an expired certificate is refused at the handshake like a revoked one.
		if c.id.certExpired(time.Now()) {
			c.log.Error("this node's certificate has expired; run athena node join again")
			return ErrCertExpired
		}
		welcomed, err := c.connect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if revoked(err) {
			c.log.Error("the server revoked this node", "error", err)
			return ErrRevoked
		}
		// A refusal of the certificate at the handshake, again and again (not a passing failure to read the node's row).
		if refused = refused + 1; !certRefused(err) {
			refused = 0
		} else if refused >= refusalsFatal {
			c.log.Error("the server keeps refusing this node's certificate", "error", err)
			return ErrRevoked
		}
		if welcomed {
			backoff = c.opts.MinBackoff
			c.log.Warn("link to the server ended", "error", err)
		} else {
			c.log.Info("cannot reach the server", "error", err, "retry", backoff)
		}
		pause := backoff/2 + rand.N(backoff/2+1)
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		backoff = min(backoff*2, c.opts.MaxBackoff)
	}
}

// connect runs one link connection until it ends; welcomed reports whether the server welcomed it.
func (c *Client) connect(ctx context.Context) (welcomed bool, err error) {
	dctx, dcancel := context.WithTimeout(ctx, dialTimeout)
	lc, err := c.opts.Dial(dctx)
	dcancel()
	if err != nil {
		return false, err
	}
	cctx, cancel := context.WithCancel(ctx)
	cn := &conn{ctx: cctx, cancel: cancel, wel: make(chan *athenav1.NodeWelcome, 1),
		pending: map[uint64]chan *athenav1.NodeResult{}, factIDs: map[uint64]uint64{}, subs: map[uint64]context.CancelFunc{},
		ansWake: make(chan struct{}, 1)}
	cn.sess = link.New(lc, link.Options{Heartbeat: heartbeat, DeadAfter: deadAfter, OnFrame: func(f *athenav1.Frame) { c.onFrame(cn, f) }})
	errc := make(chan error, 1)
	go func() { errc <- cn.sess.Run(cctx) }()
	defer func() {
		cancel()
		c.setCurrent(cn, false)
		cn.end()
	}()

	hello := &athenav1.NodeHello{Protocol: Protocol, NodeId: c.id.NodeID, Version: c.opts.Version, Caps: c.caps()}
	if err := cn.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeHello{NodeHello: hello}}); err != nil {
		cancel()
		<-errc
		return false, err
	}
	var w *athenav1.NodeWelcome
	wt := time.NewTimer(welcomeTimeout)
	select {
	case w = <-cn.wel:
		wt.Stop()
	case err := <-errc:
		wt.Stop()
		return false, err
	case <-wt.C:
		cancel()
		<-errc
		return false, errors.New("the server sent no welcome")
	}

	// The server applied the facts through its mark: they go, and the rest are queued, in order, before anything else.
	moved, err := c.ob.applied(w.GetAppliedFactSeq())
	if err != nil {
		cancel()
		<-errc
		return true, fmt.Errorf("nodelink: outbox: %w", err)
	}
	if len(moved) > 0 {
		c.ansMu.Lock()
		then := map[uint64]func(string){}
		for from, to := range moved {
			if fn, ok := c.answered[from]; ok {
				then[to] = fn
				delete(c.answered, from)
			}
		}
		maps.Copy(c.answered, then)
		c.ansMu.Unlock()
		c.log.Warn("facts made before the first welcome numbered again after the server's mark", "facts", len(moved))
	}
	cn.sendMu.Lock()
	cn.queued = w.GetAppliedFactSeq()
	for {
		done, ferr := c.flushFacts(cn)
		if ferr != nil || done {
			err = ferr
			break
		}
		select {
		case <-time.After(retryPause):
		case <-cctx.Done():
			err = cctx.Err()
		}
		if err != nil {
			break
		}
	}
	cn.sendMu.Unlock()
	if err != nil {
		cancel()
		<-errc
		return true, err
	}
	c.setCurrent(cn, true)
	c.log.Info("linked to the server", "node", c.id.NodeID, "name", w.GetNodeName(), "applied", w.GetAppliedFactSeq())
	go c.pump(cn)
	go c.sendCheckpoints(cn)
	go c.reportStatus(cn)
	go c.awaitPrompts(cn)
	return true, <-errc
}

// awaitPrompts fetches the prompts until the server gives them, retrying with the link's backoff while the link
// lasts (the next link tries again), and closes Ready on the first success: a node serves only with the server's
// prompts, never the built-in ones.
func (c *Client) awaitPrompts(cn *conn) {
	wait := c.opts.MinBackoff
	for {
		if _, err := c.fetchPrompts(cn.ctx); err == nil {
			c.readyOnce.Do(func() { close(c.ready) })
			return
		} else {
			c.log.Warn("the server gave no prompts", "error", err, "retry", wait)
		}
		t := time.NewTimer(wait)
		select {
		case <-cn.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(wait*2, c.opts.MaxBackoff)
	}
}

// end fails the connection's waiting requests and stops its subscriptions.
func (cn *conn) end() {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	cn.closed = true
	for _, stop := range cn.subs {
		stop()
	}
	cn.subs = nil
	cn.dropSeeds()
}

// caps are the Hello's capabilities: Options.Caps, else what the attached node's status says.
func (c *Client) caps() *athenav1.Capabilities {
	if c.opts.Caps != nil {
		return c.opts.Caps()
	}
	caps := &athenav1.Capabilities{Os: runtime.GOOS, Arch: runtime.GOARCH}
	if t := c.target(); t != nil {
		s := t.Status()
		caps.Slots = uint32(max(s.Slots, 0))
		for _, r := range s.Runtimes {
			caps.Runtimes = append(caps.Runtimes, &athenav1.RuntimeCap{Name: r.Name, Version: r.Version, Ready: r.Ready, Why: r.Why})
		}
	}
	return caps
}

func (c *Client) onFrame(cn *conn, f *athenav1.Frame) {
	switch b := f.GetBody().(type) {
	case *athenav1.Frame_NodeWelcome:
		select {
		case cn.wel <- b.NodeWelcome:
		default:
		}
	case *athenav1.Frame_NodeResult:
		r := b.NodeResult
		cn.mu.Lock()
		if ch, ok := cn.pending[r.GetId()]; ok {
			delete(cn.pending, r.GetId())
			cn.mu.Unlock()
			ch <- r
			return
		}
		seq, ok := cn.factIDs[r.GetId()]
		delete(cn.factIDs, r.GetId())
		cn.mu.Unlock()
		if !ok {
			return
		}
		if r.GetError() != "" {
			// The server keeps its mark moving past a fact it refuses: it would refuse it again.
			c.log.Warn("the server refused a fact", "seq", seq, "error", r.GetError())
		}
		c.ansMu.Lock()
		then := c.answered[seq]
		delete(c.answered, seq)
		c.ansMu.Unlock()
		if then != nil {
			then(r.GetError()) // in seq order, on this goroutine: it only records
		}
		for {
			old := cn.answered.Load()
			if seq <= old || cn.answered.CompareAndSwap(old, seq) {
				break
			}
		}
		signal(cn.ansWake)
	case *athenav1.Frame_NodeCall:
		c.serve(cn, b.NodeCall)
	case *athenav1.Frame_BlobChunk:
		c.onBlobChunk(cn, b.BlobChunk)
	case *athenav1.Frame_Error:
		c.log.Warn("the server reported an error", "code", b.Error.GetCode(), "message", b.Error.GetMessage())
	}
}

// fact commits one fact to the outbox and wakes the sender. It never waits on the network.
func (c *Client) fact(method string, args any) error { return c.factThen(method, args, nil) }

// factThen is fact, with then called with the server's answer when it comes on a link of this Client ("" when the
// fact was applied). It must not block.
func (c *Client) factThen(method string, args any, then func(errMsg string)) error {
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("nodelink: %s: %w", method, err)
	}
	var seq uint64
	if then == nil {
		_, err = c.ob.append(method, b, time.Now())
	} else {
		// Under ansMu, so the answer cannot be looked for before then is in place.
		c.ansMu.Lock()
		if seq, err = c.ob.append(method, b, time.Now()); err == nil {
			c.answered[seq] = then
		}
		c.ansMu.Unlock()
	}
	if err != nil {
		c.log.Error("fact not recorded", "method", method, "error", err)
		return err
	}
	signal(c.factWake)
	return nil
}

// flushFacts queues the outbox's facts above cn.queued, in order; done is false when the link pushed back first. The
// caller holds cn.sendMu.
func (c *Client) flushFacts(cn *conn) (done bool, err error) {
	for {
		rows, err := c.ob.after(cn.queued, factBatch)
		if err != nil {
			return false, fmt.Errorf("nodelink: outbox: %w", err)
		}
		if len(rows) == 0 {
			return true, nil
		}
		// Flagged before they go, never after: a fact on the wire must not be numbered again (see applied).
		if err := c.ob.markSent(cn.queued, rows[len(rows)-1].seq); err != nil {
			return false, fmt.Errorf("nodelink: outbox: %w", err)
		}
		for _, r := range rows {
			id := c.nextID.Add(1)
			cn.mu.Lock()
			cn.factIDs[id] = r.seq
			cn.mu.Unlock()
			err := cn.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeCall{
				NodeCall: &athenav1.NodeCall{Id: id, Method: r.method, Args: r.args, FactSeq: r.seq}}})
			if err != nil {
				cn.mu.Lock()
				delete(cn.factIDs, id)
				cn.mu.Unlock()
				if errors.Is(err, link.ErrBackpressure) {
					return false, nil
				}
				return false, err
			}
			cn.queued = r.seq
		}
	}
}

// pump sends new facts as they are committed, and deletes the ones the server answered.
func (c *Client) pump(cn *conn) {
	var deleted uint64
	for {
		cn.sendMu.Lock()
		done, err := c.flushFacts(cn)
		cn.sendMu.Unlock()
		if err != nil {
			c.log.Error("send facts", "error", err)
		}
		if a := cn.answered.Load(); a > deleted {
			if err := c.ob.deleteThrough(a); err != nil {
				c.log.Error("outbox: delete applied facts", "error", err)
			} else {
				deleted = a
			}
		}
		var t *time.Timer
		var retry <-chan time.Time
		if !done || err != nil {
			t = time.NewTimer(retryPause)
			retry = t.C
		}
		select {
		case <-cn.ctx.Done():
		case <-c.factWake:
		case <-cn.ansWake:
		case <-retry:
		}
		if t != nil {
			t.Stop()
		}
		if cn.ctx.Err() != nil {
			return
		}
	}
}

// request makes one request and decodes its answer into out (nil: none expected).
func (c *Client) request(ctx context.Context, method string, args, out any) error {
	return c.requestTimeout(ctx, method, args, out, c.opts.RequestTimeout)
}

func (c *Client) requestTimeout(ctx context.Context, method string, args, out any, timeout time.Duration) error {
	cn := c.current()
	if cn == nil {
		return ErrOffline
	}
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("nodelink: %s: %w", method, err)
	}
	id := c.nextID.Add(1)
	ch := make(chan *athenav1.NodeResult, 1)
	cn.mu.Lock()
	if cn.closed {
		cn.mu.Unlock()
		return ErrOffline
	}
	cn.pending[id] = ch
	cn.mu.Unlock()
	defer func() {
		cn.mu.Lock()
		delete(cn.pending, id)
		cn.mu.Unlock()
	}()
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	frame := &athenav1.Frame{Body: &athenav1.Frame_NodeCall{NodeCall: &athenav1.NodeCall{Id: id, Method: method, Args: b}}}
	for {
		cn.sendMu.Lock()
		done, err := c.flushFacts(cn)
		if done && err == nil {
			err = cn.sess.Send(athenav1.Channel_CONTROL, true, frame)
			if err == nil {
				cn.sendMu.Unlock()
				break
			}
		}
		cn.sendMu.Unlock()
		if err != nil && !errors.Is(err, link.ErrBackpressure) {
			if errors.Is(err, link.ErrClosed) {
				return errLinkLost
			}
			return err
		}
		select {
		case <-time.After(retryPause):
		case <-cn.ctx.Done():
			return errLinkLost
		case <-tctx.Done():
			return c.timedOut(ctx, method, timeout)
		}
	}
	select {
	case r := <-ch:
		if err := resultError(r); err != nil {
			return err
		}
		if err := decodeValue(r.GetValue(), out); err != nil {
			return fmt.Errorf("nodelink: %s answer: %w", method, err)
		}
		return nil
	case <-cn.ctx.Done():
		return errLinkLost
	case <-tctx.Done():
		return c.timedOut(ctx, method, timeout)
	}
}

func (c *Client) timedOut(ctx context.Context, method string, timeout time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("the control server did not answer %s within %s", method, timeout)
}

// resultError is the error a NodeResult carries: a *core.UserError when the server marked it so.
func resultError(r *athenav1.NodeResult) error {
	if r.GetError() == "" {
		return nil
	}
	return DecodeError(r.GetError(), r.GetUserError())
}

// sentinels are the node's errors a caller matches with errors.Is, by message across the link.
var sentinels = []error{node.ErrNotFound, node.ErrNoWorkspace, node.ErrNoSession}

// DecodeError turns a NodeResult's error back into an error: one of the node's sentinels (node.ErrNotFound,
// node.ErrNoWorkspace, node.ErrNoSession) by its message, a *core.UserError when user is set, else a plain error.
func DecodeError(msg string, user bool) error {
	for _, s := range sentinels {
		if msg == s.Error() {
			return s
		}
	}
	if user {
		return &core.UserError{Msg: msg}
	}
	return errors.New(msg)
}

// EncodeError is the NodeResult error and user_error of err: user for a *core.UserError and the node's sentinels.
func EncodeError(err error) (msg string, user bool) {
	if err == nil {
		return "", false
	}
	var ue *core.UserError
	if errors.As(err, &ue) {
		return err.Error(), true
	}
	for _, s := range sentinels {
		if errors.Is(err, s) {
			return s.Error(), true
		}
	}
	return err.Error(), false
}

// cacheTicket keeps t as the latest snapshot of its Ticket, for Ticket while offline.
func (c *Client) cacheTicket(t core.Ticket) {
	if t.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ticketGen++
	c.tickets[t.ID] = cachedTicket{t: t, gen: c.ticketGen}
	if len(c.tickets) <= ticketCacheMax {
		return
	}
	oldest, gen := "", c.ticketGen
	for id, ct := range c.tickets {
		if ct.gen < gen {
			oldest, gen = id, ct.gen
		}
	}
	delete(c.tickets, oldest)
}

func (c *Client) cachedTicket(id string) (core.Ticket, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ct, ok := c.tickets[id]
	return ct.t, ok
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
