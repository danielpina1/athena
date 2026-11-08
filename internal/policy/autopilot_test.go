package policy

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
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

func autopilotSettings(mode string) Settings {
	s := testSettings(mode)
	s.Autopilot = []AutopilotEntry{okEntry()}
	return s
}

func scheduleTrigger(slug string) core.Trigger {
	return core.Trigger{ID: "schedule:night:1", Source: ScheduleSource, Kind: core.KindSchedule, Scope: "schedule",
		Meta: map[string]string{"autopilot": slug, "repo": "o/r"}}
}

func TestGateAdmitsEnabledEntry(t *testing.T) {
	g := New(autopilotSettings("observe"), allListen, time.Minute).Gate(scheduleTrigger("night-job"), core.Thread{}, nil)
	if g.Strength != "inferred" || g.Basis.Via != core.ViaAutopilot || g.Basis.Sender != "night-job" ||
		!reflect.DeepEqual(g.Basis.Rules, []string{"autopilot/night-job"}) {
		t.Fatalf("gate = %+v", g)
	}
}

func TestGateRefusesGoneEntry(t *testing.T) {
	off := autopilotSettings("auto")
	off.Autopilot[0].Enabled = false
	missing := autopilotSettings("auto")
	missing.Autopilot = nil
	for name, s := range map[string]Settings{"disabled": off, "missing": missing} {
		g := New(s, allListen, time.Minute).Gate(scheduleTrigger("night-job"), core.Thread{}, nil)
		if g.Strength != "" || g.Reason.Code != CodeAutopilotGone {
			t.Errorf("%s: gate = %+v", name, g)
		}
	}
}

func TestGateModeOffRefuses(t *testing.T) {
	g := New(autopilotSettings("off"), allListen, time.Minute).Gate(scheduleTrigger("night-job"), core.Thread{}, nil)
	if g.Strength != "" || g.Reason.Code != CodeModeOff {
		t.Fatalf("gate = %+v", g)
	}
}

func TestAssessAutopilot(t *testing.T) {
	yes := core.Assessment{Accept: true, Reason: "due"}
	for _, tc := range []struct {
		mode, kind, code string
		would            bool
	}{{"auto", "accept", CodeSaid, false}, {"observe", "ignore", CodeWouldAccept, true}, {"off", "ignore", CodeModeOff, false}} {
		k, rs, w, err := New(autopilotSettings(tc.mode), allListen, time.Minute).Assess(yes, "inferred", core.ViaAutopilot)
		if err != nil || k != tc.kind || w != tc.would || len(rs) == 0 || rs[0].Code != tc.code {
			t.Errorf("%s: %q %+v %v %v", tc.mode, k, rs, w, err)
		}
	}
	p := New(autopilotSettings("auto"), allListen, time.Minute)
	if k, _, _, err := p.Assess(core.Assessment{Accept: false}, "inferred", core.ViaAutopilot); err != nil || k != "ignore" {
		t.Errorf("reject: %q %v", k, err)
	}
	stray := core.Assessment{Accept: true, Rules: []core.RuleJudgement{{Name: "nope", Matches: true}}}
	if k, _, _, err := p.Assess(stray, "inferred", core.ViaAutopilot); err != nil || k != "accept" {
		t.Errorf("stray judgement: %q %v", k, err)
	}
}

func TestAssessOtherViaStillJudgesRules(t *testing.T) {
	p := New(autopilotSettings("auto"), allListen, time.Minute)
	if _, _, _, err := p.Assess(core.Assessment{Accept: true}, "inferred", core.ViaAdmission); err == nil {
		t.Fatal("an assessment judging no enabled rule was accepted")
	}
}

func TestAdmitsInferredAutopilotRule(t *testing.T) {
	rules := []string{"autopilot/night-job"}
	off := autopilotSettings("auto")
	off.Autopilot[0].Enabled = false
	for name, tc := range map[string]struct {
		s    Settings
		want bool
	}{"auto": {autopilotSettings("auto"), true}, "observe": {autopilotSettings("observe"), false}, "disabled": {off, false}} {
		if got := New(tc.s, allListen, time.Minute).AdmitsInferred(rules); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestRevalidateAutopilot(t *testing.T) {
	m := core.Mandate{Strength: "inferred", Thread: "schedule:night-job", Basis: core.MandateBasis{Via: core.ViaAutopilot,
		Source: ScheduleSource, Scope: "schedule", Sender: "night-job", Rules: []string{"autopilot/night-job"}}}
	if !New(autopilotSettings("auto"), allListen, time.Minute).Revalidate(m) {
		t.Error("an enabled entry in auto mode does not hold")
	}
	if New(autopilotSettings("auto"), listens(func(string) bool { return false }), time.Minute).Revalidate(m) {
		t.Error("held although the Thread is not listened to")
	}
	if New(autopilotSettings("observe"), allListen, time.Minute).Revalidate(m) {
		t.Error("held outside auto mode")
	}
	gone := autopilotSettings("auto")
	gone.Autopilot = nil
	if New(gone, allListen, time.Minute).Revalidate(m) {
		t.Error("held after the entry went")
	}
}

func TestTrustOfScheduleIsOwner(t *testing.T) {
	if got := New(autopilotSettings("auto"), allListen, time.Minute).Trust(scheduleTrigger("night-job")); got != core.TrustOwner {
		t.Fatalf("trust = %q", got)
	}
}

func TestEffectsTellsAutopilotTicket(t *testing.T) {
	tk := core.Ticket{ID: "A", Mandate: core.Mandate{Strength: "inferred", Basis: core.MandateBasis{Via: core.ViaAutopilot,
		Source: ScheduleSource, Sender: "night-job", Rules: []string{"autopilot/night-job"}}}}
	tickets := []core.Ticket{tk}
	old := autopilotSettings("auto")

	off := autopilotSettings("auto")
	off.Autopilot[0].Enabled = false
	n := Effects(tickets, old, off)["A"]
	if !strings.Contains(n, "Autopilot entry") || !strings.Contains(n, "no longer applies") {
		t.Errorf("disabled: %q", n)
	}
	if strings.Count(n, "\n") != 0 || strings.Contains(n, "may no longer start work") {
		t.Errorf("disabled must send exactly one note: %q", n)
	}

	changed := autopilotSettings("auto")
	changed.Autopilot[0].Instructions = "tidy up, twice"
	if n := Effects(tickets, old, changed)["A"]; !strings.Contains(n, "tidy up, twice") {
		t.Errorf("changed: %q", n)
	}

	other := autopilotSettings("auto")
	other.Autopilot = append(other.Autopilot, AutopilotEntry{Name: "Other", Instructions: "x", Repo: "o/r", Enabled: true, Cron: "0 4 * * *"})
	if got := Effects(tickets, old, other); len(got) != 0 {
		t.Errorf("unrelated entry: %v", got)
	}
}
