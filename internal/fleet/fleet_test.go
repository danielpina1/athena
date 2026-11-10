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
	e.placedRepo(nodeID, "o/r")
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

// TestTokenScopedToNodesWork: a node gets a token only for a repository its live Assignments work in.
func TestTokenScopedToNodesWork(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	e.placedRepo(nodeID, "o/r")
	e.placedRepo(otherID, "x/y")
	f := e.fleet(func(c *Config) { c.Creds = fakeCreds{} })
	cn := newClient(t, f, nodeID)
	cn.run()
	creds := cn.c.Credentials()
	if tok, _, err := creds.Token(context.Background(), "O/R", true); err != nil || tok != "tok-O/R" {
		t.Fatalf("own repo: %q, %v", tok, err)
	}
	for _, repo := range []string{"x/y", "z/z"} {
		tok, _, err := creds.Token(context.Background(), repo, true)
		if err == nil || tok != "" || !strings.Contains(err.Error(), "this node has no work in "+repo) {
			t.Fatalf("%s: %q, %v, want refused", repo, tok, err)
		}
	}
}

const otherID = "n9876543210"

// TestIDCallsScopedToNode: a node cannot expire, wait on, settle, start or list another node's Approvals and
// Proposals, nor read a Trigger of a Ticket not on it.
func TestIDCallsScopedToNode(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	mine, theirs := e.placed(nodeID), e.placed(otherID)
	theirApproval, err := e.eng.RequestApproval(core.Approval{TicketID: theirs.ID, Tool: "Bash", Summary: "push"})
	if err != nil {
		t.Fatal(err)
	}
	pr := func(tk core.Ticket) core.Proposal {
		p, err := e.eng.Propose(core.Proposal{TicketID: tk.ID, Kind: core.ProposalPull, Summary: "pr"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	myProposal, theirProposal := pr(mine), pr(theirs)
	f := e.fleet(nil)
	cn := newClient(t, f, nodeID)
	cn.run()
	eng := cn.c.Engine()

	// Facts: refused, yet answered (the mark moves).
	if err := eng.ExpireApproval(theirApproval.ID); err != nil {
		t.Fatal(err)
	}
	if err := eng.SettleProposal(theirProposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Requests: refused. Each runs after the facts before it (R12), so the checks below see them applied or refused.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := eng.WaitApproval(ctx, theirApproval.ID); err == nil || !strings.Contains(err.Error(), "not on this node") {
		t.Fatalf("wait = %v", err)
	}
	if _, err := eng.StartProposal(theirProposal.ID); err == nil || !strings.Contains(err.Error(), "not on this node") {
		t.Fatalf("start proposal = %v", err)
	}
	if _, err := eng.Trigger(theirs.Origin.TriggerID); err == nil || !strings.Contains(err.Error(), "not on this node") {
		t.Fatalf("their trigger = %v", err)
	}
	if tr, err := eng.Trigger(mine.Origin.TriggerID); err != nil || tr.ID != mine.Origin.TriggerID {
		t.Fatalf("my trigger = %+v, %v", tr, err)
	}
	ps, err := eng.PendingProposals()
	if err != nil || len(ps) != 1 || ps[0].ID != myProposal.ID {
		t.Fatalf("pending proposals = %+v, %v; want only mine", ps, err)
	}
	if a, _ := e.eng.Approval(theirApproval.ID); a.State != core.ApprovalPending {
		t.Fatalf("their approval = %s, want still pending", a.State)
	}
	if p, _ := e.eng.Proposal(theirProposal.ID); p.State != core.ProposalPending {
		t.Fatalf("their proposal = %s, want still pending", p.State)
	}
	if n, _ := e.st.GetNode(nodeID); n.AppliedFactSeq != 2 {
		t.Fatalf("mark = %d, want 2 (refused facts still move it)", n.AppliedFactSeq)
	}
}

// TestSecondLinkReplacesFirst: the node connects again while facts are in flight on its first link. The first link
// closes with 1000, the second is welcomed with the mark the first reached, no fact applies twice or is lost, and the
// node is online once the first link is gone.
func TestSecondLinkReplacesFirst(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	tk := e.placed(nodeID)
	f := e.fleet(nil)
	const total = 200
	one := func(uint64) nodelink.ArgsRecordUsage {
		return nodelink.ArgsRecordUsage{TicketID: tk.ID, Usage: core.UsageDelta{ToolCalls: 1}}
	}
	w1 := dialWire(t, f, nodeID, hello(nodeID))
	w1.waitWelcome()
	for seq := uint64(1); seq <= total; seq++ {
		w1.fact(seq, seq, nodelink.MethodRecordUsage, one(seq))
	}
	w2 := dialWire(t, f, nodeID, hello(nodeID))
	mark := w2.waitWelcome().GetAppliedFactSeq()
	if code, reason := w1.conn.closedWith(t); code != 1000 || reason != "replaced by a new link" {
		t.Fatalf("first link closed %d %q", code, reason)
	}
	select {
	case <-w1.served:
	case <-time.After(waitTimeout):
		t.Fatal("the first link's ServeNode did not return")
	}
	if got := e.ticket(tk.ID).Stats.ToolCalls; uint64(got) != mark {
		t.Fatalf("tool calls = %d at the welcome, mark %d", got, mark)
	}
	// The second link replays what its outbox still holds: everything above some answered seq, here from 1.
	for seq := uint64(1); seq <= total; seq++ {
		w2.fact(1000+seq, seq, nodelink.MethodRecordUsage, one(seq))
	}
	for seq := uint64(1); seq <= total; seq++ {
		if r := w2.result(); r.GetId() != 1000+seq {
			t.Fatalf("result %d = %v", seq, r)
		}
	}
	if got := e.ticket(tk.ID).Stats.ToolCalls; got != total {
		t.Fatalf("tool calls = %d, want %d: each fact once", got, total)
	}
	if v := view(t, f, nodeID); v.State != StateOnline {
		t.Fatalf("state = %s after the first link left, want online", v.State)
	}
}

// TestReplacementBoundedByStuckApplier: a replaced link whose applier does not stop refuses the new link after
// replaceWait instead of stalling it, and Revoke meanwhile is not held up.
func TestReplacementBoundedByStuckApplier(t *testing.T) {
	old := replaceWait
	replaceWait = 100 * time.Millisecond
	t.Cleanup(func() { replaceWait = old })
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	f := e.fleet(nil)
	// A link that is attached but whose applier never ends (as one stuck in a fact would be).
	_, sc := pipePair() // never run: only the applier's state matters
	stuck := newNodeLink(f, nodeID, sc)
	r := f.Remote(nodeID)
	r.attach(stuck, "mini", hello(nodeID), time.Now())

	start := time.Now()
	w := dialWire(t, f, nodeID, hello(nodeID))
	if code, _ := w.conn.closedWith(t); code != closeTryLater {
		t.Fatalf("new link closed %d, want %d", code, closeTryLater)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("refusal took %s", d)
	}
	if stuck.ctx.Err() == nil {
		t.Fatal("the old link was not ended")
	}
	done := make(chan error, 1)
	go func() { done <- f.Revoke(nodeID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Revoke stalled behind the stuck link")
	}
}

// TestSubGapBeforeClose: lines a slow reader had no room for end as a gap before the feed closes.
func TestSubGapBeforeClose(t *testing.T) {
	s := &remoteSub{ch: make(chan node.Line, 3), gapFrom: -1} // room for two lines and the last gap
	s.push([]node.Line{{Offset: 0, Data: []byte("a")}, {Offset: 2, Data: []byte("b")}, {Offset: 4, Data: []byte("c")},
		{Offset: 6, Data: []byte("dd")}}, true)
	var got []node.Line
	for l := range s.ch {
		got = append(got, l)
	}
	if len(got) != 3 || got[0].Offset != 0 || got[1].Offset != 2 || got[2].Offset != 4 || got[2].GapTo != 9 {
		t.Fatalf("lines = %+v, want a, b, then a gap 4..9", got)
	}
}
