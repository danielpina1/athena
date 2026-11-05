package hub

import (
	"context"
	"fmt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
)

// fakeSource is a Source over maps.
type fakeSource struct {
	mu        sync.Mutex
	tickets   map[string]core.Ticket
	approvals map[string]core.Approval
	triggers  map[string]core.Trigger
	threads   map[string]core.Thread
	decisions map[string]core.Decision
	health    []HealthItem
	revision  int
	settings  string
	node      node.Status
	repos     []*athenav1.RepoView
	repoReads int
	git       map[string]bool
	kinds     []string
	muted     []string
	prompts   *athenav1.PromptsView
	// triggerReads counts Trigger calls.
	triggerReads int
}

func newFake() *fakeSource {
	return &fakeSource{tickets: map[string]core.Ticket{}, approvals: map[string]core.Approval{},
		triggers: map[string]core.Trigger{}, threads: map[string]core.Thread{}, decisions: map[string]core.Decision{},
		settings: "{}", prompts: &athenav1.PromptsView{}}
}

var epoch0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func (f *fakeSource) put(t core.Ticket) { f.mu.Lock(); f.tickets[t.ID] = t; f.mu.Unlock() }

func (f *fakeSource) Tickets(endedLimit int) ([]core.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out, ended []core.Ticket
	for _, t := range f.tickets {
		if t.Ended == nil {
			out = append(out, t)
		} else {
			ended = append(ended, t)
		}
	}
	sort.Slice(ended, func(i, j int) bool { return ended[i].UpdatedAt.After(ended[j].UpdatedAt) })
	if len(ended) > endedLimit {
		ended = ended[:endedLimit]
	}
	out = append(out, ended...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeSource) Ticket(id string) (core.Ticket, core.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[id]
	if !ok {
		return core.Ticket{}, core.Stage{}, fmt.Errorf("ticket %s not found", id)
	}
	return t, core.TicketStage(&t), nil
}

func sortedBy[T any](m map[string]T, less func(a, b T) bool, keep func(T) bool, limit int) []T {
	var out []T
	for _, v := range m {
		if keep(v) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (f *fakeSource) PendingApprovals() ([]core.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sortedBy(f.approvals, func(a, b core.Approval) bool { return a.ID < b.ID },
		func(a core.Approval) bool { return a.State == core.ApprovalPending }, 0), nil
}

func (f *fakeSource) AnsweredApprovals(limit int) ([]core.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sortedBy(f.approvals, func(a, b core.Approval) bool { return a.ID > b.ID },
		func(a core.Approval) bool { return a.State != core.ApprovalPending }, limit), nil
}

func (f *fakeSource) TicketAnsweredApprovals(ticketID string) ([]core.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sortedBy(f.approvals, func(a, b core.Approval) bool { return a.ID > b.ID },
		func(a core.Approval) bool { return a.State != core.ApprovalPending && a.TicketID == ticketID }, 0), nil
}

func (f *fakeSource) Repos() ([]*athenav1.RepoView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repoReads++
	return f.repos, nil
}

func (f *fakeSource) setRepos(r ...*athenav1.RepoView) { f.mu.Lock(); f.repos = r; f.mu.Unlock() }

func (f *fakeSource) GitFolder(dir string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.git[dir] }

func (f *fakeSource) setGit(dir string, git bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.git == nil {
		f.git = map[string]bool{}
	}
	f.git[dir] = git
}

func (f *fakeSource) Notifications() ([]string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kinds, f.muted, nil
}

func (f *fakeSource) Prompts() (*athenav1.PromptsView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return proto.Clone(f.prompts).(*athenav1.PromptsView), nil
}

func (f *fakeSource) setMuted(m ...string) { f.mu.Lock(); f.muted = m; f.mu.Unlock() }

func (f *fakeSource) Approval(id string) (core.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.approvals[id]
	if !ok {
		return a, fmt.Errorf("approval %s not found", id)
	}
	return a, nil
}

func (f *fakeSource) RecentTriggers(limit int) ([]core.Trigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sortedBy(f.triggers, func(a, b core.Trigger) bool { return a.ID > b.ID }, func(core.Trigger) bool { return true }, limit), nil
}

func (f *fakeSource) Trigger(id string) (core.Trigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggerReads++
	t, ok := f.triggers[id]
	if !ok {
		return t, fmt.Errorf("trigger %s not found", id)
	}
	return t, nil
}

func (f *fakeSource) ActiveThreads() ([]core.Thread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sortedBy(f.threads, func(a, b core.Thread) bool { return a.Key < b.Key }, func(t core.Thread) bool { return t.Active() }, 0), nil
}

func (f *fakeSource) RecentThreads(limit int) ([]core.Thread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sortedBy(f.threads, func(a, b core.Thread) bool { return a.Key > b.Key }, func(core.Thread) bool { return true }, limit), nil
}

func (f *fakeSource) Thread(key string) (core.Thread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.threads[key]
	if !ok {
		return t, fmt.Errorf("thread %s not found", key)
	}
	return t, nil
}

func (f *fakeSource) Decisions(thread string, limit int) ([]core.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sortedBy(f.decisions, func(a, b core.Decision) bool { return a.ID > b.ID },
		func(d core.Decision) bool { return thread == "" || d.Thread == thread }, limit), nil
}

func (f *fakeSource) Decision(id string) (core.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.decisions[id]
	if !ok {
		return d, fmt.Errorf("decision %s not found", id)
	}
	return d, nil
}

func (f *fakeSource) Health() []HealthItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HealthItem(nil), f.health...)
}

func (f *fakeSource) setHealth(items ...HealthItem) { f.mu.Lock(); f.health = items; f.mu.Unlock() }

func (f *fakeSource) Policy() (int, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revision, "v1", f.settings, nil
}

func (f *fakeSource) NodeStatus() node.Status { f.mu.Lock(); defer f.mu.Unlock(); return f.node }

func ticket(id string) core.Ticket {
	return core.Ticket{ID: id, Title: "title " + id, CreatedAt: epoch0, UpdatedAt: epoch0}
}

func ended(id string, n int) core.Ticket {
	t := ticket(id)
	at := epoch0.Add(time.Duration(n) * time.Minute)
	t.UpdatedAt, t.Ended = at, &core.Ended{At: at, By: core.EndByExecutor, Outcome: core.OutcomeCompleted}
	return t
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// start returns a running Hub over f.
func start(t *testing.T, f *fakeSource) *Hub {
	t.Helper()
	h := New(f, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.Start(ctx)
	return h
}

func next(t *testing.T, s *Sub) *athenav1.Delta {
	t.Helper()
	select {
	case d, ok := <-s.C():
		if !ok {
			t.Fatal("feed closed")
		}
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("no delta")
		return nil
	}
}

func none(t *testing.T, s *Sub) {
	t.Helper()
	select {
	case d := <-s.C():
		t.Fatalf("unexpected delta %v", d)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSnapshotBounds(t *testing.T) {
	f := newFake()
	for i := 0; i < 150; i++ {
		f.put(ended(fmt.Sprintf("E-%03d", i), i))
	}
	f.put(ticket("T-1"))
	f.put(ticket("T-2"))
	for i := 0; i < 70; i++ {
		f.mu.Lock()
		f.approvals[fmt.Sprintf("A-%03d", i)] = core.Approval{ID: fmt.Sprintf("A-%03d", i), State: core.ApprovalApproved}
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.approvals["P-1"] = core.Approval{ID: "P-1", State: core.ApprovalPending}
	for i := 0; i < 300; i++ {
		f.decisions[fmt.Sprintf("D-%03d", i)] = core.Decision{ID: fmt.Sprintf("D-%03d", i), Thread: "x:1"}
		f.triggers[fmt.Sprintf("R-%03d", i)] = core.Trigger{ID: fmt.Sprintf("R-%03d", i)}
	}
	for i := 0; i < 130; i++ {
		f.threads[fmt.Sprintf("x:%03d", i)] = core.Thread{Key: fmt.Sprintf("x:%03d", i)}
	}
	f.threads["x:000a"] = core.Thread{Key: "x:000a", Queued: []string{"R-1"}}
	f.mu.Unlock()
	h := New(f, quiet())
	s, err := h.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tickets) != 102 {
		t.Errorf("tickets = %d, want 2 open + 100 ended", len(s.Tickets))
	}
	ids := map[string]bool{}
	for _, tk := range s.Tickets {
		ids[tk.Id] = true
	}
	if !ids["T-1"] || !ids["E-149"] || !ids["E-050"] || ids["E-049"] {
		t.Errorf("wrong tickets kept: T-1=%v E-149=%v E-050=%v E-049=%v", ids["T-1"], ids["E-149"], ids["E-050"], ids["E-049"])
	}
	if len(s.Approvals) != 51 {
		t.Errorf("approvals = %d, want 1 pending + 50 answered", len(s.Approvals))
	}
	if len(s.Decisions) != 200 || len(s.Triggers) != 200 {
		t.Errorf("decisions %d triggers %d, want 200 each", len(s.Decisions), len(s.Triggers))
	}
	if len(s.Threads) != 101 {
		t.Errorf("threads = %d, want the active one plus 100 recent", len(s.Threads))
	}
}

func TestDeltaPerCommittedChange(t *testing.T) {
	f := newFake()
	f.put(ticket("T-1"))
	h := start(t, f)
	sub, _ := h.Subscribe(0, h.Epoch())
	defer sub.Close()
	h.OnCommit(store.Changes{Tickets: []string{"T-1"}})
	d := next(t, sub)
	if d.Seq != 1 || d.GetTicket().GetId() != "T-1" {
		t.Fatalf("first delta = %v", d)
	}
	f.put(func() core.Ticket { x := ticket("T-1"); x.Title = "renamed"; return x }())
	h.OnCommit(store.Changes{Tickets: []string{"T-1"}})
	d = next(t, sub)
	if d.Seq != 2 || d.GetTicket().GetTitle() != "renamed" {
		t.Fatalf("second delta = %v", d)
	}
	none(t, sub)
}

func TestDeltaKinds(t *testing.T) {
	f := newFake()
	f.mu.Lock()
	f.approvals["A-1"] = core.Approval{ID: "A-1", State: core.ApprovalPending}
	f.triggers["R-1"] = core.Trigger{ID: "R-1"}
	f.threads["x:1"] = core.Thread{Key: "x:1"}
	f.decisions["D-1"] = core.Decision{ID: "D-1"}
	f.revision = 3
	f.mu.Unlock()
	h := start(t, f)
	sub, _ := h.Subscribe(0, h.Epoch())
	h.OnCommit(store.Changes{Approvals: []string{"A-1"}, Triggers: []string{"R-1"}, Threads: []string{"x:1"},
		Decisions: []string{"D-1"}, Policy: true})
	got := ""
	for i := 0; i < 5; i++ {
		d := next(t, sub)
		if d.Seq != uint64(i+1) {
			t.Errorf("seq %d at position %d", d.Seq, i)
		}
		got += fmt.Sprintf("%T ", d.Change)
	}
	for _, w := range []string{"Approval", "Trigger", "Thread", "Decision", "Policy"} {
		if !strings.Contains(got, "Delta_"+w+" ") {
			t.Errorf("no %s delta in %s", w, got)
		}
	}
}

func TestCursorInsideRingResumes(t *testing.T) {
	f := newFake()
	h := start(t, f)
	for i := 1; i <= 5; i++ {
		f.put(ticket(fmt.Sprintf("T-%d", i)))
		h.OnCommit(store.Changes{Tickets: []string{fmt.Sprintf("T-%d", i)}})
	}
	waitSeq(t, h, 5)
	sub, need := h.Subscribe(3, h.Epoch())
	defer sub.Close()
	if need {
		t.Fatal("snapshot needed for a cursor inside the ring")
	}
	if d := next(t, sub); d.Seq != 4 {
		t.Errorf("first replayed seq = %d, want 4", d.Seq)
	}
	if d := next(t, sub); d.Seq != 5 {
		t.Errorf("second replayed seq = %d, want 5", d.Seq)
	}
	none(t, sub)
	if _, need := h.Subscribe(5, h.Epoch()); need {
		t.Error("the current cursor needs no snapshot")
	}
}

func waitSeq(t *testing.T, h *Hub, seq uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		got := h.seq
		h.mu.Unlock()
		if got >= seq {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("seq never reached %d", seq)
}

func TestCursorOutsideRingNeedsSnapshot(t *testing.T) {
	h := New(newFake(), quiet())
	for i := 0; i < ringDeltas+10; i++ {
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Node{Node: &athenav1.NodeStatus{Name: "n"}}})
	}
	if len(h.ring) != ringDeltas {
		t.Fatalf("ring holds %d, want %d", len(h.ring), ringDeltas)
	}
	// The ring now starts at seq 11: cursor 10 is the oldest resumable.
	if _, need := h.Subscribe(9, h.Epoch()); !need {
		t.Error("cursor 9 is outside the ring")
	}
	if sub, need := h.Subscribe(10, h.Epoch()); need {
		t.Error("cursor 10 is the edge of the ring and resumes")
	} else {
		sub.Close()
	}
	if _, need := h.Subscribe(h.seq+1, h.Epoch()); !need {
		t.Error("a cursor ahead of the hub needs a snapshot")
	}
}

func TestRingBytesBound(t *testing.T) {
	h := New(newFake(), quiet())
	big := strings.Repeat("x", 1<<20)
	for i := 0; i < 20; i++ {
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Node{Node: &athenav1.NodeStatus{Name: big}}})
	}
	if h.ringBytes > ringBytes || len(h.ring) >= 20 {
		t.Errorf("ring keeps %d bytes in %d deltas", h.ringBytes, len(h.ring))
	}
}

func TestEpochMismatchNeedsSnapshot(t *testing.T) {
	h := New(newFake(), quiet())
	if _, need := h.Subscribe(0, "an-older-process"); !need {
		t.Error("another epoch must get a snapshot")
	}
	if _, need := h.Subscribe(0, ""); !need {
		t.Error("no epoch must get a snapshot")
	}
	if h.Epoch() == New(newFake(), quiet()).Epoch() {
		t.Error("epochs repeat")
	}
}

func TestSlowSubscriberResyncedNotBlocking(t *testing.T) {
	f := newFake()
	h := New(f, quiet())
	slow, _ := h.Subscribe(0, h.Epoch())
	fast, _ := h.Subscribe(0, h.Epoch())
	got := make(chan int, 1)
	go func() {
		n := 0
		for range fast.C() {
			n++
			if n == subBuffer+50 {
				break
			}
		}
		got <- n
	}()
	done := make(chan struct{})
	go func() {
		for i := 0; i < subBuffer+50; i++ {
			h.publish(&athenav1.Delta{Change: &athenav1.Delta_Node{Node: &athenav1.NodeStatus{Name: "n"}}})
			time.Sleep(50 * time.Microsecond)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on the slow subscriber")
	}
	if n := <-got; n != subBuffer+50 {
		t.Errorf("the fast subscriber got %d deltas, want %d", n, subBuffer+50)
	}
	n := 0
	for range slow.C() {
		n++
	}
	if n != subBuffer {
		t.Errorf("slow subscriber drained %d deltas before its close, want %d", n, subBuffer)
	}
	if !slow.Resync() {
		t.Error("slow subscriber was closed without resync")
	}
	if fast.Resync() {
		t.Error("the fast subscriber must not be resynced")
	}
}

func TestDetailOnlyToWatchers(t *testing.T) {
	f := newFake()
	f.put(ticket("T-1"))
	f.put(ticket("T-2"))
	h := start(t, f)
	watcher, _ := h.Subscribe(0, h.Epoch())
	other, _ := h.Subscribe(0, h.Epoch())
	watcher.Watch("T-1")
	h.OnCommit(store.Changes{Tickets: []string{"T-1"}})

	var kinds []string
	for i := 0; i < 2; i++ {
		d := next(t, watcher)
		kinds = append(kinds, fmt.Sprintf("%T", d.Change))
		if d.GetTicketDetail() != nil && (d.Seq != 0 || d.GetTicketDetail().GetSummary().GetId() != "T-1") {
			t.Errorf("detail delta = %v", d)
		}
	}
	if kinds[0] != "*athenav1.Delta_Ticket" || kinds[1] != "*athenav1.Delta_TicketDetail" {
		t.Errorf("watcher got %v", kinds)
	}
	if d := next(t, other); d.GetTicket() == nil {
		t.Errorf("other got %v", d)
	}
	none(t, other)

	// A change to a Ticket nobody watches sends no detail, and an unwatched one stops.
	h.OnCommit(store.Changes{Tickets: []string{"T-2"}})
	if d := next(t, watcher); d.GetTicket().GetId() != "T-2" {
		t.Errorf("got %v", d)
	}
	none(t, watcher)
	watcher.Unwatch("T-1")
	h.OnCommit(store.Changes{Tickets: []string{"T-1"}})
	next(t, watcher)
	none(t, watcher)
}

func TestRoutingRecordsInSnapshot(t *testing.T) {
	f := newFake()
	f.mu.Lock()
	f.decisions["D-1"] = core.Decision{ID: "D-1", Kind: "ignore", Thread: "github:o/r#1", WouldAccept: true,
		TriggerIDs: []string{"R-1"}, At: epoch0,
		Reasons: []core.Reason{{Code: "not_addressed", Values: map[string]string{"text": "hi"}}, {Code: "policy_off"}}}
	f.triggers["R-1"] = core.Trigger{ID: "R-1", Source: "github", Actor: core.ActorRef{ID: "ann"}, Threads: []string{"github:o/r#1"},
		Text: "hello\nthere", Disposition: core.Disposition{Filtered: "x"}, At: epoch0}
	f.threads["github:o/r#1"] = core.Thread{Key: "github:o/r#1", Queued: []string{"R-1"}, Decisions: []string{"D-1"},
		Subscriptions: []core.Subscription{{TicketID: "T-1", Own: true}}}
	f.mu.Unlock()
	s, err := New(f, quiet()).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Decisions) != 1 {
		t.Fatalf("decisions = %v", s.Decisions)
	}
	d := s.Decisions[0]
	if !d.WouldAccept || d.Kind != "ignore" || len(d.Reasons) != 2 || d.Reasons[0].Code != "not_addressed" ||
		d.Reasons[0].Values["text"] != "hi" || d.DismissedAt != nil || d.Triggers[0] != "R-1" {
		t.Errorf("decision = %v", d)
	}
	if tr := s.Triggers[0]; tr.Text != "hello there" || tr.Outcome != "filtered:x" || tr.Thread != "github:o/r#1" || tr.Actor != "ann" {
		t.Errorf("trigger = %v", tr)
	}
	if th := s.Threads[0]; th.Queued != 1 || th.Decisions != 1 || th.Tickets[0] != "T-1(own)" {
		t.Errorf("thread = %v", th)
	}
}

func TestApprovalInputRedacted(t *testing.T) {
	f := newFake()
	f.mu.Lock()
	f.approvals["A-1"] = core.Approval{ID: "A-1", TicketID: "T-1", State: core.ApprovalPending, Tool: "Bash",
		Input: []byte(`{"command":"curl -H 'x' https://x.test -d ghs_16C7e42F292c6912E7710c838347Ae178B4a"}`)}
	f.mu.Unlock()
	h := start(t, f)
	snap, err := h.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if in := snap.Approvals[0].InputRedacted; strings.Contains(in, "ghs_") || !strings.Contains(in, "***") {
		t.Errorf("snapshot input = %q", in)
	}
	sub, _ := h.Subscribe(0, h.Epoch())
	h.OnCommit(store.Changes{Approvals: []string{"A-1"}})
	if in := next(t, sub).GetApproval().GetInputRedacted(); strings.Contains(in, "ghs_") {
		t.Errorf("delta input = %q", in)
	}
}

func TestTriggerAndReasonRedacted(t *testing.T) {
	tok := "ghs_16C7e42F292c6912E7710c838347Ae178B4a"
	tr := core.Trigger{ID: "R-1", Text: "use " + tok}
	if v := triggerView(&tr); strings.Contains(v.Text, "ghs_") {
		t.Errorf("trigger text = %q", v.Text)
	}
	d := core.Decision{Reasons: []core.Reason{{Code: "x", Values: map[string]string{"text": tok}}}}
	if v := decisionView(&d); strings.Contains(v.Reasons[0].Values["text"], "ghs_") {
		t.Errorf("reason value = %q", v.Reasons[0].Values["text"])
	}
}

func TestCommitOverflowResyncsEverySubscriber(t *testing.T) {
	f := newFake()
	f.put(ticket("T-1"))
	h := New(f, quiet())
	h.in = make(chan store.Changes, 1)
	sub, _ := h.Subscribe(0, h.Epoch())
	h.publish(&athenav1.Delta{Change: &athenav1.Delta_Node{Node: &athenav1.NodeStatus{}}})
	<-sub.C()
	// The loop is not running: the first commit fits, the others overflow.
	for i := 0; i < 3; i++ {
		h.OnCommit(store.Changes{Tickets: []string{"T-1"}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Start(ctx)
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("got a delta, want a close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber not closed")
	}
	if !sub.Resync() {
		t.Error("closed without resync")
	}
	// A cursor from before the loss is refused; the hub works on afterwards.
	if _, need := h.Subscribe(1, h.Epoch()); !need {
		t.Error("a cursor from before the loss resumed")
	}
	again, need := h.Subscribe(0, "")
	if !need {
		t.Fatal("expected snapshot")
	}
	h.OnCommit(store.Changes{Tickets: []string{"T-1"}})
	if d := next(t, again); d.GetTicket() == nil {
		t.Errorf("got %v", d)
	}
}

func TestHealthPublishedOnlyOnChange(t *testing.T) {
	f := newFake()
	f.setHealth(HealthItem{Name: "github", OK: true})
	h := New(f, quiet())
	h.healthTick = time.Hour
	t0 := epoch0
	h.now = func() time.Time { return t0 }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Start(ctx)
	sub, _ := h.Subscribe(0, h.Epoch())
	h.publishHealth(h.probeHealth())
	none(t, sub)
	f.setHealth(HealthItem{Name: "github", OK: false, Message: "rate limited"})
	t0 = epoch0.Add(time.Minute)
	h.publishHealth(h.probeHealth())
	d := next(t, sub)
	it := d.GetHealth().GetItems()[0]
	if it.Ok || it.Message != "rate limited" || !it.Since.AsTime().Equal(t0) {
		t.Errorf("health delta = %v", d)
	}
	h.publishHealth(h.probeHealth())
	none(t, sub)
	snap, _ := h.Snapshot()
	if snap.Health.Items[0].Ok {
		t.Error("snapshot health is stale")
	}
}

func TestNodeChangePublished(t *testing.T) {
	f := newFake()
	f.node = node.Status{Name: "mac", Slots: 4}
	h := start(t, f)
	sub, _ := h.Subscribe(0, h.Epoch())
	h.NodeChanged()
	none(t, sub) // unchanged: nothing to say
	f.mu.Lock()
	f.node = node.Status{Name: "mac", Slots: 4, SlotsInUse: 1, LiveTicketIDs: []string{"T-1"}, Queued: 2}
	f.mu.Unlock()
	h.NodeChanged()
	n := next(t, sub).GetNode()
	if n.SlotsInUse != 1 || n.Queued != 2 || n.LiveTicketIds[0] != "T-1" || n.Name != "mac" {
		t.Errorf("node = %v", n)
	}
}

func TestSnapshotCursorAndEpochIsolation(t *testing.T) {
	f := newFake()
	f.put(ticket("T-1"))
	h := start(t, f)
	h.OnCommit(store.Changes{Tickets: []string{"T-1"}})
	waitSeq(t, h, 1)
	s, err := h.Snapshot()
	if err != nil || s.Cursor != 1 {
		t.Fatalf("snapshot cursor = %d, %v", s.Cursor, err)
	}
	if _, need := h.Subscribe(s.Cursor, h.Epoch()); need {
		t.Error("the snapshot's cursor in this epoch must resume")
	}
	if _, need := h.Subscribe(s.Cursor, "another-epoch"); !need {
		t.Error("the snapshot's cursor in another epoch must not resume")
	}
}

// TestSnapshotHandoff: a client that needs a snapshot subscribes first, takes the snapshot, then forwards the feed's
// deltas above the snapshot's cursor: in order, with no gap, while commits keep arriving.
func TestSnapshotHandoff(t *testing.T) {
	f := newFake()
	h := start(t, f)
	commit := func(n int) {
		id := fmt.Sprintf("T-%d", n)
		f.put(ticket(id))
		h.OnCommit(store.Changes{Tickets: []string{id}})
		waitSeq(t, h, uint64(n))
	}
	commit(1)
	sub, need := h.Subscribe(0, "stale-epoch")
	if !need {
		t.Fatal("a stale epoch needs a snapshot")
	}
	defer sub.Close()
	commit(2)
	snap, err := h.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	commit(3)
	commit(4)
	var seqs []uint64
	for i := 0; i < 3; i++ { // seq 2, 3, 4 reached the feed
		if d := next(t, sub); d.Seq > snap.Cursor {
			seqs = append(seqs, d.Seq)
		}
	}
	for i, q := range seqs {
		if q != snap.Cursor+1+uint64(i) {
			t.Fatalf("forwarded seqs %v after cursor %d: gap or disorder", seqs, snap.Cursor)
		}
	}
	if len(seqs) == 0 || seqs[len(seqs)-1] != 4 {
		t.Errorf("forwarded %v, want up to 4", seqs)
	}
}

func TestSourceReadErrorResyncs(t *testing.T) {
	f := newFake()
	h := start(t, f)
	sub, _ := h.Subscribe(0, h.Epoch())
	// T-gone is not in the Source: its read fails after the commit.
	h.OnCommit(store.Changes{Tickets: []string{"T-gone"}})
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("got a delta, want a close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber not resynced after a failed read")
	}
	if !sub.Resync() {
		t.Error("closed without resync")
	}
	if _, need := h.Subscribe(0, h.Epoch()); !need {
		t.Error("cursor 0 must not resume across a lost commit")
	}
}

func TestSubscribeAfterShutdownIsClosed(t *testing.T) {
	h := New(newFake(), quiet())
	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx)
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		sub, _ := h.Subscribe(0, h.Epoch())
		select {
		case _, ok := <-sub.C():
			if ok {
				t.Fatal("got a delta")
			}
			return
		case <-time.After(20 * time.Millisecond):
			sub.Close()
			if time.Now().After(deadline) {
				t.Fatal("Subscribe after shutdown returned an open feed")
			}
		}
	}
}

// blockingSource holds its Ticket and Health reads until released, to catch a read still in flight.
type blockingSource struct {
	*fakeSource
	entered chan string
	release chan struct{}
	block   atomic.Bool
}

func (b *blockingSource) wait(what string) {
	if b.block.Load() {
		b.entered <- what
		<-b.release
	}
}

func (b *blockingSource) Ticket(id string) (core.Ticket, core.Stage, error) {
	b.wait("ticket")
	return b.fakeSource.Ticket(id)
}

func (b *blockingSource) Health() []HealthItem {
	b.wait("health")
	return b.fakeSource.Health()
}

// TestWaitJoinsReadsInFlight: after ctx is done, Wait returns only once the loop's and the probe's reads of the
// Source have returned, so the store behind it can close after Wait.
func TestWaitJoinsReadsInFlight(t *testing.T) {
	for _, what := range []string{"ticket", "health"} {
		f := newFake()
		f.put(core.Ticket{ID: "T-0001", Title: "x", CreatedAt: epoch0, UpdatedAt: epoch0})
		src := &blockingSource{fakeSource: f, entered: make(chan string, 4), release: make(chan struct{})}
		h := New(src, quiet())
		h.healthTick = time.Hour
		if what == "health" {
			h.healthTick = 5 * time.Millisecond
		}
		ctx, cancel := context.WithCancel(context.Background())
		h.Start(ctx)
		src.block.Store(true)
		if what == "ticket" {
			h.OnCommit(store.Changes{Tickets: []string{"T-0001"}})
		}
		select {
		case got := <-src.entered:
			if got != what {
				t.Fatalf("%s: blocked in %s", what, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no read started", what)
		}
		cancel()
		waited := make(chan struct{})
		go func() { h.Wait(); close(waited) }()
		select {
		case <-waited:
			t.Fatalf("%s: Wait returned with a read in flight", what)
		case <-time.After(50 * time.Millisecond):
		}
		src.block.Store(false)
		close(src.release)
		select {
		case <-waited:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: Wait did not return after the read", what)
		}
	}
}

func repoView(name, kind string) *athenav1.RepoView {
	return &athenav1.RepoView{Name: name, Kind: kind}
}

func TestReposInSnapshotAndDeltaOnlyOnChange(t *testing.T) {
	f := newFake()
	f.setRepos(repoView("a/b", "github"))
	h := start(t, f)
	snap, err := h.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Repos) != 1 || snap.Repos[0].Name != "a/b" {
		t.Fatalf("snapshot repos = %v", snap.Repos)
	}
	sub, _ := h.Subscribe(0, h.Epoch())
	defer sub.Close()

	// A Ticket commit that leaves the list as it was publishes the Ticket alone.
	f.put(ticket("T-1"))
	h.OnCommit(store.Changes{Tickets: []string{"T-1"}})
	if d := next(t, sub); d.GetTicket() == nil {
		t.Fatalf("first delta = %v", d)
	}
	none(t, sub)

	// A change publishes the whole list, once.
	f.setRepos(repoView("/tmp/x", "dir"), repoView("a/b", "github"))
	placed := ticket("T-1")
	placed.Execution = &core.Execution{Directory: "/tmp/x"}
	f.put(placed)
	h.OnCommit(store.Changes{Tickets: []string{"T-1"}})
	next(t, sub) // the Ticket
	d := next(t, sub)
	if r := d.GetRepos().GetRepos(); len(r) != 2 || r[0].Name != "/tmp/x" || r[0].Kind != "dir" {
		t.Fatalf("repos delta = %v", d)
	}
	h.OnCommit(store.Changes{Tickets: []string{"T-1"}})
	next(t, sub)
	none(t, sub)
}

// ReposChanged (a scan found other local repositories) publishes the list when it changed, and only then.
func TestReposChangedPublishesOnlyAChange(t *testing.T) {
	f := newFake()
	f.setRepos(repoView("a/b", "github"))
	h := start(t, f)
	sub, _ := h.Subscribe(0, h.Epoch())
	defer sub.Close()
	h.ReposChanged()
	none(t, sub)
	f.setRepos(repoView("a/b", "github"), repoView("/src/x", "git"))
	h.ReposChanged()
	if r := next(t, sub).GetRepos().GetRepos(); len(r) != 2 || r[1].Name != "/src/x" || r[1].Kind != "git" {
		t.Fatalf("repos delta = %v", r)
	}
	h.ReposChanged()
	none(t, sub)
}

// FoldersChanged (the pass over the placement folders found a .git entry) republishes the summaries of the Tickets in
// those folders, with their branch, and to a watcher the detail; other Tickets are not sent again.
func TestFoldersChangedRepublishesTheirTickets(t *testing.T) {
	f := newFake()
	in, other := ticket("T-1"), ticket("T-2")
	in.Execution = &core.Execution{Directory: "/src/x"}
	other.Execution = &core.Execution{Directory: "/src/y"}
	f.put(in)
	f.put(other)
	h := start(t, f)
	sub, _ := h.Subscribe(0, h.Epoch())
	defer sub.Close()
	sub.Watch("T-1")
	f.setGit("/src/x", true)
	h.FoldersChanged([]string{"/src/x"})
	if d := next(t, sub).GetTicket(); d.GetId() != "T-1" || d.GetBranch() != "athena/t-1" {
		t.Fatalf("ticket delta = %v", d)
	}
	if d := next(t, sub).GetTicketDetail(); d.GetSummary().GetBranch() != "athena/t-1" {
		t.Fatalf("detail delta = %v", d)
	}
	none(t, sub)
	// Nothing changed since: a wake for the local scans sends no Ticket.
	h.ReposChanged()
	none(t, sub)
}

// A commit of a Ticket whose place is known and not newer than its last use does not even read the repos.
func TestKnownPlacementSkipsReposRead(t *testing.T) {
	f := newFake()
	used := &athenav1.RepoView{Name: "o/r", Kind: "github", LastUsedAt: timestamppb.New(epoch0.Add(time.Hour))}
	f.setRepos(used)
	h := start(t, f)
	sub, _ := h.Subscribe(0, h.Epoch())
	defer sub.Close()
	tk := ticket("T-1")
	tk.Repo = "o/r"
	f.put(tk)
	f.mu.Lock()
	before := f.repoReads
	f.mu.Unlock()
	h.OnCommit(store.Changes{Tickets: []string{"T-1"}})
	next(t, sub)
	f.mu.Lock()
	after := f.repoReads
	f.mu.Unlock()
	if after != before {
		t.Errorf("repos were read %d times for a progress commit", after-before)
	}
	// A newer Ticket on it moves it up the list: read.
	tk.ID, tk.CreatedAt = "T-2", epoch0.Add(2*time.Hour)
	f.put(tk)
	h.OnCommit(store.Changes{Tickets: []string{"T-2"}})
	next(t, sub)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.repoReads == after {
		t.Error("a newer Ticket on a known repository did not re-read the repos")
	}
}

func TestNotificationsInSnapshotAndDeltaOnChange(t *testing.T) {
	f := newFake()
	f.kinds = []string{"approval", "answer"}
	h := start(t, f)
	snap, _ := h.Snapshot()
	if got := snap.Notifications; len(got.Kinds) != 2 || len(got.Muted) != 0 {
		t.Fatalf("snapshot notifications = %v", got)
	}
	sub, _ := h.Subscribe(0, h.Epoch())
	defer sub.Close()
	h.OnCommit(store.Changes{Notifications: true}) // a save that changed nothing
	none(t, sub)
	f.setMuted("answer")
	h.OnCommit(store.Changes{Notifications: true})
	d := next(t, sub)
	if n := d.GetNotifications(); len(n.GetMuted()) != 1 || n.Muted[0] != "answer" || len(n.Kinds) != 2 {
		t.Fatalf("notifications delta = %v", d)
	}
}

func (f *fakeSource) setPrompts(v *athenav1.PromptsView) { f.mu.Lock(); f.prompts = v; f.mu.Unlock() }

func TestPromptsDeltaOnSave(t *testing.T) {
	f := newFake()
	f.setPrompts(&athenav1.PromptsView{Revision: 1, Parts: []*athenav1.PromptPartView{{Name: "worker", Overridden: true}}})
	h := start(t, f)
	snap, _ := h.Snapshot()
	if got := snap.Prompts; got.GetRevision() != 1 || len(got.Parts) != 1 || !got.Parts[0].Overridden {
		t.Fatalf("snapshot prompts = %v", got)
	}
	sub, _ := h.Subscribe(0, h.Epoch())
	defer sub.Close()
	f.setPrompts(&athenav1.PromptsView{Revision: 2, Parts: []*athenav1.PromptPartView{{Name: "worker", Overridden: true, DefaultUpdated: true}}})
	h.OnCommit(store.Changes{Prompts: true})
	d := next(t, sub)
	if p := d.GetPrompts(); p.GetRevision() != 2 || !p.Parts[0].DefaultUpdated {
		t.Fatalf("prompts delta = %v", d)
	}
}
