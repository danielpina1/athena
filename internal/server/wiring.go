package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/fleet"
	"github.com/danielpina1/athena/internal/hub"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/outbox"
	"github.com/danielpina1/athena/internal/pki"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/runtime/codex"
	"github.com/danielpina1/athena/internal/sources"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/danielpina1/athena/internal/sources/schedule"
	"github.com/danielpina1/athena/internal/store"
	"github.com/danielpina1/athena/internal/web"
	"github.com/danielpina1/athena/internal/web/ui"
)

// The parts `athena server` wires between the store and the node: the GitHub App and Source, the Policy, the node's
// credentials, the Codex runtime, the Evals' ticker and the metrics of Trigger intake, replies and GitHub requests.
//
// Token scope: a worker of a GitHub Ticket gets no token in its environment. Its git and gh shims ask the node socket
// for its own Assignment's token, which the node mints through Credentials as a token for the Ticket's repository
// alone with github.WorkerPermissions(); any process of the owner's user that knows the Ticket and Assignment ids can
// ask the same (same-user threat model, spec §11).

const (
	// evaluateEvery is how often the Evals' due work runs while a Thread has Triggers queued or an Eval in progress;
	// the check itself is one store query, and the work calls no model.
	defaultEvaluateEvery = 2 * time.Second
	// checkTimeout bounds the GitHub check at startup.
	checkTimeout = 30 * time.Second
	// identityTimeout bounds a bot lookup for a worker's commit identity.
	identityTimeout = 10 * time.Second
	// httpTimeout bounds one GitHub request.
	httpTimeout = 30 * time.Second
	// drainTimeout is how long running effects get to finish at shutdown.
	drainTimeout = 5 * time.Second
)

// evaluateEvery is defaultEvaluateEvery; tests shorten it.
var evaluateEvery = defaultEvaluateEvery

// checkRetryFirst and checkRetryMax bound the pauses of the background retry of a failed GitHub check (30 s doubling
// to 10 min); tests shorten them.
var (
	checkRetryFirst = 30 * time.Second
	checkRetryMax   = 10 * time.Minute
)

// gitHub is the server's GitHub: the App (nil when its key could not be read), the node's credentials, and the Source,
// which runs once the App passed its check (at startup, or later in the background).
//
// The node gets credentials whenever GitHub is configured, so an open GitHub Ticket never fails because GitHub was
// unreachable at boot: its launch waits for a token and retries (node credentialsLater). The router starts, and the
// Engine queues relays, whenever the App exists, before its check passed: relays and notices queue durably and reach
// the router as soon as it runs; candidates only come once the Source runs.
type gitHub struct {
	cfg *GitHubConfig
	app *github.App
	// creds is an appCredentials, or an unavailableCredentials when the App could not be made.
	creds node.Credentials

	mu     sync.Mutex
	source *github.Source
	// account is the installation account's login, which the default Policy allows; set by a passing check.
	account string
	// err says why the Source does not run (the latest check failure); nil once the check passed.
	err error
}

// setupGitHub makes the App and checks it once, as `athena github check` does. A failure is kept in err (status shows
// it); the server runs on, and retryCheck tries again.
func setupGitHub(cfg *GitHubConfig, version string, reg *metrics.Registry, hc *http.Client, log *slog.Logger) *gitHub {
	g := &gitHub{cfg: cfg}
	if hc == nil {
		hc = &http.Client{Timeout: httpTimeout}
	}
	hc = instrument(hc, reg)
	app, err := github.NewApp(cfg.appConfig(version), hc, nil)
	if err != nil {
		g.err = err
		g.creds = unavailableCredentials{err: err}
		log.Error("github: the app cannot be used; fix the configuration and restart", "error", err)
		return g
	}
	g.app = app
	g.creds = &appCredentials{app: app}
	g.check(log)
	return g
}

// check runs the GitHub check once; a pass records the account and seeds the credentials with the bot.
func (g *gitHub) check(log *slog.Logger) bool {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	c, err := checkApp(ctx, g.app, g.cfg)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.err = err
		log.Error("github: check failed; the source waits and the check is retried", "error", err)
		return false
	}
	g.err, g.account = nil, c.Account
	if ac, ok := g.creds.(*appCredentials); ok {
		ac.seed(c.Bot, c.BotID)
	}
	log.Info("github: app checked", "bot", c.Bot, "account", c.Account, "repos", strings.Join(g.cfg.Repos, ","))
	return true
}

// checked reports whether the check passed, with the installation account; false without GitHub (g nil).
func (g *gitHub) checked() (account string, ok bool) {
	if g == nil {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.account, g.app != nil && g.err == nil
}

// attach records the Source once it is registered.
func (g *gitHub) attach(src *github.Source) {
	g.mu.Lock()
	g.source = src
	g.mu.Unlock()
}

// running reports whether the Source is registered.
func (g *gitHub) running() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.source != nil
}

// retryCheck retries a failed check every checkRetryFirst doubling to checkRetryMax until it passes, then calls
// passed, or until ctx is done.
func (g *gitHub) retryCheck(ctx context.Context, log *slog.Logger, passed func(account string)) {
	for d := checkRetryFirst; ; d = min(2*d, checkRetryMax) {
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if g.check(log) {
			account, _ := g.checked()
			passed(account)
			return
		}
	}
}

// health is the GitHub Source's health: why it does not run, or the Source's own.
func (g *gitHub) health() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	if g.source == nil {
		return fmt.Errorf("github: not started")
	}
	return g.source.Health()
}

// instrument returns a copy of hc whose transport counts the requests by status (github_requests_total) and keeps
// the installation's rate-limit remaining (github_rate_remaining).
func instrument(hc *http.Client, reg *metrics.Registry) *http.Client {
	c := *hc
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = &countingTransport{base: base, reg: reg}
	return &c
}

type countingTransport struct {
	base http.RoundTripper
	reg  *metrics.Registry
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if t.reg == nil {
		return res, err
	}
	status := "error"
	if err == nil {
		status = strconv.Itoa(res.StatusCode)
	}
	t.reg.Counter("github_requests_total", "status", status).Add(1)
	// The App's own (JWT) requests have a limit of their own: only installation requests set the gauge.
	if err == nil && !strings.Contains(req.URL.Path, "/app/installations/") {
		if v, perr := strconv.ParseInt(res.Header.Get("x-ratelimit-remaining"), 10, 64); perr == nil {
			t.reg.Gauge("github_rate_remaining").Set(float64(v))
		}
	}
	return res, err
}

// appCredentials gives the node a GitHub Ticket's credentials through the App: a token for the Ticket's repository
// with the workers' permissions, and the bot as the commit identity.
type appCredentials struct {
	app *github.App

	mu           sync.Mutex
	login, email string
}

// Token mints a token for repo alone, with github.WorkerPermissions(), or github.ReaderPermissions() unless write.
func (c *appCredentials) Token(ctx context.Context, repo string, write bool) (string, time.Time, error) {
	perms := github.ReaderPermissions()
	if write {
		perms = github.WorkerPermissions()
	}
	t, err := c.app.ScopedToken(ctx, repo, perms)
	if err != nil {
		return "", time.Time{}, err
	}
	return t.Value, t.ExpiresAt, nil
}

var (
	_ node.TokenRevoker = (*appCredentials)(nil)
	_ node.PullOpener   = (*appCredentials)(nil)
)

// OpenPull opens a pull request the owner applied (node.PullOpener): through the API, with a write token minted for
// this call alone and revoked after.
func (c *appCredentials) OpenPull(ctx context.Context, repo, head string, p node.HeldPull) (int, string, error) {
	t, err := c.app.ScopedToken(ctx, repo, github.WorkerPermissions())
	if err != nil {
		return 0, "", err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.app.RevokeToken(rctx, t.Value); err != nil {
			slog.Warn("revoke the token of an opened pull request; it expires on its own", "repo", repo, "error", err)
		}
	}()
	return c.app.CreatePull(ctx, t.Value, repo, github.NewPull{Title: p.Title, Body: p.Body, Base: p.Base, Head: head,
		Draft: p.Draft})
}

// Revoke revokes a worker's token the node dropped (node.TokenRevoker).
func (c *appCredentials) Revoke(ctx context.Context, token string) error {
	return c.app.RevokeToken(ctx, token)
}

// seed records the bot the check fetched, so Identity needs no lookup of its own.
func (c *appCredentials) seed(login string, id int64) {
	if login == "" {
		return
	}
	c.mu.Lock()
	c.login, c.email = login, fmt.Sprintf("%d+%s@users.noreply.github.com", id, login)
	c.mu.Unlock()
}

// unavailableCredentials stand in when the App could not be made (its key unreadable): every request fails as one
// that may pass, so the node's GitHub launches wait instead of failing their Tickets, until a restart with a fixed
// configuration.
type unavailableCredentials struct{ err error }

func (u unavailableCredentials) Token(context.Context, string, bool) (string, time.Time, error) {
	return "", time.Time{}, fmt.Errorf("github app unavailable: %v", u.err)
}

func (unavailableCredentials) Identity() (string, string) { return "", "" }

// Identity is the bot's login and its noreply address, <id>+<login>@users.noreply.github.com; it is looked up once
// and remembered after the first success (empty until then).
func (c *appCredentials) Identity() (login, email string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.login != "" {
		return c.login, c.email
	}
	ctx, cancel := context.WithTimeout(context.Background(), identityTimeout)
	defer cancel()
	l, id, err := c.app.Bot(ctx)
	if err != nil || l == "" {
		return "", ""
	}
	c.login, c.email = l, fmt.Sprintf("%d+%s@users.noreply.github.com", id, l)
	return c.login, c.email
}

// intake is the Sources' view of the Engine; it times each Receive (trigger_intake_seconds).
type intake struct {
	*core.Engine
	hist *metrics.Histogram
}

func (i intake) Receive(t core.Trigger) (string, error) {
	start := time.Now()
	res, err := i.Engine.Receive(t)
	i.hist.Observe(time.Since(start))
	return res, err
}

var (
	_ github.Intake   = intake{}
	_ schedule.Intake = intake{}
)

// timedSource is the GitHub Source as registered: every part of it, its Channel's sends timed (reply_send_seconds).
type timedSource struct {
	*github.Source
	hist *metrics.Histogram
}

func (s timedSource) Send(ctx context.Context, ch core.ChannelRef, req sources.SendRequest) (sources.SendResult, error) {
	start := time.Now()
	res, err := s.Source.Send(ctx, ch, req)
	s.hist.Observe(time.Since(start))
	return res, err
}

var (
	_ sources.Source        = timedSource{}
	_ sources.Channel       = timedSource{}
	_ sources.ThreadContext = timedSource{}
	_ sources.References    = timedSource{}
)

// policyCache holds the current Policy settings and the Policy built from them. The Engine reads the Policy inside
// its transactions, where the settings store cannot be read (transactions do not nest): the cache is filled at
// startup and replaced by each save, outside any transaction.
type policyCache struct {
	st *store.Store
	// store is replaced by allow (under saveMu and mu).
	store   *policy.Store
	scope   core.SourceScope
	timeout time.Duration

	saveMu sync.Mutex // one save at a time
	mu     sync.RWMutex
	cur    policy.Settings
	rev    int
	pol    core.Policy
	// unchecked is set while GitHub is configured and its startup check has not passed: the defaults do not know the
	// installation's account yet. While nobody saved settings, the Engine then has no Policy (see policy).
	unchecked bool
}

func newPolicyCache(st *store.Store, allowed []string, scope core.SourceScope, timeout time.Duration) (*policyCache, error) {
	c := &policyCache{st: st, store: policy.NewStore(st, policy.Defaults(allowed)), scope: scope, timeout: timeout}
	s, rev, err := c.store.Current()
	if err != nil {
		return nil, err
	}
	c.set(s, rev)
	return c, nil
}

func (c *policyCache) set(s policy.Settings, rev int) {
	c.mu.Lock()
	c.cur, c.rev, c.pol = s, rev, policy.New(s, c.scope, c.timeout)
	c.mu.Unlock()
}

// policy is the Engine's Policy getter. While nobody saved settings and the startup check has not passed, the defaults
// cannot know the installation's account and would allow nobody: there is no Policy then, so the Engine evaluates
// nothing and decides nothing (R36), and queued Triggers wait for the check instead of being recorded ignored.
func (c *policyCache) policy() core.Policy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.rev == 0 && c.unchecked {
		return nil
	}
	return c.pol
}

// nodePolicy is the node's Policy (node.Policy) over the cache: each call reads the current Policy, so a saved change
// reaches the next session the node prepares. It reads the Policy built from the current settings even while the
// Engine has none (policy): their tool policies hold then too. A Policy that is not a node.Policy fails closed:
// restricted, no allow rule, every repository held.
type nodePolicy struct{ c *policyCache }

func (p nodePolicy) current() node.Policy {
	p.c.mu.RLock()
	defer p.c.mu.RUnlock()
	np, _ := p.c.pol.(node.Policy)
	return np
}

func (p nodePolicy) ToolPolicy(class string) string {
	if np := p.current(); np != nil {
		return np.ToolPolicy(class)
	}
	return policy.ToolRestricted
}

func (p nodePolicy) Allows(tool, rule, repo string) bool {
	if np := p.current(); np != nil {
		return np.Allows(tool, rule, repo)
	}
	return false
}

func (p nodePolicy) Shadow(repo string) bool {
	if np := p.current(); np != nil {
		return np.Shadow(repo)
	}
	return true
}

func (p nodePolicy) Trust(t core.Trigger) string {
	if np := p.current(); np != nil {
		return np.Trust(t)
	}
	return core.TrustStranger
}

// pairing is the Engine's pairing defaults: the current settings' (policy.Settings.PairingDefaults).
func (c *policyCache) pairing() core.PairingDefaults {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur.PairingDefaults()
}

// settings is the router's: the current settings.
func (c *policyCache) settings() (policy.Settings, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur, nil
}

// saved reads the current settings and revision from the store: unlike current, it sees a save as soon as it
// commits, before the cache is replaced.
func (c *policyCache) saved() (policy.Settings, int, error) {
	c.mu.RLock()
	st := c.store
	c.mu.RUnlock()
	return st.Current()
}

// current returns the settings and their revision.
func (c *policyCache) current() (policy.Settings, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur, c.rev
}

// allow makes the defaults allow the installation account found by a late check: while nobody saved settings, they
// are the current ones, and the Engine is told of the change.
func (c *policyCache) allow(allowed []string, tell tellFunc) error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.Lock()
	c.store = policy.NewStore(c.st, policy.Defaults(allowed))
	c.mu.Unlock()
	s, rev, err := c.store.Current()
	if err != nil {
		return err
	}
	if rev != 0 {
		c.mu.Lock()
		c.unchecked = false
		c.mu.Unlock()
		return nil
	}
	old, _ := c.current()
	c.mu.Lock()
	c.cur, c.rev, c.pol, c.unchecked = s, 0, policy.New(s, c.scope, c.timeout), false
	c.mu.Unlock()
	_, err = tell(old, s)
	return err
}

// awaitCheck marks the defaults as not knowing the installation's account until allow is called (see policy).
func (c *policyCache) awaitCheck() {
	c.mu.Lock()
	c.unchecked = true
	c.mu.Unlock()
}

// policyChangeKey is the kv key of a settings change saved but not yet told to the Engine.
const policyChangeKey = "policy.changed.pending"

// pendingChange is a settings change from From to To, saved (or about to be) as Revision.
type pendingChange struct {
	From     policy.Settings `json:"from"`
	To       policy.Settings `json:"to"`
	Revision int             `json:"revision"`
}

// tellFunc tells the Engine of a settings change and returns the notices it sent.
type tellFunc func(from, to policy.Settings) (map[string]string, error)

// change saves s as the revision after base (-1: the current one), makes it current and tells the Engine, all under
// one lock, so each change is told from the revision it replaced; a base that is not current is ErrStaleRevision. The
// change is recorded in kv before the save and cleared once told: a crash or a failed tell leaves it there, and the
// next change (or the next start, resume) tells it first.
func (c *policyCache) change(s policy.Settings, base int, origin string, tell tellFunc) (rev int, cur policy.Settings, notices map[string]string, err error) {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	if err := c.resumeLocked(tell); err != nil {
		return 0, cur, nil, fmt.Errorf("policy: tell the previous change: %w", err)
	}
	old, current := c.current()
	if base >= 0 && base != current {
		return 0, cur, nil, policy.ErrStaleRevision
	}
	base = current
	if err := c.writePending(&pendingChange{From: old, To: s, Revision: base + 1}); err != nil {
		return 0, cur, nil, err
	}
	if _, err := c.store.Save(s, base, origin); err != nil {
		if cerr := c.writePending(nil); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return 0, cur, nil, err
	}
	cur, rev, err = c.store.Current()
	if err != nil {
		return 0, cur, nil, err
	}
	c.set(cur, rev)
	if notices, err = tell(old, cur); err != nil {
		return rev, cur, nil, err
	}
	return rev, cur, notices, c.writePending(nil)
}

// resume tells the Engine of a change saved but not told (see change).
func (c *policyCache) resume(tell tellFunc) error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	return c.resumeLocked(tell)
}

func (c *policyCache) resumeLocked(tell tellFunc) error {
	raw, ok, err := c.st.KVGet(policyChangeKey)
	if err != nil || !ok || raw == "" {
		return err
	}
	var p pendingChange
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return c.writePending(nil) // unreadable: nothing to tell
	}
	if _, rev := c.current(); rev < p.Revision {
		return c.writePending(nil) // the save never happened
	}
	if _, err := tell(p.From, p.To); err != nil {
		return err
	}
	return c.writePending(nil)
}

// writePending records p in kv, or clears the record when p is nil.
func (c *policyCache) writePending(p *pendingChange) error {
	if p == nil {
		return c.st.KVSet(policyChangeKey, "")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return c.st.KVSet(policyChangeKey, string(raw))
}

// evaluate runs the Evals' due work now and then every evaluateEvery, only while a Thread has Triggers queued, an
// Eval in progress or a candidate held by the brake, until ctx is done. While paused() it decides nothing.
func evaluate(ctx context.Context, eng *core.Engine, paused func() bool, log *slog.Logger) {
	run := func() {
		if paused != nil && paused() {
			return // the owner paused Athena: Triggers stay queued, nothing is decided
		}
		active, err := eng.ActiveThreads()
		if err != nil {
			log.Error("evaluate: active threads", "error", err)
			return
		}
		if len(active) == 0 {
			// Nothing queued, but a candidate the brake holds waits for the owner to catch up.
			if held, err := eng.HasBrakes(); err != nil || !held {
				if err != nil {
					log.Error("evaluate: brakes", "error", err)
				}
				return
			}
		}
		if err := eng.EvaluateDue(); err != nil {
			log.Error("evaluate: due work", "error", err)
		}
	}
	run()
	t := time.NewTicker(evaluateEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// resumeAssessments makes sure, before the effects runner starts, that every Eval whose candidate has not reached the
// router still has its assessment effect (it was queued with the Eval, durably). A delivered Eval keeps its deadline.
func resumeAssessments(eng *core.Engine, fx *effects.Runner, log *slog.Logger) {
	rows, err := fx.Pending(core.EffectAssessment)
	if err != nil {
		log.Error("startup: list assessment effects", "error", err)
		return
	}
	pending := make(map[string]bool, len(rows))
	for _, r := range rows {
		pending[r.Key] = true
	}
	resumed, err := eng.ResumeAssessments(pending)
	if err != nil {
		log.Error("startup: resume assessments", "error", err)
		return
	}
	for _, id := range resumed {
		log.Warn("startup: queued the missing assessment of an undelivered eval", "eval", id)
	}
}

// --- the dashboard's endpoint ---

// webShutdown bounds the wait for the web requests in flight at shutdown; the rest are cut. The login's requests are
// short, and a connection a browser opened ahead without a request yet would otherwise hold the stop for 5 s.
const webShutdown = 2 * time.Second

// webEndpoint is the dashboard's HTTPS endpoint and what feeds it. newWebEndpoint (before the effects runner starts)
// binds the listener, so a busy port fails the start, prepares the certificate and the push key, and registers the
// Pusher; start (once the Dispatcher exists) runs the hub, the login, the Notifier and the listener. With TLS on it
// also binds the node listener (web.nodeListen) and loads the node CA; with TLS off nodes are not served.
type webEndpoint struct {
	cfg    WebConfig
	origin string
	st     *store.Store
	log    *slog.Logger
	now    func() time.Time

	l        net.Listener
	tls      *web.TLSManager
	vapid    *web.VAPID // nil without an https origin: no push
	pusher   *web.Pusher
	presence *web.Presence
	// The node endpoint: nil and "" with TLS off.
	nodeCA  *pki.CA
	nodeL   net.Listener
	nodeURL string
	// nodeLinks serves a node's link once its certificate is checked: the fleet, set by start (notServingNodes until).
	nodeLinks web.NodeLinkHandler

	// set by start
	nodeSrv *http.Server
	hub     *hub.Hub
	auth    *web.Auth
	dash    *web.Dashboard
	srv     *web.Server
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	stopOnce sync.Once
}

// webHooks are the tests' ways into the endpoint; the zero value is production.
type webHooks struct {
	// pushClient sends the pushes (default: a client with a 20 s timeout).
	pushClient *http.Client
	// now is the login's clock (default time.Now).
	now func() time.Time
	// ready, when set, is called with the endpoint once it serves.
	ready func(*webEndpoint)
	// home is the home directory localRepoRoots expands "~/" against (default os.UserHomeDir()).
	home string
}

func newWebEndpoint(stateDir string, cfg WebConfig, st *store.Store, eng *core.Engine, fx *effects.Runner, hooks webHooks,
	log *slog.Logger) (_ *webEndpoint, err error) {
	log = log.With("component", "web")
	l, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("server: web listen %s: %w", cfg.Listen, err)
	}
	var nodeL net.Listener
	defer func() {
		if err != nil {
			l.Close()
			if nodeL != nil {
				nodeL.Close()
			}
		}
	}()
	// The origin names the bound port: the configured one, or the one the system chose for port 0.
	_, port, _ := net.SplitHostPort(l.Addr().String())
	w := &webEndpoint{cfg: cfg, origin: cfg.origin(port), st: st, log: log, now: hooks.now, l: l,
		presence: web.NewPresence(nil)}
	if w.now == nil {
		w.now = time.Now
	}
	var obt web.Obtainer
	if cfg.TLS == "acme" {
		if obt, err = web.NewLegoObtainer(stateDir, cfg.tlsConfig().ACME); err != nil {
			return nil, err
		}
	}
	if w.tls, err = web.NewTLSManager(stateDir, cfg.tlsConfig(), obt, log, time.Now); err != nil {
		return nil, err
	}
	w.nodeLinks = notServingNodes{}
	if w.tls.Mode() == "off" {
		log.Info("nodes are not served: nodes need web.tls \"ca\" or \"acme\"")
	} else {
		if w.nodeCA, err = pki.LoadOrCreateCA(pki.NodeCADir(stateDir), pki.NodeCAHost, nil); err != nil {
			return nil, err
		}
		addr := cfg.NodeListenAddr()
		if nodeL, err = net.Listen("tcp", addr); err != nil {
			return nil, fmt.Errorf("server: web node listen %s: %w", addr, err)
		}
		w.nodeL = nodeL
		_, nodePort, _ := net.SplitHostPort(nodeL.Addr().String())
		w.nodeURL = "wss://" + net.JoinHostPort(cfg.Host, nodePort) + "/node"
	}
	if strings.HasPrefix(w.origin, "https://") {
		// The tokens' subject is the dashboard's own origin (ruling R27).
		if w.vapid, err = web.LoadOrCreateVAPID(stateDir, w.origin); err != nil {
			return nil, err
		}
		w.pusher = web.NewPusher(web.PusherConfig{Store: st, VAPID: w.vapid, Client: hooks.pushClient, Log: log,
			Stage: func(ticketID string) (core.Stage, error) {
				_, stage, err := eng.Ticket(ticketID)
				return stage, err
			},
			Visible: w.presence.Visible,
		})
		w.pusher.Register(fx)
	}
	return w, nil
}

// start runs the hub over the Dispatcher's reads, the certificate's renewals, the push Notifier and the listener.
func (w *webEndpoint) start(d *Dispatcher, flt *fleet.Fleet, fx *effects.Runner, version string, hubRef *atomic.Pointer[hub.Hub]) error {
	src := newHubSource(d)
	src.tls = w.tls
	w.hub = hub.New(src, w.log)
	auth, err := web.NewAuth(w.st, w.origin, w.cfg.Host, w.now)
	if err != nil {
		return err
	}
	w.auth = auth
	if w.nodeL != nil {
		// The fleet serves each node's link once its certificate is checked.
		w.nodeLinks = flt
		// Made before anything runs, so a failure leaves nothing to stop.
		if w.nodeSrv, err = web.NodeListener(web.NodeListenerConfig{Store: w.st, CA: w.nodeCA,
			GetCertificate: w.tls.Config().GetCertificate, Handler: w.nodeLinks, Log: w.log}); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.st.OnCommit(w.hub.OnCommit)
	hubRef.Store(w.hub)
	w.hub.Start(ctx)
	d.SetAuth(auth)
	w.dash = web.NewDashboard(web.DashboardConfig{Auth: auth, Hub: w.hub, Commander: d, Streams: flt, Presence: w.presence,
		Log: w.log, Version: version})
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.tls.Run(ctx)
	}()
	if d.facts != nil {
		// What the used folders are (a repository, where they resolve) is found off the hub's loop too, beside the
		// scans and as often; a finding that differs reaches the dashboards with the repositories and the summaries
		// of the Tickets in those folders (their branch).
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			d.facts.run(ctx, localScanEvery, w.hub.FoldersChanged)
		}()
	}
	if d.local != nil {
		// The scans run off the hub's loop; a result that differs reaches the dashboards through ReposChanged.
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			d.local.run(ctx, w.hub.ReposChanged)
		}()
	}
	if w.vapid != nil {
		notifier := web.NewNotifier(web.NotifierConfig{Feed: w.hub, Store: w.st, Visible: w.presence.Visible, Wake: fx.Wake, Log: w.log})
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			notifier.Run(ctx, nil)
		}()
	}
	w.srv = web.NewServer(web.ServerConfig{Auth: auth, Dashboard: w.dash, VAPID: w.vapid, CA: w.tls.CA(), TLS: w.tls.Config(), UI: ui.FS(), Artifacts: flt, Log: w.log,
		NodeJoin: web.NodeJoin(w.st, w.nodeCA, w.now, w.nodeURL)})
	if w.nodeSrv != nil {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			if err := w.nodeSrv.ServeTLS(w.nodeL, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				w.log.Error("web: node listener stopped", "error", err)
			}
		}()
		w.log.Info("node endpoint serving", "url", w.nodeURL, "listen", w.nodeL.Addr().String())
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		if err := w.srv.Serve(w.l); err != nil {
			w.log.Error("web: listener stopped", "error", err)
		}
	}()
	w.log.Info("dashboard serving", "url", w.origin, "listen", w.l.Addr().String(), "tls", w.tls.Mode())
	return nil
}

// stop closes the listener, ends the dashboard connections and stops the Notifier, the renewals and the hub, and waits
// for each, so nothing of the web part touches the store after it returns. It is idempotent and safe before start.
func (w *webEndpoint) stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		if w.srv == nil {
			w.l.Close()
			if w.nodeL != nil {
				w.nodeL.Close()
			}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), webShutdown)
		if w.nodeSrv != nil {
			if err := w.nodeSrv.Shutdown(ctx); err != nil {
				_ = w.nodeSrv.Close()
			}
		} else if w.nodeL != nil {
			w.nodeL.Close()
		}
		if err := w.srv.Shutdown(ctx); err != nil {
			w.log.Warn("web: shutdown", "error", err)
		}
		cancel()
		w.st.OnCommit(nil)
		w.cancel()
		w.wg.Wait()
		// Last, with every connection and the Notifier gone: no read of the store is in flight after this.
		w.hub.Wait()
	})
}

// notServingNodes is the node link handler until the fleet serves nodes: it closes every link at once.
type notServingNodes struct{}

func (notServingNodes) ServeNode(_ string, c link.Conn) {
	_ = c.Close(link.CloseProtocol, "nodes are not served yet")
}

// status is web.status: the URL, the certificate's health and the open connections.
func (w *webEndpoint) status() WebStatusResult {
	ok, msg, notAfter := w.tls.Health()
	out := WebStatusResult{Serving: true, URL: w.origin, TLS: w.tls.Mode(), TLSOK: ok, TLSMessage: msg, Clients: w.dash.Connections()}
	if !notAfter.IsZero() {
		out.CertNotAfter = &notAfter
	}
	return out
}

// CodexHome is Athena's own CODEX_HOME, <stateDir>/codex: never the owner's ~/.codex.
func CodexHome(stateDir string) string { return filepath.Join(stateDir, "codex") }

// CodexRuntime is the Codex runtime of the configuration c (whether or not c enables it: see CodexConfig.On), for a
// server or the `athena codex` commands on stateDir: its CODEX_HOME is CodexHome, the hooks and the MCP server run
// athenaBin and reach the node socket, and the hooks are checked for the node's work root. binary, when set
// (`athena server --codex`), replaces the configured executable. Its decision hooks wait codex.DecisionHookTimeout,
// whatever the approval timeout (checkCodexApprovalTimeout).
func CodexRuntime(stateDir string, c FileConfig, athenaBin, binary string) *codex.Runtime {
	if binary == "" {
		binary = c.Runtimes.Codex.Binary
	}
	if binary == "" {
		binary = DefaultCodexBinary
	}
	return &codex.Runtime{
		Binary:    binary,
		Home:      CodexHome(stateDir),
		Model:     c.Runtimes.Codex.Model,
		AthenaBin: athenaBin,
		Socket:    filepath.Join(stateDir, NodeSocket),
		WorkRoot:  filepath.Join(stateDir, "work"),
	}
}

// checkCodexApprovalTimeout refuses an approval timeout Codex's decision hooks would not outlast by
// codex.DecisionHookMargin: Codex would give up on the hook before the node decides the Approval.
func checkCodexApprovalTimeout(approvalTimeout time.Duration) error {
	if max := codex.DecisionHookTimeout - codex.DecisionHookMargin; approvalTimeout > max {
		return fmt.Errorf("server: an approval timeout of %s is longer than Codex's hooks wait (at most %s); "+
			"shorten --approval-timeout, or set runtimes.codex.enabled to false", approvalTimeout, max)
	}
	return nil
}

// Executable is this program's path with symlinks resolved: the athena binary hooks and MCP servers run.
func Executable() (string, error) { return executable() }

// applyProposals is the apply_proposal effect handler: the node pushes the branch or opens the pull request the owner
// applied (node.ApplyProposal), and the Proposal is settled with the result. It is never retried: applying an outward
// effect twice is worse than asking again, so a failure settles the Proposal failed and the owner applies it again if
// they still want it. While Athena is paused it waits, like a reply.
// proposalApplier pushes a Proposal's branch or opens its pull request on the node that has its Ticket.
type proposalApplier interface {
	ApplyProposal(ctx context.Context, p core.Proposal) error
}

func applyProposals(st *store.Store, eng *core.Engine, n proposalApplier, paused func() bool, log *slog.Logger) effects.Handler {
	return func(ctx context.Context, key string, _ []byte) error {
		if paused != nil && paused() {
			return effects.Later(outbox.PausedWait)
		}
		var p *core.Proposal
		if err := st.Tx(func(tx *store.Tx) error {
			var err error
			p, err = tx.GetProposal(key)
			return err
		}); err != nil {
			return err
		}
		if p == nil || p.State != core.ProposalPending {
			return nil
		}
		applyErr := n.ApplyProposal(ctx, *p)
		if applyErr != nil {
			log.Warn("applying a proposal failed", "proposal", p.ID, "ticket", p.TicketID, "kind", p.Kind, "error", applyErr)
		}
		if err := eng.SettleProposal(p.ID, applyErr); err != nil {
			log.Error("settle an applied proposal; it stays pending", "proposal", p.ID, "error", err)
		}
		return nil
	}
}

// heldRepos are the repositories a settings change switched to shadow mode.
func heldRepos(from, to policy.Settings) []string {
	var out []string
	for repo, r := range to.Repos {
		if r.Mode == policy.RepoShadow && from.Repos[repo].Mode != policy.RepoShadow {
			out = append(out, repo)
		}
	}
	slices.Sort(out)
	return out
}

// tokenDropper drops the write tokens of a repository on the nodes: the fleet (every node), or one node.
type tokenDropper interface {
	DropTokens(repo string)
}

// holdSwitched holds back the work of the repositories a settings change switched to shadow mode: every node drops
// their write tokens (revoked, so a copy a worker kept stops working), and their replies waiting to go out are held for
// the owner. Both are safe to repeat (a change told again). n may be nil (tests).
func holdSwitched(eng *core.Engine, n tokenDropper, from, to policy.Settings) error {
	for _, repo := range heldRepos(from, to) {
		if n != nil {
			n.DropTokens(repo)
		}
		if err := eng.HoldWaiting(repo); err != nil {
			return err
		}
	}
	return nil
}
