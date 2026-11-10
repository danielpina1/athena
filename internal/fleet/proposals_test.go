package fleet

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// heldPush starts the Ticket's Assignment and proposes its branch as a push held for the owner.
func (e *env) heldPush(tk core.Ticket) core.Proposal {
	e.t.Helper()
	if err := e.eng.AssignmentStarted(tk.ID, tk.LatestAssignment().ID, "s1", "", core.RuntimeClaude); err != nil {
		e.t.Fatal(err)
	}
	p, err := e.eng.Propose(core.Proposal{TicketID: tk.ID, Kind: core.ProposalPush, Summary: "push",
		Payload: json.RawMessage(`{"branch":"athena/x","sha":"0123456789abcdef0123456789abcdef01234567"}`)})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// settleFact applies a node's SettleProposal fact as the fleet would.
func settleFact(f *Fleet, nodeID, id, applyErr string) error {
	b, _ := json.Marshal(nodelink.ArgsSettleProposal{ID: id, Error: applyErr})
	return f.fact(nodeID, &athenav1.NodeCall{Method: nodelink.MethodSettleProposal, Args: b})
}

func startProposal(f *Fleet, nodeID, id string) error {
	_, err := f.answer(context.Background(), nodeID, &nodelink.ArgsStartProposal{ID: id})
	return err
}

func writeToken(f *Fleet, nodeID, repo string) error {
	_, err := f.answer(context.Background(), nodeID, &nodelink.ArgsToken{Repo: repo, Write: true})
	return err
}

func (e *env) proposalState(id string) string {
	e.t.Helper()
	p, err := e.eng.Proposal(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.State
}

// TestSettleProposalFromStartingNode: a push started on one node settles from that node's replayed fact after its
// Ticket moved to another node; the node the Ticket moved to, which never started it, cannot settle it.
func TestSettleProposalFromStartingNode(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	tk := e.placedRepo(nodeID, "o/r")
	p := e.heldPush(tk)
	f := e.fleet(nil)

	if err := startProposal(f, nodeID, p.ID); err != nil {
		t.Fatalf("start on its node: %v", err)
	}
	// The node goes offline mid-apply and the owner retries the Ticket elsewhere.
	if err := e.eng.NodeOffline(nodeID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.RetryElsewhere(tk.ID); err != nil {
		t.Fatal(err)
	}
	moved := e.ticket(tk.ID)
	if err := e.eng.AssignmentPlaced(tk.ID, moved.LatestAssignment().ID, otherID); err != nil {
		t.Fatal(err)
	}

	if err := settleFact(f, otherID, p.ID, ""); err == nil || !strings.Contains(err.Error(), "not on this node") {
		t.Fatalf("settle from a node that never started it = %v, want refused", err)
	}
	if s := e.proposalState(p.ID); s != core.ProposalPending {
		t.Fatalf("state after a refused settle = %s", s)
	}
	if err := settleFact(f, nodeID, p.ID, ""); err != nil {
		t.Fatalf("replayed settle from the starting node: %v", err)
	}
	if s := e.proposalState(p.ID); s != core.ProposalApplied {
		t.Fatalf("state = %s, want applied", s)
	}
}

// TestApplyHeldProposalAfterTicketEnded: the owner applies a held push of a Ticket that ended on a remote node. The
// apply goes to that node, which may start the Proposal, get a write token for its repository and settle it while the
// apply is under way; without the apply (before, after, or as another node) each is refused.
func TestApplyHeldProposalAfterTicketEnded(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	tk := e.placedRepo(nodeID, "o/r")
	p := e.heldPush(tk)
	if err := e.eng.FileReport(tk.ID, core.ReportInput{State: core.ReportReady, Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	if e.ticket(tk.ID).Ended == nil {
		t.Fatal("the ticket did not end")
	}
	f := e.fleet(func(c *Config) { c.Creds = fakeCreds{} })

	refused := func(when string) {
		t.Helper()
		if err := startProposal(f, nodeID, p.ID); err == nil {
			t.Fatalf("%s: start accepted", when)
		}
		if err := writeToken(f, nodeID, "o/r"); err == nil {
			t.Fatalf("%s: token granted", when)
		}
	}
	refused("before the apply")

	w := dialWire(t, f, nodeID, hello(nodeID))
	w.waitWelcome()
	type outcome struct{ start, token, other, otherToken, settle error }
	got := make(chan outcome, 1)
	go func() {
		for {
			var c *athenav1.NodeCall
			select {
			case c = <-w.calls:
			case <-w.done:
				return
			}
			if c.GetMethod() == nodelink.ServeApplyProposal {
				var o outcome
				o.other = startProposal(f, otherID, p.ID)
				o.otherToken = writeToken(f, otherID, "o/r")
				o.start = startProposal(f, nodeID, p.ID)
				o.token = writeToken(f, nodeID, "O/R")
				o.settle = settleFact(f, nodeID, p.ID, "")
				got <- o
			}
			_ = w.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeResult{
				NodeResult: &athenav1.NodeResult{Id: c.GetId()}}})
		}
	}()
	if err := f.ApplyProposal(context.Background(), p); err != nil {
		t.Fatalf("apply: %v", err)
	}
	o := <-got
	if o.start != nil || o.token != nil || o.settle != nil {
		t.Fatalf("during the apply: start %v, token %v, settle %v", o.start, o.token, o.settle)
	}
	if o.other == nil || o.otherToken == nil {
		t.Fatalf("another node during the apply: start %v, token %v; want refused", o.other, o.otherToken)
	}
	if s := e.proposalState(p.ID); s != core.ProposalApplied {
		t.Fatalf("state = %s, want applied", s)
	}
	refused("after the apply")
}

// TestApplyGoesToTheNodeWithTheWork: a Ticket retried elsewhere and placed on a second node that has not started has
// its worktree on the first; applying its held push goes there, and with that node offline (and no worktree here) it
// fails with "<name> is offline".
func TestApplyGoesToTheNodeWithTheWork(t *testing.T) {
	e, f := executorEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	tk := e.placedRepo(nodeID, "o/r")
	p := e.heldPush(tk)
	if err := e.eng.NodeOffline(nodeID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.RetryElsewhere(tk.ID); err != nil {
		t.Fatal(err)
	}
	moved := e.ticket(tk.ID)
	if err := e.eng.AssignmentPlaced(tk.ID, moved.LatestAssignment().ID, otherID); err != nil {
		t.Fatal(err)
	}
	other := dialAnswering(t, f, otherID)

	err := f.ApplyProposal(context.Background(), p)
	if err == nil || err.Error() != "mini is offline" {
		t.Fatalf("apply = %v, want mini is offline", err)
	}
	if other.got(nodelink.ServeApplyProposal) {
		t.Fatal("the apply went to the node without the worktree")
	}
}
