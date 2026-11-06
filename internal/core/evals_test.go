package core

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/metrics"
)

// evalPolicy is a Policy whose gate admits by simple rules (or a per-Trigger override), whose version tests change,
// and whose assessment follows the accept mode as the policy package's does.
type evalPolicy struct {
	version   string
	mode      string
	gates     map[string]Gate
	holds     bool
	assessErr error
	// trust is the class the policy gives every Trigger.
	trust string
	// ceiling is the WIP ceiling of every repository; 3 when 0.
	ceiling int
	// shadow names the repositories held for the owner.
	shadow map[string]bool
}

func newEvalPolicy() *evalPolicy {
	return &evalPolicy{version: "v1", mode: "observe", gates: map[string]Gate{}, holds: true}
}

func (p *evalPolicy) Version() string            { return p.version }
func (p *evalPolicy) EvalTimeout() time.Duration { return time.Minute }

// Gate: a person addressing the agent is explicit; a person on a Thread with Tickets is an explicit follow-up; a bot
// is an inferred candidate; anyone else is not admitted.
func (p *evalPolicy) Gate(t Trigger, _ Thread, tickets []Ticket) Gate {
	if g, ok := p.gates[t.ID]; ok {
		return g
	}
	switch {
	case t.Human && t.Addressed != "":
		return Gate{Strength: "explicit", Basis: MandateBasis{Via: ViaAddressed, Source: t.Source, Scope: t.Scope, Sender: t.Actor.ID}}
	case t.Human && len(tickets) > 0:
		return Gate{Strength: "explicit", Basis: MandateBasis{Via: ViaFollowup, Source: t.Source, Scope: t.Scope}}
	case t.Bot:
		return Gate{Strength: "inferred", Basis: MandateBasis{Via: ViaBot, Source: t.Source, Scope: t.Scope, Sender: t.Actor.ID}}
	}
	return Gate{Reason: Reason{Code: "not_a_member", TriggerID: t.ID}}
}

func (p *evalPolicy) Revalidate(Mandate) bool { return p.holds }

func (p *evalPolicy) AdmitsInferred(rules []string) bool { return p.mode == "auto" && len(rules) > 0 }
func (p *evalPolicy) WIPCeiling(string) int {
	if p.ceiling > 0 {
		return p.ceiling
	}
	return 3
}
func (p *evalPolicy) Shadow(repo string) bool { return p.shadow[repo] }
func (p *evalPolicy) Trust(Trigger) string    { return p.trust }

func (p *evalPolicy) Assess(a Assessment, strength string) (string, []Reason, bool, error) {
	if p.assessErr != nil {
		return "", nil, false, p.assessErr
	}
	if strength == "explicit" || !a.Accept {
		if a.Accept {
			return "accept", nil, false, nil
		}
		return "ignore", nil, false, nil
	}
	if !slices.ContainsFunc(a.Rules, func(j RuleJudgement) bool { return j.Matches }) {
		return "ignore", []Reason{{Code: "no_matching_rule"}}, false, nil
	}
	switch p.mode {
	case "auto":
		return "accept", nil, false, nil
	case "observe":
		return "ignore", []Reason{{Code: "would_accept"}}, true, nil
	}
	return "ignore", []Reason{{Code: "mode_off"}}, false, nil
}

// evals is an intake harness whose Policy is an evalPolicy.
type evals struct {
	*intake
	pol *evalPolicy
}

func newEvals(t *testing.T) *evals {
	t.Helper()
	return newEvalsWith(t, nil)
}

// newEvalsWith is newEvals with more Engine Options, which configure sets.
func newEvalsWith(t *testing.T, configure func(*Options)) *evals {
	t.Helper()
	h := &evals{pol: newEvalPolicy()}
	in := &intake{scope: &fakeScope{deaf: map[string]bool{}}, policy: &fakePolicy{holds: true}}
	in.harness = newHarnessWith(t, func(o *Options) {
		o.Scope = in.scope
		o.Policy = func() Policy { return h.pol }
		o.Wake = func() { in.wakes.Add(1) }
		if configure != nil {
			configure(o)
		}
	})
	h.intake = in
	return h
}

// request is a person's comment that mentions the agent, on threads; the primary Thread is in repository o/r.
func (h *evals) request(id string, threads ...string) Trigger {
	tr := h.message(id, threads...)
	tr.Addressed = AddressedMention
	tr.Text = "@athena please fix " + id
	tr.Meta = map[string]string{"repo": "o/r"}
	return tr
}

func (h *evals) due() {
	h.t.Helper()
	h.must(h.eng.EvaluateDue())
}

func (h *evals) evalOf(thread string) *Evaluation {
	if th := h.repos.thread(thread); th != nil {
		return th.Eval
	}
	return nil
}

func (h *evals) decide(d DecisionInput) DecisionResult {
	h.t.Helper()
	r, err := h.eng.Decide(d)
	if err != nil {
		h.t.Fatalf("Decide %+v: %v", d, err)
	}
	return r
}

// acceptRequest receives a person's request on thread, evaluates it and accepts it; it returns the new Ticket's ID.
func (h *evals) acceptRequest(id, thread string) string {
	h.t.Helper()
	h.receive(h.request(id, thread))
	h.due()
	r := h.decide(DecisionInput{TriggerID: id, Kind: "accept", Title: "Fix " + id})
	if r.Ticket == nil {
		h.t.Fatalf("accept of %s created no ticket: %+v", id, r)
	}
	h.settle(true)
	return r.Ticket.ID
}

func (h *evals) decisionsOn(thread string) []Decision {
	var out []Decision
	for _, d := range h.repos.decisionList() {
		if d.Thread == thread {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b Decision) int { return a.At.Compare(b.At) })
	return out
}

func codes(rs []Reason) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Code)
	}
	return out
}

// --- EvaluateDue ---

func TestBatchOnePerThread(t *testing.T) {
	h := newEvals(t)
	bot := h.botMessage("c:1", issue) // inferred
	h.receive(bot)
	h.tick(1)
	h.receive(h.request("c:2", issue))
	h.tick(1)
	h.receive(h.request("c:3", issue)) // the latest explicit one is the primary
	h.receive(h.request("c:9", other))
	h.due()
	ev := h.evalOf(issue)
	if ev == nil {
		t.Fatal("no eval on the issue")
	}
	eq(t, "batch", ev.TriggerIDs, []string{"c:1", "c:2", "c:3"})
	eq(t, "primary", ev.Primary, "c:3")
	eq(t, "strength", ev.Strength, "explicit")
	eq(t, "basis", ev.Basis.Via, ViaAddressed)
	eq(t, "version", ev.PolicyVersion, "v1")
	eq(t, "started", ev.StartedAt, h.now())
	eq(t, "queue emptied", h.queued(issue), []string(nil))
	eq(t, "other thread has its own eval", h.evalOf(other).TriggerIDs, []string{"c:9"})
	var fx []fakeEffect
	for _, e := range h.repos.effectList() {
		if e.Kind == "assessment" {
			fx = append(fx, e)
		}
	}
	eq(t, "assessment effects", len(fx), 2)
	byKey := map[string]fakeEffect{}
	for _, e := range fx {
		byKey[e.Key] = e
	}
	var payload struct{ EvalID, TriggerID string }
	if err := json.Unmarshal(byKey[ev.ID].Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	eq(t, "payload", payload, struct{ EvalID, TriggerID string }{ev.ID, "c:3"})
	if h.wakes.Load() == 0 {
		t.Error("enqueuing the assessment did not wake the runner")
	}
	eq(t, "eval.started events", len(h.eventsOf(EventEvalStarted)), 2)

	// A fourth Trigger arriving meanwhile waits for the next Eval.
	h.receive(h.request("c:4", issue))
	h.due()
	eq(t, "same eval", h.evalOf(issue).ID, ev.ID)
	eq(t, "fourth queued", h.queued(issue), []string{"c:4"})
	h.decide(DecisionInput{TriggerID: "c:2", Kind: "ignore", Reason: "duplicate"})
	eq(t, "still queued after the decision", h.queued(issue), []string{"c:4"})
	h.due()
	eq(t, "next eval holds the fourth", h.evalOf(issue).TriggerIDs, []string{"c:4"})

	// Without a Policy, EvaluateDue does nothing.
	plain := newHarness(t)
	plain.must(plain.eng.EvaluateDue())
}

func TestNotAdmittedIgnoredWithoutRouter(t *testing.T) {
	h := newEvals(t)
	h.receive(h.message("c:1", issue)) // an unaddressed person, not admitted
	h.receive(h.ciRun("ci:1", issue))
	h.due()
	eq(t, "no eval", h.evalOf(issue), (*Evaluation)(nil))
	eq(t, "queue emptied", h.queued(issue), []string(nil))
	eq(t, "no effect", len(h.repos.effectList()), 0)
	ds := h.decisionsOn(issue)
	if len(ds) != 1 {
		t.Fatalf("decisions = %+v", ds)
	}
	d := ds[0]
	eq(t, "kind", d.Kind, "ignore")
	eq(t, "triggers", d.TriggerIDs, []string{"c:1", "ci:1"})
	eq(t, "reasons", d.Reasons, []Reason{{Code: "not_a_member", TriggerID: "c:1"}, {Code: "not_a_member", TriggerID: "ci:1"}})
	eq(t, "thread lists it", h.repos.thread(issue).Decisions, []string{d.ID})
	eq(t, "decision.recorded", len(h.eventsOf(EventDecisionRecorded)), 1)
	eq(t, "no eval started", len(h.eventsOf(EventEvalStarted)), 0)
}

// --- Decide: accept ---

func TestAcceptCreatesTicketMandateSubscriptions(t *testing.T) {
	h := newEvals(t)
	h.receive(h.request("c:1", issue))
	h.tick(1)
	second := h.request("c:2", issue, other)
	second.Channels = []ChannelRef{{Source: "github", Thread: issue}}
	h.receive(second)
	h.due()
	ev := *h.evalOf(issue)
	r := h.decide(DecisionInput{TriggerID: "c:1", Kind: "accept", Reason: "clear request", Title: "  Fix the login page\nmore  ",
		Ack: "On it."})
	eq(t, "refused", r.Refused, "")
	if r.Ticket == nil {
		t.Fatal("no ticket")
	}
	eq(t, "assignment pending", assignmentStates(r.Ticket), []AssignmentState{AssignmentPending})
	h.settle(true)
	tk := h.ticket(r.Ticket.ID)
	eq(t, "title", tk.Title, "Fix the login page")
	eq(t, "repo", tk.Repo, "o/r")
	eq(t, "decision id", tk.DecisionID, r.Decision.ID)
	eq(t, "mandate", tk.Mandate, Mandate{Strength: "explicit", PolicyVersion: "v1", Actor: alice, Thread: issue,
		Basis: MandateBasis{Via: ViaAddressed, Source: "github", Scope: "comments", Sender: "alice"}})
	eq(t, "origin", tk.Origin, Origin{TriggerID: "c:2", TriggerIDs: []string{"c:1", "c:2"}})
	eq(t, "channels", tk.Channels, []ChannelRef{{Source: "github", Thread: issue}})
	eq(t, "no follows", tk.Follows, []Follow(nil))
	var inputs []string
	for _, in := range tk.Inputs {
		eq(t, "input kind", in.Kind, InputTrigger)
		eq(t, "human", in.Human, true)
		inputs = append(inputs, in.TriggerID)
	}
	eq(t, "primary first", inputs, []string{"c:2", "c:1"})
	eq(t, "assignment running", assignmentStates(tk), []AssignmentState{AssignmentRunning})
	eq(t, "started once", h.callsFor("start", tk.ID), 1)

	d := r.Decision
	eq(t, "kind", d.Kind, "accept")
	eq(t, "ticket", d.TicketID, tk.ID)
	eq(t, "thread", d.Thread, issue)
	eq(t, "triggers", d.TriggerIDs, ev.TriggerIDs)
	eq(t, "ack", d.Ack, "On it.")
	eq(t, "decision mandate", *d.Mandate, tk.Mandate)
	eq(t, "reason", d.Reasons, []Reason{{Code: CodeSaid, Values: map[string]string{"text": "clear request"}}})
	eq(t, "eval ended", h.evalOf(issue), (*Evaluation)(nil))
	fx := h.repos.effectList()
	eq(t, "the assessment, then the acknowledgement", len(fx), 2)
	eq(t, "ack queued (R4)", fx[len(fx)-1].Key, ReceiptID(tk.ID, PurposeAcknowledgement, "c:2", "On it."))

	iss, oth := h.repos.thread(issue), h.repos.thread(other)
	eq(t, "issue subscription", iss.Subscriptions, []Subscription{{TicketID: tk.ID, Own: true}})
	eq(t, "other subscription", oth.Subscriptions, []Subscription{{TicketID: tk.ID}})
	eq(t, "ticket.created", len(h.eventsOf(EventTicketCreated)), 1)

	// A follow-up on the issue now reaches the Ticket and stays its own.
	h.receive(h.message("c:3", issue))
	eq(t, "follow-up owned", h.queued(issue), []string(nil))
	eq(t, "follow-up reached", len(inputsOf(h.ticket(tk.ID), "c:3")), 1)

	// An inferred accept records the rules its assessment matched.
	h.pol.mode = "auto"
	h.receive(h.botMessage("b:1", pull1))
	h.due()
	a := &Assessment{Accept: true, Rules: []RuleJudgement{{Name: "deps", Matches: true, Evidence: "bump"}, {Name: "docs"}}}
	r = h.decide(DecisionInput{TriggerID: "b:1", Kind: "accept", Title: "Bump deps", Assessment: a})
	if r.Ticket == nil {
		t.Fatalf("inferred accept: %+v", r)
	}
	eq(t, "inferred mandate", r.Ticket.Mandate.Strength, "inferred")
	eq(t, "matched rules", r.Ticket.Mandate.Basis.Rules, []string{"deps"})
	eq(t, "bot basis", r.Ticket.Mandate.Basis.Via, ViaBot)

	// An accept needs a title.
	h.receive(h.request("c:5", pull2))
	h.due()
	r = h.decide(DecisionInput{TriggerID: "c:5", Kind: "accept", Title: "  "})
	if r.Refused == "" || r.Ticket != nil {
		t.Errorf("untitled accept: %+v", r)
	}
	if h.evalOf(pull2) == nil {
		t.Error("a refused accept ended the eval")
	}
}

// Review Focus 1: accepting the same Trigger twice makes one Ticket.
func TestAcceptTwiceOneTicket(t *testing.T) {
	h := newEvals(t)
	h.receive(h.request("c:1", issue))
	h.due()
	first := h.decide(DecisionInput{TriggerID: "c:1", Kind: "accept", Title: "Fix it"})
	second := h.decide(DecisionInput{TriggerID: "c:1", Kind: "accept", Title: "Fix it again"})
	eq(t, "same decision", second.Decision.ID, first.Decision.ID)
	if second.Ticket == nil || second.Ticket.ID != first.Ticket.ID {
		t.Fatalf("second accept: %+v", second)
	}
	tickets, _, decisions := h.repos.counts()
	eq(t, "one ticket", tickets, 1)
	eq(t, "one decision", decisions, 1)
	eq(t, "one created event", len(h.eventsOf(EventTicketCreated)), 1)

	// Queued again (a person's replay), it starts no new Eval: it is ignored as having started its Ticket.
	h.tick(1)
	h.must(h.eng.Reevaluate("c:1"))
	h.due()
	eq(t, "no eval", h.evalOf(issue), (*Evaluation)(nil))
	ds := h.decisionsOn(issue)
	eq(t, "ignored", ds[len(ds)-1].Kind, "ignore")
	eq(t, "started ticket", ds[len(ds)-1].Reasons, []Reason{{Code: CodeStartedTicket, TriggerID: "c:1",
		Values: map[string]string{"ticket": first.Ticket.ID}}})
	tickets, _, _ = h.repos.counts()
	eq(t, "still one ticket", tickets, 1)
}

func TestDirectAcceptOfInferredRefused(t *testing.T) {
	h := newEvals(t)
	h.pol.mode = "auto"
	h.receive(h.botMessage("b:1", issue))
	h.due()
	ev := h.evalOf(issue)
	r := h.decide(DecisionInput{TriggerID: "b:1", Kind: "accept", Title: "Do it"})
	if r.Refused == "" {
		t.Fatalf("accept without assessment: %+v", r)
	}
	eq(t, "eval kept", h.evalOf(issue).ID, ev.ID)
	_, _, decisions := h.repos.counts()
	eq(t, "no decision", decisions, 0)

	// A direct accept (no Eval holds it) of a Trigger the gate did not admit as explicit is refused too.
	h.receive(h.botMessage("b:2", other))
	r = h.decide(DecisionInput{TriggerID: "b:2", Kind: "accept", Title: "Do it"})
	if r.Refused == "" {
		t.Fatalf("direct accept of inferred: %+v", r)
	}
	// Even with an Assessment that accepts it in auto mode, a direct accept of an inferred Trigger is refused.
	match := &Assessment{Accept: true, Rules: []RuleJudgement{{Name: "deps", Matches: true, Evidence: "bump"}}}
	r = h.decide(DecisionInput{TriggerID: "b:2", Kind: "accept", Title: "Do it", Assessment: match})
	if !strings.HasPrefix(r.Refused, CodeNotExplicit) || r.Ticket != nil {
		t.Fatalf("direct accept of inferred with an assessment: %+v", r)
	}
	tickets, _, decisions := h.repos.counts()
	eq(t, "no ticket", tickets, 0)
	eq(t, "still no decision", decisions, 0)
	eq(t, "still queued", h.queued(other), []string{"b:2"})
	// One the gate does not admit at all.
	h.receive(h.message("c:1", pull1))
	r = h.decide(DecisionInput{TriggerID: "c:1", Kind: "accept", Title: "Do it"})
	if r.Refused == "" {
		t.Fatalf("direct accept of not admitted: %+v", r)
	}
	// A direct accept of an explicit request works, and takes it out of the queue.
	h.receive(h.request("c:2", pull2))
	r = h.decide(DecisionInput{TriggerID: "c:2", Kind: "accept", Title: "Fix"})
	if r.Ticket == nil {
		t.Fatalf("direct explicit accept: %+v", r)
	}
	eq(t, "dequeued", h.queued(pull2), []string(nil))

	// Malformed calls are errors.
	_, err := h.eng.Decide(DecisionInput{TriggerID: "nope", Kind: "accept", Title: "x"})
	wantUserError(t, "unknown trigger", err)
	_, err = h.eng.Decide(DecisionInput{TriggerID: "b:1", Kind: "maybe"})
	wantUserError(t, "unknown kind", err)
	_, err = h.eng.Decide(DecisionInput{TriggerID: "b:1", Kind: "stop"})
	wantUserError(t, "stop without ticket", err)
}

func TestObserveModeWouldAccept(t *testing.T) {
	h := newEvals(t) // observe is the default mode
	h.receive(h.botMessage("b:1", issue))
	h.due()
	a := &Assessment{Accept: true, Reason: "deps bump", Rules: []RuleJudgement{{Name: "deps", Matches: true, Evidence: "bump"}}}
	r := h.decide(DecisionInput{TriggerID: "b:1", Kind: "accept", Title: "Bump", Assessment: a})
	eq(t, "refused", r.Refused, "")
	eq(t, "no ticket", r.Ticket, (*Ticket)(nil))
	eq(t, "kind", r.Decision.Kind, "ignore")
	eq(t, "would accept", r.Decision.WouldAccept, true)
	eq(t, "reasons", codes(r.Decision.Reasons), []string{"would_accept"})
	eq(t, "eval ended", h.evalOf(issue), (*Evaluation)(nil))

	// An inferred candidate ignored from its card says so.
	h.receive(h.botMessage("b:2", other))
	h.due()
	r = h.decide(DecisionInput{TriggerID: "b:2", Kind: "ignore", Reason: "chatter", IgnoredFromCard: true})
	eq(t, "from card", r.Decision.IgnoredFromCard, true)
	eq(t, "card reasons", codes(r.Decision.Reasons), []string{CodeIgnoredFromCard, CodeSaid})

	// An assessment that fails records unavailable.
	h.pol.assessErr = errors.New("rule docs not judged")
	h.receive(h.botMessage("b:3", pull1))
	h.due()
	r = h.decide(DecisionInput{TriggerID: "b:3", Kind: "accept", Title: "x", Assessment: a})
	eq(t, "failed assessment", r.Decision.Kind, "unavailable")
	eq(t, "failed eval ended", h.evalOf(pull1), (*Evaluation)(nil))
}

// --- When Eval does not finish ---

func TestExpiryRequeuesThenUnavailable(t *testing.T) {
	h := newEvals(t)
	h.receive(h.request("c:1", issue))
	h.due()
	seen := map[string]bool{}
	for i := 1; i <= 2; i++ {
		ev := h.evalOf(issue)
		seen[ev.ID] = true
		h.must(h.eng.CandidateDelivered(ev.ID))
		h.tick(59)
		h.due()
		eq(t, "not yet expired", h.evalOf(issue).ID, ev.ID)
		h.tick(1)
		h.due() // expires, queues the Trigger again and starts a fresh Eval at once
		next := h.evalOf(issue)
		if next == nil || seen[next.ID] {
			t.Fatalf("expiry %d: eval %+v", i, next)
		}
		eq(t, "batch kept", next.TriggerIDs, []string{"c:1"})
		eq(t, "expired count", h.repos.thread(issue).Expired, i)
	}
	ev := h.evalOf(issue)
	h.must(h.eng.CandidateDelivered(ev.ID))
	h.tick(60)
	h.due()
	eq(t, "no eval", h.evalOf(issue), (*Evaluation)(nil))
	eq(t, "nothing queued", h.queued(issue), []string(nil))
	eq(t, "count reset", h.repos.thread(issue).Expired, 0)
	ds := h.decisionsOn(issue)
	eq(t, "one decision", len(ds), 1)
	eq(t, "unavailable", ds[0].Kind, "unavailable")
	eq(t, "timeout", codes(ds[0].Reasons), []string{CodeTimeout})

	h.must(h.eng.Dismiss(ds[0].ID))
	got := h.decisionsOn(issue)[0]
	eq(t, "dismissed", got.Dismissed != nil, true)
	wantUserError(t, "dismiss unknown", h.eng.Dismiss("nope"))
}

func TestUndeliveredNeverExpires(t *testing.T) {
	h := newEvals(t)
	h.receive(h.request("c:1", issue))
	h.due()
	ev := h.evalOf(issue)
	h.tick(3600)
	h.due()
	eq(t, "kept", h.evalOf(issue).ID, ev.ID)
	h.must(h.eng.CandidateDelivered(ev.ID))
	at := *h.evalOf(issue).DeliveredAt
	h.tick(10)
	h.must(h.eng.CandidateDelivered(ev.ID)) // idempotent
	eq(t, "delivered once", *h.evalOf(issue).DeliveredAt, at)
	h.must(h.eng.CandidateDelivered("gone")) // an ended Eval is ignored
	c, err := h.eng.Candidate("c:1")
	h.must(err)
	eq(t, "candidate eval", c.Eval.ID, ev.ID)
	eq(t, "candidate triggers", len(c.Triggers), 1)
	eq(t, "candidate thread", c.Thread.Key, issue)
}

func TestOutdatedVersionUnavailable(t *testing.T) {
	h := newEvals(t)
	h.receive(h.request("c:1", issue))
	h.receive(h.request("c:2", issue))
	h.due()
	h.pol.version = "v2" // changed without PolicyChanged, such as across a restart
	r := h.decide(DecisionInput{TriggerID: "c:1", Kind: "accept", Title: "Fix"})
	eq(t, "no ticket", r.Ticket, (*Ticket)(nil))
	eq(t, "unavailable", r.Decision.Kind, "unavailable")
	eq(t, "policy changed", codes(r.Decision.Reasons), []string{CodePolicyChanged})
	eq(t, "batch requeued", h.queued(issue), []string{"c:1", "c:2"})
	h.due()
	eq(t, "fresh eval under v2", h.evalOf(issue).PolicyVersion, "v2")
}

// --- Follow-ups, attach and hand-back ---

func TestOwnedFollowupOnlyStop(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	b := h.acceptRequest("c:2", other)
	h.receive(h.message("c:3", issue)) // owned by a
	for _, kind := range []string{"accept", "ignore"} {
		r := h.decide(DecisionInput{TriggerID: "c:3", Kind: kind, Title: "x"})
		if !strings.Contains(r.Refused, CodeWorkerDecides) {
			t.Errorf("%s of an owned follow-up: %+v", kind, r)
		}
	}
	wantUserError(t, "attach elsewhere", h.eng.Attach(b, "c:3", ""))
	r := h.decide(DecisionInput{TriggerID: "c:3", Kind: "stop", TicketID: a, Reason: "they said stop"})
	eq(t, "stop", r.Decision.Kind, "stop")
	eq(t, "stop ticket", r.Decision.TicketID, a)
	ended := h.ticket(a).Ended
	if ended == nil || ended.By != EndByHuman || ended.Outcome != OutcomeStopped {
		t.Fatalf("ended = %+v", ended)
	}
	eq(t, "stop message not sent to Eval", h.queued(issue), []string(nil))
	// A stop of an ended Ticket records ignore.
	h.receive(h.request("c:4", pull1))
	r = h.decide(DecisionInput{TriggerID: "c:4", Kind: "stop", TicketID: a})
	eq(t, "stop of ended", r.Decision.Kind, "ignore")
	eq(t, "stop unknown", codes(r.Decision.Reasons), []string{CodeStopUnknown})
}

func TestHandbackQueuesOnceNoOtherOwner(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	b := h.acceptRequest("c:2", other)
	h.subscribe(b, issue, false) // b follows the issue too, as its own
	h.receive(h.message("c:3", issue))
	h.settle(true)
	queued, err := h.eng.Handback(a, "c:3", "not mine")
	h.must(err)
	eq(t, "another ticket still decides it", queued, false)
	_, err = h.eng.Handback(a, "c:3", "again")
	wantUserError(t, "twice", err)
	queued, err = h.eng.Handback(b, "c:3", "not mine either")
	h.must(err)
	eq(t, "queued once no ticket decides it", queued, true)
	eq(t, "on the primary", h.queued(issue), []string{"c:3"})
	eq(t, "subscription kept", len(h.repos.thread(issue).Subscriptions), 2)
	h.due()
	c, err := h.eng.Candidate("c:3")
	h.must(err)
	eq(t, "handed back", c.HandedBack, map[string]string{a: "not mine", b: "not mine either"})
	eq(t, "tickets on thread", len(c.Tickets), 2)
	// Attached back to the Ticket that handed it back: refused.
	wantUserError(t, "attach to the ticket that handed it back", h.eng.Attach(a, "c:3", ""))

	// A bot's message, which went to Eval without the Ticket, queues nothing.
	h.receive(h.botMessage("b:1", other))
	queued, err = h.eng.Handback(b, "b:1", "noise")
	h.must(err)
	eq(t, "bot not queued again", queued, false)
	_, err = h.eng.Handback(b, "c:1", "never reached")
	wantUserError(t, "not reached", err)
}

func TestAttachRoutesAndRequeuesOthers(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	h.receive(h.request("c:2", other))
	h.receive(h.request("c:3", other))
	h.due()
	h.must(h.eng.Attach(a, "c:2", "same bug as T-0001"))
	in := inputsOf(h.ticket(a), "c:2")
	if len(in) != 1 || in[0].Note != "same bug as T-0001" || !in[0].Human {
		t.Fatalf("attached input = %+v", in)
	}
	d := h.disposition("c:2")
	eq(t, "attached", d.Attached, []string{a})
	eq(t, "reached", d.Reached, []string{a})
	eq(t, "subscribed", subscription(h.repos.thread(other), a) != nil, true)
	eq(t, "eval ended", h.evalOf(other), (*Evaluation)(nil))
	eq(t, "the rest waits", h.queued(other), []string{"c:3"})
	// The attached message is now the Ticket's follow-up.
	r := h.decide(DecisionInput{TriggerID: "c:2", Kind: "ignore"})
	if !strings.Contains(r.Refused, CodeWorkerDecides) {
		t.Errorf("ignore after attach: %+v", r)
	}
	// An ended Ticket resumes with the attached message.
	h.must(h.eng.FileReport(a, ReportInput{State: ReportReady, Summary: "done"}))
	h.receive(h.request("c:5", pull1))
	h.must(h.eng.Attach(a, "c:5", ""))
	eq(t, "resumed", h.ticket(a).Ended, (*Ended)(nil))
	// A stopped Ticket refuses.
	h.must(h.eng.StopTicket(a, "enough"))
	h.receive(h.request("c:6", pull2))
	wantUserError(t, "stopped", h.eng.Attach(a, "c:6", ""))
}

func TestReevaluate(t *testing.T) {
	h := newEvals(t)
	h.receive(h.request("c:1", issue))
	h.due()
	h.decide(DecisionInput{TriggerID: "c:1", Kind: "ignore", Reason: "not now"})
	h.must(h.eng.Reevaluate("c:1"))
	eq(t, "queued", h.queued(issue), []string{"c:1"})
	if h.disposition("c:1").Requeued == nil {
		t.Error("requeue not recorded")
	}
	eq(t, "requeued event", len(h.eventsOf(EventTriggerRequeued)), 1)
	wantUserError(t, "unknown", h.eng.Reevaluate("nope"))
}

// --- Release on end ---

func TestEndReleasesUnreadFollowups(t *testing.T) {
	ack := func(h *evals, triggerID, ticketID string) {
		h.must(h.repos.Tx(func(tx Tx) error {
			tr, err := tx.GetTrigger(triggerID)
			if err != nil {
				return err
			}
			tr.Disposition.Acknowledged = append(tr.Disposition.Acknowledged, ticketID)
			return tx.PutTrigger(tr)
		}))
	}
	t.Run("ready", func(t *testing.T) {
		h := newEvals(t)
		a := h.acceptRequest("c:1", issue)
		h.receive(h.message("m:read", issue))
		h.settle(true)
		h.read(a)
		h.tick(1)
		h.must(h.eng.FileReport(a, ReportInput{State: ReportProgress, Summary: "on it"}))
		h.tick(1)
		h.receive(h.message("m:unread", issue)) // delivered after its last report, never read
		h.receive(h.message("m:acked", issue))
		h.receive(h.message("m:back", issue))
		h.settle(true)
		ack(h, "m:acked", a)
		_, err := h.eng.Handback(a, "m:back", "other work")
		h.must(err)
		h.receive(h.botMessage("b:1", issue)) // bots went to Eval already
		eq(t, "before", h.queued(issue), []string{"m:back", "b:1"})
		h.tick(1)
		h.must(h.eng.FileReport(a, ReportInput{State: ReportReady, Summary: "done"}))
		eq(t, "ended", h.ticket(a).Ended.Outcome, OutcomeCompleted)
		eq(t, "unread released", h.queued(issue), []string{"m:back", "b:1", "m:unread"})
		eq(t, "no longer reached", h.disposition("m:unread").Reached, []string(nil))
		eq(t, "read one stays", h.disposition("m:read").Reached, []string{a})
	})
	t.Run("stop", func(t *testing.T) {
		h := newEvals(t)
		a := h.acceptRequest("c:1", issue)
		h.receive(h.message("m:1", issue))
		h.must(h.eng.StopTicket(a, "enough"))
		eq(t, "released", h.queued(issue), []string{"m:1"})
	})
	t.Run("another owner keeps it", func(t *testing.T) {
		h := newEvals(t)
		a := h.acceptRequest("c:1", issue)
		b := h.acceptRequest("c:2", other)
		h.subscribe(b, issue, false)
		h.receive(h.message("m:1", issue))
		h.must(h.eng.StopTicket(a, ""))
		eq(t, "b still decides it", h.queued(issue), []string(nil))
		h.must(h.eng.StopTicket(b, ""))
		eq(t, "released by the last", h.queued(issue), []string{"m:1"})
	})
	t.Run("thread closed", func(t *testing.T) {
		h := newEvals(t)
		a := h.acceptRequest("c:1", issue)
		h.must(h.eng.BindThread(a, pull1, true))
		h.must(h.eng.FileReport(a, ReportInput{State: ReportReady, Summary: "PR open"}))
		h.tick(1)
		h.receive(h.message("m:1", pull1))
		h.settle(true)
		h.closePull(pull1, OutcomeMerged)
		eq(t, "ended", h.ticket(a).Ended.Outcome, OutcomeMerged)
		eq(t, "released on the PR", h.queued(pull1), []string{"m:1"})
	})
}

// --- Policy change ---

func TestPolicyChangedNotifiesAffectedOnly(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	b := h.acceptRequest("c:2", other)
	h.receive(h.request("c:3", pull1))
	h.receive(h.request("c:4", pull1))
	h.due()
	h.pol.version = "v2"
	h.must(h.eng.PolicyChanged("v2", map[string]string{a: "Rule deps changed: only patch bumps."}))
	ta, tb := h.ticket(a), h.ticket(b)
	last := ta.Inputs[len(ta.Inputs)-1]
	eq(t, "note kind", last.Kind, InputNote)
	eq(t, "note text", last.Text, "Rule deps changed: only patch bumps.")
	eq(t, "unaffected untouched", inputKinds(tb), []InputKind{InputTrigger})
	eq(t, "eval ended", h.evalOf(pull1), (*Evaluation)(nil))
	eq(t, "batch requeued", h.queued(pull1), []string{"c:3", "c:4"})
	ds := h.decisionsOn(pull1)
	eq(t, "unavailable", ds[len(ds)-1].Kind, "unavailable")
	eq(t, "reason", codes(ds[len(ds)-1].Reasons), []string{CodePolicyChanged})
}

// A Policy-change note is no work: it does not keep a closed pull request's resumed work open.
func TestPolicyNoteDoesNotDeferClose(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	h.must(h.eng.BindThread(a, pull1, true))
	h.must(h.eng.FileReport(a, ReportInput{State: ReportFailed, Summary: "tests fail"}))
	closedAt := h.now().Add(5 * time.Second)
	h.tick(10)
	h.must(h.eng.RetryTicket(a))
	h.settle(true)
	h.tick(1)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportReady, Summary: "fixed"}))
	h.tick(1)
	h.must(h.eng.PolicyChanged("v2", map[string]string{a: "Proactive work is no longer accepted."}))
	h.settle(true)
	h.must(h.eng.ThreadClosed(pull1, OutcomeMerged, closedAt))
	if e := h.ticket(a).Ended; e == nil || e.Outcome != OutcomeMerged {
		t.Fatalf("ended = %+v", e)
	}

	// Nor while the note still waits undelivered.
	b := h.acceptRequest("c:2", other)
	h.must(h.eng.BindThread(b, pull2, true))
	h.must(h.eng.FileReport(b, ReportInput{State: ReportFailed, Summary: "tests fail"}))
	closedAt = h.now().Add(5 * time.Second)
	h.tick(10)
	h.must(h.eng.RetryTicket(b))
	h.settle(true)
	h.tick(1)
	h.must(h.eng.FileReport(b, ReportInput{State: ReportReady, Summary: "fixed"}))
	h.tick(1)
	h.must(h.eng.PolicyChanged("v3", map[string]string{b: "Rule deps no longer applies."}))
	if len(h.ticket(b).Undelivered) != 1 {
		t.Fatalf("note not waiting: %v", h.ticket(b).Undelivered)
	}
	h.must(h.eng.ThreadClosed(pull2, OutcomeMerged, closedAt))
	if e := h.ticket(b).Ended; e == nil || e.Outcome != OutcomeMerged {
		t.Fatalf("undelivered note: ended = %+v", e)
	}
}

// eval_seconds measures an Eval from its start to its Decision.
func TestEvalSecondsMetric(t *testing.T) {
	h := newEvals(t)
	reg := metrics.NewRegistry()
	h.eng.SetMetrics(reg)
	h.receive(h.request("c:1", issue))
	h.due()
	h.tick(7)
	h.decide(DecisionInput{TriggerID: "c:1", Kind: "accept", Title: "Fix c:1"})
	var found bool
	for _, hv := range reg.Snapshot(0).Histograms {
		if hv.Name == "eval_seconds" {
			found = true
			eq(t, "count", hv.Count, int64(1))
			if hv.P50 < 6 || hv.P50 > 8 {
				t.Errorf("p50 = %v, want about 7s", hv.P50)
			}
		}
	}
	if !found {
		t.Fatal("no eval_seconds histogram")
	}
}

// At startup an undelivered Eval whose assessment effect is missing gets it again; one whose effect waits, and a
// delivered one, are left as they are.
func TestResumeAssessments(t *testing.T) {
	h := newEvals(t)
	h.receive(h.request("c:1", issue))
	h.receive(h.request("c:2", other))
	h.receive(h.request("c:3", pull1))
	h.due()
	a, b, c := h.evalOf(issue), h.evalOf(other), h.evalOf(pull1)
	h.must(h.eng.CandidateDelivered(c.ID))
	before := len(h.effectsOf(EffectAssessment))
	got, err := h.eng.ResumeAssessments(map[string]bool{a.ID: true})
	h.must(err)
	eq(t, "resumed", got, []string{b.ID})
	eq(t, "effects (a key enqueued again stays one row)", len(h.effectsOf(EffectAssessment)), before)
	got, err = h.eng.ResumeAssessments(map[string]bool{a.ID: true, b.ID: true})
	h.must(err)
	eq(t, "nothing missing", got, []string(nil))
	eq(t, "c keeps its deadline", h.evalOf(pull1).DeliveredAt != nil, true)
}

func TestAcceptRecordsTrust(t *testing.T) {
	h := newEvals(t)
	h.pol.trust = TrustCollaborator
	issue := "github:o/r#1"
	h.receive(h.request("c:1", issue))
	h.due()
	r := h.decide(DecisionInput{TriggerID: "c:1", Kind: "accept", Title: "Fix it"})
	if r.Ticket == nil {
		t.Fatalf("no ticket: %+v", r)
	}
	eq(t, "trust", r.Ticket.Mandate.Trust, TrustCollaborator)
	eq(t, "trust class", r.Ticket.Mandate.TrustClass(), TrustCollaborator)
}

func TestLegacyMandateTrust(t *testing.T) {
	for _, tc := range []struct {
		m    Mandate
		want string
	}{
		{Mandate{Basis: MandateBasis{Via: ViaOwner}}, TrustOwner},
		{Mandate{Basis: MandateBasis{Via: ViaAddressed}}, TrustAllowed},
		{Mandate{Basis: MandateBasis{Via: ViaBot}}, TrustBot},
		{Mandate{Trust: TrustStranger, Basis: MandateBasis{Via: ViaOwner}}, TrustStranger},
	} {
		eq(t, "class", tc.m.TrustClass(), tc.want)
	}
}
