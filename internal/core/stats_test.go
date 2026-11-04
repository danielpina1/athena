package core

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/metrics"
)

func TestTicketStatsAccumulate(t *testing.T) {
	h := newHarness(t)
	tk := h.submit("Make it blue")
	first := tk.Assignments[0].ID

	h.tick(2)
	h.must(h.eng.AssignmentStarted(tk.ID, first, "sess-1", ""))
	h.tick(3)
	h.must(h.eng.FileReport(tk.ID, ReportInput{State: ReportProgress, Summary: "looking"}))
	h.tick(5)
	// A question for the person: the wait starts here.
	h.must(h.eng.FileReport(tk.ID, ReportInput{State: ReportWaiting, Summary: "need a color", Question: "which color?"}))
	h.tick(30)
	h.must(h.eng.AddPersonInput(tk.ID, "blue"))

	// An Approval waits 7 s for its answer.
	h.tick(1)
	ap, err := h.eng.RequestApproval(Approval{TicketID: tk.ID, AssignmentID: first, Tool: "Bash", Summary: "rm -rf x"})
	if err != nil {
		t.Fatal(err)
	}
	h.tick(7)
	h.must(h.eng.AnswerApproval(ap.ID, true, ""))

	// What the node saw: two Results, a crash, hook denials.
	h.must(h.eng.RecordUsage(tk.ID, first, UsageDelta{
		TimeToFirstOutput: 1500 * time.Millisecond, Turns: 2, ToolCalls: 5, ToolErrors: 1, HookDenials: 1,
		InputTokens: 100, OutputTokens: 50, CacheReadTokens: 300, CostUSD: 0.02, ActiveTime: 4 * time.Second,
	}))
	h.must(h.eng.RecordUsage(tk.ID, first, UsageDelta{
		TimeToFirstOutput: 9 * time.Second, Turns: 1, ToolCalls: 2, HookDenials: 1, Crashes: 1,
		InputTokens: 20, OutputTokens: 10, CacheReadTokens: 100, CostUSD: 0.01, ActiveTime: time.Second,
	}))

	// The Assignment fails and a person retries it.
	h.must(h.eng.FileReport(tk.ID, ReportInput{State: ReportFailed, Summary: "stuck"}))
	h.must(h.eng.RetryTicket(tk.ID))

	got := h.ticket(tk.ID).Stats
	want := TicketStats{
		QueueWait:         2 * time.Second,
		TimeToFirstOutput: 1500 * time.Millisecond, // only the first one counts
		TimeToFirstReport: 5 * time.Second,         // from Ticket creation (2 s queued + 3 s)
		ActiveTime:        5 * time.Second,
		PersonWaitTime:    37 * time.Second, // 30 s question + 7 s approval
		Turns:             3,
		ToolCalls:         7,
		ToolErrors:        1,
		HookDenials:       2,
		Escalations:       1,
		Questions:         1,
		PersonMessages:    1,
		Retries:           1,
		Crashes:           1,
		InputTokens:       120,
		OutputTokens:      60,
		CacheReadTokens:   400,
		CostUSD:           0.03,
	}
	if math.Abs(got.CostUSD-want.CostUSD) > 1e-9 {
		t.Errorf("cost = %v, want %v", got.CostUSD, want.CostUSD)
	}
	got.CostUSD, want.CostUSD = 0, 0
	eq(t, "stats", got, want)
	if r := h.ticket(tk.ID).Stats.CacheHitRatio(); math.Abs(r-400.0/520.0) > 1e-9 {
		t.Errorf("cache hit ratio = %v, want %v", r, 400.0/520.0)
	}
}

func TestStatsPersonWaitEndsWithTicket(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportWaiting, Question: "ok?"}))
	h.tick(10)
	h.must(h.eng.StopTicket(id, "never mind"))
	eq(t, "person wait", h.ticket(id).Stats.PersonWaitTime, 10*time.Second)
	eq(t, "questions", h.ticket(id).Stats.Questions, 1)
}

func TestStatsRollBackWithTheirCommand(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportWaiting, Question: "ok?"}))
	before := h.ticket(id).Stats
	h.repos.failWith = errors.New("disk full")
	h.tick(10)
	if err := h.eng.AddPersonInput(id, "yes"); err == nil {
		t.Fatal("expected the command to fail")
	}
	if err := h.eng.RecordUsage(id, "", UsageDelta{Turns: 1}); err == nil {
		t.Fatal("expected the usage record to fail")
	}
	h.repos.failWith = nil
	eq(t, "stats after failed commands", h.ticket(id).Stats, before)
}

func TestRecordUsageUnknownTicket(t *testing.T) {
	h := newHarness(t)
	wantUserError(t, "unknown ticket", h.eng.RecordUsage("T-9999", "", UsageDelta{Turns: 1}))
}

func TestFirstTrySuccess(t *testing.T) {
	done := &Ended{Outcome: OutcomeCompleted}
	cases := []struct {
		name string
		t    Ticket
		want bool
	}{
		{"clean completion", Ticket{Ended: done}, true},
		{"merged", Ticket{Ended: &Ended{Outcome: OutcomeMerged}}, true},
		{"still open", Ticket{}, false},
		{"dropped", Ticket{Ended: &Ended{Outcome: OutcomeDropped}}, false},
		{"stopped", Ticket{Ended: &Ended{Outcome: OutcomeStopped}}, false},
		{"retried", Ticket{Ended: done, Stats: TicketStats{Retries: 1}}, false},
		{"crashed", Ticket{Ended: done, Stats: TicketStats{Crashes: 1}}, false},
		{"unprompted correction", Ticket{Ended: done, Stats: TicketStats{PersonMessages: 1}}, false},
		{"answered its question", Ticket{Ended: done, Stats: TicketStats{Questions: 1, PersonMessages: 1}}, true},
		{"question and a correction", Ticket{Ended: done, Stats: TicketStats{Questions: 1, PersonMessages: 2}}, false},
	}
	for _, c := range cases {
		if got := FirstTrySuccess(&c.t); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if (TicketStats{}).CacheHitRatio() != 0 {
		t.Error("empty stats must have ratio 0")
	}
}

func TestEngineRecordsMetrics(t *testing.T) {
	h := newHarness(t)
	reg := metrics.NewRegistry()
	h.eng.SetMetrics(reg)
	tk := h.submit("Measure me")
	a := tk.Assignments[0].ID
	h.tick(2)
	h.must(h.eng.AssignmentStarted(tk.ID, a, "s", ""))
	h.must(h.eng.RecordUsage(tk.ID, a, UsageDelta{InputTokens: 7, OutputTokens: 3, CacheReadTokens: 5, CostUSD: 0.5}))
	h.must(h.eng.FileReport(tk.ID, ReportInput{State: ReportReady, Summary: "done"}))

	snap := reg.Snapshot(0)
	hist := map[string]int64{}
	for _, hv := range snap.Histograms {
		hist[hv.Name+"/"+hv.Labels["command"]] += hv.Count
	}
	for _, k := range []string{"engine_command_seconds/submit", "engine_command_seconds/assignment_started", "engine_command_seconds/file_report", "engine_command_seconds/record_usage", "dispatch_seconds/"} {
		if hist[k] != 1 {
			t.Errorf("%s count = %d, want 1 (have %v)", k, hist[k], hist)
		}
	}
	counters := map[string]float64{}
	for _, c := range snap.Counters {
		key := c.Name
		for _, l := range []string{"kind", "outcome", "runtime", "model"} {
			if v, ok := c.Labels[l]; ok {
				key += "/" + l + "=" + v
			}
		}
		counters[key] = c.Value
	}
	for k, want := range map[string]float64{
		"tokens_total/kind=input/runtime=claude/model=opus":               7,
		"tokens_total/kind=output/runtime=claude/model=opus":              3,
		"tokens_total/kind=cache_read/runtime=claude/model=opus":          5,
		"cost_usd_total/runtime=claude/model=opus":                        0.5,
		"tickets_ended_total/outcome=completed/runtime=claude/model=opus": 1,
	} {
		if counters[k] != want {
			t.Errorf("%s = %v, want %v (have %v)", k, counters[k], want, counters)
		}
	}
}

// A session reports its cumulative cost on every turn, also from a resumed process: only the growth counts, per session.
func TestRecordUsageSessionCostIsCumulative(t *testing.T) {
	h := newHarness(t)
	reg := metrics.NewRegistry()
	h.eng.SetMetrics(reg)
	id := h.accepted()
	for _, c := range []struct {
		session string
		total   float64
	}{
		{"s1", 0.010}, {"s1", 0.015}, // two turns of one process
		{"s1", 0.015}, // a repeated report adds nothing
		{"s1", 0.020}, // a resumed process carries on from 0.015
		{"s2", 0.004}, // another session starts from 0
		{"s1", 0.012}, // a lower total is ignored
	} {
		h.must(h.eng.RecordUsage(id, "", UsageDelta{SessionID: c.session, SessionCostUSD: c.total, OutputTokens: 10}))
	}
	h.must(h.eng.RecordUsage(id, "", UsageDelta{CostUSD: 0.001}))
	got := h.ticket(id).Stats
	if math.Abs(got.CostUSD-0.025) > 1e-9 {
		t.Errorf("cost = %v, want 0.025 (0.020 + 0.004 + 0.001)", got.CostUSD)
	}
	eq(t, "output tokens are per turn and summed", got.OutputTokens, int64(60))
	var metric float64
	for _, c := range reg.Snapshot(0).Counters {
		if c.Name == "cost_usd_total" {
			metric += c.Value
		}
	}
	if math.Abs(metric-0.025) > 1e-9 {
		t.Errorf("cost_usd_total = %v, want 0.025", metric)
	}
}

// A task that names no model runs on the node's default: the Executor reports the model it started the Assignment
// with, the Assignment keeps it, and the metrics are labelled with it rather than "default".
func TestMetricsUseTheEffectiveModel(t *testing.T) {
	h := newHarness(t)
	reg := metrics.NewRegistry()
	h.eng.SetMetrics(reg)
	tk, err := h.eng.SubmitOwnerTask("No model named", TaskOptions{Dir: "/work/x"})
	h.must(err)
	a := tk.Assignments[0].ID
	h.must(h.eng.AssignmentStarted(tk.ID, a, "s", "sonnet"))
	eq(t, "assignment model", h.ticket(tk.ID).Assignments[0].Model, "sonnet")
	h.must(h.eng.RecordUsage(tk.ID, a, UsageDelta{OutputTokens: 3, CostUSD: 0.5}))
	h.must(h.eng.FileReport(tk.ID, ReportInput{State: ReportReady, Summary: "done"}))
	models := map[string]string{}
	for _, c := range reg.Snapshot(0).Counters {
		models[c.Name] = c.Labels["model"]
	}
	for _, name := range []string{"tokens_total", "cost_usd_total", "tickets_ended_total"} {
		if models[name] != "sonnet" {
			t.Errorf("%s model label = %q, want sonnet (have %v)", name, models[name], models)
		}
	}
}
