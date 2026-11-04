package node

import (
	"slices"
	"strconv"
	"strings"
)

// Status is what a dashboard shows of the node: its slots, the Tickets with a live session, and the launches queued.
type Status struct {
	Name          string
	Slots         int
	SlotsInUse    int
	LiveTicketIDs []string // sorted
	Queued        int
	ClaudeVersion string
}

// Status returns the node's current status.
func (n *Node) Status() Status {
	n.mu.Lock()
	ids := make([]string, 0, len(n.live))
	for id := range n.live {
		ids = append(ids, id)
	}
	n.mu.Unlock()
	slices.Sort(ids)
	return Status{Name: n.cfg.Name, Slots: n.cfg.Slots, SlotsInUse: len(ids), LiveTicketIDs: ids,
		Queued: int(n.queued.Load()), ClaudeVersion: n.cfg.ClaudeVersion}
}

// publishStatus records the queue length and calls OnStatus when the live Tickets or the queue changed since the
// last call. It runs on the loop goroutine, after each batch of messages.
func (n *Node) publishStatus(st *loopState) {
	n.queued.Store(int32(len(st.queue)))
	if n.cfg.OnStatus == nil {
		return
	}
	s := n.Status()
	sig := strings.Join(s.LiveTicketIDs, ",") + "|" + strconv.Itoa(s.Queued)
	if sig == n.statusSig {
		return
	}
	n.statusSig = sig
	n.cfg.OnStatus()
}
