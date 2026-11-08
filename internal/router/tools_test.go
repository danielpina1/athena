package router

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/sources"
)

// callSocket sends one request line to a socket and decodes one response line.
func callSocket(socket string, req, resp any) error {
	c, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	line, _ := json.Marshal(req)
	if _, err := c.Write(append(line, '\n')); err != nil {
		return err
	}
	return json.NewDecoder(c).Decode(resp)
}

// call runs a router tool and decodes its result into out; it fails the test on a tool error.
func (e *env) call(tool string, args any, out any) {
	e.t.Helper()
	if err := e.tryCall(tool, args, out); err != nil {
		e.t.Fatalf("%s: %v", tool, err)
	}
}

func (e *env) tryCall(tool string, args any, out any) error {
	e.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.host.CallTool(context.Background(), tool, raw)
	if err != nil {
		return err
	}
	b, err := json.Marshal(res)
	if err != nil {
		e.t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s result %s: %v", tool, b, err)
		}
	}
	return nil
}

type routeResults struct {
	Results []struct {
		TriggerID string `json:"trigger_id"`
		Kind      string `json:"kind"`
		Decision  string `json:"decision"`
		Reason    string `json:"reason"`
		Refused   string `json:"refused"`
		Ticket    *struct {
			TicketID string `json:"ticket_id"`
			Title    string `json:"title"`
			State    string `json:"state"`
		} `json:"ticket"`
	} `json:"results"`
}

func TestRouteBatchIndependentItems(t *testing.T) {
	e := newEnv(t)
	a, c := "github:issuecomment:1", "github:issuecomment:3"
	e.receive(explicit(a, "github:o/r#1"))
	e.receive(explicit(c, "github:o/r#3"))
	e.must(e.eng.EvaluateDue())
	var got routeResults
	e.call("route", map[string]any{"decisions": []map[string]any{
		{"trigger_id": a, "kind": "accept", "title": "Fix the label", "ack": "On it: fixing the label.", "reason": "clear"},
		{"trigger_id": a, "kind": "ignore", "reason": "second thoughts"},
		{"trigger_id": c, "kind": "ignore", "reason": "chatter"},
	}}, &got)
	if len(got.Results) != 3 {
		t.Fatalf("results = %+v", got)
	}
	r1, r2, r3 := got.Results[0], got.Results[1], got.Results[2]
	if r1.TriggerID != a || r1.Kind != "accept" || r1.Decision != "accept" || r1.Ticket == nil || r1.Ticket.Title != "Fix the label" ||
		r1.Refused != "" {
		t.Fatalf("item 1 = %+v", r1)
	}
	if r2.TriggerID != a || r2.Kind != "ignore" || r2.Refused == "" || r2.Decision != "" {
		t.Fatalf("item 2 = %+v, want refused", r2)
	}
	if r3.TriggerID != c || r3.Decision != "ignore" || r3.Refused != "" || !strings.Contains(r3.Reason, "chatter") {
		t.Fatalf("item 3 = %+v", r3)
	}
	tk, _, err := e.eng.Ticket(r1.Ticket.TicketID)
	e.must(err)
	if tk.Title != "Fix the label" || tk.Ended != nil {
		t.Fatalf("ticket = %+v", tk)
	}

	// Malformed items are refused alone; a call needs 1 to 20 items.
	e.receive(explicit("github:issuecomment:5", "github:o/r#5"))
	e.must(e.eng.EvaluateDue())
	e.call("route", map[string]any{"decisions": []map[string]any{
		{"trigger_id": "github:issuecomment:5", "kind": "accept"},
		{"trigger_id": "github:issuecomment:404", "kind": "ignore", "reason": "x"},
		{"trigger_id": "github:issuecomment:5", "kind": "stop", "reason": "x"},
		{"trigger_id": "github:issuecomment:5", "kind": "attach", "ticket_id": tk.ID},
	}}, &got)
	for i, r := range got.Results {
		if i < 3 && r.Refused == "" {
			t.Errorf("item %d = %+v, want refused", i+1, r)
		}
	}
	if r := got.Results[3]; r.Refused != "" || r.Ticket == nil || r.Ticket.TicketID != tk.ID {
		t.Fatalf("attach = %+v", r)
	}
	if err := e.tryCall("route", map[string]any{"decisions": []any{}}, nil); err == nil {
		t.Error("an empty route was accepted")
	}
	many := make([]map[string]any, 21)
	for i := range many {
		many[i] = map[string]any{"trigger_id": a, "kind": "ignore", "reason": "x"}
	}
	if err := e.tryCall("route", map[string]any{"decisions": many}, nil); err == nil {
		t.Error("21 items were accepted")
	}
	if err := e.tryCall("route", map[string]any{"decisions": []map[string]any{{"trigger_id": a, "kind": "ignore", "bogus": 1}}}, nil); err == nil {
		t.Error("an unknown field was accepted")
	}
}

func TestEvalDecideObserveMode(t *testing.T) {
	e := newEnv(t)
	id, thread := "github:issuecomment:2", "github:o/r#2"
	e.receive(inferred(id, thread))
	e.must(e.eng.EvaluateDue())
	var ctx struct {
		TriggerID      string `json:"trigger_id"`
		Strength       string `json:"strength"`
		Mode           string `json:"mode"`
		Text           string `json:"text"`
		PolicyVersion  string `json:"policy_version"`
		TicketsTotal   int    `json:"tickets_total"`
		RelatedTickets []any  `json:"related_tickets"`
		Rules          []struct {
			Name         string `json:"name"`
			Instructions string `json:"instructions"`
		} `json:"rules"`
		Thread struct {
			Messages []struct {
				ID      string `json:"id"`
				Text    string `json:"text"`
				Chars   int    `json:"chars"`
				Trigger bool   `json:"trigger"`
			} `json:"messages"`
			Stubs []struct {
				ID      string `json:"id"`
				Preview string `json:"preview"`
			} `json:"stubs"`
		} `json:"thread"`
	}
	e.call("eval", map[string]any{"trigger_id": id}, &ctx)
	if ctx.TriggerID != id || ctx.Strength != "inferred" || ctx.Mode != policy.ModeObserve || ctx.Text != "just fix the typo" {
		t.Fatalf("eval = %+v", ctx)
	}
	if ctx.PolicyVersion != policy.Version(e.settings) {
		t.Fatalf("policy version = %q, want %q", ctx.PolicyVersion, policy.Version(e.settings))
	}
	if len(ctx.Rules) != 1 || ctx.Rules[0].Name != "copy-fixes" || ctx.Rules[0].Instructions == "" {
		t.Fatalf("rules = %+v, want the enabled rule only", ctx.Rules)
	}
	if len(ctx.Thread.Messages) != 3 || !ctx.Thread.Messages[1].Trigger || ctx.Thread.Messages[2].Chars != 900 ||
		len(ctx.Thread.Stubs) != 1 {
		t.Fatalf("thread window = %+v", ctx.Thread)
	}

	var read struct {
		Messages []struct {
			ID    string `json:"id"`
			Text  string `json:"text"`
			Error string `json:"error"`
		} `json:"messages"`
	}
	e.call("eval_read", map[string]any{"trigger_id": id, "ids": []string{"github:issue:9", id, "github:issuecomment:404"}}, &read)
	if len(read.Messages) != 3 || read.Messages[0].Text == "" || read.Messages[1].Text != "just fix the typo" || read.Messages[2].Error == "" {
		t.Fatalf("eval_read = %+v", read)
	}

	// An assessment must judge every enabled rule.
	if err := e.tryCall("eval_decide", map[string]any{"trigger_id": id, "rules": []any{}, "accept": true,
		"reason": "typo", "title": "Fix the typo"}, nil); err == nil {
		t.Error("an assessment without judgements was recorded")
	}
	if err := e.tryCall("eval_decide", map[string]any{"trigger_id": id, "accept": true, "reason": "typo",
		"rules": []map[string]any{{"name": "copy-fixes", "matches": true, "evidence": "\"just fix the typo\""}}}, nil); err == nil {
		t.Error("an accept without a title was recorded")
	}

	var got struct {
		Decision    string `json:"decision"`
		WouldAccept bool   `json:"would_accept"`
		Reason      string `json:"reason"`
		Refused     string `json:"refused"`
		Ticket      any    `json:"ticket"`
	}
	e.call("eval_decide", map[string]any{"trigger_id": id, "accept": true, "reason": "a typo fix", "title": "Fix the typo",
		"ack":   "On it: fixing the typo.",
		"rules": []map[string]any{{"name": "copy-fixes", "matches": true, "evidence": "\"just fix the typo\""}}}, &got)
	if got.Decision != core.DecisionIgnore || !got.WouldAccept || got.Ticket != nil || got.Refused != "" {
		t.Fatalf("observe-mode decision = %+v", got)
	}
	ds, err := e.eng.Decisions(thread, 1)
	e.must(err)
	if len(ds) != 1 || !ds[0].WouldAccept || ds[0].Kind != core.DecisionIgnore {
		t.Fatalf("decision = %+v", ds)
	}
	tickets, err := e.eng.Tickets(true)
	e.must(err)
	if len(tickets) != 0 {
		t.Fatalf("observe mode started %d tickets", len(tickets))
	}
	if err := e.tryCall("eval", map[string]any{"trigger_id": id}, nil); err == nil {
		t.Error("eval of a decided candidate")
	}

	// An explicit candidate is routed, not assessed.
	e.receive(explicit("github:issuecomment:3", "github:o/r#3"))
	e.must(e.eng.EvaluateDue())
	if err := e.tryCall("eval", map[string]any{"trigger_id": "github:issuecomment:3"}, nil); err == nil ||
		!strings.Contains(err.Error(), "route") {
		t.Errorf("eval of an explicit candidate: %v", err)
	}
}

func TestReadResources(t *testing.T) {
	e := newEnv(t)
	tk := e.accept("github:issuecomment:1", "github:o/r#1")
	for i := 2; i <= 3; i++ {
		e.accept("github:issuecomment:"+string(rune('0'+i)), "github:o/r#"+string(rune('0'+i)))
	}
	var list struct {
		Items []struct {
			TicketID string `json:"ticket_id"`
			State    string `json:"state"`
			Read     string `json:"read"`
		} `json:"items"`
		NextOffset *int `json:"next_offset"`
	}
	e.call("read", map[string]any{"resource": "tickets", "limit": 2}, &list)
	if len(list.Items) != 2 || list.NextOffset == nil || *list.NextOffset != 2 || list.Items[0].State != "working/queued" {
		t.Fatalf("tickets page = %+v", list)
	}
	e.call("read", map[string]any{"resource": "tickets", "offset": 2, "limit": 2}, &list)
	if len(list.Items) != 1 || list.NextOffset != nil || list.Items[0].TicketID != tk.ID {
		t.Fatalf("second page = %+v", list)
	}
	var page struct {
		Resource   string `json:"resource"`
		TotalChars int    `json:"total_chars"`
		NextOffset *int   `json:"next_offset"`
		Text       string `json:"text"`
	}
	for _, res := range []string{"ticket/" + tk.ID, "thread/github:o/r#1", "trigger/github:issuecomment:1"} {
		e.call("read", map[string]any{"resource": res}, &page)
		if page.Resource != res || page.TotalChars == 0 || !json.Valid([]byte(page.Text)) {
			t.Fatalf("%s = %+v", res, page)
		}
	}
	e.call("read", map[string]any{"resource": "ticket/" + tk.ID, "limit": 10}, &page)
	if len(page.Text) != 10 || page.NextOffset == nil || *page.NextOffset != 10 {
		t.Fatalf("a short text page = %+v", page)
	}
	var ds struct {
		Items []struct {
			DecisionID string `json:"decision_id"`
			Kind       string `json:"kind"`
		} `json:"items"`
	}
	e.call("read", map[string]any{"resource": "decisions"}, &ds)
	if len(ds.Items) != 3 || ds.Items[0].Kind != "accept" {
		t.Fatalf("decisions = %+v", ds)
	}
	for _, bad := range []map[string]any{{"resource": "nope"}, {"resource": "ticket/T-9999"}, {"resource": "tickets", "limit": 51}} {
		if err := e.tryCall("read", bad, nil); err == nil {
			t.Errorf("read %v succeeded", bad)
		}
	}
}

func TestTicketTools(t *testing.T) {
	e := newEnv(t)
	tk := e.accept("github:issuecomment:1", "github:o/r#1")
	var out map[string]any
	e.call("note", map[string]any{"ticket_id": tk.ID, "text": "the other ticket renamed the file"}, &out)
	if err := e.tryCall("note", map[string]any{"ticket_id": tk.ID, "text": strings.Repeat("word ", 101)}, nil); err == nil {
		t.Error("a 101-word note was accepted")
	}
	e.call("request_status", map[string]any{"ticket_id": tk.ID}, &out)
	if out["asked"] != false {
		t.Fatalf("request_status of a queued ticket = %v", out)
	}
	if err := e.tryCall("ticket_retry", map[string]any{"ticket_id": tk.ID}, nil); err == nil {
		t.Error("retry of a ticket that is not stalled")
	}
	if err := e.tryCall("reply", map[string]any{"ticket_id": tk.ID, "purpose": "acknowledgement", "text": "hi",
		"trigger_id": "github:issuecomment:1"}, nil); err == nil {
		t.Error("the router acknowledged")
	}
	if err := e.tryCall("reply", map[string]any{"ticket_id": tk.ID, "purpose": "result", "text": "done"}, nil); err == nil {
		t.Error("a result without a ready report")
	}
	e.call("reply", map[string]any{"ticket_id": tk.ID, "purpose": "answer", "text": "yes", "trigger_id": "github:issuecomment:1"}, &out)
	if out["receipt_id"] == "" || out["status"] != "queued" {
		t.Fatalf("reply = %v", out)
	}
	e.call("trigger_requeue", map[string]any{"trigger_id": "github:issuecomment:1"}, &out)
	e.call("thread_unsubscribe", map[string]any{"thread": "github:o/r#1"}, &out)
	notices, err := e.fx.Pending(core.EffectNotice)
	e.must(err)
	if len(notices) != 0 {
		t.Fatalf("the router's own requeue and unsubscribe sent notices: %+v", notices)
	}
	if err := e.tryCall("nope", map[string]any{}, nil); err == nil {
		t.Error("an unknown tool ran")
	}
}

var _ node.RouterHandler = (*Host)(nil)

func TestReferencedTickets(t *testing.T) {
	e := newEnv(t)
	e.accept("github:issuecomment:1", "github:o/r#1")
	e.accept("github:issuecomment:2", "github:o/r#2")
	pr := e.accept("github:issuecomment:3", "github:o/r#12")
	if pr.ID != "T-0003" {
		t.Fatalf("ticket = %s", pr.ID)
	}
	tr := explicit("github:issuecomment:5", "github:o/r#5")
	tr.Text = "@athena the change in #12 broke this; see also https://github.com/O/R/pull/12 and other/repo#3"
	e.receive(tr)
	e.must(e.eng.EvaluateDue())
	v, err := e.eng.Candidate(tr.ID)
	e.must(err)
	raw, err := e.host.candidateCard(v)
	e.must(err)
	var card struct {
		Referenced []struct {
			TicketID string `json:"ticket_id"`
		} `json:"referenced_tickets"`
	}
	e.must(json.Unmarshal([]byte(raw), &card))
	if len(card.Referenced) != 1 || card.Referenced[0].TicketID != "T-0003" {
		t.Fatalf("referenced_tickets = %+v in %s", card.Referenced, raw)
	}
}

func TestEvalReadUnreadable(t *testing.T) {
	batch := []core.Trigger{{ID: "b:1"}}
	failing := func(string) (core.Trigger, error) { return core.Trigger{}, errors.New("disk on fire") }
	got := readOne("b:1", sources.ContextWindow{}, batch, failing)
	if got.Error == "" || got.Text != "" {
		t.Fatalf("read of an unreadable batch trigger = %+v, want an error entry", got)
	}
	got = readOne("x:9", sources.ContextWindow{}, batch, failing)
	if got.Error == "" {
		t.Fatalf("read of an unknown id = %+v", got)
	}
}

func TestEvalShowsNoRulesForAutopilot(t *testing.T) {
	e := newEnv(t)
	e.reg.Register(scheduleSource{})
	e.settings.Autopilot = []policy.AutopilotEntry{{Name: "Night", Instructions: "tidy", Repo: "o/r", Enabled: true, Cron: "0 3 * * *"}}
	id := "schedule:night:1"
	e.receive(core.Trigger{ID: id, Source: policy.ScheduleSource, Kind: core.KindSchedule, Scope: "schedule",
		Threads: []string{"schedule:night"}, At: time.Now(), Text: "tidy",
		Meta: map[string]string{"autopilot": "night", "repo": "o/r"}})
	e.must(e.eng.EvaluateDue())
	var ctx struct {
		Strength string `json:"strength"`
		Rules    []any  `json:"rules"`
	}
	e.call("eval", map[string]any{"trigger_id": id}, &ctx)
	if ctx.Strength != "inferred" || len(ctx.Rules) != 0 {
		t.Fatalf("eval = %+v, want an inferred candidate with no rules", ctx)
	}
}

// scheduleSource listens to every schedule Thread.
type scheduleSource struct{ fakeSource }

func (scheduleSource) Name() string { return "schedule" }
