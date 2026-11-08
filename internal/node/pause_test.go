package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// parkedByPause reports whether the Ticket's latest Assignment still runs, with its session, and has an open
// Interruption with reason paused.
func (h *harness) parkedByPause(id string) bool {
	tk, _ := h.ticket(id)
	a := tk.LatestAssignment()
	i := a.OpenInterruption()
	return a.State == core.AssignmentRunning && a.SessionID == fixtureSessionID && i != nil && i.Reason == core.InterruptPaused
}

// turnEnded reports whether the Ticket's transcript shows a turn's result.
func (h *harness) turnEnded(id string) bool {
	b, _ := os.ReadFile(h.n.transcriptPath(id))
	return strings.Contains(string(b), `"type":"result"`)
}

// While paused no queued launch starts; once resumed, both go.
func TestPausedNodeLaunchesNothing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Slots = 2 })
	h.n.SetPaused(true)
	if !h.n.Paused() {
		t.Fatal("Paused() = false after SetPaused(true)")
	}
	a, b := h.submit("One", t.TempDir()), h.submit("Two", t.TempDir())
	h.never("a launch while paused", 200*time.Millisecond, func() bool { return h.n.liveCount() > 0 })
	if n := len(h.invocations()); n != 0 {
		t.Fatalf("claude started %d times while paused", n)
	}
	h.n.SetPaused(false)
	h.running(a.ID)
	h.running(b.ID)
}

// A session mid-turn when the node pauses keeps its process until the turn ends, then parks: its Assignment runs on
// with its session and an open Interruption with reason paused. Started again after the resume, it continues the same
// session with the pause note.
func TestPauseParksAtTurnEnd(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	h := newHarness(t, func(c *Config) { c.CloseGrace = 300 * time.Millisecond })
	tk := h.submit("Task", t.TempDir())
	_, a := h.running(tk.ID)
	if r := h.report(tk.ID, a.ID, core.ReportProgress, "going"); !r.OK {
		t.Fatalf("report = %+v", r)
	}
	h.n.SetPaused(true)
	h.never("the session parked mid-turn", 200*time.Millisecond, func() bool {
		s := h.n.liveSession(tk.ID)
		return s == nil || s.process() == nil
	})
	if h.parkedByPause(tk.ID) {
		t.Fatal("interruption recorded before the turn ended")
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil { // the turn ends
		t.Fatal(err)
	}
	h.eventually("the session parked", func() bool { return h.n.liveSession(tk.ID) == nil })
	if !h.parkedByPause(tk.ID) {
		got, _ := h.ticket(tk.ID)
		t.Fatalf("assignment after parking = %+v, want running with its session and an open paused interruption", got.LatestAssignment())
	}

	h.n.SetPaused(false)
	got, _ := h.ticket(tk.ID)
	h.n.Start(got, *got.LatestAssignment())
	h.eventually("the session resumed", func() bool { n, _ := h.starts(tk.ID); return n == 2 })
	if _, args := h.starts(tk.ID); !strings.Contains(strings.Join(args, " "), "--resume "+fixtureSessionID) {
		t.Fatalf("resumed with %v, want --resume %s", args, fixtureSessionID)
	}
	h.eventually("the pause note sent", func() bool { return strings.Contains(h.stdin(), pauseNote) })
	if in := h.stdin(); strings.Contains(in, recoveryNote) || strings.Contains(in, restartNote) {
		t.Fatalf("stdin = %q, want the pause note only", in)
	}
	h.eventually("the interruption closed", func() bool {
		got, _ := h.ticket(tk.ID)
		l := got.LatestAssignment()
		return l.ID == a.ID && len(l.Interruptions) == 1 && l.Interruptions[0].ResumedAt != nil
	})
}

// A turn that ends without a Report while paused is not nudged into a new turn: the session parks owing the Report,
// which the pause records so the resume asks for it.
func TestPausedTurnWithoutReportParksUnnudged(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	h := newHarness(t, func(c *Config) { c.CloseGrace = 300 * time.Millisecond })
	tk := h.submit("Task", t.TempDir())
	h.running(tk.ID)
	h.n.SetPaused(true)
	if err := os.WriteFile(hold, nil, 0o600); err != nil { // the turn ends, no Report filed
		t.Fatal(err)
	}
	h.eventually("the session parked", func() bool { return h.n.liveSession(tk.ID) == nil && h.parkedByPause(tk.ID) })
	if strings.Contains(h.stdin(), nudgeText) {
		t.Fatalf("stdin = %q, want no nudge while paused", h.stdin())
	}
	if n, _ := h.starts(tk.ID); n != 1 {
		t.Fatalf("claude started %d times, want 1", n)
	}
	got, _ := h.ticket(tk.ID)
	if i := got.LatestAssignment().OpenInterruption(); i == nil || !i.ReportOwed {
		t.Fatalf("interruption = %+v, want the pause owing a Report", i)
	}
}

// A Start that arrives while the session the pause parked is still leaving starts it again once it left.
func TestStartWhileParkedSessionLeavesRelaunches(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", t.TempDir())
	h.running(tk.ID)
	s := h.n.liveSession(tk.ID)
	s.markPauseParked()
	got, _ := h.ticket(tk.ID)
	h.n.Start(got, *got.LatestAssignment())
	h.eventually("the start noted on the live session", func() bool { return s.startsWhenLeft() })
	if n, _ := h.starts(tk.ID); n != 1 {
		t.Fatalf("claude started %d times while the session was live, want 1", n)
	}
}

// An idle session (its turn ended well, nothing sent since) parks as soon as the node pauses. An Input that arrives
// while it is parked waits for the resume, then reaches the relaunched session.
func TestPauseParksIdleSessionAtOnce(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	h := newHarness(t, func(c *Config) { c.CloseGrace = 300 * time.Millisecond })
	tk := h.submit("Task", t.TempDir())
	_, a := h.running(tk.ID)
	if r := h.report(tk.ID, a.ID, core.ReportProgress, "going"); !r.OK {
		t.Fatalf("report = %+v", r)
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.eventually("the turn ended", func() bool { return h.turnEnded(tk.ID) })
	if h.n.liveSession(tk.ID) == nil {
		t.Fatal("the idle session left before the pause")
	}
	h.n.SetPaused(true)
	h.eventually("the idle session parked", func() bool { return h.n.liveSession(tk.ID) == nil && h.parkedByPause(tk.ID) })

	if err := h.eng.AddPersonInput(tk.ID, "while-paused-7e1f"); err != nil {
		t.Fatal(err)
	}
	h.never("a launch while paused", 300*time.Millisecond, func() bool { return h.n.liveCount() > 0 })
	if strings.Contains(h.stdin(), "while-paused-7e1f") {
		t.Fatal("the input reached a session while paused")
	}
	h.n.SetPaused(false)
	h.eventually("the input reached the resumed session", func() bool { return strings.Contains(h.stdin(), "while-paused-7e1f") })
	if n, args := h.starts(tk.ID); n != 2 || !strings.Contains(strings.Join(args, " "), "--resume "+fixtureSessionID) {
		t.Fatalf("starts = %d, last %v; want a second start resuming the session", n, args)
	}
}

// Review Focus 1: a session waiting on an Approval is mid-turn. The pause leaves it running: the person's answer still
// reaches it, and it parks once that turn ends.
func TestPauseParksAfterApprovalTurn(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	h := newHarness(t, func(c *Config) { c.CloseGrace = 300 * time.Millisecond })
	tk := h.submit("Task", t.TempDir())
	_, a := h.running(tk.ID)
	if r := h.report(tk.ID, a.ID, core.ReportProgress, "going"); !r.OK {
		t.Fatalf("report = %+v", r)
	}
	input := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "make deploy"}}
	got := make(chan HookResponse, 1)
	go func() { got <- h.hook(EventPreToolUse, tk.ID, a.ID, input) }()
	var pending []core.Approval
	h.eventually("approval pending", func() bool {
		var err error
		pending, err = h.eng.PendingApprovals()
		return err == nil && len(pending) == 1
	})
	h.n.SetPaused(true)
	h.never("the session parked while its approval waits", 200*time.Millisecond, func() bool {
		s := h.n.liveSession(tk.ID)
		return s == nil || s.process() == nil
	})
	if err := h.eng.AnswerApproval(pending[0].ID, true, "go ahead"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.Verdict != VerdictAllow {
			t.Fatalf("verdict = %+v, want allow", r)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the approval's answer never reached the session")
	}
	if h.n.liveSession(tk.ID) == nil {
		t.Fatal("the session parked before its turn ended")
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil { // the approved turn ends
		t.Fatal(err)
	}
	h.eventually("the session parked after its turn", func() bool { return h.n.liveSession(tk.ID) == nil && h.parkedByPause(tk.ID) })
}

// A launch taken off the queue whose process has not started when the node pauses is put off: it goes back to the queue
// (a launch wait) instead of starting a turn.
func TestPauseBeforeSpawnPutsTheLaunchOff(t *testing.T) {
	h := newHarness(t, nil)
	s := h.n.newSession(core.Ticket{ID: "T-1"}, core.Assignment{ID: "a"}, placement{})
	r := &runner{n: h.n, s: s, log: s.log}
	if r.putOffForPause() || s.launchWaitOf().waits() {
		t.Fatal("a launch put off while not paused")
	}
	h.n.SetPaused(true)
	if !r.putOffForPause() || !s.launchWaitOf().waits() {
		t.Fatalf("launch wait while paused = %+v, want the launch put off", s.launchWaitOf())
	}
}

// A session whose process crashes while the node is paused is not started again: it parks owing its Report, and only
// the resume starts it, with the pause note.
func TestCrashWhilePausedParks(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.CloseGrace = 300 * time.Millisecond })
	t.Setenv("ATHENA_FAKE_EXIT_MID_TURN_ONCE", filepath.Join(t.TempDir(), "crashed"))
	// The owner pauses just as the process dies.
	h.n.restartDelay = func(int) time.Duration { h.n.SetPaused(true); return 0 }
	tk := h.submit("Crashy task", "")
	h.eventually("the crashed session parked", func() bool { return h.n.liveSession(tk.ID) == nil && h.parkedByPause(tk.ID) })
	h.never("a start while paused", 300*time.Millisecond, func() bool { return len(h.invocations()) > 1 })
	got, _ := h.ticket(tk.ID)
	if i := got.LatestAssignment().OpenInterruption(); i == nil || !i.ReportOwed {
		t.Fatalf("interruption = %+v, want the pause owing a Report", i)
	}
	if r := got.LatestAssignment().Report; r != nil && r.State == core.ReportFailed {
		t.Fatalf("report = %+v, want no failure", r)
	}

	h.n.restartDelay = func(int) time.Duration { return 0 }
	h.n.SetPaused(false)
	h.n.Start(got, *got.LatestAssignment())
	h.eventually("started again after the resume", func() bool { return len(h.invocations()) == 2 })
	h.eventually("the pause note sent", func() bool { return strings.Contains(h.stdin(), pauseNote) })
}

// While paused a busy session is sent no new Input: it waits, undelivered. Resumed before the turn ends, the Input
// reaches the session once the turn is over.
func TestPausedBusySessionGetsNoInputs(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	h := newHarness(t, func(c *Config) { c.CloseGrace = 300 * time.Millisecond })
	tk := h.submit("Task", t.TempDir())
	h.running(tk.ID)
	h.n.SetPaused(true)
	if err := h.eng.AddPersonInput(tk.ID, "while-paused-4c2a"); err != nil {
		t.Fatal(err)
	}
	h.never("the input reached the busy session while paused", 300*time.Millisecond, func() bool {
		return strings.Contains(h.stdin(), "while-paused-4c2a")
	})
	if got, _ := h.ticket(tk.ID); len(got.Undelivered) != 1 {
		t.Fatalf("undelivered = %v, want the input waiting", got.Undelivered)
	}
	h.n.SetPaused(false)
	if err := os.WriteFile(hold, nil, 0o600); err != nil { // the turn ends
		t.Fatal(err)
	}
	h.eventually("the input reached the session after the resume", func() bool {
		got, _ := h.ticket(tk.ID)
		return strings.Contains(h.stdin(), "while-paused-4c2a") && len(got.Undelivered) == 0
	})
	if n, _ := h.starts(tk.ID); n != 1 {
		t.Fatalf("claude started %d times, want the same process", n)
	}
}

// A handed-off session whose turn ends under the pause owing a Report records the pause: the handoff park alone would
// lose the Report, and the resume would not start the session to ask for it.
func TestPausedHandedOffSessionRecordsOwedReport(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_FIXTURE", openTurnFixture(t))
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	if err := h.eng.BindThread(tk.ID, "github:o/r#1", true); err != nil {
		t.Fatal(err)
	}
	if got := h.report(tk.ID, a.ID, core.ReportReady, "PR is up"); !got.OK {
		t.Fatalf("report = %+v", got)
	}
	s := h.n.lookup(tk.ID, a.ID)
	h.eventually("the handoff seen", s.handedOff)
	var sent []string
	r := &runner{n: h.n, s: s, eng: h.eng, log: s.log, sent: map[string]bool{}, started: true, initSeen: true,
		send: func(text string) error { sent = append(sent, text); return nil }}
	r.inputSent()
	h.n.paused.Store(true)
	defer h.n.paused.Store(false)
	r.onEvent(rt.TurnEnd{OK: true, Subtype: "success"})
	got, _ := h.ticket(tk.ID)
	if i := got.LatestAssignment().OpenInterruption(); i == nil || i.Reason != core.InterruptPaused || !i.ReportOwed {
		t.Fatalf("interruption = %+v, want the pause owing a Report", i)
	}
	if !r.park || len(sent) != 0 {
		t.Fatalf("park %v, sent %q", r.park, sent)
	}
}

// A resume that lands between the turn's end and its parking leaves no owed Report behind: the turn is checked as if
// the pause had never been, and the worker is nudged.
func TestResumeBeforeParkChecksTheOwedTurn(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_FIXTURE", openTurnFixture(t))
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	s := h.n.lookup(tk.ID, a.ID)
	var sent []string
	r := &runner{n: h.n, s: s, eng: h.eng, log: s.log, sent: map[string]bool{}, started: true, initSeen: true,
		send: func(text string) error { sent = append(sent, text); return nil }}
	r.inputSent()
	r.idle, r.reportOwed = true, true // the turn ended owing, then the resume came
	r.parkOwing()
	if len(sent) != 1 || sent[0] != nudgeText || r.reportOwed || r.park {
		t.Fatalf("sent %q, reportOwed %v, park %v; want one nudge and nothing owed", sent, r.reportOwed, r.park)
	}
}
