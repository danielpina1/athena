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

// Result is one measured quantity: a latency in nanoseconds (its p99 counts) or a size in bytes (its Value counts).
type Result struct {
	Name    string `json:"name"`
	Unit    string `json:"unit"`
	Samples int    `json:"samples,omitempty"`
	P50     int64  `json:"p50,omitempty"`
	P99     int64  `json:"p99,omitempty"`
	Max     int64  `json:"max,omitempty"`
	Value   int64  `json:"value,omitempty"`
	Budget  int64  `json:"budget"`
}

// value is the number budgets and regressions are judged on.
func (r Result) value() int64 {
	if r.Unit == "bytes" {
		return r.Value
	}
	return r.P99
}

const (
	// maxRegression is the growth over the baseline that fails a run.
	maxRegression = 0.10
	// latencyNoise is the share of a result's budget that its p99 may grow by without counting as a regression however
	// large that is in percent: the p99 of a 100 µs operation varies by more than 10% from run to run, and the
	// dispatch p99 depends on how long git takes, while the budgets guard the absolute numbers.
	latencyNoise = 0.05
	// sizeNoise is the same for sizes in bytes.
	sizeNoise = int64(2 << 20)
)

func newReport(results ...Result) Report {
	return Report{Go: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Results: results}
}

// breaches lists the results over their budget.
func breaches(r Report) []string {
	var out []string
	for _, res := range r.Results {
		if res.Budget > 0 && res.value() > res.Budget {
			out = append(out, fmt.Sprintf("%s: %s exceeds its budget %s", res.Name, show(res.Unit, res.value()), show(res.Unit, res.Budget)))
		}
	}
	return out
}

// regressions lists the results more than maxRegression (and the noise floor) above the baseline, and baseline results
// the run lacks. A baseline from another platform says nothing about this one and is not compared.
func regressions(base, cur Report) []string {
	if base.GOOS != cur.GOOS || base.GOARCH != cur.GOARCH {
		return nil
	}
	var out []string
	for _, b := range base.Results {
		i := slices.IndexFunc(cur.Results, func(r Result) bool { return r.Name == b.Name })
		if i < 0 {
			out = append(out, fmt.Sprintf("%s: in the baseline but not measured", b.Name))
			continue
		}
		c := cur.Results[i]
		floor := int64(latencyNoise * float64(c.Budget))
		if c.Unit == "bytes" {
			floor = sizeNoise
		}
		grew := c.value() - b.value()
		if float64(c.value()) > float64(b.value())*(1+maxRegression) && grew > floor {
			out = append(out, fmt.Sprintf("%s: %s is %.0f%% above the baseline %s", c.Name, show(c.Unit, c.value()),
				100*float64(grew)/float64(b.value()), show(b.Unit, b.value())))
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
