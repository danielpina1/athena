package quota

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

var base = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// clock is a settable clock for a Tracker.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func at(t time.Time) *time.Time          { return &t }
func near(a, b float64) bool             { return math.Abs(a-b) < 1e-9 }
func newTracker(t *testing.T, path string, c *clock) *Tracker {
	t.Helper()
	tr, err := New(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// Observe keeps the latest reading of each window, its Used kept in [0, 1] and its reset in UTC, per runtime and per
// bucket (Codex's limitId): two buckets' "5h" are two windows. Every change is written to quota.json, 0600.
func TestObserveNormalizesAndPersists(t *testing.T) {
	c := &clock{t: base}
	path := filepath.Join(t.TempDir(), "quota.json")
	tr := newTracker(t, path, c)
	changes := 0
	tr.OnChange(func() { changes++ })

	if s := tr.State("codex"); s.Measured || tr.Headroom("codex") != 1 {
		t.Fatalf("before any reading: %+v, headroom %v", s, tr.Headroom("codex"))
	}
	local := base.Add(3 * time.Hour).In(time.FixedZone("x", 3600))
	tr.Observe("codex", []rt.Window{
		{Bucket: "codex", Name: "5h", Used: 1.7, ResetsAt: &local},
		{Bucket: "codex", Name: "7d", Used: -0.2},
		{Bucket: "codex_other", Name: "5h", Used: 0.3},
		{Name: "", Used: 0.9}, // no name: not a window
	})
	c.advance(time.Minute)
	// A later reading replaces its own window only.
	tr.Observe("codex", []rt.Window{{Bucket: "codex", Name: "7d", Used: 0.25, ResetsAt: at(base.Add(48 * time.Hour))}})
	tr.Observe("claude", []rt.Window{{Name: "5h", Used: 0.5}})

	s := tr.State("codex")
	want := []Window{
		{Bucket: "codex", Name: "5h", Used: 1, ResetsAt: at(base.Add(3 * time.Hour))},
		{Bucket: "codex", Name: "7d", Used: 0.25, ResetsAt: at(base.Add(48 * time.Hour))},
		{Bucket: "codex_other", Name: "5h", Used: 0.3},
	}
	if !reflect.DeepEqual(s.Windows, want) {
		t.Fatalf("windows = %+v\nwant %+v", s.Windows, want)
	}
	if s.Windows[0].ResetsAt.Location() != time.UTC {
		t.Fatalf("reset not in UTC: %v", s.Windows[0].ResetsAt)
	}
	if !s.Measured || !s.ReadAt.Equal(base.Add(time.Minute)) || s.LimitedUntil != nil {
		t.Fatalf("state = %+v", s)
	}
	if h := tr.Headroom("codex"); !near(h, 0) {
		t.Fatalf("headroom = %v, want 0 (a window used up)", h)
	}
	if h := tr.Headroom("claude"); !near(h, 0.5) {
		t.Fatalf("claude headroom = %v, want 0.5", h)
	}
	if changes != 3 {
		t.Fatalf("%d changes, want 3", changes)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("quota.json mode %v, want 0600", fi.Mode().Perm())
	}
	var f struct {
		Runtimes map[string]struct {
			Windows []struct {
				Bucket string    `json:"bucket"`
				Name   string    `json:"name"`
				Used   float64   `json:"used"`
				ReadAt time.Time `json:"read_at"`
			} `json:"windows"`
		} `json:"runtimes"`
	}
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("quota.json: %v\n%s", err, b)
	}
	cw := f.Runtimes["codex"].Windows
	if len(cw) != 3 || cw[1].Name != "7d" || !cw[1].ReadAt.Equal(base.Add(time.Minute)) || !cw[0].ReadAt.Equal(base) {
		t.Fatalf("persisted codex windows = %+v", cw)
	}
	if len(f.Runtimes["claude"].Windows) != 1 {
		t.Fatalf("persisted claude = %+v", f.Runtimes["claude"])
	}
	// No temporary file is left beside it.
	if es, _ := os.ReadDir(filepath.Dir(path)); len(es) != 1 {
		t.Fatalf("state dir holds %d entries, want quota.json alone", len(es))
	}
}

// A window whose reset has passed is stale: it no longer counts against the headroom.
func TestStaleWindowIgnored(t *testing.T) {
	c := &clock{t: base}
	tr := newTracker(t, "", c)
	tr.Observe("claude", []rt.Window{
		{Name: "5h", Used: 0.95, ResetsAt: at(base.Add(time.Hour))},
		{Name: "7d", Used: 0.4, ResetsAt: at(base.Add(72 * time.Hour))},
	})
	if h := tr.Headroom("claude"); !near(h, 0.05) {
		t.Fatalf("headroom = %v, want 0.05", h)
	}
	c.advance(time.Hour)
	s := tr.State("claude")
	if !s.Windows[0].Stale || s.Windows[1].Stale {
		t.Fatalf("windows = %+v, want 5h stale and 7d not", s.Windows)
	}
	if h := tr.Headroom("claude"); !near(h, 0.6) {
		t.Fatalf("headroom = %v, want 0.6 (the 5h window reset)", h)
	}
	if h := s.Headroom(base.Add(100 * time.Hour)); !near(h, 1) {
		t.Fatalf("headroom with every window stale = %v, want 1", h)
	}

	// A limited window sets LimitedUntil to its reset; once that passes the runtime is no longer limited.
	tr.Observe("codex", []rt.Window{{Bucket: "codex", Name: "5h", Used: 1, Limited: true, ResetsAt: at(c.now().Add(30 * time.Minute))}})
	s = tr.State("codex")
	if !s.Limited(c.now()) || s.LimitedUntil == nil || !s.LimitedUntil.Equal(c.now().Add(30*time.Minute)) {
		t.Fatalf("limited window: %+v", s)
	}
	c.advance(30 * time.Minute)
	if s := tr.State("codex"); s.Limited(c.now()) || s.LimitedUntil != nil {
		t.Fatalf("after the reset: %+v", s)
	}
}

// A Hit with no reset backs off 15 min, doubling per consecutive hit up to 2 h; a good turn starts over.
func TestHitBackoffDoublesAndCaps(t *testing.T) {
	c := &clock{t: base}
	tr := newTracker(t, "", c)
	for i, want := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour, 2 * time.Hour} {
		tr.Hit("codex", rt.LimitHit{Runtime: "codex"})
		s := tr.State("codex")
		if s.LimitedUntil == nil || !s.LimitedUntil.Equal(c.now().Add(want)) {
			t.Fatalf("hit %d: LimitedUntil = %v, want now + %v", i+1, s.LimitedUntil, want)
		}
		c.advance(want)
	}
	tr.TurnOK("codex")
	tr.Hit("codex", rt.LimitHit{Runtime: "codex"})
	if s := tr.State("codex"); !s.LimitedUntil.Equal(c.now().Add(15 * time.Minute)) {
		t.Fatalf("after a good turn: LimitedUntil = %v, want now + 15m", s.LimitedUntil)
	}
	// A known reset is the wait itself.
	c.advance(15 * time.Minute)
	reset := c.now().Add(3 * time.Hour)
	tr.Hit("codex", rt.LimitHit{Runtime: "codex", ResetsAt: &reset})
	if s := tr.State("codex"); !s.LimitedUntil.Equal(reset) || !s.Limited(c.now()) {
		t.Fatalf("known reset: %+v", s)
	}
}

// A reset that has already passed is no reset: the Hit backs off, so the launch never relaunches at once in a loop.
func TestPastResetBacksOff(t *testing.T) {
	c := &clock{t: base}
	tr := newTracker(t, "", c)
	past := base.Add(-time.Minute)
	tr.Hit("claude", rt.LimitHit{Runtime: "claude", ResetsAt: &past})
	s := tr.State("claude")
	if !s.Limited(c.now()) || !s.LimitedUntil.Equal(base.Add(15*time.Minute)) {
		t.Fatalf("past reset: %+v, want limited until now + 15m", s)
	}
	now := base
	tr.Hit("claude", rt.LimitHit{Runtime: "claude", ResetsAt: &now})
	if s := tr.State("claude"); !s.LimitedUntil.Equal(base.Add(30 * time.Minute)) {
		t.Fatalf("reset at now: LimitedUntil = %v, want now + 30m (the second hit)", s.LimitedUntil)
	}
}

// A transient limit (a rate of requests) backs off 1, 2, 4 … min, up to 30 min.
func TestTransientBackoff(t *testing.T) {
	c := &clock{t: base}
	tr := newTracker(t, "", c)
	for i, want := range []time.Duration{1, 2, 4, 8, 16, 30, 30} {
		reset := c.now().Add(time.Hour) // a transient limit's reset is not the wait
		tr.Hit("codex", rt.LimitHit{Runtime: "codex", Transient: true, ResetsAt: &reset})
		if s := tr.State("codex"); !s.LimitedUntil.Equal(c.now().Add(want * time.Minute)) {
			t.Fatalf("transient hit %d: LimitedUntil = %v, want now + %v min", i+1, s.LimitedUntil, want)
		}
		c.advance(want * time.Minute)
	}
	tr.TurnOK("codex")
	tr.Hit("codex", rt.LimitHit{Runtime: "codex", Transient: true})
	if s := tr.State("codex"); !s.LimitedUntil.Equal(c.now().Add(time.Minute)) {
		t.Fatalf("after a good turn: %v", s.LimitedUntil)
	}
}

// A new Tracker on the same file has what the last one knew: readings, the limit and the backoff count.
func TestReloadAfterRestart(t *testing.T) {
	c := &clock{t: base}
	path := filepath.Join(t.TempDir(), "quota.json")
	tr := newTracker(t, path, c)
	tr.Observe("codex", []rt.Window{{Bucket: "codex", Name: "5h", Used: 0.42, ResetsAt: at(base.Add(4 * time.Hour))}})
	tr.Hit("codex", rt.LimitHit{Runtime: "codex"})
	before := tr.State("codex")

	again := newTracker(t, path, c)
	after := again.State("codex")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("reloaded state = %+v\nwant %+v", after, before)
	}
	// The consecutive count survived: the next hit doubles.
	c.advance(15 * time.Minute)
	again.Hit("codex", rt.LimitHit{Runtime: "codex"})
	if s := again.State("codex"); !s.LimitedUntil.Equal(c.now().Add(30 * time.Minute)) {
		t.Fatalf("hit after reload: %v, want now + 30m", s.LimitedUntil)
	}
	// A file that cannot be read as quota is an error, with a Tracker that starts empty and replaces it.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad, err := New(path, c.now)
	if err == nil || bad == nil || bad.State("codex").Measured {
		t.Fatalf("corrupt file: %v, %+v", err, bad)
	}
}
