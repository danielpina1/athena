package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/sources"
	"github.com/danielpina1/athena/internal/store"
)

var (
	_ sources.Source = (*Source)(nil)
	_ Intake         = (*core.Engine)(nil)
	_ KV             = (*store.Store)(nil)
)

// fakeGitHub serves the testdata/github fixtures the way GitHub's REST API does: lists filtered by since and sorted
// by updated_at, pages with Link headers, ETags with 304s, and failures queued per route.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server
	clk *clock

	mu             sync.Mutex
	repo           map[string]any
	issues         []map[string]any
	comments       []map[string]any
	reviewComments []map[string]any
	pulls          map[string]map[string]any
	reviews        map[string][]map[string]any
	branches       map[string]map[string]any
	suites         map[string]map[string]any
	fail           map[string][]int // route → statuses to answer, in order
	requests       []request
	notModified    int
	// after, when set, runs under the lock after each response, as GitHub changing between two requests.
	after func(route string, q url.Values)
}

type request struct {
	route  string
	status int
	at     time.Time
	query  url.Values
}

func loadFixture(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "github", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{t: t, clk: newClock(), fail: map[string][]int{}}
	loadFixture(t, "repo.json", &g.repo)
	loadFixture(t, "issues.json", &g.issues)
	loadFixture(t, "issue_comments.json", &g.comments)
	loadFixture(t, "review_comments.json", &g.reviewComments)
	loadFixture(t, "pulls.json", &g.pulls)
	loadFixture(t, "reviews.json", &g.reviews)
	loadFixture(t, "branches.json", &g.branches)
	loadFixture(t, "check_suites.json", &g.suites)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/777/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_installationsecret","expires_at":%q}`, g.clk.now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /users/athena-bot[bot]", func(w http.ResponseWriter, r *http.Request) {
		g.serve(w, r, "bot", map[string]any{"login": "athena-bot[bot]", "id": 4242, "type": "Bot"})
	})
	mux.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) { g.serve(w, r, "repo", g.repo) })
	mux.HandleFunc("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("state") != "all" || q.Get("sort") != "updated" || q.Get("direction") != "asc" {
			t.Errorf("issues query = %s", r.URL.RawQuery)
		}
		g.serveList(w, r, "issues", g.issues)
	})
	mux.HandleFunc("GET /repos/o/r/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		for _, it := range g.issues {
			if fmt.Sprint(it["number"]) == r.PathValue("n") {
				g.serve(w, r, "issue", it)
				return
			}
		}
		g.serve(w, r, "issue", nil)
	})
	mux.HandleFunc("GET /repos/o/r/issues/comments", func(w http.ResponseWriter, r *http.Request) {
		g.serveList(w, r, "comments", g.comments)
	})
	mux.HandleFunc("GET /repos/o/r/pulls/comments", func(w http.ResponseWriter, r *http.Request) {
		g.serveList(w, r, "reviewcomments", g.reviewComments)
	})
	mux.HandleFunc("GET /repos/o/r/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		g.serve(w, r, "pull", g.pulls[r.PathValue("n")])
	})
	mux.HandleFunc("GET /repos/o/r/pulls/{n}/reviews", func(w http.ResponseWriter, r *http.Request) {
		g.serve(w, r, "reviews", g.reviews[r.PathValue("n")])
	})
	mux.HandleFunc("GET /repos/o/r/branches/{b}", func(w http.ResponseWriter, r *http.Request) {
		g.serve(w, r, "branch", g.branches[r.PathValue("b")])
	})
	mux.HandleFunc("GET /repos/o/r/commits/{sha}/check-suites", func(w http.ResponseWriter, r *http.Request) {
		v, ok := g.suites[r.PathValue("sha")]
		if !ok {
			v = map[string]any{"total_count": 0, "check_suites": []any{}}
		}
		g.serve(w, r, "checksuites", v)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// serve answers a route: a queued failure, a 404 for nil, a 304 for a matching ETag, or v as JSON.
func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request, route string, v any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.respond(w, r, route, v, "")
}

func (g *fakeGitHub) respond(w http.ResponseWriter, r *http.Request, route string, v any, link string) {
	status := http.StatusOK
	defer func() {
		g.requests = append(g.requests, request{route: route, status: status, at: g.clk.now(), query: r.URL.Query()})
		if g.after != nil {
			g.after(route, r.URL.Query())
		}
	}()
	if q := g.fail[route]; len(q) > 0 {
		status, g.fail[route] = q[0], q[1:]
		switch status {
		case http.StatusForbidden:
			w.Header().Set("x-ratelimit-remaining", "0")
			w.Header().Set("x-ratelimit-reset", strconv.FormatInt(g.clk.now().Add(2*time.Minute).Unix(), 10))
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message":"API rate limit exceeded for installation ID 777."}`)
		default:
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message":"boom: token ghs_leakedsecretvalue1234 was used"}`)
		}
		return
	}
	if v == nil || reflect.ValueOf(v).IsNil() {
		status = http.StatusNotFound
		w.WriteHeader(status)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return
	}
	body, err := json.Marshal(v)
	if err != nil {
		g.t.Error(err)
	}
	sum := sha256.Sum256(append(body, link...))
	etag := `W/"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("x-ratelimit-remaining", "4999")
	if r.Header.Get("If-None-Match") == etag {
		status = http.StatusNotModified
		g.notModified++
		w.WriteHeader(status)
		return
	}
	w.Header().Set("ETag", etag)
	if link != "" {
		w.Header().Set("Link", link)
	}
	w.Write(body)
}

// serveList serves a list as GitHub does with since, sort=updated, direction=asc, per_page and page.
func (g *fakeGitHub) serveList(w http.ResponseWriter, r *http.Request, route string, all []map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	q := r.URL.Query()
	since, err := time.Parse(time.RFC3339, q.Get("since"))
	if err != nil {
		g.t.Errorf("%s: since = %q", route, q.Get("since"))
	}
	if q.Get("sort") != "updated" || q.Get("direction") != "asc" {
		g.t.Errorf("%s: query = %s", route, r.URL.RawQuery)
	}
	var items []map[string]any
	for _, it := range all {
		u, _ := time.Parse(time.RFC3339, it["updated_at"].(string))
		if !u.Before(since) {
			items = append(items, it)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i]["updated_at"].(string) < items[j]["updated_at"].(string) })
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		per = 30
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	lo, hi := min((page-1)*per, len(items)), min(page*per, len(items))
	link := ""
	if hi < len(items) {
		q.Set("page", strconv.Itoa(page+1))
		link = fmt.Sprintf(`<%s%s?%s>; rel="next"`, g.srv.URL, r.URL.Path, q.Encode())
	}
	out := items[lo:hi]
	if out == nil {
		out = []map[string]any{}
	}
	g.respond(w, r, route, out, link)
}

func (g *fakeGitHub) failNext(route string, statuses ...int) {
	g.mu.Lock()
	g.fail[route] = append(g.fail[route], statuses...)
	g.mu.Unlock()
}

// edit changes a fixture under the lock.
func (g *fakeGitHub) edit(fn func()) {
	g.mu.Lock()
	fn()
	g.mu.Unlock()
}

func (g *fakeGitHub) issue(number int) map[string]any {
	for _, it := range g.issues {
		if it["number"] == float64(number) {
			return it
		}
	}
	g.t.Fatalf("no issue #%d", number)
	return nil
}

func (g *fakeGitHub) routes() []request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// fakeIntake is the Engine side: Receive stores each Trigger ID once.
type fakeIntake struct {
	mu         sync.Mutex
	stored     map[string]core.Trigger
	storeCount map[string]int
	receives   map[string]int // Receive calls per ID
	binds      []bindCall
	closes     []closeCall
	followed   []string
	receiveErr func(core.Trigger) error
	bindErr    error
}

type bindCall struct {
	Ticket, Thread string
	Own            bool
}

type closeCall struct {
	Thread  string
	Outcome core.TicketOutcome
	At      time.Time
}

func newFakeIntake() *fakeIntake {
	return &fakeIntake{stored: map[string]core.Trigger{}, storeCount: map[string]int{}, receives: map[string]int{}}
}

func (f *fakeIntake) Receive(t core.Trigger) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receives[t.ID]++
	if f.receiveErr != nil {
		if err := f.receiveErr(t); err != nil {
			return "", err
		}
	}
	if _, ok := f.stored[t.ID]; ok {
		return core.ReceiveDuplicate, nil
	}
	f.stored[t.ID] = t
	f.storeCount[t.ID]++
	return core.ReceiveStored, nil
}

func (f *fakeIntake) BindThread(ticketID, thread string, own bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.binds = append(f.binds, bindCall{ticketID, thread, own})
	return f.bindErr
}

func (f *fakeIntake) ThreadClosed(thread string, outcome core.TicketOutcome, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, closeCall{thread, outcome, at})
	return nil
}

func (f *fakeIntake) FollowedThreads(source string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if source != "github" {
		return nil, fmt.Errorf("source %q", source)
	}
	return slices.Clone(f.followed), nil
}

// mutations counts the calls that change the Engine.
func (f *fakeIntake) mutations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.binds) + len(f.closes)
	for _, c := range f.receives {
		n += c
	}
	return n
}

func (f *fakeIntake) trigger(t *testing.T, id string) core.Trigger {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	tr, ok := f.stored[id]
	if !ok {
		t.Fatalf("trigger %s not received; have %v", id, slices.Sorted(mapKeys(f.stored)))
	}
	return tr
}

func (f *fakeIntake) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.stored[id]
	return ok
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// memKV is the cursor store; failSet makes chosen writes fail.
type memKV struct {
	mu      sync.Mutex
	m       map[string]string
	failSet func(key string) bool
}

func newMemKV() *memKV { return &memKV{m: map[string]string{}} }

func (k *memKV) KVGet(key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok, nil
}

func (k *memKV) KVSet(key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failSet != nil && k.failSet(key) {
		return errors.New("disk full")
	}
	k.m[key] = value
	return nil
}

func (k *memKV) KVList(prefix string) (map[string]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]string{}
	for key, v := range k.m {
		if strings.HasPrefix(key, prefix) {
			out[key] = v
		}
	}
	return out, nil
}

func (k *memKV) get(key string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.m[key]
}

type sourceHarness struct {
	t     *testing.T
	gh    *fakeGitHub
	in    *fakeIntake
	kv    *memKV
	src   *Source
	waits []time.Duration
}

func newSourceHarness(t *testing.T, cfg SourceConfig) *sourceHarness {
	t.Helper()
	h := &sourceHarness{t: t, gh: newFakeGitHub(t), in: newFakeIntake(), kv: newMemKV()}
	h.src = h.newSource(cfg)
	return h
}

// newSource starts a Source as a restarted process would: a new App and client (no ETags), the same kv and Engine.
func (h *sourceHarness) newSource(cfg SourceConfig) *Source {
	h.t.Helper()
	if cfg.Repos == nil {
		cfg.Repos = []string{"o/r"}
	}
	app := newApp(h.t, h.gh.srv, h.gh.clk)
	s := NewSource(cfg, app, h.in, h.kv, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	s.wait = func(ctx context.Context, d time.Duration) error {
		h.waits = append(h.waits, d)
		h.gh.clk.add(d)
		return ctx.Err()
	}
	return s
}

func (h *sourceHarness) poll() error {
	h.t.Helper()
	return h.src.poll(context.Background())
}

func (h *sourceHarness) mustPoll() {
	h.t.Helper()
	if err := h.poll(); err != nil {
		h.t.Fatalf("poll: %v", err)
	}
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNewIssueMentionBecomesTrigger(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{ReceiveAll: []string{"O/R"}})
	h.mustPoll()

	got := h.in.trigger(t, "github:issue:9001")
	want := core.Trigger{
		ID: "github:issue:9001", Source: "github", Kind: core.KindMessage,
		Actor: core.ActorRef{Source: "github", ID: "alice"}, Human: true,
		Addressed: core.AddressedMention, Scope: "issues",
		Threads: []string{"github:o/r#1"},
		At:      at("2026-11-03T11:10:00Z"),
		Text:    "Flaky login test\n\n@athena-bot please fix the flaky login test.",
		Meta: map[string]string{"repo": "o/r", "number": "1", "kind": "issue", "url": "https://github.com/o/r/issues/1",
			"receiveAll": "true", "association": "MEMBER"},
		Channels: []core.ChannelRef{{Source: "github", Thread: "github:o/r#1", Address: map[string]string{"repo": "o/r", "number": "1"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("issue trigger\n got %+v\nwant %+v", got, want)
	}

	// A person's pull request: Scope pulls, kind pull, its head; the repository receives all in every scope.
	pr := h.in.trigger(t, "github:issue:9003")
	if pr.Scope != "pulls" || pr.Meta["kind"] != "pull" || pr.Meta["head"] != "fix-typo" || pr.Addressed != "" ||
		pr.Meta["receiveAll"] != "true" || pr.Threads[0] != "github:o/r#3" || !pr.Human {
		t.Fatalf("pull trigger = %+v", pr)
	}
	// An old issue that was only updated (closed) is not new: no Trigger for it.
	if h.in.has("github:issue:9004") {
		t.Fatal("an issue created before the cursor became a Trigger")
	}

	// Mentions are whole words, case-insensitive, outside quoted lines.
	c := h.in.trigger(t, "github:issuecomment:104")
	if c.Addressed != core.AddressedMention || c.Scope != "comments" || c.Meta["kind"] != "issue" || c.Threads[0] != "github:o/r#1" {
		t.Fatalf("comment 104 = %+v", c)
	}
	if q := h.in.trigger(t, "github:issuecomment:101"); q.Addressed != "" {
		t.Fatalf("a mention only inside a quote addressed the agent: %+v", q)
	}
	for body, want := range map[string]bool{
		"@athena-bot do it": true, "hey @ATHENA-BOT.": true, "(@athena-bot)": true,
		"@athena-botty": false, "mail@athena-bot": false, "athena-bot": false, "> @athena-bot\nok": false,
	} {
		if got := h.src.addressed(body) == core.AddressedMention; got != want {
			t.Errorf("addressed(%q) = %v", body, got)
		}
	}

	// Review comments thread their replies under the top-level comment and reply to it.
	rc := h.in.trigger(t, "github:reviewcomment:202")
	wantAddr := map[string]string{"repo": "o/r", "number": "2", "comment_id": "201"}
	if rc.Threads[0] != "github:o/r#2" || rc.ReplyTo != "github:reviewcomment:201" || rc.Meta["kind"] != "pull" ||
		!reflect.DeepEqual(rc.Channels, []core.ChannelRef{{Source: "github", Thread: "github:o/r#2", Address: wantAddr}}) ||
		rc.Addressed != core.AddressedMention {
		t.Fatalf("review comment 202 = %+v", rc)
	}
	if top := h.in.trigger(t, "github:reviewcomment:201"); top.Channels[0].Address["comment_id"] != "201" || top.ReplyTo != "" {
		t.Fatalf("review comment 201 = %+v", top)
	}
}

func TestOwnCommentDropped(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.mustPoll()
	for _, id := range []string{"github:issuecomment:102", "github:issue:9002"} {
		if n := h.in.receives[id]; n != 0 {
			t.Fatalf("the App's own %s reached Receive %d times", id, n)
		}
	}
	// The own pull request is still bound: only its Trigger is dropped.
	if len(h.in.binds) != 1 || h.in.binds[0].Thread != "github:o/r#2" {
		t.Fatalf("binds = %+v", h.in.binds)
	}
	// The own login is dropped whatever its case, and so is the App's own check suite.
	if !h.src.own(ghUser{Login: "Athena-Bot[bot]"}) || h.src.own(ghUser{Login: "athena-bot"}) {
		t.Fatal("own() is wrong")
	}
}

func TestOtherBotMarkedBot(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.mustPoll()
	c := h.in.trigger(t, "github:issuecomment:103")
	if !c.Bot || c.Human || c.Actor != (core.ActorRef{Source: "github", ID: "dependabot[bot]"}) ||
		c.Threads[0] != "github:o/r#2" || c.Meta["kind"] != "pull" || c.Meta["number"] != "2" {
		t.Fatalf("bot comment = %+v", c)
	}
	// A login ending in [bot] is a bot even when the type says User.
	if !isBot(ghUser{Login: "renovate[bot]", Type: "User"}) || isBot(ghUser{Login: "bob", Type: "User"}) {
		t.Fatal("isBot is wrong")
	}
}

func TestRestartBetweenFetchAndCursorNoDuplicateTicket(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	// The process dies after Receive and before the cursors are written: no write of a read position lands (the
	// floors are not ones: each is set once and never moves).
	h.kv.failSet = func(key string) bool {
		return strings.HasPrefix(key, "github.cursor.") && !strings.HasSuffix(key, ".ci") && !strings.HasSuffix(key, ".issuesfloor")
	}
	if err := h.poll(); err == nil {
		t.Fatal("a poll whose cursors were not saved succeeded")
	}
	first := len(h.in.stored)
	if first == 0 {
		t.Fatal("nothing received")
	}
	h.kv.failSet = nil
	h.src = h.newSource(SourceConfig{})
	h.mustPoll()
	if len(h.in.stored) != first {
		t.Fatalf("stored %d, then %d after the restart", first, len(h.in.stored))
	}
	dup := 0
	for id, n := range h.in.storeCount {
		if n != 1 {
			t.Errorf("%s stored %d times", id, n)
		}
		if h.in.receives[id] > 1 {
			dup++
		}
	}
	if dup == 0 {
		t.Fatal("the restart re-read nothing: the test does not exercise duplicates")
	}
	if len(h.in.closes) != 2 {
		t.Fatalf("closes repeated after the restart: %+v", h.in.closes)
	}
}

func TestFailedPollDoesNotAdvanceCursor(t *testing.T) {
	t.Run("read fails", func(t *testing.T) {
		h := newSourceHarness(t, SourceConfig{})
		h.gh.failNext("comments", http.StatusInternalServerError)
		if err := h.poll(); err == nil || !errors.Is(err, ErrTransient) {
			t.Fatalf("poll = %v", err)
		}
		if c := h.kv.get("github.cursor.o/r.comments"); c != "2026-11-03T11:00:00Z" {
			t.Fatalf("comments cursor = %q", c)
		}
		if h.in.has("github:issuecomment:101") {
			t.Fatal("a comment arrived from a failed read")
		}
		// The other endpoints went on.
		if !h.in.has("github:issue:9001") || !h.in.has("github:reviewcomment:201") {
			t.Fatal("one failing endpoint stopped the others")
		}
		h.mustPoll()
		for _, id := range []string{"github:issuecomment:101", "github:issuecomment:103", "github:issuecomment:104"} {
			if h.in.receives[id] != 1 || h.in.storeCount[id] != 1 {
				t.Errorf("%s: received %d, stored %d", id, h.in.receives[id], h.in.storeCount[id])
			}
		}
		if c := h.kv.get("github.cursor.o/r.comments"); c != "2026-11-03T11:22:00Z" {
			t.Fatalf("comments cursor after the good poll = %q", c)
		}
	})
	t.Run("receive fails mid-list", func(t *testing.T) {
		h := newSourceHarness(t, SourceConfig{})
		failed := false
		h.in.receiveErr = func(tr core.Trigger) error {
			if tr.ID == "github:issuecomment:104" && !failed {
				failed = true
				return errors.New("database is locked")
			}
			return nil
		}
		if err := h.poll(); err == nil {
			t.Fatal("poll succeeded")
		}
		// 101 (11:12) and the App's own 102 (11:14) are done; 104 (11:16) failed; 103 (11:22) was not reached.
		if c := h.kv.get("github.cursor.o/r.comments"); c != "2026-11-03T11:14:00Z" {
			t.Fatalf("comments cursor = %q", c)
		}
		if h.in.receives["github:issuecomment:103"] != 0 {
			t.Fatal("the endpoint went on after a failed item")
		}
		h.mustPoll()
		for _, id := range []string{"github:issuecomment:101", "github:issuecomment:103", "github:issuecomment:104"} {
			if h.in.storeCount[id] != 1 {
				t.Errorf("%s stored %d times", id, h.in.storeCount[id])
			}
		}
	})
}

func TestHealthAfterThreeFailures(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.gh.failNext("issues", 500, 502, 503)
	for i := 1; i <= 3; i++ {
		if err := h.poll(); err == nil {
			t.Fatalf("poll %d succeeded", i)
		}
		if err := h.src.Health(); (err != nil) != (i == 3) {
			t.Fatalf("after %d failed polls Health = %v", i, err)
		}
	}
	err := h.src.Health()
	if !strings.Contains(err.Error(), "3") || strings.Contains(err.Error(), "ghs_leaked") {
		t.Fatalf("Health = %v", err)
	}
	h.mustPoll()
	if err := h.src.Health(); err != nil {
		t.Fatalf("Health after a good poll = %v", err)
	}
	// One failure after a success starts the count again.
	h.gh.failNext("reviewcomments", 500)
	h.poll()
	if err := h.src.Health(); err != nil {
		t.Fatalf("Health after one failure = %v", err)
	}
}

func TestPRFromTicketBranchBound(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.mustPoll()
	want := []bindCall{{Ticket: "T-0007", Thread: "github:o/r#2", Own: true}}
	if !reflect.DeepEqual(h.in.binds, want) {
		t.Fatalf("binds = %+v", h.in.binds)
	}
	// Seen again (an update moves it past the cursor): not bound again.
	h.gh.edit(func() { h.gh.issue(2)["updated_at"] = "2026-11-03T11:50:00Z" })
	h.mustPoll()
	if len(h.in.binds) != 1 {
		t.Fatalf("bound again: %+v", h.in.binds)
	}
	// Closed without a merge: closed once.
	h.gh.edit(func() {
		it := h.gh.issue(2)
		it["state"], it["closed_at"], it["updated_at"] = "closed", "2026-11-03T11:55:00Z", "2026-11-03T11:55:00Z"
	})
	h.mustPoll()
	h.mustPoll()
	wantClose := []closeCall{{Thread: "github:o/r#2", Outcome: core.OutcomeClosed, At: at("2026-11-03T11:55:00Z")}}
	if got := closesOf(h.in, "github:o/r#2"); !reflect.DeepEqual(got, wantClose) {
		t.Fatalf("closes = %+v", got)
	}
	// Reopened: bound again, and a later close counts again.
	h.gh.edit(func() {
		it := h.gh.issue(2)
		it["state"], it["closed_at"], it["updated_at"] = "open", nil, "2026-11-03T11:58:00Z"
	})
	h.mustPoll()
	if len(h.in.binds) != 2 || h.in.binds[1] != want[0] {
		t.Fatalf("binds after reopen = %+v", h.in.binds)
	}
	if h.kv.get("github.state.github:o/r#2") != "open" {
		t.Fatalf("state = %q", h.kv.get("github.state.github:o/r#2"))
	}
	// Issues and other branches are never bound.
	for _, b := range h.in.binds {
		if b.Thread != "github:o/r#2" {
			t.Fatalf("bound %+v", b)
		}
	}
	for ref, want := range map[string]string{
		"athena/T-0007": "T-0007", "athena/t-12345": "T-12345", "athena/T-7": "", "feature/T-0007": "", "athena/T-0007/x": "",
	} {
		if got := ticketOfBranch(ref); got != want {
			t.Errorf("ticketOfBranch(%q) = %q", ref, got)
		}
	}
	// A bind the Engine refuses (the Ticket ended) does not fail the poll.
	h.in.bindErr = &core.UserError{Msg: "ticket T-0007 has ended"}
	h.gh.edit(func() {
		it := h.gh.issue(2)
		it["state"], it["closed_at"], it["updated_at"] = "closed", "2026-11-03T11:59:00Z", "2026-11-03T11:59:00Z"
	})
	h.mustPoll()
	h.gh.edit(func() {
		it := h.gh.issue(2)
		it["state"], it["closed_at"], it["updated_at"] = "open", nil, "2026-11-03T11:59:30Z"
	})
	h.mustPoll()
}

func closesOf(in *fakeIntake, thread string) []closeCall {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []closeCall
	for _, c := range in.closes {
		if c.Thread == thread {
			out = append(out, c)
		}
	}
	return out
}

func TestMergedPRCallsThreadClosed(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.mustPoll()
	h.mustPoll()
	want := []closeCall{
		{Thread: "github:o/r#4", Outcome: core.OutcomeClosed, At: at("2026-11-03T11:30:00Z")},
		{Thread: "github:o/r#3", Outcome: core.OutcomeMerged, At: at("2026-11-03T11:40:00Z")},
	}
	if !reflect.DeepEqual(h.in.closes, want) {
		t.Fatalf("closes = %+v", h.in.closes)
	}
	if h.kv.get("github.state.github:o/r#3") != "merged" {
		t.Fatalf("state = %q", h.kv.get("github.state.github:o/r#3"))
	}
}

func TestReviewApprovalWithoutBody(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.in.followed = []string{"github:o/r#2", "github:o/r#1", "github:other/repo#2"}
	h.mustPoll()
	got := h.in.trigger(t, "github:review:301")
	want := core.Trigger{
		ID: "github:review:301", Source: "github", Kind: core.KindReview,
		Actor: core.ActorRef{Source: "github", ID: "carol"}, Human: true, Scope: "comments",
		Threads: []string{"github:o/r#2"}, At: at("2026-11-03T11:45:00Z"),
		Meta: map[string]string{"repo": "o/r", "number": "2", "kind": "pull", "head": "athena/t-0007", "state": "APPROVED",
			"association": "MEMBER", "url": "https://github.com/o/r/pull/2#pullrequestreview-301"},
		Channels: []core.ChannelRef{{Source: "github", Thread: "github:o/r#2", Address: map[string]string{"repo": "o/r", "number": "2"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("review\n got %+v\nwant %+v", got, want)
	}
	if h.in.has("github:review:302") {
		t.Fatal("a pending review became a Trigger")
	}
	// Issue #1 is not a pull request: no reviews read for it.
	for _, r := range h.gh.routes() {
		if r.route == "reviews" && r.status != 200 && r.status != 304 {
			t.Fatalf("reviews request answered %d", r.status)
		}
	}
	n := 0
	for _, r := range h.gh.routes() {
		if r.route == "reviews" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d reviews requests", n)
	}
}

// Receive-all is per repository: a repository that is not listed receives mentions only, in every scope.
func TestReceiveAllIsPerRepository(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{ReceiveAll: []string{"o/other"}})
	h.mustPoll()
	for _, id := range []string{"github:issue:9001", "github:issue:9003"} {
		if got := h.in.trigger(t, id); got.Meta["receiveAll"] != "" {
			t.Fatalf("%s of an unlisted repository carries receiveAll: %+v", id, got.Meta)
		}
	}
}

func TestCIOnFollowedPRNotHuman(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{ReceiveAll: []string{"o/r"}})
	h.in.followed = []string{"github:o/r#2"}
	h.mustPoll()
	got := h.in.trigger(t, "github:checksuite:402:failure")
	if got.Kind != core.KindCI || got.Human || got.Bot || got.Scope != "ci" ||
		got.Actor != (core.ActorRef{Source: "github", ID: "github-actions"}) ||
		!reflect.DeepEqual(got.Threads, []string{"github:o/r#2"}) || got.At != at("2026-11-03T11:28:00Z") ||
		got.Meta["receiveAll"] != "true" || got.Meta["sha"] != "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222" ||
		got.Meta["conclusion"] != "failure" || got.Meta["number"] != "2" || got.Meta["head"] != "athena/t-0007" ||
		got.Text == "" || len(got.Channels) != 1 {
		t.Fatalf("PR CI = %+v", got)
	}
	if h.in.has("github:checksuite:403:") || len(h.in.receives) != len(h.in.stored) {
		t.Fatalf("a queued suite became a Trigger: %v", h.in.receives)
	}
	main := h.in.trigger(t, "github:checksuite:401:success")
	if !reflect.DeepEqual(main.Threads, []string{"github:o/r@main"}) || main.Human || main.Kind != core.KindCI ||
		main.Meta["kind"] != "branch" || main.Meta["branch"] != "main" || len(main.Channels) != 0 ||
		main.Meta["url"] != "https://github.com/o/r/commit/aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111/checks" {
		t.Fatalf("default-branch CI = %+v", main)
	}
	if !h.src.Listens("github:o/r@main") {
		t.Fatal("the default-branch Thread is not listened to")
	}
	// Suites from before the Source first started are history, not Triggers.
	h.gh.edit(func() {
		h.gh.suites["aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"]["check_suites"] = []map[string]any{{
			"id": 400, "status": "completed", "conclusion": "success", "head_branch": "main",
			"head_sha": "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111", "app": map[string]any{"slug": "github-actions"},
			"updated_at": "2026-11-03T10:00:00Z",
		}}
	})
	h.mustPoll()
	if h.in.has("github:checksuite:400:success") {
		t.Fatal("an old suite became a Trigger")
	}
}

func TestRateLimitWaitsForReset(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.gh.failNext("issues", http.StatusForbidden)
	start := h.gh.clk.now()
	err := h.poll()
	var rl ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("poll = %v", err)
	}
	if !reflect.DeepEqual(h.waits, []time.Duration{2 * time.Minute}) {
		t.Fatalf("waits = %v", h.waits)
	}
	// Every request after the limit waited for its reset.
	limited := false
	for _, r := range h.gh.routes() {
		if r.route == "issues" && r.status == http.StatusForbidden {
			limited = true
			continue
		}
		if limited && r.at.Before(start.Add(2*time.Minute)) {
			t.Fatalf("%s requested at %v, before the reset", r.route, r.at)
		}
	}
	h.mustPoll()
	if !h.in.has("github:issue:9001") {
		t.Fatal("issues not read after the reset")
	}

	// The wait ends with the context, and a stopped poll is not a failed one.
	h.src.wait = sleepCtx
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.src.blockedUntil = h.gh.clk.now().Add(time.Hour)
	done := make(chan error, 1)
	go func() { done <- h.src.poll(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("poll = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rate-limit wait ignored the context")
	}
}

func TestNotModifiedCostsNothing(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.mustPoll()
	h.mustPoll() // since is inclusive: the last items come back once as duplicates
	before := h.in.mutations()
	n := len(h.gh.routes())
	h.mustPoll()
	if got := h.in.mutations(); got != before {
		t.Fatalf("an unchanged poll made %d Intake calls", got-before)
	}
	third := h.gh.routes()[n:]
	if len(third) == 0 {
		t.Fatal("the third poll made no requests")
	}
	for _, r := range third {
		if r.status != http.StatusNotModified {
			t.Fatalf("an unchanged poll fetched %s (%d)", r.route, r.status)
		}
	}
}

func TestNotModifiedAfterFailedItemIsReadAgain(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	// The first comment fails, so its cursor does not move and the next request is the same one: its 304 must
	// still deliver the comments.
	failed := false
	h.in.receiveErr = func(tr core.Trigger) error {
		if tr.ID == "github:issuecomment:101" && !failed {
			failed = true
			return errors.New("database is locked")
		}
		return nil
	}
	if err := h.poll(); err == nil {
		t.Fatal("poll succeeded")
	}
	h.mustPoll()
	if !h.in.has("github:issuecomment:101") || !h.in.has("github:issuecomment:103") {
		t.Fatal("a 304 hid comments that were never processed")
	}
}

func TestPagesBeyondTheFirst(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.src.perPage = 2
	h.mustPoll()
	for _, id := range []string{"github:issuecomment:101", "github:issuecomment:103", "github:issuecomment:104",
		"github:issue:9001", "github:issue:9003", "github:reviewcomment:201", "github:reviewcomment:202"} {
		if !h.in.has(id) {
			t.Errorf("%s not received", id)
		}
	}
	if c := h.kv.get("github.cursor.o/r.comments"); c != "2026-11-03T11:22:00Z" {
		t.Fatalf("cursor = %q", c)
	}
}

func TestMalformedItemSkipped(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.gh.edit(func() {
		h.gh.comments = append(h.gh.comments, map[string]any{"id": "not-a-number", "updated_at": "2026-11-03T11:17:00Z"})
	})
	h.mustPoll()
	if h.src.Malformed() != 1 || !h.in.has("github:issuecomment:103") {
		t.Fatalf("malformed = %d", h.src.Malformed())
	}
}

func TestListens(t *testing.T) {
	s := NewSource(SourceConfig{Repos: []string{"Owner/Repo"}}, nil, newFakeIntake(), newMemKV(), nil)
	for thread, want := range map[string]bool{
		"github:owner/repo#12": true, "github:Owner/Repo@main": true, "github:OWNER/REPO@release/1.x": true,
		"github:owner/other#1": false, "github:owner/repo#": false, "github:owner/repo#x": false, "github:owner/repo": false,
		"feishu:owner/repo#1": false, "github:owner/repo@": false,
	} {
		if got := s.Listens(thread); got != want {
			t.Errorf("Listens(%q) = %v", thread, got)
		}
	}
	if s.Name() != "github" {
		t.Fatalf("Name = %q", s.Name())
	}
}

func TestRunPollsUntilCancelled(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.src.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.gh.mu.Lock()
		n := 0
		for _, r := range h.gh.requests {
			if r.route == "issues" {
				n++
			}
		}
		h.gh.mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not poll twice")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// commentsQueries returns the since of each comments request, in order.
func (g *fakeGitHub) sinces(route string) []string {
	var out []string
	for _, r := range g.routes() {
		if r.route == route {
			out = append(out, r.query.Get("since")+"|"+r.query.Get("page"))
		}
	}
	return out
}

func TestCommentsOnPullCarryHead(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.mustPoll()
	for _, id := range []string{"github:issuecomment:103", "github:reviewcomment:201", "github:reviewcomment:202"} {
		if got := h.in.trigger(t, id).Meta["head"]; got != "athena/t-0007" {
			t.Errorf("%s head = %q", id, got)
		}
	}
	if _, ok := h.in.trigger(t, "github:issuecomment:104").Meta["head"]; ok {
		t.Error("a comment on an issue has a head")
	}
}

func TestNewIssueAfterFailedNeighbourStillDelivered(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	// #1 was created at 11:10 and last updated at 11:16; #5, an old issue, was updated at 11:13, between the two.
	h.gh.edit(func() {
		h.gh.issues = append(h.gh.issues, map[string]any{
			"id": 9005, "number": 5, "title": "Older", "state": "open", "html_url": "https://github.com/o/r/issues/5",
			"user":       map[string]any{"login": "dave", "id": 14, "type": "User"},
			"created_at": "2026-09-01T09:00:00Z", "updated_at": "2026-11-03T11:13:00Z",
		})
	})
	failed := false
	h.in.receiveErr = func(tr core.Trigger) error {
		if tr.ID == "github:issue:9001" && !failed {
			failed = true
			return errors.New("database is locked")
		}
		return nil
	}
	if err := h.poll(); err == nil {
		t.Fatal("poll succeeded")
	}
	if c := h.kv.get("github.cursor.o/r.issues"); c != "2026-11-03T11:13:00Z" {
		t.Fatalf("issues cursor = %q", c)
	}
	h.mustPoll()
	h.mustPoll()
	if h.in.storeCount["github:issue:9001"] != 1 {
		t.Fatalf("the opening Trigger of #1 stored %d times", h.in.storeCount["github:issue:9001"])
	}
	if f := h.kv.get("github.cursor.o/r.issuesfloor"); f != "2026-11-03T11:00:00Z" {
		t.Fatalf("issues floor = %q", f)
	}
}

func TestThreadKeysCanonical(t *testing.T) {
	if ThreadKey("Owner/Repo", 5) != "github:owner/repo#5" || BranchThreadKey("Owner/Repo", "Main") != "github:owner/repo@Main" {
		t.Fatalf("ThreadKey = %q, BranchThreadKey = %q", ThreadKey("Owner/Repo", 5), BranchThreadKey("Owner/Repo", "Main"))
	}
	h := newSourceHarness(t, SourceConfig{Repos: []string{"O/R"}})
	h.in.followed = []string{"github:O/R#2"}
	h.mustPoll()
	for id, thread := range map[string]string{
		"github:issue:9001": "github:o/r#1", "github:issuecomment:103": "github:o/r#2", "github:reviewcomment:201": "github:o/r#2",
		"github:review:301": "github:o/r#2", "github:checksuite:402:failure": "github:o/r#2", "github:checksuite:401:success": "github:o/r@main",
	} {
		tr := h.in.trigger(t, id)
		if tr.Threads[0] != thread || tr.Meta["repo"] != "o/r" {
			t.Errorf("%s: thread %q, repo %q", id, tr.Threads[0], tr.Meta["repo"])
		}
		for _, c := range tr.Channels {
			if c.Thread != thread {
				t.Errorf("%s: channel thread %q", id, c.Thread)
			}
		}
	}
	if len(h.in.binds) != 1 || h.in.binds[0].Thread != "github:o/r#2" {
		t.Fatalf("binds = %+v", h.in.binds)
	}
	if got := closesOf(h.in, "github:o/r#3"); len(got) != 1 {
		t.Fatalf("closes = %+v", h.in.closes)
	}
	if h.kv.get("github.cursor.o/r.comments") == "" || h.kv.get("github.state.github:o/r#3") != "merged" {
		t.Fatalf("kv = %v", h.kv.m)
	}
	if !h.src.Listens("github:o/r#2") || !h.src.Listens("github:O/R#2") {
		t.Fatal("Listens")
	}
}

func TestGoneThreadStopsFailing(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.in.followed = []string{"github:o/r#9"}
	h.gh.failNext("issue", http.StatusGone, http.StatusGone, http.StatusGone)
	h.mustPoll()
	// R19: the Thread is dropped and remembered as gone; the Engine is not told it closed.
	if got := closesOf(h.in, "github:o/r#9"); len(got) != 0 {
		t.Fatalf("closes = %+v", got)
	}
	if got := h.kv.get("github.state.github:o/r#9"); got != "gone" {
		t.Fatalf("state = %q", got)
	}
	if _, ok := h.src.Dropped()["github:o/r#9"]; !ok {
		t.Fatalf("dropped = %v", h.src.Dropped())
	}
	h.mustPoll()
	n := 0
	for _, r := range h.gh.routes() {
		if r.route == "issue" {
			n++
		}
	}
	if n != 1 || len(closesOf(h.in, "github:o/r#9")) != 0 {
		t.Fatalf("a gone Thread was read %d times", n)
	}
	if err := h.src.Health(); err != nil {
		t.Fatal(err)
	}
}

// Dropped survives a restart: it is rebuilt from the Threads remembered as gone.
func TestDroppedSurvivesRestart(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.in.followed = []string{"github:o/r#9"}
	h.gh.failNext("issue", http.StatusGone)
	h.mustPoll()
	h.kv.KVSet("github.state.github:o/r#7", "closed") // other states are not gone
	s := h.newSource(SourceConfig{})
	if len(s.Dropped()) != 0 {
		t.Fatalf("a new Source starts with %v", s.Dropped())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got := s.Dropped()
	if len(got) != 1 || !strings.HasPrefix(got["github:o/r#9"], "gone") {
		t.Fatalf("dropped after restart = %v", got)
	}
	// A successful read of the Thread brings it back: no longer dropped, here or after another restart.
	if err := s.readOK("github:o/r#9"); err != nil {
		t.Fatal(err)
	}
	if len(s.Dropped()) != 0 || h.kv.get("github.state.github:o/r#9") == "gone" {
		t.Fatalf("dropped = %v, state = %q", s.Dropped(), h.kv.get("github.state.github:o/r#9"))
	}
}

// R19: reads that have failed for more than two days are a fresh start, not a reason to stay dropped.
func TestStaleFailureStartsFresh(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.gh.edit(func() { delete(h.gh.pulls, "3") }) // /pulls/3 answers 404
	h.kv.KVSet("github.failing.github:o/r#3", h.gh.clk.now().Add(-49*time.Hour).UTC().Format(time.RFC3339))
	if err := h.poll(); err == nil {
		t.Fatal("poll succeeded")
	}
	if len(h.src.Dropped()) != 0 {
		t.Fatalf("dropped = %v", h.src.Dropped())
	}
	if got := h.kv.get("github.failing.github:o/r#3"); got != h.gh.clk.now().UTC().Format(time.RFC3339) {
		t.Fatalf("first failure = %q, want now", got)
	}
	// Between one and two days it is dropped, as before.
	h.kv.KVSet("github.failing.github:o/r#3", h.gh.clk.now().Add(-25*time.Hour).UTC().Format(time.RFC3339))
	h.mustPoll()
	if _, ok := h.src.Dropped()["github:o/r#3"]; !ok {
		t.Fatalf("dropped = %v", h.src.Dropped())
	}
}

func TestLongFailingItemDropped(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	var pull3 map[string]any
	h.gh.edit(func() {
		pull3 = h.gh.pulls["3"]
		delete(h.gh.pulls, "3")                              // /pulls/3 answers 404
		h.gh.issue(1)["updated_at"] = "2026-11-03T11:50:00Z" // after #3 (11:40)
	})
	for i := 0; i < 2; i++ { // the first failure, and an hour later
		if err := h.poll(); err == nil {
			t.Fatalf("poll %d succeeded", i)
		}
		h.gh.clk.add(time.Hour)
	}
	if c := h.kv.get("github.cursor.o/r.issues"); c != "2026-11-03T11:30:00Z" {
		t.Fatalf("issues cursor = %q", c)
	}
	h.gh.clk.add(23 * time.Hour)
	h.mustPoll()
	if c := h.kv.get("github.cursor.o/r.issues"); c != "2026-11-03T11:50:00Z" {
		t.Fatalf("the failing item still blocks its endpoint: cursor = %q", c)
	}
	if _, ok := h.src.Dropped()["github:o/r#3"]; !ok || len(h.src.Dropped()) != 1 {
		t.Fatalf("dropped = %v", h.src.Dropped())
	}
	// It reads again later: the failure is cleared.
	h.gh.edit(func() {
		h.gh.pulls["3"] = pull3
		h.gh.issue(3)["updated_at"] = "2026-11-03T11:55:00Z"
	})
	h.mustPoll()
	if len(h.src.Dropped()) != 0 || h.kv.get("github.failing.github:o/r#3") != "" {
		t.Fatalf("dropped = %v, failing = %q", h.src.Dropped(), h.kv.get("github.failing.github:o/r#3"))
	}
}

func TestForkPRNotBound(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.gh.edit(func() {
		h.gh.pulls["2"]["head"].(map[string]any)["repo"] = map[string]any{"full_name": "mallory/r"}
		h.gh.pulls["2"]["head"].(map[string]any)["ref"] = "athena/T-0007"
	})
	h.mustPoll()
	if len(h.in.binds) != 0 {
		t.Fatalf("a fork's pull request was bound: %+v", h.in.binds)
	}
	h2 := newSourceHarness(t, SourceConfig{})
	h2.gh.edit(func() { h2.gh.pulls["2"]["head"].(map[string]any)["repo"] = map[string]any{"full_name": "O/R"} })
	h2.mustPoll()
	if len(h2.in.binds) != 1 {
		t.Fatalf("binds = %+v", h2.in.binds)
	}
}

func TestKeysetPagingDeliversItemUpdatedBetweenPages(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.src.perPage = 2
	first := true
	h.gh.after = func(route string, q url.Values) {
		if route == "comments" && first {
			// 101 is edited after the first page: it moves to the end of the list.
			first = false
			for _, c := range h.gh.comments {
				if c["id"] == float64(101) {
					c["updated_at"] = "2026-11-03T11:30:00Z"
				}
			}
		}
	}
	h.mustPoll()
	for _, id := range []string{"github:issuecomment:101", "github:issuecomment:103", "github:issuecomment:104"} {
		if h.in.storeCount[id] != 1 {
			t.Errorf("%s stored %d times", id, h.in.storeCount[id])
		}
	}
	if h.in.receives["github:issuecomment:104"] < 2 {
		t.Fatal("no overlap between pages")
	}
	if c := h.kv.get("github.cursor.o/r.comments"); c != "2026-11-03T11:30:00Z" {
		t.Fatalf("cursor = %q", c)
	}
}

func TestKeysetPagingSameUpdatedAtAndCap(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.src.perPage = 2
	h.gh.edit(func() {
		for _, c := range h.gh.comments {
			c["updated_at"] = "2026-11-03T11:12:00Z"
		}
	})
	h.mustPoll()
	for _, id := range []string{"github:issuecomment:101", "github:issuecomment:103", "github:issuecomment:104"} {
		if h.in.storeCount[id] != 1 {
			t.Errorf("%s stored %d times", id, h.in.storeCount[id])
		}
	}
	// The rounds of one endpoint are capped; the rest comes next poll. (With one item a page, the overlap takes a
	// round and an item sharing the new since another: three rounds move one item on.)
	h2 := newSourceHarness(t, SourceConfig{})
	h2.src.perPage, h2.src.maxRounds = 1, 3
	h2.mustPoll()
	if h2.in.has("github:issuecomment:103") {
		t.Fatal("the cap did not stop the endpoint")
	}
	for i := 0; i < 10 && !h2.in.has("github:issuecomment:103"); i++ {
		h2.mustPoll()
	}
	if !h2.in.has("github:issuecomment:103") {
		t.Fatal("later polls did not go on")
	}
}

func TestPollOverlapsCursor(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{})
	h.mustPoll()
	h.mustPoll()
	got := h.gh.sinces("comments")
	if want := []string{"2026-11-03T11:00:00Z|", "2026-11-03T11:21:00Z|"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("comments since = %v", got)
	}
}

func TestTriggerCarriesAssociation(t *testing.T) {
	h := newSourceHarness(t, SourceConfig{ReceiveAll: []string{"O/R"}})
	h.mustPoll()
	// The fixture's issue comments carry author_association at the top level of the item.
	for id, want := range map[string]string{
		"github:issuecomment:101": "MEMBER", "github:issuecomment:103": "NONE",
		"github:issue:9003": "MEMBER", "github:reviewcomment:201": "MEMBER",
	} {
		if got := h.in.trigger(t, id).Meta["association"]; got != want {
			t.Errorf("%s association = %q, want %q", id, got, want)
		}
	}
	// A payload without the field yields no key at all.
	r := repo{key: "o/r", full: "o/r"}
	tr := h.src.trigger("x", core.KindMessage, "comments", r, 1, false, ghUser{Login: "zed", Type: "User"}, "", at("2026-11-03T11:10:00Z"), "hi", "")
	if v, ok := tr.Meta["association"]; ok {
		t.Errorf("association = %q for a payload without author_association", v)
	}
	tr = h.src.trigger("x", core.KindMessage, "comments", r, 1, false, ghUser{Login: "zed", Type: "User"}, "COLLABORATOR", at("2026-11-03T11:10:00Z"), "hi", "")
	if tr.Meta["association"] != "COLLABORATOR" {
		t.Errorf("association = %q", tr.Meta["association"])
	}
}
