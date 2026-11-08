package core

import (
	"testing"
)

// waiting stores an open Ticket of repo that waits on the owner: a question (answer), or a pending Approval.
func (h *evals) waiting(id, repo string, approval bool) {
	h.t.Helper()
	tk := &Ticket{ID: id, Title: id, Repo: repo, Attention: &Attention{ID: "q-" + id, Question: "which?"}}
	h.must(h.repos.Tx(func(tx Tx) error {
		if approval {
			tk.Attention = nil
			tk.PendingApproval = "ap-" + id
			if err := tx.PutApproval(&Approval{ID: "ap-" + id, TicketID: id, State: ApprovalPending}); err != nil {
				return err
			}
		}
		return tx.PutTicket(tk)
	}))
}

// caughtUp makes a waiting Ticket working again, as if the owner had answered it.
func (h *evals) caughtUp(id string) {
	h.t.Helper()
	h.must(h.repos.Tx(func(tx Tx) error {
		tk, err := tx.GetTicket(id)
		if err != nil {
			return err
		}
		tk.Attention, tk.PendingApproval = nil, ""
		if a, _ := tx.GetApproval("ap-" + id); a != nil {
			a.State = ApprovalDenied
			if err := tx.PutApproval(a); err != nil {
				return err
			}
		}
		return tx.PutTicket(tk)
	}))
}

func (h *evals) brakes() []Brake {
	h.t.Helper()
	var out []Brake
	h.must(h.repos.Tx(func(tx Tx) (err error) { out, err = tx.Brakes(); return }))
	return out
}

func (h *evals) botCandidate(id, thread string) Trigger {
	tr := h.botMessage(id, thread)
	tr.Meta = map[string]string{"repo": "acme/web"}
	return tr
}

func TestOwedAttentionCountsEachTicketOnce(t *testing.T) {
	review := Assignment{ID: "a", State: AssignmentRunning, Report: &Report{State: ReportReady}}
	tickets := []Ticket{
		{ID: "A", Repo: "acme/web", PendingApproval: "ap"},
		{ID: "B", Repo: "acme/web", Assignments: []Assignment{review}},
		{ID: "C", Repo: "acme/web", Assignments: []Assignment{{ID: "c", State: AssignmentRunning}}},
		{ID: "D", Repo: "acme/api", Attention: &Attention{ID: "q"}},
	}
	approvals := []Approval{
		{ID: "ap", TicketID: "A", State: ApprovalPending},
		{ID: "ap2", TicketID: "A", State: ApprovalPending},
		{ID: "ap3", TicketID: "C", State: ApprovalPending}, // C is working, but an Approval of it waits too
	}
	eq(t, "acme/web", OwedAttention(tickets, approvals, "acme/web"), 3)
	eq(t, "acme/api", OwedAttention(tickets, approvals, "acme/api"), 1)
	eq(t, "elsewhere", OwedAttention(tickets, approvals, "acme/none"), 0)
}

func TestInferredCandidateBrakedAtCeiling(t *testing.T) {
	h := newEvals(t)
	h.waiting("T1", "acme/web", true)
	h.waiting("T2", "acme/web", false)
	h.waiting("T3", "acme/web", false)
	h.receive(h.botCandidate("c:1", issue))
	h.due()
	eq(t, "no eval", h.evalOf(issue), (*Evaluation)(nil))
	eq(t, "no assessment", len(h.repos.effectList()), 0)
	ds := h.decisionsOn(issue)
	if len(ds) != 1 {
		t.Fatalf("decisions = %+v", ds)
	}
	eq(t, "kind", ds[0].Kind, DecisionIgnore)
	eq(t, "triggers", ds[0].TriggerIDs, []string{"c:1"})
	eq(t, "reasons", ds[0].Reasons, []Reason{{Code: "wip_brake",
		Values: map[string]string{"repo": "acme/web", "owed": "3", "ceiling": "3"}}})
	bs := h.brakes()
	if len(bs) != 1 || bs[0].TriggerID != "c:1" || bs[0].Repo != "acme/web" || bs[0].Thread != issue {
		t.Fatalf("brakes = %+v", bs)
	}

	// A thread of another repository, or of none, is not held.
	other := h.botMessage("c:2", pull1)
	other.Meta = map[string]string{"repo": "acme/api"}
	h.receive(other)
	h.receive(h.botMessage("c:3", pull2))
	h.due()
	if h.evalOf(pull1) == nil || h.evalOf(pull2) == nil {
		t.Fatal("a candidate outside the full repository was held")
	}
}

func TestExplicitNeverBraked(t *testing.T) {
	h := newEvals(t)
	for _, id := range []string{"T1", "T2", "T3"} {
		h.waiting(id, "o/r", false)
	}
	h.receive(h.request("c:1", issue))
	h.due()
	if h.evalOf(issue) == nil {
		t.Fatal("an explicit request was braked")
	}
	eq(t, "no brakes", len(h.brakes()), 0)
	if tk := h.submit("Do the thing"); tk.ID == "" {
		t.Fatal("owner task refused")
	}
	eq(t, "still no brakes", len(h.brakes()), 0)
}

func TestBrakeReleasesOnce(t *testing.T) {
	h := newEvals(t)
	h.waiting("T1", "acme/web", true)
	h.waiting("T2", "acme/web", false)
	h.waiting("T3", "acme/web", false)
	h.receive(h.botCandidate("c:1", issue))
	h.tick(1)
	h.receive(h.botCandidate("c:2", pull1))
	h.due()
	eq(t, "both held", len(h.brakes()), 2)

	h.caughtUp("T1") // owed 2 of 3: room for one
	for i := 0; i < 3; i++ {
		h.tick(1)
		h.due()
	}
	evals := 0
	for _, th := range []string{issue, pull1} {
		if h.evalOf(th) != nil {
			evals++
		}
	}
	eq(t, "evals started", evals, 1)
	if h.evalOf(issue) == nil {
		t.Fatal("the older candidate was not the one released")
	}
	eq(t, "brake rows left", len(h.brakes()), 1)
	eq(t, "assessments", len(h.repos.effectList()), 1)

	h.caughtUp("T2")
	h.caughtUp("T3") // owed 0, one Eval running: room for the second
	h.tick(1)
	h.due()
	if h.evalOf(pull1) == nil {
		t.Fatal("the second candidate was not released")
	}
	eq(t, "brakes cleared", len(h.brakes()), 0)
	eq(t, "requeued once each", len(h.eventsOf(EventTriggerRequeued)), 2)
	eq(t, "evals", len(h.eventsOf(EventEvalStarted)), 2)
}

func TestBrakeReleasedWhenCeilingRises(t *testing.T) {
	h := newEvals(t)
	h.pol.ceiling = 1
	h.waiting("T1", "acme/web", false)
	h.receive(h.botCandidate("c:1", issue))
	h.due()
	eq(t, "held", len(h.brakes()), 1)
	h.pol.ceiling = 2
	h.tick(1)
	h.due() // released and evaluated: now the ceiling is 2 and one waits, so it proceeds
	if h.evalOf(issue) == nil {
		t.Fatal("not released")
	}
}

func TestManualReevaluateEvaluatesHeldOnce(t *testing.T) {
	h := newEvals(t)
	h.waiting("T1", "acme/web", false)
	h.waiting("T2", "acme/web", false)
	h.waiting("T3", "acme/web", false)
	h.receive(h.botCandidate("c:1", issue))
	h.due()
	eq(t, "held", len(h.brakes()), 1)
	h.caughtUp("T1")
	h.caughtUp("T2")
	h.caughtUp("T3")
	// The owner re-evaluates it by hand before any release.
	h.must(h.eng.Reevaluate("c:1"))
	h.tick(1)
	for i := 0; i < 4; i++ {
		h.due()
		h.tick(1)
	}
	eq(t, "evals started", len(h.eventsOf(EventEvalStarted)), 1)
	eq(t, "requeued", len(h.eventsOf(EventTriggerRequeued)), 1)
	eq(t, "no brake left", len(h.brakes()), 0)
}

func TestNewCandidateDoesNotJumpHeldOnes(t *testing.T) {
	h := newEvals(t)
	h.waiting("T1", "acme/web", false)
	h.waiting("T2", "acme/web", false)
	h.waiting("T3", "acme/web", false)
	h.receive(h.botCandidate("c:1", issue))
	h.due()
	// Two Evals start in the repository (explicit work is never braked) ...
	for i, th := range []string{pull2, other} {
		r := h.request("x:"+string(rune('1'+i)), th)
		r.Meta = map[string]string{"repo": "acme/web"}
		h.receive(r)
	}
	h.due()
	// ... and the owner catches up to 1 of 3: no room is left to release the held candidate.
	h.caughtUp("T1")
	h.caughtUp("T2")
	h.tick(1)
	h.due()
	eq(t, "held candidate stays", len(h.brakes()), 1)

	// A new candidate arrives while owed < ceiling: it queues behind the held one.
	h.tick(1)
	h.receive(h.botCandidate("c:2", pull1))
	h.due()
	if h.evalOf(pull1) != nil {
		t.Fatal("the new candidate jumped the held one")
	}
	bs := h.brakes()
	if len(bs) != 2 || bs[0].TriggerID != "c:1" || bs[1].TriggerID != "c:2" {
		t.Fatalf("brakes = %+v, want c:1 then c:2", bs)
	}
	ds := h.decisionsOn(pull1)
	eq(t, "reason", ds[0].Reasons, []Reason{{Code: "wip_brake",
		Values: map[string]string{"repo": "acme/web", "owed": "1", "ceiling": "3"}}})

	// The running Evals end; room for one: the older candidate goes first.
	h.decide(DecisionInput{TriggerID: "x:1", Kind: "ignore", Reason: "no"})
	h.decide(DecisionInput{TriggerID: "x:2", Kind: "ignore", Reason: "no"})
	h.pol.ceiling = 2
	h.tick(1)
	h.due()
	if h.evalOf(issue) == nil || h.evalOf(pull1) != nil {
		t.Fatalf("release order wrong: issue eval %v, pull1 eval %v", h.evalOf(issue), h.evalOf(pull1))
	}
	eq(t, "newer still held", len(h.brakes()), 1)
}

func TestBrakeRepoCaseDoesNotReleasePastCeiling(t *testing.T) {
	h := newEvals(t)
	h.pol.ceiling = 1
	h.waiting("T1", "O/R", false)
	a := h.botMessage("c:1", issue)
	a.Meta = map[string]string{"repo": "O/R"}
	b := h.botMessage("c:2", pull1)
	b.Meta = map[string]string{"repo": "o/r"}
	h.receive(a)
	h.tick(1)
	h.receive(b)
	h.due()
	eq(t, "both held", len(h.brakes()), 2)
	h.caughtUp("T1") // room for exactly one
	h.tick(1)
	h.due()
	n := 0
	for _, th := range []string{issue, pull1} {
		if h.evalOf(th) != nil {
			n++
		}
	}
	eq(t, "released", n, 1)
	eq(t, "still held", len(h.brakes()), 1)
}
