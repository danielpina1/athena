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
	if !moved {
		tg.Start(t, *a) // back on the node its work last ran on: nothing to seed
	} else if err := f.seed(t, *a, nodeID, prev, tg); err != nil {
		return err
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
		sd.Unsent = node.SeedSendFailed
		if errors.Is(err, node.ErrCheckpointTooBig) {
			sd.Unsent = node.SeedTooBig
		}
	}
	if file != "" {
		defer os.Remove(file)
	}
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
	m := f.cfg.Mirror
	if m == nil {
		return "", nil
	}
	tmp, err := m.tempFile("athena-seed-*.bundle")
	if err != nil {
		return "", err
	}
	sha, err := m.Bundle(t.Repo, t.ID, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		if errors.Is(err, ErrNoCheckpoint) {
			return "", nil
		}
		return "", err
	}
	sd.SHA = sha
	if c, ok, err := f.Checkpoint(t.ID); err == nil && ok {
		sd.At = c.At
	}
	return tmp.Name(), nil
}

// seedThenStart sends the node the seed (its bundle's chunks, then the seed call) and the Start, on its current link.
func (r *Remote) seedThenStart(sd node.Seed, file string, t core.Ticket, a core.Assignment) error {
	lk := r.link()
	if lk == nil {
		return offline(r.nodeName())
	}
	args := nodelink.ArgsSeed{TicketID: sd.TicketID, AssignmentID: sd.AssignmentID, SHA: sd.SHA, At: sd.At, From: sd.From,
		Offline: sd.Offline, Unsent: sd.Unsent}
	if file != "" {
		id, err := lk.sendSeed(sd.TicketID, file)
		if err != nil {
			return err
		}
		args.Transfer = id
	}
	lk.command(nodelink.ServeSeed, args)
	lk.command(nodelink.ServeStart, nodelink.ArgsStart{Ticket: t, Assignment: a})
	return nil
}

// sendSeed sends the bundle file as BlobChunks of kind "seed" and returns once the link's writer took the last of
// them, so a CONTROL frame queued after it cannot overtake them.
func (lk *nodeLink) sendSeed(ticketID, file string) (string, error) {
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
		chunk := &athenav1.BlobChunk{TransferId: id, Kind: blobKindSeed, TicketId: ticketID, Data: bytes.Clone(buf[:n]), Last: last}
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
