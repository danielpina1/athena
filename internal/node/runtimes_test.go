package node

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/runtime/claude"
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
	// A worktree's commits land in the repository's git directory.
	if want := filepath.Join(repo, ".git"); len(o.WritableRoots) != 1 || !sameFile(o.WritableRoots[0], want) {
		t.Errorf("writable roots = %v, want [%s] (work dir %s)", o.WritableRoots, want, wd)
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
	rs := defaultRuntimes(Config{ClaudeBinary: "/opt/claude"})
	if r, ok := rs["claude"].(claude.Runtime); len(rs) != 1 || !ok || r.Binary != "/opt/claude" || r.Name() != "claude" {
		t.Fatalf("default runtimes = %#v", rs)
	}
}

func sameFile(a, b string) bool {
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ea == eb
}
