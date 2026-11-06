package node

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/quota"
	"github.com/danielpina1/athena/internal/render"
	"github.com/danielpina1/athena/internal/runtime/claude"
	"github.com/danielpina1/athena/internal/runtime/codex"
	"github.com/danielpina1/athena/internal/runtime/rt"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/prompts"
)

const (
	fixtureSessionID = "27abadfa-c58f-4e97-aaf3-64a8fdaff179"
	waitTimeout      = 10 * time.Second
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

// harness is a real store, a real Engine and a Node running fakeclaude, serving its socket.
type harness struct {
	t        *testing.T
	n        *Node
	eng      *core.Engine
	st       *store.Store
	cfg      Config
	stdinLog string
	argsLog  string

	evMu   sync.Mutex
	events []core.Event
}

func newHarness(t *testing.T, mod func(*Config)) *harness {
	t.Helper()
	return newHarnessWith(t, mod, nil)
}

// newHarnessWith is newHarness with the Engine made by mk (default core.NewEngine).
func newHarnessWith(t *testing.T, mod func(*Config), mk func(st *store.Store, n *Node, emit func(core.Event)) *core.Engine) *harness {
	t.Helper()
	base := t.TempDir()
	// Unix socket paths are short on macOS; keep it out of the long test temp dir.
	sockDir, err := os.MkdirTemp("/tmp", "athn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	h := &harness{t: t, stdinLog: filepath.Join(base, "stdin.log"), argsLog: filepath.Join(base, "args.log")}
	t.Setenv("ATHENA_FAKE_FIXTURE", claudeFixture(t, "simple-turn.jsonl"))
	t.Setenv("ATHENA_FAKE_STDIN_LOG", h.stdinLog)
	t.Setenv("ATHENA_FAKE_ARGS_APPEND", h.argsLog)
	h.cfg = Config{
		WorkRoot:        filepath.Join(base, "work"),
		StateDir:        filepath.Join(base, "state"),
		AthenaBin:       "/usr/bin/true",
		SocketPath:      filepath.Join(sockDir, "n.sock"),
		ClaudeBinary:    fakeClaude(t),
		Blocked:         []string{"rm", "git push"},
		ApprovalTimeout: 5 * time.Second,
	}
	if mod != nil {
		mod(&h.cfg)
	}
	st, err := store.Open(filepath.Join(base, "state", "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	h.st = st
	h.n = New(h.cfg, nil)
	h.n.restartDelay = func(int) time.Duration { return 0 }
	emit := func(e core.Event) {
		h.evMu.Lock()
		h.events = append(h.events, e)
		h.evMu.Unlock()
	}
	if mk == nil {
		h.eng = core.NewEngine(st.Repos(), h.n, nil, emit)
	} else {
		h.eng = mk(st, h.n, emit)
	}
	h.n.SetEngine(h.eng)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- h.n.Serve(ctx) }()
	t.Cleanup(func() {
		h.n.Close()
		cancel()
		select {
		case <-served:
		case <-time.After(waitTimeout):
			t.Error("Serve did not return after cancel")
		}
		st.Close()
	})
	h.eventually("socket answers ping", func() bool {
		var r Response
		return callNode(h.cfg.SocketPath, PingRequest{Op: OpPing}, &r) == nil && r.OK
	})
	return h
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// never fails if cond becomes true within d.
func (h *harness) never(what string, d time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			h.t.Fatalf("unexpected: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) submit(text, dir string) core.Ticket {
	h.t.Helper()
	tk, err := h.eng.SubmitOwnerTask(text, core.TaskOptions{Dir: dir})
	if err != nil {
		h.t.Fatal(err)
	}
	return tk
}

func (h *harness) ticket(id string) (core.Ticket, core.Stage) {
	h.t.Helper()
	tk, st, err := h.eng.Ticket(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return tk, st
}

func (h *harness) assignmentState(id string) core.AssignmentState {
	tk, _ := h.ticket(id)
	return tk.LatestAssignment().State
}

// running waits until the Ticket's latest Assignment runs and has a session, and returns it.
func (h *harness) running(id string) (core.Ticket, core.Assignment) {
	h.t.Helper()
	var tk core.Ticket
	h.eventually(id+" running", func() bool {
		tk, _ = h.ticket(id)
		a := tk.LatestAssignment()
		return a.State == core.AssignmentRunning && a.SessionID != "" && len(tk.Undelivered) == 0
	})
	return tk, *tk.LatestAssignment()
}

type invocation struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

func (h *harness) invocations() []invocation {
	h.t.Helper()
	f, err := os.Open(h.argsLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	var out []invocation
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var inv invocation
		if err := json.Unmarshal(sc.Bytes(), &inv); err != nil {
			h.t.Fatalf("args log: %v", err)
		}
		out = append(out, inv)
	}
	return out
}

func (h *harness) stdin() string {
	b, err := os.ReadFile(h.stdinLog)
	if err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
	return string(b)
}

func (h *harness) hook(event, ticket, assignment string, input any) HookResponse {
	h.t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		h.t.Fatal(err)
	}
	var resp HookResponse
	if err := callNode(h.cfg.SocketPath, HookRequest{Op: OpHook, Event: event, Ticket: ticket, Assignment: assignment, Input: raw}, &resp); err != nil {
		h.t.Fatalf("hook %s: %v", event, err)
	}
	return resp
}

func (h *harness) report(ticket, assignment string, status core.ReportState, summary string) Response {
	h.t.Helper()
	var resp Response
	req := ReportRequest{Op: OpReport, Ticket: ticket, Assignment: assignment, Status: string(status), Summary: summary}
	if err := callNode(h.cfg.SocketPath, req, &resp); err != nil {
		h.t.Fatalf("report: %v", err)
	}
	return resp
}

func (h *harness) eventsOf(typ string) []core.Event {
	h.evMu.Lock()
	defer h.evMu.Unlock()
	var out []core.Event
	for _, e := range h.events {
		if e.Type == typ {
			out = append(out, e)
		}
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

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README")
	git(t, repo, "commit", "-q", "-m", "init")
	return repo
}

// --- tests ---

func TestTicketDir(t *testing.T) {
	tk := core.Ticket{ID: "T-0042", Title: "  Fix: the *Login* page, now!  "}
	if got, want := TicketDir("/w", tk), "/w/fix-the-login-page-now-t-0042"; got != want {
		t.Fatalf("TicketDir = %q, want %q", got, want)
	}
	if got, want := TicketDir("/w", core.Ticket{ID: "T-1", Title: "!!!"}), "/w/ticket-t-1"; got != want {
		t.Fatalf("TicketDir(no slug) = %q, want %q", got, want)
	}
	long := core.Ticket{ID: "T-2", Title: strings.Repeat("abc ", 40)}
	if base := filepath.Base(TicketDir("/w", long)); len(base) > 60 || strings.Contains(base, "--") || !strings.HasSuffix(base, "-t-2") {
		t.Fatalf("TicketDir(long) base = %q", base)
	}
}

func TestStartRunsSessionAndReportsStarted(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Fix the thing\nwith more detail", "")
	tk, a := h.running(tk.ID)
	if a.SessionID != fixtureSessionID {
		t.Fatalf("SessionID = %q, want %q", a.SessionID, fixtureSessionID)
	}
	if a.Model != "sonnet" {
		t.Fatalf("Model = %q, want the node's default sonnet (the task named none)", a.Model)
	}
	if in := tk.Inputs[0]; in.DeliveredTo != a.ID || in.DeliveredAt == nil {
		t.Fatalf("first input not marked delivered: %+v", in)
	}
	h.eventually("first input read", func() bool {
		tk, _ := h.ticket(tk.ID)
		return tk.Inputs[0].ReadAt != nil
	})
	dir := TicketDir(h.cfg.WorkRoot, tk)
	if want := filepath.Join(h.cfg.WorkRoot, "fix-the-thing-t-0001"); dir != want {
		t.Fatalf("TicketDir = %q, want %q", dir, want)
	}
	if wd, err := WorkingDir(h.cfg.WorkRoot, tk); err != nil || wd != dir {
		t.Fatalf("WorkingDir = %q, %v; want %q", wd, err, dir)
	}
	invs := h.invocations()
	if len(invs) != 1 {
		t.Fatalf("claude started %d times, want 1", len(invs))
	}
	args := invs[0].Args
	for flag, want := range map[string]string{
		"--settings":             filepath.Join(dir, ".athena", "settings.json"),
		"--mcp-config":           filepath.Join(dir, ".athena", "mcp.json"),
		"--model":                "sonnet",
		"--append-system-prompt": prompts.Worker,
	} {
		if got, ok := argValue(args, flag); !ok || got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	if slices.Contains(args, "--resume") {
		t.Errorf("first start resumed: %v", args)
	}
	if !slices.Contains(args, "--replay-user-messages") {
		t.Errorf("worker session does not echo its input: %v", args)
	}
	for k, want := range map[string]string{
		"ATHENA_TICKET":           tk.ID,
		"ATHENA_ASSIGNMENT":       a.ID,
		"ATHENA_SOCKET":           h.cfg.SocketPath,
		"ATHENA_DIR":              dir,
		"ATHENA_NAME":             "fix-the-thing-t-0001",
		"ATHENA_BLOCKED_COMMANDS": "rm,git push",
	} {
		if got := invs[0].Env[k]; got != want {
			t.Errorf("env %s = %q, want %q", k, got, want)
		}
	}
	for _, f := range []string{"settings.json", "mcp.json"} {
		st, err := os.Stat(filepath.Join(dir, ".athena", f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v mode %v, want 0600", f, err, st)
		}
	}
	if got := h.stdin(); !strings.Contains(got, "with more detail") || !strings.Contains(got, tk.ID) {
		t.Errorf("first message lacks the task: %q", got)
	}
	var mcpCfg struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if raw, err := os.ReadFile(filepath.Join(dir, ".athena", "mcp.json")); err != nil || json.Unmarshal(raw, &mcpCfg) != nil {
		t.Fatalf("mcp.json: %v", err)
	}
	if env := mcpCfg.MCPServers["athena"].Env; env["ATHENA_ASSIGNMENT"] != a.ID || env["ATHENA_TICKET"] != tk.ID {
		t.Errorf("mcp.json env = %v, want ATHENA_ASSIGNMENT=%s ATHENA_TICKET=%s", env, a.ID, tk.ID)
	}
	if ign, err := os.ReadFile(filepath.Join(dir, ".athena", ".gitignore")); err != nil || strings.TrimSpace(string(ign)) != "*" {
		t.Errorf(".athena/.gitignore = %q, %v", ign, err)
	}
}

func TestStartIsIdempotentAndDeliverCoalesces(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	tk, a := h.running(tk.ID)
	h.n.Start(tk, a)
	h.n.Start(tk, a)
	h.n.Deliver(tk, a)
	h.n.Deliver(tk, a)
	h.never("a second claude process", 300*time.Millisecond, func() bool { return len(h.invocations()) > 1 })
	// The fixture's turn ends without a report, so the worker is also nudged once; the task itself goes once.
	in := h.stdin()
	if n := strings.Count(in, "\n") - strings.Count(in, nudgeText); n != 1 {
		t.Fatalf("stdin got %d lines besides the nudge, want 1 (no duplicate delivery):\n%s", n, in)
	}
}

func TestReportOpEndsTicketAndStopsSession(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	if r := h.report(tk.ID, "other", core.ReportReady, "done"); r.OK || r.Error == "" {
		t.Fatalf("report from a stranger = %+v, want an error", r)
	}
	if r := h.report(tk.ID, a.ID, core.ReportReady, "done"); !r.OK {
		t.Fatalf("report = %+v", r)
	}
	tk, _ = h.ticket(tk.ID)
	if tk.Ended == nil || tk.Ended.Outcome != core.OutcomeCompleted {
		t.Fatalf("ticket not completed: %+v", tk.Ended)
	}
	h.eventually("session gone", func() bool { return h.n.liveCount() == 0 })
}

// TestReadyReportRefusedUntilFailureSeen: the real CLI (haiku) filed a ready report in the same model step as a Write
// the owner denied, so the Ticket ended completed with a false claim. A result claim made before the model has taken
// a step after a failed tool call is refused; progress and blocked reports are not.
func TestReadyReportRefusedUntilFailureSeen(t *testing.T) {
	h := newHarness(t, nil)
	// The turn stays open: the session's own turn end would clear what this test's runner records.
	t.Setenv("ATHENA_FAKE_FIXTURE", openTurnFixture(t))
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	s := h.n.lookup(tk.ID, a.ID)
	if s == nil {
		t.Fatal("no live session")
	}
	r := &runner{n: h.n, s: s, eng: h.eng, log: s.log, sent: map[string]bool{}}
	r.onEvent(rt.ToolUse{ID: "w1", Name: "Write"})
	r.onEvent(rt.ToolUse{ID: "r1", Name: "mcp__athena__report"})
	r.onEvent(rt.ToolResult{ID: "w1", IsError: true})
	for _, st := range []core.ReportState{core.ReportReady, core.ReportDropped} {
		if got := h.report(tk.ID, a.ID, st, "wrote it"); got.OK || !strings.Contains(got.Error, "failed") {
			t.Fatalf("%s report = %+v, want a refusal", st, got)
		}
	}
	if tk, _ := h.ticket(tk.ID); tk.Ended != nil {
		t.Fatalf("ticket ended: %+v", tk.Ended)
	}
	if got := h.report(tk.ID, a.ID, core.ReportBlocked, "denied"); !got.OK {
		t.Fatalf("blocked report = %+v", got)
	}
	// The model's next step has seen the failure.
	r.onEvent(rt.AssistantText{Text: "The write was denied."})
	if got := h.report(tk.ID, a.ID, core.ReportReady, "done without it"); !got.OK {
		t.Fatalf("ready report after the next step = %+v", got)
	}
}

// The node's own deny reaches the hook before the failed tool result reaches stdout: a result claim filed in the same
// step is refused from the deny on, even while events of that step written before the result are still read.
func TestHookDenialRefusesSameStepReport(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_FIXTURE", openTurnFixture(t))
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	s := h.n.lookup(tk.ID, a.ID)
	r := &runner{n: h.n, s: s, eng: h.eng, log: s.log, sent: map[string]bool{}}
	if v := h.hook(EventPreToolUse, tk.ID, a.ID, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf build"}}); v.Verdict != VerdictDeny {
		t.Fatalf("verdict = %+v, want deny", v)
	}
	if got := h.report(tk.ID, a.ID, core.ReportReady, "built"); got.OK || !strings.Contains(got.Error, "failed") {
		t.Fatalf("ready report right after the deny = %+v, want a refusal", got)
	}
	// The step's own tool_use line, read late, is not a later step.
	r.onEvent(rt.ToolUse{ID: "b1", Name: "Bash"})
	r.onEvent(rt.ToolUse{ID: "r1", Name: "mcp__athena__report"})
	if got := h.report(tk.ID, a.ID, core.ReportReady, "built"); got.OK {
		t.Fatalf("ready report before the failed result was seen = %+v, want a refusal", got)
	}
	r.onEvent(rt.ToolResult{ID: "b1", IsError: true})
	r.onEvent(rt.AssistantText{Text: "rm is blocked."})
	if got := h.report(tk.ID, a.ID, core.ReportReady, "built without it"); !got.OK {
		t.Fatalf("ready report after the next step = %+v", got)
	}
}

func TestEscalationCreatesApprovalAndWaits(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	input := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "make deploy"}}
	got := make(chan HookResponse, 1)
	go func() { got <- h.hook(EventPreToolUse, tk.ID, a.ID, input) }()
	var pending []core.Approval
	h.eventually("approval pending", func() bool {
		var err error
		pending, err = h.eng.PendingApprovals()
		return err == nil && len(pending) == 1
	})
	ap := pending[0]
	if ap.TicketID != tk.ID || ap.AssignmentID != a.ID || ap.Tool != "Bash" || ap.Rule != "bash-escalate:deploy" || !strings.Contains(ap.Summary, "make deploy") {
		t.Fatalf("approval = %+v", ap)
	}
	if _, st := h.ticket(tk.ID); st != (core.Stage{Stage: "person", Action: "approval"}) {
		t.Fatalf("stage = %+v, want person/approval", st)
	}
	select {
	case r := <-got:
		t.Fatalf("hook answered before the person: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	if err := h.eng.AnswerApproval(ap.ID, true, "go ahead"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.Verdict != VerdictAllow {
			t.Fatalf("verdict = %+v, want allow", r)
		}
	case <-time.After(waitTimeout):
		t.Fatal("hook never answered")
	}
}

func TestEscalationDeniedByPerson(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	got := make(chan HookResponse, 1)
	input := map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "kubectl apply -f x"}}
	go func() { got <- h.hook(EventPermissionRequest, tk.ID, a.ID, input) }()
	var pending []core.Approval
	h.eventually("approval pending", func() bool {
		pending, _ = h.eng.PendingApprovals()
		return len(pending) == 1
	})
	if err := h.eng.AnswerApproval(pending[0].ID, false, "not today"); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.Verdict != VerdictDeny || !strings.Contains(r.Reason, "not today") {
		t.Fatalf("verdict = %+v, want deny with the note", r)
	}
}

func TestApprovalTimeoutDenies(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ApprovalTimeout = 50 * time.Millisecond })
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	input := map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "./deploy.sh"}}
	r := h.hook(EventPreToolUse, tk.ID, a.ID, input)
	if r.Verdict != VerdictDeny || r.Reason == "" {
		t.Fatalf("verdict = %+v, want deny with a reason", r)
	}
	req := h.eventsOf(core.EventApprovalRequested)
	if len(req) != 1 {
		t.Fatalf("approval.requested events = %d, want 1", len(req))
	}
	var ap *core.Approval
	if err := h.st.Tx(func(tx *store.Tx) (err error) { ap, err = tx.GetApproval(req[0].ApprovalID); return err }); err != nil {
		t.Fatal(err)
	}
	if ap == nil || ap.State != core.ApprovalExpired {
		t.Fatalf("approval = %+v, want expired", ap)
	}
	if _, st := h.ticket(tk.ID); st.Stage != "working" {
		t.Fatalf("stage = %+v, want working after the expiry", st)
	}
}

// An escalation dies with its session: when a retry stops the Assignment, its pending Approval expires at once and the
// hook is denied, instead of the Ticket showing person/approval until the Approval times out.
func TestEscalationExpiresWhenSessionStops(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ApprovalTimeout = time.Minute })
	t.Setenv("ATHENA_FAKE_FIXTURE", openTurnFixture(t))
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	got := make(chan HookResponse, 1)
	go func() {
		got <- h.hook(EventPreToolUse, tk.ID, a.ID, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "make deploy"}})
	}()
	var pending []core.Approval
	h.eventually("approval pending", func() bool {
		pending, _ = h.eng.PendingApprovals()
		return len(pending) == 1
	})
	if r := h.report(tk.ID, a.ID, core.ReportBlocked, "stuck"); !r.OK {
		t.Fatalf("report = %+v", r)
	}
	if err := h.eng.RetryTicket(tk.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.Verdict != VerdictDeny {
			t.Fatalf("verdict = %+v, want deny", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hook kept waiting after its session stopped")
	}
	var ap *core.Approval
	if err := h.st.Tx(func(tx *store.Tx) (err error) { ap, err = tx.GetApproval(pending[0].ID); return err }); err != nil {
		t.Fatal(err)
	}
	if ap == nil || ap.State != core.ApprovalExpired {
		t.Fatalf("approval = %+v, want expired", ap)
	}
	if _, st := h.ticket(tk.ID); st.Stage == "person" && st.Action == "approval" {
		t.Fatalf("stage = %+v after the retry", st)
	}
}

// expireFails is the real Engine except that ExpireApproval fails, as it would with the store gone.
type expireFails struct{ *core.Engine }

func (expireFails) ExpireApproval(string) error { return fmt.Errorf("store closed") }

func TestEscalationDeniesPromptlyWhenExpiryFails(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ApprovalTimeout = 50 * time.Millisecond })
	h.n.SetEngine(expireFails{h.eng})
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	got := make(chan HookResponse, 1)
	go func() {
		got <- h.hook(EventPreToolUse, tk.ID, a.ID, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "make deploy"}})
	}()
	select {
	case r := <-got:
		if r.Verdict != VerdictDeny {
			t.Fatalf("verdict = %+v, want deny", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook hung after the expiry failed")
	}
	// The approval is still pending (expiry failed); Serve must still shut down (checked by the harness cleanup).
	if pending, _ := h.eng.PendingApprovals(); len(pending) != 1 {
		t.Fatalf("pending approvals = %d, want 1", len(pending))
	}
}

func TestHookDecisions(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	tk, a := h.running(tk.ID)
	dir := TicketDir(h.cfg.WorkRoot, tk)
	home, _ := os.UserHomeDir()
	cases := []struct {
		name, event, tool string
		input             map[string]any
		want              string
	}{
		{"write inside", EventPreToolUse, "Write", map[string]any{"file_path": filepath.Join(dir, "a.txt"), "content": "x"}, VerdictAllow},
		{"blocked command", EventPreToolUse, "Bash", map[string]any{"command": "echo hi | rm -rf /"}, VerdictDeny},
		{"credential read", EventPreToolUse, "Read", map[string]any{"file_path": filepath.Join(home, ".ssh", "id_rsa")}, VerdictDeny},
		{"state dir read", EventPermissionRequest, "Read", map[string]any{"file_path": filepath.Join(h.cfg.StateDir, "athena.db")}, VerdictDeny},
		{"session config write", EventPreToolUse, "Write", map[string]any{"file_path": filepath.Join(dir, ".athena", "settings.json"), "content": "{}"}, VerdictDeny},
	}
	for _, c := range cases {
		r := h.hook(c.event, tk.ID, a.ID, map[string]any{"tool_name": c.tool, "tool_input": c.input})
		if r.Verdict != c.want {
			t.Errorf("%s: verdict = %+v, want %s", c.name, r, c.want)
		}
	}
	if r := h.hook(EventPreToolUse, tk.ID, a.ID, map[string]any{"tool_name": "Bash"}); r.Verdict != VerdictDeny {
		t.Errorf("malformed input: %+v, want deny", r)
	}
	if r := h.hook(EventPostToolUse, tk.ID, a.ID, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}}); r.Verdict != VerdictAllow {
		t.Errorf("PostToolUse: %+v, want allow", r)
	}
	if pending, _ := h.eng.PendingApprovals(); len(pending) != 0 {
		t.Errorf("unexpected approvals: %+v", pending)
	}
}

func TestHookRejectsUnknownSession(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	write := map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": "/tmp/x", "content": "x"}}
	for _, ids := range [][2]string{{tk.ID, "nope"}, {"T-9999", a.ID}, {"", ""}} {
		if r := h.hook(EventPreToolUse, ids[0], ids[1], write); r.Verdict != VerdictDeny {
			t.Errorf("PreToolUse from %v: %+v, want deny", ids, r)
		}
		if r := h.hook(EventStop, ids[0], ids[1], map[string]any{}); r.Verdict != VerdictAllow {
			t.Errorf("Stop from %v: %+v, want allow (never block Stop)", ids, r)
		}
	}
	if pending, _ := h.eng.PendingApprovals(); len(pending) != 0 {
		t.Errorf("a stranger's hook created approvals: %+v", pending)
	}
	tk, _ = h.ticket(tk.ID)
	if tk.LatestAssignment().IdleAt != nil {
		t.Error("a stranger's Stop ended the turn")
	}
	var r Response
	if err := callNode(h.cfg.SocketPath, map[string]string{"op": "bogus"}, &r); err != nil || r.OK || r.Error == "" {
		t.Errorf("unknown op: %+v, %v; want an error", r, err)
	}
}

func TestStopHookEndsTurn(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	if r := h.hook(EventStop, tk.ID, a.ID, map[string]any{"hook_event_name": "Stop", "stop_hook_active": false}); r.Verdict != VerdictAllow {
		t.Fatalf("Stop verdict = %+v, want allow", r)
	}
	tk, _ = h.ticket(tk.ID)
	if tk.LatestAssignment().IdleAt == nil {
		t.Fatal("Stop did not record the turn's end")
	}
}

func TestSessionStartContext(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	tk, a := h.running(tk.ID)
	r := h.hook(EventSessionStart, tk.ID, a.ID, map[string]any{"hook_event_name": "SessionStart", "source": "startup"})
	if r.Verdict != VerdictAllow {
		t.Fatalf("verdict = %+v", r)
	}
	dir := TicketDir(h.cfg.WorkRoot, tk)
	for _, want := range []string{"ATHENA_TICKET=" + tk.ID, "ATHENA_DIR=" + dir, "ATHENA_NAME=task-t-0001", "ATHENA_BLOCKED_COMMANDS=rm,git push", "Mandate", "explicit", "owner"} {
		if !strings.Contains(r.AdditionalContext, want) {
			t.Errorf("additionalContext lacks %q:\n%s", want, r.AdditionalContext)
		}
	}
}

// Review Focus 1: a Claude process that dies mid-turn is resumed with the same session id and a recovery note.
func TestCrashResumesSameSession(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_EXIT_MID_TURN_ONCE", filepath.Join(t.TempDir(), "crashed"))
	tk := h.submit("Crashy task\nbody-7f3e", "")
	h.eventually("two claude starts", func() bool { return len(h.invocations()) == 2 })
	second := h.invocations()[1].Args
	if id, ok := argValue(second, "--resume"); !ok || id != fixtureSessionID {
		t.Fatalf("second start args = %v, want --resume %s", second, fixtureSessionID)
	}
	h.eventually("recovery note sent", func() bool { return strings.Contains(h.stdin(), recoveryNote) })
	tk, a := h.running(tk.ID)
	if _, st := h.ticket(tk.ID); st.Stage != "working" {
		t.Fatalf("stage = %+v, want working", st)
	}
	if n := h.n.liveCount(); n != 1 {
		t.Fatalf("live sessions = %d, want 1 (never working without a process)", n)
	}
	// The process died before its turn ended or it reported, so the task may not be in its history: it goes again,
	// once, after the recovery note.
	if n := strings.Count(h.stdin(), "body-7f3e"); n != 2 {
		t.Fatalf("task sent %d times, want 2", n)
	}
	h.never("a third start", 200*time.Millisecond, func() bool { return len(h.invocations()) > 2 })
	if r := h.report(tk.ID, a.ID, core.ReportReady, "done"); !r.OK {
		t.Fatalf("report after recovery = %+v", r)
	}
}

func TestCrashLoopReportsFailed(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_EXIT_MID_TURN", "1")
	tk := h.submit("Doomed task", "")
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(tk.ID)
		r := tk.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	tk, st := h.ticket(tk.ID)
	if got := tk.LatestAssignment().Report.Summary; !strings.HasPrefix(got, "session crashed repeatedly: exit status 3") {
		t.Fatalf("summary = %q", got)
	}
	if st != (core.Stage{Stage: "person", Action: "failed"}) {
		t.Fatalf("stage = %+v, want person/failed", st)
	}
	if n := len(h.invocations()); n != 1+maxRestarts {
		t.Fatalf("claude started %d times, want %d", n, 1+maxRestarts)
	}
	h.eventually("session gone", func() bool { return h.n.liveCount() == 0 })

	// A person's message wakes the worker again: the session resumes instead of the input sitting undelivered.
	t.Setenv("ATHENA_FAKE_EXIT_MID_TURN", "")
	if err := h.eng.AddPersonInput(tk.ID, "please try again"); err != nil {
		t.Fatal(err)
	}
	h.eventually("message delivered to a new process", func() bool {
		tk, _ := h.ticket(tk.ID)
		return len(tk.Undelivered) == 0 && strings.Contains(h.stdin(), "please try again") && h.n.liveCount() == 1
	})
}

// fixtureLines writes the lines of a captured fixture that match keep, in order, to a new fixture and returns its path.
func fixtureLines(t *testing.T, name string, keep func(i int, line string) bool) string {
	t.Helper()
	b, err := os.ReadFile(claudeFixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for i, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if keep(i, l) {
			out.WriteString(l + "\n")
		}
	}
	p := filepath.Join(t.TempDir(), "part-"+name)
	if err := os.WriteFile(p, []byte(out.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// An Input counts as read only once a turn ends (a Result) or the session files a Report after it was sent: the CLI
// does not echo user messages, so a step that was already in flight says nothing about it. A crash before either
// sends it again.
func TestInputReadOnlyAtTurnEndOrReport(t *testing.T) {
	h := newHarness(t, nil)
	isText := func(l string) bool {
		return strings.Contains(l, `"type":"assistant"`) && strings.Contains(l, `"type":"text"`)
	}
	textSeen := false
	// A turn that takes one step (text) and never ends; each later message gets another step, then the first process dies.
	t.Setenv("ATHENA_FAKE_FIXTURE", fixtureLines(t, "simple-turn.jsonl", func(i int, l string) bool {
		if i == 0 {
			return true
		}
		if isText(l) && !textSeen {
			textSeen = true
			return true
		}
		return false
	}))
	t.Setenv("ATHENA_FAKE_LATER_FIXTURE", fixtureLines(t, "simple-turn.jsonl", func(_ int, l string) bool { return isText(l) }))
	t.Setenv("ATHENA_FAKE_LATER_EXIT_ONCE", filepath.Join(t.TempDir(), "died"))
	tk := h.submit("Stepping task\nbody-5e6f", "")
	h.running(tk.ID)
	if err := h.eng.AddPersonInput(tk.ID, "mid-turn-q9"); err != nil {
		t.Fatal(err)
	}
	h.eventually("a second claude start", func() bool { return len(h.invocations()) == 2 })
	h.eventually("inputs sent again", func() bool { return strings.Count(h.stdin(), "mid-turn-q9") == 2 })
	if n := strings.Count(h.stdin(), "body-5e6f"); n != 2 {
		t.Errorf("task sent %d times, want 2 (no turn end or report followed it before the crash)", n)
	}
	tk, a := h.running(tk.ID)
	for _, in := range tk.Inputs {
		if in.ReadAt != nil {
			t.Errorf("input %q marked read without a turn end or report", in.Text)
		}
	}
	// A report filed after they were sent: the session has taken them in.
	if r := h.report(tk.ID, a.ID, core.ReportProgress, "on it"); !r.OK {
		t.Fatalf("report = %+v", r)
	}
	h.eventually("inputs read after the report", func() bool {
		tk, _ := h.ticket(tk.ID)
		return tk.Inputs[0].ReadAt != nil && tk.Inputs[1].ReadAt != nil
	})
}

// A turn that ends in an error (here a real `error_max_turns` result; also an expired login) leaves the CLI waiting
// with stdin open: the Assignment reports failed with the subtype and the reason instead of staying working. (A usage
// limit is no failure: the Ticket fails over, TestClaudeLimitFailsOverToCodex.)
func TestErrorResultReportsFailed(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_FIXTURE", claudeFixture(t, "max-turns.jsonl"))
	tk := h.submit("Two files", "")
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(tk.ID)
		r := tk.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	tk, st := h.ticket(tk.ID)
	got := tk.LatestAssignment().Report.Summary
	if !strings.Contains(got, "error_max_turns") || !strings.Contains(got, "Reached maximum number of turns (1)") {
		t.Fatalf("summary = %q, want the subtype and the reason", got)
	}
	if st != (core.Stage{Stage: "person", Action: "failed"}) {
		t.Fatalf("stage = %+v, want person/failed", st)
	}
	if n := len(h.invocations()); n != 1 {
		t.Fatalf("claude started %d times, want 1 (the process lives on)", n)
	}
	if strings.Contains(h.stdin(), nudgeText) {
		t.Fatal("a failed turn was nudged")
	}
}

// The documented shape of an API failure: is_error with subtype success and the message in "result". Its text is
// masked before it reaches the report.
func TestErrorResultTextIsRedacted(t *testing.T) {
	h := newHarness(t, nil)
	init, _, _ := strings.Cut(string(must(os.ReadFile(claudeFixture(t, "simple-turn.jsonl")))), "\n")
	fx := filepath.Join(t.TempDir(), "api-error.jsonl")
	result := `{"type":"result","subtype":"success","is_error":true,"session_id":"` + fixtureSessionID +
		`","num_turns":1,"result":"API Error: 401 authorization: Bearer sk-ant-s3cret is invalid · Please run /login"}`
	if err := os.WriteFile(fx, []byte(init+"\n"+result+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATHENA_FAKE_FIXTURE", fx)
	tk := h.submit("Doomed", "")
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(tk.ID)
		r := tk.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	tk, _ = h.ticket(tk.ID)
	got := tk.LatestAssignment().Report.Summary
	if strings.Contains(got, "sk-ant-s3cret") || !strings.Contains(got, "API Error: 401") || !strings.Contains(got, "Please run /login") {
		t.Fatalf("summary = %q, want the masked API error", got)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// A turn that ends without a Report since the worker's last Input gets one nudge; a second such turn reports blocked.
func TestTurnWithoutReportIsNudgedThenBlocked(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_LATER_FIXTURE", fixtureLines(t, "simple-turn.jsonl", func(_ int, l string) bool {
		return strings.Contains(l, `"type":"result"`)
	}))
	tk := h.submit("Silent task", "")
	h.eventually("blocked report", func() bool {
		tk, _ := h.ticket(tk.ID)
		r := tk.LatestAssignment().Report
		return r != nil && r.State == core.ReportBlocked
	})
	tk, st := h.ticket(tk.ID)
	if got := tk.LatestAssignment().Report.Summary; got != "worker ended its turn twice without reporting" {
		t.Fatalf("summary = %q", got)
	}
	if st != (core.Stage{Stage: "person", Action: "blocked"}) {
		t.Fatalf("stage = %+v, want person/blocked", st)
	}
	if n := strings.Count(h.stdin(), nudgeText); n != 1 {
		t.Fatalf("nudged %d times, want 1:\n%s", n, h.stdin())
	}
}

// A turn that ends after the worker reported since its last Input is not nudged.
func TestReportedTurnIsNotNudged(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_FIXTURE", openTurnFixture(t))
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	s := h.n.lookup(tk.ID, a.ID)
	var sent []string
	r := &runner{n: h.n, s: s, eng: h.eng, log: s.log, sent: map[string]bool{}, started: true, initSeen: true,
		send: func(text string) error { sent = append(sent, text); return nil }}
	r.inputSent()
	if got := h.report(tk.ID, a.ID, core.ReportProgress, "on it"); !got.OK {
		t.Fatalf("report = %+v", got)
	}
	r.onEvent(rt.TurnEnd{OK: true, Subtype: "success"})
	r.onEvent(rt.TurnEnd{OK: true, Subtype: "success"})
	if len(sent) != 0 {
		t.Fatalf("sent %q after a reported turn", sent)
	}
	// A new Input without a Report: the turn's end is nudged once.
	r.inputSent()
	r.onEvent(rt.TurnEnd{OK: true, Subtype: "success"})
	if len(sent) != 1 || sent[0] != nudgeText {
		t.Fatalf("sent %q, want one nudge", sent)
	}
	if tk, _ := h.ticket(tk.ID); tk.LatestAssignment().Report.State != core.ReportProgress {
		t.Fatalf("report = %+v, want the progress report kept", tk.LatestAssignment().Report)
	}
}

// openTurnFixture is a turn that never ends: the init line only.
func openTurnFixture(t *testing.T) string {
	t.Helper()
	return fixtureLines(t, "simple-turn.jsonl", func(i int, _ string) bool { return i == 0 })
}

// A crash loop's failed report says why the process ended: its exit status and the end of its stderr, secrets masked.
func TestCrashReportCarriesCause(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_EXIT_MID_TURN", "1")
	t.Setenv("ATHENA_FAKE_STDERR", "starting up\n\nfatal: ANTHROPIC_API_KEY=sk-ant-s3cret rejected by the API\n\n")
	tk := h.submit("Doomed task", "")
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(tk.ID)
		r := tk.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	tk, _ = h.ticket(tk.ID)
	got := tk.LatestAssignment().Report.Summary
	for _, want := range []string{crashSummary, "exit status 3", "fatal: ANTHROPIC_API_KEY=*** rejected by the API"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "sk-ant-s3cret") {
		t.Errorf("summary leaks the key: %q", got)
	}
}

func TestCrashCauseIsBounded(t *testing.T) {
	got := crashCause(fmt.Errorf("exit status 1"), strings.Repeat("x", 1000)+"\n"+strings.Repeat("é", 400)+"\n")
	if n := len([]rune(got)); n > summaryMaxRunes {
		t.Fatalf("cause has %d runes, want at most %d: %q", n, summaryMaxRunes, got)
	}
	if !strings.HasPrefix(got, "exit status 1") || !strings.HasSuffix(got, "é") {
		t.Fatalf("cause = %q, want the exit status and the end of the last line", got)
	}
}

// Review Focus 4: several Inputs arriving mid-turn are all delivered in order, exactly once, each marked delivered.
func TestMidTurnInputsInOrder(t *testing.T) {
	h := newHarness(t, nil)
	// A turn that never ends: the init line only, no result.
	lines, err := os.ReadFile(claudeFixture(t, "simple-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	init, _, _ := strings.Cut(string(lines), "\n")
	open := filepath.Join(t.TempDir(), "open-turn.jsonl")
	if err := os.WriteFile(open, []byte(init+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATHENA_FAKE_FIXTURE", open)
	tk := h.submit("Long task", "")
	_, a := h.running(tk.ID)
	msgs := []string{"msg-one", "msg-two", "msg-three", "msg-four"}
	for _, m := range msgs {
		if err := h.eng.AddPersonInput(tk.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	h.eventually("all delivered", func() bool {
		tk, _ := h.ticket(tk.ID)
		return len(tk.Undelivered) == 0 && strings.Contains(h.stdin(), "msg-four")
	})
	tk, _ = h.ticket(tk.ID)
	for _, in := range tk.Inputs {
		if in.DeliveredTo != a.ID || in.DeliveredAt == nil {
			t.Errorf("input %q not marked delivered: %+v", in.Text, in)
		}
	}
	log := h.stdin()
	last := -1
	for _, m := range msgs {
		if n := strings.Count(log, m); n != 1 {
			t.Errorf("%s delivered %d times, want 1", m, n)
		}
		i := strings.Index(log, m)
		if i < last {
			t.Errorf("%s out of order", m)
		}
		last = i
	}
}

func TestSlotsQueue(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Slots = 1 })
	a := h.submit("First", "")
	b := h.submit("Second", "")
	_, aa := h.running(a.ID)
	h.never("second ticket started while the slot is taken", 300*time.Millisecond, func() bool {
		return h.assignmentState(b.ID) != core.AssignmentPending
	})
	if _, st := h.ticket(b.ID); st != (core.Stage{Stage: "working", Action: "queued"}) {
		t.Fatalf("second stage = %+v, want working/queued", st)
	}
	if r := h.report(a.ID, aa.ID, core.ReportReady, "done"); !r.OK {
		t.Fatalf("report = %+v", r)
	}
	h.running(b.ID)
}

func TestParallelTicketsSameRepoGetSeparateWorktrees(t *testing.T) {
	h := newHarness(t, nil)
	repo := newRepo(t)
	a := h.submit("Change one", repo)
	b := h.submit("Change two", repo)
	a, _ = h.running(a.ID)
	b, _ = h.running(b.ID)
	da, db := TicketDir(h.cfg.WorkRoot, a), TicketDir(h.cfg.WorkRoot, b)
	if da == db {
		t.Fatalf("both tickets in %s", da)
	}
	for dir, branch := range map[string]string{da: "athena/t-0001", db: "athena/t-0002"} {
		if got := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
			t.Errorf("%s on %q, want %q", dir, got, branch)
		}
		if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
			t.Errorf("worktree %s lacks the checkout: %v", dir, err)
		}
		if st := git(t, dir, "status", "--porcelain"); st != "" {
			t.Errorf("worktree %s not clean (.athena must be ignored): %q", dir, st)
		}
		if wd, err := WorkingDir(h.cfg.WorkRoot, map[string]core.Ticket{da: a, db: b}[dir]); err != nil || wd != dir {
			t.Errorf("WorkingDir = %q, %v; want %q", wd, err, dir)
		}
	}
	if got := git(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("source checkout moved to %q", got)
	}
	if st := git(t, repo, "status", "--porcelain"); st != "" {
		t.Errorf("source checkout touched: %q", st)
	}
	if invs := h.invocations(); len(invs) != 2 || invs[0].Env["ATHENA_DIR"] == invs[1].Env["ATHENA_DIR"] {
		t.Errorf("sessions did not get their own directories: %+v", invs)
	}
}

func TestTicketEndRemovesWorktreeUnlessUnpushed(t *testing.T) {
	h := newHarness(t, nil)
	repo := newRepo(t)
	clean := h.submit("Nothing to keep", repo)
	work := h.submit("Has a commit", repo)
	dirty := h.submit("Modified file", repo)
	untracked := h.submit("New file", repo)
	var dirs []string
	var as []core.Assignment
	for _, tk := range []core.Ticket{clean, work, dirty, untracked} {
		tk, a := h.running(tk.ID)
		dirs = append(dirs, TicketDir(h.cfg.WorkRoot, tk))
		as = append(as, a)
	}
	dc, dw, dd, du := dirs[0], dirs[1], dirs[2], dirs[3]
	if err := os.WriteFile(filepath.Join(dw, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dw, "add", "new.txt")
	git(t, dw, "commit", "-q", "-m", "work")
	if err := os.WriteFile(filepath.Join(dd, "README"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(du, "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i, tk := range []core.Ticket{clean, work, dirty, untracked} {
		if r := h.report(tk.ID, as[i].ID, core.ReportReady, "done"); !r.OK {
			t.Fatal(r)
		}
	}
	h.eventually("clean worktree removed", func() bool { _, err := os.Stat(dc); return os.IsNotExist(err) })
	h.eventually("sessions gone", func() bool { return h.n.liveCount() == 0 })
	h.n.Close() // waits for cleanup to finish
	for _, f := range []string{filepath.Join(dw, "new.txt"), filepath.Join(dd, "README"), filepath.Join(du, "scratch.txt")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("worktree work was removed (%s): %v", f, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dd, "README")); string(b) != "changed\n" {
		t.Errorf("modified file lost its change: %q", b)
	}
	if got := git(t, repo, "branch", "--list", "athena/t-0001"); got == "" {
		t.Error("branch of the removed worktree was deleted")
	}
}

func TestNonGitDirSerialized(t *testing.T) {
	h := newHarness(t, nil)
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := h.submit("First here", dir)
	b := h.submit("Second here", dir)
	c := h.submit("Elsewhere", "")
	a, aa := h.running(a.ID)
	h.running(c.ID)
	h.never("second ticket started in the busy directory", 300*time.Millisecond, func() bool {
		return h.assignmentState(b.ID) != core.AssignmentPending
	})
	if wd, err := WorkingDir(h.cfg.WorkRoot, a); err != nil || wd != real {
		t.Fatalf("WorkingDir = %q, %v; want %q (a non-git dir is used as-is)", wd, err, real)
	}
	if r := h.report(a.ID, aa.ID, core.ReportReady, "done"); !r.OK {
		t.Fatal(r)
	}
	h.running(b.ID)
	var dirs []string
	for _, inv := range h.invocations() {
		dirs = append(dirs, inv.Env["ATHENA_DIR"])
	}
	if slices.Index(dirs, real) < 0 || strings.Count(strings.Join(dirs, "\n"), real) != 2 {
		t.Fatalf("ATHENA_DIR values = %v, want %s twice", dirs, real)
	}
}

func TestMissingDirectoryFailsTicket(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Nowhere", filepath.Join(t.TempDir(), "missing"))
	h.eventually("failed report", func() bool {
		tk, _ := h.ticket(tk.ID)
		r := tk.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	if n := len(h.invocations()); n != 0 {
		t.Fatalf("claude started %d times", n)
	}
}

func TestStopTicketClosesSession(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Task", "")
	h.running(tk.ID)
	if err := h.eng.StopTicket(tk.ID, "enough"); err != nil {
		t.Fatal(err)
	}
	h.eventually("session gone", func() bool { return h.n.liveCount() == 0 })
}

// blockingEngine blocks every callback until released, to prove Start/Deliver/Stop never call the Engine.
type blockingEngine struct{ release chan struct{} }

func (b blockingEngine) wait() { <-b.release }
func (b blockingEngine) AssignmentStarted(string, string, string, string, string) error {
	b.wait()
	return nil
}
func (b blockingEngine) InputsDelivered(string, string, []string) error { b.wait(); return nil }
func (b blockingEngine) DeliveryFailed(string, string, string) error    { b.wait(); return nil }
func (b blockingEngine) InputsRead(string, []string) error              { b.wait(); return nil }
func (b blockingEngine) TurnEnded(string, string) error                 { b.wait(); return nil }
func (b blockingEngine) AssignmentInterrupted(string, string, string) error {
	b.wait()
	return nil
}
func (b blockingEngine) FileReport(string, core.ReportInput) error { b.wait(); return nil }
func (b blockingEngine) UsageLimited(string, string, core.UsageLimit) error {
	b.wait()
	return nil
}
func (b blockingEngine) Ticket(string) (core.Ticket, core.Stage, error) {
	b.wait()
	return core.Ticket{}, core.Stage{}, fmt.Errorf("released")
}
func (b blockingEngine) RequestApproval(core.Approval) (core.Approval, error) {
	b.wait()
	return core.Approval{}, fmt.Errorf("released")
}
func (b blockingEngine) WaitApproval(context.Context, string) (core.Approval, error) {
	b.wait()
	return core.Approval{}, fmt.Errorf("released")
}
func (b blockingEngine) ExpireApproval(string) error { b.wait(); return nil }
func (b blockingEngine) Trigger(string) (core.Trigger, error) {
	b.wait()
	return core.Trigger{}, fmt.Errorf("released")
}
func (b blockingEngine) ReplyOnce(core.ReplyRequest) (core.ReplyOutcome, error) {
	b.wait()
	return core.ReplyOutcome{}, nil
}
func (b blockingEngine) FollowForContext(string, string) error { b.wait(); return nil }
func (b blockingEngine) MarkUnrelated(string, string, string) error {
	b.wait()
	return nil
}
func (b blockingEngine) Handback(string, string, string) (bool, error) { b.wait(); return false, nil }
func (b blockingEngine) BindThread(string, string, bool) error         { b.wait(); return nil }
func (b blockingEngine) RecordUsage(string, string, core.UsageDelta) error {
	b.wait()
	return nil
}

func TestExecutorMethodsNeverBlock(t *testing.T) {
	eng := blockingEngine{release: make(chan struct{})}
	n := New(Config{WorkRoot: t.TempDir(), StateDir: t.TempDir(), AthenaBin: "/usr/bin/true", SocketPath: "/tmp/unused.sock", ClaudeBinary: "/nonexistent"}, eng)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			tk := core.Ticket{ID: fmt.Sprintf("T-%d", i), Title: "x", Execution: &core.Execution{}}
			a := core.Assignment{ID: fmt.Sprintf("a%d", i), State: core.AssignmentPending}
			n.Start(tk, a)
			n.Deliver(tk, a)
			n.Stop(tk, a, "x")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start/Deliver/Stop blocked on the Engine")
	}
	close(eng.release)
	n.Close()
}

func TestTranscriptLivesInStateDir(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Logged task", "")
	h.running(tk.ID)
	tr := TranscriptPath(h.cfg.StateDir, tk.ID)
	if want := filepath.Join(h.cfg.StateDir, "transcripts", "T-0001.jsonl"); tr != want {
		t.Fatalf("TranscriptPath = %q, want %q", tr, want)
	}
	h.eventually("transcript written", func() bool {
		b, err := os.ReadFile(tr)
		return err == nil && strings.Contains(string(b), fixtureSessionID)
	})
	if _, err := os.Stat(filepath.Join(TicketDir(h.cfg.WorkRoot, tk), ".athena", "transcript.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("transcript also written in the ticket dir: %v", err)
	}
}

func TestApprovalSummaryRedactsSecrets(t *testing.T) {
	got := summarize("Bash", json.RawMessage(`{"command":"GITHUB_TOKEN=ghp_s3cret make deploy --password hunter2"}`), "command needs approval (deploy)")
	if strings.Contains(got, "ghp_s3cret") || strings.Contains(got, "hunter2") {
		t.Fatalf("summary leaks a secret: %q", got)
	}
	if !strings.Contains(got, "make deploy") || !strings.Contains(got, "GITHUB_TOKEN=***") {
		t.Fatalf("summary = %q", got)
	}
}

// The Result of every turn carries the session's cumulative cost but that turn's tokens; a resumed process carries the
// cost on. The Ticket's cost is the session's true total and its tokens the sum (fixtures captured from the real CLI).
func TestUsageFromCumulativeResults(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_FIXTURE", claudeFixture(t, "two-turns.jsonl"))
	tk := h.submit("Two turns", "")
	h.running(tk.ID)
	stats := func() core.TicketStats {
		cur, _ := h.ticket(tk.ID)
		return cur.Stats
	}
	h.eventually("both results recorded", func() bool { return stats().Turns == 2 })
	s := stats()
	if math.Abs(s.CostUSD-0.0229594) > 1e-9 {
		t.Errorf("cost = %v, want the session total 0.0229594", s.CostUSD)
	}
	if s.InputTokens != 20 || s.OutputTokens != 83 || s.CacheReadTokens != 21983+29721 {
		t.Errorf("tokens = %d in, %d out, %d cache read; want 20, 83, 51704", s.InputTokens, s.OutputTokens, s.CacheReadTokens)
	}
	if s.ActiveTime != 5795*time.Millisecond {
		t.Errorf("active time = %v, want 5.795s (the per-turn durations)", s.ActiveTime)
	}

	// A retry resumes the same session in a new process, whose Result starts from the earlier cost.
	_, a := h.running(tk.ID)
	if r := h.report(tk.ID, a.ID, core.ReportFailed, "stuck"); !r.OK {
		t.Fatalf("report: %+v", r)
	}
	t.Setenv("ATHENA_FAKE_FIXTURE", claudeFixture(t, "resumed-turn.jsonl"))
	if err := h.eng.RetryTicket(tk.ID); err != nil {
		t.Fatal(err)
	}
	h.eventually("resumed result recorded", func() bool { return stats().Turns == 3 })
	s = stats()
	if math.Abs(s.CostUSD-0.0268904) > 1e-9 {
		t.Errorf("cost after resume = %v, want 0.0268904, not the sum of the reported totals", s.CostUSD)
	}
	if s.InputTokens != 30 || s.OutputTokens != 124 {
		t.Errorf("tokens after resume = %d in, %d out; want 30, 124", s.InputTokens, s.OutputTokens)
	}
}

// Inputs that reach a worker on a Source's Thread are cards: who wrote them, when, where, the URL, the trigger_id the
// worker acknowledges or hands back, and at most 600 characters of text with a pointer to the rest.
func TestTriggerInputCard(t *testing.T) {
	at := time.Date(2026, 11, 4, 10, 30, 0, 0, time.UTC)
	long := strings.Repeat("é", 700)
	triggers := map[string]core.Trigger{
		"github:issuecomment:1": {ID: "github:issuecomment:1", Source: "github", Actor: core.ActorRef{Source: "github", ID: "alice"},
			Human: true, At: at, Kind: core.KindMessage, Threads: []string{"github:o/r#12"}, ReplyTo: "github:issue:9",
			Meta: map[string]string{"repo": "O/R", "number": "12", "url": "https://github.com/O/R/issues/12#issuecomment-1"}},
		"github:issuecomment:2": {ID: "github:issuecomment:2", Source: "github", Actor: core.ActorRef{Source: "github", ID: "bob"},
			Human: true, At: at, Threads: []string{"github:o/r#3"}, Meta: map[string]string{"repo": "o/r", "number": "3"}},
		"github:checksuite:5": {ID: "github:checksuite:5", Source: "github", Actor: core.ActorRef{Source: "github", ID: "ci"},
			Bot: true, At: at, Kind: core.KindCI, Threads: []string{"github:o/r@main"}, Meta: map[string]string{"repo": "o/r"}},
		"owner-1": {ID: "owner-1", Source: "owner", Human: true, At: at, Text: "owner text"},
		"github:issue:7": {ID: "github:issue:7", Source: "github", Actor: core.ActorRef{Source: "github", ID: "carol"},
			Human: true, At: at, Threads: []string{"github:o/r#7"}, Meta: map[string]string{"repo": "o/r", "number": "7"},
			Disposition: core.Disposition{Acknowledged: []string{"T-1"}}},
	}
	lookup := func(id string) (core.Trigger, bool) { tr, ok := triggers[id]; return tr, ok }
	tk := core.Ticket{ID: "T-1", Inputs: []core.Input{
		{ID: "i1", Kind: core.InputTrigger, TriggerID: "github:issuecomment:1", Text: "please fix the typo", Human: true, Note: "same page as T-0"},
		{ID: "i2", Kind: core.InputTrigger, TriggerID: "github:issuecomment:2", Text: long, Human: true, Context: true},
		{ID: "i3", Kind: core.InputTrigger, TriggerID: "github:checksuite:5", Text: "CI check suite failure on main"},
		{ID: "i4", Kind: core.InputTrigger, TriggerID: "owner-1", Text: "owner text", Human: true},
		{ID: "i5", Kind: core.InputNote, Text: "T-2 found the root cause"},
		{ID: "i6", Kind: core.InputTrigger, TriggerID: "github:issue:7", Text: "please review it", Human: true},
	}}
	got := renderInputs(tk, []string{"i1", "i2", "i3", "i4", "i5", "i6"}, lookup)
	for _, want := range []string{
		"[input i1: message from a person]\ntrigger_id: github:issuecomment:1\n" +
			"acknowledged: no; ack it before substantial work if it is a request you take up\nfrom: alice\nat: 2026-11-04T10:30:00Z\n" +
			"thread: O/R#12 (github:o/r#12)\nurl: https://github.com/O/R/issues/12#issuecomment-1\nreplies_to: github:issue:9\n" +
			"routing note: same page as T-0\nplease fix the typo\n",
		"[input i2: message from a person]\ntrigger_id: github:issuecomment:2\nfrom: bob\n",
		"thread: o/r#3 (github:o/r#3)\ncontext: it reached you on a thread you follow for context",
		strings.Repeat("é", 600) + "…\n[cut: 600 of 700 characters shown; read the rest with gh on o/r#3]",
		"[input i3: ci]\ntrigger_id: github:checksuite:5\nfrom: ci (bot)\n",
		"thread: o/r@main (github:o/r@main)\n",
		"[input i4: message from the owner (not on GitHub)]\nowner text\n",
		"[input i5: note]\nNote: T-2 found the root cause\n",
		// Athena acknowledged the Ticket's first message when it accepted it: the worker must not ack it again.
		"[input i6: message from a person]\ntrigger_id: github:issue:7\nacknowledged: yes, already on its thread; do not ack it again\nfrom: carol\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered inputs lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, strings.Repeat("é", 601)) {
		t.Error("a card carries more than 600 characters of text")
	}
	// A context message and a bot's say nothing of acknowledging: they need none.
	if strings.Count(got, "acknowledged: ") != 2 {
		t.Errorf("acknowledged lines on context or bot cards:\n%s", got)
	}
	if strings.Index(got, "[input i1") > strings.Index(got, "[input i5") {
		t.Error("inputs out of order")
	}
	// Without a way to read Triggers, a Trigger Input is plain text, and not the owner's.
	if plain := renderInputs(tk, []string{"i1"}, nil); !strings.HasPrefix(plain, "[input i1: message]\nplease fix the typo") {
		t.Errorf("plain rendering = %q", plain)
	}
}

// R24 M2: only the owner's own Inputs say they are from the owner when an Input is not a card.
func TestInputLabelFallback(t *testing.T) {
	triggers := map[string]core.Trigger{
		"owner-1":  {ID: "owner-1", Source: core.OwnerActor.Source, Human: true},
		"slack:1":  {ID: "slack:1", Source: "slack", Human: true},
		"github:1": {ID: "github:1", Source: "github", Human: true, Threads: []string{"github:o/r#1"}},
	}
	lookup := func(id string) (core.Trigger, bool) { tr, ok := triggers[id]; return tr, ok }
	tk := core.Ticket{Inputs: []core.Input{
		{ID: "a", Kind: core.InputTrigger, TriggerID: "owner-1", Text: "t", Human: true},
		{ID: "b", Kind: core.InputTrigger, TriggerID: "slack:1", Text: "t", Human: true},
		{ID: "c", Kind: core.InputTrigger, TriggerID: "missing", Text: "t", Human: true},
		{ID: "d", Kind: core.InputText, Text: "t", Human: true},
	}}
	for id, want := range map[string]string{"a": "message from the owner (not on GitHub)", "b": "message", "c": "message",
		"d": "message from the owner (not on GitHub)"} {
		got := renderInputs(tk, []string{id}, lookup)
		if !strings.HasPrefix(got, "[input "+id+": "+want+"]\n") {
			t.Errorf("input %s: %q, want label %q", id, got, want)
		}
	}
}

// R30: the Inputs of an Assignment's first message are read once the session files a Report after it, even while the
// turn goes on; the release of unread Inputs when a Ticket ends relies on it.
func TestFirstMessageInputsReadAfterReport(t *testing.T) {
	h := newHarness(t, nil)
	t.Setenv("ATHENA_FAKE_FIXTURE", openTurnFixture(t))
	tk := h.submit("Open task\nbody-r30", "")
	tk, a := h.running(tk.ID)
	h.never("first input read before a report or a turn end", 200*time.Millisecond, func() bool {
		tk, _ := h.ticket(tk.ID)
		return tk.Inputs[0].ReadAt != nil
	})
	if r := h.report(tk.ID, a.ID, core.ReportProgress, "looking"); !r.OK {
		t.Fatal(r)
	}
	h.eventually("first input read after the report", func() bool {
		tk, _ := h.ticket(tk.ID)
		return tk.Inputs[0].ReadAt != nil
	})
}

// starts returns how many sessions of the Ticket fakeclaude started, and the arguments of the last one.
func (h *harness) starts(ticketID string) (int, []string) {
	n, last := 0, []string(nil)
	for _, inv := range h.invocations() {
		if inv.Env["ATHENA_TICKET"] == ticketID {
			n++
			last = inv.Args
		}
	}
	return n, last
}

// I1 (R34): a ready report of a Ticket that waits for its pull request hands the work off: once that turn ends, the
// session's process ends and its slot is free, so another Ticket starts while the Assignment keeps running with its
// session. A review comment on the pull request later resumes the same session (--resume) through the normal queue.
func TestReadyHandoffFreesTheSlotAndAFollowUpResumes(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	h := newHarness(t, func(c *Config) { c.Slots = 1 })
	a := h.submit("Open a pull request", "")
	_, aa := h.running(a.ID)
	pull := "github:o/r#7"
	if err := h.eng.BindThread(a.ID, pull, true); err != nil {
		t.Fatal(err)
	}
	if r := h.report(a.ID, aa.ID, core.ReportReady, "opened #7"); !r.OK {
		t.Fatalf("ready report = %+v", r)
	}
	b := h.submit("Second task", "")
	h.never("second ticket started while the first turn runs", 200*time.Millisecond, func() bool {
		return h.assignmentState(b.ID) != core.AssignmentPending
	})
	if err := os.WriteFile(hold, nil, 0o600); err != nil { // the ready turn ends
		t.Fatal(err)
	}
	_, ba := h.running(b.ID)
	tk, st := h.ticket(a.ID)
	if tk.Ended != nil || tk.LatestAssignment().ID != aa.ID || tk.LatestAssignment().State != core.AssignmentRunning ||
		tk.LatestAssignment().SessionID != fixtureSessionID || st != (core.Stage{Stage: "person", Action: "review"}) {
		t.Fatalf("handed-off ticket: ended %v, assignment %+v, stage %+v; want it open, waiting for review, its "+
			"assignment running with its session", tk.Ended, tk.LatestAssignment(), st)
	}
	if s := h.n.liveSession(a.ID); s != nil {
		t.Fatal("the handed-off ticket still holds a session")
	}

	// A person's review comment on the pull request: the first Ticket resumes once the slot is free again.
	review := core.Trigger{ID: "github:review:1", Source: "github", Kind: core.KindMessage, Human: true,
		Actor: core.ActorRef{Source: "github", ID: "alice"}, Threads: []string{pull}, At: time.Now(),
		Text: "please rename the flag (review-c4d1)"}
	if _, err := h.eng.Receive(review); err != nil {
		t.Fatal(err)
	}
	h.never("the follow-up resumed while the slot is taken", 200*time.Millisecond, func() bool {
		n, _ := h.starts(a.ID)
		return n > 1
	})
	if r := h.report(b.ID, ba.ID, core.ReportReady, "done"); !r.OK {
		t.Fatalf("second report = %+v", r)
	}
	h.eventually("the first ticket resumed", func() bool { n, _ := h.starts(a.ID); return n == 2 })
	_, args := h.starts(a.ID)
	if id, ok := argValue(args, "--resume"); !ok || id != fixtureSessionID {
		t.Fatalf("resumed with %v, want --resume %s", args, fixtureSessionID)
	}
	h.eventually("the review delivered", func() bool {
		tk, _ := h.ticket(a.ID)
		return len(tk.Undelivered) == 0 && strings.Contains(h.stdin(), "review-c4d1")
	})
	if strings.Count(h.stdin(), resumeNote) != 1 || strings.Contains(h.stdin(), recoveryNote) {
		t.Fatalf("stdin = %q, want the resume note once and no crash note", h.stdin())
	}
	if tk, _ := h.ticket(a.ID); tk.LatestAssignment().ID != aa.ID {
		t.Fatalf("assignment %s, want the same one resumed", tk.LatestAssignment().ID)
	}
}

// TestStatusFollowsSessionsAndQueue: Status reports the live Tickets and the queue, and OnStatus fires when they
// change, from the loop.
func TestStatusFollowsSessionsAndQueue(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(c *Config) {
		c.Name, c.ClaudeVersion, c.Slots = "mac", "2.0.1", 1
		c.OnStatus = func() { calls.Add(1) }
	})
	if s := h.n.Status(); s.Name != "mac" || s.Slots != 1 || s.SlotsInUse != 0 || s.ClaudeVersion != "2.0.1" || len(s.LiveTicketIDs) != 0 {
		t.Fatalf("idle status = %+v", s)
	}
	first := h.submit("first body", t.TempDir())
	h.eventually("first live", func() bool { return h.n.Status().SlotsInUse == 1 })
	second := h.submit("second body", t.TempDir())
	h.eventually("second queued", func() bool { return h.n.Status().Queued == 1 })
	if s := h.n.Status(); len(s.LiveTicketIDs) != 1 || s.LiveTicketIDs[0] != first.ID {
		t.Fatalf("status = %+v, want only %s live", s, first.ID)
	}
	if calls.Load() < 2 {
		t.Errorf("OnStatus called %d times, want at least 2", calls.Load())
	}
	_, a := h.running(first.ID)
	if r := h.report(first.ID, a.ID, core.ReportReady, "done"); !r.OK {
		t.Fatalf("report = %+v", r)
	}
	h.eventually("second takes the slot", func() bool {
		s := h.n.Status()
		return s.Queued == 0 && len(s.LiveTicketIDs) == 1 && s.LiveTicketIDs[0] == second.ID
	})
}

func TestNewSessionUsesOverride(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.WorkerPrompt = func() string { return "WORKER OVERRIDE" } })
	tk := h.submit("Fix the typo", "")
	h.running(tk.ID)
	_, args := h.starts(tk.ID)
	if got, ok := argValue(args, "--append-system-prompt"); !ok || got != "WORKER OVERRIDE" {
		t.Fatalf("--append-system-prompt = %q, want the override", got)
	}
	if slices.Contains(args, "--resume") {
		t.Errorf("first start resumed: %v", args)
	}
}

// A session resumed after the owner changed the prompt keeps the prompt it started with; a Ticket's next fresh session
// gets the new one.
func TestResumedSessionKeepsPrompt(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	var prompt atomic.Value
	prompt.Store("OVERRIDE ONE")
	h := newHarness(t, func(c *Config) {
		c.Slots = 2
		c.WorkerPrompt = func() string { return prompt.Load().(string) }
	})
	a := h.submit("Open a pull request", "")
	_, aa := h.running(a.ID)
	pull := "github:o/r#7"
	if err := h.eng.BindThread(a.ID, pull, true); err != nil {
		t.Fatal(err)
	}
	if r := h.report(a.ID, aa.ID, core.ReportReady, "opened #7"); !r.OK {
		t.Fatalf("ready report = %+v", r)
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil { // the ready turn ends, the session's process with it
		t.Fatal(err)
	}
	h.eventually("the handoff", func() bool { return h.n.liveSession(a.ID) == nil })
	prompt.Store("OVERRIDE TWO") // saved while the session waits
	review := core.Trigger{ID: "github:review:1", Source: "github", Kind: core.KindMessage, Human: true,
		Actor: core.ActorRef{Source: "github", ID: "alice"}, Threads: []string{pull}, At: time.Now(), Text: "please rename the flag"}
	if _, err := h.eng.Receive(review); err != nil {
		t.Fatal(err)
	}
	h.eventually("the session resumed", func() bool { n, _ := h.starts(a.ID); return n == 2 })
	_, args := h.starts(a.ID)
	if _, ok := argValue(args, "--resume"); !ok {
		t.Fatalf("second start did not resume: %v", args)
	}
	if got, _ := argValue(args, "--append-system-prompt"); got != "OVERRIDE ONE" {
		t.Fatalf("resumed with prompt %q, want the one the session started with", got)
	}
	// A different Ticket starts fresh with the new prompt.
	b := h.submit("Second task", t.TempDir())
	h.eventually("second started", func() bool { n, _ := h.starts(b.ID); return n == 1 })
	_, bargs := h.starts(b.ID)
	if got, _ := argValue(bargs, "--append-system-prompt"); got != "OVERRIDE TWO" {
		t.Fatalf("new session prompt = %q, want OVERRIDE TWO", got)
	}
}

// An ended Ticket's kept prompt goes with it; a person's follow-up reopens the Ticket and resumes its session with the
// current effective prompt (ruling R18), not the compiled default.
func TestReopenedTicketResumesWithEffectivePrompt(t *testing.T) {
	var prompt atomic.Value
	prompt.Store("OVERRIDE ONE")
	h := newHarness(t, func(c *Config) { c.WorkerPrompt = func() string { return prompt.Load().(string) } })
	a := h.submit("Fix the typo", "")
	_, aa := h.running(a.ID)
	if r := h.report(a.ID, aa.ID, core.ReportReady, "done"); !r.OK {
		t.Fatalf("report = %+v", r)
	}
	path := PromptPath(h.cfg.StateDir, a.ID)
	h.eventually("the Ticket ended and its kept prompt went", func() bool {
		tk, _ := h.ticket(a.ID)
		_, err := os.Stat(path)
		return tk.Ended != nil && os.IsNotExist(err)
	})
	prompt.Store("OVERRIDE TWO")
	if err := h.eng.AddPersonInput(a.ID, "one more thing"); err != nil {
		t.Fatal(err)
	}
	h.eventually("the session resumed", func() bool { n, _ := h.starts(a.ID); return n == 2 })
	_, args := h.starts(a.ID)
	if _, ok := argValue(args, "--resume"); !ok {
		t.Fatalf("second start did not resume: %v", args)
	}
	if got, _ := argValue(args, "--append-system-prompt"); got != "OVERRIDE TWO" {
		t.Fatalf("reopened ticket prompt = %q, want the current effective prompt", got)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "OVERRIDE TWO" {
		t.Fatalf("kept prompt = %q, %v", b, err)
	}
}

// interruptRecorder is the Engine the node reports to, noting the interruptions it is told of.
type interruptRecorder struct {
	*core.Engine
	mu    sync.Mutex
	calls []string
}

func (r *interruptRecorder) AssignmentInterrupted(ticketID, assignmentID, reason string) error {
	r.mu.Lock()
	r.calls = append(r.calls, ticketID+"/"+assignmentID+"/"+reason)
	r.mu.Unlock()
	return r.Engine.AssignmentInterrupted(ticketID, assignmentID, reason)
}

func (r *interruptRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// A shutdown marks the sessions that were busy as interrupted, and only those: a session whose last turn ended well and
// that was sent nothing since is idle.
func TestShutdownMarksBusySessionsInterrupted(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	h := newHarness(t, func(c *Config) { c.Slots, c.CloseGrace = 2, 300*time.Millisecond })
	rec := &interruptRecorder{Engine: h.eng}
	h.n.SetEngine(rec)
	busy, idle := h.submit("Busy one", t.TempDir()), h.submit("Idle one", t.TempDir())
	_, busyA := h.running(busy.ID)
	_, idleA := h.running(idle.ID)
	for _, p := range [][2]string{{busy.ID, busyA.ID}, {idle.ID, idleA.ID}} {
		if r := h.report(p[0], p[1], core.ReportProgress, "going"); !r.OK {
			t.Fatalf("report = %+v", r)
		}
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil { // both turns end well, with a report each
		t.Fatal(err)
	}
	h.eventually("both turns ended", func() bool {
		for _, id := range []string{busy.ID, idle.ID} {
			if b, _ := os.ReadFile(h.n.transcriptPath(id)); !strings.Contains(string(b), `"type":"result"`) {
				return false
			}
		}
		return true
	})
	if err := os.Remove(hold); err != nil { // the next turn holds again
		t.Fatal(err)
	}
	if err := h.eng.AddPersonInput(busy.ID, "one more thing"); err != nil {
		t.Fatal(err)
	}
	h.eventually("the input sent", func() bool { return strings.Contains(h.stdin(), "one more thing") })
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("interruptions before the shutdown: %v", got)
	}
	h.n.Close()
	if got, want := rec.seen(), []string{busy.ID + "/" + busyA.ID + "/" + core.InterruptRestart}; !slices.Equal(got, want) {
		t.Fatalf("interruptions = %v, want %v", got, want)
	}
	tk, _ := h.ticket(busy.ID)
	if tk.LatestAssignment().OpenInterruption() == nil || tk.LatestAssignment().State != core.AssignmentRunning {
		t.Fatalf("busy assignment = %+v, want running with an open interruption", tk.LatestAssignment())
	}
	if tk, _ := h.ticket(idle.ID); len(tk.LatestAssignment().Interruptions) != 0 {
		t.Fatalf("idle assignment interrupted: %+v", tk.LatestAssignment().Interruptions)
	}
}

// The CLI's Stop hook fires while a closing session winds down; it is no turn end, or the Assignment would look idle
// and never resume.
func TestStopHookDuringShutdownIsIgnored(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	h := newHarness(t, func(c *Config) { c.CloseGrace = 300 * time.Millisecond })
	tk := h.submit("Busy one", t.TempDir())
	_, a := h.running(tk.ID)
	s := h.n.lookup(tk.ID, a.ID)
	if s == nil {
		t.Fatal("no live session")
	}
	s.mu.Lock()
	s.shutdown = true
	s.mu.Unlock()
	h.hook(EventStop, tk.ID, a.ID, map[string]any{})
	if got, _ := h.ticket(tk.ID); got.LatestAssignment().IdleAt != nil {
		t.Fatalf("a Stop during shutdown recorded a turn end: %+v", got.LatestAssignment())
	}
	s.mu.Lock()
	s.shutdown = false
	s.mu.Unlock()
	h.hook(EventStop, tk.ID, a.ID, map[string]any{})
	if got, _ := h.ticket(tk.ID); got.LatestAssignment().IdleAt == nil {
		t.Fatal("a Stop outside a shutdown records no turn end")
	}
}

// A session a restart cut short resumes with the restart note, not the crash note, and closes the interruption.
func TestRestartResumeUsesRestartNote(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "turn-may-end")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	h := newHarness(t, func(c *Config) { c.CloseGrace = 300 * time.Millisecond })
	tk := h.submit("Busy one", t.TempDir())
	h.running(tk.ID)
	h.n.Close()
	got, _ := h.ticket(tk.ID)
	a := *got.LatestAssignment()
	if a.OpenInterruption() == nil || a.SessionID != fixtureSessionID {
		t.Fatalf("assignment after the shutdown = %+v, want an open interruption and its session", a)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.cfg.StateDir, runDir)); len(entries) != 0 {
		t.Fatalf("run files left after the shutdown: %v", entries)
	}
	n2 := New(h.cfg, h.eng)
	n2.restartDelay = func(int) time.Duration { return 0 }
	t.Cleanup(func() { n2.Close() })
	n2.Start(got, a)
	h.eventually("the restart note sent", func() bool { return strings.Contains(h.stdin(), restartNote+recoveryAdvice) })
	if strings.Contains(h.stdin(), recoveryNote) {
		t.Fatalf("stdin = %q, want no crash note", h.stdin())
	}
	_, args := h.starts(tk.ID)
	if id, ok := argValue(args, "--resume"); !ok || id != fixtureSessionID {
		t.Fatalf("resumed with %v, want --resume %s", args, fixtureSessionID)
	}
	h.eventually("the interruption closed", func() bool {
		got, _ := h.ticket(tk.ID)
		l := got.LatestAssignment()
		return len(l.Interruptions) == 1 && l.Interruptions[0].ResumedAt != nil
	})
}

// --- Codex sessions (fakecodex, and the real athena binary for the hooks and the MCP server) ---

var (
	codexOnce                 sync.Once
	fakeCodexPath, athenaPath string
	codexErr                  error
)

// codexBins builds fakecodex and athena once per test binary run, beside fakeclaude (removed by TestMain).
func codexBins(t *testing.T) (fakecodex, athena string) {
	t.Helper()
	fakeClaude(t) // makes fakeDir
	codexOnce.Do(func() {
		fakeCodexPath, athenaPath = filepath.Join(fakeDir, "codex"), filepath.Join(fakeDir, "athena")
		for out, pkg := range map[string]string{
			fakeCodexPath: "github.com/danielpina1/athena/internal/runtime/codex/testdata/fakecodex",
			athenaPath:    "github.com/danielpina1/athena/cmd/athena",
		} {
			if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
				codexErr = fmt.Errorf("build %s: %v\n%s", pkg, err, b)
				return
			}
		}
	})
	if codexErr != nil {
		t.Fatal(codexErr)
	}
	return fakeCodexPath, athenaPath
}

// codexHarness is a harness whose node also has Codex (fakecodex), with the real athena binary running the hooks and
// the MCP server. rpcLog receives what the session sent fakecodex and the hooks it ran.
type codexHarness struct {
	*harness
	rpcLog, argsLog string
}

func newCodexHarness(t *testing.T, fixture string, mod func(*Config)) *codexHarness {
	t.Helper()
	fake, athena := codexBins(t)
	dir := t.TempDir()
	ch := &codexHarness{rpcLog: filepath.Join(dir, "rpc.log"), argsLog: filepath.Join(dir, "args-{ticket}.json")}
	t.Setenv("ATHENA_FAKE_CODEX_RPC_LOG", ch.rpcLog)
	t.Setenv("ATHENA_FAKE_CODEX_ARGS_LOG", ch.argsLog)
	if fixture != "" {
		p := filepath.Join(dir, "fixture.jsonl")
		if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ATHENA_FAKE_CODEX_FIXTURE", p)
	}
	ch.harness = newHarness(t, func(c *Config) {
		c.AthenaBin = athena
		c.Runtimes = map[string]rt.Runtime{
			core.RuntimeClaude: claude.Runtime{Binary: c.ClaudeBinary},
			core.RuntimeCodex: &codex.Runtime{Binary: fake, Home: filepath.Join(c.StateDir, "codex"), AthenaBin: athena,
				Socket: c.SocketPath, WorkRoot: c.WorkRoot, Version: "test"},
		}
		if mod != nil {
			mod(c)
		}
	})
	return ch
}

func (h *codexHarness) submitCodex(text string) core.Ticket {
	h.t.Helper()
	tk, err := h.eng.SubmitOwnerTask(text, core.TaskOptions{Runtime: core.RuntimeCodex})
	if err != nil {
		h.t.Fatal(err)
	}
	return tk
}

// rpc returns the lines of the RPC log.
func (h *codexHarness) rpc() []string {
	b, err := os.ReadFile(h.rpcLog)
	if err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (h *codexHarness) rpcHas(sub string) bool {
	return slices.ContainsFunc(h.rpc(), func(l string) bool { return strings.Contains(l, sub) })
}

func (h *codexHarness) transcript(ticketID string) string {
	b, _ := os.ReadFile(TranscriptPath(h.cfg.StateDir, ticketID))
	return string(b)
}

// codexTurn is a fixture turn (thread and turn ids are fakecodex's placeholders) with body between its start and end.
func codexTurn(body ...string) string {
	lines := append([]string{`{"method":"turn/started","params":{"threadId":"thr_fixture_0001","turn":{"id":"turn_fixture_0001","items":[],"status":"inProgress","startedAt":1893456001}}}`}, body...)
	lines = append(lines, `{"method":"turn/completed","params":{"threadId":"thr_fixture_0001","turn":{"id":"turn_fixture_0001","items":[],"status":"completed","startedAt":1893456001,"completedAt":1893456005,"durationMs":4200}}}`)
	return strings.Join(lines, "\n") + "\n"
}

func codexCommandItem(method, id, command string) string {
	status, extra := "inProgress", `"aggregatedOutput":null,"exitCode":null,"durationMs":null`
	if method == "item/completed" {
		status, extra = "completed", `"aggregatedOutput":"","exitCode":0,"durationMs":20`
	}
	at := `"startedAtMs":1893456007250`
	if method == "item/completed" {
		at = `"completedAtMs":1893456007750`
	}
	cmd, _ := json.Marshal(command)
	return `{"method":"` + method + `","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","item":{"id":"` + id +
		`","type":"commandExecution","command":` + string(cmd) + `,"commandActions":[],"cwd":"/home/user/work","source":"agent","status":"` +
		status + `",` + extra + `},` + at + `}}`
}

// codexHook is a fakecodex directive running Athena's hook for event on a Bash tool call of command.
func codexHook(event, command string) string {
	input := map[string]any{
		"session_id": "thr_fixture_0001", "turn_id": "turn_fixture_0001", "transcript_path": nil, "cwd": "/home/user/work",
		"model": "gpt-5-codex", "permission_mode": "default", "hook_event_name": event, "tool_name": "Bash",
		"tool_input": map[string]any{"command": command},
	}
	if event == EventPreToolUse {
		input["tool_use_id"] = "call_fixture_0001"
	}
	b, _ := json.Marshal(map[string]any{"fake": "hook", "event": event, "input": input})
	return string(b)
}

func codexApprovalRequest(command string, wait bool) string {
	b, _ := json.Marshal(map[string]any{"fake": "request", "wait": wait, "method": "item/commandExecution/requestApproval",
		"params": map[string]any{"threadId": "thr_fixture_0001", "turnId": "turn_fixture_0001", "itemId": "item_0003",
			"startedAtMs": 1893456007500, "command": command}})
	return string(b)
}

func TestCodexTicketRunsAndReports(t *testing.T) {
	h := newCodexHarness(t, codexTurn(
		`{"fake":"mcp","tool":"report","arguments":{"status":"ready","summary":"all done"}}`,
		`{"method":"item/started","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","item":{"id":"item_0007","type":"mcpToolCall","server":"athena","tool":"report","status":"inProgress","arguments":{"status":"ready","summary":"all done"},"result":null,"error":null,"durationMs":31},"startedAtMs":1893456003750}}`,
		`{"method":"item/completed","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","item":{"id":"item_0007","type":"mcpToolCall","server":"athena","tool":"report","status":"completed","arguments":{"status":"ready","summary":"all done"},"result":{"content":[{"type":"text","text":"ok"}],"structuredContent":null},"error":null,"durationMs":31},"completedAtMs":1893456004000}}`,
	), nil)
	tk := h.submitCodex("Do the codex thing")
	h.eventually("the codex worker's ready report ends the ticket", func() bool {
		cur, _ := h.ticket(tk.ID)
		return cur.Ended != nil
	})
	cur, _ := h.ticket(tk.ID)
	a := cur.LatestAssignment()
	if cur.Ended.Outcome != core.OutcomeCompleted || a.Report == nil || a.Report.Summary != "all done" {
		t.Fatalf("ended %+v, report %+v", cur.Ended, a.Report)
	}
	if a.SessionID != "thr_fixture_"+tk.ID {
		t.Fatalf("session id = %q, want the Codex thread", a.SessionID)
	}
	// The report went through `athena mcp`, which the session's -c overrides pointed at this Ticket and Assignment.
	h.eventually("the MCP call's result is logged", func() bool { return h.rpcHas(`"fake":"mcp"`) })
	if !h.rpcHas(`Report filed (ready)`) || h.rpcHas(`"isError":true`) {
		t.Fatalf("the MCP call did not file the report:\n%s", strings.Join(h.rpc(), "\n"))
	}
	b, err := os.ReadFile(strings.ReplaceAll(h.argsLog, "{ticket}", tk.ID))
	if err != nil {
		t.Fatal(err)
	}
	var inv struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env"`
	}
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Env["ATHENA_RUNTIME"] != "codex" || inv.Env["ATHENA_ASSIGNMENT"] != a.ID || inv.Env["ATHENA_TICKET"] != tk.ID {
		t.Fatalf("session env = %v", inv.Env)
	}
	if !slices.Contains(inv.Args, `mcp_servers.athena.args=["mcp","--ticket","`+tk.ID+`"]`) {
		t.Fatalf("session args = %v", inv.Args)
	}
	// The first message says which runtime the worker is on.
	if !h.rpcHas(`ATHENA_RUNTIME=codex`) {
		t.Fatal("the first message does not carry ATHENA_RUNTIME")
	}
}

func TestCodexHookDenyBlocksItem(t *testing.T) {
	h := newCodexHarness(t, codexTurn(
		codexHook(EventPreToolUse, "cat ~/.ssh/id_rsa"),
		codexCommandItem("item/started", "item_0003", "cat ~/.ssh/id_rsa"),
		codexCommandItem("item/completed", "item_0003", "cat ~/.ssh/id_rsa"),
	), nil)
	tk := h.submitCodex("Read a key")
	h.eventually("the hook ran", func() bool { return h.rpcHas(`"fake":"hook"`) })
	var rec struct {
		Denied bool   `json:"denied"`
		Exit   int    `json:"exit"`
		Stdout string `json:"stdout"`
	}
	for _, l := range h.rpc() {
		if strings.Contains(l, `"fake":"hook"`) {
			if err := json.Unmarshal([]byte(l), &rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !rec.Denied || rec.Exit != 0 || !strings.Contains(rec.Stdout, `"permissionDecisionReason":"reading credential files is not allowed"`) {
		t.Fatalf("hook = %+v, want Codex's deny with its reason", rec)
	}
	h.eventually("the turn ended", func() bool {
		cur, _ := h.ticket(tk.ID)
		return cur.LatestAssignment().IdleAt != nil || strings.Contains(h.transcript(tk.ID), `"turn/completed"`)
	})
	if strings.Contains(h.transcript(tk.ID), "item_0003") {
		t.Fatal("the denied command's item reached the transcript")
	}
	h.eventually("the denial counted", func() bool {
		cur, _ := h.ticket(tk.ID)
		return cur.Stats.HookDenials == 1
	})
}

// approveOnly waits for exactly one pending Approval of the Ticket and approves it.
func (h *codexHarness) approveOnly(ticketID string) core.Approval {
	h.t.Helper()
	var pending []core.Approval
	h.eventually("an approval is pending", func() bool {
		pending, _ = h.eng.PendingApprovals()
		return len(pending) > 0
	})
	if len(pending) != 1 || pending[0].TicketID != ticketID {
		h.t.Fatalf("pending approvals = %+v, want one of %s", pending, ticketID)
	}
	if err := h.eng.AnswerApproval(pending[0].ID, true, "go"); err != nil {
		h.t.Fatal(err)
	}
	return pending[0]
}

func TestCodexApprovalRequestBecomesApproval(t *testing.T) {
	h := newCodexHarness(t, codexTurn(
		codexCommandItem("item/started", "item_0003", "kubectl apply -f app.yaml"),
		codexApprovalRequest("kubectl apply -f app.yaml", true),
		codexCommandItem("item/completed", "item_0003", "kubectl apply -f app.yaml"),
	), nil)
	tk := h.submitCodex("Deploy")
	ap := h.approveOnly(tk.ID)
	if ap.Tool != "Bash" || ap.Rule != "bash-escalate:kubectl" || !strings.Contains(ap.Summary, "kubectl apply -f app.yaml") {
		t.Fatalf("approval = %+v", ap)
	}
	h.eventually("Codex is told accept", func() bool { return h.rpcHas(`"decision":"accept"`) })

	// A denied one answers decline; network and permission requests always ask the owner.
	n := h.n
	s := n.liveSession(tk.ID)
	if s == nil {
		t.Fatal("no live session")
	}
	ctx := context.Background()
	got := make(chan rt.Verdict, 1)
	go func() {
		got <- n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "network", Host: "registry.npmjs.org", TurnID: "t9"})
	}()
	var pending []core.Approval
	h.eventually("network approval", func() bool { pending, _ = h.eng.PendingApprovals(); return len(pending) == 1 })
	if p := pending[0]; p.Rule != "network_access" || p.Summary != "Network: registry.npmjs.org" {
		t.Fatalf("network approval = %+v", p)
	}
	if err := h.eng.AnswerApproval(pending[0].ID, false, "no"); err != nil {
		t.Fatal(err)
	}
	if v := <-got; v.Allow {
		t.Fatalf("denied network approval = %+v", v)
	}
	go func() {
		got <- n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "permissions", Host: "*", Paths: []string{"/opt/cache"}, Reason: "needs the cache"})
	}()
	h.eventually("permissions approval", func() bool { pending, _ = h.eng.PendingApprovals(); return len(pending) == 1 })
	if p := pending[0]; p.Rule != "codex_permissions" || !strings.Contains(p.Summary, "/opt/cache") || !strings.Contains(p.Summary, "any host") {
		t.Fatalf("permissions approval = %+v", p)
	}
	if err := h.eng.AnswerApproval(pending[0].ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if v := <-got; !v.Allow {
		t.Fatalf("approved permissions = %+v", v)
	}
	// A command the rules allow, or deny, needs nobody; a file change is judged on every path.
	if v := n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "command", Command: "ls -la"}); !v.Allow {
		t.Fatalf("ls = %+v, want allow", v)
	}
	if v := n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "command", Command: "cat ~/.ssh/id_rsa"}); v.Allow || v.Reason == "" {
		t.Fatalf("credential read = %+v, want deny with a reason", v)
	}
	if v := n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "file", Paths: []string{"a.txt", ".athena/mcp.json"}}); v.Allow {
		t.Fatalf("file change to Athena's MCP config = %+v, want deny", v)
	}
	if v := n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "file", Paths: []string{"a.txt", "b/c.txt"}}); !v.Allow {
		t.Fatalf("file change in the ticket dir = %+v, want allow", v)
	}
}

// TestCodexOneCommandOneApproval: when Codex both runs Athena's PermissionRequest hook and asks Athena for approval of
// the same command in the same turn, in either order, the owner answers one Approval, and both get its verdict.
func TestCodexOneCommandOneApproval(t *testing.T) {
	const cmd = "kubectl apply -f app.yaml"
	for name, body := range map[string][]string{
		"hook first":    {codexHook(EventPermissionRequest, cmd), codexApprovalRequest(cmd, true)},
		"request first": {codexApprovalRequest(cmd, false), codexHook(EventPermissionRequest, cmd)},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCodexHarness(t, codexTurn(append([]string{codexCommandItem("item/started", "item_0003", cmd)}, body...)...), nil)
			tk := h.submitCodex("Deploy")
			h.approveOnly(tk.ID)
			h.eventually("Codex is told accept", func() bool { return h.rpcHas(`"decision":"accept"`) })
			h.eventually("the hook allowed", func() bool { return h.rpcHas(`\"behavior\":\"allow\"`) })
			cur, _ := h.ticket(tk.ID)
			if cur.Stats.Escalations != 1 {
				t.Fatalf("escalations = %d, want one Approval for the one command", cur.Stats.Escalations)
			}
			if pending, _ := h.eng.PendingApprovals(); len(pending) != 0 {
				t.Fatalf("pending approvals left: %+v", pending)
			}
		})
	}
}

func TestCodexResumeUsesThreadID(t *testing.T) {
	t.Setenv("ATHENA_FAKE_CODEX_EXIT_MID_TURN", "1")
	h := newCodexHarness(t, codexTurn(`{"method":"item/started","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","item":{"id":"item_0002","type":"agentMessage","text":"","phase":"final_answer"},"startedAtMs":1893456000750}}`), nil)
	tk := h.submitCodex("Crash and resume")
	h.eventually("the crashed session resumed its thread", func() bool {
		return h.rpcHas(`"method":"thread/resume"`) && h.rpcHas(`"threadId":"thr_fixture_`+tk.ID+`"`)
	})
	for _, l := range h.rpc() {
		if strings.Contains(l, `"thread/resume"`) && !strings.Contains(l, `"threadId":"thr_fixture_`+tk.ID+`"`) {
			t.Fatalf("resume of another thread: %s", l)
		}
	}
}

func TestMarkerWrittenPerProcess(t *testing.T) {
	t.Setenv("ATHENA_FAKE_CODEX_EXIT_MID_TURN", "1")
	h := newCodexHarness(t, codexTurn(), nil)
	tk := h.submitCodex("Crash repeatedly")
	h.eventually("the assignment failed after its restarts", func() bool {
		cur, _ := h.ticket(tk.ID)
		a := cur.LatestAssignment()
		return a.Report != nil && a.Report.State == core.ReportFailed
	})
	cur, _ := h.ticket(tk.ID)
	a := cur.LatestAssignment()
	var markers []render.Marker
	for _, l := range strings.Split(h.transcript(tk.ID), "\n") {
		var m struct {
			Athena     string `json:"athena"`
			Runtime    string `json:"runtime"`
			Model      string `json:"model"`
			Assignment string `json:"assignment"`
			Role       string `json:"role"`
			Reason     string `json:"reason"`
		}
		if json.Unmarshal([]byte(l), &m) == nil && m.Athena == "session" {
			markers = append(markers, render.Marker{Runtime: m.Runtime, Model: m.Model, Assignment: m.Assignment, Role: m.Role, Reason: m.Reason})
		}
	}
	if len(markers) != 1+maxRestarts {
		t.Fatalf("markers = %+v, want one per process (%d)", markers, 1+maxRestarts)
	}
	for i, m := range markers {
		// The Ticket names no model: Codex runs its own default, which the first process's thread named.
		want, model := "resume", a.Model
		if i == 0 {
			want, model = "start", ""
		}
		if m.Reason != want || m.Runtime != "codex" || m.Assignment != a.ID || m.Role != "implement" || m.Model != model {
			t.Fatalf("marker %d = %+v, want %s on model %q", i, m, want, model)
		}
	}
	if a.Model == "" {
		t.Fatal("the Assignment records no model: Codex's default should be named by its thread")
	}
	// A Claude Code session's transcript opens with its own marker.
	ct := h.submit("Claude task", "")
	h.running(ct.ID)
	first, _, _ := strings.Cut(h.transcript(ct.ID), "\n")
	if !strings.Contains(first, `"athena":"session"`) || !strings.Contains(first, `"runtime":"claude"`) || !strings.Contains(first, `"reason":"start"`) {
		t.Fatalf("claude transcript opens with %s", first)
	}
}

func TestCodexNotReadyRefusesWithWhy(t *testing.T) {
	t.Setenv("ATHENA_FAKE_CODEX_LOGGED_OUT", "1")
	h := newCodexHarness(t, codexTurn(), nil)
	tk := h.submitCodex("Pinned to a logged-out Codex")
	h.eventually("the pinned ticket failed", func() bool {
		cur, _ := h.ticket(tk.ID)
		a := cur.LatestAssignment()
		return a.Report != nil && a.Report.State == core.ReportFailed
	})
	cur, _ := h.ticket(tk.ID)
	if s := cur.LatestAssignment().Report.Summary; !strings.Contains(s, codex.NotLoggedIn) {
		t.Fatalf("summary = %q, want the Why %q", s, codex.NotLoggedIn)
	}
	if h.rpcHas(`"method":"thread/start"`) {
		t.Fatal("a session started on a Codex that is not ready")
	}
}

func TestAutoRunsClaudeUntilTheChooser(t *testing.T) {
	h := newHarness(t, nil)
	tk, err := h.eng.SubmitOwnerTask("Auto task", core.TaskOptions{Runtime: core.RuntimeAuto})
	if err != nil {
		t.Fatal(err)
	}
	h.running(tk.ID)
	if inv := h.invocations(); len(inv) != 1 || inv[0].Env["ATHENA_RUNTIME"] != "claude" {
		t.Fatalf("invocations = %+v, want one Claude Code session", inv)
	}
	if _, err := h.eng.SubmitOwnerTask("Bad", core.TaskOptions{Runtime: "gemini"}); err == nil {
		t.Fatal("an unknown runtime was accepted")
	}
}

// pendingWith waits for one pending Approval whose summary holds want, and returns it.
func (h *codexHarness) pendingWith(want string) core.Approval {
	h.t.Helper()
	var found core.Approval
	h.eventually("an approval about "+want, func() bool {
		pending, _ := h.eng.PendingApprovals()
		for _, p := range pending {
			if strings.Contains(p.Summary, want) {
				found = p
				return true
			}
		}
		return false
	})
	return found
}

// On a node that sandboxes its workers, what Codex asks to run or write outside its sandbox needs a person even when
// the rules allow it, through either door; a rules deny stays a deny, and the PreToolUse hook (in the sandbox) is the
// rules' alone.
func TestCodexUnsandboxedAsksNeedApproval(t *testing.T) {
	h := newCodexHarness(t, codexTurn(codexApprovalRequest("ls -la", true)), func(c *Config) { c.Sandbox = &SandboxConfig{} })
	tk := h.submitCodex("List")
	ap := h.approveOnly(tk.ID)
	if ap.Rule != ruleCodexUnsandboxed || ap.Summary != "Run outside the sandbox: ls -la" {
		t.Fatalf("approval = %+v", ap)
	}
	h.eventually("Codex is told accept", func() bool { return h.rpcHas(`"decision":"accept"`) })
	n, s := h.n, h.n.liveSession(tk.ID)
	if s == nil {
		t.Fatal("no live session")
	}
	ctx := context.Background()
	if v := n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "command", Command: "cat ~/.ssh/id_rsa", TurnID: "t2"}); v.Allow {
		t.Fatalf("credential read outside the sandbox = %+v, want deny", v)
	}
	// A file approval names every path and the write grant.
	got := make(chan rt.Verdict, 1)
	go func() {
		got <- n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "file", Paths: []string{"a.txt", "b/c.txt", "b"}, TurnID: "t2",
			Reason: "needs b (and writes under b for the rest of the session)"})
	}()
	fp := h.pendingWith("a.txt")
	if fp.Rule != ruleCodexUnsandboxed || fp.Summary != "Run outside the sandbox: a.txt, b/c.txt, b — needs b (and writes under b for the rest of the session)" {
		t.Fatalf("file approval = %+v", fp)
	}
	if err := h.eng.AnswerApproval(fp.ID, false, "no"); err != nil {
		t.Fatal(err)
	}
	if v := <-got; v.Allow {
		t.Fatalf("denied file approval = %+v", v)
	}
	// The PermissionRequest hook asks too; PreToolUse does not.
	hook := func(event, command string) chan HookResponse {
		in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command}, "turn_id": "t3"})
		c := make(chan HookResponse, 1)
		go func() {
			c <- n.handleHook(ctx, HookRequest{Event: event, Ticket: tk.ID, Assignment: s.a.ID, Runtime: core.RuntimeCodex, Input: in})
		}()
		return c
	}
	if r := <-hook(EventPreToolUse, "go test ./..."); r.Verdict != VerdictAllow {
		t.Fatalf("PreToolUse = %+v, want allow", r)
	}
	pr := hook(EventPermissionRequest, "go test ./...")
	hp := h.pendingWith("go test ./...")
	if hp.Rule != ruleCodexUnsandboxed {
		t.Fatalf("hook approval = %+v", hp)
	}
	if err := h.eng.AnswerApproval(hp.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if r := <-pr; r.Verdict != VerdictAllow {
		t.Fatalf("approved PermissionRequest = %+v", r)
	}
}

// An answer is shared only between Codex's PermissionRequest hook and its approval request, once each, and only for
// exactly the same action: a broader ask, a PreToolUse ask, or the same door asking again asks anew.
func TestCodexSharedVerdictScope(t *testing.T) {
	h := newCodexHarness(t, codexTurn(codexApprovalRequest("kubectl apply -f app.yaml", true)), nil)
	tk := h.submitCodex("Deploy")
	h.approveOnly(tk.ID)
	h.eventually("Codex is told accept", func() bool { return h.rpcHas(`"decision":"accept"`) })
	n, s := h.n, h.n.liveSession(tk.ID)
	ctx := context.Background()
	file := func(turn string, paths ...string) chan rt.Verdict {
		c := make(chan rt.Verdict, 1)
		go func() { c <- n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "file", Paths: paths, TurnID: turn}) }()
		return c
	}
	hook := func(event, turn, command string) chan HookResponse {
		in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command}, "turn_id": turn})
		c := make(chan HookResponse, 1)
		go func() {
			c <- n.handleHook(ctx, HookRequest{Event: event, Ticket: tk.ID, Assignment: s.a.ID, Runtime: core.RuntimeCodex, Input: in})
		}()
		return c
	}
	answer := func(want string, yes bool) {
		t.Helper()
		ap := h.pendingWith(want)
		if err := h.eng.AnswerApproval(ap.ID, yes, ""); err != nil {
			t.Fatal(err)
		}
	}
	// A broader file ask in the same turn is a new Approval, naming every path it escalates.
	a := file("t1", "../out/a")
	answer("../out/a", true)
	if v := <-a; !v.Allow {
		t.Fatalf("approved = %+v", v)
	}
	b := file("t1", "../out/a", "src/ok.go", "../out/secret")
	answer("Write: ../out/a, ../out/secret", false)
	if v := <-b; v.Allow {
		t.Fatalf("broader ask = %+v, want its own (denied) Approval", v)
	}
	// The hook and the request share one answer, once: the same door asking again asks anew.
	const cmd = "kubectl apply -f x.yaml"
	r1 := hook(EventPermissionRequest, "t2", cmd)
	answer(cmd, true)
	if r := <-r1; r.Verdict != VerdictAllow {
		t.Fatalf("hook = %+v", r)
	}
	if v := n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "command", Command: cmd, TurnID: "t2"}); !v.Allow {
		t.Fatalf("the request after the hook = %+v, want the shared allow", v)
	}
	again := make(chan rt.Verdict, 1)
	go func() {
		again <- n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "command", Command: cmd, TurnID: "t2"})
	}()
	answer(cmd, false)
	if v := <-again; v.Allow {
		t.Fatalf("the command run again = %+v, want a new (denied) Approval", v)
	}
	// A PreToolUse escalation is not shared with the request; exact text: another spacing is another command.
	p := hook(EventPreToolUse, "t3", cmd)
	answer(cmd, true)
	if r := <-p; r.Verdict != VerdictAllow {
		t.Fatalf("PreToolUse = %+v", r)
	}
	q := make(chan rt.Verdict, 1)
	go func() { q <- n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "command", Command: cmd, TurnID: "t3"}) }()
	answer(cmd, false)
	if v := <-q; v.Allow {
		t.Fatalf("request after a PreToolUse allow = %+v, want its own Approval", v)
	}
	r2 := hook(EventPermissionRequest, "t4", cmd)
	answer(cmd, true)
	<-r2
	sp := make(chan rt.Verdict, 1)
	go func() {
		sp <- n.approveRequest(ctx, s, rt.ApprovalRequest{Kind: "command", Command: "kubectl  apply -f x.yaml", TurnID: "t4"})
	}()
	answer("kubectl  apply", false)
	if v := <-sp; v.Allow {
		t.Fatalf("differently spaced command = %+v, want its own Approval", v)
	}
}

// A Codex hook deny shows no result; a later item means the model saw it, so a ready report after it is accepted.
func TestCodexDenyThenLaterItemAcceptsReady(t *testing.T) {
	h := newCodexHarness(t, codexTurn(
		codexHook(EventPreToolUse, "cat ~/.ssh/id_rsa"),
		codexCommandItem("item/started", "item_0003", "cat ~/.ssh/id_rsa"),
		codexCommandItem("item/completed", "item_0003", "cat ~/.ssh/id_rsa"),
		`{"method":"item/completed","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","item":{"id":"item_0004","type":"agentMessage","text":"That file is off limits; reporting.","phase":"commentary"},"completedAtMs":1893456003500}}`,
		`{"fake":"mcp","tool":"report","arguments":{"status":"ready","summary":"done without the key"}}`,
	), nil)
	tk := h.submitCodex("Read a key, then report")
	h.eventually("the ready report ends the ticket", func() bool {
		cur, _ := h.ticket(tk.ID)
		return cur.Ended != nil
	})
	cur, _ := h.ticket(tk.ID)
	if cur.Ended.Outcome != core.OutcomeCompleted || cur.Stats.HookDenials != 1 {
		t.Fatalf("ended %+v, denials %d", cur.Ended, cur.Stats.HookDenials)
	}
}

// The rules use the runtime the session runs on: ATHENA_RUNTIME names it, and a decision hook naming another is
// denied.
func TestHookRuntimeIsTheSessions(t *testing.T) {
	h := newHarness(t, nil)
	tk := h.submit("Claude work", "")
	_, a := h.running(tk.ID)
	s := h.n.liveSession(tk.ID)
	if !slices.Contains(h.n.variables(s), "ATHENA_RUNTIME=claude") {
		t.Fatalf("variables = %v", h.n.variables(s))
	}
	in := json.RawMessage(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)
	for rtName, want := range map[string]string{"": VerdictAllow, core.RuntimeClaude: VerdictAllow, core.RuntimeCodex: VerdictDeny} {
		r := h.n.handleHook(context.Background(), HookRequest{Event: EventPreToolUse, Ticket: tk.ID, Assignment: a.ID, Runtime: rtName, Input: in})
		if r.Verdict != want {
			t.Errorf("hook --runtime %q: %+v, want %s", rtName, r, want)
		}
	}
	ch := newCodexHarness(t, codexTurn(codexApprovalRequest("kubectl apply -f app.yaml", true)), nil)
	ctk := ch.submitCodex("Codex work")
	ch.approveOnly(ctk.ID)
	cs := ch.n.liveSession(ctk.ID)
	if !slices.Contains(ch.n.variables(cs), "ATHENA_RUNTIME=codex") || hookRuntime(cs) != core.RuntimeCodex {
		t.Fatalf("codex variables = %v", ch.n.variables(cs))
	}
	r := ch.n.handleHook(context.Background(), HookRequest{Event: EventPreToolUse, Ticket: ctk.ID, Assignment: cs.a.ID, Runtime: core.RuntimeClaude, Input: in})
	if r.Verdict != VerdictDeny || !strings.Contains(r.Reason, "runs on codex") {
		t.Fatalf("claude hook on a codex session: %+v", r)
	}
}

// countingCodex is Codex that counts its Invalidate calls.
type countingCodex struct {
	*codex.Runtime
	invalidated *atomic.Int32
}

func (c countingCodex) Invalidate() { c.invalidated.Add(1); c.Runtime.Invalidate() }

// A Start that finds the session out of Athena's control fails the Ticket with why, and the next launch checks anew.
func TestCodexStartUncontrolledFailsWithWhy(t *testing.T) {
	t.Setenv("ATHENA_FAKE_CODEX_TRUST_PROJECT", "1")
	repo := newRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".codex", "config.toml"), []byte("model = \"other\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".codex/config.toml")
	git(t, repo, "commit", "-q", "-m", "codex settings")
	var invalidated atomic.Int32
	h := newCodexHarness(t, codexTurn(), func(c *Config) {
		c.Runtimes[core.RuntimeCodex] = countingCodex{Runtime: c.Runtimes[core.RuntimeCodex].(*codex.Runtime), invalidated: &invalidated}
	})
	tk, err := h.eng.SubmitOwnerTask("Work in a repo with Codex settings", core.TaskOptions{Dir: repo, Runtime: core.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	var r *core.Report
	h.eventually("failed report", func() bool {
		cur, _ := h.ticket(tk.ID)
		r = cur.LatestAssignment().Report
		return r != nil && r.State == core.ReportFailed
	})
	if !strings.Contains(r.Summary, "a repository's Codex settings are loaded") {
		t.Fatalf("summary = %q", r.Summary)
	}
	if invalidated.Load() != 1 {
		t.Fatalf("Invalidate called %d times, want 1", invalidated.Load())
	}
}

// A share key covers every call, in any order, by exact text; an input that does not decode is keyed by its bytes.
func TestShareKey(t *testing.T) {
	w := func(p string) ToolCall {
		return ToolCall{Tool: "Write", Input: mustJSON(map[string]string{"file_path": p})}
	}
	if shareKey("t", []ToolCall{w("a"), w("b")}) != shareKey("t", []ToolCall{w("b"), w("a")}) {
		t.Error("the key depends on the calls' order")
	}
	if shareKey("t", []ToolCall{w("a")}) == shareKey("t", []ToolCall{w("a"), w("b")}) {
		t.Error("a broader ask has the same key")
	}
	if shareKey("", []ToolCall{w("a")}) != "" {
		t.Error("an ask without a turn is shared")
	}
	bash := func(c string) ToolCall {
		return ToolCall{Tool: "Bash", Input: mustJSON(map[string]string{"command": c})}
	}
	if shareKey("t", []ToolCall{bash(`echo "a  b"`)}) == shareKey("t", []ToolCall{bash(`echo "a b"`)}) {
		t.Error("spacing is collapsed")
	}
	bad1, bad2 := ToolCall{Tool: "Bash", Input: json.RawMessage(`{"command":`)}, ToolCall{Tool: "Bash", Input: json.RawMessage(`{"command":"x`)}
	if callSubject(bad1) != `{"command":` || shareKey("t", []ToolCall{bad1}) == shareKey("t", []ToolCall{bad2}) {
		t.Errorf("undecodable input subject = %q", callSubject(bad1))
	}
}

// Athena's own tool is no later item: a ready report filed in the step of a denied call stays refused.
func TestCodexDenyThenReportItemStillRefused(t *testing.T) {
	h := newCodexHarness(t, codexTurn(
		codexHook(EventPreToolUse, "cat ~/.ssh/id_rsa"),
		codexCommandItem("item/started", "item_0003", "cat ~/.ssh/id_rsa"),
		codexCommandItem("item/completed", "item_0003", "cat ~/.ssh/id_rsa"),
		`{"method":"item/started","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","item":{"id":"item_0007","type":"mcpToolCall","server":"athena","tool":"report","status":"inProgress","arguments":{"status":"ready","summary":"read it"},"result":null,"error":null,"durationMs":null},"startedAtMs":1893456003750}}`,
		`{"fake":"mcp","tool":"report","arguments":{"status":"ready","summary":"read it"}}`,
	), nil)
	tk := h.submitCodex("Read a key and report at once")
	h.eventually("the report was answered", func() bool { return h.rpcHas(`"fake":"mcp"`) })
	var rec struct {
		IsError bool   `json:"isError"`
		Text    string `json:"text"`
	}
	for _, l := range h.rpc() {
		if strings.Contains(l, `"fake":"mcp"`) {
			if err := json.Unmarshal([]byte(l), &rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !rec.IsError || !strings.Contains(rec.Text, "failed") {
		t.Fatalf("report answer = %+v, want the same-step refusal", rec)
	}
	if cur, _ := h.ticket(tk.ID); cur.Ended != nil {
		t.Fatalf("ticket ended: %+v", cur.Ended)
	}
}

// --- runtimes per Assignment (T8) ---

// newTracker is a quota Tracker in the test's own directory.
func newTracker(t *testing.T) *quota.Tracker {
	t.Helper()
	tr, err := quota.New(filepath.Join(t.TempDir(), "quota.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// An auto Ticket runs on the runtime the chooser picks: Codex while Claude Code is limited. Its Assignment records the
// runtime, the session's quota reading reaches the Tracker, and Status shows both runtimes.
func TestAutoTicketLaunchesOnChosenRuntime(t *testing.T) {
	tr := newTracker(t)
	reset := time.Now().Add(time.Hour)
	tr.Hit(core.RuntimeClaude, rt.LimitHit{Runtime: core.RuntimeClaude, ResetsAt: &reset})
	h := newCodexHarness(t, codexTurn(), func(c *Config) { c.Quota = tr })
	// An auto Ticket's model is Claude Code's: on Codex the session runs Codex's own default.
	tk, err := h.eng.SubmitOwnerTask("Pick a runtime", core.TaskOptions{Runtime: core.RuntimeAuto, Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	_, a := h.running(tk.ID)
	if a.Runtime != core.RuntimeCodex {
		t.Fatalf("assignment runtime = %q, want codex", a.Runtime)
	}
	if h.rpcHas(`"opus"`) || a.Model == "opus" {
		t.Fatalf("codex was given the Ticket's Claude Code model (assignment model %q)", a.Model)
	}
	if !h.rpcHas(`"method":"thread/start"`) || len(h.invocations()) != 0 {
		t.Fatalf("want one Codex session and no Claude Code one (claude ran %d times)", len(h.invocations()))
	}
	h.eventually("codex's quota reading reached the tracker", func() bool { return tr.State(core.RuntimeCodex).Measured })
	var claudeStatus, codexStatus RuntimeStatus
	h.eventually("status lists both runtimes", func() bool {
		for _, r := range h.n.Status().Runtimes {
			switch r.Name {
			case core.RuntimeClaude:
				claudeStatus = r
			case core.RuntimeCodex:
				codexStatus = r
			}
		}
		return codexStatus.Live == 1 && codexStatus.Quota.Measured
	})
	if !claudeStatus.Ready || !claudeStatus.Quota.Limited(time.Now()) || claudeStatus.Live != 0 {
		t.Fatalf("claude status = %+v", claudeStatus)
	}
	if !codexStatus.Ready || codexStatus.Version == "" {
		t.Fatalf("codex status = %+v", codexStatus)
	}

	// With both free, the preferred runtime (Claude Code) runs the next auto Ticket.
	h2 := newHarness(t, nil)
	tk2, err := h2.eng.SubmitOwnerTask("Auto task", core.TaskOptions{Runtime: core.RuntimeAuto})
	if err != nil {
		t.Fatal(err)
	}
	_, a2 := h2.running(tk2.ID)
	if a2.Runtime != core.RuntimeClaude || h2.invocations()[0].Env["ATHENA_RUNTIME"] != core.RuntimeClaude {
		t.Fatalf("assignment runtime = %q, invocations %+v", a2.Runtime, h2.invocations())
	}
}

// A launch whose runtime is limited waits, queued and with no session, then starts once the limit resets: the Ticket
// stays working/queued meanwhile and never waits for a person.
func TestLaunchWaitsThenStarts(t *testing.T) {
	tr := newTracker(t)
	reset := time.Now().Add(1500 * time.Millisecond)
	tr.Hit(core.RuntimeClaude, rt.LimitHit{Runtime: core.RuntimeClaude, ResetsAt: &reset})
	h := newHarness(t, func(c *Config) { c.Quota = tr })
	tk := h.submit("Wait for the reset", "")
	h.never("a session while claude is limited", time.Second, func() bool {
		cur, st := h.ticket(tk.ID)
		if st != (core.Stage{Stage: "working", Action: "queued"}) {
			t.Fatalf("stage while waiting = %+v", st)
		}
		return len(h.invocations()) > 0 || cur.LatestAssignment().Report != nil
	})
	if h.n.Status().SlotsInUse != 0 {
		t.Fatalf("the waiting launch holds a slot: %+v", h.n.Status())
	}
	_, a := h.running(tk.ID)
	if a.Runtime != core.RuntimeClaude || len(h.invocations()) != 1 {
		t.Fatalf("after the reset: runtime %q, %d sessions", a.Runtime, len(h.invocations()))
	}
	if !time.Now().After(reset) {
		t.Fatal("the session started before the reset")
	}
}

// testClock is a clock tests move by hand, safe for concurrent use.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// A new Assignment resumes the Ticket's last session only when it runs on that session's runtime (and role): on
// another runtime it starts fresh.
func TestResumeOnlySameRuntimeSession(t *testing.T) {
	clk := &testClock{t: time.Now()}
	tr, err := quota.New(filepath.Join(t.TempDir(), "quota.json"), clk.now)
	if err != nil {
		t.Fatal(err)
	}
	h := newCodexHarness(t, codexTurn(), func(c *Config) { c.Quota = tr })
	tk, err := h.eng.SubmitOwnerTask("Move between runtimes", core.TaskOptions{Runtime: core.RuntimeAuto})
	if err != nil {
		t.Fatal(err)
	}
	_, first := h.running(tk.ID)
	if first.Runtime != core.RuntimeClaude || first.SessionID != fixtureSessionID {
		t.Fatalf("first assignment = %+v", first)
	}
	// limit makes runtime limited for an hour; retry fails the running Assignment and retries the Ticket.
	limit := func(runtime string) {
		reset := clk.now().Add(time.Hour)
		tr.Hit(runtime, rt.LimitHit{Runtime: runtime, ResetsAt: &reset})
	}
	retry := func() core.Assignment {
		t.Helper()
		cur, _ := h.ticket(tk.ID)
		if err := h.eng.FileReport(tk.ID, core.ReportInput{AssignmentID: cur.LatestAssignment().ID, State: core.ReportFailed, Summary: "stuck"}); err != nil {
			t.Fatal(err)
		}
		if err := h.eng.RetryTicket(tk.ID); err != nil {
			t.Fatal(err)
		}
		cur, _ = h.ticket(tk.ID)
		id := cur.LatestAssignment().ID
		var a core.Assignment
		h.eventually("the retry started", func() bool {
			cur, _ := h.ticket(tk.ID)
			a = *cur.LatestAssignment()
			return a.ID == id && a.SessionID != ""
		})
		return a
	}
	// Claude Code is limited: the retry runs on Codex, in a new thread, not in claude's session.
	limit(core.RuntimeClaude)
	second := retry()
	if second.Runtime != core.RuntimeCodex || h.rpcHas(`"method":"thread/resume"`) || !h.rpcHas(`"method":"thread/start"`) {
		t.Fatalf("second assignment = %+v; rpc %v", second, h.rpc())
	}
	if err := os.Remove(h.harness.argsLog); err != nil {
		t.Fatal(err)
	}
	// Claude Code's limit resets and Codex is limited: the next retry resumes Claude Code's own session.
	clk.advance(2 * time.Hour)
	limit(core.RuntimeCodex)
	third := retry()
	if third.Runtime != core.RuntimeClaude {
		t.Fatalf("third assignment = %+v", third)
	}
	inv := h.invocations()
	if len(inv) != 1 {
		t.Fatalf("%d claude sessions, want 1", len(inv))
	}
	if id, ok := argValue(inv[0].Args, "--resume"); !ok || id != fixtureSessionID {
		t.Fatalf("claude args = %v, want --resume %s", inv[0].Args, fixtureSessionID)
	}

	// The rule itself: same runtime and role only; a review starts fresh.
	tkt := core.Ticket{Execution: &core.Execution{Runtime: core.RuntimeAuto}, Assignments: []core.Assignment{
		{ID: "a1", SessionID: "claude-1", Runtime: core.RuntimeClaude},
		{ID: "a2", SessionID: "codex-1", Runtime: core.RuntimeCodex},
		{ID: "a3", SessionID: "review-1", Runtime: core.RuntimeClaude, Role: core.RoleReview},
		{ID: "a4"},
	}}
	for _, c := range []struct{ runtime, role, want string }{
		{core.RuntimeClaude, "", "claude-1"}, {core.RuntimeCodex, "", "codex-1"}, {core.RuntimeClaude, core.RoleReview, ""},
		{"other", "", ""},
	} {
		if got := previousSessionID(tkt, "a4", c.runtime, c.role); got != c.want {
			t.Errorf("previousSessionID(%s, %q) = %q, want %q", c.runtime, c.role, got, c.want)
		}
	}
	// An Assignment from before runtimes were recorded ran on the Ticket's runtime (claude for auto).
	legacy := core.Ticket{Execution: &core.Execution{Runtime: core.RuntimeAuto}, Assignments: []core.Assignment{{ID: "a1", SessionID: "old"}, {ID: "a2"}}}
	if got := previousSessionID(legacy, "a2", core.RuntimeClaude, ""); got != "old" {
		t.Errorf("legacy = %q", got)
	}
	if got := previousSessionID(legacy, "a2", core.RuntimeCodex, ""); got != "" {
		t.Errorf("legacy on codex = %q", got)
	}
}

// A quota change tells OnStatus, and Status shows it, with no session involved.
func TestStatusFiresOnQuotaChange(t *testing.T) {
	tr := newTracker(t)
	var calls atomic.Int32
	h := newHarness(t, func(c *Config) { c.Quota = tr; c.OnStatus = func() { calls.Add(1) } })
	before := calls.Load()
	tr.Observe(core.RuntimeClaude, []rt.Window{{Name: "5h", Used: 0.4}})
	h.eventually("OnStatus after the quota changed", func() bool { return calls.Load() > before })
	st := h.n.Status()
	if len(st.Runtimes) != 1 || st.Runtimes[0].Name != core.RuntimeClaude || !st.Runtimes[0].Ready || !st.Runtimes[0].Quota.Measured {
		t.Fatalf("runtimes = %+v", st.Runtimes)
	}
}

// countingChooser counts its choices per Ticket.
type countingChooser struct {
	Chooser
	mu sync.Mutex
	n  map[string]int
}

func (c *countingChooser) Choose(in ChoiceInput) Choice {
	c.mu.Lock()
	c.n[in.Ticket.ID]++
	c.mu.Unlock()
	return c.Chooser.Choose(in)
}

func (c *countingChooser) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[id]
}

// A launch held back by its runtime's session cap waits, queued, for a session to end: it is not chosen again in a
// loop meanwhile, and it starts once the session holding the cap ends. Auto and pinned launches alike, and one whose
// other runtime is limited as well.
func TestSessionCapWaitsWithoutLooping(t *testing.T) {
	for _, c := range []struct {
		name    string
		runtime string
		codex   bool // Codex is there too, limited
	}{
		{"auto", core.RuntimeAuto, false},
		{"pinned", core.RuntimeClaude, false},
		{"capped plus limited", core.RuntimeAuto, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := newTracker(t)
			ch := &countingChooser{Chooser: HeadroomChooser{Preferred: core.RuntimeClaude, MaxSessions: map[string]int{core.RuntimeClaude: 1}},
				n: map[string]int{}}
			mod := func(cfg *Config) { cfg.Quota, cfg.Chooser = tr, ch }
			var h *harness
			if c.codex {
				reset := time.Now().Add(time.Hour)
				tr.Hit(core.RuntimeCodex, rt.LimitHit{Runtime: core.RuntimeCodex, ResetsAt: &reset})
				h = newCodexHarness(t, codexTurn(), mod).harness
			} else {
				h = newHarness(t, mod)
			}
			t.Setenv("ATHENA_FAKE_FIXTURE", openTurnFixture(t))
			submit := func(text string) core.Ticket {
				tk, err := h.eng.SubmitOwnerTask(text, core.TaskOptions{Runtime: c.runtime})
				if err != nil {
					t.Fatal(err)
				}
				return tk
			}
			first := submit("Holds the cap")
			_, a := h.running(first.ID)
			second := submit("Waits for the cap")
			h.never("the second ticket starts while the cap is held", time.Second, func() bool {
				cur, st := h.ticket(second.ID)
				if st != (core.Stage{Stage: "working", Action: "queued"}) {
					t.Fatalf("stage while waiting = %+v", st)
				}
				return cur.LatestAssignment().SessionID != ""
			})
			if n := ch.count(second.ID); n < 1 || n > 3 {
				t.Fatalf("the waiting launch was chosen %d times in 1 s, want 1 to 3", n)
			}
			if r := h.report(first.ID, a.ID, core.ReportReady, "done"); !r.OK {
				t.Fatalf("report = %+v", r)
			}
			_, a2 := h.running(second.ID)
			if a2.Runtime != core.RuntimeClaude {
				t.Fatalf("second ran on %q", a2.Runtime)
			}
			if n := ch.count(second.ID); n > 4 {
				t.Fatalf("the waiting launch was chosen %d times in all", n)
			}
		})
	}
}

// --- usage-limit failover ---

const limitSessionID = "limit-session-0001"

// claudeLimitFixture is a Claude Code turn that says something and stops at its usage limit: a rejected
// rate_limit_event resetting at reset (a unix time, or fakeclaude's {now+Ns}; "" for none), then the error result.
func claudeLimitFixture(t *testing.T, reset string) string {
	t.Helper()
	lines := []string{
		`{"type":"system","subtype":"init","cwd":"/home/user/work","session_id":"` + limitSessionID + `","model":"claude-haiku-4-5","tools":[]}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I am halfway through the parser."}]},"session_id":"` + limitSessionID + `"}`,
	}
	if reset != "" {
		lines = append(lines, `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":`+reset+`,"rateLimitType":"five_hour"},"session_id":"`+limitSessionID+`"}`)
	}
	lines = append(lines, `{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached","session_id":"`+limitSessionID+`","num_turns":1}`)
	p := filepath.Join(t.TempDir(), "limit.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// limitedAssignment waits until the Ticket's first Assignment ended at its usage limit, and returns the Ticket.
func (h *harness) limitedAssignment(id string) core.Ticket {
	h.t.Helper()
	var tk core.Ticket
	h.eventually("the first assignment ends at its usage limit", func() bool {
		tk, _ = h.ticket(id)
		return len(tk.Assignments) > 1 && tk.Assignments[0].EndedReason == core.ReasonUsageLimit
	})
	return tk
}

// codexTurnStart is the text of the first turn/start the Codex session sent, "" before it sent one.
func (h *codexHarness) codexTurnStart() string {
	for _, l := range h.rpc() {
		var m struct {
			Method string `json:"method"`
			Params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(l), &m) == nil && m.Method == "turn/start" && len(m.Params.Input) > 0 {
			return m.Params.Input[0].Text
		}
	}
	return ""
}

// A Claude Code session that stops at its usage limit fails over to Codex, with no person asked: its work in progress
// is checkpointed, its Assignment ends at the limit, and Codex continues in the same work dir, its first message
// telling it, after the worker preamble, what the Claude session knew.
func TestClaudeLimitFailsOverToCodex(t *testing.T) {
	h := newCodexHarness(t, codexTurn(), nil)
	hold := filepath.Join(t.TempDir(), "hold")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	t.Setenv("ATHENA_FAKE_FIXTURE", claudeLimitFixture(t, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)))
	repo := newRepo(t)
	tk, err := h.eng.SubmitOwnerTask("Fix the parser", core.TaskOptions{Dir: repo, Runtime: core.RuntimeAuto})
	if err != nil {
		t.Fatal(err)
	}
	h.eventually("claude runs", func() bool { return len(h.invocations()) == 1 })
	wd, err := WorkingDir(h.cfg.WorkRoot, tk)
	if err != nil {
		t.Fatal(err)
	}
	// The worker leaves work in progress, then its turn stops at the limit.
	h.eventually("the worktree exists", func() bool { _, err := os.Stat(wd); return err == nil })
	if err := os.WriteFile(filepath.Join(wd, "wip.txt"), []byte("half a parser\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cur := h.limitedAssignment(tk.ID)
	h.eventually("codex gets its first message", func() bool { return h.codexTurnStart() != "" })
	limited := cur.Assignments[0]
	if limited.Limit == nil || limited.Limit.Runtime != core.RuntimeClaude || limited.Limit.ResetsAt == nil ||
		!strings.Contains(limited.Limit.Message, "usage limit") || limited.Report != nil {
		t.Fatalf("limited assignment = %+v, limit %+v", limited, limited.Limit)
	}
	cur, st := h.ticket(tk.ID)
	if next := cur.Assignments[1]; next.Runtime != core.RuntimeCodex || next.Role != "" {
		t.Fatalf("next assignment = %+v", next)
	}
	if st.Stage == "person" || cur.Attention != nil {
		t.Fatalf("stage %+v, attention %+v", st, cur.Attention)
	}
	if !h.n.Quota().State(core.RuntimeClaude).Limited(time.Now()) {
		t.Fatal("claude is not limited in the quota")
	}

	// Codex works in the same directory.
	b, err := os.ReadFile(strings.ReplaceAll(h.argsLog, "{ticket}", tk.ID))
	if err != nil {
		t.Fatal(err)
	}
	var inv invocation
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatal(err)
	}
	if claudeDir := h.invocations()[0].Env["ATHENA_DIR"]; inv.Env["ATHENA_DIR"] != wd || claudeDir != wd {
		t.Fatalf("codex in %q, claude in %q, want both in %q", inv.Env["ATHENA_DIR"], claudeDir, wd)
	}
	// Its first message: the preamble, then the handoff note with the Ticket's request and Claude's last words.
	text := h.codexTurnStart()
	opening := fmt.Sprintf(handoffOpening, "Claude Code")
	pre, note, ok := strings.Cut(text, "\n"+opening)
	if !ok || !strings.HasPrefix(pre, "You are the worker of Athena Ticket "+tk.ID) || !strings.HasSuffix(pre, "ATHENA_RUNTIME=codex\n") {
		t.Fatalf("first message:\n%s", text)
	}
	for _, want := range []string{"Fix the parser", "I am halfway through the parser.", "Untracked files: 1"} {
		if !strings.Contains(note, want) {
			t.Errorf("handoff lacks %q:\n%s", want, note)
		}
	}
	// The work in progress is in the checkpoint; the branch has no new commit.
	ref := "refs/athena/" + strings.ToLower(tk.ID) + "/checkpoint"
	if tree := git(t, repo, "ls-tree", "--name-only", ref); !strings.Contains(tree, "wip.txt") {
		t.Fatalf("checkpoint tree = %q", tree)
	}
	if git(t, repo, "rev-parse", ref+"^") != git(t, repo, "rev-parse", "refs/heads/"+branchName(tk)) {
		t.Fatal("the checkpoint is not on the branch's head, or the branch moved")
	}
	// The transcript says where the Ticket moved, and why.
	h.eventually("the failover marker", func() bool {
		return strings.Contains(h.transcript(tk.ID), `"runtime":"codex"`) && strings.Contains(h.transcript(tk.ID), `"reason":"failover"`)
	})
}

// pidCheckRuntime is a runtime that, at each session start, notes whether a session process it or a sibling started
// still runs, or the Ticket's run file still names one.
type pidCheckRuntime struct {
	rt.Runtime
	stateDir string
	mu       *sync.Mutex
	pids     *[]int
	overlaps *[]string
}

func (p pidCheckRuntime) Start(ctx context.Context, o rt.StartOptions) (rt.Session, error) {
	p.mu.Lock()
	for _, pid := range *p.pids {
		if syscall.Kill(pid, 0) == nil {
			*p.overlaps = append(*p.overlaps, fmt.Sprintf("%s starts while pid %d runs", p.Name(), pid))
		}
	}
	if _, err := os.Stat(runFile(p.stateDir, o.TicketID)); err == nil {
		*p.overlaps = append(*p.overlaps, p.Name()+" starts while the run file names a process")
	}
	p.mu.Unlock()
	sess, err := p.Runtime.Start(ctx, o)
	if err == nil {
		p.mu.Lock()
		*p.pids = append(*p.pids, sess.Pid())
		p.mu.Unlock()
	}
	return sess, err
}

// Two sessions never work in one worktree: the limited session's process has exited, and its run file is gone, before
// the next one spawns.
func TestNoTwoSessionsInOneWorktree(t *testing.T) {
	mu, pids, overlaps := &sync.Mutex{}, &[]int{}, &[]string{}
	h := newCodexHarness(t, codexTurn(), func(c *Config) {
		for name, r := range c.Runtimes {
			c.Runtimes[name] = pidCheckRuntime{Runtime: r, stateDir: c.StateDir, mu: mu, pids: pids, overlaps: overlaps}
		}
	})
	t.Setenv("ATHENA_FAKE_FIXTURE", claudeLimitFixture(t, "{now+3600s}"))
	tk, err := h.eng.SubmitOwnerTask("Fix the parser", core.TaskOptions{Dir: newRepo(t), Runtime: core.RuntimeAuto})
	if err != nil {
		t.Fatal(err)
	}
	h.limitedAssignment(tk.ID)
	h.eventually("codex gets its first message", func() bool { return h.codexTurnStart() != "" })
	mu.Lock()
	defer mu.Unlock()
	if len(*pids) != 2 || len(*overlaps) != 0 {
		t.Fatalf("pids %v, overlaps %v", *pids, *overlaps)
	}
}

// Both runtimes limited: the Ticket stays working/queued, launches nothing until the earliest reset (Claude Code's
// here), then launches once, on that runtime, resuming its session.
func TestBothLimitedWaitsForEarliestReset(t *testing.T) {
	h := newCodexHarness(t, codexTurn(), nil)
	fixture := claudeLimitFixture(t, "{now+4s}")
	t.Setenv("ATHENA_FAKE_FIXTURE", fixture)
	codexReset := time.Now().Add(20 * time.Second)
	h.n.Quota().Hit(core.RuntimeCodex, rt.LimitHit{Runtime: core.RuntimeCodex, ResetsAt: &codexReset})
	tk, err := h.eng.SubmitOwnerTask("Fix the parser", core.TaskOptions{Runtime: core.RuntimeAuto})
	if err != nil {
		t.Fatal(err)
	}
	cur := h.limitedAssignment(tk.ID)
	reset := cur.Assignments[0].Limit.ResetsAt
	if reset == nil || !reset.After(time.Now()) {
		t.Fatalf("limit reset = %v, want one ahead", reset)
	}
	// The session after the reset ends its turn well.
	if err := os.WriteFile(fixture, must(os.ReadFile(claudeFixture(t, "simple-turn.jsonl"))), 0o600); err != nil {
		t.Fatal(err)
	}
	launched := func() bool { return len(h.invocations()) > 1 || h.rpcHas(`"method":"thread/start"`) }
	if _, st := h.ticket(tk.ID); st != (core.Stage{Stage: "working", Action: "queued"}) {
		t.Fatalf("stage while both are limited = %+v", st)
	}
	h.never("a launch before the earliest reset", time.Until(*reset)-200*time.Millisecond, launched)
	if _, st := h.ticket(tk.ID); st != (core.Stage{Stage: "working", Action: "queued"}) {
		t.Fatalf("stage before the reset = %+v", st)
	}
	h.eventually("one launch after the reset", func() bool { return len(h.invocations()) == 2 })
	h.never("a second launch", time.Second, func() bool { return len(h.invocations()) > 2 || h.rpcHas(`"method":"thread/start"`) })
	if id, _ := argValue(h.invocations()[1].Args, "--resume"); id != limitSessionID {
		t.Fatalf("the launch after the reset resumes %q, want the limited session", id)
	}
	if _, st := h.ticket(tk.ID); st.Stage != "working" {
		t.Fatalf("stage after the reset = %+v", st)
	}
}

// A limit that says no reset, or one that has already passed, backs off (15 minutes first) rather than wait on a time
// that means nothing: the Ticket stays working/queued, its limit shows no reset, and nothing launches meanwhile.
func TestUnknownResetBacksOff(t *testing.T) {
	for name, reset := range map[string]string{"no reset": "", "a reset that passed": "1600000000"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			t.Setenv("ATHENA_FAKE_FIXTURE", claudeLimitFixture(t, reset))
			tk := h.submit("Fix the parser", "")
			cur := h.limitedAssignment(tk.ID)
			if l := cur.Assignments[0].Limit; l == nil || l.ResetsAt != nil {
				t.Fatalf("limit = %+v, want no reset", l)
			}
			if next := cur.Assignments[1]; next.Runtime != core.RuntimeClaude {
				t.Fatalf("the ticket pinned to claude moved: %+v", next)
			}
			q := h.n.Quota().State(core.RuntimeClaude)
			if q.LimitedUntil == nil || q.LimitedUntil.Before(time.Now().Add(14*time.Minute)) || q.LimitedUntil.After(time.Now().Add(16*time.Minute)) {
				t.Fatalf("limited until %v, want the 15 minute backoff", q.LimitedUntil)
			}
			h.never("a launch during the backoff", 1500*time.Millisecond, func() bool { return len(h.invocations()) > 1 })
			if _, st := h.ticket(tk.ID); st != (core.Stage{Stage: "working", Action: "queued"}) {
				t.Fatalf("stage = %+v", st)
			}
		})
	}
}

// limitAfterReport runs a Claude Code turn in which the worker files req (its Ticket and Assignment filled in) before
// the turn stops at its usage limit, and returns the Ticket once the limit is in the quota. before runs first.
func limitAfterReport(t *testing.T, before func(h *harness, tk core.Ticket), req ReportRequest) (*harness, core.Ticket) {
	t.Helper()
	h := newHarness(t, nil)
	hold := filepath.Join(t.TempDir(), "hold")
	t.Setenv("ATHENA_FAKE_HOLD_RESULT", hold)
	t.Setenv("ATHENA_FAKE_FIXTURE", claudeLimitFixture(t, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)))
	tk := h.submit("Fix the parser", "")
	tk, a := h.running(tk.ID)
	if before != nil {
		before(h, tk)
	}
	req.Op, req.Ticket, req.Assignment = OpReport, tk.ID, a.ID
	var resp Response
	if err := callNode(h.cfg.SocketPath, req, &resp); err != nil || !resp.OK {
		t.Fatalf("report: %v %+v", err, resp)
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.eventually("the limit reaches the quota", func() bool { return h.n.Quota().State(core.RuntimeClaude).Limited(time.Now()) })
	return h, tk
}

// A limit on the turn in which the worker reported blocked, with a question, is an ordinary turn end: the Ticket still
// waits for the person's answer, and no new session runs past it.
func TestLimitAfterBlockedReportKeepsThePersonStage(t *testing.T) {
	h, tk := limitAfterReport(t, nil, ReportRequest{Status: string(core.ReportBlocked), Summary: "needs a key", Question: "Which API key?"})
	h.never("a new assignment or session", 1500*time.Millisecond, func() bool {
		cur, _ := h.ticket(tk.ID)
		return len(cur.Assignments) > 1 || len(h.invocations()) > 1
	})
	cur, st := h.ticket(tk.ID)
	a := cur.Assignments[0]
	if a.State != core.AssignmentRunning || a.EndedReason != "" || a.Report == nil || a.Report.State != core.ReportBlocked {
		t.Fatalf("assignment = %+v", a)
	}
	if cur.Attention == nil || cur.Attention.Question != "Which API key?" || !cur.WaitsForPerson() || st != (core.Stage{Stage: "person", Action: "answer"}) {
		t.Fatalf("attention %+v, waits %v, stage %+v", cur.Attention, cur.WaitsForPerson(), st)
	}
}

// A limit on the turn in which the worker reported ready on a Ticket that waits for its pull request is the end of a
// hand-off: the session parks as usual, the ready Report stands, and nothing starts again.
func TestLimitAfterReadyReportParks(t *testing.T) {
	h, tk := limitAfterReport(t, func(h *harness, tk core.Ticket) {
		if err := h.eng.BindThread(tk.ID, "github:o/r#1", true); err != nil {
			t.Fatal(err)
		}
	}, ReportRequest{Status: string(core.ReportReady), Summary: "PR is up"})
	h.eventually("the session parks", func() bool { return h.n.liveCount() == 0 })
	h.never("a new assignment or session", 1500*time.Millisecond, func() bool {
		cur, _ := h.ticket(tk.ID)
		return len(cur.Assignments) > 1 || len(h.invocations()) > 1
	})
	cur, st := h.ticket(tk.ID)
	a := cur.Assignments[0]
	if cur.Ended != nil || a.State != core.AssignmentRunning || a.Report == nil || a.Report.State != core.ReportReady || !cur.HandedOff() {
		t.Fatalf("ended %+v, assignment %+v", cur.Ended, a)
	}
	if st != (core.Stage{Stage: "person", Action: "review"}) || cur.WaitsForPerson() {
		t.Fatalf("stage %+v, waits %v", st, cur.WaitsForPerson())
	}
}
