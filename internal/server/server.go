// Package server runs `athena server`: the store, the Policy, the Sources (GitHub, when config.json names an App), the
// effects runner with the Outbox and the router, the Engine, the local node with its socket, and the control socket the
// owner's CLI talks to. wiring.go holds the parts between them; config.go the configuration file.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/hub"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/outbox"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/router"
	"github.com/danielpina1/athena/internal/sources"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/danielpina1/athena/internal/store"
)

// Files in the state directory.
const (
	DBFile        = "athena.db"
	ControlSocket = "athena.sock"
	NodeSocket    = "node.sock"
	PIDFile       = "athena.pid"
	LogDir        = "logs"
	LogFile       = "athena.log"
)

const (
	socketWait    = 5 * time.Second
	pingTimeout   = 2 * time.Second
	acceptBackoff = 100 * time.Millisecond
)

// ErrAlreadyRunning is returned when another server answers on the control socket.
var ErrAlreadyRunning = errors.New("athena server already running")

// Config configures the server.
type Config struct {
	// StateDir holds the database, the sockets, the pid file and the logs.
	StateDir string
	// Node configures the local node; Run sets its StateDir and SocketPath, defaults its WorkRoot to <StateDir>/work
	// and its AthenaBin to this executable.
	Node node.Config
	// Stderr receives a copy of the log besides <StateDir>/logs/athena.log (default os.Stderr).
	Stderr io.Writer
	// CodexBinary is the codex executable (`athena server --codex`), replacing the configuration's
	// runtimes.codex.binary; empty keeps it.
	CodexBinary string
	// Version is Athena's version, sent to GitHub in the User-Agent (default "dev").
	Version string
	// HTTPClient reaches GitHub (default: a client with a 30 s timeout); its transport is wrapped for the metrics.
	HTTPClient *http.Client

	// dispatcherReady, when set (tests), is called with the Dispatcher before the control socket serves.
	dispatcherReady func(*Dispatcher)
	// web are the tests' ways into the dashboard's endpoint.
	web webHooks
}

// Run serves until ctx is done, then shuts down: it stops the dashboard's endpoint (its listener, its connections, the
// push Notifier and the hub), stops accepting control connections, stops the Sources (and
// waits for their last poll), the Evals' ticker and the router, drains the effects runner (waiting effects resume on the next start), closes the node
// (sessions end and resume on the next start), waits for the node socket to close and closes the store.
//
// Wiring order, so that nothing waits on what is not there yet: store → configuration → GitHub App (checked) → Policy
// store (the default allows the installation's account) → node (with the App's credentials) → effects runner →
// Engine (Wake is the runner's, relays whenever the App exists) → GitHub Source (once the check passed) → Outbox
// (reply) → router (assessment, relay, notice) → node socket → startup recovery (Evals, a pending Policy change,
// Assignments) → runner, Sources, router, ticker. A check that failed is retried in the background (30 s doubling to
// 10 min); when it passes the Source starts and the default Policy allows the installation's account. With web.enabled
// the dashboard's listener is bound and its certificate and push key prepared before the runner starts (the Pusher is
// one of its handlers), and it serves once the Dispatcher exists, before the control socket does.
func Run(ctx context.Context, cfg Config) (err error) {
	if cfg.StateDir == "" {
		return errors.New("server: StateDir is required")
	}
	stateDir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("server: resolve state dir: %w", err)
	}
	if err := ensurePrivateDir(stateDir); err != nil {
		return err
	}
	ctrlPath := filepath.Join(stateDir, ControlSocket)
	if err := claimSocket(ctrlPath); err != nil {
		return err
	}

	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	logger, closeLog, err := openLog(stateDir, stderr)
	if err != nil {
		return err
	}
	defer closeLog()
	// The node logs through the default logger it finds when it is created.
	prevLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prevLogger)

	pidPath := filepath.Join(stateDir, PIDFile)
	_ = os.Remove(pidPath)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("server: write pid file: %w", err)
	}
	defer os.Remove(pidPath)

	st, err := store.Open(filepath.Join(stateDir, DBFile))
	if err != nil {
		return err
	}
	defer st.Close()
	// One registry for the whole server: the store, the Engine and the node record into it.
	reg := metrics.NewRegistry()
	st.SetMetrics(reg)
	resCtx, resCancel := context.WithCancel(context.Background())
	resDone := make(chan struct{})
	go func() {
		defer close(resDone)
		sampleResources(resCtx, reg)
	}()
	defer func() {
		resCancel()
		<-resDone
	}()

	fileCfg, err := LoadConfig(stateDir)
	if err != nil {
		return err
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	sourceReg := sources.NewRegistry()
	var gh *gitHub
	var allowed []string
	if fileCfg.GitHub != nil {
		gh = setupGitHub(fileCfg.GitHub, version, reg, cfg.HTTPClient, logger)
		if account, ok := gh.checked(); ok {
			allowed = []string{account}
		}
	}
	// The router and the relays run whenever the App exists, even before its check passed (see gitHub).
	routerOn := gh != nil && gh.app != nil
	// Nobody configured a Policy yet: the default allows the installation's account (nobody without GitHub).
	pc, err := newPolicyCache(st, allowed, sourceReg, fileCfg.EvalTimeout())
	if err != nil {
		return err
	}
	if _, ok := gh.checked(); gh != nil && !ok {
		pc.awaitCheck()
	}

	ncfg := cfg.Node
	ncfg.Metrics = reg
	if gh != nil {
		ncfg.Credentials = gh.creds
	}
	ncfg.StateDir = stateDir
	ncfg.WorkerPrompt = func() string { return currentPrompt(st, logger, true) }
	ncfg.ReviewerPrompt = func() string { return currentReviewerPrompt(st, logger) }
	ncfg.SocketPath = filepath.Join(stateDir, NodeSocket)
	if ncfg.Sandbox == nil {
		ncfg.Sandbox = fileCfg.nodeSandbox()
	}
	if ncfg.Sandbox == nil {
		logger.Warn("claude code sandbox is off for workers: their commands can write anywhere this user can")
	} else {
		logger.Info("claude code sandbox is on for workers", "required", ncfg.Sandbox.Required,
			"weakerNetworkIsolation", ncfg.Sandbox.WeakerNetworkIsolation,
			"allowedDomains", append(slices.Clone(node.GitHubDomains), ncfg.Sandbox.AllowedDomains...))
	}
	if ncfg.WorkRoot == "" {
		ncfg.WorkRoot = filepath.Join(stateDir, "work")
	}
	if ncfg.WorkRoot, err = filepath.Abs(ncfg.WorkRoot); err != nil {
		return fmt.Errorf("server: resolve work root: %w", err)
	}
	if ncfg.AthenaBin == "" {
		if ncfg.AthenaBin, err = executable(); err != nil {
			return err
		}
	}
	if fileCfg.Runtimes.Codex.On() {
		if err := checkCodexApprovalTimeout(ncfg.ApprovalTimeout); err != nil {
			return err
		}
		// The node runs Codex Tickets on it. Its config.toml is what `athena codex status|login` render too: the hooks are
		// checked for the node's own work root and the session names the server's version, neither of which is in it.
		cx := CodexRuntime(stateDir, fileCfg, ncfg.AthenaBin, cfg.CodexBinary)
		cx.WorkRoot, cx.Version = ncfg.WorkRoot, version
		runtimes := node.DefaultRuntimes(ncfg)
		for name, r := range ncfg.Runtimes {
			runtimes[name] = r
		}
		runtimes[cx.Name()] = cx
		ncfg.Runtimes = runtimes
		logger.Info("codex is configured; `athena codex status` checks it", "binary", cx.Binary, "home", cx.Home)
	} else {
		logger.Info("codex is disabled (runtimes.codex.enabled is false)")
	}
	if ncfg.Chooser == nil {
		ncfg.Chooser = fileCfg.Runtimes.chooser()
	}
	logger.Info("runtimes", "default", fileCfg.Runtimes.Default, "preferred", fileCfg.Runtimes.Preferred,
		"avoidAbove", fileCfg.Runtimes.AvoidAbove)

	// The hub, made once the Dispatcher exists, hears of the node's changes through hubRef.
	var hubRef atomic.Pointer[hub.Hub]
	if fileCfg.WebOn() {
		onStatus := ncfg.OnStatus
		ncfg.OnStatus = func() {
			if onStatus != nil {
				onStatus()
			}
			if h := hubRef.Load(); h != nil {
				h.NodeChanged()
			}
		}
	}

	// The Node needs the Engine and the Engine its Executor: the Node is made first and given the Engine after.
	n := node.New(ncfg, nil)
	nodeClosed := false
	closeNode := func() {
		if !nodeClosed {
			nodeClosed = true
			n.Close()
		}
	}
	defer closeNode()
	emit := func(e core.Event) {
		attrs := []any{"type", e.Type, "ticket", e.TicketID, "assignment", e.AssignmentID}
		if e.ApprovalID != "" {
			attrs = append(attrs, "approval", e.ApprovalID)
		}
		logger.Info("event", attrs...)
	}
	fx := effects.New(st, logger.With("component", "effects"))
	eng := core.NewEngineWith(st.Repos(), n, core.Options{Now: time.Now, Emit: emit, Policy: pc.policy, Scope: sourceReg,
		Wake: fx.Wake, Relays: routerOn, DefaultRuntime: fileCfg.Runtimes.Default, Pairing: pc.pairing})
	eng.SetMetrics(reg)
	n.SetEngine(eng)
	// registerSource registers the GitHub Source once the App passed its check: here, or later in the background.
	registerSource := func() {
		src := github.NewSource(github.SourceConfig{Repos: fileCfg.GitHub.Repos, Interval: fileCfg.GitHub.interval(),
			ReceiveAll: fileCfg.GitHub.ReceiveAll}, gh.app,
			intake{Engine: eng, hist: reg.Histogram("trigger_intake_seconds")}, st, logger.With("source", github.SourceName))
		sourceReg.Register(timedSource{Source: src, hist: reg.Histogram("reply_send_seconds")})
		gh.attach(src)
	}
	if _, ok := gh.checked(); routerOn && ok {
		registerSource()
	}
	fx.Handle(core.EffectReply, outbox.Handler(sourceReg, st), outbox.Options)
	var host *router.Host
	if routerOn {
		host = router.New(router.Config{Bin: ncfg.AthenaBin, StateDir: stateDir, Model: fileCfg.Router.Model,
			Socket: ncfg.SocketPath, ClaudeBinary: ncfg.ClaudeBinary, KV: st, Policy: pc.settings,
			Prompt: func() string { return currentPrompt(st, logger, false) }}, eng, fx, sourceReg, logger)
		n.SetRouter(host)
	}
	var webEP *webEndpoint
	if fileCfg.WebOn() {
		if webEP, err = newWebEndpoint(stateDir, *fileCfg.Web, st, eng, fx, cfg.web, logger); err != nil {
			return err
		}
		defer webEP.stop()
	}
	expirePendingApprovals(eng, logger)

	nodeCtx, nodeCancel := context.WithCancel(context.Background())
	defer nodeCancel()
	served := make(chan error, 1)
	go func() { served <- n.Serve(nodeCtx) }()
	// The dashboard shows each runtime's readiness from the start, not only after its first launch.
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		n.RefreshRuntimes(nodeCtx)
	}()
	defer func() { nodeCancel(); <-refreshed }()
	nodeDone := false
	waitNode := func() error {
		if nodeDone {
			return nil
		}
		nodeDone = true
		return <-served
	}
	defer func() {
		closeNode()
		nodeCancel()
		if serr := waitNode(); serr != nil && err == nil {
			err = serr
		}
	}()
	if err := waitNodeSocket(ncfg.SocketPath, served, &nodeDone); err != nil {
		return err
	}

	// Startup recovery: Evals keep their state (a delivered one its deadline) and an undelivered one its assessment
	// effect; the runner resumes the waiting effects; interrupted Assignments start again.
	resumeAssessments(eng, fx, logger)
	workCtx, workCancel := context.WithCancel(context.Background())
	evalDone := make(chan struct{})
	var late sync.WaitGroup
	stopWork := func() {
		workCancel()
		// The Sources stop before the effects drain and the store closes: no Receive after.
		late.Wait()
		sourceReg.Wait()
		if host != nil {
			host.Stop()
		}
		<-evalDone
		if err := fx.Drain(drainTimeout); err != nil {
			logger.Warn("effects: drain", "error", err)
		}
	}
	// The runner stops with Drain, which lets running effects finish first; the other parts stop with workCtx.
	fx.Start(context.Background())
	recoverAssignments(eng, n, logger)
	sourceReg.Start(workCtx)
	if _, ok := gh.checked(); routerOn && !ok {
		// GitHub was unreachable or refused the check: try again in the background; the Source starts once it passes.
		late.Add(1)
		go func() {
			defer late.Done()
			gh.retryCheck(workCtx, logger, func(account string) {
				if err := pc.allow([]string{account}, func(from, to policy.Settings) (map[string]string, error) {
					return tellEngine(eng, from, to)
				}); err != nil {
					logger.Error("policy: allow the installation account", "error", err)
				}
				registerSource()
				sourceReg.Start(workCtx)
			})
		}()
	}
	if host != nil {
		if err := host.Start(workCtx); err != nil {
			logger.Error("router: not started", "error", err)
		}
	}
	go func() {
		defer close(evalDone)
		evaluate(workCtx, eng, logger)
	}()
	workStopped := false
	defer func() {
		if !workStopped {
			stopWork()
		}
	}()

	l, err := net.Listen("unix", ctrlPath)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", ctrlPath, err)
	}
	defer l.Close()
	if err := os.Chmod(ctrlPath, 0o600); err != nil {
		return fmt.Errorf("server: chmod %s: %w", ctrlPath, err)
	}
	logger.Info("athena server started", "stateDir", stateDir, "pid", os.Getpid())

	c := &control{stateDir: stateDir, eng: eng, workRoot: ncfg.WorkRoot, model: ncfg.Model, log: logger, reg: reg,
		st: st, fx: fx, gh: gh, host: host, policy: pc, node: n}
	if err := c.resumePolicyChange(); err != nil {
		logger.Error("startup: tell the pending policy change", "error", err)
	}
	if webEP != nil {
		// The dashboard lists the local git repositories under localRepoRoots, never the state directory's own.
		home := cfg.web.home
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		c.local = newLocalRepos(fileCfg.localRepoRoots(home), []string{stateDir, ncfg.WorkRoot}, logger)
		c.facts = newPlaceFacts()
		if err := webEP.start(c, n, fx, version, &hubRef); err != nil {
			return err
		}
		c.web = webEP
		if cfg.web.ready != nil {
			cfg.web.ready(webEP)
		}
	}
	if cfg.dispatcherReady != nil {
		cfg.dispatcherReady(c)
	}
	ctrlCtx, ctrlCancel := context.WithCancel(context.Background())
	defer ctrlCancel()
	var conns sync.WaitGroup
	conns.Add(1)
	go func() {
		defer conns.Done()
		c.pruneResults(ctrlCtx)
	}()
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := l.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				logger.Warn("control: accept", "error", err)
				time.Sleep(acceptBackoff)
				continue
			}
			conns.Add(1)
			go func() {
				defer conns.Done()
				c.serveConn(ctrlCtx, conn)
			}()
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case serr := <-served:
		nodeDone = true
		runErr = fmt.Errorf("server: node socket stopped: %v", serr)
	}
	logger.Info("athena server stopping")
	// The dashboard's connections end first: they act through the Dispatcher, and the hub reads the store.
	webEP.stop()
	l.Close()
	<-acceptDone
	ctrlCancel()
	conns.Wait()
	workStopped = true
	stopWork()
	// The deferred steps close the node, wait for its socket and close the store, in that order.
	return runErr
}

// expirePendingApprovals expires every Approval still pending from a previous run: the hook that waited for it died
// with that run's sessions, so nobody can act on an answer.
func expirePendingApprovals(eng *core.Engine, log *slog.Logger) {
	pending, err := eng.PendingApprovals()
	if err != nil {
		log.Error("startup: list pending approvals", "error", err)
		return
	}
	for _, a := range pending {
		if err := eng.ExpireApproval(a.ID); err != nil {
			log.Error("startup: expire approval", "ticket", a.TicketID, "assignment", a.AssignmentID, "approval", a.ID, "error", err)
			continue
		}
		log.Info("expired approval from a previous run", "ticket", a.TicketID, "assignment", a.AssignmentID, "approval", a.ID)
	}
}

// recoverAssignments starts again, of every open Ticket, a pending latest Assignment and a running one that was
// interrupted: mid-work (no turn of its worker ended idle), sent an Input after its turn last ended, with Inputs
// still waiting, or recorded as interrupted by a shutdown that no start has resumed since, unless it waits for a person
// after a failed or blocked report. One that was cut short without a shutdown (a crash) gets its interruption recorded
// first. The node resumes a running one's session with a restart or recovery note and sends it the Inputs that still
// wait (Ticket.Undelivered). A running Assignment idle
// since its last turn (one whose worker handed off included) keeps its Session without a process: the node resumes
// it, with the resume note, when its next Input arrives.
func recoverAssignments(eng *core.Engine, n *node.Node, log *slog.Logger) {
	tickets, err := eng.Tickets(false)
	if err != nil {
		log.Error("recovery: list tickets", "error", err)
		return
	}
	for _, t := range tickets {
		a := t.LatestAssignment()
		if a == nil || (a.State != core.AssignmentRunning && a.State != core.AssignmentPending) {
			continue
		}
		if a.State == core.AssignmentRunning && !interrupted(&t, a) {
			log.Info("idle assignment resumes on its next input", "ticket", t.ID, "assignment", a.ID)
			continue
		}
		if a.State == core.AssignmentRunning && a.OpenInterruption() == nil && busyMidWork(a) {
			// The server died without a shutdown (a crash, a kill): nobody recorded that the session was cut short.
			// Record it now, so the Ticket shows it and the node resumes the session with the restart note.
			if err := eng.AssignmentInterrupted(t.ID, a.ID, core.InterruptRestart); err != nil {
				log.Error("recovery: record interruption", "ticket", t.ID, "assignment", a.ID, "error", err)
			} else if fresh, _, err := eng.Ticket(t.ID); err == nil && fresh.LatestAssignment() != nil && fresh.LatestAssignment().ID == a.ID {
				t, a = fresh, fresh.LatestAssignment()
			}
		}
		log.Info("recovering assignment", "ticket", t.ID, "assignment", a.ID, "state", a.State, "undelivered", len(t.Undelivered))
		n.Start(t, *a)
	}
}

// interrupted reports whether a running Assignment was busy when the server stopped: no turn of its worker ended
// since it started, an Input (or the nudge to report) reached it after its turn last ended, Inputs wait for it, or a
// shutdown recorded an interruption that no start has closed and no turn ended after.
// A Ticket whose latest Report is failed or blocked, with no person's message since, waits for a person instead: a
// failed turn may skip the Stop hook, but nothing is to run until someone answers (plan 1 R34).
func interrupted(t *core.Ticket, a *core.Assignment) bool {
	if t.WaitsForPerson() {
		return false
	}
	if i := a.OpenInterruption(); i != nil && (a.IdleAt == nil || !a.IdleAt.After(i.At)) {
		return true
	}
	return busyMidWork(a) || len(t.Undelivered) > 0
}

// busyMidWork reports whether a running Assignment's worker had a turn going: none ended since it started, or an Input
// (or the nudge to report) reached it after its turn last ended.
func busyMidWork(a *core.Assignment) bool {
	return a.IdleAt == nil || (a.DeliveredAt != nil && a.DeliveredAt.After(*a.IdleAt))
}

// ensurePrivateDir creates dir 0700, or tightens an existing one that others may enter.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("server: create %s: %w", dir, err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("server: %s is not a directory", dir)
	}
	if st.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("server: chmod %s: %w", dir, err)
		}
	}
	return nil
}

// claimSocket makes the control socket path free: a server answering there is ErrAlreadyRunning, a socket nobody
// listens on is removed, and anything else is left alone and refused.
func claimSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("server: %s exists and is not a socket", path)
	}
	conn, err := net.DialTimeout("unix", path, pingTimeout)
	if err == nil {
		defer conn.Close()
		if ping(conn) {
			return ErrAlreadyRunning
		}
		return fmt.Errorf("server: %s is in use by another process", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("server: remove stale socket: %w", err)
	}
	return nil
}

// ping sends a ping on a connection and reports whether an OK came back.
func ping(conn net.Conn) bool {
	_ = conn.SetDeadline(time.Now().Add(pingTimeout))
	if err := json.NewEncoder(conn).Encode(map[string]string{"op": OpPing}); err != nil {
		return false
	}
	var r struct {
		OK bool `json:"ok"`
	}
	return json.NewDecoder(conn).Decode(&r) == nil && r.OK
}

// waitNodeSocket waits until the node socket answers a ping, or fails if Serve returned.
func waitNodeSocket(path string, served <-chan error, done *bool) error {
	deadline := time.Now().Add(socketWait)
	for {
		select {
		case err := <-served:
			*done = true
			if err == nil {
				err = errors.New("stopped")
			}
			return fmt.Errorf("server: node socket: %w", err)
		default:
		}
		if conn, err := net.DialTimeout("unix", path, pingTimeout); err == nil {
			ok := ping(conn)
			conn.Close()
			if ok {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server: node socket %s does not answer", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// openLog opens <stateDir>/logs/athena.log (dir 0700, file 0600) and returns a JSON logger writing to it and stderr.
func openLog(stateDir string, stderr io.Writer) (*slog.Logger, func(), error) {
	dir := filepath.Join(stateDir, LogDir)
	if err := ensurePrivateDir(dir); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, LogFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("server: open log: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("server: chmod log: %w", err)
	}
	h := slog.NewJSONHandler(io.MultiWriter(f, stderr), nil)
	return slog.New(h), func() { f.Close() }, nil
}

// executable returns this program's path with symlinks resolved.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("server: locate athena binary: %w", err)
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe, nil
}
