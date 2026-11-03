package store

import (
	"errors"
	"testing"
)

func TestSettingsRevisions(t *testing.T) {
	s, _ := openTemp(t)
	if _, ok, err := s.CurrentSettings("policy"); err != nil || ok {
		t.Fatalf("empty block: ok %v err %v", ok, err)
	}
	if rev, err := s.SaveSettings("policy", 0, `{"a":1}`); err != nil || rev != 1 {
		t.Fatalf("first = %d %v", rev, err)
	}
	if rev, err := s.SaveSettings("policy", 1, `{"a":2}`); err != nil || rev != 2 {
		t.Fatalf("second = %d %v", rev, err)
	}
	if _, err := s.SaveSettings("policy", 1, `{"a":3}`); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale = %v", err)
	}
	// Blocks are independent.
	if rev, err := s.SaveSettings("other", 0, `{}`); err != nil || rev != 1 {
		t.Fatalf("other block = %d %v", rev, err)
	}
	cur, ok, err := s.CurrentSettings("policy")
	if err != nil || !ok || cur.Revision != 2 || cur.Doc != `{"a":2}` {
		t.Fatalf("current = %+v %v %v", cur, ok, err)
	}
	h, err := s.SettingsHistory("policy", 0)
	if err != nil || len(h) != 2 || h[0].Revision != 2 || h[1].Revision != 1 {
		t.Fatalf("history = %+v %v", h, err)
	}
	if h, _ = s.SettingsHistory("policy", 1); len(h) != 1 || h[0].Revision != 2 {
		t.Fatalf("limit = %+v", h)
	}
}
