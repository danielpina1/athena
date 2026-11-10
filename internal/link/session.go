package link

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// Close codes the Session sends.
const (
	CloseDead     = 4000 // no frame for DeadAfter
	CloseProtocol = 4001 // a frame the peer must not send: undecodable, or a gap in its sequence
)

var (
	// ErrBackpressure is a Send into a full channel queue; the caller decides what to drop or coalesce.
	ErrBackpressure = errors.New("link: channel queue full")
	// ErrClosed is a Send after Run has returned.
	ErrClosed = errors.New("link: session closed")
	// ErrPeerDead is the Run result when nothing was received for DeadAfter.
	ErrPeerDead = errors.New("link: peer silent")
	// ErrInternal is the Run result when the session could not encode a frame it had accepted.
	ErrInternal = errors.New("link: internal error")
	// ErrProtocol is the Run result when the peer broke the framing or sequencing rules.
	ErrProtocol = errors.New("link: protocol violation")
)

const (
	defaultHeartbeat = 15 * time.Second
	defaultDeadAfter = 45 * time.Second
	defaultAckDelay  = 200 * time.Millisecond
	defaultQueue     = 256
	// blobQueue is BLOB's default queue: its frames are near MaxFrame, so 16 of them is already 16 MiB in memory.
	blobQueue = 16
	// defaultMaxUnackedBytes bounds the encoded size of the durable frames in flight, next to maxUnacked.
	defaultMaxUnackedBytes = 64 << 20
	// maxUnacked bounds the durable frames in flight (sent and not acked, plus queued): a peer that pings but never
	// acks makes durable Sends fail with ErrBackpressure instead of growing memory.
	maxUnacked = 1024
)

// StampSlack is what Channel, Seq and Ack add to an encoded Frame at most: three fields, two with a 10-byte varint.
// Send refuses a frame whose body leaves less room than that under MaxFrame.
const StampSlack = 24

// Resume carries a dropped Session's state into its successor on a new connection.
type Resume struct {
	// Received is the old Session's LastReceived. The caller sends it as Hello.last_seq so the peer replays after it,
	// and the new Session keeps dropping duplicates from there.
	Received uint64
	// PeerLastSeq is the peer's Hello.last_seq: the last frame of ours it received. Frames up to it count as acked.
	PeerLastSeq uint64
	// Unacked is the old Session's Unacked(). Those after PeerLastSeq are written first, with their seq unchanged.
	Unacked []*athenav1.Frame
}

// Options configures a Session. The zero value of each field selects the default.
type Options struct {
	Heartbeat time.Duration // Ping interval, 15 s
	DeadAfter time.Duration // silence before the peer is dead, 45 s
	AckDelay  time.Duration // silence before a standalone ack, 200 ms
	// SendQueue is the bound of each channel's send queue; a channel not listed holds 256, BLOB 16.
	SendQueue map[athenav1.Channel]int
	// MaxUnackedBytes bounds the encoded size of the durable frames sent and not acked, plus queued; 64 MiB. A durable
	// Send over it is ErrBackpressure, as it is over the 1024-frame cap.
	MaxUnackedBytes int
	// OnFrame receives every frame except Ping, Pong and Credit, in order, once each, from the reader goroutine.
	// It must not block for long.
	OnFrame func(*athenav1.Frame)
	// OnCredit is called from the reader goroutine after a Credit frame raised ch's total (see Session.Credit). It must
	// not block for long.
	OnCredit func(ch athenav1.Channel)
	Resume   *Resume
	Now      func() time.Time                     // default time.Now
	After    func(time.Duration) <-chan time.Time // default: stoppable timers from time.NewTimer
}

// Session is the reliable layer of one link connection. Durable frames are numbered per direction from 1 and acked
// cumulatively; non-durable frames carry seq 0 and are lost with the connection. Each channel has its own bounded
// send queue, written CONTROL, EVENTS, STREAM, BLOB. A Session lives for one connection: after Run returns, hand
// Unacked() and LastReceived() to the next one through Options.Resume.
type Session struct {
	conn Conn
	o    Options
	now  func() time.Time
	wake chan struct{} // writer has work
	ack  chan struct{} // a durable frame arrived

	mu       sync.Mutex
	closed   bool
	pending  int // durable frames in the queues
	bytes    int // encoded size of the durable frames in the queues and in unacked
	sizes    map[*athenav1.Frame]int
	initErr  error
	sendSeq  uint64
	unacked  []*athenav1.Frame
	replay   []*athenav1.Frame // unacked frames of a resumed Session not yet rewritten
	recvSeq  uint64
	ackDue   bool // a received frame is not yet acked
	ackFired bool // and AckDelay passed without a frame to piggyback on
	pingDue  bool
	pongDue  bool
	pongAt   uint64
	queues   map[athenav1.Channel][]queued
	credit   map[athenav1.Channel]uint32
	lastRecv time.Time
}

// New returns a Session over c; Run starts it.
func New(c Conn, o Options) *Session {
	if o.Heartbeat <= 0 {
		o.Heartbeat = defaultHeartbeat
	}
	if o.DeadAfter <= 0 {
		o.DeadAfter = defaultDeadAfter
	}
	if o.AckDelay <= 0 {
		o.AckDelay = defaultAckDelay
	}
	if o.MaxUnackedBytes <= 0 {
		o.MaxUnackedBytes = defaultMaxUnackedBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Session{
		conn:   c,
		o:      o,
		now:    o.Now,
		wake:   make(chan struct{}, 1),
		ack:    make(chan struct{}, 1),
		queues: map[athenav1.Channel][]queued{},
		credit: map[athenav1.Channel]uint32{},
		sizes:  map[*athenav1.Frame]int{},
	}
	if r := o.Resume; r != nil {
		s.recvSeq = r.Received
		s.sendSeq = r.PeerLastSeq
		if n := len(r.Unacked); n > 0 && r.PeerLastSeq > r.Unacked[n-1].GetSeq() {
			// The peer claims a frame of ours that we never numbered: another session's state, or a lie.
			s.initErr = fmt.Errorf("%w: resume beyond sent, peer has %d, last sent is %d", ErrProtocol, r.PeerLastSeq, r.Unacked[n-1].GetSeq())
		}
		for _, f := range r.Unacked {
			switch {
			case f.GetSeq() <= r.PeerLastSeq:
			case f.GetSeq() != s.sendSeq+1:
				s.initErr = fmt.Errorf("%w: resume gap, peer has %d, next frame is %d", ErrProtocol, s.sendSeq, f.GetSeq())
			default:
				s.sendSeq = f.GetSeq()
				s.unacked = append(s.unacked, f)
				s.replay = append(s.replay, f)
				s.track(f, proto.Size(f))
			}
		}
	}
	return s
}

type queued struct {
	f       *athenav1.Frame
	durable bool
}

// channelOrder is the writer's service order.
var channelOrder = []athenav1.Channel{athenav1.Channel_CONTROL, athenav1.Channel_EVENTS, athenav1.Channel_STREAM, athenav1.Channel_BLOB}

// track counts a durable frame of n bytes as in flight.
func (s *Session) track(f *athenav1.Frame, n int) {
	s.sizes[f] = n
	s.bytes += n
}

// untrack releases a durable frame's bytes.
func (s *Session) untrack(f *athenav1.Frame) {
	s.bytes -= s.sizes[f]
	delete(s.sizes, f)
}

// Send queues f on ch, taking ownership of it. It never blocks: a full queue, or maxUnacked durable frames or
// MaxUnackedBytes in flight, is ErrBackpressure. A durable frame is numbered when it is written, so seq follows write
// order across channels, and is kept until the peer acks it. A frame with no body, or too big to carry its seq and
// ack, is refused.
func (s *Session) Send(ch athenav1.Channel, durable bool, f *athenav1.Frame) error {
	if f.GetBody() == nil {
		return fmt.Errorf("%w: channel %v, body %T", ErrInvalid, ch, f.GetBody())
	}
	f.Channel = ch
	f.Seq, f.Ack = 0, 0
	n := proto.Size(f)
	if n > MaxFrame-StampSlack {
		return fmt.Errorf("%w: %d bytes, over %d", ErrInvalid, n, MaxFrame-StampSlack)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	limit, ok := s.o.SendQueue[ch]
	if !ok {
		limit = defaultQueue
		if ch == athenav1.Channel_BLOB {
			limit = blobQueue
		}
	}
	if len(s.queues[ch]) >= limit || durable && (len(s.unacked)+s.pending >= maxUnacked || s.bytes+n > s.o.MaxUnackedBytes) {
		s.mu.Unlock()
		return ErrBackpressure
	}
	s.queues[ch] = append(s.queues[ch], queued{f, durable})
	if durable {
		s.pending++
		s.track(f, n)
	}
	s.mu.Unlock()
	signal(s.wake)
	return nil
}

// Grant gives the peer items more credit on ch. Credit belongs to one connection: the grant is not durable, a
// Session's tally starts at 0, and after a resume the caller grants again.
func (s *Session) Grant(ch athenav1.Channel, items uint32) error {
	return s.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{
		Body: &athenav1.Frame_Credit{Credit: &athenav1.Credit{Channel: ch, Items: items}},
	})
}

// Credit is the total the peer has granted on ch over this connection.
func (s *Session) Credit(ch athenav1.Channel) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credit[ch]
}

// Queued is how many frames ch's queue holds that the writer has not taken yet. The writer takes one frame at a time
// and writes it before it takes the next: a frame sent on any channel once Queued(ch) is 0 is written after every frame
// sent on ch before. After Run returns it is 0.
func (s *Session) Queued(ch athenav1.Channel) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queues[ch])
}

// Unacked is the durable frames sent and not yet acked, in seq order. After Run returns it also holds the durable
// frames that were still queued, numbered, so nothing accepted by Send is lost.
func (s *Session) Unacked() []*athenav1.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*athenav1.Frame(nil), s.unacked...)
}

// LastReceived is the seq of the last durable frame received in order.
func (s *Session) LastReceived() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvSeq
}

// after is Options.After, or a real timer the caller can stop: time.After would leave every unfired timer alive
// until it expires, however soon the Session ends.
func (s *Session) after(d time.Duration) (<-chan time.Time, func()) {
	if s.o.After != nil {
		return s.o.After(d), func() {}
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// Run drives the connection until the peer is dead (ErrPeerDead, close code 4000), the peer breaks the rules
// (ErrProtocol, 4001), the connection fails, or ctx ends. It closes the connection before returning.
func (s *Session) Run(ctx context.Context) error {
	if s.initErr != nil {
		s.flush()
		_ = s.conn.Close(CloseProtocol, "resume gap")
		return s.initErr
	}
	// The loops do not end with ctx itself: the Close below must come first, so the peer reads its code and reason (a
	// WebSocket read cancelled under it drops the connection without them).
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	s.mu.Lock()
	s.lastRecv = s.now()
	s.mu.Unlock()

	errc := make(chan error, 4)
	var wg sync.WaitGroup
	for _, loop := range []func(context.Context) error{s.readLoop, s.writeLoop, s.heartbeatLoop, s.ackLoop} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errc <- loop(rctx)
		}()
	}
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
		err = ctx.Err()
	}
	code, reason := 1000, "closing"
	switch {
	case errors.Is(err, ErrPeerDead):
		code, reason = CloseDead, "peer silent"
	case errors.Is(err, ErrProtocol):
		code, reason = CloseProtocol, "protocol violation"
	case errors.Is(err, ErrInternal):
		code, reason = 1011, "internal error"
	case ctx.Err() != nil:
		code, reason = 1001, "going away"
	}
	_ = s.conn.Close(code, reason) // before cancel: a cancelled WebSocket read drops the connection without a close frame
	cancel()
	wg.Wait()
	s.flush()
	return err
}

// flush numbers the durable frames still queued so Unacked covers them, and refuses further Sends.
func (s *Session) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, ch := range channelOrder {
		for _, q := range s.queues[ch] {
			if q.durable {
				s.sendSeq++
				q.f.Seq = s.sendSeq
				s.unacked = append(s.unacked, q.f)
			}
		}
	}
	s.pending = 0
	s.queues = nil
	s.replay = nil
}

func (s *Session) readLoop(ctx context.Context) error {
	for {
		b, err := s.conn.Read(ctx)
		if err != nil {
			return err
		}
		f, err := Decode(b)
		if err != nil {
			s.refuseFrame(err)
			return fmt.Errorf("%w: %v", ErrProtocol, err)
		}
		deliver, err := s.receive(f)
		if err != nil {
			return err
		}
		if deliver && s.o.OnFrame != nil {
			s.o.OnFrame(f)
		}
		if c := f.GetCredit(); c != nil && s.o.OnCredit != nil {
			s.o.OnCredit(c.GetChannel())
		}
	}
}

// refuseFrame tells the peer, best effort, that a frame did not decode: an invalid Error written straight to the
// connection (the write loop's queue is not drained on the way to the close, and Conn writes may be concurrent).
func (s *Session) refuseFrame(cause error) {
	b, err := Encode(&athenav1.Frame{Channel: athenav1.Channel_CONTROL,
		Body: &athenav1.Frame_Error{Error: &athenav1.Error{Code: "invalid", Message: "undecodable frame: " + cause.Error()}}})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.conn.Write(ctx, b)
}

// receive does the link-level bookkeeping of one frame and reports whether the caller should see it.
func (s *Session) receive(f *athenav1.Frame) (deliver bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRecv = s.now()
	if a := f.GetAck(); a > 0 {
		n := 0
		for n < len(s.unacked) && s.unacked[n].GetSeq() <= a {
			s.untrack(s.unacked[n])
			s.unacked[n] = nil
			n++
		}
		s.unacked = s.unacked[n:]
	}
	if seq := f.GetSeq(); seq != 0 {
		switch {
		case seq <= s.recvSeq: // the peer missed our ack; say it again
			s.ackDue = true
			signal(s.ack)
			return false, nil
		case seq != s.recvSeq+1:
			return false, fmt.Errorf("%w: got seq %d after %d", ErrProtocol, seq, s.recvSeq)
		}
		s.recvSeq = seq
		s.ackDue = true
		signal(s.ack)
	}
	switch b := f.GetBody().(type) {
	case *athenav1.Frame_Ping:
		if at := b.Ping.GetAtUnixMs(); at != 0 { // at 0 is a standalone ack, nothing to answer
			s.pongDue, s.pongAt = true, at
			signal(s.wake)
		}
	case *athenav1.Frame_Pong:
	case *athenav1.Frame_Credit:
		ch, n := b.Credit.GetChannel(), b.Credit.GetItems()
		if sum := uint64(s.credit[ch]) + uint64(n); sum > 1<<32-1 {
			s.credit[ch] = 1<<32 - 1
		} else {
			s.credit[ch] = uint32(sum)
		}
	default:
		return true, nil
	}
	return false, nil
}

// ackLoop turns "a received frame is unacked" into a standalone ack once AckDelay passes without a frame to carry it.
func (s *Session) ackLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ack:
		}
		delay, stop := s.after(s.o.AckDelay)
		select {
		case <-ctx.Done():
			stop()
			return ctx.Err()
		case <-delay:
		}
		s.mu.Lock()
		if s.ackDue {
			s.ackFired = true
		}
		s.mu.Unlock()
		signal(s.wake)
	}
}

// heartbeatLoop pings every Heartbeat and declares the peer dead exactly DeadAfter after the last frame: the
// deadline timer is re-armed for what remains whenever it fires early.
func (s *Session) heartbeatLoop(ctx context.Context) error {
	ping, stopPing := s.after(s.o.Heartbeat)
	dead, stopDead := s.after(s.o.DeadAfter)
	defer func() { stopPing(); stopDead() }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ping:
			ping, stopPing = s.after(s.o.Heartbeat)
			s.mu.Lock()
			s.pingDue = true
			s.mu.Unlock()
			signal(s.wake)
		case <-dead:
			s.mu.Lock()
			rem := s.o.DeadAfter - s.now().Sub(s.lastRecv)
			s.mu.Unlock()
			if rem <= 0 {
				return ErrPeerDead
			}
			dead, stopDead = s.after(rem)
		}
	}
}

func (s *Session) writeLoop(ctx context.Context) error {
	for {
		b, err := s.next()
		if err != nil {
			return err
		}
		if b == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-s.wake:
			}
			continue
		}
		if err := s.conn.Write(ctx, b); err != nil {
			return err
		}
	}
}

// next picks the frame to write, stamps and encodes it, or returns nil when there is nothing to say. Order: frames of
// a resume (they hold the lowest seqs), pong and ping, the channel queues, then a standalone ack. A frame that will
// not encode (Send's size check makes that a bug) is dropped, numbered or not, and ends the session with ErrInternal.
func (s *Session) next() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var f *athenav1.Frame
	fresh := false // a queued durable frame, numbered just now
	switch {
	case len(s.replay) > 0:
		f, s.replay[0], s.replay = s.replay[0], nil, s.replay[1:]
	case s.pongDue:
		s.pongDue = false
		f = &athenav1.Frame{Channel: athenav1.Channel_CONTROL, Body: &athenav1.Frame_Pong{Pong: &athenav1.Pong{AtUnixMs: s.pongAt}}}
	case s.pingDue:
		s.pingDue = false
		f = &athenav1.Frame{Channel: athenav1.Channel_CONTROL, Body: &athenav1.Frame_Ping{Ping: &athenav1.Ping{AtUnixMs: uint64(s.now().UnixMilli())}}}
	default:
		for _, ch := range channelOrder {
			q := s.queues[ch]
			if len(q) == 0 {
				continue
			}
			head := q[0]
			q[0] = queued{}
			s.queues[ch] = q[1:]
			f = head.f
			if head.durable {
				s.pending--
				s.sendSeq++
				f.Seq = s.sendSeq
				s.unacked = append(s.unacked, f)
				fresh = true
			}
			break
		}
	}
	if f == nil && s.ackDue && s.ackFired {
		f = &athenav1.Frame{Channel: athenav1.Channel_CONTROL, Body: &athenav1.Frame_Ping{Ping: &athenav1.Ping{}}}
	}
	if f == nil {
		return nil, nil
	}
	f.Ack = s.recvSeq
	b, err := Encode(f)
	if err != nil {
		if f.Seq != 0 && len(s.unacked) > 0 && s.unacked[len(s.unacked)-1] == f {
			s.untrack(f)
			s.unacked[len(s.unacked)-1] = nil
			s.unacked = s.unacked[:len(s.unacked)-1]
			if fresh {
				s.sendSeq--
			}
		}
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	s.ackDue, s.ackFired = false, false
	return b, nil
}
