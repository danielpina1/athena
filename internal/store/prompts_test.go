package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func openPrompts(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSavePromptConflictsOnStaleBase(t *testing.T) {
	st := openPrompts(t)
	var changes []Changes
	st.OnCommit(func(c Changes) { changes = append(changes, c) })
	text := "mine"
	rev, err := st.SavePrompt("worker", &text, 0, "default", "web")
	if err != nil || rev != 1 {
		t.Fatalf("first save = %d, %v", rev, err)
	}
	if len(changes) != 1 || !changes[0].Prompts {
		t.Fatalf("changes = %+v, want Prompts", changes)
	}
	if _, err := st.SavePrompt("worker", &text, 0, "default", "web"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save = %v, want ErrConflict", err)
	}
	// The revision is shared by all parts: a save of another part moves the base for every part.
	if rev, err = st.SavePrompt("router", &text, 1, "d", "web"); err != nil || rev != 2 {
		t.Fatalf("router save = %d, %v", rev, err)
	}
	if _, err := st.SavePrompt("worker", nil, 1, "default", "web"); !errors.Is(err, ErrConflict) {
		t.Fatalf("worker reset on the old base = %v, want ErrConflict", err)
	}
	if len(changes) != 2 {
		t.Fatalf("a refused save reported changes: %d", len(changes))
	}
	over, latest, err := st.PromptOverrides()
	if err != nil || latest != 2 || len(over) != 2 || over["worker"].Text != "mine" {
		t.Fatalf("overrides = %v, latest %d, %v", over, latest, err)
	}
}

func TestResetDropsTheOverrideAndHistoryKeepsIt(t *testing.T) {
	st := openPrompts(t)
	a, b := "one", "two"
	for i, text := range []*string{&a, &b, nil} {
		if _, err := st.SavePrompt("worker", text, uint32(i), "def", "web"); err != nil {
			t.Fatal(err)
		}
	}
	over, latest, err := st.PromptOverrides()
	if err != nil || latest != 3 || len(over) != 0 {
		t.Fatalf("overrides after a reset = %v, latest %d, %v", over, latest, err)
	}
	hist, err := st.PromptHistory("worker", 2)
	if err != nil || len(hist) != 2 || hist[0].Revision != 3 || hist[0].Text != nil || hist[1].Text == nil || *hist[1].Text != "two" {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	if hist[0].DefaultText != "def" || hist[0].At.IsZero() || hist[0].Origin != "web" {
		t.Fatalf("history entry = %+v", hist[0])
	}
}
