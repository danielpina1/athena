package server

import (
	"log/slog"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/policy"
	"github.com/danielpina1/athena/internal/sources"
	"github.com/danielpina1/athena/internal/sources/github"
	"github.com/danielpina1/athena/internal/sources/schedule"
)

// autopilotEntries are the settings' enabled Autopilot entries as the schedule Source takes them.
func autopilotEntries(s policy.Settings) []schedule.Entry {
	var out []schedule.Entry
	for _, e := range s.Autopilot {
		if !e.Enabled {
			continue
		}
		name := strings.TrimSpace(e.Name)
		entry := schedule.Entry{Slug: policy.AutopilotSlug(name), Name: name, Instructions: e.Instructions,
			Repo: strings.TrimSpace(e.Repo), Cron: e.Cron, Event: e.Event}
		if e.Event == policy.EventCIRed {
			hours := e.CIRedHours
			if hours == 0 {
				hours = policy.DefaultCIRedHours
			}
			entry.CIRedAfter = time.Duration(hours) * time.Hour
		}
		out = append(out, entry)
	}
	return out
}

// newScheduleSource makes the Source that fires the owner's Autopilot entries, under the settings current at each tick.
// An entry waits until the GitHub Source is up and polling its repository.
func newScheduleSource(pc *policyCache, reg *sources.Registry, gh *gitHub, paused func() bool, eventInterval time.Duration,
	in schedule.Intake, kv schedule.KV, log *slog.Logger) *schedule.Source {
	return schedule.New(schedule.Config{
		Entries: func() []schedule.Entry {
			s, _ := pc.current()
			return autopilotEntries(s)
		},
		Known:         func(repo string) bool { return reg.Listens(github.ThreadKey(repo, 1)) },
		Paused:        paused,
		Repos:         github.NewWatcher(gh.app.Client(), gh.cfg.Slug),
		EventInterval: eventInterval,
	}, in, kv, log)
}
