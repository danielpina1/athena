package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

const (
	// applyTimeout bounds an ApplyProposal on a node, which bounds it at 15 minutes itself.
	applyTimeout = 16 * time.Minute
	// subBuffer is how many transcript lines a remote subscription holds for its reader before it drops to a gap.
	subBuffer = 1024
	// reconcileStopReason is the reason of a Stop reconcile sends.
	reconcileStopReason = "the Assignment ended or moved while its node was away"
)

// Remote is a remote node as a Target. While its node is connected its methods are calls over the link; while not,
// Start, Deliver, Stop and SetPaused are dropped (reconcile on the next connect redoes the first three; Task 8's
// placement sends the kill switch again) and the calls that need an answer fail with ErrNodeOffline.
type Remote struct {
	f  *Fleet
	id string

	// connMu orders a link's welcome against another link of the node and against Revoke.
	connMu sync.Mutex

	mu       sync.Mutex
	name     string
	lk       *nodeLink // nil while offline
	caps     *athenav1.Capabilities
	version  string
	status   node.Status
	lastSeen time.Time
}

// ID is the node's id.
func (r *Remote) ID() string { return r.id }

// Online reports whether the node is connected.
func (r *Remote) Online() bool { return r.link() != nil }

func (r *Remote) link() *nodeLink {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lk
}

func (r *Remote) nodeName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.name
}

func (r *Remote) attach(lk *nodeLink, name string, h *athenav1.NodeHello, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lk, r.name, r.caps, r.version, r.lastSeen = lk, name, h.GetCaps(), h.GetVersion(), at
}

// detach takes lk off the Remote, if it is still its link.
func (r *Remote) detach(lk *nodeLink) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lk != lk {
		return false
	}
	r.lk = nil
	r.lastSeen = r.f.now()
	return true
}

// setStatus keeps the node's latest status; true when it changed.
func (r *Remote) setStatus(s node.Status) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if reflect.DeepEqual(r.status, s) {
		return false
	}
	r.status = s
	return true
}

// Status is the node's last reported status.
func (r *Remote) Status() node.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Start sends the node the Assignment to launch, with the Ticket's snapshot.
func (r *Remote) Start(t core.Ticket, a core.Assignment) {
	r.command(nodelink.ServeStart, nodelink.ArgsStart{Ticket: t, Assignment: a})
}

// Deliver sends the node the Ticket's waiting Inputs to deliver.
func (r *Remote) Deliver(t core.Ticket, a core.Assignment) {
	r.command(nodelink.ServeDeliver, nodelink.ArgsDeliver{Ticket: t, Assignment: a})
}

// Stop tells the node to end the Assignment's session.
func (r *Remote) Stop(t core.Ticket, a core.Assignment, reason string) {
	r.command(nodelink.ServeStop, nodelink.ArgsStop{Ticket: t, Assignment: a, Reason: reason})
}

// SetPaused sends the owner's kill switch.
func (r *Remote) SetPaused(paused bool) {
	r.command(nodelink.ServeSetPaused, nodelink.ArgsSetPaused{Paused: paused})
}

// DropTokens has the node drop its write tokens of a repository the owner held.
func (r *Remote) DropTokens(repo string) {
	r.command(nodelink.ServeDropTokens, nodelink.ArgsDropTokens{Repo: repo})
}

// command queues a call whose answer nobody waits for, in order with the others; offline it is dropped. It never
// blocks: the Engine calls Start, Deliver and Stop under its lock.
func (r *Remote) command(method string, args any) {
	lk := r.link()
	if lk == nil {
		r.f.log.Debug("node offline: call dropped", "node", r.id, "method", method)
		return
	}
	b, err := json.Marshal(args)
	if err != nil {
		r.f.log.Error("encode a node call", "node", r.id, "method", method, "error", err)
		return
	}
	lk.queueCommand(method, b)
}

// command queues a call on this link whose answer nobody waits for.
func (lk *nodeLink) command(method string, args any) {
	b, err := json.Marshal(args)
	if err != nil {
		lk.f.log.Error("encode a node call", "node", lk.nodeID, "method", method, "error", err)
		return
	}
	lk.queueCommand(method, b)
}

// Interrupt asks the node to abandon the Ticket's current turn.
func (r *Remote) Interrupt(ticketID string) error {
	_, err := r.call(context.Background(), RequestTimeout, nodelink.ServeInterrupt, nodelink.ArgsInterrupt{TicketID: ticketID})
	return err
}

// ApplyProposal has the node push a Proposal's branch and open its pull request.
func (r *Remote) ApplyProposal(ctx context.Context, p core.Proposal) error {
	_, err := r.call(ctx, applyTimeout, nodelink.ServeApplyProposal, nodelink.ArgsApplyProposal{Proposal: p})
	return err
}

// TicketDiff is the Ticket's diff on the node; its patch may be cut to fit the link (Truncated).
func (r *Remote) TicketDiff(ctx context.Context, ticketID string) (node.Diff, error) {
	var d node.Diff
	v, err := r.call(ctx, RequestTimeout, nodelink.ServeDiff, nodelink.ArgsDiff{TicketID: ticketID})
	if err == nil {
		err = json.Unmarshal(v, &d)
	}
	return d, err
}

// TicketFiles lists the Ticket's files on the node.
func (r *Remote) TicketFiles(ctx context.Context, ticketID string) ([]node.FileEntry, bool, error) {
	var res nodelink.ResultFiles
	v, err := r.call(ctx, RequestTimeout, nodelink.ServeFiles, nodelink.ArgsFiles{TicketID: ticketID})
	if err == nil {
		err = json.Unmarshal(v, &res)
	}
	return res.Files, res.Truncated, err
}

// OpenTicketFile fetches one of the Ticket's files from the node (at most 768 KiB) into an unlinked temporary file.
func (r *Remote) OpenTicketFile(ctx context.Context, ticketID, rel string) (*os.File, error) {
	v, err := r.call(ctx, RequestTimeout, nodelink.ServeOpenFile, nodelink.ArgsOpenFile{TicketID: ticketID, Path: rel})
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "athena-node-file-*")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(f.Name()) // the open file stays readable; nothing is left behind
	if _, err := f.Write(v); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Subscribe starts a feed of the Ticket's transcript lines from the node. The node sends its backfill as live lines,
// so backfill is empty and the channel carries them all, in order; a reader that falls behind gets a gap Line. The
// channel closes when the node ends the feed, the link ends, or cancel is called.
func (r *Remote) Subscribe(ticketID string, from int64) ([]node.Line, <-chan node.Line, func(), error) {
	lk := r.link()
	if lk == nil {
		return nil, nil, nil, offline(r.nodeName())
	}
	id := lk.nextID.Add(1)
	s := &remoteSub{ch: make(chan node.Line, subBuffer+1), gapFrom: -1}
	lk.mu.Lock()
	if lk.ended {
		lk.mu.Unlock()
		return nil, nil, nil, offline(r.nodeName())
	}
	lk.subs[id] = s // before the call: lines may follow its answer at once
	lk.mu.Unlock()
	cancel := func() {
		lk.mu.Lock()
		_, live := lk.subs[id]
		delete(lk.subs, id)
		lk.mu.Unlock()
		s.end()
		if live && lk.ctx.Err() == nil {
			if b, err := json.Marshal(nodelink.ArgsUnsubscribe{Sub: id}); err == nil {
				lk.queueCommand(nodelink.ServeUnsubscribe, b)
			}
		}
	}
	if _, err := r.callOn(context.Background(), lk, id, RequestTimeout, nodelink.ServeSubscribe,
		nodelink.ArgsSubscribe{TicketID: ticketID, From: from}); err != nil {
		lk.mu.Lock()
		delete(lk.subs, id)
		lk.mu.Unlock()
		s.end()
		return nil, nil, nil, err
	}
	return nil, s.ch, sync.OnceFunc(cancel), nil
}

// call makes one call to the node and returns its value.
func (r *Remote) call(ctx context.Context, timeout time.Duration, method string, args any) ([]byte, error) {
	lk := r.link()
	if lk == nil {
		return nil, offline(r.nodeName())
	}
	return r.callOn(ctx, lk, lk.nextID.Add(1), timeout, method, args)
}

func (r *Remote) callOn(ctx context.Context, lk *nodeLink, id uint64, timeout time.Duration, method string, args any) ([]byte, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("fleet: %s: %w", method, err)
	}
	ch := make(chan *athenav1.NodeResult, 1)
	lk.mu.Lock()
	if lk.ended {
		lk.mu.Unlock()
		return nil, offline(r.nodeName())
	}
	lk.pending[id] = func(res *athenav1.NodeResult) { ch <- res }
	lk.mu.Unlock()
	defer func() {
		lk.mu.Lock()
		delete(lk.pending, id)
		lk.mu.Unlock()
	}()
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	frame := &athenav1.Frame{Body: &athenav1.Frame_NodeCall{NodeCall: &athenav1.NodeCall{Id: id, Method: method, Args: b}}}
	if err := lk.send(tctx, frame); err != nil {
		if lk.ctx.Err() != nil {
			return nil, offline(r.nodeName())
		}
		return nil, timedOut(ctx, r.nodeName(), method, timeout)
	}
	select {
	case res := <-ch:
		if res.GetError() != "" {
			return nil, nodelink.DecodeError(res.GetError(), res.GetUserError())
		}
		return res.GetValue(), nil
	case <-lk.ctx.Done():
		return nil, offline(r.nodeName())
	case <-tctx.Done():
		return nil, timedOut(ctx, r.nodeName(), method, timeout)
	}
}

func timedOut(ctx context.Context, name, method string, timeout time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%s did not answer %s within %s", name, method, timeout)
}

// remoteSub is one transcript feed from a node.
type remoteSub struct {
	mu      sync.Mutex
	ch      chan node.Line
	done    bool
	gapFrom int64 // the offset lines were dropped from, -1 when none were
	gapTo   int64 // the end of the last line dropped
}

// push hands lines to the reader without blocking; what does not fit becomes a gap before the next line that does,
// or before the end. The channel keeps one slot free for that last gap, so the end never loses it.
func (s *remoteSub) push(lines []node.Line, closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	room := func(n int) bool { return len(s.ch)+n <= cap(s.ch)-1 }
	for _, l := range lines {
		if s.gapFrom >= 0 {
			if !room(2) {
				s.dropped(l)
				continue
			}
			s.ch <- node.Line{Offset: s.gapFrom, GapTo: l.Offset}
			s.gapFrom = -1
		}
		if !room(1) {
			s.dropped(l)
			continue
		}
		s.ch <- l
	}
	if closed {
		if s.gapFrom >= 0 {
			s.ch <- node.Line{Offset: s.gapFrom, GapTo: s.gapTo} // the reserved slot
			s.gapFrom = -1
		}
		s.done = true
		close(s.ch)
	}
}

// dropped notes a line the reader had no room for.
func (s *remoteSub) dropped(l node.Line) {
	if s.gapFrom < 0 {
		s.gapFrom = l.Offset
	}
	s.gapTo = l.Offset + int64(len(l.Data)) + 1
	if l.GapTo != 0 {
		s.gapTo = l.GapTo
	}
}

func (s *remoteSub) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		s.done = true
		close(s.ch)
	}
}

// nodeLink is one connection of a node: a fresh link Session from Hello to its end.
type nodeLink struct {
	f      *Fleet
	nodeID string
	r      *Remote // set once welcomed
	sess   *link.Session
	conn   *closeConn
	ctx    context.Context
	cancel context.CancelFunc
	hello  chan *athenav1.NodeHello
	done   chan struct{} // closed once every goroutine of the link has returned
	// applierDone is closed when the fact applier returned: no fact of this link is applied after it.
	applierDone chan struct{}
	wg          sync.WaitGroup
	nextID      atomic.Uint64
	mark        uint64 // the applied-fact mark; the applier's alone once welcomed
	// welcomed is set, with r, before the Welcome is sent: the node's calls may come right after it.
	welcomed atomic.Bool

	mu       sync.Mutex
	ended    bool
	pending  map[uint64]func(*athenav1.NodeResult) // calls to the node waiting for their answer
	subs     map[uint64]*remoteSub
	facts    []*athenav1.NodeCall // received, not yet applied
	received uint64               // facts received on this link
	applied  uint64               // facts the applier is through with
	advance  chan struct{}        // closed and replaced whenever applied grows
	cmds     []*athenav1.Frame    // commands not yet sent
	// transfers are the checkpoint bundles the node sends on BLOB, by transfer id (mirror.go).
	transfers map[string]*transfer
	// cmdSender is set once the goroutine sending cmds runs.
	cmdSender bool

	factWake   chan struct{}
	cmdWake    chan struct{}
	statusWake chan struct{}
}

func newNodeLink(f *Fleet, nodeID string, c link.Conn) *nodeLink {
	ctx, cancel := context.WithCancel(f.ctx)
	lk := &nodeLink{f: f, nodeID: nodeID, conn: &closeConn{Conn: c}, ctx: ctx, cancel: cancel,
		hello: make(chan *athenav1.NodeHello, 1), done: make(chan struct{}), applierDone: make(chan struct{}),
		pending: map[uint64]func(*athenav1.NodeResult){}, subs: map[uint64]*remoteSub{}, advance: make(chan struct{}), transfers: map[string]*transfer{},
		factWake: make(chan struct{}, 1), cmdWake: make(chan struct{}, 1), statusWake: make(chan struct{}, 1)}
	lk.sess = link.New(lk.conn, link.Options{Heartbeat: Heartbeat, DeadAfter: DeadAfter, OnFrame: lk.onFrame})
	return lk
}

// end closes the link with code and reason.
func (lk *nodeLink) end(code int, reason string) {
	lk.conn.closeWith(code, reason)
	lk.cancel()
}

// closeSubs ends the link's subscriptions and refuses new calls.
func (lk *nodeLink) closeSubs() {
	lk.mu.Lock()
	lk.ended = true
	subs := lk.subs
	lk.subs = map[uint64]*remoteSub{}
	lk.mu.Unlock()
	for _, s := range subs {
		s.end()
	}
}

// onFrame runs on the link's reader goroutine: it never blocks.
func (lk *nodeLink) onFrame(fr *athenav1.Frame) {
	switch b := fr.GetBody().(type) {
	case *athenav1.Frame_NodeHello:
		select {
		case lk.hello <- b.NodeHello:
		default:
		}
	case *athenav1.Frame_NodeCall:
		lk.onCall(b.NodeCall)
	case *athenav1.Frame_NodeResult:
		lk.mu.Lock()
		fn := lk.pending[b.NodeResult.GetId()]
		delete(lk.pending, b.NodeResult.GetId())
		lk.mu.Unlock()
		if fn != nil {
			fn(b.NodeResult)
		}
	case *athenav1.Frame_NodeStatusReport:
		signal(lk.statusWake)
	case *athenav1.Frame_BlobChunk:
		lk.onChunk(b.BlobChunk)
	case *athenav1.Frame_Error:
		lk.f.log.Warn("node reported an error", "node", lk.nodeID, "code", b.Error.GetCode(), "message", b.Error.GetMessage())
	}
}

func (lk *nodeLink) onCall(call *athenav1.NodeCall) {
	if !lk.welcomed.Load() {
		// Nothing but the Hello comes before the Welcome; a node that sends calls first breaks the protocol.
		lk.end(link.CloseProtocol, "a call before the welcome")
		return
	}
	switch {
	case call.GetMethod() == nodelink.MethodLines:
		var a nodelink.ArgsLines
		if err := decodeStrict(call.GetArgs(), &a); err != nil {
			return
		}
		lk.mu.Lock()
		s := lk.subs[a.Sub]
		if a.Closed {
			delete(lk.subs, a.Sub)
		}
		lk.mu.Unlock()
		if s != nil {
			s.push(a.Lines, a.Closed)
		}
	case call.GetFactSeq() > 0:
		lk.mu.Lock()
		lk.facts = append(lk.facts, call)
		lk.received++
		lk.mu.Unlock()
		signal(lk.factWake)
	default:
		lk.mu.Lock()
		need := lk.received
		lk.mu.Unlock()
		lk.wg.Add(1)
		go func() {
			defer lk.wg.Done()
			if !lk.waitApplied(need) {
				return
			}
			v, err := lk.f.request(lk.ctx, lk.nodeID, call)
			lk.reply(call.GetId(), v, err)
		}()
	}
}

// waitApplied waits until the applier is through with the first n facts of the link; false when the link ended first.
func (lk *nodeLink) waitApplied(n uint64) bool {
	for {
		lk.mu.Lock()
		done, ch := lk.applied >= n, lk.advance
		lk.mu.Unlock()
		if done {
			return true
		}
		select {
		case <-ch:
		case <-lk.ctx.Done():
			return false
		}
	}
}

// applyFacts applies the link's facts one at a time, in the order they came (fact_seq order), and answers each once
// it is applied or skipped. The node deletes its outbox through the highest seq answered, so no fact is answered
// before the ones ahead of it.
func (lk *nodeLink) applyFacts() {
	defer lk.wg.Done()
	defer close(lk.applierDone)
	for {
		if lk.ctx.Err() != nil {
			return // an ended link applies nothing more, even what it already received
		}
		lk.mu.Lock()
		var call *athenav1.NodeCall
		if len(lk.facts) > 0 {
			call = lk.facts[0]
			lk.facts[0] = nil
			lk.facts = lk.facts[1:]
		}
		lk.mu.Unlock()
		if call == nil {
			select {
			case <-lk.ctx.Done():
				return
			case <-lk.factWake:
			}
			continue
		}
		seq := call.GetFactSeq()
		var err error
		if seq > lk.mark {
			var ok bool
			if err, ok = lk.applyOne(call); !ok {
				return // the link ended before the fact could be recorded; the next link replays it
			}
			lk.mark = seq
		}
		lk.reply(call.GetId(), nil, err)
		lk.mu.Lock()
		lk.applied++
		close(lk.advance)
		lk.advance = make(chan struct{})
		lk.mu.Unlock()
	}
}

// applyOne applies a fact and moves the mark past it. An error the Engine would give again (a UserError, args that do
// not decode) is the fact's answer and still moves the mark; any other is retried while the link lasts. ok is false
// when the link ended first.
func (lk *nodeLink) applyOne(call *athenav1.NodeCall) (answer error, ok bool) {
	seq := call.GetFactSeq()
	if call.GetMethod() == nodelink.MethodCheckpointSent {
		// Its bundle came before it on this link: it must be in the mirror, or dropped, before the fact is applied.
		var a nodelink.ArgsCheckpointSent
		if decodeStrict(call.GetArgs(), &a) == nil && !lk.awaitTransfers(a.TicketID) {
			return nil, false
		}
	}
	pause := 100 * time.Millisecond
	for {
		err := lk.f.fact(lk.nodeID, call)
		var ue *core.UserError
		if err == nil || errors.As(err, &ue) {
			if err != nil {
				lk.f.log.Warn("node fact refused", "node", lk.nodeID, "seq", seq, "method", call.GetMethod(), "error", err)
			}
			answer = err
			break
		}
		lk.f.log.Error("apply node fact", "node", lk.nodeID, "seq", seq, "method", call.GetMethod(), "error", err, "retry", pause)
		if !lk.sleep(pause) {
			return nil, false
		}
		pause = min(pause*2, 5*time.Second)
	}
	// The fact is applied; its mark must be recorded before it is answered, or a replay would apply it twice.
	pause = 100 * time.Millisecond
	for {
		_, err := lk.f.cfg.Store.AdvanceFactSeq(lk.nodeID, seq)
		if err == nil {
			return answer, true
		}
		lk.f.log.Error("record node fact mark", "node", lk.nodeID, "seq", seq, "error", err, "retry", pause)
		if !lk.sleep(pause) {
			return nil, false
		}
		pause = min(pause*2, 5*time.Second)
	}
}

func (lk *nodeLink) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-lk.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// reply answers a node's call; one the link pushes back on is tried again until the link ends.
func (lk *nodeLink) reply(id uint64, value []byte, err error) {
	msg, user := nodelink.EncodeError(err)
	if msg != "" {
		value = nil
	}
	_ = lk.send(lk.ctx, &athenav1.Frame{Body: &athenav1.Frame_NodeResult{NodeResult: &athenav1.NodeResult{
		Id: id, Value: value, Error: msg, UserError: user}}})
}

// send queues a durable frame on CONTROL, waiting out backpressure until ctx or the link ends.
func (lk *nodeLink) send(ctx context.Context, fr *athenav1.Frame) error {
	for {
		err := lk.sess.Send(athenav1.Channel_CONTROL, true, fr)
		if !errors.Is(err, link.ErrBackpressure) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-lk.ctx.Done():
			return lk.ctx.Err()
		case <-time.After(retryPause):
		}
	}
}

// queueCommand queues a call nobody waits for; one sender sends them in order, and logs the node's refusals.
func (lk *nodeLink) queueCommand(method string, args []byte) {
	id := lk.nextID.Add(1)
	fr := &athenav1.Frame{Body: &athenav1.Frame_NodeCall{NodeCall: &athenav1.NodeCall{Id: id, Method: method, Args: args}}}
	lk.mu.Lock()
	if lk.ended || lk.ctx.Err() != nil {
		lk.mu.Unlock()
		return
	}
	lk.pending[id] = func(res *athenav1.NodeResult) {
		if res.GetError() != "" {
			lk.f.log.Warn("node refused a call", "node", lk.nodeID, "method", method, "error", res.GetError())
		}
	}
	lk.cmds = append(lk.cmds, fr)
	if !lk.cmdSender {
		// Under mu, before ended is set: the link's end waits for this sender.
		lk.cmdSender = true
		lk.wg.Add(1)
		go lk.sendCommands()
	}
	lk.mu.Unlock()
	signal(lk.cmdWake)
}

func (lk *nodeLink) sendCommands() {
	defer lk.wg.Done()
	for {
		lk.mu.Lock()
		cmds := lk.cmds
		lk.cmds = nil
		lk.mu.Unlock()
		for _, fr := range cmds {
			if err := lk.send(lk.ctx, fr); err != nil {
				return
			}
		}
		select {
		case <-lk.ctx.Done():
			return
		case <-lk.cmdWake:
		}
	}
}

// refreshStatus asks the node for its exact status when the link comes up and after each status report it pushes.
func (lk *nodeLink) refreshStatus() {
	defer lk.wg.Done()
	signal(lk.statusWake)
	for {
		select {
		case <-lk.ctx.Done():
			return
		case <-lk.statusWake:
		}
		lk.fetchStatus()
	}
}

// fetchStatus asks the node for its status and keeps it.
func (lk *nodeLink) fetchStatus() (node.Status, bool) {
	var s node.Status
	v, err := lk.r.callOn(lk.ctx, lk, lk.nextID.Add(1), RequestTimeout, nodelink.ServeStatus, nodelink.ArgsStatus{})
	if err == nil {
		err = json.Unmarshal(v, &s)
	}
	if err != nil {
		if lk.ctx.Err() == nil {
			lk.f.log.Warn("node status", "node", lk.nodeID, "error", err)
		}
		return s, false
	}
	if lk.r.setStatus(s) {
		lk.f.changed()
	}
	return s, true
}

// reconcile brings the node in line with the Engine: it starts the Assignments placed on it that are pending or
// running (the snapshot refreshes the node's Ticket cache), delivers waiting Inputs, and stops the sessions of
// Tickets whose latest Assignment ended or moved elsewhere.
func (lk *nodeLink) reconcile() {
	eng := lk.f.engine()
	r := lk.r
	tickets, err := eng.Tickets(false)
	if err != nil {
		lk.f.log.Error("reconcile: list tickets", "node", lk.nodeID, "error", err)
		return
	}
	for _, t := range tickets {
		a := t.LatestAssignment()
		if a == nil || a.Node != lk.nodeID || a.State == core.AssignmentEnded {
			continue
		}
		r.Start(t, *a)
		if len(t.Undelivered) > 0 && a.State == core.AssignmentRunning {
			r.Deliver(t, *a)
		}
	}
	s, ok := lk.fetchStatus()
	if !ok {
		return
	}
	for _, id := range s.LiveTicketIDs {
		t, _, err := eng.Ticket(id)
		if err != nil {
			var ue *core.UserError
			if errors.As(err, &ue) {
				// A Ticket the server does not know: nothing to keep it running for.
				r.Stop(core.Ticket{ID: id}, core.Assignment{}, reconcileStopReason)
				continue
			}
			lk.f.log.Error("reconcile: read ticket", "node", lk.nodeID, "ticket", id, "error", err)
			continue
		}
		latest := t.LatestAssignment()
		if latest != nil && latest.Node == lk.nodeID && latest.State != core.AssignmentEnded {
			continue
		}
		// The node runs one of the Ticket's Assignments that is no longer its live one here; Stop names the session's
		// Assignment, so tell it of each one placed on this node (the node ignores the others).
		sent := false
		for i := len(t.Assignments) - 1; i >= 0; i-- {
			if a := t.Assignments[i]; a.Node == lk.nodeID || a.Node == "" {
				r.Stop(t, a, reconcileStopReason)
				sent = true
			}
		}
		if !sent && latest != nil {
			r.Stop(t, *latest, reconcileStopReason)
		}
	}
}

// closeConn is the link's Conn with the close code and reason the fleet chose: the Session closes its Conn itself,
// with its own code, when its context ends.
type closeConn struct {
	link.Conn
	mu     sync.Mutex
	set    bool
	code   int
	reason string
}

func (c *closeConn) closeWith(code int, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.set {
		c.set, c.code, c.reason = true, code, reason
	}
}

func (c *closeConn) Close(code int, reason string) error {
	c.mu.Lock()
	if c.set {
		code, reason = c.code, c.reason
	}
	c.mu.Unlock()
	return c.Conn.Close(code, reason)
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
