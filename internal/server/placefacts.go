package server

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// placeFact is what the dashboard's list of places needs to know about a folder a Ticket used: whether it has a .git
// entry (kind git), and the path it resolves to through symbolic links (to match a found repository).
type placeFact struct {
	git      bool
	resolved string
}

// placeFacts finds the placeFacts of the folders the hub's Repos reads, off the hub's loop: Repos must not block on
// I/O beyond the store, and a folder on a dead network mount or an unplugged volume would stall every dashboard.
// Repos and the Tickets' summaries read only what the last pass found (lookup); a folder it has not seen is guessed a
// plain folder resolving to itself and wakes a pass at once, whose finding reaches the dashboards through the change
// callback when it differs. A periodic pass probes every folder read since the one before again. A folder nobody read
// meanwhile keeps its fact: forgetting it would turn the next read (a dashboard's Snapshot after a quiet spell) back
// into the guess, and the hub, whose last published list still holds the fact, would see no change to publish. The
// facts are a few bytes per folder a Ticket ever used.
type placeFacts struct {
	// probe finds one folder's facts (the filesystem by default; tests replace it).
	probe func(dir string) placeFact
	wake  chan struct{}

	mu    sync.Mutex
	known map[string]placeFact
	asked map[string]bool // the folders read since the last periodic pass
}

func newPlaceFacts() *placeFacts {
	return &placeFacts{probe: probePlace, wake: make(chan struct{}, 1), known: map[string]placeFact{},
		asked: map[string]bool{}}
}

// guessPlace is a folder's fact until a pass has probed it.
func guessPlace(dir string) placeFact { return placeFact{resolved: filepath.Clean(dir)} }

// probePlace reads the folder's facts from the filesystem.
func probePlace(dir string) placeFact {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return placeFact{git: err == nil, resolved: resolvePath(dir)}
}

// lookup returns dir's facts from the last pass, never touching the filesystem; before one, the guess, and a pass is
// woken. Without a placeFacts (the dashboard off, or a test) it is always the guess.
func (f *placeFacts) lookup(dir string) placeFact {
	if f == nil {
		return guessPlace(dir)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked[dir] = true
	if fact, ok := f.known[dir]; ok {
		return fact
	}
	select {
	case f.wake <- struct{}{}:
	default:
	}
	return guessPlace(dir)
}

// refresh probes the folders read and not known yet, or, when periodic, every folder read since the last periodic
// pass. It returns the folders whose fact differs from what lookup returned for them before, sorted.
func (f *placeFacts) refresh(periodic bool) []string {
	f.mu.Lock()
	var dirs []string
	for dir := range f.asked {
		if _, ok := f.known[dir]; periodic || !ok {
			dirs = append(dirs, dir)
		}
	}
	if periodic {
		f.asked = map[string]bool{}
	}
	f.mu.Unlock()
	slices.Sort(dirs)

	found := make(map[string]placeFact, len(dirs))
	for _, dir := range dirs {
		found[dir] = f.probe(dir)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var changed []string
	for _, dir := range dirs {
		fact := found[dir]
		before, ok := f.known[dir]
		if !ok {
			before = guessPlace(dir)
		}
		if before != fact {
			changed = append(changed, dir)
		}
		f.known[dir] = fact
	}
	return changed
}

// run passes over the folders when lookup wakes it and every every, until ctx is done, calling changed with the
// folders after a pass that found something new. Each pass runs on a goroutine of its own, so one stuck in a
// filesystem call never holds up the shutdown: once ctx is done run returns, and that pass's result, whenever it
// comes, is still kept.
func (f *placeFacts) run(ctx context.Context, every time.Duration, changed func(dirs []string)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		periodic := false
		select {
		case <-ctx.Done():
			return
		case <-f.wake:
		case <-tick.C:
			periodic = true
		}
		res := make(chan []string, 1)
		go func() { res <- f.refresh(periodic) }()
		select {
		case <-ctx.Done():
			return
		case c := <-res:
			if len(c) > 0 && changed != nil && ctx.Err() == nil {
				changed(c)
			}
		}
	}
}
