// Package router runs Athena's router: one long-lived headless Claude Code session with no built-in tools and only the
// router's MCP tools (reference 007 "Main Session tools", "MasterAssessor", "Messages delivered to Sessions"). Candidates,
// relays and notices reach it through durable effects; its tool calls come back through the node socket's router role.
package router

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/runtime/claude"
	"github.com/danielpina1/athena/internal/sources"
	"github.com/danielpina1/athena/prompts"
)

const (
	// sessionKey is the kv key of the router session's id, resumed on the next start.
	sessionKey   = "router.session"
	defaultModel = "haiku"
	// maxRestarts is plan 1's crash policy: a session that died on its own is resumed this many times in a row, with
	// a pause of n seconds before restart n. Past it, the router starts a fresh session instead, since the resumed one
	// may be what crashes; a turn that ends resets the count.
	maxRestarts = 3
	// noticeHold is how long a held notice waits before its handler looks at it again: it never sends one alone, so
	// this only bounds how long the row sleeps; a relay or candidate carries it out sooner.
	noticeHold = time.Hour
	// freshPause is the restartDelay step before a fresh session replaces one that crashed repeatedly.
	freshPause = 10
	// maxNotices bounds the held notices one message carries, oldest first; the rest ride the next one.
	maxNotices = 20
	// tokenBytes is the size of a router start's token.
	tokenBytes = 32
	// recoveryNote opens the message that resends, to a restarted session, what the crashed one may not have taken in.
	recoveryNote = "Your previous process ended unexpectedly; these messages may not have reached you."
	// sessionWait is how long a message waits for the session to be up (a start or a restart) before its effect fails
	// and backs off.
	sessionWait = 5 * time.Second
)

// assessmentConcurrency is how many candidates are prepared at once (plan 2 Task 12).
const assessmentConcurrency = 4

// effectBackoff is the retry of the router's effects (reference 007 Effects: 2 s doubling to 60 s).
var effectBackoff = effects.Backoff{First: 2 * time.Second, Max: 60 * time.Second}

var errNotRunning = errors.New("router: the router session is not running")

// KV is where the router keeps its session id; *store.Store is one.
type KV interface {
	KVGet(key string) (string, bool, error)
	KVSet(key, value string) error
}

// Config configures the router Host.
type Config struct {
	// Bin is the athena binary the session's hooks and MCP server run (default: this executable).
	Bin string
	// StateDir is Athena's state directory: the session runs in <StateDir>/router and its transcript is
	// <StateDir>/transcripts/router.jsonl.
	StateDir string
	// Model is the router's model (default "haiku").
	Model string
	// Socket is the node socket, which serves the router role.
	Socket string
	// ClaudeBinary is the claude executable (default "claude").
	ClaudeBinary string
	// KV keeps the session id (kv router.session); required.
	KV KV
	// Policy returns the current Policy settings, whose enabled rules an assessment judges; nil gives none.
	Policy func() (policy.Settings, error)
}

// Host runs the router session and serves its tools. Its methods are safe for concurrent use.
type Host struct {
	cfg Config
	eng *core.Engine
	fx  *effects.Runner
	reg *sources.Registry
	log *slog.Logger

	// restartDelay is the pause before restart n (1-based) of a crashed session.
	restartDelay func(n int) time.Duration

	mu      sync.Mutex
	token   string // the running process's token; empty while none runs
	sess    *claude.Session
	up      chan struct{} // closed while sess is set; replaced when it is cleared
	stopped bool
	health  error
	cancel  context.CancelFunc
	done    chan struct{}

	// sendMu makes one message at a time: the held notices it takes ride exactly one message, and a starting
	// session's resend goes before any other.
	sendMu sync.Mutex

	// unMu guards unconfirmed, the messages that may not be in the session's conversation yet (unconfirmed.go).
	unMu        sync.Mutex
	unconfirmed []sentMsg

	// windows caches, by Eval id, the Thread window an assessment loaded.
	winMu   sync.Mutex
	windows map[string]window
}

// New returns a Host and registers its effect handlers on fx: assessment (four at once) and relay (ordered), which
// send one message each, and notice (ordered), which holds its notices for the next message; all retry after 2 s
// doubling to 60 s.
func New(cfg Config, eng *core.Engine, fx *effects.Runner, reg *sources.Registry, log *slog.Logger) *Host {
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.ClaudeBinary == "" {
		cfg.ClaudeBinary = "claude"
	}
	if cfg.Bin == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.Bin = exe
		}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &Host{cfg: cfg, eng: eng, fx: fx, reg: reg, log: log.With("component", "router"),
		restartDelay: func(n int) time.Duration { return time.Duration(n) * time.Second },
		windows:      map[string]window{}, up: make(chan struct{})}
	// Candidates load their Thread's window (a Source read) before their message: four at once, so one slow or failing
	// read holds no other back; the messages themselves still go one at a time (sendMu). Relays and notices keep their
	// order.
	fx.Handle(core.EffectAssessment, h.handleAssessment, effects.Options{Concurrency: assessmentConcurrency, Backoff: effectBackoff})
	opts := effects.Options{Ordered: true, Backoff: effectBackoff}
	fx.Handle(core.EffectRelay, h.handleRelay, opts)
	fx.Handle(core.EffectNotice, h.handleNotice, opts)
	return h
}

// Start starts the router session, resuming the one kept in kv, and keeps it running until ctx is done or Stop.
func (h *Host) Start(ctx context.Context) error {
	if h.cfg.StateDir == "" || h.cfg.Socket == "" || h.cfg.KV == nil {
		return errors.New("router: StateDir, Socket and KV are required")
	}
	if err := privateDir(h.dir()); err != nil {
		return err
	}
	if err := privateDir(filepath.Join(h.dir(), ".athena")); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done != nil {
		return errors.New("router: already started")
	}
	if err := h.loadUnconfirmed(); err != nil {
		return err
	}
	ctx, h.cancel = context.WithCancel(ctx)
	h.done = make(chan struct{})
	go h.supervise(ctx)
	return nil
}

// Stop ends the router session and waits for it. Effects that wait stay queued for the next start.
func (h *Host) Stop() {
	h.mu.Lock()
	h.stopped = true
	sess, cancel, done := h.sess, h.cancel, h.done
	h.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Health is nil while the router session runs, else why it does not.
func (h *Host) Health() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sess == nil && h.health == nil {
		return errNotRunning
	}
	return h.health
}

// Router states State reports.
const (
	StateRunning  = "running"
	StateStarting = "starting"
	StateCrashed  = "crashed"
	StateStopped  = "stopped"
)

// State says what the router session does now: running (its process is up), starting (Start was called and the
// first process is not up yet), crashed (its process ended on its own; it restarts after a pause) or stopped (never
// started, or Stop was called).
func (h *Host) State() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.stopped || h.done == nil:
		return StateStopped
	case h.sess != nil:
		return StateRunning
	case h.health != nil:
		return StateCrashed
	}
	return StateStarting
}

// SessionID is the router session's id as kept in kv (router.session), or empty before its first start.
func (h *Host) SessionID() string {
	id, _, err := h.cfg.KV.KVGet(sessionKey)
	if err != nil {
		return ""
	}
	return id
}

// ValidToken reports whether token is the running router process's (node.RouterHandler).
func (h *Host) ValidToken(token string) bool {
	h.mu.Lock()
	cur := h.token
	h.mu.Unlock()
	return cur != "" && len(token) == len(cur) && subtle.ConstantTimeCompare([]byte(token), []byte(cur)) == 1
}

func (h *Host) dir() string { return filepath.Join(h.cfg.StateDir, "router") }

// supervise runs the session and restarts it when it dies on its own (see maxRestarts).
func (h *Host) supervise(ctx context.Context) {
	defer close(h.done)
	for crashes := 0; ; {
		cause := h.runOnce(ctx, &crashes)
		if h.isStopped() || ctx.Err() != nil {
			return
		}
		crashes++
		h.setHealth(fmt.Errorf("router: session ended: %s", cause))
		pause := h.restartDelay(crashes)
		if crashes > maxRestarts {
			// Resuming keeps failing: the next start is a fresh session, after a longer pause.
			h.log.Error("router session crashed repeatedly; starting a new session", "cause", cause)
			if err := h.cfg.KV.KVSet(sessionKey, ""); err != nil {
				h.log.Error("clear router session id", "error", err)
			}
			pause = h.restartDelay(freshPause)
			crashes = 0
		} else {
			h.log.Warn("router session ended unexpectedly; resuming", "restart", crashes, "cause", cause)
		}
		t := time.NewTimer(pause)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

func (h *Host) isStopped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopped
}

func (h *Host) setHealth(err error) {
	h.mu.Lock()
	h.health = err
	h.mu.Unlock()
}

// runOnce starts one router process with a fresh token, follows its events until it exits, and says why it ended.
// A turn that ends resets crashes. The process first gets the messages that may not have reached the last one.
func (h *Host) runOnce(ctx context.Context, crashes *int) string {
	token, err := newToken()
	if err != nil {
		return err.Error()
	}
	adir := filepath.Join(h.dir(), ".athena")
	settings, mcpConfig := filepath.Join(adir, "settings.json"), filepath.Join(adir, "mcp.json")
	if err := claude.WriteSettings(settings, h.cfg.Bin, h.cfg.Socket, time.Minute); err != nil {
		return "write settings: " + err.Error()
	}
	if err := claude.WriteRouterMCPConfig(mcpConfig, h.cfg.Bin, h.cfg.Socket, token); err != nil {
		return "write mcp config: " + err.Error()
	}
	resume, _, err := h.cfg.KV.KVGet(sessionKey)
	if err != nil {
		return "read session id: " + err.Error()
	}
	noTools := ""
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return "stopped"
	}
	h.token = token
	h.mu.Unlock()
	sess, err := claude.Start(ctx, claude.Options{
		Binary:             h.cfg.ClaudeBinary,
		Dir:                h.dir(),
		Model:              h.cfg.Model,
		ResumeID:           resume,
		SettingsPath:       settings,
		MCPConfigPath:      mcpConfig,
		AppendSystemPrompt: prompts.Router,
		Tools:              &noTools,
		Env: []string{"ATHENA_ROLE=" + node.RoleRouter, "ATHENA_ROUTER_TOKEN=" + token,
			"ATHENA_SOCKET=" + h.cfg.Socket},
		TranscriptPath:     filepath.Join(h.cfg.StateDir, "transcripts", "router.jsonl"),
		ReplayUserMessages: true,
	})
	if err != nil {
		h.clearSession(nil)
		return "start claude: " + err.Error()
	}
	// The resend goes before any waiting message: it is written before the session is published, under sendMu.
	h.sendMu.Lock()
	h.resend(sess)
	h.mu.Lock()
	h.sess, h.health = sess, nil
	close(h.up)
	h.mu.Unlock()
	h.sendMu.Unlock()
	h.log.Info("router session started", "resume", resume != "")
	for ev := range sess.Events() {
		h.observe(ev)
		if e, ok := ev.(claude.Result); ok {
			*crashes = 0
			if e.IsError {
				h.log.Warn("router turn failed", "subtype", e.Subtype, "errors", redact.String(strings.Join(e.Errors, "; ")))
			}
		}
	}
	err = sess.Wait()
	if h.isStopped() && !sess.Killed() {
		// A stop closed the session's input and it exited on its own: the CLI finished the turn it took the messages
		// into, so they are in the conversation and the next start does not send them again.
		h.dropTaken()
	}
	h.clearSession(sess)
	cause := "exited"
	if err != nil {
		cause = err.Error()
	}
	if tail := strings.TrimSpace(redact.String(sess.Stderr())); tail != "" {
		lines := strings.Split(tail, "\n")
		cause += ": " + lines[len(lines)-1]
	}
	return cause
}

// clearSession forgets the session (if it is still the current one) and its token.
func (h *Host) clearSession(sess *claude.Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sess != nil && h.sess == sess {
		h.sess = nil
		h.up = make(chan struct{})
	}
	h.token = ""
}

// session returns the running session, waiting up to sessionWait for one to start.
func (h *Host) session(ctx context.Context) (*claude.Session, error) {
	t := time.NewTimer(sessionWait)
	defer t.Stop()
	for {
		h.mu.Lock()
		sess, up, stopped := h.sess, h.up, h.stopped
		h.mu.Unlock()
		if sess != nil {
			return sess, nil
		}
		if stopped {
			return nil, errNotRunning
		}
		select {
		case <-up:
		case <-t.C:
			return nil, errNotRunning
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (h *Host) keepSession(id string) {
	if id == "" {
		return
	}
	if cur, _, err := h.cfg.KV.KVGet(sessionKey); err == nil && cur == id {
		return
	}
	if err := h.cfg.KV.KVSet(sessionKey, id); err != nil {
		h.log.Error("keep router session id", "error", err)
	}
}

// --- Messages ---

// deliver sends one message to the router session: the held notices first, one JSON document per line, then text.
// The message is kept as unconfirmed before it is written, then the notices it carried are settled; until the session
// takes it into a turn that ends, every start sends it again.
func (h *Host) deliver(ctx context.Context, text string) error {
	// Wait for the session outside sendMu, which a starting session takes for its resend.
	if _, err := h.session(ctx); err != nil {
		return err
	}
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	h.mu.Lock()
	sess := h.sess
	h.mu.Unlock()
	if sess == nil {
		return errNotRunning
	}
	held, err := h.fx.Pending(core.EffectNotice)
	if err != nil {
		return err
	}
	held = held[:min(len(held), maxNotices)]
	var b strings.Builder
	for _, n := range held {
		line, err := h.renderNotice(n.Payload)
		if err != nil {
			return err
		}
		if line != "" {
			b.WriteString(line + "\n")
		}
	}
	b.WriteString(text)
	msg := b.String()
	// Kept before it is written, so its row and the notices it carries can be settled once it is.
	id, err := h.sent(msg)
	if err != nil {
		return err
	}
	if err := sess.SendMessage(msg, id); err != nil {
		h.unsent(id)
		return fmt.Errorf("router: send: %w", err)
	}
	for _, n := range held {
		if _, err := h.fx.Complete(core.EffectNotice, n.Key, n.Payload); err != nil {
			h.log.Error("settle a delivered notice", "key", n.Key, "error", err)
		}
	}
	return nil
}

// handleAssessment steers an Eval's candidate card into the router's turn, then tells the Engine it was delivered,
// which starts the Eval's deadline. An Eval that ended meanwhile is dropped.
func (h *Host) handleAssessment(ctx context.Context, key string, payload []byte) error {
	var p struct {
		EvalID    string `json:"evalId"`
		TriggerID string `json:"triggerId"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		h.log.Error("drop a malformed assessment", "key", key, "error", err)
		return nil
	}
	view, err := h.eng.Candidate(p.TriggerID)
	var ue *core.UserError
	if errors.As(err, &ue) {
		return nil // the Eval ended
	}
	if err != nil {
		return err
	}
	if view.Eval.ID != key {
		return nil // another Eval holds the Trigger now; this one ended
	}
	card, err := h.candidateCard(view)
	if err != nil {
		return err
	}
	if err := h.deliver(ctx, card); err != nil {
		return err
	}
	return h.eng.CandidateDelivered(key)
}

// handleRelay steers a follow-up, a worker's report or a worker's question into the router's turn, rendered from the
// state current now; one that no longer applies is dropped.
func (h *Host) handleRelay(ctx context.Context, key string, payload []byte) error {
	var m core.Message
	if err := json.Unmarshal(payload, &m); err != nil {
		h.log.Error("drop a malformed relay", "key", key, "error", err)
		return nil
	}
	text, err := h.renderRelay(m)
	if err != nil || text == "" {
		return err
	}
	return h.deliver(ctx, text)
}

// handleNotice holds a notice: it never wakes the router alone, and the next relay or candidate carries it.
func (h *Host) handleNotice(context.Context, string, []byte) error { return effects.Later(noticeHold) }

// --- Helpers ---

func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("router: token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// privateDir creates dir 0700, or makes an existing one 0700.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("router: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("router: chmod %s: %w", dir, err)
	}
	return nil
}
