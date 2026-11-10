package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// request files an Approval for the ticket's running Assignment and fails the test on error.
func (h *harness) request(ticketID string) Approval {
	h.t.Helper()
	a, err := h.eng.RequestApproval(Approval{
		TicketID: ticketID, AssignmentID: runningOf(h.ticket(ticketID)).ID,
		Tool: "Bash", Summary: "rm -rf build", Rule: "destructive", Input: json.RawMessage(`{"command":"rm -rf build"}`),
	})
	if err != nil {
		h.t.Fatalf("RequestApproval: %v", err)
	}
	return a
}

// waitResult is what a goroutine blocked in WaitApproval got.
type waitResult struct {
	a   Approval
	err error
}

func (h *harness) wait(ctx context.Context, id string) <-chan waitResult {
	out := make(chan waitResult, 1)
	go func() {
		a, err := h.eng.WaitApproval(ctx, id)
		out <- waitResult{a, err}
	}()
	return out
}

func recv(t *testing.T, ch <-chan waitResult) waitResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("WaitApproval did not return")
		return waitResult{}
	}
}

func TestRequestApprovalShowsPersonStage(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	eq(t, "stage before", stageOf(h.ticket(id)), Stage{Stage: "working"})
	h.tick(5)
	a := h.request(id)
	if len(a.ID) != 12 {
		t.Errorf("approval id %q, want 12 hex characters", a.ID)
	}
	eq(t, "state", a.State, ApprovalPending)
	eq(t, "created at", a.CreatedAt, h.now())
	eq(t, "stored", h.repos.approval(a.ID), &a)
	tk := h.ticket(id)
	eq(t, "pending approval", tk.PendingApproval, a.ID)
	eq(t, "stage", stageOf(tk), Stage{"person", "approval"})
	_, st, err := h.eng.Ticket(id)
	h.must(err)
	eq(t, "queried stage", st, Stage{"person", "approval"})
	eq(t, "events", h.events.types()[len(h.events.types())-1], EventApprovalRequested)
	ev := h.events.events[len(h.events.events)-1]
	eq(t, "event approval", ev.ApprovalID, a.ID)
	eq(t, "event ticket", ev.TicketID, id)
	eq(t, "event tool", ev.Data["tool"], any("Bash"))

	pending, err := h.eng.PendingApprovals()
	h.must(err)
	eq(t, "pending", pending, []Approval{a})
}

func TestRequestApprovalRefusals(t *testing.T) {
	h := newHarness(t)
	_, err := h.eng.RequestApproval(Approval{TicketID: "T-9999", Tool: "Bash"})
	wantUserError(t, "unknown ticket", err)
	id := h.accepted()
	h.must(h.eng.StopTicket(id, ""))
	_, err = h.eng.RequestApproval(Approval{TicketID: id, Tool: "Bash"})
	wantUserError(t, "ended ticket", err)
	eq(t, "nothing stored", len(h.repos.approvals), 0)
}

func TestAnswerApprovalWakesWaiter(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.request(id)
	got := h.wait(context.Background(), a.ID)
	select {
	case r := <-got:
		t.Fatalf("WaitApproval returned before an answer: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	h.tick(7)
	h.must(h.eng.AnswerApproval(a.ID, true, "go ahead"))
	r := recv(t, got)
	h.must(r.err)
	eq(t, "state", r.a.State, ApprovalApproved)
	eq(t, "answer", r.a.Answer, "go ahead")
	if r.a.AnsweredAt == nil || !r.a.AnsweredAt.Equal(h.now()) {
		t.Errorf("AnsweredAt = %v, want %v", r.a.AnsweredAt, h.now())
	}
	tk := h.ticket(id)
	eq(t, "pending cleared", tk.PendingApproval, "")
	eq(t, "stage", stageOf(tk), Stage{Stage: "working"})
	last := h.events.events[len(h.events.events)-1]
	eq(t, "event type", last.Type, EventApprovalAnswered)
	eq(t, "event state", last.Data["state"], any(ApprovalApproved))
	pending, err := h.eng.PendingApprovals()
	h.must(err)
	eq(t, "no longer pending", len(pending), 0)
}

func TestDenyApproval(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.request(id)
	h.must(h.eng.AnswerApproval(a.ID, false, "not that"))
	got, err := h.eng.WaitApproval(context.Background(), a.ID)
	h.must(err)
	eq(t, "state", got.State, ApprovalDenied)
	eq(t, "answer", got.Answer, "not that")
}

func TestWaitApprovalAlreadyAnswered(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.request(id)
	h.must(h.eng.AnswerApproval(a.ID, true, ""))
	got, err := h.eng.WaitApproval(context.Background(), a.ID)
	h.must(err)
	eq(t, "state", got.State, ApprovalApproved)
	if n := len(h.eng.waiters); n != 0 {
		t.Errorf("%d waiter channels left registered", n)
	}
}

func TestWaitApprovalContextAndUnknown(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.request(id)
	ctx, cancel := context.WithCancel(context.Background())
	got := h.wait(ctx, a.ID)
	cancel()
	r := recv(t, got)
	if !errors.Is(r.err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", r.err)
	}
	_, err := h.eng.WaitApproval(context.Background(), "nope")
	wantUserError(t, "unknown approval", err)
	// The Approval is still pending and can still be answered.
	h.must(h.eng.AnswerApproval(a.ID, true, ""))
}

func TestWaitApprovalDoesNotBlockCommands(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.request(id)
	got := h.wait(context.Background(), a.ID)
	done := make(chan error, 1)
	go func() { done <- h.eng.AddPersonInput(id, "hello") }()
	select {
	case err := <-done:
		h.must(err)
	case <-time.After(5 * time.Second):
		t.Fatal("a command blocked behind WaitApproval")
	}
	h.must(h.eng.ExpireApproval(a.ID))
	recv(t, got)
}

func TestExpireApproval(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.request(id)
	got := h.wait(context.Background(), a.ID)
	h.tick(3)
	h.must(h.eng.ExpireApproval(a.ID))
	r := recv(t, got)
	h.must(r.err)
	eq(t, "state", r.a.State, ApprovalExpired)
	if r.a.AnsweredAt == nil || !r.a.AnsweredAt.Equal(h.now()) {
		t.Errorf("AnsweredAt = %v, want %v", r.a.AnsweredAt, h.now())
	}
	tk := h.ticket(id)
	eq(t, "pending cleared", tk.PendingApproval, "")
	last := h.events.events[len(h.events.events)-1]
	eq(t, "event type", last.Type, EventApprovalAnswered)
	eq(t, "event state", last.Data["state"], any(ApprovalExpired))
	// Expiring again, or one an answer beat, changes nothing.
	h.must(h.eng.ExpireApproval(a.ID))
	eq(t, "still expired", h.repos.approval(a.ID).State, ApprovalExpired)
	wantUserError(t, "unknown approval", h.eng.ExpireApproval("nope"))
}

func TestAnswerTwiceIsUserError(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.request(id)
	h.must(h.eng.AnswerApproval(a.ID, true, ""))
	wantUserError(t, "second answer", h.eng.AnswerApproval(a.ID, false, "too late"))
	eq(t, "state kept", h.repos.approval(a.ID).State, ApprovalApproved)
	h.must(h.eng.ExpireApproval(a.ID))
	eq(t, "expire does not override", h.repos.approval(a.ID).State, ApprovalApproved)
	wantUserError(t, "unknown approval", h.eng.AnswerApproval("nope", true, ""))
}

func TestSeveralApprovalsKeepStagePerson(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	first := h.request(id)
	h.tick(1)
	second := h.request(id)
	eq(t, "first stays shown", h.ticket(id).PendingApproval, first.ID)
	h.must(h.eng.AnswerApproval(first.ID, true, ""))
	tk := h.ticket(id)
	eq(t, "second shown", tk.PendingApproval, second.ID)
	eq(t, "stage", stageOf(tk), Stage{"person", "approval"})
	h.must(h.eng.AnswerApproval(second.ID, true, ""))
	eq(t, "stage after both", stageOf(h.ticket(id)), Stage{Stage: "working"})
}

func TestEndingTicketExpiresApprovals(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.request(id)
	b := h.request(id)
	got := h.wait(context.Background(), a.ID)
	h.must(h.eng.StopTicket(id, "enough"))
	r := recv(t, got)
	h.must(r.err)
	eq(t, "waiter state", r.a.State, ApprovalExpired)
	eq(t, "second state", h.repos.approval(b.ID).State, ApprovalExpired)
	tk := h.ticket(id)
	eq(t, "pending cleared", tk.PendingApproval, "")
	eq(t, "stage", stageOf(tk), Stage{Stage: "ended"})
	pending, err := h.eng.PendingApprovals()
	h.must(err)
	eq(t, "none pending", len(pending), 0)
	answered := 0
	for _, e := range h.events.events {
		if e.Type == EventApprovalAnswered {
			answered++
		}
	}
	eq(t, "answered events", answered, 2)
}

func TestApprovalTakesTheTicketsRepoAndAllowedCallsCountNothing(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	tk := h.ticket(id)
	tk.Repo = "acme/web"
	raw, err := json.Marshal(tk)
	h.must(err)
	h.repos.mu.Lock()
	h.repos.tickets[id] = raw
	h.repos.mu.Unlock()
	a := h.request(id)
	eq(t, "repo", a.Repo, "acme/web")
	eq(t, "escalations after a request", h.ticket(id).Stats.Escalations, 1)

	h.must(h.eng.AllowedByRule(id, "Bash", "bash-escalate:npm", "acme/web"))
	eq(t, "escalations after an allowed call", h.ticket(id).Stats.Escalations, 1)
	ev := h.events.events[len(h.events.events)-1]
	eq(t, "event", ev.Type, EventApprovalAllowed)
	eq(t, "event text", ev.Data["text"], any("Allowed by your standing rule: Bash · bash-escalate:npm in acme/web"))
}

func TestApprovalAndProposalGetters(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.request(id)
	got, err := h.eng.Approval(a.ID)
	h.must(err)
	eq(t, "approval", got, a)
	var ue *UserError
	if _, err := h.eng.Approval("nope"); !errors.As(err, &ue) {
		t.Fatalf("missing approval = %v, want a UserError", err)
	}
	if _, err := h.eng.Proposal("nope"); !errors.As(err, &ue) {
		t.Fatalf("missing proposal = %v, want a UserError", err)
	}
}
