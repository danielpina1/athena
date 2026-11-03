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

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/runtime/claude"
	"github.com/danielpina1/athena/prompts"
)

const (
	defaultSlots           = 4
	defaultModel           = "sonnet"
	defaultApprovalTimeout = 30 * time.Minute
	// hookTimeoutMargin is how much longer than an Approval a decision hook may block, so the node, not Claude
	// Code, decides what an unanswered Approval means.
	hookTimeoutMargin = 5 * time.Minute
	// maxRestarts bounds how often a session that died on its own is resumed before its Assignment reports failed.
	maxRestarts = 3
	// recoveryNote opens the first message of a session resumed after its process ended without a Stop.
	recoveryNote   = "Your previous process ended unexpectedly; continue the task."
	recoveryAdvice = " Check your directory and git history before repeating any outside action."
	crashSummary   = "session crashed repeatedly"
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
}

// EngineCallbacks is the part of *core.Engine the node calls: the Executor callbacks, the Ticket query and Approvals.
type EngineCallbacks interface {
	AssignmentStarted(ticketID, assignmentID, sessionID string) error
	InputsDelivered(ticketID, assignmentID string, inputIDs []string) error
	DeliveryFailed(ticketID, assignmentID, errMsg string) error
	InputsRead(ticketID string, inputIDs []string) error
	TurnEnded(ticketID, assignmentID string) error
	FileReport(ticketID string, r core.ReportInput) error
	Ticket(id string) (core.Ticket, core.Stage, error)
	RequestApproval(a core.Approval) (core.Approval, error)
	WaitApproval(ctx context.Context, id string) (core.Approval, error)
	ExpireApproval(id string) error
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

	// restartDelay is the pause before restart n (1-based) of a crashed session.
	restartDelay func(n int) time.Duration

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
	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{
		cfg: cfg, ctx: ctx, cancel: cancel, log: slog.Default(),
		notify:       make(chan struct{}, 1),
		done:         make(chan struct{}),
		live:         map[string]*session{},
		restartDelay: func(n int) time.Duration { return time.Duration(n) * time.Second },
		reg:          cfg.Metrics,
		slotsGauge:   cfg.Metrics.Gauge("slots_in_use"),
		spawnHist:    cfg.Metrics.Histogram("session_spawn_seconds", "runtime", runtimeClaude),
		initHist:     cfg.Metrics.Histogram("session_init_seconds", "runtime", runtimeClaude),
	}
	if eng != nil {
		n.SetEngine(eng)
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

// Close stops every session without reporting anything (their Assignments stay running, to be resumed by the next
// node) and waits for them. Queued launches are dropped.
func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		n.post(message{kind: msgShutdown})
		<-n.done
		n.runners.Wait()
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
			}
		}
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
	p, err := place(n.cfg.WorkRoot, t)
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
			if p, err := place(n.cfg.WorkRoot, t); err == nil {
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
	if !st.shutting && !s.isStopped() && len(s.deliverCh) > 0 {
		// Inputs arrived after the runner stopped listening: launch the Assignment again to take them.
		a := s.a
		a.State = core.AssignmentRunning
		if id := s.currentSessionID(); id != "" {
			a.SessionID = id
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

// schedule launches queued sessions while slots are free, skipping a Ticket whose previous session still ends and a
// non-git directory another Ticket uses.
func (n *Node) schedule(st *loopState) {
	for i := 0; i < len(st.queue); {
		if n.liveCount() >= n.cfg.Slots {
			return
		}
		s := st.queue[i]
		if n.liveSession(s.t.ID) != nil || (s.place.dirKey != "" && st.busy[s.place.dirKey] != "") {
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
	stopCh    chan struct{}

	mu          sync.Mutex
	stopped     bool
	stopReason  string
	ticketEnded bool
	sessionID   string
	// unseenFailure is set when a tool call failed and the model has not taken a step since: it may have filed a
	// result claim in the same step, before it saw the failure.
	unseenFailure bool
}

func (n *Node) newSession(t core.Ticket, a core.Assignment, p placement) *session {
	return &session{
		t: t, a: a, place: p,
		name:      filepath.Base(TicketDir(n.cfg.WorkRoot, t)),
		log:       n.log.With("ticket", t.ID, "assignment", a.ID),
		deliverCh: make(chan struct{}, 1),
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

func (s *session) setUnseenFailure(v bool) {
	s.mu.Lock()
	s.unseenFailure = v
	s.mu.Unlock()
}

func (s *session) hasUnseenFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unseenFailure
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

// variables returns the execution variables, as NAME=value pairs.
func (n *Node) variables(s *session) []string {
	return []string{
		"ATHENA_TICKET=" + s.t.ID,
		"ATHENA_NAME=" + s.name,
		"ATHENA_DIR=" + s.place.workDir,
		"ATHENA_BLOCKED_COMMANDS=" + strings.Join(n.cfg.Blocked, ","),
		"ATHENA_SOCKET=" + n.cfg.SocketPath,
	}
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
	// unread those sent that no step of the session has taken yet (resent after a crash).
	sent        map[string]bool
	unconfirmed []string
	unread      []string
	sessionID   string
	initSeen    bool

	// usage is what the session did since the last RecordUsage; tools maps the ids of tool calls in flight to their
	// tool; firstSent and firstOutput time the Ticket's time to first output.
	usage      core.UsageDelta
	tools      map[string]string
	spawned    time.Time
	firstSent  time.Time
	outputSeen bool
}

func (n *Node) run(s *session) {
	defer n.runners.Done()
	defer n.post(message{kind: msgEnded, s: s})
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
	if err := n.prepare(s); err != nil {
		r.fail("prepare ticket directory: " + err.Error())
		return
	}
	// A running Assignment with a session resumes it after a crash or a node restart; a new Assignment of a
	// Ticket (retry, resume) continues the Ticket's last session.
	resumeID, recovering := s.a.SessionID, s.a.SessionID != ""
	if resumeID == "" {
		resumeID = previousSessionID(s.t, s.a.ID)
	}
	if recovering {
		r.seedUnread()
	}
	model := n.cfg.Model
	if s.t.Execution != nil && s.t.Execution.Model != "" {
		model = s.t.Execution.Model
	}
	adir := filepath.Join(s.place.workDir, ".athena")
	env := append([]string{"ATHENA_ASSIGNMENT=" + s.a.ID}, n.variables(s)...)
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
		})
		if err != nil {
			r.fail("start claude: " + err.Error())
			return
		}
		r.spawned = time.Now()
		n.spawnHist.Observe(r.spawned.Sub(spawnStart))
		r.log.Info("session started", "resume", resumeID != "", "recovering", recovering)
		if !r.drive(sess, recovering) {
			return
		}
		r.usage.Crashes++
		r.flushUsage()
		crashes++
		if crashes > maxRestarts {
			r.fail(crashSummary)
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
// until the process exits (reported true: a crash) or the session is stopped (false). It always drains the events
// and closes the process.
func (r *runner) drive(sess *claude.Session, recovering bool) (crashed bool) {
	s := r.s
	events := sess.Events()
	r.initSeen = false
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
		case <-s.stopCh:
			crashed = false
			r.log.Info("session stopped", "reason", s.reason())
			_ = sess.Close()
			for ev := range events {
				r.account(ev) // a last Result still counts
			}
			break loop
		}
	}
	_ = sess.Close()
	err := sess.Wait()
	r.flushUsage()
	if s.isStopped() {
		return false
	}
	r.log.Warn("session process ended unexpectedly", "error", err, "killed", sess.Killed())
	return crashed
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
	if recovering {
		b.WriteString(recoveryNote + recoveryAdvice + "\n")
	} else {
		fmt.Fprintf(&b, "You are the worker of Athena Ticket %s: %s\n\nExecution variables:\n", t.ID, t.Title)
		for _, v := range r.n.variables(s) {
			b.WriteString(v + "\n")
		}
	}
	// Inputs a crashed process received but never took into a step go again, ahead of what waits.
	if len(r.unread) > 0 {
		b.WriteString("\n" + renderInputs(t, r.unread))
	}
	if len(ids) > 0 {
		b.WriteString("\n" + renderInputs(t, ids))
	}
	if r.firstSent.IsZero() {
		r.firstSent = time.Now()
	}
	if err := sess.Send(b.String()); err != nil {
		r.log.Warn("send first message", "error", err)
		return
	}
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
	if err := sess.Send(renderInputs(t, ids)); err != nil {
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
			r.unread = append(r.unread, in.ID)
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
	for _, id := range ids {
		r.sent[id] = true
	}
	r.unread = append(r.unread, ids...)
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
		if err := r.eng.AssignmentStarted(r.s.t.ID, r.s.a.ID, e.SessionID); err != nil {
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
			r.s.setUnseenFailure(true)
		}
	case claude.AssistantText, claude.ToolUse, claude.Result:
		r.s.setUnseenFailure(false)
		// A step of the session ran after the Inputs were sent: it took them in.
		if r.initSeen && len(r.unread) > 0 && r.started {
			ids := r.unread
			r.unread = nil
			if err := r.eng.InputsRead(r.s.t.ID, ids); err != nil {
				r.log.Error("record read", "error", err)
			}
		}
	}
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

// renderInputs renders Inputs for the session, in order.
func renderInputs(t core.Ticket, ids []string) string {
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
			fmt.Fprintf(&b, "[input %s: note]\n%s\n\n", in.ID, in.Text)
		default:
			from := "message"
			if in.Human {
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
// the key of a non-git directory only one Ticket may use at a time (if any).
type placement struct {
	workDir string
	repo    string
	dirKey  string
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
	case p.repo != "":
		if err := os.MkdirAll(filepath.Dir(p.workDir), 0o700); err != nil {
			return err
		}
		if err := ensureWorktree(p.repo, p.workDir, branchName(s.t)); err != nil {
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
	if err := claude.WriteSettings(filepath.Join(adir, "settings.json"), n.cfg.AthenaBin, n.cfg.SocketPath, hookTimeout); err != nil {
		return err
	}
	return claude.WriteMCPConfig(filepath.Join(adir, "mcp.json"), n.cfg.AthenaBin, n.cfg.SocketPath, s.t.ID, s.a.ID)
}

// ensureWorktree creates dir as a worktree of repo on branch, from the repository's HEAD for a new branch; an
// existing dir is reused (a resumed Ticket).
func ensureWorktree(repo, dir, branch string) error {
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dir)
		}
		return nil
	}
	// A new Ticket's branch does not exist yet: creating it with the worktree is the one git call the common case needs
	// (each git process costs ~20 ms, which counts against the dispatch budget).
	if _, err := gitOut(repo, "worktree", "add", "-b", branch, dir, "HEAD"); err == nil {
		return nil
	}
	// The branch exists (a resumed Ticket) or a removed worktree is still registered: prune, then reuse the branch.
	_, _ = gitOut(repo, "worktree", "prune")
	var err error
	if _, berr := gitOut(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); berr == nil {
		_, err = gitOut(repo, "worktree", "add", dir, branch)
	} else {
		_, err = gitOut(repo, "worktree", "add", "-b", branch, dir, "HEAD")
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
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
