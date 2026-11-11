package server

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/effects"
	"github.com/danielpina1/athena/internal/fleet"
	"github.com/danielpina1/athena/internal/planner"
)

// planPausedWait is how long a plan waits while Athena is paused.
const planPausedWait = 30 * time.Second

// planHandler is the plan effect handler: it asks the Strategy for a plan-first Ticket's split and hands the Engine the
// draft (PlanDrafted) or the reason it failed (PlanFailed). A failed plan is never retried on its own (R17): the owner
// tries again. Only a store error is retried; a turn the server's stop interrupted stays for the next start. While Athena
// is paused it waits. nodes gives the ready runtimes and the node names a sub-task may name; limits the split limits.
func planHandler(eng *core.Engine, s planner.Strategy, paused func() bool, nodes func() ([]string, []string),
	limits func() core.SplitLimits, log *slog.Logger) effects.Handler {
	return func(ctx context.Context, key string, _ []byte) error {
		t, _, err := eng.Ticket(key)
		if err != nil {
			if refused(err) {
				return nil // gone: nothing to plan
			}
			return err
		}
		if t.Ended != nil || t.Plan == nil || t.Plan.State != core.PlanPlanning {
			return nil
		}
		if paused != nil && paused() {
			return effects.Later(planPausedWait)
		}
		r := planner.Request{TicketID: t.ID, Title: t.Title, Repo: t.Repo, MaxTasks: limits().WithDefaults().MaxChildren}
		if len(t.Inputs) > 0 {
			r.Text = t.Inputs[0].Text
		}
		if t.Execution != nil {
			r.Dir = t.Execution.Directory
		}
		r.Runtimes, r.Nodes = nodes()
		b, planErr := s.Plan(ctx, r)
		if planErr != nil && ctx.Err() != nil {
			return ctx.Err() // the server is stopping: the effect stays and plans again on the next start
		}
		if planErr != nil {
			log.Warn("planning failed", "ticket", t.ID, "strategy", s.Name(), "error", planErr)
			err = eng.PlanFailed(t.ID, planErr.Error())
		} else {
			err = eng.PlanDrafted(t.ID, b)
		}
		if err != nil {
			if refused(err) {
				log.Warn("planning: the Engine refused the plan", "ticket", t.ID, "error", err)
				return nil
			}
			return err
		}
		return nil
	}
}

// fleetPlanFacts are the ready runtimes and the names of the usable nodes of the fleet, for the planner's hints: a
// node online (not draining), a runtime ready on any of them. Both are sorted.
func fleetPlanFacts(f *fleet.Fleet) func() ([]string, []string) {
	return func() ([]string, []string) {
		var runtimes, names []string
		for _, n := range f.Nodes() {
			if n.State != fleet.StateOnline {
				continue
			}
			names = append(names, n.Name)
			for _, r := range n.Status.Runtimes {
				if r.Ready && !slices.Contains(runtimes, r.Name) {
					runtimes = append(runtimes, r.Name)
				}
			}
		}
		slices.Sort(runtimes)
		slices.Sort(names)
		return runtimes, names
	}
}

// refused reports whether err is the Engine refusing a command (a core.UserError), not a store failure.
func refused(err error) bool {
	var ue *core.UserError
	return errors.As(err, &ue)
}
