package metrics

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Histogram layout: fixed log-scale buckets with ratio 2^(1/4), from 1 µs to 1 h. Bucket i holds the values in
// (upperBound(i-1), upperBound(i)]; bucket 0 also takes everything at or below 1 µs and the last bucket everything
// above 1 h. Values are in seconds (Observe converts a Duration), but any quantity in that range works.
const (
	minValue         = 1e-6
	bucketsPerOctave = 4
	// numBuckets covers minValue*2^((numBuckets-1)/4) >= 1 h: 4*log2(3.6e9) = 126.99, so 128 buckets.
	numBuckets = 128

	minuteSlots = 60
	hourSlots   = 24
)

// bounds[i] is the upper bound of bucket i.
var bounds = func() (b [numBuckets]float64) {
	for i := range b {
		b[i] = minValue * math.Pow(2, float64(i)/bucketsPerOctave)
	}
	return b
}()

// bucketIndex returns the bucket v falls in.
func bucketIndex(v float64) int {
	if !(v > minValue) { // also NaN
		return 0
	}
	i := int(math.Ceil(bucketsPerOctave * math.Log2(v/minValue)))
	// Guard the rounding of Log2 at an exact bound.
	if i > 0 && v <= bounds[min(i-1, numBuckets-1)] {
		i--
	}
	return min(i, numBuckets-1)
}

// hslot is the histogram of one period: a minute, an hour or the whole run. Every field is atomic, so observers never
// block each other or a reader.
type hslot struct {
	epoch   atomic.Int64 // the period this slot holds, in minutes or hours since the Unix epoch
	count   atomic.Int64
	sum     atomic.Uint64 // float64 bits
	buckets [numBuckets]atomic.Int64
}

func (s *hslot) add(idx int, v float64) {
	s.buckets[idx].Add(1)
	s.count.Add(1)
	addFloat(&s.sum, v)
}

func (s *hslot) reset() {
	for i := range s.buckets {
		s.buckets[i].Store(0)
	}
	s.count.Store(0)
	s.sum.Store(0)
}

// Histogram is a log-bucketed distribution over the whole run and over the last 60 minutes and 24 hours (60 one-minute
// and 24 one-hour slots, rotated by wall clock). A nil *Histogram ignores observations.
//
// Observing is lock-free except when a slot starts a new period (once a minute or hour per series). An observation
// racing that rotation may land in the period just before or after it.
type Histogram struct {
	clock func() int64
	total hslot
	mu    sync.Mutex // serializes slot rotation
	min   [minuteSlots]atomic.Pointer[hslot]
	hour  [hourSlots]atomic.Pointer[hslot]
}

func newHistogram(clock func() int64) *Histogram { return &Histogram{clock: clock} }

// Observe records a duration.
func (h *Histogram) Observe(d time.Duration) {
	if h == nil {
		return
	}
	h.ObserveValue(d.Seconds())
}

// ObserveValue records a value in seconds (1 µs … 1 h; values outside land in the first or last bucket).
func (h *Histogram) ObserveValue(v float64) {
	if h == nil {
		return
	}
	idx := bucketIndex(v)
	h.total.add(idx, v)
	now := h.clock()
	m, hr := now/60, now/3600
	if s := h.slot(&h.min[m%minuteSlots], m); s != nil {
		s.add(idx, v)
	}
	if s := h.slot(&h.hour[hr%hourSlots], hr); s != nil {
		s.add(idx, v)
	}
}

// slot returns the slot for period e, starting it if it still holds an older period; nil if it already moved past e.
func (h *Histogram) slot(p *atomic.Pointer[hslot], e int64) *hslot {
	if s := p.Load(); s != nil && s.epoch.Load() == e {
		return s
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := p.Load()
	switch {
	case s == nil:
		s = new(hslot)
		s.epoch.Store(e)
		p.Store(s)
	case s.epoch.Load() < e:
		s.reset()
		s.epoch.Store(e)
	case s.epoch.Load() > e:
		return nil
	}
	return s
}

// merge adds the slots covering window into acc. window 0 is the whole run; up to an hour it merges that many
// one-minute slots (the current one included), up to 24 hours one-hour slots.
func (h *Histogram) merge(window time.Duration, acc *hacc) {
	if window <= 0 {
		acc.add(&h.total)
		return
	}
	now := h.clock()
	if window <= time.Hour {
		n := min(int64((window+time.Minute-1)/time.Minute), minuteSlots)
		for i := int64(0); i < n; i++ {
			e := now/60 - i
			if s := h.min[e%minuteSlots].Load(); s != nil && s.epoch.Load() == e {
				acc.add(s)
			}
		}
		return
	}
	n := min(int64((window+time.Hour-1)/time.Hour), hourSlots)
	for i := int64(0); i < n; i++ {
		e := now/3600 - i
		if s := h.hour[e%hourSlots].Load(); s != nil && s.epoch.Load() == e {
			acc.add(s)
		}
	}
}

// hacc accumulates slots for a snapshot.
type hacc struct {
	count   int64
	sum     float64
	buckets [numBuckets]int64
}

func (a *hacc) add(s *hslot) {
	for i := range s.buckets {
		a.buckets[i] += s.buckets[i].Load()
	}
	a.count += s.count.Load()
	a.sum += math.Float64frombits(s.sum.Load())
}

// quantile returns the upper bound of the bucket holding the q-th quantile (0 < q <= 1), or 0 without samples. The
// true value is within one bucket (a factor of 2^(1/4), 19%) below it.
func (a *hacc) quantile(q float64) float64 {
	var total int64
	for _, c := range a.buckets {
		total += c
	}
	if total == 0 {
		return 0
	}
	target := int64(math.Ceil(q * float64(total)))
	target = max(target, 1)
	var cum int64
	for i, c := range a.buckets {
		cum += c
		if cum >= target {
			return bounds[i]
		}
	}
	return bounds[numBuckets-1]
}

// addFloat adds v to the float64 stored as bits in p.
func addFloat(p *atomic.Uint64, v float64) {
	for {
		old := p.Load()
		if p.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}
