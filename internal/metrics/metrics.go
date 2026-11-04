// Package metrics is Athena's in-process metrics core: counters, gauges and log-bucketed histograms with p50/p95/p99,
// each kept since start and over the last 60 minutes and 24 hours.
//
// A nil *Registry, and the nil handles it returns, are valid and do nothing, so instrumented code needs no checks and
// costs nothing when metrics are off. Handles are cheap to keep: look a series up once and reuse it on hot paths.
package metrics

import (
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// maxSeriesPerName bounds the label combinations of one metric; further ones share an "other" series.
	maxSeriesPerName = 256
	// maxLabels bounds the label pairs of a series.
	maxLabels  = 4
	otherValue = "other"
)

// labelKeys are the label keys a series may carry: the bounded sets of the performance design (runtime, model, node,
// tool, outcome, repo) and the dimensions of the instrumented operations.
var labelKeys = map[string]bool{
	"runtime": true, "model": true, "node": true, "tool": true, "outcome": true, "repo": true,
	"command": true, "event": true, "verdict": true, "kind": true, "error": true,
}

type kind uint8

const (
	kindCounter kind = iota + 1
	kindGauge
	kindHistogram
)

type label struct{ key, value string }

type entry struct {
	kind    kind
	name    string
	labels  []label // sorted by key
	counter *Counter
	gauge   *Gauge
	hist    *Histogram
}

// Registry holds every series. It is safe for concurrent use.
type Registry struct {
	clock func() int64 // Unix seconds

	mu      sync.RWMutex
	series  map[string]*entry
	perName map[string]int
	kinds   map[string]kind
}

// NewRegistry returns an empty Registry that rotates its windows by the wall clock.
func NewRegistry() *Registry {
	return newRegistry(func() int64 { return time.Now().Unix() })
}

func newRegistry(clock func() int64) *Registry {
	return &Registry{clock: clock, series: map[string]*entry{}, perName: map[string]int{}, kinds: map[string]kind{}}
}

// bad reports a misuse of labels or kinds: it panics in test binaries, so mistakes fail the tests, and is ignored in
// production, where a metrics mistake must never take the server down.
func bad(msg string) {
	if testing.Testing() {
		panic("metrics: " + msg)
	}
}

// Counter returns the counter of name and labels (alternating key, value), creating it on first use.
func (r *Registry) Counter(name string, labels ...string) *Counter {
	if r == nil {
		return nil
	}
	return r.get(kindCounter, name, labels).counter
}

// Gauge returns the gauge of name and labels, creating it on first use.
func (r *Registry) Gauge(name string, labels ...string) *Gauge {
	if r == nil {
		return nil
	}
	return r.get(kindGauge, name, labels).gauge
}

// Histogram returns the histogram of name and labels, creating it on first use.
func (r *Registry) Histogram(name string, labels ...string) *Histogram {
	if r == nil {
		return nil
	}
	return r.get(kindHistogram, name, labels).hist
}

// get finds or creates a series. Finding one allocates nothing.
func (r *Registry) get(k kind, name string, labels []string) *entry {
	if len(labels)%2 != 0 {
		bad("odd number of label arguments for " + name)
		labels = labels[:len(labels)-1]
	}
	var pairs [maxLabels]label
	n := 0
	for i := 0; i+1 < len(labels); i += 2 {
		if !labelKeys[labels[i]] {
			bad("unknown label key " + labels[i] + " on " + name)
			continue
		}
		if n == maxLabels {
			bad("too many labels on " + name)
			break
		}
		// Insertion sort by key, so the order callers pass labels in does not matter.
		j := n
		for ; j > 0 && pairs[j-1].key > labels[i]; j-- {
			pairs[j] = pairs[j-1]
		}
		pairs[j] = label{labels[i], labels[i+1]}
		n++
	}
	var buf [192]byte
	key := appendKey(buf[:0], name, pairs[:n])

	r.mu.RLock()
	e := r.series[string(key)] // a map lookup keyed by string(bytes) does not allocate
	r.mu.RUnlock()
	if e != nil {
		if e.kind != k {
			bad("metric " + name + " used as two kinds")
			return r.fallback(k, name)
		}
		return e
	}
	return r.create(k, name, pairs[:n], key)
}

func appendKey(buf []byte, name string, pairs []label) []byte {
	buf = append(buf, name...)
	for _, p := range pairs {
		buf = append(buf, 0)
		buf = append(buf, p.key...)
		buf = append(buf, '=')
		buf = append(buf, p.value...)
	}
	return buf
}

func (r *Registry) create(k kind, name string, pairs []label, key []byte) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.series[string(key)]; e != nil {
		if e.kind != k {
			bad("metric " + name + " used as two kinds")
			return r.fallbackLocked(k, name)
		}
		return e
	}
	if prev, ok := r.kinds[name]; ok && prev != k {
		bad("metric " + name + " used as two kinds")
		return r.fallbackLocked(k, name)
	}
	r.kinds[name] = k
	if r.perName[name] >= maxSeriesPerName {
		// Too many label combinations: share one series whose label values are all "other".
		other := make([]label, len(pairs))
		for i, p := range pairs {
			other[i] = label{p.key, otherValue}
		}
		okey := appendKey(nil, name, other)
		if e := r.series[string(okey)]; e != nil {
			return e
		}
		return r.add(k, name, other, string(okey))
	}
	r.perName[name]++
	return r.add(k, name, append([]label(nil), pairs...), string(key))
}

func (r *Registry) add(k kind, name string, labels []label, key string) *entry {
	e := &entry{kind: k, name: name, labels: labels}
	switch k {
	case kindCounter:
		e.counter = &Counter{clock: r.clock}
	case kindGauge:
		e.gauge = &Gauge{}
	default:
		e.hist = newHistogram(r.clock)
	}
	r.series[key] = e
	return e
}

// fallback returns a detached series of the kind asked for, so a misuse in production still yields a usable handle.
func (r *Registry) fallback(k kind, name string) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fallbackLocked(k, name)
}

func (r *Registry) fallbackLocked(k kind, name string) *entry {
	key := "\x00detached\x00" + name + string(rune('0'+k))
	if e := r.series[key]; e != nil {
		return e
	}
	return r.add(k, name, nil, key)
}

// Gauge is a value that is set, such as slots in use. A nil *Gauge ignores writes.
type Gauge struct{ bits atomic.Uint64 }

// Set sets the gauge.
func (g *Gauge) Set(v float64) {
	if g == nil {
		return
	}
	g.bits.Store(math.Float64bits(v))
}

// Value returns the last value set.
func (g *Gauge) Value() float64 {
	if g == nil {
		return 0
	}
	return math.Float64frombits(g.bits.Load())
}

type cslot struct {
	epoch atomic.Int64
	bits  atomic.Uint64
}

// Counter is a monotonically increasing sum, kept since start and over the last 60 minutes and 24 hours. A nil
// *Counter ignores additions.
type Counter struct {
	clock func() int64
	total atomic.Uint64
	mu    sync.Mutex
	min   [minuteSlots]cslot
	hour  [hourSlots]cslot
}

// Add adds n.
func (c *Counter) Add(n int64) { c.AddFloat(float64(n)) }

// AddFloat adds v, for quantities such as dollars.
func (c *Counter) AddFloat(v float64) {
	if c == nil {
		return
	}
	addFloat(&c.total, v)
	now := c.clock()
	m, h := now/60, now/3600
	if s := c.slot(&c.min[m%minuteSlots], m); s != nil {
		addFloat(&s.bits, v)
	}
	if s := c.slot(&c.hour[h%hourSlots], h); s != nil {
		addFloat(&s.bits, v)
	}
}

func (c *Counter) slot(s *cslot, e int64) *cslot {
	if s.epoch.Load() == e {
		return s
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch cur := s.epoch.Load(); {
	case cur < e:
		s.bits.Store(0)
		s.epoch.Store(e)
	case cur > e:
		return nil
	}
	return s
}

func (c *Counter) sum(window time.Duration) float64 {
	if window <= 0 {
		return math.Float64frombits(c.total.Load())
	}
	now := c.clock()
	var sum float64
	if window <= time.Hour {
		n := min(int64((window+time.Minute-1)/time.Minute), minuteSlots)
		for i := int64(0); i < n; i++ {
			e := now/60 - i
			if s := &c.min[e%minuteSlots]; s.epoch.Load() == e {
				sum += math.Float64frombits(s.bits.Load())
			}
		}
		return sum
	}
	n := min(int64((window+time.Hour-1)/time.Hour), hourSlots)
	for i := int64(0); i < n; i++ {
		e := now/3600 - i
		if s := &c.hour[e%hourSlots]; s.epoch.Load() == e {
			sum += math.Float64frombits(s.bits.Load())
		}
	}
	return sum
}

// Snapshot is the state of every series over one window. Histogram values are in seconds.
type Snapshot struct {
	// Window is the period the snapshot covers; 0 is since start.
	Window     time.Duration    `json:"window"`
	Counters   []CounterValue   `json:"counters,omitempty"`
	Gauges     []GaugeValue     `json:"gauges,omitempty"`
	Histograms []HistogramValue `json:"histograms,omitempty"`
}

// CounterValue is a counter's sum over the window.
type CounterValue struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

// GaugeValue is a gauge's current value.
type GaugeValue struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

// HistogramValue is a histogram over the window: its sample count and sum and its percentiles, each the upper bound of
// the bucket that holds it (within 19% of the true value).
type HistogramValue struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Count  int64             `json:"count"`
	Sum    float64           `json:"sum"`
	P50    float64           `json:"p50"`
	P95    float64           `json:"p95"`
	P99    float64           `json:"p99"`
}

// Snapshot returns every series over window: 0 for since start, up to an hour for the last that many minutes (rounded
// up to whole minutes), up to 24 hours for that many whole hours; longer windows are cut to 24 hours. Series are sorted
// by name and labels. A nil Registry has an empty Snapshot.
func (r *Registry) Snapshot(window time.Duration) Snapshot {
	snap := Snapshot{Window: window}
	if r == nil {
		return snap
	}
	r.mu.RLock()
	entries := make([]*entry, 0, len(r.series))
	for _, e := range r.series {
		entries = append(entries, e)
	}
	r.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].name != entries[j].name {
			return entries[i].name < entries[j].name
		}
		return labelString(entries[i].labels) < labelString(entries[j].labels)
	})
	for _, e := range entries {
		lm := labelMap(e.labels)
		switch e.kind {
		case kindCounter:
			snap.Counters = append(snap.Counters, CounterValue{Name: e.name, Labels: lm, Value: e.counter.sum(window)})
		case kindGauge:
			snap.Gauges = append(snap.Gauges, GaugeValue{Name: e.name, Labels: lm, Value: e.gauge.Value()})
		default:
			var acc hacc
			e.hist.merge(window, &acc)
			snap.Histograms = append(snap.Histograms, HistogramValue{
				Name: e.name, Labels: lm, Count: acc.count, Sum: acc.sum,
				P50: acc.quantile(0.50), P95: acc.quantile(0.95), P99: acc.quantile(0.99),
			})
		}
	}
	return snap
}

func labelMap(ls []label) map[string]string {
	if len(ls) == 0 {
		return nil
	}
	m := make(map[string]string, len(ls))
	for _, l := range ls {
		m[l.key] = l.value
	}
	return m
}

func labelString(ls []label) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.key + "=" + l.value + ",")
	}
	return b.String()
}
