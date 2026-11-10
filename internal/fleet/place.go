package fleet

import (
	"cmp"
	"context"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
)

// LocalID is the id of the node embedded in the server.
const LocalID = "local"

// placeRetry is the pause before a placement that failed on the store is tried again.
const placeRetry = time.Second

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidName reports whether s follows the node-name rule: 1 to 32 characters of a-z, 0-9 and -, starting with a letter.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// LocalName is the node-name form of a host name: lower case, every other character a "-", cut to 32 characters and
// started with a letter; "this-machine" when nothing of it is left.
func LocalName(host string) string {
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	b := []byte(host)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	s := strings.TrimLeft(string(b), "0123456789-")
	if len(s) > 32 {
		s = s[:32]
	}
	s = strings.TrimRight(s, "-")
	if !ValidName(s) {
		return "this-machine"
	}
	return s
}

// --- choosing a node ---

// candidate is a node as placement sees it.
type candidate struct {
	ID, Name                  string
	Local                     bool
	Online, Draining, Revoked bool
	// Slots is how many sessions the node runs at once; InUse and Queued its sessions and the launches waiting.
	Slots, InUse, Queued int
	// Ready are the runtimes the node may launch on now, by name.
	Ready map[string]bool
}

// load is the node's (slots in use + queued) / slots.
func (c candidate) load() float64 {
	return float64(c.InUse+c.Queued) / float64(max(c.Slots, 1))
}

// neededRuntime is the runtime the Assignment must launch on, "" when any ready one will do: the Assignment's own pin,
// else the Ticket's runtime (Claude Code when it names none); auto and a review let the node choose.
func neededRuntime(t core.Ticket, a core.Assignment) string {
	if a.Runtime != "" {
		return a.Runtime
	}
	if a.Role == core.RoleReview || (t.Execution != nil && t.Execution.Runtime == core.RuntimeAuto) {
		return ""
	}
	if t.Execution != nil && t.Execution.Runtime != "" {
		return t.Execution.Runtime
	}
	return core.RuntimeClaude
}

// dirTicket reports whether the Ticket works in a folder of one machine rather than in a repository.
func dirTicket(t core.Ticket) bool {
	return t.Repo == "" && t.Execution != nil && t.Execution.Directory != ""
}

func pinOf(t core.Ticket) string {
	if t.Execution != nil {
		return t.Execution.Node
	}
	return ""
}

// choose picks the node of a pending Assignment, or says what it waits for. The rules, in order:
//
//  1. Hard rules. A node qualifies only when it is online, not draining, not revoked and not in a.Avoid; it is the
//     Ticket's pinned node when there is one; it has the runtime ready (the pinned one, or any for auto and a review);
//     and, for a Dir Ticket, it is the node the folder belongs to (the pinned node, else the local one).
//  2. Warmth: the node of the Ticket's previous Assignment.
//  3. For a review, a node with a ready runtime other than the implementer's (preferred, not required).
//  4. Load: the lowest (slots in use + queued) / slots; ties go to the local node, then by name.
//
// While no remote node is enrolled, an unpinned Assignment that only the runtime keeps off the local node goes there
// all the same: the node's own launch check then says why it cannot run, as before there were nodes.
func choose(t core.Ticket, a core.Assignment, nodes []candidate) (id string, waitReason string) {
	pin, need, dir := pinOf(t), neededRuntime(t, a), dirTicket(t)
	var ok []candidate
	var local *candidate
	remotes := false
	localOnlyRuntime := false
	for i := range nodes {
		c := nodes[i]
		if c.Local {
			local = &nodes[i]
		} else if !c.Revoked {
			remotes = true
		}
		if !c.Online || c.Draining || c.Revoked || slices.Contains(a.Avoid, c.ID) {
			continue
		}
		if pin != "" && c.ID != pin {
			continue
		}
		if dir && pin == "" && !c.Local {
			continue
		}
		if !hasRuntime(c, need) {
			localOnlyRuntime = localOnlyRuntime || c.Local
			continue
		}
		ok = append(ok, c)
	}
	if len(ok) == 0 {
		if pin == "" && !remotes && localOnlyRuntime {
			return local.ID, ""
		}
		switch {
		case pin != "":
			name := pin
			for _, c := range nodes {
				if c.ID == pin {
					name = c.Name
				}
			}
			return "", "Waiting for " + name
		case dir && local != nil:
			return "", "Waiting for " + local.Name
		case need == "":
			return "", "Waiting for a node with a ready runtime"
		}
		return "", "Waiting for a node with " + core.RuntimeLabel(need)
	}
	if prev := previousNode(t, a, local); prev != "" {
		for _, c := range ok {
			if c.ID == prev {
				return c.ID, ""
			}
		}
	}
	if a.Role == core.RoleReview && t.Review != nil && t.Review.Runtime != "" {
		// A review prefers a runtime other than the implementer's (the node's chooser picks it when ready): a node that
		// has one ready wins over one that has only the implementer's, whatever their load.
		var other []candidate
		for _, c := range ok {
			if hasOtherRuntime(c, t.Review.Runtime) {
				other = append(other, c)
			}
		}
		if len(other) > 0 {
			ok = other
		}
	}
	best := slices.MinFunc(ok, func(x, y candidate) int {
		if c := cmp.Compare(x.load(), y.load()); c != 0 {
			return c
		}
		if x.Local != y.Local {
			if x.Local {
				return -1
			}
			return 1
		}
		return cmp.Compare(x.Name, y.Name)
	})
	return best.ID, ""
}

// previousNode is the node of the Assignment before a ("" placed on none: the local node, from before there were
// nodes), or "" when a is the first.
func previousNode(t core.Ticket, a core.Assignment, local *candidate) string {
	i := slices.IndexFunc(t.Assignments, func(x core.Assignment) bool { return x.ID == a.ID })
	if i < 0 {
		i = len(t.Assignments)
	}
	if i == 0 {
		return ""
	}
	prev := t.Assignments[i-1].Node
	if prev == "" && local != nil {
		prev = local.ID
	}
	return prev
}

func hasOtherRuntime(c candidate, not string) bool {
	for name, r := range c.Ready {
		if r && name != not {
			return true
		}
	}
	return false
}

func hasRuntime(c candidate, need string) bool {
	if need != "" {
		return c.Ready[need]
	}
	for _, r := range c.Ready {
		if r {
			return true
		}
	}
	return false
}

// --- the Executor ---

var _ core.Executor = (*Fleet)(nil)

// Start queues a pending Assignment that is not placed yet for placement, on the fleet's own goroutine; any other goes
// to its node (an Assignment placed on no node, from before there were nodes, to the local one).
func (f *Fleet) Start(t core.Ticket, a core.Assignment) {
	if a.Node == "" && a.State == core.AssignmentPending {
		f.enqueue(t.ID, a.ID)
		return
	}
	if tg := f.Target(a.Node); tg != nil {
		tg.Start(t, a)
	}
}

// Deliver sends the Ticket's waiting Inputs to the node its Assignment runs on. An Assignment still waiting for its
// node takes them with its Start.
func (f *Fleet) Deliver(t core.Ticket, a core.Assignment) {
	if a.Node == "" && a.State == core.AssignmentPending {
		return
	}
	if tg := f.Target(a.Node); tg != nil {
		tg.Deliver(t, a)
	}
}

// Stop ends the Assignment on its node, and drops it from placement if it still waits for one.
func (f *Fleet) Stop(t core.Ticket, a core.Assignment, reason string) {
	if a.Node == "" {
		f.unqueue(t.ID, a.ID)
	}
	if tg := f.Target(a.Node); tg != nil {
		tg.Stop(t, a, reason)
	}
}

// SetPaused sends the owner's kill switch to every node, now and to each remote that connects later.
func (f *Fleet) SetPaused(paused bool) {
	f.pubMu.Lock()
	defer f.pubMu.Unlock()
	f.paused = paused
	for _, tg := range f.targets() {
		tg.SetPaused(paused) // dropped by an offline remote: sendState redoes it on connect
	}
}

// DropTokens has every node drop its write tokens of a repository the owner held, now and each remote that connects
// within a token's life (an offline remote keeps the tokens it holds).
func (f *Fleet) DropTokens(repo string) {
	f.pubMu.Lock()
	defer f.pubMu.Unlock()
	f.dropped[repo] = f.now()
	for _, tg := range f.targets() {
		tg.DropTokens(repo)
	}
}

// tokenLife bounds how long a node may hold a write token: a GitHub installation token lives an hour.
const tokenLife = time.Hour

// sendState queues, on a link about to be published, the kill switch and the token drops a node that was away may
// have missed. The caller holds pubMu.
func (f *Fleet) sendState(lk *nodeLink) {
	lk.command(nodelink.ServeSetPaused, nodelink.ArgsSetPaused{Paused: f.paused})
	for repo, at := range f.dropped {
		if f.now().Sub(at) > tokenLife {
			delete(f.dropped, repo)
			continue
		}
		lk.command(nodelink.ServeDropTokens, nodelink.ArgsDropTokens{Repo: repo})
	}
}

// targets are the local node and every remote known.
func (f *Fleet) targets() []Target {
	var out []Target
	if f.cfg.Local != nil {
		out = append(out, f.cfg.Local)
	}
	f.mu.Lock()
	for _, r := range f.remotes {
		out = append(out, r)
	}
	f.mu.Unlock()
	return out
}

// WaitReason is why the Ticket's pending Assignment is not placed yet ("Waiting for mac-studio"), or "".
func (f *Fleet) WaitReason(ticketID string) string {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	if w := f.waiting[ticketID]; w != nil {
		return w.reason
	}
	return ""
}

// --- per-Ticket calls, to the node that has the Ticket ---

// ticketTarget is the node that has the Ticket's work: the one its latest placed Assignment is on (an Assignment
// placed on none that ran is the local node's, from before there were nodes). A Ticket the Engine does not know goes
// to the local node, which answers for it as it always did.
func (f *Fleet) ticketTarget(ticketID string) (Target, error) {
	id := ""
	if eng := f.engine(); eng != nil {
		t, _, err := eng.Ticket(ticketID)
		var ue *core.UserError
		switch {
		case err == nil:
			id = placedNode(t)
		case !errors.As(err, &ue):
			return nil, err
		}
	}
	return f.Target(id), nil
}

func placedNode(t core.Ticket) string {
	for i := len(t.Assignments) - 1; i >= 0; i-- {
		a := t.Assignments[i]
		if a.Node != "" {
			return a.Node
		}
		if a.State != core.AssignmentPending {
			return ""
		}
	}
	return ""
}

// Interrupt asks the Ticket's session, on its node, to abandon its current turn.
func (f *Fleet) Interrupt(ticketID string) error {
	tg, err := f.ticketTarget(ticketID)
	if err != nil {
		return err
	}
	if tg == nil {
		return node.ErrNoSession
	}
	return tg.Interrupt(ticketID)
}

// Subscribe feeds the Ticket's transcript from its node (web.Streams).
func (f *Fleet) Subscribe(ticketID string, from int64) ([]node.Line, <-chan node.Line, func(), error) {
	tg, err := f.ticketTarget(ticketID)
	if err != nil {
		return nil, nil, nil, err
	}
	if tg == nil {
		return nil, nil, nil, node.ErrNoWorkspace
	}
	return tg.Subscribe(ticketID, from)
}

// TicketDiff is the Ticket's diff on its node (web.Artifacts).
func (f *Fleet) TicketDiff(ctx context.Context, ticketID string) (node.Diff, error) {
	tg, err := f.ticketTarget(ticketID)
	if err != nil {
		return node.Diff{}, err
	}
	if tg == nil {
		return node.Diff{}, node.ErrNoWorkspace
	}
	return tg.TicketDiff(ctx, ticketID)
}

// TicketFiles lists the Ticket's files on its node (web.Artifacts).
func (f *Fleet) TicketFiles(ctx context.Context, ticketID string) ([]node.FileEntry, bool, error) {
	tg, err := f.ticketTarget(ticketID)
	if err != nil {
		return nil, false, err
	}
	if tg == nil {
		return nil, false, node.ErrNoWorkspace
	}
	return tg.TicketFiles(ctx, ticketID)
}

// OpenTicketFile opens one of the Ticket's files from its node (web.Artifacts).
func (f *Fleet) OpenTicketFile(ctx context.Context, ticketID, rel string) (*os.File, error) {
	tg, err := f.ticketTarget(ticketID)
	if err != nil {
		return nil, err
	}
	if tg == nil {
		return nil, node.ErrNoWorkspace
	}
	return tg.OpenTicketFile(ctx, ticketID, rel)
}

// ApplyProposal has the node that has the Proposal's Ticket push its branch or open its pull request.
func (f *Fleet) ApplyProposal(ctx context.Context, p core.Proposal) error {
	tg, err := f.ticketTarget(p.TicketID)
	if err != nil {
		return err
	}
	if tg == nil {
		return node.ErrNoWorkspace
	}
	return tg.ApplyProposal(ctx, p)
}

// --- placement ---

// waitEntry is a pending Assignment waiting for placement.
type waitEntry struct {
	assignment string
	seq        uint64 // queue order
	reason     string // why it waits, once a pass found no node
}

func (f *Fleet) enqueue(ticketID, assignmentID string) {
	f.pmu.Lock()
	if w := f.waiting[ticketID]; w == nil || w.assignment != assignmentID {
		f.pseq++
		f.waiting[ticketID] = &waitEntry{assignment: assignmentID, seq: f.pseq}
	}
	f.pmu.Unlock()
	signal(f.placeWake)
}

func (f *Fleet) unqueue(ticketID, assignmentID string) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	if w := f.waiting[ticketID]; w != nil && w.assignment == assignmentID {
		delete(f.waiting, ticketID)
	}
}

// placeLoop places the waiting Assignments whenever one is queued or a node changes (connects, reports a status, is
// drained or undrained), until Close.
func (f *Fleet) placeLoop() {
	defer close(f.placeDone)
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-f.placeWake:
		}
		if f.placeWaiting() {
			time.AfterFunc(placeRetry, func() { signal(f.placeWake) })
		}
	}
}

// placeWaiting runs one placement pass over the waiting Assignments, oldest first; true when one failed on the store
// and is to be tried again.
func (f *Fleet) placeWaiting() (retry bool) {
	eng := f.engine()
	if eng == nil {
		return false
	}
	f.pmu.Lock()
	type item struct {
		ticket string
		w      waitEntry
	}
	var queue []item
	for id, w := range f.waiting {
		queue = append(queue, item{id, *w})
	}
	f.pmu.Unlock()
	if len(queue) == 0 {
		return false
	}
	slices.SortFunc(queue, func(x, y item) int { return cmp.Compare(x.w.seq, y.w.seq) })
	tickets, err := eng.Tickets(false)
	if err != nil {
		f.log.Error("placement: list tickets", "error", err)
		return true
	}
	byID := make(map[string]*core.Ticket, len(tickets))
	for i := range tickets {
		byID[tickets[i].ID] = &tickets[i]
	}
	nodes := f.candidates(tickets)
	for _, it := range queue {
		if f.ctx.Err() != nil {
			return false
		}
		t := byID[it.ticket]
		var a *core.Assignment
		if t != nil {
			a = t.LatestAssignment()
		}
		if t == nil || a == nil || a.ID != it.w.assignment || a.State != core.AssignmentPending || a.Node != "" {
			f.unqueue(it.ticket, it.w.assignment) // ended, replaced, or placed already
			continue
		}
		id, why := choose(*t, *a, nodes)
		if id == "" {
			f.pmu.Lock()
			if w := f.waiting[it.ticket]; w != nil && w.assignment == a.ID {
				w.reason = why
			}
			f.pmu.Unlock()
			continue
		}
		if err := eng.AssignmentPlaced(t.ID, a.ID, id); err != nil {
			var ue *core.UserError
			if errors.As(err, &ue) {
				f.unqueue(it.ticket, a.ID)
				continue
			}
			f.log.Error("placement: record", "ticket", t.ID, "assignment", a.ID, "node", id, "error", err)
			retry = true
			continue
		}
		f.unqueue(it.ticket, a.ID)
		for i := range nodes {
			if nodes[i].ID == id {
				nodes[i].Queued++ // the next Assignment of this pass sees it
			}
		}
		f.log.Info("assignment placed", "ticket", t.ID, "assignment", a.ID, "node", id)
		f.launch(t.ID, a.ID, id)
	}
	return retry
}

// launch starts a just placed Assignment on its node, with a snapshot read after the placement committed, so it carries
// the node and every Input that came before. A Stop the Engine sent between that read and the Start may have reached
// the node first: an Assignment found ended after the Start is stopped again.
func (f *Fleet) launch(ticketID, assignmentID, nodeID string) {
	eng := f.engine()
	tg := f.Target(nodeID)
	if tg == nil {
		return
	}
	find := func() (core.Ticket, *core.Assignment, bool) {
		t, _, err := eng.Ticket(ticketID)
		if err != nil {
			f.log.Error("placement: read ticket", "ticket", ticketID, "error", err)
			return t, nil, false
		}
		for i := range t.Assignments {
			if t.Assignments[i].ID == assignmentID {
				return t, &t.Assignments[i], true
			}
		}
		return t, nil, false
	}
	t, a, ok := find()
	if !ok || a.State == core.AssignmentEnded || t.Ended != nil {
		return
	}
	if _, moved := f.seedFrom(t, *a, nodeID); moved {
		f.launchSeeded(ticketID, assignmentID, nodeID) // its worktree comes from the checkpoint first
		return
	}
	tg.Start(t, *a)
	if t, a, ok = find(); ok && (a.State == core.AssignmentEnded || t.Ended != nil) {
		tg.Stop(t, *a, a.EndedReason)
	}
}

// candidates are the nodes as placement sees them now. A node's load is the larger of what its status says and what
// the Engine says is on it (pending, or running with its turn going), so Assignments placed since its last status count.
func (f *Fleet) candidates(tickets []core.Ticket) []candidate {
	onNode := map[string]int{}
	for i := range tickets {
		a := tickets[i].LatestAssignment()
		if a == nil || a.State == core.AssignmentEnded {
			continue
		}
		if a.State == core.AssignmentRunning && a.IdleAt != nil && (a.DeliveredAt == nil || !a.DeliveredAt.After(*a.IdleAt)) {
			continue // idle: no turn going
		}
		id := a.Node
		if id == "" {
			if a.State == core.AssignmentPending {
				continue // waiting for placement
			}
			id = f.cfg.LocalID
		}
		onNode[id]++
	}
	var out []candidate
	for _, v := range f.Nodes() {
		c := candidate{ID: v.ID, Name: v.Name, Local: v.Local, Online: v.State == StateOnline || v.State == StateDraining,
			Draining: v.State == StateDraining, Revoked: v.State == StateRevoked, Ready: map[string]bool{}}
		s := v.Status
		c.Slots, c.InUse, c.Queued = s.Slots, s.SlotsInUse, s.Queued
		for _, r := range s.Runtimes {
			// A runtime not checked yet may well be ready: the node's own check decides at launch.
			c.Ready[r.Name] = r.Ready || !r.Tested
		}
		if v.Caps != nil {
			if c.Slots == 0 {
				c.Slots = int(v.Caps.GetSlots())
			}
			if len(s.Runtimes) == 0 {
				for _, r := range v.Caps.GetRuntimes() {
					c.Ready[r.GetName()] = r.GetReady()
				}
			}
		}
		if n := onNode[v.ID]; n > c.InUse+c.Queued {
			c.Queued = n - c.InUse
		}
		out = append(out, c)
	}
	return out
}
