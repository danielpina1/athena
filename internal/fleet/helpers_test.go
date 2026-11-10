package fleet

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
)

const waitTimeout = 10 * time.Second

// pipeConn is a link.Conn over one end of a net.Pipe: each message is a 4-byte length and the bytes. It records the
// first close code and reason it was closed with.
type pipeConn struct {
	c    net.Conn
	wmu  sync.Mutex
	once sync.Once

	mu     sync.Mutex
	code   int
	reason string
	closed chan struct{}
}

func pipePair() (*pipeConn, *pipeConn) {
	a, b := net.Pipe()
	return &pipeConn{c: a, closed: make(chan struct{})}, &pipeConn{c: b, closed: make(chan struct{})}
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

func (p *pipeConn) Close(code int, reason string) error {
	p.once.Do(func() {
		p.mu.Lock()
		p.code, p.reason = code, reason
		p.mu.Unlock()
		_ = p.c.Close()
		close(p.closed)
	})
	return nil
}

// closedWith waits for the conn to close and returns its code and reason.
func (p *pipeConn) closedWith(t *testing.T) (int, string) {
	t.Helper()
	select {
	case <-p.closed:
	case <-time.After(waitTimeout):
		t.Fatal("the link did not close")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.reason
}

// env is a temp store, a real Engine with an Executor that records nothing, and a node row.
type env struct {
	t   *testing.T
	st  *store.Store
	eng *core.Engine
	dir string
}

type nopExec struct{}

func (nopExec) Start(core.Ticket, core.Assignment)        {}
func (nopExec) Deliver(core.Ticket, core.Assignment)      {}
func (nopExec) Stop(core.Ticket, core.Assignment, string) {}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, dir: dir}
	e.eng = core.NewEngine(st.Repos(), nopExec{}, nil, nil)
	return e
}

// addNode makes the row a join would.
func (e *env) addNode(id, name string) {
	e.t.Helper()
	if err := e.st.PutNode(store.Node{ID: id, Name: name, CertSerial: "01", CreatedAt: time.Now()}); err != nil {
		e.t.Fatal(err)
	}
}

// placed submits a task in a temporary folder and places its Assignment on the node.
func (e *env) placed(nodeID string) core.Ticket {
	e.t.Helper()
	return e.placedWith(nodeID, core.TaskOptions{Dir: e.t.TempDir()})
}

// placedRepo is placed for a task on a repository.
func (e *env) placedRepo(nodeID, repo string) core.Ticket {
	e.t.Helper()
	return e.placedWith(nodeID, core.TaskOptions{Repo: repo})
}

func (e *env) placedWith(nodeID string, opts core.TaskOptions) core.Ticket {
	e.t.Helper()
	tk, err := e.eng.SubmitOwnerTask("Fix the typo", opts)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.eng.AssignmentPlaced(tk.ID, tk.LatestAssignment().ID, nodeID); err != nil {
		e.t.Fatal(err)
	}
	tk, _, err = e.eng.Ticket(tk.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return tk
}

func (e *env) ticket(id string) core.Ticket {
	e.t.Helper()
	tk, _, err := e.eng.Ticket(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tk
}

func (e *env) fleet(mod func(*Config)) *Fleet {
	e.t.Helper()
	cfg := Config{Engine: e.eng, Store: e.st, LocalID: "local", LocalName: "server"}
	if mod != nil {
		mod(&cfg)
	}
	f := New(cfg)
	e.t.Cleanup(f.Close)
	return f
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// serve starts f.ServeNode on one end of a pipe and returns the other end; served closes when ServeNode returns.
func serve(f *Fleet, id string) (nodeEnd, serverEnd *pipeConn, served chan struct{}) {
	nc, sc := pipePair()
	served = make(chan struct{})
	go func() {
		defer close(served)
		f.ServeNode(id, sc)
	}()
	return nc, sc, served
}

// wireNode is a node speaking the wire format by hand: what it sends and when is the test's.
type wireNode struct {
	t       *testing.T
	sess    *link.Session
	cancel  context.CancelFunc
	done    chan struct{}
	welcome chan *athenav1.NodeWelcome
	results chan *athenav1.NodeResult
	calls   chan *athenav1.NodeCall
	conn    *pipeConn // the server's end
	served  chan struct{}

	// order is what came from the server, in order: "blob:<kind>" or a call's method; blob the chunks' data.
	omu   sync.Mutex
	order []string
	blobs []byte
}

func (w *wireNode) received() ([]string, []byte) {
	w.omu.Lock()
	defer w.omu.Unlock()
	return append([]string(nil), w.order...), append([]byte(nil), w.blobs...)
}

func dialWire(t *testing.T, f *Fleet, certID string, hello *athenav1.NodeHello) *wireNode {
	t.Helper()
	nc, sc, served := serve(f, certID)
	ctx, cancel := context.WithCancel(context.Background())
	w := &wireNode{t: t, cancel: cancel, done: make(chan struct{}), conn: sc, served: served,
		welcome: make(chan *athenav1.NodeWelcome, 1), results: make(chan *athenav1.NodeResult, 64),
		calls: make(chan *athenav1.NodeCall, 64)}
	w.sess = link.New(nc, link.Options{Heartbeat: 3 * time.Second, DeadAfter: 9 * time.Second, OnFrame: func(fr *athenav1.Frame) {
		switch b := fr.GetBody().(type) {
		case *athenav1.Frame_NodeWelcome:
			w.welcome <- b.NodeWelcome
		case *athenav1.Frame_NodeResult:
			w.results <- b.NodeResult
		case *athenav1.Frame_NodeCall:
			w.omu.Lock()
			w.order = append(w.order, b.NodeCall.GetMethod())
			w.omu.Unlock()
			w.calls <- b.NodeCall
		case *athenav1.Frame_BlobChunk:
			w.omu.Lock()
			w.order = append(w.order, "blob:"+b.BlobChunk.GetKind())
			w.blobs = append(w.blobs, b.BlobChunk.GetData()...)
			w.omu.Unlock()
		}
	}})
	go func() {
		defer close(w.done)
		_ = w.sess.Run(ctx)
	}()
	t.Cleanup(w.close)
	if err := w.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeHello{NodeHello: hello}}); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *wireNode) close() {
	w.cancel()
	<-w.done
	select {
	case <-w.served:
	case <-time.After(waitTimeout):
		w.t.Error("ServeNode did not return after the node left")
	}
}

func (w *wireNode) waitWelcome() *athenav1.NodeWelcome {
	w.t.Helper()
	select {
	case wl := <-w.welcome:
		return wl
	case <-time.After(waitTimeout):
		w.t.Fatal("no welcome")
		return nil
	}
}

// fact sends a fact as the node's outbox would.
func (w *wireNode) fact(id, seq uint64, method string, args any) {
	w.t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeCall{
		NodeCall: &athenav1.NodeCall{Id: id, Method: method, Args: b, FactSeq: seq}}}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *wireNode) result() *athenav1.NodeResult {
	w.t.Helper()
	select {
	case r := <-w.results:
		return r
	case <-time.After(waitTimeout):
		w.t.Fatal("no result")
		return nil
	}
}

func hello(id string) *athenav1.NodeHello {
	return &athenav1.NodeHello{Protocol: nodelink.Protocol, NodeId: id, Version: "test",
		Caps: &athenav1.Capabilities{Os: "linux", Arch: "arm64", Slots: 2}}
}

// clientNode is a nodelink.Client whose dials reach the fleet over pipes.
type clientNode struct {
	t      *testing.T
	c      *nodelink.Client
	f      *Fleet
	id     string
	mu     sync.Mutex
	conns  []*pipeConn // the server's ends, one per dial
	cancel context.CancelFunc
	done   chan struct{}
}

func newClient(t *testing.T, f *Fleet, id string) *clientNode {
	t.Helper()
	cn := &clientNode{t: t, f: f, id: id}
	c, err := nodelink.New(&nodelink.Identity{NodeID: id, Name: id}, nodelink.Options{
		StateDir:   t.TempDir(),
		Version:    "test",
		MinBackoff: 20 * time.Millisecond,
		MaxBackoff: 50 * time.Millisecond,
		Dial: func(ctx context.Context) (link.Conn, error) {
			nc, sc, _ := serve(cn.f, cn.id)
			cn.mu.Lock()
			cn.conns = append(cn.conns, sc)
			cn.mu.Unlock()
			return nc, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cn.c = c
	t.Cleanup(func() {
		cn.stop()
		c.Close()
	})
	return cn
}

func (cn *clientNode) run() {
	cn.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cn.cancel, cn.done = cancel, make(chan struct{})
	go func() {
		defer close(cn.done)
		_ = cn.c.Run(ctx)
	}()
	eventually(cn.t, "the node is online", cn.c.Online)
}

func (cn *clientNode) stop() {
	if cn.cancel == nil {
		return
	}
	cn.cancel()
	<-cn.done
	cn.cancel = nil
}

func (cn *clientNode) lastConn() *pipeConn {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return cn.conns[len(cn.conns)-1]
}

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
	fakeDir  string
)

// fakeClaude builds the runtime package's fake `claude` once per test binary run.
func fakeClaude(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakeDir, fakeErr = os.MkdirTemp("", "fakeclaude")
		if fakeErr != nil {
			return
		}
		fakePath = filepath.Join(fakeDir, "claude")
		out, err := exec.Command("go", "build", "-o", fakePath,
			"github.com/danielpina1/athena/internal/runtime/claude/testdata/fakeclaude").CombinedOutput()
		if err != nil {
			fakeErr = errors.New("build fakeclaude: " + err.Error() + "\n" + string(out))
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakePath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeDir != "" {
		os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}
