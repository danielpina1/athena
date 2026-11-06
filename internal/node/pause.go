package node

import "github.com/danielpina1/athena/internal/core"

// The kill switch on the node (the owner's `athena pause`): while paused no queued launch starts, and every live
// session parks at the end of its current turn, an idle one at once. Parking ends the process the way a handoff does:
// the Assignment runs on with its session, and an Interruption with reason paused records why it has no process. The
// server starts parked Assignments again on resume, through the path a restart uses.

// pauseNote opens the first message of a session started again after the owner's pause parked it.
const pauseNote = "Athena was paused by its owner at the end of your last turn and has resumed; continue the task if " +
	"anything is left, and report your status."

// pausedWhy is why a launch that was about to start waits.
const pausedWhy = "athena is paused"

// SetPaused sets the node's kill switch. Paused, no queued launch starts and every live session parks at the end of its
// current turn (an idle one now); resumed, the queue launches again. Parked Assignments are started again by the
// caller (Start), as after a restart.
func (n *Node) SetPaused(paused bool) {
	if n.paused.Swap(paused) == paused {
		return
	}
	if !paused {
		n.post(message{kind: msgWake})
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, s := range n.live {
		s.pokePause()
	}
}

// Paused reports whether the node's kill switch is set.
func (n *Node) Paused() bool { return n.paused.Load() }

// pokePause wakes the session's runner to park it if it is idle.
func (s *session) pokePause() {
	select {
	case s.pauseCh <- struct{}{}:
	default:
	}
}

func (s *session) markPauseParked() {
	s.mu.Lock()
	s.pauseParked, s.parked = true, true
	s.mu.Unlock()
}

func (s *session) unmarkPauseParked() {
	s.mu.Lock()
	s.pauseParked, s.parked = false, false
	s.mu.Unlock()
}

// startWhenLeft notes a Start of the session's Assignment while the session is live: one the pause parked is closing,
// so it starts again once it left (onEnded). It changes nothing for any other session.
func (s *session) startWhenLeft() {
	s.mu.Lock()
	if s.pauseParked {
		s.startAgain = true
	}
	s.mu.Unlock()
}

func (s *session) startsWhenLeft() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startAgain
}

// owesReportAtPause decides, at a turn end, whether the session parks owing its Report: while paused, a turn that ended
// without the Report it owed, and was not nudged yet, is not nudged into a new turn. It reports whether the nudge is
// skipped; the pause records the Report owed, and the relaunch's pause note asks for it.
func (r *runner) owesReportAtPause() bool {
	r.reportOwed = r.n.Paused() && !r.nudged && r.s.reportCount() <= r.inputReports
	return r.reportOwed
}

// parkIfPaused parks the session while the node is paused and its turn is over with nothing sent since: it records the
// turn's end, then the pause (AssignmentPaused, with whether the turn owed a Report), so a restart sees the Interruption
// after the idle turn; the runner ends the process. A turn that waits on an Approval is not over: it parks when it ends.
func (r *runner) parkIfPaused() {
	s := r.s
	if !r.n.Paused() || !r.idle || r.park || !r.initSeen || !r.started || s.isStopped() {
		return
	}
	// Marked first: a resume's Start that comes once the pause is recorded finds the session parked (startWhenLeft).
	s.markPauseParked()
	if err := r.eng.TurnEnded(s.t.ID, s.a.ID); err != nil {
		r.log.Error("record turn end", "error", err)
		s.unmarkPauseParked()
		return
	}
	if err := r.eng.AssignmentPaused(s.t.ID, s.a.ID, r.reportOwed); err != nil {
		r.log.Error("record pause", "error", err)
		s.unmarkPauseParked()
		return
	}
	r.log.Info("athena is paused; session parked at the end of its turn", "reportOwed", r.reportOwed)
	r.park = true
}

// putOffForPause puts off a launch about to start its first process while the node is paused: the runner ends and the loop
// queues it again, held until the resume.
func (r *runner) putOffForPause() bool {
	if !r.n.Paused() {
		return false
	}
	r.log.Info("launch held: " + pausedWhy)
	r.s.setLaunchWait(minLaunchWait, pausedWhy)
	return true
}

// pausedByOwner reports whether the Ticket's Assignment id has an open Interruption the owner's pause caused.
func pausedByOwner(t core.Ticket, id string) bool {
	for i := range t.Assignments {
		if a := &t.Assignments[i]; a.ID == id {
			in := a.OpenInterruption()
			return in != nil && in.Reason == core.InterruptPaused
		}
	}
	return false
}
