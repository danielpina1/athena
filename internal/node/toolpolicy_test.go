package node

import (
	"context"
	"fmt"
	"log/slog"
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
func (fakeToolPolicy) Trust(core.Trigger) string           { return core.TrustOwner }

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

// With the owner's sandbox off, a restricted session's commands still cannot read the node's secrets (a key outside
// the state directory, in its configured and its resolved spelling), the owner's credentials, the home directory, or
// Athena's state directory and work root as wholes: a Ticket directory made later is denied too. What the session
// needs is read again (allowRead): its own directory, the node socket. Nothing read again holds a secret or credential.
func TestRestrictedSessionDeniesSecrets(t *testing.T) {
	if !SandboxSupported() {
		t.Skip("no sandbox on this platform")
	}
	keys := t.TempDir()
	if err := os.WriteFile(filepath.Join(keys, "app.pem"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	keysLink := filepath.Join(t.TempDir(), "keys")
	if err := os.Symlink(keys, keysLink); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(keysLink, "app.pem") // configured through a link: its real path is denied too
	h := newHarness(t, func(c *Config) {
		c.Sandbox, c.Policy, c.Secrets = nil, restrictOwner, []string{key}
		c.WorkRoot = filepath.Join(c.StateDir, "work") // as the server places it
	})
	if err := os.MkdirAll(h.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tk := h.submit("Stranger's change", newRepo(t))
	tk, _ = h.running(tk.ID)
	dir := TicketDir(h.cfg.WorkRoot, tk)
	settings := readSessionSettings(t, dir)
	sb := settings.Sandbox
	if sb == nil {
		t.Fatal("no sandbox block")
	}
	deny, allow := sb.Filesystem.DenyRead, sb.Filesystem.AllowRead
	home, _ := os.UserHomeDir()
	covered := func(list []string, p string) bool {
		return slices.ContainsFunc(list, func(d string) bool { return within(d, p) || within(d, resolvedPath(p)) })
	}
	for _, want := range []string{key, resolvedPath(key), home, filepath.Join(home, ".ssh"), filepath.Join(home, ".git-credentials"),
		filepath.Join(home, ".config", "git", "credentials"), h.cfg.StateDir, h.cfg.WorkRoot} {
		if !slices.Contains(deny, want) && !slices.Contains(deny, resolvedPath(want)) {
			t.Errorf("denyRead lacks %s: %v", want, deny)
		}
	}
	// Whole directories, not a snapshot of their entries: a sibling Ticket made after the session started is denied.
	later := filepath.Join(h.cfg.WorkRoot, "later-ticket-t-9")
	if err := os.MkdirAll(later, 0o700); err != nil {
		t.Fatal(err)
	}
	if !covered(deny, later) || covered(allow, later) {
		t.Errorf("a later Ticket's directory is readable: deny %v, allow %v", deny, allow)
	}
	for _, need := range []string{dir, h.cfg.SocketPath} {
		if !covered(allow, need) {
			t.Errorf("allowRead %v does not give back %s, which the session needs", allow, need)
		}
	}
	for _, a := range allow {
		for _, guarded := range []string{key, resolvedPath(key), filepath.Join(home, ".ssh"), filepath.Join(home, ".git-credentials")} {
			if within(a, guarded) || within(guarded, a) {
				t.Errorf("allowRead %s gives back %s", a, guarded)
			}
		}
	}
	// Git reads no global config of the owner's (unreadable, it would stop git), and its XDG files are the session's.
	if settings.Env["GIT_CONFIG_GLOBAL"] != os.DevNull || !strings.HasPrefix(settings.Env["XDG_CONFIG_HOME"], restrictedCacheDir(dir)) {
		t.Errorf("env = %v, want GIT_CONFIG_GLOBAL=%s and XDG_CONFIG_HOME in the session's cache", settings.Env, os.DevNull)
	}
	// A standard session on the same node has no sandbox at all, as before.
	if sb, _ := h.n.sessionSandbox(dir, false, false); sb != nil {
		t.Errorf("standard sandbox = %+v, want none", sb)
	}
}

// On Linux, where the sandbox lets commands reach any Unix socket, a restricted session's commands cannot see the
// daemons' sockets (Docker, containerd, Podman, and the user's runtime directory but for the node socket): the paths
// are denied to reads and writes, so that the sandbox masks them. Elsewhere, and for a standard session, nothing
// changes.
func TestRestrictedSandboxMasksDaemonSockets(t *testing.T) {
	xdg := t.TempDir()
	for _, d := range []string{"podman", "athena"} {
		if err := os.MkdirAll(filepath.Join(xdg, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(xdg, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(xdg, "athena", "node.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	n := New(Config{SocketPath: sock, StateDir: t.TempDir(), Sandbox: &SandboxConfig{}}, nil)
	work := t.TempDir()
	masked := []string{"/var/run/docker.sock", "/run/docker.sock", "/run/containerd", "/run/podman",
		resolvedPath(filepath.Join(xdg, "podman")), resolvedPath(filepath.Join(xdg, "bus"))}

	sb, _ := n.sessionSandboxOn("linux", work, false, true)
	for _, m := range masked {
		if !slices.Contains(sb.DenyRead, m) || !slices.Contains(sb.DenyWrite, m) {
			t.Errorf("linux restricted: %s not masked: denyRead %v, denyWrite %v", m, sb.DenyRead, sb.DenyWrite)
		}
	}
	for _, d := range append(slices.Clone(sb.DenyRead), sb.DenyWrite...) {
		if within(d, resolvedPath(sock)) {
			t.Errorf("linux restricted: %s masks the node socket", d)
		}
	}
	for _, c := range []struct {
		goos       string
		restricted bool
	}{{"darwin", true}, {"linux", false}} {
		sb, _ := n.sessionSandboxOn(c.goos, work, false, c.restricted)
		if slices.Contains(sb.DenyRead, "/var/run/docker.sock") || slices.Contains(sb.DenyWrite, "/var/run/docker.sock") {
			t.Errorf("%s restricted=%v: daemon sockets masked: %+v", c.goos, c.restricted, sb)
		}
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

// A restricted Ticket runs on Claude Code whatever runtime it names: under auto even where Codex is preferred, and
// pinned to Codex (a stranger's Ticket gets the server's default) too. The runtime it ran on is recorded as ever.
func TestRestrictedRunsOnClaudeOnly(t *testing.T) {
	h := newCodexHarness(t, codexTurn(), func(c *Config) {
		c.Policy = restrictOwner
		c.Chooser = HeadroomChooser{Preferred: core.RuntimeCodex}
	})
	auto, err := h.eng.SubmitOwnerTask("Auto stranger task", core.TaskOptions{Runtime: core.RuntimeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if _, a := h.running(auto.ID); a.Runtime != core.RuntimeClaude {
		t.Fatalf("auto ran on %q, want claude", a.Runtime)
	}
	pinned, err := h.eng.SubmitOwnerTask("Pinned stranger task", core.TaskOptions{Runtime: core.RuntimeCodex, Model: "gpt-5-codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, a := h.running(pinned.ID); a.Runtime != core.RuntimeClaude {
		t.Fatalf("codex-pinned ran on %q, want claude", a.Runtime)
	}
	inv := h.invocations()
	if len(inv) != 2 {
		t.Fatalf("invocations = %+v, want two Claude Code sessions", inv)
	}
	for _, i := range inv {
		if i.Env["ATHENA_RUNTIME"] != core.RuntimeClaude || slices.Contains(i.Args, "gpt-5-codex") {
			t.Errorf("invocation = %+v, want Claude Code without Codex's model", i)
		}
	}
	if strings.Contains(strings.Join(h.rpc(), "\n"), "thread/start") {
		t.Fatal("a Codex session started")
	}
}

// Where Claude Code cannot run, a restricted Ticket fails, saying why, and nothing starts.
func TestRestrictedWithoutClaudeFails(t *testing.T) {
	h := newCodexHarness(t, codexTurn(), func(c *Config) {
		c.Policy = restrictOwner
		delete(c.Runtimes, core.RuntimeClaude)
	})
	tk := h.submitCodex("Stranger's change")
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(tk.ID)
		r := tk.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	tk, _ = h.ticket(tk.ID)
	if got := tk.LatestAssignment().Report.Summary; !strings.HasPrefix(got, restrictedRuntimeSummary) || len(got) == len(restrictedRuntimeSummary) {
		t.Fatalf("summary = %q, want %q and a cause", got, restrictedRuntimeSummary)
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

// A restricted session's commands cannot read what Claude Code keeps in its per-user temporary root (other Tickets'
// task output), in either spelling. The session gets a temporary root of its own, in a parent of restricted roots that
// is denied as a whole, so only its own is read again; a standard session is unchanged.
func TestRestrictedDeniesClaudeTempRoot(t *testing.T) {
	old := tempBase
	tempBase = "/tmp" // what the node uses: the length below is of the real paths; nothing is made here
	t.Cleanup(func() { tempBase = old })
	n := New(Config{SocketPath: filepath.Join(t.TempDir(), "n.sock"), StateDir: t.TempDir(), Sandbox: &SandboxConfig{}}, nil)
	work, other := t.TempDir(), t.TempDir()
	sb, _ := n.sessionSandboxOn("darwin", work, false, true)
	denied := func(p string) bool {
		return slices.ContainsFunc(sb.DenyRead, func(d string) bool { return within(d, p) })
	}
	allowed := func(p string) bool {
		return slices.ContainsFunc(sb.AllowRead, func(a string) bool { return within(a, p) })
	}
	for _, root := range claudeTempRoots(os.Getuid()) {
		for _, p := range []string{root, resolvedPath(root), filepath.Join(root, "-work-T2", "sid", "tasks", "b1.output"), filepath.Join(resolvedPath(root), "-work-T2", "sid", "tasks", "b1.output")} {
			if !denied(p) || allowed(p) {
				t.Errorf("%s is readable: deny %v, allow %v", p, sb.DenyRead, sb.AllowRead)
			}
		}
	}
	own, theirs := restrictedTempDir(work), restrictedTempDir(other)
	if own == theirs || !strings.HasPrefix(own, restrictedTempParent(os.Getuid())+"/") {
		t.Fatalf("own = %s, theirs = %s", own, theirs)
	}
	if !denied(theirs) || allowed(theirs) || !denied(resolvedPath(theirs)) || allowed(resolvedPath(theirs)) {
		t.Errorf("another session's temporary root is readable: deny %v, allow %v", sb.DenyRead, sb.AllowRead)
	}
	if !allowed(own) || !allowed(resolvedPath(own)) {
		t.Errorf("its own temporary root %s is not read again: %v", own, sb.AllowRead)
	}
	// Claude Code puts claude-<uid> in the root and falls back to the shared one above 44 bytes.
	if got := len(filepath.Join(own, fmt.Sprintf("claude-%d", os.Getuid()))); got > 44 {
		t.Errorf("per-user directory is %d bytes, over Claude Code's 44", got)
	}
	std, _ := n.sessionSandboxOn("darwin", work, false, false)
	if slices.Contains(std.DenyRead, claudeTempRoots(os.Getuid())[0]) {
		t.Errorf("standard session denied the temp root: %v", std.DenyRead)
	}
}

// A restricted session starts with its own temporary root, made private; a standard one keeps Claude Code's default.
func TestRestrictedSessionGetsOwnTempRoot(t *testing.T) {
	if !SandboxSupported() {
		t.Skip("no sandbox on this platform")
	}
	startEnv := func(p Policy) (*harness, core.Ticket, []string) {
		rec := newRecordingRuntime(fakeClaude(t))
		h := newHarness(t, func(c *Config) { c.Policy, c.Runtimes = p, map[string]rt.Runtime{"claude": rec} })
		tk := h.submit("A change", newRepo(t))
		tk, _ = h.running(tk.ID)
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return h, tk, slices.Clone((*rec.starts)[0].Env)
	}
	h, tk, env := startEnv(restrictOwner)
	own := restrictedTempDir(TicketDir(h.cfg.WorkRoot, tk))
	t.Cleanup(func() { _ = os.RemoveAll(own) })
	if !slices.Contains(env, "CLAUDE_CODE_TMPDIR="+own) {
		t.Fatalf("env = %v, want CLAUDE_CODE_TMPDIR=%s", env, own)
	}
	st, err := os.Stat(own)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("temporary root: %v %v", st, err)
	}
	if _, _, env := startEnv(fakeToolPolicy{}); slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "CLAUDE_CODE_TMPDIR=") }) {
		t.Fatalf("standard session's env = %v", env)
	}
}

// A restricted session's temporary root goes when its Ticket ends, whatever becomes of the worktree.
func TestCleanupRemovesRestrictedTemp(t *testing.T) {
	work := t.TempDir()
	if err := makeRestrictedTemp(work); err != nil {
		t.Fatal(err)
	}
	own := restrictedTempDir(work)
	t.Cleanup(func() { _ = os.RemoveAll(own) })
	cleanupWorktree(placement{workDir: work}, core.Ticket{}, "", slog.Default())
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatalf("temporary root still there: %v", err)
	}
}

// A restricted session reads no shell snapshot of the owner's: they are other sessions' too, and Claude Code carries on
// without one.
func TestRestrictedReadsNoShellSnapshots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	n := New(Config{SocketPath: filepath.Join(t.TempDir(), "n.sock"), StateDir: t.TempDir()}, nil)
	_, allow := n.restrictedReads("darwin", t.TempDir())
	snap := filepath.Join(home, ".claude", "shell-snapshots", "snapshot-zsh-1-a.sh")
	if slices.ContainsFunc(allow, func(a string) bool { return within(a, snap) || within(a, resolvedPath(snap)) }) {
		t.Fatalf("allowRead gives back %s: %v", snap, allow)
	}
}

// Only directories of known toolchains under the home directory are read again: a project's bin directory on PATH
// would give back the project.
func TestHomeToolchainsKeepsToKnownRoots(t *testing.T) {
	home := "/home/o"
	path := strings.Join([]string{"/usr/bin", home + "/.nvm/versions/node/v22/bin", home + "/go/bin", home + "/.local/bin", home + "/.cargo/bin",
		home + "/src/proj/bin", home + "/go/src/acme/proj/bin", home + "/bin", home, "relative/bin", "/elsewhere/bin"}, string(filepath.ListSeparator))
	want := []string{home + "/.nvm/versions/node/v22/bin", home + "/.nvm/versions/node/v22", home + "/go/bin", home + "/.local/bin", home + "/.cargo/bin"}
	if got := homeToolchains(home, path); !slices.Equal(got, want) {
		t.Fatalf("homeToolchains = %v, want %v", got, want)
	}
}

// A restricted session needs a Claude Code that knows sandbox.filesystem.allowRead: on an older one its Ticket fails,
// saying so, and nothing starts. A version newer than the minimum, or one the node cannot read, launches as ever.
func TestRestrictedNeedsRecentClaude(t *testing.T) {
	failure := func(version string, p Policy) (core.Ticket, *harness) {
		h := newHarness(t, func(c *Config) { c.Policy = p })
		h.n.claudeVersion = func(context.Context) string { return version }
		tk := h.submit("Stranger's change", "")
		h.eventually("settled", func() bool {
			tk, _ := h.ticket(tk.ID)
			r := tk.LatestAssignment().Report
			return r != nil && r.State == core.ReportFailed || len(h.invocations()) > 0
		})
		tk, _ = h.ticket(tk.ID)
		return tk, h
	}
	tk, h := failure("2.1.279", restrictOwner)
	want := "This Ticket came from someone outside your allow list, and such work needs Claude Code " + minRestrictedClaude + " or newer; this machine has 2.1.279."
	if got := tk.LatestAssignment().Report; got == nil || got.State != core.ReportFailed || got.Summary != want {
		t.Fatalf("report = %+v, want failed with %q", got, want)
	}
	if n := len(h.invocations()); n != 0 {
		t.Fatalf("the session started %d times", n)
	}
	for _, v := range []string{minRestrictedClaude, "2.2.0", "", "nonsense"} {
		if _, h := failure(v, restrictOwner); len(h.invocations()) != 1 {
			t.Errorf("version %q: invocations = %d, want it launched", v, len(h.invocations()))
		}
	}
	if _, h := failure("2.1.279", fakeToolPolicy{}); len(h.invocations()) != 1 {
		t.Errorf("standard session on an old Claude Code did not launch")
	}
}

// On Linux the sandbox skips a read deny of a path that does not exist, so a standard session making Claude Code's
// per-user directory later would leave it readable: it is made before the restricted session starts.
func TestRestrictedTempMakesTheDeniedRoot(t *testing.T) {
	old := tempBase
	tempBase = t.TempDir()
	t.Cleanup(func() { tempBase = old })
	if err := makeRestrictedTemp(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	root := claudeTempRoots(os.Getuid())[0]
	if st, err := os.Stat(root); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("%s: %v %v, want a private directory", root, st, err)
	}
}
