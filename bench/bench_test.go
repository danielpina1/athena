// Package bench enforces Athena's performance budgets (spec §10.3). `make bench` runs TestBudgets and the benchmarks
// with ATHENA_BENCH=1: it measures hook decisions, Engine commands and dispatch against a fake runtime (no model, no
// network) and the idle RSS of a real `athena server`, writes results.json, fails on a budget breach or a regression
// of more than 10% against baseline.json, and `make bench-baseline` records a new baseline.
package bench

import (
	"os"
	"slices"
	"testing"
	"time"
)

const (
	hookBudget     = 10 * time.Millisecond
	engineBudget   = 5 * time.Millisecond
	dispatchBudget = 100 * time.Millisecond
	rssBudget      = 100 << 20

	resultsFile  = "results.json"
	baselineFile = "baseline.json"

	// rounds is how many times each latency is measured: the reported p99 is the median of the rounds' p99s, which
	// keeps one noisy round from failing a run.
	rounds = 5
	// rssIdle is how long the server idles before its RSS is read: past its second resource sample.
	rssIdle = 11 * time.Second
)

// quantile returns the q-quantile (0 < q <= 1) of the samples by the nearest-rank method.
func quantile(samples []time.Duration, q float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	rank := int(q*float64(len(s)) + 0.999999)
	return s[min(max(rank, 1), len(s))-1]
}

// summarize condenses several rounds of latencies into a Result.
func summarize(name string, budget time.Duration, rounds [][]time.Duration) Result {
	var p50s, p99s, all []time.Duration
	for _, r := range rounds {
		p50s = append(p50s, quantile(r, 0.50))
		p99s = append(p99s, quantile(r, 0.99))
		all = append(all, r...)
	}
	return Result{
		Name: name, Unit: "ns", Samples: len(all) / len(rounds),
		P50: int64(quantile(p50s, 0.5)), P99: int64(quantile(p99s, 0.5)), Max: int64(quantile(all, 1)), Budget: int64(budget),
	}
}

// measureRounds runs one measurement several times.
func measureRounds(name string, budget time.Duration, n int, measure func(n int) []time.Duration) Result {
	var rs [][]time.Duration
	for i := 0; i < rounds; i++ {
		rs = append(rs, measure(n))
	}
	return summarize(name, budget, rs)
}

func TestBudgets(t *testing.T) {
	if os.Getenv("ATHENA_BENCH") == "" {
		t.Skip("set ATHENA_BENCH=1 (make bench) to measure the performance budgets")
	}
	report := newReport(
		measureRounds("hook_decision", hookBudget, 3000, func(n int) []time.Duration { return measureHook(t, n) }),
		measureRounds("engine_command", engineBudget, 500, func(n int) []time.Duration { return measureEngine(t, n) }),
		measureRounds("dispatch", dispatchBudget, 60, func(n int) []time.Duration { return measureDispatch(t, n, false) }),
		measureRounds("dispatch_worktree", dispatchBudget, 40, func(n int) []time.Duration { return measureDispatch(t, n, true) }),
		Result{Name: "server_idle_rss", Unit: "bytes", Value: int64(idleRSS(t, rssIdle)), Budget: rssBudget},
	)
	for _, r := range report.Results {
		if r.Unit == "bytes" {
			t.Logf("%-18s %-24s budget %s", r.Name, show(r.Unit, r.Value), show(r.Unit, r.Budget))
			continue
		}
		t.Logf("%-18s p50 %-12s p99 %-12s budget %s", r.Name, show(r.Unit, r.P50), show(r.Unit, r.value()), show(r.Unit, r.Budget))
	}
	if err := writeReport(resultsFile, report); err != nil {
		t.Fatal(err)
	}

	for _, msg := range breaches(report) {
		t.Errorf("budget breach: %s", msg)
	}
	if os.Getenv("ATHENA_BENCH_UPDATE") != "" {
		if err := writeReport(baselineFile, report); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %s", baselineFile)
		return
	}
	base, err := readReport(baselineFile)
	if err != nil {
		t.Fatalf("no baseline (%v): record one with make bench-baseline", err)
	}
	if base.GOOS != report.GOOS || base.GOARCH != report.GOARCH {
		t.Logf("baseline is from %s/%s, this is %s/%s: regressions not compared", base.GOOS, base.GOARCH, report.GOOS, report.GOARCH)
	}
	for _, msg := range regressions(base, report) {
		t.Errorf("regression: %s", msg)
	}
}

// reportLatencies adds a benchmark's p99 to its results; ns/op is the mean of the measured operations (the setup
// before them is not part of it).
func reportLatencies(b *testing.B, d []time.Duration) {
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	if len(d) > 0 {
		b.ReportMetric(float64(sum)/float64(len(d)), "ns/op")
	}
	b.ReportMetric(float64(quantile(d, 0.99)), "p99-ns")
}

func BenchmarkHookDecision(b *testing.B) { reportLatencies(b, measureHook(b, b.N)) }

func BenchmarkEngineCommand(b *testing.B) { reportLatencies(b, measureEngine(b, b.N)) }

func BenchmarkDispatch(b *testing.B) { reportLatencies(b, measureDispatch(b, b.N, false)) }

// TestHarnessSmoke runs each measurement a few times, so `make check` notices a broken harness without judging
// timings, which race-detector builds distort.
func TestHarnessSmoke(t *testing.T) {
	for name, d := range map[string][]time.Duration{
		"hook":     measureHook(t, 10),
		"engine":   measureEngine(t, 6),
		"dispatch": measureDispatch(t, 2, false),
	} {
		if len(d) == 0 || quantile(d, 0.99) <= 0 {
			t.Errorf("%s: no measurements: %v", name, d)
		}
	}
}

func TestQuantile(t *testing.T) {
	var s []time.Duration
	for i := 100; i >= 1; i-- {
		s = append(s, time.Duration(i))
	}
	for q, want := range map[float64]time.Duration{0.5: 50, 0.99: 99, 1: 100, 0.01: 1} {
		if got := quantile(s, q); got != want {
			t.Errorf("quantile(%v) = %v, want %v", q, got, want)
		}
	}
	if quantile(nil, 0.5) != 0 {
		t.Error("empty quantile must be 0")
	}
}

func TestCompare(t *testing.T) {
	ms := int64(time.Millisecond)
	base := newReport(
		Result{Name: "hook_decision", Unit: "ns", P99: 2 * ms, Budget: 10 * ms},
		Result{Name: "tiny", Unit: "ns", P99: 100 * int64(time.Microsecond), Budget: 10 * ms},
		Result{Name: "server_idle_rss", Unit: "bytes", Value: 40 << 20, Budget: 100 << 20},
	)
	cur := func(hook, tiny, rss int64) Report {
		return newReport(
			Result{Name: "hook_decision", Unit: "ns", P99: hook, Budget: 10 * ms},
			Result{Name: "tiny", Unit: "ns", P99: tiny, Budget: 10 * ms},
			Result{Name: "server_idle_rss", Unit: "bytes", Value: rss, Budget: 100 << 20},
		)
	}
	tiny := 100 * int64(time.Microsecond)
	for _, c := range []struct {
		name string
		cur  Report
		want int // regressions
	}{
		{"same", cur(2*ms, tiny, 40<<20), 0},
		{"9% slower", cur(2*ms+ms/6, tiny, 40<<20), 0},
		{"30% slower", cur(2*ms+ms*6/10, tiny, 40<<20), 1},
		{"20% slower but inside the noise floor", cur(2*ms+ms*4/10, tiny, 40<<20), 0},
		{"faster", cur(ms, tiny, 30<<20), 0},
		{"doubling inside the noise floor", cur(2*ms, 2*tiny, 40<<20), 0},
		{"rss +20%", cur(2*ms, tiny, 48<<20), 1},
		{"rss +1 MB is noise", cur(2*ms, tiny, 41<<20), 0},
		{"everything worse", cur(3*ms, tiny, 60<<20), 2},
	} {
		if got := regressions(base, c.cur); len(got) != c.want {
			t.Errorf("%s: regressions = %v, want %d", c.name, got, c.want)
		}
	}
	if got := regressions(base, newReport(base.Results[0])); len(got) != 2 {
		t.Errorf("missing results = %v, want 2", got)
	}
	other := base
	other.GOOS = "plan9"
	if got := regressions(other, cur(9*ms, tiny, 90<<20)); len(got) != 0 {
		t.Errorf("a baseline from another platform was compared: %v", got)
	}
	if got := breaches(cur(11*ms, tiny, 101<<20)); len(got) != 2 {
		t.Errorf("breaches = %v, want 2", got)
	}
	if got := breaches(cur(10*ms, tiny, 100<<20)); len(got) != 0 {
		t.Errorf("at the budget is not a breach: %v", got)
	}
}
