// Package quota tracks each runtime's usage limits as its sessions report them: the latest reading of every window,
// and when a limit stopped work, until when the runtime is limited. The node's runtime chooser reads it (State), and it
// survives a restart in <stateDir>/quota.json. It keeps no history.
package quota

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

// The backoffs of a limit that names no reset (or one already passed): 15 min doubling to 2 h for a used-up limit,
// 1 min doubling to 30 min for a transient one (a rate of requests).
const (
	unknownBase   = 15 * time.Minute
	unknownMax    = 2 * time.Hour
	transientBase = time.Minute
	transientMax  = 30 * time.Minute
)

// Window is the latest reading of one usage-limit window.
type Window struct {
	// Bucket is the limit the window belongs to (Codex's limitId; empty for Claude Code); Name its length ("5h", "7d",
	// "<n>m", or "primary"/"secondary" when the runtime did not say). A window is keyed by both: two buckets' "5h" are
	// two windows.
	Bucket, Name string
	// Used is the fraction of the window used, in [0, 1]; ResetsAt when it resets, nil when not known.
	Used     float64
	ResetsAt *time.Time
	// Limited: the runtime said it refuses work until the window resets.
	Limited bool
	// Stale: the window's reset has passed since it was read, so its Used no longer holds.
	Stale bool
}

// State is what the Tracker knows of one runtime.
type State struct {
	// Windows are the latest readings, sorted by bucket and name.
	Windows []Window
	// LimitedUntil is when the runtime may work again; nil when it is not limited.
	LimitedUntil *time.Time
	// ReadAt is when the latest reading came; Measured is false until a first one did.
	ReadAt   time.Time
	Measured bool
}

// Headroom is the share of the runtime's quota left at now: 1 − the largest Used of the windows that have not reset by
// then; 1 when nothing is measured.
func (s State) Headroom(now time.Time) float64 {
	used := 0.0
	for _, w := range s.Windows {
		if w.Stale || (w.ResetsAt != nil && !w.ResetsAt.After(now)) {
			continue
		}
		used = max(used, w.Used)
	}
	return 1 - used
}

// Limited reports whether the runtime refuses work at now.
func (s State) Limited(now time.Time) bool {
	return s.LimitedUntil != nil && now.Before(*s.LimitedUntil)
}

// Tracker is the quota of every runtime of a node. It is safe for concurrent use.
type Tracker struct {
	path string
	now  func() time.Time

	mu   sync.Mutex
	rts  map[string]*runtimeState
	subs []func()
}

// runtimeState is one runtime's part of quota.json.
type runtimeState struct {
	Windows      []fileWindow `json:"windows,omitempty"`
	LimitedUntil *time.Time   `json:"limited_until,omitempty"`
	// LimitedBy is the window whose reading set LimitedUntil; nil when a turn's limit (Hit) did, or none.
	LimitedBy *windowKey `json:"limited_by,omitempty"`
	// Hits and TransientHits count the consecutive limits that named no reset, since the last good turn.
	Hits          int `json:"hits,omitempty"`
	TransientHits int `json:"transient_hits,omitempty"`
}

// windowKey names a window of a runtime: its bucket and name.
type windowKey struct {
	Bucket string `json:"bucket,omitempty"`
	Name   string `json:"name"`
}

type fileWindow struct {
	Bucket   string     `json:"bucket,omitempty"`
	Name     string     `json:"name"`
	Used     float64    `json:"used"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	Limited  bool       `json:"limited,omitempty"`
	ReadAt   time.Time  `json:"read_at"`
}

type file struct {
	Runtimes map[string]*runtimeState `json:"runtimes"`
}

// New returns a Tracker that keeps its state in path (written 0600, atomically, on every change; "" keeps it in
// memory), loaded from it when it exists. now is the clock (nil: time.Now). A file that cannot be read as quota is an
// error, returned with a Tracker that starts empty and replaces the file at its first change.
func New(path string, now func() time.Time) (*Tracker, error) {
	if now == nil {
		now = time.Now
	}
	t := &Tracker{path: path, now: now, rts: map[string]*runtimeState{}}
	if path == "" {
		return t, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return t, fmt.Errorf("quota: read %s: %w", path, err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return t, fmt.Errorf("quota: %s is not a quota file: %w", path, err)
	}
	for name, rs := range f.Runtimes {
		if rs != nil {
			t.rts[name] = rs
		}
	}
	return t, nil
}

// OnChange adds f to what is called after each change, outside the Tracker's lock. f must not block.
func (t *Tracker) OnChange(f func()) {
	t.mu.Lock()
	t.subs = append(t.subs, f)
	t.mu.Unlock()
}

// Observe records a runtime's reading of its windows. Each window replaces the earlier reading of its own bucket and
// name only (Claude Code reports some of its windows at a time); a window without a name is ignored. Used is kept in
// [0, 1]. A Limited window limits the runtime until its reset; one with no reset ahead limits it for the first unknown
// backoff (15 min) unless it is already limited, without counting as a hit. The window whose reading set the limit
// lifts it once it reads not limited with Used below 1 (a plan upgrade, overage, an early reset), unless another
// window is still read as limited; a limit a turn hit (Hit) is never lifted by a reading.
func (t *Tracker) Observe(runtime string, ws []rt.Window) {
	t.change(func(now time.Time) bool {
		rs := t.state(runtime)
		changed := false
		for _, w := range ws {
			if w.Name == "" {
				continue
			}
			fw := fileWindow{Bucket: w.Bucket, Name: w.Name, Used: clamp(w.Used), Limited: w.Limited, ReadAt: now}
			if w.ResetsAt != nil {
				r := w.ResetsAt.UTC()
				fw.ResetsAt = &r
			}
			i := slices.IndexFunc(rs.Windows, func(x fileWindow) bool { return x.Bucket == w.Bucket && x.Name == w.Name })
			if i < 0 {
				rs.Windows = append(rs.Windows, fw)
			} else {
				rs.Windows[i] = fw
			}
			changed = true
			key := windowKey{Bucket: w.Bucket, Name: w.Name}
			switch {
			case w.Limited:
				until := now.Add(unknownBase)
				if fw.ResetsAt != nil && fw.ResetsAt.After(now) {
					until = *fw.ResetsAt
				} else if limitedAt(rs, now) {
					continue
				}
				if extend(rs, until) {
					rs.LimitedBy = &key
				}
			case fw.Used < 1 && rs.LimitedBy != nil && *rs.LimitedBy == key:
				// The window that limited the runtime no longer does: lift its limit, less any other window's.
				rs.LimitedUntil, rs.LimitedBy = nil, nil
				relimit(rs, now)
			}
		}
		slices.SortFunc(rs.Windows, func(a, b fileWindow) int {
			return cmp.Or(cmp.Compare(a.Bucket, b.Bucket), cmp.Compare(a.Name, b.Name))
		})
		return changed
	})
}

// Hit records a limit that stopped a turn. A transient one backs off 1, 2, 4 … min up to 30 min, whatever its reset
// says; another waits for its reset, or, when it names none or one that has already passed, backs off 15 min doubling
// per consecutive hit up to 2 h. A runtime already limited for longer stays so.
func (t *Tracker) Hit(runtime string, h rt.LimitHit) {
	t.change(func(now time.Time) bool {
		rs := t.state(runtime)
		var until time.Time
		switch {
		case h.Transient:
			rs.TransientHits++
			until = now.Add(backoff(transientBase, transientMax, rs.TransientHits))
		case h.ResetsAt != nil && h.ResetsAt.After(now):
			until = h.ResetsAt.UTC()
		default:
			rs.Hits++
			until = now.Add(backoff(unknownBase, unknownMax, rs.Hits))
		}
		if extend(rs, until) {
			rs.LimitedBy = nil
		}
		return true
	})
}

// TurnOK records a turn of the runtime that ended well: the next limit backs off from the start again.
func (t *Tracker) TurnOK(runtime string) {
	t.change(func(time.Time) bool {
		rs := t.rts[runtime]
		if rs == nil || (rs.Hits == 0 && rs.TransientHits == 0) {
			return false
		}
		rs.Hits, rs.TransientHits = 0, 0
		return true
	})
}

// State returns what the Tracker knows of a runtime now.
func (t *Tracker) State(runtime string) State {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var s State
	rs := t.rts[runtime]
	if rs == nil {
		return s
	}
	for _, w := range rs.Windows {
		var reset *time.Time
		if w.ResetsAt != nil {
			r := *w.ResetsAt
			reset = &r
		}
		s.Windows = append(s.Windows, Window{Bucket: w.Bucket, Name: w.Name, Used: w.Used, ResetsAt: reset, Limited: w.Limited,
			Stale: reset != nil && !reset.After(now)})
		if w.ReadAt.After(s.ReadAt) {
			s.ReadAt = w.ReadAt
		}
	}
	s.Measured = len(s.Windows) > 0
	if limitedAt(rs, now) {
		u := *rs.LimitedUntil
		s.LimitedUntil = &u
	}
	return s
}

// Headroom is the runtime's State(runtime).Headroom now.
func (t *Tracker) Headroom(runtime string) float64 { return t.State(runtime).Headroom(t.now()) }

// change runs f under the lock; when it reports a change, the state is written and the subscribers called.
func (t *Tracker) change(f func(now time.Time) bool) {
	t.mu.Lock()
	if !f(t.now()) {
		t.mu.Unlock()
		return
	}
	err := t.save()
	subs := slices.Clone(t.subs)
	t.mu.Unlock()
	if err != nil {
		slog.Default().Warn("quota: save", "path", t.path, "error", err)
	}
	for _, s := range subs {
		s()
	}
}

func (t *Tracker) state(runtime string) *runtimeState {
	rs := t.rts[runtime]
	if rs == nil {
		rs = &runtimeState{}
		t.rts[runtime] = rs
	}
	return rs
}

// save writes the state to path, whole: a temporary file in the same directory, renamed over it. Caller holds mu.
func (t *Tracker) save() error {
	if t.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(file{Runtimes: t.rts}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".quota-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}

// limitedAt reports whether rs is limited at now.
func limitedAt(rs *runtimeState, now time.Time) bool {
	return rs.LimitedUntil != nil && now.Before(*rs.LimitedUntil)
}

// extend limits rs until until, unless it is already limited for longer; it reports whether it did.
func extend(rs *runtimeState, until time.Time) bool {
	until = until.UTC()
	if rs.LimitedUntil == nil || until.After(*rs.LimitedUntil) {
		rs.LimitedUntil = &until
		return true
	}
	return false
}

// relimit limits rs again by the windows still read as limited: until a reset ahead, or the first unknown backoff
// from its reading for one with none.
func relimit(rs *runtimeState, now time.Time) {
	for _, w := range rs.Windows {
		if !w.Limited {
			continue
		}
		until := w.ReadAt.Add(unknownBase)
		if w.ResetsAt != nil && w.ResetsAt.After(now) {
			until = *w.ResetsAt
		}
		if until.After(now) && extend(rs, until) {
			rs.LimitedBy = &windowKey{Bucket: w.Bucket, Name: w.Name}
		}
	}
}

// backoff is base doubled for each hit after the first, up to most.
func backoff(base, most time.Duration, hits int) time.Duration {
	d := base
	for i := 1; i < hits && d < most; i++ {
		d *= 2
	}
	return min(d, most)
}

func clamp(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return min(max(f, 0), 1)
}
