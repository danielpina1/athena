package nodelink

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// pipeConn is a link.Conn over one end of a net.Pipe: each message is a 4-byte length and the bytes.
type pipeConn struct {
	c    net.Conn
	wmu  sync.Mutex
	once sync.Once
}

func pipePair() (*pipeConn, *pipeConn) {
	a, b := net.Pipe()
	return &pipeConn{c: a}, &pipeConn{c: b}
}

func (p *pipeConn) Read(ctx context.Context) ([]byte, error) {
	stop := context.AfterFunc(ctx, func() { _ = p.c.SetReadDeadline(time.Now()) })
	defer stop()
	var n [4]byte
	if _, err := io.ReadFull(p.c, n[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint32(n[:]))
	if _, err := io.ReadFull(p.c, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (p *pipeConn) Write(ctx context.Context, b []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = p.c.SetWriteDeadline(time.Now()) })
	defer stop()
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err := p.c.Write(buf)
	return err
}

func (p *pipeConn) Close(int, string) error {
	p.once.Do(func() { _ = p.c.Close() })
	return nil
}

// fakeServer is the server's end of one node link: it answers the Hello with a Welcome, records every NodeCall, and
// answers facts (acknowledged) and requests (with answer, if set).
type fakeServer struct {
	t       *testing.T
	applied uint64
	answer  func(call *athenav1.NodeCall) *athenav1.NodeResult
	// gate, when set, stops the server reading after gateAfter messages until it is closed: a server that pushes back.
	gate      chan struct{}
	gateAfter int
	// silentFacts: facts are not answered (the server has not applied them).
	silentFacts bool

	mu      sync.Mutex
	hello   *athenav1.NodeHello
	calls   []*athenav1.NodeCall
	results []*athenav1.NodeResult
	// order is what came, in order: "blob <transfer> <bytes> <last>" or the call's method.
	order []string
	blobs []*athenav1.BlobChunk
	sess  *link.Session
	done  chan struct{}
	stop  context.CancelFunc
}

// servers hands a fresh fakeServer to each dial of the client, in order.
type servers struct {
	t    *testing.T
	next chan *fakeServer
}

func newServers(t *testing.T) *servers { return &servers{t: t, next: make(chan *fakeServer, 8)} }

func (s *servers) dial(ctx context.Context) (link.Conn, error) {
	select {
	case fs := <-s.next:
		nc, sc := pipePair()
		fs.start(sc)
		return nc, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return nil, errors.New("no server")
	}
}

func (s *servers) add(applied uint64) *fakeServer {
	fs := &fakeServer{t: s.t, applied: applied, done: make(chan struct{})}
	s.next <- fs
	return fs
}

func (fs *fakeServer) start(c link.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	fs.stop = cancel
	if fs.gate != nil {
		c = &gatedConn{Conn: c, gate: fs.gate, after: fs.gateAfter}
	}
	fs.sess = link.New(c, link.Options{Heartbeat: 3 * time.Second, DeadAfter: 9 * time.Second, OnFrame: fs.onFrame})
	go func() {
		defer close(fs.done)
		_ = fs.sess.Run(ctx)
	}()
}

func (fs *fakeServer) onFrame(f *athenav1.Frame) {
	switch b := f.GetBody().(type) {
	case *athenav1.Frame_NodeHello:
		fs.mu.Lock()
		fs.hello = b.NodeHello
		fs.mu.Unlock()
		_ = fs.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeWelcome{
			NodeWelcome: &athenav1.NodeWelcome{Epoch: "e1", AppliedFactSeq: fs.applied, HeartbeatMs: 3000, NodeName: "mini"}}})
	case *athenav1.Frame_NodeResult:
		fs.mu.Lock()
		fs.results = append(fs.results, b.NodeResult)
		fs.mu.Unlock()
	case *athenav1.Frame_BlobChunk:
		fs.mu.Lock()
		fs.blobs = append(fs.blobs, b.BlobChunk)
		fs.order = append(fs.order, "blob")
		fs.mu.Unlock()
	case *athenav1.Frame_NodeCall:
		call := b.NodeCall
		fs.mu.Lock()
		fs.calls = append(fs.calls, call)
		fs.order = append(fs.order, call.GetMethod())
		fs.mu.Unlock()
		if call.GetMethod() == MethodLines || fs.silentFacts && call.GetFactSeq() > 0 {
			return
		}
		var r *athenav1.NodeResult
		if fs.answer != nil {
			r = fs.answer(call)
		}
		if r == nil {
			r = &athenav1.NodeResult{}
		}
		r.Id = call.GetId()
		_ = fs.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeResult{NodeResult: r}})
	}
}

// facts returns the fact calls received so far.
func (fs *fakeServer) facts() []*athenav1.NodeCall {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []*athenav1.NodeCall
	for _, c := range fs.calls {
		if c.GetFactSeq() > 0 {
			out = append(out, c)
		}
	}
	return out
}

// result returns the result of the server call id, or nil.
func (fs *fakeServer) result(id uint64) *athenav1.NodeResult {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, r := range fs.results {
		if r.GetId() == id {
			return r
		}
	}
	return nil
}

func (fs *fakeServer) close() {
	fs.stop()
	<-fs.done
}

// call sends a server→node call; its result lands in results.
func (fs *fakeServer) call(t *testing.T, id uint64, method string, args any) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeCall{
		NodeCall: &athenav1.NodeCall{Id: id, Method: method, Args: b}}}); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newTestClient is a Client on a temp state directory that dials srv.
func newTestClient(t *testing.T, srv *servers) *Client {
	t.Helper()
	id := &Identity{NodeID: "n0123456789", Name: "mini", NodeURL: "wss://athena.test:8444/node"}
	c, err := New(id, Options{StateDir: t.TempDir(), Version: "test", Dial: srv.dial, MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// run starts c.Run until the test ends.
func run(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// fakeTarget records what serve asks of the node.
type fakeTarget struct {
	mu       sync.Mutex
	starts   []core.Ticket
	dropped  []string
	dir      string
	backfill []node.Line
	live     chan node.Line
	// bundle is what CheckpointBundle writes, sha its commit; sinces records the bases it was asked for.
	bundle []byte
	sha    string
	sinces []string
	tooBig bool
}

func (f *fakeTarget) CheckpointBundle(_ context.Context, _ string, since string, w io.Writer) (string, error) {
	f.mu.Lock()
	f.sinces = append(f.sinces, since)
	b, sha := f.bundle, f.sha
	f.mu.Unlock()
	if sha == "" {
		return "", node.ErrNoCheckpoint
	}
	if since == sha {
		return sha, node.ErrCheckpointUnchanged
	}
	if f.tooBig {
		return "", node.ErrCheckpointTooBig
	}
	_, err := w.Write(b)
	return sha, err
}

func (f *fakeTarget) setCheckpoint(b []byte, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bundle, f.sha = b, sha
}

func (f *fakeTarget) bases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sinces...)
}

func (f *fakeTarget) Start(t core.Ticket, a core.Assignment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, t)
}
func (f *fakeTarget) Seed(node.Seed) error                      { return nil }
func (f *fakeTarget) Deliver(core.Ticket, core.Assignment)      {}
func (f *fakeTarget) Stop(core.Ticket, core.Assignment, string) {}
func (f *fakeTarget) Interrupt(string) error                    { return node.ErrNoSession }
func (f *fakeTarget) SetPaused(bool)                            {}
func (f *fakeTarget) DropTokens(repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, repo)
}
func (f *fakeTarget) ApplyProposal(context.Context, core.Proposal) error { return nil }
func (f *fakeTarget) TicketDiff(context.Context, string) (node.Diff, error) {
	return node.Diff{}, node.ErrNoWorkspace
}
func (f *fakeTarget) TicketFiles(context.Context, string) ([]node.FileEntry, bool, error) {
	return nil, false, nil
}
func (f *fakeTarget) OpenTicketFile(_ context.Context, _, rel string) (*os.File, error) {
	if f.dir == "" {
		return nil, node.ErrNotFound
	}
	return os.Open(f.dir + "/" + rel)
}
func (f *fakeTarget) Subscribe(string, int64) ([]node.Line, <-chan node.Line, func(), error) {
	if f.live == nil {
		return nil, nil, func() {}, node.ErrNoSession
	}
	return f.backfill, f.live, func() {}, nil
}
func (f *fakeTarget) Status() node.Status { return node.Status{Name: "mini", Slots: 2} }

// gatedConn reads after messages, then waits for gate to close before reading more.
type gatedConn struct {
	link.Conn
	gate  chan struct{}
	after int
	n     int // Read is called from one goroutine
}

func (g *gatedConn) Read(ctx context.Context) ([]byte, error) {
	if g.n >= g.after {
		select {
		case <-g.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	g.n++
	return g.Conn.Read(ctx)
}

// methodsOf lists the methods of the calls the server received, leaving out prompts requests.
func (fs *fakeServer) methodsOf() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []string
	for _, c := range fs.calls {
		if c.GetMethod() != MethodPrompts {
			out = append(out, c.GetMethod())
		}
	}
	return out
}

// checkRun asserts the fact seqs received are exactly from+1..to, in order, once each.
func checkRun(t *testing.T, got []*athenav1.NodeCall, from, to uint64) {
	t.Helper()
	if uint64(len(got)) != to-from {
		t.Fatalf("got %d facts, want %d (%d..%d)", len(got), to-from, from+1, to)
	}
	for i, c := range got {
		if want := from + 1 + uint64(i); c.GetFactSeq() != want {
			t.Fatalf("fact %d has seq %d, want %d", i, c.GetFactSeq(), want)
		}
	}
}
