package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/server"
)

const waitTimeout = 10 * time.Second

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
	fakeDir  string
)

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

func newStateDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "ath")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// athena runs one CLI command and returns its exit code, stdout and stderr.
func athena(ctx context.Context, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(ctx, args[0], args[1:], &out, &errb)
	return code, out.String(), errb.String()
}

// startServer runs `athena server` with fakeclaude replaying a turn that never ends.
func startServer(t *testing.T) string {
	t.Helper()
	dir := newStateDir(t)
	fixture, err := filepath.Abs(filepath.Join("..", "runtime", "claude", "testdata", "claude", "simple-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	init, _, _ := strings.Cut(string(lines), "\n")
	open := filepath.Join(t.TempDir(), "open-turn.jsonl")
	if err := os.WriteFile(open, []byte(init+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATHENA_FAKE_FIXTURE", open)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var serverErr bytes.Buffer
	go func() {
		var out bytes.Buffer
		done <- Run(ctx, "server", []string{"--state-dir", dir, "--claude", fakeClaude(t), "--codex", noCodex, "--slots", "2",
			"--model", "haiku", "--blocked", "rm,git push", "--approval-timeout", "5s"}, &out, &lockedWriter{w: &serverErr})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("athena server exit code = %d", code)
			}
		case <-time.After(waitTimeout):
			t.Error("athena server did not stop")
		}
	})
	eventually(t, "server answers", func() bool {
		code, _, _ := athena(context.Background(), "approvals", "--state-dir", dir)
		return code == 0
	})
	return dir
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// detail reads a Ticket through the control socket.
func detail(t *testing.T, dir, id string) server.TicketDetail {
	t.Helper()
	var d server.TicketDetail
	if err := call(dir, server.Request{Op: server.OpTicket, ID: id}, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func runningAssignment(t *testing.T, dir, id string) string {
	t.Helper()
	var aid string
	eventually(t, id+" running", func() bool {
		d := detail(t, dir, id)
		a := d.Assignments[len(d.Assignments)-1]
		aid = a.ID
		return a.State == "running" && a.SessionID != "" && d.Undelivered == 0
	})
	return aid
}

func hook(t *testing.T, dir, ticket, assignment, command string) <-chan node.HookResponse {
	t.Helper()
	got := make(chan node.HookResponse, 1)
	raw, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": command}})
	go func() {
		conn, err := net.Dial("unix", filepath.Join(dir, "node.sock"))
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = json.NewEncoder(conn).Encode(node.HookRequest{Op: node.OpHook, Event: node.EventPreToolUse, Ticket: ticket, Assignment: assignment, Input: raw})
		var r node.HookResponse
		_ = json.NewDecoder(conn).Decode(&r)
		got <- r
	}()
	return got
}

// --- tests ---

func TestCLINoServer(t *testing.T) {
	dir := newStateDir(t)
	want := "athena server is not running (no " + filepath.Join(dir, "athena.sock") + ")\n"
	for _, args := range [][]string{
		{"status"}, {"task", "do it"}, {"logs", "T-0001"}, {"reply", "T-0001", "hi"}, {"stop", "T-0001"},
		{"retry", "T-0001"}, {"approvals"}, {"approve", "abc"}, {"deny", "abc"},
		{"policy", "show"}, {"triggers"}, {"threads"}, {"decisions"}, {"reevaluate", "x"}, {"dismiss", "x"},
		{"unsubscribe", "github:o/r#1"},
	} {
		code, out, errOut := athena(context.Background(), append(args, "--state-dir", dir)...)
		if code != 1 || errOut != want || out != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want 1 and %q", args, code, out, errOut, want)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	dir := newStateDir(t)
	for _, args := range [][]string{
		{"task"}, {"logs"}, {"reply", "T-1"}, {"stop"}, {"retry"}, {"approve"}, {"deny"}, {"status", "a", "b"},
		{"status", "--bogus"}, {"github"}, {"github", "nope"}, {"policy"}, {"policy", "set"}, {"policy", "show", "x"},
		{"reevaluate"}, {"dismiss"}, {"unsubscribe"}, {"threads", "x"}, {"task", "x", "--dir", "/tmp", "--repo", "o/r"},
	} {
		if code, _, errOut := athena(context.Background(), append(args, "--state-dir", dir)...); code != 2 || errOut == "" {
			t.Errorf("%v: exit %d, stderr %q; want 2 with usage", args, code, errOut)
		}
	}
}

func TestCLICommands(t *testing.T) {
	dir := startServer(t)
	ctx := context.Background()
	code, out, errOut := athena(ctx, "task", "Fix the login page\nmore detail", "--state-dir", dir, "--model", "opus")
	if code != 0 || out != "T-0001\n" {
		t.Fatalf("task: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	runningAssignment(t, dir, "T-0001")

	code, out, _ = athena(ctx, "status", "--state-dir", dir)
	health, table, _ := strings.Cut(out, "\n\n")
	if want := "sources:   none (owner tasks only; see athena github setup)\neffects:   ok\nrouter:    off"; health != want {
		t.Fatalf("status health = %q, want %q", health, want)
	}
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if code != 0 || len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "ID STAGE AGE TITLE" {
		t.Fatalf("status: exit %d\n%s", code, out)
	}
	if f := strings.Fields(lines[1]); len(f) < 4 || f[0] != "T-0001" || f[1] != "working" || !strings.HasSuffix(f[2], "s") || !strings.Contains(lines[1], "Fix the login page") {
		t.Fatalf("status row = %q", lines[1])
	}

	code, out, _ = athena(ctx, "status", "T-0001", "--state-dir", dir)
	for _, want := range []string{"Fix the login page", "stage:", "working", "dir:", "fix-the-login-page-t-0001", "claude", "opus", "running"} {
		if !strings.Contains(out, want) {
			t.Errorf("status T-0001 lacks %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Fatalf("status T-0001 exit %d", code)
	}

	if code, out, errOut := athena(ctx, "logs", "T-0001", "--state-dir", dir); code != 0 || !strings.Contains(out, "  init  ") || strings.Contains(out, "{") {
		t.Fatalf("logs: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, errOut := athena(ctx, "reply", "T-0001", "please", "hurry", "--state-dir", dir); code != 0 {
		t.Fatalf("reply: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := athena(ctx, "retry", "T-0001", "--state-dir", dir); code != 1 || errOut != "ticket T-0001 is not waiting for a retry\n" {
		t.Fatalf("retry: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := athena(ctx, "status", "T-0042", "--state-dir", dir); code != 1 || errOut != "ticket T-0042 not found\n" {
		t.Fatalf("status unknown: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := athena(ctx, "stop", "T-0001", "--reason", "changed my mind", "--state-dir", dir); code != 0 {
		t.Fatalf("stop: exit %d, stderr %q", code, errOut)
	}
	code, out, _ = athena(ctx, "status", "--state-dir", dir)
	if code != 0 || strings.Contains(out, "T-0001") {
		t.Fatalf("status after stop:\n%s", out)
	}
	code, out, _ = athena(ctx, "status", "--all", "--state-dir", dir)
	if code != 0 || !strings.Contains(out, "ended:stopped") {
		t.Fatalf("status --all after stop:\n%s", out)
	}
	if code, _, errOut := athena(ctx, "reply", "T-0001", "again", "--state-dir", dir); code != 1 || errOut != "ticket T-0001 was stopped\n" {
		t.Fatalf("reply to stopped: exit %d, stderr %q", code, errOut)
	}
}

func TestTaskResolvesRelativeDirAgainstClient(t *testing.T) {
	dir := startServer(t)
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	code, out, errOut := athena(context.Background(), "task", "Work in proj", "--dir", "proj", "--state-dir", dir)
	if code != 0 {
		t.Fatalf("task: exit %d, stderr %q", code, errOut)
	}
	d := detail(t, dir, strings.TrimSpace(out))
	if want := filepath.Join(cwd, "proj"); d.SourceDir != want {
		t.Fatalf("source dir = %q, want %q (resolved against the client's directory)", d.SourceDir, want)
	}
}

func TestApproveFromCLI(t *testing.T) {
	dir := startServer(t)
	ctx := context.Background()
	if code, _, errOut := athena(ctx, "task", "Deploy it", "--state-dir", dir); code != 0 {
		t.Fatalf("task: %d %q", code, errOut)
	}
	aid := runningAssignment(t, dir, "T-0001")

	approvalID := func() string {
		var id string
		eventually(t, "approval listed", func() bool {
			code, out, _ := athena(ctx, "approvals", "--state-dir", dir)
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if code != 0 || len(lines) != 2 || !strings.Contains(lines[1], "T-0001") || !strings.Contains(lines[1], "make deploy") {
				return false
			}
			id = strings.Fields(lines[1])[0]
			return true
		})
		return id
	}

	got := hook(t, dir, "T-0001", aid, "make deploy")
	id := approvalID()
	if code, out, _ := athena(ctx, "status", "--state-dir", dir); code != 0 || !strings.Contains(out, "person/approval") {
		t.Fatalf("status during approval:\n%s", out)
	}
	if code, out, _ := athena(ctx, "status", "T-0001", "--state-dir", dir); code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "make deploy") {
		t.Fatalf("status detail lacks the pending approval:\n%s", out)
	}
	if code, _, errOut := athena(ctx, "approve", id, "--note", "go ahead", "--state-dir", dir); code != 0 {
		t.Fatalf("approve: %d %q", code, errOut)
	}
	select {
	case r := <-got:
		if r.Verdict != node.VerdictAllow {
			t.Fatalf("hook = %+v, want allow", r)
		}
	case <-time.After(waitTimeout):
		t.Fatal("hook never answered")
	}
	if code, _, errOut := athena(ctx, "approve", id, "--state-dir", dir); code != 1 || errOut != "approval "+id+" is already approved\n" {
		t.Fatalf("second approve: %d %q", code, errOut)
	}

	got = hook(t, dir, "T-0001", aid, "make deploy")
	id = approvalID()
	if code, _, errOut := athena(ctx, "deny", id, "--note", "not today", "--state-dir", dir); code != 0 {
		t.Fatalf("deny: %d %q", code, errOut)
	}
	select {
	case r := <-got:
		if r.Verdict != node.VerdictDeny || !strings.Contains(r.Reason, "not today") {
			t.Fatalf("hook = %+v, want deny with the note", r)
		}
	case <-time.After(waitTimeout):
		t.Fatal("hook never answered")
	}
	if code, out, _ := athena(ctx, "approvals", "--state-dir", dir); code != 0 || strings.TrimSpace(out) != "no pending approvals" {
		t.Fatalf("approvals after answers: %d %q", code, out)
	}
}

func TestFormatAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		3 * time.Second: "3s", 90 * time.Second: "1m", 3 * time.Hour: "3h", 50 * time.Hour: "2d", -time.Second: "0s",
	} {
		if got := formatAge(d); got != want {
			t.Errorf("formatAge(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestMetricsCommand(t *testing.T) {
	dir := startServer(t)
	ctx := context.Background()
	code, out, errOut := athena(ctx, "task", "--state-dir", dir, "measure me")
	if code != 0 {
		t.Fatalf("task: %d %s", code, errOut)
	}
	id := strings.TrimSpace(out)
	aid := runningAssignment(t, dir, id)
	if r := <-hook(t, dir, id, aid, "echo hi"); r.Verdict != node.VerdictAllow {
		t.Fatalf("hook = %+v", r)
	}

	code, out, errOut = athena(ctx, "metrics", "--state-dir", dir, "--window", "1h")
	if code != 0 {
		t.Fatalf("metrics: %d %s", code, errOut)
	}
	for _, name := range []string{"engine_command_seconds", "hook_decision_seconds"} {
		found := false
		for _, l := range strings.Split(out, "\n") {
			f := strings.Fields(l)
			if len(f) >= 3 && f[0] == name && f[2] != "0" {
				found = true
			}
		}
		if !found {
			t.Errorf("no %s row with a non-zero count in:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "command=submit") || !strings.Contains(out, "verdict=allow") {
		t.Errorf("labels missing in:\n%s", out)
	}
	if code, _, errOut := athena(ctx, "metrics", "--state-dir", dir, "--window", "7d"); code != 1 || !strings.Contains(errOut, "window") {
		t.Errorf("bad window: %d %q", code, errOut)
	}
	// status prints the stats.
	_, o, _ := athena(ctx, "status", "--state-dir", dir, id)
	if !strings.Contains(o, "stats:") || !strings.Contains(o, "queue wait") {
		t.Errorf("status lacks stats:\n%s", o)
	}
}

// TestServerHelperProcess is the body of the subprocess TestServerStopsOnSIGHUP runs; it does nothing in a normal run.
func TestServerHelperProcess(t *testing.T) {
	dir := os.Getenv("ATHENA_TEST_SERVER_DIR")
	if dir == "" {
		t.Skip("helper process")
	}
	os.Exit(Run(context.Background(), "server", []string{"--state-dir", dir, "--claude", os.Getenv("ATHENA_TEST_CLAUDE"), "--codex", noCodex}, io.Discard, os.Stderr))
}

// noCodex is the codex the CLI's test servers run: none, so no test reaches the owner's codex.
const noCodex = "/nonexistent/athena-test-codex"

// A closed terminal sends SIGHUP; the server stops gracefully on it, as on SIGINT and SIGTERM, instead of dying with
// its sessions still running.
func TestServerStopsOnSIGHUP(t *testing.T) {
	dir := newStateDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestServerHelperProcess$")
	cmd.Env = append(os.Environ(), "ATHENA_TEST_SERVER_DIR="+dir, "ATHENA_TEST_CLAUDE="+fakeClaude(t))
	var stderr bytes.Buffer
	cmd.Stderr = &lockedWriter{w: &stderr}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	eventually(t, "server answers", func() bool {
		code, _, _ := athena(context.Background(), "approvals", "--state-dir", dir)
		return code == 0
	})
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("server exited with %v after SIGHUP, want a clean exit 0\n%s", err, stderr.String())
		}
	case <-time.After(waitTimeout):
		t.Fatalf("server still running after SIGHUP\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "athena server stopping") {
		t.Fatalf("log = %q, want it to say the server stops", stderr.String())
	}
}

// `athena task --runtime` names the runtime the task runs on; left out, the server's default (auto) does, and the
// status shows the runtime each Assignment ran on.
func TestTaskRuntimeFlag(t *testing.T) {
	dir := startServer(t)
	ctx := context.Background()
	code, out, errOut := athena(ctx, "task", "Pinned to claude", "--runtime", "claude", "--state-dir", dir)
	if code != 0 {
		t.Fatalf("task --runtime claude: exit %d, stderr %q", code, errOut)
	}
	if d := detail(t, dir, strings.TrimSpace(out)); d.Runtime != "claude" {
		t.Fatalf("runtime = %q, want claude", d.Runtime)
	}
	code, out, errOut = athena(ctx, "task", "Pick one", "--state-dir", dir)
	if code != 0 {
		t.Fatalf("task: exit %d, stderr %q", code, errOut)
	}
	id := strings.TrimSpace(out)
	if d := detail(t, dir, id); d.Runtime != "auto" {
		t.Fatalf("runtime = %q, want the server's default auto", d.Runtime)
	}
	runningAssignment(t, dir, id)
	if _, out, _ := athena(ctx, "status", id, "--state-dir", dir); !strings.Contains(out, "runtime:   auto") || !strings.Contains(out, "running  on claude") {
		t.Fatalf("status %s:\n%s", id, out)
	}
	if code, _, errOut := athena(ctx, "task", "Bad", "--runtime", "gemini", "--state-dir", dir); code != 1 || !strings.Contains(errOut, `"gemini"`) {
		t.Fatalf("task --runtime gemini: exit %d, stderr %q", code, errOut)
	}
}

// `athena task --review` pairs the task with a review of its ready reports; --review=false pairs it with none, and
// without the flag the Policy's pairing default (off) holds.
func TestTaskReviewFlag(t *testing.T) {
	dir := startServer(t)
	ctx := context.Background()
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"--review"}, "review"}, {[]string{"--review=false"}, ""}, {nil, ""}} {
		args := append([]string{"task", "Reviewed task"}, tc.args...)
		code, out, errOut := athena(ctx, append(args, "--state-dir", dir)...)
		if code != 0 {
			t.Fatalf("task %v: exit %d, stderr %q", tc.args, code, errOut)
		}
		if d := detail(t, dir, strings.TrimSpace(out)); d.Pairing != tc.want {
			t.Errorf("task %v: pairing %q, want %q", tc.args, d.Pairing, tc.want)
		}
	}
}
