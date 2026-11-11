package fleet

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
)

// Children deliver commits (R6), and a lead merges its children's branches, a dependent its deps' (R5). A child or
// dep that ran on another node than the Ticket that needs it has its branch shipped through the server's mirror (R7),
// never node to node: before the fleet forwards a Start or Deliver of a Ticket with a Repo, it ships each needed
// branch the target node does not have yet, as a bundle of that Ticket's checkpoint from the mirror (BlobChunks of
// kind "branch", then the importBranch call, whose answer the fleet waits for).
//
// A child's last checkpoint may reach the mirror after its ready report, so a ship first waits, up to shipWait, for
// the reported head to be in the mirror (recordCheckpoint wakes it). That wait never runs on the Engine's goroutine:
// a Ticket with branches to ship has its Starts and Delivers queued, and one goroutine of the Fleet's ships, then
// forwards them, in call order. A Ticket with nothing to ship and nothing queued is forwarded inline, as before. A
// ship that fails is logged and the forward goes on: the Ticket's Input names the branch, and its worker can ask the
// owner. Which branches a node has is kept in memory, by node, child and head; a node that links again starts over.

// shipWait bounds the wait for a commit to reach the mirror, by a ship or a fork seed; a variable for tests.
var shipWait = 5 * time.Minute

const (
	// blobKindBranch is BlobChunk.kind of a shipped branch's bundle.
	blobKindBranch = "branch"
	// importTimeout bounds a node's import of a shipped branch, which bounds its git at 5 minutes itself.
	importTimeout = 6 * time.Minute
)

// forwardHook, when set (tests), runs as the fleet forwards a Start (deliver false) or Deliver of t to the node.
var forwardHook func(nodeID string, deliver bool, t core.Ticket)

// shipKey is a branch a node has: the child's lowercased id and its head.
type shipKey struct{ node, child, sha string }

// shipOp is a Start or Deliver waiting for its Ticket's ships.
type shipOp struct {
	t       core.Ticket
	a       core.Assignment
	nodeID  string
	deliver bool
}

// shipQueue is a Ticket's ops waiting for its ships, in call order.
type shipQueue struct{ ops []shipOp }

// needed returns the ended children and ended deps of t that completed with a Head, children first, each once.
func needed(t core.Ticket, get func(id string) (core.Ticket, bool)) []core.Ticket {
	var out []core.Ticket
	seen := map[string]bool{}
	for _, id := range slices.Concat(t.Children, t.Deps) {
		if seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		c, ok := get(id)
		if !ok || c.Ended == nil || c.Ended.Outcome != core.OutcomeCompleted || headOf(c) == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// headOf is the branch head of a child's ready report.
func headOf(c core.Ticket) string {
	a := c.LatestAssignment()
	if a == nil || a.Report == nil {
		return ""
	}
	return a.Report.Head
}

// toShip are the Tickets whose branches t needs on nodeID: those whose last Assignment ran on another node, and not
// shipped there yet.
func (f *Fleet) toShip(t core.Ticket, nodeID string) []core.Ticket {
	eng := f.engine()
	if t.Repo == "" || f.cfg.Mirror == nil || eng == nil || len(t.Children)+len(t.Deps) == 0 {
		return nil
	}
	get := func(id string) (core.Ticket, bool) {
		c, _, err := eng.Ticket(id)
		return c, err == nil
	}
	nodeID = f.nodeKey(nodeID)
	var out []core.Ticket
	for _, c := range needed(t, get) {
		a := c.LatestAssignment()
		if a == nil || f.nodeKey(a.Node) == nodeID || f.hasShipped(nodeID, c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (f *Fleet) hasShipped(nodeID string, c core.Ticket) bool {
	f.shipMu.Lock()
	defer f.shipMu.Unlock()
	return f.shipped[shipKey{nodeID, strings.ToLower(c.ID), headOf(c)}]
}

// send forwards a Start (deliver false) or Deliver of t to the node, after the branches t needs there: inline when
// there are none and none of t's ops is queued, else queued behind them; queued reports which.
func (f *Fleet) send(t core.Ticket, a core.Assignment, nodeID string, deliver bool) (queued bool) {
	op := shipOp{t: t, a: a, nodeID: nodeID, deliver: deliver}
	ships := f.toShip(t, nodeID)
	f.shipMu.Lock()
	q := f.shipQs[t.ID]
	if q == nil && (len(ships) == 0 || f.ctx.Err() != nil) {
		f.shipMu.Unlock()
		f.forward(op)
		return false
	}
	if q == nil {
		// Under mu, as Close sets closed before it waits for the queues: no queue starts after that.
		f.mu.Lock()
		closed := f.closed
		if !closed {
			f.ships.Add(1)
		}
		f.mu.Unlock()
		if closed {
			f.shipMu.Unlock()
			return false
		}
		q = &shipQueue{}
		f.shipQs[t.ID] = q
		go f.runShips(t.ID, q)
	}
	q.ops = append(q.ops, op)
	f.shipMu.Unlock()
	return true
}

// forward sends op to its node.
func (f *Fleet) forward(op shipOp) {
	tg := f.Target(op.nodeID)
	if tg == nil {
		return
	}
	if forwardHook != nil {
		forwardHook(op.nodeID, op.deliver, op.t)
	}
	if op.deliver {
		tg.Deliver(op.t, op.a)
	} else {
		tg.Start(op.t, op.a)
	}
}

// runShips ships, then forwards, the Ticket's queued ops in order, until none is left.
func (f *Fleet) runShips(ticketID string, q *shipQueue) {
	defer f.ships.Done()
	for {
		f.shipMu.Lock()
		if len(q.ops) == 0 {
			delete(f.shipQs, ticketID)
			f.shipMu.Unlock()
			return
		}
		op := q.ops[0]
		q.ops = q.ops[1:]
		f.shipMu.Unlock()
		f.shipThenForward(op)
	}
}

// shipThenForward ships what op's Ticket needs on its node, then forwards op, unless its Assignment ended meanwhile
// (the Engine's Stop went out). As launch, a Start whose Assignment is found ended after it is stopped again.
func (f *Fleet) shipThenForward(op shipOp) {
	eng := f.engine()
	if eng == nil || f.ctx.Err() != nil {
		return
	}
	live := func() (core.Ticket, *core.Assignment, bool) {
		t, _, err := eng.Ticket(op.t.ID)
		if err != nil {
			f.log.Error("ship: read ticket", "ticket", op.t.ID, "error", err)
			return t, nil, false
		}
		for i := range t.Assignments {
			if a := &t.Assignments[i]; a.ID == op.a.ID {
				return t, a, t.Ended == nil && a.State != core.AssignmentEnded
			}
		}
		return t, nil, false
	}
	t, _, ok := live()
	if !ok {
		return
	}
	f.shipAll(t, op.nodeID)
	f.forward(op)
	if op.deliver {
		return
	}
	if t, a, ok := live(); !ok && a != nil {
		if tg := f.Target(op.nodeID); tg != nil {
			tg.Stop(t, *a, a.EndedReason)
		}
	}
}

// shipAll ships each branch t needs on the node and does not have yet, logging the ships that fail.
func (f *Fleet) shipAll(t core.Ticket, nodeID string) {
	for _, c := range f.toShip(t, nodeID) {
		if err := f.ship(t, c, nodeID); err != nil && f.ctx.Err() == nil {
			f.log.Warn("ship a branch", "ticket", t.ID, "child", c.ID, "node", nodeID, "error", err)
		}
	}
}

// ship sends the node the branch of c at its head, which t needs: once the head is in the mirror (waiting up to
// shipWait), a bundle of c's checkpoint, imported there as athena/<c>.
func (f *Fleet) ship(t, c core.Ticket, nodeID string) error {
	head := headOf(c)
	repo := c.Repo
	if repo == "" {
		repo = t.Repo
	}
	if !f.waitMirror(repo, head) {
		return fmt.Errorf("%s's commit %s did not reach the server within %s", c.ID, head, shipWait)
	}
	file, _, err := f.mirrorFile(repo, c.ID)
	if err != nil {
		return err
	}
	if file == "" {
		return fmt.Errorf("the server holds no checkpoint of %s", c.ID)
	}
	defer os.Remove(file)
	args := nodelink.ArgsImportBranch{TicketID: t.ID, Child: c.ID, SHA: head}
	if f.nodeKey(nodeID) == f.cfg.LocalID {
		if f.cfg.Local == nil {
			return fmt.Errorf("no local node")
		}
		err = f.cfg.Local.ImportBranch(node.BranchImport{TicketID: t.ID, Child: c.ID, SHA: head, Bundle: file})
	} else {
		err = f.Remote(nodeID).importBranch(args, file)
	}
	if err != nil {
		return err
	}
	f.shipMu.Lock()
	f.shipped[shipKey{f.nodeKey(nodeID), strings.ToLower(c.ID), head}] = true
	f.shipMu.Unlock()
	f.log.Info("branch shipped", "ticket", t.ID, "child", c.ID, "node", nodeID, "commit", head)
	return nil
}

// importBranch sends the node a shipped branch: the bundle file's chunks, then the importBranch call, whose answer it
// returns.
func (r *Remote) importBranch(args nodelink.ArgsImportBranch, file string) error {
	lk := r.link()
	if lk == nil {
		return offline(r.nodeName())
	}
	id, err := lk.sendFile(blobKindBranch, args.Child, file)
	if err != nil {
		return err
	}
	args.Transfer = id
	_, err = r.callOn(r.f.ctx, lk, lk.nextID.Add(1), importTimeout, nodelink.ServeImportBranch, args)
	return err
}

// waitMirror waits up to shipWait for the commit sha to be in repo's mirror, woken by each checkpoint recorded; false
// when it is not there by then (or the Fleet closed). A commit a wait already gave up on is looked up without waiting
// again, so a node that never sends it does not hold every later forward of the Ticket.
func (f *Fleet) waitMirror(repo, sha string) bool {
	if f.cfg.Mirror == nil {
		return false
	}
	key := strings.ToLower(repo) + " " + sha
	timer := time.NewTimer(shipWait)
	defer timer.Stop()
	for {
		f.shipMu.Lock()
		wake, late := f.mirrorWake, f.late[key]
		f.shipMu.Unlock()
		ok, err := f.cfg.Mirror.Has(repo, sha)
		if err != nil {
			f.log.Warn("look a commit up in the mirror", "repo", repo, "commit", sha, "error", err)
			return false
		}
		if ok || late {
			if ok && late {
				f.shipMu.Lock()
				delete(f.late, key)
				f.shipMu.Unlock()
			}
			return ok
		}
		select {
		case <-wake:
		case <-timer.C:
			f.shipMu.Lock()
			f.late[key] = true
			f.shipMu.Unlock()
			return false
		case <-f.ctx.Done():
			return false
		}
	}
}

// mirrorChanged wakes the waits for a commit to reach the mirror.
func (f *Fleet) mirrorChanged() {
	f.shipMu.Lock()
	close(f.mirrorWake)
	f.mirrorWake = make(chan struct{})
	f.shipMu.Unlock()
}

// forgetShipped forgets which branches the node has: it linked again.
func (f *Fleet) forgetShipped(nodeID string) {
	f.shipMu.Lock()
	defer f.shipMu.Unlock()
	for k := range f.shipped {
		if k.node == nodeID {
			delete(f.shipped, k)
		}
	}
}
