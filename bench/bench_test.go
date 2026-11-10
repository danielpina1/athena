// Package bench enforces Athena's performance budgets (spec §10.3). `make bench` runs TestBudgets and the benchmarks
// with ATHENA_BENCH=1: it measures hook decisions, Engine commands and dispatch against a fake runtime (no model, no
// network) and the idle RSS of a real `athena server`, writes results.json, fails on a budget breach or a regression
// of more than 10% against baseline.json (a missing baseline for this platform fails too, unless
// ATHENA_BENCH_ALLOW_NO_BASELINE=1), and `make bench-baseline` records a new baseline. dispatch_worktree is a repo
// task's dispatch net of the time `git worktree add` itself takes, measured alongside.
package bench

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	hookBudget     = 10 * time.Millisecond
	engineBudget   = 5 * time.Millisecond
	dispatchBudget = 100 * time.Millisecond
	rssBudget      = 100 << 20
	// nodeRSSBudget is the idle RSS of a connected `athena node run`; remoteBudget the p99 of a remote dispatch (§10.3
	// of the multi-node spec).
	nodeRSSBudget = 30 << 20
	remoteBudget  = 100 * time.Millisecond
	// linkBudget is the p95 of the dashboard's latencies: a delta, a stream item and a command's result reaching a
	// phone (spec §10.3); the "_ms" and "_mb" in these budgets' names are the names the spec mandates, while a result
	// stores nanoseconds and bytes (its Unit). webRSSBudget the idle RSS with one client connected.
	linkBudget   = 250 * time.Millisecond
	webRSSBudget = 100 << 20

	// Regressions smaller than these are not reported however large they are in percent: they are below what a
	// machine's timing resolves from run to run.
	hookFloor     = 20 * time.Microsecond
	engineFloor   = 20 * time.Microsecond
	dispatchFloor = time.Millisecond
	rssFloor      = 2 << 20
	linkFloor     = 5 * time.Millisecond

	resultsFile  = "results.json"
	baselineFile = "baseline.json"

	// rounds is how many times each latency is measured (see Result for what is reported).
	rounds = 5
	// probeSamples is how many reference measurements a probe takes.
	probeSamples = 15
	// rssIdle is how long the server idles before its RSS is read: past its second resource sample.
	rssIdle = 11 * time.Second
	// webRSSIdle is the settle before the RSS of a server with a connected client is read.
	webRSSIdle = 30 * time.Second
	// linkSamples is how many samples each dashboard latency takes.
	linkSamples = 200
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

// roundStats are the statistics of one round of latencies. raw is the p99 regressions are judged on (p99 is what the
// budget is judged on; they differ when a cost outside Athena's control is taken out of it), probe the round's
// reference cost on this machine at that time (0 if none).
type roundStats struct{ p50, p99, max, raw, probe time.Duration }

func statsOf(samples []time.Duration) roundStats {
	p99 := quantile(samples, 0.99)
	return roundStats{quantile(samples, 0.50), p99, quantile(samples, 1), p99, 0}
}

// summarize condenses several rounds into a Result; floor is its smallest regression.
func summarize(name string, budget, floor time.Duration, samples int, rs []roundStats) Result {
	var p50s, p99s, maxs, raws, probes []time.Duration
	for _, r := range rs {
		p50s = append(p50s, r.p50)
		p99s = append(p99s, r.p99)
		maxs = append(maxs, r.max)
		raws = append(raws, r.raw)
		probes = append(probes, r.probe)
	}
	return Result{
		Name: name, Unit: "ns", Samples: samples,
		P50: int64(quantile(p50s, 0.5)), P99: int64(quantile(p99s, 0.5)), Best: int64(slices.Min(raws)), Max: int64(slices.Max(maxs)),
		Probe: int64(quantile(probes, 0.5)), Floor: int64(floor), Budget: int64(budget),
	}
}

// measureRounds runs one measurement several times. probe, if not nil, measures a reference cost of the same kind
// (such as starting a process) right before each round, so that results can be compared across machine load.
func measureRounds(name string, budget, floor time.Duration, n int, measure, probe func(n int) []time.Duration) Result {
	var rs []roundStats
	for i := 0; i < rounds; i++ {
		var pr time.Duration
		if probe != nil {
			pr = quantile(probe(probeSamples), 0.5)
		}
		r := statsOf(measure(n))
		r.probe = pr
		rs = append(rs, r)
	}
	return summarize(name, budget, floor, n, rs)
}

// measureNetRounds is measureRounds for a latency that includes a cost outside Athena's control, which control
// measures: the budget is judged on the dispatch's p99 minus the control's (a round's p99 net of it), the control is
// the round's probe.
func measureNetRounds(name string, budget, floor time.Duration, n int, measure, control func(n int) []time.Duration) Result {
	var rs []roundStats
	for i := 0; i < rounds; i++ {
		c := statsOf(control(n))
		r := statsOf(measure(n))
		r.p99 = max(r.p99-c.p99, 0)
		r.probe = c.p50
		rs = append(rs, r)
	}
	return summarize(name, budget, floor, n, rs)
}

// measureP95Rounds is measureRounds for a latency judged on its p95 (Result.P95): Best, which regressions are judged
// on, is then the lowest round p95.
func measureP95Rounds(name string, budget, floor time.Duration, n int, measure func(n int) []time.Duration) Result {
	var rs []roundStats
	var p95s []time.Duration
	for i := 0; i < rounds; i++ {
		samples := measure(n)
		r := statsOf(samples)
		r.raw = quantile(samples, 0.95)
		p95s = append(p95s, r.raw)
		rs = append(rs, r)
	}
	res := summarize(name, budget, floor, n, rs)
	res.P95 = int64(quantile(p95s, 0.5))
	return res
}

// retries is how often a result found over its budget or regressed is measured again before the run fails. Timing on a
// shared machine drifts with its load over minutes, which a baseline recorded at another time does not know; a real
// slowdown shows in every attempt, so the best attempt is kept (the lowest p99 and best p99).
const retries = 2

type measurement struct {
	name string
	run  func() Result
}

func TestBudgets(t *testing.T) {
	if os.Getenv("ATHENA_BENCH") == "" {
		t.Skip("set ATHENA_BENCH=1 (make bench) to measure the performance budgets")
	}
	measurements := []measurement{
		{"hook_decision", func() Result {
			return measureRounds("hook_decision", hookBudget, hookFloor, 5000, func(n int) []time.Duration { return measureHook(t, n) }, nil)
		}},
		{"hook_decision_codex", func() Result {
			return measureRounds("hook_decision_codex", hookBudget, hookFloor, 5000, func(n int) []time.Duration { return measureHookCodex(t, n) }, nil)
		}},
		{"engine_command", func() Result {
			return measureRounds("engine_command", engineBudget, engineFloor, 2000, func(n int) []time.Duration { return measureEngine(t, n) }, nil)
		}},
		{"dispatch", func() Result {
			return measureRounds("dispatch", dispatchBudget, dispatchFloor, 100, func(n int) []time.Duration { return measureDispatch(t, n, false) },
				func(n int) []time.Duration { return measureSpawn(t, n) })
		}},
		{"dispatch_worktree", func() Result {
			return measureNetRounds("dispatch_worktree", dispatchBudget, dispatchFloor, 40,
				func(n int) []time.Duration { return measureDispatch(t, n, true) },
				func(n int) []time.Duration { return measureGitWorktree(t, n) })
		}},
		{"server_idle_rss", func() Result {
			return Result{Name: "server_idle_rss", Unit: "bytes", Value: int64(idleRSS(t, rssIdle)), Floor: rssFloor, Budget: rssBudget}
		}},
		{"node_idle_rss", func() Result {
			return Result{Name: "node_idle_rss", Unit: "bytes", Value: int64(nodeIdleRSS(t, rssIdle)), Floor: rssFloor, Budget: nodeRSSBudget}
		}},
		{"dispatch_remote_p99", func() Result {
			return measureRounds("dispatch_remote_p99", remoteBudget, dispatchFloor, 40, func(n int) []time.Duration { return measureRemoteDispatch(t, n) }, nil)
		}},
		{"link_delta_p95_ms", func() Result {
			return measureP95Rounds("link_delta_p95_ms", linkBudget, linkFloor, linkSamples, func(n int) []time.Duration { return measureLinkDelta(t, n) })
		}},
		{"stream_item_p95_ms", func() Result {
			return measureP95Rounds("stream_item_p95_ms", linkBudget, linkFloor, linkSamples, func(n int) []time.Duration { return measureStreamItem(t, n) })
		}},
		{"command_result_p95_ms", func() Result {
			return measureP95Rounds("command_result_p95_ms", linkBudget, linkFloor, linkSamples, func(n int) []time.Duration { return measureCommandResult(t, n) })
		}},
		{"server_idle_rss_web_mb", func() Result {
			return Result{Name: "server_idle_rss_web_mb", Unit: "bytes", Value: int64(webIdleRSS(t, webRSSIdle)), Floor: rssFloor, Budget: webRSSBudget}
		}},
	}
	report := newReport()
	for _, m := range measurements {
		report.Results = append(report.Results, m.run())
	}

	update := os.Getenv("ATHENA_BENCH_UPDATE") != ""
	var base Report
	var baseErr error
	if !update {
		if base, baseErr = readReport(baselineFile); baseErr == nil && !sameHost(base, report) {
			baseErr = fmt.Errorf("no baseline for %s/%s (%s is from %s/%s)", report.GOOS, report.GOARCH, baselineFile, base.GOOS, base.GOARCH)
		}
	}
	// failing lists the names of the results over budget or regressed.
	failing := func() []string {
		var names []string
		for _, r := range report.Results {
			one := Report{Results: []Result{r}}
			if len(breaches(one)) > 0 {
				names = append(names, r.Name)
			}
		}
		if baseErr == nil && !update {
			for _, msg := range regressions(base, report) {
				name, _, _ := strings.Cut(msg, ":")
				names = append(names, name)
			}
		}
		return names
	}
	for attempt := 1; attempt <= retries; attempt++ {
		names := failing()
		if len(names) == 0 {
			break
		}
		t.Logf("measuring again (attempt %d): %v", attempt+1, names)
		for _, m := range measurements {
			if !slices.Contains(names, m.name) {
				continue
			}
			i := slices.IndexFunc(report.Results, func(r Result) bool { return r.Name == m.name })
			report.Results[i] = best(report.Results[i], m.run())
		}
	}

	for _, r := range report.Results {
		if r.Unit == "bytes" {
			t.Logf("%-24s %-24s budget %s", r.Name, show(r.Unit, r.Value), show(r.Unit, r.Budget))
			continue
		}
		if r.P95 > 0 {
			t.Logf("%-24s p50 %-12s p95 %-12s best p95 %-12s budget %s", r.Name, show(r.Unit, r.P50), show(r.Unit, r.P95), show(r.Unit, r.Best), show(r.Unit, r.Budget))
			continue
		}
		t.Logf("%-24s p50 %-12s p99 %-12s best p99 %-12s budget %s", r.Name, show(r.Unit, r.P50), show(r.Unit, r.P99), show(r.Unit, r.Best), show(r.Unit, r.Budget))
	}
	if err := writeReport(resultsFile, report); err != nil {
		t.Fatal(err)
	}
	for _, msg := range breaches(report) {
		t.Errorf("budget breach: %s", msg)
	}
	if update {
		if err := writeReport(baselineFile, report); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %s", baselineFile)
		return
	}
	if baseErr != nil {
		noBaseline(t, baseErr.Error())
		return
	}
	for _, msg := range regressions(base, report) {
		t.Errorf("regression: %s", msg)
	}
}

// best merges two attempts at one measurement, keeping the lower of each number that a slow machine can only inflate;
// of two attempts with a probe, the one with the lower p99 relative to its probe supplies Best and Probe.
func best(a, b Result) Result {
	a.P50, a.P99, a.Max = min(a.P50, b.P50), min(a.P99, b.P99), min(a.Max, b.Max)
	if a.P95 > 0 {
		a.P95 = min(a.P95, b.P95)
	}
	switch {
	case a.Probe > 0 && b.Probe > 0:
		if float64(b.Best)/float64(b.Probe) < float64(a.Best)/float64(a.Probe) {
			a.Best, a.Probe = b.Best, b.Probe
		}
	default:
		a.Best = min(a.Best, b.Best)
	}
	if a.Unit == "bytes" {
		a.Value = min(a.Value, b.Value)
	}
	return a
}

// noBaseline fails the run loudly: without a baseline for this platform nothing guards against regressions, which
// must not pass silently. ATHENA_BENCH_ALLOW_NO_BASELINE=1 lets a run on a new platform through.
func noBaseline(t *testing.T, why string) {
	t.Helper()
	msg := "NO BASELINE: " + why + "; run make bench-baseline and commit the result"
	if os.Getenv("ATHENA_BENCH_ALLOW_NO_BASELINE") != "" {
		t.Logf("%s (allowed by ATHENA_BENCH_ALLOW_NO_BASELINE)", msg)
		return
	}
	t.Error(msg)
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

func BenchmarkHookDecisionCodex(b *testing.B) { reportLatencies(b, measureHookCodex(b, b.N)) }

func BenchmarkEngineCommand(b *testing.B) { reportLatencies(b, measureEngine(b, b.N)) }

func BenchmarkDispatch(b *testing.B) { reportLatencies(b, measureDispatch(b, b.N, false)) }

// TestHarnessSmoke runs each measurement a few times, so `make check` notices a broken harness without judging
// timings, which race-detector builds distort.
func TestHarnessSmoke(t *testing.T) {
	for name, d := range map[string][]time.Duration{
		"hook":       measureHook(t, 10),
		"hook_codex": measureHookCodex(t, 10),
		"engine":     measureEngine(t, 6),
		"dispatch":   measureDispatch(t, 2, false),
		"remote":     measureRemoteDispatch(t, 2),
		"link":       measureLinkDelta(t, 2),
		"stream":     measureStreamItem(t, 2),
		"command":    measureCommandResult(t, 2),
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
	us, ms := int64(time.Microsecond), int64(time.Millisecond)
	mk := func(hook, engine, dispatch, rss int64) Report {
		return newReport(
			Result{Name: "hook_decision", Unit: "ns", P99: hook, Best: hook, Floor: int64(hookFloor), Budget: 10 * ms},
			Result{Name: "engine_command", Unit: "ns", P99: engine, Best: engine, Floor: int64(engineFloor), Budget: 5 * ms},
			Result{Name: "dispatch", Unit: "ns", P99: dispatch, Best: dispatch, Floor: int64(dispatchFloor), Budget: 100 * ms},
			Result{Name: "server_idle_rss", Unit: "bytes", Value: rss, Floor: rssFloor, Budget: 100 << 20},
		)
	}
	base := mk(360*us, 150*us, 25*ms, 40<<20)
	for _, c := range []struct {
		name string
		cur  Report
		want int // regressions
	}{
		{"same", mk(360*us, 150*us, 25*ms, 40<<20), 0},
		{"faster", mk(200*us, 100*us, 20*ms, 30<<20), 0},
		{"9% slower everywhere", mk(392*us, 163*us, 27*ms, 43<<20), 0},
		{"hook +20%", mk(432*us, 150*us, 25*ms, 40<<20), 1},
		{"hook x2.3", mk(830*us, 150*us, 25*ms, 40<<20), 1},
		{"engine x2.8", mk(360*us, 420*us, 25*ms, 40<<20), 1},
		{"dispatch +24%", mk(360*us, 150*us, 31*ms, 40<<20), 1},
		{"rss +20%", mk(360*us, 150*us, 25*ms, 48<<20), 1},
		{"rss +1 MB is below the floor", mk(360*us, 150*us, 25*ms, 41<<20), 0},
		{"everything doubles", mk(720*us, 300*us, 50*ms, 80<<20), 4},
	} {
		if got := regressions(base, c.cur); len(got) != c.want {
			t.Errorf("%s: regressions = %v, want %d", c.name, got, c.want)
		}
	}
	// A busy machine: dispatch takes twice as long but so does the reference spawn; that is not a regression, while the
	// same slowdown against an unchanged reference is.
	withProbe := func(best, probe int64) Report {
		r := mk(360*us, 150*us, best, 40<<20)
		r.Results[2].Probe = probe
		return r
	}
	pbase := withProbe(25*ms, 10*ms)
	if got := regressions(pbase, withProbe(50*ms, 20*ms)); len(got) != 0 {
		t.Errorf("a uniformly slower machine regressed: %v", got)
	}
	if got := regressions(pbase, withProbe(50*ms, 10*ms)); len(got) != 1 {
		t.Errorf("a dispatch twice as slow on an unchanged machine = %v, want 1", got)
	}
	if got := regressions(pbase, withProbe(31*ms, 10*ms)); len(got) != 1 {
		t.Errorf("a dispatch 24%% slower on an unchanged machine = %v, want 1", got)
	}
	// Doubling a tiny latency is below the floor; a result above the floor is not.
	tiny := newReport(Result{Name: "t", Unit: "ns", Best: 10 * us, P99: 10 * us, Floor: int64(hookFloor)})
	tiny2 := newReport(Result{Name: "t", Unit: "ns", Best: 20 * us, P99: 20 * us, Floor: int64(hookFloor)})
	if got := regressions(tiny, tiny2); len(got) != 0 {
		t.Errorf("a 10 µs growth is below the 20 µs floor: %v", got)
	}
	if got := regressions(base, newReport(base.Results[0])); len(got) != 3 {
		t.Errorf("missing results = %v, want 3", got)
	}
	if !sameHost(base, mk(0, 0, 0, 0)) {
		t.Error("reports of this host differ")
	}
	other := base
	other.GOOS = "plan9"
	if sameHost(other, base) {
		t.Error("reports of two platforms compare")
	}
	if got := breaches(mk(11*ms, 150*us, 25*ms, 101<<20)); len(got) != 2 {
		t.Errorf("breaches = %v, want 2", got)
	}
	// A latency with a p95 is judged on it, not on its p99.
	p95 := newReport(Result{Name: "command_result_p95_ms", Unit: "ns", P95: 200 * ms, P99: 400 * ms, Best: 200 * ms, Budget: 250 * ms})
	if got := breaches(p95); len(got) != 0 {
		t.Errorf("a p95 within budget breached on its p99: %v", got)
	}
	p95.Results[0].P95 = 251 * ms
	if got := breaches(p95); len(got) != 1 {
		t.Errorf("a p95 over budget = %v, want 1 breach", got)
	}
	if got := breaches(mk(10*ms, 5*ms, 100*ms, 100<<20)); len(got) != 0 {
		t.Errorf("at the budget is not a breach: %v", got)
	}
}
