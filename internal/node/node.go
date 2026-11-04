package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/runtime/claude"
	"github.com/danielpina1/athena/prompts"
)

const (
	defaultSlots           = 4
	defaultModel           = "sonnet"
	defaultApprovalTimeout = 30 * time.Minute
	// defaultCredentialRetry and maxCredentialRetry bound the pauses of a launch waiting for GitHub credentials.
	defaultCredentialRetry = 5 * time.Second
	maxCredentialRetry     = 5 * time.Minute
	// hookTimeoutMargin is how much longer than an Approval a decision hook may block, so the node, not Claude
	// Code, decides what an unanswered Approval means.
	hookTimeoutMargin = 5 * time.Minute
	// maxRestarts bounds how often a session that died on its own is resumed before its Assignment reports failed.
	maxRestarts = 3
	// recoveryNote opens the first message of a session resumed after its process ended without a Stop.
	recoveryNote   = "Your previous process ended unexpectedly; continue the task."
	recoveryAdvice = " Check your directory and git history before repeating any outside action."
	// resumeNote opens the first message of a session resumed after it was paused with nothing to do: a handoff (a
	// ready report that waits for its pull request), or a turn that ended idle before a node restart.
	resumeNote   = "Your session was paused while it waited; it resumes for the input that follows."
	crashSummary = "session crashed repeatedly"
	// nudgeText is sent, once, when a turn ends without a Report since the worker's last Input; blockedSummary is the
	// report filed when the next turn ends without one too.
	nudgeText      = "Your turn ended without a report. Call the report tool now with your status."
	blockedSummary = "worker ended its turn twice without reporting"
	// branchPrefix names the branch of a Ticket's worktree: athena/<lowercased ticket id>.
	branchPrefix = "athena/"
	slugMaxLen   = 40
	// runtimeClaude is the runtime label of the metrics this node records.
	runtimeClaude = "claude"
)

// Config configures a Node.
type Config struct {
	// WorkRoot holds the Ticket directories (default <StateDir>/work).
	WorkRoot string
	// StateDir is Athena's state directory; sessions may not read it.
	StateDir string
	// AthenaBin is the athena binary hooks and the MCP server run (default: this executable).
	AthenaBin string
	// SocketPath is the node's Unix socket, served by Serve.
	SocketPath string
	// Slots bounds how many sessions run at once (default 4); further launches queue.
	Slots int
	// Blocked lists the commands sessions may not run (ATHENA_BLOCKED_COMMANDS).
	Blocked []string
	// Model is the model of a Ticket that names none (default "sonnet").
	Model string
	// ClaudeBinary is the claude executable (default "claude").
	ClaudeBinary string
	// ApprovalTimeout is how long an escalated tool call waits for a person before it is denied (default 30m).
	ApprovalTimeout time.Duration
	// Metrics receives the node's timings and gauges (session_spawn_seconds, session_init_seconds,
	// hook_decision_seconds, mcp_tool_seconds, slots_in_use, tool_calls_total); nil records nothing.
	Metrics *metrics.Registry
	// Credentials gives GitHub Tickets (those with a Repo) their per-Assignment token and commit identity; without it
	// such a Ticket fails at launch.
	Credentials Credentials
	// CredentialRetry is the first pause before a launch that could not get its GitHub credentials yet tries again
	// (default 5 s, doubling to 5 min).
	CredentialRetry time.Duration
	// Sandbox, when set, turns on Claude Code's sandbox for worker sessions (sandbox.go); nil leaves it off.
	Sandbox *SandboxConfig
	// Logger receives the node's log lines (default slog.Default()).
	Logger *slog.Logger
	// Name and ClaudeVersion are shown in Status; the node does not interpret them.
	Name, ClaudeVersion string
	// OnStatus is called, from the node's loop goroutine, whenever the sessions in use or the launches queued change
	// (see Status). It must not block or call back into the node.
	OnStatus func()
}

// EngineCallbacks is the part of *core.Engine the node calls: the Executor callbacks, the Ticket query and Approvals.
type EngineCallbacks interface {
	AssignmentStarted(ticketID, assignmentID, sessionID, model string) error
	InputsDelivered(ticketID, assignmentID string, inputIDs []string) error
	DeliveryFailed(ticketID, assignmentID, errMsg string) error
	InputsRead(ticketID string, inputIDs []string) error
	TurnEnded(ticketID, assignmentID string) error
	FileReport(ticketID string, r core.ReportInput) error
	Ticket(id string) (core.Ticket, core.Stage, error)
	RequestApproval(a core.Approval) (core.Approval, error)
	WaitApproval(ctx context.Context, id string) (core.Approval, error)
	ExpireApproval(id string) error
	// Trigger reads a stored Trigger: a Ticket's origin for its execution variables, an Input's for its card.
	Trigger(id string) (core.Trigger, error)
	// The worker tools' commands (workertools.go) and the binding of a reported pull request.
	Reply(r core.ReplyRequest) (string, error)
	FollowForContext(ticketID, thread string) error
	MarkUnrelated(ticketID, thread, reason string) error
	Handback(ticketID, triggerID, reason string) (bool, error)
	BindThread(ticketID, thread string, own bool) error
	// RecordUsage adds what the node observed to the Ticket's stats. It takes the Engine's lock: never call it while
	// holding a node lock that Start, Deliver or Stop also take.
	RecordUsage(ticketID, assignmentID string, u core.UsageDelta) error
}

var _ EngineCallbacks = (*core.Engine)(nil)
var _ core.Executor = (*Node)(nil)

// Node is the local Executor: one headless Claude Code session per running Assignment, in the Ticket's directory.
//
// Concurrency contract (the Engine calls Start, Deliver and Stop while it holds its own lock):
//   - Start, Deliver and Stop only append to an unbounded mailbox; the node's loop goroutine handles it. They never
//     call the Engine and never wait on anything the node holds while calling the Engine.
//   - Engine callbacks run on per-session runner goroutines and socket handlers, never under a node lock.
//   - n.mu guards only the registry of live sessions; it is never held while calling the Engine.
type Node struct {
	cfg    Config
	eng    atomic.Pointer[engineRef]
	router atomic.Pointer[routerRef]
	ctx    context.Context
	cancel context.CancelFunc
	log    *slog.Logger

	mbMu   sync.Mutex
	mbox   []message
	notify chan struct{}

	done      chan struct{} // closed when the loop has exited
	closeOnce sync.Once
	runners   sync.WaitGroup

	mu   sync.Mutex
	live map[string]*session // by Ticket ID: launched sessions until their runner ends

	// streams holds the Tickets' stream rings (stream.go).
	streams streamHub

	// restartDelay is the pause before restart n (1-based) of a crashed session.
	restartDelay func(n int) time.Duration

	// queued is the number of launches waiting (written by the loop goroutine); statusSig is the last status
	// OnStatus was told of (loop goroutine only).
	queued    atomic.Int32
	statusSig string

	// tokens holds each GitHub Assignment's token, in memory only.
	tokens *tokenStore
	// afterDrop, when set (tests), runs on a runner right after it dropped its Assignment's token.
	afterDrop func(s *session)
	// afterMirrorFetch, when set (tests), runs right after the mirror's fetch from GitHub.
	afterMirrorFetch func(mirror string)
	// grants holds the nonces of the node's own git calls on a clone (repos.go): nonce -> nodeGrant.
	grants sync.Map

	// Metric handles; nil without Config.Metrics.
	reg        *metrics.Registry
	slotsGauge *metrics.Gauge
	spawnHist  *metrics.Histogram
	initHist   *metrics.Histogram
}

type engineRef struct{ e EngineCallbacks }

// New returns a Node and starts its loop. eng may be nil and set later with SetEngine (the Engine itself needs the
// Node as its Executor); it must be set before the first Start.
func New(cfg Config, eng EngineCallbacks) *Node {
	if cfg.Slots <= 0 {
		cfg.Slots = defaultSlots
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.ApprovalTimeout <= 0 {
		cfg.ApprovalTimeout = defaultApprovalTimeout
	}
	if cfg.WorkRoot == "" && cfg.StateDir != "" {
		cfg.WorkRoot = filepath.Join(cfg.StateDir, "work")
	}
	if abs, err := filepath.Abs(cfg.WorkRoot); err == nil {
		cfg.WorkRoot = abs
	}
	if cfg.AthenaBin == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.AthenaBin = exe
		}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{
		cfg: cfg, ctx: ctx, cancel: cancel, log: log,
		tokens:       newTokenStore(cfg.Credentials),
		notify:       make(chan struct{}, 1),
		done:         make(chan struct{}),
		live:         map[string]*session{},
		restartDelay: func(n int) time.Duration { return time.Duration(n) * time.Second },
		reg:          cfg.Metrics,
		slotsGauge:   cfg.Metrics.Gauge("slots_in_use"),
		spawnHist:    cfg.Metrics.Histogram("session_spawn_seconds", "runtime", runtimeClaude),
		initHist:     cfg.Metrics.Histogram("session_init_seconds", "runtime", runtimeClaude),
	}
	n.tokens.log = log
	if eng != nil {
		n.SetEngine(eng)
	}
	if cfg.StateDir != "" && cfg.AthenaBin != "" {
		if err := writeShims(cfg.StateDir, cfg.AthenaBin); err != nil {
			log.Error("write the gh shim", "error", err)
		}
	}
	go n.loop()
	return n
}

// SetEngine sets the Engine the node reports to.
func (n *Node) SetEngine(eng EngineCallbacks) { n.eng.Store(&engineRef{eng}) }

func (n *Node) engine() EngineCallbacks {
	if r := n.eng.Load(); r != nil {
		return r.e
	}
	return nil
}

// Start queues a pending Assignment for launch (core.Executor). A repeated Start of an Assignment that is queued or
// runs changes nothing. An Assignment that already has a session id resumes that session with a recovery note.
func (n *Node) Start(t core.Ticket, a core.Assignment) {
	n.post(message{kind: msgStart, t: t, a: a})
}

// Deliver queues delivery of the Ticket's waiting Inputs to its running session (core.Executor). Deliveries
// coalesce: the session sends whatever waits when it gets to it. A running Assignment without a live session (its
// process gave up) is launched again.
func (n *Node) Deliver(t core.Ticket, a core.Assignment) {
	n.post(message{kind: msgDeliver, t: t, a: a})
}

// Stop ends an Assignment's session (core.Executor); once its Ticket ended, the Ticket's worktree is cleaned up.
func (n *Node) Stop(t core.Ticket, a core.Assignment, reason string) {
	n.post(message{kind: msgStop, t: t, a: a, reason: reason})
}

// ErrNoSession is an Interrupt of a Ticket that has no session process running.
var ErrNoSession = errors.New("node: no live session")

// Interrupt asks the Ticket's running session to abandon its current turn; the process stays for the next Input. A
// Ticket without a running process is ErrNoSession.
func (n *Node) Interrupt(ticketID string) error {
	s := n.liveSession(ticketID)
	if s == nil {
		return ErrNoSession
	}
	p := s.process()
	if p == nil {
		return ErrNoSession
	}
	if err := p.InterruptTurn(); err != nil {
		if errors.Is(err, claude.ErrExited) || s.over() { // the process is ending
			return ErrNoSession
		}
		return err
	}
	return nil
}

// Close stops every session without reporting anything (their Assignments stay running, to be resumed by the next
// node) and waits for them. Queued launches are dropped.
func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		n.post(message{kind: msgShutdown})
		<-n.done
		n.runners.Wait()
		n.tokens.wait()
		n.cancel()
	})
	return nil
}

// liveCount returns how many sessions hold a slot.
func (n *Node) liveCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.live)
}

func (n *Node) liveSession(ticketID string) *session {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.live[ticketID]
}

// lookup returns the live session of a Ticket if assignmentID is its Assignment.
func (n *Node) lookup(ticketID, assignmentID string) *session {
	if ticketID == "" || assignmentID == "" {
		return nil
	}
	s := n.liveSession(ticketID)
	if s == nil || s.a.ID != assignmentID {
		return nil
	}
	return s
}

// --- mailbox and loop ---

type msgKind int

const (
	msgStart msgKind = iota
	msgDeliver
	msgStop
	msgEnded
	msgShutdown
	// msgWake only schedules: a launch put off (waiting for credentials) falls due.
	msgWake
)

type message struct {
	kind   msgKind
	t      core.Ticket
	a      core.Assignment
	reason string
	s      *session
}

// post appends to the mailbox; it never blocks beyond the mailbox's own short critical section.
func (n *Node) post(m message) {
	n.mbMu.Lock()
	n.mbox = append(n.mbox, m)
	n.mbMu.Unlock()
	select {
	case n.notify <- struct{}{}:
	default:
	}
}

func (n *Node) take() []message {
	n.mbMu.Lock()
	defer n.mbMu.Unlock()
	ms := n.mbox
	n.mbox = nil
	return ms
}

// loopState is owned by the loop goroutine.
type loopState struct {
	queue    []*session        // launches waiting for a slot or their directory
	busy     map[string]string // non-git directory -> Ticket using it
	shutting bool
}

func (n *Node) loop() {
	defer close(n.done)
	st := &loopState{busy: map[string]string{}}
	for {
		<-n.notify
		for _, m := range n.take() {
			switch m.kind {
			case msgStart:
				n.onStart(st, m.t, m.a)
			case msgDeliver:
				n.onDeliver(st, m.t, m.a)
			case msgStop:
				n.onStop(st, m.t, m.a, m.reason)
			case msgEnded:
				n.onEnded(st, m.s)
			case msgShutdown:
				n.onShutdown(st)
			case msgWake:
				n.schedule(st)
			}
		}
		n.publishStatus(st)
		if st.shutting && n.liveCount() == 0 {
			return
		}
	}
}

func (n *Node) onStart(st *loopState, t core.Ticket, a core.Assignment) {
	if st.shutting || t.Ended != nil || a.ID == "" || a.State == core.AssignmentEnded {
		return
	}
	if s := n.liveSession(t.ID); s != nil && s.a.ID == a.ID {
		return
	}
	for i, q := range st.queue {
		if q.t.ID == t.ID {
			if q.a.ID == a.ID {
				return
			}
			// A newer Assignment of the Ticket replaces a launch that never happened.
			st.queue = slices.Delete(st.queue, i, i+1)
			break
		}
	}
	if t.Repo != "" && n.cfg.Credentials == nil {
		n.goFail(t, a, "ticket works in GitHub repository "+t.Repo+", but this node has no GitHub credentials")
		return
	}
	p, err := n.place(t)
	if err != nil {
		n.goFail(t, a, "prepare ticket directory: "+err.Error())
		return
	}
	st.queue = append(st.queue, n.newSession(t, a, p))
	n.schedule(st)
}

func (n *Node) onDeliver(st *loopState, t core.Ticket, a core.Assignment) {
	if st.shutting || t.Ended != nil {
		return
	}
	if s := n.liveSession(t.ID); s != nil {
		if s.a.ID == a.ID {
			s.requestDeliver()
		}
		return
	}
	for _, q := range st.queue {
		if q.t.ID == t.ID {
			return // its launch sends what waits
		}
	}
	if a.State == core.AssignmentRunning {
		// The Assignment runs in the domain but its process is gone (it crashed repeatedly or failed): wake it.
		n.onStart(st, t, a)
	}
}

func (n *Node) onStop(st *loopState, t core.Ticket, a core.Assignment, reason string) {
	ended := t.Ended != nil
	if ended {
		n.streams.end(t.ID)
	}
	st.queue = slices.DeleteFunc(st.queue, func(q *session) bool {
		return q.a.ID == a.ID || (ended && q.t.ID == t.ID)
	})
	if s := n.liveSession(t.ID); s != nil {
		if s.a.ID == a.ID {
			s.stop(reason, ended)
		}
		return
	}
	if ended {
		// No session left to clean up after itself.
		n.runners.Add(1)
		go func() {
			defer n.runners.Done()
			if p, err := n.place(t); err == nil {
				cleanupWorktree(p, t, n.log.With("ticket", t.ID, "assignment", a.ID))
			}
		}()
	}
}

func (n *Node) onEnded(st *loopState, s *session) {
	n.mu.Lock()
	if n.live[s.t.ID] == s {
		delete(n.live, s.t.ID)
	}
	n.mu.Unlock()
	n.slotsGauge.Set(float64(n.liveCount()))
	if k := s.place.dirKey; k != "" && st.busy[k] == s.t.ID {
		delete(st.busy, k)
	}
	if wait, attempt := s.credentialWait(); wait > 0 && !st.shutting && !s.isStopped() {
		// The launch waits for GitHub credentials without a slot: it is queued again, due after the pause.
		if !slices.ContainsFunc(st.queue, func(q *session) bool { return q.t.ID == s.t.ID }) {
			ns := n.newSession(s.t, s.a, s.place)
			ns.credAttempt, ns.notBefore = attempt+1, time.Now().Add(wait)
			st.queue = append(st.queue, ns)
			time.AfterFunc(wait, func() { n.post(message{kind: msgWake}) })
		}
	} else if !st.shutting && !s.isStopped() && len(s.deliverCh) > 0 {
		// Inputs arrived after the runner stopped listening: launch the Assignment again to take them.
		a := s.a
		a.State = core.AssignmentRunning
		if id := s.currentSessionID(); id != "" {
			a.SessionID = id
		}
		if s.isParked() {
			// It handed off and its turn ended idle: it resumes as a paused session, not a crashed one.
			now := time.Now()
			a.IdleAt, a.DeliveredAt = &now, nil
		}
		n.onStart(st, s.t, a)
	}
	n.schedule(st)
}

func (n *Node) onShutdown(st *loopState) {
	st.shutting = true
	st.queue = nil
	n.mu.Lock()
	live := make([]*session, 0, len(n.live))
	for _, s := range n.live {
		live = append(live, s)
	}
	n.mu.Unlock()
	for _, s := range live {
		s.stop("node shutting down", false)
	}
}

// schedule launches queued sessions while slots are free, skipping a Ticket whose previous session still ends, a
// non-git directory another Ticket uses and a launch put off until later (waiting for credentials).
func (n *Node) schedule(st *loopState) {
	for i := 0; i < len(st.queue); {
		if n.liveCount() >= n.cfg.Slots {
			return
		}
		s := st.queue[i]
		if n.liveSession(s.t.ID) != nil || (s.place.dirKey != "" && st.busy[s.place.dirKey] != "") ||
			time.Now().Before(s.notBefore) {
			i++
			continue
		}
		st.queue = slices.Delete(st.queue, i, i+1)
		n.mu.Lock()
		n.live[s.t.ID] = s
		n.mu.Unlock()
		n.slotsGauge.Set(float64(n.liveCount()))
		if k := s.place.dirKey; k != "" {
			st.busy[k] = s.t.ID
		}
		n.runners.Add(1)
		go n.run(s)
	}
}

// goFail reports a launch that failed before any session existed, off the loop.
func (n *Node) goFail(t core.Ticket, a core.Assignment, summary string) {
	n.runners.Add(1)
	go func() {
		defer n.runners.Done()
		log := n.log.With("ticket", t.ID, "assignment", a.ID)
		log.Error("launch failed", "error", summary)
		if eng := n.engine(); eng != nil {
			if err := eng.FileReport(t.ID, core.ReportInput{AssignmentID: a.ID, State: core.ReportFailed, Summary: summary}); err != nil {
				log.Error("file failed report", "error", err)
			}
		}
	}()
}

// --- sessions ---

// session is one launched Assignment. Its fields but the guarded ones are fixed at creation.
type session struct {
	t     core.Ticket
	a     core.Assignment
	place placement
	name  string
	log   *slog.Logger

	deliverCh chan struct{} // cap 1: a delivery is wanted
	reportCh  chan struct{} // cap 1: the session filed a Report
	stopCh    chan struct{}

	mu          sync.Mutex
	stopped     bool
	stopReason  string
	ticketEnded bool
	sessionID   string
	// unseenFailure is set when a tool call failed and the model has not taken a step since: it may have filed a
	// result claim in the same step, before it saw the failure. deniedUnseen is set when the node denied a tool call
	// and its failed result has not been read from stdout yet: the deny answers the hook before stdout shows the
	// result, and events of the same step written before it may still be read meanwhile.
	unseenFailure bool
	deniedUnseen  bool
	// reports counts the Reports the session filed through the socket.
	reports int
	// githubVars are the ATHENA_GITHUB_* execution variables of a GitHub Ticket, set before the session starts.
	githubVars []string
	// ended is set once the session's runner is done, before it leaves the registry.
	ended bool
	// handoff is set while the session's latest Report is ready and its Ticket stayed open (it waits for its pull
	// request); parked once its runner ended the process for it, the Assignment running on with its session.
	handoff bool
	parked  bool
	// credWait is set when the launch could not get its GitHub credentials yet: the pause before it is queued again.
	credWait time.Duration

	// credAttempt numbers the launch attempts of a launch waiting for credentials (1 for the first); notBefore is when
	// a queued launch put off for them falls due. Both are fixed at creation.
	credAttempt int
	notBefore   time.Time
	// proc is the session's running process, nil between processes and after the last.
	proc *claude.Session
}

func (s *session) setProcess(p *claude.Session) {
	s.mu.Lock()
	s.proc = p
	s.mu.Unlock()
}

func (s *session) process() *claude.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc
}

// noteHandoff records the state of the Report the session just filed: ready on a Ticket that stays open hands off.
func (s *session) noteHandoff(handoff bool) {
	s.mu.Lock()
	s.handoff = handoff
	s.mu.Unlock()
}

func (s *session) handedOff() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handoff
}

func (s *session) markParked() {
	s.mu.Lock()
	s.parked = true
	s.mu.Unlock()
}

func (s *session) isParked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parked
}

func (s *session) setCredentialWait(d time.Duration) {
	s.mu.Lock()
	s.credWait = d
	s.mu.Unlock()
}

// credentialWait returns the pause of a launch that waits for credentials (zero: none) and its attempt number.
func (s *session) credentialWait() (time.Duration, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credWait, s.credAttempt
}

func (s *session) markEnded() {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
}

// over reports whether the session was stopped or its runner is done.
func (s *session) over() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped || s.ended
}

func (n *Node) newSession(t core.Ticket, a core.Assignment, p placement) *session {
	return &session{
		t: t, a: a, place: p, credAttempt: 1,
		name:      filepath.Base(TicketDir(n.cfg.WorkRoot, t)),
		log:       n.log.With("ticket", t.ID, "assignment", a.ID),
		deliverCh: make(chan struct{}, 1),
		reportCh:  make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
	}
}

func (s *session) requestDeliver() {
	select {
	case s.deliverCh <- struct{}{}:
	default:
	}
}

func (s *session) stop(reason string, ticketEnded bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ticketEnded = s.ticketEnded || ticketEnded
	if s.stopped {
		return
	}
	s.stopped = true
	s.stopReason = reason
	close(s.stopCh)
}

func (s *session) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *session) reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopReason
}

func (s *session) endedTicket() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ticketEnded
}

// stepTaken records a step of the model after the last failed tool result it was shown.
func (s *session) stepTaken() {
	s.mu.Lock()
	s.unseenFailure = false
	s.mu.Unlock()
}

// toolFailed records a failed tool result read from stdout; it is also the result of a deny the node gave.
func (s *session) toolFailed() {
	s.mu.Lock()
	s.unseenFailure = true
	s.deniedUnseen = false
	s.mu.Unlock()
}

// noteDenial records that the node denied a tool call of the session.
func (s *session) noteDenial() {
	s.mu.Lock()
	s.deniedUnseen = true
	s.mu.Unlock()
}

// turnOver clears both: whatever failed in the turn, its end has been seen.
func (s *session) turnOver() {
	s.mu.Lock()
	s.unseenFailure = false
	s.deniedUnseen = false
	s.mu.Unlock()
}

func (s *session) hasUnseenFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unseenFailure || s.deniedUnseen
}

// noteReport records that the session filed a Report and wakes its runner.
func (s *session) noteReport() {
	s.mu.Lock()
	s.reports++
	s.mu.Unlock()
	select {
	case s.reportCh <- struct{}{}:
	default:
	}
}

// reportCount returns how many Reports the session filed so far.
func (s *session) reportCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reports
}

func (s *session) currentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *session) setSessionID(id string) {
	s.mu.Lock()
	s.sessionID = id
	s.mu.Unlock()
}

func (s *session) setGitHubVars(vars []string) {
	s.mu.Lock()
	s.githubVars = vars
	s.mu.Unlock()
}

// variables returns the execution variables, as NAME=value pairs.
func (n *Node) variables(s *session) []string {
	vars := []string{
		"ATHENA_TICKET=" + s.t.ID,
		"ATHENA_NAME=" + s.name,
		"ATHENA_DIR=" + s.place.workDir,
		"ATHENA_BLOCKED_COMMANDS=" + strings.Join(n.cfg.Blocked, ","),
		"ATHENA_SOCKET=" + n.cfg.SocketPath,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(vars, s.githubVars...)
}

// sessionContext is what the SessionStart hook adds to the session: the execution variables and the Mandate.
func (n *Node) sessionContext(s *session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Athena Ticket %s: %s\n\nExecution variables:\n", s.t.ID, s.t.Title)
	for _, v := range n.variables(s) {
		b.WriteString(v + "\n")
	}
	m := s.t.Mandate
	fmt.Fprintf(&b, "\nMandate: %s authorization from %s:%s, scope %q, policy version %q.\n",
		m.Strength, m.Actor.Source, m.Actor.ID, m.Scope, m.PolicyVersion)
	return b.String()
}

// --- the runner: one goroutine per launched session ---

type runner struct {
	n   *Node
	s   *session
	eng EngineCallbacks
	log *slog.Logger
	// started is set once the Assignment is known to run, so InputsDelivered counts.
	started bool
	// sent holds every Input sent to this Assignment's sessions; unconfirmed those sent before it was known to run;
	// unread those sent that no turn end or Report has followed yet (resent after a crash).
	sent        map[string]bool
	unconfirmed []string
	unread      []unreadInput
	sessionID   string
	initSeen    bool
	// model is the model the sessions run with: the Ticket's, or the node's default.
	model string
	// send writes a message to the current process.
	send func(text string) error
	// inputReports is the session's Report count when it was last sent an Input (or its first message); nudged is set
	// once a turn that ended without a Report since then was nudged.
	inputReports int
	nudged       bool
	// idle is set when a turn ended well and nothing was sent since; park once the runner ends the process because its
	// worker handed off (parkIfHandedOff). paused is set when the first process resumes a paused session.
	idle   bool
	park   bool
	paused bool

	// usage is what the session did since the last RecordUsage; tools maps the ids of tool calls in flight to their
	// tool; firstSent and firstOutput time the Ticket's time to first output.
	usage      core.UsageDelta
	tools      map[string]string
	spawned    time.Time
	firstSent  time.Time
	outputSeen bool
}

// unreadInput is an Input sent to the session that may not be in its history yet; reports is the session's Report
// count once it was sent, so a later Report shows the session took it in.
type unreadInput struct {
	id      string
	reports int
}

func (n *Node) run(s *session) {
	defer n.runners.Done()
	defer n.post(message{kind: msgEnded, s: s})
	// The token lives as long as the Assignment's runner: a stop, a failure or a crash give-up drops it.
	// A new session of an Assignment whose earlier session dropped its token may hold one again.
	n.tokens.revive(s.a.ID)
	defer func() {
		s.markEnded()
		n.tokens.drop(s.a.ID)
		if n.afterDrop != nil {
			n.afterDrop(s)
		}
	}()
	r := &runner{n: n, s: s, eng: n.engine(), log: s.log, sent: map[string]bool{}, started: s.a.State == core.AssignmentRunning}
	r.run()
	if s.endedTicket() {
		cleanupWorktree(s.place, s.t, s.log)
	}
}

func (r *runner) run() {
	s, n := r.s, r.n
	if s.isStopped() {
		return
	}
	// GitHub credentials that cannot be had yet (GitHub unreachable, the bot not known yet) delay the launch; they are
	// retried until they come or the session is stopped, never filed as failed: the Ticket shows working/queued meanwhile.
	// The launch waits without a slot: the runner ends, and the loop queues it again, due after the pause (onEnded).
	if err := n.prepare(s); err != nil {
		if !credentialsLater(err) {
			r.fail("prepare ticket directory: " + redact.String(err.Error()))
			return
		}
		d := n.credentialDelay(s.credAttempt)
		r.log.Warn("github credentials not available yet; will retry the launch", "attempt", s.credAttempt, "retryIn", d.String(),
			"error", redact.String(err.Error()))
		s.setCredentialWait(d)
		return
	}
	s.setGitHubVars(n.githubVariables(s))
	// A running Assignment with a session resumes it after a crash or a node restart; a new Assignment of a
	// Ticket (retry, resume) continues the Ticket's last session.
	resumeID, recovering := s.a.SessionID, s.a.SessionID != ""
	if resumeID == "" {
		resumeID = previousSessionID(s.t, s.a.ID)
	}
	// A session whose last turn ended idle, with nothing delivered since (a handoff, or an idle Assignment after a node
	// restart), was paused, not interrupted: it resumes with the resume note instead of the crash note.
	r.paused = recovering && pausedAt(s.a)
	if recovering {
		r.seedUnread()
	}
	model := n.cfg.Model
	if s.t.Execution != nil && s.t.Execution.Model != "" {
		model = s.t.Execution.Model
	}
	r.model = model
	adir := filepath.Join(s.place.workDir, ".athena")
	env := append([]string{"ATHENA_ASSIGNMENT=" + s.a.ID}, n.variables(s)...)
	env = append(env, n.githubEnv(s)...)
	for crashes := 0; ; {
		spawnStart := time.Now()
		sess, err := claude.Start(n.ctx, claude.Options{
			Binary:             n.cfg.ClaudeBinary,
			Dir:                s.place.workDir,
			Model:              model,
			ResumeID:           resumeID,
			SettingsPath:       filepath.Join(adir, "settings.json"),
			MCPConfigPath:      filepath.Join(adir, "mcp.json"),
			AppendSystemPrompt: prompts.Worker,
			Env:                env,
			TranscriptPath:     n.transcriptPath(s.t.ID),
			OnLine:             func(off int64, line []byte) { n.streams.add(s.t.ID, off, line) },
		})
		if err != nil {
			r.fail("start claude: " + err.Error())
			return
		}
		r.spawned = time.Now()
		n.spawnHist.Observe(r.spawned.Sub(spawnStart))
		r.log.Info("session started", "resume", resumeID != "", "recovering", recovering)
		crashed, cause := r.drive(sess, recovering)
		if !crashed {
			return
		}
		r.paused = false
		r.usage.Crashes++
		r.flushUsage()
		crashes++
		if crashes > maxRestarts {
			r.fail(crashSummary + ": " + cause)
			return
		}
		if r.sessionID != "" {
			resumeID = r.sessionID
		}
		recovering = true
		if !r.sleep(n.restartDelay(crashes)) {
			return
		}
	}
}

// drive runs one process: it sends the first message, then turns events and delivery requests into Engine facts
// until the process exits (reported true: a crash, with its cause) or the session is stopped (false). It always
// drains the events and closes the process.
func (r *runner) drive(sess *claude.Session, recovering bool) (crashed bool, cause string) {
	s := r.s
	s.setProcess(sess)
	defer s.setProcess(nil)
	events := sess.Events()
	r.initSeen = false
	r.send = sess.Send
	r.sendFirst(sess, recovering)
	crashed = true
loop:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			r.onEvent(ev)
		case <-s.deliverCh:
			r.deliver(sess)
		case <-s.reportCh:
			r.markRead(s.reportCount())
			r.parkIfHandedOff()
		case <-s.stopCh:
			crashed = false
			r.log.Info("session stopped", "reason", s.reason())
			_ = sess.Close()
			for ev := range events {
				r.account(ev) // a last Result still counts
			}
			break loop
		}
		if r.park {
			// The worker handed off and its turn is over: the process ends, the slot is free, the Assignment runs on
			// with its session, which a later Input resumes.
			crashed = false
			r.log.Info("worker handed off; session paused until its next input")
			_ = sess.Close()
			for ev := range events {
				r.account(ev)
			}
			break loop
		}
	}
	_ = sess.Close()
	err := sess.Wait()
	r.flushUsage()
	if s.isStopped() || r.park {
		return false, ""
	}
	cause = crashCause(err, sess.Stderr())
	r.log.Warn("session process ended unexpectedly", "error", err, "killed", sess.Killed(), "cause", cause)
	return crashed, cause
}

// crashCause says why a session process ended: its exit status, then the last non-empty lines of its stderr with
// secrets masked, at most summaryMaxRunes in all (the end of the stderr is kept).
func crashCause(err error, stderr string) string {
	cause := "exited"
	if err != nil {
		cause = err.Error()
	}
	room := summaryMaxRunes - utf8.RuneCountInString(cause) - len(": ")
	var tail []string
	lines := strings.Split(redact.String(stderr), "\n")
	for i := len(lines) - 1; i >= 0 && room > 0; i-- {
		line := []rune(strings.TrimSpace(lines[i]))
		if len(line) == 0 {
			continue
		}
		if len(tail) > 0 {
			if room -= len(" | "); room <= 1 {
				break
			}
		}
		if len(line) > room {
			line = append([]rune("…"), line[len(line)-room+1:]...)
		}
		room -= len(line)
		tail = append([]string{string(line)}, tail...)
	}
	if len(tail) == 0 {
		return cause
	}
	return cause + ": " + strings.Join(tail, " | ")
}

func (r *runner) sendFirst(sess *claude.Session, recovering bool) {
	s := r.s
	t, _, err := r.eng.Ticket(s.t.ID)
	if err != nil {
		r.log.Error("read ticket", "error", err)
		t = s.t
	}
	ids := r.pending(t)
	var b strings.Builder
	if recovering && r.paused {
		b.WriteString(resumeNote + "\n")
	} else if recovering {
		b.WriteString(recoveryNote + recoveryAdvice + "\n")
	} else {
		fmt.Fprintf(&b, "You are the worker of Athena Ticket %s: %s\n\nExecution variables:\n", t.ID, t.Title)
		for _, v := range r.n.variables(s) {
			b.WriteString(v + "\n")
		}
	}
	// Inputs a crashed process received but never took into a step go again, ahead of what waits.
	if len(r.unread) > 0 {
		b.WriteString("\n" + renderInputs(t, r.unreadIDs(), r.trigger))
	}
	if len(ids) > 0 {
		b.WriteString("\n" + renderInputs(t, ids, r.trigger))
	}
	if r.firstSent.IsZero() {
		r.firstSent = time.Now()
	}
	if err := sess.Send(b.String()); err != nil {
		r.log.Warn("send first message", "error", err)
		return
	}
	// What went again counts from this send: only a Report after it shows the session took it in.
	for i := range r.unread {
		r.unread[i].reports = r.s.reportCount()
	}
	r.inputSent()
	r.markSent(ids)
}

// deliver sends the Inputs that wait for the Ticket now and reports exactly those as delivered.
func (r *runner) deliver(sess *claude.Session) {
	t, _, err := r.eng.Ticket(r.s.t.ID)
	if err != nil {
		r.log.Error("read ticket", "error", err)
		return
	}
	if t.Ended != nil {
		return
	}
	ids := r.pending(t)
	if len(ids) == 0 {
		return
	}
	if err := sess.Send(renderInputs(t, ids, r.trigger)); err != nil {
		if errors.Is(err, claude.ErrExited) {
			return // still waiting: the restarted process takes them
		}
		r.log.Warn("delivery failed", "error", err)
		if err := r.eng.DeliveryFailed(t.ID, r.s.a.ID, err.Error()); err != nil {
			r.log.Error("record delivery failure", "error", err)
		}
		return
	}
	r.markSent(ids)
}

// seedUnread takes over, when a new runner resumes a running Assignment (a node restart, or a relaunch after its
// process gave up), the trigger Inputs an earlier process of the Assignment received but no step of it read: they may
// never have reached the session's history, so the first message sends them again, once, after the recovery note.
func (r *runner) seedUnread() {
	t, _, err := r.eng.Ticket(r.s.t.ID)
	if err != nil {
		r.log.Error("read ticket", "error", err)
		t = r.s.t
	}
	for _, in := range t.Inputs {
		if in.Kind == core.InputTrigger && in.DeliveredTo == r.s.a.ID && in.ReadAt == nil && !slices.Contains(t.Undelivered, in.ID) && !r.sent[in.ID] {
			r.sent[in.ID] = true
			r.unread = append(r.unread, unreadInput{id: in.ID})
		}
	}
}

// pending returns the Ticket's waiting Inputs not yet sent to this Assignment, in order.
func (r *runner) pending(t core.Ticket) []string {
	var ids []string
	for _, id := range t.Undelivered {
		if !r.sent[id] {
			ids = append(ids, id)
		}
	}
	return ids
}

func (r *runner) markSent(ids []string) {
	if len(ids) == 0 {
		return
	}
	// Read the Report count after the send: a Report filed before it cannot have seen these Inputs.
	r.inputSent()
	reports := r.s.reportCount()
	for _, id := range ids {
		r.sent[id] = true
		r.unread = append(r.unread, unreadInput{id: id, reports: reports})
	}
	if !r.started {
		r.unconfirmed = append(r.unconfirmed, ids...)
		return
	}
	r.delivered(ids)
}

func (r *runner) delivered(ids []string) {
	if err := r.eng.InputsDelivered(r.s.t.ID, r.s.a.ID, ids); err != nil {
		r.log.Error("record delivery", "error", err)
	}
}

// account adds an event to the usage the runner has not yet recorded and feeds the tool call counters; a Result ends
// a turn and records it.
func (r *runner) account(ev claude.Event) {
	switch e := ev.(type) {
	case claude.AssistantText:
		r.firstOutput()
	case claude.ToolUse:
		r.firstOutput()
		r.usage.ToolCalls++
		if r.tools == nil {
			r.tools = map[string]string{}
		}
		r.tools[e.ID] = e.Name
	case claude.ToolResult:
		tool, ok := r.tools[e.ToolUseID]
		if !ok {
			tool = "unknown"
		}
		delete(r.tools, e.ToolUseID)
		if e.IsError {
			r.usage.ToolErrors++
		}
		r.n.reg.Counter("tool_calls_total", "tool", tool, "error", strconv.FormatBool(e.IsError)).Add(1)
	case claude.Result:
		r.usage.Turns += max(e.NumTurns, 1)
		r.usage.ActiveTime += time.Duration(e.DurationMS) * time.Millisecond
		r.usage.InputTokens += int64(e.InputTokens)
		r.usage.OutputTokens += int64(e.OutputTokens)
		r.usage.CacheReadTokens += int64(e.CacheReadTokens)
		// total_cost_usd is the session's cumulative cost (carried across resumed processes); the Engine records its growth.
		r.usage.SessionID, r.usage.SessionCostUSD = e.SessionID, e.CostUSD
		r.flushUsage()
	}
}

// firstOutput times the first output of the first message the worker was sent.
func (r *runner) firstOutput() {
	if r.outputSeen || r.firstSent.IsZero() {
		return
	}
	r.outputSeen = true
	r.usage.TimeToFirstOutput = time.Since(r.firstSent)
}

// flushUsage records the pending usage on the Ticket. It runs on the runner's goroutine and holds no node lock.
func (r *runner) flushUsage() {
	u := r.usage
	if u == (core.UsageDelta{}) {
		return
	}
	r.usage = core.UsageDelta{}
	if err := r.eng.RecordUsage(r.s.t.ID, r.s.a.ID, u); err != nil {
		r.log.Error("record usage", "error", err)
	}
}

func (r *runner) onEvent(ev claude.Event) {
	r.account(ev)
	switch e := ev.(type) {
	case claude.Init:
		r.initSeen = true
		if !r.spawned.IsZero() {
			r.n.initHist.Observe(time.Since(r.spawned))
		}
		if e.SessionID != "" {
			r.sessionID = e.SessionID
			r.s.setSessionID(e.SessionID)
		}
		if err := r.eng.AssignmentStarted(r.s.t.ID, r.s.a.ID, e.SessionID, r.model); err != nil {
			r.log.Error("record start", "error", err)
			return
		}
		r.started = true
		if len(r.unconfirmed) > 0 {
			ids := r.unconfirmed
			r.unconfirmed = nil
			r.delivered(ids)
		}
	case claude.ToolResult:
		// The CLI runs the tool calls of one model step together: a ready report in the same step as a Write that
		// was denied runs after the denial, but the model wrote it without knowing. Its next step has seen the result.
		if e.IsError {
			r.s.toolFailed()
		}
	case claude.AssistantText, claude.ToolUse:
		r.s.stepTaken()
	case claude.Result:
		r.s.turnOver()
		// A failed turn (an API error, an expired login, a rate limit, too many turns) leaves the CLI waiting for the
		// next message: report it, or the Ticket stays working with nothing running. Its Inputs may not have reached
		// the model, so they stay unread and a retry sends them again. A turn Athena itself ends (a stop) never gets
		// here: the stop drains the events without onEvent, and fail does nothing once the session is stopped.
		if e.IsError || (e.Subtype != "" && e.Subtype != "success") {
			r.nudged = false
			r.fail(turnFailure(e))
			return
		}
		// The turn that took in everything sent before its end is over.
		r.markRead(-1)
		r.idle = true
		r.turnEnded()
		r.parkIfHandedOff()
	}
}

// parkIfHandedOff ends the process of a worker that handed off (its latest Report is ready and its Ticket waits for
// its pull request) once its turn is over with nothing left for it: no Input waits and no Approval is pending. It
// records the turn's end, so a node restart, too, sees the Assignment idle; the Assignment runs on with its session.
func (r *runner) parkIfHandedOff() {
	s := r.s
	if !r.idle || r.park || !r.initSeen || !r.started || !s.handedOff() || s.isStopped() || len(s.deliverCh) > 0 {
		return
	}
	t, _, err := r.eng.Ticket(s.t.ID)
	if err != nil {
		r.log.Error("read ticket", "error", err)
		return
	}
	// The Ticket's own state decides: its running Assignment, this one, still stands on a ready Report.
	if l := t.LatestAssignment(); !t.HandedOff() || l == nil || l.ID != s.a.ID || t.PendingApproval != "" || len(r.pending(t)) > 0 {
		return
	}
	if err := r.eng.TurnEnded(s.t.ID, s.a.ID); err != nil {
		r.log.Error("record turn end", "error", err)
		return
	}
	s.markParked()
	r.park = true
}

// pausedAt reports whether a running Assignment's last turn ended idle with nothing delivered since.
func pausedAt(a core.Assignment) bool {
	return a.IdleAt != nil && (a.DeliveredAt == nil || !a.DeliveredAt.After(*a.IdleAt))
}

// turnFailure is the summary of a failed turn's report: its subtype and its reason, secrets masked.
func turnFailure(e claude.Result) string {
	why := strings.TrimSpace(e.Text)
	if why == "" {
		why = strings.Join(e.Errors, "; ")
	}
	subtype := e.Subtype
	if subtype == "" || subtype == "success" {
		subtype = "error"
	}
	summary := "turn failed (" + subtype + ")"
	if why != "" {
		summary += ": " + redact.String(why)
	}
	if utf8.RuneCountInString(summary) > summaryMaxRunes {
		summary = string([]rune(summary)[:summaryMaxRunes-1]) + "…"
	}
	return summary
}

// inputSent notes that the session was just sent an Input: the turn that takes it in must end with a Report.
func (r *runner) inputSent() {
	r.inputReports = r.s.reportCount()
	r.nudged = false
	r.idle = false
}

// turnEnded checks that a turn that ended well filed a Report since the worker's last Input: the first time it did
// not, the worker is nudged to report; the second, the Assignment reports blocked.
func (r *runner) turnEnded() {
	if r.s.reportCount() > r.inputReports {
		r.nudged = false
		return
	}
	if !r.nudged {
		r.nudged = true
		if r.send == nil {
			return
		}
		r.log.Info("turn ended without a report; nudging the worker")
		if err := r.send(nudgeText); err != nil {
			r.log.Warn("send nudge", "error", err)
			return
		}
		r.idle = false
		// The nudge is a delivery of no Input: DeliveredAt moves, so a restart sees the nudged turn as busy (R33).
		if r.started {
			r.delivered(nil)
		}
		return
	}
	r.inputSent() // start over: the turn of the next Input is checked afresh
	if r.s.isStopped() {
		return
	}
	r.log.Warn("worker ended its turn twice without reporting")
	if err := r.eng.FileReport(r.s.t.ID, core.ReportInput{AssignmentID: r.s.a.ID, State: core.ReportBlocked, Summary: blockedSummary}); err != nil {
		r.log.Error("file blocked report", "error", err)
	}
}

// markRead records as read the unread Inputs sent before the session's Report number reports was filed; -1 (a turn
// end) records all. The CLI does not echo user messages: a step already in flight when an Input was sent says nothing
// about it, only a Report or the end of the turn does.
func (r *runner) markRead(reports int) {
	if !r.initSeen || !r.started || len(r.unread) == 0 {
		return
	}
	var ids []string
	keep := r.unread[:0]
	for _, u := range r.unread {
		if reports < 0 || u.reports < reports {
			ids = append(ids, u.id)
		} else {
			keep = append(keep, u)
		}
	}
	r.unread = keep
	if len(ids) == 0 {
		return
	}
	if err := r.eng.InputsRead(r.s.t.ID, ids); err != nil {
		r.log.Error("record read", "error", err)
	}
}

func (r *runner) unreadIDs() []string {
	ids := make([]string, len(r.unread))
	for i, u := range r.unread {
		ids[i] = u.id
	}
	return ids
}

// fail reports the Assignment failed, unless it was stopped meanwhile.
func (r *runner) fail(summary string) {
	if r.s.isStopped() {
		return
	}
	r.log.Error("assignment failed", "error", summary)
	if err := r.eng.FileReport(r.s.t.ID, core.ReportInput{AssignmentID: r.s.a.ID, State: core.ReportFailed, Summary: summary}); err != nil {
		r.log.Error("file failed report", "error", err)
	}
}

// sleep waits d, or reports false once the session is stopped.
func (r *runner) sleep(d time.Duration) bool {
	if d <= 0 {
		return !r.s.isStopped()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.s.stopCh:
		return false
	}
}

// previousSessionID returns the session id of the Ticket's latest other Assignment that had one.
func previousSessionID(t core.Ticket, assignmentID string) string {
	for i := len(t.Assignments) - 1; i >= 0; i-- {
		if a := t.Assignments[i]; a.ID != assignmentID && a.SessionID != "" {
			return a.SessionID
		}
	}
	return ""
}

// trigger reads an Input's Trigger for its card.
func (r *runner) trigger(id string) (core.Trigger, bool) {
	tr, err := r.eng.Trigger(id)
	if err != nil {
		r.log.Warn("read trigger", "trigger", id, "error", err)
		return core.Trigger{}, false
	}
	return tr, true
}

// renderInputs renders Inputs for the session, in order; an Input whose Trigger came from a Source's Thread is a
// Trigger card (inputCard), trigger reading the Trigger.
func renderInputs(t core.Ticket, ids []string, trigger func(id string) (core.Trigger, bool)) string {
	var b strings.Builder
	for _, id := range ids {
		var in *core.Input
		for i := range t.Inputs {
			if t.Inputs[i].ID == id {
				in = &t.Inputs[i]
				break
			}
		}
		if in == nil {
			continue
		}
		switch in.Kind {
		case core.InputResumed:
			fmt.Fprintf(&b, "[input %s: the Ticket was resumed by the message that follows]\n\n", in.ID)
		case core.InputNote:
			fmt.Fprintf(&b, "[input %s: note]\nNote: %s\n\n", in.ID, in.Text)
		default:
			var tr core.Trigger
			found := false
			if in.TriggerID != "" && trigger != nil {
				if tr, found = trigger(in.TriggerID); found && len(tr.Threads) > 0 {
					b.WriteString(inputCard(*in, tr))
					continue
				}
			}
			// Only the owner's own Inputs say so: a Trigger of the owner Source, or a person Input without a Trigger.
			from := "message"
			if in.Human && (in.TriggerID == "" || (found && tr.Source == core.OwnerActor.Source)) {
				from = "message from the owner"
			}
			fmt.Fprintf(&b, "[input %s: %s]\n%s\n\n", in.ID, from, in.Text)
		}
	}
	return b.String()
}

// --- directories ---

// TranscriptPath returns the file a Ticket's sessions append their stream-json output to, across Assignments:
// <stateDir>/transcripts/<ticket id>.jsonl. It lives outside the Ticket's directory, so it survives the removal of a
// worktree, and in the state directory, which sessions may not read.
func TranscriptPath(stateDir, ticketID string) string {
	return filepath.Join(stateDir, "transcripts", ticketID+".jsonl")
}

// transcriptPath is the Ticket's TranscriptPath, or "" (the session's own default) without a state directory.
func (n *Node) transcriptPath(ticketID string) string {
	if n.cfg.StateDir == "" {
		return ""
	}
	return TranscriptPath(n.cfg.StateDir, ticketID)
}

// TicketDir returns a Ticket's directory: <workRoot>/<slug of its title>-<lowercased ticket id>.
func TicketDir(workRoot string, t core.Ticket) string {
	return filepath.Join(workRoot, slug(t.Title)+"-"+strings.ToLower(t.ID))
}

// WorkingDir returns the directory a Ticket's session runs in: its TicketDir, unless the Ticket's directory is a
// non-git directory, which is used as-is.
func WorkingDir(workRoot string, t core.Ticket) (string, error) {
	p, err := place(workRoot, t)
	return p.workDir, err
}

// slug lowercases a title to ASCII letters and digits joined by single dashes, at most slugMaxLen long.
func slug(title string) string {
	var b []byte
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && len(b) > 0 {
				b = append(b, '-')
			}
			b = append(b, byte(r))
			dash = false
		} else {
			dash = true
		}
		if len(b) >= slugMaxLen {
			break
		}
	}
	s := strings.TrimRight(string(b[:min(len(b), slugMaxLen)]), "-")
	if s == "" {
		return "ticket"
	}
	return s
}

// placement is where a Ticket runs: its working directory, the git repository its worktree comes from (if any) and
// the key of a non-git directory only one Ticket may use at a time (if any). For a GitHub Ticket, github is its
// repository (owner/name) and repo the node's clone of it.
type placement struct {
	workDir string
	repo    string
	dirKey  string
	github  string
}

// place is a Ticket's placement on this node: a GitHub Ticket without a directory of its own works in a worktree of
// the node's clone of its repository.
func (n *Node) place(t core.Ticket) (placement, error) {
	p, err := place(n.cfg.WorkRoot, t)
	if err != nil {
		return p, err
	}
	if t.Repo != "" && p.repo == "" && p.dirKey == "" {
		if _, _, err := splitRepo(t.Repo); err != nil {
			return placement{}, err
		}
		p.repo, p.github = CloneDir(n.cfg.StateDir, t.Repo), t.Repo
	}
	return p, nil
}

func place(workRoot string, t core.Ticket) (placement, error) {
	td := TicketDir(workRoot, t)
	dir := ""
	if t.Execution != nil {
		dir = t.Execution.Directory
	}
	if dir == "" {
		return placement{workDir: td}, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return placement{}, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return placement{}, fmt.Errorf("directory %s is not available: %w", dir, err)
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return placement{}, fmt.Errorf("%s is not a directory", dir)
	}
	// A directory with a .git entry is its repository's top level; asking git for it costs a process (~20 ms), which
	// counts against the dispatch budget, so only other directories (a subdirectory of a repository) are asked.
	if _, err := os.Lstat(filepath.Join(real, ".git")); err == nil {
		return placement{workDir: td, repo: real}, nil
	}
	if top, err := gitOut(real, "rev-parse", "--show-toplevel"); err == nil && top != "" {
		return placement{workDir: td, repo: top}, nil
	}
	return placement{workDir: real, dirKey: real}, nil
}

func branchName(t core.Ticket) string { return branchPrefix + strings.ToLower(t.ID) }

// prepare makes the session's directory and writes its settings and MCP config.
func (n *Node) prepare(s *session) error {
	p := s.place
	switch {
	case p.github != "":
		if login, _ := n.cfg.Credentials.Identity(); login == "" {
			// Without the bot's identity the worker's commits would carry no author: wait for it.
			return &credentialError{msg: "the GitHub App's bot identity is not known yet"}
		}
		if err := os.MkdirAll(filepath.Dir(p.workDir), 0o700); err != nil {
			return err
		}
		if err := n.prepareGitHub(s); err != nil {
			return err
		}
	case p.repo != "":
		if err := os.MkdirAll(filepath.Dir(p.workDir), 0o700); err != nil {
			return err
		}
		if err := ensureWorktree(p.repo, p.workDir, branchName(s.t), "HEAD", nil); err != nil {
			return err
		}
	case p.dirKey != "":
		// A non-git directory is used as it is.
	default:
		if err := os.MkdirAll(p.workDir, 0o700); err != nil {
			return err
		}
	}
	adir := filepath.Join(p.workDir, ".athena")
	if err := os.MkdirAll(adir, 0o700); err != nil {
		return err
	}
	// Keep Athena's files (transcript, settings) out of every commit the worker makes.
	if err := os.WriteFile(filepath.Join(adir, ".gitignore"), []byte("*\n"), 0o600); err != nil {
		return err
	}
	hookTimeout := n.cfg.ApprovalTimeout + hookTimeoutMargin
	sb, sbEnv := n.sandboxFor(p.workDir)
	if err := claude.WriteSettingsWith(filepath.Join(adir, "settings.json"), n.cfg.AthenaBin, n.cfg.SocketPath, hookTimeout, sb, sbEnv...); err != nil {
		return err
	}
	return claude.WriteMCPConfig(filepath.Join(adir, "mcp.json"), n.cfg.AthenaBin, n.cfg.SocketPath, s.t.ID, s.a.ID)
}

// ensureWorktree creates dir as a worktree of repo on branch, from base for a new branch (without tracking it); an
// existing dir is reused (a resumed Ticket).
func ensureWorktree(repo, dir, branch, base string, env []string) error {
	gitOut := func(dir string, args ...string) (string, error) { return gitIn(dir, env, args...) }
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dir)
		}
		return nil
	}
	// A new Ticket's branch does not exist yet: creating it with the worktree is the one git call the common case needs
	// (each git process costs ~20 ms, which counts against the dispatch budget).
	if _, err := gitOut(repo, "worktree", "add", "--no-track", "-b", branch, dir, base); err == nil {
		return nil
	}
	// The branch exists (a resumed Ticket) or a removed worktree is still registered: prune, then reuse the branch.
	_, _ = gitOut(repo, "worktree", "prune")
	var err error
	if _, berr := gitOut(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); berr == nil {
		_, err = gitOut(repo, "worktree", "add", dir, branch)
	} else {
		_, err = gitOut(repo, "worktree", "add", "--no-track", "-b", branch, dir, base)
	}
	return err
}

// cleanupWorktree removes an ended Ticket's worktree when nothing in it would be lost: no uncommitted or untracked
// changes (Athena's own .athena directory is ignored) and no commits on its branch that exist nowhere else (no other
// branch, no remote). Otherwise it keeps it and logs why. The branch itself is kept.
func cleanupWorktree(p placement, t core.Ticket, log *slog.Logger) {
	if p.repo == "" {
		return
	}
	if _, err := os.Stat(p.workDir); err != nil {
		return
	}
	var env []string
	if p.github != "" {
		// The clone is shared: no fetch or worktree add of another Ticket runs meanwhile, and its config is not trusted.
		defer lockRepo(p.repo)()
		env = cloneGitEnv(p.repo, p.github)
	}
	gitOut := func(dir string, args ...string) (string, error) { return gitIn(dir, env, args...) }
	dirty, err := gitOut(p.workDir, "status", "--porcelain")
	if err != nil {
		log.Warn("keeping worktree: cannot read its status", "dir", p.workDir, "error", err)
		return
	}
	if dirty != "" {
		log.Info("kept worktree with uncommitted changes", "dir", p.workDir)
		return
	}
	branch := branchName(t)
	count, err := gitOut(p.repo, "rev-list", "--count", "refs/heads/"+branch, "--not", "--exclude="+branch, "--branches", "--remotes")
	if err != nil {
		log.Warn("keeping worktree: cannot tell whether its branch is pushed", "dir", p.workDir, "error", err)
		return
	}
	if count != "0" {
		log.Info("kept worktree with unpushed commits", "dir", p.workDir, "branch", branch, "commits", count)
		return
	}
	// No --force: git itself refuses if anything changed since the check.
	if _, err := gitOut(p.repo, "worktree", "remove", p.workDir); err != nil {
		log.Warn("keeping worktree: remove failed", "dir", p.workDir, "error", err)
		return
	}
	log.Info("removed worktree", "dir", p.workDir)
}

// gitOut runs git in dir and returns its trimmed stdout.
func gitOut(dir string, args ...string) (string, error) { return gitIn(dir, nil, args...) }

// gitIn runs git in dir with env (the node's, if nil) and returns its trimmed stdout.
func gitIn(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
