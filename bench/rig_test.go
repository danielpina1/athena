package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/server"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
	"github.com/danielpina1/athena/internal/web/webauthntest"
)

// The rig is the real store, Engine and node wired as `athena server` wires them, with the fake `claude` of the
// runtime package as the runtime: no model, no network. What it measures is Athena's own overhead.

const (
	startTimeout = 20 * time.Second
	// stopTimeout is how long a server gets to exit after SIGINT.
	stopTimeout = 10 * time.Second
)

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

// hook sends one hook request of runtime ("" is Claude Code) over the node socket, as `athena hook` does, and returns
// its latency.
func (r *rig) hook(tk core.Ticket, a core.Assignment, runtime, tool string, input any) time.Duration {
	raw, _ := json.Marshal(map[string]any{"hook_event_name": node.EventPreToolUse, "tool_name": tool, "tool_input": input,
		"turn_id": "turn_bench"})
	req := node.HookRequest{Op: node.OpHook, Event: node.EventPreToolUse, Ticket: tk.ID, Assignment: a.ID, Runtime: runtime, Input: raw}
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
	// Claude Code's tool calls, and Codex's (argv shell calls, a patch, a commit the attribution rule reads), whose
	// decisions also normalise the call and check the protected paths.
	patch := "*** Begin Patch\n*** Update File: " + filepath.Join(wd, "a.txt") + "\n@@\n-x\n+y\n*** Add File: b.txt\n+z\n*** End Patch\n"
	calls := []struct {
		runtime, tool string
		input         any
	}{
		{"", "Write", map[string]any{"file_path": filepath.Join(wd, "a.txt"), "content": "x"}},
		{"", "Read", map[string]any{"file_path": filepath.Join(wd, "a.txt")}},
		{"", "Bash", map[string]any{"command": "ls -la"}},
		{core.RuntimeCodex, "exec_command", map[string]any{"cmd": []string{"bash", "-lc", "ls -la && git status"}, "workdir": wd}},
		{core.RuntimeCodex, "apply_patch", map[string]any{"command": patch}},
		{core.RuntimeCodex, "shell", map[string]any{"command": []string{"git", "commit", "-m", "Fix the parser\n\nCloses #3"}}},
	}
	for i := 0; i < 50; i++ { // warm up
		c := calls[i%len(calls)]
		r.hook(tk, a, c.runtime, c.tool, c.input)
	}
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		c := calls[i%len(calls)]
		out = append(out, r.hook(tk, a, c.runtime, c.tool, c.input))
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

// stopServer interrupts a server subprocess and waits for it; one that has not exited after stopTimeout is killed.
func stopServer(cmd *exec.Cmd) {
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(stopTimeout):
		_ = cmd.Process.Kill()
		<-done
	}
}

// idleRSS starts a real `athena server` with the fake runtime, leaves it idle past two samples of its resource
// gauges and returns the process_rss_bytes it reports (its peak RSS where the OS offers nothing better).
func idleRSS(tb testing.TB, idle time.Duration) uint64 {
	tb.Helper()
	bin := build(tb, "athena", "github.com/danielpina1/athena/cmd/athena")
	fake := fakeClaude(tb)
	dir := shortDir(tb)
	// Codex is a binary that does not exist: no measurement reaches this machine's codex.
	cmd := exec.Command(bin, "server", "--state-dir", dir, "--claude", fake, "--codex", "/nonexistent/athena-bench-codex")
	if err := cmd.Start(); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { stopServer(cmd) })
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
	var snap metrics.Snapshot
	err := controlCall(stateDir, server.Request{Op: server.OpMetrics, Window: "start"}, &snap)
	return snap, err
}

// controlCall sends one request to a server's control socket and decodes the data of its answer into out (nil to
// ignore it).
func controlCall(stateDir string, req server.Request, out any) error {
	conn, err := net.DialTimeout("unix", filepath.Join(stateDir, server.ControlSocket), time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	var resp struct {
		OK    bool            `json:"ok"`
		Error string          `json:"error"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s: %s", req.Op, resp.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(resp.Data, out)
}

// dashboard is a real `athena server` (fake runtime) with the dashboard's endpoint on loopback, tls off, and one
// signed-in client on the link: the path a phone takes, minus the network. It has one running Ticket whose session
// answers every message with a line (ATHENA_FAKE_LATER_FIXTURE).
type dashboard struct {
	tb     testing.TB
	dir    string
	origin string
	ticket string

	sess   *link.Session
	frames chan *athenav1.Frame
	cmds   int
	// seq and offset are the latest delta and stream offset taken from the link.
	seq    uint64
	offset int64
}

// webWait bounds every wait for the server.
const webWait = 20 * time.Second

// quiet is how long the link must be silent before a sample is over: the late frames of one sample (the session's
// echo, the Ticket's changes after it) must not answer the next.
const quiet = 15 * time.Millisecond

func startDashboard(tb testing.TB) *dashboard {
	tb.Helper()
	bin := build(tb, "athena", "github.com/danielpina1/athena/cmd/athena")
	fake := fakeClaude(tb)
	dir := shortDir(tb)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	listen := l.Addr().String()
	l.Close()
	if err := server.SetupWeb(dir, server.WebConfig{Listen: listen, Host: "localhost", TLS: "off"}); err != nil {
		tb.Fatal(err)
	}
	// No local repository discovery: a scan of this machine's folders would only add noise to the measurements.
	cfg, err := server.LoadConfig(dir)
	if err != nil {
		tb.Fatal(err)
	}
	cfg.LocalRepoRoots = &[]string{}
	if err := server.SaveConfig(dir, cfg); err != nil {
		tb.Fatal(err)
	}
	// Every user line the session reads after the first is answered with one assistant line.
	later := filepath.Join(dir, "later.jsonl")
	line := `{"type":"assistant","message":{"model":"fake","id":"msg_bench","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"ok"}],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(later, []byte(line), 0o600); err != nil {
		tb.Fatal(err)
	}
	cmd := exec.Command(bin, "server", "--state-dir", dir, "--claude", fake)
	cmd.Env = append(os.Environ(), "ATHENA_FAKE_LATER_FIXTURE="+later)
	if err := cmd.Start(); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { stopServer(cmd) })
	d := &dashboard{tb: tb, dir: dir, origin: "http://localhost" + listen[strings.LastIndex(listen, ":"):], frames: make(chan *athenav1.Frame, 1<<14)}
	for deadline := time.Now().Add(startTimeout); ; time.Sleep(20 * time.Millisecond) {
		var st server.WebStatusResult
		if err := controlCall(dir, server.Request{Op: server.OpWebStatus}, &st); err == nil && st.Serving {
			break
		}
		if time.Now().After(deadline) {
			tb.Fatal("the dashboard's endpoint did not come up")
		}
	}
	d.connect(d.signIn())
	d.ticket = d.startTicket()
	return d
}

// signIn enrolls a software passkey with a token issued the way `athena web enroll` does (the store is shared with
// the running server) and logs in; it returns the session cookie.
func (d *dashboard) signIn() string {
	tb := d.tb
	st, err := store.Open(filepath.Join(d.dir, "athena.db"))
	if err != nil {
		tb.Fatal(err)
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	token, err := web.IssueEnrollToken(st, 0)
	slog.SetDefault(prev)
	st.Close()
	if err != nil {
		tb.Fatal(err)
	}
	authn := webauthntest.New(tb, d.origin, "localhost")
	cookies := map[string]*http.Cookie{}
	post := func(path string, body []byte) []byte {
		req, err := http.NewRequest("POST", d.origin+path, bytes.NewReader(body))
		if err != nil {
			tb.Fatal(err)
		}
		req.Header.Set("Origin", d.origin)
		req.Header.Set("Content-Type", "application/json")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err := (&http.Client{Timeout: webWait}).Do(req)
		if err != nil {
			tb.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			tb.Fatalf("%s = %d %s", path, resp.StatusCode, out)
		}
		for _, c := range resp.Cookies() {
			if c.MaxAge < 0 {
				delete(cookies, c.Name)
			} else {
				cookies[c.Name] = c
			}
		}
		return out
	}
	body, _ := json.Marshal(map[string]string{"token": token, "name": "bench"})
	post("/auth/register/finish", authn.Create(tb, post("/auth/register/begin", body)))
	cookies = map[string]*http.Cookie{}
	post("/auth/login/finish", authn.Assert(tb, post("/auth/login/begin", nil)))
	return cookies[web.SessionCookie].Value
}

// connect opens the link and says hello; the snapshot is taken off the frames.
func (d *dashboard) connect(cookie string) {
	tb := d.tb
	h := http.Header{}
	h.Set("Origin", d.origin)
	h.Set("Cookie", web.SessionCookie+"="+cookie)
	ctx, cancel := context.WithTimeout(context.Background(), webWait)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(d.origin, "http")+"/ws", &websocket.DialOptions{
		HTTPHeader: h, Subprotocols: []string{link.Subprotocol}, CompressionMode: websocket.CompressionNoContextTakeover})
	if err != nil {
		tb.Fatalf("dial: %v", err)
	}
	d.sess = link.New(link.NewWS(c), link.Options{OnFrame: func(f *athenav1.Frame) {
		select {
		case d.frames <- f:
		default: // never blocks the reader; a sample then times out
		}
	}})
	hello := &athenav1.Hello{Protocol: web.Protocol, ClientId: "bench"}
	if err := d.sess.Send(athenav1.Channel_CONTROL, false, &athenav1.Frame{Body: &athenav1.Frame_Hello{Hello: hello}}); err != nil {
		tb.Fatal(err)
	}
	rctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.sess.Run(rctx); close(done) }()
	tb.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(webWait):
		}
	})
	snap := d.await(func(f *athenav1.Frame) bool { return f.GetSnapshot() != nil }).GetSnapshot()
	d.seq = snap.GetCursor()
}

// await returns the first frame match accepts, dropping the others; it fails the run after webWait.
func (d *dashboard) await(match func(*athenav1.Frame) bool) *athenav1.Frame {
	d.tb.Helper()
	timeout := time.After(webWait)
	for {
		select {
		case f := <-d.frames:
			if match(f) {
				return f
			}
		case <-timeout:
			d.tb.Fatal("timed out waiting for a frame")
		}
	}
}

// settle drains the link until it has been silent for quiet, and keeps the latest delta seq and stream offset.
func (d *dashboard) settle() {
	for {
		select {
		case f := <-d.frames:
			d.note(f)
		case <-time.After(quiet):
			return
		}
	}
}

func (d *dashboard) note(f *athenav1.Frame) {
	d.seq = max(d.seq, f.GetDelta().GetSeq())
	for _, it := range f.GetStreamItems().GetItems() {
		d.offset = max(d.offset, it.GetOffset())
	}
}

// startTicket submits a task through the control socket and waits until its session runs and its output is open on
// the link; the grant lets the stream carry every sample's lines.
func (d *dashboard) startTicket() string {
	tb := d.tb
	var sub struct {
		ID string `json:"id"`
	}
	if err := controlCall(d.dir, server.Request{Op: server.OpSubmit, Text: "dashboard benchmark"}, &sub); err != nil {
		tb.Fatal(err)
	}
	if err := d.sess.Grant(athenav1.Channel_STREAM, 1<<20); err != nil {
		tb.Fatal(err)
	}
	if err := d.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_StreamOpen{
		StreamOpen: &athenav1.StreamOpen{TicketId: sub.ID}}}); err != nil {
		tb.Fatal(err)
	}
	// The session's own output, not only the runtime marker the node writes before its process starts: from then on a
	// Reply is a message of its own, not part of the session's first one.
	d.await(func(f *athenav1.Frame) bool {
		d.note(f)
		if f.GetStreamItems().GetTicketId() != sub.ID {
			return false
		}
		for _, it := range f.GetStreamItems().GetItems() {
			if it.GetKind() != "runtime" {
				return true
			}
		}
		return false
	})
	d.settle()
	return sub.ID
}

// reply sends a Reply Command over the link; it returns the Command's id.
func (d *dashboard) reply() string {
	d.cmds++
	id := fmt.Sprintf("c%d", d.cmds)
	cmd := &athenav1.Command{Id: id, Action: &athenav1.Command_Reply{Reply: &athenav1.Reply{TicketId: d.ticket, Text: "again " + id}}}
	if err := d.sess.Send(athenav1.Channel_CONTROL, true, &athenav1.Frame{Body: &athenav1.Frame_Command{Command: cmd}}); err != nil {
		d.tb.Fatal(err)
	}
	return id
}

// measureLinkDelta returns n latencies from an Engine commit (a Reply through the control socket, so that the link
// is not part of what triggers it) to the Ticket's delta arriving on the link. The control socket's round trip to the
// commit is inside it: nothing outside the server sees the commit itself.
func measureLinkDelta(tb testing.TB, n int) []time.Duration {
	d := startDashboard(tb)
	out := make([]time.Duration, 0, n)
	for i := 0; i < n+10; i++ {
		start := time.Now()
		req := server.Request{Op: server.OpReply, ID: d.ticket, Text: fmt.Sprintf("delta %d", i)}
		if err := controlCall(d.dir, req, nil); err != nil {
			tb.Fatal(err)
		}
		got := d.await(func(f *athenav1.Frame) bool {
			return f.GetDelta().GetTicket().GetId() == d.ticket && f.GetDelta().GetSeq() > d.seq
		})
		took := time.Since(start)
		d.note(got)
		d.settle()
		if i >= 10 { // the first commands warm the page cache
			out = append(out, took)
		}
	}
	return out
}

// measureStreamItem returns n latencies from a Reply to the fake runtime's answer to it (its assistant line, a "say"
// item) arriving on the link; the echo of the Reply's own user line (an "other" item) does not end a sample.
func measureStreamItem(tb testing.TB, n int) []time.Duration {
	d := startDashboard(tb)
	out := make([]time.Duration, 0, n)
	for i := 0; i < n+10; i++ {
		start := time.Now()
		d.reply()
		got := d.await(func(f *athenav1.Frame) bool {
			for _, it := range f.GetStreamItems().GetItems() {
				if f.GetStreamItems().GetTicketId() == d.ticket && it.GetOffset() > d.offset && it.GetKind() == "say" {
					return true
				}
			}
			return false
		})
		took := time.Since(start)
		d.note(got)
		d.settle()
		if i >= 10 {
			out = append(out, took)
		}
	}
	return out
}

// measureCommandResult returns n latencies from a Reply Command sent on the link to its CommandResult.
func measureCommandResult(tb testing.TB, n int) []time.Duration {
	d := startDashboard(tb)
	out := make([]time.Duration, 0, n)
	for i := 0; i < n+10; i++ {
		start := time.Now()
		id := d.reply()
		res := d.await(func(f *athenav1.Frame) bool { return f.GetCommandResult().GetId() == id }).GetCommandResult()
		took := time.Since(start)
		if !res.GetOk() {
			tb.Fatalf("reply = %v", res)
		}
		d.settle()
		if i >= 10 {
			out = append(out, took)
		}
	}
	return out
}

// webIdleRSS is idleRSS with one client connected to the dashboard's endpoint, as the owner's phone is.
func webIdleRSS(tb testing.TB, idle time.Duration) uint64 {
	tb.Helper()
	d := startDashboard(tb)
	time.Sleep(idle)
	snap, err := controlMetrics(d.dir)
	if err != nil {
		tb.Fatal(err)
	}
	var st server.WebStatusResult
	if err := controlCall(d.dir, server.Request{Op: server.OpWebStatus}, &st); err != nil || st.Clients != 1 {
		tb.Fatalf("web.status = %+v, %v; want one connected client", st, err)
	}
	for _, g := range snap.Gauges {
		if g.Name == "process_rss_bytes" {
			return uint64(g.Value)
		}
	}
	tb.Fatal("server reports no process_rss_bytes")
	return 0
}
