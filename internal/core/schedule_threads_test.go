package core

import (
	"fmt"
	"testing"
)

const night = "schedule:night"

// firing is a Trigger an Autopilot entry raised on its Thread: not a person's, so it resumes no ended Ticket.
func (h *evals) firing(id string) Trigger {
	return Trigger{ID: id, Source: SourceSchedule, Kind: KindSchedule, Actor: ActorRef{Source: SourceSchedule, ID: "night"},
		Scope: "schedule", Threads: []string{night}, At: h.now(), Text: "tidy the docs", Meta: map[string]string{"repo": "o/r"}}
}

// run fires the entry once and accepts the firing; it returns the new Ticket's ID.
func (h *evals) run(id string) string {
	h.t.Helper()
	return h.acceptOn(h.firing(id))
}

// acceptOn receives a Trigger no person sent, evaluates it and accepts it; it returns the new Ticket's ID.
func (h *evals) acceptOn(tr Trigger) string {
	h.t.Helper()
	h.pol.gates[tr.ID] = Gate{Strength: StrengthExplicit, Basis: MandateBasis{Via: ViaAddressed, Source: tr.Source}}
	h.receive(tr)
	h.due()
	r := h.decide(DecisionInput{TriggerID: tr.ID, Kind: "accept", Title: "Tidy " + tr.ID})
	if r.Ticket == nil {
		h.t.Fatalf("accept of %s created no ticket: %+v", tr.ID, r)
	}
	h.settle(true)
	h.tick(1)
	return r.Ticket.ID
}

// finishedRun is a run whose Ticket opened pull and ended when it merged, so a person's comment there can resume it.
func (h *evals) finishedRun(id, pull string) string {
	h.t.Helper()
	tk := h.run(id)
	h.must(h.eng.BindThread(tk, pull, true))
	h.closePull(pull, OutcomeMerged)
	h.tick(1)
	return tk
}

func (h *evals) busy(thread string, want bool, why string) {
	h.t.Helper()
	got, err := h.eng.ThreadBusy(thread)
	if err != nil || got != want {
		h.t.Fatalf("%s: ThreadBusy(%s) = %v, %v; want %v", why, thread, got, err, want)
	}
}

// A permanent Thread does not collect one Subscription per run: an accept lets go of the runs that ended.
func TestScheduleThreadKeepsOnlyItsLatestEndedRun(t *testing.T) {
	h := newEvals(t)
	var last string
	for i := range 5 {
		last = h.finishedRun(fmt.Sprintf("schedule:night:%d", i), fmt.Sprintf("github:o/r#%d", 100+i))
	}
	eq(t, "subscriptions", subscribers(h.repos.thread(night)), []string{last})
	h.busy(night, false, "every run ended")
}

// A person's comment on a finished run's pull request resumes it long after a later accept let go of it: the entry
// has a run going again, and must not fire another.
func TestResumedRunCountsAsBusy(t *testing.T) {
	h := newEvals(t)
	first := h.finishedRun("schedule:night:1", pull1)
	second := h.finishedRun("schedule:night:2", pull2)
	eq(t, "the first run forgotten", subscribers(h.repos.thread(night)), []string{second})
	h.busy(night, false, "both runs ended")

	h.receive(h.message("c:1", pull1))
	eq(t, "resumed", h.ticket(first).Ended, (*Ended)(nil))
	h.busy(night, true, "a run resumed")
	eq(t, "subscribed again", subscription(h.repos.thread(night), first), &Subscription{TicketID: first, Own: true})

	h.must(h.eng.FileReport(first, ReportInput{State: ReportReady, Summary: "done again"}))
	h.closePull(pull1, OutcomeMerged)
	h.busy(night, false, "the resumed run ended again")
}

// A run's Subscriptions go, a Ticket attached to the Thread's stay: a person's comment can still resume that one.
func TestAcceptKeepsTicketsAttachedToTheScheduleThread(t *testing.T) {
	h := newEvals(t)
	attached := h.sourceTicket(issue)
	h.must(h.repos.Tx(func(tx Tx) error {
		return tx.PutThread(&Thread{Key: night, Source: SourceSchedule, Subscriptions: []Subscription{{TicketID: attached}}})
	}))
	h.must(h.eng.BindThread(attached, pull1, true))
	h.closePull(pull1, OutcomeMerged)
	eq(t, "attached ticket ended", h.ticket(attached).Ended != nil, true)
	last := h.finishedRun("schedule:night:1", pull2)
	eq(t, "attached ticket kept", subscribers(h.repos.thread(night)), []string{attached, last})
}

// Nothing changes on a Thread that is not an entry's: its ended Tickets keep their Subscriptions.
func TestGitHubThreadKeepsEndedRunsAndAllItsDecisions(t *testing.T) {
	h := newEvals(t)
	var want []string
	total := scheduleDecisionsKept + 1
	for i := range total {
		tr := h.firing(fmt.Sprintf("c:%d", i))
		tr.Source, tr.Threads = "github", []string{issue}
		tk := h.acceptOn(tr)
		h.must(h.eng.BindThread(tk, pull1, true))
		h.closePull(pull1, OutcomeMerged)
		h.tick(1)
		want = append(want, tk)
	}
	th := h.repos.thread(issue)
	eq(t, "every ended ticket still subscribed", subscribers(th), want)
	eq(t, "every decision listed", len(th.Decisions), total)
}

// An Eval start and an accept on a permanent Thread read the Decisions of their Triggers, not the Thread's history.
func TestScheduleEvalDoesNotLoadTheThreadsDecisions(t *testing.T) {
	h := newEvals(t)
	for i := range 3 {
		h.finishedRun(fmt.Sprintf("schedule:night:%d", i), fmt.Sprintf("github:o/r#%d", 100+i))
	}
	h.repos.decisionScans = 0
	h.run("schedule:night:next")
	eq(t, "decisions of the whole thread loaded", h.repos.decisionScans, 0)
}

// Requeued by a person long after, a firing that already started a Ticket starts no second one.
func TestRequeuedFiringStartsNoSecondTicket(t *testing.T) {
	h := newEvals(t)
	first := h.finishedRun("schedule:night:0", pull1)
	for i := 1; i < 4; i++ {
		h.finishedRun(fmt.Sprintf("schedule:night:%d", i), fmt.Sprintf("github:o/r#%d", 100+i))
	}
	h.must(h.eng.Reevaluate("schedule:night:0"))
	h.due()
	eq(t, "no eval", h.evalOf(night), (*Evaluation)(nil))
	ds := h.decisionsOn(night)
	eq(t, "ignored as started", ds[len(ds)-1].Reasons, []Reason{{Code: CodeStartedTicket, TriggerID: "schedule:night:0",
		Values: map[string]string{"ticket": first}}})
}

// A permanent Thread keeps the IDs of its latest Decisions only; the Decisions themselves stay, for Routing.
func TestScheduleThreadListsItsLatestDecisions(t *testing.T) {
	h := newEvals(t)
	total := scheduleDecisionsKept + 3
	for i := range total {
		h.finishedRun(fmt.Sprintf("schedule:night:%d", i), fmt.Sprintf("github:o/r#%d", 100+i))
	}
	th := h.repos.thread(night)
	eq(t, "listed", len(th.Decisions), scheduleDecisionsKept)
	ds := h.decisionsOn(night)
	eq(t, "all stored", len(ds), total)
	eq(t, "latest listed last", th.Decisions[len(th.Decisions)-1], ds[len(ds)-1].ID)
}
