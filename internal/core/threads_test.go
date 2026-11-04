package core

import (
	"slices"
	"testing"
)

// unsubscribedOf returns the tickets of the thread.unsubscribed Events on a Thread, in order.
func (h *harness) unsubscribedOf(thread string) []string {
	var out []string
	for _, ev := range h.eventsOf(EventThreadUnsubscribed) {
		if ev.Data["thread"] == thread {
			out = append(out, ev.TicketID)
		}
	}
	return out
}

func subscribers(th *Thread) []string {
	if th == nil {
		return nil
	}
	var out []string
	for _, s := range th.Subscriptions {
		out = append(out, s.TicketID)
	}
	return out
}

// Reference 007 markUnrelated, 002 "Handing back": an attached Thread found unrelated drops only that Ticket's
// Subscription and hands back, with the reason, the follow-ups there the Ticket decides.
func TestMarkUnrelatedHandsBackOwned(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	b := h.acceptRequest("c:2", pull2)
	h.subscribe(a, other, false) // attached, as Attach would
	h.subscribe(b, other, false)
	h.receive(h.message("c:3", other)) // owned by a and b
	h.receive(h.message("c:4", issue)) // a's own follow-up on its origin Thread
	h.settle(true)
	eq(t, "owned, not queued", h.queued(other), []string(nil))

	wantUserError(t, "no reason", h.eng.MarkUnrelated(a, other, " "))
	wantUserError(t, "not subscribed", h.eng.MarkUnrelated(a, pull1, "unrelated"))
	wantUserError(t, "unknown ticket", h.eng.MarkUnrelated("T-9999", other, "unrelated"))

	h.must(h.eng.MarkUnrelated(a, other, "unrelated"))
	eq(t, "only a's subscription dropped", subscribers(h.repos.thread(other)), []string{b})
	eq(t, "handed back with the reason", h.disposition("c:3").HandedBack, map[string]string{a: "unrelated"})
	eq(t, "b still decides it", h.queued(other), []string(nil))
	eq(t, "the issue's follow-up stays a's", h.disposition("c:4").HandedBack, map[string]string(nil))
	eq(t, "event", h.unsubscribedOf(other), []string{a})

	// Once b finds it unrelated too, no Ticket decides c:3 and it goes to Eval: Task 5's hand-back.
	h.must(h.eng.MarkUnrelated(b, other, "not mine either"))
	eq(t, "queued once no ticket decides it", h.queued(other), []string{"c:3"})
	eq(t, "no subscriptions left", subscribers(h.repos.thread(other)), []string(nil))

	// A message on the Thread now reaches neither Ticket.
	h.receive(h.message("c:5", other))
	eq(t, "reaches no ticket", h.disposition("c:5").Reached, []string(nil))
}

// A Ticket cannot drop its origin Thread, nor a Thread a Source bound it to.
func TestCannotUnrelateOriginThread(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	wantUserError(t, "mandate thread", h.eng.MarkUnrelated(a, issue, "unrelated"))
	eq(t, "kept", subscribers(h.repos.thread(issue)), []string{a})

	// The origin Trigger's primary Thread, when the Mandate names another one.
	h.must(h.repos.Tx(func(tx Tx) error {
		tk, err := tx.GetTicket(a)
		if err != nil {
			return err
		}
		tk.Mandate.Thread = other
		return tx.PutTicket(tk)
	}))
	wantUserError(t, "origin primary thread", h.eng.MarkUnrelated(a, issue, "unrelated"))

	h.must(h.eng.BindThread(a, pull1, true))
	wantUserError(t, "bound thread", h.eng.MarkUnrelated(a, pull1, "unrelated"))
	eq(t, "bound kept", subscribers(h.repos.thread(pull1)), []string{a})
	eq(t, "no events", h.unsubscribedOf(issue) == nil && h.unsubscribedOf(pull1) == nil, true)
}

// Reference 002 "Context-only subscriptions": a Thread followed for context sends its messages to the Ticket but lends
// it no follow-up strength and does not resume it.
func TestFollowForContextNoResume(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	wantUserError(t, "bad key", h.eng.FollowForContext(a, "nocolon"))
	wantUserError(t, "unknown ticket", h.eng.FollowForContext("T-9999", other))

	h.must(h.eng.FollowForContext(a, other))
	eq(t, "context subscription", h.repos.thread(other).Subscriptions, []Subscription{{TicketID: a, Context: true}})
	eq(t, "no follow", h.ticket(a).Follows, []Follow(nil))
	h.must(h.eng.FollowForContext(a, other)) // again: unchanged
	eq(t, "once", len(h.repos.thread(other).Subscriptions), 1)
	// Its own Thread stays its own.
	h.must(h.eng.FollowForContext(a, issue))
	eq(t, "own stays own", h.repos.thread(issue).Subscriptions, []Subscription{{TicketID: a, Own: true}})

	h.receive(h.message("c:2", other))
	in := inputsOf(h.ticket(a), "c:2")
	eq(t, "reached for context", len(in) == 1 && in[0].Context, true)
	eq(t, "not owned: queued for eval", h.queued(other), []string{"c:2"})

	h.must(h.eng.FileReport(a, ReportInput{State: ReportReady, Summary: "done"}))
	eq(t, "ended", h.ticket(a).Ended != nil, true)
	wantUserError(t, "ended ticket", h.eng.FollowForContext(a, pull1))
	h.receive(h.message("c:3", other))
	tk := h.ticket(a)
	eq(t, "not resumed", tk.Ended != nil, true)
	eq(t, "no new assignment", len(tk.Assignments), 1)
	eq(t, "queued for eval", slices.Contains(h.queued(other), "c:3"), true)
}

// Reference 007 unsubscribe: every Subscription on the Thread goes, the Tickets that followed it follow it no more, and
// each keeps deciding the follow-ups it received there.
func TestUnsubscribeDropsAll(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	b := h.acceptRequest("c:2", other)
	h.must(h.eng.BindThread(a, pull1, true))
	h.subscribe(b, pull1, false)
	h.receive(h.message("c:3", pull1))
	eq(t, "reached both", h.disposition("c:3").Reached, []string{a, b})

	h.must(h.eng.Unsubscribe(pull1))
	eq(t, "no subscriptions", subscribers(h.repos.thread(pull1)), []string(nil))
	eq(t, "a follows it no more", h.ticket(a).Follows, []Follow(nil))
	eq(t, "events", h.unsubscribedOf(pull1), []string{a, b})
	eq(t, "still reached", h.disposition("c:3").Reached, []string{a, b})
	// They still decide c:3: the main Session may only stop.
	r := h.decide(DecisionInput{TriggerID: "c:3", Kind: "ignore"})
	eq(t, "worker decides", r.Refused != "", true)

	// Its close no longer ends a, and a new message reaches no Ticket.
	h.closePull(pull1, OutcomeMerged)
	eq(t, "a open", h.ticket(a).Ended == nil, true)
	h.receive(h.message("c:4", pull1))
	eq(t, "reaches no ticket", h.disposition("c:4").Reached, []string(nil))
	eq(t, "queued for eval", h.queued(pull1), []string{"c:4"})

	h.must(h.eng.Unsubscribe(pull1)) // nothing left: a no-op
	h.must(h.eng.Unsubscribe("github:o/r#404"))
	eq(t, "no more events", len(h.unsubscribedOf(pull1)), 2)
	wantUserError(t, "bad key", h.eng.Unsubscribe("nocolon"))
}

// Reference 007 note: a fact from other work reaches the Ticket's worker as a note Input, which grants nothing.
func TestNoteAddsContextInput(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	wantUserError(t, "blank", h.eng.Note(a, "  "))
	wantUserError(t, "unknown", h.eng.Note("T-9999", "x"))
	h.must(h.eng.Note(a, "T-0002 changed the login API"))
	tk := h.ticket(a)
	last := tk.Inputs[len(tk.Inputs)-1]
	eq(t, "note", [2]string{string(last.Kind), last.Text}, [2]string{string(InputNote), "T-0002 changed the login API"})
	eq(t, "no trigger", last.TriggerID, "")
	eq(t, "waits for delivery", slices.Contains(tk.Undelivered, last.ID), true)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportReady}))
	wantUserError(t, "ended", h.eng.Note(a, "x"))
}

// Reference 007 requestStatus: only a running Assignment whose worker is idle is asked.
func TestRequestStatusOnlyIdle(t *testing.T) {
	h := newEvals(t)
	a := h.acceptRequest("c:1", issue)
	_, err := h.eng.RequestStatus("T-9999")
	wantUserError(t, "unknown", err)

	asked, err := h.eng.RequestStatus(a)
	h.must(err)
	eq(t, "a turn is in progress", asked, false)

	h.tick(1)
	h.must(h.eng.TurnEnded(a, runningOf(h.ticket(a)).ID))
	n := len(h.ticket(a).Inputs)
	asked, err = h.eng.RequestStatus(a)
	h.must(err)
	eq(t, "idle: asked", asked, true)
	tk := h.ticket(a)
	eq(t, "one input", len(tk.Inputs), n+1)
	last := tk.Inputs[len(tk.Inputs)-1]
	eq(t, "text input", last.Kind, InputText)
	eq(t, "asks for a report", last.Text, statusRequest)
	asked, err = h.eng.RequestStatus(a)
	h.must(err)
	eq(t, "the request waits for delivery: not idle", asked, false)

	h.settle(true) // delivered after the turn ended
	asked, err = h.eng.RequestStatus(a)
	h.must(err)
	eq(t, "delivered since the turn ended", asked, false)

	h.must(h.eng.FileReport(a, ReportInput{State: ReportReady}))
	asked, err = h.eng.RequestStatus(a)
	h.must(err)
	eq(t, "ended", asked, false)
}
