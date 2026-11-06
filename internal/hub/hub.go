// Package hub turns store commits into the dashboard's state: a bounded Snapshot, then Deltas.
//
// A commit tells the hub only the ids it wrote (store.Changes). The hub reads each one's current state from its Source
// once, maps it to a view, gives it the next seq and fans it out to the subscribers. It never trusts the order of
// commit hooks: the view is what the Source holds when the hub reads, so a later read can only be newer.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pause"
	"github.com/danielpina1/athena/internal/pb/athenav1"
	"github.com/danielpina1/athena/internal/store"
)

const (
	// Snapshot bounds.
	endedTickets     = 100
	answeredApproval = 50
	recentThreads    = 100
	recentDecisions  = 200
	recentTriggers   = 200

	// ringDeltas and ringBytes bound the replay ring: whichever is reached first.
	ringDeltas = 4096
	ringBytes  = 8 << 20

	// subBuffer is how many deltas a subscriber may have waiting; one more closes it with resync.
	subBuffer = 256
	// changeBuffer is how many commits may wait for the hub's loop.
	changeBuffer = 1024
	// healthEvery is the period of the one probe the dashboard path has.
	healthEvery = 15 * time.Second
)

// HealthItem is one probed component: a Source, the router, an effects kind, tls or push.
type HealthItem struct {
	Name    string
	OK      bool
	Message string
}

// Source is what the hub reads. The Engine and the Store implement the ticket, approval, trigger, thread and decision
// reads (the server adapts them); tests use a fake. Every read returns the current committed state.
type Source interface {
	// Tickets returns every open Ticket and the endedLimit most recently ended ones. The limit is the Source's to
	// apply (in the database): a long history must not be read to be thrown away.
	Tickets(endedLimit int) ([]core.Ticket, error)
	Ticket(id string) (core.Ticket, core.Stage, error)
	PendingApprovals() ([]core.Approval, error)
	// AnsweredApprovals returns the most recently answered (not pending) Approvals, newest first.
	AnsweredApprovals(limit int) ([]core.Approval, error)
	// TicketAnsweredApprovals returns one Ticket's Approvals that are not pending, newest answered first.
	TicketAnsweredApprovals(ticketID string) ([]core.Approval, error)
	Approval(id string) (core.Approval, error)
	// RecentTriggers returns the newest Triggers, newest first.
	RecentTriggers(limit int) ([]core.Trigger, error)
	Trigger(id string) (core.Trigger, error)
	ActiveThreads() ([]core.Thread, error)
	// RecentThreads returns the most recently active stored Threads, active ones included.
	RecentThreads(limit int) ([]core.Thread, error)
	Thread(key string) (core.Thread, error)
	// Decisions returns the Decisions of a Thread, or of every Thread when thread is empty, newest first.
	Decisions(thread string, limit int) ([]core.Decision, error)
	Decision(id string) (core.Decision, error)
	// Health probes every component. The hub calls it every 15 s on a goroutine of its own, once in Start, and in
	// Snapshot only before Start; it must not call back into the hub.
	Health() []HealthItem
	// Policy returns the current Policy revision (0 while nobody saved settings), its version and canonical JSON.
	Policy() (revision int, version string, settingsJSON string, err error)
	NodeStatus() node.Status
	// Repos returns the known repositories and folders, the last used first, then the local git repositories found on
	// disk. It must not block on I/O beyond the store: the hub reads it on its loop.
	Repos() ([]*athenav1.RepoView, error)
	// GitFolder reports whether dir has a .git entry, as a pass off the hub's loop last found it (false before one has
	// probed it). It must not touch the filesystem: the hub reads it on its loop, for each Ticket summary's branch.
	GitFolder(dir string) bool
	// Trust is the trust class the current Policy gives the Trigger's sender; empty while there is no Policy.
	Trust(t core.Trigger) string
	// Notifications returns the notification kinds that exist and the ones the owner muted.
	Notifications() (kinds, muted []string, err error)
	// Prompts returns the prompt revision and which parts the owner overrode.
	Prompts() (*athenav1.PromptsView, error)
	// Control returns the kill switch.
	Control() pause.State
}

type ringEntry struct {
	d    *athenav1.Delta
	size int
}

// Hub fans store commits out as Deltas. Its methods are safe for concurrent use.
type Hub struct {
	src   Source
	log   *slog.Logger
	epoch string
	now   func() time.Time

	// in carries the commits to the loop; the commit hook never blocks on it (OnCommit). overflowed is set when it was
	// full and a commit was dropped, wake tells the loop.
	in         chan store.Changes
	overflowed atomic.Bool
	wake       chan struct{}
	nodeWake   chan struct{}
	reposWake  chan struct{}
	healthTick time.Duration

	mu        sync.Mutex
	seq       uint64
	floor     uint64 // the oldest cursor a resume can start from: ring[0].seq-1, or seq when the ring is empty
	ring      []ringEntry
	ringBytes int
	subs      map[*Sub]struct{}
	// What the loop last published, to publish a change only.
	health      *athenav1.Health
	healthSince map[string]healthState
	node        *athenav1.NodeStatus
	// quotaAt is when a node delta last went out because only a quota reading differed; quotaEvery is the least time
	// between two such deltas (the const quotaEvery, which a test may set before Start); quotaLater says a timer will
	// publish the node again.
	quotaAt    time.Time
	quotaEvery time.Duration
	quotaLater bool
	repos      *athenav1.RepoList
	notifs     *athenav1.NotificationPrefsView
	closed     bool // the hub has stopped: Subscribe returns closed feeds
	// folders are the placement folders whose facts changed since the loop last republished their Tickets.
	folders map[string]bool

	// actors caches the GitHub login of a reply's Trigger, by Trigger id (replyActors); "" for none.
	actorMu sync.Mutex
	actors  map[string]string

	// running counts the loop and the health probe; Wait waits for both after ctx is done.
	running sync.WaitGroup
}

type healthState struct {
	ok    bool
	msg   string
	since time.Time
}

// New returns a Hub reading src; Start runs it. Its epoch is random per process start.
func New(src Source, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return &Hub{
		src: src, log: log, epoch: hex.EncodeToString(b[:]), now: time.Now,
		in: make(chan store.Changes, changeBuffer), wake: make(chan struct{}, 1), nodeWake: make(chan struct{}, 1),
		reposWake: make(chan struct{}, 1), healthTick: healthEvery, subs: map[*Sub]struct{}{}, healthSince: map[string]healthState{},
		folders: map[string]bool{}, actors: map[string]string{}, quotaEvery: quotaEvery,
	}
}

// Epoch identifies this process's delta sequence: a cursor from another epoch is meaningless.
func (h *Hub) Epoch() string { return h.epoch }

// OnCommit is the function to register with (*store.Store).OnCommit. It only does a non-blocking send: when the loop
// is behind and the buffer is full, the commit is not queued and the hub is marked overflowed instead. The loop then
// drops what is queued, forgets its ring (so no cursor resumes across the loss) and closes every subscriber with
// resync; each client takes a fresh Snapshot, which is read after the lost commit and so includes it.
func (h *Hub) OnCommit(c store.Changes) {
	select {
	case h.in <- c:
	default:
		h.overflowed.Store(true)
		h.signal(h.wake)
	}
}

// NodeChanged tells the hub the node's slots, live Tickets or queue changed (wire it to node.Config.OnStatus).
func (h *Hub) NodeChanged() { h.signal(h.nodeWake) }

// ReposChanged tells the hub the Source's repositories may have changed outside a commit (a scan of the local
// repositories found others); the loop reads them and publishes the list when it differs from the last published.
func (h *Hub) ReposChanged() { h.signal(h.reposWake) }

// FoldersChanged tells the hub a pass over the placement folders found other facts about dirs (a .git entry, where
// they resolve): the loop publishes the repositories when they differ, and the summaries of the Tickets placed in
// those folders, whose branch follows the .git entry.
func (h *Hub) FoldersChanged(dirs []string) {
	h.mu.Lock()
	for _, d := range dirs {
		h.folders[d] = true
	}
	h.mu.Unlock()
	h.signal(h.reposWake)
}

func (h *Hub) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Start runs the hub until ctx is done. It takes the first health reading before it returns.
func (h *Hub) Start(ctx context.Context) {
	health, n := h.probeHealth(), nodeStatus(h.src.NodeStatus())
	// The first reads of the repos and the notification prefs only seed "what was last published"; a failure leaves it
	// empty, and the first change publishes.
	var repos *athenav1.RepoList
	if r, err := h.src.Repos(); err == nil {
		repos = &athenav1.RepoList{Repos: r}
	}
	notifs, _ := h.notificationsView()
	h.mu.Lock()
	h.health, h.node, h.repos, h.notifs = health, n, repos, notifs
	h.mu.Unlock()
	h.running.Add(2) // the loop and its probe
	go h.run(ctx)
}

// Wait returns once the goroutines Start began have returned, after ctx is done: no read of the Source is in flight
// then, so the store behind it may close. Snapshot and Detail, which run on their callers' goroutines, are the
// callers' to stop first.
func (h *Hub) Wait() { h.running.Wait() }

func (h *Hub) run(ctx context.Context) {
	defer h.running.Done()
	defer h.closeAll()
	probed := make(chan *athenav1.Health, 1)
	go func() {
		defer h.running.Done()
		h.probeEvery(ctx, probed)
	}()
	for {
		// An overflow comes first: what is queued is older than the loss and is dropped with it.
		h.recover()
		select {
		case <-ctx.Done():
			return
		case c := <-h.in:
			h.apply(c)
		case <-h.wake:
		case <-h.nodeWake:
			h.publishNode()
		case <-h.reposWake:
			if h.publishRepos() {
				h.publishFolders()
			}
		case next := <-probed:
			h.publishHealth(next)
		}
	}
}

// probeEvery probes health on its own goroutine, so a slow probe never delays the commits' deltas; the loop publishes
// the result. A result the loop has not taken yet is skipped, not queued.
func (h *Hub) probeEvery(ctx context.Context, out chan<- *athenav1.Health) {
	tick := time.NewTicker(h.healthTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			select {
			case out <- h.probeHealth():
			default:
			}
		}
	}
}

// recover handles an overflow, if there was one: see OnCommit.
func (h *Hub) recover() {
	if !h.overflowed.Swap(false) {
		return
	}
	for drained := false; !drained; {
		select {
		case <-h.in:
		default:
			drained = true
		}
	}
	h.log.Warn("hub fell behind the commits: resyncing subscribers")
	h.mu.Lock()
	h.seq++ // a gap, so a cursor from before the loss cannot match
	h.ring, h.ringBytes, h.floor = nil, 0, h.seq
	for s := range h.subs {
		h.dropLocked(s, true)
	}
	h.mu.Unlock()
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		h.dropLocked(s, false)
	}
}

// apply reads the current state of every id one commit wrote and publishes one delta each.
func (h *Hub) apply(c store.Changes) {
	reposStale := false
	for _, id := range c.Tickets {
		t, st, err := h.src.Ticket(id)
		if err != nil {
			h.lost("read ticket", "id", id, "error", err)
			return
		}
		reposStale = reposStale || h.placementNew(&t)
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Ticket{Ticket: ticketSummary(&t, st, h.src.GitFolder)}})
		if h.watched(id) {
			h.sendDetail(id, h.detail(&t, st))
		}
	}
	if reposStale {
		if !h.publishRepos() {
			return
		}
	}
	for _, id := range c.Approvals {
		a, err := h.src.Approval(id)
		if err != nil {
			h.lost("read approval", "id", id, "error", err)
			return
		}
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Approval{Approval: approvalView(&a)}})
	}
	for _, id := range c.Triggers {
		t, err := h.src.Trigger(id)
		if err != nil {
			h.lost("read trigger", "id", id, "error", err)
			return
		}
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Trigger{Trigger: triggerView(&t, h.src.Trust)}})
	}
	for _, key := range c.Threads {
		th, err := h.src.Thread(key)
		if err != nil {
			h.lost("read thread", "key", key, "error", err)
			return
		}
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Thread{Thread: threadView(&th)}})
	}
	for _, id := range c.Decisions {
		d, err := h.src.Decision(id)
		if err != nil {
			h.lost("read decision", "id", id, "error", err)
			return
		}
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Decision{Decision: decisionView(&d)}})
	}
	if c.Notifications {
		if !h.publishNotifications() {
			return
		}
	}
	if c.Prompts {
		p, err := h.src.Prompts()
		if err != nil {
			h.lost("read prompts", "error", err)
			return
		}
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Prompts{Prompts: p}})
	}
	if c.Control {
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Control{Control: controlView(h.src.Control())}})
	}
	if c.Policy {
		if p, err := h.policyView(); err != nil {
			h.lost("read policy", "error", err)
			return
		} else {
			h.publish(&athenav1.Delta{Change: &athenav1.Delta_Policy{Policy: p}})
		}
	}
}

// lost handles a commit whose state the hub could not read: it cannot publish what changed, so it resyncs everyone
// (see OnCommit) rather than leave clients with stale state.
func (h *Hub) lost(what string, args ...any) {
	h.log.Warn("hub: "+what+": resyncing subscribers", args...)
	h.overflowed.Store(true)
}

func (h *Hub) policyView() (*athenav1.PolicyView, error) {
	rev, version, settings, err := h.src.Policy()
	if err != nil {
		return nil, err
	}
	return &athenav1.PolicyView{Revision: uint32(rev), Version: version, SettingsJson: settings}, nil
}

// placementNew reports whether the Ticket can change the known repositories: its repository or folder is not in the
// last published list, or the Ticket is newer than that entry's last use (it moves up). Most commits are a running
// Ticket's progress and answer false without reading the Source.
func (h *Hub) placementNew(t *core.Ticket) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	known := func(name string) bool {
		if name == "" {
			return true
		}
		if h.repos == nil {
			return false
		}
		for _, r := range h.repos.Repos {
			if r.Name == name {
				return r.LastUsedAt != nil && !t.CreatedAt.After(r.LastUsedAt.AsTime())
			}
		}
		return false
	}
	dir := ""
	if t.Execution != nil {
		dir = t.Execution.Directory
	}
	return !known(t.Repo) || !known(dir)
}

// publishRepos publishes the known repositories when they differ from the last published. It reports false when the
// Source could not be read (the hub then resyncs everyone).
func (h *Hub) publishRepos() bool {
	repos, err := h.src.Repos()
	if err != nil {
		h.lost("read repos", "error", err)
		return false
	}
	next := &athenav1.RepoList{Repos: repos}
	h.mu.Lock()
	same := h.repos != nil && proto.Equal(h.repos, next)
	h.repos = next
	h.mu.Unlock()
	if !same {
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Repos{Repos: next}})
	}
	return true
}

// publishFolders publishes the summary (and the detail, to its watchers) of each bounded Ticket placed in a folder
// FoldersChanged named since the last call.
func (h *Hub) publishFolders() {
	h.mu.Lock()
	dirs := h.folders
	if len(dirs) > 0 {
		h.folders = map[string]bool{}
	}
	h.mu.Unlock()
	if len(dirs) == 0 {
		return
	}
	tickets, err := h.src.Tickets(endedTickets)
	if err != nil {
		h.lost("read tickets", "error", err)
		return
	}
	for _, t := range boundTickets(tickets) {
		if t.Execution == nil || !dirs[t.Execution.Directory] {
			continue
		}
		st := core.TicketStage(&t)
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Ticket{Ticket: ticketSummary(&t, st, h.src.GitFolder)}})
		if h.watched(t.ID) {
			h.sendDetail(t.ID, h.detail(&t, st))
		}
	}
}

func (h *Hub) notificationsView() (*athenav1.NotificationPrefsView, error) {
	kinds, muted, err := h.src.Notifications()
	if err != nil {
		return nil, err
	}
	return &athenav1.NotificationPrefsView{Kinds: kinds, Muted: muted}, nil
}

// publishNotifications publishes the notification preferences when they differ from the last published; false when
// the Source could not be read.
func (h *Hub) publishNotifications() bool {
	next, err := h.notificationsView()
	if err != nil {
		h.lost("read notification prefs", "error", err)
		return false
	}
	h.mu.Lock()
	same := h.notifs != nil && proto.Equal(h.notifs, next)
	h.notifs = next
	h.mu.Unlock()
	if !same {
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Notifications{Notifications: next}})
	}
	return true
}

// quotaEvery is the least between two node deltas that differ only in a quota reading: readings arrive with every turn.
const quotaEvery = time.Second

// quotaOnly reports whether a and b differ at most in the readings of the runtimes' quota (their windows, when they
// were read, until when they are limited), not in anything else a node delta carries.
func quotaOnly(a, b *athenav1.NodeStatus) bool {
	strip := func(n *athenav1.NodeStatus) *athenav1.NodeStatus {
		c := proto.Clone(n).(*athenav1.NodeStatus)
		for _, r := range c.Runtimes {
			r.Windows, r.LimitedUntil, r.ReadAt = nil, nil, nil
		}
		return c
	}
	return proto.Equal(strip(a), strip(b))
}

func (h *Hub) publishNode() {
	n := nodeStatus(h.src.NodeStatus())
	h.mu.Lock()
	if h.node != nil && proto.Equal(h.node, n) {
		h.mu.Unlock()
		return
	}
	if wait := h.quotaEvery - time.Since(h.quotaAt); h.node != nil && quotaOnly(h.node, n) && wait > 0 {
		// A quota reading again, too soon after the last: say it once, when the second is over, with what is then known.
		if !h.quotaLater {
			h.quotaLater = true
			time.AfterFunc(wait, func() {
				h.mu.Lock()
				h.quotaLater = false
				h.mu.Unlock()
				h.NodeChanged()
			})
		}
		h.mu.Unlock()
		return
	}
	if h.node != nil && quotaOnly(h.node, n) {
		h.quotaAt = time.Now()
	}
	h.node = n
	h.mu.Unlock()
	h.publish(&athenav1.Delta{Change: &athenav1.Delta_Node{Node: n}})
}

// probeHealth reads the Source's health (without the lock: it may do I/O) and stamps each item with when its state
// last changed.
func (h *Hub) probeHealth() *athenav1.Health {
	items := h.src.Health()
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	out := &athenav1.Health{}
	seen := map[string]bool{}
	for _, it := range items {
		seen[it.Name] = true
		prev, ok := h.healthSince[it.Name]
		if !ok || prev.ok != it.OK || prev.msg != it.Message {
			prev = healthState{ok: it.OK, msg: it.Message, since: now}
			h.healthSince[it.Name] = prev
		}
		out.Items = append(out.Items, &athenav1.HealthItem{Name: it.Name, Ok: it.OK, Message: it.Message, Since: ts(prev.since)})
	}
	for name := range h.healthSince {
		if !seen[name] {
			delete(h.healthSince, name)
		}
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	return out
}

func (h *Hub) publishHealth(next *athenav1.Health) {
	h.mu.Lock()
	same := h.health != nil && proto.Equal(h.health, next)
	h.health = next
	h.mu.Unlock()
	if !same {
		h.publish(&athenav1.Delta{Change: &athenav1.Delta_Health{Health: next}})
	}
}

// publish gives d the next seq, keeps it in the ring and offers it to every subscriber. A subscriber whose buffer is
// full is closed with resync; nothing here waits.
func (h *Hub) publish(d *athenav1.Delta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	d.Seq = h.seq
	size := proto.Size(d)
	h.ring = append(h.ring, ringEntry{d, size})
	h.ringBytes += size
	for len(h.ring) > ringDeltas || (h.ringBytes > ringBytes && len(h.ring) > 1) {
		h.ringBytes -= h.ring[0].size
		h.ring[0] = ringEntry{}
		h.ring = h.ring[1:]
	}
	h.floor = h.ring[0].d.Seq - 1
	for s := range h.subs {
		select {
		case s.ch <- d:
		default:
			h.dropLocked(s, true)
		}
	}
}

// sendDetail sends a ticket_detail delta to the subscribers watching the Ticket. Detail deltas are not sequenced (seq
// 0) and not kept in the ring: they are per-watcher state, so they must not leave a gap in the others' seq. A client
// gets the current detail from Detail when it starts watching.
func (h *Hub) sendDetail(id string, td *athenav1.TicketDetail) {
	d := &athenav1.Delta{Change: &athenav1.Delta_TicketDetail{TicketDetail: td}}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if !s.watching[id] {
			continue
		}
		select {
		case s.ch <- d:
		default:
			h.dropLocked(s, true)
		}
	}
}

func (h *Hub) watched(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.watching[id] {
			return true
		}
	}
	return false
}

// dropLocked removes s and closes its channel; resync says the hub did it because s fell behind.
func (h *Hub) dropLocked(s *Sub, resync bool) {
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	s.resync = resync
	close(s.ch)
}

// Sub is one subscriber's feed.
type Sub struct {
	h        *Hub
	ch       chan *athenav1.Delta
	watching map[string]bool // guarded by h.mu
	resync   bool            // guarded by h.mu; written before ch is closed
}

// C delivers the subscriber's deltas. Deltas are shared between subscribers: do not modify them. The channel is closed
// by Close, by the hub's shutdown, or, when Resync reports true, because the subscriber fell behind.
func (s *Sub) C() <-chan *athenav1.Delta { return s.ch }

// Resync reports whether the hub closed the feed because the subscriber could not keep up (or the hub lost commits):
// the client must take a new Snapshot and subscribe again.
func (s *Sub) Resync() bool {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return s.resync
}

// Close ends the feed; it is idempotent.
func (s *Sub) Close() {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.h.dropLocked(s, false)
}

// Watch asks for ticket_detail deltas of a Ticket; Unwatch stops them.
func (s *Sub) Watch(ticketID string) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.watching[ticketID] = true
}

func (s *Sub) Unwatch(ticketID string) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	delete(s.watching, ticketID)
}

// Subscribe starts a feed. When cursor (of this epoch) is still inside the ring, the feed first replays the deltas
// after it and snapshotNeeded is false. Otherwise (a cursor outside the ring, ahead of it, or of another epoch)
// snapshotNeeded is true and the feed carries only what is published from now on: the caller takes a Snapshot, sends
// it, and then forwards the feed's deltas with seq above the snapshot's cursor, dropping the rest. Deltas are full
// states of one record, so applying one the snapshot already includes is idempotent; one above the cursor can carry
// older state than the snapshot read (the hub reads each record when it publishes), which is transient: a newer
// delta for that record follows and corrects it.
func (h *Hub) Subscribe(cursor uint64, epoch string) (sub *Sub, snapshotNeeded bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var replay []*athenav1.Delta
	snapshotNeeded = epoch != h.epoch || cursor < h.floor || cursor > h.seq
	if !snapshotNeeded {
		for _, e := range h.ring {
			if e.d.Seq > cursor {
				replay = append(replay, e.d)
			}
		}
	}
	sub = &Sub{h: h, ch: make(chan *athenav1.Delta, subBuffer+len(replay)), watching: map[string]bool{}}
	for _, d := range replay {
		sub.ch <- d
	}
	if h.closed {
		close(sub.ch)
		return sub, true
	}
	h.subs[sub] = struct{}{}
	return sub, snapshotNeeded
}

// Snapshot returns the bounded state at the hub's cursor: the cursor is read first, so every delta above it is
// either already in the state or still to come. Tickets: every open one and the 100 most recently ended; Approvals:
// the pending and the last 50 answered; Threads: the active and the 100 most recent; Decisions and Triggers: the last
// 200. It reads the Source afresh; an error means no snapshot (never a partial one).
func (h *Hub) Snapshot() (*athenav1.Snapshot, error) {
	h.mu.Lock()
	cursor := h.seq
	health := h.health
	h.mu.Unlock()
	s := &athenav1.Snapshot{Cursor: cursor}

	tickets, err := h.src.Tickets(endedTickets)
	if err != nil {
		return nil, err
	}
	for _, t := range boundTickets(tickets) {
		s.Tickets = append(s.Tickets, ticketSummary(&t, core.TicketStage(&t), h.src.GitFolder))
	}

	pending, err := h.src.PendingApprovals()
	if err != nil {
		return nil, err
	}
	answered, err := h.src.AnsweredApprovals(answeredApproval)
	if err != nil {
		return nil, err
	}
	for _, a := range append(pending, answered...) {
		s.Approvals = append(s.Approvals, approvalView(&a))
	}

	trigs, err := h.src.RecentTriggers(recentTriggers)
	if err != nil {
		return nil, err
	}
	for _, t := range trigs {
		s.Triggers = append(s.Triggers, triggerView(&t, h.src.Trust))
	}

	active, err := h.src.ActiveThreads()
	if err != nil {
		return nil, err
	}
	recent, err := h.src.RecentThreads(recentThreads)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, th := range append(active, recent...) {
		if !seen[th.Key] {
			seen[th.Key] = true
			s.Threads = append(s.Threads, threadView(&th))
		}
	}

	decs, err := h.src.Decisions("", recentDecisions)
	if err != nil {
		return nil, err
	}
	for _, d := range decs {
		s.Decisions = append(s.Decisions, decisionView(&d))
	}

	s.Node = nodeStatus(h.src.NodeStatus())
	if health == nil {
		health = h.probeHealth() // before Start
	}
	s.Health = health
	if s.Policy, err = h.policyView(); err != nil {
		return nil, err
	}
	if s.Repos, err = h.src.Repos(); err != nil {
		return nil, err
	}
	if s.Notifications, err = h.notificationsView(); err != nil {
		return nil, err
	}
	if s.Prompts, err = h.src.Prompts(); err != nil {
		return nil, err
	}
	s.Control = controlView(h.src.Control())
	return s, nil
}

// boundTickets keeps every open Ticket and the 100 ended ones with the newest end time (Ended.At), then orders them by
// updated_at, newest first.
func boundTickets(all []core.Ticket) []core.Ticket {
	var open, ended []core.Ticket
	for _, t := range all {
		if t.Ended == nil {
			open = append(open, t)
		} else {
			ended = append(ended, t)
		}
	}
	sort.SliceStable(ended, func(i, j int) bool { return ended[i].Ended.At.After(ended[j].Ended.At) })
	if len(ended) > endedTickets {
		ended = ended[:endedTickets]
	}
	out := append(open, ended...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Detail returns a Ticket's summary, timeline and Threads.
func (h *Hub) Detail(ticketID string) (*athenav1.TicketDetail, error) {
	t, st, err := h.src.Ticket(ticketID)
	if err != nil {
		return nil, err
	}
	return h.detail(&t, st), nil
}

func (h *Hub) detail(t *core.Ticket, st core.Stage) *athenav1.TicketDetail {
	var origin *core.Trigger
	if id := t.Origin.TriggerID; id != "" {
		if tr, err := h.src.Trigger(id); err == nil {
			origin = &tr
		} else {
			h.log.Debug("hub: detail: origin trigger", "ticket", t.ID, "trigger", id, "error", err)
		}
	}
	var pending *core.Approval
	if id := t.PendingApproval; id != "" {
		if a, err := h.src.Approval(id); err == nil {
			pending = &a
		}
	}
	// Without the answered Approvals the timeline lacks them; that is better than no detail at all.
	answered, err := h.src.TicketAnsweredApprovals(t.ID)
	if err != nil {
		h.log.Warn("hub: detail: answered approvals", "ticket", t.ID, "error", err)
	}
	return ticketDetail(t, st, h.src.GitFolder, origin, pending, answered, h.replyActors(t))
}

// replyActorsMax bounds the Trigger reads one detail makes to name who replied; actorCacheMax bounds the logins the
// hub remembers (a Trigger never changes, so a login read once holds).
const (
	replyActorsMax = 50
	actorCacheMax  = 4096
)

// replyActors are the GitHub logins of the people whose messages reached t, by Trigger id. It looks at the newest
// replies first, each Trigger once, and reads at most replyActorsMax Triggers the cache does not know.
func (h *Hub) replyActors(t *core.Ticket) map[string]string {
	out := map[string]string{}
	looked := map[string]bool{}
	reads := 0
	for i := len(t.Inputs) - 1; i >= 0 && reads < replyActorsMax; i-- {
		in := &t.Inputs[i]
		id := in.TriggerID
		if !in.Human || in.Context || !strings.HasPrefix(id, "github:") || looked[id] {
			continue
		}
		looked[id] = true
		h.actorMu.Lock()
		login, known := h.actors[id]
		h.actorMu.Unlock()
		if !known {
			reads++
			// A Trigger the store does not have is remembered with no login too: an Input's Trigger is stored before
			// it, so a missing one stays missing, and asking again on every commit would only cost reads. A store
			// that failed is not an answer: nothing is remembered, and the next detail asks again.
			tr, err := h.src.Trigger(id)
			if err != nil && !errors.Is(err, core.ErrNotFound) {
				continue
			}
			login = tr.Actor.ID
			h.actorMu.Lock()
			if len(h.actors) >= actorCacheMax {
				clear(h.actors)
			}
			h.actors[id] = login
			h.actorMu.Unlock()
		}
		if login != "" {
			out[id] = login
		}
	}
	return out
}

func controlView(c pause.State) *athenav1.Control {
	v := &athenav1.Control{Paused: c.Paused, By: c.By}
	if !c.Since.IsZero() {
		v.Since = timestamppb.New(c.Since)
	}
	return v
}
