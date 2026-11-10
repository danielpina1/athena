package nodelink

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

// replayBacklog is more facts than one channel queue holds (256), so a replay meets backpressure.
const replayBacklog = 400

func recordFacts(t *testing.T, c *Client, n int) {
	t.Helper()
	for i := range n {
		if err := c.Engine().TurnEnded("T1", "A"+string(rune('a'+i%26))); err != nil {
			t.Fatal(err)
		}
	}
}

// Facts made while a backpressured replay is under way follow it, gap-free and once each.
func TestFactsDuringBackpressuredReplay(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	recordFacts(t, c, replayBacklog)
	fs := srv.add(0)
	fs.gate, fs.gateAfter = make(chan struct{}), 1 // the Hello, then nothing until the gate opens
	run(t, c)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // more facts while the replay waits
		defer wg.Done()
		recordFacts(t, c, 50)
	}()
	time.Sleep(100 * time.Millisecond)
	if c.Online() {
		t.Fatal("online before the backlog was queued")
	}
	close(fs.gate)
	wg.Wait()
	eventually(t, "every fact", func() bool { return len(fs.facts()) >= replayBacklog+50 })
	time.Sleep(50 * time.Millisecond)
	checkRun(t, fs.facts(), 0, replayBacklog+50)
}

// A link that drops mid-replay: the next server, with its own mark, gets everything above it, gap-free and once each.
func TestReplayAfterDropMidReplay(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	recordFacts(t, c, replayBacklog)
	first := srv.add(0)
	first.gate, first.gateAfter, first.silentFacts = make(chan struct{}), 100, true
	run(t, c)
	eventually(t, "part of the replay", func() bool { return len(first.facts()) > 50 })
	const mark = 37 // the first server applied this much before it went
	second := srv.add(mark)
	first.close()
	close(first.gate)
	eventually(t, "the replay above the mark", func() bool { return len(second.facts()) >= replayBacklog-mark })
	time.Sleep(50 * time.Millisecond)
	checkRun(t, second.facts(), mark, replayBacklog)
}

// Facts and requests reach the server in the order the node made them.
func TestFactsAndRequestsKeepTheirOrder(t *testing.T) {
	srv := newServers(t)
	c := newTestClient(t, srv)
	fs := srv.add(0)
	run(t, c)
	eventually(t, "online", c.Online)

	eng := c.Engine()
	// The request is made right after the fact is committed, before the pump may have sent it.
	if err := eng.TurnEnded("T1", "A1"); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- eng.FileReport("T1", core.ReportInput{AssignmentID: "A1", State: core.ReportReady}) }()
	time.Sleep(50 * time.Millisecond)
	if err := eng.ExpireApproval("AP1"); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second fact", func() bool { return len(fs.facts()) == 2 })
	want := []string{MethodTurnEnded, MethodFileReport, MethodExpireApproval}
	if got := fs.methodsOf(); !slices.Equal(got, want) {
		t.Fatalf("wire order = %v, want %v", got, want)
	}
}
