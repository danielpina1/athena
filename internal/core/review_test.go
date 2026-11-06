package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// pairedOptions makes every Ticket that names no pairing a review-paired one, with maxRounds rounds.
func pairedOptions(maxRounds int) func(*Options) {
	return func(o *Options) {
		o.Pairing = func() PairingDefaults { return PairingDefaults{Mode: PairingReview, MaxRounds: maxRounds} }
	}
}

// newPairedRouter is an evals harness that relays to the main Session and pairs every Ticket with review by default.
func newPairedRouter(t *testing.T) *evals {
	t.Helper()
	return newEvalsWith(t, func(o *Options) {
		o.Relays = true
		pairedOptions(2)(o)
	})
}

// launch starts the Ticket's latest pending Assignment as the Executor would, its session on runtime, and returns it.
func (h *harness) launch(ticketID, runtime string) string {
	h.t.Helper()
	a := h.ticket(ticketID).LatestAssignment()
	if a == nil || a.State != AssignmentPending {
		h.t.Fatalf("ticket %s has no pending assignment", ticketID)
	}
	c := execCall{TicketID: ticketID, AssignmentID: a.ID}
	h.must(h.eng.AssignmentStarted(c.TicketID, c.AssignmentID, "session-"+c.AssignmentID, "", runtime))
	// As the node does, an implementer's session takes what waits for it into its first step; a review takes nothing.
	if a.Role != RoleReview {
		h.takeWaiting(ticketID, a.ID)
	}
	return c.AssignmentID
}

// takeWaiting delivers what waits for the Ticket to its running Assignment, whose session reads it with what was
// delivered before.
func (h *harness) takeWaiting(ticketID, assignmentID string) {
	h.t.Helper()
	if waiting := h.ticket(ticketID).Undelivered; len(waiting) > 0 {
		h.must(h.eng.InputsDelivered(ticketID, assignmentID, waiting))
	}
	var unread []string
	for _, in := range h.ticket(ticketID).Inputs {
		if in.ReadAt == nil && in.DeliveredTo == assignmentID {
			unread = append(unread, in.ID)
		}
	}
	h.must(h.eng.InputsRead(ticketID, unread))
}

// pairedTicket submits a task paired with review on runtime and starts its implementer on implRuntime.
func (h *harness) pairedTicket(runtime, implRuntime string) (ticketID, implementer string) {
	h.t.Helper()
	tk, err := h.eng.SubmitOwnerTask("Fix the login page", TaskOptions{Dir: "/work/x", Runtime: runtime, Pairing: PairingReview})
	h.must(err)
	return tk.ID, h.launch(tk.ID, implRuntime)
}

func (h *harness) ready(ticketID, assignmentID, summary string) {
	h.t.Helper()
	h.must(h.eng.FileReport(ticketID, ReportInput{AssignmentID: assignmentID, State: ReportReady, Summary: summary}))
}

// reportRelays are the keys of the worker-report relays queued for the main Session.
func reportRelays(h *harness) []string {
	var out []string
	for _, k := range effectKeys(h.effectsOf(EffectRelay)) {
		if strings.HasPrefix(k, "report:") {
			out = append(out, k)
		}
	}
	return out
}

func countType(types []string, typ string) int {
	n := 0
	for _, t := range types {
		if t == typ {
			n++
		}
	}
	return n
}

func TestReadyStartsReviewWhenPaired(t *testing.T) {
	h := newPairedRouter(t)
	id := h.acceptRequest("c:1", issue)
	impl := h.ticket(id).LatestAssignment().ID
	eq(t, "a router ticket takes the pairing default", h.ticket(id).Pairing, PairingReview)
	h.tick(1)
	h.ready(id, impl, "login fixed")
	tk := h.ticket(id)
	if tk.Ended != nil {
		t.Fatalf("a paired ready ended the ticket: %+v", tk.Ended)
	}
	ia := findAssignment(tk, impl)
	eq(t, "implementer ended", ia.State, AssignmentEnded)
	eq(t, "ended for review", ia.EndedReason, ReasonReview)
	if ia.Report == nil || ia.Report.State != ReportReady || ia.Report.Summary != "login fixed" {
		t.Fatalf("report not recorded: %+v", ia.Report)
	}
	if tk.Review == nil || tk.Review.Round != 1 || tk.Review.Implementer != impl || tk.Review.Held.Summary != "login fixed" ||
		tk.Review.Held.State != ReportReady {
		t.Fatalf("review state = %+v", tk.Review)
	}
	rv := tk.LatestAssignment()
	eq(t, "review role", rv.Role, RoleReview)
	eq(t, "review pending", rv.State, AssignmentPending)
	eq(t, "review unpinned", rv.Runtime, "")
	eq(t, "stage", TicketStage(tk).Stage, "working")
	eq(t, "executor", h.exec.methods()[len(h.exec.methods())-2:], []string{"stop:" + impl + ":" + ReasonReview, "start:" + rv.ID})
	// Nothing speaks yet: no report event, no relay to the router, no reply.
	eq(t, "no report event", countType(h.events.types(), EventReportFiled), 0)
	eq(t, "no report relay", reportRelays(h.harness), []string(nil))
}

func TestReadyPassesThroughWithoutPairing(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.ready(id, h.ticket(id).LatestAssignment().ID, "done")
	if tk := h.ticket(id); tk.Ended == nil || tk.Ended.Outcome != OutcomeCompleted || tk.Review != nil {
		t.Fatalf("unpaired ready: ended %+v, review %+v", tk.Ended, tk.Review)
	}

	// "off" turns a paired default off for one task; "review" pairs one task without a default; others are refused.
	p := newHarnessWith(t, pairedOptions(2))
	tk, err := p.eng.SubmitOwnerTask("Task", TaskOptions{Dir: "/w", Pairing: PairingOff})
	p.must(err)
	eq(t, "off", tk.Pairing, "")
	tk, err = p.eng.SubmitOwnerTask("Task", TaskOptions{Dir: "/w"})
	p.must(err)
	eq(t, "default", tk.Pairing, PairingReview)
	tk, err = h.eng.SubmitOwnerTask("Task", TaskOptions{Dir: "/w", Pairing: PairingReview})
	h.must(err)
	eq(t, "on", tk.Pairing, PairingReview)
	var ue *UserError
	if _, err := h.eng.SubmitOwnerTask("Task", TaskOptions{Dir: "/w", Pairing: "split"}); !errors.As(err, &ue) {
		t.Fatalf("unknown pairing: %v", err)
	}

	// A review's own report is never a ready that starts a review.
	id2, impl := p.pairedTicket(RuntimeClaude, RuntimeClaude)
	p.ready(id2, impl, "done")
	rv := p.launch(id2, RuntimeCodex)
	if err := p.eng.FileReport(id2, ReportInput{AssignmentID: rv, State: ReportReady, Summary: "x"}); !errors.As(err, &ue) {
		t.Fatalf("a review's ready report: %v", err)
	}
}

func TestApprovedAppliesHeldReadyWithoutPR(t *testing.T) {
	h := newPairedRouter(t)
	id := h.acceptRequest("c:1", issue)
	impl := h.ticket(id).LatestAssignment().ID // its request reached it, unread when it reports: read by then
	h.ready(id, impl, "login fixed")
	rv := h.launch(id, RuntimeCodex)
	h.tick(5)
	h.must(h.eng.FileReview(id, rv, ReviewApproved, "- [low] login.go:3 — a comment is stale"))
	tk := h.ticket(id)
	if tk.Ended == nil || tk.Ended.Outcome != OutcomeCompleted {
		t.Fatalf("approved without a PR: ended %+v", tk.Ended)
	}
	ra := findAssignment(tk, rv)
	eq(t, "review ended", ra.EndedReason, ReasonReviewed)
	if ra.ReviewOutcome == nil || *ra.ReviewOutcome != (ReviewOutcome{Verdict: ReviewApproved, First: "- [low] login.go:3 — a comment is stale"}) {
		t.Fatalf("outcome = %+v", ra.ReviewOutcome)
	}
	eq(t, "rounds", tk.Stats.ReviewRounds, 1)
	eq(t, "review cleared", tk.Review == nil, true)
	last := tk.LatestAssignment()
	if last.ID == rv || last.Role != "" || last.Report == nil || last.Report.Summary != "login fixed" || last.Report.At != h.now() {
		t.Fatalf("held report not applied as filed now: %+v", last)
	}
	// The result reply goes out once, through the router's relay of the applied report.
	want := "report:" + id + ":" + last.ID + ":" + stamp(h.now())
	eq(t, "one report relay", reportRelays(h.harness), []string{want})
	eq(t, "one report event", countType(h.events.types(), EventReportFiled), 1)
}

func TestApprovedWithPRParksImplementer(t *testing.T) {
	h := newIntake(t)
	id, impl := h.pairedTicket(RuntimeAuto, RuntimeClaude)
	h.must(h.eng.BindThread(id, pull1, true))
	h.ready(id, impl, "PR opened")
	if h.ticket(id).HandedOff() {
		t.Fatal("handed off before the review")
	}
	rv := h.launch(id, RuntimeCodex)
	starts := h.exec.count("start")
	h.must(h.eng.FileReview(id, rv, ReviewApproved, "- none"))
	tk := h.ticket(id)
	if tk.Ended != nil || !tk.HandedOff() {
		t.Fatalf("approved with a PR: ended %+v, handed off %v", tk.Ended, tk.HandedOff())
	}
	a := tk.LatestAssignment()
	ia := findAssignment(tk, impl)
	if a.State != AssignmentRunning || a.Role != "" || a.SessionID != ia.SessionID || a.Runtime != RuntimeClaude ||
		a.IdleAt == nil || a.DeliveredAt != nil || a.StartedAt == nil || a.Report == nil || a.Report.State != ReportReady {
		t.Fatalf("parked implementer = %+v", a)
	}
	eq(t, "stage", TicketStage(tk), Stage{"person", "review"})
	eq(t, "no launch: the session resumes for the next input", h.exec.count("start"), starts)

	// Its pull request merging ends it as merged.
	h.closePull(pull1, OutcomeMerged)
	eq(t, "merged", h.ticket(id).Ended.Outcome, OutcomeMerged)
}

func TestChangesResumesImplementerWithFindings(t *testing.T) {
	h := newHarness(t)
	id, impl := h.pairedTicket(RuntimeClaude, RuntimeClaude)
	h.ready(id, impl, "done")
	rv := h.launch(id, RuntimeCodex)
	findings := "- [high] login.go:12 — the password is logged\n- [low] login.go:40 — typo"
	h.must(h.eng.FileReview(id, rv, ReviewChanges, findings))
	tk := h.ticket(id)
	if tk.Ended != nil || tk.Review != nil {
		t.Fatalf("changes: ended %+v, review %+v", tk.Ended, tk.Review)
	}
	next := tk.LatestAssignment()
	if next.Role != "" || next.State != AssignmentPending || next.Runtime != RuntimeClaude {
		t.Fatalf("next implementer = %+v, want pending on the implementer's runtime", next)
	}
	eq(t, "started", h.exec.last("start").AssignmentID, next.ID)
	in := tk.Inputs[len(tk.Inputs)-1]
	eq(t, "kind", in.Kind, InputReview)
	eq(t, "text", in.Text, "Review (round 1, codex) requested changes:\n"+findings)
	eq(t, "waiting for it", tk.Undelivered[len(tk.Undelivered)-1], in.ID)
	eq(t, "outcome", *findAssignment(tk, rv).ReviewOutcome, ReviewOutcome{Verdict: ReviewChanges, First: "- [high] login.go:12 — the password is logged"})
	eq(t, "stage", TicketStage(tk).Stage, "working")

	// The implementer's next ready starts round 2.
	impl2 := h.launch(id, RuntimeClaude)
	h.ready(id, impl2, "fixed")
	tk = h.ticket(id)
	if tk.Review == nil || tk.Review.Round != 2 || tk.LatestAssignment().Role != RoleReview {
		t.Fatalf("round 2 not started: %+v", tk.Review)
	}

	// An auto Ticket's changes round is left to the chooser, whose warm rule keeps the implementer's runtime (F-26).
	id, impl = h.pairedTicket(RuntimeAuto, RuntimeCodex)
	h.ready(id, impl, "done")
	rv = h.launch(id, RuntimeClaude)
	h.must(h.eng.FileReview(id, rv, ReviewChanges, "- [medium] a.go:1 — racy"))
	eq(t, "auto unpinned", h.ticket(id).LatestAssignment().Runtime, "")
}

func TestRoundsCapAppliesHeldReady(t *testing.T) {
	h := newHarnessWith(t, pairedOptions(2))
	tk, err := h.eng.SubmitOwnerTask("Task", TaskOptions{Dir: "/w"})
	h.must(err)
	id := tk.ID
	impl := h.launch(id, RuntimeClaude)
	for round := 1; round <= 2; round++ {
		h.ready(id, impl, "attempt")
		rv := h.launch(id, RuntimeCodex)
		h.must(h.eng.FileReview(id, rv, ReviewChanges, "- [high] a.go:1 — still wrong"))
		if round == 1 {
			impl = h.launch(id, RuntimeClaude)
		}
	}
	tk = *h.ticket(id)
	if tk.Ended == nil || tk.Ended.Outcome != OutcomeCompleted {
		t.Fatalf("after the last round the held ready applies: ended %+v", tk.Ended)
	}
	eq(t, "rounds", tk.Stats.ReviewRounds, 2)
	eq(t, "applied", tk.LatestAssignment().Report.Summary, "attempt")
}

func TestReviewFailureNeverPersonStage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state ReportState
		start bool
	}{
		{"crash past its restarts", ReportFailed, true},
		{"two turns without a verdict", ReportBlocked, true},
		{"a launch that failed", ReportFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			id, impl := h.pairedTicket(RuntimeClaude, RuntimeClaude)
			h.ready(id, impl, "done")
			rv := h.ticket(id).LatestAssignment().ID
			if tc.start {
				h.launch(id, RuntimeCodex)
			}
			h.must(h.eng.FileReport(id, ReportInput{AssignmentID: rv, State: tc.state, Summary: "session crashed"}))
			tk := h.ticket(id)
			if tk.Ended == nil || tk.Ended.Outcome != OutcomeCompleted {
				t.Fatalf("a failed review applies the held ready: ended %+v, stage %+v", tk.Ended, TicketStage(tk))
			}
			ra := findAssignment(tk, rv)
			eq(t, "outcome", *ra.ReviewOutcome, ReviewOutcome{Verdict: ReviewFailed, First: ReviewFailed})
			eq(t, "no report on the review", ra.Report == nil, true)
			eq(t, "rounds", tk.Stats.ReviewRounds, 1)
		})
	}

	// A review that cannot apply its verdict says so, and a report that is no failure is refused.
	h := newHarness(t)
	id, impl := h.pairedTicket(RuntimeClaude, RuntimeClaude)
	h.ready(id, impl, "done")
	rv := h.launch(id, RuntimeCodex)
	var ue *UserError
	for _, s := range []ReportState{ReportProgress, ReportWaiting, ReportDropped} {
		if err := h.eng.FileReport(id, ReportInput{AssignmentID: rv, State: s, Summary: "x"}); !errors.As(err, &ue) {
			t.Fatalf("a review's %s report: %v", s, err)
		}
	}
	if st := TicketStage(h.ticket(id)); st.Stage == "person" {
		t.Fatalf("stage %+v", st)
	}
}

func TestReviewLimitFailsOverUnpinned(t *testing.T) {
	h := newHarness(t)
	id, impl := h.pairedTicket(RuntimeClaude, RuntimeClaude)
	h.ready(id, impl, "done")
	rv := h.launch(id, RuntimeCodex)
	h.must(h.eng.UsageLimited(id, rv, UsageLimit{Runtime: RuntimeCodex}))
	tk := h.ticket(id)
	next := tk.LatestAssignment()
	if next.Role != RoleReview || next.Runtime != "" || tk.Review == nil {
		t.Fatalf("after a review's limit: %+v, review %+v", next, tk.Review)
	}
}

func TestInconsistentVerdictRejected(t *testing.T) {
	for _, tc := range []struct {
		verdict, findings string
		ok                bool
	}{
		{ReviewApproved, "- none", true},
		{ReviewApproved, "- [low] a.go:3 — a nit", true},
		{ReviewChanges, "- [low] a.go:3 — a nit", true},
		{ReviewChanges, "- [high] a.go:3 — broken\n- [medium] b/c.go:10 — racy", true},
		{ReviewChanges, "  - [high] a.go:3 — broken  \n", true},
		{ReviewApproved, "- [high] a.go:3 — broken", false},
		{ReviewApproved, "- [low] a.go:3 — fine\n- [medium] a.go:4 — racy", false},
		{ReviewChanges, "- none", false},
		{ReviewApproved, "- none\n- [low] a.go:3 — x", false},
		{ReviewApproved, "", false},
		{ReviewApproved, "looks good", false},
		{ReviewChanges, "- [critical] a.go:3 — x", false},
		{ReviewChanges, "- [high] a.go — no line", false},
		{ReviewChanges, "- [high] a.go:3 - plain dash", false},
		{ReviewChanges, "- [high] a.go:3 — x\n\n- [low] b.go:1 — y", false},
		{"maybe", "- none", false},
		{ReviewFailed, "- none", false},
	} {
		err := CheckReview(tc.verdict, tc.findings)
		var ue *UserError
		if tc.ok != (err == nil) || (err != nil && !errors.As(err, &ue)) {
			t.Errorf("CheckReview(%q, %q) = %v", tc.verdict, tc.findings, err)
		}
	}

	h := newHarness(t)
	id, impl := h.pairedTicket(RuntimeClaude, RuntimeClaude)
	h.ready(id, impl, "done")
	rv := h.launch(id, RuntimeCodex)
	err := h.eng.FileReview(id, rv, ReviewApproved, "- [high] a.go:3 — broken")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "changes") {
		t.Fatalf("approved with a high finding: %v", err)
	}
	tk := h.ticket(id)
	if a := findAssignment(tk, rv); a.State != AssignmentRunning || a.ReviewOutcome != nil || tk.Review == nil {
		t.Fatalf("a refused verdict changed the review: %+v", a)
	}
	// Only the running review gives a verdict.
	if err := h.eng.FileReview(id, impl, ReviewApproved, "- none"); !errors.As(err, &ue) {
		t.Fatalf("a verdict from the implementer: %v", err)
	}
	if err := h.eng.FileReview("T-9999", rv, ReviewApproved, "- none"); !errors.As(err, &ue) {
		t.Fatalf("a verdict on no ticket: %v", err)
	}
}

// A person's message that reaches the Ticket during its review is not ended over: the held report is dropped and the
// implementer resumes, on its own runtime, with the message; its next ready report is reviewed again while rounds
// remain. The same holds when the review fails.
func TestInputDuringReviewResumesImplementer(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := newHarness(t)
		id, impl := h.pairedTicket(RuntimeAuto, RuntimeClaude)
		h.ready(id, impl, "done")
		rv := h.launch(id, RuntimeCodex)
		h.must(h.eng.AddPersonInput(id, "please also fix the footer"))
		events := countType(h.events.types(), EventReportFiled)
		if fail {
			h.must(h.eng.FileReport(id, ReportInput{AssignmentID: rv, State: ReportFailed, Summary: "crashed"}))
		} else {
			h.must(h.eng.FileReview(id, rv, ReviewApproved, "- none"))
		}
		tk := h.ticket(id)
		if tk.Ended != nil {
			t.Fatalf("fail=%v: the ticket ended over a person's message: %+v", fail, tk.Ended)
		}
		next := tk.LatestAssignment()
		if next.Role != "" || next.State != AssignmentPending || next.Runtime != "" || next.Report != nil {
			t.Fatalf("fail=%v: next = %+v, want a pending implementer left to the chooser on an auto ticket", fail, next)
		}
		eq(t, "started", h.exec.last("start").AssignmentID, next.ID)
		msg := tk.Inputs[len(tk.Inputs)-1]
		if msg.Text != "please also fix the footer" || !slices.Contains(tk.Undelivered, msg.ID) {
			t.Fatalf("fail=%v: the message does not wait for the implementer: %+v %v", fail, msg, tk.Undelivered)
		}
		eq(t, "the held report is dropped: no report event", countType(h.events.types(), EventReportFiled), events)
		eq(t, "stage", TicketStage(tk).Stage, "working")
		impl2 := h.launch(id, RuntimeClaude)
		h.ready(id, impl2, "footer fixed too")
		if tk := h.ticket(id); tk.Review == nil || tk.Review.Round != 2 {
			t.Fatalf("fail=%v: the next ready is not reviewed again: %+v", fail, tk.Review)
		}
	}
}

// The resumed implementer follows the changes round's rule: a Ticket that names a runtime pins it to the implementer's,
// an auto one leaves it to the chooser, whose warm rule keeps it there (F-26).
func TestInputDuringReviewPinsLikeChanges(t *testing.T) {
	for _, tc := range []struct{ runtime, want string }{{RuntimeCodex, RuntimeCodex}, {RuntimeAuto, ""}} {
		h := newHarness(t)
		id, impl := h.pairedTicket(tc.runtime, RuntimeCodex)
		h.ready(id, impl, "done")
		rv := h.launch(id, RuntimeClaude)
		h.must(h.eng.AddPersonInput(id, "one more thing"))
		h.must(h.eng.FileReview(id, rv, ReviewApproved, "- none"))
		eq(t, tc.runtime+" ticket: resumed implementer's runtime", h.ticket(id).LatestAssignment().Runtime, tc.want)
	}
}

// On a Ticket with a pull request, a message that arrived during the review is sent: the parked implementer is
// launched (it resumes its paused session once the review's session is gone), not merely delivered to.
func TestInputDuringReviewWithPRLaunchesImplementer(t *testing.T) {
	h := newIntake(t)
	id, impl := h.pairedTicket(RuntimeAuto, RuntimeClaude)
	h.must(h.eng.BindThread(id, pull1, true))
	h.ready(id, impl, "PR opened")
	rv := h.launch(id, RuntimeCodex)
	h.must(h.eng.AddPersonInput(id, "please also fix the footer"))
	h.must(h.eng.FileReview(id, rv, ReviewApproved, "- none"))
	tk := h.ticket(id)
	a := tk.LatestAssignment()
	if !tk.HandedOff() || a.State != AssignmentRunning {
		t.Fatalf("not handed off: %+v", a)
	}
	eq(t, "the parked implementer is launched", h.exec.last("start").AssignmentID, a.ID)
	if len(tk.Undelivered) == 0 {
		t.Fatal("nothing waits for it")
	}
}

// A Ticket that ends during its review keeps no review in progress.
func TestEndDuringReviewClearsIt(t *testing.T) {
	h := newHarness(t)
	id, impl := h.pairedTicket(RuntimeClaude, RuntimeClaude)
	h.ready(id, impl, "done")
	h.must(h.eng.StopTicket(id, "enough"))
	if tk := h.ticket(id); tk.Review != nil {
		t.Fatalf("review = %+v", tk.Review)
	}
}
