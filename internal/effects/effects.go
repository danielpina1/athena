// Package effects runs the external work that Engine commands enqueue in the store's effects table, after the
// command's transaction has committed.
//
// A Runner owns one goroutine per registered kind. Rows run oldest first by seq; a handler returning nil deletes its
// row, an error reschedules it with exponential backoff (the attempt count is persisted, so a restart continues
// where it stopped), and Later reschedules it without counting a failure. An Ordered kind runs one row at a time and a
// failing row holds the later ones back. The runner sleeps until the earliest due row or a Wake; it never polls.
package effects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/store"
)

// Handler performs one effect. key and payload are those given to Enqueue. It must be idempotent: a crash between the
// handler's side effect and the deletion of its row runs it again.
type Handler func(ctx context.Context, key string, payload []byte) error

// Backoff is the retry delay: attempt n (1-based) waits First * 2^(n-1), at most Max.
type Backoff struct{ First, Max time.Duration }

// Options configure a kind.
type Options struct {
	// Concurrency is the number of rows of the kind that run at once; at least 1. Ignored when Ordered.
	Concurrency int
	// Ordered runs the kind's rows one at a time in seq order; a row that fails or is delayed holds the later ones.
	Ordered bool
	Backoff Backoff
}

// healthAttempts is the number of failures in a row after which a kind is reported unhealthy.
const healthAttempts = 3

// maxLastError bounds the failure text stored with a row.
const maxLastError = 1000

var (
	// storeErrorPause is how long a kind waits after its store queries fail.
	storeErrorPause = time.Second
	// minLater is the shortest delay Later can ask for, so that a handler cannot spin the runner.
	minLater = 100 * time.Millisecond
	// drainGrace is how long Drain waits for handlers after cancelling their context.
	drainGrace = 2 * time.Second
)

// ErrDrainTimeout is returned by Drain when handlers were still running after the timeout and had to be cancelled.
var ErrDrainTimeout = errors.New("effects: drain timed out, handlers cancelled")

// ErrLater matches (errors.Is) the error returned by Later.
var ErrLater = errors.New("effects: later")

type laterError struct{ d time.Duration }

func (e *laterError) Error() string        { return fmt.Sprintf("effects: not yet, retry in %s", e.d) }
func (e *laterError) Is(target error) bool { return target == ErrLater }

// Later is returned by a handler to say the effect cannot be done yet. The row is rescheduled to run after d, and the
// attempt is not counted as a failure.
func Later(d time.Duration) error { return &laterError{d: d} }

// delay returns the Backoff delay for attempt n (1-based).
func (b Backoff) delay(n int) time.Duration {
	d := b.First
	for i := 1; i < n; i++ {
		if d >= b.Max || d > time.Duration(1<<62)/2 {
			return b.Max
		}
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	return d
}

// Runner executes the effects table. Create it with New, register handlers with Handle, then Start it.
type Runner struct {
	st  *store.Store
	log *slog.Logger

	mu       sync.Mutex
	kinds    map[string]*kind
	started  bool
	drained  bool
	ctx      context.Context // handlers' context, set by Start
	cancel   context.CancelFunc
	stopping chan struct{} // closed by Drain
	loops    sync.WaitGroup
}

// New returns a Runner over st. A nil log discards logs.
func New(st *store.Store, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{st: st, log: log, kinds: map[string]*kind{}, stopping: make(chan struct{})}
}

type kind struct {
	r    *Runner
	name string
	h    Handler
	o    Options

	wake  chan struct{} // capacity 1: a pending re-evaluation
	flush chan flushReq

	mu       sync.Mutex
	inflight map[string]bool // keys being handled
	workers  sync.WaitGroup  // Add only from the loop goroutine
}

// Handle registers the handler for a kind; registering a kind twice panics, as it is a wiring bug. It may be called before or after Start; after Drain it has no effect.
func (r *Runner) Handle(name string, h Handler, o Options) {
	if o.Ordered || o.Concurrency < 1 {
		o.Concurrency = 1
	}
	if o.Backoff.First <= 0 {
		o.Backoff.First = time.Second
	}
	if o.Backoff.Max < o.Backoff.First {
		o.Backoff.Max = o.Backoff.First
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.kinds[name]; dup {
		panic("effects: handler registered twice for kind " + name)
	}
	k := &kind{r: r, name: name, h: h, o: o, wake: make(chan struct{}, 1), flush: make(chan flushReq),
		inflight: map[string]bool{}}
	r.kinds[name] = k
	if r.started && !r.drained {
		r.loops.Add(1)
		go k.loop()
	}
}

// Start begins running effects. It returns at once; the work stops when ctx is done or Drain is called. Handlers get a
// context derived from ctx. Start does nothing if the Runner was already started or drained.
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.drained {
		return
	}
	r.started = true
	r.ctx, r.cancel = context.WithCancel(ctx)
	for _, k := range r.kinds {
		r.loops.Add(1)
		go k.loop()
	}
}

// Wake tells the Runner that effects were enqueued (call it after the Engine commit that enqueued them). It never
// blocks.
func (r *Runner) Wake() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range r.kinds {
		k.poke()
	}
}

// Drain stops the Runner. It starts no more effects, waits up to timeout for the ones running, then cancels their
// context and waits a short grace. Rows that did not run, or were interrupted, stay in the table for the next Runner.
// It returns ErrDrainTimeout if handlers had to be cancelled, and is safe to call more than once or before Start.
func (r *Runner) Drain(timeout time.Duration) error {
	r.mu.Lock()
	if !r.drained {
		r.drained = true
		close(r.stopping)
	}
	started := r.started
	r.mu.Unlock()
	if !started {
		return nil
	}
	done := make(chan struct{})
	go func() { r.loops.Wait(); close(done) }()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		r.cancel()
		return nil
	case <-t.C:
	}
	r.cancel()
	g := time.NewTimer(drainGrace)
	defer g.Stop()
	select {
	case <-done:
	case <-g.C:
	}
	return ErrDrainTimeout
}

// Health reports the kinds that need attention: a kind is present, with an error naming it, the key and the last
// failure, while one of its effects has failed three times in a row. The map is empty when all is well.
func (r *Runner) Health() map[string]error {
	rows, err := r.st.FailingEffects(healthAttempts)
	if err != nil {
		return map[string]error{"effects": fmt.Errorf("effects: reading health: %w", err)}
	}
	out := map[string]error{}
	more := map[string]int{}
	for _, e := range rows {
		if _, ok := out[e.Kind]; ok {
			more[e.Kind]++
			continue
		}
		out[e.Kind] = fmt.Errorf("effects: %s %q failed %d times: %s", e.Kind, e.Key, e.Attempts, redact.String(e.LastError))
	}
	for k, n := range more {
		out[k] = fmt.Errorf("%w (and %d more failing)", out[k], n)
	}
	return out
}

func (k *kind) poke() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// loop evaluates the kind whenever it is woken or the earliest row falls due, until the Runner stops.
func (k *kind) loop() {
	r := k.r
	defer r.loops.Done()
	defer k.workers.Wait() // Drain waits for running handlers too
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		sleep := k.pass()
		var fire <-chan time.Time
		if sleep >= 0 {
			timer.Reset(sleep)
			fire = timer.C
		}
		select {
		case <-r.stopping:
			return
		case <-r.ctx.Done():
			return
		case <-k.wake:
		case <-fire:
		case f := <-k.flush:
			k.pass()
			if f.wait {
				k.workers.Wait()
			}
			close(f.ack)
		}
		if fire != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

// pass starts every row that is due and can run, and returns how long to sleep until the next row falls due
// (negative: until woken).
func (k *kind) pass() time.Duration {
	r := k.r
	select {
	case <-r.stopping:
		return -1
	case <-r.ctx.Done():
		return -1
	default:
	}
	sleep, err := k.schedule()
	if err != nil {
		r.log.Error("effects: reading queue", "kind", k.name, "err", redact.String(err.Error()))
		return storeErrorPause
	}
	return sleep
}

func (k *kind) schedule() (time.Duration, error) {
	st := k.r.st
	now := st.Now()
	k.mu.Lock()
	busy := len(k.inflight)
	k.mu.Unlock()

	if k.o.Ordered {
		if busy > 0 {
			return -1, nil // the running row's completion wakes the loop
		}
		head, ok, err := st.HeadEffect(k.name)
		if err != nil || !ok {
			return -1, err
		}
		if head.DueAt.After(now) {
			return head.DueAt.Sub(now), nil
		}
		k.start(head)
		return -1, nil
	}

	if free := k.o.Concurrency - busy; free > 0 {
		due, err := st.DueEffects(k.name, now, k.o.Concurrency+busy)
		if err != nil {
			return -1, err
		}
		for _, e := range due {
			if free == 0 {
				break
			}
			k.mu.Lock()
			running := k.inflight[e.Key]
			k.mu.Unlock()
			if running {
				continue
			}
			k.start(e)
			free--
		}
	}
	next, ok, err := st.NextEffectDue(k.name, now)
	if err != nil || !ok {
		return -1, err
	}
	return next.Sub(now), nil
}

// start runs e in its own goroutine. Only the loop goroutine calls it.
func (k *kind) start(e store.Effect) {
	k.mu.Lock()
	k.inflight[e.Key] = true
	k.mu.Unlock()
	k.workers.Add(1)
	go func() {
		defer k.workers.Done()
		k.run(k.r.ctx, e)
		k.mu.Lock()
		delete(k.inflight, e.Key)
		k.mu.Unlock()
		k.poke() // the freed slot, and anything that fell due meanwhile
	}()
}

// run calls the handler and settles the row.
func (k *kind) run(ctx context.Context, e store.Effect) {
	r := k.r
	log := r.log.With("kind", k.name, "key", e.Key)
	err := k.call(ctx, e)
	var later *laterError
	switch {
	case err == nil:
		if _, serr := r.st.CompleteEffect(k.name, e.Key, e.Payload); serr != nil {
			log.Error("effects: deleting finished effect", "err", redact.String(serr.Error()))
		}
	case errors.As(err, &later):
		d := max(later.d, minLater)
		if _, serr := r.st.DelayEffect(k.name, e.Key, e.Payload, r.st.Now().Add(d)); serr != nil {
			log.Error("effects: delaying effect", "err", redact.String(serr.Error()))
		}
	case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		// Interrupted by shutdown, not a failure: the row stays as it was for the next Runner.
	default:
		msg := truncate(redact.String(err.Error()), maxLastError)
		attempts, due, serr := r.st.FailEffect(k.name, e.Key, e.Payload, msg, r.st.Now(), k.o.Backoff.delay)
		if serr != nil {
			log.Error("effects: recording failure", "err", redact.String(serr.Error()))
			return
		}
		if attempts > 0 {
			log.Warn("effects: effect failed", "attempts", attempts, "retry_at", due, "err", msg)
		}
	}
}

// call runs the handler, turning a panic into an error.
func (k *kind) call(ctx context.Context, e store.Effect) (err error) {
	defer func() {
		if p := recover(); p != nil {
			k.r.log.Error("effects: handler panicked", "kind", k.name, "key", e.Key, "panic", fmt.Sprint(p),
				"stack", string(debug.Stack()))
			err = fmt.Errorf("handler panicked: %v", p)
		}
	}()
	return k.h(ctx, e.Key, e.Payload)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 { // do not split a UTF-8 sequence
		cut--
	}
	return s[:cut] + "..."
}

type flushReq struct {
	wait bool
	ack  chan struct{}
}

// flushKind runs a pass for the kind and waits for its running handlers to finish. Tests use it to settle the Runner
// deterministically.
func (r *Runner) flushKind(name string) { r.flushKindWait(name, true) }

// flushKindNoWait runs a pass for the kind without waiting for the handlers it started.
func (r *Runner) flushKindNoWait(name string) { r.flushKindWait(name, false) }

func (r *Runner) flushKindWait(name string, wait bool) {
	r.mu.Lock()
	k := r.kinds[name]
	r.mu.Unlock()
	f := flushReq{wait: wait, ack: make(chan struct{})}
	k.flush <- f
	<-f.ack
}
