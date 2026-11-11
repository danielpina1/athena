package core

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// repoTask submits a running owner task on repo and returns its id.
func repoTask(h *harness, title, repo string) string {
	h.t.Helper()
	t, err := h.eng.SubmitOwnerTask(title, TaskOptions{Repo: repo, Model: "opus"})
	h.must(err)
	h.settle(true)
	return t.ID
}

func ask(h *harness, from, to, text string) AgentMessage {
	h.t.Helper()
	m, err := h.eng.AskAgent(from, to, text)
	h.must(err)
	return m
}

func agentInputs(t *Ticket) []Input {
	var out []Input
	for _, in := range t.Inputs {
		if in.Kind == InputAgent {
			out = append(out, in)
		}
	}
	return out
}

func messagesOf(h *harness, id string) []*AgentMessage {
	h.t.Helper()
	var out []*AgentMessage
	h.must(h.repos.Tx(func(tx Tx) (err error) { out, err = tx.AgentMessagesOf(id); return }))
	return out
}

func TestAskAgentDeliversInput(t *testing.T) {
	h := splitHarness(t)
	a, b := repoTask(h, "A", "o/r"), repoTask(h, "B", "o/r")
	edit(h, b, func(t *Ticket) { t.Attention = &Attention{ID: "q", Question: "Which colour?", At: base} })
	m := ask(h, a, b, "What schema do you use?")
	if !strings.HasPrefix(m.ID, "m-") {
		t.Errorf("message id %q lacks m-", m.ID)
	}
	eq(t, "message", AgentMessage{From: m.From, To: m.To, Text: m.Text, Hops: m.Hops},
		AgentMessage{From: a, To: b, Text: "What schema do you use?", Hops: 1})
	bt := h.ticket(b)
	ins := agentInputs(bt)
	if len(ins) != 1 {
		t.Fatalf("agent inputs: %+v", ins)
	}
	in := ins[0]
	eq(t, "input", [3]string{in.From, in.MessageID, in.Text}, [3]string{a, m.ID, "What schema do you use?"})
	eq(t, "automatic", automatic(&in), true)
	eq(t, "answer flag", in.Answer, false)
	if bt.Attention == nil || bt.Attention.ID != "q" {
		t.Errorf("attention cleared: %+v", bt.Attention)
	}
	if !strings.Contains(strings.Join(bt.Undelivered, ","), in.ID) {
		t.Errorf("input not queued for delivery: %v", bt.Undelivered)
	}
	eq(t, "messages of a", len(messagesOf(h, a)), 1)
	eq(t, "messages of b", len(messagesOf(h, b)), 1)
	eq(t, "message ids", messagesOf(h, a)[0].ID, messagesOf(h, b)[0].ID)
	n := 0
	for _, ev := range h.eventsOf(EventAgentMessage) {
		if ev.TicketID == a || ev.TicketID == b {
			n++
		}
	}
	eq(t, "agent.message events", n, 2)
}

func TestAnswerAgentReachesAsker(t *testing.T) {
	h := splitHarness(t)
	a, b, c := repoTask(h, "A", "o/r"), repoTask(h, "B", "o/r"), repoTask(h, "C", "o/r")
	m := ask(h, a, b, "Which port?")
	_, err := h.eng.AnswerAgent(c, m.ID, "8080")
	wantRefusal(t, "answer by another", err, "message "+m.ID+" was not sent to your ticket")
	_, err = h.eng.AnswerAgent(b, "m-000000000000", "8080")
	wantRefusal(t, "unknown message", err, "no message m-000000000000 of your ticket")
	ans, err := h.eng.AnswerAgent(b, m.ID, "8080")
	h.must(err)
	eq(t, "answer", AgentMessage{From: ans.From, To: ans.To, ReplyTo: ans.ReplyTo, Text: ans.Text},
		AgentMessage{From: b, To: a, ReplyTo: m.ID, Text: "8080"})
	ins := agentInputs(h.ticket(a))
	if len(ins) != 1 {
		t.Fatalf("asker inputs: %+v", ins)
	}
	eq(t, "asker input", [3]string{ins[0].From, ins[0].MessageID, ins[0].Text}, [3]string{b, m.ID, "8080"})
	eq(t, "answer flag", ins[0].Answer, true)
	_, err = h.eng.AnswerAgent(b, m.ID, "9090")
	wantRefusal(t, "answered twice", err, "you answered message "+m.ID+" already")

	// An ended asker gets nothing more.
	m2 := ask(h, a, b, "And the host?")
	h.must(h.eng.StopTicket(a, "done"))
	before := len(h.ticket(a).Inputs)
	_, err = h.eng.AnswerAgent(b, m2.ID, "localhost")
	h.must(err)
	eq(t, "ended asker inputs", len(h.ticket(a).Inputs), before)
}

func TestHopLimitRaisesAttention(t *testing.T) {
	h := splitHarness(t)
	a, b, c, d := repoTask(h, "A", "o/r"), repoTask(h, "B", "o/r"), repoTask(h, "C", "o/r"), repoTask(h, "D", "o/r")
	eq(t, "hop 1", ask(h, a, b, "1").Hops, 1)
	eq(t, "hop 2", ask(h, b, c, "2").Hops, 2)
	eq(t, "hop 3", ask(h, c, d, "3").Hops, 3)
	_, err := h.eng.AskAgent(d, a, "4")
	sentence := "This message would be hop 4 of a chain of agent messages, past the limit of 3. Answer from what you know, or report waiting with a question for the owner."
	wantUserError(t, "hop 4", err)
	if err != nil {
		eq(t, "hop 4 refusal", err.Error(), sentence)
	}
	dt := h.ticket(d)
	eq(t, "stage", stageOf(dt), Stage{"person", "answer"})
	if dt.Attention == nil {
		t.Fatal("no attention on the sender")
	}
	eq(t, "attention", dt.Attention.Question, "Agents are messaging in a loop. "+sentence+" Reply to let "+d+" go on, or stop it.")
	eq(t, "messages of a", len(messagesOf(h, a)), 1)
}

func TestStaleChainDoesNotBlock(t *testing.T) {
	h := splitHarness(t)
	a, b := repoTask(h, "A", "o/r"), repoTask(h, "B", "o/r")
	h.must(h.repos.Tx(func(tx Tx) error {
		return tx.PutAgentMessage(&AgentMessage{ID: "m-old", From: b, To: a, Text: "old", Hops: 3, At: h.now()})
	}))
	h.tick(int((DefaultMessageWindow + time.Second) / time.Second))
	eq(t, "hop", ask(h, a, b, "fresh").Hops, 1)
}

func TestRateLimitRaisesAttention(t *testing.T) {
	h := splitHarness(t)
	a, b := repoTask(h, "A", "o/r"), repoTask(h, "B", "o/r")
	start := h.now()
	for range 10 {
		ask(h, a, b, "ping")
		h.tick(1)
	}
	_, err := h.eng.AskAgent(a, b, "ping")
	sentence := "Your ticket sent 10 agent messages in the last 10 minutes, the limit. Wait, or report waiting with a question for the owner."
	wantUserError(t, "11th", err)
	if err != nil {
		eq(t, "11th refusal", err.Error(), sentence)
	}
	at := h.ticket(a).Attention
	if at == nil || !strings.Contains(at.Question, sentence) {
		t.Fatalf("attention: %+v", at)
	}
	// 10 minutes and 1 second after the last of them, the next passes.
	h.clock.Store(int64(start.Add(9*time.Second+10*time.Minute+time.Second).Sub(base) / time.Second))
	ask(h, a, b, "ping again")
}

func TestAskRefusals(t *testing.T) {
	h := splitHarness(t)
	a, b := repoTask(h, "A", "o/r"), repoTask(h, "B", "o/r")
	other := repoTask(h, "Other", "o/other")
	ended := repoTask(h, "Ended", "o/r")
	h.must(h.eng.StopTicket(ended, "done"))
	cases := []struct {
		what, from, to, text, want string
	}{
		{"self", a, a, "hi", "cannot send a message to itself"},
		{"ended", a, ended, "hi", ended + " has ended; agents() lists the live ones."},
		{"invisible", a, other, "hi", other + " is not one your ticket can reach: agents() lists those it can."},
		{"bad id", a, "x-1", "hi", "not a ticket id"},
		{"empty", a, b, "  ", "1 to 4000"},
		{"long", a, b, strings.Repeat("é", 4001), "1 to 4000"},
	}
	for _, c := range cases {
		_, err := h.eng.AskAgent(c.from, c.to, c.text)
		wantRefusal(t, c.what, err, c.want)
	}
	if n := len(messagesOf(h, a)); n != 0 {
		t.Errorf("refusals stored %d messages", n)
	}
	if h.ticket(a).Attention != nil {
		t.Error("a plain refusal raised an Attention")
	}
}

func TestWaitForTicketAndMessage(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("ui", "Part A"), sub("api", "Part B"))
	h.settle(true)
	ready(h, ids[0], "UI done", "abc1234def")
	ctx := context.Background()

	r, err := h.eng.WaitFor(ctx, lead, strings.ToLower(ids[0]))
	h.must(err)
	if !r.Done || r.Ticket == nil {
		t.Fatalf("wait for ended child: %+v", r)
	}
	eq(t, "result", [4]string{r.Ticket.ID, r.Ticket.Relation, r.Summary, r.Head}, [4]string{ids[0], "child", "UI done", "abc1234def"})

	_, err = h.eng.WaitFor(ctx, lead, "m-123456789012")
	wantRefusal(t, "all-digit message id", err, "no message m-123456789012 of your ticket")
	_, err = h.eng.WaitFor(ctx, lead, "nope")
	wantUserError(t, "bad id", err)
	other := repoTask(h, "Other", "o/other")
	_, err = h.eng.WaitFor(ctx, lead, other)
	wantRefusal(t, "invisible", err, "is not one your ticket can reach")

	// It wakes when the answer is stored.
	m := ask(h, lead, ids[1], "Which port?")
	type out struct {
		r   WaitResult
		err error
		at  time.Time
	}
	done := make(chan out, 1)
	go func() {
		r, err := h.eng.WaitFor(ctx, ids[1], m.ID)
		done <- out{r, err, time.Now()}
	}()
	time.Sleep(30 * time.Millisecond)
	answered := time.Now()
	_, err = h.eng.AnswerAgent(ids[1], m.ID, "8080")
	h.must(err)
	select {
	case o := <-done:
		h.must(o.err)
		if !o.r.Done || o.r.Answer == nil || o.r.Answer.Text != "8080" {
			t.Fatalf("woken result: %+v", o.r)
		}
		if d := o.at.Sub(answered); d > 50*time.Millisecond {
			t.Errorf("woke after %v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitFor did not wake on the answer")
	}

	// Not done within the bound: the note.
	h.eng.waitForMax = 20 * time.Millisecond
	m2 := ask(h, lead, ids[1], "And the host?")
	r, err = h.eng.WaitFor(ctx, lead, m2.ID)
	h.must(err)
	eq(t, "not yet", r, WaitResult{Note: "Not yet. End your turn with report waiting; Athena delivers the result to you as a message when it arrives."})
	r, err = h.eng.WaitFor(ctx, ids[0], lead)
	h.must(err)
	eq(t, "lead not done", r.Done, false)
}

func TestAgentsVisibleSet(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	s2 := sub("api", "Part B")
	s2.Node = "mac"
	ids := spawn(h, lead, sub("ui", "Part A"), s2)
	h.settle(true)
	r1 := repoTask(h, "R1", "o/r")
	r2 := repoTask(h, "R2", "o/r")
	repoTask(h, "Other", "o/other")
	ended := repoTask(h, "Ended", "o/r")
	h.must(h.eng.StopTicket(ended, "done"))
	h.tick(5)
	h.must(h.eng.AddPersonInput(r1, "go on")) // r1 is now the most recently updated
	ready(h, ids[0], "UI done", "abc1234")

	views, err := h.eng.Agents(ids[1])
	h.must(err)
	var got [][2]string
	for _, v := range views {
		got = append(got, [2]string{v.ID, v.Relation})
	}
	eq(t, "child's view", got, [][2]string{{lead, "lead"}, {ids[0], "sibling"}, {r1, "repo"}, {r2, "repo"}})
	eq(t, "sibling branch", views[1].Branch, "athena/"+strings.ToLower(ids[0]))
	eq(t, "lead branch", views[0].Branch, "")

	views, err = h.eng.Agents(lead)
	h.must(err)
	got = nil
	for _, v := range views {
		got = append(got, [2]string{v.ID, v.Relation})
	}
	eq(t, "lead's view", got, [][2]string{{ids[0], "child"}, {ids[1], "child"}, {r1, "repo"}, {r2, "repo"}})
	eq(t, "node", [2]string{views[1].Node, views[1].NodeName}, [2]string{"n-mac", "mac"})
	eq(t, "stage", views[0].Stage, "ended")
	eq(t, "repo", views[2].Repo, "o/r")
}

// A wait on an open Ticket of the repository that is not kin comes to its result when the Ticket ends.
func TestWaitForRepoTicketThatEnds(t *testing.T) {
	h := splitHarness(t)
	a, b := repoTask(h, "A", "o/r"), repoTask(h, "B", "o/r")
	type out struct {
		r   WaitResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := h.eng.WaitFor(context.Background(), a, b)
		done <- out{r, err}
	}()
	time.Sleep(30 * time.Millisecond)
	h.must(h.eng.StopTicket(b, "done"))
	select {
	case o := <-done:
		h.must(o.err)
		if !o.r.Done || o.r.Ticket == nil {
			t.Fatalf("result: %+v", o.r)
		}
		eq(t, "view", [3]string{o.r.Ticket.ID, o.r.Ticket.Relation, o.r.Ticket.Stage}, [3]string{b, "repo", "ended"})
	case <-time.After(time.Second):
		t.Fatal("WaitFor did not wake on the end")
	}
	// A wait that starts after the end finds it out of reach.
	_, err := h.eng.WaitFor(context.Background(), a, b)
	wantRefusal(t, "ended repo ticket", err, "is not one your ticket can reach")
}

func TestAgentQuestionHeldWhileRecipientWaitsForOwner(t *testing.T) {
	h := splitHarness(t)
	a, b := repoTask(h, "A", "o/r"), repoTask(h, "B", "o/r")
	h.must(h.eng.FileReport(b, ReportInput{State: ReportBlocked, Summary: "stuck", Question: "Which colour?"}))
	before := h.callsFor("deliver", b)
	ask(h, a, b, "What schema do you use?")
	bt := h.ticket(b)
	ins := agentInputs(bt)
	if len(ins) != 1 {
		t.Fatalf("agent inputs: %+v", ins)
	}
	eq(t, "kept undelivered", slices.Contains(bt.Undelivered, ins[0].ID), true)
	eq(t, "not deliverable yet", slices.Contains(bt.Deliverable(), ins[0].ID), false)
	eq(t, "no delivery while waiting", h.callsFor("deliver", b), before)

	h.tick(1)
	h.must(h.eng.AddPersonInput(b, "Blue."))
	eq(t, "deliverable after answer", slices.Contains(h.ticket(b).Deliverable(), ins[0].ID), true)
	h.settle(true)
	if !slices.Contains(h.delivered, [2]string{b, ins[0].ID}) {
		t.Errorf("agent input never delivered: %v", h.delivered)
	}
}

func TestAskHeldTicketWaitingOnSenderRefused(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("a", "A"), sub("b", "B", "a"))
	a, b := ids[0], ids[1]
	// As ApproveBreakdown leaves a plan-first lead: no Assignment, waiting on every child.
	edit(h, lead, func(t *Ticket) { t.Assignments = nil; t.Deps = []string{a, b}; t.Waiting = []string{a, b} })
	h.settle(true)
	_, err := h.eng.AskAgent(a, lead, "Which schema?")
	wantRefusal(t, "held lead", err, lead+" starts only after your ticket ends, so it cannot answer; finish your sub-task or report blocked with a question.")
	_, err = h.eng.AskAgent(a, b, "Ready soon?")
	wantRefusal(t, "held dependent sibling", err, b+" starts only after your ticket ends")
	eq(t, "no inputs", len(agentInputs(h.ticket(lead))), 0)
}
