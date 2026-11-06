package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/quota"
	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/render"
	"github.com/danielpina1/athena/internal/runtime/rt"
	"github.com/danielpina1/athena/prompts"
)

const (
	defaultSlots           = 4
	defaultModel           = "sonnet"
	defaultApprovalTimeout = 30 * time.Minute
	// defaultCredentialRetry and maxCredentialRetry bound the pauses of a launch waiting for GitHub credentials.
	defaultCredentialRetry = 5 * time.Second
	maxCredentialRetry     = 5 * time.Minute
	// minLaunchWait is the shortest pause of a launch that waits for its runtime's reset.
	minLaunchWait = time.Second
	// hookTimeoutMargin is how much longer than an Approval a decision hook may block, so the node, not Claude
	// Code, decides what an unanswered Approval means.
	hookTimeoutMargin = 5 * time.Minute
	// maxRestarts bounds how often a session that died on its own is resumed before its Assignment reports failed.
	maxRestarts = 3
	// recoveryNote opens the first message of a session resumed after its process ended without a Stop.
	recoveryNote   = "Your previous process ended unexpectedly; continue the task."
	recoveryAdvice = " Check your directory and git history before repeating any outside action."
	// restartNote opens the first message of a session resumed after a server restart cut its turn short.
	restartNote = "Athena restarted while you were working; continue the task."
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
	// runtimeClaude is the runtime of a Ticket that names none.
	runtimeClaude = core.RuntimeClaude
	// roleImplement is the role a worker session's transcript marker names.
	roleImplement = "implement"
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
	// ClaudeBinary is the claude executable (default "claude") of the default Runtimes.
	ClaudeBinary string
	// Runtimes are the runtimes sessions run on, by name (rt.Runtime.Name); each Assignment runs on the one Chooser
	// picks: a Ticket's pinned runtime, or for auto one with headroom. Default: Claude Code alone (DefaultRuntimes).
	Runtimes map[string]rt.Runtime
	// Chooser picks each launch's runtime (default DefaultChooser()).
	Chooser Chooser
	// Quota tracks the runtimes' usage limits from what their sessions report; nil makes one that keeps its state in
	// <StateDir>/quota.json (in memory without a StateDir).
	Quota *quota.Tracker
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
	// WorkerPrompt returns the system prompt a new worker session starts with (the owner's overrides applied); nil
	// gives prompts.Worker. A resumed session keeps the prompt it started with (see sessionPrompt).
	WorkerPrompt func() string
	// ReviewerPrompt returns the system prompt a review session starts with (the owner's override applied); nil gives
	// prompts.Reviewer. A review session always starts anew, so it is never kept.
	ReviewerPrompt func() string
	// Sandbox, when set, turns on Claude Code's sandbox for worker sessions (sandbox.go); nil leaves it off.
	Sandbox *SandboxConfig
	// Policy, when set, gives each Ticket's sessions their tool policy by the trust class of its Mandate
	// (toolpolicy.go): a restricted session always runs sandboxed, reaching GitHub alone, and asks nobody for anything.
	// nil runs every session standard.
	Policy Policy
	// Logger receives the node's log lines (default slog.Default()).
	Logger *slog.Logger
	// Name and ClaudeVersion are shown in Status; the node does not interpret them.
	Name, ClaudeVersion string
	// OnStatus is called, from the node's loop goroutine, whenever the sessions in use, the launches queued, a
	// runtime's readiness or its quota change (see Status). It must not block or call back into the node.
	OnStatus func()
	// CloseGrace is how long a session's process gets to end after its stdin closes before it is killed (default
	// 10 s).
	CloseGrace time.Duration
}

// EngineCallbacks is the part of *core.Engine the node calls: the Executor callbacks, the Ticket query and Approvals.
type EngineCallbacks interface {
	AssignmentStarted(ticketID, assignmentID, sessionID, model, runtime string) error
	InputsDelivered(ticketID, assignmentID string, inputIDs []string) error
	DeliveryFailed(ticketID, assignmentID, errMsg string) error
	InputsRead(ticketID string, inputIDs []string) error
	TurnEnded(ticketID, assignmentID string) error
	// AssignmentInterrupted records that the session of a busy Assignment is cut short from outside (a shutdown).
	AssignmentInterrupted(ticketID, assignmentID, reason string) error
	// AssignmentPaused records that the owner's pause parked the session at the end of its turn (reportOwed: the turn
	// ended without the Report the worker owed).
	AssignmentPaused(ticketID, assignmentID string, reportOwed bool) error
	// UsageLimited records that the session of a running Assignment stopped at its usage limit: a new Assignment takes
	// over, with no person stage.
	UsageLimited(ticketID, assignmentID string, l core.UsageLimit) error
	FileReport(ticketID string, r core.ReportInput) error
	// FileReview files a review session's verdict (implement + review pairing).
	FileReview(ticketID, assignmentID, verdict, findings string) error
	Ticket(id string) (core.Ticket, core.Stage, error)
	RequestApproval(a core.Approval) (core.Approval, error)
	WaitApproval(ctx context.Context, id string) (core.Approval, error)
	ExpireApproval(id string) error
	// Trigger reads a stored Trigger: a Ticket's origin for its execution variables, an Input's for its card.
	Trigger(id string) (core.Trigger, error)
	// The worker tools' commands (workertools.go) and the binding of a reported pull request.
	ReplyOnce(r core.ReplyRequest) (core.ReplyOutcome, error)
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
	// canSandbox reports whether this machine can sandbox a session (default SandboxSupported).
	canSandbox func() bool

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

	// quota is Config.Quota (or the node's own); chooser Config.Chooser (or DefaultChooser()).
	quota   *quota.Tracker
	chooser Chooser
	// chooseMu makes a launch's choice and its count of the sessions per runtime one step, so two launches never both
	// take a runtime's last session.
	chooseMu sync.Mutex
	// endGen counts the sessions that left the registry holding a runtime they chose (under mu): a launch waiting for a
	// session to end is due once it moved.
	endGen int64
	// infos is the last readiness each runtime reported (checkRuntime), for Status.
	infoMu sync.Mutex
	infos  map[string]rt.Info
	// statusGen moves whenever a runtime's readiness or quota changes, so publishStatus tells OnStatus.
	statusGen atomic.Int64
	// paused is the kill switch (pause.go): no queued launch starts and live sessions park at their turn's end.
	paused atomic.Bool
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
	if cfg.Runtimes == nil {
		cfg.Runtimes = DefaultRuntimes(cfg)
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
		canSandbox:   SandboxSupported,
		reg:          cfg.Metrics,
		slotsGauge:   cfg.Metrics.Gauge("slots_in_use"),
	}
	n.tokens.log = log
	n.infos = map[string]rt.Info{}
	n.chooser = cfg.Chooser
	if n.chooser == nil {
		n.chooser = DefaultChooser()
	}
	n.quota = cfg.Quota
	if n.quota == nil {
		path := ""
		if cfg.StateDir != "" {
			path = filepath.Join(cfg.StateDir, QuotaFile)
		}
		var err error
		if n.quota, err = quota.New(path, nil); err != nil {
			log.Error("read the quota state; starting without it", "error", err)
		}
	}
	n.quota.OnChange(n.statusChanged)
	// Claude Code's session timings exist from the start; another runtime's from its first session.
	n.histogram("session_spawn_seconds", runtimeClaude)
	n.histogram("session_init_seconds", runtimeClaude)
	if eng != nil {
		n.SetEngine(eng)
	}
	if cfg.StateDir != "" && cfg.AthenaBin != "" {
		if err := writeShims(cfg.StateDir, cfg.AthenaBin); err != nil {
			log.Error("write the gh shim", "error", err)
		}
	}
	reapOrphans(cfg.StateDir, log)
	go n.loop()
	return n
}

// histogram is the node's timing of name for a runtime (session_spawn_seconds, session_init_seconds); nil without
// Config.Metrics.
func (n *Node) histogram(name, runtime string) *metrics.Histogram {
	return n.reg.Histogram(name, "runtime", runtime)
}

// QuotaFile is the file in the state directory the node's quota Tracker keeps its state in.
const QuotaFile = "quota.json"

// statusChanged notes a change of a runtime's readiness or quota: the loop tells OnStatus.
func (n *Node) statusChanged() {
	n.statusGen.Add(1)
	n.post(message{kind: msgStatus})
}

// Quota is the node's quota Tracker.
func (n *Node) Quota() *quota.Tracker { return n.quota }

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
		if errors.Is(err, rt.ErrExited) || s.over() { // the process is ending
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
	// msgWake only schedules: a launch put off (waiting for credentials or a runtime) falls due.
	msgWake
	// msgStatus only publishes the status: a runtime's readiness or quota changed.
	msgStatus
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
			case msgStatus:
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
		s.startWhenLeft() // a session the pause parked, still leaving, starts again once it left
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
		n.dropPrompt(t.ID)
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
				log := n.log.With("ticket", t.ID, "assignment", a.ID)
				n.restoreGitLinks(t, log)
				cleanupWorktree(p, t, n.cfg.WorkRoot, log)
			}
		}()
	}
}

func (n *Node) onEnded(st *loopState, s *session) {
	n.mu.Lock()
	if n.live[s.t.ID] == s {
		delete(n.live, s.t.ID)
		if s.chosenRuntime() != "" {
			// A session that took a runtime frees it. A launch that only chose to wait took none: counting its own
			// end would wake it at once, and it would choose again in a loop.
			n.endGen++
		}
	}
	n.mu.Unlock()
	n.slotsGauge.Set(float64(n.liveCount()))
	if k := s.place.dirKey; k != "" && st.busy[k] == s.t.ID {
		delete(st.busy, k)
	}
	if w := s.launchWaitOf(); w.waits() && !st.shutting && !s.isStopped() {
		// The launch waits (for GitHub credentials, its runtime's reset or a session to end) without a slot: it is queued
		// again, due after the pause or once a session ends.
		if !slices.ContainsFunc(st.queue, func(q *session) bool { return q.t.ID == s.t.ID }) {
			ns := n.newSession(s.t, s.a, s.place)
			ns.credAttempt = s.credAttempt
			if w.credentials {
				ns.credAttempt++
			}
			ns.awaitSession, ns.awaitGen = w.awaitSession, w.awaitGen
			if w.d > 0 {
				ns.notBefore = time.Now().Add(w.d)
				time.AfterFunc(w.d, func() { n.post(message{kind: msgWake}) })
			}
			st.queue = append(st.queue, ns)
		}
	} else if !st.shutting && !s.isStopped() && (len(s.deliverCh) > 0 || s.startsWhenLeft()) {
		// Inputs arrived after the runner stopped listening, or the resume started it while the pause that parked it
		// still closed it: launch the Assignment again.
		a := s.a
		a.State = core.AssignmentRunning
		if id := s.currentSessionID(); id != "" {
			a.SessionID = id
		}
		if a.Runtime == "" {
			a.Runtime = s.chosenRuntime() // the runtime its session ran on, which its resume keeps
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
		s.stopForShutdown()
	}
}

// schedule launches queued sessions while slots are free, skipping a Ticket whose previous session still ends, a
// non-git directory another Ticket uses and a launch put off until later (waiting for credentials or a runtime).
func (n *Node) schedule(st *loopState) {
	if n.Paused() {
		return // the queue waits for the resume (SetPaused)
	}
	for i := 0; i < len(st.queue); {
		if n.liveCount() >= n.cfg.Slots {
			return
		}
		s := st.queue[i]
		if n.liveSession(s.t.ID) != nil || (s.place.dirKey != "" && st.busy[s.place.dirKey] != "") || !n.due(s) {
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

// due reports whether a queued launch may go: one put off is due after its pause, or once a session ended since it
// chose to wait for one (whichever comes first).
func (n *Node) due(s *session) bool {
	if s.awaitSession {
		n.mu.Lock()
		ended := n.endGen != s.awaitGen
		n.mu.Unlock()
		if ended {
			return true
		}
		if s.notBefore.IsZero() {
			return false
		}
	}
	return !time.Now().Before(s.notBefore)
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
	pauseCh   chan struct{} // cap 1: the node paused; park if idle
	stopCh    chan struct{}

	mu          sync.Mutex
	stopped     bool
	stopReason  string
	shutdown    bool
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
	// pauseParked is set once the node's pause parked the session; startAgain once a Start came for it while it was
	// still leaving (pause.go).
	pauseParked bool
	startAgain  bool
	// wait is set when the launch cannot go yet (GitHub credentials, its runtime limited or at its session cap): it
	// is queued again, without a slot, to be tried after the pause.
	wait launchWait

	// credAttempt numbers the launch attempts of a launch waiting for credentials (1 for the first); notBefore is when
	// a queued launch put off falls due; awaitSession makes it due also once a session ended since endGen was awaitGen.
	// All are fixed at creation.
	credAttempt  int
	notBefore    time.Time
	awaitSession bool
	awaitGen     int64
	// proc is the session's running process, nil between processes and after the last.
	proc rt.Session
	// shared holds the escalations an ask about the same action in the same turn shares (escalateShared), by key.
	shared map[string]*sharedVerdict
	// runtime is the runtime the session's runner chose (chooseRuntime), set before it prepares its directory.
	runtime string
	// toolPolicy is the session's tool policy (policy.ToolStandard, policy.ToolRestricted), read once from the Policy
	// when it prepares its directory, so that a change of the Policy never flips a running session.
	toolPolicy string
}

func (s *session) setToolPolicy(p string) {
	s.mu.Lock()
	s.toolPolicy = p
	s.mu.Unlock()
}

// restricted reports whether the session runs under the restricted tool policy: any but standard, so a session not
// prepared yet is restricted too.
func (s *session) restricted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toolPolicy != policy.ToolStandard
}

func (s *session) setRuntime(name string) {
	s.mu.Lock()
	s.runtime = name
	s.mu.Unlock()
}

// chosenRuntime is the runtime the session's runner chose, "" before it did.
func (s *session) chosenRuntime() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtime
}

// currentRuntime is the runtime the session runs on: its runner's, or before the runner chose, the Ticket's.
func (s *session) currentRuntime() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime != "" {
		return s.runtime
	}
	return runtimeName(s.t)
}

func (s *session) setProcess(p rt.Session) {
	s.mu.Lock()
	s.proc = p
	s.mu.Unlock()
}

func (s *session) process() rt.Session {
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

// launchWait is why and how long a launch waits before it is tried again.
type launchWait struct {
	// d is the pause (zero: none); why says what it waits for.
	d   time.Duration
	why string
	// credentials: it waits for GitHub credentials (the next attempt backs off further).
	credentials bool
	// awaitSession: it waits, too, for a session to end after the node's endGen was awaitGen.
	awaitSession bool
	awaitGen     int64
}

func (w launchWait) waits() bool { return w.d > 0 || w.awaitSession }

// setLaunchWait puts the launch off for d (why says what it waits for): the runner ends and the loop queues it again,
// due after the pause.
func (s *session) setLaunchWait(d time.Duration, why string) {
	s.mu.Lock()
	s.wait.d, s.wait.why = d, why
	s.mu.Unlock()
}

// awaitSessionEnd makes the launch's wait end, too, once a session ends after endGen gen.
func (s *session) awaitSessionEnd(gen int64) {
	s.mu.Lock()
	s.wait.awaitSession, s.wait.awaitGen = true, gen
	s.mu.Unlock()
}

// setCredentialWait puts the launch off for d while its GitHub credentials cannot be had.
func (s *session) setCredentialWait(d time.Duration) {
	s.setLaunchWait(d, "github credentials not available yet")
	s.mu.Lock()
	s.wait.credentials = true
	s.mu.Unlock()
}

func (s *session) launchWaitOf() launchWait {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wait
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
		pauseCh:   make(chan struct{}, 1),
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

// stopForShutdown stops the session because the node closes: its Assignment stays running for the next node, and its
// runner records an interruption if the session was busy.
func (s *session) stopForShutdown() {
	s.mu.Lock()
	s.shutdown = !s.stopped
	s.mu.Unlock()
	s.stop("node shutting down", false)
}

// stoppedByShutdown reports whether the node's shutdown stopped the session. The CLI's Stop hook may still fire while
// the process closes; that is no turn end.
func (s *session) stoppedByShutdown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdown
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

// athenaToolPrefix begins the names of Athena's own MCP tools.
const athenaToolPrefix = "mcp__athena__"

// denialSeen records that the model has seen the node's last deny (a runtime that shows no result of a denied call).
func (s *session) denialSeen() {
	s.mu.Lock()
	s.deniedUnseen = false
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
		"ATHENA_RUNTIME=" + s.currentRuntime(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(vars, s.githubVars...)
}

// sessionContext is what the SessionStart hook adds to the session: the execution variables and the Mandate.
func (n *Node) sessionContext(s *session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Athena Ticket %s: %s\n\nExecution variables:\n", s.t.ID, oneLine(s.t.Title))
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
	// model is the model the sessions run with: the Ticket's, or the node's default (Claude Code), or the one the
	// runtime's session named (Codex's own default). runtime is the runtime's name.
	model   string
	runtime string
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
	// restarted is set when the first process resumes a session a server restart cut short.
	restarted bool
	// reportOwed is set when a turn ended while paused without the Report the worker owed (pause.go).
	reportOwed bool
	// redeliver is set when sending waiting Inputs failed while the process ran: the next turn end tries again.
	redeliver bool
	// handoff is the handoff note (handoff.go) the first message of the Assignment's first process carries, if any.
	handoff string

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
		n.restoreGitLinks(s.t, s.log)
		cleanupWorktree(s.place, s.t, n.cfg.WorkRoot, s.log)
	}
}

func (r *runner) run() {
	s, n := r.s, r.n
	if s.isStopped() {
		return
	}
	// The runtime first: a launch whose runtime is limited (or at its session cap) waits without a slot, like one
	// waiting for credentials; one whose runtime cannot run it fails with why.
	rtm, ok := r.chooseRuntime()
	if !ok {
		return
	}
	// GitHub credentials that cannot be had yet (GitHub unreachable, the bot not known yet) delay the launch; they are
	// retried until they come or the session is stopped, never filed as failed: the Ticket shows working/queued meanwhile.
	// The launch waits without a slot: the runner ends, and the loop queues it again, due after the pause (onEnded).
	if err := n.prepare(s); err != nil {
		if su := (*sandboxUnavailableError)(nil); errors.As(err, &su) {
			r.fail(su.Error())
			return
		}
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
		// Only a session of the same runtime and role continues: another runtime's session id means nothing here.
		resumeID = previousSessionID(s.t, s.a.ID, rtm.Name(), s.a.Role)
	}
	// A session whose last turn ended idle, with nothing delivered since (a handoff, or an idle Assignment after a node
	// restart), was paused, not interrupted: it resumes with the resume note instead of the crash note.
	r.paused = recovering && pausedAt(s.a)
	r.restarted = recovering && !r.paused && restartInterrupted(s.a)
	if recovering && !s.reviewing() {
		r.seedUnread() // a review is sent no Inputs
	}
	// The Ticket's model, else the node's default for Claude Code; Codex runs its own default (config.toml), which its
	// session names at start (Init). An auto Ticket's model is a Claude Code model (the only kind a task named before
	// runtimes): on Codex it runs Codex's default rather than a model Codex does not have.
	model := ""
	if x := s.t.Execution; x != nil && x.Model != "" && (x.Runtime != core.RuntimeAuto || rtm.Name() == runtimeClaude) {
		model = x.Model
	} else if rtm.Name() == runtimeClaude {
		model = n.cfg.Model
	}
	r.model = model
	adir := filepath.Join(s.place.workDir, ".athena")
	env := append([]string{"ATHENA_ASSIGNMENT=" + s.a.ID}, n.variables(s)...)
	env = append(env, n.githubEnv(s)...)
	role, markerRole := roleWorker, roleImplement
	var systemPrompt string
	if s.reviewing() {
		// A review starts fresh with its own prompt, and leaves the worker's kept prompt alone.
		systemPrompt, role, markerRole = n.reviewerPrompt(), core.RoleReview, core.RoleReview
	} else {
		systemPrompt = n.sessionPrompt(s.t.ID, resumeID != "")
	}
	trPath := n.transcriptPath(s.t.ID)
	if trPath == "" {
		trPath = filepath.Join(adir, "transcript.jsonl")
	}
	sandbox, sandboxEnv := n.sessionSandbox(s.place.workDir, s.reviewing(), s.restricted())
	var roots []string
	if !s.reviewing() {
		roots = n.writableRoots(s.place.workDir)
	}
	// A new Assignment after a usage limit (or after another runtime's work) tells its session what the one before knew.
	failover := false
	if !recovering {
		r.handoff, failover = r.handoffFor(rtm.Name(), resumeID)
	}
	for crashes := 0; ; {
		if crashes == 0 && r.putOffForPause() {
			return // paused while it prepared: its first turn waits for the resume
		}
		spawnStart := time.Now()
		// One transcript per process: the session appends every line it prints, and it is closed after the process.
		tr, err := rt.OpenTranscript(trPath, func(off int64, line []byte) { n.streams.add(s.t.ID, off, line) })
		if err != nil {
			r.fail("start " + rtm.Name() + ": " + err.Error())
			return
		}
		// Each process's lines follow a marker naming its runtime, so a transcript that moves between runtimes shows where.
		reason := "start"
		if resumeID != "" {
			reason = "resume"
		}
		if failover {
			reason, failover = "failover", false // its first process only
		}
		if s.reviewing() && resumeID == "" {
			reason = "review"
		}
		// Its model is the one the session runs with, once known: Codex's own default is named by its first process.
		tr.Append(render.MarkerLine(render.Marker{At: time.Now(), Runtime: rtm.Name(), Model: r.model, Assignment: s.a.ID,
			Role: markerRole, Reason: reason}))
		// The transcript keeps what the worker was sent, so the session view shows it (internal/render); the echo is no
		// event the runner acts on (onEvent and account ignore rt.UserEcho).
		sess, err := rtm.Start(n.ctx, rt.StartOptions{
			Dir:           s.place.workDir,
			Model:         model,
			ResumeID:      resumeID,
			SystemPrompt:  systemPrompt,
			Env:           env,
			Transcript:    tr,
			Role:          role,
			ReadOnly:      s.reviewing(),
			Sandbox:       sandbox,
			SandboxEnv:    sandboxEnv,
			Approve:       func(ctx context.Context, req rt.ApprovalRequest) rt.Verdict { return n.approveRequest(ctx, s, req) },
			TicketID:      s.t.ID,
			AssignmentID:  s.a.ID,
			WritableRoots: roots,
			CloseGrace:    n.cfg.CloseGrace,
		})
		if err != nil {
			_ = tr.Close()
			if inv, ok := rtm.(invalidator); ok && uncontrolled(err) {
				inv.Invalidate() // the next launch checks again, and trusts Athena's hooks anew
			}
			r.fail("start " + rtm.Name() + ": " + redact.String(err.Error()))
			return
		}
		r.spawned = time.Now()
		n.histogram("session_spawn_seconds", rtm.Name()).Observe(r.spawned.Sub(spawnStart))
		n.recordProcess(s.t.ID, sess.Pid(), rtm.Name())
		r.runtime = rtm.Name()
		r.log.Info("session started", "resume", resumeID != "", "recovering", recovering)
		crashed, cause := r.drive(sess, recovering)
		_ = tr.Close()
		if rtm.Name() == core.RuntimeCodex {
			// Codex's sandbox let the process write the worktree's git links: they are put back before anything reads them.
			n.restoreGitLinks(s.t, r.log)
		}
		if !crashed {
			return
		}
		r.paused, r.restarted = false, false
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
			r.interruptBusy()
			return
		}
	}
}

// chooseRuntime picks the runtime of the launch (Config.Chooser): it asks the runtimes the choice may take for their
// readiness (only the pinned one, when the launch is pinned and cannot fail over; a stop ends the check), then chooses
// and records the choice on the session in one step with counting the sessions per runtime. A wait puts the launch off
// (setLaunchWait), a failover ends the Assignment at its limit (failOver) and a refusal fails it with why; all three
// report false.
func (r *runner) chooseRuntime() (rt.Runtime, bool) {
	s, n := r.s, r.n
	names := slices.Sorted(maps.Keys(n.cfg.Runtimes))
	// A session pinned only by itself (movable) whose runtime is limited may fail over: the others are asked too.
	if p := pinnedRuntime(s.t, s.a); p != "" && !(movable(s.t, s.a) && n.quota.State(p).Limited(time.Now())) {
		names = []string{p}
	}
	cctx, cancel := context.WithCancel(n.ctx)
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-cctx.Done():
		}
	}()
	ready := map[string]rt.Info{}
	for _, name := range names {
		if rtm, ok := n.cfg.Runtimes[name]; ok {
			ready[name] = n.eligible(s.a, rtm, n.checkRuntime(cctx, name, rtm))
		}
	}
	cancel()
	if s.isStopped() {
		return nil, false
	}
	n.chooseMu.Lock()
	n.mu.Lock()
	live, gen := n.liveRuntimesLocked(s), n.endGen
	n.mu.Unlock()
	c := n.chooser.Choose(ChoiceInput{Ticket: s.t, Assignment: s.a, Ready: ready, Quota: n.quota.State, Live: live, Now: time.Now()})
	launch := c.Runtime != "" && c.Until.IsZero() && !c.AwaitSession
	if info, checked := ready[c.Runtime]; launch && (!checked || !info.Ready) {
		// Whatever the Chooser says, nothing launches on a runtime the node did not find ready.
		launch = false
		why := fmt.Sprintf("runtime %q is not available on this node", c.Runtime)
		if checked {
			why = c.Runtime + " cannot run this ticket: " + whyOf(info)
		}
		c = Choice{Why: why}
	}
	if launch {
		s.setRuntime(c.Runtime)
	}
	n.chooseMu.Unlock()
	switch {
	case c.FailOver:
		r.failOver(c)
		return nil, false
	case launch:
		rtm, ok := n.cfg.Runtimes[c.Runtime]
		if !ok {
			r.fail(fmt.Sprintf("runtime %q is not available on this node", c.Runtime))
			return nil, false
		}
		return rtm, true
	case !c.Until.IsZero() || c.AwaitSession:
		var d time.Duration
		if !c.Until.IsZero() {
			// A reset that is due already still waits a moment: the launch is never retried in a loop.
			d = max(time.Until(c.Until), minLaunchWait)
		}
		r.log.Info("launch waits for its runtime", "why", c.Why, "runtime", c.Runtime, "retryIn", d.String(),
			"untilSessionEnds", c.AwaitSession)
		s.setLaunchWait(d, c.Why)
		if c.AwaitSession {
			s.awaitSessionEnd(gen)
		}
		return nil, false
	default:
		r.fail(redact.String(c.Why))
		return nil, false
	}
}

// liveRuntimesLocked counts the live sessions per runtime they chose, except s (the launch choosing). Caller holds mu.
func (n *Node) liveRuntimesLocked(except *session) map[string]int {
	live := map[string]int{}
	for _, o := range n.live {
		if o == except {
			continue
		}
		if name := o.chosenRuntime(); name != "" {
			live[name]++
		}
	}
	return live
}

// drive runs one process: it sends the first message, then turns events and delivery requests into Engine facts
// until the process exits (reported true: a crash, with its cause) or the session is stopped (false). It always
// drains the events and closes the process.
func (r *runner) drive(sess rt.Session, recovering bool) (crashed bool, cause string) {
	s := r.s
	s.setProcess(sess)
	defer s.setProcess(nil)
	events := sess.Events()
	r.initSeen = false
	r.send = func(text string) error { return sess.Send(text, "") }
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
		case <-s.pauseCh:
			r.parkIfPaused()
		case <-s.stopCh:
			crashed = false
			r.log.Info("session stopped", "reason", s.reason())
			r.interruptBusy()
			_ = sess.Close()
			for ev := range events {
				r.account(ev) // a last Result still counts
			}
			break loop
		}
		if r.park {
			// The worker handed off, or the node paused, and its turn is over: the process ends, the slot is free, the
			// Assignment runs on with its session, which a later Input (or the resume) starts again.
			crashed = false
			_ = sess.Close()
			for ev := range events {
				r.account(ev)
			}
			break loop
		}
	}
	_ = sess.Close()
	err := sess.Wait()
	r.n.forgetProcess(s.t.ID, sess.Pid())
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

func (r *runner) sendFirst(sess rt.Session, recovering bool) {
	s := r.s
	t, _, err := r.eng.Ticket(s.t.ID)
	if err != nil {
		r.log.Error("read ticket", "error", err)
		t = s.t
	}
	ids := r.pending(t)
	var b strings.Builder
	if recovering && pausedByOwner(t, s.a.ID) {
		b.WriteString(pauseNote + "\n")
	} else if recovering && r.paused {
		b.WriteString(resumeNote + "\n")
	} else if recovering && r.restarted {
		b.WriteString(restartNote + recoveryAdvice + "\n")
	} else if recovering {
		b.WriteString(recoveryNote + recoveryAdvice + "\n")
	} else if s.reviewing() {
		b.WriteString(r.reviewBrief(t))
	} else {
		b.WriteString(workerPreamble(t, r.n.variables(s)))
		if r.handoff != "" {
			b.WriteString("\n" + r.handoff + "\n")
		}
	}
	r.handoff = "" // once: a restarted process gets the recovery note
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
	if err := sess.Send(b.String(), ""); err != nil {
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

// workerPreamble opens a new session's first message: who the worker is and its execution variables. The session view
// (internal/render) recognises it and shows only the Inputs after it.
func workerPreamble(t core.Ticket, vars []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the worker of Athena Ticket %s: %s\n\nExecution variables:\n", t.ID, oneLine(t.Title))
	for _, v := range vars {
		b.WriteString(v + "\n")
	}
	return b.String()
}

// deliver sends the Inputs that wait for the Ticket now and reports exactly those as delivered.
func (r *runner) deliver(sess rt.Session) {
	if r.parkIfPaused(); r.park {
		return // paused while idle: the Inputs wait for the session's start after the resume
	}
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
	if err := sess.Send(renderInputs(t, ids, r.trigger), ""); err != nil {
		if errors.Is(err, rt.ErrExited) {
			return // still waiting: the restarted process takes them
		}
		r.log.Warn("delivery failed", "error", err)
		if err := r.eng.DeliveryFailed(t.ID, r.s.a.ID, err.Error()); err != nil {
			r.log.Error("record delivery failure", "error", err)
		}
		// The Inputs still wait. A turn that cannot take input (Codex's review or compact turn refuses a steer) takes them
		// once it ends: the delivery is tried again then.
		r.redeliver = true
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

// pending returns the Ticket's waiting Inputs not yet sent to this Assignment, in order; none to a review, whose
// Inputs wait for the implementer.
func (r *runner) pending(t core.Ticket) []string {
	if r.s.reviewing() {
		return nil
	}
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

// account adds an event to the usage the runner has not yet recorded and feeds the tool call counters; a TurnEnd ends
// a turn and records it.
func (r *runner) account(ev rt.Event) {
	switch e := ev.(type) {
	case rt.AssistantText:
		r.firstOutput()
	case rt.ToolUse:
		r.firstOutput()
		r.usage.ToolCalls++
		if r.tools == nil {
			r.tools = map[string]string{}
		}
		r.tools[e.ID] = e.Name
	case rt.ToolResult:
		tool, ok := r.tools[e.ID]
		if !ok {
			tool = "unknown"
		}
		delete(r.tools, e.ID)
		if e.IsError {
			r.usage.ToolErrors++
		}
		r.n.reg.Counter("tool_calls_total", "tool", tool, "error", strconv.FormatBool(e.IsError)).Add(1)
	case rt.TurnEnd:
		r.usage.Turns += max(e.NumTurns, 1)
		r.usage.ActiveTime += e.Duration
		r.usage.InputTokens += e.InputTokens
		r.usage.OutputTokens += e.OutputTokens
		r.usage.CacheReadTokens += e.CacheReadTokens
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
	u.Runtime = r.s.chosenRuntime()
	if err := r.eng.RecordUsage(r.s.t.ID, r.s.a.ID, u); err != nil {
		r.log.Error("record usage", "error", err)
	}
}

// onEvent acts on one of the session's events.
func (r *runner) onEvent(ev rt.Event) {
	r.account(ev)
	switch e := ev.(type) {
	case rt.QuotaUpdate:
		r.n.quota.Observe(r.runtime, e.Windows)
	case rt.Init:
		r.initSeen = true
		if !r.spawned.IsZero() {
			r.n.histogram("session_init_seconds", r.runtime).Observe(time.Since(r.spawned))
		}
		if e.SessionID != "" {
			r.sessionID = e.SessionID
			r.s.setSessionID(e.SessionID)
		}
		if r.model == "" {
			r.model = e.Model // the runtime's own default, named by the session
		}
		if err := r.eng.AssignmentStarted(r.s.t.ID, r.s.a.ID, e.SessionID, r.model, r.runtime); err != nil {
			r.log.Error("record start", "error", err)
			return
		}
		r.started = true
		if len(r.unconfirmed) > 0 {
			ids := r.unconfirmed
			r.unconfirmed = nil
			r.delivered(ids)
		}
	case rt.ToolResult:
		// The CLI runs the tool calls of one model step together: a ready report in the same step as a Write that
		// was denied runs after the denial, but the model wrote it without knowing. Its next step has seen the result.
		if e.IsError {
			r.s.toolFailed()
		}
	case rt.AssistantText, rt.ToolUse:
		r.s.stepTaken()
		if tu, ok := e.(rt.ToolUse); r.runtime == core.RuntimeCodex && (!ok || !strings.HasPrefix(tu.Name, athenaToolPrefix)) {
			// Codex shows no result of a call its PreToolUse hook denied, and runs that hook before the call's item
			// starts: a later item means the deny already went back to the model. Athena's own tools are not one: the
			// report of the denied call's step starts its item after the deny too.
			r.s.denialSeen()
		}
	case rt.TurnEnd:
		r.s.turnOver()
		if r.redeliver {
			r.redeliver = false
			r.s.requestDeliver()
		}
		// A failed turn (an API error, an expired login, a rate limit, too many turns) leaves the CLI waiting for the
		// next message: report it, or the Ticket stays working with nothing running. Its Inputs may not have reached
		// the model, so they stay unread and a retry sends them again. A turn Athena itself ends (a stop) never gets
		// here: the stop drains the events without onEvent, and fail does nothing once the session is stopped. A usage
		// limit that stopped the turn is no failure: the Ticket fails over (usageLimited), unless the worker had already
		// handed its work over in the turn (settledThisTurn), which ends as a turn that ended well and limits the runtime
		// in the quota only: the next Input, sent to a limited session, fails over then.
		switch {
		case e.Limit != nil && !r.settledThisTurn():
			r.nudged = false
			r.usageLimited(e)
			return
		case e.Limit != nil:
			r.n.quota.Hit(r.runtime, *e.Limit)
			r.log.Info("usage limit reached after the worker handed its work over; the turn ends as usual", "runtime", r.runtime)
		case e.OK:
			r.n.quota.TurnOK(r.runtime)
		default:
			r.nudged = false
			r.fail(turnFailure(e))
			return
		}
		// The turn that took in everything sent before its end is over.
		r.markRead(-1)
		r.idle = true
		if !r.owesReportAtPause() { // paused, the session parks owing its Report instead of a nudged turn
			r.turnEnded()
		}
		r.parkIfHandedOff()
		r.parkIfPaused()
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
	r.log.Info("worker handed off; session paused until its next input")
	s.markParked()
	r.park = true
}

// restartInterrupted reports whether a server restart cut the Assignment's session short and it has not resumed since.
func restartInterrupted(a core.Assignment) bool {
	i := a.OpenInterruption()
	return i != nil && i.Reason == core.InterruptRestart
}

// interruptBusy records, on a node shutdown, that the session was busy when it was cut short: the next node resumes
// it on its own. A session is busy from its first message until a turn ends well, and again after each Input or nudge
// sent since. It runs on the runner goroutine, before the process is closed.
func (r *runner) interruptBusy() {
	if !r.s.stoppedByShutdown() || r.idle {
		return
	}
	// A turn end the Engine knows (the CLI's Stop hook) that this runner has not read yet: the session is idle.
	if t, _, err := r.eng.Ticket(r.s.t.ID); err == nil {
		if l := t.LatestAssignment(); l != nil && l.ID == r.s.a.ID && pausedAt(*l) && len(t.Undelivered) == 0 {
			return
		}
	}
	if err := r.eng.AssignmentInterrupted(r.s.t.ID, r.s.a.ID, core.InterruptRestart); err != nil {
		r.log.Error("record interruption", "error", err)
	}
}

// pausedAt reports whether a running Assignment's last turn ended idle with nothing delivered since.
func pausedAt(a core.Assignment) bool {
	return a.IdleAt != nil && (a.DeliveredAt == nil || !a.DeliveredAt.After(*a.IdleAt))
}

// turnFailure is the summary of a failed turn's report: its subtype and its reason, secrets masked.
func turnFailure(e rt.TurnEnd) string {
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
	nudge, summary := r.s.nudge()
	if !r.nudged {
		r.nudged = true
		if r.send == nil {
			return
		}
		r.log.Info("turn ended without a report; nudging the worker")
		if err := r.send(nudge); err != nil {
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
	r.idle = true // nothing was sent: the session waits
	if r.s.isStopped() {
		return
	}
	r.log.Warn("worker ended its turn twice without reporting")
	if err := r.eng.FileReport(r.s.t.ID, core.ReportInput{AssignmentID: r.s.a.ID, State: core.ReportBlocked, Summary: summary}); err != nil {
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

// settledThisTurn reports whether the worker filed, since its last Input, a Report that hands the work to a person or
// to its pull request: blocked, waiting or ready, the Assignment's latest. A limit after it stops nothing the Ticket
// waits for, and a new session would run past the person stage or the finished hand-off.
func (r *runner) settledThisTurn() bool {
	if r.s.reportCount() <= r.inputReports {
		return false
	}
	t, _, err := r.eng.Ticket(r.s.t.ID)
	if err != nil {
		r.log.Error("read ticket", "error", err)
		return false
	}
	for _, a := range t.Assignments {
		if a.ID == r.s.a.ID && a.Report != nil {
			switch a.Report.State {
			case core.ReportBlocked, core.ReportWaiting, core.ReportReady:
				return true
			}
		}
	}
	return false
}

// usageLimited handles a turn that stopped at its runtime's usage limit: the limit goes to the quota (which the next
// launch's choice reads), the work in progress is checkpointed, and the Engine ends the Assignment and queues a new one
// (UsageLimited), which the chooser places: on the other runtime, or on this one once it resets. The session ends as if
// stopped, and its process has exited before the next one starts in the same worktree (one live session per Ticket).
// No person is asked: only when the Engine cannot take the fact does the Assignment report failed.
func (r *runner) usageLimited(e rt.TurnEnd) {
	s, n := r.s, r.n
	n.quota.Hit(r.runtime, *e.Limit)
	if s.isStopped() {
		return
	}
	if !r.started {
		r.fail(turnFailure(e)) // the Engine never saw this Assignment run
		return
	}
	n.checkpointSession(s)
	l := core.UsageLimit{Runtime: r.runtime, Message: redact.String(strings.TrimSpace(e.Limit.Message))}
	if at := e.Limit.ResetsAt; at != nil && at.After(time.Now()) {
		// A reset that has passed says nothing of when the limit lifts (the quota backs off instead): unknown.
		reset := at.UTC()
		l.ResetsAt = &reset
	}
	if utf8.RuneCountInString(l.Message) > summaryMaxRunes {
		l.Message = string([]rune(l.Message)[:summaryMaxRunes-1]) + "…"
	}
	if err := r.eng.UsageLimited(s.t.ID, s.a.ID, l); err != nil {
		r.log.Error("record the usage limit", "error", err)
		r.fail(turnFailure(e))
		return
	}
	r.log.Info("usage limit reached; the ticket continues in a new assignment", "runtime", r.runtime, "resetsAt", l.ResetsAt)
	s.stop(core.ReasonUsageLimit, false)
}

// failOver ends the Assignment at its session's runtime's limit before that session resumes (Choice.FailOver: the
// runtime only its session pinned is limited and another is free): as for a limit inside a turn (usageLimited), the
// work in progress is checkpointed and the Engine ends the Assignment with the quota's reset and queues a new one
// (UsageLimited), which the chooser places on the free runtime and which gets the handoff note. No session ran, so
// nothing has to exit first. When the Engine cannot take the fact, the launch waits for the reset instead.
func (r *runner) failOver(c Choice) {
	s, n := r.s, r.n
	n.checkpointSession(s)
	reset := c.Until.UTC()
	if err := r.eng.UsageLimited(s.t.ID, s.a.ID, core.UsageLimit{Runtime: c.Runtime, ResetsAt: &reset}); err != nil {
		r.log.Error("record the usage limit; the launch waits for the reset", "error", err)
		s.setLaunchWait(max(time.Until(c.Until), minLaunchWait), c.Why)
		return
	}
	r.log.Info("the session's runtime is limited; the ticket continues in a new assignment", "runtime", c.Runtime, "resetsAt", reset)
	s.stop(core.ReasonUsageLimit, false)
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

// previousSessionID returns the session id of the Ticket's latest other Assignment that had one on runtime in role
// ("" is the worker's): a session of another runtime or role is not continued. A review always starts fresh.
func previousSessionID(t core.Ticket, assignmentID, runtime, role string) string {
	if role == core.RoleReview {
		return ""
	}
	for i := len(t.Assignments) - 1; i >= 0; i-- {
		a := t.Assignments[i]
		if a.ID == assignmentID || a.SessionID == "" || a.Role != role || ranOn(t, a) != runtime {
			continue
		}
		return a.SessionID
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
			fmt.Fprintf(&b, "[input %s: note]\nNote: %s\n\n", in.ID, quoteText(in.Text))
		case core.InputReview:
			fmt.Fprintf(&b, "[input %s: review]\n%s\n\nAddress the findings, then report ready again.\n\n", in.ID, quoteText(in.Text))
		default:
			var tr core.Trigger
			found := false
			if in.TriggerID != "" && trigger != nil {
				if tr, found = trigger(in.TriggerID); found && len(tr.Threads) > 0 {
					b.WriteString(inputCard(t.ID, *in, tr))
					continue
				}
			}
			// Only the owner's own Inputs say so: a Trigger of the owner Source, or a person Input without a Trigger. They
			// come from the dashboard or `athena reply`, carry no trigger_id and are answered in the session's output.
			from := "message"
			if in.Human && (in.TriggerID == "" || (found && tr.Source == core.OwnerActor.Source)) {
				from = "message from the owner (not on GitHub)"
			}
			fmt.Fprintf(&b, "[input %s: %s]\n%s\n\n", in.ID, from, quoteText(in.Text))
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

// PromptPath returns the file that keeps the system prompt a Ticket's session started with: <stateDir>/prompts/<ticket
// id>.md. Next to the transcript, it lives in the state directory, outside every path the sandbox lets a session write
// (with the sandbox off, or an owner allowWrite that covers the state directory, a worker could rewrite its kept
// prompt). It is removed when the Ticket ends.
func PromptPath(stateDir, ticketID string) string {
	return filepath.Join(stateDir, "prompts", ticketID+".md")
}

// dropPrompt removes the prompt kept for an ended Ticket's sessions.
func (n *Node) dropPrompt(ticketID string) {
	if n.cfg.StateDir != "" {
		if err := os.Remove(PromptPath(n.cfg.StateDir, ticketID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			n.log.Warn("remove the kept session prompt", "ticket", ticketID, "error", err)
		}
	}
}

// sessionPrompt is the system prompt of the Ticket's next session. A new session gets the current worker prompt
// (overrides applied) and the node keeps it; a resumed one keeps the prompt it started with, so an override saved
// meanwhile reaches only sessions that start after it. A resume with no kept prompt (a reopened ended Ticket, whose
// prompt went with it, or a session from before prompts were kept) gets the current effective prompt.
func (n *Node) sessionPrompt(ticketID string, resumed bool) string {
	path := ""
	if n.cfg.StateDir != "" {
		path = PromptPath(n.cfg.StateDir, ticketID)
	}
	if resumed && path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return string(b)
		}
	}
	// A new session, or a resume with no kept prompt (the Ticket ended and was reopened, or the session predates kept
	// prompts): the current effective prompt, which is kept from here on.
	p := prompts.Worker
	if n.cfg.WorkerPrompt != nil {
		p = n.cfg.WorkerPrompt()
	}
	if path != "" {
		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err == nil {
			err = os.WriteFile(path, []byte(p), 0o600)
		}
		if err != nil {
			n.log.Warn("keep the session prompt: a resume will use the current one", "ticket", ticketID, "error", err)
		}
	}
	return p
}

// reviewerPrompt is the system prompt of a review session: the current reviewer prompt, overrides applied.
func (n *Node) reviewerPrompt() string {
	if n.cfg.ReviewerPrompt != nil {
		return n.cfg.ReviewerPrompt()
	}
	return prompts.Reviewer
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
	// common is repo's git directory (repoGitDir) when the placement was first made; "" without a repo.
	common string
	// worktreeConfig is the worktree's config.worktree as git made it ("" for none); nil until recorded
	// (recordWorktreeConfig).
	worktreeConfig *string
}

// place is a Ticket's placement on this node (placements.go): the one recorded at its first placement, or a new one,
// recorded.
func (n *Node) place(t core.Ticket) (placement, error) {
	if p, ok, err := n.recordedPlacement(t); ok || err != nil {
		return p, err
	}
	p, err := n.derivePlacement(t)
	if err != nil {
		return p, err
	}
	if err := n.checkUnrecorded(t, p); err != nil {
		n.log.Warn("placement refused: a Ticket that ran is now in a repository it has no worktree of", "ticket", t.ID,
			"repo", p.repo, "error", err)
		return placement{}, errBecameRepo
	}
	return p, n.recordPlacement(t, p)
}

// derivePlacement is where a Ticket goes: a GitHub Ticket without a directory of its own works in a worktree of
// the node's clone of its repository. A directory that is the state directory, or inside it, is refused.
func (n *Node) derivePlacement(t core.Ticket) (placement, error) {
	p, err := place(n.cfg.WorkRoot, t)
	if err != nil {
		return p, err
	}
	if t.Execution != nil && t.Execution.Directory != "" {
		dir := p.dirKey
		if dir == "" {
			dir = p.repo
		}
		if err := n.checkDirectory(dir); err != nil {
			return placement{}, err
		}
	}
	if t.Repo != "" && p.repo == "" && p.dirKey == "" {
		if _, _, err := splitRepo(t.Repo); err != nil {
			return placement{}, err
		}
		p.repo, p.github = CloneDir(n.cfg.StateDir, t.Repo), t.Repo
	}
	switch {
	case p.github != "":
		p.common = filepath.Join(p.repo, ".git") // the node's clone, which it makes itself
	case p.repo != "":
		if p.common, err = repoGitDir(p.repo); err != nil {
			return placement{}, err
		}
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

// WorktreeBranch is the branch of the Ticket's worktree (athena/<lowercased id>) in the cases a view can tell without
// running git: the Ticket names a GitHub repository and no directory, or its directory has a .git entry (a repository's
// top level). A repository with a plain-folder directory (place uses the folder itself) and a subdirectory of a
// repository (place asks git) give "". hasGit tells whether a directory has a .git entry; the hub answers it from what
// a pass off its loop found, so a view never touches the filesystem (nil: no directory has one).
func WorktreeBranch(t core.Ticket, hasGit func(dir string) bool) string {
	dir := ""
	if t.Execution != nil {
		dir = t.Execution.Directory
	}
	if dir == "" {
		if t.Repo != "" {
			return branchName(t)
		}
		return ""
	}
	if hasGit != nil && hasGit(dir) {
		return branchName(t)
	}
	return ""
}

// prepare makes the session's directory and writes its settings and MCP config.
func (n *Node) prepare(s *session) error {
	p := s.place
	s.setToolPolicy(n.toolPolicy(s.t))
	if err := n.checkRestrictedSandbox(s); err != nil {
		return err
	}
	// An earlier session may have repointed the worktree's git links and its process not ended cleanly (a node that
	// died, a child that outlived it): put them back before anything, the sandbox included, reads them.
	if err := n.restoreGitLinks(s.t, s.log); err != nil {
		return err
	}
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
		n.recordWorktreeConfig(s.t, s.log)
	case p.repo != "":
		if err := os.MkdirAll(filepath.Dir(p.workDir), 0o700); err != nil {
			return err
		}
		if err := ensureWorktreeIn(p.repo, p.common, p.workDir, branchName(s.t), "HEAD", nil); err != nil {
			return err
		}
		n.recordWorktreeConfig(s.t, s.log)
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
	name := s.currentRuntime() // the one the runner chose
	rtm, ok := n.cfg.Runtimes[name]
	if !ok {
		return fmt.Errorf("runtime %q is not available on this node", name)
	}
	sb, sbEnv := n.sessionSandbox(p.workDir, s.reviewing(), s.restricted())
	role := roleWorker
	if s.reviewing() {
		role = core.RoleReview
	}
	return rtm.Prepare(rt.Prepare{
		WorkDir: p.workDir, AthenaBin: n.cfg.AthenaBin, Socket: n.cfg.SocketPath, TicketID: s.t.ID, AssignmentID: s.a.ID,
		HookTimeout: n.cfg.ApprovalTimeout + hookTimeoutMargin, Sandbox: sb, SandboxEnv: sbEnv, Role: role,
		ReadOnly: s.reviewing(),
	})
}

// ensureWorktree creates dir as a worktree of repo on branch, from base for a new branch (without tracking it); an
// existing dir is reused (a resumed Ticket). Its git directory must be where worktreeAdmin says (the node finds it
// there from then on, never from the files a worker can rewrite).
func ensureWorktree(repo, dir, branch, base string, env []string) error {
	common, err := repoGitDir(repo)
	if err != nil {
		return err
	}
	return ensureWorktreeIn(repo, common, dir, branch, base, env)
}

// ensureWorktreeIn is ensureWorktree with repo's git directory known (placement.common).
func ensureWorktreeIn(repo, common, dir, branch, base string, env []string) (err error) {
	renv := repoGitEnv(env, common, repo)
	gitOut := func(args ...string) (string, error) { return gitIn(repo, renv, args...) }
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dir)
		}
		return nil
	}
	admin := worktreeAdmin(common, dir)
	added := func() error {
		// git names a worktree's git directory after its directory, unless that name is taken.
		b, err := readSmall(filepath.Join(admin, "gitdir"))
		if err != nil {
			return fmt.Errorf("the worktree at %s has no git directory at %s: %w", dir, admin, err)
		}
		at, want := strings.TrimSpace(string(b)), filepath.Join(dir, ".git")
		if !filepath.IsAbs(at) {
			at = filepath.Join(admin, at)
		}
		if a, err := os.Stat(filepath.Dir(at)); err != nil || filepath.Base(at) != ".git" || !sameDir(a, filepath.Dir(want)) {
			return fmt.Errorf("the git directory %s is not the worktree at %s's", admin, dir)
		}
		return nil
	}
	if _, err := os.Lstat(admin); err == nil {
		_, _ = gitOut("worktree", "prune") // a removed worktree of the same name is still registered
	}
	// A new Ticket's branch does not exist yet: creating it with the worktree is the one git call the common case needs
	// (each git process costs ~20 ms, which counts against the dispatch budget).
	if _, err := gitOut("worktree", "add", "--no-track", "-b", branch, dir, base); err == nil {
		return added()
	}
	// The branch exists (a resumed Ticket) or a removed worktree is still registered: prune, then reuse the branch.
	_, _ = gitOut("worktree", "prune")
	if _, berr := gitOut("rev-parse", "--verify", "--quiet", "refs/heads/"+branch); berr == nil {
		_, err = gitOut("worktree", "add", dir, branch)
	} else {
		_, err = gitOut("worktree", "add", "--no-track", "-b", branch, dir, base)
	}
	if err != nil {
		return err
	}
	return added()
}

func sameDir(a os.FileInfo, dir string) bool {
	b, err := os.Stat(dir)
	return err == nil && os.SameFile(a, b)
}

// repoGitDir is the git directory of the repository whose top level is repo: repo/.git when it is a directory (the
// node's clones, and an owner's repository), else the common directory git names for it. A Ticket's worker never
// works in repo itself.
func repoGitDir(repo string) (string, error) {
	dotGit := filepath.Join(repo, ".git")
	if st, err := os.Lstat(dotGit); err == nil && st.IsDir() {
		return dotGit, nil
	}
	out, err := gitIn(repo, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Clean(out), nil
}

// worktreeAdmin is the git directory of the worktree at dir of the repository whose git directory is common, as
// ensureWorktree made it: <common>/worktrees/<dir's name>.
func worktreeAdmin(common, dir string) string {
	return filepath.Join(common, "worktrees", filepath.Base(dir))
}

// repoGitEnv is env (the node's, if nil) with git's directories pinned to the repository's own: git reads no .git
// entry, commondir file or gitdir file to find them.
func repoGitEnv(env []string, common, repo string) []string {
	return append(gitBase(env), "GIT_DIR="+common, "GIT_COMMON_DIR="+common, "GIT_WORK_TREE="+repo)
}

// worktreeGitEnv is env (the node's, if nil) with git's directories pinned to those Athena made for the worktree at
// dir: its git directory admin (findWorktreeAdmin), the repository's common directory and dir. The worktree's .git file
// and its git directory's commondir, which the worker can rewrite to point at a directory of its own (with a config
// naming a filter driver, an fsmonitor or hooks), are never read.
func worktreeGitEnv(env []string, admin, common, dir string) []string {
	return append(gitBase(env), "GIT_DIR="+admin, "GIT_COMMON_DIR="+common, "GIT_WORK_TREE="+dir)
}

// legacyAdminRe matches the name git gives a worktree's git directory when its directory's name is taken: the name and
// a number.
var legacyAdminRe = regexp.MustCompile(`^[0-9]+$`)

// findWorktreeAdmin is the git directory of the worktree at dir of the repository whose git directory is common:
// worktreeAdmin, as ensureWorktree makes it, or for a worktree made before ensureWorktree checked that, the
// <common>/worktrees/<dir's name><number> git named it, when its gitdir file names dir/.git and dir is in the node's
// work root (workRoot). A worker can rewrite a gitdir file, but the name limits the choice to a git directory of this
// worktree's own name, which is the Ticket's; the common directory stays pinned whichever it is. Anything else is an
// error, and the caller keeps away from the worktree. Never for ensureWorktree, which checks the name it makes.
func findWorktreeAdmin(common, dir, workRoot string, log *slog.Logger) (string, error) {
	admin := worktreeAdmin(common, dir)
	realDir, okDir := realpath(filepath.Join(dir, ".git"))
	// names reports whether the gitdir file of the git directory cand names dir's .git. It only ever rules a candidate
	// out: what may be chosen is fixed by its name.
	names := func(cand string) bool {
		b, err := readSmall(filepath.Join(cand, "gitdir"))
		if err != nil || !okDir {
			return false
		}
		at := strings.TrimSpace(string(b))
		if !filepath.IsAbs(at) {
			at = filepath.Join(cand, at) // worktree.useRelativePaths (git 2.48+)
		}
		real, ok := realpath(at)
		return ok && real == realDir
	}
	if st, err := os.Lstat(admin); err == nil && st.IsDir() && names(admin) {
		return admin, nil
	}
	// A worktree made before ensureWorktree checked the name, whose own was taken (by another worktree of the same
	// name): git's <name><number>, only for a worktree in the node's work root.
	realRoot, okRoot := realpath(workRoot)
	if entries, err := os.ReadDir(filepath.Join(common, "worktrees")); err == nil && okDir && okRoot && workRoot != "" &&
		within(realRoot, filepath.Dir(realDir)) {
		base := filepath.Base(dir)
		for _, e := range entries {
			rest, ok := strings.CutPrefix(e.Name(), base)
			if !ok || !legacyAdminRe.MatchString(rest) || !e.IsDir() {
				continue
			}
			if cand := filepath.Join(common, "worktrees", e.Name()); names(cand) {
				log.Info("using the git dir git named a worktree of an earlier node", "dir", dir, "gitdir", cand)
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("the worktree at %s has an unregistered or renamed git dir (not %s)", dir, admin)
}

func gitBase(env []string) []string {
	if env == nil {
		return os.Environ()
	}
	return slices.Clone(env)
}

// keptNote is what the log says of a kept worktree: its git links were put back (restoreGitLinks), but the files in
// it are the worker's.
const keptNote = "its git links were checked against the placement record; its files are the worker's: " +
	"read them before you run anything there (docs/dev/codex.md §5)"

// cleanupWorktree removes an ended Ticket's worktree when nothing in it would be lost: no uncommitted or untracked
// changes (Athena's own .athena directory is ignored) and no commits on its branch that exist nowhere else (no other
// branch, no remote). Otherwise it keeps it and logs why. The branch itself is kept; the Ticket's checkpoint ref goes
// with the worktree.
func cleanupWorktree(p placement, t core.Ticket, workRoot string, log *slog.Logger) {
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
	common := p.common
	if common == "" {
		var err error
		if common, err = repoGitDir(p.repo); err != nil {
			log.Warn("keeping worktree: cannot find its repository's git directory", "ticket", t.ID, "dir", p.workDir, "error", err)
			return
		}
	}
	admin, err := findWorktreeAdmin(common, p.workDir, workRoot, log)
	if err != nil {
		log.Warn("keeping worktree: remove it by hand", "ticket", t.ID, "dir", p.workDir, "error", err)
		return
	}
	// git's directories are those Athena made, never those the worktree's files name now.
	wenv, renv := worktreeGitEnv(env, admin, common, p.workDir), repoGitEnv(env, common, p.repo)
	gitOut := func(dir string, args ...string) (string, error) {
		if dir == p.workDir {
			return gitIn(dir, wenv, args...)
		}
		return gitIn(dir, renv, args...)
	}
	dirty, err := gitOut(p.workDir, "status", "--porcelain")
	if err != nil {
		log.Warn("keeping worktree: cannot read its status", "dir", p.workDir, "error", err)
		return
	}
	if dirty != "" {
		log.Info("kept worktree with uncommitted changes", "dir", p.workDir, "note", keptNote)
		return
	}
	branch := branchName(t)
	count, err := gitOut(p.repo, "rev-list", "--count", "refs/heads/"+branch, "--not", "--exclude="+branch, "--branches", "--remotes")
	if err != nil {
		log.Warn("keeping worktree: cannot tell whether its branch is pushed", "dir", p.workDir, "error", err)
		return
	}
	if count != "0" {
		log.Info("kept worktree with unpushed commits", "dir", p.workDir, "branch", branch, "commits", count, "note", keptNote)
		return
	}
	// No --force: git itself refuses if anything changed since the check.
	if _, err := gitOut(p.repo, "worktree", "remove", p.workDir); err != nil {
		log.Warn("keeping worktree: remove failed", "dir", p.workDir, "error", err)
		return
	}
	log.Info("removed worktree", "dir", p.workDir)
	// Its checkpoint (checkpoint.go) goes with it: nothing of the Ticket's work in progress stays reachable.
	if _, err := gitOut(p.repo, "update-ref", "-d", checkpointRef(t.ID)); err != nil {
		log.Warn("remove the checkpoint ref", "ref", checkpointRef(t.ID), "error", err)
	}
}

// gitOut runs git in dir and returns its trimmed stdout.
func gitOut(dir string, args ...string) (string, error) { return gitIn(dir, nil, args...) }

// gitPins are the command-scope settings of every node git gitIn runs: no hook (a worktree add runs post-checkout) and
// no fsmonitor of the repository's, whose config a worker may have reached; repos.go's noHooks, as arguments.
var gitPins = []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}

// gitIn runs git in dir with env (the node's, if nil) and the pins above, and returns its trimmed stdout.
func gitIn(dir string, env []string, args ...string) (string, error) {
	return gitInCtx(context.Background(), dir, env, args...)
}

// gitInCtx is gitIn bounded by ctx: git is killed once ctx ends, and its output is waited for at most a second more.
func gitInCtx(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	full := append(slices.Clone(gitPins), "-C", dir)
	cmd := exec.CommandContext(ctx, "git", append(full, args...)...)
	if ctx.Done() != nil {
		cmd.WaitDelay = time.Second // only a bounded call: gitIn waits for its output as it always did
	}
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

// recordProcess notes a session process of runtime in the run directory, for the next node to stop if this one dies
// first.
func (n *Node) recordProcess(ticketID string, pid int, runtime string) {
	if n.cfg.StateDir == "" {
		return
	}
	if err := recordProcess(n.cfg.StateDir, ticketID, pid, runtime); err != nil {
		n.log.Warn("record session process", "ticket", ticketID, "error", err)
	}
}

func (n *Node) forgetProcess(ticketID string, pid int) {
	if n.cfg.StateDir != "" {
		forgetProcess(n.cfg.StateDir, ticketID, pid)
	}
}
