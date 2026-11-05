package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/danielpina1/athena/internal/metrics"
)

// Evals: the Evals of Threads and the Decisions they end with, the main Session's accept, ignore, stop and attach, a
// worker's hand-back, a person's replay and dismissal, and the restart of Evals on a Policy change, ported from the
// reference (src/core/evals.ts; handBack and queueForEval in part.ts; create, attach, letGo and policyChanged in
// tickets.ts).

// EffectAssessment is the effect kind that steers an Eval's candidate into the main Session; its key is the Eval's ID.
const EffectAssessment = "assessment"

// Decision kinds.
const (
	DecisionAccept      = "accept"
	DecisionIgnore      = "ignore"
	DecisionStop        = "stop"
	DecisionUnavailable = "unavailable"
)

// Strengths a gate gives.
const (
	StrengthExplicit = "explicit"
	StrengthInferred = "inferred"
)

// Reason codes the core records on Decisions.
const (
	// CodeSaid carries the words an assessor or a person wrote, in Values["text"].
	CodeSaid = "said"
	// CodeTimeout: the Eval expired a third time in a row.
	CodeTimeout = "timeout"
	// CodePolicyChanged: the Policy version changed while the Eval ran, or the Decision was made under an older one.
	CodePolicyChanged = "policy_changed"
	// CodeRouted: the Trigger was attached to a Ticket (Values["ticket"]).
	CodeRouted = "routed"
	// CodeStartedTicket: the Trigger already started a Ticket (Values["ticket"]).
	CodeStartedTicket = "started_ticket"
	// CodeAcceptedBefore: a Trigger of the batch had been accepted before (Values["trigger"]).
	CodeAcceptedBefore = "accepted_before"
	// CodeStopUnknown: a stop named a Ticket that is unknown or ended.
	CodeStopUnknown = "stop_unknown"
	// CodeStopNotExplicit: a stop came from a candidate that is not explicit.
	CodeStopNotExplicit = "stop_not_explicit"
	// CodeIgnoredFromCard: an inferred candidate was ignored from its card, without an assessment.
	CodeIgnoredFromCard = "ignored_from_card"
	// CodeNoAdmissible: nothing in the batch was admitted, and the gate gave no reason.
	CodeNoAdmissible = "no_admissible"
	// CodeAssessmentFailed: the Policy could not judge the assessment (Values["error"]).
	CodeAssessmentFailed = "assessment_failed"
)

// Codes a refused Decide starts its DecisionResult.Refused with.
const (
	// CodeWorkerDecides: a Ticket's worker decides the follow-up; only stop is allowed.
	CodeWorkerDecides = "worker_decides"
	// CodeNotExplicit: an accept of a candidate that is not explicit without an assessment that accepts it.
	CodeNotExplicit = "not_explicit"
	// CodeNotAdmissible: a direct Decision of a Trigger the gate does not admit.
	CodeNotAdmissible = "not_admissible"
	// CodeTitleRequired: an accept without a title.
	CodeTitleRequired = "title_required"
)

// evalRequeues is how many times in a row an expired Eval queues its Triggers again before it records unavailable.
const evalRequeues = 2

// CandidateView is the Eval that holds a Trigger, as its assessor sees it: the batch, its Thread, the Tickets on the
// Thread and, by Ticket, the reasons Tickets gave when they handed a Trigger of the batch back.
type CandidateView struct {
	Eval       Evaluation
	Triggers   []Trigger
	Thread     Thread
	Tickets    []Ticket
	HandedBack map[string]string
}

// DecisionInput is the main Session's Decision on a Trigger: the Eval that holds it, or a direct one of it alone.
// Title is the new Ticket's (required to accept) and Ack the acknowledgement an accept carries; TicketID names the
// Ticket a stop ends. Assessment is required to accept an inferred candidate. IgnoredFromCard marks an ignore of an
// inferred candidate from its card.
type DecisionInput struct {
	TriggerID       string
	Kind            string
	Reason          string
	Title           string
	Ack             string
	TicketID        string
	Assessment      *Assessment
	IgnoredFromCard bool
	// Origin is who decides; OriginRouter for the main Session, whose stop then sends it no notice of the end.
	Origin CommandOrigin
}

// DecisionResult is what Decide recorded, with the Ticket an accept created (or the earlier accept of the same Trigger
// started); Refused says why nothing was recorded, starting with a code such as CodeWorkerDecides.
type DecisionResult struct {
	Decision Decision
	Ticket   *Ticket
	Refused  string
}

// --- The pass ---

// EvaluateDue does the Evals' due work: an Eval whose candidate reached the main Session more than the Policy's
// EvalTimeout ago queues its Triggers again, twice in a row, and records unavailable the third time (an undelivered
// one never expires); then each Thread whose queued Triggers wait starts one Eval of all of them. Each Thread changes
// in its own transaction. Without a Policy it does nothing.
func (e *Engine) EvaluateDue() error {
	if e.policy == nil {
		return nil
	}
	p := e.policy()
	if p == nil {
		return nil
	}
	threads, err := e.activeThreads()
	if err != nil {
		return err
	}
	var errs []error
	for _, th := range threads {
		if th.Eval == nil {
			continue
		}
		key := th.Key
		errs = append(errs, e.command(cmdEvaluateDue, func(o *op) error { return o.due(key, p) }))
	}
	if threads, err = e.activeThreads(); err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, th := range threads {
		if th.Eval != nil || len(th.Queued) == 0 {
			continue
		}
		key := th.Key
		errs = append(errs, e.command(cmdEvaluateDue, func(o *op) error { return o.startEval(key, p) }))
	}
	return errors.Join(errs...)
}

// ActiveThreads returns the Threads with Triggers queued or an Eval in progress, ordered by key: a cheap query that
// tells whether EvaluateDue has anything to do.
func (e *Engine) ActiveThreads() ([]Thread, error) {
	threads, err := e.activeThreads()
	if err != nil {
		return nil, err
	}
	out := make([]Thread, 0, len(threads))
	for _, th := range threads {
		out = append(out, *th)
	}
	return out, nil
}

func (e *Engine) activeThreads() ([]*Thread, error) {
	var threads []*Thread
	err := e.repos.Tx(func(tx Tx) error {
		var err error
		threads, err = tx.ActiveThreads()
		return err
	})
	return threads, err
}

// due expires a Thread's Eval past its deadline: EvalTimeout after its candidate reached the main Session.
func (o *op) due(key string, p Policy) error {
	th, err := o.tx.GetThread(key)
	if err != nil || th == nil || th.Eval == nil || th.Eval.DeliveredAt == nil {
		return err
	}
	if o.at.Before(th.Eval.DeliveredAt.Add(p.EvalTimeout())) {
		return nil
	}
	expired := th.Expired + 1
	if expired > evalRequeues {
		_, err := o.finish(th, th.Eval, &Decision{Kind: DecisionUnavailable, Reasons: []Reason{{Code: CodeTimeout}}})
		return err
	}
	ids := th.Eval.TriggerIDs
	th.Eval = nil
	th.Expired = expired
	th.Queued = requeueFirst(th.Queued, ids)
	return o.tx.PutThread(th)
}

// startEval starts one Eval of the Triggers queued on a Thread: a follow-up a Ticket decides leaves the queue, a
// Trigger that already started a Ticket is ignored as such, and the gate judges the rest; the strongest admitted
// Trigger (the later of equals) is the primary. A batch with nothing admitted is ignored with the gate's reasons.
func (o *op) startEval(key string, p Policy) error {
	th, err := o.tx.GetThread(key)
	if err != nil || th == nil || th.Eval != nil || len(th.Queued) == 0 {
		return err
	}
	accepted, err := o.acceptedOn(th.Key)
	if err != nil {
		return err
	}
	var triggers []*Trigger
	var started []string
	var startedReasons []Reason
	for _, id := range th.Queued {
		tr, err := o.tx.GetTrigger(id)
		if err != nil {
			return err
		}
		if tr == nil {
			continue
		}
		owners, err := followupOwners(tr, o.tx.GetTicket, th)
		if err != nil {
			return err
		}
		if len(owners) > 0 {
			continue // a Ticket's worker decides it
		}
		if d := accepted[id]; d != nil {
			started = append(started, id)
			startedReasons = append(startedReasons, Reason{Code: CodeStartedTicket, TriggerID: id,
				Values: map[string]string{"ticket": d.TicketID}})
			continue
		}
		triggers = append(triggers, tr)
	}
	th.Queued = nil
	if len(started) > 0 {
		if err := o.record(th, &Decision{Kind: DecisionIgnore, TriggerIDs: started, Reasons: startedReasons}); err != nil {
			return err
		}
		th.Expired = 0
	}
	tickets, err := o.ticketsOn(th)
	if err != nil {
		return err
	}
	var reasons []Reason
	var primary *Trigger
	var gate Gate
	for _, tr := range triggers {
		g := p.Gate(*tr, *th, tickets)
		if g.Strength == "" {
			r := g.Reason
			if r.Code == "" {
				r.Code = CodeNoAdmissible
			}
			if r.TriggerID == "" {
				r.TriggerID = tr.ID
			}
			reasons = append(reasons, r)
			continue
		}
		if primary == nil || rank(g.Strength) > rank(gate.Strength) ||
			(rank(g.Strength) == rank(gate.Strength) && !tr.At.Before(primary.At)) {
			primary, gate = tr, g
		}
	}
	ids := make([]string, 0, len(triggers))
	for _, tr := range triggers {
		ids = append(ids, tr.ID)
	}
	if primary == nil {
		if len(ids) > 0 {
			if len(reasons) == 0 {
				reasons = []Reason{{Code: CodeNoAdmissible}}
			}
			if err := o.record(th, &Decision{Kind: DecisionIgnore, TriggerIDs: ids, Reasons: reasons}); err != nil {
				return err
			}
			th.Expired = 0
		}
		return o.tx.PutThread(th)
	}
	ev := &Evaluation{ID: newID(), Thread: th.Key, Primary: primary.ID, TriggerIDs: ids, Strength: gate.Strength,
		Basis: gate.Basis, PolicyVersion: p.Version(), StartedAt: o.at}
	th.Eval = ev
	if err := o.tx.PutThread(th); err != nil {
		return err
	}
	if err := o.enqueueAssessment(ev); err != nil {
		return err
	}
	o.emitEvent(Event{Type: EventEvalStarted, At: o.at, Data: map[string]any{"evalId": ev.ID, "thread": th.Key,
		"triggerIds": slices.Clone(ids), "primary": ev.Primary, "strength": ev.Strength}})
	return nil
}

// rank orders strengths: explicit over inferred over none.
func rank(strength string) int {
	switch strength {
	case StrengthExplicit:
		return 2
	case StrengthInferred:
		return 1
	}
	return 0
}

// --- The main Session ---

// Candidate returns the Eval that holds a Trigger, as its assessor sees it; a UserError when none does.
func (e *Engine) Candidate(triggerID string) (CandidateView, error) {
	var view CandidateView
	err := e.repos.Tx(func(tx Tx) error {
		tr, err := tx.GetTrigger(triggerID)
		if err != nil {
			return err
		}
		if tr == nil {
			return userErrorf("trigger %s not found", triggerID)
		}
		var th *Thread
		if len(tr.Threads) > 0 {
			if th, err = tx.GetThread(tr.Threads[0]); err != nil {
				return err
			}
		}
		if th == nil || th.Eval == nil || !slices.Contains(th.Eval.TriggerIDs, triggerID) {
			return userErrorf("no eval holds trigger %s", triggerID)
		}
		view = CandidateView{Eval: *th.Eval, Thread: *th, HandedBack: map[string]string{}}
		for _, id := range th.Eval.TriggerIDs {
			t, err := tx.GetTrigger(id)
			if err != nil {
				return err
			}
			if t == nil {
				continue
			}
			view.Triggers = append(view.Triggers, *t)
			for _, ticket := range slices.Sorted(maps.Keys(t.Disposition.HandedBack)) {
				reason := t.Disposition.HandedBack[ticket]
				if prev, ok := view.HandedBack[ticket]; ok && prev != reason {
					reason = prev + "; " + reason
				}
				view.HandedBack[ticket] = reason
			}
		}
		o := &op{tx: tx}
		view.Tickets, err = o.ticketsOn(th)
		return err
	})
	if err != nil {
		return CandidateView{}, err
	}
	return view, nil
}

// CandidateDelivered records that an Eval's candidate reached the main Session: its deadline counts from now. A
// second delivery, or one of an Eval that has ended, changes nothing.
func (e *Engine) CandidateDelivered(evalID string) error {
	if evalID == "" {
		return userErrorf("an eval id is required")
	}
	return e.command(cmdCandidateDelivered, func(o *op) error {
		threads, err := o.tx.ActiveThreads()
		if err != nil {
			return err
		}
		for _, th := range threads {
			if th.Eval == nil || th.Eval.ID != evalID {
				continue
			}
			if th.Eval.DeliveredAt != nil {
				return nil
			}
			th.Eval.DeliveredAt = o.timestamp()
			return o.tx.PutThread(th)
		}
		return nil
	})
}

// Decide records the main Session's Decision on a Trigger: the Eval that holds it, or a direct Eval of it alone.
//
// Accept creates the Ticket, its Mandate, Subscriptions to every Thread of the batch (the primary one its own) and
// first Assignment, and records the acknowledgement text on the Decision. An inferred candidate is accepted only on an
// Assessment the Policy turns into an accept; in observe mode it records ignore with WouldAccept. A Trigger accepted
// before returns that Decision and its Ticket. While a Ticket's worker decides a person's follow-up, only stop is
// allowed. A stop ends the named live Ticket as a person's stop. A Decision under an outdated Policy version records
// unavailable and queues the batch again.
//
// A refusal records nothing and returns DecisionResult.Refused with a nil error; a malformed call (unknown Trigger or
// kind, a stop without a Ticket) returns a *UserError.
func (e *Engine) Decide(d DecisionInput) (DecisionResult, error) {
	if d.TriggerID == "" {
		return DecisionResult{}, userErrorf("a decision needs a trigger id")
	}
	switch d.Kind {
	case DecisionAccept, DecisionIgnore:
	case DecisionStop:
		if d.TicketID == "" {
			return DecisionResult{}, userErrorf("a stop needs a ticket id")
		}
	default:
		return DecisionResult{}, userErrorf("unknown decision kind %q (accept, ignore or stop)", d.Kind)
	}
	if d.Kind == DecisionAccept {
		if err := ackRefusal(d.Ack); err != nil {
			return DecisionResult{}, err
		}
	}
	var res DecisionResult
	err := e.command(cmdDecide, func(o *op) error {
		res = DecisionResult{}
		o.origin = d.Origin
		p := o.currentPolicy()
		if p == nil {
			return userErrorf("deciding needs a Policy")
		}
		return o.decide(d, p, &res)
	})
	if err != nil {
		return DecisionResult{}, err
	}
	return res, nil
}

func (o *op) decide(d DecisionInput, p Policy, res *DecisionResult) error {
	tr, err := o.tx.GetTrigger(d.TriggerID)
	if err != nil {
		return err
	}
	if tr == nil {
		return userErrorf("trigger %s not found", d.TriggerID)
	}
	if len(tr.Threads) == 0 {
		return userErrorf("trigger %s belongs to no thread", d.TriggerID)
	}
	th, err := o.thread(tr.Threads[0])
	if err != nil {
		return err
	}
	held := th.Eval != nil && slices.Contains(th.Eval.TriggerIDs, tr.ID)
	if d.Kind == DecisionAccept {
		accepted, err := o.acceptedOn(th.Key)
		if err != nil {
			return err
		}
		if earlier := accepted[tr.ID]; earlier != nil {
			// Only an Eval an older version started can still hold it: the rest of its batch is judged again without it.
			if held {
				if others := without(th.Eval.TriggerIDs, tr.ID); len(others) > 0 {
					_, err = o.reassess(th, Reason{Code: CodeAcceptedBefore, Values: map[string]string{"trigger": tr.ID}}, tr.ID)
				} else {
					_, err = o.finish(th, th.Eval, &Decision{Kind: DecisionIgnore, Reasons: []Reason{{Code: CodeStartedTicket,
						TriggerID: tr.ID, Values: map[string]string{"ticket": earlier.TicketID}}}})
				}
				if err != nil {
					return err
				}
			}
			res.Decision = *earlier
			tk, err := o.tx.GetTicket(earlier.TicketID)
			if err != nil {
				return err
			}
			if tk != nil {
				c := cloneTicket(tk)
				res.Ticket = &c
			}
			return nil
		}
	}
	if d.Kind != DecisionStop {
		owners, err := followupOwners(tr, o.tx.GetTicket, th)
		if err != nil {
			return err
		}
		if len(owners) > 0 {
			res.Refused = fmt.Sprintf("%s: ticket %s decides this follow-up; only stop is allowed", CodeWorkerDecides,
				strings.Join(owners, ", "))
			return nil
		}
	}
	// An accept needs a title; one whose assessment does not accept records an ignore, which needs none.
	if d.Kind == DecisionAccept && titleOf(d.Title) == "" && (d.Assessment == nil || d.Assessment.Accept) {
		res.Refused = CodeTitleRequired + ": an accept needs a title"
		return nil
	}
	ev := th.Eval
	if !held {
		tickets, err := o.ticketsOn(th)
		if err != nil {
			return err
		}
		g := p.Gate(*tr, *th, tickets)
		if g.Strength == "" {
			res.Refused = fmt.Sprintf("%s: %s", CodeNotAdmissible, g.Reason.Code)
			return nil
		}
		// Direct accept refuses a Trigger the gate did not admit as explicit, whatever an Assessment says: an
		// inferred candidate is accepted only through its Eval.
		if d.Kind == DecisionAccept && g.Strength != StrengthExplicit {
			res.Refused = CodeNotExplicit + ": a direct accept needs a Trigger the gate admits as explicit"
			return nil
		}
		// A gate-only Eval, decided at once beside any Eval its Thread has in progress.
		ev = &Evaluation{ID: newID(), Thread: th.Key, Primary: tr.ID, TriggerIDs: []string{tr.ID}, Strength: g.Strength,
			Basis: g.Basis, PolicyVersion: p.Version(), StartedAt: o.at}
	}
	return o.decideEval(th, ev, d, p, res)
}

// decideEval applies a Decision to an Eval of a Thread: the one in progress, or a gate-only one.
func (o *op) decideEval(th *Thread, ev *Evaluation, d DecisionInput, p Policy, res *DecisionResult) error {
	own := th.Eval != nil && th.Eval.ID == ev.ID
	record := func(dec *Decision) error {
		got, err := o.finish(th, ev, dec)
		if err == nil {
			res.Decision = *got
		}
		return err
	}
	if p.Version() != ev.PolicyVersion {
		if own {
			got, err := o.reassess(th, Reason{Code: CodePolicyChanged})
			if err == nil {
				res.Decision = *got
			}
			return err
		}
		return record(&Decision{Kind: DecisionUnavailable, Reasons: []Reason{{Code: CodePolicyChanged}}})
	}
	said := saidReason(d.Reason)
	switch d.Kind {
	case DecisionIgnore:
		dec := &Decision{Kind: DecisionIgnore}
		if ev.Strength != StrengthExplicit && (d.IgnoredFromCard || d.Assessment == nil) {
			dec.IgnoredFromCard = true
			dec.Reasons = []Reason{{Code: CodeIgnoredFromCard}}
		}
		dec.Reasons = append(dec.Reasons, said...)
		return record(dec)
	case DecisionStop:
		tk, err := o.tx.GetTicket(d.TicketID)
		if err != nil {
			return err
		}
		if tk == nil || tk.Ended != nil {
			return record(&Decision{Kind: DecisionIgnore, Reasons: []Reason{{Code: CodeStopUnknown}}})
		}
		if ev.Strength != StrengthExplicit {
			return record(&Decision{Kind: DecisionIgnore, Reasons: []Reason{{Code: CodeStopNotExplicit}}})
		}
		if err := record(&Decision{Kind: DecisionStop, TicketID: tk.ID, Reasons: said}); err != nil {
			return err
		}
		// Eval decided the strength of the request: it is a person's stop, which withdraws the Mandate.
		if err := o.end(tk, EndByHuman, OutcomeStopped, d.Reason); err != nil {
			return err
		}
		return o.tx.PutTicket(tk)
	}
	return o.accept(th, ev, d, p, res)
}

// accept records an accept with the Mandate the Eval gives and creates its Ticket; an inferred candidate needs an
// Assessment the Policy turns into an accept.
func (o *op) accept(th *Thread, ev *Evaluation, d DecisionInput, p Policy, res *DecisionResult) error {
	kind, reasons, would := DecisionAccept, saidReason(d.Reason), false
	var rules []string
	if a := d.Assessment; a != nil {
		k, rs, w, err := p.Assess(*a, ev.Strength)
		if err != nil {
			got, err := o.finish(th, ev, &Decision{Kind: DecisionUnavailable,
				Reasons: []Reason{{Code: CodeAssessmentFailed, Values: map[string]string{"error": err.Error()}}}})
			if err == nil {
				res.Decision = *got
			}
			return err
		}
		kind, reasons, would = k, rs, w
		if d.Reason != "" && d.Reason != a.Reason {
			reasons = append(reasons, saidReason(d.Reason)...)
		}
		for _, j := range a.Rules {
			if j.Matches {
				rules = append(rules, j.Name)
			}
		}
	} else if ev.Strength != StrengthExplicit {
		res.Refused = CodeNotExplicit + ": an inferred candidate is accepted only on an assessment"
		return nil
	}
	if kind != DecisionAccept {
		got, err := o.finish(th, ev, &Decision{Kind: DecisionIgnore, Reasons: reasons, WouldAccept: would})
		if err == nil {
			res.Decision = *got
		}
		return err
	}
	if ev.Strength != StrengthExplicit && !p.AdmitsInferred(rules) {
		res.Refused = CodeNotExplicit + ": proactive work is not accepted on these rules now"
		return nil
	}

	primary, err := o.tx.GetTrigger(ev.Primary)
	if err != nil {
		return err
	}
	if primary == nil {
		return fmt.Errorf("core: eval %s: primary trigger %s not stored", ev.ID, ev.Primary)
	}
	basis := ev.Basis
	if len(rules) > 0 {
		basis.Rules = rules
	}
	mandate := Mandate{Strength: ev.Strength, PolicyVersion: ev.PolicyVersion, Actor: primary.Actor, Thread: ev.Thread, Basis: basis}
	n, err := o.tx.NextSeq(ticketSeq)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("T-%04d", n)
	decisionMandate := mandate
	dec, err := o.finish(th, ev, &Decision{Kind: DecisionAccept, TicketID: id, Mandate: &decisionMandate, Reasons: reasons, Ack: d.Ack})
	if err != nil {
		return err
	}
	t := &Ticket{ID: id, Title: titleOf(d.Title), DecisionID: dec.ID, Mandate: mandate, CreatedAt: o.at, UpdatedAt: o.at,
		Origin:    Origin{TriggerID: primary.ID, TriggerIDs: slices.Clone(ev.TriggerIDs)},
		Execution: &Execution{Runtime: "claude"}, Repo: th.Meta["repo"]}
	// The primary Trigger comes first, then the rest of the batch in order.
	batch := []*Trigger{primary}
	for _, tid := range ev.TriggerIDs {
		if tid == primary.ID {
			continue
		}
		tr, err := o.tx.GetTrigger(tid)
		if err != nil {
			return err
		}
		if tr != nil {
			batch = append(batch, tr)
		}
	}
	for _, tr := range batch {
		in := Input{ID: newID(), Kind: InputTrigger, At: o.at, TriggerID: tr.ID, Text: tr.Text, Human: tr.Human}
		t.Inputs = append(t.Inputs, in)
		t.Undelivered = append(t.Undelivered, in.ID)
		for _, ch := range tr.Channels {
			if !slices.ContainsFunc(t.Channels, func(c ChannelRef) bool { return sameChannel(c, ch) }) {
				ch.Address = maps.Clone(ch.Address)
				t.Channels = append(t.Channels, ch)
			}
		}
	}
	assignment := o.newAssignment(t)
	if err := o.tx.PutTicket(t); err != nil {
		return err
	}
	// Subscriptions only: a Source's BindThread, not an accept, makes a Ticket follow a Thread.
	var subscribed []string
	for _, tr := range batch {
		for _, key := range tr.Threads {
			if slices.Contains(subscribed, key) {
				continue
			}
			subscribed = append(subscribed, key)
			if err := o.subscribe(t.ID, key, key == ev.Thread); err != nil {
				return err
			}
		}
	}
	// The acknowledgement the accept carries goes out through Reply in this transaction, where a person's message
	// started the Ticket (the reference's acknowledgeAccepted: bots and CI are not acknowledged).
	if strings.TrimSpace(d.Ack) != "" && primary.Human {
		if _, err := o.reply(ReplyRequest{TicketID: t.ID, TriggerID: primary.ID, Purpose: PurposeAcknowledgement, Text: d.Ack}); err != nil {
			return err
		}
	}
	o.start(t, assignment)
	o.event(EventTicketCreated, t, assignment, map[string]any{"decisionId": dec.ID, "strength": mandate.Strength})
	res.Decision = *dec
	c := cloneTicket(t)
	res.Ticket = &c
	return nil
}

// Attach routes a Trigger to a Ticket it points at, whose worker then decides it as a follow-up: the Ticket subscribes
// to the Trigger's Threads and receives it as an Input with the note, and the Eval that holds it ends (the other
// Triggers of its batch wait for a fresh Eval). An ended Ticket resumes under its Mandate. A follow-up a worker still
// decides is refused, as are a Trigger the Ticket handed back, a Ticket a person stopped and one whose Mandate no
// longer holds.
func (e *Engine) Attach(ticketID, triggerID, note string) error {
	return e.command(cmdAttach, func(o *op) error {
		tr, err := o.tx.GetTrigger(triggerID)
		if err != nil {
			return err
		}
		if tr == nil {
			return userErrorf("trigger %s not found", triggerID)
		}
		if len(tr.Threads) == 0 {
			return userErrorf("trigger %s belongs to no thread", triggerID)
		}
		if _, back := tr.Disposition.HandedBack[ticketID]; back {
			return userErrorf("ticket %s handed trigger %s back", ticketID, triggerID)
		}
		primary, err := o.thread(tr.Threads[0])
		if err != nil {
			return err
		}
		owners, err := followupOwners(tr, o.tx.GetTicket, primary)
		if err != nil {
			return err
		}
		if len(owners) > 0 {
			return userErrorf("ticket %s decides trigger %s; only stop is allowed", strings.Join(owners, ", "), triggerID)
		}
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		if t.Ended != nil && t.Ended.By == EndByHuman {
			return userErrorf("ticket %s was stopped", ticketID)
		}
		if t.Ended != nil && !o.revalidates(t) {
			return userErrorf("the mandate of ticket %s no longer holds; it needs a new accept", ticketID)
		}
		if slices.Contains(primary.Queued, triggerID) {
			primary.Queued = without(primary.Queued, triggerID)
			if err := o.tx.PutThread(primary); err != nil {
				return err
			}
		}
		for _, key := range slices.Compact(slices.Clone(tr.Threads)) {
			if err := o.subscribe(t.ID, key, false); err != nil {
				return err
			}
		}
		d := &tr.Disposition
		requeued, reached, context := d.Requeued != nil, slices.Contains(d.Reached, t.ID), slices.Contains(d.Context, t.ID)
		d.Requeued = nil
		if !reached {
			d.Reached = append(d.Reached, t.ID)
		}
		d.Context = without(d.Context, t.ID)
		if !slices.Contains(d.Attached, t.ID) {
			d.Attached = append(d.Attached, t.ID)
		}
		if err := o.tx.PutTrigger(tr); err != nil {
			return err
		}
		var existing *Input
		for i := range t.Inputs {
			if t.Inputs[i].Kind == InputTrigger && t.Inputs[i].TriggerID == tr.ID {
				existing = &t.Inputs[i]
			}
		}
		in := Input{ID: newID(), Kind: InputTrigger, At: o.at, TriggerID: tr.ID, Text: tr.Text, Human: tr.Human, Note: note}
		switch {
		case existing == nil:
			o.addInput(t, in)
		case note != "" && slices.Contains(t.Undelivered, existing.ID):
			existing.Note = note
			t.UpdatedAt = o.at
		case requeued || !reached || context || t.Ended != nil:
			// A person sent it to Eval, or the Ticket let it go, read it for context only or ended after its worker had
			// it: the worker gets it again.
			o.addInput(t, in)
		case note != "":
			o.addInput(t, in)
		}
		// The Eval that holds it ends, since the Ticket has it now.
		th, err := o.tx.GetThread(tr.Threads[0])
		if err != nil {
			return err
		}
		if th != nil && th.Eval != nil && slices.Contains(th.Eval.TriggerIDs, tr.ID) {
			reason := Reason{Code: CodeRouted, Values: map[string]string{"ticket": t.ID}}
			if len(th.Eval.TriggerIDs) == 1 {
				_, err = o.finish(th, th.Eval, &Decision{Kind: DecisionIgnore, Reasons: []Reason{reason}})
			} else {
				reason.TriggerID = tr.ID
				_, err = o.reassess(th, reason, tr.ID)
			}
			if err != nil {
				return err
			}
		}
		o.wake(t)
		return o.tx.PutTicket(t)
	})
}

// --- The Executor ---

// Handback records that a Ticket hands back, once and with a reason, a Trigger that reached it, and queues it for a
// fresh Eval on its primary Thread once no other Ticket decides it, if this Ticket did; it answers whether it queued
// it. The Ticket keeps its Subscription.
func (e *Engine) Handback(ticketID, triggerID, reason string) (bool, error) {
	if strings.TrimSpace(reason) == "" {
		return false, userErrorf("a hand-back needs a reason")
	}
	var queued bool
	err := e.command(cmdHandback, func(o *op) error {
		queued = false
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		tr, err := o.tx.GetTrigger(triggerID)
		if err != nil {
			return err
		}
		if tr == nil {
			return o.unknownTrigger(t, triggerID)
		}
		// An owner Trigger has no Thread to queue it on: handed back, nobody would ever decide it.
		if tr.Source == ownerSource {
			return userErrorf("messages from the owner can't be handed back; say in your session output that it is out of scope")
		}
		if !slices.ContainsFunc(t.Inputs, func(in Input) bool { return in.Kind == InputTrigger && in.TriggerID == triggerID }) {
			return userErrorf("trigger %s did not reach ticket %s; %s", triggerID, ticketID, o.triggerChoices(t))
		}
		if _, back := tr.Disposition.HandedBack[ticketID]; back {
			return userErrorf("ticket %s handed trigger %s back already", ticketID, triggerID)
		}
		queued, err = o.handback(ticketID, tr, reason)
		return err
	})
	return queued, err
}

// handback records that a Ticket hands a Trigger back with a reason and queues it for Eval on its primary Thread if
// the Ticket decided it and no other Ticket does now; it reports whether it queued it, and puts the Trigger.
func (o *op) handback(ticketID string, tr *Trigger, reason string) (bool, error) {
	var primary *Thread
	var err error
	if len(tr.Threads) > 0 {
		if primary, err = o.thread(tr.Threads[0]); err != nil {
			return false, err
		}
	}
	before, err := followupOwners(tr, o.tx.GetTicket, primary)
	if err != nil {
		return false, err
	}
	if tr.Disposition.HandedBack == nil {
		tr.Disposition.HandedBack = map[string]string{}
	}
	tr.Disposition.HandedBack[ticketID] = reason
	after, err := followupOwners(tr, o.tx.GetTicket, primary)
	if err != nil {
		return false, err
	}
	queued := false
	if primary != nil && slices.Contains(before, ticketID) && len(after) == 0 && queueForEval(primary, tr.ID) {
		queued = true
		if err := o.tx.PutThread(primary); err != nil {
			return false, err
		}
	}
	return queued, o.tx.PutTrigger(tr)
}

// --- People ---

// Reevaluate is a person's replay: it queues a stored Trigger for a fresh Eval on its primary Thread and records the
// requeue, after which no Ticket's worker decides it. A Trigger an Eval holds is refused. The main Session's requeue
// (origin OriginRouter) is refused for a follow-up a Ticket's worker decides: only the worker hands it back, while a
// person may still send it to Eval (reference orchestrator_trigger_requeue). The main Session hears of a person's.
func (e *Engine) Reevaluate(triggerID string, origin ...CommandOrigin) error {
	return e.command(cmdReevaluate, func(o *op) error {
		o.origin = originOf(origin)
		tr, err := o.tx.GetTrigger(triggerID)
		if err != nil {
			return err
		}
		if tr == nil {
			return userErrorf("trigger %s not found", triggerID)
		}
		if len(tr.Threads) == 0 {
			return userErrorf("trigger %s belongs to no thread", triggerID)
		}
		th, err := o.thread(tr.Threads[0])
		if err != nil {
			return err
		}
		if th.Eval != nil && slices.Contains(th.Eval.TriggerIDs, triggerID) {
			return userErrorf("an eval holds trigger %s already", triggerID)
		}
		if o.origin == OriginRouter {
			owners, err := followupOwners(tr, o.tx.GetTicket, th)
			if err != nil {
				return err
			}
			if len(owners) > 0 {
				return userErrorf("%s: ticket %s decides trigger %s; its worker hands it back if it is not its own",
					CodeWorkerDecides, strings.Join(owners, ", "), triggerID)
			}
		}
		queueForEval(th, triggerID)
		if err := o.tx.PutThread(th); err != nil {
			return err
		}
		tr.Disposition.Requeued = o.timestamp()
		if err := o.tx.PutTrigger(tr); err != nil {
			return err
		}
		o.emitEvent(Event{Type: EventTriggerRequeued, At: o.at, Data: map[string]any{"triggerId": triggerID, "thread": th.Key}})
		return o.notice("trigger_requeued:"+triggerID+":"+stamp(o.at),
			Message{Kind: NoticeTriggerRequeued, TriggerID: triggerID, Thread: th.Key, At: o.at})
	})
}

// Dismiss records that a person dismissed the problem an unavailable Decision shows.
func (e *Engine) Dismiss(decisionID string) error {
	return e.command(cmdDismiss, func(o *op) error {
		d, err := o.tx.GetDecision(decisionID)
		if err != nil {
			return err
		}
		if d == nil || d.Kind != DecisionUnavailable {
			return userErrorf("decision %s is not an unavailable decision", decisionID)
		}
		if d.Dismissed != nil {
			return nil
		}
		d.Dismissed = o.timestamp()
		return o.tx.PutDecision(d)
	})
}

// --- Settings ---

// PolicyChanged tells each open Ticket in notices how the change to version bears on it, oldest first, as a note
// Input with the notice's text (computed by the caller with policy.Effects), and restarts every Eval in progress: each
// records unavailable and its batch is queued again, under the new version at the next pass.
func (e *Engine) PolicyChanged(version string, notices map[string]string) error {
	return e.command(cmdPolicyChanged, func(o *op) error {
		open, err := o.tx.OpenTickets()
		if err != nil {
			return err
		}
		slices.SortStableFunc(open, func(a, b *Ticket) int {
			if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
		for _, t := range open {
			text := notices[t.ID]
			if strings.TrimSpace(text) == "" {
				continue
			}
			in := Input{ID: newID(), Kind: InputNote, At: o.at, Text: text}
			o.addInput(t, in)
			o.event(EventInputAdded, t, "", map[string]any{"inputId": in.ID, "policyVersion": version})
			if err := o.tx.PutTicket(t); err != nil {
				return err
			}
		}
		threads, err := o.tx.ActiveThreads()
		if err != nil {
			return err
		}
		for _, th := range threads {
			if th.Eval == nil {
				continue
			}
			if _, err := o.reassess(th, Reason{Code: CodePolicyChanged}); err != nil {
				return err
			}
		}
		o.emitEvent(Event{Type: EventPolicyChanged, At: o.at, Data: map[string]any{"version": version}})
		return nil
	})
}

// --- Releasing follow-ups on a Ticket's end ---

// unread returns, for a Ticket about to end, the Triggers of its Inputs that still wait for its worker (waiting) and
// the person's follow-ups it decides that its worker neither read before its latest report or turn end nor took up by
// acknowledging or answering them (left). Handed-back follow-ups are not the Ticket's to decide.
func (o *op) unread(t *Ticket) (waiting, left []string, err error) {
	until := readUntil(t, true)
	lookup := func(id string) (*Ticket, error) {
		if id == t.ID {
			return t, nil
		}
		return o.tx.GetTicket(id)
	}
	seen := map[string]bool{}
	for i := range t.Inputs {
		in := &t.Inputs[i]
		if in.Kind != InputTrigger || in.TriggerID == "" {
			continue
		}
		undelivered := slices.Contains(t.Undelivered, in.ID)
		if undelivered && !slices.Contains(waiting, in.TriggerID) {
			waiting = append(waiting, in.TriggerID)
		}
		if (!undelivered && readBefore(in, until)) || seen[in.TriggerID] {
			continue
		}
		seen[in.TriggerID] = true
		tr, err := o.tx.GetTrigger(in.TriggerID)
		if err != nil {
			return nil, nil, err
		}
		if tr == nil || !tr.Human || len(tr.Threads) == 0 || slices.Contains(tr.Disposition.Acknowledged, t.ID) {
			continue
		}
		primary, err := o.tx.GetThread(tr.Threads[0])
		if err != nil {
			return nil, nil, err
		}
		owners, err := followupOwners(tr, lookup, primary)
		if err != nil {
			return nil, nil, err
		}
		if slices.Contains(owners, t.ID) {
			left = append(left, tr.ID)
		}
	}
	return waiting, left, nil
}

// letGo is the release of an ended Ticket (see unread): it no longer counts as having reached the Triggers waiting and
// left, and each of left goes to Eval on its primary Thread unless another Ticket decides it, a Ticket took it up, or a
// Decision settled it; a stop that ended this Ticket on the message settles it unless a Ticket handed it back.
func (o *op) letGo(t *Ticket, waiting, left []string) error {
	for _, id := range append(slices.Clone(waiting), left...) {
		tr, err := o.tx.GetTrigger(id)
		if err != nil {
			return err
		}
		// A Trigger without a Thread (the owner's) was never fanned out: it keeps where it went.
		if tr == nil || len(tr.Threads) == 0 || !slices.Contains(tr.Disposition.Reached, t.ID) {
			continue
		}
		tr.Disposition.Reached = without(tr.Disposition.Reached, t.ID)
		tr.Disposition.Context = without(tr.Disposition.Context, t.ID)
		if err := o.tx.PutTrigger(tr); err != nil {
			return err
		}
	}
	lookup := func(id string) (*Ticket, error) {
		if id == t.ID {
			return t, nil
		}
		return o.tx.GetTicket(id)
	}
	for _, id := range left {
		tr, err := o.tx.GetTrigger(id)
		if err != nil {
			return err
		}
		if tr == nil || len(tr.Disposition.Acknowledged) > 0 {
			continue
		}
		th, err := o.thread(tr.Threads[0])
		if err != nil {
			return err
		}
		owners, err := followupOwners(tr, lookup, th)
		if err != nil {
			return err
		}
		if len(owners) > 0 {
			continue
		}
		decided, err := o.decided(tr, t.ID)
		if err != nil {
			return err
		}
		if !decided && queueForEval(th, id) {
			if err := o.tx.PutThread(th); err != nil {
				return err
			}
		}
	}
	return nil
}

// decided reports whether the latest Decision on a Trigger settled it for a Ticket ending: one that settled what it
// asks for (settled), or a stop of that Ticket on it that no Ticket handed back.
func (o *op) decided(tr *Trigger, ticketID string) (bool, error) {
	ds, err := o.tx.Decisions(tr.Threads[0], 0)
	if err != nil {
		return false, err
	}
	for _, d := range ds {
		if !slices.Contains(d.TriggerIDs, tr.ID) {
			continue
		}
		stopped := d.Kind == DecisionStop && d.TicketID == ticketID && len(tr.Disposition.HandedBack) == 0
		return settled(d) || stopped, nil
	}
	return false, nil
}

// settled reports whether a Decision resolved what a person's message asks for: a stop, taken or refused, decides only
// that Ticket's work, routing leaves it to the Ticket's worker, and unavailable decided nothing.
func settled(d *Decision) bool {
	if d.Kind == DecisionStop || d.Kind == DecisionUnavailable {
		return false
	}
	for _, r := range d.Reasons {
		switch r.Code {
		case CodeStopNotExplicit, CodeStopUnknown, CodeRouted:
			return false
		}
	}
	return true
}

// --- Helpers ---

// record stores a Decision made on a Thread now and lists it on the Thread; the caller puts the Thread.
func (o *op) record(th *Thread, d *Decision) error {
	d.ID = newID()
	d.At = o.at
	d.Thread = th.Key
	th.Decisions = append(th.Decisions, d.ID)
	if err := o.tx.PutDecision(d); err != nil {
		return err
	}
	data := map[string]any{"decisionId": d.ID, "kind": d.Kind, "thread": th.Key, "triggerIds": slices.Clone(d.TriggerIDs)}
	ev := Event{Type: EventDecisionRecorded, TicketID: d.TicketID, At: o.at, Data: data}
	o.emitEvent(ev)
	return nil
}

// finish records an Eval's Decision and puts the Thread: the Thread's own Eval ends with it (and its expiry count
// resets), while a gate-only one leaves it going and takes its Trigger out of the queue.
func (o *op) finish(th *Thread, ev *Evaluation, d *Decision) (*Decision, error) {
	d.TriggerIDs = slices.Clone(ev.TriggerIDs)
	if err := o.record(th, d); err != nil {
		return nil, err
	}
	o.evalDone(ev)
	if th.Eval != nil && th.Eval.ID == ev.ID {
		th.Eval = nil
		th.Expired = 0
	} else {
		th.Queued = without(th.Queued, ev.TriggerIDs...)
	}
	return d, o.tx.PutThread(th)
}

// enqueueAssessment queues the effect that steers an Eval's candidate into the main Session.
func (o *op) enqueueAssessment(ev *Evaluation) error {
	payload, err := json.Marshal(struct {
		EvalID    string `json:"evalId"`
		TriggerID string `json:"triggerId"`
	}{ev.ID, ev.Primary})
	if err != nil {
		return err
	}
	return o.enqueue(EffectAssessment, ev.ID, payload)
}

// ResumeAssessments is startup's check of the Evals in progress: one whose candidate never reached the main Session
// still needs its assessment effect, which its start queued in the same transaction. pending names the Evals whose
// effect waits; each other undelivered Eval has it queued again, and their IDs are returned (normally none). A
// delivered Eval keeps its deadline and needs nothing.
func (e *Engine) ResumeAssessments(pending map[string]bool) ([]string, error) {
	var resumed []string
	err := e.command(cmdEvaluateDue, func(o *op) error {
		threads, err := o.tx.ActiveThreads()
		if err != nil {
			return err
		}
		for _, th := range threads {
			if ev := th.Eval; ev != nil && ev.DeliveredAt == nil && !pending[ev.ID] {
				if err := o.enqueueAssessment(ev); err != nil {
					return err
				}
				resumed = append(resumed, ev.ID)
			}
		}
		return nil
	})
	return resumed, err
}

// evalDone records eval_seconds, from an Eval's start to its Decision, once the command commits.
func (o *op) evalDone(ev *Evaluation) {
	if o.reg == nil || ev == nil || ev.StartedAt.IsZero() {
		return
	}
	d := nonNegative(o.at.Sub(ev.StartedAt))
	o.observe(func(r *metrics.Registry) { r.Histogram("eval_seconds").Observe(d) })
}

// reassess ends a Thread's Eval as unavailable for reason and queues its Triggers but skip again, ahead of the others.
func (o *op) reassess(th *Thread, reason Reason, skip ...string) (*Decision, error) {
	ids := without(th.Eval.TriggerIDs, skip...)
	d := &Decision{Kind: DecisionUnavailable, TriggerIDs: slices.Clone(th.Eval.TriggerIDs), Reasons: []Reason{reason}}
	if err := o.record(th, d); err != nil {
		return nil, err
	}
	o.evalDone(th.Eval)
	th.Eval = nil
	th.Expired = 0
	th.Queued = requeueFirst(th.Queued, ids)
	return d, o.tx.PutThread(th)
}

// acceptedOn returns, by Trigger, the accept Decision that started a Ticket from it, among a Thread's Decisions: an
// Eval and its Decision run on its Triggers' primary Thread.
func (o *op) acceptedOn(thread string) (map[string]*Decision, error) {
	ds, err := o.tx.Decisions(thread, 0)
	if err != nil {
		return nil, err
	}
	out := map[string]*Decision{}
	for _, d := range ds {
		if d.Kind != DecisionAccept || d.TicketID == "" {
			continue
		}
		for _, id := range d.TriggerIDs {
			if _, ok := out[id]; !ok {
				out[id] = d
			}
		}
	}
	return out, nil
}

// ticketsOn returns the Tickets subscribed to a Thread.
func (o *op) ticketsOn(th *Thread) ([]Ticket, error) {
	var out []Ticket
	for _, s := range th.Subscriptions {
		t, err := o.tx.GetTicket(s.TicketID)
		if err != nil {
			return nil, err
		}
		if t != nil {
			out = append(out, *t)
		}
	}
	return out, nil
}

// subscribe subscribes a Ticket to a Thread as its own (no longer for context), own when the Ticket opened it.
func (o *op) subscribe(ticketID, key string, own bool) error {
	th, err := o.thread(key)
	if err != nil {
		return err
	}
	if s := subscription(th, ticketID); s != nil {
		s.Context = false
		s.Own = s.Own || own
	} else {
		th.Subscriptions = append(th.Subscriptions, Subscription{TicketID: ticketID, Own: own})
		o.emitEvent(Event{Type: EventThreadSubscribed, TicketID: ticketID, At: o.at, Data: map[string]any{"thread": key, "own": own}})
	}
	return o.tx.PutThread(th)
}

// currentPolicy returns the Engine's current Policy, or nil without one.
func (o *op) currentPolicy() Policy {
	if o.policy == nil {
		return nil
	}
	return o.policy()
}

func saidReason(text string) []Reason {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []Reason{{Code: CodeSaid, Values: map[string]string{"text": text}}}
}

// without returns list without the given items, or nil when nothing is left.
func without(list []string, items ...string) []string {
	var out []string
	for _, s := range list {
		if !slices.Contains(items, s) {
			out = append(out, s)
		}
	}
	return out
}

// requeueFirst puts ids ahead of the queue, each once.
func requeueFirst(queue, ids []string) []string {
	out := slices.Clone(ids)
	for _, id := range queue {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func sameChannel(a, b ChannelRef) bool {
	return a.Source == b.Source && a.Thread == b.Thread && maps.Equal(a.Address, b.Address)
}
