package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"time"
)

// Report is one run of the budget measurements, as written to results.json and kept in baseline.json.
type Report struct {
	Go      string   `json:"go"`
	GOOS    string   `json:"goos"`
	GOARCH  string   `json:"goarch"`
	Results []Result `json:"results"`
}

// Result is one measured quantity: a latency in nanoseconds or a size in bytes (its Value).
//
// For a latency, P99 is the median over the rounds of each round's p99, which the budget is judged on; Best is the
// lowest round p99, which regressions are judged on: noise only ever slows a round, so the best of several rounds moves
// little from run to run, and a real slowdown raises every round. Probe, for latencies that depend on what the machine
// does at the time (starting processes), is the cost of a reference operation of that kind measured alongside, and
// regressions are judged on Best scaled to the probe of the baseline, so a busy machine is not a regression. Floor is the smallest growth, in the unit of the
// result, that counts as a regression however large it is in percent.
type Result struct {
	Name    string `json:"name"`
	Unit    string `json:"unit"`
	Samples int    `json:"samples,omitempty"`
	P50     int64  `json:"p50,omitempty"`
	P99     int64  `json:"p99,omitempty"`
	Best    int64  `json:"best,omitempty"`
	Probe   int64  `json:"probe,omitempty"`
	Max     int64  `json:"max,omitempty"`
	Value   int64  `json:"value,omitempty"`
	Floor   int64  `json:"floor"`
	Budget  int64  `json:"budget"`
}

// budgeted is the number budgets are judged on.
func (r Result) budgeted() int64 {
	if r.Unit == "bytes" {
		return r.Value
	}
	return r.P99
}

// comparedTo is the number regressions are judged on, as it would be under the conditions the baseline b was
// measured in.
func (r Result) comparedTo(b Result) int64 {
	if r.Unit == "bytes" {
		return r.Value
	}
	if r.Probe > 0 && b.Probe > 0 {
		return int64(float64(r.Best) * float64(b.Probe) / float64(r.Probe))
	}
	return r.Best
}

// maxRegression is the growth over the baseline that fails a run (spec 10.3).
const maxRegression = 0.10

func newReport(results ...Result) Report {
	return Report{Go: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Results: results}
}

// sameHost reports whether two reports come from the same platform, the only case in which their numbers compare.
func sameHost(a, b Report) bool { return a.GOOS == b.GOOS && a.GOARCH == b.GOARCH }

// breaches lists the results over their budget.
func breaches(r Report) []string {
	var out []string
	for _, res := range r.Results {
		if res.Budget > 0 && res.budgeted() > res.Budget {
			out = append(out, fmt.Sprintf("%s: %s exceeds its budget %s", res.Name, show(res.Unit, res.budgeted()), show(res.Unit, res.Budget)))
		}
	}
	return out
}

// regressions lists the results more than maxRegression above the baseline and more than their Floor above it, and
// baseline results the run lacks.
func regressions(base, cur Report) []string {
	var out []string
	for _, b := range base.Results {
		i := slices.IndexFunc(cur.Results, func(r Result) bool { return r.Name == b.Name })
		if i < 0 {
			out = append(out, fmt.Sprintf("%s: in the baseline but not measured", b.Name))
			continue
		}
		c := cur.Results[i]
		now := c.comparedTo(b)
		base := b.comparedTo(b)
		if grew := now - base; float64(now) > float64(base)*(1+maxRegression) && grew > c.Floor {
			out = append(out, fmt.Sprintf("%s: %s is %.0f%% above the baseline %s", c.Name, show(c.Unit, now),
				100*float64(grew)/float64(base), show(b.Unit, base)))
		}
	}
	return out
}

func show(unit string, v int64) string {
	if unit == "bytes" {
		return fmt.Sprintf("%.1f MB", float64(v)/(1<<20))
	}
	return time.Duration(v).String()
}

func writeReport(path string, r Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func readReport(path string) (Report, error) {
	var r Report
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}
