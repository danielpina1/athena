package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

// These tests live here, not beside fakecodex, because `go test ./...` skips testdata directories.

var (
	fakeOnce sync.Once
	fakeDir  string
	fakePath string
	fakeErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeDir != "" {
		os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}

// fakeBinary builds testdata/fakecodex once per test binary run.
func fakeBinary(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakeDir, fakeErr = os.MkdirTemp("", "fakecodex")
		if fakeErr != nil {
			return
		}
		fakePath = filepath.Join(fakeDir, "codex")
		if out, err := exec.Command("go", "build", "-o", fakePath, "./testdata/fakecodex").CombinedOutput(); err != nil {
			fakeErr = errors.New("build fakecodex: " + err.Error() + "\n" + string(out))
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakePath
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(fixtureDir, name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeProc is a running fakecodex with a Conn on it; what it sends arrives translated on events, and its server
// requests on requests.
type fakeProc struct {
	t        *testing.T
	cmd      *exec.Cmd
	conn     *Conn
	events   chan rt.Event
	requests chan Message
	exited   chan error
	home     string
	rpcLog   string
}

// startFake runs `fakecodex app-server`; env is the ATHENA_FAKE_CODEX_* knobs.
func startFake(t *testing.T, env ...string) *fakeProc {
	t.Helper()
	return startFakeWith(t, nil, env...)
}

// startFakeWith is startFake with prepare run on its CODEX_HOME before the process starts (so --strict-config sees
// what prepare wrote).
func startFakeWith(t *testing.T, prepare func(home string), env ...string) *fakeProc {
	t.Helper()
	dir := t.TempDir()
	p := &fakeProc{t: t, events: make(chan rt.Event, 256), requests: make(chan Message, 16), exited: make(chan error, 1), home: filepath.Join(dir, "codex-home"), rpcLog: filepath.Join(dir, "rpc.jsonl")}
	if err := os.MkdirAll(p.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(p.home)
	}
	cmd := exec.Command(fakeBinary(t), "app-server", "--listen", "stdio://", "--strict-config")
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "CODEX_HOME=" + p.home, "ATHENA_TICKET=t1", "ATHENA_FAKE_CODEX_RPC_LOG=" + p.rpcLog}, env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.cmd = cmd
	p.conn = NewConn(stdout, stdin, nil)
	tr := NewTranslator()
	go func() {
		p.conn.Run(func(m Message) {
			if len(m.ID) > 0 {
				p.requests <- m
				return
			}
			for _, e := range tr.Translate(m) {
				p.events <- e
			}
		})
		// StdoutPipe may only be waited on after its reads are done.
		p.exited <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-p.exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	return p
}

func (p *fakeProc) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.conn.Call(ctx, method, params, result)
}

func (p *fakeProc) must(method string, params, result any) {
	p.t.Helper()
	if err := p.call(method, params, result); err != nil {
		p.t.Fatalf("%s: %v", method, err)
	}
}

// begin performs the handshake and thread/start; it returns the thread id.
func (p *fakeProc) begin() string {
	p.t.Helper()
	p.must("initialize", map[string]any{"clientInfo": map[string]string{"name": "athena", "version": "test"}}, nil)
	if err := p.conn.Notify("initialized", nil); err != nil {
		p.t.Fatal(err)
	}
	var res struct{ Thread struct{ ID string } }
	p.must("thread/start", map[string]any{"cwd": "/home/user/work", "model": "gpt-test"}, &res)
	return res.Thread.ID
}

// startTurn sends a turn/start and returns the turn's id.
func (p *fakeProc) startTurn(thread, text, uuid string) string {
	p.t.Helper()
	var res struct{ Turn struct{ ID string } }
	p.must("turn/start", map[string]any{"threadId": thread, "clientUserMessageId": uuid, "input": []map[string]any{{"type": "text", "text": text}}}, &res)
	return res.Turn.ID
}

// until collects events until one satisfies stop (it is included).
func (p *fakeProc) until(stop func(rt.Event) bool) []rt.Event {
	p.t.Helper()
	var evs []rt.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e := <-p.events:
			evs = append(evs, e)
			if stop(e) {
				return evs
			}
		case <-timeout:
			p.t.Fatalf("timed out; events so far: %+v", evs)
		}
	}
}

func isTurnEnd(e rt.Event) bool { _, ok := e.(rt.TurnEnd); return ok }

func (p *fakeProc) rpcLines() []map[string]any {
	p.t.Helper()
	b, err := os.ReadFile(p.rpcLog)
	if err != nil {
		p.t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			p.t.Fatalf("rpc log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func TestFakecodexSpeaksTheFixture(t *testing.T) {
	argsLog := filepath.Join(t.TempDir(), "args.json")
	p := startFake(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_ARGS_LOG="+argsLog)
	thread := p.begin()
	if thread != "thr_fixture_t1" {
		t.Fatalf("thread id = %q", thread)
	}
	turn := p.startTurn(thread, "hello there", "u-1")
	if turn != "turn_fixture_0001" {
		t.Fatalf("turn id = %q", turn)
	}
	got := p.until(isTurnEnd)
	want := []rt.Event{
		rt.Init{SessionID: "thr_fixture_t1", Model: "gpt-test"}, // fakecodex's own thread/started, once
		rt.Unknown{Kind: "turn/started"},
		rt.Unknown{Kind: "userMessage"},
		rt.UserEcho{Text: "hello there", UUID: "u-1"}, // what was sent, as the real server echoes it
		rt.Unknown{Kind: "agentMessage"},
		rt.Unknown{Kind: "item/agentMessage/delta"},
		rt.AssistantText{Text: "Done."},
		rt.TurnEnd{SessionID: "thr_fixture_t1", OK: true, Text: "Done.", InputTokens: 1114, OutputTokens: 12, CacheReadTokens: 4096, NumTurns: 1, Duration: 4200 * time.Millisecond},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n%+v\nwant:\n%+v", got, want)
	}

	// Its other answers.
	var acct struct {
		Account map[string]any
	}
	p.must("account/read", nil, &acct)
	if acct.Account["type"] != "chatgpt" {
		t.Fatalf("account = %v", acct)
	}
	var limits struct {
		AccountID  string
		RateLimits struct{ Primary struct{ UsedPercent int } }
	}
	p.must("account/rateLimits/read", nil, &limits)
	if limits.AccountID != "acct_fixture" || limits.RateLimits.Primary.UsedPercent != 12 {
		t.Fatalf("limits = %+v", limits)
	}
	p.must("config/batchWrite", map[string]any{"edits": []any{}}, nil)
	var re *RPCError
	if err := p.call("no/such/method", nil, nil); !errors.As(err, &re) || re.Code != -32601 {
		t.Fatalf("unknown method: %v", err)
	}

	// The RPC log has every request, and the args log the arguments and CODEX_HOME.
	var methods []string
	for _, l := range p.rpcLines() {
		methods = append(methods, l["method"].(string))
	}
	if w := []string{"initialize", "initialized", "thread/start", "turn/start", "account/read", "account/rateLimits/read", "config/batchWrite", "no/such/method"}; !reflect.DeepEqual(methods, w) {
		t.Fatalf("rpc log = %v", methods)
	}
	b, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Args      []string
		CodexHome string
		Env       map[string]string
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.Args, []string{"app-server", "--listen", "stdio://", "--strict-config"}) || rec.CodexHome != p.home || rec.Env["ATHENA_TICKET"] != "t1" {
		t.Fatalf("args log = %+v", rec)
	}
}

// A steer folds into the running turn, one turn/completed ends it, a later turn/start replays the later fixture, and
// a steer with no turn to steer is refused.
func TestFakecodexSteerLaterTurnAndHold(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "release")
	p := startFake(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_LATER_FIXTURE="+fixturePath(t, "steer-turn"), "ATHENA_FAKE_CODEX_HOLD_RESULT="+hold)
	thread := p.begin()
	turn := p.startTurn(thread, "first", "u-1")
	p.until(func(e rt.Event) bool { a, ok := e.(rt.AssistantText); return ok && a.Text == "Done." })

	var re *RPCError
	err := p.call("turn/steer", map[string]any{"threadId": thread, "expectedTurnId": "turn_other", "input": []map[string]string{{"type": "text", "text": "x"}}}, nil)
	if !errors.As(err, &re) || re.Code != -32600 {
		t.Fatalf("steer of another turn: %v", err)
	}
	var steered struct{ TurnID string }
	p.must("turn/steer", map[string]any{"threadId": thread, "expectedTurnId": turn, "clientUserMessageId": "u-2", "input": []map[string]string{{"type": "text", "text": "and this"}}}, &steered)
	if steered.TurnID != turn {
		t.Fatalf("steer turnId = %q", steered.TurnID)
	}
	evs := p.until(func(e rt.Event) bool { a, ok := e.(rt.AssistantText); return ok && a.Text == "Noted: it is sunny." })
	var echoed bool
	for _, e := range evs {
		if u, ok := e.(rt.UserEcho); ok && u.Text == "and this" && u.UUID == "u-2" {
			echoed = true
		}
		if isTurnEnd(e) {
			t.Fatalf("the turn ended while held: %+v", e)
		}
	}
	if !echoed {
		t.Fatalf("no echo of the steer in %+v", evs)
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	end := p.until(isTurnEnd)[0].(rt.TurnEnd)
	if !end.OK || end.Text != "Noted: it is sunny." { // the turn ended once, after both messages
		t.Fatalf("TurnEnd = %+v", end)
	}
	select {
	case e := <-p.events:
		t.Fatalf("an extra event after the turn's end: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}

	// The turn is over: a steer is refused, a turn/start replays the later fixture as a whole turn.
	if err := p.call("turn/steer", map[string]any{"threadId": thread, "expectedTurnId": turn, "input": []any{}}, nil); !errors.As(err, &re) {
		t.Fatalf("steer after the end: %v", err)
	}
	if id := p.startTurn(thread, "second", "u-3"); id != "turn_fixture_0002" {
		t.Fatalf("second turn id = %q", id)
	}
	evs = p.until(isTurnEnd)
	if u, ok := evs[2].(rt.UserEcho); !ok || u.Text != "second" || u.UUID != "u-3" {
		t.Fatalf("second turn events: %+v", evs)
	}
}

func TestFakecodexInterruptEndsTheTurnInterrupted(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "never")
	p := startFake(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_HOLD_RESULT="+hold)
	thread := p.begin()
	turn := p.startTurn(thread, "go", "")
	p.until(func(e rt.Event) bool { a, ok := e.(rt.AssistantText); return ok && a.Text == "Done." })
	p.must("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn}, nil)
	end := p.until(isTurnEnd)[0].(rt.TurnEnd)
	if end.OK || end.Subtype != "interrupted" || end.SessionID != thread {
		t.Fatalf("TurnEnd = %+v", end)
	}
	// The held turn/completed never arrives after it, and the process takes the next turn.
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-p.events:
		t.Fatalf("an event after the interrupt: %+v", e)
	case <-time.After(150 * time.Millisecond):
	}
	if id := p.startTurn(thread, "again", ""); id != "turn_fixture_0002" {
		t.Fatalf("next turn id = %q", id)
	}
}

func TestFakecodexSendsServerRequestsAndLogsTheAnswer(t *testing.T) {
	p := startFake(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "approval-turn"))
	thread := p.begin()
	p.startTurn(thread, "clean", "")
	var req Message
	select {
	case req = <-p.requests:
	case <-time.After(10 * time.Second):
		t.Fatal("no server request")
	}
	if req.Method != "item/commandExecution/requestApproval" || string(req.ID) != "1000" {
		t.Fatalf("request = %+v", req)
	}
	// The turn waits for the answer.
	select {
	case e := <-p.events:
		if _, ok := e.(rt.ToolResult); ok {
			t.Fatalf("the turn went on before the answer: %+v", e)
		}
	case <-time.After(100 * time.Millisecond):
	}
	if err := p.conn.Respond(req.ID, map[string]string{"decision": "accept"}); err != nil {
		t.Fatal(err)
	}
	evs := p.until(isTurnEnd)
	var results int
	for _, e := range evs {
		if _, ok := e.(rt.ToolResult); ok {
			results++
		}
	}
	if results != 1 || !evs[len(evs)-1].(rt.TurnEnd).OK {
		t.Fatalf("events after the answer: %+v", evs)
	}
	var answered bool
	for _, l := range p.rpcLines() {
		if l["id"] == float64(1000) && strings.Contains(toJSON(l["result"]), `"accept"`) {
			answered = true
		}
	}
	if !answered {
		t.Fatalf("the answer is not in the rpc log: %v", p.rpcLines())
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

const hookConfig = "# written by a test\n[[hooks.PreToolUse]]\n[[hooks.PreToolUse.hooks]]\ntype = \"command\"\ncommand = %q\n\n[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"true\"\n"

// writeHookHome puts a PreToolUse hook that records its stdin and denies in $CODEX_HOME/config.toml.
func writeHookHome(t *testing.T, home string) (stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	stdinFile = filepath.Join(dir, "hook-stdin.json")
	script := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat > "+stdinFile+"\necho '{\"hookSpecificOutput\":{\"permissionDecision\":\"deny\"}}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(hookConfig, "%q", `"`+script+`"`, 1)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return stdinFile
}

func TestFakecodexHookDenialDropsTheItem(t *testing.T) {
	fx := filepath.Join(t.TempDir(), "hook-turn.jsonl")
	lines := []string{
		`{"method":"turn/started","params":{"threadId":"thr_fixture_0001","turn":{"id":"turn_fixture_0001","items":[],"status":"inProgress"}}}`,
		`{"fake":"hook","event":"PreToolUse","input":{"tool_name":"Bash","tool_input":{"command":"rm -rf build"}}}`,
		`{"method":"item/started","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","startedAtMs":1,"item":{"id":"item_9","type":"commandExecution","command":"rm -rf build","cwd":"/w","commandActions":[],"status":"inProgress"}}}`,
		`{"method":"item/completed","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","completedAtMs":2,"item":{"id":"item_9","type":"commandExecution","command":"rm -rf build","cwd":"/w","commandActions":[],"status":"completed","exitCode":0}}}`,
		`{"method":"item/completed","params":{"threadId":"thr_fixture_0001","turnId":"turn_fixture_0001","completedAtMs":3,"item":{"id":"item_10","type":"agentMessage","text":"ok"}}}`,
		`{"method":"turn/completed","params":{"threadId":"thr_fixture_0001","turn":{"id":"turn_fixture_0001","items":[],"status":"completed","durationMs":5}}}`,
	}
	if err := os.WriteFile(fx, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startFake(t, "ATHENA_FAKE_CODEX_FIXTURE="+fx)
	stdinFile := writeHookHome(t, p.home)
	thread := p.begin()
	p.startTurn(thread, "x", "")
	for _, e := range p.until(isTurnEnd) {
		switch e.(type) {
		case rt.ToolUse, rt.ToolResult:
			t.Fatalf("the denied item reached the client: %+v", e)
		}
	}
	in, err := os.ReadFile(stdinFile)
	if err != nil || !strings.Contains(string(in), "rm -rf build") {
		t.Fatalf("hook stdin = %q, %v", in, err)
	}
	var ran bool
	for _, l := range p.rpcLines() {
		if l["fake"] == "hook" && l["event"] == "PreToolUse" && l["denied"] == true && l["exit"] == float64(0) {
			ran = true
		}
	}
	if !ran {
		t.Fatalf("no hook record in %v", p.rpcLines())
	}
}

func TestFakecodexHooksListFollowsTheMode(t *testing.T) {
	type entry struct {
		EventName   string
		Command     string
		Enabled     bool
		Source      string
		TrustStatus string
	}
	list := func(mode string) []entry {
		var env []string
		if mode != "" {
			env = []string{"ATHENA_FAKE_CODEX_HOOKS=" + mode}
		}
		p := startFake(t, env...)
		writeHookHome(t, p.home)
		p.must("initialize", map[string]any{"clientInfo": map[string]string{"name": "athena", "version": "test"}}, nil)
		var res struct {
			Data []struct {
				Cwd   string
				Hooks []entry
			}
		}
		p.must("hooks/list", map[string]any{"cwds": []string{"/home/user/work"}}, &res)
		if len(res.Data) != 1 || res.Data[0].Cwd != "/home/user/work" {
			t.Fatalf("%s: hooks/list = %+v", mode, res)
		}
		return res.Data[0].Hooks
	}
	for _, mode := range []string{"", "trusted"} {
		h := list(mode)
		if len(h) != 2 || h[0].EventName != "preToolUse" || h[0].TrustStatus != "trusted" || !h[0].Enabled || h[0].Source != "user" || h[1].EventName != "stop" {
			t.Errorf("mode %q: hooks = %+v", mode, h)
		}
	}
	if h := list("untrusted"); len(h) != 2 || h[0].TrustStatus != "untrusted" || h[1].TrustStatus != "untrusted" {
		t.Errorf("untrusted: hooks = %+v", h)
	}
	h := list("foreign")
	if len(h) != 3 || h[2].Source != "project" || !h[2].Enabled || h[2].TrustStatus != "trusted" || h[0].Source != "user" {
		t.Errorf("foreign: hooks = %+v", h)
	}
}

func TestFakecodexUntrustedHookDoesNotRun(t *testing.T) {
	fx := filepath.Join(t.TempDir(), "f.jsonl")
	body := `{"fake":"hook","event":"PreToolUse","input":{"tool_name":"Bash"}}` + "\n" +
		`{"method":"turn/completed","params":{"threadId":"thr_fixture_0001","turn":{"id":"turn_fixture_0001","items":[],"status":"completed"}}}` + "\n"
	if err := os.WriteFile(fx, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startFake(t, "ATHENA_FAKE_CODEX_FIXTURE="+fx, "ATHENA_FAKE_CODEX_HOOKS=untrusted")
	stdinFile := writeHookHome(t, p.home)
	thread := p.begin()
	p.startTurn(thread, "x", "")
	p.until(isTurnEnd)
	if _, err := os.Stat(stdinFile); err == nil {
		t.Fatal("an untrusted hook ran")
	}
}

func TestFakecodexExitMidTurnAndLoggedOut(t *testing.T) {
	p := startFake(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "tool-turn"), "ATHENA_FAKE_CODEX_EXIT_MID_TURN=1")
	thread := p.begin()
	p.startTurn(thread, "x", "")
	select {
	case err := <-p.exited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 3 {
			t.Fatalf("exit = %v, want exit status 3", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fakecodex did not exit")
	}
	p.exited <- nil // for the cleanup
	for len(p.events) > 0 {
		if isTurnEnd(<-p.events) {
			t.Fatal("the turn ended; it should have died half way")
		}
	}

	q := startFake(t, "ATHENA_FAKE_CODEX_LOGGED_OUT=1")
	var acct struct{ Account any }
	q.must("account/read", nil, &acct)
	if acct.Account != nil {
		t.Fatalf("account = %v, want none", acct.Account)
	}
	var re *RPCError
	if err := q.call("account/rateLimits/read", nil, nil); !errors.As(err, &re) {
		t.Fatalf("rate limits while logged out: %v", err)
	}
}

// fakecodex knows app-server, --version and login; any other command is refused.
func TestFakecodexRefusesOtherCommands(t *testing.T) {
	err := exec.Command(fakeBinary(t), "exec", "hello").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("err = %v", err)
	}
}

func TestFakecodexVersionAndLogin(t *testing.T) {
	cmd := exec.Command(fakeBinary(t), "--version")
	cmd.Env = []string{"ATHENA_FAKE_CODEX_VERSION=0.161.2"}
	if out, err := cmd.Output(); err != nil || string(out) != "codex-cli 0.161.2\n" {
		t.Fatalf("--version = %q, %v", out, err)
	}
	if out, err := exec.Command(fakeBinary(t), "--version").Output(); err != nil || string(out) != "codex-cli "+VerifiedVersion+"\n" {
		t.Fatalf("default --version = %q, %v", out, err)
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.json")
	cmd = exec.Command(fakeBinary(t), "login", "--with-api-key")
	cmd.Env = []string{"CODEX_HOME=" + filepath.Join(dir, "home"), "ATHENA_FAKE_CODEX_ARGS_LOG=" + argsLog}
	cmd.Stdin = strings.NewReader("sk-fixture-not-a-key\n")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "sk-fixture") {
		t.Fatalf("login = %q, %v", out, err)
	}
	b, err := os.ReadFile(argsLog)
	if err != nil || strings.Contains(string(b), "sk-fixture") {
		t.Fatalf("args log = %q, %v", b, err)
	}
	var rec struct {
		Args      []string
		CodexHome string
	}
	if err := json.Unmarshal(b, &rec); err != nil || !reflect.DeepEqual(rec.Args, []string{"login", "--with-api-key"}) || rec.CodexHome != filepath.Join(dir, "home") {
		t.Fatalf("args log = %+v, %v", rec, err)
	}
}

// Without a readable protocol.json fakecodex refuses to serve unchecked, unless told to (ATHENA_FAKE_CODEX_PROTOCOL=off).
func TestFakecodexNeedsItsProtocol(t *testing.T) {
	run := func(proto string) error {
		cmd := exec.Command(fakeBinary(t), "app-server", "--listen", "stdio://")
		cmd.Env = []string{"CODEX_HOME=" + t.TempDir(), "ATHENA_FAKE_CODEX_PROTOCOL=" + proto}
		cmd.Stdin = strings.NewReader("")
		return cmd.Run()
	}
	var ee *exec.ExitError
	if err := run(filepath.Join(t.TempDir(), "missing.json")); !errors.As(err, &ee) || ee.ExitCode() != 5 {
		t.Fatalf("missing protocol.json: err = %v, want exit 5", err)
	}
	if err := run("off"); err != nil {
		t.Fatalf("protocol off: %v", err)
	}
}

// fakecodex is as strict as the real server: a request is checked against protocol.json (params an object when the
// method requires params, the required fields present), so a driver that passes here passes there.
func TestFakecodexRejectsInvalidParams(t *testing.T) {
	p := startFake(t)
	p.must("initialize", map[string]any{"clientInfo": map[string]string{"name": "athena", "version": "test"}}, nil)
	var re *RPCError
	for _, c := range []struct {
		name, method string
		params       any
	}{
		{"null params", "account/read", json.RawMessage("null")},
		// No case leaves params out: Conn always sends them ({} for nil).
		{"params not an object", "hooks/list", json.RawMessage("[]")},
		{"missing required field", "thread/resume", map[string]any{}},
		{"turn/start without input", "turn/start", map[string]any{"threadId": "t"}},
	} {
		if err := p.call(c.method, c.params, nil); !errors.As(err, &re) || re.Code != -32600 {
			t.Errorf("%s: err = %v, want -32600", c.name, err)
		}
	}
	// What the driver sends is accepted: nil becomes {}, and params that need none may be anything object-shaped.
	p.must("account/read", nil, nil)
	p.must("hooks/list", nil, nil)
	p.must("account/rateLimits/read", nil, nil)
}

// A hooks config in the shape Codex's --strict-config rejects (an entry with no matcher group before it, a handler
// with no type) stops fakecodex at start, and a config in the real shape does not.
func TestFakecodexStrictConfigRejectsTheOldHookShape(t *testing.T) {
	for _, c := range []struct {
		name, cfg string
		ok        bool
	}{
		{"real shape", "[[hooks.PreToolUse]]\n[[hooks.PreToolUse.hooks]]\ntype = \"command\"\ncommand = \"true\"\n", true},
		{"matcher group", "[[hooks.PreToolUse]]\nmatcher = \"Bash\"\n[[hooks.PreToolUse.hooks]]\ncommand = \"true\"\ntype = \"command\"\n", true},
		{"no matcher group", "[[hooks.PreToolUse.hooks]]\ntype = \"command\"\ncommand = \"true\"\n", false},
		{"no type", "[[hooks.PreToolUse]]\n[[hooks.PreToolUse.hooks]]\ncommand = \"true\"\n", false},
		{"group of another event", "[[hooks.Stop]]\n[[hooks.PreToolUse.hooks]]\ntype = \"command\"\ncommand = \"true\"\n", false},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(c.cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(fakeBinary(t), "app-server", "--listen", "stdio://", "--strict-config")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CODEX_HOME=" + dir}
		in, _ := cmd.StdinPipe()
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case err := <-exited:
			if c.ok || err == nil || !strings.Contains(stderr.String(), "config.toml") {
				t.Errorf("%s: exited early: %v, %q", c.name, err, stderr.String())
			}
		case <-time.After(500 * time.Millisecond):
			if !c.ok {
				t.Errorf("%s: still running, want it to refuse the config", c.name)
			}
			in.Close()
			<-exited
		}
	}
}

// turn/start while a turn runs steers into it, as the real server does, instead of being refused.
func TestFakecodexTurnStartDuringATurnSteersIt(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "release")
	p := startFake(t, "ATHENA_FAKE_CODEX_FIXTURE="+fixturePath(t, "simple-turn"), "ATHENA_FAKE_CODEX_LATER_FIXTURE="+fixturePath(t, "steer-turn"), "ATHENA_FAKE_CODEX_HOLD_RESULT="+hold)
	thread := p.begin()
	turn := p.startTurn(thread, "first", "u-1")
	p.until(func(e rt.Event) bool { a, ok := e.(rt.AssistantText); return ok && a.Text == "Done." })
	if id := p.startTurn(thread, "more", "u-2"); id != turn {
		t.Fatalf("turn id = %q, want the running turn %q", id, turn)
	}
	evs := p.until(func(e rt.Event) bool { a, ok := e.(rt.AssistantText); return ok && a.Text == "Noted: it is sunny." })
	var echoed bool
	for _, e := range evs {
		if u, ok := e.(rt.UserEcho); ok && u.Text == "more" && u.UUID == "u-2" {
			echoed = true
		}
	}
	if !echoed {
		t.Fatalf("no echo in %+v", evs)
	}
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p.until(isTurnEnd)
}

// protoextract is not built by `go build ./...` (it lives under testdata), so build it here.
func TestProtoextractCompiles(t *testing.T) {
	out := filepath.Join(t.TempDir(), "protoextract")
	if b, err := exec.Command("go", "build", "-o", out, "./testdata/protoextract").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
}
