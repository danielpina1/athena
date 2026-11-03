package core

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func eq[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got  %#v\n want %#v", what, got, want)
	}
}

func isUserError(err error) bool {
	var ue *UserError
	return errors.As(err, &ue)
}

func wantUserError(t *testing.T, what string, err error) {
	t.Helper()
	if !isUserError(err) {
		t.Errorf("%s: got %v, want a *UserError", what, err)
	}
}

func stageOf(t *Ticket) Stage { return TicketStage(t) }

func inputKinds(t *Ticket) []InputKind {
	var out []InputKind
	for _, in := range t.Inputs {
		out = append(out, in.Kind)
	}
	return out
}

func assignmentStates(t *Ticket) []AssignmentState {
	var out []AssignmentState
	for _, a := range t.Assignments {
		out = append(out, a.State)
	}
	return out
}

// lastInput returns the ID of the Ticket's latest Input.
func lastInput(t *Ticket) string { return t.Inputs[len(t.Inputs)-1].ID }

// --- Owner tasks ---

func TestSubmitOwnerTaskStartsAssignment(t *testing.T) {
	h := newHarness(t)
	got, err := h.eng.SubmitOwnerTask("Fix the login bug\nIt 500s on submit.", TaskOptions{Dir: "/work/login", Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	stored := h.ticket(got.ID)
	eq(t, "returned ticket equals stored", got, *stored)
	eq(t, "id", got.ID, "T-0001")
	eq(t, "title", got.Title, "Fix the login bug")
	eq(t, "mandate", got.Mandate, Mandate{Strength: "explicit", PolicyVersion: OwnerPolicyVersion, Actor: OwnerActor, Scope: "owner"})
	eq(t, "execution", got.Execution, &Execution{Directory: "/work/login", Runtime: "claude", Model: "opus"})
	eq(t, "created at", got.CreatedAt, base)
	eq(t, "assignments", assignmentStates(stored), []AssignmentState{AssignmentPending})

	// The Trigger and the accept Decision are recorded with it.
	trig := h.repos.trigger(got.Origin.TriggerID)
	if trig == nil {
		t.Fatal("origin trigger not stored")
	}
	eq(t, "trigger", *trig, Trigger{ID: trig.ID, Source: "owner", Actor: OwnerActor, Human: true, At: base, Text: "Fix the login bug\nIt 500s on submit."})
	eq(t, "origin", got.Origin, Origin{TriggerID: trig.ID, TriggerIDs: []string{trig.ID}})
	decisions := h.repos.decisionList()
	if len(decisions) != 1 {
		t.Fatalf("decisions: %d, want 1", len(decisions))
	}
	d := decisions[0]
	eq(t, "decision", d, Decision{ID: got.DecisionID, Kind: "accept", TicketID: got.ID, TriggerIDs: []string{trig.ID}, At: base, Mandate: &got.Mandate})

	// The text is the first Input, a person's trigger, waiting for delivery.
	eq(t, "inputs", got.Inputs, []Input{{ID: got.Inputs[0].ID, Kind: InputTrigger, At: base, TriggerID: trig.ID, Text: trig.Text, Human: true}})
	eq(t, "undelivered", got.Undelivered, []string{got.Inputs[0].ID})
	eq(t, "stage", stageOf(stored), Stage{"working", "queued"})

	// exec.Start after commit, with the committed Ticket; ticket.created emitted.
	eq(t, "executor calls", h.exec.methods(), []string{"start:" + got.Assignments[0].ID})
	start := h.exec.last("start")
	eq(t, "start ticket", start.Ticket, *stored)
	eq(t, "start assignment", start.Assignment, stored.Assignments[0])
	eq(t, "events", h.events.types(), []string{EventTicketCreated})
	eq(t, "event ticket", h.events.events[0].TicketID, got.ID)

	// Ticket ids count up.
	eq(t, "second id", h.submit("again").ID, "T-0002")
}

func TestSubmitOwnerTaskTitle(t *testing.T) {
	h := newHarness(t)
	long := strings.Repeat("é", 100) + "\nsecond line"
	eq(t, "truncated to 80 runes", h.submit(long).Title, strings.Repeat("é", 80))
	eq(t, "first line trimmed", h.submit("  \t hello world \r\nmore").Title, "hello world")
	_, err := h.eng.SubmitOwnerTask(" \n ", TaskOptions{})
	wantUserError(t, "empty task", err)
	tickets, _, _ := h.repos.counts()
	eq(t, "tickets stored", tickets, 2)
}

// --- Executor callbacks ---

func TestAssignmentStartedDeliversWaitingInputs(t *testing.T) {
	h := newHarness(t)
	tk := h.submit("Fix it")
	a := tk.Assignments[0].ID
	h.tick(1)
	h.must(h.eng.AssignmentStarted(tk.ID, a, "sess-1"))
	stored := h.ticket(tk.ID)
	started := base.Add(1e9)
	eq(t, "assignment", stored.Assignments[0], Assignment{ID: a, State: AssignmentRunning, CreatedAt: base, StartedAt: &started, SessionID: "sess-1"})
	eq(t, "executor calls", h.exec.methods(), []string{"start:" + a, "deliver:" + a})
	deliver := h.exec.last("deliver")
	eq(t, "deliver ticket", deliver.Ticket, *stored)
	eq(t, "deliver assignment state", deliver.Assignment.State, AssignmentRunning)
	eq(t, "events", h.events.types(), []string{EventTicketCreated, EventAssignmentStarted})
	eq(t, "event assignment", h.events.events[1].AssignmentID, a)
	eq(t, "stage", stageOf(stored), Stage{Stage: "working"})

	// Delivered: the Input is no longer waiting.
	h.must(h.eng.InputsDelivered(tk.ID, a, []string{tk.Inputs[0].ID}))
	stored = h.ticket(tk.ID)
	eq(t, "undelivered", len(stored.Undelivered), 0)
	eq(t, "delivered to", stored.Inputs[0].DeliveredTo, a)

	// Unknown ticket is a UserError.
	wantUserError(t, "unknown ticket", h.eng.AssignmentStarted("T-9999", a, ""))
}

func TestReportQuestionSetsAttention(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportProgress, Summary: "need a call", Question: "Which color?"}))
	stored := h.ticket(id)
	if stored.Attention == nil || stored.Attention.Question != "Which color?" || stored.Attention.ID == "" {
		t.Fatalf("attention: %#v", stored.Attention)
	}
	eq(t, "report", stored.Assignments[0].Report, &Report{State: ReportProgress, Summary: "need a call", At: base})
	eq(t, "stage", stageOf(stored), Stage{"person", "answer"})
	types := h.events.types()
	eq(t, "last event", types[len(types)-1], EventReportFiled)
}

func TestPersonInputClearsAttentionAndDelivers(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportProgress, Summary: "asking", Question: "Which color?"}))
	delivers := h.exec.count("deliver")
	h.tick(1)
	h.must(h.eng.AddPersonInput(id, "Blue"))
	stored := h.ticket(id)
	if stored.Attention != nil {
		t.Errorf("attention not cleared: %#v", stored.Attention)
	}
	in := stored.Inputs[len(stored.Inputs)-1]
	trig := h.repos.trigger(in.TriggerID)
	if trig == nil {
		t.Fatal("person trigger not stored")
	}
	eq(t, "trigger", *trig, Trigger{ID: in.TriggerID, Source: "owner", Actor: OwnerActor, Human: true, At: base.Add(1e9), Text: "Blue"})
	eq(t, "input", in, Input{ID: in.ID, Kind: InputTrigger, At: base.Add(1e9), TriggerID: trig.ID, Text: "Blue", Human: true})
	eq(t, "undelivered", stored.Undelivered, []string{in.ID})
	eq(t, "deliver called", h.exec.count("deliver"), delivers+1)
	eq(t, "deliver to running", h.exec.last("deliver").AssignmentID, stored.Assignments[0].ID)
	types := h.events.types()
	eq(t, "last event", types[len(types)-1], EventInputAdded)
	eq(t, "stage", stageOf(stored), Stage{Stage: "working"})
	h.settle(true)
	eq(t, "delivered", h.ticket(id).Undelivered, []string(nil))
}

func TestReportReadyEndsCompleted(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.ticket(id).Assignments[0].ID
	h.must(h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportProgress, Summary: "asking", Question: "Merge it?"}))
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportReady, Summary: "done"}))
	stored := h.ticket(id)
	at := base.Add(1e9)
	eq(t, "ended", stored.Ended, &Ended{At: at, By: EndByExecutor, Outcome: OutcomeCompleted})
	if stored.Attention != nil {
		t.Errorf("attention kept: %#v", stored.Attention)
	}
	eq(t, "assignment state", stored.Assignments[0].State, AssignmentEnded)
	eq(t, "assignment ended at", stored.Assignments[0].EndedAt, &at)
	eq(t, "report", stored.Assignments[0].Report, &Report{State: ReportReady, Summary: "done", At: at})
	eq(t, "stop", h.exec.methods()[len(h.exec.methods())-1], "stop:"+a+":completed")
	types := h.events.types()
	eq(t, "events", types[len(types)-2:], []string{EventReportFiled, EventTicketEnded})
	eq(t, "stage", stageOf(stored), Stage{Stage: "ended"})
}

func TestReportDroppedEnds(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.ticket(id).Assignments[0].ID
	h.must(h.eng.FileReport(id, ReportInput{State: ReportDropped, Summary: "nothing to do"}))
	stored := h.ticket(id)
	eq(t, "ended", stored.Ended, &Ended{At: base, By: EndByExecutor, Outcome: OutcomeDropped})
	eq(t, "stop", h.exec.methods()[len(h.exec.methods())-1], "stop:"+a+":dropped")
	ended := h.events.events[len(h.events.events)-1]
	eq(t, "ended event", ended.Type, EventTicketEnded)
	eq(t, "ended event data", ended.Data, map[string]any{"by": EndByExecutor, "outcome": OutcomeDropped})
}

func TestBlockedThenHumanInputReleases(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportBlocked, Summary: "needs a decision"}))
	eq(t, "blocked stage", stageOf(h.ticket(id)), Stage{"person", "blocked"})
	h.tick(1)
	h.must(h.eng.AddPersonInput(id, "Use plan B"))
	eq(t, "released stage", stageOf(h.ticket(id)), Stage{Stage: "working"})
	eq(t, "waits for person", h.ticket(id).WaitsForPerson(), false)
}

func TestRetryOnlyWhenAllowed(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	err := h.eng.RetryTicket(id)
	wantUserError(t, "retry while working", err)
	wantUserError(t, "retry unknown", h.eng.RetryTicket("T-9999"))
	first := h.ticket(id).Assignments[0].ID
	h.must(h.eng.FileReport(id, ReportInput{State: ReportFailed, Summary: "broke"}))
	h.tick(1)
	h.must(h.eng.RetryTicket(id))
	stored := h.ticket(id)
	eq(t, "assignments", assignmentStates(stored), []AssignmentState{AssignmentEnded, AssignmentPending})
	eq(t, "ended reason", stored.Assignments[0].EndedReason, "retried")
	second := stored.Assignments[1].ID
	m := h.exec.methods()
	eq(t, "executor calls", m[len(m)-2:], []string{"stop:" + first + ":retried", "start:" + second})
	types := h.events.types()
	eq(t, "last event", types[len(types)-1], EventAssignmentEnded)
	eq(t, "stage", stageOf(stored), Stage{"working", "queued"})
	// Not retryable again until it stalls again.
	wantUserError(t, "retry twice", h.eng.RetryTicket(id))
}

func TestStopEndsAndStopsExecutor(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.ticket(id).Assignments[0].ID
	h.tick(2)
	h.must(h.eng.StopTicket(id, "Human asked to stop"))
	stored := h.ticket(id)
	at := base.Add(2e9)
	eq(t, "ended", stored.Ended, &Ended{At: at, By: EndByHuman, Outcome: OutcomeStopped, Reason: "Human asked to stop"})
	eq(t, "assignment", stored.Assignments[0].State, AssignmentEnded)
	eq(t, "undelivered", stored.Undelivered, []string(nil))
	eq(t, "stop", h.exec.methods()[len(h.exec.methods())-1], "stop:"+a+":Human asked to stop")
	ev := h.events.events[len(h.events.events)-1]
	eq(t, "event", ev.Type, EventTicketEnded)
	eq(t, "event data", ev.Data, map[string]any{"by": EndByHuman, "outcome": OutcomeStopped, "reason": "Human asked to stop"})
	// Stopping it again changes nothing.
	calls := len(h.exec.methods())
	h.must(h.eng.StopTicket(id, "again"))
	eq(t, "no more calls", len(h.exec.methods()), calls)
	eq(t, "unchanged", h.ticket(id).Ended.Reason, "Human asked to stop")
	wantUserError(t, "stop unknown", h.eng.StopTicket("T-9999", ""))
}

func TestExecutorNotCalledOnRollback(t *testing.T) {
	h := newHarness(t)
	h.repos.failWith = errors.New("disk full")
	_, err := h.eng.SubmitOwnerTask("Fix it", TaskOptions{})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want disk full", err)
	}
	tickets, triggers, decisions := h.repos.counts()
	eq(t, "nothing stored", [3]int{tickets, triggers, decisions}, [3]int{0, 0, 0})
	eq(t, "no executor calls", h.exec.methods(), []string(nil))
	eq(t, "no events", h.events.types(), []string(nil))

	// A command on an existing ticket that fails to commit changes nothing and calls nothing.
	h.repos.failWith = nil
	id := h.accepted()
	before := h.ticket(id)
	calls, events := len(h.exec.methods()), len(h.events.types())
	h.repos.failWith = errors.New("disk full")
	for name, err := range map[string]error{
		"input":  h.eng.AddPersonInput(id, "more"),
		"stop":   h.eng.StopTicket(id, ""),
		"report": h.eng.FileReport(id, ReportInput{State: ReportReady, Summary: "done"}),
	} {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	eq(t, "ticket unchanged", h.ticket(id), before)
	eq(t, "no executor calls after rollback", len(h.exec.methods()), calls)
	eq(t, "no events after rollback", len(h.events.types()), events)
	// The ticket counter did not move either.
	h.repos.failWith = nil
	eq(t, "next id", h.submit("next").ID, "T-0002")
}

func TestInputsDeliveredInOrderExactlyOnce(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.ticket(id).Assignments[0].ID
	delivers := h.exec.count("deliver")
	var ids []string
	for _, text := range []string{"one", "two", "three"} {
		h.tick(1)
		h.must(h.eng.AddPersonInput(id, text))
		ids = append(ids, lastInput(h.ticket(id)))
	}
	eq(t, "undelivered in order", h.ticket(id).Undelivered, ids)
	eq(t, "deliver per input", h.exec.count("deliver"), delivers+3)
	eq(t, "last deliver sees all, in order", h.exec.last("deliver").Ticket.Undelivered, ids)

	// A partial delivery drains only what it names, and keeps the order of the rest.
	h.tick(1)
	h.must(h.eng.InputsDelivered(id, a, ids[:1]))
	eq(t, "after first", h.ticket(id).Undelivered, ids[1:])
	h.must(h.eng.InputsDelivered(id, a, ids[1:]))
	after := h.ticket(id)
	eq(t, "drained", after.Undelivered, []string(nil))
	for _, in := range after.Inputs[1:] {
		eq(t, "delivered to "+in.Text, in.DeliveredTo, a)
	}

	// Delivering the same IDs again is a no-op.
	h.tick(1)
	h.must(h.eng.InputsDelivered(id, a, ids))
	eq(t, "redelivery is a no-op", h.ticket(id), after)
}

func TestTurnEndedAndInputsRead(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.ticket(id).Assignments[0].ID
	h.tick(1)
	h.read(id)
	at := base.Add(1e9)
	eq(t, "read at", h.ticket(id).Inputs[0].ReadAt, &at)
	h.tick(1)
	h.must(h.eng.InputsRead(id, []string{h.ticket(id).Inputs[0].ID}))
	eq(t, "read at kept", h.ticket(id).Inputs[0].ReadAt, &at)
	h.must(h.eng.TurnEnded(id, a))
	idle := base.Add(2e9)
	eq(t, "idle at", h.ticket(id).Assignments[0].IdleAt, &idle)
	// A turn end of another Assignment is dropped.
	h.tick(1)
	h.must(h.eng.TurnEnded(id, "another"))
	eq(t, "idle at unchanged", h.ticket(id).Assignments[0].IdleAt, &idle)
	wantUserError(t, "read unknown", h.eng.InputsRead("T-9999", nil))
}

func TestQueries(t *testing.T) {
	h := newHarness(t)
	open := h.accepted()
	h.tick(1)
	done := h.submit("Second").ID
	h.must(h.eng.StopTicket(done, ""))
	tk, st, err := h.eng.Ticket(open)
	h.must(err)
	eq(t, "ticket", tk, *h.ticket(open))
	eq(t, "stage", st, Stage{Stage: "working"})
	_, _, err = h.eng.Ticket("T-9999")
	wantUserError(t, "unknown ticket", err)
	list, err := h.eng.Tickets(false)
	h.must(err)
	eq(t, "open tickets", len(list), 1)
	eq(t, "open ticket id", list[0].ID, open)
	list, err = h.eng.Tickets(true)
	h.must(err)
	eq(t, "all tickets", len(list), 2)
}

// TestReportReadyRefusedWhileApprovalPending: the real CLI runs a Write and the report call in the same step, so a
// ready report can arrive while the Write still waits for the owner. It must not end the Ticket completed.
func TestReportReadyRefusedWhileApprovalPending(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.ticket(id).Assignments[0].ID
	ap := h.request(id)
	wantUserError(t, "ready", h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportReady, Summary: "wrote it"}))
	wantUserError(t, "dropped", h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportDropped, Summary: "nothing"}))
	if tk := h.ticket(id); tk.Ended != nil || tk.Assignments[0].Report != nil {
		t.Fatalf("a refused report changed the ticket: ended %v report %v", tk.Ended, tk.Assignments[0].Report)
	}
	// Reports that claim no result are fine while the approval waits.
	h.must(h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportProgress, Summary: "waiting for approval"}))
	h.must(h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportBlocked, Summary: "needs approval", Question: "Approve?"}))
	// Once the owner answers, a ready report is accepted.
	h.must(h.eng.AnswerApproval(ap.ID, false, "no"))
	h.must(h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportReady, Summary: "done without it"}))
	eq(t, "outcome", h.ticket(id).Ended.Outcome, OutcomeCompleted)
}

func TestReportValidation(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	first := h.ticket(id).Assignments[0].ID
	wantUserError(t, "unknown ticket", h.eng.FileReport("T-9999", ReportInput{State: ReportProgress}))
	wantUserError(t, "unknown assignment", h.eng.FileReport(id, ReportInput{AssignmentID: "nope", State: ReportProgress}))
	h.must(h.eng.FileReport(id, ReportInput{State: ReportFailed, Summary: "broke"}))
	h.tick(1)
	h.must(h.eng.RetryTicket(id))
	// A report from the Assignment a retry replaced is refused.
	wantUserError(t, "replaced assignment", h.eng.FileReport(id, ReportInput{AssignmentID: first, State: ReportReady}))
	if h.ticket(id).Ended != nil {
		t.Error("a stale ready report ended the ticket")
	}
}

func TestAddPersonInputRefusals(t *testing.T) {
	h := newHarness(t)
	wantUserError(t, "unknown ticket", h.eng.AddPersonInput("T-9999", "hi"))
	id := h.accepted()
	wantUserError(t, "empty text", h.eng.AddPersonInput(id, "  "))
}

// TestConcurrentCommands drives the Engine from several goroutines, as the Executor's callbacks and a person's commands
// arrive; run with -race.
func TestConcurrentCommands(t *testing.T) {
	h := newHarness(t)
	h.exec.strict, h.events.strict = false, false
	var ids []string
	for range 4 {
		ids = append(ids, h.accepted())
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		a := h.ticket(id).Assignments[0].ID
		wg.Add(3)
		go func() {
			defer wg.Done()
			for range 20 {
				if err := h.eng.AddPersonInput(id, "more"); err != nil {
					t.Error(err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 20 {
				tk, _, err := h.eng.Ticket(id)
				if err != nil {
					t.Error(err)
					return
				}
				if err := h.eng.InputsDelivered(id, a, tk.Undelivered); err != nil {
					t.Error(err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := h.eng.Tickets(true); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		tk := h.ticket(id)
		eq(t, "inputs", len(tk.Inputs), 21)
		// Every Input is either still waiting or was delivered, never both.
		waiting := map[string]bool{}
		for _, u := range tk.Undelivered {
			waiting[u] = true
		}
		for _, in := range tk.Inputs[1:] {
			if waiting[in.ID] == (in.DeliveredTo != "") {
				t.Errorf("input %s: waiting=%v deliveredTo=%q", in.ID, waiting[in.ID], in.DeliveredTo)
			}
		}
	}
}

// --- Ported from the reference tests/core-engine.spec.ts ---

// "keeps inputs undelivered until an assignment is running, and asks to launch one whose launch failed again"
func TestRefKeepsInputsUndeliveredUntilAssignmentRuns(t *testing.T) {
	h := newHarness(t)
	tk := h.submit("Fix")
	h.settle(false)
	h.must(h.eng.AddPersonInput(tk.ID, "also this"))
	h.settle(false)
	stored := h.ticket(tk.ID)
	eq(t, "undelivered", len(stored.Undelivered), 2)
	eq(t, "no deliveries", h.exec.count("deliver"), 0)
	eq(t, "start asked again", h.exec.count("start"), 2)
	h.must(h.eng.AssignmentStarted(tk.ID, stored.Assignments[0].ID, "s"))
	h.settle(false)
	eq(t, "delivered", h.delivered, [][2]string{{tk.ID, stored.Undelivered[0]}, {tk.ID, stored.Undelivered[1]}})
}

// "takes delivery reports only from the assignment that still runs the open ticket"
func TestRefDeliveryReportsOnlyFromRunningAssignment(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.ticket(id).Assignments[0].ID
	h.must(h.eng.AddPersonInput(id, "2"))
	input := lastInput(h.ticket(id))
	h.must(h.eng.DeliveryFailed(id, a, "Session busy"))
	eq(t, "delivery", h.ticket(id).Delivery, &DeliveryFailure{Error: "Session busy", At: base})
	h.must(h.eng.DeliveryFailed(id, "another", "other"))
	h.must(h.eng.InputsDelivered(id, "another", []string{input}))
	eq(t, "undelivered kept", h.ticket(id).Undelivered, []string{input})
	eq(t, "delivery kept", h.ticket(id).Delivery.Error, "Session busy")
	eq(t, "stage", stageOf(h.ticket(id)), Stage{"person", "delivery"})
	h.must(h.eng.InputsDelivered(id, a, []string{input}))
	eq(t, "undelivered", h.ticket(id).Undelivered, []string(nil))
	if h.ticket(id).Delivery != nil {
		t.Error("delivery not cleared")
	}
	h.must(h.eng.AddPersonInput(id, "3"))
	h.must(h.eng.StopTicket(id, ""))
	h.must(h.eng.DeliveryFailed(id, a, "late"))
	stored := h.ticket(id)
	eq(t, "undelivered after stop", stored.Undelivered, []string(nil))
	if stored.Ended == nil || stored.Delivery != nil {
		t.Errorf("ended %v delivery %v", stored.Ended, stored.Delivery)
	}
	// Long errors are cut to 300 characters.
	id2 := h.accepted()
	h.must(h.eng.DeliveryFailed(id2, h.ticket(id2).Assignments[0].ID, strings.Repeat("x", 400)))
	eq(t, "error length", len(h.ticket(id2).Delivery.Error), 300)
}

// "resumes an ended ticket on any person's message on its thread, under the same mandate and without a Decision"
func TestRefResumesEndedTicketOnPersonInput(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportReady, Summary: "done"}))
	mandate := h.ticket(id).Mandate
	h.tick(1)
	h.must(h.eng.AddPersonInput(id, "one more thing"))
	stored := h.ticket(id)
	if stored.Ended != nil {
		t.Fatalf("still ended: %#v", stored.Ended)
	}
	eq(t, "assignments", assignmentStates(stored), []AssignmentState{AssignmentEnded, AssignmentPending})
	eq(t, "mandate", stored.Mandate, mandate)
	eq(t, "input kinds", inputKinds(stored), []InputKind{InputTrigger, InputTrigger, InputResumed})
	resumed := stored.Inputs[2]
	eq(t, "resumed names the message", resumed.TriggerID, stored.Inputs[1].TriggerID)
	// The resume note goes ahead of the message.
	eq(t, "undelivered", stored.Undelivered, []string{resumed.ID, stored.Inputs[1].ID})
	eq(t, "start", h.exec.count("start"), 2)
	eq(t, "start new assignment", h.exec.last("start").AssignmentID, stored.Assignments[1].ID)
	eq(t, "no new decision", len(h.repos.decisionList()), 1)
	types := h.events.types()
	eq(t, "events", types[len(types)-2:], []string{EventInputAdded, EventTicketResumed})
	h.settle(true)
	eq(t, "delivered after resume", h.ticket(id).Undelivered, []string(nil))
}

// "keeps a ticket a person stopped ended: the stop withdrew its mandate"
func TestRefStoppedTicketStaysEnded(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.StopTicket(id, ""))
	eq(t, "stop", h.exec.count("stop"), 1)
	wantUserError(t, "input to stopped ticket", h.eng.AddPersonInput(id, "again"))
	stored := h.ticket(id)
	eq(t, "ended by", stored.Ended.By, EndByHuman)
	eq(t, "start once", h.exec.count("start"), 1)
	eq(t, "inputs", len(stored.Inputs), 1)
}

// "asks, keeps, replaces and clears the question for a person" (without askedBy/answers/outbox, which need Threads)
func TestRefAsksKeepsReplacesQuestion(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportProgress, Summary: "need a call", Question: "Which color?"}))
	asked := h.ticket(id).Attention
	h.must(h.eng.FileReport(id, ReportInput{State: ReportBlocked, Summary: "still waiting"}))
	eq(t, "kept", h.ticket(id).Attention, asked)
	h.must(h.eng.FileReport(id, ReportInput{State: ReportBlocked, Summary: "same", Question: "Which color?"}))
	eq(t, "same question keeps it", h.ticket(id).Attention, asked)
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{State: ReportProgress, Summary: "other", Question: "Which size?"}))
	replaced := h.ticket(id).Attention
	if replaced == nil || replaced.Question != "Which size?" || replaced.ID == asked.ID {
		t.Errorf("replaced: %#v (asked %#v)", replaced, asked)
	}
}

// "ends the ticket on a final report: dropped drops it, ready completes it" (Threads cases skipped)
func TestRefFinalReportOutcomes(t *testing.T) {
	outcome := func(state ReportState) *Ticket {
		h := newHarness(t)
		id := h.accepted()
		h.must(h.eng.FileReport(id, ReportInput{State: ReportProgress, Summary: "asking", Question: "Merge it?"}))
		h.must(h.eng.FileReport(id, ReportInput{State: state, Summary: "done"}))
		return h.ticket(id)
	}
	dropped := outcome(ReportDropped)
	eq(t, "dropped", dropped.Ended, &Ended{At: base, By: EndByExecutor, Outcome: OutcomeDropped})
	ready := outcome(ReportReady)
	eq(t, "ready", ready.Ended.Outcome, OutcomeCompleted)
	if ready.Attention != nil {
		t.Error("ready kept attention")
	}
	failed := outcome(ReportFailed)
	if failed.Ended != nil || failed.Attention == nil || failed.Attention.Question != "Merge it?" {
		t.Errorf("failed: ended %v attention %v", failed.Ended, failed.Attention)
	}
}

// "refuses a question with a result" and "refuses a report on an ended ticket"
func TestRefRefusesQuestionWithResultAndReportOnEnded(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	wantUserError(t, "question with ready", h.eng.FileReport(id, ReportInput{State: ReportReady, Summary: "x", Question: "Q?"}))
	if h.ticket(id).Ended != nil {
		t.Error("refused report ended the ticket")
	}
	h.must(h.eng.StopTicket(id, ""))
	wantUserError(t, "report on ended", h.eng.FileReport(id, ReportInput{State: ReportProgress, Summary: "x"}))
}

// "stops waiting for a person on a person's message, a later report that is neither blocked nor failed, or a retry"
func TestRefStopsWaitingForPerson(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	tk := func() *Ticket { return h.ticket(id) }
	h.must(h.eng.FileReport(id, ReportInput{State: ReportBlocked, Summary: "needs a decision"}))
	eq(t, "waits", tk().WaitsForPerson(), true)
	eq(t, "stage", stageOf(tk()), Stage{"person", "blocked"})
	h.tick(1)
	h.must(h.eng.AddPersonInput(id, "do X"))
	h.settle(true)
	eq(t, "person's message lifts it", tk().WaitsForPerson(), false)
	eq(t, "stage after message", stageOf(tk()), Stage{Stage: "working"})
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{State: ReportFailed, Summary: "broke"}))
	eq(t, "failed waits", tk().WaitsForPerson(), true)
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{State: ReportProgress, Summary: "found a way"}))
	eq(t, "progress lifts it", tk().WaitsForPerson(), false)
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{State: ReportBlocked, Summary: "needs access"}))
	h.must(h.eng.RetryTicket(id))
	eq(t, "retry lifts it", tk().WaitsForPerson(), false)
}

// "launches a failed launch again for a person's message" (the Policy-change half needs Policy)
func TestRefRelaunchesFailedLaunchOnPersonInput(t *testing.T) {
	h := newHarness(t)
	tk := h.submit("Fix")
	h.must(h.eng.FileReport(tk.ID, ReportInput{State: ReportFailed, Summary: "The prepare command failed"}))
	starts := h.exec.count("start")
	h.tick(1)
	h.must(h.eng.AddPersonInput(tk.ID, "try again"))
	eq(t, "start once more", h.exec.count("start"), starts+1)
	last := h.exec.last("start")
	eq(t, "restarts the pending assignment", last.AssignmentID, tk.Assignments[0].ID)
	eq(t, "pending", last.Assignment.State, AssignmentPending)
}

// "retries a ticket that reported failed with a new Assignment" (the status half needs requestStatus)
func TestRefRetriesFailedTicket(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	wantUserError(t, "not retryable", h.eng.RetryTicket(id))
	h.must(h.eng.FileReport(id, ReportInput{State: ReportFailed, Summary: "broke"}))
	h.must(h.eng.RetryTicket(id))
	retried := h.ticket(id)
	eq(t, "assignments", assignmentStates(retried), []AssignmentState{AssignmentEnded, AssignmentPending})
	eq(t, "reason", retried.Assignments[0].EndedReason, "retried")
	eq(t, "stop once", h.exec.count("stop"), 1)
	last := h.exec.last("start")
	eq(t, "start new", last.Assignment, retried.Assignments[1])
}

// "forgets a failed delivery once a new Assignment takes over or the ticket ends"
func TestRefForgetsFailedDelivery(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	failed := func() {
		t.Helper()
		h.must(h.eng.DeliveryFailed(id, runningOf(h.ticket(id)).ID, "Session busy"))
		eq(t, "delivery", h.ticket(id).Delivery.Error, "Session busy")
	}
	failed()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportBlocked, Summary: "needs access"}))
	h.must(h.eng.RetryTicket(id))
	h.settle(true)
	if h.ticket(id).Delivery != nil {
		t.Error("retry kept delivery")
	}
	failed()
	h.must(h.eng.StopTicket(id, ""))
	if h.ticket(id).Delivery != nil {
		t.Error("stop kept delivery")
	}
}

// "starts a new Assignment for a ticket whose Input keeps failing to reach its worker, which takes the Input"
func TestRefRetryAfterFailedDelivery(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportFailed, Summary: "tests fail"}))
	h.tick(5)
	h.must(h.eng.AddPersonInput(id, "try this"))
	answered := h.ticket(id)
	eq(t, "undelivered", len(answered.Undelivered), 1)
	h.must(h.eng.DeliveryFailed(id, runningOf(answered).ID, "Session gone"))
	eq(t, "stage", stageOf(h.ticket(id)), Stage{"person", "delivery"})
	h.must(h.eng.RetryTicket(id))
	retried := h.ticket(id)
	eq(t, "assignments", assignmentStates(retried), []AssignmentState{AssignmentEnded, AssignmentPending})
	if retried.Delivery != nil {
		t.Error("delivery kept")
	}
	// The answer still waits; the task text, delivered to the failed worker but never read, comes back ahead of it.
	eq(t, "undelivered kept", retried.Undelivered, append([]string{retried.Inputs[0].ID}, answered.Undelivered...))
}

// "changes nothing and delivers nothing on a late start of an Assignment a retry replaced"
func TestRefLateStartOfReplacedAssignment(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	first := h.ticket(id).Assignments[0].ID
	h.must(h.eng.FileReport(id, ReportInput{State: ReportFailed, Summary: "broke"}))
	h.must(h.eng.RetryTicket(id))
	before := h.ticket(id)
	delivers := h.exec.count("deliver")
	h.must(h.eng.AssignmentStarted(id, first, "late"))
	h.settle(false)
	eq(t, "assignments", h.ticket(id).Assignments, before.Assignments)
	eq(t, "delivers", h.exec.count("deliver"), delivers)
}

// "refuses to retry a ticket a person stopped after its failed report"
func TestRefRefusesRetryOfStoppedTicket(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportFailed, Summary: "broke"}))
	h.must(h.eng.StopTicket(id, ""))
	wantUserError(t, "retry stopped", h.eng.RetryTicket(id))
}

// "gives a retry's Assignment the messages the failed one had delivered but no step of its worker took"
func TestRefRetryRequeuesDeliveredUnread(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.tick(1)
	h.must(h.eng.AddPersonInput(id, "2"))
	h.settle(true)
	input := lastInput(h.ticket(id))
	eq(t, "delivered", h.ticket(id).Undelivered, []string(nil))
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{State: ReportFailed, Summary: "The turn broke."}))
	h.tick(1)
	h.must(h.eng.RetryTicket(id))
	// The first Input was delivered and not read either, so it comes back too, ahead of the later one.
	eq(t, "requeued", h.ticket(id).Undelivered, []string{h.ticket(id).Inputs[0].ID, input})
}

// A message the worker read is not given to the retry again (the converse of the case above).
func TestRefRetryKeepsReadInputs(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.read(id)
	h.tick(1)
	h.must(h.eng.AddPersonInput(id, "2"))
	h.settle(true)
	input := lastInput(h.ticket(id))
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{State: ReportFailed, Summary: "broke"}))
	h.must(h.eng.RetryTicket(id))
	eq(t, "only the unread one", h.ticket(id).Undelivered, []string{input})
}

// "gives a retry only what the Assignment it replaces had delivered and no step took, not what an earlier end let go of"
func TestRefRetryRequeuesOnlyReplacedAssignmentInputs(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.tick(1)
	h.must(h.eng.AddPersonInput(id, "2"))
	h.settle(true)
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{State: ReportDropped, Summary: "nothing to do"}))
	h.tick(1)
	h.must(h.eng.AddPersonInput(id, "3"))
	three := h.ticket(id).Inputs[len(h.ticket(id).Inputs)-2].ID // the resume note follows it
	h.settle(true)
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{State: ReportBlocked, Summary: "Which page?"}))
	h.tick(1)
	h.must(h.eng.RetryTicket(id))
	eq(t, "requeued", h.ticket(id).Undelivered, []string{three})
}

// "records how a ticket ended"
func TestRefRecordsHowTicketEnded(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.StopTicket(id, "Human asked to stop"))
	eq(t, "ended", *h.ticket(id).Ended, Ended{At: base, By: EndByHuman, Outcome: OutcomeStopped, Reason: "Human asked to stop"})
	// A reason equal to the outcome is not repeated.
	id2 := h.accepted()
	h.must(h.eng.StopTicket(id2, "stopped"))
	eq(t, "no reason", h.ticket(id2).Ended.Reason, "")
}
