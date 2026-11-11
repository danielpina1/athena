package nodelink

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// Each agent call of the engine proxy is a request (fact_seq 0) of its own method, with its Args, and decodes the answer.
func TestProxyAgentCalls(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	fs := srv.add(0)
	answers := map[string]any{
		MethodSpawnChildren: []string{"T-0002"},
		MethodAskAgent:      core.AgentMessage{ID: "m-1", From: "T-0001", To: "T-0002", Text: "q"},
		MethodAnswerAgent:   core.AgentMessage{ID: "m-2", From: "T-0002", To: "T-0001", Text: "a", ReplyTo: "m-1"},
		MethodWaitFor:       core.WaitResult{Done: true, Summary: "done"},
		MethodAgents:        []core.AgentView{{ID: "T-0002", Relation: "child"}},
		MethodPrompts:       ResultPrompts{Worker: "W", Reviewer: "R"},
	}
	fs.answer = func(call *athenav1.NodeCall) *athenav1.NodeResult {
		v, ok := answers[call.GetMethod()]
		if !ok {
			return nil
		}
		b, _ := json.Marshal(v)
		return &athenav1.NodeResult{Value: b}
	}
	run(t, c)
	select {
	case <-c.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	eng := c.Engine()
	specs := []core.ChildSpec{{Title: "Docs", Instructions: "Write them", Base: "abc"}}
	ids, err := eng.SpawnChildren("T-0001", specs)
	if err != nil || !reflect.DeepEqual(ids, []string{"T-0002"}) {
		t.Fatalf("SpawnChildren = %v %v", ids, err)
	}
	m, err := eng.AskAgent("T-0001", "T-0002", "q")
	if err != nil || m.ID != "m-1" {
		t.Fatalf("AskAgent = %+v %v", m, err)
	}
	m, err = eng.AnswerAgent("T-0002", "m-1", "a")
	if err != nil || m.ReplyTo != "m-1" {
		t.Fatalf("AnswerAgent = %+v %v", m, err)
	}
	w, err := eng.WaitFor(context.Background(), "T-0001", "T-0002")
	if err != nil || !w.Done || w.Summary != "done" {
		t.Fatalf("WaitFor = %+v %v", w, err)
	}
	views, err := eng.Agents("T-0001")
	if err != nil || len(views) != 1 || views[0].ID != "T-0002" {
		t.Fatalf("Agents = %+v %v", views, err)
	}
	want := map[string]any{
		MethodSpawnChildren: &ArgsSpawnChildren{LeadID: "T-0001", Specs: specs},
		MethodAskAgent:      &ArgsAskAgent{FromID: "T-0001", Target: "T-0002", Text: "q"},
		MethodAnswerAgent:   &ArgsAnswerAgent{FromID: "T-0002", MessageID: "m-1", Text: "a"},
		MethodWaitFor:       &ArgsWaitFor{TicketID: "T-0001", Target: "T-0002"},
		MethodAgents:        &ArgsAgents{TicketID: "T-0001"},
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	seen := map[string]bool{}
	for _, call := range fs.calls {
		w, ok := want[call.GetMethod()]
		if !ok {
			continue
		}
		seen[call.GetMethod()] = true
		if call.GetFactSeq() != 0 || IsFact(call.GetMethod()) {
			t.Errorf("%s is not a request", call.GetMethod())
		}
		got, ok := NewArgs(call.GetMethod())
		if !ok {
			t.Fatalf("no Args for %s", call.GetMethod())
		}
		if err := json.Unmarshal(call.GetArgs(), got); err != nil || !reflect.DeepEqual(got, w) {
			t.Errorf("%s args = %+v (%v), want %+v", call.GetMethod(), got, err, w)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("calls seen = %v", seen)
	}
}
