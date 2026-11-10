package fleet

import "time"

// A remote node whose link ends is offline in Nodes() at once. Its Tickets show the "node offline" person stage (and
// offer Retry elsewhere) only once it stays away OfflineAfter: an offline timer per node, set when its link ends (and,
// for every enrolled node, when the server starts, since the node was last seen), calls Engine.NodeOffline when it
// fires with the node still away; a node that connects stops its timer and Engine.NodeBack clears the flag.

// offTimer is a node's armed offline timer; gen tells a stopped timer's late call from the live one's.
type offTimer struct {
	stop func() bool
	gen  uint64
}

// armAtStart arms the offline timer of every enrolled node: none is connected yet, and each has OfflineAfter to come
// back before its Tickets show it offline, since the last time it was seen.
func (f *Fleet) armAtStart() {
	rows, err := f.cfg.Store.Nodes()
	if err != nil {
		f.log.Error("list nodes for their offline timers", "error", err)
		return
	}
	for _, n := range rows {
		since := n.LastSeen
		if since.IsZero() {
			since = f.now()
		}
		f.armOffline(n.ID, since, f.cfg.OfflineAfter)
	}
}

// armOffline (re)arms the node's offline timer: after d, a node still away is offline since since.
func (f *Fleet) armOffline(id string, since time.Time, d time.Duration) {
	f.offMu.Lock()
	defer f.offMu.Unlock()
	if f.ctx.Err() != nil {
		return
	}
	if old := f.offTimers[id]; old != nil {
		old.stop()
	}
	f.offGen++
	tm := &offTimer{gen: f.offGen}
	f.offTimers[id] = tm
	gen := tm.gen
	tm.stop = f.cfg.AfterFunc(d, func() { f.offlineDue(id, since, gen) })
}

// disarmOffline stops the node's offline timer, if one is armed.
func (f *Fleet) disarmOffline(id string) {
	f.offMu.Lock()
	defer f.offMu.Unlock()
	if tm := f.offTimers[id]; tm != nil {
		tm.stop()
		delete(f.offTimers, id)
	}
}

func (f *Fleet) disarmAll() {
	f.offMu.Lock()
	defer f.offMu.Unlock()
	for id, tm := range f.offTimers {
		tm.stop()
		delete(f.offTimers, id)
	}
}

// offlineDue is the node's offline timer firing: a node still away has its Tickets flagged offline since since. The
// check and the flag are under the node's connMu, as a welcome's NodeBack: a node that connects meanwhile is never
// left flagged.
func (f *Fleet) offlineDue(id string, since time.Time, gen uint64) {
	f.offMu.Lock()
	tm := f.offTimers[id]
	if tm == nil || tm.gen != gen {
		f.offMu.Unlock()
		return
	}
	delete(f.offTimers, id)
	f.offMu.Unlock()
	if f.ctx.Err() != nil {
		return
	}
	r := f.Remote(id)
	r.connMu.Lock()
	defer r.connMu.Unlock()
	if r.Online() {
		return
	}
	eng := f.engine()
	if eng == nil {
		f.armOffline(id, since, placeRetry) // the server is not ready yet
		return
	}
	if err := eng.NodeOffline(id, since); err != nil {
		f.log.Error("flag the node's tickets offline", "node", id, "error", err)
		f.armOffline(id, since, placeRetry)
		return
	}
	f.log.Info("node offline", "node", id, "since", since)
}

// nodeBack stops the node's offline timer and clears its Tickets' offline flag. The caller holds the node's connMu.
func (f *Fleet) nodeBack(id string) {
	f.disarmOffline(id)
	if eng := f.engine(); eng != nil {
		if err := eng.NodeBack(id); err != nil {
			f.log.Error("clear the node's tickets' offline flag", "node", id, "error", err)
		}
	}
}
