// Package github is the GitHub side of Athena: the App's authentication (a JWT, installation tokens and tokens scoped
// to one repository) and a small REST client over net/http with ETags, pagination, rate-limit tracking and an error
// taxonomy the polling Source and the Channel act on. There is no go-github and no JWT library.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielpina1/athena/internal/redact"
)

// DefaultAPIURL is GitHub's REST endpoint.
const DefaultAPIURL = "https://api.github.com"

const (
	apiVersion = "2022-11-28"
	// maxErrorRunes bounds the response body an error carries.
	maxErrorRunes = 300
	// maxBody bounds a response body read into memory.
	maxBody = 64 << 20
	// maxPages bounds a pagination loop.
	maxPages = 10000
	// defaultRateWait is how long a rate limit with no reset information is assumed to last.
	defaultRateWait = time.Minute
)

// The error classes. An error returned by Client matches exactly one of them with errors.Is, or is an ErrRateLimited,
// or a context error when the caller's context ended.
var (
	// ErrTransient is a failure worth retrying: 5xx, 408, a network error or timeout, and a 404 on a read (a repository
	// the App lost access to reads as a 404, so a 404 is retried rather than dropped).
	ErrTransient = errors.New("github: transient error")
	// ErrGone is a target that no longer accepts the request: 410, a 404 on a write, or a locked target (a 422, or a
	// 403 to a non-collaborator, whose message or errors[] say so; the APIError has Locked set).
	ErrGone = errors.New("github: gone")
	// ErrPermanent is any other failure the same request will not get past.
	ErrPermanent = errors.New("github: permanent error")
)

// ErrRateLimited is a 403 or 429 caused by a rate limit: the remaining count was 0 or a Retry-After came back. Reset is
// when to try again. Test for it with errors.As (target ErrRateLimited) or errors.Is(err, ErrRateLimited{}).
type ErrRateLimited struct{ Reset time.Time }

func (e ErrRateLimited) Error() string {
	return "github: rate limited until " + e.Reset.UTC().Format(time.RFC3339)
}

// Is makes errors.Is(err, ErrRateLimited{}) match any rate limit, whatever its Reset.
func (e ErrRateLimited) Is(target error) bool { _, ok := target.(ErrRateLimited); return ok }

// APIError is a failed response. Its message is the response body truncated to 300 runes and redacted; it never holds
// the token or the Authorization header. It unwraps to its class (ErrTransient, ErrGone, ErrPermanent) or, for a rate
// limit, to the ErrRateLimited.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
	// Locked is set when the target is locked (a 403 or 422 that says so): nothing can be posted to it.
	Locked bool
	class  error
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		s += ": " + e.Message
	}
	return redact.String(s)
}

func (e *APIError) Unwrap() error { return e.class }

// Client is a GitHub REST client for one kind of token: it holds that kind's ETags and rate-limit numbers. It is safe
// for concurrent use.
type Client struct {
	hc    *http.Client
	base  *url.URL
	token func(ctx context.Context) (string, error)
	ua    string
	now   func() time.Time
	// onUnauthorized, when set, is told the token a request used when GitHub answered 401; the request is then
	// retried once if the token func now returns a different token.
	onUnauthorized func(token string)

	mu    sync.Mutex
	etags map[string]cached

	remaining atomic.Int64 // -1 until a response says
	reset     atomic.Int64 // unix seconds, 0 until a response says
}

type cached struct {
	etag string
	body []byte
}

// NewClient returns a client for apiURL (DefaultAPIURL when empty) that authenticates every request with the token
// token returns. version is the Athena version, sent in the User-Agent. hc defaults to a client with a 30 s timeout.
// It refuses an apiURL it cannot parse, that is not http(s) with a host, that carries credentials, or that is plain
// http to a host other than loopback (127.0.0.1, ::1, localhost).
func NewClient(hc *http.Client, apiURL string, token func(ctx context.Context) (string, error), version string) (*Client, error) {
	return newClient(hc, apiURL, token, version, time.Now)
}

func newClient(hc *http.Client, apiURL string, token func(ctx context.Context) (string, error), version string, now func() time.Time) (*Client, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	if version == "" {
		version = "dev"
	}
	base, err := parseAPIURL(apiURL)
	if err != nil {
		return nil, err
	}
	c := &Client{hc: hc, base: base, token: token, ua: "athena/" + version, now: now, etags: map[string]cached{}}
	c.remaining.Store(-1)
	return c, nil
}

// parseAPIURL checks an API URL: https, or http only to loopback, with a host and no credentials.
func parseAPIURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("github: API URL is not a valid URL: %s", redact.String(err.Error()))
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("github: API URL needs a scheme and a host")
	}
	if u.User != nil {
		return nil, errors.New("github: API URL must not carry credentials")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); !strings.EqualFold(h, "localhost") && h != "127.0.0.1" && h != "::1" {
			return nil, fmt.Errorf("github: API URL %s is plain http; only https (or http to loopback) is allowed", u.Host)
		}
	default:
		return nil, fmt.Errorf("github: API URL scheme %q is not https", u.Scheme)
	}
	return u, nil
}

// resolve turns a request path into a full URL on the API host. The path must start with "/".
func (c *Client) resolve(path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("github: refusing request path %q: it must start with /: %w", path, ErrPermanent)
	}
	return c.base.String() + path, nil
}

// sameOrigin reports whether u is on the API's scheme and host (and carries no credentials).
func (c *Client) sameOrigin(u *url.URL) bool {
	return u.User == nil && u.Scheme == c.base.Scheme && strings.EqualFold(u.Host, c.base.Host)
}

// RateRemaining is the x-ratelimit-remaining of the latest response that had one, or -1 before any.
func (c *Client) RateRemaining() int { return int(c.remaining.Load()) }

// RateReset is the x-ratelimit-reset of the latest response that had one, or the zero time before any.
func (c *Client) RateReset() time.Time {
	if r := c.reset.Load(); r != 0 {
		return time.Unix(r, 0).UTC()
	}
	return time.Time{}
}

// Get fetches path (relative to the API URL, with its query) and decodes the JSON body into out (which may be nil).
// It sends If-None-Match when it holds an ETag for the path: on a 304 it returns notModified = true and leaves out
// untouched; Cached then gives the last body.
func (c *Client) Get(ctx context.Context, path string, out any) (notModified bool, err error) {
	c.mu.Lock()
	prev, had := c.etags[path]
	c.mu.Unlock()
	var hdr http.Header
	if had {
		hdr = http.Header{"If-None-Match": {prev.etag}}
	}
	target, err := c.resolve(path)
	if err != nil {
		return false, err
	}
	resp, err := c.do(ctx, http.MethodGet, target, path, nil, hdr)
	if err != nil {
		return false, err
	}
	if resp.status == http.StatusNotModified {
		if !had {
			return false, c.fail(http.MethodGet, path, resp)
		}
		return true, nil
	}
	if out != nil {
		if err := json.Unmarshal(resp.body, out); err != nil {
			return false, fmt.Errorf("github: GET %s: decode: %w", path, err)
		}
	}
	c.mu.Lock()
	if etag := resp.header.Get("ETag"); etag != "" {
		c.etags[path] = cached{etag: etag, body: resp.body}
	} else {
		delete(c.etags, path)
	}
	c.mu.Unlock()
	return false, nil
}

// Cached decodes the body of the last successful Get of path into out and reports whether there was one.
func (c *Client) Cached(path string, out any) bool {
	c.mu.Lock()
	e, ok := c.etags[path]
	c.mu.Unlock()
	return ok && json.Unmarshal(e.body, out) == nil
}

// Post sends in as JSON to path and decodes the response into out (which may be nil).
func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	return c.write(ctx, http.MethodPost, path, in, out)
}

// Patch sends in as JSON to path and decodes the response into out (which may be nil).
func (c *Client) Patch(ctx context.Context, path string, in, out any) error {
	return c.write(ctx, http.MethodPatch, path, in, out)
}

func (c *Client) write(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("github: %s %s: encode: %w", method, path, err)
		}
	}
	target, err := c.resolve(path)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, method, target, path, body, nil)
	if err != nil {
		return err
	}
	if out != nil && len(resp.body) > 0 {
		if err := json.Unmarshal(resp.body, out); err != nil {
			return fmt.Errorf("github: %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

var nextLink = regexp.MustCompile(`<([^>]*)>\s*;[^,]*\brel="?next"?`)

// Paginate GETs path and every page its Link header names as next, calling fn with each page's raw body. It stops at
// the first error fn returns. A next link to another host is refused so the token never leaves the API.
func (c *Client) Paginate(ctx context.Context, path string, fn func(json.RawMessage) error) error {
	target, err := c.resolve(path)
	if err != nil {
		return err
	}
	for page := 0; page < maxPages; page++ {
		resp, err := c.do(ctx, http.MethodGet, target, path, nil, nil)
		if err != nil {
			return err
		}
		if err := fn(resp.body); err != nil {
			return err
		}
		m := nextLink.FindStringSubmatch(resp.header.Get("Link"))
		if m == nil {
			return nil
		}
		next, err := url.Parse(m[1])
		if err != nil || !c.sameOrigin(next) {
			return &APIError{Method: http.MethodGet, Path: path, Status: resp.status, Message: "next page is not on the API host", class: ErrPermanent}
		}
		target = next.String()
	}
	return &APIError{Method: http.MethodGet, Path: path, Status: http.StatusOK, Message: "too many pages", class: ErrPermanent}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends one request. It returns the response for a 2xx and for a 304; any other status is an error. label is the
// path shown in errors (the full URL of a next page can carry a query only).
func (c *Client) do(ctx context.Context, method, target, label string, body []byte, extra http.Header) (*response, error) {
	if u, err := url.Parse(target); err != nil || !c.sameOrigin(u) {
		return nil, fmt.Errorf("github: refusing %s %s: not on the API host: %w", method, label, ErrPermanent)
	}
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	r, err := c.send(ctx, method, target, label, body, extra, tok)
	var ae *APIError
	if c.onUnauthorized != nil && errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
		// The token was rejected: have its owner drop it, then retry once with whatever the token func returns now.
		c.onUnauthorized(tok)
		tok2, terr := c.token(ctx)
		if terr != nil {
			return nil, terr
		}
		if tok2 != tok {
			return c.send(ctx, method, target, label, body, extra, tok2)
		}
	}
	return r, err
}

// send makes one attempt with tok.
func (c *Client) send(ctx context.Context, method, target, label string, body []byte, extra http.Header, tok string) (*response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %s", method, label, redact.String(err.Error()))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header[k] = v
	}
	res, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("github: %s %s: %w", method, label, ctx.Err())
		}
		// The transport error text names the URL and the cause, never the headers; redact it all the same.
		return nil, &APIError{Method: method, Path: label, Message: redact.String(transportMessage(err)), class: ErrTransient}
	}
	defer res.Body.Close()
	limit := int64(maxBody)
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNotModified {
		limit = 64 << 10
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit))
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("github: %s %s: %w", method, label, ctx.Err())
		}
		return nil, &APIError{Method: method, Path: label, Status: res.StatusCode, Message: "reading the response: " + redact.String(err.Error()), class: ErrTransient}
	}
	r := &response{status: res.StatusCode, header: res.Header, body: data}
	c.record(res.Header)
	if r.status >= 200 && r.status < 300 || r.status == http.StatusNotModified {
		return r, nil
	}
	return nil, c.fail(method, label, r)
}

// transportMessage is err's text without the *url.Error wrapper's URL when it is one (the URL is in the path anyway).
func transportMessage(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// record keeps the rate-limit numbers of a response.
func (c *Client) record(h http.Header) {
	if v, err := strconv.ParseInt(h.Get("x-ratelimit-remaining"), 10, 64); err == nil {
		c.remaining.Store(v)
	}
	if v, err := strconv.ParseInt(h.Get("x-ratelimit-reset"), 10, 64); err == nil {
		c.reset.Store(v)
	}
}

// fail classifies a failed response.
func (c *Client) fail(method, path string, r *response) error {
	msg := apiMessage(r.body)
	e := &APIError{Method: method, Path: path, Status: r.status, Message: truncate(redact.String(msg), maxErrorRunes)}
	locked := lockedBody(r.body, msg)
	switch s := r.status; {
	case s == http.StatusForbidden || s == http.StatusTooManyRequests:
		if reset, ok := c.rateLimitReset(r, msg); ok {
			return rateLimited{ErrRateLimited{Reset: reset}, e}
		}
		if s == http.StatusForbidden && locked {
			e.class, e.Locked = ErrGone, true
			break
		}
		e.class = ErrPermanent
	case s >= 500 || s == http.StatusRequestTimeout:
		e.class = ErrTransient
	case s == http.StatusNotFound && method == http.MethodGet:
		e.class = ErrTransient
	case s == http.StatusNotFound || s == http.StatusGone:
		e.class = ErrGone
	case s == http.StatusUnprocessableEntity && locked:
		e.class, e.Locked = ErrGone, true
	default:
		e.class = ErrPermanent
	}
	return e
}

// rateLimitReset reports whether a 403/429 is a rate limit and when it ends: Retry-After wins, then
// x-ratelimit-reset, else a minute from now. A 429, and a 403 whose message says "rate limit", count as limits even
// without the headers.
func (c *Client) rateLimitReset(r *response, msg string) (time.Time, bool) {
	now := c.now()
	if v, err := strconv.ParseInt(strings.TrimSpace(r.header.Get("Retry-After")), 10, 64); err == nil && v >= 0 {
		return now.Add(time.Duration(v) * time.Second), true
	}
	exhausted := strings.TrimSpace(r.header.Get("x-ratelimit-remaining")) == "0"
	if !exhausted && r.status != http.StatusTooManyRequests && !strings.Contains(strings.ToLower(msg), "rate limit") {
		return time.Time{}, false
	}
	if v, err := strconv.ParseInt(r.header.Get("x-ratelimit-reset"), 10, 64); err == nil && v > 0 {
		return time.Unix(v, 0).UTC(), true
	}
	return now.Add(defaultRateWait), true
}

// rateLimited is the error of a rate-limited response: an ErrRateLimited (errors.As finds it) that also carries the
// APIError (errors.As finds that too).
type rateLimited struct {
	ErrRateLimited
	api *APIError
}

func (e rateLimited) Unwrap() []error { return []error{e.ErrRateLimited, e.api} }
func (e rateLimited) Error() string   { return e.api.Error() + " (" + e.ErrRateLimited.Error() + ")" }

// apiMessage is the "message" of a GitHub error body, or the body itself when it is not that JSON.
func apiMessage(body []byte) string {
	var v struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &v) == nil && v.Message != "" {
		return v.Message
	}
	return strings.TrimSpace(string(body))
}

var lockedWord = regexp.MustCompile(`(?i)\blocked\b`)

// lockedBody reports whether an error body says the target is locked: in its top-level message (msg) or in any
// errors[] entry, whether that is a string or an object with a message.
func lockedBody(body []byte, msg string) bool {
	if lockedWord.MatchString(msg) {
		return true
	}
	var v struct {
		Errors []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	for _, raw := range v.Errors {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			var o struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(raw, &o) != nil {
				continue
			}
			text = o.Message
		}
		if lockedWord.MatchString(text) {
			return true
		}
	}
	return false
}

// truncate cuts s to n runes, adding an ellipsis when it cut.
func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

var _ slog.LogValuer = Token{}
