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

// StartProposal marks a push being applied once: a second start (a run after one that never settled) is not first.
func TestStartProposalOnce(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", true)
	p, err := h.eng.Propose(Proposal{TicketID: a, Kind: ProposalPush, Summary: "push", Payload: json.RawMessage(`{"branch":"b","sha":"1"}`)})
	h.must(err)
	first, err := h.eng.StartProposal(p.ID)
	h.must(err)
	eq(t, "first start", first, true)
	eq(t, "started at", h.proposals(a)[0].StartedAt != nil, true)
	again, err := h.eng.StartProposal(p.ID)
	h.must(err)
	eq(t, "second start", again, false)
	h.must(h.eng.SettleProposal(p.ID, errors.New("x")))
	_, err = h.eng.StartProposal(p.ID)
	var ue *UserError
	eq(t, "settled: a UserError", errors.As(err, &ue), true)
}

// failedPull proposes a pull request, starts and fails it, as the node does when GitHub refuses it.
func (h *evals) failedPull(ticketID string) Proposal {
	h.t.Helper()
	p, err := h.eng.Propose(Proposal{TicketID: ticketID, Kind: ProposalPull, Summary: "pr", Payload: json.RawMessage(`{"title":"t"}`)})
	h.must(err)
	_, err = h.eng.StartProposal(p.ID)
	h.must(err)
	h.must(h.eng.SettleProposal(p.ID, errors.New("GitHub said no")))
	return p
}

func (h *evals) proposal(ticketID, id string) *Proposal {
	h.t.Helper()
	for _, p := range h.proposals(ticketID) {
		if p.ID == id {
			return p
		}
	}
	h.t.Fatalf("proposal %s not found", id)
	return nil
}

// The owner's "Try again" on a failed Proposal: it is pending again, its start and error forgotten, and the node is
// told to apply it once more.
func TestApplyFailedProposalTriesAgain(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", true)
	p := h.failedPull(a)
	eq(t, "no longer waiting on the owner", TicketStage(h.ticket(a)).Action != "proposal", true)
	before := len(h.applyEffects())

	out, err := h.eng.ApplyProposal(p.ID)
	h.must(err)
	got := h.proposal(a, p.ID)
	eq(t, "returned state", out.State, ProposalPending)
	eq(t, "state", got.State, ProposalPending)
	eq(t, "start forgotten", got.StartedAt == nil, true)
	eq(t, "error cleared", got.Error, "")
	eq(t, "undecided", got.DecidedAt == nil, true)
	eq(t, "enqueued again", len(h.applyEffects()), before+1)
	eq(t, "waits on the owner again", TicketStage(h.ticket(a)), Stage{"person", "proposal"})
	first, err := h.eng.StartProposal(p.ID)
	h.must(err)
	eq(t, "the retry starts afresh", first, true)
}

// A failed Proposal can be discarded.
func TestDiscardFailedProposal(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", true)
	p := h.failedPull(a)
	h.must(h.eng.DiscardProposal(p.ID))
	eq(t, "discarded", h.proposal(a, p.ID).State, ProposalDiscarded)
}

// A reply Proposal whose Receipt is no longer held (gone, or sent some other way) can still be discarded.
func TestDiscardReplyWithoutHeldReceipt(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", true)
	id := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: "Done."})
	p := h.proposals(a)[0]
	h.must(h.repos.Tx(func(tx Tx) error {
		rc, err := tx.GetReceipt(id)
		if err != nil {
			return err
		}
		rc.State = ReceiptSent
		return tx.PutReceipt(rc)
	}))
	h.must(h.eng.DiscardProposal(p.ID))
	eq(t, "discarded", h.proposal(a, p.ID).State, ProposalDiscarded)
	eq(t, "the receipt is left as it was", h.receipt(id).State, ReceiptSent)
}

// Once the node began to apply a Proposal, discarding it is refused: the push or pull request may already be on GitHub.
// Before the start a discard still works.
func TestDiscardWhileApplyingRefused(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", true)
	p, err := h.eng.Propose(Proposal{TicketID: a, Kind: ProposalPush, Summary: "push", Payload: json.RawMessage(`{"branch":"b","sha":"1"}`)})
	h.must(err)
	_, err = h.eng.ApplyProposal(p.ID)
	h.must(err)
	_, err = h.eng.StartProposal(p.ID)
	h.must(err)
	err = h.eng.DiscardProposal(p.ID)
	var ue *UserError
	eq(t, "refused as a UserError", errors.As(err, &ue), true)
	eq(t, "says why", err != nil && strings.Contains(err.Error(), "Athena is applying this now"), true)
	eq(t, "still pending", h.proposal(a, p.ID).State, ProposalPending)

	q, err := h.eng.Propose(Proposal{TicketID: a, Kind: ProposalPull, Summary: "pr", Payload: json.RawMessage(`{"title":"t"}`)})
	h.must(err)
	_, err = h.eng.ApplyProposal(q.ID)
	h.must(err)
	h.must(h.eng.DiscardProposal(q.ID))
	eq(t, "discarded before its start", h.proposal(a, q.ID).State, ProposalDiscarded)
}

// A newer push supersedes a failed one too: the owner is not left with an old failed push beside the new one.
func TestPushSupersedesFailedPush(t *testing.T) {
	h := newEvals(t)
	a := h.shadowed("c:1", true)
	old, err := h.eng.Propose(Proposal{TicketID: a, Kind: ProposalPush, Summary: "push 1", Payload: json.RawMessage(`{"branch":"b","sha":"1"}`)})
	h.must(err)
	h.must(h.eng.SettleProposal(old.ID, errors.New("x")))
	_, err = h.eng.Propose(Proposal{TicketID: a, Kind: ProposalPush, Summary: "push 2", Payload: json.RawMessage(`{"branch":"b","sha":"2"}`)})
	h.must(err)
	eq(t, "the failed push", h.proposal(a, old.ID).State, ProposalSuperseded)
}

// A Ticket whose sender's class runs restricted is held for the owner whatever its repository's mode (R15): its reply
// waits as a Proposal in a live repository too.
func TestRestrictedReplyIsHeldInLiveRepository(t *testing.T) {
	h := newEvals(t)
	h.pol.trust = TrustStranger
	h.pol.restricted = map[string]bool{TrustStranger: true}
	a := h.shadowed("c:1", false)
	id := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: "Done."})
	eq(t, "receipt held", h.receipt(id).State, ReceiptHeld)
	eq(t, "nothing queued", len(h.replyEffects()), 0)
	ps := h.proposals(a)
	eq(t, "one reply proposal", len(ps) == 1 && ps[0].Kind == ProposalReply, true)
	eq(t, "stage", TicketStage(h.ticket(a)), Stage{"person", "proposal"})
}
