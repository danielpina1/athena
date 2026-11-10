package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
)

const nodeID = "n0123456789"

// usage is a RecordUsage fact that adds 1<<(seq-1) tool calls: the sum says which facts applied.
func usage(ticketID string, seq uint64) nodelink.ArgsRecordUsage {
	return nodelink.ArgsRecordUsage{TicketID: ticketID, Usage: core.UsageDelta{ToolCalls: 1 << (seq - 1)}}
}

// TestFactsHighWaterSurvivesServerRestart: facts 1-3 apply; a new Fleet (and Engine) on the same store welcomes the
// node with mark 3, and of 2-4 sent again only 4 applies. Every fact is answered, in order.
func TestFactsHighWaterSurvivesServerRestart(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	tk := e.placed(nodeID)

	f1 := e.fleet(nil)
	w := dialWire(t, f1, nodeID, hello(nodeID))
	if got := w.waitWelcome(); got.GetAppliedFactSeq() != 0 || got.GetNodeName() != "mini" {
		t.Fatalf("welcome = %v", got)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		w.fact(100+seq, seq, nodelink.MethodRecordUsage, usage(tk.ID, seq))
	}
	for seq := uint64(1); seq <= 3; seq++ {
		if r := w.result(); r.GetId() != 100+seq || r.GetError() != "" {
			t.Fatalf("result %d = %v", seq, r)
		}
	}
	if got := e.ticket(tk.ID).Stats.ToolCalls; got != 7 {
		t.Fatalf("tool calls = %d, want 7 (facts 1-3)", got)
	}
	w.close()
	f1.Close()

	// The server restarts: a new Engine and Fleet on the same store.
	e.eng = core.NewEngine(e.st.Repos(), nopExec{}, nil, nil)
	f2 := e.fleet(nil)
	w = dialWire(t, f2, nodeID, hello(nodeID))
	if got := w.waitWelcome(); got.GetAppliedFactSeq() != 3 {
		t.Fatalf("welcome mark = %d, want 3", got.GetAppliedFactSeq())
	}
	for seq := uint64(2); seq <= 4; seq++ {
		w.fact(200+seq, seq, nodelink.MethodRecordUsage, usage(tk.ID, seq))
	}
	for seq := uint64(2); seq <= 4; seq++ {
		if r := w.result(); r.GetId() != 200+seq || r.GetError() != "" {
			t.Fatalf("result %d = %v", seq, r)
		}
	}
	if got := e.ticket(tk.ID).Stats.ToolCalls; got != 15 {
		t.Fatalf("tool calls = %d, want 15 (only 4 applied again)", got)
	}
	if n, _ := e.st.GetNode(nodeID); n.AppliedFactSeq != 4 {
		t.Fatalf("mark = %d, want 4", n.AppliedFactSeq)
	}
}

// TestRefusedFactAdvancesMark: a fact the Engine refuses is answered with its error and still moves the mark.
func TestRefusedFactAdvancesMark(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	f := e.fleet(nil)
	w := dialWire(t, f, nodeID, hello(nodeID))
	w.waitWelcome()
	w.fact(1, 1, nodelink.MethodTurnEnded, nodelink.ArgsTurnEnded{TicketID: "nope", AssignmentID: "a"})
	if r := w.result(); r.GetId() != 1 || r.GetError() == "" || !r.GetUserError() {
		t.Fatalf("result = %v, want a user error", r)
	}
	if n, _ := e.st.GetNode(nodeID); n.AppliedFactSeq != 1 {
		t.Fatalf("mark = %d, want 1", n.AppliedFactSeq)
	}
}

// TestHelloChecked: a Hello naming another node than its certificate, or another protocol, closes the link.
func TestHelloChecked(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	f := e.fleet(nil)

	w := dialWire(t, f, nodeID, hello("n9999999999"))
	if code, reason := w.conn.closedWith(t); code != link.CloseProtocol || reason != "node id does not match its certificate" {
		t.Fatalf("close = %d %q", code, reason)
	}
	h := hello(nodeID)
	h.Protocol = 2
	w = dialWire(t, f, nodeID, h)
	if code, reason := w.conn.closedWith(t); code != link.CloseProtocol || reason != "node protocol 2, server protocol 1; update the node" {
		t.Fatalf("close = %d %q", code, reason)
	}
}

// TestFactsBeforeRequestsInOrder: a request starts only after the facts the node sent before it applied (R12).
func TestFactsBeforeRequestsInOrder(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	tk := e.placed(nodeID)
	f := e.fleet(nil)
	cn := newClient(t, f, nodeID)
	cn.run()
	eng := cn.c.Engine()
	for i := 1; i <= 20; i++ {
		if err := eng.RecordUsage(tk.ID, "", core.UsageDelta{ToolCalls: 1}); err != nil {
			t.Fatal(err)
		}
		got, _, err := eng.Ticket(tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Stats.ToolCalls != i {
			t.Fatalf("request %d saw %d tool calls: it ran before the fact before it", i, got.Stats.ToolCalls)
		}
	}
}

type fakeCreds struct{}

func (fakeCreds) Token(ctx context.Context, repo string, write bool) (string, time.Time, error) {
	if repo == "gone/repo" {
		return "", time.Time{}, errors.New("permanent: no installation")
	}
	return "tok-" + repo, time.Now().Add(time.Hour), nil
}

func (fakeCreds) Identity() (string, string) { return "athena[bot]", "bot@example.com" }

// TestRequestsRunConcurrently: a WaitApproval held by the server does not hold up a Token.
func TestRequestsRunConcurrently(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	tk := e.placed(nodeID)
	f := e.fleet(func(c *Config) { c.Creds = fakeCreds{} })
	cn := newClient(t, f, nodeID)
	cn.run()
	eng := cn.c.Engine()
	ap, err := eng.RequestApproval(core.Approval{TicketID: tk.ID, Tool: "Bash", Summary: "push"})
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan core.Approval, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		a, err := eng.WaitApproval(ctx, ap.ID)
		if err != nil {
			t.Error(err)
		}
		waited <- a
	}()
	time.Sleep(100 * time.Millisecond) // the wait is on the server
	start := time.Now()
	tok, _, err := cn.c.Credentials().Token(context.Background(), "o/r", true)
	if err != nil || tok != "tok-o/r" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("token took %s behind the wait", d)
	}
	select {
	case a := <-waited:
		t.Fatalf("wait returned early: %+v", a)
	default:
	}
	if err := e.eng.AnswerApproval(ap.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-waited:
		if a.State != core.ApprovalApproved {
			t.Fatalf("approval = %+v", a)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the wait did not end with the answer")
	}
}

// TestRevokeClosesLiveLink: Revoke closes the node's link with 4003 and Nodes shows it revoked.
func TestRevokeClosesLiveLink(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	f := e.fleet(nil)
	changes := make(chan struct{}, 64)
	f.OnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	cn := newClient(t, f, nodeID)
	cn.run()
	if v := view(t, f, nodeID); v.State != StateOnline || v.Name != "mini" || v.Local {
		t.Fatalf("view = %+v", v)
	}
	if err := f.Revoke(nodeID); err != nil {
		t.Fatal(err)
	}
	if code, reason := cn.lastConn().closedWith(t); code != CloseRevoked || reason != "this node was revoked" {
		t.Fatalf("close = %d %q", code, reason)
	}
	if v := view(t, f, nodeID); v.State != StateRevoked {
		t.Fatalf("state = %s, want revoked", v.State)
	}
	select {
	case <-changes:
	default:
		t.Fatal("OnChange was not called")
	}
}

func view(t *testing.T, f *Fleet, id string) NodeView {
	t.Helper()
	for _, v := range f.Nodes() {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no node %s in %+v", id, f.Nodes())
	return NodeView{}
}

// TestReconcileStartsAndStops: on connect the node is started on the Ticket placed on it; after a reconnect, the
// session of a Ticket that ended while the node was away is stopped.
func TestReconcileStartsAndStops(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	tk := e.placed(nodeID)
	f := e.fleet(nil)

	t.Setenv("ATHENA_FAKE_FIXTURE", filepath.Join("..", "runtime", "claude", "testdata", "claude", "simple-turn.jsonl"))
	if abs, err := filepath.Abs(filepath.Join("..", "runtime", "claude", "testdata", "claude", "simple-turn.jsonl")); err == nil {
		t.Setenv("ATHENA_FAKE_FIXTURE", abs)
	}
	cn := newClient(t, f, nodeID)
	state := t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "athf") // Unix socket paths are short on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	n := node.New(node.Config{
		WorkRoot: filepath.Join(state, "work"), StateDir: state, AthenaBin: "/usr/bin/true",
		SocketPath:   filepath.Join(sockDir, "n.sock"),
		ClaudeBinary: fakeClaude(t), Name: "mini",
		Credentials: cn.c.Credentials(), Policy: cn.c.Policy(),
		WorkerPrompt: cn.c.WorkerPrompt, ReviewerPrompt: cn.c.ReviewerPrompt, OnStatus: cn.c.StatusChanged,
	}, cn.c.Engine())
	sctx, scancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = n.Serve(sctx)
	}()
	t.Cleanup(func() {
		n.Close()
		scancel()
		<-served
	})
	cn.c.Attach(n)
	cn.run()

	eventually(t, "the Assignment runs on the node", func() bool {
		got := e.ticket(tk.ID)
		a := got.LatestAssignment()
		return a.State == core.AssignmentRunning && a.SessionID != ""
	})
	eventually(t, "the node's session is live", func() bool { return slices.Contains(n.Status().LiveTicketIDs, tk.ID) })
	eventually(t, "the fleet sees it live", func() bool {
		return slices.Contains(view(t, f, nodeID).Status.LiveTicketIDs, tk.ID)
	})

	cn.stop()
	eventually(t, "the node is offline", func() bool { return view(t, f, nodeID).State == StateOffline })
	// Ended while the node was away: the Stop never reached it.
	if err := e.eng.StopTicket(tk.ID, "not needed"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(n.Status().LiveTicketIDs, tk.ID) {
		t.Fatal("the session ended without a Stop")
	}
	cn.run()
	eventually(t, "reconcile stops the ended Ticket's session", func() bool {
		return !slices.Contains(n.Status().LiveTicketIDs, tk.ID)
	})
	// A call that needs an answer comes back with the node's own error.
	if err := f.Target(nodeID).Interrupt(tk.ID); !errors.Is(err, node.ErrNoSession) {
		t.Fatalf("interrupt = %v, want ErrNoSession", err)
	}
}

// TestRemoteOffline: an offline Remote drops Start and refuses the calls that need an answer with "<name> is offline".
func TestRemoteOffline(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	f := e.fleet(nil)
	tg := f.Target(nodeID)
	tg.Start(core.Ticket{ID: "t"}, core.Assignment{ID: "a"})
	err := tg.Interrupt("t")
	if !errors.Is(err, ErrNodeOffline) || !strings.Contains(err.Error(), "mini is offline") {
		t.Fatalf("interrupt = %v", err)
	}
	if f.Target("local") != Target(nil) {
		t.Fatal("no local node configured, yet a local target")
	}
}
