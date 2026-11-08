package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/danielpina1/athena/internal/sources/schedule"
)

// Watcher reads the repository facts that Autopilot's event entries wait for. It implements schedule.Repos.
type Watcher struct {
	c    *Client
	slug string
}

// webFlow is the account GitHub commits as when it makes a commit itself, as for the Merge button.
const webFlow = "web-flow"

// NewWatcher returns a Watcher over c. appSlug is the App's own slug: its check suites do not count, and its commits
// are Athena's own.
func NewWatcher(c *Client, appSlug string) *Watcher { return &Watcher{c: c, slug: appSlug} }

// repoPath is the API path of repo as the owner wrote it (GitHub compares names without case).
func repoPath(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// get GETs path into out; on a 304 it decodes the cached body.
func (w *Watcher) get(ctx context.Context, path string, out any) error {
	notModified, err := w.c.Get(ctx, path, out)
	if err != nil {
		return err
	}
	if notModified && !w.c.Cached(path, out) {
		return fmt.Errorf("github: GET %s: 304 without a cached body: %w", path, ErrTransient)
	}
	return nil
}

// Head returns the default branch of repo and the sha it points at.
func (w *Watcher) Head(ctx context.Context, repo string) (branch, sha string, err error) {
	p := repoPath(repo)
	var rec struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := w.get(ctx, p, &rec); err != nil {
		return "", "", err
	}
	if rec.DefaultBranch == "" {
		return "", "", fmt.Errorf("github: repository %s has no default branch: %w", repo, ErrPermanent)
	}
	var br struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := w.get(ctx, p+"/branches/"+url.PathEscape(rec.DefaultBranch), &br); err != nil {
		return "", "", err
	}
	if br.Commit.SHA == "" {
		return "", "", fmt.Errorf("github: branch %s of %s has no head commit: %w", rec.DefaultBranch, repo, ErrPermanent)
	}
	return rec.DefaultBranch, br.Commit.SHA, nil
}

// Compare reads what lies between base and head. A comparison of more than GitHub's 300-file limit is judged on the
// files returned. A 404 (a base that a force-push removed) is schedule.ErrNoCompare.
func (w *Watcher) Compare(ctx context.Context, repo, base, head string) (schedule.Comparison, error) {
	var (
		cmp     schedule.Comparison
		commits []ghCommit
		total   int
	)
	path := repoPath(repo) + "/compare/" + url.PathEscape(base) + "..." + url.PathEscape(head) + "?per_page=100"
	err := w.c.Paginate(ctx, path, func(raw json.RawMessage) error {
		var page struct {
			Total   int        `json:"total_commits"`
			Commits []ghCommit `json:"commits"`
			Files   []struct {
				Filename string `json:"filename"`
			} `json:"files"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("github: GET %s: decode: %w", path, err)
		}
		total = page.Total
		commits = append(commits, page.Commits...)
		for _, f := range page.Files {
			cmp.Files = append(cmp.Files, f.Filename)
		}
		return nil
	})
	var api *APIError
	if errors.As(err, &api) && api.Status == 404 {
		return schedule.Comparison{}, schedule.ErrNoCompare
	}
	cmp.OnlyAthena = len(commits) == total && w.onlyAthena(commits)
	return cmp, err
}

// ghCommit is a commit of a comparison. Author and committer are the GitHub accounts of its addresses, null for an
// address tied to no account.
type ghCommit struct {
	Author    *ghUser           `json:"author"`
	Committer *ghUser           `json:"committer"`
	Parents   []json.RawMessage `json:"parents"`
}

// onlyAthena reports whether the App's bot wrote these commits. Workers commit as the bot, and a squash or rebase of
// their pull request keeps it as the author. The one commit a person's name can be on is the merge commit of the Merge
// button: it is the merger's, committed by GitHub, so it passes, and whatever it brought in is judged here on its own.
// At least one commit must be the bot's, or a merge of nothing of Athena's would pass. A person who forges the bot's
// address only stops an entry firing.
func (w *Watcher) onlyAthena(commits []ghCommit) bool {
	if w.slug == "" {
		return false
	}
	bot, own := w.slug+"[bot]", false
	for _, c := range commits {
		switch {
		case c.Author != nil && strings.EqualFold(c.Author.Login, bot):
			own = true
		case len(c.Parents) > 1 && c.Committer != nil && c.Committer.Login == webFlow:
		default:
			return false
		}
	}
	return own
}

// CI summarizes the completed check suites of sha, without the App's own: Red when one concluded failure or timed_out,
// Unknown when there are none or some still run, Green otherwise. It reads the first 100 suites.
func (w *Watcher) CI(ctx context.Context, repo, sha string) (schedule.CIState, error) {
	var page struct {
		CheckSuites []ghSuite `json:"check_suites"`
	}
	if err := w.get(ctx, repoPath(repo)+"/commits/"+url.PathEscape(sha)+"/check-suites?per_page=100", &page); err != nil {
		return schedule.CIUnknown, err
	}
	counted, running := 0, false
	for _, cs := range page.CheckSuites {
		if cs.App != nil && w.slug != "" && strings.EqualFold(cs.App.Slug, w.slug) {
			continue
		}
		counted++
		switch {
		case cs.Status != "completed" || cs.Conclusion == nil || *cs.Conclusion == "":
			running = true
		case *cs.Conclusion == "failure" || *cs.Conclusion == "timed_out":
			return schedule.CIRed, nil
		}
	}
	if counted == 0 || running {
		return schedule.CIUnknown, nil
	}
	return schedule.CIGreen, nil
}
