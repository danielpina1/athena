package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// channelled gives a Trigger the Channel of a comment on its primary Thread.
func channelled(tr Trigger) Trigger {
	tr.Channels = []ChannelRef{{Source: "github", Thread: tr.Threads[0], Address: map[string]string{"comment": tr.ID}}}
	return tr
}

// replyEffects returns the queued reply effects.
func (h *harness) replyEffects() []fakeEffect {
	var out []fakeEffect
	for _, e := range h.repos.effectList() {
		if e.Kind == EffectReply {
			out = append(out, e)
		}
	}
	return out
}

func (h *harness) receipt(id string) *Receipt {
	h.t.Helper()
	var r *Receipt
	h.must(h.repos.Tx(func(tx Tx) error {
		var err error
		r, err = tx.GetReceipt(id)
		return err
	}))
	if r == nil {
		h.t.Fatalf("receipt %s not stored", id)
	}
	return r
}

func (h *harness) reply(r ReplyRequest) string {
	h.t.Helper()
	id, err := h.eng.Reply(r)
	if err != nil {
		h.t.Fatalf("Reply %+v: %v", r, err)
	}
	return id
}

// sourced accepts a person's request on the issue whose Trigger has a Channel, and returns the Ticket's ID.
func (h *evals) sourced(id string) string {
	h.t.Helper()
	h.receive(channelled(h.request(id, issue)))
	h.due()
	r := h.decide(DecisionInput{TriggerID: id, Kind: "accept", Title: "Fix " + id})
	if r.Ticket == nil {
		h.t.Fatalf("accept of %s: %+v", id, r)
	}
	h.settle(true)
	return r.Ticket.ID
}

func TestReplyIdempotent(t *testing.T) {
	h := newEvals(t)
	a := h.sourced("c:1")
	before := len(h.replyEffects())
	id := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: "It is the cookie."})
	sum := sha256.Sum256([]byte(a + "|answer|c:1|It is the cookie."))
	eq(t, "id", id, hex.EncodeToString(sum[:])[:16])

	fx := h.replyEffects()
	eq(t, "one reply queued", len(fx), before+1)
	last := fx[len(fx)-1]
	eq(t, "effect key", last.Key, id)
	var payload struct {
		ReceiptID string `json:"receiptId"`
	}
	h.must(json.Unmarshal(last.Payload, &payload))
	eq(t, "payload names the receipt", payload.ReceiptID, id)

	r := h.receipt(id)
	eq(t, "receipt", [6]string{r.TicketID, r.Purpose, r.TriggerID, r.Text, r.Conversation, r.State},
		[6]string{a, PurposeAnswer, "c:1", "It is the cookie.", issue, ReceiptWaiting})
	eq(t, "channel: the trigger's", r.Channel, ChannelRef{Source: "github", Thread: issue, Address: map[string]string{"comment": "c:1"}})
	eq(t, "queued", r.Seq > 0 && !r.QueuedAt.IsZero(), true)
	eq(t, "answered records it", h.disposition("c:1").Acknowledged, []string{a})

	h.tick(5)
	again := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: "It is the cookie."})
	eq(t, "same id", again, id)
	eq(t, "nothing new queued", len(h.replyEffects()), before+1)
	eq(t, "receipt unchanged", *h.receipt(id), *r)
}

// Reference replies.md and 003: an acknowledgement goes out at most once per Trigger per Ticket, ≤ 100 words, only for
// a person's message that reached the Ticket other than for context.
func TestSecondAckRefused(t *testing.T) {
	h := newEvals(t)
	a := h.sourced("c:1")
	h.receive(channelled(h.message("c:2", issue)))
	h.settle(true)

	id := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:2", Purpose: PurposeAcknowledgement, Text: "On it: the login button."})
	eq(t, "acknowledged", h.disposition("c:2").Acknowledged, []string{a})
	eq(t, "conversation", h.receipt(id).Conversation, issue)
	eq(t, "identical again: same id", h.reply(ReplyRequest{TicketID: a, TriggerID: "c:2", Purpose: PurposeAcknowledgement,
		Text: "On it: the login button."}), id)
	_, err := h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:2", Purpose: PurposeAcknowledgement, Text: "Working on it."})
	wantUserError(t, "second acknowledgement", err)
	eq(t, "one ack queued", len(h.replyEffects()), 1)
	eq(t, "recorded once", h.disposition("c:2").Acknowledged, []string{a})

	h.receive(channelled(h.message("c:3", issue)))
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:3", Purpose: PurposeAcknowledgement,
		Text: strings.Repeat("word ", 101)})
	wantUserError(t, "over 100 words", err)
	h.reply(ReplyRequest{TicketID: a, TriggerID: "c:3", Purpose: PurposeAcknowledgement, Text: strings.Repeat("word ", 100)})

	_, err = h.eng.Reply(ReplyRequest{TicketID: a, Purpose: PurposeAcknowledgement, Text: "On it."})
	wantUserError(t, "no trigger", err)
	h.receive(channelled(h.botMessage("b:1", issue)))
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "b:1", Purpose: PurposeAcknowledgement, Text: "On it."})
	wantUserError(t, "a bot's message", err)
	h.must(h.eng.FollowForContext(a, other))
	h.receive(channelled(h.message("c:4", other)))
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:4", Purpose: PurposeAcknowledgement, Text: "On it."})
	wantUserError(t, "reached only for context", err)
	h.receive(channelled(h.message("c:5", pull1)))
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:5", Purpose: PurposeAcknowledgement, Text: "On it."})
	wantUserError(t, "did not reach the ticket", err)
}

// R4: an accept that carries an acknowledgement queues it through Reply in the accept's transaction.
func TestAcceptQueuesAck(t *testing.T) {
	h := newEvals(t)
	h.receive(channelled(h.request("c:1", issue)))
	h.due()
	r := h.decide(DecisionInput{TriggerID: "c:1", Kind: "accept", Title: "Fix it", Ack: "On it: fixing the login page."})
	if r.Ticket == nil {
		t.Fatalf("accept: %+v", r)
	}
	fx := h.replyEffects()
	eq(t, "one reply queued", len(fx), 1)
	id := ReceiptID(r.Ticket.ID, PurposeAcknowledgement, "c:1", "On it: fixing the login page.")
	eq(t, "key", fx[0].Key, id)
	rc := h.receipt(id)
	eq(t, "purpose", rc.Purpose, PurposeAcknowledgement)
	eq(t, "to the trigger's channel", rc.Channel.Address["comment"], "c:1")
	eq(t, "acknowledged", h.disposition("c:1").Acknowledged, []string{r.Ticket.ID})
	_, err := h.eng.Reply(ReplyRequest{TicketID: r.Ticket.ID, TriggerID: "c:1", Purpose: PurposeAcknowledgement, Text: "Again."})
	wantUserError(t, "the worker cannot acknowledge it again", err)

	// An acknowledgement over 100 words refuses the accept, which records nothing.
	h.receive(channelled(h.request("c:2", other)))
	h.due()
	_, err = h.eng.Decide(DecisionInput{TriggerID: "c:2", Kind: "accept", Title: "Fix", Ack: strings.Repeat("w ", 101)})
	wantUserError(t, "long ack", err)
	eq(t, "eval still holds it", h.evalOf(other) != nil, true)

	// No ack, no reply.
	h.decide(DecisionInput{TriggerID: "c:2", Kind: "accept", Title: "Fix"})
	eq(t, "still one reply", len(h.replyEffects()), 1)
}

// Reference replies.md: attention only for the Ticket's current question for a person.
func TestAttentionOnlyWithAttention(t *testing.T) {
	h := newEvals(t)
	a := h.sourced("c:1")
	_, err := h.eng.Reply(ReplyRequest{TicketID: a, Purpose: PurposeAttention, Text: "Which page?"})
	wantUserError(t, "no attention", err)

	h.must(h.eng.FileReport(a, ReportInput{State: ReportWaiting, Question: "Which page?"}))
	h.receive(channelled(h.message("c:9", pull1))) // reaches no Ticket
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:9", Purpose: PurposeAttention, Text: "Which page?"})
	wantUserError(t, "attention to a trigger that did not reach the ticket", err)
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:404", Purpose: PurposeAttention, Text: "Which page?"})
	wantUserError(t, "attention to an unknown trigger", err)
	id := h.reply(ReplyRequest{TicketID: a, Purpose: PurposeAttention, Text: "Which page do you mean?"})
	eq(t, "id from the attention", id, ReceiptID(a, PurposeAttention, h.ticket(a).Attention.ID, "Which page do you mean?"))
	rc := h.receipt(id)
	eq(t, "to the ticket's conversation", rc.Conversation, issue)
	eq(t, "the ticket's channel", rc.Channel.Address["comment"], "c:1")
	eq(t, "the same text in one attention: one receipt",
		h.reply(ReplyRequest{TicketID: a, Purpose: PurposeAttention, Text: "Which page do you mean?"}), id)
	eq(t, "one queued", len(h.replyEffects()), 1)
	via := h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAttention, Text: "Which page, exactly?"})
	eq(t, "to a trigger that reached it", h.receipt(via).Channel.Address["comment"], "c:1")

	h.must(h.eng.AddPersonInput(a, "The login page.")) // the person answers: Attention clears
	eq(t, "cleared", h.ticket(a).Attention == nil, true)
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, Purpose: PurposeAttention, Text: "Which page, again?"})
	wantUserError(t, "attention cleared", err)

	// R15: a new question is a new attention; the same text asks again.
	h.tick(1)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportWaiting, Question: "Which page?"}))
	n := len(h.replyEffects())
	again := h.reply(ReplyRequest{TicketID: a, Purpose: PurposeAttention, Text: "Which page do you mean?"})
	if again == id {
		t.Error("a new attention's same text reused the old receipt")
	}
	eq(t, "queued anew", len(h.replyEffects()), n+1)
}

// Reference replies.md: an answer only to a person's message that reached the Ticket; a result once per ready or
// dropped round; anything else is refused.
func TestReplyPurposeRules(t *testing.T) {
	h := newEvals(t)
	a := h.sourced("c:1")
	h.receive(channelled(h.message("c:9", pull1))) // reaches no Ticket
	_, err := h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:9", Purpose: PurposeAnswer, Text: "Yes."})
	wantUserError(t, "answer to a trigger that did not reach it", err)
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, Purpose: PurposeAnswer, Text: "Yes."})
	wantUserError(t, "answer without a trigger", err)
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, Purpose: "progress", Text: "Halfway."})
	wantUserError(t, "unknown purpose", err)
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: strings.Repeat("x", 4001)})
	wantUserError(t, "over 4,000 characters", err)
	h.reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: strings.Repeat("é", 4000)})
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:1", Purpose: PurposeAnswer, Text: " "})
	wantUserError(t, "empty", err)
	_, err = h.eng.Reply(ReplyRequest{TicketID: "T-9999", TriggerID: "c:1", Purpose: PurposeAnswer, Text: "x"})
	wantUserError(t, "unknown ticket", err)

	_, err = h.eng.Reply(ReplyRequest{TicketID: a, Purpose: PurposeResult, Text: "Fixed."})
	wantUserError(t, "result before ready", err)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportReady, Summary: "fixed"}))
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:9", Purpose: PurposeResult, Text: "Fixed in #2."})
	wantUserError(t, "result to a trigger that did not reach the ticket", err)
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, TriggerID: "c:404", Purpose: PurposeResult, Text: "Fixed in #2."})
	wantUserError(t, "result to an unknown trigger", err)
	id := h.reply(ReplyRequest{TicketID: a, Purpose: PurposeResult, Text: "Fixed in #2."})
	l := h.ticket(a).LatestAssignment()
	round := l.ID + "@" + l.Report.At.UTC().Format(time.RFC3339Nano)
	eq(t, "id from the round", id, ReceiptID(a, PurposeResult, round, "Fixed in #2."))
	eq(t, "result to the ticket's conversation", h.receipt(id).Conversation, issue)
	eq(t, "the same result again", h.reply(ReplyRequest{TicketID: a, Purpose: PurposeResult, Text: "Fixed in #2."}), id)
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, Purpose: PurposeResult, Text: "Fixed, really."})
	wantUserError(t, "a second result in one round", err)

	// A person's follow-up resumes it; its next ready round may have its own result.
	h.tick(1)
	h.receive(channelled(h.message("c:2", issue)))
	h.settle(true)
	_, err = h.eng.Reply(ReplyRequest{TicketID: a, Purpose: PurposeResult, Text: "Fixed again."})
	wantUserError(t, "resumed: not ready", err)
	h.tick(1)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportDropped, Summary: "nothing to do"}))
	n := len(h.replyEffects())
	second := h.reply(ReplyRequest{TicketID: a, Purpose: PurposeResult, Text: "Fixed in #2."})
	if second == id {
		t.Error("the next round's same result reused the old receipt")
	}
	eq(t, "queued anew", len(h.replyEffects()), n+1)
	eq(t, "twice in one round: one receipt", h.reply(ReplyRequest{TicketID: a, Purpose: PurposeResult, Text: "Fixed in #2."}), second)
	eq(t, "still one more", len(h.replyEffects()), n+1)
}
