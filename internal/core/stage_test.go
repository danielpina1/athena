package core

import (
	"testing"
	"time"
)

var (
	earlier = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later   = earlier.Add(time.Hour)
)

func assignment(mods ...func(*Assignment)) Assignment {
	a := Assignment{ID: "a", State: AssignmentRunning}
	for _, m := range mods {
		m(&a)
	}
	return a
}

func reported(state ReportState, mods ...func(*Assignment)) Assignment {
	a := assignment(func(a *Assignment) { a.Report = &Report{State: state} })
	for _, m := range mods {
		m(&a)
	}
	return a
}

func withState(s AssignmentState) func(*Assignment) {
	return func(a *Assignment) { a.State = s }
}

func newTicket(mods ...func(*Ticket)) *Ticket {
	t := &Ticket{
		ID: "t", Title: "x", DecisionID: "d",
		Mandate:     Mandate{Strength: "explicit", PolicyVersion: "v", Actor: ActorRef{Source: "feishu", ID: "ou"}},
		Assignments: []Assignment{assignment()},
		Origin:      Origin{TriggerID: "x", TriggerIDs: []string{"x"}},
	}
	for _, m := range mods {
		m(t)
	}
	return t
}

func assignments(as ...Assignment) func(*Ticket) { return func(t *Ticket) { t.Assignments = as } }
func withDelivery(t *Ticket)                     { t.Delivery = &DeliveryFailure{Error: "offline"} }
func withApproval(t *Ticket)                     { t.PendingApproval = "ap" }
func withAttention(t *Ticket)                    { t.Attention = &Attention{ID: "q", Question: "?"} }
func withEnded(t *Ticket) {
	t.Ended = &Ended{By: EndByExecutor, Outcome: OutcomeClosed, Reason: "closed"}
}

// answered is a person's message that reached the Ticket after the report.
func answered(t *Ticket) {
	t.Inputs = []Input{{ID: "i", Kind: InputTrigger, TriggerID: "e", At: later, Human: true}}
}

// reportedAt is a report of state made at `earlier`, so `answered` follows it.
func reportedAt(state ReportState) Assignment {
	return reported(state, func(a *Assignment) { a.Report.At = earlier })
}

func each(mods ...func(*Ticket)) func(*Ticket) {
	return func(t *Ticket) {
		for _, m := range mods {
			m(t)
		}
	}
}

// The rows of the docs/reference/003-execution.md table, in order; a Ticket that matches several rows takes the first.
func TestTicketStage(t *testing.T) {
	cases := []struct {
		name   string
		ticket *Ticket
		want   Stage
	}{
		{"an ended Ticket", newTicket(withEnded, withAttention, assignments(reported(ReportFailed))), Stage{Stage: "ended"}},
		{"an undelivered Input", newTicket(withDelivery), Stage{"person", "delivery"}},
		{"delivery before answer and review", newTicket(withDelivery, withAttention, assignments(reported(ReportReady))), Stage{"person", "delivery"}},
		{"a pending Approval", newTicket(withApproval), Stage{"person", "approval"}},
		{"delivery before an Approval", newTicket(withDelivery, withApproval), Stage{"person", "delivery"}},
		{"an Approval before answer and review", newTicket(withApproval, withAttention, assignments(reported(ReportReady))), Stage{"person", "approval"}},
		{"an ended Ticket with an Approval", newTicket(withEnded, withApproval), Stage{Stage: "ended"}},
		{"an Attention", newTicket(withAttention), Stage{"person", "answer"}},
		{"answer before review", newTicket(withAttention, assignments(reported(ReportReady))), Stage{"person", "answer"}},
		{"answer before a failure", newTicket(withAttention, assignments(reported(ReportFailed))), Stage{"person", "answer"}},
		{"a ready Report", newTicket(assignments(reported(ReportReady))), Stage{"person", "review"}},
		{"a ready Report of an ended Assignment", newTicket(assignments(reported(ReportReady, withState(AssignmentEnded)))), Stage{"person", "review"}},
		{"a blocked Report", newTicket(assignments(reported(ReportBlocked))), Stage{"person", "blocked"}},
		{"a failed Report", newTicket(assignments(reported(ReportFailed))), Stage{"person", "failed"}},
		{"a blocked Report a person answered since", newTicket(answered, assignments(reportedAt(ReportBlocked))), Stage{Stage: "working"}},
		{"a failed Report a person answered since", newTicket(answered, assignments(reportedAt(ReportFailed))), Stage{Stage: "working"}},
		{"no Assignment", newTicket(assignments()), Stage{"working", "queued"}},
		{"a pending Assignment", newTicket(assignments(reported(ReportFailed, withState(AssignmentEnded)), assignment(withState(AssignmentPending)))), Stage{"working", "queued"}},
		{"a waiting Report", newTicket(assignments(reported(ReportWaiting))), Stage{Stage: "idle"}},
		{"an ended Assignment", newTicket(assignments(assignment(withState(AssignmentEnded)))), Stage{Stage: "idle"}},
		{"a running Assignment", newTicket(), Stage{Stage: "working"}},
		{"a progress Report", newTicket(assignments(reported(ReportProgress))), Stage{Stage: "working"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TicketStage(c.ticket); got != c.want {
				t.Fatalf("TicketStage = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestCanRetry(t *testing.T) {
	cases := []struct {
		name   string
		want   bool
		ticket *Ticket
	}{
		{"a failed open Ticket", true, newTicket(assignments(reported(ReportFailed)))},
		{"a blocked open Ticket", true, newTicket(assignments(reported(ReportBlocked)))},
		{"a failed Ticket that also awaits an answer", true, newTicket(withAttention, assignments(reported(ReportFailed)))},
		{"a blocked Ticket whose Input is undelivered", true, newTicket(withDelivery, assignments(reported(ReportBlocked)))},
		{"a failed Ticket that ended", false, newTicket(withEnded, assignments(reported(ReportFailed)))},
		{"a Ticket retried already, whose new Assignment is pending", false, newTicket(assignments(reported(ReportFailed, withState(AssignmentEnded)), assignment(withState(AssignmentPending))))},
		{"a ready Ticket", false, newTicket(assignments(reported(ReportReady)))},
		{"a running Ticket", false, newTicket()},
		{"a Ticket without an Assignment", false, newTicket(assignments())},
		// The worker has the person's message, so a retry would only interrupt it.
		{"a failed Ticket a person answered since", false, newTicket(answered, assignments(reportedAt(ReportFailed)))},
		// Unless delivering that message failed: a new Assignment starts the worker again and takes the message.
		{"a failed Ticket whose delivery of a person's answer failed", true, newTicket(answered, withDelivery, assignments(reportedAt(ReportFailed)))},
		{"a working Ticket whose Input could not be delivered", true, newTicket(withDelivery)},
		{"an ended Ticket whose Input could not be delivered", false, newTicket(withEnded, withDelivery)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CanRetry(c.ticket); got != c.want {
				t.Fatalf("CanRetry = %v, want %v", got, c.want)
			}
		})
	}
	t.Run("offers a retry in each person stage a new Assignment remedies", func(t *testing.T) {
		tk := newTicket(answered, withDelivery, assignments(reportedAt(ReportFailed)))
		if got := TicketStage(tk); got != (Stage{"person", "delivery"}) {
			t.Fatalf("stage = %+v", got)
		}
		if !CanRetry(tk) {
			t.Fatal("CanRetry = false")
		}
	})
}

func TestWaitsForPerson(t *testing.T) {
	human := func(at time.Time) Input { return Input{ID: "i", Kind: InputTrigger, At: at, Human: true} }
	bot := Input{ID: "b", Kind: InputTrigger, At: later}
	cases := []struct {
		name   string
		ticket *Ticket
		want   bool
	}{
		{"blocked, no input", newTicket(assignments(reportedAt(ReportBlocked))), true},
		{"failed, no input", newTicket(assignments(reportedAt(ReportFailed))), true},
		{"blocked, bot input after", newTicket(assignments(reportedAt(ReportBlocked)), func(t *Ticket) { t.Inputs = []Input{bot} }), true},
		{"blocked, human input before", newTicket(assignments(reportedAt(ReportBlocked)), func(t *Ticket) { t.Inputs = []Input{human(earlier.Add(-time.Minute))} }), true},
		{"blocked, human input after", newTicket(assignments(reportedAt(ReportBlocked)), func(t *Ticket) { t.Inputs = []Input{human(later)} }), false},
		{"blocked, human text input after", newTicket(assignments(reportedAt(ReportBlocked)), func(t *Ticket) {
			t.Inputs = []Input{{ID: "i", Kind: InputText, At: later, Human: true}}
		}), true},
		{"ready", newTicket(assignments(reported(ReportReady))), false},
		{"no assignment", newTicket(assignments()), false},
		{"assignment without report", newTicket(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ticket.WaitsForPerson(); got != c.want {
				t.Fatalf("WaitsForPerson = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLatestAssignment(t *testing.T) {
	if newTicket(assignments()).LatestAssignment() != nil {
		t.Fatal("want nil for no assignments")
	}
	tk := newTicket(assignments(assignment(), assignment(func(a *Assignment) { a.ID = "b" })))
	if got := tk.LatestAssignment(); got == nil || got.ID != "b" {
		t.Fatalf("LatestAssignment = %+v", got)
	}
}

// Only a trigger Input is a person's message; a human text Input does not release a blocked Ticket.
func TestStageHumanTextInputDoesNotRelease(t *testing.T) {
	tk := newTicket(assignments(reportedAt(ReportBlocked)), func(t *Ticket) {
		t.Inputs = []Input{{ID: "i", Kind: InputText, At: later, Human: true}}
	})
	if got := TicketStage(tk); got != (Stage{"person", "blocked"}) {
		t.Fatalf("stage = %+v, want person/blocked", got)
	}
}
