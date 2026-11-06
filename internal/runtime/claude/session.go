package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/danielpina1/athena/internal/runtime/rt"
)

// ErrExited is returned by Send and Interrupt once the process has exited. It is rt.ErrExited.
var ErrExited = rt.ErrExited

// Options configures one Claude Code session.
type Options struct {
	Binary             string // default "claude"
	Dir                string // working directory; the transcript lives in <Dir>/.athena unless TranscriptPath is set
	Model              string
	ResumeID           string
	SettingsPath       string // --settings; the only settings source Athena allows
	MCPConfigPath      string // --mcp-config; only these servers are loaded
	AppendSystemPrompt string
	// Env is added to the child's environment, which is otherwise only the allowlisted part of the parent's
	// (childEnv).
	Env []string
	// CloseGrace is how long Close waits after closing stdin before it kills
	// the process. Default 10s.
	CloseGrace time.Duration
	// Transcript, when set, receives every stdout line instead of a file the session opens itself (TranscriptPath and
	// OnLine are then unused); the caller closes it after Wait.
	Transcript *rt.Transcript
	// TranscriptPath, when set, is the file every stdout line is appended to instead of <Dir>/.athena/transcript.jsonl.
	// Its directory is created 0700 and the file 0600.
	TranscriptPath string
	// OnLine, when set, is called for each stdout line after it is written to the transcript, on the reader goroutine
	// (it must not block): offset is the byte offset the line starts at in the transcript file, counting what the file
	// held before this session appended; line is the line without its newline, and is the callee's to keep. Each
	// line takes len(line)+1 bytes in the file.
	OnLine func(offset int64, line []byte)
	// Tools, when set, is passed as --tools: the built-in tools the session has. A pointer to "" disables every
	// built-in tool (`--tools ""`, checked against Claude Code 2.1.280: its init lists no tools, MCP tools stay).
	Tools *string
	// ReplayUserMessages passes --replay-user-messages: each user message comes back as a UserEcho once a turn takes
	// it in.
	ReplayUserMessages bool
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
	if o.Tools != nil {
		args = append(args, "--tools", *o.Tools)
	}
	if o.ReplayUserMessages {
		args = append(args, "--replay-user-messages")
	}
	// Always passed: with --setting-sources "" the --settings file is the only
	// source of hooks, so a session without it would run without Athena's hooks.
	args = append(args, "--settings", o.SettingsPath, "--setting-sources", "",
		"--mcp-config", o.MCPConfigPath, "--strict-mcp-config", "--permission-mode", "default")
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
	proc *rt.Proc

	// stdinMu serialises writers only; it is never taken by Close, because a
	// writer can block on a full pipe while holding it.
	stdinMu     sync.Mutex
	stdinClosed atomic.Bool
	reqSeq      atomic.Int64

	// events carries the session's events for Events; rtEvents the same translated for the node (Runtime.Start). A
	// session fills one of them: push queues a line's events for it, finish marks the reader done, pump delivers.
	events   <-chan Event
	rtEvents <-chan rt.Event
	push     func([]Event)
	finish   func()
	pump     func()

	closedCh  chan struct{} // closed once Close has returned
	closeOnce sync.Once
	closeMu   sync.Mutex
	done      chan struct{} // closed after exit and after Events closed

	transcript    *rt.Transcript
	ownTranscript bool // the session opened the transcript and closes it
}

// Start launches the process. The child's environment is the allowlisted part of the parent's plus o.Env.
func Start(ctx context.Context, o Options) (*Session, error) { return start(ctx, o, false) }

// start launches the process; forRT makes the session deliver its events translated for the node (rtEvents).
func start(ctx context.Context, o Options, forRT bool) (*Session, error) {
	if o.Dir == "" {
		return nil, errors.New("claude: Options.Dir is required")
	}
	// Hooks fail closed: never start a session that would run without
	// Athena's hooks or with MCP servers from anywhere but Athena's config.
	for _, f := range []struct{ name, path string }{
		{"SettingsPath", o.SettingsPath}, {"MCPConfigPath", o.MCPConfigPath},
	} {
		if f.path == "" {
			return nil, fmt.Errorf("claude: Options.%s is required", f.name)
		}
		if st, err := os.Stat(f.path); err != nil || st.IsDir() {
			return nil, fmt.Errorf("claude: Options.%s %q is not a readable file", f.name, f.path)
		}
	}
	if o.Binary == "" {
		o.Binary = "claude"
	}

	adir := filepath.Join(o.Dir, ".athena")
	if err := rt.PrivateDir(adir); err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	s := &Session{
		closedCh:   make(chan struct{}),
		done:       make(chan struct{}),
		transcript: o.Transcript,
	}
	if s.transcript == nil {
		trPath := o.TranscriptPath
		if trPath == "" {
			trPath = filepath.Join(adir, "transcript.jsonl")
		}
		tr, err := rt.OpenTranscript(trPath, o.OnLine)
		if err != nil {
			return nil, fmt.Errorf("claude: %w", err)
		}
		s.transcript, s.ownTranscript = tr, true
	}
	if forRT {
		q := rt.NewQueue[rt.Event]()
		s.rtEvents = q.Out()
		// push runs on the stdout reader alone, which owns limits.
		var limits limitWatch
		s.push = func(evs []Event) {
			out := make([]rt.Event, len(evs))
			for i, ev := range evs {
				out[i] = limits.translate(ev)
			}
			q.Push(out...)
		}
		s.finish, s.pump = q.Finish, func() { q.Pump(s.closedCh) }
	} else {
		q := rt.NewQueue[Event]()
		s.events = q.Out()
		s.push = func(evs []Event) { q.Push(evs...) }
		s.finish, s.pump = q.Finish, func() { q.Pump(s.closedCh) }
	}

	p, err := rt.StartProc(ctx, rt.ProcOptions{
		Binary:     o.Binary,
		Args:       buildArgs(o),
		Dir:        o.Dir,
		Env:        childEnv(os.Environ(), o.Env),
		CloseGrace: o.CloseGrace,
	})
	if err != nil {
		if s.ownTranscript {
			s.transcript.Close()
		}
		return nil, fmt.Errorf("claude: %w", err)
	}
	s.proc = p

	go s.readLoop()
	pumpDone := make(chan struct{})
	go func() { s.pump(); close(pumpDone) }()
	go func() {
		<-p.Exited()
		<-pumpDone
		close(s.done)
	}()
	return s, nil
}

// limitText matches what a failed turn's result says when Claude Code stopped it at a usage limit.
var limitText = regexp.MustCompile(`(?i)usage limit|rate limit|hit your limit|limit reached`)

// limitWatch translates a session's events for the node (toRT), and sets the Limit of the TurnEnd of a turn that
// failed at its usage limit: one that saw a rate_limit_event with status "rejected" (resetting when that event says,
// whichever window it names), or whose result is an error that says so (limitText; no reset known). A turn that ended
// well is not limited, and a rejection does not outlive its turn. One watch serves one session's events, in order.
type limitWatch struct{ rejected *rt.LimitHit }

func (w *limitWatch) translate(ev Event) rt.Event {
	out := toRT(ev)
	switch e := ev.(type) {
	case Unknown:
		if e.Type == "rate_limit_event" {
			if h, ok := rejectedLimit(e.Raw); ok {
				w.rejected = &h
			}
		}
	case Result:
		te, ok := out.(rt.TurnEnd)
		if !ok {
			return out
		}
		hit := w.rejected
		w.rejected = nil
		if te.OK {
			return te
		}
		why := strings.TrimSpace(strings.Join(append([]string{e.Text}, e.Errors...), "\n"))
		if hit == nil && limitText.MatchString(why) {
			hit = &rt.LimitHit{Runtime: runtimeName}
		}
		if hit != nil {
			hit.Message = why
			te.Limit = hit
		}
		return te
	}
	return out
}

// rejectedLimit reads a rate_limit_event whose rate_limit_info.status is "rejected": a limit of whatever window
// rateLimitType names, even one parseRateLimit does not map, resetting at its resetsAt (or its window's in
// unifiedWindows; nil when neither says).
func rejectedLimit(line []byte) (rt.LimitHit, bool) {
	var ev struct {
		Info *struct {
			Status         string `json:"status"`
			ResetsAt       int64  `json:"resetsAt"`
			RateLimitType  string `json:"rateLimitType"`
			UnifiedWindows map[string]struct {
				ResetsAt int64 `json:"resetsAt"`
			} `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Info == nil || ev.Info.Status != "rejected" {
		return rt.LimitHit{}, false
	}
	h := rt.LimitHit{Runtime: runtimeName}
	reset := ev.Info.ResetsAt
	if reset == 0 {
		reset = ev.Info.UnifiedWindows[ev.Info.RateLimitType].ResetsAt
	}
	if reset > 0 {
		at := time.Unix(reset, 0)
		h.ResetsAt = &at
	}
	return h, true
}

// childEnv is a session's environment (rt.ChildEnv): Claude Code's own CLAUDE_ and ANTHROPIC_ variables are kept
// whatever their name, since they carry the credentials the session needs.
func childEnv(parent, extra []string) []string {
	return rt.ChildEnv(parent, extra, "CLAUDE_", "ANTHROPIC_")
}

// readLoop copies stdout lines to the transcript and the event queue. It never
// waits on the consumer. Raw lines are never logged: they may contain secrets.
func (s *Session) readLoop() {
	stdout := s.proc.Stdout()
	defer func() {
		stdout.Close()
		if s.ownTranscript {
			s.transcript.Close()
		}
		s.finish()
	}()
	r := bufio.NewReaderSize(stdout, 64*1024)
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
	s.transcript.Append(line)
	evs, err := ParseEvents(line)
	if err != nil {
		evs = []Event{Unknown{Type: "invalid", Raw: json.RawMessage("null")}}
	}
	s.push(evs)
}

// Events returns the session's events in order. The channel is buffered, but
// the consumer must drain it: events beyond the buffer queue in memory (the
// stdout reader and the transcript never wait for the consumer). It is closed
// when the process has exited and everything it printed was delivered. After
// Close returns, a consumer that stops reading for a moment loses the
// undelivered rest; the transcript still has every line.
func (s *Session) Events() <-chan Event { return s.events }

// Send writes one user message to the session. A message sent while a turn is
// running is folded into that turn. uuid, when not empty, is the message's uuid:
// with --replay-user-messages, the echo of the message carries it back
// (UserEcho.UUID), so the message is known by it whatever the CLI does to its
// text. After the process has exited it returns ErrExited.
func (s *Session) Send(text, uuid string) error {
	msg := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	}
	if uuid != "" {
		msg["uuid"] = uuid
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return s.writeLine(line)
}

// SendMessage is Send.
func (s *Session) SendMessage(text, uuid string) error { return s.Send(text, uuid) }

// InterruptTurn asks the session to abandon the running turn and keep the
// process alive for the next message (stream-json control request). The turn
// ends with a Result of subtype error_during_execution.
func (s *Session) InterruptTurn() error {
	id := fmt.Sprintf("athena-%d", s.reqSeq.Add(1))
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
	exited := s.proc.Exited()
	select {
	case <-exited:
		return ErrExited
	default:
	}
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	if s.stdinClosed.Load() {
		return errors.New("claude: session input is closed")
	}
	if _, err := s.proc.Stdin().Write(append(line, '\n')); err != nil {
		if s.stdinClosed.Load() {
			return errors.New("claude: session input is closed")
		}
		select {
		case <-exited:
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
func (s *Session) Interrupt() error { return s.proc.Signal(syscall.SIGINT) }

// Close closes the session's stdin, waits up to the close grace for the
// process to exit, then kills it (and its process group). It is idempotent.
func (s *Session) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	// Closing the pipe also unblocks a Send stuck on a full pipe; the grace
	// timer and kill never depend on the writers' lock.
	s.stdinClosed.Store(true)
	s.proc.CloseStdin()
	s.closeOnce.Do(func() { close(s.closedCh) })
	return nil
}

// Killed reports whether Athena had to kill the process (Close timed out or
// the context was cancelled) rather than the process exiting on its own.
func (s *Session) Killed() bool { return s.proc.Killed() }

// Wait blocks until the process has exited and Events is closed, and returns
// the process's exit error (nil on a clean exit).
func (s *Session) Wait() error {
	<-s.done
	return s.proc.Wait()
}

// Stderr returns the last few KB of the process's stderr. It may contain
// anything the CLI printed; do not log it unfiltered.
func (s *Session) Stderr() string { return s.proc.Stderr() }

// Pid returns the process id of the session's process, which leads its own process group.
func (s *Session) Pid() int { return s.proc.Pid() }
