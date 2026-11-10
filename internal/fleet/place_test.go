package fleet

import (
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

func cand(id, name string, load int, rts ...string) candidate {
	c := candidate{ID: id, Name: name, Online: true, Slots: 4, InUse: load, Ready: map[string]bool{}}
	for _, r := range rts {
		c.Ready[r] = true
	}
	return c
}

func TestChooseRules(t *testing.T) {
	local := cand("local", "server", 0, core.RuntimeClaude)
	local.Local = true
	busyLocal := local
	busyLocal.InUse = 3
	mini := cand("n1", "mini", 1, core.RuntimeClaude)
	studio := cand("n2", "mac-studio", 3, core.RuntimeClaude)
	drainingMini := mini
	drainingMini.Draining = true
	codexOnly := cand("n3", "codexbox", 2, core.RuntimeCodex)
	studioOff := studio
	studioOff.Online = false
	drainingLocal := local
	drainingLocal.Draining = true

	work := core.Ticket{ID: "T-1", Execution: &core.Execution{Runtime: core.RuntimeClaude}}
	pinned := core.Ticket{ID: "T-2", Execution: &core.Execution{Runtime: core.RuntimeClaude, Node: "n2"}}
	dir := core.Ticket{ID: "T-3", Execution: &core.Execution{Runtime: core.RuntimeClaude, Directory: "/src/x"}}
	auto := core.Ticket{ID: "T-4", Execution: &core.Execution{Runtime: core.RuntimeAuto}}
	warm := core.Ticket{ID: "T-5", Execution: &core.Execution{Runtime: core.RuntimeClaude},
		Assignments: []core.Assignment{{ID: "a0", State: core.AssignmentEnded, Node: "n2"}, {ID: "a1", State: core.AssignmentPending}}}
	pending := core.Assignment{ID: "a1", State: core.AssignmentPending}
	review := core.Ticket{ID: "T-7", Execution: &core.Execution{Runtime: core.RuntimeClaude},
		Review: &core.ReviewState{Runtime: core.RuntimeClaude}}
	reviewA := core.Assignment{ID: "r1", State: core.AssignmentPending, Role: core.RoleReview}
	both := cand("n5", "both", 2, core.RuntimeClaude, core.RuntimeCodex)

	cases := []struct {
		name     string
		t        core.Ticket
		a        core.Assignment
		nodes    []candidate
		want     string
		wantWait string
	}{
		{"lowest load", work, pending, []candidate{busyLocal, mini, studio}, "n1", ""},
		{"ties go to the local node", work, pending, []candidate{cand("n1", "mini", 0, core.RuntimeClaude), local}, "local", ""},
		{"then to the name", work, pending, []candidate{cand("n9", "zed", 0, core.RuntimeClaude), cand("n1", "abe", 0, core.RuntimeClaude)}, "n1", ""},
		{"the pin wins over the load", pinned, pending, []candidate{local, mini, studio}, "n2", ""},
		{"a pin that is offline waits", pinned, pending, []candidate{local, mini, studioOff}, "", "Waiting for mac-studio"},
		{"draining is excluded", work, pending, []candidate{busyLocal, drainingMini}, "local", ""},
		{"a moved Ticket whose pin was cleared goes elsewhere", core.Ticket{ID: "T-8", Execution: &core.Execution{Runtime: core.RuntimeClaude},
			Assignments: []core.Assignment{{ID: "a0", State: core.AssignmentEnded, Node: "n2"}, {ID: "a1", State: core.AssignmentPending, Avoid: []string{"n2"}}}},
			core.Assignment{ID: "a1", State: core.AssignmentPending, Avoid: []string{"n2"}}, []candidate{busyLocal, mini, studio}, "n1", ""},
		{"avoid is excluded", work, core.Assignment{ID: "a1", State: core.AssignmentPending, Avoid: []string{"n1"}},
			[]candidate{busyLocal, mini}, "local", ""},
		{"warmth beats the load", warm, pending, []candidate{local, mini, studio}, "n2", ""},
		{"a dir ticket goes only to its node", dir, pending, []candidate{busyLocal, mini}, "local", ""},
		{"a dir ticket waits for its node", dir, pending, []candidate{drainingLocal, mini}, "", "Waiting for server"},
		{"auto with only codex ready picks the codex node", auto, pending, []candidate{
			cand("local", "server", 0), cand("n1", "mini", 0), codexOnly}, "n3", ""},
		{"a review prefers a node with another runtime over the load", review, reviewA,
			[]candidate{local, both}, "n5", ""},
		{"a review takes the implementer's runtime when no other is ready", review, reviewA,
			[]candidate{local, mini}, "local", ""},
		{"a pinned runtime nobody has waits", core.Ticket{ID: "T-6", Execution: &core.Execution{Runtime: core.RuntimeCodex}},
			pending, []candidate{mini, cand("n4", "other", 0)}, "", "Waiting for a node with Codex"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, wait := choose(c.t, c.a, c.nodes)
			if got != c.want || wait != c.wantWait {
				t.Fatalf("choose = %q, %q; want %q, %q", got, wait, c.want, c.wantWait)
			}
		})
	}
}

// TestPinnedTicketWaits: a Ticket pinned to an offline node waits, saying for which, and is placed on it as soon as it
// connects.
func TestPinnedTicketWaits(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mac-studio")
	f := e.fleet(func(c *Config) { c.Engine = nil })
	eng := core.NewEngine(e.st.Repos(), f, nil, nil)
	f.SetEngine(eng)
	e.eng = eng

	tk, err := eng.SubmitOwnerTask("Fix the typo", core.TaskOptions{Node: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the wait reason", func() bool { return f.WaitReason(tk.ID) == "Waiting for mac-studio" })
	if got := e.ticket(tk.ID); got.LatestAssignment().Node != "" {
		a := got.LatestAssignment()
		t.Fatalf("placed on %q while its node is offline", a.Node)
	}

	h := hello(nodeID)
	h.Caps.Runtimes = []*athenav1.RuntimeCap{{Name: core.RuntimeClaude, Ready: true}}
	w := dialWire(t, f, nodeID, h)
	w.waitWelcome()
	welcomed := time.Now()
	eventually(t, "placed on the node", func() bool {
		got := e.ticket(tk.ID)
		return got.LatestAssignment().Node == nodeID
	})
	if d := time.Since(welcomed); d > 100*time.Millisecond {
		t.Errorf("placed %s after the node connected, want within 100ms", d)
	}
	if r := f.WaitReason(tk.ID); r != "" {
		t.Errorf("wait reason %q after placement", r)
	}
	deadline := time.After(waitTimeout)
	for {
		select {
		case c := <-w.calls:
			if c.GetMethod() == nodelink.ServeStart {
				return
			}
		case <-deadline:
			t.Fatal("the node got no Start")
		}
	}
}

func TestLocalName(t *testing.T) {
	for in, want := range map[string]string{
		"Daniels-MacBook-Pro.local": "daniels-macbook-pro",
		"build_01":                  "build-01",
		"42box":                     "box",
		"":                          "this-machine",
		"---":                       "this-machine",
		"a-very-long-host-name-that-goes-past-the-limit": "a-very-long-host-name-that-goes",
	} {
		if got := LocalName(in); got != want || !ValidName(got) {
			t.Errorf("LocalName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestWaitChangeSignalled: the fleet says when why a Ticket waits appears and when it goes, for the dashboard.
func TestWaitChangeSignalled(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mac-studio")
	f := e.fleet(func(c *Config) { c.Engine = nil })
	got := make(chan string, 8)
	f.OnWaitChange(func(id string) { got <- id })
	eng := core.NewEngine(e.st.Repos(), f, nil, nil)
	f.SetEngine(eng)
	e.eng = eng

	tk, err := eng.SubmitOwnerTask("Fix the typo", core.TaskOptions{Node: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-got:
		if id != tk.ID {
			t.Fatalf("signalled %s, want %s", id, tk.ID)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no signal when the Ticket began to wait")
	}
	h := hello(nodeID)
	h.Caps.Runtimes = []*athenav1.RuntimeCap{{Name: core.RuntimeClaude, Ready: true}}
	w := dialWire(t, f, nodeID, h)
	w.waitWelcome()
	select {
	case id := <-got:
		if id != tk.ID {
			t.Fatalf("signalled %s, want %s", id, tk.ID)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no signal when the Ticket stopped waiting")
	}
}
