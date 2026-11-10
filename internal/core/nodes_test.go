package core

import (
	"reflect"
	"testing"
)

func TestAssignmentPlacedRecordsNode(t *testing.T) {
	h := newHarness(t)
	tk := h.submit("work")
	aid := tk.Assignments[0].ID
	h.must(h.eng.AssignmentPlaced(tk.ID, aid, "n1"))
	eq(t, "node", h.ticket(tk.ID).Assignments[0].Node, "n1")

	h.settle(true)
	h.must(h.eng.StopTicket(tk.ID, "x"))
	h.must(h.eng.AssignmentPlaced(tk.ID, aid, "n2"))
	eq(t, "ended assignment keeps its node", h.ticket(tk.ID).Assignments[0].Node, "n1")
}

func TestNodeOfflineStageAndBack(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.AssignmentPlaced(id, h.ticket(id).Assignments[0].ID, "n1"))
	other := h.submit("other")
	h.must(h.eng.AssignmentPlaced(other.ID, other.Assignments[0].ID, "n2"))
	before := len(h.eventsOf(EventNodeOffline))

	h.must(h.eng.NodeOffline("n1", base))
	eq(t, "stage", stageOf(h.ticket(id)), Stage{"person", "offline"})
	if h.ticket(id).Assignments[0].NodeOffline == nil {
		t.Fatal("NodeOffline not set")
	}
	if h.ticket(other.ID).Assignments[0].NodeOffline != nil {
		t.Error("other node's assignment flagged")
	}
	evs := h.eventsOf(EventNodeOffline)
	eq(t, "events", len(evs)-before, 1)
	eq(t, "event data", evs[len(evs)-1].Data["node"], any("n1"))

	h.must(h.eng.NodeBack("n1"))
	eq(t, "stage after back", stageOf(h.ticket(id)), Stage{Stage: "working"})
	eq(t, "back events", len(h.eventsOf(EventNodeBack)), 1)
}

func TestRetryElsewhereEndsAndAvoids(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	h.must(h.eng.AssignmentPlaced(id, h.ticket(id).Assignments[0].ID, "n2"))
	h.must(h.eng.NodeOffline("n2", base))
	h.must(h.eng.RetryElsewhere(id))

	tk := h.ticket(id)
	eq(t, "assignments", len(tk.Assignments), 2)
	eq(t, "old state", tk.Assignments[0].State, AssignmentEnded)
	eq(t, "old reason", tk.Assignments[0].EndedReason, ReasonNodeOffline)
	eq(t, "new state", tk.Assignments[1].State, AssignmentPending)
	if !reflect.DeepEqual(tk.Assignments[1].Avoid, []string{"n2"}) {
		t.Errorf("avoid = %v", tk.Assignments[1].Avoid)
	}
	if tk.Assignments[1].NodeOffline != nil || tk.Assignments[1].Node != "" {
		t.Error("new assignment must start unplaced and online")
	}
}

func TestRetryElsewhereRefusedWhenOnline(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	wantUserError(t, "online", h.eng.RetryElsewhere(id))
	h.must(h.eng.AssignmentPlaced(id, h.ticket(id).Assignments[0].ID, "n1"))
	wantUserError(t, "placed but online", h.eng.RetryElsewhere(id))
}

func TestPinStoredOnSubmit(t *testing.T) {
	h := newHarness(t)
	tk, err := h.eng.SubmitOwnerTask("pinned", TaskOptions{Dir: "/w", Node: "n3"})
	h.must(err)
	eq(t, "pin", h.ticket(tk.ID).Execution.Node, "n3")
}
