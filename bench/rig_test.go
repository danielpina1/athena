package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/server"
	"github.com/danielpina1/athena/internal/store"
)

// The rig is the real store, Engine and node wired as `athena server` wires them, with the fake `claude` of the
// runtime package as the runtime: no model, no network. What it measures is Athena's own overhead.

const startTimeout = 20 * time.Second

var (
	buildMu   sync.Mutex
	buildDir  string
	buildErr  error
	fakePath  string
	athenaBin string
)

// TestMain removes the binaries the benchmarks built.
func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func build(tb testing.TB, out, pkg string) string {
	tb.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if buildDir == "" {
		buildDir, buildErr = os.MkdirTemp("", "athbench")
		if buildErr != nil {
			tb.Fatal(buildErr)
		}
	}
	path := filepath.Join(buildDir, out)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	cmd := exec.Command("go", "build", "-o", path, pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		tb.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
	return path
}

// fakeClaude builds the fake `claude` once and points it at a turn that never ends: its init line only, so the
// Assignment stays running.
func fakeClaude(tb testing.TB) string {
	tb.Helper()
	if fakePath == "" {
		fakePath = build(tb, "claude", "github.com/danielpina1/athena/internal/runtime/claude/testdata/fakeclaude")
	}
	lines, err := os.ReadFile(filepath.Join("..", "internal", "runtime", "claude", "testdata", "claude", "simple-turn.jsonl"))
	if err != nil {
		tb.Fatal(err)
	}
	init, _, _ := strings.Cut(string(lines), "\n")
	fixture := filepath.Join(tb.TempDir(), "open-turn.jsonl")
	if err := os.WriteFile(fixture, []byte(init+"\n"), 0o600); err != nil {
		tb.Fatal(err)
	}
	tb.Setenv("ATHENA_FAKE_FIXTURE", fixture)
	return fakePath
}

// shortDir returns a state directory with a path short enough for Unix sockets.
func shortDir(tb testing.TB) string {
	tb.Helper()
	d, err := os.MkdirTemp("/tmp", "athb")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type rig struct {
	tb      testing.TB
	dir     string
	reg     *metrics.Registry
	eng     *core.Engine
	started chan startedEvent
}

type startedEvent struct {
	ticket string
	at     time.Time
}

func newRig(tb testing.TB) *rig {
	tb.Helper()
	fake := fakeClaude(tb)
	dir := shortDir(tb)
	reg := metrics.NewRegistry()
	// Log to a file in JSON as the server does; the node picks the default logger up when it is created.
	logFile, err := os.Create(filepath.Join(dir, "athena.log"))
	if err != nil {
		tb.Fatal(err)
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logFile, nil)))
	st, err := store.Open(filepath.Join(dir, "athena.db"))
	if err != nil {
		tb.Fatal(err)
	}
	st.SetMetrics(reg)
	n := node.New(node.Config{
		StateDir: dir, WorkRoot: filepath.Join(dir, "work"), SocketPath: filepath.Join(dir, "node.sock"),
		AthenaBin: "/usr/bin/true", ClaudeBinary: fake, Slots: 16, ApprovalTimeout: time.Minute, Metrics: reg,
	}, nil)
	r := &rig{tb: tb, dir: dir, reg: reg, started: make(chan startedEvent, 4096)}
	eng := core.NewEngine(st.Repos(), n, time.Now, func(e core.Event) {
		if e.Type == core.EventAssignmentStarted {
			select {
			case r.started <- startedEvent{e.TicketID, time.Now()}:
			default:
			}
		}
	})
	eng.SetMetrics(reg)
	n.SetEngine(eng)
	r.eng = eng
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- n.Serve(ctx) }()
	tb.Cleanup(func() {
		n.Close()
		cancel()
		<-served
		st.Close()
		slog.SetDefault(prev)
		logFile.Close()
	})
	deadline := time.Now().Add(startTimeout)
	for {
		if c, err := net.DialTimeout("unix", filepath.Join(dir, "node.sock"), time.Second); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			tb.Fatal("node socket did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return r
}

// waitStarted waits for the Assignment-started event of a Ticket.
func (r *rig) waitStarted(ticket string) time.Time {
	r.tb.Helper()
	timeout := time.After(startTimeout)
	for {
		select {
		case e := <-r.started:
			if e.ticket == ticket {
				return e.at
			}
		case <-timeout:
			r.tb.Fatalf("ticket %s did not start", ticket)
		}
	}
}

// runningTicket submits a task and waits until its session runs.
func (r *rig) runningTicket(dir string) (core.Ticket, core.Assignment) {
	r.tb.Helper()
	tk, err := r.eng.SubmitOwnerTask("benchmark task", core.TaskOptions{Dir: dir})
	if err != nil {
		r.tb.Fatal(err)
	}
	r.waitStarted(tk.ID)
	cur, _, err := r.eng.Ticket(tk.ID)
	if err != nil {
		r.tb.Fatal(err)
	}
	return cur, *cur.LatestAssignment()
}

// hook sends one hook request over the node socket, as `athena hook` does, and returns its latency.
func (r *rig) hook(tk core.Ticket, a core.Assignment, tool string, input map[string]any) time.Duration {
	raw, _ := json.Marshal(map[string]any{"hook_event_name": node.EventPreToolUse, "tool_name": tool, "tool_input": input})
	req := node.HookRequest{Op: node.OpHook, Event: node.EventPreToolUse, Ticket: tk.ID, Assignment: a.ID, Input: raw}
	start := time.Now()
	conn, err := net.DialTimeout("unix", filepath.Join(r.dir, "node.sock"), time.Second)
	if err != nil {
		r.tb.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		r.tb.Fatal(err)
	}
	var resp node.HookResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		r.tb.Fatal(err)
	}
	d := time.Since(start)
	if resp.Verdict != node.VerdictAllow {
		r.tb.Fatalf("%s hook: %+v, want allow", tool, resp)
	}
	return d
}

// measureHook returns the latencies of n hook decisions of a running session.
func measureHook(tb testing.TB, n int) []time.Duration {
	r := newRig(tb)
	tk, a := r.runningTicket("")
	wd, err := node.WorkingDir(filepath.Join(r.dir, "work"), tk)
	if err != nil {
		tb.Fatal(err)
	}
	calls := []struct {
		tool  string
		input map[string]any
	}{
		{"Write", map[string]any{"file_path": filepath.Join(wd, "a.txt"), "content": "x"}},
		{"Read", map[string]any{"file_path": filepath.Join(wd, "a.txt")}},
		{"Bash", map[string]any{"command": "ls -la"}},
	}
	for i := 0; i < 50; i++ { // warm up
		c := calls[i%len(calls)]
		r.hook(tk, a, c.tool, c.input)
	}
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		c := calls[i%len(calls)]
		out = append(out, r.hook(tk, a, c.tool, c.input))
	}
	return out
}

// measureEngine returns the latencies of n Engine commands (reports and turn ends) of a running Ticket, each one
// transaction on the real SQLite store with synchronous=FULL.
func measureEngine(tb testing.TB, n int) []time.Duration {
	r := newRig(tb)
	tk, a := r.runningTicket("")
	out := make([]time.Duration, 0, n)
	for i := 0; i < n+20; i++ {
		start := time.Now()
		var err error
		if i%2 == 0 {
			err = r.eng.FileReport(tk.ID, core.ReportInput{AssignmentID: a.ID, State: core.ReportProgress, Summary: fmt.Sprintf("step %d", i)})
		} else {
			err = r.eng.TurnEnded(tk.ID, a.ID)
		}
		d := time.Since(start)
		if err != nil {
			tb.Fatal(err)
		}
		if i >= 20 { // the first commands warm the page cache
			out = append(out, d)
		}
	}
	return out
}

// measureDispatch returns n dispatch latencies: from submitting a task to its session running (Assignment pending ->
// started), with the fake runtime. With repo set the task names a git repository, so a worktree is created too.
func measureDispatch(tb testing.TB, n int, repo bool) []time.Duration {
	r := newRig(tb)
	dir := ""
	if repo {
		dir = newRepo(tb)
	}
	out := make([]time.Duration, 0, n)
	for i := 0; i < n+3; i++ {
		start := time.Now()
		tk, err := r.eng.SubmitOwnerTask("dispatch benchmark", core.TaskOptions{Dir: dir})
		if err != nil {
			tb.Fatal(err)
		}
		at := r.waitStarted(tk.ID)
		if i >= 3 { // the first sessions warm the file cache
			out = append(out, at.Sub(start))
		}
		if err := r.eng.StopTicket(tk.ID, "done"); err != nil {
			tb.Fatal(err)
		}
		if repo {
			// The node removes a stopped Ticket's clean worktree in the background (three git processes); let it
			// finish so it does not compete with the next dispatch, which is what is measured.
			wd, err := node.WorkingDir(filepath.Join(r.dir, "work"), tk)
			if err != nil {
				tb.Fatal(err)
			}
			for deadline := time.Now().Add(startTimeout); ; time.Sleep(2 * time.Millisecond) {
				if _, err := os.Stat(wd); os.IsNotExist(err) || time.Now().After(deadline) {
					break
				}
			}
		}
	}
	return out
}

// measureSpawn returns n latencies of starting the fake runtime and waiting for it to exit (its stdin is closed): the
// reference cost of the process a dispatch starts, which a busy machine, or a security scanner, inflates.
func measureSpawn(tb testing.TB, n int) []time.Duration {
	fake := fakeClaude(tb)
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		cmd := exec.Command(fake)
		start := time.Now()
		if err := cmd.Run(); err != nil {
			tb.Fatal(err)
		}
		out = append(out, time.Since(start))
	}
	return out
}

// measureGitWorktree returns n latencies of the one git process a repo task's dispatch cannot do without,
// `git worktree add -b`, run directly: git is an external program like the model, and on a loaded machine (or behind
// macOS's xcrun shim) its start alone takes tens of milliseconds, so dispatch_worktree is judged net of it.
func measureGitWorktree(tb testing.TB, n int) []time.Duration {
	repo := newRepo(tb)
	out := make([]time.Duration, 0, n)
	for i := 0; i < n+3; i++ {
		dir := filepath.Join(tb.TempDir(), "wt")
		branch := fmt.Sprintf("b%d", i)
		start := time.Now()
		cmd := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", branch, dir, "HEAD")
		if b, err := cmd.CombinedOutput(); err != nil {
			tb.Fatalf("git worktree add: %v\n%s", err, b)
		}
		d := time.Since(start)
		if i >= 3 {
			out = append(out, d)
		}
		for _, args := range [][]string{{"worktree", "remove", "--force", dir}, {"branch", "-q", "-D", branch}} {
			if b, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
				tb.Fatalf("git %v: %v\n%s", args, err, b)
			}
		}
	}
	return out
}

// newRepo makes a small git repository with one commit.
func newRepo(tb testing.TB) string {
	tb.Helper()
	dir, err := os.MkdirTemp("", "athrepo")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { os.RemoveAll(dir) })
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=b", "GIT_AUTHOR_EMAIL=b@b", "GIT_COMMITTER_NAME=b", "GIT_COMMITTER_EMAIL=b@b")
		if b, err := cmd.CombinedOutput(); err != nil {
			tb.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o600); err != nil {
		tb.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

// idleRSS starts a real `athena server` with the fake runtime, leaves it idle past two samples of its resource
// gauges and returns the process_rss_bytes it reports (its peak RSS where the OS offers nothing better).
func idleRSS(tb testing.TB, idle time.Duration) uint64 {
	tb.Helper()
	bin := build(tb, "athena", "github.com/danielpina1/athena/cmd/athena")
	fake := fakeClaude(tb)
	dir := shortDir(tb)
	cmd := exec.Command(bin, "server", "--state-dir", dir, "--claude", fake)
	if err := cmd.Start(); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(startTimeout)
	for {
		if _, err := controlMetrics(dir); err == nil {
			break
		}
		if time.Now().After(deadline) {
			tb.Fatal("athena server did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(idle)
	snap, err := controlMetrics(dir)
	if err != nil {
		tb.Fatal(err)
	}
	for _, g := range snap.Gauges {
		if g.Name == "process_rss_bytes" {
			return uint64(g.Value)
		}
	}
	tb.Fatal("server reports no process_rss_bytes")
	return 0
}

// controlMetrics asks a server's control socket for its metrics.
func controlMetrics(stateDir string) (metrics.Snapshot, error) {
	conn, err := net.DialTimeout("unix", filepath.Join(stateDir, server.ControlSocket), time.Second)
	if err != nil {
		return metrics.Snapshot{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(server.Request{Op: server.OpMetrics, Window: "start"}); err != nil {
		return metrics.Snapshot{}, err
	}
	var resp struct {
		OK    bool             `json:"ok"`
		Error string           `json:"error"`
		Data  metrics.Snapshot `json:"data"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return metrics.Snapshot{}, err
	}
	if !resp.OK {
		return metrics.Snapshot{}, fmt.Errorf("metrics: %s", resp.Error)
	}
	return resp.Data, nil
}
