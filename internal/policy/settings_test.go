package policy

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/store"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewStore(st, Defaults([]string{"athena-app"}))
}

func TestDefaults(t *testing.T) {
	s := Defaults([]string{"acct"})
	if s.Mode != "observe" || len(s.Rules) != 0 {
		t.Fatalf("defaults = %+v", s)
	}
	p := s.Sources["github"]
	if len(p.Allowed) != 1 || p.Allowed[0] != "acct" {
		t.Fatalf("allowed = %v", p.Allowed)
	}
	for _, scope := range []string{"issues", "pulls", "comments", "ci"} {
		r, ok := p.Admission[scope]
		want := Row{ListedUnaddressed: "inferred", UnlistedAddressed: "ignore", UnlistedUnaddressed: "ignore"}
		if scope == "ci" {
			want = Row{UnlistedUnaddressed: "ignore"}
		}
		if !ok || r != want {
			t.Errorf("row %s = %+v", scope, r)
		}
	}
	if err := Validate(s); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCurrentEmptyReturnsDefaults(t *testing.T) {
	s := openStore(t)
	cur, rev, err := s.Current()
	if err != nil || rev != 0 {
		t.Fatalf("Current = %+v %d %v", cur, rev, err)
	}
	if Version(cur) != Version(Defaults([]string{"athena-app"})) {
		t.Fatalf("not the defaults: %+v", cur)
	}
}

func TestSaveAndHistory(t *testing.T) {
	s := openStore(t)
	cur, _, _ := s.Current()
	cur.Mode = "auto"
	cur.Rules = []Rule{{Name: "deps", Instructions: "Bump.", Enabled: true}}
	rev, err := s.Save(cur, 0, "cli")
	if err != nil || rev != 1 {
		t.Fatalf("first save = %d %v", rev, err)
	}
	got, rev, err := s.Current()
	if err != nil || rev != 1 || got.Mode != "auto" || len(got.Rules) != 1 {
		t.Fatalf("Current = %+v %d %v", got, rev, err)
	}
	got.Mode = "off"
	if rev, err = s.Save(got, 1, "router"); err != nil || rev != 2 {
		t.Fatalf("second save = %d %v", rev, err)
	}
	h, err := s.History(10)
	if err != nil || len(h) != 2 {
		t.Fatalf("history = %+v %v", h, err)
	}
	if h[0].Revision != 2 || h[0].Origin != "router" || h[0].Settings.Mode != "off" || h[1].Revision != 1 || h[1].Origin != "cli" {
		t.Fatalf("history order or content wrong: %+v", h)
	}
	if h[0].At.IsZero() {
		t.Error("history carries no time")
	}
	if h, _ = s.History(1); len(h) != 1 || h[0].Revision != 2 {
		t.Fatalf("limit 1: %+v", h)
	}
}

func TestSaveStampsTimeFromStoreClock(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return at })
	s := NewStore(st, Defaults([]string{"a"}))
	if _, err := s.Save(Defaults([]string{"a"}), 0, "sync"); err != nil {
		t.Fatal(err)
	}
	h, _ := s.History(1)
	if len(h) != 1 || !h[0].At.Equal(at) {
		t.Fatalf("history = %+v", h)
	}
}

func TestSaveStaleRevisionRefused(t *testing.T) {
	s := openStore(t)
	cur, _, _ := s.Current()
	if _, err := s.Save(cur, 0, "cli"); err != nil {
		t.Fatal(err)
	}
	cur.Mode = "auto"
	if _, err := s.Save(cur, 0, "cli"); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale save: %v, want ErrStaleRevision", err)
	}
	if _, err := s.Save(cur, 5, "cli"); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("future revision: %v, want ErrStaleRevision", err)
	}
	if got, rev, _ := s.Current(); rev != 1 || got.Mode != "observe" {
		t.Fatalf("a refused save changed state: %+v %d", got, rev)
	}
}

func TestSaveConcurrentOneWins(t *testing.T) {
	s := openStore(t)
	cur, _, _ := s.Current()
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Save(cur, 0, "cli")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrStaleRevision) {
			t.Fatal(err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d saves from revision 0 succeeded, want 1", ok)
	}
}

func TestSaveValidation(t *testing.T) {
	s := openStore(t)
	base := func() Settings { c, _, _ := s.Current(); return c }
	tests := map[string]func(c *Settings){
		"bad mode":         func(c *Settings) { c.Mode = "sometimes" },
		"empty mode":       func(c *Settings) { c.Mode = "" },
		"empty rule name":  func(c *Settings) { c.Rules = []Rule{{Name: "  ", Enabled: true}} },
		"duplicate rule":   func(c *Settings) { c.Rules = []Rule{{Name: "a"}, {Name: "a"}} },
		"unknown row":      func(c *Settings) { p := c.Sources["github"]; p.Admission["wiki"] = Row{}; c.Sources["github"] = p },
		"listed cell bad":  func(c *Settings) { *c = withCell(*c, "comments", CellListedUnaddressed, "explicit") },
		"unlisted unaddr":  func(c *Settings) { *c = withCell(*c, "comments", CellUnlistedUnaddressed, "explicit") },
		"unlisted addr":    func(c *Settings) { *c = withCell(*c, "comments", CellUnlistedAddressed, "maybe") },
		"ci listed cell":   func(c *Settings) { *c = withCell(*c, "ci", CellListedUnaddressed, "inferred") },
		"ci addressed":     func(c *Settings) { *c = withCell(*c, "ci", CellUnlistedAddressed, "explicit") },
		"empty login":      func(c *Settings) { p := c.Sources["github"]; p.Allowed = []string{"a", " "}; c.Sources["github"] = p },
		"empty bot":        func(c *Settings) { p := c.Sources["github"]; p.Bots = []string{""}; c.Sources["github"] = p },
		"empty source key": func(c *Settings) { c.Sources[""] = SourcePart{} },
	}
	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			c := base()
			f(&c)
			if _, err := s.Save(c, 0, "cli"); err == nil || errors.Is(err, ErrStaleRevision) {
				t.Fatalf("Save = %v, want a validation error", err)
			}
		})
	}
	if _, err := s.Save(base(), 0, "elsewhere"); err == nil {
		t.Error("an unknown origin was accepted")
	}
	if _, rev, _ := s.Current(); rev != 0 {
		t.Fatalf("invalid saves took revision %d", rev)
	}

	// Allowed choices pass; the unlisted-addressed cell accepts explicit; empty cells take the default.
	c := base()
	c = withCell(c, "comments", CellUnlistedAddressed, "explicit")
	c = withCell(c, "comments", CellListedUnaddressed, "ignore")
	c = withCell(c, "ci", CellUnlistedUnaddressed, "inferred")
	p := c.Sources["github"]
	p.Admission["issues"] = Row{} // all defaults
	c.Sources["github"] = p
	if _, err := s.Save(c, 0, "cli"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Current()
	if r := got.Sources["github"].Admission["issues"]; r.ListedUnaddressed != "inferred" || r.UnlistedAddressed != "ignore" {
		t.Errorf("empty cells were not defaulted: %+v", r)
	}
	if r := got.Sources["github"].Admission["comments"]; r.UnlistedAddressed != "explicit" || r.ListedUnaddressed != "ignore" {
		t.Errorf("cells not kept: %+v", r)
	}
}

func TestSaveDoesNotAliasCaller(t *testing.T) {
	s := openStore(t)
	c, _, _ := s.Current()
	if _, err := s.Save(c, 0, "cli"); err != nil {
		t.Fatal(err)
	}
	// Even a very long rule text round-trips.
	c.Rules = []Rule{{Name: "big", Instructions: strings.Repeat("x", 1<<16), Enabled: true}}
	if _, err := s.Save(c, 1, "cli"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Current()
	if len(got.Rules[0].Instructions) != 1<<16 {
		t.Fatal("rule text truncated")
	}
}
