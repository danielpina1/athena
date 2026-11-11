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
// there is none (or why it could not send it). prepareGitHub then builds the worktree from it instead of from the
// default branch: the bundle is fetched into the clone at refs/athena/<id>/seed only, the branch athena/<id> starts at
// the checkpoint's parent (the branch head the checkpoint was taken on), and the checkpoint's tree is restored into the
// worktree, its changes left unstaged (restoreCheckpoint). With no checkpoint, the branch starts from the one pushed
// to GitHub, if it was, else from the default branch.
//
// A Ticket that moves back to a node that kept its worktree (Ruling R22) has that worktree's state committed to a side
// ref, refs/athena/<id>/superseded-<unix time>, then the branch and the worktree reset to the seed's checkpoint. The
// session's first message says where the work came from (movedLine), and names the side ref.

const (
	// seedTimeout bounds the git calls that build a worktree from a seed.
	seedTimeout = 5 * time.Minute
	// seedNameMax bounds the moved-from node name a seed carries.
	seedNameMax = 64
	// seedKeep is how long a seed nobody used is kept; seedSweepEvery how often the node looks for old ones.
	seedKeep       = 14 * 24 * time.Hour
	seedSweepEvery = time.Hour
	// seedBundleGrace is how young a bundle with no record may be before the sweep takes it for an orphan.
	seedBundleGrace = 10 * time.Minute
)

// How a seeded worktree started.
const (
	seedFromCheckpoint = "checkpoint"
	seedFromPushed     = "pushed"
	seedFromBase       = "base"
	seedFromOwn        = "own" // a worktree this node kept, the seed's checkpoint not restorable
)

// Why a seed holds no checkpoint although the server had one (Seed.Unsent), or the node could not restore it.
const (
	SeedTooBig     = "too-big"
	SeedSendFailed = "failed"
	seedRestore    = "restore"
	// SeedForkLate is a child placed away from its lead whose lead's commit did not reach the server in time: it is
	// seeded with no bundle and starts from the default branch.
	SeedForkLate = "fork-late"
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
	// Unsent, with no Bundle, says the server had a checkpoint it could not send: SeedTooBig or SeedSendFailed.
	Unsent string
	// Fork says the Bundle is the lead's checkpoint, for a child placed away from its lead (childgit.go): the child's
	// branch is made at Base, the lead's commit the bundle holds, and nothing is restored.
	Fork bool
	Base string
}

// seedRecord is a Seed as the node keeps it, <StateDir>/seeds/<lowercased id>.json, its bundle beside it (.bundle).
// Note is the moved line, set once the worktree is built from it; the session's first message takes it.
type seedRecord struct {
	TicketID     string    `json:"ticketId"`
	AssignmentID string    `json:"assignmentId"`
	Bundle       bool      `json:"bundle,omitempty"`
	SHA          string    `json:"sha,omitempty"`
	At           time.Time `json:"at,omitzero"`
	From         string    `json:"from"`
	Offline      bool      `json:"offline,omitempty"`
	Unsent       string    `json:"unsent,omitempty"`
	Created      time.Time `json:"created,omitzero"`
	Note         string    `json:"note,omitempty"`
	Fork         bool      `json:"fork,omitempty"`
	Base         string    `json:"base,omitempty"`
}

// seedTicketRe is a Ticket id a seed file is named after.
var seedTicketRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (n *Node) seedPath(ticketID, ext string) string {
	return filepath.Join(n.cfg.StateDir, "seeds", strings.ToLower(ticketID)+ext)
}

// Seed keeps the server's seed of the Ticket's next Assignment, replacing any earlier one; the launch of that
// Assignment builds its worktree from it. The same seed again (the same Assignment and checkpoint) changes nothing.
// The bundle file is the node's once Seed returns (moved, else copied and left for the caller to remove).
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
	switch sd.Unsent {
	case "", SeedTooBig, SeedSendFailed, SeedForkLate:
	default:
		return &core.UserError{Msg: "bad seed: unknown reason " + sd.Unsent}
	}
	if sd.Fork && (sd.Bundle == "" || !shaRe.MatchString(sd.Base)) {
		return &core.UserError{Msg: "bad seed: a fork needs the lead's checkpoint and the commit to start at"}
	}
	if !sd.Fork {
		sd.Base = ""
	}
	if sd.Bundle != "" {
		sd.Unsent = ""
	} else {
		sd.SHA = ""
	}
	if old, ok := n.readSeed(sd.TicketID); ok && old.AssignmentID == sd.AssignmentID && old.SHA == sd.SHA {
		return nil // a repeated seed (placement and reconcile both sent it): the first stands
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
	rec := seedRecord{TicketID: sd.TicketID, AssignmentID: sd.AssignmentID, From: from, Offline: sd.Offline, Unsent: sd.Unsent, Created: time.Now()}
	if sd.Bundle != "" {
		if err := takeFile(sd.Bundle, n.seedPath(sd.TicketID, ".bundle"), MaxCheckpointBundle); err != nil {
			return fmt.Errorf("keep the seed bundle: %w", err)
		}
		rec.Bundle, rec.SHA, rec.At = true, sd.SHA, sd.At
		rec.Fork, rec.Base = sd.Fork, sd.Base
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

// seedFor is the seed kept for the Ticket's Assignment assignmentID; ok is false when there is none. A seed nobody
// used (its restore did not complete) whose Assignment ended goes over to the Ticket's next Assignment on this node:
// the retry of a launch that failed half-way seeds again, rather than run on what the failure left.
func (n *Node) seedFor(t core.Ticket, assignmentID string) (rec seedRecord, ok bool) {
	rec, ok = n.readSeed(t.ID)
	if !ok {
		return rec, false
	}
	if rec.AssignmentID == assignmentID {
		return rec, true
	}
	if rec.Note != "" {
		return seedRecord{}, false
	}
	for _, a := range t.Assignments {
		if a.ID == rec.AssignmentID && a.State == core.AssignmentEnded {
			n.log.Info("an unused seed goes to the ticket's next assignment", "ticket", t.ID, "from", rec.AssignmentID,
				"to", assignmentID)
			rec.AssignmentID = assignmentID
			if err := n.writeSeed(t.ID, rec); err != nil {
				n.log.Warn("keep the seed", "ticket", t.ID, "error", err)
			}
			return rec, true
		}
	}
	return seedRecord{}, false
}

// readSeed is the Ticket's kept seed, whatever its Assignment.
func (n *Node) readSeed(ticketID string) (rec seedRecord, ok bool) {
	if n.cfg.StateDir == "" || !seedTicketRe.MatchString(ticketID) {
		return rec, false
	}
	b, err := readSmall(n.seedPath(ticketID, ".json"))
	if err != nil || json.Unmarshal(b, &rec) != nil {
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

// maybeSweepSeeds sweeps the seeds, on a goroutine of the node's, at most once per seedSweepEvery (every launch asks).
func (n *Node) maybeSweepSeeds() {
	if n.cfg.StateDir == "" {
		return
	}
	now := time.Now().UnixNano()
	last := n.seedSwept.Load()
	if last != 0 && time.Duration(now-last) < seedSweepEvery || !n.seedSwept.CompareAndSwap(last, now) {
		return
	}
	n.runners.Add(1)
	go func() {
		defer n.runners.Done()
		n.sweepSeeds()
	}()
}

// sweepSeeds removes the seeds of Tickets that ended, those older than seedKeep, and bundles with no seed.
func (n *Node) sweepSeeds() {
	dir := filepath.Join(n.cfg.StateDir, "seeds")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	eng := n.engine()
	kept := map[string]bool{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !seedTicketRe.MatchString(id) {
			continue
		}
		var rec seedRecord
		b, err := readSmall(filepath.Join(dir, e.Name()))
		old := err != nil || json.Unmarshal(b, &rec) != nil || time.Since(rec.Created) > seedKeep
		ended := false
		if !old && eng != nil && seedTicketRe.MatchString(rec.TicketID) {
			if t, _, err := eng.Ticket(rec.TicketID); err == nil && t.Ended != nil {
				ended = true
			}
		}
		if old || ended {
			n.dropSeed(id)
			continue
		}
		kept[id] = true
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".bundle")
		if !ok || kept[id] {
			continue
		}
		// A Seed under way writes its bundle before its record: a young bundle is left to it.
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) < seedBundleGrace {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// takeMovedNote is the moved line of the session's first message, once its worktree was built from a seed ("" when
// it was not), and whether the seed was a fork (the line then says where a new child's branch starts, not that the
// Ticket moved); the seed is dropped once it is read.
func (n *Node) takeMovedNote(s *session) (note string, fork bool) {
	rec, ok := n.seedFor(s.t, s.a.ID)
	if !ok || rec.Note == "" {
		return "", false
	}
	n.dropSeed(s.t.ID)
	return rec.Note, rec.Fork || rec.Unsent == SeedForkLate
}

// movedLine says where the moved Ticket's work came from (how): rec's checkpoint, the branch pushed to GitHub, the
// default branch, or this node's own worktree.
func movedLine(rec seedRecord, how, branch, defaultBranch string) string {
	from := "This Ticket moved from " + rec.From
	if rec.Offline {
		from += ", which went offline"
	}
	why := "no checkpoint of its work reached the server"
	switch rec.Unsent {
	case SeedTooBig:
		why = "its last checkpoint was too large to send here"
	case SeedSendFailed:
		why = "its last checkpoint could not be sent here"
	case seedRestore:
		why = "its last checkpoint could not be restored here"
	}
	switch how {
	case seedFromCheckpoint:
		if rec.At.IsZero() {
			return from + "; the work below was restored from its last checkpoint."
		}
		return from + "; the work below was restored from its last checkpoint at " + rec.At.UTC().Format("2006-01-02 15:04 UTC") + "."
	case seedFromPushed:
		return from + "; " + why + ", so the work below starts from the branch " + branch +
			" as it was last pushed. Work in progress after that push is not here."
	case seedFromOwn:
		return from + "; " + why + ", so the work below is this node's own, as it was when the Ticket left it."
	}
	return from + "; " + why + " and its branch was never pushed, so the work starts again from " +
		defaultBranch + ". Nothing done on " + rec.From + " is here."
}

// seedGit runs git on the clone with its pinned environment, bounded by ctx.
func seedGit(ctx context.Context, clone string, env []string) func(args ...string) (string, error) {
	cenv := repoGitEnv(env, filepath.Join(clone, ".git"), clone)
	return func(args ...string) (string, error) { return gitInCtx(ctx, clone, cenv, args...) }
}

// fetchSeed fetches the seed's bundle into the clone's seed ref of the Ticket and returns the checkpoint's parent.
// Only the checkpoint ref is taken (the lead's, for a fork), with every object checked and no protocol but the bundle
// file, and it must be the seed's commit.
func (n *Node) fetchSeed(t core.Ticket, git func(args ...string) (string, error), rec seedRecord) (parent string, err error) {
	seedRef, src := seedRefOf(t), checkpointRef(t.ID)
	if rec.Fork {
		src = checkpointRef(t.Lead)
	}
	err = fetchBundle(git, n.seedPath(t.ID, ".bundle"), src, seedRef)
	var got string
	if err == nil {
		got, err = git("rev-parse", "--verify", "--quiet", seedRef+"^{commit}")
	}
	if err == nil && got != rec.SHA {
		err = fmt.Errorf("the seed holds %s, not the checkpoint %s", got, rec.SHA)
	}
	if err == nil {
		parent, err = git("rev-parse", "--verify", "--quiet", rec.SHA+"^1^{commit}")
	}
	return parent, err
}

// restoreHook, when set (tests), runs before a checkpoint is restored; an error fails the restore.
var restoreHook func() error

func seedRefOf(t core.Ticket) string { return "refs/athena/" + strings.ToLower(t.ID) + "/seed" }

// restoreCheckpoint makes the worktree's files the checkpoint's tree, files the checkpoint lacks removed, and leaves
// the index at HEAD: the work in progress is back as unstaged changes (new files untracked).
func restoreCheckpoint(ctx context.Context, workDir string, wenv []string, sha string) error {
	if restoreHook != nil {
		if err := restoreHook(); err != nil {
			return err
		}
	}
	if _, err := gitInCtx(ctx, workDir, wenv, "read-tree", "--reset", "-u", sha); err != nil {
		return err
	}
	_, err := gitInCtx(ctx, workDir, wenv, "reset", "-q")
	return err
}

// finishSeed keeps the moved line for the first message and drops the bundle and the seed ref.
func (n *Node) finishSeed(s *session, git func(args ...string) (string, error), rec seedRecord, note string) {
	if _, err := git("update-ref", "-d", seedRefOf(s.t)); err != nil {
		s.log.Warn("remove the seed ref", "error", err)
	}
	rec.Note, rec.Bundle = note, false
	if err := n.writeSeed(s.t.ID, rec); err != nil {
		s.log.Warn("keep the moved note", "error", err)
	}
	if err := os.Remove(n.seedPath(s.t.ID, ".bundle")); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("remove the seed bundle", "error", err)
	}
}

// seedWorktree builds the session's new worktree of the clone from its seed (under the clone's lock): see the top of
// the file. A bundle git refuses, or one that does not hold the seed's checkpoint, is logged and the worktree starts
// as if there were none. A worktree whose restore failed is removed, so the next launch seeds it again.
func (n *Node) seedWorktree(s *session, clone, defaultBranch, base string, env []string, rec seedRecord) error {
	ctx, cancel := context.WithTimeout(n.ctx, seedTimeout)
	defer cancel()
	t, p := s.t, s.place
	git := seedGit(ctx, clone, env)
	if rec.Fork {
		return n.forkWorktree(ctx, s, git, clone, env, rec)
	}
	branch := branchName(t)
	if rec.Unsent == SeedForkLate {
		return n.forkLateWorktree(s, git, clone, base, env, rec)
	}
	from, how := base, seedFromBase
	if rec.Bundle {
		if parent, err := n.fetchSeed(t, git, rec); err != nil {
			s.log.Warn("the seed's checkpoint cannot be restored; the worktree starts without it", "error", err)
			rec.Unsent = seedRestore
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
			err = restoreCheckpoint(ctx, p.workDir, wenv, rec.SHA)
		}
		if err != nil {
			// Half built: gone, so the retry seeds it again rather than run on the parent with no word of it.
			_, _ = git("worktree", "remove", "--force", p.workDir)
			_ = os.RemoveAll(p.workDir)
			_, _ = git("worktree", "prune")
			return fmt.Errorf("restore the checkpoint into the worktree: %w", err)
		}
	}
	n.finishSeed(s, git, rec, movedLine(rec, how, branch, defaultBranch))
	s.log.Info("worktree seeded", "from", how, "commit", from)
	return nil
}

// reseedWorktree is a seed for a Ticket whose worktree this node kept (it moved back here): the worktree's state goes
// to a side ref, then the branch and the worktree are reset to the seed's checkpoint (Ruling R22). Without a
// checkpoint it can restore, the worktree stays as it is. Under the clone's lock.
func (n *Node) reseedWorktree(s *session, rec seedRecord) error {
	if rec.Fork {
		// A fork seed only says where a new child's branch starts: the worktree here has its branch already.
		n.dropSeed(s.t.ID)
		return nil
	}
	ctx, cancel := context.WithTimeout(n.ctx, seedTimeout)
	defer cancel()
	t, p := s.t, s.place
	clone := p.repo
	git := seedGit(ctx, clone, cloneGitEnv(clone, p.github))
	branch := branchName(t)
	parent := ""
	if rec.Bundle {
		var err error
		if parent, err = n.fetchSeed(t, git, rec); err != nil {
			s.log.Warn("the seed's checkpoint cannot be restored; the kept worktree stays", "error", err)
			rec.Unsent = seedRestore
		}
	}
	if parent == "" {
		n.finishSeed(s, git, rec, movedLine(rec, seedFromOwn, branch, ""))
		return nil
	}
	wenv, err := n.worktreeEnv(p, t)
	if err != nil {
		return err
	}
	// A ref of its own, never one an earlier reseed made: created only, with another name if that one is taken.
	var side, kept string
	for try := 0; ; try++ {
		side = fmt.Sprintf("%ssuperseded-%d", ticketRefs(t.ID), time.Now().UnixNano())
		if kept, err = checkpointTo(p.workDir, side, wenv, "superseded by a seed", true); err == nil {
			break
		}
		if try == 2 {
			return fmt.Errorf("keep the worktree's state before the seed: %w", err)
		}
	}
	wgit := func(args ...string) (string, error) { return gitInCtx(ctx, p.workDir, wenv, args...) }
	if _, err := wgit("reset", "-q", "--hard", parent); err != nil {
		return err
	}
	if _, err := wgit("clean", "-fdq"); err != nil {
		return err
	}
	if err := restoreCheckpoint(ctx, p.workDir, wenv, rec.SHA); err != nil {
		return fmt.Errorf("restore the checkpoint into the worktree: %w", err)
	}
	note := movedLine(rec, seedFromCheckpoint, branch, "") + " This node's own earlier work on it is kept at " + side + "."
	n.finishSeed(s, git, rec, note)
	s.log.Info("kept worktree reset to the seed", "commit", parent, "superseded", kept, "ref", side)
	return nil
}
