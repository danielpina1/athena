package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/sources/schedule"
	"github.com/danielpina1/athena/internal/store"
)

const (
	sweepName  = "Nightly sweep"
	sweepSlug  = "nightly-sweep"
	sweepTitle = "Sweep the repository"
	// autopilotPoll is how often the tests' schedule Source polls repository events.
	autopilotPoll = 20 * time.Millisecond
)

var (
	sweepThread = schedule.ThreadKey(sweepSlug)
	headB       = strings.Repeat("b", 40)
	headC       = strings.Repeat("c", 40)
)

// sweepEntry is an enabled entry on repo that waits for event.
func sweepEntry(repo, event string) policy.AutopilotEntry {
	return policy.AutopilotEntry{Name: sweepName, Instructions: "Tidy what the last change left behind.", Repo: repo,
		Enabled: true, Event: event}
}

// autopilotRun is a server over the fake GitHub with Autopilot settings saved before it started.
type autopilotRun struct {
	*testServer
	t     *testing.T
	g     *fakeGitHub
	d     *Dispatcher
	dir   string
	token string
}

// startAutopilot starts a server over the fake GitHub with the settings made by set, polling repository events every
// autopilotPoll, once the router runs and GitHub is healthy. seed, when set, writes to the store before the start.
// T-0001's worktree directory is made ahead, so no git reaches github.com.
func startAutopilot(t *testing.T, g *fakeGitHub, set func(*policy.Settings), seed func(*store.Store)) *autopilotRun {
	t.Helper()
	fake := newFakeEnv(t, openTurnFixture(t))
	fastEvaluate(t)
	dir := newStateDir(t)
	g.configure(t, dir, 600)
	s := policy.Defaults([]string{"o"})
	set(&s)
	saveSettings(t, dir, s)
	if seed != nil {
		offlineWith(t, dir, time.Now, func(_ *core.Engine, st *store.Store) { seed(st) })
	}
	if err := os.MkdirAll(node.TicketDir(filepath.Join(dir, "work"), core.Ticket{ID: "T-0001", Title: sweepTitle}), 0o700); err != nil {
		t.Fatal(err)
	}
	ready := make(chan *Dispatcher, 1)
	srv := startServerWith(t, dir, func(c *Config) {
		c.ScheduleEventInterval = autopilotPoll
		c.dispatcherReady = func(d *Dispatcher) { ready <- d }
	})
	d := <-ready
	token := routerToken(t, fake)
	eventually(t, "router running and github healthy", func() bool {
		h := srv.health()
		row, ok := sourceRow(h, "github")
		return h.Router.State == "running" && ok && row.Error == ""
	})
	return &autopilotRun{testServer: srv, t: t, g: g, d: d, dir: dir, token: token}
}

func sourceRow(h HealthView, name string) (SourceHealth, bool) {
	for _, row := range h.Sources {
		if row.Name == name {
			return row, true
		}
	}
	return SourceHealth{}, false
}

// waitBaseline waits until the entry has taken sha as the head it starts from: only a later move fires.
func (a *autopilotRun) waitBaseline(sha string) {
	a.t.Helper()
	eventually(a.t, "the entry saw the head", func() bool {
		raw, ok, err := a.d.st.KVGet("schedule.state." + sweepSlug)
		if err != nil {
			a.t.Fatal(err)
		}
		var st struct {
			Base string `json:"base"`
		}
		if ok {
			if err := json.Unmarshal([]byte(raw), &st); err != nil {
				a.t.Fatal(err)
			}
		}
		return st.Base == sha
	})
}

// polled counts the reads of the default branch's head.
func (a *autopilotRun) polled() int {
	n := 0
	for _, r := range a.g.requestLog() {
		if r == "GET /repos/o/r/branches/main" {
			n++
		}
	}
	return n
}

// waitPolls waits until the entry has looked at the repository twice more: a poll that began before the caller's change
// is over by the second.
func (a *autopilotRun) waitPolls() {
	a.t.Helper()
	from := a.polled()
	eventually(a.t, "two more polls", func() bool { return a.polled() >= from+2 })
}

func (a *autopilotRun) triggers() []TriggerView {
	a.t.Helper()
	var list []TriggerView
	a.ok(Request{Op: OpTriggers, Thread: sweepThread}, &list)
	return list
}

func (a *autopilotRun) decisions() []DecisionView {
	a.t.Helper()
	var list []DecisionView
	a.ok(Request{Op: OpDecisions, Thread: sweepThread}, &list)
	return list
}

func (a *autopilotRun) tickets() []TicketSummary {
	a.t.Helper()
	var list []TicketSummary
	a.ok(Request{Op: OpTickets, All: true}, &list)
	return list
}

// waitDelivered waits until the entry's candidate reached the router.
func (a *autopilotRun) waitDelivered() {
	a.t.Helper()
	eventually(a.t, "the candidate reached the router", func() bool {
		for _, th := range a.threads() {
			if th.Key == sweepThread && th.Eval != nil && th.Eval.DeliveredAt != nil {
				return true
			}
		}
		return false
	})
}

// accept has the router assess the candidate as worth doing, through its real eval_decide tool and with no rule
// judgements: the entry is its own rule. There is no acknowledgement, since no Channel would carry it.
func (a *autopilotRun) accept(triggerID string) map[string]any {
	a.t.Helper()
	args, _ := json.Marshal(map[string]any{"trigger_id": triggerID, "rules": []any{}, "accept": true,
		"reason": "the entry's work is due", "title": sweepTitle})
	var rr node.RouterResponse
	nodeCall(a.t, a.dir, node.RouterRequest{Op: node.OpRouter, Token: a.token, Tool: "eval_decide", Args: args}, &rr)
	if !rr.OK {
		a.t.Fatalf("eval_decide = %+v %s", rr, rr.Result)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Result, &out); err != nil {
		a.t.Fatal(err)
	}
	return out
}

func headTrigger(sha string) string { return fmt.Sprintf("schedule:%s:head:%s", sweepSlug, sha) }

func TestAutopilotEntriesMapping(t *testing.T) {
	got := autopilotEntries(policy.Settings{Autopilot: []policy.AutopilotEntry{
		{Name: "Off", Instructions: "x", Repo: "o/r", Cron: "0 3 * * *"},
		{Name: " Night  Job_2 ", Instructions: "tidy", Repo: " o/r ", Enabled: true, Cron: "0 3 * * *"},
		{Name: "Red", Instructions: "fix", Repo: "o/r", Enabled: true, Event: policy.EventCIRed},
		{Name: "Red long", Instructions: "fix", Repo: "o/r", Enabled: true, Event: policy.EventCIRed, CIRedHours: 6},
		{Name: "Moved", Instructions: "look", Repo: "o/r", Enabled: true, Event: policy.EventBranchMoved},
	}})
	want := []schedule.Entry{
		{Slug: "night-job-2", Name: "Night  Job_2", Instructions: "tidy", Repo: "o/r", Cron: "0 3 * * *"},
		{Slug: "red", Name: "Red", Instructions: "fix", Repo: "o/r", Event: policy.EventCIRed, CIRedAfter: 4 * time.Hour},
		{Slug: "red-long", Name: "Red long", Instructions: "fix", Repo: "o/r", Event: policy.EventCIRed, CIRedAfter: 6 * time.Hour},
		{Slug: "moved", Name: "Moved", Instructions: "look", Repo: "o/r", Event: policy.EventBranchMoved},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("entries = %+v, want %+v", got, want)
	}
}

// In observe mode the router's accept records what Athena would have done, and starts nothing.
func TestAutopilotObserveRecordsWouldAccept(t *testing.T) {
	g := newFakeGitHub(t)
	a := startAutopilot(t, g, func(s *policy.Settings) {
		s.Autopilot = []policy.AutopilotEntry{sweepEntry(fakeRepo, policy.EventBranchMoved)}
	}, nil)
	a.waitBaseline(strings.Repeat("a", 40))
	g.setHead(headB)
	a.waitDelivered()
	a.accept(headTrigger(headB))
	list := a.decisions()
	if len(list) != 1 || list[0].Kind != core.DecisionIgnore || !list[0].WouldAccept {
		t.Fatalf("decisions = %+v, want one ignore with WouldAccept", list)
	}
	if got := a.tickets(); len(got) != 0 {
		t.Fatalf("tickets = %+v, want none", got)
	}
}

// In auto mode the router's accept opens a Ticket that works in the entry's repository with the owner's trust, and a
// further move of the branch while it is open reaches neither the Thread nor the Ticket.
func TestAutopilotAutoOpensTicket(t *testing.T) {
	g := newFakeGitHub(t)
	a := startAutopilot(t, g, func(s *policy.Settings) {
		s.Mode = policy.ModeAuto
		s.Autopilot = []policy.AutopilotEntry{sweepEntry(fakeRepo, policy.EventBranchMoved)}
	}, nil)
	a.waitBaseline(strings.Repeat("a", 40))
	g.setHead(headB)
	a.waitDelivered()
	if out := a.accept(headTrigger(headB)); out["decision"] != core.DecisionAccept {
		t.Fatalf("eval_decide = %+v, want an accept", out)
	}
	a.running("T-0001")
	tk, _, err := a.d.eng.Ticket("T-0001")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Repo != fakeRepo || tk.Mandate.Basis.Via != core.ViaAutopilot || tk.Mandate.Trust != core.TrustOwner ||
		!slices.Equal(tk.Mandate.Basis.Rules, []string{policy.AutopilotRule(sweepSlug)}) {
		t.Fatalf("ticket repo %q mandate %+v", tk.Repo, tk.Mandate)
	}
	inputs := len(tk.Inputs)

	g.setHead(headC)
	a.waitPolls()
	if got := a.triggers(); len(got) != 1 {
		t.Fatalf("triggers = %+v, want the first only", got)
	}
	if tk, _, err = a.d.eng.Ticket("T-0001"); err != nil || len(tk.Inputs) != inputs {
		t.Fatalf("inputs = %+v %v, want still %d", tk.Inputs, err, inputs)
	}
}

// With the repository at its ceiling the candidate is held as a Brake; further moves add nothing while it is held, and
// when the owner catches up the one held candidate is evaluated.
func TestAutopilotBrakedThenReleasedOnce(t *testing.T) {
	g := newFakeGitHub(t)
	a := startAutopilot(t, g, func(s *policy.Settings) {
		s.WIPCeiling = 1
		s.Autopilot = []policy.AutopilotEntry{sweepEntry(fakeRepo, policy.EventBranchMoved)}
	}, func(st *store.Store) {
		waiting := &core.Ticket{ID: "T-9001", Title: "Waits on the owner", Repo: fakeRepo,
			Mandate:   core.Mandate{Strength: "explicit", PolicyVersion: core.OwnerPolicyVersion, Basis: core.MandateBasis{Via: core.ViaOwner}},
			Attention: &core.Attention{ID: "q1", Question: "Which page?", At: time.Now()}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if err := st.Tx(func(tx *store.Tx) error { return tx.PutTicket(waiting) }); err != nil {
			t.Fatal(err)
		}
	})
	a.waitBaseline(strings.Repeat("a", 40))
	g.setHead(headB)
	eventually(t, "the candidate was held by the brake", func() bool {
		for _, d := range a.decisions() {
			if d.Kind == core.DecisionIgnore && slices.Contains(d.Reasons, core.CodeWIPBrake) {
				return true
			}
		}
		return false
	})
	var brakes []core.Brake
	if err := a.d.st.Tx(func(tx *store.Tx) (err error) { brakes, err = tx.Brakes(); return }); err != nil {
		t.Fatal(err)
	}
	if len(brakes) != 1 || brakes[0].Thread != sweepThread || brakes[0].TriggerID != headTrigger(headB) {
		t.Fatalf("brakes = %+v, want the first move's on %s", brakes, sweepThread)
	}

	g.setHead(headC)
	a.waitPolls()
	if got := a.triggers(); len(got) != 1 {
		t.Fatalf("triggers while braked = %+v, want the first only", got)
	}
	if got := a.decisions(); len(got) != 1 {
		t.Fatalf("decisions while braked = %+v, want the brake's only", got)
	}

	a.ok(Request{Op: OpStop, ID: "T-9001"}, nil)
	a.waitDelivered()
	a.waitPolls()
	if got := a.triggers(); len(got) != 1 || got[0].ID != headTrigger(headB) {
		t.Fatalf("triggers after the release = %+v, want the held one only", got)
	}
}

// While paused an entry raises nothing; on resume the move it missed fires once.
func TestAutopilotNothingFiresWhilePaused(t *testing.T) {
	g := newFakeGitHub(t)
	a := startAutopilot(t, g, func(s *policy.Settings) {
		s.Autopilot = []policy.AutopilotEntry{sweepEntry(fakeRepo, policy.EventBranchMoved)}
	}, nil)
	a.waitBaseline(strings.Repeat("a", 40))
	a.ok(Request{Op: OpPause}, nil)
	g.setHead(headB)
	time.Sleep(10 * autopilotPoll)
	if got := a.triggers(); len(got) != 0 {
		t.Fatalf("triggers while paused = %+v", got)
	}
	a.ok(Request{Op: OpResume}, nil)
	eventually(t, "the move fired on resume", func() bool { return len(a.triggers()) == 1 })
	a.waitPolls()
	if got := a.triggers(); len(got) != 1 {
		t.Fatalf("triggers after resume = %+v, want one", got)
	}
}

// In a shadow repository an Autopilot Ticket's pull request is held as a Proposal, as any other Ticket's is.
func TestAutopilotShadowRepoHoldsOutput(t *testing.T) {
	g := newFakeGitHub(t)
	a := startAutopilot(t, g, func(s *policy.Settings) {
		s.Mode = policy.ModeAuto
		s.Repos = map[string]policy.RepoSettings{fakeRepo: {Mode: policy.RepoShadow}}
		s.Autopilot = []policy.AutopilotEntry{sweepEntry(fakeRepo, policy.EventBranchMoved)}
	}, nil)
	a.waitBaseline(strings.Repeat("a", 40))
	g.setHead(headB)
	a.waitDelivered()
	if out := a.accept(headTrigger(headB)); out["decision"] != core.DecisionAccept {
		t.Fatalf("eval_decide = %+v, want an accept", out)
	}
	asg := a.running("T-0001")
	var pr node.ProposeResponse
	nodeCall(t, a.dir, node.ProposeRequest{Op: node.OpPropose, Ticket: "T-0001", Assignment: asg.ID,
		Pull: &node.HeldPull{Title: "Sweep the repository"}}, &pr)
	if !pr.OK || !pr.Held {
		t.Fatalf("propose = %+v, want the pull request held", pr)
	}
	ps, err := a.d.eng.PendingProposals()
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, p := range ps {
		if p.TicketID == "T-0001" {
			kinds = append(kinds, p.Kind)
		}
	}
	if !slices.Contains(kinds, core.ProposalPull) {
		t.Fatalf("pending proposals = %v, want a pull request", kinds)
	}
	if n := len(g.openedPulls()); n != 0 {
		t.Fatalf("%d pull requests opened", n)
	}
}

// An entry on a repository the GitHub Source does not poll shows in the schedule row of health.
func TestAutopilotHealthShown(t *testing.T) {
	g := newFakeGitHub(t)
	a := startAutopilot(t, g, func(s *policy.Settings) {
		s.Autopilot = []policy.AutopilotEntry{sweepEntry("o/other", policy.EventBranchMoved)}
	}, nil)
	eventually(t, "the schedule row carries the error", func() bool {
		row, ok := sourceRow(a.health(), schedule.SourceName)
		return ok && strings.Contains(row.Error, "o/other")
	})
}

// An entry that writes its repository in another case than the GitHub Source's configuration is still known: its
// events fire, and health shows nothing wrong.
func TestAutopilotRepoCaseDoesNotMatter(t *testing.T) {
	g := newFakeGitHub(t)
	g.setChanged("cmd/app/main.go", "go.mod")
	a := startAutopilot(t, g, func(s *policy.Settings) {
		s.Autopilot = []policy.AutopilotEntry{sweepEntry("O/R", policy.EventManifestChanged)}
	}, nil)
	a.waitBaseline(strings.Repeat("a", 40))
	g.setHead(headB)
	eventually(t, "the entry fired", func() bool { return len(a.triggers()) == 1 })
	if row, ok := sourceRow(a.health(), schedule.SourceName); !ok || row.Error != "" {
		t.Fatalf("schedule row = %+v (shown: %v), want it healthy", row, ok)
	}
}
