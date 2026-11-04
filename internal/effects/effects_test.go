package effects

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/store"
)

const wait = 10 * time.Second

var t0 = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

// clock is a settable time source for the store.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openStore(t *testing.T, path string, clk *clock) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if clk != nil {
		st.SetClock(clk.Now)
	}
	return st
}

// newEnv returns a store on a fake clock and a Runner over it.
func newEnv(t *testing.T) (*store.Store, *clock, *Runner) {
	t.Helper()
	clk := &clock{t: t0}
	st := openStore(t, filepath.Join(t.TempDir(), "athena.db"), clk)
	return st, clk, New(st, nil)
}

func enqueue(t *testing.T, st *store.Store, kind, key, payload string) {
	t.Helper()
	if err := st.Repos().Tx(func(tx core.Tx) error { return tx.Enqueue(kind, key, []byte(payload)) }); err != nil {
		t.Fatal(err)
	}
}

func rows(t *testing.T, st *store.Store) []store.Effect {
	t.Helper()
	r, err := st.ListEffects()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func start(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	t.Cleanup(func() {
		cancel()
		if err := r.Drain(wait); err != nil {
			t.Errorf("Drain: %v", err)
		}
	})
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(wait):
		t.Fatal("timed out waiting for the handler")
		panic("unreachable")
	}
}

var fast = Options{Backoff: Backoff{First: 2 * time.Second, Max: 20 * time.Second}}

func TestRunsAfterWake(t *testing.T) {
	st, _, r := newEnv(t)
	got := make(chan string, 4)
	r.Handle("reply", func(_ context.Context, key string, payload []byte) error {
		got <- key + "=" + string(payload)
		return nil
	}, fast)
	start(t, r)

	enqueue(t, st, "reply", "k", "hello")
	r.Wake()
	if g := recv(t, got); g != "k=hello" {
		t.Fatalf("handler got %q", g)
	}
	r.flushKind("reply")
	if left := rows(t, st); len(left) != 0 {
		t.Fatalf("rows after success = %+v, want none", left)
	}
}

func TestEmptyPayloadRuns(t *testing.T) {
	st, _, r := newEnv(t)
	got := make(chan int, 1)
	r.Handle("notice", func(_ context.Context, _ string, payload []byte) error { got <- len(payload); return nil }, fast)
	start(t, r)
	enqueue(t, st, "notice", "k", "")
	r.Wake()
	if n := recv(t, got); n != 0 {
		t.Fatalf("payload length = %d", n)
	}
	r.flushKind("notice")
	if left := rows(t, st); len(left) != 0 {
		t.Fatalf("rows = %+v, want none", left)
	}
}

func TestBackoffDelay(t *testing.T) {
	b := Backoff{First: 2 * time.Second, Max: 20 * time.Second}
	for n, want := range map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second,
		4: 16 * time.Second, 5: 20 * time.Second, 6: 20 * time.Second, 70: 20 * time.Second, 5000: 20 * time.Second} {
		if got := b.delay(n); got != want {
			t.Errorf("delay(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestRetryBackoffDoubles(t *testing.T) {
	st, clk, r := newEnv(t)
	var calls int
	r.Handle("reply", func(context.Context, string, []byte) error { calls++; return errors.New("boom") }, fast)
	start(t, r)
	enqueue(t, st, "reply", "k", "p")

	for i, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 20 * time.Second, 20 * time.Second} {
		r.Wake()
		r.flushKind("reply")
		got := rows(t, st)
		if len(got) != 1 {
			t.Fatalf("rows = %+v", got)
		}
		if got[0].Attempts != i+1 || got[0].LastError != "boom" {
			t.Fatalf("after run %d: attempts = %d, last_error = %q", i+1, got[0].Attempts, got[0].LastError)
		}
		if due := got[0].DueAt; !due.Equal(clk.Now().Add(want)) {
			t.Fatalf("after run %d: due %v, want now+%v", i+1, due.Sub(clk.Now()), want)
		}
		// Not due yet: a wake must not run it again.
		r.Wake()
		r.flushKind("reply")
		if calls != i+1 {
			t.Fatalf("handler ran %d times before the backoff elapsed, want %d", calls, i+1)
		}
		clk.Advance(want)
	}
}

func TestTimerRunsRowWhenDue(t *testing.T) {
	// Real clock, tiny backoff: the runner must wake itself when the retry falls due, with no Wake.
	st := openStore(t, filepath.Join(t.TempDir(), "athena.db"), nil)
	r := New(st, nil)
	calls := make(chan int, 4)
	n := 0
	r.Handle("reply", func(context.Context, string, []byte) error {
		n++
		calls <- n
		if n < 3 {
			return errors.New("not yet")
		}
		return nil
	}, Options{Backoff: Backoff{First: 5 * time.Millisecond, Max: 20 * time.Millisecond}})
	start(t, r)
	enqueue(t, st, "reply", "k", "p")
	r.Wake()
	for want := 1; want <= 3; want++ {
		if got := recv(t, calls); got != want {
			t.Fatalf("call %d, want %d", got, want)
		}
	}
	r.flushKind("reply")
	if left := rows(t, st); len(left) != 0 {
		t.Fatalf("rows = %+v", left)
	}
}

func TestLaterDoesNotCountFailure(t *testing.T) {
	if !errors.Is(Later(time.Second), ErrLater) || errors.Is(errors.New("x"), ErrLater) {
		t.Fatal("Later must match ErrLater, and only it")
	}
	st, clk, r := newEnv(t)
	var calls int
	r.Handle("assessment", func(context.Context, string, []byte) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("waiting for the router: %w", Later(30*time.Second))
		}
		return nil
	}, fast)
	start(t, r)
	enqueue(t, st, "assessment", "k", "p")

	r.Wake()
	r.flushKind("assessment")
	got := rows(t, st)
	if len(got) != 1 || got[0].Attempts != 0 || got[0].LastError != "" || !got[0].DueAt.Equal(clk.Now().Add(30*time.Second)) {
		t.Fatalf("rows after Later = %+v, want attempts 0, no error, due now+30s", got)
	}
	if h := r.Health(); len(h) != 0 {
		t.Fatalf("Health = %v", h)
	}
	clk.Advance(29 * time.Second)
	r.Wake()
	r.flushKind("assessment")
	if calls != 1 {
		t.Fatalf("ran again before the delay: %d calls", calls)
	}
	clk.Advance(time.Second)
	r.Wake()
	r.flushKind("assessment")
	if calls != 2 || len(rows(t, st)) != 0 {
		t.Fatalf("calls = %d, rows = %+v, want 2 calls and no rows", calls, rows(t, st))
	}
}

func TestOrderedHoldsBackLater(t *testing.T) {
	st, clk, r := newEnv(t)
	var mu sync.Mutex
	var calls []string
	failA := true
	r.Handle("relay", func(_ context.Context, key string, _ []byte) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, key)
		if key == "a" && failA {
			failA = false
			return errors.New("down")
		}
		return nil
	}, Options{Ordered: true, Concurrency: 8, Backoff: Backoff{First: 2 * time.Second, Max: 10 * time.Second}})
	start(t, r)
	enqueue(t, st, "relay", "a", "1")
	enqueue(t, st, "relay", "b", "2")

	r.Wake()
	r.flushKind("relay")
	r.Wake()
	r.flushKind("relay")
	mu.Lock()
	if fmt.Sprint(calls) != "[a]" {
		t.Fatalf("calls = %v, want only a: b must wait behind the failing row", calls)
	}
	mu.Unlock()

	clk.Advance(2 * time.Second)
	r.Wake()
	r.flushKind("relay")
	r.flushKind("relay")
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(calls) != "[a a b]" {
		t.Fatalf("calls = %v, want [a a b]", calls)
	}
	if left := rows(t, st); len(left) != 0 {
		t.Fatalf("rows = %+v", left)
	}
}

func TestOrderedRunsOneAtATime(t *testing.T) {
	st, _, r := newEnv(t)
	started := make(chan string, 4)
	release := make(chan struct{})
	var active, maxActive atomic.Int32
	r.Handle("reply", func(_ context.Context, key string, _ []byte) error {
		n := active.Add(1)
		defer active.Add(-1)
		for m := maxActive.Load(); n > m && !maxActive.CompareAndSwap(m, n); m = maxActive.Load() {
		}
		started <- key
		<-release
		return nil
	}, Options{Ordered: true, Concurrency: 5, Backoff: fast.Backoff})
	start(t, r)
	for _, k := range []string{"a", "b", "c"} {
		enqueue(t, st, "reply", k, "p")
	}
	r.Wake()
	if k := recv(t, started); k != "a" {
		t.Fatalf("first = %s", k)
	}
	r.Wake()
	r.flushKindNoWait("reply")
	release <- struct{}{}
	if k := recv(t, started); k != "b" {
		t.Fatalf("second = %s", k)
	}
	release <- struct{}{}
	if k := recv(t, started); k != "c" {
		t.Fatalf("third = %s", k)
	}
	release <- struct{}{}
	r.flushKind("reply")
	if m := maxActive.Load(); m != 1 {
		t.Fatalf("%d handlers ran at once, want 1", m)
	}
}

func TestConcurrencyLimitAndNoDoubleRun(t *testing.T) {
	st, _, r := newEnv(t)
	started := make(chan string, 8)
	release := make(chan struct{})
	var mu sync.Mutex
	runs := map[string]int{}
	var active, maxActive atomic.Int32
	r.Handle("assessment", func(_ context.Context, key string, _ []byte) error {
		n := active.Add(1)
		defer active.Add(-1)
		for m := maxActive.Load(); n > m && !maxActive.CompareAndSwap(m, n); m = maxActive.Load() {
		}
		mu.Lock()
		runs[key]++
		mu.Unlock()
		started <- key
		<-release
		return nil
	}, Options{Concurrency: 2, Backoff: fast.Backoff})
	start(t, r)
	for _, k := range []string{"a", "b", "c", "d"} {
		enqueue(t, st, "assessment", k, "p")
	}
	r.Wake()
	first := []string{recv(t, started), recv(t, started)}
	if fmt.Sprint(first) != "[a b]" && fmt.Sprint(first) != "[b a]" {
		t.Fatalf("first two = %v, want a and b", first)
	}
	// Extra passes while a and b run must not start them again or exceed the limit.
	for i := 0; i < 3; i++ {
		r.Wake()
		r.flushKindNoWait("assessment")
	}
	mu.Lock()
	if len(runs) != 2 {
		t.Fatalf("runs = %v, want only a and b started", runs)
	}
	mu.Unlock()
	close(release)
	recv(t, started)
	recv(t, started)
	r.flushKind("assessment")
	mu.Lock()
	defer mu.Unlock()
	for k, n := range runs {
		if n != 1 {
			t.Errorf("%s ran %d times", k, n)
		}
	}
	if m := maxActive.Load(); m != 2 {
		t.Fatalf("%d handlers ran at once, want 2", m)
	}
	if len(runs) != 4 || len(rows(t, st)) != 0 {
		t.Fatalf("runs = %v, rows = %+v", runs, rows(t, st))
	}
}

func TestFailingRowDoesNotBlockUnorderedKind(t *testing.T) {
	st, _, r := newEnv(t)
	got := make(chan string, 4)
	r.Handle("assessment", func(_ context.Context, key string, _ []byte) error {
		got <- key
		if key == "a" {
			return errors.New("bad")
		}
		return nil
	}, Options{Concurrency: 1, Backoff: fast.Backoff})
	start(t, r)
	enqueue(t, st, "assessment", "a", "p")
	enqueue(t, st, "assessment", "b", "p")
	r.Wake()
	recv(t, got)
	recv(t, got)
	r.flushKind("assessment")
	left := rows(t, st)
	if len(left) != 1 || left[0].Key != "a" {
		t.Fatalf("rows = %+v, want only a left", left)
	}
}

func TestHealthAfterThreeFailures(t *testing.T) {
	st, clk, r := newEnv(t)
	r.Handle("reply", func(context.Context, string, []byte) error {
		return errors.New("POST failed: Authorization: Bearer ghs_supersecrettoken123")
	}, fast)
	start(t, r)
	enqueue(t, st, "reply", "owner/repo#7", "p")

	for i := 1; i <= 3; i++ {
		if h := r.Health(); len(h) != 0 {
			t.Fatalf("Health before failure %d = %v, want empty", i, h)
		}
		r.Wake()
		r.flushKind("reply")
		clk.Advance(time.Minute)
	}
	h := r.Health()
	err := h["reply"]
	if err == nil || len(h) != 1 {
		t.Fatalf("Health = %v, want an error for reply", h)
	}
	for _, want := range []string{"reply", "owner/repo#7", "3 times", "POST failed", "***"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Errorf("error leaks the token: %q", err)
	}
	if left := rows(t, st); strings.Contains(left[0].LastError, "supersecret") {
		t.Errorf("stored last_error leaks the token: %q", left[0].LastError)
	}

	// A newer message for the key resets its failure record, so the kind is healthy again.
	enqueue(t, st, "reply", "owner/repo#7", "p2")
	if h := r.Health(); len(h) != 0 {
		t.Fatalf("Health after replacement = %v", h)
	}
}

func TestPanicIsAFailure(t *testing.T) {
	st, _, r := newEnv(t)
	got := make(chan string, 4)
	r.Handle("notice", func(_ context.Context, key string, _ []byte) error {
		got <- key
		if key == "a" {
			panic("kaboom")
		}
		return nil
	}, Options{Ordered: false, Concurrency: 2, Backoff: fast.Backoff})
	start(t, r)
	enqueue(t, st, "notice", "a", "p")
	enqueue(t, st, "notice", "b", "p")
	r.Wake()
	recv(t, got)
	recv(t, got)
	r.flushKind("notice")
	left := rows(t, st)
	if len(left) != 1 || left[0].Key != "a" || left[0].Attempts != 1 || !strings.Contains(left[0].LastError, "kaboom") {
		t.Fatalf("rows = %+v, want a recorded as failed with the panic, b done", left)
	}
}

func TestReplacedWhileRunningRunsAgain(t *testing.T) {
	st, _, r := newEnv(t)
	started := make(chan string, 4)
	release := make(chan struct{})
	r.Handle("reply", func(_ context.Context, _ string, payload []byte) error {
		started <- string(payload)
		if string(payload) == "old" {
			<-release
		}
		return nil
	}, fast)
	start(t, r)
	enqueue(t, st, "reply", "k", "old")
	r.Wake()
	if p := recv(t, started); p != "old" {
		t.Fatalf("payload %q", p)
	}
	enqueue(t, st, "reply", "k", "new")
	r.Wake()
	close(release)
	if p := recv(t, started); p != "new" {
		t.Fatalf("second payload %q, want new: the replacement must not be lost", p)
	}
	r.flushKind("reply")
	if left := rows(t, st); len(left) != 0 {
		t.Fatalf("rows = %+v", left)
	}
}

func TestSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "athena.db")
	clk := &clock{t: t0}
	st := openStore(t, path, clk)
	r1 := New(st, nil)
	r1.Handle("reply", func(context.Context, string, []byte) error { return errors.New("down") }, fast)
	ctx1, cancel1 := context.WithCancel(context.Background())
	r1.Start(ctx1)
	enqueue(t, st, "reply", "k", "p")
	r1.Wake()
	r1.flushKind("reply")
	cancel1()
	if err := r1.Drain(wait); err != nil {
		t.Fatal(err)
	}
	enqueue(t, st, "reply", "fresh", "q") // enqueued while no runner is running
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	clk.Advance(time.Minute)
	st2 := openStore(t, path, clk)
	if got := rows(t, st2); len(got) != 2 || got[0].Attempts != 1 {
		t.Fatalf("rows after reopen = %+v, want the failure record kept", got)
	}
	got := make(chan string, 4)
	r2 := New(st2, nil)
	r2.Handle("reply", func(_ context.Context, key string, _ []byte) error { got <- key; return nil }, fast)
	start(t, r2) // no Wake: Start itself picks up what is due
	if a, b := recv(t, got), recv(t, got); a != "k" || b != "fresh" {
		t.Fatalf("ran %s then %s, want k then fresh", a, b)
	}
	r2.flushKind("reply")
	if left := rows(t, st2); len(left) != 0 {
		t.Fatalf("rows = %+v", left)
	}
}

func TestDrainKeepsPendingRows(t *testing.T) {
	st, _, r := newEnv(t)
	started := make(chan string, 4)
	release := make(chan struct{})
	var mu sync.Mutex
	var ran []string
	r.Handle("reply", func(_ context.Context, key string, _ []byte) error {
		mu.Lock()
		ran = append(ran, key)
		mu.Unlock()
		started <- key
		<-release
		return nil
	}, Options{Ordered: true, Backoff: fast.Backoff})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	enqueue(t, st, "reply", "a", "1")
	enqueue(t, st, "reply", "b", "2")
	r.Wake()
	recv(t, started)

	drained := make(chan error, 1)
	go func() { drained <- r.Drain(wait) }()
	// Release only once Drain marked the Runner drained: a handler released earlier would let the loop start b, which
	// then blocks on release until Drain times out.
	deadline := time.Now().Add(wait)
	for {
		r.mu.Lock()
		marked := r.drained
		r.mu.Unlock()
		if marked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Drain never marked the Runner drained")
		}
		time.Sleep(time.Millisecond)
	}
	release <- struct{}{} // the running handler finishes within the timeout
	if err := recv(t, drained); err != nil {
		t.Fatalf("Drain = %v, want nil", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(ran) != "[a]" {
		t.Fatalf("ran = %v: draining must not start b", ran)
	}
	left := rows(t, st)
	if len(left) != 1 || left[0].Key != "b" || left[0].Attempts != 0 {
		t.Fatalf("rows = %+v, want only b, untouched", left)
	}
	r.Wake() // after Drain nothing runs and nothing blocks
	if err := r.Drain(time.Second); err != nil {
		t.Fatalf("second Drain = %v", err)
	}
}

func TestDrainCancelsStuckHandlerAndKeepsRow(t *testing.T) {
	st, _, r := newEnv(t)
	started := make(chan struct{}, 1)
	r.Handle("reply", func(ctx context.Context, _ string, _ []byte) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}, fast)
	r.Start(context.Background())
	enqueue(t, st, "reply", "a", "1")
	r.Wake()
	recv(t, started)
	if err := r.Drain(20 * time.Millisecond); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("Drain = %v, want ErrDrainTimeout", err)
	}
	left := rows(t, st)
	if len(left) != 1 || left[0].Attempts != 0 || left[0].LastError != "" {
		t.Fatalf("rows = %+v, want the interrupted row untouched, not a failure", left)
	}
}

func TestContextCancelStopsRunner(t *testing.T) {
	_, _, r := newEnv(t)
	r.Handle("reply", func(context.Context, string, []byte) error { return nil }, fast)
	r.Handle("notice", func(context.Context, string, []byte) error { return nil }, fast)
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	cancel()
	if err := r.Drain(wait); err != nil {
		t.Fatalf("Drain after cancel = %v", err)
	}
}

func TestDrainBeforeStart(t *testing.T) {
	_, _, r := newEnv(t)
	if err := r.Drain(time.Second); err != nil {
		t.Fatal(err)
	}
	r.Start(context.Background()) // ignored after Drain
	r.Wake()
}

func TestKindsAreIndependent(t *testing.T) {
	st, _, r := newEnv(t)
	got := make(chan string, 4)
	r.Handle("reply", func(context.Context, string, []byte) error { return errors.New("down") }, Options{Ordered: true, Backoff: fast.Backoff})
	r.Handle("notice", func(_ context.Context, key string, _ []byte) error { got <- key; return nil }, Options{Ordered: true, Backoff: fast.Backoff})
	start(t, r)
	enqueue(t, st, "reply", "a", "p")
	enqueue(t, st, "notice", "n", "p")
	r.Wake()
	if k := recv(t, got); k != "n" {
		t.Fatalf("notice key %q", k)
	}
}

func TestPendingAndCompleteOutsideHandler(t *testing.T) {
	st, _, r := newEnv(t)
	enqueue(t, st, "notice", "b", "2")
	enqueue(t, st, "relay", "x", "r")
	enqueue(t, st, "notice", "a", "1")
	got, err := r.Pending("notice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "b" || got[1].Key != "a" {
		t.Fatalf("pending = %+v, want b then a", got)
	}
	// A row replaced since it was read stays.
	enqueue(t, st, "notice", "a", "1b")
	if done, err := r.Complete("notice", "a", []byte("1")); err != nil || done {
		t.Fatalf("complete of a replaced row: %v %v", done, err)
	}
	if done, err := r.Complete("notice", "b", []byte("2")); err != nil || !done {
		t.Fatalf("complete: %v %v", done, err)
	}
	got, _ = r.Pending("notice")
	if len(got) != 1 || got[0].Key != "a" || string(got[0].Payload) != "1b" {
		t.Fatalf("after complete: %+v", got)
	}
}
