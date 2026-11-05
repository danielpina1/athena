// Package rt is what the node needs of a coding-agent runtime (Claude Code, Codex): its readiness, the per-session
// files it reads, and a running session's events and input. The node speaks only to these interfaces; each runtime
// package translates its own protocol into them. It also holds the pieces every runtime shares: the process (Proc), the
// transcript (Transcript), the child environment (ChildEnv) and the sandbox description (Sandbox).
package rt

import (
	"context"
	"errors"
	"time"
)

// ErrExited is returned by a Session's Send and InterruptTurn once its process has exited.
var ErrExited = errors.New("runtime: process has exited")

// Runtime is one coding-agent CLI a node can run sessions on.
type Runtime interface {
	// Name is the runtime's label in Tickets, Assignments and metrics: "claude", "codex".
	Name() string
	// Check says whether the runtime is installed and ready to run sessions here.
	Check(ctx context.Context) Info
	// Prepare writes the per-session files a session of the runtime reads (its settings, hooks and MCP config) into
	// the session's working directory.
	Prepare(p Prepare) error
	// Start launches a session. Its process runs until the session is closed or the context is cancelled.
	Start(ctx context.Context, o StartOptions) (Session, error)
}

// Info is what Check found.
type Info struct {
	// Installed: the CLI ran. Ready: it can run sessions now (installed, logged in, its hooks in place). Tested: its
	// version is not newer than the one Athena was verified against.
	Installed, Ready, Tested bool
	Version                  string
	// Why says what keeps the runtime from being ready, or what was found about it otherwise; empty when all is well.
	Why string
}

// Prepare is what a runtime's per-session files are written from.
type Prepare struct {
	// WorkDir is the session's working directory; the files go in its .athena directory.
	WorkDir string
	// AthenaBin is the athena binary the hooks and the MCP server run; Socket the node's socket they reach.
	AthenaBin, Socket string
	// TicketID and AssignmentID are the session's, given to the MCP server.
	TicketID, AssignmentID string
	// HookTimeout bounds the hooks that wait for a person (an Approval); zero is the runtime's default.
	HookTimeout time.Duration
	// Sandbox, when set, confines the session's commands; SandboxEnv is the environment its commands need with it.
	Sandbox    *Sandbox
	SandboxEnv []string
	// Role is the session's role ("worker").
	Role string
}

// StartOptions configures one session's process.
type StartOptions struct {
	// Dir is the session's working directory, already prepared (Runtime.Prepare).
	Dir string
	// Model is the model to run, ResumeID the runtime's session id to resume ("" for a new session).
	Model, ResumeID string
	// SystemPrompt is added to the runtime's own system prompt.
	SystemPrompt string
	// Env is added to the child's environment, which is otherwise only the allowlisted part of the parent's
	// (ChildEnv).
	Env []string
	// Transcript receives every line the runtime prints, as the runtime prints it. The caller opens it and closes it
	// after Session.Wait.
	Transcript *Transcript
	// Role is the session's role ("worker"); ReadOnly asks for a session that may not change its directory.
	Role     string
	ReadOnly bool
	// Sandbox confines the session's commands for a runtime that takes it at start (Codex); one that reads it from
	// the files Prepare wrote (Claude Code) ignores it.
	Sandbox *Sandbox
	// SandboxEnv is the environment the session's commands need with the sandbox (the CA variables of
	// Prepare.SandboxEnv), for a runtime without per-session files, whose commands inherit its process environment
	// (Codex); one that takes it through Prepare (Claude Code) ignores it.
	SandboxEnv []string
	// Approve decides the approvals a runtime asks its client for (Codex); one whose approvals go through Athena's
	// hooks (Claude Code) never calls it.
	Approve func(context.Context, ApprovalRequest) Verdict
	// TicketID and AssignmentID are the session's, for a runtime that passes them to its MCP server at start.
	TicketID, AssignmentID string
	// WritableRoots are directories besides Dir the session must write to (the repository's git directory of a
	// worktree), for a runtime whose sandbox takes them at start.
	WritableRoots []string
	// CloseGrace is how long Close waits after closing the session's input before it kills the process (zero: the
	// runtime's default).
	CloseGrace time.Duration
}

// Session is a running session. All methods are safe for concurrent use.
type Session interface {
	// Events returns the session's events in order. The consumer must drain it; it is closed once the process has
	// exited and everything it printed was delivered.
	Events() <-chan Event
	// Send writes one user message; a message sent while a turn runs is folded into that turn. uuid, when not empty,
	// comes back in the message's UserEcho. After the process has exited it returns ErrExited.
	Send(text, uuid string) error
	// InterruptTurn abandons the running turn and keeps the process for the next message.
	InterruptTurn() error
	// Close closes the session's input, waits up to the close grace for the process to exit, then kills its process
	// group. It is idempotent.
	Close() error
	// Wait blocks until the process has exited and Events is closed, and returns the process's exit error.
	Wait() error
	// Killed reports whether Athena had to kill the process rather than it exiting on its own.
	Killed() bool
	// Stderr returns the last few KB of the process's stderr. It may contain anything; do not log it unfiltered.
	Stderr() string
	// Pid is the process id of the session's process, which leads its own process group.
	Pid() int
}

// ApprovalRequest is an approval a runtime asks its client for.
type ApprovalRequest struct {
	// Kind is "command", "file", "network" or "permissions".
	Kind                       string
	Command, Cwd, Host, Reason string
	Paths                      []string
}

// Verdict answers an ApprovalRequest.
type Verdict struct {
	Allow  bool
	Reason string
}
