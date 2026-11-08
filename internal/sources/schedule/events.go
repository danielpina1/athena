package schedule

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/policy"
)

// CIState is what the check suites of a commit say.
type CIState int

const (
	// CIUnknown: no suites yet, or none has failed and some still run.
	CIUnknown CIState = iota
	CIGreen
	CIRed
)

// ErrNoCompare means the two commits cannot be compared, as after a force-push.
var ErrNoCompare = errors.New("schedule: these commits cannot be compared")

// Repos reads the repository facts that events wait for.
type Repos interface {
	Head(ctx context.Context, repo string) (branch, sha string, err error)
	// ChangedFiles returns ErrNoCompare when base is no longer part of the history.
	ChangedFiles(ctx context.Context, repo, base, head string) ([]string, error)
	CI(ctx context.Context, repo, sha string) (CIState, error)
}

// eventFailuresReported is how many polls of a repository must fail in a row before Health says so.
const eventFailuresReported = 3

type eventFailure struct {
	polls int
	err   string
}

// manifests are the base names of the files whose change fires manifest_changed.
var manifests = map[string]bool{
	"go.mod": true, "go.sum": true, "package.json": true, "package-lock.json": true, "pnpm-lock.yaml": true,
	"yarn.lock": true, "requirements.txt": true, "pyproject.toml": true, "poetry.lock": true, "uv.lock": true,
	"Pipfile": true, "Pipfile.lock": true, "Cargo.toml": true, "Cargo.lock": true, "Gemfile": true, "Gemfile.lock": true,
	"composer.json": true, "composer.lock": true, "pom.xml": true, "build.gradle": true, "build.gradle.kts": true,
}

// PollEvents looks once at every enabled event entry. It does nothing while paused or without Repos. A repository
// that cannot be read does not stop the others.
func (s *Source) PollEvents(ctx context.Context) {
	if s.cfg.Repos == nil || s.cfg.Entries == nil || (s.cfg.Paused != nil && s.cfg.Paused()) {
		return
	}
	s.work.Lock()
	defer s.work.Unlock()
	entries := s.cfg.Entries()
	byRepo := map[string][]Entry{} // lower-cased repository → its event entries, in order
	var order []string
	for _, e := range entries {
		if e.Event == "" || e.Repo == "" || (s.cfg.Known != nil && !s.cfg.Known(e.Repo)) {
			continue
		}
		k := strings.ToLower(e.Repo)
		if byRepo[k] == nil {
			order = append(order, k)
		}
		byRepo[k] = append(byRepo[k], e)
	}
	if len(order) == 0 {
		s.mu.Lock()
		s.eventBad = map[string]eventFailure{}
		s.mu.Unlock()
		return
	}
	if err := s.forgetDisabled(entries); err != nil {
		s.log.Warn("schedule state not read", "error", err)
		return
	}
	now := s.cfg.Now()
	bad := map[string]eventFailure{}
	for _, k := range order {
		group := byRepo[k]
		if err := s.pollRepo(ctx, group, now); err != nil {
			f := s.eventBad[k]
			bad[k] = eventFailure{polls: f.polls + 1, err: err.Error()}
			s.log.Warn("schedule repository events not read", "repo", group[0].Repo, "error", err)
		}
	}
	s.mu.Lock()
	s.eventBad = bad
	s.mu.Unlock()
}

// pollRepo reads the head of one repository once and lets each of its entries act on it. It returns the first error
// but still lets every entry have its turn.
func (s *Source) pollRepo(ctx context.Context, group []Entry, now time.Time) error {
	repo := group[0].Repo
	branch, sha, err := s.cfg.Repos.Head(ctx, repo)
	if err != nil {
		return err
	}
	var first error
	for _, e := range group {
		if err := s.pollEvent(ctx, e, branch, sha, now); err != nil && first == nil {
			first = fmt.Errorf("entry %s: %w", e.Slug, err)
		}
	}
	return first
}

func (s *Source) pollEvent(ctx context.Context, e Entry, branch, sha string, now time.Time) error {
	st, err := s.load(e.Slug)
	if err != nil {
		return err
	}
	spec := e.Event + "@" + strings.ToLower(e.Repo)
	if st == nil || st.Spec != spec {
		// A new, edited (event or repository) or re-enabled entry starts from what the repository is now: nothing for the past. The day's
		// count and last firing stay.
		next := &state{}
		if st != nil {
			next = st
		}
		next.Spec, next.Base, next.RedSince, next.Episode = spec, sha, time.Time{}, ""
		if e.Event != policy.EventCIRed {
			return s.save(e.Slug, next)
		}
		st = next
	}
	switch e.Event {
	case policy.EventBranchMoved:
		return s.branchMoved(e, st, branch, sha, now)
	case policy.EventManifestChanged:
		return s.manifestChanged(ctx, e, st, branch, sha, now)
	case policy.EventCIRed:
		return s.ciRed(ctx, e, st, branch, sha, now)
	}
	return fmt.Errorf("unknown event %q", e.Event)
}

// fireEvent fires f for an event entry and saves st when it fired; a dropped event changes no state, so it is
// still there at the next poll (Ruling D). It reports whether the event fired.
func (s *Source) fireEvent(e Entry, st *state, f firing, now time.Time) (bool, error) {
	f.At = now
	out, err := s.fire(e, st, f, now)
	return out == fired, err
}

func (s *Source) branchMoved(e Entry, st *state, branch, sha string, now time.Time) error {
	if sha == st.Base {
		return nil
	}
	ok, err := s.fireEvent(e, st, firing{
		ID:  fmt.Sprintf("%s:%s:head:%s", SourceName, e.Slug, sha),
		Why: fmt.Sprintf("%s of %s moved from %s to %s.", branch, e.Repo, short(st.Base), short(sha)),
	}, now)
	if err != nil || !ok {
		return err
	}
	st.Base = sha
	return s.save(e.Slug, st)
}

func (s *Source) manifestChanged(ctx context.Context, e Entry, st *state, branch, sha string, now time.Time) error {
	if sha == st.Base {
		return nil
	}
	if busy, err := s.in.ThreadBusy(ThreadKey(e.Slug)); err != nil || busy {
		return err
	}
	files, err := s.cfg.Repos.ChangedFiles(ctx, e.Repo, st.Base, sha)
	if errors.Is(err, ErrNoCompare) {
		st.Base = sha
		return s.save(e.Slug, st)
	}
	if err != nil {
		return err
	}
	// A comparison of more than GitHub's 300-file limit is judged on the files returned.
	changed := manifestFiles(files)
	if len(changed) > 0 {
		ok, err := s.fireEvent(e, st, firing{
			ID:  fmt.Sprintf("%s:%s:manifest:%s", SourceName, e.Slug, sha),
			Why: fmt.Sprintf("dependency manifests changed on %s of %s: %s.", branch, e.Repo, listManifests(changed)),
		}, now)
		if err != nil || !ok {
			return err
		}
	}
	st.Base = sha
	return s.save(e.Slug, st)
}

func (s *Source) ciRed(ctx context.Context, e Entry, st *state, branch, sha string, now time.Time) error {
	ci, err := s.cfg.Repos.CI(ctx, e.Repo, sha)
	if err != nil {
		return err
	}
	switch ci {
	case CIGreen:
		if st.RedSince.IsZero() && st.Episode == "" {
			return s.save(e.Slug, st)
		}
		st.RedSince, st.Episode = time.Time{}, ""
		return s.save(e.Slug, st)
	case CIRed:
		if st.RedSince.IsZero() {
			st.RedSince = now
			if err := s.save(e.Slug, st); err != nil {
				return err
			}
		}
	default:
		return s.save(e.Slug, st)
	}
	after := e.CIRedAfter
	if after <= 0 {
		after = policy.DefaultCIRedHours * time.Hour
	}
	episode := strconv.FormatInt(st.RedSince.Unix(), 10)
	if st.Episode == episode || now.Sub(st.RedSince) < after {
		return nil
	}
	ok, err := s.fireEvent(e, st, firing{
		ID:  fmt.Sprintf("%s:%s:ci:%s", SourceName, e.Slug, episode),
		Why: fmt.Sprintf("CI on %s of %s has been red for %s.", branch, e.Repo, lateText(after)),
	}, now)
	if err != nil || !ok {
		return err
	}
	st.Episode = episode
	return s.save(e.Slug, st)
}

// maxListedManifests bounds the paths named in the Trigger text.
const maxListedManifests = 20

// listManifests names the first maxListedManifests paths and counts the others.
func listManifests(changed []string) string {
	if len(changed) <= maxListedManifests {
		return strings.Join(changed, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(changed[:maxListedManifests], ", "), len(changed)-maxListedManifests)
}

// manifestFiles are the changed dependency manifests, sorted and without repeats. A path with a character outside
// the plain set of file names is named by its base name only: the text goes to a worker as the owner's own task, and
// a pusher chooses directory names.
func manifestFiles(files []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		base := path.Base(f)
		if !manifests[base] {
			continue
		}
		if !plainPath(f) {
			f = base
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func plainPath(p string) bool {
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._-/@+", r):
		default:
			return false
		}
	}
	return true
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
