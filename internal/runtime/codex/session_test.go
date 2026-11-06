package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

// rig is a Runtime on fakecodex with Athena's home written (as the node's Check leaves it), a working directory and a
// transcript, and the logs fakecodex keeps: the client's lines (rpcLog) and its own arguments (argsLog).
type rig struct {
	r               *Runtime
	dir             string
	rpcLog, argsLog string
	transcript      string
	tr              *rt.Transcript
}

// newRig sets env (the ATHENA_FAKE_CODEX_* knobs) for fakecodex, writes Athena's CODEX_HOME and opens a transcript.
func newRig(t *testing.T, env ...string) *rig {
	t.Helper()
	r, rpcLog := newRuntime(t, env...)
	if err := WriteHome(r.home()); err != nil {
		t.Fatal(err)
	}
	g := &rig{r: r, dir: t.TempDir(), rpcLog: rpcLog, argsLog: filepath.Join(t.TempDir(), "args.json")}
	t.Setenv("ATHENA_FAKE_CODEX_ARGS_LOG", g.argsLog)
	g.transcript = filepath.Join(t.TempDir(), "transcript.jsonl")
	tr, err := rt.OpenTranscript(g.transcript, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.tr = tr
	t.Cleanup(func() { tr.Close() })
	return g
}

// options fills what every session needs into o.
func (g *rig) options(o rt.StartOptions) rt.StartOptions {
	if o.Dir == "" {
		o.Dir = g.dir
	}
	o.Transcript = g.tr
	if o.TicketID == "" {
		o.TicketID, o.AssignmentID = "T-1", "a-1"
	}
	o.Env = append(o.Env, "ATHENA_TICKET=t1")
	return o
}

// start starts a session and closes it when the test ends.
func (g *rig) start(t *testing.T, o rt.StartOptions) rt.Session {
	t.Helper()
	s, err := g.r.Start(context.Background(), g.options(o))
	if err != nil {
		t.Fatalf("Start: %v (stderr: %s)", err, stderrOf(s))
	}
	t.Cleanup(func() {
		s.Close()
		for range s.Events() {
		}
		_ = s.Wait()
	})
	return s
}

func stderrOf(s rt.Session) string {
	if s == nil {
		return ""
	}
	return s.Stderr()
}

// requests returns the client's requests in the RPC log with the given method, in order, as decoded objects.
func (g *rig) requests(t *testing.T, method string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range g.lines(t) {
		if l["method"] == method && l["id"] != nil {
			out = append(out, l)
		}
	}
	return out
}

// answers returns the client's answers to fakecodex's server requests, by request id.
func (g *rig) answers(t *testing.T) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, l := range g.lines(t) {
		if l["method"] == nil && l["id"] != nil {
			b, _ := json.Marshal(l["id"])
			out[string(b)] = l
		}
	}
	return out
}

func (g *rig) lines(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(g.rpcLog)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func params(req map[string]any) map[string]any {
	p, _ := req["params"].(map[string]any)
	return p
}

// next returns the session's events until stop says so, failing after a few seconds.
func next(t *testing.T, s rt.Session, stop func(rt.Event) bool) []rt.Event {
	t.Helper()
	var evs []rt.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatalf("events closed before the expected one; got %#v", evs)
			}
			evs = append(evs, e)
			if stop(e) {
				return evs
			}
		case <-timeout:
			t.Fatalf("timed out; got %#v", evs)
		}
	}
}

func echoOf(text string) func(rt.Event) bool {
	return func(e rt.Event) bool { u, ok := e.(rt.UserEcho); return ok && u.Text == text }
}

func turnEnd(evs []rt.Event) rt.TurnEnd {
	return evs[len(evs)-1].(rt.TurnEnd)
}

// writeFixture writes a fixture of fakecodex lines into a temporary file and returns its path.
func writeFixture(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	turnStartedLine   = `{"method":"turn/started","params":{"threadId":"thr_fixture_0001","turn":{"id":"turn_fixture_0001","items":[],"status":"inProgress"}}}`
	turnCompletedLine = `{"method":"turn/completed","params":{"threadId":"thr_fixture_0001","turn":{"id":"turn_fixture_0001","items":[],"status":"completed","durationMs":10}}}`
)

func TestStartNewThreadSendsPromptAndSandbox(t *testing.T) {
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"))
	// Neither the server's CODEX_HOME nor one in the session's Env reaches Codex: only Athena's.
	t.Setenv("CODEX_HOME", "/nonexistent/.codex")
	roots := []string{"/repo/.git/worktrees/wt", "/repo/.git/objects"}
	s := g.start(t, rt.StartOptions{
		Model: "gpt-5-codex", SystemPrompt: "You are an Athena worker.", WritableRoots: roots,
		Env: []string{"CODEX_HOME=/elsewhere"},
	})
	evs := next(t, s, func(e rt.Event) bool { _, ok := e.(rt.QuotaUpdate); return ok })
	if init, ok := evs[0].(rt.Init); !ok || init != (rt.Init{SessionID: "thr_fixture_t1", Model: "gpt-5-codex"}) {
		t.Fatalf("first event = %#v, want the thread's Init", evs[0])
	}
	if err := s.Send("Say done.", "u-1"); err != nil {
		t.Fatal(err)
	}
	turn := next(t, s, isTurnEnd)
	if !slices.ContainsFunc(turn, func(e rt.Event) bool { return e == rt.Event(rt.UserEcho{Text: "Say done.", UUID: "u-1"}) }) {
		t.Fatalf("no UserEcho for the message: %#v", turn)
	}
	for _, e := range append(evs[1:], turn...) {
		if _, ok := e.(rt.Init); ok {
			t.Fatalf("a second Init (thread/started repeats the thread): %#v", append(evs, turn...))
		}
	}
	evs = turn
	if end := turnEnd(evs); !end.OK || end.SessionID != "thr_fixture_t1" || end.Text != "Done." {
		t.Fatalf("TurnEnd = %+v", end)
	}

	var order []string
	for _, l := range g.lines(t) {
		if m, _ := l["method"].(string); m != "" && l["id"] != nil {
			order = append(order, m)
		}
	}
	if want := []string{"initialize", "hooks/list", "config/read", "thread/start", "account/rateLimits/read", "turn/start"}; !slices.Equal(order, want) {
		t.Fatalf("requests = %v, want %v", order, want)
	}
	init := params(g.requests(t, "initialize")[0])
	if ci := init["clientInfo"].(map[string]any); ci["name"] != "athena" || ci["version"] != "dev" {
		t.Errorf("clientInfo = %v", ci)
	}
	optOut := init["capabilities"].(map[string]any)["optOutNotificationMethods"]
	if want := []any{"item/agentMessage/delta", "item/commandExecution/outputDelta", "item/fileChange/outputDelta",
		"item/reasoning/textDelta", "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "item/plan/delta",
		"command/exec/outputDelta"}; !reflect.DeepEqual(optOut, want) {
		t.Errorf("optOutNotificationMethods = %v", optOut)
	}
	if got, want := params(g.requests(t, "thread/start")[0]), map[string]any{
		"cwd": g.dir, "model": "gpt-5-codex", "developerInstructions": "You are an Athena worker.",
		"approvalPolicy": "on-request", "approvalsReviewer": "user", "sandbox": "workspace-write", "serviceName": "athena",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("thread/start params = %v\nwant %v", got, want)
	}
	if got, want := params(g.requests(t, "turn/start")[0]), map[string]any{
		"threadId": "thr_fixture_t1", "input": []any{map[string]any{"type": "text", "text": "Say done."}},
		"clientUserMessageId": "u-1",
		"sandboxPolicy":       map[string]any{"type": "workspaceWrite", "writableRoots": []any{roots[0], roots[1]}},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("turn/start params = %v\nwant %v", got, want)
	}
	if p := params(g.requests(t, "config/read")[0]); p["cwd"] != g.dir || p["includeLayers"] != true {
		t.Errorf("config/read params = %v", p)
	}

	// The process: the app-server Athena drives, with Athena's home, and only the MCP server's -c overrides.
	args := g.args(t)
	if want := []string{"app-server", "--listen", "stdio://", "--strict-config",
		"-c", `mcp_servers.athena.args=["mcp","--ticket","T-1"]`,
		"-c", `mcp_servers.athena.env={ATHENA_SOCKET="` + g.r.Socket + `",ATHENA_ASSIGNMENT="a-1"}`}; !slices.Equal(args.Args, want) {
		t.Errorf("args = %q\nwant %q", args.Args, want)
	}
	if args.CodexHome != g.r.Home {
		t.Errorf("CODEX_HOME = %q, want Athena's %q", args.CodexHome, g.r.Home)
	}
	assertOnlyMCPOverrides(t, args.Args)
	// F-13: serviceName is a field of thread/start in the protocol Athena was verified against (fakecodex refuses any
	// field the protocol does not list, so the start above proves every field the session sends).
	if !slices.Contains(loadProtocol(t).RequestFields["thread/start"], "serviceName") {
		t.Error("protocol.json does not list serviceName for thread/start")
	}
}

// assertOnlyMCPOverrides fails unless every -c override sets Athena's MCP server, never hooks.* or features.*.
func assertOnlyMCPOverrides(t *testing.T, args []string) {
	t.Helper()
	for i, a := range args {
		if a != "-c" {
			continue
		}
		key, _, _ := strings.Cut(args[i+1], "=")
		if !strings.HasPrefix(key, "mcp_servers.athena.") || strings.HasPrefix(key, "hooks") || strings.HasPrefix(key, "features") {
			t.Errorf("override %q sets more than Athena's MCP server", args[i+1])
		}
	}
}

type argsRecord struct {
	Args      []string          `json:"args"`
	CodexHome string            `json:"codexHome"`
	Env       map[string]string `json:"env"`
}

func (g *rig) args(t *testing.T) argsRecord {
	t.Helper()
	b, err := os.ReadFile(g.argsLog)
	if err != nil {
		t.Fatal(err)
	}
	var a argsRecord
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStartReviewRoleIsReadOnly(t *testing.T) {
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"))
	s := g.start(t, rt.StartOptions{Role: "review", ReadOnly: true, WritableRoots: []string{"/ignored"}})
	if err := s.Send("Review it.", ""); err != nil {
		t.Fatal(err)
	}
	next(t, s, isTurnEnd)
	if want := `mcp_servers.athena.args=["mcp","--ticket","T-1","--role","review"]`; !slices.Contains(g.args(t).Args, want) {
		t.Errorf("args = %q, want %s", g.args(t).Args, want)
	}
	if p := params(g.requests(t, "thread/start")[0]); p["sandbox"] != "read-only" {
		t.Errorf("thread/start sandbox = %v", p["sandbox"])
	}
	p := params(g.requests(t, "turn/start")[0])
	if !reflect.DeepEqual(p["sandboxPolicy"], map[string]any{"type": "readOnly"}) {
		t.Errorf("turn/start sandboxPolicy = %v", p["sandboxPolicy"])
	}
	if _, ok := p["clientUserMessageId"]; ok {
		t.Errorf("an empty uuid is sent: %v", p)
	}
}

func TestSessionEnvAddsSandboxEnvAndAthenasHome(t *testing.T) {
	parent := []string{"PATH=/bin", "CODEX_HOME=/home/me/.codex", "OPENAI_API_KEY=k", "GITHUB_TOKEN=secret", "CODEX_X=1"}
	env := sessionEnv(parent, rt.StartOptions{Env: []string{"ATHENA_TICKET=t", "CODEX_HOME=/other"}, SandboxEnv: []string{"SSL_CERT_FILE=/ca.pem"}}, "/state/codex")
	want := []string{"PATH=/bin", "OPENAI_API_KEY=k", "CODEX_X=1", "ATHENA_TICKET=t", "SSL_CERT_FILE=/ca.pem", "CODEX_HOME=/state/codex"}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %q\nwant %q", env, want)
	}
}

func TestResumeOmitsInstructions(t *testing.T) {
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"))
	s := g.start(t, rt.StartOptions{ResumeID: "thr_fixture_old", Model: "gpt-5-codex", SystemPrompt: "not again"})
	evs := next(t, s, func(e rt.Event) bool { _, ok := e.(rt.Init); return ok })
	if init := evs[len(evs)-1].(rt.Init); init.SessionID != "thr_fixture_old" {
		t.Fatalf("Init = %+v, want the resumed thread", init)
	}
	if len(g.requests(t, "thread/start")) != 0 {
		t.Fatal("a resumed session started a new thread")
	}
	if got, want := params(g.requests(t, "thread/resume")[0]), map[string]any{
		"threadId": "thr_fixture_old", "cwd": g.dir, "model": "gpt-5-codex", "approvalPolicy": "on-request",
		"approvalsReviewer": "user", "sandbox": "workspace-write", "excludeTurns": true,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thread/resume params = %v\nwant %v", got, want)
	}
	if err := s.Send("Go on.", ""); err != nil {
		t.Fatal(err)
	}
	if end := turnEnd(next(t, s, isTurnEnd)); end.SessionID != "thr_fixture_old" {
		t.Fatalf("TurnEnd = %+v", end)
	}
}

// holdFile makes fakecodex hold each turn's turn/completed until it exists; release creates it.
func holdFile(t *testing.T) (env string, release func()) {
	p := filepath.Join(t.TempDir(), "release")
	return "ATHENA_FAKE_CODEX_HOLD_RESULT=" + p, func() {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (g *rig) methodsSent(t *testing.T, of ...string) []string {
	t.Helper()
	var out []string
	for _, l := range g.lines(t) {
		if m, _ := l["method"].(string); l["id"] != nil && slices.Contains(of, m) {
			out = append(out, m)
		}
	}
	return out
}

func TestSendStartsThenSteers(t *testing.T) {
	hold, release := holdFile(t)
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_LATER_FIXTURE="+fixturePath(t, "steer-turn"), hold)
	s := g.start(t, rt.StartOptions{})
	if err := s.Send("first", "u-1"); err != nil {
		t.Fatal(err)
	}
	next(t, s, echoOf("first"))
	if err := s.Send("second", "u-2"); err != nil {
		t.Fatal(err)
	}
	evs := next(t, s, echoOf("second"))
	if e := evs[len(evs)-1].(rt.UserEcho); e.UUID != "u-2" {
		t.Fatalf("steered echo = %+v", e)
	}
	release()
	next(t, s, isTurnEnd)
	// The turn is over: the next message starts a turn of its own.
	if err := s.Send("third", "u-3"); err != nil {
		t.Fatal(err)
	}
	next(t, s, isTurnEnd)
	if got := g.methodsSent(t, "turn/start", "turn/steer"); !slices.Equal(got, []string{"turn/start", "turn/steer", "turn/start"}) {
		t.Fatalf("requests = %v", got)
	}
	steer := params(g.requests(t, "turn/steer")[0])
	if steer["expectedTurnId"] != "turn_fixture_0001" || steer["clientUserMessageId"] != "u-2" || steer["threadId"] != "thr_fixture_t1" {
		t.Fatalf("turn/steer params = %v", steer)
	}
	if _, ok := steer["sandboxPolicy"]; ok {
		t.Fatal("turn/steer carries a sandboxPolicy, which it does not take")
	}
}

func TestSteerFallsBackToStart(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		hold, release := holdFile(t)
		g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_LATER_FIXTURE="+fixturePath(t, "steer-turn"),
			hold, "ATHENA_FAKE_CODEX_STEER_ERROR=mismatch")
		s := g.start(t, rt.StartOptions{})
		if err := s.Send("first", ""); err != nil {
			t.Fatal(err)
		}
		next(t, s, echoOf("first"))
		if err := s.Send("second", "u-2"); err != nil {
			t.Fatalf("Send after a refused steer: %v", err)
		}
		next(t, s, echoOf("second")) // Codex took the turn/start into the running turn
		release()
		next(t, s, isTurnEnd)
		if got := g.methodsSent(t, "turn/start", "turn/steer"); !slices.Equal(got, []string{"turn/start", "turn/steer", "turn/start"}) {
			t.Fatalf("requests = %v", got)
		}
		if p := params(g.requests(t, "turn/start")[1]); p["clientUserMessageId"] != "u-2" {
			t.Fatalf("fallback turn/start = %v", p)
		}
	})
	t.Run("not steerable", func(t *testing.T) {
		hold, release := holdFile(t)
		g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_LATER_FIXTURE="+fixturePath(t, "steer-turn"),
			hold, "ATHENA_FAKE_CODEX_STEER_ERROR=notSteerable")
		s := g.start(t, rt.StartOptions{})
		if err := s.Send("first", ""); err != nil {
			t.Fatal(err)
		}
		next(t, s, echoOf("first"))
		// The running turn takes no input at all: both the steer and its fallback are refused, and Send says so.
		err := s.Send("second", "")
		if err == nil || !strings.Contains(err.Error(), "cannot steer") {
			t.Fatalf("Send during a turn that cannot take input = %v", err)
		}
		release()
		next(t, s, isTurnEnd)
		if err := s.Send("third", ""); err != nil {
			t.Fatalf("Send after the turn: %v", err)
		}
		next(t, s, isTurnEnd)
		if got := g.methodsSent(t, "turn/start", "turn/steer"); !slices.Equal(got, []string{"turn/start", "turn/steer", "turn/start", "turn/start"}) {
			t.Fatalf("requests = %v", got)
		}
	})
}

func TestInterruptEndsTurnInterrupted(t *testing.T) {
	hold, release := holdFile(t)
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_LATER_FIXTURE="+fixturePath(t, "simple-turn"), hold)
	s := g.start(t, rt.StartOptions{})
	// No turn: nothing to interrupt, and nothing is sent.
	if err := s.InterruptTurn(); err != nil {
		t.Fatal(err)
	}
	if err := s.Send("go", ""); err != nil {
		t.Fatal(err)
	}
	next(t, s, echoOf("go"))
	if err := s.InterruptTurn(); err != nil {
		t.Fatal(err)
	}
	end := turnEnd(next(t, s, isTurnEnd))
	if end.OK || end.Subtype != "interrupted" {
		t.Fatalf("TurnEnd = %+v, want an interrupted turn", end)
	}
	reqs := g.requests(t, "turn/interrupt")
	if len(reqs) != 1 || params(reqs[0])["turnId"] != "turn_fixture_0001" || params(reqs[0])["threadId"] != "thr_fixture_t1" {
		t.Fatalf("turn/interrupt requests = %v", reqs)
	}
	// The process stays for the next message, which starts a new turn.
	release()
	if err := s.Send("again", ""); err != nil {
		t.Fatal(err)
	}
	if end := turnEnd(next(t, s, isTurnEnd)); !end.OK {
		t.Fatalf("next TurnEnd = %+v", end)
	}
}

// approvalsFixture is a turn that asks for a file change, more permissions and a network command.
func approvalsFixture(t *testing.T) string {
	return writeFixture(t, turnStartedLine,
		`{"method":"item/started","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","startedAtMs":1,"item":{"id":"item_0010","type":"fileChange","status":"inProgress","changes":[{"path":"/home/user/work/a.go","kind":{"type":"update","move_path":null},"diff":""}]}}}`,
		`{"fake":"request","method":"item/fileChange/requestApproval","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","itemId":"item_0010","startedAtMs":2,"reason":"edit a.go"}}`,
		`{"method":"item/completed","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","completedAtMs":3,"item":{"id":"item_0010","type":"fileChange","status":"completed","changes":[{"path":"/home/user/work/a.go","kind":{"type":"update","move_path":null},"diff":""}]}}}`,
		`{"fake":"request","method":"item/permissions/requestApproval","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","itemId":"item_0011","startedAtMs":4,"cwd":"/home/user/work","reason":"needs the cache","permissions":{"fileSystem":{"write":["/home/user/.cache/x"]},"network":{"enabled":true}}}}`,
		`{"fake":"request","method":"item/commandExecution/requestApproval","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","itemId":"item_0012","startedAtMs":5,"command":"curl https://example.com","cwd":"/home/user/work","networkApprovalContext":{"host":"example.com","protocol":"https"}}}`,
		turnCompletedLine)
}

func TestApprovalRequestUsesApprove(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(map[bool]string{true: "allow", false: "deny"}[allow], func(t *testing.T) {
			g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "approval-turn"), "ATHENA_FAKE_CODEX_LATER_FIXTURE="+approvalsFixture(t))
			var mu sync.Mutex
			var asked []rt.ApprovalRequest
			s := g.start(t, rt.StartOptions{Approve: func(_ context.Context, r rt.ApprovalRequest) rt.Verdict {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, r)
				return rt.Verdict{Allow: allow}
			}})
			for _, msg := range []string{"clean", "edit"} {
				if err := s.Send(msg, ""); err != nil {
					t.Fatal(err)
				}
				next(t, s, isTurnEnd)
			}
			mu.Lock()
			defer mu.Unlock()
			want := []rt.ApprovalRequest{
				{Kind: "command", Command: "/bin/zsh -lc 'rm -rf build'", Cwd: "/home/user/work", Reason: "Command is outside the sandbox's write roots", TurnID: "turn_fixture_0001"},
				{Kind: "file", Paths: []string{"/home/user/work/a.go"}, Reason: "edit a.go", TurnID: "turn_fixture_0002"},
				{Kind: "permissions", Cwd: "/home/user/work", Reason: "needs the cache (turns network access on)", Host: "*", Paths: []string{"/home/user/.cache/x"}, TurnID: "turn_fixture_0002"},
				{Kind: "network", Command: "curl https://example.com", Cwd: "/home/user/work", Host: "example.com", TurnID: "turn_fixture_0002"},
			}
			if !reflect.DeepEqual(asked, want) {
				t.Fatalf("Approve got %+v\nwant %+v", asked, want)
			}
			ans := g.answers(t)
			dec := map[bool]string{true: "accept", false: "decline"}[allow]
			for _, id := range []string{"1000", "1001", "1003"} {
				if r := ans[id]["result"]; !reflect.DeepEqual(r, map[string]any{"decision": dec}) {
					t.Errorf("answer %s = %v, want decision %s", id, ans[id], dec)
				}
			}
			grant := map[string]any{"permissions": map[string]any{}}
			if allow {
				grant = map[string]any{"scope": "turn", "permissions": map[string]any{
					"fileSystem": map[string]any{"write": []any{"/home/user/.cache/x"}}, "network": map[string]any{"enabled": true}}}
			}
			if r := ans["1002"]["result"]; !reflect.DeepEqual(r, grant) {
				t.Errorf("permissions answer = %v, want %v", ans["1002"], grant)
			}
		})
	}
	t.Run("no Approve", func(t *testing.T) {
		g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "approval-turn"))
		s := g.start(t, rt.StartOptions{})
		if err := s.Send("clean", ""); err != nil {
			t.Fatal(err)
		}
		next(t, s, isTurnEnd)
		if r := g.answers(t)["1000"]["result"]; !reflect.DeepEqual(r, map[string]any{"decision": "decline"}) {
			t.Fatalf("answer = %v, want decline", r)
		}
	})
}

func TestApprovalCancelledOnClose(t *testing.T) {
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "approval-turn"))
	asked := make(chan struct{})
	ended := make(chan error, 1)
	s, err := g.r.Start(context.Background(), g.options(rt.StartOptions{Approve: func(ctx context.Context, _ rt.ApprovalRequest) rt.Verdict {
		close(asked)
		<-ctx.Done()
		ended <- ctx.Err()
		return rt.Verdict{Allow: true} // too late: a cancelled approval is declined whatever Approve says
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("clean", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("Approve was not called")
	}
	start := time.Now()
	s.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Close took %v", d)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Approve's context ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel Approve's context")
	}
	for range s.Events() {
	}
	_ = s.Wait()
	if r := g.answers(t)["1000"]["result"]; !reflect.DeepEqual(r, map[string]any{"decision": "decline"}) {
		t.Fatalf("answer = %v, want decline", g.answers(t)["1000"])
	}
	if s.Killed() {
		t.Error("the app-server had to be killed: it should exit when its input closes")
	}
	if err := s.Send("again", ""); !errors.Is(err, rt.ErrExited) {
		t.Fatalf("Send after Close = %v, want rt.ErrExited", err)
	}
}

// Review Focus 4: a server request Athena does not model is answered at once, in its method's own shape, so no turn
// waits on it.
func TestUnhandledServerRequestsAreAnswered(t *testing.T) {
	req := func(method, params string) string {
		return `{"fake":"request","method":"` + method + `","params":` + params + `}`
	}
	const ids = `"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001"`
	fixture := writeFixture(t, turnStartedLine,
		req("item/tool/requestUserInput", `{`+ids+`,"itemId":"item_0020","isBlocking":true,"questions":[{"id":"q1","header":"Pick","question":"Which?"}]}`),
		req("mcpServer/elicitation/request", `{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","serverName":"athena","mode":"form","message":"Your name?","requestedSchema":{"type":"object","properties":{}}}`),
		req("item/tool/call", `{`+ids+`,"callId":"call_1","tool":"lookup","arguments":{}}`),
		req("account/chatgptAuthTokens/refresh", `{"reason":"unauthorized","previousAccountId":"acct_fixture"}`),
		req("attestation/generate", `{}`),
		req("execCommandApproval", `{"conversationId":"thr_fixture_0001","callId":"call_2","command":["ls"],"cwd":"/home/user/work","parsedCmd":[]}`),
		req("applyPatchApproval", `{"conversationId":"thr_fixture_0001","callId":"call_3","fileChanges":{}}`),
		req("item/future/request", `{`+ids+`}`),
		turnCompletedLine)
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixture)
	s := g.start(t, rt.StartOptions{Approve: func(context.Context, rt.ApprovalRequest) rt.Verdict {
		t.Error("Approve was asked about a request that is not an approval")
		return rt.Verdict{}
	}})
	start := time.Now()
	if err := s.Send("go", ""); err != nil {
		t.Fatal(err)
	}
	next(t, s, isTurnEnd)
	// fakecodex sends each request only once the previous one is answered: the whole turn is the sum of the answers.
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("eight requests took %v to answer", d)
	}
	ans := g.answers(t)
	byMethod := map[string]map[string]any{
		"item/tool/requestUserInput": ans["1000"], "mcpServer/elicitation/request": ans["1001"], "item/tool/call": ans["1002"],
		"account/chatgptAuthTokens/refresh": ans["1003"], "attestation/generate": ans["1004"], "execCommandApproval": ans["1005"],
		"applyPatchApproval": ans["1006"], "item/future/request": ans["1007"],
	}
	wantResult := map[string]any{
		"mcpServer/elicitation/request": map[string]any{"action": "decline"},
		"item/tool/call":                map[string]any{"contentItems": []any{}, "success": false},
		"execCommandApproval":           map[string]any{"decision": "denied"},
		"applyPatchApproval":            map[string]any{"decision": "denied"},
	}
	wantCode := map[string]float64{
		"item/tool/requestUserInput": -32000, "account/chatgptAuthTokens/refresh": -32601, "attestation/generate": -32601,
		"item/future/request": -32601,
	}
	for m, a := range byMethod {
		if a == nil {
			t.Errorf("%s: no answer", m)
			continue
		}
		if want, ok := wantResult[m]; ok && !reflect.DeepEqual(a["result"], want) {
			t.Errorf("%s: answer %v, want result %v", m, a, want)
		}
		if code, ok := wantCode[m]; ok {
			if e, _ := a["error"].(map[string]any); e == nil || e["code"] != code {
				t.Errorf("%s: answer %v, want error %v", m, a, code)
			}
		}
	}
	// The answers agree with protocol.json's record of how each server request is answered.
	for m, kind := range loadProtocol(t).ServerRequestAnswers {
		a, ok := byMethod[m]
		if !ok {
			continue // the approvals, which TestApprovalRequestUsesApprove covers
		}
		if _, isErr := a["error"]; isErr != (kind == "error") {
			t.Errorf("%s: answered %v, protocol.json says %q", m, a, kind)
		}
	}
}

func TestTranscriptEnvelopesIncomingOnly(t *testing.T) {
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_LATER_FIXTURE="+fixturePath(t, "approval-turn"))
	const prompt = "PROMPT-MARKER-7731: you are Athena's worker"
	s := g.start(t, rt.StartOptions{SystemPrompt: prompt, Approve: func(context.Context, rt.ApprovalRequest) rt.Verdict { return rt.Verdict{Allow: true} }})
	for _, msg := range []string{"Say done.", "Clean the build directory."} {
		if err := s.Send(msg, ""); err != nil {
			t.Fatal(err)
		}
		next(t, s, isTurnEnd)
	}
	b, err := os.ReadFile(g.transcript)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, secret := range []string{prompt, "acct_fixture", "planType", "userAgent"} {
		if strings.Contains(text, secret) {
			t.Errorf("the transcript holds %q, which only a response carries", secret)
		}
	}
	if strings.Contains(text, "item/agentMessage/delta") {
		t.Error("the transcript holds a delta the session opted out of")
	}
	methods := map[string]bool{}
	for i, l := range strings.Split(strings.TrimSpace(text), "\n") {
		var env struct {
			At    string          `json:"at"`
			Codex json.RawMessage `json:"codex"`
		}
		if err := json.Unmarshal([]byte(l), &env); err != nil {
			t.Fatalf("line %d is not an envelope: %v: %s", i+1, err, l)
		}
		if _, err := time.Parse(time.RFC3339Nano, env.At); err != nil {
			t.Errorf("line %d: at %q: %v", i+1, env.At, err)
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(env.Codex, &m); err != nil || m.Method == "" || m.Result != nil {
			t.Errorf("line %d is not a notification or a server request: %s", i+1, env.Codex)
		}
		methods[m.Method] = true
	}
	for _, want := range []string{"thread/started", "turn/started", "item/completed", "turn/completed", "item/commandExecution/requestApproval"} {
		if !methods[want] {
			t.Errorf("the transcript lacks %s (has %v)", want, methods)
		}
	}
}

func TestExitMidTurnEndsEvents(t *testing.T) {
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_EXIT_MID_TURN=1")
	s, err := g.r.Start(context.Background(), g.options(rt.StartOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("Say done.", ""); err != nil {
		t.Fatal(err)
	}
	var evs []rt.Event
	timeout := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case e, ok := <-s.Events():
			if !ok {
				done = true
				break
			}
			evs = append(evs, e)
		case <-timeout:
			t.Fatalf("events did not close after the exit; got %#v", evs)
		}
	}
	if slices.ContainsFunc(evs, isTurnEnd) {
		t.Fatalf("the process exited mid-turn; there must be no TurnEnd: %#v", evs)
	}
	if !slices.ContainsFunc(evs, echoOf("Say done.")) {
		t.Fatalf("the events before the exit are lost: %#v", evs)
	}
	if err := s.Wait(); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("Wait = %v, want exit status 3", err)
	}
	if err := s.Send("again", ""); !errors.Is(err, rt.ErrExited) {
		t.Fatalf("Send after the exit = %v, want rt.ErrExited", err)
	}
	if err := s.InterruptTurn(); !errors.Is(err, rt.ErrExited) {
		t.Fatalf("InterruptTurn after the exit = %v, want rt.ErrExited", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaReadAtStart(t *testing.T) {
	g := newRig(t)
	s := g.start(t, rt.StartOptions{})
	evs := next(t, s, func(e rt.Event) bool { _, ok := e.(rt.QuotaUpdate); return ok })
	q := evs[len(evs)-1].(rt.QuotaUpdate)
	at := func(sec int64) *time.Time { t := time.Unix(sec, 0).UTC(); return &t }
	want := []rt.Window{{Bucket: "codex", Name: "5h", Used: 0.12, ResetsAt: at(1893456000 + 18000)}, {Bucket: "codex", Name: "7d", Used: 0.04, ResetsAt: at(1893456000 + 604800)}}
	if !reflect.DeepEqual(q.Windows, want) {
		t.Fatalf("QuotaUpdate = %+v, want %+v", q.Windows, want)
	}
	if n := len(g.requests(t, "account/rateLimits/read")); n != 1 {
		t.Fatalf("%d rate-limit reads, want 1", n)
	}

	// A Codex that cannot say (here: logged out) still starts a session, without a QuotaUpdate.
	g = newRig(t, "ATHENA_FAKE_CODEX_LOGGED_OUT=1", "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"))
	s = g.start(t, rt.StartOptions{})
	if err := s.Send("Say done.", ""); err != nil {
		t.Fatal(err)
	}
	for _, e := range next(t, s, isTurnEnd) {
		if _, ok := e.(rt.QuotaUpdate); ok {
			t.Fatalf("a QuotaUpdate without rate limits: %#v", e)
		}
	}
}

// R49 (2), F-11: a session is checked on its own app-server, for its own directory, before its thread starts; one that
// Athena's hooks and settings would not control is refused with the Why, and the check never trusts or writes
// anything.
func TestStartRefusesUncontrolledSessions(t *testing.T) {
	project := func(content string) func(dir string) {
		return func(dir string) {
			if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".codex", "config.toml"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name    string
		env     []string
		prepare func(dir string)
		home    func(home string)
		why     string // "" for a session that starts
		hooks   bool   // refused by the hooks (ErrHooksUncontrolled), not the settings
	}{
		{name: "a foreign hook", env: []string{"ATHENA_FAKE_CODEX_HOOKS=foreign"}, why: "a preToolUse hook Athena did not write is enabled", hooks: true},
		{name: "Athena's hooks not trusted yet", env: []string{"ATHENA_FAKE_CODEX_HOOKS=untrusted"}, why: `Athena's PreToolUse hook is "untrusted", not trusted`, hooks: true},
		{name: "a trusted project turning a feature on", env: []string{"ATHENA_FAKE_CODEX_TRUST_PROJECT=1"},
			prepare: project("[features]\nmulti_agent = true\n"), why: "the Codex feature multi_agent is on"},
		{name: "a trusted project adding an MCP server", env: []string{"ATHENA_FAKE_CODEX_TRUST_PROJECT=1"},
			prepare: project("[mcp_servers.helper]\ncommand = \"/usr/bin/helper\"\n"), why: "an MCP server Athena did not configure is set up (helper)"},
		{name: "a trusted project with any setting", env: []string{"ATHENA_FAKE_CODEX_TRUST_PROJECT=1"},
			prepare: project("model = \"other\"\n"), why: "a repository's Codex settings are loaded"},
		{name: "a project Codex does not load", prepare: project("[features]\nmulti_agent = true\n[mcp_servers.helper]\ncommand = \"/x\"\n")},
		{name: "a feature turned on in the home", home: func(home string) { editConfig(t, home, "memories = false", "memories = true") },
			why: "the Codex feature memories is on"},
		{name: "config/read without its layers", env: []string{"ATHENA_FAKE_CODEX_LAYERS=null"}, why: "Codex did not list its configuration layers"},
		{name: "config/read without Athena's user layer", env: []string{"ATHENA_FAKE_CODEX_LAYERS=nouser"},
			why: "Codex does not show Athena's config.toml as its user configuration"},
		{name: "Athena's MCP server replaced", home: func(home string) {
			editConfig(t, home, "command = \""+testAthenaBin+"\"\nargs", "command = \"/usr/bin/other\"\nargs")
		}, why: `Athena's MCP server runs "/usr/bin/other"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t, append(c.env, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"))...)
			if c.prepare != nil {
				c.prepare(g.dir)
			}
			if c.home != nil {
				c.home(g.r.Home)
			}
			before, err := os.ReadFile(filepath.Join(g.r.Home, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			s, err := g.r.Start(context.Background(), g.options(rt.StartOptions{}))
			if c.why == "" {
				if err != nil {
					t.Fatalf("Start: %v", err)
				}
				s.Close()
				for range s.Events() {
				}
				_ = s.Wait()
				return
			}
			if s != nil {
				s.Close()
				t.Fatalf("Start succeeded; want a refusal saying %q", c.why)
			}
			if err == nil || !strings.Contains(err.Error(), c.why) {
				t.Fatalf("Start = %v; want a refusal saying %q", err, c.why)
			}
			var hu ErrHooksUncontrolled
			var cu ErrConfigUncontrolled
			switch {
			case c.hooks && (!errors.As(err, &hu) || !strings.Contains(hu.Why, c.why)):
				t.Fatalf("err = %#v, want ErrHooksUncontrolled", err)
			case !c.hooks && (!errors.As(err, &cu) || !strings.Contains(cu.Why, c.why)):
				t.Fatalf("err = %#v, want ErrConfigUncontrolled", err)
			}
			if n := g.methodsSent(t, "thread/start", "thread/resume", "config/batchWrite"); len(n) != 0 {
				t.Fatalf("a refused session sent %v", n)
			}
			if after, _ := os.ReadFile(filepath.Join(g.r.Home, "config.toml")); string(after) != string(before) {
				t.Fatal("the control check changed config.toml")
			}
		})
	}
}

func editConfig(t *testing.T, home, old, new string) {
	t.Helper()
	p := filepath.Join(home, "config.toml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("config.toml lacks %q", old)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStartRefusesIncompleteOptions(t *testing.T) {
	g := newRig(t)
	for name, o := range map[string]rt.StartOptions{
		"no dir":        {Transcript: g.tr, TicketID: "T", AssignmentID: "a"},
		"no transcript": {Dir: g.dir, TicketID: "T", AssignmentID: "a"},
		"no ticket":     {Dir: g.dir, Transcript: g.tr, AssignmentID: "a"},
	} {
		if s, err := g.r.Start(context.Background(), o); err == nil || s != nil {
			t.Errorf("%s: Start = %v, %v", name, s, err)
		}
	}
	r := &Runtime{Binary: g.r.Binary, Home: g.r.Home, AthenaBin: g.r.AthenaBin}
	if _, err := r.Start(context.Background(), g.options(rt.StartOptions{})); err == nil {
		t.Error("Start without the node's socket")
	}
}

// declinedNotes returns the transcript's notes of the approvals the session declined on its own.
func (g *rig) declinedNotes(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(g.transcript)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var env struct {
			Athena map[string]any `json:"athena"`
		}
		if json.Unmarshal([]byte(l), &env) == nil && env.Athena != nil {
			out = append(out, env.Athena)
		}
	}
	return out
}

func noApprove(t *testing.T) func(context.Context, rt.ApprovalRequest) rt.Verdict {
	return func(_ context.Context, r rt.ApprovalRequest) rt.Verdict {
		t.Errorf("Approve was asked %+v, which the session must decline on its own", r)
		return rt.Verdict{Allow: true}
	}
}

// R50 I1, I2, M3: what Athena cannot show the approver in full is declined at once, never asked, and noted in the
// transcript: input to a running terminal, a file change whose paths are unknown, a permissions request with a special
// path (here the filesystem root, beside network access) or a field the schema does not have.
func TestApprovalsAthenaCannotShowAreDeclined(t *testing.T) {
	const ids = `"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","startedAtMs":1`
	req := func(method, params string) string {
		return `{"fake":"request","method":"` + method + `","params":{` + ids + `,` + params + `}}`
	}
	fixture := writeFixture(t, turnStartedLine,
		req("item/commandExecution/requestApproval", `"itemId":"item_0030","kind":"writeStdin","command":"python3","cwd":"/home/user/work"`),
		req("item/fileChange/requestApproval", `"itemId":"item_0031","reason":"edit"`),
		req("item/permissions/requestApproval", `"itemId":"item_0032","cwd":"/home/user/work","permissions":{"network":{"enabled":true},"fileSystem":{"entries":[{"access":"write","path":{"type":"special","value":{"kind":"root"}}}]}}`),
		req("item/permissions/requestApproval", `"itemId":"item_0033","cwd":"/home/user/work","permissions":{"fileSystem":{"write":["/x"]},"macos":{"accessibility":true}}`),
		turnCompletedLine)
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixture)
	s := g.start(t, rt.StartOptions{Approve: noApprove(t)})
	if err := s.Send("go", ""); err != nil {
		t.Fatal(err)
	}
	next(t, s, isTurnEnd)
	ans := g.answers(t)
	for id, want := range map[string]any{
		"1000": map[string]any{"decision": "decline"}, "1001": map[string]any{"decision": "decline"},
		"1002": map[string]any{"permissions": map[string]any{}}, "1003": map[string]any{"permissions": map[string]any{}},
	} {
		if !reflect.DeepEqual(ans[id]["result"], want) {
			t.Errorf("answer %s = %v, want %v", id, ans[id], want)
		}
	}
	notes := g.declinedNotes(t)
	if len(notes) != 4 {
		t.Fatalf("declined notes = %v, want 4", notes)
	}
	for i, want := range []string{"writeStdin", "paths are not known", "special path (root)", "cannot read in full"} {
		if r, _ := notes[i]["reason"].(string); !strings.Contains(r, want) {
			t.Errorf("note %d = %v, want a reason with %q", i, notes[i], want)
		}
	}
	if notes[0]["declined"] != "item/commandExecution/requestApproval" || notes[0]["id"] != float64(1000) {
		t.Errorf("note 0 = %v", notes[0])
	}
}

func TestDescribePermissions(t *testing.T) {
	for name, c := range map[string]struct {
		raw  string
		want permissionGrant
		why  string
	}{
		"writes, reads, network": {
			raw: `{"fileSystem":{"write":["/w"],"read":["/r"],"globScanMaxDepth":2,"entries":[` +
				`{"access":"write","path":{"type":"glob_pattern","pattern":"/g/**"}},{"access":"read","path":{"type":"path","path":"/r2"}},` +
				`{"access":"deny","path":{"type":"path","path":"/secret"}}]},"network":{"enabled":true}}`,
			want: permissionGrant{writes: []string{"/w", "/g/**"}, reads: []string{"/r", "/r2"}, network: true},
		},
		"network off, nulls":  {raw: `{"fileSystem":null,"network":{"enabled":false}}`},
		"special tmpdir":      {raw: `{"fileSystem":{"entries":[{"access":"read","path":{"type":"special","value":{"kind":"tmpdir"}}}]}}`, why: "special path (tmpdir)"},
		"unknown path type":   {raw: `{"fileSystem":{"entries":[{"access":"write","path":{"type":"url","url":"x"}}]}}`, why: "cannot read in full"},
		"unknown access":      {raw: `{"fileSystem":{"entries":[{"access":"admin","path":{"type":"path","path":"/x"}}]}}`, why: "unknown access (admin)"},
		"unknown network key": {raw: `{"network":{"enabled":true,"hosts":["*"]}}`, why: "cannot read in full"},
		"not an object":       {raw: `["root"]`, why: "cannot read in full"},
		"absent":              {raw: ``, why: "cannot read in full"},
		"empty path":          {raw: `{"fileSystem":{"entries":[{"access":"write","path":{"type":"path","path":""}}]}}`, why: "empty path"},
		"empty legacy write":  {raw: `{"fileSystem":{"write":["/x",""]}}`, why: "empty path"},
		"empty legacy read":   {raw: `{"fileSystem":{"read":[""]}}`, why: "empty path"},
		"two values in a row": {raw: `{} {}`, why: "cannot read in full"},
	} {
		g, why := describePermissions(json.RawMessage(c.raw))
		if c.why != "" {
			if !strings.Contains(why, c.why) {
				t.Errorf("%s: why = %q, want %q", name, why, c.why)
			}
			continue
		}
		if why != "" || !reflect.DeepEqual(g, c.want) {
			t.Errorf("%s: %+v, %q; want %+v", name, g, why, c.want)
		}
	}
}

// R50 M5: a server request still waiting for Athena when the process exits is declined, and nothing hangs.
func TestPendingApprovalAtExitIsDeclined(t *testing.T) {
	fixture := writeFixture(t, turnStartedLine,
		`{"fake":"request","wait":false,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","itemId":"item_0040","startedAtMs":1,"command":"make","cwd":"/home/user/work"}}`,
		`{"fake":"exit","code":3}`)
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixture)
	asked, ended := make(chan struct{}), make(chan error, 1)
	s, err := g.r.Start(context.Background(), g.options(rt.StartOptions{Approve: func(ctx context.Context, _ rt.ApprovalRequest) rt.Verdict {
		close(asked)
		<-ctx.Done()
		ended <- ctx.Err()
		return rt.Verdict{Allow: true}
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("go", ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for range s.Events() {
		}
		done <- s.Wait()
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exit status 3") {
			t.Fatalf("Wait = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the session hangs after its process exited with an approval pending")
	}
	<-asked
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Approve's context ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the exit did not end Approve's context")
	}
	// The request was answered (declined; the dead server cannot read it): nothing is left being answered.
	sess := s.(*session)
	deadline := time.Now().Add(5 * time.Second)
	for {
		sess.mu.Lock()
		n := sess.inflight
		sess.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests still being answered", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// R50 M5: Send and InterruptTurn racing Close neither hang nor misbehave (run under -race).
func TestSendAndInterruptRaceClose(t *testing.T) {
	hold, _ := holdFile(t)
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_LATER_FIXTURE="+fixturePath(t, "steer-turn"), hold)
	s, err := g.r.Start(context.Background(), g.options(rt.StartOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range s.Events() {
		}
	}()
	if err := s.Send("first", ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.Send("more", "") }()
		go func() { defer wg.Done(); _ = s.InterruptTurn() }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); _ = s.Close() }()
	all := make(chan struct{})
	go func() { wg.Wait(); close(all) }()
	select {
	case <-all:
	case <-time.After(20 * time.Second):
		t.Fatal("Send, InterruptTurn or Close hangs")
	}
	_ = s.Wait()
	if err := s.Send("after", ""); !errors.Is(err, rt.ErrExited) {
		t.Fatalf("Send after Close = %v", err)
	}
	if err := s.InterruptTurn(); !errors.Is(err, rt.ErrExited) {
		t.Fatalf("InterruptTurn after Close = %v", err)
	}
}

// R50 M5: Codex names a turn (turn/started) before it answers the turn/start; an interrupt then goes out at once,
// without waiting for the answer.
func TestInterruptBeforeTheTurnStartAnswer(t *testing.T) {
	release := filepath.Join(t.TempDir(), "reply")
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_HOLD_TURN_START_REPLY="+release,
		"ATHENA_FAKE_CODEX_HOLD_RESULT="+filepath.Join(t.TempDir(), "never"))
	s := g.start(t, rt.StartOptions{})
	sent := make(chan error, 1)
	go func() { sent <- s.Send("go", "") }()
	next(t, s, echoOf("go")) // turn/started came: the turn has its id, its answer has not
	if err := s.InterruptTurn(); err != nil {
		t.Fatal(err)
	}
	if end := turnEnd(next(t, s, isTurnEnd)); end.Subtype != "interrupted" {
		t.Fatalf("TurnEnd = %+v", end)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if reqs := g.requests(t, "turn/interrupt"); len(reqs) != 1 || params(reqs[0])["turnId"] != "turn_fixture_0001" {
		t.Fatalf("turn/interrupt requests = %v", reqs)
	}
	// The late answer does not revive the turn that ended: a message now starts a new one.
	s.(*session).mu.Lock()
	active := s.(*session).activeTurn
	s.(*session).mu.Unlock()
	if active != "" {
		t.Fatalf("active turn = %q after the turn ended", active)
	}
}

// R50 M8: an interrupt while a turn/start waits for Codex, before Codex has named the turn, is kept and sent as soon as
// the turn has an id.
func TestInterruptWhileTheTurnStarts(t *testing.T) {
	release := filepath.Join(t.TempDir(), "start")
	g := newRig(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_HOLD_TURN_START="+release,
		"ATHENA_FAKE_CODEX_HOLD_RESULT="+filepath.Join(t.TempDir(), "never"))
	s := g.start(t, rt.StartOptions{})
	sent := make(chan error, 1)
	go func() { sent <- s.Send("go", "") }()
	sess := s.(*session)
	deadline := time.Now().Add(5 * time.Second)
	for {
		sess.mu.Lock()
		starting := sess.starting
		sess.mu.Unlock()
		if starting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn/start never went out")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.InterruptTurn(); err != nil {
		t.Fatal(err)
	}
	if n := len(g.requests(t, "turn/interrupt")); n != 0 {
		t.Fatalf("an interrupt went out with no turn named (%d)", n)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if end := turnEnd(next(t, s, isTurnEnd)); end.Subtype != "interrupted" {
		t.Fatalf("TurnEnd = %+v, want the started turn interrupted", end)
	}
	if reqs := g.requests(t, "turn/interrupt"); len(reqs) != 1 || params(reqs[0])["turnId"] != "turn_fixture_0001" {
		t.Fatalf("turn/interrupt requests = %v", reqs)
	}
}
