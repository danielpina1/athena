package core

import "time"

// Ports the core depends on, ported from the reference (src/core/ports.ts).

// Repositories is durable state. Every Engine command runs in one Tx.
//
// Tx must not be called from inside a Tx function: the SQLite store serializes transactions on one connection,
// so a nested Tx deadlocks.
type Repositories interface {
	// Tx runs fn in a transaction, committed if fn returns nil and rolled back otherwise.
	Tx(fn func(Tx) error) error
}

// Tx is the handle for one transaction; it is valid only inside the function passed to Repositories.Tx.
type Tx interface {
	// GetTicket returns the Ticket with the given ID, or nil, nil if there is none.
	GetTicket(id string) (*Ticket, error)
	// PutTicket inserts or replaces a Ticket.
	PutTicket(t *Ticket) error
	// OpenTickets returns every Ticket that has not ended, least recently updated first.
	OpenTickets() ([]*Ticket, error)
	// AllTickets returns every Ticket, ended ones included, least recently updated first.
	AllTickets() ([]*Ticket, error)
	// PutTrigger inserts or replaces a Trigger.
	PutTrigger(t *Trigger) error
	// PutDecision inserts or replaces a Decision.
	PutDecision(d *Decision) error
	// GetApproval returns the Approval with the given ID, or nil, nil if there is none.
	GetApproval(id string) (*Approval, error)
	// PutApproval inserts or replaces an Approval.
	PutApproval(a *Approval) error
	// PendingApprovals returns every Approval still pending, oldest first.
	PendingApprovals() ([]*Approval, error)
	// NextSeq returns the next value of the named counter: 1 on first use, then one more each call.
	// It moves only if the transaction commits.
	NextSeq(name string) (int, error)
}

// Executor executes Assignments; concurrency and workspace management are its own concern.
//
// The Engine calls it only after the command's transaction committed, with a snapshot of the committed Ticket.
// Each method only queues work and must not block or call an Engine command synchronously; the work reports back
// through the Engine's callbacks, from any goroutine.
type Executor interface {
	// Start queues a pending Assignment for launch; it reports back through Engine.AssignmentStarted or a failed report.
	Start(t Ticket, a Assignment)
	// Deliver queues delivery of the Ticket's waiting Inputs (Ticket.Undelivered, in order) to its running Assignment;
	// it reports back through Engine.InputsDelivered or Engine.DeliveryFailed, and Engine.InputsRead once the
	// worker's Session takes them into a step.
	Deliver(t Ticket, a Assignment)
	// Stop ends an Assignment's work, such as cancelling its Session's turn; the Ticket or Assignment has already
	// ended in the domain.
	Stop(t Ticket, a Assignment, reason string)
}

// Event types the Engine emits after a change.
const (
	EventTicketCreated     = "ticket.created"
	EventTicketEnded       = "ticket.ended"
	EventTicketResumed     = "ticket.resumed"
	EventAssignmentStarted = "assignment.started"
	EventAssignmentEnded   = "assignment.ended"
	EventReportFiled       = "report.filed"
	EventInputAdded        = "input.added"
	EventApprovalRequested = "approval.requested"
	EventApprovalAnswered  = "approval.answered"
)

// Event is what the Engine reports after a committed change.
type Event struct {
	Type         string
	TicketID     string
	AssignmentID string
	ApprovalID   string
	At           time.Time
	Data         map[string]any
}
