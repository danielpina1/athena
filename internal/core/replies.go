package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Replies: the Engine's messages to Channels, each by the rule its purpose names, ported from the reference
// (src/core/replies.ts, prompts/replies.md). A reply is a receipt and a `reply` effect written in the command's
// transaction; the Outbox sends it after commit.

// EffectReply is the effect kind that sends a reply; its key is the receipt's ID and its payload names it.
const EffectReply = "reply"

// Purposes of a reply.
const (
	// PurposeAcknowledgement: the worker takes up a person's message that reached its Ticket.
	PurposeAcknowledgement = "acknowledgement"
	// PurposeAnswer: the answer to a person's message that reached the Ticket.
	PurposeAnswer = "answer"
	// PurposeResult: the outcome of a ready or dropped round.
	PurposeResult = "result"
	// PurposeAttention: the Ticket's current question for a person.
	PurposeAttention = "attention"
)

// States of a receipt.
const (
	ReceiptWaiting = "waiting"
	ReceiptSent    = "sent"
	ReceiptDropped = "dropped"
)

// Reasons a receipt is dropped.
const (
	// DropChannelGone: the Source no longer listens to the conversation, or said it can never receive the message.
	DropChannelGone = "channelGone"
	// DropNoChannel: the Source has no Channel to send through.
	DropNoChannel = "noChannel"
	// DropRefused: the Channel refused the message for good, such as a missing permission or an invalid address; the
	// receipt's LastError says why.
	DropRefused = "refused"
)

const (
	// ackMaxWords bounds an acknowledgement (replies.md: "100 words is a hard ceiling").
	ackMaxWords = 100
	// replyMaxRunes bounds every other reply.
	replyMaxRunes = 4000
	// receiptSeq names the counter that orders receipts.
	receiptSeq = "receipt"
)

// ReplyRequest asks for a message to a Channel. TriggerID names the person's message an acknowledgement or answer is
// for (required for both); a result or attention may name one that reached the Ticket, to reply where it was written.
type ReplyRequest struct {
	TicketID  string
	TriggerID string
	// Purpose is acknowledgement, answer, result or attention.
	Purpose string
	Text    string
}

// Receipt is a queued message and what became of it; only the Engine creates one and only the Outbox settles it.
type Receipt struct {
	ID        string `json:"id"`
	TicketID  string `json:"ticketId,omitempty"`
	Purpose   string `json:"purpose"`
	TriggerID string `json:"triggerId,omitempty"`
	Text      string `json:"text"`
	// Channel is where it goes, resolved when it was queued.
	Channel ChannelRef `json:"channel"`
	// Conversation is the Channel's Thread; a Ticket's messages to one conversation go out in queue order.
	Conversation string `json:"conversation"`
	// State is waiting, sent or dropped.
	State string `json:"state"`
	// Started counts the sends begun, recorded before each is made so a send that died unrecorded is known to have
	// begun; Attempts counts only the failed ones.
	Started int `json:"started,omitempty"`
	// Attempts counts the failed sends.
	Attempts      int        `json:"attempts,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	DroppedReason string     `json:"droppedReason,omitempty"`
	DroppedAt     *time.Time `json:"droppedAt,omitempty"`
	SentAt        *time.Time `json:"sentAt,omitempty"`
	// Seq is its place in the queue; QueuedAt is when it was queued.
	Seq      int       `json:"seq"`
	QueuedAt time.Time `json:"queuedAt"`
	// Round names, for a result, the Report round it reports (its Assignment and the Report's time), and for attention
	// the Attention (question) it asks.
	Round string `json:"round,omitempty"`
}

// ReceiptID is a reply's stable ID: the first 16 hex characters of sha256(ticket|purpose|slot|text), so the same
// reply asked for twice is queued once. slot is the Trigger's ID for an acknowledgement or an answer, the round
// (<assignment>@<report time, RFC3339Nano>) for a result and the Attention's ID for attention.
func ReceiptID(ticketID, purpose, slot, text string) string {
	sum := sha256.Sum256([]byte(ticketID + "|" + purpose + "|" + slot + "|" + text))
	return hex.EncodeToString(sum[:])[:16]
}

// Reply queues a message for the Channel its purpose names and returns its receipt's ID; the same request again
// returns the same ID and queues nothing. Each purpose has its rule:
//
//   - acknowledgement: the open Ticket's worker takes up a person's message that reached it other than for context,
//     at most once per Trigger (≤ 100 words); it records the Trigger's Disposition.Acknowledged. A later request for
//     the same Trigger, in any wording, returns the first receipt's ID and queues nothing.
//   - answer: to a person's message that reached the Ticket; it records Disposition.Acknowledged too.
//   - result: once per round whose Report is ready or dropped.
//   - attention: only while the Ticket has an Attention, at most once per Attention: a later request for the same
//     question, in any wording, returns the first receipt's ID and queues nothing.
//
// Other replies are at most 4,000 characters. A refused request returns a *UserError.
func (e *Engine) Reply(r ReplyRequest) (string, error) {
	out, err := e.ReplyOnce(r)
	return out.ReceiptID, err
}

// ReplyOutcome is the receipt a reply request queued, or found queued before.
type ReplyOutcome struct {
	ReceiptID string
	// Repeat: the request repeated a reply queued before (the same message again, a second acknowledgement of a
	// Trigger the Ticket acknowledged already, or the same question again), so nothing new went out.
	Repeat bool
}

// ReplyOnce is Reply, saying whether the request repeated a reply queued before. A second acknowledgement of the same
// Trigger, in any wording, is such a repeat rather than a refusal: the router may have acknowledged the message when it
// accepted the Ticket, before the worker read it.
func (e *Engine) ReplyOnce(r ReplyRequest) (ReplyOutcome, error) {
	if err := checkReply(r); err != nil {
		return ReplyOutcome{}, err
	}
	var out ReplyOutcome
	err := e.command(cmdReply, func(o *op) error {
		var err error
		out, err = o.reply(r)
		return err
	})
	if err != nil {
		return ReplyOutcome{}, err
	}
	return out, nil
}

// checkReply refuses a request whose purpose is unknown or whose text is empty or too long.
func checkReply(r ReplyRequest) error {
	switch r.Purpose {
	case PurposeAcknowledgement, PurposeAnswer, PurposeResult, PurposeAttention:
	default:
		return userErrorf("unknown reply purpose %q (acknowledgement, answer, result or attention)", r.Purpose)
	}
	if strings.TrimSpace(r.Text) == "" {
		return userErrorf("a reply needs text")
	}
	if r.Purpose == PurposeAcknowledgement {
		if n := len(strings.Fields(r.Text)); n > ackMaxWords {
			return userErrorf("an acknowledgement is at most %d words, not %d", ackMaxWords, n)
		}
	} else if n := utf8.RuneCountInString(r.Text); n > replyMaxRunes {
		return userErrorf("a reply is at most %d characters, not %d", replyMaxRunes, n)
	}
	if (r.Purpose == PurposeAcknowledgement || r.Purpose == PurposeAnswer) && r.TriggerID == "" {
		return userErrorf("an %s names the person's message it is for", r.Purpose)
	}
	return nil
}

// reply checks a request against its purpose's rule and queues it once, in the command's transaction.
func (o *op) reply(r ReplyRequest) (ReplyOutcome, error) {
	id, repeat, err := o.queueReply(r)
	return ReplyOutcome{ReceiptID: id, Repeat: repeat}, err
}

func (o *op) queueReply(r ReplyRequest) (string, bool, error) {
	if err := checkReply(r); err != nil {
		return "", false, err
	}
	t, err := o.require(r.TicketID)
	if err != nil {
		return "", false, err
	}
	var tr *Trigger
	if r.TriggerID != "" {
		if tr, err = o.tx.GetTrigger(r.TriggerID); err != nil {
			return "", false, err
		}
		if tr == nil {
			return "", false, o.unknownTrigger(t, r.TriggerID)
		}
		// The owner's messages have no channel: an acknowledgement or answer would land on the Ticket's own thread.
		if tr.Source == ownerSource && (r.Purpose == PurposeAcknowledgement || r.Purpose == PurposeAnswer) {
			return "", false, userErrorf("messages from the owner need no %s: answer in your session, which the owner reads live; %s",
				r.Purpose, o.triggerChoices(t))
		}
	}
	// A result or attention may name a Trigger to reply where it was written: only one that reached the Ticket.
	if tr != nil && (r.Purpose == PurposeResult || r.Purpose == PurposeAttention) && !reachedTicket(t, tr.ID, true) {
		return "", false, userErrorf("trigger %s did not reach ticket %s; %s", tr.ID, t.ID, o.triggerChoices(t))
	}
	// The ID's third part (R15): a result's round, an attention's Attention, else the Trigger, so the same text in a
	// later round or for a later question is a new message.
	slot, round := r.TriggerID, ""
	switch r.Purpose {
	case PurposeResult:
		if l := t.LatestAssignment(); l != nil && l.Report != nil {
			round = l.ID + "@" + l.Report.At.UTC().Format(time.RFC3339Nano)
			slot = round
		}
	case PurposeAttention:
		if t.Attention != nil {
			slot, round = t.Attention.ID, t.Attention.ID
		}
	}
	id := ReceiptID(t.ID, r.Purpose, slot, r.Text)
	existing, err := o.tx.GetReceipt(id)
	if err != nil {
		return "", false, err
	}
	if existing != nil {
		// Queued before: nothing new goes out, but the worker still took the message up.
		return id, true, o.takeUp(t, r, tr)
	}
	receipts, err := o.tx.TicketReceipts(t.ID)
	if err != nil {
		return "", false, err
	}
	asker := tr
	switch r.Purpose {
	case PurposeAcknowledgement:
		if t.Ended != nil {
			return "", false, userErrorf("ticket %s has ended", t.ID)
		}
		if !reachedTicket(t, tr.ID, false) {
			return "", false, userErrorf("trigger %s did not reach ticket %s other than for context, so it needs no acknowledgement; %s",
				tr.ID, t.ID, o.triggerChoices(t))
		}
		if !tr.Human {
			return "", false, userErrorf("trigger %s is not a person's message, so it needs no acknowledgement; %s", tr.ID, o.triggerChoices(t))
		}
		// Acknowledged before (by the router's accept, or by the worker in other words): nothing new goes out.
		if i := slices.IndexFunc(receipts, func(x *Receipt) bool { return x.Purpose == PurposeAcknowledgement && x.TriggerID == tr.ID }); i >= 0 {
			return receipts[i].ID, true, o.takeUp(t, r, tr)
		}
	case PurposeAnswer:
		if !reachedTicket(t, tr.ID, true) {
			return "", false, userErrorf("trigger %s did not reach ticket %s; %s", tr.ID, t.ID, o.triggerChoices(t))
		}
		if !tr.Human {
			return "", false, userErrorf("trigger %s is not a person's message, so it needs no answer; %s", tr.ID, o.triggerChoices(t))
		}
	case PurposeResult:
		l := t.LatestAssignment()
		if l == nil || l.Report == nil || (l.Report.State != ReportReady && l.Report.State != ReportDropped) ||
			(t.Ended != nil && t.Ended.By == EndByHuman) {
			return "", false, userErrorf("ticket %s has no ready or dropped report to give the result of: the result follows "+
				"the worker's ready or dropped report", t.ID)
		}
		if slices.ContainsFunc(receipts, func(x *Receipt) bool { return x.Purpose == PurposeResult && x.Round == round }) {
			return "", false, userErrorf("ticket %s gave the result of this round already", t.ID)
		}
	case PurposeAttention:
		if t.Attention == nil {
			return "", false, userErrorf("ticket %s has no question for a person", t.ID)
		}
		// At most once per question: asked again, in any wording (a relay resent after a restart), it is the receipt
		// already queued, and nothing new goes out.
		if i := slices.IndexFunc(receipts, func(x *Receipt) bool { return x.Purpose == PurposeAttention && x.Round == round }); i >= 0 {
			return receipts[i].ID, true, nil
		}
		if asker == nil && t.Attention.TriggerID != "" {
			if asker, err = o.tx.GetTrigger(t.Attention.TriggerID); err != nil {
				return "", false, err
			}
		}
	}
	ch, ok := replyChannel(t, asker)
	if !ok {
		return "", false, userErrorf("ticket %s has no channel to reply to", t.ID)
	}
	seq, err := o.tx.NextSeq(receiptSeq)
	if err != nil {
		return "", false, err
	}
	rc := &Receipt{ID: id, TicketID: t.ID, Purpose: r.Purpose, TriggerID: r.TriggerID, Text: r.Text, Channel: ch,
		Conversation: ch.Thread, State: ReceiptWaiting, Seq: seq, QueuedAt: o.at, Round: round}
	if err := o.tx.PutReceipt(rc); err != nil {
		return "", false, err
	}
	payload, err := json.Marshal(struct {
		ReceiptID string `json:"receiptId"`
	}{id})
	if err != nil {
		return "", false, err
	}
	if err := o.enqueue(EffectReply, id, payload); err != nil {
		return "", false, err
	}
	return id, false, o.takeUp(t, r, tr)
}

// takeUp records, for an acknowledgement or an answer, that the Ticket's worker took the person's message up.
func (o *op) takeUp(t *Ticket, r ReplyRequest, tr *Trigger) error {
	if tr == nil || (r.Purpose != PurposeAcknowledgement && r.Purpose != PurposeAnswer) ||
		slices.Contains(tr.Disposition.Acknowledged, t.ID) {
		return nil
	}
	tr.Disposition.Acknowledged = append(tr.Disposition.Acknowledged, t.ID)
	return o.tx.PutTrigger(tr)
}

// replyChannel resolves where a reply goes: the Channel of the Trigger it is for, else the Ticket's first Channel on
// that Trigger's primary Thread (or, without a Trigger, on the Mandate's Thread, or any when the Mandate names none).
// A Ticket with no such Channel (an owner task, or one whose Triggers came without a Channel) has nowhere to reply:
// a Channel made up from the Mandate's Thread alone would carry no address, and the Outbox could only retry it forever.
func replyChannel(t *Ticket, tr *Trigger) (ChannelRef, bool) {
	if tr != nil && len(tr.Channels) > 0 {
		return cloneChannel(tr.Channels[0]), true
	}
	thread := t.Mandate.Thread
	if tr != nil && len(tr.Threads) > 0 {
		thread = tr.Threads[0]
	}
	for _, ch := range t.Channels {
		if thread == "" || ch.Thread == thread {
			return cloneChannel(ch), true
		}
	}
	return ChannelRef{}, false
}

func cloneChannel(ch ChannelRef) ChannelRef {
	ch.Address = maps.Clone(ch.Address)
	return ch
}

// reachedTicket reports whether a Trigger reached the Ticket as an Input; with context false, other than only on a
// Thread it follows for context.
func reachedTicket(t *Ticket, triggerID string, context bool) bool {
	return slices.ContainsFunc(t.Inputs, func(in Input) bool {
		return in.Kind == InputTrigger && in.TriggerID == triggerID && (context || !in.Context)
	})
}

// unknownTrigger refuses a trigger_id that names no Trigger. An Input's own id is the usual mistake: the refusal names
// the Trigger that Input carries, or says it is the owner's and has none.
func (o *op) unknownTrigger(t *Ticket, id string) error {
	for _, in := range t.Inputs {
		if in.ID != id {
			continue
		}
		if in.TriggerID != "" {
			if tr, err := o.tx.GetTrigger(in.TriggerID); err == nil && tr != nil && tr.Source != ownerSource {
				return userErrorf("%s is an input id, not a trigger_id: that input's trigger_id is %s; %s", id, tr.ID, o.triggerChoices(t))
			}
		}
		return userErrorf("%s is the input id of a message from the owner, which has no trigger_id: it needs no "+
			"acknowledgement, answer or hand-back; answer in your session output, which the owner reads live; %s", id, o.triggerChoices(t))
	}
	return userErrorf("trigger %s not found: use a trigger_id from an input card; %s", id, o.triggerChoices(t))
}

// triggerChoices names, for a refusal, the trigger_ids a Ticket's worker may acknowledge, answer or hand back: the
// people's messages that reached the Ticket, other than the owner's, each marked when the Ticket acknowledged it
// already or when it reached the Ticket only for context (answer it, never acknowledge it).
func (o *op) triggerChoices(t *Ticket) string {
	var ids []string
	seen := map[string]bool{}
	for _, in := range t.Inputs {
		if in.Kind != InputTrigger || in.TriggerID == "" || seen[in.TriggerID] {
			continue
		}
		seen[in.TriggerID] = true
		tr, err := o.tx.GetTrigger(in.TriggerID)
		if err != nil || tr == nil || !tr.Human || tr.Source == ownerSource {
			continue
		}
		label := tr.ID
		switch {
		case slices.Contains(tr.Disposition.Acknowledged, t.ID):
			label += " (acknowledged already)"
		case !reachedTicket(t, tr.ID, false):
			label += " (context only)"
		}
		ids = append(ids, label)
	}
	if len(ids) == 0 {
		return "ticket " + t.ID + " has no person's message with a trigger_id"
	}
	return "ticket " + t.ID + "'s trigger_ids: " + strings.Join(ids, ", ")
}

// ackRefusal returns a *UserError when an accept's acknowledgement text could never be sent, so the accept records
// nothing.
func ackRefusal(ack string) error {
	if strings.TrimSpace(ack) == "" {
		return nil
	}
	if err := checkReply(ReplyRequest{Purpose: PurposeAcknowledgement, TriggerID: "-", Text: ack}); err != nil {
		return userErrorf("the accept's acknowledgement: %s", err)
	}
	return nil
}
