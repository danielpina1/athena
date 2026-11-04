package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/sources"
)

// The Channel: Send writes a reply to the issue or pull request a Trigger came from, and to the review thread when
// the Trigger was a review comment (reference 003 "Channel failure", 008 "Replies: the Channel"). A Channel address
// is {repo (full_name), number, comment_id?}; comment_id is the top comment of a review thread.

const (
	// dedupeWindow is how far back a retry looks for the message it may already have posted when it does not know
	// when the message was queued.
	dedupeWindow = time.Hour
	// maxBodyRunes keeps a comment under GitHub's 65,536-character limit, which would otherwise answer 422 forever.
	maxBodyRunes = 60000
)

// receiptMarker is the hidden line that makes a comment recognisable as the send of one receipt.
func receiptMarker(receiptID string) string { return "<!-- athena:receipt:" + receiptID + " -->" }

// target is a parsed Channel address.
type target struct {
	repo      string // as the address names it (GitHub's full_name)
	n         int
	commentID int64 // the top comment of a review thread, or 0
}

func (t target) path() string { return "/repos/" + strings.ToLower(t.repo) }

// parseTarget reads and checks a Channel address: the repository must be a configured one, the numbers positive, and
// the address must be the one of the Thread the Channel is for.
func (s *Source) parseTarget(ch core.ChannelRef) (target, error) {
	bad := func(why string) (target, error) {
		return target{}, fmt.Errorf("github: channel address %v: %s: %w", ch.Address, why, ErrPermanent)
	}
	if ch.Source != SourceName {
		return bad("not a github Channel")
	}
	repo := ch.Address["repo"]
	if !validRepo.MatchString(repo) || !s.repos[strings.ToLower(repo)] {
		return bad("repo is not a configured owner/name")
	}
	n, err := strconv.Atoi(ch.Address["number"])
	if err != nil || n <= 0 {
		return bad("number is not a positive integer")
	}
	t := target{repo: repo, n: n}
	if v := ch.Address["comment_id"]; v != "" {
		if t.commentID, err = strconv.ParseInt(v, 10, 64); err != nil || t.commentID <= 0 {
			return bad("comment_id is not a positive integer")
		}
	}
	if ThreadKey(repo, n) != ch.Thread {
		return bad("repo and number are not those of the Thread " + ch.Thread)
	}
	return t, nil
}

// Link is the page a person opens to see the conversation: the review thread's comment when the address has one;
// empty when the address is not valid.
func (s *Source) Link(ch core.ChannelRef) string {
	t, err := s.parseTarget(ch)
	if err != nil {
		return ""
	}
	base := "https://github.com/" + t.repo
	if t.commentID != 0 {
		return fmt.Sprintf("%s/pull/%d#discussion_r%d", base, t.n, t.commentID)
	}
	return fmt.Sprintf("%s/issues/%d", base, t.n)
}

// Send posts text, with the receipt's hidden marker, as a reply in the review thread (comment_id) or as a comment on
// the issue or pull request. A reply whose review comment is gone goes to the conversation instead. It answers Gone
// when the issue or pull request is deleted or locked, a refusal (sources.ErrRefused) for a permanent failure, and
// any other failure is an error to retry. On a retry (attempt
// above zero) the earlier send may have reached GitHub, so the comments of the conversation since the message was
// queued are read first and a comment of the App with the marker counts as sent.
func (s *Source) Send(ctx context.Context, ch core.ChannelRef, req sources.SendRequest) (sources.SendResult, error) {
	receiptID, text := req.ReceiptID, req.Text
	t, err := s.parseTarget(ch)
	if err != nil {
		return "", refusal(err)
	}
	c := s.client()
	if c == nil {
		return "", errors.New("github: no client")
	}
	if req.Attempt > 0 && receiptID != "" {
		found, err := s.alreadySent(ctx, t, receiptID, req.QueuedAt)
		if err != nil {
			if goneErr(err) {
				return sources.Gone, nil
			}
			return "", refusal(fmt.Errorf("github: check for an earlier send: %w", err))
		}
		if found {
			return sources.Sent, nil
		}
	}
	body := map[string]string{"body": commentBody(text, receiptID)}
	if t.commentID != 0 {
		err := c.Post(ctx, fmt.Sprintf("%s/pulls/%d/comments/%d/replies", t.path(), t.n, t.commentID), body, nil)
		if err == nil {
			return sources.Sent, nil
		}
		// Only a missing review comment falls back; a locked or deleted conversation ends the message.
		var ae *APIError
		if !errors.Is(err, ErrGone) || (errors.As(err, &ae) && (ae.Locked || ae.Status == http.StatusGone)) {
			return gone(err)
		}
	}
	err = c.Post(ctx, fmt.Sprintf("%s/issues/%d/comments", t.path(), t.n), body, nil)
	if err == nil {
		return sources.Sent, nil
	}
	return gone(err)
}

// gone is the outcome of a failed post: Gone for a target that can never receive it, else the error (a refusal when
// it is permanent).
func gone(err error) (sources.SendResult, error) {
	if goneErr(err) {
		return sources.Gone, nil
	}
	return "", refusal(err)
}

// refusal marks a permanent failure (ErrPermanent: a 403 such as a missing App permission, a 422, an invalid address)
// as sources.ErrRefused, which the Outbox drops instead of retrying; any other error is returned as it is.
func refusal(err error) error {
	if errors.Is(err, ErrPermanent) {
		return fmt.Errorf("%w: %w", err, sources.ErrRefused)
	}
	return err
}

func goneErr(err error) bool { return errors.Is(err, ErrGone) }

// commentBody is the text with the receipt's marker on its own last line, cut so that GitHub accepts it.
func commentBody(text, receiptID string) string {
	if r := []rune(text); len(r) > maxBodyRunes {
		text = string(r[:maxBodyRunes-1]) + "…"
	}
	if receiptID == "" {
		return text
	}
	return text + "\n\n" + receiptMarker(receiptID)
}

// alreadySent reports whether the App has posted the receipt's marker in the conversation since the message was queued
// (an hour ago when that is not known): in its issue comments, and, for a review reply (which may have fallen back to a plain comment), in the pull
// request's review comments too.
func (s *Source) alreadySent(ctx context.Context, t target, receiptID string, queuedAt time.Time) (bool, error) {
	login, id, err := s.identity(ctx)
	if err != nil {
		return false, err
	}
	from := queuedAt
	if from.IsZero() {
		from = s.now().Add(-dedupeWindow)
	}
	since := url.Values{"since": {from.UTC().Truncate(time.Second).Format(time.RFC3339)}, "per_page": {strconv.Itoa(pageSize)}}.Encode()
	paths := []string{fmt.Sprintf("%s/issues/%d/comments?%s", t.path(), t.n, since)}
	if t.commentID != 0 {
		paths = append([]string{fmt.Sprintf("%s/pulls/%d/comments?%s", t.path(), t.n, since)}, paths...)
	}
	marker := receiptMarker(receiptID)
	for _, path := range paths {
		found := false
		err := s.client().Paginate(ctx, path, func(page json.RawMessage) error {
			var list []ghComment
			if err := json.Unmarshal(page, &list); err != nil {
				return fmt.Errorf("github: GET %s: %w", path, err)
			}
			for _, c := range list {
				if u := user(c.User); strings.Contains(c.Body, marker) &&
					(strings.EqualFold(u.Login, login) || (id != 0 && u.ID == id)) {
					found = true
				}
			}
			return nil
		})
		var ae *APIError
		switch {
		case found:
			return true, nil
		case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
			// Not readable: the post that follows says whether the conversation is gone.
		case err != nil:
			return false, err
		}
	}
	return false, nil
}

// identity is the App's bot login and user id, read once. The polling goroutine keeps its own copy, so a send
// running beside a poll shares nothing with it.
func (s *Source) identity(ctx context.Context) (string, int64, error) {
	s.mu.Lock()
	login, id := s.chLogin, s.chID
	s.mu.Unlock()
	if login != "" {
		return login, id, nil
	}
	if s.app == nil {
		return "", 0, errors.New("github: no App")
	}
	login, id, err := s.app.Bot(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("github: the App's bot identity: %w", err)
	}
	s.mu.Lock()
	s.chLogin, s.chID = login, id
	s.mu.Unlock()
	return login, id, nil
}
