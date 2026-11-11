package core

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Plan first: a TaskOptions.PlanFirst Ticket waits, with no Assignment (R1), while a planner drafts its breakdown
// (EffectPlan). The owner edits the draft, then approves it, which splits the Ticket into child Tickets it waits for, or
// rejects it, which runs it as one Ticket. A failed plan is tried again only when the owner asks (Replan).

// EffectPlan is the effect kind that asks the planner for a plan-first Ticket's breakdown; its key is the Ticket's ID
// and its payload is {}.
const EffectPlan = "plan"

// LocalNode is the id of the node embedded in the server, where a folder Ticket with no pin works.
const LocalNode = "local"

// Types of the TicketEvents of a split plan.
const (
	EventSplitDrafted  = "split/drafted"
	EventSplitApproved = "split/approved"
	EventSplitRejected = "split/rejected"
	EventSplitFailed   = "split/failed"
	EventSplitOne      = "split/one"
)

const (
	msgSplitNeedsGit = "Splitting needs a git repository: each child works on its own branch."
	msgSplitStale    = "The split changed meanwhile; reload it and try again."
	noteSplitOne     = "The planner found nothing to split; it runs as one ticket."
	// planErrorMax bounds a failed plan's Error, in runes.
	planErrorMax = 300
)

// enqueuePlan asks the planner for t's breakdown.
func (o *op) enqueuePlan(t *Ticket) error {
	return o.enqueue(EffectPlan, t.ID, []byte("{}"))
}

// planned loads the open Ticket whose plan is in state; nil, nil when it is not, as for a late effect.
func (o *op) planned(ticketID, state string) (*Ticket, error) {
	t, err := o.require(ticketID)
	if err != nil {
		return nil, err
	}
	if t.Ended != nil || t.Plan == nil || t.Plan.State != state {
		return nil, nil
	}
	return t, nil
}

// draft loads the open Ticket whose plan is a draft at revision, or refuses.
func (o *op) draft(ticketID string, revision int) (*Ticket, error) {
	t, err := o.require(ticketID)
	if err != nil {
		return nil, err
	}
	if t.Plan == nil {
		return nil, userErrorf("%s was not planned first", ticketID)
	}
	if t.Ended != nil || t.Plan.State != PlanDraft || t.Plan.Breakdown == nil || t.Plan.Breakdown.Revision != revision {
		return nil, userErrorf(msgSplitStale)
	}
	return t, nil
}

// PlanDrafted is the planner's breakdown b of the plan-first Ticket. It applies only while the Ticket is planning, and
// is a no-op otherwise, as for a late effect. The breakdown, with no Base (the planner names none), becomes the draft at
// the revision after the last one (1 for the first), with its overlaps; one of
// fewer than 2 sub-tasks runs the Ticket as one (R18), and one the Policy auto-approves splits it at once (R20).
func (e *Engine) PlanDrafted(ticketID string, b Breakdown) error {
	return e.command(cmdPlanDrafted, func(o *op) error {
		t, err := o.planned(ticketID, PlanPlanning)
		if err != nil || t == nil {
			return err
		}
		b.Tasks = slices.Clone(b.Tasks)
		for i := range b.Tasks {
			if b.Tasks[i].Key == "" {
				b.Tasks[i].Key = "s" + strconv.Itoa(i+1)
			}
			// Only a worker's hand_off branches a child from a commit of its own; an up-front child starts late.
			b.Tasks[i].Base = ""
		}
		b.Revision = t.Plan.Revision + 1
		b.Overlaps = Overlaps(b.Tasks)
		t.Plan = &PlanState{State: PlanDraft, Breakdown: &b, At: o.at, Revision: b.Revision}
		if len(b.Tasks) < 2 {
			b.Note = noteSplitOne
			o.runAsOne(t)
			o.recordEvent(t, EventSplitOne, "", "", noteSplitOne)
			return o.tx.PutTicket(t)
		}
		o.recordEvent(t, EventSplitDrafted, "", "", "")
		if n := o.splitLimits().AutoApprove; len(b.Tasks) <= n && len(b.Overlaps) == 0 {
			// A draft that cannot be approved, such as one over the fan-out cap, stays a draft for the owner.
			var refused *UserError
			if _, err := o.approve(t); err != nil && !errors.As(err, &refused) {
				return err
			}
		}
		return o.tx.PutTicket(t)
	})
}

// PlanFailed is the planner's failure to draft the plan-first Ticket's breakdown: the plan fails with why, cut to 300
// runes, and waits for the owner. Like PlanDrafted, it applies only while the Ticket is planning.
func (e *Engine) PlanFailed(ticketID, why string) error {
	return e.command(cmdPlanFailed, func(o *op) error {
		t, err := o.planned(ticketID, PlanPlanning)
		if err != nil || t == nil {
			return err
		}
		why = truncateRunes(strings.TrimSpace(why), planErrorMax)
		t.Plan = &PlanState{State: PlanFailed, Error: why, At: o.at, Revision: t.Plan.Revision}
		o.recordEvent(t, EventSplitFailed, "", "", why)
		return o.tx.PutTicket(t)
	})
}

// UpdateBreakdown replaces the draft's sub-tasks with tasks, checked, when the draft is still at revision, and returns
// the new draft: its overlaps recomputed and its revision the next one.
func (e *Engine) UpdateBreakdown(ticketID string, revision int, tasks []ChildSpec) (Breakdown, error) {
	var out Breakdown
	err := e.command(cmdUpdateBreakdown, func(o *op) error {
		t, err := o.draft(ticketID, revision)
		if err != nil {
			return err
		}
		checked, err := CheckSpecs(tasks, nil, o.splitLimits().MaxChildren)
		if err != nil {
			return err
		}
		b := *t.Plan.Breakdown
		b.Tasks = checked
		b.Overlaps = Overlaps(checked)
		b.Revision++
		t.Plan.Breakdown = &b
		t.Plan.Revision = b.Revision
		t.Plan.At = o.at
		t.UpdatedAt = o.at
		out = b
		return o.tx.PutTicket(t)
	})
	if err != nil {
		return Breakdown{}, err
	}
	return out, nil
}

// ApproveBreakdown splits the plan-first Ticket into the draft's sub-tasks, when the draft is still at revision, and
// returns the children's ids in order. The Ticket waits for them all, then starts to merge and deliver their work.
func (e *Engine) ApproveBreakdown(ticketID string, revision int) ([]string, error) {
	var ids []string
	err := e.command(cmdApproveBreakdown, func(o *op) error {
		t, err := o.draft(ticketID, revision)
		if err != nil {
			return err
		}
		if ids, err = o.approve(t); err != nil {
			return err
		}
		return o.tx.PutTicket(t)
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// approve spawns the draft's children under lead, which waits for them all with no Assignment and is told how the work
// was split; it changes lead but does not put it.
func (o *op) approve(lead *Ticket) ([]string, error) {
	ids, err := o.spawn(lead, lead.Plan.Breakdown.Tasks)
	if err != nil {
		return nil, err
	}
	lead.Deps = slices.Clone(ids)
	lead.Waiting = slices.Clone(ids)
	lead.Plan.State = PlanApproved
	lead.Plan.At = o.at
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id + ": " + lead.Plan.Breakdown.Tasks[i].Title
	}
	text := fmt.Sprintf("Your task was split into %d child tickets: %s. When they are done, merge their branches into yours "+
		"(git merge athena/<id>) and deliver the result.", len(ids), strings.Join(parts, ", "))
	o.addInput(lead, Input{ID: newID(), Kind: InputTask, At: o.at, Text: text})
	o.recordEvent(lead, EventSplitApproved, "", "", strings.Join(ids, ", "))
	return ids, nil
}

// RejectBreakdown is the owner's choice to run the plan-first Ticket as one, from a draft or a failed plan: it gets its
// Assignment and starts.
func (e *Engine) RejectBreakdown(ticketID string) error {
	return e.command(cmdRejectBreakdown, func(o *op) error {
		t, err := o.unsettledPlan(ticketID)
		if err != nil {
			return err
		}
		o.runAsOne(t)
		o.recordEvent(t, EventSplitRejected, "", "", "")
		return o.tx.PutTicket(t)
	})
}

// Replan asks the planner again for the plan-first Ticket's breakdown, from a draft or a failed plan.
func (e *Engine) Replan(ticketID string) error {
	return e.command(cmdReplan, func(o *op) error {
		t, err := o.unsettledPlan(ticketID)
		if err != nil {
			return err
		}
		t.Plan = &PlanState{State: PlanPlanning, At: o.at, Revision: t.Plan.Revision}
		t.UpdatedAt = o.at
		if err := o.enqueuePlan(t); err != nil {
			return err
		}
		return o.tx.PutTicket(t)
	})
}

// unsettledPlan loads the open Ticket whose plan is a draft or failed, or refuses.
func (o *op) unsettledPlan(ticketID string) (*Ticket, error) {
	t, err := o.require(ticketID)
	if err != nil {
		return nil, err
	}
	if t.Ended != nil || t.Plan == nil || (t.Plan.State != PlanDraft && t.Plan.State != PlanFailed) {
		return nil, userErrorf("%s has no split draft or failed plan to settle", ticketID)
	}
	return t, nil
}

// runAsOne settles t's plan as rejected and starts t as one Ticket, with its first Assignment.
func (o *op) runAsOne(t *Ticket) {
	t.Plan.State = PlanRejected
	t.Plan.At = o.at
	o.start(t, o.newAssignment(t))
	t.UpdatedAt = o.at
}
