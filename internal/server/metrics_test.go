package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/node"
)

func histogramOf(s metrics.Snapshot, name string, labels map[string]string) (metrics.HistogramValue, bool) {
next:
	for _, h := range s.Histograms {
		if h.Name != name {
			continue
		}
		for k, v := range labels {
			if h.Labels[k] != v {
				continue next
			}
		}
		return h, true
	}
	return metrics.HistogramValue{}, false
}

func counterSum(s metrics.Snapshot, name string, labels map[string]string) float64 {
	var sum float64
next:
	for _, c := range s.Counters {
		if c.Name != name {
			continue
		}
		for k, v := range labels {
			if c.Labels[k] != v {
				continue next
			}
		}
		sum += c.Value
	}
	return sum
}

func TestMetricsCommand(t *testing.T) {
	newFakeEnv(t, claudeFixture(t, "simple-turn.jsonl"))
	dir := newStateDir(t)
	s := startServer(t, dir)

	id := s.submit("Measure it")
	a := s.running(id)
	d := s.detail(id)
	input, _ := json.Marshal(map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": filepath.Join(d.Dir, "a.txt"), "content": "x"}})
	var hr node.HookResponse
	nodeCall(t, dir, node.HookRequest{Op: node.OpHook, Event: node.EventPreToolUse, Ticket: id, Assignment: a.ID, Input: input}, &hr)
	if hr.Verdict != node.VerdictAllow {
		t.Fatalf("hook = %+v", hr)
	}

	var snap metrics.Snapshot
	// The fixture's Result reaches the Ticket's stats through the node: wait for it.
	eventually(t, "usage recorded", func() bool {
		s.ok(Request{Op: OpMetrics, Window: "1h"}, &snap)
		return counterSum(snap, "tokens_total", map[string]string{"kind": "output"}) > 0
	})

	for _, c := range []struct {
		name   string
		labels map[string]string
	}{
		{"engine_command_seconds", map[string]string{"command": "submit"}},
		{"engine_command_seconds", map[string]string{"command": "assignment_started"}},
		{"hook_decision_seconds", map[string]string{"event": "PreToolUse", "verdict": "allow"}},
		{"store_tx_seconds", nil},
		{"dispatch_seconds", nil},
		{"session_spawn_seconds", map[string]string{"runtime": "claude"}},
		{"session_init_seconds", map[string]string{"runtime": "claude"}},
	} {
		h, ok := histogramOf(snap, c.name, c.labels)
		if !ok || h.Count == 0 || h.P50 <= 0 || h.P99 < h.P50 {
			t.Errorf("%s%v = %+v, %v; want a non-empty histogram", c.name, c.labels, h, ok)
		}
	}
	if n := counterSum(snap, "tool_calls_total", map[string]string{"tool": "Write", "error": "false"}); n != 1 {
		t.Errorf("Write tool calls = %v, want 1", n)
	}
	if v := counterSum(snap, "cost_usd_total", map[string]string{"runtime": "claude"}); v <= 0 {
		t.Errorf("cost_usd_total = %v", v)
	}
	gauges := map[string]float64{}
	for _, g := range snap.Gauges {
		gauges[g.Name] = g.Value
	}
	if gauges["slots_in_use"] != 1 || gauges["goroutines"] <= 0 || gauges["process_rss_bytes"] <= 0 {
		t.Errorf("gauges = %v", gauges)
	}

	// The same numbers are on the Ticket.
	eventually(t, "stats on the ticket", func() bool { return s.detail(id).Stats.Turns > 0 })
	st := s.detail(id).Stats
	if st.ToolCalls != 1 || st.OutputTokens == 0 || st.CostUSD <= 0 || st.TimeToFirstOutput <= 0 || st.QueueWait < 0 {
		t.Errorf("ticket stats = %+v", st)
	}

	if r, err := call(dir, Request{Op: OpMetrics, Window: "7d"}); err != nil || r.OK {
		t.Errorf("window 7d = %+v, %v; want a refusal", r, err)
	}
}
