package node

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// fakeToolPolicy is a Policy whose tool policy per trust class is the map's, standard for a class it lacks.
type fakeToolPolicy map[string]string

func (f fakeToolPolicy) ToolPolicy(class string) string {
	if v, ok := f[class]; ok {
		return v
	}
	return policy.ToolStandard
}
func (fakeToolPolicy) Allows(tool, rule, repo string) bool { return false }
func (fakeToolPolicy) Shadow(repo string) bool             { return false }

// restrictOwner makes the owner's own tasks restricted, so a harness's submit runs one.
var restrictOwner = fakeToolPolicy{core.TrustOwner: policy.ToolRestricted}

// countingEngine is the Engine with its Approval requests counted.
type countingEngine struct {
	EngineCallbacks
	requested atomic.Int32
}

func (e *countingEngine) RequestApproval(a core.Approval) (core.Approval, error) {
	e.requested.Add(1)
	return e.EngineCallbacks.RequestApproval(a)
}

func TestToolPolicyOfTicket(t *testing.T) {
	stranger := core.Ticket{Mandate: core.Mandate{Trust: core.TrustStranger}}
	owner := core.Ticket{Mandate: core.Mandate{Basis: core.MandateBasis{Via: core.ViaOwner}}}
	if got := New(Config{}, nil).toolPolicy(stranger); got != policy.ToolStandard {
		t.Errorf("no Policy: stranger = %q, want standard", got)
	}
	p, ok := policy.New(policy.Defaults(nil), nil, time.Minute).(Policy)
	if !ok {
		t.Fatal("the concrete Policy is not a node.Policy")
	}
	n := New(Config{Policy: p}, nil)
	if got := n.toolPolicy(stranger); got != policy.ToolRestricted {
		t.Errorf("stranger = %q, want restricted", got)
	}
	if got := n.toolPolicy(owner); got != policy.ToolStandard {
		t.Errorf("owner = %q, want standard", got)
	}
	// A tool policy the node does not know is restricted: it fails closed.
	odd := New(Config{Policy: fakeToolPolicy{core.TrustOwner: "lenient"}}, nil)
	if got := odd.toolPolicy(owner); got != policy.ToolRestricted {
		t.Errorf("unknown tool policy = %q, want restricted", got)
	}
}

func TestRestrictedSandbox(t *testing.T) {
	in := &rt.Sandbox{AllowWrite: []string{"/w"}, DenyWrite: []string{"/w/.athena/settings.json"}, DenyRead: []string{"/k.pem"},
		AllowedDomains: append(slices.Clone(GitHubDomains), "pypi.org"), UnixSockets: []string{"/n.sock"}, WeakerNetworkIsolation: true}
	before := *in
	before.AllowedDomains = slices.Clone(in.AllowedDomains)
	out := restrictedSandbox(in)
	if !out.Required || !slices.Equal(out.AllowedDomains, GitHubDomains) {
		t.Fatalf("restricted = %+v, want required with GitHub's domains alone", out)
	}
	if !reflect.DeepEqual(*in, before) {
		t.Fatalf("input mutated: %+v, was %+v", *in, before)
	}
	if out == in || &out.AllowedDomains[0] == &GitHubDomains[0] {
		t.Fatal("restrictedSandbox shares memory with its input or GitHubDomains")
	}
	if !slices.Equal(out.AllowWrite, in.AllowWrite) || !slices.Equal(out.DenyWrite, in.DenyWrite) || !slices.Equal(out.DenyRead, in.DenyRead) ||
		!slices.Equal(out.UnixSockets, in.UnixSockets) || !out.WeakerNetworkIsolation {
		t.Fatalf("restricted dropped more than the network: %+v", out)
	}
	if out := restrictedSandbox(nil); out == nil || !out.Required || !slices.Equal(out.AllowedDomains, GitHubDomains) {
		t.Fatalf("restricted of no sandbox = %+v, want a required one", out)
	}
}

// A restricted session's sandbox is required and reaches GitHub alone, whatever the owner configured; with no sandbox
// configured it still gets one.
func TestRestrictedSessionSandbox(t *testing.T) {
	if !SandboxSupported() {
		t.Skip("no sandbox on this platform")
	}
	for _, sbc := range []*SandboxConfig{{AllowedDomains: []string{"pypi.org"}, AllowWrite: []string{"/cache/go"}}, nil} {
		h := newHarness(t, func(c *Config) { c.Sandbox, c.Policy = sbc, restrictOwner })
		tk := h.submit("Stranger's change", newRepo(t))
		tk, _ = h.running(tk.ID)
		dir := TicketDir(h.cfg.WorkRoot, tk)
		settings := readSessionSettings(t, dir)
		sb := settings.Sandbox
		if sb == nil {
			t.Fatalf("config %+v: no sandbox block", sbc)
		}
		if !sb.Enabled || !sb.FailIfUnavailable || !slices.Equal(sb.Network.AllowedDomains, GitHubDomains) || !sb.Network.StrictAllowlist {
			t.Errorf("config %+v: sandbox = %+v", sbc, sb)
		}
		if !slices.Contains(sb.Network.AllowUnixSockets, h.cfg.SocketPath) && !sb.Network.AllowAllUnixSockets {
			t.Errorf("config %+v: the node socket is unreachable: %+v", sbc, sb.Network)
		}
		// The owner's caches are not its to write; its own are, each made at prepare and named in its env.
		cache := restrictedCacheDir(dir)
		if slices.Contains(sb.Filesystem.AllowWrite, "/cache/go") || !slices.Contains(sb.Filesystem.AllowWrite, cache) {
			t.Errorf("config %+v: allowWrite = %v, want its own cache %s and not the owner's", sbc, sb.Filesystem.AllowWrite, cache)
		}
		for env, sub := range map[string]string{"GOCACHE": "go-build", "GOMODCACHE": "gomod", "npm_config_cache": "npm",
			"PIP_CACHE_DIR": "pip", "XDG_CACHE_HOME": "xdg"} {
			want := filepath.Join(cache, sub)
			if settings.Env[env] != want {
				t.Errorf("config %+v: env %s = %q, want %q", sbc, env, settings.Env[env], want)
			}
			if st, err := os.Stat(want); err != nil || !st.IsDir() {
				t.Errorf("config %+v: cache %s not made: %v", sbc, want, err)
			}
		}
		if settings.Env["GOFLAGS"] != "-modcacherw" {
			t.Errorf("config %+v: GOFLAGS = %q", sbc, settings.Env["GOFLAGS"])
		}
	}
}

// With the owner's sandbox off, a restricted session's commands still cannot read the node's secrets or Athena's
// state directory, beyond what the session needs: its own directory and git directories, and the node socket.
func TestRestrictedSessionDeniesSecrets(t *testing.T) {
	if !SandboxSupported() {
		t.Skip("no sandbox on this platform")
	}
	key := filepath.Join(t.TempDir(), "app.pem")
	h := newHarness(t, func(c *Config) {
		c.Sandbox, c.Policy, c.Secrets = nil, restrictOwner, []string{key}
		c.WorkRoot = filepath.Join(c.StateDir, "work") // as the server places it
	})
	if err := os.MkdirAll(h.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(h.cfg.StateDir, "secret.db")
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tk := h.submit("Stranger's change", newRepo(t))
	tk, _ = h.running(tk.ID)
	dir := TicketDir(h.cfg.WorkRoot, tk)
	sb := readSessionSettings(t, dir).Sandbox
	if sb == nil {
		t.Fatal("no sandbox block")
	}
	deny := sb.Filesystem.DenyRead
	if !slices.Contains(deny, key) || !slices.Contains(deny, resolvedPath(db)) {
		t.Fatalf("denyRead = %v, want the key %s and the state dir's %s", deny, key, db)
	}
	home, _ := os.UserHomeDir()
	if !slices.Contains(deny, filepath.Join(home, ".ssh")) {
		t.Errorf("denyRead = %v lacks ~/.ssh", deny)
	}
	for _, d := range deny {
		for _, need := range []string{dir, h.cfg.SocketPath} {
			if r := resolvedPath(need); within(d, r) {
				t.Errorf("denyRead %s covers %s, which the session needs", d, need)
			}
		}
	}
	// A standard session on the same node has no sandbox at all, as before.
	if sb, _ := h.n.sessionSandbox(dir, false, false); sb != nil {
		t.Errorf("standard sandbox = %+v, want none", sb)
	}
}

func TestDenyAllBut(t *testing.T) {
	root := resolvedPath(t.TempDir())
	for _, d := range []string{"work/a", "work/b", "repos/o/r/.git", "repos/o/s", "keys"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "athena.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := denyAllBut(root, []string{filepath.Join(root, "work/a"), filepath.Join(root, "repos/o/r/.git"), "", "/elsewhere"})
	slices.Sort(got)
	want := []string{filepath.Join(root, "athena.db"), filepath.Join(root, "keys"), filepath.Join(root, "repos/o/s"), filepath.Join(root, "work/b")}
	if !slices.Equal(got, want) {
		t.Fatalf("denyAllBut = %v, want %v", got, want)
	}
	if got := denyAllBut(root, []string{"/elsewhere"}); !slices.Equal(got, []string{root}) {
		t.Fatalf("nothing kept = %v, want the root", got)
	}
}

// A restricted Ticket under auto runs on Claude Code even where Codex is preferred; one pinned to Codex fails without
// starting anything.
func TestRestrictedRunsOnClaudeOnly(t *testing.T) {
	h := newCodexHarness(t, codexTurn(), func(c *Config) {
		c.Policy = restrictOwner
		c.Chooser = HeadroomChooser{Preferred: core.RuntimeCodex}
	})
	auto, err := h.eng.SubmitOwnerTask("Auto stranger task", core.TaskOptions{Runtime: core.RuntimeAuto})
	if err != nil {
		t.Fatal(err)
	}
	_, a := h.running(auto.ID)
	if a.Runtime != core.RuntimeClaude {
		t.Fatalf("auto ran on %q, want claude", a.Runtime)
	}
	pinned := h.submitCodex("Pinned stranger task")
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(pinned.ID)
		r := tk.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	tk, _ := h.ticket(pinned.ID)
	if got := tk.LatestAssignment().Report.Summary; got != restrictedRuntimeSummary {
		t.Fatalf("summary = %q, want %q", got, restrictedRuntimeSummary)
	}
	if strings.Contains(strings.Join(h.rpc(), "\n"), "thread/start") {
		t.Fatal("a Codex session started")
	}
}

// On a machine that cannot sandbox, a restricted session does not start: its Assignment fails, saying why.
func TestRestrictedNeedsTheSandbox(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Policy = restrictOwner })
	h.n.canSandbox = func() bool { return false }
	tk := h.submit("Stranger's change", "")
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(tk.ID)
		r := tk.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	tk, _ = h.ticket(tk.ID)
	const want = "This Ticket came from someone outside your allow list, and it runs only inside the sandbox, which this machine cannot start: "
	if got := tk.LatestAssignment().Report.Summary; !strings.HasPrefix(got, want) || len(got) == len(want) {
		t.Fatalf("summary = %q, want %q and a cause", got, want)
	}
	if n := len(h.invocations()); n != 0 {
		t.Fatalf("the session started %d times", n)
	}
}

// A restricted Codex session's network and permission requests are declined without asking anyone.
// A restricted session never launches on Codex; should one run there anyway, its asks are still declined (defence in
// depth): the session here is made restricted by hand.
func TestRestrictedCodexNetworkDeclined(t *testing.T) {
	h := newCodexHarness(t, codexTurn(), nil)
	ce := &countingEngine{EngineCallbacks: h.eng}
	h.n.SetEngine(ce)
	tk := h.submitCodex("Stranger's change")
	var s *session
	h.eventually("live session", func() bool { s = h.n.liveSession(tk.ID); return s != nil })
	s.mu.Lock()
	s.toolPolicy = policy.ToolRestricted
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	for _, req := range []rt.ApprovalRequest{
		{Kind: "network", Host: "registry.npmjs.org", TurnID: "t1"},
		{Kind: "permissions", Host: "*", Paths: []string{"/opt/cache"}, Reason: "needs the cache", TurnID: "t1"},
		{Kind: "command", Command: "kubectl apply -f app.yaml", TurnID: "t2"},
		// What the rules allow, asked to run outside Codex's sandbox: with no node sandbox, still nobody is asked.
		{Kind: "command", Command: "ls -la", TurnID: "t3"},
	} {
		v := h.n.approveRequest(ctx, s, req)
		if v.Allow || v.Reason != restrictedEscalateReason {
			t.Errorf("%s = %+v, want declined with %q", req.Kind, v, restrictedEscalateReason)
		}
	}
	if n := ce.requested.Load(); n != 0 {
		t.Fatalf("%d approvals requested", n)
	}
	if pending, _ := h.eng.PendingApprovals(); len(pending) != 0 {
		t.Fatalf("pending approvals = %+v", pending)
	}
}

// A standard session's preparation is today's: the sandbox sandboxFor makes, the worker's MCP tools, and rules that
// still escalate.
func TestStandardUnchanged(t *testing.T) {
	if !SandboxSupported() {
		t.Skip("no sandbox on this platform")
	}
	for _, p := range []Policy{nil, fakeToolPolicy{}, fakeToolPolicy{core.TrustStranger: policy.ToolRestricted}} {
		rec := newRecordingRuntime(fakeClaude(t))
		h := newHarness(t, func(c *Config) {
			c.Runtimes = map[string]rt.Runtime{"claude": rec}
			c.Sandbox = &SandboxConfig{Required: true, AllowedDomains: []string{"pypi.org"}, AllowWrite: []string{"/cache/go"},
				DenyRead: []string{"/keys/app.pem"}, CAFile: "/etc/ca.pem", WeakerNetworkIsolation: true}
			c.Policy = p
		})
		tk := h.submit("Owner's change", newRepo(t))
		tk, a := h.running(tk.ID)
		wd, err := WorkingDir(h.cfg.WorkRoot, tk)
		if err != nil {
			t.Fatal(err)
		}
		sb, env := h.n.sandboxFor(wd)
		want := rt.Prepare{WorkDir: wd, AthenaBin: h.cfg.AthenaBin, Socket: h.cfg.SocketPath, TicketID: tk.ID, AssignmentID: a.ID,
			HookTimeout: h.cfg.ApprovalTimeout + hookTimeoutMargin, Sandbox: sb, SandboxEnv: env, Role: roleWorker}
		rec.mu.Lock()
		prepares, starts := slices.Clone(*rec.prepares), slices.Clone(*rec.starts)
		rec.mu.Unlock()
		if len(prepares) != 1 || len(starts) != 1 {
			t.Fatalf("policy %v: prepares %d, starts %d", p, len(prepares), len(starts))
		}
		if !reflect.DeepEqual(prepares[0], want) {
			t.Errorf("policy %v: prepare = %+v\nwant %+v", p, prepares[0], want)
		}
		if o := starts[0]; !reflect.DeepEqual(o.Sandbox, sb) || !slices.Equal(o.SandboxEnv, env) {
			t.Errorf("policy %v: start sandbox = %+v %v", p, o.Sandbox, o.SandboxEnv)
		}
		s := h.n.liveSession(tk.ID)
		if s == nil || s.restricted() {
			t.Fatalf("policy %v: session %+v, want a standard live one", p, s)
		}
		rc := h.n.ruleContext(s, core.RuntimeClaude)
		if rc.Restricted {
			t.Errorf("policy %v: rule context restricted", p)
		}
		if d := Evaluate(rc, "Bash", in(map[string]any{"command": "kubectl apply -f x.yaml"})); d.Verdict != VerdictEscalate {
			t.Errorf("policy %v: kubectl = %+v, want escalate", p, d)
		}
		if d := Evaluate(rc, "WebFetch", in(map[string]any{"url": "https://example.com"})); d.Verdict != VerdictAllow {
			t.Errorf("policy %v: WebFetch = %+v, want allow", p, d)
		}
		cs := mcpClient(t, tk.ID, a.ID, h.cfg.SocketPath)
		tools, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tl := range tools.Tools {
			names = append(names, tl.Name)
		}
		if slices.Sort(names); !slices.Equal(names, slices.Sorted(slices.Values(WorkerTools()))) {
			t.Errorf("policy %v: tools = %v, want %v", p, names, WorkerTools())
		}
	}
}
