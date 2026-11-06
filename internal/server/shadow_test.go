package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/danielpina1/athena/internal/store"
)

// startRoutedTicket starts a server over the fake GitHub, has the router accept a mention on o/r#3 as T-0001 with an
// acknowledgement, and returns the server, its Dispatcher and the running Assignment. The Ticket's worktree directory
// is made ahead, so no git reaches github.com.
func startRoutedTicket(t *testing.T, g *fakeGitHub, before func(dir string)) (*testServer, *Dispatcher, string, core.Assignment) {
	t.Helper()
	fake := newFakeEnv(t, openTurnFixture(t))
	fastEvaluate(t)
	dir := newStateDir(t)
	g.configure(t, dir, 600)
	if before != nil {
		before(dir)
	}
	if err := os.MkdirAll(node.TicketDir(filepath.Join(dir, "work"), core.Ticket{ID: "T-0001", Title: "Fix the flaky test"}), 0o700); err != nil {
		t.Fatal(err)
	}
	ready := make(chan *Dispatcher, 1)
	s := startServerWith(t, dir, func(c *Config) { c.dispatcherReady = func(d *Dispatcher) { ready <- d } })
	d := <-ready
	token := routerToken(t, fake)
	eventually(t, "router running and github healthy", func() bool {
		h := s.health()
		return h.Router.State == "running" && len(h.Sources) == 1 && h.Sources[0].Error == ""
	})
	cid := g.addComment(3, "o", "@athena-bot the login test is flaky, please fix it")
	triggerID := fmt.Sprintf("github:issuecomment:%d", cid)
	thread := github.ThreadKey(fakeRepo, 3)
	eventually(t, "the candidate reached the router", func() bool {
		for _, th := range s.threads() {
			if th.Key == thread && th.Eval != nil && th.Eval.DeliveredAt != nil {
				return true
			}
		}
		return false
	})
	args, _ := json.Marshal(map[string]any{"decisions": []map[string]any{{"trigger_id": triggerID, "kind": "accept",
		"title": "Fix the flaky test", "ack": "On it: I will fix the flaky login test."}}})
	var rr node.RouterResponse
	nodeCall(t, dir, node.RouterRequest{Op: node.OpRouter, Token: token, Tool: "route", Args: args}, &rr)
	if !rr.OK {
		t.Fatalf("route = %+v %s", rr, rr.Result)
	}
	return s, d, dir, s.running("T-0001")
}

// receiptsOf reads a Ticket's reply receipts and pending Proposals from a stopped server's store.
func receiptsOf(t *testing.T, dir, ticketID string) ([]*core.Receipt, []*core.Proposal) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*core.Receipt
	var ps []*core.Proposal
	if err := st.Tx(func(tx *store.Tx) error {
		if rs, err = tx.TicketReceipts(ticketID); err != nil {
			return err
		}
		ps, err = tx.TicketProposals(ticketID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rs, ps
}

// Review Focus 4: switching a repository to shadow with `athena policy set` revokes the write token a Ticket holds,
// its next token reads only, and the reply waiting to go out (GitHub refused it so far) is held with a Proposal.
func TestShadowSwitchDropsTokenAndHoldsWaitingReply(t *testing.T) {
	g := newFakeGitHub(t)
	g.failComments.Store(true)
	s, _, dir, a := startRoutedTicket(t, g, nil)
	gh := func() string {
		var cr node.CredentialResponse
		nodeCall(t, dir, node.CredentialRequest{Op: node.OpCredential, Ticket: "T-0001", Assignment: a.ID, Purpose: node.CredentialGh}, &cr)
		if !cr.OK || cr.Token == "" {
			t.Fatalf("credential = %+v", cr)
		}
		return cr.Token
	}
	permsOf := func(tok string) map[string]string {
		for _, m := range g.minted() {
			if m.Token == tok {
				return m.Perms
			}
		}
		t.Fatalf("token %s was not minted", tok)
		return nil
	}
	write := gh()
	if p := permsOf(write); p["contents"] != "write" || p["pull_requests"] != "write" {
		t.Fatalf("live token permissions = %v, want write", p)
	}
	eventually(t, "the acknowledgement failed to go out", func() bool {
		return slices.Contains(g.requestLog(), "POST /repos/o/r/issues/3/comments")
	})

	var shown PolicyView
	s.ok(Request{Op: OpPolicyShow}, &shown)
	next := shown.Settings
	next.Repos = map[string]policy.RepoSettings{"o/r": {Mode: policy.RepoShadow}}
	raw, _ := json.Marshal(next)
	var set PolicySetResult
	s.ok(Request{Op: OpPolicySet, Policy: raw}, &set)

	eventually(t, "the write token revoked", func() bool { return slices.Contains(g.revokedTokens(), write) })
	read := gh()
	if p := permsOf(read); read == write || p["contents"] != "read" || p["pull_requests"] != "read" || p["issues"] != "read" {
		t.Fatalf("token after the switch = %v (same: %v), want read-only", p, read == write)
	}
	s.stop()
	if n := len(g.postedComments()); n != 0 {
		t.Fatalf("%d comments posted", n)
	}
	rs, ps := receiptsOf(t, dir, "T-0001")
	if len(rs) != 1 || rs[0].State != core.ReceiptHeld {
		t.Fatalf("receipts = %+v, want the acknowledgement held", rs)
	}
	if len(ps) != 1 || ps[0].Kind != core.ProposalReply || ps[0].State != core.ProposalPending ||
		!strings.Contains(string(ps[0].Payload), rs[0].ID) {
		t.Fatalf("proposals = %+v, want a pending reply Proposal for %s", ps, rs[0].ID)
	}
}

// Applying a pull request whose Ticket's pending push cannot be made (no worktree of the repository on this node)
// fails both with the push's error, opens no pull request, and is not retried.
func TestApplyPullFailedPushNotRetried(t *testing.T) {
	g := newFakeGitHub(t)
	_, d, _, _ := startRoutedTicket(t, g, nil)
	push, err := d.eng.Propose(core.Proposal{TicketID: "T-0001", Kind: core.ProposalPush, Summary: "push",
		Payload: json.RawMessage(`{"branch":"athena/t-0001","sha":"` + strings.Repeat("a", 40) + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	pull, err := d.eng.Propose(core.Proposal{TicketID: "T-0001", Kind: core.ProposalPull, Summary: "pr",
		Payload: json.RawMessage(`{"args":["pr","create","--title","T"],"branch":"athena/t-0001"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.eng.ApplyProposal(pull.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pull request settled", func() bool {
		ps, err := d.eng.PendingProposals()
		if err != nil {
			t.Fatal(err)
		}
		return len(ps) == 0
	})
	got := map[string]core.Proposal{}
	if err := d.st.Tx(func(tx *store.Tx) error {
		for _, id := range []string{push.ID, pull.ID} {
			p, err := tx.GetProposal(id)
			if err != nil {
				return err
			}
			got[id] = *p
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pp, pr := got[push.ID], got[pull.ID]
	if pp.State != core.ProposalFailed || pr.State != core.ProposalFailed || pr.Error != pp.Error || !strings.Contains(pp.Error, "worktree") {
		t.Fatalf("push %s %q, pull %s %q; want both failed with the push's error", pp.State, pp.Error, pr.State, pr.Error)
	}
	time.Sleep(300 * time.Millisecond)
	if fx, err := d.fx.Pending(core.EffectApplyProposal); err != nil || len(fx) != 0 {
		t.Fatalf("apply effects left = %+v %v, want none (not retried)", fx, err)
	}
	for _, r := range g.requestLog() {
		if strings.Contains(r, "/pulls") && strings.HasPrefix(r, "POST") {
			t.Fatalf("a pull request was opened: %v", g.requestLog())
		}
	}
}
