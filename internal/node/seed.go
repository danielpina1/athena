package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

// When a Ticket's Assignment moves to this node from another (Retry elsewhere, after its node went offline; or a
// review placed away from the implementer's node), the server seeds its worktree before the Start: Seed hands the node
// the Ticket's latest checkpoint from the server's mirror (a git bundle of refs/athena/<id>/checkpoint), or tells it
// there is none. prepareGitHub then builds the worktree from it instead of from the default branch: the bundle is
// fetched into the clone at refs/athena/<id>/seed only, the branch athena/<id> starts at the checkpoint's parent (the
// branch head the checkpoint was taken on), and `git checkout <checkpoint> -- .` brings the work in progress back as
// uncommitted changes. With no checkpoint, the branch starts from the one pushed to GitHub, if it was, else from the
// default branch. The session's first message says where the work came from (movedLine).

const (
	// seedTimeout bounds the git calls that build a worktree from a seed.
	seedTimeout = 5 * time.Minute
	// seedNameMax bounds the moved-from node name a seed carries.
	seedNameMax = 64
)

// How a seeded worktree started.
const (
	seedFromCheckpoint = "checkpoint"
	seedFromPushed     = "pushed"
	seedFromBase       = "base"
)

// Seed is the server's word that the Ticket's next Assignment moved to this node, with the work to restore.
type Seed struct {
	TicketID, AssignmentID string
	// Bundle is a git bundle file of the Ticket's checkpoint ref, at most MaxCheckpointBundle long; Seed takes it (the
	// file is moved, or copied, into the node's state). "" when the server holds no checkpoint of the Ticket.
	Bundle string
	// SHA is the checkpoint commit the bundle holds, and At when the server took it.
	SHA string
	At  time.Time
	// From is the name of the node the Ticket moved from; Offline says it moved because that node went offline.
	From    string
	Offline bool
}

// seedRecord is a Seed as the node keeps it, <StateDir>/seeds/<lowercased id>.json, its bundle beside it (.bundle).
// Note is the moved line, set once the worktree is built from it; the session's first message takes it.
type seedRecord struct {
	AssignmentID string    `json:"assignmentId"`
	Bundle       bool      `json:"bundle,omitempty"`
	SHA          string    `json:"sha,omitempty"`
	At           time.Time `json:"at,omitzero"`
	From         string    `json:"from"`
	Offline      bool      `json:"offline,omitempty"`
	Note         string    `json:"note,omitempty"`
}

// seedTicketRe is a Ticket id a seed file is named after.
var seedTicketRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (n *Node) seedPath(ticketID, ext string) string {
	return filepath.Join(n.cfg.StateDir, "seeds", strings.ToLower(ticketID)+ext)
}

// Seed keeps the server's seed of the Ticket's next Assignment, replacing any earlier one; the launch of that
// Assignment builds its worktree from it, if the node has no worktree of the Ticket yet. The bundle file is the node's
// once Seed returns (moved, else copied and left for the caller to remove).
func (n *Node) Seed(sd Seed) error {
	if n.cfg.StateDir == "" {
		return errors.New("no state directory for seeds")
	}
	if !seedTicketRe.MatchString(sd.TicketID) || sd.AssignmentID == "" || len(sd.AssignmentID) > 128 {
		return &core.UserError{Msg: "bad seed: ticket or assignment id"}
	}
	if sd.Bundle != "" && !shaRe.MatchString(sd.SHA) {
		return &core.UserError{Msg: "bad seed: the checkpoint is not a commit name"}
	}
	from := oneLine(sd.From)
	if r := []rune(from); len(r) > seedNameMax {
		from = string(r[:seedNameMax])
	}
	if from == "" {
		from = "another node"
	}
	if err := os.MkdirAll(filepath.Join(n.cfg.StateDir, "seeds"), 0o700); err != nil {
		return err
	}
	n.dropSeed(sd.TicketID)
	rec := seedRecord{AssignmentID: sd.AssignmentID, From: from, Offline: sd.Offline}
	if sd.Bundle != "" {
		if err := takeFile(sd.Bundle, n.seedPath(sd.TicketID, ".bundle"), MaxCheckpointBundle); err != nil {
			return fmt.Errorf("keep the seed bundle: %w", err)
		}
		rec.Bundle, rec.SHA, rec.At = true, sd.SHA, sd.At
	}
	if err := n.writeSeed(sd.TicketID, rec); err != nil {
		n.dropSeed(sd.TicketID)
		return err
	}
	n.log.Info("seed kept", "ticket", sd.TicketID, "assignment", sd.AssignmentID, "checkpoint", sd.SHA, "from", from)
	return nil
}

// takeFile moves src to dst, or copies it there when they are on different file systems; a file over max is refused.
func takeFile(src, dst string, max int64) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return fmt.Errorf("%s is not a bundle of at most %d MiB", src, max>>20)
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, max+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = fmt.Errorf("the bundle is over %d MiB", max>>20)
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

func (n *Node) writeSeed(ticketID string, rec seedRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeFileAtomic(n.seedPath(ticketID, ".json"), string(b))
}

// seedFor is the seed kept for the Ticket's Assignment assignmentID; ok is false when there is none (or it is
// another Assignment's).
func (n *Node) seedFor(ticketID, assignmentID string) (rec seedRecord, ok bool) {
	if n.cfg.StateDir == "" || !seedTicketRe.MatchString(ticketID) {
		return rec, false
	}
	b, err := readSmall(n.seedPath(ticketID, ".json"))
	if err != nil {
		return rec, false
	}
	if json.Unmarshal(b, &rec) != nil || rec.AssignmentID != assignmentID {
		return seedRecord{}, false
	}
	return rec, true
}

// dropSeed removes the Ticket's seed and its bundle.
func (n *Node) dropSeed(ticketID string) {
	if n.cfg.StateDir == "" || !seedTicketRe.MatchString(ticketID) {
		return
	}
	for _, ext := range []string{".json", ".bundle"} {
		if err := os.Remove(n.seedPath(ticketID, ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			n.log.Warn("remove the ticket's seed", "ticket", ticketID, "error", err)
		}
	}
}

// takeMovedNote is the moved line of the session's first message, once its worktree was built from a seed ("" when
// it was not); the seed is dropped once it is read.
func (n *Node) takeMovedNote(s *session) string {
	rec, ok := n.seedFor(s.t.ID, s.a.ID)
	if !ok || rec.Note == "" {
		return ""
	}
	n.dropSeed(s.t.ID)
	return rec.Note
}

// movedLine says where the moved Ticket's work came from: rec's checkpoint, the branch pushed to GitHub, or the
// default branch (how).
func movedLine(rec seedRecord, how, branch, defaultBranch string) string {
	from := "This Ticket moved from " + rec.From
	if rec.Offline {
		from += ", which went offline"
	}
	switch how {
	case seedFromCheckpoint:
		if rec.At.IsZero() {
			return from + "; the work below was restored from its last checkpoint."
		}
		return from + "; the work below was restored from its last checkpoint at " + rec.At.UTC().Format("2006-01-02 15:04 UTC") + "."
	case seedFromPushed:
		return from + "; no checkpoint of its work reached the server, so the work below starts from the branch " + branch +
			" as it was last pushed. Work in progress after that push is not here."
	}
	return from + "; no checkpoint of its work reached the server and its branch was never pushed, so the work starts again from " +
		defaultBranch + ". Nothing done on " + rec.From + " is here."
}

// seedWorktree builds the session's worktree of the clone from its seed (under the clone's lock): see the top of the
// file. A bundle git refuses, or one that does not hold the seed's checkpoint, is logged and the worktree starts as if
// there were none. The bundle is removed and the moved line kept for the first message.
func (n *Node) seedWorktree(s *session, clone, defaultBranch, base string, env []string, rec seedRecord) error {
	ctx, cancel := context.WithTimeout(n.ctx, seedTimeout)
	defer cancel()
	t, p := s.t, s.place
	cenv := repoGitEnv(env, filepath.Join(clone, ".git"), clone)
	git := func(args ...string) (string, error) { return gitInCtx(ctx, clone, cenv, args...) }
	branch, seedRef := branchName(t), "refs/athena/"+strings.ToLower(t.ID)+"/seed"
	bundle := n.seedPath(t.ID, ".bundle")
	from, how := base, seedFromBase
	if rec.Bundle {
		// Only the checkpoint ref is taken, into a ref of this Ticket's, with every object checked and no protocol but
		// the bundle file.
		_, err := git("-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "transfer.fsckObjects=true",
			"-c", "fetch.fsckObjects=true", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "fetch", "--quiet", "--no-tags",
			"--no-write-fetch-head", "--no-auto-gc", "--no-recurse-submodules", bundle, "+"+checkpointRef(t.ID)+":"+seedRef)
		var got, parent string
		if err == nil {
			got, err = git("rev-parse", "--verify", "--quiet", seedRef+"^{commit}")
		}
		if err == nil && got != rec.SHA {
			err = fmt.Errorf("the seed holds %s, not the checkpoint %s", got, rec.SHA)
		}
		if err == nil {
			parent, err = git("rev-parse", "--verify", "--quiet", rec.SHA+"^1^{commit}")
		}
		if err != nil {
			s.log.Warn("the seed's checkpoint cannot be restored; the worktree starts without it", "error", err)
		} else {
			from, how = parent, seedFromCheckpoint
		}
	}
	if how != seedFromCheckpoint {
		if sha, err := git("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch+"^{commit}"); err == nil && sha != "" {
			from, how = sha, seedFromPushed
		}
	}
	// The branch starts at from, whatever this node kept of it from an earlier Assignment: its worktree is gone.
	_, _ = git("worktree", "prune")
	if _, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		if _, err := git("update-ref", "refs/heads/"+branch, from); err != nil {
			return err
		}
	}
	if err := ensureWorktree(clone, p.workDir, branch, from, env); err != nil {
		return err
	}
	if how == seedFromCheckpoint {
		wenv, err := n.worktreeEnv(p, t)
		if err == nil {
			_, err = gitInCtx(ctx, p.workDir, wenv, "checkout", rec.SHA, "--", ".")
		}
		if err != nil {
			return fmt.Errorf("restore the checkpoint into the worktree: %w", err)
		}
	}
	if _, err := git("update-ref", "-d", seedRef); err != nil {
		s.log.Warn("remove the seed ref", "error", err)
	}
	rec.Note = movedLine(rec, how, branch, defaultBranch)
	rec.Bundle = false
	if err := n.writeSeed(t.ID, rec); err != nil {
		s.log.Warn("keep the moved note", "error", err)
	}
	if err := os.Remove(bundle); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("remove the seed bundle", "error", err)
	}
	s.log.Info("worktree seeded", "from", how, "commit", from)
	return nil
}
