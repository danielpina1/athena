package core

import "time"

// ReasonUsageLimit is the EndedReason of an Assignment whose session stopped at its runtime's usage limit.
const ReasonUsageLimit = "usage_limit"

// UsageLimit is the usage limit that stopped an Assignment's session: the runtime it ran on, when the limit resets
// (nil when not known, or when what the runtime said had already passed) and what the runtime said, secrets masked.
type UsageLimit struct {
	Runtime  string     `json:"runtime"`
	ResetsAt *time.Time `json:"resetsAt,omitempty"`
	Message  string     `json:"message,omitempty"`
}

// UsageLimited is the Executor's fact that the session of the Assignment that runs the open Ticket stopped at its
// runtime's usage limit l. No person needs to act: the Assignment ends with ReasonUsageLimit and l, the trigger Inputs
// its worker never read go back ahead of what waits, and a new pending Assignment of the same role takes over, pinned
// to l's runtime when the Ticket names one (claude or codex) and else left for the Executor to place (auto). The old
// one stops and the new one starts. No Report is filed and no Attention raised, so the Ticket never waits for a person
// because of a limit. A fact about any other Assignment changes nothing.
func (e *Engine) UsageLimited(ticketID, assignmentID string, l UsageLimit) error {
	return e.command(cmdUsageLimited, func(o *op) error {
		t, err := o.running(ticketID, assignmentID)
		if t == nil || err != nil {
			return err
		}
		a := runningAssignment(t)
		limit := l
		limit.ResetsAt = clonePtr(l.ResetsAt)
		a.Limit = &limit
		role := a.Role
		o.endAssignment(t, a, ReasonUsageLimit)
		o.requeueUntaken(t, assignmentID)
		next := o.newAssignment(t)
		n := findAssignment(t, next)
		n.Role = role
		if x := t.Execution; x != nil && (x.Runtime == RuntimeClaude || x.Runtime == RuntimeCodex) {
			n.Runtime = l.Runtime
		}
		t.UpdatedAt = o.at
		o.stop(t, assignmentID, ReasonUsageLimit)
		o.start(t, next)
		return o.tx.PutTicket(t)
	})
}
