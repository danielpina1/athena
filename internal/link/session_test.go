package link

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// memConn is one end of an in-memory Conn pair.
type memConn struct {
	in   chan []byte
	out  chan []byte
	done chan struct{}
	once *sync.Once
	mu   *sync.Mutex
	code *int
	drop bool // writes vanish
}

func memPair() (*memConn, *memConn) {
	a2b, b2a := make(chan []byte, 1024), make(chan []byte, 1024)
	done, once, mu := make(chan struct{}), &sync.Once{}, &sync.Mutex{}
	code := new(int)
	return &memConn{in: b2a, out: a2b, done: done, once: once, mu: mu, code: code},
		&memConn{in: a2b, out: b2a, done: done, once: once, mu: mu, code: code}
}

func (m *memConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-m.in:
		return b, nil
	case <-m.done:
		return nil, errors.New("closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *memConn) Write(ctx context.Context, b []byte) error {
	if m.drop {
		return nil
	}
	select {
	case m.out <- b:
		return nil
	case <-m.done:
		return errors.New("closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *memConn) Close(code int, _ string) error {
	m.once.Do(func() {
		m.mu.Lock()
		*m.code = code
		m.mu.Unlock()
		close(m.done)
	})
	return nil
}

func (m *memConn) closeCode() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.code
}

// fakeClock fires After channels only when Advance passes their time.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, fakeWaiter{c.now.Add(d), ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	keep := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
}

func (c *fakeClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// advanceUntil advances clk by step until cond holds: the sessions register their timers asynchronously.
func advanceUntil(t *testing.T, clk *fakeClock, step time.Duration, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		clk.Advance(step)
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type collector struct {
	mu     sync.Mutex
	frames []*athenav1.Frame
}

func (c *collector) add(f *athenav1.Frame) {
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
}

func (c *collector) cursors() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []uint64
	for _, f := range c.frames {
		out = append(out, f.GetDelta().GetSeq())
	}
	return out
}

func (c *collector) len() int { return len(c.cursors()) }

// delta is a durable-test payload; Delta.seq is only a tag here, never the link seq.
func delta(tag uint64) *athenav1.Frame {
	return &athenav1.Frame{Body: &athenav1.Frame_Delta{Delta: &athenav1.Delta{Seq: tag}}}
}

func start(t *testing.T, c Conn, o Options) (*Session, func() error) {
	t.Helper()
	s := New(c, o)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- s.Run(ctx) }()
	t.Cleanup(cancel)
	return s, func() error { cancel(); return <-res }
}

func TestDurableFramesNumberedAndAcked(t *testing.T) {
	ca, cb := memPair()
	clk := newFakeClock()
	var got collector
	a, _ := start(t, ca, Options{Heartbeat: time.Hour, DeadAfter: 2 * time.Hour, Now: clk.Now, After: clk.After})
	b, _ := start(t, cb, Options{Heartbeat: time.Hour, DeadAfter: 2 * time.Hour, Now: clk.Now, After: clk.After, OnFrame: got.add})

	for i := uint64(1); i <= 3; i++ {
		if err := a.Send(athenav1.Channel_EVENTS, true, delta(i*10)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "3 frames", func() bool { return got.len() == 3 })
	for i, f := range got.frames {
		if f.Seq != uint64(i+1) || f.Channel != athenav1.Channel_EVENTS {
			t.Fatalf("frame %d: seq %d channel %v", i, f.Seq, f.Channel)
		}
	}
	if n := len(a.Unacked()); n != 3 {
		t.Fatalf("unacked before ack: %d", n)
	}
	if b.LastReceived() != 3 {
		t.Fatalf("LastReceived %d", b.LastReceived())
	}
	// Silence on b's side: the standalone ack arrives after 200 ms.
	advanceUntil(t, clk, 200*time.Millisecond, "standalone ack", func() bool { return len(a.Unacked()) == 0 })

}

func TestAckPiggybacksOnNextFrame(t *testing.T) {
	ca, cb := memPair()
	clk := newFakeClock() // never advanced: a standalone ack cannot fire
	a, _ := start(t, ca, Options{Heartbeat: time.Hour, DeadAfter: 2 * time.Hour, Now: clk.Now, After: clk.After})
	var got collector
	b, _ := start(t, cb, Options{Heartbeat: time.Hour, DeadAfter: 2 * time.Hour, Now: clk.Now, After: clk.After, OnFrame: got.add})
	if err := a.Send(athenav1.Channel_EVENTS, true, delta(1)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b received", func() bool { return got.len() == 1 })
	if err := b.Send(athenav1.Channel_CONTROL, true, delta(2)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a's frame acked by b's reply", func() bool { return len(a.Unacked()) == 0 })
}

func TestDuplicateSeqDropped(t *testing.T) {
	ca, cb := memPair()
	var got collector
	_, _ = start(t, cb, Options{OnFrame: got.add})
	send := func(seq uint64, tag uint64) {
		f := delta(tag)
		f.Channel, f.Seq = athenav1.Channel_EVENTS, seq
		b, err := Encode(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := ca.Write(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	send(1, 10)
	send(2, 20)
	send(2, 21) // resent after a resume: dropped
	send(1, 11)
	send(3, 30)
	eventually(t, "3 delivered", func() bool { return got.len() == 3 })
	time.Sleep(20 * time.Millisecond)
	if c := got.cursors(); len(c) != 3 || c[0] != 10 || c[1] != 20 || c[2] != 30 {
		t.Fatalf("delivered %v", c)
	}
}

func TestSequenceGapClosesConnection(t *testing.T) {
	ca, cb := memPair()
	s := New(cb, Options{})
	f := delta(1)
	f.Channel, f.Seq = athenav1.Channel_EVENTS, 2
	b, _ := Encode(f)
	_ = ca.Write(context.Background(), b)
	if err := s.Run(context.Background()); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Run: %v", err)
	}
	if cb.closeCode() != CloseProtocol {
		t.Fatalf("close code %d", cb.closeCode())
	}
}

func TestResumeReplaysUnacked(t *testing.T) {
	ca, _ := memPair()
	ca.drop = true // everything a writes is lost: the link is already dead
	a1, stop := start(t, ca, Options{})
	for i := uint64(1); i <= 5; i++ {
		if err := a1.Send(athenav1.Channel_EVENTS, true, delta(i)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "a wrote all 5", func() bool {
		a1.mu.Lock()
		defer a1.mu.Unlock()
		return a1.sendSeq == 5
	})
	_ = stop()
	// A closed Session refuses Send.
	if err := a1.Send(athenav1.Channel_EVENTS, true, delta(6)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Run: %v", err)
	}

	// New connection. The peer's Hello.last_seq is 2: it has frames 1 and 2.
	nc, nd := memPair()
	var got collector
	b2, _ := start(t, nd, Options{OnFrame: got.add, Resume: &Resume{Received: 2}})
	a2, _ := start(t, nc, Options{Resume: &Resume{PeerLastSeq: 2, Unacked: a1.Unacked()}})
	if err := a2.Send(athenav1.Channel_EVENTS, true, delta(6)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "frames 3..6", func() bool { return got.len() == 4 })
	time.Sleep(20 * time.Millisecond)
	c := got.cursors()
	for i, want := range []uint64{3, 4, 5, 6} {
		if c[i] != want {
			t.Fatalf("delivered %v, want 3 4 5 6", c)
		}
		if got.frames[i].Seq != want {
			t.Fatalf("frame %d has link seq %d, want %d (numbering continues)", i, got.frames[i].Seq, want)
		}
	}
	if len(c) != 4 || b2.LastReceived() != 6 {
		t.Fatalf("delivered %v, LastReceived %d", c, b2.LastReceived())
	}
}

func TestResumeGapRefused(t *testing.T) {
	ca, _ := memPair()
	f := delta(1)
	f.Seq = 5
	s := New(ca, Options{Resume: &Resume{PeerLastSeq: 2, Unacked: []*athenav1.Frame{f}}})
	if err := s.Run(context.Background()); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Run: %v", err)
	}
}

func TestStreamQueueFullDoesNotBlockControl(t *testing.T) {
	ca, cb := memPair()
	ca.drop = false
	// a's writer is stalled by a Conn that blocks Write until released.
	gate := make(chan struct{})
	slow := &gatedConn{memConn: ca, gate: gate}
	a, _ := start(t, slow, Options{SendQueue: map[athenav1.Channel]int{athenav1.Channel_STREAM: 2, athenav1.Channel_CONTROL: 4}})
	var got collector
	_, _ = start(t, cb, Options{OnFrame: got.add})

	// The writer takes one frame and blocks in Write; the rest queue.
	_ = a.Send(athenav1.Channel_STREAM, false, delta(1))
	eventually(t, "writer in Write", func() bool { return slow.blocked() })
	_ = a.Send(athenav1.Channel_STREAM, false, delta(2))
	_ = a.Send(athenav1.Channel_STREAM, false, delta(3))
	start := time.Now()
	if err := a.Send(athenav1.Channel_STREAM, false, delta(4)); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("full stream queue: %v", err)
	}
	if err := a.Send(athenav1.Channel_CONTROL, true, delta(100)); err != nil {
		t.Fatalf("control send with stream full: %v", err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("Send blocked")
	}
	close(gate)
	eventually(t, "all delivered", func() bool { return got.len() == 4 })
	// Served CONTROL first once the writer is free: 1 was already in flight, then control, then the stream.
	if c := got.cursors(); c[0] != 1 || c[1] != 100 || c[2] != 2 || c[3] != 3 {
		t.Fatalf("order %v, want 1 100 2 3", c)
	}
}

type gatedConn struct {
	*memConn
	gate chan struct{}
	mu   sync.Mutex
	in   bool
}

func (g *gatedConn) Write(ctx context.Context, b []byte) error {
	g.mu.Lock()
	g.in = true
	g.mu.Unlock()
	select {
	case <-g.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.memConn.Write(ctx, b)
}

func (g *gatedConn) blocked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.in
}

func TestCreditTracking(t *testing.T) {
	ca, cb := memPair()
	a, _ := start(t, ca, Options{})
	b, _ := start(t, cb, Options{})
	if err := a.Grant(athenav1.Channel_STREAM, 256); err != nil {
		t.Fatal(err)
	}
	if err := a.Grant(athenav1.Channel_STREAM, 44); err != nil {
		t.Fatal(err)
	}
	if err := a.Grant(athenav1.Channel_EVENTS, 5); err != nil {
		t.Fatal(err)
	}
	eventually(t, "credit", func() bool { return b.Credit(athenav1.Channel_STREAM) == 300 })
	if b.Credit(athenav1.Channel_EVENTS) != 5 || b.Credit(athenav1.Channel_CONTROL) != 0 || a.Credit(athenav1.Channel_STREAM) != 0 {
		t.Fatalf("credit: events %d control %d, a's own %d", b.Credit(athenav1.Channel_EVENTS), b.Credit(athenav1.Channel_CONTROL), a.Credit(athenav1.Channel_STREAM))
	}
}

func TestHeartbeatKillsSilentPeer(t *testing.T) {
	ca, cb := memPair() // nobody reads cb: the peer is silent
	clk := newFakeClock()
	s := New(ca, Options{Now: clk.Now, After: clk.After})
	res := make(chan error, 1)
	go func() { res <- s.Run(context.Background()) }()

	for _, at := range []time.Duration{15, 30} {
		eventually(t, "heartbeat timer", func() bool { return clk.waiting() >= 1 })
		clk.Advance(15 * time.Second)
		select {
		case err := <-res:
			t.Fatalf("died at %v: %v", at*time.Second, err)
		case b := <-cb.in:
			f, err := Decode(b)
			if err != nil || f.GetPing() == nil || f.GetPing().AtUnixMs == 0 {
				t.Fatalf("expected a ping, got %v %v", f, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no ping")
		}
	}
	eventually(t, "heartbeat timer", func() bool { return clk.waiting() >= 1 })
	clk.Advance(15 * time.Second) // 45 s of silence
	if err := <-res; !errors.Is(err, ErrPeerDead) {
		t.Fatalf("Run: %v", err)
	}
	if ca.closeCode() != CloseDead {
		t.Fatalf("close code %d, want %d", ca.closeCode(), CloseDead)
	}
}

func TestHeartbeatAnyFrameKeepsPeerAlive(t *testing.T) {
	ca, cb := memPair()
	clk := newFakeClock()
	s := New(ca, Options{Now: clk.Now, After: clk.After})
	res := make(chan error, 1)
	go func() { res <- s.Run(context.Background()) }()
	pong, _ := Encode(&athenav1.Frame{Body: &athenav1.Frame_Pong{Pong: &athenav1.Pong{AtUnixMs: 1}}})
	for i := 0; i < 6; i++ { // 90 s, a frame every 15 s
		eventually(t, "heartbeat timer", func() bool { return clk.waiting() >= 1 })
		_ = cb.Write(context.Background(), pong)
		eventually(t, "frame read", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.lastRecv.Equal(clk.Now())
		})
		clk.Advance(15 * time.Second)
		eventually(t, "heartbeat timer", func() bool { return clk.waiting() >= 1 })
		select {
		case err := <-res:
			t.Fatalf("died: %v", err)
		default:
		}
	}
}

func TestPingIsAnswered(t *testing.T) {
	ca, cb := memPair()
	_, _ = start(t, ca, Options{})
	ping, _ := Encode(&athenav1.Frame{Body: &athenav1.Frame_Ping{Ping: &athenav1.Ping{AtUnixMs: 77}}})
	_ = cb.Write(context.Background(), ping)
	select {
	case b := <-cb.in:
		if f, err := Decode(b); err != nil || f.GetPong().GetAtUnixMs() != 77 {
			t.Fatalf("got %v %v", f, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no pong")
	}
}

func TestWebSocketSubprotocolRequired(t *testing.T) {
	srvSession := make(chan *Session, 1)
	var got collector
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			return
		}
		s := New(c, Options{OnFrame: got.add})
		srvSession <- s
		_ = s.Run(r.Context())
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// No subprotocol, and a wrong one: refused.
	for _, protos := range [][]string{nil, {"other.v1"}} {
		_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: protos})
		if err == nil {
			t.Fatalf("dial with %v succeeded", protos)
		}
		if resp == nil || resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("dial with %v: %v, %v", protos, resp, err)
		}
	}

	// With athena.v1, frames flow both ways over the real socket.
	wc, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: []string{Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	if wc.Subprotocol() != Subprotocol {
		t.Fatalf("negotiated %q", wc.Subprotocol())
	}
	client := New(NewWS(wc), Options{})
	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- client.Run(rctx) }()
	if err := client.Send(athenav1.Channel_EVENTS, true, delta(7)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "frame over websocket", func() bool { return got.len() == 1 })
	srvS := <-srvSession
	eventually(t, "ack over websocket", func() bool { return len(client.Unacked()) == 0 && srvS.LastReceived() == 1 })
	stop()
	<-done
}

func TestUnackedCoversFramesStillQueuedAtClose(t *testing.T) {
	ca, _ := memPair()
	slow := &gatedConn{memConn: ca, gate: make(chan struct{})} // the writer stays stuck in its first Write
	a, stop := start(t, slow, Options{})
	_ = a.Send(athenav1.Channel_STREAM, true, delta(1))
	eventually(t, "writer in Write", func() bool { return slow.blocked() })
	_ = a.Send(athenav1.Channel_STREAM, true, delta(2))
	_ = a.Send(athenav1.Channel_STREAM, false, delta(3)) // not durable: lost with the connection
	_ = a.Send(athenav1.Channel_CONTROL, true, delta(4))
	_ = stop()
	var tags, seqs []uint64
	for _, f := range a.Unacked() {
		tags, seqs = append(tags, f.GetDelta().GetSeq()), append(seqs, f.Seq)
	}
	if len(tags) != 3 || tags[0] != 1 || tags[1] != 4 || tags[2] != 2 || seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("unacked tags %v seqs %v, want tags 1 4 2 seqs 1 2 3", tags, seqs)
	}
}
