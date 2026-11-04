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
	// localScanBudget bounds the time one scan spends; a scan that runs out is cut short.
	localScanBudget = 5 * time.Second
	// localScanEvery is the time between two scans.
	localScanEvery = 3 * time.Minute
	// localScanDepth is how far below a root a repository is looked for: its children and grandchildren.
	localScanDepth = 2
	// readDirBatch is how many entries of a folder are read at a time, the budget checked between batches.
	readDirBatch = 256
)

// localRepos finds the git repositories under the configured roots, in the background, for the dashboard's list of
// places a task can start in. Reads never wait for a scan: they get the last result.
type localRepos struct {
	roots []string
	skip  []string
	every time.Duration
	log   *slog.Logger
	// scan is one scan (scanLocalRepos over the roots by default; tests replace it).
	scan func(ctx context.Context) scanResult

	mu       sync.Mutex
	found    []string
	complete bool            // found is from a scan that was not cut short
	noted    map[string]bool // the limits already logged
}

// scanResult is what one scan found, sorted, and whether the time budget or ctx cut it short (cut) or it stopped at
// the cap (capped).
type scanResult struct {
	found       []string
	cut, capped bool
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
	l := &localRepos{roots: slices.Clone(roots), skip: slices.Clone(skip), every: localScanEvery, log: log,
		noted: map[string]bool{}}
	l.scan = func(ctx context.Context) scanResult {
		return scanLocalRepos(ctx, l.roots, l.skip, maxLocalRepos, localScanBudget)
	}
	return l
}

// list returns the repositories the last kept scan found, sorted; nil before the first scan or without a finder.
func (l *localRepos) list() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.found)
}

// apply keeps a scan's result and reports whether the list changed. A scan cut short never replaces a complete list
// (it would shrink it, or make it flap); it is kept only while there is none yet. The cut and the cap are each logged
// once.
func (l *localRepos) apply(r scanResult) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	note := func(key, msg string, args ...any) {
		if !l.noted[key] {
			l.noted[key] = true
			l.log.Info(msg, args...)
		}
	}
	if r.capped {
		note("cap", "local repositories: the list stops at its cap, at most max; narrow localRepoRoots to choose which",
			"max", maxLocalRepos)
	}
	if r.cut {
		note("cut", "local repositories: a scan ran out of its time budget; the last complete list is kept",
			"budget", localScanBudget)
		if l.complete {
			return false
		}
	} else {
		l.complete = true
	}
	if slices.Equal(l.found, r.found) {
		return false
	}
	l.found = r.found
	return true
}

// run scans now and every l.every until ctx is done, calling changed after a scan whose kept result differs. Each scan
// runs on a goroutine of its own, so a scan stuck in a filesystem call (a dead network mount) never holds up the
// shutdown: once ctx is done run returns, and that scan's result, whenever it comes, is dropped.
func (l *localRepos) run(ctx context.Context, changed func()) {
	tick := time.NewTicker(l.every)
	defer tick.Stop()
	for {
		res := make(chan scanResult, 1)
		go func() { res <- l.scan(ctx) }()
		select {
		case <-ctx.Done():
			return
		case r := <-res:
			if ctx.Err() == nil && l.apply(r) && changed != nil {
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
// and its grandchildren, sorted, at most limit of them, in at most budget. A root that is a symbolic link is resolved
// once, and the skips with it, so the paths found are the resolved ones. It does not descend into a repository, a
// hidden folder, node_modules, vendor or a skipped path, and never follows a symbolic link below a root. An unreadable
// folder, or a missing root, is passed over.
func scanLocalRepos(ctx context.Context, roots, skip []string, limit int, budget time.Duration) scanResult {
	deadline := time.Now().Add(budget)
	skipped := map[string]bool{}
	for _, s := range skip {
		if s != "" {
			skipped[resolvePath(s)] = true
		}
	}
	seen := map[string]bool{}
	var r scanResult
	stop := func() bool {
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			r.cut = true
		}
		if len(r.found) >= limit {
			r.capped = true
		}
		return r.cut || r.capped
	}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if stop() || skipped[dir] {
			return
		}
		if isGitRepo(dir) {
			// Overlapping roots can reach a repository twice.
			if !seen[dir] {
				seen[dir] = true
				r.found = append(r.found, dir)
			}
			return
		}
		if depth >= localScanDepth {
			return
		}
		f, err := os.Open(dir)
		if err != nil {
			return
		}
		defer f.Close()
		for {
			entries, err := f.ReadDir(readDirBatch)
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
			// io.EOF ends the folder; another error passes over the rest of it.
			if err != nil || stop() {
				return
			}
		}
	}
	for _, root := range roots {
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		walk(real, 0)
	}
	slices.Sort(r.found)
	return r
}

// resolvePath is p with its symbolic links resolved, or p cleaned when it cannot be (it does not exist).
func resolvePath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// isGitRepo reports whether dir has a .git directory, or a .git file (a worktree or a submodule).
func isGitRepo(dir string) bool {
	st, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && (st.IsDir() || st.Mode().IsRegular())
}
