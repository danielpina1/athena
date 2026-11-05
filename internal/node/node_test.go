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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/runtime/claude"
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
	tk := h.submit("Task", "")
	_, a := h.running(tk.ID)
	s := h.n.lookup(tk.ID, a.ID)
	if s == nil {
		t.Fatal("no live session")
	}
	r := &runner{n: h.n, s: s, eng: h.eng, log: s.log, sent: map[string]bool{}}
	r.onEvent(claude.ToolUse{ID: "w1", Name: "Write"})
	r.onEvent(claude.ToolUse{ID: "r1", Name: "mcp__athena__report"})
	r.onEvent(claude.ToolResult{ToolUseID: "w1", IsError: true})
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
	r.onEvent(claude.AssistantText{Text: "The write was denied."})
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
	r.onEvent(claude.ToolUse{ID: "b1", Name: "Bash"})
	r.onEvent(claude.ToolUse{ID: "r1", Name: "mcp__athena__report"})
	if got := h.report(tk.ID, a.ID, core.ReportReady, "built"); got.OK {
		t.Fatalf("ready report before the failed result was seen = %+v, want a refusal", got)
	}
	r.onEvent(claude.ToolResult{ToolUseID: "b1", IsError: true})
	r.onEvent(claude.AssistantText{Text: "rm is blocked."})
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

// A turn that ends in an error (here a real `error_max_turns` result; also an expired login or a rate limit) leaves the
// CLI waiting with stdin open: the Assignment reports failed with the subtype and the reason instead of staying working.
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
	r.onEvent(claude.Result{Subtype: "success"})
	r.onEvent(claude.Result{Subtype: "success"})
	if len(sent) != 0 {
		t.Fatalf("sent %q after a reported turn", sent)
	}
	// A new Input without a Report: the turn's end is nudged once.
	r.inputSent()
	r.onEvent(claude.Result{Subtype: "success"})
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
func (b blockingEngine) AssignmentStarted(string, string, string, string) error {
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
