package core

import (
	"encoding/json"
	"errors"
	"slices"
	"time"
	"unicode/utf8"
)

// Proposals: work that waits for the owner to say yes before it leaves the machine. A reply in a repository held for
// the owner, a push of a branch and a pull request each become a Proposal; the owner applies or discards it.

// Kinds of a Proposal.
const (
	// ProposalReply: a reply held in a shadow repository; its payload names the held Receipt.
	ProposalReply = "reply"
	// ProposalPush: a branch push; its payload names the branch and the commit.
	ProposalPush = "push"
	// ProposalPull: a pull request; its payload carries the `gh` arguments and the branch.
	ProposalPull = "pull_request"
)

// States of a Proposal.
const (
	ProposalPending    = "pending"
	ProposalApplied    = "applied"
	ProposalDiscarded  = "discarded"
	ProposalFailed     = "failed"
	ProposalSuperseded = "superseded"
)

// ReceiptHeld is the state of a Receipt a Proposal holds back: not sent, not queued.
const ReceiptHeld = "held"

// EffectApplyProposal is the effect kind that makes a push or pull-request Proposal happen; its key is the Proposal's ID.
const EffectApplyProposal = "apply_proposal"

// dropDiscarded is the reason of a held Receipt the owner discarded.
const dropDiscarded = "discarded by the owner"

// summaryMaxRunes bounds a held reply's Summary.
const summaryMaxRunes = 140

// Proposal is a step held for the owner.
type Proposal struct {
	ID       string `json:"id"`
	TicketID string `json:"ticketId"`
	Repo     string `json:"repo,omitempty"`
	// Kind is reply, push or pull_request.
	Kind string `json:"kind"`
	// State is pending until the owner decides, then applied, discarded or failed; superseded when a newer push
	// replaced it.
	State   string `json:"state"`
	Summary string `json:"summary"`
	// Payload: reply {"receiptId"}; push {"branch","sha"}; pull_request {"args":[...],"branch"}.
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
	DecidedAt *time.Time      `json:"decidedAt,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// Propose records a pending Proposal for an open Ticket and shows the Ticket as waiting for the owner. A pending push
// of the same Ticket is superseded by a newer one.
func (e *Engine) Propose(p Proposal) (Proposal, error) {
	var out Proposal
	err := e.command(cmdPropose, func(o *op) error {
		t, err := o.require(p.TicketID)
		if err != nil {
			return err
		}
		switch p.Kind {
		case ProposalReply, ProposalPush, ProposalPull:
		default:
			return userErrorf("unknown proposal kind %q (reply, push or pull_request)", p.Kind)
		}
		if t.Ended != nil {
			return userErrorf("ticket %s has ended", t.ID)
		}
		p.Payload = slices.Clone(p.Payload)
		out, err = o.propose(t, p)
		return err
	})
	if err != nil {
		return Proposal{}, err
	}
	return out, nil
}

func (o *op) propose(t *Ticket, p Proposal) (Proposal, error) {
	p.ID = newID()
	p.State = ProposalPending
	p.CreatedAt = o.at
	p.DecidedAt = nil
	p.Error = ""
	p.Repo = t.Repo
	if p.Kind == ProposalPush {
		older, err := o.tx.TicketProposals(t.ID)
		if err != nil {
			return Proposal{}, err
		}
		for _, x := range older {
			if x.Kind == ProposalPush && x.State == ProposalPending {
				if err := o.closeProposal(x, ProposalSuperseded, ""); err != nil {
					return Proposal{}, err
				}
			}
		}
	}
	if err := o.tx.PutProposal(&p); err != nil {
		return Proposal{}, err
	}
	o.proposalEvent(EventProposalCreated, &p, map[string]any{"kind": p.Kind, "summary": p.Summary, "repo": p.Repo})
	return p, o.syncProposals(t.ID)
}

// ApplyProposal does what the owner approved. A reply releases its held Receipt to the Outbox and is applied; a push
// or pull request stays pending while the node makes it happen (EffectApplyProposal) until SettleProposal reports the
// result. A Proposal that is not pending is a UserError.
func (e *Engine) ApplyProposal(id string) (Proposal, error) {
	var out Proposal
	err := e.command(cmdApplyProposal, func(o *op) error {
		p, err := o.pendingProposal(id)
		if err != nil {
			return err
		}
		if p.Kind != ProposalReply {
			if err := o.enqueue(EffectApplyProposal, p.ID, nil); err != nil {
				return err
			}
			out = *p
			return nil
		}
		rc, err := o.heldReceipt(p)
		if err != nil {
			return err
		}
		rc.State = ReceiptWaiting
		if err := o.tx.PutReceipt(rc); err != nil {
			return err
		}
		payload, err := json.Marshal(struct {
			ReceiptID string `json:"receiptId"`
		}{rc.ID})
		if err != nil {
			return err
		}
		if err := o.enqueue(EffectReply, rc.ID, payload); err != nil {
			return err
		}
		if err := o.closeProposal(p, ProposalApplied, ""); err != nil {
			return err
		}
		out = *p
		return o.syncProposals(p.TicketID)
	})
	if err != nil {
		return Proposal{}, err
	}
	return out, nil
}

// DiscardProposal is the owner's no. A held reply is dropped, never sent.
func (e *Engine) DiscardProposal(id string) error {
	return e.command(cmdDiscardProposal, func(o *op) error {
		p, err := o.pendingProposal(id)
		if err != nil {
			return err
		}
		if p.Kind == ProposalReply {
			rc, err := o.heldReceipt(p)
			if err != nil {
				return err
			}
			rc.State, rc.DroppedReason, rc.DroppedAt = ReceiptDropped, dropDiscarded, o.timestamp()
			if err := o.tx.PutReceipt(rc); err != nil {
				return err
			}
		}
		if err := o.closeProposal(p, ProposalDiscarded, ""); err != nil {
			return err
		}
		return o.syncProposals(p.TicketID)
	})
}

// SettleProposal records the result the node reports for a push or pull request it was told to apply: applied when err
// is nil, else failed with the error's text. Settling one that is not pending changes nothing.
func (e *Engine) SettleProposal(id string, applyErr error) error {
	return e.command(cmdSettleProposal, func(o *op) error {
		p, err := o.tx.GetProposal(id)
		if err != nil {
			return err
		}
		if p == nil {
			return userErrorf("proposal %s not found", id)
		}
		if p.State != ProposalPending {
			return nil
		}
		state, msg := ProposalApplied, ""
		if applyErr != nil {
			state, msg = ProposalFailed, applyErr.Error()
		}
		if err := o.closeProposal(p, state, msg); err != nil {
			return err
		}
		return o.syncProposals(p.TicketID)
	})
}

// PendingProposals returns the Proposals waiting for the owner, oldest first.
func (e *Engine) PendingProposals() ([]Proposal, error) {
	var list []*Proposal
	err := e.repos.Tx(func(tx Tx) error {
		var err error
		list, err = tx.PendingProposals()
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]Proposal, 0, len(list))
	for _, p := range list {
		out = append(out, *p)
	}
	return out, nil
}

func (o *op) pendingProposal(id string) (*Proposal, error) {
	p, err := o.tx.GetProposal(id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, userErrorf("proposal %s not found", id)
	}
	if p.State != ProposalPending {
		return nil, userErrorf("proposal %s is already %s", id, p.State)
	}
	return p, nil
}

// heldReceipt loads the held Receipt a reply Proposal's payload names.
func (o *op) heldReceipt(p *Proposal) (*Receipt, error) {
	var pl struct {
		ReceiptID string `json:"receiptId"`
	}
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		return nil, err
	}
	rc, err := o.tx.GetReceipt(pl.ReceiptID)
	if err != nil {
		return nil, err
	}
	if rc == nil || rc.State != ReceiptHeld {
		return nil, errors.New("proposal " + p.ID + " names a receipt that is not held")
	}
	return rc, nil
}

// closeProposal closes a Proposal in state.
func (o *op) closeProposal(p *Proposal, state, msg string) error {
	p.State, p.Error, p.DecidedAt = state, msg, o.timestamp()
	if err := o.tx.PutProposal(p); err != nil {
		return err
	}
	data := map[string]any{"state": state, "kind": p.Kind}
	if msg != "" {
		data["error"] = msg
	}
	o.proposalEvent(EventProposalSettled, p, data)
	return nil
}

// syncProposals sets the Ticket's count of Proposals waiting for the owner.
func (o *op) syncProposals(ticketID string) error {
	t, err := o.tx.GetTicket(ticketID)
	if err != nil || t == nil {
		return err
	}
	all, err := o.tx.TicketProposals(ticketID)
	if err != nil {
		return err
	}
	n := 0
	for _, p := range all {
		if p.State == ProposalPending {
			n++
		}
	}
	if t.PendingProposals == n {
		return nil
	}
	t.PendingProposals = n
	t.UpdatedAt = o.at
	return o.tx.PutTicket(t)
}

func (o *op) proposalEvent(typ string, p *Proposal, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["proposalId"] = p.ID
	o.emitEvent(Event{Type: typ, TicketID: p.TicketID, At: o.at, Data: data})
}

// shadowRepo reports whether the Ticket's repository is held for the owner.
func (o *op) shadowRepo(t *Ticket) bool {
	if o.policy == nil || t.Repo == "" {
		return false
	}
	pol := o.policy()
	return pol != nil && pol.Shadow(t.Repo)
}

// holdReply makes a reply Proposal for a held Receipt.
func (o *op) holdReply(t *Ticket, rc *Receipt) error {
	payload, err := json.Marshal(struct {
		ReceiptID string `json:"receiptId"`
	}{rc.ID})
	if err != nil {
		return err
	}
	summary := rc.Text
	if utf8.RuneCountInString(summary) > summaryMaxRunes {
		summary = string([]rune(summary)[:summaryMaxRunes])
	}
	_, err = o.propose(t, Proposal{TicketID: t.ID, Kind: ProposalReply, Summary: summary, Payload: payload})
	return err
}
