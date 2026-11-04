package core

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// Intake: a Source's Triggers, the Threads a Source makes a Ticket follow and their closes, ported from the reference
// (src/core/intake.ts; bind, threadClosed, followsDone and unfinished in tickets.ts; followupOwners in model.ts).

// CodeNotListening is the Disposition.Filtered code of a Trigger whose primary Thread no Source listens to.
const CodeNotListening = "not_listening"

// Results of Receive.
const (
	ReceiveStored    = "stored"
	ReceiveDuplicate = "duplicate"
	ReceiveFiltered  = "filtered"
)

// Receive stores a Source's Trigger once and answers stored, duplicate (its ID is stored already) or filtered (no
// Source listens to its primary Thread; it is stored with that Disposition and goes nowhere). A stored Trigger is
// recorded on each of its Threads and fans out, as an Input, to every live Ticket subscribed to any of them, with no
// check of who sent it. A person's message also reaches an ended Ticket a person did not stop on a Thread that is not
// followed only for context, which resumes at once under its Mandate if the Mandate still revalidates; otherwise the
// message reaches no Ticket. A person's message that reached a Ticket on a Thread of its own stays that Ticket's;
// every other Trigger, bots' and CI's included, is queued for Eval on its primary Thread.
func (e *Engine) Receive(trigger Trigger) (string, error) {
	if trigger.ID == "" {
		return "", userErrorf("a trigger needs an id")
	}
	if len(trigger.Threads) == 0 {
		return "", userErrorf("trigger %s belongs to no thread", trigger.ID)
	}
	for _, key := range trigger.Threads {
		if !validThreadKey(key) {
			return "", userErrorf("trigger %s: thread key %q is not <source>:<key>", trigger.ID, key)
		}
	}
	var result string
	err := e.command(cmdReceive, func(o *op) error {
		stored, err := o.tx.GetTrigger(trigger.ID)
		if err != nil {
			return err
		}
		if stored != nil {
			result = ReceiveDuplicate
			return nil
		}
		t := cloneTrigger(trigger)
		t.Disposition = Disposition{}
		primary := t.Threads[0]
		if e.scope != nil && !e.scope.Listens(primary) {
			t.Disposition.Filtered = CodeNotListening
			result = ReceiveFiltered
			return o.tx.PutTrigger(&t)
		}
		result = ReceiveStored
		return o.receive(&t)
	})
	if err != nil {
		return "", err
	}
	return result, nil
}

// reach is a Ticket a Trigger reached; context while it reached it only on Threads it follows for context.
type reach struct {
	ticket  *Ticket
	context bool
}

func (o *op) receive(t *Trigger) error {
	threads := map[string]*Thread{}
	var keys []string
	reached := map[string]*reach{}
	refused := map[string]bool{}
	var order []string
	for _, key := range t.Threads {
		if threads[key] != nil {
			continue
		}
		th, err := o.thread(key)
		if err != nil {
			return err
		}
		threads[key] = th
		keys = append(keys, key)
		if key == t.Threads[0] && len(t.Meta) > 0 {
			// The Source's facts about the Thread, such as its repository, ride on its Triggers.
			if th.Meta == nil {
				th.Meta = map[string]string{}
			}
			maps.Copy(th.Meta, t.Meta)
		}
		for _, s := range th.Subscriptions {
			r := reached[s.TicketID]
			if r == nil && refused[s.TicketID] {
				continue
			}
			if r == nil {
				tk, err := o.tx.GetTicket(s.TicketID)
				if err != nil {
					return err
				}
				if tk == nil {
					continue
				}
				r = &reach{ticket: tk, context: true}
			}
			if r.ticket.Ended != nil {
				// Only a person's message on a Thread that is the Ticket's own resumes an ended Ticket: bots and CI
				// do not (its Executor reads its Threads again when it resumes), and a Ticket a person stopped
				// receives nothing, its Mandate withdrawn.
				if !t.Human || s.Context || r.ticket.Ended.By == EndByHuman {
					continue
				}
			}
			if (r.ticket.Ended != nil || r.ticket.HandedOff()) && reached[s.TicketID] == nil && !o.revalidates(r.ticket) {
				// The message would resume the Ticket's work (an ended Ticket, or one whose worker handed off and
				// waits with no process for its pull request), but the Mandate is revoked: it reaches no Ticket.
				refused[s.TicketID] = true
				continue
			}
			r.context = r.context && s.Context
			if reached[s.TicketID] == nil {
				reached[s.TicketID] = r
				order = append(order, s.TicketID)
			}
		}
	}
	for _, id := range order {
		r := reached[id]
		in := Input{ID: newID(), Kind: InputTrigger, At: o.at, TriggerID: t.ID, Text: t.Text, Human: t.Human, Context: r.context}
		o.addInput(r.ticket, in)
		t.Disposition.Reached = append(t.Disposition.Reached, id)
		if r.context {
			t.Disposition.Context = append(t.Disposition.Context, id)
		}
	}
	lookup := func(id string) (*Ticket, error) {
		if r := reached[id]; r != nil {
			return r.ticket, nil
		}
		return o.tx.GetTicket(id)
	}
	owners, err := followupOwners(t, lookup, threads[t.Threads[0]])
	if err != nil {
		return err
	}
	queued := len(owners) == 0 && queueForEval(threads[t.Threads[0]], t.ID)
	if err := o.followupRelay(t, owners); err != nil {
		return err
	}
	for _, key := range keys {
		if err := o.tx.PutThread(threads[key]); err != nil {
			return err
		}
	}
	if err := o.tx.PutTrigger(t); err != nil {
		return err
	}
	for _, id := range order {
		tk := reached[id].ticket
		o.wake(tk)
		if err := o.tx.PutTicket(tk); err != nil {
			return err
		}
	}
	data := map[string]any{"triggerId": t.ID, "threads": slices.Clone(t.Threads), "human": t.Human,
		"reached": slices.Clone(t.Disposition.Reached), "queued": queued}
	if len(t.Disposition.Context) > 0 {
		data["context"] = slices.Clone(t.Disposition.Context)
	}
	o.emitEvent(Event{Type: EventTriggerReceived, At: o.at, Data: data})
	return nil
}

// BindThread makes an open Ticket follow one of a Source's Threads, such as a pull request's, own when the Ticket
// opened it, and subscribes it to the Thread as its own (no longer for context). A closed Thread bound again is open
// again; own stays once set. The closes of the Threads a Ticket follows, not a ready report, end it.
func (e *Engine) BindThread(ticketID, thread string, own bool) error {
	if !validThreadKey(thread) {
		return userErrorf("thread key %q is not <source>:<key>", thread)
	}
	return e.command(cmdBindThread, func(o *op) error {
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
		th.Closed = nil
		if s := subscription(th, ticketID); s != nil {
			s.Context = false
			s.Bound = true
			s.Own = s.Own || own
		} else {
			th.Subscriptions = append(th.Subscriptions, Subscription{TicketID: ticketID, Bound: true, Own: own})
			o.event(EventThreadSubscribed, t, "", map[string]any{"thread": thread, "own": own})
		}
		if f := follow(t, thread); f != nil {
			f.Own = f.Own || own
			f.Closed = nil
		} else {
			t.Follows = append(t.Follows, Follow{Thread: thread, Own: own})
		}
		t.UpdatedAt = o.at
		if err := o.tx.PutThread(th); err != nil {
			return err
		}
		return o.tx.PutTicket(t)
	})
}

// ThreadClosed records that a Source's Thread closed, merged or closed, at a time: each open Ticket that follows it
// records the close and ends, as the latest close of the Threads it is done with did (FollowsDone), unless its work
// has not finished (unfinished); such work ends it with its next ready or dropped report.
func (e *Engine) ThreadClosed(thread string, outcome TicketOutcome, at time.Time) error {
	if outcome != OutcomeMerged && outcome != OutcomeClosed {
		return userErrorf("a thread closes merged or closed, not %q", outcome)
	}
	if !validThreadKey(thread) {
		return userErrorf("thread key %q is not <source>:<key>", thread)
	}
	return e.command(cmdThreadClosed, func(o *op) error {
		th, err := o.tx.GetThread(thread)
		if err != nil {
			return err
		}
		if th != nil {
			th.Closed = &Close{At: at, Outcome: outcome}
			if err := o.tx.PutThread(th); err != nil {
				return err
			}
		}
		open, err := o.tx.OpenTickets()
		if err != nil {
			return err
		}
		for _, t := range open {
			f := follow(t, thread)
			if f == nil || f.Closed != nil {
				continue
			}
			f.Closed = &Close{At: at, Outcome: outcome}
			t.UpdatedAt = o.at
			if done := t.followsDone(); done != nil {
				unfinished, err := o.unfinished(t, at)
				if err != nil {
					return err
				}
				if !unfinished {
					// Work resumed after the close and finished since ends now; otherwise the Ticket ended with the close.
					when := done.At
					if l := t.LatestAssignment(); l != nil && l.CreatedAt.After(at) {
						when = o.at
					}
					if err := o.endAt(t, EndByExecutor, done.Outcome, "", when); err != nil {
						return err
					}
				}
			}
			if err := o.tx.PutTicket(t); err != nil {
				return err
			}
		}
		return nil
	})
}

// unfinished reports whether a Ticket's work has not finished at a close at `at`: its worker took up, by acknowledging
// or answering it, a person's follow-up the Ticket owns and has not reported other than progress, or ended a turn,
// since its Session took that message into a step; or work resumed after the close (its Assignment was created after
// it) has no ready report, or Inputs other than notes wait or arrived after that report.
func (o *op) unfinished(t *Ticket, at time.Time) (bool, error) {
	until := readUntil(t, false)
	lookup := func(id string) (*Ticket, error) {
		if id == t.ID {
			return t, nil
		}
		return o.tx.GetTicket(id)
	}
	for _, in := range t.Inputs {
		if in.Kind != InputTrigger || readBefore(&in, until) {
			continue
		}
		tr, err := o.tx.GetTrigger(in.TriggerID)
		if err != nil {
			return false, err
		}
		if tr == nil || len(tr.Threads) == 0 || !slices.Contains(tr.Disposition.Acknowledged, t.ID) {
			continue
		}
		primary, err := o.tx.GetThread(tr.Threads[0])
		if err != nil {
			return false, err
		}
		owners, err := followupOwners(tr, lookup, primary)
		if err != nil {
			return false, err
		}
		if slices.Contains(owners, t.ID) {
			return true, nil
		}
	}
	latest := t.LatestAssignment()
	if latest == nil || !latest.CreatedAt.After(at) {
		return false, nil
	}
	r := latest.Report
	if r == nil || r.State != ReportReady {
		return true, nil
	}
	for _, id := range t.Undelivered {
		if in := findInput(t, id); in == nil || in.Kind != InputNote {
			return true, nil
		}
	}
	for _, in := range t.Inputs {
		// A note, such as a Policy change's notice, asks for no work (the reference leaves out its policy-changed
		// Input; notes from the main Session ask for nothing either).
		if in.Kind != InputNote && in.At.After(r.At) {
			return true, nil
		}
	}
	return false, nil
}

// followupOwners returns the Tickets whose worker decides a person's follow-up: those it reached on Threads of their
// own that did not hand it back and can still decide it. A Ticket can while it is live, and while it is ended, not by
// a person, with the Trigger waiting to resume it. None once a person sent it to Eval, or while an Eval holds it.
// primary is the Trigger's primary Thread, or nil when none is stored.
func followupOwners(t *Trigger, ticket func(id string) (*Ticket, error), primary *Thread) ([]string, error) {
	d := t.Disposition
	if !t.Human || d.Requeued != nil {
		return nil, nil
	}
	if primary != nil && primary.Eval != nil && slices.Contains(primary.Eval.TriggerIDs, t.ID) {
		return nil, nil
	}
	var owners []string
	for _, id := range d.Reached {
		if _, back := d.HandedBack[id]; back || slices.Contains(d.Context, id) {
			continue
		}
		tk, err := ticket(id)
		if err != nil {
			return nil, err
		}
		if tk == nil {
			continue
		}
		if tk.Ended == nil || (tk.Ended.By != EndByHuman && waitsToResume(tk, t.ID)) {
			owners = append(owners, id)
		}
	}
	return owners, nil
}

// waitsToResume reports whether an Input of the Trigger waits undelivered on the Ticket.
func waitsToResume(t *Ticket, triggerID string) bool {
	for _, in := range t.Inputs {
		if in.Kind == InputTrigger && in.TriggerID == triggerID && slices.Contains(t.Undelivered, in.ID) {
			return true
		}
	}
	return false
}

// queueForEval queues a Trigger on its primary Thread for the next Eval, unless it waits there already or an Eval
// holds it; it reports whether it queued it.
func queueForEval(primary *Thread, triggerID string) bool {
	if slices.Contains(primary.Queued, triggerID) || (primary.Eval != nil && slices.Contains(primary.Eval.TriggerIDs, triggerID)) {
		return false
	}
	primary.Queued = append(primary.Queued, triggerID)
	return true
}

// readUntil is until when a Ticket's worker had a turn to read what reached it: its latest report (other than
// progress, unless progress is set) or the end of its latest turn, whichever is later.
func readUntil(t *Ticket, progress bool) time.Time {
	var until time.Time
	for i := len(t.Assignments) - 1; i >= 0; i-- {
		if r := t.Assignments[i].Report; r != nil && (progress || r.State != ReportProgress) {
			until = r.At
			break
		}
	}
	for _, a := range t.Assignments {
		if a.IdleAt != nil && a.IdleAt.After(until) {
			until = *a.IdleAt
		}
	}
	return until
}

// readBefore reports whether the worker's Session took a trigger Input into a step before until.
func readBefore(in *Input, until time.Time) bool {
	return in.ReadAt != nil && in.ReadAt.Before(until)
}

// thread loads a Thread, or returns a new one, not yet stored, for its key.
func (o *op) thread(key string) (*Thread, error) {
	th, err := o.tx.GetThread(key)
	if err != nil || th != nil {
		return th, err
	}
	source, _, _ := strings.Cut(key, ":")
	return &Thread{Key: key, Source: source}, nil
}

func subscription(th *Thread, ticketID string) *Subscription {
	for i := range th.Subscriptions {
		if th.Subscriptions[i].TicketID == ticketID {
			return &th.Subscriptions[i]
		}
	}
	return nil
}

func follow(t *Ticket, thread string) *Follow {
	for i := range t.Follows {
		if t.Follows[i].Thread == thread {
			return &t.Follows[i]
		}
	}
	return nil
}

// validThreadKey reports whether a key has the form <source>:<key>.
func validThreadKey(key string) bool {
	source, rest, ok := strings.Cut(key, ":")
	return ok && source != "" && rest != ""
}

// validRepo reports whether repo is owner/name, each part of the letters, digits, '.', '_' and '-' repository names
// use, and neither part "." or "..".
func validRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	part := func(p string) bool {
		if p == "" || p == "." || p == ".." {
			return false
		}
		for _, r := range p {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
				return false
			}
		}
		return true
	}
	return ok && part(owner) && part(name)
}

// cloneTrigger returns a copy that shares no slice or map with t.
func cloneTrigger(t Trigger) Trigger {
	c := t
	c.Threads = slices.Clone(t.Threads)
	c.Meta = maps.Clone(t.Meta)
	if t.Channels != nil {
		c.Channels = make([]ChannelRef, len(t.Channels))
		for i, ch := range t.Channels {
			ch.Address = maps.Clone(ch.Address)
			c.Channels[i] = ch
		}
	}
	return c
}

// FollowedThreads returns, sorted and once each, the Threads of a Source that open Tickets follow and that have not
// closed for them: the pull requests whose reviews and CI the Source reads.
func (e *Engine) FollowedThreads(source string) ([]string, error) {
	open, err := e.Tickets(false)
	if err != nil {
		return nil, err
	}
	prefix := source + ":"
	seen := map[string]bool{}
	for _, t := range open {
		for _, f := range t.Follows {
			if f.Closed == nil && strings.HasPrefix(f.Thread, prefix) {
				seen[f.Thread] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}
