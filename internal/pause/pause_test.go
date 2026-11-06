package pause

import (
	"sync"
	"testing"
	"time"
)

type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKV) KVGet(key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok, nil
}

func (k *memKV) KVSet(key, v string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = v
	return nil
}

func TestSwitchPersistsAndIsIdempotent(t *testing.T) {
	kv := &memKV{m: map[string]string{}}
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	now := at
	clock := func() time.Time { return now }
	s, err := Open(kv, clock)
	if err != nil {
		t.Fatal(err)
	}
	if s.Paused() {
		t.Fatal("fresh switch is paused")
	}
	var changes []State
	s.OnChange(func(st State) { changes = append(changes, st) })

	st, err := s.Pause("cli")
	if err != nil || !st.Paused || !st.Since.Equal(at) || st.By != "cli" || !s.Paused() {
		t.Fatalf("pause: %+v %v", st, err)
	}
	s2, err := Open(kv, clock)
	if err != nil || !s2.Paused() || !s2.State().Since.Equal(at) || s2.State().By != "cli" {
		t.Fatalf("reopened: %+v %v", s2.State(), err)
	}
	now = at.Add(time.Hour)
	if st, _ := s.Pause("web"); !st.Since.Equal(at) || st.By != "cli" {
		t.Fatalf("pausing again changed it: %+v", st)
	}
	if len(changes) != 1 {
		t.Fatalf("OnChange after pause twice = %d, want 1", len(changes))
	}
	if st, err := s.Resume("web"); err != nil || st.Paused || s.Paused() {
		t.Fatalf("resume: %+v %v", st, err)
	}
	if _, err := s.Resume("web"); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[1].Paused {
		t.Fatalf("OnChange = %+v, want pause then resume once each", changes)
	}
	if s3, _ := Open(kv, clock); s3.Paused() {
		t.Fatal("reopened after resume: still paused")
	}
}

// Callbacks run in the order of the changes even when pause and resume race: the last one sees the final state.
func TestOnChangeOrderedUnderRace(t *testing.T) {
	kv := &memKV{m: map[string]string{}}
	s, err := Open(kv, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var last State
	s.OnChange(func(st State) {
		time.Sleep(time.Millisecond)
		mu.Lock()
		last = st
		mu.Unlock()
	})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.Pause("cli") }()
		go func() { defer wg.Done(); s.Resume("cli") }()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if last.Paused != s.Paused() {
		t.Fatalf("last callback saw paused=%v, final state paused=%v", last.Paused, s.Paused())
	}
}
