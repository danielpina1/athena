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
	if err != nil || s.Paused() {
		t.Fatalf("fresh switch: paused=%v err=%v", s.Paused(), err)
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
