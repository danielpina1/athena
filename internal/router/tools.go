package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/sources"
)

// The router's tools (reference src/app/tools/main.ts and src/app/assessment.ts), run for the node socket's router role.
// Arguments are checked here; a refusal is an error whose text the model reads.

const (
	maxRouteItems = 20
	titleMax      = 200
	reasonMax     = 500
	// noteWords bounds an attach's routing note; ackWords a note to a worker (reference NOTE_WORDS, IM_WORDS).
	noteWords = 30
	noteMax   = 100
	// Assessment context sizes (007 "Result shapes and assessment context").
	triggerChars = 4000
	batchChars   = 400
	readChars    = 8000
	maxReadIDs   = 20
	// contextTimeout bounds one read of a Thread's window.
	contextTimeout = 30 * time.Second
	// maxWindows bounds the cached windows; Evals that ended are dropped first.
	maxWindows = 64
)

const (
	msgFinished = "No assessment is in progress for this trigger: it was decided, timed out or never reached assessment."
	msgExplicit = "This is an explicit request, which is routed instead of assessed: use route with its trigger_id and " +
		"kind accept, attach, stop or ignore."
	msgNoContext = "This Source gives no thread context; assess the trigger's own text and the batch."
)

// CallTool runs one router tool with its JSON arguments (node.RouterHandler).
func (h *Host) CallTool(ctx context.Context, tool string, args json.RawMessage) (any, error) {
	switch tool {
	case "route":
		return h.route(args)
	case "eval":
		return h.eval(ctx, args)
	case "eval_read":
		return h.evalRead(ctx, args)
	case "eval_decide":
		return h.evalDecide(args)
	case "read":
		return h.read(args)
	case "reply":
		return h.reply(args)
	case "note":
		return h.note(args)
	case "request_status":
		return h.requestStatus(args)
	case "ticket_retry":
		return h.ticketRetry(args)
	case "trigger_requeue":
		return h.triggerRequeue(args)
	case "thread_unsubscribe":
		return h.threadUnsubscribe(args)
	}
	return nil, fmt.Errorf("unknown router tool %q", tool)
}

// decode reads a tool's arguments strictly: an unknown field is an error.
func decode(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid arguments: more than one JSON value")
	}
	return nil
}

// engineError turns an Engine error into what the model reads: a refusal as it is, anything else as not recorded.
func engineError(err error) error {
	var ue *core.UserError
	if errors.As(err, &ue) {
		return errors.New(ue.Msg)
	}
	return fmt.Errorf("not recorded: %v; retry", err)
}

func required(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func (h *Host) card(ticketID string) (*ticketCard, error) {
	t, _, err := h.eng.Ticket(ticketID)
	if err != nil {
		return nil, engineError(err)
	}
	c := cardOf(t)
	return &c, nil
}

// --- route ---

type routeItem struct {
	TriggerID       string `json:"trigger_id"`
	Kind            string `json:"kind"`
	TicketID        string `json:"ticket_id"`
	Title           string `json:"title"`
	Ack             string `json:"ack"`
	Reason          string `json:"reason"`
	IgnoredFromCard bool   `json:"ignored_from_card"`
}

type routeResult struct {
	TriggerID string      `json:"trigger_id"`
	Kind      string      `json:"kind"`
	Decision  string      `json:"decision,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	Ticket    *ticketCard `json:"ticket,omitempty"`
	Refused   string      `json:"refused,omitempty"`
}

// invalid says why an item cannot run: a field its kind needs is missing, or one breaks its limit.
func invalid(it routeItem) string {
	if strings.TrimSpace(it.TriggerID) == "" {
		return "trigger_id is required"
	}
	switch it.Kind {
	case core.DecisionAccept:
		if strings.TrimSpace(it.Title) == "" {
			return "accept needs title"
		}
		if utf8.RuneCountInString(it.Title) > titleMax {
			return fmt.Sprintf("title: at most %d characters", titleMax)
		}
	case core.DecisionIgnore:
		if strings.TrimSpace(it.Reason) == "" {
			return "ignore needs reason"
		}
	case core.DecisionStop:
		if strings.TrimSpace(it.TicketID) == "" {
			return "stop needs ticket_id"
		}
		if strings.TrimSpace(it.Reason) == "" {
			return "stop needs reason"
		}
	case "attach":
		if strings.TrimSpace(it.TicketID) == "" {
			return "attach needs ticket_id"
		}
		if n := len(strings.Fields(it.Reason)); n > noteWords {
			return fmt.Sprintf("reason: an attach's routing note is at most %d words, not %d", noteWords, n)
		}
	default:
		return fmt.Sprintf("unknown kind %q (accept, ignore, stop or attach)", it.Kind)
	}
	if utf8.RuneCountInString(it.Reason) > reasonMax {
		return fmt.Sprintf("reason: at most %d characters", reasonMax)
	}
	if it.IgnoredFromCard && it.Kind != core.DecisionIgnore {
		return "ignored_from_card is for ignore only"
	}
	return ""
}

// route runs 1 to 20 routing decisions in order, each as its own Engine command; a refused item leaves the others.
func (h *Host) route(raw json.RawMessage) (any, error) {
	var args struct {
		Decisions []routeItem `json:"decisions"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if n := len(args.Decisions); n < 1 || n > maxRouteItems {
		return nil, fmt.Errorf("decisions holds 1 to %d items, not %d", maxRouteItems, n)
	}
	// decided maps the Triggers an earlier item of this call decided, with their batches, to that item's Trigger.
	decided := map[string]string{}
	results := make([]routeResult, 0, len(args.Decisions))
	for _, it := range args.Decisions {
		r := routeResult{TriggerID: it.TriggerID, Kind: it.Kind}
		if why := invalid(it); why != "" {
			r.Refused = why
		} else if by, ok := decided[it.TriggerID]; ok && it.Kind == core.DecisionIgnore {
			r.Refused = fmt.Sprintf("decided in this call with %s, whose decision took its batch", by)
		} else {
			ids, err := h.routeOne(it, &r)
			if err != nil {
				r = routeResult{TriggerID: it.TriggerID, Kind: it.Kind, Refused: err.Error()}
			}
			for _, id := range ids {
				if _, ok := decided[id]; !ok {
					decided[id] = it.TriggerID
				}
			}
		}
		if r.Refused != "" {
			h.log.Info("route item refused", "trigger", it.TriggerID, "kind", it.Kind, "refused", r.Refused)
		}
		results = append(results, r)
	}
	return struct {
		Results []routeResult `json:"results"`
	}{results}, nil
}

// routeOne runs one valid item and returns the Triggers its Decision decided.
func (h *Host) routeOne(it routeItem, r *routeResult) ([]string, error) {
	if it.Kind == "attach" {
		if err := h.eng.Attach(it.TicketID, it.TriggerID, strings.TrimSpace(it.Reason)); err != nil {
			return nil, engineError(err)
		}
		c, err := h.card(it.TicketID)
		if err != nil {
			return nil, err
		}
		r.Reason, r.Ticket = "Attached: the ticket follows the trigger's thread and receives the trigger", c
		return nil, nil
	}
	in := core.DecisionInput{TriggerID: it.TriggerID, Kind: it.Kind, Reason: it.Reason, Origin: core.OriginRouter}
	switch it.Kind {
	case core.DecisionAccept:
		in.Title, in.Ack = it.Title, it.Ack
	case core.DecisionStop:
		in.TicketID = it.TicketID
	case core.DecisionIgnore:
		in.IgnoredFromCard = it.IgnoredFromCard
	}
	res, err := h.eng.Decide(in)
	if err != nil {
		return nil, engineError(err)
	}
	if res.Refused != "" {
		return nil, errors.New(res.Refused)
	}
	r.Decision, r.Reason = res.Decision.Kind, reasonText(res.Decision.Reasons)
	if res.Decision.TicketID != "" {
		if c, err := h.card(res.Decision.TicketID); err == nil {
			r.Ticket = c
		}
	}
	return res.Decision.TriggerIDs, nil
}

// --- eval, eval_read, eval_decide ---

// window is the Thread window an assessment loaded, or why it could not.
type window struct {
	win sources.ContextWindow
	err string
}

// candidate returns the Eval in progress that holds an inferred Trigger.
func (h *Host) candidate(triggerID string) (core.CandidateView, *core.Trigger, error) {
	if err := required("trigger_id", triggerID); err != nil {
		return core.CandidateView{}, nil, err
	}
	v, err := h.eng.Candidate(triggerID)
	var ue *core.UserError
	if errors.As(err, &ue) {
		return core.CandidateView{}, nil, errors.New(msgFinished)
	}
	if err != nil {
		return core.CandidateView{}, nil, engineError(err)
	}
	if v.Eval.Strength == core.StrengthExplicit {
		return core.CandidateView{}, nil, errors.New(msgExplicit)
	}
	for i := range v.Triggers {
		if v.Triggers[i].ID == triggerID {
			return v, &v.Triggers[i], nil
		}
	}
	return core.CandidateView{}, nil, errors.New(msgFinished)
}

// window loads, once per Eval, the Thread window around the Trigger from its Source.
func (h *Host) window(ctx context.Context, v core.CandidateView, triggerID string) window {
	h.winMu.Lock()
	w, ok := h.windows[v.Eval.ID]
	h.winMu.Unlock()
	if ok {
		return w
	}
	tc, isContext := h.reg.Source(v.Thread.Source).(sources.ThreadContext)
	if !isContext {
		return window{err: msgNoContext}
	}
	cctx, cancel := context.WithTimeout(ctx, contextTimeout)
	defer cancel()
	win, err := tc.Context(cctx, v.Thread.Key, triggerID)
	if err != nil {
		// Not cached: the next call tries again.
		return window{err: "Thread context unavailable: " + cut(err.Error(), 300) + ". Assess from the trigger and batch, or try eval again."}
	}
	w = window{win: win}
	h.winMu.Lock()
	if len(h.windows) >= maxWindows {
		clear(h.windows)
	}
	h.windows[v.Eval.ID] = w
	h.winMu.Unlock()
	return w
}

func (h *Host) dropWindow(evalID string) {
	h.winMu.Lock()
	delete(h.windows, evalID)
	h.winMu.Unlock()
}

type windowMsg struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	At      string `json:"at,omitempty"`
	Trigger bool   `json:"trigger,omitempty"`
	Text    string `json:"text,omitempty"`
	Chars   int    `json:"chars,omitempty"`
}

type windowStub struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	Preview string `json:"preview"`
}

type ruleLine struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
}

// settings returns the current Policy settings, or none without a Policy.
func (h *Host) settings() (policy.Settings, error) {
	if h.cfg.Policy == nil {
		return policy.Settings{}, nil
	}
	s, err := h.cfg.Policy()
	if err != nil {
		return policy.Settings{}, fmt.Errorf("the Policy cannot be read now: %v; retry", err)
	}
	return s, nil
}

// eval returns an inferred candidate's assessment context: context, not instructions (admission.md holds those).
func (h *Host) eval(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		TriggerID string `json:"trigger_id"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	v, tr, err := h.candidate(args.TriggerID)
	if err != nil {
		return nil, err
	}
	s, err := h.settings()
	if err != nil {
		return nil, err
	}
	rules := []ruleLine{}
	for _, r := range s.Rules {
		if r.Enabled {
			rules = append(rules, ruleLine{r.Name, r.Instructions})
		}
	}
	type batchLine struct {
		TriggerID string `json:"trigger_id"`
		From      string `json:"from"`
		At        string `json:"at"`
		cutText
		Read string `json:"read,omitempty"`
	}
	var batch []batchLine
	for _, o := range v.Triggers {
		if o.ID == tr.ID {
			continue
		}
		l := batchLine{TriggerID: o.ID, From: o.Actor.ID, At: o.At.UTC().Format(time.RFC3339), cutText: cutTo(o.Text, batchChars)}
		if l.Chars > 0 {
			l.Read = "trigger/" + o.ID
		}
		batch = append(batch, l)
	}
	type threadPart struct {
		Messages []windowMsg  `json:"messages,omitempty"`
		Stubs    []windowStub `json:"stubs,omitempty"`
		Error    string       `json:"error,omitempty"`
	}
	w := h.window(ctx, v, tr.ID)
	thread := threadPart{Error: w.err}
	for _, m := range w.win.Messages {
		if m.ID == tr.ID {
			thread.Messages = append(thread.Messages, windowMsg{ID: m.ID, From: m.From, At: m.At, Trigger: true})
			continue
		}
		thread.Messages = append(thread.Messages, windowMsg{ID: m.ID, From: m.From, At: m.At, Text: m.Text, Chars: m.Chars})
	}
	for _, st := range w.win.Stubs {
		thread.Stubs = append(thread.Stubs, windowStub(st))
	}
	cards, total := related(v.Tickets)
	text := cutTo(tr.Text, triggerChars)
	handed := v.HandedBack
	if len(handed) == 0 {
		handed = nil
	}
	return struct {
		TriggerID string `json:"trigger_id"`
		Strength  string `json:"strength"`
		Mode      string `json:"mode"`
		Source    struct {
			Kind   string `json:"kind"`
			Actor  string `json:"actor"`
			Thread string `json:"thread"`
		} `json:"source"`
		cutText
		Batch          []batchLine       `json:"batch,omitempty"`
		RelatedTickets []ticketCard      `json:"related_tickets"`
		TicketsTotal   int               `json:"tickets_total"`
		HandedBack     map[string]string `json:"handed_back,omitempty"`
		PolicyVersion  string            `json:"policy_version"`
		Rules          []ruleLine        `json:"rules"`
		Thread         threadPart        `json:"thread"`
	}{TriggerID: tr.ID, Strength: v.Eval.Strength, Mode: s.Mode,
		Source: struct {
			Kind   string `json:"kind"`
			Actor  string `json:"actor"`
			Thread string `json:"thread"`
		}{tr.Source, tr.Actor.ID, v.Thread.Key},
		cutText: text, Batch: batch, RelatedTickets: cards, TicketsTotal: total, HandedBack: handed,
		PolicyVersion: v.Eval.PolicyVersion, Rules: rules, Thread: thread}, nil
}

// evalRead returns whole the messages of a candidate's window and batch, by id.
func (h *Host) evalRead(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		TriggerID string   `json:"trigger_id"`
		IDs       []string `json:"ids"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if n := len(args.IDs); n < 1 || n > maxReadIDs {
		return nil, fmt.Errorf("ids holds 1 to %d message ids, not %d", maxReadIDs, n)
	}
	v, tr, err := h.candidate(args.TriggerID)
	if err != nil {
		return nil, err
	}
	w := h.window(ctx, v, tr.ID)
	out := make([]readMsg, 0, len(args.IDs))
	for _, id := range args.IDs {
		out = append(out, readOne(id, w.win, v.Triggers, h.eng.Trigger))
	}
	return struct {
		TriggerID string    `json:"trigger_id"`
		Messages  []readMsg `json:"messages"`
	}{tr.ID, out}, nil
}

// readMsg is one message eval_read returns, or why it cannot.
type readMsg struct {
	ID    string `json:"id"`
	From  string `json:"from,omitempty"`
	Text  string `json:"text,omitempty"`
	Chars int    `json:"chars,omitempty"`
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

// readOne reads one message of a candidate's window or batch: a stored Trigger whole, else what the window has of it.
func readOne(id string, win sources.ContextWindow, batch []core.Trigger, lookup func(string) (core.Trigger, error)) readMsg {
	var msg *sources.Msg
	var stub *sources.Stub
	for i := range win.Messages {
		if win.Messages[i].ID == id {
			msg = &win.Messages[i]
		}
	}
	for i := range win.Stubs {
		if win.Stubs[i].ID == id {
			stub = &win.Stubs[i]
		}
	}
	inBatch := slices.ContainsFunc(batch, func(t core.Trigger) bool { return t.ID == id })
	if msg == nil && stub == nil && !inBatch {
		return readMsg{ID: id, Error: "not a message of this candidate's window or batch"}
	}
	t, err := lookup(id)
	switch {
	case err == nil:
		c := cutTo(t.Text, readChars)
		return readMsg{ID: id, From: t.Actor.ID, Text: c.Text, Chars: c.Chars}
	case msg != nil:
		r := readMsg{ID: id, From: msg.From, Text: msg.Text, Chars: msg.Chars}
		if msg.Chars > 0 {
			r.Note = "Only the window's cut of this message is available."
		}
		return r
	case stub != nil:
		return readMsg{ID: id, From: stub.From, Text: stub.Preview, Note: "Only the window's preview of this message is available."}
	}
	return readMsg{ID: id, Error: "this message cannot be read now: " + cut(err.Error(), 200) + "; retry"}
}

// evalDecide records an assessment: the Policy turns it into the Decision (observe mode records would-accept).
func (h *Host) evalDecide(raw json.RawMessage) (any, error) {
	var args struct {
		TriggerID string               `json:"trigger_id"`
		Rules     []core.RuleJudgement `json:"rules"`
		Accept    bool                 `json:"accept"`
		Reason    string               `json:"reason"`
		Title     string               `json:"title"`
		Ack       string               `json:"ack"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("reason", args.Reason); err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(args.Reason) > reasonMax {
		return nil, fmt.Errorf("reason: at most %d characters", reasonMax)
	}
	if args.Accept {
		if strings.TrimSpace(args.Title) == "" {
			return nil, errors.New("an accept needs a title")
		}
		if utf8.RuneCountInString(args.Title) > titleMax {
			return nil, fmt.Errorf("title: at most %d characters", titleMax)
		}
	}
	v, tr, err := h.candidate(args.TriggerID)
	if err != nil {
		return nil, err
	}
	a := core.Assessment{Rules: args.Rules, Accept: args.Accept, Reason: args.Reason}
	if a.Rules == nil {
		a.Rules = []core.RuleJudgement{}
	}
	// Check the judgements first, so a malformed assessment records nothing and can be corrected.
	s, err := h.settings()
	if err != nil {
		return nil, err
	}
	if _, _, _, err := policy.AssessmentOutcome(a, s, v.Eval.Strength); err != nil {
		return nil, errors.New(strings.TrimPrefix(err.Error(), "policy: "))
	}
	in := core.DecisionInput{TriggerID: tr.ID, Kind: core.DecisionAccept, Reason: args.Reason, Assessment: &a,
		Origin: core.OriginRouter}
	if args.Accept {
		in.Title, in.Ack = args.Title, args.Ack
	}
	res, err := h.eng.Decide(in)
	if err != nil {
		return nil, engineError(err)
	}
	h.dropWindow(v.Eval.ID)
	out := struct {
		TriggerID   string      `json:"trigger_id"`
		Decision    string      `json:"decision,omitempty"`
		Reason      string      `json:"reason,omitempty"`
		WouldAccept bool        `json:"would_accept,omitempty"`
		Ticket      *ticketCard `json:"ticket,omitempty"`
		Refused     string      `json:"refused,omitempty"`
	}{TriggerID: tr.ID, Refused: res.Refused}
	if res.Refused == "" {
		out.Decision, out.Reason, out.WouldAccept = res.Decision.Kind, reasonText(res.Decision.Reasons), res.Decision.WouldAccept
		if res.Decision.TicketID != "" {
			out.Ticket, _ = h.card(res.Decision.TicketID)
		}
	}
	return out, nil
}

// --- read ---

// read returns one page of a resource: a list of cards, or a text page of one record.
func (h *Host) read(raw json.RawMessage) (any, error) {
	var args struct {
		Resource string `json:"resource"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if args.Offset < 0 || args.Limit < 0 {
		return nil, errors.New("offset and limit are not negative")
	}
	list := func() (int, error) {
		switch {
		case args.Limit == 0:
			return pageItems, nil
		case args.Limit > maxPageItems:
			return 0, fmt.Errorf("a list page holds at most %d items", maxPageItems)
		}
		return args.Limit, nil
	}
	text := func(v any) (any, error) {
		limit := args.Limit
		switch {
		case limit == 0:
			limit = pageChars
		case limit > maxPageChars:
			return nil, fmt.Errorf("a text page holds at most %d characters", maxPageChars)
		}
		body, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return pageOf(args.Resource, string(body), args.Offset, limit), nil
	}
	kind, id, _ := strings.Cut(args.Resource, "/")
	switch {
	case args.Resource == "tickets":
		limit, err := list()
		if err != nil {
			return nil, err
		}
		all, err := h.eng.Tickets(true)
		if err != nil {
			return nil, engineError(err)
		}
		slices.SortStableFunc(all, func(a, b core.Ticket) int {
			if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(b.ID, a.ID)
		})
		cards := make([]ticketCard, len(all))
		for i, t := range all {
			cards[i] = cardOf(t)
		}
		return listOf(cards, args.Offset, limit), nil
	case args.Resource == "decisions":
		limit, err := list()
		if err != nil {
			return nil, err
		}
		ds, err := h.eng.Decisions("", args.Offset+limit+1)
		if err != nil {
			return nil, engineError(err)
		}
		cards := make([]decisionCard, len(ds))
		for i, d := range ds {
			cards[i] = decisionOf(d)
		}
		return listOf(cards, args.Offset, limit), nil
	case kind == "ticket" && id != "":
		v, err := h.ticketDetails(id)
		if err != nil {
			return nil, err
		}
		return text(v)
	case kind == "thread" && id != "":
		v, err := h.threadDetails(id)
		if err != nil {
			return nil, err
		}
		return text(v)
	case kind == "trigger" && id != "":
		v, err := h.triggerDetails(id)
		if err != nil {
			return nil, err
		}
		return text(v)
	}
	return nil, fmt.Errorf("unknown resource %q: tickets, ticket/<id>, thread/<key>, trigger/<id> or decisions", args.Resource)
}

const (
	detailAssignments = 3
	detailInputs      = 10
	inputChars        = 80
)

func (h *Host) ticketDetails(id string) (any, error) {
	t, _, err := h.eng.Ticket(id)
	if err != nil {
		return nil, engineError(err)
	}
	type reportLine struct {
		State string    `json:"state"`
		At    time.Time `json:"at"`
		cutText
	}
	type assignmentLine struct {
		ID          string      `json:"id"`
		State       string      `json:"state"`
		At          time.Time   `json:"at"`
		EndedReason string      `json:"ended_reason,omitempty"`
		Report      *reportLine `json:"report,omitempty"`
	}
	type inputLine struct {
		Kind        string    `json:"kind"`
		At          time.Time `json:"at"`
		TriggerID   string    `json:"trigger_id,omitempty"`
		Text        string    `json:"text,omitempty"`
		Chars       int       `json:"chars,omitempty"`
		Note        string    `json:"note,omitempty"`
		Context     bool      `json:"context,omitempty"`
		Undelivered bool      `json:"undelivered,omitempty"`
	}
	type threadLine struct {
		Thread string `json:"thread"`
		Own    bool   `json:"own,omitempty"`
		Bound  bool   `json:"bound,omitempty"`
		Closed string `json:"closed,omitempty"`
	}
	var assignments []assignmentLine
	for i := len(t.Assignments) - 1; i >= 0 && len(assignments) < detailAssignments; i-- {
		a := t.Assignments[i]
		l := assignmentLine{ID: a.ID, State: string(a.State), At: a.CreatedAt, EndedReason: a.EndedReason}
		if r := a.Report; r != nil {
			l.Report = &reportLine{State: string(r.State), At: r.At, cutText: cutTo(r.Summary, relayChars)}
		}
		assignments = append(assignments, l)
	}
	var inputs []inputLine
	for i := len(t.Inputs) - 1; i >= 0 && len(inputs) < detailInputs; i-- {
		in := t.Inputs[i]
		l := inputLine{Kind: string(in.Kind), At: in.At, TriggerID: in.TriggerID, Note: in.Note, Context: in.Context,
			Undelivered: slices.Contains(t.Undelivered, in.ID)}
		if in.Kind != core.InputTrigger {
			c := cutTo(in.Text, inputChars)
			l.Text, l.Chars = c.Text, c.Chars
		}
		inputs = append(inputs, l)
	}
	threads := []threadLine{}
	if m := t.Mandate.Thread; m != "" {
		threads = append(threads, threadLine{Thread: m, Own: true})
	}
	for _, f := range t.Follows {
		l := threadLine{Thread: f.Thread, Own: f.Own, Bound: true}
		if f.Closed != nil {
			l.Closed = string(f.Closed.Outcome)
		}
		if i := slices.IndexFunc(threads, func(x threadLine) bool { return x.Thread == f.Thread }); i >= 0 {
			threads[i] = l
		} else {
			threads = append(threads, l)
		}
	}
	var result string
	if r := latestReport(&t); r != nil {
		result = r.Summary
	}
	return struct {
		ticketCard
		TriggerID   string           `json:"trigger_id"`
		Batch       []string         `json:"batch,omitempty"`
		DecisionID  string           `json:"decision_id,omitempty"`
		Repo        string           `json:"repo,omitempty"`
		RequestedBy string           `json:"requested_by"`
		Via         string           `json:"via,omitempty"`
		Rules       []string         `json:"rules,omitempty"`
		Threads     []threadLine     `json:"threads"`
		Result      string           `json:"result,omitempty"`
		Assignments []assignmentLine `json:"assignments"`
		Inputs      []inputLine      `json:"inputs"`
	}{brief(t), t.Origin.TriggerID, t.Origin.TriggerIDs, t.DecisionID, t.Repo, t.Mandate.Actor.ID, t.Mandate.Basis.Via,
		t.Mandate.Basis.Rules, threads, result, assignments, inputs}, nil
}

func (h *Host) threadDetails(key string) (any, error) {
	th, err := h.eng.Thread(key)
	if err != nil {
		return nil, engineError(err)
	}
	type ticketLine struct {
		ticketCard
		Own     bool `json:"own,omitempty"`
		Context bool `json:"context,omitempty"`
		Bound   bool `json:"bound,omitempty"`
	}
	tickets := []ticketLine{}
	for _, s := range th.Subscriptions {
		t, _, err := h.eng.Ticket(s.TicketID)
		if err != nil {
			continue
		}
		tickets = append(tickets, ticketLine{cardOf(t), s.Own, s.Context, s.Bound})
	}
	type evaluating struct {
		Strength    string     `json:"strength"`
		Since       time.Time  `json:"since"`
		DeliveredAt *time.Time `json:"delivered_at,omitempty"`
		TriggerIDs  []string   `json:"trigger_ids"`
	}
	var ev *evaluating
	if e := th.Eval; e != nil {
		ev = &evaluating{Strength: e.Strength, Since: e.StartedAt, DeliveredAt: e.DeliveredAt, TriggerIDs: e.TriggerIDs}
	}
	ds, err := h.eng.Decisions(key, 5)
	if err != nil {
		return nil, engineError(err)
	}
	decisions := make([]decisionCard, len(ds))
	for i, d := range ds {
		decisions[i] = decisionOf(d)
	}
	closed := ""
	if th.Closed != nil {
		closed = string(th.Closed.Outcome)
	}
	return struct {
		Thread     string         `json:"thread"`
		Source     string         `json:"source"`
		Label      string         `json:"label,omitempty"`
		Closed     string         `json:"closed,omitempty"`
		Tickets    []ticketLine   `json:"tickets"`
		Evaluating *evaluating    `json:"evaluating,omitempty"`
		Queued     []string       `json:"queued,omitempty"`
		Decisions  []decisionCard `json:"decisions"`
	}{th.Key, th.Source, th.Label, closed, tickets, ev, th.Queued, decisions}, nil
}

func (h *Host) triggerDetails(id string) (any, error) {
	tr, err := h.eng.Trigger(id)
	if err != nil {
		return nil, engineError(err)
	}
	var replyTo *triggerLine
	if tr.ReplyTo != "" {
		if p, err := h.eng.Trigger(tr.ReplyTo); err == nil {
			l := lineOf(p, previewChars)
			replyTo = &l
		}
	}
	d := tr.Disposition
	type disposition struct {
		Filtered     string            `json:"filtered,omitempty"`
		DeliveredTo  []string          `json:"delivered_to,omitempty"`
		Context      []string          `json:"context,omitempty"`
		Attached     []string          `json:"attached,omitempty"`
		HandedBack   map[string]string `json:"handed_back,omitempty"`
		Acknowledged []string          `json:"acknowledged,omitempty"`
		Requeued     *time.Time        `json:"requeued,omitempty"`
	}
	return struct {
		TriggerID   string       `json:"trigger_id"`
		Source      string       `json:"source"`
		From        string       `json:"from"`
		Human       bool         `json:"human,omitempty"`
		Bot         bool         `json:"bot,omitempty"`
		Kind        string       `json:"kind,omitempty"`
		At          time.Time    `json:"at"`
		Threads     []string     `json:"threads"`
		Text        string       `json:"text"`
		RepliesTo   *triggerLine `json:"replies_to,omitempty"`
		Disposition disposition  `json:"disposition"`
	}{tr.ID, tr.Source, tr.Actor.ID, tr.Human, tr.Bot, tr.Kind, tr.At, tr.Threads, tr.Text, replyTo,
		disposition{d.Filtered, d.Reached, d.Context, d.Attached, d.HandedBack, d.Acknowledged, d.Requeued}}, nil
}

// --- Ticket tools ---

func (h *Host) reply(raw json.RawMessage) (any, error) {
	var args struct {
		TicketID  string `json:"ticket_id"`
		TriggerID string `json:"trigger_id"`
		Purpose   string `json:"purpose"`
		Text      string `json:"text"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	switch args.Purpose {
	case core.PurposeAnswer, core.PurposeResult, core.PurposeAttention:
	case core.PurposeAcknowledgement:
		return nil, errors.New("the ticket's worker acknowledges the requests it takes up; the router replies with answer, result or attention")
	default:
		return nil, fmt.Errorf("unknown purpose %q (answer, result or attention)", args.Purpose)
	}
	if err := required("ticket_id", args.TicketID); err != nil {
		return nil, err
	}
	id, err := h.eng.Reply(core.ReplyRequest{TicketID: args.TicketID, TriggerID: args.TriggerID, Purpose: args.Purpose,
		Text: args.Text})
	if err != nil {
		return nil, engineError(err)
	}
	return struct {
		ReceiptID string `json:"receipt_id"`
		Status    string `json:"status"`
	}{id, "queued"}, nil
}

type ticketArgs struct {
	TicketID string `json:"ticket_id"`
}

func (h *Host) ticketID(raw json.RawMessage) (string, error) {
	var args ticketArgs
	if err := decode(raw, &args); err != nil {
		return "", err
	}
	return args.TicketID, required("ticket_id", args.TicketID)
}

type ticketResult struct {
	Reason string      `json:"reason"`
	Asked  *bool       `json:"asked,omitempty"`
	Ticket *ticketCard `json:"ticket,omitempty"`
}

func (h *Host) note(raw json.RawMessage) (any, error) {
	var args struct {
		TicketID string `json:"ticket_id"`
		Text     string `json:"text"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if n := len(strings.Fields(args.Text)); n > noteMax {
		return nil, fmt.Errorf("a note is at most %d words, not %d", noteMax, n)
	}
	if err := h.eng.Note(args.TicketID, args.Text); err != nil {
		return nil, engineError(err)
	}
	c, err := h.card(args.TicketID)
	return ticketResult{Reason: "Noted for the worker", Ticket: c}, err
}

func (h *Host) requestStatus(raw json.RawMessage) (any, error) {
	id, err := h.ticketID(raw)
	if err != nil {
		return nil, err
	}
	asked, err := h.eng.RequestStatus(id)
	if err != nil {
		return nil, engineError(err)
	}
	reason := "The worker is not waiting; nothing was sent"
	if asked {
		reason = "Asked the worker for a status report"
	}
	c, err := h.card(id)
	return ticketResult{Reason: reason, Asked: &asked, Ticket: c}, err
}

func (h *Host) ticketRetry(raw json.RawMessage) (any, error) {
	id, err := h.ticketID(raw)
	if err != nil {
		return nil, err
	}
	if err := h.eng.RetryTicket(id); err != nil {
		return nil, engineError(err)
	}
	c, err := h.card(id)
	return ticketResult{Reason: "Started a new assignment", Ticket: c}, err
}

func (h *Host) triggerRequeue(raw json.RawMessage) (any, error) {
	var args struct {
		TriggerID string `json:"trigger_id"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("trigger_id", args.TriggerID); err != nil {
		return nil, err
	}
	if err := h.eng.Reevaluate(args.TriggerID, core.OriginRouter); err != nil {
		return nil, engineError(err)
	}
	return struct {
		TriggerID string `json:"trigger_id"`
		Queued    bool   `json:"queued"`
	}{args.TriggerID, true}, nil
}

func (h *Host) threadUnsubscribe(raw json.RawMessage) (any, error) {
	var args struct {
		Thread string `json:"thread"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := h.eng.Unsubscribe(args.Thread, core.OriginRouter); err != nil {
		return nil, engineError(err)
	}
	return struct {
		Thread       string `json:"thread"`
		Unsubscribed bool   `json:"unsubscribed"`
	}{args.Thread, true}, nil
}
