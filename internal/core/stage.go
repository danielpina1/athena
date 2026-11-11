package core

// Stage is a Ticket's high-level execution stage: waiting on a person (Action: delivery, answer, review,
// blocked, failed or offline), working (Action: queued, or empty), idle between turns, or ended.
type Stage struct {
	Stage  string `json:"stage"`
	Action string `json:"action,omitempty"`
}

// TicketStage classifies a Ticket; the rules follow the table in docs/reference/003-execution.md in order,
// and the first matching row wins.
func TicketStage(t *Ticket) Stage {
	if t.Ended != nil {
		return Stage{Stage: "ended"}
	}
	if a := t.LatestAssignment(); a != nil && a.NodeOffline != nil && a.State != AssignmentEnded {
		return Stage{"person", "offline"}
	}
	if t.Plan != nil {
		switch t.Plan.State {
		case PlanPlanning:
			return Stage{"working", "planning"}
		case PlanDraft, PlanFailed:
			return Stage{"person", "plan"}
		}
	}
	if t.Attention != nil && t.Attention.Child != "" {
		return Stage{"person", "child"}
	}
	if len(t.Assignments) == 0 && len(t.Waiting) > 0 {
		return Stage{"working", "waiting"}
	}
	if t.Delivery != nil {
		return Stage{"person", "delivery"}
	}
	if t.PendingApproval != "" {
		return Stage{"person", "approval"}
	}
	if t.PendingProposals > 0 {
		return Stage{"person", "proposal"}
	}
	if t.Attention != nil {
		return Stage{"person", "answer"}
	}
	a := t.LatestAssignment()
	var report ReportState
	if a != nil && a.Report != nil {
		report = a.Report.State
	}
	if report == ReportReady {
		return Stage{"person", "review"}
	}
	// A human follow-up after a blocked or failed report returns the Ticket to the executor.
	if (report == ReportBlocked || report == ReportFailed) && t.WaitsForPerson() {
		return Stage{"person", string(report)}
	}
	if a == nil || a.State == AssignmentPending {
		return Stage{"working", "queued"}
	}
	if report == ReportWaiting || a.State == AssignmentEnded {
		return Stage{Stage: "idle"}
	}
	return Stage{Stage: "working"}
}

// CanRetry reports whether an open Ticket is stalled on a blocked or failed report without a newer human
// follow-up, or has a failed Input delivery.
func CanRetry(t *Ticket) bool {
	return t.Ended == nil && (t.WaitsForPerson() || t.Delivery != nil)
}
