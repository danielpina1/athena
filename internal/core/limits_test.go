package core

import (
	"testing"
	"time"
)

// limitedTicket is a running Ticket of runtime whose first Assignment has a delivered trigger Input its worker never
// read, and a second Input that waits.
func limitedTicket(t *testing.T, h *harness, runtime string) (ticketID, assignmentID, unread string) {
	t.Helper()
	tk, err := h.eng.SubmitOwnerTask("Fix the parser", TaskOptions{Dir: "/work/x", Runtime: runtime})
	h.must(err)
	h.settle(true)
	h.must(h.eng.AddPersonInput(tk.ID, "also the lexer"))
	h.settle(false)
	stored := h.ticket(tk.ID)
	return tk.ID, stored.Assignments[0].ID, lastInput(stored)
}

// A usage limit ends the running Assignment with ReasonUsageLimit and its limit, and a new pending Assignment of the
// same role takes over: the old one stops, the new one starts, and the trigger Inputs its worker never read go first.
func TestUsageLimitedRequeuesOnNewAssignment(t *testing.T) {
	h := newHarness(t)
	id, old, unread := limitedTicket(t, h, RuntimeAuto)
	h.must(h.eng.AssignmentStarted(id, old, "s1", "", RuntimeClaude))
	h.must(h.eng.InputsRead(id, []string{h.ticket(id).Inputs[0].ID}))
	h.tick(5)
	reset := base.Add(time.Hour)
	h.must(h.eng.UsageLimited(id, old, UsageLimit{Runtime: RuntimeClaude, ResetsAt: &reset, Message: "usage limit reached"}))

	got := h.ticket(id)
	eq(t, "assignments", assignmentStates(got), []AssignmentState{AssignmentEnded, AssignmentPending})
	ended, next := got.Assignments[0], got.Assignments[1]
	eq(t, "ended reason", ended.EndedReason, ReasonUsageLimit)
	if ended.Limit == nil || ended.Limit.Runtime != RuntimeClaude || ended.Limit.ResetsAt == nil || !ended.Limit.ResetsAt.Equal(reset) ||
		ended.Limit.Message != "usage limit reached" {
		t.Fatalf("limit = %+v", ended.Limit)
	}
	eq(t, "ended at", *ended.EndedAt, base.Add(5*time.Second))
	eq(t, "new role", next.Role, ended.Role)
	eq(t, "auto: the new assignment is unpinned", next.Runtime, "")
	eq(t, "the unread input goes first", got.Undelivered, []string{unread})
	m := h.exec.methods()
	eq(t, "executor calls", m[len(m)-2:], []string{"stop:" + old + ":" + ReasonUsageLimit, "start:" + next.ID})
	eq(t, "no retry counted", got.Stats.Retries, 0)

	// A review keeps its role on the Assignment that takes over.
	h2 := newHarness(t)
	id2, old2, _ := limitedTicket(t, h2, RuntimeAuto)
	h2.must(h2.repos.Tx(func(tx Tx) error {
		tk, err := tx.GetTicket(id2)
		if err != nil {
			return err
		}
		tk.Assignments[0].Role = RoleReview
		return tx.PutTicket(tk)
	}))
	h2.must(h2.eng.UsageLimited(id2, old2, UsageLimit{Runtime: RuntimeCodex}))
	eq(t, "review role kept", h2.ticket(id2).Assignments[1].Role, RoleReview)
}

// A limit is no person stage: no Report, no question, nothing waits for a person, and the Ticket shows working.
func TestUsageLimitedNeverPersonStage(t *testing.T) {
	h := newHarness(t)
	id, old, _ := limitedTicket(t, h, RuntimeAuto)
	events := len(h.events.types())
	h.must(h.eng.UsageLimited(id, old, UsageLimit{Runtime: RuntimeClaude}))
	got := h.ticket(id)
	for _, a := range got.Assignments {
		if a.Report != nil {
			t.Fatalf("assignment %s has report %+v", a.ID, a.Report)
		}
	}
	if got.Attention != nil || got.WaitsForPerson() || CanRetry(got) || got.Ended != nil {
		t.Fatalf("attention %+v, waits %v, retry %v, ended %+v", got.Attention, got.WaitsForPerson(), CanRetry(got), got.Ended)
	}
	eq(t, "stage while queued", stageOf(got), Stage{"working", "queued"})
	for _, typ := range h.events.types()[events:] {
		if typ == EventReportFiled {
			t.Fatal("a report was filed")
		}
	}
	h.settle(true)
	eq(t, "stage once the new one runs", stageOf(h.ticket(id)), Stage{Stage: "working"})
}

// A Ticket pinned to a runtime stays on it: the Assignment that takes over is pinned to the limited runtime, and waits
// for its reset rather than move.
func TestPinnedTicketKeepsRuntimeAfterLimit(t *testing.T) {
	for _, rtm := range []string{RuntimeCodex, RuntimeClaude} {
		h := newHarness(t)
		id, old, _ := limitedTicket(t, h, rtm)
		h.must(h.eng.UsageLimited(id, old, UsageLimit{Runtime: rtm}))
		eq(t, rtm+": new assignment runtime", h.ticket(id).Assignments[1].Runtime, rtm)
	}
}

// Only the running Assignment of an open Ticket can be limited: anything else changes nothing.
func TestUsageLimitedIgnoresStaleTarget(t *testing.T) {
	h := newHarness(t)
	id, old, _ := limitedTicket(t, h, RuntimeAuto)
	before := h.ticket(id)
	calls := len(h.exec.methods())
	h.must(h.eng.UsageLimited(id, "nope", UsageLimit{Runtime: RuntimeClaude}))
	eq(t, "unknown assignment", h.ticket(id), before)

	// The limited Assignment again, once it was replaced.
	h.must(h.eng.UsageLimited(id, old, UsageLimit{Runtime: RuntimeClaude}))
	replaced := h.ticket(id)
	h.must(h.eng.UsageLimited(id, old, UsageLimit{Runtime: RuntimeClaude}))
	eq(t, "replaced assignment", h.ticket(id), replaced)
	// The pending one that took over, before it runs.
	h.must(h.eng.UsageLimited(id, replaced.Assignments[1].ID, UsageLimit{Runtime: RuntimeClaude}))
	eq(t, "pending assignment", h.ticket(id), replaced)
	eq(t, "executor calls", len(h.exec.methods()), calls+2)

	// An ended Ticket.
	h.settle(true)
	h.must(h.eng.StopTicket(id, "done"))
	stopped := h.ticket(id)
	h.must(h.eng.UsageLimited(id, stopped.Assignments[1].ID, UsageLimit{Runtime: RuntimeClaude}))
	eq(t, "ended ticket", h.ticket(id), stopped)
	wantUserError(t, "unknown ticket", h.eng.UsageLimited("T-9999", "a", UsageLimit{}))
}
