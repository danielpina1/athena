package schedule

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/sources"
	"github.com/danielpina1/athena/internal/store"
)

var (
	_ sources.Source = (*Source)(nil)
	_ Intake         = (*core.Engine)(nil)
	_ KV             = (*store.Store)(nil)
)

type fakeIntake struct {
	got  []core.Trigger
	seen map[string]bool
	busy bool
}

func (f *fakeIntake) Receive(t core.Trigger) (string, error) {
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[t.ID] {
		return core.ReceiveDuplicate, nil
	}
	f.seen[t.ID] = true
	f.got = append(f.got, t)
	return core.ReceiveStored, nil
}

func (f *fakeIntake) ThreadBusy(string) (bool, error) { return f.busy, nil }

type mapKV struct {
	m       map[string]string
	failSet bool
}

func (k *mapKV) KVGet(key string) (string, bool, error) { v, ok := k.m[key]; return v, ok, nil }
func (k *mapKV) KVSet(key, value string) error {
	if k.failSet {
		return errors.New("disk full")
	}
	k.m[key] = value
	return nil
}

type rig struct {
	t      *testing.T
	now    time.Time
	in     *fakeIntake
	kv     *mapKV
	entry  Entry
	paused bool
	known  bool
	src    *Source
}

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t
}

func newRig(t *testing.T, cron, now string) *rig {
	r := &rig{t: t, now: at(now), in: &fakeIntake{}, kv: &mapKV{m: map[string]string{}}, known: true,
		entry: Entry{Slug: "night", Name: "Night", Instructions: "Tidy the docs.", Repo: "o/r", Cron: cron}}
	r.src = r.source()
	return r
}

func (r *rig) source() *Source {
	return New(Config{
		Entries: func() []Entry { return []Entry{r.entry} },
		Known:   func(string) bool { return r.known },
		Paused:  func() bool { return r.paused },
		Now:     func() time.Time { return r.now },
	}, r.in, r.kv, nil)
}

func (r *rig) tickAt(now string) {
	r.t.Helper()
	r.now = at(now)
	r.src.Tick()
}

func (r *rig) state() state {
	r.t.Helper()
	st, err := r.src.load(r.entry.Slug)
	if err != nil || st == nil {
		r.t.Fatalf("state: %v, %v", st, err)
	}
	return *st
}

func (r *rig) fires(n int) {
	r.t.Helper()
	if len(r.in.got) != n {
		r.t.Fatalf("triggers = %d, want %d: %+v", len(r.in.got), n, r.in.got)
	}
}

func TestNewEntryStartsNow(t *testing.T) {
	r := newRig(t, "0 3 * * *", "2026-11-01 12:00:00")
	r.src.Tick()
	r.fires(0)
	if c := r.state().Cursor; !c.Equal(r.now) {
		t.Fatalf("cursor = %v, want %v", c, r.now)
	}
}

func TestFiresOnSchedule(t *testing.T) {
	r := newRig(t, "0 3 * * *", "2026-11-01 12:00:00")
	r.src.Tick()
	r.tickAt("2026-11-02 02:59:30")
	r.fires(0)
	r.tickAt("2026-11-02 03:00:10")
	r.fires(1)
	got := r.in.got[0]
	want := core.Trigger{
		ID: "schedule:night:cron:2026-11-02T03:00", Source: "schedule", Kind: "schedule", Scope: "schedule",
		Actor: core.ActorRef{Source: "schedule", ID: "night"}, At: r.now,
		Text:    "Tidy the docs.\n\nWhy now: scheduled for 03:00.",
		Threads: []string{"schedule:night"},
		Meta:    map[string]string{"repo": "o/r", "autopilot": "night"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trigger = %+v\nwant %+v", got, want)
	}
	r.tickAt("2026-11-02 03:00:40")
	r.fires(1)
}

func TestBusyEntryDoesNotFire(t *testing.T) {
	r := newRig(t, "0 3 * * *", "2026-11-01 12:00:00")
	r.src.Tick()
	r.in.busy = true
	r.tickAt("2026-11-02 03:00:10")
	r.fires(0)
	r.in.busy = false
	r.tickAt("2026-11-02 03:00:40")
	r.fires(0)
	r.tickAt("2026-11-03 03:00:10")
	r.fires(1)
}

func TestCatchUpFiresLatestOnce(t *testing.T) {
	r := newRig(t, "0 3 * * *", "2026-11-01 02:00:00")
	r.src.Tick()
	r.tickAt("2026-11-03 08:00:00")
	r.fires(1)
	if r.in.got[0].ID != "schedule:night:cron:2026-11-03T03:00" {
		t.Fatalf("id = %s", r.in.got[0].ID)
	}
	want := "Tidy the docs.\n\nWhy now: catch-up, due 03:00, 5h late; the repository may have moved since."
	if r.in.got[0].Text != want {
		t.Fatalf("text = %q", r.in.got[0].Text)
	}
}

func TestMissedOver24hFiresNothing(t *testing.T) {
	r := newRig(t, "0 3 * * 1", "2026-11-01 02:00:00") // Sundays; the Monday 2026-11-02 firing is 2 days old
	r.src.Tick()
	r.tickAt("2026-11-04 08:00:00")
	r.fires(0)
}

func TestRestartDoesNotRefire(t *testing.T) {
	r := newRig(t, "0 3 * * *", "2026-11-01 12:00:00")
	r.src.Tick()
	r.tickAt("2026-11-02 03:00:10")
	r.fires(1)
	r.src = r.source()
	r.in.seen = nil // even a core with no memory of the Trigger must not see another
	r.tickAt("2026-11-02 03:00:40")
	r.fires(1)
}

func TestCrashBeforeStateSaveIsDuplicateOnly(t *testing.T) {
	r := newRig(t, "0 3 * * *", "2026-11-01 12:00:00")
	r.src.Tick()
	r.kv.failSet = true
	r.tickAt("2026-11-02 03:00:10")
	r.fires(1)
	r.kv.failSet = false
	r.src = r.source()
	r.tickAt("2026-11-02 03:00:40")
	r.fires(1)
	st := r.state()
	if st.LastFired.IsZero() || st.Fires != 0 {
		t.Fatalf("a duplicate is fired but uncounted: %+v", st)
	}
}

func TestEditedCronStartsOver(t *testing.T) {
	r := newRig(t, "0 3 * * *", "2026-11-01 12:00:00")
	r.src.Tick()
	r.entry.Cron = "0 2 * * *"
	r.tickAt("2026-11-02 04:00:00")
	r.fires(0)
	st := r.state()
	if st.Spec != "0 2 * * *" || !st.Cursor.Equal(r.now) {
		t.Fatalf("state = %+v", st)
	}
	r.tickAt("2026-11-03 02:00:10")
	r.fires(1)
}

func TestPausedHoldsAndFiresAfter(t *testing.T) {
	r := newRig(t, "0 3 * * *", "2026-11-01 12:00:00")
	r.src.Tick()
	before := r.state().Cursor
	r.paused = true
	r.tickAt("2026-11-02 03:05:00")
	r.fires(0)
	if c := r.state().Cursor; !c.Equal(before) {
		t.Fatalf("cursor moved to %v", c)
	}
	r.paused = false
	r.tickAt("2026-11-02 03:20:00")
	r.fires(1)
	r.tickAt("2026-11-02 03:20:30")
	r.fires(1)
}

func TestUnknownRepoHoldsAndReports(t *testing.T) {
	r := newRig(t, "0 3 * * *", "2026-11-01 12:00:00")
	r.src.Tick()
	r.known = false
	r.tickAt("2026-11-02 03:00:10")
	r.fires(0)
	if r.src.Health() == nil {
		t.Fatal("Health = nil for an unknown repository")
	}
	r.known = true
	r.tickAt("2026-11-02 03:00:40")
	r.fires(1)
	if err := r.src.Health(); err != nil {
		t.Fatalf("Health = %v", err)
	}
}

func TestDailyCapSurvivesRestart(t *testing.T) {
	r := newRig(t, "* * * * *", "2026-11-01 12:00:00") // faster than MinGap: only the cap is under test
	r.src.Tick()
	st := r.state()
	st.Date, st.Fires = "2026-11-02", DailyCap-1
	if err := r.src.save("night", &st); err != nil {
		t.Fatal(err)
	}
	r.tickAt("2026-11-02 03:00:10")
	r.fires(1)
	r.src = r.source()
	r.tickAt("2026-11-02 03:20:10")
	r.fires(1)
	if got := r.state().Fires; got != DailyCap {
		t.Fatalf("fires = %d", got)
	}
	r.tickAt("2026-11-03 03:20:10")
	r.fires(2)
}

func TestMinGapComparesDueTimes(t *testing.T) {
	r := newRig(t, "*/15 * * * *", "2026-11-02 02:50:00")
	r.src.Tick()
	r.tickAt("2026-11-02 03:00:10")
	r.fires(1)
	r.tickAt("2026-11-02 03:15:05")
	r.fires(2)
}

func TestFallBackFiresOnce(t *testing.T) {
	lisbon, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Skip("no tz database")
	}
	r := newRig(t, "30 1 * * *", "2026-10-24 12:00:00")
	r.src.cfg.Now = func() time.Time { return r.now.In(lisbon) }
	r.src.Tick()
	// 2026-10-25: clocks go back at 02:00 WEST to 01:00 WET, so 01:30 happens twice.
	for _, utc := range []string{"2026-10-25 00:31:00", "2026-10-25 01:31:00", "2026-10-25 02:31:00"} {
		r.tickAt(utc)
	}
	r.fires(1)
	if r.in.got[0].ID != "schedule:night:cron:2026-10-25T01:30" {
		t.Fatalf("id = %s", r.in.got[0].ID)
	}
}

func TestListens(t *testing.T) {
	r := newRig(t, "0 3 * * *", "2026-11-01 12:00:00")
	for thread, want := range map[string]bool{"schedule:night": true, "schedule:other": false, "github:o/r#1": false} {
		if got := r.src.Listens(thread); got != want {
			t.Errorf("Listens(%q) = %v, want %v", thread, got, want)
		}
	}
}
