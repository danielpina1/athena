// Package pause is the kill switch: one persisted flag the owner sets with `athena pause` or the dashboard. While it
// is set, the Engine's evaluation, the router and the Outbox stand still; nothing queued is dropped.
package pause

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const key = "pause"

// State is the switch. By says who set it: cli or web.
type State struct {
	Paused bool      `json:"paused"`
	Since  time.Time `json:"since,omitempty"`
	By     string    `json:"by,omitempty"`
}

// KV is the store's plain string storage.
type KV interface {
	KVGet(string) (string, bool, error)
	KVSet(string, string) error
}

// Switch is the persisted kill switch. Its methods are safe for concurrent use.
type Switch struct {
	kv  KV
	now func() time.Time

	paused atomic.Bool

	mu       sync.Mutex // serializes writes and the state
	state    State
	onChange func(State)
}

// Open loads the switch from kv (not paused when nothing was saved).
func Open(kv KV, now func() time.Time) (*Switch, error) {
	if now == nil {
		now = time.Now
	}
	s := &Switch{kv: kv, now: now}
	raw, ok, err := kv.KVGet(key)
	if err != nil {
		return nil, fmt.Errorf("pause: read: %w", err)
	}
	if ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &s.state); err != nil {
			return nil, fmt.Errorf("pause: decode: %w", err)
		}
	}
	s.paused.Store(s.state.Paused)
	return s, nil
}

// State returns the current state.
func (s *Switch) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Paused reports whether the switch is set, without taking a lock.
func (s *Switch) Paused() bool { return s.paused.Load() }

// OnChange sets the function called after each real change is saved, outside the lock. Set it before the switch is
// used by other goroutines.
func (s *Switch) OnChange(f func(State)) {
	s.mu.Lock()
	s.onChange = f
	s.mu.Unlock()
}

// Pause sets the switch. Pausing a paused switch changes nothing (Since and By stay).
func (s *Switch) Pause(by string) (State, error) {
	return s.set(State{Paused: true, Since: s.now().UTC(), By: by})
}

// Resume clears the switch; resuming a switch that is not paused changes nothing.
func (s *Switch) Resume(by string) (State, error) { return s.set(State{}) }

func (s *Switch) set(next State) (State, error) {
	s.mu.Lock()
	if s.state.Paused == next.Paused {
		cur := s.state
		s.mu.Unlock()
		return cur, nil
	}
	raw, err := json.Marshal(next)
	if err == nil {
		err = s.kv.KVSet(key, string(raw))
	}
	if err != nil {
		s.mu.Unlock()
		return s.State(), fmt.Errorf("pause: save: %w", err)
	}
	s.state = next
	s.paused.Store(next.Paused)
	f := s.onChange
	s.mu.Unlock()
	if f != nil {
		f(next)
	}
	return next, nil
}
