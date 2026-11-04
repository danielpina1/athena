package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// streamEnv is a node without a loop: just its stream rings and a transcript directory.
type streamEnv struct {
	n    *Node
	path string
	next int64 // the offset the next line lands at
}

func newStreamEnv(t *testing.T) *streamEnv {
	t.Helper()
	dir := t.TempDir()
	return &streamEnv{n: &Node{cfg: Config{StateDir: dir}}, path: TranscriptPath(dir, "T-1")}
}

func body(i int) string { return fmt.Sprintf(`{"type":"x","n":%d}`, i) }

// file writes lines n..n+count-1 to the transcript only (what a restarted node finds on disk).
func (e *streamEnv) file(t *testing.T, from, count int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(e.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := from; i < from+count; i++ {
		fmt.Fprintln(f, body(i))
		e.next += int64(len(body(i))) + 1
	}
}

// write is what a session does: the transcript gets the line and the ring hears of it.
func (e *streamEnv) write(t *testing.T, i int) int64 {
	t.Helper()
	off := e.next
	e.file(t, i, 1)
	e.n.streams.add("T-1", off, []byte(body(i)))
	return off
}

func nums(lines []Line) string {
	var s []string
	for _, l := range lines {
		if l.GapTo != 0 {
			s = append(s, fmt.Sprintf("gap(%d-%d)", l.Offset, l.GapTo))
			continue
		}
		var n int
		fmt.Sscanf(string(l.Data), `{"type":"x","n":%d}`, &n)
		s = append(s, fmt.Sprint(n))
	}
	return strings.Join(s, " ")
}

func recv(t *testing.T, ch <-chan Line) Line {
	t.Helper()
	select {
	case l, ok := <-ch:
		if !ok {
			t.Fatal("live channel closed")
		}
		return l
	case <-time.After(5 * time.Second):
		t.Fatal("no line")
	}
	return Line{}
}

func TestSubscribeBackfillFromFileThenLive(t *testing.T) {
	e := newStreamEnv(t)
	e.file(t, 0, 5) // written before this process started: not in the ring
	off5 := e.write(t, 5)
	e.write(t, 6)

	back, live, cancel, err := e.n.Subscribe("T-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if got := nums(back); got != "0 1 2 3 4 5 6" {
		t.Fatalf("backfill = %s", got)
	}
	for i, l := range back {
		if i > 0 && l.Offset != back[i-1].Offset+int64(len(back[i-1].Data))+1 {
			t.Fatalf("line %d at %d does not follow line %d", i, l.Offset, i-1)
		}
	}
	off7 := e.write(t, 7)
	if l := recv(t, live); l.Offset != off7 || string(l.Data) != body(7) {
		t.Fatalf("live = %+v", l)
	}

	// From the middle: inside the ring; and from inside a line, which starts at the next one.
	back2, _, cancel2, _ := e.n.Subscribe("T-1", off5)
	defer cancel2()
	if got := nums(back2); got != "5 6 7" {
		t.Fatalf("from line 5 = %s", got)
	}
	back3, _, cancel3, _ := e.n.Subscribe("T-1", off5+3)
	defer cancel3()
	if got := nums(back3); got != "6 7" {
		t.Fatalf("from inside line 5 = %s", got)
	}
	// Older than the ring and inside a file line.
	back4, _, cancel4, _ := e.n.Subscribe("T-1", 3)
	defer cancel4()
	if got := nums(back4); got != "1 2 3 4 5 6 7" {
		t.Fatalf("from inside line 0 = %s", got)
	}
	// At the end: nothing, but live still works.
	back5, live5, cancel5, _ := e.n.Subscribe("T-1", e.next)
	defer cancel5()
	if len(back5) != 0 {
		t.Fatalf("past the end = %s", nums(back5))
	}
	e.write(t, 8)
	if l := recv(t, live5); string(l.Data) != body(8) {
		t.Fatalf("live after empty backfill = %+v", l)
	}
	cancel()
	cancel() // twice is fine
	if _, ok := <-live; ok {
		// drained lines may remain; the channel must close eventually
		for range live {
		}
	}
}

func TestTailMinusOne(t *testing.T) {
	e := newStreamEnv(t)
	e.file(t, 0, 300)
	back, _, cancel, err := e.n.Subscribe("T-1", -1)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if len(back) != TailLines || string(back[0].Data) != body(100) || string(back[len(back)-1].Data) != body(299) {
		t.Fatalf("file tail = %d lines %s..", len(back), nums(back[:1]))
	}
	// Some in the ring, the rest from the file before it.
	for i := 300; i < 350; i++ {
		e.write(t, i)
	}
	back, _, cancel, _ = e.n.Subscribe("T-1", -1)
	cancel()
	if len(back) != TailLines || string(back[0].Data) != body(150) || string(back[len(back)-1].Data) != body(349) {
		t.Fatalf("mixed tail = %d lines from %s", len(back), nums(back[:1]))
	}
	// A full ring answers alone.
	for i := 350; i < 600; i++ {
		e.write(t, i)
	}
	if err := os.Remove(e.path); err != nil {
		t.Fatal(err)
	}
	back, _, cancel, _ = e.n.Subscribe("T-1", -1)
	cancel()
	if len(back) != TailLines || string(back[0].Data) != body(400) {
		t.Fatalf("ring tail = %d lines from %s", len(back), nums(back[:1]))
	}
	// A Ticket with no transcript and no ring: empty.
	if back, _, cancel, err := e.n.Subscribe("T-2", -1); err != nil || len(back) != 0 {
		t.Fatalf("unknown ticket: %v, %d lines", err, len(back))
	} else {
		cancel()
	}
	if _, _, _, err := e.n.Subscribe("../x", -1); err == nil {
		t.Fatal("a ticket id with a path in it was accepted")
	}
}

func TestBackfillOfLongHistoryIsCutWithGap(t *testing.T) {
	e := newStreamEnv(t)
	e.file(t, 0, 2500)
	back, _, cancel, err := e.n.Subscribe("T-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(back) != ringMaxLines+1 || back[0].GapTo == 0 || back[0].Offset != 0 || back[1].Offset != back[0].GapTo ||
		string(back[1].Data) != body(500) {
		t.Fatalf("backfill = %d lines, first %+v second %+v", len(back), back[0], back[1])
	}
}

func TestRingBoundedAndLongLinesKeptAsOffsets(t *testing.T) {
	e := newStreamEnv(t)
	for i := 0; i < 3000; i++ {
		e.n.streams.add("T-1", int64(i*10), []byte("0123456789"))
	}
	e.n.streams.add("T-1", 30000, make([]byte, maxLineBytes+1))
	for i := 0; i < 8; i++ { // 8 x 200 KiB overflow 1 MiB
		e.n.streams.add("T-1", int64(40000+i*300000), make([]byte, 200<<10))
	}
	r := e.n.streams.rings["T-1"]
	if len(r.lines) > ringMaxLines || r.bytes > ringMaxBytes+200<<10 || r.bytes < 0 {
		t.Fatalf("ring holds %d lines, %d bytes", len(r.lines), r.bytes)
	}
	for _, l := range r.lines {
		if l.Offset == 30000 && l.Data != nil {
			t.Fatal("a line over the cap kept its data")
		}
	}
}

func TestSlowSubscriberGetsGapOthersUnaffected(t *testing.T) {
	e := newStreamEnv(t)
	e.write(t, 0)
	_, slow, cancelSlow, _ := e.n.Subscribe("T-1", -1)
	defer cancelSlow()
	_, fast, cancelFast, _ := e.n.Subscribe("T-1", -1)
	defer cancelFast()

	const total = 1500
	var got []Line
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for l := range fast {
			got = append(got, l)
			if len(got) == total {
				return
			}
		}
	}()
	offs := make([]int64, total+1)
	start := time.Now()
	for i := 1; i <= total; i++ {
		offs[i] = e.write(t, i)
		if i%100 == 0 {
			time.Sleep(time.Millisecond) // let the fast one keep up
		}
	}
	wg.Wait()
	if time.Since(start) > 5*time.Second {
		t.Fatalf("publishing took %v: the slow subscriber slowed the node", time.Since(start))
	}
	if len(got) != total || got[0].GapTo != 0 || got[total-1].Offset != offs[total] {
		t.Fatalf("fast subscriber got %d lines", len(got))
	}
	// The slow one holds the first 512 and nothing else; the next line it has room for follows a gap.
	for i := 1; i <= subscriberBuffer; i++ {
		if l := recv(t, slow); l.Offset != offs[i] {
			t.Fatalf("slow line %d at %d, want %d", i, l.Offset, offs[i])
		}
	}
	e.write(t, total+1)
	gap := recv(t, slow)
	if gap.GapTo == 0 || gap.Offset != offs[subscriberBuffer+1] || gap.GapTo != e.next-int64(len(body(total+1)))-1 {
		t.Fatalf("gap = %+v, want %d..%d", gap, offs[subscriberBuffer+1], e.next-int64(len(body(total+1)))-1)
	}
	if l := recv(t, slow); string(l.Data) != body(total+1) {
		t.Fatalf("after the gap: %+v", l)
	}
}

func TestRingFreedAfterTicketEndWithoutSubscribers(t *testing.T) {
	old := ringTTL
	ringTTL = 30 * time.Millisecond
	defer func() { ringTTL = old }()
	e := newStreamEnv(t)
	e.write(t, 0)
	_, _, cancel, _ := e.n.Subscribe("T-1", -1)
	e.n.streams.end("T-1")
	time.Sleep(100 * time.Millisecond)
	if !e.hasRing() {
		t.Fatal("the ring was freed under a subscriber")
	}
	cancel() // the last subscriber leaves: the clock starts
	eventually(t, func() bool { return !e.hasRing() })

	// A watch of a Ticket that wrote nothing leaves no ring behind.
	_, _, cancel2, _ := e.n.Subscribe("T-9", -1)
	cancel2()
	e.n.streams.mu.Lock()
	n := len(e.n.streams.rings)
	e.n.streams.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d rings left", n)
	}
}

func (e *streamEnv) hasRing() bool {
	e.n.streams.mu.Lock()
	defer e.n.streams.mu.Unlock()
	return e.n.streams.rings["T-1"] != nil
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("condition not reached")
}

// TestSessionLinesReachSubscribers: a launched session's lines reach the ring through the OnLine wiring, at the
// offsets the transcript file has them, and a subscriber's backfill is served from that ring.
func TestSessionLinesReachSubscribers(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Streamed task", "")
	h.running(tk.ID)
	tr := TranscriptPath(h.cfg.StateDir, tk.ID)
	ringLines := func() []Line {
		h.n.streams.mu.Lock()
		defer h.n.streams.mu.Unlock()
		if r := h.n.streams.rings[tk.ID]; r != nil {
			return append([]Line(nil), r.lines...)
		}
		return nil
	}
	h.eventually("ring has the transcript's lines", func() bool {
		b, err := os.ReadFile(tr)
		return err == nil && strings.Contains(string(b), fixtureSessionID) && len(ringLines()) == strings.Count(string(b), "\n")
	})
	file, _ := os.ReadFile(tr)
	ring := ringLines()
	if len(ring) == 0 {
		t.Fatal("the ring is empty: OnLine is not wired")
	}
	for _, l := range ring {
		if end := int(l.Offset) + len(l.Data); end >= len(file) || string(file[l.Offset:end]) != string(l.Data) {
			t.Fatalf("ring line at %d does not match the transcript", l.Offset)
		}
	}
	back, _, cancel, err := h.n.Subscribe(tk.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(back) < len(ring) || nums(back[len(back)-1:]) == "" {
		t.Fatalf("backfill has %d lines, ring %d", len(back), len(ring))
	}
}

// A line the backfill covered, or one before from, is not delivered live.
func TestSubscriberFloorFiltersCoveredLines(t *testing.T) {
	e := newStreamEnv(t)
	e.file(t, 0, 3)
	back, live, cancel, _ := e.n.Subscribe("T-1", 0)
	defer cancel()
	if len(back) != 3 {
		t.Fatalf("backfill = %s", nums(back))
	}
	e.n.streams.add("T-1", back[2].Offset, back[2].Data) // OnLine of a line the file read saw already
	off := e.next
	e.file(t, 3, 1)
	e.n.streams.add("T-1", off, []byte(body(3)))
	if l := recv(t, live); string(l.Data) != body(3) {
		t.Fatalf("live = %+v, want line 3 only", l)
	}
}
