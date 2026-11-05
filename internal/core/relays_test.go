package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// newRelayEvals is an evals harness whose Engine enqueues the main Session's relays and notices.
func newRelayEvals(t *testing.T) *evals {
	t.Helper()
	h := &evals{pol: newEvalPolicy()}
	in := &intake{scope: &fakeScope{deaf: map[string]bool{}}, policy: &fakePolicy{holds: true}}
	in.harness = newHarnessWith(t, func(o *Options) {
		o.Scope = in.scope
		o.Policy = func() Policy { return h.pol }
		o.Wake = func() { in.wakes.Add(1) }
		o.Relays = true
	})
	h.intake = in
	return h
}

// effectsOf returns the queued effects of a kind, in queue order.
func (h *harness) effectsOf(kind string) []fakeEffect {
	var out []fakeEffect
	for _, e := range h.repos.effectList() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func effectKeys(fx []fakeEffect) []string {
	var out []string
	for _, e := range fx {
		out = append(out, e.Key)
	}
	return out
}

func payloadOf(t *testing.T, e fakeEffect) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("payload of %s/%s: %v", e.Kind, e.Key, err)
	}
	return m
}

func TestFollowupRelayEnqueued(t *testing.T) {
	h := newRelayEvals(t)
	id := h.sourceTicket(issue)
	h.receive(h.message("c:1", issue))
	fx := h.effectsOf(EffectRelay)
	eq(t, "relay keys", effectKeys(fx), []string{"followup:c:1"})
	p := payloadOf(t, fx[0])
	eq(t, "kind", p["kind"], any(RelayFollowUp))
	eq(t, "trigger", p["triggerId"], any("c:1"))
	eq(t, "the follow-up reached its owner", h.repos.trigger("c:1").Disposition.Reached, []string{id})

	// A bot's message and a person's message no Ticket decides are no follow-ups.
	h.receive(h.botMessage("c:2", issue))
	h.receive(h.message("c:3", other))
	eq(t, "still one relay", effectKeys(h.effectsOf(EffectRelay)), []string{"followup:c:1"})
}

func TestReportRelays(t *testing.T) {
	h := newRelayEvals(t)
	id := h.sourceTicket(issue)
	tk := h.repos.ticket(id)
	a := tk.LatestAssignment().ID

	h.must(h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportProgress, Summary: "half way"}))
	eq(t, "progress stays internal", len(h.effectsOf(EffectRelay)), 0)

	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportBlocked, Summary: "need a key"}))
	at := h.repos.ticket(id).LatestAssignment().Report.At
	want := "report:" + id + ":" + a + ":" + at.UTC().Format(time.RFC3339Nano)
	eq(t, "report relay", effectKeys(h.effectsOf(EffectRelay)), []string{want})
	p := payloadOf(t, h.effectsOf(EffectRelay)[0])
	eq(t, "kind", p["kind"], any(RelayWorkerReport))
	eq(t, "ticket", p["ticketId"], any(id))

	// A question raises an Attention: the relay is the attention, not the report; the same question again adds none.
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportWaiting, Summary: "which?", Question: "red or blue?"}))
	att := h.repos.ticket(id).Attention
	if att == nil {
		t.Fatal("no attention")
	}
	keys := effectKeys(h.effectsOf(EffectRelay))
	eq(t, "attention relay", keys, []string{want, "attention:" + id + ":" + att.ID})
	h.tick(1)
	h.must(h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportWaiting, Summary: "still", Question: "red or blue?"}))
	eq(t, "same question, no new relay", len(h.effectsOf(EffectRelay)), 3)
	last := h.effectsOf(EffectRelay)[2]
	eq(t, "the repeat relays as a report", strings.HasPrefix(last.Key, "report:"), true)
}

func TestOwnerTicketsNotRelayed(t *testing.T) {
	h := newRelayEvals(t)
	id := h.accepted()
	a := h.repos.ticket(id).LatestAssignment().ID
	h.must(h.eng.FileReport(id, ReportInput{AssignmentID: a, State: ReportReady, Summary: "done"}))
	eq(t, "no relay", len(h.effectsOf(EffectRelay)), 0)
	eq(t, "no notice", len(h.effectsOf(EffectNotice)), 0)
}

func TestNoticesEnqueued(t *testing.T) {
	h := newRelayEvals(t)
	id := h.acceptRequest("c:1", issue)
	eq(t, "no notice yet", len(h.effectsOf(EffectNotice)), 0)

	// A person's requeue tells the router; the router's own does not.
	h.must(h.eng.Reevaluate("c:1"))
	fx := h.effectsOf(EffectNotice)
	if len(fx) != 1 || !strings.HasPrefix(fx[0].Key, "trigger_requeued:c:1:") {
		t.Fatalf("notices = %v", effectKeys(fx))
	}
	eq(t, "kind", payloadOf(t, fx[0])["kind"], any(NoticeTriggerRequeued))
	h.tick(1)
	h.must(h.eng.Reevaluate("c:1", OriginRouter))
	eq(t, "the router's requeue", len(h.effectsOf(EffectNotice)), 1)

	// Context follows and unrelated threads.
	h.must(h.eng.FollowForContext(id, other))
	h.must(h.eng.MarkUnrelated(id, other, "different bug"))
	keys := effectKeys(h.effectsOf(EffectNotice))
	eq(t, "follow and unrelated", keys[1:], []string{"thread_followed:" + id + ":" + other, "thread_unrelated:" + id + ":" + other})

	// A person's unsubscribe; the router's own is silent.
	h.must(h.eng.Unsubscribe(issue))
	keys = effectKeys(h.effectsOf(EffectNotice))
	eq(t, "unsubscribe", keys[len(keys)-1], "thread_unsubscribed:"+issue)
	n := len(keys)
	h.must(h.eng.FollowForContext(id, "github:o/r#5"))
	n++
	h.must(h.eng.Unsubscribe("github:o/r#5", OriginRouter))
	eq(t, "router unsubscribe", len(h.effectsOf(EffectNotice)), n)

	// A Ticket's end tells the router, unless the router's stop ended it.
	h.must(h.eng.StopTicket(id, "enough"))
	keys = effectKeys(h.effectsOf(EffectNotice))
	if last := keys[len(keys)-1]; !strings.HasPrefix(last, "ticket_ended:"+id+":") {
		t.Fatalf("last notice = %s", last)
	}
	n = len(keys)
	t2 := h.acceptRequest("c:7", "github:o/r#7")
	h.receive(h.request("c:8", "github:o/r#7"))
	h.due()
	r := h.decide(DecisionInput{TriggerID: "c:8", Kind: "stop", TicketID: t2, Reason: "asked", Origin: OriginRouter})
	eq(t, "stopped", r.Decision.Kind, "stop")
	eq(t, "no notice of the router's stop", len(h.effectsOf(EffectNotice)), n)
}

func TestRelaysOffByDefault(t *testing.T) {
	h := newEvals(t)
	id := h.sourceTicket(issue)
	h.receive(h.message("c:1", issue))
	h.must(h.eng.StopTicket(id, "enough"))
	eq(t, "no relay", len(h.effectsOf(EffectRelay)), 0)
	eq(t, "no notice", len(h.effectsOf(EffectNotice)), 0)
}

func TestRouterRequeueRefusesWorkerFollowup(t *testing.T) {
	h := newRelayEvals(t)
	h.sourceTicket(issue)
	h.receive(h.message("c:1", issue))
	err := h.eng.Reevaluate("c:1", OriginRouter)
	if err == nil || !strings.Contains(err.Error(), CodeWorkerDecides) {
		t.Fatalf("router requeue of a worker's follow-up: %v", err)
	}
	h.must(h.eng.Reevaluate("c:1")) // a person may
}

func TestAssessedIgnoreNeedsNoTitle(t *testing.T) {
	h := newEvals(t)
	h.receive(h.botMessage("c:1", issue))
	h.due()
	r := h.decide(DecisionInput{TriggerID: "c:1", Kind: "accept", Reason: "no rule",
		Assessment: &Assessment{Accept: false, Reason: "no rule"}})
	eq(t, "refused", r.Refused, "")
	eq(t, "kind", r.Decision.Kind, "ignore")
	r = h.decide(DecisionInput{TriggerID: "c:1", Kind: "accept"})
	eq(t, "a decided trigger", r.Refused != "" || r.Decision.Kind != "", true)

	// An accept still needs its title.
	h.receive(h.botMessage("c:2", issue))
	h.due()
	h.pol.mode = "auto"
	r = h.decide(DecisionInput{TriggerID: "c:2", Kind: "accept",
		Assessment: &Assessment{Accept: true, Rules: []RuleJudgement{{Name: "r", Matches: true, Evidence: "x"}}}})
	eq(t, "title", strings.HasPrefix(r.Refused, CodeTitleRequired), true)
}

func TestCoreQueries(t *testing.T) {
	h := newEvals(t)
	id := h.acceptRequest("c:1", issue)
	tr, err := h.eng.Trigger("c:1")
	h.must(err)
	eq(t, "trigger", tr.ID, "c:1")
	var ue *UserError
	if _, err := h.eng.Trigger("nope"); !errors.Is(err, ErrNotFound) || !errors.As(err, &ue) {
		t.Errorf("unknown trigger: %v, want a UserError that is ErrNotFound", err)
	}
	th, err := h.eng.Thread(issue)
	h.must(err)
	eq(t, "thread subscriptions", th.Subscriptions[0].TicketID, id)
	ds, err := h.eng.Decisions("", 10)
	h.must(err)
	eq(t, "all decisions", len(ds), 1)
	ds, err = h.eng.Decisions(issue, 10)
	h.must(err)
	eq(t, "thread decisions", ds[0].TicketID, id)
}
