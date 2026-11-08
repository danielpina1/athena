package policy

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func okEntry() AutopilotEntry {
	return AutopilotEntry{Name: "Night Job", Instructions: "tidy up", Repo: "octo/repo", Enabled: true, Cron: "0 3 * * *"}
}

func TestValidateAutopilot(t *testing.T) {
	with := func(f func(e *AutopilotEntry)) []AutopilotEntry {
		e := okEntry()
		f(&e)
		return []AutopilotEntry{e}
	}
	many := make([]AutopilotEntry, MaxAutopilotEntries+1)
	for i := range many {
		many[i] = okEntry()
		many[i].Name = fmt.Sprintf("job %d", i)
	}
	cases := []struct {
		name    string
		entries []AutopilotEntry
		want    string
	}{
		{"empty name", with(func(e *AutopilotEntry) { e.Name = " " }), "name"},
		{"bad name", with(func(e *AutopilotEntry) { e.Name = "-lead" }), "name"},
		{"long name", with(func(e *AutopilotEntry) { e.Name = strings.Repeat("a", 65) }), "name"},
		{"one slug", []AutopilotEntry{okEntry(), func() AutopilotEntry { e := okEntry(); e.Name = "night-job"; return e }()}, "same"},
		{"no instructions", with(func(e *AutopilotEntry) { e.Instructions = " " }), "instructions"},
		{"long instructions", with(func(e *AutopilotEntry) { e.Instructions = strings.Repeat("x", MaxAutopilotInstructions+1) }), "instructions"},
		{"bad repo", with(func(e *AutopilotEntry) { e.Repo = "octo" }), "repo"},
		{"neither", with(func(e *AutopilotEntry) { e.Cron = "" }), "exactly one"},
		{"both", with(func(e *AutopilotEntry) { e.Event = EventCIRed }), "exactly one"},
		{"unknown event", with(func(e *AutopilotEntry) { e.Cron = ""; e.Event = "nope" }), "event"},
		{"bad cron", with(func(e *AutopilotEntry) { e.Cron = "61 * * * *" }), "cron"},
		{"too frequent", with(func(e *AutopilotEntry) { e.Cron = "*/5 * * * *" }), "15 minutes"},
		{"hours too big", with(func(e *AutopilotEntry) { e.Cron = ""; e.Event = EventCIRed; e.CIRedHours = 169 }), "ciRedHours"},
		{"hours on other", with(func(e *AutopilotEntry) { e.CIRedHours = 4 }), "ciRedHours"},
		{"disabled still checked", with(func(e *AutopilotEntry) { e.Enabled = false; e.Repo = "x" }), "repo"},
		{"too many", many, "50"},
	}
	for _, c := range cases {
		s := Defaults([]string{"octo"})
		s.Autopilot = c.entries
		err := Validate(s)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want substring %q", c.name, err, c.want)
		}
	}
	s := Defaults([]string{"octo"})
	s.Autopilot = []AutopilotEntry{okEntry(), {Name: "ci", Instructions: "fix", Repo: "o/r", Event: EventCIRed, CIRedHours: 168}}
	if err := Validate(s); err != nil {
		t.Fatalf("valid entries refused: %v", err)
	}
}

func TestAutopilotSlug(t *testing.T) {
	if got := AutopilotSlug("Night  Job_2"); got != "night-job-2" {
		t.Fatalf("slug = %q", got)
	}
	if got := AutopilotRule("x"); got != "autopilot/x" {
		t.Fatalf("rule = %q", got)
	}
	s := Defaults(nil)
	s.Autopilot = []AutopilotEntry{okEntry()}
	if e, ok := s.AutopilotEntry("night-job"); !ok || e.Name != "Night Job" {
		t.Fatalf("lookup = %v %v", e, ok)
	}
	if _, ok := s.AutopilotEntry("nope"); ok {
		t.Fatal("found a missing entry")
	}
}

func TestNormalizeKeepsAutopilot(t *testing.T) {
	e := okEntry()
	e.Name, e.Repo = "  Night Job ", " octo/repo "
	s := Defaults([]string{"octo"})
	s.Autopilot = []AutopilotEntry{e}
	n := normalize(s)
	want := okEntry()
	if !reflect.DeepEqual(n.Autopilot, []AutopilotEntry{want}) {
		t.Fatalf("normalized = %+v", n.Autopilot)
	}
	st := openStore(t)
	if _, err := st.Save(s, 0, OriginCLI); err != nil {
		t.Fatal(err)
	}
	got, _, err := st.Current()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Autopilot, []AutopilotEntry{want}) {
		t.Fatalf("reloaded = %+v", got.Autopilot)
	}
}

func TestVersionIgnoresAbsentAndDisabledEntries(t *testing.T) {
	base := Defaults([]string{"octo"})
	if v := Version(base); v != "c69dc3e0f1ff" {
		t.Fatalf("version of defaults = %s", v)
	}
	dis := base
	d := okEntry()
	d.Enabled = false
	dis.Autopilot = []AutopilotEntry{d}
	if Version(dis) != Version(base) {
		t.Fatal("a disabled entry changed the version")
	}
	en := base
	en.Autopilot = []AutopilotEntry{okEntry()}
	if Version(en) == Version(base) {
		t.Fatal("an enabled entry left the version")
	}
	ch := base
	e := okEntry()
	e.Instructions = "other"
	ch.Autopilot = []AutopilotEntry{e}
	if Version(ch) == Version(en) {
		t.Fatal("changed instructions left the version")
	}
}
