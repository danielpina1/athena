package core

import (
	"encoding/json"
	"time"
)

// Relays and notices: the main Session's messages about what happened, enqueued in the command's transaction so a
// crash after the commit cannot lose them (reference 007 "Messages delivered to Sessions", src/app/notices.ts). A
// relay steers a person's follow-up, a worker's report or a worker's question into the main Session's turn; a notice
// waits for the next relay or candidate and rides along with it. Payloads carry ids only: the router renders the cards
// from the state current when it sends them.

// Effect kinds of the main Session's messages.
const (
	EffectRelay  = "relay"
	EffectNotice = "notice"
)

// Relay kinds, the "kind" of a relay payload.
const (
	RelayFollowUp        = "follow_up"
	RelayWorkerReport    = "worker_report"
	RelayWorkerAttention = "worker_attention"
)

// Notice kinds, the "kind" of a notice payload.
const (
	NoticeTicketEnded        = "ticket_ended"
	NoticeTriggerRequeued    = "trigger_requeued"
	NoticeThreadUnsubscribed = "thread_unsubscribed"
	NoticeThreadUnrelated    = "thread_unrelated"
	NoticeThreadFollowed     = "thread_followed"
)

// CommandOrigin names who issued a command. The main Session gets no notice of what it did itself (reference: "no redundant
// notice for a save, requeue, unsubscribe or attach it performed itself"), so its commands carry OriginRouter. The
// zero Origin is a person or the system.
type CommandOrigin string

// OriginRouter is the main Session, the router.
const OriginRouter CommandOrigin = "router"

// originOf returns the first origin given, or the zero CommandOrigin.
func originOf(origin []CommandOrigin) CommandOrigin {
	if len(origin) > 0 {
		return origin[0]
	}
	return ""
}

// Message is the payload of a relay or notice effect: its kind and the ids it is about.
type Message struct {
	Kind         string    `json:"kind"`
	TicketID     string    `json:"ticketId,omitempty"`
	AssignmentID string    `json:"assignmentId,omitempty"`
	TriggerID    string    `json:"triggerId,omitempty"`
	AttentionID  string    `json:"attentionId,omitempty"`
	Thread       string    `json:"thread,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	ReportAt     time.Time `json:"reportAt,omitzero"`
	At           time.Time `json:"at,omitzero"`
}

// stamp formats a time for an effect key: it names one fact, so the same fact enqueued twice is one row.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// relay enqueues a relay for the main Session, when the Engine runs one.
func (o *op) relay(key string, m Message) error {
	if !o.relays {
		return nil
	}
	return o.message(EffectRelay, key, m)
}

// notice enqueues a notice for the main Session, unless the main Session's own command caused it.
func (o *op) notice(key string, m Message) error {
	if !o.relays || o.origin == OriginRouter {
		return nil
	}
	return o.message(EffectNotice, key, m)
}

func (o *op) message(kind, key string, m Message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return o.enqueue(kind, key, payload)
}

// ownerTicket reports whether a Ticket is the owner's own task: the main Session neither started it nor can reply on
// it, so its reports and end are no concern of the main Session.
func ownerTicket(t *Ticket) bool {
	return t.Mandate.PolicyVersion == OwnerPolicyVersion || t.Mandate.Basis.Via == ViaOwner
}

// reportRelay relays a worker's report: a new question as worker_attention, any other report but progress as
// worker_report (reference reportRelay). attention is the Attention the report raised, nil when it raised none.
func (o *op) reportRelay(t *Ticket, a *Assignment, attention *Attention) error {
	if ownerTicket(t) || a.Report == nil {
		return nil
	}
	if attention != nil {
		return o.relay("attention:"+t.ID+":"+attention.ID,
			Message{Kind: RelayWorkerAttention, TicketID: t.ID, AssignmentID: a.ID, AttentionID: attention.ID})
	}
	if a.Report.State == ReportProgress {
		return nil
	}
	return o.relay("report:"+t.ID+":"+a.ID+":"+stamp(a.Report.At),
		Message{Kind: RelayWorkerReport, TicketID: t.ID, AssignmentID: a.ID, ReportAt: a.Report.At})
}

// followupRelay relays a person's message that reached Tickets whose workers decide it.
func (o *op) followupRelay(t *Trigger, owners []string) error {
	if !t.Human || len(owners) == 0 {
		return nil
	}
	return o.relay("followup:"+t.ID, Message{Kind: RelayFollowUp, TriggerID: t.ID})
}

// endNotice tells the main Session a Ticket ended.
func (o *op) endNotice(t *Ticket) error {
	if ownerTicket(t) || t.Ended == nil {
		return nil
	}
	return o.notice("ticket_ended:"+t.ID+":"+stamp(o.at), Message{Kind: NoticeTicketEnded, TicketID: t.ID, At: o.at})
}
