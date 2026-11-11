// Package core is the pure domain model of Athena: no I/O, no clocks, no storage.
// It is ported from the TypeScript reference (src/core/model.ts).
package core

import (
	"errors"
	"time"
)

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

// Report is what an Assignment last said: its Question for a person, if it asked one, and the pull request it named
// (Pull, 0 for none).
type Report struct {
	State    ReportState `json:"state"`
	Summary  string      `json:"summary"`
	Question string      `json:"question,omitempty"`
	Pull     int         `json:"pull,omitempty"`
	// Head is a child's branch head at its ready report.
	Head string    `json:"head,omitempty"`
	At   time.Time `json:"at"`
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
	// Interruptions are the times the Assignment's session was cut short from outside, such as by a server restart,
	// oldest first; the last one is open until the session starts again.
	Interruptions []Interruption `json:"interruptions,omitempty"`
	// Runtime is the runtime the Assignment's session runs on (RuntimeClaude, RuntimeCodex): pinned when it was made, or
	// the one its Executor started it on (AssignmentStarted). Empty until then; a Ticket's Assignments may each run on
	// another one.
	Runtime string `json:"runtime,omitempty"`
	// Role is what the Assignment's session does: empty for the Ticket's worker (it implements), RoleReview for a
	// review of its work.
	Role string `json:"role,omitempty"`
	// Limit is the usage limit its session stopped at, for an Assignment that ended with ReasonUsageLimit.
	Limit *UsageLimit `json:"limit,omitempty"`
	// Node is the node the fleet placed the Assignment on; empty until placed.
	Node string `json:"node,omitempty"`
	// NodeOffline is when the Assignment's node went offline; set while it is, cleared when the node is back.
	NodeOffline *time.Time `json:"nodeOffline,omitempty"`
	// Avoid lists the nodes the fleet must not place the Assignment on.
	Avoid []string `json:"avoid,omitempty"`
	// ReviewOutcome is what a review Assignment (RoleReview) concluded, once it ended with ReasonReviewed.
	ReviewOutcome *ReviewOutcome `json:"reviewOutcome,omitempty"`
	// Continues is the implement Assignment whose held ready report this one applied after its review (applyHeld): it
	// continues that Assignment's session and launched none of its own until an Input reaches it.
	Continues string `json:"continues,omitempty"`
}

// RoleReview is the Role of an Assignment that reviews the Ticket's work.
const RoleReview = "review"

// InterruptRestart is the reason of an Interruption a server restart caused.
const InterruptRestart = "server restart"

// InterruptPaused is the reason of an Interruption the owner's pause caused: the session parked at the end of its turn.
const InterruptPaused = "paused"

// Interruption is a time an Assignment's session was cut short while it worked. ResumedAt is when the session started
// again, and EndedAt when the Assignment ended before it did (a stop, a failover, a review); both are nil while the
// Interruption is open.
type Interruption struct {
	At        time.Time  `json:"at"`
	Reason    string     `json:"reason"`
	ResumedAt *time.Time `json:"resumedAt,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	// ReportOwed is set on a pause (InterruptPaused) whose turn ended without the Report the worker owed: it was not
	// nudged into a new turn while paused, so the resume starts its session again to ask for it.
	ReportOwed bool `json:"reportOwed,omitempty"`
}

// OpenInterruption returns the Assignment's last Interruption when its session has not started again since and the
// Assignment has not ended, else nil.
func (a *Assignment) OpenInterruption() *Interruption {
	if n := len(a.Interruptions); n > 0 && a.Interruptions[n-1].ResumedAt == nil && a.Interruptions[n-1].EndedAt == nil {
		return &a.Interruptions[n-1]
	}
	return nil
}

// InputKind says what an Input carries.
type InputKind string

const (
	InputTrigger InputKind = "trigger"
	InputText    InputKind = "text"
	InputNote    InputKind = "note"
	InputResumed InputKind = "resumed"
	// InputReview carries a review's findings to the implementer whose ready report it sent back (FileReview).
	InputReview InputKind = "review"
	// InputAgent carries a message from another agent (AgentMessage); it grants nothing.
	InputAgent InputKind = "agent"
	// InputChild reports a child Ticket's result or failure to its lead.
	InputChild InputKind = "child"
	// InputTask carries a sub-task's instructions to a child.
	InputTask InputKind = "task"
)

// Input is something that reached a Ticket after it started.
type Input struct {
	ID        string    `json:"id"`
	Kind      InputKind `json:"kind"`
	At        time.Time `json:"at"`
	TriggerID string    `json:"triggerId,omitempty"`
	Text      string    `json:"text,omitempty"`
	// Note is the main Session's routing note on a Trigger it attached to the Ticket.
	Note string `json:"note,omitempty"`
	// Human is set when a person wrote the Trigger.
	Human bool `json:"human,omitempty"`
	// Context is set when the Trigger reached the Ticket only on Threads it follows for context: the Ticket reads it
	// but does not own it, and it neither lifts a wait for a person nor resumes the Ticket.
	Context bool `json:"context,omitempty"`
	// ReadAt is when the worker's Session took the Input into a step; nil while it waits.
	ReadAt *time.Time `json:"readAt,omitempty"`
	// DeliveredTo is the Assignment the Input was last delivered to.
	DeliveredTo string     `json:"deliveredTo,omitempty"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
	// From is the Ticket an InputAgent or InputChild comes from.
	From string `json:"from,omitempty"`
	// MessageID is the AgentMessage an InputAgent carries.
	MessageID string `json:"messageId,omitempty"`
	// Answer is set when an InputAgent answers a question the Ticket asked.
	Answer bool `json:"answer,omitempty"`
}

// Attention is a question for a person; TriggerID is the human message that raised it.
type Attention struct {
	ID        string `json:"id"`
	Question  string `json:"question"`
	TriggerID string `json:"triggerId,omitempty"`
	// At is when the Ticket began waiting for a person's answer; a replaced question keeps it.
	At time.Time `json:"at,omitzero"`
	// Child is the child Ticket whose failure raised the Attention on its lead.
	Child string `json:"child,omitempty"`
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
	// Thread is the key of the Thread the Mandate's work started on; revalidation asks the Source whether it still
	// listens to it. Owner Mandates have none.
	Thread string `json:"thread,omitempty"`
	// Basis records how the gate admitted the Trigger; revalidation reads it.
	Basis MandateBasis `json:"basis,omitzero"`
	// Trust is the trust class of the batch of Triggers the Mandate was given on, fixed at accept (batchTrust; see TrustClass).
	Trust string `json:"trust,omitempty"`
}

// Trust classes of a sender. The Policy classifies each Trigger; a Mandate records the class of its batch (batchTrust).
const (
	TrustOwner        = "owner"
	TrustAllowed      = "allowed"
	TrustCollaborator = "collaborator"
	TrustStranger     = "stranger"
	TrustBot          = "bot"
	TrustCI           = "ci"
)

// Tool policies of a trust class's work (Policy.ToolPolicy): standard, or restricted (sandboxed, asking nobody, and
// held for the owner). A value other than standard counts as restricted.
const (
	ToolStandard   = "standard"
	ToolRestricted = "restricted"
)

// TrustClass is the Mandate's trust class: the recorded one, else, for a Mandate made before classes were recorded,
// owner when the owner gave it, bot for a bot's, and allowed otherwise (the gate admitted a listed sender).
func (m Mandate) TrustClass() string {
	if m.Trust != "" {
		return m.Trust
	}
	switch m.Basis.Via {
	case ViaOwner:
		return TrustOwner
	case ViaBot:
		return TrustBot
	}
	return TrustAllowed
}

// Admission paths a MandateBasis names in Via.
const (
	ViaAddressed = "addressed"
	ViaFollowup  = "followup"
	ViaAdmission = "admission"
	ViaBot       = "bot"
	ViaOwner     = "owner"
	// ViaAutopilot: an Autopilot entry started the work and is its own rule.
	ViaAutopilot = "autopilot"
)

// MandateBasis records how the gate admitted a Trigger: by which path (Via) and, for revalidation, the receiving
// Source, scope and row, the listed sender and the rules that matched.
type MandateBasis struct {
	// Via is addressed, followup, admission, bot, owner or autopilot.
	Via    string   `json:"via,omitempty"`
	Source string   `json:"source,omitempty"`
	Scope  string   `json:"scope,omitempty"`
	Row    string   `json:"row,omitempty"`
	Sender string   `json:"sender,omitempty"`
	Rules  []string `json:"rules,omitempty"`
	// Observation marks an inferred candidate admitted by observation.
	Observation bool `json:"observation,omitempty"`
}

// The runtimes a Ticket may run on (Execution.Runtime, TaskOptions.Runtime). RuntimeAuto lets the Executor choose for
// each Assignment; an Assignment records the one it ran on (Assignment.Runtime).
const (
	RuntimeClaude = "claude"
	RuntimeCodex  = "codex"
	RuntimeAuto   = "auto"
)

// RuntimeLabel is how a runtime is named to a person: "Claude Code", "Codex", "Auto"; another name as it is.
func RuntimeLabel(name string) string {
	switch name {
	case RuntimeClaude:
		return "Claude Code"
	case RuntimeCodex:
		return "Codex"
	case RuntimeAuto:
		return "Auto"
	}
	return name
}

// Execution is what a Ticket's Executor chose. Runtime is the Ticket's choice (RuntimeClaude, RuntimeCodex or
// RuntimeAuto); each Assignment records the runtime it actually ran on.
type Execution struct {
	Name       string `json:"name"`
	Directory  string `json:"directory"`
	Assignment string `json:"assignment"`
	Runtime    string `json:"runtime,omitempty"`
	Node       string `json:"node,omitempty"`
	Model      string `json:"model,omitempty"`
	// Base is the commit a forked child's branch starts at.
	Base string `json:"base,omitempty"`
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
	// PendingProposals counts the Proposals of this Ticket waiting for the owner; only the Engine writes it.
	PendingProposals int `json:"pendingProposals,omitempty"`
	// Stats are the Ticket's performance numbers, updated in the same transaction as the command that causes them.
	Stats TicketStats `json:"stats"`
	// SessionCosts is, per runtime session, the cumulative cost it last reported: a runtime reports a session's total
	// cost on every turn, and a resumed process carries it on, so only the growth is added to Stats.CostUSD.
	SessionCosts map[string]float64 `json:"sessionCosts,omitempty"`
	// Follows are the Threads a Source made the Ticket follow; only the Engine writes them.
	Follows []Follow `json:"follows,omitempty"`
	// Repo is the repository (owner/name) the Ticket works in, or empty.
	Repo string `json:"repo,omitempty"`
	// RequestID is the client's id for the submit that created the Ticket; a second submit under it is a duplicate.
	RequestID string `json:"requestId,omitempty"`
	// Pairing is how the Ticket's work is paired: PairingReview has each ready report reviewed first (review.go), ""
	// none.
	Pairing string `json:"pairing,omitempty"`
	// Review is the review in progress, nil when none is.
	Review *ReviewState `json:"review,omitempty"`
	// Lead is the Ticket this one is a child of, or empty (split.go).
	Lead string `json:"lead,omitempty"`
	// Children are the IDs of the Tickets split off this one, every one it ever had.
	Children []string `json:"children,omitempty"`
	// Deps are the sibling Ticket IDs that must end before this child starts.
	Deps []string `json:"deps,omitempty"`
	// Waiting are the Deps not yet ended; while any is, the Ticket has no Assignment.
	Waiting []string `json:"waiting,omitempty"`
	// Depth is how deeply the Ticket is nested: 0 for a lead or a plain Ticket.
	Depth int `json:"depth,omitempty"`
	// Plan is the split plan of a plan-first Ticket, nil otherwise.
	Plan *PlanState `json:"plan,omitempty"`
	// SplitKey is the sub-task key the child was made from.
	SplitKey string `json:"splitKey,omitempty"`
}

// Follow is a Thread a Ticket follows: Own when the Ticket opened it, Closed once the Thread closed.
type Follow struct {
	Thread string `json:"thread"`
	Own    bool   `json:"own,omitempty"`
	Closed *Close `json:"closed,omitempty"`
}

// Close records when and how a Thread closed.
type Close struct {
	At      time.Time     `json:"at"`
	Outcome TicketOutcome `json:"outcome"`
}

// SourceSchedule names the Source of Autopilot entries, whose Threads are permanent: one per entry, for good.
const SourceSchedule = "schedule"

// Kinds of Trigger.
const (
	KindMessage = "message"
	KindReview  = "review"
	KindCI      = "ci"
	KindClosed  = "closed"
	// KindSchedule is a Trigger an Autopilot entry raised.
	KindSchedule = "schedule"
)

// Ways a Trigger can address the agent.
const (
	AddressedMention = "mention"
	AddressedDirect  = "direct"
)

// Trigger is one immutable event produced by a source; Text is untrusted external data. Its Disposition is the only
// part that changes after it is stored.
type Trigger struct {
	ID     string    `json:"id"`
	Source string    `json:"source"`
	Actor  ActorRef  `json:"actor"`
	Human  bool      `json:"human,omitempty"`
	At     time.Time `json:"at"`
	Text   string    `json:"text,omitempty"`
	// Kind is message, review, ci, closed or schedule.
	Kind string `json:"kind,omitempty"`
	// Bot is set when an automation wrote the Trigger.
	Bot bool `json:"bot,omitempty"`
	// Addressed is mention or direct when the Trigger addressed the agent, empty otherwise.
	Addressed string `json:"addressed,omitempty"`
	// Scope is the receiving-scope row of the Source: issues, pulls, comments, ci or owner.
	Scope string `json:"scope,omitempty"`
	// Threads are the keys of the Threads the Trigger belongs to; Threads[0] is the primary one.
	Threads []string `json:"threads,omitempty"`
	// ReplyTo is the ID of the Trigger this one answers.
	ReplyTo string `json:"replyTo,omitempty"`
	// Meta belongs to the Source; the core does not interpret it.
	Meta     map[string]string `json:"meta,omitempty"`
	Channels []ChannelRef      `json:"channels,omitempty"`
	// Disposition is what became of the Trigger.
	Disposition Disposition `json:"disposition,omitzero"`
}

// Disposition records what became of a Trigger.
type Disposition struct {
	// Filtered is the Source's code when no scope received the Trigger.
	Filtered string `json:"filtered,omitempty"`
	// Reached lists the Tickets it reached by fan-out or a person's requeue.
	Reached []string `json:"reached,omitempty"`
	// Context lists the Tickets of Reached it reached only on Threads they follow for context, which makes it none of
	// theirs.
	Context []string `json:"context,omitempty"`
	// Attached lists the Tickets a Decision attached it to.
	Attached []string `json:"attached,omitempty"`
	// Requeued is when a person last queued it for a fresh Eval.
	Requeued *time.Time `json:"requeued,omitempty"`
	// HandedBack maps each Ticket that handed it back to the reason.
	HandedBack map[string]string `json:"handedBack,omitempty"`
	// Acknowledged lists the Tickets whose Executor acknowledged or answered it.
	Acknowledged []string `json:"acknowledged,omitempty"`
}

// Subscription is a Ticket's interest in a Thread.
type Subscription struct {
	TicketID string `json:"ticketId"`
	// Context is set when the Ticket follows the Thread only for context: its messages reach the Ticket but give
	// it no follow-up strength and do not resume it.
	Context bool `json:"context,omitempty"`
	// Bound is set when the Ticket was bound to the Thread by a Source rather than created from it.
	Bound bool `json:"bound,omitempty"`
	// Own is set when the Ticket opened the Thread.
	Own bool `json:"own,omitempty"`
}

// Evaluation is the Eval in progress on a Thread.
type Evaluation struct {
	ID     string `json:"id"`
	Thread string `json:"thread"`
	// Primary is the ID of the admissible Trigger that gave the batch its strength.
	Primary    string   `json:"primary"`
	TriggerIDs []string `json:"triggerIds"`
	// Strength is explicit or inferred.
	Strength      string       `json:"strength"`
	Basis         MandateBasis `json:"basis,omitzero"`
	PolicyVersion string       `json:"policyVersion"`
	StartedAt     time.Time    `json:"startedAt"`
	// DeliveredAt is when the candidate reached the main Session; the deadline counts from it.
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
}

// Thread is a conversation in a Source: the Triggers queued on it for Eval, its Eval, its Subscriptions.
type Thread struct {
	// Key is "<source>:<key>".
	Key           string         `json:"key"`
	Source        string         `json:"source"`
	Label         string         `json:"label,omitempty"`
	Subscriptions []Subscription `json:"subscriptions,omitempty"`
	// Queued lists the IDs of Triggers waiting for an Eval.
	Queued []string    `json:"queued,omitempty"`
	Eval   *Evaluation `json:"eval,omitempty"`
	// Expired counts the Evals of the queued Triggers that expired in a row.
	Expired   int      `json:"expired,omitempty"`
	Decisions []string `json:"decisions,omitempty"`
	Closed    *Close   `json:"closed,omitempty"`
	// Meta belongs to the Source; the core does not interpret it.
	Meta map[string]string `json:"meta,omitempty"`
}

// Active reports whether the Thread has Triggers queued or an Eval in progress.
func (t *Thread) Active() bool { return len(t.Queued) > 0 || t.Eval != nil }

// Reason says why the core recorded a Decision or set a Trigger aside: a code with its values and, for one Trigger
// of a batch, that Trigger's ID.
type Reason struct {
	Code      string            `json:"code"`
	Values    map[string]string `json:"values,omitempty"`
	TriggerID string            `json:"triggerId,omitempty"`
}

// Decision is the outcome of one evaluation.
type Decision struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	TicketID   string    `json:"ticketId,omitempty"`
	TriggerIDs []string  `json:"triggerIds,omitempty"`
	At         time.Time `json:"at"`
	Mandate    *Mandate  `json:"mandate,omitempty"`
	// Thread is the key of the Thread the Decision was made on.
	Thread  string   `json:"thread,omitempty"`
	Reasons []Reason `json:"reasons,omitempty"`
	// WouldAccept is set on an ignore of a candidate the Policy would have accepted.
	WouldAccept bool `json:"wouldAccept,omitempty"`
	// IgnoredFromCard is set when an inferred candidate was ignored from its card.
	IgnoredFromCard bool `json:"ignoredFromCard,omitempty"`
	// Dismissed is when a person dismissed the Decision's problem.
	Dismissed *time.Time `json:"dismissed,omitempty"`
	// Ack is the acknowledgement text an accept sent.
	Ack string `json:"ack,omitempty"`
}

// LatestAssignment returns the Ticket's most recent Assignment, or nil if it has none.
func (t *Ticket) LatestAssignment() *Assignment {
	if len(t.Assignments) == 0 {
		return nil
	}
	return &t.Assignments[len(t.Assignments)-1]
}

// WaitsForPerson reports whether the latest Assignment reported blocked or failed and no person's message
// reached the Ticket since, other than on a Thread it follows only for context. A later report that is neither, a person's message, or a retry (a new Assignment) lifts it.
func (t *Ticket) WaitsForPerson() bool {
	a := t.LatestAssignment()
	if a == nil || a.Report == nil {
		return false
	}
	if s := a.Report.State; s != ReportBlocked && s != ReportFailed {
		return false
	}
	for _, in := range t.Inputs {
		// A person's message on a Thread the Ticket follows only for context is not addressed to its worker.
		if in.Kind == InputTrigger && in.Human && !in.Context && in.At.After(a.Report.At) {
			return false
		}
	}
	return true
}

// HandedOff reports whether an open Ticket's worker handed the work off: the running Assignment's latest Report is
// ready, so the Ticket waits for the Threads it follows (its pull request) to close, and its Executor keeps no process
// for it. The next Input resumes the work, as a message resumes an ended Ticket.
func (t *Ticket) HandedOff() bool {
	if t.Ended != nil {
		return false
	}
	a := runningAssignment(t)
	return a != nil && a.Report != nil && a.Report.State == ReportReady
}

// FollowsDone reports whether a Ticket that follows Threads is done with them, and the outcome it then ends with: no
// Thread it opened (Own) is still open or, when it opened none, every Thread it follows closed. The outcome is that of
// the latest of those closes. A Ticket that follows no Thread is never done with them.
func (t *Ticket) FollowsDone() (TicketOutcome, bool) {
	c := t.followsDone()
	if c == nil {
		return "", false
	}
	return c.Outcome, true
}

// followsDone is FollowsDone's latest close, or nil while the Ticket is not done with its Threads.
func (t *Ticket) followsDone() *Close {
	var own, all []Follow
	for _, f := range t.Follows {
		all = append(all, f)
		if f.Own {
			own = append(own, f)
		}
	}
	counted := all
	if len(own) > 0 {
		counted = own
	}
	var latest *Close
	for _, f := range counted {
		if f.Closed == nil {
			return nil
		}
		if latest == nil || !f.Closed.At.Before(latest.At) {
			latest = f.Closed
		}
	}
	return latest
}

// ErrNodeOffline is in the error of a call to a node that is not connected (a *UserError whose message is
// "<name> is offline"); the fleet returns it, the dashboard shows its message.
var ErrNodeOffline = errors.New("node offline")

// ReasonNodeOffline is why Retry elsewhere ends an Assignment whose node went offline.
const ReasonNodeOffline = "node offline"

// SourceAgent is the Source of an Input that another agent wrote.
const SourceAgent = "agent"

// Reasons a child Ticket ends or is released.
const (
	// ReasonLeadEnded is why a lead's ending stops its open children.
	ReasonLeadEnded = "lead ended"
	// ReasonContinuedWithout is why a failed child is stopped so its dependents can start.
	ReasonContinuedWithout = "continued without it"
)
