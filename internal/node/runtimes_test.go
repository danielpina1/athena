package node

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/quota"
	"github.com/danielpina1/athena/internal/runtime/claude"
	"github.com/danielpina1/athena/internal/runtime/codex"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// recordingRuntime is Claude Code that keeps what the node asked of it.
type recordingRuntime struct {
	claude.Runtime
	mu       *sync.Mutex
	prepares *[]rt.Prepare
	starts   *[]rt.StartOptions
}

func newRecordingRuntime(bin string) recordingRuntime {
	return recordingRuntime{Runtime: claude.Runtime{Binary: bin}, mu: &sync.Mutex{}, prepares: &[]rt.Prepare{}, starts: &[]rt.StartOptions{}}
}

func (r recordingRuntime) Prepare(p rt.Prepare) error {
	r.mu.Lock()
	*r.prepares = append(*r.prepares, p)
	r.mu.Unlock()
	return r.Runtime.Prepare(p)
}

func (r recordingRuntime) Start(ctx context.Context, o rt.StartOptions) (rt.Session, error) {
	r.mu.Lock()
	*r.starts = append(*r.starts, o)
	r.mu.Unlock()
	return r.Runtime.Start(ctx, o)
}

// The node runs a Ticket on the runtime its Config names, and gives it the session's files, ids and writable roots.
func TestSessionRunsOnConfiguredRuntime(t *testing.T) {
	rec := newRecordingRuntime(fakeClaude(t))
	h := newHarness(t, func(c *Config) { c.Runtimes = map[string]rt.Runtime{"claude": rec} })
	repo := newRepo(t)
	tk := h.submit("Use the runtime", repo)
	tk, a := h.running(tk.ID)
	if a.SessionID != fixtureSessionID {
		t.Fatalf("SessionID = %q", a.SessionID)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(*rec.prepares) != 1 || len(*rec.starts) != 1 {
		t.Fatalf("prepares %d, starts %d; want 1 each", len(*rec.prepares), len(*rec.starts))
	}
	p, o := (*rec.prepares)[0], (*rec.starts)[0]
	wd, err := WorkingDir(h.cfg.WorkRoot, tk)
	if err != nil {
		t.Fatal(err)
	}
	if p.WorkDir != o.Dir || p.TicketID != tk.ID || p.AssignmentID != a.ID || p.Socket != h.cfg.SocketPath ||
		p.AthenaBin != h.cfg.AthenaBin || p.HookTimeout != h.cfg.ApprovalTimeout+hookTimeoutMargin || p.Role != "worker" {
		t.Errorf("prepare = %+v", p)
	}
	if o.TicketID != tk.ID || o.AssignmentID != a.ID || o.Role != "worker" || o.Transcript == nil || o.Model != "sonnet" || o.ReadOnly {
		t.Errorf("start = %+v", o)
	}
	// A worktree's commits land in its git directory and the repository's objects, refs and logs; its config and hooks
	// are none of them.
	common := filepath.Join(repo, ".git")
	want := []string{filepath.Join(common, "worktrees", filepath.Base(wd)), filepath.Join(common, "objects"), filepath.Join(common, "refs"), filepath.Join(common, "logs")}
	if len(o.WritableRoots) != len(want) {
		t.Fatalf("writable roots = %v, want %v (work dir %s)", o.WritableRoots, want, wd)
	}
	for i := range want {
		if !sameFile(o.WritableRoots[i], want[i]) {
			t.Errorf("writable root %d = %s, want %s", i, o.WritableRoots[i], want[i])
		}
	}
}

// The owner's sandbox write paths are writable roots too, less any that holds a protected path or lies in one.
func TestWritableRootsAddSandboxAllowWrite(t *testing.T) {
	repo := newRepo(t)
	cache := filepath.Join(t.TempDir(), "go-build")
	rec := newRecordingRuntime(fakeClaude(t))
	h := newHarness(t, func(c *Config) {
		c.Runtimes = map[string]rt.Runtime{"claude": rec}
		c.Sandbox = &SandboxConfig{AllowWrite: []string{cache, repo, filepath.Join(repo, ".git", "hooks", "x"), filepath.Dir(repo)}}
	})
	tk := h.submit("Use the cache", repo)
	h.running(tk.ID)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	roots := (*rec.starts)[0].WritableRoots
	if len(roots) != 5 || roots[4] != cache {
		t.Fatalf("writable roots = %v, want the four git roots and %s only", roots, cache)
	}
}

// A Ticket whose runtime the node does not have fails with why; no other runtime runs in its place.
func TestUnavailableRuntimeFailsWithWhy(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Runtimes = map[string]rt.Runtime{"other": claude.Runtime{Binary: c.ClaudeBinary}} })
	tk := h.submit("No claude here", "")
	var r *core.Report
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(tk.ID)
		r = tk.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	if !strings.Contains(r.Summary, `runtime "claude" is not available on this node`) {
		t.Fatalf("summary = %q", r.Summary)
	}
	if n := len(h.invocations()); n != 0 {
		t.Fatalf("claude started %d times", n)
	}
}

func TestDefaultRuntimesAreClaudeCode(t *testing.T) {
	rs := DefaultRuntimes(Config{ClaudeBinary: "/opt/claude"})
	if r, ok := rs["claude"].(claude.Runtime); len(rs) != 1 || !ok || r.Binary != "/opt/claude" || r.Name() != "claude" {
		t.Fatalf("default runtimes = %#v", rs)
	}
}

func sameFile(a, b string) bool {
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ea == eb
}

// --- the runtime chooser ---

var chooseNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

var (
	readyInfo      = rt.Info{Installed: true, Ready: true, Tested: true}
	loggedOut      = rt.Info{Installed: true, Why: "codex is not logged in"}
	defaultChooser = HeadroomChooser{Preferred: core.RuntimeClaude, AvoidAbove: map[string]float64{"5h": 0.85, "7d": 0.90}}
)

// ranAssignment is an ended Assignment of role that ran on runtime.
func ranAssignment(id, runtime, role string) core.Assignment {
	started := chooseNow.Add(-time.Hour)
	return core.Assignment{ID: id, State: core.AssignmentEnded, StartedAt: &started, SessionID: "sess-" + id, Runtime: runtime, Role: role}
}

// choiceFor is the input of a launch of the Ticket's latest Assignment, with every runtime ready and unmeasured unless
// ready and quota say otherwise.
func choiceFor(runtime string, as []core.Assignment, ready map[string]rt.Info, qs map[string]quota.State) ChoiceInput {
	if ready == nil {
		ready = map[string]rt.Info{core.RuntimeClaude: readyInfo, core.RuntimeCodex: readyInfo}
	}
	tk := core.Ticket{ID: "T-0001", Execution: &core.Execution{Runtime: runtime}, Assignments: as}
	return ChoiceInput{Ticket: tk, Assignment: as[len(as)-1], Ready: ready, Quota: func(name string) quota.State { return qs[name] },
		Live: map[string]int{}, Now: chooseNow}
}

func limitedFor(d time.Duration) quota.State {
	until := chooseNow.Add(d)
	return quota.State{Measured: true, LimitedUntil: &until, Windows: []quota.Window{{Name: "5h", Used: 1, ResetsAt: &until, Limited: true}}}
}

func used(windows map[string]float64) quota.State {
	reset := chooseNow.Add(time.Hour)
	s := quota.State{Measured: true}
	for _, name := range []string{"5h", "7d"} {
		if u, ok := windows[name]; ok {
			s.Windows = append(s.Windows, quota.Window{Bucket: "b", Name: name, Used: u, ResetsAt: &reset})
		}
	}
	return s
}

func wantLaunch(t *testing.T, c Choice, runtime string) {
	t.Helper()
	if c.Runtime != runtime || !c.Until.IsZero() || c.AwaitSession {
		t.Fatalf("choice = %+v, want a launch on %s", c, runtime)
	}
}

// auto keeps the runtime of the Ticket's latest Assignment of the same role that ran (a warm session), over the
// preferred one.
func TestAutoPrefersWarmRuntime(t *testing.T) {
	pending := core.Assignment{ID: "a3", State: core.AssignmentPending}
	in := choiceFor(core.RuntimeAuto, []core.Assignment{ranAssignment("a1", core.RuntimeCodex, ""), pending}, nil, nil)
	wantLaunch(t, defaultChooser.Choose(in), core.RuntimeCodex)

	// A review that ran on claude since is not the worker's session.
	in = choiceFor(core.RuntimeAuto, []core.Assignment{ranAssignment("a1", core.RuntimeCodex, ""),
		ranAssignment("a2", core.RuntimeClaude, core.RoleReview), pending}, nil, nil)
	wantLaunch(t, defaultChooser.Choose(in), core.RuntimeCodex)

	// Nothing ran yet: the preferred runtime.
	in = choiceFor(core.RuntimeAuto, []core.Assignment{pending}, nil, nil)
	wantLaunch(t, defaultChooser.Choose(in), core.RuntimeClaude)

	// An Assignment from before runtimes were recorded ran on the Ticket's runtime: claude for auto.
	legacy := ranAssignment("a1", "", "")
	in = choiceFor(core.RuntimeAuto, []core.Assignment{legacy, pending}, nil, nil)
	if c := (HeadroomChooser{Preferred: core.RuntimeCodex}).Choose(in); c.Runtime != core.RuntimeClaude {
		t.Fatalf("legacy warm = %+v, want claude", c)
	}
}

// auto never picks a limited runtime, and when every ready one is limited it waits for the earliest reset.
func TestAutoSkipsLimited(t *testing.T) {
	as := []core.Assignment{ranAssignment("a1", core.RuntimeClaude, ""), {ID: "a2", State: core.AssignmentPending}}
	in := choiceFor(core.RuntimeAuto, as, nil, map[string]quota.State{core.RuntimeClaude: limitedFor(time.Hour)})
	wantLaunch(t, defaultChooser.Choose(in), core.RuntimeCodex)

	in = choiceFor(core.RuntimeAuto, as, nil, map[string]quota.State{
		core.RuntimeClaude: limitedFor(2 * time.Hour), core.RuntimeCodex: limitedFor(40 * time.Minute)})
	c := defaultChooser.Choose(in)
	if c.Runtime != "" || !c.Until.Equal(chooseNow.Add(40*time.Minute)) || c.Why == "" || c.AwaitSession {
		t.Fatalf("both limited: %+v, want a wait until the earliest reset", c)
	}
}

// auto drops a runtime over an avoidAbove threshold of any of its windows, unless that leaves none; among what is
// left, past the warm and the preferred runtime, the largest headroom wins.
func TestAutoAvoidsAboveThresholdUnlessAll(t *testing.T) {
	as := []core.Assignment{{ID: "a1", State: core.AssignmentPending}}
	in := choiceFor(core.RuntimeAuto, as, nil, map[string]quota.State{
		core.RuntimeClaude: used(map[string]float64{"5h": 0.86, "7d": 0.2}), core.RuntimeCodex: used(map[string]float64{"5h": 0.5})})
	wantLaunch(t, defaultChooser.Choose(in), core.RuntimeCodex)

	in = choiceFor(core.RuntimeAuto, as, nil, map[string]quota.State{
		core.RuntimeClaude: used(map[string]float64{"5h": 0.1, "7d": 0.95}), core.RuntimeCodex: used(map[string]float64{"5h": 0.5})})
	wantLaunch(t, defaultChooser.Choose(in), core.RuntimeCodex)

	// At the threshold is not over it.
	in = choiceFor(core.RuntimeAuto, as, nil, map[string]quota.State{core.RuntimeClaude: used(map[string]float64{"5h": 0.85})})
	wantLaunch(t, defaultChooser.Choose(in), core.RuntimeClaude)

	// Both over: neither is dropped, and the preferred one runs.
	in = choiceFor(core.RuntimeAuto, as, nil, map[string]quota.State{
		core.RuntimeClaude: used(map[string]float64{"5h": 0.97}), core.RuntimeCodex: used(map[string]float64{"7d": 0.92})})
	wantLaunch(t, defaultChooser.Choose(in), core.RuntimeClaude)

	// No warm or preferred candidate: the largest headroom.
	ready := map[string]rt.Info{core.RuntimeClaude: readyInfo, core.RuntimeCodex: readyInfo, "third": readyInfo}
	in = choiceFor(core.RuntimeAuto, as, ready, map[string]quota.State{
		core.RuntimeClaude: used(map[string]float64{"5h": 0.6}), core.RuntimeCodex: used(map[string]float64{"5h": 0.3}),
		"third": used(map[string]float64{"5h": 0.4})})
	wantLaunch(t, HeadroomChooser{}.Choose(in), core.RuntimeCodex)
}

// auto never picks a runtime that is not ready (logged out, hooks not Athena's), even when it is warm or preferred;
// with none ready the launch fails with every runtime's why.
func TestAutoSkipsUnreadyRuntime(t *testing.T) {
	as := []core.Assignment{ranAssignment("a1", core.RuntimeCodex, ""), {ID: "a2", State: core.AssignmentPending}}
	ready := map[string]rt.Info{core.RuntimeClaude: readyInfo, core.RuntimeCodex: loggedOut}
	in := choiceFor(core.RuntimeAuto, as, ready, nil)
	wantLaunch(t, HeadroomChooser{Preferred: core.RuntimeCodex}.Choose(in), core.RuntimeClaude)

	// The ready runtime is limited: wait for it, never run the unready one.
	in = choiceFor(core.RuntimeAuto, as, ready, map[string]quota.State{core.RuntimeClaude: limitedFor(time.Hour)})
	if c := defaultChooser.Choose(in); c.Runtime != "" || !c.Until.Equal(chooseNow.Add(time.Hour)) {
		t.Fatalf("only the unready one free: %+v, want a wait for claude", c)
	}

	ready = map[string]rt.Info{core.RuntimeClaude: {Why: "claude --version: not found"}, core.RuntimeCodex: loggedOut}
	c := defaultChooser.Choose(choiceFor(core.RuntimeAuto, as, ready, nil))
	if c.Runtime != "" || !c.Until.IsZero() || c.AwaitSession ||
		c.Why != "no runtime is ready: claude: claude --version: not found; codex: codex is not logged in" {
		t.Fatalf("none ready: %+v", c)
	}
}

// A Ticket pinned to a runtime, or an Assignment pinned to one, that is not ready fails with its why: no other runtime
// runs in its place.
func TestPinnedUnreadyRuntimeFailsWithWhy(t *testing.T) {
	ready := map[string]rt.Info{core.RuntimeClaude: readyInfo, core.RuntimeCodex: loggedOut}
	c := defaultChooser.Choose(choiceFor(core.RuntimeCodex, []core.Assignment{{ID: "a1"}}, ready, nil))
	if c.Runtime != "" || !c.Until.IsZero() || c.Why != "codex cannot run this ticket: codex is not logged in" {
		t.Fatalf("pinned ticket: %+v", c)
	}
	c = defaultChooser.Choose(choiceFor(core.RuntimeAuto, []core.Assignment{{ID: "a1", Runtime: core.RuntimeCodex}}, ready, nil))
	if c.Runtime != "" || !strings.Contains(c.Why, "codex is not logged in") {
		t.Fatalf("pinned assignment: %+v", c)
	}
	// A runtime the node does not have.
	c = defaultChooser.Choose(choiceFor(core.RuntimeCodex, []core.Assignment{{ID: "a1"}}, map[string]rt.Info{core.RuntimeClaude: readyInfo}, nil))
	if c.Runtime != "" || c.Why != `runtime "codex" is not available on this node` {
		t.Fatalf("missing runtime: %+v", c)
	}
	// A Ticket that names no runtime (from before runtimes) runs on claude.
	tk := choiceFor("", []core.Assignment{{ID: "a1"}}, nil, nil)
	tk.Ticket.Execution = nil
	wantLaunch(t, HeadroomChooser{Preferred: core.RuntimeCodex}.Choose(tk), core.RuntimeClaude)
}

// A pinned runtime that is limited is waited for, until its reset; the other runtime does not take over.
func TestPinnedLimitedWaits(t *testing.T) {
	q := map[string]quota.State{core.RuntimeClaude: limitedFor(90 * time.Minute)}
	c := defaultChooser.Choose(choiceFor(core.RuntimeClaude, []core.Assignment{{ID: "a1"}}, nil, q))
	if c.Runtime != core.RuntimeClaude || !c.Until.Equal(chooseNow.Add(90*time.Minute)) || !strings.Contains(c.Why, "claude") {
		t.Fatalf("pinned limited: %+v, want a wait for claude's reset", c)
	}
	// A resumed Assignment keeps its session's runtime: limited, it waits too.
	c = defaultChooser.Choose(choiceFor(core.RuntimeAuto, []core.Assignment{ranAssignment("a1", core.RuntimeClaude, "")}, nil, q))
	if c.Until.IsZero() {
		t.Fatalf("resumed limited: %+v, want a wait", c)
	}
}

// A review runs on another runtime than the implementer's when one is free; else on the implementer's.
func TestReviewPrefersOtherRuntime(t *testing.T) {
	review := core.Assignment{ID: "a2", State: core.AssignmentPending, Role: core.RoleReview}
	as := []core.Assignment{ranAssignment("a1", core.RuntimeClaude, ""), review}
	wantLaunch(t, defaultChooser.Choose(choiceFor(core.RuntimeAuto, as, nil, nil)), core.RuntimeCodex)

	as = []core.Assignment{ranAssignment("a1", core.RuntimeCodex, ""), review}
	wantLaunch(t, HeadroomChooser{Preferred: core.RuntimeCodex}.Choose(choiceFor(core.RuntimeAuto, as, nil, nil)), core.RuntimeClaude)

	// The other one is not ready: the implementer's runtime reviews.
	as = []core.Assignment{ranAssignment("a1", core.RuntimeClaude, ""), review}
	ready := map[string]rt.Info{core.RuntimeClaude: readyInfo, core.RuntimeCodex: loggedOut}
	wantLaunch(t, defaultChooser.Choose(choiceFor(core.RuntimeAuto, as, ready, nil)), core.RuntimeClaude)

	// A Ticket pinned to a runtime pins its implementer, not its review.
	as = []core.Assignment{ranAssignment("a1", core.RuntimeClaude, ""), review}
	wantLaunch(t, defaultChooser.Choose(choiceFor(core.RuntimeClaude, as, nil, nil)), core.RuntimeCodex)
}

// A runtime at its session cap is not chosen; with every candidate at its cap, the launch waits for a session to end
// rather than for a timer.
func TestMaxSessionsPerRuntime(t *testing.T) {
	ch := HeadroomChooser{Preferred: core.RuntimeClaude, MaxSessions: map[string]int{core.RuntimeClaude: 2, core.RuntimeCodex: 1}}
	as := []core.Assignment{{ID: "a1", State: core.AssignmentPending}}
	in := choiceFor(core.RuntimeAuto, as, nil, nil)
	in.Live = map[string]int{core.RuntimeClaude: 2}
	wantLaunch(t, ch.Choose(in), core.RuntimeCodex)

	in.Live = map[string]int{core.RuntimeClaude: 2, core.RuntimeCodex: 1}
	c := ch.Choose(in)
	if c.Runtime != "" || !c.Until.IsZero() || !c.AwaitSession || c.Why == "" {
		t.Fatalf("all at their cap: %+v, want a wait for a session to end", c)
	}
	// A cap of 0 is no cap.
	in.Live = map[string]int{core.RuntimeClaude: 9}
	wantLaunch(t, HeadroomChooser{Preferred: core.RuntimeClaude}.Choose(in), core.RuntimeClaude)

	// One at its cap, the other limited: whichever frees first, a session end or the reset.
	in = choiceFor(core.RuntimeAuto, as, nil, map[string]quota.State{core.RuntimeCodex: limitedFor(time.Hour)})
	in.Live = map[string]int{core.RuntimeClaude: 2}
	c = ch.Choose(in)
	if c.Runtime != "" || !c.AwaitSession || !c.Until.Equal(chooseNow.Add(time.Hour)) {
		t.Fatalf("capped and limited: %+v", c)
	}

	// A pinned runtime at its cap waits for a session to end too.
	in = choiceFor(core.RuntimeCodex, as, nil, nil)
	in.Live = map[string]int{core.RuntimeCodex: 1}
	if c := ch.Choose(in); c.Runtime != core.RuntimeCodex || !c.AwaitSession || !c.Until.IsZero() {
		t.Fatalf("pinned at its cap: %+v", c)
	}
}

// fixedChooser always chooses its runtime.
type fixedChooser string

func (f fixedChooser) Choose(ChoiceInput) Choice { return Choice{Runtime: string(f)} }

// The node never launches on a runtime the readiness it gathered says is not ready, whatever a Chooser answers: the
// launch fails with that runtime's why.
func TestNodeRefusesAnUnreadyChoice(t *testing.T) {
	t.Setenv("ATHENA_FAKE_CODEX_LOGGED_OUT", "1")
	h := newCodexHarness(t, codexTurn(), func(c *Config) { c.Chooser = fixedChooser(core.RuntimeCodex) })
	tk, err := h.eng.SubmitOwnerTask("Auto, chosen badly", core.TaskOptions{Runtime: core.RuntimeAuto})
	if err != nil {
		t.Fatal(err)
	}
	h.eventually("the launch failed", func() bool {
		cur, _ := h.ticket(tk.ID)
		r := cur.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	cur, _ := h.ticket(tk.ID)
	if s := cur.LatestAssignment().Report.Summary; !strings.Contains(s, "codex cannot run this ticket: "+codex.NotLoggedIn) {
		t.Fatalf("summary = %q", s)
	}
	if h.rpcHas(`"method":"thread/start"`) {
		t.Fatal("a session started on a Codex that is not ready")
	}
}

// slowCheck is a runtime whose check answers only once its context ends.
type slowCheck struct{ claude.Runtime }

func (slowCheck) Name() string { return "slow" }
func (slowCheck) Check(ctx context.Context) rt.Info {
	<-ctx.Done()
	return rt.Info{Why: "check: " + ctx.Err().Error()}
}

// A check cut short by a stop is no answer: Status keeps what it knew before.
func TestCancelledCheckIsNotKept(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Runtimes = map[string]rt.Runtime{core.RuntimeClaude: claude.Runtime{Binary: c.ClaudeBinary}, "slow": slowCheck{}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.n.checkRuntime(ctx, "slow", slowCheck{})
	for _, r := range h.n.Status().Runtimes {
		if r.Name == "slow" && r.Why != "not checked yet" {
			t.Fatalf("status = %+v, want the cancelled check left out", r)
		}
	}
}
