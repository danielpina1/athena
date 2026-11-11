package core

import (
	"slices"
	"strings"
	"testing"
)

// fakeNodes is a NodeDirectory: node name to id.
type fakeNodes map[string]string

func (n fakeNodes) Resolve(s string) (string, bool) {
	for name, id := range n {
		if s == name || s == id {
			return id, true
		}
	}
	return "", false
}

func (n fakeNodes) Name(id string) string {
	for name, nid := range n {
		if nid == id {
			return name
		}
	}
	return id
}

// splitHarness is a harness with a Policy (WIP ceiling 3) and a node directory.
func splitHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, func(o *Options) {
		o.Policy = func() Policy { return &fakePolicy{holds: true} }
		o.Nodes = fakeNodes{"mac": "n-mac", "pi": "n-pi"}
	})
}

// repoLead submits a running owner task on o/r and returns its id.
func repoLead(h *harness) string {
	h.t.Helper()
	lead, err := h.eng.SubmitOwnerTask("Build the feature", TaskOptions{Repo: "o/r", Model: "opus"})
	h.must(err)
	h.settle(true)
	return lead.ID
}

// edit changes a stored Ticket.
func edit(h *harness, id string, f func(t *Ticket)) {
	h.t.Helper()
	h.must(h.repos.Tx(func(tx Tx) error {
		t, err := tx.GetTicket(id)
		if err != nil {
			return err
		}
		f(t)
		return tx.PutTicket(t)
	}))
}

func sub(key, title string, deps ...string) ChildSpec {
	return ChildSpec{Key: key, Title: title, Instructions: "Do " + title + ".", Areas: []string{"internal/" + key}, Deps: deps}
}

func spawn(h *harness, lead string, specs ...ChildSpec) []string {
	h.t.Helper()
	ids, err := h.eng.SpawnChildren(lead, specs)
	h.must(err)
	return ids
}

// ready has a running child report ready with a head and ends it.
func ready(h *harness, id, summary, head string) {
	h.t.Helper()
	h.must(h.eng.FileReport(id, ReportInput{State: ReportReady, Summary: summary, Head: head}))
}

func wantRefusal(t *testing.T, what string, err error, want string) {
	t.Helper()
	wantUserError(t, what, err)
	if err != nil && !strings.Contains(err.Error(), want) {
		t.Errorf("%s: got %q, want it to contain %q", what, err.Error(), want)
	}
}

func childInputs(t *Ticket) []Input {
	var out []Input
	for _, in := range t.Inputs {
		if in.Kind == InputChild {
			out = append(out, in)
		}
	}
	return out
}

func eventTypes(t *Ticket) []string {
	var out []string
	for _, e := range t.Events {
		out = append(out, e.Type)
	}
	return out
}

func TestSpawnChildrenCopiesMandateAndStarts(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	s2 := sub("api", "Part B")
	s2.Node, s2.Runtime, s2.Model = "mac", RuntimeCodex, "gpt"
	ids := spawn(h, lead, sub("ui", "Part A"), s2)
	eq(t, "ids", ids, []string{"T-0002", "T-0003"})
	l := h.ticket(lead)
	eq(t, "lead children", l.Children, ids)
	for _, id := range ids {
		c := h.ticket(id)
		eq(t, id+" mandate", c.Mandate, l.Mandate)
		eq(t, id+" origin", c.Origin, l.Origin)
		eq(t, id+" repo", c.Repo, "o/r")
		eq(t, id+" lead", c.Lead, lead)
		eq(t, id+" depth", c.Depth, 1)
		eq(t, id+" channels", len(c.Channels), 0)
		eq(t, id+" pairing", c.Pairing, "")
		eq(t, id+" assignments", assignmentStates(c), []AssignmentState{AssignmentPending})
		eq(t, id+" inputs", inputKinds(c), []InputKind{InputTask})
		brief := c.Inputs[0].Text
		for _, want := range []string{lead, c.Title, "Do " + c.Title + ".", "internal/" + c.SplitKey, "athena/" + strings.ToLower(id),
			"Commit your work on your branch and report ready; do not push or open a pull request: " + lead + " merges your branch."} {
			if !strings.Contains(brief, want) {
				t.Errorf("%s brief lacks %q:\n%s", id, want, brief)
			}
		}
		var dec *Decision
		h.must(h.repos.Tx(func(tx Tx) (err error) { dec, err = tx.GetDecision(c.DecisionID); return }))
		if dec == nil {
			t.Fatalf("%s: no decision", id)
		}
		eq(t, id+" decision", dec.Reasons, []Reason{{Code: "split", Values: map[string]string{"lead": lead}}})
		eq(t, id+" decision kind", dec.Kind, "accept")
	}
	eq(t, "child A execution", h.ticket(ids[0]).Execution, &Execution{Runtime: RuntimeClaude, Model: "opus"})
	eq(t, "child B execution", h.ticket(ids[1]).Execution, &Execution{Runtime: RuntimeCodex, Model: "gpt", Node: "n-mac"})
	eq(t, "split keys", []string{h.ticket(ids[0]).SplitKey, h.ticket(ids[1]).SplitKey}, []string{"ui", "api"})
	h.settle(true)
	for _, id := range ids {
		eq(t, id+" running", assignmentStates(h.ticket(id)), []AssignmentState{AssignmentRunning})
	}
}

func TestDepsHoldUntilEnded(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("a", "Part A"), sub("b", "Part B", "a"))
	a, b := ids[0], ids[1]
	eq(t, "b deps", h.ticket(b).Deps, []string{a})
	eq(t, "b waiting", h.ticket(b).Waiting, []string{a})
	eq(t, "b assignments", len(h.ticket(b).Assignments), 0)
	eq(t, "b stage", stageOf(h.ticket(b)), Stage{"working", "waiting"})
	h.settle(true)
	eq(t, "b still no assignment", len(h.ticket(b).Assignments), 0)

	ready(h, a, "Part A done", "abc1234def")
	eq(t, "a head", h.ticket(a).Assignments[0].Report.Head, "abc1234def")
	got := h.ticket(b)
	eq(t, "b waiting after", len(got.Waiting), 0)
	eq(t, "b deps kept", got.Deps, []string{a})
	eq(t, "b assignments after", assignmentStates(got), []AssignmentState{AssignmentPending})
	eq(t, "b inputs", inputKinds(got), []InputKind{InputTask, InputChild})
	if !strings.Contains(got.Inputs[1].Text, "athena/t-0002") || !strings.Contains(got.Inputs[1].Text, "abc1234") {
		t.Errorf("dep input: %q", got.Inputs[1].Text)
	}
	eq(t, "dep input from", got.Inputs[1].From, a)
	h.settle(true)
	eq(t, "b runs", assignmentStates(h.ticket(b)), []AssignmentState{AssignmentRunning})
}

func TestInputDoesNotStartWaitingChild(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("a", "Part A"), sub("b", "Part B", "a"))
	h.settle(true)
	starts := h.exec.count("start")
	h.must(h.eng.AddPersonInput(ids[1], "also mind the tests"))
	got := h.ticket(ids[1])
	eq(t, "inputs", inputKinds(got), []InputKind{InputTask, InputTrigger})
	eq(t, "assignments", len(got.Assignments), 0)
	eq(t, "starts", h.exec.count("start"), starts)
}

func TestSpawnFolderChildrenPinned(t *testing.T) {
	h := splitHarness(t)
	lead, err := h.eng.SubmitOwnerTask("Folder work", TaskOptions{Dir: "/work/x", Node: "n-pi"})
	h.must(err)
	h.settle(true)
	s := sub("a", "Part A")
	s.Node = "mac"
	ids := spawn(h, lead.ID, s)
	eq(t, "pinned", h.ticket(ids[0]).Execution, &Execution{Directory: "/work/x", Runtime: RuntimeClaude, Node: "n-pi"})

	// Without a pin, the node of the lead's placement.
	other, err := h.eng.SubmitOwnerTask("Folder work 2", TaskOptions{Dir: "/work/y"})
	h.must(err)
	h.settle(true)
	h.must(h.eng.AssignmentPlaced(other.ID, h.ticket(other.ID).Assignments[0].ID, "n-mac"))
	ids = spawn(h, other.ID, s)
	eq(t, "placement node", h.ticket(ids[0]).Execution.Node, "n-mac")
}

func TestSpawnRefusals(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)

	edit(h, lead, func(t *Ticket) { t.Depth = 3 })
	_, err := h.eng.SpawnChildren(lead, []ChildSpec{sub("a", "A")})
	wantRefusal(t, "depth", err, "Child tickets nest at most 3 deep; do this part yourself.")
	edit(h, lead, func(t *Ticket) { t.Depth = 0 })

	_, err = h.eng.SpawnChildren(lead, []ChildSpec{sub("x", "X", "y"), sub("y", "Y", "x")})
	wantRefusal(t, "cycle", err, "cycle")

	_, err = h.eng.SpawnChildren(lead, []ChildSpec{func() ChildSpec { s := sub("z", "Z"); s.Node = "nowhere"; return s }()})
	wantRefusal(t, "node", err, "no such node: nowhere")

	spawn(h, lead, sub("a", "A"), sub("b", "B"), sub("c", "C"), sub("d", "D"))
	_, err = h.eng.SpawnChildren(lead, []ChildSpec{sub("e", "E"), sub("f", "F")})
	wantRefusal(t, "cap", err, lead+" may have at most 5 child tickets (it has 4); split into fewer pieces or do the rest yourself.")

	// A lead in planning is refused by SpawnChildren, not by o.spawn (ApproveBreakdown's path).
	planned := repoLead(h)
	edit(h, planned, func(t *Ticket) { t.Plan = &PlanState{State: PlanPlanning} })
	_, err = h.eng.SpawnChildren(planned, []ChildSpec{sub("a", "A")})
	wantUserError(t, "planning", err)
	var ids []string
	h.must(h.eng.run(func(o *op) error {
		l, err := o.require(planned)
		if err != nil {
			return err
		}
		if ids, err = o.spawn(l, []ChildSpec{sub("a", "A")}); err != nil {
			return err
		}
		return o.tx.PutTicket(l)
	}))
	eq(t, "o.spawn", len(ids), 1)

	// A Dir-less and Repo-less lead.
	plain, err := h.eng.SubmitOwnerTask("Plain", TaskOptions{})
	h.must(err)
	_, err = h.eng.SpawnChildren(plain.ID, []ChildSpec{sub("a", "A")})
	wantRefusal(t, "plain", err, "Splitting needs a git repository: each child works on its own branch.")

	_, err = h.eng.SpawnChildren("T-0999", []ChildSpec{sub("a", "A")})
	wantUserError(t, "unknown", err)
}

func TestSpawnBrake(t *testing.T) {
	h := splitHarness(t)
	inferred := repoLead(h)
	explicit := repoLead(h)
	edit(h, inferred, func(t *Ticket) { t.Mandate.Strength = StrengthInferred })
	for i := 0; i < 3; i++ {
		id := repoLead(h)
		edit(h, id, func(t *Ticket) { t.Attention = &Attention{ID: "q", Question: "?"} })
	}
	_, _, before := h.repos.counts()
	_, err := h.eng.SpawnChildren(inferred, []ChildSpec{sub("a", "A")})
	wantRefusal(t, "brake", err, "Athena holds back new work in o/r: 3 things wait on the owner there (the ceiling is 3). Do this part yourself, or wait.")
	_, _, after := h.repos.counts()
	eq(t, "no decision", after, before)
	eq(t, "no children", len(h.ticket(inferred).Children), 0)
	spawn(h, explicit, sub("a", "A"))
}

func TestLeadReadyRefusedWithOpenChildren(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	spawn(h, lead, sub("a", "A"), sub("b", "B"))
	err := h.eng.FileReport(lead, ReportInput{State: ReportReady, Summary: "done"})
	wantRefusal(t, "ready", err, lead+" has open child tickets (T-0002, T-0003): wait_for them, or ask the owner to drop them, before you report ready.")
	eq(t, "lead open", h.ticket(lead).Ended == nil, true)
}

func TestEndingLeadStopsChildren(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("a", "A"), sub("b", "B", "a"))
	h.settle(true)
	running := h.ticket(ids[0]).Assignments[0].ID
	h.must(h.eng.StopTicket(lead, ""))
	for _, id := range ids {
		c := h.ticket(id)
		if c.Ended == nil {
			t.Fatalf("%s still open", id)
		}
		eq(t, id+" ended", *c.Ended, Ended{At: base, By: EndByExecutor, Outcome: OutcomeStopped, Reason: ReasonLeadEnded})
	}
	if !slices.Contains(h.exec.methods(), "stop:"+running+":"+ReasonLeadEnded) {
		t.Errorf("running child not stopped: %v", h.exec.methods())
	}
	eq(t, "lead got nothing", len(childInputs(h.ticket(lead))), 0)
	eq(t, "b never started", len(h.ticket(ids[1]).Assignments), 0)
}

func TestChildFailureAttentionOnLead(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("a", "Part A"))
	h.settle(true)
	h.must(h.eng.FileReport(ids[0], ReportInput{State: ReportFailed, Summary: "tests fail"}))
	l := h.ticket(lead)
	if l.Attention == nil {
		t.Fatal("no attention on lead")
	}
	eq(t, "child", l.Attention.Child, ids[0])
	eq(t, "question", l.Attention.Question, "Child T-0002 (Part A) failed: tests fail. Retry it, drop it, or continue without it.")
	eq(t, "stage", stageOf(l), Stage{"person", "child"})
	h.must(h.eng.RetryTicket(ids[0]))
	eq(t, "cleared", h.ticket(lead).Attention, (*Attention)(nil))

	h.settle(true)
	h.must(h.eng.FileReport(ids[0], ReportInput{State: ReportBlocked, Summary: "no access"}))
	eq(t, "blocked question", h.ticket(lead).Attention.Question, "Child T-0002 (Part A) is blocked: no access. Retry it, drop it, or continue without it.")
}

func TestDropStopsUnstartedDependents(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("a", "A"), sub("b", "B", "a"), sub("c", "C"), sub("d", "D", "b"))
	a, b, c, d := ids[0], ids[1], ids[2], ids[3]
	h.settle(true)
	h.must(h.eng.StopTicket(a, ""))
	for _, id := range []string{a, b, d} {
		got := h.ticket(id)
		if got.Ended == nil || got.Ended.Outcome != OutcomeStopped {
			t.Errorf("%s not stopped: %+v", id, got.Ended)
		}
	}
	eq(t, "c open", h.ticket(c).Ended == nil, true)
	eq(t, "c runs", assignmentStates(h.ticket(c)), []AssignmentState{AssignmentRunning})
	l := h.ticket(lead)
	eq(t, "lead open", l.Ended == nil, true)
	var texts []string
	for _, in := range childInputs(l) {
		texts = append(texts, in.Text)
	}
	eq(t, "lead inputs", texts, []string{a + " was dropped by the owner.", b + " was dropped by the owner.", d + " was dropped by the owner."})
}

func TestDropKeepsPlanFirstLead(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("a", "A"), sub("b", "B"))
	a, b := ids[0], ids[1]
	// As ApproveBreakdown leaves a plan-first lead: no Assignment, waiting on every child.
	edit(h, lead, func(t *Ticket) { t.Assignments = nil; t.Deps = []string{a, b}; t.Waiting = []string{a, b} })
	h.settle(true)
	h.must(h.eng.StopTicket(a, ""))
	l := h.ticket(lead)
	eq(t, "lead open", l.Ended == nil, true)
	eq(t, "lead deps", l.Deps, []string{b})
	eq(t, "lead waiting", l.Waiting, []string{b})
	eq(t, "lead no assignment", len(l.Assignments), 0)
	eq(t, "b open", h.ticket(b).Ended == nil, true)

	ready(h, b, "B done", "1234567890")
	l = h.ticket(lead)
	eq(t, "lead waiting after", len(l.Waiting), 0)
	eq(t, "lead deps after", len(l.Deps), 0)
	eq(t, "lead starts", assignmentStates(l), []AssignmentState{AssignmentPending})
}

func TestChildEventsRecorded(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("a", "A"), sub("b", "B"))
	h.settle(true)
	h.must(h.eng.FileReport(ids[0], ReportInput{State: ReportFailed, Summary: "broke"}))
	h.must(h.eng.StopTicket(ids[0], ""))
	ready(h, ids[1], "ok", "abcdef0")
	l := h.ticket(lead)
	eq(t, "events", eventTypes(l), []string{"child/spawned", "child/spawned", "child/failed", "child/ended", "child/ended"})
	eq(t, "outcomes", []string{l.Events[3].Outcome, l.Events[4].Outcome}, []string{"stopped", "ready"})
	eq(t, "refs", []string{l.Events[0].Ref, l.Events[2].Ref, l.Events[4].Ref}, []string{ids[0], ids[0], ids[1]})
	if !slices.Contains(h.events.types(), EventChildrenSpawned) {
		t.Errorf("no %s event: %v", EventChildrenSpawned, h.events.types())
	}
}

func TestContinueWithoutStartsDependents(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("a", "A"), sub("b", "B", "a"))
	a, b := ids[0], ids[1]
	h.settle(true)
	h.must(h.eng.FileReport(a, ReportInput{State: ReportFailed, Summary: "broke"}))
	wantRefusal(t, "not a child", h.eng.ContinueWithout(lead), lead+" is not a child ticket")
	h.must(h.eng.ContinueWithout(a))
	got := h.ticket(a)
	eq(t, "a ended", *got.Ended, Ended{At: base, By: EndByHuman, Outcome: OutcomeStopped, Reason: ReasonContinuedWithout})
	eq(t, "b starts", assignmentStates(h.ticket(b)), []AssignmentState{AssignmentPending})
	eq(t, "b only its brief", inputKinds(h.ticket(b)), []InputKind{InputTask})
	l := h.ticket(lead)
	eq(t, "attention cleared", l.Attention, (*Attention)(nil))
	in := childInputs(l)
	eq(t, "lead told", len(in), 1)
	eq(t, "lead text", in[0].Text, "The owner chose to continue without "+a+".")
	wantRefusal(t, "ended", h.eng.ContinueWithout(a), a+" has ended")
}

func TestChildReadyTellsLead(t *testing.T) {
	h := splitHarness(t)
	lead := repoLead(h)
	ids := spawn(h, lead, sub("a", "Part A"))
	h.settle(true)
	running := h.ticket(lead).Assignments[0].ID
	ready(h, ids[0], "All wired", "abc1234def")
	l := h.ticket(lead)
	in := childInputs(l)
	eq(t, "one input", len(in), 1)
	eq(t, "text", in[0].Text, "T-0002 (Part A) is ready: All wired. Its branch athena/t-0002 is at abc1234; merge it into yours with git merge athena/t-0002.")
	eq(t, "from", in[0].From, ids[0])
	eq(t, "undelivered", l.Undelivered, []string{in[0].ID})
	eq(t, "delivered", h.exec.last("deliver").AssignmentID, running)
}
