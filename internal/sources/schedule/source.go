// Package schedule is the Source that fires the owner's Autopilot entries: a cron entry raises a Trigger at each
// local-time firing.
package schedule

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/cron"
)

// SourceName is the Source's name and the prefix of its Thread keys and Trigger IDs.
const SourceName = core.SourceSchedule

const (
	TickInterval  = 30 * time.Second
	EventInterval = 5 * time.Minute
	// MinGap is the least time between two firings of an entry.
	MinGap = 15 * time.Minute
	// CatchUpWindow is how late a missed clock firing may still run.
	CatchUpWindow = 24 * time.Hour
	// DailyCap is the most firings of an entry in one local day.
	DailyCap = 24

	// catchUpGrace is how late a firing may run before its text says it is a catch-up.
	catchUpGrace = 5 * time.Minute
)

// Entry is one enabled Autopilot entry.
type Entry struct {
	Slug, Name, Instructions, Repo string
	Cron, Event                    string
	CIRedAfter                     time.Duration
}

// Intake is what the Source needs of the core.
type Intake interface {
	Receive(t core.Trigger) (string, error)
	ThreadBusy(thread string) (bool, error)
}

// KV holds the entries' state documents.
type KV interface {
	KVGet(key string) (string, bool, error)
	KVSet(key, value string) error
	// KVList returns the entries whose key starts with prefix.
	KVList(prefix string) (map[string]string, error)
}

// Config configures the Source.
type Config struct {
	// Entries returns the enabled entries; it is read on every tick.
	Entries func() []Entry
	// Known tells whether the GitHub Source knows the repository.
	Known func(repo string) bool
	// Paused tells whether Athena is paused.
	Paused func() bool
	Now    func() time.Time
	Wait   func(ctx context.Context, d time.Duration) error
	// TickInterval defaults to the constant; tests shorten it.
	TickInterval time.Duration
	// Repos reads the facts repository events wait for; nil means no event entry fires.
	Repos Repos
	// EventInterval defaults to the constant; tests shorten it.
	EventInterval time.Duration
}

// ThreadKey is the Thread of an entry's runs.
func ThreadKey(slug string) string { return SourceName + ":" + slug }

// Source fires Autopilot entries. Run, Listens and Health are safe for concurrent use.
type Source struct {
	cfg Config
	in  Intake
	kv  KV
	log *slog.Logger

	// work serializes a tick and an event poll, which read and write the same state documents.
	work sync.Mutex

	mu       sync.Mutex
	unknown  map[string]string       // slug → repository the GitHub Source does not know
	eventBad map[string]eventFailure // lower-cased repository → its failing polls in a row
}

// New returns the Source; log defaults to a logger that discards.
func New(cfg Config, in Intake, kv KV, log *slog.Logger) *Source {
	if log == nil {
		log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = sleepCtx
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = TickInterval
	}
	if cfg.EventInterval <= 0 {
		cfg.EventInterval = EventInterval
	}
	return &Source{cfg: cfg, in: in, kv: kv, log: log, unknown: map[string]string{}, eventBad: map[string]eventFailure{}}
}

// Name is "schedule".
func (s *Source) Name() string { return SourceName }

// Listens reports whether thread is the Thread of a currently enabled entry.
func (s *Source) Listens(thread string) bool {
	if s.cfg.Entries == nil {
		return false
	}
	for _, e := range s.cfg.Entries() {
		if ThreadKey(e.Slug) == thread {
			return true
		}
	}
	return false
}

// Health is an error while an enabled entry names a repository the GitHub Source does not know.
func (s *Source) Health() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	slugs := make([]string, 0, len(s.unknown))
	for slug := range s.unknown {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		return fmt.Errorf("schedule: entry %s targets %s, which the github source does not know", slug, s.unknown[slug])
	}
	repos := make([]string, 0, len(s.eventBad))
	for repo := range s.eventBad {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		if f := s.eventBad[repo]; f.polls >= eventFailuresReported {
			return fmt.Errorf("schedule: repository events of %s failed %d polls in a row: %s", repo, f.polls, f.err)
		}
	}
	return nil
}

// Run ticks at once and then every TickInterval until ctx is done, and returns nil then. With Repos set, it polls the
// repository events on their own timer beside it.
func (s *Source) Run(ctx context.Context) error {
	if s.cfg.Repos != nil {
		done := make(chan struct{})
		defer func() { <-done }()
		var stop context.CancelFunc
		ctx, stop = context.WithCancel(ctx)
		defer stop()
		go func() {
			defer close(done)
			for {
				s.PollEvents(ctx)
				if err := s.cfg.Wait(ctx, s.cfg.EventInterval); err != nil || ctx.Err() != nil {
					return
				}
			}
		}()
	}
	for {
		s.Tick()
		if err := s.cfg.Wait(ctx, s.cfg.TickInterval); err != nil || ctx.Err() != nil {
			return nil
		}
	}
}

// Tick looks at every enabled entry once.
func (s *Source) Tick() {
	s.work.Lock()
	defer s.work.Unlock()
	var entries []Entry
	if s.cfg.Entries != nil {
		entries = s.cfg.Entries()
	}
	now := s.cfg.Now()
	unknown := map[string]string{}
	if err := s.forgetDisabled(entries); err != nil {
		s.log.Warn("schedule state not read", "error", err)
		return
	}
	for _, e := range entries {
		if e.Repo != "" && s.cfg.Known != nil && !s.cfg.Known(e.Repo) {
			unknown[e.Slug] = e.Repo
		}
		if e.Cron == "" {
			continue
		}
		if err := s.tickClock(e, now); err != nil {
			s.log.Warn("schedule entry not handled", "slug", e.Slug, "error", err)
		}
	}
	s.mu.Lock()
	s.unknown = unknown
	s.mu.Unlock()
}

// forgetDisabled clears the Spec of the state document of every entry that is not enabled (disabled or deleted), so
// that enabling it again starts over instead of firing for the time it was off. LastFired, Date and Fires stay: the
// daily cap survives.
func (s *Source) forgetDisabled(entries []Entry) error {
	docs, err := s.kv.KVList(stateKey(""))
	if err != nil {
		return err
	}
	enabled := map[string]bool{}
	for _, e := range entries {
		enabled[e.Slug] = true
	}
	for key := range docs {
		slug := strings.TrimPrefix(key, stateKey(""))
		if enabled[slug] {
			continue
		}
		st, err := s.load(slug)
		if err != nil {
			return err
		}
		if st == nil || st.Spec == "" {
			continue
		}
		st.Spec = ""
		if err := s.save(slug, st); err != nil {
			return err
		}
	}
	return nil
}

// tickClock fires the latest clock firing of e due at or before now, if any.
func (s *Source) tickClock(e Entry, now time.Time) error {
	st, err := s.load(e.Slug)
	if err != nil {
		return err
	}
	if st == nil || st.Spec != e.Cron {
		// A new or edited entry starts now: nothing for the past. The day's count and last firing stay.
		next := &state{}
		if st != nil {
			next = st
		}
		next.Spec, next.Cursor = e.Cron, now
		return s.save(e.Slug, next)
	}
	sched, err := cron.Parse(e.Cron)
	if err != nil {
		return err
	}
	from := st.Cursor.In(now.Location())
	if floor := now.Add(-CatchUpWindow); from.Before(floor) {
		from = floor
	}
	var due time.Time
	for t := from; ; {
		n := sched.Next(t)
		if n.IsZero() || n.After(now) {
			break
		}
		due, t = n, n
	}
	if due.IsZero() {
		return nil
	}
	late := now.Sub(due)
	why := "scheduled for " + due.Format("15:04") + "."
	if late > catchUpGrace {
		why = fmt.Sprintf("catch-up, due %s, %s late; the repository may have moved since.", due.Format("15:04"), lateText(late))
	}
	out, err := s.fire(e, st, firing{
		ID:  fmt.Sprintf("%s:%s:cron:%s", SourceName, e.Slug, due.Format("2006-01-02T15:04")),
		At:  due,
		Why: why,
	}, now)
	if err != nil {
		return err
	}
	if out == held {
		return nil
	}
	st.Cursor = due
	return s.save(e.Slug, st)
}

type outcome int

const (
	fired   outcome = iota
	held            // paused or repository unknown: nothing changes
	dropped         // busy, inside MinGap or at DailyCap
)

// firing is one firing of an entry. At is what MinGap compares against LastFired: the due time of a clock firing,
// the time of the check for an event.
type firing struct {
	ID, Why string
	At      time.Time
}

// fire raises the Trigger of f unless something holds or drops it. On fired it updates st (LastFired, the day's
// count) for the caller to save. A Receive answer of duplicate counts as fired but not against DailyCap.
func (s *Source) fire(e Entry, st *state, f firing, now time.Time) (outcome, error) {
	if s.cfg.Paused != nil && s.cfg.Paused() {
		return held, nil
	}
	if e.Repo != "" && s.cfg.Known != nil && !s.cfg.Known(e.Repo) {
		return held, nil
	}
	busy, err := s.in.ThreadBusy(ThreadKey(e.Slug))
	if err != nil {
		return held, err
	}
	if busy {
		return dropped, nil
	}
	if !st.LastFired.IsZero() && f.At.Sub(st.LastFired) < MinGap {
		return dropped, nil
	}
	today := now.Format("2006-01-02")
	if st.Date != today {
		st.Date, st.Fires = today, 0
	}
	if st.Fires >= DailyCap {
		return dropped, nil
	}
	res, err := s.in.Receive(core.Trigger{
		ID: f.ID, Source: SourceName, Kind: core.KindSchedule, Scope: "schedule",
		Actor:   core.ActorRef{Source: SourceName, ID: e.Slug},
		At:      now,
		Text:    e.Instructions + "\n\nWhy now: " + f.Why,
		Threads: []string{ThreadKey(e.Slug)},
		Meta:    map[string]string{"repo": e.Repo, "autopilot": e.Slug},
	})
	if err != nil {
		return held, err
	}
	switch res {
	case core.ReceiveStored:
		st.Fires++
	case core.ReceiveDuplicate:
	default:
		return dropped, nil
	}
	st.LastFired = f.At
	return fired, nil
}

// lateText writes a delay in whole minutes, as 5h, 5h30m or 20m.
func lateText(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dm", m)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
