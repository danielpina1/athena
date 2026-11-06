package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
	fakeDir  string
)

// fakeBinary builds testdata/fakeclaude once per test binary run.
func fakeBinary(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakeDir, fakeErr = os.MkdirTemp("", "fakeclaude")
		if fakeErr != nil {
			return
		}
		fakePath = filepath.Join(fakeDir, "claude")
		out, err := exec.Command("go", "build", "-o", fakePath, "./testdata/fakeclaude").CombinedOutput()
		if err != nil {
			fakeErr = &buildError{err, string(out)}
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakePath
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return "build fakeclaude: " + e.err.Error() + "\n" + e.out }

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeDir != "" {
		os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}

// withStubs fills in the settings and MCP config files Start requires.
func withStubs(t *testing.T, o Options) Options {
	t.Helper()
	dir := t.TempDir()
	if o.SettingsPath == "" {
		o.SettingsPath = filepath.Join(dir, "settings.json")
		if err := os.WriteFile(o.SettingsPath, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if o.MCPConfigPath == "" {
		o.MCPConfigPath = filepath.Join(dir, "mcp.json")
		if err := os.WriteFile(o.MCPConfigPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func fixtureAbs(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "claude", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func collect(t *testing.T, s *Session) []Event {
	t.Helper()
	var evs []Event
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-timeout:
			t.Fatal("timed out waiting for Events to close")
		}
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		l, err := r.ReadString('\n')
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, "\n"))
		}
		if err != nil {
			return out
		}
	}
}

func TestSessionAgainstFake(t *testing.T) {
	dir := t.TempDir()
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	s, err := Start(context.Background(), withStubs(t, Options{
		Binary: fakeBinary(t),
		Dir:    dir,
		Model:  "haiku",
		Env: []string{
			"ATHENA_FAKE_FIXTURE=" + fixtureAbs(t, "simple-turn.jsonl"),
			"ATHENA_FAKE_STDIN_LOG=" + stdinLog,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("make a file", ""); err != nil {
		t.Fatal(err)
	}

	// Read until the turn's Result, then close stdin like Athena does at the end.
	var evs []Event
	deadline := time.After(20 * time.Second)
	for done := false; !done; {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("events closed before the result")
			}
			evs = append(evs, ev)
			_, done = ev.(Result)
		case <-deadline:
			t.Fatal("no result")
		}
	}
	if err := s.Send("second", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	evs = append(evs, collect(t, s)...)
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if s.Killed() {
		t.Fatal("a cooperative process must not be killed")
	}

	var sawInit, sawWrite bool
	for _, ev := range evs {
		switch e := ev.(type) {
		case Init:
			sawInit = e.SessionID != ""
		case ToolUse:
			sawWrite = sawWrite || e.Name == "Write"
		}
	}
	if !sawInit || !sawWrite {
		t.Fatalf("events missing init/write: %d events", len(evs))
	}

	// Stdin: one stream-json user message per Send.
	got := lines(t, stdinLog)
	if len(got) != 2 {
		t.Fatalf("stdin lines = %d: %v", len(got), got)
	}
	var m struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(got[0]), &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "user" || m.Message.Role != "user" || m.Message.Content != "make a file" {
		t.Fatalf("stdin message = %s", got[0])
	}

	// Transcript: every raw stdout line, byte for byte, 0600 in a 0700 dir.
	want := lines(t, fixtureAbs(t, "simple-turn.jsonl"))
	tr := filepath.Join(dir, ".athena", "transcript.jsonl")
	if gotTr := lines(t, tr); !reflect.DeepEqual(gotTr, want) {
		t.Fatalf("transcript has %d lines, want %d", len(gotTr), len(want))
	}
	if st, _ := os.Stat(tr); st.Mode().Perm() != 0o600 {
		t.Fatalf("transcript mode = %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(dir, ".athena")); st.Mode().Perm() != 0o700 {
		t.Fatalf(".athena mode = %o", st.Mode().Perm())
	}

	if err := s.Send("late", ""); err == nil {
		t.Fatal("Send after exit must return an error")
	}
}

func TestSessionResumeArgs(t *testing.T) {
	argsLog := filepath.Join(t.TempDir(), "args.json")
	o := Options{
		Binary:             fakeBinary(t),
		Dir:                t.TempDir(),
		Model:              "haiku",
		ResumeID:           "sess-123",
		AppendSystemPrompt: "be brief",
		Env:                []string{"ATHENA_FAKE_ARGS_LOG=" + argsLog, "ATHENA_TICKET=tkt-9", "ATHENA_NAME=worker"},
	}
	o = withStubs(t, o)
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	_ = s.Wait()

	var rec struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env"`
	}
	b, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", "haiku", "--settings", o.SettingsPath, "--setting-sources", "",
		"--mcp-config", o.MCPConfigPath, "--strict-mcp-config", "--permission-mode", "default",
		"--resume", "sess-123", "--append-system-prompt", "be brief",
	}
	if !reflect.DeepEqual(rec.Args, want) {
		t.Fatalf("args =\n%q\nwant\n%q", rec.Args, want)
	}
	if rec.Env["ATHENA_TICKET"] != "tkt-9" || rec.Env["ATHENA_NAME"] != "worker" {
		t.Fatalf("env not passed through: %v", rec.Env)
	}
	if !reflect.DeepEqual(buildArgs(o), want) {
		t.Fatal("buildArgs disagrees with what the process received")
	}
}

func TestBuildArgsOmitsOptionalFlags(t *testing.T) {
	args := buildArgs(Options{Model: "haiku", SettingsPath: "s", MCPConfigPath: "m"})
	for _, flag := range []string{"--settings", "--mcp-config", "--setting-sources", "--strict-mcp-config"} {
		found := false
		for _, a := range args {
			found = found || a == flag
		}
		if !found {
			t.Fatalf("%s missing from %q", flag, args)
		}
	}
	for _, a := range args {
		if a == "--resume" || a == "--append-system-prompt" {
			t.Fatalf("unexpected %s in %q", a, args)
		}
	}
	// Sessions never bypass permissions.
	for i, a := range args {
		if a == "--permission-mode" && args[i+1] != "default" {
			t.Fatalf("permission mode = %q", args[i+1])
		}
		if a == "--dangerously-skip-permissions" {
			t.Fatal("bypass flag present")
		}
	}
}

func TestCloseKillsAfterTimeout(t *testing.T) {
	s, err := Start(context.Background(), withStubs(t, Options{
		Binary:     fakeBinary(t),
		Dir:        t.TempDir(),
		CloseGrace: 300 * time.Millisecond,
		Env:        []string{"ATHENA_FAKE_IGNORE_STDIN_CLOSE=1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 5*time.Second {
		t.Fatalf("Close took %v, want ~300ms", d)
	}
	if !s.Killed() {
		t.Fatal("expected the process to be killed")
	}
	collect(t, s)
	if err := s.Wait(); err == nil {
		t.Fatal("Wait should report the kill")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestExitMidTurnClosesEvents(t *testing.T) {
	s, err := Start(context.Background(), withStubs(t, Options{
		Binary: fakeBinary(t),
		Dir:    t.TempDir(),
		Env: []string{
			"ATHENA_FAKE_FIXTURE=" + fixtureAbs(t, "simple-turn.jsonl"),
			"ATHENA_FAKE_EXIT_MID_TURN=1",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("go", ""); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, s)
	for _, ev := range evs {
		if _, ok := ev.(Result); ok {
			t.Fatal("process exited mid-turn; there must be no Result")
		}
	}
	if len(evs) == 0 {
		t.Fatal("expected the events emitted before the exit")
	}
	err = s.Wait()
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("Wait = %v, want exit status 3", err)
	}
	if err := s.Send("again", ""); err == nil {
		t.Fatal("Send after exit must return an error")
	}
}

func TestReaderNeverBlocksOnUndrainedEvents(t *testing.T) {
	dir := t.TempDir()
	s, err := Start(context.Background(), withStubs(t, Options{
		Binary: fakeBinary(t),
		Dir:    dir,
		Env: []string{
			"ATHENA_FAKE_FIXTURE=" + fixtureAbs(t, "simple-turn.jsonl"),
			"ATHENA_FAKE_REPEAT=500",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("go", ""); err != nil {
		t.Fatal(err)
	}
	// Nobody reads Events. The process must still run to completion and every
	// line must still reach the transcript.
	want := 500 * len(lines(t, fixtureAbs(t, "simple-turn.jsonl")))
	tr := filepath.Join(dir, ".athena", "transcript.jsonl")
	deadline := time.Now().Add(20 * time.Second)
	for len(lines(t, tr)) < want {
		if time.Now().After(deadline) {
			t.Fatalf("transcript stalled at %d of %d lines", len(lines(t, tr)), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	// The channel still closes even though most events were never consumed.
	timeout := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-s.Events():
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("Events never closed")
		}
	}
}

func TestInterruptSendsSIGINT(t *testing.T) {
	s, err := Start(context.Background(), withStubs(t, Options{Binary: fakeBinary(t), Dir: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Interrupt(); err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	err = s.Wait()
	if err == nil || !strings.Contains(err.Error(), "interrupt") {
		t.Fatalf("Wait = %v, want a SIGINT exit", err)
	}
	if err := s.Interrupt(); err == nil {
		t.Fatal("Interrupt after exit must return an error")
	}
}

func TestInterruptTurnWritesControlRequest(t *testing.T) {
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	s, err := Start(context.Background(), withStubs(t, Options{
		Binary: fakeBinary(t),
		Dir:    t.TempDir(),
		Env:    []string{"ATHENA_FAKE_STDIN_LOG=" + stdinLog},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InterruptTurn(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	collect(t, s)
	got := lines(t, stdinLog)
	if len(got) != 1 {
		t.Fatalf("stdin = %v", got)
	}
	var m struct {
		Type    string `json:"type"`
		Request struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(got[0]), &m); err != nil || m.Type != "control_request" || m.Request.Subtype != "interrupt" {
		t.Fatalf("stdin = %s (%v)", got[0], err)
	}
}

func TestStartErrors(t *testing.T) {
	if _, err := Start(context.Background(), Options{Binary: fakeBinary(t)}); err == nil {
		t.Fatal("Start without Dir must fail")
	}
	if _, err := Start(context.Background(), Options{Binary: "/nonexistent/claude", Dir: t.TempDir()}); err == nil {
		t.Fatal("Start with a missing binary must fail")
	}
}

func TestContextCancelKillsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, withStubs(t, Options{Binary: fakeBinary(t), Dir: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	collect(t, s)
	if err := s.Wait(); err == nil {
		t.Fatal("Wait should report the kill")
	}
}

func TestStartRequiresSettingsAndMCPConfig(t *testing.T) {
	good := withStubs(t, Options{Binary: fakeBinary(t), Dir: t.TempDir()})
	cases := map[string]func(o *Options){
		"no settings":       func(o *Options) { o.SettingsPath = "" },
		"no mcp config":     func(o *Options) { o.MCPConfigPath = "" },
		"missing settings":  func(o *Options) { o.SettingsPath = filepath.Join(t.TempDir(), "nope.json") },
		"missing mcp":       func(o *Options) { o.MCPConfigPath = filepath.Join(t.TempDir(), "nope.json") },
		"settings is a dir": func(o *Options) { o.SettingsPath = t.TempDir() },
	}
	for name, mutate := range cases {
		o := good
		o.Dir = t.TempDir()
		mutate(&o)
		s, err := Start(context.Background(), o)
		if err == nil {
			s.Close()
			t.Errorf("%s: Start must fail", name)
		}
		if _, statErr := os.Stat(filepath.Join(o.Dir, ".athena")); statErr == nil {
			t.Errorf("%s: Start must fail before creating anything in Dir", name)
		}
	}
	// And the valid options still start.
	s, err := Start(context.Background(), good)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	collect(t, s)
}

// SendMessage writes the message's uuid; with --replay-user-messages the echo carries it back.
func TestSendMessageUUIDEchoed(t *testing.T) {
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	s, err := Start(context.Background(), withStubs(t, Options{
		Binary: fakeBinary(t), Dir: t.TempDir(), Model: "haiku", ReplayUserMessages: true,
		Env: []string{"ATHENA_FAKE_STDIN_LOG=" + stdinLog},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SendMessage("hello", "0b9f3c3e-2f43-4c1e-9d1a-1f2e3d4c5b6a"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("events closed before the echo")
			}
			if e, ok := ev.(UserEcho); ok {
				if e != (UserEcho{Text: "hello", UUID: "0b9f3c3e-2f43-4c1e-9d1a-1f2e3d4c5b6a"}) {
					t.Fatalf("echo = %+v", e)
				}
				_ = s.Close()
				raw, _ := os.ReadFile(stdinLog)
				if !strings.Contains(string(raw), `"uuid":"0b9f3c3e-2f43-4c1e-9d1a-1f2e3d4c5b6a"`) {
					t.Fatalf("stdin = %s, want the uuid", raw)
				}
				return
			}
		case <-deadline:
			t.Fatal("no echo")
		}
	}
}

func TestCloseDoesNotHangOnBlockedSend(t *testing.T) {
	s, err := Start(context.Background(), withStubs(t, Options{
		Binary:     fakeBinary(t),
		Dir:        t.TempDir(),
		CloseGrace: 300 * time.Millisecond,
		Env:        []string{"ATHENA_FAKE_NO_READ_STDIN=1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	sendErr := make(chan error, 1)
	go func() { sendErr <- s.Send(strings.Repeat("x", 4<<20), "") }() // far more than a pipe buffer
	select {
	case err := <-sendErr:
		t.Fatalf("Send returned early (%v); the child should not be reading", err)
	case <-time.After(300 * time.Millisecond):
	}

	closed := make(chan error, 1)
	start := time.Now()
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung behind a Send blocked on a full stdin pipe")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close took %v, want about the 300ms grace", d)
	}
	if !s.Killed() {
		t.Fatal("a child that never reads stdin must be killed after the grace")
	}
	select {
	case err := <-sendErr:
		if err == nil {
			t.Fatal("the blocked Send must fail once the session is closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked Send never returned")
	}
	// InterruptTurn must not stall either.
	if err := s.InterruptTurn(); err == nil {
		t.Fatal("InterruptTurn after Close must fail")
	}
	collect(t, s)
}

func TestTranscriptPathAppendsAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(t.TempDir(), "transcripts", "T-0001.jsonl")
	for i := 0; i < 2; i++ {
		s, err := Start(context.Background(), withStubs(t, Options{
			Binary:         fakeBinary(t),
			Dir:            dir,
			TranscriptPath: tr,
			Env:            []string{"ATHENA_FAKE_FIXTURE=" + fixtureAbs(t, "simple-turn.jsonl")},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Send("go", ""); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		collect(t, s)
		_ = s.Wait()
	}
	fixture := lines(t, fixtureAbs(t, "simple-turn.jsonl"))
	if got := lines(t, tr); !reflect.DeepEqual(got, append(append([]string{}, fixture...), fixture...)) {
		t.Fatalf("transcript has %d lines, want %d (both sessions appended)", len(got), 2*len(fixture))
	}
	if st, _ := os.Stat(tr); st.Mode().Perm() != 0o600 {
		t.Fatalf("transcript mode = %o", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Dir(tr)); st.Mode().Perm() != 0o700 {
		t.Fatalf("transcript dir mode = %o", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, ".athena", "transcript.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("a transcript was also written in the working dir: %v", err)
	}
}

// TestOnLineOffsetsMatchFile: OnLine gets each line with the byte offset it starts at in the transcript, also when
// the file already held lines from an earlier session.
func TestOnLineOffsetsMatchFile(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(t.TempDir(), "transcripts", "T-0001.jsonl")
	for i := 0; i < 2; i++ {
		type got struct {
			off  int64
			line string
		}
		var mu sync.Mutex
		var seen []got
		s, err := Start(context.Background(), withStubs(t, Options{
			Binary:         fakeBinary(t),
			Dir:            dir,
			TranscriptPath: tr,
			Env:            []string{"ATHENA_FAKE_FIXTURE=" + fixtureAbs(t, "simple-turn.jsonl")},
			OnLine: func(off int64, line []byte) {
				mu.Lock()
				seen = append(seen, got{off, string(line)})
				mu.Unlock()
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Send("go", ""); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		collect(t, s)
		_ = s.Wait()
		file, err := os.ReadFile(tr)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		if len(seen) == 0 {
			t.Fatal("OnLine was never called")
		}
		for _, g := range seen {
			end := int(g.off) + len(g.line)
			if int(g.off) > len(file) || end >= len(file) || string(file[g.off:end]) != g.line || file[end] != '\n' {
				t.Fatalf("session %d: OnLine(%d, %.40q) does not match the file", i, g.off, g.line)
			}
		}
		if i == 1 && seen[0].off == 0 {
			t.Fatal("the second session's first offset is 0: it ignored what the file held")
		}
		mu.Unlock()
	}
}

// TestChildEnvironmentIsAllowlisted: a worker session sees only the basic process variables, the locale, XDG,
// Claude Code's and Anthropic's own variables (its credentials among them), Athena's variables without secret-looking
// names, and Options.Env; never the server's other variables, such as its tokens and cloud keys.
func TestChildEnvironmentIsAllowlisted(t *testing.T) {
	envLog := filepath.Join(t.TempDir(), "env.log")
	t.Setenv("GITHUB_TOKEN", "ghp_parent")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws_parent")
	t.Setenv("FOO", "bar")
	t.Setenv("LC_ALL", "C")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-parent")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-parent")
	t.Setenv("ATHENA_FROM_PARENT", "kept")
	t.Setenv("ATHENA_TOKEN", "dropped")
	t.Setenv("ATHENA_FAKE_ENV_LOG", envLog)
	s, err := Start(context.Background(), withStubs(t, Options{
		Binary: fakeBinary(t),
		Dir:    t.TempDir(),
		Env:    []string{"ATHENA_TICKET=T-7", "ATHENA_SOCKET=/tmp/n.sock"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	collect(t, s)
	_ = s.Wait()
	env := map[string]string{}
	for _, l := range lines(t, envLog) {
		if k, v, ok := strings.Cut(l, "="); ok {
			env[k] = v
		}
	}
	for _, k := range []string{"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "FOO", "ATHENA_TOKEN"} {
		if v, ok := env[k]; ok {
			t.Errorf("child has %s=%q", k, v)
		}
	}
	for k, want := range map[string]string{
		"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME"), "LC_ALL": "C", "XDG_CONFIG_HOME": "/xdg",
		"ANTHROPIC_API_KEY": "sk-ant-parent", "CLAUDE_CODE_OAUTH_TOKEN": "oauth-parent",
		"ATHENA_FROM_PARENT": "kept", "ATHENA_TICKET": "T-7", "ATHENA_SOCKET": "/tmp/n.sock",
	} {
		if got, ok := env[k]; !ok || got != want {
			t.Errorf("child %s = %q (present %v), want %q", k, got, ok, want)
		}
	}
}

// TestFakeEnvLogPerTicket: fakeclaude puts each Ticket's environment in its own file when the log path names
// {ticket}, so a test driving several sessions (the dashboard e2e's sessionctl) finds the right one.
func TestFakeEnvLogPerTicket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ATHENA_FAKE_ENV_LOG", filepath.Join(dir, "env-{ticket}.log"))
	s, err := Start(context.Background(), withStubs(t, Options{
		Binary: fakeBinary(t),
		Dir:    t.TempDir(),
		Env:    []string{"ATHENA_TICKET=T-7"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	collect(t, s)
	_ = s.Wait()
	found := false
	for _, l := range lines(t, filepath.Join(dir, "env-T-7.log")) {
		found = found || l == "ATHENA_TICKET=T-7"
	}
	if !found {
		t.Fatal("env-T-7.log lacks ATHENA_TICKET=T-7")
	}
}

func TestBuildArgsTools(t *testing.T) {
	none := ""
	args := buildArgs(Options{SettingsPath: "s", MCPConfigPath: "m", Tools: &none})
	i := slices.Index(args, "--tools")
	if i < 0 || i+1 >= len(args) || args[i+1] != "" {
		t.Fatalf("args = %q, want --tools \"\"", args)
	}
	if slices.Contains(buildArgs(Options{SettingsPath: "s", MCPConfigPath: "m"}), "--tools") {
		t.Fatal("--tools passed without Options.Tools")
	}
}

func TestBuildArgsReplayUserMessages(t *testing.T) {
	if !slices.Contains(buildArgs(Options{SettingsPath: "s", MCPConfigPath: "m", ReplayUserMessages: true}), "--replay-user-messages") {
		t.Fatal("--replay-user-messages missing")
	}
	if slices.Contains(buildArgs(Options{SettingsPath: "s", MCPConfigPath: "m"}), "--replay-user-messages") {
		t.Fatal("--replay-user-messages passed unasked")
	}
}
