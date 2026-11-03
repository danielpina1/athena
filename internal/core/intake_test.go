package core

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/metrics"
)

// Threads of one GitHub repository: an issue, two pull requests and an unrelated issue.
const (
	issue = "github:o/r#1"
	pull1 = "github:o/r#2"
	pull2 = "github:o/r#3"
	other = "github:o/r#9"
)

var alice = ActorRef{Source: "github", ID: "alice"}

// fakeScope listens to every Thread but the deaf ones.
type fakeScope struct{ deaf map[string]bool }

func (s *fakeScope) Listens(thread string) bool { return !s.deaf[thread] }

// fakePolicy revalidates every Mandate as holds says, and records what it was asked.
type fakePolicy struct {
	holds       bool
	revalidated []Mandate
}

func (p *fakePolicy) Version() string                     { return "v1" }
func (p *fakePolicy) EvalTimeout() time.Duration          { return time.Minute }
func (p *fakePolicy) Gate(Trigger, Thread, []Ticket) Gate { return Gate{} }
func (p *fakePolicy) Revalidate(m Mandate) bool {
	p.revalidated = append(p.revalidated, m)
	return p.holds
}
func (p *fakePolicy) AdmitsInferred([]string) bool { return false }
func (p *fakePolicy) Assess(Assessment, string) (string, []Reason, bool, error) {
	return "ignore", nil, false, nil
}

// intake is a harness whose Engine has a Source scope, a Policy and a Wake.
type intake struct {
	*harness
	scope  *fakeScope
	policy *fakePolicy
	wakes  atomic.Int32
}

func newIntake(t *testing.T) *intake {
	t.Helper()
	h := &intake{scope: &fakeScope{deaf: map[string]bool{}}, policy: &fakePolicy{holds: true}}
	h.harness = newHarnessWith(t, func(o *Options) {
		o.Scope = h.scope
		o.Policy = func() Policy { return h.policy }
		o.Wake = func() { h.wakes.Add(1) }
	})
	return h
}

// sourceTicket returns a running Ticket accepted from a person's request on thread: its Mandate names that Thread,
// and it subscribes to the Thread as its own.
func (h *intake) sourceTicket(thread string) string {
	h.t.Helper()
	id := h.accepted()
	h.must(h.repos.Tx(func(tx Tx) error {
		t, err := tx.GetTicket(id)
		if err != nil {
			return err
		}
		t.Mandate = Mandate{Strength: "explicit", PolicyVersion: "v1", Actor: alice, Scope: "issues", Thread: thread,
			Basis: MandateBasis{Via: ViaAddressed, Source: "github", Sender: "alice"}}
		return tx.PutTicket(t)
	}))
	h.subscribe(id, thread, false)
	return id
}

// subscribe subscribes a Ticket to a Thread directly, as an accept (or, with context, followForContext) would.
func (h *intake) subscribe(ticketID, thread string, context bool) {
	h.t.Helper()
	h.must(h.repos.Tx(func(tx Tx) error {
		th, err := tx.GetThread(thread)
		if err != nil {
			return err
		}
		if th == nil {
			th = &Thread{Key: thread, Source: "github"}
		}
		th.Subscriptions = append(th.Subscriptions, Subscription{TicketID: ticketID, Context: context})
		return tx.PutThread(th)
	}))
}

func (h *intake) receive(tr Trigger) string {
	h.t.Helper()
	got, err := h.eng.Receive(tr)
	if err != nil {
		h.t.Fatalf("Receive %s: %v", tr.ID, err)
	}
	return got
}

// message is a person's comment on threads.
func (h *intake) message(id string, threads ...string) Trigger {
	return Trigger{ID: id, Source: "github", Kind: KindMessage, Actor: alice, Human: true, Scope: "comments",
		Threads: threads, At: h.now(), Text: "please also fix " + id}
}

// botMessage is another bot's comment on threads.
func (h *intake) botMessage(id string, threads ...string) Trigger {
	return Trigger{ID: id, Source: "github", Kind: KindMessage, Actor: ActorRef{Source: "github", ID: "lint[bot]"}, Bot: true,
		Scope: "comments", Threads: threads, At: h.now(), Text: "coverage dropped"}
}

// ciRun is a CI result on threads.
func (h *intake) ciRun(id string, threads ...string) Trigger {
	return Trigger{ID: id, Source: "github", Kind: KindCI, Actor: ActorRef{Source: "github", ID: "github-actions"}, Scope: "ci",
		Threads: threads, At: h.now(), Text: "build failed"}
}

func (h *intake) queued(thread string) []string {
	if th := h.repos.thread(thread); th != nil {
		return th.Queued
	}
	return nil
}

func (h *intake) disposition(triggerID string) Disposition {
	h.t.Helper()
	tr := h.repos.trigger(triggerID)
	if tr == nil {
		h.t.Fatalf("trigger %s not stored", triggerID)
	}
	return tr.Disposition
}

// closePull closes a Thread the Ticket follows, at the current time.
func (h *intake) closePull(thread string, outcome TicketOutcome) {
	h.t.Helper()
	h.must(h.eng.ThreadClosed(thread, outcome, h.now()))
}

// inputsOf returns the trigger Inputs a Trigger gave a Ticket.
func inputsOf(t *Ticket, triggerID string) []Input {
	var out []Input
	for _, in := range t.Inputs {
		if in.Kind == InputTrigger && in.TriggerID == triggerID {
			out = append(out, in)
		}
	}
	return out
}

// eventsOf returns the emitted Events of one type.
func (h *harness) eventsOf(typ string) []Event {
	h.events.mu.Lock()
	defer h.events.mu.Unlock()
	var out []Event
	for _, e := range h.events.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// callsFor counts the Executor calls of one method for one Ticket.
func (h *harness) callsFor(method, ticketID string) int {
	h.exec.mu.Lock()
	defer h.exec.mu.Unlock()
	n := 0
	for _, c := range h.exec.calls {
		if c.Method == method && c.TicketID == ticketID {
			n++
		}
	}
	return n
}

// --- Receive ---

// Review Focus 1: the same Source event seen twice is one Trigger and reaches each Ticket once.
func TestReceiveDuplicate(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	tr := h.message("github:issuecomment:1", issue)
	eq(t, "first", h.receive(tr), "stored")
	again := tr
	again.Text = "edited"
	eq(t, "second", h.receive(again), "duplicate")
	eq(t, "stored text", h.repos.trigger(tr.ID).Text, tr.Text)
	eq(t, "inputs", len(inputsOf(h.ticket(a), tr.ID)), 1)
	eq(t, "received events", len(h.eventsOf(EventTriggerReceived)), 1)
	// A bot's message, which is queued for Eval, is queued once.
	b := h.botMessage("github:issuecomment:2", issue)
	h.receive(b)
	eq(t, "duplicate bot", h.receive(b), "duplicate")
	eq(t, "queued once", h.queued(issue), []string{b.ID})
}

func TestReceiveValidates(t *testing.T) {
	h := newIntake(t)
	_, err := h.eng.Receive(Trigger{ID: "x"})
	wantUserError(t, "no thread", err)
	_, err = h.eng.Receive(Trigger{Threads: []string{issue}})
	wantUserError(t, "no id", err)
	_, err = h.eng.Receive(Trigger{ID: "y", Threads: []string{"nosource"}})
	wantUserError(t, "bad thread key", err)
}

func TestReceiveFilteredWhenNotListening(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	h.scope.deaf[issue] = true
	tr := h.message("c:1", issue, other)
	eq(t, "filtered", h.receive(tr), "filtered")
	d := h.disposition(tr.ID)
	eq(t, "filtered code", d.Filtered, CodeNotListening)
	eq(t, "reached", d.Reached, []string(nil))
	eq(t, "no input", len(inputsOf(h.ticket(a), tr.ID)), 0)
	eq(t, "not queued", h.queued(issue), []string(nil))
	eq(t, "other thread not recorded", h.repos.thread(other), (*Thread)(nil))
	eq(t, "no event", len(h.eventsOf(EventTriggerReceived)), 0)
	eq(t, "again", h.receive(tr), "duplicate")
	// Only the primary Thread decides whether the Source listens.
	h.scope.deaf = map[string]bool{other: true}
	eq(t, "secondary deaf", h.receive(h.message("c:2", issue, other)), "stored")
}

// A plan 1 Engine, with no Source scope and no Policy, listens to everything.
func TestReceiveWithoutScopeListens(t *testing.T) {
	h := &intake{harness: newHarness(t)}
	tr := h.message("c:1", issue)
	eq(t, "stored", h.receive(tr), "stored")
	eq(t, "queued", h.queued(issue), []string{tr.ID})
}

func TestFanOutToSubscribedTickets(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	b := h.sourceTicket(pull1)
	h.subscribe(b, issue, false) // b subscribes to both Threads of the Trigger
	c := h.sourceTicket(other)
	stopped := h.sourceTicket(issue)
	h.must(h.eng.StopTicket(stopped, ""))
	beforeA, beforeB := h.callsFor("deliver", a), h.callsFor("deliver", b)

	tr := h.message("c:1", issue, pull1)
	tr.Meta = map[string]string{"repo": "o/r", "number": "1"}
	h.receive(tr)
	for _, id := range []string{a, b} {
		ins := inputsOf(h.ticket(id), tr.ID)
		if len(ins) != 1 {
			t.Fatalf("%s: %d inputs, want 1", id, len(ins))
		}
		eq(t, id+" input", ins[0], Input{ID: ins[0].ID, Kind: InputTrigger, At: h.now(), TriggerID: tr.ID, Text: tr.Text, Human: true})
		eq(t, id+" undelivered", h.ticket(id).Undelivered, []string{ins[0].ID})
	}
	eq(t, "deliver a", h.callsFor("deliver", a), beforeA+1)
	eq(t, "deliver b", h.callsFor("deliver", b), beforeB+1)
	eq(t, "unsubscribed ticket", len(inputsOf(h.ticket(c), tr.ID)), 0)
	eq(t, "stopped ticket", len(inputsOf(h.ticket(stopped), tr.ID)), 0)
	eq(t, "reached", h.disposition(tr.ID).Reached, []string{a, b})
	h.settle(false)
	eq(t, "delivered", h.ticket(a).Undelivered, []string(nil))

	// The Trigger is recorded on each of its Threads, which are created as needed; the primary one takes its meta.
	n := h.message("c:2", "github:o/r#50", issue)
	n.Meta = map[string]string{"repo": "o/r", "number": "50"}
	h.receive(n)
	created := h.repos.thread("github:o/r#50")
	if created == nil {
		t.Fatal("primary thread not created")
	}
	eq(t, "created source", created.Source, "github")
	eq(t, "created meta", created.Meta, map[string]string{"repo": "o/r", "number": "50"})
	eq(t, "secondary meta untouched", h.repos.thread(issue).Meta, map[string]string{"repo": "o/r", "number": "1"})
	eq(t, "first secondary has none", h.repos.thread(pull1).Meta, map[string]string(nil))
	eq(t, "issue subscriptions kept", len(h.repos.thread(issue).Subscriptions), 3)

	// A bot's message reaches them too, as no person's.
	bot := h.botMessage("c:3", issue)
	h.receive(bot)
	eq(t, "bot input human", inputsOf(h.ticket(a), bot.ID)[0].Human, false)
	ev := h.eventsOf(EventTriggerReceived)
	eq(t, "received events", len(ev), 3)
	eq(t, "event data", ev[0].Data, map[string]any{"triggerId": tr.ID, "threads": []string{issue, pull1}, "human": true,
		"reached": []string{a, b}, "queued": false})
}

// A person's message on a Ticket's own Thread stays that Ticket's: it takes no Eval.
func TestPersonFollowupOwnedNoEval(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	tr := h.message("c:1", issue)
	h.receive(tr)
	eq(t, "not queued", h.queued(issue), []string(nil))
	eq(t, "reached", h.disposition(tr.ID).Reached, []string{a})
	eq(t, "no wake without effects", h.wakes.Load(), int32(0))
	// One that reaches no Ticket goes to Eval on its primary Thread.
	lone := h.message("c:2", other)
	h.receive(lone)
	eq(t, "lone queued on primary", h.queued(other), []string{lone.ID})
	// One that reaches a on issue, a Thread of its own, stays a's whatever its primary Thread.
	via := h.message("c:3", other, issue)
	h.receive(via)
	eq(t, "owned via secondary", h.queued(other), []string{lone.ID})
	eq(t, "issue untouched", h.queued(issue), []string(nil))
	eq(t, "reached via secondary", h.disposition(via.ID).Reached, []string{a})
}

func TestReceiveQueuesWhenNoTicketOwns(t *testing.T) {
	h := newIntake(t)
	lone := h.message("c:1", other)
	h.receive(lone)
	eq(t, "queued", h.queued(other), []string{lone.ID})
	eq(t, "event", h.eventsOf(EventTriggerReceived)[0].Data["queued"], any(true))
}

// A Thread followed for context delivers its messages but gives no ownership, no release of a block and no resume.
func TestContextSubscriptionNeitherOwnsNorResumes(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	h.subscribe(a, other, true)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportBlocked, Summary: "needs a decision"}))
	h.tick(1)
	tr := h.message("c:1", other)
	h.receive(tr)
	ins := inputsOf(h.ticket(a), tr.ID)
	if len(ins) != 1 {
		t.Fatalf("%d inputs, want 1", len(ins))
	}
	eq(t, "context input", ins[0].Context, true)
	eq(t, "human", ins[0].Human, true)
	eq(t, "reached", h.disposition(tr.ID).Reached, []string{a})
	eq(t, "context", h.disposition(tr.ID).Context, []string{a})
	eq(t, "queued", h.queued(other), []string{tr.ID})
	eq(t, "still waits", h.ticket(a).WaitsForPerson(), true)
	// On both a context Thread and its own, the message is the Ticket's.
	both := h.message("c:2", other, issue)
	h.receive(both)
	eq(t, "both not context", inputsOf(h.ticket(a), both.ID)[0].Context, false)
	eq(t, "both context list", h.disposition(both.ID).Context, []string(nil))
	eq(t, "both owned", h.queued(other), []string{tr.ID})
	eq(t, "lifted", h.ticket(a).WaitsForPerson(), false)
	// An ended Ticket is not resumed from a context Thread.
	h.must(h.eng.FileReport(a, ReportInput{State: ReportReady, Summary: "done"}))
	late := h.message("c:3", other)
	h.receive(late)
	eq(t, "still ended", h.ticket(a).Ended != nil, true)
	eq(t, "no input", len(inputsOf(h.ticket(a), late.ID)), 0)
	eq(t, "late queued", h.queued(other), []string{tr.ID, late.ID})
}

// Review Focus 2: another bot's message, or CI, reaches the subscribed Tickets as context and still goes to Eval.
func TestBotMessageFansOutButQueuesForEval(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	bot := h.botMessage("c:1", issue)
	h.receive(bot)
	ci := h.ciRun("cs:1", issue)
	h.receive(ci)
	for _, id := range []string{bot.ID, ci.ID} {
		ins := inputsOf(h.ticket(a), id)
		if len(ins) != 1 || ins[0].Human {
			t.Fatalf("%s: inputs %+v, want one not human", id, ins)
		}
		eq(t, id+" reached", h.disposition(id).Reached, []string{a})
	}
	eq(t, "queued", h.queued(issue), []string{bot.ID, ci.ID})
	// While its worker waits for a person, a bot's message is held back.
	h.settle(false)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportBlocked, Summary: "stuck"}))
	h.tick(1)
	held := h.botMessage("c:2", issue)
	h.receive(held)
	in := inputsOf(h.ticket(a), held.ID)[0]
	eq(t, "held", slices.Contains(h.ticket(a).Undelivered, in.ID), false)
}

// Review Focus 5: a person's comment on an ended Ticket's merged PR resumes it under its Mandate with no Eval.
func TestPersonResumesEndedTicket(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	h.must(h.eng.BindThread(a, pull1, true))
	h.closePull(pull1, OutcomeMerged)
	eq(t, "merged", h.ticket(a).Ended.Outcome, OutcomeMerged)
	mandate := h.ticket(a).Mandate
	h.tick(5)
	tr := h.message("c:1", pull1)
	h.receive(tr)
	tk := h.ticket(a)
	if tk.Ended != nil {
		t.Fatalf("still ended: %+v", tk.Ended)
	}
	eq(t, "revalidated", h.policy.revalidated, []Mandate{mandate})
	eq(t, "mandate unchanged", tk.Mandate, mandate)
	eq(t, "assignments", assignmentStates(tk), []AssignmentState{AssignmentEnded, AssignmentPending})
	eq(t, "started", h.exec.last("start").AssignmentID, tk.LatestAssignment().ID)
	in := inputsOf(tk, tr.ID)[0]
	resumed := tk.Inputs[len(tk.Inputs)-1]
	eq(t, "resumed note", resumed.Kind, InputResumed)
	eq(t, "resumed trigger", resumed.TriggerID, tr.ID)
	eq(t, "undelivered", tk.Undelivered, []string{resumed.ID, in.ID})
	eq(t, "not queued", h.queued(pull1), []string(nil))
	eq(t, "reached", h.disposition(tr.ID).Reached, []string{a})
	ev := h.eventsOf(EventTicketResumed)
	if len(ev) != 1 || ev[0].TicketID != a || ev[0].Data["triggerId"] != tr.ID {
		t.Fatalf("resumed events %+v", ev)
	}
	h.settle(true)
	eq(t, "delivered", h.ticket(a).Undelivered, []string(nil))
	eq(t, "still follows the merged PR", h.ticket(a).Follows[0].Closed != nil, true)
}

func TestBotDoesNotResumeEndedTicket(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	h.must(h.eng.BindThread(a, pull1, true))
	h.closePull(pull1, OutcomeMerged)
	starts := h.callsFor("start", a)
	h.tick(5)
	bot := h.botMessage("c:1", pull1)
	h.receive(bot)
	ci := h.ciRun("cs:1", pull1)
	h.receive(ci)
	tk := h.ticket(a)
	eq(t, "still ended", tk.Ended != nil, true)
	eq(t, "no inputs", len(inputsOf(tk, bot.ID))+len(inputsOf(tk, ci.ID)), 0)
	eq(t, "no start", h.callsFor("start", a), starts)
	eq(t, "not reached", h.disposition(bot.ID).Reached, []string(nil))
	eq(t, "queued", h.queued(pull1), []string{bot.ID, ci.ID})
	eq(t, "not revalidated", len(h.policy.revalidated), 0)
}

func TestStoppedTicketReceivesNothing(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	h.must(h.eng.StopTicket(a, "not needed"))
	tr := h.message("c:1", issue)
	h.receive(tr)
	bot := h.botMessage("c:2", issue)
	h.receive(bot)
	tk := h.ticket(a)
	eq(t, "stopped", tk.Ended.Outcome, OutcomeStopped)
	eq(t, "no inputs", len(inputsOf(tk, tr.ID))+len(inputsOf(tk, bot.ID)), 0)
	eq(t, "not reached", h.disposition(tr.ID).Reached, []string(nil))
	eq(t, "queued", h.queued(issue), []string{tr.ID, bot.ID})
	eq(t, "no ticket.resumed", len(h.eventsOf(EventTicketResumed)), 0)
}

// A Mandate that no longer holds keeps the Ticket ended; the message reaches no Ticket and goes to Eval.
func TestResumeRefusedWhenRevalidationFails(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportReady, Summary: "done"}))
	starts := h.callsFor("start", a)
	h.policy.holds = false
	tr := h.message("c:1", issue)
	h.receive(tr)
	tk := h.ticket(a)
	eq(t, "still ended", tk.Ended.Outcome, OutcomeCompleted)
	eq(t, "revalidated", len(h.policy.revalidated), 1)
	eq(t, "no input", len(inputsOf(tk, tr.ID)), 0)
	eq(t, "nothing waits", tk.Undelivered, []string(nil))
	eq(t, "no start", h.callsFor("start", a), starts)
	eq(t, "not reached", h.disposition(tr.ID).Reached, []string(nil))
	eq(t, "queued", h.queued(issue), []string{tr.ID})
	// Once it holds again, a person's message resumes it.
	h.policy.holds = true
	h.receive(h.message("c:2", issue))
	eq(t, "resumed", h.ticket(a).Ended, (*Ended)(nil))
}

// The owner's message resumes a Source's ended Ticket only while its Mandate holds; an owner task needs no Source.
func TestPersonInputRevalidatesSourceMandate(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportReady, Summary: "done"}))
	h.policy.holds = false
	wantUserError(t, "revoked", h.eng.AddPersonInput(a, "one more thing"))
	eq(t, "still ended", h.ticket(a).Ended != nil, true)
	owner := h.accepted()
	h.must(h.eng.FileReport(owner, ReportInput{State: ReportReady, Summary: "done"}))
	h.must(h.eng.AddPersonInput(owner, "one more thing"))
	eq(t, "owner task resumed", h.ticket(owner).Ended, (*Ended)(nil))
}

// --- BindThread and ThreadClosed ---

func TestBindThreadFollowsAndSubscribes(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	h.must(h.eng.BindThread(a, pull1, true))
	h.must(h.eng.BindThread(a, pull2, true))
	h.must(h.eng.BindThread(a, issue, false)) // already subscribed: now also followed
	eq(t, "follows", h.ticket(a).Follows, []Follow{{Thread: pull1, Own: true}, {Thread: pull2, Own: true}, {Thread: issue}})
	eq(t, "pull subscription", h.repos.thread(pull1).Subscriptions, []Subscription{{TicketID: a, Bound: true, Own: true}})
	eq(t, "issue subscription", h.repos.thread(issue).Subscriptions, []Subscription{{TicketID: a, Bound: true}})
	ev := h.eventsOf(EventThreadSubscribed)
	eq(t, "subscribed events", len(ev), 2)
	eq(t, "event", ev[0].TicketID, a)
	eq(t, "event thread", ev[0].Data, map[string]any{"thread": pull1, "own": true})

	// A closed Thread bound again is open again; own stays once set.
	h.closePull(pull1, OutcomeClosed)
	eq(t, "open: pull2 is", h.ticket(a).Ended, (*Ended)(nil))
	eq(t, "thread closed", h.repos.thread(pull1).Closed != nil, true)
	h.must(h.eng.BindThread(a, pull1, false))
	eq(t, "reopened follow", h.ticket(a).Follows[0], Follow{Thread: pull1, Own: true})
	eq(t, "reopened thread", h.repos.thread(pull1).Closed, (*Close)(nil))
	eq(t, "one subscription", len(h.repos.thread(pull1).Subscriptions), 1)
	eq(t, "no new event", len(h.eventsOf(EventThreadSubscribed)), 2)

	// Binding a Thread followed for context makes it the Ticket's.
	h.subscribe(a, other, true)
	h.must(h.eng.BindThread(a, other, false))
	eq(t, "context lifted", h.repos.thread(other).Subscriptions, []Subscription{{TicketID: a, Bound: true}})

	wantUserError(t, "unknown ticket", h.eng.BindThread("T-9999", pull1, true))
	wantUserError(t, "bad thread", h.eng.BindThread(a, "", true))
	h.must(h.eng.StopTicket(a, ""))
	wantUserError(t, "ended ticket", h.eng.BindThread(a, "github:o/r#4", true))
}

// Two own PRs: one merged leaves the Ticket open; the second closed ends it closed, when it closed.
func TestThreadClosedEndsWhenAllOwnPullsClosed(t *testing.T) {
	h := newIntake(t)
	a := h.sourceTicket(issue)
	h.must(h.eng.BindThread(a, issue, false))
	h.must(h.eng.BindThread(a, pull1, true))
	h.must(h.eng.BindThread(a, pull2, true))
	running := runningOf(h.ticket(a)).ID
	first := h.now()
	h.closePull(pull1, OutcomeMerged)
	tk := h.ticket(a)
	eq(t, "open", tk.Ended, (*Ended)(nil))
	eq(t, "close recorded", tk.Follows[1].Closed, &Close{At: first, Outcome: OutcomeMerged})
	h.tick(10)
	second := h.now()
	h.tick(5) // the close is reported after it happened
	h.must(h.eng.ThreadClosed(pull2, OutcomeClosed, second))
	tk = h.ticket(a)
	if tk.Ended == nil {
		t.Fatal("not ended")
	}
	eq(t, "ended", *tk.Ended, Ended{At: second, By: EndByExecutor, Outcome: OutcomeClosed})
	eq(t, "updated at", tk.UpdatedAt, h.now())
	eq(t, "issue still open", tk.Follows[0].Closed, (*Close)(nil))
	eq(t, "assignment stopped", h.exec.last("stop").AssignmentID, running)
	ended := len(h.eventsOf(EventTicketEnded))
	h.must(h.eng.ThreadClosed(pull2, OutcomeClosed, second))
	eq(t, "closing again changes nothing", len(h.eventsOf(EventTicketEnded)), ended)
	wantUserError(t, "bad outcome", h.eng.ThreadClosed(pull1, OutcomeCompleted, second))

	// A Ticket that opened no PR ends once every one it follows closed, as the latest did; its ready report does not end it.
	b := h.sourceTicket(other)
	h.must(h.eng.BindThread(b, "github:o/r#20", false))
	h.must(h.eng.BindThread(b, "github:o/r#21", false))
	h.must(h.eng.FileReport(b, ReportInput{State: ReportReady, Summary: "pushed to their branch"}))
	eq(t, "ready does not end", h.ticket(b).Ended, (*Ended)(nil))
	eq(t, "awaiting review", TicketStage(h.ticket(b)), Stage{"person", "review"})
	h.tick(10)
	late := h.now()
	h.must(h.eng.ThreadClosed("github:o/r#21", OutcomeMerged, late))
	h.must(h.eng.ThreadClosed("github:o/r#20", OutcomeClosed, late.Add(-time.Second)))
	eq(t, "ended as the latest close", h.ticket(b).Ended.Outcome, OutcomeMerged)
	eq(t, "ended at the latest close", h.ticket(b).Ended.At, late)
}

// A close does not end a Ticket while work resumed after it is unfinished; its ready report then ends it as the close did.
func TestCloseWaitsForUnfinishedResumedWork(t *testing.T) {
	h := newIntake(t)
	// Retried after its PR closed, before the close was reported.
	a := h.sourceTicket(issue)
	h.must(h.eng.BindThread(a, pull1, true))
	h.must(h.eng.FileReport(a, ReportInput{State: ReportFailed, Summary: "tests fail"}))
	closedAt := h.now().Add(5 * time.Second)
	h.tick(10)
	h.must(h.eng.RetryTicket(a))
	h.settle(true)
	h.must(h.eng.ThreadClosed(pull1, OutcomeClosed, closedAt))
	eq(t, "deferred", h.ticket(a).Ended, (*Ended)(nil))
	eq(t, "close recorded", h.ticket(a).Follows[0].Closed, &Close{At: closedAt, Outcome: OutcomeClosed})
	h.tick(1)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportReady, Summary: "done"}))
	eq(t, "ready ends as the close", h.ticket(a).Ended.Outcome, OutcomeClosed)

	// Resumed by a person after its merge: its ready report ends it merged again.
	b := h.sourceTicket(other)
	h.must(h.eng.BindThread(b, pull2, true))
	h.closePull(pull2, OutcomeMerged)
	h.tick(10)
	h.receive(h.message("c:1", pull2))
	h.settle(true)
	eq(t, "resumed", h.ticket(b).Ended, (*Ended)(nil))
	h.must(h.eng.FileReport(b, ReportInput{State: ReportReady, Summary: "answered"}))
	eq(t, "ended merged again", h.ticket(b).Ended.Outcome, OutcomeMerged)

	// A deferred close, then dropped work: it ends as the close did.
	c := h.sourceTicket(issue)
	h.must(h.eng.BindThread(c, "github:o/r#30", true))
	h.must(h.eng.FileReport(c, ReportInput{State: ReportFailed, Summary: "tests fail"}))
	closedAt = h.now()
	h.tick(10)
	h.must(h.eng.RetryTicket(c))
	h.settle(true)
	h.must(h.eng.ThreadClosed("github:o/r#30", OutcomeMerged, closedAt))
	eq(t, "deferred again", h.ticket(c).Ended, (*Ended)(nil))
	h.must(h.eng.FileReport(c, ReportInput{State: ReportDropped, Summary: "nothing left"}))
	eq(t, "dropped ends as the close", h.ticket(c).Ended.Outcome, OutcomeMerged)

	// A Ticket whose worker acknowledged a person's follow-up it has not read through a turn waits too.
	d := h.sourceTicket("github:o/r#40")
	h.must(h.eng.BindThread(d, "github:o/r#41", true))
	h.must(h.eng.FileReport(d, ReportInput{State: ReportReady, Summary: "PR ready"}))
	h.tick(1)
	fu := h.message("c:2", "github:o/r#40")
	h.receive(fu)
	h.settle(true)
	h.read(d)
	h.must(h.repos.Tx(func(tx Tx) error { // the worker acknowledged it (Replies records this in Task 6)
		tr, err := tx.GetTrigger(fu.ID)
		if err != nil {
			return err
		}
		tr.Disposition.Acknowledged = []string{d}
		return tx.PutTrigger(tr)
	}))
	h.tick(1)
	h.must(h.eng.FileReport(d, ReportInput{State: ReportProgress, Summary: "working on it"}))
	h.closePull("github:o/r#41", OutcomeMerged)
	eq(t, "taken-up follow-up defers", h.ticket(d).Ended, (*Ended)(nil))
	h.tick(1)
	h.must(h.eng.FileReport(d, ReportInput{State: ReportReady, Summary: "done"}))
	eq(t, "then ready ends it", h.ticket(d).Ended.Outcome, OutcomeMerged)
}

func TestFollowsDone(t *testing.T) {
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	closed := func(s int, o TicketOutcome) *Close { return &Close{At: at(s), Outcome: o} }
	cases := []struct {
		name    string
		follows []Follow
		want    TicketOutcome
		done    bool
	}{
		{"none", nil, "", false},
		{"own open", []Follow{{Thread: "a", Own: true}}, "", false},
		{"own closed, other open", []Follow{{Thread: "a", Own: true, Closed: closed(1, OutcomeMerged)}, {Thread: "b"}}, OutcomeMerged, true},
		{"one own of two open", []Follow{{Thread: "a", Own: true, Closed: closed(1, OutcomeMerged)}, {Thread: "b", Own: true}}, "", false},
		{"latest own close wins", []Follow{{Thread: "a", Own: true, Closed: closed(2, OutcomeClosed)}, {Thread: "b", Own: true, Closed: closed(1, OutcomeMerged)}}, OutcomeClosed, true},
		{"no own, one open", []Follow{{Thread: "a", Closed: closed(1, OutcomeMerged)}, {Thread: "b"}}, "", false},
		{"no own, all closed", []Follow{{Thread: "a", Closed: closed(3, OutcomeMerged)}, {Thread: "b", Closed: closed(1, OutcomeClosed)}}, OutcomeMerged, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tk := &Ticket{Follows: c.follows}
			got, done := tk.FollowsDone()
			if got != c.want || done != c.done {
				t.Fatalf("FollowsDone = %q, %v; want %q, %v", got, done, c.want, c.done)
			}
		})
	}
}

// --- Options ---

// A command that enqueued an effect wakes the effects runner once it committed, and only then.
func TestWakeAfterEnqueue(t *testing.T) {
	h := newIntake(t)
	h.must(h.eng.command("test", func(o *op) error { return o.enqueue("reply", "k1", []byte("{}")) }))
	eq(t, "woken", h.wakes.Load(), int32(1))
	h.must(h.eng.command("test", func(o *op) error { return nil }))
	eq(t, "nothing enqueued", h.wakes.Load(), int32(1))
	err := h.eng.command("test", func(o *op) error {
		if err := o.enqueue("reply", "k2", nil); err != nil {
			return err
		}
		return &UserError{Msg: "refused"}
	})
	wantUserError(t, "rolled back", err)
	eq(t, "not woken on rollback", h.wakes.Load(), int32(1))
	eq(t, "one effect", len(h.repos.effectList()), 1)
}

func TestReceiveCommandMetric(t *testing.T) {
	h := newIntake(t)
	reg := metrics.NewRegistry()
	h.eng.SetMetrics(reg)
	h.receive(h.message("c:1", issue))
	h.must(h.eng.ThreadClosed(issue, OutcomeClosed, h.now()))
	got := map[string]int64{}
	for _, hv := range reg.Snapshot(0).Histograms {
		got[hv.Labels["command"]] += hv.Count
	}
	eq(t, "receive", got[cmdReceive], int64(1))
	eq(t, "thread_closed", got[cmdThreadClosed], int64(1))
}
