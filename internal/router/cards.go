package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/danielpina1/athena/internal/core"
)

// Cards: compact, bounded projections of Tickets, Triggers and Decisions for the router, and the candidate, relay and
// notice messages built from them (reference src/app/cards.ts, src/app/notices.ts; sizes from 007 "Result shapes and
// assessment context" and "Read resources").

const (
	previewChars = 160
	// relayChars bounds a Trigger's text or a report's summary in a candidate or relay.
	relayChars = 600
	// relatedTickets is how many of a Thread's Tickets a candidate or an eval lists.
	relatedTickets = 8
	// Text pages: 4,000 UTF-16 code units by default and 8,000 at most; lists: 20 items by default and 50 at most.
	pageChars    = 4000
	maxPageChars = 8000
	pageItems    = 20
	maxPageItems = 50
)

// followupNote is what a follow-up asks of the router: nothing but a stop the person asks for.
const followupNote = "Its ticket's worker has it and decides; act only if the person asks to stop the work " +
	"(route kind stop with this trigger_id)."

// preview collapses whitespace and cuts text for a card.
func preview(text string, n int) string {
	return cut(strings.Join(strings.Fields(text), " "), n)
}

// cut returns the first n runes of s.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// cutText is a text cut to n runes, with its full length in Chars when it was cut.
type cutText struct {
	Text  string `json:"text"`
	Chars int    `json:"chars,omitempty"`
}

func cutTo(s string, n int) cutText {
	if r := []rune(s); len(r) > n {
		return cutText{Text: string(r[:n]) + "…", Chars: len(r)}
	}
	return cutText{Text: s}
}

type endedCard struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	Reason string    `json:"reason"`
}

type attentionCard struct {
	Question  string `json:"question"`
	TriggerID string `json:"trigger_id,omitempty"`
}

// ticketCard is a Ticket as the router sees it in lists and messages.
type ticketCard struct {
	TicketID  string         `json:"ticket_id"`
	Title     string         `json:"title"`
	State     string         `json:"state"`
	Strength  string         `json:"strength"`
	Ended     *endedCard     `json:"ended,omitempty"`
	Attention *attentionCard `json:"attention,omitempty"`
	Preview   string         `json:"preview,omitempty"`
	Read      string         `json:"read"`
}

// cardState is an open Ticket's stage (person/<action>, working, working/queued, idle) or an ended one's outcome.
func cardState(t *core.Ticket) string {
	if t.Ended != nil {
		return string(t.Ended.Outcome)
	}
	st := core.TicketStage(t)
	switch {
	case st.Stage == "person":
		return "person/" + st.Action
	case st.Stage == "working" && st.Action != "":
		return "working/" + st.Action
	}
	return st.Stage
}

func cardOf(t core.Ticket) ticketCard {
	c := ticketCard{TicketID: t.ID, Title: t.Title, State: cardState(&t), Strength: t.Mandate.Strength, Read: "ticket/" + t.ID}
	if e := t.Ended; e != nil {
		reason := e.Reason
		if reason == "" {
			reason = string(e.Outcome)
		}
		c.Ended = &endedCard{At: e.At, By: string(e.By), Reason: reason}
	}
	if a := t.Attention; a != nil {
		c.Attention = &attentionCard{Question: a.Question, TriggerID: a.TriggerID}
	}
	if r := latestReport(&t); r != nil {
		c.Preview = preview(r.Summary, previewChars)
	}
	return c
}

// brief is a Ticket's card without the report preview, for a message that carries or needs no summary.
func brief(t core.Ticket) ticketCard {
	c := cardOf(t)
	c.Preview = ""
	return c
}

func latestReport(t *core.Ticket) *core.Report {
	for i := len(t.Assignments) - 1; i >= 0; i-- {
		if r := t.Assignments[i].Report; r != nil {
			return r
		}
	}
	return nil
}

// related orders a Thread's Tickets open first, newest first within each, and keeps the first eight.
func related(tickets []core.Ticket) ([]ticketCard, int) {
	order := slices.Clone(tickets)
	slices.SortStableFunc(order, func(a, b core.Ticket) int {
		if (a.Ended == nil) != (b.Ended == nil) {
			if a.Ended == nil {
				return -1
			}
			return 1
		}
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	cards := []ticketCard{}
	for i, t := range order {
		if i == relatedTickets {
			break
		}
		cards = append(cards, cardOf(t))
	}
	return cards, len(tickets)
}

// triggerLine is a Trigger in a candidate: who, when and its text up to 600 characters, with a read pointer when cut.
type triggerLine struct {
	TriggerID string `json:"trigger_id"`
	From      string `json:"from"`
	At        string `json:"at"`
	Human     bool   `json:"human,omitempty"`
	Bot       bool   `json:"bot,omitempty"`
	Kind      string `json:"kind,omitempty"`
	cutText
	ReplyTo string `json:"replies_to,omitempty"`
	Read    string `json:"read,omitempty"`
}

func lineOf(tr core.Trigger, n int) triggerLine {
	l := triggerLine{TriggerID: tr.ID, From: tr.Actor.ID, At: tr.At.UTC().Format(time.RFC3339), Human: tr.Human, Bot: tr.Bot,
		Kind: tr.Kind, cutText: cutTo(tr.Text, n), ReplyTo: tr.ReplyTo}
	if l.Chars > 0 {
		l.Read = "trigger/" + tr.ID
	}
	return l
}

func encode(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// candidateCard renders an Eval's candidate: the batch, the Thread's Tickets, where the primary Trigger went, the
// hand-back reasons and what to do next.
func (h *Host) candidateCard(v core.CandidateView) (string, error) {
	var primary *core.Trigger
	lines := make([]triggerLine, 0, len(v.Triggers))
	for i := range v.Triggers {
		tr := &v.Triggers[i]
		if tr.ID == v.Eval.Primary {
			primary = tr
		}
		lines = append(lines, lineOf(*tr, relayChars))
	}
	if primary == nil {
		return "", fmt.Errorf("router: eval %s: primary trigger %s is not in its batch", v.Eval.ID, v.Eval.Primary)
	}
	cards, total := related(v.Tickets)
	next := "Route it with route by trigger_id: accept, attach, stop or ignore."
	if v.Eval.Strength != core.StrengthExplicit {
		next = "Assess it with eval and eval_decide, or ignore it with route (ignored_from_card) when the card already shows it touches no enabled rule."
	}
	card := struct {
		Kind           string            `json:"kind"`
		TriggerID      string            `json:"trigger_id"`
		Strength       string            `json:"strength"`
		Thread         string            `json:"thread"`
		Label          string            `json:"label,omitempty"`
		Triggers       []triggerLine     `json:"triggers"`
		RelatedTickets []ticketCard      `json:"related_tickets"`
		TicketsTotal   int               `json:"tickets_total"`
		DeliveredTo    []string          `json:"delivered_to,omitempty"`
		HandedBack     map[string]string `json:"handed_back,omitempty"`
		Next           string            `json:"next"`
	}{"candidate", primary.ID, v.Eval.Strength, v.Thread.Key, v.Thread.Label, lines, cards, total,
		primary.Disposition.Reached, v.HandedBack, next}
	if len(card.HandedBack) == 0 {
		card.HandedBack = nil
	}
	return encode(card)
}

// renderRelay renders a relay from the state current now; "" drops one that no longer applies.
func (h *Host) renderRelay(m core.Message) (string, error) {
	switch m.Kind {
	case core.RelayFollowUp:
		return h.followupRelay(m.TriggerID)
	case core.RelayWorkerReport, core.RelayWorkerAttention:
		t, _, err := h.eng.Ticket(m.TicketID)
		var ue *core.UserError
		if errors.As(err, &ue) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if m.Kind == core.RelayWorkerAttention {
			// A question answered or replaced since is no longer asked (a replacement has its own relay).
			if t.Attention == nil || t.Attention.ID != m.AttentionID {
				return "", nil
			}
			return encode(struct {
				Kind   string     `json:"kind"`
				Ticket ticketCard `json:"ticket"`
				Read   string     `json:"read"`
			}{core.RelayWorkerAttention, brief(t), "ticket/" + t.ID})
		}
		var a *core.Assignment
		for i := range t.Assignments {
			if t.Assignments[i].ID == m.AssignmentID {
				a = &t.Assignments[i]
			}
		}
		// A report replaced since by the same Assignment is superseded: the newer fact stands (or stays internal).
		if a == nil || a.Report == nil || !a.Report.At.Equal(m.ReportAt) {
			return "", nil
		}
		summary := cutTo(a.Report.Summary, relayChars)
		return encode(struct {
			Kind      string     `json:"kind"`
			Ticket    ticketCard `json:"ticket"`
			TriggerID string     `json:"trigger_id,omitempty"`
			State     string     `json:"state"`
			Summary   string     `json:"summary"`
			Chars     int        `json:"chars,omitempty"`
			Read      string     `json:"read"`
		}{core.RelayWorkerReport, brief(t), t.Origin.TriggerID, string(a.Report.State), summary.Text, summary.Chars, "ticket/" + t.ID})
	}
	return "", nil
}

// followupRelay renders a person's message that reached Tickets whose workers decide it, as their workers read it, with
// those Tickets; "" once no live Ticket decides it.
func (h *Host) followupRelay(triggerID string) (string, error) {
	tr, err := h.eng.Trigger(triggerID)
	var ue *core.UserError
	if errors.As(err, &ue) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	d := tr.Disposition
	if !tr.Human || d.Requeued != nil {
		return "", nil
	}
	type reached struct {
		TicketID string `json:"ticket_id"`
		Title    string `json:"title"`
		State    string `json:"state"`
	}
	var to []reached
	for _, id := range d.Reached {
		if _, back := d.HandedBack[id]; back || slices.Contains(d.Context, id) {
			continue
		}
		t, _, err := h.eng.Ticket(id)
		if errors.As(err, &ue) {
			continue
		}
		if err != nil {
			return "", err
		}
		if t.Ended == nil {
			to = append(to, reached{t.ID, t.Title, cardState(&t)})
		}
	}
	if len(to) == 0 {
		return "", nil
	}
	thread := ""
	if len(tr.Threads) > 0 {
		thread = tr.Threads[0]
	}
	return encode(struct {
		Kind string `json:"kind"`
		triggerLine
		Thread      string    `json:"thread"`
		DeliveredTo []reached `json:"delivered_to"`
		Note        string    `json:"note"`
	}{core.RelayFollowUp, lineOf(tr, relayChars), thread, to, followupNote})
}

// renderNotice renders a held notice; "" leaves out one about something gone.
func (h *Host) renderNotice(payload []byte) (string, error) {
	var m core.Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return "", nil
	}
	at := m.At.UTC().Format(time.RFC3339)
	switch m.Kind {
	case core.NoticeTicketEnded:
		t, _, err := h.eng.Ticket(m.TicketID)
		var ue *core.UserError
		if errors.As(err, &ue) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return encode(struct {
			Kind   string     `json:"kind"`
			Ticket ticketCard `json:"ticket"`
		}{m.Kind, cardOf(t)})
	case core.NoticeTriggerRequeued:
		return encode(struct {
			Kind      string `json:"kind"`
			TriggerID string `json:"trigger_id"`
			At        string `json:"at"`
			Read      string `json:"read"`
		}{m.Kind, m.TriggerID, at, "trigger/" + m.TriggerID})
	case core.NoticeThreadUnsubscribed:
		return encode(struct {
			Kind   string `json:"kind"`
			Thread string `json:"thread"`
			At     string `json:"at"`
			Note   string `json:"note"`
		}{m.Kind, m.Thread, at, "A person removed every ticket's subscription to this thread."})
	case core.NoticeThreadUnrelated:
		return encode(struct {
			Kind     string `json:"kind"`
			TicketID string `json:"ticket_id"`
			Thread   string `json:"thread"`
			Reason   string `json:"reason"`
			At       string `json:"at"`
			Note     string `json:"note"`
		}{m.Kind, m.TicketID, m.Thread, m.Reason, at, "Its worker found this thread unrelated after reading it; attach later " +
			"triggers there to this ticket only when they point at it explicitly."})
	case core.NoticeThreadFollowed:
		return encode(struct {
			Kind     string `json:"kind"`
			TicketID string `json:"ticket_id"`
			Thread   string `json:"thread"`
			At       string `json:"at"`
			Note     string `json:"note"`
		}{m.Kind, m.TicketID, m.Thread, at, "For context only: its messages reach this ticket and grant nothing."})
	}
	return "", nil
}

// reasonText says a Decision's reasons in words.
func reasonText(rs []core.Reason) string {
	var parts []string
	for _, r := range rs {
		if r.Code == core.CodeSaid {
			parts = append(parts, r.Values["text"])
			continue
		}
		s := r.Code
		var vals []string
		for _, k := range slices.Sorted(maps.Keys(r.Values)) {
			vals = append(vals, k+"="+r.Values[k])
		}
		if r.TriggerID != "" {
			vals = append(vals, "trigger="+r.TriggerID)
		}
		if len(vals) > 0 {
			s += " (" + strings.Join(vals, ", ") + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// decisionCard is a Decision in a list.
type decisionCard struct {
	DecisionID  string    `json:"decision_id"`
	Kind        string    `json:"kind"`
	Reason      string    `json:"reason,omitempty"`
	Thread      string    `json:"thread"`
	TriggerIDs  []string  `json:"trigger_ids"`
	TicketID    string    `json:"ticket_id,omitempty"`
	WouldAccept bool      `json:"would_accept,omitempty"`
	At          time.Time `json:"at"`
}

func decisionOf(d core.Decision) decisionCard {
	return decisionCard{DecisionID: d.ID, Kind: d.Kind, Reason: reasonText(d.Reasons), Thread: d.Thread,
		TriggerIDs: d.TriggerIDs, TicketID: d.TicketID, WouldAccept: d.WouldAccept, At: d.At}
}

// textPage is one page of a text, in UTF-16 code units so that pages joined give the text back.
type textPage struct {
	Resource   string `json:"resource"`
	Offset     int    `json:"offset"`
	TotalChars int    `json:"total_chars"`
	NextOffset *int   `json:"next_offset"`
	Text       string `json:"text"`
}

func pageOf(resource, text string, offset, limit int) textPage {
	units := utf16.Encode([]rune(text))
	start := min(offset, len(units))
	end := min(len(units), start+limit)
	p := textPage{Resource: resource, Offset: start, TotalChars: len(units), Text: string(utf16.Decode(units[start:end]))}
	if end < len(units) {
		p.NextOffset = &end
	}
	return p
}

// listPage is one page of a list.
type listPage[T any] struct {
	Items      []T  `json:"items"`
	NextOffset *int `json:"next_offset"`
}

func listOf[T any](items []T, offset, limit int) listPage[T] {
	start := min(offset, len(items))
	end := min(len(items), start+limit)
	p := listPage[T]{Items: items[start:end]}
	if p.Items == nil {
		p.Items = []T{}
	}
	if end < len(items) {
		p.NextOffset = &end
	}
	return p
}
