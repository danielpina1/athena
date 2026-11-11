package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

// plannerFixture writes a fakeclaude fixture whose one turn answers with result, and points the planner's role at it.
func plannerFixture(t *testing.T, result string) {
	t.Helper()
	line, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1,
		"session_id": "9d1c7e2a-0000-4000-8000-000000000002", "result": result})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "planner.jsonl")
	init := `{"type":"system","subtype":"init","cwd":"/w","session_id":"9d1c7e2a-0000-4000-8000-000000000002","tools":[],"mcp_servers":[],"model":"claude-haiku-4-5-20251001","permissionMode":"default","claude_code_version":"2.1.280"}`
	if err := os.WriteFile(p, []byte(init+"\n"+string(line)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATHENA_FAKE_PLANNER_FIXTURE", p)
}

// threeTasks is a planner reply of three sub-tasks; with overlap, the first and the third share internal/api.
func threeTasks(overlap bool) string {
	third := "docs"
	if overlap {
		third = "internal/api/doc.go"
	}
	return `Here is the split: {"tasks":[` +
		`{"key":"api","title":"Add the API","instructions":"Add the endpoint.","areas":["internal/api"]},` +
		`{"key":"ui","title":"Add the page","instructions":"Add the page.","areas":["web/src"],"deps":["api"]},` +
		`{"key":"docs","title":"Document it","instructions":"Write the docs.","areas":["` + third + `"]}],"note":"API first."}`
}

// startPlanServer starts a server and returns it with its Dispatcher.
func startPlanServer(t *testing.T, dir string) (*testServer, *Dispatcher) {
	t.Helper()
	ready := make(chan *Dispatcher, 1)
	s := startServerWith(t, dir, func(c *Config) {
		fastClose(c)
		c.dispatcherReady = func(d *Dispatcher) { ready <- d }
	})
	return s, <-ready
}

// submitPlanFirst submits a plan-first task in a fresh git repository and returns its id.
func (s *testServer) submitPlanFirst(text string) string {
	s.t.Helper()
	var sub struct {
		ID string `json:"id"`
	}
	s.ok(Request{Op: OpSubmit, Text: text, Dir: newRepo(s.t), PlanFirst: true}, &sub)
	if sub.ID == "" {
		s.t.Fatal("submit returned no id")
	}
	return sub.ID
}

// waitStage waits until the Ticket's stage is want.
func (s *testServer) waitStage(id, want string) {
	s.t.Helper()
	var got string
	eventually(s.t, id+" at "+want, func() bool {
		got = s.detail(id).Stage
		return got == want
	})
}

func ticketOf(t *testing.T, d *Dispatcher, id string) core.Ticket {
	t.Helper()
	tk, _, err := d.eng.Ticket(id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// A plan-first task's planner runs the --claude binary (fakeclaude, never the real claude) and its breakdown becomes
// the draft the owner reviews: three sub-tasks, one overlap, the planner's transcript under the state directory.
func TestPlanEffectDraftsWithFakeClaude(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	plannerFixture(t, threeTasks(true))
	dir := newStateDir(t)
	s, d := startPlanServer(t, dir)

	id := s.submitPlanFirst("Add search\n\nAdd a search API, its page and its docs.")
	s.waitStage(id, "person/plan")
	tk := ticketOf(t, d, id)
	if tk.Plan == nil || tk.Plan.State != core.PlanDraft || tk.Plan.Breakdown == nil {
		t.Fatalf("plan = %+v", tk.Plan)
	}
	b := tk.Plan.Breakdown
	if len(b.Tasks) != 3 || len(b.Overlaps) != 1 || b.Strategy != "llm" {
		t.Fatalf("breakdown = %+v", b)
	}
	if len(tk.Assignments) != 0 {
		t.Fatalf("a drafted plan-first Ticket has Assignments: %v", tk.Assignments)
	}
	if _, err := os.Stat(filepath.Join(dir, "transcripts", "planner.jsonl")); err != nil {
		t.Fatalf("no planner transcript under the state directory: %v", err)
	}
	invs := fake.invocations(t)
	if len(invs) != 1 {
		t.Fatalf("fakeclaude ran %d times, want once (the planner): %q", len(invs), invs)
	}
	if m, _ := argValue(invs[0], "--model"); m != "haiku" {
		t.Errorf("planner model = %q, want haiku", m)
	}
	// The one message names the limit, the ready runtimes, the nodes and the task.
	msg := fake.stdin(t)
	for _, want := range []string{"at most 5 sub-tasks", "Runtimes: claude", "Nodes: ", "Add a search API, its page and its docs."} {
		if !strings.Contains(msg, want) {
			t.Errorf("planner message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Nodes: none offered") {
		t.Errorf("the planner was offered no node:\n%s", msg)
	}
}

// A planner reply with no split fails the plan with the planner's reason, for the owner; it is not retried.
func TestPlanEffectFailureShown(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	plannerFixture(t, "I would rather not split this one.")
	dir := newStateDir(t)
	s, d := startPlanServer(t, dir)

	id := s.submitPlanFirst("Add search")
	s.waitStage(id, "person/plan")
	tk := ticketOf(t, d, id)
	if tk.Plan == nil || tk.Plan.State != core.PlanFailed || !strings.Contains(tk.Plan.Error, "the reply holds no JSON object with tasks") {
		t.Fatalf("plan = %+v", tk.Plan)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(fake.invocations(t)); n != 1 {
		t.Fatalf("the planner ran %d times, want once: a failure is never retried", n)
	}
}

// A planner turn the server stopped in the middle of runs again on the next start, and its draft arrives.
func TestPlanEffectSurvivesRestart(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	plannerFixture(t, threeTasks(false))
	hold := filepath.Join(t.TempDir(), "release")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	dir := newStateDir(t)
	s, _ := startPlanServer(t, dir)

	id := s.submitPlanFirst("Add search")
	eventually(t, "the planner started", func() bool { return len(fake.invocations(t)) == 1 })
	s.stop()

	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, d := startPlanServer(t, dir)
	s.waitStage(id, "person/plan")
	if tk := ticketOf(t, d, id); tk.Plan.State != core.PlanDraft || len(tk.Plan.Breakdown.Tasks) != 3 {
		t.Fatalf("plan = %+v", tk.Plan)
	}
	if n := len(fake.invocations(t)); n != 2 {
		t.Fatalf("the planner ran %d times, want twice", n)
	}
}

// While Athena is paused the planner does not run; resuming runs it at once.
func TestPlanHeldWhilePaused(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	plannerFixture(t, threeTasks(true))
	dir := newStateDir(t)
	s, _ := startPlanServer(t, dir)

	s.ok(Request{Op: OpPause}, nil)
	id := s.submitPlanFirst("Add search")
	time.Sleep(500 * time.Millisecond)
	if st := s.detail(id).Stage; st != "working/planning" {
		t.Fatalf("stage while paused = %q", st)
	}
	if n := len(fake.invocations(t)); n != 0 {
		t.Fatalf("the planner ran %d times while paused", n)
	}
	s.ok(Request{Op: OpResume}, nil)
	s.waitStage(id, "person/plan")
}

// The Engine's split limits are the current Policy's: an auto-approve limit saved in the settings splits a draft at once,
// and the planner is asked for at most the Policy's fan-out cap.
func TestPlanEffectFollowsPolicySplit(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	plannerFixture(t, threeTasks(false))
	dir := newStateDir(t)
	s, d := startPlanServer(t, dir)

	var pv PolicyView
	s.ok(Request{Op: OpPolicyShow}, &pv)
	pv.Settings.Split.MaxChildren = 4
	pv.Settings.Split.AutoApprove = 3
	raw, err := json.Marshal(pv.Settings)
	if err != nil {
		t.Fatal(err)
	}
	s.ok(Request{Op: OpPolicySet, Policy: raw}, nil)

	id := s.submitPlanFirst("Add search")
	eventually(t, id+" split at once", func() bool {
		tk := ticketOf(t, d, id)
		return tk.Plan != nil && tk.Plan.State == core.PlanApproved && len(tk.Children) == 3
	})
	if msg := fake.stdin(t); !strings.Contains(msg, "at most 4 sub-tasks") {
		t.Errorf("planner message lacks the Policy's cap:\n%s", msg)
	}
}
