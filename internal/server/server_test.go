package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"github.com/danielpina1/athena/internal/store"
)

const (
	fixtureSessionID = "27abadfa-c58f-4e97-aaf3-64a8fdaff179"
	waitTimeout      = 10 * time.Second
	recoveryText     = "previous process ended unexpectedly"
)

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
	fakeDir  string
)

// fakeClaude builds the runtime package's fake `claude` once per test binary run.
func fakeClaude(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakeDir, fakeErr = os.MkdirTemp("", "fakeclaude")
		if fakeErr != nil {
			return
		}
		fakePath = filepath.Join(fakeDir, "claude")
		out, err := exec.Command("go", "build", "-o", fakePath,
			"github.com/danielpina1/athena/internal/runtime/claude/testdata/fakeclaude").CombinedOutput()
		if err != nil {
			fakeErr = fmt.Errorf("build fakeclaude: %v\n%s", err, out)
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakePath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeDir != "" {
		os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}

func claudeFixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "runtime", "claude", "testdata", "claude", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// openTurnFixture is a fixture whose turn never ends: the init line only, so the Assignment keeps running.
func openTurnFixture(t *testing.T) string {
	t.Helper()
	lines, err := os.ReadFile(claudeFixture(t, "simple-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	init, _, _ := strings.Cut(string(lines), "\n")
	p := filepath.Join(t.TempDir(), "open-turn.jsonl")
	if err := os.WriteFile(p, []byte(init+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeEnv points fakeclaude at a fixture and returns its stdin and args logs.
type fakeEnv struct{ stdinLog, argsLog string }

func newFakeEnv(t *testing.T, fixture string) fakeEnv {
	t.Helper()
	base := t.TempDir()
	f := fakeEnv{stdinLog: filepath.Join(base, "stdin.log"), argsLog: filepath.Join(base, "args.log")}
	t.Setenv("ATHENA_FAKE_FIXTURE", fixture)
	t.Setenv("ATHENA_FAKE_STDIN_LOG", f.stdinLog)
	t.Setenv("ATHENA_FAKE_ARGS_APPEND", f.argsLog)
	return f
}

func (f fakeEnv) stdin(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.stdinLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func (f fakeEnv) invocations(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile(f.argsLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var inv struct {
			Args []string `json:"args"`
		}
		if err := json.Unmarshal([]byte(line), &inv); err != nil {
			t.Fatalf("args log: %v", err)
		}
		out = append(out, inv.Args)
	}
	return out
}

func argValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

// newStateDir returns a short state directory: Unix socket paths are limited to ~104 bytes on macOS.
func newStateDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "ath")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func testConfig(t *testing.T, stateDir string) Config {
	return Config{
		StateDir: stateDir,
		Node: node.Config{
			ClaudeBinary:    fakeClaude(t),
			AthenaBin:       "/usr/bin/true",
			ApprovalTimeout: 5 * time.Second,
		},
		Stderr: io.Discard,
	}
}

type testServer struct {
	t        *testing.T
	stateDir string
	cancel   context.CancelFunc
	done     chan error
	stopped  bool
}

func startServer(t *testing.T, stateDir string) *testServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &testServer{t: t, stateDir: stateDir, cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- Run(ctx, testConfig(t, stateDir)) }()
	t.Cleanup(s.stop)
	eventually(t, "control socket answers", func() bool {
		select {
		case err := <-s.done:
			t.Fatalf("Run returned early: %v", err)
		default:
		}
		r, err := call(stateDir, Request{Op: OpPing})
		return err == nil && r.OK
	})
	return s
}

func (s *testServer) stop() {
	if s.stopped {
		return
	}
	s.stopped = true
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Errorf("Run: %v", err)
		}
	case <-time.After(waitTimeout):
		s.t.Fatal("Run did not return after cancel")
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rawResponse is a control response with its data left raw.
type rawResponse struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error"`
	Data  json.RawMessage `json:"data"`
}

func call(stateDir string, req Request) (rawResponse, error) {
	conn, err := net.DialTimeout("unix", filepath.Join(stateDir, "athena.sock"), time.Second)
	if err != nil {
		return rawResponse{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return rawResponse{}, err
	}
	var r rawResponse
	err = json.NewDecoder(conn).Decode(&r)
	return r, err
}

func (s *testServer) ok(req Request, out any) {
	s.t.Helper()
	r, err := call(s.stateDir, req)
	if err != nil {
		s.t.Fatalf("%s: %v", req.Op, err)
	}
	if !r.OK {
		s.t.Fatalf("%s: error %q", req.Op, r.Error)
	}
	if out != nil {
		if err := json.Unmarshal(r.Data, out); err != nil {
			s.t.Fatalf("%s: decode %s: %v", req.Op, r.Data, err)
		}
	}
}

func (s *testServer) detail(id string) TicketDetail {
	s.t.Helper()
	var d TicketDetail
	s.ok(Request{Op: OpTicket, ID: id}, &d)
	return d
}

func (s *testServer) submit(text string) string {
	s.t.Helper()
	var sub struct {
		ID string `json:"id"`
	}
	s.ok(Request{Op: OpSubmit, Text: text}, &sub)
	if sub.ID == "" {
		s.t.Fatal("submit returned no id")
	}
	return sub.ID
}

// running waits until the Ticket's latest Assignment runs in a session with nothing left to deliver.
func (s *testServer) running(id string) core.Assignment {
	s.t.Helper()
	var d TicketDetail
	eventually(s.t, id+" running", func() bool {
		d = s.detail(id)
		a := d.Assignments[len(d.Assignments)-1]
		return a.State == core.AssignmentRunning && a.SessionID != "" && d.Undelivered == 0
	})
	return d.Assignments[len(d.Assignments)-1]
}

// nodeCall sends one request to the node socket.
func nodeCall(t *testing.T, stateDir string, req, resp any) {
	t.Helper()
	conn, err := net.DialTimeout("unix", filepath.Join(stateDir, "node.sock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(conn).Decode(resp); err != nil {
		t.Fatal(err)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// nopExec is an Executor that does nothing: it stands for a server that is down.
type nopExec struct{}

func (nopExec) Start(core.Ticket, core.Assignment)        {}
func (nopExec) Deliver(core.Ticket, core.Assignment)      {}
func (nopExec) Stop(core.Ticket, core.Assignment, string) {}

// offline runs fn against the state dir's store while no server runs.
func offline(t *testing.T, stateDir string, fn func(*core.Engine)) {
	t.Helper()
	st, err := store.Open(filepath.Join(stateDir, "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fn(core.NewEngine(st.Repos(), nopExec{}, nil, nil))
}

// --- tests ---

func TestEndToEndWithFake(t *testing.T) {
	newFakeEnv(t, claudeFixture(t, "simple-turn.jsonl"))
	dir := newStateDir(t)
	s := startServer(t, dir)

	for _, f := range []string{"athena.sock", "node.sock", "logs/athena.log", "athena.pid"} {
		if m := mode(t, filepath.Join(dir, f)); m != 0o600 {
			t.Errorf("%s mode = %o, want 600", f, m)
		}
	}
	if m := mode(t, filepath.Join(dir, "logs")); m != 0o700 {
		t.Errorf("logs dir mode = %o, want 700", m)
	}
	if pid, err := os.ReadFile(filepath.Join(dir, "athena.pid")); err != nil || strings.TrimSpace(string(pid)) != fmt.Sprint(os.Getpid()) {
		t.Errorf("pid file = %q, %v", pid, err)
	}

	id := s.submit("Fix the thing\nwith more detail")
	if id != "T-0001" {
		t.Fatalf("id = %q", id)
	}
	a := s.running(id)
	var list []TicketSummary
	s.ok(Request{Op: OpTickets}, &list)
	if len(list) != 1 || list[0].ID != id || list[0].Stage != "working" || list[0].Title != "Fix the thing" {
		t.Fatalf("tickets = %+v, want one working", list)
	}

	var resp node.Response
	nodeCall(t, dir, node.ReportRequest{Op: node.OpReport, Ticket: id, Assignment: a.ID, Status: "ready", Summary: "done it"}, &resp)
	if !resp.OK {
		t.Fatalf("report = %+v", resp)
	}
	s.ok(Request{Op: OpTickets}, &list)
	if len(list) != 0 {
		t.Fatalf("open tickets after ready = %+v", list)
	}
	s.ok(Request{Op: OpTickets, All: true}, &list)
	if len(list) != 1 || list[0].Stage != "ended:completed" {
		t.Fatalf("all tickets = %+v, want ended:completed", list)
	}
	d := s.detail(id)
	if d.Stage != "ended:completed" || d.Runtime != "claude" || !strings.HasSuffix(d.Dir, "fix-the-thing-t-0001") {
		t.Fatalf("detail = %+v", d)
	}
	if r := d.Assignments[0].Report; r == nil || r.State != core.ReportReady || r.Summary != "done it" {
		t.Fatalf("report = %+v", r)
	}

	// Logs: one rendered line per event of the transcript; never a raw line.
	var lines []LogLine
	eventually(t, "transcript rendered", func() bool {
		lines = s.logs(id, false)
		return len(lines) > 0 && lines[len(lines)-1].Kind == "other"
	})
	kinds := map[string]bool{}
	for _, l := range lines {
		kinds[l.Kind] = true
		if strings.Contains(l.Text, "{\"") || l.At.IsZero() {
			t.Errorf("bad log line %+v", l)
		}
	}
	for _, k := range []string{"init", "say", "tool", "result", "other"} {
		if !kinds[k] {
			t.Errorf("no %s line in %+v", k, lines)
		}
	}
	if !slices.ContainsFunc(lines, func(l LogLine) bool { return l.Kind == "tool" && strings.HasPrefix(l.Text, "Write ") }) {
		t.Errorf("no Write tool line in %+v", lines)
	}

	// Unknown tickets are a user error.
	if r, err := call(dir, Request{Op: OpTicket, ID: "T-9999"}); err != nil || r.OK || r.Error != "ticket T-9999 not found" {
		t.Fatalf("unknown ticket = %+v, %v", r, err)
	}
	if r, err := call(dir, Request{Op: "nope"}); err != nil || r.OK || !strings.Contains(r.Error, "unknown op") {
		t.Fatalf("unknown op = %+v, %v", r, err)
	}

	s.stop()
	logData, err := os.ReadFile(filepath.Join(dir, "logs", "athena.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), `"type":"ticket.created"`) || !strings.Contains(string(logData), `"ticket":"T-0001"`) {
		t.Errorf("athena.log lacks the ticket.created event:\n%s", logData)
	}
	for _, f := range []string{"athena.sock", "node.sock", "athena.pid"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s left behind after shutdown: %v", f, err)
		}
	}
}

// logs reads a logs stream to its end.
func (s *testServer) logs(id string, follow bool) []LogLine {
	s.t.Helper()
	conn, err := net.DialTimeout("unix", filepath.Join(s.stateDir, "athena.sock"), time.Second)
	if err != nil {
		s.t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(Request{Op: OpLogs, ID: id, Follow: follow}); err != nil {
		s.t.Fatal(err)
	}
	var out []LogLine
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var r struct {
			OK    bool    `json:"ok"`
			Error string  `json:"error"`
			Data  LogLine `json:"data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || !r.OK {
			s.t.Fatalf("logs line %s: %v", sc.Bytes(), err)
		}
		out = append(out, r.Data)
	}
	return out
}

func TestLogsFollowEndsWithTicket(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	s := startServer(t, dir)
	id := s.submit("Follow me")
	a := s.running(id)

	got := make(chan []LogLine, 1)
	go func() { got <- s.logs(id, true) }()
	// The stream stays open while the Ticket runs.
	select {
	case l := <-got:
		t.Fatalf("follow ended while the ticket runs: %+v", l)
	case <-time.After(300 * time.Millisecond):
	}
	var resp node.Response
	nodeCall(t, dir, node.ReportRequest{Op: node.OpReport, Ticket: id, Assignment: a.ID, Status: "dropped", Summary: "nothing to do"}, &resp)
	select {
	case l := <-got:
		if len(l) == 0 || l[0].Kind != "init" {
			t.Fatalf("follow lines = %+v, want the init line", l)
		}
	case <-time.After(waitTimeout):
		t.Fatal("follow did not end with the ticket")
	}

	// A follow stream still open does not hold up shutdown.
	id2 := s.submit("Second")
	s.running(id2)
	conn, err := net.Dial("unix", filepath.Join(dir, "athena.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(Request{Op: OpLogs, ID: id2, Follow: true}); err != nil {
		t.Fatal(err)
	}
	s.stop()
}

// Review Focus 2: a restart resumes the running Assignment's session and delivers what waited for it exactly once.
func TestRestartResumesRunningAssignment(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	s := startServer(t, dir)
	id := s.submit("Long task\nbody-a1b2")
	a := s.running(id)
	if a.SessionID != fixtureSessionID {
		t.Fatalf("SessionID = %q", a.SessionID)
	}
	s.stop()

	// While the server is down, a person's message arrives that never reached the worker.
	offline(t, dir, func(e *core.Engine) {
		if err := e.AddPersonInput(id, "follow-up-c3d4"); err != nil {
			t.Fatal(err)
		}
		tk, _, _ := e.Ticket(id)
		if len(tk.Undelivered) != 1 || tk.LatestAssignment().State != core.AssignmentRunning {
			t.Fatalf("before restart: undelivered %v, assignment %+v", tk.Undelivered, tk.LatestAssignment())
		}
	})

	s = startServer(t, dir)
	eventually(t, "a second claude start", func() bool { return len(fake.invocations(t)) == 2 })
	if rid, ok := argValue(fake.invocations(t)[1], "--resume"); !ok || rid != fixtureSessionID {
		t.Fatalf("second start args = %v, want --resume %s", fake.invocations(t)[1], fixtureSessionID)
	}
	eventually(t, "follow-up delivered", func() bool {
		d := s.detail(id)
		return d.Undelivered == 0 && strings.Contains(fake.stdin(t), "follow-up-c3d4")
	})
	if !strings.Contains(fake.stdin(t), recoveryText) {
		t.Errorf("no recovery note in stdin:\n%s", fake.stdin(t))
	}
	d := s.detail(id)
	if len(d.Assignments) != 1 || d.Assignments[0].ID != a.ID || d.Stage != "working" {
		t.Fatalf("after restart: %+v", d)
	}

	// A further restart sends nothing again.
	s.stop()
	s = startServer(t, dir)
	eventually(t, "a third claude start", func() bool { return len(fake.invocations(t)) == 3 })
	s.running(id)
	time.Sleep(300 * time.Millisecond)
	s.stop()
	in := fake.stdin(t)
	for _, text := range []string{"body-a1b2", "follow-up-c3d4"} {
		if n := strings.Count(in, text); n != 1 {
			t.Errorf("%q sent %d times, want exactly 1:\n%s", text, n, in)
		}
	}
	offline(t, dir, func(e *core.Engine) {
		tk, _, _ := e.Ticket(id)
		for _, inp := range tk.Inputs {
			if inp.DeliveredTo != a.ID {
				t.Errorf("input %+v not delivered to %s", inp, a.ID)
			}
		}
	})
}

func TestRestartStartsPendingAssignment(t *testing.T) {
	fake := newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	var id string
	offline(t, dir, func(e *core.Engine) {
		tk, err := e.SubmitOwnerTask("Queued while down\nbody-e5f6", core.TaskOptions{})
		if err != nil {
			t.Fatal(err)
		}
		id = tk.ID
	})
	s := startServer(t, dir)
	s.running(id)
	if invs := fake.invocations(t); len(invs) != 1 || slices.Contains(invs[0], "--resume") {
		t.Fatalf("invocations = %v, want one fresh start", invs)
	}
	if n := strings.Count(fake.stdin(t), "body-e5f6"); n != 1 {
		t.Fatalf("task sent %d times", n)
	}
}

func TestSingleInstanceAndStaleSocket(t *testing.T) {
	newFakeEnv(t, openTurnFixture(t))
	dir := newStateDir(t)
	s := startServer(t, dir)
	err := Run(context.Background(), testConfig(t, dir))
	if err == nil || err.Error() != "athena server already running" {
		t.Fatalf("second Run = %v, want already running", err)
	}
	s.stop()

	// A socket nobody answers on is stale and removed; a loose state dir is tightened.
	sock := filepath.Join(dir, "athena.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s = startServer(t, dir)
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("state dir mode = %o, want 700", m)
	}
	s.stop()

	// Anything else at the socket path is left alone.
	if err := os.WriteFile(sock, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), testConfig(t, dir))
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("Run with a file at the socket path = %v", err)
	}
	if b, _ := os.ReadFile(sock); string(b) != "not a socket" {
		t.Fatal("file at the socket path was touched")
	}
}

func TestStageString(t *testing.T) {
	for _, c := range []struct {
		t    core.Ticket
		want string
	}{
		{core.Ticket{Ended: &core.Ended{Outcome: core.OutcomeCompleted}}, "ended:completed"},
		{core.Ticket{Attention: &core.Attention{Question: "?"}, Assignments: []core.Assignment{{State: core.AssignmentRunning}}}, "person/answer"},
		{core.Ticket{Assignments: []core.Assignment{{State: core.AssignmentRunning}}}, "working"},
		{core.Ticket{Assignments: []core.Assignment{{State: core.AssignmentPending}}}, "working/queued"},
		{core.Ticket{Assignments: []core.Assignment{{State: core.AssignmentEnded}}}, "idle"},
	} {
		if got := StageString(&c.t); got != c.want {
			t.Errorf("StageString(%+v) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestUserAndInternalErrors(t *testing.T) {
	if r := errorResponse(&core.UserError{Msg: "nope"}, nil); r.OK || r.Error != "nope" {
		t.Fatalf("user error = %+v", r)
	}
	if r := errorResponse(errors.New("disk on fire"), discardLogger()); r.OK || r.Error != "internal error: disk on fire" {
		t.Fatalf("internal error = %+v", r)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
