//go:build live

package server

// TestLiveGitHubMention drives the whole GitHub path against a real sandbox repository, the real GitHub App and the
// real `claude` (haiku). It needs the owner to have created and installed the App (docs/dev/github-app.md) and costs
// a little money. It runs only with
//
//	ATHENA_LIVE_GITHUB=1 ATHENA_LIVE_REPO=owner/sandbox \
//	ATHENA_LIVE_APP_ID=... ATHENA_LIVE_APP_SLUG=... ATHENA_LIVE_APP_KEY=/path/github-app.pem ATHENA_LIVE_INSTALLATION=... \
//	go test -tags live -run LiveGitHub -v -timeout 25m ./internal/server
//
// The server runs in a fresh temp state dir whose config.json the test writes from those variables (the owner's own
// ~/.athena is never touched). The issue is created as the owner through the `gh` CLI, because the App's own comments
// never become Triggers; everything else goes through the App's installation token. Neither the key nor a token is
// ever printed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/sources/github"
)

// liveGHSkip is the message of the skip when the variables are unset.
// liveGHBudget is the time the whole test may wait; docs/dev/github-app.md documents go test -timeout 30m.
const liveGHBudget = 25 * time.Minute

const liveGHSkip = "set ATHENA_LIVE_GITHUB=1 and ATHENA_LIVE_REPO to run"

// liveTokenRe matches the token shapes Athena redacts: ghs_, ghu_, gho_, ghp_ and github_pat_.
var liveTokenRe = regexp.MustCompile(`(?:gh[suop]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)

// liveGH is a small GitHub REST client that acts as the App's installation. It never returns or logs the token.
type liveGH struct {
	t    *testing.T
	app  *github.App
	repo string
	hc   *http.Client
}

// do sends a request as the installation and decodes a JSON answer into out (nil to ignore it). It returns the status.
func (g *liveGH) do(method, path string, in, out any) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tok, err := g.app.InstallationToken(ctx)
	if err != nil {
		return 0, fmt.Errorf("installation token: %w", err)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Value)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "athena-live-test")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: transport error", method, path)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %.300s", method, path, resp.Status, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

type liveComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// botComments returns the comments of the issue or pull request n written by login.
func (g *liveGH) botComments(n int, login string) ([]liveComment, error) {
	var all []liveComment
	if _, err := g.do(http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", g.repo, n), nil, &all); err != nil {
		return nil, err
	}
	var out []liveComment
	for _, c := range all {
		if strings.EqualFold(c.User.Login, login) {
			out = append(out, c)
		}
	}
	return out, nil
}

// liveOwnerGH runs the owner's `gh` CLI (their own login, not the App's) and returns its trimmed output.
func liveOwnerGH(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = string(ee.Stderr)
		}
		t.Fatalf("gh %s: %v %s", strings.Join(args[:min(len(args), 2)], " "), err, msg)
	}
	return strings.TrimSpace(string(out))
}

func TestLiveGitHubMention(t *testing.T) {
	repo := os.Getenv("ATHENA_LIVE_REPO")
	if os.Getenv("ATHENA_LIVE_GITHUB") != "1" || repo == "" {
		t.Skip(liveGHSkip)
	}
	// The test was asked for: a missing tool is a failure, not a skip.
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("ATHENA_LIVE_GITHUB=1 is set but claude is not on PATH")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Fatal("ATHENA_LIVE_GITHUB=1 is set but gh is not on PATH: the issue is created as the owner")
	}
	// One deadline for every wait below, inside the documented go test -timeout 30m (the server's stop and the cleanup
	// follow it).
	deadline := time.Now().Add(liveGHBudget)
	left := func() time.Duration { return time.Until(deadline) }
	appID, err1 := strconv.ParseInt(os.Getenv("ATHENA_LIVE_APP_ID"), 10, 64)
	inst, err2 := strconv.ParseInt(os.Getenv("ATHENA_LIVE_INSTALLATION"), 10, 64)
	slug, key := os.Getenv("ATHENA_LIVE_APP_SLUG"), os.Getenv("ATHENA_LIVE_APP_KEY")
	if err1 != nil || err2 != nil || slug == "" || key == "" {
		t.Fatal("set ATHENA_LIVE_APP_ID, ATHENA_LIVE_APP_SLUG, ATHENA_LIVE_APP_KEY (a 0600 PEM file) and ATHENA_LIVE_INSTALLATION")
	}

	// The server's configuration: a fresh state dir configured the way `athena github setup` does it.
	stateDir := newStateDir(t)
	if err := SetupGitHub(stateDir, GitHubConfig{AppID: appID, Slug: slug, KeyFile: key, InstallationID: inst,
		Repos: []string{repo}, IntervalSeconds: 10}); err != nil {
		t.Fatal(err)
	}
	app, err := github.NewApp(github.AppConfig{AppID: appID, Slug: slug, KeyFile: key, InstallationID: inst, Version: "live-test"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	gh := &liveGH{t: t, app: app, repo: repo, hc: &http.Client{Timeout: time.Minute}}
	botLogin := slug + "[bot]"

	s := startLiveIn(t, claudeBin, stateDir)
	await(t, min(time.Minute, left()), "the github source is healthy and the router runs", func() bool {
		h := s.health()
		return h.Router.State == "running" && len(h.Sources) == 1 && h.Sources[0].Error == ""
	})

	// A deliberate Policy for this fresh state dir: the owner may start work, and mode auto accepts it.
	owner := liveOwnerGH(t, "api", "user", "--jq", ".login")
	pol, _ := json.Marshal(policy.Settings{Mode: policy.ModeAuto, Sources: map[string]policy.SourcePart{
		policy.GitHubSource: {Allowed: []string{owner}}}})
	var set PolicySetResult
	s.ok(Request{Op: OpPolicySet, Policy: pol}, &set)
	t.Logf("policy revision %d saved: mode auto, allowed %s", set.Revision, owner)

	// 1. The owner opens an issue that mentions the App.
	now := time.Now().UTC()
	file := fmt.Sprintf("hello-%d.txt", now.Unix()) // a new name each run, so a second run still produces a diff
	title := "athena live test " + now.Format("20060102-150405")
	body := "@" + slug + " create a file " + file + " containing hi and open a PR"
	url := liveOwnerGH(t, "issue", "create", "--repo", repo, "--title", title, "--body", body)
	number, err := strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
	if err != nil {
		t.Fatalf("issue url %q: %v", url, err)
	}
	t.Logf("issue %s", url)
	var branch string
	t.Cleanup(func() {
		// Close the issue, close the pull request if it is still open, and delete the Ticket's branch.
		if _, err := gh.do(http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, number), map[string]string{"state": "closed", "state_reason": "not_planned"}, nil); err != nil {
			t.Logf("cleanup: close issue: %v", err)
		}
		if branch == "" {
			return
		}
		var open []struct {
			Number int `json:"number"`
		}
		repoOwner := strings.Split(repo, "/")[0]
		if _, err := gh.do(http.MethodGet, fmt.Sprintf("/repos/%s/pulls?state=open&head=%s:%s", repo, repoOwner, branch), nil, &open); err == nil {
			for _, p := range open {
				if _, err := gh.do(http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, p.Number), map[string]string{"state": "closed"}, nil); err != nil {
					t.Logf("cleanup: close pull request #%d: %v", p.Number, err)
				}
			}
		}
		if code, err := gh.do(http.MethodDelete, fmt.Sprintf("/repos/%s/git/refs/heads/%s", repo, branch), nil, nil); err != nil && code != http.StatusUnprocessableEntity && code != http.StatusNotFound {
			t.Logf("cleanup: delete branch %s: %v", branch, err)
		}
	})

	// 2. Within 10 minutes: an acknowledgement by the App's bot, a Ticket, and a pull request from its branch that
	// touches hello.txt.
	thread := github.ThreadKey(repo, number)
	var ticket string
	await(t, left(), "a Ticket on "+thread, func() bool {
		for _, th := range s.threads() {
			if th.Key == thread && len(th.Tickets) > 0 {
				ticket, _, _ = strings.Cut(th.Tickets[0], "(")
				return true
			}
		}
		return false
	})
	s.dumpOnFailure(t, ticket)
	branch = "athena/" + strings.ToLower(ticket)
	t.Logf("ticket %s, branch %s", ticket, branch)

	var ack liveComment
	await(t, left(), "an acknowledgement by "+botLogin, func() bool {
		cs, err := gh.botComments(number, botLogin)
		if err != nil {
			t.Logf("comments: %v", err)
			return false
		}
		if len(cs) > 0 {
			ack = cs[0]
			return true
		}
		return false
	})
	t.Logf("acknowledgement comment %d", ack.ID)

	var pull int
	await(t, left(), "a pull request from "+branch+" touching "+file, func() bool {
		var prs []struct {
			Number int `json:"number"`
			Head   struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}
		if _, err := gh.do(http.MethodGet, fmt.Sprintf("/repos/%s/pulls?state=open&per_page=100", repo), nil, &prs); err != nil {
			t.Logf("pulls: %v", err)
			return false
		}
		for _, p := range prs {
			if p.Head.Ref != branch {
				continue
			}
			var files []struct {
				Filename string `json:"filename"`
			}
			if _, err := gh.do(http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", repo, p.Number), nil, &files); err != nil {
				t.Logf("pull files: %v", err)
				return false
			}
			for _, f := range files {
				if f.Filename == file {
					pull = p.Number
					return true
				}
			}
		}
		return false
	})
	t.Logf("pull request #%d", pull)

	checkBotComments := func(when string) {
		t.Helper()
		var cs []liveComment
		for _, n := range []int{number, pull} {
			l, err := gh.botComments(n, botLogin)
			if err != nil {
				t.Fatal(err)
			}
			cs = append(cs, l...)
		}
		sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
		// The bot's comments cannot be told apart by purpose from their bodies, so the rule is the same on both sides
		// of the merge: the first one is the acknowledgement and never changes, and at most one more may exist, a result.
		if len(cs) == 0 || cs[0].ID != ack.ID || cs[0].Body != ack.Body || len(cs) > 2 {
			t.Fatalf("%d bot comments %s; want the acknowledgement, unchanged, plus at most one result", len(cs), when)
		}
		for _, c := range cs[1:] {
			t.Logf("%s: later comment %d by the bot (a result, not a second acknowledgement): %d bytes", when, c.ID, len(c.Body))
		}
	}
	checkBotComments("before the merge")

	// 3. The merge, through the API with the App's token.
	if _, err := gh.do(http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, pull), map[string]string{"merge_method": "squash"}, nil); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// 4. The Ticket ends merged.
	await(t, left(), "the Ticket to end merged", func() bool { return s.detail(ticket).Stage == "ended:merged" })

	// 5. No second acknowledgement: after a few more polls the same rule still holds.
	time.Sleep(35 * time.Second)
	checkBotComments("after the merge")

	// No token or key in athena logs, the transcripts or the server log.
	for _, l := range s.logLines(t, ticket) {
		if liveTokenRe.MatchString(l.Text) || strings.Contains(l.Text, "PRIVATE KEY") {
			t.Errorf("athena logs %s holds a token or key (%s line)", ticket, l.Kind)
		}
	}
	for _, dir := range []string{"transcripts", LogDir} {
		files, _ := filepath.Glob(filepath.Join(stateDir, dir, "*"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if liveTokenRe.Match(b) || bytes.Contains(b, []byte("PRIVATE KEY")) {
				t.Errorf("%s holds a token or key", f)
			}
		}
	}
}
