package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

type listens func(thread string) bool

func (f listens) Listens(thread string) bool { return f(thread) }

var allListen = listens(func(string) bool { return true })

const threadKey = "github:acme/app#7"

// testSettings returns the defaults for a Source "github" whose allowed list is Alice, with a bot list and two rules.
func testSettings(mode string) Settings {
	s := Defaults([]string{"Alice"})
	s.Mode = mode
	p := s.Sources["github"]
	p.Bots = []string{"dependabot[bot]"}
	s.Sources["github"] = p
	s.Rules = []Rule{
		{Name: "deps", Instructions: "Bump dependencies.", Enabled: true},
		{Name: "docs", Instructions: "Fix typos in docs.", Enabled: true},
	}
	return s
}

// withCell sets one admission cell of the GitHub part.
func withCell(s Settings, scope, cell, value string) Settings {
	p := s.Sources["github"]
	adm := map[string]Row{}
	for k, v := range p.Admission {
		adm[k] = v
	}
	r := adm[scope]
	switch cell {
	case CellListedUnaddressed:
		r.ListedUnaddressed = value
	case CellUnlistedAddressed:
		r.UnlistedAddressed = value
	case CellUnlistedUnaddressed:
		r.UnlistedUnaddressed = value
	}
	adm[scope] = r
	p.Admission = adm
	s.Sources["github"] = p
	return s
}

func person(id, addressed, scope string, receiveAll bool) core.Trigger {
	t := core.Trigger{ID: "github:issuecomment:1", Source: "github", Actor: core.ActorRef{Source: "github", ID: id},
		Human: true, Addressed: addressed, Scope: scope, Kind: core.KindMessage, Threads: []string{threadKey}}
	if receiveAll {
		t.Meta = map[string]string{"receiveAll": "true"}
	}
	return t
}

func bot(id, addressed string, receiveAll bool) core.Trigger {
	t := person(id, addressed, "comments", receiveAll)
	t.Human, t.Bot = false, true
	return t
}

func ci(receiveAll bool) core.Trigger {
	t := core.Trigger{ID: "github:checksuite:9", Source: "github", Kind: core.KindCI, Scope: "ci",
		Threads: []string{"github:acme/app@main"}}
	if receiveAll {
		t.Meta = map[string]string{"receiveAll": "true"}
	}
	return t
}

// follow returns a Thread with a Subscription to T1 and the Tickets it names.
func follow(contextOnly bool, ended *core.Ended) (core.Thread, []core.Ticket) {
	th := core.Thread{Key: threadKey, Source: "github", Subscriptions: []core.Subscription{{TicketID: "T1", Context: contextOnly}}}
	return th, []core.Ticket{{ID: "T1", Ended: ended}}
}

var (
	endedByExecutor = &core.Ended{By: core.EndByExecutor, Outcome: core.OutcomeCompleted}
	endedByPerson   = &core.Ended{By: core.EndByHuman, Outcome: core.OutcomeStopped}
)

func TestGate(t *testing.T) {
	openT, openTickets := follow(false, nil)
	doneT, doneTickets := follow(false, endedByExecutor)
	stoppedT, stoppedTickets := follow(false, endedByPerson)
	ctxT, ctxTickets := follow(true, nil)
	none := core.Thread{Key: threadKey, Source: "github"}

	tests := []struct {
		name    string
		s       Settings
		trig    core.Trigger
		th      core.Thread
		tickets []core.Ticket
		// want
		strength    string
		via, row    string
		observation bool
		code        string
	}{
		{name: "listed + mention is explicit", s: testSettings("observe"), trig: person("Alice", "mention", "comments", false), th: none,
			strength: "explicit", via: core.ViaAddressed},
		{name: "listed + mention explicit in mode off", s: testSettings("off"), trig: person("alice", "mention", "issues", false), th: none,
			strength: "explicit", via: core.ViaAddressed},
		{name: "listed follow-up on a Thread with an open Ticket is explicit", s: testSettings("observe"), trig: person("ALICE", "", "comments", false),
			th: openT, tickets: openTickets, strength: "explicit", via: core.ViaFollowup},
		{name: "listed follow-up counts an ended Ticket a person did not stop", s: testSettings("auto"), trig: person("alice", "", "comments", false),
			th: doneT, tickets: doneTickets, strength: "explicit", via: core.ViaFollowup},
		{name: "a Ticket a person stopped gives no follow-up strength", s: testSettings("auto"), trig: person("alice", "", "comments", false),
			th: stoppedT, tickets: stoppedTickets, code: CodeNotReceived},
		{name: "unlisted follow-up is inferred (observe mode, no observation)", s: testSettings("observe"), trig: person("bob", "", "comments", false),
			th: openT, tickets: openTickets, strength: "inferred", via: core.ViaFollowup},
		{name: "unlisted follow-up is inferred (auto)", s: testSettings("auto"), trig: person("bob", "", "comments", false),
			th: openT, tickets: openTickets, strength: "inferred", via: core.ViaFollowup},
		{name: "unlisted follow-up in mode off is not admitted", s: testSettings("off"), trig: person("bob", "", "comments", false),
			th: openT, tickets: openTickets, code: CodeModeOff},
		{name: "explicit unlisted-addressed cell outranks follow-up strength", s: withCell(testSettings("observe"), "comments", CellUnlistedAddressed, "explicit"),
			trig: person("bob", "mention", "comments", false), th: openT, tickets: openTickets,
			strength: "explicit", via: core.ViaAdmission, row: CellUnlistedAddressed},
		{name: "explicit unlisted-addressed cell works in mode off", s: withCell(testSettings("off"), "issues", CellUnlistedAddressed, "explicit"),
			trig: person("bob", "mention", "issues", false), th: none, strength: "explicit", via: core.ViaAdmission, row: CellUnlistedAddressed},
		{name: "unlisted mention with the default cell is ignored", s: testSettings("auto"), trig: person("bob", "mention", "comments", false),
			th: none, code: CodeNotAMember},
		{name: "unlisted mention with cell inferred is inferred", s: withCell(testSettings("observe"), "pulls", CellUnlistedAddressed, "inferred"),
			trig: person("bob", "mention", "pulls", false), th: none, strength: "inferred", via: core.ViaAdmission, row: CellUnlistedAddressed},
		{name: "unlisted mention with cell inferred in mode off", s: withCell(testSettings("off"), "pulls", CellUnlistedAddressed, "inferred"),
			trig: person("bob", "mention", "pulls", false), th: none, code: CodeModeOff},
		{name: "listed unaddressed in a receive-all scope uses the default inferred cell", s: testSettings("auto"),
			trig: person("alice", "", "comments", true), th: none, strength: "inferred", via: core.ViaAdmission, row: CellListedUnaddressed, observation: true},
		{name: "observed cell admission carries Observation in observe mode too", s: testSettings("observe"), trig: person("alice", "", "issues", true), th: none,
			strength: "inferred", via: core.ViaAdmission, row: CellListedUnaddressed, observation: true},
		{name: "listed unaddressed where only mentions are received", s: testSettings("auto"),
			trig: person("alice", "", "comments", false), th: none, code: CodeNotReceived},
		{name: "listed unaddressed with cell ignore", s: withCell(testSettings("auto"), "comments", CellListedUnaddressed, "ignore"),
			trig: person("alice", "", "comments", true), th: none, code: CodeAdmissionIgnore},
		{name: "unlisted unaddressed with the default cell", s: testSettings("auto"), trig: person("bob", "", "comments", true), th: none, code: CodeNotAMember},
		{name: "unlisted unaddressed with cell inferred", s: withCell(testSettings("auto"), "comments", CellUnlistedUnaddressed, "inferred"),
			trig: person("bob", "", "comments", true), th: none, strength: "inferred", via: core.ViaAdmission, row: CellUnlistedUnaddressed, observation: true},
		{name: "unlisted unaddressed cell inferred but only mentions received", s: withCell(testSettings("auto"), "comments", CellUnlistedUnaddressed, "inferred"),
			trig: person("bob", "", "comments", false), th: none, code: CodeNotReceived},
		{name: "context-only Thread: unlisted follow-up uses the table", s: testSettings("auto"), trig: person("bob", "", "comments", true),
			th: ctxT, tickets: ctxTickets, code: CodeNotAMember},
		{name: "context-only Thread: listed person is not explicit", s: testSettings("auto"), trig: person("alice", "", "comments", true),
			th: ctxT, tickets: ctxTickets, strength: "inferred", via: core.ViaAdmission, row: CellListedUnaddressed, observation: true},
		{name: "listed bot + mention is inferred", s: testSettings("auto"), trig: bot("dependabot[bot]", "mention", false), th: none,
			strength: "inferred", via: core.ViaBot},
		{name: "listed bot where every message is received", s: testSettings("observe"), trig: bot("Dependabot[bot]", "", true), th: none,
			strength: "inferred", via: core.ViaBot},
		{name: "listed bot unaddressed where only mentions are received", s: testSettings("auto"), trig: bot("dependabot[bot]", "", false), th: none,
			code: CodeNotReceived},
		{name: "listed bot in mode off", s: testSettings("off"), trig: bot("dependabot[bot]", "mention", false), th: none, code: CodeModeOff},
		{name: "unlisted bot is not admitted", s: testSettings("auto"), trig: bot("renovate[bot]", "mention", true), th: none, code: CodeBotNotListed},
		{name: "a bot gets no follow-up strength", s: testSettings("auto"), trig: bot("renovate[bot]", "", true), th: openT, tickets: openTickets, code: CodeBotNotListed},
		{name: "a bot cannot use the person cells", s: withCell(testSettings("auto"), "comments", CellUnlistedAddressed, "explicit"),
			trig: bot("renovate[bot]", "mention", true), th: none, code: CodeBotNotListed},
		{name: "CI with cell inferred is inferred", s: withCell(testSettings("auto"), "ci", CellUnlistedUnaddressed, "inferred"),
			trig: ci(true), th: none, strength: "inferred", via: core.ViaAdmission, row: CellUnlistedUnaddressed, observation: true},
		{name: "CI with the default cell is ignored", s: testSettings("auto"), trig: ci(true), th: none, code: CodeAdmissionIgnore},
		{name: "CI where the scope does not receive every message", s: withCell(testSettings("auto"), "ci", CellUnlistedUnaddressed, "inferred"),
			trig: ci(false), th: none, code: CodeNotReceived},
		{name: "CI in mode off", s: withCell(testSettings("off"), "ci", CellUnlistedUnaddressed, "inferred"), trig: ci(true), th: none, code: CodeModeOff},
		{name: "a Source without a Policy part admits nothing", s: Settings{Mode: "auto"}, trig: person("alice", "mention", "comments", false), th: none,
			code: CodeSourceUnconfigured},
		{name: "a Trigger neither human, bot nor CI", s: testSettings("auto"), trig: core.Trigger{ID: "x", Source: "github", Scope: "comments", Addressed: "mention"},
			th: none, code: CodeNotAPerson},
		{name: "unknown scope row", s: testSettings("auto"), trig: person("bob", "mention", "wiki", false), th: none, code: CodeUnknownScope},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := New(tc.s, allListen, time.Minute)
			g := p.Gate(tc.trig, tc.th, tc.tickets)
			if g.Strength != tc.strength {
				t.Fatalf("strength = %q, want %q (reason %+v)", g.Strength, tc.strength, g.Reason)
			}
			if tc.strength == "" {
				if g.Reason.Code != tc.code {
					t.Fatalf("reason = %+v, want code %q", g.Reason, tc.code)
				}
				if g.Reason.TriggerID != tc.trig.ID {
					t.Fatalf("reason trigger = %q, want %q", g.Reason.TriggerID, tc.trig.ID)
				}
				if g.Basis.Via != "" {
					t.Fatalf("a refusal carries a basis: %+v", g.Basis)
				}
				return
			}
			if g.Reason.Code != "" {
				t.Fatalf("an admission carries a reason: %+v", g.Reason)
			}
			b := g.Basis
			if b.Via != tc.via || b.Row != tc.row || b.Observation != tc.observation {
				t.Fatalf("basis = %+v, want via %q row %q observation %v", b, tc.via, tc.row, tc.observation)
			}
			if b.Source != "github" || b.Scope != tc.trig.Scope || b.Sender != tc.trig.Actor.ID {
				t.Fatalf("basis source/scope/sender = %+v for trigger %+v", b, tc.trig.Actor)
			}
		})
	}
}

func TestGateDoesNotShareSettings(t *testing.T) {
	s := testSettings("auto")
	p := New(s, allListen, time.Minute)
	v := p.Version()
	s.Mode = "off"
	s.Sources["github"] = SourcePart{}
	if p.Version() != v || p.Gate(person("alice", "mention", "comments", false), core.Thread{}, nil).Strength != "explicit" {
		t.Fatal("changing the settings a Policy was built from changed the Policy")
	}
}

func mandate(strength string, b core.MandateBasis) core.Mandate {
	return core.Mandate{Strength: strength, PolicyVersion: "v1", Actor: core.ActorRef{Source: "github", ID: b.Sender},
		Scope: "repo", Thread: threadKey, Basis: b}
}

func TestRevalidate(t *testing.T) {
	addressed := mandate("explicit", core.MandateBasis{Via: core.ViaAddressed, Source: "github", Scope: "comments", Sender: "alice"})
	followupExplicit := mandate("explicit", core.MandateBasis{Via: core.ViaFollowup, Source: "github", Scope: "comments", Sender: "alice"})
	followupInferred := mandate("inferred", core.MandateBasis{Via: core.ViaFollowup, Source: "github", Scope: "comments", Sender: "bob", Rules: []string{"deps"}})
	cellExplicit := mandate("explicit", core.MandateBasis{Via: core.ViaAdmission, Source: "github", Scope: "comments", Row: CellUnlistedAddressed, Sender: "bob"})
	cellListed := mandate("inferred", core.MandateBasis{Via: core.ViaAdmission, Source: "github", Scope: "comments", Row: CellListedUnaddressed, Sender: "alice", Rules: []string{"docs"}})
	cellUnlisted := mandate("inferred", core.MandateBasis{Via: core.ViaAdmission, Source: "github", Scope: "comments", Row: CellUnlistedUnaddressed, Sender: "bob", Rules: []string{"docs"}})
	cellCI := mandate("inferred", core.MandateBasis{Via: core.ViaAdmission, Source: "github", Scope: "ci", Row: CellUnlistedUnaddressed, Rules: []string{"deps"}})
	botM := mandate("inferred", core.MandateBasis{Via: core.ViaBot, Source: "github", Scope: "comments", Sender: "dependabot[bot]", Rules: []string{"deps"}})
	owner := core.Mandate{Strength: "explicit", PolicyVersion: core.OwnerPolicyVersion, Actor: core.OwnerActor, Scope: "owner", Basis: core.MandateBasis{Via: core.ViaOwner}}

	mut := func(s Settings, f func(p *SourcePart)) Settings {
		p := s.Sources["github"]
		f(&p)
		s.Sources["github"] = p
		return s
	}
	tests := []struct {
		name  string
		s     Settings
		m     core.Mandate
		scope core.SourceScope
		want  bool
	}{
		{"addressed: sender still listed", testSettings("off"), addressed, allListen, true},
		{"addressed: sender removed", mut(testSettings("auto"), func(p *SourcePart) { p.Allowed = []string{"carol"} }), addressed, allListen, false},
		{"addressed: Source no longer listens to the Thread", testSettings("auto"), addressed, listens(func(string) bool { return false }), false},
		{"addressed: the Source is asked about the Mandate.Thread, not its Scope", testSettings("auto"), addressed, listens(func(th string) bool { return th == threadKey }), true},
		{"addressed: no Thread recorded fails closed", testSettings("auto"), func() core.Mandate { m := addressed; m.Thread = ""; return m }(), allListen, false},
		{"addressed: Source part gone", Settings{Mode: "auto"}, addressed, allListen, false},
		{"explicit follow-up passes even when its sender was removed", mut(testSettings("off"), func(p *SourcePart) { p.Allowed = nil }), followupExplicit, allListen, true},
		{"inferred follow-up: auto and rule enabled", testSettings("auto"), followupInferred, allListen, true},
		{"inferred follow-up: observe mode", testSettings("observe"), followupInferred, allListen, false},
		{"inferred follow-up: rule disabled", func() Settings {
			s := testSettings("auto")
			s.Rules[0].Enabled = false
			return s
		}(), followupInferred, allListen, false},
		{"inferred follow-up: rule removed", func() Settings {
			s := testSettings("auto")
			s.Rules = s.Rules[1:]
			return s
		}(), followupInferred, allListen, false},
		{"inferred without matched rules", testSettings("auto"), mandate("inferred", core.MandateBasis{Via: core.ViaFollowup, Source: "github", Sender: "bob"}), allListen, false},
		{"cell explicit: unchanged", withCell(testSettings("off"), "comments", CellUnlistedAddressed, "explicit"), cellExplicit, allListen, true},
		{"cell explicit weakened to inferred", withCell(testSettings("auto"), "comments", CellUnlistedAddressed, "inferred"), cellExplicit, allListen, false},
		{"cell explicit: sender now listed", withCell(testSettings("auto"), "comments", CellUnlistedAddressed, "ignore"), func() core.Mandate {
			m := cellExplicit
			m.Basis.Sender = "alice"
			return m
		}(), allListen, true},
		{"listed cell: unchanged under auto", testSettings("auto"), cellListed, allListen, true},
		{"listed cell: observe mode", testSettings("observe"), cellListed, allListen, false},
		{"listed cell: sender removed from the list", mut(testSettings("auto"), func(p *SourcePart) { p.Allowed = []string{"carol"} }), cellListed, allListen, false},
		{"listed cell: sender removed but unlisted cell inferred", withCell(mut(testSettings("auto"), func(p *SourcePart) { p.Allowed = []string{"carol"} }),
			"comments", CellUnlistedUnaddressed, "inferred"), cellListed, allListen, true},
		{"listed cell weakened to ignore", withCell(testSettings("auto"), "comments", CellListedUnaddressed, "ignore"), cellListed, allListen, false},
		{"listed cell: matched rule disabled", func() Settings {
			s := testSettings("auto")
			s.Rules[1].Enabled = false
			return s
		}(), cellListed, allListen, false},
		{"unlisted cell: weakened to ignore", testSettings("auto"), cellUnlisted, allListen, false},
		{"unlisted cell: still inferred", withCell(testSettings("auto"), "comments", CellUnlistedUnaddressed, "inferred"), cellUnlisted, allListen, true},
		{"CI cell: still inferred", withCell(testSettings("auto"), "ci", CellUnlistedUnaddressed, "inferred"), cellCI, allListen, true},
		{"CI cell: weakened", testSettings("auto"), cellCI, allListen, false},
		{"bot: still listed", testSettings("auto"), botM, allListen, true},
		{"bot: removed from the bot list", mut(testSettings("auto"), func(p *SourcePart) { p.Bots = nil }), botM, allListen, false},
		{"bot: observe mode", testSettings("observe"), botM, allListen, false},
		{"bot: rule disabled", func() Settings {
			s := testSettings("auto")
			s.Rules[0].Enabled = false
			return s
		}(), botM, allListen, false},
		{"owner Mandate always holds", Settings{Mode: "off"}, owner, listens(func(string) bool { return false }), true},
		{"unknown basis fails closed", testSettings("auto"), mandate("explicit", core.MandateBasis{Via: "mystery", Source: "github"}), allListen, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := New(tc.s, tc.scope, time.Minute).Revalidate(tc.m); got != tc.want {
				t.Fatalf("Revalidate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAdmitsInferred(t *testing.T) {
	s := testSettings("auto")
	s.Rules = append(s.Rules, Rule{Name: "off-rule", Enabled: false})
	tests := []struct {
		mode  string
		rules []string
		want  bool
	}{
		{"auto", []string{"deps"}, true},
		{"auto", []string{"deps", "docs"}, true},
		{"auto", nil, false},
		{"auto", []string{"off-rule"}, false},
		{"auto", []string{"deps", "gone"}, false},
		{"observe", []string{"deps"}, false},
		{"off", []string{"deps"}, false},
	}
	for _, tc := range tests {
		s.Mode = tc.mode
		if got := New(s, allListen, time.Minute).AdmitsInferred(tc.rules); got != tc.want {
			t.Errorf("mode %s rules %v: %v, want %v", tc.mode, tc.rules, got, tc.want)
		}
	}
}

func judged(matches ...bool) core.Assessment {
	names := []string{"deps", "docs"}
	a := core.Assessment{Accept: true, Reason: "a dependency bump"}
	for i, m := range matches {
		j := core.RuleJudgement{Name: names[i], Matches: m}
		if m {
			j.Evidence = "the issue asks to bump a dependency"
		}
		a.Rules = append(a.Rules, j)
	}
	return a
}

func TestAssessmentRequiresEveryEnabledRule(t *testing.T) {
	s := testSettings("auto")
	s.Rules = append(s.Rules, Rule{Name: "retired", Enabled: false})

	bad := map[string]core.Assessment{
		"a rule is missing": judged(true),
		"a rule is judged twice": {Accept: true, Rules: append(judged(true, false).Rules,
			core.RuleJudgement{Name: "deps", Matches: true, Evidence: "again"})},
		"a rule that is not enabled is judged": {Accept: true, Rules: append(judged(true, false).Rules,
			core.RuleJudgement{Name: "retired", Matches: true, Evidence: "x"})},
		"an unknown rule is judged": {Accept: true, Rules: []core.RuleJudgement{{Name: "deps", Matches: true, Evidence: "x"}, {Name: "docs"}, {Name: "nope"}}},
		"no judgements":             {Accept: true},
	}
	for name, a := range bad {
		t.Run(name, func(t *testing.T) {
			kind, reasons, would, err := AssessmentOutcome(a, s, "inferred")
			if err == nil {
				t.Fatalf("got kind %q reasons %v would %v, want an error", kind, reasons, would)
			}
		})
	}

	// A rejecting assessment still has to judge every rule.
	if _, _, _, err := AssessmentOutcome(core.Assessment{Accept: false, Rules: judged(false).Rules}, s, "inferred"); err == nil {
		t.Fatal("a rejection that skipped a rule was accepted")
	}
	// With nothing enabled, the empty judgement list is complete (and cannot match).
	empty := Defaults([]string{"alice"})
	kind, reasons, _, err := AssessmentOutcome(core.Assessment{Accept: true, Reason: "why not"}, empty, "inferred")
	if err != nil || kind != "ignore" || len(reasons) == 0 || reasons[0].Code != CodeNoMatchingRule {
		t.Fatalf("no enabled rules: kind %q reasons %+v err %v", kind, reasons, err)
	}
}

func TestObserveRecordsWouldAccept(t *testing.T) {
	s := testSettings("observe")
	kind, reasons, would, err := AssessmentOutcome(judged(true, false), s, "inferred")
	if err != nil || kind != "ignore" || !would {
		t.Fatalf("kind %q would %v err %v, want ignore, would-accept", kind, would, err)
	}
	if len(reasons) == 0 || reasons[0].Code != CodeWouldAccept {
		t.Fatalf("reasons = %+v, want %s first", reasons, CodeWouldAccept)
	}
	if got := reasons[len(reasons)-1]; got.Code != CodeSaid || got.Values["text"] != "a dependency bump" {
		t.Fatalf("the assessor's words are not kept: %+v", reasons)
	}
	// A negative assessment in observe mode is a plain ignore.
	kind, _, would, err = AssessmentOutcome(core.Assessment{Accept: false, Reason: "chatter", Rules: judged(false, false).Rules}, s, "inferred")
	if err != nil || kind != "ignore" || would {
		t.Fatalf("negative: kind %q would %v err %v", kind, would, err)
	}
	// The Policy delegates with its own settings.
	p := New(s, allListen, time.Minute)
	k2, r2, w2, err := p.Assess(judged(true, false), "inferred")
	if err != nil || k2 != "ignore" || !w2 || len(r2) != len(reasons) {
		t.Fatalf("Policy.Assess = %q %+v %v %v", k2, r2, w2, err)
	}
}

func TestAutoAcceptsWithMatchingRule(t *testing.T) {
	s := testSettings("auto")
	kind, reasons, would, err := AssessmentOutcome(judged(false, true), s, "inferred")
	if err != nil || kind != "accept" || would {
		t.Fatalf("kind %q would %v err %v, want accept", kind, would, err)
	}
	if len(reasons) != 1 || reasons[0].Code != CodeSaid {
		t.Fatalf("reasons = %+v", reasons)
	}

	t.Run("no matching rule is an ignore", func(t *testing.T) {
		kind, reasons, would, err := AssessmentOutcome(judged(false, false), s, "inferred")
		if err != nil || kind != "ignore" || would || reasons[0].Code != CodeNoMatchingRule {
			t.Fatalf("kind %q reasons %+v would %v err %v", kind, reasons, would, err)
		}
	})
	t.Run("a match without evidence is an ignore", func(t *testing.T) {
		a := judged(true, false)
		a.Rules[0].Evidence = "  "
		kind, reasons, _, err := AssessmentOutcome(a, s, "inferred")
		if err != nil || kind != "ignore" || reasons[0].Code != CodeNoEvidence {
			t.Fatalf("kind %q reasons %+v err %v", kind, reasons, err)
		}
	})
	t.Run("the assessor says no", func(t *testing.T) {
		a := judged(true, true)
		a.Accept = false
		a.Reason = "actually just chat"
		kind, reasons, would, err := AssessmentOutcome(a, s, "inferred")
		if err != nil || kind != "ignore" || would || reasons[0].Code != CodeSaid || reasons[0].Values["text"] != "actually just chat" {
			t.Fatalf("kind %q reasons %+v would %v err %v", kind, reasons, would, err)
		}
	})
	t.Run("mode off never accepts an inferred candidate", func(t *testing.T) {
		off := testSettings("off")
		kind, reasons, would, err := AssessmentOutcome(judged(true, false), off, "inferred")
		if err != nil || kind != "ignore" || would || reasons[0].Code != CodeModeOff {
			t.Fatalf("kind %q reasons %+v would %v err %v", kind, reasons, would, err)
		}
	})
	t.Run("an explicit candidate follows the assessor in every mode", func(t *testing.T) {
		for _, mode := range []string{"off", "observe", "auto"} {
			kind, _, would, err := AssessmentOutcome(core.Assessment{Accept: true, Reason: "ok"}, testSettings(mode), "explicit")
			if err != nil || kind != "accept" || would {
				t.Fatalf("%s: kind %q would %v err %v", mode, kind, would, err)
			}
		}
	})
	t.Run("a strength that was not admitted is an error", func(t *testing.T) {
		if _, _, _, err := AssessmentOutcome(judged(true, false), s, ""); err == nil {
			t.Fatal("assessed a candidate with no strength")
		}
	})
}

func versionOf(t *testing.T, doc string) string {
	t.Helper()
	var s Settings
	if err := json.NewDecoder(strings.NewReader(doc)).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return Version(s)
}

func TestVersionStableUnderKeyOrder(t *testing.T) {
	a := `{"mode":"auto","rules":[{"name":"deps","instructions":"Bump.","enabled":true},{"name":"old","instructions":"x","enabled":false}],
	"sources":{"github":{"allowed":["Alice","bob"],"bots":["dependabot[bot]"],
	"admission":{"issues":{"listedUnaddressed":"inferred","unlistedAddressed":"ignore","unlistedUnaddressed":"ignore"},
	"comments":{"listedUnaddressed":"ignore","unlistedAddressed":"inferred","unlistedUnaddressed":"ignore"}}},
	"feishu":{"allowed":["u1"]}}}`
	b := `{"sources":{"feishu":{"allowed":["u1"]},"github":{"admission":{"comments":{"unlistedUnaddressed":"ignore","unlistedAddressed":"inferred","listedUnaddressed":"ignore"},
	"issues":{"unlistedUnaddressed":"ignore","listedUnaddressed":"inferred","unlistedAddressed":"ignore"}},
	"bots":["dependabot[bot]"],"allowed":["BOB","alice"]}},
	"rules":[{"enabled":true,"instructions":"Bump.","name":"deps"}],"mode":"auto"}`
	va, vb := versionOf(t, a), versionOf(t, b)
	if va != vb {
		t.Fatalf("version depends on key or list order or on disabled rules: %s vs %s", va, vb)
	}
	if len(va) != 12 || strings.Trim(va, "0123456789abcdef") != "" {
		t.Fatalf("version %q is not 12 hex characters", va)
	}
	// An omitted cell is its default.
	c := `{"mode":"auto","rules":[{"name":"deps","instructions":"Bump.","enabled":true}],"sources":{"github":{"allowed":["alice","bob"],"bots":["dependabot[bot]"],
	"admission":{"comments":{"unlistedAddressed":"inferred","listedUnaddressed":"ignore"}}}, "feishu":{"allowed":["u1"]}}}`
	d := `{"mode":"auto","rules":[{"name":"deps","instructions":"Bump.","enabled":true}],"sources":{"github":{"allowed":["alice","bob"],"bots":["dependabot[bot]"],
	"admission":{"comments":{"unlistedAddressed":"inferred","listedUnaddressed":"ignore","unlistedUnaddressed":"ignore"},"issues":{}}}, "feishu":{"allowed":["u1"]}}}`
	if versionOf(t, c) != versionOf(t, d) {
		t.Fatal("an omitted admission cell differs from its default")
	}

	// Each part of the Policy changes the version.
	base := testSettings("observe")
	v0 := Version(base)
	changes := map[string]func(s *Settings){
		"mode":         func(s *Settings) { s.Mode = "auto" },
		"instructions": func(s *Settings) { s.Rules[0].Instructions = "Bump carefully." },
		"enabled":      func(s *Settings) { s.Rules[0].Enabled = false },
		"new rule":     func(s *Settings) { s.Rules = append(s.Rules, Rule{Name: "n", Enabled: true}) },
		"allowed": func(s *Settings) {
			p := s.Sources["github"]
			p.Allowed = []string{"alice", "zed"}
			s.Sources["github"] = p
		},
		"bots":      func(s *Settings) { p := s.Sources["github"]; p.Bots = nil; s.Sources["github"] = p },
		"admission": func(s *Settings) { *s = withCell(*s, "pulls", CellUnlistedAddressed, "explicit") },
	}
	for name, f := range changes {
		s := testSettings("observe")
		f(&s)
		if Version(s) == v0 {
			t.Errorf("changing %s kept the version", name)
		}
	}
}
