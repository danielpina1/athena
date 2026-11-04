package policy

import (
	"strings"
	"testing"

	"github.com/danielpina1/athena/internal/core"
)

func ticket(id, strength string, b core.MandateBasis) core.Ticket {
	b.Source = "github"
	return core.Ticket{ID: id, Mandate: mandate(strength, b)}
}

func TestEffectsRuleChanged(t *testing.T) {
	old := testSettings("auto")
	cur := testSettings("auto")
	cur.Rules[0].Instructions = "Bump dependencies, one pull request each."
	cur.Rules[1].Enabled = false

	tickets := []core.Ticket{
		ticket("deps", "inferred", core.MandateBasis{Via: core.ViaBot, Sender: "dependabot[bot]", Rules: []string{"deps"}}),
		ticket("docs", "inferred", core.MandateBasis{Via: core.ViaFollowup, Sender: "bob", Rules: []string{"docs"}}),
		ticket("both", "inferred", core.MandateBasis{Via: core.ViaFollowup, Sender: "bob", Rules: []string{"deps", "docs"}}),
		ticket("explicit", "explicit", core.MandateBasis{Via: core.ViaAddressed, Sender: "alice"}),
	}
	got := Effects(tickets, old, cur)
	if len(got) != 3 {
		t.Fatalf("effects = %v, want deps, docs and both", got)
	}
	if n := got["deps"]; !strings.Contains(n, "deps") || !strings.Contains(n, "one pull request each") {
		t.Errorf("a changed rule's notice must carry its new text: %q", n)
	}
	if n := got["docs"]; !strings.Contains(n, "docs") || !strings.Contains(n, "no longer applies") {
		t.Errorf("a disabled rule's notice must say it no longer applies: %q", n)
	}
	if n := got["both"]; !strings.Contains(n, "one pull request each") || !strings.Contains(n, "no longer applies") {
		t.Errorf("both causes belong in one notice: %q", n)
	}
	if _, ok := got["explicit"]; ok {
		t.Error("a ticket that depends on no rule was notified")
	}

	// A removed rule no longer applies either; a renamed-only change elsewhere affects nobody.
	gone := testSettings("auto")
	gone.Rules = gone.Rules[1:]
	if n := Effects(tickets[:1], old, gone)["deps"]; !strings.Contains(n, "no longer applies") {
		t.Errorf("removed rule: %q", n)
	}
	if got := Effects(tickets, old, old); len(got) != 0 {
		t.Errorf("no change, effects = %v", got)
	}
	// A rule that was already off, or an unrelated rule, changes nothing for this ticket.
	other := testSettings("auto")
	other.Rules[1].Instructions = "different"
	if got := Effects(tickets[:1], old, other); len(got) != 0 {
		t.Errorf("an unrelated rule change notified %v", got)
	}
}

func TestEffectsRequesterRemoved(t *testing.T) {
	old := testSettings("auto")
	cur := testSettings("auto")
	p := cur.Sources["github"]
	p.Allowed = []string{"carol"}
	p.Bots = nil
	cur.Sources["github"] = p

	tickets := []core.Ticket{
		ticket("addressed", "explicit", core.MandateBasis{Via: core.ViaAddressed, Sender: "alice"}),
		ticket("listedCell", "inferred", core.MandateBasis{Via: core.ViaAdmission, Scope: "comments", Row: CellListedUnaddressed, Sender: "alice", Rules: []string{"deps"}}),
		ticket("bot", "inferred", core.MandateBasis{Via: core.ViaBot, Sender: "dependabot[bot]", Rules: []string{"deps"}}),
		ticket("followup", "explicit", core.MandateBasis{Via: core.ViaFollowup, Sender: "alice"}),
		ticket("stillOK", "explicit", core.MandateBasis{Via: core.ViaAddressed, Sender: "carol"}),
	}
	got := Effects(tickets, old, cur)
	for _, id := range []string{"addressed", "listedCell", "bot"} {
		if n := got[id]; !strings.Contains(n, "may no longer start work") {
			t.Errorf("%s: notice %q", id, n)
		}
	}
	for _, id := range []string{"followup", "stillOK"} {
		if n, ok := got[id]; ok {
			t.Errorf("%s was notified: %q", id, n)
		}
	}

	// Weakening a cell the Mandate was accepted under affects it too.
	weak := withCell(testSettings("auto"), "comments", CellListedUnaddressed, "ignore")
	if n := Effects(tickets[1:2], old, weak)["listedCell"]; !strings.Contains(n, "may no longer start work") {
		t.Errorf("weakened cell: %q", n)
	}
	// An ended Ticket is not notified.
	ended := tickets[0]
	ended.Ended = &core.Ended{By: core.EndByExecutor}
	if got := Effects([]core.Ticket{ended}, old, cur); len(got) != 0 {
		t.Errorf("an ended ticket was notified: %v", got)
	}
}

func TestEffectsModeLeftAuto(t *testing.T) {
	old := testSettings("auto")
	tickets := []core.Ticket{
		ticket("proactive", "inferred", core.MandateBasis{Via: core.ViaBot, Sender: "dependabot[bot]", Rules: []string{"deps"}}),
		ticket("explicit", "explicit", core.MandateBasis{Via: core.ViaAddressed, Sender: "alice"}),
	}
	for _, mode := range []string{"observe", "off"} {
		got := Effects(tickets, old, testSettings(mode))
		if n := got["proactive"]; !strings.Contains(n, "no longer accepted") || !strings.Contains(n, mode) {
			t.Errorf("mode %s: notice %q", mode, n)
		}
		if _, ok := got["explicit"]; ok {
			t.Errorf("mode %s: an explicit Mandate was notified", mode)
		}
	}
	// Moving between modes that never admitted proactive work notifies nobody.
	if got := Effects(tickets, testSettings("observe"), testSettings("off")); len(got) != 0 {
		t.Errorf("observe to off: %v", got)
	}
}
