package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/quota"
	"github.com/danielpina1/athena/internal/runtime/claude"
	"github.com/danielpina1/athena/internal/runtime/codex"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// roleWorker is the role of a Ticket's worker session.
const roleWorker = "worker"

// DefaultRuntimes are the runtimes of a Config that names none: Claude Code alone, run from cfg.ClaudeBinary. A server
// that has more (Codex) adds them to these.
func DefaultRuntimes(cfg Config) map[string]rt.Runtime {
	return map[string]rt.Runtime{runtimeClaude: claude.Runtime{Binary: cfg.ClaudeBinary}}
}

// runtimeName is the runtime a Ticket names when it pins one: its Execution's; Claude Code when it names none (a Ticket
// from before runtimes), and for auto, which pins none (the chooser picks per Assignment).
func runtimeName(t core.Ticket) string {
	name := runtimeClaude
	if t.Execution != nil && t.Execution.Runtime != "" && t.Execution.Runtime != core.RuntimeAuto {
		name = t.Execution.Runtime
	}
	return name
}

// ranOn is the runtime an Assignment's session ran on: the one it recorded, or, for an Assignment from before runtimes
// were recorded, the Ticket's (runtimeName). It is "" for an Assignment that never ran (no start, no session).
func ranOn(t core.Ticket, a core.Assignment) string {
	if a.StartedAt == nil && a.SessionID == "" {
		return ""
	}
	if a.Runtime != "" {
		return a.Runtime
	}
	return runtimeName(t)
}

// pinnedRuntime is the runtime an Assignment must launch on, "" when the chooser may pick (auto): the Assignment's own
// (pinned when it was made, or the one it ran on, which a resume keeps), else the Ticket's when it names claude or codex
// (or none, from before runtimes: Claude Code).
func pinnedRuntime(t core.Ticket, a core.Assignment) string {
	if a.Runtime != "" {
		return a.Runtime
	}
	if r := ranOn(t, a); r != "" {
		return r
	}
	if t.Execution != nil && t.Execution.Runtime == core.RuntimeAuto {
		return ""
	}
	return runtimeName(t)
}

// assumedReady is the Info of Claude Code, which the node takes as ready without asking it: its sessions fail on their
// own when it is not, and `claude --version` costs a process per launch. Its version is the one the Config names.
func (n *Node) assumedReady() rt.Info {
	return rt.Info{Installed: true, Ready: true, Tested: true, Version: n.cfg.ClaudeVersion}
}

// checkRuntime is what the node knows of a runtime's readiness now: Claude Code is assumed ready (assumedReady);
// another runtime is asked (Codex caches its answer). The answer is kept for Status, unless ctx ended meanwhile. ctx
// bounds the check.
func (n *Node) checkRuntime(ctx context.Context, name string, r rt.Runtime) rt.Info {
	info := n.assumedReady()
	if r.Name() != runtimeClaude {
		info = r.Check(ctx)
		if !info.Ready && info.Why == "" {
			info.Why = "it is not ready"
		}
		if ctx.Err() != nil {
			return info // cut short (a stop): no answer for Status
		}
	}
	n.infoMu.Lock()
	old, had := n.infos[name]
	n.infos[name] = info
	n.infoMu.Unlock()
	if !had || old != info {
		n.statusChanged()
	}
	return info
}

// RefreshRuntimes checks every runtime but Claude Code (assumedReady) once, so Status shows their readiness before
// their first launch. The server runs it in the background at start; ctx bounds it.
func (n *Node) RefreshRuntimes(ctx context.Context) {
	for _, name := range slices.Sorted(maps.Keys(n.cfg.Runtimes)) {
		if r := n.cfg.Runtimes[name]; r.Name() != runtimeClaude {
			n.checkRuntime(ctx, name, r)
		}
	}
}

// knownInfo is the runtime's last known readiness: Claude Code's assumed one, another's last Check (Why "not checked
// yet" before its first).
func (n *Node) knownInfo(name string, r rt.Runtime) rt.Info {
	if r.Name() == runtimeClaude {
		return n.assumedReady()
	}
	n.infoMu.Lock()
	defer n.infoMu.Unlock()
	if info, ok := n.infos[name]; ok {
		return info
	}
	return rt.Info{Why: "not checked yet"}
}

// --- the runtime chooser ---

// Chooser picks the runtime an Assignment's session launches on, or says why it waits or fails. It must not block:
// the node calls it with what it already knows (ChoiceInput).
type Chooser interface {
	Choose(in ChoiceInput) Choice
}

// ChoiceInput is what a Chooser decides from: the launch (the Ticket as its launch saw it, and the Assignment to
// launch), the readiness of the node's runtimes by name (only the pinned one's, for a pinned launch), their quota, how
// many sessions each runs besides this launch, and the time.
type ChoiceInput struct {
	Ticket     core.Ticket
	Assignment core.Assignment
	Ready      map[string]rt.Info
	Quota      func(runtime string) quota.State
	Live       map[string]int
	Now        time.Time
}

// Choice is a Chooser's answer. With Runtime set and a zero Until and no AwaitSession, the session launches on it now.
// A non-zero Until waits until then; AwaitSession waits for a session of the node to end (a runtime's session cap
// frees) or for Until, whichever comes first; Runtime then names the runtime waited for, if one. Runtime "" with
// neither fails the launch with Why. Why says why in every case but a launch.
type Choice struct {
	Runtime      string
	Until        time.Time
	Why          string
	AwaitSession bool
}

// unknownWait is how long a launch waits when every runtime is limited and none says until when.
const unknownWait = 15 * time.Minute

// HeadroomChooser chooses by readiness and quota headroom:
//
//  1. An Assignment pinned to a runtime (Assignment.Runtime, or the runtime it ran on: a resume keeps its session's)
//     runs on it; a Ticket that names claude or codex too. Not ready, the launch fails with its Why; limited, it waits
//     until its reset; at its session cap, for a session to end. No other runtime takes its place.
//  2. auto: the candidates are the ready runtimes that are not limited and below their MaxSessions. A review drops the
//     implementer's runtime when another candidate is left; then the candidates over any AvoidAbove window are dropped,
//     unless that leaves none. The runtime of the Ticket's latest Assignment of the same role that ran (a warm
//     session) wins, else Preferred, else the largest headroom (ties: by name).
//  3. No candidate: the launch waits until the earliest reset (unknownWait when none is known), or for a session to
//     end when a session cap excluded one. No runtime ready at all: it fails with "no runtime is ready: <whys>".
type HeadroomChooser struct {
	// Preferred is the owner's default runtime among the candidates.
	Preferred string
	// AvoidAbove maps a window name ("5h", "7d") to the fraction of it used above which a runtime is avoided.
	AvoidAbove map[string]float64
	// MaxSessions caps the sessions a runtime runs at once; 0 or missing is no cap.
	MaxSessions map[string]int
}

// DefaultChooser is the HeadroomChooser of a node configured with none: Claude Code preferred, runtimes avoided above
// 85% of their 5-hour or 90% of their 7-day window (nucleos' quota-brake defaults), no session caps.
func DefaultChooser() HeadroomChooser {
	return HeadroomChooser{Preferred: runtimeClaude, AvoidAbove: map[string]float64{"5h": 0.85, "7d": 0.90}}
}

// Choose implements Chooser.
func (c HeadroomChooser) Choose(in ChoiceInput) Choice {
	if name := pinnedRuntime(in.Ticket, in.Assignment); name != "" {
		return c.pinned(in, name)
	}
	return c.auto(in)
}

func (c HeadroomChooser) pinned(in ChoiceInput, name string) Choice {
	info, ok := in.Ready[name]
	if !ok {
		return Choice{Why: fmt.Sprintf("runtime %q is not available on this node", name)}
	}
	if !info.Ready {
		return Choice{Why: name + " cannot run this ticket: " + whyOf(info)}
	}
	if q := in.quota(name); q.Limited(in.Now) {
		return Choice{Runtime: name, Until: *q.LimitedUntil, Why: name + " is limited until " + stamp(*q.LimitedUntil)}
	}
	if c.atCap(name, in.Live) {
		return Choice{Runtime: name, AwaitSession: true, Why: c.capWhy(name)}
	}
	return Choice{Runtime: name}
}

func (c HeadroomChooser) auto(in ChoiceInput) Choice {
	var ready, whys []string
	for _, name := range slices.Sorted(maps.Keys(in.Ready)) {
		if info := in.Ready[name]; info.Ready {
			ready = append(ready, name)
		} else {
			whys = append(whys, name+": "+whyOf(info))
		}
	}
	if len(ready) == 0 {
		if len(whys) == 0 {
			whys = []string{"this node has none"}
		}
		return Choice{Why: "no runtime is ready: " + strings.Join(whys, "; ")}
	}
	var cands, waits []string
	var earliest time.Time
	capped := false
	for _, name := range ready {
		q := in.quota(name)
		switch {
		case q.Limited(in.Now):
			if earliest.IsZero() || q.LimitedUntil.Before(earliest) {
				earliest = *q.LimitedUntil
			}
			waits = append(waits, name+" is limited until "+stamp(*q.LimitedUntil))
		case c.atCap(name, in.Live):
			capped = true
			waits = append(waits, c.capWhy(name))
		default:
			cands = append(cands, name)
		}
	}
	if len(cands) == 0 {
		ch := Choice{Until: earliest, AwaitSession: capped, Why: "no runtime is free: " + strings.Join(waits, "; ")}
		if earliest.IsZero() && !capped {
			ch.Until = in.Now.Add(unknownWait)
		}
		return ch
	}
	if in.Assignment.Role == core.RoleReview && len(cands) > 1 {
		if impl := latestRan(in.Ticket, in.Assignment.ID, func(a core.Assignment) bool { return a.Role != core.RoleReview }); impl != "" {
			if rest := slices.DeleteFunc(slices.Clone(cands), func(n string) bool { return n == impl }); len(rest) > 0 {
				cands = rest
			}
		}
	}
	if kept := slices.DeleteFunc(slices.Clone(cands), func(n string) bool { return c.over(in.quota(n), in.Now) }); len(kept) > 0 {
		cands = kept
	}
	role := in.Assignment.Role
	if warm := latestRan(in.Ticket, in.Assignment.ID, func(a core.Assignment) bool { return a.Role == role }); slices.Contains(cands, warm) {
		return Choice{Runtime: warm}
	}
	if slices.Contains(cands, c.Preferred) {
		return Choice{Runtime: c.Preferred}
	}
	best := cands[0]
	for _, n := range cands[1:] {
		if in.quota(n).Headroom(in.Now) > in.quota(best).Headroom(in.Now) {
			best = n
		}
	}
	return Choice{Runtime: best}
}

// quota is the runtime's quota, unmeasured without a Quota.
func (in ChoiceInput) quota(name string) quota.State {
	if in.Quota == nil {
		return quota.State{}
	}
	return in.Quota(name)
}

// over reports whether a window of q that has not reset by now is used above its AvoidAbove threshold.
func (c HeadroomChooser) over(q quota.State, now time.Time) bool {
	for _, w := range q.Windows {
		limit, ok := c.AvoidAbove[w.Name]
		if !ok || w.Stale || (w.ResetsAt != nil && !w.ResetsAt.After(now)) {
			continue
		}
		if w.Used > limit {
			return true
		}
	}
	return false
}

func (c HeadroomChooser) atCap(name string, live map[string]int) bool {
	max := c.MaxSessions[name]
	return max > 0 && live[name] >= max
}

func (c HeadroomChooser) capWhy(name string) string {
	return fmt.Sprintf("%s runs its most sessions (%d)", name, c.MaxSessions[name])
}

// latestRan is the runtime of the Ticket's latest Assignment other than skip that ran and that keep accepts; "" if none.
func latestRan(t core.Ticket, skip string, keep func(core.Assignment) bool) string {
	for i := len(t.Assignments) - 1; i >= 0; i-- {
		a := t.Assignments[i]
		if a.ID == skip || !keep(a) {
			continue
		}
		if r := ranOn(t, a); r != "" {
			return r
		}
	}
	return ""
}

func whyOf(info rt.Info) string {
	if info.Why == "" {
		return "it is not ready"
	}
	return info.Why
}

// stamp is a time in a Choice's Why.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// invalidator is a runtime whose readiness answer is cached (Codex): a session start that found it uncontrolled drops
// the answer, so the next launch checks (and re-trusts its hooks) again.
type invalidator interface{ Invalidate() }

// uncontrolled reports whether a session start failed because the runtime would run it outside Athena's control (a
// hook or a setting that is not Athena's): its Why is in the error.
func uncontrolled(err error) bool {
	var hu codex.ErrHooksUncontrolled
	var cu codex.ErrConfigUncontrolled
	return errors.As(err, &hu) || errors.As(err, &cu)
}
