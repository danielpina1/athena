package core

import "testing"

// The pause records its own Interruption, with whether the parked turn owed a Report; lifting it without a start closes
// it as resumed, and only a pause.
func TestAssignmentPausedAndLifted(t *testing.T) {
	h := newHarness(t)
	id := h.accepted()
	a := h.ticket(id).Assignments[0].ID
	h.tick(1)
	h.must(h.eng.AssignmentStarted(id, a, "sess", "", ""))
	h.tick(1)
	h.must(h.eng.AssignmentPaused(id, a, true))
	got := h.ticket(id).Assignments[0]
	eq(t, "paused", got.Interruptions, []Interruption{{At: base.Add(2e9), Reason: InterruptPaused, ReportOwed: true}})
	h.must(h.eng.AssignmentPaused(id, a, false))
	eq(t, "still one", len(h.ticket(id).Assignments[0].Interruptions), 1)
	h.tick(1)
	h.must(h.eng.PauseLifted(id, a))
	lifted := base.Add(3e9)
	got = h.ticket(id).Assignments[0]
	eq(t, "lifted", got.Interruptions[0].ResumedAt, &lifted)
	eq(t, "no open one", got.OpenInterruption(), (*Interruption)(nil))
	eq(t, "still running", got.State, AssignmentRunning)

	// A restart's interruption is not the pause's to lift.
	h.tick(1)
	h.must(h.eng.AssignmentInterrupted(id, a, InterruptRestart))
	h.must(h.eng.PauseLifted(id, a))
	eq(t, "restart stays open", h.ticket(id).Assignments[0].OpenInterruption().Reason, InterruptRestart)
}
