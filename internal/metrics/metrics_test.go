package metrics

import (
	"math"
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"
)

// testClock is a settable wall clock for window tests.
type testClock struct{ now atomicInt64 }

type atomicInt64 struct {
	mu sync.Mutex
	v  int64
}

func (a *atomicInt64) load() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

func (a *atomicInt64) add(d int64) {
	a.mu.Lock()
	a.v += d
	a.mu.Unlock()
}

func newTestRegistry() (*Registry, *testClock) {
	c := &testClock{}
	c.now.v = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC).Unix()
	return newRegistry(c.now.load), c
}

func (c *testClock) advance(d time.Duration) { c.now.add(int64(d / time.Second)) }

func TestHistogramPercentiles(t *testing.T) {
	r, _ := newTestRegistry()
	h := r.Histogram("engine_command_seconds", "command", "x")
	// 10k samples spread log-uniformly over 100 µs … 1 s.
	rng := rand.New(rand.NewSource(1))
	samples := make([]float64, 10000)
	for i := range samples {
		samples[i] = math.Exp(math.Log(1e-4) + rng.Float64()*(math.Log(1)-math.Log(1e-4)))
		h.ObserveValue(samples[i])
	}
	slices.Sort(samples)
	exact := func(q float64) float64 { return samples[int(math.Ceil(q*float64(len(samples))))-1] }

	snap := r.Snapshot(0)
	if len(snap.Histograms) != 1 {
		t.Fatalf("histograms = %+v", snap.Histograms)
	}
	hv := snap.Histograms[0]
	if hv.Count != 10000 {
		t.Errorf("count = %d", hv.Count)
	}
	var sum float64
	for _, s := range samples {
		sum += s
	}
	if math.Abs(hv.Sum-sum) > 1e-6*sum {
		t.Errorf("sum = %v, want %v", hv.Sum, sum)
	}
	for _, c := range []struct {
		q   float64
		got float64
	}{{0.50, hv.P50}, {0.95, hv.P95}, {0.99, hv.P99}} {
		want := exact(c.q)
		if d := bucketIndex(c.got) - bucketIndex(want); d < -1 || d > 1 {
			t.Errorf("p%v = %v, exact %v: %d buckets apart", c.q*100, c.got, want, d)
		}
	}
}

func TestHistogramConcurrent(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("dispatch_seconds")
	c := r.Counter("tokens_total", "kind", "input")
	const workers, each = 8, 5000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() { // a reader racing the writers
		for {
			select {
			case <-stop:
				return
			default:
				r.Snapshot(time.Hour)
			}
		}
	}()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				h.Observe(time.Duration(i+1) * time.Microsecond)
				c.Add(2)
			}
		}()
	}
	wg.Wait()
	close(stop)
	snap := r.Snapshot(0)
	if got := snap.Histograms[0].Count; got != workers*each {
		t.Errorf("count = %d, want %d", got, workers*each)
	}
	if got := snap.Counters[0].Value; got != 2*workers*each {
		t.Errorf("counter = %v, want %d", got, 2*workers*each)
	}
}

func TestWindows(t *testing.T) {
	r, clock := newTestRegistry()
	h := r.Histogram("hook_decision_seconds", "event", "PreToolUse", "verdict", "allow")
	c := r.Counter("cost_usd_total", "runtime", "claude", "model", "haiku")
	h.Observe(time.Millisecond)
	c.AddFloat(0.5)

	clock.advance(30 * time.Minute)
	h.Observe(time.Millisecond)
	h.Observe(time.Millisecond)
	c.AddFloat(0.25)

	count := func(w time.Duration) (int64, float64) {
		s := r.Snapshot(w)
		return s.Histograms[0].Count, s.Counters[0].Value
	}
	if n, v := count(time.Hour); n != 3 || v != 0.75 {
		t.Errorf("1h after 30m = %d, %v; want 3, 0.75", n, v)
	}

	clock.advance(45 * time.Minute) // the first sample is now 75 minutes old
	if n, v := count(time.Hour); n != 2 || v != 0.25 {
		t.Errorf("1h after 75m = %d, %v; want 2, 0.25", n, v)
	}
	if n, v := count(24 * time.Hour); n != 3 || v != 0.75 {
		t.Errorf("24h = %d, %v; want 3, 0.75", n, v)
	}

	clock.advance(30 * time.Hour)
	if n, v := count(24 * time.Hour); n != 0 || v != 0 {
		t.Errorf("24h after 30h = %d, %v; want 0, 0", n, v)
	}
	if n, v := count(0); n != 3 || v != 0.75 {
		t.Errorf("since start = %d, %v; want 3, 0.75", n, v)
	}
	// A slot is reused after its period: new samples do not see the old ones.
	h.Observe(time.Millisecond)
	if n, _ := count(time.Hour); n != 1 {
		t.Errorf("1h after reuse = %d, want 1", n)
	}
}

func TestGaugeAndLabels(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("slots_in_use")
	g.Set(3)
	g.Set(2)
	// The same series is returned whatever the label order.
	a := r.Counter("tool_calls_total", "tool", "Bash", "error", "false")
	b := r.Counter("tool_calls_total", "error", "false", "tool", "Bash")
	if a != b {
		t.Error("label order created two series")
	}
	a.Add(1)
	b.Add(1)
	snap := r.Snapshot(0)
	if len(snap.Gauges) != 1 || snap.Gauges[0].Value != 2 {
		t.Errorf("gauges = %+v", snap.Gauges)
	}
	if len(snap.Counters) != 1 || snap.Counters[0].Value != 2 || snap.Counters[0].Labels["tool"] != "Bash" || snap.Counters[0].Labels["error"] != "false" {
		t.Errorf("counters = %+v", snap.Counters)
	}
}

func TestUnknownLabelKeyPanicsInTests(t *testing.T) {
	r := NewRegistry()
	mustPanic := func(what string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", what)
			}
		}()
		fn()
	}
	mustPanic("unknown key", func() { r.Counter("tokens_total", "bogus", "x") })
	mustPanic("odd labels", func() { r.Counter("tokens_total", "kind") })
	mustPanic("kind clash", func() {
		r.Counter("same_name")
		r.Histogram("same_name")
	})
}

func TestSeriesCapCollapsesToOther(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < maxSeriesPerName+50; i++ {
		r.Counter("tool_calls_total", "tool", "tool"+string(rune('A'+i%26))+time.Duration(i).String(), "error", "false").Add(1)
	}
	snap := r.Snapshot(0)
	if len(snap.Counters) > maxSeriesPerName+1 {
		t.Errorf("%d series, want at most %d", len(snap.Counters), maxSeriesPerName+1)
	}
	var other float64
	for _, c := range snap.Counters {
		if c.Labels["tool"] == "other" {
			other = c.Value
		}
	}
	if other != 50 {
		t.Errorf("overflow series = %v, want 50", other)
	}
}

func TestNilRegistryIsFreeNoOp(t *testing.T) {
	var r *Registry
	h := r.Histogram("engine_command_seconds", "command", "x")
	c := r.Counter("tokens_total", "kind", "input")
	g := r.Gauge("slots_in_use")
	if h != nil || c != nil || g != nil {
		t.Fatal("nil registry returned live handles")
	}
	allocs := testing.AllocsPerRun(100, func() {
		r.Histogram("engine_command_seconds", "command", "x").Observe(time.Millisecond)
		r.Counter("tokens_total", "kind", "input").Add(1)
		r.Gauge("slots_in_use").Set(1)
	})
	if allocs != 0 {
		t.Errorf("disabled instrumentation allocates %v", allocs)
	}
	if s := r.Snapshot(time.Hour); len(s.Histograms)+len(s.Counters)+len(s.Gauges) != 0 {
		t.Errorf("nil snapshot = %+v", s)
	}
}

func TestEnabledHotPathDoesNotAllocate(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("hook_decision_seconds", "event", "PreToolUse", "verdict", "allow")
	r.Histogram("engine_command_seconds", "command", "file_report") // series exists: lookups only read
	allocs := testing.AllocsPerRun(200, func() {
		h.Observe(time.Millisecond)
		r.Histogram("engine_command_seconds", "command", "file_report").Observe(time.Millisecond)
	})
	if allocs != 0 {
		t.Errorf("hot path allocates %v", allocs)
	}
}

func BenchmarkHistogramObserve(b *testing.B) {
	h := NewRegistry().Histogram("dispatch_seconds")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.Observe(time.Duration(i%1000+1) * time.Microsecond)
	}
}
