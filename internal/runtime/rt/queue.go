package rt

import (
	"sync"
	"time"
)

const (
	// abandonAfter is how long, once the session's Close has returned, a Queue's pump waits for a consumer before it
	// concludes nobody is reading and drops the rest.
	abandonAfter = 250 * time.Millisecond
	// queueBuffer is the capacity of a Queue's Out channel.
	queueBuffer = 64
)

// Queue holds a session's events until its pump delivers them, so the reader of the runtime's output never waits on
// the consumer: Push never blocks, and events beyond Out's buffer wait in memory. A session pushes from its reader,
// calls Finish when the reader is done, and runs Pump on a goroutine of its own.
type Queue[E any] struct {
	mu         sync.Mutex
	items      []E
	readerDone bool
	notify     chan struct{} // cap 1: the queue changed
	out        chan E
}

// NewQueue returns an empty Queue.
func NewQueue[E any]() *Queue[E] {
	return &Queue[E]{notify: make(chan struct{}, 1), out: make(chan E, queueBuffer)}
}

// Out is the channel Pump delivers to; it is closed once the reader is done and every event was delivered (or
// dropped, see Pump).
func (q *Queue[E]) Out() <-chan E { return q.out }

// Push queues events, in order.
func (q *Queue[E]) Push(evs ...E) {
	if len(evs) == 0 {
		return
	}
	q.mu.Lock()
	q.items = append(q.items, evs...)
	q.mu.Unlock()
	q.wake()
}

// Finish marks the reader done: Pump closes Out once the queue is empty.
func (q *Queue[E]) Finish() {
	q.mu.Lock()
	q.readerDone = true
	q.mu.Unlock()
	q.wake()
}

func (q *Queue[E]) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Pump delivers queued events to Out and closes it once the reader is done and the queue is empty. closed is closed
// once the session's Close has returned: from then on a consumer that stops reading for abandonAfter loses the
// undelivered rest (the transcript still has every line).
func (q *Queue[E]) Pump(closed <-chan struct{}) {
	defer close(q.out)
	abandoned := false
	var zero E
	for {
		q.mu.Lock()
		var ev E
		have := len(q.items) > 0
		if have {
			ev = q.items[0]
			q.items[0] = zero
			q.items = q.items[1:]
		}
		finished := q.readerDone && !have
		q.mu.Unlock()
		if finished {
			return
		}
		if !have {
			<-q.notify
			continue
		}
		if abandoned {
			continue
		}
		select {
		case q.out <- ev:
			continue
		case <-closed:
		}
		// Close has returned: keep delivering to a consumer that is still draining, but give up on one that has gone
		// away.
		t := time.NewTimer(abandonAfter)
		select {
		case q.out <- ev:
			t.Stop()
		case <-t.C:
			abandoned = true
		}
	}
}
