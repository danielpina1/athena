package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/sources"
	"github.com/danielpina1/athena/internal/sources/github"
)

// The parts `athena server` wires between the store and the node: the GitHub App and Source, the Policy, the node's
// credentials, the Evals' ticker and the metrics of Trigger intake, replies and GitHub requests.
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

// gitHub is the server's GitHub: the App, the Source registered in the Registry (nil when it could not start) and,
// when it could not, why.
type gitHub struct {
	cfg    *GitHubConfig
	app    *github.App
	source *github.Source
	creds  *appCredentials
	// account is the installation account's login, which the default Policy allows.
	account string
	// err says why GitHub does not run; nil when it does.
	err error
}

// setupGitHub makes the App and checks it as `athena github check` does. Any failure is kept in err: the server
// runs on without GitHub, and status shows why.
func setupGitHub(cfg *GitHubConfig, version string, reg *metrics.Registry, hc *http.Client, log *slog.Logger) *gitHub {
	g := &gitHub{cfg: cfg}
	if hc == nil {
		hc = &http.Client{Timeout: httpTimeout}
	}
	hc = instrument(hc, reg)
	app, err := github.NewApp(cfg.appConfig(version), hc, nil)
	if err != nil {
		g.err = err
		log.Error("github: not started", "error", err)
		return g
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	check, err := checkApp(ctx, app, cfg)
	if err != nil {
		g.err = err
		log.Error("github: not started; fix the configuration or the installation and restart", "error", err)
		return g
	}
	g.app, g.account = app, check.Account
	g.creds = &appCredentials{app: app}
	log.Info("github: app checked", "bot", check.Bot, "account", check.Account, "repos", strings.Join(cfg.Repos, ","))
	return g
}

// health is the GitHub Source's health: why it did not start, or the Source's own.
func (g *gitHub) health() error {
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

// Token mints a token for repo alone, with github.WorkerPermissions().
func (c *appCredentials) Token(ctx context.Context, repo string) (string, time.Time, error) {
	t, err := c.app.ScopedToken(ctx, repo, github.WorkerPermissions())
	if err != nil {
		return "", time.Time{}, err
	}
	return t.Value, t.ExpiresAt, nil
}

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

// intake is the GitHub Source's view of the Engine; it times each Receive (trigger_intake_seconds).
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

var _ github.Intake = intake{}

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
	store   *policy.Store
	scope   core.SourceScope
	timeout time.Duration

	saveMu sync.Mutex // one save at a time
	mu     sync.RWMutex
	cur    policy.Settings
	rev    int
	pol    core.Policy
}

func newPolicyCache(st *policy.Store, scope core.SourceScope, timeout time.Duration) (*policyCache, error) {
	c := &policyCache{store: st, scope: scope, timeout: timeout}
	s, rev, err := st.Current()
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

// policy is the Engine's Policy getter.
func (c *policyCache) policy() core.Policy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pol
}

// settings is the router's: the current settings.
func (c *policyCache) settings() (policy.Settings, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur, nil
}

// current returns the settings and their revision.
func (c *policyCache) current() (policy.Settings, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur, c.rev
}

// save stores s as the next revision and makes it current; it returns the settings it replaced.
func (c *policyCache) save(s policy.Settings, origin string) (old policy.Settings, rev int, err error) {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	old, base := c.current()
	rev, err = c.store.Save(s, base, origin)
	if err != nil {
		return old, 0, err
	}
	saved, rev, err := c.store.Current()
	if err != nil {
		return old, 0, err
	}
	c.set(saved, rev)
	return old, rev, nil
}

// evaluate runs the Evals' due work now and then every evaluateEvery, only while a Thread has Triggers queued or an
// Eval in progress, until ctx is done.
func evaluate(ctx context.Context, eng *core.Engine, log *slog.Logger) {
	run := func() {
		active, err := eng.ActiveThreads()
		if err != nil {
			log.Error("evaluate: active threads", "error", err)
			return
		}
		if len(active) == 0 {
			return
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
