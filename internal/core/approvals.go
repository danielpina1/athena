package core

import (
	"context"
	"encoding/json"
	"slices"
	"time"
)

// Approval states.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
)

// Approval is a risky action a hook on a running session escalated for a person to answer. Its Ticket shows the
// person stage "approval" while it is pending.
type Approval struct {
	ID           string `json:"id"`
	TicketID     string `json:"ticketId"`
	AssignmentID string `json:"assignmentId,omitempty"`
	// Tool names the tool the session wants to use; Summary says what it would do and Rule which rule escalated it.
	Tool    string `json:"tool,omitempty"`
	Summary string `json:"summary,omitempty"`
	Rule    string `json:"rule,omitempty"`
	// Repo is the repository (owner/name) of the Ticket the action is for, or empty; with Tool and Rule it is the
	// action class a standing allow rule names.
	Repo string `json:"repo,omitempty"`
	// Input is the tool's input as the session gave it.
	Input json.RawMessage `json:"input,omitempty"`
	// State is pending until a person answers it (approved or denied) or it expires.
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"createdAt"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`
	// Answer is the person's note.
	Answer string `json:"answer,omitempty"`
}

// reasonTicketEnded is the note of an Approval that expired because its Ticket ended.
const reasonTicketEnded = "ticket ended"

// RequestApproval records a pending Approval for an open Ticket and shows the Ticket as waiting for a person. The
// Engine assigns the ID, the pending state and the creation time. While an earlier Approval of the Ticket is pending,
// that one stays the Ticket's PendingApproval.
func (e *Engine) RequestApproval(a Approval) (Approval, error) {
	var out Approval
	err := e.command(cmdRequestApproval, func(o *op) error {
		t, err := o.require(a.TicketID)
		if err != nil {
			return err
		}
		if t.Ended != nil {
			return userErrorf("ticket %s has ended", a.TicketID)
		}
		a.ID = newID()
		a.Repo = t.Repo
		a.State = ApprovalPending
		a.CreatedAt = o.at
		a.AnsweredAt = nil
		a.Answer = ""
		a.Input = slices.Clone(a.Input)
		if t.PendingApproval == "" {
			t.PendingApproval = a.ID
		}
		t.Stats.Escalations++
		t.UpdatedAt = o.at
		if err := o.tx.PutApproval(&a); err != nil {
			return err
		}
		if err := o.tx.PutTicket(t); err != nil {
			return err
		}
		o.approvalEvent(EventApprovalRequested, &a, map[string]any{"tool": a.Tool, "summary": a.Summary, "rule": a.Rule})
		out = a
		return nil
	})
	if err != nil {
		return Approval{}, err
	}
	out.Input = slices.Clone(out.Input)
	return out, nil
}

// AnswerApproval is a person's answer to a pending Approval, with an optional note for the session. Answering one
// that is not pending is a UserError.
func (e *Engine) AnswerApproval(id string, approve bool, note string) error {
	state := ApprovalDenied
	if approve {
		state = ApprovalApproved
	}
	return e.command(cmdAnswerApproval, func(o *op) error {
		a, err := o.requireApproval(id)
		if err != nil {
			return err
		}
		if a.State != ApprovalPending {
			return userErrorf("approval %s is already %s", id, a.State)
		}
		return o.settleApproval(a, state, note)
	})
}

// ExpireApproval closes a pending Approval nobody answered in time. Expiring one that is no longer pending changes
// nothing, so an expiry that races an answer is harmless.
func (e *Engine) ExpireApproval(id string) error {
	return e.command(cmdExpireApproval, func(o *op) error {
		a, err := o.requireApproval(id)
		if err != nil || a.State != ApprovalPending {
			return err
		}
		return o.settleApproval(a, ApprovalExpired, "")
	})
}

// PendingApprovals returns the Approvals waiting for a person, oldest first.
func (e *Engine) PendingApprovals() ([]Approval, error) {
	var list []*Approval
	err := e.repos.Tx(func(tx Tx) error {
		var err error
		list, err = tx.PendingApprovals()
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]Approval, 0, len(list))
	for _, a := range list {
		out = append(out, *a)
	}
	return out, nil
}

// WaitApproval blocks until the Approval is answered or expired and returns it, or until ctx is done. It does not hold
// the Engine, so commands (the answer among them) run while it waits.
func (e *Engine) WaitApproval(ctx context.Context, id string) (Approval, error) {
	a, err := e.approval(id)
	if err != nil || a.State != ApprovalPending {
		return a, err
	}
	ch := e.waiter(id)
	// An answer may have committed between the read and the registration; look again now that its wake-up is certain.
	if a, err = e.approval(id); err != nil || a.State != ApprovalPending {
		e.release(id)
		return a, err
	}
	select {
	case <-ch:
		return e.approval(id)
	case <-ctx.Done():
		return Approval{}, ctx.Err()
	}
}

// approval reads an Approval, or fails with a UserError when there is none.
func (e *Engine) approval(id string) (Approval, error) {
	var a *Approval
	err := e.repos.Tx(func(tx Tx) (err error) {
		a, err = tx.GetApproval(id)
		return err
	})
	if err != nil {
		return Approval{}, err
	}
	if a == nil {
		return Approval{}, userErrorf("approval %s not found", id)
	}
	a.Input = slices.Clone(a.Input)
	return *a, nil
}

// waiter returns the channel that closes when the Approval is no longer pending, creating it on first use.
func (e *Engine) waiter(id string) chan struct{} {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	ch, ok := e.waiters[id]
	if !ok {
		ch = make(chan struct{})
		e.waiters[id] = ch
	}
	return ch
}

// release wakes everyone waiting on an Approval and forgets its channel.
func (e *Engine) release(id string) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	if ch, ok := e.waiters[id]; ok {
		close(ch)
		delete(e.waiters, id)
	}
}

// requireApproval loads an Approval, or fails with a UserError when there is none.
func (o *op) requireApproval(id string) (*Approval, error) {
	a, err := o.tx.GetApproval(id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, userErrorf("approval %s not found", id)
	}
	return a, nil
}

// settleApproval closes a pending Approval in state and, if it was the one its Ticket showed, makes the Ticket show
// the next pending one or none. Waiters wake once the command committed.
func (o *op) settleApproval(a *Approval, state, note string) error {
	a.State = state
	a.AnsweredAt = o.timestamp()
	a.Answer = note
	if err := o.tx.PutApproval(a); err != nil {
		return err
	}
	t, err := o.tx.GetTicket(a.TicketID)
	if err != nil {
		return err
	}
	if t != nil {
		t.Stats.PersonWaitTime += nonNegative(o.at.Sub(a.CreatedAt))
		if t.PendingApproval == a.ID {
			t.PendingApproval = ""
			pending, err := o.tx.PendingApprovals()
			if err != nil {
				return err
			}
			for _, p := range pending {
				if p.TicketID == t.ID && p.ID != a.ID {
					t.PendingApproval = p.ID
					break
				}
			}
			t.UpdatedAt = o.at
		}
		if err := o.tx.PutTicket(t); err != nil {
			return err
		}
	}
	data := map[string]any{"state": state}
	if note != "" {
		data["answer"] = note
	}
	o.approvalEvent(EventApprovalAnswered, a, data)
	o.effects = append(o.effects, effect{kind: effectRelease, approvalID: a.ID})
	return nil
}

// expireTicketApprovals expires every pending Approval of an ended Ticket and clears what shows it.
func (o *op) expireTicketApprovals(t *Ticket) error {
	t.PendingApproval = ""
	pending, err := o.tx.PendingApprovals()
	if err != nil {
		return err
	}
	for _, a := range pending {
		if a.TicketID != t.ID {
			continue
		}
		a.State = ApprovalExpired
		a.AnsweredAt = o.timestamp()
		a.Answer = reasonTicketEnded
		t.Stats.PersonWaitTime += nonNegative(o.at.Sub(a.CreatedAt))
		if err := o.tx.PutApproval(a); err != nil {
			return err
		}
		o.approvalEvent(EventApprovalAnswered, a, map[string]any{"state": ApprovalExpired, "answer": reasonTicketEnded})
		o.effects = append(o.effects, effect{kind: effectRelease, approvalID: a.ID})
	}
	return nil
}

func (o *op) approvalEvent(typ string, a *Approval, data map[string]any) {
	o.effects = append(o.effects, effect{kind: effectEvent, event: Event{
		Type: typ, TicketID: a.TicketID, AssignmentID: a.AssignmentID, ApprovalID: a.ID, At: o.at, Data: data,
	}})
}

// AllowedByRule records on an open Ticket's timeline that the owner's standing allow rule let an action through with no
// Approval. It changes no state of the Ticket: nothing waited for the owner, so no escalation is counted.
func (e *Engine) AllowedByRule(ticketID, tool, rule, repo string) error {
	return e.command(cmdAllowedByRule, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		o.event(EventApprovalAllowed, t, "", map[string]any{"tool": tool, "rule": rule, "repo": repo,
			"text": "Allowed by your standing rule: " + tool + " · " + rule + " in " + repo})
		return nil
	})
}
