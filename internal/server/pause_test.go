package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
