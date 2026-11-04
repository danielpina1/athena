package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/redact"
)

// The polling Source: each poll reads, per repository, the issues and pull requests, issue comments and review
// comments updated since a cursor, the reviews and check suites of the pull requests open Tickets follow, and the
// default branch's check suites, and turns them into Triggers; pull requests opened from a Ticket's branch are bound
// to it, and closes and merges are reported (reference 003 "Source failure" and "Pull requests").

// SourceName is the GitHub Source's name, the prefix of its Thread keys.
const SourceName = "github"

const (
	// DefaultInterval is the time between two polls of every repository.
	DefaultInterval = 30 * time.Second
	// firstLookback is how far back a repository is read the first time, when it has no cursor.
	firstLookback = time.Hour
	// overlap is how far before its cursor an endpoint is read again, for items GitHub shows late.
	overlap = time.Minute
	// pageSize is the per_page of every list request; a full page means there are more.
	pageSize = 100
	// defaultMaxRounds caps the pages one endpoint reads in one poll; the next poll goes on from the cursor.
	defaultMaxRounds = 10
	// unhealthyAfter is how many polls in a row must fail before Health reports an error.
	unhealthyAfter = 3
	// dropAfter is how long reads of an item or Thread may keep failing before it is dropped: skipped, so it no
	// longer fails the poll or holds back its endpoint (reference 003 "Source failure").
	dropAfter = 24 * time.Hour
)

// The kv keys of a repository: github.cursor.<repo>.<endpoint>, with the canonical (lower-case) repo.
const (
	endpointIssues         = "issues"
	endpointComments       = "comments"
	endpointReviewComments = "reviewcomments"
	// floorIssues and floorCI are not moving cursors: each is set once, an hour before the repository's first poll.
	// An issue or pull request created since floorIssues is new (its opening post is a Trigger); a check suite
	// updated before floorCI is history.
	floorIssues = "issuesfloor"
	floorCI     = "ci"
)

// States remembered per Thread (github.state.<thread>) so a close, a reopen or a deletion is acted on once.
const (
	stateOpen   = "open"
	stateClosed = "closed"
	stateMerged = "merged"
	stateGone   = "gone"
)

// SourceConfig configures the GitHub Source.
type SourceConfig struct {
	// Repos are the repositories (owner/name) the Source polls and listens to, compared case-insensitively.
	Repos []string
	// Interval is the time between polls; DefaultInterval when zero.
	Interval time.Duration
	// ReceiveAll marks the scope rows (issues, pulls, comments, ci) that receive every message: their Triggers carry
	// Meta["receiveAll"] = "true".
	ReceiveAll map[string]bool
}

// Intake is the part of the Engine the Source drives.
type Intake interface {
	Receive(t core.Trigger) (string, error)
	BindThread(ticketID, thread string, own bool) error
	ThreadClosed(thread string, outcome core.TicketOutcome, at time.Time) error
	// FollowedThreads returns the Threads of a Source open Tickets follow and that have not closed for them.
	FollowedThreads(source string) ([]string, error)
}

// KV holds the Source's cursors, floors, remembered Thread states and first-failure times.
type KV interface {
	KVGet(key string) (string, bool, error)
	KVSet(key, value string) error
}

// ThreadKey is the canonical Thread key of an issue or pull request: github:<owner>/<name>#<n>, lower-case repo.
func ThreadKey(repo string, n int) string {
	return SourceName + ":" + strings.ToLower(repo) + "#" + strconv.Itoa(n)
}

// BranchThreadKey is the canonical Thread key of a branch's CI: github:<owner>/<name>@<branch>, lower-case repo.
func BranchThreadKey(repo, branch string) string {
	return SourceName + ":" + strings.ToLower(repo) + "@" + branch
}

// Source is the GitHub polling Source. Run polls; Name, Listens, Health, Malformed and Dropped are safe for concurrent
// use.
type Source struct {
	cfg   SourceConfig
	app   *App
	in    Intake
	kv    KV
	log   *slog.Logger
	slug  string
	repos map[string]bool // canonical owner/name

	now       func() time.Time
	wait      func(ctx context.Context, d time.Duration) error
	perPage   int
	maxRounds int
	mention   *regexp.Regexp

	// Poll state, used only by the polling goroutine.
	login        string            // the App's bot login, learned on the first poll
	botID        int64             // and its user id
	done         map[string]string // read key → the first path whose items were all processed (a 304 on it is nothing new)
	isPull       map[string]bool   // Thread → whether it is a pull request (it never changes)
	pulls        map[string]ghPull // Thread → its pull request, read once per poll
	blockedUntil time.Time         // no request before this: a rate limit's reset

	mu        sync.Mutex
	failed    int               // polls failed in a row
	lastErr   string            // the latest poll error, redacted
	malformed int               // list items skipped as malformed
	dropped   map[string]string // Thread → why its reads are skipped (gone, or failing for dropAfter), redacted
}

// repo is one configured repository as a poll sees it.
type repo struct {
	key     string // canonical owner/name: Thread, cursor and state keys, request paths
	full    string // GitHub's full_name, for Meta and Channel addresses
	branch  string // the default branch
	htmlURL string
}

func (r repo) path() string { return "/repos/" + r.key }

// NewSource returns the Source over app's installation client. log defaults to a logger that discards.
func NewSource(cfg SourceConfig, app *App, in Intake, kv KV, log *slog.Logger) *Source {
	if log == nil {
		log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	s := &Source{cfg: cfg, app: app, in: in, kv: kv, log: log, repos: map[string]bool{}, now: time.Now, wait: sleepCtx,
		perPage: pageSize, maxRounds: defaultMaxRounds, done: map[string]string{}, isPull: map[string]bool{},
		dropped: map[string]string{}}
	for _, r := range cfg.Repos {
		s.repos[strings.ToLower(r)] = true
	}
	if app != nil {
		s.now = app.now
		s.slug = app.cfg.Slug
	}
	if s.slug != "" {
		s.mention = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_-])@` + regexp.QuoteMeta(s.slug) + `(?:$|[^A-Za-z0-9_-])`)
	}
	return s
}

// Name is "github".
func (s *Source) Name() string { return SourceName }

// Listens reports whether thread is an issue or pull request (github:<repo>#<n>) or a branch (github:<repo>@<branch>)
// of a configured repository, compared case-insensitively.
func (s *Source) Listens(thread string) bool {
	key, ok := strings.CutPrefix(thread, SourceName+":")
	if !ok {
		return false
	}
	i := strings.LastIndexAny(key, "#@")
	if i <= 0 || i == len(key)-1 {
		return false
	}
	if key[i] == '#' {
		if n, err := strconv.Atoi(key[i+1:]); err != nil || n <= 0 {
			return false
		}
	}
	return s.repos[strings.ToLower(key[:i])]
}

// Health is an error once unhealthyAfter polls in a row failed; a fully successful poll clears it.
func (s *Source) Health() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed >= unhealthyAfter {
		return fmt.Errorf("github: %d polls in a row failed; the latest: %s", s.failed, s.lastErr)
	}
	return nil
}

// Malformed is how many list items were skipped because they could not be read.
func (s *Source) Malformed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.malformed
}

// Dropped names the Threads whose reads the Source skips, with why: gone (deleted), or failing for a day. A Thread
// leaves the list once a read of it succeeds.
func (s *Source) Dropped() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.dropped)
}

// Run polls every repository at once and then every Interval until ctx is done, and returns nil then.
func (s *Source) Run(ctx context.Context) error {
	interval := s.cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	for {
		_ = s.poll(ctx)
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// poll polls every repository once and records the outcome for Health; a poll the context stopped is not counted.
func (s *Source) poll(ctx context.Context) error {
	err := s.pollAll(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.mu.Lock()
	if err == nil {
		s.failed, s.lastErr = 0, ""
	} else {
		s.failed++
		s.lastErr = redact.String(err.Error())
	}
	s.mu.Unlock()
	return err
}

func (s *Source) pollAll(ctx context.Context) error {
	s.pulls = map[string]ghPull{}
	if s.login == "" {
		if err := s.throttle(ctx); err != nil {
			return err
		}
		login, id, err := s.app.Bot(ctx)
		s.noteRate(err)
		if err != nil {
			// Without its own login the Source could not drop the App's own comments: it reads nothing.
			s.warn("", "bot identity", err)
			return fmt.Errorf("github: the App's bot identity: %w", err)
		}
		s.login, s.botID = login, id
	}
	var errs []error
	followed, err := s.in.FollowedThreads(SourceName)
	if err != nil {
		s.warn("", "followed threads", err)
		errs = append(errs, fmt.Errorf("github: followed threads: %w", err))
	}
	for _, r := range s.cfg.Repos {
		if ctx.Err() != nil {
			break
		}
		if err := s.pollRepo(ctx, r, followed); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var validRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// pollRepo reads the repository record, then each endpoint in turn; one failing does not stop the others.
func (s *Source) pollRepo(ctx context.Context, name string, followed []string) error {
	if !validRepo.MatchString(name) {
		return fmt.Errorf("github: repository %q is not owner/name", name)
	}
	r := repo{key: strings.ToLower(name)}
	var rec struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		HTMLURL       string `json:"html_url"`
	}
	if _, err := s.get(ctx, r.path(), &rec); err != nil {
		s.warn(r.key, "repository", err)
		return fmt.Errorf("github: %s: repository: %w", r.key, err)
	}
	r.full, r.branch, r.htmlURL = rec.FullName, rec.DefaultBranch, rec.HTMLURL
	if r.full == "" {
		r.full = name
	}
	var errs []error
	step := func(what string, fn func() error) {
		if ctx.Err() != nil {
			return
		}
		if err := fn(); err != nil {
			s.warn(r.key, what, err)
			errs = append(errs, fmt.Errorf("github: %s %s: %w", r.key, what, err))
		}
	}
	step(endpointIssues, func() error {
		floor, err := s.floor(r, floorIssues)
		if err != nil {
			return err
		}
		q := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"asc"}}
		return s.cursorList(ctx, r, endpointIssues, r.path()+"/issues", q, func(raw json.RawMessage) (time.Time, error) {
			return s.handleIssue(ctx, r, raw, floor)
		})
	})
	step(endpointComments, func() error {
		q := url.Values{"sort": {"updated"}, "direction": {"asc"}}
		return s.cursorList(ctx, r, endpointComments, r.path()+"/issues/comments", q, func(raw json.RawMessage) (time.Time, error) {
			return s.handleIssueComment(ctx, r, raw)
		})
	})
	step(endpointReviewComments, func() error {
		q := url.Values{"sort": {"updated"}, "direction": {"asc"}}
		return s.cursorList(ctx, r, endpointReviewComments, r.path()+"/pulls/comments", q, func(raw json.RawMessage) (time.Time, error) {
			return s.handleReviewComment(ctx, r, raw)
		})
	})
	seen := map[int]bool{}
	for _, thread := range followed {
		n, ok := threadNumber(r.key, thread)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		step("pull "+strconv.Itoa(n), func() error {
			canon := ThreadKey(r.key, n)
			if st, _, err := s.kv.KVGet(stateKey(canon)); err != nil || st == stateGone {
				return err
			}
			_, err := s.threadRead(canon, s.pollFollowedPull(ctx, r, canon, n))
			return err
		})
	}
	step("default branch ci", func() error { return s.pollDefaultBranch(ctx, r) })
	return errors.Join(errs...)
}

func (s *Source) warn(repo, endpoint string, err error) {
	s.log.Warn("github poll failed", "repo", repo, "endpoint", endpoint, "error", redact.String(err.Error()))
}

// --- Requests ---

// throttle waits out a rate limit before the next request.
func (s *Source) throttle(ctx context.Context) error {
	until := s.blockedUntil
	if c := s.client(); c != nil && c.RateRemaining() == 0 {
		if r := c.RateReset(); r.After(until) {
			until = r
		}
	}
	if d := until.Sub(s.now()); d > 0 {
		return s.wait(ctx, d)
	}
	return ctx.Err()
}

// noteRate remembers a rate limit's reset: no request goes out before it.
func (s *Source) noteRate(err error) {
	var rl ErrRateLimited
	if errors.As(err, &rl) && rl.Reset.After(s.blockedUntil) {
		s.blockedUntil = rl.Reset
	}
}

func (s *Source) client() *Client {
	if s.app == nil {
		return nil
	}
	return s.app.Client()
}

// get GETs path into out: on a 304 it decodes the cached body and reports notModified.
func (s *Source) get(ctx context.Context, path string, out any) (notModified bool, err error) {
	if err := s.throttle(ctx); err != nil {
		return false, err
	}
	c := s.client()
	notModified, err = c.Get(ctx, path, out)
	s.noteRate(err)
	if err != nil {
		return false, err
	}
	if notModified && !c.Cached(path, out) {
		return false, fmt.Errorf("github: GET %s: 304 without a cached body: %w", path, ErrTransient)
	}
	return notModified, nil
}

func (s *Source) paginate(ctx context.Context, path string, fn func(json.RawMessage) error) error {
	if err := s.throttle(ctx); err != nil {
		return err
	}
	err := s.client().Paginate(ctx, path, fn)
	s.noteRate(err)
	return err
}

// pull returns a pull request, read once per poll.
func (s *Source) pull(ctx context.Context, r repo, n int) (ghPull, error) {
	thread := ThreadKey(r.key, n)
	if pr, ok := s.pulls[thread]; ok {
		return pr, nil
	}
	var pr ghPull
	if _, err := s.get(ctx, r.path()+"/pulls/"+strconv.Itoa(n), &pr); err != nil {
		return ghPull{}, err
	}
	s.pulls[thread] = pr
	return pr, nil
}

// readAll reads a list that has no cursor (a pull request's reviews, a commit's check suites): the first page with
// get, so an unchanged list costs a 304 and nothing else once all its items were processed, and, when it is full, the
// later pages with Paginate. items extracts a page's items and handle processes one. It stops at the first error.
func (s *Source) readAll(ctx context.Context, key, path string, items func([]byte) ([]json.RawMessage, error),
	handle func(json.RawMessage) (time.Time, error)) error {
	var body json.RawMessage
	notModified, err := s.get(ctx, path, &body)
	if err != nil {
		return err
	}
	if notModified && s.done[key] == path {
		return nil
	}
	delete(s.done, key)
	process := func(page []byte) (int, error) {
		list, err := items(page)
		if err != nil {
			return 0, fmt.Errorf("github: GET %s: %w", path, err)
		}
		for _, raw := range list {
			if _, err := handle(raw); err != nil {
				return 0, err
			}
		}
		return len(list), nil
	}
	n, err := process(body)
	if err == nil && n >= s.perPage {
		err = s.paginate(ctx, path+"&page=2", func(page json.RawMessage) error {
			_, err := process(page)
			return err
		})
	}
	if err == nil {
		s.done[key] = path
	}
	return err
}

func arrayItems(body []byte) ([]json.RawMessage, error) {
	var list []json.RawMessage
	err := json.Unmarshal(body, &list)
	return list, err
}

// cursorList reads an endpoint of a repository (sorted by updated_at, oldest first) from its cursor
// (github.cursor.<repo>.<endpoint>, an RFC 3339 updated_at; an hour ago the first time), starting overlap before it.
// Pages are keyset: after a full page the next request starts at the last item's updated_at (since is inclusive, so
// the overlap answers duplicate), and only a page whose items all share one updated_at moves on by page number; at
// most maxRounds requests per poll. The cursor moves to the updated_at of the last item processed, also when a later
// one failed.
func (s *Source) cursorList(ctx context.Context, r repo, endpoint, base string, q url.Values,
	handle func(raw json.RawMessage) (time.Time, error)) error {
	key := cursorKey(r, endpoint)
	cur, had, err := s.cursor(key)
	if err != nil {
		return err
	}
	since := cur
	if had {
		since = cur.Add(-overlap)
	}
	var last time.Time
	page, complete := 1, false
	for round := 0; round < s.maxRounds; round++ {
		q.Set("since", since.UTC().Format(time.RFC3339))
		q.Set("per_page", strconv.Itoa(s.perPage))
		q.Del("page")
		if page > 1 {
			q.Set("page", strconv.Itoa(page))
		}
		path := base + "?" + q.Encode()
		var list []json.RawMessage
		var notModified bool
		if notModified, err = s.get(ctx, path, &list); err != nil {
			break
		}
		if round == 0 {
			if notModified && s.done[key] == path {
				complete = true
				break
			}
			delete(s.done, key)
		}
		var pageLast time.Time
		for _, raw := range list {
			u, herr := handle(raw)
			if herr != nil {
				err = herr
				break
			}
			if u.After(pageLast) {
				pageLast = u
			}
		}
		if pageLast.After(last) {
			last = pageLast
		}
		if err != nil {
			break
		}
		if len(list) < s.perPage {
			complete = true
			if round == 0 {
				s.done[key] = path
			}
			break
		}
		if pageLast.After(since) {
			since, page = pageLast, 1
		} else {
			page++
		}
	}
	if last.After(cur) || !had {
		next := cur
		if last.After(cur) {
			next = last
		}
		if serr := s.kv.KVSet(key, next.UTC().Format(time.RFC3339)); serr != nil {
			err = errors.Join(err, fmt.Errorf("github: save cursor %s: %w", key, serr))
			delete(s.done, key)
		}
	}
	if err == nil && !complete {
		s.log.Info("github endpoint read capped for this poll; it goes on next poll", "repo", r.key, "endpoint", endpoint)
	}
	return err
}

func cursorKey(r repo, endpoint string) string { return "github.cursor." + r.key + "." + endpoint }

func stateKey(thread string) string   { return "github.state." + thread }
func failingKey(thread string) string { return "github.failing." + thread }

// cursor returns a stored cursor, or an hour ago when there is none.
func (s *Source) cursor(key string) (time.Time, bool, error) {
	v, ok, err := s.kv.KVGet(key)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("github: read cursor %s: %w", key, err)
	}
	if ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, true, nil
		}
		s.log.Warn("github cursor unreadable; reading from an hour ago", "key", key)
	}
	return s.now().Add(-firstLookback).UTC().Truncate(time.Second), false, nil
}

// floor is github.cursor.<repo>.<name>, set once to an hour before the repository's first poll.
func (s *Source) floor(r repo, name string) (time.Time, error) {
	key := cursorKey(r, name)
	floor, had, err := s.cursor(key)
	if err != nil || had {
		return floor, err
	}
	if err := s.kv.KVSet(key, floor.Format(time.RFC3339)); err != nil {
		return floor, fmt.Errorf("github: save %s: %w", key, err)
	}
	return floor, nil
}

func (s *Source) skip(what string, err error) (time.Time, error) {
	s.mu.Lock()
	s.malformed++
	s.mu.Unlock()
	s.log.Warn("github item skipped as malformed", "item", what, "error", redact.String(fmt.Sprint(err)))
	return time.Time{}, nil
}

// --- Failing reads ---

// readError reports whether err is GitHub's answer to a read (not the Engine's, a rate limit or the context's).
func readError(err error) bool {
	if errors.Is(err, ErrRateLimited{}) {
		return false
	}
	return errors.Is(err, ErrTransient) || errors.Is(err, ErrPermanent) || errors.Is(err, ErrGone)
}

// threadRead settles the outcome of reading a Thread's data. A success clears its first-failure time. A read GitHub
// answers as gone records the Thread closed and gone, and drops it. Another read error keeps the Thread's first
// failure (github.failing.<thread>) and stands until reads have failed for dropAfter; then the Thread is dropped:
// the caller skips it, so it no longer fails the poll or holds back its endpoint. Other errors stand.
func (s *Source) threadRead(thread string, err error) (drop bool, _ error) {
	if err == nil {
		return false, s.readOK(thread)
	}
	if !readError(err) {
		return false, err
	}
	if errors.Is(err, ErrGone) {
		return true, s.gone(thread, err)
	}
	key := failingKey(thread)
	v, ok, kerr := s.kv.KVGet(key)
	if kerr != nil {
		return false, errors.Join(err, kerr)
	}
	first, perr := time.Parse(time.RFC3339, v)
	if !ok || perr != nil {
		if kerr := s.kv.KVSet(key, s.now().UTC().Format(time.RFC3339)); kerr != nil {
			return false, errors.Join(err, kerr)
		}
		return false, err
	}
	if s.now().Sub(first) < dropAfter {
		return false, err
	}
	why := "reads failing since " + v + ": " + redact.String(err.Error())
	s.mu.Lock()
	s.dropped[thread] = why
	s.mu.Unlock()
	s.log.Warn("github thread dropped", "thread", thread, "reason", why)
	return true, nil
}

// readOK clears a Thread's failure once a read of it succeeds.
func (s *Source) readOK(thread string) error {
	s.mu.Lock()
	delete(s.dropped, thread)
	s.mu.Unlock()
	key := failingKey(thread)
	v, ok, err := s.kv.KVGet(key)
	if err != nil || !ok || v == "" {
		return err
	}
	return s.kv.KVSet(key, "")
}

// gone records a deleted Thread: closed for the Tickets that follow it, and never read again.
func (s *Source) gone(thread string, cause error) error {
	s.mu.Lock()
	s.dropped[thread] = "gone: " + redact.String(cause.Error())
	s.mu.Unlock()
	if st, _, err := s.kv.KVGet(stateKey(thread)); err != nil || st == stateGone {
		return err
	}
	s.log.Info("github thread gone", "thread", thread)
	if err := s.in.ThreadClosed(thread, core.OutcomeClosed, s.now().UTC()); err != nil {
		return fmt.Errorf("close %s: %w", thread, err)
	}
	return s.kv.KVSet(stateKey(thread), stateGone)
}

// --- Translation ---

type ghUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

type ghIssue struct {
	ID          int64      `json:"id"`
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	HTMLURL     string     `json:"html_url"`
	User        *ghUser    `json:"user"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	PullRequest *struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

type ghPull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

type ghComment struct {
	ID             int64     `json:"id"`
	Body           string    `json:"body"`
	HTMLURL        string    `json:"html_url"`
	IssueURL       string    `json:"issue_url"`
	PullRequestURL string    `json:"pull_request_url"`
	InReplyToID    int64     `json:"in_reply_to_id"`
	User           *ghUser   `json:"user"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type ghReview struct {
	ID          int64      `json:"id"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	HTMLURL     string     `json:"html_url"`
	User        *ghUser    `json:"user"`
	SubmittedAt *time.Time `json:"submitted_at"`
}

type ghSuite struct {
	ID         int64   `json:"id"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
	HeadBranch string  `json:"head_branch"`
	HeadSHA    string  `json:"head_sha"`
	App        *struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"app"`
	UpdatedAt time.Time `json:"updated_at"`
}

// user is an item's author; a deleted account reads as GitHub's "ghost".
func user(u *ghUser) ghUser {
	if u == nil || u.Login == "" {
		return ghUser{Login: "ghost", Type: "User"}
	}
	return *u
}

// isBot: a GitHub App's account or a login ending in [bot].
func isBot(u ghUser) bool {
	return u.Type == "Bot" || strings.HasSuffix(strings.ToLower(u.Login), "[bot]")
}

// own reports whether the App itself wrote the item.
func (s *Source) own(u ghUser) bool {
	return (s.login != "" && strings.EqualFold(u.Login, s.login)) || (s.botID != 0 && u.ID == s.botID)
}

// addressed is mention when the text names @<slug> as a whole word, case-insensitively, outside quoted lines (a
// quote repeats someone else's words; it does not address the agent).
func (s *Source) addressed(text string) string {
	if s.mention == nil {
		return ""
	}
	var own []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
			own = append(own, line)
		}
	}
	if s.mention.MatchString(strings.Join(own, "\n")) {
		return core.AddressedMention
	}
	return ""
}

func kindOf(pull bool) string {
	if pull {
		return "pull"
	}
	return "issue"
}

// trigger fills what every message Trigger of a person or bot shares.
func (s *Source) trigger(id, kind, scope string, r repo, n int, pull bool, u ghUser, at time.Time, text, link string) core.Trigger {
	thread := ThreadKey(r.key, n)
	meta := map[string]string{"repo": r.full, "number": strconv.Itoa(n), "kind": kindOf(pull)}
	if link != "" {
		meta["url"] = link
	}
	if s.cfg.ReceiveAll[scope] {
		meta["receiveAll"] = "true"
	}
	bot := isBot(u)
	return core.Trigger{
		ID: id, Source: SourceName, Kind: kind, Scope: scope,
		Actor: core.ActorRef{Source: SourceName, ID: u.Login}, Human: !bot, Bot: bot,
		Addressed: s.addressed(text), Threads: []string{thread}, At: at.UTC(), Text: text, Meta: meta,
		Channels: []core.ChannelRef{{Source: SourceName, Thread: thread, Address: map[string]string{"repo": r.full, "number": strconv.Itoa(n)}}},
	}
}

// receive hands a Trigger to the Engine. A Trigger the Engine refuses as invalid is skipped, not retried.
func (s *Source) receive(t core.Trigger) error {
	res, err := s.in.Receive(t)
	var ue *core.UserError
	if errors.As(err, &ue) {
		_, _ = s.skip(t.ID, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("receive %s: %w", t.ID, err)
	}
	s.log.Debug("github trigger", "trigger", t.ID, "result", res)
	return nil
}

// itemPull reads the pull request an item belongs to under the failing-read rules: skip is set when the item is to
// be skipped (its pull request is gone, or its reads failed for dropAfter).
func (s *Source) itemPull(ctx context.Context, r repo, n int) (pr ghPull, skip bool, err error) {
	pr, rerr := s.pull(ctx, r, n)
	skip, err = s.threadRead(ThreadKey(r.key, n), rerr)
	return pr, skip, err
}

// handleIssue handles one issue or pull request of the issues list: the Trigger of a new one (created since the
// repository's issues floor; a repeat answers duplicate), the binding of a pull request from a Ticket's branch of
// this repository the first time it is seen and when it reopens, and a close or merge once per transition.
func (s *Source) handleIssue(ctx context.Context, r repo, raw json.RawMessage, floor time.Time) (time.Time, error) {
	var it ghIssue
	if err := json.Unmarshal(raw, &it); err != nil || it.ID == 0 || it.Number <= 0 || it.UpdatedAt.IsZero() {
		return s.skip(r.key+" issue", errOr(err, "no id, number or updated_at"))
	}
	thread := ThreadKey(r.key, it.Number)
	pull := it.PullRequest != nil
	s.isPull[thread] = pull
	var pr ghPull
	ticket := ""
	if pull {
		var skip bool
		var err error
		if pr, skip, err = s.itemPull(ctx, r, it.Number); err != nil || skip {
			return it.UpdatedAt, err
		}
		// Only a branch of this repository is a Ticket's: a fork can name its branch athena/<ticket-id> too.
		if pr.Head.Repo != nil && strings.EqualFold(pr.Head.Repo.FullName, r.key) {
			ticket = ticketOfBranch(pr.Head.Ref)
		}
	}
	state := stateOpen
	closedAt := it.UpdatedAt
	if it.State == "closed" {
		state = stateClosed
		if it.ClosedAt != nil {
			closedAt = *it.ClosedAt
		}
		if pull && it.PullRequest.MergedAt != nil {
			state, closedAt = stateMerged, *it.PullRequest.MergedAt
		}
	}
	prev, had, err := s.kv.KVGet(stateKey(thread))
	if err != nil {
		return time.Time{}, err
	}
	reopened := had && prev != stateOpen && state == stateOpen
	if ticket != "" && (!had || reopened) {
		if err := s.bind(ticket, thread); err != nil {
			return time.Time{}, err
		}
	}
	if u := user(it.User); !it.CreatedAt.Before(floor) && !s.own(u) {
		scope := "issues"
		if pull {
			scope = "pulls"
		}
		text := it.Title
		if it.Body != "" {
			text += "\n\n" + it.Body
		}
		t := s.trigger("github:issue:"+strconv.FormatInt(it.ID, 10), core.KindMessage, scope, r, it.Number, pull, u, it.CreatedAt, text, it.HTMLURL)
		if pull && pr.Head.Ref != "" {
			t.Meta["head"] = pr.Head.Ref
		}
		if err := s.receive(t); err != nil {
			return time.Time{}, err
		}
	}
	switch {
	case state != stateOpen && prev != state:
		if err := s.in.ThreadClosed(thread, core.TicketOutcome(state), closedAt.UTC()); err != nil {
			return time.Time{}, fmt.Errorf("close %s: %w", thread, err)
		}
		if err := s.kv.KVSet(stateKey(thread), state); err != nil {
			return time.Time{}, err
		}
	case reopened || (state == stateOpen && !had && ticket != ""):
		if err := s.kv.KVSet(stateKey(thread), stateOpen); err != nil {
			return time.Time{}, err
		}
	}
	return it.UpdatedAt, nil
}

// bind makes the Ticket follow its pull request as its own; a Ticket the Engine refuses (unknown or ended) is left.
func (s *Source) bind(ticket, thread string) error {
	err := s.in.BindThread(ticket, thread, true)
	var ue *core.UserError
	if errors.As(err, &ue) {
		s.log.Info("github pull request not bound", "ticket", ticket, "thread", thread, "reason", ue.Msg)
		return nil
	}
	if err != nil {
		return fmt.Errorf("bind %s to %s: %w", thread, ticket, err)
	}
	return nil
}

var ticketBranch = regexp.MustCompile(`^athena/([Tt]-[0-9]{4,})$`)

// ticketOfBranch is the Ticket ID (T-NNNN) of a branch athena/<ticket-id>, or "".
func ticketOfBranch(ref string) string {
	m := ticketBranch.FindStringSubmatch(ref)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

func (s *Source) handleIssueComment(ctx context.Context, r repo, raw json.RawMessage) (time.Time, error) {
	var c ghComment
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == 0 || c.UpdatedAt.IsZero() {
		return s.skip(r.key+" issue comment", errOr(err, "no id or updated_at"))
	}
	n, ok := lastNumber(c.IssueURL)
	if !ok {
		return s.skip(r.key+" issue comment "+strconv.FormatInt(c.ID, 10), errors.New("no issue number in issue_url"))
	}
	// An issue comment's page is /pull/<n> when the issue is a pull request.
	pull := strings.Contains(c.HTMLURL, "/pull/")
	s.isPull[ThreadKey(r.key, n)] = pull
	u := user(c.User)
	if s.own(u) {
		return c.UpdatedAt, nil
	}
	t := s.trigger("github:issuecomment:"+strconv.FormatInt(c.ID, 10), core.KindMessage, "comments", r, n, pull, u, c.CreatedAt, c.Body, c.HTMLURL)
	if pull {
		pr, skip, err := s.itemPull(ctx, r, n)
		if err != nil || skip {
			return c.UpdatedAt, err
		}
		if pr.Head.Ref != "" {
			t.Meta["head"] = pr.Head.Ref
		}
	}
	if err := s.receive(t); err != nil {
		return time.Time{}, err
	}
	return c.UpdatedAt, nil
}

// handleReviewComment: a comment on a pull request's diff, on the pull request's Thread; replies go under the top
// comment of its review thread, which a reply also answers.
func (s *Source) handleReviewComment(ctx context.Context, r repo, raw json.RawMessage) (time.Time, error) {
	var c ghComment
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == 0 || c.UpdatedAt.IsZero() {
		return s.skip(r.key+" review comment", errOr(err, "no id or updated_at"))
	}
	n, ok := lastNumber(c.PullRequestURL)
	if !ok {
		return s.skip(r.key+" review comment "+strconv.FormatInt(c.ID, 10), errors.New("no number in pull_request_url"))
	}
	s.isPull[ThreadKey(r.key, n)] = true
	u := user(c.User)
	if s.own(u) {
		return c.UpdatedAt, nil
	}
	pr, skip, err := s.itemPull(ctx, r, n)
	if err != nil || skip {
		return c.UpdatedAt, err
	}
	t := s.trigger("github:reviewcomment:"+strconv.FormatInt(c.ID, 10), core.KindMessage, "comments", r, n, true, u, c.CreatedAt, c.Body, c.HTMLURL)
	if pr.Head.Ref != "" {
		t.Meta["head"] = pr.Head.Ref
	}
	top := c.ID
	if c.InReplyToID != 0 {
		top = c.InReplyToID
		t.ReplyTo = "github:reviewcomment:" + strconv.FormatInt(c.InReplyToID, 10)
	}
	t.Channels[0].Address["comment_id"] = strconv.FormatInt(top, 10)
	if err := s.receive(t); err != nil {
		return time.Time{}, err
	}
	return c.UpdatedAt, nil
}

// pollFollowedPull reads the reviews and the head's check suites of a pull request an open Ticket follows.
func (s *Source) pollFollowedPull(ctx context.Context, r repo, thread string, n int) error {
	pull, known := s.isPull[thread]
	if !known {
		var it ghIssue
		if _, err := s.get(ctx, r.path()+"/issues/"+strconv.Itoa(n), &it); err != nil {
			return err
		}
		pull = it.PullRequest != nil
		s.isPull[thread] = pull
	}
	if !pull {
		return nil
	}
	pr, err := s.pull(ctx, r, n)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/pulls/%d/reviews?per_page=%d", r.path(), n, s.perPage)
	if err := s.readAll(ctx, "reviews "+thread, path, arrayItems, func(raw json.RawMessage) (time.Time, error) {
		return s.handleReview(r, n, pr.Head.Ref, raw)
	}); err != nil {
		return err
	}
	if pr.Head.SHA == "" {
		return nil
	}
	return s.checkSuites(ctx, r, thread, pr.Head.SHA, func(t *core.Trigger) {
		t.Meta["kind"], t.Meta["number"], t.Meta["head"] = "pull", strconv.Itoa(n), pr.Head.Ref
		if pr.HTMLURL != "" {
			t.Meta["url"] = pr.HTMLURL + "/checks"
		}
		t.Channels = []core.ChannelRef{{Source: SourceName, Thread: thread, Address: map[string]string{"repo": r.full, "number": strconv.Itoa(n)}}}
	})
}

// handleReview: a submitted review, approvals with no body included; pending (unsubmitted) and dismissed ones are not
// Triggers.
func (s *Source) handleReview(r repo, n int, head string, raw json.RawMessage) (time.Time, error) {
	var rv ghReview
	if err := json.Unmarshal(raw, &rv); err != nil || rv.ID == 0 {
		return s.skip(r.key+" review", errOr(err, "no id"))
	}
	switch rv.State {
	case "APPROVED", "CHANGES_REQUESTED", "COMMENTED":
	default:
		return time.Time{}, nil
	}
	if rv.SubmittedAt == nil {
		return time.Time{}, nil
	}
	u := user(rv.User)
	if s.own(u) {
		return *rv.SubmittedAt, nil
	}
	t := s.trigger("github:review:"+strconv.FormatInt(rv.ID, 10), core.KindReview, "comments", r, n, true, u, *rv.SubmittedAt, rv.Body, rv.HTMLURL)
	t.Meta["state"] = rv.State
	if head != "" {
		t.Meta["head"] = head
	}
	if err := s.receive(t); err != nil {
		return time.Time{}, err
	}
	return *rv.SubmittedAt, nil
}

// pollDefaultBranch reads the check suites of the default branch's head, on the Thread github:<repo>@<branch>.
func (s *Source) pollDefaultBranch(ctx context.Context, r repo) error {
	if r.branch == "" {
		return fmt.Errorf("github: repository %s has no default branch: %w", r.key, ErrPermanent)
	}
	var br struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if _, err := s.get(ctx, r.path()+"/branches/"+url.PathEscape(r.branch), &br); err != nil {
		return err
	}
	if br.Commit.SHA == "" {
		return fmt.Errorf("github: branch %s of %s has no head commit: %w", r.branch, r.key, ErrPermanent)
	}
	return s.checkSuites(ctx, r, BranchThreadKey(r.key, r.branch), br.Commit.SHA, func(t *core.Trigger) {
		t.Meta["kind"], t.Meta["branch"] = "branch", r.branch
		if r.htmlURL != "" {
			t.Meta["url"] = r.htmlURL + "/commit/" + br.Commit.SHA + "/checks"
		}
	})
}

// checkSuites turns the completed check suites of a commit into CI Triggers on thread; decorate adds the Thread's
// Meta and Channels. Suites last updated before the repository's CI floor are history.
func (s *Source) checkSuites(ctx context.Context, r repo, thread, sha string, decorate func(*core.Trigger)) error {
	floor, err := s.floor(r, floorCI)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/commits/%s/check-suites?per_page=%d", r.path(), url.PathEscape(sha), s.perPage)
	suites := func(body []byte) ([]json.RawMessage, error) {
		var page struct {
			CheckSuites []json.RawMessage `json:"check_suites"`
		}
		err := json.Unmarshal(body, &page)
		return page.CheckSuites, err
	}
	return s.readAll(ctx, "suites "+thread, path, suites, func(raw json.RawMessage) (time.Time, error) {
		var cs ghSuite
		if err := json.Unmarshal(raw, &cs); err != nil || cs.ID == 0 {
			return s.skip(r.key+" check suite", errOr(err, "no id"))
		}
		if cs.Status != "completed" || cs.Conclusion == nil || *cs.Conclusion == "" || cs.UpdatedAt.Before(floor) {
			return time.Time{}, nil
		}
		app, name := "", ""
		if cs.App != nil {
			app, name = cs.App.Slug, cs.App.Name
		}
		if s.slug != "" && strings.EqualFold(app, s.slug) {
			return cs.UpdatedAt, nil
		}
		if name == "" {
			name = app
		}
		short := sha
		if len(short) > 7 {
			short = short[:7]
		}
		meta := map[string]string{"repo": r.full, "sha": sha, "conclusion": *cs.Conclusion}
		if s.cfg.ReceiveAll["ci"] {
			meta["receiveAll"] = "true"
		}
		t := core.Trigger{
			ID: fmt.Sprintf("github:checksuite:%d:%s", cs.ID, *cs.Conclusion), Source: SourceName, Kind: core.KindCI,
			Actor: core.ActorRef{Source: SourceName, ID: app}, Scope: "ci", Threads: []string{thread}, At: cs.UpdatedAt.UTC(),
			Text: fmt.Sprintf("%s check suite %s on %s (%s)", name, *cs.Conclusion, cs.HeadBranch, short), Meta: meta,
		}
		decorate(&t)
		if err := s.receive(t); err != nil {
			return time.Time{}, err
		}
		return cs.UpdatedAt, nil
	})
}

// threadNumber is n of github:<repo>#<n> when the Thread is of repo (compared case-insensitively).
func threadNumber(repo, thread string) (int, bool) {
	key, ok := strings.CutPrefix(thread, SourceName+":")
	if !ok {
		return 0, false
	}
	r, num, ok := strings.Cut(key, "#")
	if !ok || !strings.EqualFold(r, repo) {
		return 0, false
	}
	n, err := strconv.Atoi(num)
	return n, err == nil && n > 0
}

// lastNumber is the number that ends an API URL such as https://api.github.com/repos/o/r/issues/12.
func lastNumber(u string) (int, bool) {
	i := strings.LastIndexByte(u, '/')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(u[i+1:])
	return n, err == nil && n > 0
}

func errOr(err error, msg string) error {
	if err != nil {
		return err
	}
	return errors.New(msg)
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
