package core

import (
	"slices"
	"strings"
	"testing"
)

// planLead submits a plan-first owner task on o/r and returns its id.
func planLead(h *harness) string {
	h.t.Helper()
	lead, err := h.eng.SubmitOwnerTask("Build the feature", TaskOptions{Repo: "o/r", PlanFirst: true})
	h.must(err)
	h.settle(true)
	return lead.ID
}

func drafted(h *harness, id string, tasks ...ChildSpec) {
	h.t.Helper()
	h.must(h.eng.PlanDrafted(id, Breakdown{Tasks: tasks}))
}

// runReady starts what is queued and has the child report ready.
func runReady(h *harness, id string) {
	h.t.Helper()
	h.settle(true)
	ready(h, id, "done "+id, "abcdef1234")
	h.settle(true)
}

func TestPlanFirstMakesNoAssignment(t *testing.T) {
	h := splitHarness(t)
	id := planLead(h)
	l := h.ticket(id)
	eq(t, "assignments", len(l.Assignments), 0)
	eq(t, "plan", l.Plan, &PlanState{State: PlanPlanning, At: base})
	eq(t, "stage", stageOf(l), Stage{"working", "planning"})
	eq(t, "inputs", inputKinds(l), []InputKind{InputTrigger})
	eq(t, "plan effects", effectKeys(h.effectsOf(EffectPlan)), []string{id})
	eq(t, "payload", string(h.effectsOf(EffectPlan)[0].Payload), "{}")
	eq(t, "starts", h.exec.count("start"), 0)

	f, err := h.eng.SubmitOwnerTask("Folder work", TaskOptions{Dir: "/work/x", PlanFirst: true})
	h.must(err)
	eq(t, "folder node", h.ticket(f.ID).Execution.Node, LocalNode)
	p, err := h.eng.SubmitOwnerTask("Pinned folder work", TaskOptions{Dir: "/work/y", Node: "n-pi", PlanFirst: true})
	h.must(err)
	eq(t, "pinned folder node", h.ticket(p.ID).Execution.Node, "n-pi")

	_, err = h.eng.SubmitOwnerTask("Nowhere", TaskOptions{PlanFirst: true})
	wantRefusal(t, "no repository", err, "Splitting needs a git repository: each child works on its own branch.")
}

func TestPlanDraftedThenApproved(t *testing.T) {
	h := splitHarness(t)
	id := planLead(h)
	drafted(h, id, sub("a", "Part A"), sub("b", "Part B"), sub("c", "Part C", "a"))
	l := h.ticket(id)
	eq(t, "state", l.Plan.State, PlanDraft)
	eq(t, "revision", l.Plan.Breakdown.Revision, 1)
	eq(t, "stage", stageOf(l), Stage{"person", "plan"})

	ids, err := h.eng.ApproveBreakdown(id, 1)
	h.must(err)
	eq(t, "ids", ids, []string{"T-0002", "T-0003", "T-0004"})
	l = h.ticket(id)
	eq(t, "state", l.Plan.State, PlanApproved)
	eq(t, "deps", l.Deps, ids)
	eq(t, "waiting", l.Waiting, ids)
	eq(t, "lead assignments", len(l.Assignments), 0)
	eq(t, "lead stage", stageOf(l), Stage{"working", "waiting"})
	split := l.Inputs[len(l.Inputs)-1]
	eq(t, "split input", split.Kind, InputTask)
	eq(t, "split text", split.Text, "Your task was split into 3 child tickets: T-0002: Part A, T-0003: Part B, T-0004: Part C. "+
		"When they are done, merge their branches into yours (git merge athena/<id>) and deliver the result.")
	eq(t, "a assignments", len(h.ticket(ids[0]).Assignments), 1)
	eq(t, "b assignments", len(h.ticket(ids[1]).Assignments), 1)
	eq(t, "c assignments", len(h.ticket(ids[2]).Assignments), 0)

	runReady(h, ids[0])
	eq(t, "c starts", len(h.ticket(ids[2]).Assignments), 1)
	runReady(h, ids[1])
	eq(t, "lead still waits", len(h.ticket(id).Assignments), 0)
	runReady(h, ids[2])
	l = h.ticket(id)
	eq(t, "lead assignments", len(l.Assignments), 1)
	eq(t, "lead inputs", inputKinds(l), []InputKind{InputTrigger, InputTask, InputChild, InputChild, InputChild})
	var from []string
	for _, in := range childInputs(l) {
		from = append(from, in.From)
	}
	eq(t, "child inputs in order", from, ids)
	eq(t, "lead running", runningOf(l) != nil, true)
}

func TestApproveRefusesBadDraft(t *testing.T) {
	h := splitHarness(t)
	id := planLead(h)
	drafted(h, id, sub("a", "Part A"), sub("b", "Part B"))

	_, err := h.eng.UpdateBreakdown(id, 1, []ChildSpec{sub("a", "Part A", "b"), sub("b", "Part B", "a")})
	wantRefusal(t, "cycle", err, "form a cycle")
	eq(t, "revision kept", h.ticket(id).Plan.Breakdown.Revision, 1)

	same := []ChildSpec{sub("a", "Part A"), sub("b", "Part B"), sub("c", "Part C")}
	same[2].Areas = []string{"internal/a/x"}
	b, err := h.eng.UpdateBreakdown(id, 1, same)
	h.must(err)
	eq(t, "revision", b.Revision, 2)
	eq(t, "overlaps", b.Overlaps, []Overlap{{A: "a", B: "c", Areas: []string{"internal/a"}}})
	eq(t, "stored", *h.ticket(id).Plan.Breakdown, b)

	_, err = h.eng.UpdateBreakdown(id, 1, same)
	wantRefusal(t, "stale update", err, "The split changed meanwhile; reload it and try again.")
	_, err = h.eng.ApproveBreakdown(id, 1)
	wantRefusal(t, "stale approve", err, "The split changed meanwhile; reload it and try again.")
	eq(t, "still a draft", h.ticket(id).Plan.State, PlanDraft)
	eq(t, "no children", len(h.ticket(id).Children), 0)
}

func TestTinyBreakdownRunsAsOne(t *testing.T) {
	h := splitHarness(t)
	id := planLead(h)
	drafted(h, id, sub("a", "Part A"))
	l := h.ticket(id)
	eq(t, "state", l.Plan.State, PlanRejected)
	eq(t, "note", l.Plan.Breakdown.Note, "The planner found nothing to split; it runs as one ticket.")
	eq(t, "assignments", len(l.Assignments), 1)
	eq(t, "children", len(l.Children), 0)
	eq(t, "starts", h.exec.count("start"), 1)
}

func TestAutoApproveOnlyWithoutOverlap(t *testing.T) {
	h := newHarnessWith(t, func(o *Options) {
		o.Policy = func() Policy { return &fakePolicy{holds: true} }
		o.Nodes = fakeNodes{"mac": "n-mac"}
		o.Split = func() SplitLimits { return SplitLimits{AutoApprove: 3} }
	})
	id := planLead(h)
	drafted(h, id, sub("a", "Part A"), sub("b", "Part B"), sub("c", "Part C"))
	l := h.ticket(id)
	eq(t, "approved", l.Plan.State, PlanApproved)
	eq(t, "children", len(l.Children), 3)

	other := planLead(h)
	tasks := []ChildSpec{sub("a", "Part A"), sub("b", "Part B"), sub("c", "Part C")}
	tasks[2].Areas = []string{"internal/a"}
	drafted(h, other, tasks...)
	o := h.ticket(other)
	eq(t, "overlap stays a draft", o.Plan.State, PlanDraft)
	eq(t, "no children", len(o.Children), 0)

	big := planLead(h)
	drafted(h, big, sub("a", "A"), sub("b", "B"), sub("c", "C"), sub("d", "D"))
	eq(t, "over the limit stays a draft", h.ticket(big).Plan.State, PlanDraft)
}

func TestRejectStartsAsOne(t *testing.T) {
	h := splitHarness(t)
	id := planLead(h)
	wantRefusal(t, "reject while planning", h.eng.RejectBreakdown(id), "")
	drafted(h, id, sub("a", "Part A"), sub("b", "Part B"))
	h.must(h.eng.RejectBreakdown(id))
	l := h.ticket(id)
	eq(t, "state", l.Plan.State, PlanRejected)
	eq(t, "assignments", len(l.Assignments), 1)
	eq(t, "children", len(l.Children), 0)
	h.settle(true)
	eq(t, "running", runningOf(h.ticket(id)) != nil, true)

	failed := planLead(h)
	h.must(h.eng.PlanFailed(failed, "the planner timed out"))
	h.must(h.eng.RejectBreakdown(failed))
	eq(t, "from failed", h.ticket(failed).Plan.State, PlanRejected)
	eq(t, "from failed assignments", len(h.ticket(failed).Assignments), 1)
	wantRefusal(t, "reject twice", h.eng.RejectBreakdown(failed), "")
}

func TestPlanFirstLeadSurvivesDrop(t *testing.T) {
	h := splitHarness(t)
	id := planLead(h)
	drafted(h, id, sub("a", "Part A"), sub("b", "Part B"), sub("c", "Part C"))
	ids, err := h.eng.ApproveBreakdown(id, 1)
	h.must(err)
	h.settle(true)
	h.must(h.eng.StopTicket(ids[0], "not needed"))
	l := h.ticket(id)
	eq(t, "lead open", l.Ended == nil, true)
	eq(t, "deps", l.Deps, ids[1:])
	eq(t, "waiting", l.Waiting, ids[1:])
	runReady(h, ids[1])
	eq(t, "lead waits", len(h.ticket(id).Assignments), 0)
	runReady(h, ids[2])
	eq(t, "lead starts", len(h.ticket(id).Assignments), 1)
}

func TestSplitEventsRecorded(t *testing.T) {
	h := splitHarness(t)
	has := func(id, typ string) {
		t.Helper()
		if !slices.Contains(eventTypes(h.ticket(id)), typ) {
			t.Errorf("%s: events %v lack %s", id, eventTypes(h.ticket(id)), typ)
		}
	}
	approved := planLead(h)
	drafted(h, approved, sub("a", "Part A"), sub("b", "Part B"))
	has(approved, EventSplitDrafted)
	_, err := h.eng.ApproveBreakdown(approved, 1)
	h.must(err)
	has(approved, EventSplitApproved)

	rejected := planLead(h)
	drafted(h, rejected, sub("a", "Part A"), sub("b", "Part B"))
	h.must(h.eng.RejectBreakdown(rejected))
	has(rejected, EventSplitRejected)

	failed := planLead(h)
	why := strings.Repeat("é", 400)
	h.must(h.eng.PlanFailed(failed, why))
	has(failed, EventSplitFailed)
	f := h.ticket(failed)
	eq(t, "failed state", f.Plan.State, PlanFailed)
	eq(t, "error cut", f.Plan.Error, strings.Repeat("é", 300))
	eq(t, "failed stage", stageOf(f), Stage{"person", "plan"})

	one := planLead(h)
	drafted(h, one, sub("a", "Part A"))
	has(one, EventSplitOne)
}

func TestReplanFromFailed(t *testing.T) {
	h := splitHarness(t)
	id := planLead(h)
	wantRefusal(t, "replan while planning", h.eng.Replan(id), "")
	h.must(h.eng.PlanFailed(id, "no answer"))
	h.must(h.eng.Replan(id))
	l := h.ticket(id)
	eq(t, "plan", l.Plan, &PlanState{State: PlanPlanning, At: base})
	eq(t, "plan effects", effectKeys(h.effectsOf(EffectPlan)), []string{id})
	drafted(h, id, sub("a", "Part A"), sub("b", "Part B"))
	eq(t, "drafted", h.ticket(id).Plan.State, PlanDraft)
	h.must(h.eng.Replan(id))
	eq(t, "replanned from draft", h.ticket(id).Plan.State, PlanPlanning)
}

func TestLatePlanEffectIgnored(t *testing.T) {
	h := splitHarness(t)
	id := planLead(h)
	drafted(h, id, sub("a", "Part A"), sub("b", "Part B"))
	h.must(h.eng.RejectBreakdown(id))
	before := h.ticket(id)
	drafted(h, id, sub("x", "Part X"), sub("y", "Part Y"))
	h.must(h.eng.PlanFailed(id, "late"))
	eq(t, "unchanged", h.ticket(id), before)
}
