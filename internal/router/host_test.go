package router

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/runtime/claude"
	"github.com/danielpina1/athena/internal/sources"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/prompts"
)

const waitFor = 10 * time.Second

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

// nopExec is an Executor that runs nothing.
type nopExec struct{}

func (nopExec) Start(core.Ticket, core.Assignment)        {}
func (nopExec) Deliver(core.Ticket, core.Assignment)      {}
func (nopExec) Stop(core.Ticket, core.Assignment, string) {}

// fakeSource is the github Source: it listens to every Thread and reads a fixed window.
type fakeSource struct{}

func (fakeSource) Name() string                  { return "github" }
func (fakeSource) Listens(string) bool           { return true }
func (fakeSource) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (fakeSource) Health() error                 { return nil }
func (fakeSource) References(text, thread string) []string {
	return github.ParseReferences(text, thread)
}
func (fakeSource) Context(_ context.Context, thread, triggerID string) (sources.ContextWindow, error) {
	return sources.ContextWindow{
		Messages: []sources.Msg{
			{ID: "github:issue:9", From: "bob", At: "2026-11-01T10:00:00Z", Text: "the label says Sumbit", Chars: 0},
			{ID: triggerID, From: "alice", At: "2026-11-01T11:00:00Z", Text: "just fix the typo"},
			{ID: "github:issuecomment:77", From: "carol", At: "2026-11-01T11:05:00Z", Text: strings.Repeat("x", 400), Chars: 900},
		},
		Stubs: []sources.Stub{{ID: "github:issuecomment:78", From: "dan", Preview: "unrelated"}},
	}, nil
}

// env is a store, an Engine with the real Policy, an effects Runner, a Registry and a router Host on fakeclaude.
type env struct {
	t        *testing.T
	st       *store.Store
	eng      *core.Engine
	fx       *effects.Runner
	reg      *sources.Registry
	host     *Host
	cfg      Config
	settings policy.Settings
	stdinLog string
	argsLog  string
	cancel   context.CancelFunc
	// wake is the current Runner's Wake, so the Engine wakes a Runner made by restart.
	wake atomic.Pointer[effects.Runner]
	log  *slog.Logger
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{t: t, stdinLog: filepath.Join(base, "stdin.log"), argsLog: filepath.Join(base, "args.log")}
	t.Setenv("ATHENA_FAKE_FIXTURE", claudeFixture(t, "simple-turn.jsonl"))
	t.Setenv("ATHENA_FAKE_STDIN_LOG", e.stdinLog)
	t.Setenv("ATHENA_FAKE_ARGS_APPEND", e.argsLog)
	st, err := store.Open(filepath.Join(base, "state", "athena.db"))
	if err != nil {
		t.Fatal(err)
	}
	e.st = st
	e.reg = sources.NewRegistry()
	e.reg.Register(fakeSource{})
	e.settings = policy.Defaults([]string{"alice"})
	e.settings.Rules = []policy.Rule{{Name: "copy-fixes", Instructions: "typo and wording fixes", Enabled: true},
		{Name: "off-rule", Instructions: "never", Enabled: false}}
	e.fx = effects.New(st, nil)
	e.wake.Store(e.fx)
	e.eng = core.NewEngineWith(st.Repos(), nopExec{}, core.Options{
		Policy: func() core.Policy { return policy.New(e.settings, e.reg, time.Minute) }, Scope: e.reg, Wake: func() { e.wake.Load().Wake() }, Relays: true,
	})
	sockDir, err := os.MkdirTemp("/tmp", "athrt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	e.cfg = Config{
		Bin: "/usr/bin/true", StateDir: filepath.Join(base, "state"), Model: "haiku",
		Socket: filepath.Join(sockDir, "n.sock"), ClaudeBinary: fakeClaude(t), KV: st,
		Policy: func() (policy.Settings, error) { return e.settings, nil },
	}
	e.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	e.host = New(e.cfg, e.eng, e.fx, e.reg, e.log)
	e.host.restartDelay = func(int) time.Duration { return 0 }
	t.Cleanup(func() {
		if e.cancel != nil {
			e.host.Stop()
			e.cancel()
			_ = e.fx.Drain(time.Second)
		}
		st.Close()
	})
	return e
}

// start starts the Host and the effects Runner.
func (e *env) start() {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	if err := e.host.Start(ctx); err != nil {
		e.t.Fatal(err)
	}
	e.fx.Start(ctx)
}

func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// messages returns the user messages the router session received, in order.
func (e *env) messages() []string {
	f, err := os.Open(e.stdinLog)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var m struct {
			Type    string `json:"type"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Type == "user" {
			out = append(out, m.Message.Content)
		}
	}
	return out
}

type invocation struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

func (e *env) invocations() []invocation {
	raw, err := os.ReadFile(e.argsLog)
	if err != nil {
		return nil
	}
	var out []invocation
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var inv invocation
		if json.Unmarshal([]byte(line), &inv) == nil {
			out = append(out, inv)
		}
	}
	return out
}

func flagValue(args []string, name string) (string, bool) {
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

// explicit is alice's comment that mentions the agent on thread.
func explicit(id, thread string) core.Trigger {
	return core.Trigger{ID: id, Source: "github", Kind: core.KindMessage, Actor: core.ActorRef{Source: "github", ID: "alice"},
		Human: true, Addressed: core.AddressedMention, Scope: "comments", Threads: []string{thread}, At: time.Now(),
		Text: "@athena please fix " + id, Channels: []core.ChannelRef{{Source: "github", Thread: thread,
			Address: map[string]string{"repo": "o/r"}}}}
}

// inferred is alice's comment that does not mention the agent, in a scope that receives every message.
func inferred(id, thread string) core.Trigger {
	tr := explicit(id, thread)
	tr.Addressed = ""
	tr.Text = "just fix the typo"
	tr.Meta = map[string]string{"receiveAll": "true"}
	return tr
}

func (e *env) receive(tr core.Trigger) {
	e.t.Helper()
	got, err := e.eng.Receive(tr)
	if err != nil || got != core.ReceiveStored {
		e.t.Fatalf("Receive %s: %s %v", tr.ID, got, err)
	}
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// accept receives an explicit request and accepts it directly; it returns the Ticket.
func (e *env) accept(id, thread string) core.Ticket {
	e.t.Helper()
	e.receive(explicit(id, thread))
	e.must(e.eng.EvaluateDue())
	r, err := e.eng.Decide(core.DecisionInput{TriggerID: id, Kind: core.DecisionAccept, Title: "Fix " + id})
	if err != nil || r.Ticket == nil {
		e.t.Fatalf("accept %s: %+v %v", id, r, err)
	}
	return *r.Ticket
}

func TestRouterStartsWithNoTools(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.eventually("the router session to start", func() bool { return len(e.invocations()) == 1 })
	inv := e.invocations()[0]
	if v, ok := flagValue(inv.Args, "--tools"); !ok || v != "" {
		t.Fatalf("args = %q, want --tools \"\"", inv.Args)
	}
	if !slices.Contains(inv.Args, "--strict-mcp-config") {
		t.Fatalf("args lack --strict-mcp-config: %q", inv.Args)
	}
	if v, _ := flagValue(inv.Args, "--model"); v != "haiku" {
		t.Fatalf("model = %q", v)
	}
	if v, _ := flagValue(inv.Args, "--append-system-prompt"); v != prompts.Router {
		t.Fatal("the router prompt is not the system prompt")
	}
	adir := filepath.Join(e.cfg.StateDir, "router", ".athena")
	if v, _ := flagValue(inv.Args, "--settings"); v != filepath.Join(adir, "settings.json") {
		t.Fatalf("settings = %q", v)
	}
	mcpPath, _ := flagValue(inv.Args, "--mcp-config")
	if mcpPath != filepath.Join(adir, "mcp.json") {
		t.Fatalf("mcp config = %q", mcpPath)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Args []string          `json:"args"`
			Env  map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	raw, err := os.ReadFile(mcpPath)
	e.must(err)
	e.must(json.Unmarshal(raw, &cfg))
	srv := cfg.MCPServers["athena"]
	tok := srv.Env["ATHENA_ROUTER_TOKEN"]
	if len(cfg.MCPServers) != 1 || strings.Join(srv.Args, " ") != "mcp --role router" || len(tok) != 64 {
		t.Fatalf("mcp config = %s", raw)
	}
	if inv.Env["ATHENA_ROLE"] != node.RoleRouter || inv.Env["ATHENA_ROUTER_TOKEN"] != tok || inv.Env["ATHENA_TICKET"] != "" {
		t.Fatalf("session env = %v", inv.Env)
	}
	if !e.host.ValidToken(tok) || e.host.ValidToken("") || e.host.ValidToken(strings.Repeat("0", 64)) {
		t.Fatal("ValidToken disagrees with the MCP config")
	}
	st, err := os.Stat(filepath.Join(e.cfg.StateDir, "router"))
	e.must(err)
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("router dir mode = %v", st.Mode())
	}
	if _, err := os.Stat(filepath.Join(e.cfg.StateDir, "transcripts")); err != nil {
		t.Fatalf("transcript dir: %v", err)
	}
}

func TestAssessmentDeliveredThenDeadline(t *testing.T) {
	e := newEnv(t)
	thread := "github:o/r#1"
	e.receive(explicit("github:issuecomment:1", thread))
	e.must(e.eng.EvaluateDue())
	th, err := e.eng.Thread(thread)
	e.must(err)
	if th.Eval == nil || th.Eval.DeliveredAt != nil {
		t.Fatalf("eval before delivery = %+v", th.Eval)
	}
	// An assessment whose Eval ended is dropped without a message.
	e.must(e.st.Repos().Tx(func(tx core.Tx) error {
		return tx.Enqueue(core.EffectAssessment, "gone", []byte(`{"evalId":"gone","triggerId":"github:issuecomment:404"}`))
	}))
	e.start()
	e.eventually("the candidate to be delivered", func() bool {
		th, err := e.eng.Thread(thread)
		return err == nil && th.Eval != nil && th.Eval.DeliveredAt != nil
	})
	e.eventually("the candidate message", func() bool { return len(e.messages()) > 0 })
	time.Sleep(100 * time.Millisecond)
	msgs := e.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %q", msgs)
	}
	var card map[string]any
	e.must(json.Unmarshal([]byte(msgs[0]), &card))
	if card["kind"] != "candidate" || card["trigger_id"] != "github:issuecomment:1" || card["strength"] != "explicit" {
		t.Fatalf("candidate = %s", msgs[0])
	}
	e.eventually("the assessment rows to go", func() bool {
		rows, _ := e.fx.Pending(core.EffectAssessment)
		return len(rows) == 0
	})
}

func TestNoticesRideNextMessage(t *testing.T) {
	e := newEnv(t)
	thread := "github:o/r#1"
	tk := e.accept("github:issuecomment:1", thread)
	e.must(e.eng.Reevaluate("github:issuecomment:1"))                                                         // notice 1: trigger_requeued
	e.must(e.eng.Unsubscribe(thread))                                                                         // notice 2: thread_unsubscribed
	e.must(e.eng.FileReport(tk.ID, core.ReportInput{State: core.ReportBlocked, Summary: "need the API key"})) // a relay
	e.start()
	e.eventually("one message", func() bool { return len(e.messages()) == 1 })
	msg := e.messages()[0]
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	var kinds []string
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		kinds = append(kinds, fmt.Sprint(m["kind"]))
	}
	want := []string{core.NoticeTriggerRequeued, core.NoticeThreadUnsubscribed, core.RelayWorkerReport}
	if !slices.Equal(kinds, want) {
		t.Fatalf("message kinds = %v, want %v\n%s", kinds, want, msg)
	}
	if !strings.Contains(msg, "need the API key") || !strings.Contains(msg, tk.ID) {
		t.Fatalf("relay = %s", msg)
	}
	e.eventually("the notices to be taken", func() bool {
		rows, _ := e.fx.Pending(core.EffectNotice)
		return len(rows) == 0
	})

	// A notice alone does not wake the router; it waits for the next message.
	e.must(e.eng.StopTicket(tk.ID, "enough"))
	time.Sleep(300 * time.Millisecond)
	if n := len(e.messages()); n != 1 {
		t.Fatalf("a notice alone was sent: %d messages", n)
	}
	rows, err := e.fx.Pending(core.EffectNotice)
	e.must(err)
	if len(rows) != 1 {
		t.Fatalf("held notices = %+v", rows)
	}
}

func TestRouterResumesSession(t *testing.T) {
	e := newEnv(t)
	e.must(e.st.KVSet(sessionKey, "sess-before"))
	e.start()
	e.eventually("the router session to start", func() bool { return len(e.invocations()) == 1 })
	if v, _ := flagValue(e.invocations()[0].Args, "--resume"); v != "sess-before" {
		t.Fatalf("args = %q, want --resume sess-before", e.invocations()[0].Args)
	}
	// The session id the process reports is kept for the next start.
	tk := e.accept("github:issuecomment:1", "github:o/r#1")
	e.must(e.eng.FileReport(tk.ID, core.ReportInput{State: core.ReportBlocked, Summary: "stuck"}))
	e.eventually("the new session id to be kept", func() bool {
		v, _, _ := e.st.KVGet(sessionKey)
		return v == "27abadfa-c58f-4e97-aaf3-64a8fdaff179"
	})
}

func TestRouterRestartsAfterCrash(t *testing.T) {
	e := newEnv(t)
	t.Setenv("ATHENA_FAKE_EXIT_MID_TURN_ONCE", filepath.Join(t.TempDir(), "crashed"))
	tk := e.accept("github:issuecomment:1", "github:o/r#1")
	e.start()
	e.must(e.eng.FileReport(tk.ID, core.ReportInput{State: core.ReportBlocked, Summary: "stuck"}))
	e.eventually("a restart", func() bool { return len(e.invocations()) == 2 })
	invs := e.invocations()
	if v, _ := flagValue(invs[1].Args, "--resume"); v != "27abadfa-c58f-4e97-aaf3-64a8fdaff179" {
		t.Fatalf("restart args = %q, want a resume of the crashed session", invs[1].Args)
	}
	if invs[0].Env["ATHENA_ROUTER_TOKEN"] == invs[1].Env["ATHENA_ROUTER_TOKEN"] {
		t.Fatal("the restart kept the old token")
	}
	if e.host.ValidToken(invs[0].Env["ATHENA_ROUTER_TOKEN"]) {
		t.Fatal("the crashed process's token still works")
	}
	// The restarted session gets again what the crashed one may not have taken in.
	e.eventually("the resend", func() bool { return len(e.messages()) == 2 })
	msgs := e.messages()
	if !strings.HasPrefix(msgs[1], recoveryNote) || !strings.Contains(msgs[1], `"stuck"`) {
		t.Fatalf("resend = %q", msgs[1])
	}
}

func TestRouterToolTokenRequired(t *testing.T) {
	e := newEnv(t)
	n := node.New(node.Config{StateDir: e.cfg.StateDir, SocketPath: e.cfg.Socket, AthenaBin: "/usr/bin/true"}, nil)
	n.SetRouter(e.host)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- n.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-served
		n.Close()
	})
	e.start()
	e.eventually("the router session to start", func() bool { return len(e.invocations()) == 1 })
	tok := e.invocations()[0].Env["ATHENA_ROUTER_TOKEN"]
	call := func(token string) node.RouterResponse {
		t.Helper()
		var resp node.RouterResponse
		e.eventually("the socket", func() bool {
			return callSocket(e.cfg.Socket, node.RouterRequest{Op: node.OpRouter, Token: token, Tool: "read",
				Args: json.RawMessage(`{"resource":"tickets"}`)}, &resp) == nil
		})
		return resp
	}
	if r := call(""); r.OK {
		t.Fatalf("no token: %+v", r)
	}
	if r := call(strings.Repeat("ab", 32)); r.OK {
		t.Fatalf("a wrong token: %+v", r)
	}
	r := call(tok)
	if !r.OK || !strings.Contains(string(r.Result), `"items"`) {
		t.Fatalf("with the token: %+v", r)
	}
}

// restart stops the Host and its Runner, as a server shutdown does, and starts a new Host and Runner on the same store.
func (e *env) restart() {
	e.t.Helper()
	e.host.Stop()
	e.cancel()
	_ = e.fx.Drain(time.Second)
	e.fx = effects.New(e.st, nil)
	e.wake.Store(e.fx)
	e.host = New(e.cfg, e.eng, e.fx, e.reg, e.log)
	e.host.restartDelay = func(int) time.Duration { return 0 }
	e.start()
}

// noResultFixture is a turn that starts and never ends: the message is echoed but no Result follows.
func noResultFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(claudeFixture(t, "simple-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "no-result.jsonl")
	first, _, _ := strings.Cut(string(raw), "\n")
	if err := os.WriteFile(p, []byte(first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func count(msgs []string, sub string) int {
	n := 0
	for _, m := range msgs {
		n += strings.Count(m, sub)
	}
	return n
}

func TestUnconfirmedSurviveRestart(t *testing.T) {
	e := newEnv(t)
	t.Setenv("ATHENA_FAKE_FIXTURE", noResultFixture(t))
	t.Setenv("ATHENA_FAKE_NO_ECHO", "1") // never taken into a turn
	tk := e.accept("github:issuecomment:1", "github:o/r#1")
	e.start()
	e.must(e.eng.FileReport(tk.ID, core.ReportInput{State: core.ReportBlocked, Summary: "need the deploy key"}))
	e.eventually("the relay", func() bool { return count(e.messages(), "need the deploy key") == 1 })
	e.eventually("the relay row to go", func() bool {
		rows, _ := e.fx.Pending(core.EffectRelay)
		return len(rows) == 0
	})
	e.restart()
	e.eventually("the resend", func() bool { return len(e.messages()) == 2 })
	time.Sleep(200 * time.Millisecond)
	msgs := e.messages()
	if len(msgs) != 2 || !strings.HasPrefix(msgs[1], recoveryNote) || count(msgs, "need the deploy key") != 2 {
		t.Fatalf("messages = %q, want the relay resent once", msgs)
	}
	// A further restart still finds it unconfirmed, and resends it once, without a second recovery note.
	e.restart()
	e.eventually("the second resend", func() bool { return len(e.messages()) == 3 })
	if msgs := e.messages(); strings.Count(msgs[2], recoveryNote) != 1 || count(msgs[2:], "need the deploy key") != 1 {
		t.Fatalf("second resend = %q", msgs[2])
	}
}

// An echo is matched by the uuid its message was sent with, not by its text: the CLI may reshape what it echoes, and
// another message's echo never takes a message of the same text.
func TestEchoMatchedByUUID(t *testing.T) {
	e := newEnv(t)
	u1, err := e.host.sent("m1")
	e.must(err)
	u2, err := e.host.sent("m2")
	e.must(err)
	if u1 == "" || u2 == "" || u1 == u2 {
		t.Fatalf("uuids %q %q, want two distinct", u1, u2)
	}
	e.host.observe(claude.UserEcho{Text: "m1 as the CLI reshaped it", UUID: u1})
	e.host.observe(claude.UserEcho{Text: "m2 reshaped", UUID: "another-message"})
	e.host.observe(claude.Result{SessionID: "s"})
	if got := e.host.pendingTexts(); !slices.Equal(got, []string{"m2"}) {
		t.Fatalf("unconfirmed = %q, want m1 taken by its uuid and m2 kept", got)
	}
}

// An echo whose uuid matches no message (a CLI that gives the echo a uuid of its own) still takes its message by text,
// so the message is not sent again on every start.
func TestEchoWithForeignUUIDMatchedByText(t *testing.T) {
	e := newEnv(t)
	_, err := e.host.sent("m1")
	e.must(err)
	_, err = e.host.sent("m2")
	e.must(err)
	e.host.observe(claude.UserEcho{Text: "m2", UUID: "a-uuid-of-the-cli"})
	e.host.observe(claude.Result{SessionID: "s"})
	if got := e.host.pendingTexts(); !slices.Equal(got, []string{"m1"}) {
		t.Fatalf("unconfirmed = %q, want m2 taken by its text", got)
	}
}

// A restart that is no crash sends again nothing the session already echoed: an echo in another shape still takes its
// message by uuid, and an echoed message whose turn the stop let finish is in the conversation.
func TestRestartResendsNothingEchoed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture func(*testing.T) string
	}{
		{"turn ended", func(t *testing.T) string { return claudeFixture(t, "simple-turn.jsonl") }},
		{"turn open at the stop", noResultFixture},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			t.Setenv("ATHENA_FAKE_FIXTURE", tc.fixture(t))
			t.Setenv("ATHENA_FAKE_ECHO_BLOCKS", "1")
			tk := e.accept("github:issuecomment:1", "github:o/r#1")
			e.start()
			e.must(e.eng.FileReport(tk.ID, core.ReportInput{State: core.ReportBlocked, Summary: "need the deploy key"}))
			e.eventually("the relay taken", func() bool {
				if count(e.messages(), "need the deploy key") != 1 {
					return false
				}
				e.host.unMu.Lock()
				defer e.host.unMu.Unlock()
				return len(e.host.unconfirmed) == 0 || (len(e.host.unconfirmed) == 1 && e.host.unconfirmed[0].Taken)
			})
			e.restart()
			e.eventually("the new session", func() bool { return len(e.invocations()) == 2 })
			time.Sleep(300 * time.Millisecond)
			if msgs := e.messages(); len(msgs) != 1 {
				t.Fatalf("messages = %q, want nothing resent", msgs)
			}
			if got := e.host.pendingTexts(); len(got) != 0 {
				t.Fatalf("unconfirmed after the restart = %q", got)
			}
		})
	}
}

func TestResultClearsOnlyTakenMessages(t *testing.T) {
	e := newEnv(t)
	e.host.sent("m1")
	e.host.sent("m2")
	e.host.observe(claude.UserEcho{Text: "m1"}) // an echo without a uuid falls back to its text
	e.host.observe(claude.Result{SessionID: "s"})
	if got := e.host.pendingTexts(); !slices.Equal(got, []string{"m2"}) {
		t.Fatalf("unconfirmed = %q, want m2 kept: it was not taken before the Result", got)
	}
	raw, ok, err := e.st.KVGet(unconfirmedKey)
	e.must(err)
	if !ok || !strings.Contains(raw, "m2") || strings.Contains(raw, `"m1"`) {
		t.Fatalf("kv %s = %s", unconfirmedKey, raw)
	}
	e.host.observe(claude.UserEcho{Text: "m2"})
	e.host.observe(claude.Result{SessionID: "s"})
	if got := e.host.pendingTexts(); len(got) != 0 {
		t.Fatalf("unconfirmed = %q, want none", got)
	}
}

func TestNoticesSurviveHostRestart(t *testing.T) {
	e := newEnv(t)
	tk := e.accept("github:issuecomment:1", "github:o/r#1")
	e.start()
	e.must(e.eng.Unsubscribe("github:o/r#1"))
	time.Sleep(100 * time.Millisecond)
	e.restart()
	e.must(e.eng.FileReport(tk.ID, core.ReportInput{State: core.ReportBlocked, Summary: "stuck"}))
	e.eventually("the relay", func() bool { return len(e.messages()) == 1 })
	if msg := e.messages()[0]; !strings.Contains(msg, core.NoticeThreadUnsubscribed) || !strings.Contains(msg, `"stuck"`) {
		t.Fatalf("message = %s, want the held notice and the relay", msg)
	}
}

func TestRecoveryResendNoDuplicate(t *testing.T) {
	e := newEnv(t)
	t.Setenv("ATHENA_FAKE_EXIT_MID_TURN_ONCE", filepath.Join(t.TempDir(), "crashed"))
	e.host.restartDelay = func(int) time.Duration { return 300 * time.Millisecond }
	tk := e.accept("github:issuecomment:1", "github:o/r#1")
	e.start()
	e.must(e.eng.FileReport(tk.ID, core.ReportInput{State: core.ReportBlocked, Summary: "first"}))
	e.eventually("the crash", func() bool {
		return len(e.invocations()) == 1 && count(e.messages(), `"first"`) == 1 && e.host.Health() != nil
	})
	// While the session restarts, the next relay waits for it.
	e.must(e.eng.FileReport(tk.ID, core.ReportInput{State: core.ReportFailed, Summary: "second"}))
	e.eventually("both after the restart", func() bool { return count(e.messages(), `"second"`) >= 1 && len(e.messages()) >= 3 })
	time.Sleep(200 * time.Millisecond)
	msgs := e.messages()
	if count(msgs, `"second"`) != 1 || count(msgs, `"first"`) != 2 || !strings.HasPrefix(msgs[1], recoveryNote) {
		t.Fatalf("messages = %q, want the resend first and each relay once more at most", msgs)
	}
}

// Minor 7: a message whose effect runs while the server stops (the Host stopped before the effects drained) is put
// off, not counted as a failed attempt, so repeated restarts never push the router's kinds into health.
func TestStoppedHostPutsMessagesOff(t *testing.T) {
	e := newEnv(t)
	tk := e.accept("github:issuecomment:1", "github:o/r#1")
	e.start()
	e.eventually("the session", func() bool { return e.host.Health() == nil })
	e.host.Stop()
	if err := e.host.deliver(context.Background(), "late"); !errors.Is(err, effects.ErrLater) {
		t.Fatalf("deliver after Stop = %v, want effects.Later", err)
	}
	e.must(e.eng.FileReport(tk.ID, core.ReportInput{State: core.ReportBlocked, Summary: "stuck at shutdown"}))
	time.Sleep(300 * time.Millisecond)
	rows, err := e.fx.Pending(core.EffectRelay)
	e.must(err)
	if len(rows) != 1 || rows[0].Attempts != 0 {
		t.Fatalf("relay rows = %+v, want one kept with no failed attempt", rows)
	}
}

func TestNewRouterSessionUsesOverride(t *testing.T) {
	e := newEnv(t)
	e.host.cfg.Prompt = func() string { return "ROUTER OVERRIDE" }
	e.start()
	e.eventually("the router session to start", func() bool { return len(e.invocations()) == 1 })
	if v, _ := flagValue(e.invocations()[0].Args, "--append-system-prompt"); v != "ROUTER OVERRIDE" {
		t.Fatalf("system prompt = %q, want the override", v)
	}
	if kept, _, _ := e.st.KVGet(promptKey); kept != "ROUTER OVERRIDE" {
		t.Fatalf("kept prompt = %q", kept)
	}
}

func TestResumedRouterSessionKeepsPrompt(t *testing.T) {
	e := newEnv(t)
	e.host.cfg.Prompt = func() string { return "NEW OVERRIDE" }
	e.must(e.st.KVSet(sessionKey, "sess-before"))
	e.must(e.st.KVSet(promptKey, "PROMPT IT STARTED WITH"))
	e.start()
	e.eventually("the router session to start", func() bool { return len(e.invocations()) == 1 })
	args := e.invocations()[0].Args
	if v, _ := flagValue(args, "--resume"); v != "sess-before" {
		t.Fatalf("args = %q, want a resume", args)
	}
	if v, _ := flagValue(args, "--append-system-prompt"); v != "PROMPT IT STARTED WITH" {
		t.Fatalf("system prompt = %q, want the one the session started with", v)
	}
}

// A resume with no kept prompt gets the current effective prompt and keeps it.
func TestResumedRouterSessionWithoutKeptPromptUsesCurrent(t *testing.T) {
	e := newEnv(t)
	e.host.cfg.Prompt = func() string { return "NEW OVERRIDE" }
	e.must(e.st.KVSet(sessionKey, "sess-before"))
	e.start()
	e.eventually("the router session to start", func() bool { return len(e.invocations()) == 1 })
	if v, _ := flagValue(e.invocations()[0].Args, "--append-system-prompt"); v != "NEW OVERRIDE" {
		t.Fatalf("system prompt = %q, want the current one", v)
	}
	if kept, _, _ := e.st.KVGet(promptKey); kept != "NEW OVERRIDE" {
		t.Fatalf("kept prompt = %q", kept)
	}
}
