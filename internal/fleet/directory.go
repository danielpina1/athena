package fleet

import (
	"strings"

	"github.com/danielpina1/athena/internal/core"
	"github.com/danielpina1/athena/internal/store"
)

// The Fleet is the Engine's core.NodeDirectory: a child's node hint names a node by id or name. The Engine asks
// inside a command's transaction, while the store's one connection is taken, so the directory answers from the nodes
// as last read (dir), refreshed when the Fleet starts, a node links or is revoked, and whenever Nodes lists them.

var _ core.NodeDirectory = (*Fleet)(nil)

// Resolve returns the id of the node named or identified by nameOrID: the server's own node (by LocalID or LocalName,
// when it has one), else an enrolled node not revoked, by id, then by name ignoring case.
func (f *Fleet) Resolve(nameOrID string) (string, bool) {
	s := strings.TrimSpace(nameOrID)
	if s == "" {
		return "", false
	}
	if f.cfg.Local != nil && (s == f.cfg.LocalID || strings.EqualFold(s, f.cfg.LocalName)) {
		return f.cfg.LocalID, true
	}
	f.dirMu.Lock()
	defer f.dirMu.Unlock()
	for _, n := range f.dir {
		if n.RevokedAt == nil && n.ID == s {
			return n.ID, true
		}
	}
	for _, n := range f.dir {
		if n.RevokedAt == nil && strings.EqualFold(n.Name, s) {
			return n.ID, true
		}
	}
	return "", false
}

// Name returns a node's name (nodeName), without reading the store: the server's own for "" and LocalID, else the
// directory's, else the id.
func (f *Fleet) Name(id string) string {
	if id == "" || id == f.cfg.LocalID {
		return f.cfg.LocalName
	}
	f.dirMu.Lock()
	for _, n := range f.dir {
		if n.ID == id {
			f.dirMu.Unlock()
			return n.Name
		}
	}
	f.dirMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.remotes[id]; r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.name
	}
	return id
}

// refreshDir reads the nodes from the store into the directory.
func (f *Fleet) refreshDir() {
	if f.cfg.Store == nil {
		return
	}
	rows, err := f.cfg.Store.Nodes()
	if err != nil {
		f.log.Error("read the nodes for the directory", "error", err)
		return
	}
	f.setDir(rows)
}

func (f *Fleet) setDir(rows []store.Node) {
	f.dirMu.Lock()
	f.dir = rows
	f.dirMu.Unlock()
}
