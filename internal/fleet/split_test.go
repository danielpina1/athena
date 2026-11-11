package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// splitEnv is executorEnv whose Engine resolves child node hints through the Fleet (core.NodeDirectory), with a
// mirror.
func splitEnv(t *testing.T) (*env, *Fleet) {
	t.Helper()
	e := newEnv(t)
	f := e.fleet(func(c *Config) {
		c.Engine = nil
		c.Mirror = NewMirror(filepath.Join(t.TempDir(), "checkpoints"))
	})
	e.eng = core.NewEngineWith(e.st.Repos(), f, core.Options{Nodes: f})
	f.SetEngine(e.eng)
	return e, f
}

// running places a task on the repository on the node and starts its Assignment.
func (e *env) running(nodeID, repo string) core.Ticket {
	e.t.Helper()
	tk := e.placedRepo(nodeID, repo)
	if err := e.eng.AssignmentStarted(tk.ID, tk.LatestAssignment().ID, "s-"+tk.ID, "", core.RuntimeClaude); err != nil {
		e.t.Fatal(err)
	}
	return e.ticket(tk.ID)
}

// child spawns one child of lead from spec and waits for its Assignment to be placed on nodeID; with start, it is
// started there too.
func (e *env) child(lead string, spec core.ChildSpec, nodeID string, start bool) core.Ticket {
	e.t.Helper()
	if spec.Title == "" {
		spec.Title, spec.Instructions = "A part", "Do the part."
	}
	ids, err := e.eng.SpawnChildren(lead, []core.ChildSpec{spec})
	if err != nil {
		e.t.Fatal(err)
	}
	eventually(e.t, "the child placed on "+nodeID, func() bool {
		tk := e.ticket(ids[0])
		a := tk.LatestAssignment()
		return a != nil && a.Node == nodeID
	})
	if start {
		tk := e.ticket(ids[0])
		a := tk.LatestAssignment()
		if err := e.eng.AssignmentStarted(ids[0], a.ID, "s-"+ids[0], "", core.RuntimeClaude); err != nil {
			e.t.Fatal(err)
		}
	}
	return e.ticket(ids[0])
}

// ready files the child's ready report with its branch head.
func (e *env) ready(child core.Ticket, head string) {
	e.t.Helper()
	if err := e.eng.FileReport(child.ID, core.ReportInput{AssignmentID: child.LatestAssignment().ID, State: core.ReportReady,
		Summary: "done", Head: head}); err != nil {
		e.t.Fatal(err)
	}
	if got := e.ticket(child.ID); got.Ended == nil || got.Ended.Outcome != core.OutcomeCompleted {
		e.t.Fatalf("the child did not complete: %+v", got.Ended)
	}
}

// request sends a request as the node's link does and returns its answer.
func (w *wireNode) request(id uint64, method string, args any) *athenav1.NodeResult {
	w.t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_NodeCall{
		NodeCall: &athenav1.NodeCall{Id: id, Method: method, Args: b}}}); err != nil {
		w.t.Fatal(err)
	}
	for {
		r := w.result()
		if r.GetId() == id {
			return r
		}
	}
}

// argsOf are the args of every call of method the node got, in order.
func (a *answering) argsOf(method string) []json.RawMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []json.RawMessage
	for i, m := range a.methods {
		if m == method {
			out = append(out, a.args[i])
		}
	}
	return out
}

// delivered reports whether the node got a Deliver of the Ticket with an undelivered Input.
func (a *answering) delivered(ticketID string) bool {
	for _, raw := range a.argsOf(nodelink.ServeDeliver) {
		var d nodelink.ArgsDeliver
		if json.Unmarshal(raw, &d) == nil && d.Ticket.ID == ticketID && len(d.Ticket.Undelivered) > 0 {
			return true
		}
	}
	return false
}

// childWork makes, in a clone of base, a commit on the child's branch and its checkpoint on top; it returns the clone,
// the child's head and the checkpoint.
func childWork(t *testing.T, base, childID string) (repo, head, cp string) {
	t.Helper()
	repo = t.TempDir()
	gitT(t, repo, "clone", "-q", base, ".")
	gitT(t, repo, "checkout", "-q", "-b", "athena/"+strings.ToLower(childID))
	if err := os.WriteFile(filepath.Join(repo, "child.txt"), []byte("the child's part\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "child.txt")
	gitT(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "child work")
	head = gitT(t, repo, "rev-parse", "HEAD")
	cp = checkpointIn(t, repo, childID, "wip.txt", "after the report\n")
	return repo, head, cp
}

// TestRemoteHandOffSpawns: a worker on a remote node splits work off through the node link: the child is created
// under its lead. The same call naming a Ticket of another node is refused.
func TestRemoteHandOffSpawns(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	lead := e.placedRepo(nodeID, "o/r")
	elsewhere := e.placedRepo(otherID, "o/r")
	f := e.fleet(nil)
	cn := newClient(t, f, nodeID)
	cn.run()
	specs := []core.ChildSpec{{Title: "Write the docs", Instructions: "Document the flag."}}

	ids, err := cn.c.Engine().SpawnChildren(lead.ID, specs)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("ids = %v", ids)
	}
	if c := e.ticket(ids[0]); c.Lead != lead.ID || c.Title != "Write the docs" {
		t.Fatalf("child = %+v", c)
	}
	if got := e.ticket(lead.ID); !slices.Contains(got.Children, ids[0]) {
		t.Fatalf("lead's children = %v", got.Children)
	}

	if _, err := cn.c.Engine().SpawnChildren(elsewhere.ID, specs); err == nil || !strings.Contains(err.Error(), "is not on this node") {
		t.Fatalf("a hand_off for another node's ticket = %v", err)
	}
	if got := e.ticket(elsewhere.ID); len(got.Children) != 0 {
		t.Fatalf("another node's ticket got children: %v", got.Children)
	}
}

// TestRemoteAskAgentAndAnswer: a child on one node asks its lead on another; the question reaches the lead's node as
// a delivered Input, and the answer reaches the child's node the same way. agents is scoped to the caller's Ticket.
func TestRemoteAskAgentAndAnswer(t *testing.T) {
	e, f := splitEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	lead := e.running(nodeID, "o/r")
	n1 := dialAnswering(t, f, nodeID)
	n2 := dialAnswering(t, f, otherID)
	child := e.child(lead.ID, core.ChildSpec{Node: "studio"}, otherID, true)

	r := n2.request(1001, nodelink.MethodAskAgent, nodelink.ArgsAskAgent{FromID: child.ID, Target: lead.ID, Text: "Which file holds the flag?"})
	if r.GetError() != "" {
		t.Fatalf("askAgent = %s", r.GetError())
	}
	var msg core.AgentMessage
	if err := json.Unmarshal(r.GetValue(), &msg); err != nil || msg.ID == "" {
		t.Fatalf("askAgent value = %s (%v)", r.GetValue(), err)
	}
	eventually(t, "the question delivered on the lead's node", func() bool { return n1.delivered(lead.ID) })

	r = n1.request(1002, nodelink.MethodAnswerAgent, nodelink.ArgsAnswerAgent{FromID: lead.ID, MessageID: msg.ID, Text: "flags.go"})
	if r.GetError() != "" {
		t.Fatalf("answerAgent = %s", r.GetError())
	}
	eventually(t, "the answer delivered on the child's node", func() bool { return n2.delivered(child.ID) })

	r = n2.request(1003, nodelink.MethodAgents, nodelink.ArgsAgents{TicketID: child.ID})
	var views []core.AgentView
	if r.GetError() != "" || json.Unmarshal(r.GetValue(), &views) != nil || len(views) == 0 || views[0].ID != lead.ID {
		t.Fatalf("agents = %s %s", r.GetValue(), r.GetError())
	}
	// Each call is the calling Ticket's node's alone.
	for i, call := range []struct {
		method string
		args   any
	}{
		{nodelink.MethodAskAgent, nodelink.ArgsAskAgent{FromID: child.ID, Target: lead.ID, Text: "hi"}},
		{nodelink.MethodAnswerAgent, nodelink.ArgsAnswerAgent{FromID: child.ID, MessageID: msg.ID, Text: "hi"}},
		{nodelink.MethodWaitFor, nodelink.ArgsWaitFor{TicketID: child.ID, Target: lead.ID}},
		{nodelink.MethodAgents, nodelink.ArgsAgents{TicketID: child.ID}},
		{nodelink.MethodSpawnChildren, nodelink.ArgsSpawnChildren{LeadID: child.ID, Specs: []core.ChildSpec{{Title: "x", Instructions: "y"}}}},
	} {
		r := n1.request(uint64(1100+i), call.method, call.args)
		if !strings.Contains(r.GetError(), "is not on this node") {
			t.Errorf("%s from another node = %q", call.method, r.GetError())
		}
	}
}

// seedArgs are the args of the seed calls the node got.
func (a *answering) seedArgs() []nodelink.ArgsSeed {
	var out []nodelink.ArgsSeed
	for _, raw := range a.argsOf(nodelink.ServeSeed) {
		var s nodelink.ArgsSeed
		_ = json.Unmarshal(raw, &s)
		out = append(out, s)
	}
	return out
}

// calls are the node's commands after its first setPaused, runs of the same one (status reads, a burst of chunks)
// collapsed.
func calls(a *answering) []string {
	order, _ := a.received()
	var got []string
	for _, o := range order {
		if o != nodelink.ServeStatus && (len(got) == 0 || got[len(got)-1] != o) {
			got = append(got, o)
		}
	}
	return got
}

// TestForkSeedAcrossNodes: a hand_off child pinned to another node than its lead's is seeded there with a fork of the
// lead's checkpoint from the mirror, naming the lead's commit, before its Start; the bundle holds that commit, so the
// child's branch starts at the lead's head.
func TestForkSeedAcrossNodes(t *testing.T) {
	e, f := splitEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	lead := e.running(nodeID, "o/r")
	leadRepo := nodeRepo(t)
	gitT(t, leadRepo, "checkout", "-q", "-b", "athena/"+strings.ToLower(lead.ID))
	gitT(t, leadRepo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "lead work")
	head := gitT(t, leadRepo, "rev-parse", "HEAD")
	cp := checkpointIn(t, leadRepo, lead.ID, "wip.txt", "the lead's work in progress\n")
	if err := f.cfg.Mirror.Receive("o/r", lead.ID, bytes.NewReader(bundleOf(t, leadRepo, lead.ID, ""))); err != nil {
		t.Fatal(err)
	}
	if err := f.recordCheckpoint(Checkpoint{TicketID: lead.ID, Repo: "o/r", SHA: cp, Node: nodeID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	dialAnswering(t, f, nodeID)
	n2 := dialAnswering(t, f, otherID)
	child := e.child(lead.ID, core.ChildSpec{Node: "studio", Base: head}, otherID, false)

	eventually(t, "the child's start on studio", func() bool { return n2.got(nodelink.ServeStart) })
	if got := strings.Join(calls(n2), ","); got != "setPaused,blob:seed,seed,start" {
		t.Fatalf("calls = %s, want the fork seed's chunks, the seed, then the start", got)
	}
	seeds := n2.seedArgs()
	if len(seeds) != 1 {
		t.Fatalf("seeds = %+v", seeds)
	}
	sd := seeds[0]
	if sd.TicketID != child.ID || !sd.Fork || sd.Base != head || sd.SHA != cp || sd.From != "mini" || sd.Transfer == "" || sd.Unsent != "" {
		t.Fatalf("seed = %+v", sd)
	}
	// The bundle holds the lead's checkpoint, and the lead's head under it: a branch made at Base is the lead's head.
	_, data := n2.received()
	file := filepath.Join(t.TempDir(), "fork.bundle")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	gitT(t, other, "init", "-q")
	gitT(t, other, "fetch", "-q", file, "+"+node.CheckpointRef(lead.ID)+":refs/fork")
	gitT(t, other, "branch", "athena/"+strings.ToLower(child.ID), sd.Base)
	if got := gitT(t, other, "rev-parse", "athena/"+strings.ToLower(child.ID)); got != head {
		t.Fatalf("the child's branch = %s, want the lead's head %s", got, head)
	}
}

// TestForkSeedLate: when the lead's commit does not reach the mirror within shipWait, the child is seeded without a
// bundle, saying so, and starts.
func TestForkSeedLate(t *testing.T) {
	defer func(old time.Duration) { shipWait = old }(shipWait)
	shipWait = 100 * time.Millisecond
	e, f := splitEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	lead := e.running(nodeID, "o/r")
	dialAnswering(t, f, nodeID)
	n2 := dialAnswering(t, f, otherID)
	e.child(lead.ID, core.ChildSpec{Node: "studio", Base: strings.Repeat("a", 40)}, otherID, false)

	eventually(t, "the child's start on studio", func() bool { return n2.got(nodelink.ServeStart) })
	if got := strings.Join(calls(n2), ","); got != "setPaused,seed,start" {
		t.Fatalf("calls = %s, want the seed, then the start", got)
	}
	sd := n2.seedArgs()[0]
	if sd.Fork || sd.Transfer != "" || sd.Unsent != node.SeedForkLate || sd.From != "mini" {
		t.Fatalf("seed = %+v, want no bundle and %q", sd, node.SeedForkLate)
	}
}

// forwards records what the fleet forwards after shipping, through forwardHook.
type forwards struct {
	mu  sync.Mutex
	got []string // "<method> <ticket> <title>"
	at  []string // the imported branch's head on the lead's node when each was forwarded
}

func (fw *forwards) list() []string {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return append([]string(nil), fw.got...)
}

// TestShipWaitsForChildCheckpoint: a child on n2 reports ready before its last checkpoint reached the server; the
// Deliver of its lead's Input on n1 waits for it. Once the checkpoint lands in the mirror, the child's branch is
// shipped to n1 and imported there (athena/<child> at the reported head) before the Input is delivered.
func TestShipWaitsForChildCheckpoint(t *testing.T) {
	e, f := splitEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	lead := e.running(nodeID, "o/r")

	base := nodeRepo(t)
	n1State := t.TempDir()
	clone := node.CloneDir(n1State, "o/r")
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		t.Fatal(err)
	}
	gitT(t, filepath.Dir(clone), "clone", "-q", base, clone)
	// The kill switch keeps n1 from launching the lead (it has no way to reach GitHub here): only the import is real.
	f.SetPaused(true)
	cn := newClient(t, f, nodeID)
	n := node.New(node.Config{WorkRoot: filepath.Join(n1State, "work"), StateDir: n1State, Name: "mini",
		Credentials: cn.c.Credentials()}, cn.c.Engine())
	t.Cleanup(func() { _ = n.Close() })
	cn.c.Attach(n)
	cn.run()
	n2 := dialAnswering(t, f, otherID)
	child := e.child(lead.ID, core.ChildSpec{Node: "studio"}, otherID, true)
	childRepo, head, cp := childWork(t, base, child.ID)
	branch := "refs/heads/athena/" + strings.ToLower(child.ID)

	fw := &forwards{}
	forwardHook = func(nodeID string, deliver bool, tk core.Ticket) {
		at, _ := gitOut(clone, "rev-parse", "--verify", "--quiet", branch)
		fw.mu.Lock()
		defer fw.mu.Unlock()
		fw.got = append(fw.got, fmt.Sprintf("%v %s", deliver, tk.ID))
		fw.at = append(fw.at, at)
	}
	defer func() { forwardHook = nil }()

	e.ready(child, head) // the lead's Input: child ready
	time.Sleep(300 * time.Millisecond)
	if got := fw.list(); slices.Contains(got, "true "+lead.ID) {
		t.Fatalf("the lead's Input was delivered before the child's branch reached the server: %v", got)
	}

	// The child's node sends its checkpoint: its chunks, then the fact.
	n2.sendBundle("x1", child.ID, bundleOf(t, childRepo, child.ID, ""), 1<<20)
	n2.fact(1, 1, nodelink.MethodCheckpointSent, nodelink.ArgsCheckpointSent{TicketID: child.ID, Repo: "o/r", SHA: cp})
	eventually(t, "the lead's Input delivered", func() bool { return slices.Contains(fw.list(), "true "+lead.ID) })
	fw.mu.Lock()
	i := slices.Index(fw.got, "true "+lead.ID)
	at := fw.at[i]
	fw.mu.Unlock()
	if at != head {
		t.Fatalf("athena/%s on n1 was %q when the Input was delivered, want %s", strings.ToLower(child.ID), at, head)
	}
}

// gitOut runs git in dir and returns its trimmed output and error.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// TestShipKeepsDeliverOrder: two Delivers to a lead whose child's branch is still on its way reach the lead's node in
// call order, after the branch was shipped (its chunks, then importBranch); a later one ships nothing again.
func TestShipKeepsDeliverOrder(t *testing.T) {
	e, f := splitEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	lead := e.running(nodeID, "o/r")
	n1 := dialAnswering(t, f, nodeID)
	dialAnswering(t, f, otherID)
	child := e.child(lead.ID, core.ChildSpec{Node: "studio"}, otherID, true)
	childRepo, head, cp := childWork(t, nodeRepo(t), child.ID)
	e.ready(child, head)

	titles := func() []string {
		var out []string
		for _, raw := range n1.argsOf(nodelink.ServeDeliver) {
			var d nodelink.ArgsDeliver
			_ = json.Unmarshal(raw, &d)
			if d.Ticket.Title == "first" || d.Ticket.Title == "second" {
				out = append(out, d.Ticket.Title)
			}
		}
		return out
	}
	tk := e.ticket(lead.ID)
	a := *tk.LatestAssignment()
	first, second := tk, tk
	first.Title, second.Title = "first", "second"
	f.Deliver(first, a)
	f.Deliver(second, a)
	time.Sleep(200 * time.Millisecond)
	if len(titles()) != 0 || n1.got(nodelink.ServeImportBranch) {
		t.Fatalf("calls = %v before the child's branch reached the server", calls(n1))
	}

	if err := f.cfg.Mirror.Receive("o/r", child.ID, bytes.NewReader(bundleOf(t, childRepo, child.ID, ""))); err != nil {
		t.Fatal(err)
	}
	if err := f.recordCheckpoint(Checkpoint{TicketID: child.ID, Repo: "o/r", SHA: cp, Node: otherID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both Delivers", func() bool { return len(titles()) == 2 })
	if got := strings.Join(titles(), ","); got != "first,second" {
		t.Fatalf("delivers = %s, want first,second", got)
	}
	// Every Deliver since the child's ready report waited for the branch: none before the import.
	got := calls(n1)
	ib := slices.Index(got, nodelink.ServeImportBranch)
	if ib < 1 || got[ib-1] != "blob:branch" || slices.Contains(got[ib:], "blob:branch") {
		t.Fatalf("calls = %v, want the branch's chunks, then importBranch, once", got)
	}
	n1.mu.Lock()
	imported := false
	for i, m := range n1.methods {
		var d nodelink.ArgsDeliver
		switch {
		case m == nodelink.ServeImportBranch:
			imported = true
		case m == nodelink.ServeDeliver && json.Unmarshal(n1.args[i], &d) == nil && !imported &&
			slices.ContainsFunc(d.Ticket.Inputs, func(in core.Input) bool { return in.Kind == core.InputChild }):
			t.Errorf("the child's Input was delivered (%q) before its branch was imported", d.Ticket.Title)
		}
	}
	n1.mu.Unlock()
	var ia nodelink.ArgsImportBranch
	_ = json.Unmarshal(n1.argsOf(nodelink.ServeImportBranch)[0], &ia)
	if ia.TicketID != lead.ID || ia.Child != child.ID || ia.SHA != head || ia.Transfer == "" {
		t.Fatalf("importBranch = %+v", ia)
	}

	// Shipped once: another Deliver goes out inline, with nothing shipped again.
	f.Deliver(first, a)
	eventually(t, "the third Deliver", func() bool { return len(titles()) == 3 })
	if n := len(n1.argsOf(nodelink.ServeImportBranch)); n != 1 {
		t.Fatalf("%d imports, want 1", n)
	}
}

// TestNeeded: a Ticket needs the branches of its ended children and deps that completed with a head.
func TestNeeded(t *testing.T) {
	done := func(id, head string, outcome core.TicketOutcome) core.Ticket {
		return core.Ticket{ID: id, Ended: &core.Ended{Outcome: outcome},
			Assignments: []core.Assignment{{ID: "a", Report: &core.Report{State: core.ReportReady, Head: head}}}}
	}
	all := map[string]core.Ticket{
		"T-2": done("T-2", "aaa", core.OutcomeCompleted),
		"T-3": done("T-3", "", core.OutcomeCompleted),
		"T-4": done("T-4", "bbb", core.OutcomeStopped),
		"T-5": {ID: "T-5"},
		"T-6": done("T-6", "ccc", core.OutcomeCompleted),
	}
	get := func(id string) (core.Ticket, bool) { tk, ok := all[id]; return tk, ok }
	tk := core.Ticket{ID: "T-1", Children: []string{"T-2", "T-3", "T-4", "T-5", "T-9"}, Deps: []string{"T-6", "T-2"}}
	var ids []string
	for _, c := range needed(tk, get) {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "T-2,T-6" {
		t.Fatalf("needed = %v", ids)
	}
}

// TestNodeDirectory: a node hint resolves by id or name, case-free for names, the server's own node included; a
// revoked node does not resolve.
func TestNodeDirectory(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	e.addNode(otherID, "studio")
	f := e.fleet(func(c *Config) { c.Local = node.New(node.Config{WorkRoot: t.TempDir(), StateDir: t.TempDir()}, nil) })
	var _ core.NodeDirectory = f
	for in, want := range map[string]string{"mini": nodeID, "Studio": otherID, nodeID: nodeID, "server": "local", "local": "local"} {
		if id, ok := f.Resolve(in); !ok || id != want {
			t.Errorf("Resolve(%q) = %q %v, want %q", in, id, ok, want)
		}
	}
	if _, ok := f.Resolve("nowhere"); ok {
		t.Error("an unknown node resolved")
	}
	if err := f.Revoke(otherID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Resolve("studio"); ok {
		t.Error("a revoked node resolved")
	}
	if f.Name(nodeID) != "mini" || f.Name("local") != "server" {
		t.Errorf("names = %q %q", f.Name(nodeID), f.Name("local"))
	}
}

// TestMirrorHas: Has looks a commit up in the repository's mirror.
func TestMirrorHas(t *testing.T) {
	m := NewMirror(filepath.Join(t.TempDir(), "checkpoints"))
	if ok, err := m.Has("o/r", strings.Repeat("a", 40)); ok || err != nil {
		t.Fatalf("Has in no mirror = %v %v", ok, err)
	}
	repo := nodeRepo(t)
	parent := gitT(t, repo, "rev-parse", "HEAD")
	sha := checkpointIn(t, repo, "T-1", "a.txt", "x\n")
	if err := m.Receive("o/r", "T-1", bytes.NewReader(bundleOf(t, repo, "T-1", ""))); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{sha, parent} {
		if ok, err := m.Has("o/r", c); !ok || err != nil {
			t.Errorf("Has(%s) = %v %v", c, ok, err)
		}
	}
	if ok, err := m.Has("o/r", strings.Repeat("b", 40)); ok || err != nil {
		t.Errorf("Has(missing) = %v %v", ok, err)
	}
	if _, err := m.Has("o/r", "--upload-pack=x"); err == nil {
		t.Error("Has took an option for a commit")
	}
}

// TestSweepKeepsChildOfOpenLead: an ended child's checkpoint stays in the mirror while its lead is open, however long
// ago the child ended; once the lead ended long enough ago, both go.
func TestSweepKeepsChildOfOpenLead(t *testing.T) {
	e := newEnv(t)
	e.addNode(nodeID, "mini")
	clk := &clock{}
	clk.set(time.Now())
	m := NewMirror(filepath.Join(t.TempDir(), "checkpoints"))
	f := e.fleet(func(c *Config) { c.Mirror = m; c.Now = clk.now })
	lead := e.placedRepo(nodeID, "o/r")
	ids, err := e.eng.SpawnChildren(lead.ID, []core.ChildSpec{{Title: "Part", Instructions: "Do it."}})
	if err != nil {
		t.Fatal(err)
	}
	repo := nodeRepo(t)
	sha := checkpointIn(t, repo, ids[0], "a.txt", "x\n")
	if err := m.Receive("o/r", ids[0], bytes.NewReader(bundleOf(t, repo, ids[0], ""))); err != nil {
		t.Fatal(err)
	}
	if err := f.recordCheckpoint(Checkpoint{TicketID: ids[0], Repo: "o/r", SHA: sha, Node: nodeID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.StopTicket(ids[0], "not needed"); err != nil {
		t.Fatal(err)
	}
	clk.set(time.Now().Add(2 * MirrorKeep))
	if err := f.SweepMirror(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.Head("o/r", ids[0]); !ok {
		t.Fatal("the child's checkpoint was swept while its lead is open")
	}
	if err := e.eng.StopTicket(lead.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := f.SweepMirror(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.Head("o/r", ids[0]); ok {
		t.Fatal("the child's checkpoint outlived its ended lead")
	}
}
