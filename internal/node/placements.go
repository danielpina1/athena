package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/danielpina1/athena/internal/core"
)

// A Ticket's placement (where its session works: a plain directory, a worktree of a repository, of the node's clone
// for a GitHub Ticket) is derived once, at its first placement, and recorded in the state directory, which no worker
// writes. Every later launch, view and cleanup uses the record and never derives it again: a worker of a plain
// directory that turned it into a repository (`git init`, with a config naming a smudge filter) must not have the next
// launch make a worktree of it, which would run the filter on the host, outside any sandbox. A Ticket placed before
// records existed is derived once more, recorded, and logged.

// placementRecord is a placement as recorded.
type placementRecord struct {
	WorkDir string `json:"workDir"`
	Repo    string `json:"repo,omitempty"`
	Common  string `json:"common,omitempty"`
	DirKey  string `json:"dirKey,omitempty"`
	GitHub  string `json:"github,omitempty"`
}

// placementsDir holds the records, one file per Ticket.
const placementsDir = "placements"

// placementPath is the record of ticketID, or "" when the node keeps none (no state directory, an id that is no
// file name).
func (n *Node) placementPath(ticketID string) string {
	if n.cfg.StateDir == "" || ticketID == "" || strings.ContainsAny(ticketID, `/\`) || ticketID == "." || ticketID == ".." {
		return ""
	}
	return filepath.Join(n.cfg.StateDir, placementsDir, ticketID+".json")
}

// recordedPlacement is t's recorded placement; ok is false when it has none. A record that cannot be read is an error:
// the Ticket is not placed.
func (n *Node) recordedPlacement(t core.Ticket) (placement, bool, error) {
	path := n.placementPath(t.ID)
	if path == "" {
		return placement{}, false, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return placement{}, false, nil
	}
	var r placementRecord
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	if err == nil && r.WorkDir == "" {
		err = errors.New("no working directory")
	}
	if err != nil {
		return placement{}, true, fmt.Errorf("the placement record of %s: %w", t.ID, err)
	}
	p := placement{workDir: r.WorkDir, repo: r.Repo, common: r.Common, dirKey: r.DirKey, github: r.GitHub}
	// The state directory is refused whenever it is asked, as when the placement was made.
	if dir := p.dirKey; dir != "" || (p.repo != "" && p.github == "") {
		if dir == "" {
			dir = p.repo
		}
		if err := n.checkDirectory(dir); err != nil {
			return placement{}, true, err
		}
	}
	return p, true, nil
}

// recordPlacement records t's placement p, made now.
func (n *Node) recordPlacement(t core.Ticket, p placement) error {
	path := n.placementPath(t.ID)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(placementRecord{WorkDir: p.workDir, Repo: p.repo, Common: p.common, DirKey: p.dirKey, GitHub: p.github})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, string(b)+"\n"); err != nil {
		return fmt.Errorf("record the placement of %s: %w", t.ID, err)
	}
	kind := "directory"
	switch {
	case p.github != "":
		kind = "github"
	case p.repo != "":
		kind = "worktree"
	case p.dirKey != "":
		kind = "plain"
	}
	// A Ticket that already ran had its placement derived before records existed: say so, once.
	earlier := len(t.Assignments) > 1
	if a := t.LatestAssignment(); a != nil && a.SessionID != "" {
		earlier = true
	}
	n.log.Info("placement recorded", "ticket", t.ID, "kind", kind, "dir", p.workDir, "placed_before_records", earlier)
	return nil
}
