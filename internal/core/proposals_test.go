package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// shadowed gives a sourced Ticket the repository o/r and puts that repository in shadow mode when shadow is true.
func (h *evals) shadowed(id string, shadow bool) string {
	h.t.Helper()
	tid := h.sourced(id)
	h.must(h.repos.Tx(func(tx Tx) error {
		t, err := tx.GetTicket(tid)
		if err != nil {
			return err
		}
		t.Repo = "o/r"
		return tx.PutTicket(t)
	}))
	h.pol.shadow = map[string]bool{"o/r": shadow}
	return tid
}

func (h *evals) proposals(ticketID string) []*Proposal {
	h.t.Helper()
	var out []*Proposal
	h.must(h.repos.Tx(func(tx Tx) error {
		var err error
		out, err = tx.TicketProposals(ticketID)
		return err
	}))
	return out
}

func (h *evals) applyEffects() []fakeEffect {
	var out []fakeEffect
	for _, e := range h.repos.effectList() {
		if e.Kind == EffectApplyProposal {
			out = append(out, e)
		}
	}
	return out
}

func TestShadowReplyIsHeld(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", true)
	long := strings.Repeat("x", 300)
	id := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: long})
	eq(t, "receipt held", h.receipt(id).State, ReceiptHeld)
	eq(t, "no reply effect", len(h.replyEffects()), 0)
	ps := h.proposals(a)
	eq(t, "one proposal", len(ps), 1)
	eq(t, "kind", ps[0].Kind, ProposalReply)
	eq(t, "state", ps[0].State, ProposalPending)
	eq(t, "repo", ps[0].Repo, "o/r")
	eq(t, "summary is the first 140 characters", ps[0].Summary, long[:140])
	var pl struct {
		ReceiptID string `json:"receiptId"`
	}
	h.must(json.Unmarshal(ps[0].Payload, &pl))
	eq(t, "payload names the receipt", pl.ReceiptID, id)
	eq(t, "stage", TicketStage(h.ticket(a)), Stage{"person", "proposal"})
	// The same reply again holds nothing more.
	h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: long})
	eq(t, "still one proposal", len(h.proposals(a)), 1)
}

func TestApplyReplyReleasesReceipt(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", true)
	id := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: "Done."})
	p := h.proposals(a)[0]
	out, err := h.eng.ApplyProposal(p.ID)
	h.must(err)
	eq(t, "returned state", out.State, ProposalApplied)
	eq(t, "receipt waiting", h.receipt(id).State, ReceiptWaiting)
	fx := h.replyEffects()
	eq(t, "reply enqueued", len(fx), 1)
	eq(t, "key", fx[0].Key, id)
	eq(t, "proposal applied", h.proposals(a)[0].State, ProposalApplied)
	eq(t, "stage leaves person", TicketStage(h.ticket(a)).Action != "proposal", true)
	_, err = h.eng.ApplyProposal(p.ID)
	var ue *UserError
	eq(t, "second apply is a UserError", errors.As(err, &ue), true)
}

func TestDiscardReplyDropsReceipt(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", true)
	id := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: "Done."})
	p := h.proposals(a)[0]
	h.must(h.eng.DiscardProposal(p.ID))
	r := h.receipt(id)
	eq(t, "receipt dropped", r.State, ReceiptDropped)
	eq(t, "reason", r.DroppedReason, "discarded by the owner")
	eq(t, "dropped at", r.DroppedAt != nil, true)
	eq(t, "proposal discarded", h.proposals(a)[0].State, ProposalDiscarded)
	eq(t, "stage leaves person", TicketStage(h.ticket(a)).Action != "proposal", true)
	eq(t, "nothing sent", len(h.replyEffects()), 0)
	var ue *UserError
	eq(t, "discard again is a UserError", errors.As(h.eng.DiscardProposal(p.ID), &ue), true)
}

func TestPushProposalSupersedes(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", false)
	first, err := h.eng.Propose(Proposal{TicketID: a, Kind: ProposalPush, Summary: "push 1", Payload: json.RawMessage(`{"branch":"b","sha":"1"}`)})
	h.must(err)
	second, err := h.eng.Propose(Proposal{TicketID: a, Kind: ProposalPush, Summary: "push 2", Payload: json.RawMessage(`{"branch":"b","sha":"2"}`)})
	h.must(err)
	eq(t, "pending", second.State, ProposalPending)
	eq(t, "repo from the ticket", second.Repo, "o/r")
	states := map[string]string{}
	for _, p := range h.proposals(a) {
		states[p.ID] = p.State
	}
	eq(t, "states", states, map[string]string{first.ID: ProposalSuperseded, second.ID: ProposalPending})
	eq(t, "stage", TicketStage(h.ticket(a)), Stage{"person", "proposal"})
	pend, err := h.eng.PendingProposals()
	h.must(err)
	eq(t, "pending list", len(pend), 1)
	_, err = h.eng.Propose(Proposal{TicketID: a, Kind: "bogus", Summary: "x"})
	var ue *UserError
	eq(t, "unknown kind", errors.As(err, &ue), true)
}

func TestApplyPushEnqueuesEffect(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", false)
	p, err := h.eng.Propose(Proposal{TicketID: a, Kind: ProposalPush, Summary: "push", Payload: json.RawMessage(`{"branch":"b","sha":"1"}`)})
	h.must(err)
	out, err := h.eng.ApplyProposal(p.ID)
	h.must(err)
	eq(t, "stays pending", out.State, ProposalPending)
	fx := h.applyEffects()
	eq(t, "one effect", len(fx), 1)
	eq(t, "key is the proposal id", fx[0].Key, p.ID)
	eq(t, "still pending until settled", h.proposals(a)[0].State, ProposalPending)

	h.must(h.eng.SettleProposal(p.ID, nil))
	eq(t, "applied", h.proposals(a)[0].State, ProposalApplied)
	eq(t, "stage leaves person", TicketStage(h.ticket(a)).Action != "proposal", true)

	q, err := h.eng.Propose(Proposal{TicketID: a, Kind: ProposalPull, Summary: "pr", Payload: json.RawMessage(`{"args":["a"],"branch":"b"}`)})
	h.must(err)
	h.must(h.eng.SettleProposal(q.ID, errors.New("x")))
	var got *Proposal
	for _, x := range h.proposals(a) {
		if x.ID == q.ID {
			got = x
		}
	}
	eq(t, "failed", [2]string{got.State, got.Error}, [2]string{ProposalFailed, "x"})
	eq(t, "decided", got.DecidedAt != nil, true)
}

func TestLiveReplyUnchanged(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", false)
	id := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: "Done."})
	eq(t, "waiting", h.receipt(id).State, ReceiptWaiting)
	eq(t, "enqueued", len(h.replyEffects()), 1)
	eq(t, "no proposals", len(h.proposals(a)), 0)
}

// A repository switched to shadow holds the replies still waiting to go out: each becomes held with a reply Proposal,
// and a switch of another repository holds nothing.
func TestHoldWaitingHoldsRepoReplies(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", false)
	id := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: "Done."})
	h.must(h.eng.HoldWaiting("o/s"))
	eq(t, "another repository's switch leaves it waiting", h.receipt(id).State, ReceiptWaiting)
	eq(t, "no proposal yet", len(h.proposals(a)), 0)

	h.pol.shadow = map[string]bool{"o/r": true}
	h.must(h.eng.HoldWaiting("O/R"))
	eq(t, "held", h.receipt(id).State, ReceiptHeld)
	ps := h.proposals(a)
	eq(t, "one proposal", len(ps), 1)
	eq(t, "kind", ps[0].Kind, ProposalReply)
	var pl struct {
		ReceiptID string `json:"receiptId"`
	}
	h.must(json.Unmarshal(ps[0].Payload, &pl))
	eq(t, "payload names the receipt", pl.ReceiptID, id)
	eq(t, "stage", TicketStage(h.ticket(a)), Stage{"person", "proposal"})

	// Again: nothing more is held.
	h.must(h.eng.HoldWaiting("o/r"))
	eq(t, "still one proposal", len(h.proposals(a)), 1)
	// Applied, the reply waits to go out again.
	_, err := h.eng.ApplyProposal(ps[0].ID)
	h.must(err)
	eq(t, "released", h.receipt(id).State, ReceiptWaiting)
}
