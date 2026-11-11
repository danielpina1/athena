package nodelink

import (
	"errors"
	"os"
	"strings"

	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// The server seeds the worktree of a Ticket that moved to this node (node.Node.Seed): the bundle of the Ticket's
// checkpoint comes as BlobChunks of kind "seed" on BLOB, then the seed call and the Start on CONTROL, which the server
// sends only once its writer took the last chunk. The chunks are written to a file in <StateDir>/tmp on the reader
// goroutine, which then serves the seed call: the node takes the whole file before it sees the Start.

const (
	// blobKindSeed is BlobChunk.kind of a seed bundle.
	blobKindSeed = "seed"
	// maxSeeds bounds the seed transfers one link holds at once.
	maxSeeds = 4
)

// seedTransfer is one seed or branch bundle on its way in.
type seedTransfer struct {
	kind   string
	ticket string
	f      *os.File
	size   int64
	done   bool // its last chunk came
	bad    bool // over the bound, or a write failed: it is no seed
}

// discard closes and removes the transfer's file.
func (tr *seedTransfer) discard() {
	if tr.f != nil {
		tr.f.Close()
		os.Remove(tr.f.Name())
		tr.f = nil
	}
}

// onBlobChunk takes one chunk of a seed or of a shipped branch (branch.go), on the reader goroutine; both count
// against maxSeeds. A chunk of another kind, of another kind or for another Ticket than its transfer's, past the bound
// or past maxSeeds transfers, is dropped (its transfer is then neither).
func (c *Client) onBlobChunk(cn *conn, ch *athenav1.BlobChunk) {
	if ch.GetKind() != blobKindSeed && ch.GetKind() != blobKindBranch {
		c.log.Warn("a blob of an unknown kind from the server; dropped", "kind", ch.GetKind())
		return
	}
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if cn.closed {
		return
	}
	if cn.seeds == nil {
		cn.seeds = map[string]*seedTransfer{}
	}
	id := ch.GetTransferId()
	tr := cn.seeds[id]
	if tr == nil {
		if len(cn.seeds) >= maxSeeds || id == "" {
			c.log.Warn("too many seeds and branches at once from the server; dropped", "kind", ch.GetKind(),
				"ticket", ch.GetTicketId())
			return
		}
		tr = &seedTransfer{kind: ch.GetKind(), ticket: ch.GetTicketId()}
		f, err := os.CreateTemp(c.tmpDir, "athena-"+ch.GetKind()+"-*.bundle")
		if err != nil {
			c.log.Error("keep a seed", "error", err)
			tr.bad = true
		}
		tr.f = f
		cn.seeds[id] = tr
	}
	if tr.bad || tr.done {
		return
	}
	if tr.kind != ch.GetKind() || !strings.EqualFold(tr.ticket, ch.GetTicketId()) {
		tr.bad = true
		tr.discard()
		return
	}
	tr.size += int64(len(ch.GetData()))
	if tr.size > node.MaxCheckpointBundle {
		c.log.Warn("a seed over the bound from the server; dropped", "ticket", tr.ticket)
		tr.bad = true
		tr.discard()
		return
	}
	if _, err := tr.f.Write(ch.GetData()); err != nil {
		c.log.Error("write a seed", "error", err)
		tr.bad = true
		tr.discard()
		return
	}
	if ch.GetLast() {
		tr.done = true
		if err := tr.f.Close(); err != nil {
			tr.bad = true
			os.Remove(tr.f.Name())
		}
	}
}

// seed serves the seed call: the node takes the transfer's bundle, if it came whole, else seeds without one (the
// worktree then starts from the pushed branch or the default branch, and the session hears so).
func (c *Client) seed(cn *conn, t target, a ArgsSeed) error {
	sd := node.Seed{TicketID: a.TicketID, AssignmentID: a.AssignmentID, SHA: a.SHA, At: a.At, From: a.From, Offline: a.Offline,
		Unsent: a.Unsent, Fork: a.Fork, Base: a.Base}
	var path string
	if a.Transfer != "" {
		p, err := c.takeTransfer(cn, blobKindSeed, a.Transfer, a.TicketID)
		switch {
		case errors.Is(err, errNotWhole):
			c.log.Warn("the seed's bundle did not arrive whole; seeding without it", "ticket", a.TicketID)
			if sd.Fork {
				// A fork is nothing without the lead's checkpoint: the child starts from the default branch, told why.
				sd.Fork, sd.Base, sd.SHA, sd.Unsent = false, "", "", node.SeedForkLate
			}
		case err != nil:
			return errors.New("the seed's bundle is another ticket's")
		default:
			path = p
			sd.Bundle = path
		}
	}
	if sd.Fork && path == "" {
		sd.Fork, sd.Base, sd.SHA, sd.Unsent = false, "", "", node.SeedForkLate
	}
	err := t.Seed(sd)
	if path != "" {
		os.Remove(path) // taken (moved) by the node, or refused
	}
	return err
}

// errNotWhole is a transfer that is not there whole: never came, cut short, over a bound, or of another kind.
var errNotWhole = errors.New("did not arrive whole")

// takeTransfer takes the whole transfer id of kind for ticketID off the connection and returns its file, which is the
// caller's to remove. A transfer of another Ticket is discarded with an error; one not whole, with errNotWhole.
func (c *Client) takeTransfer(cn *conn, kind, id, ticketID string) (string, error) {
	cn.mu.Lock()
	tr := cn.seeds[id]
	delete(cn.seeds, id)
	cn.mu.Unlock()
	switch {
	case tr == nil || !tr.done || tr.bad || tr.f == nil || tr.kind != kind:
		if tr != nil {
			tr.discard()
		}
		return "", errNotWhole
	case !strings.EqualFold(tr.ticket, ticketID):
		tr.discard()
		return "", errors.New("the bundle is another ticket's")
	}
	return tr.f.Name(), nil
}

// dropSeeds removes the seeds a connection that ended was still receiving or never used. The caller holds cn.mu.
func (cn *conn) dropSeeds() {
	for id, tr := range cn.seeds {
		if tr.f != nil {
			if !tr.done {
				tr.f.Close()
			}
			os.Remove(tr.f.Name())
			tr.f = nil
		}
		delete(cn.seeds, id)
	}
}
