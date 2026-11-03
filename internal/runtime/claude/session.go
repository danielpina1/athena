package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultCloseGrace = 10 * time.Second
	// drainGrace bounds how long the reader keeps reading stdout after the
	// process has exited (a stray grandchild may hold the pipe open).
	drainGrace = 2 * time.Second
	// abandonAfter is how long, once Close has returned, the event pump waits
	// for a consumer before it concludes nobody is reading and drops the rest.
	abandonAfter = 250 * time.Millisecond
	eventBuffer  = 64
	stderrTail   = 4096
)

// ErrExited is returned by Send and Interrupt once the process has exited.
var ErrExited = errors.New("claude: process has exited")

// Options configures one Claude Code session.
type Options struct {
	Binary             string // default "claude"
	Dir                string // working directory; the transcript lives in <Dir>/.athena
	Model              string
	ResumeID           string
	SettingsPath       string // --settings; the only settings source Athena allows
	MCPConfigPath      string // --mcp-config; only these servers are loaded
	AppendSystemPrompt string
	// Env is added to the parent environment of the child process.
	Env []string
	// CloseGrace is how long Close waits after closing stdin before it kills
	// the process. Default 10s.
	CloseGrace time.Duration
}

// buildArgs returns the argument list for o.
//
// --setting-sources "" keeps user, project and local settings out of the
// session, so a repo's .claude/settings.json or a user defaultMode cannot
// change Athena's hooks or permissions; only --settings applies.
// --permission-mode default is passed explicitly and bypass modes are never used.
func buildArgs(o Options) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.SettingsPath != "" {
		args = append(args, "--settings", o.SettingsPath)
	}
	args = append(args, "--setting-sources", "")
	if o.MCPConfigPath != "" {
		args = append(args, "--mcp-config", o.MCPConfigPath)
	}
	args = append(args, "--strict-mcp-config", "--permission-mode", "default")
	if o.ResumeID != "" {
		args = append(args, "--resume", o.ResumeID)
	}
	if o.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", o.AppendSystemPrompt)
	}
	return args
}

// Session is a running Claude Code process. All methods are safe for
// concurrent use.
type Session struct {
	cmd        *exec.Cmd
	stdout     *os.File // read end of the stdout pipe
	closeGrace time.Duration

	stdinMu     sync.Mutex
	stdin       io.WriteCloser
	stdinClosed bool
	reqSeq      int

	events chan Event

	qmu        sync.Mutex
	queue      []Event
	readerDone bool
	notify     chan struct{} // cap 1: queue changed
	closedCh   chan struct{} // closed once Close has returned
	closeOnce  sync.Once
	closeMu    sync.Mutex
	exited     chan struct{} // closed after cmd.Wait returned
	done       chan struct{} // closed after exit and after Events closed
	waitErr    error
	killed     atomic.Bool
	transcript *os.File
	stderr     *tailBuffer
}

// Start launches the process. The child's environment is the parent's plus o.Env.
func Start(ctx context.Context, o Options) (*Session, error) {
	if o.Dir == "" {
		return nil, errors.New("claude: Options.Dir is required")
	}
	if o.Binary == "" {
		o.Binary = "claude"
	}
	if o.CloseGrace <= 0 {
		o.CloseGrace = defaultCloseGrace
	}

	adir := filepath.Join(o.Dir, ".athena")
	if err := os.MkdirAll(adir, 0o700); err != nil {
		return nil, fmt.Errorf("claude: create %s: %w", adir, err)
	}
	if err := os.Chmod(adir, 0o700); err != nil {
		return nil, fmt.Errorf("claude: chmod %s: %w", adir, err)
	}
	tr, err := os.OpenFile(filepath.Join(adir, "transcript.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("claude: open transcript: %w", err)
	}

	s := &Session{
		closeGrace: o.CloseGrace,
		events:     make(chan Event, eventBuffer),
		notify:     make(chan struct{}, 1),
		closedCh:   make(chan struct{}),
		exited:     make(chan struct{}),
		done:       make(chan struct{}),
		transcript: tr,
		stderr:     &tailBuffer{max: stderrTail},
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		tr.Close()
		return nil, err
	}
	cmd := exec.CommandContext(ctx, o.Binary, buildArgs(o)...)
	s.cmd = cmd
	cmd.Dir = o.Dir
	cmd.Env = append(os.Environ(), o.Env...)
	cmd.Stdout = pw
	cmd.Stderr = s.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own group: Ctrl-C and kills stay scoped
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		s.killed.Store(true)
		return s.killGroup()
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		pr.Close()
		pw.Close()
		tr.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		tr.Close()
		return nil, fmt.Errorf("claude: start %s: %w", o.Binary, err)
	}
	pw.Close() // the child holds the write end now
	s.stdin, s.stdout = stdin, pr

	go s.wait()
	go s.readLoop()
	pumpDone := make(chan struct{})
	go func() { s.pump(); close(pumpDone) }()
	go func() {
		<-s.exited
		<-pumpDone
		close(s.done)
	}()
	return s, nil
}

func (s *Session) wait() {
	err := s.cmd.Wait()
	s.waitErr = err
	close(s.exited)
	_ = s.stdout.SetReadDeadline(time.Now().Add(drainGrace))
}

// readLoop copies stdout lines to the transcript and the event queue. It never
// waits on the consumer. Raw lines are never logged: they may contain secrets.
func (s *Session) readLoop() {
	defer func() {
		s.stdout.Close()
		s.transcript.Close()
		s.qmu.Lock()
		s.readerDone = true
		s.qmu.Unlock()
		s.wake()
	}()
	r := bufio.NewReaderSize(s.stdout, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			s.handleLine(line)
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) handleLine(line []byte) {
	_, _ = s.transcript.Write(line)
	if line[len(line)-1] != '\n' {
		_, _ = s.transcript.Write([]byte{'\n'})
	}
	evs, err := ParseEvents(line)
	if err != nil {
		evs = []Event{Unknown{Type: "invalid", Raw: json.RawMessage("null")}}
	}
	s.qmu.Lock()
	s.queue = append(s.queue, evs...)
	s.qmu.Unlock()
	s.wake()
}

func (s *Session) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// pump delivers queued events to the Events channel and closes it once the
// reader is done and the queue is empty.
func (s *Session) pump() {
	defer close(s.events)
	abandoned := false
	for {
		s.qmu.Lock()
		var ev Event
		have := len(s.queue) > 0
		if have {
			ev = s.queue[0]
			s.queue[0] = nil
			s.queue = s.queue[1:]
		}
		finished := s.readerDone && !have
		s.qmu.Unlock()
		if finished {
			return
		}
		if !have {
			<-s.notify
			continue
		}
		if abandoned {
			continue
		}
		select {
		case s.events <- ev:
			continue
		case <-s.closedCh:
		}
		// Close has returned: keep delivering to a consumer that is still
		// draining, but give up on one that has gone away.
		t := time.NewTimer(abandonAfter)
		select {
		case s.events <- ev:
			t.Stop()
		case <-t.C:
			abandoned = true
		}
	}
}

// Events returns the session's events in order. The channel is buffered, but
// the consumer must drain it: events beyond the buffer queue in memory (the
// stdout reader and the transcript never wait for the consumer). It is closed
// when the process has exited and everything it printed was delivered. After
// Close returns, a consumer that stops reading for a moment loses the
// undelivered rest; the transcript still has every line.
func (s *Session) Events() <-chan Event { return s.events }

// Send writes one user message to the session. A message sent while a turn is
// running is folded into that turn. After the process has exited it returns an
// error.
func (s *Session) Send(text string) error {
	line, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	if err != nil {
		return err
	}
	return s.writeLine(line)
}

// InterruptTurn asks the session to abandon the running turn and keep the
// process alive for the next message (stream-json control request). The turn
// ends with a Result of subtype error_during_execution.
func (s *Session) InterruptTurn() error {
	s.stdinMu.Lock()
	s.reqSeq++
	id := fmt.Sprintf("athena-%d", s.reqSeq)
	s.stdinMu.Unlock()
	line, err := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": id,
		"request":    map[string]any{"subtype": "interrupt"},
	})
	if err != nil {
		return err
	}
	return s.writeLine(line)
}

func (s *Session) writeLine(line []byte) error {
	select {
	case <-s.exited:
		return ErrExited
	default:
	}
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	if s.stdinClosed {
		return errors.New("claude: session input is closed")
	}
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		select {
		case <-s.exited:
			return ErrExited
		default:
		}
		return fmt.Errorf("claude: write to session: %w", err)
	}
	return nil
}

// Interrupt sends SIGINT to the process. Claude Code treats that as the end of
// the session (it prints an error Result and exits); use InterruptTurn to stop
// only the current turn.
func (s *Session) Interrupt() error {
	select {
	case <-s.exited:
		return ErrExited
	default:
	}
	if err := s.cmd.Process.Signal(syscall.SIGINT); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return ErrExited
		}
		return err
	}
	return nil
}

// Close closes the session's stdin, waits up to the close grace for the
// process to exit, then kills it (and its process group). It is idempotent.
func (s *Session) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	s.stdinMu.Lock()
	if !s.stdinClosed {
		s.stdinClosed = true
		_ = s.stdin.Close()
	}
	s.stdinMu.Unlock()

	t := time.NewTimer(s.closeGrace)
	defer t.Stop()
	select {
	case <-s.exited:
	case <-t.C:
		s.killed.Store(true)
		_ = s.killGroup()
		<-s.exited
	}
	s.closeOnce.Do(func() { close(s.closedCh) })
	return nil
}

// Killed reports whether Athena had to kill the process (Close timed out or
// the context was cancelled) rather than the process exiting on its own.
func (s *Session) Killed() bool { return s.killed.Load() }

// Wait blocks until the process has exited and Events is closed, and returns
// the process's exit error (nil on a clean exit).
func (s *Session) Wait() error {
	<-s.done
	return s.waitErr
}

// Stderr returns the last few KB of the process's stderr. It may contain
// anything the CLI printed; do not log it unfiltered.
func (s *Session) Stderr() string { return s.stderr.String() }

func (s *Session) killGroup() error {
	select {
	case <-s.exited:
		return os.ErrProcessDone
	default:
	}
	pid := s.cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return s.cmd.Process.Kill()
	}
	return nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
