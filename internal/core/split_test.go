package core

import (
	"reflect"
	"strings"
	"testing"
)

func spec(key, title string, mods ...func(*ChildSpec)) ChildSpec {
	s := ChildSpec{Key: key, Title: title, Instructions: "do " + title}
	for _, m := range mods {
		m(&s)
	}
	return s
}

func deps(d ...string) func(*ChildSpec) { return func(s *ChildSpec) { s.Deps = d } }

func TestCheckSpecsRefusals(t *testing.T) {
	six := make([]ChildSpec, 6)
	for i := range six {
		six[i] = spec("", "x")
	}
	long := strings.Repeat("m", 65)
	tests := []struct {
		name     string
		specs    []ChildSpec
		existing []string
		want     string
	}{
		{"none", nil, nil, "a split needs between 1 and 5 sub-tasks; this one has 0"},
		{"six", six, nil, "a split needs between 1 and 5 sub-tasks; this one has 6"},
		{"over cap with existing", []ChildSpec{spec("a", "a"), spec("b", "b")}, []string{"T-0001", "T-0002", "T-0003", "T-0004"}, "a split needs between 1 and 1 sub-tasks; this one has 2"},
		{"duplicate key", []ChildSpec{spec("a", "a"), spec("a", "b")}, nil, `sub-task key "a" is used twice`},
		{"digit key", []ChildSpec{spec("1", "a")}, nil, `sub-task key "1" needs a letter, so it cannot be mistaken for a ticket id`},
		{"bad key", []ChildSpec{spec("A_b", "a")}, nil, `sub-task key "A_b" must be`},
		{"bare number dep", []ChildSpec{spec("a", "a", deps("7"))}, []string{"T-0007"}, `sub-task "a" depends on "7", which is not a sub-task of this split or a child of the lead`},
		{"self dep", []ChildSpec{spec("a", "a", deps("a"))}, nil, `sub-task "a" depends on itself`},
		{"cycle", []ChildSpec{spec("a", "a", deps("b")), spec("b", "b", deps("a"))}, nil, "the sub-tasks' dependencies form a cycle: a → b → a"},
		{"unknown dep", []ChildSpec{spec("a", "a"), spec("b", "b", deps("x"))}, nil, `sub-task "b" depends on "x", which is not a sub-task of this split or a child of the lead`},
		{"unknown ticket dep", []ChildSpec{spec("a", "a", deps("T-0009"))}, []string{"T-0007"}, "which is not a sub-task"},
		{"bad runtime", []ChildSpec{spec("a", "a", func(s *ChildSpec) { s.Runtime = "gpt" })}, nil, `unknown runtime "gpt"`},
		{"empty title", []ChildSpec{spec("a", "")}, nil, "needs a title of 1 to 80 characters"},
		{"long title", []ChildSpec{spec("a", strings.Repeat("é", 81))}, nil, "needs a title of 1 to 80 characters"},
		{"no instructions", []ChildSpec{spec("a", "a", func(s *ChildSpec) { s.Instructions = " " })}, nil, "needs instructions"},
		{"long model", []ChildSpec{spec("a", "a", func(s *ChildSpec) { s.Model = long })}, nil, "model hint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CheckSpecs(tt.specs, tt.existing, 5)
			if err == nil {
				t.Fatal("want a refusal")
			}
			if _, ok := err.(*UserError); !ok {
				t.Fatalf("want *UserError, got %T", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestCheckSpecsNumbersKeys(t *testing.T) {
	in := []ChildSpec{spec("", "a", deps("T-0007")), spec("", "b", deps("s1"))}
	out, err := CheckSpecs(in, []string{"T-0007"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Key != "s1" || out[1].Key != "s2" {
		t.Fatalf("keys %q %q", out[0].Key, out[1].Key)
	}
	if !reflect.DeepEqual(out[0].Deps, []string{"T-0007"}) {
		t.Fatalf("deps %v", out[0].Deps)
	}
	if in[0].Key != "" {
		t.Fatal("input was mutated")
	}
}

func TestOverlaps(t *testing.T) {
	areas := func(a ...string) func(*ChildSpec) { return func(s *ChildSpec) { s.Areas = a } }
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"prefix", "internal/core", "internal/core/engine.go", true},
		{"not a segment prefix", "internal/co", "internal/core", false},
		{"trailing slash", "web/", "web", true},
		{"dot slash", "./web", "web/src", true},
		{"equal", "a/b.go", "a/b.go", true},
		{"disjoint", "web", "internal", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Overlaps([]ChildSpec{spec("a", "a", areas(tt.a)), spec("b", "b", areas(tt.b))})
			if (len(got) == 1) != tt.want {
				t.Fatalf("got %v", got)
			}
			if tt.want && (got[0].A != "a" || got[0].B != "b") {
				t.Fatalf("pair %v", got[0])
			}
		})
	}
	three := Overlaps([]ChildSpec{spec("a", "a", areas("x")), spec("b", "b", areas("y")), spec("c", "c", areas("x/z", "y"))})
	if len(three) != 2 || three[0].B != "c" || three[0].A != "a" || three[1].A != "b" {
		t.Fatalf("order %v", three)
	}
}

func TestParseTicketID(t *testing.T) {
	for in, want := range map[string]string{"T-12": "T-0012", "t-0012": "T-0012", "12": "T-0012", " 7 ": "T-0007"} {
		if got, ok := ParseTicketID(in); !ok || got != want {
			t.Errorf("%q: got %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "T-", "x", "s1", "-3", "0", "T-1a"} {
		if got, ok := ParseTicketID(in); ok {
			t.Errorf("%q: got %q, want no id", in, got)
		}
	}
}

func TestSplitLimitsWithDefaults(t *testing.T) {
	l := SplitLimits{MaxChildren: 8}.WithDefaults()
	if l.MaxChildren != 8 || l.MaxHops != 3 || l.MessageLimit != 10 || l.MessageWindow != DefaultMessageWindow {
		t.Fatalf("%+v", l)
	}
}

func TestStagesOfSplit(t *testing.T) {
	plan := func(state string) func(*Ticket) { return func(t *Ticket) { t.Plan = &PlanState{State: state} } }
	noAssign := func(t *Ticket) { t.Assignments = nil }
	tests := []struct {
		name string
		mods []func(*Ticket)
		want Stage
	}{
		{"planning", []func(*Ticket){noAssign, plan(PlanPlanning)}, Stage{"working", "planning"}},
		{"draft", []func(*Ticket){noAssign, plan(PlanDraft)}, Stage{"person", "plan"}},
		{"failed plan", []func(*Ticket){noAssign, plan(PlanFailed)}, Stage{"person", "plan"}},
		{"approved plan is not a stage", []func(*Ticket){plan(PlanApproved)}, Stage{Stage: "working"}},
		{"child attention outranks a question", []func(*Ticket){func(t *Ticket) { t.Attention = &Attention{ID: "a", Child: "T-0002"} }}, Stage{"person", "child"}},
		{"plain question", []func(*Ticket){func(t *Ticket) { t.Attention = &Attention{ID: "a"} }}, Stage{"person", "answer"}},
		{"waiting", []func(*Ticket){noAssign, func(t *Ticket) { t.Waiting = []string{"T-0002"} }}, Stage{"working", "waiting"}},
		{"waiting with an assignment is not held", []func(*Ticket){func(t *Ticket) { t.Waiting = []string{"T-0002"} }}, Stage{Stage: "working"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TicketStage(newTicket(tt.mods...)); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	ended := newTicket(plan(PlanPlanning), func(t *Ticket) { t.Ended = &Ended{} })
	if got := TicketStage(ended); got.Stage != "ended" {
		t.Fatalf("ended outranks plan: %+v", got)
	}
}
