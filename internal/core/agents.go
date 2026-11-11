package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Agent messages: a worker asks another Ticket's worker (AskAgent), which answers (AnswerAgent); either may wait for
// a Ticket to end or a message to be answered (WaitFor), and Agents lists the Tickets a worker can reach (R11). A
// message grants nothing: it reaches its Ticket as an automatic InputAgent. Two limits stop loops: a chain of
// unanswered asks is at most MaxHops long, and a Ticket sends at most MessageLimit messages per MessageWindow. Either
// refusal raises an Attention on the sender, so the owner sees agents messaging in a loop.

// Relations of an AgentView to the Ticket that sees it.
const (
	relationLead    = "lead"
	relationChild   = "child"
	relationSibling = "sibling"
	relationRepo    = "repo"
)

const (
	// messageMaxRunes bounds the text of an agent message.
	messageMaxRunes = 4000
	// agentsMax bounds the Tickets Agents lists.
	agentsMax = 20
	// messageIDPrefix starts every agent message id, so a wait_for target is told from a Ticket id.
	messageIDPrefix = "m-"
	// waitNotYet is WaitFor's note when what it waits for is not done within its bound.
	waitNotYet = "Not yet. End your turn with report waiting; Athena delivers the result to you as a message when it arrives."
)

// AskAgent sends text from the Ticket fromID to the Ticket target and returns the stored message. Refusals are
// UserErrors, checked in order: the text's length, a target that is not a Ticket id, the sender itself, a target that
// has ended, one the sender cannot reach (R11), a held one that waits for the sender, the hop limit and the rate limit. A hop or rate refusal also raises an
// Attention on the sender (unless it has one), which commits although the command returns the refusal.
func (e *Engine) AskAgent(fromID, target, text string) (AgentMessage, error) {
	var msg AgentMessage
	var refusal error
	err := e.command(cmdAskAgent, func(o *op) error {
		refusal = nil
		text, err := messageText(text)
		if err != nil {
			return err
		}
		toID, ok := ParseTicketID(target)
		if !ok {
			return userErrorf("%q is not a ticket id", target)
		}
		from, err := o.require(fromID)
		if err != nil {
			return err
		}
		if from.Ended != nil {
			return userErrorf("ticket %s has ended", fromID)
		}
		if toID == from.ID {
			return userErrorf("a ticket cannot send a message to itself")
		}
		to, err := o.tx.GetTicket(toID)
		if err != nil {
			return err
		}
		if to == nil {
			return userErrorf("there is no ticket %s; agents() lists the live ones.", toID)
		}
		if to.Ended != nil {
			return userErrorf("%s has ended; agents() lists the live ones.", toID)
		}
		if relationOf(from, to) == "" {
			return userErrorf("%s is not one your ticket can reach: agents() lists those it can.", toID)
		}
		if len(to.Assignments) == 0 && (slices.Contains(to.Waiting, from.ID) || slices.Contains(to.Deps, from.ID)) {
			// A held Ticket starts only after the sender ends: the question would wait on its own asker.
			return userErrorf("%s starts only after your ticket ends, so it cannot answer; finish your sub-task or report blocked with a question.", toID)
		}
		lim := o.splitLimits()
		hops, err := o.hops(from.ID, lim.MessageWindow)
		if err != nil {
			return err
		}
		if hops > lim.MaxHops {
			refusal = userErrorf("This message would be hop %d of a chain of agent messages, past the limit of %d. "+
				"Answer from what you know, or report waiting with a question for the owner.", hops, lim.MaxHops)
			return o.loopAttention(from, refusal)
		}
		if refusal, err = o.rateLimit(from, lim); refusal != nil || err != nil {
			return err
		}
		msg = AgentMessage{ID: messageIDPrefix + newID(), From: from.ID, To: to.ID, Text: text, Hops: hops, At: o.at}
		if err := o.tx.PutAgentMessage(&msg); err != nil {
			return err
		}
		o.addInput(to, Input{ID: newID(), Kind: InputAgent, At: o.at, From: from.ID, MessageID: msg.ID, Text: text})
		o.messageEvents(from, to, &msg)
		return o.tx.PutTicket(to)
	})
	if err == nil {
		err = refusal
	}
	if err != nil {
		return AgentMessage{}, err
	}
	return msg, nil
}

// AnswerAgent answers, from the Ticket fromID, the message messageID that was sent to it, and returns the stored
// answer. An answer counts against the rate limit, but not the hop limit; an open asker gets it as an InputAgent with
// Answer set and MessageID naming the question, and an ended one gets nothing more.
func (e *Engine) AnswerAgent(fromID, messageID, text string) (AgentMessage, error) {
	var msg AgentMessage
	var refusal error
	err := e.command(cmdAnswerAgent, func(o *op) error {
		refusal = nil
		text, err := messageText(text)
		if err != nil {
			return err
		}
		from, err := o.require(fromID)
		if err != nil {
			return err
		}
		if from.Ended != nil {
			return userErrorf("ticket %s has ended", fromID)
		}
		q, err := o.tx.GetAgentMessage(messageID)
		if err != nil {
			return err
		}
		if q == nil {
			return userErrorf("no message %s of your ticket", messageID)
		}
		if q.To != from.ID {
			return userErrorf("message %s was not sent to your ticket", messageID)
		}
		if q.ReplyTo != "" {
			return userErrorf("message %s is an answer; it needs none", messageID)
		}
		answered, err := o.tx.AgentAnswer(messageID)
		if err != nil {
			return err
		}
		if answered != nil {
			return userErrorf("you answered message %s already", messageID)
		}
		if refusal, err = o.rateLimit(from, o.splitLimits()); refusal != nil || err != nil {
			return err
		}
		msg = AgentMessage{ID: messageIDPrefix + newID(), From: from.ID, To: q.From, Text: text, ReplyTo: q.ID, Hops: q.Hops, At: o.at}
		if err := o.tx.PutAgentMessage(&msg); err != nil {
			return err
		}
		o.releaseAgentWait(messageWaitKey(q.ID))
		asker, err := o.tx.GetTicket(q.From)
		if err != nil {
			return err
		}
		if asker == nil || asker.Ended != nil {
			if asker != nil {
				o.messageEvents(from, asker, &msg)
			}
			return nil
		}
		o.addInput(asker, Input{ID: newID(), Kind: InputAgent, At: o.at, From: from.ID, MessageID: q.ID, Text: text, Answer: true})
		o.messageEvents(from, asker, &msg)
		return o.tx.PutTicket(asker)
	})
	if err == nil {
		err = refusal
	}
	if err != nil {
		return AgentMessage{}, err
	}
	return msg, nil
}

// WaitFor waits until target is done and says what it came to, or, past min(ctx, WaitForMax), that it is not yet.
// A target starting m- is an agent message from or to ticketID, done once answered; otherwise it is a Ticket id the
// Ticket can reach (R11), done once ended. It does not hold the Engine, so commands run while it waits.
func (e *Engine) WaitFor(ctx context.Context, ticketID, target string) (WaitResult, error) {
	check, key, err := e.waitCheck(ticketID, target)
	if err != nil {
		return WaitResult{}, err
	}
	if r, err := check(); err != nil || r.Done {
		return r, err
	}
	ch := e.agentWait(key)
	defer e.leaveAgentWait(key, ch)
	// The end or answer may have committed between the check and the registration; look again now that its wake-up
	// is certain.
	if r, err := check(); err != nil || r.Done {
		return r, err
	}
	bound := e.waitForMax
	if bound <= 0 {
		bound = WaitForMax
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-ch:
		return check()
	case <-timer.C:
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return WaitResult{}, ctx.Err()
		}
	}
	return WaitResult{Note: waitNotYet}, nil
}

// waitCheck validates a WaitFor and returns the check of whether its target is done, and the agentWaiters key that
// wakes it.
func (e *Engine) waitCheck(ticketID, target string) (func() (WaitResult, error), string, error) {
	target = strings.TrimSpace(target)
	if strings.HasPrefix(strings.ToLower(target), messageIDPrefix) {
		id := messageIDPrefix + target[len(messageIDPrefix):]
		check := func() (r WaitResult, err error) {
			err = e.repos.Tx(func(tx Tx) error {
				m, err := tx.GetAgentMessage(id)
				if err != nil {
					return err
				}
				if m == nil || (m.From != ticketID && m.To != ticketID) {
					return userErrorf("no message %s of your ticket", id)
				}
				answer, err := tx.AgentAnswer(id)
				if err != nil || answer == nil {
					return err
				}
				r = WaitResult{Done: true, Answer: answer}
				return nil
			})
			return r, err
		}
		_, err := check()
		return check, messageWaitKey(id), err
	}
	id, ok := ParseTicketID(target)
	if !ok {
		return nil, "", userErrorf("%q is not a ticket id or a message id", target)
	}
	// Visibility is judged when the wait starts: an open Ticket of the repository stays reachable once it ends, so the
	// wait on it comes to its result.
	rel := ""
	check := func() (r WaitResult, err error) {
		err = e.repos.Tx(func(tx Tx) error {
			from, err := tx.GetTicket(ticketID)
			if err != nil {
				return err
			}
			if from == nil {
				return userErrorf("ticket %s not found", ticketID)
			}
			t, err := tx.GetTicket(id)
			if err != nil {
				return err
			}
			if rel == "" && t != nil && t.ID != from.ID {
				rel = relationOf(from, t)
			}
			if rel == "" || t == nil {
				return userErrorf("%s is not one your ticket can reach: agents() lists those it can.", id)
			}
			if t.Ended == nil {
				return nil
			}
			v := e.agentView(t, rel)
			r = WaitResult{Done: true, Ticket: &v}
			if a := t.LatestAssignment(); a != nil && a.Report != nil {
				r.Summary, r.Head = a.Report.Summary, a.Report.Head
			}
			return nil
		})
		return r, err
	}
	_, err := check()
	return check, ticketWaitKey(id), err
}

// Agents lists the Tickets ticketID can reach (R11): its lead first, then its children and siblings in id order, then
// the open Tickets of its repository, most recently updated first; agentsMax at most in all.
func (e *Engine) Agents(ticketID string) ([]AgentView, error) {
	var out []AgentView
	err := e.repos.Tx(func(tx Tx) error {
		out = nil
		from, err := tx.GetTicket(ticketID)
		if err != nil {
			return err
		}
		if from == nil {
			return userErrorf("ticket %s not found", ticketID)
		}
		seen := map[string]bool{from.ID: true}
		add := func(t *Ticket) {
			if t == nil || seen[t.ID] || len(out) >= agentsMax {
				return
			}
			if rel := relationOf(from, t); rel != "" {
				seen[t.ID] = true
				out = append(out, e.agentView(t, rel))
			}
		}
		var lead *Ticket
		if from.Lead != "" {
			if lead, err = tx.GetTicket(from.Lead); err != nil {
				return err
			}
			add(lead)
		}
		kin := slices.Clone(from.Children)
		if lead != nil {
			kin = append(kin, lead.Children...)
		}
		slices.SortFunc(kin, compareTicketIDs)
		for _, id := range kin {
			t, err := tx.GetTicket(id)
			if err != nil {
				return err
			}
			add(t)
		}
		if from.Repo == "" {
			return nil
		}
		open, err := tx.OpenTickets()
		if err != nil {
			return err
		}
		slices.SortStableFunc(open, func(a, b *Ticket) int {
			return cmp.Or(b.UpdatedAt.Compare(a.UpdatedAt), compareTicketIDs(a.ID, b.ID))
		})
		for _, t := range open {
			add(t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// relationOf is how the Ticket from reaches t: as its lead, its child, its sibling or an open Ticket of its
// repository; empty when it cannot (R11).
func relationOf(from, t *Ticket) string {
	switch {
	case from.Lead != "" && t.ID == from.Lead:
		return relationLead
	case t.Lead != "" && t.Lead == from.ID:
		return relationChild
	case from.Lead != "" && t.Lead == from.Lead:
		return relationSibling
	case from.Repo != "" && t.Repo == from.Repo && t.Ended == nil:
		return relationRepo
	}
	return ""
}

// agentView is what an agent sees of t, reached as rel.
func (e *Engine) agentView(t *Ticket, rel string) AgentView {
	st := TicketStage(t)
	v := AgentView{ID: t.ID, Title: t.Title, Stage: st.Stage, Action: st.Action, Relation: rel, Repo: t.Repo}
	if x := t.Execution; x != nil {
		v.Node, v.Runtime = x.Node, x.Runtime
	}
	if a := t.LatestAssignment(); a != nil {
		if a.Node != "" {
			v.Node = a.Node
		}
		if a.Runtime != "" {
			v.Runtime = a.Runtime
		}
	}
	if v.Node != "" && e.nodes != nil {
		v.NodeName = e.nodes.Name(v.Node)
	}
	if t.Lead != "" && t.Ended != nil && t.Ended.Outcome == OutcomeCompleted {
		v.Branch = childBranch(t.ID)
	}
	return v
}

// compareTicketIDs orders Ticket ids by number: T-0009 before T-0010 before T-10000.
func compareTicketIDs(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}

// messageText is an agent message's text, trimmed, or a UserError when it is empty or too long.
func messageText(text string) (string, error) {
	text = strings.TrimSpace(text)
	if n := utf8.RuneCountInString(text); n < 1 || n > messageMaxRunes {
		return "", userErrorf("a message needs 1 to %d characters; this one has %d", messageMaxRunes, n)
	}
	return text, nil
}

// hops is the hop an ask from the Ticket fromID would be: one past the longest chain among the asks to it that are
// unanswered and were sent within window (an older one no longer counts).
func (o *op) hops(fromID string, window time.Duration) (int, error) {
	list, err := o.tx.AgentMessagesOf(fromID)
	if err != nil {
		return 0, err
	}
	since := o.at.Add(-window)
	most := 0
	for _, m := range list {
		if m.To != fromID || m.ReplyTo != "" || m.At.Before(since) || m.Hops <= most {
			continue
		}
		answer, err := o.tx.AgentAnswer(m.ID)
		if err != nil {
			return 0, err
		}
		if answer == nil {
			most = m.Hops
		}
	}
	return most + 1, nil
}

// rateLimit returns the refusal of one more message from t past the rate limit, having raised the loop Attention, or
// nil when the message may go.
func (o *op) rateLimit(t *Ticket, lim SplitLimits) (refusal, err error) {
	n, err := o.tx.CountAgentMessagesFrom(t.ID, o.at.Add(-lim.MessageWindow))
	if err != nil || n < lim.MessageLimit {
		return nil, err
	}
	refusal = userErrorf("Your ticket sent %d agent messages in the last %s, the limit. Wait, or report waiting with a question for the owner.",
		n, windowWords(lim.MessageWindow))
	return refusal, o.loopAttention(t, refusal)
}

// loopAttention raises, unless the Ticket already has one, the Attention of a hop or rate refusal, so the owner sees
// the loop; the command still returns the refusal, after this commits.
func (o *op) loopAttention(t *Ticket, refusal error) error {
	if t.Attention != nil {
		return nil
	}
	t.Attention = &Attention{ID: newID(), At: o.at,
		Question: fmt.Sprintf("Agents are messaging in a loop. %s Reply to let %s go on, or stop it.", refusal.Error(), t.ID)}
	t.UpdatedAt = o.at
	return o.tx.PutTicket(t)
}

// windowWords says a message window in words: "10 minutes", "1 minute", or a duration below a minute.
func windowWords(d time.Duration) string {
	switch m := int(d / time.Minute); {
	case d%time.Minute != 0 || m == 0:
		return d.String()
	case m == 1:
		return "minute"
	default:
		return fmt.Sprintf("%d minutes", m)
	}
}

// messageEvents emits EventAgentMessage on both Tickets of m.
func (o *op) messageEvents(from, to *Ticket, m *AgentMessage) {
	data := map[string]any{"messageId": m.ID, "from": m.From, "to": m.To}
	if m.ReplyTo != "" {
		data["replyTo"] = m.ReplyTo
	}
	o.event(EventAgentMessage, from, "", data)
	o.event(EventAgentMessage, to, "", data)
}

// --- Waiting ---

// agentWaiter is the channel the WaitFor callers of one key share, and how many wait on it.
type agentWaiter struct {
	ch chan struct{}
	n  int
}

func ticketWaitKey(id string) string  { return "ticket:" + id }
func messageWaitKey(id string) string { return "msg:" + id }

// releaseAgentWait wakes, after the command commits, the WaitFor callers of key.
func (o *op) releaseAgentWait(key string) {
	o.effects = append(o.effects, effect{kind: effectAgentRelease, waitKey: key})
}

// agentWait registers a WaitFor caller of key and returns the channel that closes when it is released.
func (e *Engine) agentWait(key string) chan struct{} {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	w, ok := e.agentWaiters[key]
	if !ok {
		w = &agentWaiter{ch: make(chan struct{})}
		e.agentWaiters[key] = w
	}
	w.n++
	return w.ch
}

// leaveAgentWait unregisters a WaitFor caller of key that waited on ch, and forgets the channel once nobody waits on
// it. A channel already released is no longer registered.
func (e *Engine) leaveAgentWait(key string, ch chan struct{}) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	if w, ok := e.agentWaiters[key]; ok && w.ch == ch {
		if w.n--; w.n <= 0 {
			delete(e.agentWaiters, key)
		}
	}
}

// releaseAgentWait wakes every WaitFor caller of key and forgets its channel.
func (e *Engine) releaseAgentWait(key string) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	if w, ok := e.agentWaiters[key]; ok {
		close(w.ch)
		delete(e.agentWaiters, key)
	}
}
