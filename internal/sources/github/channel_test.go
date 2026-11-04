package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/sources"
)

var (
	_ sources.Channel       = (*Source)(nil)
	_ sources.ThreadContext = (*Source)(nil)
)

// chReq is one request the fake GitHub received.
type chReq struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

func (r chReq) String() string { return r.Method + " " + r.Path }

// chFake is a GitHub that answers by "METHOD /path" with a handler, 404 otherwise, and records every request.
type chFake struct {
	t   *testing.T
	srv *httptest.Server
	clk *clock

	mu       sync.Mutex
	routes   map[string]http.HandlerFunc
	requests []chReq
}

func newChFake(t *testing.T) *chFake {
	f := &chFake{t: t, clk: newClock(), routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		f.mu.Lock()
		h := f.routes[r.Method+" "+r.URL.Path]
		if r.URL.Path != "/app/installations/777/access_tokens" {
			f.requests = append(f.requests, chReq{r.Method, r.URL.Path, r.URL.Query(), parsed})
		}
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/app/installations/777/access_tokens":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_installationsecret","expires_at":%q}`, f.clk.now().Add(time.Hour).Format(time.RFC3339))
		case r.URL.Path == "/users/athena-bot[bot]":
			fmt.Fprint(w, `{"login":"athena-bot[bot]","id":4242,"type":"Bot"}`)
		case h != nil:
			h(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *chFake) route(method, path string, h http.HandlerFunc) {
	f.mu.Lock()
	f.routes[method+" "+path] = h
	f.mu.Unlock()
}

// reply answers with a status and a JSON body.
func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

func (f *chFake) reqs() []chReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chReq(nil), f.requests...)
}

// posts are the POSTs, in order.
func (f *chFake) posts() []chReq {
	var out []chReq
	for _, r := range f.reqs() {
		if r.Method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

func (f *chFake) source() *Source {
	f.t.Helper()
	app := newApp(f.t, f.srv, f.clk)
	return NewSource(SourceConfig{Repos: []string{"o/r"}}, app, newFakeIntake(), newMemKV(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func addr(repo string, n int, commentID string) core.ChannelRef {
	a := map[string]string{"repo": repo, "number": fmt.Sprint(n)}
	if commentID != "" {
		a["comment_id"] = commentID
	}
	return core.ChannelRef{Source: SourceName, Thread: ThreadKey(repo, n), Address: a}
}

// queuedAt is when the receipts of these tests were queued: half an hour before the fake clock's now.
func queuedAt() time.Time { return newClock().now().Add(-30 * time.Minute) }

func req(receiptID string, attempt int, text string) sources.SendRequest {
	return sources.SendRequest{ReceiptID: receiptID, Attempt: attempt, QueuedAt: queuedAt(), Text: text}
}

const (
	issueComments = "/repos/o/r/issues/5/comments"
	reviewReplies = "/repos/o/r/pulls/5/comments/77/replies"
	reviewList    = "/repos/o/r/pulls/5/comments"
)

func TestSendIssueComment(t *testing.T) {
	f := newChFake(t)
	f.route("POST", issueComments, reply(201, `{"id":1}`))
	s := f.source()
	res, err := s.Send(context.Background(), addr("O/r", 5, ""), req("rcpt-1", 0, "Hello there."))
	if err != nil || res != sources.Sent {
		t.Fatalf("Send = %v, %v", res, err)
	}
	posts := f.posts()
	if len(posts) != 1 || posts[0].Path != issueComments {
		t.Fatalf("posts = %v", posts)
	}
	want := "Hello there.\n\n<!-- athena:receipt:rcpt-1 -->"
	if posts[0].Body["body"] != want {
		t.Fatalf("body = %q, want %q", posts[0].Body["body"], want)
	}
	for _, r := range f.reqs() { // a first attempt never lists
		if r.Method == http.MethodGet && strings.Contains(r.Path, "/comments") {
			t.Fatalf("the first attempt listed comments: %v", r)
		}
	}
}

func TestReplyToReviewComment(t *testing.T) {
	f := newChFake(t)
	f.route("POST", reviewReplies, reply(201, `{"id":2}`))
	res, err := f.source().Send(context.Background(), addr("o/r", 5, "77"), req("rcpt-2", 0, "Renamed."))
	if err != nil || res != sources.Sent {
		t.Fatalf("Send = %v, %v", res, err)
	}
	posts := f.posts()
	if len(posts) != 1 || posts[0].Path != reviewReplies || posts[0].Body["body"] != "Renamed.\n\n<!-- athena:receipt:rcpt-2 -->" {
		t.Fatalf("posts = %+v", posts)
	}
}

func TestDeletedReviewCommentFallsBack(t *testing.T) {
	f := newChFake(t)
	f.route("POST", reviewReplies, reply(404, `{"message":"Not Found"}`))
	f.route("POST", issueComments, reply(201, `{"id":3}`))
	res, err := f.source().Send(context.Background(), addr("o/r", 5, "77"), req("rcpt-3", 0, "Renamed."))
	if err != nil || res != sources.Sent {
		t.Fatalf("Send = %v, %v", res, err)
	}
	posts := f.posts()
	if len(posts) != 2 || posts[0].Path != reviewReplies || posts[1].Path != issueComments {
		t.Fatalf("posts = %v", posts)
	}
	if posts[1].Body["body"] != "Renamed.\n\n<!-- athena:receipt:rcpt-3 -->" {
		t.Fatalf("fallback body = %q", posts[1].Body["body"])
	}
}

func TestDeletedPullRequestIsGone(t *testing.T) {
	f := newChFake(t) // both posts answer 404
	res, err := f.source().Send(context.Background(), addr("o/r", 5, "77"), req("rcpt", 0, "Hi."))
	if err != nil || res != sources.Gone {
		t.Fatalf("Send = %v, %v", res, err)
	}
	if len(f.posts()) != 2 {
		t.Fatalf("posts = %v", f.posts())
	}
	f = newChFake(t)
	f.route("POST", issueComments, reply(410, `{"message":"This issue was deleted"}`))
	if res, err := f.source().Send(context.Background(), addr("o/r", 5, ""), req("rcpt", 0, "Hi.")); err != nil || res != sources.Gone {
		t.Fatalf("410: Send = %v, %v", res, err)
	}
}

func TestLockedIsGone(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"422 message", 422, `{"message":"Unable to create comment because issue is locked.","documentation_url":"x"}`},
		{"422 errors[]", 422, `{"message":"Validation Failed","errors":[{"resource":"Issue","code":"custom","message":"Conversation is locked"}]}`},
		{"403 message", 403, `{"message":"Unable to create comment because issue is locked."}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newChFake(t)
			f.route("POST", issueComments, reply(c.status, c.body))
			f.route("POST", reviewReplies, reply(c.status, c.body))
			for _, ch := range []core.ChannelRef{addr("o/r", 5, ""), addr("o/r", 5, "77")} {
				res, err := f.source().Send(context.Background(), ch, req("rcpt", 0, "Hi."))
				if err != nil || res != sources.Gone {
					t.Fatalf("Send(%v) = %v, %v", ch.Address, res, err)
				}
			}
			// A locked reply target does not fall back: the issue comment would be locked too.
			if len(f.posts()) != 2 {
				t.Fatalf("a locked reply fell back: posts = %v", f.posts())
			}
		})
	}
}

func TestServerErrorRetriable(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		class  error
	}{
		{"502", 502, `{"message":"Bad Gateway"}`, ErrTransient},
		{"403 permission", 403, `{"message":"Resource not accessible by integration"}`, ErrPermanent},
		{"422 other", 422, `{"message":"Validation Failed"}`, ErrPermanent},
		{"rate limit", 429, `{"message":"rate limit"}`, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newChFake(t)
			f.route("POST", issueComments, reply(c.status, c.body))
			res, err := f.source().Send(context.Background(), addr("o/r", 5, ""), req("rcpt", 0, "Hi."))
			if err == nil || res == sources.Gone || res == sources.Sent {
				t.Fatalf("Send = %v, %v; want an error", res, err)
			}
			if c.class != nil && !errors.Is(err, c.class) {
				t.Fatalf("err = %v, want class %v", err, c.class)
			}
			// A permanent failure is a refusal for the Outbox, which drops the message; anything else is retried.
			if refused := errors.Is(err, sources.ErrRefused); refused != (c.class == ErrPermanent) {
				t.Fatalf("err = %v: refused %v", err, refused)
			}
		})
	}
}

func TestRetryFindsMarkerNoPost(t *testing.T) {
	f := newChFake(t)
	marker := "<!-- athena:receipt:rcpt-9 -->"
	f.route("GET", issueComments, reply(200, fmt.Sprintf(`[
		{"id":1,"user":{"login":"alice","id":11},"body":"quoting %s"},
		{"id":2,"user":{"login":"athena-bot[bot]","id":4242,"type":"Bot"},"body":"Done.\n\n%s"}]`, marker, marker)))
	f.route("POST", issueComments, reply(201, `{}`))
	res, err := f.source().Send(context.Background(), addr("o/r", 5, ""), req("rcpt-9", 2, "Done."))
	if err != nil || res != sources.Sent {
		t.Fatalf("Send = %v, %v", res, err)
	}
	if len(f.posts()) != 0 {
		t.Fatalf("a message already there was posted again: %v", f.posts())
	}
	list := f.reqs()
	var got chReq
	for _, r := range list {
		if r.Method == "GET" && r.Path == issueComments {
			got = r
		}
	}
	// R20: the check reads from the time the receipt was queued, not from an hour ago.
	since, err := time.Parse(time.RFC3339, got.Query.Get("since"))
	if err != nil || !since.Equal(queuedAt()) {
		t.Fatalf("since = %q, want the queued time %s", got.Query.Get("since"), queuedAt().Format(time.RFC3339))
	}
}

// With no queued time the check falls back to the last hour.
func TestRetryWithoutQueuedTimeLooksBackAnHour(t *testing.T) {
	f := newChFake(t)
	f.route("GET", issueComments, reply(200, `[]`))
	f.route("POST", issueComments, reply(201, `{}`))
	r := req("rcpt", 1, "Hi.")
	r.QueuedAt = time.Time{}
	if res, err := f.source().Send(context.Background(), addr("o/r", 5, ""), r); err != nil || res != sources.Sent {
		t.Fatalf("Send = %v, %v", res, err)
	}
	for _, q := range f.reqs() {
		if q.Method == "GET" && q.Path == issueComments {
			if since, _ := time.Parse(time.RFC3339, q.Query.Get("since")); !since.Equal(f.clk.now().Add(-time.Hour)) {
				t.Fatalf("since = %q", q.Query.Get("since"))
			}
		}
	}
}

func TestRetryWithoutOwnMarkerPosts(t *testing.T) {
	f := newChFake(t)
	marker := "<!-- athena:receipt:rcpt-9 -->"
	// Another user's comment with the marker, and the bot's comment with another receipt's marker.
	f.route("GET", issueComments, reply(200, fmt.Sprintf(`[
		{"id":1,"user":{"login":"alice","id":11},"body":"%s"},
		{"id":2,"user":{"login":"athena-bot[bot]","id":4242},"body":"Other.\n\n<!-- athena:receipt:rcpt-8 -->"}]`, marker)))
	f.route("POST", issueComments, reply(201, `{}`))
	res, err := f.source().Send(context.Background(), addr("o/r", 5, ""), req("rcpt-9", 1, "Done."))
	if err != nil || res != sources.Sent || len(f.posts()) != 1 {
		t.Fatalf("Send = %v, %v; posts %v", res, err, f.posts())
	}
}

func TestRetryChecksReviewRepliesAndTheFallback(t *testing.T) {
	marker := "<!-- athena:receipt:rcpt-4 -->"
	bot := `"user":{"login":"athena-bot[bot]","id":4242}`
	// The reply went out before the failure.
	f := newChFake(t)
	f.route("GET", reviewList, reply(200, fmt.Sprintf(`[{"id":78,%s,"body":"Renamed.\n\n%s"}]`, bot, marker)))
	f.route("GET", issueComments, reply(200, `[]`))
	f.route("POST", reviewReplies, reply(201, `{}`))
	if res, err := f.source().Send(context.Background(), addr("o/r", 5, "77"), req("rcpt-4", 1, "Renamed.")); err != nil || res != sources.Sent {
		t.Fatalf("Send = %v, %v", res, err)
	}
	if len(f.posts()) != 0 {
		t.Fatalf("posted again: %v", f.posts())
	}
	// The reply target was gone and the fallback issue comment went out before the failure.
	f = newChFake(t)
	f.route("GET", reviewList, reply(200, `[]`))
	f.route("GET", issueComments, reply(200, fmt.Sprintf(`[{"id":9,%s,"body":"Renamed.\n\n%s"}]`, bot, marker)))
	f.route("POST", reviewReplies, reply(201, `{}`))
	if res, err := f.source().Send(context.Background(), addr("o/r", 5, "77"), req("rcpt-4", 1, "Renamed.")); err != nil || res != sources.Sent {
		t.Fatalf("Send = %v, %v", res, err)
	}
	if len(f.posts()) != 0 {
		t.Fatalf("posted again: %v", f.posts())
	}
}

func TestRetryListingFailureIsRetriedNotPosted(t *testing.T) {
	f := newChFake(t)
	f.route("GET", issueComments, reply(502, `{"message":"Bad Gateway"}`))
	f.route("POST", issueComments, reply(201, `{}`))
	res, err := f.source().Send(context.Background(), addr("o/r", 5, ""), req("rcpt", 1, "Hi."))
	if err == nil || res == sources.Sent || len(f.posts()) != 0 {
		t.Fatalf("Send = %v, %v; posts %v", res, err, f.posts())
	}
	// A listing the conversation's deletion fails with 410 ends the message; a 404 lets the post decide.
	f = newChFake(t)
	f.route("GET", issueComments, reply(410, `{"message":"gone"}`))
	if res, err := f.source().Send(context.Background(), addr("o/r", 5, ""), req("rcpt", 1, "Hi.")); err != nil || res != sources.Gone {
		t.Fatalf("410 listing: Send = %v, %v", res, err)
	}
	f = newChFake(t) // the listing and the post both answer 404
	if res, err := f.source().Send(context.Background(), addr("o/r", 5, ""), req("rcpt", 1, "Hi.")); err != nil || res != sources.Gone {
		t.Fatalf("404 listing: Send = %v, %v", res, err)
	}
}

func TestRetryPagesTheListing(t *testing.T) {
	f := newChFake(t)
	marker := "<!-- athena:receipt:rcpt-p -->"
	f.route("GET", issueComments, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprintf(w, `[{"id":2,"user":{"login":"athena-bot[bot]","id":4242},"body":"x\n\n%s"}]`, marker)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, f.srv.URL, issueComments))
		fmt.Fprint(w, `[{"id":1,"user":{"login":"alice","id":11},"body":"hi"}]`)
	})
	if res, err := f.source().Send(context.Background(), addr("o/r", 5, ""), req("rcpt-p", 1, "x")); err != nil || res != sources.Sent || len(f.posts()) != 0 {
		t.Fatalf("Send = %v, %v; posts %v", res, err, f.posts())
	}
}

func TestSendRefusesBadAddresses(t *testing.T) {
	f := newChFake(t)
	s := f.source()
	for _, ch := range []core.ChannelRef{
		{Source: SourceName, Thread: "github:o/r#5", Address: map[string]string{"number": "5"}},
		{Source: SourceName, Thread: "github:o/r#5", Address: map[string]string{"repo": "o/r"}},
		{Source: SourceName, Thread: "github:o/r#5", Address: map[string]string{"repo": "o/r", "number": "0"}},
		{Source: SourceName, Thread: "github:o/r#5", Address: map[string]string{"repo": "o/r/../x", "number": "5"}},
		{Source: SourceName, Thread: "github:o/r#5", Address: map[string]string{"repo": "o/r", "number": "5", "comment_id": "7/../x"}},
		{Source: SourceName, Thread: "github:x/y#5", Address: map[string]string{"repo": "x/y", "number": "5"}}, // not configured
		// R21: the address must match the Thread it is sent for.
		{Source: SourceName, Thread: "github:o/r#6", Address: map[string]string{"repo": "o/r", "number": "5"}},
		{Source: SourceName, Thread: "github:o/r#5", Address: map[string]string{"repo": "o/r", "number": "6"}},
		{Source: SourceName, Thread: "", Address: map[string]string{"repo": "o/r", "number": "5"}},
		{Source: "feishu", Thread: "feishu:c", Address: map[string]string{"repo": "o/r", "number": "5"}},
	} {
		if res, err := s.Send(context.Background(), ch, req("rcpt", 0, "Hi.")); err == nil || !errors.Is(err, sources.ErrRefused) {
			t.Errorf("Send(%v) = %v, %v; want a refusal", ch, res, err)
		}
	}
	if len(f.reqs()) != 0 {
		t.Fatalf("requests = %v", f.reqs())
	}
}

func TestSendCutsAnOversizedBody(t *testing.T) {
	f := newChFake(t)
	f.route("POST", issueComments, reply(201, `{}`))
	long := strings.Repeat("é", 70000)
	if res, err := f.source().Send(context.Background(), addr("o/r", 5, ""), req("rcpt", 0, long)); err != nil || res != sources.Sent {
		t.Fatalf("Send = %v, %v", res, err)
	}
	body := f.posts()[0].Body["body"].(string)
	if n := len([]rune(body)); n > 65536 || !strings.HasSuffix(body, "\n\n<!-- athena:receipt:rcpt -->") {
		t.Fatalf("body is %d runes; ends %q", n, body[len(body)-40:])
	}
}

func TestLink(t *testing.T) {
	s := newChFake(t).source()
	for _, c := range []struct {
		ch   core.ChannelRef
		want string
	}{
		{addr("O/r", 5, ""), "https://github.com/O/r/issues/5"},
		{addr("o/r", 5, "77"), "https://github.com/o/r/pull/5#discussion_r77"},
		{core.ChannelRef{Source: SourceName, Address: map[string]string{"repo": "../x", "number": "5"}}, ""},
	} {
		if got := s.Link(c.ch); got != c.want {
			t.Errorf("Link(%v) = %q, want %q", c.ch.Address, got, c.want)
		}
	}
}
