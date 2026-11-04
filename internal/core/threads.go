package core

import (
	"slices"
	"strings"
)

// Threads and Subscriptions a worker, the main Session or a person changes, and the facts the main Session gives a
// worker, ported from the reference (followForContext, markUnrelated, dropSubscriptions, note and requestStatus in
// src/core/tickets.ts).

// statusRequest is the text Input RequestStatus delivers.
const statusRequest = "Report the current ticket status with the report tool. This is a status-only request: do not " +
	"resume implementation or perform external actions."

// FollowForContext subscribes an open Ticket to a related Thread for context: its messages reach the Ticket but lend it
// no follow-up strength and do not resume it. A Ticket subscribed to the Thread already keeps its Subscription as it
// is.
func (e *Engine) FollowForContext(ticketID, thread string) error {
	if !validThreadKey(thread) {
		return userErrorf("thread key %q is not <source>:<key>", thread)
	}
	return e.command(cmdFollowForContext, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		if t.Ended != nil {
			return userErrorf("ticket %s has ended", ticketID)
		}
		th, err := o.thread(thread)
		if err != nil {
			return err
		}
		if subscription(th, ticketID) != nil {
			return nil
		}
		th.Subscriptions = append(th.Subscriptions, Subscription{TicketID: ticketID, Context: true})
		o.event(EventThreadSubscribed, t, "", map[string]any{"thread": thread, "context": true})
		if err := o.notice("thread_followed:"+ticketID+":"+thread,
			Message{Kind: NoticeThreadFollowed, TicketID: ticketID, Thread: thread, At: o.at}); err != nil {
			return err
		}
		return o.tx.PutThread(th)
	})
}

// MarkUnrelated is a worker's finding that a Thread attached to its Ticket is unrelated: only that Ticket's
// Subscription to it is dropped, and the Ticket hands back, with the reason, the follow-ups there it decides (see
// Handback), each queued for Eval once no other Ticket decides it. The Ticket's origin Thread (its Mandate's, or its
// origin Trigger's primary Thread) and a Thread a Source bound it to are refused.
func (e *Engine) MarkUnrelated(ticketID, thread, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return userErrorf("marking a thread unrelated needs a reason")
	}
	return e.command(cmdMarkUnrelated, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		origin, err := o.tx.GetTrigger(t.Origin.TriggerID)
		if err != nil {
			return err
		}
		if thread == t.Mandate.Thread || (origin != nil && len(origin.Threads) > 0 && origin.Threads[0] == thread) {
			return userErrorf("thread %s is the origin of ticket %s", thread, ticketID)
		}
		th, err := o.tx.GetThread(thread)
		if err != nil {
			return err
		}
		var s *Subscription
		if th != nil {
			s = subscription(th, ticketID)
		}
		if s == nil {
			return userErrorf("ticket %s is not subscribed to thread %s", ticketID, thread)
		}
		if s.Bound || follow(t, thread) != nil {
			return userErrorf("a source bound ticket %s to thread %s", ticketID, thread)
		}
		th.Subscriptions = slices.DeleteFunc(th.Subscriptions, func(x Subscription) bool { return x.TicketID == ticketID })
		if len(th.Subscriptions) == 0 {
			th.Subscriptions = nil
		}
		if err := o.tx.PutThread(th); err != nil {
			return err
		}
		o.event(EventThreadUnsubscribed, t, "", map[string]any{"thread": thread, "by": "executor", "reason": reason})
		if err := o.notice("thread_unrelated:"+ticketID+":"+thread,
			Message{Kind: NoticeThreadUnrelated, TicketID: ticketID, Thread: thread, Reason: reason, At: o.at}); err != nil {
			return err
		}
		// The follow-ups on the Thread the Ticket decides go back, through Task 5's hand-back.
		seen := map[string]bool{}
		for _, in := range t.Inputs {
			if in.Kind != InputTrigger || in.TriggerID == "" || seen[in.TriggerID] {
				continue
			}
			seen[in.TriggerID] = true
			tr, err := o.tx.GetTrigger(in.TriggerID)
			if err != nil {
				return err
			}
			if tr == nil || !slices.Contains(tr.Threads, thread) {
				continue
			}
			primary, err := o.tx.GetThread(tr.Threads[0])
			if err != nil {
				return err
			}
			owners, err := followupOwners(tr, o.tx.GetTicket, primary)
			if err != nil {
				return err
			}
			if !slices.Contains(owners, ticketID) {
				continue
			}
			if _, err := o.handback(ticketID, tr, reason); err != nil {
				return err
			}
		}
		return nil
	})
}

// Unsubscribe is a person's, or the main Session's (origin OriginRouter): every Subscription on the Thread is dropped,
// and the Tickets that followed it follow it no more, so its close no longer ends them. Each keeps deciding the
// follow-ups it received there. A Thread with no Subscriptions changes nothing. The main Session hears of a person's.
func (e *Engine) Unsubscribe(thread string, origin ...CommandOrigin) error {
	if !validThreadKey(thread) {
		return userErrorf("thread key %q is not <source>:<key>", thread)
	}
	return e.command(cmdUnsubscribe, func(o *op) error {
		o.origin = originOf(origin)
		th, err := o.tx.GetThread(thread)
		if err != nil || th == nil || len(th.Subscriptions) == 0 {
			return err
		}
		dropped := th.Subscriptions
		th.Subscriptions = nil
		if err := o.tx.PutThread(th); err != nil {
			return err
		}
		for _, s := range dropped {
			t, err := o.tx.GetTicket(s.TicketID)
			if err != nil {
				return err
			}
			if t == nil {
				continue
			}
			if follow(t, thread) != nil {
				t.Follows = slices.DeleteFunc(t.Follows, func(f Follow) bool { return f.Thread == thread })
				if len(t.Follows) == 0 {
					t.Follows = nil
				}
				t.UpdatedAt = o.at
				if err := o.tx.PutTicket(t); err != nil {
					return err
				}
			}
			o.event(EventThreadUnsubscribed, t, "", map[string]any{"thread": thread, "by": "person"})
		}
		// One notice per Thread: a later removal replaces one not yet sent.
		return o.notice("thread_unsubscribed:"+thread, Message{Kind: NoticeThreadUnsubscribed, Thread: thread, At: o.at})
	})
}

// Note gives an open Ticket's worker a fact from other work as a note Input, which grants nothing.
func (e *Engine) Note(ticketID, text string) error {
	if strings.TrimSpace(text) == "" {
		return userErrorf("a note needs text")
	}
	return e.command(cmdNote, func(o *op) error {
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		if t.Ended != nil {
			return userErrorf("ticket %s has ended", ticketID)
		}
		in := Input{ID: newID(), Kind: InputNote, At: o.at, Text: text}
		o.addInput(t, in)
		o.event(EventInputAdded, t, "", map[string]any{"inputId": in.ID})
		return o.tx.PutTicket(t)
	})
}

// RequestStatus asks the worker of an open Ticket for its status, as a text Input, when its Assignment runs and is
// idle: a turn ended after the latest delivery and nothing waits to be delivered. It answers whether it asked.
func (e *Engine) RequestStatus(ticketID string) (bool, error) {
	var asked bool
	err := e.command(cmdRequestStatus, func(o *op) error {
		asked = false
		t, err := o.require(ticketID)
		if err != nil {
			return err
		}
		a := runningAssignment(t)
		if t.Ended != nil || a == nil || !idle(t, a) {
			return nil
		}
		in := Input{ID: newID(), Kind: InputText, At: o.at, Text: statusRequest}
		o.addInput(t, in)
		o.event(EventInputAdded, t, a.ID, map[string]any{"inputId": in.ID, "status": true})
		asked = true
		return o.tx.PutTicket(t)
	})
	return asked, err
}

// idle reports whether a running Assignment's worker has no turn in progress: its latest turn ended after the latest
// delivery, and no Input waits to be delivered.
func idle(t *Ticket, a *Assignment) bool {
	if a.IdleAt == nil || len(t.Undelivered) > 0 {
		return false
	}
	return a.DeliveredAt == nil || a.IdleAt.After(*a.DeliveredAt)
}
