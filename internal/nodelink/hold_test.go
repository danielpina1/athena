package nodelink

import (
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// With HoldUntilServing, the server's starts and delivers that come before the node serves its socket wait for
// Serving, in the order they came; a stop of a held start cancels it; other calls are not held. Ready alone does not
// open the gate: the socket must serve first.
func TestStartsHeldUntilServing(t *testing.T) {
	srv := newServers(t)
	id := &Identity{NodeID: "n0123456789", Name: "mini", NodeURL: "wss://athena.test:8444/node"}
	c, err := New(id, Options{StateDir: t.TempDir(), Version: "test", Dial: srv.dial, MinBackoff: 5 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond, HoldUntilServing: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	tgt := &fakeTarget{}
	c.attach(tgt)
	var give atomic.Bool
	fs := srv.add(0)
	fs.answer = func(call *athenav1.NodeCall) *athenav1.NodeResult {
		if call.GetMethod() != MethodPrompts {
			return nil
		}
		if !give.Load() {
			return &athenav1.NodeResult{Error: "not yet"}
		}
		b, _ := json.Marshal(ResultPrompts{Worker: "W", Reviewer: "R"})
		return &athenav1.NodeResult{Value: b}
	}
	run(t, c)
	eventually(t, "online", c.Online)

	// Between the Welcome and the prompts' answer.
	tk := func(id string) core.Ticket { return core.Ticket{ID: id} }
	as := func(id string) core.Assignment { return core.Assignment{ID: id} }
	fs.call(t, 1, ServeStart, ArgsStart{Ticket: tk("T1"), Assignment: as("A1")})
	fs.call(t, 2, ServeDeliver, ArgsDeliver{Ticket: tk("T1"), Assignment: as("A1")})
	fs.call(t, 3, ServeStart, ArgsStart{Ticket: tk("T2"), Assignment: as("A2")})
	fs.call(t, 4, ServeStop, ArgsStop{Ticket: tk("T2"), Assignment: as("A2"), Reason: "moved"})
	fs.call(t, 5, ServeSetPaused, ArgsSetPaused{Paused: false})
	eventually(t, "every call answered", func() bool {
		for id := uint64(1); id <= 5; id++ {
			if fs.result(id) == nil {
				return false
			}
		}
		return true
	})
	for id := uint64(1); id <= 5; id++ {
		if e := fs.result(id).GetError(); e != "" {
			t.Fatalf("call %d: %s", id, e)
		}
	}
	// The stop and setPaused went through; nothing started or was delivered.
	if got, want := tgt.seen(), []string{"stop T2/A2", "paused false"}; !slices.Equal(got, want) {
		t.Fatalf("before the prompts: %v, want %v", got, want)
	}
	give.Store(true)
	select {
	case <-c.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	time.Sleep(20 * time.Millisecond)
	if got := tgt.seen(); len(got) != 2 {
		t.Fatalf("ready but not serving: %v", got)
	}
	c.Serving()
	if got, want := tgt.seen(), []string{"stop T2/A2", "paused false", "start T1/A1", "deliver T1/A1"}; !slices.Equal(got, want) {
		t.Fatalf("after Serving: %v, want %v", got, want)
	}
	// Once serving, a start goes straight to the node.
	fs.call(t, 6, ServeStart, ArgsStart{Ticket: tk("T3"), Assignment: as("A3")})
	eventually(t, "T3 started", func() bool { return slices.Contains(tgt.seen(), "start T3/A3") })
}
