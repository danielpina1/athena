package nodelink

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

func TestFactsReplayOnceInOrder(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	eng := c.Engine()
	// Three facts while offline: each returns once its row is committed.
	if err := eng.TurnEnded("T1", "A1"); err != nil {
		t.Fatal(err)
	}
	if err := eng.InputsRead("T1", []string{"I1", "I2"}); err != nil {
		t.Fatal(err)
	}
	if err := eng.AssignmentPaused("T1", "A1", true); err != nil {
		t.Fatal(err)
	}

	first := srv.add(1)
	run(t, c)
	eventually(t, "facts 2 and 3", func() bool { return len(first.facts()) >= 2 })
	time.Sleep(50 * time.Millisecond) // nothing more comes
	got := first.facts()
	if len(got) != 2 || got[0].GetFactSeq() != 2 || got[1].GetFactSeq() != 3 {
		t.Fatalf("facts = %v, want seqs 2 and 3 once", seqs(got))
	}
	if got[0].GetMethod() != MethodInputsRead || got[1].GetMethod() != MethodAssignmentPaused {
		t.Fatalf("methods = %s, %s", got[0].GetMethod(), got[1].GetMethod())
	}
	var a ArgsAssignmentPaused
	dec := json.NewDecoder(bytes.NewReader(got[1].GetArgs()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil || a != (ArgsAssignmentPaused{TicketID: "T1", AssignmentID: "A1", ReportOwed: true}) {
		t.Fatalf("args = %+v, %v", a, err)
	}
	if !IsFact(MethodAssignmentPaused) || IsFact(MethodFileReport) {
		t.Fatal("IsFact")
	}

	// Reconnect with the server at 3: nothing is resent.
	second := srv.add(3)
	first.close()
	eventually(t, "the second link", func() bool {
		second.mu.Lock()
		defer second.mu.Unlock()
		return second.hello != nil
	})
	eventually(t, "online again", c.Online)
	time.Sleep(100 * time.Millisecond)
	if f := second.facts(); len(f) != 0 {
		t.Fatalf("resent %v", seqs(f))
	}
	// A new fact after the replay is numbered after it.
	if err := eng.ExpireApproval("AP1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "fact 4", func() bool { return len(second.facts()) == 1 })
	if f := second.facts()[0]; f.GetFactSeq() != 4 || f.GetMethod() != MethodExpireApproval {
		t.Fatalf("fact = %d %s", f.GetFactSeq(), f.GetMethod())
	}
}

func seqs(calls []*athenav1.NodeCall) []uint64 {
	var out []uint64
	for _, c := range calls {
		out = append(out, c.GetFactSeq())
	}
	return out
}

func TestRequestOfflineFailsFast(t *testing.T) {
	c := newTestClient(t, newServers(t))
	start := time.Now()
	err := c.Engine().FileReport("T1", core.ReportInput{AssignmentID: "A1", State: core.ReportReady, Summary: "done"})
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("took %v", d)
	}
	if !errors.Is(err, ErrOffline) || err.Error() != "the control server is unreachable; wait a minute and try again" {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := c.Credentials().Token(t.Context(), "o/r", true); !errors.Is(err, ErrOffline) {
		t.Fatalf("token err = %v", err)
	}
}

func TestPolicyFailsClosedOffline(t *testing.T) {
	p := newTestClient(t, newServers(t)).Policy()
	if got := p.ToolPolicy("owner"); got != core.ToolRestricted {
		t.Fatalf("ToolPolicy = %q", got)
	}
	if p.Allows("Bash", "git push", "o/r") {
		t.Fatal("Allows offline")
	}
	if !p.Shadow("o/r") {
		t.Fatal("Shadow offline is false")
	}
	if got := p.Trust(core.Trigger{}); got != core.TrustStranger {
		t.Fatalf("Trust = %q", got)
	}
}

func TestTicketFromCacheOffline(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	tgt := &fakeTarget{}
	c.attach(tgt)
	fs := srv.add(0)
	fs.answer = func(call *athenav1.NodeCall) *athenav1.NodeResult {
		if call.GetMethod() == MethodTicket {
			return &athenav1.NodeResult{Error: "ticket T9 not found", UserError: true}
		}
		return nil
	}
	run(t, c)
	eventually(t, "online", c.Online)
	fs.call(t, 1, ServeStart, ArgsStart{Ticket: core.Ticket{ID: "T1", Title: "fix the build"}, Assignment: core.Assignment{ID: "A1"}})
	eventually(t, "the start result", func() bool { return fs.result(1) != nil })
	if r := fs.result(1); r.GetError() != "" {
		t.Fatalf("start: %s", r.GetError())
	}
	if len(tgt.starts) != 1 || tgt.starts[0].ID != "T1" {
		t.Fatalf("starts = %v", tgt.starts)
	}
	// Online, Ticket is a request.
	var ue *core.UserError
	if _, _, err := c.Engine().Ticket("T9"); !errors.As(err, &ue) {
		t.Fatalf("online Ticket err = %v", err)
	}

	fs.close() // no server waits for the reconnect: the client stays offline

	eventually(t, "offline", func() bool { return !c.Online() })
	tk, st, err := c.Engine().Ticket("T1")
	if err != nil || tk.Title != "fix the build" || st != core.TicketStage(&tk) {
		t.Fatalf("cached Ticket = %+v %+v %v", tk, st, err)
	}
	if _, _, err := c.Engine().Ticket("T2"); !errors.Is(err, ErrOffline) {
		t.Fatalf("unseen Ticket err = %v", err)
	}
}

func TestRequestRoundTrip(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	fs := srv.add(0)
	fs.answer = func(call *athenav1.NodeCall) *athenav1.NodeResult {
		switch call.GetMethod() {
		case MethodHandback:
			var a ArgsHandback
			dec := json.NewDecoder(bytes.NewReader(call.GetArgs()))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&a); err != nil || a.TriggerID != "TR1" {
				return &athenav1.NodeResult{Error: "bad args"}
			}
			return &athenav1.NodeResult{Value: []byte("true")}
		case MethodToken:
			return &athenav1.NodeResult{Error: "repository gone", UserError: true}
		case MethodPrompts:
			b, _ := json.Marshal(ResultPrompts{Worker: "W", Reviewer: "R"})
			return &athenav1.NodeResult{Value: b}
		}
		return nil
	}
	run(t, c)
	select {
	case <-c.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	if c.WorkerPrompt() != "W" || c.ReviewerPrompt() != "R" {
		t.Fatalf("prompts %q %q", c.WorkerPrompt(), c.ReviewerPrompt())
	}
	ok, err := c.Engine().Handback("T1", "TR1", "not mine")
	if err != nil || !ok {
		t.Fatalf("Handback = %v %v", ok, err)
	}
	if _, _, err := c.Credentials().Token(t.Context(), "o/r", true); err == nil || !isPermanent(err) {
		t.Fatalf("token err = %v, want permanent", err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, call := range fs.calls {
		if call.GetFactSeq() != 0 && !IsFact(call.GetMethod()) {
			t.Fatalf("request %s with fact_seq", call.GetMethod())
		}
	}
	if fs.hello.GetProtocol() != Protocol || fs.hello.GetNodeId() != "n0123456789" {
		t.Fatalf("hello = %v", fs.hello)
	}
	if !slices.Contains(Methods(), MethodHandback) {
		t.Fatal("Methods")
	}
}
