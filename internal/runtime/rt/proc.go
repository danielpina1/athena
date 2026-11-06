package rt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/danielpina1/athena/internal/redact"
)

const (
	defaultCloseGrace = 10 * time.Second
	// drainGrace bounds how long a reader keeps reading stdout after the process has exited (a stray grandchild may
	// hold the pipe open).
	drainGrace = 2 * time.Second
	waitDelay  = 2 * time.Second
	stderrTail = 4096
)

// ProcOptions configures a runtime's process.
type ProcOptions struct {
	Binary string
	Args   []string
	Dir    string
	// Env is the child's whole environment (see ChildEnv).
	Env []string
	// CloseGrace is how long CloseStdin waits for the process to exit before it kills the group (default 10 s).
	CloseGrace time.Duration
}

// Proc is a runtime's process: in its own process group (so Ctrl-C and kills stay scoped), killed with its group when
// the context is cancelled, its stdin a pipe, its stdout a pipe the caller reads to EOF and closes, and the tail of its
// stderr kept. All methods are safe for concurrent use.
type Proc struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     *os.File // read end of the stdout pipe
	closeGrace time.Duration
	stdinOnce  sync.Once
	exited     chan struct{} // closed after cmd.Wait returned
	waitErr    error
	killed     atomic.Bool
	stderr     *tailBuffer
}

// StartProc starts the process.
func StartProc(ctx context.Context, o ProcOptions) (*Proc, error) {
	if o.CloseGrace <= 0 {
		o.CloseGrace = defaultCloseGrace
	}
	p := &Proc{closeGrace: o.CloseGrace, exited: make(chan struct{}), stderr: &tailBuffer{max: stderrTail}}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, o.Binary, o.Args...)
	p.cmd = cmd
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	cmd.Stdout = pw
	cmd.Stderr = p.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	cmd.Cancel = p.KillGroup
	stdin, err := cmd.StdinPipe()
	if err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("start %s: %w", o.Binary, err)
	}
	pw.Close() // the child holds the write end now
	p.stdin, p.stdout = stdin, pr
	go p.wait()
	return p, nil
}

func (p *Proc) wait() {
	p.waitErr = p.cmd.Wait()
	close(p.exited)
	_ = p.stdout.SetReadDeadline(time.Now().Add(drainGrace))
}

// Stdin is the process's stdin. Writers serialise themselves; CloseStdin closes it.
func (p *Proc) Stdin() io.WriteCloser { return p.stdin }

// Stdout is the read end of the process's stdout. Reads stop at most a short drain after the process has exited.
func (p *Proc) Stdout() *os.File { return p.stdout }

// Exited is closed once the process has exited.
func (p *Proc) Exited() <-chan struct{} { return p.exited }

// CloseStdin closes the process's stdin, waits up to the close grace for the process to exit, then kills its group,
// and returns once the process has exited. It is idempotent. Closing the pipe also unblocks a writer stuck on a full
// pipe.
func (p *Proc) CloseStdin() {
	p.stdinOnce.Do(func() { _ = p.stdin.Close() })
	t := time.NewTimer(p.closeGrace)
	defer t.Stop()
	select {
	case <-p.exited:
	case <-t.C:
		_ = p.KillGroup()
		<-p.exited
	}
}

// KillGroup kills the process's group (the process alone if the group cannot be signalled) and marks the process
// killed. After the process has exited it returns os.ErrProcessDone.
func (p *Proc) KillGroup() error {
	select {
	case <-p.exited:
		return os.ErrProcessDone
	default:
	}
	p.killed.Store(true)
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

// Signal sends sig to the process (not its group). After the process has exited it returns ErrExited.
func (p *Proc) Signal(sig os.Signal) error {
	select {
	case <-p.exited:
		return ErrExited
	default:
	}
	if err := p.cmd.Process.Signal(sig); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return ErrExited
		}
		return err
	}
	return nil
}

// Wait blocks until the process has exited and returns its exit error (nil on a clean exit).
func (p *Proc) Wait() error {
	<-p.exited
	return p.waitErr
}

// Killed reports whether Athena killed the process (the close grace ran out, the context was cancelled, KillGroup)
// rather than it exiting on its own.
func (p *Proc) Killed() bool { return p.killed.Load() }

// Pid returns the process id, which is also its process group id.
func (p *Proc) Pid() int { return p.cmd.Process.Pid }

// Stderr returns the last 4 KiB of the process's stderr. It may contain anything; do not log it unfiltered.
func (p *Proc) Stderr() string { return p.stderr.String() }

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, b...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// envNames are the parent variables a session inherits by name; envPrefixes those it inherits by prefix.
var (
	envNames    = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TERM", "LANG"}
	envPrefixes = []string{"LC_", "XDG_", "ATHENA_"}
)

// ChildEnv returns a session's environment: the parent's variables named in envNames or starting with one of
// envPrefixes, except those whose name looks secret (redact.SecretName), and those starting with one of keepPrefixes
// whatever their name (a runtime's own variables, which carry the credentials its session needs: CLAUDE_ and
// ANTHROPIC_ for Claude Code, CODEX_ and OPENAI_ for Codex), then extra. The server's other variables, its tokens and
// cloud keys among them, and another runtime's credentials never reach a session.
func ChildEnv(parent, extra []string, keepPrefixes ...string) []string {
	var env []string
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !inheritable(name, keepPrefixes) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

func inheritable(name string, keep []string) bool {
	for _, p := range keep {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	allowed := false
	for _, n := range envNames {
		allowed = allowed || name == n
	}
	for _, p := range envPrefixes {
		allowed = allowed || strings.HasPrefix(name, p)
	}
	return allowed && !redact.SecretName(name)
}
