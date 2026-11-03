// Package core is the pure domain model of Athena: no I/O, no clocks, no storage.
// It is ported from the TypeScript reference (src/core/model.ts).
package core

import "time"

// ReportState is what an Assignment reports: work is in progress, its worker waits for a Trigger,
// a result is ready, a person must act to unblock or recover it, or nothing is left to do.
type ReportState string

const (
	ReportProgress ReportState = "progress"
	ReportWaiting  ReportState = "waiting"
	ReportReady    ReportState = "ready"
	ReportBlocked  ReportState = "blocked"
	ReportFailed   ReportState = "failed"
	ReportDropped  ReportState = "dropped"
)

// AssignmentState is the lifecycle of an Assignment.
type AssignmentState string

const (
	AssignmentPending AssignmentState = "pending"
	AssignmentRunning AssignmentState = "running"
	AssignmentEnded   AssignmentState = "ended"
)

// Report is what an Assignment last said.
type Report struct {
	State   ReportState `json:"state"`
	Summary string      `json:"summary"`
	At      time.Time   `json:"at"`
}

// Assignment binds a Ticket to one executor instance.
type Assignment struct {
	ID          string          `json:"id"`
	State       AssignmentState `json:"state"`
	CreatedAt   time.Time       `json:"createdAt"`
	StartedAt   *time.Time      `json:"startedAt,omitempty"`
	EndedAt     *time.Time      `json:"endedAt,omitempty"`
	EndedReason string          `json:"endedReason,omitempty"`
	Report      *Report         `json:"report,omitempty"`
	// DeliveredAt is when Inputs last reached the worker.
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
	// IdleAt is when a turn of its worker last ended with nothing left to run or waiting for it.
	IdleAt *time.Time `json:"idleAt,omitempty"`
	// SessionID is the runtime session to resume.
	SessionID string `json:"sessionId,omitempty"`
	// Model is the model the Executor runs the Assignment with: the Ticket's, or the Executor's default when it names
	// none.
	Model string `json:"model,omitempty"`
}

// InputKind says what an Input carries.
type InputKind string

const (
	InputTrigger InputKind = "trigger"
	InputText    InputKind = "text"
	InputNote    InputKind = "note"
	InputResumed InputKind = "resumed"
)

// Input is something that reached a Ticket after it started.
type Input struct {
	ID        string    `json:"id"`
	Kind      InputKind `json:"kind"`
	At        time.Time `json:"at"`
	TriggerID string    `json:"triggerId,omitempty"`
	Text      string    `json:"text,omitempty"`
	// Human is set when a person wrote the Trigger.
	Human bool `json:"human,omitempty"`
	// ReadAt is when the worker's Session took the Input into a step; nil while it waits.
	ReadAt *time.Time `json:"readAt,omitempty"`
	// DeliveredTo is the Assignment the Input was last delivered to.
	DeliveredTo string     `json:"deliveredTo,omitempty"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
}

// Attention is a question for a person; TriggerID is the human message that raised it.
type Attention struct {
	ID        string `json:"id"`
	Question  string `json:"question"`
	TriggerID string `json:"triggerId,omitempty"`
	// At is when the Ticket began waiting for a person's answer; a replaced question keeps it.
	At time.Time `json:"at,omitzero"`
}

// TicketEndBy says who ended a Ticket.
type TicketEndBy string

const (
	EndByExecutor TicketEndBy = "executor"
	EndByHuman    TicketEndBy = "human"
)

// TicketOutcome is how a Ticket ended.
type TicketOutcome string

const (
	OutcomeCompleted TicketOutcome = "completed"
	OutcomeMerged    TicketOutcome = "merged"
	OutcomeClosed    TicketOutcome = "closed"
	OutcomeDropped   TicketOutcome = "dropped"
	OutcomeStopped   TicketOutcome = "stopped"
)

// Ended records when, by whom and how a Ticket ended.
type Ended struct {
	At      time.Time     `json:"at"`
	By      TicketEndBy   `json:"by"`
	Outcome TicketOutcome `json:"outcome"`
	Reason  string        `json:"reason,omitempty"`
}

// ActorRef is an actor identity in its source's own vocabulary.
type ActorRef struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}

// ChannelRef is a write-only address on a thread; the address format belongs to the source.
type ChannelRef struct {
	Source  string            `json:"source"`
	Thread  string            `json:"thread"`
	Address map[string]string `json:"address,omitempty"`
}

// Mandate is the immutable authorization produced by an accept Decision.
type Mandate struct {
	Strength      string   `json:"strength"`
	PolicyVersion string   `json:"policyVersion"`
	Actor         ActorRef `json:"actor"`
	Scope         string   `json:"scope,omitempty"`
}

// Execution is what a Ticket's Executor chose.
type Execution struct {
	Name       string `json:"name"`
	Directory  string `json:"directory"`
	Assignment string `json:"assignment"`
	Runtime    string `json:"runtime,omitempty"`
	Node       string `json:"node,omitempty"`
	Model      string `json:"model,omitempty"`
}

// Origin holds the Triggers a Ticket was accepted from: the one that started it and the batch its evaluation held.
type Origin struct {
	TriggerID  string   `json:"triggerId"`
	TriggerIDs []string `json:"triggerIds"`
}

// DeliveryFailure is the latest failed delivery of an Input to a worker, until a delivery succeeds.
type DeliveryFailure struct {
	Error string    `json:"error"`
	At    time.Time `json:"at"`
}

// Ticket is an authorized work item with its assignments, channels and inputs.
type Ticket struct {
	ID         string       `json:"id"`
	Title      string       `json:"title"`
	DecisionID string       `json:"decisionId"`
	Mandate    Mandate      `json:"mandate"`
	Channels   []ChannelRef `json:"channels,omitempty"`
	// Ended is nil while the Ticket is open.
	Ended       *Ended       `json:"ended,omitempty"`
	Attention   *Attention   `json:"attention,omitempty"`
	Assignments []Assignment `json:"assignments,omitempty"`
	Inputs      []Input      `json:"inputs,omitempty"`
	// Undelivered lists the IDs of Inputs not yet handed to a running Assignment.
	Undelivered []string         `json:"undelivered,omitempty"`
	CreatedAt   time.Time        `json:"createdAt"`
	UpdatedAt   time.Time        `json:"updatedAt"`
	Origin      Origin           `json:"origin"`
	Execution   *Execution       `json:"execution,omitempty"`
	Delivery    *DeliveryFailure `json:"delivery,omitempty"`
	// PendingApproval is the ID of an Approval of this Ticket waiting for a person; empty if none.
	PendingApproval string `json:"pendingApproval,omitempty"`
	// Stats are the Ticket's performance numbers, updated in the same transaction as the command that causes them.
	Stats TicketStats `json:"stats"`
	// SessionCosts is, per runtime session, the cumulative cost it last reported: a runtime reports a session's total
	// cost on every turn, and a resumed process carries it on, so only the growth is added to Stats.CostUSD.
	SessionCosts map[string]float64 `json:"sessionCosts,omitempty"`
}

// Trigger is one immutable event produced by a source; Text is untrusted external data.
type Trigger struct {
	ID     string    `json:"id"`
	Source string    `json:"source"`
	Actor  ActorRef  `json:"actor"`
	Human  bool      `json:"human,omitempty"`
	At     time.Time `json:"at"`
	Text   string    `json:"text,omitempty"`
}

// Decision is the outcome of one evaluation.
type Decision struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	TicketID   string    `json:"ticketId,omitempty"`
	TriggerIDs []string  `json:"triggerIds,omitempty"`
	At         time.Time `json:"at"`
	Mandate    *Mandate  `json:"mandate,omitempty"`
}

// LatestAssignment returns the Ticket's most recent Assignment, or nil if it has none.
func (t *Ticket) LatestAssignment() *Assignment {
	if len(t.Assignments) == 0 {
		return nil
	}
	return &t.Assignments[len(t.Assignments)-1]
}

// WaitsForPerson reports whether the latest Assignment reported blocked or failed and no person's message
// reached the Ticket since. A later report that is neither, a person's message, or a retry (a new Assignment) lifts it.
func (t *Ticket) WaitsForPerson() bool {
	a := t.LatestAssignment()
	if a == nil || a.Report == nil {
		return false
	}
	if s := a.Report.State; s != ReportBlocked && s != ReportFailed {
		return false
	}
	for _, in := range t.Inputs {
		// TODO(context): reference also requires !context (an Input from a Thread followed only for context).
		if in.Kind == InputTrigger && in.Human && in.At.After(a.Report.At) {
			return false
		}
	}
	return true
}
