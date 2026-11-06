package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/outbox"
	"github.com/danielpina1/athena/internal/pause"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
)

// While paused a mention is stored and queued but not decided, and a reply queued meanwhile is held; resuming lets both
// go (the Decision appears, the reply is sent).
func TestPausedServerStopsDecisions(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	fastEvaluate(t)
	prev := outbox.PausedWait
	outbox.PausedWait = 100 * time.Millisecond
	t.Cleanup(func() { outbox.PausedWait = prev })
	g := newFakeGitHub(t)
	dir := newStateDir(t)
	g.configure(t, dir, 600)
	if err := os.MkdirAll(node.TicketDir(filepath.Join(dir, "work"), core.Ticket{ID: "T-0001", Title: "Fix the flaky test"}), 0o700); err != nil {
		t.Fatal(err)
	}
	s := startServer(t, dir)
	token := routerToken(t, fake)
	eventually(t, "router running and github healthy", func() bool {
		h := s.health()
		return h.Router.State == "running" && len(h.Sources) == 1 && h.Sources[0].Error == ""
	})

	s.ok(Request{Op: OpPause}, nil)
	if h := s.health(); !h.Paused || h.PausedBy != "cli" {
		t.Fatalf("health while paused = %+v", h)
	}
	cid := g.addComment(3, "o", "@athena-bot the login test is flaky, please fix it")
	triggerID := fmt.Sprintf("github:issuecomment:%d", cid)
	thread := github.ThreadKey(fakeRepo, 3)
	eventually(t, "the trigger is stored", func() bool {
		var trs []TriggerView
		s.ok(Request{Op: OpTriggers, Thread: thread}, &trs)
		return len(trs) == 1
	})
	time.Sleep(5 * evaluateEvery)
	var decs []DecisionView
	s.ok(Request{Op: OpDecisions}, &decs)
	if len(decs) != 0 {
		t.Fatalf("decisions while paused = %+v", decs)
	}
	for _, th := range s.threads() {
		if th.Key == thread && th.Eval != nil {
			t.Fatalf("evaluated while paused: %+v", th.Eval)
		}
	}

	s.ok(Request{Op: OpResume}, nil)
	eventually(t, "the candidate reached the router", func() bool {
		for _, th := range s.threads() {
			if th.Key == thread && th.Eval != nil && th.Eval.DeliveredAt != nil {
				return true
			}
		}
		return false
	})

	// A reply queued while paused is held until resume.
	s.ok(Request{Op: OpPause}, nil)
	args, _ := json.Marshal(map[string]any{"decisions": []map[string]any{{"trigger_id": triggerID, "kind": "accept",
		"title": "Fix the flaky test", "ack": "On it: I will fix the flaky login test."}}})
	var rr node.RouterResponse
	nodeCall(t, dir, node.RouterRequest{Op: node.OpRouter, Token: token, Tool: "route", Args: args}, &rr)
	if !rr.OK {
		t.Fatalf("route = %+v %s", rr, rr.Result)
	}
	time.Sleep(600 * time.Millisecond)
	if n := len(g.postedComments()); n != 0 {
		t.Fatalf("%d comments posted while paused", n)
	}
	s.ok(Request{Op: OpResume}, nil)
	eventually(t, "the held reply was sent", func() bool { return len(g.postedComments()) == 1 })
}

// Resuming from the dashboard needs a fresh passkey confirmation; pausing does not.
func TestResumeNeedsReauth(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sw, err := pause.Open(st, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{st: st, pause: sw, log: discardLogger()}
	do := func(id string, a *athenav1.Command, sensitive bool) *athenav1.CommandResult {
		a.Id = id
		return d.Do(context.Background(), web.Origin{ClientID: "phone", Sensitive: sensitive}, a)
	}
	pause := &athenav1.Command{Action: &athenav1.Command_Pause{Pause: &athenav1.Pause{}}}
	resume := func() *athenav1.Command {
		return &athenav1.Command{Action: &athenav1.Command_Resume{Resume: &athenav1.Resume{}}}
	}
	if r := do("p1", proto.Clone(pause).(*athenav1.Command), false); !r.GetOk() || !sw.Paused() || sw.State().By != "web" {
		t.Fatalf("pause without reauth = %v", r)
	}
	if r := do("r1", resume(), false); r.GetCode() != web.CodeReauthRequired || !sw.Paused() {
		t.Fatalf("resume without reauth = %v", r)
	}
	if r := do("r1", resume(), true); !r.GetOk() || sw.Paused() {
		t.Fatalf("resume with reauth = %v", r)
	}
}

// Resume releases the work that was put off at once, even with the production hold (30 s): a reply deferred while paused
// is sent within seconds of the resume, not when its delay ends.
func TestResumeReleasesHeldReplyAtOnce(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	fastEvaluate(t)
	if outbox.PausedWait != 30*time.Second {
		t.Fatalf("PausedWait = %v, want the production 30 s", outbox.PausedWait)
	}
	g := newFakeGitHub(t)
	dir := newStateDir(t)
	g.configure(t, dir, 600)
	if err := os.MkdirAll(node.TicketDir(filepath.Join(dir, "work"), core.Ticket{ID: "T-0001", Title: "Fix the flaky test"}), 0o700); err != nil {
		t.Fatal(err)
	}
	s := startServer(t, dir)
	token := routerToken(t, fake)
	eventually(t, "router running and github healthy", func() bool {
		h := s.health()
		return h.Router.State == "running" && len(h.Sources) == 1 && h.Sources[0].Error == ""
	})
	cid := g.addComment(3, "o", "@athena-bot the login test is flaky, please fix it")
	triggerID := fmt.Sprintf("github:issuecomment:%d", cid)
	thread := github.ThreadKey(fakeRepo, 3)
	eventually(t, "the candidate reached the router", func() bool {
		for _, th := range s.threads() {
			if th.Key == thread && th.Eval != nil && th.Eval.DeliveredAt != nil {
				return true
			}
		}
		return false
	})
	s.ok(Request{Op: OpPause}, nil)
	args, _ := json.Marshal(map[string]any{"decisions": []map[string]any{{"trigger_id": triggerID, "kind": "accept",
		"title": "Fix the flaky test", "ack": "On it: I will fix the flaky login test."}}})
	var rr node.RouterResponse
	nodeCall(t, dir, node.RouterRequest{Op: node.OpRouter, Token: token, Tool: "route", Args: args}, &rr)
	if !rr.OK {
		t.Fatalf("route = %+v %s", rr, rr.Result)
	}
	time.Sleep(1500 * time.Millisecond) // the reply ran once and was put off 30 s
	if n := len(g.postedComments()); n != 0 {
		t.Fatalf("%d comments posted while paused", n)
	}
	s.ok(Request{Op: OpResume}, nil)
	deadline := time.Now().Add(4 * time.Second)
	for len(g.postedComments()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(g.postedComments()); n != 1 {
		t.Fatalf("%d comments posted 4 s after resume, want the held reply", n)
	}
}

// Review Focus 2: a server stopped mid-session while paused starts paused and launches nothing; `athena resume` resumes
// the session through plan 5's restart path (the restart note, the same session).
func TestRestartWhilePausedStaysParked(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	s := startServerWith(t, dir, fastClose)
	id := s.submit("Long task\nbody-p4u5")
	a := s.running(id)
	s.ok(Request{Op: OpPause}, nil)
	s.stop()
	if got := s.interruptions(id); len(got) != 1 || got[0].Reason != core.InterruptRestart || got[0].ResumedAt != nil {
		t.Fatalf("after the stop: interruptions %+v, want one open restart", got)
	}

	s = startServerWith(t, dir, fastClose)
	if h := s.health(); !h.Paused {
		t.Fatalf("health after the restart = %+v, want paused", h)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(fake.invocations(t)); n != 1 {
		t.Fatalf("claude started %d times, want only the first start while paused", n)
	}
	if got := s.interruptions(id); len(got) != 1 || got[0].ResumedAt != nil {
		t.Fatalf("while paused: interruptions %+v, want the restart still open", got)
	}

	s.ok(Request{Op: OpResume}, nil)
	eventually(t, "the session resumed", func() bool { return len(fake.invocations(t)) == 2 })
	if rid, ok := argValue(fake.invocations(t)[1], "--resume"); !ok || rid != fixtureSessionID {
		t.Fatalf("second start args = %v, want --resume %s", fake.invocations(t)[1], fixtureSessionID)
	}
	eventually(t, "the interruption resumed", func() bool {
		got := s.interruptions(id)
		return len(got) == 1 && got[0].ResumedAt != nil
	})
	if in := fake.stdin(t); !strings.Contains(in, `"`+restartText) || strings.Contains(in, recoveryText) {
		t.Fatalf("stdin = %q, want the restart note and no crash note", in)
	}
	if d := s.detail(id); len(d.Assignments) != 1 || d.Assignments[0].ID != a.ID || d.Stage != "working" {
		t.Fatalf("after the resume: stage %q, assignments %+v, want the same Assignment working", d.Stage, d.Assignments)
	}
}

// pausedText opens the first message of a session started again after the pause parked it.
const pausedText = "Athena was paused by its owner at the end of your last turn"

// parkedServer starts a server whose session parks under the pause at the end of its first turn, after filing a
// progress Report when report is set; it returns the server, the fake's logs, the Ticket and its Assignment.
func parkedServer(t *testing.T, report bool) (*testServer, fakeEnv, string, core.Assignment) {
	t.Helper()
	fake := newFakeEnv(t, claudeFixture(t, "simple-turn.jsonl"))
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	dir := newStateDir(t)
	s := startServerWith(t, dir, fastClose)
	id := s.submit("Task\nbody-r3s4")
	a := s.running(id)
	if report {
		var resp node.Response
		nodeCall(t, dir, node.ReportRequest{Op: node.OpReport, Ticket: id, Assignment: a.ID, Status: "progress", Summary: "going"}, &resp)
		if !resp.OK {
			t.Fatalf("report = %+v", resp)
		}
	}
	s.ok(Request{Op: OpPause}, nil)
	if err := os.WriteFile(hold, nil, 0o600); err != nil { // the turn ends
		t.Fatal(err)
	}
	eventually(t, "the session parked", func() bool {
		got := s.interruptions(id)
		return len(got) == 1 && got[0].Reason == core.InterruptPaused && got[0].ResumedAt == nil && got[0].ReportOwed == !report
	})
	time.Sleep(200 * time.Millisecond)
	if n := len(fake.invocations(t)); n != 1 {
		t.Fatalf("claude started %d times while paused, want 1", n)
	}
	return s, fake, id, a
}

// Controller ruling R6: an idle session the pause parked, with nothing waiting for it, is not started on resume. The
// pause closes (Paused by the owner, then Resumed) and the Assignment waits, running, for its next Input, which resumes
// its session.
func TestResumeLeavesIdleParkedUntilInput(t *testing.T) {
	s, fake, id, a := parkedServer(t, true)
	s.ok(Request{Op: OpResume}, nil)
	eventually(t, "the pause closed by the resume", func() bool {
		got := s.interruptions(id)
		return len(got) == 1 && got[0].Reason == core.InterruptPaused && got[0].ResumedAt != nil
	})
	time.Sleep(300 * time.Millisecond)
	if n := len(fake.invocations(t)); n != 1 {
		t.Fatalf("claude started %d times after the resume of an idle session, want 1", n)
	}
	if d := s.detail(id); len(d.Assignments) != 1 || d.Assignments[0].ID != a.ID || d.Assignments[0].State != core.AssignmentRunning {
		t.Fatalf("after the resume: %+v, want the same Assignment running", d.Assignments)
	}

	s.ok(Request{Op: OpReply, ID: id, Text: "after-resume-5d6e"}, nil)
	eventually(t, "the input resumed the session", func() bool { return len(fake.invocations(t)) == 2 })
	if rid, ok := argValue(fake.invocations(t)[1], "--resume"); !ok || rid != fixtureSessionID {
		t.Fatalf("second start args = %v, want --resume %s", fake.invocations(t)[1], fixtureSessionID)
	}
	eventually(t, "the input sent", func() bool { return strings.Contains(fake.stdin(t), "after-resume-5d6e") })
	if in := fake.stdin(t); !strings.Contains(in, resumeText) || strings.Contains(in, pausedText) {
		t.Fatalf("stdin = %q, want the resume note of an idle session, not the pause note", in)
	}
	if d := s.detail(id); len(d.Assignments) != 1 || d.Assignments[0].ID != a.ID || d.Stage != "working" {
		t.Fatalf("after the input: stage %q, assignments %+v, want the same Assignment working", d.Stage, d.Assignments)
	}
}

// A session parked owing its Report (its turn ended without one while paused) is started again on resume, on the same
// Assignment, with the pause note asking for it. The pause closes as resumed.
func TestResumeRelaunchesParkedOwingReport(t *testing.T) {
	s, fake, id, a := parkedServer(t, false)
	s.ok(Request{Op: OpResume}, nil)
	eventually(t, "the session started again", func() bool { return len(fake.invocations(t)) == 2 })
	if rid, ok := argValue(fake.invocations(t)[1], "--resume"); !ok || rid != fixtureSessionID {
		t.Fatalf("second start args = %v, want --resume %s", fake.invocations(t)[1], fixtureSessionID)
	}
	eventually(t, "the pause note sent", func() bool { return strings.Contains(fake.stdin(t), pausedText) })
	eventually(t, "the pause closed by the start", func() bool {
		got := s.interruptions(id)
		return len(got) == 1 && got[0].Reason == core.InterruptPaused && got[0].ResumedAt != nil
	})
	if d := s.detail(id); len(d.Assignments) != 1 || d.Assignments[0].ID != a.ID || d.Stage != "working" {
		t.Fatalf("after the resume: stage %q, assignments %+v, want the same Assignment working", d.Stage, d.Assignments)
	}
}
