package policy

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/cron"
)

// ScheduleSource is the Source of a Trigger an Autopilot entry raises.
const ScheduleSource = "schedule"

// Events an Autopilot entry can wait for.
const (
	EventBranchMoved     = "branch_moved"
	EventManifestChanged = "manifest_changed"
	EventCIRed           = "ci_red"
)

// Bounds of the Autopilot entries.
const (
	DefaultCIRedHours        = 4
	MaxAutopilotEntries      = 50
	MaxAutopilotInstructions = 4000
	maxCIRedHours            = 168
	minAutopilotInterval     = 15 * time.Minute
)

// AutopilotEntry is one of the owner's standing instructions: work to do in a repository on a schedule (Cron) or
// when something happens (Event); exactly one of the two is set.
type AutopilotEntry struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
	Repo         string `json:"repo"`
	Enabled      bool   `json:"enabled"`
	Cron         string `json:"cron,omitempty"`
	Event        string `json:"event,omitempty"`
	// CIRedHours is how long a branch stays red before ci_red fires; only for ci_red, and 0 is DefaultCIRedHours.
	CIRedHours int `json:"ciRedHours,omitempty"`
}

var (
	autopilotNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]{0,63}$`)
	autopilotRepoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	slugSepRE       = regexp.MustCompile(`[ _]+`)
)

// AutopilotSlug is the name in lower case with each run of spaces and underscores turned into "-".
func AutopilotSlug(name string) string {
	return slugSepRE.ReplaceAllString(strings.ToLower(name), "-")
}

const (
	autopilotPrefix = "autopilot/"
	// metaAutopilot is the Meta key of a schedule Trigger that holds the slug of the entry that raised it.
	metaAutopilot = "autopilot"
)

// AutopilotRule is the name of the rule an entry's work runs under.
func AutopilotRule(slug string) string { return autopilotPrefix + slug }

// AutopilotEntry returns the entry with the slug.
func (s Settings) AutopilotEntry(slug string) (AutopilotEntry, bool) {
	for _, e := range s.Autopilot {
		if AutopilotSlug(strings.TrimSpace(e.Name)) == slug {
			return e, true
		}
	}
	return AutopilotEntry{}, false
}

// validateAutopilot checks every entry, disabled ones too, since a disabled entry may be enabled later.
func validateAutopilot(s Settings) error {
	if len(s.Autopilot) > MaxAutopilotEntries {
		return fmt.Errorf("policy: %d autopilot entries; at most %d", len(s.Autopilot), MaxAutopilotEntries)
	}
	slugs := map[string]string{}
	for _, e := range s.Autopilot {
		name := strings.TrimSpace(e.Name)
		if !autopilotNameRE.MatchString(name) {
			return fmt.Errorf("policy: autopilot name %q must start with a letter or digit and hold at most 64 letters, digits, spaces, _ or -", name)
		}
		slug := AutopilotSlug(name)
		if other, ok := slugs[slug]; ok {
			return fmt.Errorf("policy: autopilot entries %q and %q are the same entry (slug %q); keep one", other, name, slug)
		}
		slugs[slug] = name
		if strings.TrimSpace(e.Instructions) == "" {
			return fmt.Errorf("policy: autopilot %q has no instructions", name)
		}
		if len([]rune(e.Instructions)) > MaxAutopilotInstructions {
			return fmt.Errorf("policy: autopilot %q instructions are over %d characters", name, MaxAutopilotInstructions)
		}
		if !autopilotRepoRE.MatchString(strings.TrimSpace(e.Repo)) {
			return fmt.Errorf("policy: autopilot %q repo %q is not owner/name", name, e.Repo)
		}
		if (e.Cron == "") == (e.Event == "") {
			return fmt.Errorf("policy: autopilot %q needs exactly one of cron and event", name)
		}
		if e.Cron != "" {
			sch, err := cron.Parse(e.Cron)
			if err != nil {
				return fmt.Errorf("policy: autopilot %q cron: %w", name, err)
			}
			if sch.TooFrequent(minAutopilotInterval) {
				return fmt.Errorf("policy: autopilot %q cron fires less than 15 minutes apart", name)
			}
		}
		switch e.Event {
		case "", EventBranchMoved, EventManifestChanged, EventCIRed:
		default:
			return fmt.Errorf("policy: autopilot %q event %q is not %s, %s or %s", name, e.Event, EventBranchMoved, EventManifestChanged, EventCIRed)
		}
		if e.CIRedHours < 0 || e.CIRedHours > maxCIRedHours {
			return fmt.Errorf("policy: autopilot %q ciRedHours %d is not 0 to %d", name, e.CIRedHours, maxCIRedHours)
		}
		if e.CIRedHours != 0 && e.Event != EventCIRed {
			return fmt.Errorf("policy: autopilot %q sets ciRedHours on an entry that is not ci_red", name)
		}
	}
	return nil
}

// autopilotOutcome turns an assessment of an Autopilot candidate into an Engine outcome. The entry is its own rule,
// so there are no rule judgements to check (any given are ignored); the accept mode decides as for any inferred case.
func autopilotOutcome(a core.Assessment, s Settings) (string, []core.Reason, bool, error) {
	said := func() []core.Reason {
		if strings.TrimSpace(a.Reason) == "" {
			return nil
		}
		return []core.Reason{{Code: CodeSaid, Values: map[string]string{"text": a.Reason}}}
	}
	if !a.Accept {
		return kindIgnore, said(), false, nil
	}
	switch s.Mode {
	case ModeAuto:
		return kindAccept, said(), false, nil
	case ModeObserve:
		return kindIgnore, append([]core.Reason{{Code: CodeWouldAccept}}, said()...), true, nil
	}
	return kindIgnore, []core.Reason{{Code: CodeModeOff}}, false, nil
}

// autopilotNote is the notice for an open Ticket that ran under the entry when the settings change from old to cur:
// the entry went away or off, or its instructions changed.
func autopilotNote(old, cur Settings, slug string) string {
	oe, ok := old.AutopilotEntry(slug)
	if !ok || !oe.Enabled {
		return ""
	}
	ne, ok := cur.AutopilotEntry(slug)
	switch {
	case !ok || !ne.Enabled:
		return fmt.Sprintf("The Autopilot entry %q that started this work no longer applies.", oe.Name)
	case ne.Instructions != oe.Instructions:
		return fmt.Sprintf("The Autopilot entry %q that started this work changed. Its new instructions: %s", ne.Name, ne.Instructions)
	}
	return ""
}
