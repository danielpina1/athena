package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// maxLocalRepos bounds the local git repositories one scan finds; they have their own cap, apart from maxRepos,
	// so they never crowd out the used places or the configured GitHub repositories.
	maxLocalRepos = 200
	// localScanBudget bounds the time one scan spends; a scan that runs out keeps what it found.
	localScanBudget = 5 * time.Second
	// localScanEvery is the time between two scans.
	localScanEvery = 3 * time.Minute
	// localScanDepth is how far below a root a repository is looked for: its children and grandchildren.
	localScanDepth = 2
)

// localRepos finds the git repositories under the configured roots, in the background, for the dashboard's list of
// places a task can start in. Reads never wait for a scan: they get the last result.
type localRepos struct {
	roots  []string
	skip   map[string]bool
	every  time.Duration
	budget time.Duration
	log    *slog.Logger

	mu    sync.Mutex
	found []string
}

// newLocalRepos returns the finder for roots, never descending into skip (the state directory, the work root); nil
// when there is no root, which turns the listing off.
func newLocalRepos(roots, skip []string, log *slog.Logger) *localRepos {
	if len(roots) == 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	l := &localRepos{roots: slices.Clone(roots), skip: map[string]bool{}, every: localScanEvery, budget: localScanBudget,
		log: log}
	for _, s := range skip {
		if s != "" {
			l.skip[filepath.Clean(s)] = true
		}
	}
	return l
}

// list returns the repositories the last scan found, sorted; nil before the first scan or without a finder.
func (l *localRepos) list() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.found)
}

// refresh scans once and reports whether the result differs from the last one.
func (l *localRepos) refresh(ctx context.Context) bool {
	found := scanLocalRepos(ctx, l.roots, l.skip, maxLocalRepos, l.budget)
	l.mu.Lock()
	defer l.mu.Unlock()
	if slices.Equal(l.found, found) {
		return false
	}
	l.found = found
	return true
}

// run scans now and every l.every until ctx is done, calling changed after a scan whose result differs.
func (l *localRepos) run(ctx context.Context, changed func()) {
	tick := time.NewTicker(l.every)
	defer tick.Stop()
	for {
		start := time.Now()
		if l.refresh(ctx) {
			l.log.Info("local repositories found", "count", len(l.list()), "took", time.Since(start).Round(time.Millisecond))
			if changed != nil {
				changed()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// scanLocalRepos returns the git repositories (a folder with a .git directory or file) among each root, its children
// and its grandchildren, sorted, at most limit of them, in at most budget. It does not descend into a repository, a
// hidden folder, node_modules, vendor or a skipped path, and never follows a symbolic link below a root. An
// unreadable folder is passed over.
func scanLocalRepos(ctx context.Context, roots []string, skip map[string]bool, limit int, budget time.Duration) []string {
	deadline := time.Now().Add(budget)
	seen := map[string]bool{}
	var out []string
	stop := func() bool { return len(out) >= limit || ctx.Err() != nil || time.Now().After(deadline) }
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if stop() || skip[dir] {
			return
		}
		if isGitRepo(dir) {
			// Overlapping roots can reach a repository twice.
			if !seen[dir] {
				seen[dir] = true
				out = append(out, dir)
			}
			return
		}
		if depth >= localScanDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			// A symbolic link's entry is not a directory here: links are never followed.
			name := e.Name()
			if !e.IsDir() || strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
				continue
			}
			walk(filepath.Join(dir, name), depth+1)
			if stop() {
				return
			}
		}
	}
	for _, root := range roots {
		walk(filepath.Clean(root), 0)
	}
	slices.Sort(out)
	return out
}

// isGitRepo reports whether dir has a .git directory, or a .git file (a worktree or a submodule).
func isGitRepo(dir string) bool {
	st, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && (st.IsDir() || st.Mode().IsRegular())
}
