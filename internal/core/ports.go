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
	// TicketByRequestID returns the Ticket created under the given request id, or nil, nil if there is none.
	TicketByRequestID(requestID string) (*Ticket, error)
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
	// GetTrigger returns the Trigger with the given ID, or nil, nil if there is none.
	GetTrigger(id string) (*Trigger, error)
	// GetThread returns the Thread with the given key, or nil, nil if there is none.
	GetThread(key string) (*Thread, error)
	// PutThread inserts or replaces a Thread.
	PutThread(t *Thread) error
	// ActiveThreads returns every Thread with Triggers queued or an Eval in progress, ordered by key.
	ActiveThreads() ([]*Thread, error)
	// GetDecision returns the Decision with the given ID, or nil, nil if there is none.
	GetDecision(id string) (*Decision, error)
	// Decisions returns the Decisions made on a Thread, most recent first; limit <= 0 returns all.
	Decisions(thread string, limit int) ([]*Decision, error)
	// DecisionsOfTriggers returns the Decisions made on a Thread about any of the given Triggers, most recent first. It
	// costs what those Triggers have, not what the Thread has.
	DecisionsOfTriggers(thread string, triggerIDs []string) ([]*Decision, error)
	// RecentDecisions returns the Decisions of every Thread, most recent first; limit <= 0 returns all.
	RecentDecisions(limit int) ([]*Decision, error)
	// Enqueue records external work for the effects runner in the same transaction. Work is identified by
	// (kind, key): enqueuing a key still waiting replaces its payload, makes it due now and keeps its place in line.
	Enqueue(kind, key string, payload []byte) error
	// KVGet returns the plain string stored under key; the bool is false if there is none.
	KVGet(key string) (string, bool, error)
	// KVSet stores a plain string under key.
	KVSet(key, value string) error
	// GetReceipt returns the reply receipt with the given ID, or nil, nil if there is none.
	GetReceipt(id string) (*Receipt, error)
	// PutReceipt inserts or replaces a reply receipt.
	PutReceipt(r *Receipt) error
	// TicketReceipts returns a Ticket's reply receipts, in queue order (Seq).
	TicketReceipts(ticketID string) ([]*Receipt, error)
	// PutAgentMessage inserts or replaces an agent message.
	PutAgentMessage(m *AgentMessage) error
	// GetAgentMessage returns the agent message with the given ID, or nil, nil if there is none.
	GetAgentMessage(id string) (*AgentMessage, error)
	// AgentMessagesOf returns the agent messages sent from or to a Ticket, oldest first.
	AgentMessagesOf(ticketID string) ([]*AgentMessage, error)
	// CountAgentMessagesFrom counts the agent messages a Ticket sent at or after since.
	CountAgentMessagesFrom(ticketID string, since time.Time) (int, error)
	// AgentAnswer returns the agent message that answers messageID, or nil, nil if there is none.
	AgentAnswer(messageID string) (*AgentMessage, error)
	// GetProposal returns the Proposal with the given ID, or nil, nil if there is none.
	GetProposal(id string) (*Proposal, error)
	// PutProposal inserts or replaces a Proposal.
	PutProposal(p *Proposal) error
	// PendingProposals returns every Proposal still pending, oldest first.
	PendingProposals() ([]*Proposal, error)
	// TicketProposals returns a Ticket's Proposals in every state, oldest first.
	TicketProposals(ticketID string) ([]*Proposal, error)
	// PutBrake inserts or replaces the Brake of a Trigger.
	PutBrake(b Brake) error
	// DeleteBrake removes the Brake of a Trigger; none is not an error.
	DeleteBrake(triggerID string) error
	// Brakes returns every Brake, oldest first.
	Brakes() ([]Brake, error)
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
	EventNodeOffline       = "node.offline"
	EventNodeBack          = "node.back"
	EventTicketEnded       = "ticket.ended"
	EventTicketResumed     = "ticket.resumed"
	EventAssignmentStarted = "assignment.started"
	EventAssignmentEnded   = "assignment.ended"
	EventReportFiled       = "report.filed"
	EventReviewFiled       = "review.filed"
	EventInputAdded        = "input.added"
	EventApprovalRequested = "approval.requested"
	EventApprovalAnswered  = "approval.answered"
	// EventApprovalAllowed is an action a standing allow rule let through with no Approval.
	EventApprovalAllowed  = "approval.allowed"
	EventTriggerReceived  = "trigger.received"
	EventThreadSubscribed = "thread.subscribed"
	// EventThreadUnsubscribed is emitted for each Ticket whose Subscription to a Thread was dropped.
	EventThreadUnsubscribed = "thread.unsubscribed"
	EventEvalStarted        = "eval.started"
	EventDecisionRecorded   = "decision.recorded"
	EventTriggerRequeued    = "trigger.requeued"
	EventPolicyChanged      = "policy.changed"
	EventProposalCreated    = "proposal.created"
	EventProposalSettled    = "proposal.settled"
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

// SourceScope tells the core which Threads a Source still listens to. Implementations route by the Source prefix of
// the Thread key.
type SourceScope interface {
	// Listens reports whether the Source of the Thread key currently receives that Thread.
	Listens(thread string) bool
}

// Gate is the Policy's judgement of one Trigger on its Thread.
type Gate struct {
	// Strength is explicit or inferred when the Trigger is admitted, empty when it is not.
	Strength string `json:"strength,omitempty"`
	// Basis records how the Trigger was admitted; it becomes the Mandate's Basis on accept.
	Basis MandateBasis `json:"basis,omitzero"`
	// Reason says why the Trigger was not admitted; it is zero when it was.
	Reason Reason `json:"reason,omitzero"`
}

// RuleJudgement is an assessor's verdict on one enabled rule: whether the candidate matches it and the evidence.
type RuleJudgement struct {
	Name     string `json:"name"`
	Matches  bool   `json:"matches"`
	Evidence string `json:"evidence,omitempty"`
}

// Assessment is what the main Session returns for an inferred candidate: one judgement per enabled rule and its
// direction.
type Assessment struct {
	Rules  []RuleJudgement `json:"rules"`
	Accept bool            `json:"accept"`
	Reason string          `json:"reason,omitempty"`
}

// Policy is a stateless snapshot of the settings that decide who may start work. It never changes after it is built;
// a settings change produces a new Policy with a new Version.
type Policy interface {
	// Version identifies the rules, accept mode and Source parts the Policy was built from.
	Version() string
	// EvalTimeout is how long an Eval may run after its candidate reached the main Session.
	EvalTimeout() time.Duration
	// Gate judges a Trigger on its Thread, given the Tickets that may subscribe to the Thread.
	Gate(t Trigger, th Thread, tickets []Ticket) Gate
	// Revalidate reports whether an existing Mandate still holds before its Ticket resumes.
	Revalidate(m Mandate) bool
	// AdmitsInferred reports whether proactive work is accepted now: the accept mode is auto and the named rules,
	// at least one, are all enabled.
	AdmitsInferred(rules []string) bool
	// Assess converts an Assessment of a candidate of the given strength into an Engine outcome: the Decision kind
	// (accept or ignore), its reasons and whether an ignore records "would accept". An error means the assessment
	// failed. via is the Eval's Basis.Via: an Autopilot candidate is judged without rule judgements.
	Assess(a Assessment, strength, via string) (kind string, reasons []Reason, wouldAccept bool, err error)
	// WIPCeiling is how many Tickets of the repository may be in flight at once; always at least 1.
	WIPCeiling(repo string) int
	// Shadow reports whether the repository is in shadow mode: its work is held for the owner.
	Shadow(repo string) bool
	// Trust is the trust class of the Trigger's sender.
	Trust(t Trigger) string
	// ToolPolicy is the tool policy (ToolStandard, ToolRestricted) of work a sender of the class started.
	ToolPolicy(class string) string
}
