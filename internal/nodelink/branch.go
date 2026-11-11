package nodelink

import (
	"errors"
	"os"

	"github.com/danielpina1/athena/internal/node"
)

// The server ships a branch to this node (plan 7, R7): the branch of a child or dep that ran on another node, which
// the Ticket starting or being delivered to here needs. The bundle of the child's checkpoint comes from the server's
// mirror as BlobChunks of kind "branch" on BLOB, which count with the seeds against maxSeeds (seed.go); then the
// importBranch call on CONTROL, sent once the server's writer took the last chunk. The node imports the branch
// (node.Node.ImportBranch) and answers; the server forwards the Start or Deliver only after that answer.

// blobKindBranch is BlobChunk.kind of a shipped branch's bundle.
const blobKindBranch = "branch"

// importBranch imports the transfer's bundle at path (the server's word that a.Child's branch is a.SHA) and answers
// call id; the file is removed after, whatever the import did with it.
func (c *Client) importBranch(cn *conn, t target, id uint64, a ArgsImportBranch, path string) {
	err := t.ImportBranch(node.BranchImport{TicketID: a.TicketID, Child: a.Child, SHA: a.SHA, Bundle: path})
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		c.log.Warn("remove a branch's bundle", "file", path, "error", rerr)
	}
	c.reply(cn, id, nil, err)
}
