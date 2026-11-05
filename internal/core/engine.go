package core

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/metrics"
)

// The Engine is the single writer of Triggers, Decisions, Tickets and Assignments, ported from the reference
// (src/core/engine.ts, tickets.ts, assignments.ts) for owner tasks, the Executor's callbacks and a person's commands.

const (
	// OwnerPolicyVersion is the Policy version an owner task's Mandate records; plan 1 has no Policy.
	OwnerPolicyVersion = "owner"
	// ownerScope is the Mandate scope of an owner task.
	ownerScope = "owner"
	// ownerSource is the Source of a Trigger the owner wrote.
	ownerSource = "owner"
	// ticketSeq names the counter Ticket IDs come from.
	ticketSeq = "ticket"
	// titleMaxRunes bounds a Ticket title taken from a task's first line.
	titleMaxRunes = 80
	// deliveryErrorMax bounds the error a failed delivery records (the reference keeps 300).
	deliveryErrorMax = 300
	// reasonRetried is why a retry ends the stalled Assignment.
	reasonRetried = "retried"
)

// OwnerActor is the owner, who submits tasks and writes a person's Inputs.
var OwnerActor = ActorRef{Source: ownerSource, ID: "owner"}

// UserError is an invalid request: an unknown Ticket, a command its state does not allow, or a malformed input.
// Commands return it as *UserError; match it with errors.As.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

// ErrDuplicate is returned with the existing Ticket when a task is submitted again under the same request id.
var ErrDuplicate = errors.New("duplicate request")

func userErrorf(format string, args ...any) error {
	return &UserError{Msg: fmt.Sprintf(format, args...)}
}

// TaskOptions are where and how an owner task runs.
type TaskOptions struct {
	Dir   string
	Model string
	// Repo is the repository (owner/name) the task works in, through its Source; it excludes Dir.
	Repo string
	// RequestID makes the submit idempotent: a task submitted again with an id already stored creates nothing and
	// returns the stored Ticket with ErrDuplicate.
	RequestID string
}

// ReportInput is a worker's report on its Ticket. A Question asks a person (an empty one keeps the current question);
// AssignmentID, when set, names the Assignment the report is from, and a report from one that no longer runs is refused.
// Pull is the pull request the report names, kept on the Report (binding it is the caller's).
type ReportInput struct {
	AssignmentID string
	State        ReportState
	Summary      string
	Question     string
	Pull         int
}

// Engine is the domain's single entry point. Each command runs in one transaction; the Executor calls and Events it
// produces run in order only after the transaction committed. It is safe for concurrent use: commands are serialized.
type Engine struct {
	repos Repositories
	exec  Executor
	now   func() time.Time
	emit  func(Event)
	// mu serializes commands, from their transaction through their effects, so effects run in commit order.
	mu sync.Mutex
	// waiters holds, per pending Approval someone waits on, the channel that closes when it is answered or expires.
	// It has its own lock so a waiter never needs mu.
	waitMu  sync.Mutex
	waiters map[string]chan struct{}
	// reg and cmdHist are set by SetMetrics before the first command; nil without metrics.
	reg     *metrics.Registry
	cmdHist map[string]*metrics.Histogram
	// policy, scope and wake come from Options; each may be nil.
	policy func() Policy
	scope  SourceScope
	wake   func()
	// relays is Options.Relays.
	relays bool
}

// Options configure an Engine beyond its Repositories and Executor. Every field may be left zero.
type Options struct {
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time
	// Emit receives the Events after commit; it defaults to a no-op.
	Emit func(Event)
	// Policy returns the current Policy. Without one, every Mandate revalidates, as an owner task's always does.
	Policy func() Policy
	// Scope tells which Threads the Sources listen to. Without one, every Thread is listened to.
	Scope SourceScope
	// Wake is called after a command that enqueued external work committed, such as the effects runner's Wake.
	Wake func()
	// Relays makes commands enqueue the main Session's relay and notice effects (see notices.go). Leave it off when no
	// router runs, or they pile up unread.
	Relays bool
}

// NewEngine returns an Engine with the plan 1 options: a clock and an event sink (see Options).
func NewEngine(repos Repositories, exec Executor, now func() time.Time, emit func(Event)) *Engine {
	return NewEngineWith(repos, exec, Options{Now: now, Emit: emit})
}

// NewEngineWith returns an Engine configured by opts. Emit and Wake, like the Executor, run after commit while the
// command still holds the Engine: they must not block or call an Engine command (queries are fine). Policy and the
// Scope's Listens run inside a command's transaction and must not call the Engine either.
func NewEngineWith(repos Repositories, exec Executor, opts Options) *Engine {
	now, emit := opts.Now, opts.Emit
	if now == nil {
		now = time.Now
	}
	if emit == nil {
		emit = func(Event) {}
	}
	return &Engine{repos: repos, exec: exec, now: now, emit: emit, waiters: map[string]chan struct{}{},
		policy: opts.Policy, scope: opts.Scope, wake: opts.Wake, relays: opts.Relays}
}

// --- Owner tasks ---

// SubmitOwnerTask records the owner's task: a Trigger, a direct accept Decision with an explicit Mandate, and a Ticket
// whose title is the text's first line, with one pending Assignment and the text as its first Input; then it queues
// the Assignment's launch. A task on a repository (TaskOptions.Repo, owner/name) works there and has no Channel: its
// worker reports to the owner, and its replies are refused.
func (e *Engine) SubmitOwnerTask(text string, opts TaskOptions) (Ticket, error) {
	title := titleOf(text)
	if title == "" {
		return Ticket{}, userErrorf("a task needs text")
	}
	if opts.Repo != "" {
		if !validRepo(opts.Repo) {
			return Ticket{}, userErrorf("repository %q is not owner/name", opts.Repo)
		}
		if opts.Dir != "" {
			return Ticket{}, userErrorf("a task on a repository runs in its own worktree: it takes no directory")
		}
	}
	var created, existing *Ticket
	err := e.command(cmdSubmit, func(o *op) error {
		if opts.RequestID != "" {
			t, err := o.tx.TicketByRequestID(opts.RequestID)
			if err != nil {
				return err
			}
			if t != nil {
				existing = t
				return nil
			}
		}
		n, err := o.tx.NextSeq(ticketSeq)
		if err != nil {
			return err
		}
		id := fmt.Sprintf("T-%04d", n)
		// The owner's message goes straight to its Ticket, with no routing: record where it reached.
		trigger := &Trigger{ID: newID(), Source: ownerSource, Actor: OwnerActor, Human: true, At: o.at, Text: text,
			Disposition: Disposition{Reached: []string{id}}}
		mandate := Mandate{Strength: "explicit", PolicyVersion: OwnerPolicyVersion, Actor: OwnerActor, Scope: ownerScope}
		if opts.Repo != "" {
			// The owner's word is the whole basis: no Source admitted it, so no Source revalidates it.
			mandate.Basis = MandateBasis{Via: ViaOwner}
		}
		decisionMandate := mandate
		decision := &Decision{ID: newID(), Kind: "accept", TicketID: id, TriggerIDs: []string{trigger.ID}, At: o.at, Mandate: &decisionMandate}
		t := &Ticket{
			ID: id, Title: title, DecisionID: decision.ID, Mandate: mandate,
			CreatedAt: o.at, UpdatedAt: o.at,
			Origin:    Origin{TriggerID: trigger.ID, TriggerIDs: []string{trigger.ID}},
			Execution: &Execution{Directory: opts.Dir, Runtime: "claude", Model: opts.Model},
			Repo:      opts.Repo,
			RequestID: opts.RequestID,
		}
		assignment := o.newAssignment(t)
		input := Input{ID: newID(), Kind: InputTrigger, At: o.at, TriggerID: trigger.ID, Text: text, Human: true}
		t.Inputs = []Input{input}
		t.Undelivered = []string{input.ID}
		if err := o.tx.PutTrigger(trigger); err != nil {
			return err
		}
		if err := o.tx.PutDecision(decision); err != nil {
			return err
		}
		if err := o.tx.PutTicket(t); err != nil {
			return err
		}
		o.start(t, assignment)
		o.event(EventTicketCreated, t, assignment, map[string]any{"decisionId": decision.ID})
		created = t
		return nil
	})
	if err != nil {
		return Ticket{}, err
	}
	if existing != nil {
		return cloneTicket(existing), ErrDuplicate
	}
	return cloneTicket(created), nil
}

// --- Executor callbacks ---

// AssignmentStarted is the Executor's fact that an Assignment runs, in the runtime session sessionID with model (the
// model the Executor chose, its default when the Ticket names none); the Inputs that wait for it go out. A start of
// an unknown or ended Assignment, such as one a retry replaced, changes nothing.
func (e *Engine) AssignmentStarted(ticketID, assignmentID, sessionID, model string) error {
	return e.command(cmdAssignmentStarted, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		a := findAssignment(t, assignmentID)
		if a == nil || a.State == AssignmentEnded {
			return nil
		}
		a.State = AssignmentRunning
		if a.StartedAt == nil {
			a.StartedAt = o.timestamp()
			// From the Assignment's creation to its session starting: the wait for a slot plus the launch.
			wait := nonNegative(o.at.Sub(a.CreatedAt))
			t.Stats.QueueWait += wait
			if o.reg != nil {
				o.observe(func(r *metrics.Registry) { r.Histogram("dispatch_seconds").Observe(wait) })
			}
		}
		if sessionID != "" {
			a.SessionID = sessionID
		}
		if model != "" {
			a.Model = model
		}
		t.UpdatedAt = o.at
		if t.Ended == nil && len(t.Undelivered) > 0 {
			o.deliver(t, a.ID)
		}
		o.event(EventAssignmentStarted, t, a.ID, map[string]any{"sessionId": sessionID})
		return o.tx.PutTicket(t)
	})
}

// InputsDelivered is the Executor's fact that Inputs reached the Assignment that runs the open Ticket, which clears a
// failed delivery. A fact about an Assignment that no longer runs, or about Inputs that no longer wait, changes nothing.
// With no inputIDs it is the fact that a message that is no Input reached the worker, such as the nudge to report
// after a turn without one (plan 1 R33): only the Assignment's DeliveredAt moves, so its worker counts as busy.
func (e *Engine) InputsDelivered(ticketID, assignmentID string, inputIDs []string) error {
	return e.command(cmdInputsDelivered, func(o *op) error {
		t, err := o.running(ticketID, assignmentID)
		if t == nil || err != nil {
			return err
		}
		if len(inputIDs) == 0 {
			runningAssignment(t).DeliveredAt = o.timestamp()
			t.UpdatedAt = o.at
			return o.tx.PutTicket(t)
		}
		delivered := map[string]bool{}
		var waiting []string
		for _, id := range t.Undelivered {
			if slices.Contains(inputIDs, id) {
				delivered[id] = true
			} else {
				waiting = append(waiting, id)
			}
		}
		if len(delivered) == 0 {
			return nil
		}
		t.Undelivered = waiting
		t.UpdatedAt = o.at
		runningAssignment(t).DeliveredAt = o.timestamp()
		for i := range t.Inputs {
			in := &t.Inputs[i]
			if in.Kind == InputTrigger && delivered[in.ID] {
				in.DeliveredTo = assignmentID
				in.DeliveredAt = o.timestamp()
			}
		}
		t.Delivery = nil
		return o.tx.PutTicket(t)
	})
}

// DeliveryFailed is the Executor's fact that delivering Inputs to the Assignment that runs the open Ticket failed; the
// Ticket shows it until a delivery succeeds, a new Assignment starts or the Ticket ends.
func (e *Engine) DeliveryFailed(ticketID, assignmentID, errMsg string) error {
	return e.command(cmdDeliveryFailed, func(o *op) error {
		t, err := o.running(ticketID, assignmentID)
		if t == nil || err != nil {
			return err
		}
		t.Delivery = &DeliveryFailure{Error: truncateRunes(errMsg, deliveryErrorMax), At: o.at}
		return o.tx.PutTicket(t)
	})
}

// InputsRead is the Executor's fact that the worker's Session of an open Ticket took the trigger Inputs inputIDs into
// a step, from which its model reads them.
func (e *Engine) InputsRead(ticketID string, inputIDs []string) error {
	return e.command(cmdInputsRead, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil || t.Ended != nil {
			return err
		}
		changed := false
		for i := range t.Inputs {
			in := &t.Inputs[i]
			if in.Kind == InputTrigger && in.ReadAt == nil && slices.Contains(inputIDs, in.ID) {
				in.ReadAt = o.timestamp()
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return o.tx.PutTicket(t)
	})
}

// TurnEnded is the Executor's fact that a turn of the worker of the Assignment that runs the open Ticket ended.
func (e *Engine) TurnEnded(ticketID, assignmentID string) error {
	return e.command(cmdTurnEnded, func(o *op) error {
		t, err := o.running(ticketID, assignmentID)
		if t == nil || err != nil {
			return err
		}
		runningAssignment(t).IdleAt = o.timestamp()
		return o.tx.PutTicket(t)
	})
}

// FileReport records a worker's report on the open Ticket's current Assignment. Its Question asks a person (a new
// question replaces the current one, and a ready report clears it). A blocked or failed report holds back automatic
// Inputs until a person's message arrives; a dropped report ends the Ticket dropped, and a ready one ends it completed.
// A Ticket that follows Threads ends with them instead (FollowsDone): a ready report ends it only once it is done with
// them, as the latest close did, and a dropped one after such a close ends it as that close did.
func (e *Engine) FileReport(ticketID string, r ReportInput) error {
	switch r.State {
	case ReportProgress, ReportWaiting, ReportReady, ReportBlocked, ReportFailed, ReportDropped:
	default:
		return userErrorf("unknown report state %q", r.State)
	}
	if r.Question != "" && r.State == ReportReady {
		return userErrorf("a ready report cannot ask a question: mention optional follow-ups in summary and report ready " +
			"without question, or, when a person must decide before the work is done, report waiting with question instead")
	}
	return e.command(cmdFileReport, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		if t.Ended != nil {
			return userErrorf("ticket %s has ended", ticketID)
		}
		a := runningAssignment(t)
		if a == nil {
			a = t.LatestAssignment()
		}
		if a == nil {
			return userErrorf("ticket %s has no assignment", ticketID)
		}
		if r.AssignmentID != "" && r.AssignmentID != a.ID {
			if findAssignment(t, r.AssignmentID) == nil {
				return userErrorf("ticket %s has no assignment %s", ticketID, r.AssignmentID)
			}
			return userErrorf("assignment %s of ticket %s has ended", r.AssignmentID, ticketID)
		}
		if a.State == AssignmentEnded {
			return userErrorf("assignment %s of ticket %s has ended", a.ID, ticketID)
		}
		// A worker may file its report in the same step as a tool call that waits for approval (the real CLI runs
		// them in parallel); a result claim then cannot be true yet, and the denial may make it false.
		if t.PendingApproval != "" && (r.State == ReportReady || r.State == ReportDropped) {
			return userErrorf("an action of ticket %s waits for the owner's approval; report %s after it is answered", ticketID, r.State)
		}
		attention := t.Attention
		var raised *Attention
		if r.Question != "" && (attention == nil || attention.Question != r.Question) {
			// A replaced question keeps the time the Ticket began waiting.
			since := o.at
			if attention != nil && !attention.At.IsZero() {
				since = attention.At
			}
			attention = &Attention{ID: newID(), Question: r.Question, At: since}
			raised = attention
			t.Stats.Questions++
		}
		if r.State == ReportReady {
			o.settleAttention(t)
			attention = nil
		}
		t.Attention = attention
		if !hasReport(t) {
			t.Stats.TimeToFirstReport = nonNegative(o.at.Sub(t.CreatedAt))
		}
		a.Report = &Report{State: r.State, Summary: r.Summary, Question: r.Question, Pull: r.Pull, At: o.at}
		t.UpdatedAt = o.at
		if r.State == ReportBlocked || r.State == ReportFailed {
			t.Undelivered = slices.DeleteFunc(t.Undelivered, func(id string) bool { return automatic(findInput(t, id)) })
			if len(t.Undelivered) == 0 {
				t.Undelivered = nil
			}
		}
		data := map[string]any{"state": r.State, "summary": r.Summary}
		if r.Question != "" {
			data["question"] = r.Question
		}
		o.event(EventReportFiled, t, a.ID, data)
		if err := o.reportRelay(t, a, raised); err != nil {
			return err
		}
		// A Ticket that follows Threads ends with them: ready ends it only once it is done with them, as they ended, and
		// dropped after a close it deferred ends it as that close did.
		done, isDone := t.FollowsDone()
		switch {
		case r.State == ReportDropped && isDone:
			if err := o.end(t, EndByExecutor, done, ""); err != nil {
				return err
			}
		case r.State == ReportDropped:
			if err := o.end(t, EndByExecutor, OutcomeDropped, ""); err != nil {
				return err
			}
		case r.State == ReportReady && len(t.Follows) == 0:
			if err := o.end(t, EndByExecutor, OutcomeCompleted, ""); err != nil {
				return err
			}
		case r.State == ReportReady && isDone:
			if err := o.end(t, EndByExecutor, done, ""); err != nil {
				return err
			}
		}
		return o.tx.PutTicket(t)
	})
}

// --- Person commands ---

// AddPersonInput gives a Ticket the owner's message: it is recorded as a person's Trigger and reaches the Ticket as a
// human Input, which answers its question (clearing Attention), releases a blocked or failed report, and goes to the
// running Assignment (or launches a pending one again). An ended Ticket resumes with a new Assignment, unless a person
// stopped it or its Mandate no longer revalidates; a Ticket whose worker handed off (HandedOff) resumes only while its
// Mandate revalidates too.
func (e *Engine) AddPersonInput(ticketID, text string) error {
	return e.AddPersonInputWithID(ticketID, newID(), text)
}

// AddPersonInputWithID is AddPersonInput under a caller-chosen Input id: an id the Ticket already has is a no-op, so
// a message sent again changes nothing.
func (e *Engine) AddPersonInputWithID(ticketID, inputID, text string) error {
	if inputID == "" {
		return userErrorf("a message needs an id")
	}
	if strings.TrimSpace(text) == "" {
		return userErrorf("a message needs text")
	}
	return e.command(cmdPersonInput, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		if findInput(t, inputID) != nil {
			return nil
		}
		if t.Ended != nil && t.Ended.By == EndByHuman {
			return userErrorf("ticket %s was stopped", ticketID)
		}
		if (t.Ended != nil || t.HandedOff()) && !o.revalidates(t) {
			return userErrorf("the mandate of ticket %s no longer holds; it needs a new accept", ticketID)
		}
		trigger := &Trigger{ID: newID(), Source: ownerSource, Actor: OwnerActor, Human: true, At: o.at, Text: text,
			Disposition: Disposition{Reached: []string{ticketID}}}
		if err := o.tx.PutTrigger(trigger); err != nil {
			return err
		}
		o.settleAttention(t)
		t.Attention = nil
		t.Stats.PersonMessages++
		input := Input{ID: inputID, Kind: InputTrigger, At: o.at, TriggerID: trigger.ID, Text: text, Human: true}
		o.addInput(t, input)
		o.event(EventInputAdded, t, "", map[string]any{"inputId": input.ID})
		o.wake(t)
		return o.tx.PutTicket(t)
	})
}

// StopTicket is a person's stop, which withdraws the Ticket's Mandate: it ends stopped, by a human, and its
// Assignments stop. Stopping an ended Ticket changes nothing.
func (e *Engine) StopTicket(ticketID, reason string) error {
	return e.command(cmdStop, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil || t.Ended != nil {
			return err
		}
		if err := o.end(t, EndByHuman, OutcomeStopped, reason); err != nil {
			return err
		}
		return o.tx.PutTicket(t)
	})
}

// RetryTicket is a person's retry of an open Ticket stalled on a blocked or failed report or a failed delivery
// (CanRetry): the stalled Assignment ends and a new one starts under the same Mandate, receiving first the Inputs the
// old one had delivered but whose worker never read.
func (e *Engine) RetryTicket(ticketID string) error {
	return e.command(cmdRetry, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		latest := t.LatestAssignment()
		if latest == nil || !CanRetry(t) {
			return userErrorf("ticket %s is not waiting for a retry", ticketID)
		}
		replaced := latest.ID
		t.Stats.Retries++
		o.endAssignment(t, latest, reasonRetried)
		o.requeueUntaken(t, replaced)
		assignment := o.newAssignment(t)
		t.UpdatedAt = o.at
		o.stop(t, replaced, reasonRetried)
		o.start(t, assignment)
		return o.tx.PutTicket(t)
	})
}

// --- Queries ---

// Ticket returns the Ticket with the given ID and its stage.
func (e *Engine) Ticket(id string) (Ticket, Stage, error) {
	var t *Ticket
	err := e.repos.Tx(func(tx Tx) error {
		var err error
		t, err = tx.GetTicket(id)
		return err
	})
	if err != nil {
		return Ticket{}, Stage{}, err
	}
	if t == nil {
		return Ticket{}, Stage{}, userErrorf("ticket %s not found", id)
	}
	return *t, TicketStage(t), nil
}

// Tickets returns the open Tickets, or every Ticket when includeEnded is set, least recently updated first.
func (e *Engine) Tickets(includeEnded bool) ([]Ticket, error) {
	var list []*Ticket
	err := e.repos.Tx(func(tx Tx) error {
		var err error
		if includeEnded {
			list, err = tx.AllTickets()
		} else {
			list, err = tx.OpenTickets()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]Ticket, 0, len(list))
	for _, t := range list {
		out = append(out, *t)
	}
	return out, nil
}

// --- One command ---

type effectKind int

const (
	effectStart effectKind = iota
	effectDeliver
	effectStop
	effectEvent
	// effectRelease wakes the waiters of an Approval that is no longer pending.
	effectRelease
)

// effect is an Executor call or an Event a command produces; it runs after the command commits. Executor calls
// receive a snapshot of the Ticket as committed.
type effect struct {
	kind         effectKind
	ticket       *Ticket
	assignmentID string
	reason       string
	event        Event
	approvalID   string
}

// op is one command in progress: its transaction, its time and the effects it collected.
type op struct {
	tx      Tx
	at      time.Time
	effects []effect
	// policy is the Engine's Policy getter, nil without one.
	policy func() Policy
	// enqueued is set once the command enqueued external work; Wake runs after it committed.
	enqueued bool
	// relays is the Engine's Options.Relays; origin is who issued the command.
	relays bool
	origin CommandOrigin
	// reg is the Engine's registry; observations wait in metrics until the command committed.
	reg     *metrics.Registry
	metrics []func(*metrics.Registry)
}

// observe queues a metrics observation that runs once the command committed. Callers check o.reg first, so that no
// closure is made without metrics.
func (o *op) observe(f func(*metrics.Registry)) { o.metrics = append(o.metrics, f) }

// command runs the named command: fn in one transaction and, only once it committed, its effects in order. With
// metrics, its latency from arrival (lock wait included) to the last effect is recorded as engine_command_seconds.
func (e *Engine) command(name string, fn func(o *op) error) error {
	h := e.cmdHist[name]
	if h == nil {
		return e.run(fn)
	}
	start := time.Now()
	err := e.run(fn)
	h.Observe(time.Since(start))
	return err
}

func (e *Engine) run(fn func(o *op) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var o *op
	err := e.repos.Tx(func(tx Tx) error {
		// A fresh op per attempt, so nothing collected by a failed attempt leaks.
		o = &op{tx: tx, at: e.now(), reg: e.reg, policy: e.policy, relays: e.relays}
		return fn(o)
	})
	if err != nil {
		return err
	}
	for _, f := range o.metrics {
		f(e.reg)
	}
	for _, fx := range o.effects {
		if fx.kind == effectEvent {
			e.emit(fx.event)
			continue
		}
		if fx.kind == effectRelease {
			e.release(fx.approvalID)
			continue
		}
		t := cloneTicket(fx.ticket)
		var a Assignment
		if p := findAssignment(&t, fx.assignmentID); p != nil {
			a = *p
		}
		switch fx.kind {
		case effectStart:
			e.exec.Start(t, a)
		case effectDeliver:
			e.exec.Deliver(t, a)
		case effectStop:
			e.exec.Stop(t, a, fx.reason)
		}
	}
	if o.enqueued && e.wake != nil {
		e.wake()
	}
	return nil
}

func (o *op) timestamp() *time.Time {
	at := o.at
	return &at
}

func (o *op) start(t *Ticket, assignmentID string) {
	o.effects = append(o.effects, effect{kind: effectStart, ticket: t, assignmentID: assignmentID})
}

func (o *op) deliver(t *Ticket, assignmentID string) {
	o.effects = append(o.effects, effect{kind: effectDeliver, ticket: t, assignmentID: assignmentID})
}

func (o *op) stop(t *Ticket, assignmentID, reason string) {
	o.effects = append(o.effects, effect{kind: effectStop, ticket: t, assignmentID: assignmentID, reason: reason})
}

func (o *op) event(typ string, t *Ticket, assignmentID string, data map[string]any) {
	o.emitEvent(Event{Type: typ, TicketID: t.ID, AssignmentID: assignmentID, At: o.at, Data: data})
}

// emitEvent queues an Event that need not concern one Ticket.
func (o *op) emitEvent(ev Event) {
	o.effects = append(o.effects, effect{kind: effectEvent, event: ev})
}

// enqueue records external work for the effects runner in the command's transaction; the Engine's Wake runs once the
// command committed.
func (o *op) enqueue(kind, key string, payload []byte) error {
	if err := o.tx.Enqueue(kind, key, payload); err != nil {
		return err
	}
	o.enqueued = true
	return nil
}

// revalidates reports whether a Ticket's Mandate still holds under the current Policy before it resumes (an ended
// Ticket, or one whose worker handed off). An owner task's Mandate needs no Source, and without a Policy every Mandate
// holds.
func (o *op) revalidates(t *Ticket) bool {
	m := t.Mandate
	if o.policy == nil || m.PolicyVersion == OwnerPolicyVersion || m.Basis.Via == ViaOwner {
		return true
	}
	p := o.policy()
	return p == nil || p.Revalidate(m)
}

// require loads a Ticket, or fails with a UserError when there is none.
func (o *op) require(id string) (*Ticket, error) {
	t, err := o.tx.GetTicket(id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, userErrorf("ticket %s not found", id)
	}
	return t, nil
}

// running loads the Ticket while it is open and assignmentID runs it; nil, nil otherwise.
func (o *op) running(ticketID, assignmentID string) (*Ticket, error) {
	t, err := o.require(ticketID)
	if err != nil {
		return nil, err
	}
	if a := runningAssignment(t); t.Ended != nil || a == nil || a.ID != assignmentID {
		return nil, nil
	}
	return t, nil
}

// newAssignment adds a pending Assignment and returns its ID; a failed delivery to the one before no longer concerns
// the Ticket.
func (o *op) newAssignment(t *Ticket) string {
	a := Assignment{ID: newID(), State: AssignmentPending, CreatedAt: o.at}
	t.Assignments = append(t.Assignments, a)
	t.Delivery = nil
	return a.ID
}

func (o *op) endAssignment(t *Ticket, a *Assignment, reason string) {
	a.State = AssignmentEnded
	a.EndedAt = o.timestamp()
	a.EndedReason = reason
	o.event(EventAssignmentEnded, t, a.ID, map[string]any{"reason": reason})
}

// addInput adds an Input once and asks the Executor to deliver it, or to launch again a pending Assignment whose
// launch failed once no person is waited for. Automatic Inputs are held back while the worker waits for a person.
func (o *op) addInput(t *Ticket, in Input) {
	if findInput(t, in.ID) != nil {
		return
	}
	t.Inputs = append(t.Inputs, in)
	t.UpdatedAt = o.at
	live := t.Ended == nil
	held := live && automatic(&in) && t.WaitsForPerson()
	if !held {
		t.Undelivered = append(t.Undelivered, in.ID)
	}
	if !live || held {
		return
	}
	if r := runningAssignment(t); r != nil {
		o.deliver(t, r.ID)
	} else if l := t.LatestAssignment(); l != nil && l.State == AssignmentPending && !t.WaitsForPerson() {
		o.start(t, l.ID)
	}
}

// wake resumes an ended Ticket a person's message waits for, under its unchanged Mandate: no Decision, since nothing
// new is authorized. A Ticket a person stopped stays ended. The resume note goes ahead of the waiting messages.
func (o *op) wake(t *Ticket) {
	if t.Ended == nil || t.Ended.By == EndByHuman || len(t.Undelivered) == 0 {
		return
	}
	var first *Input
	for _, id := range t.Undelivered {
		if in := findInput(t, id); in != nil && in.Kind == InputTrigger && in.Human && !in.Context {
			first = in
			break
		}
	}
	if first == nil {
		return
	}
	triggerID := first.TriggerID
	t.Ended = nil
	t.UpdatedAt = o.at
	assignment := o.newAssignment(t)
	resumed := Input{ID: newID(), Kind: InputResumed, TriggerID: triggerID, At: o.at}
	t.Inputs = append(t.Inputs, resumed)
	t.Undelivered = append([]string{resumed.ID}, t.Undelivered...)
	o.start(t, assignment)
	o.event(EventTicketResumed, t, assignment, map[string]any{"triggerId": triggerID})
}

// requeueUntaken puts back, ahead of what waits, the trigger Inputs delivered to the replaced Assignment that no step
// of its worker read: a cancel may have dropped them.
func (o *op) requeueUntaken(t *Ticket, replaced string) {
	var untaken []string
	for _, in := range t.Inputs {
		if in.Kind == InputTrigger && in.ReadAt == nil && in.DeliveredTo == replaced && !slices.Contains(t.Undelivered, in.ID) {
			untaken = append(untaken, in.ID)
		}
	}
	t.Undelivered = append(untaken, t.Undelivered...)
}

// end ends an open Ticket with its outcome: its Assignments end and stop, and what waited for it, its question and a
// failed delivery are dropped, and its pending Approvals expire. reason says why in words; one equal to the outcome
// is not recorded.
func (o *op) end(t *Ticket, by TicketEndBy, outcome TicketOutcome, reason string) error {
	return o.endAt(t, by, outcome, reason, o.at)
}

// endAt is end with the time the ending fact happened, such as a pull request's close, which may be earlier than now;
// the Ticket's last change and its Assignments' ends are still now. Every end lets go of the person's follow-ups its
// worker did not read or take up, which go to Eval when no other Ticket decides them (see letGo).
func (o *op) endAt(t *Ticket, by TicketEndBy, outcome TicketOutcome, reason string, at time.Time) error {
	if t.Ended != nil {
		return nil
	}
	// What the Ticket lets go of is worked out while it is still live, as its own follow-ups.
	waiting, left, err := o.unread(t)
	if err != nil {
		return err
	}
	if err := o.expireTicketApprovals(t); err != nil {
		return err
	}
	o.settleAttention(t)
	var stopping []string
	for i := range t.Assignments {
		if a := &t.Assignments[i]; a.State != AssignmentEnded {
			a.State = AssignmentEnded
			a.EndedAt = o.timestamp()
			stopping = append(stopping, a.ID)
		}
	}
	ended := &Ended{At: at, By: by, Outcome: outcome}
	if reason != "" && reason != string(outcome) {
		ended.Reason = reason
	}
	t.Ended = ended
	t.Undelivered = nil
	t.UpdatedAt = o.at
	t.Attention = nil
	t.Delivery = nil
	if err := o.letGo(t, waiting, left); err != nil {
		return err
	}
	runtime, model := runtimeModel(t, t.LatestAssignment())
	if o.reg != nil {
		o.observe(func(r *metrics.Registry) {
			r.Counter("tickets_ended_total", "outcome", string(outcome), "runtime", runtime, "model", model).Add(1)
		})
	}
	data := map[string]any{"by": by, "outcome": outcome}
	if ended.Reason != "" {
		data["reason"] = ended.Reason
	}
	o.event(EventTicketEnded, t, "", data)
	if err := o.endNotice(t); err != nil {
		return err
	}
	stopReason := reason
	if stopReason == "" {
		stopReason = string(outcome)
	}
	for _, id := range stopping {
		o.stop(t, id, stopReason)
	}
	return nil
}

// --- Helpers ---

// automatic reports whether an Input is a message no person wrote, such as a bot's or a note.
func automatic(in *Input) bool {
	return in != nil && (in.Kind == InputNote || (in.Kind == InputTrigger && !in.Human))
}

// settleAttention adds the time the Ticket waited for a person's answer so far to its stats; the caller clears or
// replaces the Attention.
func (o *op) settleAttention(t *Ticket) {
	if a := t.Attention; a != nil && !a.At.IsZero() {
		t.Stats.PersonWaitTime += nonNegative(o.at.Sub(a.At))
	}
}

// hasReport reports whether any Assignment of the Ticket has reported.
func hasReport(t *Ticket) bool {
	for i := range t.Assignments {
		if t.Assignments[i].Report != nil {
			return true
		}
	}
	return false
}

func nonNegative(d time.Duration) time.Duration { return max(d, 0) }

// runtimeModel returns the metric labels of a Ticket's runtime and model. The model is the one the Executor ran the
// Assignment a with (nil for none), else the one the task named, else "default" (no Executor has run the Ticket).
func runtimeModel(t *Ticket, a *Assignment) (runtime, model string) {
	runtime, model = "unknown", "default"
	if x := t.Execution; x != nil {
		if x.Runtime != "" {
			runtime = x.Runtime
		}
		if x.Model != "" {
			model = x.Model
		}
	}
	if a != nil && a.Model != "" {
		model = a.Model
	}
	return runtime, model
}

func runningAssignment(t *Ticket) *Assignment {
	for i := range t.Assignments {
		if t.Assignments[i].State == AssignmentRunning {
			return &t.Assignments[i]
		}
	}
	return nil
}

func findAssignment(t *Ticket, id string) *Assignment {
	for i := range t.Assignments {
		if t.Assignments[i].ID == id {
			return &t.Assignments[i]
		}
	}
	return nil
}

func findInput(t *Ticket, id string) *Input {
	for i := range t.Inputs {
		if t.Inputs[i].ID == id {
			return &t.Inputs[i]
		}
	}
	return nil
}

// newID returns 12 random hex characters.
func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// titleOf returns a task's first non-blank-trimmed line, cut to titleMaxRunes.
func titleOf(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return truncateRunes(strings.TrimSpace(line), titleMaxRunes)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// cloneTicket returns a deep copy, so the Executor's snapshot shares nothing with the Engine's.
func cloneTicket(t *Ticket) Ticket {
	c := *t
	if t.Channels != nil {
		c.Channels = make([]ChannelRef, len(t.Channels))
		for i, ch := range t.Channels {
			ch.Address = maps.Clone(ch.Address)
			c.Channels[i] = ch
		}
	}
	c.Ended = clonePtr(t.Ended)
	c.Attention = clonePtr(t.Attention)
	if t.Assignments != nil {
		c.Assignments = make([]Assignment, len(t.Assignments))
		for i, a := range t.Assignments {
			a.StartedAt = clonePtr(a.StartedAt)
			a.EndedAt = clonePtr(a.EndedAt)
			a.DeliveredAt = clonePtr(a.DeliveredAt)
			a.IdleAt = clonePtr(a.IdleAt)
			a.Report = clonePtr(a.Report)
			c.Assignments[i] = a
		}
	}
	if t.Inputs != nil {
		c.Inputs = make([]Input, len(t.Inputs))
		for i, in := range t.Inputs {
			in.ReadAt = clonePtr(in.ReadAt)
			in.DeliveredAt = clonePtr(in.DeliveredAt)
			c.Inputs[i] = in
		}
	}
	c.Undelivered = slices.Clone(t.Undelivered)
	c.Origin.TriggerIDs = slices.Clone(t.Origin.TriggerIDs)
	c.Execution = clonePtr(t.Execution)
	c.Delivery = clonePtr(t.Delivery)
	c.SessionCosts = maps.Clone(t.SessionCosts)
	return c
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Trigger returns the stored Trigger with the given ID.
func (e *Engine) Trigger(id string) (Trigger, error) {
	var t *Trigger
	err := e.repos.Tx(func(tx Tx) error {
		var err error
		t, err = tx.GetTrigger(id)
		return err
	})
	if err != nil {
		return Trigger{}, err
	}
	if t == nil {
		return Trigger{}, userErrorf("trigger %s not found", id)
	}
	return *t, nil
}

// Thread returns the stored Thread with the given key.
func (e *Engine) Thread(key string) (Thread, error) {
	var th *Thread
	err := e.repos.Tx(func(tx Tx) error {
		var err error
		th, err = tx.GetThread(key)
		return err
	})
	if err != nil {
		return Thread{}, err
	}
	if th == nil {
		return Thread{}, userErrorf("thread %s not found", key)
	}
	return *th, nil
}

// Decisions returns the Decisions made on a Thread, or on every Thread when thread is empty, most recent first; limit
// <= 0 returns all.
func (e *Engine) Decisions(thread string, limit int) ([]Decision, error) {
	var list []*Decision
	err := e.repos.Tx(func(tx Tx) error {
		var err error
		if thread == "" {
			list, err = tx.RecentDecisions(limit)
		} else {
			list, err = tx.Decisions(thread, limit)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]Decision, 0, len(list))
	for _, d := range list {
		out = append(out, *d)
	}
	return out, nil
}
