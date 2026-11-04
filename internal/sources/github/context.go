package github

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/sources"
)

// The thread-context reader: an assessment sees a Thread as a window around its Trigger (reference 007 "Result shapes
// and assessment context", 008 "Thread context for assessments").

const (
	// firstMessages are the messages from the start of the Thread that are always in the window.
	firstMessages = 5
	// aroundTrigger is how many messages on either side of the Trigger are in the window.
	aroundTrigger = 5
	// nearTrigger is how many of those, on either side, are shown at nearLimit.
	nearTrigger = 2
	// textLimit and nearLimit cut a message's text, in runes.
	textLimit = 400
	nearLimit = 1200
	// previewLimit cuts a stub's preview, in runes.
	previewLimit = 50
)

// message is one message of a Thread before it is windowed.
type message struct {
	id, from, text string
	at             time.Time
}

// Context reads thread, an issue or pull request of a configured repository, and returns the window around the
// Trigger: the Thread's messages (the issue or pull request body first, then issue comments and, for a pull request,
// review comments and reviews with a body, by time) with the first five and five on either side of the Trigger in
// full (cut to 400 runes, 1,200 for the two on either side, with Chars set to the original length), and the others as
// stubs. The App's own comments are messages like any other. A Trigger that is not a message of the Thread (a check
// suite, a deleted comment) gets the first five and the last five. The Thread's own errors (ErrGone, ErrTransient)
// come back as they are.
func (s *Source) Context(ctx context.Context, thread, triggerID string) (sources.ContextWindow, error) {
	if !s.Listens(thread) {
		return sources.ContextWindow{}, fmt.Errorf("github: context: %q is not a Thread of a configured repository", thread)
	}
	key := strings.TrimPrefix(thread, SourceName+":")
	i := strings.LastIndexByte(key, '#')
	n, err := strconv.Atoi(key[i+1:])
	if i < 0 || err != nil || n <= 0 {
		return sources.ContextWindow{}, fmt.Errorf("github: context: %q is not an issue or pull request", thread)
	}
	base := fmt.Sprintf("/repos/%s", strings.ToLower(key[:i]))
	c := s.client()
	if c == nil {
		return sources.ContextWindow{}, fmt.Errorf("github: no client")
	}
	var it ghIssue
	if err := c.Paginate(ctx, fmt.Sprintf("%s/issues/%d", base, n), func(page json.RawMessage) error {
		return json.Unmarshal(page, &it)
	}); err != nil {
		return sources.ContextWindow{}, err
	}
	pull := it.PullRequest != nil
	list := func(path string, each func(json.RawMessage) error) error {
		return c.Paginate(ctx, path+"?per_page="+strconv.Itoa(pageSize), func(page json.RawMessage) error {
			var items []json.RawMessage
			if err := json.Unmarshal(page, &items); err != nil {
				return fmt.Errorf("github: GET %s: %w", path, err)
			}
			for _, raw := range items {
				if err := each(raw); err != nil {
					return err
				}
			}
			return nil
		})
	}

	text := it.Title
	if it.Body != "" {
		text += "\n\n" + it.Body
	}
	first := message{id: "github:issue:" + strconv.FormatInt(it.ID, 10), from: user(it.User).Login, text: text, at: it.CreatedAt}
	var rest []message
	comment := func(kind string) func(json.RawMessage) error {
		return func(raw json.RawMessage) error {
			var cm ghComment
			if json.Unmarshal(raw, &cm) != nil || cm.ID == 0 {
				return nil // a malformed item is not a message
			}
			rest = append(rest, message{id: "github:" + kind + ":" + strconv.FormatInt(cm.ID, 10), from: user(cm.User).Login,
				text: cm.Body, at: cm.CreatedAt})
			return nil
		}
	}
	if err := list(fmt.Sprintf("%s/issues/%d/comments", base, n), comment("issuecomment")); err != nil {
		return sources.ContextWindow{}, err
	}
	if pull {
		if err := list(fmt.Sprintf("%s/pulls/%d/comments", base, n), comment("reviewcomment")); err != nil {
			return sources.ContextWindow{}, err
		}
		err := list(fmt.Sprintf("%s/pulls/%d/reviews", base, n), func(raw json.RawMessage) error {
			var rv ghReview
			// A pending review is not submitted, and a review without a body says nothing a message would.
			if json.Unmarshal(raw, &rv) != nil || rv.ID == 0 || rv.SubmittedAt == nil || strings.TrimSpace(rv.Body) == "" {
				return nil
			}
			rest = append(rest, message{id: "github:review:" + strconv.FormatInt(rv.ID, 10), from: user(rv.User).Login,
				text: rv.Body, at: *rv.SubmittedAt})
			return nil
		})
		if err != nil {
			return sources.ContextWindow{}, err
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].at.Before(rest[j].at) })
	all := append([]message{first}, rest...)
	return window(all, triggerID), nil
}

// window picks the messages shown in full and turns the others into stubs.
func window(all []message, triggerID string) sources.ContextWindow {
	limit := make([]int, len(all)) // 0: a stub
	show := func(i, l int) {
		if i >= 0 && i < len(all) && l > limit[i] {
			limit[i] = l
		}
	}
	for i := 0; i < firstMessages; i++ {
		show(i, textLimit)
	}
	t := -1
	for i, m := range all {
		if m.id == triggerID {
			t = i
			break
		}
	}
	if t >= 0 {
		for d := -aroundTrigger; d <= aroundTrigger; d++ {
			l := textLimit
			if d != 0 && d >= -nearTrigger && d <= nearTrigger {
				l = nearLimit
			}
			show(t+d, l)
		}
	} else {
		for i := len(all) - firstMessages; i < len(all); i++ {
			show(i, textLimit)
		}
	}
	var w sources.ContextWindow
	for i, m := range all {
		at := m.at.UTC().Format(time.RFC3339)
		if limit[i] == 0 {
			w.Stubs = append(w.Stubs, sources.Stub{ID: m.id, From: m.from, Preview: cut(strings.Join(strings.Fields(m.text), " "), previewLimit)})
			continue
		}
		msg := sources.Msg{ID: m.id, From: m.from, At: at, Text: m.text}
		if r := []rune(m.text); len(r) > limit[i] {
			msg.Text, msg.Chars = string(r[:limit[i]]), len(r)
		}
		w.Messages = append(w.Messages, msg)
	}
	return w
}

// cut shortens s to n runes.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
