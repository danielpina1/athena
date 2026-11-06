package node

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/danielpina1/athena/internal/quota"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

// Status is what a dashboard shows of the node: its slots, the Tickets with a live session, the launches queued, and
// its runtimes.
type Status struct {
	Name          string
	Slots         int
	SlotsInUse    int
	LiveTicketIDs []string // sorted
	Queued        int
	ClaudeVersion string
	// Runtimes are the node's runtimes, sorted by name.
	Runtimes []RuntimeStatus
}

// RuntimeStatus is one runtime of the node: its last known readiness (Claude Code is taken as ready; another runtime's
// is its last check, Why "not checked yet" before the first), the models a task may pick on it (from the same check),
// its quota, and how many live sessions run on it.
type RuntimeStatus struct {
	Name, Version, Why string
	Ready, Tested      bool
	Models             []rt.ModelInfo
	Quota              quota.State
	Live               int
}

// Status returns the node's current status. It never checks a runtime: it shows what the last check found.
func (n *Node) Status() Status {
	n.mu.Lock()
	ids := make([]string, 0, len(n.live))
	for id := range n.live {
		ids = append(ids, id)
	}
	live := n.liveRuntimesLocked(nil)
	n.mu.Unlock()
	slices.Sort(ids)
	var rts []RuntimeStatus
	for _, name := range slices.Sorted(maps.Keys(n.cfg.Runtimes)) {
		info := n.knownInfo(name, n.cfg.Runtimes[name])
		rts = append(rts, RuntimeStatus{Name: name, Version: info.Version, Why: info.Why, Ready: info.Ready, Tested: info.Tested,
			Models: info.Models, Quota: n.quota.State(name), Live: live[name]})
	}
	return Status{Name: n.cfg.Name, Slots: n.cfg.Slots, SlotsInUse: len(ids), LiveTicketIDs: ids,
		Queued: int(n.queued.Load()), ClaudeVersion: n.cfg.ClaudeVersion, Runtimes: rts}
}

// publishStatus records the queue length and calls OnStatus when the live Tickets, the queue, the sessions per
// runtime, or a runtime's readiness or quota changed since the last call. It runs on the loop goroutine, after each
// batch of messages.
func (n *Node) publishStatus(st *loopState) {
	n.queued.Store(int32(len(st.queue)))
	if n.cfg.OnStatus == nil {
		return
	}
	s := n.Status()
	sig := strings.Join(s.LiveTicketIDs, ",") + "|" + strconv.Itoa(s.Queued) + "|" + strconv.FormatInt(n.statusGen.Load(), 10)
	for _, r := range s.Runtimes {
		sig += "|" + r.Name + ":" + strconv.Itoa(r.Live)
	}
	if sig == n.statusSig {
		return
	}
	n.statusSig = sig
	n.cfg.OnStatus()
}
