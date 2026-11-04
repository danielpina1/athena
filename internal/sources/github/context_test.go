package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/sources"
)

// serveThread serves one issue and its comments the way GitHub does.
func (f *chFake) serveThread(n int, issue map[string]any, comments, reviewComments, reviews []map[string]any) {
	js := func(v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(v) }
	}
	base := fmt.Sprintf("/repos/o/r/issues/%d", n)
	f.route("GET", base, js(issue))
	f.route("GET", base+"/comments", js(comments))
	f.route("GET", fmt.Sprintf("/repos/o/r/pulls/%d/comments", n), js(reviewComments))
	f.route("GET", fmt.Sprintf("/repos/o/r/pulls/%d/reviews", n), js(reviews))
}

func person(login string, id int) map[string]any {
	return map[string]any{"login": login, "id": id, "type": "User"}
}

func ts(min int) string {
	return time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute).Format(time.RFC3339)
}

func TestContextWindowShape(t *testing.T) {
	f := newChFake(t)
	issue := map[string]any{"id": 5000, "number": 5, "title": "Big thread", "body": strings.Repeat("b", 500),
		"user": person("alice", 11), "created_at": ts(0)}
	var comments []map[string]any
	for i := 1; i <= 30; i++ {
		text := fmt.Sprintf("comment\n%d  ", i)     // whitespace runs collapse in a stub's preview
		text += strings.Repeat("x", 2000-len(text)) // 2,000 runes
		comments = append(comments, map[string]any{"id": 100 + i, "user": person("bob", 12), "body": text, "created_at": ts(i)})
	}
	f.serveThread(5, issue, comments, nil, nil)

	w, err := f.source().Context(t.Context(), "github:o/r#5", "github:issuecomment:120") // the 20th comment
	if err != nil {
		t.Fatal(err)
	}
	// 31 messages (the body is message 0): the first five, and five on either side of comment 20 (messages 15..25).
	var ids []string
	for _, m := range w.Messages {
		ids = append(ids, m.ID)
	}
	want := []string{"github:issue:5000"}
	for _, i := range []int{1, 2, 3, 4, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25} {
		want = append(want, fmt.Sprintf("github:issuecomment:%d", 100+i))
	}
	if strings.Join(ids, " ") != strings.Join(want, " ") || len(w.Messages) != 16 {
		t.Fatalf("messages = %v\nwant       %v", ids, want)
	}
	if len(w.Stubs) != 15 {
		t.Fatalf("%d stubs, want 15", len(w.Stubs))
	}
	// Text: 400 runes; 1,200 for the two on either side of the Trigger; Chars is the length before the cut.
	limits := map[string]int{}
	for _, m := range w.Messages {
		n := len([]rune(m.Text))
		limits[m.ID] = n
		wantN, wantChars := 400, 2000
		switch m.ID {
		case "github:issuecomment:118", "github:issuecomment:119", "github:issuecomment:121", "github:issuecomment:122":
			wantN = 1200
		case "github:issue:5000":
			wantChars = len([]rune("Big thread\n\n" + issue["body"].(string))) // 512
		}
		if n != wantN || m.Chars != wantChars {
			t.Errorf("%s: %d runes, Chars %d; want %d, %d", m.ID, n, m.Chars, wantN, wantChars)
		}
	}
	m := w.Messages[0]
	if m.From != "alice" || m.At != ts(0) || !strings.HasPrefix(m.Text, "Big thread\n\nbbb") {
		t.Fatalf("message 0 = %+v", m)
	}
	if m := w.Messages[len(w.Messages)-1]; m.From != "bob" || m.At != ts(25) {
		t.Fatalf("last message = %+v", m)
	}
	// Stubs: 50 runes, in thread order, with the rest of the thread.
	for _, st := range w.Stubs {
		if len([]rune(st.Preview)) != 50 || st.From != "bob" {
			t.Errorf("stub %+v", st)
		}
	}
	if w.Stubs[0].ID != "github:issuecomment:105" || w.Stubs[len(w.Stubs)-1].ID != "github:issuecomment:130" {
		t.Fatalf("stubs run %s .. %s", w.Stubs[0].ID, w.Stubs[len(w.Stubs)-1].ID)
	}
	if !strings.HasPrefix(w.Stubs[0].Preview, "comment 5 xxx") {
		t.Fatalf("preview = %q", w.Stubs[0].Preview)
	}
}

func TestContextShortTextsAreNotCut(t *testing.T) {
	f := newChFake(t)
	f.serveThread(5, map[string]any{"id": 5000, "number": 5, "title": "T", "body": "", "user": person("alice", 11), "created_at": ts(0)},
		[]map[string]any{
			{"id": 1, "user": person("bob", 12), "body": strings.Repeat("é", 400), "created_at": ts(1)},
			{"id": 2, "user": person("bob", 12), "body": "multi\nline   preview\n", "created_at": ts(2)},
		}, nil, nil)
	w, err := f.source().Context(t.Context(), "github:o/r#5", "github:issuecomment:2")
	if err != nil || len(w.Messages) != 3 || len(w.Stubs) != 0 {
		t.Fatalf("window = %+v, %v", w, err)
	}
	for _, m := range w.Messages {
		if m.Chars != 0 {
			t.Errorf("%s: Chars = %d for an uncut text", m.ID, m.Chars)
		}
	}
	if w.Messages[0].Text != "T" || len([]rune(w.Messages[1].Text)) != 400 {
		t.Fatalf("messages = %+v", w.Messages)
	}
}

// The pull request fixtures: the body, an issue comment, two review comments (a reply among them), a review with an
// empty body (not a message) and a pending one (not submitted), plus a review with a body added here.
func TestContextPullMergesReviewCommentsAndReviews(t *testing.T) {
	var issues, comments, rcs []map[string]any
	var reviews map[string][]map[string]any
	loadFixture(t, "issues.json", &issues)
	loadFixture(t, "issue_comments.json", &comments)
	loadFixture(t, "review_comments.json", &rcs)
	loadFixture(t, "reviews.json", &reviews)
	var pull map[string]any
	var onPull []map[string]any
	for _, it := range issues {
		if it["number"] == float64(2) {
			pull = it
		}
	}
	for _, c := range comments {
		if strings.HasSuffix(c["issue_url"].(string), "/issues/2") {
			onPull = append(onPull, c)
		}
	}
	rv := append(reviews["2"], map[string]any{"id": 303, "user": person("dave", 14), "state": "COMMENTED",
		"body": "Looks good overall.", "submitted_at": "2026-11-03T11:30:00Z"})

	f := newChFake(t)
	f.serveThread(2, pull, onPull, rcs, rv)
	for _, trig := range []string{"github:reviewcomment:202", "github:review:303", "github:issue:9002"} {
		w, err := f.source().Context(t.Context(), "github:o/r#2", trig)
		if err != nil {
			t.Fatalf("%s: %v", trig, err)
		}
		var got []string
		for _, m := range w.Messages {
			got = append(got, m.ID+"/"+m.From)
		}
		want := "github:issue:9002/athena-bot[bot] github:issuecomment:103/dependabot[bot] github:reviewcomment:201/carol github:reviewcomment:202/alice github:review:303/dave"
		if strings.Join(got, " ") != want || len(w.Stubs) != 0 {
			t.Fatalf("%s: messages\n%v\nwant\n%s\nstubs %v", trig, strings.Join(got, " "), want, w.Stubs)
		}
		if w.Messages[0].Text != "Fix the flaky login test\n\nCloses #1." || w.Messages[2].Text != "nit: rename this helper" {
			t.Fatalf("texts: %q %q", w.Messages[0].Text, w.Messages[2].Text)
		}
	}
}

func TestContextIncludesTheAppsOwnComments(t *testing.T) {
	var comments []map[string]any
	loadFixture(t, "issue_comments.json", &comments)
	var issues []map[string]any
	loadFixture(t, "issues.json", &issues)
	f := newChFake(t)
	f.serveThread(1, issues[0], comments[:2], nil, nil)
	w, err := f.source().Context(t.Context(), "github:o/r#1", "github:issuecomment:101")
	if err != nil || len(w.Messages) != 3 {
		t.Fatalf("window = %+v, %v", w, err)
	}
	if m := w.Messages[2]; m.ID != "github:issuecomment:102" || m.From != "athena-bot[bot]" {
		t.Fatalf("the App's comment = %+v", m)
	}
}

func TestContextTriggerNotInThread(t *testing.T) {
	// A trigger with no message (a check suite) or a deleted comment: the first five and the last five.
	f := newChFake(t)
	var comments []map[string]any
	for i := 1; i <= 20; i++ {
		comments = append(comments, map[string]any{"id": i, "user": person("bob", 12), "body": fmt.Sprint("c", i), "created_at": ts(i)})
	}
	f.serveThread(5, map[string]any{"id": 5000, "number": 5, "title": "T", "user": person("alice", 11), "created_at": ts(0)}, comments, nil, nil)
	w, err := f.source().Context(t.Context(), "github:o/r#5", "github:checksuite:9:failure")
	if err != nil || len(w.Messages) != 10 || len(w.Stubs) != 11 {
		t.Fatalf("window = %d messages, %d stubs, %v", len(w.Messages), len(w.Stubs), err)
	}
	if w.Messages[5].ID != "github:issuecomment:16" || w.Messages[9].ID != "github:issuecomment:20" {
		t.Fatalf("messages = %+v", w.Messages)
	}
}

func TestContextErrors(t *testing.T) {
	f := newChFake(t)
	s := f.source()
	for _, thread := range []string{"github:o/r@main", "github:x/y#1", "feishu:o/r#1", "github:o/r#0", "github:o/r#abc"} {
		if _, err := s.Context(t.Context(), thread, "github:issue:1"); err == nil {
			t.Errorf("Context(%q) succeeded", thread)
		}
	}
	// A deleted conversation reads as an error the caller can tell is gone.
	f.route("GET", "/repos/o/r/issues/7", reply(410, `{"message":"gone"}`))
	if _, err := s.Context(t.Context(), "github:o/r#7", "github:issue:1"); !isClass(err, ErrGone) {
		t.Fatalf("err = %v", err)
	}
	f.route("GET", "/repos/o/r/issues/8", reply(502, `{"message":"Bad Gateway"}`))
	if _, err := s.Context(t.Context(), "github:o/r#8", "github:issue:1"); !isClass(err, ErrTransient) {
		t.Fatalf("err = %v", err)
	}
	var _ sources.ContextWindow
}
