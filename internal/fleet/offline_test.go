package fleet

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/nodelink"
)

// fakeClock is a clock the test moves: Now and the timers AfterFunc makes.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	fn      func()
	stopped bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, fn func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := &fakeTimer{at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, tm)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !tm.stopped
		tm.stopped = true
		return was
	}
}

// pending is how many timers have not fired or stopped.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, tm := range c.timers {
		if !tm.stopped {
			n++
		}
	}
	return n
}

// advance moves the clock by d and runs the timers that are due, in order.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for _, tm := range c.timers {
		if !tm.stopped && !tm.at.After(c.now) {
			tm.stopped = true
			due = append(due, tm.fn)
		}
	}
	c.mu.Unlock()
	for _, fn := range due {
		fn()
	}
}

// TestOfflineAfterThreshold: a node whose link ends is offline in Nodes() at once; its Tickets show the "node offline"
// person stage only after OfflineAfter without a reconnect, and a reconnect clears it.
func TestOfflineAfterThreshold(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	tk := e.placed(nodeID)
	clk := newFakeClock()
	f := e.fleet(func(c *Config) { c.Now, c.AfterFunc = clk.Now, clk.AfterFunc })
	stage := func() core.Stage {
		_, s, err := e.eng.Ticket(tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	offlineStage := core.Stage{Stage: "person", Action: "offline"}

	w := dialWire(t, f, nodeID, hello(nodeID))
	w.waitWelcome()
	eventually(t, "the node is online", func() bool { return view(t, f, nodeID).State == StateOnline })
	w.close()
	eventually(t, "the node is offline at once", func() bool { return view(t, f, nodeID).State == StateOffline })
	eventually(t, "the offline timer is set", func() bool { return clk.pending() > 0 })

	clk.advance(OfflineAfter - time.Second)
	time.Sleep(50 * time.Millisecond)
	if s := stage(); s == offlineStage {
		t.Fatal("the Ticket shows the node offline at 1:59")
	}
	clk.advance(time.Second)
	eventually(t, "the Ticket shows the node offline at 2:00", func() bool { return stage() == offlineStage })
	if got := e.ticket(tk.ID); got.LatestAssignment().NodeOffline == nil || !got.LatestAssignment().NodeOffline.Equal(clk.Now().Add(-OfflineAfter)) {
		t.Fatalf("offline since %v, want the link's end %v", got.LatestAssignment().NodeOffline, clk.Now().Add(-OfflineAfter))
	}

	w2 := dialWire(t, f, nodeID, hello(nodeID))
	w2.waitWelcome()
	eventually(t, "a reconnect clears the stage", func() bool { return stage() != offlineStage })
	w2.close()
	eventually(t, "the node is offline again", func() bool { return view(t, f, nodeID).State == StateOffline })

	// A node that comes back within the threshold never shows offline.
	eventually(t, "a new offline timer", func() bool { return clk.pending() > 0 })
	clk.advance(time.Minute)
	w3 := dialWire(t, f, nodeID, hello(nodeID))
	w3.waitWelcome()
	eventually(t, "the node is online", func() bool { return view(t, f, nodeID).State == StateOnline })
	clk.advance(2 * OfflineAfter)
	time.Sleep(50 * time.Millisecond)
	if s := stage(); s == offlineStage {
		t.Fatal("a node back within the threshold shows offline")
	}
}

// TestOfflineAfterServerRestart: a node that never connects after the server starts is offline OfflineAfter after the
// start, since the last time it was seen.
func TestOfflineAfterServerRestart(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	tk := e.placed(nodeID)
	clk := newFakeClock()
	seen := clk.Now().Add(-time.Hour)
	if err := e.st.TouchNode(nodeID, seen); err != nil {
		t.Fatal(err)
	}
	e.fleet(func(c *Config) { c.Now, c.AfterFunc = clk.Now, clk.AfterFunc })
	clk.advance(OfflineAfter - time.Second)
	time.Sleep(50 * time.Millisecond)
	if got := e.ticket(tk.ID); got.LatestAssignment().NodeOffline != nil {
		t.Fatal("offline before the node had OfflineAfter to come back")
	}
	clk.advance(time.Second)
	eventually(t, "offline since it was last seen", func() bool {
		got := e.ticket(tk.ID)
		a := got.LatestAssignment()
		return a.NodeOffline != nil && a.NodeOffline.Equal(seen)
	})
}

// TestSeedBeforeStart: Retry elsewhere places the Ticket on another node, which gets the checkpoint's bundle from the
// mirror as seed chunks, then the seed call naming it, then the Start; the mirror's temporary directory is left empty.
func TestSeedBeforeStart(t *testing.T) {
	e, f := executorEnv(t, func(c *Config) { c.Mirror = NewMirror(filepath.Join(t.TempDir(), "checkpoints")) })
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	tk := e.placedRepo(nodeID, "o/r")
	repo := nodeRepo(t)
	sha := checkpointIn(t, repo, tk.ID, "a.txt", "half done\n")
	bundle := bundleOf(t, repo, tk.ID, "")
	if err := f.cfg.Mirror.Receive("o/r", tk.ID, bytes.NewReader(bundle)); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	if err := f.recordCheckpoint(Checkpoint{TicketID: tk.ID, Repo: "o/r", SHA: sha, Node: nodeID, At: at}); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.NodeOffline(nodeID, time.Now()); err != nil {
		t.Fatal(err)
	}
	a := dialAnswering(t, f, otherID)
	if err := e.eng.RetryElsewhere(tk.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the start on studio", func() bool { return a.got(nodelink.ServeStart) })
	order, data := a.received()
	var got []string
	for _, o := range order {
		if o != nodelink.ServeStatus && (len(got) == 0 || got[len(got)-1] != o) {
			got = append(got, o)
		}
	}
	if strings.Join(got, ",") != "setPaused,blob:seed,seed,start" {
		t.Fatalf("order = %v, want the seed's chunks, the seed, then the start", order)
	}
	if len(data) == 0 {
		t.Fatal("no bundle data")
	}
	a.mu.Lock()
	var seed nodelink.ArgsSeed
	for i, m := range a.methods {
		if m == nodelink.ServeSeed {
			_ = json.Unmarshal(a.args[i], &seed)
		}
	}
	a.mu.Unlock()
	if seed.TicketID != tk.ID || seed.SHA != sha || !seed.At.Equal(at) || seed.From != "mini" || !seed.Offline || seed.Transfer == "" {
		t.Fatalf("seed = %+v", seed)
	}
	// The bundle the node got holds the checkpoint.
	file := filepath.Join(t.TempDir(), "got.bundle")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if heads := gitT(t, repo, "bundle", "list-heads", file); !strings.Contains(heads, sha) {
		t.Fatalf("bundle heads = %s", heads)
	}
	eventually(t, "the mirror's temporary directory empty", func() bool {
		entries, _ := os.ReadDir(filepath.Join(f.cfg.Mirror.dir, mirrorTmp))
		return len(entries) == 0
	})
}
