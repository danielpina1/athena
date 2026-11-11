package fleet

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"slices"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/link"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// An Assignment of a Ticket with a Repo that is placed on another node than the Assignment before it (Retry elsewhere
// after its node went offline, or a review placed away from the implementer's node) has its worktree seeded there from
// the Ticket's checkpoint in the mirror: the node gets the bundle and the seed before the Start (node.Node.Seed). The
// embedded node takes the bundle as a file; a remote one in BlobChunks of kind "seed" on its link's BLOB channel, then
// the seed call and the Start on CONTROL once the link's writer took the last chunk (CONTROL is written before BLOB).
//
// A hand_off child (plan 7) placed on another node than its lead's is seeded the same way with a fork seed: the
// lead's checkpoint, once the lead's commit it branches from is in the mirror, and the child's branch made at that
// commit (forkFrom, forkSeed).

const (
	// blobKindSeed is BlobChunk.kind of a seed bundle.
	blobKindSeed = "seed"
	// seedChunk is the most data one seed BlobChunk carries.
	seedChunk = 512 << 10
)

// seedFrom is the node the Ticket's work is on when a, placed on nodeID, moves it from there: the node of the latest
// earlier Assignment that ran (one stranded before it started has no work), else of the one just before a; the Ticket
// has a Repo and that node is another. A Ticket back on the node its work last ran on is not moved.
func (f *Fleet) seedFrom(t core.Ticket, a core.Assignment, nodeID string) (prev string, ok bool) {
	if t.Repo == "" {
		return "", false
	}
	i := slices.IndexFunc(t.Assignments, func(x core.Assignment) bool { return x.ID == a.ID })
	if i <= 0 {
		return "", false
	}
	prev = t.Assignments[i-1].Node
	for j := i - 1; j >= 0; j-- {
		if x := t.Assignments[j]; x.StartedAt != nil || x.SessionID != "" {
			prev = x.Node
			break
		}
	}
	if prev == "" {
		prev = f.cfg.LocalID
	}
	if nodeID == "" {
		nodeID = f.cfg.LocalID
	}
	return prev, prev != nodeID
}

// forkFrom is the node of the lead a new child is forked from when a, placed on nodeID, starts it there: the child has
// a Repo, a lead and a Base (a hand_off child, R4), a is its first Assignment, and the lead's latest Assignment is on
// another node. The child's branch then comes from the lead's checkpoint in the mirror (a fork seed, R7).
func (f *Fleet) forkFrom(t core.Ticket, a core.Assignment, nodeID string) (leadNode string, ok bool) {
	if t.Repo == "" || t.Lead == "" || t.Execution == nil || t.Execution.Base == "" || len(t.Assignments) == 0 ||
		t.Assignments[0].ID != a.ID {
		return "", false
	}
	eng := f.engine()
	if eng == nil {
		return "", false
	}
	lead, _, err := eng.Ticket(t.Lead)
	if err != nil {
		return "", false
	}
	la := lead.LatestAssignment()
	if la == nil {
		return "", false
	}
	leadNode = f.nodeKey(la.Node)
	return leadNode, leadNode != f.nodeKey(nodeID)
}

// needsSeed reports whether a, placed on nodeID, is seeded before its Start: its Ticket moved there (seedFrom), or it
// is a child forked from its lead on another node (forkFrom).
func (f *Fleet) needsSeed(t core.Ticket, a core.Assignment, nodeID string) bool {
	if _, moved := f.seedFrom(t, a, nodeID); moved {
		return true
	}
	_, fork := f.forkFrom(t, a, nodeID)
	return fork
}

// nodeKey is the node id an Assignment placed on id runs on: LocalID for "".
func (f *Fleet) nodeKey(id string) string {
	if id == "" {
		return f.cfg.LocalID
	}
	return id
}

// NodeName is the node's name: the server's own for "" and LocalID.
func (f *Fleet) NodeName(id string) string { return f.nodeName(id) }

func (f *Fleet) nodeName(id string) string {
	if id == "" || id == f.cfg.LocalID {
		return f.cfg.LocalName
	}
	return f.Remote(id).nodeName()
}

// launchSeeded seeds the just placed Assignment's worktree on its node, then starts it there, on a goroutine of the
// Fleet's: building and sending a bundle takes a while, and placement goes on meanwhile. As launch, an Assignment found
// ended after its Start is stopped again. A remote that is offline gets nothing: reconcile seeds and starts it when it
// connects (the Assignment is still pending).
func (f *Fleet) launchSeeded(ticketID, assignmentID, nodeID string) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	if run := f.seeding[assignmentID]; run != nil {
		// Placement and reconcile (or a resume) can both ask: one seed runs, and runs again only if it failed.
		run.again = true
		f.mu.Unlock()
		return
	}
	run := &seedRun{}
	f.seeding[assignmentID] = run
	f.seeds.Add(1)
	f.mu.Unlock()
	go func() {
		defer f.seeds.Done()
		for {
			err := f.seedAndStart(ticketID, assignmentID, nodeID)
			if err != nil && f.ctx.Err() == nil {
				f.log.Warn("seed and start", "ticket", ticketID, "assignment", assignmentID, "node", nodeID, "error", err)
			}
			f.mu.Lock()
			again := run.again && err != nil && !f.closed
			run.again = false
			if !again {
				delete(f.seeding, assignmentID)
			}
			f.mu.Unlock()
			if !again {
				return
			}
		}
	}()
}

// seedRun is a seed under way; again is set when another launch of the same Assignment asked meanwhile.
type seedRun struct{ again bool }

// seedHook, when set (tests), runs as a seed starts.
var seedHook func()

func (f *Fleet) seedAndStart(ticketID, assignmentID, nodeID string) error {
	if seedHook != nil {
		seedHook()
	}
	eng := f.engine()
	tg := f.Target(nodeID)
	if eng == nil || tg == nil {
		return nil
	}
	find := func() (core.Ticket, *core.Assignment, bool) {
		t, _, err := eng.Ticket(ticketID)
		if err != nil {
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
		return nil
	}
	prev, moved := f.seedFrom(t, *a, nodeID)
	leadNode, fork := f.forkFrom(t, *a, nodeID)
	// The branches it needs first (branches.go): this goroutine may wait for them.
	f.shipAll(t, nodeID)
	switch {
	case fork:
		if err := f.forkSeed(t, *a, nodeID, leadNode, tg); err != nil {
			return err
		}
	case moved:
		if err := f.seed(t, *a, nodeID, prev, tg); err != nil {
			return err
		}
	default:
		tg.Start(t, *a) // back on the node its work last ran on: nothing to seed
	}
	if t, a, ok = find(); ok && (a.State == core.AssignmentEnded || t.Ended != nil) {
		tg.Stop(t, *a, a.EndedReason)
	}
	return nil
}

// seed sends the node the Ticket's seed from prev, then the Start.
func (f *Fleet) seed(t core.Ticket, a core.Assignment, nodeID, prev string, tg Target) error {
	sd := node.Seed{TicketID: t.ID, AssignmentID: a.ID, From: f.nodeName(prev)}
	if i := slices.IndexFunc(t.Assignments, func(x core.Assignment) bool { return x.ID == a.ID }); i > 0 {
		sd.Offline = t.Assignments[i-1].EndedReason == core.ReasonNodeOffline
	}
	file, err := f.seedBundle(t, &sd)
	if err != nil {
		f.log.Warn("the checkpoint cannot be bundled; seeding without it", "ticket", t.ID, "error", err)
		sd.Unsent = unsentReason(err)
	}
	if file != "" {
		defer os.Remove(file)
	}
	return f.seedThenStart(sd, file, t, a, nodeID, tg)
}

// unsentReason is the node.Seed.Unsent of a bundle the mirror could not make.
func unsentReason(err error) string {
	if errors.Is(err, node.ErrCheckpointTooBig) {
		return node.SeedTooBig
	}
	return node.SeedSendFailed
}

// forkSeed sends the node a fork seed of the child t, placed away from its lead's node leadNode, then the Start: once
// the lead's commit (Execution.Base) is in the mirror, waiting up to shipWait for it, the bundle of the lead's
// checkpoint; past that wait, no bundle, and the child starts from the default branch (node.SeedForkLate).
func (f *Fleet) forkSeed(t core.Ticket, a core.Assignment, nodeID, leadNode string, tg Target) error {
	base := t.Execution.Base
	sd := node.Seed{TicketID: t.ID, AssignmentID: a.ID, From: f.nodeName(leadNode)}
	var file string
	if f.cfg.Mirror != nil && f.waitMirror(t.Repo, base) {
		var err error
		file, sd.SHA, err = f.mirrorFile(t.Repo, t.Lead)
		switch {
		case err != nil:
			f.log.Warn("the lead's checkpoint cannot be bundled; the child starts without it", "ticket", t.ID, "lead", t.Lead,
				"error", err)
			sd.Unsent = unsentReason(err)
		case file == "":
			sd.Unsent = node.SeedForkLate
		default:
			defer os.Remove(file)
			sd.Fork, sd.Base = true, base
		}
	} else {
		f.log.Warn("the lead's commit did not reach the mirror in time; the child starts from the default branch",
			"ticket", t.ID, "lead", t.Lead, "commit", base)
		sd.Unsent = node.SeedForkLate
	}
	return f.seedThenStart(sd, file, t, a, nodeID, tg)
}

// seedThenStart sends the node the seed sd, with its bundle file ("" for none), then the Start.
func (f *Fleet) seedThenStart(sd node.Seed, file string, t core.Ticket, a core.Assignment, nodeID string, tg Target) error {
	if nodeID == "" || nodeID == f.cfg.LocalID {
		sd.Bundle = file
		if err := f.cfg.Local.Seed(sd); err != nil {
			return err
		}
		tg.Start(t, a)
		return nil
	}
	return f.Remote(nodeID).seedThenStart(sd, file, t, a)
}

// seedBundle writes the Ticket's checkpoint from the mirror to a file in the mirror's temporary directory and sets
// sd's commit and time; "" when the mirror holds none.
func (f *Fleet) seedBundle(t core.Ticket, sd *node.Seed) (string, error) {
	file, sha, err := f.mirrorFile(t.Repo, t.ID)
	if err != nil || file == "" {
		return "", err
	}
	sd.SHA = sha
	if c, ok, err := f.Checkpoint(t.ID); err == nil && ok {
		sd.At = c.At
	}
	return file, nil
}

// mirrorFile writes the bundle of the Ticket's checkpoint in repo's mirror to a file in the mirror's temporary
// directory, and returns it with its commit; "" when the mirror holds none (or there is no mirror). The file is the
// caller's to remove.
func (f *Fleet) mirrorFile(repo, ticketID string) (file, sha string, err error) {
	m := f.cfg.Mirror
	if m == nil {
		return "", "", nil
	}
	tmp, err := m.tempFile("athena-bundle-*.bundle")
	if err != nil {
		return "", "", err
	}
	sha, err = m.Bundle(repo, ticketID, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		if errors.Is(err, ErrNoCheckpoint) {
			return "", "", nil
		}
		return "", "", err
	}
	return tmp.Name(), sha, nil
}

// seedThenStart sends the node the seed (its bundle's chunks, then the seed call) and the Start, on its current link.
func (r *Remote) seedThenStart(sd node.Seed, file string, t core.Ticket, a core.Assignment) error {
	lk := r.link()
	if lk == nil {
		return offline(r.nodeName())
	}
	args := nodelink.ArgsSeed{TicketID: sd.TicketID, AssignmentID: sd.AssignmentID, SHA: sd.SHA, At: sd.At, From: sd.From,
		Offline: sd.Offline, Unsent: sd.Unsent, Fork: sd.Fork, Base: sd.Base}
	if file != "" {
		id, err := lk.sendFile(blobKindSeed, sd.TicketID, file)
		if err != nil {
			return err
		}
		args.Transfer = id
	}
	lk.command(nodelink.ServeSeed, args)
	lk.command(nodelink.ServeStart, nodelink.ArgsStart{Ticket: t, Assignment: a})
	return nil
}

// sendFile sends the bundle file as BlobChunks of kind ("seed", or "branch" for a shipped branch) and returns its
// transfer id once the link's writer took the last of them, so a CONTROL frame queued after it cannot overtake them.
func (lk *nodeLink) sendFile(kind, ticketID, file string) (string, error) {
	fh, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	id := hex.EncodeToString(idb)
	buf := make([]byte, seedChunk)
	for {
		n, rerr := io.ReadFull(fh, buf)
		last := rerr == io.EOF || rerr == io.ErrUnexpectedEOF
		if rerr != nil && !last {
			return "", rerr
		}
		chunk := &athenav1.BlobChunk{TransferId: id, Kind: kind, TicketId: ticketID, Data: bytes.Clone(buf[:n]), Last: last}
		if err := lk.sendBlob(chunk); err != nil {
			return "", err
		}
		if last {
			break
		}
	}
	for lk.sess.Queued(athenav1.Channel_BLOB) > 0 {
		if !lk.sleep(retryPause) {
			return "", offline(lk.r.nodeName())
		}
	}
	if lk.ctx.Err() != nil {
		return "", offline(lk.r.nodeName())
	}
	return id, nil
}

// sendBlob queues one durable BlobChunk, waiting out backpressure while the link lasts.
func (lk *nodeLink) sendBlob(chunk *athenav1.BlobChunk) error {
	for {
		err := lk.sess.Send(athenav1.Channel_BLOB, true, &athenav1.Frame{Body: &athenav1.Frame_BlobChunk{BlobChunk: chunk}})
		if !errors.Is(err, link.ErrBackpressure) {
			if errors.Is(err, link.ErrClosed) {
				return offline(lk.r.nodeName())
			}
			return err
		}
		if !lk.sleep(retryPause) {
			return offline(lk.r.nodeName())
		}
	}
}
