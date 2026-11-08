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

// NewWatcher returns a Watcher over c. appSlug is the App's own slug: its check suites do not count.
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

// ChangedFiles lists the files that differ between base and head. A comparison of more than GitHub's 300-file limit
// is judged on the files returned. A 404 (a base that a force-push removed) is schedule.ErrNoCompare.
func (w *Watcher) ChangedFiles(ctx context.Context, repo, base, head string) ([]string, error) {
	var files []string
	path := repoPath(repo) + "/compare/" + url.PathEscape(base) + "..." + url.PathEscape(head) + "?per_page=100"
	err := w.c.Paginate(ctx, path, func(raw json.RawMessage) error {
		var page struct {
			Files []struct {
				Filename string `json:"filename"`
			} `json:"files"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("github: GET %s: decode: %w", path, err)
		}
		for _, f := range page.Files {
			files = append(files, f.Filename)
		}
		return nil
	})
	var api *APIError
	if errors.As(err, &api) && api.Status == 404 {
		return nil, schedule.ErrNoCompare
	}
	return files, err
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
